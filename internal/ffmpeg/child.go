package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
)

const (
	// DefaultTailLines is how many of the last output lines an ExitInfo keeps. IR names no number
	// (the PowerShell product kept none); 20 holds the usual failure (an error line and its context).
	DefaultTailLines = 20
	// maxLineBytes bounds one output line; a longer one is cut into lines of this size.
	maxLineBytes = 8192
)

// Spec describes one ffmpeg child.
type Spec struct {
	Name string // the camera, for log records
	Exe  string
	Args []string
	Dir  string
	// Env is the complete environment. Nil means ChildEnv(os.Environ(), runtime.GOOS), which carries
	// the Windows TZ rule.
	Env []string
	// Starter creates the process; nil means ExecStarter{}.
	Starter Starter
	// Redactor masks every output line before it reaches the log or the tail. Nil means a
	// Redactor with no registered secrets, which still masks scheme://user:pass@ userinfo.
	Redactor *logx.Redactor
	// Log receives each output line (stderr and stdout at INFO and DEBUG, with attr stream); nil: no log.
	Log *slog.Logger
	// TailLines is the number of last lines kept; zero means DefaultTailLines.
	TailLines int
	// Clock times the run; nil means the real clock.
	Clock clock.Clock
	// Cleanup runs exactly once: after the process has exited and its output has been drained
	// (before Wait returns), or at once when Start fails. Use it to delete the input list
	// (WriteInputList) and to run RemoveEmptySegment.
	Cleanup func()
}

// ExitInfo is the account of one run.
type ExitInfo struct {
	Code      int           // exit code; -1 when the OS gave none
	RanFor    time.Duration // from start to the process exit
	LastLines []string      // the last lines of stderr and stdout, redacted, oldest first
	Err       error         // a failure of waiting (not a non-zero exit)
}

// Message is the LastError text of the PowerShell product: "ffmpeg exited with code N after Ns",
// N rounded like PowerShell's [int] cast (half to even).
func (e ExitInfo) Message() string {
	return fmt.Sprintf("ffmpeg exited with code %d after %ds", e.Code, int64(math.RoundToEven(e.RanFor.Seconds())))
}

// Child is one running ffmpeg. All methods are safe for concurrent use.
type Child struct {
	proc    Process
	clk     clock.Clock
	started clock.Mono
	at      time.Time
	tail    *tail
	done    chan struct{}
	info    ExitInfo
}

// Start launches the child and returns at once. The process gets no stdin; its stderr and stdout
// are read line by line, each line redacted before it is logged or kept. When ctx is cancelled the
// child is killed (hard, as the PowerShell product always does); nothing else here manages its
// lifetime: restarts, backoff, stall watching and the job object belong to the supervisor.
func Start(ctx context.Context, spec Spec) (*Child, error) {
	cleanup := onceFunc(spec.Cleanup)
	if spec.Exe == "" {
		cleanup()
		return nil, errors.New("ffmpeg: no executable")
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, err
	}
	if spec.Env == nil {
		spec.Env = ChildEnv(os.Environ(), runtime.GOOS)
	}
	if spec.Starter == nil {
		spec.Starter = ExecStarter{}
	}
	if spec.Redactor == nil {
		spec.Redactor = logx.NewRedactor()
	}
	if spec.TailLines <= 0 {
		spec.TailLines = DefaultTailLines
	}
	if spec.Clock == nil {
		spec.Clock = clock.Real()
	}
	t := &tail{n: spec.TailLines, red: spec.Redactor, log: spec.Log, name: spec.Name}
	stdout, stderr := t.writer("stdout", slog.LevelDebug), t.writer("stderr", slog.LevelInfo)

	startedMono, startedAt := spec.Clock.Mono(), spec.Clock.Now()
	proc, err := spec.Starter.Start(StartConfig{
		Exe: spec.Exe, Args: spec.Args, Dir: spec.Dir, Env: spec.Env, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("ffmpeg: cannot start %s: %w", spec.Exe, err)
	}
	c := &Child{proc: proc, clk: spec.Clock, started: startedMono, at: startedAt, tail: t, done: make(chan struct{})}
	go func() {
		code, werr := proc.Wait()
		c.info = ExitInfo{Code: code, RanFor: spec.Clock.Since(startedMono), Err: werr}
		stdout.flush()
		stderr.flush()
		c.info.LastLines = t.lines()
		cleanup()
		close(c.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			c.Kill()
		case <-c.done:
		}
	}()
	return c, nil
}

// Pid is the process id.
func (c *Child) Pid() int { return c.proc.Pid() }

// StartedAt is the wall time of the start.
func (c *Child) StartedAt() time.Time { return c.at }

// Done is closed when the process has exited and its output has been drained.
func (c *Child) Done() <-chan struct{} { return c.done }

// Wait blocks until the process has exited and returns the account of the run.
func (c *Child) Wait() ExitInfo {
	<-c.done
	return c.info
}

// Kill ends the process hard and at once. Safe after exit and more than once. Wait still reports
// the exit.
func (c *Child) Kill() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	return c.proc.Kill()
}

func onceFunc(f func()) func() {
	if f == nil {
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(f) }
}

// tail redacts lines and keeps the last n.
type tail struct {
	n    int
	red  *logx.Redactor
	log  *slog.Logger
	name string

	mu   sync.Mutex
	kept []string
}

func (t *tail) add(stream string, level slog.Level, line string) {
	line = t.red.String(line)
	t.mu.Lock()
	if len(t.kept) == t.n {
		copy(t.kept, t.kept[1:])
		t.kept = t.kept[:t.n-1]
	}
	t.kept = append(t.kept, line)
	t.mu.Unlock()
	if t.log != nil {
		t.log.Log(context.Background(), level, line, slog.String("camera", t.name), slog.String("stream", stream))
	}
}

func (t *tail) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.kept...)
}

// lineWriter splits a byte stream into lines on \r or \n (blank lines dropped). A multi-byte
// character split across two writes stays whole because only the ASCII separators cut.
type lineWriter struct {
	t      *tail
	stream string
	level  slog.Level
	mu     sync.Mutex
	buf    []byte
}

func (t *tail) writer(stream string, level slog.Level) *lineWriter {
	return &lineWriter{t: t, stream: stream, level: level}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' || b == '\r' {
			w.emit()
			continue
		}
		w.buf = append(w.buf, b)
		if len(w.buf) >= maxLineBytes {
			w.emit()
		}
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit()
}

func (w *lineWriter) emit() {
	if len(w.buf) == 0 {
		return
	}
	line := strings.ToValidUTF8(string(w.buf), "�")
	w.buf = w.buf[:0]
	if strings.TrimSpace(line) == "" {
		return
	}
	w.t.add(w.stream, w.level, line)
}
