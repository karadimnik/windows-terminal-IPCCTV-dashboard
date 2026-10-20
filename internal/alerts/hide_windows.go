package alerts

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
