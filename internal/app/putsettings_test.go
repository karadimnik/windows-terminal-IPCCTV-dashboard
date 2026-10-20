package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/cli"
)

const camsFile = "\xef\xbb\xbf[\r\n    {\"Name\": \"Front\", \"IP\": \"192.0.2.10\", \"Record\": true, \"Note\": \"kept\"},\r\n    {\"Name\": \"Back\", \"IP\": \"192.0.2.11\", \"Quality\": \"stream2\"}\r\n]\r\n"

func camsPath(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "cameras.json")
	if err := os.WriteFile(p, []byte(camsFile), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPutCamerasNoChangeWritesNothing(t *testing.T) {
	p := camsPath(t)
	n, err := PutCameras(p, []CameraRow{
		{OriginalName: "Front", Name: "Front", IP: "192.0.2.10", Record: true},
		{OriginalName: "Back", Name: "Back", IP: "192.0.2.11", Quality: "stream2"},
	})
	if err != nil || n != 0 || read(t, p) != camsFile {
		t.Fatalf("%d %v\n%s", n, err, read(t, p))
	}
}

func TestPutCamerasEditKeepsBOMAndUnknownFields(t *testing.T) {
	p := camsPath(t)
	n, err := PutCameras(p, []CameraRow{
		{OriginalName: "Back", Name: "Back door", IP: "cam.lan:8554", Quality: "stream1", Record: true},
		{OriginalName: "Front", Name: "Front", IP: "192.0.2.10", Record: true},
		{Name: "Garage", IP: "192.0.2.12"},
	})
	if err != nil || n != 2 { // Back edited, Garage added; the reordering is covered by those
		t.Fatalf("%d %v", n, err)
	}
	got := read(t, p)
	if !strings.HasPrefix(got, "\xef\xbb\xbf") || !strings.Contains(got, `"Note": "kept"`) || !strings.Contains(got, `"IP": "cam.lan:8554"`) {
		t.Fatalf("file:\n%s", got)
	}
	c, err := LoadCameras(p)
	if err != nil || len(c.List) != 3 || c.List[0].Name != "Back door" || c.List[2].Name != "Garage" || !c.BOM {
		t.Fatalf("%+v %v", c, err)
	}
	// removing one
	n, err = PutCameras(p, []CameraRow{{OriginalName: "Front", Name: "Front", IP: "192.0.2.10", Record: true}})
	if err != nil || n != 2 {
		t.Fatalf("removal of two: %d %v", n, err)
	}
	if c, _ := LoadCameras(p); len(c.List) != 1 || c.List[0].Name != "Front" {
		t.Errorf("%+v", c)
	}
	// a pure reordering is a change
	p2 := camsPath(t)
	n, err = PutCameras(p2, []CameraRow{
		{OriginalName: "Back", Name: "Back", IP: "192.0.2.11", Quality: "stream2"},
		{OriginalName: "Front", Name: "Front", IP: "192.0.2.10", Record: true},
	})
	if err != nil || n != 1 || read(t, p2) == camsFile {
		t.Errorf("reorder: %d %v", n, err)
	}
}

func TestPutCamerasRefusalsLeaveTheFileByteIdentical(t *testing.T) {
	ok := CameraRow{OriginalName: "Front", Name: "Front", IP: "192.0.2.10", Record: true}
	for _, c := range []struct {
		name  string
		rows  []CameraRow
		row   int
		field string
	}{
		{"no legal character", []CameraRow{{Name: "///", IP: "192.0.2.12"}}, 0, "name"},
		{"reserved name", []CameraRow{{Name: "CON", IP: "192.0.2.12"}}, 0, "name"},
		{"blank name", []CameraRow{{Name: " ", IP: "192.0.2.12"}}, 0, "name"},
		{"bad port", []CameraRow{ok, {Name: "X", IP: "192.0.2.12:99999"}}, 1, "ip"},
		{"scheme in the address", []CameraRow{{Name: "X", IP: "rtsp://192.0.2.12"}}, 0, "ip"},
		{"no address", []CameraRow{{Name: "X"}}, 0, "ip"},
		{"bad stream", []CameraRow{{Name: "X", IP: "192.0.2.12", Quality: "a b"}}, 0, "quality"},
		{"same name twice", []CameraRow{{Name: "X", IP: "192.0.2.12"}, {Name: "x ", IP: "192.0.2.13"}}, 1, "name"},
		{"same folder", []CameraRow{{Name: "A B", IP: "192.0.2.12"}, {Name: "A_B", IP: "192.0.2.13"}}, 1, "name"},
		{"camera gone from the file", []CameraRow{{OriginalName: "Nope", Name: "Nope", IP: "192.0.2.12"}}, 0, "name"},
		{"original twice", []CameraRow{ok, ok}, 1, "name"},
	} {
		p := camsPath(t)
		n, err := PutCameras(p, c.rows)
		var inv *InvalidRows
		if !errors.As(err, &inv) || !errors.Is(err, cli.ErrRefused) || n != 0 {
			t.Errorf("%s: %d %v", c.name, n, err)
			continue
		}
		found := false
		for _, pr := range inv.Problems {
			found = found || (pr.Row == c.row && pr.Field == c.field)
		}
		if !found {
			t.Errorf("%s: %+v", c.name, inv.Problems)
		}
		if read(t, p) != camsFile {
			t.Errorf("%s: the file changed", c.name)
		}
		if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
			t.Errorf("%s: leftovers in the folder: %v", c.name, entries)
		}
	}
}

const cfgFile = "\xef\xbb\xbf{\r\n    \"SegmentSeconds\":  900,\r\n    \"Surprise\":  {\"keep\": [1, 2]},\r\n    \"logging\":  {\"level\": \"warn\"}\r\n}\r\n"

func TestEditConfigManyIsAllOrNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "holocron.config.json")
	os.WriteFile(p, []byte(cfgFile), 0o644)
	n := func(s string) json.RawMessage { return ParseValue(s) }

	eds, err := EditConfigMany(p, []KeyValue{{"ReserveGB", n("30")}, {"MaxRetentionDays", n("14")}, {"SegmentSeconds", n("600")}}, plat)
	if err != nil || len(eds) != 3 || !eds[0].Changed || !eds[1].Changed || !eds[2].Changed || string(eds[2].Old) != "900" {
		t.Fatalf("%+v %v", eds, err)
	}
	got := read(t, p)
	if !strings.HasPrefix(got, "\xef\xbb\xbf") || !strings.Contains(got, `"Surprise"`) || !strings.Contains(got, `"ReserveGB": 30`) || !strings.Contains(got, `"SegmentSeconds": 600`) {
		t.Fatalf("file:\n%s", got)
	}
	// unsetting, and a repeat that changes nothing writes nothing
	before := read(t, p)
	eds, err = EditConfigMany(p, []KeyValue{{"ReserveGB", n("30")}, {"MaxRetentionDays", nil}}, plat)
	if err != nil || eds[0].Changed || !eds[1].Changed || strings.Contains(read(t, p), "MaxRetentionDays") {
		t.Fatalf("%+v %v\n%s", eds, err, read(t, p))
	}
	again := read(t, p)
	if _, err := EditConfigMany(p, []KeyValue{{"ReserveGB", n("30")}}, plat); err != nil || read(t, p) != again || again == before {
		t.Error("a no-op edit changed the file")
	}

	// one bad value, one good: nothing is written
	base := read(t, p)
	for _, bad := range [][]KeyValue{
		{{"ReserveGB", n("40")}, {"SegmentSeconds", n("0")}},
		{{"ReserveGB", n("40")}, {"RootPath", n(`"/x"`)}},
		{{"ReserveGB", n("-1")}},
		{{"ReserveGB", n(`"lots"`)}},
	} {
		if _, err := EditConfigMany(p, bad, plat); !errors.Is(err, cli.ErrRefused) {
			t.Errorf("%v: %v", bad, err)
		}
		if read(t, p) != base {
			t.Errorf("%v: the file changed", bad)
		}
	}
	// a document problem keeps the validation error for field mapping
	_, err = EditConfigMany(p, []KeyValue{{"SegmentSeconds", n("0")}}, plat)
	var verr interface{ Unwrap() []error }
	if !errors.As(err, &verr) {
		t.Errorf("%T", err)
	}
}
