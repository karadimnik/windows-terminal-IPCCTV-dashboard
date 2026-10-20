package alerts

import (
	"context"
	"os"
	"testing"
	"time"
)

// Live checks against the real Windows of the machine running the test. Each writes ONE labelled
// TEST item and only when HOLOCRON_LIVE names it, so `go test ./...` never touches the OS.
//
//	HOLOCRON_LIVE=eventlog  one "IPCCTV: TEST ..." entry in the Application log
//	HOLOCRON_LIVE=toast     one labelled TEST toast
func liveEvent() Event {
	return Event{ID: IDTest, Severity: Info, Title: "TEST - Holocron alert test, ignore",
		Message: "This is a TEST written by the Holocron alerts package test. Nothing is wrong.", At: time.Now()}
}

func TestLiveEventLog(t *testing.T) {
	if os.Getenv("HOLOCRON_LIVE") != "eventlog" {
		t.Skip("set HOLOCRON_LIVE=eventlog")
	}
	e := NewEventLog(EventLogConfig{}, newTestLog().For("alerts"))
	defer e.Close()
	if err := e.Send(context.Background(), liveEvent()); err != nil {
		t.Fatalf("Application log write: %v", err)
	}
	if w, ok := e.w.(*winEventLog); ok {
		t.Logf("written as source %q", w.Source)
	}
}

func TestLiveToast(t *testing.T) {
	if os.Getenv("HOLOCRON_LIVE") != "toast" {
		t.Skip("set HOLOCRON_LIVE=toast")
	}
	d := NewDesktop(DesktopConfig{}, newTestLog().For("alerts"))
	if !d.Enabled() {
		t.Fatal("powershell.exe not found")
	}
	if err := d.Send(context.Background(), liveEvent()); err != nil {
		t.Fatalf("toast: %v", err)
	}
}
