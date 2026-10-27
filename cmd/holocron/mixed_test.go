package main

import (
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
	"holocron/internal/config"
	"holocron/internal/prune"
)

// nameSeg is the name rule archive stats and the doctor count by (no file-type check, as before
// D20): a segment of cam in the spool, or filed under the date in its name; misfiled otherwise.
func nameSeg(f archivetest.TFile, cam, ext string, fold bool) (start time.Time, footage, misfiled bool) {
	start, ok := archivetest.SegStart(f.Name, cam, ext, fold)
	if !ok {
		return start, false, false
	}
	if f.Day != "" && start.Format("2006-01-02") != f.Day {
		return start, false, true
	}
	return start, true, false
}

// TestStatsAndDoctorOnMixedTrees: archive stats and the doctor's per-camera files count the spool
// and the filed days, and report misfiled segments, as a brute-force walk of the tree says.
func TestStatsAndDoctorOnMixedTrees(t *testing.T) {
	var segs, filed, misfiled int
	for _, tr := range archivetest.Mixed(20261030, 60) {
		e, root := layoutEnv(t, tr)
		fold := archive.MatcherFor(e.opts.Platform).FoldCase
		truth := archivetest.Walk(t, root, tr.Container, fold)
		st, err := collectStats(e)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Cameras) != len(truth.Cameras) {
			t.Fatalf("%s: %d cameras, the walk has %d", tr.Name, len(st.Cameras), len(truth.Cameras))
		}
		now := tr.Now.Add(2 * time.Hour)
		r0 := &drun{e: e, now: now, cache: map[string]*onceValue{}}
		statsExt := "." + strings.TrimPrefix(prune.RulesFrom(e.cfg).Container, ".")
		docExt := r0.ext()
		truth = archivetest.Walk(t, root, statsExt, fold)
		docTruth := archivetest.Walk(t, root, docExt, fold)
		for i, c := range truth.Cameras {
			var want camStats
			want.Folder = c.Name
			var wantDoc camFiles
			for _, f := range c.AllFiles() {
				_, ok, mis := nameSeg(f, c.Name, statsExt, fold)
				if mis {
					want.Misfiled++
				}
				if !ok {
					continue
				}
				want.Segments++
				want.Bytes += f.Size
				if f.Day != "" {
					want.Filed++
				}
			}
			for _, f := range docTruth.Cameras[i].AllFiles() {
				start, ok, mis := nameSeg(f, c.Name, docExt, fold)
				if mis {
					wantDoc.Misfiled++
				}
				if ok && !start.Before(now.Add(-25*time.Hour)) {
					wantDoc.Segs = append(wantDoc.Segs, segFile{Name: f.Name, Path: f.Path})
					if f.Day != "" {
						wantDoc.Filed++
					}
				}
			}
			got := st.Cameras[i]
			if got.Folder != want.Folder || got.Segments != want.Segments || got.Bytes != want.Bytes || got.Filed != want.Filed || got.Misfiled != want.Misfiled {
				t.Errorf("%s/%s: stats %+v, want %+v", tr.Name, c.Name, got, want)
			}
			segs, filed, misfiled = segs+got.Segments, filed+got.Filed, misfiled+got.Misfiled
			if safe, err := archive.SafeName(c.Name); err != nil || safe != c.Name {
				continue // the doctor reads cameras by their safe name: "Back Yard" is folder Back_Yard
			}

			r := &drun{e: e, now: now, cache: map[string]*onceValue{}}
			cf := r.files(config.Camera{Name: c.Name})
			gotPaths, wantPaths := map[string]bool{}, map[string]bool{}
			for _, s := range cf.Segs {
				gotPaths[s.Path] = true
			}
			for _, s := range wantDoc.Segs {
				wantPaths[s.Path] = true
			}
			if cf.Err != nil || len(cf.Segs) != len(wantDoc.Segs) || len(gotPaths) != len(wantPaths) || cf.Filed != wantDoc.Filed || cf.Misfiled != wantDoc.Misfiled {
				t.Errorf("%s/%s: doctor files %d (filed %d, misfiled %d) %v, want %d (filed %d, misfiled %d)", tr.Name, c.Name, len(cf.Segs), cf.Filed, cf.Misfiled, cf.Err, len(wantDoc.Segs), wantDoc.Filed, wantDoc.Misfiled)
			}
			for p := range wantPaths {
				if !gotPaths[p] {
					t.Errorf("%s/%s: doctor misses %s", tr.Name, c.Name, p)
				}
			}
		}
	}
	t.Logf("archive stats: %d segments, %d filed, %d misfiled", segs, filed, misfiled)
	if filed < 300 || misfiled < 10 {
		t.Error("the corpus is too thin")
	}
}
