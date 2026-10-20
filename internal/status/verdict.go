package status

import (
	"fmt"
	"time"
)

const (
	defaultStatusIntervalSeconds = 5.0
	minHeartbeatLimitSeconds     = 15.0
	defaultStartTolerance        = 30 * time.Second
)

// ProcessInspector answers "is this pid a live process, and when did it start".
// A zero startTime with ok true means the start time is unknown (for example the
// process exists but cannot be queried); Verdict then skips the start-time check.
type ProcessInspector interface {
	Alive(pid int64) (startTime time.Time, ok bool)
}

// VerdictConfig carries the inputs the verdict needs besides the document.
type VerdictConfig struct {
	// StatusIntervalSeconds from the recorder config; <= 0 means 5.
	StatusIntervalSeconds float64
	// StartTolerance is how far the process start time may differ from
	// StartedAt; 0 means 30 s, negative disables the start-time check.
	StartTolerance time.Duration
}

// HeartbeatLimit is max(3 x StatusIntervalSeconds, 15 s).
func (c VerdictConfig) HeartbeatLimit() time.Duration {
	iv := c.StatusIntervalSeconds
	if iv <= 0 {
		iv = defaultStatusIntervalSeconds
	}
	limit := iv * 3
	if limit < minHeartbeatLimitSeconds {
		limit = minHeartbeatLimitSeconds
	}
	return time.Duration(limit * float64(time.Second))
}

// Verdict decides whether the supervisor behind snapshot is running. Both
// signals must agree: a heartbeat (UpdatedAt) no older than the limit, AND the
// recorded SupervisorPid alive with a start time matching StartedAt. The reasons
// mirror Get-CamSupervisorState; the stop-kind wording for non-requested stops
// follows ui/supervisor.go (finding F2). A heartbeat further in the future than
// the limit is not trusted (clock stepped); the next heartbeat repairs it.
func Verdict(snapshot *Document, now time.Time, procs ProcessInspector, cfg VerdictConfig) (running bool, reason string) {
	if snapshot == nil {
		return false, "no status file has been written yet"
	}
	updated := snapshot.UpdatedAt.Time()
	updatedText := ""
	if !snapshot.UpdatedAt.IsNull() {
		updatedText = updated.Format(TimeLayout)
	}

	if snapshot.State != StateRunning {
		if snapshot.StopKind != "" && snapshot.StopKind != StopKindRequested {
			return false, fmt.Sprintf("the supervisor stopped (%s) at %s", snapshot.StopKind, updatedText)
		}
		return false, fmt.Sprintf("the supervisor recorded a clean shutdown at %s", updatedText)
	}

	if snapshot.UpdatedAt.IsNull() {
		return false, "the status file carries no usable heartbeat"
	}
	limit := cfg.HeartbeatLimit()
	age := now.Sub(updated)
	rounded := float64(age.Round(100*time.Millisecond)) / float64(time.Second)
	if age > limit {
		return false, fmt.Sprintf("the heartbeat is %.1fs old (limit %ds)", rounded, int(limit.Seconds()))
	}
	if age < -limit {
		return false, fmt.Sprintf("the heartbeat is %.1fs in the future (clock step?)", -rounded)
	}

	pid := snapshot.SupervisorPid
	var start time.Time
	alive := false
	if pid > 0 {
		start, alive = procs.Alive(pid)
	}
	if !alive {
		return false, fmt.Sprintf("process %d is no longer running", pid)
	}

	tol := cfg.StartTolerance
	if tol == 0 {
		tol = defaultStartTolerance
	}
	if tol > 0 && !start.IsZero() && !snapshot.StartedAt.IsNull() {
		diff := start.Sub(snapshot.StartedAt.Time())
		if diff < 0 {
			diff = -diff
		}
		if diff > tol {
			return false, fmt.Sprintf("process %d is not the recorded supervisor (started %s, the status says %s): pid reused",
				pid, start.Format(TimeLayout), snapshot.StartedAt.Time().Format(TimeLayout))
		}
	}
	return true, fmt.Sprintf("pid %d, heartbeat %.1fs old", pid, rounded)
}

// FakeProcesses is a ProcessInspector for tests: pid -> start time.
type FakeProcesses map[int64]time.Time

func (f FakeProcesses) Alive(pid int64) (time.Time, bool) {
	t, ok := f[pid]
	return t, ok
}
