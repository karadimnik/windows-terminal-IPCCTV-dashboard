package prune

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
	"holocron/internal/osx"
)

// oldCollect is Collect as it was before the layout library (its Newest is archive.Matcher.Scan,
// proved equal to the old scan in internal/archive; created is archive.CreatedTime, the same code moved).
func oldCollect(root string, p osx.Platform, r Rules, now time.Time, dryRun bool) (Inputs, error) {
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
			continue
		}
		if archive.IsReparse(fi) {
			in.Skipped = append(in.Skipped, dir+" (reparse point: its content is outside the archive)")
			continue
		}
		if !fi.IsDir() {
			continue
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
				Name: c.Name(), MTime: ci.ModTime().UTC(), Size: ci.Size(), Created: archive.CreatedTime(ci),
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

// TestCollectEqualsTheOldCollect: over the prune goldens, the names goldens and generated trees,
// each with a valid ownership marker (so the listing runs), the new Collect gives exactly the old
// Inputs. Free space is fixed so both runs see the same number.
func TestCollectEqualsTheOldCollect(t *testing.T) {
	trees := archivetest.All(t, "../../testdata/golden", 120)
	cams, files := 0, 0
	for _, tr := range trees {
		root, _ := tr.Build(t, t.TempDir())
		if err := os.MkdirAll(filepath.Join(root, "_state"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{ "Owner": %q, "Schema": 1, "RootPath": %q }`, archive.MarkerOwner, root)
		if err := os.WriteFile(archive.MarkerPath(root), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		p := freePlatform{osx.Current(), &scaledFS{OSFS: OSFS{Platform: osx.Current()}, Scale: 1}}
		p.fs.Reset(100)
		r := Rules{Container: tr.Container, SegmentSeconds: float64(tr.SegmentSeconds), StallFactor: 2.5}
		for _, dry := range []bool{false, true} {
			want, werr := oldCollect(root, p, r, tr.Now, dry)
			got, gerr := Collect(root, p, r, tr.Now, dry)
			if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(want, got) {
				t.Fatalf("%s dry=%v:\n old %+v %v\n new %+v %v", tr.Name, dry, want, werr, got, gerr)
			}
			if want.MarkerProblem != "" || want.RootProblem != "" {
				t.Fatalf("%s: the listing did not run: %q %q", tr.Name, want.RootProblem, want.MarkerProblem)
			}
		}
		in, _ := Collect(root, p, r, tr.Now, false)
		cams += len(in.Cameras)
		for _, c := range in.Cameras {
			files += len(c.Files)
		}
	}
	t.Logf("%d trees, %d camera folders, %d files collected identically", len(trees), cams, files)
}
