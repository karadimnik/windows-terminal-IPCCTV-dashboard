//go:build windows

package archive

import (
	"io/fs"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func reparseAttr(fi fs.FileInfo) bool {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

var procGetFinalPathNameByHandleW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")

// realPath is where path lands, from the system's own answer for an open handle: symbolic links
// and junctions followed, 8.3 short names expanded. filepath.EvalSymlinks does not do this: since
// Go 1.23 it fails at a junction instead of following it, and it needs to list every ancestor.
func realPath(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]uint16, 512)
	for {
		r, _, e := procGetFinalPathNameByHandleW.Call(f.Fd(), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0) // VOLUME_NAME_DOS
		n := int(uint32(r))
		if n == 0 {
			return "", &fs.PathError{Op: "GetFinalPathNameByHandle", Path: path, Err: e}
		}
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]uint16, n)
	}
	s := syscall.UTF16ToString(buf)
	switch {
	case strings.HasPrefix(s, `\\?\UNC\`):
		return `\\` + s[len(`\\?\UNC\`):], nil
	case strings.HasPrefix(s, `\\?\`):
		return s[len(`\\?\`):], nil
	}
	return s, nil
}
