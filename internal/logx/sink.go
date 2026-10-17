package logx

import (
	"io"
	"os"
	"strings"
	"sync"
)

// Sink receives complete, already formatted and redacted lines (each ends in a newline). The
// handler ignores the returned error; the built-in sinks never return one.
type Sink interface {
	Write(p []byte) (int, error)
	Close() error
}

// MemorySink keeps lines in memory (tests).
type MemorySink struct {
	mu    sync.Mutex
	lines []string
}

func NewMemorySink() *MemorySink { return &MemorySink{} }

func (m *MemorySink) Write(p []byte) (int, error) {
	m.mu.Lock()
	m.lines = append(m.lines, strings.TrimRight(string(p), "\r\n"))
	m.mu.Unlock()
	return len(p), nil
}

func (m *MemorySink) Close() error { return nil }

// Lines returns a copy of the recorded lines, without their newlines.
func (m *MemorySink) Lines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.lines...)
}

// String joins the recorded lines with newlines.
func (m *MemorySink) String() string { return strings.Join(m.Lines(), "\n") }

// WriterSink adapts an io.Writer; write errors are swallowed.
type WriterSink struct {
	mu sync.Mutex
	w  io.Writer
}

func NewWriterSink(w io.Writer) *WriterSink { return &WriterSink{w: w} }

// StderrSink writes to os.Stderr.
func StderrSink() *WriterSink { return NewWriterSink(os.Stderr) }

func (s *WriterSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	func() {
		defer func() { _ = recover() }()
		_, _ = s.w.Write(p)
	}()
	return len(p), nil
}

func (s *WriterSink) Close() error { return nil }

type fanOut []Sink

// FanOut writes every line to all sinks.
func FanOut(sinks ...Sink) Sink { return fanOut(sinks) }

func (f fanOut) Write(p []byte) (int, error) {
	for _, s := range f {
		func() {
			defer func() { _ = recover() }()
			_, _ = s.Write(p)
		}()
	}
	return len(p), nil
}

func (f fanOut) Close() error {
	var first error
	for _, s := range f {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
