package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const lanToken = "0123456789abcdef-lan-token"

// guardHandler builds the real handler chain, as run() does, over an empty
// temporary script directory so no request can reach a real file.
func guardHandler(t *testing.T, mutate func(*Config)) (http.Handler, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		Listen:      "127.0.0.1:8787",
		ArchiveRoot: dir,
		StatusFile:  dir + "/_state/status.json",
		ScriptDir:   dir,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	logs := &bytes.Buffer{}
	handler, err := newHandler(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	return handler, logs
}

type probe struct {
	method  string
	path    string
	host    string
	headers map[string]string
	body    string

	emptyHost bool
}

func (p probe) do(h http.Handler) *httptest.ResponseRecorder {
	method := p.method
	if method == "" {
		method = http.MethodGet
	}
	path := p.path
	if path == "" {
		path = "/api/cameras"
	}
	host := p.host
	if host == "" {
		host = "127.0.0.1:8787"
	}
	var body io.Reader = http.NoBody
	if p.body != "" {
		body = strings.NewReader(p.body)
	}
	req := httptest.NewRequest(method, "http://"+host+path, body)
	req.Host = host
	if p.emptyHost {
		req.Host = ""
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// reached reports whether a response came from a handler rather than from the
// guard. A handler's own 4xx or 5xx is irrelevant here; only 403 and 415 are
// the guard's.
func reached(code int) bool {
	return code != http.StatusForbidden && code != http.StatusUnsupportedMediaType
}

func TestGuardHost(t *testing.T) {
	cases := []struct {
		host  string
		empty bool
		allow bool
	}{
		{host: "localhost", allow: true},
		{host: "localhost:8787", allow: true},
		{host: "LocalHost:8787", allow: true},
		{host: "127.0.0.1:8787", allow: true},
		{host: "127.0.0.1", allow: true},
		{host: "[::1]:8787", allow: true},
		{host: "attacker.example"},
		{host: "attacker-rebind.example:8787"},
		{host: "127.0.0.1.attacker.example"},
		{host: "localhost.evil.example"},
		{host: "localhost.evil.example:8787"},
		{host: "127.0.0.2:8787"},
		{host: "", empty: true},
	}
	for _, c := range cases {
		for _, path := range []string{"/", "/api/cameras"} {
			handler, _ := guardHandler(t, nil)
			code := probe{path: path, host: c.host, emptyHost: c.empty}.do(handler).Code
			if c.allow && code == http.StatusForbidden {
				t.Errorf("Host %q on %s: refused (403), want allowed", c.host, path)
			}
			if !c.allow && code != http.StatusForbidden {
				t.Errorf("Host %q on %s: got %d, want 403", c.host, path, code)
			}
		}
	}
}

func TestGuardHostRefusalBodyIsShortAndPlain(t *testing.T) {
	handler, _ := guardHandler(t, nil)
	rec := probe{path: "/api/status", host: "attacker-rebind.example"}.do(handler)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	if body := rec.Body.String(); len(body) > 80 || strings.Contains(body, "{") || strings.Contains(body, "<") {
		t.Errorf("refusal body is not a short plain message: %q", body)
	}
}

func TestGuardStaticPageOnlyChecksHost(t *testing.T) {
	handler, _ := guardHandler(t, nil)
	rec := probe{path: "/", headers: map[string]string{
		"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site",
	}}.do(handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with foreign Origin and cross-site: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("GET / did not serve the page")
	}
	if got := rec.Header().Get("Cross-Origin-Resource-Policy"); got != "" {
		t.Errorf("static page carries API-only header Cross-Origin-Resource-Policy=%q", got)
	}
}

func TestGuardOrigin(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		origin string // "-" means absent
		allow  bool
	}{
		{"absent", "127.0.0.1:8787", "-", true},
		{"same origin ip", "127.0.0.1:8787", "http://127.0.0.1:8787", true},
		{"same origin localhost", "localhost:8787", "http://localhost:8787", true},
		{"same origin ipv6", "[::1]:8787", "http://[::1]:8787", true},
		{"foreign", "127.0.0.1:8787", "http://evil.example", false},
		{"foreign with port", "127.0.0.1:8787", "http://evil.example:8787", false},
		{"other loopback name", "127.0.0.1:8787", "http://localhost:8787", false},
		{"other port", "127.0.0.1:8787", "http://127.0.0.1:9999", false},
		{"null", "127.0.0.1:8787", "null", false},
		{"unparseable", "127.0.0.1:8787", "http://[bad", false},
	}
	for _, c := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			handler, _ := guardHandler(t, nil)
			headers := map[string]string{}
			if c.origin != "-" {
				headers["Origin"] = c.origin
			}
			p := probe{method: method, host: c.host, headers: headers}
			if method == http.MethodPost {
				p.path = "/api/acknowledge"
				p.body = `{}`
				headers["Content-Type"] = "application/json"
			}
			code := p.do(handler).Code
			if c.allow && !reached(code) {
				t.Errorf("%s %s: refused with %d, want allowed", method, c.name, code)
			}
			if !c.allow && code != http.StatusForbidden {
				t.Errorf("%s %s: got %d, want 403", method, c.name, code)
			}
		}
	}
}

func TestGuardSecFetchSite(t *testing.T) {
	cases := []struct {
		value string // "-" means absent
		allow bool
	}{
		{"-", true},
		{"same-origin", true},
		{"none", true},
		{"cross-site", false},
		{"same-site", false},
		{"bogus", false},
	}
	for _, c := range cases {
		handler, _ := guardHandler(t, nil)
		headers := map[string]string{}
		if c.value != "-" {
			headers["Sec-Fetch-Site"] = c.value
		}
		code := probe{headers: headers}.do(handler).Code
		if c.allow && !reached(code) {
			t.Errorf("Sec-Fetch-Site %q: refused with %d, want allowed", c.value, code)
		}
		if !c.allow && code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %q: got %d, want 403", c.value, code)
		}
	}
}

func TestGuardContentType(t *testing.T) {
	cases := []struct {
		ct    string // "-" means no header
		allow bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"Application/JSON", true},
		{"text/plain", false},
		{"text/plain;charset=UTF-8", false},
		{"application/x-www-form-urlencoded", false},
		{"multipart/form-data; boundary=x", false},
		{"application/jsonx", false},
		{"application/json/extra", false},
		{";;;", false},
		{"-", false},
	}
	paths := map[string]string{
		http.MethodPost:   "/api/acknowledge",
		http.MethodPut:    "/api/config",
		http.MethodPatch:  "/api/config",
		http.MethodDelete: "/api/config",
	}
	for method, path := range paths {
		for _, c := range cases {
			handler, _ := guardHandler(t, nil)
			headers := map[string]string{}
			if c.ct != "-" {
				headers["Content-Type"] = c.ct
			}
			code := probe{method: method, path: path, headers: headers, body: `{"a":1}`}.do(handler).Code
			if c.allow && !reached(code) {
				t.Errorf("%s Content-Type %q: refused with %d, want allowed", method, c.ct, code)
			}
			if !c.allow && code != http.StatusUnsupportedMediaType {
				t.Errorf("%s Content-Type %q: got %d, want 415", method, c.ct, code)
			}
		}
	}
}

func TestGuardContentTypeIgnoredWithoutBodyAndOnGet(t *testing.T) {
	handler, _ := guardHandler(t, nil)
	// A GET never carries a meaningful body, whatever type it claims.
	rec := probe{headers: map[string]string{"Content-Type": "text/plain"}}.do(handler)
	if !reached(rec.Code) {
		t.Errorf("GET with text/plain: refused with %d", rec.Code)
	}
	// A state-changing request with no body at all has nothing to sniff.
	rec = probe{method: http.MethodPost, path: "/api/control"}.do(handler)
	if !reached(rec.Code) {
		t.Errorf("bodiless POST: refused with %d", rec.Code)
	}
}

func TestGuardPageShapedRequestIsAccepted(t *testing.T) {
	handler, _ := guardHandler(t, nil)
	rec := probe{
		method: http.MethodPost, path: "/api/acknowledge", host: "127.0.0.1:8787",
		body: `{"notice":"nope","at":"x"}`,
		headers: map[string]string{
			"Origin":         "http://127.0.0.1:8787",
			"Sec-Fetch-Site": "same-origin",
			"Content-Type":   "application/json",
		},
	}.do(handler)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want the handler's own 422 for an unknown notice", rec.Code)
	}
}

func TestGuardResponseHeaders(t *testing.T) {
	handler, _ := guardHandler(t, nil)
	for _, p := range []probe{
		{path: "/api/cameras"},
		{path: "/api/cameras", host: "attacker.example"},
		{method: http.MethodPost, path: "/api/control", headers: map[string]string{"Content-Type": "text/plain"}, body: "x"},
	} {
		rec := p.do(handler)
		h := rec.Header()
		if got := h.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
			t.Errorf("%s %s (%d): Cross-Origin-Resource-Policy=%q", p.method, p.host, rec.Code, got)
		}
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s %s (%d): X-Content-Type-Options=%q", p.method, p.host, rec.Code, got)
		}
		if got := h.Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("%s %s (%d): Cache-Control=%q", p.method, p.host, rec.Code, got)
		}
	}
}

func TestGuardLeavesHandlerCacheHeaderAlone(t *testing.T) {
	guarded := newGuard(Config{Listen: "127.0.0.1:8787"}, slog.New(slog.NewTextHandler(io.Discard, nil)))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "private, max-age=60")
		}))
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/clip", nil)
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "private, max-age=60" {
		t.Errorf("Cache-Control=%v, want the handler's own value only", got)
	}
}

func lanConfig(c *Config) {
	c.Listen = "192.168.50.20:8787"
	c.Token = lanToken
	c.AllowedHosts = []string{"nvr.lan", "Cam.Example:8443"}
}

func TestGuardLANHosts(t *testing.T) {
	cases := []struct {
		host  string
		allow bool
	}{
		{"192.168.50.20:8787", true},
		{"192.168.50.20", true},
		{"nvr.lan:8787", true},
		{"NVR.LAN", true},
		{"cam.example", true},
		{"localhost:8787", true},
		{"192.168.50.21:8787", false},
		{"nvr.lan.evil.example", false},
		{"evil.example", false},
	}
	for _, c := range cases {
		handler, _ := guardHandler(t, lanConfig)
		code := probe{host: c.host, headers: map[string]string{"Authorization": "Bearer " + lanToken}}.do(handler).Code
		if c.allow && !reached(code) {
			t.Errorf("LAN Host %q: refused with %d, want allowed", c.host, code)
		}
		if !c.allow && code != http.StatusForbidden {
			t.Errorf("LAN Host %q: got %d, want 403", c.host, code)
		}
	}
}

func TestGuardLANWildcardBindAdmitsIPLiteralsOnly(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8787", ":8787"} {
		for host, allow := range map[string]bool{
			"192.168.50.20:8787": true,
			"[fe80::1]:8787":     true,
			"nvr.lan":            false,
			"evil.example":       false,
		} {
			handler, _ := guardHandler(t, func(c *Config) { c.Listen = listen; c.Token = lanToken })
			code := probe{host: host, headers: map[string]string{"Authorization": "Bearer " + lanToken}}.do(handler).Code
			if allow && !reached(code) || !allow && code != http.StatusForbidden {
				t.Errorf("listen %s, Host %q: got %d, want allowed=%v", listen, host, code, allow)
			}
		}
	}
	// Loopback mode never admits arbitrary IP literals.
	handler, _ := guardHandler(t, nil)
	if code := (probe{host: "192.168.50.20:8787"}).do(handler).Code; code != http.StatusForbidden {
		t.Errorf("loopback mode, LAN IP Host: got %d, want 403", code)
	}
}

func TestGuardLoopbackModeDoesNotAllowAnArbitraryListenHost(t *testing.T) {
	handler, _ := guardHandler(t, nil)
	if code := (probe{host: "192.168.50.20:8787"}).do(handler).Code; code != http.StatusForbidden {
		t.Errorf("non-loopback Host in loopback mode: got %d, want 403", code)
	}
}

func TestGuardTokenFlowUnchanged(t *testing.T) {
	handler, _ := guardHandler(t, lanConfig)
	host := "192.168.50.20:8787"

	if code := (probe{host: host}).do(handler).Code; code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", code)
	}
	if code := (probe{host: host, headers: map[string]string{"Authorization": "Bearer wrong-wrong-wrong-wrong"}}).do(handler).Code; code != http.StatusUnauthorized {
		t.Errorf("wrong bearer: got %d, want 401", code)
	}
	if code := (probe{host: host, headers: map[string]string{"Authorization": "Bearer " + lanToken}}).do(handler).Code; !reached(code) {
		t.Errorf("right bearer: got %d", code)
	}

	boot := probe{path: "/?token=" + lanToken, host: host}.do(handler)
	if boot.Code != http.StatusSeeOther || boot.Header().Get("Location") != "/" {
		t.Fatalf("bootstrap: got %d Location=%q, want 303 to /", boot.Code, boot.Header().Get("Location"))
	}
	cookies := boot.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != tokenCookie || cookies[0].Value != lanToken {
		t.Fatalf("bootstrap cookie: %v", cookies)
	}

	withCookie := probe{path: "/", host: host, headers: map[string]string{"Cookie": cookies[0].String()}}.do(handler)
	if withCookie.Code != http.StatusOK {
		t.Errorf("cookie request: got %d, want 200", withCookie.Code)
	}
}

func TestGuardRefusalIsLoggedWithoutSecrets(t *testing.T) {
	handler, logs := guardHandler(t, nil)
	rec := probe{
		method: http.MethodPost, path: "/api/control", body: `{"scope":"all","action":"stop","secret":"BODYSECRET"}`,
		headers: map[string]string{
			"Origin":         "http://evil.example",
			"Sec-Fetch-Site": "cross-site",
			"Content-Type":   "text/plain",
			"Authorization":  "Bearer HEADERSECRET",
			"Cookie":         camuiCookieForTest(),
		},
	}.do(handler)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	out := logs.String()
	if !strings.Contains(out, "POST") || !strings.Contains(out, "/api/control") {
		t.Errorf("refusal log lacks method and path:\n%s", out)
	}
	if n := strings.Count(out, "refused"); n != 1 {
		t.Errorf("refusal logged %d times, want once:\n%s", n, out)
	}
	for _, secret := range []string{"BODYSECRET", "HEADERSECRET", "evil.example", "COOKIESECRET"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
}

func camuiCookieForTest() string { return tokenCookie + "=COOKIESECRET" }
