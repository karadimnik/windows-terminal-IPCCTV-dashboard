package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"holocron/internal/logx"
)

func ok(context.Context, Input) (Output, error) { return Output{}, nil }

// fakeActions covers read-only, mutating, audit, refusing, failing and panicking handlers.
func fakeActions() []Action {
	return []Action{
		{Name: "status.show", Component: "cli", Run: ok},
		{Name: "camera.list", Component: "ctlapi", Run: func(context.Context, Input) (Output, error) {
			return Output{Target: "all", Data: []string{"a"}}, nil
		}},
		{Name: "camera.add", Component: "supervisor", Mutating: true, Run: ok},
		{Name: "user.add", Component: "auth", Mutating: true, Audit: true, Run: ok},
		{Name: "audit.view", Component: "auth", Audit: true, Run: ok},
		{Name: "config.get", Component: "web", Run: func(context.Context, Input) (Output, error) { return Output{}, Refuse("guard %s", "view-only") }},
		{Name: "camera.remove", Component: "supervisor", Mutating: true, Run: func(context.Context, Input) (Output, error) {
			return Output{}, fmt.Errorf("wrapped: %w", Refuse("camera is recording"))
		}},
		{Name: "relay.probe", Component: "relay", Run: func(context.Context, Input) (Output, error) { return Output{}, errors.New("relay down") }},
		{Name: "config.set", Component: "cli", Mutating: true, Run: func(context.Context, Input) (Output, error) { return Output{}, errors.New("write failed") }},
		{Name: "install.run", Component: "install", Mutating: true, Audit: true, Run: func(context.Context, Input) (Output, error) { return Output{}, errors.New("no permission") }},
		{Name: "user.del", Component: "auth", Mutating: true, Audit: true, Run: func(context.Context, Input) (Output, error) { panic("boom") }},
		{Name: "web.peek", Component: "web", Run: func(context.Context, Input) (Output, error) { var m map[string]int; m["x"] = 1; return Output{}, nil }},
	}
}

type expect struct {
	level  string
	result string
	err    bool
	audit  bool
}

var expectations = map[string]expect{
	"status.show":   {"DEBUG", "ok", false, false},
	"camera.list":   {"DEBUG", "ok", false, false},
	"camera.add":    {"INFO", "ok", false, true},
	"user.add":      {"INFO", "ok", false, true},
	"audit.view":    {"DEBUG", "ok", false, true},
	"config.get":    {"WARN", "refused", true, false},
	"camera.remove": {"WARN", "refused", true, true},
	"relay.probe":   {"ERROR", "fail", true, false},
	"config.set":    {"ERROR", "fail", true, true},
	"install.run":   {"ERROR", "fail", true, true},
	"user.del":      {"ERROR", "fail", true, true},
	"web.peek":      {"ERROR", "fail", true, false},
}

type rig struct {
	runner *Runner
	reg    *Registry
	mem    *logx.MemorySink
	audit  *logx.MemorySink
	log    *logx.Logger
}

func newRig(t *testing.T, level string) *rig {
	t.Helper()
	reg := NewRegistry()
	for _, a := range fakeActions() {
		reg.Register(a)
	}
	mem, audit := logx.NewMemorySink(), logx.NewMemorySink()
	l, errs := logx.New(logx.Options{Config: logx.Config{Level: level}, JSON: true, Sink: mem, Audit: audit, Getenv: func(string) string { return "" }})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	return &rig{runner: NewRunner(reg, l), reg: reg, mem: mem, audit: audit, log: l}
}

func records(t *testing.T, m *logx.MemorySink) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range m.Lines() {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad JSON record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestEveryActionLeavesOneBeginAndOneResult(t *testing.T) {
	if len(fakeActions()) != len(expectations) {
		t.Fatal("expectations out of step with fake actions")
	}
	for _, a := range newRig(t, "debug").reg.All() {
		t.Run(a.Name, func(t *testing.T) {
			rg := newRig(t, "debug")
			ctx := WithActor(context.Background(), "cli:tester")
			_, err := rg.runner.Do(ctx, a.Name, Input{Target: "cam1"})
			want := expectations[a.Name]
			if (err != nil) != want.err {
				t.Fatalf("err = %v, want error=%v", err, want.err)
			}
			recs := records(t, rg.mem)
			if len(recs) != 2 {
				t.Fatalf("%d records, want exactly begin + result: %v", len(recs), rg.mem.Lines())
			}
			begin, result := recs[0], recs[1]
			if begin["msg"] != "action.begin" || begin["level"] != "DEBUG" {
				t.Errorf("begin = %v", begin)
			}
			if result["msg"] != "action.result" || result["level"] != want.level || result["result"] != want.result {
				t.Errorf("result = %v, want level %s result %s", result, want.level, want.result)
			}
			for _, rec := range recs {
				for _, f := range []string{"time", "level", "component", "action", "actor", "target", "action_id", "pid"} {
					if _, present := rec[f]; !present {
						t.Errorf("%s record lacks %q: %v", rec["msg"], f, rec)
					}
				}
				if rec["action"] != a.Name || rec["actor"] != "cli:tester" || rec["component"] != a.Component {
					t.Errorf("wrong identity fields: %v", rec)
				}
				if (rec["audit"] == true) != want.audit {
					t.Errorf("audit flag = %v, want %v", rec["audit"], want.audit)
				}
			}
			if _, present := result["duration_ms"]; !present {
				t.Errorf("result lacks duration_ms: %v", result)
			}
			if begin["action_id"] != result["action_id"] || len(result["action_id"].(string)) != 16 {
				t.Errorf("action ids differ or are not 8 bytes hex: %v / %v", begin["action_id"], result["action_id"])
			}
			if _, present := result["err"]; present != want.err {
				t.Errorf("err attribute present = %v, want %v", present, want.err)
			}
			wantTarget := "cam1"
			if a.Name == "camera.list" {
				wantTarget = "all"
			}
			if result["target"] != wantTarget {
				t.Errorf("result target = %v, want %s", result["target"], wantTarget)
			}
			if got := len(records(t, rg.audit)); (got > 0) != want.audit || (want.audit && got != 2) {
				t.Errorf("audit stream has %d records, audit=%v", got, want.audit)
			}
		})
	}
}

func TestDefaultLevelKeepsOnlyWhatMatters(t *testing.T) {
	for name, want := range expectations {
		rg := newRig(t, "")
		rg.runner.Do(context.Background(), name, Input{})
		recs := records(t, rg.mem)
		visible := want.level != "DEBUG"
		if (len(recs) == 1) != visible || len(recs) > 1 {
			t.Errorf("%s: %d records at INFO, visible=%v", name, len(recs), visible)
			continue
		}
		if visible && recs[0]["level"] != want.level {
			t.Errorf("%s: level %v, want %s", name, recs[0]["level"], want.level)
		}
	}
}

func TestPanicBecomesErrorResultAndProcessSurvives(t *testing.T) {
	rg := newRig(t, "")
	_, err := rg.runner.Do(context.Background(), "user.del", Input{})
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("err = %v", err)
	}
	recs := records(t, rg.mem)
	if len(recs) != 1 || recs[0]["level"] != "ERROR" || !strings.Contains(recs[0]["err"].(string), "boom") || recs[0]["stack"] == nil {
		t.Fatalf("records = %v", recs)
	}
	if _, err := rg.runner.Do(context.Background(), "camera.add", Input{}); err != nil {
		t.Errorf("runner unusable after a panic: %v", err)
	}
}

func TestRefusedMatchesErrRefused(t *testing.T) {
	rg := newRig(t, "")
	_, err := rg.runner.Do(context.Background(), "camera.remove", Input{})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "camera is recording") {
		t.Errorf("err = %v", err)
	}
}

func TestActionIDPropagation(t *testing.T) {
	var seen string
	rg := newRig(t, "debug")
	rg.reg.Register(Action{Name: "probe.id", Component: "cli", Run: func(ctx context.Context, in Input) (Output, error) {
		seen = ActionIDFrom(ctx)
		return Output{}, nil
	}})
	tests := []struct {
		name string
		ctx  context.Context
		in   Input
		want string // "" = generated
	}{
		{"generated", context.Background(), Input{}, ""},
		{"from input", context.Background(), Input{ActionID: "aaaabbbbccccdddd"}, "aaaabbbbccccdddd"},
		{"from context", WithActionID(context.Background(), "1111222233334444"), Input{}, "1111222233334444"},
		{"input beats context", WithActionID(context.Background(), "1111222233334444"), Input{ActionID: "aaaabbbbccccdddd"}, "aaaabbbbccccdddd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rg.mem = logx.NewMemorySink()
			l, _ := logx.New(logx.Options{Config: logx.Config{Level: "debug"}, JSON: true, Sink: rg.mem, Getenv: func(string) string { return "" }})
			rg.runner.Logs = l
			rg.runner.Do(tc.ctx, "probe.id", tc.in)
			recs := records(t, rg.mem)
			if len(recs) != 2 || recs[0]["action_id"] != recs[1]["action_id"] || recs[0]["action_id"] != seen {
				t.Fatalf("ids differ: records %v, handler saw %q", recs, seen)
			}
			if tc.want != "" && seen != tc.want {
				t.Errorf("id = %q, want %q", seen, tc.want)
			}
		})
	}
	a, b := NewActionID(), NewActionID()
	if a == b || len(a) != 16 {
		t.Errorf("ids %q %q", a, b)
	}
}

func TestDurationUsesRunnerClock(t *testing.T) {
	rg := newRig(t, "debug")
	t0 := time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC)
	calls := 0
	rg.runner.Now = func() time.Time { calls++; return t0.Add(time.Duration(calls) * 750 * time.Millisecond) }
	rg.runner.Do(context.Background(), "status.show", Input{})
	recs := records(t, rg.mem)
	if recs[1]["duration_ms"] != float64(750) {
		t.Errorf("duration_ms = %v, want 750", recs[1]["duration_ms"])
	}
}

func TestActorDefaultsAndHelpers(t *testing.T) {
	rg := newRig(t, "debug")
	rg.runner.Do(context.Background(), "status.show", Input{})
	if got := records(t, rg.mem)[0]["actor"]; got != ActorSystem {
		t.Errorf("default actor = %v", got)
	}
	if !strings.HasPrefix(CLIActor(), "cli:") || len(CLIActor()) <= 4 || WebActor("ann") != "web:ann" {
		t.Errorf("actor helpers: %q %q", CLIActor(), WebActor("ann"))
	}
}

func TestUnknownActionIsLoggedAsError(t *testing.T) {
	rg := newRig(t, "")
	_, err := rg.runner.Do(context.Background(), "nope.nope", Input{})
	recs := records(t, rg.mem)
	if !errors.Is(err, ErrUnknownAction) || len(recs) != 1 || recs[0]["level"] != "ERROR" || recs[0]["action"] != "nope.nope" {
		t.Errorf("err %v, records %v", err, recs)
	}
}

type recordingTB struct{ failures []string }

func (r *recordingTB) Helper() {}
func (r *recordingTB) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestAssertAllExposedAreActions(t *testing.T) {
	reg := newRig(t, "").reg
	good := &recordingTB{}
	AssertExposedIn(good, reg, Exposed{"camera.add", "status.show"})
	if len(good.failures) != 0 {
		t.Errorf("registered names flagged: %v", good.failures)
	}
	// Negative control: an exposed name that is not an Action must fail the assertion.
	bad := &recordingTB{}
	AssertExposedIn(bad, reg, Exposed{"camera.add", "camera.explode", "GET /api/secret"})
	if len(bad.failures) != 2 || !strings.Contains(bad.failures[0], "camera.explode") {
		t.Errorf("negative control: failures = %v, want two", bad.failures)
	}
	// The public form checks the Default registry (empty here), so any name fails.
	pub := &recordingTB{}
	AssertAllExposedAreActions(pub, []string{"never.registered"})
	if len(pub.failures) != 1 {
		t.Errorf("AssertAllExposedAreActions did not fail on an unregistered name: %v", pub.failures)
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Action{Name: "b.x", Component: "cli", Run: ok})
	reg.Register(Action{Name: "a.x", Component: "cli", Run: ok})
	if all := reg.All(); len(all) != 2 || all[0].Name != "a.x" {
		t.Errorf("All = %v", all)
	}
	if _, found := reg.Lookup("c.x"); found {
		t.Error("Lookup found a missing action")
	}
	panics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	panics("duplicate", func() { reg.Register(Action{Name: "a.x", Component: "cli", Run: ok}) })
	panics("empty name", func() { reg.Register(Action{Component: "cli", Run: ok}) })
	panics("nil run", func() { reg.Register(Action{Name: "n.r", Component: "cli"}) })
	panics("no component", func() { reg.Register(Action{Name: "n.c", Run: ok}) })
}

func TestConcurrentDo(t *testing.T) {
	rg := newRig(t, "debug")
	var wg sync.WaitGroup
	names := make([]string, 0, len(expectations))
	for n := range expectations {
		names = append(names, n)
	}
	for g := 0; g < 100; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for _, n := range names {
				rg.runner.Do(WithActor(context.Background(), fmt.Sprintf("web:u%d", g)), n, Input{Target: "t"})
			}
		}(g)
	}
	wg.Wait()
	if got, want := len(rg.mem.Lines()), 100*len(names)*2; got != want {
		t.Errorf("%d records, want %d", got, want)
	}
	rg.log.Levels().SetDefault(logx.LevelError) // run-time change while idle is race-clean too
}

// unauditedMutating lists the Mutating actions of a registry that are not audited.
func unauditedMutating(r *Registry) (bad []string) {
	for _, a := range r.All() {
		if a.Mutating && !a.Audit {
			bad = append(bad, a.Name)
		}
	}
	return bad
}

func TestMutatingActionsAreAudited(t *testing.T) {
	rg := newRig(t, "")
	if bad := unauditedMutating(rg.reg); len(bad) != 0 {
		t.Fatalf("Mutating actions registered without Audit: %v", bad)
	}
	for _, a := range rg.reg.All() {
		before := len(rg.audit.Lines())
		rg.runner.Do(context.Background(), a.Name, Input{})
		if a.Mutating && len(rg.audit.Lines()) == before {
			t.Errorf("Mutating action %s produced no audit record", a.Name)
		}
	}
	// A read-only action can still opt in; one that does not stays out of the audit stream.
	if a, _ := rg.reg.Lookup("audit.view"); !a.Audit || a.Mutating {
		t.Errorf("audit.view = %+v", a)
	}
	if a, _ := rg.reg.Lookup("status.show"); a.Audit {
		t.Error("read-only action must not become audited")
	}
	// Negative control: an action that bypasses Register is flagged by the check.
	rg.reg.actions["sneaky.write"] = Action{Name: "sneaky.write", Component: "cli", Mutating: true, Run: ok}
	if bad := unauditedMutating(rg.reg); len(bad) != 1 || bad[0] != "sneaky.write" {
		t.Errorf("negative control: unaudited = %v, want [sneaky.write]", bad)
	}
}
