package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"unicode/utf8"

	"holocron/internal/logx"
)

// EventWriter writes one entry to the OS event log. msg already carries the "IPCCTV: " prefix.
type EventWriter interface {
	Write(id int, sev Severity, msg string) error
	Close() error
}

// EventLog is the "eventlog" channel: the Windows Application log (source "Application", or
// "IPCCTV" when that source is registered, entries prefixed "IPCCTV: ", the technique of the
// PowerShell product's Write-CamEventLogEntry), and syslog (journald receives it) on Linux and
// macOS. It never elevates: if the log cannot be opened it logs once and disables itself.
type EventLog struct {
	log  *slog.Logger
	open func() (EventWriter, error)

	mu       sync.Mutex
	w        EventWriter
	disabled bool
}

// NewEventLog opens the OS log lazily on the first event.
func NewEventLog(cfg EventLogConfig, log *slog.Logger) *EventLog {
	e := &EventLog{log: log, open: openOSEventLog}
	if !enabledOr(cfg.Enabled, true) {
		e.disabled = true
	}
	return e
}

// newEventLogWith is NewEventLog over a given opener (tests).
func newEventLogWith(log *slog.Logger, open func() (EventWriter, error)) *EventLog {
	return &EventLog{log: log, open: open}
}

func (*EventLog) Name() string { return "eventlog" }

func (e *EventLog) Enabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.disabled
}

const maxEventLogRunes = 4000

func (e *EventLog) Send(_ context.Context, ev Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disabled {
		return fmt.Errorf("event log channel is disabled")
	}
	if e.w == nil {
		w, err := e.open()
		if err != nil {
			e.disabled = true
			e.log.Warn("the OS event log cannot be opened; the eventlog channel is disabled for this run",
				logx.Action("alert.channel"), logx.Target("eventlog"), logx.Result(logx.ResultFail), logx.Err(err))
			return err
		}
		e.w = w
	}
	msg := eventLogPrefix + ev.text()
	if utf8.RuneCountInString(msg) > maxEventLogRunes {
		msg = string([]rune(msg)[:maxEventLogRunes-3]) + "..."
	}
	return e.w.Write(ev.ID, ev.Severity, msg)
}

// Close releases the OS handle.
func (e *EventLog) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w == nil {
		return nil
	}
	err := e.w.Close()
	e.w = nil
	return err
}
