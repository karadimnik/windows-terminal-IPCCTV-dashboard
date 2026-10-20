package prune

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
)

// The property test: thousands of generated archives (seeded, run once, a few seconds). For each,
// the plan is checked against rules re-derived here independently of plan.go, and the executor is
// run over an in-memory filesystem that changes files under it.
const (
	propSeed  = 20261020
	propTrees = 4000
)

// memFS is an in-memory FS. mutate runs before every Remove (a recorder writing, a file swapped).
type memFS struct {
	files   map[string]Info
	base    uint64
	freed   float64
	factor  float64 // how many bytes a removal really frees per byte (0: none, 2: double)
	fail    map[string]bool
	removed map[string]Info // what each removed file was when it went
	mutate  func(path string)
}

func (m *memFS) Stat(p string) (Info, error) {
	i, ok := m.files[p]
	if !ok {
		return Info{}, os.ErrNotExist
	}
	return i, nil
}

func (m *memFS) Remove(p string) error {
	if m.mutate != nil {
		m.mutate(p)
	}
	i, ok := m.files[p]
	switch {
	case m.fail[p]:
		return errors.New("simulated failure")
	case !ok:
		return os.ErrNotExist
	case i.Dir:
		return errors.New("is a directory")
	}
	delete(m.files, p)
	m.removed[p] = i
	m.freed += float64(i.Size) * m.factor
	return nil
}

func (m *memFS) FreeBytes(string) (uint64, error) { return m.base + uint64(m.freed), nil }

type genTree struct {
	in Inputs
	fs *memFS
}

var nearMiss = []func(cam string, t time.Time) string{
	func(cam string, t time.Time) string { // an Arabic-Indic digit in the date
		n := segName(cam, t, ".mkv")
		i := len(cam) + 3
		return n[:i] + "٣" + n[i+1:]
	},
	func(cam string, t time.Time) string { return cam + "_20261345_101010+0300.mkv" }, // month 13
	func(cam string, t time.Time) string { return segName(cam, t, ".mkv") + "\n" },
	func(cam string, t time.Time) string { return segName(cam, t, ".MKV") },
	func(cam string, t time.Time) string { return segName(strings.ToLower(cam), t, ".mkv") },
	func(cam string, t time.Time) string { return segName(cam, t, ".mkv.tmp") },
	func(cam string, t time.Time) string { return segName(cam, t, ".mp4") },
	func(cam string, t time.Time) string { return segName("Other", t, ".mkv") },
	func(cam string, t time.Time) string { return "notes.txt" },
	func(cam string, t time.Time) string { return cam + "_20261020_090000+9900.mkv" }, // offset out of range
}

func generate(rng *rand.Rand, root string) genTree {
	now := testNow
	if rng.IntN(20) == 0 {
		now = now.Add(30 * day) // the clock jumped
	}
	fs := &memFS{files: map[string]Info{root: {Dir: true}}, fail: map[string]bool{}, removed: map[string]Info{},
		factor: []float64{1, 1, 1, 0, 0.5, 2}[rng.IntN(6)]}
	in := Inputs{Root: root, Matcher: archive.Matcher{FoldCase: rng.IntN(2) == 0}, Now: now}
	r := &in.Rules
	r.Container, r.SegmentSeconds, r.StallFactor = ".mkv", 900, 2.5
	switch rng.IntN(4) {
	case 0:
	case 1:
		r.MaxRetentionDays = ptr([]float64{0.5, 1, 2, 5}[rng.IntN(4)])
	default:
		r.MaxRetentionDays = ptr(float64(rng.IntN(4) + 1))
	}
	r.MaxAgePruneFraction = []float64{0.3, 0.5, 1}[rng.IntN(3)]
	free := float64(rng.IntN(64*40)) / 64
	r.ReserveGB = math.Max(0, free+float64(rng.IntN(24)-12)/2)
	r.PruneHysteresisGB = []float64{0, 0.5, 2, 5}[rng.IntN(4)]
	in.FreeBytes = uint64(free * gib)
	fs.base = in.FreeBytes
	switch rng.IntN(30) {
	case 0:
		in.RootProblem = "RootPath '" + root + "' is the root of the volume"
	case 1:
		in.MarkerProblem = "there is no ownership marker at " + archive.MarkerPath(root)
	}

	withCreated := rng.IntN(3) == 0 // as on Windows
	names := []string{"Cam_A", "Front_Entrance", "Workshop", "_state", "Yard"}
	rng.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	for _, cam := range names[:rng.IntN(4)+1] {
		dir := filepath.Join(root, cam)
		c := Camera{Dir: dir, Reparse: rng.IntN(25) == 0}
		fs.files[dir] = Info{Dir: true, Reparse: c.Reparse}
		lastStart := now.Add(-time.Duration(rng.IntN(7200)) * time.Second)
		if rng.IntN(8) == 0 {
			lastStart = now.Add(-time.Duration(rng.IntN(5)+1) * day) // a dark camera
		}
		n := rng.IntN(12)
		for i := 0; i < n; i++ {
			start := lastStart.Add(-time.Duration(i) * 900 * time.Second).Add(-time.Duration(rng.IntN(5)) * time.Second)
			if rng.IntN(6) == 0 {
				start = start.Add(-time.Duration(rng.IntN(5*24)) * time.Hour)
			}
			zone := testZone
			if rng.IntN(5) == 0 {
				zone = time.FixedZone("", 2*3600)
			}
			name := archive.FormatSegmentName(cam, start.In(zone), ".mkv")
			mtime := start.Add(900 * time.Second)
			if i == 0 {
				mtime = now.Add(-time.Duration(rng.IntN(120)) * time.Second)
			}
			switch rng.IntN(25) {
			case 0:
				mtime = now.Add(time.Duration(rng.IntN(72)) * time.Hour) // written in the future
			case 1:
				mtime = now.Add(-time.Duration(rng.IntN(240)) * time.Hour)
			case 2:
				mtime = c.lastMTime() // an equal write time
			}
			mtime = mtime.Add(time.Duration(rng.IntN(1000)) * time.Millisecond).UTC()
			f := File{Name: name, MTime: mtime, Size: int64(rng.IntN(1<<11)+1) << 20, Reparse: rng.IntN(30) == 0}
			if withCreated {
				f.Created = start.Add(time.Duration(rng.IntN(1000)) * time.Millisecond).UTC()
				if rng.IntN(15) == 0 { // copied or restored
					f.Created = now.Add(-time.Duration(rng.IntN(3600)) * time.Second).UTC()
				}
			}
			c.Files = append(c.Files, f)
		}
		for i := rng.IntN(4); i > 0; i-- {
			t := now.Add(-time.Duration(rng.IntN(10*24)) * time.Hour)
			f := File{Name: nearMiss[rng.IntN(len(nearMiss))](cam, t), MTime: t.UTC(), Size: int64(rng.IntN(1<<11)+1) << 20}
			c.Files = append(c.Files, f)
		}
		for _, f := range c.Files {
			fs.files[filepath.Join(dir, f.Name)] = Info{Size: f.Size, MTime: f.MTime, Regular: !f.Reparse, Reparse: f.Reparse}
		}
		c.Newest = refWriteNewest(c, in.Matcher)
		if rng.IntN(25) == 0 && len(c.Files) > 0 {
			c.Newest = c.Files[rng.IntN(len(c.Files))].Name // a scan that disagrees
		}
		in.Cameras = append(in.Cameras, c)
	}
	return genTree{in: in, fs: fs}
}

func (c Camera) lastMTime() time.Time {
	if len(c.Files) == 0 {
		return testNow
	}
	return c.Files[len(c.Files)-1].MTime
}

// refAllow is the allowlist re-derived: a plain file in a plain, non-underscore folder directly
// under the root, named <folder>_YYYYMMDD_HHMMSS+HHMM.mkv with ASCII digits forming a real date,
// time and offset; prefix and extension case-insensitive only when the matcher folds.
func refAllow(in Inputs, c Camera, f File) (time.Time, bool) {
	folder := filepath.Base(c.Dir)
	if c.Reparse || f.Reparse || strings.HasPrefix(folder, "_") || filepath.Dir(c.Dir) != filepath.Clean(in.Root) {
		return time.Time{}, false
	}
	flags := ""
	if in.Matcher.FoldCase {
		flags = "(?i)"
	}
	m := regexp.MustCompile(`^` + flags + regexp.QuoteMeta(folder) + `_([0-9]{8}_[0-9]{6}[+-][0-9]{4})\.mkv\z`).FindStringSubmatch(f.Name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102_150405-0700", m[1])
	if err != nil || t.Format("20060102_150405-0700") != m[1] || m[1][16:18] > "14" || m[1][18:20] > "59" {
		return time.Time{}, false
	}
	return t, true
}

func refWriteNewest(c Camera, m archive.Matcher) string {
	best := -1
	for i, f := range c.Files {
		if f.Reparse || !m.IsSegmentOf(f.Name, filepath.Base(c.Dir), ".mkv") {
			continue
		}
		if best < 0 || f.MTime.After(c.Files[best].MTime) || (f.MTime.Equal(c.Files[best].MTime) && f.Name > c.Files[best].Name) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return c.Files[best].Name
}

// checkPlan returns every way d breaks the guarantees for in (empty: none).
func checkPlan(in Inputs, d Decision) []string {
	var bad []string
	if in.RootProblem != "" || in.MarkerProblem != "" {
		if len(d.Deletions) > 0 || d.Refusal == nil || (d.Refusal.Gate != GateRoot && d.Refusal.Gate != GateMarker) {
			bad = append(bad, "(c) deleted or did not refuse under a root/marker problem")
		}
		return bad
	}
	type ref struct {
		cam     Camera
		f       File
		start   time.Time
		path    string
		protect bool
	}
	byPath := map[string]*ref{}
	var unprotected, newest, seen []*ref
	for _, c := range in.Cameras {
		var segs []*ref
		for _, f := range c.Files {
			if t, ok := refAllow(in, c, f); ok {
				segs = append(segs, &ref{cam: c, f: f, start: t, path: filepath.Join(c.Dir, f.Name)})
			}
		}
		if len(segs) == 0 {
			continue
		}
		if c.Newest != "" {
			found := false
			for _, s := range segs {
				found = found || in.same(s.f.Name, c.Newest)
			}
			if !found {
				for _, s := range segs {
					s.protect = true
					byPath[s.path] = s
				}
				continue
			}
		}
		var top, ctop time.Time
		var w *ref
		for _, s := range segs {
			if s.start.After(top) {
				top = s.start
			}
			if s.f.Created.After(ctop) {
				ctop = s.f.Created
			}
			if w == nil || s.f.MTime.After(w.f.MTime) || (s.f.MTime.Equal(w.f.MTime) && s.f.Name > w.f.Name) {
				w = s
			}
		}
		newestOf := func(s *ref) bool { return s.start.Equal(top) || (!ctop.IsZero() && s.f.Created.Equal(ctop)) }
		var rep *ref
		for _, s := range segs {
			if newestOf(s) {
				s.protect = true
				if rep == nil || s.f.MTime.Before(rep.f.MTime) {
					rep = s
				}
			}
		}
		w.protect = true
		newest = append(newest, rep)
		seen = append(seen, segs...)
		for _, s := range segs {
			if in.same(s.f.Name, c.Newest) {
				s.protect = true
			}
			if !newestOf(s) {
				unprotected = append(unprotected, s)
			}
			byPath[s.path] = s
		}
		for _, s := range segs {
			if !s.protect && !s.f.MTime.Before(rep.f.MTime) {
				s.protect = true // not strictly older than the folder's newest
			}
		}
	}
	free := roundGB(float64(in.FreeBytes))
	age := in.Rules.MaxRetentionDays != nil && *in.Rules.MaxRetentionDays > 0
	clockBad := false
	if len(unprotected) > 0 && len(newest) > 0 && (age || free < in.Rules.ReserveGB) {
		oldest, top := unprotected[0], newest[0]
		for _, s := range unprotected {
			if s.f.MTime.Before(oldest.f.MTime) {
				oldest = s
			}
		}
		for _, s := range newest {
			if s.f.MTime.After(top.f.MTime) {
				top = s
			}
		}
		clockBad = oldest.f.MTime.After(top.f.MTime) || in.Now.Sub(top.f.MTime).Seconds() > 2250
		for _, s := range seen {
			clockBad = clockBad || s.f.MTime.After(in.Now.Add(time.Minute))
		}
	}
	if clockBad != (d.Refusal != nil && d.Refusal.Gate == GateClock) {
		bad = append(bad, fmt.Sprintf("(c) clock gate: expected refusal %v, got %+v", clockBad, d.Refusal))
	}
	if clockBad && len(d.Deletions) > 0 {
		bad = append(bad, "(c) deleted although the clock gate refuses")
	}
	var cutoff time.Time
	expired, held := 0, false
	if age {
		cutoff = in.Now.Add(-time.Duration(*in.Rules.MaxRetentionDays * float64(day)))
		var eb, tb float64
		for _, s := range seen {
			tb += float64(s.f.Size)
			if !s.protect && s.f.MTime.Before(cutoff) {
				expired++
				eb += float64(s.f.Size)
			}
		}
		lim := in.Rules.MaxAgePruneFraction
		held = expired > 0 && (float64(expired)/float64(len(seen)) > lim || (tb > 0 && eb/tb > lim))
	}
	if held != d.BreakerHeld && !clockBad {
		bad = append(bad, fmt.Sprintf("breaker: expected held %v, got %v", held, d.BreakerHeld))
	}
	done := map[string]bool{}
	for _, x := range d.Deletions {
		s := byPath[x.Path]
		switch {
		case done[x.Path]:
			bad = append(bad, "deleted twice: "+x.Path)
		case s == nil:
			bad = append(bad, "(b) deleted a file the allowlist refuses: "+x.Path)
		case s.protect:
			bad = append(bad, "(a) deleted a protected segment: "+x.Path)
		case x.Reason == ReasonAge && (held || !s.f.MTime.Before(cutoff)):
			bad = append(bad, "deleted by age inside the cutoff or past the breaker: "+x.Path)
		case !x.MTime.Before(x.GuardMTime) || byPath[x.Guard] == nil || !byPath[x.Guard].protect:
			bad = append(bad, "not strictly older than a protected guard: "+x.Path)
		}
		done[x.Path] = true
	}
	return bad
}

func runProperty(t *testing.T, n int, seed uint64) (violations int, first string) {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	root := filepath.Join(os.TempDir(), "holocron-prop", "archive")
	deleted, refusedClock, held, blocked, removed, skipped, failed := 0, 0, 0, 0, 0, 0, 0
	for i := 0; i < n; i++ {
		g := generate(rng, root)
		d := Plan(g.in)
		bad := checkPlan(g.in, d)

		// (d) the executor over a filesystem that changes under it never removes more than the plan,
		// nor a file that changed after the plan.
		changed := map[string]bool{}
		planned := map[string]bool{}
		for _, x := range d.Deletions {
			planned[x.Path] = true
			if rng.IntN(12) == 0 {
				g.fs.fail[x.Path] = true
			}
		}
		guardOf := map[string]string{}
		for _, x := range d.Deletions {
			guardOf[x.Path] = x.Guard
		}
		// Changes land between two removals; a change inside the window between one file's
		// re-check and its own unlink is not detectable by any executor and is not generated.
		g.fs.mutate = func(removing string) {
			if rng.IntN(5) != 0 || len(d.Deletions) == 0 {
				return
			}
			p := d.Deletions[rng.IntN(len(d.Deletions))].Path
			if rng.IntN(4) == 0 {
				p = d.Deletions[rng.IntN(len(d.Deletions))].Guard
			}
			if p == removing || p == guardOf[removing] || g.fs.removed[p] != (Info{}) {
				return
			}
			if fi, ok := g.fs.files[p]; ok {
				switch rng.IntN(4) {
				case 0:
					fi.Size += 4096
				case 1:
					fi.MTime = fi.MTime.Add(time.Second)
				case 2:
					fi.Reparse, fi.Regular = true, false
				default:
					delete(g.fs.files, p)
					changed[p] = true
					return
				}
				g.fs.files[p] = fi
				changed[p] = true
			}
		}
		var st State
		res, err := (&Executor{FS: g.fs}).Run(g.in, &st)
		if err != nil {
			bad = append(bad, "executor: "+err.Error())
		}
		for p := range g.fs.removed {
			if !planned[p] {
				bad = append(bad, "(d) the executor removed an unplanned file: "+p)
			}
			if changed[p] {
				bad = append(bad, "(d) the executor removed a file that changed after the plan: "+p)
			}
		}
		if len(res.Deletions) != len(g.fs.removed) {
			bad = append(bad, fmt.Sprintf("(d) reported %d deletions, removed %d", len(res.Deletions), len(g.fs.removed)))
		}
		if len(bad) > 0 {
			violations++
			if first == "" {
				first = fmt.Sprintf("tree %d: %s", i, strings.Join(bad, "; "))
			}
		}
		deleted += len(d.Deletions)
		removed += len(g.fs.removed)
		skipped += len(res.Skipped)
		failed += res.Failed
		if d.Refusal != nil && d.Refusal.Gate == GateClock {
			refusedClock++
		}
		if d.BreakerHeld {
			held++
		}
		if d.Blocked && d.Refusal == nil {
			blocked++
		}
	}
	t.Logf("seed %d, %d trees: %d planned deletions, %d clock refusals, %d breaker holds, %d blocked runs; executor removed %d, skipped %d changed files, %d failed removals",
		seed, n, deleted, refusedClock, held, blocked, removed, skipped, failed)
	return violations, first
}

func TestPropertyGuarantees(t *testing.T) {
	if v, first := runProperty(t, propTrees, propSeed); v > 0 {
		t.Fatalf("%d of %d trees broke a guarantee; first: %s", v, propTrees, first)
	}
}

// Negative controls: each deliberate planner defect must be caught by the property checks.
func TestPropertyNegativeControls(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(bool)
	}{
		{"protect the oldest instead of the newest", func(on bool) { faults.protectOldest = on }},
		{"skip the clock gate", func(on bool) { faults.skipClock = on }},
		{"skip the circuit breaker", func(on bool) { faults.skipBreaker = on }},
	} {
		c.set(true)
		v, first := runProperty(t, 600, propSeed)
		c.set(false)
		if v == 0 {
			t.Fatalf("%s: the property checks found nothing", c.name)
		}
		t.Logf("%s: caught in %d of 600 trees (%s)", c.name, v, first)
	}
}
