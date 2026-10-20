package secrets

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"holocron/internal/osx"
)

// recorder is a Redactor that remembers what it was told.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) Register(s string) { r.mu.Lock(); r.seen = append(r.seen, s); r.mu.Unlock() }
func (r *recorder) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.seen {
		if v == s {
			return true
		}
	}
	return false
}

func noEnv() []string { return nil }

func envWithKey(hexKey string) func() []string {
	return func() []string { return []string{KeyEnv + "=" + hexKey} }
}

const keyA = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
const keyB = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"

// newTestFile opens a file backend in dir with a fake platform (no ACL calls).
func newTestFile(t testing.TB, dir string, environ func() []string) *fileProvider {
	t.Helper()
	p, err := newFile(Config{
		File:     filepath.Join(dir, "secrets.enc"),
		KeyFile:  filepath.Join(dir, "secrets.key"),
		Platform: &osx.Fake{},
		Environ:  environ,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*fileProvider)
}

// suite is the contract every writable backend meets.
func suite(t *testing.T, fresh func(t *testing.T) Provider) {
	t.Run("roundtrip overwrite delete list", func(t *testing.T) {
		p := fresh(t)
		if _, err := p.Get("a.b-c_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty store Get: %v", err)
		}
		if names, err := p.List(); err != nil || len(names) != 0 {
			t.Fatalf("empty List = %v, %v", names, err)
		}
		if err := p.Delete("a.b-c_1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Delete absent: %v", err)
		}
		must(t, p.Put("zeta", "value-one"))
		must(t, p.Put("a.b-c_1", "pa55 w0rd é中"))
		if got, err := p.Get("a.b-c_1"); err != nil || got != "pa55 w0rd é中" {
			t.Fatalf("Get = %q, %v", got, err)
		}
		must(t, p.Put("zeta", "value-two")) // overwrite
		if got, _ := p.Get("zeta"); got != "value-two" {
			t.Fatalf("overwrite: %q", got)
		}
		names, err := p.List()
		if err != nil || strings.Join(names, ",") != "a.b-c_1,zeta" {
			t.Fatalf("List = %v, %v (names only, sorted)", names, err)
		}
		for _, n := range names {
			if strings.Contains(n, "value") || strings.Contains(n, "pa55") {
				t.Fatalf("List leaked a value: %q", n)
			}
		}
		must(t, p.Delete("zeta"))
		if _, err := p.Get("zeta"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after Delete: %v", err)
		}
		if got, _ := p.Get("a.b-c_1"); got == "" {
			t.Fatal("Delete removed the wrong entry")
		}
		must(t, p.Delete("a.b-c_1"))
		if names, _ := p.List(); len(names) != 0 {
			t.Fatalf("List after deleting all = %v", names)
		}
	})
	t.Run("name validation", func(t *testing.T) {
		p := fresh(t)
		for _, n := range []string{"", "UPPER", "has space", "slash/x", "back\\x", "dot\x00", "é", strings.Repeat("a", 65), "a:b", "..\\x"} {
			if _, err := p.Get(n); !errors.Is(err, ErrInvalidName) {
				t.Errorf("Get(%q) = %v", n, err)
			}
			if err := p.Put(n, "v-value"); !errors.Is(err, ErrInvalidName) {
				t.Errorf("Put(%q) = %v", n, err)
			}
			if err := p.Delete(n); !errors.Is(err, ErrInvalidName) {
				t.Errorf("Delete(%q) = %v", n, err)
			}
		}
		must(t, p.Put(strings.Repeat("a", 64), "v-value")) // the longest legal name
	})
	t.Run("value validation", func(t *testing.T) {
		p := fresh(t)
		for _, v := range []string{"", strings.Repeat("x", MaxValueLen+1), "bad\xff\xfeutf8"} {
			if err := p.Put("n", v); !errors.Is(err, ErrInvalidValue) {
				t.Errorf("Put len %d = %v", len(v), err)
			}
		}
		if _, err := p.Get("n"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a rejected Put stored something: %v", err)
		}
	})
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// scanTree reports every file under root (other than those whose base name is in allowed) that
// contains one of the needles, plus the allowed files that do (ciphertext must not hold a value).
func scanTree(t testing.TB, root string, needles []string, allowed ...string) (leaks []string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("scan: cannot read %s: %v", path, rerr)
			return nil
		}
		for _, n := range needles {
			if bytes.Contains(b, []byte(n)) {
				leaks = append(leaks, filepath.Base(path))
				break
			}
		}
		return nil
	})
	_ = allowed // kept for call-site clarity: allowed files are scanned too, they must not hold plaintext
	return leaks
}
