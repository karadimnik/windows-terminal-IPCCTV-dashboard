package prune

import (
	"os"
	"time"

	"holocron/internal/archive"
	"holocron/internal/osx"
)

// Collect reads what Plan needs from the filesystem, read-only: the root and marker verdicts
// (archive.CheckRoot, archive.VerifyRoot), then - only when both pass - every immediate
// subfolder of the root that does not start with "_", its files and the files of its day folders
// (decision D20: archive.Layout's spool and day folders; any other sub-folder, and anything inside
// a day folder that is a folder, is not entered; links are reported and never followed), each
// folder's archive.Layout Newest, and the free space. Every file is listed, footage or not: Plan
// classifies. dryRun previews the next start for an unmarked root that would be claimed
// (VerifyRoot).
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
		spool, days, err := l.Spool(f.Name)
		if err != nil {
			continue
		}
		files := spool
		for i := len(days) - 1; i >= 0; i-- { // oldest day first, as Layout.Files
			df, err := l.DayFiles(days[i])
			if err != nil {
				continue // vanished since the listing
			}
			cam.Days = append(cam.Days, days[i].Name())
			files = append(files, df...)
		}
		for _, c := range files {
			ci, err := os.Lstat(c.Path) // not the listing's information: the pruner's own lstat
			if err != nil || ci.IsDir() {
				continue
			}
			cam.Files = append(cam.Files, File{
				Name: c.Name(), Day: c.Day, MTime: ci.ModTime().UTC(), Size: ci.Size(), Created: archive.CreatedTime(ci),
				Reparse: archive.IsReparse(ci) || !ci.Mode().IsRegular(),
			})
		}
		if n, ok, err := l.Newest(f.Name); err == nil && ok {
			cam.Newest = n.Name()
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
