package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	authLogName     = "auth.log"
	authLogMaxBytes = 1 << 20 // then auth.log becomes auth.log.1 (the previous one is dropped)
	maxLoggedName   = 64
)

// authLog is an append-only record of sign-in activity for the Operator: time,
// peer address, attempted username, event and result. It never holds a password,
// a hash or a token. The folder is the owner-only auth folder.
type authLog struct {
	dir string
	now func() time.Time

	mu   sync.Mutex
	file *os.File
	size int64
}

func newAuthLog(dir string, now func() time.Time) *authLog { return &authLog{dir: dir, now: now} }

// cleanLoggedName bounds and neutralises an attacker-chosen string: control
// characters (a newline would forge a log line) are dropped, the rest is cut to
// maxLoggedName characters.
func cleanLoggedName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) || !unicode.IsPrint(r) {
			continue
		}
		if n++; n > maxLoggedName {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// record appends one line. A failure to log is reported to the caller, who logs
// it to the console; it never blocks a sign-in.
func (l *authLog) record(ip, username, event, result string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.openLocked(); err != nil {
		return err
	}
	line := fmt.Sprintf("%s ip=%s event=%s result=%s user=%q\n", l.now().UTC().Format(time.RFC3339), cleanLoggedName(ip), event, result, cleanLoggedName(username))
	n, err := l.file.WriteString(line)
	l.size += int64(n)
	return err
}

func (l *authLog) openLocked() error {
	if l.file == nil {
		if err := l.openFileLocked(); err != nil {
			return err
		}
	}
	if l.size < authLogMaxBytes {
		return nil
	}
	path := filepath.Join(l.dir, authLogName)
	l.file.Close()
	l.file = nil
	_ = os.Remove(path + ".1")
	if err := os.Rename(path, path+".1"); err != nil {
		return err
	}
	return l.openFileLocked()
}

func (l *authLog) openFileLocked() error {
	path := filepath.Join(l.dir, authLogName)
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	fresh := os.IsNotExist(statErr)
	if fresh {
		if err := restrictToOwner(l.dir, true); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if fresh {
		if err := restrictToOwner(path, false); err != nil {
			f.Close()
			return err
		}
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.file, l.size = f, info.Size()
	return nil
}

func (l *authLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}
