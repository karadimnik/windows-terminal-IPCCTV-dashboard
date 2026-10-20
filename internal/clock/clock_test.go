package clock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func zulu(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02T15:04:05.0000000Z", s)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return v
}

// TestDSTTable ports testdata/golden/time/dst.json (generated from the PowerShell product in GTB
// Standard Time) row by row: format text, read-back, offset, %z suffix, log stamp, interval and
// heartbeat figures.
func TestDSTTable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "time", "dst.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Zone        struct{ Windows, IANA string }
		Transitions []struct {
			Kind, ChangeUTC string
			DeltaSeconds    int
			Instants        []struct {
				SecondsFromChange int
				UTC, Formatted    string
				ParsedBackUTC     string
				LocalWall         string
				UTCOffsetSeconds  int
				SegmentSuffix     string
				LogStamp          string
			}
			Interval struct {
				FromUTC                  string `json:"fromUtc"`
				ToUTC                    string `json:"toUtc"`
				UTCElapsedSeconds        float64
				LocalNaiveElapsedSeconds float64
				DueUTC                   bool `json:"dueAfter5sUsingUtc"`
				DueLocal                 bool `json:"dueAfter5sUsingLocalNaive"`
				FormattedFrom            string
				FormattedTo              string
				FormattedElapsedSeconds  float64
				HeartbeatWrittenAtUTC    string `json:"heartbeatWrittenAtUtc"`
				HeartbeatReadAtUTC       string `json:"heartbeatReadAtUtc"`
				HeartbeatUpdatedAt       string
				HeartbeatAgeSeconds      float64
				HeartbeatLimitSeconds    float64
				HeartbeatFresh           bool
			}
		}
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	loc, err := Location(d.Zone.Windows) // the PowerShell zone name, through the alias
	if err != nil {
		t.Fatal(err)
	}
	if iana, ok := WindowsZoneToIANA(d.Zone.Windows); !ok || iana != d.Zone.IANA {
		t.Fatalf("alias %q -> %q, golden says %q", d.Zone.Windows, iana, d.Zone.IANA)
	}
	if len(d.Transitions) != 2 {
		t.Fatalf("%d transitions", len(d.Transitions))
	}
	rows := 0
	for _, tr := range d.Transitions {
		change := zulu(t, tr.ChangeUTC)
		for _, i := range tr.Instants {
			rows++
			utc := zulu(t, i.UTC)
			if !utc.Equal(change.Add(time.Duration(i.SecondsFromChange) * time.Second)) {
				t.Errorf("%s %d: golden utc column inconsistent", tr.Kind, i.SecondsFromChange)
			}
			local := utc.In(loc)
			if got := FormatTimestamp(local); got != i.Formatted {
				t.Errorf("%s %d: FormatTimestamp = %s, want %s", tr.Kind, i.SecondsFromChange, got, i.Formatted)
			}
			if FormatTimestamp(utc) == i.Formatted { // negative control: the wrong zone must not match
				t.Errorf("%s %d: formatting in UTC matched the golden; the table cannot tell zones apart", tr.Kind, i.SecondsFromChange)
			}
			back, err := ParseTimestamp(i.Formatted)
			if err != nil || !back.Equal(utc) || !back.Equal(zulu(t, i.ParsedBackUTC)) || back.Location() != time.UTC {
				t.Errorf("%s %d: ParseTimestamp(%s) = %v, %v; want %s in UTC", tr.Kind, i.SecondsFromChange, i.Formatted, back, err, i.UTC)
			}
			_, off := local.Zone()
			if off != i.UTCOffsetSeconds {
				t.Errorf("%s %d: offset %d, want %d", tr.Kind, i.SecondsFromChange, off, i.UTCOffsetSeconds)
			}
			if got := local.Format("2006-01-02 15:04:05"); got != i.LocalWall {
				t.Errorf("%s %d: wall %s, want %s", tr.Kind, i.SecondsFromChange, got, i.LocalWall)
			}
			if got := local.Format("20060102_150405-0700"); got != i.SegmentSuffix {
				t.Errorf("%s %d: %%z suffix %s, want %s", tr.Kind, i.SecondsFromChange, got, i.SegmentSuffix)
			}
			if got := FormatLogStamp(local); got != i.LogStamp {
				t.Errorf("%s %d: log stamp %s, want %s", tr.Kind, i.SecondsFromChange, got, i.LogStamp)
			}
		}

		iv := tr.Interval
		from, to := zulu(t, iv.FromUTC), zulu(t, iv.ToUTC)
		// The monotonic clock measures the real 600 s however the wall reads.
		fc := NewFake(from.In(loc))
		start := fc.Mono()
		fc.Advance(to.Sub(from))
		if got := fc.Since(start).Seconds(); got != iv.UTCElapsedSeconds || got != 600 {
			t.Errorf("%s: monotonic interval %v, golden %v", tr.Kind, got, iv.UTCElapsedSeconds)
		}
		if !Due(fc, start, 5*time.Second) || !iv.DueUTC {
			t.Errorf("%s: due after 5 s must be true", tr.Kind)
		}
		// The defect of local-time arithmetic (what PowerShell avoided by using UTC): reproduced as a control.
		wall := func(s string) time.Time {
			w, _ := time.Parse("2006-01-02 15:04:05", zulu(t, s).In(loc).Format("2006-01-02 15:04:05"))
			return w
		}
		if naive := wall(iv.ToUTC).Sub(wall(iv.FromUTC)).Seconds(); naive != iv.LocalNaiveElapsedSeconds || naive-600 != float64(tr.DeltaSeconds) {
			t.Errorf("%s: naive local elapsed %v, golden %v (delta %d)", tr.Kind, naive, iv.LocalNaiveElapsedSeconds, tr.DeltaSeconds)
		}
		if (tr.Kind == "FallBack") == iv.DueLocal {
			t.Errorf("%s: local-naive due flag %v unexpected", tr.Kind, iv.DueLocal)
		}
		// The formatted pair read back differs by the true 600 s.
		a, errA := ParseTimestamp(iv.FormattedFrom)
		b, errB := ParseTimestamp(iv.FormattedTo)
		if errA != nil || errB != nil || b.Sub(a).Seconds() != iv.FormattedElapsedSeconds || iv.FormattedElapsedSeconds != 600 {
			t.Errorf("%s: formatted pair elapsed %v (%v %v)", tr.Kind, b.Sub(a).Seconds(), errA, errB)
		}
		// Heartbeat age: stored text read back as an instant, aged against the read instant.
		updated, err := ParseTimestamp(iv.HeartbeatUpdatedAt)
		if err != nil || !updated.Equal(zulu(t, iv.HeartbeatWrittenAtUTC)) {
			t.Errorf("%s: heartbeat text %s does not read back as %s", tr.Kind, iv.HeartbeatUpdatedAt, iv.HeartbeatWrittenAtUTC)
		}
		age := zulu(t, iv.HeartbeatReadAtUTC).Sub(updated).Seconds()
		if age != iv.HeartbeatAgeSeconds || age != 8 || !iv.HeartbeatFresh || age > iv.HeartbeatLimitSeconds {
			t.Errorf("%s: heartbeat age %v, golden %v, fresh %v limit %v", tr.Kind, age, iv.HeartbeatAgeSeconds, iv.HeartbeatFresh, iv.HeartbeatLimitSeconds)
		}
	}
	if rows != 22 {
		t.Errorf("%d instant rows, want 22", rows)
	}
}

func TestWindowsZoneAlias(t *testing.T) {
	loc, err := Location("GTB Standard Time")
	if err != nil || loc.String() != "Europe/Bucharest" {
		t.Fatalf("GTB Standard Time -> %v, %v", loc, err)
	}
	if again, _ := Location("Europe/Bucharest"); again.String() != loc.String() {
		t.Error("IANA name resolves differently from its Windows alias")
	}
	if l, err := Location(""); err != nil || l != time.Local {
		t.Errorf("empty name = %v, %v", l, err)
	}
	if _, ok := WindowsZoneToIANA("No Such Standard Time"); ok {
		t.Error("unknown Windows zone must not resolve")
	}
	if _, err := Location("No Such Standard Time"); err == nil { // negative control
		t.Error("unknown zone must be an error")
	}
	// the Bucharest rules are really loaded: summer +03:00, winter +02:00
	if _, off := time.Date(2026, 7, 1, 12, 0, 0, 0, loc).Zone(); off != 3*3600 {
		t.Errorf("summer offset %d", off)
	}
	if _, off := time.Date(2026, 12, 1, 12, 0, 0, 0, loc).Zone(); off != 2*3600 {
		t.Errorf("winter offset %d", off)
	}
}

func TestTimestampShape(t *testing.T) {
	bucharest, _ := Location("Europe/Bucharest")
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"seven digits, trailing zeros kept", time.Date(2026, 10, 24, 3, 30, 0, 123456700, bucharest), "2026-10-24T03:30:00.1234567+03:00"},
		{"zero fraction still has 7 digits", time.Date(2026, 10, 24, 3, 30, 0, 0, bucharest), "2026-10-24T03:30:00.0000000+03:00"},
		{"nanoseconds truncated, not rounded", time.Date(2026, 1, 2, 3, 4, 5, 999999999, time.UTC), "2026-01-02T03:04:05.9999999+00:00"},
		{"UTC is +00:00, never Z", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05.0000000+00:00"},
	}
	for _, c := range cases {
		if got := FormatTimestamp(c.t); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	for _, ok := range []string{"2026-10-25T03:30:00.1234567+03:00", "2026-10-25T00:30:00Z", "2026-10-25T00:30:00.5+00:00"} {
		if _, err := ParseTimestamp(ok); err != nil {
			t.Errorf("ParseTimestamp(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "null", "2026-10-25 03:30:00", "2026-10-25T03:30:00", "yesterday"} { // negative controls
		if _, err := ParseTimestamp(bad); err == nil {
			t.Errorf("ParseTimestamp(%q) should fail", bad)
		}
	}
}

// A +/- 3600 s step of the wall clock changes Now but not a monotonic interval, a due check or a sleeper.
// (The PowerShell product measures intervals on the UTC wall clock and does not absorb this step.)
func TestWallStepDoesNotChangeMonotonicInterval(t *testing.T) {
	start := time.Date(2026, 10, 25, 0, 50, 0, 0, time.UTC)
	for _, step := range []time.Duration{time.Hour, -time.Hour} {
		fc := NewFake(start)
		m0, w0 := fc.Mono(), fc.Now()
		fc.Advance(10 * time.Second)
		fc.StepWall(step)
		fc.Advance(20 * time.Second)

		if got := fc.Since(m0); got != 30*time.Second {
			t.Errorf("step %v: monotonic interval %v, want 30s", step, got)
		}
		if wall := fc.Now().Sub(w0); wall != 30*time.Second+step { // control: the wall interval IS wrong
			t.Errorf("step %v: wall interval %v, want %v", step, wall, 30*time.Second+step)
		}
		if Due(fc, m0, 45*time.Second) {
			t.Errorf("step %v: 30 s of real time must not be due at 45 s", step)
		}
		if !Due(fc, m0, 30*time.Second) {
			t.Errorf("step %v: due at exactly 30 s", step)
		}
	}
}

func TestFakeSleepAndAfter(t *testing.T) {
	fc := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := fc.After(10 * time.Second)
	b := fc.After(5 * time.Second)
	if fc.Sleepers() != 2 {
		t.Fatal("two sleepers expected")
	}
	fc.Advance(4 * time.Second)
	select {
	case <-a:
		t.Fatal("10 s timer fired at 4 s")
	case <-b:
		t.Fatal("5 s timer fired at 4 s")
	default:
	}
	fc.StepWall(time.Hour) // does not release or delay anyone
	fc.Advance(time.Second)
	select {
	case <-b:
	default:
		t.Fatal("5 s timer did not fire at 5 s of monotonic time")
	}
	select {
	case <-a:
		t.Fatal("10 s timer fired early")
	default:
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); fc.Sleep(5 * time.Second) }()
	for fc.Sleepers() < 2 { // the 10 s timer and the sleeper
		time.Sleep(time.Millisecond)
	}
	fc.Advance(5 * time.Second)
	wg.Wait()
	<-a
	if fc.Sleepers() != 0 {
		t.Error("sleepers left")
	}
	select {
	case <-fc.After(0):
	default:
		t.Error("After(0) must fire at once")
	}
}

func TestRealClock(t *testing.T) {
	c := Real()
	n := c.Now()
	if n.Location() != time.UTC || time.Since(n) > time.Minute || time.Since(n) < -time.Minute {
		t.Errorf("Now = %v", n)
	}
	m := c.Mono()
	c.Sleep(20 * time.Millisecond)
	if d := c.Since(m); d < 15*time.Millisecond || d > 5*time.Second {
		t.Errorf("Since after a 20 ms sleep = %v", d)
	}
	select {
	case <-c.After(10 * time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Error("After did not fire")
	}
	var _ Clock = c
	var _ Clock = NewFake(n)
}
