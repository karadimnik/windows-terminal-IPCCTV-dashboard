package supervise

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"holocron/internal/config"
	"holocron/internal/osx"
	"holocron/internal/relay"
)

// The input choice with RelayExternal: the supervisor's relay view (no relay process, no API) makes
// every camera with a relay path start on the relay input at once, as the production relay does
// for a path in its ready set; everything else is the production logic unchanged.
func TestInputChoiceRelayExternal(t *testing.T) {
	alive := RelayView{Alive: true, Ready: relay.NewReadySet([]string{"front"})}
	tests := []struct {
		name       string
		restreamer bool
		external   bool
		view       *RelayView // nil: the supervisor's relayView() without a relay process
		camera     string
		want       string // describe() of the first Plan
	}{
		{"external relay: relay input at once", false, true, nil, "Front", "start:Front/relay/0ms"},
		{"production relay serving the path: the same", true, false, &alive, "Front", "start:Front/relay/0ms"},
		{"external relay, a name without a relay path: direct, as in production", false, true, nil, "---", "start:---/direct/0ms"},
		{"relay off: direct", false, false, nil, "Front", "start:Front/direct/0ms"},
		{"production relay not answering: direct", true, false, &RelayView{Ready: relay.UnavailableReadySet()}, "Front", "start:Front/direct/0ms"},
		{"production relay up, path not yet served: held", true, false, &RelayView{Alive: true, Ready: relay.NewReadySet(nil)}, "Front", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults(osx.Current())
			cfg.RestreamerEnabled, cfg.RelayExternal = tc.restreamer, tc.external
			st := SettingsFrom(cfg)
			st.RootPath = "/arch"
			r := newRig(t, st)
			r.m.Reconcile([]cameraT{cam(tc.camera, true)})
			var rv RelayView
			if tc.view != nil {
				rv = *tc.view
			} else {
				rv = (&Supervisor{cfg: cfg}).relayView()
			}
			if got := describe(r.m.Plan(rv)); got != tc.want {
				t.Errorf("plan = %q, want %q", got, tc.want)
			}
		})
	}
}

type goldenArgs struct {
	Inputs struct {
		OutputPattern string `json:"outputPattern"`
	} `json:"inputs"`
	Args []string `json:"args"`
}

// RelayExternal over the real goroutines with a fake ffmpeg: no relay or live process is spawned,
// no MediaMTX config, API login or live login is touched, no camera login is asked for, and the
// ffmpeg vector is the production relay vector (golden mkv-relay-900, everything but the output
// folder). A relay that cannot be read is retried on the relay input with the exit backoff.
func TestSupervisorRelayExternal(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "ffmpeg", "mkv-relay-900.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g goldenArgs
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &g); err != nil {
		t.Fatal(err)
	}
	cams := []config.Camera{{Name: "Camera One", IP: "192.0.2.11", Quality: "stream1", Record: true}}
	r := newSRig(t, cams, func(c *config.Config) {
		c.RestreamerEnabled, c.RelayExternal = false, true
		c.SegmentSeconds = 900
		c.LiveViewEnabled = true // needs this supervisor's own relay: not started either
	})
	var logins, apis, creds atomic.Int32
	r.deps.APILogin = func() (relay.Login, error) { logins.Add(1); return relay.Login{}, nil }
	r.deps.LiveLogin = func() (relay.Login, error) { logins.Add(1); return relay.Login{}, nil }
	r.deps.NewRelayAPI = func(config.Config, relay.Login) RelayAPI { apis.Add(1); return r.api }
	r.deps.Credentials = countingCreds{&creds}
	r.st.script = func(kind, name string, n int) behaviour {
		if kind == "ffmpeg" && n < 3 {
			return behaviour{exitAfter: time.Second, code: 1} // the external relay refuses
		}
		return behaviour{}
	}
	r.start()
	r.step(30 * time.Second)

	ff := r.st.started("ffmpeg")
	if len(ff) != 4 {
		t.Fatalf("%d ffmpeg starts in 30 s, want 4 (three refused, backoff 3 s, 6 s, 12 s)\n%s", len(ff), r.logText())
	}
	for i, p := range ff {
		got := p.cfg.Args
		if len(got) != len(g.Args) || strings.Join(got[:len(got)-1], " ") != strings.Join(g.Args[:len(g.Args)-1], " ") {
			t.Fatalf("start %d args\n got %q\nwant %q", i, got, g.Args)
		}
		want := filepath.Join(r.root, "Camera_One", g.Args[len(g.Args)-1][strings.LastIndex(g.Args[len(g.Args)-1], `\`)+1:])
		if got[len(got)-1] != want {
			t.Errorf("start %d output %q, want %q", i, got[len(got)-1], want)
		}
	}
	if gap := ff[1].startedAt.Sub(ff[0].startedAt); gap < 4*time.Second || gap > 5*time.Second {
		t.Errorf("first restart after %v, want the 3 s backoff after a 1 s run", gap)
	}
	if n := len(r.st.started("relay")) + len(r.st.started("live")); n != 0 {
		t.Errorf("%d MediaMTX process(es) started", n)
	}
	if logins.Load() != 0 || apis.Load() != 0 || creds.Load() != 0 {
		t.Errorf("logins read %d, relay API clients %d, camera logins asked %d: want none", logins.Load(), apis.Load(), creds.Load())
	}
	if _, err := os.Stat(filepath.Join(r.root, "_mtx")); !os.IsNotExist(err) {
		t.Errorf("a MediaMTX config folder was written: %v", err)
	}
	row := r.row("Camera One")
	if !row.Alive || row.InputMode != "relay" {
		t.Errorf("row %+v", row)
	}
	if d := r.lastDoc(); d.Restreamer == nil || d.Restreamer.Enabled || d.Restreamer.Pid != nil {
		t.Errorf("restreamer section %+v: this supervisor runs none", d.Restreamer)
	}
	if !strings.Contains(r.logText(), "RelayExternal: the cameras record from the relay at rtsp://127.0.0.1:8554") {
		t.Errorf("no RelayExternal start line:\n%s", r.logText())
	}
	res := r.stop(StopRequested)
	if res.err != nil || len(res.rep.Survivors) != 0 {
		t.Fatalf("stop: %+v", res)
	}
	r.validateAll()
}

// A configuration that asks for both its own relay and an external one is refused before anything
// starts (the configuration file cannot hold it; a Source built in code can).
func TestSupervisorRelayExternalWithOwnRelayIsAConfigFault(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), func(c *config.Config) { c.RestreamerEnabled, c.RelayExternal = true, true })
	_, err := r.build().Run(t.Context())
	if ExitCode(err) != ExitConfigFault || !strings.Contains(err.Error(), "RelayExternal needs RestreamerEnabled false") {
		t.Fatalf("err %v (exit %d)", err, ExitCode(err))
	}
	if n := len(r.st.started("")); n != 0 {
		t.Errorf("%d process(es) started", n)
	}
}

type countingCreds struct{ n *atomic.Int32 }

func (c countingCreds) CameraCredential(config.Camera) (string, string, error) {
	c.n.Add(1)
	return fakeCamUser, fakeCamPass, nil
}
