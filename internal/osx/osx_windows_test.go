//go:build windows

package osx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The ACL of a file and of a folder after SetOwnerOnly names no group other than the owner.
func TestSetOwnerOnlyWindows(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := Current()
	for _, path := range []string{f, sub} {
		if err := p.SetOwnerOnly(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out, err := exec.Command("icacls.exe", path).CombinedOutput()
		if err != nil {
			t.Fatalf("icacls %s: %v: %s", path, err, out)
		}
		text := string(out)
		for _, who := range []string{"Everyone", `BUILTIN\Users`, "Authenticated Users", "BUILTIN\\Administrators", "NT AUTHORITY\\SYSTEM"} {
			if strings.Contains(text, who) {
				t.Errorf("%s still grants %s:\n%s", path, who, text)
			}
		}
	}
	if _, err := os.ReadFile(f); err != nil {
		t.Errorf("owner lost access: %v", err)
	}
	if err := p.SetOwnerOnly(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing path must fail")
	}
}
