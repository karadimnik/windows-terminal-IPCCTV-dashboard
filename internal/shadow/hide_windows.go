package shadow

import "syscall"

// createNoWindow is CREATE_NO_WINDOW.
const createNoWindow = 0x08000000

func hiddenWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
