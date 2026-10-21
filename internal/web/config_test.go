package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if cfg.StatusFile != filepath.Join("/tmp/archive", "_state", "status.json") {
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

// TestArchiveRootPrecedence pins the three sources of the archive root in
// order. The middle one is the point: the recorder owns RootPath, and a UI that
// ignored it showed an empty archive with nothing to say why when the Operator
// moved it.
func TestArchiveRootPrecedence(t *testing.T) {
	const recorderRoot = `R:\recorded`

	// scriptDir returns a directory holding a recorder.config.json with the
	// given body, or no such file at all when body is empty.
	scriptDir := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "recorder.config.json"), []byte(body), 0o644); err != nil {
				t.Fatalf("write recorder.config.json: %v", err)
			}
		}
		return dir
	}

	// camuiConfig returns the path to a camui config file with the given body.
	camuiConfig := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "camui.config.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write camui.config.json: %v", err)
		}
		return path
	}

	cases := []struct {
		name       string
		recorder   string
		configPath string
		flags      Config
		want       string
	}{
		{
			name:     "RootPath is used when nothing explicit is set",
			recorder: `{"RootPath": "R:\\recorded", "SegmentSeconds": 900}`,
			want:     recorderRoot,
		},
		{
			name:     "a UTF-8 BOM does not hide RootPath",
			recorder: "\ufeff" + `{"RootPath": "R:\\recorded"}`,
			want:     recorderRoot,
		},
		{
			name:     "an explicit flag beats RootPath",
			recorder: `{"RootPath": "R:\\recorded"}`,
			flags:    Config{ArchiveRoot: `F:\flagged`},
			want:     `F:\flagged`,
		},
		{
			name:       "an explicit config file beats RootPath",
			recorder:   `{"RootPath": "R:\\recorded"}`,
			configPath: `{"archiveRoot": "C:\\configured"}`,
			want:       `C:\configured`,
		},
		{
			name: "no recorder config at all falls back to the constant",
			want: defaultArchiveRoot,
		},
		{
			name:     "a recorder config with no RootPath falls back to the constant",
			recorder: `{"SegmentSeconds": 900}`,
			want:     defaultArchiveRoot,
		},
		{
			name:     "a blank RootPath falls back to the constant",
			recorder: `{"RootPath": "   "}`,
			want:     defaultArchiveRoot,
		},
		{
			name:     "a malformed recorder config falls back to the constant rather than refusing to start",
			recorder: `{"RootPath": "R:\\recorded"`,
			want:     defaultArchiveRoot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := tc.flags
			flags.ScriptDir = scriptDir(t, tc.recorder)

			path := ""
			if tc.configPath != "" {
				path = camuiConfig(t, tc.configPath)
			}

			cfg, err := LoadConfig(path, flags)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.ArchiveRoot != tc.want {
				t.Errorf("archive root = %q, want %q", cfg.ArchiveRoot, tc.want)
			}
			// The status file is derived, so getting the root right is only
			// useful if it carries through.
			if wantStatus := filepath.Join(tc.want, "_state", "status.json"); cfg.StatusFile != wantStatus {
				t.Errorf("status file = %q, want %q", cfg.StatusFile, wantStatus)
			}
		})
	}
}

// TestArchiveRootDoesNotOverrideAnExplicitStatusFile keeps the two independent:
// an Operator who names a status file gets that file whatever the archive root
// resolves to.
func TestArchiveRootDoesNotOverrideAnExplicitStatusFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "recorder.config.json"), []byte(`{"RootPath": "R:\\recorded"}`), 0o644); err != nil {
		t.Fatalf("write recorder.config.json: %v", err)
	}

	cfg, err := LoadConfig("", Config{ScriptDir: dir, StatusFile: `S:\elsewhere\status.json`})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ArchiveRoot != `R:\recorded` {
		t.Errorf("archive root = %q, want the recorder's RootPath", cfg.ArchiveRoot)
	}
	if cfg.StatusFile != `S:\elsewhere\status.json` {
		t.Errorf("status file = %q, want the explicit one", cfg.StatusFile)
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

func TestLiveHiddenGraceSetting(t *testing.T) {
	cfg, err := LoadConfig("", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.LiveHiddenGraceEffective(); got != 300 {
		t.Errorf("default grace = %d, want 300", got)
	}
	file := filepath.Join(t.TempDir(), "camui.config.json")
	for text, want := range map[string]int{`{"liveHiddenGraceSeconds": 20}`: 20, `{"liveHiddenGraceSeconds": 0}`: 0, `{"liveHiddenGraceSeconds": -1}`: -1,
		`{"liveHiddenGraceSeconds": -50}`: -1, `{"liveHiddenGraceSeconds": 99999999}`: 86400} {
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(file, Config{})
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.LiveHiddenGraceEffective(); got != want {
			t.Errorf("%s -> %d, want %d", text, got, want)
		}
	}
	t.Setenv("CAMUI_LIVE_HIDDEN_GRACE", "45")
	cfg, err = LoadConfig(file, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.LiveHiddenGraceEffective(); got != 45 {
		t.Errorf("env did not win: %d", got)
	}
	t.Setenv("CAMUI_LIVE_HIDDEN_GRACE", "soon")
	cfg, _ = LoadConfig("", Config{})
	if got := cfg.LiveHiddenGraceEffective(); got != 300 {
		t.Errorf("an unparsable env value must fall back to the default, got %d", got)
	}
}
