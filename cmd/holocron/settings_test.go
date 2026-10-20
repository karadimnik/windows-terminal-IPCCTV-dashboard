package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/osx"
	"holocron/internal/web"
)

// serveSettings runs the real web handler (loopback, Go system) over the rig's files with the
// holocron writer, and returns its base URL. The supervisor is the rig's fake control endpoint.
func serveSettings(t *testing.T, r *rig) string {
	t.Helper()
	plat := osx.Current()
	paths, cfg, _, err := app.Resolve(r.root, r.cfg, plat)
	if err != nil {
		t.Fatal(err)
	}
	lg, _ := app.NewLogger(app.LogOptions{Paths: paths, Config: cfg, Getenv: func(string) string { return "debug" }})
	t.Cleanup(func() { lg.Close() })
	e := &env{opts: app.Options{Root: r.root, ConfigPath: r.cfg, Platform: plat}, paths: paths, cfg: cfg, lg: lg, out: io.Discard, errw: io.Discard,
		dial: func(string) (ctl, error) { return r.ctl, nil }}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	o := web.Options{Config: web.Config{Listen: addr, ArchiveRoot: paths.Root, CamerasPath: paths.Cameras, ConfigPath: paths.Config,
		AuthDir: cfg.Web.AuthDir, TLSDir: t.TempDir()}, Logs: lg, GoSystem: true, Settings: webSettings{e: e}}
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

func put(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("PUT", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestWebSettingsWriteTheFilesAndReloadTheSupervisor(t *testing.T) {
	r := newRig(t)
	base := serveSettings(t, r)

	// no supervisor: saved, applies at the next start
	code, body := put(t, base+"/api/cameras", `{"cameras":[{"originalName":"Front","name":"Front door","ip":"cam.lan:8554","quality":"stream2","record":true},{"originalName":"Back","name":"Back","ip":"192.0.2.11","quality":"","record":false},{"name":"Side","ip":"192.0.2.12","quality":"stream1","record":true}]}`)
	if code != 200 || body["changed"] != float64(2) || !strings.Contains(body["message"].(string), "not running") {
		t.Fatalf("%d %v", code, body)
	}
	got, _ := os.ReadFile(r.cameras())
	if !bytes.HasPrefix(got, []byte("\xef\xbb\xbf")) || !strings.Contains(string(got), `"Note": "kept"`) || !strings.Contains(string(got), `"Name": "Front door"`) || !strings.Contains(string(got), `cam.lan:8554`) || !strings.Contains(string(got), `"Side"`) {
		t.Fatalf("cameras.json:\n%s", got)
	}
	if len(r.ctl.calls) != 0 {
		t.Errorf("reload without a supervisor: %v", r.ctl.calls)
	}

	// supervisor running: it is asked to reload
	r.running()
	code, body = put(t, base+"/api/config", `{"config":{"SegmentSeconds":600,"MaxRetentionDays":14}}`)
	if code != 200 || body["changed"] != float64(2) || !strings.Contains(body["message"].(string), "reloaded") {
		t.Fatalf("%d %v", code, body)
	}
	if strings.Join(r.ctl.calls, ",") != "reload" {
		t.Errorf("control calls %v", r.ctl.calls)
	}
	cfg, _ := os.ReadFile(r.cfg)
	for _, want := range []string{"\xef\xbb\xbf", `"SegmentSeconds": 600`, `"MaxRetentionDays": 14`, `"Unknown"`, `"HealthStripHeightPx": -1`, `"authDir"`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config lost %q:\n%s", want, cfg)
		}
	}
	// null unsets
	if code, _ = put(t, base+"/api/config", `{"config":{"MaxRetentionDays":null}}`); code != 200 {
		t.Fatal(code)
	}
	if cfg, _ = os.ReadFile(r.cfg); strings.Contains(string(cfg), "MaxRetentionDays") {
		t.Errorf("not unset:\n%s", cfg)
	}
	// nothing to change: nothing written, no reload
	r.ctl.calls = nil
	before, _ := os.ReadFile(r.cfg)
	code, body = put(t, base+"/api/config", `{"config":{"SegmentSeconds":600}}`)
	after, _ := os.ReadFile(r.cfg)
	if code != 200 || body["changed"] != nil || !bytes.Equal(before, after) || len(r.ctl.calls) != 0 {
		t.Errorf("no-op: %d %v %v", code, body, r.ctl.calls)
	}

	// the actions are in the audit stream, as web:loopback
	audit, _ := os.ReadFile(r.root + "/_logs/audit.log")
	for _, want := range []string{"action=web.cameras.put", "action=web.config.put", "actor=web:loopback"} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("audit lacks %s", want)
		}
	}
}

func TestWebSettingsRefusalsLeaveTheFilesByteIdentical(t *testing.T) {
	r := newRig(t)
	base := serveSettings(t, r)
	r.running()
	cams0, _ := os.ReadFile(r.cameras())
	cfg0, _ := os.ReadFile(r.cfg)
	for _, c := range []struct{ path, body, want string }{
		{"/api/cameras", `{"cameras":[{"originalName":"Front","name":"Front","ip":"192.0.2.10:70000","quality":"stream1"}]}`, "port"},
		{"/api/cameras", `{"cameras":[{"name":"A B","ip":"192.0.2.10","quality":"stream1"},{"name":"A_B","ip":"192.0.2.11","quality":"stream1"}]}`, "share the same folder"},
		{"/api/cameras", `{"cameras":[{"name":"con","ip":"192.0.2.10","quality":"stream1"}]}`, "reserved"},
		{"/api/cameras", `{"cameras":[{"name":"x","ip":"rtsp://192.0.2.10","quality":"stream1"}]}`, "host and an optional port"},
		{"/api/cameras", `{"cameras":[{"originalName":"Gone","name":"Gone","ip":"192.0.2.10","quality":"stream1"}]}`, "not in the file"},
		{"/api/config", `{"config":{"SegmentSeconds":5}}`, "between 60 and 3600"},
		{"/api/config", `{"config":{"ReserveGB":"lots"}}`, "number"},
		{"/api/config", `{"config":{"RootPath":"/x"}}`, "not an editable"},
		{"/api/config", `{"config":{"ReserveGB":30,"SegmentSeconds":5}}`, "attention"},
	} {
		code, body := put(t, base+c.path, c.body)
		if code != 422 || !strings.Contains(toJSON(body), c.want) {
			t.Errorf("%s %s: %d %v", c.path, c.body, code, body)
		}
		cams1, _ := os.ReadFile(r.cameras())
		cfg1, _ := os.ReadFile(r.cfg)
		if !bytes.Equal(cams0, cams1) || !bytes.Equal(cfg0, cfg1) {
			t.Errorf("%s: a file changed", c.body)
		}
	}
	if len(r.ctl.calls) != 0 {
		t.Errorf("a refusal reloaded the supervisor: %v", r.ctl.calls)
	}
	// a refusal is a WARN record, not an error
	if log := r.log(); strings.Contains(log, "ERROR") {
		t.Errorf("an ERROR record for a refusal:\n%s", log)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
