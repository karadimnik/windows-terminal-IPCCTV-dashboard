//go:build windows

package archive

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// renameNoReplace renames src to dst on one volume and fails when dst exists. It opens src itself
// (the entry, never a link's target) with DELETE access and without FILE_SHARE_DELETE, then renames
// that handle with SetFileInformationByHandle(FileRenameInfo, ReplaceIfExists false): an existing
// dst fails with ERROR_ALREADY_EXISTS (fs.ErrExist), and the check and the rename are one call.
// Neither os.Rename (it replaces an existing dst) nor MoveFileW is used: MoveFileW also opens and
// renames, but shares delete, so two movers that both opened src before either renamed it both
// succeeded (the second renamed the file to the name it already had) and both reported the move.
// Holding delete access exclusively makes the second opener fail (a sharing violation while the
// first holds it, not found after), so exactly one caller succeeds.
func renameNoReplace(src, dst string) error {
	from, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16FromString(dst)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(from, accessDelete, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	defer syscall.CloseHandle(h)
	var hdr fileRenameInfo
	name := unsafe.Offsetof(hdr.FileName)
	buf := make([]byte, name+uintptr(len(to))*2)
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.FileNameLength = uint32(len(to)-1) * 2 // bytes, without the terminating NUL
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[name])), len(to)), to)
	r, _, e := procSetFileInformationByHandle.Call(uintptr(h), fileRenameInfoClass, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: e}
	}
	return nil
}

const (
	accessDelete        = 0x00010000 // DELETE
	fileRenameInfoClass = 3          // FileRenameInfo
)

// fileRenameInfo is FILE_RENAME_INFO; FileName runs on past the struct.
type fileRenameInfo struct {
	ReplaceIfExists uint32 // BOOLEAN in a union with a DWORD of flags
	RootDirectory   syscall.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

var procSetFileInformationByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

const (
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

// isBusy: another process holds the file (opened without FILE_SHARE_DELETE, or locked), so the
// rename was refused; a later try can succeed.
func isBusy(err error) bool {
	return errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation) || errors.Is(err, syscall.ERROR_ACCESS_DENIED)
}
