package ctlapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/cli"
	"holocron/internal/logx"
	"holocron/internal/status"
)

type fakeBackend struct {
	mu        sync.Mutex
	running   bool
	ids       []string // action ids seen by the backend
	stops     []string
	block     chan struct{} // Reload waits on it when set
	entered   chan struct{}
	onStop    func()
	records   map[string]bool
	live      bool
	reloads   int
	snapshot  *status.Document
	stopCalls int
}

func (f *fakeBackend) seen(ctx context.Context) {
	f.mu.Lock()
	f.ids = append(f.ids, cli.ActionIDFrom(ctx))
	f.mu.Unlock()
}

func (f *fakeBackend) Snapshot() *status.Document {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return nil
	}
	return f.snapshot
}

func (f *fakeBackend) Stop(kind string) StopReport {
	f.mu.Lock()
	f.stops = append(f.stops, kind)
	on := f.onStop
	f.mu.Unlock()
	if on != nil {
		on()
	}
	return StopReport{Kind: kind, Reason: "stop requested", Final: true}
}

func (f *fakeBackend) Reload(ctx context.Context) error {
	f.seen(ctx)
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.reloads++
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) SetRecord(ctx context.Context, cam string, on bool) (Changed, error) {
	f.seen(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.records[cam]
	if !ok {
		return Changed{}, fmt.Errorf("camera %q: %w", cam, ErrNotFound)
	}
	f.records[cam] = on
	return Changed{Target: cam, Enabled: on, Changed: cur != on}, nil
}

func (f *fakeBackend) SetLive(ctx context.Context, on bool) (Changed, error) {
	f.seen(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := Changed{Target: "live", Enabled: on, Changed: f.live != on}
	f.live = on
	return ch, nil
}

type rig struct {
	t     *testing.T
	state string
	be    *fakeBackend
	srv   *Server
	logs  *logx.MemorySink
	audit *logx.MemorySink
	lg    *logx.Logger
	cl    *Client
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	r := &rig{t: t, state: filepath.Join(root, "_state"), logs: logx.NewMemorySink(), audit: logx.NewMemorySink()}
	r.be = &fakeBackend{running: true, records: map[string]bool{"Front": false, "Back Door": true},
		snapshot: &status.Document{Schema: 1, State: status.StateRunning, SupervisorPid: 4242,
			Cameras: []status.Camera{{Name: "Front", SafeName: "Front", Record: true, Alive: true, InputMode: "relay", RestartsTotal: 2}}}}
	lg, _ := logx.New(logx.Options{Config: logx.Config{Level: "trace"}, Getenv: func(string) string { return "" }, Sink: r.logs, Audit: r.audit})
	r.lg = lg
	r.srv = NewServer(Options{StateDir: r.state, Backend: r.be, Levels: lg.Levels(), Runner: cli.NewRunner(nil, lg),
		Log: lg.For("ctlapi"), Redact: lg.Redactor().Register})
	if err := r.srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.srv.Stop()
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		r.srv.Wait(ctx)
	})
	cl, err := Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	r.cl = cl
	return r
}

// raw sends a request with full control of the headers.
func (r *rig) raw(method, path, body string, hdr map[string]string) (int, string, http.Header) {
	r.t.Helper()
	req, _ := http.NewRequest(method, "http://"+r.srv.Addr()+path, strings.NewReader(body))
	req.Header.Set(HeaderKey, r.srv.secret)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestEveryRouteIsAnAction(t *testing.T) {
	var exposed cli.Exposed
	for _, rt := range Routes {
		exposed = append(exposed, rt.Action)
	}
	cli.AssertAllExposedAreActions(t, exposed)
	// negative control: an unregistered name is reported
	ft := &fakeTB{}
	cli.AssertAllExposedAreActions(ft, cli.Exposed{"ctlapi.nope"})
	if ft.n != 1 {
		t.Error("the assertion does not catch an unregistered route")
	}
}

type fakeTB struct{ n int }

func (f *fakeTB) Helper()                        {}
func (f *fakeTB) Errorf(format string, a ...any) { f.n++ }

func TestAuthAndHostGuard(t *testing.T) {
	r := newRig(t)
	_, port, _ := strings.Cut(r.srv.Addr(), ":")
	cases := []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"good", nil, 200},
		{"localhost host", map[string]string{"Host": "localhost:" + port}, 200},
		{"missing key", map[string]string{HeaderKey: ""}, 401},
		{"wrong key", map[string]string{HeaderKey: strings.Repeat("0", 64)}, 401},
		{"key prefix", map[string]string{HeaderKey: r.srv.secret[:63]}, 401},
		{"rebinding host", map[string]string{"Host": "evil.example:" + port}, 403},
		{"other port", map[string]string{"Host": "127.0.0.1:1"}, 403},
		{"no port", map[string]string{"Host": "127.0.0.1"}, 403},
		{"browser origin", map[string]string{"Origin": "http://evil.example"}, 403},
	}
	for _, c := range cases {
		code, body, hdr := r.raw("GET", "/v1/state", "", c.hdr)
		if code != c.code {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
		if code == 401 || code == 403 {
			if strings.TrimSpace(body) != "" {
				t.Errorf("%s: refusal has a body %q", c.name, body)
			}
			if hdr.Get("Access-Control-Allow-Origin") != "" {
				t.Errorf("%s: CORS header", c.name)
			}
		}
	}
	// a refused request never reaches an action: two good requests, a begin and a result each
	if n := strings.Count(r.logs.String(), "action=ctlapi.state"); n != 4 {
		t.Errorf("%d action records, want 4:\n%s", n, r.logs.String())
	}
}

func TestBodyContentTypeAndMethod(t *testing.T) {
	r := newRig(t)
	big := `{"kind":"` + strings.Repeat("x", MaxBody) + `"}`
	if code, body, _ := r.raw("POST", "/v1/stop", big, nil); code != http.StatusRequestEntityTooLarge {
		t.Errorf("big body: %d %s", code, body)
	}
	if len(r.be.stops) != 0 {
		t.Fatal("a too-large request stopped the supervisor")
	}
	if code, _, _ := r.raw("POST", "/v1/reload", "{}", map[string]string{"Content-Type": "text/plain"}); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", code)
	}
	if code, _, _ := r.raw("POST", "/v1/live", `{"enabled":true,"extra":1}`, nil); code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", code)
	}
	if code, _, _ := r.raw("POST", "/v1/live", `{}`, nil); code != http.StatusBadRequest {
		t.Errorf("missing enabled: %d", code)
	}
	if code, _, _ := r.raw("DELETE", "/v1/state", "", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: %d", code)
	}
	if code, _, _ := r.raw("GET", "/v1/nothing", "", nil); code != http.StatusNotFound {
		t.Errorf("unknown path: %d", code)
	}
	if code, _, _ := r.raw("POST", "/v1/stop", `{"kind":"window-closed"}`, nil); code != http.StatusBadRequest || len(r.be.stops) != 0 {
		t.Errorf("bad kind: %d", code)
	}
}

// G14: one state-changing action at a time; the second concurrent one gets 409 and runs nothing.
func TestConcurrentMutationsGet409(t *testing.T) {
	r := newRig(t)
	r.be.block, r.be.entered = make(chan struct{}), make(chan struct{}, 16)
	first := make(chan error, 1)
	go func() { first <- r.cl.Reload(context.Background()) }()
	<-r.be.entered // the first holds the action lock
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // bounded: without the lock they would block
			defer cancel()
			if i == 2 {
				_, errs[i] = r.cl.SetLive(ctx, true)
				return
			}
			errs[i] = r.cl.Reload(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		var ae *APIError
		if !errors.Is(err, ErrBusy) || !errors.As(err, &ae) || ae.Code != 409 {
			t.Errorf("concurrent %d: %v", i, err)
		}
	}
	// a read is not serialised
	if _, err := r.cl.State(context.Background()); err != nil {
		t.Errorf("state during a mutation: %v", err)
	}
	close(r.be.block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if r.be.reloads != 1 || r.be.live {
		t.Errorf("reloads %d live %v: a refused request ran", r.be.reloads, r.be.live)
	}
	if err := r.cl.Reload(context.Background()); err != nil {
		t.Errorf("after the first finished: %v", err)
	}
}

// The caller's action id reaches the backend and every record of the request.
func TestActionIDPropagation(t *testing.T) {
	r := newRig(t)
	ctx := cli.WithActor(cli.WithActionID(context.Background(), "a1b2c3d4e5f60718"), "cli:op")
	ch, err := r.cl.SetRecord(ctx, "Front", true)
	if err != nil || !ch.Changed || !ch.Enabled {
		t.Fatalf("%+v %v", ch, err)
	}
	if len(r.be.ids) != 1 || r.be.ids[0] != "a1b2c3d4e5f60718" {
		t.Errorf("backend saw %v", r.be.ids)
	}
	var begin, result int
	for _, l := range r.logs.Lines() {
		if !strings.Contains(l, "action=ctlapi.camera.record") {
			continue
		}
		if !strings.Contains(l, "action_id=a1b2c3d4e5f60718") || !strings.Contains(l, "actor=cli:op") || !strings.Contains(l, "target=Front") {
			t.Errorf("record without the caller's id/actor/target: %s", l)
		}
		if strings.Contains(l, "action.begin") {
			begin++
		}
		if strings.Contains(l, "action.result") && strings.Contains(l, "INFO") {
			result++
		}
	}
	if begin != 1 || result != 1 {
		t.Errorf("begin %d result %d:\n%s", begin, result, r.logs.String())
	}
	if !strings.Contains(r.audit.String(), "a1b2c3d4e5f60718") {
		t.Error("mutating action not in the audit stream")
	}
	// no id given: one is generated and returned
	code, _, hdr := r.raw("POST", "/v1/reload", "{}", map[string]string{HeaderActionID: "bad id with spaces"})
	if code != 200 || !actionIDRE.MatchString(hdr.Get(HeaderActionID)) || hdr.Get(HeaderActionID) == "bad id with spaces" {
		t.Errorf("generated id: %d %q", code, hdr.Get(HeaderActionID))
	}
}

func TestEndpoints(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	st, err := r.cl.State(ctx)
	if err != nil || !st.Running || st.Pid != 4242 || len(st.Cameras) != 1 || st.Cameras[0].Mode != "relay" || st.Cameras[0].Restarts != 2 {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := r.cl.SetRecord(ctx, "Nope", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown camera: %v", err)
	}
	if ch, err := r.cl.SetRecord(ctx, "Back Door", true); err != nil || ch.Changed {
		t.Errorf("no-op record: %+v %v", ch, err)
	}
	if ch, err := r.cl.SetLive(ctx, true); err != nil || !ch.Changed || !r.be.live {
		t.Errorf("live: %+v %v", ch, err)
	}
	// log level: immediate, per component and all (proved by what reaches the sink)
	sup := r.lg.For("supervisor")
	logged := func(level slog.Level, msg string) bool {
		sup.Log(ctx, level, msg)
		return strings.Contains(r.logs.String(), msg)
	}
	if !logged(logx.LevelTrace, "probe-trace-1") {
		t.Fatal("rig level")
	}
	lv, err := r.cl.SetLevel(ctx, "supervisor", "warn")
	if err != nil || lv.Components["supervisor"] != "WARN" || logged(logx.LevelInfo, "probe-info-2") || !logged(logx.LevelWarn, "probe-warn-3") {
		t.Errorf("set supervisor warn: %+v %v", lv, err)
	}
	if _, err := r.cl.SetLevel(ctx, "supervisor", "loud"); !errors.Is(err, cli.ErrRefused) {
		t.Errorf("bad level: %v", err)
	}
	lv, err = r.cl.SetLevel(ctx, "all", "debug")
	if err != nil || lv.Default != "DEBUG" || len(lv.Components) != 0 || !logged(logx.LevelDebug, "probe-debug-4") || logged(logx.LevelTrace, "probe-trace-5") {
		t.Errorf("all debug: %+v %v", lv, err)
	}
	if got, err := r.cl.Levels(ctx); err != nil || got.Default != "DEBUG" {
		t.Errorf("get: %+v %v", got, err)
	}
	r.be.mu.Lock()
	r.be.running = false
	r.be.mu.Unlock()
	if _, err := r.cl.State(ctx); !errors.Is(err, ErrNotRunning) {
		t.Errorf("not running: %v", err)
	}
	rep, err := r.cl.Stop(ctx, "")
	if err != nil || rep.Kind != "requested" || rep.Survivors == nil {
		t.Errorf("stop: %+v %v", rep, err)
	}
}

// The supervisor stops the endpoint first in its ordered stop, which a stop request waits for: the
// request still gets its report, the files go at once, nothing new is accepted.
func TestStopRequestSurvivesTheServerStop(t *testing.T) {
	r := newRig(t)
	r.be.onStop = func() {
		r.srv.Stop()
		if _, err := os.Stat(filepath.Join(r.state, AddrFile)); !os.IsNotExist(err) {
			t.Errorf("ctl.addr still there during the stop: %v", err)
		}
		if _, err := r.cl.State(context.Background()); !errors.Is(err, ErrUnreachable) {
			t.Errorf("a new request was accepted after Stop: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	rep, err := r.cl.Stop(context.Background(), "requested")
	if err != nil || rep.Kind != "requested" || !rep.Final {
		t.Fatalf("%+v %v", rep, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.srv.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(filepath.Dir(r.state)); !errors.Is(err, ErrUnreachable) {
		t.Errorf("dial after a clean exit: %v", err)
	}
}

func TestFilesOwnerOnlyAndRemoved(t *testing.T) {
	r := newRig(t)
	for _, n := range []string{AddrFile, SecretFile} {
		p := filepath.Join(r.state, n)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", n, fi.Mode().Perm())
		}
		assertOwnerOnlyOS(t, p)
	}
	b, _ := os.ReadFile(filepath.Join(r.state, SecretFile))
	if !secretRE.Match(b) {
		t.Errorf("secret %q is not 64 lowercase hex without a newline", b)
	}
	if a, _ := os.ReadFile(filepath.Join(r.state, AddrFile)); string(a) != r.srv.Addr() || !strings.HasPrefix(string(a), "127.0.0.1:") {
		t.Errorf("addr %q", a)
	}
	leftovers, _ := filepath.Glob(filepath.Join(r.state, "*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left: %v", leftovers)
	}
	r.srv.Stop()
	for _, n := range []string{AddrFile, SecretFile} {
		if _, err := os.Stat(filepath.Join(r.state, n)); !os.IsNotExist(err) {
			t.Errorf("%s not removed: %v", n, err)
		}
	}
	// a second server gets a fresh secret
	r2 := newRig(t)
	if r2.srv.secret == r.srv.secret {
		t.Error("secret reused")
	}
}

func TestRefusesNonLoopbackListen(t *testing.T) {
	lg, _ := logx.New(logx.Options{Getenv: func(string) string { return "" }, Sink: logx.NewMemorySink()})
	s := NewServer(Options{StateDir: t.TempDir(), Backend: &fakeBackend{}, Runner: cli.NewRunner(nil, lg), ListenAddr: "0.0.0.0:0"})
	if err := s.Start(); err == nil {
		s.Stop()
		t.Fatal("listened beyond loopback")
	}
}

// leaked reports the sinks that contain the secret.
func leaked(secret string, sinks map[string]string) []string {
	var out []string
	for name, text := range sinks {
		if strings.Contains(text, secret) {
			out = append(out, name)
		}
	}
	return out
}

// The secret never reaches a log, at TRACE, whatever the request carries; the scan finds it when
// it is there (positive control).
func TestNoSecretInLogs(t *testing.T) {
	r := newRig(t)
	secret := r.srv.secret
	ctx := context.Background()
	r.cl.State(ctx)
	r.cl.Reload(ctx)
	r.cl.SetLevel(ctx, "ctlapi", "trace")
	r.raw("GET", "/v1/state", "", map[string]string{HeaderKey: secret + "x"})
	r.raw("POST", "/v1/live", `{"enabled":true,"x":"`+secret+`"}`, nil)
	r.lg.For("ctlapi").Log(ctx, logx.LevelTrace, "a careless line with key="+secret)
	if got := leaked(secret, map[string]string{"main": r.logs.String(), "audit": r.audit.String()}); len(got) != 0 {
		t.Fatalf("secret in %v", got)
	}
	// positive control: an unredacted sink with the same traffic is caught
	raw := logx.NewMemorySink()
	raw.Write([]byte("key=" + secret + "\n"))
	if got := leaked(secret, map[string]string{"raw": raw.String()}); len(got) != 1 {
		t.Fatal("the scan does not find a secret that is there")
	}
	if !strings.Contains(r.logs.String(), "action=ctlapi") {
		t.Fatal("no records scanned")
	}
}

func TestDialErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := Dial(root); !errors.Is(err, ErrUnreachable) {
		t.Errorf("no files: %v", err)
	}
	st := filepath.Join(root, "_state")
	os.MkdirAll(st, 0o700)
	os.WriteFile(filepath.Join(st, AddrFile), []byte("192.0.2.1:80"), 0o600)
	os.WriteFile(filepath.Join(st, SecretFile), []byte(strings.Repeat("a", 64)), 0o600)
	if _, err := Dial(root); !errors.Is(err, ErrUnreachable) {
		t.Errorf("non-loopback address accepted: %v", err)
	}
	os.WriteFile(filepath.Join(st, AddrFile), []byte("127.0.0.1:9"), 0o600)
	cl, err := Dial(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.State(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Errorf("closed port: %v", err)
	}
}
