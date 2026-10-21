package shadow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
)

// The real-media checks make tiny MKV files (4 s, h264 + pcm_alaw 8000 Hz mono, as the Tapo cameras
// give) with a real ffmpeg and read them back with a real ffprobe. They run when both are found
// (HOLOCRON_IT_FFMPEG / HOLOCRON_IT_FFPROBE, else PATH) and skip otherwise.

func tool(t *testing.T, env, name string) string {
	t.Helper()
	if p := os.Getenv(env); p != "" {
		return p
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found (set %s or put it on PATH)", name, env)
	}
	return p
}

type media struct{ base, big, noAudio, ffprobe string }

func makeMedia(t *testing.T) media {
	t.Helper()
	ff, probe := tool(t, "HOLOCRON_IT_FFMPEG", "ffmpeg"), tool(t, "HOLOCRON_IT_FFPROBE", "ffprobe")
	dir := t.TempDir()
	gen := func(name string, extra ...string) string {
		out := filepath.Join(dir, name)
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000", "-t", "4"}
		args = append(args, extra...)
		args = append(args, "-y", out)
		if b, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot make the sample (%v): %s", err, b)
		}
		return out
	}
	return media{
		base:    gen("base.mkv", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-pix_fmt", "yuv420p", "-c:a", "pcm_alaw", "-ac", "1", "-ar", "8000"),
		big:     gen("big.mkv", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "5", "-pix_fmt", "yuv420p", "-c:a", "pcm_alaw", "-ac", "1", "-ar", "8000"),
		noAudio: gen("noaudio.mkv", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-pix_fmt", "yuv420p", "-an"),
		ffprobe: probe,
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
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// realArchive puts one 4 s file per entry of srcs into root/folder, 4 s apart, an hour ago.
func realArchive(t *testing.T, root string, start time.Time, srcs []string) {
	t.Helper()
	for i, src := range srcs {
		st := start.Add(time.Duration(4*i) * time.Second)
		to := filepath.Join(root, camera, archive.FormatSegmentName(camera, st, ".mkv"))
		if src == "" {
			os.MkdirAll(filepath.Dir(to), 0o755)
			os.WriteFile(to, []byte("this is not a matroska file"), 0o644)
		} else {
			copyFile(t, src, to)
		}
		end := st.Add(4 * time.Second)
		os.Chtimes(to, end, end)
	}
}

func realOptions(old, new string, m media) Options {
	return Options{Old: old, New: new, SegmentSeconds: 4, StartTolerance: time.Second, DurationTolerance: 500 * time.Millisecond, GapTolerance: 500 * time.Millisecond,
		Prober: FFprobe{Path: m.ffprobe}, Matcher: archive.DefaultMatcher}
}

func TestRealMediaScenarios(t *testing.T) {
	m := makeMedia(t)
	start := time.Now().Add(-time.Hour).Truncate(4 * time.Second)
	for _, tc := range []struct {
		name      string
		old, new  []string
		wantEqual bool
		want      string
	}{
		{"identical", []string{m.base, m.base, m.base, m.base}, []string{m.base, m.base, m.base, m.base}, true, ""},
		{"a bigger file in new", []string{m.base, m.base, m.base, m.base}, []string{m.base, m.big, m.base, m.base}, false, "size ratio new/old"},
		{"no audio in new", []string{m.base, m.base, m.base, m.base}, []string{m.base, m.base, m.noAudio, m.base}, false, "has no audio stream"},
		{"an unreadable file", []string{m.base, m.base, m.base, m.base}, []string{m.base, "", m.base, m.base}, false, "cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			old, new := filepath.Join(base, "old archive"), filepath.Join(base, "new archive")
			realArchive(t, old, start, tc.old)
			realArchive(t, new, start, tc.new)
			rep, err := Compare(context.Background(), realOptions(old, new, m))
			if err != nil {
				t.Fatal(err)
			}
			c := rep.Cameras[0]
			if c.Equal != tc.wantEqual {
				t.Fatalf("equal = %v\n%q\n%+v", c.Equal, c.Differences, c.Pairs)
			}
			if tc.want != "" && !hasDiff(c, tc.want) {
				t.Errorf("no difference contains %q: %q", tc.want, c.Differences)
			}
			if tc.wantEqual {
				// the last file's estimated end is its mtime, so every pair but possibly the last is inside the window
				if len(c.Pairs) < 3 {
					t.Fatalf("pairs %d", len(c.Pairs))
				}
				p := c.Pairs[0]
				if p.Kind != "full" || p.OldDurSec < 3.9 || p.OldDurSec > 4.2 || p.NewLayout != "h264 320x240, pcm_alaw 8000 Hz 1ch" {
					t.Errorf("%+v", p)
				}
			}
		})
	}
}

func TestFFprobeFailures(t *testing.T) {
	m := makeMedia(t)
	if _, err := (FFprobe{Path: filepath.Join(t.TempDir(), "no-such-ffprobe")}).Probe(context.Background(), m.base); err == nil {
		t.Error("a missing ffprobe should fail")
	}
	if _, err := (FFprobe{Path: m.ffprobe}).Probe(context.Background(), filepath.Join(t.TempDir(), "missing.mkv")); err == nil || !strings.Contains(err.Error(), "ffprobe") {
		t.Errorf("a missing file: %v", err)
	}
	p, err := (FFprobe{Path: m.ffprobe}).Probe(context.Background(), m.base)
	if err != nil || p.Video == nil || p.Audio == nil || p.Audio.Codec != "pcm_alaw" || p.Audio.SampleRate != 8000 || p.Audio.Channels != 1 || p.Duration < 3.9 || p.Duration > 4.2 {
		t.Fatalf("%+v %v", p, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (FFprobe{Path: m.ffprobe}).Probe(ctx, m.base); err == nil {
		t.Error("a cancelled context should fail")
	}
}

func TestParseProbeWithoutADuration(t *testing.T) {
	// a Matroska file whose writer was killed: streams and size, no duration
	p, err := ParseProbe([]byte(`{"streams":[{"codec_type":"video","codec_name":"h264","width":2560,"height":1440},{"codec_type":"audio","codec_name":"pcm_alaw","sample_rate":"8000","channels":1}],"format":{"size":"2097152"}}`))
	if err != nil || !p.NoDuration || p.Duration != 0 || p.Video == nil || p.Audio == nil {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestParseProbe(t *testing.T) {
	p, err := ParseProbe([]byte(`{"streams":[{"codec_type":"video","codec_name":"h264","width":2560,"height":1440},{"codec_type":"audio","codec_name":"pcm_alaw","sample_rate":"8000","channels":1}],"format":{"duration":"900.521000"}}`))
	if err != nil || p.Duration != 900.521 || p.Video.Width != 2560 || p.Audio.SampleRate != 8000 || p.Videos != 1 || p.Audios != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{``, `{}`, `{"format":{"duration":"N/A"}}`, `{"format":{"duration":"0"}}`, `not json`} {
		if _, err := ParseProbe([]byte(bad)); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
