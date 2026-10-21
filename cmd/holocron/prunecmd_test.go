package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/osx"
)

// seedArchive writes a sandbox archive: Front holds recent segments plus `old` expired ones (3 days
// back); Back one recent segment. The newest segment of each folder is the one being written.
func (r *rig) seedArchive(recent, old int) (oldFiles []string) {
	r.t.Helper()
	now := time.Now()
	write := func(folder string, start time.Time, size int) string {
		dir := filepath.Join(r.root, folder)
		os.MkdirAll(dir, 0o755)
		path := filepath.Join(dir, archive.FormatSegmentName(folder, start, ".mkv"))
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			r.t.Fatal(err)
		}
		os.Truncate(path, int64(size)) // sparse: sizes are what the statistics read
		if err := setCreated(path, start); err != nil {
			r.t.Fatal(err)
		}
		end := start.Add(10 * time.Minute)
		if end.After(now) {
			end = now
		}
		os.Chtimes(path, end, end)
		return path
	}
	for i := 0; i < recent; i++ {
		write("Front", now.Add(-time.Duration(recent-i)*15*time.Minute), 100<<20)
	}
	for i := 0; i < old; i++ {
		oldFiles = append(oldFiles, write("Front", now.Add(-72*time.Hour-time.Duration(i)*15*time.Minute), 200<<20))
	}
	write("Back", now.Add(-5*time.Minute), 50<<20)
	return oldFiles
}

func (r *rig) claim() {
	r.t.Helper()
	os.MkdirAll(filepath.Join(r.root, "_state"), 0o755)
	if res, err := archive.ClaimRoot(r.root, osx.Current(), ".mkv", time.Now()); err != nil || res.Problem != "" {
		r.t.Fatalf("claim: %v %+v", err, res)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestPruneDryRunIsTheDefaultAndDeletesNothing(t *testing.T) {
	r := newRig(t)
	r.noCtl = true
	old := r.seedArchive(10, 2)
	r.claim()
	if _, _, code := r.run("config", "set", "MaxRetentionDays", "1"); code != 0 {
		t.Fatal(code)
	}
	for _, args := range [][]string{{"prune"}, {"prune", "--dry-run"}} {
		out, errw, code := r.run(args...)
		if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, "would delete 2 file(s)") || !strings.Contains(out, old[0]) {
			t.Fatalf("%v: %d %q %q", args, code, out, errw)
		}
	}
	for _, f := range old {
		if !exists(f) {
			t.Fatalf("a dry run deleted %s", f)
		}
	}
	out, _, code := r.run("--json", "prune")
	var rep pruneReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 || !rep.DryRun || len(rep.Deletions) != 2 || rep.Deletions[0].Reason != "age" || rep.Refused != "" {
		t.Fatalf("--json: %v %d %q", err, code, out)
	}
	if strings.Contains(r.log(), "action=prune.delete") {
		t.Error("a dry run logged a deletion")
	}
}

func TestPruneNowNeedsYesAndRefusesWithASupervisor(t *testing.T) {
	r := newRig(t)
	old := r.seedArchive(10, 2)
	r.claim()
	r.run("config", "set", "MaxRetentionDays", "1")

	if _, errw, code := r.run("prune", "--now"); code != 2 || !strings.Contains(errw, "--yes") {
		t.Errorf("--now without --yes: %d %q", code, errw)
	}
	if _, _, code := r.run("prune", "--now", "--dry-run", "--yes"); code != 2 {
		t.Errorf("--now with --dry-run: %d", code)
	}
	// the fake control endpoint answers: a supervisor is running
	_, errw, code := r.run("prune", "--now", "--yes")
	if code != 1 || !strings.Contains(errw, "prunes on its own schedule") {
		t.Errorf("with a supervisor: %d %q", code, errw)
	}
	for _, f := range old {
		if !exists(f) {
			t.Fatalf("%s was deleted although a supervisor runs", f)
		}
	}
}

func TestPruneNowDeletesOnlyWhatTheGatesAllow(t *testing.T) {
	r := newRig(t)
	r.noCtl = true
	r.logSpec = "info"
	old := r.seedArchive(10, 2)
	r.run("config", "set", "MaxRetentionDays", "1")

	// not claimed: the ownership marker gate refuses everything
	out, errw, code := r.run("prune", "--now", "--yes")
	if code != 1 || !strings.Contains(out, "REFUSED at the marker gate") || !strings.Contains(out, "ownership marker") {
		t.Errorf("unclaimed: %d %q %q", code, out, errw)
	}
	out, _, _ = r.run("prune")
	if !strings.Contains(out, "not claimed yet") {
		t.Errorf("the dry run must say the root is unclaimed: %q", out)
	}
	for _, f := range old {
		if !exists(f) {
			t.Fatalf("an unclaimed root lost %s", f)
		}
	}

	// claimed, but outside the pruner's sandbox: refused before anything is read
	r.claim()
	t.Setenv("SANDBOX_ONLY", filepath.Join(r.base(), "elsewhere"))
	if _, errw, code := r.run("prune", "--now", "--yes"); code != 1 || !strings.Contains(errw, "outside the sandbox") {
		t.Errorf("outside the sandbox: %d %q", code, errw)
	}
	t.Setenv("SANDBOX_ONLY", "1")
	for _, f := range old {
		if !exists(f) {
			t.Fatalf("the sandbox gate let %s go", f)
		}
	}

	out, errw, code = r.run("prune", "--now", "--yes")
	if code != 0 || !strings.Contains(out, "deleted 2 file(s)") {
		t.Fatalf("now: %d %q %q", code, out, errw)
	}
	for _, f := range old {
		if exists(f) {
			t.Errorf("%s survived", f)
		}
	}
	// the newest segments are untouched
	entries, _ := os.ReadDir(filepath.Join(r.root, "Front"))
	if len(entries) != 10 {
		t.Errorf("Front holds %d files, want the 10 recent ones", len(entries))
	}
	if n := strings.Count(r.log(), "action=prune.delete"); n != 2 {
		t.Errorf("%d prune.delete records, want 2:\n%s", n, r.log())
	}
	for _, l := range strings.Split(r.log(), "\n") {
		if strings.Contains(l, "action=prune.delete") && (!strings.Contains(l, " INFO ") || !strings.Contains(l, "[prune]")) {
			t.Errorf("a deletion must be an INFO record of the prune component: %s", l)
		}
	}
}

func TestPruneRefusesAForeignRootAndAFolderOfOtherFiles(t *testing.T) {
	r := newRig(t)
	r.noCtl = true
	// a root holding a PowerShell recorder file is not ours
	os.MkdirAll(r.root, 0o755)
	os.WriteFile(filepath.Join(r.root, "cam.ps1"), []byte("x"), 0o644)
	if _, errw, code := r.run("prune"); code == 0 {
		t.Errorf("exit 0 for a foreign root: %q", errw)
	}
}

func TestArchiveStats(t *testing.T) {
	r := newRig(t)
	r.seedArchive(10, 2)
	os.MkdirAll(filepath.Join(r.root, "_logs"), 0o755)
	os.WriteFile(filepath.Join(r.root, "Front", "notes.txt"), []byte("not a segment"), 0o644)

	out, errw, code := r.run("archive", "stats")
	if code != 0 || !strings.Contains(out, "Front") || !strings.Contains(out, "Back") || strings.Contains(out, "_logs") || !strings.Contains(out, "GB/day") {
		t.Fatalf("%d %q %q", code, out, errw)
	}
	out, _, code = r.run("--json", "archive", "stats")
	var st archiveStats
	if err := json.Unmarshal([]byte(out), &st); err != nil || code != 0 {
		t.Fatalf("--json: %v %q", err, out)
	}
	byFolder := map[string]camStats{}
	for _, c := range st.Cameras {
		byFolder[c.Folder] = c
	}
	front := byFolder["Front"]
	if front.Segments != 12 || front.Bytes != 10*(100<<20)+2*(200<<20) || front.Camera != "Front" || front.Oldest == nil || !front.Oldest.Before(*front.Newest) {
		t.Errorf("Front %+v", front)
	}
	if byFolder["Back"].Segments != 1 || st.Segments != 13 || st.GBPerDay == nil || st.ProjectedDays == nil || st.FreeGB <= 0 {
		t.Errorf("stats %+v", st)
	}

	// no history: no rate, a note, still valid
	r2 := newRig(t)
	os.MkdirAll(r2.root, 0o755)
	out, _, code = r2.run("--json", "archive", "stats")
	var empty archiveStats
	if err := json.Unmarshal([]byte(out), &empty); err != nil || code != 0 || empty.GBPerDay != nil || empty.Note == "" || empty.Cameras == nil {
		t.Errorf("empty archive: %v %d %q", err, code, out)
	}
	if _, _, code := r2.run("archive", "stats", "extra"); code != 2 {
		t.Errorf("extra argument: %d", code)
	}
}
