//go:build windows

package archive

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const liveWriterEnv = "HOLOCRON_TEST_LIVE_WRITER"

// TestLiveWriterHelper is the child writer of TestLiveSizeOfAFileAnotherProcessWrites: it creates
// the file, writes 1 MiB through the open handle, says "ready" and holds the handle until stdin closes.
func TestLiveWriterHelper(t *testing.T) {
	path := os.Getenv(liveWriterEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(2)
	}
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		fmt.Println("error", err)
		os.Exit(2)
	}
	fmt.Println("ready")
	bufio.NewReader(os.Stdin).ReadString('\n')
	f.Close()
	os.Exit(0)
}

// A child process writes the newest segment and keeps it open: the live size sees all of it; the
// directory listing is the positive control (stale on NTFS, logged when this volume refreshes it).
func TestLiveSizeOfAFileAnotherProcessWrites(t *testing.T) {
	dir := camDir(t, "Cam_A")
	put(t, dir, "Cam_A_20261017_114500+0300.mkv", 300, time.Hour)
	path := filepath.Join(dir, "Cam_A_20261017_120000+0300.mkv")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLiveWriterHelper$")
	cmd.Env = append(os.Environ(), liveWriterEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Wait() }()
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" && line != "ready\r\n" {
		t.Fatalf("the writer did not get ready: %q", line)
	}

	var listed int64 = -1
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(path) {
			if fi, err := e.Info(); err == nil {
				listed = fi.Size()
			}
		}
	}
	live, err := liveSize(path)
	if err != nil || live != 1<<20 {
		t.Fatalf("live size %d (%v), want %d", live, err, 1<<20)
	}
	if listed < live {
		t.Logf("positive control: the listing shows %d bytes for the open file, the live size %d", listed, live)
	} else {
		t.Logf("positive control did not reproduce: the listing shows %d bytes, as the live size", listed)
	}
	s, err := Scan(dir, ".mkv", time.Now(), 900)
	if err != nil || s.Newest != filepath.Base(path) || s.NewestBytes != 1<<20 || s.Bytes != 1<<20+300 {
		t.Errorf("scan: %v newest %s %d bytes, total %d", err, s.Newest, s.NewestBytes, s.Bytes)
	}
}
