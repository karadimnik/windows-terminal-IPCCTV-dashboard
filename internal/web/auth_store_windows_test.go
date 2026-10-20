//go:build windows

package web

import (
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

// assertOwnerOnly reads the ACL back with icacls: inheritance is off and the only
// entry is the current user, the same assertion the TLS key tests make.
func assertOwnerOnly(t *testing.T, path string, isDir bool) {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("icacls.exe", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v: %s", path, err, out)
	}
	text := string(out)
	if strings.Contains(text, "(I)") {
		t.Errorf("%s still has inherited entries:\n%s", path, text)
	}
	entries := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, ":(") {
			entries++
		}
	}
	if entries != 1 || !strings.Contains(text, "(F)") && !strings.Contains(text, "(OI)(CI)(F)") {
		t.Errorf("%s: want exactly one full-control entry for %s:\n%s", path, current.Username, text)
	}
}
