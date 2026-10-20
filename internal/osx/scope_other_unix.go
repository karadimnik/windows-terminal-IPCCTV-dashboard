//go:build unix && !linux

package osx

import "syscall"

// macOS has no Pdeathsig: Setpgid only (an orderly stop reaches the whole group; a hard kill of
// this process leaves the children until the next start's registry reap). Compiled only.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// waitNoReap is not available: the group is signalled only until the leader is reaped (a tiny
// window remains in which a reused group id could be hit; documented in README.md).
func waitNoReap(int) bool { return false }
