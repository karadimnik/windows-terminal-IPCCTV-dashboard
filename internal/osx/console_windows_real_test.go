//go:build windows

package osx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const (
	createNewConsole = 0x00000010
	wmClose          = 0x0010
)

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
)

// consoleHelper starts the console helper in a NEW, hidden console that belongs to it alone, and
// finds that console's host: the conhost.exe whose parent is the helper. Nothing else is looked at;
// if the console was handed to a terminal (no own conhost) the test skips and kills only the helper.
func consoleHelper(t *testing.T, dir string, env ...string) (helper *exec.Cmd, conhost int, hwnd uintptr, child int) {
	t.Helper()
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{helperEnv + "=console", "HOLOCRON_OSX_DIR=" + dir}, env...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole, HideWindow: true}
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
	})
	var st map[string]any
	waitFile(t, filepath.Join(dir, "console.json"), 10*time.Second, &st)
	t.Logf("helper pid %d: %v", cmd.Process.Pid, st)
	hwnd = uintptr(st["hwnd"].(float64))
	child = int(st["child"].(float64))
	for _, pid := range descendants(t, cmd.Process.Pid) {
		if strings.EqualFold(exeName(pid), "conhost.exe") && parentPid(t, pid) == cmd.Process.Pid {
			conhost = pid
		}
	}
	var owner uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
	// A console window reports its client (the helper) as owner, not conhost.
	if conhost == 0 || hwnd == 0 || (int(owner) != cmd.Process.Pid && int(owner) != conhost) {
		t.Skipf("the helper's console is not its own conhost (conhost %d, hwnd %#x owned by %d): not touching it", conhost, hwnd, owner)
	}
	if st["present"] != true {
		t.Fatalf("the probe does not see the new console: %v", st)
	}
	return cmd, conhost, hwnd, child
}

func exeName(pid int) string {
	h, err := syscall.OpenProcess(processQueryLimited, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)
	_, img, _ := processIdentity(h)
	return filepath.Base(img)
}

func parentPid(t *testing.T, pid int) int {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(snap)
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		if int(pe.ProcessID) == pid {
			return int(pe.ParentProcessID)
		}
	}
	return 0
}

func killPid(pid int) {
	if h, err := syscall.OpenProcess(processTerminate, false, uint32(pid)); err == nil {
		syscall.TerminateProcess(h, 1)
		syscall.CloseHandle(h)
	}
}

func readEvents(dir string) []string {
	b, _ := os.ReadFile(filepath.Join(dir, "events.txt"))
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func waitEvents(dir string, n int, within time.Duration) []string {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if e := readEvents(dir); len(e) >= n {
			return e
		}
		time.Sleep(20 * time.Millisecond)
	}
	return readEvents(dir)
}

// G1 (abrupt console death): the console host is killed, no event is sent (SP2); the probe notices
// and the same handler runs its ordered stop; the bound child dies with the helper.
func TestRealWindowsConsoleProbeFires(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	h, conhost, _, child := consoleHelper(t, dir)
	w := newWatch(t, []int{h.Process.Pid, child})
	defer w.close()
	t0 := time.Now()
	killPid(conhost)
	ev := waitEvents(dir, 2, 5*time.Second)
	t.Logf("events %q, %v after killing conhost %d", ev, time.Since(t0), conhost)
	if len(ev) != 2 || !strings.HasPrefix(ev[0], "event console-gone ") || !strings.HasPrefix(ev[1], "done ") {
		t.Fatalf("events %q", ev)
	}
	if surv := w.survivors(3 * time.Second); len(surv) != 0 {
		t.Errorf("survivors %v", surv)
	}
}

// Negative control: without the probe the same death goes unnoticed and everything keeps running.
func TestRealWindowsNoProbeMissesConsoleDeath(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	h, conhost, _, child := consoleHelper(t, dir, "HOLOCRON_OSX_NOPROBE=1")
	w := newWatch(t, []int{h.Process.Pid, child})
	defer w.close()
	killPid(conhost)
	time.Sleep(3 * time.Second)
	if ev := readEvents(dir); len(ev) != 0 {
		t.Fatalf("control failed: an event arrived without the probe: %q", ev)
	}
	if surv := w.survivors(0); len(surv) != 2 {
		t.Fatalf("control failed: survivors %v, want the helper and its child", surv)
	}
	h.Process.Kill() // by pid; the job takes the child
	if surv := w.survivors(3 * time.Second); len(surv) != 0 {
		t.Errorf("after the kill: survivors %v", surv)
	}
}

// G1 (window close): WM_CLOSE to the helper's OWN console window -> CTRL_CLOSE_EVENT -> handler with
// ReasonClose, cleanup inside the budget, child gone.
func TestRealWindowsConsoleCloseEvent(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	h, _, hwnd, child := consoleHelper(t, dir)
	w := newWatch(t, []int{h.Process.Pid, child})
	defer w.close()
	t0 := time.Now()
	procPostMessageW.Call(hwnd, wmClose, 0, 0)
	ev := waitEvents(dir, 2, 6*time.Second)
	t.Logf("events %q, %v after WM_CLOSE", ev, time.Since(t0))
	if len(ev) != 2 || !strings.HasPrefix(ev[0], "event close ") || !strings.HasPrefix(ev[1], "done ") {
		t.Fatalf("events %q", ev)
	}
	if surv := w.survivors(3 * time.Second); len(surv) != 0 {
		t.Errorf("survivors %v", surv)
	}
}

// CREATE_SUSPENDED closes the assignment gap (SP3 w3): with the same 300 ms delay before the
// assignment, but suspended, no grandchild escapes.
func TestRealWindowsSuspendedClosesAssignGap(t *testing.T) {
	needReal(t)
	surv, grand := gapCase(t, "1")
	if len(surv) != 0 {
		t.Errorf("survivors %v (grandchildren %v)", surv, grand)
	}
}

// Negative control (SP3 w2): created running and assigned 300 ms later, the leaves' grandchildren
// are started outside the job and survive the hard kill.
func TestRealWindowsAssignAfterStartLetsGrandchildEscape(t *testing.T) {
	needReal(t)
	surv, grand := gapCase(t, "0")
	if len(intersect(surv, grand)) == 0 {
		t.Fatalf("control failed: no grandchild escaped (survivors %v, grandchildren %v)", surv, grand)
	}
	t.Logf("escaped: %v of grandchildren %v (expected: the assign-after-start gap)", intersect(surv, grand), grand)
}

func gapCase(t *testing.T, suspend string) (surv, grand []int) {
	dir := t.TempDir()
	h := startHelper(t, dir, "scope-sup", "HOLOCRON_OSX_N=3", "HOLOCRON_OSX_KIND=grand", "HOLOCRON_OSX_GAP=300", "HOLOCRON_OSX_SUSPEND="+suspend)
	var st helperState
	waitFile(t, filepath.Join(dir, "state.json"), 20*time.Second, &st)
	if len(st.Grand) != 3 {
		t.Fatalf("grandchildren %v", st.Grand)
	}
	all := union(descendants(t, h.Process.Pid), st.Grand)
	sw := &sweep{}
	sw.add(t, all...)
	t.Cleanup(func() { t.Logf("sweep killed %v", sw.run(t)) })
	w := newWatch(t, all)
	t.Cleanup(w.close)
	hardKill(t, h)
	surv = w.survivors(5 * time.Second)
	t.Logf("suspend=%s children %v grandchildren %v tree %v survivors %v", suspend, st.Children, st.Grand, all, surv)
	return surv, st.Grand
}
