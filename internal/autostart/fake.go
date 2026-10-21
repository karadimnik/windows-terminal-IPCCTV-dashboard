package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// FakeTools stands in for schtasks.exe, systemctl --user, launchctl and loginctl in tests, with
// state, so an install can be read back. Fail maps "tool:verb" (schtasks:/Create,
// systemctl:enable, ...) to the error that call returns; Mangle changes what /Create stores (a
// verification failure).
type FakeTools struct {
	mu       sync.Mutex
	calls    []string
	Tasks    map[string]string // name -> XML as /Query prints it
	Enabled  map[string]bool   // systemd units enabled
	Disabled map[string]bool   // launchd labels disabled
	Loaded   map[string]bool   // launchd labels loaded
	Fail     map[string]error
	Mangle   func(string) string
}

func NewFakeTools() *FakeTools {
	return &FakeTools{Tasks: map[string]string{}, Enabled: map[string]bool{}, Disabled: map[string]bool{}, Loaded: map[string]bool{}, Fail: map[string]error{}}
}

// ErrFakeExit is the error of a failing fake tool.
var ErrFakeExit = errors.New("exit status 1")

func (f *FakeTools) Exec(name string, args ...string) ([]byte, error) {
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
	if err := f.Fail[tool+":"+verb]; err != nil {
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
			f.Enabled[unit] = true
			return []byte("Created symlink ..."), nil
		case "disable":
			delete(f.Enabled, unit)
			return []byte("Removed ..."), nil
		case "is-enabled":
			if f.Enabled[unit] {
				return []byte("enabled\n"), nil
			}
			return []byte("disabled\n"), ErrFakeExit
		case "is-active":
			return []byte("inactive\n"), ErrFakeExit
		}
	case "launchctl":
		target := args[len(args)-1]
		label := target[strings.LastIndex(target, "/")+1:]
		switch verb {
		case "enable":
			delete(f.Disabled, label)
			return nil, nil
		case "disable":
			f.Disabled[label] = true
			return nil, nil
		case "print-disabled":
			var b strings.Builder
			b.WriteString("disabled services = {\n")
			for l := range f.Disabled {
				fmt.Fprintf(&b, "\t%q => disabled\n", l)
			}
			b.WriteString("\t\"com.apple.other\" => enabled\n}\n")
			return []byte(b.String()), nil
		case "print":
			if f.Loaded[label] {
				return []byte("state = running"), nil
			}
			return []byte("Could not find service"), ErrFakeExit
		case "bootout":
			delete(f.Loaded, label)
			return nil, nil
		}
	case "loginctl":
		return []byte("no\n"), nil
	}
	return nil, fmt.Errorf("fakeOS: unexpected %s %v", tool, args)
}

var enabledRE = regexp.MustCompile(`(?s)(<Settings>.*?<Enabled>)(true|false)(</Enabled>)`)

func (f *FakeTools) schtasks(args []string) ([]byte, error) {
	name := ""
	for i, a := range args {
		if a == "/TN" && i+1 < len(args) {
			name = args[i+1]
		}
	}
	switch args[0] {
	case "/Query":
		x, ok := f.Tasks[name]
		if !ok {
			return []byte("ERROR: The system cannot find the file specified.\r\n"), ErrFakeExit
		}
		return []byte(x), nil
	case "/Create":
		if _, ok := f.Tasks[name]; ok && args[len(args)-1] != "/F" {
			return []byte("ERROR: Cannot create a file when that file already exists."), ErrFakeExit
		}
		file := args[4]
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
			return []byte("ERROR: The task XML is malformed (not UTF-16)"), ErrFakeExit
		}
		x := strings.ReplaceAll(decodeTaskXML(b), "\n", "\r\n") // what schtasks prints back: CRLF, still declaring UTF-16
		if f.Mangle != nil {
			x = f.Mangle(x)
		}
		f.Tasks[name] = x
		return []byte("SUCCESS: The scheduled task \"" + name + "\" has successfully been created."), nil
	case "/Change":
		x, ok := f.Tasks[name]
		if !ok {
			return []byte("ERROR: The system cannot find the file specified."), ErrFakeExit
		}
		on := args[len(args)-1] == "/ENABLE"
		f.Tasks[name] = enabledRE.ReplaceAllString(x, "${1}"+fmt.Sprint(on)+"${3}")
		return []byte("SUCCESS"), nil
	case "/Delete":
		if _, ok := f.Tasks[name]; !ok {
			return []byte("ERROR: The system cannot find the file specified."), ErrFakeExit
		}
		delete(f.Tasks, name)
		return []byte("SUCCESS"), nil
	}
	return nil, fmt.Errorf("fake schtasks: %v", args)
}

// Calls lists every call as "tool args...".
func (f *FakeTools) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Reset forgets the calls.
func (f *FakeTools) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

var mutatingRE = regexp.MustCompile(`^(schtasks /(Create|Change|Delete)|systemctl --user (daemon-reload|enable|disable|start|stop|restart)|launchctl (bootstrap|bootout|enable|disable|load|unload)|loginctl enable-linger)`)

// Mutating keeps the calls that change the system (what a dry run must never make).
func Mutating(calls []string) []string {
	var out []string
	for _, c := range calls {
		if mutatingRE.MatchString(c) {
			out = append(out, c)
		}
	}
	return out
}
