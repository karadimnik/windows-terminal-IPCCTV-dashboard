package status

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// How conformance is checked, and what is deliberately not:
//
//   - The authority is status.schema.json, enforced by validate.go (a validator
//     for the subset of JSON Schema the schema uses; any other keyword is a
//     violation, so the schema cannot outgrow the enforcement).
//   - Every positive fixture must also decode STRICTLY (DisallowUnknownFields)
//     into the typed Document and encode back to the same JSON, which proves the
//     Go types cover every field the PowerShell writers emit.
//   - The schema property names and the Go struct tags are cross-checked.
//   - Unknown keys are allowed by the schema (forward compatibility) and are
//     tested by running-extra-keys.json, which only the lenient decode accepts.

const goldenStatus = "../../testdata/golden/status"

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func positiveFixtures(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(goldenStatus, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no positive status fixtures in %s (err %v)", goldenStatus, err)
	}
	return files
}

func violationKeys(vs []Violation) []string {
	keys := make([]string, 0, len(vs))
	for _, v := range vs {
		keys = append(keys, v.Path+" "+v.Keyword)
	}
	sort.Strings(keys)
	return keys
}

func TestPositiveFixturesValidate(t *testing.T) {
	for _, f := range positiveFixtures(t) {
		vs, err := Validate(readFile(t, f))
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
		if len(vs) != 0 {
			t.Errorf("%s: %v", filepath.Base(f), violationKeys(vs))
		}
	}
}

// Each negative fixture is broken in exactly one way; the violations must be
// exactly the listed ones, so a fixture cannot fail for an unrelated reason.
var negatives = map[string][]string{
	"neg-missing-disk":                {"$.Disk required"},
	"neg-missing-camera-field":        {"$.Cameras[0].InputMode required"},
	"neg-missing-live-camera-field":   {"$.Live.Cameras[0].MainPath required"},
	"neg-wrong-type-pid":              {"$.SupervisorPid type"},
	"neg-wrong-type-bool":             {"$.Cameras[0].Alive type"},
	"neg-wrong-type-integer-fraction": {"$.SegmentSeconds type"},
	"neg-null-not-allowed":            {"$.Cameras[0].Name type"},
	"neg-bad-enum-state":              {"$.State enum"},
	"neg-bad-enum-stopkind":           {"$.StopKind enum"},
	"neg-bad-enum-inputmode":          {"$.Cameras[0].InputMode enum"},
	"neg-bad-enum-detectedby":         {"$.LastUnexpectedStop oneOf"},
	"neg-running-with-stopkind":       {"$.StopKind const"},
	"neg-bad-timestamp-shape":         {"$.UpdatedAt format", "$.UpdatedAt pattern"},
	"neg-bad-timestamp-six-digits":    {"$.UpdatedAt pattern"},
	"neg-bad-timestamp-zulu":          {"$.UpdatedAt pattern"},
	"neg-bad-timestamp-date":          {"$.UpdatedAt format"},
	"neg-bad-event-stamp":             {"$.Events[0].At pattern"},
	"neg-live-not-enabled":            {"$.Live.Enabled const"},
	"neg-bad-windowclose-field":       {"$.LastWindowClose.WindowTitle required"},
	"neg-negative-count":              {"$.Cameras[0].RestartsTotal minimum"},
}

func TestNegativeFixturesFailForTheStatedReason(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(goldenStatus, "negative", "*.json"))
	seen := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		want, ok := negatives[name]
		if !ok {
			t.Errorf("%s has no expected violation listed in this test", name)
			continue
		}
		seen[name] = true
		vs, err := Validate(readFile(t, f))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sort.Strings(want)
		if got := violationKeys(vs); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: violations %v, want %v", name, got, want)
		}
	}
	for name := range negatives {
		if !seen[name] {
			t.Errorf("negative fixture %s listed but not found", name)
		}
	}
}

func TestSchemaKeywordsAreAllEnforced(t *testing.T) {
	// An unsupported keyword must surface as a violation, never be ignored.
	vs, err := ValidateAgainst([]byte(`{"type":"object","patternProperties":{}}`), []byte(`{}`))
	if err != nil || len(vs) != 1 || vs[0].Keyword != "schema" {
		t.Fatalf("unsupported keyword not reported: %v %v", vs, err)
	}
	vs, _ = ValidateAgainst([]byte(`{"type":"object"}`), []byte(`[1]`))
	if len(vs) != 1 || vs[0].Keyword != "type" {
		t.Fatalf("type violation not reported: %v", vs)
	}
	vs, _ = ValidateAgainst(Schema, []byte(`not json`))
	if len(vs) != 1 || vs[0].Keyword != "parse" {
		t.Fatalf("parse failure not reported: %v", vs)
	}
}

func normalised(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTypedRoundTrip(t *testing.T) {
	for _, f := range positiveFixtures(t) {
		name := filepath.Base(f)
		raw := readFile(t, f)
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var doc Document
		err := dec.Decode(&doc)
		if name == "running-extra-keys.json" {
			if err == nil {
				t.Errorf("%s: strict decode accepted unknown keys", name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: strict decode: %v", name, err)
			continue
		}
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(normalised(t, raw), normalised(t, out)) {
			t.Errorf("%s: re-encoding the typed document changed it:\n%s", name, out)
		}
		if vs, _ := Validate(out); len(vs) != 0 {
			t.Errorf("%s: the re-encoded document violates the schema: %v", name, violationKeys(vs))
		}
	}
}

func TestUnknownKeysAreTolerated(t *testing.T) {
	raw := readFile(t, filepath.Join(goldenStatus, "running-extra-keys.json"))
	if !bytes.Contains(raw, []byte("FutureTopLevelField")) || !bytes.Contains(raw, []byte("FutureDiskField")) {
		t.Fatal("the extra-keys fixture lost its unknown keys")
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("lenient decode: %v", err)
	}
	if doc.Disk == nil || doc.Disk.FreeGB == 0 {
		t.Fatal("known fields next to unknown ones were lost")
	}
}

func TestTimeRoundTripsTheSevenDigitOffsetFormat(t *testing.T) {
	for _, s := range []string{
		"2026-10-25T03:30:00.1234567+03:00", // first pass through 03:30 (summer time)
		"2026-10-25T03:30:00.1234567+02:00", // second pass, after the fall-back
		"2027-03-28T03:59:59.9999999+02:00",
		"2027-03-28T04:00:00.0000000+03:00",
		"2026-10-17T09:30:00.0000000+00:00", // UTC prints +00:00, never Z
		"2026-10-17T09:30:00.1000000-05:30",
	} {
		var tm Time
		if err := json.Unmarshal([]byte(`"`+s+`"`), &tm); err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		out, _ := json.Marshal(tm)
		if string(out) != `"`+s+`"` {
			t.Errorf("%s round-tripped to %s", s, out)
		}
	}
	var tm Time
	if err := json.Unmarshal([]byte(`null`), &tm); err != nil || !tm.IsNull() {
		t.Errorf("null: %v %v", tm, err)
	}
	if out, _ := json.Marshal(tm); string(out) != "null" {
		t.Errorf("null marshalled as %s", out)
	}
	for _, bad := range []string{`"2026-10-25T03:30:00+02:00"`, `"2026-10-25T03:30:00.123456+02:00"`, `"2026-10-25T03:30:00.1234567Z"`, `"2026-10-25 03:30:00.1234567+02:00"`, `12`} {
		if err := json.Unmarshal([]byte(bad), &tm); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

// jsonNames lists the JSON property names of a struct type.
func jsonNames(typ reflect.Type) []string {
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func TestSchemaPropertiesMatchTheGoTypes(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(Schema, &root); err != nil {
		t.Fatal(err)
	}
	defs := root["$defs"].(map[string]any)
	props := func(node map[string]any) []string {
		var names []string
		for n := range node["properties"].(map[string]any) {
			names = append(names, n)
		}
		sort.Strings(names)
		return names
	}
	live := defs["live"].(map[string]any)
	liveCamera := live["properties"].(map[string]any)["Cameras"].(map[string]any)["items"].(map[string]any)
	cases := []struct {
		name string
		node map[string]any
		typ  reflect.Type
	}{
		{"root", root, reflect.TypeOf(Document{})},
		{"lastUnexpectedStop", defs["lastUnexpectedStop"].(map[string]any), reflect.TypeOf(UnexpectedStop{})},
		{"lastWindowClose", defs["lastWindowClose"].(map[string]any), reflect.TypeOf(WindowClose{})},
		{"disk", defs["disk"].(map[string]any), reflect.TypeOf(Disk{})},
		{"prune", defs["prune"].(map[string]any), reflect.TypeOf(Prune{})},
		{"restreamer", defs["restreamer"].(map[string]any), reflect.TypeOf(Restreamer{})},
		{"live", live, reflect.TypeOf(Live{})},
		{"live camera", liveCamera, reflect.TypeOf(LiveCamera{})},
		{"event", defs["event"].(map[string]any), reflect.TypeOf(Event{})},
		{"camera", defs["camera"].(map[string]any), reflect.TypeOf(Camera{})},
	}
	for _, c := range cases {
		if got, want := jsonNames(c.typ), props(c.node); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Go fields %v, schema properties %v", c.name, got, want)
		}
		known := map[string]bool{}
		for _, n := range props(c.node) {
			known[n] = true
		}
		for _, r := range c.node["required"].([]any) {
			if !known[r.(string)] {
				t.Errorf("%s: required %v is not a property", c.name, r)
			}
		}
	}
	if n := len(defs["camera"].(map[string]any)["required"].([]any)); n != 20 {
		t.Errorf("a camera row has %d required fields, want 20", n)
	}
}

// Known differences of the PowerShell producer, recorded as expected-failure
// items (see KNOWN-DIFFERENCES.md). They pass while the PowerShell behaves as
// described, and fail the day it changes, which is the signal to update the
// document and the Go producer's own conformance test.

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	return string(readFile(t, filepath.Join("..", "..", rel)))
}

func TestKnownDifferenceF1LastWindowCloseIsNotCarried(t *testing.T) {
	before := readFile(t, filepath.Join(goldenStatus, "patched-windowclose-over-running.json"))
	after := readFile(t, filepath.Join(goldenStatus, "restarted-after-windowclose.json"))
	if !bytes.Contains(before, []byte(`"LastWindowClose"`)) {
		t.Fatal("the patched document lost LastWindowClose")
	}
	if bytes.Contains(after, []byte(`"LastWindowClose"`)) {
		t.Error("F1 no longer observed: the PowerShell writer now carries LastWindowClose; update KNOWN-DIFFERENCES.md")
	}
	// Code reading: the supervisor carries LastUnexpectedStop only, and the writer's document has no such key.
	if strings.Contains(repoFile(t, "cam.recorder.ps1"), "LastWindowClose") {
		t.Error("cam.recorder.ps1 now mentions LastWindowClose: re-check F1")
	}
	if !strings.Contains(repoFile(t, "cam.common.ps1"), "'LastWindowClose'") {
		t.Error("the warden patch no longer writes LastWindowClose")
	}
}

func TestKnownDifferenceWriterValidateSetLacksWindowClosed(t *testing.T) {
	src := repoFile(t, "cam.recorder.ps1")
	m := regexp.MustCompile(`\[ValidateSet\(([^)]*)\)\]\[string\]\$StopKind`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("the StopKind ValidateSet of Write-CamStatusFile was not found")
	}
	if strings.Contains(m[1], "window-closed") {
		t.Error("the writer now accepts window-closed; update KNOWN-DIFFERENCES.md")
	}
	if !strings.Contains(repoFile(t, "cam.common.ps1"), "'window-closed'") {
		t.Error("the warden patch no longer writes StopKind window-closed")
	}
	// The schema accepts it, and a fixture proves a real document carries it.
	if !bytes.Contains(Schema, []byte(`"window-closed"`)) {
		t.Error("the schema lost window-closed")
	}
	if !bytes.Contains(readFile(t, filepath.Join(goldenStatus, "patched-windowclose-minimal.json")), []byte(`"window-closed"`)) {
		t.Error("the window-closed fixture lost its StopKind")
	}
}

// PowerShell producer check: run the real writers (real clock, real pid, real
// free space) and validate what they wrote. Skipped without powershell.exe.
func TestPowerShellProducerOutputConforms(t *testing.T) {
	if os.Getenv("HOLOCRON_SKIP_POWERSHELL") != "" {
		t.Skip("HOLOCRON_SKIP_POWERSHELL is set")
	}
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe is not available: the PowerShell producer check did not run")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "tests", "holocron", "status-conformance.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	dir := ""
	if runtime.GOOS != "windows" {
		// Windows PowerShell must not start in a \\wsl.localhost directory.
		if st, err := os.Stat("/mnt/c/Windows/Temp"); err != nil || !st.IsDir() {
			t.Skip("no /mnt/c/Windows/Temp to start powershell.exe in")
		}
		dir = "/mnt/c/Windows/Temp"
		out, err := exec.Command("wslpath", "-w", script).Output()
		if err != nil {
			t.Skip("wslpath is not available to convert the script path")
		}
		script = strings.TrimSpace(string(out))
	}
	cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status-conformance.ps1 failed: %v\n%s", err, out)
	}
	files, _ := filepath.Glob(filepath.Join(goldenStatus, "generated", "*.json"))
	if len(files) < 10 {
		t.Fatalf("the PowerShell producer wrote %d documents, want at least 10", len(files))
	}
	for _, f := range files {
		vs, err := Validate(readFile(t, f))
		if err != nil || len(vs) != 0 {
			t.Errorf("%s: %v %v", filepath.Base(f), violationKeys(vs), err)
		}
		var doc Document
		dec := json.NewDecoder(bytes.NewReader(readFile(t, f)))
		dec.DisallowUnknownFields()
		if name := filepath.Base(f); name != "running-extra-keys.json" {
			if err := dec.Decode(&doc); err != nil {
				t.Errorf("%s: strict decode: %v", name, err)
			}
		}
	}
	t.Logf("validated %d documents written by the PowerShell producer", len(files))
}

// Result-only check of a real running document: set HOLOCRON_LIVE_STATUS to
// its path. Nothing from the document is ever printed, only counts and
// locations of violations.
func TestLiveStatusFileConforms(t *testing.T) {
	path := os.Getenv("HOLOCRON_LIVE_STATUS")
	if path == "" {
		t.Skip("HOLOCRON_LIVE_STATUS is not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skip("cannot read the live status file")
	}
	vs, err := Validate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Errorf("the live document has %d violations: %v", len(vs), violationKeys(vs))
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})))
	dec.DisallowUnknownFields()
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		t.Errorf("strict typed decode of the live document failed: %v", err)
	}
	t.Logf("live status.json: %d bytes, %d violations, state %q, %d camera rows, Live section present: %v",
		len(raw), len(vs), doc.State, len(doc.Cameras), doc.Live != nil)
}

// The Go host records a window close itself (no warden process), so LastWindowClose.DetectedBy
// accepts "nvr host" besides the PowerShell warden's "nvr warden"; anything else stays refused.
// The golden fixtures are not edited: the variants are made in memory.
func TestLastWindowCloseDetectedByHostOrWarden(t *testing.T) {
	base := readFile(t, filepath.Join(goldenStatus, "patched-windowclose-over-running.json"))
	if !bytes.Contains(base, []byte(`"nvr warden"`)) {
		t.Fatal("fixture no longer carries DetectedBy nvr warden")
	}
	for value, ok := range map[string]bool{"nvr warden": true, "nvr host": true, "supervisor": false, "": false} {
		doc := bytes.Replace(base, []byte(`"nvr warden"`), []byte(`"`+value+`"`), 1)
		vs, err := Validate(doc)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(vs) == 0; got != ok {
			t.Errorf("DetectedBy %q: valid=%v, want %v (%v)", value, got, ok, vs)
		}
		if ok {
			var d Document
			dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(doc, []byte("\xef\xbb\xbf"))))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&d); err != nil || d.LastWindowClose == nil || d.LastWindowClose.DetectedBy != value {
				t.Errorf("DetectedBy %q: strict decode %v, %+v", value, err, d.LastWindowClose)
			}
		}
	}
}
