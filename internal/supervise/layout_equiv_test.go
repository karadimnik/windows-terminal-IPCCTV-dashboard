package supervise

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
)

// oldScanRoot is ArchiveScanner.ScanRoot as it was before the layout library (its per-folder scan is
// archive.Matcher.Scan, which internal/archive proves equal to the old one).
func oldScanRoot(m archive.Matcher, container string, segSec int, root string, now time.Time) (ArchiveScan, error) {
	out := ArchiveScan{ScannedAt: now, Dirs: map[string]archive.DirScan{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "_") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil {
			continue
		}
		if archive.IsReparse(fi) {
			out.Skipped = append(out.Skipped, dir)
			continue
		}
		if !fi.IsDir() {
			continue
		}
		s, err := m.Scan(dir, container, now, segSec)
		if err != nil {
			continue
		}
		out.Dirs[e.Name()] = s
		out.Files += int64(s.Files)
		out.Bytes += s.Bytes
		if !s.OldestWrite.IsZero() && (out.Oldest.IsZero() || s.OldestWrite.Before(out.Oldest)) {
			out.Oldest = s.OldestWrite
		}
		if s.NewestWrite.After(out.Newest) {
			out.Newest = s.NewestWrite
		}
	}
	return out, nil
}

// TestArchiveScannerEqualsTheOldScan: the production scanner (layout and cache) gives what the old
// one gave, for the archive-wide scan and for every camera folder, on the first and on later calls.
func TestArchiveScannerEqualsTheOldScan(t *testing.T) {
	trees := archivetest.All(t, "../../testdata/golden", 60)
	for _, tr := range trees {
		root, _ := tr.Build(t, t.TempDir())
		m := archive.DefaultMatcher
		a := ArchiveScanner{Layout: archive.Layout{Root: root, Matcher: m, Container: tr.Container, Cache: archive.NewCache()}, SegmentSeconds: tr.SegmentSeconds}
		for call := 0; call < 3; call++ {
			want, werr := oldScanRoot(m, tr.Container, tr.SegmentSeconds, root, tr.Now)
			got, gerr := a.ScanRoot(root, tr.Now)
			if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(want, got) {
				t.Fatalf("%s call %d: ScanRoot\n old %+v %v\n new %+v %v", tr.Name, call, want, werr, got, gerr)
			}
			for name := range want.Dirs {
				dir := filepath.Join(root, name)
				w, _ := m.Scan(dir, tr.Container, tr.Now, tr.SegmentSeconds)
				g, err := a.Scan(dir, tr.Now)
				if err != nil || !reflect.DeepEqual(w, g) {
					t.Fatalf("%s/%s call %d: Scan\n old %+v\n new %+v %v", tr.Name, name, call, w, g, err)
				}
			}
		}
	}
	missing := filepath.Join(t.TempDir(), "nope")
	a := ArchiveScanner{Layout: archive.Layout{Root: missing, Cache: archive.NewCache()}}
	if s, err := a.ScanRoot(missing, time.Now()); err != nil || len(s.Dirs) != 0 {
		t.Errorf("a missing root: %+v %v", s, err)
	}
}
