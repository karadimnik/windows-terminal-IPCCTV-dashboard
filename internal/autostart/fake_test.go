package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeOS stands in for schtasks.exe, systemctl --user, launchctl and loginctl, with state, so an
// install can be read back. fail maps "tool:verb" (schtasks:/Create, systemctl:enable, ...) to the
// error that call returns; mangle changes what /Create stores (a verification failure).
type fakeOS struct {
	mu       sync.Mutex
	calls    []string
	tasks    map[string]string // name -> XML as /Query prints it
	enabled  map[string]bool   // systemd units enabled
	disabled map[string]bool   // launchd labels disabled
	loaded   map[string]bool   // launchd labels loaded
	fail     map[string]error
	mangle   func(string) string
}

func newFakeOS() *fakeOS {
	return &fakeOS{tasks: map[string]string{}, enabled: map[string]bool{}, disabled: map[string]bool{}, loaded: map[string]bool{}, fail: map[string]error{}}
}

var errExit1 = errors.New("exit status 1")

func (f *fakeOS) Exec(name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tool := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(name, `\`, "/"))), ".exe")
	f.calls = append(f.calls, tool+" "+strings.Join(args, " "))
	verb := ""
	for _, a := range args {
		if a != "--user" {
			verb = a
			break
		}
	}
	if err := f.fail[tool+":"+verb]; err != nil {
		return []byte("fake failure"), err
	}
	switch tool {
	case "schtasks":
		return f.schtasks(args)
	case "systemctl":
		unit := args[len(args)-1]
		switch verb {
		case "daemon-reload":
			return nil, nil
		case "enable":
			f.enabled[unit] = true
			return []byte("Created symlink ..."), nil
		case "disable":
			delete(f.enabled, unit)
			return []byte("Removed ..."), nil
		case "is-enabled":
			if f.enabled[unit] {
				return []byte("enabled\n"), nil
			}
			return []byte("disabled\n"), errExit1
		case "is-active":
			return []byte("inactive\n"), errExit1
		}
	case "launchctl":
		target := args[len(args)-1]
		label := target[strings.LastIndex(target, "/")+1:]
		switch verb {
		case "enable":
			delete(f.disabled, label)
			return nil, nil
		case "disable":
			f.disabled[label] = true
			return nil, nil
		case "print-disabled":
			var b strings.Builder
			b.WriteString("disabled services = {\n")
			for l := range f.disabled {
				fmt.Fprintf(&b, "\t%q => disabled\n", l)
			}
			b.WriteString("\t\"com.apple.other\" => enabled\n}\n")
			return []byte(b.String()), nil
		case "print":
			if f.loaded[label] {
				return []byte("state = running"), nil
			}
			return []byte("Could not find service"), errExit1
		case "bootout":
			delete(f.loaded, label)
			return nil, nil
		}
	case "loginctl":
		return []byte("no\n"), nil
	}
	return nil, fmt.Errorf("fakeOS: unexpected %s %v", tool, args)
}

var enabledRE = regexp.MustCompile(`(?s)(<Settings>.*?<Enabled>)(true|false)(</Enabled>)`)

func (f *fakeOS) schtasks(args []string) ([]byte, error) {
	name := ""
	for i, a := range args {
		if a == "/TN" && i+1 < len(args) {
			name = args[i+1]
		}
	}
	switch args[0] {
	case "/Query":
		x, ok := f.tasks[name]
		if !ok {
			return []byte("ERROR: The system cannot find the file specified.\r\n"), errExit1
		}
		return []byte(x), nil
	case "/Create":
		if _, ok := f.tasks[name]; ok && args[len(args)-1] != "/F" {
			return []byte("ERROR: Cannot create a file when that file already exists."), errExit1
		}
		file := args[4]
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
			return []byte("ERROR: The task XML is malformed (not UTF-16)"), errExit1
		}
		x := strings.ReplaceAll(decodeTaskXML(b), "\n", "\r\n") // what schtasks prints back: CRLF, still declaring UTF-16
		if f.mangle != nil {
			x = f.mangle(x)
		}
		f.tasks[name] = x
		return []byte("SUCCESS: The scheduled task \"" + name + "\" has successfully been created."), nil
	case "/Change":
		x, ok := f.tasks[name]
		if !ok {
			return []byte("ERROR: The system cannot find the file specified."), errExit1
		}
		on := args[len(args)-1] == "/ENABLE"
		f.tasks[name] = enabledRE.ReplaceAllString(x, "${1}"+fmt.Sprint(on)+"${3}")
		return []byte("SUCCESS"), nil
	case "/Delete":
		if _, ok := f.tasks[name]; !ok {
			return []byte("ERROR: The system cannot find the file specified."), errExit1
		}
		delete(f.tasks, name)
		return []byte("SUCCESS"), nil
	}
	return nil, fmt.Errorf("fake schtasks: %v", args)
}

func (f *fakeOS) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeOS) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// mutating are the calls that change the system.
var mutatingRE = regexp.MustCompile(`^(schtasks /(Create|Change|Delete)|systemctl --user (daemon-reload|enable|disable|start|stop|restart)|launchctl (bootstrap|bootout|enable|disable|load|unload)|loginctl enable-linger)`)

func mutating(calls []string) []string {
	var out []string
	for _, c := range calls {
		if mutatingRE.MatchString(c) {
			out = append(out, c)
		}
	}
	return out
}

type rigT struct {
	t    *testing.T
	fos  *fakeOS
	env  Env
	dir  string
	out  strings.Builder
	dry  bool
	logs *strings.Builder
}

func newRigT(t *testing.T, goos string) *rigT {
	t.Helper()
	dir := t.TempDir()
	r := &rigT{t: t, fos: newFakeOS(), dir: dir}
	present := filepath.Join(dir, "present")
	os.WriteFile(present, nil, 0o644)
	r.env = Env{GOOS: goos, Exec: r.fos.Exec, Home: filepath.Join(dir, "home"), User: `PC\alice`, UID: "S-1-5-21-1000-1000-1000-1001", SystemRoot: `C:\Windows`,
		TempDirs: []string{`C:\Users\alice\AppData\Local\Temp`, `C:\Windows\Temp`, "/tmp"},
		Stat: func(p string) (os.FileInfo, error) {
			if strings.Contains(p, "missing") {
				return nil, os.ErrNotExist
			}
			return os.Stat(present)
		},
		TempFile: func(content []byte) (string, func(), error) {
			f, err := os.CreateTemp(dir, "task-*.xml")
			if err != nil {
				return "", nil, err
			}
			f.Write(content)
			f.Close()
			return f.Name(), func() { os.Remove(f.Name()) }, nil
		}}
	if goos != "windows" {
		r.env.User, r.env.UID = "alice", "501"
	}
	return r
}

func (r *rigT) ops() *Ops {
	r.out.Reset()
	return &Ops{Env: r.env, Out: &r.out, Dry: r.dry}
}

func winSpec(r *rigT) Spec {
	return Spec{Mode: Logon, Name: TaskName, Exe: `C:\Holocron\holocron.exe`, Config: `C:\Holocron\holocron.config.json`, Record: filepath.Join(r.dir, "cfg", "_state", "install.json")}
}

func unixSpec(r *rigT, m Mode) Spec {
	return Spec{Mode: m, Name: DefaultName(m), Exe: "/opt/holocron/holocron", Config: "/home/alice/.config/holocron/holocron.config.json", Record: filepath.Join(r.dir, "cfg", "_state", "install.json")}
}

func sorted(m map[string]bool) []string {
	var s []string
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}
