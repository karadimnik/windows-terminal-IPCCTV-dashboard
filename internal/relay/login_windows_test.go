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

// A created secret file names the current user alone, and a file that was open to others is repaired.
func TestSecretFileAclOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.txt")
	l, err := LoadLiveSecret(osx.Current(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	assertOwnerOnly(t, path)

	// negative control: the same check fails on a file with the inherited (open) ACL
	open := filepath.Join(t.TempDir(), "open.txt")
	os.WriteFile(open, []byte(l.Secret), 0o644)
	if out := aclText(t, open); !strings.Contains(out, `BUILTIN\Users`) && !strings.Contains(out, "Everyone") && !strings.Contains(out, "Authenticated Users") {
		t.Skipf("the temp folder gives other accounts no access, so the repair cannot be shown here:\n%s", out)
	}
	again, err := LoadLiveSecret(osx.Current(), open, false)
	if err != nil || again.Secret != l.Secret {
		t.Fatal(again, err)
	}
	assertOwnerOnly(t, open)
}
