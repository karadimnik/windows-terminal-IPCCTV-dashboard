//go:build windows

package ctlapi

import (
	"os/exec"
	"strings"
	"testing"
)

// assertOwnerOnlyOS reads the ACL with icacls: no group or other principal, one full-control entry.
func assertOwnerOnlyOS(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("icacls.exe", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v: %s", path, err, out)
	}
	text := string(out)
	for _, who := range []string{"Everyone", `BUILTIN\Users`, "Authenticated Users", `BUILTIN\Administrators`, `NT AUTHORITY\SYSTEM`} {
		if strings.Contains(text, who) {
			t.Errorf("%s grants %s:\n%s", path, who, text)
		}
	}
	if !strings.Contains(text, "(F)") {
		t.Errorf("%s grants nobody full control:\n%s", path, text)
	}
	t.Logf("ACL of %s:\n%s", path, text)
}
