package supervise

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/clock"
	"holocron/internal/ffmpeg"
)

const (
	randomSeed  = 20261020
	randomSteps = 5000
)

// A seeded random scenario (one run, about a second): reconciles, relay restarts, starts answered
// late or failing, natural exits, kills answered late, scans, blind gaps. Invariants: never two
// children (or a child and an unanswered start) for one folder; never a Start before NextStartAt;
// RestartCount never decreases; backoff 0 or within [BackoffStart, BackoffMax]; no Start while the
// relay hold applies; Start slots at least StartStagger apart.
func TestRandomInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(randomSeed, 1))
	s := defaultSettings()
	s.SegmentSeconds = 20 // stall limit 50 s, alert limit 935 s: both reached in the run
	r := newRig(t, s)
	names := []string{"cam1", "cam2", "cam3", "cam4", "cam5", "cam6"}
	ips := map[string]string{}
	reconcile := func() {
		var list []cameraT
		for _, n := range names {
			c := cam(n, rng.IntN(5) != 0)
			if rng.IntN(20) == 0 {
				ips[n] = fmt.Sprintf("192.0.2.%d", rng.IntN(200))
			}
			if ips[n] != "" {
				c.IP = ips[n]
			}
			list = append(list, c)
		}
		r.m.Reconcile(list)
	}
	type pendingStart struct {
		e  *Entry
		at clock.Mono
	}
	var startQ []pendingStart
	var killQ []*Entry
	running := map[*Entry]bool{}
	counts := map[*Entry]int64{}
	relayAlive, relayStart := true, r.clk.Mono()
	reasons := map[string]int{}
	var lastSlot clock.Mono
	slotSet := false
	reconcile()
	for step := 0; step < randomSteps; step++ {
		switch d := rng.IntN(100); {
		case d < 2:
			r.clk.Advance(time.Duration(10+rng.IntN(3000)) * time.Second) // blind gap
		case d < 4:
			r.clk.StepWall(time.Duration(rng.IntN(7200)-3600) * time.Second)
		default:
			r.clk.Advance(time.Duration(200+rng.IntN(1300)) * time.Millisecond)
		}
		now := r.clk.Mono()
		if rng.IntN(40) == 0 {
			reconcile()
		}
		if rng.IntN(50) == 0 {
			relayAlive = !relayAlive
			if relayAlive {
				relayStart = now
			}
		}
		// answer starts whose time has come (some fail)
		keep := startQ[:0]
		for _, p := range startQ {
			if p.at > now {
				keep = append(keep, p)
				continue
			}
			if rng.IntN(8) == 0 {
				r.m.StartFailed(p.e, errors.New("boom"))
			} else {
				r.m.Started(p.e, int64(step+1))
				running[p.e] = true
			}
		}
		startQ = keep
		// kills complete after a while; some children exit on their own
		var kq []*Entry
		for _, e := range killQ {
			if rng.IntN(3) == 0 {
				r.m.Exited(e, ffmpeg.ExitInfo{Code: -1})
				delete(running, e)
			} else {
				kq = append(kq, e)
			}
		}
		killQ = kq
		for e := range running {
			if e.pending == pendNone && rng.IntN(60) == 0 {
				r.m.Exited(e, ffmpeg.ExitInfo{Code: rng.IntN(3)})
				delete(running, e)
			}
		}
		if rng.IntN(5) == 0 {
			for _, n := range names {
				if rng.IntN(2) == 0 {
					r.scan.scans[r.dir(n)] = archive.DirScan{NewestClosedWrite: r.clk.Now().Add(-time.Duration(rng.IntN(4000)) * time.Second), Newest: fmt.Sprintf("%s-%d.mkv", n, rng.IntN(3)), Recent: rng.IntN(2) == 0}
				}
			}
			r.m.Refresh()
			r.m.Down()
			r.m.Rows(nil)
		}
		ready := fakeReady{}
		for _, n := range names {
			if relayAlive && rng.IntN(2) == 0 {
				ready[n] = true
			}
		}
		rv := RelayView{Ready: ready, Alive: relayAlive, StartedAt: relayStart}
		acts := r.m.Plan(rv)
		for _, a := range acts {
			e := a.Entry
			reasons[a.Kind.String()+" "+a.Reason]++
			switch a.Kind {
			case ActStart:
				slot := now + clock.Mono(a.Delay)
				if slot < e.nextMono {
					t.Fatalf("step %d: %s started before NextStartAt", step, e.safe)
				}
				if slotSet && slot.Sub(lastSlot) < s.StartStagger {
					t.Fatalf("step %d: start slots %v apart", step, slot.Sub(lastSlot))
				}
				lastSlot, slotSet = slot, true
				held := relayAlive && e.relayPath != "" && !ready.Has(e.relayPath) && now < relayStart+clock.Mono(s.RelayReadyWait)
				if held {
					t.Fatalf("step %d: %s started while held", step, e.safe)
				}
				startQ = append(startQ, pendingStart{e, slot})
			case ActKill:
				if !running[e] {
					t.Fatalf("step %d: kill for %s which is not running", step, e.safe)
				}
				killQ = append(killQ, e)
			}
		}
		// invariants over every entry
		perFolder := map[string]int{}
		for _, e := range r.m.all() {
			if e.running || e.pending == pendStart {
				perFolder[archive.FolderKey(e.safe)]++
			}
			if e.restartCount < counts[e] {
				t.Fatalf("step %d: RestartCount of %s went down", step, e.safe)
			}
			counts[e] = e.restartCount
			if e.backoff != 0 && (e.backoff < s.BackoffStart || e.backoff > s.BackoffMax) {
				t.Fatalf("step %d: backoff %d", step, e.backoff)
			}
		}
		for f, n := range perFolder {
			if n > 1 {
				t.Fatalf("step %d: %d children for %s", step, n, f)
			}
		}
	}
	var restarts int64
	for _, c := range counts {
		restarts += c
	}
	t.Logf("seed %d, %d steps, %d entries seen, %d restarts counted, %d alerts, actions %v", randomSeed, randomSteps, len(counts), restarts, len(r.alerts.events), reasons)
	for _, k := range []string{"kill stop", "kill stalled", "kill moving to the relay", "start start", "start " + ReasonHoldExpired} {
		if reasons[k] == 0 {
			t.Errorf("the scenario never produced %q", k)
		}
	}
}
