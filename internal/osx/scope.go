package osx

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// Cmd is one child to start inside a Scope.
type Cmd struct {
	Path string   // executable; looked up in PATH when it has no directory part
	Args []string // arguments after the program name
	Dir  string   // working directory; empty: inherited
	Env  []string // complete environment; nil: this process's environment
	// Stdout and Stderr receive the output; nil discards it. Stdin is always the null device.
	Stdout, Stderr io.Writer
	Role           string // for the registry and the log, e.g. "ffmpeg:Front"
}

// Proc is a started child.
type Proc interface {
	Pid() int
	// Wait blocks until the process has exited and its output was delivered (at most OutputDrain
	// after the exit). The code is -1 when the OS gives none (a signal on Unix); on Windows it is
	// the signed 32-bit value.
	Wait() (code int, err error)
	// Kill ends the process at once (TerminateProcess / SIGKILL of its process group). Safe after
	// the exit and more than once.
	Kill() error
}

// OutputDrain is how long Wait waits for the last output after the exit (same as ffmpeg.OutputDrain).
const OutputDrain = 2 * time.Second

// ErrScopeClosed: Start after Close.
var ErrScopeClosed = errors.New("the process scope is closed")

// ScopeOptions configures a Scope.
type ScopeOptions struct {
	// Registry, when set, gets every child (pid, start token, image, group) right after its start and
	// loses it at its exit.
	Registry *ChildRegistry
	Log      *slog.Logger // nil discards
}

// Scope binds every child it starts to this process's lifetime (SP3):
//
//   - Windows: one unnamed, non-inheritable job with KILL_ON_JOB_CLOSE (never BREAKAWAY_OK);
//     children are created CREATE_SUSPENDED|CREATE_NO_WINDOW (each its own hidden console),
//     assigned, then resumed; a failed assignment kills the child and fails the start.
//   - Linux: every child is created by ONE goroutine locked to its OS thread that never returns
//     before Close, with Setpgid (each child leads its own group) and Pdeathsig SIGKILL.
//   - macOS: Setpgid only; no kernel binding (the registry reap at the next start is the net).
//
// Safe for concurrent use.
type Scope struct {
	name string
	reg  *ChildRegistry
	log  *slog.Logger

	mu     sync.Mutex
	closed bool
	live   map[int]*proc

	sys scopeSys

	// test hooks (negative controls)
	assignGap       time.Duration // Windows: no CREATE_SUSPENDED, assign this long after the start
	starterReturns  bool          // Linux: the starter goroutine locks, starts and RETURNS (SP3 l3)
	afterCreateHook func(pid int) // called between creation and assignment / registration
}

// NewScope creates the binding (the Windows job, the Linux starter thread).
func NewScope(name string, o ScopeOptions) (*Scope, error) {
	s := &Scope{name: name, reg: o.Registry, log: o.Log, live: map[int]*proc{}}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if err := s.sys.open(s); err != nil {
		return nil, fmt.Errorf("process scope %s: %w", name, err)
	}
	return s, nil
}

// proc is the common part of a child; procSys is the per-OS part.
type proc struct {
	s    *Scope
	pid  int
	role string
	done chan struct{}
	code int
	werr error
	sys  procSys
}

func (p *proc) Pid() int { return p.pid }

func (p *proc) Wait() (int, error) {
	<-p.done
	return p.code, p.werr
}

func (p *proc) Kill() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	return p.sys.kill(p)
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Start creates the child inside the scope and records it in the registry.
func (s *Scope) Start(c Cmd) (Proc, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrScopeClosed
	}
	path, err := exec.LookPath(c.Path)
	if err != nil {
		return nil, err
	}
	p := &proc{s: s, role: c.Role, done: make(chan struct{})}
	info, err := s.sys.start(s, p, path, c)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.live[p.pid] = p
	s.mu.Unlock()
	if s.reg != nil {
		if err := s.reg.Add(ChildEntry{Pid: p.pid, Start: info.Start, Image: info.Image, Group: info.Group, Role: c.Role}); err != nil {
			s.log.Warn("the child registry could not be written: "+err.Error(), "action", "child.register", "target", c.Role, "result", "fail", "pid", p.pid)
		}
	}
	return p, nil
}

// finish is called by the per-OS waiter after the exit, before Done.
func (s *Scope) finish(p *proc) {
	s.mu.Lock()
	delete(s.live, p.pid)
	s.mu.Unlock()
	if s.reg != nil {
		if err := s.reg.Remove(p.pid); err != nil {
			s.log.Warn("the child registry could not be written: "+err.Error(), "action", "child.register", "target", p.role, "result", "fail", "pid", p.pid)
		}
	}
	close(p.done)
}

// Live lists the pids of the children that have not exited, ascending.
func (s *Scope) Live() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.live))
	for pid := range s.live {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func (s *Scope) liveProcs() []*proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*proc, 0, len(s.live))
	for _, p := range s.live {
		out = append(out, p)
	}
	return out
}

// Stop is the orderly end of every child still running: Unix kill(-pgid, SIGTERM), Windows
// TerminateJobObject (every member, grandchildren included); then up to grace for them to exit,
// then SIGKILL / TerminateProcess for the rest and up to 2 s more. It returns the pids still alive,
// which are reported, never hidden.
func (s *Scope) Stop(grace time.Duration) (survivors []int) {
	ps := s.liveProcs()
	if len(ps) == 0 {
		return nil
	}
	s.sys.stopAll(s, ps)
	waitAll(ps, grace)
	for _, p := range ps {
		if !p.exited() {
			_ = p.Kill()
		}
	}
	waitAll(ps, 2*time.Second)
	for _, p := range ps {
		if !p.exited() {
			survivors = append(survivors, p.pid)
		}
	}
	sort.Ints(survivors)
	return survivors
}

func waitAll(ps []*proc, within time.Duration) {
	deadline := time.After(within)
	for _, p := range ps {
		select {
		case <-p.done:
		case <-deadline:
			return
		}
	}
}

// Close releases the binding. Whatever is still running dies with it: on Windows the job's last
// handle closes (kill on close), on Linux the starter thread ends (Pdeathsig). Further Starts fail.
func (s *Scope) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.sys.close(s)
}
