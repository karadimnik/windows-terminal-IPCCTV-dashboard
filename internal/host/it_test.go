package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"holocron/internal/config"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/prune"
	"holocron/internal/relay"
	"holocron/internal/status"
	"holocron/internal/supervise"
)

// The full chain, opt-in with HOLOCRON_IT=1 (HOLOCRON_IT_FFMPEG: ffmpeg >= 5.0): the host (this
// process) runs the REAL supervisor (supervise.NewProduction: OS lock, registry reap, process
// scope) as a child process; two cameras record with real ffmpeg. MediaMTX is not used: the only
// copy on this machine is the Operator's (off limits), so the relay is off and each camera points at
// a silent loopback listener of this test that accepts the RTSP connection and never answers, which
// keeps each ffmpeg alive for its 60 s timeout. Loopback ports 18700-18701 only.
//
// Kill the supervisor hard -> its ffmpeg are gone before the second life starts, the host restarts
// it, alert 100 then 101, status.json shows the restart (LastUnexpectedStop by the nvr host, carried
// by the new supervisor); then a stop request -> requested, no restart, nothing left.
const (
	itCamA = 18700
	itCamB = 18701
)

type itCreds struct{}

func (itCreds) CameraCredential(config.Camera) (string, string, error) {
	return "camuser-fake", "campass-NOT-A-SECRET-88", nil
}

type itSource struct {
	cfg  config.Config
	cams []config.Camera
}

func (s itSource) Config() (config.Config, error)    { return s.cfg, nil }
func (s itSource) Cameras() ([]config.Camera, error) { return s.cams, nil }

// realSupervisor is the "realsup" role: the production supervisor over a throwaway archive.
func realSupervisor() int {
	root := os.Getenv("HOLOCRON_IT_ROOT")
	os.Setenv(prune.SandboxEnv, "1")
	plat := osx.Current()
	cfg := config.Defaults(plat)
	cfg.RootPath = root
	cfg.FfmpegPath = os.Getenv("HOLOCRON_IT_FFMPEG")
	cfg.SegmentSeconds = 10
	cfg.StatusIntervalSeconds = 1
	cfg.ReconcileSeconds = 2
	cfg.ArchiveScanSeconds = 10
	cfg.PruneIntervalSeconds = 600
	cfg.ReserveGB, cfg.PruneHysteresisGB = 0, 0
	cfg.TimeoutMicroseconds = 60_000_000
	cfg.RestreamerEnabled, cfg.LiveViewEnabled = false, false
	cams := []config.Camera{
		{Name: "Cam A", IP: fmt.Sprintf("127.0.0.1:%d", itCamA), Quality: "a", Record: true},
		{Name: "Cam B", IP: fmt.Sprintf("127.0.0.1:%d", itCamB), Quality: "b", Record: true},
	}
	lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "debug"}, Getenv: func(string) string { return "" },
		Sink: logx.NewRotatingFile(logx.FileOptions{Path: filepath.Join(root, "_logs", fmt.Sprintf("supervisor-%d.log", os.Getpid())), MaxBytes: 4 << 20})})
	ring := supervise.NewEventRing()
	sup := supervise.NewProduction(supervise.SupervisorDeps{
		Source: itSource{cfg, cams}, Platform: plat, Credentials: itCreds{},
		APILogin: func() (relay.Login, error) {
			return relay.Login{User: relay.APIUser, Secret: strings.Repeat("ab", 32)}, nil
		},
		LiveLogin: func() (relay.Login, error) {
			return relay.Login{User: relay.LiveUser, Secret: strings.Repeat("cd", 32)}, nil
		},
		Logs: lg, Redactor: lg.Redactor(), Events: ring,
	}, root)
	_, err := sup.Run(context.Background())
	return supervise.ExitCode(err)
}

func silentListener(t *testing.T, port int) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen %d: %v", port, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
}

// registryOf reads the supervisor's _state/children.json.
func registryOf(state string) (sup int, kids []osx.ChildEntry) {
	var f struct {
		Supervisor int              `json:"supervisor"`
		Children   []osx.ChildEntry `json:"children"`
	}
	b, _ := os.ReadFile(osx.RegistryPath(state))
	json.Unmarshal(b, &f)
	return f.Supervisor, f.Children
}

func TestFullChainHostSupervisorFfmpeg(t *testing.T) {
	if os.Getenv("HOLOCRON_IT") != "1" {
		t.Skip("set HOLOCRON_IT=1 and HOLOCRON_IT_FFMPEG (ffmpeg >= 5.0) to run the full chain")
	}
	if os.Getenv("HOLOCRON_IT_FFMPEG") == "" {
		t.Skip("HOLOCRON_IT_FFMPEG is not set")
	}
	if _, err := ffmpeg.CheckVersion(os.Getenv("HOLOCRON_IT_FFMPEG")); err != nil {
		t.Skipf("the product needs ffmpeg >= 5.0: %v", err)
	}
	silentListener(t, itCamA)
	silentListener(t, itCamB)
	dir := t.TempDir()
	root := filepath.Join(dir, "archive")
	state := filepath.Join(root, "_state")
	os.MkdirAll(state, 0o700)
	scope, err := osx.NewScope("it-host", osx.ScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	exe, _ := os.Executable()
	sp := &ExecSpawner{Scope: scope, Path: exe, Args: []string{"-test.run=^$"}, Env: selfEnv("realsup", "HOLOCRON_IT_ROOT="+root)}
	lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "debug"}, Getenv: func(string) string { return "" },
		Sink: logx.NewRotatingFile(logx.FileOptions{Path: filepath.Join(dir, "host.log"), MaxBytes: 1 << 20})})
	alertsPath := filepath.Join(dir, "alerts.txt")
	h := New(Config{StateDir: state}, Deps{Spawner: sp, Alerts: &fileAlerts{path: alertsPath}, Log: lg.For("host"), Mode: Service})
	k := &tracker{}
	defer k.sweep(t)
	defer func() {
		if t.Failed() {
			for _, f := range append([]string{filepath.Join(dir, "host.log")}, globLogs(root)...) {
				b, _ := os.ReadFile(f)
				lines := strings.Split(string(b), "\n")
				t.Logf("---- %s (last 30)\n%s", filepath.Base(f), strings.Join(lines[max(0, len(lines)-30):], "\n"))
			}
		}
	}()
	done := make(chan Result, 1)
	go func() { r, _ := h.Run(context.Background()); done <- r }()

	// life 1: running, two ffmpeg in its registry
	var p1 int
	var kids1 []osx.ChildEntry
	waitFor(t, "the first supervisor recording with 2 ffmpeg", 40*time.Second, func() bool {
		d := readDoc(state)
		if d == nil || d.State != status.StateRunning || len(d.Cameras) != 2 {
			return false
		}
		p1 = int(d.SupervisorPid)
		s, kids := registryOf(state)
		kids1 = kids
		return s == p1 && len(kids) == 2
	})
	k.add(p1)
	for _, e := range kids1 {
		k.add(e.Pid)
	}
	t.Logf("life 1: supervisor %d, ffmpeg %+v", p1, kids1)

	t0 := time.Now()
	if err := (osx.OSInspector{}).Stop(k.e[p1]); err != nil {
		t.Fatal(err)
	}
	var pids1 []int
	for _, e := range kids1 {
		pids1 = append(pids1, e.Pid)
	}
	if a := k.gone(t, 3*time.Second, append([]int{p1}, pids1...)...); len(a) != 0 {
		t.Fatalf("alive after the supervisor's hard kill: %v", a)
	}
	gone := time.Since(t0)
	if d := readDoc(state); d != nil && d.State == status.StateRunning && int(d.SupervisorPid) != p1 {
		t.Fatal("the second life ran before the first life's ffmpeg were gone")
	}
	t.Logf("supervisor %d and its ffmpeg %v gone %v after the kill", p1, pids1, gone)

	// life 2: restarted, status shows the restart
	var p2 int
	waitFor(t, "the restarted supervisor recording", 40*time.Second, func() bool {
		d := readDoc(state)
		if d == nil || d.State != status.StateRunning || int(d.SupervisorPid) == p1 {
			return false
		}
		p2 = int(d.SupervisorPid)
		s, kids := registryOf(state)
		return s == p2 && len(kids) == 2
	})
	_, kids2 := registryOf(state)
	k.add(p2)
	for _, e := range kids2 {
		k.add(e.Pid)
	}
	d := readDoc(state)
	if d.LastUnexpectedStop.Stop == nil || d.LastUnexpectedStop.Stop.DetectedBy != "nvr host" || int(d.LastUnexpectedStop.Stop.SupervisorPid) != p1 {
		t.Errorf("the restarted supervisor's document does not show the restart: %+v", d.LastUnexpectedStop.Stop)
	}
	waitFor(t, "alerts 100 and 101", 20*time.Second, func() bool { return len(readAlerts(alertsPath)) >= 2 })
	if got := readAlerts(alertsPath); !reflect.DeepEqual(got, []int{100, 101}) {
		t.Errorf("alerts %v, want [100 101]", got)
	}
	t.Logf("life 2: supervisor %d, ffmpeg %+v, %v after the kill", p2, kids2, time.Since(t0))

	// requested stop
	os.WriteFile(filepath.Join(state, "stop.request"), []byte("it"), 0o600)
	select {
	case r := <-done:
		if r.Outcome != OutcomeRequested || r.Lives != 2 || r.Restarts != 1 {
			t.Errorf("result %+v", r)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the host did not end after the stop request")
	}
	if a := k.gone(t, 3*time.Second, k.all()...); len(a) != 0 {
		t.Errorf("left after the stop: %v", a)
	}
	if got := readAlerts(alertsPath); len(got) != 2 {
		t.Errorf("alerts after the stop %v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(state, "status.json"))
	if v, err := status.Validate(raw); err != nil || len(v) > 0 {
		t.Errorf("final status.json invalid: %v %v", err, v)
	}
	if d := readDoc(state); d == nil || d.State != status.StateStopped || d.StopKind != status.StopKindRequested {
		t.Errorf("final document %+v", d)
	}
	if _, kids := registryOf(state); len(kids) != 0 {
		t.Errorf("registry after the stop: %+v", kids)
	}
}

func globLogs(root string) []string {
	f, _ := filepath.Glob(filepath.Join(root, "_logs", "*.log"))
	return f
}
