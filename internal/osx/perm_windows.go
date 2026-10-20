//go:build windows

package osx

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// setOwnerOnly removes every inherited access entry from a file or directory and grants full
// control to the current user alone, by SID. It fails closed. Same technique as ui/fileperm_windows.go.
func setOwnerOnly(path string, isDir bool) error {
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("find the current user: %w", err)
	}
	grant := "*" + current.Uid + ":F"
	if isDir {
		grant = "*" + current.Uid + ":(OI)(CI)F"
	}
	out, err := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r", grant).CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls could not restrict %s: %v: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
