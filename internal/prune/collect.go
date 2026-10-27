package prune

import (
	"os"
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
	l := archive.Layout{Root: root, Matcher: in.Matcher, Container: r.Container}
	folders, err := l.Cameras()
	if err != nil {
		return in, err
	}
	for _, f := range folders {
		if f.Link {
			in.Skipped = append(in.Skipped, f.Path+" (reparse point: its content is outside the archive)")
			continue
		}
		cam := Camera{Dir: f.Path}
		children, err := l.Entries(f.Name)
		if err != nil {
			continue
		}
		for _, c := range children {
			ci, err := os.Lstat(c.Path) // not the listing's information: the pruner's own lstat
			if err != nil || ci.IsDir() {
				continue
			}
			cam.Files = append(cam.Files, File{
				Name: c.Name(), MTime: ci.ModTime().UTC(), Size: ci.Size(), Created: archive.CreatedTime(ci),
				Reparse: archive.IsReparse(ci) || !ci.Mode().IsRegular(),
			})
		}
		if scan, err := l.Scan(f.Name, now, int(r.SegmentSeconds)); err == nil {
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
