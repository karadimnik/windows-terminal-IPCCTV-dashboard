//go:build !windows

package shadow

import "syscall"

func hiddenWindow() *syscall.SysProcAttr { return nil }
