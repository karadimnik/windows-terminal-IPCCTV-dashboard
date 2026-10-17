//go:build !windows

package main

import "os"

func renameOver(oldpath, newpath, mode string) error { return os.Rename(oldpath, newpath) }
