//go:build !windows

package main

import "os"

func openRead(p, mode string) (*os.File, error) { return os.Open(p) }
