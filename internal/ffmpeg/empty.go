package ffmpeg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"holocron/internal/archive"
)

// EmptyOptions are the gates of RemoveEmptySegment.
type EmptyOptions struct {
	// Root is the archive root. When set, the camera folder must be a direct child of it and not
	// start with "_" (the pruner's per-file gates 4 and 5, IR 2.4).
	Root string
	// Matcher decides what a segment name is; the zero value is case-sensitive, use
	// archive.DefaultMatcher or archive.MatcherFor(platform) for the OS rule.
	Matcher archive.Matcher
	// RootRefusal is the root gate (Get-CamPruneRootRefusal): a non-empty answer, for example from
	// archive.VerifyRoot, means nothing is deleted. Nil: no root gate.
	RootRefusal func() string
}

// EmptyResult says what RemoveEmptySegment did. Messages are the PowerShell product's log lines.
type EmptyResult struct {
	Removed  []string // file names removed
	Foreign  int      // empty files left alone: not segments of this camera, or a link
	Refusal  string   // the root gate's reason when it stopped the work
	Failures []string // "<name>: <error>" for removals that failed
	Messages []string
}

// RemoveEmptySegment is Remove-CamEmptySegment (IR 3.8). Call it after the child has EXITED (or
// been killed): a child stopped in the second after a segment roll leaves a zero-byte file because
// the Matroska header was still in ffmpeg's write buffer. It removes exactly the regular files in dir
// whose length is zero AND which pass the pruner's per-file gate - a link or reparse point is
// never followed or removed, the file name must be a strict segment name of the folder's camera
// (folder name = safe name) in the configured container, and the folder must sit directly under the
// root. Anything else, empty or not, is left alone and counted as foreign. A file with content is
// never touched: it starts with a full header and plays after a hard kill.
func RemoveEmptySegment(dir, container string, o EmptyOptions) EmptyResult {
	var r EmptyResult
	// the folder itself is listed (not Root/<its name>), so the gates below judge what was listed
	c := filepath.Clean(dir)
	entries, err := archive.Layout{Root: filepath.Dir(c)}.Entries(filepath.Base(c))
	if err != nil {
		return r
	}
	type file struct{ name, path string } // name "": a link that reads as empty
	var empty []file
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := os.Lstat(e.Path)
		if err != nil {
			continue
		}
		if archive.IsReparse(fi) { // a link is never removed; one that reads as empty counts as foreign
			if ti, err := os.Stat(e.Path); err == nil && ti.Size() == 0 {
				empty = append(empty, file{})
			}
			continue
		}
		if fi.Mode().IsRegular() && fi.Size() == 0 {
			empty = append(empty, file{e.Name(), e.Path})
		}
	}
	if len(empty) == 0 {
		return r
	}
	if o.RootRefusal != nil {
		if why := o.RootRefusal(); why != "" {
			r.Refusal = why
			r.Messages = append(r.Messages, fmt.Sprintf("left %d empty file(s) in %s alone: pruning is disabled - %s", len(empty), dir, why))
			return r
		}
	}
	folderOK := folderGate(dir, o)
	safe := filepath.Base(dir)
	for _, f := range empty {
		if f.name == "" || !folderOK || !o.Matcher.IsSegmentOf(f.name, safe, container) {
			r.Foreign++
			continue
		}
		if err := os.Remove(f.path); err != nil {
			r.Failures = append(r.Failures, f.name+": "+err.Error())
			r.Messages = append(r.Messages, fmt.Sprintf("could not remove empty segment %s: %v", f.name, err))
			continue
		}
		r.Removed = append(r.Removed, f.name)
		r.Messages = append(r.Messages, fmt.Sprintf("removed empty segment %s: the child was stopped before its header reached disk", f.name))
	}
	if r.Foreign > 0 {
		r.Messages = append(r.Messages, fmt.Sprintf("%d empty foreign file(s) left alone in %s: not recorder segments", r.Foreign, dir))
	}
	return r
}

// folderGate is the folder part of the pruner's gate: not a link, directly under the root, no "_" prefix.
func folderGate(dir string, o EmptyOptions) bool {
	if fi, err := os.Lstat(dir); err != nil || archive.IsReparse(fi) || !fi.IsDir() {
		return false
	}
	if strings.HasPrefix(filepath.Base(dir), "_") {
		return false
	}
	if o.Root != "" {
		parent, root := filepath.Clean(filepath.Dir(dir)), filepath.Clean(o.Root)
		if o.Matcher.FoldCase {
			return strings.EqualFold(parent, root)
		}
		return parent == root
	}
	return true
}
