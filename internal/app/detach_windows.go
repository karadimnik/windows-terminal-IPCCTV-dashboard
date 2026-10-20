//go:build windows

package app

import (
	"os/exec"
	"syscall"
)

const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
	createNoWindow         = 0x08000000
)

// Detach starts exe with args as a background process that is not tied to this terminal: no
// console (DETACHED_PROCESS | CREATE_NO_WINDOW), its own process group, and outside the terminal's
// job (CREATE_BREAKAWAY_FROM_JOB). A job that refuses breakaway is reported in note and the start is
// retried inside it.
func Detach(exe string, args []string) (pid int, note string, err error) {
	start := func(flags uint32) (int, error) {
		cmd := exec.Command(exe, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		pid := cmd.Process.Pid
		_ = cmd.Process.Release()
		return pid, nil
	}
	base := uint32(detachedProcess | createNewProcessGroup | createNoWindow)
	if pid, err = start(base | createBreakawayFromJob); err == nil {
		return pid, "", nil
	}
	pid, err = start(base)
	if err == nil {
		note = "the terminal's job does not allow breakaway: the background copy stays in it and may end with the terminal"
	}
	return pid, note, err
}
