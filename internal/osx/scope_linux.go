//go:build linux

package osx

import (
	"syscall"
	"unsafe"
)

func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

const (
	pPID    = 1
	wExited = 0x4
	wNoWait = 0x1000000
)

// waitNoReap blocks until the child is waitable (exited, a zombie) without reaping it.
func waitNoReap(pid int) bool {
	var info [128]byte // siginfo_t
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), wExited|wNoWait, 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		return errno == 0
	}
}
