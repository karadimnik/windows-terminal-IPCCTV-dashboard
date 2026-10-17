package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procSetFileInfoByHandle = kernel32.NewProc("SetFileInformationByHandle")
	procReplaceFileW        = kernel32.NewProc("ReplaceFileW")
	errNeedWin10            = fmt.Errorf("SetFileInformationByHandle(FileRenameInfoEx) not available")
)

const (
	fileRenameInfoEx              = 22
	fileRenameFlagReplaceIfExists = 0x1
	fileRenameFlagPosixSemantics  = 0x2
	deleteAccess                  = 0x00010000
)

// renameOver replaces newpath with oldpath. mode: "mfe" = os.Rename (MoveFileEx REPLACE_EXISTING, what Go does),
// "posix" = FileRenameInfoEx with REPLACE_IF_EXISTS|POSIX_SEMANTICS, "replacefile" = ReplaceFileW.
func renameOver(oldpath, newpath, mode string) error {
	switch mode {
	case "posix":
		return renamePosix(oldpath, newpath)
	case "replacefile":
		if _, err := os.Stat(newpath); err != nil {
			return os.Rename(oldpath, newpath) // ReplaceFileW needs an existing target
		}
		a, _ := syscall.UTF16PtrFromString(newpath)
		b, _ := syscall.UTF16PtrFromString(oldpath)
		r, _, e := procReplaceFileW.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)), 0, 0, 0, 0)
		if r == 0 {
			return &os.LinkError{Op: "ReplaceFile", Old: oldpath, New: newpath, Err: e}
		}
		return nil
	}
	return os.Rename(oldpath, newpath)
}

func renamePosix(oldpath, newpath string) error {
	abs, err := filepath.Abs(newpath)
	if err != nil {
		return err
	}
	name, err := syscall.UTF16FromString(abs)
	if err != nil {
		return err
	}
	name = name[:len(name)-1] // drop the terminating NUL; the length field counts bytes
	op, _ := syscall.UTF16PtrFromString(oldpath)
	h, err := syscall.CreateFile(op, deleteAccess|syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return &os.LinkError{Op: "open-for-rename", Old: oldpath, New: newpath, Err: err}
	}
	defer syscall.CloseHandle(h)
	// FILE_RENAME_INFO: Flags uint32 @0, RootDirectory HANDLE @8, FileNameLength uint32 @16, FileName WCHAR[] @20.
	buf := make([]byte, 20+2*len(name)+2)
	*(*uint32)(unsafe.Pointer(&buf[0])) = fileRenameFlagReplaceIfExists | fileRenameFlagPosixSemantics
	*(*uint32)(unsafe.Pointer(&buf[16])) = uint32(2 * len(name))
	for i, c := range name {
		*(*uint16)(unsafe.Pointer(&buf[20+2*i])) = c
	}
	r, _, e := procSetFileInfoByHandle.Call(uintptr(h), fileRenameInfoEx, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return &os.LinkError{Op: "rename-posix", Old: oldpath, New: newpath, Err: e}
	}
	return nil
}
