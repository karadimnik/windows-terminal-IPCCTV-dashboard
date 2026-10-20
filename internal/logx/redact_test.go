package logx

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	regSecret  = "Zx9-registered-secret"
	urlPass    = "hunter22xyz"
	tokenValue = "tok-ABCDEF123456"
	cookieVal  = "sessioncookie-77aa"
	nestedPass = "nested-pw-value-1"
	withSecret = "with-attr-token-9"
	errSecret  = "err-embedded-" + regSecret
)

var plantedNeedles = []string{regSecret, urlPass, tokenValue, cookieVal, nestedPass, withSecret}

// plant logs every kind of secret at every level.
func plant(log *slog.Logger) {
	for _, lvl := range []slog.Level{LevelError, LevelWarn, LevelInfo, LevelDebug, LevelTrace} {
		log.Log(nil, lvl, "connecting to rtsp://admin:"+urlPass+"@10.0.0.5/stream using "+regSecret, //nolint:staticcheck
			slog.String("note", "plain "+regSecret),
			slog.String("url", "http://bob:"+urlPass+"@host:8000/x"),
			slog.String("Authorization", "Bearer "+tokenValue),
			slog.String("api_key", tokenValue),
			slog.Any("err", errors.New("dial failed for "+errSecret)),
			slog.Group("req", slog.Group("hdr", slog.String("Set-Cookie", cookieVal), slog.String("PassWord", nestedPass))),
			slog.Any("stringer", stringerOf("value "+regSecret)),
			slog.Any("struct", struct{ Raw string }{"raw " + regSecret}),
			Audit(),
		)
	}
}

type stringerOf string

func (s stringerOf) String() string { return string(s) }

func scan(name, haystack string, mustFind bool, t *testing.T) {
	t.Helper()
	for _, n := range plantedNeedles {
		found := strings.Contains(haystack, n)
		if mustFind && !found && (n == regSecret || n == urlPass) {
			t.Errorf("%s: positive control failed, scanner did not find %q", name, n)
		}
		if !mustFind && found {
			t.Errorf("%s: LEAK of %q", name, n)
		}
	}
}

func TestRedactionAtTraceWithPositiveControl(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", asJSON), func(t *testing.T) {
			dir := t.TempDir()
			mainFile := NewRotatingFile(FileOptions{Path: filepath.Join(dir, "holocron.log")})
			auditFile := NewRotatingFile(FileOptions{Path: filepath.Join(dir, "audit.log")})
			mem := NewMemorySink()
			auditMem := NewMemorySink()
			var errBuf bytes.Buffer
			l, errs := New(Options{
				Config: Config{Level: "trace"}, JSON: asJSON, Getenv: func(string) string { return "" },
				Sink:  FanOut(mainFile, mem, NewWriterSink(&errBuf)),
				Audit: FanOut(auditFile, auditMem),
			})
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			l.Redactor().Register(regSecret)
			l.Redactor().Register("ab") // too short: ignored

			log := l.For("web").With("with_token", withSecret).WithGroup("outer")
			plant(log)
			plant(l.For("ffmpeg:cam1"))
			l.Close()

			// Control: the same formatter and sinks but without any redaction.
			ctlMem := NewMemorySink()
			ctl := &shared{ctl: NewLevelController(LevelTrace), sink: ctlMem, json: asJSON, loc: fixedZone}
			plant(slog.New(core{s: ctl, component: "web"}).With("with_token", withSecret))

			scan("control (unredacted)", ctlMem.String(), true, t)
			if n := len(mem.Lines()); n != 10 {
				t.Fatalf("main memory sink got %d lines, want 10 (all five levels, two loggers)", n)
			}
			scan("memory sink", mem.String(), false, t)
			scan("stderr sink", errBuf.String(), false, t)
			scan("main file", readFile(t, filepath.Join(dir, "holocron.log")), false, t)
			scan("audit memory", auditMem.String(), false, t)
			scan("audit file", readFile(t, filepath.Join(dir, "audit.log")), false, t)
			if !strings.Contains(mem.String(), "rtsp://***:***@10.0.0.5/stream") || !strings.Contains(mem.String(), "***") {
				t.Errorf("expected masks in output: %s", mem.Lines()[0])
			}
		})
	}
}

func TestRedactorUnit(t *testing.T) {
	r := NewRedactor()
	r.Register("p@ss/word")
	r.Register("ab")
	tests := []struct{ in, want string }{
		{"rtsp://u:p@ss/word@host/s", "rtsp://***:***@host/s"},
		{"HTTP://user@host/", "HTTP://***:***@host/"},
		{"plain p%40ss%2Fword and p%40ss%2Fword", "plain *** and ***"},
		{"nothing ab here", "nothing ab here"},
		{"no scheme user:pass@host", "no scheme user:pass@host"},
	}
	for _, tc := range tests {
		if got := r.String(tc.in); got != tc.want {
			t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, k := range []string{"password", "Passwd", "client_secret", "X-Token", "Authorization", "Cookie", "pwHash", "apikey", "API_KEY"} {
		if got := r.attr(slog.String(k, "v")); got.Value.String() != "***" {
			t.Errorf("key %q not masked", k)
		}
	}
	if got := r.attr(slog.String("user", "v")); got.Value.String() != "v" {
		t.Error("ordinary key masked")
	}
	if got := r.attr(slog.Int("tokens_total", 3)); got.Value.String() != "***" {
		t.Error("sensitive key must be masked whatever the value kind")
	}
}

// A secret with every awkward character is masked in its raw, query, path and userinfo forms. The
// userinfo form differs from the other three, so a redactor knowing only those leaks it (positive control).
func TestRedactorMasksUserinfoEscapedForm(t *testing.T) {
	const secret = "a@b:c/d%e f+g&h#i"
	userinfo := strings.TrimPrefix(url.UserPassword("u", secret).String(), "u:")
	forms := map[string]string{
		"raw":      secret,
		"query":    url.QueryEscape(secret),
		"path":     url.PathEscape(secret),
		"userinfo": userinfo,
	}
	for name, f := range forms {
		if name != "userinfo" && f == userinfo {
			t.Fatalf("userinfo form equals the %s form: the test proves nothing", name)
		}
	}

	old := strings.NewReplacer(forms["raw"], mask, forms["query"], mask, forms["path"], mask)
	if got := old.Replace("log " + userinfo); !strings.Contains(got, userinfo) {
		t.Fatalf("positive control: the old form set should leak the userinfo form, got %q", got)
	}

	r := NewRedactor()
	r.Register(secret)
	for name, f := range forms {
		if got := r.String("x " + f + " y"); got != "x *** y" {
			t.Errorf("%s form: got %q", name, got)
		}
	}
}

func TestErrAttributeIsTruncatedAfterRedaction(t *testing.T) {
	r := NewRedactor()
	r.Register("topsecret")
	sink := NewMemorySink()
	l, _ := New(Options{Sink: sink, Redactor: r, Getenv: func(string) string { return "" }})
	log := l.For("x")

	// a secret straddling the cut must be masked before cutting, never half-exposed
	long := strings.Repeat("a", 1020) + "topsecret" + strings.Repeat("b", 500)
	log.Error("boom", Err(errors.New(long)))
	// multi-byte rune on the boundary: byte 1024 falls inside a 3-byte rune
	multi := strings.Repeat("a", 1023) + "\u20ac\u20ac"
	log.Error("boom", "err", multi)
	short := strings.Repeat("c", 1024)
	log.Error("boom", "err", short)
	log.Error("boom", "note", strings.Repeat("n", 3000)) // negative control: only err is bounded

	lines := sink.Lines()
	if strings.Contains(lines[0], "topsecret") || strings.Contains(lines[0], "secret") {
		t.Errorf("secret leaked: %.200s", lines[0])
	}
	if !strings.Contains(lines[0], "...(truncated ") || !strings.Contains(lines[0], " bytes)") {
		t.Errorf("no truncation marker: %.100s", lines[0])
	}
	if !strings.Contains(lines[0], "(truncated 499 bytes)") {
		t.Errorf("unexpected truncated count: %s", lines[0][len(lines[0])-60:])
	}
	if !utf8.ValidString(lines[1]) || strings.Contains(lines[1], "\ufffd") || !strings.Contains(lines[1], "(truncated 6 bytes)") {
		t.Errorf("rune-unsafe or wrong count: %s", lines[1][len(lines[1])-40:])
	}
	if strings.Contains(lines[2], "truncated") {
		t.Errorf("1024-byte err must not be truncated")
	}
	if strings.Contains(lines[3], "truncated") {
		t.Errorf("non-err attribute must not be truncated")
	}
}
