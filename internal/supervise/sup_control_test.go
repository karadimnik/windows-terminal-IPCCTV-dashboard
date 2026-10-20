package supervise

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/config"
)

type fakeControl struct {
	mu      sync.Mutex
	events  []string
	r       *srig
	startEr error
}

func (c *fakeControl) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	docs := c.r.docCount()
	c.events = append(c.events, "start")
	if docs == 0 {
		c.events = append(c.events, "BEFORE-FIRST-WRITE")
	}
	return c.startEr
}

func (c *fakeControl) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	alive := len(c.r.st.alive())
	c.events = append(c.events, "stop")
	if alive == 0 {
		c.events = append(c.events, "AFTER-CHILDREN")
	}
}

func (c *fakeControl) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// The control endpoint starts after the first status write and stops before any child is stopped.
func TestSupervisorControlStartsAfterFirstWriteStopsFirst(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	ctl := &fakeControl{r: r}
	r.deps.Control = ctl
	r.start()
	r.step(3 * time.Second)
	if ev := ctl.list(); len(ev) != 1 || ev[0] != "start" {
		t.Fatalf("events %v", ev)
	}
	if len(r.st.alive()) == 0 {
		t.Fatal("no child running")
	}
	r.stop(StopRequested)
	if ev := ctl.list(); len(ev) != 2 || ev[1] != "stop" {
		t.Fatalf("events %v", ev)
	}
}

// A control endpoint that cannot start leaves recording alone, and is not stopped later.
func TestSupervisorControlStartFailureIsNotFatal(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	ctl := &fakeControl{r: r, startEr: errors.New("no port")}
	r.deps.Control = ctl
	r.start()
	r.step(3 * time.Second)
	if len(r.st.of("ffmpeg", "Front")) == 0 {
		t.Fatal("recording did not start")
	}
	res := r.stop(StopRequested)
	if res.err != nil || len(ctl.list()) != 1 {
		t.Fatalf("%v %v", res.err, ctl.list())
	}
}

// Reload reconciles at once (not after ReconcileSeconds) and Snapshot shows the result.
func TestSupervisorReloadReconcilesNow(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), func(c *config.Config) { c.ReconcileSeconds = 3600 })
	if r.build().Snapshot() != nil {
		t.Error("snapshot before Run")
	}
	r.start()
	r.step(2 * time.Second)
	if snap := r.sup.Snapshot(); snap == nil || snap.State != "running" || len(snap.Cameras) != 1 {
		t.Fatalf("snapshot %+v", snap)
	}
	r.src.setCameras([]config.Camera{{Name: "Front", IP: "192.0.2.10", Record: true}, {Name: "Back", IP: "192.0.2.11", Record: true}})
	r.step(30 * time.Second)
	if len(r.st.of("ffmpeg", "Back")) != 0 {
		t.Fatal("picked up without a reload: the test proves nothing")
	}
	errc := make(chan error, 1)
	go func() { errc <- r.sup.Reload(context.Background()) }()
	r.settle()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	// the relay does not serve Back yet: held while the relay is young, then direct (no reconcile needed)
	if !r.stepUntil(20*time.Second, func() bool { return len(r.st.of("ffmpeg", "Back")) == 1 }) {
		t.Fatal("Back not started after the reload")
	}
	if snap := r.sup.Snapshot(); len(snap.Cameras) != 2 {
		t.Errorf("snapshot cameras %d", len(snap.Cameras))
	}
	r.stop(StopRequested)
	if err := r.sup.Reload(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("reload after stop: %v", err)
	}
	if r.sup.Snapshot() != nil {
		t.Error("snapshot after stop")
	}
}

// LiveViewEnabled is applied by Reload: the live MediaMTX starts and stops without a restart, the
// change is not reported as "keeps the values it started with", and the ordered stop takes it.
func TestSupervisorReloadSwitchesLiveView(t *testing.T) {
	r := newSRig(t, oneCamera("Front"), nil)
	r.start()
	r.step(3 * time.Second)
	if len(r.st.started("live")) != 0 {
		t.Fatal("live started while off")
	}
	reload := func() {
		t.Helper()
		errc := make(chan error, 1)
		go func() { errc <- r.sup.Reload(context.Background()) }()
		r.settle()
		r.step(time.Second)
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	setLive := func(on bool) {
		r.src.mu.Lock()
		r.src.cfg.LiveViewEnabled = on
		r.src.mu.Unlock()
	}
	setLive(true)
	reload()
	if !r.stepUntil(10*time.Second, func() bool { return len(r.st.started("live")) == 1 }) {
		t.Fatal("live not started by the reload")
	}
	r.step(6 * time.Second) // a heartbeat
	if d := r.lastDoc(); d.Live == nil || !d.Live.Enabled || !d.Live.Alive {
		t.Errorf("status Live %+v", d.Live)
	}
	setLive(false)
	reload()
	if !r.stepUntil(10*time.Second, func() bool { return r.st.started("live")[0].exited() }) {
		t.Fatal("live not stopped by the reload")
	}
	setLive(true)
	reload()
	if !r.stepUntil(10*time.Second, func() bool { return len(r.st.started("live")) == 2 }) {
		t.Fatal("live not started again")
	}
	r.step(6 * time.Second)
	res := r.stop(StopRequested)
	if res.err != nil || len(res.rep.Survivors) != 0 || len(r.st.alive()) != 0 {
		t.Fatalf("stop %+v, alive %d", res, len(r.st.alive()))
	}
	if txt := r.logText(); strings.Contains(txt, "keeps the values it started with") {
		t.Errorf("live switch reported as a config change needing a restart:\n%s", txt)
	}
}
