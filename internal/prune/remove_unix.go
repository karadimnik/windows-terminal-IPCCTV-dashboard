//go:build !windows

package prune

import (
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
