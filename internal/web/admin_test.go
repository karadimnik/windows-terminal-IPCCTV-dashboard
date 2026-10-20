package web

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newAdmin(t *testing.T) (*UserAdmin, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return usersWith(filepath.Join(t.TempDir(), "auth"), log, fastHasher()), logs
}

func TestUserAdminLifecycleAndSessions(t *testing.T) {
	admin, logs := newAdmin(t)
	if err := admin.Add("cli:test", "Anna", testPassword); err != nil {
		t.Fatal(err)
	}
	if err := admin.Add("cli:test", "anna", testPassword); !errors.Is(err, ErrUserExists) {
		t.Errorf("duplicate (case-insensitive): %v", err)
	}
	if err := admin.SetPassword("cli:test", "nobody", testPassword); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("passwd unknown: %v", err)
	}
	for _, bad := range []string{"short", strings.Repeat("x", 129)} {
		err := admin.SetPassword("cli:test", "Anna", bad)
		if !IsInputError(err) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := admin.Add("cli:test", "samename-1234", "samename-1234"); !IsInputError(err) {
		t.Errorf("password equal to the name: %v", err)
	}
	if err := admin.Add("cli:test", "bad name", testPassword); !IsInputError(err) {
		t.Errorf("bad name: %v", err)
	}

	u, found, _ := admin.svc.users.Get("anna")
	if !found {
		t.Fatal("user missing")
	}
	token, err := admin.svc.sessions.Create(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := admin.svc.sessions.Lookup(token); !ok {
		t.Fatal("session did not open")
	}
	// passwd ends the user's sessions
	if err := admin.SetPassword("cli:test", "ANNA", "a different long passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, ok := admin.svc.sessions.Lookup(token); ok {
		t.Error("the session survived a password change")
	}

	// remove ends them too and deletes the user
	token, _ = admin.svc.sessions.Create(u.ID)
	if err := admin.Remove("cli:test", "anna"); err != nil {
		t.Fatal(err)
	}
	if _, ok := admin.svc.sessions.Lookup(token); ok {
		t.Error("the session survived the removal")
	}
	if err := admin.Remove("cli:test", "anna"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("remove again: %v", err)
	}
	if list, _ := admin.List(); len(list) != 0 {
		t.Errorf("%+v", list)
	}

	// structured records, never the password or the hash
	for _, want := range []string{"action=user.set_password", "action=user.remove", "result=ok", "actor=cli:test"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("%q not logged:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), testPassword) || strings.Contains(logs.String(), "pbkdf2") {
		t.Error("a secret reached the log")
	}
	if b, _ := os.ReadFile(filepath.Join(admin.Dir(), "auth.log")); !strings.Contains(string(b), "remove") {
		t.Errorf("auth.log: %s", b)
	}
}

func TestUserAdminListHasNoHash(t *testing.T) {
	admin, _ := newAdmin(t)
	admin.Add("x", "anna", testPassword)
	list, err := admin.List()
	if err != nil || len(list) != 1 || list[0].Username != "anna" || list[0].Role != "admin" || list[0].CreatedAt.IsZero() {
		t.Fatalf("%+v %v", list, err)
	}
}

func TestUserAdminUnlockWritesTheMarkerTheServiceConsumes(t *testing.T) {
	admin, _ := newAdmin(t)
	if err := admin.Unlock("cli:test", "Anna"); err != nil {
		t.Fatal(err)
	}
	if !admin.svc.consumeUnlock("anna") {
		t.Error("the service did not find the marker (names compare ignoring case)")
	}
	if admin.svc.consumeUnlock("anna") {
		t.Error("the marker must be used up")
	}
	if err := admin.Unlock("cli:test", strings.Repeat("x", 65)); !IsInputError(err) {
		t.Errorf("long name: %v", err)
	}
}

func TestTLSFacade(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if _, err := CACertificatePEM(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no CA yet: %v", err)
	}
	info, err := CAFor(dir)
	if err != nil || len(info.Fingerprint) < 64 || info.Path != filepath.Join(dir, "ca.crt") {
		t.Fatalf("%+v %v", info, err)
	}
	pem, err := CACertificatePEM(dir)
	if err != nil || !bytes.HasPrefix(pem, []byte("-----BEGIN CERTIFICATE-----")) || bytes.Contains(pem, []byte("PRIVATE")) {
		t.Fatalf("%q %v", pem, err)
	}
	var out bytes.Buffer
	WriteCAInstructions(&out, info)
	if !strings.Contains(out.String(), info.Fingerprint) || strings.Contains(out.String(), "PRIVATE KEY") {
		t.Errorf("instructions:\n%s", out.String())
	}

	os.WriteFile(filepath.Join(dir, "mine.txt"), []byte("x"), 0o600)
	os.Mkdir(filepath.Join(dir, "sub"), 0o700)
	removed, left, err := RotateCA(dir)
	if err != nil || len(removed) != 2 || len(left) != 2 {
		t.Fatalf("removed %v left %v err %v", removed, left, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("the folder with foreign files must stay")
	}
	os.Remove(filepath.Join(dir, "mine.txt"))
	os.Remove(filepath.Join(dir, "sub"))
	// a fresh CA after rotation differs from the first
	info2, err := CAFor(dir)
	if err != nil || info2.Fingerprint == info.Fingerprint {
		t.Errorf("rotation kept the CA: %v", err)
	}
	// nothing but the CA files: the folder goes too
	if removed, left, err := RotateCA(dir); err != nil || len(removed) != 2 || len(left) != 0 {
		t.Fatalf("%v %v %v", removed, left, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("an empty TLS folder should be removed")
	}
}
