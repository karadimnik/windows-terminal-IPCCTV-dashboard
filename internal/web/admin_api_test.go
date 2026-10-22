package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/cli"
)

// fakeTools is AdminTools with canned answers; it remembers what it was asked (never a value).
type fakeTools struct {
	mu    sync.Mutex
	calls []string
	user  string // what SetCameraLogin received, for the leak scan's positive control
	pass  string
}

func (f *fakeTools) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeTools) ConfigKeys(context.Context) ([]ConfigKey, error) {
	f.note("keys")
	return []ConfigKey{{Key: "SegmentSeconds", Value: 900, Default: 900, Rule: "whole number >= 1", Restart: true}}, nil
}

func (f *fakeTools) SetConfigKey(_ context.Context, key string, value *string) (string, []FieldError, error) {
	switch {
	case key == "NotEditable":
		return "", nil, cli.Refuse("%q is not a key `holocron config set` may change", key)
	case value != nil && *value == "-5":
		return "", []FieldError{{Field: key, Message: "must be a whole number >= 1"}}, nil
	case value == nil:
		f.note("unset " + key)
		return key + ": 600 -> (default)", nil, nil
	}
	f.note("set " + key + " " + *value)
	return key + ": 900 -> " + *value, nil, nil
}

func (f *fakeTools) CameraLogin(context.Context) (CameraLoginState, error) {
	return CameraLoginState{Backend: "file", User: f.user != "", Password: f.pass != ""}, nil
}

func (f *fakeTools) SetCameraLogin(_ context.Context, user, password string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if user != "" {
		f.user = user
	}
	if password != "" {
		f.pass = password
	}
	return "stored the camera login (file store)", nil
}

func (f *fakeTools) PrunePreview(context.Context) (any, error) {
	return map[string]any{"dryRun": true, "deletions": []any{}}, nil
}
func (f *fakeTools) ArchiveStats(context.Context) (any, error) {
	return map[string]any{"segments": 3}, nil
}
func (f *fakeTools) LogLevels(context.Context) (any, error) {
	return map[string]any{"running": false}, nil
}
func (f *fakeTools) SetLogLevel(_ context.Context, c, l string, save bool) (string, error) {
	if l == "loud" {
		return "", BadInput("unknown level %q", l)
	}
	f.note("level " + c + " " + l)
	return "running supervisor: default " + l, nil
}
func (f *fakeTools) Autostart(context.Context) (any, error) {
	return map[string]any{"installed": false}, nil
}
func (f *fakeTools) SetAutostart(_ context.Context, on, ok bool) (string, error) {
	if on && !ok {
		return "", cli.Refuse("the PowerShell system task(s) IPCCTV-Recorder are enabled")
	}
	f.note("autostart " + map[bool]string{true: "on", false: "off"}[on])
	return "done", nil
}

func (f *fakeTools) Health(_ context.Context, groups []string) (any, time.Time, error) {
	f.note("health " + strings.Join(groups, ","))
	return map[string]any{"schema": 1, "groups": groups, "checks": []any{}, "summary": map[string]any{"verdict": "HEALTHY"}}, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), nil
}

// adminRig is the LAN listener (not view-only) with the Admin tools, anna (admin) signed in.
type adminRig struct {
	*tlsRig
	logs   *webLogs
	tools  *fakeTools
	cookie string
	bodies []string // every answer, for the leak scan
}

func newAdminRig(t *testing.T) *adminRig {
	t.Helper()
	logs := newWebLogs("trace")
	tools := &fakeTools{}
	rig := newTLSRigLogx(t, logs, func(c *Config) { c.TLSReadOnly = boolPtr(false) }, withAdmin(tools))
	return &adminRig{tlsRig: rig, logs: logs, tools: tools, cookie: rig.loginAs(t, testUser, testPassword)}
}

func (a *adminRig) call(method, path, body string) *httptest.ResponseRecorder {
	q := tlsReq{method: method, path: path, cookie: a.cookie, headers: map[string]string{"Content-Type": "application/json", "Origin": "https://" + lanHost}}
	if body != "" {
		q.body = body
	}
	rec := a.do(q)
	a.bodies = append(a.bodies, rec.Body.String())
	return rec
}

func (a *adminRig) as(cookie string) *adminRig {
	c := *a
	c.cookie = cookie
	return &c
}

func TestAdminUsersOverTheLAN(t *testing.T) {
	a := newAdminRig(t)
	// add a viewer, then sign in as it
	if rec := a.call("POST", "/api/admin/users/add", `{"username":"bob","password":"`+viewerPassword+`","role":"viewer"}`); rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("POST", "/api/admin/users/add", `{"username":"BOB","password":"`+viewerPassword+`"}`); rec.Code != 409 {
		t.Errorf("duplicate: %d", rec.Code)
	}
	for body, want := range map[string]int{
		`{"username":"carol","password":"short"}`:                                422,
		`{"username":"bad name","password":"a long enough password"}`:            422,
		`{"username":"carol","password":"a long enough password","role":"root"}`: 422,
		`{"username":"carol","password":"a long enough password","x":1}`:         400,
		`not json`: 400,
	} {
		if rec := a.call("POST", "/api/admin/users/add", body); rec.Code != want {
			t.Errorf("add %s: %d want %d", body, rec.Code, want)
		}
	}
	bob := a.as(a.loginAs(t, viewerUser, viewerPassword))
	if rec := bob.call("GET", "/api/admin/users", ""); !isAdminRefusal(rec) {
		t.Errorf("the viewer listed users: %d", rec.Code)
	}
	list := a.call("GET", "/api/admin/users", "")
	var got struct {
		Users []UserInfo `json:"users"`
		Self  string     `json:"self"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &got); err != nil || len(got.Users) != 2 || got.Users[1].Role != roleViewer || got.Self != testUser {
		t.Fatalf("list: %v %s", err, list.Body.String())
	}
	if strings.Contains(list.Body.String(), "hash") || strings.Contains(list.Body.String(), "pbkdf2") {
		t.Errorf("the list carries a hash: %s", list.Body.String())
	}

	// the last admin: anna can neither demote nor remove herself, nor be removed
	for _, c := range []struct{ path, body string }{
		{"/api/admin/users/role", `{"username":"anna","role":"viewer"}`},
		{"/api/admin/users/remove", `{"username":"anna"}`},
	} {
		if rec := a.call("POST", c.path, c.body); rec.Code != 409 || !strings.Contains(rec.Body.String(), "last admin") {
			t.Errorf("%s: %d %s", c.path, rec.Code, rec.Body.String())
		}
	}
	// promote bob: his own sessions end (he was signed in elsewhere), anna's stays
	if rec := a.call("POST", "/api/admin/users/role", `{"username":"bob","role":"admin"}`); rec.Code != 200 {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body.String())
	}
	if rec := bob.call("GET", "/api/status", ""); rec.Code != 401 {
		t.Errorf("bob's session survived his role change: %d", rec.Code)
	}
	// now anna may step down herself: her session stays and is a viewer's at once
	if rec := a.call("POST", "/api/admin/users/role", `{"username":"anna","role":"viewer"}`); rec.Code != 200 {
		t.Fatalf("self-demotion: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("GET", "/api/admin/users", ""); !isAdminRefusal(rec) {
		t.Errorf("after self-demotion: %d", rec.Code)
	}
	bob = a.as(a.loginAs(t, viewerUser, viewerPassword))
	other := a.as(a.loginAs(t, testUser, testPassword))
	// bob (admin) changes his own password: his page's session stays, his others end
	bobElsewhere := a.loginAs(t, viewerUser, viewerPassword)
	if rec := bob.call("POST", "/api/admin/users/password", `{"username":"bob","password":"bob's new long passphrase"}`); rec.Code != 200 {
		t.Fatalf("own password: %d %s", rec.Code, rec.Body.String())
	}
	if rec := bob.call("GET", "/api/admin/users", ""); rec.Code != 200 {
		t.Errorf("the page's own session was ended: %d", rec.Code)
	}
	if rec := a.as(bobElsewhere).call("GET", "/api/status", ""); rec.Code != 401 {
		t.Errorf("bob's other session survived: %d", rec.Code)
	}
	// unlock (applied at once in the running service), remove anna (a viewer now)
	if rec := bob.call("POST", "/api/admin/users/unlock", `{"username":"anna"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), "unlocked") {
		t.Errorf("unlock: %d %s", rec.Code, rec.Body.String())
	}
	if rec := bob.call("POST", "/api/admin/users/remove", `{"username":"anna"}`); rec.Code != 200 {
		t.Errorf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if rec := other.call("GET", "/api/status", ""); rec.Code != 401 {
		t.Errorf("a removed user's session: %d", rec.Code)
	}
	if rec := bob.call("POST", "/api/admin/users/remove", `{"username":"nobody"}`); rec.Code != 404 {
		t.Errorf("remove unknown: %d", rec.Code)
	}
	// bob is the last user and admin: the Admin tab never removes him
	if rec := bob.call("POST", "/api/admin/users/remove", `{"username":"bob"}`); rec.Code != 409 {
		t.Errorf("remove the very last admin from the web: %d", rec.Code)
	}

	// records: every change an Action in the audit stream, actor the signed-in user
	for _, name := range []string{ActionAdminUsersAdd, ActionAdminUsersRole, ActionAdminUsersPasswd, ActionAdminUsersUnlock, ActionAdminUsersRemove, ActionAdminUsersList} {
		found := false
		for _, l := range a.logs.audit.Lines() {
			found = found || (lineHas(l, name) && strings.Contains(l, "action.result") && strings.Contains(l, "audit=true"))
		}
		if !found {
			t.Errorf("%s is not in the audit stream", name)
		}
	}
	a.assertNoLeak(t, []string{viewerPassword, "bob's new long passphrase", testPassword})
}

var hashPattern = regexp.MustCompile(`pbkdf2|\$[0-9a-z]+\$[A-Za-z0-9+/=$.]{20,}`)

// secretHits returns the texts that hold one of the secrets or anything shaped like a password hash.
func secretHits(texts, secrets []string) []string {
	var hits []string
	for _, text := range texts {
		bad := hashPattern.MatchString(text)
		for _, s := range secrets {
			bad = bad || (s != "" && strings.Contains(text, s))
		}
		if bad {
			hits = append(hits, text)
		}
	}
	return hits
}

// assertNoLeak scans every answer, the log (TRACE) and the audit stream; the scan itself is proven
// by planted copies of the secrets and of a real stored hash.
func (a *adminRig) assertNoLeak(t *testing.T, secrets []string) {
	t.Helper()
	for where, texts := range map[string][]string{"an answer": a.bodies, "the log": a.logs.main.Lines(), "the audit stream": a.logs.audit.Lines()} {
		for _, hit := range secretHits(texts, secrets) {
			t.Errorf("a secret is in %s: %.200s", where, hit)
		}
	}
	planted := []string{"x " + storedHash(t, a.cfg.AuthDir) + " y"}
	for _, s := range secrets {
		planted = append(planted, `{"error":"`+s+`"}`)
	}
	if got := secretHits(planted, secrets); len(got) != len(planted) {
		t.Errorf("the leak scan's positive control found %d of %d", len(got), len(planted))
	}
}

// storedHash is one hash from users.json (the positive control of the hash pattern).
func storedHash(t *testing.T, dir string) string {
	raw, _ := os.ReadFile(filepath.Join(dir, usersFileName))
	var doc usersDoc
	if json.Unmarshal(raw, &doc) != nil || len(doc.Users) == 0 {
		t.Fatal("no users.json")
	}
	return doc.Users[0].Hash
}

func TestAdminToolsSections(t *testing.T) {
	a := newAdminRig(t)
	if rec := a.call("GET", "/api/admin/config", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"rule":"whole number \u003e= 1"`) {
		t.Errorf("keys: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("POST", "/api/admin/config/set", `{"key":"SegmentSeconds","value":"600"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `900 -\u003e 600`) {
		t.Errorf("set: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("POST", "/api/admin/config/set", `{"key":"SegmentSeconds","value":"-5"}`); rec.Code != 422 || !strings.Contains(rec.Body.String(), `"field":"SegmentSeconds"`) {
		t.Errorf("invalid: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("POST", "/api/admin/config/set", `{"key":"NotEditable","value":"1"}`); rec.Code != 409 || strings.HasPrefix(decode(t, rec)["error"].(string), "refused") {
		t.Errorf("not editable: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("POST", "/api/admin/config/set", `{"key":"SegmentSeconds"}`); rec.Code != 422 {
		t.Errorf("no value: %d", rec.Code)
	}
	if rec := a.call("POST", "/api/admin/config/unset", `{"key":"SegmentSeconds"}`); rec.Code != 200 {
		t.Errorf("unset: %d", rec.Code)
	}
	if rec := a.call("POST", "/api/admin/config/unset", `{}`); rec.Code != 422 {
		t.Errorf("unset without a key: %d", rec.Code)
	}
	// camera login: stored through the tools, never answered back
	const camUser, camPass = "cam-operator-x", "Cam-Pa55word-Never-Shown"
	if rec := a.call("POST", "/api/admin/camera-login", `{"user":"`+camUser+`","password":"`+camPass+`"}`); rec.Code != 200 {
		t.Errorf("camera login: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("POST", "/api/admin/camera-login", `{"user":"","password":""}`); rec.Code != 422 {
		t.Errorf("empty camera login: %d", rec.Code)
	}
	if rec := a.call("POST", "/api/admin/camera-login", `{"password":"two\nlines"}`); rec.Code != 422 {
		t.Errorf("a newline: %d", rec.Code)
	}
	st := a.call("GET", "/api/admin/camera-login", "")
	if st.Code != 200 || !strings.Contains(st.Body.String(), `"user":true`) || !strings.Contains(st.Body.String(), `"password":true`) {
		t.Errorf("status: %d %s", st.Code, st.Body.String())
	}
	if a.tools.pass != camPass || a.tools.user != camUser {
		t.Error("the tools did not receive the login (the leak scan's positive control)")
	}
	for _, p := range []string{"/api/admin/prune", "/api/admin/archive", "/api/admin/log", "/api/admin/autostart"} {
		if rec := a.call("GET", p, ""); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := a.call("POST", "/api/admin/log/level", `{"component":"all","level":"loud"}`); rec.Code != 422 {
		t.Errorf("bad level: %d", rec.Code)
	}
	if rec := a.call("POST", "/api/admin/log/level", `{"component":"--save","level":"debug"}`); rec.Code != 422 {
		t.Errorf("a flag as a component: %d", rec.Code)
	}
	if rec := a.call("POST", "/api/admin/log/level", `{"component":"web","level":"debug"}`); rec.Code != 200 {
		t.Errorf("level: %d", rec.Code)
	}
	if rec := a.call("POST", "/api/admin/autostart", `{"enabled":true}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "PowerShell") {
		t.Errorf("enable with old tasks: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("POST", "/api/admin/autostart", `{}`); rec.Code != 422 {
		t.Errorf("no enabled: %d", rec.Code)
	}
	if rec := a.call("POST", "/api/admin/autostart", `{"enabled":false}`); rec.Code != 200 {
		t.Errorf("disable: %d", rec.Code)
	}
	want := []string{"keys", "set SegmentSeconds 600", "unset SegmentSeconds", "level web debug", "autostart off"}
	if strings.Join(a.tools.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls %v, want %v", a.tools.calls, want)
	}
	a.assertNoLeak(t, []string{camUser, camPass, testPassword})
}

func TestAdminTLSSection(t *testing.T) {
	a := newAdminRig(t)
	rec := a.call("GET", "/api/admin/tls", "")
	body := decode(t, rec)
	info, _ := a.mgr.CAInfo()
	if rec.Code != 200 || body["present"] != true || body["sha256"] != info.Fingerprint {
		t.Fatalf("info: %d %v", rec.Code, body)
	}
	cert := a.call("GET", "/api/admin/tls/ca.crt", "")
	if cert.Code != 200 || !strings.Contains(cert.Body.String(), "BEGIN CERTIFICATE") || strings.Contains(cert.Body.String(), "PRIVATE KEY") {
		t.Errorf("download: %d", cert.Code)
	}
	for _, c := range []string{`{}`, `{"confirm":"rotate"}`, `{"confirm":"yes"}`} {
		if rec := a.call("POST", "/api/admin/tls/rotate", c); rec.Code != 422 {
			t.Errorf("rotate %s: %d", c, rec.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(a.cfg.TLSDir, caCertFile)); err != nil {
		t.Fatal("a refused rotation removed the CA")
	}
	if rec := a.call("POST", "/api/admin/tls/rotate", `{"confirm":"ROTATE"}`); rec.Code != 200 || decode(t, rec)["removed"] != float64(4) {
		t.Errorf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("GET", "/api/admin/tls", ""); decode(t, rec)["present"] != false {
		t.Errorf("after the rotation: %s", rec.Body.String())
	}
	if rec := a.call("GET", "/api/admin/tls/ca.crt", ""); rec.Code != 404 {
		t.Errorf("download after the rotation: %d", rec.Code)
	}
	for _, l := range a.logs.audit.Lines() {
		if strings.Contains(l, "BEGIN") {
			t.Errorf("certificate material in the audit stream: %s", l)
		}
	}
}

// Without AdminTools (camui.exe) the command-line sections answer 501; users and TLS still work.
// On loopback everything is reachable without a login.
func TestAdminOnLoopbackWithoutTools(t *testing.T) {
	h, cfg := loopbackHandler(t, func(c *Config) { c.TLSDir = filepath.Join(c.ArchiveRoot, "tls") })
	for _, p := range []string{"/api/admin/config", "/api/admin/camera-login", "/api/admin/prune", "/api/admin/archive", "/api/admin/log", "/api/admin/autostart", "/api/admin/health"} {
		if rec := (probe{path: p}).do(h); rec.Code != 501 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := (probe{path: "/api/admin/tls"}).do(h); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"present":false`) {
		t.Errorf("tls without a CA: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(cfg.TLSDir); err == nil {
		t.Error("reading the TLS section created a CA")
	}
	if rec := (probe{path: "/api/admin/users"}).do(h); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"users":[]`) {
		t.Errorf("users: %d %s", rec.Code, rec.Body.String())
	}
	add := probe{method: "POST", path: "/api/admin/users/add", headers: jsonHdr, body: `{"username":"alice","password":"a long enough password","role":"viewer"}`}
	if rec := add.do(h); rec.Code != 200 {
		t.Errorf("add on loopback: %d %s", rec.Code, rec.Body.String())
	}
	// the guard still applies to the new routes: a foreign origin and a form post are refused
	if rec := (probe{method: "POST", path: "/api/admin/users/remove", headers: map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}, body: `{"username":"alice"}`}).do(h); rec.Code != 403 {
		t.Errorf("cross-origin: %d", rec.Code)
	}
	if rec := (probe{method: "POST", path: "/api/admin/users/remove", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, body: `username=alice`}).do(h); rec.Code != 415 {
		t.Errorf("form post: %d", rec.Code)
	}
	if rec := (probe{method: "POST", path: "/api/admin/users/remove", headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"}, body: `{"username":"alice"}`}).do(h); rec.Code != 403 {
		t.Errorf("cross-site: %d", rec.Code)
	}
	if list, _ := usersWith(cfg.AuthDir, nil, fastHasher()).List(); len(list) != 1 {
		t.Errorf("a refused request changed the users: %+v", list)
	}
}

// Every Admin route is an Action named web.admin.*; the changes and the sensitive reads are audited.
func TestEveryAdminRouteIsAnAuditedAction(t *testing.T) {
	data, _ := os.ReadFile("admin_api.go")
	consts := map[string]string{}
	for _, m := range actionConst.FindAllStringSubmatch(string(data), -1) {
		consts[m[1]] = m[2]
	}
	plain, acted := scanRoutes(string(data), consts)
	if len(plain) != 0 {
		t.Errorf("admin routes registered without handleAction: %v", plain)
	}
	if len(acted) != len(adminWantAccess) {
		t.Errorf("%d admin routes found, %d classified", len(acted), len(adminWantAccess))
	}
	sensitive := map[string]bool{ActionAdminUsersList: true, ActionAdminTLSCA: true, ActionAdminTLSPrint: true, ActionAdminSecretStatus: true, ActionAdminAutostart: true}
	for pattern, name := range acted {
		if !strings.HasPrefix(pattern, "GET /api/admin/") && !strings.HasPrefix(pattern, "POST /api/admin/") {
			t.Errorf("%s: not under /api/admin/", pattern)
		}
		act, ok := cli.Default.Lookup(name)
		if !ok || !strings.HasPrefix(name, "web.admin.") {
			t.Errorf("%s: action %q", pattern, name)
			continue
		}
		mutating := strings.HasPrefix(pattern, "POST ")
		if act.Mutating != mutating || (mutating || sensitive[name]) != act.Audit {
			t.Errorf("%s (%s): mutating %v audit %v", pattern, name, act.Mutating, act.Audit)
		}
	}
}

// The Admin tab's markup and code live in admin.js, which only an admin receives; the page every
// user gets has none of it, and loads admin.js only for an admin.
func TestAdminBundleOnlyForAdmins(t *testing.T) {
	a := newAdminRig(t)
	a.addViewer(t)
	viewer := a.loginAs(t, viewerUser, viewerPassword)
	js := a.do(tlsReq{path: "/admin.js", cookie: a.cookie})
	if js.Code != 200 || !strings.Contains(js.Body.String(), "api/admin/users") || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("admin: %d %q", js.Code, js.Header().Get("Content-Type"))
	}
	if rec := a.do(tlsReq{path: "/admin.js", cookie: viewer}); !isAdminRefusal(rec) || strings.Contains(rec.Body.String(), "api/admin") {
		t.Errorf("viewer: %d", rec.Code)
	}
	if rec := a.do(tlsReq{path: "/admin.js"}); rec.Code != 401 {
		t.Errorf("no session: %d", rec.Code)
	}
	// every password field is a new-password field; no secret is ever put back into a field
	src := js.Body.String()
	for _, m := range regexp.MustCompile(`<input id="(a\w*Pass\w*)"[^>]*>`).FindAllStringSubmatch(src, -1) {
		if !strings.Contains(m[0], `type="password"`) || !strings.Contains(m[0], `autocomplete="new-password"`) {
			t.Errorf("%s: %s", m[1], m[0])
		}
	}
	if n := strings.Count(src, `type="password"`); n != 3 {
		t.Errorf("%d password fields, want 3 (new user, set password, camera)", n)
	}
	for _, page := range []string{"/", "/app.js", "/live.js", "/style.css"} {
		rec := a.do(tlsReq{path: page, cookie: viewer})
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "api/admin/") || strings.Contains(rec.Body.String(), `type="password"`) {
			t.Errorf("%s as a viewer: %d, carries admin markup", page, rec.Code)
		}
	}
	page := a.do(tlsReq{path: "/", cookie: viewer}).Body.String()
	if !regexp.MustCompile(`<section data-tab="admin" class="hide">\s*</section>`).MatchString(page) || !strings.Contains(page, `id="adminTab"`) {
		t.Error("the page's admin section is not the empty, hidden placeholder")
	}
	app := a.do(tlsReq{path: "/app.js", cookie: viewer}).Body.String()
	if !strings.Contains(app, `script.src = "admin.js"`) || !strings.Contains(app, "if (!viewer && !readOnly) await loadAdmin()") {
		t.Error("app.js does not load admin.js for admins only")
	}
}

// The Health section: the tools' document under "health", read-only (no audit record, DEBUG
// records), groups checked against the doctor's names before the tools are asked.
func TestAdminHealthRoute(t *testing.T) {
	a := newAdminRig(t)
	rec := a.call("GET", "/api/admin/health", "")
	body := decode(t, rec)
	h, _ := body["health"].(map[string]any)
	if rec.Code != 200 || body["ok"] != true || h["schema"] != float64(1) || body["checkedAt"] != "2026-10-22T12:00:00Z" {
		t.Fatalf("health: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call("GET", "/api/admin/health?group=recording,live&group=web", ""); rec.Code != 200 {
		t.Errorf("groups: %d %s", rec.Code, rec.Body.String())
	}
	asked := len(a.tools.calls)
	rec = a.call("GET", "/api/admin/health?group=recording,nope", "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "group takes setup, recording, connections, live, web, storage, system") || !strings.Contains(rec.Body.String(), `\"nope\"`) {
		t.Errorf("unknown group: %d %s", rec.Code, rec.Body.String())
	}
	if len(a.tools.calls) != asked {
		t.Errorf("the tools were asked about an invalid group: %v", a.tools.calls)
	}
	if want := []string{"health ", "health recording,live,web"}; strings.Join(a.tools.calls, "|") != strings.Join(want, "|") {
		t.Errorf("tools asked %q, want %q", a.tools.calls, want)
	}
	for _, l := range a.logs.audit.Lines() {
		if strings.Contains(l, ActionAdminHealth) && !strings.Contains(l, "access.denied") {
			t.Errorf("a read-only poll in the audit stream: %s", l)
		}
	}
	res := a.logs.records(ActionAdminHealth, "action.result")
	if len(res) == 0 || strings.Contains(strings.Join(res, "\n"), " INFO ") {
		t.Errorf("health results should be DEBUG records: %v", res)
	}
}
