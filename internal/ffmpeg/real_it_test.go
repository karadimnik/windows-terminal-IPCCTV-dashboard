package ffmpeg

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
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/osx"
)

// The real-ffmpeg checks. They run only when HOLOCRON_IT_FFMPEG names an ffmpeg (HOLOCRON_IT_FFPROBE
// names ffprobe; default: next to ffmpeg), so the default `go test` stays fast and offline. The
// relay and direct-list shapes also need HOLOCRON_IT_MEDIAMTX: the executable is COPIED into the
// test's own temp folder and run from there on loopback ports 18600-18639 only, with every
// listener it does not need switched off, and killed by its handle at the end. HOLOCRON_IT_TMP
// optionally names the folder under which the scratch folders are made.
//
//	18600 relay RTSP (anonymous)    18601 direct camera RTSP (login fakeuser / fakepass)
//
// See README.md for the Windows and WSL command lines.

const (
	itRelayPort  = 18600
	itCameraPort = 18601
	itUser       = "fakeuser"
	itPass       = "fakepass-NOT-A-SECRET"
	wrongPass    = "wrongpass-NOT-A-SECRET"
)

func itTools(t *testing.T) (ff, probe string) {
	t.Helper()
	ff = os.Getenv("HOLOCRON_IT_FFMPEG")
	if ff == "" {
		t.Skip("set HOLOCRON_IT_FFMPEG (and optionally HOLOCRON_IT_FFPROBE, HOLOCRON_IT_MEDIAMTX, HOLOCRON_IT_TMP) to run the real-ffmpeg checks")
	}
	probe = os.Getenv("HOLOCRON_IT_FFPROBE")
	if probe == "" {
		probe = filepath.Join(filepath.Dir(ff), "ffprobe")
		if runtime.GOOS == "windows" {
			probe += ".exe"
		}
	}
	v, err := CheckVersion(ff)
	if err != nil {
		t.Skipf("this ffmpeg is not usable by Holocron: %v", err)
	}
	t.Logf("ffmpeg %s (%s) on %s/%s", v, ff, runtime.GOOS, runtime.GOARCH)
	return ff, probe
}

func itTemp(t *testing.T) string {
	t.Helper()
	base := os.Getenv("HOLOCRON_IT_TMP")
	if base == "" {
		return t.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp(base, "w5-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// ---- ffprobe ----

type probeStream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
}
type probeResult struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func (p probeResult) duration() float64 { v, _ := strconv.ParseFloat(p.Format.Duration, 64); return v }
func (p probeResult) has(kind, codec string) bool {
	for _, s := range p.Streams {
		if s.CodecType == kind && (codec == "" || s.CodecName == codec) {
			return true
		}
	}
	return false
}

func ffprobe(t *testing.T, probe, file string) (probeResult, error) {
	t.Helper()
	out, err := exec.Command(probe, "-v", "error", "-print_format", "json", "-show_streams", "-show_format", file).Output()
	var r probeResult
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(out, &r)
}

// ---- processes we start ----

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

func startProc(t *testing.T, name, exe, dir string, args ...string) *proc {
	t.Helper()
	p := &proc{name: name, done: make(chan struct{})}
	p.cmd = exec.Command(exe, args...)
	p.cmd.Dir = dir
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
			p.cmd.Process.Kill()
			<-p.done
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
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

// listeners returns what a pid has bound, from netstat (Windows) or /proc/net (not needed on Linux here).
func listeners(t *testing.T, pid int) []string {
	t.Helper()
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

func mediamtxConfig(port int, auth bool) string {
	users := ""
	if auth {
		users = "authInternalUsers:\n- user: " + itUser + "\n  pass: " + itPass + "\n  permissions:\n  - action: publish\n  - action: read\n"
	}
	return fmt.Sprintf(`logLevel: warn
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
%spaths:
  cam:
`, port, users)
}

// startMediaMTX copies the executable, writes a hand config and waits for the one TCP listener; on
// Windows it asserts (netstat -ano, per pid) that nothing else is bound.
func startMediaMTX(t *testing.T, src, root string, port int, auth bool) *proc {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprintf("mtx%d", port))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "mediamtx")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "mediamtx.yml")
	if err := os.WriteFile(cfg, []byte(mediamtxConfig(port, auth)), 0o644); err != nil {
		t.Fatal(err)
	}
	p := startProc(t, fmt.Sprintf("mediamtx:%d", port), exe, dir, cfg)
	waitFor(t, "mediamtx to listen", 15*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	if runtime.GOOS == "windows" {
		got := listeners(t, p.Pid())
		if want := fmt.Sprintf("tcp 127.0.0.1:%d", port); len(got) != 1 || got[0] != want {
			t.Errorf("mediamtx pid %d listeners %v, want only [%s]", p.Pid(), got, want)
		} else {
			t.Logf("mediamtx pid %d listens only on %v", p.Pid(), got)
		}
	}
	return p
}

// startPublisher publishes a lavfi test pattern (H.264, pcm_alaw 8 kHz mono) into url.
func startPublisher(t *testing.T, ff, root, url string) *proc {
	return startProc(t, "publisher", ff, root, "-nostdin", "-loglevel", "error",
		"-re", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10",
		"-re", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000",
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "10",
		"-pix_fmt", "yuv420p", "-c:a", "pcm_alaw", "-ar", "8000", "-ac", "1",
		"-f", "rtsp", "-rtsp_transport", "tcp", url)
}

// cmdline returns the real command line of a live process.
func cmdline(t *testing.T, pid int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			fmt.Sprintf("(Get-CimInstance Win32_Process -Filter 'ProcessId=%d').CommandLine", pid)).Output()
		if err != nil {
			t.Fatalf("reading the command line of %d: %v", pid, err)
		}
		return strings.TrimSpace(string(out))
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatalf("reading the command line of %d: %v", pid, err)
	}
	return strings.ReplaceAll(string(b), "\x00", " ")
}

// ---- recording a camera with the real vector tail ----

var mkvProfile = config.ContainerProfile{Extension: ".mkv", SegmentFormat: "matroska"}

// lavfiArgs is a stand-in for the RTSP input: the same two lavfi sources as SP1, with the tail of
// the product's vector (mapping, segment muxer, pattern) appended unchanged.
func lavfiArgs(in Inputs, seconds int) []string {
	head := []string{"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-re", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10",
		"-re", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000",
		"-map", "0:v:0", "-map", "1:a:0?", "-c:v", "libx264", "-preset", "ultrafast", "-g", "10",
		"-pix_fmt", "yuv420p", "-c:a", "pcm_alaw", "-ar", "8000", "-ac", "1", "-t", strconv.Itoa(seconds)}
	tail := in.outputArgs()
	return append(head, tail[indexOf(tail, "-f"):]...) // from -f segment on: no -map, no -c copy
}

type segInfo struct {
	name     string
	start    time.Time
	mtime    time.Time
	size     int64
	probe    probeResult
	probeErr error
}

// segments lists the files of dir, parsed and probed, oldest name first.
func segments(t *testing.T, probe, dir string) []segInfo {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []segInfo
	for _, e := range es {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		s := segInfo{name: e.Name(), mtime: fi.ModTime(), size: fi.Size()}
		if _, start, ok := archive.ParseSegment(e.Name()); ok {
			s.start = start
		}
		if fi.Size() > 0 {
			s.probe, s.probeErr = ffprobe(t, probe, filepath.Join(dir, e.Name()))
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func secOfDay(t time.Time) int { return t.Hour()*3600 + t.Minute()*60 + t.Second() }

// checkSegments asserts the properties of the brief for the segments of one camera and logs the numbers.
// lastCut: the newest file was cut short by a hard kill, so its length and write time say nothing about its start.
func checkSegments(t *testing.T, segs []segInfo, safe string, segSeconds int, wantAudio, lastCut bool) {
	t.Helper()
	if len(segs) < 3 {
		t.Fatalf("%d files, want at least 3: %+v", len(segs), segs)
	}
	_, localOff := time.Now().Zone()
	for i, s := range segs {
		if !archive.IsSegmentOf(s.name, safe, ".mkv") {
			t.Errorf("%q is not accepted by IsSegmentOf(%q)", s.name, safe)
		}
		cam, start, ok := archive.ParseSegment(s.name)
		if !ok || cam != safe {
			t.Errorf("%q: ParseSegment gives %q %v", s.name, cam, ok)
		}
		if _, off := start.Zone(); off != localOff {
			t.Errorf("%q: written offset %d s, this machine's is %d s", s.name, off, localOff)
		}
		var desc string
		if s.size > 0 && s.probeErr == nil {
			dur := s.probe.duration()
			dev := start.Sub(s.mtime.Add(-time.Duration(dur * float64(time.Second))))
			desc = fmt.Sprintf("start=%s dur=%.3fs mtime-dur-vs-name=%+.2fs video=%v audio(pcm_alaw)=%v", start.Format("15:04:05-0700"), dur, dev.Seconds(), s.probe.has("video", "h264"), s.probe.has("audio", "pcm_alaw"))
			if killed := lastCut && i == len(segs)-1; !killed && (dev < -time.Second || dev > time.Second) {
				t.Errorf("%s: name start is %.2fs from the real start (mtime - duration)", s.name, dev.Seconds())
			}
			if !s.probe.has("video", "h264") || (wantAudio && !s.probe.has("audio", "pcm_alaw")) {
				t.Errorf("%s: streams %+v", s.name, s.probe.Streams)
			}
			if i > 0 && i < len(segs)-1 && (dur < float64(segSeconds)-1.2 || dur > float64(segSeconds)+1.2) {
				t.Errorf("%s: duration %.3fs, want about %d", s.name, dur, segSeconds)
			}
		} else {
			desc = fmt.Sprintf("size=%d probeErr=%v", s.size, s.probeErr)
			if i < len(segs)-1 {
				t.Errorf("%s: a closed segment is unreadable: %s", s.name, desc)
			}
		}
		if i > 0 { // after the first (partial) file the cuts are on multiples of the segment length of local time
			if m := secOfDay(start) % segSeconds; m > 1 {
				t.Errorf("%s: starts %d s after a multiple of %d s", s.name, m, segSeconds)
			}
		}
		t.Logf("  %s  %s", s.name, desc)
	}
}

func startChild(t *testing.T, exe string, args []string, tag string) *Child {
	t.Helper()
	c, err := Start(context.Background(), Spec{Name: tag, Exe: exe, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: ffmpeg pid %d", tag, c.Pid())
	t.Cleanup(func() { c.Kill(); c.Wait() })
	return c
}

func waitDone(t *testing.T, c *Child, within time.Duration) ExitInfo {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(within):
		c.Kill()
		info := c.Wait()
		t.Fatalf("child still running after %v; last lines %q", within, info.LastLines)
	}
	return c.Wait()
}

func inputsFor(root, safe string, segSeconds int, url string) Inputs {
	return Inputs{URL: url, RtspTransport: "tcp", TimeoutMicroseconds: 10000000, SegmentSeconds: segSeconds,
		Profile: mkvProfile, OutputPattern: SegmentPattern(root, safe, ".mkv")}
}

// TestRealFfmpeg is the whole real-binary check; every scenario runs once, for seconds.
func TestRealFfmpeg(t *testing.T) {
	ff, probe := itTools(t)
	base := itTemp(t)

	// (a), (b), (c): three camera names against a lavfi source, 10 s segments, ~25 s each.
	// The archive root holds a space. The "%" name proves the doubling in the pattern.
	root := filepath.Join(base, "arch ive")
	t.Run("lavfi names", func(t *testing.T) {
		for _, name := range []string{"Front", "Front Door", "Cam%Y"} {
			name := name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				safe, err := archive.SafeName(name)
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(root, safe)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				in := inputsFor(root, safe, 10, "")
				t.Logf("camera %q -> folder %q, pattern %q", name, safe, in.OutputPattern)
				c := startChild(t, ff, lavfiArgs(in, 25), safe)
				info := waitDone(t, c, 60*time.Second)
				if info.Code != 0 {
					t.Fatalf("%s; lines %q", info.Message(), info.LastLines)
				}
				t.Logf("%s (%.1fs)", info.Message(), info.RanFor.Seconds())
				segs := segments(t, probe, dir)
				checkSegments(t, segs, safe, 10, true, false)
				for _, s := range segs { // (b): the name is the literal one, not strftime-expanded
					if strings.HasPrefix(s.name, "Cam") && !strings.HasPrefix(s.name, "Cam%Y_") {
						t.Errorf("percent name was expanded: %s", s.name)
					}
				}
			})
		}
	})
	// after the parallel group: nothing but the three camera folders exists in the root (a strftime-expanded
	// folder such as "Cam2026" would show here)
	t.Run("archive root holds only the camera folders", func(t *testing.T) {
		es, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range es {
			got = append(got, e.Name())
		}
		sort.Strings(got)
		if want := []string{"Cam%Y", "Front", "Front_Door"}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("root holds %v, want %v", got, want)
		}
	})

	t.Run("hard kill leaves a partial segment", func(t *testing.T) {
		safe := "Kill_Cam"
		dir := filepath.Join(root, safe)
		os.MkdirAll(dir, 0o755)
		in := inputsFor(root, safe, 10, "")
		c := startChild(t, ff, lavfiArgs(in, 120), safe)
		count := func() int { es, _ := os.ReadDir(dir); return len(es) }
		waitFor(t, "the second segment to open", 40*time.Second, func() bool { return count() >= 2 })
		killedAt := time.Now()
		c.Kill()
		info := waitDone(t, c, 15*time.Second)
		t.Logf("killed after %v; %s; code %d", time.Since(killedAt).Round(time.Millisecond), info.Message(), info.Code)
		if info.Code == 0 {
			t.Error("a killed child reported exit code 0")
		}
		segs := segments(t, probe, dir)
		for _, s := range segs {
			t.Logf("  %s size=%d probeErr=%v", s.name, s.size, s.probeErr)
		}
		now := time.Now()
		scan, err := archive.Scan(dir, ".mkv", now, 10)
		if err != nil {
			t.Fatal(err)
		}
		last := segs[len(segs)-1]
		if scan.Files != len(segs) || scan.Newest != last.name {
			t.Errorf("Scan: files %d newest %q; directory has %d, newest %q", scan.Files, scan.Newest, len(segs), last.name)
		}
		if !scan.Recent {
			t.Errorf("Scan: the freshly killed newest file is not Recent: %+v", scan)
		}
		r := RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root, Matcher: archive.DefaultMatcher})
		after := segments(t, probe, dir)
		t.Logf("RemoveEmptySegment: removed %v foreign %d; %d -> %d files; last file size %d, readable=%v", r.Removed, r.Foreign, len(segs), len(after), last.size, last.probeErr == nil && last.size > 0)
		for _, s := range after {
			if s.size == 0 {
				t.Errorf("an empty file survived: %s", s.name)
			}
		}
		if last.size == 0 && len(r.Removed) != 1 {
			t.Errorf("the empty last file was not removed: %+v", r)
		}
		if last.size > 0 && len(r.Removed) != 0 {
			t.Errorf("a file with content was removed: %+v", r)
		}
		if len(segs) >= 2 && !segs[0].probe.has("video", "h264") {
			t.Errorf("the closed first file is unreadable: %v", segs[0].probeErr)
		}
	})

	t.Run("concat list quoting reaches real ffmpeg", func(t *testing.T) {
		dir := filepath.Join(base, "concat")
		os.MkdirAll(dir, 0o755)
		media := filepath.Join(dir, "it's here.mkv") // a quote and a space in the name
		if out, err := exec.Command(ff, "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-t", "1",
			"-c:v", "libx264", "-preset", "ultrafast", media).CombinedOutput(); err != nil {
			t.Fatalf("make media: %v %s", err, out)
		}
		list := filepath.Join(dir, "l.ffconcat")
		slash := strings.ReplaceAll(media, `\`, "/")
		if err := os.WriteFile(list, []byte("ffconcat version 1.0\nfile "+concatQuote(slash)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(ff, "-nostdin", "-hide_banner", "-loglevel", "error", "-f", "concat", "-safe", "0",
			"-protocol_whitelist", protocolWhitelist, "-i", list, "-f", "null", "-").CombinedOutput()
		if err != nil {
			t.Fatalf("ffmpeg refused the quoted name: %v\n%s", err, out)
		}
		// negative control: the unquoted form must fail, so the test would notice if ffmpeg stopped parsing quotes
		bad := filepath.Join(dir, "bad.ffconcat")
		os.WriteFile(bad, []byte("ffconcat version 1.0\nfile '"+slash+"'\n"), 0o600)
		if out, err := exec.Command(ff, "-nostdin", "-hide_banner", "-loglevel", "error", "-f", "concat", "-safe", "0",
			"-i", bad, "-f", "null", "-").CombinedOutput(); err == nil {
			t.Fatalf("an unescaped quote was accepted, the control is void: %s", out)
		}
	})

	mtx := os.Getenv("HOLOCRON_IT_MEDIAMTX")

	t.Run("relay shape: product vector over RTSP", func(t *testing.T) {
		if mtx == "" {
			t.Skip("set HOLOCRON_IT_MEDIAMTX for the RTSP relay shape")
		}
		startMediaMTX(t, mtx, base, itRelayPort, false)
		url := RelayURL(fmt.Sprintf("rtsp://127.0.0.1:%d", itRelayPort), "cam")
		startPublisher(t, ff, base, url)
		time.Sleep(2 * time.Second)
		safe, _ := archive.SafeName("Relay Cam")
		dir := filepath.Join(root, safe)
		os.MkdirAll(dir, 0o755)
		in := inputsFor(root, safe, 10, url)
		args, err := BuildArgs(in)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("ffmpeg %s", strings.Join(args, " "))
		c := startChild(t, ff, args, safe)
		time.Sleep(27 * time.Second)
		select {
		case <-c.Done():
			info := c.Wait()
			t.Fatalf("the recorder ended early: %s %q", info.Message(), info.LastLines)
		default:
		}
		c.Kill()
		info := waitDone(t, c, 15*time.Second)
		t.Logf("%s (hard kill); %d stderr lines kept: %q", info.Message(), len(info.LastLines), info.LastLines)
		segs := segments(t, probe, dir)
		checkSegments(t, segs, safe, 10, true, true)
		RemoveEmptySegment(dir, ".mkv", EmptyOptions{Root: root, Matcher: archive.DefaultMatcher})
	})

	t.Run("direct input through the list file", func(t *testing.T) {
		if mtx == "" {
			t.Skip("set HOLOCRON_IT_MEDIAMTX for the direct list-file shape")
		}
		startMediaMTX(t, mtx, base, itCameraPort, true)
		pubURL := fmt.Sprintf("rtsp://%s:%s@127.0.0.1:%d/cam", itUser, itPass, itCameraPort)
		pub := startPublisher(t, ff, base, pubURL)
		// positive control of the command-line reader: the publisher carries the password on its command line
		if cl := cmdline(t, pub.Pid()); !strings.Contains(cl, itPass) {
			t.Fatalf("positive control failed: the publisher's command line does not show the password: %s", cl)
		}
		time.Sleep(2 * time.Second)

		state := filepath.Join(base, "state")
		safe := "Direct_Cam"
		dir := filepath.Join(root, safe)
		os.MkdirAll(dir, 0o755)
		in := inputsFor(root, safe, 5, pubURL)
		list, cleanup, err := WriteInputList(osx.Current(), state, pubURL, in.TimeoutMicroseconds, in.RtspTransport)
		if err != nil {
			t.Fatal(err)
		}
		args, err := BuildArgsDirect(in, list)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Start(context.Background(), Spec{Name: safe, Exe: ff, Args: args, Cleanup: cleanup})
		if err != nil {
			cleanup()
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Kill(); c.Wait() })
		time.Sleep(1500 * time.Millisecond)
		cl := cmdline(t, c.Pid())
		t.Logf("recorder pid %d command line: %s", c.Pid(), cl)
		if strings.Contains(cl, itPass) || strings.Contains(cl, itUser) {
			t.Errorf("the credential is on the live command line")
		}
		time.Sleep(10 * time.Second)
		c.Kill()
		info := waitDone(t, c, 15*time.Second)
		t.Logf("%s; lines %q", info.Message(), info.LastLines)
		if _, err := os.Stat(list); !os.IsNotExist(err) {
			t.Errorf("the list file survived the child: %v", err)
		}
		segs := segments(t, probe, dir)
		if len(segs) < 2 {
			t.Fatalf("%d files recorded through the list", len(segs))
		}
		for _, s := range segs {
			t.Logf("  %s size=%d dur=%.2f video=%v audio=%v", s.name, s.size, s.probe.duration(), s.probe.has("video", "h264"), s.probe.has("audio", "pcm_alaw"))
		}
		first := segs[0]
		if first.probeErr != nil || !first.probe.has("video", "h264") || !first.probe.has("audio", "pcm_alaw") {
			t.Errorf("first file through the list is not a recording: %+v %v", first.probe, first.probeErr)
		}

		// a wrong password: ffmpeg echoes the URL in its error text; the Child's tail must not
		bad := fmt.Sprintf("rtsp://%s:%s@127.0.0.1:%d/cam", itUser, wrongPass, itCameraPort)
		blist, bclean, err := WriteInputList(osx.Current(), state, bad, 5000000, "tcp")
		if err != nil {
			t.Fatal(err)
		}
		bin := in
		bin.OutputPattern = SegmentPattern(base, "bad", ".mkv")
		bargs, _ := BuildArgsDirect(bin, blist)
		raw, _ := exec.Command(ff, bargs...).CombinedOutput()
		if strings.Contains(string(raw), wrongPass) {
			t.Logf("positive control: ffmpeg's own output DOES echo the password:\n%s", raw)
		} else {
			t.Logf("this ffmpeg did not echo the password in its error text:\n%s", raw)
		}
		bc, err := Start(context.Background(), Spec{Name: "bad", Exe: ff, Args: bargs, Cleanup: bclean})
		if err != nil {
			bclean()
			t.Fatal(err)
		}
		binfo := waitDone(t, bc, 30*time.Second)
		t.Logf("wrong password: %s; redacted lines %q", binfo.Message(), binfo.LastLines)
		if binfo.Code == 0 {
			t.Error("a wrong password recorded")
		}
		for _, l := range binfo.LastLines {
			if strings.Contains(l, wrongPass) {
				t.Errorf("unredacted line: %s", l)
			}
		}
	})
}
