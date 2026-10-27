package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/osx"
	"holocron/internal/web"
)

// healthOf is GET /api/admin/health on the running web app: the status, the document under
// "health" and the raw body.
func healthOf(t *testing.T, base, query string) (int, doctorReport, string) {
	t.Helper()
	resp, err := http.Get(base + "/api/admin/health" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var body struct {
		Health    doctorReport `json:"health"`
		CheckedAt time.Time    `json:"checkedAt"`
	}
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(b, &body); err != nil || body.CheckedAt.IsZero() {
			t.Fatalf("answer: %v\n%s", err, b)
		}
	}
	return resp.StatusCode, body.Health, string(b)
}

// stalledRig is a running system whose only recording file does not grow (incident of 22 Oct 2026:
// a camera stream ended and nothing new reached the disk). sleeps counts the growth waits.
func stalledRig(t *testing.T, sleeps *atomic.Int32, wait func(context.Context) error) *rig {
	t.Helper()
	now := time.Now()
	r := newRig(t)
	r.running()
	r.segment("Front", now.Add(-11*time.Minute), now.Add(-10*time.Minute), 4000)
	r.segment("Front", now.Add(-10*time.Minute), now.Add(-10*time.Minute+time.Second), 1000)
	r.useFileSecrets()
	r.noCreds = true
	r.doctorTools(func(d *doctorDeps) {
		d.sleep = func(ctx context.Context, _ time.Duration) error {
			sleeps.Add(1)
			if wait != nil {
				return wait(ctx)
			}
			return nil
		}
	})
	return r
}

func TestWebHealthIsTheDoctorDocumentAndShowsAStalledCamera(t *testing.T) {
	var sleeps atomic.Int32
	r := stalledRig(t, &sleeps, nil)
	fixed, seams := time.Now(), r.doctor
	r.doctor = func(d *doctorDeps) { // ages and free space in the details equal in both runs
		seams(d)
		d.now = func() time.Time { return fixed }
		d.platform = freePlatform{d.platform, 100 * gb}
	}
	base := serveAdmin(t, r)
	code, got, raw := healthOf(t, base, "")
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	var want doctorReport
	if err := json.Unmarshal([]byte(mustRun(r, "--json", "doctor")), &want); err != nil {
		t.Fatal(err)
	}
	if got.Schema != 1 || got.Root != want.Root || strings.Join(got.Groups, ",") != strings.Join(want.Groups, ",") || len(got.Checks) != len(want.Checks) {
		t.Fatalf("not the doctor's document:\n got %+v\nwant %+v", got.Summary, want.Summary)
	}
	for i, c := range got.Checks {
		w := want.Checks[i]
		if c.ID == "system.env" {
			continue // the web process's environment, see TestWebHealthRunsInTheWebRoleContext
		}
		if c.ID != w.ID || c.Group != w.Group || c.Level != w.Level || c.Title != w.Title || c.Detail != w.Detail || c.Fix != w.Fix {
			t.Errorf("check %d differs:\n web %+v\n cli %+v", i, c, w)
		}
	}
	// the fields are the CLI's: the same JSON keys in the same order
	var wantKeys, gotKeys map[string]json.RawMessage
	cli, _ := json.Marshal(want)
	web, _ := json.Marshal(got)
	json.Unmarshal(cli, &wantKeys)
	json.Unmarshal(web, &gotKeys)
	for k := range wantKeys {
		if _, ok := gotKeys[k]; !ok {
			t.Errorf("field %q missing", k)
		}
	}
	// the stalled camera: BROKEN, and the exact fix
	if got.Summary.Verdict != "BROKEN" || got.Summary.Failures == 0 {
		t.Errorf("summary %+v", got.Summary)
	}
	found := false
	for _, c := range got.Checks {
		if c.ID == "recording.growth" && c.Level == lvFail && strings.Contains(c.Detail, "not recording") && c.Fix == `holocron cam test "Front" ; holocron logs --component ffmpeg:Front --since 10m` {
			found = true
		}
	}
	if !found {
		t.Errorf("no failing growth line with the camera's fix:\n%s", textOf(t, raw[strings.Index(raw, `"health":`)+9:len(raw)-1]))
	}
}

func TestWebHealthGroupsAreTheDoctorsAndAnUnknownOneIs400(t *testing.T) {
	if strings.Join(web.HealthGroups, ",") != strings.Join(doctorGroups, ",") {
		t.Errorf("web.HealthGroups %v, doctorGroups %v", web.HealthGroups, doctorGroups)
	}
	var sleeps atomic.Int32
	r := stalledRig(t, &sleeps, nil)
	base := serveAdmin(t, r)
	code, rep, _ := healthOf(t, base, "?group=setup,system")
	if code != 200 || strings.Join(rep.Groups, ",") != "setup,system" {
		t.Fatalf("%d %v", code, rep.Groups)
	}
	for _, c := range rep.Checks {
		if c.Group != "setup" && c.Group != "system" {
			t.Errorf("ran %+v", c)
		}
	}
	if code, _, raw := healthOf(t, base, "?group=nope"); code != 400 || !strings.Contains(raw, "group takes setup, recording") {
		t.Errorf("unknown group: %d %s", code, raw)
	}
	if sleeps.Load() != 0 {
		t.Error("the recording group ran for setup,system")
	}
}

// Two requests at once are one run (the growth sample waits 5 times when nothing grows), and a
// third within 5 s is answered from the last run.
func TestWebHealthSharesOneRunAndCachesIt(t *testing.T) {
	var sleeps atomic.Int32
	release := make(chan struct{})
	r := stalledRig(t, &sleeps, func(ctx context.Context) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	base := serveAdmin(t, r)
	type answer struct {
		code int
		rep  doctorReport
	}
	ans := make(chan answer, 2)
	for i := 0; i < 2; i++ {
		go func() { code, rep, _ := healthOf(t, base, ""); ans <- answer{code, rep} }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for sleeps.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // the second request joins the run in progress
	close(release)
	for i := 0; i < 2; i++ {
		if a := <-ans; a.code != 200 || a.rep.Summary.Verdict != "BROKEN" {
			t.Errorf("answer %d: %+v", i, a)
		}
	}
	if n := sleeps.Load(); n != growthRounds {
		t.Errorf("%d growth waits for 2 concurrent requests, want one run's %d", n, growthRounds)
	}
	if code, _, _ := healthOf(t, base, ""); code != 200 || sleeps.Load() != growthRounds {
		t.Errorf("a third request within 5 s ran again (%d waits)", sleeps.Load())
	}
}

// A browser that goes away ends the checks, and the cancelled run is not kept for the next one.
func TestWebHealthStopsWhenTheBrowserDisconnects(t *testing.T) {
	var sleeps atomic.Int32
	cancelled := make(chan struct{}, 1)
	var armed atomic.Bool
	armed.Store(true)
	r := stalledRig(t, &sleeps, func(ctx context.Context) error {
		if !armed.Load() {
			return nil
		}
		<-ctx.Done()
		select {
		case cancelled <- struct{}{}:
		default:
		}
		return ctx.Err()
	})
	base := serveAdmin(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/admin/health", nil)
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	for sleeps.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the checks were not cancelled when the browser went away")
	}
	armed.Store(false)
	before := sleeps.Load()
	if code, _, _ := healthOf(t, base, ""); code != 200 || sleeps.Load() == before {
		t.Errorf("the cancelled run was served from the cache (%d waits)", sleeps.Load())
	}
}

// Inside the web role the environment is the web process's (info), and the census does not count
// the web role or its children as strays - while a real stray is still found.
func TestWebHealthRunsInTheWebRoleContext(t *testing.T) {
	r := newRig(t)
	r.running()
	insp := &osx.FakeInspector{Procs: map[int]osx.ProcInfo{
		7373: {Image: "/opt/ff/ffmpeg"}, 7474: {Image: "/opt/ff/ffmpeg"},
	}}
	r.insp = insp
	r.extraEnv = map[string]string{app.SandboxEnv: "1", app.EnvCameraUser: "u", app.EnvCameraPassword: "p"}
	r.doctorTools(func(d *doctorDeps) {
		d.listProcs = func() ([]osx.ProcEntry, error) {
			return []osx.ProcEntry{{Pid: 7373, PPid: 1, Name: "ffmpeg"}, {Pid: 7474, PPid: os.Getpid(), Name: "ffmpeg"}}, nil // the second: the web role's own clip export
		}
		d.lookPath = func(p string) (string, error) {
			if strings.Contains(p, "ffmpeg") {
				return "/opt/ff/ffmpeg", nil
			}
			return "", os.ErrNotExist
		}
	})
	// the guard of the CLI would refuse this root in a window with the variable set; the web role has started
	base := serveAdmin(t, r)
	code, rep, raw := healthOf(t, base, "?group=system")
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	var env *check
	var strays []string
	for i, c := range rep.Checks {
		switch {
		case c.ID == "system.env":
			env = &rep.Checks[i]
		case c.Title == "stray":
			strays = append(strays, c.Detail)
		}
	}
	if env == nil || env.Level != lvInfo || env.Fix != "" || !strings.HasPrefix(env.Detail, "environment of the web role: ") || !strings.Contains(env.Detail, app.SandboxEnv) {
		t.Errorf("environment: %+v", env)
	}
	if len(strays) != 1 || !strings.Contains(strays[0], "pid 7373") {
		t.Errorf("strays %q, want only pid 7373 (7474 is the web role's child)", strays)
	}
	for _, c := range rep.Checks {
		if c.ID == "setup.sandbox" {
			t.Errorf("the sandbox guard cannot fire in the web role: %+v", c)
		}
	}
}
