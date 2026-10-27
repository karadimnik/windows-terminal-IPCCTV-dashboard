package archivetest

import (
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ---- spool and day folders (D20, task L2) ----

// Mixed are n seeded trees in the layout of decision D20: per camera a spool of 0 to 3 segments
// and day folders <camera>/<YYYY-MM-DD>/ holding the segments of that local date, with the awkward
// cases: misfiled segments (a day folder that is not their date), another camera's segment and
// foreign files in a day folder, invalid day names (2026-13-45, 2026-02-30), other folders, a "_"
// folder, a folder inside a day folder, a file named like a day, an empty day folder, zero-byte
// segments, symbolic links and junctions named like a day and a linked file in a day folder, the
// clock-change day (2026-10-25: +0300 and +0200 names in one day folder), case variants, an mtime
// tie across folders, and a filed segment newer than the spool's (the spool-first rule).
func Mixed(seed uint64, n int) []Tree {
	rng := rand.New(rand.NewPCG(seed, seed^0x2545f4914f6cdd1d))
	cams := []string{"Cam_A", "Front_Door", "Back Yard", "Gate.Two", "Porch_é"}
	athens := func(t time.Time) time.Time { // EEST +0300 until 2026-10-25 01:00 UTC, then EET +0200
		if t.Before(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) {
			return t.In(time.FixedZone("", 3*3600))
		}
		return t.In(time.FixedZone("", 2*3600))
	}
	var out []Tree
	for i := 0; i < n; i++ {
		now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC).Add(time.Duration(rng.IntN(36*3600)) * time.Second).Truncate(time.Second)
		tr := Tree{Name: fmt.Sprintf("mixed-%03d", i), Container: ".mkv", Now: now, SegmentSeconds: 900}
		if rng.IntN(6) == 0 {
			tr.Container = ".mp4"
		}
		add := func(n Node) { n.Optional = true; tr.Nodes = append(tr.Nodes, n) }
		name := func(cam string, start time.Time) string {
			return cam + "_" + athens(start).Format("20060102_150405-0700") + tr.Container
		}
		day := func(start time.Time) string { return athens(start).Format("2006-01-02") }
		picked := rng.Perm(len(cams))[:1+rng.IntN(3)]
		sort.Ints(picked)
		for _, ci := range picked {
			cam := cams[ci]
			dir := "archive/" + cam + "/"
			add(Node{Path: "archive/" + cam, Kind: "dir"})
			open := now.Truncate(15 * time.Minute)
			// filed: up to 4 days back, a few segments each, closed
			for d := 1 + rng.IntN(4); d >= 0; d-- {
				if rng.IntN(5) == 0 {
					continue
				}
				for k := rng.IntN(5); k > 0; k-- {
					st := open.Add(-time.Duration(2+rng.IntN(96*d+1)) * 15 * time.Minute)
					if d == 0 && rng.IntN(2) == 0 {
						st = time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC).Add(time.Duration(rng.IntN(6)) * 15 * time.Minute) // around the fall-back
					}
					size := int64(1 + rng.IntN(900))
					if rng.IntN(8) == 0 {
						size = 0
					}
					nm := name(cam, st)
					if rng.IntN(7) == 0 {
						nm = strings.ToUpper(nm[:1]) + strings.ToLower(nm[1:]) // a case variant: footage only where names fold
					}
					add(Node{Path: dir + day(st) + "/" + nm, Kind: "file", Bytes: size, MTime: st.Add(900 * time.Second)})
				}
			}
			// the spool: 0 to 3 segments, the newest open
			ns := rng.IntN(4)
			for k := ns - 1; k >= 0; k-- {
				st := open.Add(-time.Duration(k) * 15 * time.Minute)
				mt := st.Add(900 * time.Second)
				if k == 0 {
					mt = now.Add(-time.Duration(rng.IntN(30)) * time.Second)
				}
				add(Node{Path: dir + name(cam, st), Kind: "file", Bytes: int64(1 + rng.IntN(900)), MTime: mt})
			}
			old := open.Add(-30 * time.Hour)
			extras := []func(){
				func() { // misfiled: a day folder that is not its date
					st := old.Add(-15 * time.Minute)
					add(Node{Path: dir + day(st.Add(24*time.Hour)) + "/" + name(cam, st), Kind: "file", Bytes: 5, MTime: st.Add(900 * time.Second)})
				},
				func() { // another camera's segment and a foreign file in a day folder
					add(Node{Path: dir + day(old) + "/" + name("Other", old), Kind: "file", Bytes: 6, MTime: old.Add(900 * time.Second)})
					add(Node{Path: dir + day(old) + "/notes.txt", Kind: "file", Bytes: 2, MTime: old})
				},
				func() { // invalid day names and other folders: never entered
					for _, bad := range []string{"2026-13-45", "2026-02-30", "old", "_tmp", "2026-1-05", "20261024"} {
						add(Node{Path: dir + bad + "/" + name(cam, old), Kind: "file", Bytes: 3, MTime: old.Add(900 * time.Second)})
					}
				},
				func() { // a folder inside a day folder: not entered
					add(Node{Path: dir + day(old) + "/deeper/" + name(cam, old), Kind: "file", Bytes: 4, MTime: old.Add(900 * time.Second)})
				},
				func() { // a file named like a day, and an empty day folder
					add(Node{Path: dir + "2026-10-20", Kind: "file", Bytes: 1, MTime: old})
					add(Node{Path: dir + "2026-10-19", Kind: "dir"})
				},
				func() { // a link and a junction named like a day: never followed
					tgt := fmt.Sprintf("outside/%s-%d", strings.ReplaceAll(cam, " ", "_"), i)
					add(Node{Path: tgt + "/" + name(cam, old.Add(-48*time.Hour)), Kind: "file", Bytes: 7, MTime: old})
					add(Node{Path: tgt + "/" + name(cam, old.Add(-72*time.Hour)), Kind: "file", Bytes: 7, MTime: old})
					add(Node{Path: dir + day(old.Add(-48*time.Hour)), Kind: "dirlink", Target: tgt})
					add(Node{Path: dir + day(old.Add(-72*time.Hour)), Kind: "junction", Target: tgt})
				},
				func() { // a linked file in a day folder, named like a segment
					tgt := fmt.Sprintf("outside/clip-%d-%d%s", i, ci, tr.Container)
					add(Node{Path: tgt, Kind: "file", Bytes: 8, MTime: old})
					add(Node{Path: dir + day(old) + "/" + name(cam, old.Add(-30*time.Minute)), Kind: "filelink", Target: tgt})
				},
				func() { // a filed segment newer than anything in the spool
					st := open.Add(-45 * time.Minute)
					add(Node{Path: dir + day(st) + "/" + name(cam, st), Kind: "file", Bytes: 9, MTime: now.Add(time.Duration(5+rng.IntN(100)) * time.Second)})
				},
				func() { // an mtime tie between the spool and a day folder
					st := open.Add(-60 * time.Minute)
					add(Node{Path: dir + day(st) + "/" + name(cam, st), Kind: "file", Bytes: 9, MTime: now.Add(-10 * time.Second)})
					add(Node{Path: dir + "zz_foreign.mkv", Kind: "file", Bytes: 1, MTime: now.Add(-10 * time.Second)})
				},
			}
			for _, x := range extras {
				if rng.IntN(4) == 0 {
					x()
				}
			}
		}
		if rng.IntN(3) == 0 {
			add(Node{Path: "archive/_state/status.json", Kind: "file", Bytes: 2, MTime: now})
		}
		out = append(out, tr)
	}
	return out
}

// ---- the brute-force walk: what a tree holds by the rules of D20, read with os.ReadDir and
// os.Lstat alone (never through the archive package), for the tests to compare readers with ----

// TFile is one non-directory entry as os.Lstat reports it.
type TFile struct {
	Name, Path, Day string // Day: the day folder holding it, "" in the spool
	Mode            fs.FileMode
	Size            int64
	MTime           time.Time
}

// TSeg is a file whose name is a segment of the camera.
type TSeg struct {
	TFile
	Start time.Time
}

// TCamera is one camera folder.
type TCamera struct {
	Name     string
	Dir      string
	Spool    []TFile            // every non-directory entry of the camera folder, name order
	Days     []string           // the day folders: real directories, YYYY-MM-DD, a calendar date; name order
	DayFiles map[string][]TFile // every non-directory entry of each day folder, name order
	Footage  []TSeg             // regular segments in the spool or filed under their own date, newest first (mtime, then later name)
	Misfiled []TSeg             // regular segments of the camera filed under another date
}

// Truth is a tree's cameras: the root's real folders not starting with "_", name order.
type Truth struct {
	Root    string
	Cameras []TCamera
}

var stampRE = regexp.MustCompile(`^[0-9]{8}_[0-9]{6}[+-][0-9]{4}$`)
var dayRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func foldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// SegStart is the start written in name when name is <cam>_YYYYMMDD_HHMMSS+ZZZZ<ext> (fold: ASCII
// case-insensitive camera and extension).
func SegStart(name, cam, ext string, fold bool) (time.Time, bool) {
	eq := func(a, b string) bool { return a == b || (fold && foldASCII(a, b)) }
	if len(name) < len(cam)+21+len(ext) || !eq(name[len(name)-len(ext):], ext) {
		return time.Time{}, false
	}
	base := name[:len(name)-len(ext)]
	stamp, prefix := base[len(base)-20:], base[:len(base)-21]
	if base[len(base)-21] != '_' || !stampRE.MatchString(stamp) || !eq(prefix, cam) {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102_150405-0700", stamp)
	if err != nil || stamp[16:18] > "14" || stamp[18:20] > "59" {
		return time.Time{}, false
	}
	return t, true
}

// IsDay is the day-folder name rule.
func IsDay(name string) bool {
	if !dayRE.MatchString(name) {
		return false
	}
	_, err := time.Parse("2006-01-02", name)
	return err == nil
}

func lstatFiles(t interface{ Fatal(...any) }, dir, day string) (files []TFile, dirs []string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Type() == fs.ModeDir {
			dirs = append(dirs, e.Name())
			continue
		}
		if fi.IsDir() {
			continue
		}
		files = append(files, TFile{Name: e.Name(), Path: p, Day: day, Mode: fi.Mode(), Size: fi.Size(), MTime: fi.ModTime()})
	}
	return files, dirs
}

// Walk reads root by the D20 rules for container (".mkv") and the case rule fold.
func Walk(t interface{ Fatal(...any) }, root, container string, fold bool) Truth {
	tr := Truth{Root: root}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		dir := filepath.Join(root, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil || strings.HasPrefix(e.Name(), "_") || fi.Mode().Type() != fs.ModeDir {
			continue
		}
		c := TCamera{Name: e.Name(), Dir: dir, DayFiles: map[string][]TFile{}}
		var subs []string
		c.Spool, subs = lstatFiles(t, dir, "")
		for _, s := range subs {
			if IsDay(s) {
				c.Days = append(c.Days, s)
				c.DayFiles[s], _ = lstatFiles(t, filepath.Join(dir, s), s)
			}
		}
		consider := func(f TFile) {
			start, ok := SegStart(f.Name, c.Name, container, fold)
			if !ok || !f.Mode.IsRegular() {
				return
			}
			s := TSeg{TFile: f, Start: start}
			if f.Day == "" || start.Format("2006-01-02") == f.Day {
				c.Footage = append(c.Footage, s)
			} else {
				c.Misfiled = append(c.Misfiled, s)
			}
		}
		for _, f := range c.Spool {
			consider(f)
		}
		for _, d := range c.Days {
			for _, f := range c.DayFiles[d] {
				consider(f)
			}
		}
		sort.SliceStable(c.Footage, func(i, j int) bool { return Newer(c.Footage[i].TFile, c.Footage[j].TFile) })
		tr.Cameras = append(tr.Cameras, c)
	}
	return tr
}

// Newer is the "which file is open" order: later mtime, then later name.
func Newer(a, b TFile) bool {
	if !a.MTime.Equal(b.MTime) {
		return a.MTime.After(b.MTime)
	}
	return a.Name > b.Name
}

// Camera is the named camera of the truth, false when the root has no such real folder.
func (tr Truth) Camera(name string) (TCamera, bool) {
	for _, c := range tr.Cameras {
		if c.Name == name {
			return c, true
		}
	}
	return TCamera{}, false
}

// footageIn are the camera's footage segments in one folder (day "" = the spool), name order.
func (c TCamera) footageIn(day string) []TSeg {
	var out []TSeg
	for _, s := range c.Footage {
		if s.Day == day {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// newestIn is the newest of segs (name order: the later name wins an mtime tie).
func newestIn(segs []TSeg) (TSeg, bool) {
	var n TSeg
	ok := false
	for _, s := range segs {
		if !ok || Newer(s.TFile, n.TFile) {
			n, ok = s, true
		}
	}
	return n, ok
}

// Live is the spool-first rule: the day folders (newest first) read while fewer than two segments
// have been seen, the newest segment (the spool's when it holds one, else that of the newest day
// folder read that holds one) and the newest closed write (every spool file but the newest, and
// the read days' footage but the newest).
func (c TCamera) Live() (read []string, newest TSeg, ok bool, closed time.Time) {
	spool := c.footageIn("")
	seen := len(spool)
	for i := len(c.Days) - 1; i >= 0 && seen < 2; i-- {
		read = append(read, c.Days[i])
		seen += len(c.footageIn(c.Days[i]))
	}
	newest, ok = newestIn(spool)
	for _, d := range read {
		if ok {
			break
		}
		newest, ok = newestIn(c.footageIn(d))
	}
	later := func(p string, m time.Time) {
		if (!ok || p != newest.Path) && m.UTC().After(closed) {
			closed = m.UTC()
		}
	}
	for _, f := range c.Spool {
		later(f.Path, f.MTime)
	}
	for _, d := range read {
		for _, s := range c.footageIn(d) {
			later(s.Path, s.MTime)
		}
	}
	return read, newest, ok, closed
}

// AllFiles are the spool's and every day folder's non-directory entries.
func (c TCamera) AllFiles() []TFile {
	out := append([]TFile{}, c.Spool...)
	for _, d := range c.Days {
		out = append(out, c.DayFiles[d]...)
	}
	return out
}

// Filed is a tree of cams cameras, each with days day folders of perDay closed 15-minute segments
// (the days before now's local date, +0300 names) and spool segments in the spool, the newest of
// them open (written 5 s before now). The newest filed segment is older than the spool's oldest.
func Filed(name string, cams, days, perDay, spool int, now time.Time) Tree {
	tr := Tree{Name: name, Container: ".mkv", Now: now, SegmentSeconds: 900}
	zone := time.FixedZone("", 3*3600)
	open := now.In(zone).Truncate(15 * time.Minute)
	for c := 0; c < cams; c++ {
		cam := "Cam_" + string(rune('A'+c))
		dir := "archive/" + cam + "/"
		tr.Nodes = append(tr.Nodes, Node{Path: "archive/" + cam, Kind: "dir"})
		for d := days; d >= 1; d-- {
			day := open.AddDate(0, 0, -d)
			midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, zone)
			for k := 0; k < perDay; k++ {
				st := midnight.Add(time.Duration(k) * 15 * time.Minute)
				tr.Nodes = append(tr.Nodes, Node{Path: dir + st.Format("2006-01-02") + "/" + cam + "_" + st.Format("20060102_150405-0700") + ".mkv",
					Kind: "file", Bytes: 3, MTime: st.Add(15 * time.Minute)})
			}
		}
		for k := spool - 1; k >= 0; k-- {
			st := open.Add(-time.Duration(k) * 15 * time.Minute)
			mt := st.Add(15 * time.Minute)
			if k == 0 {
				mt = now.Add(-5 * time.Second)
			}
			tr.Nodes = append(tr.Nodes, Node{Path: dir + cam + "_" + st.Format("20060102_150405-0700") + ".mkv", Kind: "file", Bytes: 4, MTime: mt})
		}
	}
	return tr
}

// Flat is the tree with every file of a day folder (archive/<camera>/<YYYY-MM-DD>/<name>) moved
// into its camera folder: the archive as it was before D20, for comparing a reader on a mixed tree
// with the old reader on its flat copy.
func (tr Tree) Flat() Tree {
	out := tr
	out.Name += "/flat"
	out.Nodes = nil
	for _, n := range tr.Nodes {
		parts := strings.Split(n.Path, "/")
		if len(parts) == 4 && parts[0] == "archive" && IsDay(parts[2]) {
			if n.Kind == "dir" {
				continue
			}
			n.Path = strings.Join([]string{parts[0], parts[1], parts[3]}, "/")
		}
		out.Nodes = append(out.Nodes, n)
	}
	return out
}

// Resolve is a clip request's name resolved by the documented procedure, with os.ReadDir: the
// camera folder's entries (exact, then ASCII-folded when fold), then those of the listed day folder
// whose name is the date written in name.
func (c TCamera) Resolve(name string, fold bool) (string, bool) {
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
	start, ok := SegStart(name[:dot]+".x", name[:dot-21], ".x", false)
	if !ok {
		return "", false
	}
	for _, d := range c.Days {
		if d == start.Format("2006-01-02") {
			return look(filepath.Join(c.Dir, d))
		}
	}
	return "", false
}
