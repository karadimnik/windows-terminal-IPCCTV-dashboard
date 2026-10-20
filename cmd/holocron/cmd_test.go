package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/cli"
	"holocron/internal/config"
	"holocron/internal/ctlapi"
	"holocron/internal/osx"
	"holocron/internal/status"
	"holocron/internal/supervise"
)

func TestMain(m *testing.M) {
	os.Setenv(app.SandboxEnv, "1")
	os.Exit(m.Run())
}

// fakeCtl records the calls and the action ids they carried.
type fakeCtl struct {
	mu     sync.Mutex
	calls  []string
	ids    []string
	live   bool
	levels ctlapi.Levels
	onStop func()
	err    error
}

func (f *fakeCtl) note(ctx context.Context, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.ids = append(f.ids, cli.ActionIDFrom(ctx))
}
func (f *fakeCtl) State(ctx context.Context) (ctlapi.State, error) {
	f.note(ctx, "state")
	return ctlapi.State{Running: true, Live: &ctlapi.LiveState{Enabled: f.live, Alive: f.live}}, f.err
}
func (f *fakeCtl) Stop(ctx context.Context, kind string) (ctlapi.StopReport, error) {
	f.note(ctx, "stop:"+kind)
	if f.onStop != nil {
		f.onStop()
	}
	return ctlapi.StopReport{Kind: kind, Reason: "stop requested", Survivors: []int{}, Final: true}, f.err
}
func (f *fakeCtl) Reload(ctx context.Context) error { f.note(ctx, "reload"); return f.err }
func (f *fakeCtl) SetLive(ctx context.Context, on bool) (ctlapi.Changed, error) {
	f.note(ctx, "live")
	f.live = on
	return ctlapi.Changed{Target: "LiveViewEnabled", Enabled: on, Changed: true}, f.err
}
func (f *fakeCtl) Levels(ctx context.Context) (ctlapi.Levels, error) {
	f.note(ctx, "levels")
	return f.levels, f.err
}
func (f *fakeCtl) SetLevel(ctx context.Context, c, l string) (ctlapi.Levels, error) {
	f.note(ctx, "level:"+c+"="+l)
	return ctlapi.Levels{Default: "INFO", Components: map[string]string{c: strings.ToUpper(l)}}, f.err
}

type fakeInspector struct{}

func (fakeInspector) Inspect(int) (osx.ProcInfo, error) { return osx.ProcInfo{}, osx.ErrNotRunning }
func (fakeInspector) Stop(osx.ChildEntry) error         { return errors.New("never") }

type rig struct {
	t         *testing.T
	root, cfg string
	ctl       *fakeCtl
	logSpec   string // HOLOCRON_LOG for the runs
}

const rigConfig = "\xef\xbb\xbf{\n    \"SegmentSeconds\":  60,\n    \"HealthStripHeightPx\":  -1,\n    \"Unknown\":  {\"x\": 1},\n    \"StopTimeoutSeconds\":  1,\n    \"alerts\": {\"desktop\": {\"enabled\": false}, \"eventlog\": {\"enabled\": false}}\n}\n"
const rigCameras = "\xef\xbb\xbf[{\"Name\":\"Front\",\"IP\":\"192.0.2.10\",\"Record\":true,\"Note\":\"kept\"},{\"Name\":\"Back\",\"IP\":\"192.0.2.11\",\"Record\":false}]"

func newRig(t *testing.T) *rig {
	dir := t.TempDir()
	r := &rig{t: t, root: filepath.Join(dir, "root"), cfg: filepath.Join(dir, "cfg", "holocron.config.json"), ctl: &fakeCtl{}}
	os.MkdirAll(filepath.Dir(r.cfg), 0o755)
	os.WriteFile(r.cfg, []byte(rigConfig), 0o644)
	os.WriteFile(r.cameras(), []byte(rigCameras), 0o644)
	return r
}

func (r *rig) cameras() string { return filepath.Join(filepath.Dir(r.cfg), "cameras.json") }

func (r *rig) run(args ...string) (string, string, int) {
	var out, errw bytes.Buffer
	argv := append([]string{"--root", r.root, "--config", r.cfg}, args...)
	code := run(context.Background(), argv, &out, &errw, &seams{
		dial:      func(string) (ctl, error) { return r.ctl, nil },
		inspector: fakeInspector{},
		exe:       "/nonexistent/holocron",
		getenv: func(k string) string {
			return map[string]string{app.EnvCameraUser: "u", app.EnvCameraPassword: "p", "HOLOCRON_LOG": r.logSpec}[k]
		},
	})
	return out.String(), errw.String(), code
}

// running writes a status.json that the verdict reads as a running supervisor (this test process).
func (r *rig) running() {
	r.t.Helper()
	start, ok := status.OSProcesses{}.Alive(int64(os.Getpid()))
	if !ok {
		r.t.Skip("cannot read this process's start time")
	}
	d := &status.Document{Schema: 1, State: status.StateRunning, SupervisorPid: int64(os.Getpid()), StartedAt: status.NewTime(start), UpdatedAt: status.NewTime(time.Now()),
		Cameras: []status.Camera{{Name: "Front", SafeName: "Front", Record: true, Alive: true, InputMode: "relay"}},
		Disk:    &status.Disk{FreeGB: 100, ReserveGB: 20}, Prune: &status.Prune{}, Restreamer: &status.Restreamer{Enabled: true, Alive: true, Paths: []string{"front"}}}
	os.MkdirAll(filepath.Join(r.root, "_state"), 0o700)
	if err := status.NewWriter(filepath.Join(r.root, "_state", "status.json"), status.Options{}).Write(d); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) stopped() {
	d := &status.Document{Schema: 1, State: status.StateStopped, StopKind: status.StopKindRequested, SupervisorPid: 1, UpdatedAt: status.NewTime(time.Now())}
	status.NewWriter(filepath.Join(r.root, "_state", "status.json"), status.Options{}).Write(d)
}

func (r *rig) log() string {
	b, _ := os.ReadFile(filepath.Join(r.root, "_logs", "holocron.log"))
	return string(b)
}

func TestEveryCommandIsAnAction(t *testing.T) {
	cli.AssertAllExposedAreActions(t, exposed())
	seen := map[string]bool{}
	for _, c := range commands {
		if seen[c.path] || seen[c.action] {
			t.Errorf("duplicate %s / %s", c.path, c.action)
		}
		seen[c.path], seen[c.action] = true, true
		a, ok := cli.Default.Lookup(c.action)
		if !ok || a.Component != "cli" || a.Mutating != c.mutating {
			t.Errorf("%s: %+v", c.path, a)
		}
	}
	for _, want := range []string{"up", "down", "restart", "status", "cam list", "cam test", "config set", "live status", "log level", "logs", "alert test", "doctor", "version",
		"web", "open", "user add", "tls ca", "secret set", "install", "uninstall", "autostart status", "prune", "archive stats", "host", "supervisor"} {
		if !seen[want] {
			t.Errorf("command %q missing", want)
		}
	}
	var help bytes.Buffer
	printHelp(&help)
	for _, c := range commands {
		listed := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(c.path) + `( |$)`).MatchString(help.String())
		if listed == c.role {
			t.Errorf("%s: listed=%v role=%v", c.path, listed, c.role)
		}
	}
	// negative control: a command that is not an Action is caught
	ft := &countTB{}
	cli.AssertAllExposedAreActions(ft, append(exposed(), "camera.nope"))
	if ft.n != 1 {
		t.Error("the registry test misses an unregistered command")
	}
}

type countTB struct{ n int }

func (c *countTB) Helper()               {}
func (c *countTB) Errorf(string, ...any) { c.n++ }

func TestMatchCommandAndFlags(t *testing.T) {
	for _, c := range []struct {
		argv     []string
		path     string
		leftover string
	}{
		{[]string{"cam", "add", "--name", "x"}, "cam add", "--name x"},
		{[]string{"cam"}, "", ""},
		{[]string{"log", "level", "a", "b"}, "log level", "a b"},
		{[]string{"logs", "-f"}, "logs", "-f"},
		{[]string{"user", "add", "x"}, "user add", "x"},
		{[]string{"status"}, "status", ""},
		{[]string{"nope"}, "", ""},
	} {
		cmd, rest := matchCommand(c.argv)
		got := ""
		if cmd != nil {
			got = cmd.path
		}
		if got != c.path || strings.Join(rest, " ") != c.leftover {
			t.Errorf("%v: %q %v", c.argv, got, rest)
		}
	}
	e := &env{}
	fs := newFlags(e, "x")
	save := fs.Bool("save", false, "")
	pos, err := parseFlags(fs, []string{"supervisor", "--save", "debug", "--json"})
	if err != nil || strings.Join(pos, ",") != "supervisor,debug" || !*save || !e.json {
		t.Errorf("interleaved: %v %v %v %v", pos, err, *save, e.json)
	}
	if _, err := parseFlags(newFlags(e, "x"), []string{"--bogus"}); codeOf(err) != exitUsage {
		t.Errorf("bad flag: %v", err)
	}
	if g := globalArgs("/r", "/c.json", true, true); strings.Join(g, " ") != "--root /r --config /c.json -vv" {
		t.Errorf("global %v", g)
	}
}

func TestExitCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{
		{nil, 0}, {errors.New("x"), 1}, {usageErr("x"), 2}, {quietFail("x"), 1}, {&exitError{code: 21}, 21},
		{&supervise.ConfigFault{Err: errors.New("x")}, 21}, {&supervise.AnotherSupervisorError{Owner: "x"}, 20},
		{app.ErrSandbox, 2}, {cli.Refuse("x"), 1},
	} {
		if got := codeOf(c.err); got != c.code {
			t.Errorf("%v: %d want %d", c.err, got, c.code)
		}
	}
	r := newRig(t)
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"nope"}, 2}, {[]string{"cam"}, 2}, {[]string{"cam", "show"}, 2}, {[]string{"cam", "add", "--name", "x"}, 2},
		{[]string{"config", "set", "SegmentSeconds"}, 2}, {[]string{"status", "--bogus"}, 2}, {[]string{"up", "--only", "x"}, 2},
		{[]string{"web"}, 1}, {[]string{"user", "add", "x"}, 1}, {[]string{"status"}, 1}, {[]string{"version"}, 0},
		{[]string{"cam", "show", "Nope"}, 1}, {[]string{"logs", "--level", "loud"}, 2}, {[]string{"logs", "--since", "yesterday"}, 2},
	} {
		if _, errw, code := r.run(c.args...); code != c.code {
			t.Errorf("%v: exit %d want %d (%s)", c.args, code, c.code, errw)
		}
	}
	if _, errw, _ := r.run("web"); !strings.Contains(errw, "not built yet") {
		t.Errorf("not-built message: %s", errw)
	}
	var out, errw bytes.Buffer
	if code := run(context.Background(), []string{"--root", "/", "status"}, &out, &errw, nil); code != 2 || !strings.Contains(errw.String(), "sandbox") {
		t.Errorf("drive root under the sandbox guard: %d %s", code, errw.String())
	}
	if code := run(context.Background(), []string{"--help"}, &out, &errw, nil); code != 0 || !strings.Contains(out.String(), "cam add") {
		t.Errorf("--help: %d", code)
	}
}

// Every command run leaves one begin and one result record with the required fields; a failure is
// ERROR or WARN; mutating ones reach the audit stream.
func TestCommandsLogBeginAndResult(t *testing.T) {
	r := newRig(t)
	r.logSpec = "debug" // begin and read-only results are DEBUG records (LLD 2.13)
	runs := [][]string{
		{"status"}, {"cam", "list"}, {"cam", "show", "Front"}, {"cam", "add", "--name", "Side", "--ip", "192.0.2.12"},
		{"cam", "set", "Side", "--record", "true"}, {"cam", "disable", "Side"}, {"cam", "enable", "Side"}, {"cam", "remove", "Side"},
		{"config", "get", "SegmentSeconds"}, {"config", "set", "ReserveGB", "5"}, {"config", "unset", "ReserveGB"}, {"config", "validate"},
		{"config", "show"}, {"live", "status"}, {"live", "enable"}, {"live", "disable"}, {"log", "level", "all", "info", "--save"},
		{"logs"}, {"down"}, {"alert", "test", "log"}, {"web"}, {"config", "set", "RootPath", "/x"},
	}
	for _, a := range runs {
		r.run(a...)
	}
	lines := strings.Split(r.log(), "\n")
	field := regexp.MustCompile(`action_id=([0-9a-f]+)`)
	type rec struct {
		begin, result int
		level, action string
	}
	byID := map[string]*rec{}
	for _, l := range lines {
		if !strings.Contains(l, "[cli] action.") {
			continue
		}
		m := field.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("no action_id: %s", l)
			continue
		}
		for _, f := range []string{"action=", "actor=cli:", "pid="} {
			if !strings.Contains(l, f) {
				t.Errorf("missing %s: %s", f, l)
			}
		}
		x := byID[m[1]]
		if x == nil {
			x = &rec{}
			byID[m[1]] = x
		}
		x.action = regexp.MustCompile(`action=(\S+)`).FindStringSubmatch(l)[1]
		if strings.Contains(l, "action.begin") {
			x.begin++
		} else {
			x.result++
			x.level = strings.Fields(l)[1]
			if !strings.Contains(l, "result=") || !strings.Contains(l, "duration_ms=") {
				t.Errorf("result fields: %s", l)
			}
		}
	}
	if len(byID) != len(runs) {
		t.Errorf("%d action ids for %d runs", len(byID), len(runs))
	}
	for id, x := range byID {
		if x.begin != 1 || x.result != 1 {
			t.Errorf("%s %s: begin %d result %d", id, x.action, x.begin, x.result)
		}
		if (x.action == "web.serve" || x.action == "nvr.status") && x.level != "ERROR" && x.level != "WARN" {
			t.Errorf("%s failed at %s", x.action, x.level)
		}
		if x.action == "config.set" && x.level != "INFO" && x.level != "WARN" {
			t.Errorf("config.set at %s", x.level)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(r.root, "_logs", "audit.log"))
	for _, a := range []string{"camera.add", "camera.remove", "config.set", "live.enable", "nvr.down"} {
		if !strings.Contains(string(audit), "action="+a) {
			t.Errorf("%s not audited", a)
		}
	}
	if strings.Contains(r.log(), "action=nvr.status") && strings.Contains(string(audit), "action=nvr.status") {
		t.Error("a read-only command in the audit stream")
	}
}

func TestJSONOutputsAreValid(t *testing.T) {
	r := newRig(t)
	r.running()
	for _, a := range [][]string{{"status", "--json"}, {"--json", "cam", "list"}, {"cam", "show", "Front", "--json"}, {"config", "show", "--json"},
		{"live", "status", "--json"}, {"config", "get", "alerts", "--json"}, {"--json", "down"}} {
		out, errw, code := r.run(a...)
		var v any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Errorf("%v (exit %d): not JSON: %v\n%s%s", a, code, err, out, errw)
		}
	}
	out, _, code := r.run("status", "--json")
	var st statusReport
	if json.Unmarshal([]byte(out), &st); code != 0 || !st.Running || st.Status == nil || st.Autostart != "not built yet" || st.Alerts["desktop"] {
		t.Errorf("status --json: exit %d %+v", code, st)
	}
}

func TestConfigSetValidationLeavesFileUntouched(t *testing.T) {
	r := newRig(t)
	before, _ := os.ReadFile(r.cfg)
	for _, kv := range [][2]string{{"SegmentSeconds", "0"}, {"SegmentSeconds", "abc"}, {"LiveViewEnabled", "maybe"}, {"RootPath", "/x"},
		{"logging.level", "loud"}, {"alerts.push.enabled", "true"}, {"alerts.webhook.url", "http://example.org/"}} {
		if _, _, code := r.run("config", "set", kv[0], kv[1]); code != 1 {
			t.Errorf("%v: exit %d", kv, code)
		}
		if after, _ := os.ReadFile(r.cfg); !bytes.Equal(before, after) {
			t.Fatalf("%v changed the file", kv)
		}
	}
	out, _, code := r.run("config", "set", "MaxRetentionDays", "30")
	after, _ := os.ReadFile(r.cfg)
	if code != 0 || !strings.Contains(out, "-> 30") || !bytes.HasPrefix(after, []byte("\xef\xbb\xbf")) || !bytes.Contains(after, []byte(`"Unknown"`)) || !bytes.Contains(after, []byte(`"HealthStripHeightPx"`)) {
		t.Errorf("set: %d %s\n%s", code, out, after)
	}
	if out, _, _ := r.run("config", "get", "MaxRetentionDays"); strings.TrimSpace(out) != "30" {
		t.Errorf("get %q", out)
	}
}

func TestCamRoundTripKeepsBOMAndUnknownFields(t *testing.T) {
	r := newRig(t)
	for _, a := range [][]string{{"cam", "add", "--name", "Side", "--ip", "192.0.2.12", "--quality", "stream2", "--record"},
		{"cam", "set", "Back", "--ip", "192.0.2.21"}, {"cam", "disable", "Front"}, {"cam", "remove", "Side"}} {
		if out, errw, code := r.run(a...); code != 0 {
			t.Fatalf("%v: %d %s %s", a, code, out, errw)
		}
	}
	b, _ := os.ReadFile(r.cameras())
	if !bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) || !bytes.Contains(b, []byte(`"Note"`)) || bytes.Contains(b, []byte("Side")) {
		t.Errorf("%s", b)
	}
	c, err := config.LoadCameras(r.cameras())
	if err != nil || len(c.List) != 2 || c.List[0].Record || c.List[1].IP != "192.0.2.21" {
		t.Errorf("%+v %v", c.List, err)
	}
	before, _ := os.ReadFile(r.cameras())
	if _, _, code := r.run("cam", "add", "--name", "front", "--ip", "192.0.2.30"); code != 1 {
		t.Error("duplicate accepted")
	}
	if _, _, code := r.run("cam", "add", "--name", "X", "--ip", "rtsp://u:p@h/"); code != 1 {
		t.Error("credential URL accepted as an address")
	}
	for _, bad := range []string{"192.0.2.31:0", "192.0.2.31:70000", "192.0.2.31:x", "h:1:2"} {
		if _, _, code := r.run("cam", "add", "--name", "X", "--ip", bad); code != 1 {
			t.Errorf("address %q accepted", bad)
		}
		if _, _, code := r.run("cam", "set", "Back", "--ip", bad); code != 1 {
			t.Errorf("cam set accepted %q", bad)
		}
	}
	if after, _ := os.ReadFile(r.cameras()); !bytes.Equal(before, after) {
		t.Error("a refused add changed the file")
	}
	for _, a := range [][]string{{"cam", "add", "--name", "Gate", "--ip", "127.0.0.1:18700", "--quality", "gate", "--record"}, {"cam", "set", "Back", "--ip", "cam.example.com:8554"}} {
		if out, errw, code := r.run(a...); code != 0 {
			t.Fatalf("%v: %d %s %s", a, code, out, errw)
		}
	}
	c, err = config.LoadCameras(r.cameras())
	if err != nil || len(c.List) != 3 || c.List[2].IP != "127.0.0.1:18700" || c.List[1].IP != "cam.example.com:8554" {
		t.Errorf("%+v %v", c.List, err)
	}
	if out, _, code := r.run("cam", "show", "Gate"); code != 0 || !strings.Contains(out, "127.0.0.1:18700") {
		t.Errorf("show: %d %s", code, out)
	}
}

// With a running supervisor the CLI goes through the control API, carrying its own action id.
func TestRunningSupervisorThroughTheAPI(t *testing.T) {
	r := newRig(t)
	r.running()
	if out, _, code := r.run("cam", "disable", "Front"); code != 0 || !strings.Contains(out, "reloaded") {
		t.Errorf("disable: %d %s", code, out)
	}
	if out, _, code := r.run("log", "level", "supervisor", "debug"); code != 0 || !strings.Contains(out, "supervisor=DEBUG") {
		t.Errorf("log level: %d %s", code, out)
	}
	if _, _, code := r.run("live", "enable"); code != 0 || !r.ctl.live {
		t.Errorf("live: %d", code)
	}
	if b, _ := os.ReadFile(r.cfg); bytes.Contains(b, []byte("LiveViewEnabled")) {
		t.Error("the CLI edited LiveViewEnabled itself instead of the supervisor")
	}
	want := []string{"reload", "level:supervisor=debug", "live"}
	if strings.Join(r.ctl.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v", r.ctl.calls)
	}
	logText := r.log()
	for i, id := range r.ctl.ids {
		if id == "" || !strings.Contains(logText, "action_id="+id) {
			t.Errorf("call %s carried id %q which is not the CLI action's", want[i], id)
		}
	}
	// down: the API stop, then proved by the census (status now stopped)
	r.ctl.onStop = r.stopped
	out, _, code := r.run("down")
	if code != 0 || !strings.Contains(out, "0 survivors") || r.ctl.calls[len(r.ctl.calls)-1] != "stop:requested" {
		t.Errorf("down: %d %s %v", code, out, r.ctl.calls)
	}
	if out, _, code := r.run("down"); code != 0 || !strings.Contains(out, "nothing is running") {
		t.Errorf("second down: %d %s", code, out)
	}
	// up when one is running says so and starts nothing
	r.running()
	if out, _, code := r.run("up", "--detach"); code != 0 || !strings.Contains(out, "already running") {
		t.Errorf("up: %d %s", code, out)
	}
}

func TestDownWithoutAPIUsesTheStopFileAndReportsSurvivors(t *testing.T) {
	r := newRig(t)
	r.running()
	r.ctl.err = ctlapi.ErrUnreachable
	out, _, code := r.run("down")
	if code != 1 || !strings.Contains(out, "DID NOT STOP") || !strings.Contains(out, "stop.request") {
		t.Errorf("%d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(r.root, "_state", "stop.request")); err != nil {
		t.Error("no stop request")
	}
}

func TestLogsFilters(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(filepath.Join(r.root, "_logs"), 0o755)
	now := time.Now()
	old := now.Add(-2 * time.Hour).Format("2006-01-02T15:04:05.000000-07:00")
	cur := now.Format("2006-01-02T15:04:05.000000-07:00")
	text := old + " INFO  [supervisor] old line action=camera.add\n" +
		cur + " DEBUG [supervisor] debug line\n" +
		cur + " WARN  [ffmpeg:Front] camera warn\n" +
		cur + " ERROR [relay] relay error action=relay.start\n"
	os.WriteFile(filepath.Join(r.root, "_logs", "holocron.log"), []byte(text), 0o644)
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"--level", "warn"}, []string{"camera warn", "relay error"}},
		{[]string{"--component", "ffmpeg"}, []string{"camera warn"}},
		{[]string{"--since", "1h", "--component", "supervisor"}, []string{"debug line"}},
		{[]string{"--action", "relay.start"}, []string{"relay error"}},
	} {
		out, _, code := r.run(append([]string{"logs"}, c.args...)...)
		var got []string
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l != "" && !strings.Contains(l, "action.") {
				parsed, _ := parseLogLine(l)
				got = append(got, strings.TrimSpace(strings.SplitN(parsed.Text, " action=", 2)[0]))
			}
		}
		if code != 0 || strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%v: %d %v", c.args, code, got)
		}
	}
}

func TestLogsFollow(t *testing.T) {
	r := newRig(t)
	path := filepath.Join(r.root, "_logs", "holocron.log")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, nil, 0o644)
	old := followPoll
	followPoll = 20 * time.Millisecond
	defer func() { followPoll = old }()
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuf
	done := make(chan struct{})
	go func() {
		e := &env{out: &out, paths: app.Paths{Root: r.root}, args: []string{"-f", "--component", "relay"}}
		cmdLogs(ctx, e)
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	ts := time.Now().Format("2006-01-02T15:04:05.000000-07:00")
	f.WriteString(ts + " INFO  [relay] followed\n" + ts + " INFO  [cli] other\n")
	f.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "followed") {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(out.String(), "followed") || strings.Contains(out.String(), "other") {
		t.Errorf("follow: %q", out.String())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestCamTestProbe(t *testing.T) {
	r := newRig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(c).ReadString('\n')
			c.Write([]byte("RTSP/1.0 401 Unauthorized\r\nCSeq: 1\r\n\r\n"))
			c.Close()
		}
	}()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedAddr := closed.Addr().String()
	closed.Close()
	os.WriteFile(r.cameras(), []byte(`[{"Name":"Up","IP":"`+ln.Addr().String()+`"},{"Name":"Down","IP":"`+closedAddr+`"}]`), 0o644)
	if out, _, code := r.run("cam", "test", "Up"); code != 0 || !strings.Contains(out, "reachable") || !strings.Contains(out, "401") {
		t.Errorf("%d %s", code, out)
	}
	if out, _, code := r.run("cam", "test", "Down"); code != 1 || !strings.Contains(out, "unreachable") {
		t.Errorf("%d %s", code, out)
	}
	if got := rtspTarget(config.Camera{IP: "10.0.0.5"}); got != "10.0.0.5:554" {
		t.Error(got)
	}
}
