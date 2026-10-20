//go:build windows

package relay

import (
	"fmt"
	"os/exec"
	"strings"
)

// resetACL drops every explicit entry on the file (back to what the folder gives it). osx's
// SetOwnerOnly removes inherited entries and grants the owner, but leaves an explicit entry for
// someone else (an Everyone grant, say) in place; a repair needs both steps.
func resetACL(path string) error {
	out, err := exec.Command("icacls.exe", path, "/reset").CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls could not reset %s: %v: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
