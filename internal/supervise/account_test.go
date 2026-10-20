package supervise

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"holocron/internal/ffmpeg"
)

// runOnce starts e (as if Plan had handed out a Start), lets it run for d and exits it with code.
func (r *rig) runOnce(e *Entry, d time.Duration, code int) Cleanup {
	e.pending, e.pendingInput = pendStart, InputRelay
	r.nextID++
	r.m.Started(e, r.nextID)
	r.clk.Advance(d)
	return r.m.Exited(e, ffmpeg.ExitInfo{Code: code})
}

func backoffSeq(t *testing.T, s Settings, runs []time.Duration) ([]int, *rig, *Entry) {
	t.Helper()
	r := newRig(t, s)
	r.m.Reconcile([]cameraT{cam("synth1", true)})
	e := r.m.Entry("synth1")
	var waits []int
	for _, d := range runs {
		r.runOnce(e, d, 1)
		waits = append(waits, e.backoff)
		if got := int(e.nextMono.Sub(r.clk.Mono()) / time.Second); got != e.backoff {
			t.Fatalf("NextStartAt is %d s ahead, backoff %d", got, e.backoff)
		}
	}
	return waits, r, e
}

// IR 3.5, IR 11 risk 5: 3, 6, 12, 24, 48, 60, 60 after quick failures (Set-CamChildBackoff R:2002-2029).
func TestBackoffSequenceDefaults(t *testing.T) {
	quick := []time.Duration{time.Second, time.Second, time.Second, time.Second, time.Second, time.Second, time.Second}
	waits, _, _ := backoffSeq(t, defaultSettings(), quick)
	if fmt.Sprint(waits) != "[3 6 12 24 48 60 60]" {
		t.Fatalf("waits = %v", waits)
	}
	// negative control: a cap of 50 changes the tail, so the test sees the constant
	s := defaultSettings()
	s.BackoffMax = 50
	if w, _, _ := backoffSeq(t, s, quick); fmt.Sprint(w) == "[3 6 12 24 48 60 60]" {
		t.Fatal("negative control: BackoffMax not observed")
	}
}

// IR 3.5: reset to the start only when the run lasted >= HealthyResetSeconds, at exit time.
func TestBackoffHealthyReset(t *testing.T) {
	cases := []struct {
		ran  time.Duration
		want int
	}{{300 * time.Second, 3}, {299 * time.Second, 48}, {300*time.Second + time.Millisecond, 3}, {299*time.Second + 999*time.Millisecond, 48}}
	for _, c := range cases {
		waits, _, _ := backoffSeq(t, defaultSettings(), []time.Duration{time.Second, time.Second, time.Second, time.Second, c.ran})
		if waits[4] != c.want {
			t.Errorf("after a %v run: wait %d, want %d (%v)", c.ran, waits[4], c.want, waits)
		}
	}
	// negative control: HealthyResetSeconds 301 makes the 300 s run not healthy
	s := defaultSettings()
	s.HealthyReset = 301
	if w, _, _ := backoffSeq(t, s, []time.Duration{time.Second, time.Second, time.Second, time.Second, 300 * time.Second}); w[4] == 3 {
		t.Fatal("negative control: HealthyReset not observed")
	}
}

// Check backoff (tests/cam.tests.ps1:4759-4839, scaled 1/8/15): five quick kills -> 1,2,4,8,8;
// a healthy 18 s run -> 1; the next quick one -> 2; RestartsLastHour 7; the log line shape the
// check parses ("for <name>; restart #<k> in <N>s").
func TestBackoffCheckScaled(t *testing.T) {
	s := defaultSettings()
	s.BackoffStart, s.BackoffMax, s.HealthyReset = 1, 8, 15
	q := 2 * time.Second
	waits, r, e := backoffSeq(t, s, []time.Duration{q, q, q, q, q, 18 * time.Second, q})
	if fmt.Sprint(waits) != "[1 2 4 8 8 1 2]" {
		t.Fatalf("waits = %v", waits)
	}
	if n := restartsLastHour(e, r.clk.Mono()); n != 7 || e.restartCount != 7 {
		t.Fatalf("RestartsLastHour %d total %d, want 7", n, e.restartCount)
	}
	re := regexp.MustCompile(`for synth1; restart #(\d+) in (\d+)s"`)
	got := re.FindAllStringSubmatch(r.logs.String(), -1)
	if len(got) != 7 || got[6][1] != "7" || got[6][2] != "2" {
		t.Fatalf("log lines %v", got)
	}
}

// IR 3.5: every exit counts, code 0 included; LastError is PowerShell's text with [int] rounding.
func TestExitAlwaysCounts(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile([]cameraT{cam("A", true)})
	e := r.m.Entry("A")
	c := r.runOnce(e, 5*time.Second+600*time.Millisecond, 0)
	if e.restartCount != 1 || e.lastError != "ffmpeg exited with code 0 after 6s" || e.lastErrorAt.IsZero() {
		t.Fatalf("count %d, LastError %q", e.restartCount, e.lastError)
	}
	if c.Dir != r.dir("A") || c.SafeName != "A" {
		t.Fatalf("cleanup %+v", c)
	}
	r.runOnce(e, 2500*time.Millisecond, -1)
	if e.lastError != "ffmpeg exited with code -1 after 2s" { // [int]2.5 = 2 (half to even)
		t.Fatalf("LastError %q", e.lastError)
	}
	if e.running || e.pid != 0 || e.lastPid == 0 {
		t.Fatal("pid fields after exit")
	}
}

// IR 3.4: a failed start backs off (ranFor 0) and is not a restart.
func TestStartFailedNotCounted(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile([]cameraT{cam("A", true)})
	e := r.m.Entry("A")
	for i, want := range []int{3, 6, 12} {
		e.pending, e.pendingInput = pendStart, InputDirect
		r.m.StartFailed(e, errors.New("ffmpeg not found"))
		if e.backoff != want || e.restartCount != 0 || len(e.restarts) != 0 {
			t.Fatalf("try %d: backoff %d count %d", i, e.backoff, e.restartCount)
		}
	}
	if e.lastError != "failed to start: ffmpeg not found" || e.input != InputDirect {
		t.Fatalf("LastError %q input %q", e.lastError, e.input)
	}
	if !strings.Contains(r.logs.String(), "failed to start A: ffmpeg not found; retry in 12s") {
		t.Fatal("log line missing")
	}
}

// IR 3.10, LLD 2.4: RestartsLastHour from a ring pruned on write; stamps older than 1 h drop out.
func TestRestartsRing(t *testing.T) {
	s := defaultSettings()
	s.RestartRingCap = 5
	r := newRig(t, s)
	r.m.Reconcile([]cameraT{cam("A", true)})
	e := r.m.Entry("A")
	for i := 0; i < 8; i++ {
		r.runOnce(e, time.Second, 1)
	}
	if len(e.restarts) != 5 || e.restartCount != 8 {
		t.Fatalf("ring %d total %d", len(e.restarts), e.restartCount)
	}
	r.clk.Advance(time.Hour)
	if n := restartsLastHour(e, r.clk.Mono()); n != 0 {
		t.Fatalf("after an hour: %d", n)
	}
	r.runOnce(e, time.Second, 1)
	if len(e.restarts) != 1 {
		t.Fatalf("ring not pruned on write: %d", len(e.restarts))
	}
}
