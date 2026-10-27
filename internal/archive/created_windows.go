//go:build windows

package archive

import (
	"io/fs"
	"syscall"
	"time"
)

// CreatedTime is the NTFS creation time of a file (from os.Lstat, os.Stat or DirEntry.Info), which
// Invoke-CamPrune protects the newest segment by; zero when the information carries none.
func CreatedTime(fi fs.FileInfo) time.Time {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, d.CreationTime.Nanoseconds()).UTC()
	}
	return time.Time{}
}
