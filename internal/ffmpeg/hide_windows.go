package ffmpeg

import "syscall"

// createNoWindow is CREATE_NO_WINDOW (CreateNoWindow = true in the PowerShell product).
const createNoWindow = 0x08000000

func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
