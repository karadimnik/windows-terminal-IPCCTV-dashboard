package logx

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBytes is the rotation limit when FileOptions.MaxBytes <= 0 (the PowerShell default, 2 MiB).
const DefaultMaxBytes = 2 << 20

const timeLayout = "2006-01-02T15:04:05.000000-07:00"

// FileOptions configures a RotatingFile.
type FileOptions struct {
	Path     string
	MaxBytes int64 // rotate when the file is at least this big, before appending; <= 0 means DefaultMaxBytes

	// CollapseAfter > 0 turns on repeat collapse: after this many consecutive lines of the same
	// shape (digits stripped) further ones are held back and, when the run ends (a different line,
	// Close, or CollapseFlush of silence-free repetition), the latest one is written with
	// " [+N more similar]" where N counts the held-back lines before it. 0 disables collapse.
	CollapseAfter int
	CollapseFlush time.Duration // longest a repeating run stays unreported; default 60s

	JSON      bool      // notices are written as JSON instead of text
	NoticeOut io.Writer // where a notice goes when the file itself cannot be written; default os.Stderr
	Now       func() time.Time
}

// fileOps are the file system calls, replaceable in tests to inject failures.
type fileOps struct {
	size   func(path string) (int64, error)
	remove func(path string) error
	rename func(from, to string) error
	open   func(path string) (io.WriteCloser, error)
}

// RotatingFile is a Sink for one log file that never fails its caller: Write always reports
// success, never panics, and turns a blocked rotation or a failed append into one notice per
// episode. It does not assume exclusive access to the file: it opens, appends and closes on every
// write and looks at the on-disk size each time. Safe for concurrent use.
type RotatingFile struct {
	mu  sync.Mutex
	o   FileOptions
	fs  fileOps
	max int64

	rotateBlocked bool
	pending       []string // notices waiting to be written ahead of the next successful append
	drop          struct {
		n      int
		since  time.Time
		reason string
	}

	lastShape  string
	run        int
	suppressed int
	lastHeld   []byte
	lastEmit   time.Time
}

// NewRotatingFile returns a file sink; nothing is touched on disk until the first write.
func NewRotatingFile(o FileOptions) *RotatingFile {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.CollapseFlush <= 0 {
		o.CollapseFlush = time.Minute
	}
	if o.NoticeOut == nil {
		o.NoticeOut = os.Stderr
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &RotatingFile{o: o, max: o.MaxBytes, fs: fileOps{fileSize, os.Remove, os.Rename, openAppend}}
}

// Write appends one line (rotating first when due). It always returns len(p), nil.
func (w *RotatingFile) Write(p []byte) (n int, err error) {
	n = len(p)
	defer func() { _ = recover() }()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range w.collapse(p) {
		w.emit(line)
	}
	return n, nil
}

// Close writes any held-back repeat summary. It never fails.
func (w *RotatingFile) Close() error {
	defer func() { _ = recover() }()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.suppressed > 0 {
		w.emit(w.summary())
	}
	return nil
}

var shapeRE = regexp.MustCompile(`0x?[0-9a-fA-F]{6,}|\d+`)

func (w *RotatingFile) collapse(p []byte) [][]byte {
	if w.o.CollapseAfter <= 0 {
		return [][]byte{p}
	}
	shape := shapeRE.ReplaceAllString(string(p), "#")
	now := w.o.Now()
	if w.run > 0 && shape == w.lastShape {
		w.run++
		if w.run <= w.o.CollapseAfter {
			w.lastEmit = now
			return [][]byte{p}
		}
		w.suppressed++
		w.lastHeld = append(w.lastHeld[:0], p...)
		if now.Sub(w.lastEmit) >= w.o.CollapseFlush {
			w.lastEmit = now
			return [][]byte{w.summary()}
		}
		return nil
	}
	var out [][]byte
	if w.suppressed > 0 {
		out = append(out, w.summary())
	}
	w.lastShape, w.run, w.lastEmit = shape, 1, now
	return append(out, p)
}

// summary renders the held-back run: its latest line plus the count of lines folded before it.
func (w *RotatingFile) summary() []byte {
	line := strings.TrimRight(string(w.lastHeld), "\r\n")
	if more := w.suppressed - 1; more > 0 {
		line += fmt.Sprintf(" [+%d more similar]", more)
	}
	w.suppressed = 0
	return []byte(line + "\n")
}

func (w *RotatingFile) emit(line []byte) {
	now := w.o.Now()
	w.rotateIfDue(now)

	var buf []byte
	notices := w.pending
	if w.drop.n > 0 {
		notices = append(notices[:len(notices):len(notices)], fmt.Sprintf(
			"log write failed: %d line(s) dropped from %s since %s - %s",
			w.drop.n, w.o.Path, w.drop.since.Format(timeLayout), w.drop.reason))
	}
	for _, n := range notices {
		buf = append(buf, w.noticeLine(now, n)...)
	}
	buf = append(buf, line...)

	if err := w.appendBytes(buf); err != nil {
		for _, n := range w.pending { // the file cannot carry them: deliver them to stderr now
			w.stderrNotice(now, n)
		}
		w.pending = nil
		if w.drop.n == 0 {
			w.drop.since, w.drop.reason = now, err.Error()
			w.stderrNotice(now, fmt.Sprintf("log write failed: cannot append to %s - %s. Lines are dropped until it recovers.", w.o.Path, err))
		}
		w.drop.n++
		return
	}
	w.pending = nil
	w.drop.n = 0
}

func (w *RotatingFile) rotateIfDue(now time.Time) {
	size, err := w.fs.size(w.o.Path)
	if err != nil || size < w.max {
		return // missing file or under the limit
	}
	prev := w.o.Path + ".1"
	err = w.fs.remove(prev)
	if err != nil && !os.IsNotExist(err) {
		w.blocked(now, size, err)
		return
	}
	if err = w.fs.rename(w.o.Path, prev); err != nil {
		w.blocked(now, size, err)
		return
	}
	w.rotateBlocked = false
}

func (w *RotatingFile) blocked(now time.Time, size int64, err error) {
	if w.rotateBlocked {
		return
	}
	w.rotateBlocked = true
	w.pending = append(w.pending, fmt.Sprintf(
		"log rotation blocked: %s is %d bytes, over its %d-byte limit, and could not be renamed to %s.1 - %s. Writing continues in place and rotation is retried on every write.",
		w.o.Path, size, w.max, w.o.Path, err))
}

func (w *RotatingFile) appendBytes(b []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	f, err := w.fs.open(w.o.Path)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (w *RotatingFile) noticeLine(now time.Time, msg string) string {
	ts := now.Format(timeLayout)
	if w.o.JSON {
		m, _ := json.Marshal(msg)
		return fmt.Sprintf(`{"time":"%s","level":"WARN","component":"logx","msg":%s}`+"\n", ts, m)
	}
	return fmt.Sprintf("%s WARN  [logx] %s\n", ts, msg)
}

func (w *RotatingFile) stderrNotice(now time.Time, msg string) {
	defer func() { _ = recover() }()
	_, _ = io.WriteString(w.o.NoticeOut, w.noticeLine(now, msg))
}
