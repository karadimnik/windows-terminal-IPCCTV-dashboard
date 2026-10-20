package supervise

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/relay"
	"holocron/internal/status"
)

// The real scenario, opt-in with HOLOCRON_IT=1. With HOLOCRON_IT_MEDIAMTX (a MediaMTX executable,
// COPIED into the test's temp folder and run from there) and HOLOCRON_IT_FFMPEG (ffmpeg >= 5.0):
// a throwaway "camera" MediaMTX fed by two lavfi publishers, the supervisor's own relay from the
// generated config, two cameras recording 10 s segments into a t.TempDir() archive, one publisher
// killed (error and backoff in the row) and restored (records again), then the ordered stop through
// _state/stop.request. Without a MediaMTX (Linux: no MediaMTX here, ffmpeg 4.2): the relay is off,
// the cameras point at a dead loopback port, and the start, error rows with backoff, status
// validity and the ordered stop are checked. Loopback ports 18640-18679 only:
//
//	18640 relay RTSP   18641 relay control API   18650 the camera MediaMTX   18670-18671 dead ports
//
// Only processes this test started are killed, by pid.
const (
	itRelayRTSP = 18640
	itRelayAPI  = 18641
	itCamera    = 18650
	itDead      = 18670
)

type itProc struct {
	name string
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func (p *itProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.Write(b)
}
func (p *itProc) output() string { p.mu.Lock(); defer p.mu.Unlock(); return p.out.String() }

func itStart(t *testing.T, name, exe, dir string, args ...string) *itProc {
	t.Helper()
	p := &itProc{name: name, done: make(chan struct{})}
	p.cmd = exec.Command(exe, args...)
	p.cmd.Dir = dir
	p.cmd.Env = relay.ChildEnv(os.Environ(), nil)
	p.cmd.Stdout, p.cmd.Stderr = p, p
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() { p.cmd.Wait(); close(p.done) }()
	t.Logf("started %s pid %d", name, p.cmd.Process.Pid)
	t.Cleanup(func() { p.kill(t) })
	return p
}

func (p *itProc) kill(t *testing.T) {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
		t.Logf("killed %s pid %d", p.name, p.cmd.Process.Pid)
	case <-time.After(10 * time.Second):
		t.Errorf("%s pid %d did not exit after the kill", p.name, p.cmd.Process.Pid)
	}
}

// recStarter records every pid the supervisor creates.
type recStarter struct {
	inner ffmpeg.ExecStarter
	mu    sync.Mutex
	pids  map[int]string
}

func (s *recStarter) Start(c ffmpeg.StartConfig) (ffmpeg.Process, error) {
	p, err := s.inner.Start(c)
	if err == nil {
		s.mu.Lock()
		s.pids[p.Pid()] = filepath.Base(c.Exe)
		s.mu.Unlock()
	}
	return p, err
}

func waitUntil(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

func segments(t *testing.T, root, name string) []string {
	safe, _ := archive.SafeName(name)
	entries, _ := os.ReadDir(filepath.Join(root, safe))
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func readStatus(path string) *status.Document {
	d, err := status.Read(path)
	if err != nil {
		return nil
	}
	return d
}

func rowOf(d *status.Document, name string) *status.Camera {
	if d == nil {
		return nil
	}
	for i := range d.Cameras {
		if d.Cameras[i].Name == name {
			return &d.Cameras[i]
		}
	}
	return nil
}

func itListeners(pid int) []string {
	out, err := exec.Command("netstat.exe", "-ano").Output()
	if err != nil {
		return nil
	}
	var addrs []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[len(f)-1] != fmt.Sprint(pid) {
			continue
		}
		if f[0] == "TCP" && len(f) >= 5 && f[3] == "LISTENING" {
			addrs = append(addrs, "tcp "+f[1])
		} else if f[0] == "UDP" {
			addrs = append(addrs, "udp "+f[1])
		}
	}
	sort.Strings(addrs)
	return addrs
}

func TestSupervisorRealIntegration(t *testing.T) {
	if os.Getenv("HOLOCRON_IT") != "1" {
		t.Skip("set HOLOCRON_IT=1 (and HOLOCRON_IT_FFMPEG, HOLOCRON_IT_MEDIAMTX) to run the real scenario")
	}
	ffmpegExe := os.Getenv("HOLOCRON_IT_FFMPEG")
	if ffmpegExe == "" {
		ffmpegExe = "ffmpeg"
	}
	srcMtx := os.Getenv("HOLOCRON_IT_MEDIAMTX")
	withRelay := srcMtx != ""
	plat := osx.Current()
	base := t.TempDir()
	root := filepath.Join(base, "archive")
	os.MkdirAll(root, 0o755)
	exe := func(name string) string {
		if runtime.GOOS == "windows" {
			return name + ".exe"
		}
		return name
	}

	cfg := config.Defaults(plat)
	cfg.RootPath = root
	cfg.FfmpegPath = ffmpegExe
	cfg.SegmentSeconds = 10
	cfg.StatusIntervalSeconds = 2
	cfg.ReconcileSeconds = 2
	cfg.ArchiveScanSeconds = 10
	cfg.PruneIntervalSeconds = 60
	cfg.ReserveGB, cfg.PruneHysteresisGB = 0, 0 // nothing is ever below the reserve: the pruner deletes nothing
	cfg.StopTimeoutSeconds = 30
	cfg.RestreamerEnabled = withRelay
	cfg.RelayBase = fmt.Sprintf("rtsp://127.0.0.1:%d", itRelayRTSP)
	cfg.RelayApiBase = fmt.Sprintf("http://127.0.0.1:%d", itRelayAPI)
	cfg.RestreamerConfigPath = filepath.Join(base, "relay", "mediamtx.yml")
	cfg.LiveViewEnabled = false

	cams := []config.Camera{
		{Name: "Cam One", IP: fmt.Sprintf("127.0.0.1:%d", itDead), Quality: "cam1", Record: true},
		{Name: "Cam Two", IP: fmt.Sprintf("127.0.0.1:%d", itDead+1), Quality: "cam2", Record: true},
	}
	var pubs map[string]*itProc
	publish := func(stream string) *itProc {
		return itStart(t, "publisher-"+stream, ffmpegExe, base, "-nostdin", "-loglevel", "error", "-re",
			"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-g", "10", "-pix_fmt", "yuv420p", "-f", "rtsp", "-rtsp_transport", "tcp", fmt.Sprintf("rtsp://127.0.0.1:%d/%s", itCamera, stream))
	}
	var camMtx *itProc
	if withRelay {
		mtx := filepath.Join(base, "mtx", exe("mediamtx"))
		b, err := os.ReadFile(srcMtx)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(mtx), 0o755)
		if err := os.WriteFile(mtx, b, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.RestreamerExePath = mtx
		camDir := filepath.Join(base, "camera")
		os.MkdirAll(camDir, 0o755)
		camCfg := filepath.Join(camDir, "camera.yml")
		os.WriteFile(camCfg, []byte(fmt.Sprintf(`logLevel: info
logDestinations: [stdout]
api: no
metrics: no
pprof: no
playback: no
rtsp: yes
rtspTransports: [tcp]
rtspAddress: 127.0.0.1:%d
rtmp: no
hls: no
webrtc: no
srt: no
moq: no
paths:
  cam1:
  cam2:
`, itCamera)), 0o644)
		camMtx = itStart(t, "camera-mediamtx", mtx, camDir, camCfg)
		waitUntil(t, "the camera MediaMTX to listen", 15*time.Second, func() bool {
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", itCamera), 200*time.Millisecond)
			if err == nil {
				c.Close()
			}
			return err == nil
		})
		pubs = map[string]*itProc{"cam1": publish("cam1"), "cam2": publish("cam2")}
		for i := range cams {
			cams[i].IP = fmt.Sprintf("127.0.0.1:%d", itCamera)
		}
		time.Sleep(2 * time.Second)
	}

	logs := logx.NewMemorySink()
	ring := NewEventRing()
	lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "debug"}, Getenv: func(string) string { return "" }, Sink: logx.FanOut(logs, ring)})
	defer func() {
		if t.Failed() {
			t.Logf("supervisor log:\n%s", logs.String())
			files, _ := filepath.Glob(filepath.Join(root, "_logs", "*.log"))
			for _, f := range files {
				b, _ := os.ReadFile(f)
				lines := strings.Split(string(b), "\n")
				t.Logf("---- %s (last 40 of %d lines)\n%s", filepath.Base(f), len(lines), strings.Join(lines[max(0, len(lines)-40):], "\n"))
			}
			for _, n := range []string{"Cam One", "Cam Two"} {
				t.Logf("%s files: %v", n, segments(t, root, n))
			}
			raw, _ := os.ReadFile(filepath.Join(root, "_state", "status.json"))
			t.Logf("status.json:\n%s", raw)
		}
	}()
	starter := &recStarter{pids: map[int]string{}}
	src := &fakeSource{cfg: cfg, cams: cams}
	lock := &MemoryLock{}
	sup := NewSupervisor(SupervisorDeps{
		Source: src, Lock: lock, Platform: plat, Starter: starter, Credentials: fixedCreds{},
		APILogin: func() (relay.Login, error) {
			return relay.LoadAPICredential(plat, filepath.Join(base, "secrets", "relay_api_secret.txt"), true)
		},
		Logs: lg, Redactor: lg.Redactor(), Events: ring,
	})
	runDone := make(chan runResult, 1)
	begin := time.Now()
	go func() {
		rep, err := sup.Run(context.Background())
		runDone <- runResult{rep, err}
	}()
	// whatever happens, the supervisor is stopped and every process it started is gone at the end
	t.Cleanup(func() {
		select {
		case <-sup.Done():
		default:
			t.Log("cleanup: stopping the supervisor")
			sup.Stop(StopRequested)
		}
		starter.mu.Lock()
		defer starter.mu.Unlock()
		for pid, name := range starter.pids {
			if _, ok := (status.OSProcesses{}).Alive(int64(pid)); ok {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
					t.Errorf("cleanup: killed %s pid %d, which outlived the stop", name, pid)
				}
			}
		}
	})
	statusPath := filepath.Join(root, "_state", "status.json")

	// status.json is written, valid, running
	waitUntil(t, "a running status.json", 20*time.Second, func() bool {
		d := readStatus(statusPath)
		return d != nil && d.State == "running" && len(d.Cameras) == 2
	})
	raw, _ := os.ReadFile(statusPath)
	if vs, err := status.Validate(raw); err != nil || len(vs) != 0 {
		t.Fatalf("status.json does not validate: %v %v", err, vs)
	}
	t.Logf("first valid running status.json after %v", time.Since(begin).Round(time.Millisecond))

	if !withRelay {
		// the cameras cannot connect: each row shows the error and the backoff grows
		waitUntil(t, "two restarts of each camera", 40*time.Second, func() bool {
			d := readStatus(statusPath)
			for _, n := range []string{"Cam One", "Cam Two"} {
				r := rowOf(d, n)
				if r == nil || r.RestartsTotal < 2 || !strings.HasPrefix(r.LastError, "ffmpeg exited with code") {
					return false
				}
			}
			return true
		})
		d := readStatus(statusPath)
		t.Logf("rows: %s / %s, restarts %d / %d", rowOf(d, "Cam One").LastError, rowOf(d, "Cam Two").LastError,
			rowOf(d, "Cam One").RestartsTotal, rowOf(d, "Cam Two").RestartsTotal)
		if !strings.Contains(logs.String(), "restart #2 in 6s") {
			t.Errorf("no growing backoff in the log")
		}
	} else {
		// both cameras record two segments with correct names, on the relay
		waitUntil(t, "two closed segments per camera", 60*time.Second, func() bool {
			return len(segments(t, root, "Cam One")) >= 3 && len(segments(t, root, "Cam Two")) >= 3
		})
		for _, name := range []string{"Cam One", "Cam Two"} {
			names := segments(t, root, name)
			safe, _ := archive.SafeName(name)
			for _, n := range names {
				if !archive.MatcherFor(plat).IsSegmentOf(n, safe, ".mkv") {
					t.Errorf("%s: %q is not a segment name of the camera", safe, n)
				}
			}
			t.Logf("%s: %v", safe, names)
		}
		d := readStatus(statusPath)
		for _, n := range []string{"Cam One", "Cam Two"} {
			if r := rowOf(d, n); r == nil || !r.Alive || r.InputMode != "relay" {
				t.Errorf("row %s: %+v", n, r)
			}
		}
		if d.Restreamer == nil || !d.Restreamer.Alive || d.Restreamer.Pid == nil {
			t.Fatalf("restreamer %+v", d.Restreamer)
		}
		relayPid := int(*d.Restreamer.Pid)
		if runtime.GOOS == "windows" {
			want := []string{fmt.Sprintf("tcp 127.0.0.1:%d", itRelayRTSP), fmt.Sprintf("tcp 127.0.0.1:%d", itRelayAPI)}
			if got := itListeners(relayPid); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("relay pid %d listens on %v, want %v", relayPid, got, want)
			}
			if got := itListeners(camMtx.cmd.Process.Pid); strings.Join(got, ",") != fmt.Sprintf("tcp 127.0.0.1:%d", itCamera) {
				t.Errorf("camera MediaMTX listens on %v", got)
			}
		}

		// kill one publisher: the row shows the error and restarts with backoff
		killedAt := time.Now()
		pubs["cam1"].kill(t)
		waitUntil(t, "Cam One to show the error and two restarts", 90*time.Second, func() bool {
			r := rowOf(readStatus(statusPath), "Cam One")
			return r != nil && r.RestartsTotal >= 2 && r.LastError != ""
		})
		r1 := rowOf(readStatus(statusPath), "Cam One")
		t.Logf("after the publisher was killed (%v): restarts %d, last error %q, input %q, next start %v",
			time.Since(killedAt).Round(time.Second), r1.RestartsTotal, r1.LastError, r1.InputMode, r1.NextStartAt.Time())
		if !strings.Contains(logs.String(), "for Cam One; restart #2 in 6s") {
			t.Errorf("no backoff line for Cam One")
		}
		if r2 := rowOf(readStatus(statusPath), "Cam Two"); r2 == nil || !r2.Alive || r2.RestartsTotal != 0 {
			t.Errorf("Cam Two was disturbed: %+v", r2)
		}

		// restore it: it records again
		restoredAt := time.Now()
		pubs["cam1"] = publish("cam1")
		before := len(segments(t, root, "Cam One"))
		waitUntil(t, "Cam One to record again", 120*time.Second, func() bool {
			names := segments(t, root, "Cam One")
			if len(names) < before+2 {
				return false
			}
			// the newest CLOSED segment (NTFS does not refresh the size of the open one)
			fi, err := os.Stat(filepath.Join(root, "Cam_One", names[len(names)-2]))
			return err == nil && fi.ModTime().After(restoredAt) && fi.Size() > 0
		})
		r1 = rowOf(readStatus(statusPath), "Cam One")
		t.Logf("Cam One records again %v after the restore: %v, input %q, restarts %d", time.Since(restoredAt).Round(time.Second),
			segments(t, root, "Cam One")[before:], r1.InputMode, r1.RestartsTotal)
	}

	// the ordered stop through the stop-request file
	stopAt := time.Now()
	flag := filepath.Join(root, "_state", "stop.request")
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var res runResult
	select {
	case res = <-runDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the supervisor did not stop within 60 s")
	}
	t.Logf("stopped in %v: %+v err %v", time.Since(stopAt).Round(time.Millisecond), res.rep, res.err)
	if res.err != nil || ExitCode(res.err) != 0 || res.rep.Kind != StopRequested || len(res.rep.Survivors) != 0 || !res.rep.Final {
		t.Fatalf("stop: %+v %v", res.rep, res.err)
	}
	final, err := status.Read(statusPath)
	if err != nil || final.State != "stopped" || final.StopKind != "requested" {
		t.Fatalf("final status: %v %+v", err, final)
	}
	raw, _ = os.ReadFile(statusPath)
	if vs, err := status.Validate(raw); err != nil || len(vs) != 0 {
		t.Fatalf("final status.json does not validate: %v %v", err, vs)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Errorf("stop.request still there: %v", err)
	}
	if lock.Held() {
		t.Error("lock still held")
	}
	order := []string{}
	for _, line := range strings.Split(logs.String(), "\n") {
		switch {
		case strings.Contains(line, "kill restreamer pid"):
			order = append(order, "relay")
		case strings.Contains(line, "] kill Cam"):
			order = append(order, "camera")
		}
	}
	if withRelay && (len(order) < 3 || order[len(order)-1] != "relay") {
		t.Errorf("kill order %v", order)
	}
	// nothing we started is left
	starter.mu.Lock()
	pids := map[int]string{}
	for k, v := range starter.pids {
		pids[k] = v
	}
	starter.mu.Unlock()
	var alive []string
	for pid, name := range pids {
		if _, ok := (status.OSProcesses{}).Alive(int64(pid)); ok {
			alive = append(alive, fmt.Sprintf("%s pid %d", name, pid))
		}
	}
	if len(alive) > 0 {
		t.Errorf("still alive after the stop: %v", alive)
	}
	t.Logf("%d processes started by the supervisor, all gone; kill order %v", len(pids), order)
	for _, p := range pubs {
		p.kill(t)
	}
	if camMtx != nil {
		camMtx.kill(t)
	}
}
