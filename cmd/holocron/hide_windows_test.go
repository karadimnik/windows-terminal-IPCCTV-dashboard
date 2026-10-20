//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

func hideConsole(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true} // CREATE_NO_WINDOW
}
