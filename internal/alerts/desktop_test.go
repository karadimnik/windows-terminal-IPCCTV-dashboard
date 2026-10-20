package alerts

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func decodePS(t *testing.T, enc string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(b)%2 != 0 {
		t.Fatalf("not base64 UTF-16LE: %v", err)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func b64decode(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWindowsToastCommandLine(t *testing.T) {
	e := testEvent()
	e.Title = `it's "bad" <b>&amp; 'quoted'; Remove-Item C:\x` // must never reach the script as text
	e.Message = "caf\u00e9 \u2013 `$(whoami)` %PATH%"
	args := desktopArgs("windows", e)
	if len(args) != 7 || strings.Join(args[:5], " ") != "-NoProfile -NonInteractive -NoLogo -WindowStyle Hidden" || args[5] != "-EncodedCommand" {
		t.Fatalf("args: %v", args[:6])
	}
	script := decodePS(t, args[6])
	for _, must := range []string{
		"[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]",
		"ToastText02", "SecurityElement]::Escape",
		`CreateToastNotifier('{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe').Show($toast)`,
	} {
		if !strings.Contains(script, must) {
			t.Errorf("script lacks %q", must)
		}
	}
	for _, leak := range []string{"Remove-Item", "whoami", "caf\u00e9", "bad"} {
		if strings.Contains(script, leak) {
			t.Errorf("alert text %q appears in the script outside base64", leak)
		}
	}
	// the payloads decode back to the exact text
	if !strings.Contains(script, "'"+b64(e.Title)+"'") || !strings.Contains(script, "'"+b64(e.Message)+"'") {
		t.Errorf("payloads missing")
	}
	if strings.Contains(strings.Join(args, " "), "Remove-Item") {
		t.Errorf("plain text on the command line")
	}
	if n := len(strings.Join(args, " ")); n > 30000 {
		t.Errorf("command line %d chars", n)
	}
	// a very long message is clipped so the command line stays under the 32767 limit
	e.Message = strings.Repeat("\u00e9", 20000)
	if n := len(strings.Join(desktopArgs("windows", e), " ")); n > 30000 {
		t.Errorf("long message gives a %d char command line", n)
	}
}

func TestLinuxNotifySendCommandLine(t *testing.T) {
	for _, tc := range []struct {
		sev           Severity
		urgency, icon string
	}{{Error, "critical", "dialog-error"}, {Warning, "normal", "dialog-warning"}, {Info, "low", "dialog-information"}} {
		e := testEvent()
		e.Severity, e.Title, e.Message = tc.sev, "--hint=evil", "-x body"
		got := desktopArgs("linux", e)
		want := []string{"--app-name=Holocron", "--urgency=" + tc.urgency, "--icon=" + tc.icon, "--", "--hint=evil", "-x body"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%v: %q", tc.sev, got)
		}
	}
}

func TestMacOSOsascriptCommandLine(t *testing.T) {
	e := testEvent()
	e.Title, e.Message = `-bad "title"`, `body with " and \ and 'quotes'`
	got := desktopArgs("darwin", e)
	script := strings.Join(got[:6], " ")
	if script != "-e on run argv -e display notification (item 2 of argv) with title (item 1 of argv) -e end run" {
		t.Errorf("script: %q", script)
	}
	if got[6] != " -bad \"title\"" || got[7] != `body with " and \ and 'quotes'` {
		t.Errorf("arguments: %q", got[6:])
	}
	if strings.Contains(script, "title\"") {
		t.Errorf("alert text inside script")
	}
}

func TestUnsupportedOSHasNoCommand(t *testing.T) {
	if desktopArgs("plan9", testEvent()) != nil {
		t.Errorf("args for an unsupported OS")
	}
}

func fakeDeps(goos string, onPath map[string]string, env map[string]string, run func(context.Context, string, []string) error) desktopDeps {
	return desktopDeps{goos: goos,
		lookPath: func(n string) (string, error) {
			if p, ok := onPath[n]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		getenv: func(k string) string { return env[k] },
		run:    run}
}

func TestDesktopEnabledOnlyWhenTheToolExists(t *testing.T) {
	lg := newTestLog()
	for _, tc := range []struct {
		goos, tool string
	}{{"windows", "powershell.exe"}, {"linux", "notify-send"}, {"darwin", "osascript"}} {
		on := newDesktop(DesktopConfig{}, lg.For("alerts"), fakeDeps(tc.goos, map[string]string{tc.tool: "/x/" + tc.tool}, nil, nil))
		off := newDesktop(DesktopConfig{}, lg.For("alerts"), fakeDeps(tc.goos, nil, nil, nil))
		if !on.Enabled() || off.Enabled() || on.Name() != "desktop" {
			t.Errorf("%s: on=%v off=%v", tc.goos, on.Enabled(), off.Enabled())
		}
		explicit := false
		offCfg := newDesktop(DesktopConfig{Enabled: &explicit}, lg.For("alerts"), fakeDeps(tc.goos, map[string]string{tc.tool: "/x"}, nil, nil))
		if offCfg.Enabled() {
			t.Errorf("%s: disabled by config but enabled", tc.goos)
		}
	}
	if newDesktop(DesktopConfig{}, lg.For("alerts"), fakeDeps("plan9", nil, nil, nil)).Enabled() {
		t.Errorf("plan9 enabled")
	}
	if len(lg.lines("DEBUG", "desktop alerts unavailable")) < 4 {
		t.Errorf("no DEBUG log for the unavailable cases: %v", lg.mem.Lines())
	}
}

func TestWindowsPowerShellFoundInSystemRootWhenNotOnPath(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "powershell.exe")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newDesktop(DesktopConfig{}, newTestLog().For("alerts"), fakeDeps("windows", nil, map[string]string{"SystemRoot": root}, nil))
	if !d.Enabled() || d.exe != exe {
		t.Errorf("exe = %q", d.exe)
	}
}

func TestDesktopSendRunsTheToolWithAHardTimeout(t *testing.T) {
	var gotName string
	var gotArgs []string
	var deadline time.Time
	run := func(ctx context.Context, name string, args []string) error {
		gotName, gotArgs = name, args
		deadline, _ = ctx.Deadline()
		return nil
	}
	d := newDesktop(DesktopConfig{}, newTestLog().For("alerts"), fakeDeps("linux", map[string]string{"notify-send": "/usr/bin/notify-send"}, nil, run))
	// a parent context with no deadline, and one with a longer deadline: the adapter caps both at 10 s
	hour, cancelHour := context.WithTimeout(context.Background(), time.Hour)
	defer cancelHour()
	for _, ctx := range []context.Context{context.Background(), hour} {
		before := time.Now()
		if err := d.Send(ctx, testEvent()); err != nil {
			t.Fatal(err)
		}
		if deadline.IsZero() || deadline.Sub(before) > DesktopTimeout+time.Second {
			t.Errorf("deadline %v after start", deadline.Sub(before))
		}
	}
	if gotName != "/usr/bin/notify-send" || len(gotArgs) == 0 || gotArgs[len(gotArgs)-2] != testEvent().Title {
		t.Errorf("%s %q", gotName, gotArgs)
	}
	d = newDesktop(DesktopConfig{}, newTestLog().For("alerts"), fakeDeps("linux", map[string]string{"notify-send": "x"}, nil,
		func(context.Context, string, []string) error { return errors.New("no session bus") }))
	if err := d.Send(context.Background(), testEvent()); err == nil {
		t.Errorf("tool failure must be an error")
	}
}

func TestRunHiddenReportsFailureAndHonoursTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	if err := runHidden(context.Background(), "/bin/sh", []string{"-c", "exit 0"}); err != nil {
		t.Errorf("success: %v", err)
	}
	err := runHidden(context.Background(), "/bin/sh", []string{"-c", "echo no display >&2; exit 3"})
	if err == nil || !strings.Contains(err.Error(), "no display") || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("failure: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = runHidden(ctx, "/bin/sh", []string{"-c", "sleep 30"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("timeout: %v after %v", err, time.Since(start))
	}
}
