//go:build windows

package main

import (
	"syscall"
	"time"
)

// setCreated sets a file's NTFS creation time (as internal/prune's test helper does). The pruner
// protects the latest created segment of a folder on Windows, so a fixture written in any order
// must carry the creation times ffmpeg's files have: their start.
func setCreated(path string, t time.Time) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	const fileWriteAttributes = 0x0100
	h, err := syscall.CreateFile(p, fileWriteAttributes, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	ft := syscall.NsecToFiletime(t.UnixNano())
	return syscall.SetFileTime(h, &ft, nil, nil)
}
