//go:build !windows

package status

import (
	"io/fs"
	"os"
)

var errNotExist = fs.ErrNotExist

func readFileShared(path string) ([]byte, error) { return os.ReadFile(path) }

// isSharingError: file sharing does not block readers or the rename on Unix.
func isSharingError(error) bool { return false }

// openShared is os.Open on Unix (an open file never blocks a rename).
func openShared(path string) (*os.File, error) { return os.Open(path) }
