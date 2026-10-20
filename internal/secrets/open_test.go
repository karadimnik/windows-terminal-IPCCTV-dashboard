package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"holocron/internal/osx"
)

func TestOpenSelection(t *testing.T) {
	dir := t.TempDir()
	auth := osx.Dirs{Auth: dir}
	for _, c := range []struct {
		fakeOS  string
		backend string
		want    string // backend name, or "" with wantErr
		wantErr error
	}{
		{"linux", "", "file", nil},
		{"linux", "auto", "file", nil},
		{"darwin", "auto", "file", nil},
		{"linux", "file", "file", nil},
		{"linux", "env", "env", nil},
		{"linux", "keyring", "", ErrUnsupported}, // on a Windows host the keyring opens; see the Windows test
		{"windows", "auto", "file", nil},         // Windows platform on a non-Windows host: the keyring is unsupported, fall back
	} {
		if c.backend == "keyring" && runtime.GOOS == "windows" || c.fakeOS == "windows" && runtime.GOOS == "windows" {
			continue
		}
		p, err := Open(Config{Backend: c.backend, Platform: &osx.Fake{OS: c.fakeOS, DirsValue: auth}, Environ: noEnv})
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s/%s: err = %v, want %v", c.fakeOS, c.backend, err, c.wantErr)
			continue
		}
		if err == nil && p.Backend() != c.want {
			t.Errorf("%s/%s: backend %s, want %s", c.fakeOS, c.backend, p.Backend(), c.want)
		}
	}
	if _, err := Open(Config{Backend: "bogus", Platform: &osx.Fake{DirsValue: auth}}); err == nil {
		t.Error("an unknown backend must fail")
	}
	if _, err := Open(Config{Backend: "file", Platform: &osx.Fake{}}); err == nil {
		t.Error("no auth dir and no explicit paths must fail")
	}
}

func TestOpenDefaultPathsAndExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	p, err := Open(Config{Backend: "file", Platform: &osx.Fake{DirsValue: osx.Dirs{Auth: dir}}, Environ: noEnv})
	must(t, err)
	must(t, p.Put("n", "value-x"))
	if _, err := newTestFile(t, dir, noEnv).Get("n"); err != nil {
		t.Fatalf("default location is <auth>/secrets.enc: %v", err)
	}
	other := t.TempDir()
	p, err = Open(Config{Backend: "file", File: filepath.Join(other, "s.enc"), KeyFile: filepath.Join(other, "s.key"), Platform: &osx.Fake{}, Environ: noEnv})
	must(t, err)
	must(t, p.Put("n", "value-x"))
	for _, f := range []string{"s.enc", "s.key"} {
		if _, err := os.Stat(filepath.Join(other, f)); err != nil {
			t.Errorf("explicit path not used: %v", err)
		}
	}
}

func TestRedactionHooks(t *testing.T) {
	r := &recorder{}
	dir := t.TempDir()
	p, err := Open(Config{Backend: "file", Platform: &osx.Fake{DirsValue: osx.Dirs{Auth: dir}}, Environ: noEnv, Redactor: r})
	must(t, err)
	if r.has("pw-written") {
		t.Fatal("registered before use")
	}
	if err := p.Put("n", ""); err == nil {
		t.Fatal("empty value accepted")
	}
	must(t, p.Put("n", "pw-written"))
	if !r.has("pw-written") {
		t.Error("Put did not register")
	}
	other, err := Open(Config{Backend: "file", Platform: &osx.Fake{DirsValue: osx.Dirs{Auth: dir}}, Environ: noEnv})
	must(t, err)
	r3 := &recorder{}
	other = WithRedactor(other, r3)
	if _, err := other.Get("n"); err != nil || !r3.has("pw-written") {
		t.Error("Get did not register")
	}
	if _, err := other.Get("absent"); err == nil {
		t.Fatal("expected not found")
	}
	if len(r3.seen) != 1 {
		t.Errorf("a failed Get registered something: %v", r3.seen)
	}
	if WithRedactor(p, nil) != p {
		t.Error("nil redactor must return the provider")
	}
}
