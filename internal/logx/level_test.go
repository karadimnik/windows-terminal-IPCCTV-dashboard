package logx

import (
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
		ok   bool
	}{
		{"error", LevelError, true}, {"WARN", LevelWarn, true}, {" Info ", LevelInfo, true},
		{"dEbUg", LevelDebug, true}, {"TRACE", LevelTrace, true},
		{"", 0, false}, {"warning", 0, false}, {"verbose", 0, false}, {"5", 0, false},
	}
	for _, tc := range tests {
		got, err := ParseLevel(tc.in)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("ParseLevel(%q) = %v, %v", tc.in, got, err)
		}
	}
}

func TestLevelName(t *testing.T) {
	for l, want := range map[slog.Level]string{LevelError: "ERROR", LevelWarn: "WARN", LevelInfo: "INFO", LevelDebug: "DEBUG", LevelTrace: "TRACE", LevelTrace - 3: "TRACE", LevelInfo + 1: "INFO"} {
		if got := LevelName(l); got != want {
			t.Errorf("LevelName(%d) = %s, want %s", l, got, want)
		}
	}
}

func TestParseSpec(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		def      string // "" = none
		comps    map[string]slog.Level
		wantErrs int
	}{
		{"empty", "", "", nil, 0},
		{"bare default", "debug", "debug", nil, 0},
		{"per component", "supervisor=debug,web=info", "", map[string]slog.Level{"supervisor": LevelDebug, "web": LevelInfo}, 0},
		{"default and component, spaces, case", " WARN , Relay = trace ,", "warn", map[string]slog.Level{"relay": LevelTrace}, 0},
		{"invalid ignored", "supervisor=loud,web=info,nonsense,=debug", "", map[string]slog.Level{"web": LevelInfo}, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def, comps, errs := ParseSpec(tc.in)
			if len(errs) != tc.wantErrs {
				t.Errorf("errs = %v, want %d", errs, tc.wantErrs)
			}
			if (def == nil) != (tc.def == "") || (def != nil && LevelName(*def) != strings.ToUpper(tc.def)) {
				t.Errorf("default = %v, want %q", def, tc.def)
			}
			if len(comps) != len(tc.comps) {
				t.Errorf("comps = %v, want %v", comps, tc.comps)
			}
			for k, v := range tc.comps {
				if comps[k] != v {
					t.Errorf("comps[%s] = %v, want %v", k, comps[k], v)
				}
			}
		})
	}
}

func newMem(t *testing.T, o Options) (*Logger, *MemorySink) {
	t.Helper()
	mem := NewMemorySink()
	o.Sink = mem
	if o.Getenv == nil {
		o.Getenv = func(string) string { return "" }
	}
	l, errs := New(o)
	if len(errs) != 0 {
		t.Fatalf("unexpected option errors: %v", errs)
	}
	return l, mem
}

func TestPrecedenceConfigAndEnv(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		env  string
		comp string
		want slog.Level
		errs int
	}{
		{"default INFO", Config{}, "", "web", LevelInfo, 0},
		{"config default", Config{Level: "warn"}, "", "web", LevelWarn, 0},
		{"config override beats default", Config{Level: "warn", Levels: map[string]string{"web": "debug"}}, "", "web", LevelDebug, 0},
		{"other component keeps default", Config{Level: "warn", Levels: map[string]string{"web": "debug"}}, "", "relay", LevelWarn, 0},
		{"env beats config default", Config{Level: "warn"}, "trace", "relay", LevelTrace, 0},
		{"env component beats config component", Config{Levels: map[string]string{"web": "debug"}}, "web=error", "web", LevelError, 0},
		{"invalid ignored and reported", Config{Level: "loud"}, "web=quiet", "web", LevelInfo, 2},
		{"camera falls back to ffmpeg", Config{Levels: map[string]string{"ffmpeg": "trace"}}, "", "ffmpeg:cam1", LevelTrace, 0},
		{"exact camera beats ffmpeg", Config{Levels: map[string]string{"ffmpeg": "trace", "ffmpeg:cam1": "error"}}, "", "ffmpeg:cam1", LevelError, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, errs := New(Options{Config: tc.cfg, Getenv: func(k string) string {
				if k != EnvVar {
					t.Errorf("unexpected env read %s", k)
				}
				return tc.env
			}, Sink: NewMemorySink()})
			if len(errs) != tc.errs {
				t.Errorf("errs = %v, want %d", errs, tc.errs)
			}
			if got := l.Levels().Level(tc.comp); got != tc.want {
				t.Errorf("level = %s, want %s", LevelName(got), LevelName(tc.want))
			}
		})
	}
}

func TestRuntimeChangeOnExistingLogger(t *testing.T) {
	l, mem := newMem(t, Options{})
	web := l.For("web")
	relay := l.For("relay")

	web.Debug("d1")
	relay.Debug("r1")
	if len(mem.Lines()) != 0 {
		t.Fatalf("DEBUG logged at default INFO: %v", mem.Lines())
	}
	l.Levels().SetComponent("web", LevelDebug)
	web.Debug("d2")
	relay.Debug("r2")
	if got := mem.String(); !strings.Contains(got, "d2") || strings.Contains(got, "r2") {
		t.Fatalf("component override not applied to existing logger only for web: %q", got)
	}
	l.Levels().SetDefault(LevelTrace)
	relay.Log(nil, LevelTrace, "r3") //nolint:staticcheck
	web.Log(nil, LevelTrace, "d3")   //nolint:staticcheck
	got := mem.String()
	if !strings.Contains(got, "r3") || strings.Contains(got, "d3") {
		t.Fatalf("default change: relay should now log TRACE, web (override DEBUG) should not: %q", got)
	}
	l.Levels().Clear("web")
	web.Log(nil, LevelTrace, "d4") //nolint:staticcheck
	if !strings.Contains(mem.String(), "d4") {
		t.Fatal("Clear did not return web to the default")
	}
	l.Levels().SetDefault(LevelError)
	web.Warn("w1")
	if strings.Contains(mem.String(), "w1") {
		t.Fatal("WARN logged at ERROR")
	}
	if s := l.Levels().Snapshot().String(); s != "error" {
		t.Fatalf("snapshot = %q", s)
	}
}
