package main

import (
	"strings"
	"testing"
	"time"
)

// fastHasher keeps the unit tests quick; the production cost is checked separately.
func fastHasher() passwordHasher { return passwordHasher{iterations: 1000} }

func TestHashVerifyRoundTrip(t *testing.T) {
	h := fastHasher()
	encoded, err := h.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "pbkdf2-sha256$1000$") || strings.Count(encoded, "$") != 3 {
		t.Fatalf("not self-describing: %q", encoded)
	}
	if strings.Contains(encoded, "correct horse") {
		t.Fatal("the hash contains the password")
	}
	for _, c := range []struct {
		name, password string
		want           bool
	}{
		{"right", "correct horse battery", true},
		{"wrong", "correct horse batterY", false},
		{"empty", "", false},
		{"prefix", "correct horse", false},
		{"suffix", "correct horse battery!", false},
	} {
		if got := verifyPassword(encoded, c.password); got != c.want {
			t.Errorf("%s: verify = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHashIsSalted(t *testing.T) {
	h := fastHasher()
	a, _ := h.Hash("same password here")
	b, _ := h.Hash("same password here")
	if a == b {
		t.Fatal("two hashes of one password are identical: the salt is not random")
	}
}

func TestVerifyRejectsTamperedAndMalformed(t *testing.T) {
	h := fastHasher()
	good, _ := h.Hash("a password long enough")
	parts := strings.Split(good, "$")
	flip := func(s string) string {
		b := []byte(s)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		return string(b)
	}
	cases := map[string]string{
		"flipped hash":   strings.Join([]string{parts[0], parts[1], parts[2], flip(parts[3])}, "$"),
		"flipped salt":   strings.Join([]string{parts[0], parts[1], flip(parts[2]), parts[3]}, "$"),
		"other count":    strings.Join([]string{parts[0], "1001", parts[2], parts[3]}, "$"),
		"other algo":     strings.Join([]string{"pbkdf2-sha1", parts[1], parts[2], parts[3]}, "$"),
		"zero count":     strings.Join([]string{parts[0], "0", parts[2], parts[3]}, "$"),
		"huge count":     strings.Join([]string{parts[0], "999999999999", parts[2], parts[3]}, "$"),
		"negative count": strings.Join([]string{parts[0], "-5", parts[2], parts[3]}, "$"),
		"not base64":     strings.Join([]string{parts[0], parts[1], "!!!", parts[3]}, "$"),
		"short hash":     strings.Join([]string{parts[0], parts[1], parts[2], parts[3][:10]}, "$"),
		"missing part":   strings.Join(parts[:3], "$"),
		"extra part":     good + "$x",
		"empty":          "",
		"garbage":        "hunter2",
	}
	for name, encoded := range cases {
		if verifyPassword(encoded, "a password long enough") {
			t.Errorf("%s: a damaged hash verified", name)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	old := passwordHasher{iterations: 1000}
	current := passwordHasher{iterations: 2000}
	oldHash, _ := old.Hash("some long password")
	curHash, _ := current.Hash("some long password")
	if !current.needsRehash(oldHash) {
		t.Error("a hash with a lower stored count must be upgraded")
	}
	if current.needsRehash(curHash) {
		t.Error("a hash at the current count must be left alone")
	}
	if old.needsRehash(curHash) {
		t.Error("a stronger hash must not be downgraded")
	}
	if !current.needsRehash("pbkdf2-sha1$2000$AAAA$AAAA") || !current.needsRehash("argon2id$x") || !current.needsRehash("") {
		t.Error("an unknown scheme must be flagged for upgrade")
	}
	// An old hash still verifies with the old count, which is what lets login upgrade it.
	if !verifyPassword(oldHash, "some long password") {
		t.Error("the old hash no longer verifies")
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		name, username, password string
		ok                       bool
	}{
		{"12 is enough", "anna", "abcdefghijkl", true},
		{"11 is not", "anna", "abcdefghijk", false},
		{"empty", "anna", "", false},
		{"128 is enough", "anna", strings.Repeat("a", 128), true},
		{"129 is not", "anna", strings.Repeat("a", 129), false},
		{"counted in characters, not bytes", "anna", strings.Repeat("é", 12), true},
		{"128 multibyte characters", "anna", strings.Repeat("é", 128), true},
		{"equal to the username", "administrator", "administrator", false},
		{"equal ignoring case", "Administrator", "aDMINISTRATOR", false},
		{"contains the username but is longer", "anna", "anna-anna-anna", true},
		{"spaces are allowed", "anna", "four words and a space", true},
		{"no composition rules", "anna", "aaaaaaaaaaaa", true},
	}
	for _, c := range cases {
		err := checkPasswordPolicy(c.username, c.password)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && strings.Contains(err.Error(), c.password) && c.password != "" {
			t.Errorf("%s: the error repeats the password: %q", c.name, err)
		}
	}
}

func TestProductionCostAndTime(t *testing.T) {
	if defaultPasswordHasher.iterations != 600000 {
		t.Fatalf("production iterations = %d, want 600000 (OWASP minimum for PBKDF2-HMAC-SHA256)", defaultPasswordHasher.iterations)
	}
	start := time.Now()
	encoded, err := defaultPasswordHasher.Hash("timing sample password")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "pbkdf2-sha256$600000$") {
		t.Fatalf("unexpected format %q", encoded)
	}
	t.Logf("one production hash took %v on this machine", elapsed)
}
