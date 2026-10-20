package web

import (
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Is a supervisor actually behind this snapshot?
//
// status.json's State field alone cannot answer that. Closing the NVR window
// runs no cleanup, and a supervisor killed outright writes nothing on its way
// out, so the last thing the file says can be "running" with a PID that no
// longer exists. A UI that trusts State reports six cameras recording into a
// dead process.
//
// This mirrors Get-CamSupervisorState in cam.common.ps1 - deliberately, so the
// page and the CLI cannot disagree about whether the recorder is up. Both
// signals must agree: the heartbeat has to be fresh AND the recorded process
// has to still exist. A heartbeat alone would believe a supervisor killed a
// second ago; a live PID alone would believe a PID the machine has reused.
// ---------------------------------------------------------------------------

// defaultStatusIntervalSeconds matches StatusIntervalSeconds in
// Get-CamConfigDefault. The heartbeat limit is three of those intervals.
const defaultStatusIntervalSeconds = 5.0

// minHeartbeatLimitSeconds is the floor Get-CamSupervisorState applies, so a
// short status interval cannot make the liveness test hair-triggered.
const minHeartbeatLimitSeconds = 15.0

// SupervisorState is the answer, with the reason it gives, so the page can say
// why rather than only that.
type SupervisorState struct {
	// Running is the only field the UI should branch on.
	Running bool `json:"running"`

	// Reason explains the verdict in the same words the CLI uses.
	Reason string `json:"reason"`

	// State is what status.json claimed, kept so the page can show the
	// difference between "it says running" and "it is running".
	State string `json:"state"`

	// StopKind is requested | interrupted | unexpected, empty while running.
	StopKind string `json:"stopKind"`

	Pid      int  `json:"pid"`
	PidAlive bool `json:"pidAlive"`

	HeartbeatSeconds      *float64 `json:"heartbeatSeconds"`
	HeartbeatLimitSeconds float64  `json:"heartbeatLimitSeconds"`
}

// SupervisorStateOf decides liveness from a decoded status.json.
//
// statusIntervalSeconds comes from recorder.config.json and sets the heartbeat
// limit; alive reports whether a PID is a live process and is injected so the
// decision can be tested without a real process.
func SupervisorStateOf(status map[string]any, statusIntervalSeconds float64, now time.Time, alive func(int) bool) SupervisorState {
	state := SupervisorState{Reason: "no status file has been written yet"}

	limit := statusIntervalSeconds * 3
	if limit < minHeartbeatLimitSeconds {
		limit = minHeartbeatLimitSeconds
	}
	state.HeartbeatLimitSeconds = limit

	if status == nil {
		return state
	}

	state.State = jsonString(status["State"])
	state.StopKind = jsonString(status["StopKind"])
	state.Pid = jsonInt(status["SupervisorPid"])

	updatedAt, haveUpdatedAt := parseStatusTime(jsonString(status["UpdatedAt"]))
	if haveUpdatedAt {
		age := roundTo(now.Sub(updatedAt).Seconds(), 1)
		state.HeartbeatSeconds = &age
	}

	if state.State != "running" {
		state.Reason = fmt.Sprintf("the supervisor recorded a clean shutdown at %s", jsonString(status["UpdatedAt"]))
		if state.StopKind != "" && state.StopKind != "requested" {
			state.Reason = fmt.Sprintf("the supervisor stopped (%s) at %s", state.StopKind, jsonString(status["UpdatedAt"]))
		}
		return state
	}

	if !haveUpdatedAt {
		state.Reason = "the status file carries no usable heartbeat"
		return state
	}
	if *state.HeartbeatSeconds > limit {
		state.Reason = fmt.Sprintf("the heartbeat is %.1fs old (limit %ds)", *state.HeartbeatSeconds, int(limit))
		return state
	}

	state.PidAlive = state.Pid > 0 && alive(state.Pid)
	if !state.PidAlive {
		// The case State alone gets wrong, and the one this exists for.
		state.Reason = fmt.Sprintf("the status file says running, but process %d is no longer running", state.Pid)
		return state
	}

	state.Running = true
	state.Reason = fmt.Sprintf("pid %d, heartbeat %.1fs old", state.Pid, *state.HeartbeatSeconds)
	return state
}

// parseStatusTime reads the timestamps the recorder writes. They carry a local
// offset ("2026-10-13T10:21:05.4561533+03:00"), which RFC3339 covers; the
// strip's own format is accepted too because -Status renders it that way.
func parseStatusTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05 -07:00", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func jsonString(value any) string {
	text, _ := value.(string)
	return text
}

func jsonInt(value any) int {
	number, ok := value.(float64)
	if !ok {
		return 0
	}
	return int(number)
}
