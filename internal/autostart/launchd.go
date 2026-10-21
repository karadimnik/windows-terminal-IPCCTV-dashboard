package autostart

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// launchdAgent is a per-user launchd agent in ~/Library/LaunchAgents. UNVERIFIED: no macOS machine
// was available; the plist and the command sequences are unit-tested only.
type launchdAgent struct{ env Env }

func (a launchdAgent) file(label string) string {
	return filepath.Join(a.env.Home, "Library", "LaunchAgents", label+".plist")
}

func (a launchdAgent) domain() string { return "gui/" + a.env.UID }

func (a launchdAgent) target(label string) string { return a.domain() + "/" + label }

func (a launchdAgent) entry(s Spec) (Entry, error) {
	if a.env.Home == "" || a.env.UID == "" {
		return Entry{}, errors.New("the home folder or the user id is unknown")
	}
	e, err := unixEntry(Launchd, s)
	if err != nil {
		return e, err
	}
	e.File = a.file(s.Name)
	e.Content = plistText(s.Name, e.Argv, e.WorkDir, filepath.Join(a.env.Home, "Library", "Logs", s.Name+".log"))
	return e, nil
}

// plistText is the agent: started at login (RunAtLoad), started again by launchd only when it
// exits with a failure (KeepAlive SuccessfulExit=false; the host restarts the supervisor itself),
// at most every 10 s.
func plistText(label string, argv []string, workDir, logFile string) string {
	var args strings.Builder
	for _, a := range argv {
		args.WriteString("\t\t<string>" + xmlText(a) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Created by holocron install; holocron uninstall removes it and the next install rewrites it. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlText(label) + `</string>
	<key>ProgramArguments</key>
	<array>
` + args.String() + `	</array>
	<key>WorkingDirectory</key>
	<string>` + xmlText(workDir) + `</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>` + xmlText(logFile) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlText(logFile) + `</string>
</dict>
</plist>
`
}

// parsePlist reads ProgramArguments and WorkingDirectory back (only the shape plistText writes).
func parsePlist(text string) (argv []string, workDir string, err error) {
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.Strict = false
	var key string
	inArgs := false
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return argv, workDir, nil
			}
			return nil, "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, "", err
				}
				key = s
			case "array":
				inArgs = key == "ProgramArguments"
			case "string":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, "", err
				}
				if inArgs {
					argv = append(argv, s)
				} else if key == "WorkingDirectory" {
					workDir = s
				}
			}
		case xml.EndElement:
			if t.Name.Local == "array" {
				inArgs = false
			}
		}
	}
}

func (a launchdAgent) current(name string) (*Entry, State, error) {
	path := a.file(name)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, State{}, nil
	}
	if err != nil {
		return nil, State{}, err
	}
	e := &Entry{Mode: Launchd, Name: name, File: path, Content: string(b)}
	if e.Argv, e.WorkDir, err = parsePlist(e.Content); err != nil {
		return nil, State{}, fmt.Errorf("%s does not parse: %w", path, err)
	}
	if len(e.Argv) > 0 {
		e.Exe = e.Argv[0]
	}
	var st State
	e.Enabled = true
	out, err := a.env.Exec("launchctl", "print-disabled", a.domain())
	if err != nil {
		st.Detail = "launchctl print-disabled: " + firstLine(trimOut(out))
	}
	for _, l := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=>")
		if ok && strings.Trim(strings.TrimSpace(k), `"`) == name {
			v = strings.TrimSpace(v)
			e.Enabled = !(v == "true" || v == "disabled")
		}
	}
	if _, err := a.env.Exec("launchctl", "print", a.target(name)); err == nil {
		st.Running = "loaded"
	} else {
		st.Running = "not loaded"
	}
	return e, st, nil
}

func (a launchdAgent) apply(o *Ops, want Entry, prev *Entry) error {
	if err := o.writeFile(want.File, want.Content); err != nil {
		return fmt.Errorf("cannot write %s: %w", want.File, err)
	}
	if prev == nil || prev.Enabled != want.Enabled {
		if err := a.enable(o, want, want.Enabled); err != nil {
			return err
		}
	}
	o.info(fmt.Sprintf("wrote %s (enabled %v); launchd loads it at the next login", want.File, want.Enabled))
	return nil
}

func (a launchdAgent) remove(o *Ops, cur Entry) error {
	if _, err := o.query("launchctl", "print", a.target(cur.Name)); err == nil {
		if _, err := o.do("launchctl", "bootout", a.target(cur.Name)); err != nil {
			return err
		}
	}
	file := cur.File
	if file == "" {
		file = a.file(cur.Name)
	}
	if err := o.removeFile(file); err != nil {
		return err
	}
	o.info("deleted " + file)
	return nil
}

func (a launchdAgent) enable(o *Ops, cur Entry, on bool) error {
	verb := map[bool]string{true: "enable", false: "disable"}[on]
	if _, err := o.do("launchctl", verb, a.target(cur.Name)); err != nil {
		return err
	}
	o.info("launchctl " + verb + " " + a.target(cur.Name))
	return nil
}
