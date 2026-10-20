package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"holocron/internal/cli"
)

// fakeSettings records what the handlers hand to the Go system's writer.
type fakeSettings struct {
	mu      sync.Mutex
	cameras [][]CameraPut
	configs []map[string]any
	paths   []string

	applied SettingsApplied
	invalid []FieldError
	err     error
}

func (f *fakeSettings) PutCameras(_ context.Context, path string, rows []CameraPut) (SettingsApplied, []FieldError, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cameras, f.paths = append(f.cameras, rows), append(f.paths, path)
	return f.applied, f.invalid, f.err
}

func (f *fakeSettings) PutConfig(_ context.Context, path string, values map[string]any) (SettingsApplied, []FieldError, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs, f.paths = append(f.configs, values), append(f.paths, path)
	return f.applied, f.invalid, f.err
}

func (f *fakeSettings) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cameras) + len(f.configs)
}

const goCameras = `[{"Name":"Front","IP":"192.0.2.10","Quality":"stream1","Record":true}]`

type goSettingsRig struct {
	api  *API
	mux  *http.ServeMux
	set  *fakeSettings
	logs *bytes.Buffer
	cams string
	conf string
}

func newGoSettingsRig(t *testing.T) *goSettingsRig {
	t.Helper()
	dir := t.TempDir()
	r := &goSettingsRig{set: &fakeSettings{}, logs: &bytes.Buffer{}, cams: filepath.Join(dir, "cameras.json"), conf: filepath.Join(dir, "holocron.config.json")}
	os.WriteFile(r.cams, []byte(goCameras), 0o600)
	os.WriteFile(r.conf, []byte(`{"SegmentSeconds":900,"ReserveGB":20}`), 0o600)
	log := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.api = NewAPI(Config{ArchiveRoot: dir, StatusFile: filepath.Join(dir, "_state", "status.json"), CamerasPath: r.cams, ConfigPath: r.conf}, log)
	newServeEnv([]serveOpt{withLaunch(nil, true), withSettings(r.set)}).apply(r.api)
	r.mux = http.NewServeMux()
	r.api.Routes(r.mux)
	return r
}

func (r *goSettingsRig) put(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func TestGoSettingsCamerasRoundTrip(t *testing.T) {
	r := newGoSettingsRig(t)
	r.set.applied = SettingsApplied{Changed: 1, Message: "Saved. The running recorder has reloaded."}
	rec := r.put("/api/cameras", `{"cameras":[{"originalName":"Front","name":"Front","ip":"cam.lan:8554","quality":"stream2","record":true},{"name":"Side","ip":"192.0.2.11","quality":"","record":false}]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var body CamerasResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.Ok || body.Changed != 1 || !strings.Contains(body.Message, "reloaded") || body.Path != r.cams {
		t.Fatalf("%v %+v", err, body)
	}
	if r.set.calls() != 1 || r.set.paths[0] != r.cams {
		t.Fatalf("writer calls %d paths %v", r.set.calls(), r.set.paths)
	}
	got := r.set.cameras[0]
	// host:port reaches the writer (the page's PowerShell rule would have refused it) and the rows keep their order
	if len(got) != 2 || got[0].IP != "cam.lan:8554" || got[0].OriginalName != "Front" || got[1].Name != "Side" {
		t.Errorf("rows %+v", got)
	}
	if strings.Contains(r.logs.String(), "settings written") {
		t.Error("the PowerShell writer's record appeared")
	}
	if !strings.Contains(r.logs.String(), "action=web.cameras.put") || !strings.Contains(r.logs.String(), "result=ok") {
		t.Errorf("no action record:\n%s", r.logs.String())
	}
}

func TestGoSettingsConfigRoundTrip(t *testing.T) {
	r := newGoSettingsRig(t)
	r.set.applied = SettingsApplied{Changed: 2, Message: "Saved."}
	rec := r.put("/api/config", `{"config":{"SegmentSeconds":600,"MaxRetentionDays":null}}`)
	var body ConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 || body.Changed != 2 || body.Path != r.conf || len(body.Editable) != 4 {
		t.Fatalf("%d %v %s", rec.Code, err, rec.Body.String())
	}
	if r.set.calls() != 1 || r.set.paths[0] != r.conf {
		t.Fatalf("calls %d paths %v", r.set.calls(), r.set.paths)
	}
	if v := r.set.configs[0]; v["SegmentSeconds"] != float64(600) || v["MaxRetentionDays"] != nil || len(v) != 2 {
		t.Errorf("values %+v", v)
	}
}

func TestGoSettingsRefusals(t *testing.T) {
	for _, c := range []struct {
		name, path, body string
		invalid          []FieldError
		err              error
		code             int
		want             string
	}{
		{"the page's name rule", "/api/cameras", `{"cameras":[{"name":"a/b","ip":"192.0.2.1","quality":"stream1"}]}`, nil, nil, 422, "need"},
		{"the page's stream rule", "/api/cameras", `{"cameras":[{"name":"ok","ip":"192.0.2.1","quality":"a b"}]}`, nil, nil, 422, "attention"},
		{"missing list", "/api/cameras", `{}`, nil, nil, 422, "cameras"},
		{"range", "/api/config", `{"config":{"SegmentSeconds":5}}`, nil, nil, 422, "attention"},
		{"not on the allow-list", "/api/config", `{"config":{"RootPath":"/x"}}`, nil, nil, 422, "not an editable"},
		{"empty config", "/api/config", `{"config":{}}`, nil, nil, 422, "at least one key"},
		{"writer says invalid", "/api/cameras", `{"cameras":[{"name":"ok","ip":"192.0.2.1:99999","quality":"stream1"}]}`, []FieldError{{Row: 0, Field: "ip", Message: "the port must be 1-65535"}}, nil, 422, "port must be"},
		{"writer refuses", "/api/config", `{"config":{"ReserveGB":30}}`, nil, cli.Refuse("ReserveGB=30: no"), 422, "Nothing was written"},
		{"writer fails", "/api/cameras", `{"cameras":[{"name":"ok","ip":"192.0.2.1","quality":"stream1"}]}`, nil, errors.New("disk full"), 500, "disk full"},
	} {
		r := newGoSettingsRig(t)
		r.set.invalid, r.set.err = c.invalid, c.err
		beforeC, _ := os.ReadFile(r.cams)
		beforeF, _ := os.ReadFile(r.conf)
		rec := r.put(c.path, c.body)
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
		afterC, _ := os.ReadFile(r.cams)
		afterF, _ := os.ReadFile(r.conf)
		if !bytes.Equal(beforeC, afterC) || !bytes.Equal(beforeF, afterF) {
			t.Errorf("%s: a file changed", c.name)
		}
		// the page's own refusals never reach the writer
		if c.invalid == nil && c.err == nil && r.set.calls() != 0 {
			t.Errorf("%s: the writer was called", c.name)
		}
		lvl := "result=refused"
		if c.code == 500 {
			lvl = "result=fail"
		}
		if !strings.Contains(r.logs.String(), lvl) {
			t.Errorf("%s: no %s record:\n%s", c.name, lvl, r.logs.String())
		}
	}
}

func TestGoSettingsWithoutAWriterIsStill501(t *testing.T) {
	r := newGoSettingsRig(t)
	r.api.settings = nil
	for _, c := range []struct{ path, body string }{
		{"/api/cameras", `{"cameras":[]}`}, {"/api/config", `{"config":{"ReserveGB":30}}`},
	} {
		if rec := r.put(c.path, c.body); rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "holocron config set") {
			t.Errorf("%s: %d %s", c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestGoSettingsOnTheLANListener(t *testing.T) {
	set := &fakeSettings{applied: SettingsApplied{Changed: 1, Message: "Saved."}}
	const body = `{"config":{"ReserveGB":30}}`
	json := map[string]string{"Content-Type": "application/json"}

	// read-only (the default): 401 without a session, 403 with one, the writer never called
	rig := newTLSRig(t, nil, withLaunch(nil, true), withSettings(set))
	for _, path := range []string{"/api/config", "/api/cameras"} {
		b := body
		if path == "/api/cameras" {
			b = `{"cameras":[]}`
		}
		if rec := rig.do(tlsReq{method: "PUT", path: path, headers: json, body: b}); rec.Code != 401 || isReadOnlyRefusal(rec.Body.String()) {
			t.Errorf("%s without a session: %d %s", path, rec.Code, rec.Body.String())
		}
		if rec := rig.do(tlsReq{method: "PUT", path: path, headers: json, body: b, cookie: rig.login(t)}); rec.Code != 403 || !isReadOnlyRefusal(rec.Body.String()) {
			t.Errorf("%s read-only: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if set.calls() != 0 {
		t.Fatal("the writer was called through a gate")
	}

	// full control: a signed-in user writes, as web:anna
	rig = newTLSRig(t, func(c *Config) {
		c.TLSReadOnly = boolPtr(false)
		c.ConfigPath = filepath.Join(c.ArchiveRoot, "holocron.config.json")
		os.WriteFile(c.ConfigPath, []byte(`{"ReserveGB":20}`), 0o600)
	}, withLaunch(nil, true), withSettings(set))
	if rec := rig.do(tlsReq{method: "PUT", path: "/api/config", headers: json, body: body}); rec.Code != 401 {
		t.Errorf("no session with full control: %d", rec.Code)
	}
	if set.calls() != 0 {
		t.Fatal("a request without a session reached the writer")
	}
	rec := rig.do(tlsReq{method: "PUT", path: "/api/config", headers: json, body: body, cookie: rig.login(t)})
	if rec.Code != 200 || set.calls() != 1 {
		t.Fatalf("signed in: %d %s (calls %d)", rec.Code, rec.Body.String(), set.calls())
	}
}
