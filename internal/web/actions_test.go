package web

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"holocron/internal/cli"
	"holocron/internal/logx"
)

// webLogs is a logx.Logger over memory sinks: the main log and the audit stream.
type webLogs struct {
	*logx.Logger
	main, audit *logx.MemorySink
}

func newWebLogs(level string) *webLogs {
	w := &webLogs{main: logx.NewMemorySink(), audit: logx.NewMemorySink()}
	w.Logger, _ = logx.New(logx.Options{Config: logx.Config{Level: level}, Getenv: func(string) string { return "" }, Sink: w.main, Audit: w.audit})
	return w
}

func lineHas(l, action string) bool {
	return strings.Contains(l, " action="+action+" ") || strings.HasSuffix(l, " action="+action)
}

// lines of the main sink that are the action's records with this message ("action.begin" or "action.result").
func (w *webLogs) records(action, msg string) []string {
	var out []string
	for _, l := range w.main.Lines() {
		if lineHas(l, action) && strings.Contains(l, " "+msg+" ") {
			out = append(out, l)
		}
	}
	return out
}

type fakeCtl struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeCtl) note(s string) ActionResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return ActionResult{Command: s}
}
func (f *fakeCtl) Name() string { return "fake" }
func (f *fakeCtl) Status(context.Context) (ControllerState, error) {
	return ControllerState{Running: true}, nil
}
func (f *fakeCtl) Start(_ context.Context, scope string) ActionResult {
	return f.note("start " + scope)
}
func (f *fakeCtl) Stop(_ context.Context, scope string) ActionResult { return f.note("stop " + scope) }
func (f *fakeCtl) TaskStates() map[string]TaskInfo                   { return map[string]TaskInfo{} }
func (f *fakeCtl) SetRecord(_ context.Context, c string, on bool) ActionResult {
	return f.note("record " + c + " " + map[bool]string{true: "on", false: "off"}[on])
}

func webSandbox(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "_state"), 0o700)
	os.WriteFile(filepath.Join(dir, "cameras.json"), []byte(`[{"Name":"Front","IP":"192.0.2.10","Quality":"stream1","Record":true}]`), 0o600)
	os.WriteFile(filepath.Join(dir, "recorder.config.json"), []byte(`{}`), 0o600)
	return Config{Listen: "127.0.0.1:8787", ArchiveRoot: dir, StatusFile: filepath.Join(dir, "_state", "status.json"), ScriptDir: dir, AuthDir: filepath.Join(dir, "auth")}
}

var requiredFields = []string{" action=", " actor=", " result=", " duration_ms=", " action_id=", " audit=true", " pid="}

func (w *webLogs) assertOneThread(t *testing.T, action, actor, target, result, level string) {
	t.Helper()
	begin, res := w.records(action, "action.begin"), w.records(action, "action.result")
	if len(begin) != 1 || len(res) != 1 {
		t.Errorf("%s: %d begin and %d result records, want 1 and 1\n%s", action, len(begin), len(res), strings.Join(w.main.Lines(), "\n"))
		return
	}
	for _, f := range requiredFields {
		if !strings.Contains(res[0], f) {
			t.Errorf("%s: result record lacks%s: %s", action, f, res[0])
		}
	}
	if !strings.Contains(begin[0], " DEBUG ") || !strings.Contains(begin[0], " actor="+actor) {
		t.Errorf("%s: begin record %s", action, begin[0])
	}
	if !strings.Contains(res[0], " "+level+" ") || !strings.Contains(res[0], " actor="+actor+" ") || !strings.Contains(res[0], " result="+result+" ") || !strings.Contains(res[0], "[web]") {
		t.Errorf("%s: result record %s, want %s actor %s result %s", action, res[0], level, actor, result)
	}
	if target != "" && !strings.Contains(res[0], " target="+target+" ") {
		t.Errorf("%s: target %q not in %s", action, target, res[0])
	}
	id := regexp.MustCompile(`action_id=(\w+)`)
	if b, r := id.FindStringSubmatch(begin[0]), id.FindStringSubmatch(res[0]); len(b) != 2 || len(r) != 2 || b[1] != r[1] {
		t.Errorf("%s: begin and result do not share an action id: %v %v", action, b, r)
	}
	found := false
	for _, l := range w.audit.Lines() {
		found = found || (lineHas(l, action) && strings.Contains(l, " action.result "))
	}
	if !found {
		t.Errorf("%s: the audit stream has no result record", action)
	}
}

type httpReq struct {
	method, path, body, ctype string
}

func doReq(h http.Handler, q httpReq) *httptest.ResponseRecorder {
	req := httptest.NewRequest(q.method, q.path, strings.NewReader(q.body))
	if q.ctype == "" {
		q.ctype = "application/json"
	}
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", q.ctype)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// R2 for the web app: every mutating route leaves one begin and one result record with action, actor, target,
// result, duration and audit=true, and the audit stream gets the result.
func TestEveryMutatingRouteLeavesBeginAndResult(t *testing.T) {
	cfg := webSandbox(t)
	logs := newWebLogs("debug")
	ctl := &fakeCtl{}
	h, err := newHandler(cfg, logs.For("web"), withLogs(logs), withController(ctl), withRequestLevel(slog.LevelDebug))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		q                           httpReq
		action, target, result, lvl string
		code                        int
	}{
		{httpReq{method: "POST", path: "/api/control", body: `{"scope":"all","action":"stop"}`}, ActionControl, "all/stop", "ok", "INFO", 200},
		{httpReq{method: "POST", path: "/api/cameras/Front/record", body: `{"enabled":false}`}, ActionRecord, "Front", "ok", "INFO", 200},
		{httpReq{method: "POST", path: "/api/acknowledge", body: `{"notice":"LastWindowClose","at":"2026-01-01T00:00:00Z"}`}, ActionAcknowledge, "LastWindowClose", "ok", "INFO", 200},
		{httpReq{method: "PUT", path: "/api/config", body: `{"config":{"SegmentSeconds":-5}}`}, ActionConfigPut, "config", "refused", "WARN", 422},
		{httpReq{method: "PUT", path: "/api/cameras", body: `{"cameras":[{"originalName":"","name":"","ip":"","quality":"","record":true}]}`}, ActionCamerasPut, "cameras", "refused", "WARN", 422},
		{httpReq{method: "POST", path: "/api/live/Front/whep?stream=sub", body: "v=0", ctype: "application/sdp"}, ActionLiveCreate, "Front", "refused", "WARN", 503},
		{httpReq{method: "DELETE", path: "/api/live/session/abc"}, ActionLiveDelete, "abc", "refused", "WARN", 404},
		{httpReq{method: "POST", path: "/api/auth/setup", body: loginBody("anna", "correct horse battery staple")}, ActionSetup, "anna", "ok", "INFO", 201},
	}
	for _, c := range cases {
		if rec := doReq(h, c.q); rec.Code != c.code {
			t.Errorf("%s %s: %d, want %d: %s", c.q.method, c.q.path, rec.Code, c.code, rec.Body.String())
		}
		logs.assertOneThread(t, c.action, "web:loopback", c.target, c.result, c.lvl)
	}
	if got := strings.Join(ctl.calls, "|"); got != "stop all|record Front off" {
		t.Errorf("the controller was asked: %s", got)
	}
	// a refusal before any handler (the guard) is not an action: the guard logs it, no action record is faked
	before := len(logs.records(ActionControl, "action.result"))
	if rec := doReq(h, httpReq{method: "POST", path: "/api/control", body: "x", ctype: "text/plain"}); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON body: %d", rec.Code)
	}
	if len(logs.records(ActionControl, "action.result")) != before {
		t.Error("a guard refusal produced an action record")
	}
}

// Read handlers leave a DEBUG request line (component web) and no action record; at INFO they leave nothing.
func TestReadHandlersLogAtDebug(t *testing.T) {
	cfg := webSandbox(t)
	for _, level := range []string{"debug", "info"} {
		logs := newWebLogs(level)
		h, _ := newHandler(cfg, logs.For("web"), withLogs(logs), withController(&fakeCtl{}), withRequestLevel(slog.LevelDebug))
		for _, p := range []string{"/api/status", "/api/cameras", "/api/config", "/api/live"} {
			doReq(h, httpReq{method: "GET", path: p})
		}
		var requests []string
		for _, l := range logs.main.Lines() {
			if strings.Contains(l, " request ") && strings.Contains(l, "method=GET") {
				requests = append(requests, l)
			}
			if strings.Contains(l, "action.result") {
				t.Errorf("a read produced an action record: %s", l)
			}
		}
		switch level {
		case "debug":
			if len(requests) != 4 || !strings.Contains(requests[0], " DEBUG ") || !strings.Contains(requests[0], "[web]") || !strings.Contains(requests[0], "path=/api/status") {
				t.Errorf("debug: %v", requests)
			}
		case "info":
			if len(requests) != 0 {
				t.Errorf("info: request lines %v", requests)
			}
		}
	}
}

// Without Options.Logs (camui.exe) the request lines stay at INFO in slog's text format, as the PowerShell harness reads them.
func TestStandaloneRequestLinesStayInfo(t *testing.T) {
	cfg := webSandbox(t)
	var sb strings.Builder
	log := slog.New(slog.NewTextHandler(&sb, nil))
	h, _ := newHandler(cfg, log, withController(&fakeCtl{}))
	doReq(h, httpReq{method: "GET", path: "/api/status"})
	if line := sb.String(); !strings.Contains(line, "level=INFO msg=request method=GET path=/api/status") {
		t.Errorf("%q", line)
	}
}

// A handler that panics: the Runner records an ERROR, the client gets a 500, nothing is left half-written.
func TestPanickingActionIsAnErrorAndA500(t *testing.T) {
	logs := newWebLogs("info")
	h := action(cli.NewRunner(nil, logs), ActionControl, func(http.ResponseWriter, *http.Request) { panic("boom") })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/api/control", nil))
	if rec.Code != 500 {
		t.Errorf("status %d", rec.Code)
	}
	res := logs.records(ActionControl, "action.result")
	if len(res) != 1 || !strings.Contains(res[0], " ERROR ") || !strings.Contains(res[0], "result=fail") {
		t.Errorf("%v", res)
	}
}

// On the LAN listener the actor is the signed-in user, and a sign-in attempt is anonymous.
func TestLANActors(t *testing.T) {
	logs := newWebLogs("debug")
	rig := newTLSRigLogx(t, logs, func(c *Config) { c.TLSReadOnly = boolPtr(false) })
	cookie := rig.login(t)
	logs.assertOneThread(t, ActionLogin, "web:anonymous", testUser, "ok", "INFO")
	if rec := rig.do(tlsReq{method: "POST", path: "/api/cameras/Front/record", headers: map[string]string{"Content-Type": "application/json", "Origin": "https://" + lanHost}, body: `{"enabled":false}`, cookie: cookie}); rec.Code != 200 {
		t.Fatalf("record: %d %s", rec.Code, rec.Body.String())
	}
	logs.assertOneThread(t, ActionRecord, "web:"+testUser, "Front", "ok", "INFO")
	if rec := rig.do(tlsReq{method: "POST", path: "/api/auth/logout", headers: map[string]string{"Content-Type": "application/json", "Origin": "https://" + lanHost}, cookie: cookie}); rec.Code != 200 {
		t.Fatalf("logout: %d", rec.Code)
	}
	logs.assertOneThread(t, ActionLogout, "web:"+testUser, testUser, "ok", "INFO")
	// a failed sign-in is a WARN refusal; the password never reaches the log
	rig.do(tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, body: loginBody(testUser, "wrong password entirely")})
	if res := logs.records(ActionLogin, "action.result"); len(res) != 2 || !strings.Contains(res[1], " WARN ") || !strings.Contains(res[1], "result=refused") {
		t.Errorf("failed login records: %v", res)
	}
	for _, l := range logs.main.Lines() {
		if strings.Contains(l, "wrong password entirely") || strings.Contains(l, testPassword) {
			t.Errorf("a password reached the log: %s", l)
		}
	}
}

// newTLSRigLogx is newTLSRig with the web app's logx logger: the Runner's records and the request
// lines land in logs. The camera list and recorder config are in the sandbox the handler reads.
func newTLSRigLogx(t *testing.T, logs *webLogs, mutate func(*Config)) *tlsRig {
	t.Helper()
	cfg := webSandbox(t)
	cfg.TLSListen, cfg.TLSDir = lanListen, filepath.Join(cfg.ArchiveRoot, "tls")
	if mutate != nil {
		mutate(&cfg)
	}
	log := logs.For("web")
	mgr := newCertManager(cfg.TLSDir, log)
	ip, _, err := parseTLSListen(cfg.TLSListen)
	if err != nil {
		t.Fatal(err)
	}
	names, err := buildCertNames(ip, cfg.TLSHosts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Prepare(names); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	users := newFileUserStore(cfg.AuthDir, clock.now)
	hasher := fastHasher()
	hash, _ := hasher.Hash(testPassword)
	if err := users.Put(User{ID: "u-anna", Username: testUser, Hash: hash, Role: roleAdmin, CreatedAt: clock.now()}); err != nil {
		t.Fatal(err)
	}
	auth := newAuthService(cfg.AuthDir, users, logs.For("auth"), clock.now, hasher)
	h, err := newTLSHandler(cfg, log, mgr, names, auth, withLogs(logs), withController(&fakeCtl{}), withRequestLevel(slog.LevelDebug))
	if err != nil {
		t.Fatal(err)
	}
	return &tlsRig{handler: h, logs: &bytes.Buffer{}, mgr: mgr, cfg: cfg, auth: auth, clock: clock, users: users}
}
