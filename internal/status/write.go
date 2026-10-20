package status

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	replaceAttempts   = 3
	replaceRetryDelay = 100 * time.Millisecond
)

// Options configures a Writer. The zero value is usable.
type Options struct {
	// Redactor masks the whole serialised text before it is written. Nil gets
	// a fresh Redactor (URL userinfo is always masked).
	Redactor *Redactor
	// RetryDelay between replace attempts (default 100 ms; 3 attempts).
	RetryDelay time.Duration
}

// Writer atomically replaces status.json: temp file in the same directory,
// Sync, then a replace that works while readers hold the file open (Windows:
// POSIX-semantics rename). Safe for concurrent use.
type Writer struct {
	path     string
	redactor *Redactor
	delay    time.Duration

	mu          sync.Mutex
	consecutive int
	fallbacks   int
	replace     func(tmp, path string) (bool, error)
}

func NewWriter(path string, opts Options) *Writer {
	w := &Writer{path: path, redactor: opts.Redactor, delay: opts.RetryDelay, replace: replaceFile}
	if w.redactor == nil {
		w.redactor = NewRedactor()
	}
	if w.delay <= 0 {
		w.delay = replaceRetryDelay
	}
	return w
}

// Redactor returns the redactor so the caller can register secrets.
func (w *Writer) Redactor() *Redactor { return w.redactor }

// ConsecutiveFailures counts Write calls that failed since the last success.
func (w *Writer) ConsecutiveFailures() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.consecutive
}

// Marshal serialises doc as status.json text: indented, UTF-8 without BOM, "\n"
// line ends, no HTML escaping. It does not redact.
func Marshal(doc *Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Write redacts and writes doc. A failure means this cycle is skipped: the
// previous file is untouched and still valid, the temp file is left in place
// (overwritten by the next call), and the caller tries again at the next
// heartbeat. The replace is attempted at most 3 times, 100 ms apart, and only
// for sharing violation / access denied.
func (w *Writer) Write(doc *Document) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer func() {
		if err != nil {
			w.consecutive++
		} else {
			w.consecutive = 0
		}
	}()

	text, err := Marshal(doc)
	if err != nil {
		return fmt.Errorf("status: serialise: %w", err)
	}
	text = []byte(w.redactor.String(string(text)))
	if !json.Valid(text) {
		return fmt.Errorf("status: redaction produced invalid JSON, write skipped")
	}

	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return fmt.Errorf("status: %w", err)
	}
	tmp := w.path + ".tmp"
	if err := writeSynced(tmp, text); err != nil {
		return fmt.Errorf("status: temp file: %w", err)
	}
	for attempt := 1; ; attempt++ {
		fallback, rerr := w.replace(tmp, w.path)
		if fallback {
			w.fallbacks++
		}
		if rerr == nil {
			return nil
		}
		if attempt == replaceAttempts || !isSharingError(rerr) {
			return fmt.Errorf("status: replace: %w", rerr)
		}
		time.Sleep(w.delay)
	}
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
