package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Users. The interface is what the rest of camui sees, so the file can be
// replaced (SQLite, an identity provider) without touching the login code.
// ---------------------------------------------------------------------------

const (
	usersFileName    = "users.json"
	sessionsFileName = "sessions.json"
	authFileVersion  = 1

	// roleAdmin is the only role for now. It is stored and returned and
	// enforced nowhere beyond what tlsReadOnly already does.
	roleAdmin = "admin"
)

// User is one account. Hash is the self-describing password hash (auth_password.go).
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Hash      string    `json:"hash"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func validUsername(name string) bool { return usernamePattern.MatchString(name) }

// normalizeUsername is the key under which a name is compared and throttled.
func normalizeUsername(name string) string { return strings.ToLower(name) }

// UserStore keeps accounts and the per-username failure state the lockout needs.
type UserStore interface {
	// Get finds a user by name, ignoring case. A missing user is (zero, false, nil).
	Get(username string) (User, bool, error)
	GetByID(id string) (User, bool, error)
	List() ([]User, error)
	// Put creates the user or, when the ID exists, replaces it. Names are unique ignoring case.
	Put(u User) error
	Delete(id string) error

	// Failure state: consecutive failed logins per (normalized) username, kept for
	// as long as the store lives. A store that persists it may; the file store does not.
	Failure(username string) (FailureState, bool)
	SetFailure(username string, state FailureState)
	ClearFailure(username string)
}

type usersDoc struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
}

// fileUserStore is users.json in the auth folder. It re-reads the file when it
// changes, because `camui.exe -set-password` is another process.
type fileUserStore struct {
	*failureTable
	file *privateJSON

	mu    sync.Mutex
	users []User
}

func newFileUserStore(dir string, now func() time.Time) *fileUserStore {
	return &fileUserStore{
		failureTable: newFailureTable(maxFailureEntries, failureForget, now),
		file:         newPrivateJSON(dir, usersFileName),
	}
}

// loadLocked refreshes s.users from disk if the file changed. Callers hold s.mu.
func (s *fileUserStore) loadLocked() error {
	var doc usersDoc
	changed, err := s.file.readIfChanged(&doc)
	if err != nil {
		return fmt.Errorf("%s: %w", usersFileName, err)
	}
	if !changed {
		return nil
	}
	if doc.Version != authFileVersion && (doc.Version != 0 || len(doc.Users) != 0) {
		s.file.forget()
		return fmt.Errorf("%s: unsupported version %d", usersFileName, doc.Version)
	}
	seen := map[string]bool{}
	for _, u := range doc.Users {
		key := normalizeUsername(u.Username)
		if err := checkUser(u); err != nil || seen[key] {
			s.file.forget()
			return fmt.Errorf("%s: invalid record for %q", usersFileName, u.Username)
		}
		seen[key] = true
	}
	s.users = doc.Users
	return nil
}

func checkUser(u User) error {
	switch {
	case u.ID == "":
		return errors.New("a user needs an id")
	case !validUsername(u.Username):
		return errors.New("the username must be 1 to 64 characters from A-Z a-z 0-9 . _ -")
	case u.Hash == "":
		return errors.New("a user needs a password hash")
	case u.Role == "":
		return errors.New("a user needs a role")
	}
	return nil
}

func (s *fileUserStore) Get(username string) (User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return User{}, false, err
	}
	key := normalizeUsername(username)
	for _, u := range s.users {
		if normalizeUsername(u.Username) == key {
			return u, true, nil
		}
	}
	return User{}, false, nil
}

func (s *fileUserStore) GetByID(id string) (User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return User{}, false, err
	}
	for _, u := range s.users {
		if u.ID == id {
			return u, true, nil
		}
	}
	return User{}, false, nil
}

func (s *fileUserStore) List() ([]User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return append([]User(nil), s.users...), nil
}

func (s *fileUserStore) Put(u User) error {
	if err := checkUser(u); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	next := append([]User(nil), s.users...)
	replaced := false
	for i, existing := range next {
		if existing.ID == u.ID {
			next[i], replaced = u, true
		} else if normalizeUsername(existing.Username) == normalizeUsername(u.Username) {
			return errors.New("that username is taken")
		}
	}
	if !replaced {
		next = append(next, u)
	}
	return s.saveLocked(next)
}

func (s *fileUserStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return err
	}
	next := make([]User, 0, len(s.users))
	for _, u := range s.users {
		if u.ID != id {
			next = append(next, u)
		}
	}
	if len(next) == len(s.users) {
		return nil
	}
	return s.saveLocked(next)
}

func (s *fileUserStore) saveLocked(next []User) error {
	if err := s.file.write(usersDoc{Version: authFileVersion, Users: next}); err != nil {
		return err
	}
	s.users = next
	return nil
}

// ---------------------------------------------------------------------------
// privateJSON: one JSON document in the auth folder, written through a temp file
// restricted to the owner and renamed into place, re-read when it changes.
// ---------------------------------------------------------------------------

type privateJSON struct {
	dir, path string
	stamp     fileStamp
	dirReady  bool
}

type fileStamp struct {
	mod  time.Time
	size int64
	set  bool
}

func newPrivateJSON(dir, name string) *privateJSON {
	return &privateJSON{dir: dir, path: filepath.Join(dir, name)}
}

// forget makes the next read parse the file again.
func (f *privateJSON) forget() { f.stamp = fileStamp{} }

// readIfChanged parses the file into v when it differs from the last read. A
// missing file is an empty document. It reports whether v was filled.
func (f *privateJSON) readIfChanged(v any) (bool, error) {
	info, err := os.Stat(f.path)
	if errors.Is(err, os.ErrNotExist) {
		f.stamp = fileStamp{}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	now := fileStamp{mod: info.ModTime(), size: info.Size(), set: true}
	if now == f.stamp {
		return false, nil
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, errors.New("not valid JSON")
	}
	f.stamp = now
	return true, nil
}

func (f *privateJSON) write(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if !f.dirReady {
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			return err
		}
		if err := restrictToOwner(f.dir, true); err != nil {
			return err
		}
		f.dirReady = true
	}
	if err := writePrivateFile(f.path, data); err != nil {
		return err
	}
	if info, err := os.Stat(f.path); err == nil {
		f.stamp = fileStamp{mod: info.ModTime(), size: info.Size(), set: true}
	}
	return nil
}

// newID is 16 random bytes as 32 hex characters.
func newID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic("no randomness: " + err.Error())
	}
	return hex.EncodeToString(raw)
}
