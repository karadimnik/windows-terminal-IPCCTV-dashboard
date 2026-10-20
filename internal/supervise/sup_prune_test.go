package supervise

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
)

func writeSeg(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("segment bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// The production pruner wiring on a root that fails the gate (foreign content, no marker): the claim
// is refused, nothing is deleted although the disk is under the reserve, alert 120 once over three
// runs, and the Prune section says so.
func TestSupervisorPrunerRefusedRoot(t *testing.T) {
	r := newSRig(t, nil, func(c *config.Config) { c.PruneIntervalSeconds = 20 })
	r.deps.Pruner = nil // the default: ArchivePruner over RootPath
	r.plat.free.Store(1 << 30)
	bait := filepath.Join(r.root, "notes.txt")
	os.WriteFile(bait, []byte("not the recorder's"), 0o644)
	old := filepath.Join(r.root, "Front", "Front_20261019_080000+0000.mkv")
	writeSeg(t, old, tStart.Add(-25*time.Hour))
	r.start()
	r.step(45 * time.Second)
	if n := r.al.count(120); n != 1 {
		t.Fatalf("120 alerts over three runs: %d (%v)\n%s", n, r.al.ids(), r.logText())
	}
	if !exists(bait) || !exists(old) || exists(archive.MarkerPath(r.root)) {
		t.Fatalf("the refused root was changed: bait %v old %v marker %v", exists(bait), exists(old), exists(archive.MarkerPath(r.root)))
	}
	d := r.docNow()
	if !d.Prune.Blocked || !strings.HasPrefix(d.Prune.LastMessage, "PRUNING DISABLED") || d.Prune.LastRunAt.IsNull() || d.Prune.DeletedTotal != 0 {
		t.Errorf("prune section %+v", d.Prune)
	}
	if !strings.Contains(r.logText(), "did not claim") {
		t.Error("no claim refusal line")
	}
	r.stop(StopRequested)
	r.validateAll()
}

// On a clean root the marker is claimed at start and the watermark deletes the oldest segments,
// never the newest.
func TestSupervisorPrunerClaimsAndDeletes(t *testing.T) {
	r := newSRig(t, nil, func(c *config.Config) { c.PruneIntervalSeconds = 20 })
	r.deps.Pruner = nil
	r.plat.free.Store(1 << 30) // 1 GB, under the 20 GB reserve
	r.start()
	if !exists(archive.MarkerPath(r.root)) {
		t.Fatalf("the clean root was not claimed\n%s", r.logText())
	}
	dir := filepath.Join(r.root, "Front")
	var olds []string
	for i, hm := range []string{"070000", "071500", "073000"} {
		p := filepath.Join(dir, "Front_20261020_"+hm+"+0000.mkv")
		writeSeg(t, p, tStart.Add(-2*time.Hour+time.Duration(i)*15*time.Minute))
		olds = append(olds, p)
	}
	newest := filepath.Join(dir, "Front_20261020_085900+0000.mkv")
	writeSeg(t, newest, tStart.Add(-10*time.Second))
	r.step(21 * time.Second)
	for _, p := range olds {
		if exists(p) {
			t.Errorf("%s was not pruned\n%s", filepath.Base(p), r.logText())
		}
	}
	if !exists(newest) {
		t.Fatal("the newest segment was deleted")
	}
	d := r.docNow()
	if d.Prune.DeletedTotal != 3 || !d.Prune.Blocked || r.al.count(121) != 1 {
		t.Errorf("prune %+v alerts %v", d.Prune, r.al.ids())
	}
	r.stop(StopRequested)
	r.validateAll()
}

// A root outside the sandbox is refused by the supervisor's own pruner wiring: no claim, no run,
// nothing created there.
func TestSupervisorPrunerRefusesOutsideSandbox(t *testing.T) {
	outside := "/nonexistent-holocron-w7b/archive"
	if os.PathSeparator == '\\' {
		outside = `Z:\nonexistent-holocron-w7b\archive`
	}
	r := newSRig(t, nil, func(c *config.Config) { c.PruneIntervalSeconds = 20 })
	r.cfg.RootPath = outside
	r.src.cfg.RootPath = outside
	r.deps.StatusPath = filepath.Join(r.root, "_state", "status.json")
	r.deps.Pruner = nil
	r.start()
	r.step(25 * time.Second)
	log := r.logText()
	if !strings.Contains(log, "did not claim "+outside) || !strings.Contains(log, "outside the sandbox") || !strings.Contains(log, "prune failed:") {
		t.Fatalf("no refusal:\n%s", log)
	}
	if exists(outside) || exists(filepath.Dir(outside)) {
		t.Fatal("something was created outside the sandbox")
	}
	r.stop(StopRequested)
	r.validateAll()
}
