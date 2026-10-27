package archive

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive/archivetest"
	"holocron/internal/osx"
)

// own writes a valid ownership marker for root (the trees of these tests hold folders that would
// block a claim; ownership is what the mover checks, not how it came about).
func own(t testing.TB, root string) {
	t.Helper()
	body, _ := json.Marshal(Marker{Owner: MarkerOwner, Schema: markerSchema, RootPath: root, CreatedAt: "2026-10-27T00:00:00+02:00", Note: markerNote})
	if err := writeAtomic(MarkerPath(root), body, osx.Current()); err != nil {
		t.Fatal(err)
	}
}

func moveLayout(root, container string) Layout {
	return Layout{Root: root, Matcher: MatcherFor(osx.Current()), Container: container}
}

func moveOpts(base string, now time.Time) MoveOptions {
	return MoveOptions{Platform: osx.Current(), Now: now, Margin: 120 * time.Second, Sandbox: base}
}

// snap is every regular file and folder under root, never following a link: path -> size, hash
// and write time ("dir" for a folder, "link" for anything else that is not a regular file).
func snap(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if strings.HasPrefix(rel, "_state") {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case IsReparse(fi):
			out[rel] = "link"
			if d.IsDir() {
				return fs.SkipDir
			}
		case fi.IsDir():
			out[rel] = "dir"
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("%d %x %d", fi.Size(), sha256.Sum256(b), fi.ModTime().UnixNano())
		default:
			out[rel] = "link"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// contents is the multiset of (name, size, hash, time) of the regular files in s, wherever they are.
func contents(s map[string]string) []string {
	var out []string
	for p, v := range s {
		if v != "dir" && v != "link" {
			out = append(out, filepath.Base(p)+" "+v)
		}
	}
	sort.Strings(out)
	return out
}

func sameList(a, b []string) bool {
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}

// keepsNewestTwo: each camera's two newest spool segments before the run are in its spool after it.
func keepsNewestTwo(t *testing.T, before archivetest.Truth, after archivetest.Truth, fold bool, ext string) {
	t.Helper()
	for _, c := range before.Cameras {
		var segs []archivetest.TFile
		for _, f := range c.Spool {
			if _, ok := archivetest.SegStart(f.Name, c.Name, ext, fold); ok && f.Mode.IsRegular() {
				segs = append(segs, f)
			}
		}
		sort.SliceStable(segs, func(i, j int) bool { return archivetest.Newer(segs[i], segs[j]) })
		ac, _ := after.Camera(c.Name)
		in := map[string]bool{}
		for _, f := range ac.Spool {
			in[f.Name] = true
		}
		for i := 0; i < len(segs) && i < 2; i++ {
			if !in[segs[i].Name] {
				t.Errorf("%s: %s, one of the two newest spool segments, left the spool", c.Name, segs[i].Name)
			}
		}
	}
}

// C1: over generated mixed trees, filing keeps every file (multiset of name, size, hash, write
// time), never moves the two newest spool segments, files only to the date in the name, and a
// second run moves nothing. A sample of 12 trees by default (Windows-native about 11 s), 60 with
// HOLOCRON_FULL_EQUIV=1. On the flat copy of each tree, filing then unfiling restores the tree
// exactly (paths, sizes, hashes, write times, folders).
func TestFileDaysKeepsEveryFile(t *testing.T) {
	fold := MatcherFor(osx.Current()).FoldCase
	filed, restored := 0, 0
	defer func() { t.Logf("%d files filed, %d flat trees restored", filed, restored) }()
	n := 12
	if os.Getenv("HOLOCRON_FULL_EQUIV") == "1" {
		n = 60
	}
	for _, tr := range archivetest.Mixed(20261027, n) {
		for _, tree := range []archivetest.Tree{tr, tr.Flat()} {
			base := t.TempDir()
			root, _ := tree.Build(t, base)
			own(t, root)
			l := moveLayout(root, tree.Container)
			o := moveOpts(base, tree.Now)
			before := snap(t, root)
			truth := archivetest.Walk(t, root, tree.Container, fold)
			rep := FileDays(l, o)
			filed += len(rep.Moved)
			if rep.Refused != "" || len(rep.Errors) > 0 || len(rep.Busy) > 0 {
				t.Fatalf("%s: refused %q, errors %v, busy %v", tree.Name, rep.Refused, rep.Errors, rep.Busy)
			}
			mid := snap(t, root)
			if !sameList(contents(before), contents(mid)) {
				t.Fatalf("%s: the files differ after filing", tree.Name)
			}
			keepsNewestTwo(t, truth, archivetest.Walk(t, root, tree.Container, fold), fold, tree.Container)
			for _, m := range rep.Moved {
				if c := l.Classify(m.To); c.Kind != Filed {
					t.Errorf("%s: %s was filed as %v", tree.Name, m.To, c.Kind)
				}
			}
			if again := FileDays(l, o); len(again.Moved) != 0 {
				t.Errorf("%s: a second run moved %d file(s)", tree.Name, len(again.Moved))
			}
			if tree.Name != tr.Name {
				o.RecorderRunning = false
				restored++
				un := UnfileDays(l, o)
				if un.Refused != "" || len(un.Errors) > 0 {
					t.Fatalf("%s: unfile refused %q errors %v", tree.Name, un.Refused, un.Errors)
				}
				if after := snap(t, root); !sameList(sortedKV(before), sortedKV(after)) {
					restored--
					t.Errorf("%s: file then unfile did not restore the tree:\nbefore %v\nafter  %v", tree.Name, sortedKV(before), sortedKV(after))
				}
			}
		}
	}
}

func sortedKV(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+" = "+v)
	}
	sort.Strings(out)
	return out
}

// segName is a segment of cam starting at st (+0200).
func segName(cam string, st time.Time) string {
	return cam + "_" + st.In(time.FixedZone("", 2*3600)).Format("20060102_150405-0700") + ".mkv"
}

var mt0 = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

// spoolTree is one camera Cam with n closed 15-minute segments in its spool, the newest ending at
// mt0 - age, plus extra nodes.
func spoolTree(n int, age time.Duration, extra ...archivetest.Node) archivetest.Tree {
	tr := archivetest.Tree{Name: "spool", Container: ".mkv", Now: mt0, SegmentSeconds: 900}
	tr.Nodes = append(tr.Nodes, archivetest.Node{Path: "archive/Cam", Kind: "dir"})
	for k := 0; k < n; k++ {
		st := mt0.Add(-age).Add(-time.Duration(n-k) * 15 * time.Minute)
		tr.Nodes = append(tr.Nodes, archivetest.Node{Path: "archive/Cam/" + segName("Cam", st), Kind: "file", Bytes: int64(10 + k), MTime: st.Add(15 * time.Minute)})
	}
	tr.Nodes = append(tr.Nodes, extra...)
	return tr
}

func build(t *testing.T, tr archivetest.Tree) (base, root string, links int) {
	base = t.TempDir()
	root, links = tr.Build(t, base)
	own(t, root)
	return base, root, links
}

// C2: the open and the newest closed segment never move, at any spool size 0..4; the rest do.
func TestFileDaysKeepsTwoNewest(t *testing.T) {
	for n := 0; n <= 4; n++ {
		base, root, _ := build(t, spoolTree(n, time.Hour))
		l := moveLayout(root, ".mkv")
		rep := FileDays(l, moveOpts(base, mt0))
		if want := max(0, n-2); len(rep.Moved) != want {
			t.Errorf("spool of %d: moved %d, want %d", n, len(rep.Moved), want)
		}
		segs, _ := l.Segments("Cam")
		for i, s := range segs {
			if (i < 2) != (s.Day == "") {
				t.Errorf("spool of %d: segment %d (%s) is in %q", n, i, s.Name(), s.Day)
			}
		}
	}
}

// C2: a segment written less than the margin ago stays; the same tree with the margin passed files it.
func TestFileDaysMargin(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		want int
	}{{119 * time.Second, 0}, {120 * time.Second, 1}} {
		// three segments, the oldest last written age before now; the margin rule decides it
		base, root, _ := build(t, spoolTree(3, c.age-30*time.Minute))
		rep := FileDays(moveLayout(root, ".mkv"), moveOpts(base, mt0))
		if len(rep.Moved) != c.want {
			t.Errorf("age %v: moved %d, want %d", c.age, len(rep.Moved), c.want)
		}
	}
}

// C2: only plain segments of the camera move: links, folders, foreign files and other cameras'
// segments in the spool stay; a day folder that is a link or junction is never entered; the plain
// segment beside them (positive control) moves.
func TestFileDaysMovesOnlyPlainSegments(t *testing.T) {
	old := mt0.Add(-48 * time.Hour)
	day := DayName(old.In(time.FixedZone("", 2*3600)))
	linked := old.Add(-24 * time.Hour)
	tr := spoolTree(2, 0,
		archivetest.Node{Path: "archive/Cam/" + segName("Cam", old), Kind: "file", Bytes: 5, MTime: old},
		archivetest.Node{Path: "archive/Cam/" + segName("Other", old), Kind: "file", Bytes: 5, MTime: old},
		archivetest.Node{Path: "archive/Cam/notes.txt", Kind: "file", Bytes: 5, MTime: old},
		archivetest.Node{Path: "archive/Cam/" + segName("Cam", old.Add(time.Minute)) + "/x", Kind: "file", Bytes: 5, MTime: old},
		archivetest.Node{Path: "outside/clip.mkv", Kind: "file", Bytes: 5, MTime: old},
		archivetest.Node{Path: "archive/Cam/" + segName("Cam", old.Add(2*time.Minute)), Kind: "filelink", Target: "outside/clip.mkv"},
		archivetest.Node{Path: "archive/Cam/" + segName("Cam", linked), Kind: "file", Bytes: 6, MTime: linked},
		archivetest.Node{Path: "outside/day", Kind: "dir"},
		archivetest.Node{Path: "archive/Cam/" + DayName(linked.In(time.FixedZone("", 2*3600))), Kind: "junction", Target: "outside/day"},
	)
	base, root, links := build(t, tr)
	rep := FileDays(moveLayout(root, ".mkv"), moveOpts(base, mt0))
	if len(rep.Moved) != 1 || filepath.Base(filepath.Dir(rep.Moved[0].To)) != day || filepath.Base(rep.Moved[0].From) != segName("Cam", old) {
		t.Fatalf("moved %+v, want only %s into %s", rep.Moved, segName("Cam", old), day)
	}
	if links > 0 && (len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0].Why, "link")) {
		t.Errorf("the segment of the linked day folder: skipped %+v", rep.Skipped)
	}
	if ents, _ := os.ReadDir(filepath.Join(base, "outside", "day")); len(ents) != 0 {
		t.Errorf("the junction's target was written into: %v", ents)
	}
}

// C2: a file swapped for a link between the listing and the rename is not followed or moved.
func TestFileDaysRevalidatesBeforeRename(t *testing.T) {
	for _, swap := range []bool{false, true} {
		base, root, _ := build(t, spoolTree(3, time.Hour, archivetest.Node{Path: "outside/clip.mkv", Kind: "file", Bytes: 9, MTime: mt0}))
		target := filepath.Join(base, "outside", "clip.mkv")
		var linked bool
		testHookBeforeMove = func(src string) {
			if swap {
				_ = os.Remove(src)
				linked = os.Symlink(target, src) == nil
			}
		}
		rep := FileDays(moveLayout(root, ".mkv"), moveOpts(base, mt0))
		testHookBeforeMove = nil
		switch {
		case !swap && len(rep.Moved) != 1: // positive control
			t.Errorf("no swap: moved %d, want 1", len(rep.Moved))
		case swap && !linked:
			t.Log("this OS refused the symbolic link; the swap ran as a vanished file")
			fallthrough
		case swap:
			if len(rep.Moved) != 0 || len(rep.Skipped) != 1 {
				t.Errorf("swapped: moved %+v skipped %+v", rep.Moved, rep.Skipped)
			}
			if fi, err := os.Lstat(target); err != nil || fi.Size() != 9 {
				t.Errorf("the link's target changed: %v", err)
			}
		}
	}
}

// C1: a destination that exists is a conflict and both files stay intact; the check catches a
// rename that replaces (positive control: the broken variant fails it).
func TestFileDaysNeverReplaces(t *testing.T) {
	check := func() error {
		first := mt0.Add(-time.Hour - 45*time.Minute)
		taken := "archive/Cam/" + DayName(first.In(time.FixedZone("", 2*3600))) + "/" + segName("Cam", first)
		base, root, _ := build(t, spoolTree(3, time.Hour, archivetest.Node{Path: taken, Kind: "file", Bytes: 77, MTime: mt0}))
		before := contents(snap(t, root))
		rep := FileDays(moveLayout(root, ".mkv"), moveOpts(base, mt0))
		if after := contents(snap(t, root)); !sameList(before, after) {
			return fmt.Errorf("files lost or changed: before %v after %v", before, after)
		}
		if len(rep.Conflicts) != 1 || len(rep.Moved) != 0 {
			return fmt.Errorf("conflicts %+v moved %+v", rep.Conflicts, rep.Moved)
		}
		return nil
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	renameFile = os.Rename
	defer func() { renameFile = renameNoReplace }()
	if err := check(); err == nil {
		t.Fatal("positive control: a replacing rename passed the check")
	}
}

// Crash safety on link-then-unlink systems: the same file under both names is finished, not
// reported as a conflict, and nothing is duplicated.
func TestFileDaysFinishesInterruptedMove(t *testing.T) {
	base, root, _ := build(t, spoolTree(3, time.Hour))
	l := moveLayout(root, ".mkv")
	plan := FileDays(l, MoveOptions{Platform: osx.Current(), Now: mt0, Margin: time.Minute, Sandbox: base, DryRun: true})
	if len(plan.Moved) != 1 {
		t.Fatalf("dry run planned %+v", plan.Moved)
	}
	m := plan.Moved[0]
	if err := os.Mkdir(filepath.Dir(m.To), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(m.From, m.To); err != nil {
		t.Skip("no hard links here:", err)
	}
	rep := FileDays(l, moveOpts(base, mt0))
	if len(rep.Moved) != 1 || len(rep.Conflicts) != 0 {
		t.Fatalf("moved %+v conflicts %+v", rep.Moved, rep.Conflicts)
	}
	if _, err := os.Lstat(m.From); err == nil {
		t.Error("the spool name still exists")
	}
}

// C2: the marker and sandbox gates refuse a run; the recorder gate refuses UnfileDays. Positive
// control: the same tree with every gate open moves a file.
func TestMoverGates(t *testing.T) {
	base, root, _ := build(t, spoolTree(3, time.Hour))
	l := moveLayout(root, ".mkv")
	elsewhere := t.TempDir()
	if r := FileDays(l, MoveOptions{Platform: osx.Current(), Now: mt0, Sandbox: elsewhere}); r.Refused == "" || len(r.Moved) > 0 {
		t.Errorf("outside the sandbox: %+v", r)
	}
	if r := UnfileDays(l, MoveOptions{Platform: osx.Current(), Now: mt0, Sandbox: base, RecorderRunning: true}); r.Refused == "" {
		t.Errorf("recorder running: %+v", r)
	}
	_ = os.Remove(MarkerPath(root))
	if r := FileDays(l, moveOpts(base, mt0)); r.Refused == "" || len(r.Moved) > 0 {
		t.Errorf("no marker: %+v", r)
	}
	own(t, root)
	if r := FileDays(l, moveOpts(base, mt0)); r.Refused != "" || len(r.Moved) != 1 {
		t.Errorf("positive control: %+v", r)
	}
}

// UnfileDays leaves misfiled and foreign files and their day folder, and removes a day folder only
// when it emptied it; Max bounds a run.
func TestUnfileDaysLeavesStrays(t *testing.T) {
	d1 := mt0.Add(-48 * time.Hour)
	day1 := DayName(d1.In(time.FixedZone("", 2*3600)))
	day0 := DayName(mt0.Add(-24 * time.Hour).In(time.FixedZone("", 2*3600)))
	tr := spoolTree(0, 0,
		archivetest.Node{Path: "archive/Cam/" + day1 + "/" + segName("Cam", d1), Kind: "file", Bytes: 3, MTime: d1},
		archivetest.Node{Path: "archive/Cam/" + day1 + "/" + segName("Cam", d1.Add(time.Hour)), Kind: "file", Bytes: 3, MTime: d1},
		archivetest.Node{Path: "archive/Cam/" + day0 + "/" + segName("Cam", d1.Add(2*time.Hour)), Kind: "file", Bytes: 4, MTime: d1}, // misfiled
		archivetest.Node{Path: "archive/Cam/" + day0 + "/notes.txt", Kind: "file", Bytes: 4, MTime: d1},
	)
	base, root, _ := build(t, tr)
	l := moveLayout(root, ".mkv")
	o := moveOpts(base, mt0)
	o.Max = 1
	if r := UnfileDays(l, o); len(r.Moved) != 1 || r.Deferred != 1 || len(r.DaysRemoved) != 0 {
		t.Fatalf("max 1: %+v", r)
	}
	o.Max = 0
	r := UnfileDays(l, o)
	if len(r.Moved) != 1 || len(r.Left) != 2 || len(r.DaysRemoved) != 1 || filepath.Base(r.DaysRemoved[0]) != day1 {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Lstat(filepath.Join(root, "Cam", day0, "notes.txt")); err != nil {
		t.Error("a foreign file was moved:", err)
	}
}

// fakeRecorder writes segments into the spool like ffmpeg: it appends to the open segment in
// chunks (each chunk reopens the file by name WITHOUT creating it, so a segment moved while it is
// written makes the next chunk fail) and rolls to a new segment every few chunks.
type fakeRecorder struct {
	dir, cam string
	written  map[string][]byte
	err      error
}

func (f *fakeRecorder) run(stop <-chan struct{}, flush time.Duration) {
	st := mt0
	for k := 0; ; k++ {
		name := segName(f.cam, st)
		p := filepath.Join(f.dir, name)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			f.err = err
			return
		}
		f.written[name] = nil
		for c := 0; c < 4; c++ {
			select {
			case <-stop:
				return
			case <-time.After(flush):
			}
			chunk := []byte(fmt.Sprintf("%s chunk %d;", name, c))
			fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err == nil {
				_, err = fh.Write(chunk)
				if cerr := fh.Close(); err == nil {
					err = cerr
				}
			}
			if err != nil {
				f.err = fmt.Errorf("segment %d chunk %d: %w (moved while written?)", k, c, err)
				return
			}
			f.written[name] = append(f.written[name], chunk...)
		}
		st = st.Add(15 * time.Minute)
	}
}

// C3: with a recorder appending to the newest segment and rolling files, filing over and over
// never moves a file still being written, and every byte ends up in exactly one file.
func TestFileDaysUnderAWritingRecorder(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "archive")
	if err := os.MkdirAll(filepath.Join(root, "Cam"), 0o755); err != nil {
		t.Fatal(err)
	}
	own(t, root)
	l := moveLayout(root, ".mkv")
	const flush = 5 * time.Millisecond
	rec := &fakeRecorder{dir: l.CameraDir("Cam"), cam: "Cam", written: map[string][]byte{}}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { defer close(done); rec.run(stop, flush) }()
	moved := 0
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		r := FileDays(l, MoveOptions{Platform: osx.Current(), Now: time.Now(), Margin: 4 * flush, Sandbox: base})
		if len(r.Errors) > 0 || len(r.Conflicts) > 0 {
			t.Fatalf("errors %+v conflicts %+v", r.Errors, r.Conflicts)
		}
		moved += len(r.Moved)
	}
	close(stop)
	<-done
	if rec.err != nil {
		t.Fatal(rec.err)
	}
	if moved == 0 {
		t.Fatal("positive control: nothing was filed")
	}
	segs, err := l.Segments("Cam")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != len(rec.written) {
		t.Fatalf("%d segments on disk, %d written", len(segs), len(rec.written))
	}
	for _, s := range segs {
		b, err := os.ReadFile(s.Path)
		if err != nil || string(b) != string(rec.written[s.Name()]) {
			t.Errorf("%s: content differs from what was written (%v)", s.Path, err)
		}
	}
	t.Logf("%d segments written, %d filed", len(rec.written), moved)
}

// C3 (Windows): a closed segment held open by another process without FILE_SHARE_DELETE is not
// moved and is no error (Busy); once released it moves on the next run.
func TestFileDaysBusyFileWaits(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows refuses to rename an open file")
	}
	base, root, _ := build(t, spoolTree(3, time.Hour))
	l := moveLayout(root, ".mkv")
	plan := FileDays(l, MoveOptions{Platform: osx.Current(), Now: mt0, Margin: time.Minute, Sandbox: base, DryRun: true})
	h, err := os.Open(plan.Moved[0].From) // Go opens with FILE_SHARE_READ|FILE_SHARE_WRITE only
	if err != nil {
		t.Fatal(err)
	}
	r := FileDays(l, moveOpts(base, mt0))
	if len(r.Moved) != 0 || len(r.Busy) != 1 || len(r.Errors) != 0 {
		t.Fatalf("held: %+v", r)
	}
	t.Logf("busy: %s", r.Busy[0].Why)
	_ = h.Close()
	if r := FileDays(l, moveOpts(base, mt0)); len(r.Moved) != 1 || len(r.Busy) != 0 {
		t.Fatalf("released: %+v", r)
	}
}
