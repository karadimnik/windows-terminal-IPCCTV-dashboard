package supervise

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/status"
)

func rowsJSON(t *testing.T, rows []status.Camera) []map[string]any {
	t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func keys(m map[string]any) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// IS 1.4: the 20 fields, their derivation for a running, a backing-off, a non-recording, an
// unusable-name and a colliding camera.
func TestRowsFields(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.d.Location = time.FixedZone("EEST", 3*3600)
	r.m.Reconcile([]cameraT{cam("Run", true), cam("Wait", true), cam("Off", false), cam("CON", true), cam("run", true)})
	r.scan.scans[r.dir("Run")] = archive.DirScan{Files: 12, Bytes: 1200, NewestClosedWrite: t0.Add(-312400 * time.Millisecond).Add(5 * time.Second),
		Newest: "Run_20261017_083001+0000.mkv", NewestBytes: 500, Recent: true}
	acts := r.m.Plan(RelayView{Ready: fakeReady{"run": true}})
	r.startAll(acts)
	r.clk.Advance(5 * time.Second)
	w := r.m.Entry("Wait")
	r.exit(w, 1)
	w.downSince = t0
	r.m.Refresh()
	rows := r.m.Rows(map[string]archive.DirScan{"Off": {Files: 4, Bytes: 400, NewestClosedWrite: t0.Add(-72 * time.Hour)}})
	if len(rows) != 5 {
		t.Fatalf("rows %d", len(rows))
	}
	run, wait, off, con, dup := rows[0], rows[1], rows[2], rows[3], rows[4]
	if run.Pid == nil || !run.Alive || run.LastPid == nil || *run.Pid != *run.LastPid || run.InputMode != "relay" ||
		run.CurrentSegment == nil || *run.CurrentSegmentBytes != 500 || *run.SecondsSinceLastClosedSegment != 312.4 ||
		*run.ArchiveFiles != 12 || *run.ArchiveBytes != 1200 || !run.NextStartAt.IsNull() || run.StartedAt.IsNull() {
		t.Errorf("running row %+v", run)
	}
	if b, _ := json.Marshal(run.StartedAt); string(b) != `"2026-10-17T11:30:01.0000000+03:00"` {
		t.Errorf("StartedAt %s", b)
	}
	if wait.Pid != nil || wait.Alive || wait.LastPid == nil || wait.NextStartAt.IsNull() || wait.RestartsTotal != 1 ||
		wait.RestartsLastHour != 1 || wait.LastError != "ffmpeg exited with code 1 after 5s" || wait.LastErrorAt.IsNull() ||
		wait.DownSince.IsNull() || wait.InputMode != "direct" {
		t.Errorf("backing-off row %+v", wait)
	}
	if off.Record || off.Pid != nil || off.LastPid != nil || off.InputMode != "" || off.StartedAt.IsNull() == false ||
		*off.ArchiveFiles != 4 || *off.SecondsSinceLastClosedSegment != 259205 {
		t.Errorf("non-recording row %+v", off)
	}
	if con.SafeName != "" || con.ConfigError == "" || con.Pid != nil {
		t.Errorf("unusable name row %+v", con)
	}
	if dup.SafeName != "run" || dup.ConfigError == "" || dup.Pid != nil || dup.LastPid != nil {
		t.Errorf("colliding row %+v", dup)
	}
}

// The rows have exactly the fields of the golden status documents, with the same JSON types
// wherever both are non-null (testdata/golden/status, read only).
func TestRowsGoldenShape(t *testing.T) {
	r := newRig(t, defaultSettings())
	r.m.Reconcile([]cameraT{cam("Run", true), cam("Wait", true), cam("Off", false)})
	r.scan.scans[r.dir("Run")] = archive.DirScan{Files: 2, Bytes: 9, NewestClosedWrite: t0, Newest: "x.mkv", NewestBytes: 4, Recent: true}
	r.startAll(r.m.Plan(RelayView{}))
	r.clk.Advance(time.Second)
	r.m.StartFailed(r.m.Entry("Wait"), errors.New("x"))
	r.m.Refresh()
	mine := rowsJSON(t, r.m.Rows(nil))

	files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "status", "*.json"))
	checked := 0
	for _, f := range files {
		raw, err := status.ReadRaw(f)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct{ Cameras []map[string]any }
		if json.Unmarshal(raw, &doc) != nil || len(doc.Cameras) == 0 {
			continue
		}
		for _, g := range doc.Cameras {
			if len(g) != 20 {
				continue // a minimal/negative fixture
			}
			for _, row := range mine {
				if !reflect.DeepEqual(keys(row), keys(g)) {
					t.Fatalf("%s: keys %v, golden %v", filepath.Base(f), keys(row), keys(g))
				}
				for k, gv := range g {
					if gv != nil && row[k] != nil && reflect.TypeOf(gv) != reflect.TypeOf(row[k]) {
						t.Errorf("%s %s: type %T, golden %T", filepath.Base(f), k, row[k], gv)
					}
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no golden camera rows found")
	}
}
