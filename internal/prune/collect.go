package prune

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"holocron/internal/archive"
	"holocron/internal/osx"
)

// Collect reads what Plan needs from the filesystem, read-only: the root and marker verdicts
// (archive.CheckRoot, archive.VerifyRoot), then - only when both pass - every immediate
// subfolder of the root that does not start with "_" and its files (subfolders inside a camera
// folder are not entered, links are reported and never followed), each folder's archive.Scan
// Newest, and the free space. dryRun previews the next start for an unmarked root that would be
// claimed (VerifyRoot).
func Collect(root string, p osx.Platform, r Rules, now time.Time, dryRun bool) (Inputs, error) {
	in := Inputs{Root: root, Matcher: archive.MatcherFor(p), Rules: r, Now: now.UTC()}
	if in.RootProblem = archive.CheckRoot(root, p); in.RootProblem != "" {
		return in, nil
	}
	if in.MarkerProblem = archive.VerifyRoot(root, p, r.Container, dryRun); in.MarkerProblem != "" {
		return in, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return in, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "_") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil {
			continue // vanished
		}
		if archive.IsReparse(fi) {
			in.Skipped = append(in.Skipped, dir+" (reparse point: its content is outside the archive)")
			continue
		}
		if !fi.IsDir() {
			continue // a file at the top level is never pruned
		}
		cam := Camera{Dir: dir}
		children, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, c := range children {
			ci, err := os.Lstat(filepath.Join(dir, c.Name()))
			if err != nil || ci.IsDir() {
				continue
			}
			cam.Files = append(cam.Files, File{
				Name: c.Name(), MTime: ci.ModTime().UTC(), Size: ci.Size(),
				Reparse: archive.IsReparse(ci) || !ci.Mode().IsRegular(),
			})
		}
		if scan, err := in.Matcher.Scan(dir, r.Container, now, int(r.SegmentSeconds)); err == nil {
			cam.Newest = scan.Newest
		}
		in.Cameras = append(in.Cameras, cam)
	}
	free, err := p.DiskFree(root)
	if err != nil {
		return in, err
	}
	in.FreeBytes = free
	return in, nil
}
