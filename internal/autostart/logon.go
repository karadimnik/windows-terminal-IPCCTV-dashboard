package autostart

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode/utf16"
)

// logonTask is the Windows Scheduled Task, created with schtasks.exe from a task XML this code
// generates (no PowerShell, no COM).
type logonTask struct{ env Env }

func (t logonTask) schtasks() string { return t.env.systemRoot() + `\System32\schtasks.exe` }

func (e Env) systemRoot() string {
	if e.SystemRoot != "" {
		return strings.TrimRight(e.SystemRoot, `\`)
	}
	return `C:\Windows`
}

// conhost runs holocron with no console window: a console program started by the Task Scheduler
// in the user's session otherwise opens one (the task's Hidden setting only hides the task in
// the Task Scheduler window).
func (e Env) conhost() string { return e.systemRoot() + `\System32\conhost.exe` }

// checkWinPath refuses what a task or service command line cannot carry safely: a relative path,
// a double quote (not valid in a Windows path, and it would end the quoting), a percent sign (the
// Task Scheduler expands %VARIABLES% and has no escape for it), control characters.
func checkWinPath(what, p string) error {
	switch {
	case !isWinAbs(p):
		return usage("%s %q is not an absolute Windows path", what, p)
	case strings.ContainsAny(p, `"%`):
		return usage("%s %q contains %s, which a Scheduled Task or service command line cannot carry; move holocron and its configuration to a path without it", what, p, `" or %`)
	case strings.IndexFunc(p, func(r rune) bool { return r < 0x20 }) >= 0:
		return usage("%s %q contains a control character", what, p)
	}
	return nil
}

func (t logonTask) entry(s Spec) (Entry, error) {
	for _, c := range [][2]string{{"the executable", s.Exe}, {"the configuration", s.Config}, {"the root", s.Root}} {
		if c[1] == "" && c[0] == "the root" {
			continue
		}
		if err := checkWinPath(c[0], c[1]); err != nil {
			return Entry{}, err
		}
	}
	if t.env.User == "" {
		return Entry{}, errors.New("the current Windows user is unknown")
	}
	if err := CheckWindow(s.Window, Logon); err != nil {
		return Entry{}, err
	}
	e := Entry{Mode: Logon, Name: s.Name, Exe: s.Exe, WorkDir: s.WorkDir(), Enabled: !s.Disabled, Window: WindowVisible,
		Argv: append([]string{s.Exe}, s.holocronArgs("up")...)}
	if s.Window == WindowHidden {
		e.Window = WindowHidden
		e.Argv = append([]string{t.env.conhost(), "--headless", s.Exe}, s.holocronArgs("up", "--detach")...)
	}
	principal := t.env.UID
	if principal == "" {
		principal = t.env.User
	}
	// a disabled (test) task gets a disabled logon trigger too: switching the task on with
	// `autostart enable` then still leaves nothing that can fire
	e.Content = taskXML(t.env.User, principal, e.Argv[0], winJoin(e.Argv[1:]), e.WorkDir, e.Enabled, e.Enabled)
	got, err := parseTask(e.Content)
	if err != nil {
		return Entry{}, fmt.Errorf("the generated task XML does not parse: %w", err)
	}
	e.Key = got.Key
	if d := diff(e, *got); len(d) > 0 {
		return Entry{}, fmt.Errorf("the generated task XML does not say what was asked: %s", strings.Join(d, "; "))
	}
	return e, nil
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// xmlText escapes element text (control characters are refused before this).
func xmlText(s string) string { return xmlEscaper.Replace(s) }

// taskXML is the task definition: at logon of this user, as this user with a limited token, no
// time limit, not stopped on battery, a second start ignored (the instance lock protects as well),
// started late when the logon was missed, normal priority (the default 7 is below normal and
// ffmpeg would inherit it).
func taskXML(user, principal, command, args, workDir string, enabled, triggerEnabled bool) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Author>holocron install</Author>
    <Description>Holocron NVR: starts the recorder, relay, live view and web app at logon. Created by holocron install; holocron uninstall removes it, holocron autostart disable switches it off.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>` + fmt.Sprint(triggerEnabled) + `</Enabled>
      <UserId>` + xmlText(user) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlText(principal) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>` + fmt.Sprint(enabled) + `</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlText(command) + `</Command>
      <Arguments>` + xmlText(args) + `</Arguments>
      <WorkingDirectory>` + xmlText(workDir) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

type taskDoc struct {
	Triggers struct {
		Logon []struct {
			Enabled *string `xml:"Enabled"`
			UserID  string  `xml:"UserId"`
		} `xml:"LogonTrigger"`
		Any []struct{ XMLName xml.Name } `xml:",any"`
	} `xml:"Triggers"`
	Principal struct {
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	} `xml:"Principals>Principal"`
	Settings struct {
		Enabled    *string `xml:"Enabled"`
		Multiple   *string `xml:"MultipleInstancesPolicy"`
		Disallow   *string `xml:"DisallowStartIfOnBatteries"`
		StopOnBatt *string `xml:"StopIfGoingOnBatteries"`
		Available  *string `xml:"StartWhenAvailable"`
		Limit      *string `xml:"ExecutionTimeLimit"`
		Priority   *string `xml:"Priority"`
	} `xml:"Settings"`
	Exec []struct {
		Command, Arguments, WorkingDirectory string
	} `xml:"Actions>Exec"`
	Other []struct{ XMLName xml.Name } `xml:"Actions>ComHandler"`
}

func or(p *string, def string) string {
	if p == nil {
		return def
	}
	return strings.TrimSpace(*p)
}

// decodeTaskXML accepts what schtasks /Query /XML prints: UTF-16 (with a byte-order mark) or, when
// its output is a pipe, text in the console code page that still declares encoding="UTF-16".
func decodeTaskXML(b []byte) string {
	if len(b) >= 2 && (b[0] == 0xFF && b[1] == 0xFE || b[0] == 0xFE && b[1] == 0xFF) {
		le := b[0] == 0xFF
		b = b[2:]
		u := make([]uint16, len(b)/2)
		for i := range u {
			if le {
				u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
			} else {
				u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
			}
		}
		return string(utf16.Decode(u))
	}
	return strings.TrimPrefix(string(b), "\ufeff")
}

// encodeUTF16 is how the XML file for schtasks /Create /XML is written (little-endian with a BOM,
// matching its encoding="UTF-16" declaration).
func encodeUTF16(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// parseTask reads a task definition back into an Entry (Content is the XML as given).
func parseTask(text string) (*Entry, error) {
	var d taskDoc
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil } // already decoded
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	e := &Entry{Mode: Logon, Content: text, Enabled: or(d.Settings.Enabled, "true") == "true"}
	if len(d.Exec) > 0 {
		x := d.Exec[0]
		e.Argv = append([]string{strings.TrimSpace(x.Command)}, splitWin(x.Arguments)...)
		e.WorkDir = strings.TrimSpace(x.WorkingDirectory)
		e.Exe, e.Window = e.Argv[0], WindowVisible
		if len(e.Argv) > 2 && e.Argv[1] == "--headless" {
			e.Exe, e.Window = e.Argv[2], WindowHidden
		}
	}
	user, trig := "", ""
	if len(d.Triggers.Logon) > 0 {
		user, trig = strings.ToLower(d.Triggers.Logon[0].UserID), or(d.Triggers.Logon[0].Enabled, "true")
	}
	e.Key = fmt.Sprintf("other-triggers=%d logon-trigger=%d(%s enabled=%s) actions=%d logon-type=%s multiple=%s on-battery=%s/%s start-when-available=%s time-limit=%s priority=%s",
		len(d.Triggers.Any), len(d.Triggers.Logon), user, trig, len(d.Exec)+len(d.Other), d.Principal.LogonType,
		or(d.Settings.Multiple, "IgnoreNew"), or(d.Settings.Disallow, "true"), or(d.Settings.StopOnBatt, "true"),
		or(d.Settings.Available, "false"), or(d.Settings.Limit, "PT72H"), or(d.Settings.Priority, "7"))
	return e, nil
}

// queryTask reads a task (read-only). nil when there is none (schtasks cannot read it).
func (t logonTask) queryTask(name string) (*Entry, string, error) {
	out, err := t.env.Exec(t.schtasks(), "/Query", "/TN", name, "/XML")
	if err != nil {
		return nil, trimOut(out), nil
	}
	e, perr := parseTask(decodeTaskXML(out))
	if perr != nil {
		return nil, "", fmt.Errorf("the task XML of %s does not parse: %w", name, perr)
	}
	e.Name = name
	return e, "", nil
}

func (t logonTask) current(name string) (*Entry, State, error) {
	e, _, err := t.queryTask(name)
	return e, State{}, err
}

func (t logonTask) apply(o *Ops, want Entry, prev *Entry) error {
	args := []string{"/Create", "/TN", want.Name, "/XML"}
	if o.Dry {
		fmt.Fprintf(o.Out, "would write the task XML (UTF-16) to a temporary file:\n%s\n", indent(want.Content))
		args = append(args, "<temporary file>")
	} else {
		o.logger().Debug("task XML for "+want.Name, slog.String("content", want.Content))
		path, remove, err := o.TempFile(encodeUTF16(want.Content))
		if err != nil {
			return fmt.Errorf("cannot write the task XML file: %w", err)
		}
		defer remove()
		args = append(args, path)
	}
	if prev != nil {
		args = append(args, "/F")
	}
	if _, err := o.do(t.schtasks(), args...); err != nil {
		return err
	}
	o.info(fmt.Sprintf("schtasks registered task %s (enabled %v)", want.Name, want.Enabled))
	return nil
}

func (t logonTask) remove(o *Ops, cur Entry) error {
	if _, err := o.do(t.schtasks(), "/Delete", "/TN", cur.Name, "/F"); err != nil {
		return err
	}
	o.info("schtasks deleted task " + cur.Name)
	return nil
}

func (t logonTask) enable(o *Ops, cur Entry, on bool) error {
	flag := map[bool]string{true: "/ENABLE", false: "/DISABLE"}[on]
	if _, err := o.do(t.schtasks(), "/Change", "/TN", cur.Name, flag); err != nil {
		return err
	}
	o.info(fmt.Sprintf("schtasks %s task %s", strings.ToLower(flag[1:])+"d", cur.Name))
	return nil
}

// OldTask is one of the PowerShell system's tasks as found (read-only).
type OldTask struct {
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Enabled bool   `json:"enabled"`
	Detail  string `json:"detail,omitempty"`
}

// QueryOldTasks reads the PowerShell system's tasks with schtasks /Query only; it never changes them.
func QueryOldTasks(env Env) []OldTask {
	t := logonTask{env}
	var out []OldTask
	for _, n := range OldTasks {
		e, why, err := t.queryTask(n)
		o := OldTask{Name: n}
		switch {
		case err != nil:
			o.Found, o.Detail = true, err.Error()
		case e == nil:
			o.Detail = why
		default:
			o.Found, o.Enabled = true, e.Enabled
		}
		out = append(out, o)
	}
	return out
}
