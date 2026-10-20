//go:build windows

package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"holocron/internal/osx"
)

const (
	createNoWindow   = 0x08000000
	createNewConsole = 0x00000010
	wmClose          = 0x0010
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procPostMessage = user32.NewProc("PostMessageW")
	procWindowPid   = user32.NewProc("GetWindowThreadProcessId")
	procConsoleWnd  = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow")
)

func consoleHwnd() uintptr { h, _, _ := procConsoleWnd.Call(); return h }

func hideAttr(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow} }

func newConsoleAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole, HideWindow: true}
}

// ownConsole finds the helper host's own console host (the conhost.exe whose parent is the
// helper) and its window (a console window reports its client, the helper, as owner). Skips when
// the console was handed to a terminal: nothing else is touched.
func ownConsole(t *testing.T, dir string, helper int) (conhost int, hwnd uintptr) {
	t.Helper()
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		if int(pe.ParentProcessID) == helper && strings.EqualFold(syscall.UTF16ToString(pe.ExeFile[:]), "conhost.exe") {
			conhost = int(pe.ProcessID)
		}
	}
	syscall.CloseHandle(snap)
	// the helper wrote its own GetConsoleWindow(); no window is enumerated
	b, _ := os.ReadFile(filepath.Join(dir, "host.hwnd"))
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	var pid uint32
	if n != 0 {
		procWindowPid.Call(uintptr(n), uintptr(unsafe.Pointer(&pid)))
	}
	if int(pid) == helper {
		hwnd = uintptr(n)
	}
	if conhost == 0 || hwnd == 0 {
		t.Skipf("the helper host has no console of its own (conhost %d, window %#x): not touching anything", conhost, hwnd)
	}
	return conhost, hwnd
}

// G1/G2/G3 on Windows: WM_CLOSE to the host's OWN console window -> CTRL_CLOSE_EVENT -> ordered stop
// within the 5 s, window-closed, one 103, no restart, nothing left.
func TestRealHostWindowCloseEvent(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "_state")
	cmd, done := startHostHelper(t, dir, state, newConsoleAttr)
	k := &tracker{}
	defer k.sweep(t)
	waitRunning(t, state, k)
	_, hwnd := ownConsole(t, dir, cmd.Process.Pid)
	t0 := time.Now()
	procPostMessage.Call(hwnd, wmClose, 0, 0)
	checkWindowClosed(t, dir, state, cmd, done, k, 6*time.Second)
	t.Logf("WM_CLOSE to exit %v", time.Since(t0))
}

// The console host dies without an event: the probe makes it the same window close.
func TestRealHostConsoleDeathIsWindowClose(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "_state")
	cmd, done := startHostHelper(t, dir, state, newConsoleAttr)
	k := &tracker{}
	defer k.sweep(t)
	waitRunning(t, state, k)
	conhost, _ := ownConsole(t, dir, cmd.Process.Pid)
	k.add(conhost)
	t0 := time.Now()
	if err := (osx.OSInspector{}).Stop(k.e[conhost]); err != nil {
		t.Fatal(err)
	}
	checkWindowClosed(t, dir, state, cmd, done, k, 8*time.Second)
	t.Logf("console host killed to exit %v", time.Since(t0))
}
