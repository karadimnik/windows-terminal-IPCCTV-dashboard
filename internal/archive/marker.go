package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"holocron/internal/clock"
	"holocron/internal/osx"
)

// The marker is the file that says "this recorder created this archive and may prune it". Name and
// content are those of Initialize-CamArchiveOwner (cam.common.ps1), so a root claimed by the
// PowerShell recorder is accepted and the reverse.
const (
	MarkerOwner  = "IPCCTV recorder"
	markerNote   = "The recorder prunes footage only under a root holding this file. It wrote it because the root held nothing but its own files. Do not copy it into another folder."
	markerSchema = 1
	maxMarker    = 64 << 10
)

// Marker is the JSON document, with the PowerShell key order.
type Marker struct {
	Owner     string
	Schema    int
	RootPath  string
	CreatedAt string
	Note      string
}

// MarkerPath is <root>/_state/archive-owner.json.
func MarkerPath(root string) string { return filepath.Join(root, "_state", "archive-owner.json") }

// MarkerValid reports whether root holds a marker this recorder wrote for this very root. Unlike
// PowerShell (which only checks Owner, so a marker copied into another folder satisfies it), the
// RootPath inside must also name this root: a copied or moved archive is not owned until it is
// claimed again, which ClaimRoot does when the root qualifies. why says what is wrong.
func MarkerValid(root string, p osx.Platform) (ok bool, why string) {
	path := MarkerPath(root)
	fi, err := os.Lstat(path)
	if err != nil {
		return false, "there is no ownership marker at " + path
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxMarker {
		return false, "the ownership marker at " + path + " is not a regular file of a sensible size"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, "the ownership marker at " + path + " cannot be read: " + err.Error()
	}
	var m Marker
	if err := json.Unmarshal(stripBOM(raw), &m); err != nil {
		return false, "the ownership marker at " + path + " is not valid JSON"
	}
	if m.Owner != MarkerOwner {
		return false, "the ownership marker at " + path + " was not written by this recorder"
	}
	s := syntaxFor(p)
	a, wa := s.parse(m.RootPath)
	b, wb := s.parse(root)
	if wa != "" || wb != "" {
		return false, "the ownership marker at " + path + " names a root that cannot be compared with " + root
	}
	if same, _ := s.within(a, b); !same {
		return false, fmt.Sprintf("the ownership marker at %s belongs to another folder (%s); it was copied or the archive moved", path, m.RootPath)
	}
	return true, ""
}

func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// ClaimProblem says why the recorder may not claim root as its own archive, or "" when it may:
// the root passes CheckRoot, exists as a folder, and holds nothing the recorder did not write -
// every top-level entry is "_"-prefixed (the recorder's own folders) or a real folder whose
// children are all segment files of that folder's camera in the container. An empty root
// qualifies. Read only. Up to five blockers are named.
func ClaimProblem(root string, p osx.Platform, container string) string {
	if problem := CheckRoot(root, p); problem != "" {
		return problem
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		return fmt.Sprintf("RootPath '%s' does not exist", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Sprintf("RootPath '%s' cannot be listed: %v", root, err)
	}
	var blockers []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "_") {
			continue
		}
		info, err := e.Info()
		switch {
		case err != nil:
			continue // vanished
		case IsReparse(info):
			blockers = append(blockers, name+" (a reparse point)")
			continue
		case !info.IsDir():
			blockers = append(blockers, name+" (a file at the top level)")
			continue
		}
		children, err := os.ReadDir(filepath.Join(root, name))
		if err != nil {
			blockers = append(blockers, name+" (cannot be listed)")
			continue
		}
		for _, c := range children {
			ci, err := c.Info()
			if err != nil {
				continue
			}
			if ci.IsDir() {
				blockers = append(blockers, name+"/"+c.Name()+" (a folder inside a camera folder)")
				break
			}
			if IsReparse(ci) || !ci.Mode().IsRegular() || !IsSegmentOf(c.Name(), name, container) {
				blockers = append(blockers, name+"/"+c.Name()+" (not a recorder segment)")
				break
			}
		}
	}
	if len(blockers) == 0 {
		return ""
	}
	shown := blockers
	if len(shown) > 5 {
		shown = shown[:5]
	}
	plural := "ies"
	if len(blockers) == 1 {
		plural = "y"
	}
	return fmt.Sprintf("RootPath '%s' holds %d top-level entr%s the recorder did not write: %s", root, len(blockers), plural, strings.Join(shown, ", "))
}

// ClaimResult is the outcome of ClaimRoot.
type ClaimResult struct {
	Owned   bool   // a valid marker is in place
	Created bool   // this call wrote it
	Problem string // why the root is not owned
	Path    string // the marker path
}

// ClaimRoot is Initialize-CamArchiveOwner: a no-op when a valid marker exists, otherwise it writes
// the marker - only when ClaimProblem is empty. The write is atomic (temp file in _state, then
// rename) and the file is owner-only. A refusal is a Problem, not an error; the error is for a
// failed write after the checks passed. now is stored as a local timestamp with offset.
func ClaimRoot(root string, p osx.Platform, container string, now time.Time) (ClaimResult, error) {
	res := ClaimResult{Path: MarkerPath(root)}
	if ok, _ := MarkerValid(root, p); ok {
		res.Owned = true
		return res, nil
	}
	if res.Problem = ClaimProblem(root, p, container); res.Problem != "" {
		return res, nil
	}
	m := Marker{Owner: MarkerOwner, Schema: markerSchema, RootPath: root, CreatedAt: clock.FormatTimestamp(now.In(time.Local)), Note: markerNote}
	body, err := json.MarshalIndent(m, "", "    ")
	if err != nil {
		return res, err
	}
	if err := writeAtomic(res.Path, append(body, '\n'), p); err != nil {
		return res, fmt.Errorf("write the ownership marker: %w", err)
	}
	res.Owned, res.Created = true, true
	return res, nil
}

func writeAtomic(path string, data []byte, p osx.Platform) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = p.SetOwnerOnly(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// VerifyRoot is Get-CamPruneRootRefusal: why nothing at all may be deleted under root, or "" when
// the per-file rules may run. The root must pass CheckRoot and hold a valid marker. dryRun previews
// the next start: an unmarked root that ClaimRoot would claim is not refused.
func VerifyRoot(root string, p osx.Platform, container string, dryRun bool) string {
	if problem := CheckRoot(root, p); problem != "" {
		return problem + ". Point RootPath at a folder of its own"
	}
	ok, why := MarkerValid(root, p)
	if ok {
		return ""
	}
	marker := MarkerPath(root)
	if _, err := os.Lstat(marker); err == nil || !errors.Is(err, fs.ErrNotExist) {
		// a marker exists but is not ours: say so, and say whether a re-claim is possible
		if claim := ClaimProblem(root, p, container); claim != "" {
			return fmt.Sprintf("%s, and the recorder cannot claim the root: %s. Move those entries out of RootPath, or point RootPath at a folder of its own", why, claim)
		}
		if dryRun {
			return ""
		}
		return why + ". The root qualifies - restart the recorder and it claims it again"
	}
	if claim := ClaimProblem(root, p, container); claim != "" {
		return fmt.Sprintf("there is no ownership marker at %s and the recorder cannot claim the root: %s. Move those entries out of RootPath, or point RootPath at a folder of its own, then restart the recorder", marker, claim)
	}
	if dryRun {
		return ""
	}
	return fmt.Sprintf("there is no ownership marker at %s. The root qualifies - restart the recorder and it claims it", marker)
}
