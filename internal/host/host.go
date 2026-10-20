package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/status"
)

// Exit codes of the supervisor that are themselves the record (IS 1.5, supervise.ExitCode).
const (
	ExitAnotherSupervisor = 20 // another supervisor holds the archive: restarting cannot help
	ExitConfigFault       = 21 // the configuration cannot run: restarting cannot help
)

// Alert ids (Get-CamAlertEventIdList).
const (
	AlertStopped      = 100 // unexpected stop, being restarted (Error)
	AlertRecording    = 101 // recording again after a restart (Warning)
	AlertBudgetSpent  = 102 // stopped and NOT coming back (Error)
	AlertWindowClosed = 103 // the NVR window was closed, nothing is recording (Warning)
)

// Event is an alert in the shape of alerts.Event with string severity (adapter:
// alerts.FromFields(e.ID, e.Severity, e.Title, "", e.Message, e.Key)).
type Event struct {
	ID       int
	Severity string // Error | Warning
	Title    string
	Message  string
	Key      string
}

// Alerts takes the host's alerts; Raise must not block.
type Alerts interface{ Raise(Event) }

// Proc is a running supervisor process (osx.Proc satisfies it).
type Proc interface {
	Pid() int
	Wait() (code int, err error)
	Kill() error
}

// Spawner starts one supervisor process.
type Spawner interface {
	Spawn(ctx context.Context) (Proc, error)
}

// Locker probes the instance lock: Acquire succeeds only while no supervisor holds it
// (supervise.OSLock and FileLock satisfy it).
type Locker interface {
	Acquire() (release func(), owner string, err error)
}

// Console delivers the console's requests to end (osx.OnConsoleClose) and, in attached mode, the
// console probe (osx.WatchConsole).
type Console interface {
	OnConsoleClose(fn func(osx.Reason)) (stop func())
	WatchConsole(fn func(osx.Reason)) (stop func(), err error)
}

// Mode is how the host runs.
type Mode int

const (
	// Attached: in a terminal the Operator can close. A close (or the console's death) is a
	// deliberate stop: ordered stop within the OS budget, StopKind window-closed, alert 103, no restart.
	Attached Mode = iota
	// Service: under the OS service manager; its stop is a requested stop (no alert, no restart).
	Service
)

// Outcome is why Run returned.
type Outcome string

const (
	OutcomeRequested    Outcome = "requested"
	OutcomeInterrupted  Outcome = "interrupted"
	OutcomeWindowClosed Outcome = "window-closed"
	OutcomeAnother      Outcome = "another-supervisor"
	OutcomeConfigFault  Outcome = "config-fault"
	OutcomeBudgetSpent  Outcome = "budget-spent"
	OutcomeStopFailed   Outcome = "stop-failed" // the supervisor did not stop within StopTimeout (G7)
)

// Result is the account of Run.
type Result struct {
	Outcome  Outcome
	Reason   string
	Lives    int // supervisor processes started
	Restarts int // restarts after unexpected stops
}

// ExitCode is the host process's exit code: 0 for the deliberate stops, 20 and 21 passed through,
// 1 when it stays down after a fault or its stop failed.
func (r Result) ExitCode() int {
	switch r.Outcome {
	case OutcomeRequested, OutcomeInterrupted, OutcomeWindowClosed:
		return 0
	case OutcomeAnother:
		return ExitAnotherSupervisor
	case OutcomeConfigFault:
		return ExitConfigFault
	}
	return 1
}

// Config holds the policy (Start-CamNvrHost defaults).
type Config struct {
	StateDir           string        // <root>/_state: status.json, stop.request, supervisor.lock
	MaxRestarts        int           // 5
	RestartWindow      time.Duration // 30 min
	RestartDelay       time.Duration // 5 s
	LockWait           time.Duration // 20 s: the previous supervisor's lock
	AttachedStopBudget time.Duration // osx.ConsoleBudget: inside the 5 s Windows gives after a close
	StopTimeout        time.Duration // 40 s: service stop and Ctrl+C (the supervisor's own stop takes up to 30 s)
	StatusPoll         time.Duration // 1 s: how often a restarted supervisor's status is read for alert 101
	WindowTitle        string        // for LastWindowClose and alert 103
}

func (c *Config) defaults() {
	if c.MaxRestarts <= 0 {
		c.MaxRestarts = 5
	}
	if c.RestartWindow <= 0 {
		c.RestartWindow = 30 * time.Minute
	}
	if c.RestartDelay <= 0 {
		c.RestartDelay = 5 * time.Second
	}
	if c.LockWait <= 0 {
		c.LockWait = 20 * time.Second
	}
	if c.AttachedStopBudget <= 0 {
		c.AttachedStopBudget = osx.ConsoleBudget
	}
	if c.StopTimeout <= 0 {
		c.StopTimeout = 40 * time.Second
	}
	if c.StatusPoll <= 0 {
		c.StatusPoll = time.Second
	}
}

// Deps are the collaborators; nil fields get the production default where one exists.
type Deps struct {
	Spawner    Spawner                          // required
	Alerts     Alerts                           // default: none (the log line is always written)
	Clock      clock.Clock                      // default clock.Real()
	Log        *slog.Logger                     // default discard; component "host"
	Status     *status.Writer                   // default status.NewWriter(<state>/status.json)
	ReadStatus func() (*status.Document, error) // default status.Read(<state>/status.json)
	Lock       Locker                           // default FileLock on <state>/supervisor.lock
	Console    Console                          // default OSConsole{}
	Mode       Mode
	Location   *time.Location // status.json timestamps; default time.Local
}

// Host runs the supervisor as a child process and restarts it in place after an unexpected stop
// (Start-CamNvrHost, IS 2.2 G2 to G4). See README.md.
type Host struct {
	cfg Config
	d   Deps
	clk clock.Clock
	log *slog.Logger

	statusPath, stopPath string
}

// New makes a host; nothing runs until Run.
func New(cfg Config, d Deps) *Host {
	cfg.defaults()
	if d.Clock == nil {
		d.Clock = clock.Real()
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Location == nil {
		d.Location = time.Local
	}
	h := &Host{cfg: cfg, d: d, clk: d.Clock, log: d.Log,
		statusPath: filepath.Join(cfg.StateDir, "status.json"), stopPath: filepath.Join(cfg.StateDir, "stop.request")}
	if h.d.Status == nil {
		h.d.Status = status.NewWriter(h.statusPath, status.Options{})
	}
	if h.d.ReadStatus == nil {
		p := h.statusPath
		h.d.ReadStatus = func() (*status.Document, error) { return status.Read(p) }
	}
	if h.d.Lock == nil {
		h.d.Lock = FileLock{Path: osx.LockPath(cfg.StateDir)}
	}
	if h.d.Console == nil {
		h.d.Console = OSConsole{}
	}
	return h
}

type exitInfo struct {
	code int
	err  error
}

// stopKind is the classification of one supervisor exit.
type stopKind int

const (
	stopUnexpected stopKind = iota
	stopRequested
	stopInterrupted
	stopWindowClosed
	stopAnother
	stopConfigFault
)

// classification is decided from RECORDED state only: the stop-request file, the StopKind the
// supervisor persisted (only in a document this very process wrote: SupervisorPid matches, so a
// stale "requested" of an earlier run cannot hide a crash, the IS 1.5 edge), and the two exit
// codes that are themselves records (20, 21). Never from any other exit code.
type classification struct {
	kind     stopKind
	reason   string
	recorded bool // the supervisor wrote its own LastUnexpectedStop: the host writes nothing
}

func classify(pid, code int, ranFor time.Duration, stopFile bool, doc *status.Document) classification {
	own := doc != nil && doc.SupervisorPid == int64(pid) && doc.State == status.StateStopped
	kind := ""
	if own {
		kind = doc.StopKind
	}
	switch {
	case stopFile || kind == status.StopKindRequested:
		return classification{kind: stopRequested, reason: "stop requested"}
	case kind == status.StopKindInterrupted:
		return classification{kind: stopInterrupted, reason: "interrupted"}
	case kind == status.StopKindWindowClosed:
		return classification{kind: stopWindowClosed, reason: "the NVR window was closed"}
	case code == ExitAnotherSupervisor:
		return classification{kind: stopAnother, reason: "another supervisor holds the archive"}
	case code == ExitConfigFault:
		return classification{kind: stopConfigFault, reason: "configuration fault"}
	}
	reason := fmt.Sprintf("the supervisor (pid %d) stopped after %ds without being asked to (exit %d)", pid, int(ranFor.Seconds()), code)
	if kind == status.StopKindUnexpected && doc.LastUnexpectedStop.Stop != nil {
		return classification{kind: stopUnexpected, reason: reason + ": " + doc.LastUnexpectedStop.Stop.Reason, recorded: true}
	}
	return classification{kind: stopUnexpected, reason: reason}
}

func (h *Host) raise(e Event) {
	if h.d.Alerts != nil {
		h.d.Alerts.Raise(e)
	}
	level := slog.LevelWarn
	if e.Severity == "Error" {
		level = slog.LevelError
	}
	h.log.Log(context.Background(), level, fmt.Sprintf("ALERT %d: %s - %s", e.ID, e.Title, e.Message),
		logx.Action("host.alert"), logx.Actor("host"), logx.Target(fmt.Sprint(e.ID)), logx.Result(logx.ResultOK))
}

// Run starts the supervisor and keeps it running until a deliberate stop, a refusal or the end of
// the restart budget. ctx cancellation is a requested stop.
func (h *Host) Run(ctx context.Context) (Result, error) {
	if h.d.Spawner == nil {
		return Result{}, errors.New("host: no spawner")
	}
	events := make(chan osx.Reason, 8)
	deliver := func(r osx.Reason) {
		select {
		case events <- r:
		default:
		}
	}
	defer h.d.Console.OnConsoleClose(deliver)()
	if h.d.Mode == Attached {
		stop, err := h.d.Console.WatchConsole(deliver)
		if err != nil {
			h.log.Warn("console probe not armed: "+err.Error(), logx.Action("host.console"), logx.Actor("host"), logx.Result(logx.ResultFail))
		}
		defer stop()
	}
	h.log.Info(fmt.Sprintf("nvr host pid %d starting the supervisor; it will be restarted if it stops without being asked to", os.Getpid()),
		logx.Action("host.start"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultOK), logx.PID(os.Getpid()))

	var restarts []clock.Mono
	res := Result{}
	for {
		res.Lives++
		startedAt := h.clk.Mono()
		proc, err := h.d.Spawner.Spawn(ctx)
		var cls classification
		var pid int
		if err != nil {
			cls = classification{kind: stopUnexpected, reason: "the supervisor could not be started: " + err.Error(), recorded: true}
			h.log.Error(cls.reason, logx.Action("host.supervisor.start"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultFail), logx.Err(err))
		} else {
			pid = proc.Pid()
			h.log.Info(fmt.Sprintf("supervisor started: pid %d (life %d)", pid, res.Lives),
				logx.Action("host.supervisor.start"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultOK), logx.PID(pid))
			waitDone := make(chan exitInfo, 1)
			go func() {
				code, err := proc.Wait()
				waitDone <- exitInfo{code, err}
			}()
			ex, reason, stopped := h.watch(ctx, proc, waitDone, events, res.Lives > 1, len(restarts))
			if stopped {
				return h.orderedStop(ctx, proc, waitDone, reason, res)
			}
			ranFor := h.clk.Since(startedAt)
			_, statErr := os.Lstat(h.stopPath)
			doc, _ := h.d.ReadStatus()
			cls = classify(pid, ex.code, ranFor, statErr == nil, doc)
			h.log.Info(fmt.Sprintf("supervisor pid %d exited with code %d after %ds: %s", pid, ex.code, int(ranFor.Seconds()), cls.reason),
				logx.Action("host.supervisor.stop"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultOK), logx.PID(pid), logx.DurationMS(ranFor))
		}

		switch cls.kind {
		case stopRequested, stopInterrupted, stopWindowClosed:
			res.Outcome = map[stopKind]Outcome{stopRequested: OutcomeRequested, stopInterrupted: OutcomeInterrupted, stopWindowClosed: OutcomeWindowClosed}[cls.kind]
			res.Reason = cls.reason
			h.log.Info(fmt.Sprintf("the supervisor (pid %d) stopped as asked (%s). The NVR host is done.", pid, cls.reason),
				logx.Action("host.stop"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultOK), logx.PID(pid))
			return res, nil
		case stopAnother:
			res.Outcome, res.Reason = OutcomeAnother, cls.reason
			h.log.Error("the supervisor refused to start: another one is already running. Restarting would not help, so the NVR host is done.",
				logx.Action("host.stop"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultRefused), logx.PID(pid))
			return res, nil
		case stopConfigFault:
			res.Outcome, res.Reason = OutcomeConfigFault, cls.reason
			h.log.Error("the supervisor refused to start: its configuration cannot run (exit 21). Restarting would not help, so the NVR host is done.",
				logx.Action("host.stop"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultRefused), logx.PID(pid))
			// always, first life included: nothing is recording and nothing will restart it
			h.raise(Event{ID: AlertBudgetSpent, Severity: "Error", Title: "the NVR has stopped and is NOT coming back", Key: "102",
				Message: fmt.Sprintf("configuration fault: the supervisor cannot start (pid %d exited 21), so it stays down until the configuration is fixed. Nothing is recording.", pid)})
			return res, nil
		}

		// An unexpected stop.
		if pid != 0 {
			owner, free := h.waitLockAndRecord(ctx, pid, cls)
			if !free {
				res.Outcome = OutcomeAnother
				res.Reason = "after the unexpected stop the archive lock is held by " + owner
				h.log.Error(fmt.Sprintf("%s; %s, so it is not restarted", cls.reason, res.Reason),
					logx.Action("host.restart"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultRefused), logx.PID(pid))
				return res, nil
			}
		}
		now := h.clk.Mono()
		kept := restarts[:0]
		for _, at := range restarts {
			if now.Sub(at) < h.cfg.RestartWindow {
				kept = append(kept, at)
			}
		}
		restarts = kept
		if len(restarts) >= h.cfg.MaxRestarts {
			res.Outcome, res.Reason = OutcomeBudgetSpent, cls.reason
			h.log.Error(fmt.Sprintf("%s. Restart budget spent (%d in %v): staying down.", cls.reason, len(restarts), h.cfg.RestartWindow),
				logx.Action("host.restart"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultFail), logx.PID(pid))
			h.raise(Event{ID: AlertBudgetSpent, Severity: "Error", Title: "the NVR has stopped and is NOT coming back", Key: "102",
				Message: fmt.Sprintf("%s. It has already been restarted %d times in %d minutes, so it is not being restarted again. Nothing is recording.",
					cls.reason, len(restarts), int(h.cfg.RestartWindow.Minutes()))})
			return res, nil
		}
		h.raise(Event{ID: AlertStopped, Severity: "Error", Title: "the NVR stopped unexpectedly", Key: fmt.Sprintf("100:%d", res.Lives),
			Message: fmt.Sprintf("%s. MediaMTX and every ffmpeg stopped with it. Restarting it in %ds.", cls.reason, int(h.cfg.RestartDelay.Seconds()))})
		h.log.Warn(fmt.Sprintf("restarting the supervisor in %v (restart %d of %d allowed in %v)", h.cfg.RestartDelay, len(restarts)+1, h.cfg.MaxRestarts, h.cfg.RestartWindow),
			logx.Action("host.restart"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultOK), logx.PID(pid))
		select {
		case <-h.clk.After(h.cfg.RestartDelay):
		case r := <-events:
			return h.stopWhileDown(r, res)
		case <-ctx.Done():
			res.Outcome, res.Reason = OutcomeRequested, "the host was stopped while the supervisor was down"
			return res, nil
		}
		restarts = append(restarts, h.clk.Mono())
		res.Restarts++
	}
}

// watch waits for the supervisor's exit, a console event or ctx; on a restarted life it raises 101
// once the new supervisor's own document says it is running.
func (h *Host) watch(ctx context.Context, proc Proc, waitDone <-chan exitInfo, events <-chan osx.Reason, restarted bool, nRestarts int) (exitInfo, osx.Reason, bool) {
	pending101 := restarted
	for {
		var poll <-chan time.Time
		if pending101 {
			poll = h.clk.After(h.cfg.StatusPoll)
		}
		select {
		case ex := <-waitDone:
			return ex, "", false
		case r := <-events:
			return exitInfo{}, r, true
		case <-ctx.Done():
			return exitInfo{}, reasonHostStopped, true
		case <-poll:
			if doc, err := h.d.ReadStatus(); err == nil && doc.State == status.StateRunning && doc.SupervisorPid == int64(proc.Pid()) {
				pending101 = false
				h.raise(Event{ID: AlertRecording, Severity: "Warning", Title: "the NVR is recording again", Key: fmt.Sprintf("101:%d", proc.Pid()),
					Message: fmt.Sprintf("restarted the supervisor after an unexpected stop; this is restart %d of %d allowed in %d minutes.",
						nRestarts, h.cfg.MaxRestarts, int(h.cfg.RestartWindow.Minutes()))})
			}
		}
	}
}

// waitLockAndRecord waits up to LockWait for the dead supervisor's lock to be free (the OS frees it
// at once; a live holder means another supervisor) and, holding it, writes the unexpected-stop
// document when the supervisor could not.
func (h *Host) waitLockAndRecord(ctx context.Context, pid int, cls classification) (owner string, free bool) {
	deadline := h.clk.Mono() + clock.Mono(h.cfg.LockWait)
	for {
		release, o, err := h.d.Lock.Acquire()
		if err == nil {
			if !cls.recorded {
				h.recordUnexpected(pid, cls.reason)
			}
			release()
			return "", true
		}
		owner = o
		if h.clk.Mono() >= deadline {
			return owner, false
		}
		select {
		case <-h.clk.After(250 * time.Millisecond):
		case <-ctx.Done():
			return owner, false
		}
	}
}

func (h *Host) now() status.Time { return status.NewTime(h.clk.Now().In(h.d.Location)) }

func (h *Host) baseDoc(pid int) *status.Document {
	doc, err := h.d.ReadStatus()
	if err != nil || doc == nil {
		return &status.Document{Schema: 1, SupervisorPid: int64(pid)}
	}
	if pid != 0 {
		doc.SupervisorPid = int64(pid)
	}
	return doc
}

// recordUnexpected is Set-CamStatusUnexpectedStop: the last document patched to stopped/unexpected
// with a new LastUnexpectedStop (DetectedBy "nvr host"); everything else, LastWindowClose
// included, is carried as it was (D19).
func (h *Host) recordUnexpected(pid int, reason string) {
	doc := h.baseDoc(pid)
	at := h.now()
	doc.State, doc.StopKind, doc.UpdatedAt = status.StateStopped, status.StopKindUnexpected, at
	doc.LastUnexpectedStop = status.MaybeStop{Present: true, Stop: &status.UnexpectedStop{At: at, Reason: reason, SupervisorPid: int64(pid), DetectedBy: "nvr host"}}
	if err := h.d.Status.Write(doc); err != nil {
		h.log.Error("could not record the unexpected stop in status.json: "+err.Error(), logx.Action("host.status"), logx.Actor("host"), logx.Target(h.statusPath), logx.Result(logx.ResultFail), logx.Err(err))
		return
	}
	h.log.Info("recorded StopKind 'unexpected' in status.json (the supervisor could not)", logx.Action("host.status"), logx.Actor("host"), logx.Target(h.statusPath), logx.Result(logx.ResultOK), logx.PID(pid))
}

// recordWindowClosed is Set-CamStatusWindowClosed. The host detects the close itself (no warden
// process), so DetectedBy is "nvr host" (the schema accepts it besides the PowerShell "nvr warden").
func (h *Host) recordWindowClosed(pid int, reason string) {
	doc := h.baseDoc(pid)
	at := h.now()
	doc.State, doc.StopKind, doc.UpdatedAt = status.StateStopped, status.StopKindWindowClosed, at
	doc.LastWindowClose = &status.WindowClose{At: at, Reason: reason, WindowTitle: h.cfg.WindowTitle, DetectedBy: "nvr host"}
	if err := h.d.Status.Write(doc); err != nil {
		h.log.Error("could not record the window close in status.json: "+err.Error(), logx.Action("host.status"), logx.Actor("host"), logx.Target(h.statusPath), logx.Result(logx.ResultFail), logx.Err(err))
		return
	}
	h.log.Info("recorded the close in status.json as StopKind 'window-closed'", logx.Action("host.status"), logx.Actor("host"), logx.Target(h.statusPath), logx.Result(logx.ResultOK))
}

// reasonHostStopped: Run's ctx was cancelled (the caller stops the host: a requested stop).
const reasonHostStopped osx.Reason = "host-stopped"

// closeKind: in attached mode every console request to end except Ctrl+C is the window going
// (close, logoff, shutdown, hangup, the probe, SIGTERM at the end of a session).
func (h *Host) closeKind(r osx.Reason) bool {
	return h.d.Mode == Attached && r != osx.ReasonInterrupt && r != reasonHostStopped
}

// orderedStop: the host asks for the stop through stop.request and waits for the supervisor. After a
// window close the OS ends this process about 5 s after the event, so the wait is
// AttachedStopBudget and a supervisor still running at its end is killed (its job / Pdeathsig
// takes its children); otherwise (service stop, Ctrl+C, ctx) it waits StopTimeout and NEVER kills
// the supervisor (G7): it reports it.
func (h *Host) orderedStop(ctx context.Context, proc Proc, waitDone <-chan exitInfo, r osx.Reason, res Result) (Result, error) {
	closed := h.closeKind(r)
	budget := h.cfg.StopTimeout
	if closed {
		budget = h.cfg.AttachedStopBudget
	}
	t0 := h.clk.Mono()
	pid := proc.Pid()
	h.log.Info(fmt.Sprintf("console request %q: asking the supervisor (pid %d) to stop", r, pid),
		logx.Action("host.stop"), logx.Actor("host"), logx.Target("supervisor"), logx.PID(pid))
	if err := os.MkdirAll(h.cfg.StateDir, 0o700); err == nil {
		err = os.WriteFile(h.stopPath, []byte(fmt.Sprintf("requested by the nvr host pid %d: %s\n", os.Getpid(), r)), 0o600)
		if err != nil {
			h.log.Error("could not write the stop request: "+err.Error(), logx.Action("host.stop"), logx.Actor("host"), logx.Result(logx.ResultFail), logx.Err(err))
		}
	}
	reserve := time.Duration(0)
	if closed {
		reserve = budget / 8 // room for the final document after the wait
	}
	select {
	case <-waitDone:
	case <-h.clk.After(budget - reserve):
		if !closed {
			res.Outcome = OutcomeStopFailed
			res.Reason = fmt.Sprintf("the supervisor (pid %d) DID NOT STOP within %v", pid, budget)
			h.log.Error(res.Reason+"; the stop request is left in place and the supervisor is not killed",
				logx.Action("host.stop"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultFail), logx.PID(pid), logx.DurationMS(h.clk.Since(t0)))
			return res, errors.New(res.Reason)
		}
		h.log.Error(fmt.Sprintf("the supervisor (pid %d) did not stop within the console budget; killing it (its children die with it)", pid),
			logx.Action("host.stop"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultFail), logx.PID(pid))
		_ = proc.Kill()
		select {
		case <-waitDone:
		case <-h.clk.After(reserve / 2):
		}
	}
	h.log.Info(fmt.Sprintf("the supervisor (pid %d) stopped after the request", pid),
		logx.Action("host.stop"), logx.Actor("host"), logx.Target("supervisor"), logx.Result(logx.ResultOK), logx.PID(pid), logx.DurationMS(h.clk.Since(t0)))
	if !closed {
		res.Outcome, res.Reason = OutcomeRequested, "stop requested ("+string(r)+")"
		return res, nil
	}
	return h.windowClosed(pid, r, res), nil
}

// stopWhileDown: a console request while the supervisor is down after an unexpected stop: no
// restart; in attached mode it is still the window close.
func (h *Host) stopWhileDown(r osx.Reason, res Result) (Result, error) {
	if !h.closeKind(r) {
		res.Outcome, res.Reason = OutcomeRequested, "stop requested ("+string(r)+") while the supervisor was down"
		return res, nil
	}
	return h.windowClosed(0, r, res), nil
}

func (h *Host) windowClosed(pid int, r osx.Reason, res Result) Result {
	title := h.cfg.WindowTitle
	if title == "" {
		title = "holocron"
	}
	reason := fmt.Sprintf("the NVR window '%s' was closed (%s) at %s, so the supervisor, MediaMTX and every ffmpeg were stopped",
		title, r, h.clk.Now().In(h.d.Location).Format("15:04:05"))
	if release, owner, err := h.d.Lock.Acquire(); err == nil {
		h.recordWindowClosed(pid, reason)
		release()
	} else {
		h.log.Error("could not record the window close: the archive lock is held by "+owner, logx.Action("host.status"), logx.Actor("host"), logx.Result(logx.ResultRefused))
	}
	h.raise(Event{ID: AlertWindowClosed, Severity: "Warning", Title: "the NVR window was closed - nothing is recording", Key: "103",
		Message: reason + ". Nothing will restart it: run holocron up to bring it back."})
	res.Outcome, res.Reason = OutcomeWindowClosed, reason
	return res
}

// FileLock is the host's view of the supervisor's instance lock (osx.AcquireInstanceLock).
type FileLock struct{ Path string }

func (l FileLock) Acquire() (func(), string, error) {
	lk, err := osx.AcquireInstanceLock(l.Path)
	if err != nil {
		var he *osx.HeldError
		if errors.As(err, &he) {
			return nil, he.Owner.String(), err
		}
		return nil, "unknown (" + strings.TrimSpace(err.Error()) + ")", err
	}
	return func() { _ = lk.Release() }, lk.Owner().String(), nil
}

// OSConsole is the real Console.
type OSConsole struct{}

func (OSConsole) OnConsoleClose(fn func(osx.Reason)) func() { return osx.OnConsoleClose(fn) }
func (OSConsole) WatchConsole(fn func(osx.Reason)) (func(), error) {
	return osx.WatchConsole(fn, osx.ProbeInterval)
}
