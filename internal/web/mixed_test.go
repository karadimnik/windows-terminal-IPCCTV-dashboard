package web

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive/archivetest"
)

// webFootage is the footage the index should show for c by D20 and the index's own name rule
// (camera prefix in any case, ".mkv" or ".mp4"), from the brute-force walk: oldest first.
func webFootage(c archivetest.TCamera) []segmentFile {
	var out []segmentFile
	for _, f := range c.AllFiles() {
		var start time.Time
		ok := false
		for _, ext := range []string{".mkv", ".mp4"} {
			if strings.HasSuffix(f.Name, ext) && len(f.Name) > len(c.Name) && strings.EqualFold(f.Name[:len(c.Name)], c.Name) {
				start, ok = archivetest.SegStart(c.Name+f.Name[len(c.Name):], c.Name, ext, false)
			}
			if ok {
				break
			}
		}
		if !ok || (f.Day != "" && start.Format("2006-01-02") != f.Day) {
			continue
		}
		secs := f.MTime.Sub(start).Seconds()
		if secs < 0 {
			secs = 0
		}
		out = append(out, segmentFile{Name: f.Name, Start: start, Date: start.Format("2006-01-02"), Seconds: secs, Bytes: f.Size, writtenAt: f.MTime})
	}
	return out
}

func segKey(s segmentFile) string {
	return s.Start.UTC().Format(time.RFC3339) + "|" + s.Name + "|" + s.Date
}

func sameSegments(t *testing.T, where string, got, want []segmentFile) {
	t.Helper()
	g, w := []string{}, []string{}
	for _, s := range got {
		g = append(g, segKey(s))
	}
	for _, s := range want {
		w = append(w, segKey(s))
	}
	sort.Strings(g)
	sort.Strings(w)
	if strings.Join(g, "\n") != strings.Join(w, "\n") {
		t.Errorf("%s:\n got  %v\n want %v", where, g, w)
	}
}

// TestFootageOnMixedTrees: on spool-and-day trees the footage index, the day list, a chosen day's
// segments and the clip resolver agree with a brute-force walk of the tree; a misfiled segment is
// not footage and cannot be played. It also counts the listings per request (A3): a chosen day is
// at most two (the spool and that day), the camera's day list one per folder.
func TestFootageOnMixedTrees(t *testing.T) {
	fold := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	var segs, dayReqs, clips, refused int
	maxDayListings := int64(0)
	for _, tr := range archivetest.Mixed(20261029, 60) {
		api, _, _ := newTestAPI(t)
		root, _ := tr.Build(t, t.TempDir())
		api.cfg.ArchiveRoot = root
		truth := archivetest.Walk(t, root, tr.Container, fold)
		cams, err := api.archiveCameras()
		if err != nil {
			t.Fatal(err)
		}
		for _, cam := range cams {
			c, ok := truth.Camera(cam.Dir)
			if !ok {
				t.Fatalf("%s: camera %s not in the walk", tr.Name, cam.Dir)
			}
			want := webFootage(c)
			got, err := api.listSegments(cam)
			if err != nil {
				t.Fatal(err)
			}
			sameSegments(t, tr.Name+"/"+cam.Dir+" index", got, want)
			segs += len(got)

			// the day list through the handler: one listing per folder of the camera
			before := api.footageCache.Stats().Listings
			rec := httptest.NewRecorder()
			api.handleFootage(rec, httptest.NewRequest("GET", "/api/footage?camera="+url.QueryEscape(cam.Name), nil))
			if n := api.footageCache.Stats().Listings - before; n != int64(1+len(c.Days)) {
				t.Errorf("%s/%s: day list made %d listings, want %d", tr.Name, cam.Dir, n, 1+len(c.Days))
			}
			var body FootageResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			byDate := map[string]int{}
			for _, s := range want {
				byDate[s.Date]++
			}
			if len(body.Dates) != len(byDate) {
				t.Errorf("%s/%s: %d dates, want %d", tr.Name, cam.Dir, len(body.Dates), len(byDate))
			}
			for _, d := range body.Dates {
				if d.Segments != byDate[d.Date] {
					t.Errorf("%s/%s: %s has %d segments, want %d", tr.Name, cam.Dir, d.Date, d.Segments, byDate[d.Date])
				}
				// a chosen day: the spool and that day folder only
				before := api.footageCache.Stats().Listings
				rec := httptest.NewRecorder()
				api.handleFootage(rec, httptest.NewRequest("GET", "/api/footage?camera="+url.QueryEscape(cam.Name)+"&date="+d.Date, nil))
				n := api.footageCache.Stats().Listings - before
				if n > 2 {
					t.Errorf("%s/%s %s: a day page made %d listings", tr.Name, cam.Dir, d.Date, n)
				}
				maxDayListings = max(maxDayListings, n)
				var day FootageResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &day); err != nil {
					t.Fatal(err)
				}
				if len(day.Segments) != byDate[d.Date] {
					t.Errorf("%s/%s %s: day page has %d segments, want %d", tr.Name, cam.Dir, d.Date, len(day.Segments), byDate[d.Date])
				}
				dayReqs++
			}

			// clips: every file name of the camera, its case variants and traversal attempts
			for _, f := range c.AllFiles() {
				rel, _ := filepath.Rel(c.Dir, f.Path)
				for _, ask := range []string{f.Name, strings.ToUpper(f.Name), strings.ToLower(f.Name), "../" + f.Name, "..\\" + f.Name, rel, filepath.ToSlash(rel), f.Path, f.Name + "\x00", "../" + cam.Dir + "/" + filepath.ToSlash(rel)} {
					gp, gerr := api.resolveSegment(cam, ask)
					wp, wok := c.Resolve(ask, fold)
					if wok {
						fi, err := os.Stat(wp)
						_, named := parseSegmentName(cam.Dir, ask)
						wok = err == nil && fi.Mode().IsRegular() && named && pathInside(root, wp)
					}
					switch {
					case wok != (gerr == nil):
						t.Errorf("%s/%s: clip %q = %q %v, want %q %v", tr.Name, cam.Dir, ask, gp, gerr, wp, wok)
					case wok && gp != wp:
						t.Errorf("%s/%s: clip %q = %q, want %q", tr.Name, cam.Dir, ask, gp, wp)
					case wok:
						clips++
					default:
						refused++
					}
				}
			}
			for _, m := range c.Misfiled {
				if p, err := api.resolveSegment(cam, m.Name); err == nil && p == m.Path {
					t.Errorf("%s: a misfiled segment plays: %s", tr.Name, m.Path)
				}
			}
		}
	}
	t.Logf("%d segments indexed, %d day pages (at most %d listings each), %d clips resolved, %d refused", segs, dayReqs, maxDayListings, clips, refused)
	if segs < 500 || dayReqs < 100 || clips < 500 {
		t.Error("the corpus is too thin")
	}
}
