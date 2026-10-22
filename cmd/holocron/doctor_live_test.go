package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/osx"
	"holocron/internal/relay"
	"holocron/internal/status"
	"holocron/internal/web"
)

var (
	secretA = strings.Repeat("ab", 32)
	secretB = strings.Repeat("cd", 32)
)

// liveServer is a live instance's WHEP server: OPTIONS /frontmain/whep wants camlive:<accept>.
func liveServer(t *testing.T, accept string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodOptions || req.URL.Path != "/frontmain/whep" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if req.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("camlive:"+accept)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *rig) liveRunning() {
	r.runningWith(func(d *status.Document) {
		pid := int64(4343)
		d.Live = &status.Live{Enabled: true, Alive: true, Pid: &pid, Cameras: []status.LiveCamera{{Name: "Front", Main: true, MainPath: "frontmain", Sub: true, SubPath: "frontsub"}}}
	})
}

func TestDoctorLiveGroup(t *testing.T) {
	for _, c := range []struct {
		name   string
		accept string
		mod    func(d *status.Document)
		want   []string
	}{
		{"healthy", secretA, nil, []string{
			"live.process ok   live instance  alive pid 4343",
			"live.login ok   live login     the live instance accepts the web role's login on frontmain (the same file)",
			"live.paths ok   Front          live path(s) frontsub, frontmain present"}},
		{"old secret in the instance", secretB, nil, []string{"live.login fail live login     live login refused, although the web role and the supervisor both use"}},
		{"instance down", secretA, func(d *status.Document) { d.Live.Alive, d.Live.LastError = false, "exit 1" }, []string{"live.process fail live instance  not alive - exit 1"}},
		{"sub path missing", secretA, func(d *status.Document) { d.Live.Cameras[0].Sub = false }, []string{"live.paths fail Front          live path(s) frontsub missing"}},
	} {
		r := newRig(t)
		srv := liveServer(t, c.accept)
		r.setConfig(`"LiveViewEnabled": true, "LiveWhepAddress": "` + strings.TrimPrefix(srv.URL, "http://") + `",`)
		os.WriteFile(filepath.Join(r.platformAuth(), "live_view_secret.txt"), []byte(secretA), 0o600)
		r.liveRunning()
		if c.mod != nil {
			r.runningWith(func(d *status.Document) {
				pid := int64(4343)
				d.Live = &status.Live{Enabled: true, Alive: true, Pid: &pid, Cameras: []status.LiveCamera{{Name: "Front", Main: true, MainPath: "frontmain", Sub: true, SubPath: "frontsub"}}}
				c.mod(d)
			})
		}
		r.doctorTools(func(d *doctorDeps) { d.http = srv.Client() })
		out, _, _ := r.run("--json", "doctor", "--group", "live")
		text := textOf(t, out)
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, text)
			}
		}
	}
}

func TestDoctorLiveOffIsOneInfoLine(t *testing.T) {
	r := newRig(t)
	r.running()
	r.doctorTools(nil)
	out, _, _ := r.run("--json", "doctor", "--group", "live")
	if text := textOf(t, out); strings.Count(text, "info live view      off (LiveViewEnabled false)") != 3 || strings.Contains(text, "fail") {
		t.Errorf("%s", text)
	}
}

// testRun is a doctor run of the rig's configuration, for a check called directly.
func testRun(t *testing.T, r *rig, deps func(*doctorDeps)) *drun {
	t.Helper()
	paths, cfg, warns, err := app.Resolve(r.root, r.cfg, osx.Current())
	if err != nil {
		t.Fatal(err)
	}
	lg, _ := app.NewLogger(app.LogOptions{NoFile: true, Config: cfg, Getenv: func(string) string { return "" }})
	t.Cleanup(func() { lg.Close() })
	e := &env{opts: app.Options{Root: r.root, ConfigPath: r.cfg, Platform: osx.Current(), Getenv: func(string) string { return "" }}, paths: paths, cfg: cfg, warns: warns,
		inspector: fakeInspector{}, lg: lg, auto: *r.auto, out: &bytes.Buffer{}, errw: &bytes.Buffer{}}
	d := defaultDoctorDeps()
	r.doctorSeams()(&d)
	if deps != nil {
		deps(&d)
	}
	return &drun{e: e, d: d, cache: map[string]*onceValue{}, sem: make(chan struct{}, 4)}
}

// Incident 22 Oct 2026, the first cutover hour: the web role read the live secret from the old
// system's folder, the supervisor from the platform's auth folder, so the live MediaMTX (made
// from the supervisor's) refused the web proxy and Live answered 502 while recording was fine.
// Both resolutions are built here from two configurations, as the two builds made them.
func TestDoctorIncidentWebAndSupervisorLiveSecretsDiffer(t *testing.T) {
	r := newRig(t)
	dir := filepath.Dir(r.root)
	oldDir, authDir := filepath.Join(dir, "IPCCTV"), filepath.Join(dir, "IPCCTV", "camui", "auth")
	os.MkdirAll(authDir, 0o700)
	os.WriteFile(filepath.Join(oldDir, "live_view_secret.txt"), []byte(secretB), 0o600)
	os.WriteFile(filepath.Join(authDir, "live_view_secret.txt"), []byte(secretA), 0o600)
	srv := liveServer(t, secretA) // the live instance was made from the supervisor's secret
	r.setConfig(`"LiveViewEnabled": true, "LiveWhepAddress": "` + strings.TrimPrefix(srv.URL, "http://") + `",`)
	run := testRun(t, r, func(d *doctorDeps) { d.http = srv.Client() })
	webFile := web.LiveSecretFileOf(web.Config{LiveSecretDir: oldDir, ConfigPath: r.cfg}) // the web role of the first build
	dirs := osx.Current().Dirs()
	dirs.Auth = authDir
	supFile := relay.LiveSecretPath(dirsPlatform{osx.Current(), dirs}, run.e.cfg) // the supervisor
	got := liveLoginProbe(context.Background(), run, webFile, supFile)
	var b bytes.Buffer
	printDoctor(&b, doctorReport{Checks: got}, true)
	want := "fail live login     live login refused (the web role and the supervisor use different secrets)"
	wantFix := "     fix: set LiveSecretPath to the same file in holocron.config.json"
	if !strings.HasPrefix(b.String(), want) || !strings.Contains(b.String(), wantFix) || !strings.Contains(b.String(), `"LiveSecretPath": `) {
		t.Fatalf("got:\n%s", b.String())
	}
	t.Logf("incident fixture (live secrets differ):\n%s", b.String())

	// the same two files holding the same secret are fine (the cutover workaround)
	os.WriteFile(filepath.Join(oldDir, "live_view_secret.txt"), []byte(secretA), 0o600)
	if got := liveLoginProbe(context.Background(), run, webFile, supFile); got[0].Level != lvOK || !strings.Contains(got[0].Detail, "different files with the same secret") {
		t.Errorf("%+v", got)
	}
	// a web secret that is missing
	if got := liveLoginProbe(context.Background(), run, filepath.Join(dir, "none.txt"), supFile); got[0].Level != lvFail || !strings.Contains(got[0].Fix, "set LiveSecretPath to "+supFile) {
		t.Errorf("%+v", got)
	}
}

// MediaMTX answers a wrong login after a pause; a probe that runs out of time asks again without a
// login, and an immediate 401 is a refusal, not a dead instance.
func TestDoctorLiveLoginSlowRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			select {
			case <-req.Context().Done():
			case <-time.After(6 * time.Second):
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	r := newRig(t)
	r.setConfig(`"LiveViewEnabled": true, "LiveWhepAddress": "` + strings.TrimPrefix(srv.URL, "http://") + `",`)
	f := filepath.Join(r.platformAuth(), "live_view_secret.txt")
	os.WriteFile(f, []byte(secretA), 0o600)
	run := testRun(t, r, func(d *doctorDeps) { d.http = srv.Client() })
	got := liveLoginProbe(context.Background(), run, f, f)
	if got[0].Level != lvFail || !strings.Contains(got[0].Detail, "live login refused") {
		t.Errorf("%+v", got)
	}
}
