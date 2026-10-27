package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
	"holocron/internal/config"
	"holocron/internal/osx"
	"holocron/internal/prune"
)

// oldCollectStats is collectStats as it was before the layout library (reference).
func oldCollectStats(e *env) (archiveStats, error) {
	st := archiveStats{Root: e.paths.Root, Cameras: []camStats{}, ReserveGB: e.cfg.ReserveGB, RetentionDays: e.cfg.MaxRetentionDays}
	container := "." + strings.TrimPrefix(prune.RulesFrom(e.cfg).Container, ".")
	m := archive.MatcherFor(e.opts.Platform)
	known := map[string]string{}
	if cams, err := app.LoadCameras(e.paths.Cameras); err == nil {
		for _, c := range cams.List {
			if safe, err := c.SafeName(); err == nil {
				known[strings.ToLower(safe)] = c.Name
			}
		}
	}
	entries, err := os.ReadDir(e.paths.Root)
	if err != nil {
		return st, fmt.Errorf("cannot read the archive root: %w", err)
	}
	var oldest, newest time.Time
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		cs := camStats{Folder: entry.Name(), Camera: known[strings.ToLower(entry.Name())]}
		files, err := os.ReadDir(filepath.Join(e.paths.Root, entry.Name()))
		if err != nil {
			continue
		}
		var lo, hi time.Time
		for _, f := range files {
			if f.IsDir() || !m.IsSegmentOf(f.Name(), entry.Name(), container) {
				continue
			}
			_, start, ok := archive.ParseSegment(f.Name())
			info, err := f.Info()
			if !ok || err != nil {
				continue
			}
			cs.Segments++
			cs.Bytes += info.Size()
			if lo.IsZero() || start.Before(lo) {
				lo = start
			}
			if hi.IsZero() || start.After(hi) {
				hi = start
			}
		}
		if cs.Segments > 0 {
			lo, hi := lo, hi
			cs.Oldest, cs.Newest = &lo, &hi
			if oldest.IsZero() || lo.Before(oldest) {
				oldest = lo
			}
			if newest.IsZero() || hi.After(newest) {
				newest = hi
			}
		}
		st.Segments += cs.Segments
		st.Bytes += cs.Bytes
		st.Cameras = append(st.Cameras, cs)
	}
	sort.Slice(st.Cameras, func(i, j int) bool { return st.Cameras[i].Folder < st.Cameras[j].Folder })
	if free, err := e.opts.Platform.DiskFree(e.paths.Root); err == nil {
		st.FreeGB = math.Round(float64(free)/gib*100) / 100
	}
	if span := newest.Sub(oldest).Seconds(); !oldest.IsZero() && span >= 600 {
		rate := math.Round(float64(st.Bytes)/gib/(span/86400)*100) / 100
		st.GBPerDay = &rate
		if rate > 0 {
			days := math.Round(math.Max(st.FreeGB-st.ReserveGB, 0)/rate*100) / 100
			st.ProjectedDays = &days
		}
	} else {
		st.Note = "less than 10 minutes of recordings: no rate to project from yet"
	}
	return st, nil
}

// fixedFree is the running OS with a fixed free-space answer, so two runs agree.
type fixedFree struct{ osx.Platform }

func (fixedFree) DiskFree(string) (uint64, error) { return 123 << 30, nil }

// layoutEnv is an env over a built tree, with cameras.json naming some of its folders.
func layoutEnv(t *testing.T, tr archivetest.Tree) (*env, string) {
	root, _ := tr.Build(t, t.TempDir())
	cams := filepath.Join(t.TempDir(), "cameras.json")
	body := `[{"Name":"Cam A","IP":"192.0.2.10","Record":true},{"Name":"Front Door","IP":"192.0.2.11","Record":true},{"Name":"Workshop","IP":"192.0.2.12","Record":true},{"Name":"Back Yard","IP":"192.0.2.13","Record":false}]`
	if err := os.WriteFile(cams, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p := fixedFree{osx.Current()}
	cfg := config.Defaults(p)
	cfg.Container = strings.TrimPrefix(tr.Container, ".")
	return &env{opts: app.Options{Root: root, Platform: p}, paths: app.Paths{Root: root, Cameras: cams}, cfg: cfg}, root
}

func TestArchiveStatsEqualTheOld(t *testing.T) {
	trees := archivetest.All(t, "../../testdata/golden", 120)
	segs := 0
	for _, tr := range trees {
		e, _ := layoutEnv(t, tr)
		want, werr := oldCollectStats(e)
		got, gerr := collectStats(e)
		if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(want, got) {
			t.Fatalf("%s:\n old %+v %v\n new %+v %v", tr.Name, want, werr, got, gerr)
		}
		segs += got.Segments
	}
	t.Logf("%d trees: identical archive stats, %d segments counted", len(trees), segs)
}

// oldFiles is drun.files as it was before the layout library (reference).
func oldFiles(r *drun, cam config.Camera) camFiles {
	safe, err := archive.SafeName(cam.Name)
	if err != nil {
		return camFiles{Err: err}
	}
	cf := camFiles{Safe: safe, Dir: filepath.Join(r.e.paths.Root, safe)}
	entries, err := os.ReadDir(cf.Dir)
	if err != nil {
		cf.Err = err
		return cf
	}
	m := archive.MatcherFor(r.e.opts.Platform)
	since := r.now.Add(-25 * time.Hour)
	for _, en := range entries {
		if en.IsDir() || !m.IsSegmentOf(en.Name(), safe, r.ext()) {
			continue
		}
		_, start, ok := archive.ParseSegment(en.Name())
		info, err := en.Info()
		if !ok || err != nil || start.Before(since) {
			continue
		}
		cf.Segs = append(cf.Segs, segFile{Name: en.Name(), Start: start, MTime: info.ModTime(), Size: info.Size()})
	}
	sort.Slice(cf.Segs, func(i, j int) bool { return cf.Segs[i].Start.Before(cf.Segs[j].Start) })
	return cf
}

// TestDoctorFilesEqualTheOld: for every folder of every tree (and the display names that map to
// folders), doctor's per-camera segments of the last 25 h are what the old code gave; each one's
// Path is the listed file.
func TestDoctorFilesEqualTheOld(t *testing.T) {
	trees := archivetest.All(t, "../../testdata/golden", 120)
	segs := 0
	for _, tr := range trees {
		e, root := layoutEnv(t, tr)
		ents, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{"Cam A", "Front Door", "Back Yard", "Missing Camera", "CON"}
		for _, en := range ents {
			names = append(names, en.Name())
		}
		for _, name := range names {
			r := &drun{e: e, now: tr.Now.Add(12 * time.Hour), cache: map[string]*onceValue{}}
			cam := config.Camera{Name: name}
			want, got := oldFiles(r, cam), r.files(cam)
			for i, s := range got.Segs {
				if s.Path != filepath.Join(got.Dir, s.Name) {
					t.Fatalf("%s/%s: Path %q", tr.Name, name, s.Path)
				}
				got.Segs[i].Path = ""
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("%s/%s:\n old %+v\n new %+v", tr.Name, name, want, got)
			}
			segs += len(got.Segs)
		}
	}
	if segs == 0 {
		t.Error("no segment within 25 h in any tree")
	}
	t.Logf("%d trees: identical doctor file lists, %d segments", len(trees), segs)
}
