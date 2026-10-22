package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/osx"
	"holocron/internal/web"
)

// serveAdmin runs the real web app (loopback: the box owner, an admin) over the rig's files with
// the holocron Admin tools, logging at TRACE into the rig's holocron.log and audit.log.
func serveAdmin(t *testing.T, r *rig) string {
	t.Helper()
	plat := osx.Current()
	paths, cfg, _, err := app.Resolve(r.root, r.cfg, plat)
	if err != nil {
		t.Fatal(err)
	}
	lg, _ := app.NewLogger(app.LogOptions{Paths: paths, Config: cfg, Getenv: func(string) string { return "trace" }})
	t.Cleanup(func() { lg.Close() })
	e := &env{opts: app.Options{Root: r.root, ConfigPath: r.cfg, Platform: plat, Getenv: func(k string) string { return r.extraEnv[k] }}, paths: paths, cfg: cfg, lg: lg,
		out: io.Discard, errw: io.Discard, auto: *r.auto, inspector: r.inspector(), doctor: r.doctorSeams(), dial: func(string) (ctl, error) { return r.ctl, nil }}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	o := web.Options{Config: web.Config{Listen: addr, ArchiveRoot: paths.Root, CamerasPath: paths.Cameras, ConfigPath: paths.Config,
		AuthDir: cfg.Web.AuthDir, TLSDir: filepath.Join(r.base(), "tls")}, Logs: lg, GoSystem: true, Settings: webSettings{e: e}, Admin: newWebAdmin(e)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- web.Run(ctx, o) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			c.Close()
			return "http://" + addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the web app did not start")
	return ""
}

type answers struct {
	t      *testing.T
	base   string
	bodies []string
}

func (a *answers) do(method, path, body string) (int, map[string]any) {
	a.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	a.bodies = append(a.bodies, string(b))
	var out map[string]any
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestWebAdminRunsTheCommandHelpers(t *testing.T) {
	r := newRig(t)
	r.useFileSecrets()
	a := &answers{t: t, base: serveAdmin(t, r)}

	// settings keys: the allow-list with values, defaults and rules
	code, body := a.do("GET", "/api/admin/config", "")
	keys, _ := body["keys"].([]any)
	found := map[string]map[string]any{}
	for _, k := range keys {
		m := k.(map[string]any)
		found[m["key"].(string)] = m
	}
	if code != 200 || found["SegmentSeconds"]["value"] != float64(60) || found["SegmentSeconds"]["set"] != true || found["SegmentSeconds"]["default"] != float64(900) {
		t.Fatalf("keys: %d %v", code, found["SegmentSeconds"])
	}
	if found["secrets.file"] == nil || found["secrets.file"]["warning"] == nil {
		t.Errorf("secrets.file: %v", found["secrets.file"])
	}
	before, _ := os.ReadFile(r.cfg)
	if code, body := a.do("POST", "/api/admin/config/set", `{"key":"segmentseconds","value":"600"}`); code != 200 || !strings.Contains(body["message"].(string), "SegmentSeconds: 60 -> 600") {
		t.Errorf("set: %d %v", code, body)
	}
	if cfg, _ := os.ReadFile(r.cfg); !strings.Contains(string(cfg), `"SegmentSeconds": 600`) || !strings.Contains(string(cfg), `"Unknown"`) || !strings.HasPrefix(string(cfg), "\xef\xbb\xbf") {
		t.Errorf("the file after set: %s", cfg)
	}
	if code, body := a.do("POST", "/api/admin/config/set", `{"key":"SegmentSeconds","value":"0"}`); code != 422 || body["invalid"] == nil {
		t.Errorf("invalid value: %d %v", code, body)
	}
	if code, body := a.do("POST", "/api/admin/config/set", `{"key":"RootPath","value":"/elsewhere"}`); code != 409 || !strings.Contains(body["error"].(string), "not a key") {
		t.Errorf("not editable: %d %v", code, body)
	}
	if code, _ := a.do("POST", "/api/admin/config/unset", `{"key":"SegmentSeconds"}`); code != 200 {
		t.Errorf("unset: %d", code)
	}
	if cfg, _ := os.ReadFile(r.cfg); strings.Contains(string(cfg), "SegmentSeconds") || string(cfg) == string(before) {
		t.Errorf("the file after unset: %s", cfg)
	}

	// camera login: stored in the file store, shown only as present
	const camUser, camPass = "cam-admin-7f3a", "Cam-Secret-9c41-Never-Shown"
	if _, _, code := r.run("secret", "list"); code != 0 {
		t.Fatal("secret list")
	}
	if _, body := a.do("GET", "/api/admin/camera-login", ""); body["cameraLogin"].(map[string]any)["password"] != false {
		t.Errorf("before: %v", body)
	}
	if code, body := a.do("POST", "/api/admin/camera-login", `{"user":"`+camUser+`","password":"`+camPass+`"}`); code != 200 || !strings.Contains(body["message"].(string), "stored camera.password (file store)") {
		t.Fatalf("set the login: %d %v", code, body)
	}
	code, body = a.do("GET", "/api/admin/camera-login", "")
	st := body["cameraLogin"].(map[string]any)
	if code != 200 || st["user"] != true || st["password"] != true || st["backend"] != "file" {
		t.Errorf("after: %d %v", code, body)
	}
	if out, _, _ := r.run("--json", "secret", "list"); !strings.Contains(out, "camera.password") {
		t.Errorf("the CLI does not see it: %s", out)
	}

	// users: a viewer added from the page, its password never back
	const userPass = "a viewer password from the page"
	if code, _ := a.do("POST", "/api/admin/users/add", `{"username":"bob","password":"`+userPass+`","role":"viewer"}`); code != 200 {
		t.Errorf("add: %d", code)
	}
	if out, _, _ := r.run("user", "list"); !strings.Contains(out, "viewer") {
		t.Errorf("the CLI does not see bob: %s", out)
	}

	// maintenance and autostart
	if code, body := a.do("GET", "/api/admin/prune", ""); code != 200 || body["prune"].(map[string]any)["dryRun"] != true {
		t.Errorf("prune: %d %v", code, body)
	}
	if code, body := a.do("GET", "/api/admin/archive", ""); code != 200 || body["archive"] == nil {
		t.Errorf("archive: %d %v", code, body)
	}
	if code, body := a.do("POST", "/api/admin/log/level", `{"component":"web","level":"debug"}`); code != 500 || !strings.Contains(body["error"].(string), "not running") {
		t.Errorf("level with no supervisor: %d %v", code, body)
	}
	r.running()
	if code, body := a.do("POST", "/api/admin/log/level", `{"component":"web","level":"debug"}`); code != 200 || !strings.Contains(body["message"].(string), "running supervisor") {
		t.Errorf("level: %d %v", code, body)
	}
	if code, body := a.do("GET", "/api/admin/log", ""); code != 200 || body["levels"].(map[string]any)["running"] != true {
		t.Errorf("levels: %d %v", code, body)
	}
	if code, body := a.do("GET", "/api/admin/autostart", ""); code != 200 || body["autostart"] == nil {
		t.Errorf("autostart: %d %v", code, body)
	}
	if code, body := a.do("POST", "/api/admin/autostart", `{"enabled":true}`); code != 409 || !strings.Contains(body["error"].(string), "holocron install") {
		t.Errorf("enable with nothing installed: %d %v", code, body)
	}

	// every change is an Action in holocron.log and in the audit stream
	audit, _ := os.ReadFile(filepath.Join(r.root, "_logs", "audit.log"))
	for _, name := range []string{web.ActionAdminConfigSet, web.ActionAdminConfigUnset, web.ActionAdminSecretSet, web.ActionAdminUsersAdd, web.ActionAdminLogLevel, web.ActionAdminSecretStatus} {
		if !strings.Contains(r.log(), "action="+name) || !strings.Contains(string(audit), "action="+name) {
			t.Errorf("%s: missing from holocron.log or audit.log", name)
		}
	}
	if !strings.Contains(string(audit), "actor=web:loopback") {
		t.Error("the actor is not web:loopback")
	}

	// the leak scan: answers, holocron.log at TRACE, audit.log, and every file of the rig (the store
	// holds ciphertext); positive control: a planted copy is found
	secrets := []string{camUser, camPass, userPass}
	logs := []string{r.log(), string(audit)}
	for _, s := range secrets {
		for _, text := range append(append([]string{}, a.bodies...), logs...) {
			if strings.Contains(text, s) {
				t.Errorf("%q leaked: %.300s", s, text)
			}
		}
		if hits := r.leaks(s); len(hits) > 0 {
			t.Errorf("%q is in %v", s, hits)
		}
	}
	if strings.Contains(strings.Join(a.bodies, ""), "pbkdf2") {
		t.Error("a password hash is in an answer")
	}
	os.WriteFile(filepath.Join(r.root, "_logs", "planted.log"), []byte("x "+camPass+" y"), 0o644)
	if hits := r.leaks(camPass); len(hits) != 1 {
		t.Errorf("the positive control found %v", hits)
	}
}
