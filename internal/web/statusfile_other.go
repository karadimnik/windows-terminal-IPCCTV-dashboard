//go:build !windows

package web

import "os"

// readStatusFile reads status.json. An open file never blocks a rename on Unix.
func readStatusFile(path string) ([]byte, error) { return os.ReadFile(path) }
