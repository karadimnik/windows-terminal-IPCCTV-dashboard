package alerts

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeWriter struct {
	got    []string
	ids    []int
	sevs   []Severity
	err    error
	closed bool
}

func (f *fakeWriter) Write(id int, sev Severity, msg string) error {
	if f.err != nil {
		return f.err
	}
	f.got, f.ids, f.sevs = append(f.got, msg), append(f.ids, id), append(f.sevs, sev)
	return nil
}
func (f *fakeWriter) Close() error { f.closed = true; return nil }

func TestEventLogWritesPrefixedTitleDashBody(t *testing.T) {
	w := &fakeWriter{}
	opens := 0
	e := newEventLogWith(newTestLog().For("alerts"), func() (EventWriter, error) { opens++; return w, nil })
	if !e.Enabled() || e.Name() != "eventlog" {
		t.Fatal("identity")
	}
	for _, ev := range []Event{
		{ID: 100, Severity: Error, Title: "the NVR stopped unexpectedly", Message: "reason"},
		{ID: 101, Severity: Warning, Title: "the NVR is recording again", Message: "restart 1 of 5"},
		{ID: 0, Severity: Info, Title: "TEST - Holocron alert test, ignore", Message: "TEST body"},
	} {
		if err := e.Send(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"IPCCTV: the NVR stopped unexpectedly - reason", "IPCCTV: the NVR is recording again - restart 1 of 5", "IPCCTV: TEST - Holocron alert test, ignore - TEST body"}
	for i := range want {
		if w.got[i] != want[i] {
			t.Errorf("%d: %q", i, w.got[i])
		}
	}
	if opens != 1 || w.ids[0] != 100 || w.sevs[1] != Warning || w.sevs[2] != Info {
		t.Errorf("opens=%d ids=%v sevs=%v", opens, w.ids, w.sevs)
	}
	if err := e.Close(); err != nil || !w.closed {
		t.Errorf("close: %v %v", err, w.closed)
	}
}

func TestEventLogLongMessageIsClipped(t *testing.T) {
	w := &fakeWriter{}
	e := newEventLogWith(newTestLog().For("alerts"), func() (EventWriter, error) { return w, nil })
	_ = e.Send(context.Background(), Event{ID: 1, Title: "t", Message: strings.Repeat("x", 100000)})
	if n := len([]rune(w.got[0])); n != maxEventLogRunes || !strings.HasSuffix(w.got[0], "...") {
		t.Errorf("%d runes", n)
	}
}

func TestEventLogOpenFailureLogsOnceAndDisables(t *testing.T) {
	lg := newTestLog()
	opens := 0
	e := newEventLogWith(lg.For("alerts"), func() (EventWriter, error) { opens++; return nil, errors.New("Access is denied") })
	if err := e.Send(context.Background(), testEvent()); err == nil {
		t.Errorf("first send must report the failure")
	}
	if e.Enabled() {
		t.Errorf("still enabled")
	}
	if err := e.Send(context.Background(), testEvent()); err == nil || opens != 1 {
		t.Errorf("second send: %v opens=%d", err, opens)
	}
	if n := len(lg.lines("WARN", "event log cannot be opened", "Access is denied")); n != 1 {
		t.Errorf("%d notices: %v", n, lg.mem.Lines())
	}
}

func TestEventLogWriteErrorIsReturnedAndStaysEnabled(t *testing.T) {
	w := &fakeWriter{err: errors.New("log full")}
	e := newEventLogWith(newTestLog().For("alerts"), func() (EventWriter, error) { return w, nil })
	if err := e.Send(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "log full") {
		t.Errorf("%v", err)
	}
	if !e.Enabled() {
		t.Errorf("a write error is not an open failure")
	}
}

func TestEventLogDisabledByConfig(t *testing.T) {
	off := false
	e := NewEventLog(EventLogConfig{Enabled: &off}, newTestLog().For("alerts"))
	if e.Enabled() {
		t.Errorf("enabled")
	}
	if on := NewEventLog(EventLogConfig{}, newTestLog().For("alerts")); !on.Enabled() {
		t.Errorf("default must be on")
	}
}
