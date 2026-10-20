package host

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
	"holocron/internal/osx"
)

// AuxRestart is the restart policy of an auxiliary child: restart in place, never giving up, with
// a growing delay that resets after a long healthy life, and an alert when it keeps crashing.
type AuxRestart struct {
	Delays       []time.Duration // delay before restart n (0-based after the last healthy life); the last repeats. Default 5, 10, 20, 40, 60 s
	HealthyReset time.Duration   // a life this long resets the backoff. Default 300 s
	AlertAfter   int             // crashes within AlertWindow that raise the alert. Default 5
	AlertWindow  time.Duration   // Default 30 min
	AlertID      int             // 0: no alert
	AlertTitle   string
}

func (r *AuxRestart) defaults() {
	if len(r.Delays) == 0 {
		r.Delays = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second}
	}
	if r.HealthyReset <= 0 {
		r.HealthyReset = 300 * time.Second
	}
	if r.AlertAfter <= 0 {
		r.AlertAfter = 5
	}
	if r.AlertWindow <= 0 {
		r.AlertWindow = 30 * time.Minute
	}
}

// Aux is a child of the host that is not the supervisor (the web role). It lives in the host's
// process scope, so it dies with the host. Its crash never stops or restarts the supervisor, and a
// supervisor restart never restarts it.
type Aux struct {
	Name     string
	Spawner  Spawner
	Restart  AuxRestart
	Stop     func(Proc)    // asks the child to end on its own (nil: it is only killed after StopWait)
	StopWait time.Duration // how long it gets after Stop before Kill. Default 5 s

	// Registry, with Inspector, lists the running child (pid, start token, image, role Name) for
	// the census of `holocron down`; a nil Registry lists nothing.
	Registry  *osx.ChildRegistry
	Inspector osx.Inspector
}

type auxRunner struct {
	h    *Host
	a    Aux
	log  *slog.Logger
	quit chan struct{}
	done chan struct{}

	mu   sync.Mutex
	proc Proc
	// stopAsked is set once the graceful request was made
	stopAsked bool
}

// auxSet is the running auxiliary children of one Run.
type auxSet struct {
	items    []*auxRunner
	stopOnce sync.Once
}

func (h *Host) startAux(ctx context.Context) *auxSet {
	set := &auxSet{}
	for _, a := range h.d.Aux {
		a.Restart.defaults()
		if a.StopWait <= 0 {
			a.StopWait = 5 * time.Second
		}
		r := &auxRunner{h: h, a: a, log: h.log, quit: make(chan struct{}), done: make(chan struct{})}
		set.items = append(set.items, r)
		go r.run(ctx)
	}
	return set
}

func (r *auxRunner) fields(extra ...slog.Attr) []any {
	attrs := append([]slog.Attr{logx.Actor("host"), logx.Target(r.a.Name)}, extra...)
	out := make([]any, len(attrs))
	for i, a := range attrs {
		out[i] = a
	}
	return out
}

func (r *auxRunner) run(ctx context.Context) {
	defer close(r.done)
	h, pol := r.h, r.a.Restart
	var crashes []clock.Mono
	var lastAlert clock.Mono
	alerted := false
	consecutive := 0
	for lives := 1; ; lives++ {
		started := h.clk.Mono()
		proc, err := r.a.Spawner.Spawn(ctx)
		var code int
		var waitErr error
		if err != nil {
			r.log.Error(fmt.Sprintf("%s could not be started: %v", r.a.Name, err), r.fields(logx.Action("host."+r.a.Name+".start"), logx.Result(logx.ResultFail), logx.Err(err))...)
		} else {
			r.mu.Lock()
			r.proc = proc
			asked := r.stopAsked
			r.mu.Unlock()
			r.register(proc)
			r.log.Info(fmt.Sprintf("%s started: pid %d (life %d)", r.a.Name, proc.Pid(), lives),
				r.fields(logx.Action("host."+r.a.Name+".start"), logx.Result(logx.ResultOK), logx.PID(proc.Pid()))...)
			if asked { // a stop raced the start
				r.a.askStop(proc)
			}
			code, waitErr = proc.Wait()
			r.mu.Lock()
			r.proc = nil
			r.mu.Unlock()
			r.unregister(proc)
		}
		select {
		case <-r.quit:
			if proc != nil {
				r.log.Info(fmt.Sprintf("%s (pid %d) stopped", r.a.Name, proc.Pid()), r.fields(logx.Action("host."+r.a.Name+".stop"), logx.Result(logx.ResultOK), logx.PID(proc.Pid()))...)
			}
			return
		default:
		}

		ranFor := h.clk.Since(started)
		pid := 0
		if proc != nil {
			pid = proc.Pid()
		}
		if ranFor >= pol.HealthyReset {
			consecutive = 0
		}
		now := h.clk.Mono()
		kept := crashes[:0]
		for _, at := range crashes {
			if now.Sub(at) < pol.AlertWindow {
				kept = append(kept, at)
			}
		}
		crashes = append(kept, now)
		delay := pol.Delays[min(consecutive, len(pol.Delays)-1)]
		consecutive++
		why := fmt.Sprintf("%s (pid %d) stopped without being asked after %ds (exit %d", r.a.Name, pid, int(ranFor.Seconds()), code)
		if waitErr != nil {
			why += ", " + waitErr.Error()
		}
		why += ")"
		r.log.Warn(fmt.Sprintf("%s; restarting it in %v (%d crash(es) in the last %v)", why, delay, len(crashes), pol.AlertWindow),
			r.fields(logx.Action("host."+r.a.Name+".restart"), logx.Result(logx.ResultOK), logx.PID(pid))...)
		if pol.AlertID != 0 && len(crashes) >= pol.AlertAfter && (!alerted || now.Sub(lastAlert) >= pol.AlertWindow) {
			alerted, lastAlert = true, now
			h.raise(Event{ID: pol.AlertID, Severity: "Error", Title: pol.AlertTitle, Key: fmt.Sprintf("%d:%d", pol.AlertID, lives),
				Message: fmt.Sprintf("%s. It has crashed %d times in %d minutes; it keeps being restarted (next in %v). Recording is not affected.",
					why, len(crashes), int(pol.AlertWindow.Minutes()), delay)})
		}
		select {
		case <-h.clk.After(delay):
		case <-r.quit:
			return
		}
	}
}

func (r *auxRunner) register(p Proc) {
	if r.a.Registry == nil || r.a.Inspector == nil {
		return
	}
	info, err := r.a.Inspector.Inspect(p.Pid())
	if err != nil {
		r.log.Warn(fmt.Sprintf("%s pid %d is not listed for `holocron down`: %v", r.a.Name, p.Pid(), err), r.fields(logx.Action("host."+r.a.Name+".register"), logx.Result(logx.ResultFail))...)
		return
	}
	if err := r.a.Registry.Add(osx.ChildEntry{Pid: p.Pid(), Start: info.Start, Image: info.Image, Group: info.Group, Role: r.a.Name}); err != nil {
		r.log.Warn(fmt.Sprintf("%s pid %d could not be listed: %v", r.a.Name, p.Pid(), err), r.fields(logx.Action("host."+r.a.Name+".register"), logx.Result(logx.ResultFail))...)
	}
}

func (r *auxRunner) unregister(p Proc) {
	if r.a.Registry != nil {
		_ = r.a.Registry.Remove(p.Pid())
	}
}

func (a Aux) askStop(p Proc) {
	if a.Stop != nil {
		a.Stop(p)
	}
}

// ask makes the graceful stop request once; it never blocks.
func (r *auxRunner) ask() {
	r.mu.Lock()
	r.stopAsked = true
	p := r.proc
	r.mu.Unlock()
	if p != nil {
		r.a.askStop(p)
	}
}

// requestStop sends the graceful request to every child at once (the supervisor's own stop runs
// meanwhile, so a window close needs no extra time for them).
func (s *auxSet) requestStop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		for _, r := range s.items {
			close(r.quit)
			r.ask()
		}
	})
}

// finish waits for each child to end after its request, killing the one that does not within
// StopWait (its scope would take it with the host anyway).
func (s *auxSet) finish(h *Host) {
	if s == nil {
		return
	}
	s.requestStop()
	for _, r := range s.items {
		select {
		case <-r.done:
			continue
		case <-h.clk.After(r.a.StopWait):
		}
		r.mu.Lock()
		p := r.proc
		r.mu.Unlock()
		if p != nil {
			r.log.Warn(fmt.Sprintf("%s (pid %d) did not end within %v of the request; killing it", r.a.Name, p.Pid(), r.a.StopWait),
				r.fields(logx.Action("host."+r.a.Name+".stop"), logx.Result(logx.ResultFail), logx.PID(p.Pid()))...)
			_ = p.Kill()
		}
		select {
		case <-r.done:
		case <-h.clk.After(2 * time.Second):
		}
	}
}
