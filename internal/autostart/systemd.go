package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemdUnit is a systemd USER unit (never a system unit, never sudo).
type systemdUnit struct{ env Env }

func (u systemdUnit) dir() string {
	base := u.env.ConfigHome
	if base == "" {
		base = filepath.Join(u.env.Home, ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

func unitFile(name string) string { return name + ".service" }

func checkUnixPath(what, p string) error {
	switch {
	case !strings.HasPrefix(p, "/"):
		return usage("%s %q is not an absolute path", what, p)
	case strings.IndexFunc(p, func(r rune) bool { return r < 0x20 }) >= 0:
		return usage("%s %q contains a control character", what, p)
	}
	return nil
}

func unixEntry(mode Mode, s Spec) (Entry, error) {
	for _, c := range [][2]string{{"the executable", s.Exe}, {"the configuration", s.Config}, {"the root", s.Root}} {
		if c[1] == "" && c[0] == "the root" {
			continue
		}
		if err := checkUnixPath(c[0], c[1]); err != nil {
			return Entry{}, err
		}
	}
	// the OS manager starts the host directly in service mode: its SIGTERM is then an orderly
	// "requested" stop (attached `up` would read it as a closed window and raise alert 103)
	return Entry{Mode: mode, Name: s.Name, Exe: s.Exe, WorkDir: s.WorkDir(), Enabled: !s.Disabled,
		Argv: append([]string{s.Exe}, s.holocronArgs("host", "--service")...)}, nil
}

func (u systemdUnit) entry(s Spec) (Entry, error) {
	if u.env.Home == "" && u.env.ConfigHome == "" {
		return Entry{}, errors.New("the home folder is unknown")
	}
	e, err := unixEntry(Systemd, s)
	if err != nil {
		return e, err
	}
	e.File = filepath.Join(u.dir(), unitFile(s.Name))
	e.Content = unitText(e.Argv, e.WorkDir)
	return e, nil
}

// unitText is the unit: a simple service running the host, restarted by systemd only after a
// failure (the host restarts the supervisor itself), never after exit 20 (another supervisor holds
// the archive) or 21 (configuration fault); the whole control group goes on stop.
func unitText(argv []string, workDir string) string {
	return `# Created by holocron install; holocron uninstall removes it and the next install rewrites it.
[Unit]
Description=Holocron NVR (recorder, relay, live view, web app)

[Service]
Type=simple
WorkingDirectory=` + sdPath(workDir) + `
ExecStart=` + sdJoin(argv) + `
Restart=on-failure
RestartSec=10
RestartPreventExitStatus=20 21
KillMode=control-group

[Install]
WantedBy=default.target
`
}

func (u systemdUnit) systemctl(args ...string) []string { return append([]string{"--user"}, args...) }

func (u systemdUnit) current(name string) (*Entry, State, error) {
	path := filepath.Join(u.dir(), unitFile(name))
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, State{}, nil
	}
	if err != nil {
		return nil, State{}, err
	}
	e := &Entry{Mode: Systemd, Name: name, File: path, Content: string(b)}
	for _, l := range strings.Split(e.Content, "\n") {
		if v, ok := strings.CutPrefix(l, "ExecStart="); ok {
			e.Argv = sdSplit(v)
		}
		if v, ok := strings.CutPrefix(l, "WorkingDirectory="); ok {
			e.WorkDir = strings.ReplaceAll(v, "%%", "%")
		}
	}
	if len(e.Argv) > 0 {
		e.Exe = e.Argv[0]
	}
	var st State
	out, err := u.env.Exec("systemctl", u.systemctl("is-enabled", unitFile(name))...)
	state := strings.TrimSpace(string(out))
	e.Enabled = err == nil && state == "enabled"
	if err != nil && state != "disabled" {
		st.Detail = "systemctl --user is-enabled: " + firstLine(state)
	}
	if out, _ := u.env.Exec("systemctl", u.systemctl("is-active", unitFile(name))...); len(out) > 0 {
		st.Running = firstLine(strings.TrimSpace(string(out)))
	}
	return e, st, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.TrimSpace(s)
}

func (u systemdUnit) apply(o *Ops, want Entry, prev *Entry) error {
	if err := o.writeFile(want.File, want.Content); err != nil {
		return fmt.Errorf("cannot write %s: %w", want.File, err)
	}
	if _, err := o.do("systemctl", u.systemctl("daemon-reload")...); err != nil {
		return err
	}
	switch {
	case want.Enabled && (prev == nil || !prev.Enabled):
		if _, err := o.do("systemctl", u.systemctl("enable", unitFile(want.Name))...); err != nil {
			return err
		}
	case !want.Enabled && prev != nil && prev.Enabled:
		if _, err := o.do("systemctl", u.systemctl("disable", unitFile(want.Name))...); err != nil {
			return err
		}
	}
	o.info(fmt.Sprintf("wrote %s and reloaded systemd (enabled %v)", want.File, want.Enabled))
	return nil
}

func (u systemdUnit) remove(o *Ops, cur Entry) error {
	if cur.Enabled {
		if _, err := o.do("systemctl", u.systemctl("disable", unitFile(cur.Name))...); err != nil {
			return err
		}
	}
	file := cur.File
	if file == "" {
		file = filepath.Join(u.dir(), unitFile(cur.Name))
	}
	if err := o.removeFile(file); err != nil {
		return err
	}
	if _, err := o.do("systemctl", u.systemctl("daemon-reload")...); err != nil {
		return err
	}
	o.info("deleted " + file + " and reloaded systemd")
	return nil
}

func (u systemdUnit) enable(o *Ops, cur Entry, on bool) error {
	verb := map[bool]string{true: "enable", false: "disable"}[on]
	if _, err := o.do("systemctl", u.systemctl(verb, unitFile(cur.Name))...); err != nil {
		return err
	}
	o.info("systemctl --user " + verb + " " + unitFile(cur.Name))
	return nil
}

// Linger reports loginctl's Linger for user (read-only): "yes", "no", or "" when unknown.
func Linger(env Env, user string) string {
	out, err := env.Exec("loginctl", "show-user", user, "--property=Linger", "--value")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
