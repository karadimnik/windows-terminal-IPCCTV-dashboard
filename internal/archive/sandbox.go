package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SandboxEnv names the test switch: when set, the pruner and the mover refuse any root or file
// outside the sandbox prefix. "1" means the OS temporary directory (where t.TempDir lives); any
// other value is the prefix itself; "" and "0" mean no sandbox.
const SandboxEnv = "SANDBOX_ONLY"

// ErrOutsideSandbox is returned when a sandbox is in force and a path lies outside it.
var ErrOutsideSandbox = errors.New("outside the sandbox")

// SandboxFromEnv is the sandbox prefix SandboxEnv sets, "" when none is in force.
func SandboxFromEnv() string {
	switch v := os.Getenv(SandboxEnv); v {
	case "", "0":
		return ""
	case "1":
		return os.TempDir()
	default:
		return v
	}
}

// CheckSandbox returns nil only when path lies strictly inside prefix, judged on the absolute,
// cleaned paths and again with existing links resolved. An empty prefix allows everything.
func CheckSandbox(path, prefix string) error {
	if prefix == "" {
		return nil
	}
	inside := func(p, base string) bool {
		rel, err := filepath.Rel(base, p)
		return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	ap, err1 := filepath.Abs(path)
	bp, err2 := filepath.Abs(prefix)
	if err1 != nil || err2 != nil || !inside(ap, bp) {
		return fmt.Errorf("%w: %s is not inside %s", ErrOutsideSandbox, path, prefix)
	}
	rp, rb := resolveLinks(ap), resolveLinks(bp)
	if !inside(rp, rb) {
		return fmt.Errorf("%w: %s resolves to %s, not inside %s", ErrOutsideSandbox, path, rp, rb)
	}
	return nil
}

// resolveLinks follows links in the longest existing prefix of p.
func resolveLinks(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
