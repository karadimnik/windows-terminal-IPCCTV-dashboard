package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// theOperatorsList is the real camera list, shape and all, so the tests are
// held against names and addresses that actually exist rather than invented
// ones. The addresses are the Operator's own; cameras.json holds no secrets.
var theOperatorsList = []CameraPut{
	{OriginalName: "Front Entrance", Name: "Front Entrance", IP: "192.168.50.15", Quality: "stream1", Record: true},
	{OriginalName: "Rear Balcony", Name: "Rear Balcony", IP: "192.168.50.13", Quality: "stream1", Record: true},
	{OriginalName: "Field Front", Name: "Field Front", IP: "192.168.50.41", Quality: "stream1", Record: true},
}

func TestValidCameraListPasses(t *testing.T) {
	if invalid := validateCameras(theOperatorsList); len(invalid) != 0 {
		t.Fatalf("the real camera list must validate, got %+v", invalid)
	}
}

func TestCameraNameValidation(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string // substring of the message, or "" for valid
	}{
		{name: "a real name", input: "Workshop Entrance", want: ""},
		{name: "dashes and digits", input: "Gate-2_north", want: ""},
		{name: "empty", input: "", want: "a name is required"},
		{name: "only whitespace", input: "   ", want: "a name is required"},
		{name: "a path separator", input: `Front/Entrance`, want: "may not contain"},
		{name: "a backslash", input: `Front\Entrance`, want: "may not contain"},
		{name: "a colon", input: "Front: Entrance", want: "may not contain"},
		{name: "a wildcard", input: "Front*", want: "may not contain"},
		{name: "a trailing dot", input: "Front Entrance.", want: "may not end with a dot"},
		{name: "a control character", input: "Front\x01Entrance", want: "control characters"},
		{name: "a reserved device name", input: "NUL", want: "reserved Windows device name"},
		{name: "a reserved name with an extension", input: "COM1.cam", want: "reserved Windows device name"},
		{name: "nothing legal left", input: `///`, want: "may not contain"},
		{name: "too long", input: strings.Repeat("a", maxCameraNameLength+1), want: "at most"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := validateCameraName(test.input)
			if test.want == "" && got != "" {
				t.Fatalf("%q should be valid, got %q", test.input, got)
			}
			if test.want != "" && !strings.Contains(got, test.want) {
				t.Fatalf("%q: message %q, want it to contain %q", test.input, got, test.want)
			}
		})
	}
}

func TestHostValidation(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{input: "192.168.50.15", want: ""},
		{input: "10.0.0.1", want: ""},
		{input: "camera-1.local", want: ""},
		{input: "nvr", want: ""},
		{input: "fe80::1", want: ""},
		{input: "", want: "an address is required"},
		{input: "rtsp://192.168.50.15/stream1", want: "host only"},
		{input: "192.168.50.15:554", want: "host only"},
		{input: "user:pass@192.168.50.15", want: "host only"},
		{input: "192.168.50.15/stream1", want: "host only"},
		{input: "has a space", want: "host only"},
		{input: "-leading-dash", want: "not a valid IP address or hostname"},
		{input: "under_score.local", want: "not a valid IP address or hostname"},
	}

	for _, test := range cases {
		got := validateHost(test.input)
		if test.want == "" && got != "" {
			t.Errorf("%q should be valid, got %q", test.input, got)
			continue
		}
		if test.want != "" && !strings.Contains(got, test.want) {
			t.Errorf("%q: message %q, want it to contain %q", test.input, got, test.want)
		}
	}
}

func TestDuplicateAndCollidingNamesAreRejected(t *testing.T) {
	duplicate := validateCameras([]CameraPut{
		{Name: "Workshop", IP: "192.168.50.40", Quality: "stream1"},
		{Name: "workshop", IP: "192.168.50.41", Quality: "stream1"},
	})
	if len(duplicate) != 1 || duplicate[0].Row != 1 || !strings.Contains(duplicate[0].Message, "row 1") {
		t.Fatalf("two names differing only in case must collide, got %+v", duplicate)
	}

	// "Front Entrance" and "Front_Entrance" both become the folder
	// Front_Entrance, so the second camera's footage would land in the first
	// camera's directory. Get-CamSafeName does the same substitution.
	collision := validateCameras([]CameraPut{
		{Name: "Front Entrance", IP: "192.168.50.15", Quality: "stream1"},
		{Name: "Front_Entrance", IP: "192.168.50.16", Quality: "stream1"},
	})
	if len(collision) != 1 || !strings.Contains(collision[0].Message, "same folder on disk") {
		t.Fatalf("names colliding on disk must be rejected, got %+v", collision)
	}
}

func TestStreamNameValidation(t *testing.T) {
	for _, quality := range []string{"stream1", "stream2", "main.sub", "a-b_c"} {
		if invalid := validateCameras([]CameraPut{{Name: "Cam", IP: "10.0.0.1", Quality: quality}}); len(invalid) != 0 {
			t.Errorf("%q should be a valid stream name, got %+v", quality, invalid)
		}
	}
	for _, quality := range []string{"", "   ", "stream 1", "a/b", "../etc"} {
		invalid := validateCameras([]CameraPut{{Name: "Cam", IP: "10.0.0.1", Quality: quality}})
		if len(invalid) != 1 || invalid[0].Field != "quality" {
			t.Errorf("%q should be rejected, got %+v", quality, invalid)
		}
	}
}

// TestEveryProblemIsReportedAtOnce matters for the page: marking one bad field
// per attempt turns a three-field mistake into three round trips.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	invalid := validateCameras([]CameraPut{
		{Name: "", IP: "nope://x", Quality: ""},
		{Name: "Fine", IP: "10.0.0.1", Quality: "stream1"},
	})
	if len(invalid) != 3 {
		t.Fatalf("want 3 problems on row 0, got %d: %+v", len(invalid), invalid)
	}
	for _, problem := range invalid {
		if problem.Row != 0 {
			t.Errorf("row %d should be valid: %+v", problem.Row, problem)
		}
	}
}

func TestRecorderConfigValidation(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{name: "the live values", input: map[string]any{
			"SegmentSeconds": 900.0, "ReserveGB": 20.0, "PruneHysteresisGB": 5.0, "MaxRetentionDays": nil,
		}},
		{name: "five minute segments", input: map[string]any{"SegmentSeconds": 300.0}},
		{name: "retention within range", input: map[string]any{"MaxRetentionDays": 30.0}},
		{name: "a fractional reserve", input: map[string]any{"ReserveGB": 20.5}},
		{name: "no hysteresis", input: map[string]any{"PruneHysteresisGB": 0.0}},

		{name: "segments too short", input: map[string]any{"SegmentSeconds": 10.0}, want: "between 60 and 3600 seconds"},
		{name: "segments too long", input: map[string]any{"SegmentSeconds": 86400.0}, want: "between 60 and 3600 seconds"},
		{name: "fractional segments", input: map[string]any{"SegmentSeconds": 900.5}, want: "whole number"},
		{name: "retention out of range", input: map[string]any{"MaxRetentionDays": 400.0}, want: "between 1 and 365 days"},
		{name: "retention zero", input: map[string]any{"MaxRetentionDays": 0.0}, want: "between 1 and 365 days"},
		{name: "a reserve of nothing", input: map[string]any{"ReserveGB": 0.0}, want: "between 1 and 100000 GB"},
		{name: "a null reserve", input: map[string]any{"ReserveGB": nil}, want: "a value is required"},
		{name: "a string", input: map[string]any{"ReserveGB": "twenty"}, want: "must be a number"},

		{name: "a key outside the allow-list", input: map[string]any{"RelayBase": "rtsp://127.0.0.1:8554"}, want: "not an editable setting"},
		{name: "a path key", input: map[string]any{"RootPath": `D:\elsewhere`}, want: "not an editable setting"},
		{name: "a key that does not exist", input: map[string]any{"Nonsense": 1.0}, want: "not an editable setting"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			invalid := validateRecorderConfig(test.input)
			if test.want == "" {
				if len(invalid) != 0 {
					t.Fatalf("should be accepted, got %+v", invalid)
				}
				return
			}
			if len(invalid) != 1 {
				t.Fatalf("want exactly one problem, got %+v", invalid)
			}
			if !strings.Contains(invalid[0].Message, test.want) {
				t.Fatalf("message %q, want it to contain %q", invalid[0].Message, test.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The endpoints, against real files in a temporary directory
// ---------------------------------------------------------------------------

// liveCamerasJSON is the Operator's file byte for byte: a UTF-8 BOM, CRLF, and
// a Site field that camui knows nothing about and must still preserve.
const liveCamerasJSON = "\ufeff[\r\n" +
	"    {\r\n        \"Name\":  \"Front Entrance\",\r\n        \"IP\":  \"192.168.50.15\",\r\n" +
	"        \"Quality\":  \"stream1\",\r\n        \"Record\":  true,\r\n        \"Site\":  \"north\"\r\n    },\r\n" +
	"    {\r\n        \"Name\":  \"Workshop\",\r\n        \"IP\":  \"192.168.50.40\",\r\n" +
	"        \"Quality\":  \"stream1\",\r\n        \"Record\":  true\r\n    }\r\n]\r\n"

const liveRecorderConfigJSON = `{
  "RootPath": "E:\\cameras",
  "SegmentSeconds": 900,
  "ReserveGB": 20,
  "PruneHysteresisGB": 5,
  "MaxRetentionDays": null,
  "RelayBase": "rtsp://127.0.0.1:8554",
  "StatusIntervalSeconds": 5
}
`

// newTestAPI returns an API over a throwaway script directory, with the
// PowerShell invoker replaced by a recorder of what it was asked to run. No
// test here is allowed to execute a script.
func newTestAPI(t *testing.T) (*API, *[]psResult, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cameras.json"), []byte(liveCamerasJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recorder.config.json"), []byte(liveRecorderConfigJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	calls := &[]psResult{}
	api := &API{
		cfg:   Config{ScriptDir: dir, StatusFile: filepath.Join(dir, "status.json")},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		alive: func(int) bool { return true },
	}
	api.runScript = func(timeout time.Duration, script string, args ...string) psResult {
		result := psResult{Command: filepath.Base(script) + " " + strings.Join(args, " ")}
		*calls = append(*calls, result)
		return result
	}
	return api, calls, dir
}

func request(t *testing.T, api *API, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	api.Routes(mux)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(method, path, reader))
	return recorder
}

func TestGetCamerasReturnsTheEditableFields(t *testing.T) {
	api, _, _ := newTestAPI(t)
	response := request(t, api, "GET", "/api/cameras", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}

	var body CamerasResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Cameras) != 2 {
		t.Fatalf("want 2 cameras, got %d", len(body.Cameras))
	}
	if body.Cameras[0].Name != "Front Entrance" || body.Cameras[0].IP != "192.168.50.15" {
		t.Errorf("first camera came back as %+v", body.Cameras[0])
	}
	// The unknown field is preserved by the writer, not published by the API.
	if bytes.Contains(response.Body.Bytes(), []byte("Site")) {
		t.Error("the response should carry only the four editable fields")
	}
}

// TestInvalidPutWritesNothing is the whole-request-or-nothing rule: the writer
// must not even be invoked.
func TestInvalidPutWritesNothing(t *testing.T) {
	api, calls, dir := newTestAPI(t)
	before, err := os.ReadFile(filepath.Join(dir, "cameras.json"))
	if err != nil {
		t.Fatal(err)
	}

	response := request(t, api, "PUT", "/api/cameras", `{"cameras":[
		{"originalName":"Front Entrance","name":"Front Entrance","ip":"192.168.50.15","quality":"stream1","record":true},
		{"originalName":"Workshop","name":"Workshop","ip":"rtsp://192.168.50.40/stream1","quality":"stream1","record":true}
	]}`)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", response.Code, response.Body.String())
	}
	var failure APIError
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if len(failure.Invalid) != 1 || failure.Invalid[0].Row != 1 || failure.Invalid[0].Field != "ip" {
		t.Fatalf("want one problem on row 1's ip, got %+v", failure.Invalid)
	}
	if !strings.Contains(failure.Error, "Nothing was written") {
		t.Errorf("the message must say nothing was written, got %q", failure.Error)
	}
	if len(*calls) != 0 {
		t.Fatalf("the writer must not be invoked at all, it was called %d time(s)", len(*calls))
	}

	after, err := os.ReadFile(filepath.Join(dir, "cameras.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("cameras.json changed on a rejected request")
	}
}

func TestInvalidConfigPutWritesNothing(t *testing.T) {
	api, calls, _ := newTestAPI(t)
	response := request(t, api, "PUT", "/api/config", `{"config":{"MaxRetentionDays":400}}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", response.Code, response.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("the writer must not be invoked, it was called %d time(s)", len(*calls))
	}
	if !strings.Contains(response.Body.String(), "between 1 and 365 days") {
		t.Errorf("the message must say what would be right: %s", response.Body.String())
	}
}

// TestGetConfigPublishesOnlyTheAllowList is a leak test. RelayBase is the one
// key in recorder.config.json that always holds a URL.
func TestGetConfigPublishesOnlyTheAllowList(t *testing.T) {
	api, _, _ := newTestAPI(t)
	response := request(t, api, "GET", "/api/config", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}

	raw := response.Body.String()
	for _, forbidden := range []string{"RelayBase", "rtsp://", "RootPath", "StatusIntervalSeconds"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("%q must not appear in /api/config: %s", forbidden, raw)
		}
	}

	var body ConfigResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Config) != len(editableConfigKeys) {
		t.Fatalf("want exactly the %d allow-listed keys, got %+v", len(editableConfigKeys), body.Config)
	}
	if body.Config["SegmentSeconds"] != 900.0 {
		t.Errorf("SegmentSeconds came back as %v", body.Config["SegmentSeconds"])
	}
	if body.Config["MaxRetentionDays"] != nil {
		t.Errorf("MaxRetentionDays is null in the file, came back as %v", body.Config["MaxRetentionDays"])
	}
}

func TestControlRequestsAreTranslatedToScriptArguments(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{body: `{"scope":"all","action":"stop"}`, want: "cam.ps1 -Stop"},
		{body: `{"scope":"all","action":"start"}`, want: "cam.ps1 -Start"},
		{body: `{"scope":"recorder","action":"stop"}`, want: "cam.ps1 -Stop -Recorder"},
		{body: `{"scope":"dashboard","action":"start"}`, want: "cam.ps1 -Start -Dashboard"},
	}

	for _, test := range cases {
		api, calls, _ := newTestAPI(t)
		response := request(t, api, "POST", "/api/control", test.body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", test.body, response.Code, response.Body.String())
		}
		if len(*calls) != 1 {
			t.Fatalf("%s: want one invocation, got %d", test.body, len(*calls))
		}
		if got := strings.TrimSpace((*calls)[0].Command); got != test.want {
			t.Errorf("%s ran %q, want %q", test.body, got, test.want)
		}
	}
}

func TestControlRejectsAnUnknownScopeOrAction(t *testing.T) {
	for _, body := range []string{
		`{"scope":"everything","action":"stop"}`,
		`{"scope":"all","action":"restart"}`,
		`{"scope":"all"}`,
		`{"action":"stop"}`,
	} {
		api, calls, _ := newTestAPI(t)
		response := request(t, api, "POST", "/api/control", body)
		if response.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", body, response.Code)
		}
		if len(*calls) != 0 {
			t.Errorf("%s: nothing should have been run", body)
		}
	}
}

func TestRecordToggleTranslatesToEnableOrDisable(t *testing.T) {
	api, calls, _ := newTestAPI(t)
	if response := request(t, api, "POST", "/api/cameras/Workshop/record", `{"enabled":false}`); response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if got := strings.TrimSpace((*calls)[0].Command); got != "cam.recorder.ps1 -Disable Workshop" {
		t.Errorf("ran %q", got)
	}

	if response := request(t, api, "POST", "/api/cameras/Front%20Entrance/record", `{"enabled":true}`); response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if got := strings.TrimSpace((*calls)[1].Command); got != "cam.recorder.ps1 -Enable Front Entrance" {
		t.Errorf("ran %q", got)
	}
}

func TestRecordToggleRefusesAnUnknownCamera(t *testing.T) {
	api, calls, _ := newTestAPI(t)
	response := request(t, api, "POST", "/api/cameras/Nowhere/record", `{"enabled":true}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", response.Code, response.Body.String())
	}
	if len(*calls) != 0 {
		t.Error("nothing should have been run for a camera that is not in the list")
	}
}

func TestRecordToggleNeedsAnExplicitEnabled(t *testing.T) {
	api, calls, _ := newTestAPI(t)
	response := request(t, api, "POST", "/api/cameras/Workshop/record", `{}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", response.Code, response.Body.String())
	}
	if len(*calls) != 0 {
		t.Error("an absent 'enabled' must not be read as false")
	}
}

// TestStatusCarriesTheSupervisorVerdict checks the endpoint publishes the
// liveness decision, not just the file's own State field.
func TestStatusCarriesTheSupervisorVerdict(t *testing.T) {
	api, _, dir := newTestAPI(t)
	api.alive = func(int) bool { return false }

	status := `{"State":"running","StopKind":"","SupervisorPid":4242,"UpdatedAt":"` +
		time.Now().Format(time.RFC3339Nano) + `","Cameras":[]}`
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}

	response := request(t, api, "GET", "/api/status", "")
	var body StatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Supervisor.Running {
		t.Error("a dead pid must not report running")
	}
	if body.Supervisor.State != "running" {
		t.Errorf("the file's own claim should still be reported, got %q", body.Supervisor.State)
	}
	if !strings.Contains(body.Supervisor.Reason, "4242") {
		t.Errorf("reason %q should name the pid", body.Supervisor.Reason)
	}
}
