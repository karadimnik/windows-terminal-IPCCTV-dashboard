package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"holocron/internal/ctlapi"
	"holocron/internal/status"
)

// ---------------------------------------------------------------------------
// The Controller seam (LLD 2.11). The web app changes the running system only through a
// Controller: PowerShellController (the old system: cam.ps1 and cam.recorder.ps1, schtasks) or
// APIController (the Go supervisor's control API). Status stays a file read in both.
// ---------------------------------------------------------------------------

// ActionResult is what a control action reports (the shape of one PowerShell run, which the API
// controller fills the same way so the page and its tests see one response format).
type ActionResult = psResult

// ControllerState is the controller's own view of the supervisor.
type ControllerState struct {
	Running bool
	Pid     int64
	Detail  string
}

// Controller starts, stops and toggles the recorder.
type Controller interface {
	// Name is "powershell" or "api".
	Name() string
	Status(ctx context.Context) (ControllerState, error)
	// Start and Stop take a scope: all, recorder or dashboard.
	Start(ctx context.Context, scope string) ActionResult
	Stop(ctx context.Context, scope string) ActionResult
	SetRecord(ctx context.Context, camera string, enabled bool) ActionResult
	// TaskStates is what the page's "Tasks" strip shows.
	TaskStates() map[string]TaskInfo
}

// ---------------------------------------------------------------------------
// PowerShellController: today's behaviour, unchanged.
// ---------------------------------------------------------------------------

// PowerShellController runs the repository's scripts under Windows PowerShell and asks
// schtasks.exe for the task states.
type PowerShellController struct {
	run   func(timeout time.Duration, script string, args ...string) psResult
	tasks func() map[string]TaskInfo
}

func (*PowerShellController) Name() string { return "powershell" }

// Status: the PowerShell system has no endpoint to ask; liveness is the status file's verdict.
func (*PowerShellController) Status(context.Context) (ControllerState, error) {
	return ControllerState{Detail: "the status file decides (no control endpoint)"}, nil
}

func (c *PowerShellController) control(scope, flag string) ActionResult {
	var scopeFlag []string
	switch scope {
	case "recorder":
		scopeFlag = []string{"-Recorder"}
	case "dashboard":
		scopeFlag = []string{"-Dashboard"}
	}
	return c.run(controlTimeout, "cam.ps1", append([]string{flag}, scopeFlag...)...)
}

func (c *PowerShellController) Start(_ context.Context, scope string) ActionResult {
	return c.control(scope, "-Start")
}
func (c *PowerShellController) Stop(_ context.Context, scope string) ActionResult {
	return c.control(scope, "-Stop")
}

func (c *PowerShellController) SetRecord(_ context.Context, camera string, enabled bool) ActionResult {
	flag := "-Disable"
	if enabled {
		flag = "-Enable"
	}
	return c.run(recordToggleTimeout, "cam.recorder.ps1", flag, camera)
}

func (c *PowerShellController) TaskStates() map[string]TaskInfo { return c.tasks() }

// ---------------------------------------------------------------------------
// APIController: the Go supervisor's control API.
// ---------------------------------------------------------------------------

// apiClient is the part of *ctlapi.Client the controller uses.
type apiClient interface {
	State(ctx context.Context) (ctlapi.State, error)
	Stop(ctx context.Context, kind string) (ctlapi.StopReport, error)
	SetRecord(ctx context.Context, camera string, enabled bool) (ctlapi.Changed, error)
}

// HolocronLaunch lets the page's Start button run `holocron up --detach`: the web role of
// holocron passes its own executable and the global flags it was started with.
type HolocronLaunch struct {
	Exe  string
	Args []string // --root / --config ... placed before "up"
}

// APIController drives a Go supervisor through its control endpoint. With Stopped set (the
// supervisor does not answer, and this is the holocron web role) it still answers the page: Start
// can run `holocron up --detach`, the rest say that nothing is running.
type APIController struct {
	Root    string
	Dial    func(root string) (apiClient, error) // default ctlapi.Dial
	Launch  *HolocronLaunch
	Stopped bool

	mu      sync.Mutex
	seenAt  time.Time // the last answer of the endpoint, reused by TaskStates for 2 s
	seen    ControllerState
	tasksAt time.Time
	tasks   map[string]TaskInfo
}

func (*APIController) Name() string { return "api" }

func dialCtl(root string) (apiClient, error) {
	c, err := ctlapi.Dial(root)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *APIController) client() (apiClient, error) {
	if c.Dial != nil {
		return c.Dial(c.Root)
	}
	return dialCtl(c.Root)
}

func (c *APIController) Status(ctx context.Context) (ControllerState, error) {
	cl, err := c.client()
	if err != nil {
		return ControllerState{Detail: err.Error()}, err
	}
	st, err := cl.State(ctx)
	if err != nil {
		return ControllerState{Detail: err.Error()}, err
	}
	state := ControllerState{Running: st.Running, Pid: st.Pid, Detail: fmt.Sprintf("%d camera(s)", len(st.Cameras))}
	c.mu.Lock()
	c.seen, c.seenAt = state, time.Now()
	c.mu.Unlock()
	return state, nil
}

func conflict(command, msg string) ActionResult {
	return ActionResult{Command: command, ExitCode: 1, HTTP: http.StatusConflict, Err: errors.New(msg)}
}

const startHint = "the Go system is not running: start it with holocron up (or your service manager)"

func (c *APIController) Start(ctx context.Context, scope string) ActionResult {
	const command = "holocron up --detach"
	if scope == "dashboard" {
		return conflict(command, "the web app is part of holocron up; it cannot be started on its own from its own page")
	}
	if !c.Stopped {
		return conflict(command, "the supervisor is already running (the control endpoint answers)")
	}
	if c.Launch == nil || c.Launch.Exe == "" {
		return conflict(command, startHint)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, c.Launch.Exe, append(append([]string{}, c.Launch.Args...), "up", "--detach")...)
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	res := ActionResult{Command: command, Output: scrubLines(out.String())}
	err := cmd.Run()
	res.Output = scrubLines(out.String())
	res.Duration = time.Since(started)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Err = err
		}
	}
	return res
}

func (c *APIController) Stop(ctx context.Context, scope string) ActionResult {
	const command = "POST /v1/stop"
	if scope == "dashboard" {
		return conflict(command, "the web app stops together with the rest (holocron down); it cannot stop itself from its own page")
	}
	cl, err := c.client()
	if err != nil {
		return conflict(command, "nothing is running: the supervisor's control endpoint is not there")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	rep, err := cl.Stop(ctx, status.StopKindRequested)
	res := ActionResult{Command: command, Duration: time.Since(started)}
	if err != nil {
		return apiFailure(res, err)
	}
	res.Output = []string{fmt.Sprintf("stopped: %s (%s)", rep.Kind, ScrubText(rep.Reason))}
	if len(rep.Survivors) > 0 {
		res.Output = append(res.Output, fmt.Sprintf("surviving children: %v", rep.Survivors))
	}
	return res
}

func (c *APIController) SetRecord(ctx context.Context, camera string, enabled bool) ActionResult {
	command := "POST /v1/cameras/{name}/record"
	cl, err := c.client()
	if err != nil {
		return conflict(command, "the supervisor is not running: change cameras.json with holocron cam enable|disable")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, recordToggleTimeout)
	defer cancel()
	ch, err := cl.SetRecord(ctx, camera, enabled)
	res := ActionResult{Command: command, Duration: time.Since(started)}
	if err != nil {
		return apiFailure(res, err)
	}
	state := map[bool]string{true: "ON", false: "off"}[ch.Enabled]
	if ch.Changed {
		res.Output = []string{fmt.Sprintf("%s: record %s", ScrubText(ch.Target), state)}
	} else {
		res.Output = []string{fmt.Sprintf("%s: already %s", ScrubText(ch.Target), state)}
	}
	return res
}

// apiFailure maps a control-API error onto the response: busy and not-running are conflicts, a
// refusal is 422, anything else a bad gateway.
func apiFailure(res ActionResult, err error) ActionResult {
	res.ExitCode = 1
	res.Err = err
	switch {
	case errors.Is(err, ctlapi.ErrBusy):
		res.HTTP = http.StatusConflict
	case errors.Is(err, ctlapi.ErrNotRunning), errors.Is(err, ctlapi.ErrUnreachable):
		res.HTTP = http.StatusConflict
		res.Err = fmt.Errorf("the supervisor is not running (%v)", err)
	case errors.Is(err, ctlapi.ErrNotFound):
		res.HTTP = http.StatusNotFound
	}
	return res
}

// TaskStates: there are no Scheduled Tasks in the Go system; the page's two chips show the
// supervisor and the web app themselves.
func (c *APIController) TaskStates() map[string]TaskInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tasks != nil && time.Since(c.tasksAt) < 2*time.Second {
		return c.tasks
	}
	rec := TaskInfo{Name: "holocron supervisor", Exists: true, Status: "Stopped", Enabled: "Enabled"}
	st, fresh := c.seen, time.Since(c.seenAt) < 2*time.Second
	if !fresh {
		c.mu.Unlock() // Status takes the lock
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var err error
		st, err = c.Status(ctx)
		cancel()
		c.mu.Lock()
		fresh = err == nil
	}
	if fresh && st.Running {
		rec.Status = "Running"
	}
	c.tasks = map[string]TaskInfo{
		"recorder":  rec,
		"dashboard": {Name: "holocron web", Exists: true, Status: "Running", Enabled: "Enabled"},
	}
	c.tasksAt = time.Now()
	return c.tasks
}

// ---------------------------------------------------------------------------
// Choosing per request
// ---------------------------------------------------------------------------

// controllerTTL is how long the choice is kept.
const controllerTTL = 5 * time.Second

// probeTimeout is how long the control endpoint gets to answer before the PowerShell controller is
// used instead.
const probeTimeout = time.Second

// controller returns the Controller for this moment: an explicit one (Options.Controller), else the
// API controller when <root>/_state/ctl.addr exists and answers within 1 s, else (the holocron web
// role) the API controller in its "stopped" form, else PowerShell. The decision is cached.
func (a *API) controller() Controller {
	if a.ctlFixed != nil {
		return a.ctlFixed
	}
	a.pickMu.Lock()
	defer a.pickMu.Unlock()
	if a.picked != nil && time.Since(a.pickedAt) < controllerTTL {
		return a.picked
	}
	api := &APIController{Root: a.cfg.ArchiveRoot, Dial: a.dialCtl, Launch: a.launch}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	_, err := api.Status(ctx)
	cancel()
	var chosen Controller
	switch {
	case err == nil:
		chosen = api
	case a.goSystem:
		api.Stopped = true
		chosen = api
	default:
		chosen = a.ps()
	}
	if a.picked == nil || a.picked.Name() != chosen.Name() {
		a.log.Info("control path: "+chosen.Name(), "component", "web", "reason", describeChoice(err, a.goSystem))
	}
	a.picked, a.pickedAt = chosen, time.Now()
	return chosen
}

func describeChoice(err error, goSystem bool) string {
	switch {
	case err == nil:
		return "the supervisor's control endpoint answers"
	case goSystem:
		return "holocron web role, no supervisor answering: " + strings.TrimSpace(err.Error())
	}
	return "no control endpoint answering: " + strings.TrimSpace(err.Error())
}
