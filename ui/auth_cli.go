package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// unlockDirName holds one marker file per username that `camui.exe -unlock` was
// asked to clear. The failure counters live in the running camui's memory, which
// another process cannot reach, so the running one looks for the marker at each
// sign-in attempt.
const unlockDirName = "unlock"

func unlockMarker(dir, username string) string {
	sum := sha256.Sum256([]byte(normalizeUsername(username)))
	return filepath.Join(dir, unlockDirName, hex.EncodeToString(sum[:16]))
}

// consumeUnlock reports whether an unlock was requested for username, and uses it up.
func (a *authService) consumeUnlock(username string) bool {
	return os.Remove(unlockMarker(a.dir, username)) == nil
}

// clearUnlockMarkers drops requests left from before this start: a restart
// already cleared every counter.
func clearUnlockMarkers(dir string) { _ = os.RemoveAll(filepath.Join(dir, unlockDirName)) }

const maxPasswordInput = 1024

// readPasswordLine reads one line: the whole of a pipe's first line, or what a
// person types at a console. A trailing CR/LF and a UTF-8 byte-order mark (what
// Windows PowerShell 5.1 can prepend) are removed; nothing else is touched.
func readPasswordLine(in io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(in, maxPasswordInput)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	return strings.TrimPrefix(line, "\ufeff"), nil
}

// inputError marks a refusal caused by what the caller supplied (a bad name, a weak
// password) as opposed to a store failure, so a caller can pick its own exit code or
// HTTP status.
type inputError struct{ msg string }

func (e inputError) Error() string { return e.msg }

// The actions below are plain functions on the auth service: explicit parameters, an
// error back, no flag parsing, no prompting, no os.Exit. The command-line flags and,
// later, a web page or another subcommand are thin wrappers over them.

// SetPassword creates the user (role admin) or changes the password of an existing
// one, and ends every session of that user. created is true for a new user.
func (a *authService) SetPassword(actor, username, password string) (created bool, err error) {
	started := time.Now()
	fail := func(level slog.Level, err error) (bool, error) {
		emit(a.log, level, "user.set_password", actor, username, "fail", started, "error", err.Error())
		return false, err
	}
	if !validUsername(username) {
		return fail(slog.LevelWarn, inputError{"the username must be 1 to 64 characters from A-Z a-z 0-9 . _ -"})
	}
	if err := checkPasswordPolicy(username, password); err != nil {
		return fail(slog.LevelWarn, inputError{err.Error()})
	}
	hash, err := a.hasher.Hash(password)
	if err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot hash the password: %w", err))
	}
	user, existing, err := a.users.Get(username)
	if err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot read the user file: %w", err))
	}
	if existing {
		user.Hash = hash
	} else {
		user = User{ID: newID(), Username: username, Hash: hash, Role: roleAdmin, CreatedAt: a.now().UTC()}
	}
	if err := a.users.Put(user); err != nil {
		return fail(slog.LevelError, fmt.Errorf("cannot save the user: %w", err))
	}
	if err := a.sessions.DeleteUser(user.ID); err != nil {
		return fail(slog.LevelError, fmt.Errorf("the password was saved but its sessions could not be ended: %w", err))
	}
	_ = a.authLog.record("local", user.Username, "set-password", "ok")
	emit(a.log, slog.LevelInfo, "user.set_password", actor, user.Username, "ok", started, "created", !existing)
	return !existing, nil
}

// Unlock asks a running camui to forget the failed-login count of a username (see
// unlockMarker). A camui that is not running has nothing to clear.
func (a *authService) Unlock(actor, username string) error {
	started := time.Now()
	if len(username) > 64 {
		return inputError{"the username is longer than 64 characters"}
	}
	marker := unlockMarker(a.dir, username)
	err := os.MkdirAll(filepath.Dir(marker), 0o700)
	if err == nil {
		err = restrictToOwner(a.dir, true)
	}
	if err == nil {
		err = writePrivateFile(marker, []byte(a.now().UTC().Format(time.RFC3339)))
	}
	if err != nil {
		emit(a.log, slog.LevelError, "user.unlock", actor, username, "fail", started, "error", err.Error())
		return fmt.Errorf("cannot write the unlock request: %w", err)
	}
	_ = a.authLog.record("local", username, "unlock", "requested")
	emit(a.log, slog.LevelInfo, "user.unlock", actor, username, "requested", started)
	return nil
}

// ListUsers returns every user (hashes included: callers must not print them).
func (a *authService) ListUsers() ([]User, error) { return a.users.List() }

// cliAuth is the auth service as the command line uses it: the real store and
// sessions in the auth folder, console records at WARN and above on errOut.
func cliAuth(cfg Config, hasher passwordHasher, errOut io.Writer) *authService {
	log := slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return newAuthService(cfg.AuthDir, newFileUserStore(cfg.AuthDir, time.Now), log, time.Now, hasher)
}

// runSetPassword implements -set-password: a thin wrapper that reads the password
// from standard input and prints the outcome. It never prints the password.
func runSetPassword(cfg Config, username string, stdin io.Reader, out, errOut io.Writer) int {
	return setPassword(cfg, defaultPasswordHasher, username, stdin, out, errOut)
}

func setPassword(cfg Config, hasher passwordHasher, username string, stdin io.Reader, out, errOut io.Writer) int {
	if !validUsername(username) {
		fmt.Fprintln(errOut, "the username must be 1 to 64 characters from A-Z a-z 0-9 . _ -")
		return 2
	}
	if f, ok := stdin.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprint(errOut, "Password for "+username+" (what you type is visible): ")
		}
	}
	password, err := readPasswordLine(stdin)
	if err != nil {
		fmt.Fprintln(errOut, "cannot read the password:", err)
		return 1
	}
	if password == "" {
		fmt.Fprintln(errOut, "no password was given on standard input; nothing changed")
		return 2
	}
	created, err := cliAuth(cfg, hasher, errOut).SetPassword("local", username, password)
	if err != nil {
		var bad inputError
		if errors.As(err, &bad) {
			fmt.Fprintln(errOut, bad.Error()+"; nothing changed")
			return 2
		}
		fmt.Fprintln(errOut, err)
		return 1
	}
	verb := "updated the password of"
	if created {
		verb = "created"
	}
	user, _, _ := newFileUserStore(cfg.AuthDir, time.Now).Get(username)
	fmt.Fprintf(out, "%s user %s (role %s); every session of that user was ended\n", verb, user.Username, user.Role)
	return 0
}

// runUnlock implements -unlock as a thin wrapper over authService.Unlock.
func runUnlock(cfg Config, username string, out, errOut io.Writer) int {
	if err := cliAuth(cfg, defaultPasswordHasher, errOut).Unlock("local", username); err != nil {
		fmt.Fprintln(errOut, err)
		var bad inputError
		if errors.As(err, &bad) {
			return 2
		}
		return 1
	}
	fmt.Fprintf(out, "unlock requested for %q: a running camui forgets its failed sign-ins at the next attempt for that name. Restarting camui clears all of them too.\n", username)
	return 0
}
