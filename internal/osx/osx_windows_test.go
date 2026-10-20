//go:build windows

package osx

import (
	"os"
	"os/exec"
	"os/user"
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

func icaclsText(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("icacls.exe", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v: %s", path, err, out)
	}
	return string(out)
}

// An explicit Everyone grant is dropped, and nothing but the current user remains, with no
// inherited entry. Negative control: the old two-flag call (no /reset) keeps the Everyone entry.
func TestSetOwnerOnlyDropsExplicitGrants(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mk := func(name string) string {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("icacls.exe", f, "/grant", "*S-1-1-0:R").CombinedOutput(); err != nil {
			t.Fatalf("grant Everyone: %v: %s", err, out)
		}
		if !strings.Contains(icaclsText(t, f), "Everyone") {
			t.Fatalf("setup failed: no Everyone entry on %s", f)
		}
		return f
	}

	// control: skipping the reset step leaves the Everyone entry
	skipped := mk("skipped.txt")
	for _, args := range ownerOnlyICACLS(skipped, cur.Uid, false)[1:] {
		if out, err := exec.Command("icacls.exe", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if !strings.Contains(icaclsText(t, skipped), "Everyone") {
		t.Fatal("negative control did not leave the Everyone entry: the test cannot fail")
	}

	f := mk("fixed.txt")
	if err := Current().SetOwnerOnly(f); err != nil {
		t.Fatal(err)
	}
	text := icaclsText(t, f)
	if strings.Contains(text, "Everyone") {
		t.Errorf("Everyone survived SetOwnerOnly:\n%s", text)
	}
	var entries int
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, ":(") {
			entries++
			if strings.Contains(line, "(I)") {
				t.Errorf("inherited entry remains: %s", line)
			}
			if !strings.Contains(line, `\`+cur.Username[strings.LastIndex(cur.Username, `\`)+1:]) {
				t.Errorf("entry for someone else: %s", line)
			}
		}
	}
	if entries != 1 {
		t.Errorf("want exactly 1 entry, got %d:\n%s", entries, text)
	}
}
