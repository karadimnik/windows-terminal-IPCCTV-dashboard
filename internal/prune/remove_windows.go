//go:build windows

package prune

import (
	"errors"
	"os"
	"syscall"
)

// removeFile deletes one file; DeleteFile refuses a directory and removes a link, not its target.
func removeFile(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "DeleteFile", Path: path, Err: err}
	}
	if err := syscall.DeleteFile(p); err != nil {
		return &os.PathError{Op: "DeleteFile", Path: path, Err: err}
	}
	return nil
}

// errorDirNotEmpty is ERROR_DIR_NOT_EMPTY.
const errorDirNotEmpty = syscall.Errno(145)

// removeDir removes one empty directory; RemoveDirectory refuses a file and a non-empty directory.
func removeDir(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "RemoveDirectory", Path: path, Err: err}
	}
	if err := syscall.RemoveDirectory(p); err != nil {
		if errors.Is(err, errorDirNotEmpty) {
			return &os.PathError{Op: "RemoveDirectory", Path: path, Err: ErrDirNotEmpty}
		}
		return &os.PathError{Op: "RemoveDirectory", Path: path, Err: err}
	}
	return nil
}
