//go:build unix

package osx

import (
	"os"
	"sync"
	"syscall"
)

// lockSys is flock(LOCK_EX|LOCK_NB) on the open file (O_CLOEXEC: never inherited).
type lockSys struct {
	mu sync.Mutex
	f  *os.File
}

func (s *lockSys) lock(path string) (held bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return true, nil
		}
		return false, err
	}
	s.f = f
	return false, nil
}

func (s *lockSys) writeOwner(b []byte) error {
	if err := s.f.Truncate(0); err != nil {
		return err
	}
	if _, err := s.f.WriteAt(b, 0); err != nil {
		return err
	}
	return s.f.Sync()
}

func (s *lockSys) unlock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	_ = s.f.Truncate(0)
	err := s.f.Close() // closing the last descriptor releases the flock
	s.f = nil
	return err
}

func readShared(path string) ([]byte, error) { return os.ReadFile(path) }
