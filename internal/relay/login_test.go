package relay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"holocron/internal/config"
	"holocron/internal/osx"
)

func TestPassHashIsTheDocumentedForm(t *testing.T) {
	g := loadGolden(t)
	if h := (Login{Secret: g.APILogin.Password}).PassHash(); h != g.APILogin.PasswordHash {
		t.Errorf("api: %s", h)
	}
	if h := (Login{Secret: g.LiveLogin.Secret}).PassHash(); h != g.LiveLogin.SecretHash {
		t.Errorf("live: %s", h)
	}
}

func TestLoginDoesNotPrintItsSecret(t *testing.T) {
	l := Login{User: "u", Secret: "sEcReT-value"}
	for _, s := range []string{fmt.Sprint(l), fmt.Sprintf("%v %+v %#v %s", l, l, l, l)} {
		if strings.Contains(s, "sEcReT") {
			t.Errorf("printed: %s", s)
		}
	}
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type loader struct {
	name string
	load func(p osx.Platform, path string, create bool) (Login, error)
	user string
}

var loaders = []loader{{"api", LoadAPICredential, APIUser}, {"live", LoadLiveSecret, LiveUser}}

func TestSecretFileCreateReloadIdempotent(t *testing.T) {
	for _, l := range loaders {
		t.Run(l.name, func(t *testing.T) {
			fake := &osx.Fake{}
			path := filepath.Join(t.TempDir(), "sub", "secret.txt")
			if _, err := l.load(fake, path, false); !errors.Is(err, ErrNoSecret) {
				t.Fatalf("missing, no create: %v", err)
			}
			if _, err := os.Stat(path); err == nil {
				t.Fatal("a load without create made the file")
			}
			first, err := l.load(fake, path, true)
			if err != nil {
				t.Fatal(err)
			}
			if first.User != l.user || !hex64.MatchString(first.Secret) {
				t.Fatalf("%v %q", first, first.Secret)
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != first.Secret {
				t.Errorf("file holds %q (no newline, the exact secret expected)", raw)
			}
			// the temp file did not stay
			if _, err := os.Stat(path + ".tmp"); err == nil {
				t.Error("temp file left behind")
			}
			// second call, create or not, reuses the secret and does not rewrite the file
			before, _ := os.Stat(path)
			for _, create := range []bool{true, false} {
				again, err := l.load(fake, path, create)
				if err != nil || again.Secret != first.Secret {
					t.Fatalf("reload create=%v: %v", create, err)
				}
			}
			after, _ := os.Stat(path)
			if !after.ModTime().Equal(before.ModTime()) {
				t.Error("the file was rewritten")
			}
			// a new install gets a different secret
			other, _ := l.load(fake, filepath.Join(t.TempDir(), "s"), true)
			if other.Secret == first.Secret {
				t.Error("two installs share a secret")
			}
			// the owner-only restriction was applied to the temp file before the secret went in
			calls := fake.OwnerOnlyCalls()
			if len(calls) == 0 || calls[0] != path+".tmp" {
				t.Errorf("SetOwnerOnly calls: %v", calls)
			}
		})
	}
}

func TestSecretFileWithWrongContentIsRejectedNotRewritten(t *testing.T) {
	for _, bad := range []string{"", "short", strings.Repeat("g", 64), strings.Repeat("A", 64), strings.Repeat("a", 65), "<Objs>clixml</Objs>"} {
		path := filepath.Join(t.TempDir(), "secret.txt")
		os.WriteFile(path, []byte(bad), 0o600)
		for _, l := range loaders {
			_, err := l.load(&osx.Fake{}, path, true)
			if err == nil || !strings.Contains(err.Error(), "does not hold 64 hex characters") {
				t.Errorf("%s %q: %v", l.name, bad, err)
			}
			if got, _ := os.ReadFile(path); string(got) != bad {
				t.Errorf("a rejected file was changed to %q", got)
			}
		}
	}
	// a trailing newline is tolerated (camui does the same)
	path := filepath.Join(t.TempDir(), "secret.txt")
	good := strings.Repeat("ab", 32)
	os.WriteFile(path, []byte(good+"\r\n"), 0o600)
	l, err := LoadLiveSecret(&osx.Fake{}, path, false)
	if err != nil || l.Secret != good {
		t.Errorf("%v %v", l, err)
	}
}

func TestSecretFilePermissionRepairFailureIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(path, []byte(strings.Repeat("ab", 32)), 0o600)
	fake := &osx.Fake{OS: "windows", OwnerErr: errors.New("icacls failed")}
	// on Windows the restriction is always re-applied; if it cannot be, the secret is not used
	if _, err := LoadAPICredential(fake, path, false); err == nil || !strings.Contains(err.Error(), "could not be restricted") {
		t.Errorf("%v", err)
	}
	if _, err := LoadAPICredential(&osx.Fake{OS: "windows"}, path, false); err != nil {
		t.Error(err)
	}
	// and creation fails closed: no file is left when the temp file cannot be restricted
	path2 := filepath.Join(t.TempDir(), "new.txt")
	if _, err := LoadLiveSecret(&osx.Fake{OwnerErr: errors.New("nope")}, path2, true); err == nil {
		t.Error("created a secret it could not protect")
	}
	for _, p := range []string{path2, path2 + ".tmp"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists", p)
		}
	}
}

func TestDefaultPaths(t *testing.T) {
	p := &osx.Fake{DirsValue: osx.Dirs{Auth: "/state/auth"}}
	if got := DefaultAPICredentialPath(p); got != filepath.Join("/state/auth", "relay_api_secret.txt") {
		t.Error(got)
	}
	if got := LiveSecretPath(p, config.Config{}); got != filepath.Join("/state/auth", "live_view_secret.txt") {
		t.Error(got)
	}
	if got := LiveSecretPath(p, config.Config{LiveSecretPath: "/x/secret"}); got != "/x/secret" {
		t.Error(got)
	}
}

func TestWriteConfigIfChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "mediamtx.yml")
	text := []byte("a: 1\r\nb: 2\r\n")
	if wrote, err := WriteConfigIfChanged(path, text); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(text) {
		t.Errorf("%q", got)
	}
	if wrote, err := WriteConfigIfChanged(path, text); err != nil || wrote {
		t.Errorf("unchanged text rewrote the file: %v %v", wrote, err)
	}
	if wrote, err := WriteConfigIfChanged(path, []byte("a: 2\r\n")); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("temp file left behind")
	}
}
