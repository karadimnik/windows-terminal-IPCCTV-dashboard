package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/config"
	"holocron/internal/osx"
)

// The throwaway MediaMTX check. It runs only when HOLOCRON_IT_MEDIAMTX names a MediaMTX executable
// (and HOLOCRON_IT_FFMPEG an ffmpeg): the executable is COPIED into the test's own temp folder and
// run from there, on loopback ports 18560-18599 only, every listener it does not need switched off.
// Nothing else is touched: it never reads a real config, credential or running instance.
//
//	18560 relay RTSP   18561 relay control API   18570 fake camera (a second throwaway MediaMTX)
//	18571 nothing (the offline camera)           18588 live media (UDP)   18589 live WHEP
const (
	itRelayRTSP = 18560
	itRelayAPI  = 18561
	itCamera    = 18570
	itOffline   = 18571
	itLiveUDP   = 18588
	itLiveWHEP  = 18589
)

type proc struct {
	name string
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func (p *proc) Write(b []byte) (int, error) { p.mu.Lock(); defer p.mu.Unlock(); return p.out.Write(b) }
func (p *proc) Output() string              { p.mu.Lock(); defer p.mu.Unlock(); return p.out.String() }
func (p *proc) Pid() int                    { return p.cmd.Process.Pid }

// kill ends the process we started, by its handle (hence its pid), and waits for it.
func (p *proc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.done
}

func startProc(t *testing.T, name, exe, dir string, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{name: name, done: make(chan struct{})}
	p.cmd = exec.Command(exe, args...)
	p.cmd.Dir = dir
	p.cmd.Env = env
	p.cmd.Stdout, p.cmd.Stderr = p, p
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() { p.cmd.Wait(); close(p.done) }()
	t.Logf("started %s pid %d", name, p.Pid())
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			p.kill()
			t.Logf("killed %s pid %d", name, p.Pid())
		}
	})
	return p
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

// listeners returns the local addresses a pid has bound (TCP listening and UDP), from netstat.
func listeners(t *testing.T, pid int) []string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("listener check uses netstat -ano")
	}
	out, err := exec.Command("netstat.exe", "-ano").Output()
	if err != nil {
		t.Fatalf("netstat: %v", err)
	}
	var addrs []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[len(f)-1] != fmt.Sprint(pid) {
			continue
		}
		switch f[0] {
		case "TCP":
			if len(f) >= 5 && f[3] == "LISTENING" {
				addrs = append(addrs, "tcp "+f[1])
			}
		case "UDP":
			addrs = append(addrs, "udp "+f[1])
		}
	}
	return addrs
}

func assertListeners(t *testing.T, p *proc, want ...string) {
	t.Helper()
	got := listeners(t, p.Pid())
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("%s pid %d does not listen on %s (has %v)", p.name, p.Pid(), w, got)
		}
		delete(set, w)
	}
	for extra := range set {
		t.Errorf("%s pid %d has an unexpected listener %s", p.name, p.Pid(), extra)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

const cameraConfig = `logLevel: info
logDestinations: [stdout]
api: no
metrics: no
pprof: no
playback: no
rtsp: yes
rtspTransports: [tcp]
rtspAddress: 127.0.0.1:18570
rtmp: no
hls: no
webrtc: no
srt: no
moq: no
paths:
  stream1:
  stream2:
`

func TestThrowawayMediaMTX(t *testing.T) {
	srcExe, ffmpeg := os.Getenv("HOLOCRON_IT_MEDIAMTX"), os.Getenv("HOLOCRON_IT_FFMPEG")
	if srcExe == "" || ffmpeg == "" {
		t.Skip("set HOLOCRON_IT_MEDIAMTX and HOLOCRON_IT_FFMPEG to run the throwaway MediaMTX check")
	}
	plat := osx.Current()
	root := t.TempDir()
	exe := filepath.Join(root, "mtx", "mediamtx"+map[bool]string{true: ".exe"}[runtime.GOOS == "windows"])
	copyFile(t, srcExe, exe)
	parentEnv := os.Environ()
	const camUser, camPass = "fakeuser", "fakepass-NOT-A-SECRET"

	// ---- the fake camera: a second throwaway MediaMTX, and ffmpeg publishing a test pattern into it ----
	camDir := filepath.Join(root, "camera")
	camCfg := filepath.Join(camDir, "camera.yml")
	if _, err := WriteConfigIfChanged(camCfg, []byte(cameraConfig)); err != nil {
		t.Fatal(err)
	}
	cam := startProc(t, "fake-camera", exe, camDir, ChildEnv(parentEnv, nil), camCfg)
	waitFor(t, "the fake camera to listen", 15*time.Second, func() bool {
		return len(listenersQuiet(cam.Pid())) > 0
	})
	assertListeners(t, cam, fmt.Sprintf("tcp 127.0.0.1:%d", itCamera))
	for _, stream := range []string{"stream1", "stream2"} {
		pub := startProc(t, "publisher-"+stream, ffmpeg, root, parentEnv, "-nostdin", "-loglevel", "error", "-re",
			"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-g", "10", "-pix_fmt", "yuv420p", "-f", "rtsp", "-rtsp_transport", "tcp", fmt.Sprintf("rtsp://127.0.0.1:%d/%s", itCamera, stream))
		_ = pub
	}

	// ---- the relay, from the generated config ----
	cfg := config.Defaults(plat)
	cfg.RestreamerExePath = exe
	cfg.RestreamerConfigPath = filepath.Join(root, "relay", "mediamtx.yml")
	cfg.RelayBase = fmt.Sprintf("rtsp://127.0.0.1:%d", itRelayRTSP)
	cfg.RelayApiBase = fmt.Sprintf("http://127.0.0.1:%d", itRelayAPI)
	cfg.LiveExePath = exe
	cfg.LiveConfigPath = filepath.Join(root, "live", "mediamtx-live.yml")
	cfg.LiveWhepAddress = fmt.Sprintf("127.0.0.1:%d", itLiveWHEP)
	cfg.LiveUdpAddress = fmt.Sprintf("127.0.0.1:%d", itLiveUDP)
	cfg.LiveSubStream = "stream2"
	cameras := []config.Camera{
		{Name: "Camera One", IP: fmt.Sprintf("127.0.0.1:%d", itCamera), Quality: "stream1", Record: true},
		{Name: "Camera Two", IP: fmt.Sprintf("127.0.0.1:%d", itOffline), Quality: "stream2"},
	}
	creds := fixedCreds{camUser, camPass}

	apiLogin, err := LoadAPICredential(plat, filepath.Join(root, "secrets", "relay_api_secret.txt"), true)
	if err != nil {
		t.Fatal(err)
	}
	relayIn := RelayInputs{Config: cfg, Cameras: cameras, API: apiLogin}
	relayText, err := RelayConfig(relayIn)
	if err != nil {
		t.Fatal(err)
	}
	if leaks := findLeaks(relayText, camUser, camPass, apiLogin.Secret); len(leaks) > 0 {
		t.Fatalf("the relay config leaks %q", leaks)
	}
	if _, err := WriteConfigIfChanged(cfg.RestreamerConfigPath, relayText); err != nil {
		t.Fatal(err)
	}
	if r := ConfigCurrent(cfg.RestreamerConfigPath, relayIn); !r.Current {
		t.Fatalf("a freshly written config is not current: %s", r.Detail)
	}
	srcEnv, err := SourceEnv(cameras, creds)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := RelayChildSpec(plat, cfg, filepath.Join(root, "restreamer.log"), parentEnv, srcEnv)
	if err != nil {
		t.Fatal(err)
	}
	relay := startProc(t, "relay", spec.Exe, spec.Dir, spec.Env, spec.Args...)

	api := NewAPI(cfg.RelayApiBase, apiLogin, 1500*time.Millisecond)
	policy := NewReadinessPolicy(cfg)
	set, answered := api.WaitStarted(context.Background(), policy, relay.done)
	if !answered || set.State != OK {
		t.Fatalf("the control API did not answer: answered=%v state=%v\n%s", answered, set.State, relay.Output())
	}
	t.Logf("control API answered with state %v", set.State)

	// the relay serves the camera that is up and not the one that is offline
	waitFor(t, "cameraone to be ready", 30*time.Second, func() bool {
		s, err := api.Ready(context.Background())
		return err == nil && s.Has("cameraone")
	})
	set, err = api.Ready(context.Background())
	if err != nil || set.State != OK || !set.Has("cameraone") || set.Has("cameratwo") {
		t.Fatalf("ready set: state %v names %v err %v", set.State, set.Names(), err)
	}
	t.Logf("ready set: %v", set.Names())

	// only the two configured listeners, loopback only
	assertListeners(t, relay, fmt.Sprintf("tcp 127.0.0.1:%d", itRelayRTSP), fmt.Sprintf("tcp 127.0.0.1:%d", itRelayAPI))

	// anonymous RTSP read works through the relay (the recorders use it): DESCRIBE answers 200
	if status := rtspDescribe(t, itRelayRTSP, "cameraone"); status != "RTSP/1.0 200 OK" {
		t.Errorf("DESCRIBE cameraone through the relay: %q", status)
	}

	// wrong login: Denied, after MediaMTX's pause when the budget allows, at once when it does not
	wrong := NewAPI(cfg.RelayApiBase, Login{User: apiLogin.User, Secret: strings.Repeat("0", 64)}, 6*time.Second)
	begin := time.Now()
	ws, err := wrong.Ready(context.Background())
	long := time.Since(begin)
	if err != nil || ws.State != Denied || !ws.Has("cameraone") || !ws.Has("cameratwo") {
		t.Fatalf("wrong login, 6 s budget: state %v err %v", ws.State, err)
	}
	wrong.Timeout = 300 * time.Millisecond
	begin = time.Now()
	ws, err = wrong.Ready(context.Background())
	short := time.Since(begin)
	if err != nil || ws.State != Denied {
		t.Fatalf("wrong login, 300 ms budget: state %v err %v", ws.State, err)
	}
	// the smallest budget (the 100 ms floor) is shorter than MediaMTX's pause for nearly every wrong
	// login, so this is the path through the anonymous second request
	wrong.Timeout = 100 * time.Millisecond
	begin = time.Now()
	ws, err = wrong.Ready(context.Background())
	floor := time.Since(begin)
	if err != nil || ws.State != Denied {
		t.Fatalf("wrong login, 100 ms budget: state %v err %v", ws.State, err)
	}
	t.Logf("wrong login: Denied after %v with a 6 s budget, %v with 300 ms, %v with 100 ms", long, short, floor)
	if short > 2*time.Second || floor > time.Second {
		t.Errorf("a short budget still took %v / %v", short, floor)
	}
	// the right login still works, and the anonymous request is refused at once
	if s, err := api.Ready(context.Background()); err != nil || s.State != OK {
		t.Errorf("right login after the wrong ones: %v %v", s.State, err)
	}
	anon := NewAPI(cfg.RelayApiBase, Login{}, time.Second)
	if s, err := anon.Ready(context.Background()); err != nil || s.State != Denied {
		t.Errorf("no login: %v %v", s.State, err)
	}
	var w LoginWatch
	if w.Observe(Denied) != LoginRefused || w.Observe(OK) != LoginAccepted {
		t.Error("login episode")
	}

	// ---- the live instance ----
	liveSecret, err := LoadLiveSecret(plat, filepath.Join(root, "secrets", "live_view_secret.txt"), true)
	if err != nil {
		t.Fatal(err)
	}
	liveIn := LiveInputs{Config: cfg, Cameras: cameras, Login: liveSecret}
	liveText, err := LiveConfig(liveIn)
	if err != nil {
		t.Fatal(err)
	}
	if leaks := findLeaks(liveText, camUser, camPass, liveSecret.Secret); len(leaks) > 0 {
		t.Fatalf("the live config leaks %q", leaks)
	}
	if _, err := WriteConfigIfChanged(cfg.LiveConfigPath, liveText); err != nil {
		t.Fatal(err)
	}
	liveEnv, err := LiveSourceEnv(cameras, cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	liveSpec, err := LiveChildSpec(plat, cfg, filepath.Join(root, "liveview.log"), parentEnv, liveEnv)
	if err != nil {
		t.Fatal(err)
	}
	live := startProc(t, "live", liveSpec.Exe, liveSpec.Dir, liveSpec.Env, liveSpec.Args...)
	whep := fmt.Sprintf("http://127.0.0.1:%d/cameraonemain/whep", itLiveWHEP)
	waitFor(t, "the live WHEP port to answer", 20*time.Second, func() bool {
		resp, err := http.Post(whep, "application/sdp", strings.NewReader("x"))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})
	assertListeners(t, live, fmt.Sprintf("tcp 127.0.0.1:%d", itLiveWHEP), fmt.Sprintf("udp 127.0.0.1:%d", itLiveUDP))
	// no login and a wrong login are refused (401); the right one passes authentication (the body is not an SDP offer, so 400)
	for _, c := range []struct {
		user, pass string
		want       int
	}{{"", "", 401}, {LiveUser, strings.Repeat("0", 64), 401}, {LiveUser, liveSecret.Secret, 400}} {
		req, _ := http.NewRequest(http.MethodPost, whep, strings.NewReader("not an offer"))
		req.Header.Set("Content-Type", "application/sdp")
		if c.user != "" {
			req.SetBasicAuth(c.user, c.pass)
		}
		cl := &http.Client{Timeout: 10 * time.Second}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("WHEP as %q: %v", c.user, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("WHEP as %q: HTTP %d, want %d", c.user, resp.StatusCode, c.want)
		}
	}
	// the live instance has no control API and no RTSP: its ports are closed
	if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v3/paths/list", itRelayAPI)); err == nil {
		resp.Body.Close()
	}
	t.Logf("live instance answered; relay listeners and live listeners are the configured ones only")

	// ---- no credential on disk ----
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasSuffix(path, "mediamtx.exe") || strings.HasSuffix(path, "mediamtx") {
			return nil
		}
		b, _ := os.ReadFile(path)
		base := filepath.Base(path)
		if base == "relay_api_secret.txt" || base == "live_view_secret.txt" {
			return nil
		}
		if leaks := findLeaks(b, camPass, apiLogin.Secret, liveSecret.Secret); len(leaks) > 0 {
			t.Errorf("%s holds %q", path, leaks)
		}
		return nil
	})
	// ... nor in either child's output: the API and live secrets never left their files; the camera
	// URL does appear in MediaMTX's own errors for the offline camera, which is why stdout is redacted.
	for _, p := range []*proc{relay, live} {
		if leaks := findLeaks([]byte(p.Output()), apiLogin.Secret, liveSecret.Secret); len(leaks) > 0 {
			t.Errorf("%s output holds a login secret", p.name)
		}
	}
	if strings.Contains(relay.Output(), camPass) {
		t.Logf("as expected, the relay's raw stdout carries the camera URL for the offline camera: the caller redacts it")
	}

	// ---- the relay stops: Unavailable, at once ----
	relay.kill()
	t.Logf("killed relay pid %d", relay.Pid())
	begin = time.Now()
	gone, err := api.Ready(context.Background())
	if gone.State != Unavailable || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stopped relay: state %v err %v", gone.State, err)
	}
	if gone.Has("cameraone") {
		t.Error("a stopped relay serves a path")
	}
	t.Logf("stopped relay: Unavailable after %v: %v", time.Since(begin), err)
}

func listenersQuiet(pid int) []string {
	out, err := exec.Command("netstat.exe", "-ano").Output()
	if err != nil {
		return nil
	}
	var addrs []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && f[0] == "TCP" && f[3] == "LISTENING" && f[len(f)-1] == fmt.Sprint(pid) {
			addrs = append(addrs, f[1])
		}
	}
	return addrs
}
