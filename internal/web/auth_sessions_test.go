package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSessions(t *testing.T) (*sessionStore, *fakeClock, string) {
	t.Helper()
	clock := newFakeClock()
	dir := filepath.Join(t.TempDir(), "auth")
	return newSessionStore(dir, clock.now), clock, dir
}

func TestSessionCreateLookupLogout(t *testing.T) {
	s, _, _ := newTestSessions(t)
	token, err := s.Create("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 43 {
		t.Errorf("token %q is shorter than 32 random bytes in base64", token)
	}
	sess, ok := s.Lookup(token)
	if !ok || sess.UserID != "user-1" {
		t.Fatalf("lookup: %+v %v", sess, ok)
	}
	for _, bad := range []string{"", "x", token + "x", token[:len(token)-1], strings.ToUpper(token)} {
		if _, ok := s.Lookup(bad); ok {
			t.Errorf("lookup accepted %q", bad)
		}
	}
	if err := s.Delete(token); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(token); ok {
		t.Error("session survived logout")
	}
	if err := s.Delete(token); err != nil {
		t.Errorf("logging out twice must not fail: %v", err)
	}
}

func TestSessionTokensAreRotatedAndUnique(t *testing.T) {
	s, _, _ := newTestSessions(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		token, err := s.Create("u")
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatal("a token was issued twice")
		}
		seen[token] = true
	}
}

func TestSessionIdleAndAbsoluteLifetimes(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name  string
		steps []time.Duration // advance, then look up
		alive []bool
	}{
		{"idle just under 7 days", []time.Duration{7*day - time.Second}, []bool{true}},
		{"idle exactly 7 days", []time.Duration{7 * day}, []bool{false}},
		{"used every 6 days stays alive until day 30", []time.Duration{6 * day, 6 * day, 6 * day, 6 * day}, []bool{true, true, true, true}},
		{"absolute 30 days wins over activity", []time.Duration{6 * day, 6 * day, 6 * day, 6 * day, 6 * day}, []bool{true, true, true, true, false}},
		{"alive a minute before day 30, dead at day 30", []time.Duration{6 * day, 6 * day, 6 * day, 6 * day, 6*day - time.Minute, time.Minute}, []bool{true, true, true, true, true, false}},
	}
	for _, c := range cases {
		s, clock, _ := newTestSessions(t)
		token, _ := s.Create("u")
		for i, step := range c.steps {
			clock.advance(step)
			if _, ok := s.Lookup(token); ok != c.alive[i] {
				t.Errorf("%s: step %d alive=%v, want %v", c.name, i, ok, c.alive[i])
			}
		}
	}
}

func TestExpiredSessionIsRemovedFromTheFile(t *testing.T) {
	s, clock, dir := newTestSessions(t)
	token, _ := s.Create("u")
	clock.advance(8 * 24 * time.Hour)
	if _, ok := s.Lookup(token); ok {
		t.Fatal("expired session accepted")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if strings.Contains(string(raw), "userId") {
		t.Errorf("the expired session is still on disk: %s", raw)
	}
}

func TestSessionsSurviveARestartAndStoreOnlyAHash(t *testing.T) {
	s, clock, dir := newTestSessions(t)
	token, _ := s.Create("u")
	raw, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("sessions.json holds the token itself")
	}
	for _, key := range []string{"tokenHash", "userId", "createdAt", "lastSeen", "expiresAt"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("missing %s in %s", key, raw)
		}
	}
	assertOwnerOnly(t, filepath.Join(dir, "sessions.json"), false)
	restarted := newSessionStore(dir, clock.now)
	if sess, ok := restarted.Lookup(token); !ok || sess.UserID != "u" {
		t.Error("the session did not survive a restart")
	}
}

func TestSessionDeleteForUserAndSeenByTheRunningServer(t *testing.T) {
	running, clock, dir := newTestSessions(t)
	a1, _ := running.Create("alice")
	a2, _ := running.Create("alice")
	b1, _ := running.Create("bob")
	cli := newSessionStore(dir, clock.now) // `camui.exe -set-password` is another process
	if err := cli.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{a1, a2} {
		if _, ok := running.Lookup(token); ok {
			t.Error("a session of the user survived DeleteUser in another process")
		}
	}
	if _, ok := running.Lookup(b1); !ok {
		t.Error("DeleteUser removed another user's session")
	}
}

func TestSessionCapPerUserDropsTheOldest(t *testing.T) {
	s, clock, _ := newTestSessions(t)
	var first string
	for i := 0; i < maxSessionsPerUser+5; i++ {
		clock.advance(time.Second)
		token, _ := s.Create("u")
		if i == 0 {
			first = token
		}
	}
	if _, ok := s.Lookup(first); ok {
		t.Error("the oldest session was kept beyond the cap")
	}
	if n := s.countFor("u"); n != maxSessionsPerUser {
		t.Errorf("%d sessions, want %d", n, maxSessionsPerUser)
	}
}

func TestSessionLookupDoesNotWriteOnEveryRequest(t *testing.T) {
	s, clock, dir := newTestSessions(t)
	token, _ := s.Create("u")
	path := filepath.Join(dir, "sessions.json")
	before, _ := os.Stat(path)
	for i := 0; i < 20; i++ {
		clock.advance(time.Second)
		s.Lookup(token)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("every request rewrote sessions.json")
	}
	clock.advance(lastSeenGranularity + time.Second)
	s.Lookup(token)
	reloaded := newSessionStore(dir, clock.now)
	if sess, _ := reloaded.Lookup(token); !sess.LastSeen.Equal(clock.now()) {
		t.Errorf("lastSeen on disk = %v, want %v", sess.LastSeen, clock.now())
	}
}
