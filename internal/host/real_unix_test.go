//go:build unix

package host

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func consoleHwnd() uintptr { return 0 }

func hideAttr(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// G1/G2/G3 on Unix: the terminal's hangup (SIGHUP to the attached host) stops everything in order,
// records window-closed, raises one 103 and restarts nothing.
func TestRealHostHangupIsWindowClose(t *testing.T) {
	needReal(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "_state")
	cmd, done := startHostHelper(t, dir, state, hideAttr)
	k := &tracker{}
	defer k.sweep(t)
	waitRunning(t, state, k)
	t0 := time.Now()
	cmd.Process.Signal(syscall.SIGHUP)
	checkWindowClosed(t, dir, state, cmd, done, k, 6*time.Second)
	t.Logf("hangup to exit %v", time.Since(t0))
}
