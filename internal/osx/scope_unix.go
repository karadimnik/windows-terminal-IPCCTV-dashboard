//go:build unix

package osx

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

type startReq struct {
	cmd   *exec.Cmd
	reply chan error
}

// scopeSys owns the starter goroutine. It locks its OS thread first and never unlocks it, and it
// does not return before Close: Pdeathsig is bound to the THREAD that forked the child, and a
// locked goroutine that returns ends its thread, which kills every child it created at once while
// this process lives (SP3 l3).
type scopeSys struct {
	reqs chan startReq
	quit chan struct{}
	done chan struct{}
}

func (ss *scopeSys) open(s *Scope) error {
	ss.reqs, ss.quit, ss.done = make(chan startReq), make(chan struct{}), make(chan struct{})
	if s.starterReturns {
		close(ss.done)
		return nil
	}
	go func() {
		runtime.LockOSThread() // deliberately never unlocked
		defer close(ss.done)
		for {
			select {
			case r := <-ss.reqs:
				r.reply <- r.cmd.Start()
			case <-ss.quit:
				return
			}
		}
	}()
	return nil
}

func (ss *scopeSys) run(s *Scope, cmd *exec.Cmd) error {
	if s.starterReturns { // negative control only: the SP3 l3 pitfall
		reply := make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			reply <- cmd.Start()
		}()
		return <-reply
	}
	reply := make(chan error, 1)
	select {
	case ss.reqs <- startReq{cmd, reply}:
	case <-ss.done:
		return ErrScopeClosed
	}
	return <-reply
}

type procSys struct {
	mu     sync.Mutex
	exited bool // the leader is waitable or reaped: its pid no longer pins the group id
}

func (ss *scopeSys) start(s *Scope, p *proc, path string, c Cmd) (ProcInfo, error) {
	cmd := exec.Command(path, c.Args...)
	cmd.Dir, cmd.Env = c.Dir, c.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	}
	cmd.WaitDelay = OutputDrain
	cmd.SysProcAttr = sysProcAttr()
	if err := ss.run(s, cmd); err != nil {
		return ProcInfo{}, err
	}
	p.pid = cmd.Process.Pid
	if s.afterCreateHook != nil {
		s.afterCreateHook(p.pid)
	}
	info, err := OSInspector{}.Inspect(p.pid)
	if err != nil {
		info = ProcInfo{Image: path}
	}
	info.Group = p.pid
	go p.wait(cmd)
	return info, nil
}

// wait: on Linux the exit is first observed WITHOUT reaping (waitid WNOWAIT), so the group id is
// still pinned by the zombie leader; the rest of the group (grandchildren) is killed then, and only
// after that is the leader reaped.
func (p *proc) wait(cmd *exec.Cmd) {
	if waitNoReap(p.pid) {
		p.sys.mu.Lock()
		p.sys.exited = true
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		p.sys.mu.Unlock()
	}
	err := cmd.Wait()
	p.sys.mu.Lock()
	p.sys.exited = true
	p.sys.mu.Unlock()
	p.code = -1
	if cmd.ProcessState != nil {
		p.code = cmd.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		err = nil
	}
	p.werr = err
	p.s.finish(p)
}

func (ps *procSys) signal(p *proc, sig syscall.Signal) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.exited {
		return nil
	}
	if err := syscall.Kill(-p.pid, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func (ps *procSys) kill(p *proc) error { return ps.signal(p, syscall.SIGKILL) }

func (ss *scopeSys) stopAll(s *Scope, ps []*proc) {
	for _, p := range ps {
		_ = p.sys.signal(p, syscall.SIGTERM)
	}
}

func (ss *scopeSys) close(s *Scope) error {
	select {
	case <-ss.done:
	default:
		close(ss.quit)
		<-ss.done
	}
	return nil
}
