package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/osx"
	"holocron/internal/prune"
)

// The flat corpus of the layout command tests: what the archive looked like before D20, with the
// awkward cases. Six cameras, five days (22 to 26 Oct 2026) of segments every three hours, plus the
// repeated hour of the 25 Oct fall-back (03:00 +0300 and 03:00 +0200), a zero-byte segment per
// camera, foreign files (a text file, another camera's segment) in camera folders, and a day folder
// made by hand holding a misfiled segment and a foreign file. Write times are the segment ends
// shifted so that the newest segment ended an hour ago (the commands use the real clock).

const corpusCams = 6

type corpusFile struct {
	rel   string // path under the root, "/" separated
	bytes int64
	start time.Time
}

// corpusStarts are the segment starts of one camera, oldest first.
func corpusStarts() []time.Time {
	var out []time.Time
	for d := 22; d <= 26; d++ {
		for k := 0; k < 8; k++ {
			// Europe/Athens: +0300 until 04:00 on 25 Oct (01:00 UTC), +0200 after
			wall := time.Date(2026, 10, d, 3*k, 0, 0, 0, time.UTC) // the wall clock, read as UTC
			off := 3 * time.Hour
			if !wall.Add(-off).Before(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) {
				off = 2 * time.Hour
			}
			out = append(out, wall.Add(-off).In(time.FixedZone("", int(off/time.Second))))
		}
		if d == 25 { // the repeated hour: 03:00 +0300 is in the loop, 03:00 +0200 an hour later
			out = append(out, time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC).In(time.FixedZone("", 2*3600)))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

func corpusFiles() []corpusFile {
	var out []corpusFile
	starts := corpusStarts()
	for c := 0; c < corpusCams; c++ {
		cam := fmt.Sprintf("Cam_%c", 'A'+c)
		for i, st := range starts {
			n := int64(1000 + 37*i + c)
			if i == 5 {
				n = 0 // a zero-byte segment
			}
			out = append(out, corpusFile{rel: cam + "/" + archive.FormatSegmentName(cam, st, ".mkv"), bytes: n, start: st})
		}
	}
	old := starts[0].Add(-72 * time.Hour)
	out = append(out,
		corpusFile{rel: "Cam_A/notes.txt", bytes: 12, start: starts[3]},
		corpusFile{rel: "Cam_B/" + archive.FormatSegmentName("Cam_A", starts[4], ".mkv"), bytes: 21, start: starts[4]}, // another camera's segment
		corpusFile{rel: "Cam_C/2026-10-22/" + archive.FormatSegmentName("Cam_C", old, ".mkv"), bytes: 33, start: old},  // misfiled
		corpusFile{rel: "Cam_C/2026-10-22/readme.txt", bytes: 5, start: old},
	)
	return out
}

// buildCorpus writes the corpus under root, every write time shifted by shift.
func buildCorpus(t *testing.T, root string, shift time.Duration) {
	t.Helper()
	for _, f := range corpusFiles() {
		p := filepath.Join(root, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		var body []byte
		if f.bytes > 0 {
			body = make([]byte, f.bytes)
			copy(body, "900 "+f.rel+"\x00") // textProber's duration, and a content of its own
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := setCreated(p, f.start); err != nil {
			t.Fatal(err)
		}
		end := f.start.Add(15 * time.Minute).Add(shift)
		if err := os.Chtimes(p, end, end); err != nil {
			t.Fatal(err)
		}
	}
}

// corpusShift puts the newest segment's end an hour before now; one value per test binary, so
// every corpus of a test is the same tree.
var corpusShift = sync.OnceValue(func() time.Duration {
	s := corpusStarts()
	return time.Now().Add(-time.Hour).Sub(s[len(s)-1].Add(15 * time.Minute)).Round(time.Second)
})

// manifest is every entry under root but the recorder's own "_" folders, never following a link:
// path -> "dir", or size, sha256 and write time.
func manifest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(rel, "_") {
			return fs.SkipDir
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			out[filepath.ToSlash(rel)] = "dir"
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = fmt.Sprintf("%d %x %d", fi.Size(), sha256.Sum256(b), fi.ModTime().UnixNano())
		default:
			out[filepath.ToSlash(rel)] = "other " + fi.Mode().String()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func manifestDiff(a, b map[string]string) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			out = append(out, "gone: "+k)
		} else if w != v {
			out = append(out, "changed: "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, "new: "+k)
		}
	}
	sort.Strings(out)
	return out
}

// contentsOf is the multiset of (name, size, hash, time) of the regular files of a manifest.
func contentsOf(m map[string]string) []string {
	var out []string
	for p, v := range m {
		if v != "dir" {
			out = append(out, filepath.Base(p)+" "+v)
		}
	}
	sort.Strings(out)
	return out
}

// corpusRig is a rig whose root holds the corpus, with no supervisor answering. The root is claimed
// first, as the live one was: the claim refuses a root that already holds strays.
func corpusRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.noCtl = true
	r.claim()
	buildCorpus(t, r.root, corpusShift())
	return r
}

// setKey writes a key into the rig's configuration file (MoveMaxPerCycle is not editable by config set).
func (r *rig) setKey(key, value string) {
	r.t.Helper()
	b, err := os.ReadFile(r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	s := strings.Replace(string(b), "{\n", "{\n    \""+key+"\":  "+value+",\n", 1)
	if err := os.WriteFile(r.cfg, []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// lineStarting is the first line of out that starts with prefix.
func lineStarting(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func decodeLayout(t *testing.T, out string) layoutReport {
	t.Helper()
	var rep layoutReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return rep
}

// corpus counts: per camera, segments and the ones that stay (the two newest).
var corpusPerCam = len(corpusStarts())

// D3: a dry run is the default and changes nothing; it shows the plan per camera (counts, bytes,
// days, new days, what stays, other files); --json carries the schema and the same counts.
func TestArchiveMigrateDryRunIsTheDefault(t *testing.T) {
	r := corpusRig(t)
	before := manifest(t, r.root)
	out, errw, code := r.run("archive", "migrate")
	if code != 0 || !strings.Contains(out, "DRY RUN, nothing is moved") || !strings.Contains(out, "next: holocron archive migrate --now") {
		t.Fatalf("%d %q %q", code, out, errw)
	}
	for cam, want := range map[string]string{
		"Cam_A": fmt.Sprintf("would file %d segment(s), ", corpusPerCam-2),
		"Cam_C": "into 5 day folder(s) (4 new); 2 segment(s) stay in the camera folder", // it already has 2026-10-22
		"Cam_B": "into 5 day folder(s) (5 new); 2 segment(s) stay in the camera folder; 1 other file(s) there are not segments of this camera and are left alone",
	} {
		if line := lineStarting(out, "  "+cam+" "); !strings.Contains(line, want) {
			t.Errorf("%s: %q lacks %q", cam, line, want)
		}
	}
	out, _, code = r.run("--json", "archive", "migrate")
	rep := decodeLayout(t, out)
	if code != 0 || rep.Schema != 1 || rep.Command != "archive migrate" || !rep.DryRun || rep.Moved != corpusCams*(corpusPerCam-2) || len(rep.Moves) != rep.Moved ||
		len(rep.Cameras) != corpusCams || rep.Cycles != 0 || !rep.Complete || rep.MarginSeconds != 120 {
		t.Fatalf("--json dry run: %d %+v", code, rep)
	}
	for _, c := range rep.Cameras {
		if c.Moves != corpusPerCam-2 || c.InSpool != 2 || len(c.Days) != 5 {
			t.Errorf("%s: %+v", c.Camera, c)
		}
	}
	if d := manifestDiff(before, manifest(t, r.root)); len(d) != 0 {
		t.Fatalf("a dry run changed the tree: %v", d)
	}
}

// D3: --now applies, with --json the counts of the applied run; a second run is a no-op.
func TestArchiveMigrateNowAppliesAndIsIdempotent(t *testing.T) {
	r := corpusRig(t)
	r.logSpec = "info"
	before := manifest(t, r.root)
	out, errw, code := r.run("--json", "archive", "migrate", "--now")
	rep := decodeLayout(t, out)
	if code != 0 || rep.DryRun || rep.Moved != corpusCams*(corpusPerCam-2) || !rep.Complete || rep.Cycles < 1 || len(rep.Errors)+len(rep.Conflicts) != 0 {
		t.Fatalf("now: %d %q %+v", code, errw, rep)
	}
	after := manifest(t, r.root)
	if strings.Join(contentsOf(before), "\n") != strings.Join(contentsOf(after), "\n") {
		t.Fatal("the files differ after the migration")
	}
	l := archive.Layout{Root: r.root, Matcher: archive.MatcherFor(osx.Current()), Container: ".mkv"}
	for _, m := range rep.Moves {
		if c := l.Classify(m.To); c.Kind != archive.Filed {
			t.Errorf("%s was filed as %v", m.To, c.Kind)
		}
	}
	if !strings.Contains(r.log(), "action=archive.file") || !strings.Contains(r.log(), "archive migrate cycle 1") {
		t.Errorf("no info progress records:\n%s", r.log())
	}
	out, _, code = r.run("archive", "migrate", "--now")
	if code != 0 || !strings.Contains(out, "total: filed 0 segment(s)") || !strings.Contains(out, "next: holocron archive stats") {
		t.Errorf("second run: %d %s", code, out)
	}
	if d := manifestDiff(after, manifest(t, r.root)); len(d) != 0 {
		t.Errorf("a second run changed the tree: %v", d)
	}
	out, _, _ = r.run("archive", "migrate")
	if !strings.Contains(out, "nothing to file") {
		t.Errorf("a dry run after the migration: %s", out)
	}
}

// D5: the sandbox and marker gates refuse as prune's do, moving nothing; positive control: the
// same tree claimed and inside the sandbox migrates.
func TestArchiveMigrateGates(t *testing.T) {
	r := newRig(t)
	r.noCtl = true
	buildCorpus(t, r.root, corpusShift())
	before := manifest(t, r.root)
	out, _, code := r.run("archive", "migrate", "--now")
	if code != 1 || !strings.Contains(out, "REFUSED: the root is not owned by this recorder") || !strings.Contains(out, "fix: holocron up claims the root") {
		t.Errorf("unclaimed: %d %q", code, out)
	}
	if d := manifestDiff(before, manifest(t, r.root)); len(d) != 0 {
		t.Fatalf("a refused run changed the tree: %v", d)
	}

	r = corpusRig(t)
	before = manifest(t, r.root)
	t.Setenv("SANDBOX_ONLY", filepath.Join(r.base(), "elsewhere"))
	out, _, code = r.run("--json", "archive", "migrate", "--now")
	if rep := decodeLayout(t, out); code != 1 || !strings.Contains(rep.Refused, "outside the sandbox") || rep.Fix == "" || rep.Moved != 0 {
		t.Errorf("outside the sandbox: %d %+v", code, rep)
	}
	t.Setenv("SANDBOX_ONLY", "1")
	if d := manifestDiff(before, manifest(t, r.root)); len(d) != 0 {
		t.Fatalf("a refused run changed the tree: %v", d)
	}
	if _, _, code := r.run("archive", "migrate", "--now"); code != 0 {
		t.Errorf("positive control: %d", code)
	}
}

// D5: --camera restricts the run to one camera (by cameras.json name or folder) and rejects paths
// and unknown names before anything moves; a bad --margin is a usage error.
func TestArchiveMigrateCameraFilter(t *testing.T) {
	r := corpusRig(t)
	before := manifest(t, r.root)
	for _, bad := range []string{"../Cam_A", "Cam_A/2026-10-22", `..`, "_state", filepath.Join(r.root, "Cam_A"), "Nope"} {
		if _, errw, code := r.run("archive", "migrate", "--now", "--camera", bad); code != 2 || !strings.Contains(errw, "--camera") {
			t.Errorf("--camera %q: %d %q", bad, code, errw)
		}
	}
	if _, errw, code := r.run("archive", "migrate", "--margin", "10"); code != 2 || !strings.Contains(errw, "at least 30") {
		t.Errorf("--margin 10: %d %q", code, errw)
	}
	if d := manifestDiff(before, manifest(t, r.root)); len(d) != 0 {
		t.Fatalf("a rejected run changed the tree: %v", d)
	}
	out, _, code := r.run("--json", "archive", "migrate", "--now", "--camera", "Cam_B")
	rep := decodeLayout(t, out)
	if code != 0 || len(rep.Cameras) != 1 || rep.Cameras[0].Camera != "Cam_B" || rep.Moved != corpusPerCam-2 {
		t.Fatalf("--camera Cam_B: %d %+v", code, rep)
	}
	for _, m := range rep.Moves {
		if m.Camera != "Cam_B" {
			t.Errorf("moved outside Cam_B: %+v", m)
		}
	}
	// a camera of cameras.json is found by its name; it has no folder here
	if _, errw, code := r.run("archive", "migrate", "--camera", "Front"); code != 2 || !strings.Contains(errw, "no camera folder") {
		t.Errorf("--camera Front: %d %q", code, errw)
	}
}

// D5: a destination that exists is a conflict, shown by the dry run and by --now, exit 1; neither
// file is replaced; the other files are filed (positive control).
func TestArchiveMigrateConflict(t *testing.T) {
	r := corpusRig(t)
	starts := corpusStarts()
	name := archive.FormatSegmentName("Cam_D", starts[1], ".mkv")
	taken := filepath.Join(r.root, "Cam_D", archive.DayName(starts[1]), name)
	os.MkdirAll(filepath.Dir(taken), 0o755)
	os.WriteFile(taken, []byte("someone else's file"), 0o644)
	before := manifest(t, r.root)
	out, _, code := r.run("archive", "migrate")
	if code != 1 || !strings.Contains(out, "1 file(s) CONFLICT, both files stay as they are") || !strings.Contains(out, "--now leaves both files as they are") {
		t.Errorf("dry run: %d %s", code, out)
	}
	out, _, code = r.run("--json", "archive", "migrate", "--now")
	rep := decodeLayout(t, out)
	if code != 1 || len(rep.Conflicts) != 1 || rep.Complete || rep.Moved != corpusCams*(corpusPerCam-2)-1 || !strings.Contains(rep.Next, "resolve") {
		t.Fatalf("now: %d %+v", code, rep)
	}
	after := manifest(t, r.root)
	rel := filepath.ToSlash(strings.TrimPrefix(taken, r.root+string(filepath.Separator)))
	if before[rel] != after[rel] || before["Cam_D/"+name] != after["Cam_D/"+name] {
		t.Error("a conflicting file was touched")
	}
}

// D6: a migration stopped midway (the state after a kill is some renames done: each is atomic and
// there is no journal; here several moves per camera and one day folder made but not yet used), run
// again, ends exactly as an uninterrupted one; cycles of MoveMaxPerCycle report progress.
func TestArchiveMigrateResumes(t *testing.T) {
	whole := corpusRig(t)
	if _, _, code := whole.run("archive", "migrate", "--now"); code != 0 {
		t.Fatal(code)
	}
	want := manifest(t, whole.root)

	r := corpusRig(t)
	r.setKey("MoveMaxPerCycle", "7")
	l := archive.Layout{Root: r.root, Matcher: archive.MatcherFor(osx.Current()), Container: ".mkv"}
	cut := archive.FileDays(l, archive.MoveOptions{Platform: osx.Current(), Now: time.Now(), Margin: 2 * time.Minute, Max: 5})
	if len(cut.Moved) != 5*corpusCams || cut.Refused != "" {
		t.Fatalf("the interrupted run: %+v", cut)
	}
	os.Mkdir(filepath.Join(r.root, "Cam_E", "2026-10-26"), 0o755)
	out, errw, code := r.run("archive", "migrate", "--now")
	if code != 0 || !strings.Contains(out, "cycle 2: filed") || !strings.Contains(out, "more eligible") || !strings.Contains(out, "done, nothing more is eligible") {
		t.Fatalf("resume: %d %q\n%s", code, errw, out)
	}
	got := manifest(t, r.root)
	// the write times of folders are not in the manifest; the config file sits outside the root
	if d := manifestDiff(want, got); len(d) != 0 {
		t.Errorf("the resumed migration differs from an uninterrupted one: %v", d)
	}
}

// D4: `archive migrate --now` beside a second mover (the supervisor's: FileDays with its cap) and a
// pruner pass that deletes nothing but removes empty day folders, all on the same root: nothing is
// lost or duplicated, no error or conflict is reported, and the final tree equals a single-actor
// migration.
func TestArchiveMigrateBesideTheSupervisorsMover(t *testing.T) {
	whole := corpusRig(t)
	if _, _, code := whole.run("archive", "migrate", "--now"); code != 0 {
		t.Fatal(code)
	}
	want := manifest(t, whole.root)

	r := corpusRig(t)
	r.setKey("MoveMaxPerCycle", "3")
	before := manifest(t, r.root)
	l := archive.Layout{Root: r.root, Matcher: archive.MatcherFor(osx.Current()), Container: ".mkv"}
	cfg := config.Defaults(osx.Current())
	cfg.ReserveGB, cfg.PruneHysteresisGB, cfg.MaxRetentionDays = 0, 0, nil // the pruner deletes nothing; it removes empty past day folders
	rules := prune.RulesFrom(cfg)
	ex := &prune.Executor{FS: prune.OSFS{Platform: osx.Current()}}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var other struct {
		sync.Mutex
		moved, bad int
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rep := archive.FileDays(l, archive.MoveOptions{Platform: osx.Current(), Now: time.Now(), Margin: 2 * time.Minute, Max: 2})
			in, err := prune.Collect(r.root, osx.Current(), rules, time.Now(), false)
			bad := len(rep.Errors) + len(rep.Conflicts)
			if err != nil {
				bad++
				t.Logf("collect: %v", err)
			} else if res, err := ex.Run(in, &prune.State{}); err != nil || res.Failed > 0 || len(res.Deletions) > 0 {
				bad++
				t.Logf("prune: %v failed %d deletions %d %v", err, res.Failed, len(res.Deletions), res.Decision.Messages)
			}
			for _, x := range append(rep.Errors, rep.Conflicts...) {
				t.Logf("mover: %+v", x)
			}
			other.Lock()
			other.moved += len(rep.Moved)
			other.bad += bad
			other.Unlock()
		}
	}()
	out, errw, code := r.run("--json", "archive", "migrate", "--now")
	close(stop)
	wg.Wait()
	rep := decodeLayout(t, out)
	if code != 0 || len(rep.Errors)+len(rep.Conflicts) != 0 || other.bad != 0 {
		t.Fatalf("two movers: exit %d %q, errors %v conflicts %v, the other mover's errors %d", code, errw, rep.Errors, rep.Conflicts, other.bad)
	}
	if rep.Moved+other.moved != corpusCams*(corpusPerCam-2) {
		t.Errorf("moves: migrate %d + the other mover %d, want %d in all", rep.Moved, other.moved, corpusCams*(corpusPerCam-2))
	}
	// one more pass finishes whatever the race left to the next run (skipped files)
	if !rep.Complete {
		r.run("archive", "migrate", "--now")
	}
	got := manifest(t, r.root)
	if strings.Join(contentsOf(before), "\n") != strings.Join(contentsOf(got), "\n") {
		t.Fatal("files lost, changed or duplicated")
	}
	if d := manifestDiff(want, got); len(d) != 0 {
		t.Errorf("the final tree differs from a single-actor migration: %v", d)
	}
	t.Logf("migrate moved %d, the other mover %d, skipped %d", rep.Moved, other.moved, len(rep.Skipped))
}

// D2: flatten refuses, even as a dry run, while the recorder is reported running (by its status
// file, or by a supervisor answering on the control endpoint), changes nothing, exits 1 and shows
// the fix; stopped, it proceeds (positive control). D3: its dry run is the default, --json carries
// the schema and the counts of the plan and of the applied run.
func TestArchiveFlattenRefusesWhileRecording(t *testing.T) {
	r := corpusRig(t)
	if _, _, code := r.run("archive", "migrate", "--now"); code != 0 {
		t.Fatal(code)
	}
	filed := manifest(t, r.root)
	r.running()
	for _, args := range [][]string{{"archive", "flatten", "--now"}, {"archive", "flatten"}} {
		out, _, code := r.run(args...)
		if code != 1 || !strings.Contains(out, "REFUSED: the recorder is running") || !strings.Contains(out, "reports running") || !strings.Contains(out, "fix: holocron down") {
			t.Errorf("%v with a running status: %d %s", args, code, out)
		}
	}
	r.stopped()
	r.noCtl = false // the fake control endpoint answers
	out, _, code := r.run("--json", "archive", "flatten", "--now")
	if rep := decodeLayout(t, out); code != 1 || !strings.Contains(rep.Refused, "control endpoint") || rep.Fix != "holocron down" || rep.Moved != 0 {
		t.Errorf("with a supervisor answering: %d %+v", code, rep)
	}
	if d := manifestDiff(filed, manifest(t, r.root)); len(d) != 0 {
		t.Fatalf("a refused flatten changed the tree: %v", d)
	}
	r.noCtl = true
	out, _, code = r.run("--json", "archive", "flatten")
	plan := decodeLayout(t, out)
	if code != 0 || plan.Schema != 1 || plan.Command != "archive flatten" || !plan.DryRun || plan.Moved != corpusCams*(corpusPerCam-2) || len(plan.Left) != 2 || plan.Refused != "" {
		t.Fatalf("dry run: %d %+v", code, plan)
	}
	for _, c := range plan.Cameras {
		want := 5
		if c.Camera == "Cam_C" {
			want = 4 // 2026-10-22 keeps the misfiled segment and the foreign file
		}
		if len(c.RemovedDays) != want || c.InSpool != corpusPerCam {
			t.Errorf("dry run %s: %+v", c.Camera, c)
		}
	}
	if d := manifestDiff(filed, manifest(t, r.root)); len(d) != 0 {
		t.Fatalf("a dry run changed the tree: %v", d)
	}
	out, _, code = r.run("--json", "archive", "flatten", "--now")
	rep := decodeLayout(t, out)
	if code != 0 || rep.DryRun || rep.Moved != plan.Moved || !rep.Complete || len(rep.Left) != 2 || !strings.Contains(rep.Next, "archive stats") {
		t.Fatalf("stopped: %d %+v", code, rep)
	}
	for i, c := range rep.Cameras {
		if strings.Join(c.RemovedDays, ",") != strings.Join(plan.Cameras[i].RemovedDays, ",") {
			t.Errorf("%s: removed %v, the plan said %v", c.Camera, c.RemovedDays, plan.Cameras[i].RemovedDays)
		}
	}
	out, _, _ = r.run("archive", "flatten")
	if !strings.Contains(out, "the archive is flat already") {
		t.Errorf("a dry run after flatten: %s", out)
	}
}
