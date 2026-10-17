//go:build linux || darwin

package osx

import "os"

func setOwnerOnly(path string, isDir bool) error {
	if isDir {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}
