package host

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"holocron/internal/osx"
	"holocron/internal/status"
)

// G4 + G5 for real: the supervisor process is killed hard; its children die with it at once
// (before the next life starts), the host records the stop, raises 100, restarts it after 5 s,
// raises 101 when it runs again; a stop request then ends everything, no restart, nothing left.
func TestRealHostRestartsAfterHardKill(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "_state")
	h, scope, err := newHost(dir, state, Service, filepath.Join(dir, "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	k := &tracker{}
	defer k.sweep(t)
	defer func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "host.log"))
			t.Logf("host.log:\n%s", b)
		}
	}()
	done := make(chan Result, 1)
	go func() { r, _ := h.Run(context.Background()); done <- r }()

	var p1 int
	waitFor(t, "the first supervisor running with 2 children", 15*time.Second, func() bool {
		d := readDoc(state)
		if d == nil || d.State != status.StateRunning {
			return false
		}
		p1 = int(d.SupervisorPid)
		return len(supChildren(state)[p1]) == 2
	})
	kids1 := supChildren(state)[p1]
	k.add(append([]int{p1}, kids1...)...)
	t0 := time.Now()
	if err := (osx.OSInspector{}).Stop(k.e[p1]); err != nil {
		t.Fatal(err)
	}
	if a := k.gone(t, 3*time.Second, append([]int{p1}, kids1...)...); len(a) != 0 {
		t.Fatalf("still alive after the hard kill: %v", a)
	}
	goneAfter := time.Since(t0)
	if d := readDoc(state); d != nil && d.State == status.StateRunning && int(d.SupervisorPid) != p1 {
		t.Fatalf("a second life (pid %d) was running before the first life's children were gone", d.SupervisorPid)
	}
	t.Logf("supervisor %d and children %v gone %v after the kill", p1, kids1, goneAfter)

	sawHostRecord := false
	var p2 int
	waitFor(t, "the restarted supervisor", 20*time.Second, func() bool {
		d := readDoc(state)
		if d == nil {
			return false
		}
		if d.StopKind == status.StopKindUnexpected && d.LastUnexpectedStop.Stop != nil && d.LastUnexpectedStop.Stop.DetectedBy == "nvr host" && int(d.LastUnexpectedStop.Stop.SupervisorPid) == p1 {
			sawHostRecord = true
		}
		if d.State == status.StateRunning && int(d.SupervisorPid) != p1 {
			p2 = int(d.SupervisorPid)
			return true
		}
		return false
	})
	restartAfter := time.Since(t0)
	if restartAfter < 5*time.Second {
		t.Errorf("restarted %v after the kill, want >= 5 s", restartAfter)
	}
	if !sawHostRecord {
		t.Error("never saw the host's unexpected-stop document")
	}
	waitFor(t, "alerts 100 and 101", 10*time.Second, func() bool { return len(readAlerts(filepath.Join(dir, "alerts.txt"))) >= 2 })
	if got := readAlerts(filepath.Join(dir, "alerts.txt")); !reflect.DeepEqual(got, []int{100, 101}) {
		t.Errorf("alerts %v, want [100 101]", got)
	}
	waitFor(t, "second life's children", 10*time.Second, func() bool { return len(supChildren(state)[p2]) == 2 })
	kids2 := supChildren(state)[p2]
	k.add(append([]int{p2}, kids2...)...)
	t.Logf("restarted as pid %d with children %v, %v after the kill", p2, kids2, restartAfter)

	os.WriteFile(filepath.Join(state, "stop.request"), []byte("test"), 0o600)
	select {
	case r := <-done:
		if r.Outcome != OutcomeRequested || r.Lives != 2 || r.Restarts != 1 {
			t.Errorf("result %+v", r)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the host did not end after the stop request")
	}
	if a := k.gone(t, 3*time.Second, k.all()...); len(a) != 0 {
		t.Errorf("left after the stop: %v", a)
	}
	if got := readAlerts(filepath.Join(dir, "alerts.txt")); len(got) != 2 {
		t.Errorf("the orderly stop raised an alert: %v", got)
	}
	if d := readDoc(state); d == nil || d.StopKind != status.StopKindRequested {
		t.Errorf("final document %+v", d)
	}
}

// G8 for real: a lock already held makes the supervisor exit 20; the host does not restart it.
func TestRealHostAnotherSupervisorNotRestarted(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "_state")
	lk, err := osx.AcquireInstanceLock(osx.LockPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	h, scope, err := newHost(dir, state, Service, filepath.Join(dir, "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	res, _ := h.Run(context.Background())
	if res.Outcome != OutcomeAnother || res.Lives != 1 || res.ExitCode() != 20 {
		t.Errorf("result %+v", res)
	}
	if a := readAlerts(filepath.Join(dir, "alerts.txt")); len(a) != 0 {
		t.Errorf("alerts %v", a)
	}
}

// startHostHelper runs the host as its own process.
func startHostHelper(t *testing.T, dir, state string, attr func(*exec.Cmd), env ...string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = selfEnv("host", append([]string{"HOLOCRON_HOST_DIR=" + dir, "HOLOCRON_HOST_STATE=" + state}, env...)...)
	attr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "host.log"))
			t.Logf("host.log:\n%s", b)
		}
	})
	return cmd, done
}

// waitRunning waits for the helper host's supervisor and its two children and tracks them.
func waitRunning(t *testing.T, state string, k *tracker) (sup int, kids []int) {
	t.Helper()
	waitFor(t, "the supervisor running with 2 children", 15*time.Second, func() bool {
		d := readDoc(state)
		if d == nil || d.State != status.StateRunning {
			return false
		}
		sup = int(d.SupervisorPid)
		return len(supChildren(state)[sup]) == 2
	})
	kids = supChildren(state)[sup]
	k.add(append([]int{sup}, kids...)...)
	return sup, kids
}

// G5 through the whole chain: the HOST is killed hard; the supervisor (in the host's scope) and its
// children (in the supervisor's) all die.
func TestRealHostHardKillTakesEverything(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "_state")
	cmd, _ := startHostHelper(t, dir, state, hideAttr, "HOLOCRON_HOST_MODE=service")
	k := &tracker{}
	defer k.sweep(t)
	sup, kids := waitRunning(t, state, k)
	t0 := time.Now()
	cmd.Process.Kill()
	if a := k.gone(t, 3*time.Second, k.all()...); len(a) != 0 {
		t.Fatalf("alive after the host's hard kill: %v (supervisor %d, children %v)", a, sup, kids)
	}
	t.Logf("supervisor %d and children %v gone %v after the host was killed", sup, kids, time.Since(t0))
}

// checkWindowClosed asserts the end state of an attached close: host exit 0 with window-closed,
// one 103, StopKind window-closed on disk (valid), no second life, nothing left.
func checkWindowClosed(t *testing.T, dir, state string, cmd *exec.Cmd, done <-chan struct{}, k *tracker, within time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("the host did not exit within %v", within)
	}
	var r map[string]any
	b, _ := os.ReadFile(filepath.Join(dir, "host-result.json"))
	json.Unmarshal(b, &r)
	if r["outcome"] != string(OutcomeWindowClosed) || r["lives"] != float64(1) {
		t.Errorf("host result %v", r)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("host exit code %d", code)
	}
	if a := readAlerts(filepath.Join(dir, "alerts.txt")); !reflect.DeepEqual(a, []int{AlertWindowClosed}) {
		t.Errorf("alerts %v, want [103]", a)
	}
	raw, _ := os.ReadFile(filepath.Join(state, "status.json"))
	if v, err := status.Validate(raw); err != nil || len(v) > 0 {
		t.Errorf("status.json invalid: %v %v", err, v)
	}
	if d := readDoc(state); d == nil || d.StopKind != status.StopKindWindowClosed || d.LastWindowClose == nil || d.LastWindowClose.DetectedBy != "nvr host" {
		t.Errorf("document %+v", d)
	}
	if a := k.gone(t, 3*time.Second, k.all()...); len(a) != 0 {
		t.Errorf("left after the close: %v", a)
	}
}
