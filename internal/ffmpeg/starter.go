package ffmpeg

import (
	"io"
	"os"
	"os/exec"
	"time"
)

// StartConfig is what a Starter needs to create the process.
type StartConfig struct {
	Exe  string
	Args []string
	Dir  string   // working directory; empty: inherited
	Env  []string // complete environment
	// Stdout and Stderr receive the process output. Stdin is closed (the process gets no stdin).
	Stdout, Stderr io.Writer
}

// Process is a started child.
type Process interface {
	Pid() int
	// Wait blocks until the process has exited AND its output has been delivered to the writers, or
	// OutputDrain has passed since the exit. It returns the exit code (-1 when the OS reports none,
	// e.g. a signal on Unix).
	Wait() (code int, err error)
	// Kill ends the process immediately (TerminateProcess / SIGKILL). It is safe after exit.
	Kill() error
}

// Starter creates processes. Process creation, and with it the supervisor's job object or process
// group, lives behind this interface; ExecStarter is the plain os/exec implementation.
type Starter interface {
	Start(StartConfig) (Process, error)
}

// OutputDrain is how long Wait waits for the last output after the process has exited (IR 8.4: the
// final drain waits up to 2 s).
const OutputDrain = 2 * time.Second

// ExecStarter starts the process with os/exec: no console window on Windows, stdin closed.
type ExecStarter struct{}

type execProcess struct{ cmd *exec.Cmd }

// Start implements Starter.
func (ExecStarter) Start(c StartConfig) (Process, error) {
	cmd := exec.Command(c.Exe, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Stdin = nil // /dev/null
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	cmd.WaitDelay = OutputDrain
	cmd.SysProcAttr = hideWindow()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd}, nil
}

func (p *execProcess) Pid() int { return p.cmd.Process.Pid }

func (p *execProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	code := -1
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	if _, ok := err.(*exec.ExitError); ok {
		err = nil // a non-zero exit is accounted by the code, not an error of waiting
	}
	return code, err
}

func (p *execProcess) Kill() error { return p.cmd.Process.Kill() }
