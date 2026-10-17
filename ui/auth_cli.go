package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

// runSetPassword implements -set-password: create the user (admin) or change the
// password, end all of that user's sessions, say nothing about the password.
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
	if err := checkPasswordPolicy(username, password); err != nil {
		fmt.Fprintln(errOut, err.Error()+"; nothing changed")
		return 2
	}
	now := time.Now()
	store := newFileUserStore(cfg.AuthDir, time.Now)
	hash, err := hasher.Hash(password)
	if err != nil {
		fmt.Fprintln(errOut, "cannot hash the password:", err)
		return 1
	}
	user, existing, err := store.Get(username)
	if err != nil {
		fmt.Fprintln(errOut, "cannot read the user file:", err)
		return 1
	}
	verb := "created"
	if existing {
		verb = "updated the password of"
		user.Hash = hash
	} else {
		user = User{ID: newID(), Username: username, Hash: hash, Role: roleAdmin, CreatedAt: now.UTC()}
	}
	if err := store.Put(user); err != nil {
		fmt.Fprintln(errOut, "cannot save the user:", err)
		return 1
	}
	sessions := newSessionStore(cfg.AuthDir, time.Now)
	if err := sessions.DeleteUser(user.ID); err != nil {
		fmt.Fprintln(errOut, "the password was saved but its sessions could not be ended:", err)
		return 1
	}
	_ = newAuthLog(cfg.AuthDir, time.Now).record("local", user.Username, "set-password", "ok")
	fmt.Fprintf(out, "%s user %s (role %s); every session of that user was ended\n", verb, user.Username, user.Role)
	return 0
}

// runUnlock implements -unlock: it asks a running camui to forget the failed-login
// count of a username. A camui that is not running has nothing to clear.
func runUnlock(cfg Config, username string, out, errOut io.Writer) int {
	if len(username) > 64 {
		fmt.Fprintln(errOut, "the username is longer than 64 characters")
		return 2
	}
	marker := unlockMarker(cfg.AuthDir, username)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		fmt.Fprintln(errOut, "cannot prepare the auth folder:", err)
		return 1
	}
	if err := restrictToOwner(cfg.AuthDir, true); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if err := writePrivateFile(marker, []byte(time.Now().UTC().Format(time.RFC3339))); err != nil {
		fmt.Fprintln(errOut, "cannot write the unlock request:", err)
		return 1
	}
	_ = newAuthLog(cfg.AuthDir, time.Now).record("local", username, "unlock", "requested")
	fmt.Fprintf(out, "unlock requested for %q: a running camui forgets its failed sign-ins at the next attempt for that name. Restarting camui clears all of them too.\n", username)
	return 0
}
