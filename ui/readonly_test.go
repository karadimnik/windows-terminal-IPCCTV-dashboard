package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type roRoute struct {
	method, path string
	headers      map[string]string
	body         string
	allowed      bool // may pass when the listener is view-only
}

var jsonHdr = map[string]string{"Content-Type": "application/json"}

// Every route the program registers, classified. TestEveryRouteIsClassified fails when a route appears that is not here.
var roRoutes = []roRoute{
	{"GET", "/", nil, "", true}, {"HEAD", "/", nil, "", true}, {"GET", "/index.html", nil, "", true}, {"GET", "/app.js", nil, "", true},
	{"GET", "/live.js", nil, "", true}, {"GET", "/style.css", nil, "", true}, {"GET", "/nothing-static", nil, "", true},
	{"GET", "/api/status", nil, "", true}, {"GET", "/api/live", nil, "", true}, {"GET", "/api/cameras", nil, "", true},
	{"GET", "/api/config", nil, "", true}, {"GET", "/api/footage", nil, "", true}, {"GET", "/api/clip?camera=x&segment=y", nil, "", true},
	{"GET", "/api/tls/ca.crt", nil, "", true}, {"GET", "/api/tls/info", nil, "", true},
	{"POST", "/api/live/Cam%201/whep?stream=sub", map[string]string{"Content-Type": "application/sdp"}, "v=0\r\n", true},
	{"DELETE", "/api/live/session/00000000000000000000000000000000", nil, "", true},
	// refused when view-only
	{"POST", "/api/control", jsonHdr, `{"scope":"all","action":"stop"}`, false},
	{"PUT", "/api/config", jsonHdr, `{"config":{}}`, false},
	{"PUT", "/api/cameras", jsonHdr, `{"cameras":[]}`, false},
	{"POST", "/api/cameras/Cam%201/record", jsonHdr, `{"record":false}`, false},
	{"POST", "/api/acknowledge", jsonHdr, `{"key":"x","at":"y"}`, false},
	// unknown routes and methods are refused too: deny by default
	{"GET", "/api/nothing", nil, "", false}, {"GET", "/api/live/x", nil, "", false}, {"GET", "/api/tls/ca.key", nil, "", false},
	{"POST", "/", jsonHdr, "{}", false}, {"PUT", "/", jsonHdr, "{}", false}, {"PATCH", "/api/config", jsonHdr, "{}", false},
	{"DELETE", "/api/cameras", nil, "", false}, {"DELETE", "/api/live/session/a/b", nil, "", false}, {"POST", "/api/live/whep", jsonHdr, "{}", false},
	{"POST", "/api/live/Cam%201/whep/x", jsonHdr, "{}", false}, {"OPTIONS", "/api/status", nil, "", false}, {"POST", "/api/tls/info", jsonHdr, "{}", false},
	{"GET", "/api/control", nil, "", false},
}

func isReadOnlyRefusal(body string) bool { return strings.Contains(body, readOnlyMessage) }

func TestReadOnlyRouteMethodTokenMatrix(t *testing.T) {
	cases := []struct {
		name     string
		readOnly *bool
	}{{"default (absent)", nil}, {"true", boolPtr(true)}, {"false", boolPtr(false)}}
	for _, mode := range cases {
		rig := newTLSRig(t, func(c *Config) { c.TLSReadOnly = mode.readOnly })
		view := mode.readOnly == nil || *mode.readOnly
		valid := cookieValue(tlsTestToken)
		for _, route := range roRoutes {
			base := tlsReq{method: route.method, path: route.path, headers: route.headers, body: route.body}
			// 401 beats 403: without a token the read-only gate is never reached, so it cannot be probed.
			for _, st := range []struct {
				name  string
				apply func(tlsReq) tlsReq
			}{
				{"no token", func(q tlsReq) tlsReq { return q }},
				{"wrong bearer", func(q tlsReq) tlsReq { q.headers = merge(q.headers, "Authorization", "Bearer nope"); return q }},
			} {
				rec := rig.do(st.apply(base))
				if rec.Code != 401 || isReadOnlyRefusal(rec.Body.String()) {
					t.Errorf("[%s] %s %s with %s: %d %q, want a plain 401", mode.name, route.method, route.path, st.name, rec.Code, rec.Body.String())
				}
			}
			q := base
			q.cookie = valid
			rec := rig.do(q)
			refused := rec.Code == http.StatusForbidden && isReadOnlyRefusal(rec.Body.String())
			switch {
			case view && !route.allowed && !refused:
				t.Errorf("[%s] %s %s with the right token: got %d, want the read-only 403", mode.name, route.method, route.path, rec.Code)
			case view && route.allowed && refused:
				t.Errorf("[%s] %s %s with the right token: refused as read-only but it must be allowed", mode.name, route.method, route.path)
			case !view && refused:
				t.Errorf("[%s] %s %s with the right token: read-only refusal although tlsReadOnly is false", mode.name, route.method, route.path)
			}
			if refused {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != readOnlyMessage || len(body) != 2 || len(rec.Body.String()) > 100 {
					t.Errorf("[%s] %s %s: the refusal body is not the generic JSON error: %q", mode.name, route.method, route.path, rec.Body.String())
				}
			}
		}
	}
}

func boolPtr(b bool) *bool { return &b }

func TestReadOnlyRefusalHappensBeforeAnyHandler(t *testing.T) {
	rig := newTLSRig(t, nil)
	ack := filepath.Join(filepath.Dir(rig.cfg.StatusFile), acknowledgedFileName)
	valid := cookieValue(tlsTestToken)
	for _, q := range []tlsReq{
		{method: "POST", path: "/api/acknowledge", headers: jsonHdr, body: `{"key":"stopNotice","at":"2026-01-01T00:00:00Z"}`},
		{method: "POST", path: "/api/control", headers: jsonHdr, body: `{"scope":"all","action":"stop"}`},
		{method: "PUT", path: "/api/config", headers: jsonHdr, body: `{"config":{"SegmentSeconds":300}}`},
	} {
		q.cookie = valid
		if rec := rig.do(q); rec.Code != 403 || !isReadOnlyRefusal(rec.Body.String()) {
			t.Fatalf("%s %s: %d", q.method, q.path, rec.Code)
		}
	}
	if _, err := os.Stat(ack); err == nil {
		t.Error("a refused acknowledgement wrote the sidecar")
	}
	if !strings.Contains(rig.logs.String(), "read-only listener") {
		t.Error("the refusal was not logged")
	}
	// The off switch really lets the same acknowledgement reach its handler.
	open := newTLSRig(t, func(c *Config) { c.TLSReadOnly = boolPtr(false) })
	ack2 := filepath.Join(filepath.Dir(open.cfg.StatusFile), acknowledgedFileName)
	rec := open.do(tlsReq{method: "POST", path: "/api/acknowledge", headers: map[string]string{"Content-Type": "application/json", "Origin": "https://" + lanHost}, body: `{"key":"stopNotice","at":"2026-01-01T00:00:00Z"}`, cookie: valid})
	if isReadOnlyRefusal(rec.Body.String()) {
		t.Errorf("tlsReadOnly=false still refused: %d", rec.Code)
	}
	_ = ack2
}

// routePattern finds the registrations in the source so a new route cannot appear unclassified.
var routePattern = regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) ([^"]+)"`)

func TestEveryRouteIsClassified(t *testing.T) {
	registered := map[string]bool{}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, _ := os.ReadFile(f)
		for _, m := range routePattern.FindAllStringSubmatch(string(data), -1) {
			registered[m[1]+" "+m[2]] = true
		}
	}
	if len(registered) < 15 {
		t.Fatalf("found only %d registered routes: the scan is broken", len(registered))
	}
	// Each registered pattern must have a classified example whose method matches and whose path fits the pattern.
	for pattern := range registered {
		method, path, _ := strings.Cut(pattern, " ")
		re := regexp.MustCompile("^" + regexp.QuoteMeta(path) + "$")
		if strings.Contains(path, "{") {
			re = regexp.MustCompile("^" + regexp.MustCompile(`\\\{[a-z]+\\\}`).ReplaceAllString(regexp.QuoteMeta(path), `[^/]+`) + "$")
		}
		if path == "/" {
			re = regexp.MustCompile(`^/`)
		}
		found := false
		for _, r := range roRoutes {
			p, _, _ := strings.Cut(r.path, "?")
			if r.method == method && re.MatchString(p) {
				found = true
			}
		}
		if !found {
			t.Errorf("route %q is registered but not classified in roRoutes: decide whether the view-only listener may serve it, then add it (allowed or refused) and update readOnlyAllowed", pattern)
		}
	}
	// Positive control: an unclassified pattern would be caught by the same logic.
	if readOnlyAllowed("POST", "/api/new-feature") || readOnlyAllowed("GET", "/api/new-feature") {
		t.Error("deny by default is broken: an unknown route is allowed")
	}
	// And the classification agrees with the gate.
	for _, r := range roRoutes {
		p, _, _ := strings.Cut(r.path, "?")
		if readOnlyAllowed(r.method, p) != r.allowed {
			t.Errorf("%s %s: readOnlyAllowed disagrees with the table", r.method, r.path)
		}
	}
}

func TestReadOnlyKeepsWatchingWorking(t *testing.T) {
	up := newFakeUpstream(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "live_view_secret.txt")
	_ = os.WriteFile(secret, []byte(testSecret), 0o600)
	writeTestJSON(t, filepath.Join(dir, "recorder.config.json"), map[string]any{"LiveViewEnabled": true, "LiveWhepAddress": up.address(), "LiveSecretPath": secret})
	cams := defaultCams()
	var liveCams []map[string]any
	var recCams []map[string]any
	for _, c := range cams {
		liveCams = append(liveCams, map[string]any{"Name": c.Name, "Sub": c.Sub, "Main": c.Main, "SubPath": c.SubPath, "MainPath": c.MainPath})
		recCams = append(recCams, map[string]any{"Name": c.Name, "Alive": true})
	}
	writeTestJSON(t, filepath.Join(dir, "status.json"), map[string]any{"State": "running", "SupervisorPid": os.Getpid(), "UpdatedAt": timeNowRFC(), "Cameras": recCams,
		"Live": map[string]any{"Enabled": true, "Alive": true, "Cameras": liveCams}})
	rig := newTLSRig(t, func(c *Config) { c.ScriptDir = dir; c.StatusFile = filepath.Join(dir, "status.json") })
	valid := cookieValue(tlsTestToken)
	rec := rig.do(tlsReq{method: "POST", path: "/api/live/Cam%20One/whep?stream=sub", headers: map[string]string{"Content-Type": "application/sdp", "Origin": "https://" + lanHost, "Sec-Fetch-Site": "same-origin"}, body: testOffer, cookie: valid})
	if rec.Code != 201 || rec.Header().Get("Content-Type") != "application/sdp" {
		t.Fatalf("WHEP while view-only: %d %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	del := rig.do(tlsReq{method: "DELETE", path: loc, headers: map[string]string{"Origin": "https://" + lanHost}, cookie: valid})
	if del.Code != 204 {
		t.Errorf("session DELETE while view-only: %d", del.Code)
	}
	if list := rig.do(tlsReq{path: "/api/live", cookie: valid}); list.Code != 200 || !strings.Contains(list.Body.String(), `"enabled":true`) {
		t.Errorf("GET /api/live while view-only: %d", list.Code)
	}
	if st := rig.do(tlsReq{path: "/api/status", cookie: valid}); st.Code != 200 {
		t.Errorf("GET /api/status while view-only: %d", st.Code)
	}
}

func TestReadOnlyFlagIsExposedOnlyOnTheTLSListener(t *testing.T) {
	valid := cookieValue(tlsTestToken)
	for _, c := range []struct {
		mode *bool
		want bool
	}{{nil, true}, {boolPtr(true), true}, {boolPtr(false), false}} {
		rig := newTLSRig(t, func(cfg *Config) { cfg.TLSReadOnly = c.mode })
		rec := rig.do(tlsReq{path: "/api/tls/info", cookie: valid})
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["readOnly"] != c.want {
			t.Errorf("mode %v: %s (%v)", c.mode, rec.Body.String(), err)
		}
	}
	// The loopback listener answers 404 for the route and its status carries no such field.
	handler, _ := guardHandler(t, nil)
	if rec := (probe{path: "/api/tls/info"}).do(handler); rec.Code != http.StatusNotFound {
		t.Errorf("loopback /api/tls/info: %d", rec.Code)
	}
	if rec := (probe{path: "/api/status"}).do(handler); strings.Contains(rec.Body.String(), "readOnly") {
		t.Error("the loopback status mentions readOnly")
	}
}

func TestLoopbackStillReachesTheWriteHandlers(t *testing.T) {
	handler, _ := guardHandler(t, nil)
	for _, p := range []probe{
		{method: "POST", path: "/api/control", headers: jsonHdr, body: `{"scope":"nothing","action":"nothing"}`},
		{method: "PUT", path: "/api/config", headers: jsonHdr, body: `{"config":{}}`},
		{method: "PUT", path: "/api/cameras", headers: jsonHdr, body: `{"cameras":[]}`},
		{method: "POST", path: "/api/cameras/x/record", headers: jsonHdr, body: `{}`},
		{method: "POST", path: "/api/acknowledge", headers: jsonHdr, body: `{}`},
	} {
		rec := p.do(handler)
		if isReadOnlyRefusal(rec.Body.String()) || rec.Code == http.StatusForbidden {
			t.Errorf("loopback %s %s was refused: %d %q", p.method, p.path, rec.Code, rec.Body.String())
		}
	}
}

func TestTLSReadOnlySettingSources(t *testing.T) {
	for _, c := range []struct {
		env  string
		want bool
	}{{"", true}, {"false", false}, {"0", false}, {"off", false}, {"true", true}, {"garbage", true}} {
		t.Setenv("CAMUI_TLS_READONLY", c.env)
		t.Setenv("CAMUI_TOKEN", "")
		cfg, err := LoadConfig("", Config{ScriptDir: t.TempDir()})
		if err != nil || cfg.TLSReadOnlyEffective() != c.want {
			t.Errorf("CAMUI_TLS_READONLY=%q: %v %v", c.env, cfg.TLSReadOnlyEffective(), err)
		}
	}
	t.Setenv("CAMUI_TLS_READONLY", "")
	path := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(path, []byte(`{"tlsReadOnly": false}`), 0o600)
	cfg, _ := LoadConfig(path, Config{ScriptDir: t.TempDir()})
	if cfg.TLSReadOnlyEffective() {
		t.Error("tlsReadOnly:false in the config file was ignored")
	}
}

func timeNowRFC() string { return time.Now().Format(time.RFC3339Nano) }
