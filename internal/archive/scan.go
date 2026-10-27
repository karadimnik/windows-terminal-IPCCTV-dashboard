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

// DirScan is one camera's picture, shared by the status writer and the stall watchdog. Times are
// UTC. Layout.Scan, ScanAll and ScanSpool say which folders it covers.
type DirScan struct {
	Dir       string // the camera folder
	ScannedAt time.Time

	Files int   // every non-directory entry, segment or not (as Get-CamDirScan counts)
	Bytes int64 // their total size

	Filed      int   // of Files, those in day folders (ScanAll; 0 for Scan and ScanSpool)
	FiledBytes int64 // their total size

	OldestWrite       time.Time // zero when the directory has no files
	NewestWrite       time.Time
	NewestClosedWrite time.Time // newest mtime of every spool file except Newest, and of the day folders' segments the spool-first rule read

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
	ls, err := listFolder(dir, "")
	if err != nil {
		return scanFailed(dir, now, err)
	}
	l := Layout{Matcher: m, Container: container}
	return l.scanOf(dir, filepath.Base(filepath.Clean(dir)), ls.files, nil, false, now, segmentSeconds), nil
}

// Scan is the status tick's picture of the camera, by the spool-first rule (see Layout): Files,
// Bytes and the write times are the spool's; Newest and NewestClosedWrite come from the spool and,
// only while the spool and the day folders read so far hold fewer than two segments, from day
// folders read newest first. With the open and the newest closed segment in the spool (the L4
// mover's contract) that is one listing of the camera folder, however many days are filed. A
// missing camera folder is an empty scan, not an error. Through the Cache when one is set.
func (l Layout) Scan(camera string, now time.Time, segmentSeconds int) (DirScan, error) {
	return l.scan(camera, now, segmentSeconds, scanLive)
}

// ScanAll is Scan with the totals over every day folder too: Files, Bytes, the write times, Filed
// and FiledBytes count every non-directory entry of the spool and of the day folders (foreign and
// misfiled files included, as Files counts foreign files in the spool). Newest, Recent and
// NewestClosedWrite are exactly Scan's. The archive-wide scan (disk figures) uses it.
func (l Layout) ScanAll(camera string, now time.Time, segmentSeconds int) (DirScan, error) {
	return l.scan(camera, now, segmentSeconds, scanAll)
}

// ScanSpool is the scan of the spool alone, as before day folders: no day folder is read, so
// Newest is "" when the spool holds no segment: the flat view, the reference the tests compare
// the layout's readers with.
func (l Layout) ScanSpool(camera string, now time.Time, segmentSeconds int) (DirScan, error) {
	return l.scan(camera, now, segmentSeconds, scanSpool)
}

type scanMode int

const (
	scanSpool scanMode = iota
	scanLive
	scanAll
)

func (l Layout) scan(camera string, now time.Time, segmentSeconds int, mode scanMode) (DirScan, error) {
	dir := l.CameraDir(camera)
	spool, days, err := l.Spool(camera)
	if err != nil {
		return scanFailed(dir, now, err)
	}
	var filed [][]File // newest day first
	seen := l.spoolSegments(spool, camera)
	for _, d := range days {
		if mode == scanSpool || (mode == scanLive && seen >= 2) {
			break
		}
		df, err := l.DayFiles(d)
		if err != nil { // vanished since the camera folder was listed
			continue
		}
		filed = append(filed, df)
		seen += l.daySegments(df, camera)
	}
	return l.scanOf(dir, camera, spool, filed, mode == scanAll, now, segmentSeconds), nil
}

// scanFailed is the scan of a camera folder whose listing failed: a missing folder is an empty
// scan, anything else is the error.
func scanFailed(dir string, now time.Time, err error) (DirScan, error) {
	scan := DirScan{Dir: dir, ScannedAt: now.UTC()}
	if errors.Is(err, fs.ErrNotExist) {
		if fi, serr := os.Stat(dir); serr == nil && !fi.IsDir() { // Windows reports a file as "path not found"
			return scan, fmt.Errorf("%s is not a directory", dir)
		}
		return scan, nil
	}
	return scan, err
}

// isSpoolSegment is the scan's segment rule in the spool: a regular file that is a segment of
// camera (the name the folder has, as Matcher.Scan has always used it).
func (l Layout) isSpoolSegment(f File, camera string) bool {
	return f.Info.Mode().IsRegular() && l.Matcher.IsSegmentOf(f.Name(), camera, l.Container)
}

func (l Layout) spoolSegments(files []File, camera string) (n int) {
	for _, f := range files {
		if l.isSpoolSegment(f, camera) {
			n++
		}
	}
	return n
}

func (l Layout) daySegments(files []File, camera string) (n int) {
	for _, f := range files {
		if _, ok := l.segment(f, camera); ok {
			n++
		}
	}
	return n
}

// scanOf builds the DirScan of camera from the spool's files and day folders' files (newest day
// first; each folder in name order). all counts the day folders in the totals. The spool-first
// rule uses the day folders only up to the one with which two segments have been seen.
func (l Layout) scanOf(dir, camera string, spool []File, days [][]File, all bool, now time.Time, segmentSeconds int) DirScan {
	now = now.UTC()
	scan := DirScan{Dir: dir, ScannedAt: now}
	count := func(f File, filed bool) {
		mtime := f.Info.ModTime().UTC()
		scan.Files++
		scan.Bytes += f.Info.Size()
		if filed {
			scan.Filed++
			scan.FiledBytes += f.Info.Size()
		}
		if scan.OldestWrite.IsZero() || mtime.Before(scan.OldestWrite) {
			scan.OldestWrite = mtime
		}
		if mtime.After(scan.NewestWrite) {
			scan.NewestWrite = mtime
		}
	}
	for _, f := range spool {
		count(f, false)
	}
	if all {
		for _, d := range days {
			for _, f := range d {
				count(f, true)
			}
		}
	}

	// the newest: files are in name order, so !Before lets the later name win an mtime tie
	var newest *File
	var newestM time.Time
	pick := func(f *File) {
		if mtime := f.Info.ModTime().UTC(); newest == nil || !mtime.Before(newestM) {
			newest, newestM = f, mtime
		}
	}
	seen := 0
	for i := range spool {
		if l.isSpoolSegment(spool[i], camera) {
			seen++
			pick(&spool[i])
		}
	}
	used := 0
	for used < len(days) && seen < 2 {
		seen += l.daySegments(days[used], camera)
		used++
	}
	newestFiled := false
	for _, d := range days[:used] {
		if newest != nil {
			break
		}
		for i := range d {
			if _, ok := l.segment(d[i], camera); ok {
				pick(&d[i])
				newestFiled = true
			}
		}
	}

	if newest != nil {
		scan.Newest = newest.Name()
		scan.NewestMTime = newestM
		scan.NewestBytes = newest.Info.Size()
		if n, err := newestSize(newest.Path); err == nil {
			if !newestFiled || all { // its listed size is in the totals
				scan.Bytes += n - scan.NewestBytes
			}
			if newestFiled && all {
				scan.FiledBytes += n - scan.NewestBytes
			}
			scan.NewestBytes = n
		}
		age := now.Sub(newestM)
		scan.Recent = age <= time.Duration(segmentSeconds)*time.Second+openGrace
		scan.Anomaly = -age > futureTolerance
	}
	closed := func(f File) {
		if mtime := f.Info.ModTime().UTC(); (newest == nil || f.Path != newest.Path) && mtime.After(scan.NewestClosedWrite) {
			scan.NewestClosedWrite = mtime
		}
	}
	for _, f := range spool {
		closed(f)
	}
	for _, d := range days[:used] {
		for _, f := range d {
			if _, ok := l.segment(f, camera); ok {
				closed(f)
			}
		}
	}
	return scan
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
