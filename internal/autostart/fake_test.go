package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type rigT struct {
	t    *testing.T
	fos  *FakeTools
	env  Env
	dir  string
	out  strings.Builder
	dry  bool
	logs *strings.Builder
}

func newRigT(t *testing.T, goos string) *rigT {
	t.Helper()
	dir := t.TempDir()
	r := &rigT{t: t, fos: NewFakeTools(), dir: dir}
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
