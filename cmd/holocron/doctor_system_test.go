package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/app"
	"holocron/internal/osx"
)

// hostRunning lists pid 4646 as this root's live holocron host.
func (r *rig) hostRunning(insp *osx.FakeInspector) {
	r.t.Helper()
	insp.Procs[4646] = osx.ProcInfo{Start: "h", Image: "/nonexistent/holocron"}
	r.insp = insp
	os.MkdirAll(filepath.Join(r.root, "_state"), 0o700)
	if err := os.WriteFile(filepath.Join(r.root, "_state", "host.json"), []byte(`{"pid":4646,"start":"h","image":"/nonexistent/holocron","role":"host"}`), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// Incident 22 Oct 2026: the old system's supervisor still alive (status.json fresh, its pid alive,
// no holocron host) made holocron up print "already running" and start nothing.
func TestDoctorIncidentOldSupervisorAlive(t *testing.T) {
	r := newRig(t)
	r.running() // a live pid writes status.json, and no host.json exists: not started by holocron up
	r.doctorTools(nil)
	out, _, code := r.run("doctor", "--group", "system", "--quiet")
	want := "fail processes      supervisor pid "
	wantFix := `     fix: stop that system: .\cam.ps1 -Stop in the PowerShell system's folder (another Holocron: holocron --config <its config> down); then holocron up --detach`
	if code != 1 || !strings.HasPrefix(out, want) || !strings.Contains(out, `holocron up would only print "already running"`) || !strings.Contains(out, wantFix) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	t.Logf("incident fixture (old supervisor alive):\n%s", out)
}

func TestDoctorProcesses(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(r *rig, insp *osx.FakeInspector)
		procs []osx.ProcEntry
		want  string
	}{
		{"healthy", func(r *rig, insp *osx.FakeInspector) { r.hostRunning(insp); r.running() }, nil,
			"system.processes ok   processes      host pid 4646, supervisor pid "},
		{"orphan child", func(r *rig, insp *osx.FakeInspector) {
			insp.Procs[5151] = osx.ProcInfo{Start: "c", Image: "/opt/ff/ffmpeg"}
			r.insp = insp
			osx.NewRegistry(filepath.Join(r.root, "_state", "children.json")).Add(osx.ChildEntry{Pid: 5151, Start: "c", Image: "/opt/ff/ffmpeg", Role: "ffmpeg:Front"})
		}, nil, "system.processes fail orphan         ffmpeg:Front pid 5151 (/opt/ff/ffmpeg) is alive with no supervisor"},
		{"two supervisors", func(r *rig, insp *osx.FakeInspector) {
			r.hostRunning(insp)
			r.running()
			insp.Procs[6262] = osx.ProcInfo{Start: "x", Image: "/nonexistent/holocron"}
			os.WriteFile(filepath.Join(r.root, "_state", "supervisor.lock"), []byte(`{"pid":6262}`), 0o600)
		}, nil, "system.processes fail processes      two supervisors: pid 6262 holds the instance lock"},
		{"host without supervisor", func(r *rig, insp *osx.FakeInspector) { r.hostRunning(insp) }, nil,
			"system.processes warn processes      host pid 4646 runs without a supervisor"},
		{"stray ffmpeg", func(r *rig, insp *osx.FakeInspector) {
			r.hostRunning(insp)
			r.running()
			insp.Procs[7373] = osx.ProcInfo{Image: "/opt/ff/ffmpeg"}
			insp.Procs[7474] = osx.ProcInfo{Image: "/opt/ff/ffmpeg"} // the web role's export: a child of a known pid
		}, []osx.ProcEntry{{Pid: 7373, PPid: 1, Name: "ffmpeg"}, {Pid: 7474, PPid: 4646, Name: "ffmpeg"}, {Pid: 8, PPid: 1, Name: "bash"}},
			"system.processes fail stray          ffmpeg pid 7373 (parent 1, /opt/ff/ffmpeg) belongs to no registry of this system"},
	} {
		r := newRig(t)
		insp := &osx.FakeInspector{Procs: map[int]osx.ProcInfo{}}
		r.insp = insp
		c.setup(r, insp)
		r.doctorTools(func(d *doctorDeps) {
			d.listProcs = func() ([]osx.ProcEntry, error) { return c.procs, nil }
			d.lookPath = func(p string) (string, error) {
				if p == "ffmpeg" {
					return "/opt/ff/ffmpeg", nil
				}
				return "", os.ErrNotExist
			}
		})
		text := textOf(t, mustRun(r, "--json", "doctor", "--group", "system"))
		if !strings.Contains(text, c.want) {
			t.Errorf("%s: no %q in\n%s", c.name, c.want, text)
		}
		if c.name == "stray ffmpeg" && strings.Contains(text, "7474") {
			t.Errorf("the web role's own ffmpeg is reported:\n%s", text)
		}
	}
}

func TestDoctorAutostartTarget(t *testing.T) {
	skipLinuxInstallOnWindows(t)
	r := newRig(t)
	r.doctorTools(nil)
	if text := textOf(t, mustRun(r, "--json", "doctor", "--group", "system")); !strings.Contains(text, "system.autostart warn autostart      not installed") {
		t.Errorf("%s", text)
	}
	self, _ := os.Executable()
	r.exe = self
	if out, errw, code := r.run("install", "--yes", "--allow-temp"); code != 0 {
		t.Fatalf("install: %s%s", out, errw)
	}
	if text := textOf(t, mustRun(r, "--json", "doctor", "--group", "system")); !strings.Contains(text, "system.autostart ok   autostart      holocron, enabled, runs this executable with this configuration") {
		t.Errorf("%s", text)
	}
	r.exe = filepath.Join(filepath.Dir(self), "moved", "holocron")
	out := mustRun(r, "doctor", "--group", "system")
	if l := lineWith(out, "warn autostart"); !strings.Contains(l, "runs "+self+", but this holocron is "+r.exe) || !strings.Contains(out, "fix: holocron --config "+r.cfg+" install (from "+r.exe+")") {
		t.Errorf("moved binary:\n%s", out)
	}
}

func TestDoctorLogAndEnvironment(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(filepath.Join(r.root, "_logs"), 0o755)
	st := time.Now().Add(-time.Hour).Format("2006-01-02T15:04:05.000000Z07:00")
	os.WriteFile(filepath.Join(r.root, "_logs", "holocron.log"), []byte(
		st+" WARN  [supervisor] exit 1 after 60s for Front; restart #1 in 3s action=camera.restart\n"+
			st+" WARN  [supervisor] exit 1 after 60s for Front; restart #1 in 3s action=camera.restart\n"+
			st+" ERROR [web] bind refused\n"+st+" INFO  [cli] fine\n"), 0o644)
	r.doctorTools(nil)
	text := textOf(t, mustRun(r, "--json", "doctor", "--group", "system"))
	for _, w := range []string{
		"system.log info log            1 ERROR and 2 WARN record(s) in the last 24 h; most frequent: 2x [supervisor] exit 1 after 60s for Front; restart #1 in 3s | 1x [web] bind refused",
		"system.alerts ok   alerts         desktop off, eventlog off, log on, push off, webhook off",
		"system.version info version        holocron dev",
		"system.env warn environment    HOLOCRON_CAMERA_USER and HOLOCRON_CAMERA_PASSWORD set in this window",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("no %q in\n%s", w, text)
		}
	}
	r.noCreds = true
	if text := textOf(t, mustRun(r, "--json", "doctor", "--group", "system")); !strings.Contains(text, "system.env ok   environment") {
		t.Errorf("%s", text)
	}
}

func TestDoctorWarnsOfTheSandboxVariable(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	r.logSpec = "" // the rig's getenv: add the variable for this run
	r.extraEnv = map[string]string{app.SandboxEnv: "1"}
	out := mustRun(r, "doctor", "--group", "system")
	if l := lineWith(out, "warn environment    HOLOCRON_SANDBOX_ONLY is set"); l == "" || !strings.Contains(out, "fix: "+envFix(app.SandboxEnv)) {
		t.Errorf("%s", out)
	}
}

// Incident 22 Oct 2026: HOLOCRON_SANDBOX_ONLY=1 left set in a window refuses the real root. Doctor
// says so with the fix and touches nothing of that root.
func TestDoctorIncidentSandboxVariableRefusesTheRoot(t *testing.T) {
	r := newRig(t)
	r.root = filepath.Join(filepath.Dir(r.root), "cameras") // looks like an Operator archive (E:\cameras)
	r.doctorTools(nil)
	out, _, code := r.run("doctor")
	want := "fail sandbox guard  refused by the sandbox guard (HOLOCRON_SANDBOX_ONLY=1)"
	if code != 1 || !strings.HasPrefix(out, "== setup ==\n"+want) || !strings.Contains(out, "     fix: "+envFix(app.SandboxEnv)+" (it is for tests; then run holocron doctor again)") || !strings.HasSuffix(out, "BROKEN (1 failure(s))\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(r.root); !os.IsNotExist(err) {
		t.Errorf("doctor touched the refused root: %v", err)
	}
	t.Logf("incident fixture (sandbox variable):\n%s", out)
	if js := mustRun(r, "--json", "doctor"); !strings.Contains(js, `"id": "setup.sandbox"`) || !strings.Contains(js, `"verdict": "BROKEN"`) {
		t.Errorf("%s", js)
	}
}
