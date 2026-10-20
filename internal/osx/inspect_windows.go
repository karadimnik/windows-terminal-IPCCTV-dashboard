//go:build windows

package osx

import (
	"fmt"
	"syscall"
)

// OSInspector uses OpenProcess, GetProcessTimes (creation time) and QueryFullProcessImageNameW.
// Stop checks the identity again on the handle it terminates, so a pid reused between the check
// and the kill cannot be hit.
type OSInspector struct{}

func (OSInspector) Inspect(pid int) (ProcInfo, error) {
	if pid <= 0 || pid > 1<<32-1 {
		return ProcInfo{}, ErrNotRunning
	}
	h, err := syscall.OpenProcess(processQueryLimited, false, uint32(pid))
	if err != nil {
		if err == errorInvalidParameter {
			return ProcInfo{}, ErrNotRunning
		}
		return ProcInfo{}, err
	}
	defer syscall.CloseHandle(h)
	if !alive(h) {
		return ProcInfo{}, ErrNotRunning
	}
	start, image, err := processIdentity(h)
	if err != nil {
		return ProcInfo{}, err
	}
	return ProcInfo{Start: start, Image: image}, nil
}

func (OSInspector) Stop(e ChildEntry) error {
	h, err := syscall.OpenProcess(processQueryLimited|processTerminate|synchronize, false, uint32(e.Pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	start, image, err := processIdentity(h)
	if err != nil || !alive(h) || start != e.Start || !sameImage("windows", image, e.Image) {
		return fmt.Errorf("pid %d changed before the kill", e.Pid)
	}
	if err := syscall.TerminateProcess(h, 1); err != nil {
		return err
	}
	syscall.WaitForSingleObject(h, 2000)
	return nil
}
