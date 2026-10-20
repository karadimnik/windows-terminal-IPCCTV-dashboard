//go:build !windows

package app

import (
	"os/exec"
	"syscall"
)

// Detach starts exe with args in a new session (Setsid): no controlling terminal, so closing the
// terminal sends it nothing. stdin, stdout and stderr are the null device.
func Detach(exe string, args []string) (pid int, note string, err error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, "", err
	}
	pid = cmd.Process.Pid
	go cmd.Wait() // reap it if it ends while this process still runs
	return pid, "", nil
}
