package web

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// decodeStatus builds the shape readStatus produces, so the tests exercise the
// same types the real path does - JSON numbers arrive as float64, which is the
// detail a hand-built map would hide.
func decodeStatus(t *testing.T, body string) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return parsed
}

const deadPid = 999001

// TestStaleRunningStatusIsNotRunning is the case State alone gets wrong: the
// NVR window was closed, so nothing wrote a shutdown, and the file still says
// running with a PID that has gone.
func TestStaleRunningStatusIsNotRunning(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.Local)
	status := decodeStatus(t, `{
		"State": "running",
		"StopKind": "",
		"SupervisorPid": 999001,
		"UpdatedAt": "2026-10-13T11:59:58+03:00"
	}`)
	// A fresh heartbeat on purpose: only the PID test can fail here.
	status["UpdatedAt"] = now.Add(-2 * time.Second).Format(time.RFC3339Nano)

	state := SupervisorStateOf(status, 5, now, func(int) bool { return false })

	if state.Running {
		t.Fatalf("a running status with a dead pid must not report running: %+v", state)
	}
	if state.PidAlive {
		t.Error("PidAlive must be false")
	}
	if state.State != "running" {
		t.Errorf("State should still report what the file said, got %q", state.State)
	}
	if !strings.Contains(state.Reason, "999001") || !strings.Contains(state.Reason, "no longer running") {
		t.Errorf("the reason must name the dead pid, got %q", state.Reason)
	}
}

func TestSupervisorStateOf(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.Local)
	fresh := now.Add(-3 * time.Second).Format(time.RFC3339Nano)
	oldHeartbeat := now.Add(-5 * time.Minute).Format(time.RFC3339Nano)

	cases := []struct {
		name        string
		status      string
		alive       bool
		wantRunning bool
		wantReason  string
	}{
		{
			name:        "fresh heartbeat and a live pid",
			status:      `{"State":"running","SupervisorPid":4242,"UpdatedAt":"` + fresh + `"}`,
			alive:       true,
			wantRunning: true,
			wantReason:  "pid 4242",
		},
		{
			name:        "live pid but the heartbeat stopped",
			status:      `{"State":"running","SupervisorPid":4242,"UpdatedAt":"` + oldHeartbeat + `"}`,
			alive:       true,
			wantRunning: false,
			wantReason:  "heartbeat is 300.0s old (limit 15s)",
		},
		{
			name:        "a clean shutdown",
			status:      `{"State":"stopped","StopKind":"requested","SupervisorPid":4242,"UpdatedAt":"` + fresh + `"}`,
			alive:       false,
			wantRunning: false,
			wantReason:  "clean shutdown",
		},
		{
			name:        "an unexpected stop says so",
			status:      `{"State":"stopped","StopKind":"unexpected","SupervisorPid":4242,"UpdatedAt":"` + fresh + `"}`,
			alive:       false,
			wantRunning: false,
			wantReason:  "stopped (unexpected)",
		},
		{
			name:        "no pid recorded",
			status:      `{"State":"running","UpdatedAt":"` + fresh + `"}`,
			alive:       true,
			wantRunning: false,
			wantReason:  "process 0 is no longer running",
		},
		{
			name:        "no usable heartbeat",
			status:      `{"State":"running","SupervisorPid":4242,"UpdatedAt":""}`,
			alive:       true,
			wantRunning: false,
			wantReason:  "no usable heartbeat",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			state := SupervisorStateOf(decodeStatus(t, test.status), 5, now, func(int) bool { return test.alive })
			if state.Running != test.wantRunning {
				t.Errorf("Running = %v, want %v (reason %q)", state.Running, test.wantRunning, state.Reason)
			}
			if !strings.Contains(state.Reason, test.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", state.Reason, test.wantReason)
			}
		})
	}
}

func TestSupervisorStateWithNoStatusFile(t *testing.T) {
	state := SupervisorStateOf(nil, 5, time.Now(), func(int) bool { return true })
	if state.Running {
		t.Error("no status file cannot mean running")
	}
	if state.HeartbeatSeconds != nil {
		t.Error("there is no heartbeat to report")
	}
	if state.HeartbeatLimitSeconds != minHeartbeatLimitSeconds {
		t.Errorf("limit = %v, want the %v floor", state.HeartbeatLimitSeconds, minHeartbeatLimitSeconds)
	}
}

// TestHeartbeatLimitFollowsTheStatusInterval checks the limit tracks the
// recorder's configured cadence, as Get-CamSupervisorState does, instead of
// being a constant the two implementations could drift apart on.
func TestHeartbeatLimitFollowsTheStatusInterval(t *testing.T) {
	for _, test := range []struct {
		interval float64
		want     float64
	}{
		{interval: 5, want: 15},  // 3 x 5 is under the floor
		{interval: 2, want: 15},  // the floor
		{interval: 20, want: 60}, // 3 x 20
	} {
		state := SupervisorStateOf(map[string]any{}, test.interval, time.Now(), func(int) bool { return true })
		if state.HeartbeatLimitSeconds != test.want {
			t.Errorf("interval %v: limit = %v, want %v", test.interval, state.HeartbeatLimitSeconds, test.want)
		}
	}
}

// TestProcessAliveOnThisProcess is the one claim the injected prober cannot
// make: that the real probe answers truthfully. Whatever platform the tests run
// on, this process is alive and a PID that high is not in use.
func TestProcessAliveOnThisProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("processAlive says this very process is not running")
	}
	if processAlive(deadPid) {
		t.Skip("pid " + strconv.Itoa(deadPid) + " happens to exist on this machine; cannot test the negative")
	}
	if processAlive(0) || processAlive(-1) {
		t.Error("a pid of 0 or less cannot be alive")
	}
}
