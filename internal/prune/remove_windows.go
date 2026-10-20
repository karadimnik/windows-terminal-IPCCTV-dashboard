//go:build windows

package prune

import (
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
