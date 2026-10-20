//go:build !windows

package ffmpeg

import "syscall"

func hideWindow() *syscall.SysProcAttr { return nil }
