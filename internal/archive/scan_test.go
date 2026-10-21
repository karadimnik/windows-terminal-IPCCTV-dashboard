package archive

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"holocron/internal/osx"
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

func TestScanNewestAndRecent(t *testing.T) {
	const seg = 900
	const (
		a = "Cam_A_20261017_113000+0300.mkv"
		b = "Cam_A_20261017_114500+0300.mkv"
		c = "Cam_A_20261017_120000+0300.mkv"
	)
	tests := []struct {
		name        string
		files       map[string]time.Duration // name -> age
		wantNewest  string
		wantRecent  bool
		wantAnomaly bool
		wantClosed  time.Duration // age of NewestClosedWrite; -1 (one nanosecond) means none
	}{
		{"newest and closed ones", map[string]time.Duration{a: 1800 * time.Second, b: 905 * time.Second, c: 3 * time.Second}, c, true, false, 905 * time.Second},
		{"recent by a hair", map[string]time.Duration{c: (seg + 60) * time.Second}, c, true, false, -1},
		{"just too old: still the newest (the pruner protects it), not recent", map[string]time.Duration{c: (seg + 61) * time.Second}, c, false, false, -1},
		{"ordered by mtime, not by name", map[string]time.Duration{b: 2 * time.Second, c: 100 * time.Second}, b, true, false, 100 * time.Second},
		{"equal mtimes: the later name wins", map[string]time.Duration{b: 10 * time.Second, c: 10 * time.Second}, c, true, false, 10 * time.Second},
		{"foreign files with a newer mtime are not segments", map[string]time.Duration{c: 20 * time.Second, "notes.txt": time.Second, c + ".tmp": time.Second, "Cam_B_20261017_120000+0300.mkv": time.Second, "Cam_A_20261017_120000+0300.mp4": time.Second}, c, true, false, time.Second},
		{"future mtime within tolerance is recent, no anomaly", map[string]time.Duration{c: -59 * time.Second}, c, true, false, -1},
		{"future mtime beyond tolerance is recent and an anomaly", map[string]time.Duration{c: -61 * time.Second}, c, true, true, -1},
		{"unicode digits in the name are foreign", map[string]time.Duration{"Cam_A_\u0662\u0660\u0662\u0666" + "1017_120000+0300.mkv": time.Second}, "", false, false, time.Second},
		{"empty directory", map[string]time.Duration{}, "", false, false, -1},
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
			s, err := Matcher{}.Scan(dir, ".mkv", t0, seg)
			if err != nil {
				t.Fatal(err)
			}
			if s.Newest != tc.wantNewest || s.Recent != tc.wantRecent || s.Anomaly != tc.wantAnomaly {
				t.Errorf("Newest/Recent/Anomaly = %q/%v/%v, want %q/%v/%v", s.Newest, s.Recent, s.Anomaly, tc.wantNewest, tc.wantRecent, tc.wantAnomaly)
			}
			if s.Files != len(tc.files) || s.Bytes != wantBytes {
				t.Errorf("Files/Bytes = %d/%d, want %d/%d", s.Files, s.Bytes, len(tc.files), wantBytes)
			}
			if tc.wantClosed == -1 && !s.NewestClosedWrite.IsZero() || tc.wantClosed != -1 && !s.NewestClosedWrite.Equal(t0.Add(-tc.wantClosed)) {
				t.Errorf("NewestClosedWrite = %v, want age %v", s.NewestClosedWrite, tc.wantClosed)
			}
			if tc.wantNewest != "" && (s.NewestBytes != 100 || !s.NewestMTime.Equal(t0.Add(-tc.files[tc.wantNewest]))) {
				t.Errorf("newest details: %d bytes, mtime %v", s.NewestBytes, s.NewestMTime)
			}
			if len(tc.files) > 0 && (s.NewestWrite.IsZero() || s.OldestWrite.IsZero() || s.NewestWrite.Before(s.OldestWrite)) {
				t.Errorf("oldest/newest %v %v", s.OldestWrite, s.NewestWrite)
			}
		})
	}
}

// Across the autumn change the repeated hour makes the later segment's name sort EARLIER (+0200 <
// +0300); mtime order picks the later pass (spike SP4). Control: name order alone would pick the wrong file.
func TestScanFallBackOrdersByMtime(t *testing.T) {
	dir := camDir(t, "Cam_A")
	first, second := "Cam_A_20261025_033000+0300.mkv", "Cam_A_20261025_033000+0200.mkv"
	put(t, dir, first, 10, 3600*time.Second+30*time.Second)
	put(t, dir, second, 10, 30*time.Second)
	s, _ := Matcher{}.Scan(dir, ".mkv", t0, 900)
	if s.Newest != second {
		t.Errorf("Newest = %q, want the +0200 pass %q", s.Newest, second)
	}
	if !(first > second) {
		t.Error("control: by name alone the +0300 file would win")
	}
}

// Case: insensitive on Windows, exact on Linux and macOS; both are tested on any host.
func TestScanAndMatchCase(t *testing.T) {
	dir := camDir(t, "Cam_A")
	put(t, dir, "cam_a_20261017_120000+0300.MKV", 10, time.Second)
	if s, _ := (Matcher{FoldCase: true}).Scan(dir, ".mkv", t0, 900); s.Newest == "" {
		t.Error("Windows matcher: a differently-cased file is a segment")
	}
	if s, _ := (Matcher{FoldCase: false}).Scan(dir, ".mkv", t0, 900); s.Newest != "" {
		t.Errorf("Linux matcher: a differently-cased file is foreign, got %q", s.Newest)
	}
	for _, c := range []struct {
		goos string
		fold bool
	}{{"windows", true}, {"linux", false}, {"darwin", false}} {
		if got := MatcherFor(&osx.Fake{OS: c.goos}).FoldCase; got != c.fold {
			t.Errorf("MatcherFor(%s).FoldCase = %v", c.goos, got)
		}
	}
	if DefaultMatcher.FoldCase != (runtime.GOOS == "windows") {
		t.Error("DefaultMatcher does not follow the OS")
	}
}

func TestScanMissingAndUnreadable(t *testing.T) {
	s, err := Scan(filepath.Join(t.TempDir(), "nope"), ".mkv", t0, 900)
	if err != nil || s.Files != 0 || s.Newest != "" || !s.ScannedAt.Equal(t0) {
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
	if s, _ := Scan(dir, ".mkv", t0, 900); s.Newest != "" {
		t.Errorf("an .mp4 is not a segment of an .mkv setup: %q", s.Newest)
	}
	if s, _ := Scan(dir, "mp4", t0, 900); s.Newest == "" {
		t.Error("container given without the dot must work")
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker()
	scan := func(open string, at time.Duration) DirScan {
		return DirScan{Dir: "/r/Cam_A", Newest: open, ScannedAt: t0.Add(at)}
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
	other := DirScan{Dir: "/r/Cam_B", Newest: "s2", ScannedAt: t0.Add(time.Hour)}
	if got := tr.Observe(other); !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("directories are tracked separately: %v", got)
	}
	tr.Forget("/r/Cam_B")
	if got := tr.Observe(DirScan{Dir: "/r/Cam_B", Newest: "s2", ScannedAt: t0.Add(2 * time.Hour)}); !got.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("after Forget: %v", got)
	}
}

// The newest segment's size is the live one (Get-CamLiveFileLength), not the listing's: a stand-in
// for NTFS, whose listing shows the size of the open segment as of its creation (0), reports more
// than the listing for the newest file only. A refused open keeps the listing value. Closed
// segments are never re-read.
func TestScanNewestSizeIsLive(t *testing.T) {
	const (
		closed = "Cam_A_20261017_114500+0300.mkv"
		open   = "Cam_A_20261017_120000+0300.mkv"
	)
	dir := camDir(t, "Cam_A")
	put(t, dir, closed, 300, 900*time.Second)
	put(t, dir, open, 0, 3*time.Second) // the stale listing: nothing written yet
	put(t, dir, "notes.txt", 7, time.Hour)
	var asked []string
	live := map[string]int64{open: 150 << 20}
	var fail error
	orig := newestSize
	t.Cleanup(func() { newestSize = orig })
	newestSize = func(p string) (int64, error) {
		asked = append(asked, filepath.Base(p))
		if fail != nil {
			return -1, fail
		}
		return live[filepath.Base(p)], nil
	}
	s, err := Scan(dir, ".mkv", t0, 900)
	if err != nil {
		t.Fatal(err)
	}
	if s.Newest != open || s.NewestBytes != 150<<20 || s.Bytes != 150<<20+300+7 || s.Files != 3 {
		t.Errorf("live size: newest %s %d bytes, total %d in %d files; want %s %d, total %d in 3", s.Newest, s.NewestBytes, s.Bytes, s.Files, open, 150<<20, 150<<20+307)
	}
	if len(asked) != 1 || asked[0] != open {
		t.Errorf("live size asked for %v, want the newest segment only", asked)
	}
	fail = os.ErrPermission
	s, _ = Scan(dir, ".mkv", t0, 900)
	if s.NewestBytes != 0 || s.Bytes != 307 {
		t.Errorf("refused open: newest %d bytes, total %d; want the listing (0, 307)", s.NewestBytes, s.Bytes)
	}
}
