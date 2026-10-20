package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/status"
)

// Real-process tests re-execute this test binary as a stand-in supervisor ("fakesup") or as the
// host itself ("host"). Opt-in: HOLOCRON_HOST_REAL=1. Only pids these tests start are touched.
const roleEnv = "HOLOCRON_HOST_ROLE"

func TestMain(m *testing.M) {
	switch os.Getenv(roleEnv) {
	case "fakesup":
		os.Exit(fakeSupervisor())
	case "host":
		os.Exit(hostProcess())
	case "realsup":
		os.Exit(realSupervisor())
	}
	os.Exit(m.Run())
}

func needReal(t *testing.T) {
	t.Helper()
	if os.Getenv("HOLOCRON_HOST_REAL") != "1" {
		t.Skip("real-process test: set HOLOCRON_HOST_REAL=1")
	}
}

func longRunner() osx.Cmd {
	if runtime.GOOS == "windows" {
		return osx.Cmd{Path: `C:\Windows\System32\PING.EXE`, Args: []string{"-t", "127.0.0.1"}, Role: "ping"}
	}
	return osx.Cmd{Path: "/usr/bin/sleep", Args: []string{"600"}, Role: "sleep"}
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	f.WriteString(line + "\n")
	f.Close()
}

// fakeSupervisor behaves like the supervisor towards the host: the instance lock (exit 20 when
// held), the registry reap, its own scope with two long-running children, a heartbeat document,
// and stop.request honoured with a final "requested" document. HOLOCRON_HOST_SUPMODE=hang ignores
// the stop request; crash exits 1 at once (before any document).
func fakeSupervisor() int {
	state := os.Getenv("HOLOCRON_HOST_STATE")
	mode := os.Getenv("HOLOCRON_HOST_SUPMODE")
	if mode == "crash" {
		return 1
	}
	lk, err := osx.AcquireInstanceLock(osx.LockPath(state))
	if err != nil {
		return ExitAnotherSupervisor
	}
	defer lk.Release()
	reg := osx.NewRegistry(osx.RegistryPath(state))
	osx.Reap(reg, osx.OSInspector{}, nil)
	scope, err := osx.NewScope("fakesup", osx.ScopeOptions{Registry: reg})
	if err != nil {
		return 1
	}
	defer scope.Close()
	for i := 0; i < 2; i++ {
		if p, err := scope.Start(longRunner()); err == nil {
			appendLine(filepath.Join(state, "sup-children.txt"), fmt.Sprintf("%d %d", os.Getpid(), p.Pid()))
		}
	}
	w := status.NewWriter(filepath.Join(state, "status.json"), status.Options{})
	started := status.NewTime(time.Now())
	doc := func(st, kind string) *status.Document {
		return &status.Document{Schema: 1, State: st, StopKind: kind, SupervisorPid: int64(os.Getpid()), StartedAt: started, UpdatedAt: status.NewTime(time.Now()),
			LastUnexpectedStop: status.MaybeStop{Present: true}}
	}
	stopFile := filepath.Join(state, "stop.request")
	for {
		w.Write(doc(status.StateRunning, ""))
		for i := 0; i < 5; i++ {
			time.Sleep(50 * time.Millisecond)
			if _, err := os.Stat(stopFile); err == nil && mode != "hang" {
				scope.Stop(time.Second)
				w.Write(doc(status.StateStopped, status.StopKindRequested))
				os.Remove(stopFile)
				return 0
			}
		}
	}
}

// fileAlerts writes each alert id to a file.
type fileAlerts struct {
	mu   sync.Mutex
	path string
}

func (a *fileAlerts) Raise(e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	appendLine(a.path, fmt.Sprintf("%d %s", e.ID, e.Title))
}

func readAlerts(path string) []int {
	b, _ := os.ReadFile(path)
	var out []int
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			if n, err := strconv.Atoi(f[0]); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

func selfEnv(role string, extra ...string) []string {
	return append(os.Environ(), append([]string{roleEnv + "=" + role}, extra...)...)
}

// newHost builds a real host around the fakesup spawner, in this process or in a helper.
func newHost(dir, state string, mode Mode, logPath string) (*Host, *osx.Scope, error) {
	scope, err := osx.NewScope("host", osx.ScopeOptions{})
	if err != nil {
		return nil, nil, err
	}
	exe, _ := os.Executable()
	sp := &ExecSpawner{Scope: scope, Path: exe, Args: []string{"-test.run=^$"},
		Env: selfEnv("fakesup", "HOLOCRON_HOST_STATE="+state, "HOLOCRON_HOST_SUPMODE="+os.Getenv("HOLOCRON_HOST_SUPMODE"))}
	lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "debug"}, Getenv: func(string) string { return "" },
		Sink: logx.NewRotatingFile(logx.FileOptions{Path: logPath, MaxBytes: 1 << 20})})
	h := New(Config{StateDir: state, WindowTitle: "holocron-w8-test"}, Deps{Spawner: sp, Alerts: &fileAlerts{path: filepath.Join(dir, "alerts.txt")}, Log: lg.For("host"), Mode: mode})
	return h, scope, nil
}

// hostProcess is the host as its own process (for the console and hard-kill tests).
func hostProcess() int {
	dir, state := os.Getenv("HOLOCRON_HOST_DIR"), os.Getenv("HOLOCRON_HOST_STATE")
	mode := Attached
	if os.Getenv("HOLOCRON_HOST_MODE") == "service" {
		mode = Service
	}
	h, scope, err := newHost(dir, state, mode, filepath.Join(dir, "host.log"))
	if err != nil {
		return 3
	}
	appendLine(filepath.Join(dir, "host.pid"), strconv.Itoa(os.Getpid()))
	os.WriteFile(filepath.Join(dir, "host.hwnd"), []byte(strconv.FormatUint(uint64(consoleHwnd()), 10)), 0o644)
	res, err := h.Run(context.Background())
	b, _ := json.Marshal(map[string]any{"outcome": res.Outcome, "lives": res.Lives, "restarts": res.Restarts, "err": fmt.Sprint(err)})
	os.WriteFile(filepath.Join(dir, "host-result.json"), b, 0o644)
	scope.Close()
	return res.ExitCode()
}

func supChildren(state string) map[int][]int {
	b, _ := os.ReadFile(filepath.Join(state, "sup-children.txt"))
	out := map[int][]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			s, _ := strconv.Atoi(f[0])
			c, _ := strconv.Atoi(f[1])
			out[s] = append(out[s], c)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readDoc(state string) *status.Document {
	d, err := status.Read(filepath.Join(state, "status.json"))
	if err != nil {
		return nil
	}
	return d
}

// tracker remembers every pid a test saw (supervisors and their children) with its identity, to
// prove they are gone and to kill, by verified pid only, anything left.
type tracker struct {
	mu sync.Mutex
	e  map[int]osx.ChildEntry
}

func (k *tracker) add(pids ...int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.e == nil {
		k.e = map[int]osx.ChildEntry{}
	}
	for _, pid := range pids {
		if _, ok := k.e[pid]; ok {
			continue
		}
		if info, err := (osx.OSInspector{}).Inspect(pid); err == nil {
			k.e[pid] = osx.ChildEntry{Pid: pid, Start: info.Start, Image: info.Image}
		}
	}
}

func (k *tracker) alive(pids ...int) []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []int
	for _, pid := range pids {
		if e, ok := k.e[pid]; ok {
			if ok, _ := osx.Verify(e, osx.OSInspector{}, runtime.GOOS); ok {
				out = append(out, pid)
			}
		}
	}
	return out
}

func (k *tracker) all() []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []int
	for pid := range k.e {
		out = append(out, pid)
	}
	return out
}

func (k *tracker) gone(t *testing.T, within time.Duration, pids ...int) []int {
	deadline := time.Now().Add(within)
	for {
		a := k.alive(pids...)
		if len(a) == 0 || time.Now().After(deadline) {
			return a
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (k *tracker) sweep(t *testing.T) {
	var killed []int
	for _, pid := range k.alive(k.all()...) {
		k.mu.Lock()
		e := k.e[pid]
		k.mu.Unlock()
		if (osx.OSInspector{}).Stop(e) == nil {
			killed = append(killed, pid)
		}
	}
	if len(killed) > 0 {
		t.Errorf("sweep had to kill %v", killed)
	}
}
