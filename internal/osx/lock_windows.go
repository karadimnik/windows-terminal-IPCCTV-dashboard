//go:build windows

package osx

import (
	"io"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

const (
	lockfileExclusive       = 0x2
	lockfileFailImmediately = 0x1
	errorLockViolation      = syscall.Errno(33)
	// The locked byte lies far beyond the end of the file: Windows byte-range locks are mandatory,
	// so locking the content itself would stop a second process from reading who holds it.
	lockOffsetHigh = 0x40000000
)

// lockSys is LockFileEx (exclusive, fail immediately) on a handle opened with share read/write/
// delete and default security (not inheritable).
type lockSys struct {
	mu sync.Mutex
	h  syscall.Handle
}

func (s *lockSys) overlapped() *syscall.Overlapped {
	return &syscall.Overlapped{OffsetHigh: lockOffsetHigh}
}

func (s *lockSys) lock(path string) (held bool, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return false, err
	}
	r, _, e := procLockFileEx.Call(uintptr(h), lockfileExclusive|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(s.overlapped())))
	if r == 0 {
		syscall.CloseHandle(h)
		if e == errorLockViolation {
			return true, nil
		}
		return false, e
	}
	s.h = h
	return false, nil
}

func (s *lockSys) writeOwner(b []byte) error {
	if _, err := syscall.Seek(s.h, 0, io.SeekStart); err != nil {
		return err
	}
	if err := syscall.SetEndOfFile(s.h); err != nil {
		return err
	}
	var n uint32
	if err := syscall.WriteFile(s.h, b, &n, nil); err != nil {
		return err
	}
	return syscall.FlushFileBuffers(s.h)
}

func (s *lockSys) unlock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.h == 0 {
		return nil
	}
	if _, err := syscall.Seek(s.h, 0, io.SeekStart); err == nil {
		_ = syscall.SetEndOfFile(s.h)
	}
	procUnlockFileEx.Call(uintptr(s.h), 0, 1, 0, uintptr(unsafe.Pointer(s.overlapped())))
	err := syscall.CloseHandle(s.h)
	s.h = 0
	return err
}

// readShared opens with share read/write/delete so it never disturbs the holder.
func readShared(path string) ([]byte, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	return io.ReadAll(f)
}
