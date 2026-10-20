package supervise

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
	"holocron/internal/osx"
)

// NewProduction is NewSupervisor with the OS bindings (W8): unless set in d, Lock is the OS
// instance lock on <root>/_state/supervisor.lock and Scope is the OS process scope (registry reap,
// then the job object / process group). root must be the configuration's RootPath.
func NewProduction(d SupervisorDeps, root string) *Supervisor {
	state := filepath.Join(root, "_state")
	if d.Lock == nil {
		d.Lock = OSLock{Path: osx.LockPath(state)}
	}
	if d.Scope == nil {
		var log *slog.Logger
		if d.Logs != nil {
			log = d.Logs.For("supervisor")
		}
		d.Scope = &OSScope{StateDir: state, Log: log}
	}
	return NewSupervisor(d)
}

// OSLock is the production InstanceLock (osx.AcquireInstanceLock): held for the supervisor's
// life, released by the OS when the process dies.
type OSLock struct{ Path string }

func (l OSLock) Acquire() (func(), string, error) {
	lk, err := osx.AcquireInstanceLock(l.Path)
	if err != nil {
		var he *osx.HeldError
		if errors.As(err, &he) {
			return nil, he.Owner.String(), err
		}
		return nil, fmt.Sprintf("unknown (the lock %s cannot be taken: %v)", l.Path, err), err
	}
	return func() { _ = lk.Release() }, lk.Owner().String(), nil
}

// OSScope is the production ProcessScope. Open reaps the provable orphans an earlier supervisor
// left in <state>/children.json (the supervisor calls Open holding the lock and before any child),
// then creates the osx.Scope; the returned Starter creates every child inside it (the base
// starter is not used). Close stops what is left in order and releases the binding.
type OSScope struct {
	StateDir  string
	Log       *slog.Logger
	Inspector osx.Inspector // default osx.OSInspector{}
	StopGrace time.Duration // default 2 s

	scope *osx.Scope
}

func (o *OSScope) Open(ffmpeg.Starter) (ffmpeg.Starter, error) {
	if o.Log == nil {
		o.Log = discardLogger()
	}
	in := o.Inspector
	if in == nil {
		in = osx.OSInspector{}
	}
	reg := osx.NewRegistry(osx.RegistryPath(o.StateDir))
	verdicts, err := osx.Reap(reg, in, o.Log)
	if err != nil {
		o.Log.Warn("child registry reap: "+err.Error(), logx.Action("child.reap"), logx.Result(logx.ResultFail), logx.Err(err))
	}
	killed := 0
	for _, v := range verdicts {
		if v.Killed {
			killed++
		}
	}
	if len(verdicts) > 0 {
		o.Log.Info(fmt.Sprintf("child registry: %d entr(ies) from an earlier run, %d orphan(s) stopped", len(verdicts), killed), logx.Action("child.reap"), logx.Result(logx.ResultOK))
	}
	s, err := osx.NewScope("supervisor", osx.ScopeOptions{Registry: reg, Log: o.Log})
	if err != nil {
		return nil, err
	}
	o.scope = s
	return ScopedStarter{Scope: s}, nil
}

func (o *OSScope) Close() error {
	if o.scope == nil {
		return nil
	}
	grace := o.StopGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	if surv := o.scope.Stop(grace); len(surv) > 0 {
		o.Log.Error(fmt.Sprintf("DID NOT STOP: child process(es) %v are still alive after the scope stop", surv), logx.Action("supervisor.stop"), logx.Result(logx.ResultFail))
	}
	return o.scope.Close()
}

// ScopedStarter is an ffmpeg.Starter that creates the process inside an osx.Scope (it lives here,
// not in osx, because ffmpeg imports osx).
type ScopedStarter struct{ Scope *osx.Scope }

func (s ScopedStarter) Start(c ffmpeg.StartConfig) (ffmpeg.Process, error) {
	return s.Scope.Start(osx.Cmd{Path: c.Exe, Args: c.Args, Dir: c.Dir, Env: c.Env, Stdout: c.Stdout, Stderr: c.Stderr, Role: filepath.Base(c.Exe)})
}
