package supervise

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/clock"
	"holocron/internal/config"
	"holocron/internal/osx"
	"holocron/internal/prune"
)

type fakeMover struct {
	clk *clock.Fake

	mu     sync.Mutex
	runs   []clock.Mono
	result func(n int) archive.MoveReport
	block  chan struct{} // when set, a cycle waits for it to close
}

func (f *fakeMover) File(time.Time) archive.MoveReport {
	f.mu.Lock()
	f.runs = append(f.runs, f.clk.Mono())
	n, r, b := len(f.runs), f.result, f.block
	f.mu.Unlock()
	if b != nil {
		<-b
	}
	if r == nil {
		return archive.MoveReport{}
	}
	return r(n)
}

func (f *fakeMover) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.runs) }

func moverGoroutines() int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "supervise.(*Supervisor).moveLoop")
}

// C4: the mover runs at start and every MoveIntervalSeconds; MoveToDayFolders=false stops it at the
// next reconcile without a restart and true resumes it; the loop is gone after the stop.
func TestSupervisorMoverLoopAndKillSwitch(t *testing.T) {
	r := newSRig(t, nil, func(c *config.Config) { c.MoveIntervalSeconds = 10 })
	r.mv.result = func(int) archive.MoveReport {
		return archive.MoveReport{Moved: []archive.Move{{Camera: "Front", Bytes: 3}}, Bytes: 3}
	}
	r.start()
	r.step(25 * time.Second)
	if n := r.mv.count(); n != 3 {
		t.Fatalf("cycles in 25 s at 10 s: %d", n)
	}
	if n := strings.Count(r.logText(), "filed 1 segment(s), 3 byte(s)"); n != 3 {
		t.Fatalf("%d cycle line(s)\n%s", n, r.logText())
	}
	r.src.mu.Lock()
	r.src.cfg.MoveToDayFolders = false
	r.src.mu.Unlock()
	r.step(6 * time.Second) // a reconcile
	before := r.mv.count()
	r.step(30 * time.Second)
	if n := r.mv.count(); n != before {
		t.Errorf("switched off: %d more cycle(s)", n-before)
	}
	if strings.Contains(r.logText(), "keeps the values it started with") {
		t.Error("the switch was reported as needing a restart")
	}
	r.src.mu.Lock()
	r.src.cfg.MoveToDayFolders = true
	r.src.mu.Unlock()
	r.step(16 * time.Second)
	if n := r.mv.count(); n <= before {
		t.Error("switched on again: no cycle")
	}
	if !strings.Contains(r.logText(), "MoveToDayFolders is now false") || !strings.Contains(r.logText(), "archive.file") {
		t.Errorf("log lines missing\n%s", r.logText())
	}
	r.stop(StopRequested)
	r.validateAll()
	if n := moverGoroutines(); n != 0 {
		t.Errorf("%d mover goroutine(s) left after the stop", n)
	}
}

// A cycle that only met busy files or errors doubles the wait (10, 20, 40 s); a clean one resets it.
func TestSupervisorMoverBacksOff(t *testing.T) {
	r := newSRig(t, nil, func(c *config.Config) { c.MoveIntervalSeconds = 10 })
	r.mv.result = func(n int) archive.MoveReport {
		if n <= 3 {
			return archive.MoveReport{Busy: []archive.MoveSkip{{Path: "x", Why: "in use"}}}
		}
		return archive.MoveReport{}
	}
	r.start()
	r.step(150 * time.Second)
	r.mv.mu.Lock()
	var gaps []int
	for i := 1; i < len(r.mv.runs) && i < 6; i++ {
		gaps = append(gaps, int(time.Duration(r.mv.runs[i]-r.mv.runs[i-1]).Seconds()+0.5))
	}
	r.mv.mu.Unlock()
	if want := []int{20, 40, 80, 10}; len(gaps) < 4 || gaps[0] != want[0] || gaps[1] != want[1] || gaps[2] != want[2] {
		t.Errorf("gaps %v, want %v", gaps, want)
	}
	if strings.Contains(r.logText(), "ALERT") {
		t.Error("a busy file escalated to an alert")
	}
	r.stop(StopRequested)
}

// C4: a mover hung in the file system holds neither the status tick nor the stop.
func TestSupervisorHungMoverDoesNotStallStatus(t *testing.T) {
	r := newSRig(t, nil, nil)
	r.mv.block = make(chan struct{})
	defer close(r.mv.block)
	r.start()
	docs := r.docCount()
	r.step(30 * time.Second)
	if n := r.docCount() - docs; n < 5 {
		t.Fatalf("%d status document(s) in 30 s with the mover hung", n)
	}
	r.stop(StopRequested)
	if !strings.Contains(r.logText(), "the mover is still in a cycle") {
		t.Error("the stop did not report the hung mover")
	}
}

// C5: the production mover and pruner on a flat archive of 5 days, two cameras: after a few cycles
// only the open and the newest closed segment stay in each spool and every other segment is filed
// under its date; the age rule (MaxRetentionDays 3) then deletes exactly what it deletes on the same
// archive left flat (mover off); status rows agree once the archive-wide scan ran after the last
// move (until then a row's ArchiveFiles lacks what was filed since the previous one); no camera-down alert.
func TestSupervisorMoverEndToEnd(t *testing.T) {
	cams := []config.Camera{{Name: "Front", IP: "192.168.1.50", Quality: "stream1", Record: true}, {Name: "Back", IP: "192.168.1.51", Quality: "stream1", Record: true}}
	type outcome struct {
		spool, filed int
		names        []string
		rows         string
	}
	run := func(move bool) outcome {
		days := 3.0
		r := newSRig(t, cams, func(c *config.Config) {
			c.MoveToDayFolders, c.MoveIntervalSeconds, c.MoveMaxPerCycle = move, 10, 100
			c.PruneIntervalSeconds, c.MaxRetentionDays = 45, &days
		})
		r.deps.Pruner, r.deps.Mover, r.deps.Scanner = nil, nil, nil
		for _, cam := range []string{"Front", "Back"} {
			for k := 0; k < 5*96; k++ {
				st := tStart.Add(-time.Duration(k+1) * 15 * time.Minute)
				mt := st.Add(15 * time.Minute)
				if k == 0 {
					mt = tStart.Add(-5 * time.Second)
				}
				writeSeg(t, filepath.Join(r.root, cam, cam+"_"+st.UTC().Format("20060102_150405")+"+0000.mkv"), mt)
			}
		}
		r.start()
		// mover cycles every 10 s, prune (age rule) at 0 and 45 s, the archive-wide scan (which adds
		// the filed days to a row's ArchiveFiles) at 0 and 60 s
		r.step(65 * time.Second)
		var o outcome
		l := archive.Layout{Root: r.root, Matcher: archive.MatcherFor(r.plat), Container: ".mkv"}
		for _, cam := range []string{"Front", "Back"} {
			segs, _ := l.Segments(cam)
			for _, s := range segs {
				if s.Day == "" {
					o.spool++
				} else {
					o.filed++
				}
				o.names = append(o.names, s.Name())
			}
		}
		sort.Strings(o.names)
		for _, row := range r.docNow().Cameras {
			b, _ := json.Marshal(row)
			o.rows += strings.ReplaceAll(string(b), filepath.ToSlash(r.root), "<root>") + "\n"
		}
		if r.al.count(110) != 0 || strings.Contains(r.logText(), "not filed") {
			t.Errorf("move %v: alerts %v\n%s", move, r.al.ids(), r.logText())
		}
		r.stop(StopRequested)
		r.validateAll()
		return o
	}
	flat, filed := run(false), run(true)
	if flat.filed != 0 || filed.spool != 4 || filed.filed == 0 {
		t.Fatalf("flat %d/%d, filed %d in the spools and %d in day folders", flat.spool, flat.filed, filed.spool, filed.filed)
	}
	if strings.Join(flat.names, ",") != strings.Join(filed.names, ",") || len(flat.names) >= 2*5*96 {
		t.Errorf("the pruner kept %d segment(s) flat and %d filed (of %d)", len(flat.names), len(filed.names), 2*5*96)
	}
	if flat.rows != filed.rows {
		t.Errorf("rows differ:\nflat\n%s\nfiled\n%s", flat.rows, filed.rows)
	}
}

// C4/(b): the mover files segments between a prune plan and its execution: each planned deletion
// of a moved file is a benign skip ("it is gone"), nothing fails, no alert; the next run deletes
// them in their day folders (positive control).
func TestPrunePlanThenMoverThenExec(t *testing.T) {
	root := filepath.Join(t.TempDir(), "archive")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for k := 0; k < 8; k++ {
		st := now.Add(-time.Duration(k+1) * 6 * time.Hour)
		mt := st.Add(15 * time.Minute)
		if k == 0 {
			mt = now.Add(-5 * time.Second)
		}
		writeSeg(t, filepath.Join(root, "Front", "Front_"+st.Format("20060102_150405")+"+0000.mkv"), mt)
	}
	p := osx.Current()
	if res, err := archive.ClaimRoot(root, p, ".mkv", now); err != nil || !res.Owned {
		t.Fatalf("claim %+v %v", res, err)
	}
	cfg := config.Defaults(p)
	days, frac := 1.0, 1.0
	cfg.MaxRetentionDays, cfg.MaxAgePruneFraction, cfg.ReserveGB, cfg.PruneHysteresisGB = &days, frac, 0, 0
	rules := prune.RulesFrom(cfg)
	in, err := prune.Collect(root, p, rules, now, false)
	if err != nil {
		t.Fatal(err)
	}
	l := archive.Layout{Root: root, Matcher: archive.MatcherFor(p), Container: ".mkv"}
	mv := archive.FileDays(l, archive.MoveOptions{Platform: p, Now: now, Margin: 2 * time.Minute})
	if len(mv.Moved) == 0 {
		t.Fatalf("the mover moved nothing: %+v", mv)
	}
	ex := prune.Executor{FS: prune.OSFS{Platform: p}}
	st := &prune.State{}
	res, err := ex.Run(in, st)
	if err != nil || res.Failed != 0 || len(res.Alerts) != 0 || len(res.Skipped) == 0 {
		t.Fatalf("err %v failed %d alerts %v skipped %d", err, res.Failed, res.Alerts, len(res.Skipped))
	}
	for _, s := range res.Skipped {
		if s.Why != "it is gone" {
			t.Errorf("skip %s: %s", s.Path, s.Why)
		}
	}
	in, _ = prune.Collect(root, p, rules, now, false)
	if res, _ := ex.Run(in, st); len(res.Deletions) == 0 || res.Failed != 0 {
		t.Errorf("positive control: the next run deleted %d, failed %d", len(res.Deletions), res.Failed)
	}
}
