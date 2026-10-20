package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Invented cameras only (documentation addresses 192.0.2.x), never a real file.
const psCameras = `[
    {
        "Name":  "Front Door",
        "IP":  "192.0.2.10",
        "Quality":  "stream1",
        "Record":  true,
        "Notes":  {"floor": 1, "tags": ["a", "b"]},
        "Zeta":  null
    },
    {
        "Name":  "Back Yard",
        "IP":  "192.0.2.11",
        "Quality":  "stream2"
    }
]
`

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cameras.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCamerasLoad(t *testing.T) {
	c, err := LoadCameras(writeFile(t, "\xef\xbb\xbf"+psCameras))
	if err != nil {
		t.Fatal(err)
	}
	if !c.BOM || len(c.List) != 2 {
		t.Fatalf("%+v", c)
	}
	f, b := c.List[0], c.List[1]
	if f.Name != "Front Door" || f.IP != "192.0.2.10" || f.Stream() != "stream1" || !f.Record {
		t.Errorf("front: %+v", f)
	}
	if b.Record || b.Stream() != "stream2" { // an absent Record is off
		t.Errorf("back: %+v", b)
	}
	if (Camera{}).Stream() != "stream1" || (Camera{Quality: "  "}).Stream() != "stream1" {
		t.Error("a blank Quality means stream1")
	}
	if i := c.Find("front door"); i != 0 || c.Find("nobody") != -1 {
		t.Errorf("Find: %d", i)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	// keys are case-insensitive, like the PowerShell property access
	l, err := LoadCameras(writeFile(t, `[{"name":"A","ip":"192.0.2.1","record":true}]`))
	if err != nil || l.List[0].Name != "A" || !l.List[0].Record {
		t.Errorf("lower-case keys: %v %+v", err, l)
	}
	if e, err := LoadCameras(writeFile(t, "[]")); err != nil || len(e.List) != 0 {
		t.Errorf("empty list: %v", err)
	}
}

func TestCamerasStrictness(t *testing.T) {
	bad := []struct{ name, doc, path, text string }{
		{"record as string (IR 10.9)", `[{"Name":"A","IP":"192.0.2.1","Record":"false"}]`, "cameras[0].Record", "JSON boolean"},
		{"record as number", `[{"Name":"A","IP":"192.0.2.1"},{"Name":"B","IP":"192.0.2.2","Record":1}]`, "cameras[1].Record", "JSON boolean"},
		{"record null", `[{"Name":"A","IP":"x","Record":null}]`, "cameras[0].Record", "boolean"},
		{"name number", `[{"Name":5,"IP":"192.0.2.1"}]`, "cameras[0].Name", "string"},
		{"ip array", `[{"Name":"A","IP":["x"]}]`, "cameras[0].IP", "string"},
		{"entry not an object", `[{"Name":"A","IP":"x"}, 7]`, "cameras[1]", "object"},
		{"not an array", `{"Name":"A"}`, "", "JSON array"},
		{"broken", `[{"Name":`, "", "JSON array"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			c, err := LoadCameras(writeFile(t, tc.doc))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("accepted: %v", err)
			}
			ok := false
			for _, p := range ve.Problems {
				ok = ok || p.Path == tc.path && strings.Contains(p.Message, tc.text)
			}
			if !ok {
				t.Errorf("problems %v", ve.Problems)
			}
			if c == nil {
				t.Error("the partially read list must still be returned")
			}
		})
	}
	// all problems at once
	_, err := LoadCameras(writeFile(t, `[{"Name":1,"IP":2,"Record":"x"},{"Name":"B","IP":"i","Record":3}]`))
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 4 {
		t.Errorf("want 4 problems: %v", err)
	}
	if _, err := LoadCameras(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing camera list is an error")
	}
}

func TestCamerasSavePreservesEverything(t *testing.T) {
	for _, bom := range []bool{true, false} {
		src := psCameras
		if bom {
			src = "\xef\xbb\xbf" + src
		}
		p := writeFile(t, src)
		c, _ := LoadCameras(p)
		c.List[1].Record = true // the PowerShell "-Enable" change
		c.List[0].Name = "Front Gate"
		if err := c.Save(p); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(p)
		if bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) != bom {
			t.Errorf("bom=%v: BOM after save = %v", bom, !bom)
		}
		if bytes.Count(raw, []byte("\xef\xbb\xbf")) > 1 {
			t.Error("BOM duplicated")
		}
		again, err := LoadCameras(p)
		if err != nil || again.BOM != bom || again.List[0].Name != "Front Gate" || !again.List[1].Record {
			t.Fatalf("round trip: %v %+v", err, again)
		}
		text := string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
		// unknown fields survive, in place (after Record, before the end), with their values
		if !strings.Contains(text, `"Notes": {`) || !strings.Contains(text, `"floor": 1`) || !strings.Contains(text, `"Zeta": null`) {
			t.Errorf("unknown fields lost:\n%s", text)
		}
		if strings.Index(text, `"Name"`) > strings.Index(text, `"IP"`) || strings.Index(text, `"Record"`) > strings.Index(text, `"Notes"`) {
			t.Errorf("field order changed:\n%s", text)
		}
		// Record was absent on the second camera and is added
		if strings.Count(text, `"Record"`) != 2 {
			t.Errorf("Record count:\n%s", text)
		}
		if !strings.HasSuffix(text, "]\n") {
			t.Error("trailing newline")
		}
		if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "*.tmp")); len(left) != 0 {
			t.Errorf("temp files left: %v", left)
		}
	}
}

func TestCamerasSaveBOMFollowsTheFileOnDisk(t *testing.T) {
	// a list read without BOM, saved over a file that now has one, keeps the file's BOM (Set-CamList reads the head of the target)
	p := writeFile(t, "\xef\xbb\xbf[]")
	c := &Cameras{List: []Camera{{Name: "N", IP: "192.0.2.5", Record: true}}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(p); !bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		t.Error("BOM of the existing file lost")
	}
	// a new file follows the list
	np := filepath.Join(t.TempDir(), "new.json")
	c = &Cameras{List: c.List}
	if err := c.Save(np); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(np); bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) || !strings.Contains(string(raw), `"Name": "N"`) {
		t.Errorf("new file: %q", raw)
	}
	if e := (&Cameras{}); e.Save(filepath.Join(t.TempDir(), "e.json")) != nil {
		t.Error("empty list must save")
	}
	if err := c.Save(filepath.Join(t.TempDir(), "no-such-dir", "x.json")); err == nil {
		t.Error("saving into a missing folder must fail")
	}
	// a failed save leaves the old file as it was
	old := writeFile(t, `[{"Name":"keep","IP":"192.0.2.9"}]`)
	dir := filepath.Dir(old)
	if runtime.GOOS == "windows" {
		return // folder permission bits do not stop a save there
	}
	os.Chmod(dir, 0o500)
	err := c.Save(old)
	os.Chmod(dir, 0o700)
	if err == nil && os.Geteuid() != 0 {
		t.Error("expected failure in a read-only folder")
	}
	if raw, _ := os.ReadFile(old); !strings.Contains(string(raw), "keep") && err != nil {
		t.Error("old file damaged by a failed save")
	}
}

func TestCamerasSaveEscaping(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	c := &Cameras{List: []Camera{{Name: "R&D <east> \"q\"", IP: "192.0.2.1"}}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	back, err := LoadCameras(p)
	if err != nil || back.List[0].Name != c.List[0].Name {
		t.Errorf("round trip: %v %+v", err, back)
	}
	if raw, _ := os.ReadFile(p); strings.Contains(string(raw), `\u0026`) {
		t.Errorf("HTML escaping rewrote the name: %s", raw)
	}
}

func TestCamerasValidateAndCollisions(t *testing.T) {
	c := &Cameras{List: []Camera{
		{Name: "Front Door", IP: "192.0.2.10", Quality: "stream1"}, // 0
		{Name: "front_door", IP: "192.0.2.11"},                     // 1 collides with 0 (case, space)
		{Name: "FRONT DOOR", IP: "192.0.2.12"},                     // 2 collides with 0
		{Name: "Yard", IP: "cam.example.com"},                      // 3 fine
		{Name: "CON", IP: "192.0.2.13"},                            // 4 reserved
		{Name: "Gate", IP: ""},                                     // 5 no address
		{Name: "Shed", IP: "http://x/y"},                           // 6 not a host
		{Name: "Pool", IP: "192.0.2.14", Quality: "a b"},           // 7 bad stream
		{Name: "<>", IP: "192.0.2.15"},                             // 8 no legal characters
	}}
	cols := c.Collisions()
	if len(cols) != 1 || len(cols[0].Indexes) != 3 || cols[0].Folder != "Front_Door" || cols[0].Names[2] != "FRONT DOOR" {
		t.Fatalf("collisions: %+v", cols)
	}
	err := c.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatal("accepted")
	}
	want := map[string]string{
		"cameras[1].Name": "share the folder", "cameras[2].Name": "share the folder", "cameras[4].Name": "reserved",
		"cameras[5].IP": "address is required", "cameras[6].IP": "no scheme", "cameras[7].Quality": "stream name", "cameras[8].Name": "no characters",
	}
	got := map[string]string{}
	for _, p := range ve.Problems {
		got[p.Path] = p.Message
	}
	for path, text := range want {
		if !strings.Contains(got[path], text) {
			t.Errorf("%s: %q does not contain %q", path, got[path], text)
		}
	}
	if len(ve.Problems) != len(want) {
		t.Errorf("%d problems, want %d: %v", len(ve.Problems), len(want), ve.Problems)
	}
	for _, i := range []string{"cameras[0]", "cameras[3]"} { // controls: fine cameras produce nothing
		for path := range got {
			if strings.HasPrefix(path, i) {
				t.Errorf("good camera flagged: %s", path)
			}
		}
	}
}

// A '%' in a camera NAME stays valid: the ffmpeg package doubles it in the output pattern.
func TestCameraNameMayContainPercent(t *testing.T) {
	c, err := LoadCameras(writeFile(t, `[{"Name":"Cam%Y","IP":"192.0.2.12","Quality":"stream1"}]`))
	if err != nil || len(c.List) != 1 {
		t.Fatalf("%v %+v", err, c)
	}
}

// The address may carry a port (IR 2); host and port are both validated.
func TestValidateHostWithPort(t *testing.T) {
	good := []string{"192.0.2.10", "192.0.2.10:554", "127.0.0.1:18700", "cam.example.com:8554", "cam:1", "cam:65535", "[2001:db8::1]:554", "2001:db8::1"}
	bad := map[string]string{
		"192.0.2.10:0": "port", "192.0.2.10:65536": "port", "192.0.2.10:-1": "port", "192.0.2.10:": "port", "192.0.2.10:http": "port",
		"192.0.2.10:+5": "port", "cam:5:6": "host:port", "[::1]x": "host:port", ":554": "hostname",
		"bad_host:554": "hostname", "-x.example:554": "hostname", "rtsp://h:554": "no scheme", "u:p@h:554": "credentials", "h:554/x": "path", "h :554": "no scheme",
	}
	for _, v := range good {
		if msg := validateHost(v); msg != "" {
			t.Errorf("%q refused: %s", v, msg)
		}
	}
	for v, want := range bad {
		if msg := validateHost(v); !strings.Contains(msg, want) {
			t.Errorf("%q: %q, want it to mention %q", v, msg, want)
		}
	}
}

// The end-to-end fixture shape: an extra field kept, the port in IP, validated and saved as written.
func TestCamerasPortFixtureShape(t *testing.T) {
	p := writeFile(t, `[{"Name":"cam1","IP":"127.0.0.1:18700","Quality":"cam1","Record":true,"Note":"kept"},{"Name":"cam2","IP":"127.0.0.1:18700","Quality":"cam2","Record":true}]`)
	c, err := LoadCameras(p)
	if err != nil || c.Validate() != nil || c.List[0].IP != "127.0.0.1:18700" {
		t.Fatalf("%+v %v %v", c.List, err, c.Validate())
	}
	c.List[1].IP = "127.0.0.1:99999"
	if c.Validate() == nil {
		t.Error("port 99999 accepted")
	}
}
