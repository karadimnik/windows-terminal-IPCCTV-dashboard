package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runSet(cfg Config, user, stdin string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := setPassword(cfg, fastHasher(), user, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestSetPasswordCreatesAndUpdates(t *testing.T) {
	cfg := Config{AuthDir: filepath.Join(t.TempDir(), "auth")}
	code, out, errText := runSet(cfg, "Anna", "first password here\r\n")
	if code != 0 || !strings.Contains(out, "created user Anna") {
		t.Fatalf("create: %d %q %q", code, out, errText)
	}
	store := newFileUserStore(cfg.AuthDir, time.Now)
	u, ok, _ := store.Get("anna")
	if !ok || u.Role != roleAdmin || u.ID == "" || !verifyPassword(u.Hash, "first password here") {
		t.Fatalf("stored %+v", u)
	}
	// A BOM and CRLF (Windows PowerShell 5.1 pipes) are not part of the password.
	if code, _, _ := runSet(cfg, "anna", "\ufeffsecond password here\r\n"); code != 0 {
		t.Fatal("update failed")
	}
	again, _, _ := store.Get("anna")
	if again.ID != u.ID || again.Username != "Anna" || !again.CreatedAt.Equal(u.CreatedAt) || !verifyPassword(again.Hash, "second password here") || verifyPassword(again.Hash, "first password here") {
		t.Errorf("update lost identity or kept the old password: %+v", again)
	}
	if list, _ := store.List(); len(list) != 1 {
		t.Errorf("%d users", len(list))
	}
	// The log notes it and never the password; the files are owner-only.
	logText := string(mustRead(t, filepath.Join(cfg.AuthDir, "auth.log")))
	if !strings.Contains(logText, "event=set-password result=ok") || strings.Contains(logText, "password here") {
		t.Errorf("auth.log: %q", logText)
	}
	assertOwnerOnly(t, filepath.Join(cfg.AuthDir, "users.json"), false)
}

func TestSetPasswordRefusals(t *testing.T) {
	cfg := Config{AuthDir: filepath.Join(t.TempDir(), "auth")}
	cases := map[string][2]string{
		"empty input":   {"anna", ""},
		"empty line":    {"anna", "\n"},
		"too short":     {"anna", "short\n"},
		"same as name":  {"administrator", "administrator\n"},
		"bad user name": {"an na", "a long enough password\n"},
		"empty name":    {"", "a long enough password\n"},
	}
	for name, c := range cases {
		code, out, errText := runSet(cfg, c[0], c[1])
		if code == 0 {
			t.Errorf("%s: accepted", name)
		}
		if strings.Contains(out+errText, "a long enough password") || strings.Contains(out+errText, "short") && name != "too short" {
			t.Errorf("%s: output repeats the password: %q %q", name, out, errText)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.AuthDir, "users.json")); err == nil {
		t.Error("a refused call wrote users.json")
	}
	// Only the first line counts: a pasted second line is not part of the password.
	if code, _, _ := runSet(cfg, "anna", "a long enough password\nsecond line\n"); code != 0 {
		t.Fatal("two lines refused")
	}
	u, _, _ := newFileUserStore(cfg.AuthDir, time.Now).Get("anna")
	if !verifyPassword(u.Hash, "a long enough password") {
		t.Error("the first line is not the password")
	}
}

// -set-password runs while camui runs: the change must be seen by the running
// server, and the user's sessions must be gone.
func TestSetPasswordEndsSessionsOfARunningServer(t *testing.T) {
	rig := newTLSRig(t, nil)
	cookie := rig.login(t)
	other := rig.users // the running server's store
	if code, out, errText := runSet(rig.cfg, testUser, "a brand new password!\n"); code != 0 {
		t.Fatalf("%d %s %s", code, out, errText)
	}
	if rec := rig.do(tlsReq{path: "/api/status", cookie: cookie}); rec.Code != 401 {
		t.Errorf("the old session works after a password change: %d", rec.Code)
	}
	if rec := rig.loginFrom("192.168.20.1:1", testUser, testPassword); rec.Code != 401 {
		t.Errorf("the old password still works: %d", rec.Code)
	}
	if rec := rig.loginFrom("192.168.20.2:1", testUser, "a brand new password!"); rec.Code != 200 {
		t.Errorf("the new password: %d", rec.Code)
	}
	if u, _, _ := other.Get(testUser); u.ID != "u-anna" {
		t.Errorf("the user's id changed: %+v", u)
	}
}

func TestUnlockClearsALockoutInARunningServer(t *testing.T) {
	rig := newTLSRig(t, nil)
	for i := 0; i < 5; i++ {
		rig.loginFrom(fmt.Sprintf("192.168.21.%d:1", i+1), testUser, "wrong wrong wrong 1")
	}
	if rec := rig.loginFrom("192.168.21.10:1", testUser, testPassword); rec.Code != 429 {
		t.Fatalf("not locked: %d", rec.Code)
	}
	var out, errOut bytes.Buffer
	if code := runUnlock(rig.cfg, "ANNA", &out, &errOut); code != 0 {
		t.Fatalf("unlock: %d %s", code, errOut.String())
	}
	// Another name stays locked, and the owner can sign in.
	if rec := rig.loginFrom("192.168.21.11:1", testUser, testPassword); rec.Code != 200 {
		t.Errorf("after -unlock: %d", rec.Code)
	}
	if st, present := rig.users.Failure(testUser); present && st.Count != 0 {
		t.Errorf("counter not cleared: %+v", st)
	}
	// The request is used up: it does not unlock a later lockout.
	for i := 0; i < 5; i++ {
		rig.loginFrom(fmt.Sprintf("192.168.22.%d:1", i+1), testUser, "wrong wrong wrong 1")
	}
	if rec := rig.loginFrom("192.168.22.10:1", testUser, testPassword); rec.Code != 429 {
		t.Errorf("a used unlock request unlocked again: %d", rec.Code)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(rig.cfg.AuthDir, "auth.log"))), "event=unlock result=applied") {
		t.Error("the unlock was not logged")
	}
	// A restart discards requests made while nothing was running.
	_ = runUnlock(rig.cfg, "anna", &out, &errOut)
	clearUnlockMarkers(rig.cfg.AuthDir)
	if _, err := os.Stat(unlockMarker(rig.cfg.AuthDir, "anna")); err == nil {
		t.Error("stale unlock request survived a start")
	}
}

func TestLockoutIsInMemoryOnly(t *testing.T) {
	rig := newTLSRig(t, nil)
	for i := 0; i < 5; i++ {
		rig.loginFrom(fmt.Sprintf("192.168.23.%d:1", i+1), testUser, "wrong wrong wrong 1")
	}
	fresh := newFileUserStore(rig.cfg.AuthDir, rig.clock.now) // what a restarted camui starts with
	if _, present := fresh.Failure(testUser); present {
		t.Error("lockout state reached the disk")
	}
	for _, name := range []string{"users.json", "sessions.json", "auth.log"} {
		if raw, err := os.ReadFile(filepath.Join(rig.cfg.AuthDir, name)); err == nil && strings.Contains(string(raw), "lockedUntil") {
			t.Errorf("%s holds lockout state", name)
		}
	}
}
