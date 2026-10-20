//go:build windows

package web

import (
	"io"
	"os"
	"syscall"
)

// openShared opens path for reading with FILE_SHARE_READ|WRITE|DELETE. Go's
// os.Open omits FILE_SHARE_DELETE, and a reader without it makes the
// supervisor's POSIX-semantics rename of status.json fail (spike SP6).
func openShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// readStatusFile reads status.json fully and closes it at once.
func readStatusFile(path string) ([]byte, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
