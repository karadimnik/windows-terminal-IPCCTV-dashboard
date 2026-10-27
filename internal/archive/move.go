package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"holocron/internal/osx"
)

// The mover (decision D20, task L4a): FileDays files CLOSED segments from a camera's spool into the
// day folder of the date in their names, UnfileDays puts filed segments back into the spool (the
// flat layout of before D20). Both move by renaming on the one volume (metadata only) and never
// replace an existing name, so a run cannot lose, overwrite or duplicate a recording, and each one
// is crash-safe by nature: a rename either happened or did not, the next run sees the result and
// carries on, and there is nothing to journal beyond the log lines the caller writes. A failure
// is benign: the file stays where it was, where every reader lists it and the pruner prunes it.
//
// What FileDays moves, per camera folder: a regular file (never a link, junction, other reparse
// point or folder) whose name is a segment of that camera (Classify Spooled), that is not one of
// the two newest segments of the spool (the open one and the newest closed one, newest as
// Layout.Newest decides it: by modification time, the later name winning a tie; the contract every
// reader relies on), and whose last write is at least Margin old. A zero-byte segment is filed like
// any other. Oldest first, at most Max per camera per run.
//
// Every path is checked again immediately before its rename (os.Lstat, never followed): the camera
// folder is still a plain folder directly under the root, the file is still a plain file with the
// listed size and write time and the same classification, the day folder is a plain folder, and
// both ends lie inside the sandbox when one is in force. Residual: a file swapped for a link in the
// instant between that check and the rename is renamed as a link (a rename never follows its
// source), so it is moved, not followed; readers ignore a link in a day folder.

// MoveOptions configure FileDays and UnfileDays.
type MoveOptions struct {
	Platform osx.Platform // decides the ownership-marker check (required)
	Now      time.Time    // the age of a segment is Now minus its last write
	// Margin: a spool segment written less than Margin before Now is not filed (the supervisor's
	// default is 120 s, never less than 30 s). UnfileDays ignores it.
	Margin time.Duration
	Max    int  // at most this many files moved per camera per run; 0 means no limit
	DryRun bool // plan and report, create and move nothing
	// Sandbox is the only tree a run may move in; "" means SandboxFromEnv (SANDBOX_ONLY).
	Sandbox string
	// RecorderRunning: UnfileDays refuses while true (a recorder writing into the spool would race
	// the files coming back). The caller knows whether the recorder runs; FileDays ignores it.
	RecorderRunning bool
}

// Move is one rename a run made (or, in a dry run, would make).
type Move struct {
	Camera, From, To string
	Bytes            int64
}

// MoveSkip is a file a run did not move, and why.
type MoveSkip struct {
	Camera, Path, Why string
}

// MoveReport is a finished run.
type MoveReport struct {
	Refused string // why the run moved nothing at all (the marker, the sandbox, a running recorder); "" otherwise
	DryRun  bool
	Moved   []Move
	Bytes   int64          // the bytes of Moved
	Cameras map[string]int // moves per camera folder
	// Skipped: eligible files the check before the rename turned down (the file changed, became a
	// link, its day folder is not a plain folder); the next run looks again.
	Skipped []MoveSkip
	// Conflicts: the destination name already exists. Neither file is touched; it is reported until
	// someone resolves it.
	Conflicts []MoveSkip
	// Busy: the OS refused the rename because another process holds the file (a Windows sharing
	// violation or access denied). Benign: the next run tries again.
	Busy []MoveSkip
	// Errors: any other failure (listing a folder, creating a day folder, the rename itself).
	Errors []MoveSkip
	// Left (UnfileDays): files in day folders that are not filed segments of that camera
	// (misfiled, foreign, links, folders); they are never moved, and keep their day folder.
	Left []MoveSkip
	// Deferred: eligible files over Max, left for the next run.
	Deferred int
	// DaysRemoved (UnfileDays): day folders removed because they became empty.
	DaysRemoved []string
}

func (r *MoveReport) moved(cam, from, to string, n int64) {
	r.Moved = append(r.Moved, Move{Camera: cam, From: from, To: to, Bytes: n})
	r.Bytes += n
	if r.Cameras == nil {
		r.Cameras = map[string]int{}
	}
	r.Cameras[cam]++
}

// FileDays files the closed spool segments of each camera folder named in cameras (all camera
// folders of the root when none is named) into their day folders; see the package comment of
// the mover above for what is moved and how. A day folder is created with one non-recursive
// Mkdir directly in the camera folder. The run is refused, moving nothing, when the root has no
// valid ownership marker or lies outside the sandbox.
func FileDays(l Layout, o MoveOptions, cameras ...string) MoveReport {
	rep, prefix, cams := begin(l, o, cameras)
	if rep.Refused != "" {
		return rep
	}
	for _, cam := range cams {
		l.fileCamera(cam, o, prefix, &rep)
	}
	return rep
}

// UnfileDays moves every filed segment (Classify Filed) of each camera folder named in cameras (all
// when none is named) from its day folder back into the camera folder, never replacing a name,
// then removes each day folder that became empty (rmdir, never recursive). Misfiled segments,
// foreign files, links and folders in a day folder are left untouched and reported (Left), and
// their day folder stays. Refused while o.RecorderRunning, and as FileDays refuses.
func UnfileDays(l Layout, o MoveOptions, cameras ...string) MoveReport {
	if o.RecorderRunning {
		return MoveReport{Refused: "the recorder is running: stop it before putting the day folders back into the spool", DryRun: o.DryRun}
	}
	rep, prefix, cams := begin(l, o, cameras)
	if rep.Refused != "" {
		return rep
	}
	for _, cam := range cams {
		l.unfileCamera(cam, o, prefix, &rep)
	}
	return rep
}

// begin applies the gates of a whole run and returns the sandbox prefix and the camera folders.
func begin(l Layout, o MoveOptions, cameras []string) (MoveReport, string, []string) {
	rep := MoveReport{DryRun: o.DryRun}
	prefix := o.Sandbox
	if prefix == "" {
		prefix = SandboxFromEnv()
	}
	if err := CheckSandbox(l.Root, prefix); err != nil {
		rep.Refused = err.Error()
		return rep, "", nil
	}
	if ok, why := MarkerValid(l.Root, o.Platform); !ok {
		rep.Refused = "the root is not owned by this recorder: " + why
		return rep, "", nil
	}
	if len(cameras) > 0 {
		return rep, prefix, cameras
	}
	folders, err := l.Cameras()
	if err != nil {
		rep.Refused = fmt.Sprintf("the root cannot be listed: %v", err)
		return rep, "", nil
	}
	for _, f := range folders {
		if !f.Link {
			cameras = append(cameras, f.Name)
		}
	}
	return rep, prefix, cameras
}

// spooled is a spool segment as the mover listed it.
type spooled struct {
	name string
	info fs.FileInfo
	day  string
}

func (l Layout) fileCamera(cam string, o MoveOptions, prefix string, rep *MoveReport) {
	dir := l.CameraDir(cam)
	if why := l.plainCameraDir(cam); why != "" {
		rep.Errors = append(rep.Errors, MoveSkip{Camera: cam, Path: dir, Why: why})
		return
	}
	ents, err := l.Entries(cam)
	if err != nil {
		rep.Errors = append(rep.Errors, MoveSkip{Camera: cam, Path: dir, Why: err.Error()})
		return
	}
	var segs []spooled
	for _, e := range ents {
		if e.IsDir() || l.classOf(cam, "", e.Name()).Kind != Spooled {
			continue
		}
		fi, err := os.Lstat(e.Path)
		if err != nil || !fi.Mode().IsRegular() || IsReparse(fi) {
			continue // vanished, or a link or other non-file named like a segment: never moved, never counted
		}
		_, start, _ := ParseSegment(e.Name())
		segs = append(segs, spooled{name: e.Name(), info: fi, day: DayName(start)})
	}
	sort.SliceStable(segs, func(i, j int) bool { return newerInfo(segs[i], segs[j]) })
	if len(segs) <= 2 {
		return
	}
	var due []spooled
	for _, s := range segs[2:] {
		if o.Now.Sub(s.info.ModTime()) >= o.Margin {
			due = append(due, s)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return newerInfo(due[j], due[i]) }) // oldest first
	if o.Max > 0 && len(due) > o.Max {
		rep.Deferred += len(due) - o.Max
		due = due[:o.Max]
	}
	made := map[string]bool{}
	for _, s := range due {
		src := filepath.Join(dir, s.name)
		dayDir := filepath.Join(dir, s.day)
		dst := filepath.Join(dayDir, s.name)
		if testHookBeforeMove != nil {
			testHookBeforeMove(src)
		}
		if why := l.checkSource(cam, "", src, s.info, prefix); why != "" {
			rep.Skipped = append(rep.Skipped, MoveSkip{Camera: cam, Path: src, Why: why})
			continue
		}
		if err := CheckSandbox(dst, prefix); err != nil {
			rep.Skipped = append(rep.Skipped, MoveSkip{Camera: cam, Path: src, Why: err.Error()})
			continue
		}
		if o.DryRun {
			rep.moved(cam, src, dst, s.info.Size())
			continue
		}
		if !made[s.day] {
			if why, err := l.dayFolder(cam, s.day); why != "" || err != nil {
				if err != nil {
					rep.Errors = append(rep.Errors, MoveSkip{Camera: cam, Path: dayDir, Why: err.Error()})
				} else {
					rep.Skipped = append(rep.Skipped, MoveSkip{Camera: cam, Path: src, Why: why})
				}
				continue
			}
			made[s.day] = true
		} else if why := plainDir(dayDir); why != "" {
			rep.Skipped = append(rep.Skipped, MoveSkip{Camera: cam, Path: src, Why: "its day folder " + why})
			continue
		}
		l.rename(cam, src, dst, s.info, rep)
	}
}

func (l Layout) unfileCamera(cam string, o MoveOptions, prefix string, rep *MoveReport) {
	dir := l.CameraDir(cam)
	if why := l.plainCameraDir(cam); why != "" {
		rep.Errors = append(rep.Errors, MoveSkip{Camera: cam, Path: dir, Why: why})
		return
	}
	ents, err := l.Entries(cam)
	if err != nil {
		rep.Errors = append(rep.Errors, MoveSkip{Camera: cam, Path: dir, Why: err.Error()})
		return
	}
	n := 0
	for _, d := range ents {
		if !d.IsDay() {
			continue
		}
		if why := plainDir(d.Path); why != "" {
			rep.Left = append(rep.Left, MoveSkip{Camera: cam, Path: d.Path, Why: "the day folder " + why})
			continue
		}
		dents, err := readDir(d.Path)
		if err != nil {
			rep.Errors = append(rep.Errors, MoveSkip{Camera: cam, Path: d.Path, Why: err.Error()})
			continue
		}
		kept, movedHere := 0, 0
		for _, e := range dents {
			src := entryOf(d.Path, d.Name(), e).Path
			fi, err := os.Lstat(src)
			switch {
			case err != nil:
				kept++ // vanished or unreadable: the folder is not known to be empty
				continue
			case IsReparse(fi) || !fi.Mode().IsRegular():
				kept++
				rep.Left = append(rep.Left, MoveSkip{Camera: cam, Path: src, Why: "not a plain file"})
				continue
			}
			if c := l.classOf(cam, d.Name(), e.Name()); c.Kind != Filed {
				kept++
				why := "not a segment of this camera"
				if c.Kind == Misfiled {
					why = "a segment filed under another date (misfiled)"
				}
				rep.Left = append(rep.Left, MoveSkip{Camera: cam, Path: src, Why: why})
				continue
			}
			if o.Max > 0 && n >= o.Max {
				kept++
				rep.Deferred++
				continue
			}
			kept++ // until the rename below succeeds
			if why := l.checkSource(cam, d.Name(), src, fi, prefix); why != "" {
				rep.Skipped = append(rep.Skipped, MoveSkip{Camera: cam, Path: src, Why: why})
				continue
			}
			dst := filepath.Join(dir, e.Name())
			n++
			if o.DryRun {
				rep.moved(cam, src, dst, fi.Size())
				continue
			}
			if l.rename(cam, src, dst, fi, rep) {
				kept--
				movedHere++
			}
		}
		if kept > 0 || movedHere == 0 || o.DryRun {
			continue // something stays in it, or it was empty before this run (left as it was)
		}
		if why := plainDir(d.Path); why != "" || CheckSandbox(d.Path, prefix) != nil {
			continue
		}
		if err := syscall.Rmdir(d.Path); err == nil {
			rep.DaysRemoved = append(rep.DaysRemoved, d.Path)
		} // else it is not empty after all (something arrived) or busy: it stays, and is a day folder still
	}
}

// rename moves src to dst without ever replacing dst and files the outcome in rep; true when moved.
func (l Layout) rename(cam, src, dst string, fi fs.FileInfo, rep *MoveReport) bool {
	err := renameFile(src, dst)
	switch {
	case err == nil:
		rep.moved(cam, src, dst, fi.Size())
		return true
	case errors.Is(err, fs.ErrExist):
		// The same file under both names is an interrupted link-then-unlink move (not Windows):
		// finish it. Anything else is a real conflict, and both files stay as they are.
		if si, e1 := os.Lstat(src); e1 == nil {
			if di, e2 := os.Lstat(dst); e2 == nil && os.SameFile(si, di) {
				if err := os.Remove(src); err == nil {
					rep.moved(cam, src, dst, fi.Size())
					return true
				}
			}
		}
		rep.Conflicts = append(rep.Conflicts, MoveSkip{Camera: cam, Path: src, Why: "the destination " + dst + " already exists; neither file was touched"})
	case isBusy(err):
		rep.Busy = append(rep.Busy, MoveSkip{Camera: cam, Path: src, Why: err.Error()})
	default:
		rep.Errors = append(rep.Errors, MoveSkip{Camera: cam, Path: src, Why: err.Error()})
	}
	return false
}

// checkSource re-validates a file just before its rename; "" means it may move.
func (l Layout) checkSource(cam, day, src string, listed fs.FileInfo, prefix string) string {
	if err := CheckSandbox(src, prefix); err != nil {
		return err.Error()
	}
	if why := l.plainCameraDir(cam); why != "" {
		return why
	}
	want := Spooled
	if day != "" {
		want = Filed
		if why := plainDir(filepath.Join(l.CameraDir(cam), day)); why != "" {
			return "its day folder " + why
		}
	}
	if c := l.Classify(src); c.Kind != want || c.Camera != cam || c.Day != day {
		return "it fails the allowlist"
	}
	fi, err := os.Lstat(src)
	switch {
	case err != nil:
		return "it is gone"
	case IsReparse(fi) || !fi.Mode().IsRegular():
		return "it became a link or is not a plain file"
	case fi.Size() != listed.Size():
		return fmt.Sprintf("its size changed from %d to %d bytes (a recorder may be writing)", listed.Size(), fi.Size())
	case !fi.ModTime().Equal(listed.ModTime()):
		return "it was modified after it was listed (a recorder may be writing)"
	}
	return ""
}

// plainCameraDir: the camera folder is still a plain folder directly under the root; "" when so.
func (l Layout) plainCameraDir(cam string) string {
	if cam == "" || cam != filepath.Base(cam) || cam == "." || cam == ".." || cam[0] == '_' {
		return "not a camera folder name"
	}
	if why := plainDir(l.Root); why != "" {
		return "the root " + why
	}
	if why := plainDir(l.CameraDir(cam)); why != "" {
		return "the camera folder " + why
	}
	return ""
}

// plainDir: path is a folder, not a link, junction or other reparse point; "" when so.
func plainDir(path string) string {
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		return "is gone"
	case IsReparse(fi):
		return "is a link or junction"
	case !fi.IsDir():
		return "is not a folder"
	}
	return ""
}

// dayFolder makes sure the day folder day of camera exists as a plain folder: created with one
// non-recursive Mkdir in the camera folder when missing. why says why it cannot be used.
func (l Layout) dayFolder(cam, day string) (why string, err error) {
	if !IsDayName(day) {
		return "not a day name: " + day, nil
	}
	dir := filepath.Join(l.CameraDir(cam), day)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	if why := plainDir(dir); why != "" {
		return "its day folder " + why, nil
	}
	return "", nil
}

// newerInfo is newer for the mover's listing: a was modified after b, or at the same time with a
// later name.
func newerInfo(a, b spooled) bool {
	ma, mb := a.info.ModTime(), b.info.ModTime()
	if !ma.Equal(mb) {
		return ma.After(mb)
	}
	return a.name > b.name
}

// renameFile is renameNoReplace; a test swaps in a replacing rename to prove the tests catch one.
var renameFile = renameNoReplace

// testHookBeforeMove, when set (tests only), runs after a file is listed and before it is checked
// again and moved.
var testHookBeforeMove func(src string)
