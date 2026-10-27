package prune

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"holocron/internal/archive"
	"holocron/internal/archive/archivetest"
	"holocron/internal/osx"
)

// TestCollectStaysOnTheSpoolUntilL3 pins the pruner to the flat view (task L2; task L3 teaches it
// the day folders): on a spool-and-day tree Collect returns exactly what it returns once every day
// folder is gone. It never hands the planner a filed file, and its Newest is the spool's, so a
// camera whose spool is empty keeps an empty Newest instead of one the planner cannot list.
func TestCollectStaysOnTheSpoolUntilL3(t *testing.T) {
	trees := append(archivetest.Mixed(20261101, 80), archivetest.Filed("filed-empty-spool", 3, 5, 8, 0, archivetest.Mixed(1, 1)[0].Now))
	compared, emptySpool := 0, 0
	for _, tr := range trees {
		root, _ := tr.Build(t, t.TempDir())
		if err := os.MkdirAll(filepath.Join(root, "_state"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{ "Owner": %q, "Schema": 1, "RootPath": %q }`, archive.MarkerOwner, root)
		if err := os.WriteFile(archive.MarkerPath(root), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		p := freePlatform{osx.Current(), &scaledFS{OSFS: OSFS{Platform: osx.Current()}, Scale: 1}}
		p.fs.Reset(100)
		r := Rules{Container: tr.Container, SegmentSeconds: float64(tr.SegmentSeconds), StallFactor: 2.5}
		mixed, err := Collect(root, p, r, tr.Now, false)
		if err != nil || mixed.MarkerProblem != "" || mixed.RootProblem != "" {
			t.Fatalf("%s: %v %q %q", tr.Name, err, mixed.MarkerProblem, mixed.RootProblem)
		}
		// remove every day folder: what is left is the flat part
		days := 0
		for _, c := range archivetest.Walk(t, root, tr.Container, false).Cameras {
			for _, d := range c.Days {
				if err := os.RemoveAll(filepath.Join(c.Dir, d)); err != nil {
					t.Fatal(err)
				}
				days++
			}
			if len(c.Footage) > 0 && c.Footage[0].Day != "" && len(c.Spool) == 0 {
				emptySpool++
			}
		}
		flat, err := Collect(root, p, r, tr.Now, false)
		if err != nil || !reflect.DeepEqual(mixed, flat) {
			t.Fatalf("%s (%d day folders): Collect\n mixed %+v\n flat  %+v", tr.Name, days, mixed, flat)
		}
		compared++
	}
	if emptySpool == 0 {
		t.Error("no camera with an empty spool and filed footage: the Newest pin is not exercised")
	}
	t.Logf("%d trees: Collect identical with and without the day folders (%d cameras with an empty spool)", compared, emptySpool)
}
