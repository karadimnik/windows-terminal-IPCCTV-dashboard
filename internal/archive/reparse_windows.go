//go:build windows

package archive

import (
	"io/fs"
	"syscall"
)

func reparseAttr(fi fs.FileInfo) bool {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
