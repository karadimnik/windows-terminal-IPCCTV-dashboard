package supervise

import (
	"strings"
	"testing"
)

// Invoke-CamReconcile (cam.recorder.ps1:3631-3707), IR 3.4.
func TestReconcileRecordOnly(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile([]cameraT{cam("Front Door", true), cam("Back", false), cam("CON", true)})
	if e := r.m.Entry("Front_Door"); e == nil || e.Dir() != r.dir("Front_Door") || e.RelayPath() != "frontdoor" {
		t.Fatalf("Front Door entry = %+v", e)
	}
	if r.m.Entry("Back") != nil {
		t.Fatal("Record false must have no child")
	}
	if len(r.m.entries) != 1 {
		t.Fatalf("entries = %d, want 1 (CON is a reserved device name)", len(r.m.entries))
	}
	if r.m.configErrs[2] == "" {
		t.Fatal("CON must carry a ConfigError")
	}
	if strings.Contains(r.logs.String(), "now set to record") {
		t.Fatal("the first reconcile must not log enables (R:3700)")
	}
}

// IR V5: a second camera with the same folder is refused with a ConfigError, the first keeps it.
func TestReconcileCollisionRefused(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile([]cameraT{cam("Gate", true), cam("gate", true)})
	if r.m.Entry("Gate") == nil || r.m.Entry("gate") != nil {
		t.Fatalf("entries = %v", r.m.entries)
	}
	if !strings.Contains(r.m.configErrs[1], "already used by camera 'Gate'") || r.m.configErrs[0] != "" {
		t.Fatalf("configErrs = %q", r.m.configErrs)
	}
}

// IR V12 (kept): an IP or Quality change recreates the entry and loses its counters; a Name-only
// change of a field that is not compared keeps it.
func TestReconcileChangeRecreates(t *testing.T) {
	r := newRig(t, defaultSettings())
	c := cam("Yard", true)
	r.m.Reconcile([]cameraT{c})
	old := r.m.Entry("Yard")
	old.restartCount = 4
	r.m.Reconcile([]cameraT{c})
	if r.m.Entry("Yard") != old {
		t.Fatal("an unchanged camera must keep its entry")
	}
	c.IP = "192.0.2.99"
	r.m.Reconcile([]cameraT{c})
	e := r.m.Entry("Yard")
	if e == old || e.restartCount != 0 {
		t.Fatal("an IP change must recreate the entry with zero counters")
	}
	if len(r.m.retiring) != 0 {
		t.Fatal("an entry without a child is dropped, not retired")
	}
	c.Quality = "stream2"
	r.m.Reconcile([]cameraT{c})
	if r.m.Entry("Yard") == e {
		t.Fatal("a Quality change must recreate the entry")
	}
	if !strings.Contains(r.logs.String(), "reconcile: Yard address or stream changed, restarting it") {
		t.Fatal("missing change log line")
	}
}

func TestReconcileDisableDrops(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile([]cameraT{cam("Yard", true)})
	r.m.Reconcile([]cameraT{cam("Yard", false)})
	if r.m.Entry("Yard") != nil {
		t.Fatal("disabled camera kept its entry")
	}
	r.m.Reconcile([]cameraT{cam("Yard", true)})
	if !strings.Contains(r.logs.String(), "reconcile: Yard is now set to record, starting it") ||
		!strings.Contains(r.logs.String(), "reconcile: Yard is no longer set to record, stopped it") {
		t.Fatalf("log lines missing:\n%s", r.logs.String())
	}
}
