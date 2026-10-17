package logx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var (
	fixedZone = time.FixedZone("EEST", 3*3600)
	fixedTime = time.Date(2026, 10, 17, 13, 44, 5, 123456000, fixedZone)
	fixedNow  = func() time.Time { return fixedTime }
)

func TestFormatGolden(t *testing.T) {
	tests := []struct {
		name string
		json bool
		log  func(*slog.Logger)
		want string
	}{
		{"text plain", false, func(l *slog.Logger) { l.Info("camera added") },
			"2026-10-17T13:44:05.123456+03:00 INFO  [supervisor] camera added"},
		{"text attrs quoted", false, func(l *slog.Logger) {
			l.Warn("restart with backoff", logx2("camera", "front door"), slog.Int("attempt", 3), Err(errors.New("exit status 1")))
		}, `2026-10-17T13:44:05.123456+03:00 WARN  [supervisor] restart with backoff camera="front door" attempt=3 err="exit status 1"`},
		{"text error level, group", false, func(l *slog.Logger) {
			l.Error("failed", Action("camera.add"), Result(ResultFail), slog.Group("req", slog.String("path", "/a b"), slog.Int("n", 2)), slog.Bool("x", true))
		}, `2026-10-17T13:44:05.123456+03:00 ERROR [supervisor] failed action=camera.add result=fail req.path="/a b" req.n=2 x=true`},
		{"text trace and debug padding", false, func(l *slog.Logger) {
			l.Log(nil, LevelTrace, "raw") //nolint:staticcheck
			l.Debug("dec")
		}, "2026-10-17T13:44:05.123456+03:00 TRACE [supervisor] raw\n2026-10-17T13:44:05.123456+03:00 DEBUG [supervisor] dec"},
		{"json", true, func(l *slog.Logger) {
			l.Info("camera added", Action("camera.add"), Actor("cli:nik"), DurationMS(1500*time.Millisecond), slog.Group("g", slog.String("k", "v")))
		}, `{"time":"2026-10-17T13:44:05.123456+03:00","level":"INFO","component":"supervisor","msg":"camera added","action":"camera.add","actor":"cli:nik","duration_ms":1500,"g":{"k":"v"}}`},
		{"json no attrs", true, func(l *slog.Logger) { l.Debug("x") },
			`{"time":"2026-10-17T13:44:05.123456+03:00","level":"DEBUG","component":"supervisor","msg":"x"}`},
		{"with attrs and group", false, func(l *slog.Logger) {
			l.With("a", 1).WithGroup("g").With("b", 2).Info("m", "c", 3)
		}, "2026-10-17T13:44:05.123456+03:00 INFO  [supervisor] m a=1 g.b=2 g.c=3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, mem := newMem(t, Options{JSON: tc.json, Now: fixedNow, Location: fixedZone, Config: Config{Level: "trace"}})
			tc.log(l.For("supervisor"))
			if got := mem.String(); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			if tc.json {
				var m map[string]any
				if err := json.Unmarshal([]byte(mem.Lines()[0]), &m); err != nil {
					t.Errorf("not valid JSON: %v", err)
				}
			}
		})
	}
}

func logx2(k, v string) slog.Attr { return slog.String(k, v) }

func TestFormatOneLinePerRecord(t *testing.T) {
	l, mem := newMem(t, Options{})
	l.For("cli").Info("two\nlines", "k", "a\nb")
	if len(mem.Lines()) != 1 || strings.Count(mem.Lines()[0], "\n") != 0 {
		t.Fatalf("record spans lines: %q", mem.Lines())
	}
}
