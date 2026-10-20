package alerts

import (
	"fmt"
	"strings"
	"time"
)

// Severity of an alert. The names are those of the PowerShell product's EntryType.
type Severity int

const (
	Info Severity = iota
	Warning
	Error
)

func (s Severity) String() string {
	switch s {
	case Warning:
		return "Warning"
	case Error:
		return "Error"
	}
	return "Info"
}

// ParseSeverity reads "Error", "Warning", "Info" or "Information" in any case. Anything else is
// an Error: an alert of unknown severity is raised loudly rather than quietly.
func ParseSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info", "information":
		return Info
	case "warning", "warn":
		return Warning
	}
	return Error
}

// Alert ids (IR section 6, docs/alerts.md).
const (
	IDSupervisorStopped   = 100 // Error: the supervisor stopped unexpectedly and is being restarted
	IDSupervisorRestarted = 101 // Warning: it is recording again
	IDRestartBudgetSpent  = 102 // Error: it stopped and is staying down
	IDWindowClosed        = 103 // Warning: the NVR window was closed
	IDCameraDown          = 110 // Warning: one camera made no file progress for the effective limit
	IDPruneDisabled       = 120 // Error: pruning disabled, the root is unsafe
	IDPruneBlocked        = 121 // Error: pruning blocked, the disk is filling
	IDAgeRuleHeld         = 122 // Error: the age rule's circuit breaker held a run
	IDRelayLoginRefused   = 130 // Error: the relay's control API refused the product's login
	IDTest                = 0   // a TEST event raised by Dispatcher.Test; never used by a producer
)

// Event is one alert. Key, when set, identifies the episode: the dispatcher never sends the same
// Key twice within MinRepeat. The producers apply the per-episode rules of IR 6 themselves.
type Event struct {
	ID       int
	Severity Severity
	Title    string
	Message  string
	Camera   string
	Key      string
	At       time.Time // zero means "when raised"
}

// Eventer is implemented by producer event types (supervise.Event, prune.Alert) so the producer
// package does not import this one, and the caller wires them with a one-line adapter.
type Eventer interface{ AlertEvent() Event }

// FromFields builds an Event from the string-typed fields the producers use today:
// supervise.Event{ID, Severity, Title, Camera, Message, Key} and prune.Alert{ID, Level, Title, Body}.
func FromFields(id int, severity, title, camera, message, key string) Event {
	return Event{ID: id, Severity: ParseSeverity(severity), Title: title, Camera: camera, Message: message, Key: key}
}

// Meaning is what an id stands for.
type Meaning struct {
	ID       int
	Severity Severity
	Text     string
}

var meanings = []Meaning{
	{IDSupervisorStopped, Error, "the supervisor stopped unexpectedly and is being restarted"},
	{IDSupervisorRestarted, Warning, "the supervisor is recording again after a restart"},
	{IDRestartBudgetSpent, Error, "the supervisor stopped and the restart budget is spent, so it is staying down"},
	{IDWindowClosed, Warning, "the NVR window was closed, so nothing is recording and nothing will restart it"},
	{IDCameraDown, Warning, "one camera has made no file progress for the effective limit"},
	{IDPruneDisabled, Error, "pruning is disabled because the root is unsafe, so the disk will fill"},
	{IDPruneBlocked, Error, "pruning is blocked with the disk under the reserve and nothing deletable left"},
	{IDAgeRuleHeld, Error, "the age rule's circuit breaker held a run that would delete most of the archive"},
	{IDRelayLoginRefused, Error, "the relay's control API refused the product's login"},
}

// Meanings lists the known ids in numeric order.
func Meanings() []Meaning { return append([]Meaning(nil), meanings...) }

// Describe is the meaning of an id, or "unknown alert id N".
func Describe(id int) string {
	for _, m := range meanings {
		if m.ID == id {
			return m.Text
		}
	}
	if id == IDTest {
		return "a test alert raised by 'alert test'"
	}
	return fmt.Sprintf("unknown alert id %d", id)
}

// text is the single line the PowerShell product writes to its event log and log: "{title} - {body}".
func (e Event) text() string {
	switch {
	case e.Title == "":
		return e.Message
	case e.Message == "":
		return e.Title
	}
	return e.Title + " - " + e.Message
}
