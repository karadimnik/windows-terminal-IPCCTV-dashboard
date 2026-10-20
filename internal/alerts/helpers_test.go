package alerts

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/logx"
)

// testLog is a logx logger at DEBUG writing to memory; Redactor is exposed to register secrets.
type testLog struct {
	*logx.Logger
	mem   *logx.MemorySink
	audit *logx.MemorySink
}

func newTestLog() testLog {
	const level = "DEBUG"
	mem, aud := logx.NewMemorySink(), logx.NewMemorySink()
	l, _ := logx.New(logx.Options{Config: logx.Config{Level: level}, Getenv: func(string) string { return "" }, Sink: mem, Audit: aud})
	return testLog{l, mem, aud}
}

func (l testLog) lines(substr ...string) []string {
	var out []string
next:
	for _, ln := range l.mem.Lines() {
		for _, s := range substr {
			if !strings.Contains(ln, s) {
				continue next
			}
		}
		out = append(out, ln)
	}
	return out
}

// fakeChannel records what it is sent; send decides the result.
type fakeChannel struct {
	name    string
	network bool

	mu      sync.Mutex
	got     []Event
	calls   int
	send    func(ctx context.Context, ev Event) error
	enabled func() bool
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) Enabled() bool {
	if f.enabled != nil {
		return f.enabled()
	}
	return true
}
func (f *fakeChannel) Network() bool { return f.network }
func (f *fakeChannel) Send(ctx context.Context, ev Event) error {
	f.mu.Lock()
	f.calls++
	send := f.send
	f.mu.Unlock()
	var err error
	if send != nil {
		err = send(ctx, ev)
	}
	if err == nil {
		f.mu.Lock()
		f.got = append(f.got, ev)
		f.mu.Unlock()
	}
	return err
}
func (f *fakeChannel) delivered() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Event(nil), f.got...)
}
func (f *fakeChannel) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func ids(evs []Event) []int {
	out := make([]int, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func closeD(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// loopbackPort listens on the first free port of the test range 18680-18699.
func loopbackListener(t *testing.T) net.Listener {
	t.Helper()
	for p := 18680; p <= 18699; p++ {
		if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			return l
		}
	}
	t.Fatal("no free port in 18680-18699")
	return nil
}

func fakeServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.Listener.Close()
	ts.Listener = loopbackListener(t)
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// refusedURL is a loopback URL where nothing listens.
func refusedAddr(t *testing.T) string {
	t.Helper()
	l := loopbackListener(t)
	addr := l.Addr().String()
	l.Close()
	return addr
}

type recordedReq struct {
	method string
	header http.Header
	body   string
	path   string
}

type recorder struct {
	mu   sync.Mutex
	reqs []recordedReq
}

func (r *recorder) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := req.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, recordedReq{req.Method, req.Header.Clone(), b.String(), req.URL.Path})
		r.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (r *recorder) all() []recordedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedReq(nil), r.reqs...)
}

func testEvent() Event {
	return Event{ID: IDCameraDown, Severity: Warning, Title: "TEST a camera has stopped recording",
		Message: "TEST cam1 has written nothing for 16 min", Camera: "cam1", At: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
}
