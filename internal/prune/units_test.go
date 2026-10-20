package prune

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
)

func TestRoundGBIsHalfToEven(t *testing.T) {
	for gb, want := range map[float64]float64{10.125: 10.12, 10.375: 10.38, 0.625: 0.62, 0.875: 0.88, 10.126: 10.13, 15: 15} {
		if got := roundGB(gb * gib); got != want {
			t.Errorf("roundGB(%v GB) = %v, want %v", gb, got, want)
		}
	}
}

// The tie feeds a decision: 10.125 GB free against a 10.13 GB reserve is 10.12 < 10.13 with
// banker's rounding (PowerShell) and would be 10.13, not below, rounding half up.
func TestWatermarkRoundingTie(t *testing.T) {
	cam := filepath.Join(os.TempDir(), "tie", "archive", "Cam_A")
	in := Inputs{Root: filepath.Dir(cam), Now: testNow, FreeBytes: uint64(10.125 * gib),
		Rules: Rules{ReserveGB: 10.13, Container: ".mkv", SegmentSeconds: 900, StallFactor: 2.5},
		Cameras: []Camera{{Dir: cam, Files: []File{
			{Name: segName("Cam_A", testNow.Add(-2*time.Hour), ".mkv"), MTime: testNow.Add(-2 * time.Hour), Size: gib / 100},
			{Name: segName("Cam_A", testNow.Add(-time.Minute), ".mkv"), MTime: testNow.Add(-time.Second), Size: gib},
		}}}}
	d := Plan(in)
	if !d.Watermark || d.FreeGBBefore != 10.12 || len(d.Deletions) != 1 || !strings.Contains(d.Deletions[0].Detail, "free 10.12 GB below reserve 10.13 GB") {
		t.Fatalf("watermark %v, free %v, deletions %+v", d.Watermark, d.FreeGBBefore, d.Deletions)
	}
	above := 10.135
	in.FreeBytes = uint64(above * gib) // just under 10.135 in binary: rounds to 10.13, not below
	if d := Plan(in); d.Watermark || len(d.Deletions) != 0 {
		t.Fatalf("10.135 GB: watermark %v", d.Watermark)
	}
}

func TestNetFixed(t *testing.T) {
	for _, c := range []struct {
		v    float64
		dec  int
		trim bool
		want string
	}{
		{10.125, 2, false, "10.13"}, {15, 2, false, "15.00"}, {0.6 * 100, 1, false, "60.0"}, {8.0 / 11 * 100, 1, false, "72.7"},
		{0.0005, 3, false, "0.001"}, {-0.001, 2, false, "0.00"}, {0.5, 2, true, "0.5"}, {1, 2, true, "1"}, {0.05, 2, true, "0.05"},
		{999.995, 2, false, "1000.00"}, {1e-20, 2, false, "0.00"}, {123456789.125, 2, false, "123456789.13"},
	} {
		got := netFixed(c.v, c.dec)
		if c.trim {
			got = netTrim(c.v, c.dec)
		}
		if got != c.want {
			t.Errorf("format(%v, %d) = %q, want %q", c.v, c.dec, got, c.want)
		}
	}
}

// IR U5: equal write times are ordered by file name, then folder.
func TestEqualWriteTimesOrderByName(t *testing.T) {
	root := filepath.Join(os.TempDir(), "u5", "archive")
	at := testNow.Add(-3 * day)
	mk := func(cam string, names ...time.Time) Camera {
		c := Camera{Dir: filepath.Join(root, cam)}
		for _, n := range names {
			c.Files = append(c.Files, File{Name: segName(cam, n, ".mkv"), MTime: at, Size: gib})
		}
		c.Files = append(c.Files, File{Name: segName(cam, testNow.Add(-time.Minute), ".mkv"), MTime: testNow, Size: gib})
		return c
	}
	in := Inputs{Root: root, Now: testNow, FreeBytes: 0, Rules: Rules{ReserveGB: 100, Container: ".mkv", SegmentSeconds: 900, StallFactor: 2.5},
		Cameras: []Camera{mk("Workshop", at.Add(-time.Hour)), mk("Cam_A", at.Add(time.Hour), at)}}
	d := Plan(in)
	want := []string{segName("Cam_A", at, ".mkv"), segName("Cam_A", at.Add(time.Hour), ".mkv"), segName("Workshop", at.Add(-time.Hour), ".mkv")}
	if got := base(paths(d.Deletions)); !eqList(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}

func TestExecutorRechecksEachFile(t *testing.T) {
	type tc struct {
		name   string
		change func(r *rig, victim, guard string)
		why    string
	}
	for _, c := range []tc{
		{"size", func(r *rig, v, _ string) { r.put(v, unit/32, fileTime(t, v)) }, "size changed"},
		{"mtime", func(r *rig, v, _ string) { r.touch(v, fileTime(t, v).Add(time.Second)) }, "modified after the plan"},
		{"gone", func(r *rig, v, _ string) { os.Remove(v) }, "it is gone"},
		{"guard-gone", func(r *rig, _, g string) { os.Remove(g) }, "protected newest segment"},
		{"guard-older", func(r *rig, v, g string) { r.touch(g, fileTime(t, v).Add(-time.Hour)) }, "no longer strictly older"},
		{"link", func(r *rig, v, _ string) {
			mt := fileTime(t, v)
			os.Remove(v)
			if err := os.Symlink(filepath.Join(r.dir, "elsewhere"), v); err != nil {
				t.Skip("no symbolic links here:", err)
			}
			_ = mt
		}, "link"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
			a := r.seg("Cam_A", r.now.Add(-5*day))
			b := r.seg("Cam_A", r.now.Add(-4*day))
			guard := r.seg("Cam_A", r.now.Add(-time.Minute))
			r.claim()
			in := r.inputs(false)
			r.fs.Before = func(p string) {
				if p == a {
					c.change(r, b, guard)
				}
			}
			res, err := r.exec(false).Run(in, &r.st)
			if err != nil {
				t.Fatal(err)
			}
			if !eqList(paths(res.Plan.Deletions), []string{a, b}) {
				t.Fatalf("plan %v", base(paths(res.Plan.Deletions)))
			}
			if !eqList(paths(res.Deletions), []string{a}) || len(res.Skipped) != 1 || res.Skipped[0].Path != b || !strings.Contains(res.Skipped[0].Why, c.why) {
				t.Fatalf("deleted %v, skipped %+v", base(paths(res.Deletions)), res.Skipped)
			}
			if n := r.log.count(func(x rec) bool { return x.Attrs["action"] == "prune.delete" && x.Attrs["result"] == "refused" }); n != 1 {
				t.Fatalf("%d refused prune.delete records", n)
			}
		})
	}
}

func fileTime(t *testing.T, p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// The executor deletes only planned files: when the disk frees nothing, it stops at the end of
// the plan instead of going on down the candidate list, and does not call that BLOCKED.
func TestExecutorNeverExceedsThePlan(t *testing.T) {
	r := newRig(t)
	var all []string
	for h := 10; h >= 2; h-- {
		all = append(all, r.seg("Cam_A", r.now.Add(-time.Duration(h)*time.Hour), unit))
	}
	r.seg("Cam_A", r.now.Add(-time.Minute), unit)
	r.claim()
	r.fs.Reset(10)
	r.rules.ReserveGB, r.rules.PruneHysteresisGB = 12, 1
	in := r.inputs(false)
	r.fs.Fixed = true // the plan expects 3 deletions to reach 13 GB; the disk frees nothing
	res, err := r.exec(false).Run(in, &r.st)
	if err != nil {
		t.Fatal(err)
	}
	if !eqList(paths(res.Plan.Deletions), all[:3]) || !eqList(paths(res.Deletions), all[:3]) || !eqList(sorted(r.fs.Removed), sorted(all[:3])) {
		t.Fatalf("plan %v, deleted %v", base(paths(res.Plan.Deletions)), base(paths(res.Deletions)))
	}
	if res.Blocked || !res.Short || len(res.Alerts) != 0 || !strings.Contains(res.StopLine, "stopped at the end of the plan") {
		t.Fatalf("blocked %v short %v alerts %v %q", res.Blocked, res.Short, res.Alerts, res.StopLine)
	}
}

func TestExecutorCountsAFailedRemovalAndGoesOn(t *testing.T) {
	r := newRig(t)
	r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
	a := r.seg("Cam_A", r.now.Add(-5*day))
	b := r.seg("Cam_A", r.now.Add(-4*day))
	r.seg("Cam_A", r.now.Add(-time.Minute))
	r.claim()
	r.fs.FailOn = map[string]bool{a: true}
	res := r.run(false)
	if res.Failed != 1 || !eqList(paths(res.Deletions), []string{b}) || !exists(a) {
		t.Fatalf("failed %d, deleted %v", res.Failed, base(paths(res.Deletions)))
	}
	if r.log.count(func(x rec) bool {
		return x.Attrs["action"] == "prune.delete" && x.Attrs["result"] == "fail" && x.Level.String() == "ERROR"
	}) != 1 {
		t.Fatal("the failure was not logged at ERROR")
	}
}

func TestDryRunRemovesNothing(t *testing.T) {
	r := newRig(t)
	r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
	r.seg("Cam_A", r.now.Add(-5*day))
	r.seg("Cam_A", r.now.Add(-time.Minute))
	r.claim()
	before := r.manifest()
	res := r.run(true)
	removed, changed, added := diffManifest(before, r.manifest())
	if len(res.Deletions) != 1 || len(removed)+len(changed)+len(added) > 0 || len(r.fs.Removed) > 0 || !res.DryRun {
		t.Fatalf("dry run: %d planned, files changed %d", len(res.Deletions), len(removed)+len(changed)+len(added))
	}
}

func TestNeverRemovesADirectory(t *testing.T) {
	r := newRig(t)
	r.rules.MaxRetentionDays, r.rules.MaxAgePruneFraction = ptr(1), 1.0
	old := r.seg("Cam_A", r.now.Add(-5*day))
	r.seg("Cam_A", r.now.Add(-time.Minute))
	r.claim()
	dirSeg := filepath.Join(r.root, "Cam_A", segName("Cam_A", r.now.Add(-6*day), ".mkv"))
	if err := os.MkdirAll(dirSeg, 0o755); err != nil { // a FOLDER named like an expired segment
		t.Fatal(err)
	}
	res := r.run(false)
	if !eqList(paths(res.Deletions), []string{old}) || !exists(dirSeg) {
		t.Fatalf("deleted %v; the folder exists: %v", base(paths(res.Deletions)), exists(dirSeg))
	}
	if err := removeFile(dirSeg); err == nil || !exists(dirSeg) {
		t.Fatalf("removeFile removed a directory: %v", err)
	}
}

func TestSandboxGuard(t *testing.T) {
	tmp := os.TempDir()
	inside := filepath.Join(t.TempDir(), "archive")
	if err := CheckSandbox(inside, tmp); err != nil {
		t.Fatalf("a t.TempDir root was refused: %v", err)
	}
	refused := []string{`E:\cameras`, `E:\`, `C:\`, "/srv/cameras", "/", tmp, filepath.Join(tmp, "..", "cameras"), filepath.Join(tmp, "..")}
	if home, err := os.UserHomeDir(); err == nil {
		refused = append(refused, home, filepath.Join(home, "Videos", "cameras"))
	}
	if wd, err := os.Getwd(); err == nil {
		refused = append(refused, wd) // the package folder, inside the repository
		for d := wd; ; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
				refused = append(refused, d, filepath.Join(d, "cameras"))
				break
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	if runtime.GOOS == "windows" {
		refused = append(refused, `\\wsl.localhost\Ubuntu\home`, `\\server\share\cameras`)
	}
	for _, p := range refused {
		if err := CheckSandbox(p, tmp); !errors.Is(err, ErrOutsideSandbox) {
			t.Errorf("CheckSandbox(%q) = %v, want a refusal", p, err)
		}
	}
	// A link inside the sandbox that points outside it is refused by its target.
	link := filepath.Join(t.TempDir(), "escape")
	if err := os.Symlink(filepath.Dir(mustWd(t)), link); err == nil {
		if err := CheckSandbox(filepath.Join(link, "archive"), tmp); !errors.Is(err, ErrOutsideSandbox) {
			t.Errorf("a link out of the sandbox passed: %v", err)
		}
	}

	// The executor refuses before it plans or touches anything; SANDBOX_ONLY is on in this package.
	if os.Getenv(SandboxEnv) == "" {
		t.Fatal("SANDBOX_ONLY is not set for this test run")
	}
	for _, root := range []string{`E:\cameras`, mustWd(t)} {
		fs := &memFS{files: map[string]Info{}, removed: map[string]Info{}, mutate: func(p string) { t.Fatalf("Remove(%s) called", p) }}
		in := Inputs{Root: root, Now: testNow, Rules: Rules{Container: ".mkv", ReserveGB: 1e9, SegmentSeconds: 900, StallFactor: 2.5},
			Cameras: []Camera{{Dir: filepath.Join(root, "Cam_A"), Files: []File{
				{Name: segName("Cam_A", testNow.Add(-day), ".mkv"), MTime: testNow.Add(-day), Size: gib},
				{Name: segName("Cam_A", testNow, ".mkv"), MTime: testNow, Size: gib},
			}}}}
		if _, err := (&Executor{FS: fs}).Run(in, &State{}); !errors.Is(err, ErrOutsideSandbox) {
			t.Errorf("Run(%q) = %v, want a sandbox refusal", root, err)
		}
		if _, err := (&Executor{FS: fs, Sandbox: t.TempDir()}).Run(in, &State{}); !errors.Is(err, ErrOutsideSandbox) {
			t.Errorf("Run(%q) with an explicit sandbox = %v", root, err)
		}
	}
}

func mustWd(t *testing.T) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// Collect reports links and never enters them; Newest comes from archive.Scan.
func TestCollect(t *testing.T) {
	r := newRig(t)
	r.seg("Cam_A", r.now.Add(-time.Hour))
	newest := r.seg("Cam_A", r.now.Add(-time.Minute))
	r.claim()
	r.seg("_state", r.now.Add(-time.Hour))
	r.put(filepath.Join(r.root, "Cam_A", "sub", "x.mkv"), 1, r.now)
	link := r.symlink(filepath.Join(r.dir), filepath.Join(r.root, "Out"))
	in := r.inputs(false)
	if len(in.Cameras) != 1 || len(in.Cameras[0].Files) != 2 || in.Cameras[0].Newest != filepath.Base(newest) {
		t.Fatalf("cameras %+v", in.Cameras)
	}
	if link && (len(in.Skipped) != 1 || !strings.Contains(in.Skipped[0], "reparse point")) {
		t.Fatalf("skipped %v", in.Skipped)
	}
	if in.Matcher != archive.DefaultMatcher || in.RootProblem != "" || in.MarkerProblem != "" {
		t.Fatalf("%+v", in)
	}
}
