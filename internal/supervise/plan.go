package supervise

import (
	"fmt"
	"sort"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
)

// RelayView is what Plan needs from the relay this cycle.
type RelayView struct {
	// Ready is the ONE ready set fetched for this cycle and shared by every child (IR 11 risk 8);
	// nil when the relay is off or no set was fetched (nothing is serving).
	Ready Ready
	// Alive: the relay process is running; StartedAt is its start on the monotonic clock.
	Alive     bool
	StartedAt clock.Mono
}

// Plan decides what to do now (one supervisor iteration, R:3835-4057): Kill for retiring
// children (ReasonStop), for a live direct child whose relay path is serving (ReasonMoveToRelay)
// and for a stalled child (ReasonStalled); Start for every due child that is not held, on the relay
// input when its path is in the ready set, else direct. Starts are spaced by StartStagger through
// Action.Delay (a non-blocking schedule, at most one start per StartStagger across all cameras),
// earliest-due first, then by name. Each action is handed out once: the entry stays pending until
// Started, StartFailed or Exited. The output depends only on the entries, rv and the clock.
func (m *Manager) Plan(rv RelayView) []Action {
	nowWall, nowMono := m.now()
	m.tick(nowWall, nowMono)
	var acts []Action
	for _, e := range m.retiring {
		if e.running && e.pending == pendNone {
			e.pending, e.killReason = pendKill, ReasonStop
			acts = append(acts, Action{Kind: ActKill, Entry: e, Reason: ReasonStop})
			m.log.Info(fmt.Sprintf("kill %s pid %d: stop", e.cam.Name, e.pid), logx.Action("child.stop"), logx.Target(e.cam.Name), logx.PID(int(e.pid)))
		}
	}
	if m.shutdown {
		return acts
	}
	var due []*Entry
	for _, e := range m.sorted() {
		if e.pending != pendNone {
			continue
		}
		if e.running {
			if e.input == InputDirect && m.serving(e, rv) {
				e.pending, e.killReason = pendKill, ReasonMoveToRelay
				acts = append(acts, Action{Kind: ActKill, Entry: e, Reason: ReasonMoveToRelay})
				m.log.Info(fmt.Sprintf("kill %s pid %d: the restreamer is now serving this camera, moving it off the direct input", e.cam.Name, e.pid),
					logx.Action("child.stop"), logx.Target(e.cam.Name), logx.PID(int(e.pid)))
				continue
			}
			if age, thr, ok := m.stalled(e, nowWall, nowMono); ok {
				reason := fmt.Sprintf("stalled, no segment write for %ds (limit %ds) while the process was alive", roundInt(age), roundInt(thr))
				e.pending, e.killReason, e.killError = pendKill, ReasonStalled, reason
				e.killRanFor = nowMono.Sub(e.startedMono).Seconds()
				acts = append(acts, Action{Kind: ActKill, Entry: e, Reason: ReasonStalled})
				m.log.Warn(fmt.Sprintf("watchdog %s: %s", e.cam.Name, reason), logx.Action("child.stop"), logx.Target(e.cam.Name), logx.PID(int(e.pid)))
			}
			continue
		}
		if nowMono >= e.nextMono && !m.blockedBySibling(e) {
			due = append(due, e)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].nextMono != due[j].nextMono {
			return due[i].nextMono < due[j].nextMono
		}
		return due[i].safe < due[j].safe
	})
	waitEnd := rv.StartedAt + clock.Mono(m.s.RelayReadyWait)
	for _, e := range due {
		serving := m.serving(e, rv)
		if m.s.RelayEnabled && e.relayPath != "" && !serving && rv.Alive && nowMono < waitEnd {
			e.heldSince = true
			m.log.Debug(fmt.Sprintf("%s: held while the restreamer brings up '%s'", e.cam.Name, e.relayPath), logx.Target(e.cam.Name))
			continue
		}
		reason := ReasonStart
		if e.heldSince {
			if !serving {
				reason = ReasonHoldExpired
				m.log.Warn(fmt.Sprintf("%s: the restreamer did not serve '%s' within %ds, starting it on the direct input",
					e.cam.Name, e.relayPath, int(m.s.RelayReadyWait/time.Second)), logx.Target(e.cam.Name))
			}
			e.heldSince = false
		}
		input := InputDirect
		if serving {
			input = InputRelay
		}
		slot := nowMono
		if m.slotSet && m.lastSlot+clock.Mono(m.s.StartStagger) > slot {
			slot = m.lastSlot + clock.Mono(m.s.StartStagger)
		}
		m.lastSlot, m.slotSet = slot, true
		e.pending, e.pendingInput = pendStart, input
		acts = append(acts, Action{Kind: ActStart, Entry: e, Input: input, Delay: slot.Sub(nowMono), Reason: reason})
		m.log.Debug(fmt.Sprintf("%s: start on the %s input in %v", e.cam.Name, input, slot.Sub(nowMono)), logx.Target(e.cam.Name), "path", e.relayPath, "serving", serving)
	}
	return acts
}

// serving: the relay is on, the camera has a relay path and the cycle's set has it ("denied"
// counts as ready through relay.ReadySet.Has).
func (m *Manager) serving(e *Entry, rv RelayView) bool {
	return m.s.RelayEnabled && e.relayPath != "" && rv.Ready != nil && rv.Ready.Has(e.relayPath)
}
