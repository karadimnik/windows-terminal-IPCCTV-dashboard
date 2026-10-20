package supervise

import (
	"fmt"
	"os"
	"time"

	"holocron/internal/clock"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
	"holocron/internal/prune"
	"holocron/internal/status"
)

// pruneLoop runs the pruner at start and every PruneIntervalSeconds on its own goroutine (never on
// the heartbeat's). Its State.Settle alerts (120, 121, 122) go to Alerts.
func (s *Supervisor) pruneLoop(done chan<- struct{}) {
	defer close(done)
	defer s.recoverFault("the pruner")
	st := &prune.State{}
	for {
		s.pruneOnce(st)
		select {
		case <-s.clk.After(seconds(s.cfg.PruneIntervalSeconds)):
		case <-s.stopCh:
			return
		}
	}
}

func (s *Supervisor) pruneOnce(st *prune.State) {
	now := s.clk.Now()
	res, err := s.d.Pruner.Run(now, st)
	if err != nil {
		s.plog.Error("prune failed: "+err.Error(), logx.Action("prune.run"), logx.Target(s.cfg.RootPath), logx.Result(logx.ResultFail), logx.Err(err))
		return
	}
	s.mu.Lock()
	s.pruneView = status.Prune{
		LastRunAt: s.ts(now), DeletedTotal: int64(st.DeletedTotal), FreedGBTotal: round(float64(st.FreedBytes)/gib, 3),
		Blocked: st.Blocked, LastMessage: st.LastMessage,
	}
	s.mu.Unlock()
	for _, a := range res.Alerts {
		s.alert(s.plog, Event{ID: a.ID, Severity: a.Level, Title: a.Title, Message: a.Body, Key: fmt.Sprint(a.ID)})
	}
}

// sandboxRefuses: with a sandbox in force (SANDBOX_ONLY), nothing outside it is deleted.
func (s *Supervisor) sandboxRefuses(path string) bool {
	return prune.CheckSandbox(path, sandboxFromEnv()) != nil
}

// teardown is the ordered stop (IS 2.2 G6/G7, R:4133-4191): cameras (hard kill), then the live
// view, then the relay, each waited for; survivors are reported, never hidden; then the final
// document, the stop-request file removed. The lock is released by Run.
func (s *Supervisor) teardown(req stopRequest, loopQuit chan struct{}, loopDone, pruneDone <-chan struct{}, hbCancel func(), hbDone <-chan struct{}) {
	s.log.Info("supervisor shutting down: "+req.reason, logx.Action("supervisor.stop"), "kind", req.kind)
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.stopControl()
	close(s.stopCh)
	s.cancel()
	close(loopQuit)

	budget := seconds(s.cfg.StopTimeoutSeconds)
	if budget <= 0 {
		budget = 30 * time.Second
	}
	deadline := s.clk.Mono() + clock.Mono(budget)
	phase := func() clock.Mono { return min(s.clk.Mono()+clock.Mono(10*time.Second), deadline) }

	managerOK := s.waitClosed(loopDone, phase())
	if !managerOK {
		s.log.Error("the reconcile loop did not end; stopping the children without it", logx.Action("supervisor.stop"))
	}
	s.waitStarts(phase())

	// 1. cameras
	killed := map[*ffmpeg.Child]bool{}
	if managerOK {
		s.safely("the camera shutdown", func() {
			s.publishRows()
			s.m.Shutdown()
			for _, a := range s.m.Plan(RelayView{}) {
				s.mu.Lock()
				child := s.registry[a.Entry]
				s.mu.Unlock()
				if child != nil && !killed[child] {
					killed[child] = true
					_ = child.Kill()
				}
			}
		})
	}
	s.mu.Lock()
	rest := make([]*ffmpeg.Child, 0, len(s.registry))
	for _, c := range s.registry {
		if !killed[c] {
			rest = append(rest, c)
		}
	}
	s.mu.Unlock()
	for _, c := range rest {
		s.log.Info(fmt.Sprintf("kill pid %d: %s", c.Pid(), req.reason), logx.Action("child.stop"), logx.PID(c.Pid()))
		_ = c.Kill()
	}
	s.waitCameras(phase(), managerOK)

	// 2. live view (its main paths read the relay), 3. the relay
	s.liveMu.Lock() // a run-time switch finishes first; none starts after this
	s.mu.Lock()
	liveM := s.liveM
	s.mu.Unlock()
	s.liveMu.Unlock()
	for _, m := range []*mtxChild{liveM, s.relayM} {
		if m == nil {
			continue
		}
		m.stop()
		if !s.waitClosed(m.done, phase()) {
			s.log.Error(m.label+": its supervisor did not end in time", logx.Action("relay.stop"), logx.Result(logx.ResultFail))
		}
	}
	if !s.waitClosed(pruneDone, deadline) {
		s.plog.Warn("the pruner is still in a run; it is not interrupted", logx.Action("prune.run"))
	}

	// verify: every child we started must be gone
	var survivors []int
	s.mu.Lock()
	for _, c := range s.registry {
		if !isDone(c) {
			survivors = append(survivors, c.Pid())
		}
	}
	for _, m := range []*mtxChild{liveM, s.relayM} {
		if m != nil && m.child != nil && !isDone(m.child) {
			survivors = append(survivors, m.child.Pid())
		}
	}
	s.mu.Unlock()
	if len(survivors) > 0 {
		s.log.Error(fmt.Sprintf("DID NOT STOP: %d child process(es) are still alive after the stop wait: pids %v", len(survivors), survivors),
			logx.Action("supervisor.stop"), logx.Result(logx.ResultFail))
	}

	// the final document: the rows as they were before the shutdown retired the entries (counters,
	// LastPid: the proof that no orphan was left), with every child that has gone marked so
	s.mu.Lock()
	gone := map[int64]bool{}
	for _, row := range s.rows {
		if row.Pid != nil {
			gone[*row.Pid] = true
		}
	}
	for _, p := range survivors {
		delete(gone, int64(p))
	}
	for i := range s.rows {
		row := &s.rows[i]
		if row.Pid != nil && gone[*row.Pid] {
			row.LastPid, row.Pid, row.Alive = row.Pid, nil, false
		}
		row.NextStartAt = status.Time{}
	}
	s.mu.Unlock()
	hbCancel()
	<-hbDone
	var us *status.UnexpectedStop
	if req.kind == StopUnexpected {
		us = &status.UnexpectedStop{At: s.ts(s.clk.Now()), Reason: s.d.Redactor.String(req.reason), SupervisorPid: s.d.Pid, DetectedBy: "supervisor"}
	}
	doc := s.document(status.StateStopped, req.kind, us)
	if s.onDoc != nil {
		s.onDoc(doc)
	}
	final := true
	if err := s.writer.Write(doc); err != nil {
		final = false
		s.log.Error("the final status write failed: "+err.Error(), logx.Action("status.write"), logx.Result(logx.ResultFail), logx.Err(err))
	}
	if _, err := os.Lstat(s.stopFile); err == nil {
		if err := os.Remove(s.stopFile); err != nil {
			s.log.Warn("could not remove the stop request: "+err.Error(), logx.Err(err))
		}
	}
	s.log.Info(fmt.Sprintf("supervisor stopped (%s)", req.kind), logx.Action("supervisor.stop"), logx.Result(logx.ResultOK))
	s.mu.Lock()
	s.report = StopReport{Kind: req.kind, Reason: req.reason, Survivors: survivors, Final: final}
	s.mu.Unlock()
}

func isDone(c *ffmpeg.Child) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// safely runs f and turns a panic into a log line: the teardown must go on.
func (s *Supervisor) safely(what string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error(fmt.Sprintf("%s failed: %v", what, r), logx.Action("supervisor.stop"), logx.Result(logx.ResultFail))
		}
	}()
	f()
}

const stopPoll = 100 * time.Millisecond

// waitClosed waits for ch until the deadline on the supervisor clock.
func (s *Supervisor) waitClosed(ch <-chan struct{}, until clock.Mono) bool {
	for {
		select {
		case <-ch:
			return true
		default:
		}
		left := until.Sub(s.clk.Mono())
		if left <= 0 {
			return false
		}
		select {
		case <-ch:
			return true
		case <-s.clk.After(min(left, stopPoll)):
		}
	}
}

// waitStarts lets the camera starts in flight end (they see the stop and kill what they started),
// answering their reports.
func (s *Supervisor) waitStarts(until clock.Mono) {
	all := make(chan struct{})
	go func() { s.startWG.Wait(); close(all) }()
	for {
		select {
		case <-all:
			return
		case r := <-s.startCh:
			s.safely("a start report", func() { s.onStarted(r) })
		case <-s.clk.After(stopPoll):
			if s.clk.Mono() >= until {
				return
			}
		}
	}
}

// waitCameras answers start and exit reports until every camera child has gone or the deadline.
func (s *Supervisor) waitCameras(until clock.Mono, managerOK bool) {
	for {
		s.mu.Lock()
		n := len(s.registry)
		s.mu.Unlock()
		if n == 0 && len(s.exitCh) == 0 && len(s.startCh) == 0 {
			return
		}
		select {
		case x := <-s.exitCh:
			if managerOK {
				s.safely("an exit report", func() { s.onExited(x) })
			}
		case r := <-s.startCh:
			if managerOK {
				s.safely("a start report", func() { s.onStarted(r) })
			}
		case <-s.clk.After(stopPoll):
			if s.clk.Mono() >= until {
				return
			}
		}
	}
}
