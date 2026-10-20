package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/prune"
	"holocron/internal/status"
)

// keySet lists the object keys of a status document, nested objects and the first element of each
// array of objects as "Parent.Key" (the shape the readers depend on).
func keySet(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			out = append(out, prefix+k)
			switch x := v.(type) {
			case map[string]any:
				walk(prefix+k+".", x)
			case []any:
				if len(x) > 0 {
					if o, ok := x[0].(map[string]any); ok {
						walk(prefix+k+"[].", o)
					}
				}
			}
		}
	}
	walk("", doc)
	sort.Strings(out)
	return out
}

func goldenKeys(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "status", name))
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.TrimPrefix(string(b), "\ufeff"))
	return keySet(t, b)
}

func sameKeys(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s key set differs\n got  %v\n want %v", what, got, want)
	}
}

// The ordered stop: every camera first (hard kill), then the live view, then the relay; the final
// document says stopped/requested with nothing alive; nothing of ours is left; the lock is released.
func TestSupervisorOrderedStop(t *testing.T) {
	r := newSRig(t, sixCameras(), func(c *config.Config) { c.LiveViewEnabled = true })
	r.start()
	r.step(8 * time.Second)
	if len(r.st.started("ffmpeg")) != 6 || len(r.st.started("live")) != 1 || len(r.st.started("relay")) != 1 {
		t.Fatalf("not everything started: %d %d %d\n%s", len(r.st.started("ffmpeg")), len(r.st.started("live")), len(r.st.started("relay")), r.logText())
	}
	// the live section is there and has the golden shape; the running document too
	running := r.raw[len(r.raw)-1]
	sameKeys(t, "running with live view", keySet(t, running), goldenKeys(t, "running-live.json"))

	res := r.stop(StopRequested)
	if res.err != nil || res.rep.Kind != StopRequested || len(res.rep.Survivors) != 0 || !res.rep.Final {
		t.Fatalf("%+v", res)
	}
	order := r.st.killOrder()
	if len(order) != 8 {
		t.Fatalf("%d kills, want 8", len(order))
	}
	for i, p := range order {
		want := "ffmpeg"
		if i == 6 {
			want = "live"
		} else if i == 7 {
			want = "relay"
		}
		if p.kind != want {
			t.Errorf("kill %d is %s %s, want %s", i, p.kind, p.name, want)
		}
	}
	if left := r.st.alive(); len(left) != 0 {
		t.Errorf("alive after the stop: %v", left)
	}
	final := r.lastDoc()
	if final.State != status.StateStopped || final.StopKind != status.StopKindRequested || final.Live == nil || final.Live.Alive || final.Live.Pid != nil ||
		final.Restreamer.Alive || final.Restreamer.Pid != nil || final.Restreamer.LastPid == nil || final.LastUnexpectedStop.Stop != nil {
		t.Errorf("final document: %+v live %+v relay %+v", final, final.Live, final.Restreamer)
	}
	for _, row := range final.Cameras {
		if row.Alive || row.Pid != nil || row.LastPid == nil || row.RestartsTotal != 0 {
			t.Errorf("final row %+v", row)
		}
	}
	if r.lock.Held() {
		t.Error("the lock is still held")
	}
	log := r.logText()
	for _, line := range []string{"supervisor shutting down: stop requested", "kill live view pid", "kill restreamer pid", "supervisor stopped (requested)"} {
		if !strings.Contains(log, line) {
			t.Errorf("log lacks %q", line)
		}
	}
	r.validateAll()
}

// With live view off the status key set is exactly the PowerShell master's.
func TestSupervisorLiveOffKeySetEqualsMaster(t *testing.T) {
	r := newSRig(t, sixCameras(), nil)
	r.start()
	r.step(62 * time.Second) // past the first 60 s archive scan
	r.mu.Lock()
	running := r.raw[len(r.raw)-1]
	r.mu.Unlock()
	r.stop(StopRequested)
	r.mu.Lock()
	stopped := r.raw[len(r.raw)-1]
	r.mu.Unlock()
	// the golden documents carry a non-null LastUnexpectedStop object only in the unexpected case
	sameKeys(t, "running", keySet(t, running), goldenKeys(t, "running-full.json"))
	sameKeys(t, "stopped", keySet(t, stopped), goldenKeys(t, "stopped-requested.json"))
	r.validateAll()
}

// A child that ignores the kill is reported as a survivor, not hidden; the live view and the relay
// are still stopped after the camera phase's wait; the lock is released.
func TestSupervisorReportsSurvivor(t *testing.T) {
	r := newSRig(t, oneCamera("Stubborn"), func(c *config.Config) { c.LiveViewEnabled = true })
	r.st.script = func(kind, _ string, _ int) behaviour { return behaviour{ignoreKill: kind == "ffmpeg"} }
	r.start()
	r.step(5 * time.Second)
	cam := r.st.started("ffmpeg")
	if len(cam) != 1 {
		t.Fatalf("%d starts", len(cam))
	}
	res := r.stop(StopRequested)
	if len(res.rep.Survivors) != 1 || res.rep.Survivors[0] != cam[0].pid {
		t.Fatalf("survivors %v, want [%d]", res.rep.Survivors, cam[0].pid)
	}
	order := r.st.killOrder()
	if len(order) != 3 || order[0] != cam[0] || order[1].kind != "live" || order[2].kind != "relay" {
		t.Errorf("kill order %v", order)
	}
	if !strings.Contains(r.logText(), fmt.Sprintf("DID NOT STOP: 1 child process(es) are still alive after the stop wait: pids [%d]", cam[0].pid)) {
		t.Errorf("no survivor line:\n%s", r.logText())
	}
	final := r.lastDoc()
	if final.State != "stopped" || final.StopKind != "requested" {
		t.Errorf("final %s/%s", final.State, final.StopKind)
	}
	if r.lock.Held() {
		t.Error("lock held")
	}
	cam[0].finish(0) // the test's own cleanup
	r.validateAll()
}

// A panic anywhere is an unexpected stop: LastUnexpectedStop{At, Reason, SupervisorPid,
// DetectedBy:"supervisor"}, the ordered stop still runs, Run returns ErrUnexpectedStop (exit 1).
func TestSupervisorPanicIsUnexpected(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		setup        func(r *srig)
	}{
		{"loop", "fake scanner exploded", func(r *srig) { r.sc.panicAt = 3 }},
		{"alert sink on the pruner goroutine", "alert channel exploded", func(r *srig) {
			r.al.panics = true
			r.pr.result = nil
			r.deps.Pruner = panicPruner{}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newSRig(t, sixCameras(), nil)
			c.setup(r)
			r.start()
			res := r.finish()
			if !errors.Is(res.err, ErrUnexpectedStop) || ExitCode(res.err) != ExitFailure || res.rep.Kind != StopUnexpected {
				t.Fatalf("%+v", res)
			}
			final := r.lastDoc()
			u := final.LastUnexpectedStop.Stop
			if final.State != "stopped" || final.StopKind != "unexpected" || u == nil || u.DetectedBy != "supervisor" || u.SupervisorPid != 4242 ||
				!strings.Contains(u.Reason, c.reason) || u.At.IsNull() {
				t.Fatalf("final %+v unexpected %+v", final, u)
			}
			if len(r.st.alive()) != 0 {
				t.Errorf("children alive after the unexpected stop: %v", r.st.alive())
			}
			if r.lock.Held() {
				t.Error("lock held")
			}
			r.mu.Lock()
			sameKeys(t, "stopped unexpected", keySet(t, r.raw[len(r.raw)-1]), goldenKeys(t, "stopped-unexpected-by-supervisor.json"))
			r.mu.Unlock()
			r.validateAll()
		})
	}
}

type panicPruner struct{}

func (panicPruner) Claim(time.Time) (archive.ClaimResult, error) {
	return archive.ClaimResult{Owned: true}, nil
}
func (panicPruner) Run(time.Time, *prune.State) (prune.Result, error) {
	return prune.Result{Alerts: []prune.Alert{{ID: 121, Level: "Error", Title: "t", Body: "b"}}}, nil
}

// The lock: a second supervisor is refused with exit 20 naming the owner; the lock is released on
// every exit path after it was taken (requested, unexpected, refused by the heartbeat signal); a
// configuration fault (exit 21) never takes it.
func TestSupervisorLockPaths(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		r := newSRig(t, oneCamera("Front"), nil)
		release, _, _ := r.lock.Acquire()
		defer release()
		r.build()
		_, err := r.sup.Run(context.Background())
		if !errors.Is(err, ErrAnotherSupervisor) || ExitCode(err) != ExitAnotherSupervisor || !strings.Contains(err.Error(), "test-supervisor") {
			t.Fatalf("%v", err)
		}
		if len(r.st.started("")) != 0 {
			t.Error("children started")
		}
	})
	t.Run("live heartbeat of another pid", func(t *testing.T) {
		r := newSRig(t, oneCamera("Front"), nil)
		prev := &status.Document{Schema: 1, State: "running", SupervisorPid: 777, UpdatedAt: status.NewTime(tStart.Add(-2 * time.Second)), StartedAt: status.NewTime(tStart.Add(-time.Hour))}
		r.deps.ReadPrevious = func() (*status.Document, error) { return prev, nil }
		r.deps.Processes = status.FakeProcesses{777: tStart.Add(-time.Hour)}
		r.build()
		_, err := r.sup.Run(context.Background())
		if !errors.Is(err, ErrAnotherSupervisor) || !strings.Contains(err.Error(), "pid 777") {
			t.Fatalf("%v", err)
		}
		if a, rel := r.lock.Counts(); a != 1 || rel != 1 {
			t.Errorf("lock acquired %d released %d", a, rel)
		}
	})
	t.Run("config fault", func(t *testing.T) {
		r := newSRig(t, oneCamera("Front"), func(c *config.Config) { c.Container = "avi" })
		r.build()
		_, err := r.sup.Run(context.Background())
		var cf *ConfigFault
		if !errors.As(err, &cf) || ExitCode(err) != ExitConfigFault {
			t.Fatalf("%v", err)
		}
		if a, _ := r.lock.Counts(); a != 0 {
			t.Error("lock taken on a configuration fault")
		}
	})
	for _, kind := range []string{StopRequested, StopInterrupted, StopUnexpected} {
		t.Run("stop "+kind, func(t *testing.T) {
			r := newSRig(t, oneCamera("Front"), nil)
			r.start()
			r.step(2 * time.Second)
			res := r.stop(kind)
			if res.rep.Kind != kind || (kind == StopUnexpected) != errors.Is(res.err, ErrUnexpectedStop) {
				t.Fatalf("%+v", res)
			}
			if a, rel := r.lock.Counts(); a != 1 || rel != 1 {
				t.Errorf("lock acquired %d released %d", a, rel)
			}
			if d := r.lastDoc(); d.StopKind != kind {
				t.Errorf("final StopKind %q", d.StopKind)
			}
		})
	}
	t.Run("context cancel is interrupted", func(t *testing.T) {
		r := newSRig(t, oneCamera("Front"), nil)
		r.build()
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			rep, err := r.sup.Run(ctx)
			r.runCh <- runResult{rep, err}
		}()
		<-r.sup.running
		r.step(2 * time.Second)
		cancel()
		res := r.finish()
		if res.err != nil || res.rep.Kind != StopInterrupted || r.lastDoc().StopKind != "interrupted" || r.lock.Held() {
			t.Fatalf("%+v", res)
		}
	})
}

// D19: LastUnexpectedStop (At byte for byte) and LastWindowClose are carried into the next
// supervisor's documents.
func TestSupervisorCarryForwardAcrossRuns(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	r.sc.panicAt = 4
	r.start()
	res := r.finish()
	if res.rep.Kind != StopUnexpected {
		t.Fatalf("first run %+v", res)
	}
	path := filepath.Join(r.root, "_state", "status.json")
	first, err := status.Read(path)
	if err != nil || first.LastUnexpectedStop.Stop == nil {
		t.Fatalf("first run's file: %v %+v", err, first)
	}
	at := mustJSON(t, first.LastUnexpectedStop.Stop.At)
	// the window-close patch of the warden lands on the stopped file
	first.LastWindowClose = &status.WindowClose{At: status.NewTime(tStart.Add(time.Minute)), Reason: "the NVR window was closed", WindowTitle: "cam.nvr", DetectedBy: "nvr warden"}
	if err := status.NewWriter(path, status.Options{}).Write(first); err != nil {
		t.Fatal(err)
	}

	r2 := newSRig(t, oneCamera("Front"), nil)
	r2.root, r2.cfg = r.root, r.cfg
	r2.src.cfg = r.cfg
	r2.clk.Advance(10 * time.Minute)
	r2.start()
	r2.step(6 * time.Second)
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"At": `+at) {
		t.Fatalf("the second run does not carry At %s verbatim:\n%s", at, raw)
	}
	d := r2.lastDoc()
	if d.State != "running" || d.LastUnexpectedStop.Stop == nil || d.LastUnexpectedStop.Stop.Reason != first.LastUnexpectedStop.Stop.Reason ||
		d.LastWindowClose == nil || d.LastWindowClose.WindowTitle != "cam.nvr" {
		t.Fatalf("carried: %+v / %+v", d.LastUnexpectedStop.Stop, d.LastWindowClose)
	}
	r2.stop(StopRequested)
	final, _ := status.Read(path)
	if final.LastUnexpectedStop.Stop == nil || mustJSON(t, final.LastUnexpectedStop.Stop.At) != at || final.LastWindowClose == nil {
		t.Errorf("the requested stop of run 2 dropped a notice: %+v", final)
	}
	r2.validateAll()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
