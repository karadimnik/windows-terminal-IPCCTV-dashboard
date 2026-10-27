package supervise

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
	"holocron/internal/clock"
)

func mseg(cam string, start time.Time) string {
	return cam + "_" + start.In(time.FixedZone("", 3*3600)).Format("20060102_150405-0700") + ".mkv"
}

func mday(start time.Time) string { return start.In(time.FixedZone("", 3*3600)).Format("2006-01-02") }

// spoolOnly is a Scanner that reads the spool alone (the view before D20's spool-first rule), to
// show what the rule prevents.
type spoolOnly struct{ archive.Layout }

func (s spoolOnly) Scan(dir string, now time.Time) (archive.DirScan, error) {
	return s.ScanSpool(filepath.Base(dir), now, 900)
}

// tickObs is what a status tick decides for one camera: the alert-110 events, the file progress
// (stall watchdog and alert input) and the status row fields that come from the scans.
type tickObs struct {
	Alerted, Recovered bool
	Progress           time.Time
	Segment            string
	SegmentBytes       int64
	SinceClosed        float64
	Files, Bytes       int64
	DiskFiles          int64
	DiskBytes          int64
	DiskOldest         time.Time
	DiskNewest         time.Time
}

// ticks runs a manager with the production scanner (or sc) over root from 3 h before now, then a
// status tick at now plus each offset (seconds), the files unchanged.
func ticks(t *testing.T, root string, now time.Time, sc Scanner, offsets []float64) []tickObs {
	t.Helper()
	s := defaultSettings()
	s.RootPath = root
	a := ArchiveScanner{Layout: archive.Layout{Root: root, Matcher: archive.DefaultMatcher, Container: ".mkv", Cache: archive.NewCache()}, SegmentSeconds: 900}
	if sc == nil {
		sc = a
	}
	start := now.Add(-3 * time.Hour)
	r := newRigAt(t, s, start)
	r.m = New(s, Deps{Clock: r.clk, Scanner: sc, Alerts: r.alerts, Log: r.m.log})
	r.m.Reconcile(cams([]string{"cam1"}))
	r.clk.Advance(now.Sub(start))
	var out []tickObs
	at := 0.0
	for _, off := range offsets {
		r.clk.Advance(time.Duration((off - at) * float64(time.Second)))
		at = off
		r.m.Refresh()
		var o tickObs
		for _, ev := range r.m.Down() {
			o.Alerted, o.Recovered = o.Alerted || ev.Alerted, o.Recovered || ev.Recovered
		}
		wall, _ := r.m.now()
		disk, err := a.ScanRoot(root, wall)
		if err != nil {
			t.Fatal(err)
		}
		o.DiskFiles, o.DiskBytes, o.DiskOldest, o.DiskNewest = disk.Files, disk.Bytes, disk.Oldest, disk.Newest
		o.Progress = r.m.entries["cam1"].fileProgress()
		row := r.m.Rows(disk.Dirs)[0]
		if row.CurrentSegment != nil {
			o.Segment, o.SegmentBytes = *row.CurrentSegment, *row.CurrentSegmentBytes
		}
		if row.SecondsSinceLastClosedSegment != nil {
			o.SinceClosed = *row.SecondsSinceLastClosedSegment
		}
		if row.ArchiveFiles != nil {
			o.Files, o.Bytes = *row.ArchiveFiles, *row.ArchiveBytes
		}
		out = append(out, o)
	}
	return out
}

// TestMixedTreeTickEqualsItsFlatCopy: alert 110, the stall watchdog's file progress and the status
// row of a camera on a spool-and-day tree are exactly those of the same files in one flat folder,
// tick after tick, in every shape the L4 mover can leave (the spool holding the open and the newest
// closed segment, or only the open one, the midnight roll, an empty spool) and for a stalled and a
// stopped camera. The spool-only view would raise alert 110 at once for the empty spool.
func TestMixedTreeTickEqualsItsFlatCopy(t *testing.T) {
	t1 := time.Date(2026, 10, 17, 8, 30, 1, 0, time.UTC) // 11:30:01 local, +0300
	open := t1.Truncate(15 * time.Minute)
	midnight := time.Date(2026, 10, 17, 21, 0, 5, 0, time.UTC) // 00:00:05 local on 18 October
	stalled := t1.Add(-1300 * time.Second)                     // a segment that started, then stopped growing 1000 s ago
	file := func(path string, mt time.Time) archivetest.Node {
		return archivetest.Node{Path: path, Kind: "file", Bytes: int64(100 + mt.Second()), MTime: mt}
	}
	spool := func(st, mt time.Time) archivetest.Node { return file("archive/cam1/"+mseg("cam1", st), mt) }
	filed := func(st, mt time.Time) archivetest.Node {
		return file("archive/cam1/"+mday(st)+"/"+mseg("cam1", st), mt)
	}
	day := func(from time.Time, n int) (out []archivetest.Node) {
		for k := 1; k <= n; k++ {
			st := from.Add(-time.Duration(k) * 15 * time.Minute)
			out = append(out, filed(st, st.Add(15*time.Minute)))
		}
		return out
	}
	offsets := []float64{0, 300, 600, 934, 940, 1500, 2200, 2400, 4000}
	for _, sc := range []struct {
		name         string
		now          time.Time
		nodes        []archivetest.Node
		aliveAtStart bool
	}{
		{"spool holds the open and the newest closed segment", t1,
			append([]archivetest.Node{spool(open, t1.Add(-time.Second)), spool(open.Add(-15*time.Minute), open)}, day(open.Add(-15*time.Minute), 60)...), true},
		{"spool holds only the open segment", t1, append([]archivetest.Node{spool(open, t1.Add(-time.Second))}, day(open, 40)...), true},
		{"the midnight roll: the closed segment is in yesterday's folder", midnight,
			append([]archivetest.Node{spool(midnight.Truncate(15*time.Minute), midnight.Add(-time.Second))}, day(midnight.Truncate(15*time.Minute), 5)...), true},
		{"an empty spool with recent filed segments", t1, []archivetest.Node{filed(open.Add(-15*time.Minute), t1.Add(-60*time.Second)), filed(open.Add(-30*time.Minute), open.Add(-15*time.Minute))}, true},
		{"a stalled camera: the open segment stopped growing", t1, []archivetest.Node{spool(stalled, t1.Add(-1000*time.Second)), filed(stalled.Add(-15*time.Minute), stalled)}, false},
		{"a camera that stopped two hours ago", t1, day(t1.Add(-2*time.Hour), 6), false},
	} {
		tr := archivetest.Tree{Name: sc.name, Container: ".mkv", Now: sc.now, SegmentSeconds: 900, Nodes: sc.nodes}
		root, _ := tr.Build(t, t.TempDir())
		flat, _ := tr.Flat().Build(t, t.TempDir())
		got, want := ticks(t, root, sc.now, nil, offsets), ticks(t, flat, sc.now, nil, offsets)
		if !reflect.DeepEqual(got, want) {
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("%s +%v s:\n mixed %+v\n flat  %+v", sc.name, offsets[i], got[i], want[i])
				}
			}
		}
		if got[0].Alerted == sc.aliveAtStart || got[0].Segment == "" {
			t.Errorf("%s: alert at the first tick %v, segment %q", sc.name, got[0].Alerted, got[0].Segment)
		}
		if sc.name == "an empty spool with recent filed segments" {
			if !anyAlert(got) {
				t.Errorf("%s: alert 110 never raised once nothing is written", sc.name)
			}
			l := archive.Layout{Root: root, Matcher: archive.DefaultMatcher, Container: ".mkv"}
			if blind := ticks(t, root, sc.now, spoolOnly{l}, offsets[:1]); !blind[0].Alerted {
				t.Errorf("%s: the spool-only view should have raised alert 110 at once (the false alert the rule prevents)", sc.name)
			}
		}
	}
}

func anyAlert(obs []tickObs) bool {
	for _, o := range obs {
		if o.Alerted {
			return true
		}
	}
	return false
}

// TestStatusTickCostIsOneListingPerCamera (cost, A3): six recording cameras with 30 filed days of
// 96 segments each and a spool of two: every status tick (Manager.Refresh through the production
// scanner and its cache) lists each camera folder once and no day folder.
func TestStatusTickCostIsOneListingPerCamera(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 10, 0, time.UTC)
	tr := archivetest.Filed("cost", 6, 30, 96, 2, now)
	root, _ := tr.Build(t, t.TempDir())
	s := defaultSettings()
	s.RootPath = root
	cache := archive.NewCache()
	sc := ArchiveScanner{Layout: archive.Layout{Root: root, Matcher: archive.DefaultMatcher, Container: ".mkv", Cache: cache}, SegmentSeconds: 900}
	clk := clock.NewFake(now)
	r := newRig(t, s)
	m := New(s, Deps{Clock: clk, Scanner: sc, Alerts: r.alerts, Log: r.m.log})
	var names []string
	for c := 0; c < 6; c++ {
		names = append(names, fmt.Sprintf("Cam_%c", 'A'+c))
	}
	m.Reconcile(cams(names))
	for tick := 0; tick < 5; tick++ {
		before := cache.Stats().Listings
		m.Refresh()
		if n := cache.Stats().Listings - before; n != 6 {
			t.Errorf("tick %d: %d listings for 6 cameras, want 6", tick, n)
		}
		clk.Advance(5 * time.Second)
	}
	for _, name := range names {
		e := m.entries[name]
		if !e.hasScan || !e.scan.Recent || e.scan.NewestClosedWrite.IsZero() || e.scan.Files != 2 {
			t.Errorf("%s: scan %+v", name, e.scan)
		}
	}
	before := cache.Stats().Listings
	disk, err := sc.ScanRoot(root, clk.Now())
	if n := cache.Stats().Listings - before; err != nil || n != 6*31 || disk.Files != 6*(30*96+2) {
		t.Errorf("archive scan: %d listings (want %d), %d files, %v", n, 6*31, disk.Files, err)
	}
}
