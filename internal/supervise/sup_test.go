package supervise

import (
	"strings"
	"testing"
	"time"

	"holocron/internal/clock"
	"holocron/internal/config"
	"holocron/internal/relay"
	"holocron/internal/status"
)

func argsHave(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// Six cameras start on the relay input, staggered by StartStaggerMs, after the relay; the rows show
// them alive with their pids; the documents validate.
func TestSupervisorStartsSixWithStagger(t *testing.T) {
	r := newSRig(t, sixCameras(), nil)
	r.start()
	r.step(3 * time.Second)
	ff := r.st.started("ffmpeg")
	if len(ff) != 6 {
		t.Fatalf("%d ffmpeg starts, want 6\n%s", len(ff), r.logText())
	}
	rel := r.st.started("relay")
	if len(rel) != 1 || rel[0].startedAt > ff[0].startedAt {
		t.Fatalf("relay starts %d, first at %v, first camera at %v", len(rel), rel, ff[0].startedAt)
	}
	stagger := time.Duration(r.cfg.StartStaggerMs) * time.Millisecond
	for i, p := range ff {
		if got := p.startedAt.Sub(ff[0].startedAt); got != time.Duration(i)*stagger {
			t.Errorf("start %d (%s) at +%v, want +%v", i, p.name, got, time.Duration(i)*stagger)
		}
		path, _ := relay.PathName(p.name)
		if !argsHave(p.cfg.Args, "rtsp://127.0.0.1:8554/"+strings.ToLower(path)) {
			t.Errorf("%s is not on the relay input: %q", p.name, p.cfg.Args)
		}
	}
	t.Logf("relay started at +%v, first camera at +%v", rel[0].startedAt.Sub(0), ff[0].startedAt.Sub(0))
	if span := ff[5].startedAt.Sub(ff[0].startedAt); span > 1500*time.Millisecond {
		t.Errorf("six starts took %v", span)
	}
	r.step(5 * time.Second)
	byName := map[string]*fakeProc{}
	for _, p := range ff {
		byName[p.name] = p
	}
	for _, c := range sixCameras() {
		row := r.row(c.Name)
		p := byName[c.Name]
		if !row.Alive || row.Pid == nil || *row.Pid != int64(p.pid) || row.InputMode != "relay" || row.RestartsTotal != 0 {
			t.Errorf("row %s: %+v", c.Name, row)
		}
	}
	d := r.lastDoc()
	if d.State != status.StateRunning || d.StopKind != "" || d.SupervisorPid != 4242 || d.Restreamer == nil || !d.Restreamer.Alive ||
		d.Restreamer.Pid == nil || *d.Restreamer.Pid != int64(rel[0].pid) || len(d.Restreamer.Paths) != 6 || d.Live != nil {
		t.Errorf("document: %+v restreamer %+v", d, d.Restreamer)
	}
	res := r.stop(StopRequested)
	if res.err != nil || res.rep.Kind != StopRequested || len(res.rep.Survivors) != 0 {
		t.Fatalf("stop: %+v", res)
	}
	r.validateAll()
}

// A child that exits repeatedly is restarted with the backoff 3, 6, 12, 24 s; the rows show the
// count, the error and the next start.
func TestSupervisorBackoffVisibleInRows(t *testing.T) {
	cams := []config.Camera{{Name: "Front", IP: "192.0.2.10", Quality: "stream1", Record: true}}
	r := newSRig(t, cams, nil)
	r.st.script = func(kind, name string, n int) behaviour {
		if kind == "ffmpeg" {
			return behaviour{exitAfter: time.Second, code: 8}
		}
		return behaviour{}
	}
	r.start()
	r.step(60 * time.Second)
	ff := r.st.started("ffmpeg")
	if len(ff) < 5 {
		t.Fatalf("%d starts", len(ff))
	}
	// run 1 s, then wait 3, 6, 12, 24: the gaps between starts are 1 + wait (+ up to one 1 s tick)
	for i, wait := range []int{3, 6, 12, 24} {
		gap := ff[i+1].startedAt.Sub(ff[i].startedAt)
		lo, hi := time.Duration(1+wait)*time.Second, time.Duration(2+wait)*time.Second
		if gap < lo || gap > hi {
			t.Errorf("gap %d = %v, want in [%v, %v]", i, gap, lo, hi)
		}
	}
	row := r.row("Front")
	if row.RestartsTotal < 4 || !strings.HasPrefix(row.LastError, "ffmpeg exited with code 8 after 1s") || row.LastErrorAt.IsNull() {
		t.Errorf("row %+v", row)
	}
	// between restarts the row has no pid and a NextStartAt
	r.stepUntil(60*time.Second, func() bool { return !r.row("Front").Alive })
	row = r.row("Front")
	if row.Pid != nil || row.NextStartAt.IsNull() || row.LastPid == nil {
		t.Errorf("waiting row %+v", row)
	}
	if !strings.Contains(r.logText(), "restart #2 in 6s") {
		t.Errorf("no backoff log line:\n%s", r.logText())
	}
	r.stop(StopRequested)
	r.validateAll()
}

var _ = clock.Mono(0)
