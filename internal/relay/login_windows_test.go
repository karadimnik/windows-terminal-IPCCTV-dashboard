//go:build windows

package relay

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/osx"
)

func aclText(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("icacls.exe", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v: %s", path, err, out)
	}
	return string(out)
}

func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	text := aclText(t, path)
	for _, who := range []string{"Everyone", `BUILTIN\Users`, "Authenticated Users", `BUILTIN\Administrators`, `NT AUTHORITY\SYSTEM`} {
		if strings.Contains(text, who) {
			t.Errorf("%s grants %s:\n%s", path, who, text)
		}
	}
	if !strings.Contains(text, "(F)") {
		t.Errorf("%s grants nobody full control:\n%s", path, text)
	}
}

func problems(path string, text string) []string {
	var found []string
	for _, who := range []string{"Everyone", `BUILTIN\Users`, "Authenticated Users", `BUILTIN\Administrators`, `NT AUTHORITY\SYSTEM`} {
		if strings.Contains(text, who) {
			found = append(found, who)
		}
	}
	return found
}

// A created secret file names the current user alone, and a file that was open to others is repaired.
func TestSecretFileAclOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.txt")
	l, err := LoadLiveSecret(osx.Current(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	assertOwnerOnly(t, path)

	// negative control: a plain file in the same folder inherits entries for SYSTEM and Administrators,
	// and one explicitly opened to Everyone shows that name; the check reports both
	open := filepath.Join(t.TempDir(), "open.txt")
	os.WriteFile(open, []byte(l.Secret), 0o644)
	if p := problems(open, aclText(t, open)); len(p) == 0 {
		t.Fatalf("negative control: the inherited ACL went unnoticed:\n%s", aclText(t, open))
	}
	if out, err := exec.Command("icacls.exe", open, "/grant", "*S-1-1-0:(R)").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	found := problems(open, aclText(t, open))
	hasEveryone := false
	for _, f := range found {
		hasEveryone = hasEveryone || f == "Everyone"
	}
	if !hasEveryone {
		t.Fatalf("negative control: Everyone not reported: %v", found)
	}

	// the load repairs it and still returns the stored secret
	again, err := LoadLiveSecret(osx.Current(), open, false)
	if err != nil || again.Secret != l.Secret {
		t.Fatal(again, err)
	}
	assertOwnerOnly(t, open)
}
