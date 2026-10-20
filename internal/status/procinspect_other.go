//go:build unix && !linux

package status

import (
	"syscall"
	"time"
)

// OSProcesses inspects real processes with kill(pid, 0); the start time is
// unknown here, so Verdict does not check it.
type OSProcesses struct{}

func (OSProcesses) Alive(pid int64) (time.Time, bool) {
	if pid <= 0 || pid > 1<<31-1 {
		return time.Time{}, false
	}
	err := syscall.Kill(int(pid), 0)
	return time.Time{}, err == nil || err == syscall.EPERM
}
