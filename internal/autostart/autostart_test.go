package autostart

import (
	"errors"
	"flag"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"holocron/internal/osx"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(path, []byte(got), 0o644)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update once)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestGeneratedEntriesGolden(t *testing.T) {
	r := newRigT(t, "windows")
	e, err := Generate(r.env, winSpec(r))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "task.xml", e.Content)
	if e.Command() != `C:\Holocron\holocron.exe --config C:\Holocron\holocron.config.json up` || e.WorkDir != `C:\Holocron` || e.Exe != `C:\Holocron\holocron.exe` || e.Window != WindowVisible {
		t.Errorf("task command %q workdir %q exe %q window %q", e.Command(), e.WorkDir, e.Exe, e.Window)
	}
	hs := winSpec(r)
	hs.Window = WindowHidden
	h, err := Generate(r.env, hs)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "task-hidden.xml", h.Content)
	if h.Command() != `C:\Windows\System32\conhost.exe --headless C:\Holocron\holocron.exe --config C:\Holocron\holocron.config.json up --detach` || h.Exe != `C:\Holocron\holocron.exe` || h.Window != WindowHidden {
		t.Errorf("hidden task command %q exe %q window %q", h.Command(), h.Exe, h.Window)
	}
	// both forms: interactive token (only while the user is logged on, so the console is on the
	// desktop), the task not hidden, no time limit, a second start ignored
	for _, x := range []string{e.Content, h.Content} {
		for _, want := range []string{"<LogonType>InteractiveToken</LogonType>", "<Hidden>false</Hidden>", "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>"} {
			if !strings.Contains(x, want) {
				t.Errorf("task XML lacks %s", want)
			}
		}
	}
	for _, want := range []string{"<LogonTrigger>", `<UserId>PC\alice</UserId>`, "<UserId>S-1-5-21-1000-1000-1000-1001</UserId>", "<LogonType>InteractiveToken</LogonType>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<StartWhenAvailable>true</StartWhenAvailable>", "<Enabled>true</Enabled>\n    <Hidden>false</Hidden>"} {
		if !strings.Contains(e.Content, want) {
			t.Errorf("task XML lacks %s", want)
		}
	}
	s := winSpec(r)
	s.Disabled = true
	if e, _ := Generate(r.env, s); !strings.Contains(e.Content, "<Enabled>false</Enabled>\n    <Hidden>") || e.Enabled ||
		!strings.Contains(e.Content, "<LogonTrigger>\n      <Enabled>false</Enabled>") {
		t.Error("a disabled spec must generate a disabled task with a disabled trigger")
	}

	r = newRigT(t, "linux")
	e, err = Generate(r.env, unixSpec(r, Systemd))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "holocron.service", e.Content)
	if e.File != path.Join(r.env.Home, ".config", "systemd", "user", "holocron.service") {
		t.Errorf("unit file %s", e.File)
	}
	r.env.ConfigHome = "/xdg"
	if e, _ := Generate(r.env, unixSpec(r, Systemd)); e.File != "/xdg/systemd/user/holocron.service" {
		t.Errorf("XDG_CONFIG_HOME ignored: %s", e.File)
	}

	r = newRigT(t, "darwin")
	r.env.Home = "/Users/alice"
	e, err = Generate(r.env, unixSpec(r, Launchd))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "com.holocron.nvr.plist", e.Content)
	argv, wd, err := parsePlist(e.Content)
	if err != nil || !reflect.DeepEqual(argv, e.Argv) || wd != e.WorkDir {
		t.Errorf("plist read back: %v %q %v", argv, wd, err)
	}
	if e.File != "/Users/alice/Library/LaunchAgents/com.holocron.nvr.plist" {
		t.Errorf("plist file %s", e.File)
	}
}

func TestRootIsPassedOnOnlyWhenGiven(t *testing.T) {
	r := newRigT(t, "linux")
	s := unixSpec(r, Systemd)
	s.Root = "/srv/holo root"
	e, _ := Generate(r.env, s)
	if !strings.Contains(e.Content, `ExecStart=/opt/holocron/holocron --config /home/alice/.config/holocron/holocron.config.json --root "/srv/holo root" host --service`) {
		t.Errorf("unit:\n%s", e.Content)
	}
}

func TestWindowsPathsQuotedEscapedOrRefused(t *testing.T) {
	r := newRigT(t, "windows")
	s := winSpec(r)
	s.Exe = `C:\Program Files\Holo & Cron\holocron.exe`
	s.Config = `C:\Program Files\Holo & Cron\holocron.config.json`
	e, err := Generate(r.env, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Content, `<Command>C:\Program Files\Holo &amp; Cron\holocron.exe</Command>`) ||
		!strings.Contains(e.Content, `<Arguments>--config "C:\Program Files\Holo &amp; Cron\holocron.config.json" up</Arguments>`) ||
		!strings.Contains(e.Content, `<WorkingDirectory>C:\Program Files\Holo &amp; Cron</WorkingDirectory>`) {
		t.Errorf("spaces and & not quoted/escaped:\n%s", e.Content)
	}
	got, err := parseTask(e.Content)
	if err != nil || got.Exe != s.Exe || got.Argv[2] != s.Config {
		t.Errorf("read back %+v %v", got, err)
	}
	s.Window = WindowHidden
	e, _ = Generate(r.env, s)
	if !strings.Contains(e.Content, `<Arguments>--headless "C:\Program Files\Holo &amp; Cron\holocron.exe" --config "C:\Program Files\Holo &amp; Cron\holocron.config.json" up --detach</Arguments>`) {
		t.Errorf("hidden: spaces and & not quoted/escaped:\n%s", e.Content)
	}
	if got, err := parseTask(e.Content); err != nil || got.Exe != s.Exe || got.Argv[4] != s.Config || got.Window != WindowHidden {
		t.Errorf("hidden read back %+v %v", got, err)
	}
	for _, bad := range []string{`C:\Holo%PATH%\holocron.exe`, `C:\Holo"x\holocron.exe`, `holocron.exe`, "C:\\Holo\nx\\holocron.exe"} {
		s := winSpec(r)
		s.Exe = bad
		if _, err := Generate(r.env, s); !errors.Is(err, ErrUsage) {
			t.Errorf("%q: want a usage refusal, got %v", bad, err)
		}
	}
}

func TestCommandLineRoundTrips(t *testing.T) {
	for _, args := range [][]string{
		{`C:\Program Files\x\holocron.exe`, "--config", `C:\a b\c.json`, "up"},
		{`C:\trailing slash\`, `a"b`, "", `back\\"slash`, "tab\there"},
	} {
		if got := splitWin(winJoin(args)); !reflect.DeepEqual(got, args) {
			t.Errorf("windows: %q -> %q -> %q", args, winJoin(args), got)
		}
	}
	for _, args := range [][]string{
		{"/opt/holo cron/holocron", "--config", "/home/a/100%/c.json", "--root", "/srv/$HOME/x", `q"uote\back`},
	} {
		j := sdJoin(args)
		if !strings.Contains(j, `100%%`) || !strings.Contains(j, `$$HOME`) || !strings.Contains(j, `"/opt/holo cron/holocron"`) {
			t.Errorf("systemd escaping: %s", j)
		}
		if got := sdSplit(j); !reflect.DeepEqual(got, args) {
			t.Errorf("systemd: %q -> %s -> %q", args, j, got)
		}
	}
}

func TestLogonInstallIdempotentVerifiedRecorded(t *testing.T) {
	r := newRigT(t, "windows")
	s := winSpec(r)
	res, err := Install(r.ops(), s, "v1")
	if err != nil || !res.Created || !res.Changed {
		t.Fatalf("install: %+v %v\n%s", res, err, r.out.String())
	}
	calls := r.fos.Calls()
	if len(calls) != 3 || !strings.HasPrefix(calls[0], "schtasks /Query /TN Holocron /XML") || !strings.HasPrefix(calls[1], "schtasks /Create /TN Holocron /XML ") ||
		strings.HasSuffix(calls[1], "/F") || !strings.HasPrefix(calls[2], "schtasks /Query") {
		t.Errorf("calls %q", calls)
	}
	rec, ok, err := ReadRecord(s.Record)
	if !ok || err != nil || rec.Mode != Logon || rec.Name != TaskName || rec.Exe != s.Exe || rec.Version != "v1" || !strings.HasSuffix(rec.Command, " up") || rec.Window != WindowVisible {
		t.Errorf("record %+v %v %v", rec, ok, err)
	}
	if left, _ := filepath.Glob(filepath.Join(r.dir, "task-*.xml")); len(left) != 0 {
		t.Errorf("temporary XML left behind: %v", left)
	}
	st := Status(r.env, Logon, TaskName)
	if !st.Installed || !st.Enabled || st.Exe != s.Exe || !st.ExeExists {
		t.Errorf("status %+v", st)
	}

	// second run: unchanged, read-only
	r.fos.Reset()
	res, err = Install(r.ops(), s, "v1")
	if err != nil || res.Changed || !strings.Contains(r.out.String(), "already installed, unchanged") || len(Mutating(r.fos.Calls())) != 0 {
		t.Errorf("second install: %+v %v %q %q", res, err, r.out.String(), r.fos.Calls())
	}

	// moved binary: an update that says what changed, replacing (/F), never a second task
	r.fos.Reset()
	s.Exe = `D:\Holocron\holocron.exe`
	res, err = Install(r.ops(), s, "v2")
	if err != nil || res.Created || !res.Changed || len(res.Changes) != 1 || !strings.Contains(res.Changes[0], `D:\Holocron\holocron.exe`) {
		t.Errorf("update: %+v %v", res, err)
	}
	if m := Mutating(r.fos.Calls()); len(m) != 1 || !strings.HasSuffix(m[0], "/F") {
		t.Errorf("update calls %q", m)
	}
	if len(r.fos.Tasks) != 1 {
		t.Errorf("tasks %v", r.fos.Tasks)
	}
}

func TestLogonEnableDisableRemove(t *testing.T) {
	r := newRigT(t, "windows")
	s := winSpec(r)
	s.Disabled = true
	if _, err := Install(r.ops(), s, "v1"); err != nil {
		t.Fatal(err)
	}
	if st := Status(r.env, Logon, TaskName); !st.Installed || st.Enabled {
		t.Fatalf("installed disabled: %+v", st)
	}
	trigger := func() string {
		return regexp.MustCompile(`(?s)<LogonTrigger>.*?</LogonTrigger>`).FindString(r.fos.Tasks[TaskName])
	}
	r.fos.Reset()
	if ch, err := SetEnabled(r.ops(), Logon, TaskName, true); !ch || err != nil {
		t.Fatal(ch, err)
	}
	if m := Mutating(r.fos.Calls()); len(m) != 1 || m[0] != "schtasks /Change /TN Holocron /ENABLE" {
		t.Errorf("enable %q", m)
	}
	if !strings.Contains(trigger(), "<Enabled>false</Enabled>") {
		t.Errorf("enabling a test task must leave its trigger off: %s", trigger())
	}
	if ch, err := SetEnabled(r.ops(), Logon, TaskName, true); ch || err != nil {
		t.Errorf("enable twice: %v %v", ch, err)
	}
	if ch, err := SetEnabled(r.ops(), Logon, TaskName, false); !ch || err != nil || Status(r.env, Logon, TaskName).Enabled {
		t.Errorf("disable: %v %v", ch, err)
	}
	r.fos.Reset()
	if rm, err := Remove(r.ops(), Logon, TaskName); !rm || err != nil {
		t.Fatal(rm, err)
	}
	if m := Mutating(r.fos.Calls()); len(m) != 1 || m[0] != "schtasks /Delete /TN Holocron /F" {
		t.Errorf("remove %q", m)
	}
	if rm, err := Remove(r.ops(), Logon, TaskName); rm || err != nil {
		t.Errorf("remove twice: %v %v", rm, err)
	}
	if _, err := SetEnabled(r.ops(), Logon, TaskName, true); err == nil {
		t.Error("enable of an absent task must fail")
	}
}

func TestRollbackOnFailingStep(t *testing.T) {
	// verification fails: the task this run created is deleted again
	r := newRigT(t, "windows")
	r.fos.Mangle = func(x string) string { return strings.Replace(x, "</Arguments>", " --mangled</Arguments>", 1) }
	_, err := Install(r.ops(), winSpec(r), "v1")
	if err == nil || !strings.Contains(err.Error(), "verification") || !strings.Contains(err.Error(), "rolled back") || len(r.fos.Tasks) != 0 {
		t.Errorf("err %v tasks %v", err, r.fos.Tasks)
	}
	if _, ok, _ := ReadRecord(winSpec(r).Record); ok {
		t.Error("a failed install left a record")
	}

	// an update whose verification fails puts the previous task back
	r = newRigT(t, "windows")
	if _, err := Install(r.ops(), winSpec(r), "v1"); err != nil {
		t.Fatal(err)
	}
	before := r.fos.Tasks[TaskName]
	r.fos.Mangle = func(x string) string {
		if strings.Contains(x, `D:\`) {
			return strings.Replace(x, "</Arguments>", " --mangled</Arguments>", 1)
		}
		return x
	}
	s := winSpec(r)
	s.Exe = `D:\Holocron\holocron.exe`
	if _, err := Install(r.ops(), s, "v2"); err == nil || !strings.Contains(err.Error(), "previous entry was put back") {
		t.Errorf("update rollback: %v", err)
	}
	if after, _ := parseTask(r.fos.Tasks[TaskName]); after == nil || after.Exe != `C:\Holocron\holocron.exe` {
		t.Errorf("the previous task is not back: %v", r.fos.Tasks[TaskName] == before)
	}

	// systemd: enable fails after the unit was written: the unit file is gone again
	r = newRigT(t, "linux")
	r.fos.Fail["systemctl:enable"] = ErrFakeExit
	us := unixSpec(r, Systemd)
	_, err = Install(r.ops(), us, "v1")
	unit := filepath.Join(r.env.Home, ".config", "systemd", "user", "holocron.service")
	if _, serr := os.Stat(unit); err == nil || !os.IsNotExist(serr) {
		t.Errorf("err %v, unit file still there: %v", err, serr)
	}

	// schtasks /Create fails: nothing was created, nothing to remove
	r = newRigT(t, "windows")
	r.fos.Fail["schtasks:/Create"] = ErrFakeExit
	if _, err := Install(r.ops(), winSpec(r), "v1"); err == nil || strings.Contains(strings.Join(r.fos.Calls(), ","), "/Delete") {
		t.Errorf("create failure: %v %q", err, r.fos.Calls())
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	for _, c := range []struct {
		goos string
		spec func(*rigT) Spec
	}{
		{"windows", winSpec},
		{"linux", func(r *rigT) Spec { return unixSpec(r, Systemd) }},
		{"darwin", func(r *rigT) Spec { return unixSpec(r, Launchd) }},
	} {
		r := newRigT(t, c.goos)
		r.dry = true
		s := c.spec(r)
		res, err := Install(r.ops(), s, "v1")
		out := r.out.String()
		if err != nil || !res.Created {
			t.Fatalf("%s: %v", c.goos, err)
		}
		if m := Mutating(r.fos.Calls()); len(m) != 0 {
			t.Errorf("%s: dry run ran %q", c.goos, m)
		}
		if !strings.Contains(out, "would") || !strings.Contains(out, "would write the install record") {
			t.Errorf("%s: dry run output:\n%s", c.goos, out)
		}
		if !strings.Contains(out, res.Entry.Content[:40]) && !strings.Contains(out, strings.Split(res.Entry.Content, "\n")[1]) {
			t.Errorf("%s: dry run does not show the generated content:\n%s", c.goos, out)
		}
		if _, ok, _ := ReadRecord(s.Record); ok {
			t.Errorf("%s: dry run wrote a record", c.goos)
		}
		if ents, _ := os.ReadDir(r.dir); len(ents) != 1 { // only the "present" stand-in file
			t.Errorf("%s: dry run created files: %v", c.goos, ents)
		}
		// against a real entry: remove and switch in a dry run change nothing either
		r.dry = false
		if _, err := Install(r.ops(), s, "v1"); err != nil {
			t.Fatal(err)
		}
		r.dry = true
		r.fos.Reset()
		if rm, err := Remove(r.ops(), s.Mode, s.Name); !rm || err != nil {
			t.Errorf("%s: dry remove %v %v", c.goos, rm, err)
		}
		if ch, err := SetEnabled(r.ops(), s.Mode, s.Name, false); !ch || err != nil {
			t.Errorf("%s: dry disable %v %v", c.goos, ch, err)
		}
		if m := Mutating(r.fos.Calls()); len(m) != 0 {
			t.Errorf("%s: dry remove/disable ran %q", c.goos, m)
		}
		if !Status(r.env, s.Mode, s.Name).Installed || !Status(r.env, s.Mode, s.Name).Enabled {
			t.Errorf("%s: dry run changed the entry", c.goos)
		}
	}
}

func TestSystemdSequences(t *testing.T) {
	r := newRigT(t, "linux")
	s := unixSpec(r, Systemd)
	if _, err := Install(r.ops(), s, "v1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user enable holocron.service"}
	if m := Mutating(r.fos.Calls()); !reflect.DeepEqual(m, want) {
		t.Errorf("install %q", m)
	}
	st := Status(r.env, Systemd, UnitName)
	if !st.Installed || !st.Enabled || st.Running != "inactive" || st.Exe != s.Exe {
		t.Errorf("status %+v", st)
	}
	r.fos.Reset()
	SetEnabled(r.ops(), Systemd, UnitName, false)
	SetEnabled(r.ops(), Systemd, UnitName, true)
	if m := Mutating(r.fos.Calls()); !reflect.DeepEqual(m, []string{"systemctl --user disable holocron.service", "systemctl --user enable holocron.service"}) {
		t.Errorf("switch %q", m)
	}
	r.fos.Reset()
	if _, err := Remove(r.ops(), Systemd, UnitName); err != nil {
		t.Fatal(err)
	}
	if m := Mutating(r.fos.Calls()); !reflect.DeepEqual(m, []string{"systemctl --user disable holocron.service", "systemctl --user daemon-reload"}) {
		t.Errorf("remove %q", m)
	}
	// a test-disabled unit is written and reloaded but never enabled
	r = newRigT(t, "linux")
	s = unixSpec(r, Systemd)
	s.Name, s.Disabled = "holocron-TEST-x1", true
	if _, err := Install(r.ops(), s, "v1"); err != nil {
		t.Fatal(err)
	}
	if m := Mutating(r.fos.Calls()); !reflect.DeepEqual(m, []string{"systemctl --user daemon-reload"}) {
		t.Errorf("disabled install %q", m)
	}
}

func TestLaunchdSequences(t *testing.T) {
	r := newRigT(t, "darwin")
	s := unixSpec(r, Launchd)
	if _, err := Install(r.ops(), s, "v1"); err != nil {
		t.Fatal(err)
	}
	if m := Mutating(r.fos.Calls()); !reflect.DeepEqual(m, []string{"launchctl enable gui/501/com.holocron.nvr"}) {
		t.Errorf("install %q", m)
	}
	r.fos.Reset()
	SetEnabled(r.ops(), Launchd, LaunchLabel, false)
	if st := Status(r.env, Launchd, LaunchLabel); st.Enabled || !st.Installed || st.Running != "not loaded" {
		t.Errorf("after disable %+v", st)
	}
	r.fos.Loaded[LaunchLabel] = true
	r.fos.Reset()
	if _, err := Remove(r.ops(), Launchd, LaunchLabel); err != nil {
		t.Fatal(err)
	}
	if m := Mutating(r.fos.Calls()); !reflect.DeepEqual(m, []string{"launchctl bootout gui/501/com.holocron.nvr"}) {
		t.Errorf("remove %q", m)
	}
}

func TestOldTasksAreOnlyRead(t *testing.T) {
	r := newRigT(t, "windows")
	old := strings.Replace(taskXML(`PC\alice`, "S-1", `C:\x\powershell.exe`, "-File x.ps1", `C:\x`, false, true), "\n", "\r\n", -1)
	r.fos.Tasks["IPCCTV-Recorder"] = strings.Replace(old, "<Enabled>false</Enabled>\r\n    <Hidden>", "<Enabled>true</Enabled>\r\n    <Hidden>", 1)
	r.fos.Tasks["IPCCTV-Dashboard"] = old
	got := QueryOldTasks(r.env)
	want := []OldTask{{Name: "IPCCTV-Recorder", Found: true, Enabled: true}, {Name: "IPCCTV-Dashboard", Found: true}, {Name: "IPCCTV-Ui", Detail: "ERROR: The system cannot find the file specified."}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("old tasks %+v", got)
	}
	if m := Mutating(r.fos.Calls()); len(m) != 0 {
		t.Errorf("old tasks were changed: %q", m)
	}
	// a whole install / uninstall cycle never names them
	Install(r.ops(), winSpec(r), "v1")
	Remove(r.ops(), Logon, TaskName)
	for _, c := range r.fos.Calls() {
		if strings.Contains(c, "IPCCTV") && !strings.HasPrefix(c, "schtasks /Query") {
			t.Errorf("touched an old task: %s", c)
		}
	}
	if r.fos.Tasks["IPCCTV-Dashboard"] != old {
		t.Error("an old task changed")
	}
}

func TestModesNamesAndTransientExe(t *testing.T) {
	for _, c := range []struct {
		mode, goos string
		want       Mode
		usage      bool
	}{
		{"", "windows", Logon, false}, {"", "linux", Systemd, false}, {"", "darwin", Launchd, false}, {"service", "windows", Service, false},
		{"systemd", "windows", "", true}, {"logon", "linux", "", true}, {"bogus", "linux", "", true}, {"", "plan9", "", true},
	} {
		m, err := ParseMode(c.mode, c.goos)
		if m != c.want || errors.Is(err, ErrUsage) != c.usage {
			t.Errorf("%q on %s: %q %v", c.mode, c.goos, m, err)
		}
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{{"Holocron", true}, {"Holocron-TEST-w12", true}, {"Other", false}, {"IPCCTV-Recorder", false}, {"x-TEST-y z", false}, {`a\-TEST-b`, false}} {
		if err := CheckName(c.name, Logon); (err == nil) != c.ok {
			t.Errorf("name %q: %v", c.name, err)
		}
	}
	env := Env{TempDirs: []string{`C:\Users\alice\AppData\Local\Temp`, `C:\Windows\Temp`, "/tmp"}}
	for exe, transient := range map[string]bool{
		`C:\Windows\Temp\holocron-w12\holocron.exe`: true, `c:\users\alice\appdata\local\temp\x\holocron.exe`: true,
		"/tmp/go-build123/b001/exe/holocron": true, "/home/alice/.cache/go-build/x/holocron": true,
		`C:\Holocron\holocron.exe`: false, "/opt/holocron/holocron": false, "/tmpfoo/holocron": false,
	} {
		if got := TransientExe(exe, env) != ""; got != transient {
			t.Errorf("%s: transient %v", exe, got)
		}
	}
}

func TestStatusSummaryWarnsOnMissingExe(t *testing.T) {
	r := newRigT(t, "windows")
	s := winSpec(r)
	s.Exe = `C:\missing\holocron.exe`
	if _, err := Install(r.ops(), s, "v1"); err != nil {
		t.Fatal(err)
	}
	st := Status(r.env, Logon, TaskName)
	if st.ExeExists || !strings.Contains(st.Summary(), "does not exist") {
		t.Errorf("%+v %s", st, st.Summary())
	}
	if !strings.Contains(Status(r.env, Logon, "Holocron-TEST-none").Summary(), "not installed") {
		t.Error("absent summary")
	}
}

func TestDecodeTaskXML(t *testing.T) {
	r := newRigT(t, "windows")
	e, _ := Generate(r.env, winSpec(r))
	for _, b := range [][]byte{encodeUTF16(e.Content), []byte(e.Content), append([]byte("\ufeff"), e.Content...)} {
		got, err := parseTask(decodeTaskXML(b))
		if err != nil || got.Command() != e.Command() || got.Key != e.Key {
			t.Errorf("decode: %v %+v", err, got)
		}
	}
}

func svcRig(t *testing.T) (*rigT, *osx.FakeSCM, Spec) {
	r := newRigT(t, "windows")
	scm := &osx.FakeSCM{}
	r.env.SCM = scm
	s := winSpec(r)
	s.Mode, s.Name, s.Exe = Service, ServiceName, `C:\Program Files\Holocron\holocron.exe`
	return r, scm, s
}

func TestServiceAgainstFakeSCM(t *testing.T) {
	r, scm, s := svcRig(t)
	res, err := Install(r.ops(), s, "v1")
	if err != nil || !res.Created {
		t.Fatalf("%v\n%s", err, r.out.String())
	}
	if got := scm.CallList(); !reflect.DeepEqual(got, []string{"query:Holocron", "create:Holocron", "query:Holocron"}) {
		t.Errorf("calls %q", got)
	}
	c := scm.Services[ServiceName]
	want := osx.ServiceConfig{Name: "Holocron", DisplayName: "Holocron NVR", Account: "LocalSystem", Start: osx.StartAuto, Delayed: true, RestartDelay: 60 * time.Second,
		BinaryPath:  `"C:\Program Files\Holocron\holocron.exe" --config C:\Holocron\holocron.config.json service`,
		Description: c.Description}
	if c != want || !strings.Contains(c.Description, "holocron uninstall") {
		t.Errorf("service config\n%+v\nwant\n%+v", c, want)
	}
	if st := Status(r.env, Service, ServiceName); !st.Installed || !st.Enabled || st.Exe != s.Exe || st.Running != "stopped" {
		t.Errorf("status %+v", st)
	}
	scm.Calls = nil
	if res, err := Install(r.ops(), s, "v1"); err != nil || res.Changed || !reflect.DeepEqual(scm.CallList(), []string{"query:Holocron"}) {
		t.Errorf("second install: %+v %v %q", res, err, scm.CallList())
	}
	s2 := s
	s2.Account = `NT AUTHORITY\LocalService`
	if res, err := Install(r.ops(), s2, "v2"); err != nil || res.Created || len(res.Changes) != 1 || scm.Services[ServiceName].Account != `NT AUTHORITY\LocalService` {
		t.Errorf("update: %+v %v", res, err)
	}
	if ch, err := SetEnabled(r.ops(), Service, ServiceName, false); !ch || err != nil || scm.Services[ServiceName].Start != osx.StartDisabled {
		t.Errorf("disable: %v %v %+v", ch, err, scm.Services[ServiceName])
	}
	if ch, err := SetEnabled(r.ops(), Service, ServiceName, true); !ch || err != nil || scm.Services[ServiceName].Start != osx.StartAuto || !scm.Services[ServiceName].Delayed {
		t.Errorf("enable: %v %v %+v", ch, err, scm.Services[ServiceName])
	}

	// dry runs call nothing that changes the manager
	r.dry = true
	scm.Calls = nil
	SetEnabled(r.ops(), Service, ServiceName, false)
	Remove(r.ops(), Service, ServiceName)
	s3 := s
	s3.Exe = `D:\x\holocron.exe`
	Install(r.ops(), s3, "v3")
	for _, c := range scm.CallList() {
		if !strings.HasPrefix(c, "query:") {
			t.Errorf("dry run called %s", c)
		}
	}
	if !strings.Contains(r.out.String(), "would call the service control manager: ChangeServiceConfig Holocron") {
		t.Errorf("dry run output:\n%s", r.out.String())
	}
	r.dry = false

	// a running service is only marked for deletion: uninstall says so and fails
	scm.Running = map[string]bool{ServiceName: true}
	if _, err := Remove(r.ops(), Service, ServiceName); err == nil || !strings.Contains(err.Error(), "marked for deletion") {
		t.Errorf("running: %v", err)
	}
	scm.Running = nil
	if rm, err := Remove(r.ops(), Service, ServiceName); !rm || err != nil || len(scm.Services) != 0 {
		t.Errorf("remove: %v %v", rm, err)
	}
}

func TestServiceRefusals(t *testing.T) {
	r, scm, s := svcRig(t)
	scm.Fail = map[string]error{"create": osx.ErrAccessDenied}
	_, err := Install(r.ops(), s, "v1")
	if !errors.Is(err, osx.ErrAccessDenied) || !strings.Contains(err.Error(), "elevated") {
		t.Errorf("access denied: %v", err)
	}
	for _, c := range scm.CallList() {
		if c == "delete:Holocron" {
			t.Error("rollback deleted a service this run did not create")
		}
	}
	for _, a := range []string{`PC\alice`, "alice"} {
		s.Account = a
		if _, err := Generate(r.env, s); !errors.Is(err, ErrUsage) {
			t.Errorf("account %q: %v", a, err)
		}
	}
	for _, a := range []string{"", "LocalSystem", `NT AUTHORITY\NetworkService`, `NT SERVICE\Holocron`} {
		s.Account = a
		if _, err := Generate(r.env, s); err != nil {
			t.Errorf("account %q: %v", a, err)
		}
	}
	s.Account, s.Exe = "", `C:\100%\holocron.exe`
	if _, err := Generate(r.env, s); !errors.Is(err, ErrUsage) {
		t.Errorf("percent in the service path: %v", err)
	}
	s.Exe, s.Disabled, s.Name = `C:\Holocron\holocron.exe`, true, "Holocron-TEST-s1"
	scm.Fail = nil
	if _, err := Install(r.ops(), s, "v1"); err != nil || scm.Services["Holocron-TEST-s1"].Start != osx.StartDisabled || scm.Services["Holocron-TEST-s1"].DisplayName != "Holocron NVR (Holocron-TEST-s1)" {
		t.Errorf("test service: %v %+v", err, scm.Services)
	}
}

func TestLogonWindowModes(t *testing.T) {
	r := newRigT(t, "windows")
	s := winSpec(r)
	s.Window = WindowHidden
	if _, err := Install(r.ops(), s, "v1"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := ReadRecord(s.Record); rec.Window != WindowHidden {
		t.Errorf("record %+v", rec)
	}
	if st := Status(r.env, Logon, TaskName); st.Window != WindowHidden || !strings.Contains(st.Summary(), "console window hidden") || st.Exe != s.Exe {
		t.Errorf("status %+v", st)
	}
	// hidden -> visible is an update, reported, one replacing /Create
	r.fos.Reset()
	s.Window = WindowVisible
	res, err := Install(r.ops(), s, "v1")
	if err != nil || res.Created || !res.Changed || !strings.Contains(strings.Join(res.Changes, "\n"), "console window: hidden -> visible") {
		t.Fatalf("update: %+v %v", res, err)
	}
	if m := Mutating(r.fos.Calls()); len(m) != 1 || !strings.HasSuffix(m[0], "/F") || len(r.fos.Tasks) != 1 {
		t.Errorf("update calls %q", m)
	}
	if st := Status(r.env, Logon, TaskName); st.Window != WindowVisible || st.Command != `C:\Holocron\holocron.exe --config C:\Holocron\holocron.config.json up` {
		t.Errorf("status after update %+v", st)
	}
	if rec, _, _ := ReadRecord(s.Record); rec.Window != WindowVisible {
		t.Errorf("record after update %+v", rec)
	}
	if res, err := Install(r.ops(), s, "v1"); err != nil || res.Changed {
		t.Errorf("visible twice: %+v %v", res, err)
	}
	// visible -> hidden whose verification fails: the visible task is put back
	r.fos.Mangle = func(x string) string { return strings.Replace(x, "--detach</Arguments>", "</Arguments>", 1) }
	s.Window = WindowHidden
	if _, err := Install(r.ops(), s, "v1"); err == nil || !strings.Contains(err.Error(), "previous entry was put back") {
		t.Errorf("rollback: %v", err)
	}
	if st := Status(r.env, Logon, TaskName); st.Window != WindowVisible {
		t.Errorf("after rollback %+v", st)
	}
	for _, c := range []struct {
		w    string
		m    Mode
		fail bool
	}{{"", Systemd, false}, {"visible", Logon, false}, {"hidden", Logon, false}, {"hidden", Systemd, true}, {"visible", Service, true}, {"visible", Launchd, true}, {"maybe", Logon, true}} {
		if err := CheckWindow(c.w, c.m); (err != nil) != c.fail || (err != nil && !errors.Is(err, ErrUsage)) {
			t.Errorf("%q %s: %v", c.w, c.m, err)
		}
	}
}
