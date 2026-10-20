package supervise

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
)

// startOne reconciles one camera, starts it and returns the running entry.
func startOne(t *testing.T, s Settings) (*rig, *Entry) {
	t.Helper()
	r := newRig(t, s)
	r.m.Reconcile(cams([]string{"cam1"}))
	r.startAll(r.m.Plan(RelayView{}))
	return r, r.m.Entry("cam1")
}

// tickUntilKill ticks Plan (and Refresh) once per second for at most limit and returns the
// seconds elapsed at the first Kill, or -1.
func (r *rig) tickUntilKill(limit time.Duration) (time.Duration, Action) {
	for el := time.Second; el <= limit; el += time.Second {
		r.clk.Advance(time.Second)
		r.m.Refresh()
		if k := kills(r.m.Plan(RelayView{})); len(k) > 0 {
			return el, k[0]
		}
	}
	return -1, Action{}
}

// ticks advances one second at a time, calling Plan each second (no blind gap); Starts are answered.
func (r *rig) ticks(d time.Duration) {
	for el := time.Duration(0); el < d; el += time.Second {
		r.clk.Advance(time.Second)
		r.startAll(r.m.Plan(RelayView{}))
	}
}

// IR 3.6 (R:1871-1914): stalled iff now - marker > SegmentSeconds x StallFactor (2250 s) strictly;
// with no files the marker is StartedAt. Not stalled at 2249 s or 2250 s, killed at 2251 s.
func TestStallFromStartedAt(t *testing.T) {
	r, e := startOne(t, defaultSettings())
	at, k := r.tickUntilKill(3000 * time.Second)
	if at != 2251*time.Second || k.Reason != ReasonStalled {
		t.Fatalf("killed at %v (%q), want 2251s", at, k.Reason)
	}
	if !strings.Contains(r.logs.String(), "watchdog cam1: stalled, no segment write for 2251s (limit 2250s) while the process was alive") {
		t.Fatal("watchdog line missing")
	}
	r.exit(e, -1)
	if e.restartCount != 1 || e.backoff != 3 || !strings.HasPrefix(e.lastError, "stalled, no segment write for 2251s") {
		t.Fatalf("stall accounting: count %d backoff %d LastError %q", e.restartCount, e.backoff, e.lastError)
	}
	if !strings.Contains(r.logs.String(), "watchdog restart #1 for cam1 in 3s") {
		t.Fatal("watchdog restart line missing")
	}
	// negative control: StallFactor 2.6 moves the kill
	s := defaultSettings()
	s.StallFactor = 2.6
	r2, _ := startOne(t, s)
	if at2, _ := r2.tickUntilKill(2400 * time.Second); at2 == 2251*time.Second {
		t.Fatal("negative control: StallFactor not observed")
	}
}

// The marker is the latest of StartedAt, the newest closed write and the open segment's first-seen
// time (R:3982-3993); the open segment counts only while it is recent, and it keeps a child whose
// closed segments were pruned alive (R:3984-3989).
func TestStallMarkers(t *testing.T) {
	cases := []struct {
		name string
		scan archive.DirScan
		at   time.Duration // kill time after start
	}{
		{"closed write 100 s after start", archive.DirScan{NewestClosedWrite: t0.Add(100 * time.Second)}, 2351 * time.Second},
		{"open segment not recent is not progress", archive.DirScan{Newest: "cam1_20261017_083001+0000.mkv"}, 2251 * time.Second},
	}
	for _, c := range cases {
		r, _ := startOne(t, defaultSettings())
		r.scan.scans[r.dir("cam1")] = c.scan
		r.ticks(500 * time.Second)
		r.m.Refresh() // first seen at +500 s
		at, _ := r.tickUntilKill(2600 * time.Second)
		if c.at > 0 {
			at += 500 * time.Second
		}
		if at != c.at {
			t.Errorf("%s: killed at %v, want %v", c.name, at, c.at)
		}
	}
	// the open segment's first-seen moves the deadline to 500 + 2251 s
	r, _ := startOne(t, defaultSettings())
	r.scan.scans[r.dir("cam1")] = archive.DirScan{Newest: "x.mkv", Recent: true}
	r.ticks(500 * time.Second)
	r.m.Refresh()
	if at, _ := r.tickUntilKill(3000 * time.Second); at != 2251*time.Second {
		t.Fatalf("first-seen marker: killed %v after it, want 2251s", at)
	}
}

// A young child is never stalled, however old the files are (StartedAt is part of the marker).
func TestStallYoungChild(t *testing.T) {
	r, _ := startOne(t, defaultSettings())
	r.scan.scans[r.dir("cam1")] = archive.DirScan{NewestClosedWrite: t0.Add(-3 * time.Hour)}
	if at, _ := r.tickUntilKill(2250 * time.Second); at != -1 {
		t.Fatalf("young child killed at %v", at)
	}
}

// LLD 2.1, IR 11 risk 7: a +3600 s wall step fires nothing: StartedAt is measured on the
// monotonic clock and the step is a blind interval for the file times. Negative control: without
// the blind rule the same step kills a healthy child.
func TestStallWallStep(t *testing.T) {
	for _, rule := range []bool{true, false} {
		s := defaultSettings()
		if !rule {
			s.BlindGapFactor = 1e9
		}
		r, _ := startOne(t, s)
		r.scan.scans[r.dir("cam1")] = archive.DirScan{NewestClosedWrite: t0.Add(2300 * time.Second)}
		r.m.Refresh()
		r.ticks(2400 * time.Second)
		r.m.Refresh()
		r.clk.StepWall(time.Hour)
		if k := kills(r.m.Plan(RelayView{})); (len(k) != 0) == rule {
			t.Fatalf("blind rule %v: kills %d", rule, len(k))
		}
	}
}

// Brief W7a: if the manager was not ticked for more than 3 x the tick, the gap is blind and the
// deadline moves by it. Negative control: without the rule the kill comes at once.
func TestStallBlindGap(t *testing.T) {
	for _, rule := range []bool{true, false} {
		s := defaultSettings()
		if !rule {
			s.BlindGapFactor = 1e9
		}
		r, _ := startOne(t, s)
		r.scan.scans[r.dir("cam1")] = archive.DirScan{NewestClosedWrite: t0.Add(10 * time.Second)}
		r.m.Refresh()
		r.ticks(100 * time.Second)
		r.clk.Advance(3000 * time.Second) // suspended: no tick for 3000 s
		r.m.Refresh()
		k := kills(r.m.Plan(RelayView{}))
		if rule && len(k) != 0 {
			t.Fatal("blind gap fired the watchdog")
		}
		if !rule && len(k) != 1 {
			t.Fatal("negative control: without the blind rule the watchdog must fire")
		}
		if rule {
			// progress at +10 s; ticked at +100; blind 3000 s; deadline = 10 + 2250 + 3000 + 1 => 5261
			at, _ := r.tickUntilKill(3000 * time.Second)
			if got := 3100*time.Second + at; got != 5261*time.Second {
				t.Fatalf("killed at +%v, want +5261s", got)
			}
		}
	}
}

// Check stallwatch (tests/cam.tests.ps1:5364-5475): SegmentSeconds 6 x StallFactor 2.5 = 15 s,
// status every 1 s. A healthy child rolling a segment every 6 s is never killed over 21 rolls; a
// suspended one is killed limit < t <= limit + 3.5 s after its last progress and is counted.
func TestStallWatchCheck(t *testing.T) {
	s := defaultSettings()
	s.SegmentSeconds, s.StallFactor = 6, 2.5
	r, e := startOne(t, s)
	dir := r.dir("cam1")
	var last time.Time
	roll := func(n int) {
		last = r.clk.Now()
		r.scan.scans[dir] = archive.DirScan{NewestClosedWrite: last, Newest: fmt.Sprintf("seg%03d.mkv", n), Recent: true}
	}
	maxAge := 0.0
	for n := 0; n < 21*6; n++ {
		if n%6 == 0 {
			roll(n)
		}
		r.clk.Advance(time.Second)
		r.m.Refresh()
		age, _, _ := r.m.stalled(e, r.clk.Now(), r.clk.Mono())
		maxAge = max(maxAge, age)
		if k := kills(r.m.Plan(RelayView{})); len(k) != 0 {
			t.Fatalf("healthy child killed at roll %d", n/6)
		}
	}
	if maxAge >= 15 {
		t.Fatalf("largest healthy age %.1f s not below the limit", maxAge)
	}
	// suspended: no more rolls
	at, k := r.tickUntilKill(30 * time.Second)
	sinceProgress := r.clk.Now().Sub(last)
	if k.Kind != ActKill || sinceProgress <= 15*time.Second || sinceProgress > 15*time.Second+3500*time.Millisecond {
		t.Fatalf("suspended child killed %v after progress (at %v)", sinceProgress, at)
	}
	r.exit(e, 1)
	if restartsLastHour(e, r.clk.Mono()) != 1 {
		t.Fatal("watchdog kill not counted in RestartsLastHour")
	}
}
