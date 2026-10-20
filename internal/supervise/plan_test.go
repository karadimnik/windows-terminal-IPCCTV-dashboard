package supervise

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"holocron/internal/clock"
	"holocron/internal/ffmpeg"
	"holocron/internal/relay"
)

func (r *rig) startAll(acts []Action) {
	for _, a := range acts {
		if a.Kind == ActStart {
			r.nextID++
			r.m.Started(a.Entry, r.nextID)
		}
	}
}

func (r *rig) exit(e *Entry, code int) Cleanup {
	return r.m.Exited(e, ffmpeg.ExitInfo{Code: code})
}

func starts(acts []Action) []Action {
	var out []Action
	for _, a := range acts {
		if a.Kind == ActStart {
			out = append(out, a)
		}
	}
	return out
}

func kills(acts []Action) []Action {
	var out []Action
	for _, a := range acts {
		if a.Kind == ActKill {
			out = append(out, a)
		}
	}
	return out
}

func describe(acts []Action) string {
	var p []string
	for _, a := range acts {
		s := a.Kind.String() + ":" + a.Entry.SafeName()
		if a.Kind == ActStart {
			s += fmt.Sprintf("/%s/%dms", a.Input, a.Delay.Milliseconds())
		} else {
			s += "/" + a.Reason
		}
		p = append(p, s)
	}
	return strings.Join(p, ",")
}

var six = []string{"cam1", "cam2", "cam3", "cam4", "cam5", "cam6"}

func cams(names []string) []cameraT {
	var out []cameraT
	for _, n := range names {
		out = append(out, cam(n, true))
	}
	return out
}

// A due child gets exactly one Start until it is answered (pending), new entries are due at once (R:3693-3696).
func TestPlanStartOnce(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams([]string{"cam1"}))
	if got := describe(r.m.Plan(RelayView{})); got != "start:cam1/direct/0ms" {
		t.Fatalf("plan = %s", got)
	}
	r.clk.Advance(time.Second)
	if got := r.m.Plan(RelayView{}); len(got) != 0 {
		t.Fatalf("a pending start was handed out again: %s", describe(got))
	}
}

// R:4041-4045, check inputs (tests/cam.tests.ps1:209-218): six simultaneous starts are 300 ms apart,
// the sixth 1500 ms in (5 x 300 <= 3000), as a non-blocking schedule in name order.
func TestPlanStaggerSix(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams([]string{"cam6", "cam3", "cam1", "cam5", "cam2", "cam4"}))
	got := describe(r.m.Plan(RelayView{}))
	want := "start:cam1/direct/0ms,start:cam2/direct/300ms,start:cam3/direct/600ms,start:cam4/direct/900ms,start:cam5/direct/1200ms,start:cam6/direct/1500ms"
	if got != want {
		t.Fatalf("plan = %s", got)
	}
	// negative control: StartStaggerMs 0 puts them all at 0 ms
	s := defaultSettings()
	s.StartStagger = 0
	r0 := newRig(t, s)
	r0.m.Reconcile(cams(six))
	if g := describe(r0.m.Plan(RelayView{})); g == want || strings.Contains(g, "300ms") {
		t.Fatalf("negative control: %s", g)
	}
}

// The spacing holds across Plan calls, and the earlier-due camera goes first.
func TestPlanStaggerAcrossCallsAndOrder(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams([]string{"a", "b"}))
	acts := r.m.Plan(RelayView{})
	r.startAll(acts)
	r.clk.Advance(100 * time.Millisecond)
	r.m.Reconcile(cams([]string{"a", "b", "c"}))
	if got := describe(r.m.Plan(RelayView{})); got != "start:c/direct/500ms" {
		t.Fatalf("plan = %s (b was slotted at +300 ms, c must follow 300 ms later)", got)
	}
	// earliest due first: z became due before y
	r2 := newRig(t, defaultSettings())
	r2.m.Reconcile(cams([]string{"y", "z"}))
	y, z := r2.m.Entry("y"), r2.m.Entry("z")
	y.nextMono, z.nextMono = clock.Mono(2*time.Second), clock.Mono(time.Second)
	r2.clk.Advance(3 * time.Second)
	if got := describe(r2.m.Plan(RelayView{})); got != "start:z/direct/0ms,start:y/direct/300ms" {
		t.Fatalf("plan = %s", got)
	}
}

// Never a Start before NextStartAt.
func TestPlanWaitsForBackoff(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams([]string{"a"}))
	r.startAll(r.m.Plan(RelayView{}))
	e := r.m.Entry("a")
	r.clk.Advance(time.Second)
	r.exit(e, 1) // backoff 3 s
	for i := 0; i < 2; i++ {
		r.clk.Advance(time.Second)
		if acts := r.m.Plan(RelayView{}); len(acts) != 0 {
			t.Fatalf("started %v after the exit, backoff 3 s", time.Duration(i+1)*time.Second)
		}
	}
	r.clk.Advance(time.Second)
	if len(starts(r.m.Plan(RelayView{}))) != 1 {
		t.Fatal("not started when the backoff ran out")
	}
}

// IR 3.3, R:4022-4036: while the relay is younger than RelayReadyWaitSeconds (15) and the path is
// not ready, the start is held (still held at 14 s, started by 16 s: the deadline test is strict,
// so the start comes at 15 s); after the wait it starts direct and logs once.
func TestPlanRelayHold(t *testing.T) {
	for _, c := range []struct {
		wait    time.Duration
		startAt time.Duration
	}{
		{15 * time.Second, 15 * time.Second},
		{20 * time.Second, 20 * time.Second}, // negative control: the constant decides (still held at 16 s)
	} {
		s := defaultSettings()
		s.RelayReadyWait = c.wait
		r := newRig(t, s)
		relayStart := r.clk.Mono()
		r.m.Reconcile(cams([]string{"cam1"}))
		rv := RelayView{Ready: fakeReady{}, Alive: true, StartedAt: relayStart}
		var startedAt time.Duration
		var start Action
		for el := time.Duration(0); el <= 30*time.Second; el += time.Second {
			if acts := r.m.Plan(rv); len(acts) > 0 && startedAt == 0 {
				startedAt, start = el, acts[0]
				r.startAll(acts)
			}
			r.clk.Advance(time.Second)
		}
		if startedAt != c.startAt || start.Input != InputDirect || start.Reason != ReasonHoldExpired {
			t.Fatalf("wait %v: started at %v (%s, %q), want %v", c.wait, startedAt, start.Input, start.Reason, c.startAt)
		}
		if c.wait == 15*time.Second && (startedAt <= 14*time.Second || startedAt > 16*time.Second) {
			t.Fatal("not held at 14 s or not started by 16 s")
		}
		line := fmt.Sprintf("cam1: the restreamer did not serve 'cam1' within %ds, starting it on the direct input", int(c.wait/time.Second))
		if n := strings.Count(r.logs.String(), line); n != 1 {
			t.Fatalf("hold-expired line %d times", n)
		}
	}
}

// No hold when the path is ready (relay at once), when the relay process is not alive, or when the
// relay is old (R:3926-3929).
func TestPlanNoHold(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams([]string{"cam1", "cam2"}))
	rv := RelayView{Ready: fakeReady{"cam1": true}, Alive: true, StartedAt: r.clk.Mono()}
	if got := describe(r.m.Plan(rv)); got != "start:cam1/relay/0ms" {
		t.Fatalf("young relay: %s", got)
	}
	r2 := newRig(t, defaultSettings())
	r2.m.Reconcile(cams([]string{"cam1"}))
	if got := describe(r2.m.Plan(RelayView{Ready: fakeReady{}, Alive: false})); got != "start:cam1/direct/0ms" {
		t.Fatalf("dead relay: %s", got)
	}
}

// Check inputs (tests/cam.tests.ps1:173-207): one shared set decides every camera; a path not in
// the set, an empty set and a camera without a relay path go direct; RestreamerEnabled false never
// gives the relay; "denied" counts as ready (IR 3.3).
func TestPlanInputChoice(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams(six))
	r.m.Entry("cam6").relayPath = ""
	set := fakeReady{"cam2": true, "cam4": true, "cam6": true}
	got := describe(r.m.Plan(RelayView{Ready: set}))
	want := "start:cam1/direct/0ms,start:cam2/relay/300ms,start:cam3/direct/600ms,start:cam4/relay/900ms,start:cam5/direct/1200ms,start:cam6/direct/1500ms"
	if got != want {
		t.Fatalf("batch: %s", got)
	}
	r2 := newRig(t, defaultSettings())
	r2.m.Reconcile(cams([]string{"cam1", "cam2"}))
	if got := describe(r2.m.Plan(RelayView{Ready: relay.ReadySet{State: relay.Denied}, Alive: true})); got != "start:cam1/relay/0ms,start:cam2/relay/300ms" {
		t.Fatalf("denied: %s", got)
	}
	s := defaultSettings()
	s.RelayEnabled = false
	r3 := newRig(t, s)
	r3.m.Reconcile(cams([]string{"cam1"}))
	if got := describe(r3.m.Plan(RelayView{Ready: fakeReady{"cam1": true}, Alive: true, StartedAt: r3.clk.Mono()})); got != "start:cam1/direct/0ms" {
		t.Fatalf("relay off: %s", got)
	}
	r4 := newRig(t, defaultSettings())
	r4.m.Reconcile(cams([]string{"cam1"}))
	if got := describe(r4.m.Plan(RelayView{Ready: relay.ReadySet{State: relay.Unavailable}})); got != "start:cam1/direct/0ms" {
		t.Fatalf("unavailable: %s", got)
	}
}

// IR 3.3 "Move off direct" (R:3970-3979): a live direct child whose path is ready is killed and
// restarted on the relay at once, with no restart counted and the backoff untouched.
func TestPlanMoveOffDirect(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams([]string{"cam1", "cam2"}))
	r.startAll(r.m.Plan(RelayView{}))
	e := r.m.Entry("cam1")
	e.backoff = 12
	r.clk.Advance(time.Second)
	rv := RelayView{Ready: fakeReady{"cam1": true}, Alive: true, StartedAt: r.clk.Mono()}
	if got := describe(r.m.Plan(rv)); got != "kill:cam1/moving to the relay" {
		t.Fatalf("plan = %s", got)
	}
	r.exit(e, 1)
	if e.restartCount != 0 || len(e.restarts) != 0 || e.backoff != 12 || e.lastError != "" {
		t.Fatalf("move counted: count %d backoff %d LastError %q", e.restartCount, e.backoff, e.lastError)
	}
	if got := describe(r.m.Plan(rv)); got != "start:cam1/relay/0ms" {
		t.Fatalf("restart = %s", got)
	}
	// negative control: a child already on the relay is left alone
	r.startAll(starts(r.m.Plan(rv)))
	r.m.Started(e, 999)
	r.clk.Advance(time.Second)
	if acts := kills(r.m.Plan(RelayView{Ready: fakeReady{"cam1": true, "cam2": false}})); len(acts) != 0 {
		t.Fatalf("relay child killed: %s", describe(acts))
	}
}

// Reconcile stops (disable, IP change) are Kill "stop", never counted, and the new entry of the
// same folder starts only after the old child exited.
func TestPlanRetire(t *testing.T) {
	r := newRig(t, defaultSettings())
	c := cam("Yard", true)
	r.m.Reconcile([]cameraT{c})
	r.startAll(r.m.Plan(RelayView{}))
	old := r.m.Entry("Yard")
	c.IP = "192.0.2.77"
	r.m.Reconcile([]cameraT{c})
	r.clk.Advance(time.Second)
	if got := describe(r.m.Plan(RelayView{})); got != "kill:Yard/stop" {
		t.Fatalf("plan = %s (no start while the old child runs)", got)
	}
	r.clk.Advance(time.Second)
	if acts := r.m.Plan(RelayView{}); len(acts) != 0 {
		t.Fatalf("again: %s", describe(acts))
	}
	cl := r.exit(old, 1)
	if old.restartCount != 0 || cl.Dir != r.dir("Yard") || len(r.m.retiring) != 0 {
		t.Fatalf("retired exit counted %d / cleanup %+v", old.restartCount, cl)
	}
	if got := describe(r.m.Plan(RelayView{})); got != "start:Yard/direct/0ms" {
		t.Fatalf("new entry: %s", got)
	}
	r.m.Shutdown()
	acts := r.m.Plan(RelayView{})
	if got := describe(acts); got != "" {
		t.Fatalf("pending start at shutdown: %s", got)
	}
	// a start answered after Shutdown is stopped on the next Plan
	r.m.Started(r.m.retiring[0], 5)
	if got := describe(r.m.Plan(RelayView{})); got != "kill:Yard/stop" {
		t.Fatalf("late start after shutdown: %s", got)
	}
}

// Shutdown: every running child gets Kill "stop" and nothing starts again.
func TestShutdown(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams([]string{"a", "b"}))
	r.startAll(r.m.Plan(RelayView{}))
	r.m.Shutdown()
	if got := describe(r.m.Plan(RelayView{})); got != "kill:a/stop,kill:b/stop" {
		t.Fatalf("plan = %s", got)
	}
	for _, e := range append([]*Entry(nil), r.m.retiring...) {
		r.exit(e, 1)
	}
	r.clk.Advance(time.Minute)
	if acts := r.m.Plan(RelayView{}); len(acts) != 0 || r.m.Active() != 0 {
		t.Fatalf("after shutdown: %s active %d", describe(acts), r.m.Active())
	}
	r.m.Reconcile(cams([]string{"a"}))
	if len(r.m.entries) != 0 {
		t.Fatal("reconcile after shutdown")
	}
}

// LLD 2.4 determinism: the plan does not depend on the order cameras were inserted.
func TestPlanDeterministic(t *testing.T) {
	orders := [][]string{six, {"cam4", "cam2", "cam6", "cam1", "cam5", "cam3"}, {"cam6", "cam5", "cam4", "cam3", "cam2", "cam1"}}
	var first string
	for i, o := range orders {
		r := newRig(t, defaultSettings())
		r.m.Reconcile(cams(o))
		got := describe(r.m.Plan(RelayView{Ready: fakeReady{"cam3": true}}))
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("order %v: %s, want %s", o, got, first)
		}
	}
}
