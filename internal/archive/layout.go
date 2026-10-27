package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Layout is the on-disk shape of one archive root: where a camera's folder is, what a camera's
// segments are and where each one lives. It is the one place that builds a path under the root;
// every reader lists and resolves through it, so a change of layout (day folders, decision D20) is
// internal to this package. Today the layout is flat: <Root>/<camera>/<camera>_<stamp><ext>, and
// the camera folder is also the "spool" ffmpeg writes into. A Layout is a plain value: build one
// wherever it is needed.
type Layout struct {
	Root      string
	Matcher   Matcher // the segment-name rule (case folding); Segments, Newest, Scan, Classify and Resolve use it
	Container string  // the configured extension, ".mkv"
	Cache     *Cache  // nil: every call lists and stats afresh; see Cache for what it may reuse
}

// Entry is one directory entry the layout listed, with the full path it built. The DirEntry is the
// listing's own (Name, IsDir, Type, Info), so a caller keeps exactly the stat semantics it had.
type Entry struct {
	fs.DirEntry
	Path string // full path of the entry
	Dir  string // the folder that holds it: the camera folder (spool) today
	Day  string // the day folder's name ("2026-10-27"); "" for the spool. Always "" until day folders exist
}

// File is a non-directory Entry with the listing's file information (DirEntry.Info: lstat on
// Linux and macOS, the directory record itself on Windows).
type File struct {
	Entry
	Info fs.FileInfo
}

// Created is the creation time where the OS reports one (Windows); zero elsewhere.
func (f File) Created() time.Time { return CreatedTime(f.Info) }

// Segment is a File that is a strict-pattern segment of its camera, with the start in its name.
type Segment struct {
	File
	Start time.Time // carries the offset written in the name
}

// CameraFolder is a top-level entry of the root that is not the recorder's own ("_" prefix) and
// is a folder (Link false) or a symbolic link, junction or other reparse point (Link true), as
// os.Lstat reports it.
type CameraFolder struct {
	Name string
	Path string
	Link bool
}

// CameraDir is the folder of the camera whose safe name (or enumerated folder name) is camera:
// <Root>/<camera>. ffmpeg writes there, and the supervisor creates it before a child starts.
func (l Layout) CameraDir(camera string) string { return filepath.Join(l.Root, camera) }

// Folders lists the root's entries that do not start with "_", in name order, as the listing
// reports them (folders, files and links alike). The error is os.ReadDir's.
func (l Layout) Folders() ([]Entry, error) {
	ents, err := os.ReadDir(l.Root)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(ents))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "_") {
			continue
		}
		out = append(out, Entry{DirEntry: e, Path: filepath.Join(l.Root, e.Name()), Dir: l.Root})
	}
	return out, nil
}

// Cameras is Folders checked with os.Lstat (never followed): folders, and links reported as such;
// files and entries that vanished are left out. This is Get-CamSegmentDirectories: the pruner and
// the archive scan read the folders and refuse the links.
func (l Layout) Cameras() ([]CameraFolder, error) {
	ents, err := l.Folders()
	if err != nil {
		return nil, err
	}
	var out []CameraFolder
	for _, e := range ents {
		fi, err := os.Lstat(e.Path)
		switch {
		case err != nil:
			continue // vanished
		case IsReparse(fi):
			out = append(out, CameraFolder{Name: e.Name(), Path: e.Path, Link: true})
		case fi.IsDir():
			out = append(out, CameraFolder{Name: e.Name(), Path: e.Path})
		}
	}
	return out, nil
}

// Entries lists every entry of the camera's folder, in name order, sub-folders and links
// included, always from the disk (never cached). The error is os.ReadDir's.
func (l Layout) Entries(camera string) ([]Entry, error) {
	dir := l.CameraDir(camera)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(ents))
	for i, e := range ents {
		out[i] = entryOf(dir, e)
	}
	return out, nil
}

// Files lists the camera's non-directory entries in name order with their file information; an
// entry whose information cannot be read (it vanished between the listing and the stat) is left
// out. Through the Cache when one is set (Entries never is). The error is os.ReadDir's.
func (l Layout) Files(camera string) ([]File, error) {
	dir := l.CameraDir(camera)
	if l.Cache != nil {
		return l.Cache.files(dir)
	}
	return listFiles(dir)
}

// Segments are the camera's strict-pattern segments (regular files, not links, whose name is a
// segment of this camera in the container), newest first: by modification time, the later name
// winning a tie (the archive scan's "which file is open" rule).
func (l Layout) Segments(camera string) ([]Segment, error) {
	files, err := l.Files(camera)
	if err != nil {
		return nil, err
	}
	var out []Segment
	for _, f := range files {
		if !l.isSegment(f, camera) {
			continue
		}
		_, start, _ := ParseSegment(f.Name())
		out = append(out, Segment{File: f, Start: start})
	}
	sort.SliceStable(out, func(i, j int) bool { return newer(out[i].File, out[j].File) })
	return out, nil
}

// Newest is the first of Segments: the segment ffmpeg is writing, or the last one it wrote.
func (l Layout) Newest(camera string) (Segment, bool, error) {
	segs, err := l.Segments(camera)
	if err != nil || len(segs) == 0 {
		return Segment{}, false, err
	}
	return segs[0], true, nil
}

// Scan is Matcher.Scan of the camera's folder, through the Cache when one is set.
func (l Layout) Scan(camera string, now time.Time, segmentSeconds int) (DirScan, error) {
	dir := l.CameraDir(camera)
	files, err := l.Files(camera)
	return scanOf(dir, camera, l.Matcher, l.Container, files, err, now, segmentSeconds)
}

// ErrNotListed is Resolve's answer for a name the camera's folder does not hold.
var ErrNotListed = fmt.Errorf("not in the camera folder: %w", fs.ErrNotExist)

// Resolve finds the entry called name in the camera's folder by MATCHING name against the names
// the folder lists, never by joining it onto a path: a request string can only ever select
// something enumerated from disk, so a separator, a drive letter or ".." cannot step anywhere.
// The exact name wins; where the matcher folds case (Windows, whose file system resolves a name in
// any case) a name equal but for case matches too. The entry may be of any kind: the caller checks.
func (l Layout) Resolve(camera, name string) (Entry, error) {
	if name == "" {
		return Entry{}, ErrNotListed
	}
	ents, err := l.Entries(camera)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range ents {
		if e.Name() == name {
			return e, nil
		}
	}
	if l.Matcher.FoldCase {
		for _, e := range ents {
			if strings.EqualFold(e.Name(), name) {
				return e, nil
			}
		}
	}
	return Entry{}, ErrNotListed
}

// Classify says whose segment path is: ok when, lexically after filepath.Clean, path is
// <Root>/<camera>/<name> (the root compared with the matcher's case rule), camera does not start
// with "_", and name is a strict segment of camera in the container. Nothing is read from disk.
func (l Layout) Classify(path string) (camera, name string, ok bool) {
	p := filepath.Clean(path)
	dir := filepath.Dir(p)
	name, camera = filepath.Base(p), filepath.Base(dir)
	parent, root := filepath.Dir(dir), filepath.Clean(l.Root)
	same := parent == root
	if l.Matcher.FoldCase {
		same = strings.EqualFold(parent, root)
	}
	if !same || l.Root == "" || camera == "" || strings.HasPrefix(camera, "_") || dir == parent || !l.Matcher.IsSegmentOf(name, camera, l.Container) {
		return "", "", false
	}
	return camera, name, true
}

func (l Layout) isSegment(f File, camera string) bool {
	return f.Info.Mode().IsRegular() && l.Matcher.IsSegmentOf(f.Name(), camera, l.Container)
}

// newer: a was modified after b, or at the same time with a later name.
func newer(a, b File) bool {
	ma, mb := a.Info.ModTime(), b.Info.ModTime()
	if !ma.Equal(mb) {
		return ma.After(mb)
	}
	return a.Name() > b.Name()
}

func entryOf(dir string, e fs.DirEntry) Entry {
	return Entry{DirEntry: e, Path: filepath.Join(dir, e.Name()), Dir: dir}
}

// listFiles is the uncached Files of one folder.
func listFiles(dir string) ([]File, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil { // vanished between listing and stat
			continue
		}
		out = append(out, File{Entry: entryOf(dir, e), Info: info})
	}
	return out, nil
}

// scanOf builds a DirScan from one folder's Files (name order) or its listing error.
func scanOf(dir, safe string, m Matcher, container string, files []File, err error, now time.Time, segmentSeconds int) (DirScan, error) {
	now = now.UTC()
	scan := DirScan{Dir: dir, ScannedAt: now}
	if errors.Is(err, fs.ErrNotExist) {
		if fi, serr := os.Stat(dir); serr == nil && !fi.IsDir() { // Windows reports a file as "path not found"
			return scan, fmt.Errorf("%s is not a directory", dir)
		}
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	var newest *File
	var newestM time.Time
	for i := range files {
		f := &files[i]
		mtime := f.Info.ModTime().UTC()
		scan.Files++
		scan.Bytes += f.Info.Size()
		if scan.OldestWrite.IsZero() || mtime.Before(scan.OldestWrite) {
			scan.OldestWrite = mtime
		}
		if mtime.After(scan.NewestWrite) {
			scan.NewestWrite = mtime
		}
		if !f.Info.Mode().IsRegular() || !m.IsSegmentOf(f.Name(), safe, container) {
			continue
		}
		// files are in name order, so !Before lets the later name win an mtime tie
		if newest == nil || !mtime.Before(newestM) {
			newest, newestM = f, mtime
		}
	}
	if newest != nil {
		scan.Newest = newest.Name()
		scan.NewestMTime = newestM
		scan.NewestBytes = newest.Info.Size()
		if n, err := newestSize(newest.Path); err == nil {
			scan.Bytes += n - scan.NewestBytes
			scan.NewestBytes = n
		}
		age := now.Sub(newestM)
		scan.Recent = age <= time.Duration(segmentSeconds)*time.Second+openGrace
		scan.Anomaly = -age > futureTolerance
	}
	for _, f := range files {
		if mtime := f.Info.ModTime().UTC(); f.Name() != scan.Newest && mtime.After(scan.NewestClosedWrite) {
			scan.NewestClosedWrite = mtime
		}
	}
	return scan, nil
}
