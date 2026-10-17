package logx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newFile(t *testing.T, o FileOptions) (*RotatingFile, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	o.Path = filepath.Join(dir, "sub", "holocron.log")
	stderr := &bytes.Buffer{}
	o.NoticeOut = stderr
	return NewRotatingFile(o), o.Path, stderr
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRotationBoundary(t *testing.T) {
	const max = 100
	tests := []struct {
		name       string
		preSize    int
		wantRotate bool
	}{
		{"one under", max - 1, false},
		{"exactly at", max, true},
		{"one over", max + 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, path, _ := newFile(t, FileOptions{MaxBytes: max})
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), tc.preSize), 0o600); err != nil {
				t.Fatal(err)
			}
			if n, err := w.Write([]byte("new\n")); n != 4 || err != nil {
				t.Fatalf("Write = %d, %v", n, err)
			}
			_, err := os.Stat(path + ".1")
			if rotated := err == nil; rotated != tc.wantRotate {
				t.Fatalf("rotated = %v, want %v", rotated, tc.wantRotate)
			}
			if tc.wantRotate {
				if got := readFile(t, path); got != "new\n" {
					t.Errorf("fresh file = %q", got)
				}
				if got := len(readFile(t, path+".1")); got != tc.preSize {
					t.Errorf(".1 holds %d bytes, want %d", got, tc.preSize)
				}
			} else if got := readFile(t, path); !strings.HasSuffix(got, "new\n") || len(got) != tc.preSize+4 {
				t.Errorf("appended file wrong: %d bytes", len(got))
			}
		})
	}
}

func TestOnlyOnePreviousGeneration(t *testing.T) {
	w, path, _ := newFile(t, FileOptions{MaxBytes: 50})
	for i := 0; i < 40; i++ {
		w.Write([]byte(fmt.Sprintf("line-%02d-padding-padding\n", i)))
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != "holocron.log" || names[1] != "holocron.log.1" {
		t.Fatalf("files = %v, want exactly the log and one .1", names)
	}
	if !strings.Contains(readFile(t, path), "line-39") {
		t.Error("newest line missing from the live file")
	}
}

func TestRotationFailureNeverFailsAndNoticesOncePerEpisode(t *testing.T) {
	w, path, stderr := newFile(t, FileOptions{MaxBytes: 20})
	w.Write([]byte("0123456789012345678901234\n")) // creates the file over the limit

	blocked := true
	realRename := w.fs.rename
	w.fs.rename = func(a, b string) error {
		if blocked {
			return errors.New("sharing violation")
		}
		return realRename(a, b)
	}
	for i := 0; i < 5; i++ {
		if n, err := w.Write([]byte(fmt.Sprintf("during-%d\n", i))); err != nil || n != 9 {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	got := readFile(t, path)
	if c := strings.Count(got, "log rotation blocked"); c != 1 {
		t.Fatalf("%d rotation notices, want exactly 1:\n%s", c, got)
	}
	if !strings.Contains(got, "during-4") || stderr.Len() != 0 {
		t.Errorf("lines must keep flowing in place and the notice must ride in the file (stderr=%q)", stderr)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("no .1 expected while rotation is blocked")
	}

	blocked = false // success rearms the notice
	w.Write([]byte("rotated\n"))
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation should succeed once unblocked: %v", err)
	}
	if readFile(t, path) != "rotated\n" {
		t.Error("live file should hold only the new line after rotation")
	}

	blocked = true
	w.Write(bytes.Repeat([]byte("y"), 30))
	w.Write([]byte("again\n"))
	w.Write([]byte("again\n"))
	if c := strings.Count(readFile(t, path), "log rotation blocked"); c != 1 {
		t.Errorf("second episode: %d notices, want 1", c)
	}
}

func TestRotationFailureRealDirectoryAsPrevious(t *testing.T) {
	// A non-empty directory named <log>.1 makes remove and rename fail without any hook.
	w, path, _ := newFile(t, FileOptions{MaxBytes: 10})
	if err := os.MkdirAll(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".1", "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := w.Write([]byte("a line longer than the limit\n")); err != nil {
			t.Fatal(err)
		}
	}
	got := readFile(t, path)
	if strings.Count(got, "log rotation blocked") != 1 || strings.Count(got, "a line longer") != 4 {
		t.Errorf("unexpected content:\n%s", got)
	}
}

func TestAppendFailureDropsLinesAndReportsOnce(t *testing.T) {
	w, path, stderr := newFile(t, FileOptions{})
	w.Write([]byte("before\n"))

	failing := true
	realOpen := w.fs.open
	w.fs.open = func(p string) (io.WriteCloser, error) {
		if failing {
			return nil, errors.New("disk full")
		}
		return realOpen(p)
	}
	for i := 0; i < 5; i++ {
		if n, err := w.Write([]byte("lost\n")); err != nil || n != 5 {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if c := strings.Count(stderr.String(), "log write failed"); c != 1 {
		t.Fatalf("stderr notices = %d, want 1: %q", c, stderr)
	}
	failing = false
	w.Write([]byte("after\n"))
	w.Write([]byte("after2\n"))
	lines := strings.Split(strings.TrimSpace(readFile(t, path)), "\n")
	if len(lines) != 4 || lines[0] != "before" || !strings.Contains(lines[1], "5 line(s) dropped") || lines[2] != "after" || lines[3] != "after2" {
		t.Fatalf("file = %q", lines)
	}
}

func TestAppendFailureWhenPathIsDirectory(t *testing.T) {
	w, path, stderr := newFile(t, FileOptions{})
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte("x\n")); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(stderr.String(), "log write failed") != 1 {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestWriterPanicIsContained(t *testing.T) {
	w, _, _ := newFile(t, FileOptions{})
	w.fs.open = func(string) (io.WriteCloser, error) { panic("boom") }
	if n, err := w.Write([]byte("x\n")); n != 2 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	w.fs.size = func(string) (int64, error) { panic("boom") }
	if n, err := w.Write([]byte("x\n")); n != 2 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
}

func TestRepeatCollapse(t *testing.T) {
	clk := &fakeClock{t: fixedTime}
	w, path, _ := newFile(t, FileOptions{CollapseAfter: 2, CollapseFlush: time.Minute, Now: clk.Now})
	frame := func(i int) string { return fmt.Sprintf("frame=%d dropped 0x%08x", i, i*977777) }
	for i := 1; i <= 5; i++ { // same shape: digits and hex runs stripped
		w.Write([]byte(frame(i) + "\n"))
	}
	w.Write([]byte("something else\n"))
	got := strings.Split(strings.TrimSpace(readFile(t, path)), "\n")
	want := []string{frame(1), frame(2), frame(5) + " [+2 more similar]", "something else"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("lines = %q\nwant   %q", got, want)
	}
}

func TestRepeatCollapseFlushAndClose(t *testing.T) {
	clk := &fakeClock{t: fixedTime}
	w, path, _ := newFile(t, FileOptions{CollapseAfter: 1, CollapseFlush: time.Minute, Now: clk.Now})
	for i := 0; i < 4; i++ { // 1 written, 3 held
		w.Write([]byte(fmt.Sprintf("retry %d\n", i)))
	}
	clk.Advance(time.Minute)
	w.Write([]byte("retry 4\n")) // flush interval elapsed: summary of the run so far
	w.Write([]byte("retry 5\n"))
	w.Close() // reports the held tail
	got := strings.Split(strings.TrimSpace(readFile(t, path)), "\n")
	want := []string{"retry 0", "retry 4 [+3 more similar]", "retry 5"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestRepeatCollapseOffByDefault(t *testing.T) {
	w, path, _ := newFile(t, FileOptions{})
	for i := 0; i < 5; i++ {
		w.Write([]byte("same\n"))
	}
	if strings.Count(readFile(t, path), "same") != 5 {
		t.Error("lines were collapsed although collapse is off")
	}
}

func TestRotatingFileConcurrent(t *testing.T) {
	w, path, _ := newFile(t, FileOptions{MaxBytes: 1 << 30})
	var wg sync.WaitGroup
	for g := 0; g < 200; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				w.Write([]byte(fmt.Sprintf("goroutine-%03d line-%d xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", g, i)))
			}
		}(g)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(readFile(t, path)), "\n")
	if len(lines) != 2000 {
		t.Fatalf("%d lines, want 2000", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "goroutine-") || !strings.HasSuffix(l, "xxxx") {
			t.Fatalf("interleaved line %q", l)
		}
	}
}

func TestFileModeOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows files inherit the folder ACL; mode bits are not enforced")
	}
	w, path, _ := newFile(t, FileOptions{})
	w.Write([]byte("x\n"))
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", di.Mode().Perm())
	}
}
