//go:build windows

package osx

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func goos() string { return "windows" }

const pingExe = `C:\Windows\System32\PING.EXE`

func longCmd() Cmd { return Cmd{Path: pingExe, Args: []string{"-t", "127.0.0.1"}, Role: "ping"} }

func spawnLong() (int, error) { return spawnUnbound() }

func spawnUnbound() (int, error) {
	c := exec.Command(pingExe, "-t", "127.0.0.1")
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, nil
}

func helperAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{CreationFlags: createNoWindow} }

func consoleWindow() uintptr { h, _, _ := procGetConsoleWindow.Call(); return h }

func creationTime(pid int) (uint64, bool) {
	h, err := syscall.OpenProcess(processQueryLimited, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)
	var c, e, k, u syscall.Filetime
	if syscall.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0, false
	}
	return uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime), true
}

// descendants walks the Toolhelp parent links below root; a process created before root (a reused
// parent pid) is not counted.
func descendants(t *testing.T, root int) []int {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(snap)
	parent := map[int]int{}
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		parent[int(pe.ProcessID)] = int(pe.ParentProcessID)
	}
	rootT, _ := creationTime(root)
	var out []int
	for pid := range parent {
		for p, n := parent[pid], 0; p != 0 && n < 64; p, n = parent[p], n+1 {
			if p == root {
				if ct, ok := creationTime(pid); ok && ct >= rootT {
					out = append(out, pid)
				}
				break
			}
		}
	}
	return out
}

// watch holds a handle to every pid, opened BEFORE the kill, so a reused pid cannot be mistaken
// for a survivor (SP3 method).
type watch struct {
	pids []int
	hs   []syscall.Handle
}

func newWatch(t *testing.T, pids []int) *watch {
	w := &watch{}
	for _, pid := range pids {
		h, err := syscall.OpenProcess(synchronize|processQueryLimited, false, uint32(pid))
		if err == nil {
			w.pids, w.hs = append(w.pids, pid), append(w.hs, h)
		}
	}
	return w
}

func (w *watch) survivors(within time.Duration) []int {
	deadline := time.Now().Add(within)
	var alive []int
	for i, h := range w.hs {
		ms := time.Until(deadline).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		if r, _ := syscall.WaitForSingleObject(h, uint32(ms)); r != syscall.WAIT_OBJECT_0 {
			alive = append(alive, w.pids[i])
		}
	}
	return alive
}

func (w *watch) close() {
	for _, h := range w.hs {
		syscall.CloseHandle(h)
	}
}

func hardKill(t *testing.T, cmd *exec.Cmd) { cmd.Process.Kill() } // TerminateProcess

// leftovers: the Toolhelp snapshot keeps a dead parent's pid in ParentProcessID.
func leftovers(t *testing.T, root int, dir string) []int { return descendants(t, root) }
