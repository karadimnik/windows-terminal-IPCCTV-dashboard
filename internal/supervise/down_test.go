package supervise

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
)

type downScan int

const (
	scanNone  downScan = iota // no scan ever (PowerShell $null)
	scanEmpty                 // empty directory
	scanFresh                 // NewestClosedWrite t0-1 s, open segment created (first seen) at t0
	scanOlder                 // progress 2 h before t0, open segment not recent
)

// downVerdict runs one row of check downalert: a manager started at `started`, the camera
// enabled then and never started, the scan taken at t0, Down called `after` seconds past t0.
func downVerdict(t *testing.T, s Settings, sc downScan, started time.Time, after float64) (bool, *rig) {
	t.Helper()
	r := newRigAt(t, s, started)
	r.m.Reconcile(cams([]string{"cam1"}))
	r.clk.Advance(t0.Sub(started))
	switch sc {
	case scanEmpty:
		r.scan.scans[r.dir("cam1")] = archive.DirScan{}
	case scanFresh:
		r.scan.scans[r.dir("cam1")] = archive.DirScan{NewestClosedWrite: t0.Add(-time.Second), Newest: "cam1_20261017_083001+0000.mkv", Recent: true}
	case scanOlder:
		r.scan.scans[r.dir("cam1")] = archive.DirScan{NewestClosedWrite: t0.Add(-2 * time.Hour), Newest: "old.mkv"}
	}
	if sc != scanNone {
		r.m.Refresh()
	}
	r.clk.Advance(time.Duration(after * float64(time.Second)))
	ev := r.m.Down()
	return len(ev) == 1 && ev[0].Alerted, r
}

// Check downalert (tests/cam.tests.ps1:5480-5556), IR 3.9: the ten literal rows.
func TestDownAlertRows(t *testing.T) {
	rows := []struct {
		label   string
		scan    downScan
		started time.Time
		after   float64
		want    bool
	}{
		{"defaults, stale scan at the boundary", scanFresh, t0.Add(-3 * time.Hour), 901, false},
		{"defaults, stale scan at the keyframe wait", scanFresh, t0.Add(-3 * time.Hour), 930, false},
		{"defaults, exactly the effective limit", scanFresh, t0.Add(-3 * time.Hour), 935, false},
		{"defaults, one second past the limit", scanFresh, t0.Add(-3 * time.Hour), 936, true},
		{"defaults, camera dark for 20 minutes", scanFresh, t0.Add(-3 * time.Hour), 1200, true},
		{"no progress ever, supervisor start is the floor", scanNone, t0, 901, false},
		{"no progress ever, past the limit", scanNone, t0, 936, true},
		{"empty directory scan, past the limit", scanEmpty, t0, 936, true},
		{"progress older than the supervisor start", scanOlder, t0, 901, false},
		{"progress older than the supervisor start, past limit", scanOlder, t0, 936, true},
	}
	for _, row := range rows {
		if got, _ := downVerdict(t, defaultSettings(), row.scan, row.started, row.after); got != row.want {
			t.Errorf("%s +%v s: alert %v, want %v", row.label, row.after, got, row.want)
		}
	}
	// negative control (the check's own): the pre-fix limit CameraDownAlertMinutes x 60 = 900 alerts at
	// +901 on the same scan; here the floor is made irrelevant with 60 s segments.
	s := defaultSettings()
	s.SegmentSeconds = 60
	if got, _ := downVerdict(t, s, scanFresh, t0.Add(-3*time.Hour), 901); !got {
		t.Fatal("negative control: a 900 s limit must alert at +901 s")
	}
}

// Check downalert's 12-row table of effective limits (literals), quiet at the limit, alert one
// second later; segment lengths 900, 1800 and 60.
func TestDownAlertLimitTable(t *testing.T) {
	table := []struct{ seg, min, status, want float64 }{
		{900, 5, 5, 935}, {900, 15, 5, 935}, {900, 60, 5, 3600},
		{1800, 5, 5, 1835}, {1800, 15, 5, 1835}, {1800, 60, 5, 3600},
		{60, 5, 5, 300}, {60, 15, 5, 900}, {60, 60, 5, 3600},
		{900, 15, 5, 935}, {900, 15, 5, 935}, {900, 15, 30, 960}, // ArchiveScanSeconds 300/5 play no part
	}
	for _, c := range table {
		s := defaultSettings()
		s.SegmentSeconds, s.DownAlertMinutes, s.StatusInterval = c.seg, c.min, c.status
		if got := DownAlertSeconds(s); got != c.want {
			t.Errorf("segment %v, %v min, status %v: limit %v, want %v", c.seg, c.min, c.status, got, c.want)
		}
		before, _ := downVerdict(t, s, scanFresh, t0.Add(-3*time.Hour), c.want-1)
		at, _ := downVerdict(t, s, scanFresh, t0.Add(-3*time.Hour), c.want)
		after, _ := downVerdict(t, s, scanFresh, t0.Add(-3*time.Hour), c.want+1)
		if before || at || !after {
			t.Errorf("segment %v, %v min: limit-1 %v, limit %v, limit+1 %v", c.seg, c.min, before, at, after)
		}
	}
}

// IR 3.9: one alert per episode, repeated every CameraDownRepeatHours (12) while down; recovery
// logs "is writing again after N down" with no alert, and a new episode alerts again.
func TestDownAlertRepeatAndRecovery(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile(cams([]string{"cam1"}))
	tick := func(d time.Duration) []DownEvent {
		var all []DownEvent
		for el := time.Duration(0); el < d; el += 5 * time.Second {
			r.clk.Advance(5 * time.Second)
			r.m.Refresh()
			all = append(all, r.m.Down()...)
		}
		return all
	}
	tick(935 * time.Second)
	if len(r.alerts.events) != 0 {
		t.Fatal("alert before the limit")
	}
	tick(5 * time.Second)
	if len(r.alerts.events) != 1 {
		t.Fatalf("alerts %d at +940 s", len(r.alerts.events))
	}
	ev := r.alerts.events[0]
	if ev.ID != 110 || ev.Severity != "Warning" || ev.Key != "110:cam1" || ev.Title != "a camera has stopped recording" ||
		ev.Message != "cam1 has written nothing for 16m. Last error: none recorded" {
		t.Fatalf("event %+v", ev)
	}
	if !r.m.Entry("cam1").downSince.Equal(t0) {
		t.Fatalf("DownSince %v, want the reference %v", r.m.Entry("cam1").downSince, t0)
	}
	tick(12*time.Hour - 10*time.Second)
	if len(r.alerts.events) != 1 {
		t.Fatalf("repeated early: %d", len(r.alerts.events))
	}
	tick(10 * time.Second)
	if len(r.alerts.events) != 2 || !strings.HasSuffix(r.alerts.events[1].Message, "for 12h. Last error: none recorded") {
		t.Fatalf("12 h repeat: %+v", r.alerts.events)
	}
	// writing again
	r.scan.scans[r.dir("cam1")] = archive.DirScan{NewestClosedWrite: r.clk.Now().Add(5 * time.Second)}
	evs := tick(5 * time.Second)
	if len(evs) != 1 || !evs[0].Recovered || len(r.alerts.events) != 2 {
		t.Fatalf("recovery %+v", evs)
	}
	if !strings.Contains(r.logs.String(), "cam1 is writing again after 12h down") {
		t.Fatal("recovery line missing")
	}
	if !r.m.Entry("cam1").downSince.IsZero() {
		t.Fatal("DownSince not cleared")
	}
	// negative control: CameraDownRepeatHours 13 suppresses the second alert at 12 h
	s := defaultSettings()
	s.DownRepeatHours = 13
	r2 := newRig(t, s)
	r2.m.Reconcile(cams([]string{"cam1"}))
	r2.clk.Advance(940 * time.Second)
	r2.m.Down()
	r2.clk.Advance(12 * time.Hour)
	r2.m.Down()
	if len(r2.alerts.events) != 1 {
		t.Fatal("negative control: the repeat constant is not observed")
	}
}

// LLD 2.4 / D19 (IR 3.9 latent V6): a camera enabled when the supervisor is older than the limit
// does not alert at once; its floor is its first start (its enable while never started). The
// PowerShell reference (supervisor start only) is computed beside it and does alert: this is the
// conformance difference.
func TestDownAlertLateEnableD19(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.clk.Advance(2 * time.Hour)
	r.m.Reconcile(cams([]string{"late"}))
	e := r.m.Entry("late")
	r.clk.Advance(20 * time.Second)
	e.pending, e.pendingInput = pendStart, InputDirect
	r.m.Started(e, 42) // first start at supervisor + 2 h 20 s
	r.clk.Advance(5 * time.Second)
	r.m.Refresh()
	psReference := r.m.startedWall // Measure-CamCameraDown: max(SupervisorStartedAt, progress) with no progress
	if psSecs := r.clk.Now().Sub(psReference).Seconds(); !(psSecs > DownAlertSeconds(r.s)) {
		t.Fatal("the PowerShell rule should alert here")
	}
	if ev := r.m.Down(); len(ev) != 0 || len(r.alerts.events) != 0 {
		t.Fatalf("late-enabled camera alerted at once: %+v", ev)
	}
	r.clk.Advance(930 * time.Second) // first start + 935 s
	r.m.Down()
	if len(r.alerts.events) != 0 {
		t.Fatal("alerted at the limit")
	}
	r.clk.Advance(time.Second)
	r.m.Down()
	if len(r.alerts.events) != 1 {
		t.Fatal("no alert one second past the limit from the first start")
	}
	if strings.Contains(r.logs.String(), "is writing again") {
		t.Fatal("PowerShell's alert-then-recover pair must not happen")
	}
}

// Check downalertrun (tests/cam.tests.ps1:5560-5654): SegmentSeconds 20, StatusIntervalSeconds 1,
// CameraDownAlertMinutes 20/60 -> limit 51 s. A healthy camera over three rolls raises nothing; a
// dark one raises exactly one alert within [limit, limit + 8] s after its last progress.
func TestDownAlertRunCheck(t *testing.T) {
	s := defaultSettings()
	s.SegmentSeconds, s.StatusInterval, s.DownAlertMinutes = 20, 1, 20.0/60
	if DownAlertSeconds(s) != 51 {
		t.Fatalf("limit %v", DownAlertSeconds(s))
	}
	r, _ := startOne(t, s)
	dir := r.dir("cam1")
	var last time.Time
	for n := 0; n < 3*20; n++ {
		if n%20 == 0 {
			last = r.clk.Now()
			r.scan.scans[dir] = archive.DirScan{NewestClosedWrite: last, Newest: fmt.Sprintf("s%d.mkv", n), Recent: true}
		}
		r.clk.Advance(time.Second)
		r.m.Refresh()
		r.m.Down()
	}
	if len(r.alerts.events) != 0 {
		t.Fatal("healthy camera alerted")
	}
	r.scan.scans[dir] = archive.DirScan{NewestClosedWrite: last, Newest: "s40.mkv"} // dark: no recent file
	var alertAt time.Duration
	for i := 0; i < 120; i++ {
		r.clk.Advance(time.Second)
		r.m.Refresh()
		if ev := r.m.Down(); len(ev) > 0 && ev[0].Alerted && alertAt == 0 {
			alertAt = r.clk.Now().Sub(last)
		}
	}
	if len(r.alerts.events) != 1 || alertAt < 51*time.Second || alertAt > 59*time.Second {
		t.Fatalf("alerts %d, first %v after the last progress", len(r.alerts.events), alertAt)
	}
}

// Format-CamAgeText (cam.common.ps1:75-84) with [int] rounding.
func TestFormatAge(t *testing.T) {
	for in, want := range map[float64]string{0: "0s", 119.4: "119s", 119.6: "120s", 120: "2m", 935: "16m", 7199: "120m", 7200: "2h", 43200: "12h", 2.5: "2s"} {
		if got := FormatAge(in); got != want {
			t.Errorf("FormatAge(%v) = %q, want %q", in, got, want)
		}
	}
}
