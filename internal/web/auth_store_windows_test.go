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
	// The current user must hold full control. SYSTEM and BUILTIN\Administrators may also appear: an
	// elevated administrator's token (the hosted CI runner) makes them explicit entries, and an
	// administrator can take ownership anyway. Any other principal (Users, Everyone, Authenticated
	// Users, another account) fails the check.
	ownEntries, otherEntries := 0, 0
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, ":(") {
			continue
		}
		switch {
		case strings.Contains(line, current.Username+":("):
			ownEntries++
		case strings.Contains(line, "NT AUTHORITY\\SYSTEM:(") || strings.Contains(line, "BUILTIN\\Administrators:("):
		default:
			otherEntries++
		}
	}
	if ownEntries != 1 || otherEntries != 0 || !strings.Contains(text, "(F)") && !strings.Contains(text, "(OI)(CI)(F)") {
		t.Errorf("%s: want one full-control entry for %s and no other account besides SYSTEM and Administrators:\n%s", path, current.Username, text)
	}
}
