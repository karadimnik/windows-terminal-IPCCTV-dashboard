package archive

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Layout is the on-disk shape of one archive root: where a camera's folder is, what a camera's
// segments are and where each one lives. It is the one place that builds a path under the root;
// every reader lists and resolves through it, so the layout is internal to this package. The
// layout is spool and day folders (decision D20):
//
//	<Root>/<camera>/<camera>_<stamp><ext>               the spool: ffmpeg writes here
//	<Root>/<camera>/<YYYY-MM-DD>/<camera>_<stamp><ext>  a filed day: the local date written in the names
//
// A tree with no day folders is the flat layout of before D20, and every reader gives exactly
// what it gave then. Nothing in this package moves a file into a day folder (the mover is task
// L4). A Layout is a plain value: build one wherever it is needed.
//
// What reads what. The spool alone (one listing of the camera folder) answers whether a camera
// is recording: Newest and Scan, behind the status tick, the age of the last closed segment, the
// camera-down alert (110) and the stall watchdog. That holds while the spool keeps the open
// segment and the newest closed one, which is the contract of the L4 mover: it must always leave
// the open segment AND the newest closed segment in the spool. When the spool holds fewer than
// two segments, Newest and Scan also read day folders, newest first, until they have seen two
// segments (so with one segment in the spool a status tick is two listings, and the answer is
// still exact). ScanSpool reads the spool and nothing else (the flat view, the reference the tests compare with).
// Everything that shows or counts footage reads the spool and every day folder: Files, Segments
// and ScanAll (the archive-wide scan, the footage index, archive stats, archive compare, the
// doctor). Resolve reads the spool and at most the one day folder a segment name implies.
type Layout struct {
	Root      string
	Matcher   Matcher // the segment-name rule (case folding); Segments, Newest, the scans, Classify and Resolve use it
	Container string  // the configured extension, ".mkv"
	Cache     *Cache  // nil: every call lists and stats afresh; see Cache for what it may reuse
}

// Entry is one directory entry the layout listed, with the full path it built. The DirEntry is the
// listing's own (Name, IsDir, Type, Info), so a caller keeps exactly the stat semantics it had.
type Entry struct {
	fs.DirEntry
	Path string // full path of the entry
	Dir  string // the folder that holds it: the camera folder (the spool) or one of its day folders
	Day  string // the name of the day folder that holds it ("2026-10-27"); "" in the spool
}

// IsDay reports whether the entry is a day folder: a sub-folder whose name IsDayName and whose
// listed type is a plain directory. A symbolic link, junction or other reparse point named like a
// day is never a day folder and is never entered, as Cameras never follows one at the top
// (on Windows, Go reports a junction or symbolic link as no directory and any other reparse
// point as irregular, so the listing's type bits decide without a stat).
func (e Entry) IsDay() bool { return e.Type() == fs.ModeDir && IsDayName(e.Name()) }

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

// IsDayName reports whether name is a day folder's name: YYYY-MM-DD in ASCII digits, a real
// calendar date ("2026-02-30" and "2026-13-45" are not).
func IsDayName(name string) bool {
	if len(name) != 10 || name[4] != '-' || name[7] != '-' {
		return false
	}
	y, ok1 := digits(name[0:4])
	m, ok2 := digits(name[5:7])
	d, ok3 := digits(name[8:10])
	if !ok1 || !ok2 || !ok3 {
		return false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return t.Year() == y && int(t.Month()) == m && t.Day() == d
}

// DayName is the day folder of a segment that starts at start, as ParseSegment returns it (in the
// offset written in the name): the local date in the name. So the repeated hour of a clock change
// stays in one day folder, with both offsets.
func DayName(start time.Time) string { return start.Format("2006-01-02") }

// listings counts every folder this package lists: the seam the cost tests count with.
var listings atomic.Int64

func readDir(dir string) ([]os.DirEntry, error) {
	listings.Add(1)
	return os.ReadDir(dir)
}

// CameraDir is the folder of the camera whose safe name (or enumerated folder name) is camera:
// <Root>/<camera>. ffmpeg writes there, and the supervisor creates it before a child starts.
func (l Layout) CameraDir(camera string) string { return filepath.Join(l.Root, camera) }

// Folders lists the root's entries that do not start with "_", in name order, as the listing
// reports them (folders, files and links alike). The error is os.ReadDir's.
func (l Layout) Folders() ([]Entry, error) {
	ents, err := readDir(l.Root)
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

// Entries lists every entry of the camera's folder (the spool level), in name order, sub-folders
// (day folders among them) and links included, always from the disk (never cached). The error is
// os.ReadDir's.
func (l Layout) Entries(camera string) ([]Entry, error) {
	dir := l.CameraDir(camera)
	ents, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(ents))
	for i, e := range ents {
		out[i] = entryOf(dir, "", e)
	}
	return out, nil
}

// Spool is one listing of the camera's folder, through the Cache when one is set: its
// non-directory entries in name order with their information (the spool, foreign files and links
// included; an entry whose information cannot be read, because it vanished between the listing
// and the stat, is left out) and its day folders (Entry.IsDay), newest first. The error is
// os.ReadDir's.
func (l Layout) Spool(camera string) (files []File, days []Entry, err error) {
	ls, err := l.list(l.CameraDir(camera), "")
	if err != nil {
		return nil, nil, err
	}
	for i := len(ls.days) - 1; i >= 0; i-- {
		days = append(days, ls.days[i])
	}
	return ls.files, days, nil
}

// DayFiles lists the day folder day, an entry Spool or Entries returned (Entry.IsDay; anything
// else is ErrNotListed, so a path never comes from a caller's string): its non-directory entries
// in name order with their information, Day set, through the Cache when one is set. Folders inside
// a day folder are not entered. The error is os.ReadDir's (a day folder that vanished since it was
// listed is fs.ErrNotExist).
func (l Layout) DayFiles(day Entry) ([]File, error) {
	if !day.IsDay() {
		return nil, ErrNotListed
	}
	ls, err := l.list(day.Path, day.Name())
	return ls.files, err
}

// Files lists the camera's non-directory entries: the spool's, then every day folder's, oldest day
// first, each folder in name order. A day folder that vanishes between the listings is left out.
// The error is the camera folder's os.ReadDir error.
func (l Layout) Files(camera string) ([]File, error) {
	files, days, err := l.Spool(camera)
	if err != nil {
		return nil, err
	}
	for i := len(days) - 1; i >= 0; i-- {
		df, err := l.DayFiles(days[i])
		if err != nil {
			continue
		}
		files = append(files, df...)
	}
	return files, nil
}

// Segments are the camera's footage across the spool and every day folder: regular files (not
// links) that Classify as Spooled or Filed, newest first: by modification time, the later name
// winning a tie (the archive scan's "which file is open" rule). A misfiled or foreign file is not
// footage.
func (l Layout) Segments(camera string) ([]Segment, error) {
	files, err := l.Files(camera)
	if err != nil {
		return nil, err
	}
	var out []Segment
	for _, f := range files {
		if s, ok := l.segment(f, camera); ok {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return newer(out[i].File, out[j].File) })
	return out, nil
}

// Newest is the segment ffmpeg is writing, or the last one it wrote, by the spool-first rule: the
// newest segment of the spool when it holds one, else the newest segment of the newest day folder
// that holds one. It reads the spool alone whenever the spool holds a segment.
func (l Layout) Newest(camera string) (Segment, bool, error) {
	files, days, err := l.Spool(camera)
	if err != nil {
		return Segment{}, false, err
	}
	if n := l.newestOf(files, camera); n != nil {
		return *n, true, nil
	}
	for _, d := range days {
		df, err := l.DayFiles(d)
		if err != nil {
			continue
		}
		if n := l.newestOf(df, camera); n != nil {
			return *n, true, nil
		}
	}
	return Segment{}, false, nil
}

// ErrNotListed is Resolve's answer for a name the camera's folders do not hold.
var ErrNotListed = fmt.Errorf("not in the camera folder: %w", fs.ErrNotExist)

// Resolve finds the entry called name by MATCHING name against the names the camera's folders
// list, never by joining it onto a path: a request string can only ever select something
// enumerated from disk, so a separator, a drive letter or ".." cannot step anywhere. The spool is
// searched first; then, when name parses as a segment, the day folder of the date written in it
// (that date matched against the listed day folders). In each folder the exact name wins; where
// the matcher folds case (Windows, whose file system resolves a name in any case) a name equal but
// for case matches too. The entry may be of any kind: the caller checks.
func (l Layout) Resolve(camera, name string) (Entry, error) {
	if name == "" {
		return Entry{}, ErrNotListed
	}
	ents, err := l.Entries(camera)
	if err != nil {
		return Entry{}, err
	}
	if e, ok := l.match(ents, name); ok {
		return e, nil
	}
	_, start, ok := ParseSegment(name)
	if !ok {
		return Entry{}, ErrNotListed
	}
	day := DayName(start)
	for _, d := range ents {
		if d.Name() != day || !d.IsDay() {
			continue
		}
		dents, err := readDir(d.Path)
		if err != nil {
			return Entry{}, ErrNotListed
		}
		in := make([]Entry, len(dents))
		for i, e := range dents {
			in[i] = entryOf(d.Path, day, e)
		}
		if e, ok := l.match(in, name); ok {
			return e, nil
		}
	}
	return Entry{}, ErrNotListed
}

func (l Layout) match(ents []Entry, name string) (Entry, bool) {
	for _, e := range ents {
		if e.Name() == name {
			return e, true
		}
	}
	if l.Matcher.FoldCase {
		for _, e := range ents {
			if strings.EqualFold(e.Name(), name) {
				return e, true
			}
		}
	}
	return Entry{}, false
}

// Kind is what a path in the archive is to the recorder.
type Kind uint8

const (
	Foreign  Kind = iota // not a segment of the archive (outside the root, a "_" folder, too deep, a folder that is not a day, a name that is not a segment of that camera)
	Spooled              // <Root>/<camera>/<segment of camera>: in the spool
	Filed                // <Root>/<camera>/<day>/<segment of camera>, the day being the date written in the name
	Misfiled             // <Root>/<camera>/<day>/<segment of camera> under a day that is not the date in the name
)

// Class is Classify's verdict. Camera, Day ("" in the spool) and Name are set for every Kind but
// Foreign.
type Class struct {
	Camera, Day, Name string
	Kind              Kind
}

// Footage reports a segment that is where it belongs: in the spool or filed under its own date.
func (c Class) Footage() bool { return c.Kind == Spooled || c.Kind == Filed }

// Classify says whose path is, lexically after filepath.Clean: nothing is read from disk, so the
// caller checks the file type. The root is compared with the matcher's case rule; a camera starting
// with "_" is the recorder's own folder and never a camera. The day folder must be IsDayName; a
// segment of the camera under a day that is not the date in its name is Misfiled (the doctor and
// archive stats report it), and every reader leaves it out of the footage.
func (l Layout) Classify(path string) Class {
	if l.Root == "" {
		return Class{}
	}
	p := filepath.Clean(path)
	root := filepath.Clean(l.Root)
	name, dir := filepath.Base(p), filepath.Dir(p)
	if parent := filepath.Dir(dir); parent != dir && l.sameRoot(parent, root) {
		return l.classOf(filepath.Base(dir), "", name)
	}
	cam := filepath.Dir(dir)
	if parent := filepath.Dir(cam); cam != dir && parent != cam && l.sameRoot(parent, root) {
		return l.classOf(filepath.Base(cam), filepath.Base(dir), name)
	}
	return Class{}
}

func (l Layout) sameRoot(a, root string) bool {
	if l.Matcher.FoldCase {
		return strings.EqualFold(a, root)
	}
	return a == root
}

// classOf is Classify of <Root>/<camera>/[<day>/]<name>.
func (l Layout) classOf(camera, day, name string) Class {
	if strings.HasPrefix(camera, "_") || !l.Matcher.IsSegmentOf(name, camera, l.Container) {
		return Class{}
	}
	c := Class{Camera: camera, Day: day, Name: name, Kind: Spooled}
	if day == "" {
		return c
	}
	if !IsDayName(day) {
		return Class{}
	}
	c.Kind = Filed
	if _, start, _ := ParseSegment(name); DayName(start) != day {
		c.Kind = Misfiled
	}
	return c
}

// StrayKind is why a file under a camera folder is a Stray.
type StrayKind uint8

const (
	StrayMisfiled StrayKind = iota + 1 // a segment of the camera in a day folder that is not the date in its name
	StrayLoose                         // anything else in a day folder that is not footage: a foreign file, another camera's segment
	StrayNested                        // a segment-like name in a folder the layout never enters: deeper than a day folder, or a sub-folder that is not a day
)

// Stray is a regular file under a camera folder that no reader lists as footage, so the pruner
// never deletes it.
type Stray struct {
	Path string
	Kind StrayKind
}

// Strays walks the camera's folder, never following a link or junction (filepath.WalkDir), and
// returns its strays in walk order. The camera folder's own files (the spool) are never strays:
// each reader judges those. The doctor reports them (storage.unseen). The error is the camera
// folder's.
func (l Layout) Strays(camera string) ([]Stray, error) {
	dir := l.CameraDir(camera)
	if _, err := os.Lstat(dir); err != nil {
		return nil, err
	}
	var out []Stray
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil // an unreadable folder or a vanished entry: skipped, as every reader does
		}
		rel, _ := filepath.Rel(dir, path)
		parts := strings.Split(rel, string(filepath.Separator))
		inDay := len(parts) == 2 && IsDayName(parts[0])
		c := l.Classify(path)
		switch {
		case len(parts) == 1 || c.Footage():
		case inDay && c.Kind == Misfiled:
			out = append(out, Stray{path, StrayMisfiled})
		case inDay:
			out = append(out, Stray{path, StrayLoose})
		default:
			if _, _, ok := ParseSegment(d.Name()); ok {
				out = append(out, Stray{path, StrayNested})
			}
		}
		return nil
	})
	return out, err
}

// segment is f as footage of camera: a regular file that is Spooled or Filed where it was listed.
func (l Layout) segment(f File, camera string) (Segment, bool) {
	if !f.Info.Mode().IsRegular() || !l.classOf(camera, f.Day, f.Name()).Footage() {
		return Segment{}, false
	}
	_, start, _ := ParseSegment(f.Name())
	return Segment{File: f, Start: start}, true
}

// newestOf is the newest footage segment among files of one folder in name order (the later name
// wins an mtime tie), nil when there is none.
func (l Layout) newestOf(files []File, camera string) *Segment {
	var n *Segment
	for _, f := range files {
		s, ok := l.segment(f, camera)
		if ok && (n == nil || !s.Info.ModTime().Before(n.Info.ModTime())) {
			n = &s
		}
	}
	return n
}

// newer: a was modified after b, or at the same time with a later name.
func newer(a, b File) bool {
	ma, mb := a.Info.ModTime(), b.Info.ModTime()
	if !ma.Equal(mb) {
		return ma.After(mb)
	}
	return a.Name() > b.Name()
}

func entryOf(dir, day string, e fs.DirEntry) Entry {
	return Entry{DirEntry: e, Path: filepath.Join(dir, e.Name()), Dir: dir, Day: day}
}

// listing is one folder's listing: its non-directory entries with their information, and its day
// folders, both in name order.
type listing struct {
	files []File
	days  []Entry
}

func (l Layout) list(dir, day string) (listing, error) {
	if l.Cache != nil {
		return l.Cache.list(dir, day)
	}
	return listFolder(dir, day)
}

// listFolder is the uncached list of one folder; day is the name of the day folder it is, "" for
// a camera folder.
func listFolder(dir, day string) (listing, error) {
	ents, err := readDir(dir)
	if err != nil {
		return listing{}, err
	}
	ls := listing{files: make([]File, 0, len(ents))}
	for _, e := range ents {
		if e.IsDir() {
			if d := entryOf(dir, "", e); day == "" && d.IsDay() {
				ls.days = append(ls.days, d)
			}
			continue
		}
		info, err := e.Info()
		if err != nil { // vanished between listing and stat
			continue
		}
		ls.files = append(ls.files, File{Entry: entryOf(dir, day, e), Info: info})
	}
	return ls, nil
}
