package web

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Passwords are stored as a self-describing string so the scheme can change later
// (Argon2id, say) and old hashes are upgraded the next time their owner signs in:
//
//	pbkdf2-sha256$<iterations>$<base64 salt>$<base64 hash>
const (
	pbkdf2Scheme = "pbkdf2-sha256"

	// 600,000 is the OWASP Password Storage cheat sheet's minimum for PBKDF2-HMAC-SHA256.
	pbkdf2Iterations = 600000
	// A stored count above this is not a password hash, it is a way to stall the server.
	pbkdf2MaxIterations = 10_000_000

	passwordSaltBytes = 16
	passwordHashBytes = 32

	minPasswordLength = 12
	maxPasswordLength = 128
)

// passwordHasher carries the cost so tests can run with a small one.
type passwordHasher struct{ iterations int }

var defaultPasswordHasher = passwordHasher{iterations: pbkdf2Iterations}

var b64 = base64.RawStdEncoding

// Hash returns the encoded hash of password with a fresh random salt.
func (h passwordHasher) Hash(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum, err := pbkdf2.Key(sha256.New, password, salt, h.iterations, passwordHashBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Scheme, h.iterations, b64.EncodeToString(salt), b64.EncodeToString(sum)), nil
}

// needsRehash reports whether encoded was made with a scheme or a cost weaker
// than this hasher's, so that a successful login can replace it.
func (h passwordHasher) needsRehash(encoded string) bool {
	_, iterations, _, _, ok := parseHash(encoded)
	return !ok || iterations < h.iterations
}

func parseHash(encoded string) (scheme string, iterations int, salt, sum []byte, ok bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Scheme {
		return "", 0, nil, nil, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 1 || n > pbkdf2MaxIterations {
		return "", 0, nil, nil, false
	}
	salt, err1 := b64.DecodeString(parts[2])
	sum, err2 := b64.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(salt) < 8 || len(sum) != passwordHashBytes {
		return "", 0, nil, nil, false
	}
	return parts[0], n, salt, sum, true
}

// verifyPassword compares in constant time. A damaged or unknown hash never verifies.
func verifyPassword(encoded, password string) bool {
	_, iterations, salt, want, ok := parseHash(encoded)
	if !ok {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// checkPasswordPolicy is length (in characters) and "not the username". Nothing
// else is required: composition rules make people choose worse passwords.
// The error never repeats the password.
func checkPasswordPolicy(username, password string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case n < minPasswordLength:
		return fmt.Errorf("the password must be at least %d characters", minPasswordLength)
	case n > maxPasswordLength:
		return fmt.Errorf("the password must be at most %d characters", maxPasswordLength)
	case strings.EqualFold(password, username):
		return errors.New("the password must not be the username")
	}
	return nil
}
