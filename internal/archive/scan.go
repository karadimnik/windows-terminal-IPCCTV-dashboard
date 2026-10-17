package archive

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DirScan is one camera directory's picture, shared by the status writer and the stall watchdog.
// Times are UTC.
type DirScan struct {
	Dir       string
	ScannedAt time.Time

	Files int   // every non-directory entry, segment or not (as Get-CamDirScan counts)
	Bytes int64 // their total size

	OldestWrite       time.Time // zero when the directory has no files
	NewestWrite       time.Time
	NewestClosedWrite time.Time // newest mtime of every file except the open segment

	// Open is the open segment: the newest file matching the strict segment pattern whose
	// modification time is within segmentSeconds+60 s of the scan time; "" when there is none.
	// "Newest" is by the start instant parsed from the name (the later name wins, so the repeated
	// hour after a fall-back orders correctly), then by name.
	Open      string
	OpenMTime time.Time
	OpenBytes int64
}

// openGrace is how far past the segment length a file's mtime may lag and still count as open.
const openGrace = 60 * time.Second

// futureTolerance is how far ahead of the scan time an mtime may be and still count as open
// (the pruner's clock-sanity tolerance, IR 5.2 step 7).
const futureTolerance = 60 * time.Second

// Scan reads one camera directory. The directory name is the camera's safe name and container is
// the configured extension (".mkv"); both decide which files can be the open segment. A missing
// directory is an empty scan, not an error. This replaces NTFS creation time as the "open file"
// signal (HLD section 6); equal modification times are ordered by name (IR U5).
//
// Deviation from the task text: container is an extra parameter, because "matches the strict
// pattern" depends on the configured container.
func Scan(dir, container string, now time.Time, segmentSeconds int) (DirScan, error) {
	now = now.UTC()
	scan := DirScan{Dir: dir, ScannedAt: now}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	safe := filepath.Base(filepath.Clean(dir))
	window := time.Duration(segmentSeconds)*time.Second + openGrace

	type file struct {
		name  string
		mtime time.Time
	}
	var files []file
	var openStart time.Time
	var openInfo fs.FileInfo
	for _, e := range entries { // sorted by name
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil { // vanished between listing and stat
			continue
		}
		mtime := info.ModTime().UTC()
		scan.Files++
		scan.Bytes += info.Size()
		if scan.OldestWrite.IsZero() || mtime.Before(scan.OldestWrite) {
			scan.OldestWrite = mtime
		}
		if mtime.After(scan.NewestWrite) {
			scan.NewestWrite = mtime
		}
		files = append(files, file{e.Name(), mtime})

		if !info.Mode().IsRegular() || !IsSegmentOf(e.Name(), safe, container) {
			continue
		}
		if age := now.Sub(mtime); age > window || age < -futureTolerance {
			continue
		}
		_, start, _ := ParseSegment(e.Name())
		// entries are in name order, so >= lets the later name win a tie of start instants
		if openInfo == nil || !start.Before(openStart) {
			openStart, openInfo = start, info
		}
	}
	if openInfo != nil {
		scan.Open = openInfo.Name()
		scan.OpenMTime = openInfo.ModTime().UTC()
		scan.OpenBytes = openInfo.Size()
	}
	for _, f := range files {
		if f.name != scan.Open && f.mtime.After(scan.NewestClosedWrite) {
			scan.NewestClosedWrite = f.mtime
		}
	}
	return scan, nil
}

// Tracker remembers, between scans, when each camera's open segment was first seen: the stand-in
// for NTFS creation time in the stall watchdog and the camera-down alert. The supervisor keeps one.
// First-seen is no earlier than the supervisor's own first scan, like the PowerShell floor at the
// supervisor's start.
type Tracker struct {
	mu   sync.Mutex
	seen map[string]seenSegment
}

type seenSegment struct {
	name  string
	first time.Time
}

func NewTracker() *Tracker { return &Tracker{seen: map[string]seenSegment{}} }

// Observe records the open segment of a scan and returns when it was first seen (the scan time of
// the first scan that reported this name for this directory). It returns the zero time when the
// scan has no open segment, and forgets the directory.
func (t *Tracker) Observe(s DirScan) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.Open == "" {
		delete(t.seen, s.Dir)
		return time.Time{}
	}
	if cur, ok := t.seen[s.Dir]; ok && cur.name == s.Open {
		return cur.first
	}
	t.seen[s.Dir] = seenSegment{name: s.Open, first: s.ScannedAt}
	return s.ScannedAt
}

// Forget drops a directory (a camera that was removed).
func (t *Tracker) Forget(dir string) {
	t.mu.Lock()
	delete(t.seen, dir)
	t.mu.Unlock()
}
