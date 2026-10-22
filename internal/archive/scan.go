package archive

import (
	"errors"
	"fmt"
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
	NewestClosedWrite time.Time // newest mtime of every file except Newest

	// Newest is the newest strict-pattern segment of the camera, ordered by modification time with
	// the NAME as tie-break (later name wins); "" when there is none. The pruner ALWAYS protects it,
	// as the PowerShell pruner protects the newest-created file. Name order alone is wrong across a
	// daylight-saving fall-back (spike SP4); mtime order is right.
	Newest      string
	NewestMTime time.Time
	NewestBytes int64

	// Recent: Newest was modified within segmentSeconds+60 s of the scan time (a file ahead of the
	// scan time counts as recent). Only the stall watchdog and the camera-down alert use it: a
	// camera whose newest file is not recent is not writing.
	Recent bool
	// Anomaly: Newest is dated more than 60 s in the future (clock skew); it is still Recent.
	Anomaly bool
}

// openGrace is how far past the segment length a file's mtime may lag and still be recent.
const openGrace = 60 * time.Second

// futureTolerance is how far ahead of the scan time an mtime may be before it is an Anomaly
// (the pruner's clock-sanity tolerance, IR 5.2 step 7).
const futureTolerance = 60 * time.Second

// newestSize reads the live size of the newest segment; a test replaces it to stand in for a
// listing that is stale (NTFS while the writer's handle is open).
var newestSize = LiveSize

// Scan reads one camera directory with DefaultMatcher. See Matcher.Scan.
func Scan(dir, container string, now time.Time, segmentSeconds int) (DirScan, error) {
	return DefaultMatcher.Scan(dir, container, now, segmentSeconds)
}

// Scan reads one camera directory. The directory name is the camera's safe name and container is
// the configured extension (".mkv"); both decide which files are strict-pattern segments. A missing
// directory is an empty scan, not an error. This replaces NTFS creation time as the "which file is
// open" signal (HLD section 6). The listing's size of the file being written is stale on Windows
// (NTFS updates a directory entry when the writer closes its handle), so the newest segment's size
// is read live through an open handle (LiveSize, as Get-CamLiveFileLength); a refused open keeps the
// listing's size. Closed files are fresh in the listing. Only the size is re-read, as in PowerShell:
// the open file's listing mtime may lag too, but never before its creation, and a healthy segment is
// closed before it is SegmentSeconds+60 s old, so Recent is not changed by the lag.
//
// Deviation from the task text: container is an extra parameter, because "matches the strict
// pattern" depends on the configured container.
func (m Matcher) Scan(dir, container string, now time.Time, segmentSeconds int) (DirScan, error) {
	now = now.UTC()
	scan := DirScan{Dir: dir, ScannedAt: now}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if fi, serr := os.Stat(dir); serr == nil && !fi.IsDir() { // Windows reports a file as "path not found"
			return scan, fmt.Errorf("%s is not a directory", dir)
		}
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	safe := filepath.Base(filepath.Clean(dir))

	type file struct {
		name  string
		mtime time.Time
	}
	var files []file
	var newest fs.FileInfo
	var newestM time.Time
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

		if !info.Mode().IsRegular() || !m.IsSegmentOf(e.Name(), safe, container) {
			continue
		}
		// entries are in name order, so !Before lets the later name win an mtime tie
		if newest == nil || !mtime.Before(newestM) {
			newest, newestM = info, mtime
		}
	}
	if newest != nil {
		scan.Newest = newest.Name()
		scan.NewestMTime = newestM
		scan.NewestBytes = newest.Size()
		if n, err := newestSize(filepath.Join(dir, newest.Name())); err == nil {
			scan.Bytes += n - scan.NewestBytes
			scan.NewestBytes = n
		}
		age := now.Sub(newestM)
		scan.Recent = age <= time.Duration(segmentSeconds)*time.Second+openGrace
		scan.Anomaly = -age > futureTolerance
	}
	for _, f := range files {
		if f.name != scan.Newest && f.mtime.After(scan.NewestClosedWrite) {
			scan.NewestClosedWrite = f.mtime
		}
	}
	return scan, nil
}

// Tracker remembers, between scans, when each camera's newest segment was first seen: the stand-in
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

// Observe records the newest segment of a scan and returns when it was first seen (the scan time
// of the first scan that reported this name for this directory). It returns the zero time when the
// scan has no segment, and forgets the directory.
func (t *Tracker) Observe(s DirScan) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.Newest == "" {
		delete(t.seen, s.Dir)
		return time.Time{}
	}
	if cur, ok := t.seen[s.Dir]; ok && cur.name == s.Newest {
		return cur.first
	}
	t.seen[s.Dir] = seenSegment{name: s.Newest, first: s.ScannedAt}
	return s.ScannedAt
}

// Forget drops a directory (a camera that was removed).
func (t *Tracker) Forget(dir string) {
	t.mu.Lock()
	delete(t.seen, dir)
	t.mu.Unlock()
}
