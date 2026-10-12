package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goodToken = "0123456789abcdef" // exactly minTokenLength

func TestLoopbackGuard(t *testing.T) {
	cases := []struct {
		listen  string
		token   string
		wantErr bool
		why     string
	}{
		{"127.0.0.1:8787", "", false, "loopback IPv4 needs no token"},
		{"[::1]:8787", "", false, "loopback IPv6 needs no token"},
		{"localhost:8787", "", false, "localhost resolves to loopback only"},
		{"0.0.0.0:8787", "", true, "every IPv4 interface without a token must be refused"},
		{"[::]:8787", "", true, "every IPv6 interface without a token must be refused"},
		{":8787", "", true, "an empty host means every interface"},
		{"192.168.50.50:8787", "", true, "a LAN address without a token must be refused"},
		{"0.0.0.0:8787", "short", true, "a token shorter than the minimum is not a token"},
		{"0.0.0.0:8787", goodToken, false, "a real token makes the bind deliberate"},
		{"192.168.50.50:8787", goodToken, false, "a real token makes the bind deliberate"},
		{"127.0.0.1", "", true, "an address with no port is not usable"},
	}

	for _, tc := range cases {
		err := Config{Listen: tc.listen, Token: tc.token}.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("Validate(%q, token %d chars) allowed the bind; %s", tc.listen, len(tc.token), tc.why)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Validate(%q, token %d chars) refused the bind (%v); %s", tc.listen, len(tc.token), err, tc.why)
		}
	}
}

func TestGuardMessageNamesTheRemedy(t *testing.T) {
	err := Config{Listen: "0.0.0.0:8787"}.Validate()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"refusing to start", "0.0.0.0:8787", "127.0.0.1", "CAMUI_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message does not mention %q: %s", want, err)
		}
	}
}

func TestLoadConfigDefaultsAndPrecedence(t *testing.T) {
	cfg, err := LoadConfig("", Config{})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != defaultListen {
		t.Errorf("default listen = %q, want %q", cfg.Listen, defaultListen)
	}
	if !strings.HasSuffix(cfg.StatusFile, "status.json") || !strings.Contains(cfg.StatusFile, "_state") {
		t.Errorf("derived status file = %q", cfg.StatusFile)
	}

	cfg, err = LoadConfig("", Config{Listen: "127.0.0.1:9000", ArchiveRoot: "/tmp/archive"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("flag listen ignored: %q", cfg.Listen)
	}
	if !strings.HasPrefix(cfg.StatusFile, "/tmp/archive") {
		t.Errorf("status file not derived from the flag archive root: %q", cfg.StatusFile)
	}

	t.Setenv("CAMUI_TOKEN", goodToken)
	cfg, err = LoadConfig("", Config{})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Token != goodToken {
		t.Errorf("CAMUI_TOKEN ignored: %q", cfg.Token)
	}
}

func TestRequireToken(t *testing.T) {
	reached := false
	guarded := requireToken(goodToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	call := func(build func(*http.Request)) (int, bool) {
		reached = false
		request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		build(request)
		recorder := httptest.NewRecorder()
		guarded.ServeHTTP(recorder, request)
		return recorder.Code, reached
	}

	if code, got := call(func(*http.Request) {}); code != http.StatusUnauthorized || got {
		t.Errorf("no token: code %d, handler reached %v", code, got)
	}
	if code, got := call(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+goodToken) }); code != http.StatusOK || !got {
		t.Errorf("bearer token: code %d, handler reached %v", code, got)
	}
	if code, got := call(func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }); code != http.StatusUnauthorized || got {
		t.Errorf("wrong bearer token: code %d, handler reached %v", code, got)
	}
	if code, got := call(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: tokenCookie, Value: goodToken}) }); code != http.StatusOK || !got {
		t.Errorf("cookie token: code %d, handler reached %v", code, got)
	}

	// The query-parameter bootstrap sets a cookie and redirects, so the
	// token stops appearing in the address bar.
	request := httptest.NewRequest(http.MethodGet, "/?token="+goodToken, nil)
	recorder := httptest.NewRecorder()
	guarded.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("query bootstrap: code %d", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); strings.Contains(location, "token") {
		t.Errorf("redirect still carries the token: %q", location)
	}
	if cookies := recorder.Result().Cookies(); len(cookies) != 1 || cookies[0].Value != goodToken || !cookies[0].HttpOnly {
		t.Errorf("query bootstrap did not set an HttpOnly cookie: %v", recorder.Result().Cookies())
	}

	// With no token configured the middleware must be a pass-through.
	open := requireToken("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	recorder = httptest.NewRecorder()
	open.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("loopback with no token must not require one: code %d", recorder.Code)
	}
}
