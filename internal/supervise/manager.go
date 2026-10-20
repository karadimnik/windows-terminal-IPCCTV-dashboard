package supervise

import (
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"sort"
	"time"

	"holocron/internal/archive"
	"holocron/internal/clock"
	"holocron/internal/config"
	"holocron/internal/logx"
	"holocron/internal/relay"
)

// Scanner returns the directory scan of one camera folder (archive.Scan in production).
type Scanner interface {
	Scan(dir string, now time.Time) (archive.DirScan, error)
}

// Alerts receives the alerts this package raises (only 110 today).
type Alerts interface {
	Raise(Event)
}

// Event is one alert.
type Event struct {
	ID       int
	Severity string // "Warning", "Error"
	Title    string
	Camera   string
	Message  string
	Key      string // dedupe key, "110:<SafeName>"
}

// Ready is the shared ready set of one cycle; relay.ReadySet satisfies it (Denied: every path is ready).
type Ready interface {
	Has(path string) bool
}

var _ Ready = relay.ReadySet{}

// Input is the input a child records from.
type Input string

const (
	InputNone   Input = ""
	InputRelay  Input = "relay"
	InputDirect Input = "direct"
)

// Kill reasons.
const (
	ReasonStalled     = "stalled"
	ReasonMoveToRelay = "moving to the relay"
	ReasonStop        = "stop"
)

// Start reasons.
const (
	ReasonStart       = "start"
	ReasonHoldExpired = "the relay did not serve the path in time"
)

// ActionKind is what the caller must do.
type ActionKind int

const (
	ActStart ActionKind = iota + 1
	ActKill
)

func (k ActionKind) String() string {
	switch k {
	case ActStart:
		return "start"
	case ActKill:
		return "kill"
	}
	return "?"
}

// Action is one instruction from Plan. A Start is to be performed Delay after the Plan call
// (the non-blocking stagger); a Kill at once. Every action must be answered: Start with Started or
// StartFailed, Kill with Exited once the process has gone.
type Action struct {
	Kind   ActionKind
	Entry  *Entry
	Input  Input // Start only
	Delay  time.Duration
	Reason string
}

// Cleanup asks the caller to run the empty-segment cleanup (ffmpeg.RemoveEmptySegment) on Dir.
type Cleanup struct {
	SafeName string
	Dir      string
}

// Deps are the injected collaborators.
type Deps struct {
	Clock   clock.Clock
	Scanner Scanner
	Alerts  Alerts
	Log     *slog.Logger  // component "supervisor"
	Tick    time.Duration // the caller's Plan interval; default 1 s (blind-gap rule)
}

// Settings are the configuration values the manager uses (IR 3.1).
type Settings struct {
	RootPath          string
	SegmentSeconds    float64
	StallFactor       float64
	BackoffStart      int
	BackoffMax        int
	HealthyReset      float64
	RelayEnabled      bool
	RelayReadyWait    time.Duration
	StartStagger      time.Duration
	StatusInterval    float64
	DownAlertMinutes  float64
	DownRepeatHours   float64
	RestartRingCap    int
	BlindGapFactor    float64
	BlindIntervalsCap int
}

// SettingsFrom reads Settings from a validated configuration. Negative RelayReadyWaitSeconds and
// StartStaggerMs are raised to 0 (R:3729-3734).
func SettingsFrom(c config.Config) Settings {
	return Settings{
		RootPath:          c.RootPath,
		SegmentSeconds:    float64(c.SegmentSeconds),
		StallFactor:       c.StallFactor,
		BackoffStart:      c.BackoffStartSeconds,
		BackoffMax:        c.BackoffMaxSeconds,
		HealthyReset:      float64(c.HealthyResetSeconds),
		RelayEnabled:      c.RestreamerEnabled,
		RelayReadyWait:    time.Duration(max(c.RelayReadyWaitSeconds, 0)) * time.Second,
		StartStagger:      time.Duration(max(c.StartStaggerMs, 0)) * time.Millisecond,
		StatusInterval:    c.StatusIntervalSeconds,
		DownAlertMinutes:  c.CameraDownAlertMinutes,
		DownRepeatHours:   c.CameraDownRepeatHours,
		RestartRingCap:    4096,
		BlindGapFactor:    3,
		BlindIntervalsCap: 64,
	}
}

type pendingKind int

const (
	pendNone pendingKind = iota
	pendStart
	pendKill
)

// Entry is one recording camera's child. Fields are read through the methods; only the Manager
// changes them.
type Entry struct {
	cam       config.Camera
	safe      string
	dir       string
	relayPath string

	createdWall time.Time

	running     bool
	pid         int64
	lastPid     int64
	input       Input
	startedWall time.Time
	startedMono clock.Mono
	firstStart  time.Time

	pending      pendingKind
	pendingInput Input
	killReason   string
	killError    string  // LastError text of a stall kill
	killRanFor   float64 // ranFor at the stall decision
	heldSince    bool

	backoff      int
	nextWall     time.Time
	nextMono     clock.Mono
	restartCount int64
	restarts     []clock.Mono
	lastError    string
	lastErrorAt  time.Time

	retiring bool

	scan          archive.DirScan
	hasScan       bool
	openFirstSeen time.Time

	downSince       time.Time
	downAlerted     bool
	downAlertedMono clock.Mono
}

func (e *Entry) Name() string          { return e.cam.Name }
func (e *Entry) SafeName() string      { return e.safe }
func (e *Entry) Dir() string           { return e.dir }
func (e *Entry) Camera() config.Camera { return e.cam }
func (e *Entry) RelayPath() string     { return e.relayPath }
func (e *Entry) Running() bool         { return e.running }
func (e *Entry) Pid() int64            { return e.pid }

type blindInterval struct {
	fromWall, toWall time.Time
	fromMono, toMono clock.Mono
}

// Manager is the per-camera child lifecycle state machine. It has no goroutines and does no I/O
// except through Deps; it is not safe for concurrent use (the supervisor loop owns it).
type Manager struct {
	s   Settings
	d   Deps
	log *slog.Logger

	startedWall time.Time // supervisor start: alert-110 floor

	cams       []config.Camera
	configErrs []string
	entries    map[string]*Entry
	retiring   []*Entry
	reconciled bool
	shutdown   bool

	tracker *archive.Tracker

	ticked       bool
	lastTickWall time.Time
	lastTickMono clock.Mono
	blind        []blindInterval

	slotSet  bool
	lastSlot clock.Mono
}

// New makes a Manager; the supervisor start is the clock's current time.
func New(s Settings, d Deps) *Manager {
	if d.Tick <= 0 {
		d.Tick = time.Second
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if s.RestartRingCap <= 0 {
		s.RestartRingCap = 4096
	}
	if s.BlindGapFactor <= 0 {
		s.BlindGapFactor = 3
	}
	if s.BlindIntervalsCap <= 0 {
		s.BlindIntervalsCap = 64
	}
	return &Manager{s: s, d: d, log: d.Log, startedWall: d.Clock.Now(), entries: map[string]*Entry{}, tracker: archive.NewTracker()}
}

func (m *Manager) now() (time.Time, clock.Mono) { return m.d.Clock.Now(), m.d.Clock.Mono() }

// Entry returns the current (not retiring) entry of a safe name, or nil.
func (m *Manager) Entry(safe string) *Entry { return m.entries[safe] }

// Active reports how many children are running or have an unanswered Start (teardown waits for 0).
func (m *Manager) Active() int {
	n := 0
	for _, e := range m.all() {
		if e.running || e.pending == pendStart {
			n++
		}
	}
	return n
}

// sorted returns the current entries in safe-name order.
func (m *Manager) sorted() []*Entry {
	out := make([]*Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].safe < out[j].safe })
	return out
}

func (m *Manager) all() []*Entry { return append(m.sorted(), m.retiring...) }

// Reconcile makes the entry set match the camera list (Invoke-CamReconcile, R:3631-3707): a child
// exists only for Record == true and a usable, non-colliding safe name; an IP or Quality change
// recreates the entry and loses its counters (IR V12, kept). Stops are handed out by Plan as Kill
// with ReasonStop and never touch backoff or restart counts.
func (m *Manager) Reconcile(cams []config.Camera) {
	if m.shutdown {
		return
	}
	nowWall, nowMono := m.now()
	m.cams = append([]config.Camera(nil), cams...)
	m.configErrs = make([]string, len(cams))
	safes := make([]string, len(cams))
	for i, c := range cams {
		safe, err := c.SafeName()
		if err != nil {
			m.configErrs[i] = err.Error()
			continue
		}
		safes[i] = safe
	}
	list := config.Cameras{List: m.cams}
	for _, col := range list.Collisions() {
		first := col.Indexes[0]
		for _, i := range col.Indexes[1:] {
			m.configErrs[i] = fmt.Sprintf("the archive folder '%s' is already used by camera '%s'", col.Folder, cams[first].Name)
		}
	}
	enabled := map[string]config.Camera{}
	var order []string
	for i, c := range cams {
		if m.configErrs[i] != "" {
			m.log.Warn(fmt.Sprintf("skipping %s: %s", c.Name, m.configErrs[i]), logx.Action("camera.reconcile"), logx.Target(c.Name), logx.Result("refused"))
			continue
		}
		if c.Record {
			enabled[safes[i]] = c
			order = append(order, safes[i])
		}
	}
	for _, e := range m.sorted() {
		c, ok := enabled[e.safe]
		switch {
		case !ok:
			m.retire(e)
			m.log.Info(fmt.Sprintf("reconcile: %s is no longer set to record, stopped it", e.cam.Name), logx.Action("camera.disable"), logx.Target(e.cam.Name), logx.Result("ok"))
		case c.IP != e.cam.IP || c.Quality != e.cam.Quality:
			m.retire(e)
			m.log.Info(fmt.Sprintf("reconcile: %s address or stream changed, restarting it", e.cam.Name), logx.Action("camera.change"), logx.Target(e.cam.Name), logx.Result("ok"))
		default:
			e.cam = c
		}
	}
	for _, safe := range order {
		if _, ok := m.entries[safe]; ok {
			continue
		}
		c := enabled[safe]
		path, err := relay.PathName(c.Name)
		if err != nil {
			path = ""
		}
		m.entries[safe] = &Entry{
			cam: c, safe: safe, dir: filepath.Join(m.s.RootPath, safe), relayPath: path,
			createdWall: nowWall, nextWall: nowWall, nextMono: nowMono,
		}
		if m.reconciled {
			m.log.Info(fmt.Sprintf("reconcile: %s is now set to record, starting it", c.Name), logx.Action("camera.enable"), logx.Target(c.Name), logx.Result("ok"))
		}
	}
	m.reconciled = true
}

// retire removes e from the current set; a child that is running or being started is stopped
// through Plan (Kill, ReasonStop) and the entry is dropped at its Exited.
func (m *Manager) retire(e *Entry) {
	delete(m.entries, e.safe)
	m.tracker.Forget(e.dir)
	if e.running || e.pending == pendStart {
		e.retiring = true
		m.retiring = append(m.retiring, e)
		sort.SliceStable(m.retiring, func(i, j int) bool { return m.retiring[i].safe < m.retiring[j].safe })
	}
}

// Shutdown retires every entry: from now on Plan hands out only Kill with ReasonStop.
func (m *Manager) Shutdown() {
	for _, e := range m.sorted() {
		m.retire(e)
	}
	m.shutdown = true
}

func (m *Manager) dropRetiring(e *Entry) {
	for i, r := range m.retiring {
		if r == e {
			m.retiring = append(m.retiring[:i], m.retiring[i+1:]...)
			return
		}
	}
}

// blockedBySibling: a retiring child of the same folder still runs (IP change, or a rename that
// keeps the folder on a case-insensitive filesystem): never two children for one camera.
func (m *Manager) blockedBySibling(e *Entry) bool {
	k := archive.FolderKey(e.safe)
	for _, r := range m.retiring {
		if archive.FolderKey(r.safe) == k && (r.running || r.pending != pendNone) {
			return true
		}
	}
	return false
}

// roundInt is PowerShell's [int] cast: round half to even.
func roundInt(v float64) int64 { return int64(math.RoundToEven(v)) }

func secs(d time.Duration) float64 { return d.Seconds() }
