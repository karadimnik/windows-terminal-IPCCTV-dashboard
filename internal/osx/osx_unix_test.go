//go:build linux || darwin

package osx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetOwnerOnlyUnix(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "secret")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := Current()
	if err := p.SetOwnerOnly(f); err != nil {
		t.Fatal(err)
	}
	if err := p.SetOwnerOnly(sub); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(sub); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	if err := p.SetOwnerOnly(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing path must fail")
	}
}
