//go:build windows

package status

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Negative control (SP6): a reader that opened the file WITHOUT FILE_SHARE_DELETE
// (Go's os.Open, PowerShell Get-Content) makes the replace fail with a sharing
// violation. Write must give up within its bounded retries, return the error,
// leave the old file intact and valid, and succeed again once the reader lets go.
func TestNegativeControlReaderWithoutShareDeleteSkipsTheCycle(t *testing.T) {
	out := filepath.Join(t.TempDir(), "status.json")
	w := NewWriter(out, Options{})
	doc := fixture(t, "running-full.json")
	if err := w.Write(doc); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, out)

	held, err := os.Open(out) // no FILE_SHARE_DELETE
	if err != nil {
		t.Fatal(err)
	}
	f := 99.0
	doc.UptimeSeconds = &f
	start := time.Now()
	werr := w.Write(doc)
	elapsed := time.Since(start)
	if werr == nil {
		held.Close()
		t.Fatal("Write succeeded although a reader without share-delete holds the file")
	}
	if !isSharingError(werr) {
		t.Errorf("unexpected error kind: %v", werr)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Write blocked for %v", elapsed)
	}
	t.Logf("negative control: Write returned after %v: %v", elapsed.Round(time.Millisecond), werr)
	if w.ConsecutiveFailures() != 1 {
		t.Errorf("failures %d", w.ConsecutiveFailures())
	}
	if _, err := os.Stat(out + ".tmp"); err != nil {
		t.Errorf("temp file should stay for the next cycle: %v", err)
	}
	// the held handle still reads the old, complete content
	got := make([]byte, len(before))
	if n, _ := held.ReadAt(got, 0); n != len(before) || string(got) != string(before) {
		t.Error("old content disturbed")
	}
	held.Close()
	if !equalBytes(before, readFile(t, out)) {
		t.Error("target changed by a failed replace")
	}

	if err := w.Write(doc); err != nil {
		t.Fatalf("next cycle after the reader let go: %v", err)
	}
	if got := mustRead(t, out); got.UptimeSeconds == nil || *got.UptimeSeconds != 99 || w.ConsecutiveFailures() != 0 {
		t.Error("recovery did not write the new content")
	}
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

// The \\wsl.localhost (9P) share rejects the POSIX-semantics rename with 87 or
// 50; Write must fall back to os.Rename. Set HOLOCRON_STATUS_9P_DIR to a
// \\wsl.localhost\<distro>\... directory (it is created and removed here).
func TestWriteOn9PShareFallsBackToMoveFileEx(t *testing.T) {
	base := os.Getenv("HOLOCRON_STATUS_9P_DIR")
	if base == "" {
		t.Skip("HOLOCRON_STATUS_9P_DIR not set")
	}
	if _, err := os.Stat(filepath.Dir(base)); err != nil {
		t.Skipf("9P parent absent: %v", err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Skipf("cannot create %s: %v", base, err)
	}
	defer os.RemoveAll(base)
	out := filepath.Join(base, "status.json")
	w := NewWriter(out, Options{})
	doc := fixture(t, "running-full.json")
	for i := 0; i < 2; i++ { // the second write replaces an existing target
		if err := w.Write(doc); err != nil {
			t.Fatalf("write %d on 9P: %v", i, err)
		}
	}
	if w.fallbacks != 2 {
		t.Errorf("fallbacks = %d, want 2 (the POSIX rename is unsupported on 9P)", w.fallbacks)
	}
	if _, err := Read(out); err != nil {
		t.Errorf("read back: %v", err)
	}
	// and the raw primitive reports the documented error before the fallback
	tmp := out + ".probe"
	os.WriteFile(tmp, []byte("{}"), 0o644)
	err := renamePosix(tmp, out)
	if err == nil {
		t.Log("renamePosix unexpectedly worked on this share")
	} else {
		t.Logf("renamePosix on 9P: %v", err)
		if !errors.Is(err, errInvalidParameter) && !errors.Is(err, errNotSupported) {
			t.Errorf("unexpected error: %v", err)
		}
	}
}
