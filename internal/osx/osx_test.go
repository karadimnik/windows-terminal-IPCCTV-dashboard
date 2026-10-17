package osx

import (
	"errors"
	"os"
	"reflect"
	"runtime"
	"testing"
)

func envOf(m map[string]string, home, exe string, mounts ...string) Env {
	return Env{Getenv: func(k string) string { return m[k] }, Home: home, ExeDir: exe, Mounts: mounts}
}

func TestDefaultDirs(t *testing.T) {
	tests := []struct {
		name string
		goos string
		env  Env
		want Dirs
	}{
		{"windows", "windows", envOf(map[string]string{"LOCALAPPDATA": `C:\Users\op\AppData\Local`}, `C:\Users\op`, `C:\holocron`),
			Dirs{Root: `E:\cameras`, Config: `C:\holocron`, State: `E:\cameras\_state`, Logs: `E:\cameras\_logs`,
				Auth: `C:\Users\op\AppData\Local\IPCCTV\camui\auth`, TLS: `C:\Users\op\AppData\Local\IPCCTV\tls`}},
		{"windows without LOCALAPPDATA", "windows", envOf(nil, `C:\Users\op`, `C:\holocron`),
			Dirs{Root: `E:\cameras`, Config: `C:\holocron`, State: `E:\cameras\_state`, Logs: `E:\cameras\_logs`,
				Auth: `C:\Users\op\AppData\Local\IPCCTV\camui\auth`, TLS: `C:\Users\op\AppData\Local\IPCCTV\tls`}},
		{"linux defaults", "linux", envOf(nil, "/home/op", "/usr/local/bin"),
			Dirs{Root: "/srv/cameras", Config: "/home/op/.config/holocron", State: "/srv/cameras/_state", Logs: "/srv/cameras/_logs",
				Auth: "/home/op/.local/state/holocron/auth", TLS: "/home/op/.local/state/holocron/tls"}},
		{"linux xdg", "linux", envOf(map[string]string{"XDG_CONFIG_HOME": "/etc/xc", "XDG_STATE_HOME": "/var/xs"}, "/home/op", "/x"),
			Dirs{Root: "/srv/cameras", Config: "/etc/xc/holocron", State: "/srv/cameras/_state", Logs: "/srv/cameras/_logs",
				Auth: "/var/xs/holocron/auth", TLS: "/var/xs/holocron/tls"}},
		{"linux relative xdg ignored", "linux", envOf(map[string]string{"XDG_CONFIG_HOME": "rel"}, "/home/op", "/x"),
			Dirs{Root: "/srv/cameras", Config: "/home/op/.config/holocron", State: "/srv/cameras/_state", Logs: "/srv/cameras/_logs",
				Auth: "/home/op/.local/state/holocron/auth", TLS: "/home/op/.local/state/holocron/tls"}},
		{"darwin", "darwin", envOf(nil, "/Users/op", "/x"),
			Dirs{Root: "/Users/op/Movies/cameras", Config: "/Users/op/Library/Application Support/holocron",
				State: "/Users/op/Movies/cameras/_state", Logs: "/Users/op/Movies/cameras/_logs",
				Auth: "/Users/op/Library/Application Support/holocron/auth", TLS: "/Users/op/Library/Application Support/holocron/tls"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultDirs(tc.goos, tc.env); got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func paths(p []Protected) map[string]string {
	m := map[string]string{}
	for _, x := range p {
		m[x.Path] = x.Why
	}
	return m
}

func TestProtectedFor(t *testing.T) {
	win := paths(ProtectedFor("windows", envOf(map[string]string{
		"SystemRoot": `C:\Windows`, "ProgramW6432": `C:\Program Files`, "ProgramFiles": `C:\Program Files`,
		"ProgramFiles(x86)": `C:\Program Files (x86)`, "USERPROFILE": `C:\Users\op`,
	}, `C:\Users\op`, `C:\holocron`)))
	for _, p := range []string{`C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`, `C:\Users\op`, `C:\Users`, `C:\holocron`} {
		if _, ok := win[p]; !ok {
			t.Errorf("windows set lacks %s: %v", p, win)
		}
	}
	lin := paths(ProtectedFor("linux", envOf(nil, "/home/op", "/opt/holocron/bin", "/mnt/data", "/")))
	for _, p := range []string{"/usr", "/etc", "/var", "/home/op", "/home", "/mnt/data", "/opt/holocron/bin", "/"} {
		if _, ok := lin[p]; !ok {
			t.Errorf("linux set lacks %s: %v", p, lin)
		}
	}
	if _, bad := lin["/srv/cameras"]; bad {
		t.Error("the default Linux archive root must not be protected")
	}
	mac := paths(ProtectedFor("darwin", envOf(nil, "/Users/op", "/Applications/h", "/Volumes/Disk")))
	for _, p := range []string{"/System", "/Users", "/Users/op", "/Volumes/Disk", "/Applications/h"} {
		if _, ok := mac[p]; !ok {
			t.Errorf("darwin set lacks %s: %v", p, mac)
		}
	}
	if len(ProtectedFor("windows", envOf(nil, "", ""))) != 0 {
		t.Error("empty environment must give an empty set, not empty-string entries")
	}
}

func TestFake(t *testing.T) {
	f := &Fake{Free: map[string]uint64{"/a": 5}, DefaultFree: 9}
	if v, _ := f.DiskFree("/a"); v != 5 {
		t.Error(v)
	}
	if v, _ := f.DiskFree("/b"); v != 9 || f.Name() != "linux" {
		t.Error(v)
	}
	f.FreeErr = errors.New("boom")
	if _, err := f.DiskFree("/b"); err == nil {
		t.Error("FreeErr not returned")
	}
	_ = f.SetOwnerOnly("/x")
	if !reflect.DeepEqual(f.OwnerOnlyCalls(), []string{"/x"}) {
		t.Error(f.OwnerOnlyCalls())
	}
}

func TestRealPlatform(t *testing.T) {
	p := Current()
	if p.Name() != runtime.GOOS {
		t.Errorf("Name = %s", p.Name())
	}
	dir := t.TempDir()
	free, err := p.DiskFree(dir)
	if err != nil || free == 0 {
		t.Errorf("DiskFree(%s) = %d, %v", dir, free, err)
	}
	if _, err := p.DiskFree(dir + string(os.PathSeparator) + "does-not-exist"); err == nil {
		t.Error("DiskFree of a missing path must fail")
	}
	exe, _ := os.Executable()
	found := false
	for _, pr := range p.ProtectedRoots() {
		if pr.Path == "" {
			t.Error("empty protected path")
		}
		if pr.Why == "the program's own directory" {
			found = true
		}
	}
	if !found {
		t.Errorf("own directory of %s missing from the protected set", exe)
	}
	d := p.Dirs()
	if d.Root == "" || d.State == "" || d.Logs == "" || d.Auth == "" || d.TLS == "" {
		t.Errorf("incomplete dirs: %+v", d)
	}
}
