//go:build windows

package archive

import (
	"errors"
	"os"
	"syscall"
)

// renameNoReplace renames src to dst on one volume and fails when dst exists (MoveFileW without
// MOVEFILE_REPLACE_EXISTING: ERROR_ALREADY_EXISTS, which matches fs.ErrExist). os.Rename is not
// used because on Windows it replaces an existing dst. The check and the rename are one call, so
// there is no window in which another file could appear at dst and be replaced.
func renameNoReplace(src, dst string) error {
	from, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	if err := syscall.MoveFile(from, to); err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}

const (
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

// isBusy: another process holds the file (opened without FILE_SHARE_DELETE, or locked), so the
// rename was refused; a later try can succeed.
func isBusy(err error) bool {
	return errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation) || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
