package alerts

import (
	"strings"
	"testing"
)

func TestParseSeverity(t *testing.T) {
	for in, want := range map[string]Severity{
		"Error": Error, "error": Error, "Warning": Warning, " WARN ": Warning, "Information": Info, "info": Info,
		"": Error, "bogus": Error, // unknown is loud, never quiet
	} {
		if got := ParseSeverity(in); got != want {
			t.Errorf("ParseSeverity(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDescribeCoversEveryID(t *testing.T) {
	for _, id := range []int{100, 101, 102, 103, 110, 120, 121, 122, 130} {
		if d := Describe(id); strings.HasPrefix(d, "unknown") || d == "" {
			t.Errorf("Describe(%d) = %q", id, d)
		}
	}
	if d := Describe(999); d != "unknown alert id 999" {
		t.Errorf("Describe(999) = %q", d)
	}
	// severities of IR 6
	want := map[int]Severity{100: Error, 101: Warning, 102: Error, 103: Warning, 110: Warning, 120: Error, 121: Error, 122: Error, 130: Error}
	ms := Meanings()
	if len(ms) != len(want) {
		t.Fatalf("%d meanings, want %d", len(ms), len(want))
	}
	for _, m := range ms {
		if want[m.ID] != m.Severity {
			t.Errorf("id %d severity %v, want %v", m.ID, m.Severity, want[m.ID])
		}
	}
}

type prodEvent struct {
	ID                                    int
	Severity, Title, Camera, Message, Key string
}

func (p prodEvent) AlertEvent() Event {
	return FromFields(p.ID, p.Severity, p.Title, p.Camera, p.Message, p.Key)
}

func TestProducerAdapters(t *testing.T) {
	// the shape of supervise.Event
	var e Eventer = prodEvent{110, "Warning", "a camera has stopped recording", "cam1", "cam1 has written nothing", "110:cam1"}
	got := e.AlertEvent()
	if got.ID != 110 || got.Severity != Warning || got.Camera != "cam1" || got.Key != "110:cam1" || got.Title == "" {
		t.Errorf("%+v", got)
	}
	// the shape of prune.Alert{ID, Level, Title, Body}
	p := FromFields(121, "Error", "pruning is blocked - the disk is filling", "", "BLOCKED: ...", "")
	if p.Severity != Error || p.Message != "BLOCKED: ..." {
		t.Errorf("%+v", p)
	}
}

func TestEventText(t *testing.T) {
	for _, tc := range []struct{ title, msg, want string }{
		{"t", "m", "t - m"}, {"t", "", "t"}, {"", "m", "m"},
	} {
		if got := (Event{Title: tc.title, Message: tc.msg}).text(); got != tc.want {
			t.Errorf("%q", got)
		}
	}
}
