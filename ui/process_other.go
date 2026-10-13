//go:build !windows

package main

import "syscall"

// camui ships for Windows; these exist so the package builds, vets and tests
// on the WSL side where it is cross-compiled from.

// processAlive reports whether pid is a running process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
