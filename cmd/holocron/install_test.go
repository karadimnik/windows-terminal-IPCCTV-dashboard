package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/autostart"
)

func (r *rig) unitFile(name string) string {
	return filepath.Join(r.auto.Home, ".config", "systemd", "user", name+".service")
}

func (r *rig) record() (autostart.Record, bool) {
	rec, ok, _ := autostart.ReadRecord(filepath.Join(filepath.Dir(r.cfg), "_state", "install.json"))
	return rec, ok
}

func (r *rig) autostartJSON() autostart.State {
	r.t.Helper()
	out, _, code := r.run("autostart", "status", "--json")
	var rep autostartReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		r.t.Fatalf("autostart status --json: exit %d %v\n%s", code, err, out)
	}
	return rep.Autostart
}

func TestInstallCycleSystemd(t *testing.T) {
	r := newRig(t)
	r.logSpec = "install=debug" // the exact commands and file contents are DEBUG records
	out, errw, code := r.run("install", "--dry-run")
	if code != 0 || !strings.Contains(out, "dry run: nothing is changed") || !strings.Contains(out, "would write "+r.unitFile("holocron")) ||
		!strings.Contains(out, "ExecStart=/nonexistent/holocron --config "+r.cfg+" --root "+r.root+" host --service") || !strings.Contains(out, "would run: systemctl --user daemon-reload") {
		t.Fatalf("dry run: exit %d\n%s%s", code, out, errw)
	}
	if m := autostart.Mutating(r.tools.Calls()); len(m) != 0 {
		t.Errorf("dry run ran %q", m)
	}
	if _, err := os.Stat(r.unitFile("holocron")); !os.IsNotExist(err) {
		t.Error("dry run wrote the unit")
	}
	if _, ok := r.record(); ok {
		t.Error("dry run wrote the record")
	}

	out, errw, code = r.run("install")
	if code != 0 || !strings.Contains(out, "installed and verified: systemd user unit holocron, enabled") || !strings.Contains(out, "loginctl enable-linger alice") {
		t.Fatalf("install: exit %d\n%s%s", code, out, errw)
	}
	rec, ok := r.record()
	if !ok || rec.Mode != autostart.Systemd || rec.Name != "holocron" || rec.Exe != "/nonexistent/holocron" || rec.File != r.unitFile("holocron") {
		t.Errorf("record %+v", rec)
	}
	unit, _ := os.ReadFile(r.unitFile("holocron"))
	if !strings.Contains(string(unit), "Restart=on-failure") {
		t.Errorf("unit:\n%s", unit)
	}
	if out, _, code := r.run("install"); code != 0 || !strings.Contains(out, "already installed, unchanged") {
		t.Errorf("second install: %d %s", code, out)
	}
	st := r.autostartJSON()
	if !st.Installed || !st.Enabled || !st.Recorded || st.ExeExists || st.Exe != "/nonexistent/holocron" {
		t.Errorf("status %+v", st)
	}
	if out, _, _ := r.run("status"); !strings.Contains(out, "autostart: systemd user unit holocron, enabled") || !strings.Contains(out, "does not exist (moved or deleted binary") {
		t.Errorf("status text:\n%s", out)
	}
	if out, _, _ := r.run("doctor"); !strings.Contains(out, "warn autostart") || !strings.Contains(out, "does not exist") {
		t.Errorf("doctor:\n%s", out)
	}

	r.tools.Reset()
	if out, _, code := r.run("autostart", "disable", "--dry-run"); code != 0 || len(autostart.Mutating(r.tools.Calls())) != 0 || !strings.Contains(out, "would run: systemctl --user disable holocron.service") {
		t.Errorf("dry disable: %d %s %q", code, out, r.tools.Calls())
	}
	if out, _, code := r.run("autostart", "disable"); code != 0 || !strings.Contains(out, "disabled and verified") || r.autostartJSON().Enabled {
		t.Errorf("disable: %d %s", code, out)
	}
	if out, _, code := r.run("autostart", "disable"); code != 0 || !strings.Contains(out, "already disabled, unchanged") {
		t.Errorf("disable twice: %d %s", code, out)
	}
	if out, _, code := r.run("autostart", "enable"); code != 0 || !strings.Contains(out, "enabled and verified") || !r.autostartJSON().Enabled {
		t.Errorf("enable: %d %s", code, out)
	}

	r.tools.Reset()
	if out, _, code := r.run("uninstall", "--dry-run"); code != 0 || len(autostart.Mutating(r.tools.Calls())) != 0 || !strings.Contains(out, "would delete "+r.unitFile("holocron")) {
		t.Errorf("dry uninstall: %d %s", code, out)
	}
	if _, ok := r.record(); !ok {
		t.Error("dry uninstall removed the record")
	}
	if out, _, code := r.run("uninstall"); code != 0 || !strings.Contains(out, "removed and verified gone") {
		t.Errorf("uninstall: %d %s", code, out)
	}
	if _, err := os.Stat(r.unitFile("holocron")); !os.IsNotExist(err) {
		t.Error("the unit is still there")
	}
	if _, ok := r.record(); ok {
		t.Error("the record is still there")
	}
	if out, _, code := r.run("uninstall"); code != 0 || !strings.Contains(out, "nothing to remove") {
		t.Errorf("second uninstall: %d %s", code, out)
	}

	log := r.log()
	for _, want := range []string{"action=install.run", "action=uninstall.run", "action=autostart.disable", "action=install.exec", "systemctl --user daemon-reload", "action=install.write", "INFO  [install] created systemd user unit holocron, verified"} {
		if !strings.Contains(log, want) {
			t.Errorf("holocron.log lacks %s", want)
		}
	}
	if strings.Count(log, "INFO  [install] wrote ") != 1 {
		t.Error("a dry run logged a state change at INFO (or the real install did not)")
	}
	audit, _ := os.ReadFile(filepath.Join(r.root, "_logs", "audit.log"))
	for _, a := range []string{"install.run", "uninstall.run", "autostart.enable", "autostart.disable", "autostart.status"} {
		if !strings.Contains(string(audit), "action="+a) {
			t.Errorf("%s not in the audit stream", a)
		}
	}
}

func TestInstallRefusals(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"install", "--mode", "logon"}, 2, "not available on linux"},
		{[]string{"install", "--mode", "nope"}, 2, "unknown --mode"},
		{[]string{"install", "--name", "Other"}, 2, "-TEST-"},
		{[]string{"install", "--test-disabled"}, 2, "test entries only"},
		{[]string{"install", "--account", "x"}, 2, "--mode service only"},
		{[]string{"install", "extra"}, 2, "wrong number"},
		{[]string{"autostart", "enable"}, 1, "no record"},
		{[]string{"autostart", "disable"}, 1, "no record"},
	} {
		out, errw, code := r.run(c.args...)
		if code != c.code || !strings.Contains(out+errw, c.msg) {
			t.Errorf("%v: exit %d (want %d), %q not in\n%s%s", c.args, code, c.code, c.msg, out, errw)
		}
	}
	if m := autostart.Mutating(r.tools.Calls()); len(m) != 0 {
		t.Errorf("refusals ran %q", m)
	}

	// a transient executable is refused unless --allow-temp
	r.exe = "/tmp/go-build1234/b001/exe/holocron"
	if out, errw, code := r.run("install"); code != 1 || !strings.Contains(errw, "go build cache") {
		t.Errorf("transient: %d %s%s", code, out, errw)
	}
	if !strings.Contains(r.log(), "result=refused") {
		t.Error("the refusal is not logged as refused")
	}
	if out, errw, code := r.run("install", "--allow-temp", "--name", "holocron-TEST-t1", "--test-disabled"); code != 0 || !strings.Contains(out, "installing anyway because of --allow-temp") {
		t.Errorf("--allow-temp: %d %s%s", code, out, errw)
	}
	if st := r.autostartJSON(); st.Name != "holocron-TEST-t1" || !st.Installed || st.Enabled {
		t.Errorf("test entry %+v", st)
	}
	if m := autostart.Mutating(r.tools.Calls()); strings.Contains(strings.Join(m, ","), "enable") {
		t.Errorf("a test-disabled install enabled the unit: %q", m)
	}
	if _, _, code := r.run("uninstall", "--name", "holocron-TEST-other"); code != 1 {
		t.Errorf("uninstall of a name that is not the recorded one: %d", code)
	}
	if out, _, code := r.run("uninstall"); code != 0 || !strings.Contains(out, "removed") {
		t.Errorf("uninstall: %d %s", code, out)
	}

	// an entry with holocron's name but no record: refused, then removed with --force
	os.MkdirAll(filepath.Dir(r.unitFile("holocron")), 0o755)
	os.WriteFile(r.unitFile("holocron"), []byte("[Service]\nExecStart=/opt/x/holocron up\n"), 0o644)
	if out, errw, code := r.run("uninstall"); code != 1 || !strings.Contains(errw, "did not record it") || !strings.Contains(out, "not recorded by holocron install") {
		t.Errorf("unrecorded: %d %s%s", code, out, errw)
	}
	if _, err := os.Stat(r.unitFile("holocron")); err != nil {
		t.Error("an unrecorded entry was removed without --force")
	}
	if out, _, code := r.run("uninstall", "--force"); code != 0 || !strings.Contains(out, "because of --force") {
		t.Errorf("--force: %d %s", code, out)
	}
	if _, err := os.Stat(r.unitFile("holocron")); !os.IsNotExist(err) {
		t.Error("--force did not remove it")
	}
}

// oldTask is a PowerShell system task as schtasks /Query /XML prints it.
func oldTask(enabled bool) string {
	return "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\r\n<Task version=\"1.3\" xmlns=\"http://schemas.microsoft.com/windows/2004/02/mit/task\">\r\n" +
		"  <Settings>\r\n    <Enabled>" + map[bool]string{true: "true", false: "false"}[enabled] + "</Enabled>\r\n  </Settings>\r\n" +
		"  <Actions Context=\"Author\">\r\n    <Exec>\r\n      <Command>powershell.exe</Command>\r\n    </Exec>\r\n  </Actions>\r\n</Task>\r\n"
}

func TestOldTasksGateWindows(t *testing.T) {
	r := newRig(t)
	r.auto.GOOS = "windows"
	r.tools.Tasks["IPCCTV-Recorder"] = oldTask(true)
	r.tools.Tasks["IPCCTV-Dashboard"] = oldTask(false)
	before := map[string]string{}
	for k, v := range r.tools.Tasks {
		before[k] = v
	}
	out, errw, code := r.run("install")
	if code != 1 || !strings.Contains(out, "WARNING: IPCCTV-Recorder is ENABLED") || !strings.Contains(out, "schtasks /Change /TN IPCCTV-Recorder /DISABLE") ||
		!strings.Contains(out, "IPCCTV-Dashboard disabled") || !strings.Contains(out, "IPCCTV-Ui not found") || !strings.Contains(errw, oldTasksFlag) {
		t.Errorf("gate: %d\n%s%s", code, out, errw)
	}
	// with the override the gate lets it through (the Linux temp paths of the rig then fail the
	// Windows path check, which is all this test needs)
	out, errw, code = r.run("install", "--"+oldTasksFlag)
	if !strings.Contains(out, "going on because of --"+oldTasksFlag) || code != 1 || !strings.Contains(errw, "not an absolute Windows path") {
		t.Errorf("override: %d\n%s%s", code, out, errw)
	}
	// autostart enable is gated the same way
	autostart.WriteRecord(filepath.Join(filepath.Dir(r.cfg), "_state", "install.json"), autostart.Record{Mode: autostart.Logon, Name: autostart.TaskName})
	if out, _, code := r.run("autostart", "enable"); code != 1 || !strings.Contains(out, "WARNING: IPCCTV-Recorder is ENABLED") {
		t.Errorf("enable gate: %d %s", code, out)
	}
	out, _, _ = r.run("autostart", "status")
	if !strings.Contains(out, "PowerShell system tasks: IPCCTV-Recorder ENABLED, IPCCTV-Dashboard disabled, IPCCTV-Ui not found") {
		t.Errorf("autostart status:\n%s", out)
	}
	if out, _, _ := r.run("doctor"); !strings.Contains(out, "old tasks") {
		t.Errorf("doctor:\n%s", out)
	}
	for _, c := range r.tools.Calls() {
		if strings.Contains(c, "IPCCTV") && !strings.HasPrefix(c, "schtasks /Query /TN IPCCTV-") {
			t.Errorf("an old task was touched: %s", c)
		}
	}
	for k, v := range before {
		if r.tools.Tasks[k] != v {
			t.Errorf("%s changed", k)
		}
	}
}
