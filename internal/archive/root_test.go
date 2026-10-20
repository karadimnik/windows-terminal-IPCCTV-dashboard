package archive

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"holocron/internal/osx"
)

func envOf(m map[string]string, home, exe string, mounts ...string) osx.Env {
	return osx.Env{Getenv: func(k string) string { return m[k] }, Home: home, ExeDir: exe, Mounts: mounts}
}

// The lexical gate list per OS, on fake platform data (so the Windows table runs on Linux too).
func TestCheckRootTables(t *testing.T) {
	winEnv := envOf(map[string]string{
		"SystemRoot": `C:\Windows`, "ProgramW6432": `C:\Program Files`, "ProgramFiles": `C:\Program Files`,
		"ProgramFiles(x86)": `C:\Program Files (x86)`, "USERPROFILE": `C:\Users\op`,
	}, `C:\Users\op`, `D:\tools\holocron`)
	linEnv := envOf(nil, "/home/op", "/opt/holocron/bin", "/mnt/data", "/mnt/disk2", "/")
	macEnv := envOf(nil, "/Users/op", "/Applications/Holocron.app/bin", "/Volumes/Cams", "/")

	type row struct {
		path string
		want string // substring of the problem; "" = acceptable
	}
	tables := []struct {
		goos string
		env  osx.Env
		rows []row
	}{
		{"windows", winEnv, []row{
			{`E:\cameras`, ""},
			{`E:\cameras\`, ""},
			{`e:/cameras/front`, ""},
			{`C:\Users\op\Videos\cameras`, ""}, // inside the profile: the marker decides
			{`C:\Windows\Temp\x`, ""},          // inside the Windows directory: the marker decides
			{``, "empty"},
			{`   `, "empty"},
			{`cameras`, "not an absolute path"},
			{`.\cameras`, "not an absolute path"},
			{`\cameras`, "not an absolute path"},
			{`E:cameras`, "not an absolute path"},
			{`E:\`, "root of the volume E:"},
			{`E:`, "not an absolute path"},
			{`e:\\`, "root of the volume E:"},
			{`E:\cameras\..`, "root of the volume E:"},
			{`\\nas\share`, "root of the share"},
			{`\\nas\share\`, "root of the share"},
			{`\\nas`, "UNC"},
			{`\\nas\share\cams`, ""},
			{`\\?\E:\`, "root of the volume"},
			{`\\?\UNC\nas\share`, "root of the share"},
			{`C:\Windows`, "is the Windows directory"},
			{`c:\windows\`, "is the Windows directory"},
			{`C:\Program Files`, "is Program Files"},
			{`C:\Program Files (x86)`, "Program Files (x86)"},
			{`C:\Users\op`, "is the user profile"},
			{`C:\Users`, "contains the user profile"},
			{`C:\`, "root of the volume"},
			{`D:\tools\holocron`, "is the program's own directory"},
			{`D:\tools`, "contains the program's own directory"},
			{`D:\`, "root of the volume"},
			{`C:\Users\op\..`, "contains the user profile"},
		}},
		{"linux", linEnv, []row{
			{`/srv/cameras`, ""},
			{`/srv/cameras/`, ""},
			{`/home/op/cameras`, ""},
			{`/mnt/data/cameras`, ""},
			{`/var/lib/holocron`, ""},
			{`/tmp/x/001`, ""},
			{``, "empty"},
			{`cameras`, "not an absolute path"},
			{`./cameras`, "not an absolute path"},
			{`~/cameras`, "not an absolute path"},
			{`/`, "root of the filesystem"},
			{`//`, "root of the filesystem"},
			{`/srv/cameras/..`, "is a system folder"}, // -> /srv
			{`/usr`, "is a system folder"},
			{`/etc`, "is a system folder"},
			{`/var`, "is a system folder"},
			{`/home/op`, "is the home directory"},
			{`/home`, "is "},
			{`/mnt/data`, "is a mount point"},
			{`/mnt`, "is a system folder"},
			{`/opt/holocron/bin`, "is the program's own directory"},
			{`/opt/holocron`, "contains the program's own directory"},
			{`/opt`, "is a system folder"},
			{`/USR`, ""}, // Linux is case-sensitive
		}},
		{"darwin", macEnv, []row{
			{`/Users/op/Movies/cameras`, ""},
			{`/Volumes/Cams/footage`, ""},
			{`/Volumes/Cams`, "is a mount point"},
			{`/Volumes`, "is a system folder"},
			{`/Users/op`, "is the home directory"},
			{`/Users`, "is "},
			{`/System`, "is a system folder"},
			{`/system`, "is a system folder"}, // case-insensitive
			{`/Applications/Holocron.app`, "contains the program's own directory"},
			{`/`, "root of the filesystem"},
		}},
	}
	for _, tb := range tables {
		p := osx.NewFake(tb.goos, tb.env)
		for _, r := range tb.rows {
			got := CheckRoot(r.path, p)
			if r.want == "" && got != "" || r.want != "" && !strings.Contains(got, r.want) {
				t.Errorf("%s CheckRoot(%q) = %q, want %q", tb.goos, r.path, got, r.want)
			}
		}
	}
}

// Real checks on the current OS: read-only queries and t.TempDir() only.
func TestCheckRootRealPlatform(t *testing.T) {
	p := osx.Current()
	good := t.TempDir()
	if got := CheckRoot(good, p); got != "" {
		t.Errorf("a fresh temp folder is refused: %s", got)
	}
	if got := CheckRoot(filepath.Join(good, "not-created-yet"), p); got != "" {
		t.Errorf("a folder that does not exist yet is refused: %s", got)
	}
	vol := filepath.VolumeName(good) + string(filepath.Separator)
	if got := CheckRoot(vol, p); got == "" {
		t.Errorf("the filesystem root %q is accepted", vol)
	}
	if got := CheckRoot("relative/dir", p); !strings.Contains(got, "not an absolute path") {
		t.Errorf("relative: %q", got)
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		if got := CheckRoot(home, p); got == "" {
			t.Errorf("the home folder %q is accepted", home)
		}
		if got := CheckRoot(filepath.Dir(home), p); got == "" {
			t.Errorf("the folder holding the homes %q is accepted", filepath.Dir(home))
		}
	}
	exe, _ := os.Executable()
	if got := CheckRoot(filepath.Dir(exe), p); !strings.Contains(got, "own directory") {
		t.Errorf("the program's own folder: %q", got)
	}
	if got := CheckRoot(filepath.Dir(filepath.Dir(exe)), p); got == "" {
		t.Errorf("an ancestor of the program's folder is accepted")
	}
	if runtime.GOOS == "windows" {
		for _, env := range []string{"SystemRoot", "ProgramFiles"} {
			if v := os.Getenv(env); v != "" && CheckRoot(v, p) == "" {
				t.Errorf("%%%s%% = %s is accepted", env, v)
			}
		}
	} else {
		for _, d := range []string{"/usr", "/etc", "/var"} {
			if CheckRoot(d, p) == "" {
				t.Errorf("%s is accepted", d)
			}
		}
	}
}

func TestCheckRootRefusesSymlink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	p := osx.Current()
	if got := CheckRoot(real, p); got != "" { // control: the real folder is fine
		t.Errorf("real folder refused: %s", got)
	}
	if got := CheckRoot(link, p); !strings.Contains(got, "symbolic link or reparse point") {
		t.Errorf("symlinked root: %q", got)
	}
	// a symlinked ANCESTOR is judged by where it lands: anc -> base, root anc/sub is base/sub,
	// which contains the protected base/sub/inner although the lexical path does not.
	sub := filepath.Join(base, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	anc := filepath.Join(t.TempDir(), "anc")
	if err := os.Symlink(base, anc); err != nil {
		t.Skip(err)
	}
	prot := &osx.Fake{OS: runtime.GOOS, Protected: []osx.Protected{{Path: filepath.Join(sub, "inner"), Why: "a protected folder"}}}
	if got := CheckRoot(filepath.Join(anc, "sub"), prot); !strings.Contains(got, "resolves to") || !strings.Contains(got, "contains a protected folder") {
		t.Errorf("symlinked ancestor: %q", got)
	}
	if got := CheckRoot(filepath.Join(anc, "sub"), &osx.Fake{OS: runtime.GOOS}); got != "" { // control: nothing protected
		t.Errorf("control: %q", got)
	}
}
