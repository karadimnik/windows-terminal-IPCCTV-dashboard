package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/ffmpeg"
)

func TestDoctorJSONIsOneDocumentWithTheSchemaFields(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	out, _, code := r.run("doctor", "--json")
	var rep struct {
		Schema int      `json:"schema"`
		Root   string   `json:"root"`
		Groups []string `json:"groups"`
		Checks []struct {
			ID, Group, Level, Title, Detail, Fix string
			Data                                 any
		} `json:"checks"`
		Summary struct {
			Verdict                      string
			Running                      bool
			OK, Info, Warnings, Failures int
			ElapsedMs                    int64
		} `json:"summary"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatalf("more than one document:\n%s", out)
	}
	if rep.Schema != 1 || rep.Root != r.root || strings.Join(rep.Groups, ",") != strings.Join(doctorGroups, ",") || len(rep.Checks) == 0 {
		t.Fatalf("report: %+v", rep)
	}
	n := 0
	for _, c := range rep.Checks {
		if c.ID == "" || !contains(doctorGroups, c.Group) || !contains([]string{lvOK, lvInfo, lvWarn, lvFail}, c.Level) || c.Title == "" {
			t.Errorf("check %+v", c)
		}
		if (c.Level == lvWarn || c.Level == lvFail) && c.Fix == "" {
			t.Errorf("a %s without a fix: %+v", c.Level, c)
		}
		n++
	}
	s := rep.Summary
	if s.OK+s.Info+s.Warnings+s.Failures != n || s.Verdict == "" || (code == 0) != (s.Failures == 0) {
		t.Errorf("summary %+v for %d checks, exit %d", s, n, code)
	}
}

func TestDoctorExitCodeFollowsFailuresNotWarnings(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	out, _, code := r.run("doctor", "--group", "setup")
	if code != 0 || strings.Contains(out, "\nfail ") || !strings.Contains(out, "warn ") {
		t.Fatalf("warnings only must exit 0 (exit %d):\n%s", code, out)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); !strings.HasPrefix(lines[len(lines)-1], "DEGRADED (") {
		t.Errorf("summary:\n%s", out)
	}
	r.noCreds = true
	out, _, code = r.run("doctor", "--group", "setup")
	if code != 1 || !strings.Contains(out, "fail credentials") || !strings.Contains(out, "fix: holocron secret set camera.user") || !strings.Contains(out, "BROKEN (1 failure(s)") {
		t.Errorf("a failure must exit 1 with its fix (exit %d):\n%s", code, out)
	}
}

func TestDoctorQuietPrintsOnlyProblemsAndTheSummary(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	out, _, _ := r.run("doctor", "--group", "setup", "--quiet")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range lines[:len(lines)-1] {
		if !strings.HasPrefix(l, "warn ") && !strings.HasPrefix(l, "fail ") && !strings.HasPrefix(l, "     fix: ") {
			t.Errorf("--quiet printed %q", l)
		}
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "DEGRADED") {
		t.Errorf("last line %q", last)
	}
}

func TestDoctorGroupLimitsAndRejectsUnknownGroups(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	out, _, _ := r.run("--json", "doctor", "--group", "setup")
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Checks {
		if c.Group != "setup" {
			t.Errorf("--group setup ran %+v", c)
		}
	}
	if _, errw, code := r.run("doctor", "--group", "nope"); code != exitUsage || !strings.Contains(errw, "--group takes setup, recording") {
		t.Errorf("unknown group: exit %d %q", code, errw)
	}
}

// --setup-only is the doctor of before: the same lines, no headings, no fixes, no summary.
func TestDoctorSetupOnlyIsTheOldOutput(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	r.noCreds = true
	out, _, code := r.run("doctor", "--setup-only")
	if code != 1 || strings.Contains(out, "==") || strings.Contains(out, "fix:") || strings.Contains(out, "BROKEN") ||
		!strings.HasPrefix(out, "ok   configuration") || !strings.HasSuffix(out, "\n1 check(s) failed\n") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// A check that never returns is a warning after its timeout; concurrent ones stop at the budget.
func TestDoctorChecksCannotHangIt(t *testing.T) {
	block := func(ctx context.Context, r *drun) []check { select {} }
	saved := doctorTable
	t.Cleanup(func() { doctorTable = saved })
	doctorTable = []doctorCheck{{id: "setup.block", group: "setup", timeout: 50 * time.Millisecond, run: block}}
	for i := 0; i < 9; i++ {
		doctorTable = append(doctorTable, doctorCheck{id: "setup.net", group: "setup", concurrent: true, run: block})
	}
	r := newRig(t)
	r.doctorTools(nil)
	r.doctorTools(func(d *doctorDeps) { d.checkTimeout, d.netBudget = 150*time.Millisecond, 300*time.Millisecond })
	start := time.Now()
	out, _, code := r.run("doctor")
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %s", el)
	}
	if code != 0 || strings.Count(out, "not checked: no answer within") != 10 || !strings.Contains(out, "DEGRADED (10 warning(s))") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// Every check is logged at DEBUG with its id and result.
func TestDoctorLogsEachCheckAtDebug(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	r.logSpec = "debug"
	r.run("doctor", "--group", "setup")
	if log := r.log(); !strings.Contains(log, "doctor check setup.configuration: ok") || !strings.Contains(log, "doctor.run") {
		t.Errorf("log:\n%s", log)
	}
}

// snapshot is every file under root with its size and content, except the log files.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == filepath.Join("_logs", "holocron.log") || rel == filepath.Join("_logs", "audit.log") {
			return nil
		}
		if d.IsDir() {
			m[rel] = "dir"
			return nil
		}
		b, _ := os.ReadFile(p)
		info, _ := d.Info()
		m[rel] = string(b) + "|" + info.ModTime().String()
		return nil
	})
	return m
}

// Doctor is read-only: apart from its log records and the probe files it removes again, the root
// is the same file for file (content and modification time) after a run of every group.
func TestDoctorWritesNothingButItsProbeFiles(t *testing.T) {
	r := newRig(t)
	r.doctorTools(nil)
	r.running()
	os.MkdirAll(filepath.Join(r.root, "_logs"), 0o755)
	os.MkdirAll(filepath.Join(r.root, "Front"), 0o755)
	os.WriteFile(filepath.Join(r.root, "Front", "Front_20261022_120000+0300.mkv"), []byte("x"), 0o644)
	before := snapshot(t, r.root)
	r.run("doctor")
	after := snapshot(t, r.root)
	for k, v := range after {
		if before[k] != v {
			t.Errorf("doctor changed or created %s", k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			t.Errorf("doctor removed %s", k)
		}
	}
}

// tools makes ffmpeg and MediaMTX present whatever this machine has.
func (r *rig) doctorTools(more func(*doctorDeps)) {
	r.doctor = func(d *doctorDeps) {
		d.ffmpeg = func(string) (ffmpeg.Version, error) { return ffmpeg.ParseVersion("ffmpeg version 7.1 Copyright") }
		d.mediamtx = func(string) check { return mk(lvOK, "mediamtx", "v1.21.2") }
		d.sleep = func(context.Context, time.Duration) error { return nil }
		if more != nil {
			more(d)
		}
	}
}
