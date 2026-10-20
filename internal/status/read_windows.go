//go:build windows

package status

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
)

var errNotExist = fs.ErrNotExist

// openShared opens path for reading with FILE_SHARE_READ|WRITE|DELETE. Go's
// os.Open omits FILE_SHARE_DELETE, and a reader without it makes the writer's
// rename fail (SP6).
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

func readFileShared(path string) ([]byte, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

const (
	errAccessDenied     syscall.Errno = 5
	errSharingViolation syscall.Errno = 32
	errLockViolation    syscall.Errno = 33
)

func isSharingError(err error) bool {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return false
	}
	return e == errAccessDenied || e == errSharingViolation || e == errLockViolation
}
