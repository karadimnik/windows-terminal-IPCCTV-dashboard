package supervise

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/prune"
	"holocron/internal/relay"
)

func oneCamera(name string) []config.Camera {
	return []config.Camera{{Name: name, IP: "192.0.2.10", Quality: "stream1", Record: true}}
}

// A child whose folder shows no progress is killed by the watchdog after SegmentSeconds x
// StallFactor and restarted (counted); a child that writes is never killed.
func TestSupervisorStalledChildKilled(t *testing.T) {
	cams := []config.Camera{{Name: "Dark", IP: "192.0.2.10", Record: true}, {Name: "Lit", IP: "192.0.2.11", Record: true}}
	r := newSRig(t, cams, func(c *config.Config) { c.SegmentSeconds = 10; c.StallFactor = 2.5; c.CameraDownAlertMinutes = 60 })
	r.start()
	limit := 25 * time.Second
	for i := 0; i < 400; i++ { // 40 s
		r.sc.set("Lit", archive.DirScan{Files: 2, NewestClosedWrite: r.clk.Now(), Newest: "x", Recent: true})
		r.step(100 * time.Millisecond)
	}
	dark := r.st.of("ffmpeg", "Dark")
	lit := r.st.of("ffmpeg", "Lit")
	if len(lit) != 1 || lit[0].exited() {
		t.Fatalf("the writing child was restarted: %d starts\n%s", len(lit), r.logText())
	}
	if len(dark) < 2 {
		t.Fatalf("the stalled child was not restarted: %d starts\n%s", len(dark), r.logText())
	}
	kills := r.st.killOrder()
	if len(kills) == 0 || kills[0] != dark[0] {
		t.Fatalf("kills %v", kills)
	}
	ranFor := dark[1].startedAt.Sub(dark[0].startedAt)
	// killed after limit (strictly), restarted after the 3 s backoff (+ one 1 s tick each)
	if ranFor <= limit+3*time.Second || ranFor > limit+3*time.Second+2*time.Second {
		t.Errorf("stalled child restarted %v after its start (limit %v + 3 s backoff)", ranFor, limit)
	}
	row := r.row("Dark")
	if row.RestartsTotal < 1 || !strings.HasPrefix(row.LastError, "stalled, no segment write for 26s (limit 25s)") {
		t.Errorf("row %+v", row)
	}
	if !strings.Contains(r.logText(), "watchdog Dark: stalled") {
		t.Errorf("no watchdog line")
	}
	r.stop(StopRequested)
	r.validateAll()
}

// Relay not serving the path: the camera is held while the relay is young, then starts on the
// direct input (list file, no password on the command line), and moves to the relay as soon as the
// path serves, without counting a restart.
func TestSupervisorDirectThenRelay(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	r.api.setReady(relay.NewReadySet(nil))
	r.start()
	r.step(10 * time.Second)
	if n := len(r.st.started("ffmpeg")); n != 0 {
		t.Fatalf("started %d before the hold expired", n)
	}
	r.step(7 * time.Second)
	ff := r.st.started("ffmpeg")
	if len(ff) != 1 {
		t.Fatalf("%d starts after the hold\n%s", len(ff), r.logText())
	}
	if at := ff[0].startedAt.Sub(0); at < 15*time.Second || at > 16*time.Second {
		t.Errorf("direct start at %v, want the 15 s hold", at)
	}
	args := strings.Join(ff[0].cfg.Args, " ")
	if !strings.Contains(args, "-f concat") || strings.Contains(args, fakeCamPass) || strings.Contains(args, fakeCamUser) {
		t.Fatalf("direct start args %q", args)
	}
	list := ff[0].cfg.Args[argIndex(ff[0].cfg.Args, "-i")+1]
	body, err := os.ReadFile(list)
	if err != nil || !strings.Contains(string(body), "rtsp://"+fakeCamUser+":"+fakeCamPass+"@192.0.2.10/stream1") {
		t.Fatalf("input list %s: %v %q", list, err, body)
	}
	r.step(2 * time.Second)
	if row := r.row("Front"); row.InputMode != "direct" || !row.Alive {
		t.Fatalf("row %+v", row)
	}
	r.api.setReady(relay.NewReadySet([]string{"front"}))
	r.step(3 * time.Second)
	ff = r.st.started("ffmpeg")
	if len(ff) != 2 || !ff[0].exited() || !argsHave(ff[1].cfg.Args, "rtsp://127.0.0.1:8554/front") {
		t.Fatalf("starts %d, first exited %v, second args %q", len(ff), ff[0].exited(), ff[len(ff)-1].cfg.Args)
	}
	if _, err := os.Stat(list); !os.IsNotExist(err) {
		t.Errorf("the input list %s is still there after the child exited: %v", list, err)
	}
	r.step(5 * time.Second)
	row := r.row("Front")
	if row.InputMode != "relay" || row.RestartsTotal != 0 || row.LastError != "" {
		t.Errorf("after the move: %+v", row)
	}
	for _, line := range []string{"did not serve 'front' within 15s, starting it on the direct input", "moving it off the direct input"} {
		if !strings.Contains(r.logText(), line) {
			t.Errorf("log lacks %q", line)
		}
	}
	r.stop(StopRequested)
	if strings.Contains(r.logText(), fakeCamPass) {
		t.Error("the camera password reached the log")
	}
	r.validateAll()
}

func argIndex(args []string, a string) int {
	for i, x := range args {
		if x == a {
			return i
		}
	}
	return -1
}

// Alert 110: one alert per episode at the limit, none while still down, a recovery line when the
// camera writes again; a second episode alerts again.
func TestSupervisorDownAlertEpisode(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), func(c *config.Config) {
		c.SegmentSeconds = 10
		c.StallFactor = 1000
		c.CameraDownAlertMinutes = 0.5 // 30 s, floored to 10 + 5 + 30 = 45 s
	})
	r.start()
	r.step(44 * time.Second)
	if n := r.al.count(110); n != 0 {
		t.Fatalf("alerted at 44 s: %d", n)
	}
	r.step(10 * time.Second)
	if n := r.al.count(110); n != 1 {
		t.Fatalf("110 alerts after 54 s: %d\n%s", n, r.logText())
	}
	if row := r.row("Front"); row.DownSince.IsNull() {
		t.Errorf("no DownSince: %+v", row)
	}
	r.step(60 * time.Second)
	if n := r.al.count(110); n != 1 {
		t.Fatalf("repeated within the episode: %d", n)
	}
	for i := 0; i < 100; i++ {
		r.sc.set("Front", archive.DirScan{Files: 2, NewestClosedWrite: r.clk.Now(), Newest: "x", Recent: true})
		r.step(100 * time.Millisecond)
	}
	if !strings.Contains(r.logText(), "Front is writing again after") || r.al.count(110) != 1 {
		t.Fatalf("no recovery line or an extra alert (%d)\n%s", r.al.count(110), r.logText())
	}
	if row := r.row("Front"); !row.DownSince.IsNull() {
		t.Errorf("DownSince kept after recovery")
	}
	r.step(60 * time.Second) // dark again: a new episode
	if n := r.al.count(110); n != 2 {
		t.Errorf("second episode: %d alerts", n)
	}
	r.stop(StopRequested)
	r.validateAll()
}

// The heartbeat goes on while the relay start blocks for 20 s (the PowerShell loop could not).
func TestSupervisorHeartbeatNotBlockedByRelayStart(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(r *srig)
	}{
		{"readiness wait", func(r *srig) { r.api.waitBlock = 20 * time.Second }},
		{"process start", func(r *srig) {
			r.st.script = func(kind, _ string, _ int) behaviour {
				if kind == "relay" {
					return behaviour{block: 20 * time.Second}
				}
				return behaviour{}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newSRig(t, sixCameras(), nil)
			c.setup(r)
			r.start()
			r.step(19 * time.Second)
			if c.name == "readiness wait" {
				r.api.mu.Lock()
				waits := r.api.waits
				r.api.mu.Unlock()
				if waits != 1 {
					t.Fatalf("the relay is not in its readiness wait (%d waits)", waits)
				}
			} else if n := len(r.st.started("relay")); n != 0 {
				t.Fatalf("the relay start returned early (%d)", n)
			}
			beats := r.docCount()
			// t = 0, 5, 10, 15 at least; a heartbeat sharing the start's goroutine would have 1
			if beats < 4 {
				t.Fatalf("%d heartbeats during the 19 s relay start, want >= 4", beats)
			}
			var last time.Time
			r.mu.Lock()
			for _, d := range r.docs {
				if !d.UpdatedAt.Time().After(last) {
					t.Errorf("UpdatedAt did not advance: %v after %v", d.UpdatedAt.Time(), last)
				}
				last = d.UpdatedAt.Time()
			}
			r.mu.Unlock()
			r.step(10 * time.Second)
			if n := len(r.st.started("ffmpeg")); n != 6 {
				t.Errorf("%d cameras started after the relay came up", n)
			}
			r.stop(StopRequested)
			r.validateAll()
		})
	}
}

// The pruner runs at start and every PruneIntervalSeconds on its own goroutine; its alerts go to
// Alerts (once each, as the State says) and its state to the Prune section.
func TestSupervisorPrunerSchedule(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), func(c *config.Config) { c.PruneIntervalSeconds = 20 })
	r.pr.result = func(n int) prune.Result {
		res := prune.Result{}
		if n == 2 {
			res.Alerts = []prune.Alert{{ID: 121, Level: "Error", Title: "pruning is blocked - the disk is filling", Body: "PRUNE BLOCKED: test"}}
		}
		return res
	}
	r.start()
	r.step(65 * time.Second)
	runs := r.pr.runTimes()
	if len(runs) != 4 {
		t.Fatalf("%d prune runs in 65 s, want 4 (0, 20, 40, 60)", len(runs))
	}
	for i, at := range runs {
		want := time.Duration(i) * 20 * time.Second
		if d := at.Sub(0) - want; d < 0 || d > 200*time.Millisecond {
			t.Errorf("run %d at %v, want %v", i, at.Sub(0), want)
		}
	}
	if r.al.count(121) != 1 || r.pr.claims != 1 {
		t.Errorf("alerts %v, claims %d", r.al.ids(), r.pr.claims)
	}
	if d := r.lastDoc(); d.Prune == nil || d.Prune.LastRunAt.IsNull() {
		t.Errorf("prune section %+v", d.Prune)
	}
	r.stop(StopRequested)
	r.validateAll()
}

// Alert 130 once per episode of a refused login; an unavailable relay does not end the episode, an
// accepted login does.
func TestSupervisorLoginAlertOncePerEpisode(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	r.start()
	r.step(3 * time.Second)
	r.api.setReady(relay.DeniedReadySet())
	r.step(5 * time.Second)
	r.api.setReady(relay.UnavailableReadySet())
	r.step(3 * time.Second)
	r.api.setReady(relay.DeniedReadySet())
	r.step(5 * time.Second)
	if n := r.al.count(130); n != 1 {
		t.Fatalf("130 alerts in one episode: %d", n)
	}
	r.api.setReady(relay.NewReadySet([]string{"front"}))
	r.step(2 * time.Second)
	if !strings.Contains(r.logText(), "accepts the product's login again") {
		t.Errorf("no recovery line")
	}
	r.api.setReady(relay.DeniedReadySet())
	r.step(3 * time.Second)
	if n := r.al.count(130); n != 2 {
		t.Errorf("130 alerts after a second episode: %d", n)
	}
	// denied counts as ready: the camera stayed on the relay throughout
	for _, p := range r.st.started("ffmpeg") {
		if !argsHave(p.cfg.Args, "rtsp://127.0.0.1:8554/front") {
			t.Errorf("a start left the relay: %q", p.cfg.Args)
		}
	}
	if strings.Contains(r.logText(), fakeAPIPass) {
		t.Error("the API secret reached the log")
	}
	r.stop(StopRequested)
	r.validateAll()
}

// The stop-request file stops the supervisor in order (requested) and is removed.
func TestSupervisorStopRequestFile(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	r.start()
	r.step(3 * time.Second)
	flag := filepath.Join(r.root, "_state", "stop.request")
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := r.finish()
	if res.err != nil || res.rep.Kind != StopRequested {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Errorf("stop request still there: %v", err)
	}
	if d := r.lastDoc(); d.State != "stopped" || d.StopKind != "requested" {
		t.Errorf("final %s/%s", d.State, d.StopKind)
	}
	if !strings.Contains(r.logText(), "stop requested through _state/stop.request") {
		t.Error("no stop-request line")
	}
	if len(r.st.alive()) != 0 {
		t.Errorf("alive after stop: %v", r.st.alive())
	}
	r.validateAll()
}

// A leftover stop request from an earlier run is cleared at start and does not stop the new run.
func TestSupervisorClearsLeftoverStopRequest(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	flag := filepath.Join(r.root, "_state", "stop.request")
	os.MkdirAll(filepath.Dir(flag), 0o755)
	os.WriteFile(flag, nil, 0o644)
	r.start()
	r.step(3 * time.Second)
	select {
	case res := <-r.runCh:
		t.Fatalf("stopped on the leftover request: %+v", res)
	default:
	}
	if !strings.Contains(r.logText(), "cleared a leftover stop request") {
		t.Error("no clear line")
	}
	r.stop(StopRequested)
}
