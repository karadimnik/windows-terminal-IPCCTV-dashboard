//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func longRunner() []string { return []string{"/usr/bin/sleep", "600"} }

func spawnLong(bool) (int, error) {
	c := exec.Command("/usr/bin/sleep", "600")
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, nil
}

func platCaseFold(a, b string) bool { return a == b }

func platInit(o *opts) {}

// platRun decides on WHICH goroutine/thread the children are created: the creating THREAD is what Pdeathsig is bound to.
func platRun(o *opts, fn func()) {
	switch o.mode {
	case "locked": // the main goroutine owns its thread for the whole life of the supervisor
		runtime.LockOSThread()
		fn()
	case "unlocked": // a plain goroutine, no lock
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
		wg.Wait()
	case "lockexit": // PITFALL: the starter locks its thread and returns while holding the lock: the runtime ends the thread
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); runtime.LockOSThread(); fn() }()
		wg.Wait()
	case "lockexitunlock": // control: same, but unlocked before returning, so the thread goes back to the pool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); runtime.LockOSThread(); fn(); runtime.UnlockOSThread() }()
		wg.Wait()
	default:
		fn()
	}
}

func platStart(o *opts, argv []string) (int, error) {
	if o.wrapper {
		self, _ := os.Executable()
		argv = append([]string{self, "wrap", "--"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	sa := &syscall.SysProcAttr{Setpgid: true, Pgid: o.pgid}
	if o.mode != "none" {
		sa.Pdeathsig = syscall.SIGKILL
	}
	cmd.SysProcAttr = sa
	if o.wrapper {
		w, err := cmd.StdinPipe()
		if err != nil {
			return 0, err
		}
		o.pipes = append(o.pipes, w)
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if o.pgid == 0 {
		o.pgid = cmd.Process.Pid // first child leads the shared group
	}
	pid := cmd.Process.Pid
	go func() { // reap, and show it in the log when a child dies while the supervisor lives
		cmd.Wait()
		o.log("child pid=%d exited: %v", pid, cmd.ProcessState)
	}()
	return pid, nil
}

// platWait blocks; SIGTERM = orderly stop: group TERM, short grace, group KILL.
func platWait(o *opts) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	<-c
	o.log("orderly stop: kill(-%d, SIGTERM)", o.pgid)
	syscall.Kill(-o.pgid, syscall.SIGTERM)
	time.Sleep(1500 * time.Millisecond)
	syscall.Kill(-o.pgid, syscall.SIGKILL)
	o.log("orderly stop: kill(-%d, SIGKILL) sent", o.pgid)
}

func bootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// procInfo returns a start-time token (boot id + clock ticks since boot) and the image path; error if the pid is gone or a zombie.
func procInfo(pid int) (string, string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", err
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	if f[0] == "Z" || f[0] == "X" {
		return "", "", fmt.Errorf("zombie")
	}
	ticks, _ := strconv.ParseUint(f[19], 10, 64)
	img, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("%s:%d", bootID(), ticks), img, nil
}

func killPid(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

func platWrapAttr(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func platKillTree(pid int) { syscall.Kill(-pid, syscall.SIGKILL) }

func cmdJobWrap([]string) { fmt.Println("jobwrap is windows only") }
