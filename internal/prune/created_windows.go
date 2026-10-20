//go:build windows

package prune

import (
	"io/fs"
	"syscall"
	"time"
)

// created is the NTFS creation time, which Invoke-CamPrune protects the newest segment by.
func created(fi fs.FileInfo) time.Time {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, d.CreationTime.Nanoseconds()).UTC()
	}
	return time.Time{}
}
