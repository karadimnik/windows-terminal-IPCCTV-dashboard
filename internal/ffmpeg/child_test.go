package ffmpeg

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/clock"
	"holocron/internal/logx"
)

// TestMain doubles as the helper process: with FFMPEG_TEST_HELPER set the test binary acts as a
// fake ffmpeg, so the real os/exec path is exercised without ffmpeg installed.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FFMPEG_TEST_HELPER"); mode != "" {
		helper(mode)
		return
	}
	os.Exit(m.Run())
}

func helper(mode string) {
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "Impossible to open 'rtsp://fakeuser:fakepass@192.0.2.11/stream1'")
		fmt.Fprintln(os.Stdout, "to stdout hunter2-secret")
		os.Exit(7)
	case "stdin":
		n, _ := io.Copy(io.Discard, os.Stdin) // a closed stdin ends at once
		fmt.Fprintf(os.Stderr, "stdin bytes %d\n", n)
	case "tz":
		fmt.Fprintf(os.Stderr, "TZ=%q set=%v\n", os.Getenv("TZ"), func() bool { _, ok := os.LookupEnv("TZ"); return ok }())
	case "block":
		fmt.Fprintln(os.Stderr, "running")
		time.Sleep(time.Minute)
	case "negexit":
		os.Exit(-5)
	case "flood":
		for i := 0; i < 2000; i++ {
			fmt.Fprintf(os.Stderr, "line %d\n", i)
		}
	}
	os.Exit(0)
}

func helperSpec(mode string) Spec {
	exe, _ := os.Executable()
	env := append(os.Environ(), "FFMPEG_TEST_HELPER="+mode)
	return Spec{Name: "cam1", Exe: exe, Args: []string{"-test.run=NONE"}, Env: env}
}

func TestRealProcessExitCodeAndRedaction(t *testing.T) {
	red := logx.NewRedactor()
	red.Register("hunter2-secret")
	sink := logx.NewMemorySink()
	lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "trace"}, Sink: sink, Redactor: red})
	spec := helperSpec("fail")
	spec.Redactor, spec.Log = red, lg.For("ffmpeg:cam1")
	c, err := Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pid() <= 0 {
		t.Fatalf("pid %d", c.Pid())
	}
	info := c.Wait()
	if info.Code != 7 || info.Err != nil {
		t.Fatalf("%+v", info)
	}
	got := strings.Join(info.LastLines, "\n")
	for _, bad := range []string{"fakepass", "fakeuser", "hunter2-secret"} {
		if strings.Contains(got, bad) || strings.Contains(sink.String(), bad) {
			t.Errorf("%q leaked:\ntail: %s\nlog: %s", bad, got, sink.String())
		}
	}
	if !strings.Contains(got, "rtsp://***:***@192.0.2.11/stream1") || !strings.Contains(got, "to stdout ***") {
		t.Errorf("tail not as expected: %q", info.LastLines)
	}
	if !strings.Contains(sink.String(), "rtsp://***:***@192.0.2.11/stream1") {
		t.Errorf("log lacks the redacted line: %s", sink.String())
	}
	if want := fmt.Sprintf("ffmpeg exited with code 7 after %ds", int64(math.RoundToEven(info.RanFor.Seconds()))); info.Message() != want {
		t.Errorf("message %q want %q", info.Message(), want)
	}
}

func TestRealProcessStdinIsClosed(t *testing.T) {
	c, err := Start(context.Background(), helperSpec("stdin"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(20 * time.Second):
		c.Kill()
		t.Fatal("the child waits on stdin")
	}
	if info := c.Wait(); info.Code != 0 || len(info.LastLines) != 1 || info.LastLines[0] != "stdin bytes 0" {
		t.Fatalf("%+v", info)
	}
}

func TestDefaultEnvAppliesTZRule(t *testing.T) {
	t.Setenv("TZ", "Europe/Bucharest")
	spec := helperSpec("tz")
	spec.Env = nil // default: ChildEnv(os.Environ(), GOOS)
	t.Setenv("FFMPEG_TEST_HELPER", "tz")
	defer os.Unsetenv("FFMPEG_TEST_HELPER")
	c, err := Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	info := c.Wait()
	want := `TZ="Europe/Bucharest" set=true`
	if runtime.GOOS == "windows" {
		want = `TZ="" set=false`
	}
	if len(info.LastLines) != 1 || info.LastLines[0] != want {
		t.Fatalf("got %q want %q", info.LastLines, want)
	}
}

func TestKillIsHardAndWaitStillReports(t *testing.T) {
	c, err := Start(context.Background(), helperSpec("block"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	began := time.Now()
	if err := c.Kill(); err != nil {
		t.Fatal(err)
	}
	info := c.Wait()
	if time.Since(began) > 10*time.Second {
		t.Fatal("kill took too long")
	}
	if info.Code == 0 {
		t.Fatalf("a killed child reports code 0: %+v", info)
	}
	if err := c.Kill(); err != nil {
		t.Fatalf("kill after exit: %v", err)
	}
}

func TestContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, err := Start(ctx, helperSpec("block"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		c.Kill()
		t.Fatal("context cancel did not end the child")
	}
	if _, err := Start(ctx, helperSpec("block")); err == nil {
		t.Fatal("a cancelled context started a child")
	}
}

func TestStartFailureRunsCleanupOnce(t *testing.T) {
	n := 0
	_, err := Start(context.Background(), Spec{Exe: "/definitely/not/here/ffmpeg", Cleanup: func() { n++ }})
	if err == nil || n != 1 {
		t.Fatalf("err %v cleanups %d", err, n)
	}
	if _, err := Start(context.Background(), Spec{Cleanup: func() { n++ }}); err == nil || n != 2 {
		t.Fatalf("no exe: err %v cleanups %d", err, n)
	}
}

func TestCleanupRunsAfterExitBeforeWaitReturns(t *testing.T) {
	var ran bool
	spec := helperSpec("fail")
	spec.Cleanup = func() { ran = true }
	c, err := Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	c.Wait()
	if !ran {
		t.Fatal("cleanup had not run when Wait returned")
	}
}

func TestTailKeepsOnlyTheLastLines(t *testing.T) {
	spec := helperSpec("flood")
	spec.TailLines = 5
	c, err := Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	info := c.Wait()
	want := []string{"line 1995", "line 1996", "line 1997", "line 1998", "line 1999"}
	if fmt.Sprint(info.LastLines) != fmt.Sprint(want) {
		t.Fatalf("got %v", info.LastLines)
	}
}

// ---- a scripted fake Starter: no process at all ----

type fakeProc struct {
	pid     int
	out     StartConfig
	exit    chan int
	killed  bool
	mu      sync.Mutex
	waitErr error
}

func (p *fakeProc) Pid() int { return p.pid }
func (p *fakeProc) Wait() (int, error) {
	code := <-p.exit
	return code, p.waitErr
}
func (p *fakeProc) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.killed {
		p.killed = true
		p.exit <- -1
	}
	return nil
}

type fakeStarter struct{ p *fakeProc }

func (s *fakeStarter) Start(c StartConfig) (Process, error) {
	s.p = &fakeProc{pid: 4242, out: c, exit: make(chan int, 1)}
	return s.p, nil
}

func TestFakeStarterLinesSplitRedactAndTime(t *testing.T) {
	fs := &fakeStarter{}
	clk := clock.NewFake(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	red := logx.NewRedactor()
	red.Register("s3cr3t-pass")
	spec := Spec{Name: "cam1", Exe: "ffmpeg", Args: []string{"-i", "x"}, Starter: fs, Clock: clk, Redactor: red, TailLines: 4}
	c, err := Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pid() != 4242 || !c.StartedAt().Equal(clk.Now()) {
		t.Fatalf("pid %d at %v", c.Pid(), c.StartedAt())
	}
	if fs.p.out.Env == nil {
		t.Error("the starter got no environment")
	}
	w := fs.p.out.Stderr
	// a multi-byte character (the euro sign, 3 bytes) split across writes, CR-separated progress,
	// a credential URL split mid-token, blank lines, and a line with no final newline
	io := [][]byte{[]byte("price \xe2\x82"), []byte("\xac ok\r\r\n"), []byte("open rtsp://us"), []byte("er:pw@h/t failed\n\n"),
		[]byte("token s3cr3t-pass here\n"), []byte("last without newline")}
	for _, b := range io {
		w.Write(b)
	}
	clk.Advance(12*time.Second + 900*time.Millisecond)
	fs.p.exit <- 3
	info := c.Wait()
	want := []string{"price € ok", "open rtsp://***:***@h/t failed", "token *** here", "last without newline"}
	if fmt.Sprint(info.LastLines) != fmt.Sprint(want) {
		t.Fatalf("lines %q\nwant %q", info.LastLines, want)
	}
	if info.Code != 3 || info.RanFor != 12*time.Second+900*time.Millisecond {
		t.Fatalf("%+v", info)
	}
	if info.Message() != "ffmpeg exited with code 3 after 13s" { // 12.9 s rounds like [int]
		t.Fatal(info.Message())
	}
}

func TestOverlongLineIsCut(t *testing.T) {
	fs := &fakeStarter{}
	c, _ := Start(context.Background(), Spec{Exe: "ffmpeg", Starter: fs})
	fs.p.out.Stderr.Write([]byte(strings.Repeat("a", maxLineBytes*2+10) + "\n"))
	fs.p.exit <- 0
	info := c.Wait()
	if len(info.LastLines) != 3 || len(info.LastLines[0]) != maxLineBytes || len(info.LastLines[2]) != 10 {
		t.Fatalf("%d lines", len(info.LastLines))
	}
}

func TestKillThroughFakeStarter(t *testing.T) {
	fs := &fakeStarter{}
	c, _ := Start(context.Background(), Spec{Exe: "ffmpeg", Starter: fs})
	c.Kill()
	if info := c.Wait(); info.Code != -1 || !fs.p.killed {
		t.Fatalf("%+v killed=%v", info, fs.p.killed)
	}
}

// The PowerShell product reports .NET's signed Process.ExitCode: a Windows status such as 0xC0000005
// reads negative. os.Exit(-5) is 0xFFFFFFFB on Windows and 251 (the low 8 bits) elsewhere.
func TestExitCodeSignedLikeDotNet(t *testing.T) {
	c, err := Start(context.Background(), helperSpec("negexit"))
	if err != nil {
		t.Fatal(err)
	}
	want := 251
	if runtime.GOOS == "windows" {
		want = -5
	}
	if info := c.Wait(); info.Code != want {
		t.Fatalf("code %d want %d", info.Code, want)
	}
}

func TestExitMessageRoundsHalfToEven(t *testing.T) {
	for _, c := range []struct {
		ran  time.Duration
		want string
	}{
		{2500 * time.Millisecond, "after 2s"}, {3500 * time.Millisecond, "after 4s"}, {12400 * time.Millisecond, "after 12s"},
		{12900 * time.Millisecond, "after 13s"}, {0, "after 0s"}, {499 * time.Millisecond, "after 0s"},
	} {
		got := ExitInfo{Code: 1, RanFor: c.ran}.Message()
		if want := "ffmpeg exited with code 1 " + c.want; got != want {
			t.Errorf("%v: %q want %q", c.ran, got, want)
		}
	}
}
