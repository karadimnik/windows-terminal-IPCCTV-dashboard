package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive/archivetest"
	"holocron/internal/osx"
)

const goldenRoot = "../../testdata/golden"

// oldScan is Matcher.Scan as it was before the layout library (reference for the equivalence test).
func oldScan(m Matcher, dir, container string, now time.Time, segmentSeconds int) (DirScan, error) {
	now = now.UTC()
	scan := DirScan{Dir: dir, ScannedAt: now}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if fi, serr := os.Stat(dir); serr == nil && !fi.IsDir() {
			return scan, errors.New(dir + " is not a directory")
		}
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	safe := filepath.Base(filepath.Clean(dir))
	type file struct {
		name  string
		mtime time.Time
	}
	var files []file
	var newest fs.FileInfo
	var newestM time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mtime := info.ModTime().UTC()
		scan.Files++
		scan.Bytes += info.Size()
		if scan.OldestWrite.IsZero() || mtime.Before(scan.OldestWrite) {
			scan.OldestWrite = mtime
		}
		if mtime.After(scan.NewestWrite) {
			scan.NewestWrite = mtime
		}
		files = append(files, file{e.Name(), mtime})
		if !info.Mode().IsRegular() || !m.IsSegmentOf(e.Name(), safe, container) {
			continue
		}
		if newest == nil || !mtime.Before(newestM) {
			newest, newestM = info, mtime
		}
	}
	if newest != nil {
		scan.Newest = newest.Name()
		scan.NewestMTime = newestM
		scan.NewestBytes = newest.Size()
		if n, err := LiveSize(filepath.Join(dir, newest.Name())); err == nil {
			scan.Bytes += n - scan.NewestBytes
			scan.NewestBytes = n
		}
		age := now.Sub(newestM)
		scan.Recent = age <= time.Duration(segmentSeconds)*time.Second+openGrace
		scan.Anomaly = -age > futureTolerance
	}
	for _, f := range files {
		if f.name != scan.Newest && f.mtime.After(scan.NewestClosedWrite) {
			scan.NewestClosedWrite = f.mtime
		}
	}
	return scan, nil
}

// oldClaimProblem is ClaimProblem as it was before the layout library.
func oldClaimProblem(root string, p osx.Platform, container string) string {
	if problem := CheckRoot(root, p); problem != "" {
		return problem
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return "RootPath '" + root + "' does not exist"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Sprintf("RootPath '%s' cannot be listed: %v", root, err)
	}
	var blockers []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "_") {
			continue
		}
		info, err := e.Info()
		switch {
		case err != nil:
			continue
		case IsReparse(info):
			blockers = append(blockers, name+" (a reparse point)")
			continue
		case !info.IsDir():
			blockers = append(blockers, name+" (a file at the top level)")
			continue
		}
		children, err := os.ReadDir(filepath.Join(root, name))
		if err != nil {
			blockers = append(blockers, name+" (cannot be listed)")
			continue
		}
		for _, c := range children {
			ci, err := c.Info()
			if err != nil {
				continue
			}
			if ci.IsDir() {
				blockers = append(blockers, name+"/"+c.Name()+" (a folder inside a camera folder)")
				break
			}
			if IsReparse(ci) || !ci.Mode().IsRegular() || !MatcherFor(p).IsSegmentOf(c.Name(), name, container) {
				blockers = append(blockers, name+"/"+c.Name()+" (not a recorder segment)")
				break
			}
		}
	}
	if len(blockers) == 0 {
		return ""
	}
	shown := blockers
	if len(shown) > 5 {
		shown = shown[:5]
	}
	plural := "ies"
	if len(blockers) == 1 {
		plural = "y"
	}
	return fmt.Sprintf("RootPath '%s' holds %d top-level entr%s the recorder did not write: %s", root, len(blockers), plural, strings.Join(shown, ", "))
}

// cameraNames are every top-level non-"_" entry name of root, plus one that does not exist.
func cameraNames(t testing.TB, root string) []string {
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "_") {
			out = append(out, e.Name())
		}
	}
	return append(out, "Missing_Camera")
}

// TestLayoutEqualsTheOldReaders: over every golden and generated tree, the library gives exactly
// what the code before it gave (the scan of every folder, with both matchers, uncached).
func TestLayoutEqualsTheOldReaders(t *testing.T) {
	trees := archivetest.All(t, goldenRoot, 120)
	compared, links, newest := 0, 0, 0
	for _, tr := range trees {
		root, n := tr.Build(t, t.TempDir())
		links += n
		for _, fold := range []bool{false, true} {
			m := Matcher{FoldCase: fold}
			l := Layout{Root: root, Matcher: m, Container: tr.Container}
			for _, cam := range cameraNames(t, root) {
				dir := filepath.Join(root, cam)
				want, werr := oldScan(m, dir, tr.Container, tr.Now, tr.SegmentSeconds)
				got, gerr := l.Scan(cam, tr.Now, tr.SegmentSeconds)
				old, oerr := m.Scan(dir, tr.Container, tr.Now, tr.SegmentSeconds)
				if !reflect.DeepEqual(want, got) || !reflect.DeepEqual(want, old) || (werr == nil) != (gerr == nil) || (werr == nil) != (oerr == nil) {
					t.Errorf("%s/%s fold=%v:\n old %+v %v\n new %+v %v\n Matcher.Scan %+v %v", tr.Name, cam, fold, want, werr, got, gerr, old, oerr)
				}
				compared++
				if want.Newest != "" {
					newest++
				}
				if n, ok, _ := l.Newest(cam); ok != (want.Newest != "") || (ok && (n.Name() != want.Newest || !n.Info.ModTime().UTC().Equal(want.NewestMTime))) {
					t.Errorf("%s/%s: Newest = %q, the scan says %q", tr.Name, cam, n.Name(), want.Newest)
				}
			}
		}
		p := osx.Current()
		if want, got := oldClaimProblem(root, p, tr.Container), ClaimProblem(root, p, tr.Container); want != got {
			t.Errorf("%s: ClaimProblem\n old %q\n new %q", tr.Name, want, got)
		}
	}
	t.Logf("%d trees, %d folder scans compared (%d with a newest segment), %d links made", len(trees), compared, newest, links)
	if len(trees) < 350 || newest < 500 {
		t.Errorf("the corpus is too thin: %d trees, %d scans with a newest segment", len(trees), newest)
	}
}

func TestLayoutListing(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "archive")
	cam := filepath.Join(root, "Cam_A")
	for _, d := range []string{cam, filepath.Join(root, "_state"), filepath.Join(root, "Other"), filepath.Join(cam, "2026-10-25")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	put(t, cam, "Cam_A_20261017_114500+0300.mkv", 10, 20*time.Minute)
	put(t, cam, "Cam_A_20261017_120000+0300.mkv", 5, time.Second)
	put(t, cam, "notes.txt", 1, time.Hour)
	put(t, root, "readme.txt", 1, time.Hour)
	l := Layout{Root: root, Matcher: Matcher{}, Container: ".mkv"}

	if got := l.CameraDir("Cam_A"); got != cam {
		t.Errorf("CameraDir = %q", got)
	}
	folders, err := l.Folders()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range folders {
		names = append(names, f.Name())
		if f.Path != filepath.Join(root, f.Name()) || f.Dir != root || f.Day != "" {
			t.Errorf("folder entry %+v", f)
		}
	}
	if strings.Join(names, ",") != "Cam_A,Other,readme.txt" {
		t.Errorf("Folders = %v", names)
	}
	cams, err := l.Cameras()
	if err != nil || len(cams) != 2 || cams[0] != (CameraFolder{Name: "Cam_A", Path: cam}) || cams[1].Name != "Other" {
		t.Errorf("Cameras = %+v, %v", cams, err)
	}
	ents, err := l.Entries("Cam_A")
	if err != nil || len(ents) != 4 || ents[0].Name() != "2026-10-25" || !ents[0].IsDir() {
		t.Fatalf("Entries = %v, %v", ents, err)
	}
	files, err := l.Files("Cam_A")
	if err != nil || len(files) != 3 {
		t.Fatalf("Files = %v, %v", files, err)
	}
	for _, f := range files {
		if f.Path != filepath.Join(cam, f.Name()) || f.Dir != cam || f.Day != "" || f.Info.Name() != f.Name() {
			t.Errorf("file %+v", f)
		}
	}
	segs, err := l.Segments("Cam_A")
	if err != nil || len(segs) != 2 || segs[0].Name() != "Cam_A_20261017_120000+0300.mkv" || segs[1].Start.Hour() != 11 {
		t.Errorf("Segments = %+v, %v", segs, err)
	}
	if _, ok, err := l.Newest("Missing"); ok || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Newest of a missing camera = %v, %v", ok, err)
	}
	if _, err := l.Files("Missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Files of a missing camera: %v", err)
	}
	if s, err := l.Scan("Missing", t0, 900); err != nil || s.Dir != filepath.Join(root, "Missing") || s.Files != 0 {
		t.Errorf("Scan of a missing camera = %+v, %v", s, err)
	}
	if _, err := l.Scan("readme.txt", t0, 900); err == nil {
		t.Error("Scan of a file must fail")
	}
}

func TestResolveMatchesNeverJoins(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "archive")
	cam := filepath.Join(root, "Cam_A")
	if err := os.MkdirAll(cam, 0o755); err != nil {
		t.Fatal(err)
	}
	const seg = "Cam_A_20261017_120000+0300.mkv"
	put(t, cam, seg, 5, time.Second)
	put(t, root, "secret.mkv", 1, time.Second)
	put(t, base, "outside.mkv", 1, time.Second)
	l := Layout{Root: root, Container: ".mkv"}
	e, err := l.Resolve("Cam_A", seg)
	if err != nil || e.Path != filepath.Join(cam, seg) || e.Name() != seg {
		t.Fatalf("Resolve = %+v, %v", e, err)
	}
	for _, bad := range []string{"", ".", "..", "../secret.mkv", "..\\secret.mkv", "../../outside.mkv", filepath.Join(cam, seg), "/etc/passwd", `C:\Windows\win.ini`, seg + "\x00", "cam_a_20261017_120000+0300.mkv"} {
		if e, err := l.Resolve("Cam_A", bad); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Resolve(%q) = %+v, %v", bad, e, err)
		}
	}
	l.Matcher.FoldCase = true
	if e, err := l.Resolve("Cam_A", "cam_a_20261017_120000+0300.MKV"); err != nil || e.Path != filepath.Join(cam, seg) {
		t.Errorf("Resolve with case folding = %+v, %v", e, err)
	}
	if _, err := l.Resolve("Nope", seg); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Resolve in a missing camera: %v", err)
	}
}

func TestClassify(t *testing.T) {
	root := filepath.Join(t.TempDir(), "archive")
	l := Layout{Root: root, Matcher: Matcher{}, Container: ".mkv"}
	seg := "Cam_A_20261017_120000+0300.mkv"
	for _, tc := range []struct {
		path string
		want Class
	}{
		{filepath.Join(root, "Cam_A", seg), Class{"Cam_A", "", seg, Spooled}},
		{filepath.Join(root, "Cam_A", "x", "..", seg), Class{"Cam_A", "", seg, Spooled}},
		{filepath.Join(root, "Cam_B", seg), Class{}},
		{filepath.Join(root, "_state", "_state_20261017_120000+0300.mkv"), Class{}},
		{filepath.Join(root, seg), Class{}},
		{filepath.Join(root, "Cam_A", "2026-10-17", seg), Class{"Cam_A", "2026-10-17", seg, Filed}},
		{filepath.Join(root, "Cam_A", "2026-10-18", seg), Class{"Cam_A", "2026-10-18", seg, Misfiled}},
		{filepath.Join(root, "Cam_A", "2026-02-30", seg), Class{}},
		{filepath.Join(root, "Cam_A", "2026-10-17", "x", seg), Class{}},
		{filepath.Join(root, "Cam_B", "2026-10-17", seg), Class{}},
		{filepath.Join(root, "_state", "2026-10-17", "_state_20261017_120000+0300.mkv"), Class{}},
		{filepath.Join(root, "2026-10-17", seg), Class{}},
		{filepath.Join(root+"_old", "Cam_A", seg), Class{}},
		{filepath.Join(root+"_old", "Cam_A", "2026-10-17", seg), Class{}},
		{filepath.Join(root, "Cam_A", "notes.txt"), Class{}},
		{filepath.Join(strings.ToUpper(root), "Cam_A", seg), Class{}},
	} {
		if got := l.Classify(tc.path); got != tc.want {
			t.Errorf("Classify(%q) = %+v, want %+v", tc.path, got, tc.want)
		}
	}
	l.Matcher.FoldCase = true
	if c := l.Classify(filepath.Join(strings.ToUpper(root), "Cam_A", strings.ToLower(seg))); c.Kind != Spooled || c.Camera != "Cam_A" {
		t.Errorf("Classify with case folding = %+v", c)
	}
	if c := l.Classify(filepath.Join(root, "Cam_A", "2026-10-17", strings.ToUpper(seg))); c.Kind != Filed {
		t.Errorf("Classify of a filed name in another case, folding = %+v", c)
	}
	if c := (Layout{Container: ".mkv"}).Classify(filepath.Join("Cam_A", seg)); c.Kind != Foreign {
		t.Error("a layout without a root classifies nothing")
	}
}
