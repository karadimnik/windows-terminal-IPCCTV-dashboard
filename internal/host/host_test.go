package host

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"holocron/internal/osx"
	"holocron/internal/status"
)

func stoppedDoc(pid int, kind string) *status.Document {
	d := &status.Document{Schema: 1, State: status.StateStopped, StopKind: kind, SupervisorPid: int64(pid)}
	if kind == status.StopKindUnexpected {
		d.LastUnexpectedStop = status.MaybeStop{Present: true, Stop: &status.UnexpectedStop{Reason: "panic", DetectedBy: "supervisor"}}
	}
	return d
}

// The classification uses recorded state only; the pairs that must NOT restart and the ones that
// must (an exit code alone never decides, except 20 and 21, which are records).
func TestClassifyFromRecordedStateOnly(t *testing.T) {
	running := &status.Document{Schema: 1, State: status.StateRunning, SupervisorPid: 7}
	cases := []struct {
		name     string
		code     int
		stopFile bool
		doc      *status.Document
		want     stopKind
		recorded bool
	}{
		{"stop file, killed during the stop", 1, true, running, stopRequested, false},
		{"stop file wins over exit 20", 20, true, nil, stopRequested, false},
		{"own document requested", 0, false, stoppedDoc(7, status.StopKindRequested), stopRequested, false},
		{"own document interrupted", 0, false, stoppedDoc(7, status.StopKindInterrupted), stopInterrupted, false},
		{"own document window-closed", 0, false, stoppedDoc(7, status.StopKindWindowClosed), stopWindowClosed, false},
		{"exit 20 another supervisor", 20, false, nil, stopAnother, false},
		{"exit 21 configuration fault", 21, false, nil, stopConfigFault, false},
		{"own document unexpected (supervisor recorded it)", 1, false, stoppedDoc(7, status.StopKindUnexpected), stopUnexpected, true},
		{"killed: last document still running", 1, false, running, stopUnexpected, false},
		{"exit 0 with nothing recorded is NOT a clean stop", 0, false, running, stopUnexpected, false},
		{"exit 0, no document at all", 0, false, nil, stopUnexpected, false},
		{"stale requested of an EARLIER supervisor (IS 1.5 edge)", 0, false, stoppedDoc(6, status.StopKindRequested), stopUnexpected, false},
		{"stale window-closed of an earlier supervisor", 1, false, stoppedDoc(6, status.StopKindWindowClosed), stopUnexpected, false},
	}
	for _, c := range cases {
		got := classify(7, c.code, 90*time.Second, c.stopFile, c.doc)
		if got.kind != c.want || got.recorded != c.recorded {
			t.Errorf("%s: kind %d recorded %v, want %d %v (%s)", c.name, got.kind, got.recorded, c.want, c.recorded, got.reason)
		}
	}
	if r := classify(7, 1, 90*time.Second, false, stoppedDoc(7, status.StopKindUnexpected)).reason; !strings.HasSuffix(r, ": panic") {
		t.Errorf("recorded reason not carried: %q", r)
	}
}

// G2/G4: every stop that must not be repaired ends the host after ONE life, with no alert.
func TestHostNeverRestartsDeliberateOrRefusedStops(t *testing.T) {
	for _, c := range []struct {
		name string
		end  func(h *harness, l *fakeLife)
		want Outcome
	}{
		{"stop file", func(h *harness, l *fakeLife) { os.WriteFile(h.stopPath(), nil, 0o600); l.exit(1) }, OutcomeRequested},
		{"requested", func(h *harness, l *fakeLife) { l.stopped(status.StopKindRequested, 0) }, OutcomeRequested},
		{"interrupted", func(h *harness, l *fakeLife) { l.stopped(status.StopKindInterrupted, 0) }, OutcomeInterrupted},
		{"window-closed", func(h *harness, l *fakeLife) { l.stopped(status.StopKindWindowClosed, 0) }, OutcomeWindowClosed},
		{"exit 20", func(h *harness, l *fakeLife) { l.exit(20) }, OutcomeAnother},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, Service)
			h.start()
			l := h.life(1)
			l.running()
			c.end(h, l)
			res, err := h.wait()
			if err != nil || res.Outcome != c.want {
				t.Fatalf("outcome %v %v, want %v", res.Outcome, err, c.want)
			}
			h.eventuallyStep("a possible restart", time.Second, func() bool { return h.clk.Since(0) > 2*time.Minute })
			if n := h.nLives(); n != 1 {
				t.Errorf("%d lives, want 1 (no restart)", n)
			}
			if a := h.alerts.ids(); len(a) != 0 {
				t.Errorf("alerts %v, want none", a)
			}
		})
	}
}

// G4: a killed supervisor (nothing recorded) is recorded by the host, alerted (100), restarted in
// place after 5 s, and alerted again (101) once the new one reports running.
func TestHostRestartsKilledSupervisor(t *testing.T) {
	h := newHarness(t, Service)
	h.start()
	l1 := h.life(1)
	l1.running()
	h.clk.Advance(time.Minute)
	exitAt := h.clk.Now()
	l1.exit(1) // killed: its last document still says running
	l2 := h.life(2)
	// never before the 5 s delay; the harness advances the fake clock while the host does real file
	// I/O, so the upper bound is loose
	if gap := l2.spawnedAt.Sub(exitAt); gap < 5*time.Second || gap > 10*time.Second {
		t.Errorf("restart after %v, want 5 s", gap)
	}
	if got := h.alerts.ids(); !reflect.DeepEqual(got, []int{AlertStopped}) {
		t.Fatalf("alerts at the restart %v, want [100]", got)
	}
	d := h.doc()
	if d.State != status.StateStopped || d.StopKind != status.StopKindUnexpected || d.LastUnexpectedStop.Stop == nil ||
		d.LastUnexpectedStop.Stop.DetectedBy != "nvr host" || d.LastUnexpectedStop.Stop.SupervisorPid != int64(l1.pid) {
		t.Fatalf("host document: %+v %+v", d, d.LastUnexpectedStop.Stop)
	}
	if !strings.Contains(d.LastUnexpectedStop.Stop.Reason, fmt.Sprintf("pid %d", l1.pid)) {
		t.Errorf("reason %q", d.LastUnexpectedStop.Stop.Reason)
	}
	if got, want := h.validDoc(), goldenKeys(t, "patched-unexpected-over-running.json"); !reflect.DeepEqual(got, want) {
		t.Errorf("keys %v, want the PowerShell patch's %v", got, want)
	}
	l2.running()
	h.eventually("alert 101", func() bool { return len(h.alerts.ids()) == 2 })
	if got := h.alerts.ids(); !reflect.DeepEqual(got, []int{AlertStopped, AlertRecording}) {
		t.Fatalf("alerts %v", got)
	}
	ev := h.alerts.all()
	if ev[0].Severity != "Error" || ev[1].Severity != "Warning" || !strings.Contains(ev[1].Message, "restart 1 of 5") {
		t.Errorf("alerts %+v", ev)
	}
	l2.stopped(status.StopKindRequested, 0)
	if res, _ := h.wait(); res.Outcome != OutcomeRequested || res.Restarts != 1 || res.Lives != 2 {
		t.Errorf("result %+v", res)
	}
	if len(h.alerts.ids()) != 2 {
		t.Errorf("the orderly stop raised an alert: %v", h.alerts.ids())
	}
}

// The supervisor recorded its own fault: the host keeps that record (no second writer).
func TestHostKeepsSupervisorsOwnRecord(t *testing.T) {
	h := newHarness(t, Service)
	h.start()
	l1 := h.life(1)
	l1.stopped(status.StopKindUnexpected, 1)
	h.life(2)
	if d := h.doc(); d.LastUnexpectedStop.Stop.DetectedBy != "supervisor" {
		t.Errorf("the host overwrote the supervisor's record: %+v", d.LastUnexpectedStop.Stop)
	}
	if ev := h.alerts.all(); len(ev) != 1 || !strings.Contains(ev[0].Message, "panic in the loop") {
		t.Errorf("alert 100 does not carry the recorded reason: %+v", ev)
	}
}

func TestHostUnexpectedMinimalDocumentValidates(t *testing.T) {
	h := newHarness(t, Service)
	h.start()
	h.life(1).exit(1) // died before its first status write
	h.life(2)
	if got, want := h.validDoc(), goldenKeys(t, "patched-unexpected-minimal.json"); !reflect.DeepEqual(got, want) {
		t.Errorf("keys %v, want %v", got, want)
	}
}

// G4 budget: 5 restarts in 30 min; the 6th crash raises 102 and the host stays down.
func TestHostBudgetSpentAtSixthCrash(t *testing.T) {
	h := newHarness(t, Service)
	h.onSpawn = func(l *fakeLife) { l.running() }
	h.start()
	for i := 1; i <= 6; i++ {
		l := h.life(i)
		h.eventually("running", func() bool {
			d, _ := status.Read(h.statusPath())
			return d != nil && d.SupervisorPid == int64(l.pid) && i > 1 && len(h.alerts.ids()) >= 2*(i-1) || i == 1
		})
		h.clk.Advance(30 * time.Second)
		l.exit(1)
	}
	res, err := h.wait()
	if err != nil || res.Outcome != OutcomeBudgetSpent || res.Lives != 6 || res.Restarts != 5 {
		t.Fatalf("result %+v %v", res, err)
	}
	ids := h.alerts.ids()
	want := []int{100, 101, 100, 101, 100, 101, 100, 101, 100, 101, 102}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("alerts %v, want %v", ids, want)
	}
	h.eventuallyStep("a possible restart", time.Second, func() bool { return h.clk.Since(0) > 20*time.Minute })
	if h.nLives() != 6 {
		t.Errorf("restarted after the budget was spent: %d lives", h.nLives())
	}
	if !strings.Contains(h.logs.String(), "ERROR") || !strings.Contains(h.logs.String(), "budget spent") {
		t.Errorf("no ERROR line for the spent budget:\n%s", h.logs.String())
	}
	if e := h.alerts.all()[10]; e.Severity != "Error" || !strings.Contains(e.Message, "restarted 5 times in 30 minutes") {
		t.Errorf("alert 102 %+v", e)
	}
}

// The window refills: restarts older than 30 min no longer count.
func TestHostBudgetRefillsAfterWindow(t *testing.T) {
	h := newHarness(t, Service)
	h.start()
	for i := 1; i <= 5; i++ {
		h.life(i).exit(1)
	}
	l6 := h.life(6) // 5 restarts used
	h.eventuallyStep("30 minutes", time.Minute, func() bool { return h.clk.Since(0) > 31*time.Minute })
	l6.exit(1)
	l7 := h.life(7)
	l7.stopped(status.StopKindRequested, 0)
	res, _ := h.wait()
	if res.Outcome != OutcomeRequested || res.Restarts != 6 {
		t.Errorf("result %+v", res)
	}
	for _, id := range h.alerts.ids() {
		if id == AlertBudgetSpent {
			t.Errorf("102 although the window had refilled: %v", h.alerts.ids())
		}
	}
	// negative control: the same sequence inside the window spends the budget (TestHostBudgetSpentAtSixthCrash)
}

// The host waits up to 20 s for the previous supervisor's lock; a lock that frees within that time
// is taken (the document written under it), one that does not means another supervisor.
func TestHostWaitsForPreviousLock(t *testing.T) {
	h := newHarness(t, Service)
	h.start()
	l1 := h.life(1)
	l1.running()
	h.lock.hold("pid 5000 (the dying supervisor)")
	exitAt := h.clk.Now()
	l1.exit(1)
	h.eventuallyStep("3 s of lock wait", 250*time.Millisecond, func() bool { return h.clk.Now().Sub(exitAt) >= 3*time.Second })
	if h.nLives() != 1 || len(h.alerts.ids()) != 0 {
		t.Fatalf("restarted or alerted while the lock was held")
	}
	if d := h.doc(); d.State != status.StateRunning {
		t.Fatalf("document written without the lock: %s", d.State)
	}
	h.lock.hold("")
	l2 := h.life(2)
	if l2.spawnedAt.Sub(exitAt) < 8*time.Second {
		t.Errorf("respawned %v after the exit, want lock wait + 5 s", l2.spawnedAt.Sub(exitAt))
	}
	if d := h.doc(); d.StopKind != status.StopKindUnexpected {
		t.Errorf("document %s/%s", d.State, d.StopKind)
	}
	l2.stopped(status.StopKindRequested, 0)
	h.wait()

	h2 := newHarness(t, Service)
	h2.start()
	m := h2.life(1)
	m.running()
	h2.lock.hold("pid 777 (another supervisor)")
	m.exit(1)
	res, _ := h2.wait()
	if res.Outcome != OutcomeAnother || !strings.Contains(res.Reason, "pid 777") || h2.nLives() != 1 || len(h2.alerts.ids()) != 0 {
		t.Errorf("held lock: %+v lives %d alerts %v", res, h2.nLives(), h2.alerts.ids())
	}
	if d := h2.doc(); d.State != status.StateRunning {
		t.Errorf("the host wrote status.json while another supervisor holds the lock")
	}
}

// G1/G2/G3 attached: a window close is an ordered stop through stop.request, StopKind
// window-closed, ONE alert 103, no restart.
func TestHostWindowCloseStopsOnceWithoutRestart(t *testing.T) {
	for _, r := range []osx.Reason{osx.ReasonClose, osx.ReasonConsoleGone, osx.ReasonHangup, osx.ReasonLogoff} {
		t.Run(string(r), func(t *testing.T) {
			h := newHarness(t, Attached)
			h.onSpawn = func(l *fakeLife) { l.running(); l.honourStop() }
			h.start()
			l := h.life(1)
			h.eventually("console armed", h.console.armed)
			h.console.trigger(r)
			res, err := h.wait()
			if err != nil || res.Outcome != OutcomeWindowClosed {
				t.Fatalf("%+v %v", res, err)
			}
			if l.killed.Load() {
				t.Error("the supervisor was killed although it stopped in time")
			}
			d := h.doc()
			if d.StopKind != status.StopKindWindowClosed || d.State != status.StateStopped || d.LastWindowClose == nil || !strings.Contains(d.LastWindowClose.Reason, string(r)) {
				t.Fatalf("document %s/%s %+v", d.State, d.StopKind, d.LastWindowClose)
			}
			if got, want := h.validDoc(), goldenKeys(t, "patched-windowclose-over-running.json"); !reflect.DeepEqual(got, want) {
				t.Errorf("keys %v, want %v", got, want)
			}
			h.eventuallyStep("a possible restart", time.Second, func() bool { return h.clk.Since(0) > 2*time.Minute })
			if h.nLives() != 1 {
				t.Errorf("restarted after a window close")
			}
			if ids := h.alerts.ids(); !reflect.DeepEqual(ids, []int{AlertWindowClosed}) {
				t.Errorf("alerts %v, want exactly one 103", ids)
			}
			if _, err := os.Stat(h.stopPath()); err == nil {
				t.Error("stop.request left behind by a supervisor that honoured it")
			}
		})
	}
}

// A supervisor that does not stop within the console budget is killed (its job takes its children)
// so the window close still completes inside the 5 s the OS gives.
func TestHostWindowCloseKillsHungSupervisorAtBudget(t *testing.T) {
	h := newHarness(t, Attached)
	h.onSpawn = func(l *fakeLife) { l.running() }
	h.start()
	l := h.life(1)
	h.eventually("console armed", h.console.armed)
	h.console.trigger(osx.ReasonClose)
	// let the host write stop.request and arm its timer without moving virtual time (the harness
	// would otherwise advance the clock during that real file I/O)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if _, err := os.Stat(h.stopPath()); err == nil {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	t0 := h.clk.Now()
	res, _ := h.wait()
	if res.Outcome != OutcomeWindowClosed || !l.killed.Load() {
		t.Fatalf("%+v killed=%v", res, l.killed.Load())
	}
	// the kill comes when the wait part of the budget is spent, leaving the reserve for the document
	if at := time.Unix(0, l.killedAt.Load()).Sub(t0); at < osx.ConsoleBudget*7/8 || at > osx.ConsoleBudget {
		t.Errorf("killed %v after the close, want within the %v budget after its 7/8 wait", at, osx.ConsoleBudget)
	}
	if ids := h.alerts.ids(); !reflect.DeepEqual(ids, []int{AlertWindowClosed}) {
		t.Errorf("alerts %v", ids)
	}
}

// G3: an orderly stop (service manager stop) raises no alert, restarts nothing.
func TestHostOrderlyStopRaisesNoAlert(t *testing.T) {
	for _, r := range []osx.Reason{osx.ReasonTerminate, osx.ReasonClose, osx.ReasonInterrupt} {
		h := newHarness(t, Service)
		h.onSpawn = func(l *fakeLife) { l.running(); l.honourStop() }
		h.start()
		h.life(1)
		h.eventually("console armed", h.console.armed)
		h.console.trigger(r)
		res, err := h.wait()
		if err != nil || res.Outcome != OutcomeRequested || len(h.alerts.ids()) != 0 || h.nLives() != 1 {
			t.Errorf("%s: %+v %v alerts %v", r, res, err, h.alerts.ids())
		}
		if d := h.doc(); d.StopKind != status.StopKindRequested {
			t.Errorf("%s: StopKind %q", r, d.StopKind)
		}
	}
	// Ctrl+C in attached mode is an orderly stop too, not a window close.
	h := newHarness(t, Attached)
	h.onSpawn = func(l *fakeLife) { l.running(); l.honourStop() }
	h.start()
	h.life(1)
	h.eventually("console armed", h.console.armed)
	h.console.trigger(osx.ReasonInterrupt)
	if res, _ := h.wait(); res.Outcome != OutcomeRequested || len(h.alerts.ids()) != 0 {
		t.Errorf("attached Ctrl+C: %+v %v", res, h.alerts.ids())
	}
}

// G7: a service stop waits StopTimeout and verifies; it never kills the supervisor, reports DID NOT
// STOP and leaves the stop request in place.
func TestHostServiceStopWaitsAndNeverKills(t *testing.T) {
	h := newHarness(t, Service)
	h.onSpawn = func(l *fakeLife) { l.running() } // ignores the stop request
	h.start()
	l := h.life(1)
	h.eventually("console armed", h.console.armed)
	t0 := h.clk.Now()
	h.console.trigger(osx.ReasonTerminate)
	res, err := h.wait()
	if err == nil || res.Outcome != OutcomeStopFailed || res.ExitCode() != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if l.killed.Load() {
		t.Error("the host killed the supervisor")
	}
	if took := h.clk.Now().Sub(t0); took < 40*time.Second {
		t.Errorf("gave up after %v, want StopTimeout", took)
	}
	if _, err := os.Stat(h.stopPath()); err != nil {
		t.Error("the stop request was removed")
	}
	if !strings.Contains(h.logs.String(), "DID NOT STOP") {
		t.Error("no DID NOT STOP line")
	}
	l.exit(0)
}

// A window close while the supervisor is down between restarts: no restart, still one 103.
func TestHostWindowCloseDuringRestartDelay(t *testing.T) {
	h := newHarness(t, Attached)
	h.start()
	h.life(1).exit(1)
	h.eventually("alert 100", func() bool { return len(h.alerts.ids()) == 1 })
	h.console.trigger(osx.ReasonClose)
	res, _ := h.wait()
	if res.Outcome != OutcomeWindowClosed || h.nLives() != 1 {
		t.Fatalf("%+v lives %d", res, h.nLives())
	}
	if ids := h.alerts.ids(); !reflect.DeepEqual(ids, []int{AlertStopped, AlertWindowClosed}) {
		t.Errorf("alerts %v", ids)
	}
	if d := h.doc(); d.StopKind != status.StopKindWindowClosed || d.LastUnexpectedStop.Stop == nil {
		t.Errorf("document %s, unexpected record carried: %v", d.StopKind, d.LastUnexpectedStop.Stop != nil)
	}
	h.validDoc()
}

// After a restart, a configuration fault means it is not coming back: 102.
func TestHostConfigFaultAfterRestartRaises102(t *testing.T) {
	h := newHarness(t, Service)
	h.start()
	h.life(1).exit(1)
	h.life(2).exit(21)
	res, _ := h.wait()
	if res.Outcome != OutcomeConfigFault || res.ExitCode() != 21 {
		t.Fatalf("%+v", res)
	}
	if ids := h.alerts.ids(); !reflect.DeepEqual(ids, []int{AlertStopped, AlertBudgetSpent}) {
		t.Errorf("alerts %v", ids)
	}
}

// Exit 21 on the FIRST life is not restarted and raises 102 too: the NVR is down and stays down.
func TestHostConfigFaultFirstLifeRaises102(t *testing.T) {
	h := newHarness(t, Service)
	h.start()
	l := h.life(1)
	l.running()
	l.exit(21)
	res, err := h.wait()
	if err != nil || res.Outcome != OutcomeConfigFault || res.ExitCode() != 21 {
		t.Fatalf("%+v %v", res, err)
	}
	h.eventuallyStep("a possible restart", time.Second, func() bool { return h.clk.Since(0) > 2*time.Minute })
	if n := h.nLives(); n != 1 {
		t.Errorf("%d lives, want 1 (no restart)", n)
	}
	ev := h.alerts.all()
	if len(ev) != 1 || ev[0].ID != AlertBudgetSpent || ev[0].Severity != "Error" ||
		!strings.Contains(ev[0].Message, "configuration fault: the supervisor cannot start") || !strings.Contains(ev[0].Message, "stays down") {
		t.Errorf("alerts %+v", ev)
	}
}

func TestHostSpawnFailureCountsAgainstBudget(t *testing.T) {
	h := newHarness(t, Service)
	h.spawnErr = errors.New("no such file")
	h.start()
	res, _ := h.wait()
	if res.Outcome != OutcomeBudgetSpent || res.Lives != 6 {
		t.Fatalf("%+v", res)
	}
	if ids := h.alerts.ids(); len(ids) != 6 || ids[5] != AlertBudgetSpent {
		t.Errorf("alerts %v", ids)
	}
}

func TestHostProbeNotArmedIsLogged(t *testing.T) {
	h := newHarness(t, Attached)
	h.console.probeErr = osx.ErrNoConsole
	h.onSpawn = func(l *fakeLife) { l.stopped(status.StopKindRequested, 0) }
	h.start()
	h.wait()
	if !strings.Contains(h.logs.String(), "console probe not armed") {
		t.Errorf("log:\n%s", h.logs.String())
	}
}
