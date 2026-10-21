package web

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The facades below are what `holocron user` and `holocron tls` call: thin, explicit-parameter
// wrappers over the auth service and the certificate manager that the web role itself runs on.
// No hashing, store or certificate code is repeated here.

var (
	// ErrUserExists: `user add` for a name that is taken (names compare ignoring case).
	ErrUserExists = errors.New("that user exists already")
	// ErrNoSuchUser: `user passwd` or `user remove` for a name nobody has.
	ErrNoSuchUser = errors.New("no such user")
	// ErrLastAdmin: removing or demoting the only admin, which would leave nobody able to manage.
	ErrLastAdmin = errors.New("that is the last admin: make another user an admin first")
)

// The roles a user may have (`holocron user add --role`, `holocron user role`).
const (
	RoleAdmin  = roleAdmin
	RoleViewer = roleViewer
)

// IsInputError reports whether err is a refusal caused by what the caller supplied (a bad user
// name, a weak password) rather than a failure of the store.
func IsInputError(err error) bool {
	var bad inputError
	return errors.As(err, &bad)
}

// UserInfo is one account as `user list` shows it: never the hash. Locked is known only to the
// running web app (its sign-in failures live in its memory); the command line leaves it false.
type UserInfo struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
	Locked    bool      `json:"locked"`
}

// UserAdmin manages the login users in an auth folder.
type UserAdmin struct {
	svc *authService
	// live: svc is the running LAN listener's own service, so its lockouts are visible and an
	// unlock applies at once. web: the caller is the Admin tab, where the last admin can never
	// be removed (from the command line the very last user may go, as before roles).
	live, web bool
	keep      string // the session token spared when a user's sessions end (the page's own)
}

// adminMu serialises the last-admin checks with the writes that depend on them.
var adminMu sync.Mutex

// Users opens the auth folder (empty: the web app's default folder). log receives the service's
// own records (component auth: user.set_password, user.role, user.remove, user.unlock; never a password).
func Users(authDir string, log *slog.Logger) *UserAdmin {
	return usersWith(authDir, log, defaultPasswordHasher)
}

func usersWith(authDir string, log *slog.Logger, hasher passwordHasher) *UserAdmin {
	if authDir == "" {
		authDir = defaultAuthDir()
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &UserAdmin{svc: newAuthService(authDir, newFileUserStore(authDir, time.Now), log, time.Now, hasher)}
}

// Dir is the auth folder in use.
func (u *UserAdmin) Dir() string { return u.svc.dir }

// sparing is u acting for a request whose own session (token) survives the change.
func (u *UserAdmin) sparing(token string) *UserAdmin {
	c := *u
	c.keep = token
	return &c
}

func parseRole(role string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(role))
	if !validRole(r) {
		return "", inputError{"the role must be admin or viewer"}
	}
	return r, nil
}

// Add creates an admin user; ErrUserExists when the name is taken.
func (u *UserAdmin) Add(actor, name, password string) error {
	return u.AddRole(actor, name, password, roleAdmin)
}

// AddRole creates a user with the role admin or viewer; ErrUserExists when the name is taken.
func (u *UserAdmin) AddRole(actor, name, password, role string) error {
	role, err := parseRole(role)
	if err != nil {
		return err
	}
	if validUsername(name) {
		if _, found, err := u.svc.users.Get(name); err != nil {
			return fmt.Errorf("cannot read the user file: %w", err)
		} else if found {
			return ErrUserExists
		}
	}
	_, err = u.svc.savePassword(actor, name, password, role, "")
	return err
}

// SetPassword changes an existing user's password and ends that user's sessions (all but the
// page's own when it is the caller's); ErrNoSuchUser when the name is unknown.
func (u *UserAdmin) SetPassword(actor, name, password string) error {
	if validUsername(name) {
		if _, found, err := u.svc.users.Get(name); err != nil {
			return fmt.Errorf("cannot read the user file: %w", err)
		} else if !found {
			return ErrNoSuchUser
		}
	}
	_, err := u.svc.savePassword(actor, name, password, roleAdmin, u.keep)
	return err
}

// admins counts the admins among users.
func admins(users []User) int {
	n := 0
	for _, x := range users {
		if isAdmin(x) {
			n++
		}
	}
	return n
}

// SetRole gives a user the role admin or viewer and ends that user's other sessions. Demoting the
// last admin is ErrLastAdmin; changed is false when the user had the role already.
func (u *UserAdmin) SetRole(actor, name, role string) (changed bool, err error) {
	started := time.Now()
	fail := func(level slog.Level, err error) (bool, error) {
		emit(u.svc.log, level, "user.role", actor, name, "fail", started, "error", err.Error())
		return false, err
	}
	if role, err = parseRole(role); err != nil {
		return fail(slog.LevelWarn, err)
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	users, err := u.svc.users.List()
	if err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot read the user file: %w", err))
	}
	user, found, err := u.svc.users.Get(name)
	if err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot read the user file: %w", err))
	}
	if !found {
		return fail(slog.LevelWarn, ErrNoSuchUser)
	}
	if effectiveRole(user.Role) == role {
		emit(u.svc.log, slog.LevelInfo, "user.role", actor, user.Username, "unchanged", started, "role", role)
		return false, nil
	}
	if isAdmin(user) && admins(users) <= 1 {
		return fail(slog.LevelWarn, ErrLastAdmin)
	}
	user.Role = role
	if err := u.svc.users.Put(user); err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot save the user file: %w", err))
	}
	if err := u.svc.sessions.DeleteUserExcept(user.ID, u.keep); err != nil {
		return fail(slog.LevelError, fmt.Errorf("the role was saved but the user's sessions could not be ended: %w", err))
	}
	_ = u.svc.authLog.record("local", user.Username, "role-"+role, "ok")
	emit(u.svc.log, slog.LevelInfo, "user.role", actor, user.Username, "ok", started, "role", role)
	return true, nil
}

// Remove deletes the user and every session of that user; ErrNoSuchUser when unknown. The last
// admin is ErrLastAdmin while other users remain (and always from the Admin tab).
func (u *UserAdmin) Remove(actor, name string) error {
	started := time.Now()
	fail := func(level slog.Level, err error) error {
		emit(u.svc.log, level, "user.remove", actor, name, "fail", started, "error", err.Error())
		return err
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	users, err := u.svc.users.List()
	if err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot read the user file: %w", err))
	}
	user, found, err := u.svc.users.Get(name)
	if err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot read the user file: %w", err))
	}
	if !found {
		return fail(slog.LevelWarn, ErrNoSuchUser)
	}
	if isAdmin(user) && admins(users) <= 1 && (len(users) > 1 || u.web) {
		return fail(slog.LevelWarn, ErrLastAdmin)
	}
	if err := u.svc.users.Delete(user.ID); err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot save the user file: %w", err))
	}
	if err := u.svc.sessions.DeleteUser(user.ID); err != nil {
		return fail(slog.LevelError, fmt.Errorf("the user was removed but its sessions could not be ended: %w", err))
	}
	_ = u.svc.authLog.record("local", user.Username, "remove", "ok")
	emit(u.svc.log, slog.LevelInfo, "user.remove", actor, user.Username, "ok", started)
	return nil
}

// List returns the users in file order, without hashes.
func (u *UserAdmin) List() ([]UserInfo, error) {
	users, err := u.svc.ListUsers()
	if err != nil {
		return nil, fmt.Errorf("cannot read the user file: %w", err)
	}
	out := make([]UserInfo, 0, len(users))
	for _, x := range users {
		info := UserInfo{Username: x.Username, Role: effectiveRole(x.Role), CreatedAt: x.CreatedAt}
		if u.live {
			_, info.Locked = u.svc.lock.locked(x.Username)
		}
		out = append(out, info)
	}
	return out, nil
}

// Unlock asks a running web app to forget the failed sign-ins of a name (the cross-process marker
// the service consumes at the next attempt); in the running web app itself it applies at once.
func (u *UserAdmin) Unlock(actor, name string) error {
	if err := u.svc.Unlock(actor, name); err != nil {
		return err
	}
	if u.live && u.svc.consumeUnlock(name) {
		u.svc.lock.reset(name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// TLS: the local certificate authority
// ---------------------------------------------------------------------------

func tlsManager(tlsDir string) *certManager {
	if tlsDir == "" {
		tlsDir = defaultTLSDir()
	}
	return newCertManager(tlsDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// CAFor returns the CA's description, creating the CA when there is none (what -print-ca does).
func CAFor(tlsDir string) (CAInfo, error) { return tlsManager(tlsDir).CAInfo() }

// CACertificatePEM is the CA certificate of the folder - the one file that may leave this machine.
// It never returns a key; a missing CA is an error wrapping os.ErrNotExist.
func CACertificatePEM(tlsDir string) ([]byte, error) {
	pem, err := tlsManager(tlsDir).CACertPEM()
	if err != nil {
		return nil, err
	}
	if containsKeyMaterial(pem) {
		return nil, errors.New("refusing to print: the CA file holds more than a certificate")
	}
	return pem, nil
}

// tlsFiles are the files the web role keeps in the TLS folder.
var tlsFiles = []string{caCertFile, caKeyFile, leafCertFile, leafKeyFile}

// RotateCA deletes the CA and server certificate files (and the folder when nothing else is in it),
// so the next web start creates a fresh CA and leaf. It returns the paths removed. Files it does
// not know are left alone and reported in left.
func RotateCA(tlsDir string) (removed, left []string, err error) {
	if tlsDir == "" {
		tlsDir = defaultTLSDir()
	}
	entries, err := os.ReadDir(tlsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	known := map[string]bool{}
	for _, f := range tlsFiles {
		known[f] = true
	}
	for _, entry := range entries {
		path := filepath.Join(tlsDir, entry.Name())
		if entry.IsDir() || !known[entry.Name()] {
			left = append(left, path)
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, left, fmt.Errorf("cannot delete %s: %w", path, err)
		}
		removed = append(removed, path)
	}
	if len(left) == 0 {
		_ = os.Remove(tlsDir)
	}
	return removed, left, nil
}
