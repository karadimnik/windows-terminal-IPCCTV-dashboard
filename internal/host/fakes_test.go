package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/status"
)

// ---- a fake supervisor process ----

type fakeLife struct {
	pid       int
	spawnedAt time.Time
	exitCh    chan int
	once      sync.Once
	killed    atomic.Bool
	killedAt  atomic.Int64 // virtual unix nanoseconds
	h         *harness
}

func (l *fakeLife) Pid() int { return l.pid }
func (l *fakeLife) Wait() (int, error) {
	return <-l.exitCh, nil
}
func (l *fakeLife) Kill() error {
	l.killedAt.CompareAndSwap(0, l.h.clk.Now().UnixNano())
	l.killed.Store(true)
	l.exit(1)
	return nil
}
func (l *fakeLife) exit(code int) { l.once.Do(func() { l.exitCh <- code }) }

// running writes this life's heartbeat document (the golden first write, this pid).
func (l *fakeLife) running() {
	d := l.h.goldenRunning()
	d.SupervisorPid = int64(l.pid)
	l.h.writeDoc(d)
}

// stopped writes the supervisor's own final document and exits.
func (l *fakeLife) stopped(kind string, code int) {
	d := l.h.goldenRunning()
	d.SupervisorPid = int64(l.pid)
	d.State, d.StopKind = status.StateStopped, kind
	if kind == status.StopKindUnexpected {
		d.LastUnexpectedStop = status.MaybeStop{Present: true, Stop: &status.UnexpectedStop{At: status.NewTime(l.h.clk.Now()), Reason: "panic in the loop", SupervisorPid: int64(l.pid), DetectedBy: "supervisor"}}
	}
	l.h.writeDoc(d)
	l.exit(code)
}

// honourStop: like the supervisor, notice stop.request, write requested, remove it, exit 0.
func (l *fakeLife) honourStop() {
	go func() {
		for {
			select {
			case <-time.After(2 * time.Millisecond):
			case <-l.h.done:
				return
			}
			if _, err := os.Stat(l.h.stopPath()); err == nil {
				l.stopped(status.StopKindRequested, 0)
				os.Remove(l.h.stopPath())
				return
			}
		}
	}()
}

// ---- harness ----

type harness struct {
	t       *testing.T
	clk     *clock.Fake
	state   string
	cfg     Config
	alerts  *fakeAlerts
	lock    *fakeLock
	console *fakeConsole
	logs    *logx.MemorySink
	done    chan struct{}

	mu       sync.Mutex
	lives    []*fakeLife
	onSpawn  func(*fakeLife)
	spawnErr error

	res    Result
	err    error
	finish chan struct{}
}

func newHarness(t *testing.T, mode Mode) *harness {
	h := &harness{t: t, clk: clock.NewFake(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)), state: filepath.Join(t.TempDir(), "_state"),
		alerts: &fakeAlerts{}, lock: &fakeLock{}, console: &fakeConsole{}, logs: logx.NewMemorySink(), done: make(chan struct{}), finish: make(chan struct{})}
	h.cfg = Config{StateDir: h.state, WindowTitle: "holocron"}
	h.console.mode = mode
	t.Cleanup(func() { close(h.done) })
	os.MkdirAll(h.state, 0o700)
	return h
}

func (h *harness) stopPath() string   { return filepath.Join(h.state, "stop.request") }
func (h *harness) statusPath() string { return filepath.Join(h.state, "status.json") }

func (h *harness) goldenRunning() *status.Document {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "status", "running-first-write.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	d, err := status.Parse(b)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) writeDoc(d *status.Document) {
	if err := status.NewWriter(h.statusPath(), status.Options{}).Write(d); err != nil {
		h.t.Error(err)
	}
}

func (h *harness) Spawn(ctx context.Context) (Proc, error) {
	h.mu.Lock()
	if h.spawnErr != nil {
		err := h.spawnErr
		h.mu.Unlock()
		return nil, err
	}
	l := &fakeLife{pid: 5000 + len(h.lives), spawnedAt: h.clk.Now(), exitCh: make(chan int, 1), h: h}
	h.lives = append(h.lives, l)
	on := h.onSpawn
	h.mu.Unlock()
	if on != nil {
		on(l)
	}
	return l, nil
}

func (h *harness) start() {
	lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "debug"}, Getenv: func(string) string { return "" }, Sink: h.logs})
	host := New(h.cfg, Deps{Spawner: h, Alerts: h.alerts, Clock: h.clk, Log: lg.For("host"), Lock: h.lock, Console: h.console, Mode: h.console.mode, Location: time.UTC})
	go func() {
		h.res, h.err = host.Run(context.Background())
		close(h.finish)
	}()
}

func (h *harness) life(n int) *fakeLife {
	h.t.Helper()
	h.eventually("life "+strconv.Itoa(n), func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.lives) >= n })
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lives[n-1]
}

func (h *harness) nLives() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.lives) }

// eventually advances the fake clock in steps of 100 ms until cond holds (bounded in real time).
func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	h.eventuallyStep(what, 100*time.Millisecond, cond)
}

func (h *harness) eventuallyStep(what string, step time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s (virtual %v); alerts %v; log:\n%s", what, h.clk.Now(), h.alerts.ids(), h.logs.String())
		}
		time.Sleep(time.Millisecond) // the host's real file I/O must not let virtual time run far ahead
		h.clk.Advance(step)
	}
}

// wait returns Run's result.
func (h *harness) wait() (Result, error) {
	h.t.Helper()
	h.eventually("Run to return", func() bool {
		select {
		case <-h.finish:
			return true
		default:
			return false
		}
	})
	return h.res, h.err
}

func (h *harness) doc() *status.Document {
	d, err := status.Read(h.statusPath())
	if err != nil {
		h.t.Fatalf("status.json: %v", err)
	}
	return d
}

// validDoc checks the file on disk against the schema and returns its top-level keys.
func (h *harness) validDoc() []string {
	h.t.Helper()
	raw, err := os.ReadFile(h.statusPath())
	if err != nil {
		h.t.Fatal(err)
	}
	v, err := status.Validate(raw)
	if err != nil || len(v) > 0 {
		h.t.Fatalf("status.json invalid: %v %v\n%s", err, v, raw)
	}
	return keysOf(h.t, raw)
}

func keysOf(t *testing.T, raw []byte) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func goldenKeys(t *testing.T, name string) []string {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "status", name))
	if err != nil {
		t.Fatal(err)
	}
	return keysOf(t, raw)
}

// ---- alerts, lock, console ----

type fakeAlerts struct {
	mu sync.Mutex
	ev []Event
}

func (a *fakeAlerts) Raise(e Event) { a.mu.Lock(); a.ev = append(a.ev, e); a.mu.Unlock() }
func (a *fakeAlerts) ids() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []int
	for _, e := range a.ev {
		out = append(out, e.ID)
	}
	return out
}
func (a *fakeAlerts) all() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Event(nil), a.ev...)
}

type fakeLock struct {
	mu     sync.Mutex
	holder string
	acqs   int
}

func (l *fakeLock) Acquire() (func(), string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" {
		return nil, l.holder, errors.New("held")
	}
	l.holder = "the nvr host"
	l.acqs++
	return func() { l.mu.Lock(); l.holder = ""; l.mu.Unlock() }, "the nvr host", nil
}
func (l *fakeLock) hold(owner string) { l.mu.Lock(); l.holder = owner; l.mu.Unlock() }
func (l *fakeLock) acquisitions() int { l.mu.Lock(); defer l.mu.Unlock(); return l.acqs }

type fakeConsole struct {
	mu       sync.Mutex
	mode     Mode
	fns      []func(osx.Reason)
	probeErr error
}

func (c *fakeConsole) OnConsoleClose(fn func(osx.Reason)) func() {
	c.mu.Lock()
	c.fns = append(c.fns, fn)
	c.mu.Unlock()
	return func() {}
}
func (c *fakeConsole) WatchConsole(fn func(osx.Reason)) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probeErr != nil {
		return func() {}, c.probeErr
	}
	c.fns = append(c.fns, fn)
	return func() {}, nil
}
func (c *fakeConsole) trigger(r osx.Reason) {
	c.mu.Lock()
	var f func(osx.Reason)
	if len(c.fns) > 0 {
		f = c.fns[0] // every registration feeds the same channel
	}
	c.mu.Unlock()
	f(r)
}
func (c *fakeConsole) armed() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.fns) > 0 }
