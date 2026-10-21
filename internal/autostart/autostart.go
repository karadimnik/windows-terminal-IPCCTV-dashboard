// Package autostart creates, checks, switches and removes the OS entry that starts holocron at logon
// or boot (rule R1: `holocron install`, `uninstall`, `autostart ...`). One backend per mode: a
// Scheduled Task through schtasks.exe (logon), a Windows service through the osx.ServiceManager
// seam (service), a systemd user unit (systemd), a launchd agent (launchd). Every OS tool is run
// through the Exec seam, so the tests drive fakes; see README.md.
package autostart

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"holocron/internal/logx"
	"holocron/internal/osx"
)

// Mode is the kind of autostart entry.
type Mode string

const (
	Logon   Mode = "logon"   // Windows Scheduled Task at logon of the current user (default on Windows)
	Service Mode = "service" // Windows service (needs administrator rights)
	Systemd Mode = "systemd" // systemd user unit (default on Linux)
	Launchd Mode = "launchd" // launchd agent (default on macOS)
)

// Own names: the only entries this package ever creates or removes.
const (
	TaskName    = "Holocron"
	ServiceName = "Holocron"
	UnitName    = "holocron"
	LaunchLabel = "com.holocron.nvr"
	// TestInfix must be in any other name (tests only), so a test can never touch the real entry.
	TestInfix = "-TEST-"
)

// OldTasks are the PowerShell system's Scheduled Tasks: only ever queried, never changed.
var OldTasks = []string{"IPCCTV-Recorder", "IPCCTV-Dashboard", "IPCCTV-Ui"}

// ErrUsage marks a usage fault (unknown mode, a mode of another OS, a bad name): exit 2.
var ErrUsage = errors.New("usage")

func usage(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, a...))
}

// DefaultMode is the mode `install` uses without --mode.
func DefaultMode(goos string) (Mode, error) {
	switch goos {
	case "windows":
		return Logon, nil
	case "linux":
		return Systemd, nil
	case "darwin":
		return Launchd, nil
	}
	return "", usage("autostart is not supported on %s", goos)
}

// ParseMode checks a --mode value against the OS.
func ParseMode(s, goos string) (Mode, error) {
	if s == "" {
		return DefaultMode(goos)
	}
	m := Mode(s)
	ok := map[string][]Mode{"windows": {Logon, Service}, "linux": {Systemd}, "darwin": {Launchd}}[goos]
	for _, v := range ok {
		if v == m {
			return m, nil
		}
	}
	switch m {
	case Logon, Service, Systemd, Launchd:
		return "", usage("--mode %s is not available on %s (use %s)", s, goos, joinModes(ok))
	}
	return "", usage("unknown --mode %q (logon, service, systemd or launchd)", s)
}

func joinModes(ms []Mode) string {
	var s []string
	for _, m := range ms {
		s = append(s, string(m))
	}
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, " or ")
}

// DefaultName is the entry name of a mode.
func DefaultName(m Mode) string {
	switch m {
	case Logon:
		return TaskName
	case Service:
		return ServiceName
	case Systemd:
		return UnitName
	}
	return LaunchLabel
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// CheckName accepts the mode's own name, or a test name: same characters and the TestInfix.
func CheckName(name string, m Mode) error {
	if name == DefaultName(m) {
		return nil
	}
	if !nameRE.MatchString(name) || !strings.Contains(name, TestInfix) {
		return usage("entry name %q: only %q, or a test name with %q in it (letters, digits, . _ -)", name, DefaultName(m), TestInfix)
	}
	return nil
}

// Exec runs an OS tool and returns its combined output. A non-zero exit is an error.
type Exec func(name string, args ...string) ([]byte, error)

// Env is what the backends need from the machine; RealEnv fills it, tests fake it.
type Env struct {
	GOOS       string
	Exec       Exec
	SCM        osx.ServiceManager
	Home       string // user's home folder
	ConfigHome string // $XDG_CONFIG_HOME, default <Home>/.config
	User       string // DOMAIN\alice (Windows) or alice
	UID        string // SID on Windows, numeric id on Unix
	SystemRoot string // C:\Windows
	TempDirs   []string
	TempFile   func(content []byte) (path string, remove func(), err error) // a file schtasks can read
	Stat       func(path string) (os.FileInfo, error)
}

func (e Env) stat(p string) bool {
	st := e.Stat
	if st == nil {
		st = os.Stat
	}
	fi, err := st(p)
	return err == nil && !fi.IsDir()
}

// Spec is what to install.
type Spec struct {
	Mode     Mode
	Name     string
	Exe      string // absolute path of holocron
	Config   string // absolute path of holocron.config.json
	Root     string // the archive root when given on the command line ("" = RootPath of the configuration)
	Disabled bool   // create it switched off (tests: an entry that can never fire)
	Account  string // service mode: the service account ("" = LocalSystem)
	Record   string // the install record file; "" = RecordPath(WorkDir())
}

// RecordFile is where the install record of s goes.
func (s Spec) RecordFile() string {
	if s.Record != "" {
		return s.Record
	}
	return RecordPath(s.WorkDir())
}

// WorkDir is the configuration's folder.
func (s Spec) WorkDir() string { return dirOf(s.Config) }

// holocronArgs are the arguments after the executable: the configuration, the root when given,
// then the role.
func (s Spec) holocronArgs(role ...string) []string {
	a := []string{"--config", s.Config}
	if s.Root != "" {
		a = append(a, "--root", s.Root)
	}
	return append(a, role...)
}

// Entry is a concrete OS entry: its generated text and what it runs.
type Entry struct {
	Mode    Mode
	Name    string
	File    string   // the unit or plist file ("" for a task or a service)
	Content string   // task XML, unit text, plist text, or the service settings
	Argv    []string // the program and its arguments as the entry starts them
	Exe     string   // the holocron executable among them
	WorkDir string
	Key     string // the settings that are compared besides the command (trigger, policies, account)
	Enabled bool
	Service *osx.ServiceConfig
}

// Command is the entry's command line for messages.
func (e Entry) Command() string { return displayArgv(e.Argv) }

// State is an entry as found on the system.
type State struct {
	Mode      Mode   `json:"mode"`
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Running   string `json:"running,omitempty"` // systemd is-active / service state, when known
	Command   string `json:"command,omitempty"`
	Exe       string `json:"exe,omitempty"`
	ExeExists bool   `json:"exeExists"`
	File      string `json:"file,omitempty"`
	Recorded  bool   `json:"recorded"`
	Detail    string `json:"detail,omitempty"`
}

// Summary is one line for `status` and `doctor`.
func (s State) Summary() string {
	if !s.Installed {
		t := fmt.Sprintf("not installed (%s %s; holocron install creates it)", s.Mode, s.Name)
		if s.Detail != "" {
			t += " - " + s.Detail
		}
		return t
	}
	on := "enabled"
	if !s.Enabled {
		on = "DISABLED"
	}
	t := fmt.Sprintf("%s %s, %s", kind(s.Mode), s.Name, on)
	if s.Running != "" {
		t += ", " + s.Running
	}
	t += ", runs " + s.Command
	if !s.ExeExists {
		t += " - WARNING: " + s.Exe + " does not exist (moved or deleted binary: run holocron install again from the new place)"
	}
	if !s.Recorded {
		t += " - not recorded by holocron install"
	}
	return t
}

func kind(m Mode) string {
	switch m {
	case Logon:
		return "logon task"
	case Service:
		return "Windows service"
	case Systemd:
		return "systemd user unit"
	}
	return "launchd agent"
}

// Ops is one run: the environment, where messages and records go, and whether it is a dry run.
// In a dry run every mutating command and file write is printed and nothing is changed; read-only
// queries still run.
type Ops struct {
	Env
	Out io.Writer
	Log *slog.Logger
	Dry bool
}

func (o *Ops) logger() *slog.Logger {
	if o.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o.Log
}

// query runs a read-only command (also in a dry run).
func (o *Ops) query(name string, args ...string) ([]byte, error) {
	out, err := o.Exec(name, args...)
	o.logger().Debug("exec (read-only): "+displayArgv(append([]string{name}, args...)), logx.Action("install.exec"),
		slog.String("output", trimOut(out)), logx.Err(err))
	return out, err
}

// do runs a command that changes the system; in a dry run it only prints it.
func (o *Ops) do(name string, args ...string) ([]byte, error) {
	line := displayArgv(append([]string{name}, args...))
	if o.Dry {
		fmt.Fprintln(o.Out, "would run:", line)
		return nil, nil
	}
	out, err := o.Exec(name, args...)
	if err != nil {
		o.logger().Error("exec failed: "+line, logx.Action("install.exec"), logx.Result(logx.ResultFail), slog.String("output", trimOut(out)), logx.Err(err))
		return out, fmt.Errorf("%s: %v: %s", line, err, trimOut(out))
	}
	o.logger().Debug("exec: "+line, logx.Action("install.exec"), logx.Result(logx.ResultOK), slog.String("output", trimOut(out)))
	return out, nil
}

// writeFile writes a generated file (unit, plist); in a dry run it prints its content.
func (o *Ops) writeFile(path, content string) error {
	if o.Dry {
		fmt.Fprintf(o.Out, "would write %s:\n%s\n", path, indent(content))
		return nil
	}
	o.logger().Debug("write "+path, logx.Action("install.write"), slog.String("content", content))
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (o *Ops) removeFile(path string) error {
	if o.Dry {
		fmt.Fprintln(o.Out, "would delete", path)
		return nil
	}
	o.logger().Debug("delete "+path, logx.Action("install.write"))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// info records a state change; a dry run changed nothing, so it records none.
func (o *Ops) info(msg string) {
	if o.Dry {
		return
	}
	o.logger().Info(msg, logx.Action("install.step"), logx.Result(logx.ResultOK))
}

func trimOut(b []byte) string {
	s := strings.TrimSpace(strings.ReplaceAll(string(b), "\r", ""))
	if len(s) > 2000 {
		s = s[:2000] + "..."
	}
	return s
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + strings.TrimRight(l, "\r")
	}
	return strings.Join(lines, "\n")
}

// dirOf is filepath.Dir that also understands Windows paths on any OS (the tests generate
// Windows entries on Linux).
func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i > 0 {
		if i == 2 && p[1] == ':' {
			return p[:3]
		}
		return p[:i]
	}
	return filepath.Dir(p)
}

// TransientExe says why exe looks like a binary that will not be there tomorrow (a temp folder,
// the go run build cache), or "".
func TransientExe(exe string, env Env) string {
	low := strings.ToLower(strings.ReplaceAll(exe, `\`, "/"))
	if strings.Contains(low, "/go-build") {
		return "it is in the go build cache (go run)"
	}
	for _, t := range env.TempDirs {
		if t == "" {
			continue
		}
		tl := strings.TrimRight(strings.ToLower(strings.ReplaceAll(t, `\`, "/")), "/") + "/"
		if strings.HasPrefix(low, tl) {
			return "it is under the temporary folder " + t
		}
	}
	return ""
}

// ---- the install record (<config dir>/_state/install.json) ----

// Record is what `install` created; `uninstall` removes exactly that.
type Record struct {
	Mode        Mode      `json:"mode"`
	Name        string    `json:"name"`
	Exe         string    `json:"exe"`
	Command     string    `json:"command"`
	File        string    `json:"file,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
	Version     string    `json:"version"`
}

// RecordPath is the record of a configuration folder.
func RecordPath(configDir string) string { return filepath.Join(configDir, "_state", "install.json") }
