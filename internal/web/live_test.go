package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake secret and every name that must never reach a browser. Fake values
// only: nothing here is a real credential, address or path.
const (
	testSecret  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testOffer   = "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\n"
	testAnswer  = "v=0\r\no=- 2 2 IN IP4 0.0.0.0\r\ns=-\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\n"
	testSession = "9d2e1c70-aaaa-bbbb-cccc-0123456789ab"
)

type upstreamRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

// fakeUpstream stands in for the live MediaMTX and records everything it is sent.
type fakeUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	got    []upstreamRequest
	// respond decides the answer; nil gives a normal WHEP 201.
	respond func(w http.ResponseWriter, r *http.Request)
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 1024)
		buf := bytes.NewBuffer(body)
		_, _ = buf.ReadFrom(r.Body)
		f.mu.Lock()
		f.got = append(f.got, upstreamRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: buf.String()})
		respond := f.respond
		f.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", r.URL.Path+"/"+testSession)
		w.Header().Set("Accept-Patch", "application/trickle-ice-sdpfrag")
		w.Header().Set("Link", `<stun:stun.example.invalid>; rel="ice-server"`)
		w.Header().Set("X-Upstream-Detail", "secret-internal-detail")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(testAnswer))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUpstream) address() string { return strings.TrimPrefix(f.server.URL, "http://") }

func (f *fakeUpstream) requests() []upstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstreamRequest(nil), f.got...)
}

type liveCam struct {
	Name, SubPath, MainPath string
	Sub, Main               bool
	Alive                   bool
	LastError               string
}

type liveOpts struct {
	enabled   bool
	alive     bool
	cams      []liveCam
	address   string // LiveWhepAddress
	secret    string // file contents; "-" writes no file
	heartbeat time.Duration
}

// liveRig is the real handler chain over a script directory holding a
// recorder.config.json, a status.json and a secret file.
type liveRig struct {
	handler http.Handler
	logs    *bytes.Buffer
	dir     string
}

func defaultCams() []liveCam {
	return []liveCam{
		{Name: "Cam One", SubPath: "camonesub", MainPath: "camonemain", Sub: true, Main: true, Alive: true},
		{Name: "Cam Two", SubPath: "", MainPath: "camtwomain", Sub: false, Main: true, Alive: true, LastError: "ffmpeg exited"},
	}
}

func newLiveRig(t *testing.T, o liveOpts) *liveRig {
	t.Helper()
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "live_view_secret.txt")
	if o.secret != "-" {
		if err := os.WriteFile(secretPath, []byte(o.secret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := map[string]any{"LiveViewEnabled": o.enabled, "LiveWhepAddress": o.address, "LiveSecretPath": secretPath}
	writeTestJSON(t, filepath.Join(dir, "recorder.config.json"), config)

	var cameras, liveCameras []map[string]any
	var paths []string
	for _, c := range o.cams {
		cameras = append(cameras, map[string]any{"Name": c.Name, "Alive": c.Alive, "RestartsLastHour": 2, "LastError": c.LastError})
		liveCameras = append(liveCameras, map[string]any{"Name": c.Name, "Sub": c.Sub, "Main": c.Main, "SubPath": c.SubPath, "MainPath": c.MainPath})
		paths = append(paths, c.SubPath, c.MainPath)
	}
	status := map[string]any{
		"State": "running", "SupervisorPid": os.Getpid(),
		"UpdatedAt": time.Now().Add(-o.heartbeat).Format(time.RFC3339Nano),
		"Cameras":   cameras,
	}
	if o.enabled {
		status["Live"] = map[string]any{"Enabled": true, "Alive": o.alive, "Paths": paths, "Cameras": liveCameras}
	}
	writeTestJSON(t, filepath.Join(dir, "status.json"), status)

	handler, logs := guardHandler(t, func(c *Config) { c.ScriptDir = dir; c.StatusFile = filepath.Join(dir, "status.json") })
	return &liveRig{handler: handler, logs: logs, dir: dir}
}

func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *liveRig) whep(camera, query string, h map[string]string, body string) *httptest.ResponseRecorder {
	headers := map[string]string{"Content-Type": "application/sdp"}
	for k, v := range h {
		headers[k] = v
	}
	return probe{method: http.MethodPost, path: "/api/live/" + camera + "/whep" + query, headers: headers, body: body}.do(r.handler)
}

func (r *liveRig) get(path string) *httptest.ResponseRecorder {
	return probe{path: path}.do(r.handler)
}

func okRig(t *testing.T, up *fakeUpstream) *liveRig {
	return newLiveRig(t, liveOpts{enabled: true, alive: true, cams: defaultCams(), address: up.address(), secret: testSecret})
}

func liveErrorText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not a JSON error: %v: %q", err, rec.Body.String())
	}
	return body.Error
}

// leaks lists the forbidden strings found in text.
func leaks(text string, forbidden ...string) []string {
	var found []string
	for _, f := range forbidden {
		if f != "" && strings.Contains(text, f) {
			found = append(found, f)
		}
	}
	return found
}

func TestLiveLeakScannerPositiveControl(t *testing.T) {
	// The leak assertions below mean nothing unless the scanner can see a leak.
	text := "oops " + testSecret + " via 127.0.0.1:8889 rtsp://x camonesub"
	if got := leaks(text, testSecret, "127.0.0.1:8889", "rtsp://", "camonesub"); len(got) != 4 {
		t.Fatalf("scanner found %v, want all four", got)
	}
}

func basicLogin(secret string) string {
	req, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	req.SetBasicAuth("camlive", secret)
	return req.Header.Get("Authorization")
}

func TestLiveWhepPassThrough(t *testing.T) {
	for _, c := range []struct{ query, path string }{
		{"?stream=sub", "/camonesub/whep"},
		{"?stream=main", "/camonemain/whep"},
	} {
		up := newFakeUpstream(t)
		rig := okRig(t, up)
		rec := rig.whep("Cam%20One", c.query, nil, testOffer)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: got %d %q, want 201", c.query, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/sdp" {
			t.Errorf("Content-Type %q, want application/sdp", ct)
		}
		if rec.Body.String() != testAnswer {
			t.Errorf("answer not passed through: %q", rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, "/api/live/session/") || len(loc) < len("/api/live/session/")+16 {
			t.Errorf("Location %q is not a minted session path", loc)
		}
		if leaks(loc, testSession, "camonesub", "camonemain", "whep") != nil {
			t.Errorf("Location %q carries upstream detail", loc)
		}
		reqs := up.requests()
		if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != c.path {
			t.Fatalf("upstream saw %+v, want one POST %s", reqs, c.path)
		}
		if reqs[0].Body != testOffer {
			t.Errorf("offer altered: %q", reqs[0].Body)
		}
		// Only the minimum comes back: no upstream header other than the type.
		for _, h := range []string{"X-Upstream-Detail", "Link", "Accept-Patch", "Server"} {
			if v := rec.Header().Get(h); v != "" {
				t.Errorf("upstream header %s returned: %q", h, v)
			}
		}
	}
}

func TestLiveUpstreamGetsOnlyTheMinimum(t *testing.T) {
	up := newFakeUpstream(t)
	rig := okRig(t, up)
	rec := rig.whep("Cam%20One", "?stream=sub", map[string]string{
		"Cookie": "camui_token=abc", "Authorization": "Bearer browser-token", "Referer": "http://127.0.0.1:8787/",
		"Origin": "http://127.0.0.1:8787", "Sec-Fetch-Site": "same-origin", "X-Forwarded-For": "10.1.2.3",
		"User-Agent": "BrowserUA/1", "Accept-Language": "en",
	}, testOffer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d", rec.Code)
	}
	h := up.requests()[0].Header
	// Connection: close is the transport's own (keep-alives are off), not something a browser sent.
	allowed := map[string]bool{"Authorization": true, "Content-Type": true, "Content-Length": true, "Connection": true}
	for k := range h {
		if !allowed[k] {
			t.Errorf("upstream received header %s: %v", k, h[k])
		}
	}
	if got, want := h.Get("Authorization"), basicLogin(testSecret); got != want {
		t.Errorf("upstream login %q, want camlive login", got)
	}
	if h.Get("Content-Type") != "application/sdp" {
		t.Errorf("upstream Content-Type %q", h.Get("Content-Type"))
	}
	if strings.Contains(h.Get("Authorization"), "browser-token") {
		t.Error("browser Authorization reached upstream")
	}
}

func TestLiveDeleteSession(t *testing.T) {
	up := newFakeUpstream(t)
	rig := okRig(t, up)
	loc := rig.whep("Cam%20One", "?stream=sub", nil, testOffer).Header().Get("Location")

	rec := probe{method: http.MethodDelete, path: loc}.do(rig.handler)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: got %d, want 204", rec.Code)
	}
	reqs := up.requests()
	if len(reqs) != 2 || reqs[1].Method != http.MethodDelete || reqs[1].Path != "/camonesub/whep/"+testSession {
		t.Fatalf("upstream saw %+v, want a DELETE of the stored session", reqs)
	}
	if reqs[1].Header.Get("Authorization") != basicLogin(testSecret) {
		t.Error("upstream DELETE carried no login")
	}
	if again := (probe{method: http.MethodDelete, path: loc}).do(rig.handler); again.Code != http.StatusNotFound {
		t.Errorf("second DELETE: got %d, want 404", again.Code)
	}
	if len(up.requests()) != 2 {
		t.Error("second DELETE reached upstream")
	}
}

func TestLiveClientCannotNameAnUpstream(t *testing.T) {
	up := newFakeUpstream(t)
	evil := newFakeUpstream(t)
	// The upstream answers with a Location pointing elsewhere: it must never be followed.
	up.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", evil.server.URL+"/steal")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(testAnswer))
	}
	rig := okRig(t, up)
	loc := rig.whep("Cam%20One", "?stream=sub", nil, testOffer).Header().Get("Location")
	if loc == "" || strings.Contains(loc, "steal") {
		t.Fatalf("Location %q", loc)
	}
	if rec := (probe{method: http.MethodDelete, path: loc}).do(rig.handler); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE: got %d, want 204", rec.Code)
	}
	for _, id := range []string{"http:%2F%2F" + evil.address() + "%2Fx", "..%2F..%2Fstatus", evil.address(), "0"} {
		probe{method: http.MethodDelete, path: "/api/live/session/" + id}.do(rig.handler)
	}
	if n := len(evil.requests()); n != 0 {
		t.Errorf("a foreign server received %d requests", n)
	}
}

func TestLiveGuardExceptionIsScopedToTheWhepRoute(t *testing.T) {
	up := newFakeUpstream(t)
	rig := okRig(t, up)
	cases := []struct {
		method, path, ctype string
		refused             bool
	}{
		{"POST", "/api/live/Cam%20One/whep?stream=sub", "application/sdp", false},
		{"POST", "/api/live/Cam%20One/whep?stream=sub", "application/sdp; charset=utf-8", false},
		{"POST", "/api/live/Cam%20One/whep?stream=sub", "text/plain", true},
		{"POST", "/api/live/Cam%20One/whep?stream=sub", "application/x-www-form-urlencoded", true},
		{"PUT", "/api/live/Cam%20One/whep?stream=sub", "application/sdp", true},
		{"PATCH", "/api/live/Cam%20One/whep", "application/sdp", true},
		{"DELETE", "/api/live/Cam%20One/whep", "application/sdp", true},
		{"POST", "/api/live/Cam%20One/whep/x", "application/sdp", true},
		{"POST", "/api/live/a%2Fb/whep", "application/sdp", true},
		{"POST", "/api/live//whep", "application/sdp", true},
		{"POST", "/api/live/whep", "application/sdp", true},
		{"DELETE", "/api/live/session/abc", "application/sdp", true},
		{"POST", "/api/acknowledge", "application/sdp", true},
		{"POST", "/api/control", "application/sdp", true},
		{"POST", "/api/cameras/Cam%20One/record", "application/sdp", true},
		{"PUT", "/api/cameras", "application/sdp", true},
		{"PUT", "/api/config", "application/sdp", true},
		{"POST", "/api/status", "application/sdp", true},
		// The JSON rule elsewhere is unchanged.
		{"POST", "/api/acknowledge", "application/json", false},
		{"PUT", "/api/config", "application/json", false},
	}
	for _, c := range cases {
		rec := probe{method: c.method, path: c.path, headers: map[string]string{"Content-Type": c.ctype}, body: testOffer}.do(rig.handler)
		if got := rec.Code == http.StatusUnsupportedMediaType; got != c.refused {
			t.Errorf("%s %s as %s: got %d, refused=%v want %v", c.method, c.path, c.ctype, rec.Code, got, c.refused)
		}
	}
}

func TestLiveGuardStillAppliesToLiveRoutes(t *testing.T) {
	up := newFakeUpstream(t)
	rig := okRig(t, up)
	cases := []struct {
		name    string
		host    string
		headers map[string]string
	}{
		{"rebinding host", "attacker-rebind.example:8787", nil},
		{"foreign origin", "", map[string]string{"Origin": "http://localhost:18789"}},
		{"cross-site fetch", "", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site fetch", "", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"null origin", "", map[string]string{"Origin": "null"}},
	}
	for _, c := range cases {
		headers := map[string]string{"Content-Type": "application/sdp"}
		for k, v := range c.headers {
			headers[k] = v
		}
		for _, p := range []probe{
			{method: "POST", path: "/api/live/Cam%20One/whep?stream=sub", host: c.host, headers: headers, body: testOffer},
			{method: "GET", path: "/api/live", host: c.host, headers: headers},
			{method: "DELETE", path: "/api/live/session/abc", host: c.host, headers: headers},
		} {
			if code := p.do(rig.handler).Code; code != http.StatusForbidden {
				t.Errorf("%s: %s %s got %d, want 403", c.name, p.method, p.path, code)
			}
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("%d requests reached the live instance", n)
	}
	// A same-origin browser request passes.
	ok := rig.whep("Cam%20One", "?stream=sub", map[string]string{"Origin": "http://127.0.0.1:8787", "Sec-Fetch-Site": "same-origin"}, testOffer)
	if ok.Code != http.StatusCreated {
		t.Errorf("same-origin request: got %d", ok.Code)
	}
}

func TestLiveBodyLimit(t *testing.T) {
	up := newFakeUpstream(t)
	rig := okRig(t, up)
	big := strings.Repeat("a", liveMaxOfferBytes+1)
	if rec := rig.whep("Cam%20One", "?stream=sub", nil, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("65537 bytes: got %d, want 413", rec.Code)
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("an oversize offer reached upstream (%d)", n)
	}
	exact := strings.Repeat("a", liveMaxOfferBytes)
	if rec := rig.whep("Cam%20One", "?stream=sub", nil, exact); rec.Code != http.StatusCreated {
		t.Errorf("65536 bytes: got %d, want 201", rec.Code)
	}
	if rec := rig.whep("Cam%20One", "?stream=sub", nil, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("empty offer: got %d, want 400", rec.Code)
	}
	json := rig.whep("Cam%20One", "?stream=sub", map[string]string{"Content-Type": "application/json"}, "{}")
	if json.Code != http.StatusUnsupportedMediaType {
		t.Errorf("JSON offer: got %d, want 415", json.Code)
	}
}

func TestLiveRequestValidation(t *testing.T) {
	up := newFakeUpstream(t)
	rig := okRig(t, up)
	cases := []struct {
		name, camera, query string
		want                int
		msg                 string
	}{
		{"unknown camera", "Nobody", "?stream=sub", 404, "unknown camera"},
		{"camera is case-insensitive", "cam%20one", "?stream=sub", 201, ""},
		{"no stream", "Cam%20One", "", 400, "stream must be sub or main"},
		{"bad stream", "Cam%20One", "?stream=both", 400, "stream must be sub or main"},
		{"stream with a path", "Cam%20One", "?stream=camonesub", 400, "stream must be sub or main"},
		{"stream twice", "Cam%20One", "?stream=sub&stream=main", 400, "stream must be sub or main"},
		{"camera without a sub path", "Cam%20Two", "?stream=sub", 404, "stream not available"},
		{"camera with a main path", "Cam%20Two", "?stream=main", 201, ""},
	}
	for _, c := range cases {
		rec := rig.whep(c.camera, c.query, nil, testOffer)
		if rec.Code != c.want {
			t.Errorf("%s: got %d %q, want %d", c.name, rec.Code, rec.Body.String(), c.want)
			continue
		}
		if c.msg != "" && liveErrorText(t, rec) != c.msg {
			t.Errorf("%s: error %q, want %q", c.name, liveErrorText(t, rec), c.msg)
		}
	}
	for _, r := range up.requests() {
		if strings.Contains(r.Path, "nobody") {
			t.Errorf("unknown camera reached upstream: %s", r.Path)
		}
	}
}

func TestLiveOffAndUnavailable(t *testing.T) {
	up := newFakeUpstream(t)
	cases := []struct {
		name string
		o    liveOpts
		want string
	}{
		{"flag off", liveOpts{enabled: false, cams: defaultCams(), address: up.address(), secret: testSecret}, "live view is off"},
		{"instance down", liveOpts{enabled: true, alive: false, cams: defaultCams(), address: up.address(), secret: testSecret}, "unavailable"},
		{"stale heartbeat", liveOpts{enabled: true, alive: true, cams: defaultCams(), address: up.address(), secret: testSecret, heartbeat: time.Hour}, "unavailable"},
	}
	for _, c := range cases {
		rig := newLiveRig(t, c.o)
		rec := rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
		if rec.Code != http.StatusServiceUnavailable || liveErrorText(t, rec) != c.want {
			t.Errorf("%s: got %d %q, want 503 %q", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("%d requests reached upstream", n)
	}
}

func TestLiveUpstreamFailuresAreMapped(t *testing.T) {
	cases := []struct {
		name    string
		respond func(w http.ResponseWriter, r *http.Request)
		want    int
		msg     string
	}{
		{"login refused", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad login camlive "+testSecret, 401) }, 502, "live view is misconfigured"},
		{"forbidden", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "forbidden 127.0.0.1:8889", 403) }, 502, "live view is misconfigured"},
		{"no such stream", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "path 'camonesub' not found", 404) }, 404, "stream not available"},
		{"bad offer", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "sdp: rtsp://u:p@10.9.9.9/x", 400) }, 400, "offer rejected"},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom rtsp://u:p@10.9.9.9/x camonesub", 500)
		}, 502, "live view error"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://evil.example/x", 302) }, 502, "live view error"},
		{"empty 201", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }, 502, "live view error"},
	}
	for _, c := range cases {
		up := newFakeUpstream(t)
		up.respond = c.respond
		rig := okRig(t, up)
		rec := rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
		if rec.Code != c.want || liveErrorText(t, rec) != c.msg {
			t.Errorf("%s: got %d %q, want %d %q", c.name, rec.Code, rec.Body.String(), c.want, c.msg)
		}
		all := rec.Body.String() + fmt.Sprint(rec.Header()) + rig.logs.String()
		if got := leaks(all, testSecret, up.address(), "10.9.9.9", "rtsp://", "camonesub", "evil.example", "bad login", "boom"); got != nil {
			t.Errorf("%s: leaked %v", c.name, got)
		}
	}
}

func TestLiveMisconfiguredLogsOnePerEpisode(t *testing.T) {
	up := newFakeUpstream(t)
	var refuse sync.Mutex
	deny := true
	up.respond = func(w http.ResponseWriter, r *http.Request) {
		refuse.Lock()
		d := deny
		refuse.Unlock()
		if d {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Location", r.URL.Path+"/"+testSession)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(testAnswer))
	}
	rig := okRig(t, up)
	count := func() int { return strings.Count(rig.logs.String(), "live view login was refused") }
	for i := 0; i < 3; i++ {
		rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
	}
	if count() != 1 {
		t.Fatalf("3 refusals logged %d lines, want 1:\n%s", count(), rig.logs)
	}
	refuse.Lock()
	deny = false
	refuse.Unlock()
	if rec := rig.whep("Cam%20One", "?stream=sub", nil, testOffer); rec.Code != 201 {
		t.Fatalf("recovery: got %d", rec.Code)
	}
	refuse.Lock()
	deny = true
	refuse.Unlock()
	rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
	if count() != 2 {
		t.Fatalf("a new episode logged %d lines in total, want 2", count())
	}
	if got := leaks(rig.logs.String(), testSecret, up.address(), basicLogin(testSecret)); got != nil {
		t.Errorf("log leaks %v", got)
	}
}

func TestLiveUpstreamDownAndTimeout(t *testing.T) {
	// Down: a port nothing listens on.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	rig := newLiveRig(t, liveOpts{enabled: true, alive: true, cams: defaultCams(), address: dead, secret: testSecret})
	rec := rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
	if rec.Code != http.StatusServiceUnavailable || liveErrorText(t, rec) != "unavailable" {
		t.Errorf("down: got %d %q, want 503 unavailable", rec.Code, rec.Body.String())
	}
	if got := leaks(rec.Body.String()+rig.logs.String(), dead, testSecret); got != nil {
		t.Errorf("down: leaked %v", got)
	}

	// Timeout: the upstream never answers within the limit.
	old := liveUpstreamTimeout
	liveUpstreamTimeout = 150 * time.Millisecond
	t.Cleanup(func() { liveUpstreamTimeout = old })
	up := newFakeUpstream(t)
	release := make(chan struct{})
	up.respond = func(w http.ResponseWriter, r *http.Request) { <-release }
	t.Cleanup(func() { close(release) })
	rig = okRig(t, up)
	started := time.Now()
	rec = rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
	if rec.Code != http.StatusGatewayTimeout || liveErrorText(t, rec) != "live view timed out" {
		t.Errorf("timeout: got %d %q, want 504", rec.Code, rec.Body.String())
	}
	if time.Since(started) > 3*time.Second {
		t.Errorf("timeout took %v", time.Since(started))
	}
}

func TestLiveSessionCaps(t *testing.T) {
	up := newFakeUpstream(t)
	var cams []liveCam
	for i := 0; i < 5; i++ {
		cams = append(cams, liveCam{Name: fmt.Sprintf("Cam %d", i), SubPath: fmt.Sprintf("c%dsub", i), MainPath: fmt.Sprintf("c%dmain", i), Sub: true, Main: true, Alive: true})
	}
	rig := newLiveRig(t, liveOpts{enabled: true, alive: true, cams: cams, address: up.address(), secret: testSecret})

	// Failed attempts hold no slot.
	up.respond = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }
	for i := 0; i < liveMaxSessionsPerCamera+2; i++ {
		rig.whep("Cam%200", "?stream=sub", nil, testOffer)
	}
	up.mu.Lock()
	up.respond = nil
	up.got = nil
	up.mu.Unlock()

	// Per camera.
	var first string
	for i := 0; i < liveMaxSessionsPerCamera; i++ {
		rec := rig.whep("Cam%200", "?stream=sub", nil, testOffer)
		if rec.Code != 201 {
			t.Fatalf("session %d: got %d", i, rec.Code)
		}
		if first == "" {
			first = rec.Header().Get("Location")
		}
	}
	over := rig.whep("Cam%200", "?stream=main", nil, testOffer)
	if over.Code != http.StatusTooManyRequests {
		t.Fatalf("session %d on one camera: got %d, want 429", liveMaxSessionsPerCamera+1, over.Code)
	}
	if n := len(up.requests()); n != liveMaxSessionsPerCamera {
		t.Errorf("a refused session reached upstream: %d requests", n)
	}
	// Freeing one makes room.
	probe{method: http.MethodDelete, path: first}.do(rig.handler)
	if rec := rig.whep("Cam%200", "?stream=sub", nil, testOffer); rec.Code != 201 {
		t.Errorf("after a DELETE: got %d, want 201", rec.Code)
	}

	// Total.
	for cam := 1; cam < 4; cam++ {
		for i := 0; i < liveMaxSessionsPerCamera; i++ {
			if rec := rig.whep(fmt.Sprintf("Cam%%20%d", cam), "?stream=sub", nil, testOffer); rec.Code != 201 {
				t.Fatalf("cam %d session %d: got %d", cam, i, rec.Code)
			}
		}
	}
	if rec := rig.whep("Cam%204", "?stream=sub", nil, testOffer); rec.Code != http.StatusTooManyRequests {
		t.Errorf("session %d overall: got %d, want 429", liveMaxSessions+1, rec.Code)
	}
}

func TestLiveSessionsExpire(t *testing.T) {
	old := liveSessionTTL
	liveSessionTTL = 200 * time.Millisecond
	t.Cleanup(func() { liveSessionTTL = old })
	up := newFakeUpstream(t)
	rig := okRig(t, up)
	loc := rig.whep("Cam%20One", "?stream=sub", nil, testOffer).Header().Get("Location")
	time.Sleep(300 * time.Millisecond)
	if rec := (probe{method: http.MethodDelete, path: loc}).do(rig.handler); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE after the time limit: got %d, want 404", rec.Code)
	}
}

func TestLiveSecretHandling(t *testing.T) {
	for _, c := range []struct{ name, secret string }{
		{"missing file", "-"},
		{"too short", "abcd"},
		{"upper case", strings.ToUpper(testSecret)},
		{"not hex", strings.Repeat("g", 64)},
		{"empty", ""},
	} {
		up := newFakeUpstream(t)
		rig := newLiveRig(t, liveOpts{enabled: true, alive: true, cams: defaultCams(), address: up.address(), secret: c.secret})
		rec := rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
		if rec.Code != 502 || liveErrorText(t, rec) != "live view is misconfigured" {
			t.Errorf("%s: got %d %q, want 502 misconfigured", c.name, rec.Code, rec.Body.String())
		}
		if n := len(up.requests()); n != 0 {
			t.Errorf("%s: %d requests reached upstream without a valid login", c.name, n)
		}
		if c.secret != "" && c.secret != "-" && strings.Contains(rig.logs.String()+rec.Body.String(), c.secret) {
			t.Errorf("%s: the bad secret was echoed", c.name)
		}
	}
	// A trailing newline from a hand edit is tolerated; the login is still the 64 characters.
	up := newFakeUpstream(t)
	rig := newLiveRig(t, liveOpts{enabled: true, alive: true, cams: defaultCams(), address: up.address(), secret: testSecret + "\r\n"})
	if rec := rig.whep("Cam%20One", "?stream=sub", nil, testOffer); rec.Code != 201 {
		t.Fatalf("trailing newline: got %d", rec.Code)
	}
	if up.requests()[0].Header.Get("Authorization") != basicLogin(testSecret) {
		t.Error("the newline became part of the password")
	}
	// The secret is read at use: replaced on disk, the next request uses the new one.
	other := strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(rig.dir, "live_view_secret.txt"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
	if got := up.requests()[1].Header.Get("Authorization"); got != basicLogin(other) {
		t.Errorf("a rotated secret was not picked up: %q", got)
	}
}

func TestLiveRefusesANonLoopbackUpstream(t *testing.T) {
	for _, addr := range []string{"192.0.2.10:8889", "example.invalid:8889", "0.0.0.0:8889", "", "nonsense", "http://127.0.0.1:8889"} {
		up := newFakeUpstream(t)
		rig := newLiveRig(t, liveOpts{enabled: true, alive: true, cams: defaultCams(), address: addr, secret: testSecret})
		if addr == "" {
			// The default is the documented loopback address; nothing listens there in the test, so only the mapping is checked.
			rec := rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
			if rec.Code == http.StatusBadGateway && liveErrorText(t, rec) == "live view is misconfigured" {
				t.Errorf("empty address should default to 127.0.0.1:8889, got misconfigured")
			}
			continue
		}
		rec := rig.whep("Cam%20One", "?stream=sub", nil, testOffer)
		if rec.Code != 502 || liveErrorText(t, rec) != "live view is misconfigured" {
			t.Errorf("%q: got %d %q, want 502 misconfigured (the login travels in clear HTTP)", addr, rec.Code, rec.Body.String())
		}
		if len(up.requests()) != 0 {
			t.Errorf("%q: reached upstream", addr)
		}
	}
}

func TestLiveListEndpoint(t *testing.T) {
	up := newFakeUpstream(t)
	rig := okRig(t, up)
	rec := rig.get("/api/live")
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	var body struct {
		Enabled            bool `json:"enabled"`
		Alive              bool `json:"alive"`
		HiddenGraceSeconds int  `json:"hiddenGraceSeconds"`
		Cameras            []struct {
			Name      string `json:"name"`
			Sub       bool   `json:"sub"`
			Main      bool   `json:"main"`
			CamAlive  bool   `json:"camAlive"`
			Restarts  int    `json:"restarts"`
			LastError string `json:"lastError"`
		} `json:"cameras"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.HiddenGraceSeconds != 300 {
		t.Errorf("hiddenGraceSeconds = %d, want the default 300", body.HiddenGraceSeconds)
	}
	if !body.Enabled || !body.Alive || len(body.Cameras) != 2 {
		t.Fatalf("unexpected body %s", rec.Body)
	}
	one, two := body.Cameras[0], body.Cameras[1]
	if one.Name != "Cam One" || !one.Sub || !one.Main || !one.CamAlive || one.Restarts != 2 {
		t.Errorf("camera one: %+v", one)
	}
	if two.Name != "Cam Two" || two.Sub || !two.Main || two.LastError != "ffmpeg exited" {
		t.Errorf("camera two: %+v", two)
	}
	if got := leaks(rec.Body.String(), "camonesub", "camonemain", "camtwomain", up.address(), "127.0.0.1", "://", testSecret, "SubPath", "MainPath"); got != nil {
		t.Errorf("/api/live leaks %v: %s", got, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q", ct)
	}

	// Off: enabled false, no cameras, nothing to attach tiles to.
	off := newLiveRig(t, liveOpts{enabled: false, cams: defaultCams(), address: up.address(), secret: testSecret})
	var offBody struct {
		Enabled bool  `json:"enabled"`
		Alive   bool  `json:"alive"`
		Cameras []any `json:"cameras"`
	}
	rec = off.get("/api/live")
	if err := json.Unmarshal(rec.Body.Bytes(), &offBody); err != nil || offBody.Enabled || offBody.Alive || offBody.Cameras == nil || len(offBody.Cameras) != 0 {
		t.Errorf("flag off: %s (%v)", rec.Body, err)
	}
}

func TestLiveListScrubsAndStatusDropsPaths(t *testing.T) {
	up := newFakeUpstream(t)
	cams := defaultCams()
	cams[0].LastError = "open rtsp://u:pw@10.1.1.1/stream2 failed"
	rig := newLiveRig(t, liveOpts{enabled: true, alive: true, cams: cams, address: up.address(), secret: testSecret})
	list := rig.get("/api/live").Body.String()
	status := rig.get("/api/status").Body.String()
	for name, text := range map[string]string{"/api/live": list, "/api/status": status} {
		if got := leaks(text, "rtsp://", "10.1.1.1", ":pw@"); got != nil {
			t.Errorf("%s leaks %v", name, got)
		}
	}
	if got := leaks(status, "camonesub", "camonemain", "camtwomain", "SubPath", "MainPath", `"Paths"`); got != nil {
		t.Errorf("/api/status still carries the live path names: %v", got)
	}
	var parsed struct {
		Status struct {
			Live struct {
				Alive   bool
				Cameras []struct{ Name string }
			}
		}
	}
	if err := json.Unmarshal([]byte(status), &parsed); err != nil || !parsed.Status.Live.Alive || len(parsed.Status.Live.Cameras) != 2 {
		t.Errorf("/api/status lost the rest of the Live section: %v %s", err, status)
	}
}
