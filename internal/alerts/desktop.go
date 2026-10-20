package alerts

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"holocron/internal/logx"
)

// Desktop is the "desktop" channel: a notification on the machine the recorder runs on.
//
//   - Windows: a WinRT toast under the Windows PowerShell AUMID, exactly as the PowerShell product's
//     Show-CamToast does it (unelevated, no focus steal, kept in the Action Center). That needs
//     powershell.exe, so THIS ADAPTER IS THE ONLY PLACE HOLOCRON USES POWERSHELL: Windows PowerShell,
//     only when present, hidden, never elevated, hard 10 s timeout, payload passed as base64 so no
//     alert text is ever parsed as script.
//   - Linux: notify-send when on PATH.
//   - macOS: osascript when present (compiled and unit-tested only; not run on a Mac).
//
// Otherwise Enabled() is false.
type Desktop struct {
	log  *slog.Logger
	goos string
	exe  string // resolved tool; empty = unsupported
	run  func(ctx context.Context, name string, args []string) error
}

// deps are replaced by tests.
type desktopDeps struct {
	goos     string
	lookPath func(string) (string, error)
	getenv   func(string) string
	run      func(ctx context.Context, name string, args []string) error
}

// NewDesktop resolves the notification tool once.
func NewDesktop(cfg DesktopConfig, log *slog.Logger) *Desktop {
	return newDesktop(cfg, log, desktopDeps{goos: runtime.GOOS, lookPath: exec.LookPath, getenv: os.Getenv, run: runHidden})
}

func newDesktop(cfg DesktopConfig, log *slog.Logger, dd desktopDeps) *Desktop {
	d := &Desktop{log: log, goos: dd.goos, run: dd.run}
	if !enabledOr(cfg.Enabled, true) {
		log.Debug("desktop alerts disabled by configuration", logx.Action("alert.channel"), logx.Target("desktop"), logx.Result("disabled"))
		return d
	}
	var tool string
	switch dd.goos {
	case "windows":
		tool = "powershell.exe"
	case "linux":
		tool = "notify-send"
	case "darwin":
		tool = "osascript"
	}
	if tool != "" {
		if p, err := dd.lookPath(tool); err == nil {
			d.exe = p
		} else if dd.goos == "windows" {
			// powershell.exe is not always on a service's PATH
			p := filepath.Join(dd.getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
			if dd.getenv("SystemRoot") != "" {
				if _, err := os.Stat(p); err == nil {
					d.exe = p
				}
			}
		}
	}
	if d.exe == "" {
		log.Debug("desktop alerts unavailable: no notification tool", logx.Action("alert.channel"), logx.Target("desktop"),
			logx.Result("disabled"), slog.String("tool", tool), slog.String("os", dd.goos))
	}
	return d
}

func (*Desktop) Name() string    { return "desktop" }
func (d *Desktop) Enabled() bool { return d.exe != "" }

func (d *Desktop) Send(ctx context.Context, ev Event) error {
	ctx, cancel := context.WithTimeout(ctx, DesktopTimeout)
	defer cancel()
	args := desktopArgs(d.goos, ev)
	if args == nil {
		return fmt.Errorf("desktop notifications are not supported on %s", d.goos)
	}
	return d.run(ctx, d.exe, args)
}

const (
	toastTitleRunes = 120
	toastBodyRunes  = 600
)

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-3]) + "..."
}

// desktopArgs is the argument vector (without the executable) for the OS, or nil if unsupported.
func desktopArgs(goos string, ev Event) []string {
	title, body := clip(ev.Title, toastTitleRunes), clip(ev.Message, toastBodyRunes)
	switch goos {
	case "windows":
		return []string{"-NoProfile", "-NonInteractive", "-NoLogo", "-WindowStyle", "Hidden",
			"-EncodedCommand", encodePS(toastScript(title, body))}
	case "linux":
		urgency, icon := "normal", "dialog-warning"
		switch ev.Severity {
		case Error:
			urgency, icon = "critical", "dialog-error"
		case Info:
			urgency, icon = "low", "dialog-information"
		}
		return []string{"--app-name=Holocron", "--urgency=" + urgency, "--icon=" + icon, "--", title, body}
	case "darwin":
		// title and body travel as arguments to run handler, never inside the script text
		return []string{"-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run", noLeadingDash(title), noLeadingDash(body)}
	}
	return nil
}

func noLeadingDash(s string) string {
	if strings.HasPrefix(s, "-") {
		return " " + s
	}
	return s
}

// toastAUMID is Windows PowerShell's own, as in Show-CamToast: a toast needs an AUMID with a Start
// menu entry, and inventing one would need a shortcut installed first.
const toastAUMID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// toastScript is Show-CamToast with the text delivered as base64 UTF-8 (no quoting problems, no injection).
func toastScript(title, body string) string {
	return `$ErrorActionPreference = 'Stop'
[void][Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
[void][Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom, ContentType = WindowsRuntime]
$t = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + b64(title) + `'))
$m = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('` + b64(body) + `'))
$xml = '<toast><visual><binding template="ToastText02"><text id="1">{0}</text><text id="2">{1}</text></binding></visual></toast>' -f [System.Security.SecurityElement]::Escape($t), [System.Security.SecurityElement]::Escape($m)
$document = New-Object Windows.Data.Xml.Dom.XmlDocument
$document.LoadXml($xml)
$toast = New-Object Windows.UI.Notifications.ToastNotification $document
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('` + toastAUMID + `').Show($toast)
`
}

// encodePS is the -EncodedCommand form: base64 of the UTF-16LE script.
func encodePS(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, 2*len(u))
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// runHidden runs the tool with no window and the context's deadline, keeping a clipped copy of its output for the error.
func runHidden(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	w := &limitWriter{b: &out, n: 400}
	cmd.Stdout, cmd.Stderr = w, w // one writer: exec then copies both streams on one goroutine
	hideWindow(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", filepath.Base(name), ctx.Err())
		}
		return fmt.Errorf("%s: %v: %s", filepath.Base(name), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// limitWriter keeps at most n bytes in b, then discards (never fails, so the child is never blocked).
type limitWriter struct {
	b *bytes.Buffer
	n int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if room := w.n - w.b.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.b.Write(p[:room])
	}
	return len(p), nil
}
