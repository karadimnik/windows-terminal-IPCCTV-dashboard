package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// heldOpen reports whether another handle excludes a share-none read open (ffmpeg holds its segment).
func heldOpen(p string) bool {
	u, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return false
	}
	h, err := syscall.CreateFile(u, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err == syscall.Errno(32)
	}
	syscall.CloseHandle(h)
	return false
}

func birth(p string) string {
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return ""
	}
	return fmt.Sprintf(" created=%s", time.Unix(0, d.CreationTime.Nanoseconds()).Format("15:04:05.000"))
}
