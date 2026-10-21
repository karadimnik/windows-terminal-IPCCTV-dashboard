package shadow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"holocron/internal/archive"
)

var (
	zone3 = time.FixedZone("", 3*3600)
	zone2 = time.FixedZone("", 2*3600)
	// testNow is "now" of the table tests: everything older than 90 s is closed.
	testNow = time.Date(2026, 10, 21, 18, 0, 0, 0, zone3)
)

func at(h, m, s int) time.Time { return time.Date(2026, 10, 21, h, m, s, 0, zone3) }

// spec is one generated segment.
type spec struct {
	start   time.Time
	dur     float64 // seconds (0: 900)
	size    int64   // 0: 150000 bytes per second
	noAudio bool
	noDur   bool // readable but never finalized
	audio   *Stream
	mod     time.Time // zero: the end of the segment
}

func (s spec) d() float64 {
	if s.dur == 0 {
		return 900
	}
	return s.dur
}

// fakeProber answers from the specs, keyed by path.
type fakeProber struct {
	m    map[string]Probe
	errs map[string]error
	log  []string
}

func newFake() *fakeProber { return &fakeProber{m: map[string]Probe{}, errs: map[string]error{}} }

func (f *fakeProber) Probe(_ context.Context, path string) (Probe, error) {
	f.log = append(f.log, path)
	if err := f.errs[path]; err != nil {
		return Probe{}, err
	}
	p, ok := f.m[path]
	if !ok {
		return Probe{}, fmt.Errorf("fake: no probe for %s", path)
	}
	return p, nil
}

type sandbox struct {
	t        *testing.T
	old, new string
	fp       *fakeProber
}

// newSandbox makes two roots whose names hold a space, as a Windows path often does.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	base := t.TempDir()
	s := &sandbox{t: t, old: filepath.Join(base, "old archive"), new: filepath.Join(base, "new archive"), fp: newFake()}
	for _, d := range []string{s.old, s.new} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// put writes one segment (sparse) with its mtime and registers its probe.
func (s *sandbox) put(root, folder string, sp spec) string {
	s.t.Helper()
	dir := filepath.Join(root, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	path := filepath.Join(dir, archive.FormatSegmentName(folder, sp.start, ".mkv"))
	size := sp.size
	if size == 0 {
		size = int64(sp.d() * 150000)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		s.t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		s.t.Fatal(err)
	}
	mod := sp.mod
	if mod.IsZero() {
		mod = sp.start.Add(time.Duration(sp.d() * float64(time.Second)))
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		s.t.Fatal(err)
	}
	p := Probe{Duration: sp.d(), Video: &Stream{Codec: "h264", Width: 1920, Height: 1080}, Videos: 1}
	if !sp.noAudio {
		p.Audio, p.Audios = &Stream{Codec: "pcm_alaw", SampleRate: 8000, Channels: 1}, 1
		if sp.audio != nil {
			p.Audio = sp.audio
		}
	}
	if sp.noDur {
		p.Duration, p.NoDuration = 0, true
	}
	s.fp.m[path] = p
	return path
}

func (s *sandbox) putAll(root, folder string, specs []spec) {
	for _, sp := range specs {
		s.put(root, folder, sp)
	}
}

// series is n consecutive full segments from start, each step seconds long, with `jitter`
// seconds added to the name time of every start (the recorder opens a file just after the boundary).
func series(start time.Time, n int, step int, jitter int) []spec {
	var out []spec
	for i := 0; i < n; i++ {
		out = append(out, spec{start: start.Add(time.Duration(i*step+jitter) * time.Second), dur: float64(step)})
	}
	return out
}

func (s *sandbox) options() Options {
	return Options{Old: s.old, New: s.new, Now: testNow, Prober: s.fp, Matcher: archive.DefaultMatcher}
}
