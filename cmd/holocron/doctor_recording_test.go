package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/shadow"
	"holocron/internal/status"
)

// runningWith writes a running status.json (this test process as the supervisor) changed by mod.
func (r *rig) runningWith(mod func(d *status.Document)) {
	r.t.Helper()
	r.running()
	p := filepath.Join(r.root, "_state", "status.json")
	d, err := status.Read(p)
	if err != nil {
		r.t.Fatal(err)
	}
	mod(d)
	if err := status.NewWriter(p, status.Options{}).Write(d); err != nil {
		r.t.Fatal(err)
	}
}

// segment writes a segment of camera started at start, last written at end, of size bytes.
func (r *rig) segment(camera string, start, end time.Time, size int) string {
	r.t.Helper()
	dir := filepath.Join(r.root, camera)
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, archive.FormatSegmentName(camera, start, ".mkv"))
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		r.t.Fatal(err)
	}
	os.Chtimes(p, end, end)
	return p
}

// lineOf is the doctor line whose title is title in group, with its fix line.
func lineOf(out, group, title string) (line, fix string) {
	in := false
	ls := strings.Split(out, "\n")
	for i, l := range ls {
		if strings.HasPrefix(l, "== ") {
			in = l == "== "+group+" =="
			continue
		}
		if in && len(l) > 5 && strings.HasPrefix(l[5:], title+" ") {
			if i+1 < len(ls) && strings.HasPrefix(ls[i+1], "     fix: ") {
				fix = strings.TrimPrefix(ls[i+1], "     fix: ")
			}
			return l, fix
		}
	}
	return "", ""
}

func TestDoctorSaysNotRunningOncePerRuntimeGroup(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	out, _, code := r.run("doctor")
	for g := range runtimeGroups {
		if !groupHasChecks(g) {
			continue
		}
		l, fix := lineOf(out, g, g)
		if !strings.HasPrefix(l, "warn") || !strings.Contains(l, "not checked: nothing is running (no status file") || fix != "holocron up --detach" {
			t.Errorf("%s: %q %q", g, l, fix)
		}
	}
	if code != 0 || !strings.HasPrefix(lastLine(out), "NOT RUNNING (") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func groupHasChecks(g string) bool {
	for _, c := range doctorTable {
		if c.group == g {
			return true
		}
	}
	return false
}

func lastLine(out string) string {
	ls := strings.Split(strings.TrimSpace(out), "\n")
	return ls[len(ls)-1]
}

func TestDoctorHeartbeatAge(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		want string
	}{{500 * time.Millisecond, "ok"}, {4 * time.Second, "warn"}, {7 * time.Second, "fail"}} {
		r := newRig(t)
		r.setConfig(`"StatusIntervalSeconds": 1,`)
		r.running()
		r.doctorTools(func(d *doctorDeps) { d.now = func() time.Time { return time.Now().Add(c.age) } })
		out, _, _ := r.run("doctor", "--group", "recording")
		if l, _ := lineOf(out, "recording", "heartbeat"); !strings.HasPrefix(l, c.want+" ") || !strings.Contains(l, "no holocron host process") {
			t.Errorf("age %s: %q\n%s", c.age, l, out)
		}
	}
}

func TestDoctorCameraRows(t *testing.T) {
	pid := int64(4242)
	for _, c := range []struct {
		name string
		mod  func(*status.Camera)
		want []string
	}{
		{"alive", func(c *status.Camera) { c.Pid = &pid }, []string{"ok   Front", "alive pid 4242, input relay, 0 restart(s)"}},
		{"down", func(c *status.Camera) {
			c.Alive, c.LastError, c.DownSince = false, "RTSP EOF", status.NewTime(time.Now().Add(-time.Minute))
		}, []string{"fail Front", "DOWN since", "RTSP EOF", `fix: holocron cam test "Front" ; holocron logs --component ffmpeg:Front --since 10m`}},
		{"restarts warn", func(c *status.Camera) { c.RestartsLastHour, c.LastError = 3, "exit 1" }, []string{"warn Front", "3 restart(s)", "last error: exit 1", "fix: holocron cam test"}},
		{"restarts fail", func(c *status.Camera) { c.RestartsLastHour = 10 }, []string{"fail Front", "10 restart(s)"}},
		{"direct", func(c *status.Camera) { c.InputMode = "direct" }, []string{"warn Front", "DIRECTLY", "second session", "fix: holocron doctor --group connections"}},
	} {
		r := newRig(t)
		r.runningWith(func(d *status.Document) { c.mod(&d.Cameras[0]) })
		r.doctorTools(nil)
		out, _, _ := r.run("doctor", "--group", "recording")
		sec := out[strings.Index(out, "== recording =="):]
		for _, w := range c.want {
			if !strings.Contains(sec, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, sec)
			}
		}
	}
}

// Incident 22 Oct 2026: a camera stream ended (RTSP EOF) and nothing new reached the disk. A file
// that grows between the two samples is recording; a frozen one with no recent closed segment is
// "not recording" with the camera's fix, whatever the supervisor says.
func TestDoctorGrowthCatchesAStalledCamera(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name   string
		grow   bool
		closed time.Duration // age of the newest closed segment's end
		want   string
		fix    string
	}{
		{"growing", true, 10 * time.Minute, "ok   Front          recording: ", ""},
		{"frozen", false, 10 * time.Minute, "fail Front          not recording: no new data for ", `holocron cam test "Front" ; holocron logs --component ffmpeg:Front --since 10m`},
		{"frozen after a rollover", false, 20 * time.Second, "warn Front          ", "run holocron doctor --group recording again in a minute"},
	} {
		r := newRig(t)
		r.running()
		r.segment("Front", now.Add(-c.closed-time.Minute), now.Add(-c.closed), 4000)
		cur := r.segment("Front", now.Add(-c.closed), now.Add(-c.closed+time.Second), 1000)
		r.doctorTools(func(d *doctorDeps) {
			d.sleep = func(context.Context, time.Duration) error {
				if c.grow {
					f, _ := os.OpenFile(cur, os.O_APPEND|os.O_WRONLY, 0)
					f.Write(make([]byte, 5000))
					f.Close()
				}
				return nil
			}
		})
		out, _, _ := r.run("doctor", "--group", "recording")
		sec := out[strings.Index(out, "== recording =="):]
		l, fix := growthLine(sec)
		if !strings.HasPrefix(l, c.want) || (c.fix != "" && !strings.HasPrefix(fix, c.fix)) {
			t.Errorf("%s: %q fix %q\n%s", c.name, l, fix, out)
		}
		if c.name == "frozen" {
			t.Logf("incident fixture (stalled camera):\n%s\n     fix: %s", l, fix)
		}
	}
}

// growthLine is the second "Front" line of the recording group (after the camera row).
func growthLine(sec string) (string, string) {
	ls := strings.Split(sec, "\n")
	n := 0
	for i, l := range ls {
		if len(l) > 5 && strings.HasPrefix(l[5:], "Front ") {
			if n++; n == 2 {
				fix := ""
				if i+1 < len(ls) && strings.HasPrefix(ls[i+1], "     fix: ") {
					fix = strings.TrimPrefix(ls[i+1], "     fix: ")
				}
				return l, fix
			}
		}
	}
	return "", ""
}

func TestDoctorGrowthWithoutAnySegment(t *testing.T) {
	r := newRig(t)
	r.running()
	r.doctorTools(func(d *doctorDeps) { d.sleep = func(context.Context, time.Duration) error { return nil } })
	out, _, _ := r.run("doctor", "--group", "recording")
	if l, _ := growthLine(out); !strings.Contains(l, "fail Front          not recording: no segment of the last 25 h") {
		t.Errorf("%q\n%s", l, out)
	}
}

func TestDoctorGaps(t *testing.T) {
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Minute)
	for _, c := range []struct {
		name, log, want string
	}{
		{"none", "", ""},
		{"unexplained", "", "warn Front          1 gap(s) larger than 2s in the last 24 h, longest 33 s"},
		{"at a restart", "WARN  [supervisor] exit 1 after 60s for Front; restart #1 in 3s", "info Front          1 gap(s)"},
	} {
		r := newRig(t)
		r.running()
		r.segment("Front", base, base.Add(time.Minute), 100)
		next := base.Add(time.Minute)
		if c.name != "none" {
			next = next.Add(33 * time.Second)
		}
		r.segment("Front", next, next.Add(time.Minute), 100)
		if c.log != "" {
			os.MkdirAll(filepath.Join(r.root, "_logs"), 0o755)
			os.WriteFile(filepath.Join(r.root, "_logs", "holocron.log"), []byte(base.Add(time.Minute+5*time.Second).Format("2006-01-02T15:04:05.000000Z07:00")+" "+c.log+"\n"), 0o644)
		}
		r.doctorTools(func(d *doctorDeps) { d.sleep = func(context.Context, time.Duration) error { return nil } })
		out, _, _ := r.run("--json", "doctor", "--group", "recording")
		var got string
		for _, l := range strings.Split(textOf(t, out), "\n") {
			if strings.HasPrefix(l, "recording.gaps ") {
				got = strings.TrimPrefix(l, "recording.gaps ")
			}
		}
		if c.want == "" {
			if !strings.HasPrefix(got, "ok   Front          no gap larger than 2s") {
				t.Errorf("%s: %q", c.name, got)
			}
		} else if !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}

// textOf renders a --json report one line per check: "<id> <level> <title> <detail>".
func textOf(t *testing.T, js string) string {
	t.Helper()
	var rep doctorReport
	if err := json.Unmarshal([]byte(js), &rep); err != nil {
		t.Fatalf("%v\n%s", err, js)
	}
	var b strings.Builder
	for _, c := range rep.Checks {
		b.WriteString(c.ID + " " + padRight(c.Level, 4) + " " + padRight(c.Title, 14) + " " + c.Detail + "\n")
	}
	return b.String()
}

func padRight(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}

type fakeProber struct {
	p   shadow.Probe
	err error
}

func (f fakeProber) Probe(context.Context, string) (shadow.Probe, error) { return f.p, f.err }

func TestDoctorSegmentSanityAndDeep(t *testing.T) {
	now := time.Now()
	alaw := shadow.Probe{Duration: 60, Video: &shadow.Stream{Codec: "h264"}, Audio: &shadow.Stream{Codec: "pcm_alaw"}}
	for _, c := range []struct {
		name  string
		last  int
		deep  bool
		probe fakeProber
		want  []string
	}{
		{"normal", 6000, false, fakeProber{}, []string{"ok", "100% of the median rate"}},
		{"half empty", 2000, false, fakeProber{}, []string{"warn", "33% of the median rate"}},
		{"deep ok", 6000, true, fakeProber{p: alaw}, []string{"ok", "ffprobe: video h264, audio pcm_alaw"}},
		{"deep no video", 6000, true, fakeProber{p: shadow.Probe{Audio: alaw.Audio}}, []string{"fail", "NO video stream"}},
		{"deep aac", 6000, true, fakeProber{p: shadow.Probe{Video: alaw.Video, Audio: &shadow.Stream{Codec: "aac"}}}, []string{"warn", "audio aac (pcm_alaw expected)"}},
		{"deep error", 6000, true, fakeProber{err: errors.New("ffprobe: exit 1")}, []string{"fail", "ffprobe: ffprobe: exit 1"}},
	} {
		r := newRig(t)
		r.running()
		start := now.Add(-50 * time.Minute)
		for i := 0; i < 4; i++ {
			size := 6000
			if i == 3 {
				size = c.last
			}
			r.segment("Front", start, start.Add(time.Minute), size)
			start = start.Add(time.Minute)
		}
		r.segment("Front", start, now, 10)
		r.doctorTools(func(d *doctorDeps) {
			d.sleep = func(context.Context, time.Duration) error { return nil }
			d.prober = func(config.Config) shadow.Prober { return c.probe }
		})
		args := []string{"--json", "doctor", "--group", "recording"}
		if c.deep {
			args = append(args, "--deep")
		}
		out, _, _ := r.run(args...)
		var got string
		for _, l := range strings.Split(textOf(t, out), "\n") {
			if strings.HasPrefix(l, "recording.segment ") {
				got = l
			}
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: no %q in %q", c.name, w, got)
			}
		}
	}
}
