package osx

import (
	"sync"
	"time"
)

// Reason says why the console (terminal) asked this process to end.
type Reason string

const (
	ReasonClose       Reason = "close"        // Windows CTRL_CLOSE_EVENT: the window or tab was closed
	ReasonLogoff      Reason = "logoff"       // Windows CTRL_LOGOFF_EVENT
	ReasonShutdown    Reason = "shutdown"     // Windows CTRL_SHUTDOWN_EVENT
	ReasonHangup      Reason = "hangup"       // Unix SIGHUP: the terminal went away
	ReasonTerminate   Reason = "terminate"    // SIGTERM not traced to a console event (Unix kill, service stop)
	ReasonInterrupt   Reason = "interrupt"    // Ctrl+C / Ctrl+Break (SIGINT)
	ReasonConsoleGone Reason = "console-gone" // the console probe found the console host dead (no event is sent then, SP2)
)

// ConsoleBudget is the part of the 5.04 s Windows gives a process after a close event (SP2) that
// the cleanup may use; the rest is margin for the process exit itself.
const ConsoleBudget = 4 * time.Second

// ProbeInterval is the console probe rate (4 Hz: the loss showed within 0.5 to 0.8 s in SP2).
const ProbeInterval = 250 * time.Millisecond

func onceStop(f func()) func() {
	var o sync.Once
	return func() { o.Do(f) }
}
