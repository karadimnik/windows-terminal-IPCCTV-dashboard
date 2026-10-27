//go:build !windows

package prune

import (
	"errors"
	"os"
	"syscall"
)

// removeFile unlinks one entry; unlink(2) refuses a directory, so a folder is never removed.
func removeFile(path string) error {
	if err := syscall.Unlink(path); err != nil {
		return &os.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}

// removeDir removes one empty directory; rmdir(2) refuses a file, a link and a non-empty directory.
func removeDir(path string) error {
	if err := syscall.Rmdir(path); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			err = ErrDirNotEmpty
		}
		return &os.PathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}
