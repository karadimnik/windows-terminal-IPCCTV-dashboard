package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/osx"
	"holocron/internal/status"
)

// freePlatform reports free bytes for every path.
type freePlatform struct {
	osx.Platform
	free uint64
}

func (p freePlatform) DiskFree(string) (uint64, error) { return p.free, nil }

func TestDoctorStorage(t *testing.T) {
	now := time.Now()
	recent, old := status.NewTime(now.Add(-time.Minute)), status.NewTime(now.Add(-time.Hour))
	for _, c := range []struct {
		name   string
		freeGB float64
		prune  status.Prune
		claim  bool
		want   []string
	}{
		{"at the reserve edge, pruner working", 19.9, status.Prune{LastRunAt: recent, DeletedTotal: 12, FreedGBTotal: 3}, true,
			[]string{"storage.disk ok   disk           free 19.90 GB now, reserve 20 GB", "(below the reserve; the pruner is freeing space)", "storage.prune ok   pruner         last run", "storage.marker ok"}},
		{"below the reserve, pruner silent", 10, status.Prune{LastRunAt: old}, true,
			[]string{"storage.disk warn disk", "BELOW the reserve and the pruner has not run for two intervals", "storage.prune warn pruner         has not run for two intervals"}},
		{"circuit breaker", 100, status.Prune{LastRunAt: recent, LastMessage: "age rule HELD by the circuit breaker, nothing deleted by age: 80%"}, true,
			[]string{"storage.prune warn pruner         the age rule is held by the circuit breaker (alert 122)"}},
		{"no marker", 100, status.Prune{LastRunAt: recent}, false, []string{"storage.marker fail marker         the ownership marker is not valid"}},
	} {
		r := newRig(t)
		r.runningWith(func(d *status.Document) { p := c.prune; d.Prune = &p })
		if c.claim {
			if _, err := archive.ClaimRoot(r.root, osx.Current(), ".mkv", now); err != nil {
				t.Fatal(err)
			}
		}
		r.doctorTools(func(d *doctorDeps) { d.platform = freePlatform{d.platform, uint64(c.freeGB * gb)} })
		text := textOf(t, mustRun(r, "--json", "doctor", "--group", "storage"))
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, text)
			}
		}
	}
}

// Incident 22 Oct 2026: the disk at the reserve edge with the pruner active is the design; the
// same disk with pruning blocked is a failure with its fix.
func TestDoctorIncidentFreeSpaceAtTheReserveWithPruningBlocked(t *testing.T) {
	r := newRig(t)
	r.runningWith(func(d *status.Document) {
		d.Disk = &status.Disk{FreeGB: 20, ReserveGB: 20}
		d.Prune = &status.Prune{LastRunAt: status.NewTime(time.Now().Add(-time.Minute)), Blocked: true, LastMessage: "PRUNING BLOCKED: below the reserve with nothing deletable left"}
	})
	archive.ClaimRoot(r.root, osx.Current(), ".mkv", time.Now())
	r.doctorTools(func(d *doctorDeps) { d.platform = freePlatform{d.platform, 20 * gb} })
	out, _, code := r.run("doctor", "--group", "storage", "--quiet")
	want := "fail pruner         pruning is BLOCKED (alert 120 or 121): last run "
	wantFix := "     fix: holocron prune --dry-run (it names the reason) ; free space on the disk or lower the reserve: holocron config set ReserveGB <GB>"
	if code != 1 || !strings.HasPrefix(out, want) || !strings.Contains(out, wantFix) || strings.Contains(out, "warn disk") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	t.Logf("incident fixture (reserve edge, pruning blocked):\n%s", out)
}

func TestDoctorCameraWithFarFewerSegments(t *testing.T) {
	r := newRig(t)
	cams := `[{"Name":"Front","IP":"192.0.2.10","Record":true},{"Name":"Back","IP":"192.0.2.11","Record":true},{"Name":"Side","IP":"192.0.2.12","Record":true}]`
	if err := os.WriteFile(r.cameras(), []byte(cams), 0o644); err != nil {
		t.Fatal(err)
	}
	r.running()
	start := time.Now().Add(-10 * time.Hour).Truncate(time.Minute)
	for i := 0; i < 10; i++ {
		s := start.Add(time.Duration(i) * time.Hour)
		r.segment("Front", s, s.Add(time.Hour), 100)
		r.segment("Side", s, s.Add(time.Hour), 100)
		if i < 3 {
			r.segment("Back", s, s.Add(time.Hour), 100)
		}
	}
	r.doctorTools(nil)
	text := textOf(t, mustRun(r, "--json", "doctor", "--group", "storage"))
	if !strings.Contains(text, "storage.cameras warn Back           3 segment(s), 0.00 GB in the last 24 h, against a median of 10") || !strings.Contains(text, "storage.cameras ok   Front") {
		t.Errorf("%s", text)
	}
}
