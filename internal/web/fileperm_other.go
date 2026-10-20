//go:build !windows

package web

import "os"

// restrictToOwner makes a file (0600) or directory (0700) accessible to its owner only.
func restrictToOwner(path string, isDir bool) error {
	if isDir {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}
