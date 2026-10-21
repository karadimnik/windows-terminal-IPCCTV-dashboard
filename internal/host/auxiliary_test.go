package host

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holocron/internal/osx"
)

// ---- a fake auxiliary child (the web role) ----

type auxProc struct {
	pid    int
	exitCh chan int
	once   sync.Once
	asked  atomic.Bool
	killed atomic.Bool
}

func (p *auxProc) Pid() int           { return p.pid }
func (p *auxProc) Wait() (int, error) { return <-p.exitCh, nil }
func (p *auxProc) exit(code int)      { p.once.Do(func() { p.exitCh <- code }) }
func (p *auxProc) Kill() error        { p.killed.Store(true); p.exit(1); return nil }

type auxSpawner struct {
	h        *harness
	mu       sync.Mutex
	procs    []*auxProc
	spawnAt  []time.Time
	failNext bool
	obey     bool // exits when asked to stop
}

func (s *auxSpawner) Spawn(ctx context.Context) (Proc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &auxProc{pid: 9000 + len(s.procs), exitCh: make(chan int, 1)}
	s.procs = append(s.procs, p)
	s.spawnAt = append(s.spawnAt, s.h.clk.Now())
	return p, nil
}
func (s *auxSpawner) n() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.procs) }
func (s *auxSpawner) proc(i int) *auxProc {
	s.h.eventually("aux proc", func() bool { return s.n() > i })
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.procs[i]
}
func (s *auxSpawner) gaps() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(s.spawnAt); i++ {
		out = append(out, s.spawnAt[i].Sub(s.spawnAt[i-1]))
	}
	return out
}

func (s *auxSpawner) aux(reg *osx.ChildRegistry, in osx.Inspector) Aux {
	return Aux{Name: "web", Spawner: s, Registry: reg, Inspector: in, StopWait: 5 * time.Second,
		Stop: func(p Proc) {
			ap := p.(*auxProc)
			ap.asked.Store(true)
			if s.obey {
				ap.exit(0)
			}
		},
		Restart: AuxRestart{AlertID: 104, AlertTitle: "the web app stopped unexpectedly and is being restarted"}}
}

func newAuxHarness(t *testing.T, obey bool) (*harness, *auxSpawner) {
	h := newHarness(t, Service)
	s := &auxSpawner{h: h, obey: obey}
	h.aux = []Aux{s.aux(nil, nil)}
	h.onSpawn = func(l *fakeLife) { l.running(); l.honourStop() }
	return h, s
}

// Restart in place after 5 s, then 10, 20, 40, 60, 60; a life of 300 s resets to 5 s; the 104
// alert comes with the 5th crash in 30 minutes (not before) and the supervisor is never touched.
func TestAuxRestartBackoffAlertAndSupervisorUntouched(t *testing.T) {
	h, s := newAuxHarness(t, true)
	h.start()
	sup := h.life(1)
	for i := 0; i < 6; i++ { // six crashes: the delays before the 2nd..7th start
		s.proc(i).exit(1)
		if i == 3 {
			s.proc(i + 1) // the 5th crash is not here yet
			if ids := h.alerts.ids(); len(ids) != 0 {
				t.Fatalf("alert before the 5th crash: %v", ids)
			}
		}
		h.eventually("restart", func() bool { return s.n() > i+1 })
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	got := s.gaps()
	for i, w := range want {
		if got[i] < w || got[i] > w+500*time.Millisecond {
			t.Errorf("gap %d = %v, want about %v (all %v)", i, got[i], w, got)
		}
	}
	var n104 int
	for _, e := range h.alerts.all() {
		if e.ID == 104 {
			n104++
			if e.Severity != "Error" || !strings.Contains(e.Message, "Recording is not affected") {
				t.Errorf("alert %+v", e)
			}
		}
	}
	if n104 != 1 {
		t.Errorf("alert 104 raised %d times, want exactly 1 (alerts %v)", n104, h.alerts.ids())
	}
	if h.nLives() != 1 || sup.killed.Load() {
		t.Errorf("the supervisor was touched: %d lives", h.nLives())
	}
	if c := strings.Count(h.logs.String(), "restarting it in"); c != 6 {
		t.Errorf("%d WARN restart records, want 6:\n%s", c, h.logs.String())
	}

	// a healthy life of 300 s resets the backoff to 5 s
	last := s.proc(6)
	h.eventuallyStep("300 s healthy", time.Second, func() bool {
		return h.clk.Since(0) > 0 && s.n() == 7 && time.Since(time.Time{}) > 0 && h.clk.Now().Sub(s.spawnAt[6]) >= 301*time.Second
	})
	crashedAt := h.clk.Now()
	last.exit(1)
	h.eventually("restart after reset", func() bool { return s.n() > 7 })
	if d := s.spawnAt[7].Sub(crashedAt); d > 6*time.Second {
		t.Errorf("after a healthy life the delay was %v, want 5 s", d)
	}

	h.console.trigger(osx.ReasonTerminate)
	if res, err := h.wait(); err != nil || res.Outcome != OutcomeRequested {
		t.Fatalf("%+v %v", res, err)
	}
}

// A supervisor crash and restart does not touch the web child.
func TestAuxNotRestartedWithTheSupervisor(t *testing.T) {
	h, s := newAuxHarness(t, true)
	h.onSpawn = func(l *fakeLife) {
		l.running()
		if l.pid != 5000 { // the first life is killed, not stopped
			l.honourStop()
		}
	}
	h.start()
	l1 := h.life(1)
	w := s.proc(0)
	l1.Kill()
	h.life(2)
	h.eventuallyStep("a second supervisor life", 100*time.Millisecond, func() bool { return h.nLives() >= 2 })
	if s.n() != 1 || w.killed.Load() || w.asked.Load() {
		t.Errorf("the web child was restarted or stopped with the supervisor: spawns %d", s.n())
	}
	h.console.trigger(osx.ReasonTerminate)
	h.wait()
}

// The host's end asks the web child to stop (it ends by itself, never killed); one that ignores the
// request is killed after StopWait.
func TestAuxStopsWithTheHost(t *testing.T) {
	for _, obey := range []bool{true, false} {
		name := map[bool]string{true: "obeys", false: "ignores the request"}[obey]
		t.Run(name, func(t *testing.T) {
			h, s := newAuxHarness(t, obey)
			h.start()
			h.life(1)
			w := s.proc(0)
			h.console.trigger(osx.ReasonTerminate)
			res, err := h.wait()
			if err != nil || res.Outcome != OutcomeRequested {
				t.Fatalf("%+v %v", res, err)
			}
			if !w.asked.Load() || w.killed.Load() != !obey {
				t.Errorf("asked %v killed %v, obey %v", w.asked.Load(), w.killed.Load(), obey)
			}
			if s.n() != 1 {
				t.Errorf("web restarted while the host was stopping: %d spawns", s.n())
			}
		})
	}
}

// The running child is listed in the registry for the census, and removed when it exits.
func TestAuxIsListedInTheRegistry(t *testing.T) {
	h := newHarness(t, Service)
	s := &auxSpawner{h: h, obey: true}
	reg := osx.NewRegistry(filepath.Join(h.state, "webchild.json"))
	in := &osx.FakeInspector{Procs: map[int]osx.ProcInfo{9000: {Start: "t0", Image: "/bin/holocron"}, 9001: {Start: "t1", Image: "/bin/holocron"}}}
	h.aux = []Aux{s.aux(reg, in)}
	h.onSpawn = func(l *fakeLife) { l.running(); l.honourStop() }
	h.start()
	h.life(1)
	w := s.proc(0)
	h.eventually("listed", func() bool { e, _ := reg.Load(); return len(e) == 1 })
	e, _ := reg.Load()
	if !reflect.DeepEqual(e, []osx.ChildEntry{{Pid: 9000, Start: "t0", Image: "/bin/holocron", Role: "web"}}) {
		t.Errorf("%+v", e)
	}
	w.exit(1)
	s.proc(1)
	h.eventually("relisted", func() bool { e, _ := reg.Load(); return len(e) == 1 && e[0].Pid == 9001 })
	h.console.trigger(osx.ReasonTerminate)
	h.wait()
	if e, _ := reg.Load(); len(e) != 0 {
		t.Errorf("entries left after the host ended: %+v", e)
	}
	if _, err := os.Stat(filepath.Join(h.state, "webchild.json")); err != nil {
		t.Error(err)
	}
}
