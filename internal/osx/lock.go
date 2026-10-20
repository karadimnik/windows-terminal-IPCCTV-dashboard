package osx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LockPath is <state>/supervisor.lock.
func LockPath(stateDir string) string { return filepath.Join(stateDir, "supervisor.lock") }

// ErrHeld: another process holds the instance lock.
var ErrHeld = errors.New("the instance lock is held by another process")

// LockOwner is what the holder writes into the lock file AFTER it holds the lock. It names the
// holder in messages only: ownership is decided by the OS lock, never by this content.
type LockOwner struct {
	Pid     int       `json:"pid"`
	Started time.Time `json:"started"`
	Exe     string    `json:"exe"`
}

func (o LockOwner) String() string {
	if o.Pid == 0 {
		return "an unknown process (the lock file names no owner yet)"
	}
	return fmt.Sprintf("pid %d (started %s, %s)", o.Pid, o.Started.Format(time.RFC3339), o.Exe)
}

// HeldError is the refusal; errors.Is(err, ErrHeld) holds.
type HeldError struct {
	Path  string
	Owner LockOwner
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%s is held by %s", e.Path, e.Owner)
}
func (e *HeldError) Is(target error) bool { return target == ErrHeld }

// InstanceLock is a held lock. The OS releases it when the process dies, however abruptly: there
// is never a stale lock to clean up.
type InstanceLock struct {
	path  string
	owner LockOwner
	sys   lockSys
}

// Owner is the identity this process wrote.
func (l *InstanceLock) Owner() LockOwner { return l.owner }

// Path is the lock file.
func (l *InstanceLock) Path() string { return l.path }

// AcquireInstanceLock takes the exclusive lock on path at once or fails with a *HeldError naming
// the holder (read from the file, informational). The directory is created when missing. The
// handle is not inherited by children.
func AcquireInstanceLock(path string) (*InstanceLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	exe, _ := os.Executable()
	owner := LockOwner{Pid: os.Getpid(), Started: processStart(), Exe: exe}
	l := &InstanceLock{path: path, owner: owner}
	held, err := l.sys.lock(path)
	if held {
		o, _ := ReadLockOwner(path)
		return nil, &HeldError{Path: path, Owner: o}
	}
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(owner)
	if err := l.sys.writeOwner(append(b, '\n')); err != nil {
		l.sys.unlock()
		return nil, fmt.Errorf("write the lock owner: %w", err)
	}
	return l, nil
}

// Release truncates the owner text and releases the lock. Safe to call more than once.
func (l *InstanceLock) Release() error {
	if l == nil {
		return nil
	}
	return l.sys.unlock()
}

// ReadLockOwner reads the owner text of a lock file (informational only).
func ReadLockOwner(path string) (LockOwner, error) {
	b, err := readShared(path)
	if err != nil {
		return LockOwner{}, err
	}
	var o LockOwner
	if strings.TrimSpace(string(b)) == "" {
		return o, nil
	}
	err = json.Unmarshal(b, &o)
	return o, err
}

var startedAt = time.Now()

// processStart is close enough to the process start for a message (the package is initialised at
// start-up).
func processStart() time.Time { return startedAt.Truncate(time.Second) }
