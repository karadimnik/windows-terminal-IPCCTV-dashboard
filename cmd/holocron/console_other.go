//go:build !linux && !darwin && !windows

package main

import (
	"errors"
	"os"
)

// No console-mode API is wired for this OS: a character device counts as a terminal and is
// refused, so a secret is only ever read from a pipe or a file.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0 && f.Name() != os.DevNull
}

func withoutEcho(f *os.File, fn func() error) error {
	return errors.New("cannot turn off echo on this OS: pipe the value into standard input")
}
