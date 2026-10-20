//go:build windows

package status

import (
	"math"
	"syscall"
	"time"
)

// OSProcesses inspects real processes through the Win32 API.
type OSProcesses struct{}

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// Alive opens the process, requires STILL_ACTIVE (a handle can outlive its
// process) and reads its creation time. A process that exists but cannot be
// opened (access denied) is alive with an unknown start time.
func (OSProcesses) Alive(pid int64) (time.Time, bool) {
	if pid <= 0 || pid > math.MaxUint32 {
		return time.Time{}, false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return time.Time{}, err == errAccessDenied
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil || code != stillActive {
		return time.Time{}, false
	}
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return time.Time{}, true
	}
	return time.Unix(0, created.Nanoseconds()), true
}
