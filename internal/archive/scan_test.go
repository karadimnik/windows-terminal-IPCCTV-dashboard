package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// put creates a file of the given size with mtime = t0 - age.
func put(t *testing.T, dir, name string, size int, age time.Duration) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, t0.Add(-age), t0.Add(-age)); err != nil {
		t.Fatal(err)
	}
}

func camDir(t *testing.T, name string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestScanOpenSegment(t *testing.T) {
	const seg = 900
	const (
		a = "Cam_A_20261017_113000+0300.mkv"
		b = "Cam_A_20261017_114500+0300.mkv"
		c = "Cam_A_20261017_120000+0300.mkv"
	)
	tests := []struct {
		name       string
		files      map[string]time.Duration // name -> age
		wantOpen   string
		wantClosed time.Duration // age of NewestClosedWrite; -1 (one nanosecond) means none
	}{
		{"open one and closed ones", map[string]time.Duration{a: 1800 * time.Second, b: 905 * time.Second, c: 3 * time.Second}, c, 905 * time.Second},
		{"inside window by a hair", map[string]time.Duration{c: (seg + 60) * time.Second}, c, -1},
		{"outside window: nothing is open", map[string]time.Duration{c: (seg + 61) * time.Second}, "", (seg + 61) * time.Second},
		{"two qualify: the later name wins even with the older mtime", map[string]time.Duration{b: 2 * time.Second, c: 100 * time.Second}, c, 2 * time.Second},
		{"equal mtimes: the later name wins", map[string]time.Duration{b: 10 * time.Second, c: 10 * time.Second}, c, 10 * time.Second},
		{"a foreign file with a newer mtime is not open", map[string]time.Duration{c: 20 * time.Second, "notes.txt": time.Second, "Cam_A_20261017_120000+0300.mkv.tmp": time.Second, "Cam_B_20261017_120000+0300.mkv": time.Second, "Cam_A_20261017_120000+0300.mp4": time.Second}, c, time.Second},
		{"future mtime within tolerance", map[string]time.Duration{c: -59 * time.Second}, c, -1},
		{"future mtime beyond tolerance is not open", map[string]time.Duration{c: -61 * time.Second}, "", -61 * time.Second},
		{"unicode digits in the name are foreign", map[string]time.Duration{"Cam_A_٢٠٢٦" + "1017_120000+0300.mkv": time.Second}, "", time.Second},
		{"empty directory", map[string]time.Duration{}, "", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := camDir(t, "Cam_A")
			var wantBytes int64
			for n, age := range tc.files {
				put(t, dir, n, 100, age)
				wantBytes += 100
			}
			if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil { // not counted
				t.Fatal(err)
			}
			s, err := Scan(dir, ".mkv", t0, seg)
			if err != nil {
				t.Fatal(err)
			}
			if s.Open != tc.wantOpen {
				t.Errorf("Open = %q, want %q", s.Open, tc.wantOpen)
			}
			if s.Files != len(tc.files) || s.Bytes != wantBytes {
				t.Errorf("Files/Bytes = %d/%d, want %d/%d", s.Files, s.Bytes, len(tc.files), wantBytes)
			}
			if tc.wantClosed == -1 && !s.NewestClosedWrite.IsZero() || tc.wantClosed != -1 && !s.NewestClosedWrite.Equal(t0.Add(-tc.wantClosed)) {
				t.Errorf("NewestClosedWrite = %v, want age %v", s.NewestClosedWrite, tc.wantClosed)
			}
			if tc.wantOpen != "" {
				if s.OpenBytes != 100 || !s.OpenMTime.Equal(t0.Add(-tc.files[tc.wantOpen])) {
					t.Errorf("open details: %d bytes, mtime %v", s.OpenBytes, s.OpenMTime)
				}
			}
			if len(tc.files) > 0 && (s.NewestWrite.IsZero() || s.OldestWrite.IsZero() || s.NewestWrite.Before(s.OldestWrite)) {
				t.Errorf("oldest/newest %v %v", s.OldestWrite, s.NewestWrite)
			}
		})
	}
}

// Across the autumn change the repeated hour makes the later segment's name sort EARLIER; the open
// segment is the later instant. Negative control: a name-only rule would pick the wrong file.
func TestScanRepeatedHourPicksLaterInstant(t *testing.T) {
	dir := camDir(t, "Cam_A")
	first, second := "Cam_A_20261025_033000+0300.mkv", "Cam_A_20261025_033000+0200.mkv"
	put(t, dir, first, 10, 30*time.Second)
	put(t, dir, second, 10, 30*time.Second) // equal mtimes
	s, _ := Scan(dir, ".mkv", t0, 900)
	if s.Open != second {
		t.Errorf("Open = %q, want the +0200 pass %q", s.Open, second)
	}
	if !(second < first) {
		t.Error("control: by name alone the +0200 file sorts first")
	}
}

func TestScanMissingAndUnreadable(t *testing.T) {
	s, err := Scan(filepath.Join(t.TempDir(), "nope"), ".mkv", t0, 900)
	if err != nil || s.Files != 0 || s.Open != "" || !s.ScannedAt.Equal(t0) {
		t.Errorf("missing dir: %+v, %v", s, err)
	}
	f := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(f, nil, 0o644)
	if _, err := Scan(f, ".mkv", t0, 900); err == nil { // a file is not a directory: a real error, not "empty"
		t.Error("scanning a regular file must fail")
	}
}

func TestScanOtherContainer(t *testing.T) {
	dir := camDir(t, "Cam_A")
	put(t, dir, "Cam_A_20261017_120000+0300.mp4", 10, time.Second)
	if s, _ := Scan(dir, ".mkv", t0, 900); s.Open != "" {
		t.Errorf("an .mp4 is not a segment of an .mkv setup: %q", s.Open)
	}
	if s, _ := Scan(dir, "mp4", t0, 900); s.Open == "" {
		t.Error("container given without the dot must work")
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker()
	scan := func(open string, at time.Duration) DirScan {
		return DirScan{Dir: "/r/Cam_A", Open: open, ScannedAt: t0.Add(at)}
	}
	if got := tr.Observe(scan("s1", 0)); !got.Equal(t0) {
		t.Errorf("first sight %v", got)
	}
	if got := tr.Observe(scan("s1", 5*time.Second)); !got.Equal(t0) {
		t.Errorf("same segment later: %v, want the first-seen time", got)
	}
	if got := tr.Observe(scan("s2", 900*time.Second)); !got.Equal(t0.Add(900 * time.Second)) {
		t.Errorf("new segment: %v", got)
	}
	if got := tr.Observe(scan("", 905*time.Second)); !got.IsZero() {
		t.Errorf("no open segment: %v", got)
	}
	if got := tr.Observe(scan("s2", 910*time.Second)); !got.Equal(t0.Add(910 * time.Second)) {
		t.Errorf("after a gap the same name is first seen again: %v", got)
	}
	other := DirScan{Dir: "/r/Cam_B", Open: "s2", ScannedAt: t0.Add(time.Hour)}
	if got := tr.Observe(other); !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("directories are tracked separately: %v", got)
	}
	tr.Forget("/r/Cam_B")
	if got := tr.Observe(DirScan{Dir: "/r/Cam_B", Open: "s2", ScannedAt: t0.Add(2 * time.Hour)}); !got.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("after Forget: %v", got)
	}
}
