package supervise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/clock"
	"holocron/internal/config"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/prune"
	"holocron/internal/relay"
	"holocron/internal/status"
)

// TestMain: every prune path in this package's tests is held inside the OS temp dir.
func TestMain(m *testing.M) {
	os.Setenv(prune.SandboxEnv, "1")
	os.Exit(m.Run())
}

const (
	fakeCamUser = "camuser-fake"
	fakeCamPass = "campass-NOT-A-SECRET-77"
	fakeAPIPass = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6"
)

// ---- fake processes ----

type behaviour struct {
	startErr   error
	block      time.Duration // Start blocks this long on the fake clock
	exitAfter  time.Duration // >0: exits on its own after this long
	code       int
	ignoreKill bool
}

type fakeProc struct {
	pid        int
	kind, name string // kind ffmpeg | relay | live; name: camera safe name for ffmpeg
	cfg        ffmpeg.StartConfig
	startedAt  clock.Mono
	ignoreKill bool
	exit       chan int
	once       sync.Once
	gone       atomic.Bool
	st         *fakeStarter
}

func (p *fakeProc) Pid() int { return p.pid }
func (p *fakeProc) Wait() (int, error) {
	code := <-p.exit
	return code, nil
}
func (p *fakeProc) Kill() error {
	p.st.mu.Lock()
	p.st.kills = append(p.st.kills, p)
	p.st.mu.Unlock()
	if !p.ignoreKill {
		p.finish(1)
	}
	return nil
}
func (p *fakeProc) finish(code int) {
	p.once.Do(func() {
		p.gone.Store(true)
		p.exit <- code
	})
}

func (p *fakeProc) exited() bool { return p.gone.Load() }

type fakeStarter struct {
	clk    *clock.Fake
	cfg    func() config.Config
	script func(kind, name string, n int) behaviour

	mu     sync.Mutex
	nextID int
	procs  []*fakeProc
	kills  []*fakeProc
	counts map[string]int
}

func (f *fakeStarter) kindOf(c ffmpeg.StartConfig) (kind, name string) {
	cfg := f.cfg()
	switch {
	case c.Exe == cfg.FfmpegPath:
		out := c.Args[len(c.Args)-1]
		return "ffmpeg", filepath.Base(filepath.Dir(out))
	case len(c.Args) > 0 && c.Args[0] == cfg.RestreamerConfigPath:
		return "relay", "restreamer"
	}
	return "live", "liveview"
}

func (f *fakeStarter) Start(c ffmpeg.StartConfig) (ffmpeg.Process, error) {
	kind, name := f.kindOf(c)
	f.mu.Lock()
	n := f.counts[kind+":"+name]
	f.counts[kind+":"+name] = n + 1
	f.mu.Unlock()
	var b behaviour
	if f.script != nil {
		b = f.script(kind, name, n)
	}
	if b.block > 0 {
		f.clk.Sleep(b.block)
	}
	if b.startErr != nil {
		return nil, b.startErr
	}
	f.mu.Lock()
	f.nextID++
	p := &fakeProc{pid: 1000 + f.nextID, kind: kind, name: name, cfg: c, startedAt: f.clk.Mono(), ignoreKill: b.ignoreKill, exit: make(chan int, 1), st: f}
	f.procs = append(f.procs, p)
	f.mu.Unlock()
	if b.exitAfter > 0 {
		go func() { <-f.clk.After(b.exitAfter); p.finish(b.code) }()
	}
	return p, nil
}

func (f *fakeStarter) started(kind string) []*fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeProc
	for _, p := range f.procs {
		if kind == "" || p.kind == kind {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeStarter) of(kind, name string) []*fakeProc {
	var out []*fakeProc
	for _, p := range f.started(kind) {
		if p.name == name {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeStarter) killOrder() []*fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[*fakeProc]bool{}
	var out []*fakeProc
	for _, p := range f.kills {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeStarter) alive() []*fakeProc {
	var out []*fakeProc
	for _, p := range f.started("") {
		if !p.exited() {
			out = append(out, p)
		}
	}
	return out
}

// ---- fake relay API ----

type fakeAPI struct {
	clk *clock.Fake

	mu        sync.Mutex
	set       relay.ReadySet
	readyN    int
	waitBlock time.Duration
	waits     int
}

func (a *fakeAPI) setReady(s relay.ReadySet) { a.mu.Lock(); a.set = s; a.mu.Unlock() }

func (a *fakeAPI) Ready(ctx context.Context) (relay.ReadySet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.readyN++
	if a.set.State == relay.Unavailable {
		return a.set, relay.ErrUnavailable
	}
	return a.set, nil
}

func (a *fakeAPI) WaitStarted(ctx context.Context, p relay.ReadinessPolicy, exited <-chan struct{}) (relay.ReadySet, bool) {
	a.mu.Lock()
	block := a.waitBlock
	a.waits++
	a.mu.Unlock()
	if block > 0 {
		select {
		case <-a.clk.After(block):
		case <-ctx.Done():
			return relay.ReadySet{}, false
		case <-exited:
			return relay.ReadySet{}, false
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.set, a.set.State != relay.Unavailable
}

// ---- fake config source ----

type fakeSource struct {
	mu   sync.Mutex
	cfg  config.Config
	cams []config.Camera
	err  error
}

func (f *fakeSource) Config() (config.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, nil
}
func (f *fakeSource) Cameras() ([]config.Camera, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]config.Camera(nil), f.cams...), f.err
}
func (f *fakeSource) setCameras(c []config.Camera) {
	f.mu.Lock()
	f.cams = append([]config.Camera(nil), c...)
	f.mu.Unlock()
}

// ---- fake scanner (per-camera and archive-wide) ----

type syncScanner struct {
	mu      sync.Mutex
	scans   map[string]archive.DirScan // by safe name
	calls   int
	panicAt int // panic on this call number (0: never)
}

func (f *syncScanner) Scan(dir string, now time.Time) (archive.DirScan, error) {
	f.mu.Lock()
	f.calls++
	n, p := f.calls, f.panicAt
	s, ok := f.scans[filepath.Base(dir)]
	f.mu.Unlock()
	if p > 0 && n == p {
		panic("fake scanner exploded")
	}
	if !ok {
		return archive.DirScan{Dir: dir, ScannedAt: now}, nil
	}
	s.Dir, s.ScannedAt = dir, now
	return s, nil
}

func (f *syncScanner) ScanRoot(root string, now time.Time) (ArchiveScan, error) {
	return ArchiveScan{ScannedAt: now, Dirs: map[string]archive.DirScan{}}, nil
}

func (f *syncScanner) set(safe string, s archive.DirScan) {
	f.mu.Lock()
	f.scans[safe] = s
	f.mu.Unlock()
}

// ---- fake pruner ----

type fakePruner struct {
	clk *clock.Fake

	mu     sync.Mutex
	runs   []clock.Mono
	claims int
	result func(n int) prune.Result
}

func (f *fakePruner) Claim(time.Time) (archive.ClaimResult, error) {
	f.mu.Lock()
	f.claims++
	f.mu.Unlock()
	return archive.ClaimResult{Owned: true}, nil
}

func (f *fakePruner) Run(now time.Time, st *prune.State) (prune.Result, error) {
	f.mu.Lock()
	f.runs = append(f.runs, f.clk.Mono())
	n := len(f.runs)
	r := f.result
	f.mu.Unlock()
	if r == nil {
		return prune.Result{}, nil
	}
	return r(n), nil
}

func (f *fakePruner) runTimes() []clock.Mono {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]clock.Mono(nil), f.runs...)
}

// ---- alerts ----

type syncAlerts struct {
	mu     sync.Mutex
	events []Event
	panics bool
}

func (a *syncAlerts) Raise(e Event) {
	a.mu.Lock()
	a.events = append(a.events, e)
	p := a.panics
	a.mu.Unlock()
	if p {
		panic("alert channel exploded")
	}
}

func (a *syncAlerts) ids() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []int
	for _, e := range a.events {
		out = append(out, e.ID)
	}
	return out
}

func (a *syncAlerts) count(id int) int {
	n := 0
	for _, x := range a.ids() {
		if x == id {
			n++
		}
	}
	return n
}

type fixedCreds struct{}

func (fixedCreds) CameraCredential(config.Camera) (string, string, error) {
	return fakeCamUser, fakeCamPass, nil
}

// freePlatform is an osx.Fake whose free space a test can move while the supervisor runs.
type freePlatform struct {
	*osx.Fake
	free atomic.Uint64
}

func (p *freePlatform) DiskFree(string) (uint64, error) { return p.free.Load(), nil }

// ---- the rig ----

type srig struct {
	t     *testing.T
	clk   *clock.Fake
	root  string
	cfg   config.Config
	src   *fakeSource
	mv    *fakeMover
	st    *fakeStarter
	api   *fakeAPI
	pr    *fakePruner
	al    *syncAlerts
	sc    *syncScanner
	lock  *MemoryLock
	logs  *logx.MemorySink
	ring  *EventRing
	plat  *freePlatform
	deps  SupervisorDeps
	sup   *Supervisor
	runCh chan runResult

	mu   sync.Mutex
	docs []*status.Document
	raw  [][]byte
}

type runResult struct {
	rep StopReport
	err error
}

var tStart = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func sixCameras() []config.Camera {
	var out []config.Camera
	for i, n := range []string{"Front", "Back", "Garage", "Garden", "Side", "Street"} {
		out = append(out, config.Camera{Name: n, IP: fmt.Sprintf("192.0.2.%d", 10+i), Quality: "stream1", Record: true})
	}
	return out
}

// newSRig builds a supervisor over fakes in a t.TempDir() root; mod adjusts the configuration.
func newSRig(t *testing.T, cams []config.Camera, mod func(*config.Config)) *srig {
	t.Helper()
	root := t.TempDir()
	plat := &freePlatform{Fake: &osx.Fake{OS: runtime.GOOS}}
	plat.free.Store(500 << 30)
	cfg := config.Defaults(plat)
	cfg.RootPath = root
	cfg.FfmpegPath = "ffmpeg-fake"
	cfg.RestreamerExePath = "mediamtx-fake"
	cfg.RestreamerConfigPath = filepath.Join(root, "_mtx", "mediamtx.yml")
	cfg.LiveConfigPath = filepath.Join(root, "_mtx", "mediamtx-live.yml")
	cfg.SegmentSeconds = 60
	cfg.PruneIntervalSeconds = 300
	if mod != nil {
		mod(&cfg)
	}
	clk := clock.NewFake(tStart)
	r := &srig{t: t, clk: clk, root: root, cfg: cfg, plat: plat, runCh: make(chan runResult, 1)}
	r.src = &fakeSource{cfg: cfg, cams: cams}
	r.st = &fakeStarter{clk: clk, cfg: func() config.Config { return cfg }, counts: map[string]int{}}
	r.api = &fakeAPI{clk: clk}
	paths := []string{}
	for _, c := range cams {
		if p, err := relay.PathName(c.Name); err == nil {
			paths = append(paths, p)
		}
	}
	r.api.set = relay.NewReadySet(paths)
	r.pr = &fakePruner{clk: clk}
	r.mv = &fakeMover{clk: clk}
	r.al = &syncAlerts{}
	r.sc = &syncScanner{scans: map[string]archive.DirScan{}}
	r.lock = &MemoryLock{Owner: "test-supervisor"}
	r.logs = logx.NewMemorySink()
	r.ring = NewEventRing()
	lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "debug"}, Getenv: func(string) string { return "" },
		Sink: logx.FanOut(r.logs, r.ring), Now: clk.Now, Location: time.UTC})
	lg.Redactor().Register(fakeCamPass)
	r.deps = SupervisorDeps{
		Source: r.src, Lock: r.lock, Clock: clk, Platform: plat, Starter: r.st, Scanner: r.sc, Pruner: r.pr, Mover: r.mv, Alerts: r.al,
		Credentials: fixedCreds{},
		APILogin:    func() (relay.Login, error) { return relay.Login{User: relay.APIUser, Secret: fakeAPIPass}, nil },
		LiveLogin: func() (relay.Login, error) {
			return relay.Login{User: relay.LiveUser, Secret: strings.Repeat("ef", 32)}, nil
		},
		NewRelayAPI: func(config.Config, relay.Login) RelayAPI { return r.api },
		Logs:        lg, ChildLog: func(string, string) *slog.Logger { return discardLogger() },
		Redactor: lg.Redactor(), Events: r.ring, Pid: 4242, Location: time.UTC,
		Processes: status.FakeProcesses{},
		CheckFfmpeg: func(string) (ffmpeg.Version, error) {
			return ffmpeg.Version{Raw: "7.1.1", Major: 7, Minor: 1, Patch: 1}, nil
		},
	}
	return r
}

func (r *srig) build() *Supervisor {
	r.sup = NewSupervisor(r.deps)
	r.sup.onDoc = func(d *status.Document) {
		b, err := status.Marshal(d)
		if err != nil {
			r.t.Errorf("marshal: %v", err)
		}
		r.mu.Lock()
		r.docs = append(r.docs, d)
		r.raw = append(r.raw, b)
		r.mu.Unlock()
	}
	return r.sup
}

// start runs the supervisor and waits until its goroutines are up.
func (r *srig) start() {
	r.t.Helper()
	if r.sup == nil {
		r.build()
	}
	go func() {
		rep, err := r.sup.Run(context.Background())
		r.runCh <- runResult{rep, err}
	}()
	select {
	case <-r.sup.running:
	case <-r.sup.done:
	case <-time.After(5 * time.Second):
		r.t.Fatal("the supervisor did not start")
	}
	r.settle()
}

// settle waits (real time) until every goroutine of the test binary is blocked (none runnable,
// running or in a system call, judged from the goroutine dump) and no report is queued: only then is
// the next clock step taken, so a goroutine woken by the fake clock acts at the time it was woken.
func (r *srig) settle() {
	deadline := time.Now().Add(5 * time.Second)
	calm := 0
	for time.Now().Before(deadline) {
		runtime.Gosched()
		quiet := r.sup == nil || (len(r.sup.startCh) == 0 && len(r.sup.exitCh) == 0)
		if quiet && quiescent() {
			if calm++; calm >= 2 {
				return
			}
			continue
		}
		calm = 0
		time.Sleep(50 * time.Microsecond)
	}
}

var goroutineHdr = regexp.MustCompile(`(?m)^goroutine \d+ \[([^\]]*)\]:`)

var stackBuf = make([]byte, 4<<20)
var stackMu sync.Mutex

func quiescent() bool {
	stackMu.Lock()
	defer stackMu.Unlock()
	n := runtime.Stack(stackBuf, true)
	running := 0
	for _, m := range goroutineHdr.FindAllSubmatch(stackBuf[:n], -1) {
		st := string(m[1])
		switch {
		case strings.HasPrefix(st, "running"):
			running++
		case strings.HasPrefix(st, "runnable"), strings.HasPrefix(st, "syscall"), strings.HasPrefix(st, "preempted"):
			return false
		}
	}
	return running <= 1
}

// step moves the fake clock by d in increments of at most 100 ms, settling after each.
func (r *srig) step(d time.Duration) {
	for d > 0 {
		inc := min(d, 100*time.Millisecond)
		r.clk.Advance(inc)
		r.settle()
		d -= inc
	}
}

// stepUntil steps until cond holds or max virtual time passed.
func (r *srig) stepUntil(max time.Duration, cond func() bool) bool {
	for t := time.Duration(0); t <= max; t += 100 * time.Millisecond {
		if cond() {
			return true
		}
		r.step(100 * time.Millisecond)
	}
	return cond()
}

// stop asks for the ordered stop and moves the clock until Run has returned.
func (r *srig) stop(kind string) runResult {
	r.t.Helper()
	go r.sup.Stop(kind)
	return r.finish()
}

// finish moves the clock until Run returns (at most 120 virtual seconds).
func (r *srig) finish() runResult {
	r.t.Helper()
	for i := 0; i < 1200; i++ {
		select {
		case res := <-r.runCh:
			r.runCh <- res
			return res
		default:
		}
		r.step(100 * time.Millisecond)
	}
	r.t.Fatal("Run did not return within 120 virtual seconds")
	return runResult{}
}

func (r *srig) lastDoc() *status.Document {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.docs) == 0 {
		return nil
	}
	return r.docs[len(r.docs)-1]
}

func (r *srig) docCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.docs)
}

// validateAll checks every document built (and the file on disk) against the schema.
func (r *srig) validateAll() {
	r.t.Helper()
	r.mu.Lock()
	raw := append([][]byte(nil), r.raw...)
	r.mu.Unlock()
	if len(raw) == 0 {
		r.t.Fatal("no document was written")
	}
	for i, b := range raw {
		vs, err := status.Validate(b)
		if err != nil || len(vs) != 0 {
			r.t.Fatalf("document %d does not validate: %v %v\n%s", i, err, vs, b)
		}
	}
	disk, err := os.ReadFile(filepath.Join(r.root, "_state", "status.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	if vs, err := status.Validate(disk); err != nil || len(vs) != 0 {
		r.t.Fatalf("status.json on disk does not validate: %v %v", err, vs)
	}
	if _, err := status.Read(filepath.Join(r.root, "_state", "status.json")); err != nil {
		r.t.Fatalf("status.Read: %v", err)
	}
}

func (r *srig) logText() string { return r.logs.String() }

// docNow builds the document from the state published right now (the heartbeat's view).
func (r *srig) docNow() *status.Document {
	return r.sup.document(status.StateRunning, status.StopKindNone, nil)
}

func (r *srig) row(name string) status.Camera {
	r.t.Helper()
	d := r.docNow()
	for _, c := range d.Cameras {
		if c.Name == name {
			return c
		}
	}
	r.t.Fatalf("no row for %s", name)
	return status.Camera{}
}

var _ = errors.New
var _ io.Writer = (*EventRing)(nil)
