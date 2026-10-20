//go:build linux || darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func termios(f *os.File, req uintptr, t *syscall.Termios) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(unsafe.Pointer(t)))
	if e != 0 {
		return e
	}
	return nil
}

// isTerminal reports whether f is a terminal (a character device such as /dev/null is not).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	return termios(f, ioctlGetTermios, &t) == nil
}

// withoutEcho runs fn with the terminal's echo off and restores it afterwards.
func withoutEcho(f *os.File, fn func() error) error {
	var old syscall.Termios
	if err := termios(f, ioctlGetTermios, &old); err != nil {
		return err
	}
	quiet := old
	quiet.Lflag &^= syscall.ECHO
	if err := termios(f, ioctlSetTermios, &quiet); err != nil {
		return err
	}
	defer termios(f, ioctlSetTermios, &old)
	return fn()
}
