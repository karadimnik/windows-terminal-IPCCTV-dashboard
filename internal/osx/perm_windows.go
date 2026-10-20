//go:build windows

package osx

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// setOwnerOnly resets a file or directory to a clean ACL (no explicit entries), removes every
// inherited entry and grants full control to the current user alone, by SID. It fails closed.
// Same technique as ui/fileperm_windows.go, plus the reset that drops explicit grants to others.
func setOwnerOnly(path string, isDir bool) error {
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("find the current user: %w", err)
	}
	for _, args := range ownerOnlyICACLS(path, current.Uid, isDir) {
		out, err := exec.Command("icacls.exe", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("icacls could not restrict %s: %v: %s", path, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
