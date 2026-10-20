package supervise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
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

// Stop kinds a caller may ask for (status.StopKind* values).
const (
	StopRequested   = status.StopKindRequested
	StopInterrupted = status.StopKindInterrupted
	StopUnexpected  = status.StopKindUnexpected
)

// Exit codes for cmd (IS 1.5, brief): 0 clean stop, 20 another supervisor, 21 configuration fault,
// 1 anything else (an unexpected stop included, so the host restarts it).
const (
	ExitOK                = 0
	ExitFailure           = 1
	ExitAnotherSupervisor = 20
	ExitConfigFault       = 21
)

// ErrAnotherSupervisor: the lock (or the fresh heartbeat of a live supervisor) says this archive
// already has one.
var ErrAnotherSupervisor = errors.New("another supervisor is running")

// AnotherSupervisorError names the holder.
type AnotherSupervisorError struct{ Owner string }

func (e *AnotherSupervisorError) Error() string {
	return fmt.Sprintf("a supervisor is already running (%s); refusing to start a second one - stop it first", e.Owner)
}
func (e *AnotherSupervisorError) Is(target error) bool { return target == ErrAnotherSupervisor }

// ConfigFault is a configuration the supervisor cannot run with (exit 21).
type ConfigFault struct{ Err error }

func (e *ConfigFault) Error() string { return "configuration fault: " + e.Err.Error() }
func (e *ConfigFault) Unwrap() error { return e.Err }

// ErrUnexpectedStop: the supervisor stopped on a fault (a recovered panic); StopKind=unexpected was
// recorded and the host should restart it.
var ErrUnexpectedStop = errors.New("the supervisor stopped unexpectedly")

// ExitCode maps Run's error to the process exit code.
func ExitCode(err error) int {
	var cf *ConfigFault
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrAnotherSupervisor):
		return ExitAnotherSupervisor
	case errors.As(err, &cf):
		return ExitConfigFault
	}
	return ExitFailure
}

// StopReport is the outcome of the ordered stop.
type StopReport struct {
	Kind      string // requested | interrupted | unexpected
	Reason    string
	Survivors []int // pids of children still alive after the stop wait; reported, never hidden
	Final     bool  // the final status document was written
}

// SupervisorDeps are the injected collaborators. Source, Lock and either Status or a writable
// RootPath are required; everything else has a production default.
type SupervisorDeps struct {
	Source ConfigSource
	Lock   InstanceLock
	// Status writes every document (atomic, redacted); default status.NewWriter(StatusPath).
	Status *status.Writer
	// StatusPath is status.json; default <RootPath>/_state/status.json.
	StatusPath string
	// ReadPrevious reads the previous document (carry-forward, second lock signal); default status.Read(StatusPath).
	ReadPrevious func() (*status.Document, error)
	// Processes judges the previous document's pid (second lock signal); default status.OSProcesses{}.
	Processes status.ProcessInspector

	Clock    clock.Clock
	Platform osx.Platform
	Starter  ffmpeg.Starter // default ffmpeg.ExecStarter{}
	Scope    ProcessScope   // default NoScope{}
	Scanner  RootScanner    // default ArchiveScanner for the configured container
	Pruner   Pruner         // default ArchivePruner over RootPath
	Alerts   Alerts         // default: none (the ALERT log line is always written)

	// Credentials hands out camera logins (relay source environment, direct input).
	Credentials relay.CredentialSource
	// APILogin and LiveLogin load the MediaMTX logins; defaults relay.LoadAPICredential and
	// relay.LoadLiveSecret in the platform's auth folder (created when missing).
	APILogin  func() (relay.Login, error)
	LiveLogin func() (relay.Login, error)
	// NewRelayAPI builds the control-API client; default relay.NewAPI.
	NewRelayAPI func(cfg config.Config, login relay.Login) RelayAPI

	// Logs gives the component loggers (supervisor, relay, live, prune); default discard.
	Logs LogSource
	// ChildLog gives the output log of one child (component, file path); default FileChildLogs.
	ChildLog func(component, path string) *slog.Logger
	// Redactor masks child output; every secret the supervisor learns is registered on it, on the
	// status writer's redactor and on each of Redactors.
	Redactor  *logx.Redactor
	Redactors []Registrar
	// Events feeds status.json Events; wire the same ring into the process logger's sink.
	Events *EventRing

	Pid      int64          // default os.Getpid()
	Location *time.Location // status.json timestamps; default time.Local
}

// Supervisor is the recorder: it owns the camera children (through the Manager), the relay and the
// live-view MediaMTX, the heartbeat, the pruner schedule and the ordered stop. See README.md for the
// goroutines and channels.
type Supervisor struct {
	d   SupervisorDeps
	clk clock.Clock
	cfg config.Config

	log, rlog, llog, plog *slog.Logger

	m       *Manager
	starter ffmpeg.Starter
	profile config.ContainerProfile
	writer  *status.Writer
	prev    *status.Document
	onDoc   func(*status.Document) // test hook: every document as built, before the write

	startedWall time.Time
	startedMono clock.Mono

	stopFile string

	ctx    context.Context // cancelled at the start of the ordered stop
	cancel context.CancelFunc
	stopCh chan struct{} // closed at the start of the ordered stop

	reqCh   chan stopRequest
	faultCh chan string
	startCh chan camStarted
	exitCh  chan camExited

	startWG sync.WaitGroup

	camsReady chan struct{}
	camsOnce  sync.Once

	runOnce sync.Once
	running chan struct{} // closed when Run has started the goroutines
	done    chan struct{} // closed when Run has finished

	relayM, liveM *mtxChild
	watch         relay.LoginWatch

	mu        sync.Mutex
	stopping  bool
	registry  map[*Entry]*ffmpeg.Child
	rows      []status.Camera
	cameras   []config.Camera
	archive   *ArchiveScan
	pruneView status.Prune
	api       RelayAPI
	fault     string
	report    StopReport
}

type stopRequest struct{ kind, reason string }

type camStarted struct {
	e     *Entry
	child *ffmpeg.Child
	err   error
}

type camExited struct {
	e    *Entry
	info ffmpeg.ExitInfo
}

// NewSupervisor makes a supervisor; nothing runs until Run.
func NewSupervisor(d SupervisorDeps) *Supervisor {
	if d.Clock == nil {
		d.Clock = clock.Real()
	}
	if d.Platform == nil {
		d.Platform = osx.Current()
	}
	if d.Starter == nil {
		d.Starter = ffmpeg.ExecStarter{}
	}
	if d.Scope == nil {
		d.Scope = NoScope{}
	}
	if d.Logs == nil {
		d.Logs = discardLogs{}
	}
	if d.Redactor == nil {
		d.Redactor = logx.NewRedactor()
	}
	if d.Events == nil {
		d.Events = NewEventRing()
	}
	if d.Pid == 0 {
		d.Pid = int64(os.Getpid())
	}
	if d.Location == nil {
		d.Location = time.Local
	}
	if d.Processes == nil {
		d.Processes = status.OSProcesses{}
	}
	s := &Supervisor{
		d: d, clk: d.Clock,
		log: d.Logs.For("supervisor"), rlog: d.Logs.For("relay"), llog: d.Logs.For("live"), plog: d.Logs.For("prune"),
		reqCh: make(chan stopRequest, 4), faultCh: make(chan string, 4),
		startCh: make(chan camStarted, 64), exitCh: make(chan camExited, 64),
		camsReady: make(chan struct{}), running: make(chan struct{}), done: make(chan struct{}),
		stopCh: make(chan struct{}), registry: map[*Entry]*ffmpeg.Child{},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

// Stop asks for the ordered stop with the given kind (requested, interrupted or unexpected) and
// waits until it is done. Safe from any goroutine; a stop already under way keeps its kind.
func (s *Supervisor) Stop(kind string) StopReport {
	reason := map[string]string{StopRequested: "stop requested", StopInterrupted: "supervisor shutdown",
		StopUnexpected: "the control path stopped the supervisor as unexpected"}[kind]
	if reason == "" {
		kind, reason = StopRequested, "stop requested"
	}
	select {
	case s.reqCh <- stopRequest{kind, reason}:
	default:
	}
	select {
	case <-s.running:
		<-s.done
	case <-s.done:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report
}

// Done is closed when Run has returned.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

// Run is the supervisor (cam.recorder.ps1 -Run, R:3708-4197). It returns after the ordered stop:
// nil for requested and interrupted stops, ErrUnexpectedStop after a recovered fault, an
// *AnotherSupervisorError (exit 20) or a *ConfigFault (exit 21) when it could not start. ctx
// cancellation is an interrupted stop (a signal).
func (s *Supervisor) Run(ctx context.Context) (StopReport, error) {
	started := false
	defer func() {
		if !started {
			close(s.done)
		}
	}()
	var once bool
	s.runOnce.Do(func() { once = true })
	if !once {
		return StopReport{}, errors.New("supervisor: Run called twice")
	}
	if s.d.Source == nil || s.d.Lock == nil {
		return StopReport{}, &ConfigFault{errors.New("the supervisor needs a configuration source and a lock")}
	}
	cfg, err := s.d.Source.Config()
	if err != nil {
		return StopReport{}, &ConfigFault{err}
	}
	if err := s.setup(cfg); err != nil {
		return StopReport{}, err
	}

	release, owner, err := s.d.Lock.Acquire()
	if err != nil {
		s.log.Error(fmt.Sprintf("A supervisor is already running: %s. Refusing to start a second one - use stop first.", owner),
			logx.Action("supervisor.start"), logx.Result(logx.ResultRefused), logx.Err(err))
		return StopReport{}, &AnotherSupervisorError{Owner: owner}
	}
	defer release()

	prev, _ := s.d.ReadPrevious()
	if running, reason := status.Verdict(prev, s.clk.Now(), s.d.Processes, status.VerdictConfig{StatusIntervalSeconds: cfg.StatusIntervalSeconds}); running && prev.SupervisorPid != s.d.Pid {
		owner := fmt.Sprintf("pid %d per status.json: %s", prev.SupervisorPid, reason)
		s.log.Error(fmt.Sprintf("A supervisor is already running: %s. Refusing to start a second one - use stop first.", owner),
			logx.Action("supervisor.start"), logx.Result(logx.ResultRefused))
		return StopReport{}, &AnotherSupervisorError{Owner: owner}
	}
	s.prev = prev

	if _, err := os.Lstat(s.stopFile); err == nil {
		if os.Remove(s.stopFile) == nil {
			s.log.Info("cleared a leftover stop request from an earlier run", logx.Action("supervisor.start"))
		}
	}

	starter, err := s.d.Scope.Open(s.d.Starter)
	if err != nil {
		s.log.Warn(fmt.Sprintf("could not create the process scope: %v. Recording continues, but ffmpeg and MediaMTX would OUTLIVE a crash of this process.", err),
			logx.Action("supervisor.scope"), logx.Result(logx.ResultFail), logx.Err(err))
		starter = s.d.Starter
	}
	s.starter = starter
	defer func() {
		if err := s.d.Scope.Close(); err != nil {
			s.log.Warn("closing the process scope failed", logx.Err(err))
		}
	}()

	s.startedWall, s.startedMono = s.clk.Now(), s.clk.Mono()
	s.claim()
	s.m = New(SettingsFrom(cfg), Deps{Clock: s.clk, Scanner: s.d.Scanner, Alerts: s.d.Alerts, Log: s.log, Tick: time.Second, Location: s.d.Location})
	s.log.Info(fmt.Sprintf("supervisor starting: pid %d, container %s, segments %ds, root %s, reconcile %gs, reserve %g GB",
		s.d.Pid, cfg.Container, cfg.SegmentSeconds, cfg.RootPath, cfg.ReconcileSeconds, cfg.ReserveGB),
		logx.Action("supervisor.start"), logx.Result(logx.ResultOK), logx.PID(int(s.d.Pid)))
	st := SettingsFrom(cfg)
	if lim, conf := DownAlertSeconds(st), cfg.CameraDownAlertMinutes*60; lim > conf {
		s.log.Info(fmt.Sprintf("camera-down alert 110: configured %g min, effective %.1f min (%d s) - a healthy camera is up to one segment (%d s) plus the keyframe wait and a status tick past its newest file, so a lower limit would alert on it",
			cfg.CameraDownAlertMinutes, lim/60, roundInt(lim), cfg.SegmentSeconds))
	}

	started = true
	go s.supervise(ctx)
	<-s.done
	s.mu.Lock()
	rep, fault := s.report, s.fault
	s.mu.Unlock()
	if rep.Kind == StopUnexpected {
		if fault == "" {
			fault = rep.Reason
		}
		return rep, fmt.Errorf("%w: %s", ErrUnexpectedStop, fault)
	}
	return rep, nil
}

// setup validates the configuration and fills the defaults that depend on it.
func (s *Supervisor) setup(cfg config.Config) error {
	if cfg.RootPath == "" {
		return &ConfigFault{errors.New("RootPath is empty")}
	}
	prof, ok := cfg.Profile()
	if !ok {
		return &ConfigFault{fmt.Errorf("Container %q has no entry in ContainerProfiles", cfg.Container)}
	}
	if cfg.SegmentSeconds <= 0 || cfg.StatusIntervalSeconds <= 0 || cfg.ReconcileSeconds <= 0 || cfg.PruneIntervalSeconds <= 0 {
		return &ConfigFault{errors.New("SegmentSeconds, StatusIntervalSeconds, ReconcileSeconds and PruneIntervalSeconds must be positive")}
	}
	s.cfg, s.profile = cfg, prof
	d := &s.d
	if d.StatusPath == "" {
		d.StatusPath = filepath.Join(cfg.RootPath, "_state", "status.json")
	}
	if d.Status == nil {
		d.Status = status.NewWriter(d.StatusPath, status.Options{})
	}
	if d.ReadPrevious == nil {
		path := d.StatusPath
		d.ReadPrevious = func() (*status.Document, error) { return status.Read(path) }
	}
	if d.Scanner == nil {
		d.Scanner = ArchiveScanner{Matcher: archive.MatcherFor(d.Platform), Container: prof.Extension, SegmentSeconds: cfg.SegmentSeconds}
	}
	if d.Pruner == nil {
		d.Pruner = ArchivePruner{Root: cfg.RootPath, Platform: d.Platform, Rules: prune.RulesFrom(cfg), Container: prof.Extension, Log: s.plog}
	}
	if d.APILogin == nil {
		p := d.Platform
		d.APILogin = func() (relay.Login, error) {
			return relay.LoadAPICredential(p, relay.DefaultAPICredentialPath(p), true)
		}
	}
	if d.LiveLogin == nil {
		p, c := d.Platform, cfg
		d.LiveLogin = func() (relay.Login, error) { return relay.LoadLiveSecret(p, relay.LiveSecretPath(p, c), true) }
	}
	if d.NewRelayAPI == nil {
		policy := relay.NewReadinessPolicy(cfg)
		d.NewRelayAPI = func(c config.Config, l relay.Login) RelayAPI {
			return relay.NewAPI(c.RelayApiBase, l, policy.ProbeTimeout)
		}
	}
	if d.ChildLog == nil {
		d.ChildLog = FileChildLogs(int64(cfg.LogMaxBytes), d.Redactor)
	}
	s.writer = d.Status
	s.stopFile = filepath.Join(cfg.RootPath, "_state", "stop.request")
	if cfg.RestreamerEnabled {
		s.relayM = &mtxChild{s: s, name: "restreamer", label: "restreamer", log: s.rlog, logPath: filepath.Join(cfg.RootPath, "_logs", "restreamer.log")}
	}
	if cfg.LiveViewEnabled {
		s.liveM = &mtxChild{s: s, name: "liveview", label: "live view", live: true, log: s.llog, logPath: filepath.Join(cfg.RootPath, "_logs", "liveview.log")}
	}
	s.pruneView = status.Prune{}
	return nil
}

// claim is the ownership-marker claim at start (Initialize-CamArchiveOwner); it never stops
// recording: a root that cannot be claimed leaves pruning disabled, which the first prune run reports.
func (s *Supervisor) claim() {
	res, err := s.d.Pruner.Claim(s.clk.Now())
	switch {
	case err != nil:
		s.plog.Warn(fmt.Sprintf("WARNING: could not claim %s as this recorder's archive, so pruning stays disabled: %v", s.cfg.RootPath, err),
			logx.Action("archive.claim"), logx.Target(s.cfg.RootPath), logx.Result(logx.ResultFail), logx.Err(err))
	case res.Created:
		s.plog.Info(fmt.Sprintf("claimed %s as this recorder's archive: wrote %s", s.cfg.RootPath, res.Path),
			logx.Action("archive.claim"), logx.Target(s.cfg.RootPath), logx.Result(logx.ResultOK))
	case !res.Owned:
		s.plog.Warn(fmt.Sprintf("WARNING: did not claim %s as this recorder's archive, so pruning stays disabled: %s", s.cfg.RootPath, res.Problem),
			logx.Action("archive.claim"), logx.Target(s.cfg.RootPath), logx.Result(logx.ResultRefused))
	}
}

// register masks a secret everywhere the supervisor writes.
func (s *Supervisor) register(secret string) {
	if secret == "" {
		return
	}
	s.d.Redactor.Register(secret)
	s.writer.Redactor().Register(secret)
	for _, r := range s.d.Redactors {
		r.Register(secret)
	}
}

// credentials wraps the credential source so every login handed out is registered for redaction.
type credentials struct{ s *Supervisor }

func (c credentials) CameraCredential(cam config.Camera) (string, string, error) {
	if c.s.d.Credentials == nil {
		return "", "", errors.New("no camera credential source is configured")
	}
	u, p, err := c.s.d.Credentials.CameraCredential(cam)
	if err == nil {
		c.s.register(u)
		c.s.register(p)
	}
	return u, p, err
}

// recoverFault turns a panic of a supervisor goroutine into an unexpected stop.
func (s *Supervisor) recoverFault(where string) {
	if r := recover(); r != nil {
		s.raiseFault(fmt.Sprintf("%s failed: %v", where, r))
	}
}

func (s *Supervisor) raiseFault(reason string) {
	s.mu.Lock()
	if s.fault == "" {
		s.fault = reason
	}
	s.mu.Unlock()
	s.log.Error("supervisor fault: "+reason, logx.Action("supervisor.fault"), logx.Result(logx.ResultFail))
	select {
	case s.faultCh <- reason:
	default:
	}
}

func (s *Supervisor) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// supervise starts the goroutines, waits for the reason to stop and runs the ordered stop.
func (s *Supervisor) supervise(ctx context.Context) {
	defer close(s.done)
	hbCtx, hbCancel := context.WithCancel(context.Background())
	hbDone := make(chan struct{})
	go s.heartbeat(hbCtx, hbDone)

	loopQuit, loopDone := make(chan struct{}), make(chan struct{})
	if s.relayM != nil {
		s.relayM.starting, s.relayM.startedMono = true, s.startedMono
	}
	go s.loop(loopQuit, loopDone)
	if s.relayM != nil {
		s.relayM.start()
	}
	if s.liveM != nil {
		s.liveM.start()
	}
	pruneDone := make(chan struct{})
	go s.pruneLoop(pruneDone)
	close(s.running)

	var req stopRequest
	select {
	case <-ctx.Done():
		req = stopRequest{StopInterrupted, "supervisor shutdown"}
	case req = <-s.reqCh:
	case reason := <-s.faultCh:
		req = stopRequest{StopUnexpected, "the supervisor loop failed: " + reason}
	}
	// a fault that raced a request wins: it is what happened
	s.mu.Lock()
	if s.fault != "" && req.kind != StopUnexpected {
		req = stopRequest{StopUnexpected, "the supervisor loop failed: " + s.fault}
	}
	s.mu.Unlock()
	s.teardown(req, loopQuit, loopDone, pruneDone, hbCancel, hbDone)
}

// requestStop is the loop's way to stop (the stop-request file).
func (s *Supervisor) requestStop(kind, reason string) {
	select {
	case s.reqCh <- stopRequest{kind, reason}:
	default:
	}
}

// publishCameras stores the latest camera list for the MediaMTX starts.
func (s *Supervisor) publishCameras(cams []config.Camera) {
	s.mu.Lock()
	s.cameras = append([]config.Camera(nil), cams...)
	s.mu.Unlock()
	s.camsOnce.Do(func() { close(s.camsReady) })
}

func (s *Supervisor) camerasNow() []config.Camera {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]config.Camera(nil), s.cameras...)
}

func sameConfig(a, b config.Config) bool { return reflect.DeepEqual(a, b) }
