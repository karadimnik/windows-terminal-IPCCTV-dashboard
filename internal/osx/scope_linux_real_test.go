//go:build linux

package osx

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The main goroutine stays on the main thread, so a locked starter can never land there (Go never
// ends the main thread; SP3 l3 versus l3b).
func init() { lockMainThread() }

func alivePid(pid int) bool { _, err := (OSInspector{}).Inspect(pid); return err == nil }

func gone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !alivePid(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !alivePid(pid)
}

// The dedicated starter keeps its children alive while this process lives; closing the scope ends
// the thread and Pdeathsig takes the children.
func TestRealLinuxLockedStarterKeepsChildren(t *testing.T) {
	needReal(t)
	s, err := NewScope("t", ScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Start(longCmd())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	time.Sleep(time.Second)
	if !alivePid(p.Pid()) {
		t.Fatal("the child died while the supervisor lives")
	}
	s.Close()
	if !gone(p.Pid(), 2*time.Second) {
		t.Error("the child outlived Close (the starter thread's end)")
	}
}

// Negative control (SP3 l3): a starter goroutine that locks its thread, starts and RETURNS ends
// that thread, and Pdeathsig kills the child at once although this process is alive.
func TestRealLinuxReturningStarterKillsChildren(t *testing.T) {
	needReal(t)
	s := &Scope{name: "pitfall", live: map[int]*proc{}, starterReturns: true}
	s.log = discard()
	if err := s.sys.open(s); err != nil {
		t.Fatal(err)
	}
	p, err := s.Start(longCmd())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	if !gone(p.Pid(), time.Second) {
		t.Fatal("control failed: the child of a returning locked starter survived")
	}
	code, _ := p.Wait()
	t.Logf("child %d killed by the starter thread's end, exit code %d", p.Pid(), code)
}

// G6 on Linux: the orderly stop reaches the grandchildren through the process group
// (kill(-pgid, SIGTERM), grace, SIGKILL; SP3 l7).
func TestRealLinuxOrderlyStopKillsGroup(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	s, err := NewScope("t", ScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := selfCmd("leaf", "HOLOCRON_OSX_SPAWN=1", "HOLOCRON_OSX_DIR="+dir)
	p, err := s.Start(c)
	if err != nil {
		t.Fatal(err)
	}
	var grand []int
	deadline := time.Now().Add(5 * time.Second)
	for len(grand) == 0 && time.Now().Before(deadline) {
		grand = readPids(filepath.Join(dir, "grand.pids"))
		time.Sleep(10 * time.Millisecond)
	}
	if len(grand) != 1 {
		t.Fatal("no grandchild")
	}
	sw := &sweep{}
	sw.add(t, grand...)
	defer func() { t.Logf("sweep killed %v", sw.run(t)) }()
	if info, _ := (OSInspector{}).Inspect(grand[0]); info.Group != p.Pid() {
		t.Fatalf("grandchild group %d, want the child's %d", info.Group, p.Pid())
	}
	t0 := time.Now()
	surv := s.Stop(1500 * time.Millisecond)
	if len(surv) != 0 {
		t.Errorf("survivors %v", surv)
	}
	if !gone(grand[0], 2*time.Second) {
		t.Errorf("grandchild %d survived the orderly stop", grand[0])
	}
	t.Logf("child %d and grandchild %d gone %v after the stop", p.Pid(), grand[0], time.Since(t0))
}

// OnConsoleClose on Unix: SIGHUP and SIGTERM reach the handler, which runs its cleanup; the bound
// child dies with the helper.
func TestRealUnixConsoleSignals(t *testing.T) {
	needReal(t)
	for _, c := range []struct {
		sig  syscall.Signal
		want Reason
	}{{syscall.SIGHUP, ReasonHangup}, {syscall.SIGTERM, ReasonTerminate}} {
		dir := t.TempDir()
		h := startHelper(t, dir, "console", "HOLOCRON_OSX_NOPROBE=1")
		var st map[string]any
		waitFile(t, filepath.Join(dir, "console.json"), 5*time.Second, &st)
		child := int(st["child"].(float64))
		h.Process.Signal(c.sig)
		h.Wait()
		b, _ := os.ReadFile(filepath.Join(dir, "events.txt"))
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event "+string(c.want)+" ") || !strings.HasPrefix(lines[1], "done ") {
			t.Errorf("%v: events %q", c.sig, lines)
		}
		if !gone(child, 2*time.Second) {
			t.Errorf("%v: bound child %d survived", c.sig, child)
			syscall.Kill(child, syscall.SIGKILL)
		}
	}
}
