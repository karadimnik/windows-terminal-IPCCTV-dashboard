//go:build unix && !linux

package osx

import (
	"errors"
	"syscall"
)

// OSInspector on macOS (and other Unix): the start time and image path need sysctl/proc_pidinfo
// structures that the standard library does not expose, so nothing can be verified and the reap
// leaves every entry alone (logged). Compiled only; nothing was run on a Mac.
type OSInspector struct{}

var errNoInspect = errors.New("process inspection is not implemented on this OS")

func (OSInspector) Inspect(pid int) (ProcInfo, error) {
	if pid <= 0 || syscall.Kill(pid, 0) == syscall.ESRCH {
		return ProcInfo{}, ErrNotRunning
	}
	return ProcInfo{}, errNoInspect
}

func (OSInspector) Stop(ChildEntry) error { return errNoInspect }
