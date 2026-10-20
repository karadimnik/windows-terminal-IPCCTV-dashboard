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
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSupervisor is a stand-in for the Go supervisor's control endpoint: it answers the three
// calls the APIController makes and remembers what it was asked and by whom.
type fakeSupervisor struct {
	srv  *httptest.Server
	root string

	mu      sync.Mutex
	calls   []string
	headers []http.Header
	busy    bool
	running bool
}

const fakeSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newFakeSupervisor(t *testing.T, root string) *fakeSupervisor {
	t.Helper()
	f := &fakeSupervisor{root: root, running: true}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("X-Holocron-Key") != fakeSecret {
			http.Error(w, `{"ok":false,"error":"bad key"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.calls = append(f.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		f.headers = append(f.headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		if f.busy && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"ok":false,"error":"another control action is running"}`)
			return
		}
		switch {
		case r.URL.Path == "/v1/state":
			io.WriteString(w, fmt.Sprintf(`{"ok":true,"data":{"running":%v,"pid":4242,"cameras":[{"name":"Front","record":true,"alive":true}]}}`, f.running))
		case r.URL.Path == "/v1/stop":
			io.WriteString(w, `{"ok":true,"data":{"kind":"requested","reason":"stop requested","survivors":[],"final":true}}`)
		case strings.HasPrefix(r.URL.Path, "/v1/cameras/") && strings.HasSuffix(r.URL.Path, "/record"):
			io.WriteString(w, `{"ok":true,"data":{"target":"Front","enabled":false,"changed":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	state := filepath.Join(root, "_state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(state, "ctl.addr"), []byte(strings.TrimPrefix(f.srv.URL, "http://")), 0o600)
	os.WriteFile(filepath.Join(state, "ctl.secret"), []byte(fakeSecret), 0o600)
	return f
}

func (f *fakeSupervisor) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSupervisor) count(prefix string) int {
	n := 0
	for _, c := range f.callList() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// controllerRig is an API over a sandbox root with a fake PowerShell and (optionally) a fake supervisor.
type controllerRig struct {
	api   *API
	root  string
	psRun []string
}

func newControllerRig(t *testing.T, goSystem bool) *controllerRig {
	t.Helper()
	root := t.TempDir()
	scripts := t.TempDir()
	os.WriteFile(filepath.Join(scripts, "cameras.json"), []byte(`[{"Name":"Front","IP":"192.0.2.10","Quality":"stream1","Record":true}]`), 0o600)
	os.WriteFile(filepath.Join(scripts, "recorder.config.json"), []byte(`{}`), 0o600)
	rig := &controllerRig{root: root}
	api := NewAPI(Config{ArchiveRoot: root, StatusFile: filepath.Join(root, "_state", "status.json"), ScriptDir: scripts}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	api.goSystem = goSystem
	api.runScript = func(timeout time.Duration, script string, args ...string) psResult {
		rig.psRun = append(rig.psRun, script+" "+strings.Join(args, " "))
		return psResult{Command: script + " " + strings.Join(args, " ")}
	}
	rig.api = api
	return rig
}

func (r *controllerRig) post(t *testing.T, path, body string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	r.api.Routes(mux)
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (r *controllerRig) statusController(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	r.api.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	var out struct{ Controller string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return out.Controller
}

// With no ctl.addr the PowerShell controller does exactly what it did before: the same scripts, the same switches.
func TestControllerPowerShellWhenNoEndpoint(t *testing.T) {
	rig := newControllerRig(t, false)
	if got := rig.statusController(t); got != "powershell" {
		t.Fatalf("controller %q", got)
	}
	if code, body := rig.post(t, "/api/control", `{"scope":"recorder","action":"stop"}`); code != 200 || body["ok"] != true {
		t.Fatalf("%d %v", code, body)
	}
	if code, _ := rig.post(t, "/api/cameras/Front/record", `{"enabled":false}`); code != 200 {
		t.Fatalf("record %d", code)
	}
	want := []string{"cam.ps1 -Stop -Recorder", "cam.recorder.ps1 -Disable Front"}
	if strings.Join(rig.psRun, "|") != strings.Join(want, "|") {
		t.Errorf("ran %v, want %v", rig.psRun, want)
	}
}

// With a live ctl.addr the page's Stop and record toggle go to the control API and run no PowerShell.
func TestControllerAPIWhenEndpointAnswers(t *testing.T) {
	rig := newControllerRig(t, false)
	sup := newFakeSupervisor(t, rig.root)
	if got := rig.statusController(t); got != "api" {
		t.Fatalf("controller %q", got)
	}
	code, body := rig.post(t, "/api/control", `{"scope":"all","action":"stop"}`)
	if code != 200 || body["ok"] != true || body["command"] != "POST /v1/stop" {
		t.Fatalf("stop: %d %v", code, body)
	}
	code, body = rig.post(t, "/api/cameras/Front/record", `{"enabled":false}`)
	if code != 200 || body["ok"] != true || !strings.Contains(fmt.Sprint(body["output"]), "Front: record off") {
		t.Fatalf("record: %d %v", code, body)
	}
	calls := sup.callList()
	if sup.count("POST /v1/stop") != 1 || sup.count(`POST /v1/cameras/Front/record {"enabled":false}`) != 1 {
		t.Errorf("calls %v", calls)
	}
	if len(rig.psRun) != 0 {
		t.Errorf("PowerShell ran: %v", rig.psRun)
	}
	// the actor and the action id travel to the supervisor's records
	sup.mu.Lock()
	defer sup.mu.Unlock()
	for _, h := range sup.headers {
		if h.Get("X-Holocron-Actor") != "web:loopback" && !strings.HasPrefix(h.Get("X-Holocron-Actor"), "web:") && h.Get("X-Holocron-Actor") != "" {
			t.Errorf("actor %q", h.Get("X-Holocron-Actor"))
		}
	}
	last := sup.headers[len(sup.headers)-1]
	if last.Get("X-Holocron-Actor") != "web:loopback" || len(last.Get("X-Action-Id")) != 16 {
		t.Errorf("headers actor %q action id %q", last.Get("X-Holocron-Actor"), last.Get("X-Action-Id"))
	}
}

// The choice is cached for a few seconds; after that the endpoint is asked again, and one that
// stopped answering sends the page back to PowerShell.
func TestControllerChoiceIsCachedThenRefreshed(t *testing.T) {
	rig := newControllerRig(t, false)
	sup := newFakeSupervisor(t, rig.root)
	rig.statusController(t)
	rig.statusController(t)
	rig.statusController(t)
	if n := sup.count("GET /v1/state"); n != 1 {
		t.Errorf("%d probes for 3 requests, want 1", n)
	}
	sup.srv.Close()
	rig.api.pickedAt = time.Now().Add(-time.Minute)
	if got := rig.statusController(t); got != "powershell" {
		t.Errorf("after the endpoint went away the controller is %q", got)
	}
}

// A busy supervisor is a 409 on the page, a missing record is not swallowed.
func TestControllerAPIBusyIs409(t *testing.T) {
	rig := newControllerRig(t, false)
	sup := newFakeSupervisor(t, rig.root)
	sup.busy = true
	code, body := rig.post(t, "/api/control", `{"scope":"all","action":"stop"}`)
	if code != http.StatusConflict || body["ok"] != false || !strings.Contains(fmt.Sprint(body["error"]), "another control action") {
		t.Errorf("%d %v", code, body)
	}
}

// The holocron web role with no supervisor: a clear message, not a PowerShell failure.
func TestControllerStoppedGoSystem(t *testing.T) {
	rig := newControllerRig(t, true)
	if got := rig.statusController(t); got != "api" {
		t.Fatalf("controller %q", got)
	}
	code, body := rig.post(t, "/api/control", `{"scope":"all","action":"start"}`)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]), "holocron up") {
		t.Errorf("start: %d %v", code, body)
	}
	if code, body = rig.post(t, "/api/control", `{"scope":"all","action":"stop"}`); code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]), "nothing is running") {
		t.Errorf("stop: %d %v", code, body)
	}
	if code, body = rig.post(t, "/api/cameras/Front/record", `{"enabled":false}`); code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]), "holocron cam") {
		t.Errorf("record: %d %v", code, body)
	}
	if len(rig.psRun) != 0 {
		t.Errorf("PowerShell ran: %v", rig.psRun)
	}
	// settings writes are PowerShell: refused plainly in the Go system
	mux := http.NewServeMux()
	rig.api.Routes(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/config", strings.NewReader(`{"config":{"SegmentSeconds":300}}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "holocron config set") {
		t.Errorf("config PUT: %d %s", rec.Code, rec.Body.String())
	}
	if len(rig.psRun) != 0 {
		t.Errorf("PowerShell ran for settings: %v", rig.psRun)
	}
}

// Start runs `<this executable> up --detach` with the web role's own flags, only when it was given the executable.
func TestControllerStartRunsUpDetach(t *testing.T) {
	if os.Getenv("WEB_TEST_HELPER") == "1" {
		fmt.Println("helper args:", strings.Join(os.Args[1:], " "))
		os.Exit(0)
	}
	rig := newControllerRig(t, true)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEB_TEST_HELPER", "1")
	rig.api.launch = &HolocronLaunch{Exe: exe, Args: []string{"-test.run=TestControllerStartRunsUpDetach", "--"}}
	code, body := rig.post(t, "/api/control", `{"scope":"recorder","action":"start"}`)
	if code != 200 || body["ok"] != true || !strings.Contains(fmt.Sprint(body["output"]), "helper args: ") || !strings.Contains(fmt.Sprint(body["output"]), "up --detach") {
		t.Errorf("%d %v", code, body)
	}
	if body["command"] != "holocron up --detach" {
		t.Errorf("command %v", body["command"])
	}
	// the dashboard scope is never started from the page
	if code, _ = rig.post(t, "/api/control", `{"scope":"dashboard","action":"start"}`); code != http.StatusConflict {
		t.Errorf("dashboard start: %d", code)
	}
}

// The tasks strip shows the supervisor and the web app themselves.
func TestControllerAPITaskStates(t *testing.T) {
	rig := newControllerRig(t, false)
	sup := newFakeSupervisor(t, rig.root)
	c := &APIController{Root: rig.root}
	tasks := c.TaskStates()
	if tasks["recorder"].Status != "Running" || !tasks["recorder"].Exists || tasks["dashboard"].Status != "Running" {
		t.Errorf("%+v", tasks)
	}
	sup.srv.Close()
	c2 := &APIController{Root: rig.root}
	if got := c2.TaskStates()["recorder"].Status; got != "Stopped" {
		t.Errorf("recorder %q with no endpoint", got)
	}
}
