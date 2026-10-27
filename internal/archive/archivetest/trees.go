// Package archivetest builds archive trees on disk for the old-versus-new equivalence tests of the
// layout library (task L1): the prune goldens (200-tree corpus and the tables), the names goldens,
// and generated trees with the awkward cases. Only *_test.go files import it.
package archivetest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Node is one thing to create, its Path relative to the base folder with "/" separators. Kinds:
// file, dir, filelink and dirlink (symbolic links to Target, also relative to the base), junction
// (a directory junction on Windows, a symbolic link elsewhere).
type Node struct {
	Path, Kind, Target string
	Bytes              int64
	MTime, CTime       time.Time
	// Optional: a node the file system may refuse (a name Windows forbids, a name equal but for
	// case to one already made on a case-insensitive volume); it is skipped, not a failure.
	Optional bool
}

// Tree is an archive under <base>/archive plus whatever sits beside it.
type Tree struct {
	Name           string
	Container      string // ".mkv"
	Now            time.Time
	SegmentSeconds int
	Nodes          []Node
}

type linkError struct {
	err error
	out string
}

func (e *linkError) Error() string { return e.err.Error() + ": " + strings.TrimSpace(e.out) }

// Build creates the tree under base and returns the archive root, <base>/archive (created even
// when no node lies in it). Links the OS refuses to make (Windows without the privilege) are
// skipped; Links reports how many were made.
func (tr Tree) Build(t testing.TB, base string) (root string, links int) {
	t.Helper()
	root = filepath.Join(base, "archive")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	abs := func(rel string) string { return filepath.Join(base, filepath.FromSlash(rel)) }
	for _, n := range tr.Nodes {
		p := abs(n.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			if n.Optional {
				continue
			}
			t.Fatal(err)
		}
		switch n.Kind {
		case "dir":
			if err := os.MkdirAll(p, 0o755); err != nil && !n.Optional {
				t.Fatal(err)
			}
		case "file":
			f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				if n.Optional {
					continue
				}
				t.Fatal(err)
			}
			b := make([]byte, n.Bytes)
			for i := range b {
				b[i] = n.Path[i%len(n.Path)]
			}
			_, werr := f.Write(b)
			if cerr := f.Close(); werr != nil || cerr != nil {
				t.Fatal(werr, cerr)
			}
			mt := n.MTime
			if mt.IsZero() {
				mt = tr.Now
			}
			if err := os.Chtimes(p, mt, mt); err != nil {
				t.Fatal(err)
			}
			if !n.CTime.IsZero() {
				if err := setCreated(p, n.CTime); err != nil {
					t.Fatal(err)
				}
			}
		case "filelink", "dirlink":
			if err := os.Symlink(abs(n.Target), p); err == nil {
				links++
			}
		case "junction":
			if err := junction(p, abs(n.Target)); err == nil {
				links++
			}
		default:
			t.Fatalf("node kind %q", n.Kind)
		}
	}
	return root, links
}

// ---- the prune goldens ----

type gEntry struct {
	Path, Kind, Target string
	Bytes              int64
	MTime, CTime       string
}

type gScenario struct {
	Name, Now, Container string
	SegmentSeconds       int
	Entries              []gEntry
	Steps                []struct {
		Op      string
		Entries []gEntry
	}
}

// goldenScale: the harness writes files 65536 times smaller than the sizes in a golden.
const goldenScale = 65536

// PruneGoldens are the trees of testdata/golden/prune (dir): every table scenario and every corpus
// line, each with its initial entries and the entries its "add" steps create.
func PruneGoldens(t testing.TB, dir string) []Tree {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "table", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no table goldens under %s: %v", dir, err)
	}
	var out []Tree
	for _, f := range files {
		if filepath.Base(f) == "pruneconfig.json" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s gScenario
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out = append(out, s.tree(t, "table/"+s.Name))
	}
	fh, err := os.Open(filepath.Join(dir, "corpus.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var s gScenario
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s.tree(t, s.Name))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (s gScenario) tree(t testing.TB, name string) Tree {
	tr := Tree{Name: name, Container: "." + strings.TrimPrefix(s.Container, "."), Now: parseTime(t, s.Now), SegmentSeconds: s.SegmentSeconds}
	add := func(es []gEntry) {
		for _, e := range es {
			n := Node{Path: e.Path, Kind: e.Kind, Target: e.Target, Bytes: e.Bytes / goldenScale}
			if e.MTime != "" {
				n.MTime = parseTime(t, e.MTime)
			}
			if e.CTime != "" {
				n.CTime = parseTime(t, e.CTime)
			}
			n.Optional = true // a later "add" may write a name an earlier entry holds
			tr.Nodes = append(tr.Nodes, n)
		}
	}
	add(s.Entries)
	for _, st := range s.Steps {
		if st.Op == "add" {
			add(st.Entries)
		}
	}
	return tr
}

func parseTime(t testing.TB, s string) time.Time {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return v.UTC()
}

// ---- the names goldens ----

// NameGoldens are trees made of the names in testdata/golden/names/segments.json (path): one tree
// per container and camera, whose folder holds every sample, accepted and rejected name that a
// file system can hold, each written ten minutes after the previous one.
func NameGoldens(t testing.TB, path string) []Tree {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.TrimPrefix(string(b), "\ufeff"))
	var doc struct {
		Containers []struct {
			Extension string
			Names     []struct {
				SafeName                string
				Samples, Accept, Reject []struct{ Name string }
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	var out []Tree
	for _, c := range doc.Containers {
		for _, n := range c.Names {
			tr := Tree{Name: "names/" + c.Extension + "/" + n.SafeName, Container: c.Extension, Now: now, SegmentSeconds: 900}
			at := now.Add(-24 * time.Hour)
			var all []string
			for _, group := range [][]struct{ Name string }{n.Samples, n.Accept, n.Reject} {
				for _, x := range group {
					all = append(all, x.Name)
				}
			}
			for _, name := range all {
				if name == "" || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
					continue
				}
				at = at.Add(10 * time.Minute)
				tr.Nodes = append(tr.Nodes, Node{Path: "archive/" + n.SafeName + "/" + name, Kind: "file", Bytes: 7, MTime: at, Optional: true})
			}
			out = append(out, tr)
		}
	}
	return out
}

// ---- generated trees ----

// Generated are n seeded trees with the cases the goldens do not cover: names with spaces, case
// differences, foreign files, links and junctions inside and at the top, zero-byte files, 8.3-style
// short names, +0300 and +0200 segments in one folder (the clock-change night), future-dated files,
// sub-folders inside a camera folder, a top-level file and a "_" folder.
func Generated(seed uint64, n int) []Tree {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	cams := []string{"Cam_A", "Front_Door", "Back Yard", "cam_a", "Garage%Y", "Gate.Two", "Porch_é"}
	var out []Tree
	for i := 0; i < n; i++ {
		now := time.Date(2026, 10, 25, 4, 10, 0, 0, time.UTC).Add(time.Duration(rng.IntN(48*3600)) * time.Second)
		tr := Tree{Name: fmt.Sprintf("generated-%03d", i), Container: ".mkv", Now: now, SegmentSeconds: 900}
		if rng.IntN(5) == 0 {
			tr.Container = ".mp4"
		}
		add := func(n Node) { n.Optional = true; tr.Nodes = append(tr.Nodes, n) }
		picked := rng.Perm(len(cams))[:1+rng.IntN(4)]
		sort.Ints(picked)
		for _, ci := range picked {
			cam := cams[ci]
			dir := "archive/" + cam + "/"
			add(Node{Path: "archive/" + cam, Kind: "dir"})
			segs := rng.IntN(30)
			start := now.Add(-time.Duration(segs) * 15 * time.Minute).Truncate(15 * time.Minute)
			for s := 0; s < segs; s++ {
				st := start.Add(time.Duration(s) * 15 * time.Minute)
				// the clock-change night: the same instants written with +0300 and +0200
				zone := time.FixedZone("", 3*3600)
				if st.After(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) && rng.IntN(3) > 0 {
					zone = time.FixedZone("", 2*3600)
				}
				name := cam + "_" + st.In(zone).Format("20060102_150405-0700") + tr.Container
				mt := st.Add(900 * time.Second)
				size := int64(1 + rng.IntN(4000))
				switch rng.IntN(12) {
				case 0:
					size = 0 // a child killed before its header reached disk
				case 1:
					mt = now.Add(time.Duration(2+rng.IntN(3000)) * time.Second) // future-dated
				case 2:
					name = strings.ToUpper(name[:1]) + strings.ToLower(name[1:]) // case difference
				case 3:
					mt = st.Add(time.Duration(rng.IntN(900)) * time.Second) // equal mtimes are made below
				}
				if s == segs-1 {
					mt = now.Add(-time.Duration(rng.IntN(20)) * time.Second) // the open segment
				}
				if s > 0 && rng.IntN(15) == 0 {
					mt = tr.Nodes[len(tr.Nodes)-1].MTime // an mtime tie
				}
				add(Node{Path: dir + name, Kind: "file", Bytes: size, MTime: mt, CTime: st})
			}
			for _, foreign := range []string{"notes.txt", "Thumbs.db", "CAM_A~1.MKV", cam + "_20261025_033000+0300" + tr.Container + ".tmp",
				"Other_20261025_033000+0300.mkv", cam + "_20261025_033000+0300.MKV", strings.ReplaceAll(cam, "_", " ") + "_20261025_033000+0300.mkv",
				cam + "_20261025_033000+0300 (1).mkv", "~$" + cam + ".mkv"} {
				if rng.IntN(4) == 0 {
					add(Node{Path: dir + foreign, Kind: "file", Bytes: int64(rng.IntN(3)), MTime: now.Add(-time.Duration(rng.IntN(7200)) * time.Second)})
				}
			}
			if rng.IntN(4) == 0 { // a day-like folder: flat readers never enter it
				add(Node{Path: dir + "2026-10-24/" + cam + "_20261024_120000+0300" + tr.Container, Kind: "file", Bytes: 9, MTime: now.Add(-20 * time.Hour)})
			}
			if rng.IntN(4) == 0 {
				add(Node{Path: "outside/clip_" + fmt.Sprint(ci) + tr.Container, Kind: "file", Bytes: 0, MTime: now.Add(-time.Hour)})
				add(Node{Path: dir + cam + "_20261025_000000+0300" + tr.Container, Kind: "filelink", Target: "outside/clip_" + fmt.Sprint(ci) + tr.Container})
			}
			if rng.IntN(5) == 0 {
				add(Node{Path: "outside/dir" + fmt.Sprint(ci) + "/x.mkv", Kind: "file", Bytes: 3, MTime: now.Add(-time.Hour)})
				kind := "dirlink"
				if rng.IntN(2) == 0 {
					kind = "junction"
				}
				add(Node{Path: dir + "linked", Kind: kind, Target: "outside/dir" + fmt.Sprint(ci)})
			}
		}
		if rng.IntN(3) == 0 {
			add(Node{Path: "archive/readme.txt", Kind: "file", Bytes: 5, MTime: now.Add(-time.Hour)})
		}
		if rng.IntN(3) == 0 {
			add(Node{Path: "archive/_state/status.json", Kind: "file", Bytes: 2, MTime: now})
		}
		if rng.IntN(3) == 0 {
			add(Node{Path: "outside/Linked/Linked_20261025_010000+0300" + tr.Container, Kind: "file", Bytes: 4, MTime: now.Add(-time.Hour)})
			kind := "dirlink"
			if rng.IntN(2) == 0 {
				kind = "junction"
			}
			add(Node{Path: "archive/Linked", Kind: kind, Target: "outside/Linked"})
		}
		out = append(out, tr)
	}
	return out
}

// All is every tree: the prune goldens (under goldenDir, testdata/golden), the names goldens and
// n generated ones.
func All(t testing.TB, goldenDir string, n int) []Tree {
	out := PruneGoldens(t, filepath.Join(goldenDir, "prune"))
	out = append(out, NameGoldens(t, filepath.Join(goldenDir, "names", "segments.json"))...)
	return append(out, Generated(20261027, n)...)
}
