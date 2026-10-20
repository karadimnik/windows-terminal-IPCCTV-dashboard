package status

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestVerdictTable(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	started := now.Add(-2 * time.Hour)
	const pid = 4242
	live := FakeProcesses{pid: started.Add(2 * time.Second)} // process created just before StartedAt was recorded
	mk := func(updatedAgo time.Duration) *Document {
		return &Document{Schema: 1, State: StateRunning, SupervisorPid: pid,
			StartedAt: NewTime(started), UpdatedAt: NewTime(now.Add(-updatedAgo))}
	}
	stopped := func(kind string) *Document {
		d := mk(time.Second)
		d.State, d.StopKind = StateStopped, kind
		return d
	}
	noBeat := mk(0)
	noBeat.UpdatedAt = Time{}
	noStart := mk(time.Second)
	noStart.StartedAt = Time{}

	cases := []struct {
		name  string
		doc   *Document
		now   time.Time
		procs ProcessInspector
		cfg   VerdictConfig
		run   bool
		want  string // substring of the reason
	}{
		{"no status file", nil, now, live, VerdictConfig{}, false, "no status file has been written yet"},
		{"clean shutdown", stopped("requested"), now, live, VerdictConfig{}, false, "recorded a clean shutdown at"},
		{"stopped without kind", stopped(""), now, live, VerdictConfig{}, false, "recorded a clean shutdown at"},
		{"stopped unexpected", stopped("unexpected"), now, live, VerdictConfig{}, false, "stopped (unexpected)"},
		{"stopped window-closed", stopped("window-closed"), now, live, VerdictConfig{}, false, "stopped (window-closed)"},
		{"unusable heartbeat", noBeat, now, live, VerdictConfig{}, false, "no usable heartbeat"},
		{"fresh and alive", mk(3 * time.Second), now, live, VerdictConfig{}, true, "pid 4242, heartbeat 3.0s old"},
		{"at the limit (15 s)", mk(15 * time.Second), now, live, VerdictConfig{}, true, "pid 4242"},
		{"just over the limit", mk(15*time.Second + 200*time.Millisecond), now, live, VerdictConfig{}, false, "heartbeat is 15.2s old (limit 15s)"},
		{"limit follows the interval", mk(25 * time.Second), now, live, VerdictConfig{StatusIntervalSeconds: 10}, true, "pid 4242"},
		{"limit follows the interval, stale", mk(31 * time.Second), now, live, VerdictConfig{StatusIntervalSeconds: 10}, false, "limit 30s"},
		{"short interval keeps the 15 s floor", mk(14 * time.Second), now, live, VerdictConfig{StatusIntervalSeconds: 1}, true, "pid 4242"},
		{"pid not alive", mk(time.Second), now, FakeProcesses{}, VerdictConfig{}, false, "process 4242 is no longer running"},
		{"pid zero", func() *Document { d := mk(time.Second); d.SupervisorPid = 0; return d }(), now, live, VerdictConfig{}, false, "process 0 is no longer running"},
		{"pid reused", mk(time.Second), now, FakeProcesses{pid: now.Add(-time.Minute)}, VerdictConfig{}, false, "pid reused"},
		{"start within tolerance", mk(time.Second), now, FakeProcesses{pid: started.Add(-25 * time.Second)}, VerdictConfig{}, true, "pid 4242"},
		{"tolerance configurable", mk(time.Second), now, FakeProcesses{pid: started.Add(-25 * time.Second)}, VerdictConfig{StartTolerance: 5 * time.Second}, false, "pid reused"},
		{"tolerance disabled", mk(time.Second), now, FakeProcesses{pid: now.Add(-time.Minute)}, VerdictConfig{StartTolerance: -1}, true, "pid 4242"},
		{"unknown start time is not a mismatch", mk(time.Second), now, FakeProcesses{pid: {}}, VerdictConfig{}, true, "pid 4242"},
		{"no StartedAt recorded", noStart, now, FakeProcesses{pid: now}, VerdictConfig{}, true, "pid 4242"},
		{"small future heartbeat (skew)", mk(-4 * time.Second), now, live, VerdictConfig{}, true, "pid 4242"},
		{"far future heartbeat", mk(-time.Minute), now, live, VerdictConfig{}, false, "in the future"},
		{"clock stepped forward 3600 s", mk(time.Second), now.Add(time.Hour), live, VerdictConfig{}, false, "heartbeat is 3601.0s old"},
		{"clock stepped back 3600 s", mk(time.Second), now.Add(-time.Hour), live, VerdictConfig{}, false, "in the future"},
		{"stale beat is judged before the pid", mk(time.Hour), now, FakeProcesses{}, VerdictConfig{}, false, "heartbeat is 3600.0s old"},
	}
	for _, c := range cases {
		run, reason := Verdict(c.doc, c.now, c.procs, c.cfg)
		if run != c.run || !strings.Contains(reason, c.want) {
			t.Errorf("%s: got (%v, %q), want (%v, ...%q...)", c.name, run, reason, c.run, c.want)
		}
	}
}

func TestVerdictOnFixtures(t *testing.T) {
	doc := fixture(t, "running-live.json")
	now := doc.UpdatedAt.Time().Add(2 * time.Second)
	procs := FakeProcesses{doc.SupervisorPid: doc.StartedAt.Time().Add(time.Second)}
	if run, reason := Verdict(doc, now, procs, VerdictConfig{}); !run {
		t.Errorf("fresh fixture: %s", reason)
	}
	if run, _ := Verdict(doc, now.Add(time.Minute), procs, VerdictConfig{}); run {
		t.Error("stale fixture reported running")
	}
	if run, reason := Verdict(fixture(t, "stopped-requested.json"), now, procs, VerdictConfig{}); run || !strings.Contains(reason, "clean shutdown") {
		t.Errorf("stopped fixture: %v %s", run, reason)
	}
}

func TestOSProcessesSeesThisProcess(t *testing.T) {
	var p OSProcesses
	start, ok := p.Alive(int64(os.Getpid()))
	if !ok {
		t.Fatal("own pid not alive")
	}
	if !start.IsZero() {
		if age := time.Since(start); age < -2*time.Second || age > time.Hour {
			t.Errorf("implausible start time %v (age %v)", start, age)
		}
	}
	if _, ok := p.Alive(0); ok {
		t.Error("pid 0 alive")
	}
	if _, ok := p.Alive(2147483000); ok {
		t.Error("absurd pid alive")
	}
	doc := &Document{State: StateRunning, SupervisorPid: int64(os.Getpid()), StartedAt: NewTime(start), UpdatedAt: NewTime(time.Now())}
	if run, reason := Verdict(doc, time.Now(), p, VerdictConfig{}); !run {
		t.Errorf("own process: %s", reason)
	}
}
