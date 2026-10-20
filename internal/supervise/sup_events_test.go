package supervise

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/config"
	"holocron/internal/logx"
)

func TestEventRingParsesRecords(t *testing.T) {
	ring := NewEventRing()
	at := time.Date(2026, 10, 25, 3, 30, 0, 0, time.FixedZone("", 2*3600))
	for _, json := range []bool{false, true} {
		lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "trace"}, Getenv: func(string) string { return "" }, JSON: json,
			Sink: ring, Now: func() time.Time { return at }, Location: at.Location()})
		lg.Redactor().Register("hunter2-secret")
		sup := lg.For("supervisor")
		sup.Debug("a decision")
		sup.Info("start Front pid 12 on the relay input", logx.Action("child.start"), logx.Target("Front"), "input", "relay")
		sup.Warn(`quoted "word" and key=value inside`, "note", "with spaces in it")
		lg.For("ffmpeg:Front").Info("frame= 100 fps=25")
		lg.For("relay").Error("login hunter2-secret refused for rtsp://u:p@h/x")
		lg.For("prune").Log(context.Background(), logx.LevelTrace, "trace detail")
	}
	got := ring.Newest(20)
	if len(got) != 6 {
		t.Fatalf("%d events: %+v", len(got), got)
	}
	want := []string{
		"login *** refused for rtsp://***:***@h/x",
		`quoted "word" and key=value inside`,
		"start Front pid 12 on the relay input",
	}
	for i, ev := range got {
		w := want[i%3]
		if ev.Text != w {
			t.Errorf("event %d text %q, want %q", i, ev.Text, w)
		}
		if ev.At == nil || *ev.At != "2026-10-25 03:30:00 +02:00" {
			t.Errorf("event %d at %v", i, ev.At)
		}
	}
	if n := len(ring.Newest(2)); n != 2 {
		t.Errorf("Newest(2) = %d", n)
	}
	if e := ring.Newest(0); e == nil || len(e) != 0 {
		t.Errorf("Newest(0) = %#v", e)
	}
	// a foreign line keeps its text and has no stamp
	ring.Write([]byte("log rotation blocked: something\n"))
	if ev := ring.Newest(1)[0]; ev.At != nil || ev.Text != "log rotation blocked: something" {
		t.Errorf("foreign line %+v", ev)
	}
}

// The status Events are the supervisor's own records, newest first, at most StatusEventCount,
// redacted; every status write is redacted as a whole (a secret in a camera name never reaches
// status.json), positive control included.
func TestSupervisorEventsAndRedaction(t *testing.T) {
	secretName := "Gate " + fakeCamPass
	cams := []config.Camera{{Name: secretName, IP: "192.0.2.10", Quality: "stream1", Record: true}}
	r := newSRig(t, cams, func(c *config.Config) { c.StatusEventCount = 5 })
	r.start()
	r.step(12 * time.Second)
	d := r.docNow()
	if len(d.Events) != 5 {
		t.Fatalf("%d events, want 5", len(d.Events))
	}
	for i := 1; i < len(d.Events); i++ {
		if *d.Events[i-1].At < *d.Events[i].At {
			t.Errorf("events are not newest first: %q before %q", *d.Events[i-1].At, *d.Events[i].At)
		}
	}
	r.stop(StopRequested)
	raw, err := os.ReadFile(filepath.Join(r.root, "_state", "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fakeCamPass) {
		t.Fatalf("the camera password reached status.json:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"Name": "Gate ***"`) {
		t.Errorf("the masked name is missing (positive control):\n%s", raw)
	}
	// the documents as built hold the secret (the writer is what masks it): the control
	r.mu.Lock()
	unmasked := strings.Contains(string(r.raw[len(r.raw)-1]), fakeCamPass)
	r.mu.Unlock()
	if !unmasked {
		t.Error("control: the built document does not hold the name, so the check above proves nothing")
	}
	if strings.Contains(r.logText(), fakeCamPass) {
		t.Error("the camera password reached the log")
	}
	r.validateAll()
}

// A change of holocron.config.json while running is noticed and logged once; the running values stay.
func TestSupervisorConfigChangeNoticed(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	r.start()
	r.step(2 * time.Second)
	r.src.mu.Lock()
	r.src.cfg.ReserveGB = 99
	r.src.mu.Unlock()
	r.step(12 * time.Second)
	if n := strings.Count(r.logText(), "holocron.config.json changed"); n != 1 {
		t.Errorf("change logged %d times", n)
	}
	if r.docNow().Disk.ReserveGB != 20 {
		t.Errorf("the running value changed")
	}
	r.stop(StopRequested)
}

// A camera list that cannot be read keeps the current set; enabling a camera starts it.
func TestSupervisorReconcileKeepsSetOnError(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	r.start()
	r.step(3 * time.Second)
	r.src.mu.Lock()
	r.src.err = fmt.Errorf("camera list: unexpected end of JSON input")
	r.src.mu.Unlock()
	r.step(6 * time.Second)
	if p := r.st.of("ffmpeg", "Front"); len(p) != 1 || p[0].exited() {
		t.Fatal("a read error stopped the camera")
	}
	r.src.mu.Lock()
	r.src.err = nil
	r.src.cams = append(oneCamera("Front"), config.Camera{Name: "Back", IP: "192.0.2.11", Record: true})
	r.src.mu.Unlock()
	r.api.setReady(r.api.set) // unchanged
	r.step(7 * time.Second)
	if len(r.st.of("ffmpeg", "Back")) != 1 {
		t.Errorf("the enabled camera did not start\n%s", r.logText())
	}
	if n := strings.Count(r.logText(), "reconcile failed, keeping the current camera set"); n != 1 {
		t.Errorf("read failure logged %d times", n)
	}
	r.stop(StopRequested)
}

var _ = slog.LevelInfo
