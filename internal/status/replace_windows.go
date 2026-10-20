//go:build windows

package status

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	fileRenameInfoEx              = 22
	fileRenameFlagReplaceIfExists = 0x1
	fileRenameFlagPosixSemantics  = 0x2
	deleteAccess                  = 0x00010000

	errInvalidParameter syscall.Errno = 87 // \\wsl.localhost (9P)
	errNotSupported     syscall.Errno = 50 // some network shares
)

var procSetFileInformationByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

// replaceFile replaces path with tmp using POSIX rename semantics, which succeeds
// while readers hold path open with FILE_SHARE_DELETE (SP6). Where that is not
// supported it falls back to os.Rename (MoveFileEx). Never ReplaceFileW: readers
// would see the file missing.
func replaceFile(tmp, path string) (fallback bool, err error) {
	err = renamePosix(tmp, path)
	var e syscall.Errno
	if err != nil && errors.As(err, &e) && (e == errInvalidParameter || e == errNotSupported) || errors.Is(err, errNoRenameAPI) {
		return true, os.Rename(tmp, path)
	}
	return false, err
}

var errNoRenameAPI = errors.New("SetFileInformationByHandle not available")

// fileRenameInfo is FILE_RENAME_INFO (the Ex flags share the layout).
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  uintptr
	FileNameLength uint32
	FileName       [1]uint16
}

func renamePosix(oldpath, newpath string) error {
	if err := procSetFileInformationByHandle.Find(); err != nil {
		return errNoRenameAPI
	}
	abs, err := filepath.Abs(newpath)
	if err != nil {
		return err
	}
	name, err := syscall.UTF16FromString(abs)
	if err != nil {
		return err
	}
	name = name[:len(name)-1] // the length field counts bytes without the NUL
	op, err := syscall.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(op, deleteAccess|syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return &os.LinkError{Op: "open-for-rename", Old: oldpath, New: newpath, Err: err}
	}
	defer syscall.CloseHandle(h)

	nameOff := unsafe.Offsetof(fileRenameInfo{}.FileName)
	size := nameOff + uintptr(2*len(name)) + 2
	buf := make([]uint64, (size+7)/8) // 8-byte aligned
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = fileRenameFlagReplaceIfExists | fileRenameFlagPosixSemantics
	info.FileNameLength = uint32(2 * len(name))
	dst := unsafe.Slice((*uint16)(unsafe.Add(unsafe.Pointer(info), nameOff)), len(name))
	copy(dst, name)
	r, _, e := procSetFileInformationByHandle.Call(uintptr(h), fileRenameInfoEx, uintptr(unsafe.Pointer(info)), size)
	if r == 0 {
		return &os.LinkError{Op: "rename-posix", Old: oldpath, New: newpath, Err: e}
	}
	return nil
}
