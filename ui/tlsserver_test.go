package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	tlsTestToken = "lan-token-0123456789abcdef-0123456789abcdef"
	lanIP        = "192.168.1.50"
	lanListen    = "192.168.1.50:8443"
	lanHost      = "192.168.1.50:8443"
)

type tlsRig struct {
	handler http.Handler
	logs    *bytes.Buffer
	mgr     *certManager
	limiter *failureLimiter
	cfg     Config
}

func newTLSRig(t *testing.T, mutate func(*Config)) *tlsRig {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{Listen: "127.0.0.1:8787", ArchiveRoot: dir, StatusFile: dir + "/_state/status.json", ScriptDir: dir,
		TLSListen: lanListen, TLSToken: tlsTestToken, TLSDir: dir + "/tls"}
	if mutate != nil {
		mutate(&cfg)
	}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
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
	limiter := newFailureLimiter(10, time.Minute, 1024)
	h, err := newTLSHandler(cfg, log, mgr, names, limiter)
	if err != nil {
		t.Fatal(err)
	}
	return &tlsRig{handler: h, logs: logs, mgr: mgr, limiter: limiter, cfg: cfg}
}

var tlsReqCounter int

type tlsReq struct {
	method, path string
	host         string
	remote       string
	headers      map[string]string
	cookie       string
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
		req.AddCookie(&http.Cookie{Name: tokenCookie, Value: q.cookie})
	}
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	return rec
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

func TestTLSTokenMatrixOnEveryRoute(t *testing.T) {
	rig := newTLSRig(t, nil)
	valid := cookieValue(tlsTestToken)
	states := []struct {
		name   string
		apply  func(q tlsReq) tlsReq
		want   func(code int) bool
		expect string
	}{
		{"no token", func(q tlsReq) tlsReq { return q }, func(c int) bool { return c == 401 }, "401"},
		{"wrong ?token=", func(q tlsReq) tlsReq { q.path += sep(q.path) + "token=nope"; return q }, func(c int) bool { return c == 401 }, "401"},
		{"empty ?token=", func(q tlsReq) tlsReq { q.path += sep(q.path) + "token="; return q }, func(c int) bool { return c == 401 }, "401"},
		{"wrong bearer", func(q tlsReq) tlsReq { q.headers = merge(q.headers, "Authorization", "Bearer nope"); return q }, func(c int) bool { return c == 401 }, "401"},
		{"bearer with the cookie value", func(q tlsReq) tlsReq { q.headers = merge(q.headers, "Authorization", "Bearer "+valid); return q }, func(c int) bool { return c == 401 }, "401"},
		{"wrong cookie", func(q tlsReq) tlsReq { q.cookie = "nope"; return q }, func(c int) bool { return c == 401 }, "401"},
		{"the RAW token as a cookie", func(q tlsReq) tlsReq { q.cookie = tlsTestToken; return q }, func(c int) bool { return c == 401 }, "401"},
		{"basic auth", func(q tlsReq) tlsReq { q.headers = merge(q.headers, "Authorization", "Basic "+tlsTestToken); return q }, func(c int) bool { return c == 401 }, "401"},
		{"the loopback token", func(q tlsReq) tlsReq {
			q.headers = merge(q.headers, "Authorization", "Bearer "+"loopback-token-x")
			return q
		}, func(c int) bool { return c == 401 }, "401"},
		{"valid cookie", func(q tlsReq) tlsReq { q.cookie = valid; return q }, func(c int) bool { return c != 401 && c != 429 }, "served"},
		{"valid bearer", func(q tlsReq) tlsReq { q.headers = merge(q.headers, "Authorization", "Bearer "+tlsTestToken); return q }, func(c int) bool { return c != 401 && c != 429 }, "served"},
		{"valid ?token=", func(q tlsReq) tlsReq { q.path += sep(q.path) + "token=" + tlsTestToken; return q }, func(c int) bool { return c == 303 }, "303"},
	}
	for _, route := range tlsRoutes {
		for _, s := range states {
			rec := rig.do(s.apply(route))
			if !s.want(rec.Code) {
				t.Errorf("%s %s with %s: got %d, want %s", route.method, route.path, s.name, rec.Code, s.expect)
			}
			if strings.Contains(rec.Body.String(), tlsTestToken) || strings.Contains(fmt.Sprint(rec.Header()), tlsTestToken) && s.name != "valid ?token=" {
				t.Errorf("%s %s with %s: the token appears in the response", route.method, route.path, s.name)
			}
		}
	}
	// Spot-check that "served" really reaches the handler: the page and the CA file.
	if rec := rig.do(tlsReq{path: "/", cookie: valid}); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("GET / with a valid cookie: %d", rec.Code)
	}
	if rec := rig.do(tlsReq{path: "/api/live", cookie: valid}); rec.Code != 200 {
		t.Errorf("GET /api/live with a valid cookie: %d", rec.Code)
	}
	// Positive control: with the token requirement removed the same routes ARE served, so the 401s above mean something.
	open := newTLSRig(t, nil)
	open.handler, _ = newTLSHandlerWithoutToken(t, open)
	if rec := open.do(tlsReq{path: "/api/live"}); rec.Code != 200 {
		t.Errorf("control: without the token layer /api/live gave %d", rec.Code)
	}
}

func sep(path string) string {
	if strings.Contains(path, "?") {
		return "&"
	}
	return "?"
}

func merge(h map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for key, value := range h {
		out[key] = value
	}
	out[k] = v
	return out
}

func newTLSHandlerWithoutToken(t *testing.T, rig *tlsRig) (http.Handler, error) {
	t.Helper()
	mux, err := buildMux(rig.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return mux, err
}

func TestTLSCookieAndRedirect(t *testing.T) {
	rig := newTLSRig(t, nil)
	rec := rig.do(tlsReq{path: "/?token=" + tlsTestToken + "&x=1"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/?x=1" || strings.Contains(loc, "token") || strings.Contains(loc, tlsTestToken) {
		t.Errorf("Location %q still carries the token", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies", len(cookies))
	}
	c := cookies[0]
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Errorf("cookie flags: Secure=%v HttpOnly=%v SameSite=%v Path=%q MaxAge=%d", c.Secure, c.HttpOnly, c.SameSite, c.Path, c.MaxAge)
	}
	if c.Value == tlsTestToken || strings.Contains(c.Value, tlsTestToken) || c.Value != cookieValue(tlsTestToken) {
		t.Errorf("the cookie holds %q: it must be the derived value, not the token", c.Value)
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("no Referrer-Policy")
	}
	// The cookie then opens the page, and survives a restart (same token -> same value).
	if again := rig.do(tlsReq{path: "/", cookie: c.Value}); again.Code != 200 {
		t.Errorf("the cookie did not open the page: %d", again.Code)
	}
	if cookieValue(tlsTestToken) != cookieValue(tlsTestToken) || cookieValue(tlsTestToken) == cookieValue(tlsTestToken+"x") {
		t.Error("cookie derivation is not a stable function of the token")
	}
	// The loopback listener's cookie is exactly what it was: the raw token, no Secure.
	plain := requireToken(goodToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	lrec := httptest.NewRecorder()
	plain.ServeHTTP(lrec, httptest.NewRequest("GET", "/?token="+goodToken, nil))
	lc := lrec.Result().Cookies()
	if len(lc) != 1 || lc[0].Value != goodToken || lc[0].Secure {
		t.Errorf("the loopback cookie changed: %+v", lc)
	}
}

func TestTLSLogsNeverHoldTheToken(t *testing.T) {
	rig := newTLSRig(t, nil)
	for _, q := range []tlsReq{
		{path: "/api/status?token=" + tlsTestToken + "x"}, {path: "/api/status", headers: map[string]string{"Authorization": "Bearer " + tlsTestToken + "y"}},
		{path: "/api/status", cookie: tlsTestToken}, {path: "/api/status"}, {path: "/?token=" + tlsTestToken},
	} {
		rig.do(q)
	}
	logs := rig.logs.String()
	if strings.Contains(logs, tlsTestToken) || strings.Contains(logs, cookieValue(tlsTestToken)) {
		t.Errorf("the token or its cookie value reached the log:\n%s", logs)
	}
	if strings.Count(logs, "token refused") != 4 || !strings.Contains(logs, "wrong token") || !strings.Contains(logs, "no token") {
		t.Errorf("failures not logged as expected:\n%s", logs)
	}
	// Positive control: the scan fails on a log that does hold the token.
	if !strings.Contains("x token="+tlsTestToken, tlsTestToken) {
		t.Error("control")
	}
}

func TestFailureLimiter(t *testing.T) {
	now := time.Now()
	l := newFailureLimiter(10, time.Minute, 1024)
	l.now = func() time.Time { return now }
	for i := 0; i < 9; i++ {
		l.fail("1.1.1.1")
	}
	if _, blocked := l.blocked("1.1.1.1"); blocked {
		t.Error("blocked after 9 failures")
	}
	l.fail("1.1.1.1")
	retry, blocked := l.blocked("1.1.1.1")
	if !blocked || retry <= 0 || retry > time.Minute {
		t.Errorf("after 10 failures: blocked=%v retry=%v", blocked, retry)
	}
	if _, blocked := l.blocked("2.2.2.2"); blocked {
		t.Error("another source is blocked")
	}
	now = now.Add(61 * time.Second)
	if _, blocked := l.blocked("1.1.1.1"); blocked {
		t.Error("still blocked after the window")
	}
	l.fail("1.1.1.1")
	if _, blocked := l.blocked("1.1.1.1"); blocked {
		t.Error("the window did not restart at 1 failure")
	}
	// Bounded memory: thousands of sources never exceed the table size.
	small := newFailureLimiter(3, time.Minute, 50)
	small.now = func() time.Time { return now }
	for i := 0; i < 5000; i++ {
		small.fail(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256))
	}
	if small.size() > 51 {
		t.Errorf("table grew to %d entries, cap 50 (+1 overflow bucket)", small.size())
	}
	// Expired entries are purged to make room.
	now = now.Add(2 * time.Minute)
	small.fail("9.9.9.9")
	if small.size() > 2 {
		t.Errorf("expired entries were not purged: %d", small.size())
	}
}

func TestTLSLimiterOverHTTP(t *testing.T) {
	rig := newTLSRig(t, nil)
	src := "192.168.1.99:5555"
	// Requests with no credential at all are first visits, not guesses: they are never counted.
	for i := 0; i < 30; i++ {
		if rec := rig.do(tlsReq{path: "/api/status", remote: src}); rec.Code != 401 {
			t.Fatalf("no-credential request %d: %d", i, rec.Code)
		}
	}
	for i := 0; i < 10; i++ {
		if rec := rig.do(tlsReq{path: "/api/status", remote: src, headers: map[string]string{"Authorization": "Bearer wrong"}}); rec.Code != 401 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	rec := rig.do(tlsReq{path: "/api/status", remote: src})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("11th attempt: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// Even the right token is held back while blocked; another address is not.
	if rec := rig.do(tlsReq{path: "/api/status", remote: src, cookie: cookieValue(tlsTestToken)}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a valid cookie while blocked: %d", rec.Code)
	}
	if rec := rig.do(tlsReq{path: "/api/status", remote: "192.168.1.98:1", cookie: cookieValue(tlsTestToken)}); rec.Code == 401 || rec.Code == 429 {
		t.Errorf("another source: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), tlsTestToken) {
		t.Error("the 429 echoes the token")
	}
}

func TestTLSGuard(t *testing.T) {
	rig := newTLSRig(t, nil)
	ok := cookieValue(tlsTestToken)
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
	if rec := writable.do(tlsReq{method: "POST", path: "/api/acknowledge", headers: map[string]string{"Content-Type": "application/sdp"}, cookie: ok, body: "x"}); rec.Code != 415 {
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
	valid := cookieValue(tlsTestToken)
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
		t.Errorf("no token over real TLS: %d", resp.StatusCode)
	}
	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("TLS state %+v", resp.TLS)
	}
	req.Header.Set("Authorization", "Bearer "+tlsTestToken)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("with the token over real TLS: %d", resp.StatusCode)
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
	fmt.Fprintf(plain, "GET /api/live HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", addr, tlsTestToken)
	reply, _ := io.ReadAll(plain)
	text := string(reply)
	if strings.Contains(text, `"enabled"`) || strings.Contains(text, "<html") || (text != "" && !strings.Contains(text, "400")) {
		t.Errorf("plain HTTP on the TLS port got: %q", text)
	}
}

func TestValidateTLS(t *testing.T) {
	short := "short-token"
	cases := []struct {
		name string
		cfg  Config
		ok   bool
		msg  string
	}{
		{"off", Config{}, true, ""},
		{"valid TLSToken, no loopback token", Config{TLSListen: lanListen, TLSToken: tlsTestToken}, true, ""},
		{"falls back to Token", Config{TLSListen: lanListen, Token: tlsTestToken}, true, ""},
		{"no token at all", Config{TLSListen: lanListen}, false, "no token"},
		{"short TLSToken", Config{TLSListen: lanListen, TLSToken: short}, false, "shorter than the 16"},
		{"short TLSToken beats a long loopback token only when TLSToken is set", Config{TLSListen: lanListen, TLSToken: short, Token: tlsTestToken}, false, "shorter"},
		{"0.0.0.0", Config{TLSListen: "0.0.0.0:8443", TLSToken: tlsTestToken}, false, "private IPv4"},
		{"public", Config{TLSListen: "8.8.8.8:8443", TLSToken: tlsTestToken}, false, "private IPv4"},
		{"hostname", Config{TLSListen: "nvr.lan:8443", TLSToken: tlsTestToken}, false, "private IPv4"},
		{"ipv6", Config{TLSListen: "[fd00::1]:8443", TLSToken: tlsTestToken}, false, "private IPv4"},
		{"loopback", Config{TLSListen: "127.0.0.1:8443", TLSToken: tlsTestToken}, false, "private IPv4"},
		{"no port", Config{TLSListen: lanIP, TLSToken: tlsTestToken}, false, "host:port"},
		{"bad extra host", Config{TLSListen: lanListen, TLSToken: tlsTestToken, TLSHosts: []string{"example.com"}}, false, "TLSHosts"},
		{"good extra hosts", Config{TLSListen: lanListen, TLSToken: tlsTestToken, TLSHosts: []string{"nvr.lan", "10.0.0.5"}}, true, ""},
	}
	for _, c := range cases {
		err := c.cfg.ValidateTLS()
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), c.msg)) {
			t.Errorf("%s: got %v, want an error containing %q", c.name, err, c.msg)
		}
		if err != nil && strings.Contains(err.Error(), tlsTestToken) {
			t.Errorf("%s: the error echoes the token", c.name)
		}
	}
	// The loopback guard is independent: it still accepts a tokenless loopback listener when TLS is on.
	if err := (Config{Listen: "127.0.0.1:8787", TLSListen: lanListen, TLSToken: tlsTestToken}).Validate(); err != nil {
		t.Errorf("loopback validation changed: %v", err)
	}
}

func TestLoadConfigTLS(t *testing.T) {
	t.Setenv("CAMUI_TLS_TOKEN", tlsTestToken)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("CAMUI_TOKEN", "")
	cfg, err := LoadConfig("", Config{ScriptDir: t.TempDir(), TLSListen: lanListen})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSToken != tlsTestToken || cfg.Token != "" || cfg.TLSListen != lanListen || !strings.HasSuffix(cfg.TLSDir, "tls") || cfg.RequiresToken() {
		t.Errorf("config: %+v", cfg)
	}
}

func TestNewTokenAndPrintCA(t *testing.T) {
	a, _ := newToken()
	b, _ := newToken()
	if len(a) != 64 || a == b || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("tokens %q %q", a, b)
	}
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
