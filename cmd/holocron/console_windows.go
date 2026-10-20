package main

import (
	"os"
	"syscall"
)

const enableEchoInput = 0x0004

var setConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

// isTerminal reports whether f is a console.
func isTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}

// withoutEcho runs fn with the console's echo off and restores it afterwards.
func withoutEcho(f *os.File, fn func() error) error {
	h := syscall.Handle(f.Fd())
	var old uint32
	if err := syscall.GetConsoleMode(h, &old); err != nil {
		return err
	}
	if r, _, err := setConsoleMode.Call(uintptr(h), uintptr(old&^enableEchoInput)); r == 0 {
		return err
	}
	defer setConsoleMode.Call(uintptr(h), uintptr(old))
	return fn()
}
