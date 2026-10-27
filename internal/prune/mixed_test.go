package prune

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
	"holocron/internal/osx"
)

// ---- same as flat (task L3, decision D20) ----

// fileScenario is s with a seeded subset of its closed segments filed into the day folder of the
// date in their name (archive/<camera>/<YYYY-MM-DD>/<name>): only strict segments of a camera
// folder directly in the root, never one that is, at any point of the scenario, among the two
// newest of its folder by write time, by the start in its name or by creation time (so the spool
// keeps the open segment and the newest closed one, the L4 mover's contract), never one a step
// removes. moved maps each filed path to its flat path, both relative with "/".
func fileScenario(s gScenario, ext string) (gScenario, map[string]string) {
	h := fnv.New64a()
	h.Write([]byte(s.Name))
	rng := rand.New(rand.NewPCG(h.Sum64(), 20261027))
	type seen struct {
		mt, ct, start time.Time
	}
	parse := func(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }
	strict := func(e gEntry) (string, time.Time, bool) {
		parts := strings.Split(e.Path, "/")
		if e.Kind != "file" || len(parts) != 3 || parts[0] != "archive" || strings.HasPrefix(parts[1], "_") ||
			!(archive.Matcher{}).IsSegmentOf(parts[2], parts[1], ext) {
			return "", time.Time{}, false
		}
		_, start, ok := archive.ParseSegment(parts[2])
		return parts[1], start, ok
	}
	keep := map[string]bool{}
	present := map[string]map[string]seen{} // camera -> path -> times
	settle := func() {
		for _, files := range present {
			for _, by := range []func(seen) time.Time{func(x seen) time.Time { return x.mt }, func(x seen) time.Time { return x.start }, func(x seen) time.Time { return x.ct }} {
				var ps []string
				for p := range files {
					ps = append(ps, p)
				}
				sort.Slice(ps, func(i, j int) bool {
					a, b := by(files[ps[i]]), by(files[ps[j]])
					if !a.Equal(b) {
						return a.After(b)
					}
					return ps[i] > ps[j]
				})
				for i := 0; i < len(ps) && i < 2; i++ {
					keep[ps[i]] = true
				}
			}
		}
	}
	addAll := func(es []gEntry) {
		for _, e := range es {
			cam, start, ok := strict(e)
			if !ok {
				continue
			}
			if present[cam] == nil {
				present[cam] = map[string]seen{}
			}
			present[cam][e.Path] = seen{parse(e.MTime), parse(e.CTime), start}
		}
		settle()
	}
	var early, late []gEntry
	for _, e := range s.Entries {
		if e.Late {
			late = append(late, e)
		} else {
			early = append(early, e)
		}
	}
	addAll(early)
	addAll(late)
	for _, st := range s.Steps {
		switch st.Op {
		case "add":
			addAll(st.Entries)
		case "remove":
			for _, p := range st.Paths {
				keep[p] = true
			}
		}
	}
	moved := map[string]string{}
	var cands []string
	for _, files := range present {
		for p := range files {
			if !keep[p] {
				cands = append(cands, p)
			}
		}
	}
	sort.Strings(cands)
	for _, p := range cands {
		if rng.IntN(3) == 0 {
			continue // most, not all: the rest stay in the spool
		}
		parts := strings.Split(p, "/")
		_, start, _ := archive.ParseSegment(parts[2])
		moved[parts[0]+"/"+parts[1]+"/"+archive.DayName(start)+"/"+parts[2]] = p
	}
	flatToFiled := map[string]string{}
	for f, p := range moved {
		flatToFiled[p] = f
	}
	rewrite := func(es []gEntry) []gEntry {
		out := make([]gEntry, len(es))
		for i, e := range es {
			if f, ok := flatToFiled[e.Path]; ok {
				e.Path = f
			}
			out[i] = e
		}
		return out
	}
	m := s
	m.Name += "/filed"
	m.Entries = rewrite(s.Entries)
	m.Steps = make([]gStep, len(s.Steps))
	for i, st := range s.Steps {
		st.Entries = rewrite(st.Entries)
		m.Steps[i] = st
	}
	return m, moved
}

// replayed is the outcome of every step of one replay, paths normalised: the sandbox is "<S>"
// and each filed path is its flat path, so a mixed replay and its flat original compare equal.
func replayed(t *testing.T, s gScenario, fold bool, moved map[string]string) (string, *replay) {
	r := &replay{t: t, s: s, fold: fold}
	r.run()
	steps := make([]any, len(r.results))
	for i, g := range r.results {
		res := g.res
		for _, d := range res.DaysRemoved {
			if !archive.IsDayName(filepath.Base(d)) || exists(d) {
				t.Errorf("%s: removed %s, which is not a day folder or is still there", s.Name, d)
			}
		}
		res.DaysRemoved = nil // the day folders a run removed: only the filed archive has them
		removed := make([]string, len(g.removed))
		for j, p := range g.removed {
			if f, ok := moved[p]; ok {
				p = f
			}
			removed[j] = p
		}
		sort.Strings(removed)
		steps[i] = struct {
			Op      string
			Created bool
			State   State
			Res     Result
			Removed []string
			Logged  map[string]int
		}{g.op, g.created, g.state, res, removed, g.logged}
	}
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	esc := func(p string) string { q, _ := json.Marshal(p); return string(q[1 : len(q)-1]) }
	out := string(b)
	var filed []string
	for f := range moved {
		filed = append(filed, f)
	}
	sort.Strings(filed)
	for _, f := range filed {
		out = strings.ReplaceAll(out, esc(r.abs(f)), esc(r.abs(moved[f])))
	}
	return strings.ReplaceAll(out, esc(r.sandbox), "<S>"), r
}

// syntheticScenarios are seeded archives built for the day folders: 15-minute segments over up to
// five days in Athens time, through the fall-back of 25 Oct 2026 (+0300 and +0200 names in one day
// folder) and every local midnight, with retention cutoffs that fall inside a day folder, run by a
// sequence of prune steps (dry and real age rule, watermark, a held breaker, a later clock).
func syntheticScenarios(n int) []gScenario {
	rng := rand.New(rand.NewPCG(20261025, 3))
	athens := func(t time.Time) time.Time {
		if t.Before(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) {
			return t.In(time.FixedZone("", 3*3600))
		}
		return t.In(time.FixedZone("", 2*3600))
	}
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	var out []gScenario
	for i := 0; i < n; i++ {
		now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.IntN(4*96)) * 15 * time.Minute).Add(7 * time.Minute)
		s := gScenario{Name: fmt.Sprintf("synthetic-%03d", i), Now: stamp(now), Container: "mkv", SegmentSeconds: 900, StallFactor: 2.5, Marker: "claim"}
		open := now.Truncate(15 * time.Minute)
		span := 96 + rng.IntN(4*96)
		for c := 0; c < 1+rng.IntN(3); c++ {
			cam := []string{"Front_Door", "Back Yard", "Gate"}[c]
			for k := span; k >= 0; k-- {
				if k > 0 && rng.IntN(25) == 0 {
					continue // a gap
				}
				st := open.Add(-time.Duration(k) * 15 * time.Minute)
				mt := st.Add(15 * time.Minute)
				if k == 0 {
					mt = now.Add(-5 * time.Second)
				}
				s.Entries = append(s.Entries, gEntry{Path: "archive/" + cam + "/" + cam + "_" + athens(st).Format("20060102_150405-0700") + ".mkv",
					Kind: "file", Bytes: int64(65536 * (1 + rng.IntN(5000))), MTime: stamp(mt), CTime: stamp(st)})
			}
		}
		if rng.IntN(2) == 0 {
			s.Entries = append(s.Entries, gEntry{Path: "archive/Front_Door/notes.txt", Kind: "file", Bytes: 65536, MTime: stamp(now.Add(-time.Hour)), CTime: stamp(now.Add(-time.Hour))})
		}
		ret := 0.5 + float64(rng.IntN(16))/4 // 0.5 to 4.25 days: the cutoff is in a day folder
		s.Steps = []gStep{
			{Op: "prune", Dry: true, Free: 100, Config: gConfig{ReserveGB: 20, PruneHysteresisGB: 2, MaxAgePruneFraction: 0.9, MaxRetentionDays: &ret}},
			{Op: "prune", Free: 100, Config: gConfig{ReserveGB: 20, PruneHysteresisGB: 2, MaxAgePruneFraction: 0.9, MaxRetentionDays: &ret}},
			{Op: "prune", Free: float64(rng.IntN(25)), Config: gConfig{ReserveGB: 20, PruneHysteresisGB: float64(rng.IntN(30)), MaxAgePruneFraction: 0.5}},
			{Op: "prune", Free: 100, Config: gConfig{ReserveGB: 20, MaxAgePruneFraction: 0.01, MaxRetentionDays: ptr(0.1)}},
			{Op: "prune", Free: 1, Fixed: true, Config: gConfig{ReserveGB: 1000, MaxAgePruneFraction: 0.5}},
			{Op: "prune", Shift: 86400, Free: 1, Config: gConfig{ReserveGB: 20, MaxAgePruneFraction: 0.5}},
		}
		out = append(out, s)
	}
	return out
}

// TestFiledEqualsFlat is acceptance B1 of task L3: for every prune golden scenario (the 231 trees
// of testdata/golden/prune) and for generated archives across the fall-back day, midnight and the
// retention cutoff, the same files with most closed segments filed into their day folders give,
// step by step, the same deletions, kept and protected files, plan, decision, messages, alerts,
// breaker and clock outcomes, state and logs as the flat original. It replaces
// TestCollectStaysOnTheSpoolUntilL3 (task L2's pin of the pruner to the spool).
func TestFiledEqualsFlat(t *testing.T) {
	table, corpus := loadGoldens(t)
	all := append(append(table, corpus...), syntheticScenarios(60)...)
	compared, filed, deleted, filedDeleted, breaker, clock, blocked := 0, 0, 0, 0, 0, 0, 0
	for _, s := range all {
		ext := "." + s.Container
		m, moved := fileScenario(s, ext)
		for _, fold := range []bool{false, true} {
			flat, fr := replayed(t, s, fold, nil)
			if fr.noLinks && runtime.GOOS == "windows" {
				break
			}
			mixed, mr := replayed(t, m, fold, moved)
			if flat != mixed {
				t.Fatalf("%s (fold %v, %d filed): the filed archive decides differently\n flat  %s\n filed %s", s.Name, fold, len(moved), flat, mixed)
			}
			compared++
			if fold {
				continue
			}
			filed += len(moved)
			for _, g := range mr.results {
				for _, d := range g.res.Deletions {
					if g.res.DryRun {
						continue
					}
					deleted++
					if _, ok := moved[mr.rel(d.Path)]; ok {
						filedDeleted++
					}
				}
				breaker += btoi(g.res.BreakerHeld)
				blocked += btoi(g.res.Blocked)
				if g.res.Refusal != nil && g.res.Refusal.Gate == GateClock {
					clock++
				}
			}
		}
	}
	if filedDeleted == 0 || breaker == 0 || clock == 0 || blocked == 0 {
		t.Errorf("the comparison does not exercise filed deletions (%d), the breaker (%d), the clock gate (%d) and blocked runs (%d)", filedDeleted, breaker, clock, blocked)
	}
	t.Logf("%d scenarios (%d goldens), %d replays identical; %d segments filed, %d real deletions (%d of filed segments); breaker held %d, clock refused %d, blocked %d",
		len(all), len(table)+len(corpus), compared, filed, deleted, filedDeleted, breaker, clock, blocked)
}

// The comparison can fail: a planner that ignores filed segments (Collect's old spool view) gives
// a different outcome on a filed archive.
func TestFiledEqualsFlatCanFail(t *testing.T) {
	s := syntheticScenarios(1)[0]
	var segs []gEntry // without the foreign file, which the claim refuses
	for _, e := range s.Entries {
		if strings.HasSuffix(e.Path, ".mkv") {
			segs = append(segs, e)
		}
	}
	s.Entries = segs
	m, moved := fileScenario(s, ".mkv")
	flat, _ := replayed(t, s, false, nil)
	mixed, _ := replayed(t, m, false, moved)
	if flat != mixed {
		t.Fatal("precondition: equal")
	}
	// the same filed archive before any step, with Plan blind to the day folders
	m.Steps = nil
	r := &replay{t: t, s: m}
	r.run()
	in, err := Collect(r.root, osx.Current(), r.rules, r.now, true)
	if err != nil {
		t.Fatal(err)
	}
	all, spool := 0, 0
	for i := range in.Cameras {
		all += len(in.Cameras[i].Files)
		var keep []File
		for _, f := range in.Cameras[i].Files {
			if f.Day == "" {
				keep = append(keep, f)
			}
		}
		spool += len(keep)
		in.Cameras[i].Files = keep
	}
	in.FreeBytes, in.Rules.ReserveGB = 0, 1e6
	blind := Plan(in)
	in2, _ := Collect(r.root, osx.Current(), r.rules, r.now, true)
	in2.FreeBytes, in2.Rules.ReserveGB = 0, 1e6
	seeing := Plan(in2)
	if all == spool || blind.Candidates == seeing.Candidates {
		t.Fatalf("a planner blind to day folders is not caught: %d files, %d in the spool, candidates %d vs %d (%q %q)", all, spool, blind.Candidates, seeing.Candidates, in.RootProblem, in.MarkerProblem)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- safety on spool-and-day trees (B3) ----

// pruneHard is one real prune of root under the heaviest pressure (the watermark wants everything)
// with the age rule off.
func pruneHard(t *testing.T, root string, now time.Time, container string) (Result, *scaledFS) {
	t.Helper()
	fs := &scaledFS{OSFS: OSFS{Platform: osx.Current()}, Scale: 1}
	fs.Reset(0)
	r := Rules{Container: container, SegmentSeconds: 900, StallFactor: 2.5, ReserveGB: 1e6, MaxAgePruneFraction: 0.5}
	in, err := Collect(root, freePlatform{osx.Current(), fs}, r, now, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&Executor{FS: fs}).Run(in, &State{})
	if err != nil {
		t.Fatal(err)
	}
	return res, fs
}

func markRoot(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "_state"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{ "Owner": %q, "Schema": 1, "RootPath": %q }`, archive.MarkerOwner, root)
	if err := os.WriteFile(archive.MarkerPath(root), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMixedTreesDeleteOnlyFootage: on 120 seeded spool-and-day trees with every awkward case of
// archivetest.Mixed (misfiled and foreign files in day folders, another camera's segment, invalid
// day names, other folders, a folder in a day folder, links and junctions named like a day, a
// linked file in a day folder, the fall-back day), the heaviest pressure deletes only regular
// footage of each camera (spooled, or filed under its own date), never its newest segment, and
// leaves every other file as it was.
func TestMixedTreesDeleteOnlyFootage(t *testing.T) {
	deleted, filedDeleted, untouchable := 0, 0, 0
	for _, tr := range archivetest.Mixed(20261027, 120) {
		root, _ := tr.Build(t, t.TempDir())
		markRoot(t, root)
		fold := runtime.GOOS == "windows"
		before := archivetest.Walk(t, root, tr.Container, fold)
		res, fs := pruneHard(t, root, tr.Now, tr.Container)
		gone := map[string]bool{}
		for _, p := range fs.Removed {
			gone[p] = true
		}
		l := archive.Layout{Root: root, Matcher: archive.Matcher{FoldCase: fold}, Container: tr.Container}
		for _, c := range before.Cameras {
			footage := map[string]bool{}
			for _, s := range c.Footage {
				footage[s.Path] = true
			}
			newest := ""
			if len(c.Footage) > 0 {
				newest = c.Footage[0].Path // newest by write time: the open segment
			}
			for _, f := range c.AllFiles() {
				switch {
				case gone[f.Path] && !footage[f.Path]:
					t.Errorf("%s: deleted %s, which is not footage (%+v)", tr.Name, f.Path, l.Classify(f.Path))
				case gone[f.Path] && f.Path == newest:
					t.Errorf("%s: deleted the newest segment %s", tr.Name, f.Path)
				case gone[f.Path]:
					deleted++
					filedDeleted += btoi(f.Day != "")
				case !footage[f.Path]:
					untouchable++
				}
			}
		}
		for p := range gone {
			if c := l.Classify(p); !c.Footage() {
				t.Errorf("%s: removed %s outside the footage (%+v)", tr.Name, p, c)
			}
		}
		_ = res
		// every file outside the archive (link targets) is still there
		after := archivetest.Walk(t, root, tr.Container, fold)
		for _, c := range after.Cameras {
			for _, f := range c.AllFiles() {
				if gone[f.Path] {
					t.Errorf("%s: %s removed and still listed", tr.Name, f.Path)
				}
			}
		}
	}
	if filedDeleted == 0 || untouchable == 0 {
		t.Fatalf("not exercised: %d filed deletions, %d non-footage files", filedDeleted, untouchable)
	}
	t.Logf("%d deletions (%d filed), %d non-footage files left alone", deleted, filedDeleted, untouchable)
}

// TestFiledSafetyNegatives: each hazard beside a positive control, the same segment where it
// belongs, which the same run deletes (so a test that never deletes anything cannot pass).
func TestFiledSafetyNegatives(t *testing.T) {
	day := func(st time.Time) string { return st.In(testZone).Format("2006-01-02") }
	old := testNow.Add(-72 * time.Hour)
	for _, c := range []struct {
		name string
		bad  func(r *rig) string // the file that must survive
	}{
		{"misfiled (a day that is not its date)", func(r *rig) string {
			st := old.Add(time.Hour)
			return r.put(filepath.Join(r.root, "Cam", day(st.Add(24*time.Hour)), segName("Cam", st, ".mkv")), unit/64, st.Add(15*time.Minute))
		}},
		{"another camera's segment in a day folder", func(r *rig) string {
			st := old.Add(time.Hour)
			return r.put(filepath.Join(r.root, "Cam", day(st), segName("Other", st, ".mkv")), unit/64, st.Add(15*time.Minute))
		}},
		{"a foreign file in a day folder", func(r *rig) string {
			return r.put(filepath.Join(r.root, "Cam", day(old), "notes.mkv"), unit/64, old)
		}},
		{"nested deeper than a day folder", func(r *rig) string {
			st := old.Add(time.Hour)
			return r.put(filepath.Join(r.root, "Cam", day(st), "deeper", segName("Cam", st, ".mkv")), unit/64, st.Add(15*time.Minute))
		}},
		{"a folder that is not a day", func(r *rig) string {
			st := old.Add(time.Hour)
			return r.put(filepath.Join(r.root, "Cam", "old", segName("Cam", st, ".mkv")), unit/64, st.Add(15*time.Minute))
		}},
		{"an invalid date", func(r *rig) string {
			st := old.Add(time.Hour)
			return r.put(filepath.Join(r.root, "Cam", "2026-02-30", segName("Cam", st, ".mkv")), unit/64, st.Add(15*time.Minute))
		}},
		{"a camera folder nested in a day folder's name space", func(r *rig) string {
			st := old.Add(time.Hour)
			return r.put(filepath.Join(r.root, "Cam", day(st), day(st), segName("Cam", st, ".mkv")), unit/64, st.Add(15*time.Minute))
		}},
		{"a link named like a day, to segments outside", func(r *rig) string {
			st := old.Add(-48 * time.Hour)
			out := r.put(filepath.Join(r.dir, "outside", segName("Cam", st, ".mkv")), unit/64, st.Add(15*time.Minute))
			if err := os.Symlink(filepath.Dir(out), filepath.Join(r.root, "Cam", day(st))); err != nil {
				r.t.Skipf("no symbolic links: %v", err)
			}
			return out
		}},
		{"a linked file in a day folder", func(r *rig) string {
			st := old.Add(time.Hour)
			out := r.put(filepath.Join(r.dir, "outside", "clip.mkv"), unit/64, st.Add(15*time.Minute))
			os.MkdirAll(filepath.Join(r.root, "Cam", day(st)), 0o755)
			if err := os.Symlink(out, filepath.Join(r.root, "Cam", day(st), segName("Cam", st, ".mkv"))); err != nil {
				r.t.Skipf("no symbolic links: %v", err)
			}
			return out
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			control := r.put(filepath.Join(r.root, "Cam", day(old), segName("Cam", old, ".mkv")), unit/64, old.Add(15*time.Minute))
			for k := 0; k < 3; k++ { // the spool: two closed and the open segment
				st := testNow.Add(-time.Duration(45-15*k) * time.Minute)
				mt := st.Add(15 * time.Minute)
				if k == 2 {
					mt = testNow.Add(-5 * time.Second)
				}
				r.put(filepath.Join(r.root, "Cam", segName("Cam", st, ".mkv")), unit/64, mt)
			}
			bad := c.bad(r)
			r.claimAny()
			r.rules.ReserveGB = 1e6
			r.fs.Reset(0)
			res := r.run(false)
			if exists(control) {
				t.Fatalf("positive control: the filed segment %s was not deleted (%v)", control, res.Messages)
			}
			if !exists(bad) {
				t.Fatalf("deleted %s", bad)
			}
			for _, p := range r.fs.Removed {
				if c := (archive.Layout{Root: r.root, Matcher: archive.MatcherFor(osx.Current()), Container: ".mkv"}).Classify(p); !c.Footage() {
					t.Errorf("removed %s, %+v", p, c)
				}
			}
			if spool, _ := os.ReadDir(filepath.Join(r.root, "Cam")); len(spool) == 0 {
				t.Fatal("the spool is gone")
			}
		})
	}
}

// claimAny writes a valid marker whatever the tree holds (ClaimRoot refuses the hazards on purpose).
func (r *rig) claimAny() { markRoot(r.t, r.root) }

// TestFiledGatesStillRefuse: the marker and sandbox gates refuse a root of filed segments as they
// refuse a flat one; with the marker in place (the control) the same tree is pruned.
func TestFiledGatesStillRefuse(t *testing.T) {
	build := func(t *testing.T) (*rig, string) {
		r := newRig(t)
		old := testNow.Add(-72 * time.Hour)
		p := r.put(filepath.Join(r.root, "Cam", old.In(testZone).Format("2006-01-02"), segName("Cam", old, ".mkv")), unit/64, old.Add(15*time.Minute))
		r.put(filepath.Join(r.root, "Cam", segName("Cam", testNow.Add(-15*time.Minute), ".mkv")), unit/64, testNow.Add(-5*time.Second))
		r.rules.ReserveGB = 1e6
		r.fs.Reset(0)
		return r, p
	}
	t.Run("no marker", func(t *testing.T) {
		r, p := build(t)
		if res := r.run(false); res.Refusal == nil || res.Refusal.Gate != GateMarker || !exists(p) {
			t.Fatalf("%+v, file there %v", res.Refusal, exists(p))
		}
	})
	t.Run("outside the sandbox", func(t *testing.T) {
		r, p := build(t)
		r.claim()
		ex := r.exec(false)
		ex.Sandbox = filepath.Join(r.dir, "elsewhere")
		if _, err := ex.Run(r.inputs(false), &r.st); err == nil || !exists(p) {
			t.Fatalf("err %v, file there %v", err, exists(p))
		}
	})
	t.Run("control", func(t *testing.T) {
		r, p := build(t)
		r.claim()
		if r.run(false); exists(p) {
			t.Fatal("the filed segment was not deleted")
		}
	})
}

// TestFiledRevalidation: a day folder swapped for a link between the plan and the deletion is
// caught at execution time and nothing in it (or behind it) is deleted; without the swap (the
// control) the same file goes.
func TestFiledRevalidation(t *testing.T) {
	for _, swap := range []bool{false, true} {
		r := newRig(t)
		old := testNow.Add(-72 * time.Hour)
		dayDir := filepath.Join(r.root, "Cam", old.In(testZone).Format("2006-01-02"))
		p := r.put(filepath.Join(dayDir, segName("Cam", old, ".mkv")), unit/64, old.Add(15*time.Minute))
		r.put(filepath.Join(r.root, "Cam", segName("Cam", testNow.Add(-15*time.Minute), ".mkv")), unit/64, testNow.Add(-5*time.Second))
		r.claim()
		r.rules.ReserveGB = 1e6
		r.fs.Reset(0)
		in := r.inputs(false)
		moved := filepath.Join(r.dir, "outside")
		if swap {
			if err := os.Rename(dayDir, moved); err != nil {
				t.Fatal(err)
			}
			// a junction on Windows, a symbolic link elsewhere
			rel := func(p string) string { q, _ := filepath.Rel(r.dir, p); return filepath.ToSlash(q) }
			tr := archivetest.Tree{Nodes: []archivetest.Node{{Path: rel(dayDir), Kind: "junction", Target: rel(moved)}}}
			if _, links := tr.Build(t, r.dir); links != 1 {
				t.Skip("no directory link could be made")
			}
		}
		res, err := r.exec(false).Run(in, &r.st)
		if err != nil {
			t.Fatal(err)
		}
		if !swap {
			if exists(p) {
				t.Fatal("control: the filed segment was not deleted")
			}
			continue
		}
		target := filepath.Join(moved, filepath.Base(p))
		if !exists(target) || len(res.Deletions) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Why, "day folder") {
			t.Fatalf("swap not caught: target there %v, deletions %v, skipped %+v", exists(target), res.Deletions, res.Skipped)
		}
	}
}

// TestFiledNewestSurvives: under any pressure the open segment, and with an empty spool the
// newest filed segment, are never deleted; with the spool holding the open segment and the newest
// closed one (the mover's contract) the outcome is the flat archive's.
func TestFiledNewestSurvives(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 7, 0, 0, time.UTC)
	for _, spool := range []int{0, 1, 2, 3} {
		tr := archivetest.Filed(fmt.Sprintf("spool-%d", spool), 2, 4, 6, spool, now)
		for _, reserve := range []float64{0, 1e6} {
			root, _ := tr.Build(t, t.TempDir())
			markRoot(t, root)
			fs := &scaledFS{OSFS: OSFS{Platform: osx.Current()}, Scale: 1}
			fs.Reset(0)
			r := Rules{Container: ".mkv", SegmentSeconds: 900, StallFactor: 1e9, ReserveGB: reserve, MaxAgePruneFraction: 1, MaxRetentionDays: ptr(1e-6)}
			in, err := Collect(root, freePlatform{osx.Current(), fs}, r, now, false)
			if err != nil {
				t.Fatal(err)
			}
			want := archivetest.Walk(t, root, ".mkv", false)
			if _, err := (&Executor{FS: fs}).Run(in, &State{}); err != nil {
				t.Fatal(err)
			}
			got := archivetest.Walk(t, root, ".mkv", false)
			for _, c := range want.Cameras {
				g, _ := got.Camera(c.Name)
				if len(g.Footage) == 0 || g.Footage[0].Path != c.Footage[0].Path {
					t.Fatalf("spool %d reserve %v %s: the newest %s is gone (left %d)", spool, reserve, c.Name, c.Footage[0].Path, len(g.Footage))
				}
				if len(g.Footage) != 1 {
					t.Errorf("spool %d reserve %v %s: %d segments left, want only the newest", spool, reserve, c.Name, len(g.Footage))
				}
			}
		}
	}
}

// ---- empty day folders (B4) ----

// TestEmptyDayFolders: after a real run the day folders it emptied and the ones already empty go,
// when dated before today; a folder still holding anything, today's and a later day's, a folder
// that is not a day, an invalid date, a link named like a day and the camera folder stay. A second
// run removes nothing more; a dry run removes nothing.
func TestEmptyDayFolders(t *testing.T) {
	r := newRig(t)
	cam := filepath.Join(r.root, "Cam")
	local := r.now.In(time.Local)
	dayOf := func(d int) string { return local.AddDate(0, 0, d).Format("2006-01-02") }
	segIn := func(d int, hour int) string { // a segment of the local day d, named in the local zone
		day := local.AddDate(0, 0, d)
		st := time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, time.Local)
		return r.put(filepath.Join(cam, st.Format("2006-01-02"), "Cam_"+st.Format("20060102_150405-0700")+".mkv"), unit/64, st.Add(15*time.Minute))
	}
	for k := 0; k < 3; k++ {
		st := r.now.Add(-time.Duration(45-15*k) * time.Minute)
		mt := st.Add(15 * time.Minute)
		if k == 2 {
			mt = r.now.Add(-5 * time.Second)
		}
		r.put(filepath.Join(cam, segName("Cam", st, ".mkv")), unit/64, mt)
	}
	segIn(-3, 1)
	segIn(-3, 2)                                                // emptied by the run: removed
	segIn(-2, 3)                                                // emptied but for:
	r.put(filepath.Join(cam, dayOf(-2), "notes.txt"), 1, r.now) // a foreign file: kept
	mk := func(p string) string {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stray := mk(filepath.Join(cam, dayOf(-4)))            // already empty: removed
	nested := mk(filepath.Join(cam, dayOf(-5), "deeper")) // holds a folder: kept
	keep := []string{
		filepath.Join(cam, dayOf(-2)), filepath.Dir(nested), nested,
		mk(filepath.Join(cam, dayOf(0))), mk(filepath.Join(cam, dayOf(1))), // today, tomorrow: kept
		mk(filepath.Join(cam, "old")), mk(filepath.Join(cam, "2026-02-30")), mk(filepath.Join(r.root, "_tmp", dayOf(-6))),
	}
	outside := mk(filepath.Join(r.dir, "outside"))
	link := filepath.Join(cam, dayOf(-7))
	if _, n := (archivetest.Tree{Nodes: []archivetest.Node{{Path: "archive/Cam/" + dayOf(-7), Kind: "junction", Target: "outside"}}}).Build(t, r.dir); n == 1 {
		keep = append(keep, link, outside)
	}
	r.claimAny()
	r.rules.ReserveGB = 1e6
	r.fs.Reset(0)

	if res := r.run(true); len(res.DaysRemoved) != 0 || !exists(stray) {
		t.Fatalf("a dry run removed %v", res.DaysRemoved)
	}
	res := r.run(false)
	want := []string{filepath.Join(cam, dayOf(-4)), filepath.Join(cam, dayOf(-3))}
	sort.Strings(want)
	got := append([]string{}, res.DaysRemoved...)
	sort.Strings(got)
	if !eqList(got, want) {
		t.Fatalf("removed %v, want %v (deleted %d)", got, want, len(res.Deletions))
	}
	for _, p := range append(keep, cam) {
		if !exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	for _, p := range want {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if again := r.run(false); len(again.DaysRemoved) != 0 {
		t.Fatalf("the second run removed %v", again.DaysRemoved)
	}
	for _, p := range append(keep, cam) {
		if !exists(p) {
			t.Errorf("%s was removed by the second run", p)
		}
	}
}

// TestEmptyDayFolderRevalidated: a day folder that is swapped for a link, or that something is
// written into, after Collect listed it empty is not removed; untouched (the control) it is.
func TestEmptyDayFolderRevalidated(t *testing.T) {
	for _, change := range []string{"none", "link", "file"} {
		r := newRig(t)
		cam := filepath.Join(r.root, "Cam")
		r.put(filepath.Join(cam, segName("Cam", r.now.Add(-15*time.Minute), ".mkv")), unit/64, r.now.Add(-5*time.Second))
		day := filepath.Join(cam, r.now.In(time.Local).AddDate(0, 0, -3).Format("2006-01-02"))
		if err := os.MkdirAll(day, 0o755); err != nil {
			t.Fatal(err)
		}
		r.claimAny()
		in := r.inputs(false)
		switch change {
		case "link":
			os.Remove(day)
			os.MkdirAll(filepath.Join(r.dir, "outside"), 0o755)
			rel, _ := filepath.Rel(r.dir, day)
			if _, n := (archivetest.Tree{Nodes: []archivetest.Node{{Path: filepath.ToSlash(rel), Kind: "junction", Target: "outside"}}}).Build(t, r.dir); n != 1 {
				t.Skip("no directory link could be made")
			}
		case "file":
			r.put(filepath.Join(day, segName("Cam", r.now.Add(-72*time.Hour), ".mkv")), 1, r.now.Add(-72*time.Hour))
		}
		res, err := r.exec(false).Run(in, &r.st)
		if err != nil {
			t.Fatal(err)
		}
		if change == "none" {
			if len(res.DaysRemoved) != 1 || exists(day) {
				t.Fatalf("control: %v, still there %v", res.DaysRemoved, exists(day))
			}
			continue
		}
		if len(res.DaysRemoved) != 0 || !exists(day) {
			t.Fatalf("%s: removed %v", change, res.DaysRemoved)
		}
		if change == "link" && !exists(filepath.Join(r.dir, "outside")) {
			t.Fatal("the link's target is gone")
		}
	}
}
