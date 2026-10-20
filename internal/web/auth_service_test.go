package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const genericLoginError = "Invalid username or password"

func (rig *tlsRig) loginFrom(remote, user, password string) *httptest.ResponseRecorder {
	return rig.do(tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, remote: remote, body: loginBody(user, password)})
}

func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %q", rec.Body.String())
	}
	return out
}

func TestLoginSetsAHardenedSessionCookie(t *testing.T) {
	rig := newTLSRig(t, nil)
	rec := rig.loginFrom("192.168.1.20:4000", "ANNA", testPassword) // case-insensitive name
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	c := sessionCookieOf(rec)
	if c == nil {
		t.Fatal("no cookie")
	}
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != int(sessionLifetime.Seconds()) || c.Domain != "" {
		t.Errorf("cookie flags: %+v", c)
	}
	if c.Name != "camui_session" {
		t.Errorf("cookie name %q", c.Name)
	}
	body := decode(t, rec)
	if body["username"] != "anna" || body["role"] != "admin" || body["next"] != "/" {
		t.Errorf("body %v", body)
	}
	for _, leak := range []string{c.Value, testPassword, "pbkdf2"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the login response contains %q", leak)
		}
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control %q", cc)
	}
	me := rig.do(tlsReq{path: "/api/auth/me", cookie: c.Value})
	got := decode(t, me)
	if me.Code != 200 || got["authenticated"] != true || got["username"] != "anna" || got["role"] != "admin" || got["loginRequired"] != true {
		t.Errorf("me: %d %v", me.Code, got)
	}
	anon := decode(t, rig.do(tlsReq{path: "/api/auth/me"}))
	if anon["authenticated"] != false || anon["loginRequired"] != true || anon["username"] != nil {
		t.Errorf("me when signed out: %v", anon)
	}
}

func TestEveryLoginIssuesANewTokenAndLogoutEndsIt(t *testing.T) {
	rig := newTLSRig(t, nil)
	first := sessionCookieOf(rig.loginFrom("192.168.1.20:1", testUser, testPassword)).Value
	// Logging in again while holding the first cookie replaces that session (no fixation).
	rec := rig.do(tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, cookie: first, remote: "192.168.1.20:2", body: loginBody(testUser, testPassword)})
	second := sessionCookieOf(rec).Value
	if second == first {
		t.Fatal("the token was reused across logins")
	}
	if r := rig.do(tlsReq{path: "/api/status", cookie: first}); r.Code != 401 {
		t.Errorf("the old session still works after a new login from the same browser: %d", r.Code)
	}
	if r := rig.do(tlsReq{path: "/api/status", cookie: second}); r.Code != 200 {
		t.Errorf("the new session: %d", r.Code)
	}
	out := rig.do(tlsReq{method: "POST", path: "/api/auth/logout", headers: jsonHdr, cookie: second, body: "{}"})
	cleared := sessionCookieOf(out)
	if out.Code != 200 || cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("logout: %d cookie %+v", out.Code, cleared)
	}
	if r := rig.do(tlsReq{path: "/api/status", cookie: second}); r.Code != 401 {
		t.Errorf("the session works after logout: %d", r.Code)
	}
	// Logging out when not signed in is refused like any protected call.
	if r := rig.do(tlsReq{method: "POST", path: "/api/auth/logout", headers: jsonHdr, body: "{}"}); r.Code != 401 {
		t.Errorf("logout without a session: %d", r.Code)
	}
}

// The unknown-user and wrong-password answers must be indistinguishable.
func TestUnknownUserAndWrongPasswordLookIdentical(t *testing.T) {
	rig := newTLSRig(t, nil)
	wrong := rig.loginFrom("192.168.1.30:1", testUser, "not the password at all")
	unknown := rig.loginFrom("192.168.1.31:1", "nobody-here", "not the password at all")
	if wrong.Code != 401 || unknown.Code != 401 {
		t.Fatalf("status %d / %d", wrong.Code, unknown.Code)
	}
	if wrong.Body.String() != unknown.Body.String() {
		t.Errorf("bodies differ: %q vs %q", wrong.Body.String(), unknown.Body.String())
	}
	if fmt.Sprint(wrong.Header()) != fmt.Sprint(unknown.Header()) {
		t.Errorf("headers differ: %v vs %v", wrong.Header(), unknown.Header())
	}
	if decode(t, wrong)["error"] != genericLoginError {
		t.Errorf("message %q", wrong.Body.String())
	}
	if sessionCookieOf(wrong) != nil || sessionCookieOf(unknown) != nil {
		t.Error("a failed login set a cookie")
	}
	// A name that violates the username rules gets the same answer, not a validation error.
	weird := rig.loginFrom("192.168.1.32:1", "no body/\u00e9 ", "x")
	if weird.Code != 401 || weird.Body.String() != wrong.Body.String() {
		t.Errorf("odd name: %d %q", weird.Code, weird.Body.String())
	}
}

func TestUnknownUserCostsAboutAsMuchAsAWrongPassword(t *testing.T) {
	rig := newTLSRig(t, nil)
	rig.auth.hasher = passwordHasher{iterations: 60000} // slow enough to measure, fast enough for a test
	hash, _ := rig.auth.hasher.Hash(testPassword)
	_ = rig.users.Put(User{ID: "u-anna", Username: testUser, Hash: hash, Role: roleAdmin})
	measure := func(user string) time.Duration {
		var samples []time.Duration
		for i := 0; i < 9; i++ {
			start := time.Now()
			rig.loginFrom(fmt.Sprintf("192.168.2.%d:1", i+1), user, "wrong wrong wrong wrong")
			samples = append(samples, time.Since(start))
			rig.auth.lock.reset(user) // keep both measurements out of the lockout
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[0] // the fastest run is the one least disturbed by other work on the machine
	}
	known, unknown := measure(testUser), measure("nobody-here")
	ratio := float64(unknown) / float64(known)
	t.Logf("fastest wrong password %v, unknown user %v (ratio %.2f)", known, unknown, ratio)
	if ratio < 0.5 || ratio > 2 {
		t.Errorf("an unknown user takes %.2fx a wrong password: the response time tells which names exist", ratio)
	}
}

func TestLockoutOverHTTP(t *testing.T) {
	rig := newTLSRig(t, nil)
	from := func(i int) string { return fmt.Sprintf("192.168.3.%d:1", i+1) } // a fresh address each time: only the name is throttled
	for i := 0; i < 4; i++ {
		if rec := rig.loginFrom(from(i), testUser, "wrong password number "+fmt.Sprint(i)); rec.Code != 401 {
			t.Fatalf("failure %d: %d", i+1, rec.Code)
		}
	}
	if rec := rig.loginFrom(from(4), testUser, "wrong password number 5"); rec.Code != 401 {
		t.Fatalf("the 5th failure itself is a plain 401, got %d", rec.Code)
	}
	// Now locked: even the right password is refused, with Retry-After and the same shape for every name.
	locked := rig.loginFrom(from(5), testUser, testPassword)
	if locked.Code != http.StatusTooManyRequests || locked.Header().Get("Retry-After") != "30" {
		t.Fatalf("correct password during lockout: %d Retry-After %q", locked.Code, locked.Header().Get("Retry-After"))
	}
	if sessionCookieOf(locked) != nil {
		t.Error("a locked-out login set a cookie")
	}
	body := decode(t, locked)
	if body["retryAfter"] != float64(30) || !strings.Contains(body["error"].(string), "30 seconds") {
		t.Errorf("lockout body %v", body)
	}
	// A name that does not exist locks the same way (so the answer does not say which names exist).
	for i := 0; i < 5; i++ {
		rig.loginFrom(from(10+i), "ghost", "wrong password number "+fmt.Sprint(i))
	}
	ghost := rig.loginFrom(from(20), "ghost", "whatever whatever")
	if ghost.Code != locked.Code || ghost.Header().Get("Retry-After") != locked.Header().Get("Retry-After") || ghost.Body.String() != locked.Body.String() || fmt.Sprint(ghost.Header()) != fmt.Sprint(locked.Header()) {
		t.Errorf("locked real name and locked ghost differ:\n%d %v %q\n%d %v %q", locked.Code, locked.Header(), locked.Body.String(), ghost.Code, ghost.Header(), ghost.Body.String())
	}
	// Attempts during the lock are not counted: it ends on schedule.
	rig.clock.advance(29 * time.Second)
	if rec := rig.loginFrom(from(30), testUser, testPassword); rec.Code != 429 {
		t.Errorf("29 s into a 30 s lock: %d", rec.Code)
	}
	rig.clock.advance(time.Second)
	ok := rig.loginFrom(from(31), testUser, testPassword)
	if ok.Code != 200 {
		t.Fatalf("after the lock: %d %s", ok.Code, ok.Body.String())
	}
	// Success resets the counter and says nothing about the past failures.
	fresh := newTLSRig(t, nil).loginFrom("192.168.9.9:1", testUser, testPassword)
	if strings.ReplaceAll(ok.Body.String(), "", "") != fresh.Body.String() {
		t.Errorf("a login after failures reveals more than a clean one:\n%s\n%s", ok.Body.String(), fresh.Body.String())
	}
	if st, present := rig.users.Failure(testUser); present && st.Count != 0 {
		t.Errorf("counter after success: %+v", st)
	}
	for i := 0; i < 4; i++ {
		rig.loginFrom(from(40+i), testUser, "wrong again number "+fmt.Sprint(i))
	}
	if rec := rig.loginFrom(from(50), testUser, testPassword); rec.Code != 200 {
		t.Errorf("4 failures after a success must not lock: %d", rec.Code)
	}
}

func TestLockoutDoublesAndCaps(t *testing.T) {
	rig := newTLSRig(t, nil)
	n := 0
	attempt := func(pw string) *httptest.ResponseRecorder {
		n++
		return rig.loginFrom(fmt.Sprintf("10.9.%d.%d:1", n/250, n%250+1), "anna", pw)
	}
	want := []string{"30", "60", "120", "240", "480", "900", "900"}
	for i := 0; i < 4; i++ {
		attempt("wrong wrong wrong 1")
	}
	for step, retry := range want {
		attempt("wrong wrong wrong 2") // the failure that starts this lock
		rec := attempt(testPassword)
		if rec.Code != 429 || rec.Header().Get("Retry-After") != retry {
			t.Fatalf("lock %d: %d Retry-After %q, want %s", step+1, rec.Code, rec.Header().Get("Retry-After"), retry)
		}
		rig.clock.advance(time.Duration(mustAtoi(retry)) * time.Second)
	}
}

func mustAtoi(s string) int {
	var n int
	fmt.Sscan(s, &n)
	return n
}

func TestPerAddressThrottle(t *testing.T) {
	rig := newTLSRig(t, nil)
	const ip = "192.168.4.4:5555"
	for i := 0; i < 5; i++ {
		// A different name each time: only the address is being throttled.
		if rec := rig.loginFrom(ip, fmt.Sprintf("name%d", i), "wrong password here"); rec.Code != 401 {
			t.Fatalf("burst attempt %d: %d", i+1, rec.Code)
		}
	}
	rec := rig.loginFrom(ip, "name9", "wrong password here")
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "6" {
		t.Fatalf("6th attempt in a burst: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// The right password does not get through a throttled address either.
	if rec := rig.loginFrom(ip, testUser, testPassword); rec.Code != 429 {
		t.Errorf("right password from a throttled address: %d", rec.Code)
	}
	// Another address is unaffected, and a forwarding header changes nothing.
	other := rig.do(tlsReq{method: "POST", path: "/api/auth/login", remote: "192.168.4.5:1", headers: merge(jsonHdr, "X-Forwarded-For", "192.168.4.4"), body: loginBody(testUser, testPassword)})
	if other.Code != 200 {
		t.Errorf("another address: %d", other.Code)
	}
	spoof := rig.do(tlsReq{method: "POST", path: "/api/auth/login", remote: ip, headers: merge(merge(jsonHdr, "X-Forwarded-For", "203.0.113.77"), "X-Real-IP", "203.0.113.78"), body: loginBody(testUser, testPassword)})
	if spoof.Code != 429 {
		t.Errorf("a spoofed X-Forwarded-For escaped the throttle: %d", spoof.Code)
	}
	rig.clock.advance(6 * time.Second)
	if rec := rig.loginFrom(ip, testUser, testPassword); rec.Code != 200 {
		t.Errorf("after the refill: %d", rec.Code)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(rig.cfg.AuthDir, "auth.log"))), "result=throttled") {
		t.Error("the throttling was not logged")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoginRequestShape(t *testing.T) {
	rig := newTLSRig(t, nil)
	cases := []struct {
		name string
		req  tlsReq
		want int
	}{
		{"empty body", tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr}, 400},
		{"not json", tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, body: "username=anna&password=x"}, 400},
		{"form content type", tlsReq{method: "POST", path: "/api/auth/login", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, body: "username=anna&password=x"}, 415},
		{"text/plain", tlsReq{method: "POST", path: "/api/auth/login", headers: map[string]string{"Content-Type": "text/plain"}, body: loginBody(testUser, testPassword)}, 415},
		{"over 4 KB", tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, body: `{"username":"anna","password":"` + strings.Repeat("a", 5000) + `"}`}, 413},
		{"just under 4 KB", tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, body: `{"username":"anna","password":"` + strings.Repeat("a", 4000) + `"}`}, 401},
		{"missing password", tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, body: `{"username":"anna"}`}, 401},
		{"numbers instead of strings", tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, body: `{"username":1,"password":2}`}, 400},
		{"GET", tlsReq{method: "GET", path: "/api/auth/login"}, 401}, // not public: no route
	}
	for _, c := range cases {
		rec := rig.do(c.req)
		if rec.Code != c.want {
			t.Errorf("%s: %d %q, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
		if sessionCookieOf(rec) != nil {
			t.Errorf("%s: set a cookie", c.name)
		}
	}
}

// A cross-site POST to the login endpoint is refused by the guard before any
// password is checked, and changes nothing: no failure counted, no throttle token
// taken, no session, no log line.
func TestLoginCSRFIsRefusedWithoutAnyStateChange(t *testing.T) {
	rig := newTLSRig(t, nil)
	cases := []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"foreign origin", "", map[string]string{"Origin": "https://evil.example"}},
		{"http origin", "", map[string]string{"Origin": "http://" + lanHost}},
		{"null origin", "", map[string]string{"Origin": "null"}},
		{"cross-site fetch", "", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site (sibling) fetch", "", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"rebinding host", "attacker-rebind.example:8443", nil},
		{"form post", "", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
	}
	for _, c := range cases {
		// The right password: if the guard did not stop it, this would sign in.
		headers := merge(jsonHdr, "X-Test", "1")
		for k, v := range c.headers {
			headers[k] = v
		}
		rec := rig.do(tlsReq{method: "POST", path: "/api/auth/login", host: c.host, headers: headers, remote: "192.168.5.5:1", body: loginBody(testUser, testPassword)})
		if rec.Code != 403 && rec.Code != 415 {
			t.Errorf("%s: %d, want 403 (or 415 for a form)", c.name, rec.Code)
		}
		if sessionCookieOf(rec) != nil {
			t.Errorf("%s: a cookie was issued", c.name)
		}
	}
	if _, err := os.Stat(filepath.Join(rig.cfg.AuthDir, "sessions.json")); err == nil {
		t.Error("a session was written")
	}
	if _, err := os.Stat(filepath.Join(rig.cfg.AuthDir, "auth.log")); err == nil {
		t.Error("the refused requests reached the login handler (auth.log exists)")
	}
	if rig.auth.limiter.size() != 0 {
		t.Error("a throttle token was taken")
	}
	if _, present := rig.users.Failure(testUser); present {
		t.Error("a failure was counted")
	}
	// Same attacker request with wrong passwords does not feed the lockout either.
	for i := 0; i < 8; i++ {
		rig.do(tlsReq{method: "POST", path: "/api/auth/login", headers: map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}, remote: "192.168.5.5:1", body: loginBody(testUser, "wrong wrong wrong")})
	}
	if rec := rig.loginFrom("192.168.5.6:1", testUser, testPassword); rec.Code != 200 {
		t.Errorf("cross-site attempts locked the owner out: %d", rec.Code)
	}
}

func TestNextValidation(t *testing.T) {
	good := map[string]string{
		"":                 "/",
		"/":                "/",
		"/#live":           "/#live",
		"/?a=1#live":       "/?a=1#live",
		"/api/tls/ca.crt":  "/api/tls/ca.crt",
		"/footage/Cam%201": "/footage/Cam%201",
	}
	bad := []string{
		"//evil.example", "//evil.example/path", "/\\evil.example", "\\\\evil.example", "\\evil", "/\\/evil.example",
		"https://evil.example", "http://evil.example/", "javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,x", "evil.example", "evil", "///evil", "/\t/evil.example",
		"/\n/evil.example", "/\r/evil.example", " //evil.example", "\t//evil.example", "/%0d%0aSet-Cookie:x=1" + strings.Repeat("a", 3000),
		"/login", "/login?next=/", "/login/..//evil", "mailto:x@y", "file:///etc/passwd", "/\x00", "/\u2028evil", strings.Repeat("/a", 2000),
	}
	for in, want := range good {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range bad {
		if got := safeNext(in); got != "/" {
			t.Errorf("safeNext(%q) = %q, want /", in, got)
		}
	}
	// End to end: the login answer carries the sanitised target, never the raw one.
	rig := newTLSRig(t, nil)
	for i, in := range []string{"//evil.example", "https://evil.example/", "/\\evil", "/#live"} {
		body, _ := json.Marshal(map[string]string{"username": testUser, "password": testPassword, "next": in})
		rec := rig.do(tlsReq{method: "POST", path: "/api/auth/login", headers: jsonHdr, remote: fmt.Sprintf("192.168.6.%d:1", i+1), body: string(body)})
		got := decode(t, rec)["next"]
		if (in == "/#live" && got != "/#live") || (in != "/#live" && got != "/") {
			t.Errorf("login with next=%q answered next=%v", in, got)
		}
	}
}

// ---- the route x session-state matrix ----

var publicRoutes = map[string]bool{
	"GET /login": true, "GET /login.js": true, "GET /style.css": true, "POST /api/auth/login": true, "GET /api/auth/me": true,
}

func isAuthRequired(rec *httptest.ResponseRecorder, method string) bool {
	if rec.Code != 401 || rec.Header().Get(loginRequiredHeader) == "" {
		return false
	}
	if method == "HEAD" {
		return true
	}
	var body map[string]any
	return json.Unmarshal(rec.Body.Bytes(), &body) == nil && body["error"] == "authentication required"
}

func TestRouteMatrixEverySessionState(t *testing.T) {
	rig := newTLSRig(t, nil)
	// Tokens made in this order so the expired one is really expired when the valid one is made.
	expired := sessionCookieOf(rig.loginFrom("192.168.7.1:1", testUser, testPassword)).Value
	rig.clock.advance(sessionIdleTimeout + time.Hour)
	valid := sessionCookieOf(rig.loginFrom("192.168.7.2:1", testUser, testPassword)).Value
	states := []struct{ name, cookie string }{{"no session", ""}, {"wrong cookie", "AAAA-not-a-session"}, {"expired session", expired}, {"valid session", valid}}
	if len(roRoutes) < 30 {
		t.Fatalf("only %d classified routes", len(roRoutes))
	}
	checked := 0
	for _, route := range roRoutes {
		pathOnly, _, _ := strings.Cut(route.path, "?")
		public := publicRoutes[route.method+" "+pathOnly]
		for _, st := range states {
			q := tlsReq{method: route.method, path: route.path, headers: route.headers, body: route.body, cookie: st.cookie}
			if pathOnly == "/api/auth/logout" && st.name == "valid session" {
				q.cookie = sessionCookieOf(rig.loginFrom("192.168.7.99:1", testUser, testPassword)).Value // logging out spends the session
				rig.clock.advance(7 * time.Second)                                                        // and the address throttle refills
			}
			rec := rig.do(q)
			checked++
			denied := isAuthRequired(rec, route.method)
			switch {
			case public:
				if denied {
					t.Errorf("[%s] %s %s is public but was refused for lack of a session", st.name, route.method, route.path)
				}
			case st.name == "valid session":
				if denied || rec.Code == 303 && strings.HasPrefix(rec.Header().Get("Location"), "/login") {
					t.Errorf("[%s] %s %s: refused with a valid session (%d)", st.name, route.method, route.path, rec.Code)
				}
			default:
				if !denied {
					t.Errorf("[%s] %s %s: %d %q, want 401 authentication required", st.name, route.method, route.path, rec.Code, rec.Body.String())
				}
				if rec.Header().Get("Location") != "" || sessionCookieOf(rec) != nil {
					t.Errorf("[%s] %s %s: an API denial redirected or set a cookie", st.name, route.method, route.path)
				}
				// A browser navigation to a protected GET is redirected to the login page instead.
				if route.method == "GET" || route.method == "HEAD" {
					nav := rig.do(tlsReq{method: route.method, path: route.path, headers: map[string]string{"Accept": "text/html,application/xhtml+xml"}, cookie: st.cookie})
					if nav.Code != 303 || !strings.HasPrefix(nav.Header().Get("Location"), "/login?next=") {
						t.Errorf("[%s] navigation to %s: %d Location %q, want 303 to /login", st.name, route.path, nav.Code, nav.Header().Get("Location"))
					}
				} else if html := rig.do(tlsReq{method: route.method, path: route.path, headers: merge(route.headers, "Accept", "text/html"), body: route.body, cookie: st.cookie}); html.Code != 401 {
					t.Errorf("[%s] %s %s with Accept text/html: %d, want 401 (only GET navigations redirect)", st.name, route.method, route.path, html.Code)
				}
			}
		}
	}
	t.Logf("%d routes x %d session states = %d requests checked", len(roRoutes), len(states), checked)
	// The table of public routes names real routes: each is in roRoutes.
	for key := range publicRoutes {
		found := false
		for _, r := range roRoutes {
			p, _, _ := strings.Cut(r.path, "?")
			found = found || r.method+" "+p == key
		}
		if !found {
			t.Errorf("public route %q is not in roRoutes", key)
		}
	}
	// Positive control: an unclassified new route is protected by default.
	if rec := rig.do(tlsReq{path: "/api/brand-new-thing"}); rec.Code != 401 {
		t.Errorf("an unknown API path without a session: %d", rec.Code)
	}
}

func TestProtectedNavigationRedirectCarriesASafeNext(t *testing.T) {
	rig := newTLSRig(t, nil)
	for path, want := range map[string]string{
		"/":              "/login?next=%2F",
		"/index.html":    "/login?next=%2Findex.html",
		"/?a=1":          "/login?next=%2F%3Fa%3D1",
		"//evil.example": "/login?next=%2F", // the mux would clean it; either way the target is local
	} {
		rec := rig.do(tlsReq{path: path, headers: map[string]string{"Accept": "text/html"}})
		loc := rec.Header().Get("Location")
		if rec.Code != 303 && rec.Code != 301 {
			t.Errorf("%s: %d", path, rec.Code)
		}
		if rec.Code == 303 && loc != want && path != "//evil.example" {
			t.Errorf("%s: Location %q, want %q", path, loc, want)
		}
		if strings.HasPrefix(loc, "//") || strings.Contains(loc, "evil") && !strings.HasPrefix(loc, "/login?next=") {
			t.Errorf("%s redirected off-site: %q", path, loc)
		}
	}
}

func TestLoginPageAndAssetsArePublicWithSafeHeaders(t *testing.T) {
	rig := newTLSRig(t, nil)
	page := rig.do(tlsReq{path: "/login", headers: map[string]string{"Accept": "text/html"}})
	if page.Code != 200 || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/login: %d %v", page.Code, page.Header())
	}
	csp := page.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if !strings.Contains(page.Header().Get("Cache-Control"), "no-store") || page.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers %v", page.Header())
	}
	html := page.Body.String()
	for _, want := range []string{`autocomplete="username"`, `autocomplete="current-password"`, `type="password"`, `src="login.js"`, `href="style.css"`} {
		if !strings.Contains(html, want) {
			t.Errorf("login page lacks %s", want)
		}
	}
	if regexp.MustCompile(`(?i)<script[^>]*>[^<]`).MatchString(html) || regexp.MustCompile(`(?i)\son[a-z]+=`).MatchString(html) || strings.Contains(html, "style=") {
		t.Error("the login page has inline script, an inline handler or inline style: it would break a strict CSP")
	}
	for _, p := range []string{"/login.js", "/style.css"} {
		if rec := rig.do(tlsReq{path: p}); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	// Signed in, the page sends you on.
	valid := rig.login(t)
	if rec := rig.do(tlsReq{path: "/login?next=%2F%23live", cookie: valid}); rec.Code != 303 || rec.Header().Get("Location") != "/#live" {
		t.Errorf("/login when signed in: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Login assets are not reachable as plain static files, and setup is never served here.
	for _, p := range []string{"/login.html", "/setup", "/setup.html", "/setup.js"} {
		if rec := rig.do(tlsReq{path: p, cookie: valid}); rec.Code != 404 && rec.Code != 403 {
			t.Errorf("%s signed in: %d, want it refused", p, rec.Code)
		}
	}
	if rec := rig.do(tlsReq{method: "POST", path: "/api/auth/setup", headers: jsonHdr, body: loginBody("mallory", "a long enough password"), cookie: valid}); rec.Code != 404 && rec.Code != 403 {
		t.Errorf("POST /api/auth/setup on the TLS listener: %d", rec.Code)
	}
	if n, _ := rig.users.List(); len(n) != 1 {
		t.Error("the TLS listener created a user")
	}
}

func TestViewOnlyStillLetsYouSignInAndOut(t *testing.T) {
	rig := newTLSRig(t, nil) // view-only by default
	cookie := rig.login(t)
	if rec := rig.do(tlsReq{method: "POST", path: "/api/auth/logout", headers: jsonHdr, cookie: cookie, body: "{}"}); rec.Code != 200 {
		t.Errorf("logout while view-only: %d", rec.Code)
	}
}

func TestPasswordUpgradeOnLogin(t *testing.T) {
	rig := newTLSRig(t, nil)
	weak := passwordHasher{iterations: 500}
	hash, _ := weak.Hash(testPassword)
	_ = rig.users.Put(User{ID: "u-anna", Username: testUser, Hash: hash, Role: roleAdmin})
	if rec := rig.loginFrom("192.168.8.1:1", testUser, testPassword); rec.Code != 200 {
		t.Fatalf("login with an old hash: %d", rec.Code)
	}
	u, _, _ := rig.users.Get(testUser)
	if !strings.HasPrefix(u.Hash, "pbkdf2-sha256$1000$") {
		t.Errorf("hash not upgraded: %q", u.Hash)
	}
	if rec := rig.loginFrom("192.168.8.2:1", testUser, testPassword); rec.Code != 200 {
		t.Errorf("login after the upgrade: %d", rec.Code)
	}
}

func TestSessionOfADeletedUserStopsWorking(t *testing.T) {
	rig := newTLSRig(t, nil)
	cookie := rig.login(t)
	_ = rig.users.Delete("u-anna")
	if rec := rig.do(tlsReq{path: "/api/status", cookie: cookie}); rec.Code != 401 {
		t.Errorf("session of a deleted user: %d", rec.Code)
	}
}

func TestSessionSurvivesARestartOfTheServer(t *testing.T) {
	rig := newTLSRig(t, nil)
	cookie := rig.login(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ip, _, _ := parseTLSListen(rig.cfg.TLSListen)
	names, _ := buildCertNames(ip, nil)
	users := newFileUserStore(rig.cfg.AuthDir, rig.clock.now)
	again, err := newTLSHandler(rig.cfg, log, rig.mgr, names, newAuthService(rig.cfg.AuthDir, users, log, rig.clock.now, fastHasher()))
	if err != nil {
		t.Fatal(err)
	}
	rig.handler = again
	if rec := rig.do(tlsReq{path: "/api/status", cookie: cookie}); rec.Code != 200 {
		t.Errorf("after a restart: %d", rec.Code)
	}
}

// ---- no secrets anywhere ----

func TestNoPasswordHashOrTokenInLogsResponsesOrAuthLog(t *testing.T) {
	rig := newTLSRig(t, nil)
	const wrongPw = "WRONG-PASSWORD-MARKER-xyz"
	user, _, _ := rig.users.Get(testUser)
	var all strings.Builder
	collect := func(rec *httptest.ResponseRecorder) {
		all.WriteString(rec.Body.String())
		h := rec.Header().Clone()
		h.Del("Set-Cookie") // the cookie is how the token legitimately reaches the browser
		all.WriteString(fmt.Sprint(h))
	}
	var tokens []string
	collect(rig.loginFrom("192.168.9.1:1", testUser, wrongPw))
	collect(rig.loginFrom("192.168.9.2:1", "ghost-user", wrongPw))
	ok := rig.loginFrom("192.168.9.3:1", testUser, testPassword)
	collect(ok)
	token := sessionCookieOf(ok).Value
	tokens = append(tokens, token)
	collect(rig.do(tlsReq{path: "/api/auth/me", cookie: token}))
	collect(rig.do(tlsReq{path: "/api/status", cookie: token}))
	collect(rig.do(tlsReq{method: "POST", path: "/api/auth/logout", headers: jsonHdr, cookie: token, body: "{}"}))
	for i := 0; i < 6; i++ {
		collect(rig.loginFrom("192.168.9.4:1", testUser, wrongPw))
	}

	logText := rig.logs.String()
	authLogText := string(mustRead(t, filepath.Join(rig.cfg.AuthDir, "auth.log")))
	secrets := []string{testPassword, wrongPw, user.Hash, strings.Split(user.Hash, "$")[3], token, hashToken(token)}
	// Positive control: the scanner finds each secret when it IS present.
	for _, s := range secrets {
		if !leaksAny("prefix "+s+" suffix", secrets) {
			t.Fatalf("scanner control failed for %q", s)
		}
	}
	for name, text := range map[string]string{"console log": logText, "auth.log": authLogText, "responses": all.String()} {
		if leaksAny(text, secrets) {
			t.Errorf("%s contains a password, hash or token:\n%s", name, text)
		}
	}
	for _, want := range []string{"result=fail", "result=ok", "result=throttled", "ip=192.168.9.3", `user="anna"`, `user="ghost-user"`, "event=login", "event=logout"} {
		if !strings.Contains(authLogText, want) {
			t.Errorf("auth.log lacks %q:\n%s", want, authLogText)
		}
	}
	// The sessions file holds the hash of the token only.
	if raw := string(mustRead(t, filepath.Join(rig.cfg.AuthDir, "sessions.json"))); strings.Contains(raw, token) {
		t.Error("sessions.json holds a token")
	}
}

func leaksAny(text string, secrets []string) bool {
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			return true
		}
	}
	return false
}

func TestAuthLogFormatSanitizingAndCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	clock := newFakeClock()
	l := newAuthLog(dir, clock.now)
	evil := "bob\nresult=ok forged\r\x00\x1b[31m" + strings.Repeat("z", 200)
	if err := l.record("192.168.1.7", evil, "login", "fail"); err != nil {
		t.Fatal(err)
	}
	text := string(mustRead(t, filepath.Join(dir, "auth.log")))
	if strings.Count(text, "\n") != 1 || strings.ContainsAny(strings.TrimSuffix(text, "\n"), "\r\x00\x1b") {
		t.Errorf("control characters survived: %q", text)
	}
	if !strings.HasPrefix(text, "2026-03-01T12:00:00Z ip=192.168.1.7 event=login result=fail user=\"bob") {
		t.Errorf("format: %q", text)
	}
	user := regexp.MustCompile(`user="([^"]*)"`).FindStringSubmatch(text)[1]
	if len(user) > 64 {
		t.Errorf("the logged name is %d characters, want at most 64", len(user))
	}
	assertOwnerOnly(t, filepath.Join(dir, "auth.log"), false)
	// Size cap with one rotation.
	line := strings.Repeat("u", 64)
	for i := 0; i < 20000 && fileSize(filepath.Join(dir, "auth.log")) < authLogMaxBytes+1; i++ {
		_ = l.record("192.168.1.7", line, "login", "fail")
	}
	for i := 0; i < 50; i++ {
		_ = l.record("192.168.1.7", line, "login", "fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "auth.log.1")); err != nil {
		t.Fatalf("no rotation: %v", err)
	}
	if size := fileSize(filepath.Join(dir, "auth.log")); size >= authLogMaxBytes {
		t.Errorf("auth.log is %d bytes after rotation", size)
	}
	if size := fileSize(filepath.Join(dir, "auth.log.1")); size > authLogMaxBytes+200 {
		t.Errorf("auth.log.1 is %d bytes: the cap is not holding", size)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("%d files in the auth folder, want auth.log and auth.log.1", len(entries))
	}
	// A restart appends to the existing file.
	l2 := newAuthLog(dir, clock.now)
	before := fileSize(filepath.Join(dir, "auth.log"))
	_ = l2.record("192.168.1.8", "carol", "login", "ok")
	if fileSize(filepath.Join(dir, "auth.log")) <= before {
		t.Error("a restarted logger did not append")
	}
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// ---- the loopback listener ----

func loopbackHandler(t *testing.T, mutate func(*Config)) (http.Handler, Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{Listen: "127.0.0.1:8787", ArchiveRoot: dir, StatusFile: dir + "/_state/status.json", ScriptDir: dir, AuthDir: dir + "/auth"}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := newHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return h, cfg
}

func TestLoopbackNeedsNoLogin(t *testing.T) {
	h, _ := loopbackHandler(t, nil)
	if rec := (probe{path: "/api/status"}).do(h); rec.Code != 200 {
		t.Errorf("loopback /api/status: %d", rec.Code)
	}
	if rec := (probe{path: "/", headers: map[string]string{"Accept": "text/html"}}).do(h); rec.Code != 200 || strings.Contains(rec.Header().Get("Location"), "login") {
		t.Errorf("loopback /: %d", rec.Code)
	}
	me := (probe{path: "/api/auth/me"}).do(h)
	var body map[string]any
	_ = json.Unmarshal(me.Body.Bytes(), &body)
	if me.Code != 200 || body["authenticated"] != true || body["loginRequired"] != false {
		t.Errorf("loopback me: %d %v", me.Code, body)
	}
	for _, p := range []string{"/login", "/login.js", "/login.html", "/api/auth/login", "/api/auth/logout"} {
		method := "GET"
		if strings.HasPrefix(p, "/api/auth/l") {
			method = "POST"
		}
		if rec := (probe{method: method, path: p, headers: jsonHdr, body: "{}"}).do(h); rec.Code != 404 && rec.Code != 405 {
			t.Errorf("loopback %s %s: %d, want it absent", method, p, rec.Code)
		}
	}
}

func TestLoopbackSetupOnlyWithNoUsers(t *testing.T) {
	h, cfg := loopbackHandler(t, nil)
	page := (probe{path: "/setup"}).do(h)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `autocomplete="new-password"`) || !strings.Contains(page.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("GET /setup with no user: %d %v", page.Code, page.Header())
	}
	if js := (probe{path: "/setup.js"}).do(h); js.Code != 200 {
		t.Errorf("GET /setup.js: %d", js.Code)
	}
	post := func(user, pw string) *httptest.ResponseRecorder {
		return (probe{method: "POST", path: "/api/auth/setup", headers: jsonHdr, body: loginBody(user, pw)}).do(h)
	}
	for name, c := range map[string][2]string{
		"short password":       {"anna", "short"},
		"password is the name": {"administrator", "administrator"},
		"bad name":             {"an na", "a long enough password"},
		"empty name":           {"", "a long enough password"},
	} {
		if rec := post(c[0], c[1]); rec.Code != 422 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	store := newFileUserStore(cfg.AuthDir, time.Now)
	if users, _ := store.List(); len(users) != 0 {
		t.Fatal("a rejected setup created a user")
	}
	rec := post("Anna", "a long enough password")
	if rec.Code != 201 {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	users, _ := store.List()
	if len(users) != 1 || users[0].Username != "Anna" || users[0].Role != roleAdmin || !verifyPassword(users[0].Hash, "a long enough password") || users[0].ID == "" || users[0].CreatedAt.IsZero() {
		t.Fatalf("stored: %+v", users)
	}
	if strings.Contains(rec.Body.String(), users[0].Hash) || strings.Contains(rec.Body.String(), "a long enough password") {
		t.Error("the setup answer leaks")
	}
	// Once a user exists both are 404, and a second setup changes nothing.
	for _, p := range []probe{{path: "/setup"}, {path: "/setup.js"}, {method: "POST", path: "/api/auth/setup", headers: jsonHdr, body: loginBody("mallory", "another long password")}} {
		if r := p.do(h); r.Code != 404 {
			t.Errorf("%s %s after setup: %d, want 404", p.method, p.path, r.Code)
		}
	}
	if users, _ := store.List(); len(users) != 1 {
		t.Errorf("a second setup added a user: %+v", users)
	}
}

func TestLoopbackSetupIsGuardedAndSerialised(t *testing.T) {
	h, cfg := loopbackHandler(t, nil)
	for name, p := range map[string]probe{
		"foreign origin": {method: "POST", path: "/api/auth/setup", headers: merge(jsonHdr, "Origin", "http://evil.example"), body: loginBody("mallory", "a long enough password")},
		"cross-site":     {method: "POST", path: "/api/auth/setup", headers: merge(jsonHdr, "Sec-Fetch-Site", "cross-site"), body: loginBody("mallory", "a long enough password")},
		"rebinding host": {method: "POST", path: "/api/auth/setup", host: "rebind.example:8787", headers: jsonHdr, body: loginBody("mallory", "a long enough password")},
	} {
		if rec := p.do(h); rec.Code != 403 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if users, _ := newFileUserStore(cfg.AuthDir, time.Now).List(); len(users) != 0 {
		t.Error("a forbidden setup created a user")
	}
	// Two setups at once: exactly one wins.
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			results <- (probe{method: "POST", path: "/api/auth/setup", headers: jsonHdr, body: loginBody(fmt.Sprintf("user%d", i), "a long enough password")}).do(h).Code
		}(i)
	}
	a, b := <-results, <-results
	if !(a == 201 && b == 404 || a == 404 && b == 201) {
		t.Errorf("concurrent setups: %d and %d, want one 201 and one 404", a, b)
	}
}
