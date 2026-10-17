package main

import (
	"os"
	"syscall"
)

// openRead opens p for reading. "sd" adds FILE_SHARE_DELETE; "go" is plain os.Open.
func openRead(p, mode string) (*os.File, error) {
	if mode != "sd" {
		return os.Open(p)
	}
	u, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(u, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return os.NewFile(uintptr(h), p), nil
}
