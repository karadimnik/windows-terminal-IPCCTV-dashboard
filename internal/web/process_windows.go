//go:build windows

package web

import "syscall"

// processQueryLimitedInformation is the least access that answers "does this
// PID exist and has it exited". PROCESS_QUERY_INFORMATION would also work and
// asks for more than is needed.
const processQueryLimitedInformation = 0x1000

// stillActive is STILL_ACTIVE: the exit code Windows reports for a process
// that has not exited. A handle stays openable after a process exits for as
// long as anyone holds one, so OpenProcess succeeding is not enough on its own.
const stillActive = 259

// processAlive reports whether pid is a running process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)

	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}
