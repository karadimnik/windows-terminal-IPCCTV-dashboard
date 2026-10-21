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

// A junction ANCESTOR is judged by where it lands, like a symlinked one (TestCheckRootRefusesSymlink,
// which needs a privilege a junction does not).
func TestCheckRootJunctionAncestor(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	anc := filepath.Join(t.TempDir(), "anc")
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", anc, base).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v: %s", err, out)
	}
	prot := &osx.Fake{OS: "windows", Protected: []osx.Protected{{Path: filepath.Join(sub, "inner"), Why: "a protected folder"}}}
	if got := CheckRoot(filepath.Join(anc, "sub"), prot); !strings.Contains(got, "resolves to") || !strings.Contains(got, "contains a protected folder") {
		t.Errorf("junction ancestor: %q", got)
	}
	if got := CheckRoot(filepath.Join(anc, "sub"), &osx.Fake{OS: "windows"}); got != "" {
		t.Errorf("control: %q", got)
	}
}

// The same folder spelled with its 8.3 short name (a %TEMP% under a long user name is) and its long
// name is one folder: either spelling of the root against either spelling of a protected path.
func TestCheckRootShortAndLongNames(t *testing.T) {
	base := t.TempDir()
	long := filepath.Join(base, "protectedlongname")
	short := filepath.Join(base, "PROTEC~1")
	if err := os.MkdirAll(filepath.Join(long, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(long)
	b, err := os.Stat(short)
	if err != nil || !os.SameFile(a, b) {
		t.Skipf("%s has no 8.3 name PROTEC~1 (8.3 names off on this volume): %v", long, err)
	}
	for _, c := range [][2]string{{long, short}, {short, long}} {
		p := &osx.Fake{OS: "windows", Protected: []osx.Protected{{Path: filepath.Join(c[1], "inner"), Why: "a protected folder"}}}
		if got := CheckRoot(c[0], p); !strings.Contains(got, "contains a protected folder") {
			t.Errorf("root %s, protected %s: %q", c[0], filepath.Join(c[1], "inner"), got)
		}
		if got := CheckRoot(c[0], &osx.Fake{OS: "windows"}); got != "" {
			t.Errorf("control %s: %q", c[0], got)
		}
	}
}
