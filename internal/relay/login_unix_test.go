//go:build !windows

package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/osx"
)

func TestSecretFileModesOnUnix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "newdir", "secret.txt")
	if _, err := LoadAPICredential(osx.Current(), path, true); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("created folder mode %v", fi.Mode().Perm())
	}

	// a file anyone can read is repaired, and its secret is still the stored one
	os.WriteFile(path, []byte(strings.Repeat("cd", 32)), 0o644)
	os.Chmod(path, 0o644)
	l, err := LoadAPICredential(osx.Current(), path, false)
	if err != nil || l.Secret != strings.Repeat("cd", 32) {
		t.Fatalf("%v %v", l, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("after repair: mode %v", fi.Mode().Perm())
	}

	// an existing folder is not touched
	os.Chmod(filepath.Dir(path), 0o755)
	LoadAPICredential(osx.Current(), path, true)
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o755 {
		t.Errorf("existing folder mode changed to %v", fi.Mode().Perm())
	}
}
