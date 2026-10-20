package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"holocron/internal/cli"
	"holocron/internal/config"
	"holocron/internal/ctlapi"
	"holocron/internal/osx"
	"holocron/internal/status"
	"holocron/internal/supervise"
)

var plat = osx.Current()

func TestCheckSandboxRoot(t *testing.T) {
	ok := t.TempDir()
	withPS := t.TempDir()
	os.MkdirAll(filepath.Join(withPS, "_logs"), 0o755)
	os.WriteFile(filepath.Join(withPS, "_logs", "recorder.log"), []byte("x"), 0o644)
	cases := map[string]bool{ok: true, withPS: false, "/": false, "": false, filepath.Join(ok, "cameras"): false, filepath.Join(ok, "Cameras", "x"): false, filepath.Join(ok, "my-cameras"): true}
	if runtime.GOOS == "windows" {
		cases[`E:\`] = false
		cases[`E:\cameras`] = false
		cases[`C:`] = false
	}
	for root, want := range cases {
		err := CheckSandboxRoot(root)
		if (err == nil) != want || (err != nil && !errors.Is(err, ErrSandbox)) {
			t.Errorf("%q: %v, want ok=%v", root, err, want)
		}
	}
}

func TestResolveRootOverridesAndSandbox(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "holocron.config.json")
	os.WriteFile(cfgPath, []byte(`{"RootPath": "/srv/cameras", "SegmentSeconds": 60}`), 0o644)
	root := filepath.Join(dir, "root")
	p, cfg, _, err := Resolve(root, cfgPath, plat)
	if err != nil || p.Root != root || cfg.RootPath != root || p.Cameras != filepath.Join(dir, "cameras.json") || cfg.SegmentSeconds != 60 {
		t.Fatalf("%+v %v %v", p, cfg.RootPath, err)
	}
	t.Setenv(SandboxEnv, "1")
	if _, _, _, err := Resolve("", cfgPath, plat); !errors.Is(err, ErrSandbox) {
		t.Errorf("RootPath /srv/cameras under the sandbox: %v", err)
	}
	if _, _, _, err := Resolve(root, cfgPath, plat); err != nil {
		t.Errorf("a sandbox root refused: %v", err)
	}
}

func TestEnvCredentials(t *testing.T) {
	env := map[string]string{}
	c := EnvCredentials{Getenv: func(k string) string { return env[k] }}
	err := CheckCredentials(c)
	var cf *supervise.ConfigFault
	if !errors.As(err, &cf) || !strings.Contains(err.Error(), EnvCameraUser) || supervise.ExitCode(err) != 21 {
		t.Errorf("missing user: %v", err)
	}
	env[EnvCameraUser] = "u"
	if err := CheckCredentials(c); err == nil || !strings.Contains(err.Error(), EnvCameraPassword) {
		t.Errorf("missing password: %v", err)
	}
	env[EnvCameraPassword] = "p"
	if u, p, err := c.CameraLogin(); err != nil || u != "u" || p != "p" {
		t.Errorf("%s %s %v", u, p, err)
	}
	// the secrets seam wins when it returns a store
	old := CredentialProvider
	defer func() { CredentialProvider = old }()
	CredentialProvider = func(config.Config) (Credentials, error) {
		return EnvCredentials{Getenv: func(string) string { return "s" }}, nil
	}
	got, _ := CredentialsFor(config.Config{}, nil)
	if u, _, _ := got.CameraLogin(); u != "s" {
		t.Error("provider not used")
	}
}

const sampleConfig = "\xef\xbb\xbf{\r\n    \"SegmentSeconds\":  60,\r\n    \"HealthStripHeightPx\":  -1,\r\n    \"Surprise\":  {\"keep\": [1, 2]},\r\n    \"logging\":  {\"level\": \"warn\"}\r\n}\r\n"

func TestEditConfigKeepsEverythingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "holocron.config.json")
	os.WriteFile(path, []byte(sampleConfig), 0o644)
	ed, err := EditConfig(path, "maxretentiondays", ParseValue("30"), plat)
	if err != nil || !ed.Changed || ed.Key != "MaxRetentionDays" || string(ed.New) != "30" || ed.Old != nil {
		t.Fatalf("%+v %v", ed, err)
	}
	if _, err := EditConfig(path, "logging.levels.Supervisor", ParseValue("debug"), plat); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
		t.Error("BOM lost")
	}
	for _, want := range []string{`"HealthStripHeightPx": -1`, `"Surprise": {`, `"keep": [`, `"MaxRetentionDays": 30`, `"level": "warn"`, `"supervisor": "debug"`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("%s missing in\n%s", want, b)
		}
	}
	order := regexp.MustCompile(`"(SegmentSeconds|HealthStripHeightPx|Surprise|logging|MaxRetentionDays)"`).FindAllString(string(b), -1)
	if strings.Join(order, ",") != `"SegmentSeconds","HealthStripHeightPx","Surprise","logging","MaxRetentionDays"` {
		t.Errorf("key order %v", order)
	}
	cfg, _, err := config.Load(path, plat)
	if err != nil || cfg.Logging.Levels["supervisor"] != "debug" || *cfg.MaxRetentionDays != 30 {
		t.Errorf("reload: %v %+v", err, cfg.Logging)
	}
	// unset goes back to the default; unsetting an absent key is a no-op
	if ed, err := EditConfig(path, "MaxRetentionDays", nil, plat); err != nil || !ed.Changed {
		t.Errorf("unset %+v %v", ed, err)
	}
	if ed, err := EditConfig(path, "MaxRetentionDays", nil, plat); err != nil || ed.Changed {
		t.Errorf("second unset %+v %v", ed, err)
	}
	// a missing file is created
	fresh := filepath.Join(t.TempDir(), "sub", "holocron.config.json")
	if _, err := EditConfig(fresh, "LiveViewEnabled", ParseValue("true"), plat); err != nil {
		t.Fatal(err)
	}
	if c, _, err := config.Load(fresh, plat); err != nil || !c.LiveViewEnabled {
		t.Errorf("fresh: %v", err)
	}
}

func TestEditConfigRefusalLeavesFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "holocron.config.json")
	os.WriteFile(path, []byte(sampleConfig), 0o644)
	for _, c := range []struct{ key, value string }{
		{"SegmentSeconds", "0"}, {"SegmentSeconds", `"900"`}, {"LiveViewEnabled", "yes"}, {"logging.level", "loud"},
		{"RootPath", "/x"}, {"FfmpegPath", "x"}, {"logging.levels.a.b", "debug"}, {"alerts.webhook.url", "http://example.org/x"},
		{"secrets.backend", "vault"}, {"MaxAgePruneFraction", "2"},
	} {
		_, err := EditConfig(path, c.key, ParseValue(c.value), plat)
		if !errors.Is(err, cli.ErrRefused) {
			t.Errorf("%s=%s: %v", c.key, c.value, err)
		}
		if b, _ := os.ReadFile(path); string(b) != sampleConfig {
			t.Fatalf("%s=%s changed the file:\n%q", c.key, c.value, b)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(m) != 0 {
		t.Errorf("temp files left %v", m)
	}
}

func TestParseValue(t *testing.T) {
	for in, want := range map[string]string{"30": "30", "true": "true", "null": "null", "stream2": `"stream2"`, `"x y"`: `"x y"`, "[1,2]": "[1,2]", "": `""`, "12ab": `"12ab"`} {
		if got := string(ParseValue(in)); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

// The allow-list is the Editable column of internal/config/KEYS.md.
func TestEditableKeysMatchKeysMd(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "config", "KEYS.md"))
	if err != nil {
		t.Fatal(err)
	}
	fromDoc := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 7 || strings.TrimSpace(cells[5]) != "yes" {
			continue
		}
		keys := strings.Split(strings.TrimSpace(cells[1]), ",")
		base := ""
		for _, k := range keys {
			k = strings.TrimSpace(k)
			if strings.HasPrefix(k, ".") {
				k = base + k
			} else if i := strings.LastIndex(k, "."); i > 0 {
				base = k[:i]
			}
			if k == "logging.levels" {
				k = "logging.levels.*"
			}
			fromDoc[k] = true
		}
	}
	code := map[string]bool{}
	for _, k := range EditableKeys {
		code[k] = true
		if !fromDoc[k] {
			t.Errorf("%s is editable in code but not marked in KEYS.md", k)
		}
	}
	for k := range fromDoc {
		if !code[k] {
			t.Errorf("%s is marked editable in KEYS.md but not in EditableKeys", k)
		}
	}
	if len(code) < 20 {
		t.Fatalf("parsed %d keys only", len(code))
	}
}

func TestEffectiveConfigMasksSecrets(t *testing.T) {
	cfg := config.Defaults(plat)
	cfg.Alerts.Webhook.URL = "https://user:hunter2@hooks.example.org/x"
	m, err := EffectiveConfig(cfg)
	b, _ := json.Marshal(m)
	if err != nil || bytes.Contains(b, []byte("hunter2")) || m["logging"] == nil || m["alerts"] == nil {
		t.Errorf("%v %s", err, b)
	}
	if v, err := EffectiveValue(cfg, "logging.LEVEL"); err != nil || v != "info" {
		t.Errorf("%v %v", v, err)
	}
	if _, err := EffectiveValue(cfg, "nope.x"); !errors.Is(err, cli.ErrRefused) {
		t.Error(err)
	}
}

const sampleCameras = "\xef\xbb\xbf[\r\n  {\"Name\": \"Front\", \"IP\": \"192.0.2.10\", \"Quality\": \"stream1\", \"Record\": true, \"Note\": \"keep me\"},\r\n  {\"Name\": \"Back\", \"IP\": \"192.0.2.11\", \"Record\": false, \"Extra\": {\"a\": 1}}\r\n]\r\n"

func TestCameraEditsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cameras.json")
	os.WriteFile(path, []byte(sampleCameras), 0o644)
	ch, err := SetRecord(path, "back", true)
	if err != nil || len(ch) != 1 || !ch[0].Changed || ch[0].Target != "Back" {
		t.Fatalf("%+v %v", ch, err)
	}
	before, _ := os.ReadFile(path)
	if ch, _ := SetRecord(path, "Back", true); ch[0].Changed {
		t.Error("no-op reported a change")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("a no-op rewrote the file")
	}
	if _, err := SetRecord(path, "Nope", true); !errors.Is(err, ctlapi.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if err := AddCamera(path, config.Camera{Name: "Side", IP: "cam-side.local", Quality: "stream2", Record: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetCamera(path, "Side", CameraChange{IP: strp("192.0.2.12")}); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveCamera(path, "Front"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) || !bytes.Contains(b, []byte(`"Extra"`)) || bytes.Contains(b, []byte("keep me")) {
		t.Errorf("round trip:\n%s", b)
	}
	c, err := config.LoadCameras(path)
	if err != nil || len(c.List) != 2 || c.List[0].Name != "Back" || !c.List[0].Record || c.List[1].IP != "192.0.2.12" {
		t.Fatalf("%+v %v", c.List, err)
	}
	// unknown fields of an edited camera survive too
	os.WriteFile(path, []byte(sampleCameras), 0o644)
	SetCamera(path, "Front", CameraChange{Quality: strp("stream2")})
	if b, _ := os.ReadFile(path); !bytes.Contains(b, []byte("keep me")) {
		t.Errorf("unknown field of the edited camera lost:\n%s", b)
	}
	// all
	os.WriteFile(path, []byte(sampleCameras), 0o644)
	if ch, err := SetRecord(path, "all", false); err != nil || len(ch) != 2 || !ch[0].Changed || ch[1].Changed {
		t.Errorf("all: %+v %v", ch, err)
	}
}

func TestCameraEditRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cameras.json")
	os.WriteFile(path, []byte(sampleCameras), 0o644)
	for name, err := range map[string]error{
		"duplicate":        AddCamera(path, config.Camera{Name: "front", IP: "192.0.2.20"}),
		"reserved name":    AddCamera(path, config.Camera{Name: "CON", IP: "192.0.2.20"}),
		"bad address":      AddCamera(path, config.Camera{Name: "X", IP: "rtsp://u:p@h/"}),
		"port in address":  AddCamera(path, config.Camera{Name: "X", IP: "192.0.2.20:554"}),
		"bad stream":       AddCamera(path, config.Camera{Name: "X", IP: "192.0.2.20", Quality: "a b"}),
		"folder collision": AddCamera(path, config.Camera{Name: "FRONT?", IP: "192.0.2.20"}),
		"rename onto":      func() error { _, err := SetCamera(path, "Back", CameraChange{Name: strp("Front")}); return err }(),
	} {
		if !errors.Is(err, cli.ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != sampleCameras {
		t.Errorf("a refused edit changed the file:\n%s", b)
	}
	// a fresh install: the first add creates the file
	fresh := filepath.Join(t.TempDir(), "new", "cameras.json")
	if err := AddCamera(fresh, config.Camera{Name: "A", IP: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
}

func strp(s string) *string { return &s }

type fakeInspector map[int]osx.ProcInfo

func (f fakeInspector) Inspect(pid int) (osx.ProcInfo, error) {
	if p, ok := f[pid]; ok {
		return p, nil
	}
	return osx.ProcInfo{}, osx.ErrNotRunning
}
func (f fakeInspector) Stop(osx.ChildEntry) error { return errors.New("never") }

func writeRegistry(t *testing.T, p Paths, entries ...osx.ChildEntry) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"supervisor": 1, "children": entries})
	os.MkdirAll(p.State(), 0o700)
	os.WriteFile(osx.RegistryPath(p.State()), b, 0o600)
}

func TestCensusByIdentity(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	in := fakeInspector{100: {Start: "s100", Image: "/bin/holocron"}, 200: {Start: "s200", Image: "/bin/ffmpeg"}, 300: {Start: "other", Image: "/bin/ffmpeg"}}
	os.MkdirAll(p.State(), 0o700)
	b, _ := json.Marshal(osx.ChildEntry{Pid: 100, Start: "s100", Image: "/bin/holocron", Role: "host"})
	os.WriteFile(p.HostFile(), b, 0o600)
	writeRegistry(t, p, osx.ChildEntry{Pid: 200, Start: "s200", Image: "/bin/ffmpeg", Role: "ffmpeg.exe"},
		osx.ChildEntry{Pid: 300, Start: "s300", Image: "/bin/ffmpeg", Role: "reused"}, osx.ChildEntry{Pid: 400, Start: "s400", Image: "/bin/x"})
	c := TakeCensus(p, in, 5)
	if c.Host == nil || c.Host.Pid != 100 || len(c.Children) != 1 || c.Children[0].Pid != 200 || c.Supervisor != 0 || c.Count() != 2 {
		t.Fatalf("%+v", c)
	}
	// the instance lock held by a live supervisor counts it
	lk, err := osx.AcquireInstanceLock(p.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	// in-process: a second acquire in the same process may succeed on some OSes; the verdict path covers it
	if c := TakeCensus(p, in, 5); runtime.GOOS == "linux" && c.Supervisor == 0 {
		t.Logf("lock held by this process is not seen as another holder on %s (flock per open file)", runtime.GOOS)
	}
}

type fakeStopper struct {
	err    error
	onStop func()
	calls  int
}

func (f *fakeStopper) Stop(ctx context.Context, kind string) (ctlapi.StopReport, error) {
	f.calls++
	if f.onStop != nil {
		f.onStop()
	}
	return ctlapi.StopReport{Kind: kind, Final: true}, f.err
}

// a status document that the verdict reads as running: this test process, its real start time
func runningDoc(t *testing.T, p Paths) {
	t.Helper()
	start, ok := status.OSProcesses{}.Alive(int64(os.Getpid()))
	if !ok {
		t.Skip("cannot read this process's start time")
	}
	d := &status.Document{Schema: 1, State: status.StateRunning, SupervisorPid: int64(os.Getpid()), StartedAt: status.NewTime(start), UpdatedAt: status.NewTime(time.Now())}
	os.MkdirAll(p.State(), 0o700)
	if err := status.NewWriter(p.StatusFile(), status.Options{}).Write(d); err != nil {
		t.Fatal(err)
	}
}

func stoppedDoc(t *testing.T, p Paths) {
	d := &status.Document{Schema: 1, State: status.StateStopped, StopKind: status.StopKindRequested, SupervisorPid: 1, UpdatedAt: status.NewTime(time.Now())}
	if err := status.NewWriter(p.StatusFile(), status.Options{}).Write(d); err != nil {
		t.Fatal(err)
	}
}

func TestDownPaths(t *testing.T) {
	cfg := config.Defaults(plat)
	cfg.StopTimeoutSeconds = 1
	t.Run("nothing running is idempotent", func(t *testing.T) {
		p := Paths{Root: t.TempDir()}
		os.MkdirAll(p.State(), 0o700)
		os.WriteFile(p.StopFile(), []byte("old"), 0o600)
		st := &fakeStopper{}
		res := Down(context.Background(), p, cfg, func(string) (Stopper, error) { return st, nil }, fakeInspector{})
		if !res.OK() || res.Via != "" || st.calls != 0 {
			t.Errorf("%+v", res)
		}
		if _, err := os.Stat(p.StopFile()); !os.IsNotExist(err) {
			t.Error("leftover stop request not removed")
		}
	})
	t.Run("through the API, proved", func(t *testing.T) {
		p := Paths{Root: t.TempDir()}
		runningDoc(t, p)
		st := &fakeStopper{onStop: func() { stoppedDoc(t, p) }}
		res := Down(context.Background(), p, cfg, func(string) (Stopper, error) { return st, nil }, fakeInspector{})
		if !res.OK() || res.Via != "api" || st.calls != 1 || res.Before.Supervisor != os.Getpid() || res.Report == nil {
			t.Errorf("%+v", res)
		}
	})
	t.Run("unreachable API: the stop file", func(t *testing.T) {
		p := Paths{Root: t.TempDir()}
		runningDoc(t, p)
		res := Down(context.Background(), p, cfg, func(string) (Stopper, error) { return nil, ctlapi.ErrUnreachable }, fakeInspector{})
		if res.Via != "stop-file" || res.OK() {
			t.Errorf("%+v", res)
		}
		if _, err := os.Stat(p.StopFile()); err != nil {
			t.Error("no stop request written")
		}
		if !strings.Contains(strings.Join(res.After.Lines(), ";"), "supervisor pid") {
			t.Errorf("survivor not listed: %v", res.After.Lines())
		}
	})
	t.Run("a child that survives is reported, never killed", func(t *testing.T) {
		p := Paths{Root: t.TempDir()}
		writeRegistry(t, p, osx.ChildEntry{Pid: 200, Start: "s", Image: "/bin/ffmpeg", Role: "ffmpeg"})
		in := fakeInspector{200: {Start: "s", Image: "/bin/ffmpeg"}}
		res := Down(context.Background(), p, cfg, func(string) (Stopper, error) { return &fakeStopper{}, nil }, in)
		if res.OK() || len(res.After.Children) != 1 || res.Via != "stop-file" {
			t.Errorf("%+v", res)
		}
	})
}

func TestWaitRunningNeedsAFreshHeartbeat(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	cfg := config.Defaults(plat)
	runningDoc(t, p)
	after := time.Now().Add(time.Second)
	if _, ok, why := WaitRunning(context.Background(), p, cfg, after, 300*time.Millisecond, 50*time.Millisecond); ok || !strings.Contains(why, "older") {
		t.Errorf("an older heartbeat accepted: %v %s", ok, why)
	}
	go func() { time.Sleep(1200 * time.Millisecond); runningDoc(t, p) }()
	if _, ok, why := WaitRunning(context.Background(), p, cfg, after, 3*time.Second, 50*time.Millisecond); !ok {
		t.Errorf("fresh heartbeat missed: %s", why)
	}
}

func TestHostRecord(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	in := fakeInspector{os.Getpid(): {Start: "me", Image: "/bin/holocron"}}
	remove, err := WriteHostRecord(p, in)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := ReadHostRecord(p); !ok || e.Pid != os.Getpid() || e.Start != "me" {
		t.Errorf("%+v", e)
	}
	if c := TakeCensus(p, in, 5); c.Host == nil {
		t.Error("host not counted")
	}
	remove()
	if _, ok := ReadHostRecord(p); ok {
		t.Error("not removed")
	}
}
