package supervise

import (
	"fmt"
	"time"

	"holocron/internal/clock"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
)

// Started records that the Start handed out for e has a process (Start-CamChild, R:1658-1712).
func (m *Manager) Started(e *Entry, pid int64) {
	nowWall, nowMono := m.now()
	e.pending = pendNone
	e.running = true
	e.pid, e.lastPid = pid, pid
	e.input = e.pendingInput
	e.startedWall, e.startedMono = nowWall, nowMono
	if e.firstStart.IsZero() {
		e.firstStart = nowWall
	}
	e.killReason, e.killError = "", ""
	m.log.Info(fmt.Sprintf("start %s pid %d on the %s input", e.cam.Name, pid, e.input),
		logx.Action("child.start"), logx.Target(e.cam.Name), logx.Result("ok"), logx.PID(int(pid)), "input", string(e.input))
}

// StartFailed records a Start that produced no process: it backs off with ranFor 0 and is NOT
// counted as a restart (IR 3.4, R:4047-4057).
func (m *Manager) StartFailed(e *Entry, err error) {
	nowWall, _ := m.now()
	e.pending = pendNone
	e.input = e.pendingInput
	if e.retiring {
		m.dropRetiring(e)
		return
	}
	e.lastError = "failed to start: " + err.Error()
	e.lastErrorAt = nowWall
	wait := m.setBackoff(e, 0)
	m.log.Warn(fmt.Sprintf("failed to start %s: %s; retry in %ds", e.cam.Name, err.Error(), wait),
		logx.Action("child.start"), logx.Target(e.cam.Name), logx.Result("fail"), logx.Err(err))
}

// Exited records that e's process has gone, whether on its own or after a Kill. Every exit that
// was not asked for counts as a restart whatever the code, 0 included (IR 3.5); a stall kill counts
// with the stall text (R:3998-4009); a move off the direct input and a stop never count (R:3970-3979,
// R:3637-3639). The empty-segment cleanup is always requested (IR 3.8), never performed here.
func (m *Manager) Exited(e *Entry, info ffmpeg.ExitInfo) Cleanup {
	nowWall, nowMono := m.now()
	ranFor := nowMono.Sub(e.startedMono).Seconds()
	reason := e.killReason
	e.running = false
	e.pid = 0
	e.pending = pendNone
	e.killReason = ""
	cleanup := Cleanup{SafeName: e.safe, Dir: e.dir}
	if e.retiring {
		m.dropRetiring(e)
		m.log.Info(fmt.Sprintf("stopped %s (pid %d)", e.cam.Name, e.lastPid),
			logx.Action("child.stop"), logx.Target(e.cam.Name), logx.Result("ok"), logx.PID(int(e.lastPid)))
		return cleanup
	}
	switch reason {
	case ReasonMoveToRelay:
		e.nextWall, e.nextMono = nowWall, nowMono
		m.log.Info(fmt.Sprintf("%s: the restreamer is now serving this camera, moving it off the direct input", e.cam.Name),
			logx.Action("child.move"), logx.Target(e.cam.Name), logx.Result("ok"))
	case ReasonStalled:
		m.countRestart(e, nowMono)
		e.lastError, e.lastErrorAt = e.killError, nowWall
		wait := m.setBackoff(e, e.killRanFor)
		m.log.Warn(fmt.Sprintf("watchdog restart #%d for %s in %ds", e.restartCount, e.cam.Name, wait),
			logx.Action("child.restart"), logx.Target(e.cam.Name), logx.Result("ok"))
	default:
		m.countRestart(e, nowMono)
		e.lastError = fmt.Sprintf("ffmpeg exited with code %d after %ds", info.Code, roundInt(ranFor))
		e.lastErrorAt = nowWall
		wait := m.setBackoff(e, ranFor)
		m.log.Warn(fmt.Sprintf("exit %d after %ds for %s; restart #%d in %ds", info.Code, roundInt(ranFor), e.cam.Name, e.restartCount, wait),
			logx.Action("child.restart"), logx.Target(e.cam.Name), logx.Result("ok"), logx.PID(int(e.lastPid)))
	}
	e.killError = ""
	return cleanup
}

// setBackoff is Set-CamChildBackoff (R:2002-2029): a run of at least HealthyResetSeconds resets the
// next wait to BackoffStartSeconds, otherwise it doubles from 0, raised to the start and capped.
func (m *Manager) setBackoff(e *Entry, ranFor float64) int {
	var next int
	if ranFor >= m.s.HealthyReset {
		next = m.s.BackoffStart
	} else {
		next = min(max(e.backoff*2, m.s.BackoffStart), m.s.BackoffMax)
	}
	e.backoff = next
	nowWall, nowMono := m.now()
	e.nextWall = nowWall.Add(time.Duration(next) * time.Second)
	e.nextMono = nowMono + clock.Mono(time.Duration(next)*time.Second)
	m.log.Debug("backoff", logx.Target(e.cam.Name), "ran_for_s", ranFor, "next_s", next)
	return next
}

// countRestart adds one restart: the ring keeps the stamps of the last hour, at most RestartRingCap.
func (m *Manager) countRestart(e *Entry, now clock.Mono) {
	e.restartCount++
	cut := now - clock.Mono(time.Hour)
	keep := e.restarts[:0]
	for _, s := range e.restarts {
		if s > cut {
			keep = append(keep, s)
		}
	}
	keep = append(keep, now)
	if len(keep) > m.s.RestartRingCap {
		keep = keep[len(keep)-m.s.RestartRingCap:]
	}
	e.restarts = keep
}

// restartsLastHour counts stamps newer than one hour (R:2825-2827).
func restartsLastHour(e *Entry, now clock.Mono) int64 {
	cut := now - clock.Mono(time.Hour)
	var n int64
	for _, s := range e.restarts {
		if s > cut {
			n++
		}
	}
	return n
}
