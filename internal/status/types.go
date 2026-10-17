package status

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// TimeLayout is the PowerShell round-trip ('o') format with a numeric offset:
// exactly seven fractional digits, and +00:00 rather than Z for UTC.
const TimeLayout = "2006-01-02T15:04:05.0000000-07:00"

// Time is a status.json timestamp. The zero value is JSON null ("never").
// A parsed value keeps the offset it was written with, so formatting it
// reproduces the original text.
type Time struct{ t time.Time }

// NewTime wraps an instant; the zero time.Time is null.
func NewTime(t time.Time) Time { return Time{t} }

// IsNull reports whether the value is JSON null.
func (t Time) IsNull() bool { return t.t.IsZero() }

// Time returns the instant (zero for null).
func (t Time) Time() time.Time { return t.t }

// IsZero lets encoding/json omit a null Time under omitzero.
func (t Time) IsZero() bool { return t.t.IsZero() }

func (t Time) MarshalJSON() ([]byte, error) {
	if t.t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.t.Format(TimeLayout))
}

func (t *Time) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		t.t = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("status timestamp: not a string or null")
	}
	parsed, err := time.Parse(TimeLayout, s)
	if err != nil {
		return fmt.Errorf("status timestamp: %w", err)
	}
	t.t = parsed
	return nil
}

// Document is status.json. Pointer fields and fields marked omitzero are the
// ones a minimal document (created by a patch when no file existed) lacks;
// the full document always has them.
type Document struct {
	Schema             int          `json:"Schema"`
	State              string       `json:"State"`
	StopKind           string       `json:"StopKind"`
	LastUnexpectedStop MaybeStop    `json:"LastUnexpectedStop,omitzero"`
	LastWindowClose    *WindowClose `json:"LastWindowClose,omitempty"`
	SupervisorPid      int64        `json:"SupervisorPid"`
	StartedAt          Time         `json:"StartedAt,omitzero"`
	UpdatedAt          Time         `json:"UpdatedAt"`
	UptimeSeconds      *float64     `json:"UptimeSeconds,omitempty"`
	RootPath           string       `json:"RootPath,omitempty"`
	Container          string       `json:"Container,omitempty"`
	SegmentSeconds     int          `json:"SegmentSeconds,omitempty"`
	Disk               *Disk        `json:"Disk,omitempty"`
	Prune              *Prune       `json:"Prune,omitempty"`
	Restreamer         *Restreamer  `json:"Restreamer,omitempty"`
	Live               *Live        `json:"Live,omitempty"`
	Events             []Event      `json:"Events,omitzero"`
	Cameras            []Camera     `json:"Cameras,omitzero"`
}

// MaybeStop is LastUnexpectedStop: absent (Present false), null (Present with
// a nil Stop) or a record. The writer always emits the key in a full document.
type MaybeStop struct {
	Present bool
	Stop    *UnexpectedStop
}

func (m MaybeStop) IsZero() bool { return !m.Present }

func (m MaybeStop) MarshalJSON() ([]byte, error) { return json.Marshal(m.Stop) }

func (m *MaybeStop) UnmarshalJSON(data []byte) error {
	m.Present = true
	m.Stop = nil
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	m.Stop = new(UnexpectedStop)
	return json.Unmarshal(data, m.Stop)
}

// UnexpectedStop is LastUnexpectedStop. At is carried verbatim across a
// restart: the web app dismisses the notice by comparing it byte for byte.
type UnexpectedStop struct {
	At            Time   `json:"At"`
	Reason        string `json:"Reason"`
	SupervisorPid int64  `json:"SupervisorPid"`
	DetectedBy    string `json:"DetectedBy"` // supervisor | nvr host
}

// WindowClose is LastWindowClose, written only by the warden patch.
type WindowClose struct {
	At          Time   `json:"At"`
	Reason      string `json:"Reason"`
	WindowTitle string `json:"WindowTitle"`
	DetectedBy  string `json:"DetectedBy"` // nvr warden
}

type Disk struct {
	FreeGB                 float64  `json:"FreeGB"`
	ReserveGB              float64  `json:"ReserveGB"`
	PruneHysteresisGB      float64  `json:"PruneHysteresisGB"`
	ArchiveGB              *float64 `json:"ArchiveGB"`
	ArchiveBytes           *int64   `json:"ArchiveBytes"`
	ArchiveFiles           *int64   `json:"ArchiveFiles"`
	OldestSegment          Time     `json:"OldestSegment"`
	NewestSegment          Time     `json:"NewestSegment"`
	MeasuredGBPerDay       *float64 `json:"MeasuredGBPerDay"`
	ProjectedDaysRemaining *float64 `json:"ProjectedDaysRemaining"`
	ScannedAt              Time     `json:"ScannedAt"`
}

type Prune struct {
	LastRunAt    Time    `json:"LastRunAt"`
	DeletedTotal int64   `json:"DeletedTotal"`
	FreedGBTotal float64 `json:"FreedGBTotal"`
	Blocked      bool    `json:"Blocked"`
	LastMessage  string  `json:"LastMessage"`
}

// Restreamer is the recording relay; always present, even when disabled.
type Restreamer struct {
	Enabled       bool     `json:"Enabled"`
	RelayBase     string   `json:"RelayBase"`
	Pid           *int64   `json:"Pid"`
	LastPid       *int64   `json:"LastPid"`
	Alive         bool     `json:"Alive"`
	StartedAt     Time     `json:"StartedAt"`
	RestartsTotal int64    `json:"RestartsTotal"`
	Paths         []string `json:"Paths"`
	LastError     string   `json:"LastError"`
	LastErrorAt   Time     `json:"LastErrorAt"`
}

// Live is the live-view MediaMTX; the key is absent unless live view is on.
type Live struct {
	Enabled       bool         `json:"Enabled"`
	Pid           *int64       `json:"Pid"`
	LastPid       *int64       `json:"LastPid"`
	Alive         bool         `json:"Alive"`
	StartedAt     Time         `json:"StartedAt"`
	RestartsTotal int64        `json:"RestartsTotal"`
	Paths         []string     `json:"Paths"`
	Cameras       []LiveCamera `json:"Cameras"`
	LastError     string       `json:"LastError"`
	LastErrorAt   Time         `json:"LastErrorAt"`
}

type LiveCamera struct {
	Name     string `json:"Name"`
	Sub      bool   `json:"Sub"`
	Main     bool   `json:"Main"`
	SubPath  string `json:"SubPath"`
	MainPath string `json:"MainPath"`
}

// Event is one tail line of recorder.log, newest first. At is the LOG stamp
// ("yyyy-MM-dd HH:mm:ss +03:00"), not the round-trip format, or nil.
type Event struct {
	At   *string `json:"At"`
	Text string  `json:"Text"`
}

type Camera struct {
	Name                          string   `json:"Name"`
	SafeName                      string   `json:"SafeName"`
	Record                        bool     `json:"Record"`
	Pid                           *int64   `json:"Pid"`
	LastPid                       *int64   `json:"LastPid"`
	Alive                         bool     `json:"Alive"`
	CurrentSegment                *string  `json:"CurrentSegment"`
	CurrentSegmentBytes           *int64   `json:"CurrentSegmentBytes"`
	SecondsSinceLastClosedSegment *float64 `json:"SecondsSinceLastClosedSegment"`
	InputMode                     string   `json:"InputMode"` // "" | relay | direct
	RestartsLastHour              int64    `json:"RestartsLastHour"`
	RestartsTotal                 int64    `json:"RestartsTotal"`
	StartedAt                     Time     `json:"StartedAt"`
	NextStartAt                   Time     `json:"NextStartAt"`
	LastError                     string   `json:"LastError"`
	LastErrorAt                   Time     `json:"LastErrorAt"`
	ArchiveFiles                  *int64   `json:"ArchiveFiles"`
	ArchiveBytes                  *int64   `json:"ArchiveBytes"`
	DownSince                     Time     `json:"DownSince"`
	ConfigError                   string   `json:"ConfigError"`
}
