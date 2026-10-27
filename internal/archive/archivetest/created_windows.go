//go:build windows

package archivetest

import (
	"os/exec"
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

// junction makes a directory junction (mklink /J needs no privilege, unlike a symbolic link).
func junction(link, target string) error {
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		return &linkError{err: err, out: string(out)}
	}
	return nil
}
