//go:build windows

package prune

import (
	"syscall"
	"time"
)

// setCreated sets a file's NTFS creation time, as Set-CamTestFileAge does.
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
