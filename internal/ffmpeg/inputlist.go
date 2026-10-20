package ffmpeg

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"holocron/internal/osx"
)

const (
	listPrefix = "ffin-"
	listSuffix = ".ffconcat"
	// StaleListAge is the age after which SweepInputLists removes a leftover list.
	StaleListAge = time.Hour
)

// WriteInputList writes the ffconcat list that carries a direct input's URL (SP1b), so the camera
// password never appears on the ffmpeg command line:
//
//	ffconcat version 1.0
//	file 'rtsp://user:pass@host/stream'
//	option rtsp_transport tcp
//	option timeout 10000000
//
// The file is created in dir (created if missing, owner-only) as ffin-<random>.ffconcat, with
// O_EXCL and mode 0600, restricted with p.SetOwnerOnly BEFORE the secret is written; a failed
// restriction removes the file and fails (fail closed). Stale lists older than StaleListAge in dir
// are swept first, best effort. cleanup removes the file; call it after the child has exited (on
// Windows the file is share-locked while ffmpeg runs). It is safe to call more than once.
//
// ffmpeg still prints the credentialed URL in its own error text; the stderr redaction in Child
// is mandatory, this does not replace it. The URL stays in this process's memory and on disk
// until cleanup.
func WriteInputList(p osx.Platform, dir, url string, timeoutUS int64, transport string) (path string, cleanup func(), err error) {
	if url == "" || strings.ContainsAny(url, "\r\n\x00") {
		return "", nil, errors.New("ffmpeg: the input URL is empty or contains a line break")
	}
	if transport == "" || strings.ContainsAny(transport, " \t\r\n'\\\x00") {
		return "", nil, fmt.Errorf("ffmpeg: unusable RTSP transport %q", transport)
	}
	if timeoutUS <= 0 {
		return "", nil, errors.New("ffmpeg: the RTSP timeout must be positive")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	SweepInputLists(dir, StaleListAge)

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", nil, err
	}
	path = filepath.Join(dir, listPrefix+hex.EncodeToString(rnd[:])+listSuffix)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, func(), error) {
		f.Close()
		os.Remove(path)
		return "", nil, err
	}
	if err := p.SetOwnerOnly(path); err != nil {
		return fail(fmt.Errorf("ffmpeg: cannot restrict the input list to its owner: %w", err))
	}
	text := "ffconcat version 1.0\n" +
		"file " + concatQuote(url) + "\n" +
		"option rtsp_transport " + transport + "\n" +
		"option timeout " + fmt.Sprint(timeoutUS) + "\n"
	if _, err := f.WriteString(text); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", nil, err
	}
	var once sync.Once
	return path, func() { once.Do(func() { os.Remove(path) }) }, nil
}

// concatQuote quotes s for an ffconcat token (libavutil av_get_token): single quotes, and a single
// quote inside becomes '\” (close, escaped quote, reopen). A backslash is literal inside quotes.
func concatQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SweepInputLists removes list files (ffin-*.ffconcat, regular files only) in dir whose
// modification time is older than maxAge, and returns how many it removed. A list a running child
// holds open on Windows cannot be removed and is left. Errors are ignored: this is housekeeping.
func SweepInputLists(dir string, maxAge time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, listPrefix) || !strings.HasSuffix(name, listSuffix) {
			continue
		}
		full := filepath.Join(dir, name)
		fi, err := os.Lstat(full)
		if err != nil || !fi.Mode().IsRegular() || time.Since(fi.ModTime()) < maxAge {
			continue
		}
		if os.Remove(full) == nil {
			removed++
		}
	}
	return removed
}
