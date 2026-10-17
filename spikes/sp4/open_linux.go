package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// heldOpen reports whether any process of this user has p open (scans /proc/*/fd).
func heldOpen(p string) bool {
	abs, _ := filepath.Abs(p)
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, f := range fds {
		if t, err := os.Readlink(f); err == nil && strings.TrimSuffix(t, " (deleted)") == abs {
			return true
		}
	}
	return false
}

// birth returns the file's birth time from statx(2) (linux/amd64), or says it is not reported.
func birth(p string) string {
	const sysStatx, statxBtime = 332, 0x800
	atFdcwd := ^uintptr(99) // AT_FDCWD = -100
	var buf [256]byte
	path, err := syscall.BytePtrFromString(p)
	if err != nil {
		return ""
	}
	_, _, e := syscall.Syscall6(sysStatx, atFdcwd, uintptr(unsafe.Pointer(path)), 0, statxBtime, uintptr(unsafe.Pointer(&buf[0])), 0)
	if e != 0 {
		return fmt.Sprintf(" statx=%v", e)
	}
	mask := *(*uint32)(unsafe.Pointer(&buf[0]))
	if mask&statxBtime == 0 {
		return " birth=not-reported"
	}
	sec := *(*int64)(unsafe.Pointer(&buf[80]))
	nsec := *(*uint32)(unsafe.Pointer(&buf[88]))
	return " birth=" + time.Unix(sec, int64(nsec)).Format("15:04:05.000")
}
