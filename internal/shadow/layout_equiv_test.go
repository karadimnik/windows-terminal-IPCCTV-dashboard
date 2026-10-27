package shadow

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
)

// oldCameraFolders and oldList are cameraFolders and list as they were before the layout library.
func oldCameraFolders(root string) ([]string, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "_") || !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

func oldList(o *Options, root, folder string) (listing, error) {
	var l listing
	dir := filepath.Join(root, folder)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return l, err
	}
	var all []*seg
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !o.Matcher.IsSegmentOf(name, folder, o.Container) {
			l.other = append(l.other, name)
			continue
		}
		_, start, _ := archive.ParseSegment(name)
		path := filepath.Join(dir, name)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		all = append(all, &seg{name: name, path: path, start: start, size: fi.Size(), mod: fi.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if !a.start.Equal(b.start) {
			return a.start.Before(b.start)
		}
		return a.name < b.name
	})
	for i, s := range all {
		age := o.Now.Sub(s.mod)
		newestOpen := i == len(all)-1 && age < time.Duration(o.SegmentSeconds)*time.Second+time.Minute
		if age < o.Quiet || newestOpen {
			l.open = append(l.open, s.name)
			continue
		}
		l.closed = append(l.closed, s)
	}
	sort.Strings(l.open)
	sort.Strings(l.other)
	return l, nil
}

// TestCompareListingEqualsTheOld: over every golden and generated tree, the camera folders and
// every folder's listing of archive compare are what the old code gave.
func TestCompareListingEqualsTheOld(t *testing.T) {
	trees := archivetest.All(t, "../../testdata/golden", 120)
	n := 0
	for _, tr := range trees {
		root, _ := tr.Build(t, t.TempDir())
		want, werr := oldCameraFolders(root)
		got, gerr := cameraFolders(root)
		if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: folders\n old %v %v\n new %v %v", tr.Name, want, werr, got, gerr)
		}
		for _, fold := range []bool{false, true} {
			o := &Options{Old: root, New: root, Now: tr.Now, Container: tr.Container, Matcher: archive.Matcher{FoldCase: fold}}
			o.Defaults()
			for _, folder := range append(want, "Missing") {
				wl, werr := oldList(o, root, folder)
				gl, gerr := list(o, root, folder)
				if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(wl, gl) {
					t.Fatalf("%s/%s fold=%v:\n old %+v %v\n new %+v %v", tr.Name, folder, fold, wl, werr, gl, gerr)
				}
				n += len(gl.closed) + len(gl.open)
			}
		}
	}
	t.Logf("%d trees: identical folders and listings, %d segments listed", len(trees), n)
}
