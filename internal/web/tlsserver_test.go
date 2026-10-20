package web

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	lanIP        = "192.168.1.50"
	lanListen    = "192.168.1.50:8443"
	lanHost      = "192.168.1.50:8443"
	testUser     = "anna"
	testPassword = "correct horse battery staple"
)

type tlsRig struct {
	handler http.Handler
	logs    *bytes.Buffer
	mgr     *certManager
	cfg     Config
	auth    *authService
	clock   *fakeClock
	users   *fileUserStore
}

// newTLSRig builds the LAN listener's real handler chain over a throwaway auth
// folder holding one user (anna), with a cheap hasher and an injected clock.
func newTLSRig(t *testing.T, mutate func(*Config), opts ...serveOpt) *tlsRig {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{Listen: "127.0.0.1:8787", ArchiveRoot: dir, StatusFile: dir + "/_state/status.json", ScriptDir: dir,
		TLSListen: lanListen, TLSDir: dir + "/tls", AuthDir: dir + "/auth"}
	if mutate != nil {
		mutate(&cfg)
	}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
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
	auth := newAuthService(cfg.AuthDir, users, log, clock.now, hasher)
	h, err := newTLSHandler(cfg, log, mgr, names, auth, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &tlsRig{handler: h, logs: logs, mgr: mgr, cfg: cfg, auth: auth, clock: clock, users: users}
}

var tlsReqCounter int

type tlsReq struct {
	method, path string
	host         string
	remote       string
	headers      map[string]string
	cookie       string // value of the session cookie
	body         string
}

func (rig *tlsRig) do(q tlsReq) *httptest.ResponseRecorder {
	if q.method == "" {
		q.method = http.MethodGet
	}
	if q.host == "" {
		q.host = lanHost
	}
	if q.remote == "" {
		tlsReqCounter++
		q.remote = fmt.Sprintf("192.168.%d.%d:50000", (tlsReqCounter/250)%250+2, tlsReqCounter%250+2)
	}
	var body io.Reader = http.NoBody
	if q.body != "" {
		body = strings.NewReader(q.body)
	}
	req := httptest.NewRequest(q.method, "https://"+q.host+q.path, body)
	req.Host = q.host
	req.RemoteAddr = q.remote
	for k, v := range q.headers {
		req.Header.Set(k, v)
	}
	if q.cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: q.cookie})
	}
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	return rec
}

// login signs testUser in from a fresh address and returns the session cookie value.
func (rig *tlsRig) login(t *testing.T) string {
	t.Helper()
	rec := rig.do(tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, body: loginBody(testUser, testPassword)})
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("login set no session cookie")
	return ""
}

func loginBody(user, password string) string {
	b, _ := json.Marshal(map[string]string{"username": user, "password": password})
	return string(b)
}

// every route the LAN listener serves, plus unknown ones: all must be behind the token.
var tlsRoutes = []tlsReq{
	{method: "GET", path: "/"}, {method: "GET", path: "/index.html"}, {method: "GET", path: "/app.js"}, {method: "GET", path: "/live.js"},
	{method: "GET", path: "/style.css"}, {method: "GET", path: "/api/status"}, {method: "GET", path: "/api/cameras"},
	{method: "PUT", path: "/api/cameras", headers: map[string]string{"Content-Type": "application/json"}, body: "{}"},
	{method: "GET", path: "/api/config"}, {method: "PUT", path: "/api/config", headers: map[string]string{"Content-Type": "application/json"}, body: "{}"},
	{method: "POST", path: "/api/control", headers: map[string]string{"Content-Type": "application/json"}, body: "{}"},
	{method: "POST", path: "/api/acknowledge", headers: map[string]string{"Content-Type": "application/json"}, body: "{}"},
	{method: "POST", path: "/api/cameras/Cam%201/record", headers: map[string]string{"Content-Type": "application/json"}, body: "{}"},
	{method: "GET", path: "/api/footage"}, {method: "GET", path: "/api/clip?camera=x&segment=y"},
	{method: "GET", path: "/api/live"},
	{method: "POST", path: "/api/live/Cam%201/whep?stream=sub", headers: map[string]string{"Content-Type": "application/sdp"}, body: "v=0\r\n"},
	{method: "DELETE", path: "/api/live/session/00000000000000000000000000000000"},
	{method: "GET", path: "/api/tls/ca.crt"}, {method: "GET", path: "/api/tls/info"},
	{method: "GET", path: "/api/nothing"}, {method: "GET", path: "/nothing"}, {method: "POST", path: "/nothing", headers: map[string]string{"Content-Type": "application/json"}, body: "{}"},
}

func merge(h map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for key, value := range h {
		out[key] = value
	}
	out[k] = v
	return out
}

func TestTLSGuard(t *testing.T) {
	rig := newTLSRig(t, nil)
	ok := rig.login(t)
	cases := []struct {
		name    string
		host    string
		headers map[string]string
		want    int // 0 means "not a guard refusal"
	}{
		{"own https origin", "", map[string]string{"Origin": "https://" + lanHost, "Sec-Fetch-Site": "same-origin"}, 0},
		{"foreign https origin", "", map[string]string{"Origin": "https://evil.example"}, 403},
		{"http origin against the https listener", "", map[string]string{"Origin": "http://" + lanHost}, 403},
		{"null origin", "", map[string]string{"Origin": "null"}, 403},
		{"cross-site fetch", "", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site fetch", "", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"rebinding host", "attacker-rebind.example:8443", nil, 403},
		{"another LAN address as Host", "192.168.1.51:8443", nil, 403},
		{"localhost host (this PC)", "localhost:8443", nil, 0},
	}
	for _, c := range cases {
		headers := merge(c.headers, "Content-Type", "application/sdp")
		rec := rig.do(tlsReq{method: "POST", path: "/api/live/Cam%201/whep?stream=sub", host: c.host, headers: headers, cookie: ok, body: "v=0\r\n"})
		if c.want == 403 && rec.Code != 403 {
			t.Errorf("%s: got %d, want 403", c.name, rec.Code)
		}
		if c.want == 0 && (rec.Code == 403 || rec.Code == 415 || rec.Code == 401) {
			t.Errorf("%s: refused with %d", c.name, rec.Code)
		}
	}
	// The WHEP route's single exception holds here too, and only here.
	writable := newTLSRig(t, func(c *Config) { off := false; c.TLSReadOnly = &off })
	if rec := writable.do(tlsReq{method: "POST", path: "/api/acknowledge", headers: map[string]string{"Content-Type": "application/sdp"}, cookie: writable.login(t), body: "x"}); rec.Code != 415 {
		t.Errorf("application/sdp on another route: %d", rec.Code)
	}
}

func TestLoopbackHandlerIsUnchanged(t *testing.T) {
	handler, _ := guardHandler(t, nil)
	if rec := (probe{path: "/api/tls/ca.crt"}).do(handler); rec.Code != http.StatusNotFound {
		t.Errorf("/api/tls/ca.crt on the loopback listener: %d, want 404", rec.Code)
	}
	if rec := (probe{path: "/api/tls/info"}).do(handler); rec.Code != http.StatusNotFound {
		t.Errorf("/api/tls/info on the loopback listener: %d, want 404", rec.Code)
	}
	for _, host := range []string{lanHost, lanIP, "nvr.lan:8443"} {
		if rec := (probe{path: "/api/status", host: host}).do(handler); rec.Code != http.StatusForbidden {
			t.Errorf("Host %s on the loopback listener: %d, want 403 (the allowlist must not widen)", host, rec.Code)
		}
	}
}

func TestTLSRoutesServeOnlyTheCertificate(t *testing.T) {
	rig := newTLSRig(t, nil)
	valid := rig.login(t)
	rec := rig.do(tlsReq{path: "/api/tls/ca.crt", cookie: valid})
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-x509-ca-cert" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(rec.Header().Get("Content-Disposition"), "ipcctv-local-ca.crt") {
		t.Fatalf("ca.crt: %d %v", rec.Code, rec.Header())
	}
	if containsKeyMaterial(rec.Body.Bytes()) || !strings.HasPrefix(rec.Body.String(), "-----BEGIN CERTIFICATE-----") {
		t.Error("the download is not a bare certificate")
	}
	info := rig.do(tlsReq{path: "/api/tls/info", cookie: valid})
	ca, _, _ := rig.mgr.EnsureCA()
	if info.Code != 200 || !strings.Contains(info.Body.String(), fingerprint(ca)) || strings.Contains(info.Body.String(), "PRIVATE KEY") {
		t.Errorf("info: %d %s", info.Code, info.Body.String())
	}
	// Nothing in the TLS folder other than the two certificates can be fetched by path.
	for _, p := range []string{"/tls/ca.key", "/ca.key", "/leaf.key", "/api/tls/ca.key", "/api/tls/../ca.key"} {
		if r := rig.do(tlsReq{path: p, cookie: valid}); r.Code == 200 && containsKeyMaterial(r.Body.Bytes()) {
			t.Errorf("%s served key material", p)
		}
	}
}

func TestRealTLSListener(t *testing.T) {
	rig := newTLSRig(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	server := &http.Server{Handler: rig.handler, TLSConfig: serverTLSConfig(rig.mgr)}
	go server.ServeTLS(ln, "", "")
	defer server.Close()
	addr := ln.Addr().String()

	ca, _, _ := rig.mgr.EnsureCA()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()

	req, _ := http.NewRequest("GET", "https://"+addr+"/api/live", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("no session over real TLS: %d", resp.StatusCode)
	}
	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("TLS state %+v", resp.TLS)
	}
	// A browser navigation is sent to the login page.
	nav, _ := http.NewRequest("GET", "https://"+addr+"/", nil)
	nav.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err = client.Do(nav)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=%2F" {
		t.Errorf("navigation without a session: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Sign in over the wire (the guard's host check needs the listener's own name).
	login, _ := http.NewRequest("POST", "https://"+addr+"/api/auth/login", strings.NewReader(loginBody(testUser, testPassword)))
	login.Header.Set("Content-Type", "application/json")
	login.Host = lanHost
	resp, err = client.Do(login)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if resp.StatusCode != 200 || cookie == nil {
		t.Fatalf("login over real TLS: %d", resp.StatusCode)
	}
	req.AddCookie(cookie)
	req.Host = lanHost
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("signed in over real TLS: %d", resp.StatusCode)
	}
	leaf := resp.TLS.PeerCertificates[0]
	if !leaf.IPAddresses[0].Equal(net.ParseIP(lanIP)) || len(leaf.DNSNames) == 0 || leaf.IsCA {
		t.Errorf("served leaf: %v %v", leaf.IPAddresses, leaf.DNSNames)
	}
	// A client that does not trust the CA refuses.
	bare := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}
	if _, err := bare.Get("https://" + addr + "/"); err == nil {
		t.Error("an untrusting client connected")
	}
	// Plain HTTP to the TLS port gets no application content.
	plain, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	plain.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(plain, "GET /api/live HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\n\r\n", addr, sessionCookie, cookie.Value)
	reply, _ := io.ReadAll(plain)
	text := string(reply)
	if strings.Contains(text, `"enabled"`) || strings.Contains(text, "<html") || (text != "" && !strings.Contains(text, "400")) {
		t.Errorf("plain HTTP on the TLS port got: %q", text)
	}
}

func TestValidateTLS(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
		msg  string
	}{
		{"off", Config{}, true, ""},
		{"valid", Config{TLSListen: lanListen}, true, ""},
		{"0.0.0.0", Config{TLSListen: "0.0.0.0:8443"}, false, "private IPv4"},
		{"public", Config{TLSListen: "8.8.8.8:8443"}, false, "private IPv4"},
		{"hostname", Config{TLSListen: "nvr.lan:8443"}, false, "private IPv4"},
		{"ipv6", Config{TLSListen: "[fd00::1]:8443"}, false, "private IPv4"},
		{"loopback", Config{TLSListen: "127.0.0.1:8443"}, false, "private IPv4"},
		{"no port", Config{TLSListen: lanIP}, false, "host:port"},
		{"bad extra host", Config{TLSListen: lanListen, TLSHosts: []string{"example.com"}}, false, "TLSHosts"},
		{"good extra hosts", Config{TLSListen: lanListen, TLSHosts: []string{"nvr.lan", "10.0.0.5"}}, true, ""},
	}
	for _, c := range cases {
		err := c.cfg.ValidateTLS()
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), c.msg)) {
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.msg)
		}
	}
	// The loopback guard is independent: a tokenless loopback listener is fine when TLS is on.
	if err := (Config{Listen: "127.0.0.1:8787", TLSListen: lanListen}).Validate(); err != nil {
		t.Errorf("loopback validation changed: %v", err)
	}
}

// The LAN listener refuses to start without a user, in the style of the old token refusal.
func TestValidateTLSUsersRefusesWithoutAUser(t *testing.T) {
	dir := t.TempDir()
	store := newFileUserStore(dir, time.Now)
	cfg := Config{TLSListen: lanListen, AuthDir: dir}
	err := cfg.ValidateTLSUsers(store)
	if err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "-set-password") || !strings.Contains(err.Error(), "/setup") {
		t.Fatalf("no user: %v", err)
	}
	if err := (Config{}).ValidateTLSUsers(store); err != nil {
		t.Errorf("TLS off must not need a user: %v", err)
	}
	_ = store.Put(User{ID: "1", Username: "anna", Hash: "h", Role: roleAdmin})
	if err := cfg.ValidateTLSUsers(store); err != nil {
		t.Errorf("with a user: %v", err)
	}
	// An unreadable user file also refuses: fail closed.
	bad := t.TempDir()
	_ = os.WriteFile(bad+"/users.json", []byte("{{"), 0o600)
	if err := cfg.ValidateTLSUsers(newFileUserStore(bad, time.Now)); err == nil {
		t.Error("a corrupt users.json did not refuse")
	}
}

func TestLoadConfigTLS(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("CAMUI_TOKEN", "")
	cfg, err := LoadConfig("", Config{ScriptDir: t.TempDir(), TLSListen: lanListen})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "" || cfg.TLSListen != lanListen || !strings.HasSuffix(cfg.TLSDir, "tls") || cfg.RequiresToken() {
		t.Errorf("config: %+v", cfg)
	}
	want := filepath.Join(os.Getenv("LOCALAPPDATA"), "IPCCTV", "camui", "auth")
	if cfg.AuthDir != want {
		t.Errorf("default auth folder %q, want %q", cfg.AuthDir, want)
	}
	explicit, _ := LoadConfig("", Config{ScriptDir: t.TempDir(), AuthDir: "X:/elsewhere"})
	if explicit.AuthDir != "X:/elsewhere" {
		t.Errorf("-auth-dir ignored: %q", explicit.AuthDir)
	}
}

func TestPrintCA(t *testing.T) {
	dir := t.TempDir() + "/tls"
	var out, errOut bytes.Buffer
	if code := runPrintCA(Config{TLSDir: dir}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	mgr := newCertManager(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ca, _, _ := mgr.EnsureCA()
	text := out.String()
	if !strings.Contains(text, fingerprint(ca)) || !strings.Contains(text, dir) || strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, "BEGIN") {
		t.Errorf("print-ca output:\n%s", text)
	}
	if strings.Contains(strings.ToLower(text), "token") {
		t.Errorf("print-ca still talks about a token:\n%s", text)
	}
	if _, err := os.Stat(dir + "/leaf.crt"); err == nil {
		t.Error("-print-ca created a server certificate")
	}
	// Idempotent: the same CA the second time.
	var out2 bytes.Buffer
	runPrintCA(Config{TLSDir: dir}, &out2, &errOut)
	if !strings.Contains(out2.String(), fingerprint(ca)) {
		t.Error("-print-ca created a second CA")
	}
}
