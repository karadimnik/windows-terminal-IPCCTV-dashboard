package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/osx"
	"holocron/internal/status"
)

// The end-to-end run (HOLOCRON_IT=1): the real holocron executable, a t.TempDir() root, two lavfi
// cameras published into a throwaway MediaMTX (a copy of HOLOCRON_IT_MEDIAMTX) on loopback ports
// 18740-18779 only, credentials through the two environment variables with fake values:
// up --detach -> running with both cameras recording -> cam disable one -> log level supervisor
// debug -> config set -> down with 0 survivors. Everything this test starts is recorded by pid and
// stopped by pid in t.Cleanup.
//
//	HOLOCRON_IT=1 HOLOCRON_IT_FFMPEG=... HOLOCRON_IT_MEDIAMTX=... [HOLOCRON_IT_EXE=<built holocron>]
const (
	itCamPort   = 18740
	itRelayPort = 18741
	itAPIPort   = 18742
	itSegment   = 10
)

type itRig struct {
	t                    *testing.T
	exe, ffmpeg, mtx     string
	scratch, root, cfg   string
	env                  []string
	pass                 string
	mu                   sync.Mutex
	started              []*exec.Cmd            // processes this test started itself
	seen                 map[int]osx.ChildEntry // holocron's processes, recorded while it ran
	inspector            osx.Inspector
	firstStatus, stopDur time.Duration
}

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

func TestEndToEnd(t *testing.T) {
	if os.Getenv("HOLOCRON_IT") != "1" {
		t.Skip("set HOLOCRON_IT=1 (and HOLOCRON_IT_FFMPEG, HOLOCRON_IT_MEDIAMTX) for the end-to-end run")
	}
	r := &itRig{t: t, ffmpeg: os.Getenv("HOLOCRON_IT_FFMPEG"), seen: map[int]osx.ChildEntry{}, inspector: osx.OSInspector{}}
	srcMtx := os.Getenv("HOLOCRON_IT_MEDIAMTX")
	if r.ffmpeg == "" || srcMtx == "" {
		t.Skip("HOLOCRON_IT_FFMPEG and HOLOCRON_IT_MEDIAMTX are required")
	}
	r.scratch = t.TempDir()
	r.root = filepath.Join(r.scratch, "root")
	r.cfg = filepath.Join(r.scratch, "cfg", "holocron.config.json")
	for _, d := range []string{r.root, filepath.Dir(r.cfg), filepath.Join(r.scratch, "bin"), filepath.Join(r.scratch, "mtx"), filepath.Join(r.scratch, "camera"), filepath.Join(r.scratch, "local")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	r.mtx = filepath.Join(r.scratch, "bin", exeName("mediamtx"))
	copyExe(t, srcMtx, r.mtx)
	r.exe = os.Getenv("HOLOCRON_IT_EXE")
	if r.exe == "" {
		r.exe = filepath.Join(r.scratch, "bin", exeName("holocron"))
		out, err := exec.Command("go", "build", "-o", r.exe, ".").CombinedOutput()
		if err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
	}
	r.pass = fmt.Sprintf("fake-pass-%d", time.Now().UnixNano())
	r.env = append(os.Environ(),
		"HOLOCRON_CAMERA_USER=fakeuser", "HOLOCRON_CAMERA_PASSWORD="+r.pass,
		app.SandboxEnv+"=1", "SANDBOX_ONLY="+r.scratch,
		"LOCALAPPDATA="+filepath.Join(r.scratch, "local"), "XDG_STATE_HOME="+filepath.Join(r.scratch, "local"),
		"HOLOCRON_LOG=", "WSLENV=")
	t.Cleanup(r.cleanup)

	r.writeConfig()
	r.startCameras()

	// 1. up --detach: an honest start within 60 s
	t0 := time.Now()
	out, code := r.holocron(90*time.Second, "up", "--detach")
	t.Logf("up --detach (exit %d, %.1fs):\n%s", code, time.Since(t0).Seconds(), out)
	if code != 0 || !strings.Contains(out, "RECORDING") {
		r.dumpLog()
		t.Fatalf("up --detach failed")
	}
	r.firstStatus = time.Since(t0)
	r.record()

	// 2. both cameras recording: alive and writing segments
	if !r.waitFor(60*time.Second, func() bool {
		st := r.status()
		return st != nil && cameraAlive(st, "cam1") && cameraAlive(st, "cam2") && r.segments("cam1") >= 2 && r.segments("cam2") >= 2
	}) {
		r.dumpLog()
		t.Fatalf("both cameras did not record: %+v", r.status())
	}
	r.record()
	t.Logf("recording: cam1 %d segments, cam2 %d segments", r.segments("cam1"), r.segments("cam2"))
	r.checkListeners()

	// 3. cam disable cam2: it stops recording
	out, code = r.holocron(30*time.Second, "cam", "disable", "cam2")
	t.Logf("cam disable cam2 (exit %d):\n%s", code, out)
	if code != 0 || !strings.Contains(out, "reloaded") {
		t.Fatalf("cam disable failed")
	}
	if !r.waitFor(20*time.Second, func() bool { st := r.status(); return st != nil && !cameraAlive(st, "cam2") && cameraAlive(st, "cam1") }) {
		t.Fatalf("cam2 did not stop: %+v", r.status())
	}
	n2 := r.segments("cam2")
	stoppedAt := time.Now()

	// 4. log level supervisor debug: DEBUG records of the supervisor appear
	before := r.countLog("DEBUG [supervisor]")
	out, code = r.holocron(20*time.Second, "log", "level", "supervisor", "debug")
	t.Logf("log level (exit %d):\n%s", code, out)
	if code != 0 || before != 0 {
		t.Fatalf("log level failed (DEBUG supervisor lines before: %d)", before)
	}
	if !r.waitFor(20*time.Second, func() bool { return r.countLog("DEBUG [supervisor]") > 0 }) {
		t.Fatal("no DEBUG supervisor record after the level change")
	}

	// 5. config set an editable key
	out, code = r.holocron(20*time.Second, "config", "set", "MaxRetentionDays", "30")
	t.Logf("config set (exit %d):\n%s", code, out)
	if b, _ := os.ReadFile(r.cfg); code != 0 || !bytes.Contains(b, []byte(`"MaxRetentionDays": 30`)) {
		t.Fatalf("config set: %s", b)
	}

	// cam2 stays stopped while cam1 goes on
	n1 := r.segments("cam1")
	r.waitFor(time.Duration(itSegment+3)*time.Second, func() bool { return r.segments("cam1") > n1 })
	if got := r.segments("cam2"); got > n2+1 { // the segment open at the disable may close once
		t.Errorf("cam2 kept recording after the disable: %d -> %d in %.0fs", n2, got, time.Since(stoppedAt).Seconds())
	}
	if r.segments("cam1") <= n1 {
		t.Errorf("cam1 stopped recording (%d segments)", n1)
	}
	r.record()

	// 6. down: proved, 0 survivors
	t1 := time.Now()
	out, code = r.holocron(90*time.Second, "down")
	r.stopDur = time.Since(t1)
	t.Logf("down (exit %d, %.1fs):\n%s", code, r.stopDur.Seconds(), out)
	if code != 0 || !strings.Contains(out, "0 survivors") {
		t.Fatal("down did not prove the stop")
	}
	if alive := r.aliveSeen(); len(alive) != 0 {
		t.Fatalf("still alive after down: %v", alive)
	}
	st, _ := status.Read(filepath.Join(r.root, "_state", "status.json"))
	if st == nil || st.State != status.StateStopped || st.StopKind != status.StopKindRequested {
		t.Errorf("final status %+v", st)
	}
	for _, f := range []string{"ctl.addr", "ctl.secret", "host.json"} {
		if _, err := os.Stat(filepath.Join(r.root, "_state", f)); !os.IsNotExist(err) {
			t.Errorf("%s left after down: %v", f, err)
		}
	}
	r.leakScan()
	t.Logf("NUMBERS: time to first valid status %.1fs; segments cam1 %d cam2 %d; stop %.1fs; survivors 0; holocron processes seen %d",
		r.firstStatus.Seconds(), r.segments("cam1"), r.segments("cam2"), r.stopDur.Seconds(), len(r.seen))
	// idempotent
	if out, code = r.holocron(30*time.Second, "down"); code != 0 || !strings.Contains(out, "nothing is running") {
		t.Errorf("second down (exit %d): %s", code, out)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func copyExe(t *testing.T, from, to string) {
	b, err := os.ReadFile(from)
	must(t, err)
	must(t, os.WriteFile(to, b, 0o755))
}

func (r *itRig) writeConfig() {
	cfg := map[string]any{
		"RootPath": r.root, "FfmpegPath": r.ffmpeg, "SegmentSeconds": itSegment,
		"RestreamerEnabled": true, "RestreamerExePath": r.mtx, "RestreamerConfigPath": filepath.Join(r.scratch, "mtx", "mediamtx.yml"),
		"RelayBase": fmt.Sprintf("rtsp://127.0.0.1:%d", itRelayPort), "RelayApiBase": fmt.Sprintf("http://127.0.0.1:%d", itAPIPort),
		"LiveViewEnabled": false, "LiveConfigPath": filepath.Join(r.scratch, "mtx", "mediamtx-live.yml"),
		"LiveSecretPath":  filepath.Join(r.scratch, "local", "live_view_secret.txt"),
		"LiveWhepAddress": "127.0.0.1:18743", "LiveUdpAddress": "127.0.0.1:18744",
		"ReconcileSeconds": 2, "StatusIntervalSeconds": 2, "ArchiveScanSeconds": 3, "ReserveGB": 0, "PruneHysteresisGB": 0,
		"StopTimeoutSeconds": 30, "RelayStartupWaitSeconds": 20,
		"alerts": map[string]any{"desktop": map[string]any{"enabled": false}, "eventlog": map[string]any{"enabled": false}},
	}
	b, _ := json.MarshalIndent(cfg, "", "    ")
	must(r.t, os.WriteFile(r.cfg, append([]byte("\xef\xbb\xbf"), b...), 0o644))
	// IP carries the throwaway camera's port (host:port, as `cam add --ip` accepts)
	cams := fmt.Sprintf(`[{"Name":"cam1","IP":"127.0.0.1:%d","Quality":"cam1","Record":true,"Note":"kept"},{"Name":"cam2","IP":"127.0.0.1:%d","Quality":"cam2","Record":true}]`, itCamPort, itCamPort)
	must(r.t, os.WriteFile(filepath.Join(filepath.Dir(r.cfg), "cameras.json"), []byte(cams), 0o644))
}

const itCameraConfig = `logLevel: info
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
  all_others:
`

func (r *itRig) start(name string, dir string, exe string, args ...string) *exec.Cmd {
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	log, err := os.Create(filepath.Join(r.scratch, name+".log"))
	must(r.t, err)
	cmd.Stdout, cmd.Stderr = log, log
	hideConsole(cmd)
	must(r.t, cmd.Start())
	r.mu.Lock()
	r.started = append(r.started, cmd)
	r.mu.Unlock()
	go func() { cmd.Wait(); log.Close() }()
	r.t.Logf("started %s pid %d", name, cmd.Process.Pid)
	return cmd
}

func (r *itRig) startCameras() {
	camCfg := filepath.Join(r.scratch, "camera", "camera.yml")
	must(r.t, os.WriteFile(camCfg, []byte(fmt.Sprintf(itCameraConfig, itCamPort)), 0o644))
	r.start("camera-mediamtx", filepath.Dir(camCfg), r.mtx, camCfg)
	if !r.waitFor(15*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", itCamPort), time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	}) {
		r.t.Fatal("the throwaway camera MediaMTX does not listen")
	}
	for _, p := range []string{"cam1", "cam2"} {
		r.start("publisher-"+p, r.scratch, r.ffmpeg, "-hide_banner", "-loglevel", "warning", "-re",
			"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000",
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "10", "-pix_fmt", "yuv420p",
			"-c:a", "pcm_alaw", "-f", "rtsp", "-rtsp_transport", "tcp", fmt.Sprintf("rtsp://127.0.0.1:%d/%s", itCamPort, p))
	}
	time.Sleep(2 * time.Second)
}

// holocron runs one command of the real executable with the test environment.
func (r *itRig) holocron(timeout time.Duration, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"--root", r.root, "--config", r.cfg}, args...)
	cmd := exec.CommandContext(ctx, r.exe, full...)
	cmd.Env = r.env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
		buf.WriteString(err.Error())
	}
	return buf.String(), code
}

func (r *itRig) status() *status.Document {
	d, err := status.Read(filepath.Join(r.root, "_state", "status.json"))
	if err != nil {
		return nil
	}
	return d
}

func cameraAlive(d *status.Document, name string) bool {
	for _, c := range d.Cameras {
		if c.Name == name {
			return c.Alive
		}
	}
	return false
}

func (r *itRig) segments(cam string) int {
	m, _ := filepath.Glob(filepath.Join(r.root, cam, cam+"_*.mkv"))
	n := 0
	for _, f := range m {
		if fi, err := os.Stat(f); err == nil && fi.Size() > 0 {
			n++
		}
	}
	return n
}

func (r *itRig) waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return cond()
}

func (r *itRig) logText() string {
	b, _ := os.ReadFile(filepath.Join(r.root, "_logs", "holocron.log"))
	return string(b)
}

func (r *itRig) countLog(s string) int { return strings.Count(r.logText(), s) }

func (r *itRig) dumpLog() {
	txt := r.logText()
	if len(txt) > 12000 {
		txt = txt[len(txt)-12000:]
	}
	r.t.Logf("holocron.log (tail):\n%s", txt)
}

// record notes every holocron process (host, supervisor, children) with its identity.
func (r *itRig) record() {
	p := app.Paths{Root: r.root}
	note := func(pid int, role string) {
		if pid <= 0 {
			return
		}
		if info, err := r.inspector.Inspect(pid); err == nil {
			r.seen[pid] = osx.ChildEntry{Pid: pid, Start: info.Start, Image: info.Image, Role: role}
		}
	}
	if h, ok := app.ReadHostRecord(p); ok {
		note(h.Pid, "host")
	}
	if d := r.status(); d != nil {
		note(int(d.SupervisorPid), "supervisor")
		for _, c := range d.Cameras {
			if c.Pid != nil {
				note(int(*c.Pid), "ffmpeg:"+c.Name)
			}
		}
		if d.Restreamer != nil && d.Restreamer.Pid != nil {
			note(int(*d.Restreamer.Pid), "relay")
		}
	}
	entries, _ := osx.NewRegistry(osx.RegistryPath(p.State())).Load()
	for _, e := range entries {
		r.seen[e.Pid] = e
	}
}

func (r *itRig) aliveSeen() []string {
	var out []string
	for _, e := range r.seen {
		if ok, _ := osx.Verify(e, r.inspector, runtime.GOOS); ok {
			out = append(out, fmt.Sprintf("%s pid %d", e.Role, e.Pid))
		}
	}
	return out
}

// checkListeners: the relay MediaMTX listens on its two configured ports only (Windows netstat).
func (r *itRig) checkListeners() {
	d := r.status()
	if runtime.GOOS != "windows" || d == nil || d.Restreamer == nil || d.Restreamer.Pid == nil {
		return
	}
	out, err := exec.Command("netstat.exe", "-ano").Output()
	if err != nil {
		r.t.Logf("netstat: %v", err)
		return
	}
	pid := fmt.Sprint(*d.Restreamer.Pid)
	var got []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[len(f)-1] == pid && ((f[0] == "TCP" && len(f) >= 5 && f[3] == "LISTENING") || f[0] == "UDP") {
			got = append(got, f[0]+" "+f[1])
		}
	}
	want := map[string]bool{fmt.Sprintf("TCP 127.0.0.1:%d", itRelayPort): true, fmt.Sprintf("TCP 127.0.0.1:%d", itAPIPort): true}
	for _, g := range got {
		if !want[g] {
			r.t.Errorf("the relay listens on %s (outside the plan)", g)
		}
	}
	r.t.Logf("relay pid %s listeners: %v", pid, got)
}

// leakScan: the fake password is in no file the run wrote.
func (r *itRig) leakScan() {
	var hits []string
	filepath.Walk(r.scratch, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Size() > 64<<20 || strings.HasSuffix(path, ".mkv") || strings.HasSuffix(path, ".exe") {
			return nil
		}
		if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(r.pass)) {
			hits = append(hits, path)
		}
		return nil
	})
	if len(hits) != 0 {
		r.t.Errorf("the camera password is in %v", hits)
	}
}

func (r *itRig) cleanup() {
	// holocron first, the orderly way, if anything of it is still up
	if len(r.aliveSeen()) > 0 {
		out, code := r.holocron(90*time.Second, "down")
		r.t.Logf("cleanup down (exit %d): %s", code, out)
	}
	// then a sweep of every recorded pid that is still the same process
	for _, e := range r.seen {
		if ok, _ := osx.Verify(e, r.inspector, runtime.GOOS); ok {
			r.t.Errorf("cleanup: killing survivor %s pid %d", e.Role, e.Pid)
			if p, err := os.FindProcess(e.Pid); err == nil {
				p.Kill()
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.started {
		_ = c.Process.Kill() // ours; an exited one just reports an error
	}
	time.Sleep(500 * time.Millisecond)
}
