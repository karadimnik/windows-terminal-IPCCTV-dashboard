package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const viewerUser, viewerPassword = "bob", "a viewer's long passphrase"

// wantAccess is the decision, written down independently of routeAccess: who may reach each route
// of roRoutes (keyed "METHOD path" without the query). A route of roRoutes that is missing here fails.
var wantAccess = map[string]access{
	"GET /login": accessPublic, "GET /login.js": accessPublic, "GET /style.css": accessPublic,
	"POST /api/auth/login": accessPublic, "GET /api/auth/me": accessPublic,

	"GET /": accessViewer, "HEAD /": accessViewer, "GET /index.html": accessViewer, "GET /app.js": accessViewer, "GET /live.js": accessViewer,
	"GET /nothing-static": accessViewer, "GET /api/status": accessViewer, "GET /api/live": accessViewer, "GET /api/footage": accessViewer,
	"GET /api/clip": accessViewer, "GET /api/tls/ca.crt": accessViewer, "GET /api/tls/info": accessViewer, "POST /api/auth/logout": accessViewer,
	"POST /api/live/Cam%201/whep": accessViewer, "DELETE /api/live/session/00000000000000000000000000000000": accessViewer,

	"GET /admin.js": accessAdmin, "GET /api/cameras": accessAdmin, "GET /api/config": accessAdmin,
	"POST /api/control": accessAdmin, "PUT /api/config": accessAdmin, "PUT /api/cameras": accessAdmin,
	"POST /api/cameras/Cam%201/record": accessAdmin, "POST /api/acknowledge": accessAdmin,

	// not registered on the LAN listener at all: a session is needed, then the mux answers 404 or 405
	"GET /setup": accessViewer, "GET /setup.js": accessViewer, "POST /api/auth/setup": accessViewer,
	"GET /api/nothing": accessViewer, "GET /api/live/x": accessViewer, "GET /api/tls/ca.key": accessViewer, "POST /": accessViewer,
	"PUT /": accessViewer, "PATCH /api/config": accessViewer, "DELETE /api/cameras": accessViewer, "DELETE /api/live/session/a/b": accessViewer,
	"POST /api/live/whep": accessViewer, "POST /api/live/Cam%201/whep/x": accessViewer, "OPTIONS /api/status": accessViewer,
	"POST /api/tls/info": accessViewer, "GET /api/control": accessViewer,
}

// adminWantAccess are the Admin tab's routes (every one admin-only).
var adminWantAccess = map[string]access{
	"GET /api/admin/config":          accessAdmin,
	"POST /api/admin/config/set":     accessAdmin,
	"POST /api/admin/config/unset":   accessAdmin,
	"GET /api/admin/users":           accessAdmin,
	"POST /api/admin/users/add":      accessAdmin,
	"POST /api/admin/users/password": accessAdmin,
	"POST /api/admin/users/role":     accessAdmin,
	"POST /api/admin/users/remove":   accessAdmin,
	"POST /api/admin/users/unlock":   accessAdmin,
	"GET /api/admin/camera-login":    accessAdmin,
	"POST /api/admin/camera-login":   accessAdmin,
	"GET /api/admin/tls":             accessAdmin,
	"GET /api/admin/tls/ca.crt":      accessAdmin,
	"POST /api/admin/tls/rotate":     accessAdmin,
	"GET /api/admin/prune":           accessAdmin,
	"GET /api/admin/archive":         accessAdmin,
	"GET /api/admin/log":             accessAdmin,
	"POST /api/admin/log/level":      accessAdmin,
	"GET /api/admin/autostart":       accessAdmin,
	"POST /api/admin/autostart":      accessAdmin,
}

func init() {
	for k, v := range adminWantAccess {
		wantAccess[k] = v
	}
}

func isAdminRefusal(rec *httptest.ResponseRecorder) bool {
	var body map[string]any
	return rec.Code == http.StatusForbidden && json.Unmarshal(rec.Body.Bytes(), &body) == nil && body["error"] == adminOnlyMessage
}

// addViewer puts bob (viewer) into the rig's user file.
func (rig *tlsRig) addViewer(t *testing.T) {
	t.Helper()
	hash, _ := fastHasher().Hash(viewerPassword)
	if err := rig.users.Put(User{ID: "u-bob", Username: viewerUser, Hash: hash, Role: roleViewer, CreatedAt: rig.clock.now()}); err != nil {
		t.Fatal(err)
	}
}

func (rig *tlsRig) loginAs(t *testing.T, user, password string) string {
	t.Helper()
	c := sessionCookieOf(rig.loginFrom("192.168.200.1:1", user, password))
	if c == nil {
		t.Fatalf("%s could not sign in", user)
	}
	rig.clock.advance(7 * time.Second) // the address throttle refills
	return c.Value
}

// harmless turns a state-changing probe into one the guard refuses after the session gate (415:
// not JSON), so passing the role check is visible without running the handler.
func harmless(q tlsReq) tlsReq {
	if changesState(q.method) && q.body != "" {
		q.headers = map[string]string{"Content-Type": "text/plain"}
	}
	return q
}

func TestRoleMatrixOverEveryRoute(t *testing.T) {
	logs := newWebLogs("debug")
	open := newTLSRigLogx(t, logs, func(c *Config) { c.TLSReadOnly = boolPtr(false) })
	open.addViewer(t)
	viewOnly := newTLSRigLogx(t, newWebLogs("info"), nil)
	viewOnly.addViewer(t)
	lb, _ := loopbackHandler(t, nil)
	const token = "0123456789abcdef-token"
	tok, _ := loopbackHandler(t, func(c *Config) { c.Token = token })

	adminCookie, viewerCookie := open.loginAs(t, testUser, testPassword), open.loginAs(t, viewerUser, viewerPassword)
	roAdmin, roViewer := viewOnly.loginAs(t, testUser, testPassword), viewOnly.loginAs(t, viewerUser, viewerPassword)
	denials := 0
	for _, route := range roRoutes {
		pathOnly, _, _ := strings.Cut(route.path, "?")
		key := route.method + " " + pathOnly
		want, ok := wantAccess[key]
		if !ok {
			t.Errorf("%s: no expected access class in wantAccess", key)
			continue
		}
		base := harmless(tlsReq{method: route.method, path: route.path, headers: route.headers, body: route.body})
		name := key
		// LAN, no session: 401 unless public
		if rec := open.do(base); want != accessPublic && !isAuthRequired(rec, route.method) {
			t.Errorf("[LAN no session] %s: %d, want 401", name, rec.Code)
		}
		// LAN viewer
		q := base
		q.cookie = viewerCookie
		rec := open.do(q)
		switch {
		case want == accessAdmin && !isAdminRefusal(rec):
			t.Errorf("[LAN viewer] %s: %d %q, want the admin-only 403", name, rec.Code, rec.Body.String())
		case want == accessAdmin:
			denials++
		case want != accessAdmin && (isAdminRefusal(rec) || rec.Code == 401):
			t.Errorf("[LAN viewer] %s: %d, a viewer may reach it", name, rec.Code)
		}
		// LAN admin
		q.cookie = adminCookie
		if rec := open.do(q); isAdminRefusal(rec) || rec.Code == 401 {
			t.Errorf("[LAN admin] %s: %d", name, rec.Code)
		}
		// view-only listener: the role check comes first, then view-only refuses every change
		q.cookie = roViewer
		if rec := viewOnly.do(q); want == accessAdmin && !isAdminRefusal(rec) {
			t.Errorf("[view-only viewer] %s: %d, want the admin-only 403", name, rec.Code)
		}
		q = tlsReq{method: route.method, path: route.path, headers: route.headers, body: route.body, cookie: roAdmin}
		if pathOnly != "/api/auth/logout" {
			rec := viewOnly.do(q)
			if refused := rec.Code == 403 && isReadOnlyRefusal(rec.Body.String()); refused == route.allowed || isAdminRefusal(rec) {
				t.Errorf("[view-only admin] %s: %d %q (view-only allows it: %v)", name, rec.Code, rec.Body.String(), route.allowed)
			}
		}
		// loopback: the box owner, an admin; no login, so never 401 or the admin-only 403
		lq := probe{method: base.method, path: base.path, headers: base.headers, body: base.body}
		if rec := lq.do(lb); rec.Code == 401 || isAdminRefusal(rec) {
			t.Errorf("[loopback] %s: %d", name, rec.Code)
		}
		// the legacy token mode (camui.exe): the token, then everything as an admin
		if rec := lq.do(tok); rec.Code != 401 || !strings.Contains(rec.Body.String(), "token is required") {
			t.Errorf("[token, none given] %s: %d", name, rec.Code)
		}
		lq.headers = merge(lq.headers, "Authorization", "Bearer "+token)
		if rec := lq.do(tok); rec.Code == 401 || isAdminRefusal(rec) {
			t.Errorf("[token] %s: %d", name, rec.Code)
		}
	}
	// every refusal of the viewer is in the audit stream
	var audited []string
	for _, l := range logs.audit.Lines() {
		if strings.Contains(l, "access.denied") && strings.Contains(l, "result=denied") && strings.Contains(l, "actor=web:"+viewerUser) && strings.Contains(l, "role=viewer") {
			audited = append(audited, l)
		}
	}
	if denials < 8 || len(audited) != denials {
		t.Errorf("%d admin routes refused to the viewer, %d audit records:\n%s", denials, len(audited), strings.Join(audited, "\n"))
	}
	t.Logf("%d routes x 7 modes; %d admin-only refusals audited", len(roRoutes), denials)
}

// Path and method tricks are judged as the route the mux ends up running.
func TestRoleGateSeesThroughPathTricks(t *testing.T) {
	rig := newTLSRigLogx(t, newWebLogs("info"), func(c *Config) { c.TLSReadOnly = boolPtr(false) })
	rig.addViewer(t)
	viewer, admin := rig.loginAs(t, viewerUser, viewerPassword), rig.loginAs(t, testUser, testPassword)
	for _, p := range []string{
		"/api/admin/../config", "//api/config", "/api/./config", "/api//config", "/api/x/../cameras", "/api/%63onfig",
		"/api/config/", "/API/CONFIG", "/Api/Config", "/api/config%2F", "/api/config/.", "/admin.js/", "/./admin.js", "/static/../admin.js",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			rec := rig.do(tlsReq{method: method, path: p, cookie: viewer})
			switch {
			case isAdminRefusal(rec), method == "HEAD" && rec.Code == 403:
			case rec.Code == 404:
			case rec.Code == 301: // the file server's own canonical redirect; where it leads is refused too
				u, _ := url.Parse("https://" + lanHost + p)
				next, _ := u.Parse(rec.Header().Get("Location"))
				if again := rig.do(tlsReq{method: method, path: next.RequestURI(), cookie: viewer}); again.Code != 403 {
					t.Errorf("viewer %s %s -> %s: %d", method, p, next.RequestURI(), again.Code)
				}
			default:
				t.Errorf("viewer %s %s: %d %q, want the admin-only 403 or a 404", method, p, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "SegmentSeconds") || strings.Contains(rec.Body.String(), "192.0.2.10") {
				t.Errorf("viewer %s %s: settings in the answer", method, p)
			}
			if rec := rig.do(tlsReq{method: method, path: p}); !isAuthRequired(rec, method) {
				t.Errorf("no session %s %s: %d", method, p, rec.Code)
			}
		}
		// an admin is sent on or answered, never refused for the role
		if rec := rig.do(tlsReq{path: p, cookie: admin}); isAdminRefusal(rec) || rec.Code == 401 {
			t.Errorf("admin GET %s: %d", p, rec.Code)
		}
	}
	// method override headers change nothing: the method on the wire decides
	for _, h := range []string{"X-HTTP-Method-Override", "X-Method-Override", "X-HTTP-Method"} {
		if rec := rig.do(tlsReq{method: "POST", path: "/api/control", headers: map[string]string{"Content-Type": "application/json", h: "GET"}, body: `{"scope":"all","action":"stop"}`, cookie: viewer}); !isAdminRefusal(rec) {
			t.Errorf("viewer POST /api/control with %s: GET: %d", h, rec.Code)
		}
		if rec := rig.do(tlsReq{path: "/api/cameras", headers: map[string]string{h: "OPTIONS"}, cookie: viewer}); !isAdminRefusal(rec) {
			t.Errorf("viewer GET /api/cameras with %s: %d", h, rec.Code)
		}
		if rec := rig.do(tlsReq{path: "/api/status", headers: map[string]string{h: "PUT"}, cookie: viewer}); rec.Code != 200 {
			t.Errorf("viewer GET /api/status with %s: %d", h, rec.Code)
		}
	}
	// OPTIONS and other methods nobody registered: a session first, then the mux's 405, no handler
	for _, m := range []string{"OPTIONS", "TRACE", "PROPFIND"} {
		if rec := rig.do(tlsReq{method: m, path: "/api/config", cookie: viewer}); rec.Code != 405 && !isAdminRefusal(rec) {
			t.Errorf("viewer %s /api/config: %d", m, rec.Code)
		}
		if rec := rig.do(tlsReq{method: m, path: "/api/config"}); rec.Code != 401 {
			t.Errorf("no session %s /api/config: %d", m, rec.Code)
		}
	}
}

// The role is read from the user file on every request: a demotion applies to a session that is
// still open at its very next request.
func TestDemotionTakesEffectOnTheNextRequest(t *testing.T) {
	rig := newTLSRigLogx(t, newWebLogs("info"), func(c *Config) { c.TLSReadOnly = boolPtr(false) })
	rig.addViewer(t) // a second user, so anna is not the only one
	hash, _ := fastHasher().Hash(testPassword)
	rig.users.Put(User{ID: "u-carol", Username: "carol", Hash: hash, Role: roleAdmin, CreatedAt: rig.clock.now()})
	cookie := rig.loginAs(t, testUser, testPassword)
	if rec := rig.do(tlsReq{path: "/api/config", cookie: cookie}); rec.Code != 200 {
		t.Fatalf("admin: %d", rec.Code)
	}
	// another process (holocron user role) demotes anna but spares this session: it now acts as a viewer
	other := usersWith(rig.cfg.AuthDir, nil, fastHasher())
	if _, err := other.sparing(cookie).SetRole("cli:test", testUser, "viewer"); err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(tlsReq{path: "/api/config", cookie: cookie}); !isAdminRefusal(rec) {
		t.Errorf("after the demotion: %d", rec.Code)
	}
	if rec := rig.do(tlsReq{path: "/api/status", cookie: cookie}); rec.Code != 200 {
		t.Errorf("a viewer's route after the demotion: %d", rec.Code)
	}
	var me map[string]any
	json.Unmarshal(rig.do(tlsReq{path: "/api/auth/me", cookie: cookie}).Body.Bytes(), &me)
	if me["role"] != roleViewer {
		t.Errorf("me after the demotion: %v", me)
	}
	// the command line's own path (nothing spared): the session ends
	if _, err := other.SetRole("cli:test", testUser, "admin"); err != nil {
		t.Fatal(err)
	}
	if rec := rig.do(tlsReq{path: "/api/status", cookie: cookie}); rec.Code != 401 {
		t.Errorf("after a role change from the command line: %d, want the session ended", rec.Code)
	}
}

// A pre-roles users.json signs in as an admin and reaches the admin routes.
func TestPreRolesUserSignsInAsAdmin(t *testing.T) {
	rig := newTLSRigLogx(t, newWebLogs("info"), func(c *Config) { c.TLSReadOnly = boolPtr(false) })
	hash, _ := fastHasher().Hash(testPassword)
	doc := `{"version":1,"users":[{"id":"u-old","username":"olduser","hash":"` + hash + `","createdAt":"2026-10-01T10:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(rig.cfg.AuthDir, usersFileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := rig.loginFrom("192.168.201.1:1", "olduser", testPassword)
	if rec.Code != 200 || decode(t, rec)["role"] != roleAdmin {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	if r := rig.do(tlsReq{path: "/api/config", cookie: sessionCookieOf(rec).Value}); r.Code != 200 {
		t.Errorf("admin route: %d", r.Code)
	}
}

// Every registered pattern has an access class, every class names a registered pattern, and an
// unclassified pattern is refused at run time.
func TestEveryRouteHasAnAccessClass(t *testing.T) {
	registered := map[string]bool{}
	consts := map[string]string{}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, _ := os.ReadFile(f)
		for _, m := range actionConst.FindAllStringSubmatch(string(data), -1) {
			consts[m[1]] = m[2]
		}
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, _ := os.ReadFile(f)
		plain, acted := scanRoutes(string(data), consts)
		for _, p := range plain {
			registered[p] = true
		}
		for p := range acted {
			registered[p] = true
		}
	}
	if len(registered) < 25 {
		t.Fatalf("found only %d routes: the scan is broken", len(registered))
	}
	for p := range registered {
		if routeAccess[p] == accessUnclassified {
			t.Errorf("route %q has no access class: decide public, viewer or admin and add it to routeAccess (access.go)", p)
		}
	}
	for p := range routeAccess {
		if !registered[p] {
			t.Errorf("routeAccess names %q, which no file registers", p)
		}
	}
	// positive control: the same check catches a new route
	plain, _ := scanRoutes(`mux.HandleFunc("GET /api/brand-new", a.h)`, consts)
	if len(plain) != 1 || routeAccess[plain[0]] != accessUnclassified {
		t.Error("the positive control is broken")
	}
	// at run time an unclassified pattern is refused even to an admin, after the session check
	rig := newTLSRig(t, func(c *Config) { c.TLSReadOnly = boolPtr(false) })
	cookie := rig.login(t)
	mux := http.NewServeMux()
	reached := false
	mux.HandleFunc("GET /api/brand-new", func(w http.ResponseWriter, r *http.Request) { reached = true })
	h := rig.auth.gate(mux, mux)
	req := httptest.NewRequest("GET", "https://"+lanHost+"/api/brand-new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 || reached {
		t.Errorf("an unclassified route: %d reached=%v", rec.Code, reached)
	}
	req = httptest.NewRequest("GET", "https://"+lanHost+"/api/brand-new", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 || reached {
		t.Errorf("an unclassified route without a session: %d", rec.Code)
	}
	if !strings.Contains(rig.logs.String(), "route has no access class") {
		t.Error("the refusal was not logged")
	}
}
