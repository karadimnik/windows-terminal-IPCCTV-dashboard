//go:build windows

package archive

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/osx"
)

// A junction needs no privilege; it must be treated as a reparse point both as a root and inside a root.
func TestJunctionIsReparse(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	junction := filepath.Join(base, "junction")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, real).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v: %s", err, out)
	}
	fi, err := os.Lstat(junction)
	if err != nil || !IsReparse(fi) {
		t.Fatalf("junction not detected as a reparse point: %v", err)
	}
	if rf, _ := os.Lstat(real); IsReparse(rf) {
		t.Error("control: a plain folder is not a reparse point")
	}
	p := osx.Current()
	if got := CheckRoot(junction, p); !strings.Contains(got, "reparse point") {
		t.Errorf("junction as root: %q", got)
	}
	if got := CheckRoot(real, p); got != "" {
		t.Errorf("control: %q", got)
	}
	root := t.TempDir()
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", filepath.Join(root, "Cam_J"), real).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v: %s", err, out)
	}
	if got := ClaimProblem(root, p, ".mkv"); !strings.Contains(got, "Cam_J (a reparse point)") {
		t.Errorf("junction inside the root: %q", got)
	}
}
