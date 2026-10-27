package web

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive/archivetest"
)

// The footage index as it was before the layout library (reference functions).

func (a *API) oldArchiveCameras() ([]archiveCamera, error) {
	entries, err := os.ReadDir(a.cfg.ArchiveRoot)
	if err != nil {
		return nil, fmt.Errorf("read the archive root: %w", scrubErr(err))
	}
	rows, _, _ := a.readCameras()
	byDir := make(map[string]archiveCamera, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		byDir[strings.ToLower(entry.Name())] = archiveCamera{Dir: entry.Name(), Name: entry.Name()}
	}
	out := make([]archiveCamera, 0, len(byDir))
	for _, row := range rows {
		key := strings.ToLower(safeCameraName(row.Name))
		cam, ok := byDir[key]
		if !ok {
			continue
		}
		cam.Name = row.Name
		cam.Known = true
		out = append(out, cam)
		delete(byDir, key)
	}
	orphans := make([]archiveCamera, 0, len(byDir))
	for _, cam := range byDir {
		orphans = append(orphans, cam)
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Dir < orphans[j].Dir })
	return append(out, orphans...), nil
}

func (a *API) oldListSegments(cam archiveCamera) ([]segmentFile, error) {
	dir := filepath.Join(a.cfg.ArchiveRoot, cam.Dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the folder for camera %q: %w", cam.Name, scrubErr(err))
	}
	out := make([]segmentFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		start, ok := parseSegmentName(cam.Dir, entry.Name())
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		seconds := info.ModTime().Sub(start).Seconds()
		if seconds < 0 {
			seconds = 0
		}
		out = append(out, segmentFile{Name: entry.Name(), Start: start, Date: start.Format("2006-01-02"), Seconds: seconds, Bytes: info.Size(), writtenAt: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	if last := len(out) - 1; last >= 0 && time.Since(out[last].writtenAt) < inProgressWindow {
		out[last].InProgress = true
	}
	return out, nil
}

func (a *API) oldResolveSegment(cam archiveCamera, segment string) (string, error) {
	if _, ok := parseSegmentName(cam.Dir, segment); !ok {
		return "", errNoSuchSegment
	}
	full := filepath.Join(a.cfg.ArchiveRoot, cam.Dir, segment)
	if !pathInside(a.cfg.ArchiveRoot, full) {
		return "", errNoSuchSegment
	}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return "", errNoSuchSegment
	}
	return full, nil
}

// TestFootageEqualsTheOldIndex: over every golden and generated tree, the camera list, every
// camera's segments (twice, the second through the cache) and the clip resolution of every listed
// name and its variants are what the old code gave. A resolved clip is the same file; the path is
// the listed spelling, where the old one echoed the request's.
func TestFootageEqualsTheOldIndex(t *testing.T) {
	trees := archivetest.All(t, "../../testdata/golden", 120)
	resolved := 0
	for _, tr := range trees {
		api, _, _ := newTestAPI(t)
		root, _ := tr.Build(t, t.TempDir())
		api.cfg.ArchiveRoot = root
		want, werr := api.oldArchiveCameras()
		got, gerr := api.archiveCameras()
		if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: cameras\n old %+v %v\n new %+v %v", tr.Name, want, werr, got, gerr)
		}
		for _, cam := range want {
			ws, werr := api.oldListSegments(cam)
			for call := 0; call < 2; call++ {
				gs, gerr := api.listSegments(cam)
				if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(ws, gs) {
					t.Fatalf("%s/%s call %d: segments\n old %+v %v\n new %+v %v", tr.Name, cam.Dir, call, ws, werr, gs, gerr)
				}
			}
			ents, _ := os.ReadDir(filepath.Join(root, cam.Dir))
			var asks []string
			for _, e := range ents {
				n := e.Name()
				asks = append(asks, n, strings.ToLower(n), strings.ToUpper(n), "../"+n, "..\\"+n, cam.Dir+"/../"+n, n+" ")
			}
			asks = append(asks, "", "..", cam.Dir+"_20261025_033000+0300.mkv", cam.Dir+"_20261025_033000+0300.mp4")
			for _, ask := range asks {
				wp, werr := api.oldResolveSegment(cam, ask)
				gp, gerr := api.resolveSegment(cam, ask)
				if (werr == nil) != (gerr == nil) {
					t.Fatalf("%s/%s: resolve %q\n old %q %v\n new %q %v", tr.Name, cam.Dir, ask, wp, werr, gp, gerr)
				}
				if werr == nil {
					wi, _ := os.Stat(wp)
					gi, _ := os.Stat(gp)
					if !os.SameFile(wi, gi) || (wp != gp && !strings.EqualFold(wp, gp)) {
						t.Fatalf("%s/%s: resolve %q gives %q, the old code %q", tr.Name, cam.Dir, ask, gp, wp)
					}
					resolved++
				}
			}
		}
	}
	if resolved == 0 {
		t.Error("no clip resolved: the corpus does not exercise resolveSegment")
	}
	t.Logf("%d trees: identical camera lists and segment indexes, %d clip requests resolved to the same file", len(trees), resolved)
}
