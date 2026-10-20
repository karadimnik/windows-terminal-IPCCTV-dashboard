//go:build !linux && !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// macOS (and other unix): compiled to prove the code builds; no run was possible. Setpgid only, no Pdeathsig.
func longRunner() []string { return []string{"/bin/sleep", "600"} }
func spawnLong(bool) (int, error) {
	c := exec.Command("/bin/sleep", "600")
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, nil
}
func platCaseFold(a, b string) bool { return a == b }
func platInit(o *opts)              {}
func platRun(o *opts, fn func())    { fn() }
func platWait(o *opts) {
	for {
		time.Sleep(time.Hour)
	}
}
func procInfo(int) (string, string, error) { return "", "", errors.New("not implemented on this OS") }
func killPid(pid int) error                { return syscall.Kill(pid, syscall.SIGKILL) }
func platWrapAttr(cmd *exec.Cmd)           { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func platKillTree(pid int)                 { syscall.Kill(-pid, syscall.SIGKILL) }
func cmdJobWrap([]string)                  {}

func platStart(o *opts, argv []string) (int, error) {
	if o.wrapper {
		argv = append([]string{"sp3", "wrap", "--"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: o.pgid}
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
		o.pgid = cmd.Process.Pid
	}
	return cmd.Process.Pid, nil
}
