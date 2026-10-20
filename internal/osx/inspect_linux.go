//go:build linux

package osx

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// OSInspector reads /proc: start token <boot_id>:<starttime ticks> (stat field 22), image
// readlink /proc/<pid>/exe, group stat field 5. A zombie is not running.
type OSInspector struct{}

var bootID = sync.OnceValue(func() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
})

func procStat(pid int) (state string, pgrp int, ticks string, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, "", ErrNotRunning
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", 0, "", fmt.Errorf("pid %d: malformed stat", pid)
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3
	if len(f) < 20 {
		return "", 0, "", fmt.Errorf("pid %d: short stat", pid)
	}
	if f[0] == "Z" || f[0] == "X" {
		return f[0], 0, "", ErrNotRunning
	}
	pgrp, _ = strconv.Atoi(f[2])
	return f[0], pgrp, f[19], nil
}

func startToken(ticks string) string { return bootID() + ":" + ticks }

func (OSInspector) Inspect(pid int) (ProcInfo, error) {
	if pid <= 0 {
		return ProcInfo{}, ErrNotRunning
	}
	_, pgrp, ticks, err := procStat(pid)
	if err != nil {
		return ProcInfo{}, err
	}
	img, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ProcInfo{}, err
	}
	return ProcInfo{Start: startToken(ticks), Image: img, Group: pgrp}, nil
}

const (
	sysPidfdOpen       = 434
	sysPidfdSendSignal = 424
)

// Stop opens a pidfd (which pins the process), checks the start token again through /proc, then
// kills the group (when recorded) or the process through the pidfd.
func (OSInspector) Stop(e ChildEntry) error {
	fd, _, errno := syscall.Syscall(sysPidfdOpen, uintptr(e.Pid), 0, 0)
	if errno != 0 {
		return fmt.Errorf("pidfd_open %d: %w", e.Pid, errno)
	}
	defer syscall.Close(int(fd))
	_, _, ticks, err := procStat(e.Pid)
	if err != nil || startToken(ticks) != e.Start {
		return fmt.Errorf("pid %d changed before the kill", e.Pid)
	}
	if e.Group > 0 {
		return syscall.Kill(-e.Group, syscall.SIGKILL)
	}
	if _, _, errno := syscall.Syscall6(sysPidfdSendSignal, fd, uintptr(syscall.SIGKILL), 0, 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
