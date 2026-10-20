//go:build unix

package osx

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func goos() string { return runtime.GOOS }

func longCmd() Cmd { return Cmd{Path: "/usr/bin/sleep", Args: []string{"600"}, Role: "sleep"} }

// spawnLong starts a grandchild the plain way: it stays in its parent's process group.
func spawnLong() (int, error) {
	c := exec.Command("/usr/bin/sleep", "600")
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, nil
}

// spawnUnbound: no Pdeathsig, its own group (what an unprotected supervisor leaves behind).
func spawnUnbound() (int, error) {
	c := exec.Command("/usr/bin/sleep", "600")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, nil
}

func helperAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func consoleWindow() uintptr { return 0 }

// descendants lists every process below root (ppid links in /proc).
func descendants(t *testing.T, root int) []int {
	parent := map[int]int{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) > 1 {
			parent[pid], _ = strconv.Atoi(f[1])
		}
	}
	var out []int
	for pid := range parent {
		for p, n := parent[pid], 0; p != 0 && n < 64; p, n = parent[p], n+1 {
			if p == root {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// withToken lists live processes whose environment carries the token.
func withToken(token string) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err == nil && strings.Contains(string(b), token) {
			if _, err := (OSInspector{}).Inspect(pid); err == nil {
				out = append(out, pid)
			}
		}
	}
	return out
}

// watch records identities now; survivors waits up to within for each to be gone.
type watch struct{ entries []ChildEntry }

func newWatch(t *testing.T, pids []int) *watch {
	w := &watch{}
	for _, pid := range pids {
		if info, err := (OSInspector{}).Inspect(pid); err == nil {
			w.entries = append(w.entries, ChildEntry{Pid: pid, Start: info.Start, Image: info.Image})
		}
	}
	return w
}

func (w *watch) survivors(within time.Duration) []int {
	deadline := time.Now().Add(within)
	var alive []int
	for {
		alive = alive[:0]
		for _, e := range w.entries {
			if info, err := (OSInspector{}).Inspect(e.Pid); err == nil && info.Start == e.Start {
				alive = append(alive, e.Pid)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			return append([]int(nil), alive...)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (w *watch) close() {}

func hardKill(t *testing.T, cmd *exec.Cmd) { cmd.Process.Signal(syscall.SIGKILL) }

// leftovers: every live process carrying the helper's scratch dir in its environment (children
// inherit it), so a child reparented to init after the kill is still found.
func leftovers(t *testing.T, root int, dir string) []int { return withToken("HOLOCRON_OSX_DIR=" + dir) }
