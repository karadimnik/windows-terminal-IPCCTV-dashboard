package status

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustRead(t *testing.T, path string) *Document {
	t.Helper()
	doc, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func marshalString(t *testing.T, d *Document) string {
	t.Helper()
	b, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriterRoundTripsEveryPositiveFixture(t *testing.T) {
	for _, f := range positiveFixtures(t) {
		in := mustRead(t, f)
		out := filepath.Join(t.TempDir(), "_state", "status.json") // directory is created
		if err := NewWriter(out, Options{}).Write(in); err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
		raw := readFile(t, out)
		if bytes.HasPrefix(raw, utf8BOM) || bytes.Contains(raw, []byte("\r")) || !bytes.HasSuffix(raw, []byte("}\n")) {
			t.Errorf("%s: not UTF-8 no BOM / LF", filepath.Base(f))
		}
		if vs, err := Validate(raw); err != nil || len(vs) != 0 {
			t.Errorf("%s: written file violates the schema: %v %v", filepath.Base(f), violationKeys(vs), err)
		}
		if a, b := marshalString(t, in), marshalString(t, mustRead(t, out)); a != b {
			t.Errorf("%s: round trip differs", filepath.Base(f))
		}
		if _, err := os.Stat(out + ".tmp"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: temp file left behind", filepath.Base(f))
		}
	}
}

func TestWriterRedactsTheWholeText(t *testing.T) {
	doc := mustRead(t, filepath.Join(goldenStatus, "running-full.json"))
	const secret = `S3cr"et\pw`
	doc.Prune.LastMessage = "relay at rtsp://admin:hunter22@192.0.2.9/x failed; token " + secret
	doc.Cameras[0].LastError = "login " + secret + " rejected"

	// control: the unredacted serialisation shows the planted values
	control := marshalString(t, doc)
	if !strings.Contains(control, "admin:hunter22@") || !strings.Contains(control, `S3cr\"et\\pw`) {
		t.Fatal("positive control does not contain the planted secrets")
	}

	out := filepath.Join(t.TempDir(), "status.json")
	w := NewWriter(out, Options{})
	w.Redactor().Register(secret)
	if err := w.Write(doc); err != nil {
		t.Fatal(err)
	}
	text := string(readFile(t, out))
	for _, leak := range []string{"admin:", "hunter22", "S3cr", "et\\\\pw"} {
		if strings.Contains(text, leak) {
			t.Errorf("file leaks %q", leak)
		}
	}
	if !strings.Contains(text, "rtsp://***:***@192.0.2.9/x") || !strings.Contains(text, "token ***") {
		t.Errorf("masks missing in file")
	}
	if _, err := Read(out); err != nil {
		t.Fatalf("redacted file must still parse: %v", err)
	}
}

// A reader that opened the file the share-delete way never blocks the replace
// (POSIX rename semantics on Windows, plain rename elsewhere).
func TestWriteSucceedsWhileShareDeleteReaderHoldsTheFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "status.json")
	w := NewWriter(out, Options{})
	doc := mustRead(t, filepath.Join(goldenStatus, "running-full.json"))
	if err := w.Write(doc); err != nil {
		t.Fatal(err)
	}
	held, err := openShared(out)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	f := 2.0
	doc.UptimeSeconds = &f
	start := time.Now()
	if err := w.Write(doc); err != nil {
		t.Fatalf("write while a share-delete reader holds the file: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("write took %v", time.Since(start))
	}
	if got := mustRead(t, out); got.UptimeSeconds == nil || *got.UptimeSeconds != 2 {
		t.Errorf("new content not visible")
	}
}

func TestWriteFailureSkipsTheCycleAndKeepsTheOldFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "status.json")
	w := NewWriter(out, Options{RetryDelay: time.Millisecond})
	doc := mustRead(t, filepath.Join(goldenStatus, "running-full.json"))
	if err := w.Write(doc); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, out)
	boom := errors.New("boom")
	w.replace = func(string, string) (bool, error) { return false, boom }
	if err := w.Write(doc); !errors.Is(err, boom) || w.ConsecutiveFailures() != 1 {
		t.Fatalf("err %v failures %d", err, w.ConsecutiveFailures())
	}
	if !bytes.Equal(before, readFile(t, out)) {
		t.Error("old file changed by a failed replace")
	}
	w.replace = replaceFile
	if err := w.Write(doc); err != nil || w.ConsecutiveFailures() != 0 {
		t.Fatalf("recovery: %v", err)
	}
}

func TestConcurrentWriterAndReaders(t *testing.T) {
	dir := t.TempDir()
	runConcurrent(t, dir, 3*time.Second)
}

// runConcurrent: one writer every 20 ms, four share-delete readers, the stated
// duration. Zero failed writes, partial/invalid reads, missing-file reads.
func runConcurrent(t *testing.T, dir string, d time.Duration) {
	t.Helper()
	out := filepath.Join(dir, "status.json")
	base := mustRead(t, filepath.Join(goldenStatus, "running-full.json"))
	pad := strings.Repeat("x", 8192)
	base.Events = append(base.Events, Event{Text: pad})
	w := NewWriter(out, Options{})
	seq := 0.0
	write := func() error {
		seq++
		v := seq
		base.UptimeSeconds = &v
		return w.Write(base)
	}
	if err := write(); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	var reads, noStatus, invalid, otherErr, regress atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			last := 0.0
			for !stop.Load() {
				doc, err := Read(out)
				switch {
				case errors.Is(err, ErrNoStatus):
					noStatus.Add(1)
				case errors.Is(err, ErrInvalid):
					invalid.Add(1)
				case err != nil:
					otherErr.Add(1)
					t.Logf("read error: %v", err)
				default:
					reads.Add(1)
					if doc.UptimeSeconds == nil || *doc.UptimeSeconds < last {
						regress.Add(1)
					} else {
						last = *doc.UptimeSeconds
					}
				}
				time.Sleep(200 * time.Microsecond)
			}
		}()
	}
	writes, failed := 1, 0
	tick := time.NewTicker(20 * time.Millisecond)
	deadline := time.After(d)
loop:
	for {
		select {
		case <-tick.C:
			writes++
			if err := write(); err != nil {
				failed++
				t.Logf("write failed: %v", err)
			}
		case <-deadline:
			break loop
		}
	}
	tick.Stop()
	stop.Store(true)
	wg.Wait()
	t.Logf("writes %d failed %d; reads %d noStatus %d invalid %d otherErr %d regress %d",
		writes, failed, reads.Load(), noStatus.Load(), invalid.Load(), otherErr.Load(), regress.Load())
	if failed != 0 || noStatus.Load() != 0 || invalid.Load() != 0 || otherErr.Load() != 0 || regress.Load() != 0 {
		t.Fail()
	}
	if reads.Load() < 100 {
		t.Errorf("too few reads: %d", reads.Load())
	}
}
