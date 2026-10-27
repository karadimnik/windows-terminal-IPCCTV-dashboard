package archive

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive/archivetest"
	"holocron/internal/osx"
)

// wantScan is the DirScan the D20 rules give for c (archivetest.Walk, no archive code): mode
// scanSpool reads the spool alone, scanLive adds the spool-first day folders, scanAll also counts
// every day folder's files.
func wantScan(c archivetest.TCamera, dir string, mode scanMode, now time.Time, segSec int) DirScan {
	s := DirScan{Dir: dir, ScannedAt: now.UTC()}
	count := func(f archivetest.TFile, filed bool) {
		m := f.MTime.UTC()
		s.Files++
		s.Bytes += f.Size
		if filed {
			s.Filed++
			s.FiledBytes += f.Size
		}
		if s.OldestWrite.IsZero() || m.Before(s.OldestWrite) {
			s.OldestWrite = m
		}
		if m.After(s.NewestWrite) {
			s.NewestWrite = m
		}
	}
	for _, f := range c.Spool {
		count(f, false)
	}
	if mode == scanAll {
		for _, d := range c.Days {
			for _, f := range c.DayFiles[d] {
				count(f, true)
			}
		}
	}
	if mode == scanSpool {
		c.Days = nil
	}
	_, n, ok, closed := c.Live()
	if ok {
		s.Newest, s.NewestMTime, s.NewestBytes = n.Name, n.MTime.UTC(), n.Size
		age := now.Sub(n.MTime)
		s.Recent = age <= time.Duration(segSec)*time.Second+openGrace
		s.Anomaly = -age > futureTolerance
	}
	s.NewestClosedWrite = closed
	return s
}

func paths[T any](xs []T, path func(T) string) []string {
	out := []string{}
	for _, x := range xs {
		out = append(out, path(x))
	}
	return out
}

// wantClass is Classify by the D20 rules, from the path relative to root.
func wantClass(rel, container string, fold bool) Class {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || len(parts) > 3 || strings.HasPrefix(parts[0], "_") {
		return Class{}
	}
	cam, name := parts[0], parts[len(parts)-1]
	start, ok := archivetest.SegStart(name, cam, container, fold)
	if !ok {
		return Class{}
	}
	if len(parts) == 2 {
		return Class{cam, "", name, Spooled}
	}
	day := parts[1]
	if !archivetest.IsDay(day) {
		return Class{}
	}
	if start.Format("2006-01-02") != day {
		return Class{cam, day, name, Misfiled}
	}
	return Class{cam, day, name, Filed}
}

// wantResolve is Resolve by its documented procedure, with os.ReadDir: the spool's entries (exact,
// then folded), then those of the day folder the name's date names.
func wantResolve(c archivetest.TCamera, name string, fold bool) (string, bool) {
	look := func(dir string) (string, bool) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return "", false
		}
		for _, e := range ents {
			if e.Name() == name {
				return filepath.Join(dir, e.Name()), true
			}
		}
		for _, e := range ents {
			if fold && strings.EqualFold(e.Name(), name) {
				return filepath.Join(dir, e.Name()), true
			}
		}
		return "", false
	}
	if name == "" {
		return "", false
	}
	if p, ok := look(c.Dir); ok {
		return p, true
	}
	dot := strings.LastIndexByte(name, '.')
	if dot < 22 || strings.ContainsAny(name[:dot], "/\\\x00") {
		return "", false
	}
	ext := name[dot+1:]
	if len(ext) < 1 || len(ext) > 8 || strings.Trim(ext, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return "", false
	}
	start, ok := archivetest.SegStart(name[:dot]+".x", name[:dot-21], ".x", false)
	if !ok {
		return "", false
	}
	day := start.Format("2006-01-02")
	for _, d := range c.Days {
		if d == day {
			return look(filepath.Join(c.Dir, d))
		}
	}
	return "", false
}

// wantClaimable is the claim rule by D20: every top-level entry not starting with "_" is a real
// folder; each holds only segments of its camera and day folders, and a day folder holds only
// segments filed under its date.
func wantClaimable(t *testing.T, root, container string, fold bool) bool {
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "_") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil || fi.Mode().Type() != fs.ModeDir {
			return false
		}
		children, _ := os.ReadDir(dir)
		for _, c := range children {
			ci, _ := os.Lstat(filepath.Join(dir, c.Name()))
			if ci.Mode().Type() == fs.ModeDir && archivetest.IsDay(c.Name()) {
				inner, _ := os.ReadDir(filepath.Join(dir, c.Name()))
				for _, x := range inner {
					xi, _ := os.Lstat(filepath.Join(dir, c.Name(), x.Name()))
					start, ok := archivetest.SegStart(x.Name(), e.Name(), container, fold)
					if !xi.Mode().IsRegular() || !ok || start.Format("2006-01-02") != c.Name() {
						return false
					}
				}
				continue
			}
			if _, ok := archivetest.SegStart(c.Name(), e.Name(), container, fold); !ci.Mode().IsRegular() || !ok {
				return false
			}
		}
	}
	return true
}

// TestMixedTreesAgreeWithTheWalk: over generated spool-and-day-folder trees, every reader of the
// library gives what an independent walk of the same tree gives by the rules of D20, with both
// case rules, uncached and cached.
func TestMixedTreesAgreeWithTheWalk(t *testing.T) {
	trees := archivetest.Mixed(20261028, 160)
	var cams, filed, misfiled, spoolFirst, fallback, resolved, links, claimable int
	for _, tr := range trees {
		root, n := tr.Build(t, t.TempDir())
		links += n
		p := osx.Current()
		if want := wantClaimable(t, root, tr.Container, MatcherFor(p).FoldCase); (ClaimProblem(root, p, tr.Container) == "") != want {
			t.Errorf("%s: ClaimProblem = %q, claimable by the walk: %v", tr.Name, ClaimProblem(root, p, tr.Container), want)
		} else if want {
			claimable++
		}
		for _, fold := range []bool{false, true} {
			truth := archivetest.Walk(t, root, tr.Container, fold)
			for _, cached := range []bool{false, true} {
				l := Layout{Root: root, Matcher: Matcher{FoldCase: fold}, Container: tr.Container}
				if cached {
					l.Cache = NewCache()
				}
				for _, c := range truth.Cameras {
					where := tr.Name + "/" + c.Name
					cams++
					for range 2 { // a second call through the cache
						files, err := l.Files(c.Name)
						if got, want := paths(files, func(f File) string { return f.Path + "|" + f.Day }), paths(c.AllFiles(), func(f archivetest.TFile) string { return f.Path + "|" + f.Day }); err != nil || !reflect.DeepEqual(got, want) {
							t.Errorf("%s fold=%v: Files\n got  %v %v\n want %v", where, fold, got, err, want)
						}
						segs, err := l.Segments(c.Name)
						if got, want := paths(segs, func(s Segment) string { return s.Path }), paths(c.Footage, func(s archivetest.TSeg) string { return s.Path }); err != nil || !reflect.DeepEqual(got, want) {
							t.Errorf("%s fold=%v: Segments\n got  %v %v\n want %v", where, fold, got, err, want)
						}
						for _, mode := range []scanMode{scanSpool, scanLive, scanAll} {
							got, err := l.scan(c.Name, tr.Now, tr.SegmentSeconds, mode)
							if want := wantScan(c, l.CameraDir(c.Name), mode, tr.Now, tr.SegmentSeconds); err != nil || !reflect.DeepEqual(got, want) {
								t.Errorf("%s fold=%v mode=%d:\n got  %+v %v\n want %+v", where, fold, mode, got, err, want)
							}
						}
					}
					read, wn, wok, _ := c.Live()
					n, ok, err := l.Newest(c.Name)
					if err != nil || ok != wok || (ok && n.Path != wn.Path) {
						t.Errorf("%s fold=%v: Newest = %q %v %v, want %q %v", where, fold, n.Path, ok, err, wn.Path, wok)
					}
					if wok && wn.Day == "" {
						spoolFirst++
					} else if wok {
						fallback++
					}
					_ = read
					filed += len(c.Footage)
					misfiled += len(c.Misfiled)

					// Classify every file under the camera folder, at any depth
					filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
						if err != nil || d.IsDir() {
							return err
						}
						rel, _ := filepath.Rel(root, p)
						if got, want := l.Classify(p), wantClass(rel, tr.Container, fold); got != want {
							t.Errorf("%s fold=%v: Classify(%s) = %+v, want %+v", where, fold, rel, got, want)
						}
						return nil
					})

					// Resolve every name the camera's folders hold, its case variants and traversal attempts
					var names []string
					for _, f := range c.AllFiles() {
						rel, _ := filepath.Rel(c.Dir, f.Path)
						names = append(names, f.Name, strings.ToUpper(f.Name), strings.ToLower(f.Name), "../"+f.Name, "..\\"+f.Name, rel,
							filepath.ToSlash(rel), strings.ReplaceAll(rel, string(filepath.Separator), "\\"), f.Path, f.Name+"\x00",
							"../"+c.Name+"/"+filepath.ToSlash(rel), "../"+c.Name+"_old/"+f.Name, "/"+f.Name, `C:\`+f.Name)
					}
					for _, d := range c.Days {
						names = append(names, d, d+"/", "../"+d)
					}
					for _, name := range names {
						e, err := l.Resolve(c.Name, name)
						want, wok := wantResolve(c, name, fold)
						switch {
						case wok != (err == nil):
							t.Errorf("%s fold=%v: Resolve(%q) = %q %v, want %q %v", where, fold, name, e.Path, err, want, wok)
						case wok && e.Path != want:
							t.Errorf("%s fold=%v: Resolve(%q) = %q, want %q", where, fold, name, e.Path, want)
						case !wok && !errors.Is(err, fs.ErrNotExist):
							t.Errorf("%s fold=%v: Resolve(%q) error %v", where, fold, name, err)
						case wok:
							resolved++
							if in := filepath.Dir(e.Path); in != c.Dir && filepath.Dir(in) != c.Dir {
								t.Errorf("%s: Resolve(%q) left the camera folder: %s", where, name, e.Path)
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d trees, %d camera reads, %d footage, %d misfiled, newest in the spool %d, in a day folder %d, %d names resolved, %d links, %d claimable",
		len(trees), cams, filed, misfiled, spoolFirst, fallback, resolved, links, claimable)
	if filed < 3000 || misfiled < 20 || spoolFirst < 100 || fallback < 20 || claimable < 5 {
		t.Errorf("the corpus is too thin")
	}
}

// buildDays makes cams cameras with days day folders of perDay segments each, and spool segments
// in the spool, under a fresh root; the newest filed segment is 30 minutes older than the spool's oldest.
func buildDays(t *testing.T, cams, days, perDay, spool int) (Layout, []string, time.Time) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "archive")
	zone := time.FixedZone("", 3*3600)
	now := time.Date(2026, 10, 27, 12, 0, 10, 0, zone)
	var names []string
	for c := 0; c < cams; c++ {
		cam := "Cam_" + string(rune('A'+c))
		names = append(names, cam)
		dir := filepath.Join(root, cam)
		write := func(sub string, start time.Time, mt time.Time) {
			p := filepath.Join(dir, sub, FormatSegmentName(cam, start, ".mkv"))
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, mt, mt); err != nil {
				t.Fatal(err)
			}
		}
		for d := 1; d <= days; d++ {
			day := now.AddDate(0, 0, -d)
			sub := DayName(day)
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
			midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, zone)
			for k := 0; k < perDay; k++ {
				st := midnight.Add(time.Duration(k) * 15 * time.Minute)
				write(sub, st, st.Add(15*time.Minute))
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for k := 0; k < spool; k++ {
			st := now.Truncate(15 * time.Minute).Add(-time.Duration(k) * 15 * time.Minute)
			write("", st, st.Add(15*time.Minute))
		}
	}
	return Layout{Root: root, Matcher: Matcher{}, Container: ".mkv"}, names, now
}

// TestStatusTickListsOnlyTheSpool (cost): on 6 cameras x 30 day folders x 96 segments, a status
// tick (Layout.Scan, and Newest) lists each camera folder once and no day folder while the spool
// holds the open and the newest closed segment, cached or not; the full readers list every day.
func TestStatusTickListsOnlyTheSpool(t *testing.T) {
	l, cams, now := buildDays(t, 6, 30, 96, 2)
	count := func(f func(cam string)) int64 {
		before := listings.Load()
		for _, cam := range cams {
			f(cam)
		}
		return listings.Load() - before
	}
	for _, cache := range []*Cache{nil, NewCache()} {
		l.Cache = cache
		for tick := 0; tick < 3; tick++ {
			if n := count(func(cam string) {
				s, err := l.Scan(cam, now, 900)
				if err != nil || s.Newest == "" || s.NewestClosedWrite.IsZero() || s.Files != 2 {
					t.Fatalf("Scan %+v %v", s, err)
				}
			}); n != int64(len(cams)) {
				t.Errorf("cache=%v tick %d: %d listings for %d cameras, want 1 each", cache != nil, tick, n, len(cams))
			}
		}
		if n := count(func(cam string) { l.Newest(cam) }); n != int64(len(cams)) {
			t.Errorf("Newest: %d listings, want %d", n, len(cams))
		}
		if n := count(func(cam string) { l.ScanAll(cam, now, 900) }); n != int64(len(cams)*31) {
			t.Errorf("ScanAll: %d listings, want %d", n, len(cams)*31)
		}
		if n := count(func(cam string) { l.Segments(cam) }); n != int64(len(cams)*31) {
			t.Errorf("Segments: %d listings, want %d", n, len(cams)*31)
		}
	}
	if cs := l.Cache.Stats(); cs.Listings == 0 {
		t.Error("the cache counted no listing")
	}
}

// TestStatusTickFallsBackToTheNewestDay pins the fallback of the spool-first rule: with one segment
// in the spool a tick also lists the newest day folder (two listings, so the newest closed write is
// still exact), with none it lists day folders until two segments are seen; never more.
func TestStatusTickFallsBackToTheNewestDay(t *testing.T) {
	for _, tc := range []struct {
		spool, perDay int
		listings      int64
		newestFiled   bool
	}{
		{2, 96, 1, false},
		{1, 96, 2, false},
		{0, 96, 2, true},
		{0, 1, 3, true}, // one segment a day: two day folders to see two segments
	} {
		l, cams, now := buildDays(t, 1, 30, tc.perDay, tc.spool)
		cam := cams[0]
		before := listings.Load()
		s, err := l.Scan(cam, now, 900)
		if n := listings.Load() - before; err != nil || n != tc.listings {
			t.Errorf("spool %d: %d listings, want %d (%v)", tc.spool, n, tc.listings, err)
		}
		all, _ := l.Segments(cam)
		if s.Newest != all[0].Name() || s.NewestClosedWrite.IsZero() || !s.NewestClosedWrite.Equal(all[1].Info.ModTime().UTC()) {
			t.Errorf("spool %d: newest %q closed %v, want %q closed at %v", tc.spool, s.Newest, s.NewestClosedWrite, all[0].Name(), all[1].Info.ModTime())
		}
		if s.Files != tc.spool || s.Recent != (tc.spool > 0) {
			t.Errorf("spool %d: Files %d Recent %v", tc.spool, s.Files, s.Recent)
		}
		if sp, _ := l.ScanSpool(cam, now, 900); (sp.Newest == "") != tc.newestFiled {
			t.Errorf("spool %d: ScanSpool newest %q", tc.spool, sp.Newest)
		}
	}
}

// TestDayFolderAppearsAndVanishes: a day folder made or removed between calls is seen at the next
// call, cached or not; one removed after the camera folder was listed is an error of DayFiles and
// is left out of Files.
func TestDayFolderAppearsAndVanishes(t *testing.T) {
	for _, cache := range []*Cache{nil, NewCache()} {
		l, cams, now := buildDays(t, 1, 2, 3, 0)
		l.Cache = cache
		cam := cams[0]
		segs, _ := l.Segments(cam)
		if len(segs) != 6 {
			t.Fatalf("%d segments", len(segs))
		}
		_, days, _ := l.Spool(cam)
		if err := os.RemoveAll(days[0].Path); err != nil {
			t.Fatal(err)
		}
		if _, err := l.DayFiles(days[0]); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("DayFiles of a vanished day folder: %v", err)
		}
		if segs, _ := l.Segments(cam); len(segs) != 3 {
			t.Errorf("after the newest day vanished: %d segments", len(segs))
		}
		if s, _ := l.Scan(cam, now, 900); s.Newest == "" || strings.Contains(s.Newest, DayName(now.AddDate(0, 0, -1))) {
			t.Errorf("Scan after the newest day vanished: %+v", s)
		}
		day := filepath.Join(l.CameraDir(cam), DayName(now))
		if err := os.Mkdir(day, 0o755); err != nil {
			t.Fatal(err)
		}
		name := FormatSegmentName(cam, now.Add(-time.Hour), ".mkv")
		if err := os.WriteFile(filepath.Join(day, name), []byte("y"), 0o644); err != nil {
			t.Fatal(err)
		}
		if n, ok, _ := l.Newest(cam); !ok || n.Name() != name || n.Day != DayName(now) {
			t.Errorf("Newest after a new day folder: %+v %v", n.Entry, ok)
		}
		if e, err := l.Resolve(cam, name); err != nil || e.Path != filepath.Join(day, name) {
			t.Errorf("Resolve in the new day folder: %+v %v", e, err)
		}
	}
}

// TestDayRules pins the day-folder name rule and the DST day.
func TestDayRules(t *testing.T) {
	for name, want := range map[string]bool{"2026-10-25": true, "2024-02-29": true, "2026-02-29": false, "2026-02-30": false, "2026-13-45": false,
		"2026-00-10": false, "2026-10-00": false, "2026-1-05": false, "20261025": false, "2026_10_25": false, "2026-10-25 ": false, "２026-10-25": false, "": false} {
		if IsDayName(name) != want {
			t.Errorf("IsDayName(%q) = %v", name, !want)
		}
	}
	for _, n := range []string{"Cam_A_20261025_033000+0300.mkv", "Cam_A_20261025_033000+0200.mkv", "Cam_A_20261025_235959+0200.mkv", "Cam_A_20261025_000000+0300.mkv"} {
		_, start, ok := ParseSegment(n)
		if !ok || DayName(start) != "2026-10-25" {
			t.Errorf("DayName(%s) = %s", n, DayName(start))
		}
	}
}

// TestClaimRuleAcceptsDayFolders: date-named sub-folders holding the camera's segments filed under
// their date do not block a claim; any other sub-folder, a misfiled or foreign file in a day
// folder and a folder inside a day folder do, as a folder in a camera folder always has.
func TestClaimRuleAcceptsDayFolders(t *testing.T) {
	p := osx.Current()
	const seg = "Cam_A_20261017_120000+0300.mkv"
	for _, tc := range []struct {
		name  string
		paths []string // files; a trailing "/" is a folder
		want  string   // "" = claimable, else a substring of the problem
	}{
		{"spool and days", []string{"Cam_A/" + seg, "Cam_A/2026-10-17/" + seg, "Cam_A/2026-10-16/Cam_A_20261016_235959+0300.mkv"}, ""},
		{"an empty day folder", []string{"Cam_A/2026-10-18/"}, ""},
		{"days only", []string{"Cam_A/2026-10-17/" + seg}, ""},
		{"another folder", []string{"Cam_A/2026-10-17/" + seg, "Cam_A/old/"}, "Cam_A/old (a folder inside a camera folder)"},
		{"an invalid date", []string{"Cam_A/2026-02-30/"}, "Cam_A/2026-02-30 (a folder inside a camera folder)"},
		{"misfiled", []string{"Cam_A/2026-10-18/" + seg}, "Cam_A/2026-10-18/" + seg + " (filed under another day)"},
		{"foreign in a day", []string{"Cam_A/2026-10-17/notes.txt"}, "Cam_A/2026-10-17/notes.txt (not a recorder segment)"},
		{"other camera in a day", []string{"Cam_A/2026-10-17/Cam_B_20261017_120000+0300.mkv"}, "(not a recorder segment)"},
		{"a folder in a day", []string{"Cam_A/2026-10-17/x/"}, "Cam_A/2026-10-17/x (a folder inside a day folder)"},
	} {
		root := filepath.Join(t.TempDir(), "archive")
		for _, rel := range tc.paths {
			full := filepath.Join(root, filepath.FromSlash(rel))
			if strings.HasSuffix(rel, "/") {
				if err := os.MkdirAll(full, 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got := ClaimProblem(root, p, ".mkv")
		if (tc.want == "") != (got == "") || !strings.Contains(got, filepath.FromSlash(tc.want)) && !strings.Contains(got, tc.want) {
			t.Errorf("%s: ClaimProblem = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestStraysOverMixedTrees: on the awkward seeded trees, Strays names exactly the misfiled
// segments and the other regular files of the day folders the brute-force walk finds, and as
// nested only segment-named regular files in folders no reader enters; never footage, never a
// spool file, never anything behind a link.
func TestStraysOverMixedTrees(t *testing.T) {
	fold := runtime.GOOS == "windows"
	kinds := map[StrayKind]int{}
	for _, tr := range archivetest.Mixed(20261028, 120) {
		root, _ := tr.Build(t, t.TempDir())
		truth := archivetest.Walk(t, root, tr.Container, fold)
		l := Layout{Root: root, Matcher: Matcher{FoldCase: fold}, Container: tr.Container}
		for _, c := range truth.Cameras {
			want := map[string]StrayKind{}
			footage := map[string]bool{}
			for _, s := range c.Footage {
				footage[s.Path] = true
			}
			for _, s := range c.Misfiled {
				want[s.Path] = StrayMisfiled
			}
			for _, files := range c.DayFiles {
				for _, f := range files {
					if _, ok := want[f.Path]; !ok && f.Mode.IsRegular() && !footage[f.Path] {
						want[f.Path] = StrayLoose
					}
				}
			}
			strays, err := l.Strays(c.Name)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]StrayKind{}
			for _, s := range strays {
				kinds[s.Kind]++
				if s.Kind == StrayNested {
					rel, _ := filepath.Rel(c.Dir, s.Path)
					parts := strings.Split(rel, string(filepath.Separator))
					fi, err := os.Lstat(s.Path)
					if _, _, ok := ParseSegment(filepath.Base(s.Path)); !ok || err != nil || !fi.Mode().IsRegular() || len(parts) < 2 || (len(parts) == 2 && IsDayName(parts[0])) {
						t.Errorf("%s: nested stray %s is not a segment in a folder no reader enters", tr.Name, s.Path)
					}
					continue
				}
				got[s.Path] = s.Kind
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s/%s: strays\n got  %v\n want %v", tr.Name, c.Name, got, want)
			}
		}
	}
	if kinds[StrayMisfiled] == 0 || kinds[StrayLoose] == 0 || kinds[StrayNested] == 0 {
		t.Fatalf("not every kind exercised: %v", kinds)
	}
	t.Logf("strays found: %d misfiled, %d loose, %d nested", kinds[StrayMisfiled], kinds[StrayLoose], kinds[StrayNested])
}
