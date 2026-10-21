package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Session lifetimes. A session ends when it has been idle for sessionIdleTimeout
// or when sessionLifetime has passed since the login, whichever is first.
const (
	sessionIdleTimeout = 7 * 24 * time.Hour
	sessionLifetime    = 30 * 24 * time.Hour

	// A device that never signs out still cannot pile up sessions without bound.
	maxSessionsPerUser = 20

	// lastSeen is written back at most this often; the session file is not
	// rewritten on every request.
	lastSeenGranularity = 10 * time.Minute
)

// Session is what the server remembers about a browser. The token itself exists
// only in the browser's cookie: the server keeps its SHA-256, so a copy of
// sessions.json cannot be replayed.
type Session struct {
	TokenHash string    `json:"tokenHash"`
	UserID    string    `json:"userId"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type sessionsDoc struct {
	Version  int       `json:"version"`
	Sessions []Session `json:"sessions"`
}

// sessionStore is sessions.json in the auth folder, with the same rules as the
// user file: owner-only, atomic, and re-read when another process changes it.
type sessionStore struct {
	now  func() time.Time
	file *privateJSON

	mu       sync.Mutex
	sessions map[string]Session // by token hash
}

func newSessionStore(dir string, now func() time.Time) *sessionStore {
	return &sessionStore{now: now, file: newPrivateJSON(dir, sessionsFileName), sessions: map[string]Session{}}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// loadLocked refreshes from disk. A file that cannot be read is treated as no
// sessions at all: nobody is signed in, which fails closed.
func (s *sessionStore) loadLocked() {
	var doc sessionsDoc
	changed, err := s.file.readIfChanged(&doc)
	if err != nil {
		s.sessions = map[string]Session{}
		s.file.forget()
		return
	}
	if !changed {
		return
	}
	s.sessions = make(map[string]Session, len(doc.Sessions))
	if doc.Version == authFileVersion {
		for _, sess := range doc.Sessions {
			s.sessions[sess.TokenHash] = sess
		}
	}
}

func (s *sessionStore) saveLocked() error {
	list := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		list = append(list, sess)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	return s.file.write(sessionsDoc{Version: authFileVersion, Sessions: list})
}

func (s *sessionStore) expired(sess Session, now time.Time) bool {
	return !now.Before(sess.ExpiresAt) || !now.Before(sess.LastSeen.Add(sessionIdleTimeout))
}

// Create starts a session for userID and returns its token. Every login gets a new one.
func (s *sessionStore) Create(userID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	now := s.now()
	for hash, sess := range s.sessions {
		if s.expired(sess, now) {
			delete(s.sessions, hash)
		}
	}
	for s.countForLocked(userID) >= maxSessionsPerUser {
		s.dropOldestLocked(userID)
	}
	s.sessions[hashToken(token)] = Session{TokenHash: hashToken(token), UserID: userID, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(sessionLifetime)}
	if err := s.saveLocked(); err != nil {
		return "", fmt.Errorf("save sessions: %w", err)
	}
	return token, nil
}

// Lookup finds the live session for a cookie value. An expired one is removed.
func (s *sessionStore) Lookup(token string) (Session, bool) {
	if token == "" || len(token) > 128 {
		return Session{}, false
	}
	hash := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	sess, ok := s.sessions[hash]
	if !ok {
		return Session{}, false
	}
	now := s.now()
	if s.expired(sess, now) {
		delete(s.sessions, hash)
		_ = s.saveLocked()
		return Session{}, false
	}
	if now.Sub(sess.LastSeen) >= lastSeenGranularity {
		sess.LastSeen = now
		s.sessions[hash] = sess
		_ = s.saveLocked() // best effort: the session is valid either way
	}
	return sess, true
}

// Delete ends one session (logout). Unknown tokens are not an error.
func (s *sessionStore) Delete(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	hash := hashToken(token)
	if _, ok := s.sessions[hash]; !ok {
		return nil
	}
	delete(s.sessions, hash)
	return s.saveLocked()
}

// DeleteUser ends every session of a user (password change, user removal).
func (s *sessionStore) DeleteUser(userID string) error { return s.DeleteUserExcept(userID, "") }

// DeleteUserExcept ends every session of a user but the one of keepToken (an admin changing his
// own password or role in the page stays signed in there; "" spares none).
func (s *sessionStore) DeleteUserExcept(userID, keepToken string) error {
	keep := ""
	if keepToken != "" {
		keep = hashToken(keepToken)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	n := len(s.sessions)
	for hash, sess := range s.sessions {
		if sess.UserID == userID && hash != keep {
			delete(s.sessions, hash)
		}
	}
	if len(s.sessions) == n {
		return nil
	}
	return s.saveLocked()
}

func (s *sessionStore) countFor(userID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countForLocked(userID)
}

func (s *sessionStore) countForLocked(userID string) int {
	n := 0
	for _, sess := range s.sessions {
		if sess.UserID == userID {
			n++
		}
	}
	return n
}

func (s *sessionStore) dropOldestLocked(userID string) {
	var oldest string
	var oldestAt time.Time
	for hash, sess := range s.sessions {
		if sess.UserID == userID && (oldest == "" || sess.CreatedAt.Before(oldestAt)) {
			oldest, oldestAt = hash, sess.CreatedAt
		}
	}
	delete(s.sessions, oldest)
}
