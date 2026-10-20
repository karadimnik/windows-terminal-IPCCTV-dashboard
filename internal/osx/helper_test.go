package osx

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The real-process tests re-execute this test binary in a helper role (HOLOCRON_OSX_HELPER) and
// work only with pids they started and recorded. They are opt-in: HOLOCRON_OSX_REAL=1.
const helperEnv = "HOLOCRON_OSX_HELPER"

func TestMain(m *testing.M) {
	if role := os.Getenv(helperEnv); role != "" {
		runHelper(role)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func needReal(t *testing.T) {
	t.Helper()
	if os.Getenv("HOLOCRON_OSX_REAL") != "1" {
		t.Skip("real-process test: set HOLOCRON_OSX_REAL=1")
	}
}

// helperState is what a helper writes once its children exist.
type helperState struct {
	Pid      int   `json:"pid"`
	Children []int `json:"children"`
	Grand    []int `json:"grand"`
}

func hlog(dir, format string, a ...any) {
	f, err := os.OpenFile(filepath.Join(dir, "helper.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%d pid=%d %s\n", time.Now().UnixMilli(), os.Getpid(), fmt.Sprintf(format, a...))
	f.Close()
}

func writeJSONFile(path string, v any) {
	b, _ := json.Marshal(v)
	tmp := path + ".tmp"
	os.WriteFile(tmp, b, 0o644)
	os.Rename(tmp, path)
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	f.WriteString(line + "\n")
	f.Close()
}

func envInt(name string) int { n, _ := strconv.Atoi(os.Getenv(name)); return n }

func forever() {
	for {
		time.Sleep(time.Hour)
	}
}

func runHelper(role string) {
	dir := os.Getenv("HOLOCRON_OSX_DIR")
	switch role {
	case "leaf": // a child; with SPAWN it starts a long-running grandchild at once
		if os.Getenv("HOLOCRON_OSX_SPAWN") == "1" {
			pid, err := spawnLong()
			hlog(dir, "leaf spawned grandchild %d err=%v", pid, err)
			if err == nil {
				appendLine(filepath.Join(dir, "grand.pids"), strconv.Itoa(pid))
			}
		}
		forever()
	case "scope-sup": // a supervisor: a Scope, N children, state file, then lives until killed
		s, err := NewScope("helper", ScopeOptions{Registry: NewRegistry(RegistryPath(dir))})
		if err != nil {
			hlog(dir, "scope: %v", err)
			os.Exit(3)
		}
		if g := envInt("HOLOCRON_OSX_GAP"); g > 0 {
			if os.Getenv("HOLOCRON_OSX_SUSPEND") == "0" {
				s.assignGap = time.Duration(g) * time.Millisecond
			} else {
				s.afterCreateHook = func(int) { time.Sleep(time.Duration(g) * time.Millisecond) }
			}
		}
		st := helperState{Pid: os.Getpid()}
		n := max(envInt("HOLOCRON_OSX_N"), 1)
		for i := 0; i < n; i++ {
			c := longCmd()
			if os.Getenv("HOLOCRON_OSX_KIND") == "grand" {
				c = selfCmd("leaf", "HOLOCRON_OSX_SPAWN=1")
			}
			p, err := s.Start(c)
			if err != nil {
				hlog(dir, "start: %v", err)
				continue
			}
			st.Children = append(st.Children, p.Pid())
			hlog(dir, "started child %d", p.Pid())
		}
		if os.Getenv("HOLOCRON_OSX_KIND") == "grand" { // the leaves report their grandchildren
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && len(readPids(filepath.Join(dir, "grand.pids"))) < len(st.Children) {
				time.Sleep(20 * time.Millisecond)
			}
			st.Grand = readPids(filepath.Join(dir, "grand.pids"))
		}
		writeJSONFile(filepath.Join(dir, "state.json"), st)
		forever()
	case "race": // starts children back to back until killed (at most 20), each pid appended after its start
		s, err := NewScope("race", ScopeOptions{})
		if err != nil {
			os.Exit(3)
		}
		for i := 0; i < 20; i++ {
			p, err := s.Start(longCmd())
			if err == nil {
				appendLine(filepath.Join(dir, "race.pids"), strconv.Itoa(p.Pid()))
			}
			time.Sleep(15 * time.Millisecond)
		}
		forever()
	case "orphans": // children WITHOUT any binding, recorded in the registry, then exit
		reg := NewRegistry(RegistryPath(dir))
		st := helperState{Pid: os.Getpid()}
		for i := 0; i < 3; i++ {
			pid, err := spawnUnbound()
			if err != nil {
				hlog(dir, "spawn: %v", err)
				continue
			}
			info, err := OSInspector{}.Inspect(pid)
			if err != nil {
				hlog(dir, "inspect %d: %v", pid, err)
			}
			reg.Add(ChildEntry{Pid: pid, Start: info.Start, Image: info.Image, Group: info.Group, Role: "orphan"})
			st.Children = append(st.Children, pid)
		}
		writeJSONFile(filepath.Join(dir, "state.json"), st)
	case "lock": // try the lock; report; hold it until killed
		l, err := AcquireInstanceLock(os.Getenv("HOLOCRON_OSX_LOCK"))
		if err != nil {
			writeJSONFile(filepath.Join(dir, "lock.json"), map[string]any{"err": err.Error(), "held": isHeld(err), "owner": ownerOf(err)})
			return
		}
		writeJSONFile(filepath.Join(dir, "lock.json"), map[string]any{"acquired": true, "pid": l.Owner().Pid})
		forever()
	case "echo": // prints its arguments and one variable, exits 7
		fmt.Fprintf(os.Stdout, "args=%s\n", strings.Join(os.Args[2:], "|"))
		fmt.Fprintf(os.Stderr, "env=%s\n", os.Getenv("HOLOCRON_OSX_ECHO"))
		os.Exit(7)
	case "console": // OnConsoleClose (+ the probe unless NOPROBE), one bound child, report and exit after the "cleanup"
		runConsoleHelper(dir)
	}
}

func isHeld(err error) bool { _, ok := err.(*HeldError); return ok }

func ownerOf(err error) int {
	if he, ok := err.(*HeldError); ok {
		return he.Owner.Pid
	}
	return 0
}

func runConsoleHelper(dir string) {
	s, err := NewScope("console", ScopeOptions{})
	if err != nil {
		os.Exit(3)
	}
	child, _ := s.Start(longCmd())
	var once sync.Once
	events := filepath.Join(dir, "events.txt")
	handle := func(r Reason) {
		once.Do(func() {
			appendLine(events, fmt.Sprintf("event %s %d", r, time.Now().UnixMilli()))
			time.Sleep(300 * time.Millisecond) // the ordered stop
			appendLine(events, fmt.Sprintf("done %d", time.Now().UnixMilli()))
			s.Close()
			os.Exit(0)
		})
	}
	OnConsoleClose(handle)
	probe := "off"
	if os.Getenv("HOLOCRON_OSX_NOPROBE") != "1" {
		if _, err := WatchConsole(handle, 0); err != nil {
			probe = err.Error()
		} else {
			probe = "on"
		}
	}
	ok, detail := ConsolePresent()
	writeJSONFile(filepath.Join(dir, "console.json"), map[string]any{"pid": os.Getpid(), "child": pidOf(child), "probe": probe, "present": ok, "detail": detail, "hwnd": consoleWindow()})
	time.Sleep(60 * time.Second) // failsafe
}

func pidOf(p Proc) int {
	if p == nil {
		return 0
	}
	return p.Pid()
}

func selfCmd(role string, env ...string) Cmd {
	exe, _ := os.Executable()
	return Cmd{Path: exe, Args: []string{"-test.run=^$"}, Env: append(os.Environ(), append([]string{helperEnv + "=" + role}, env...)...), Role: role}
}

func readPids(path string) []int {
	b, _ := os.ReadFile(path)
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// startHelper runs this test binary in a helper role with a scratch dir; the process is killed in
// Cleanup when still running.
func startHelper(t *testing.T, dir, role string, env ...string) *exec.Cmd {
	t.Helper()
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{helperEnv + "=" + role, "HOLOCRON_OSX_DIR=" + dir}, env...)...)
	cmd.SysProcAttr = helperAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "helper.log"))
			t.Logf("helper.log:\n%s", b)
		}
	})
	t.Logf("helper %s pid %d", role, cmd.Process.Pid)
	return cmd
}

func waitFile(t *testing.T, path string, within time.Duration, v any) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 && json.Unmarshal(b, v) == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s after %v", path, within)
}

// sweep kills, by pid and only after verifying the recorded identity, whatever a test left.
type sweep struct {
	mu      sync.Mutex
	entries []ChildEntry
}

func (s *sweep) add(t *testing.T, pids ...int) {
	for _, pid := range pids {
		if info, err := (OSInspector{}).Inspect(pid); err == nil {
			s.mu.Lock()
			s.entries = append(s.entries, ChildEntry{Pid: pid, Start: info.Start, Image: info.Image})
			s.mu.Unlock()
		}
	}
}

func (s *sweep) run(t *testing.T) (killed []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if ok, _ := Verify(e, OSInspector{}, goos()); ok {
			if (OSInspector{}).Stop(e) == nil {
				killed = append(killed, e.Pid)
			}
		}
	}
	s.entries = nil
	return killed
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func lockMainThread() { runtime.LockOSThread() }
