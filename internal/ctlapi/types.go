package ctlapi

import (
	"errors"

	"holocron/internal/status"
)

// Wire constants.
const (
	HeaderKey      = "X-Holocron-Key"   // the secret of <root>/_state/ctl.secret
	HeaderActionID = "X-Action-Id"      // carried into the supervisor's records
	HeaderActor    = "X-Holocron-Actor" // cli:<os user> or web:<name>; anything else is ignored
	MaxBody        = 64 << 10           // request body limit
	AddrFile       = "ctl.addr"
	SecretFile     = "ctl.secret"
)

// Errors a Backend returns to choose the status code (they are also what the client returns).
var (
	ErrNotFound    = errors.New("not found")                     // 404
	ErrNotRunning  = errors.New("the supervisor is not running") // 503
	ErrBusy        = errors.New("another control action is running")
	ErrUnreachable = errors.New("the control endpoint is not reachable")
)

// State is GET /v1/state: the supervisor's running document, condensed.
type State struct {
	Running bool          `json:"running"`
	Pid     int64         `json:"pid"`
	Started string        `json:"started"`
	Updated string        `json:"updated"`
	Cameras []CameraState `json:"cameras"`
	Relay   *RelayState   `json:"relay"`
	Live    *LiveState    `json:"live"`
	Prune   *status.Prune `json:"prune"`
	Disk    *status.Disk  `json:"disk"`
}

// CameraState is one recording camera.
type CameraState struct {
	Name             string `json:"name"`
	SafeName         string `json:"safeName"`
	Record           bool   `json:"record"`
	Alive            bool   `json:"alive"`
	Mode             string `json:"mode"` // relay | direct | ""
	Pid              *int64 `json:"pid"`
	Restarts         int64  `json:"restarts"`
	RestartsLastHour int64  `json:"restartsLastHour"`
	LastError        string `json:"lastError"`
}

// RelayState is the recording MediaMTX.
type RelayState struct {
	Enabled bool     `json:"enabled"`
	Alive   bool     `json:"alive"`
	Pid     *int64   `json:"pid"`
	Paths   []string `json:"paths"`
}

// LiveState is the live-view MediaMTX (Enabled false when it is off).
type LiveState struct {
	Enabled   bool   `json:"enabled"`
	Alive     bool   `json:"alive"`
	Pid       *int64 `json:"pid"`
	LastError string `json:"lastError"`
}

// StopReport is POST /v1/stop's answer (supervise.StopReport).
type StopReport struct {
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
	Survivors []int  `json:"survivors"`
	Final     bool   `json:"final"`
}

// Changed is the answer of a toggle: whether anything had to change.
type Changed struct {
	Target  string `json:"target"`
	Enabled bool   `json:"enabled"`
	Changed bool   `json:"changed"`
}

// Levels is GET|POST /v1/log/level.
type Levels struct {
	Default    string            `json:"default"`
	Components map[string]string `json:"components"`
}

type stopRequest struct {
	Kind string `json:"kind"`
}

type enabledRequest struct {
	Enabled *bool `json:"enabled"`
}

type levelRequest struct {
	Component string `json:"component"`
	Level     string `json:"level"`
}

// reply is every response body.
type reply struct {
	OK       bool   `json:"ok"`
	ActionID string `json:"action_id,omitempty"`
	Error    string `json:"error,omitempty"`
	Data     any    `json:"data,omitempty"`
}

// StateFrom condenses a running status document.
func StateFrom(d *status.Document) State {
	if d == nil {
		return State{Cameras: []CameraState{}}
	}
	st := State{Running: d.State == status.StateRunning, Pid: d.SupervisorPid, Cameras: []CameraState{}, Prune: d.Prune, Disk: d.Disk}
	if !d.StartedAt.IsNull() {
		st.Started = d.StartedAt.Time().Format("2006-01-02T15:04:05Z07:00")
	}
	if !d.UpdatedAt.IsNull() {
		st.Updated = d.UpdatedAt.Time().Format("2006-01-02T15:04:05Z07:00")
	}
	for _, c := range d.Cameras {
		st.Cameras = append(st.Cameras, CameraState{Name: c.Name, SafeName: c.SafeName, Record: c.Record, Alive: c.Alive, Mode: c.InputMode,
			Pid: c.Pid, Restarts: c.RestartsTotal, RestartsLastHour: c.RestartsLastHour, LastError: c.LastError})
	}
	if r := d.Restreamer; r != nil {
		st.Relay = &RelayState{Enabled: r.Enabled, Alive: r.Alive, Pid: r.Pid, Paths: r.Paths}
	}
	st.Live = &LiveState{}
	if l := d.Live; l != nil {
		st.Live = &LiveState{Enabled: l.Enabled, Alive: l.Alive, Pid: l.Pid, LastError: l.LastError}
	}
	return st
}
