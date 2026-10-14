package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// leakyStatus is shaped like the recorder's status.json but deliberately
// carries everything that must never reach the browser: a credentialled RTSP
// URL, the restreamer's relay base, and a bare camera address.
const leakyStatus = `{
  "Schema": 1,
  "State": "running",
  "RootPath": "E:\\cameras",
  "Disk": { "FreeGB": 282.58, "ReserveGB": 20 },
  "Restreamer": { "Enabled": true, "RelayBase": "rtsp://127.0.0.1:8554", "Paths": ["frontentrance"] },
  "Events": [
    { "At": "2026-10-12 19:06:17 +03:00", "Text": "open rtsp://camwatch:Sup3rSecret!@192.168.50.32/stream1 failed" },
    { "At": "2026-10-12 19:06:18 +03:00", "Text": "retry rtsp://camwatch:Sup3rSecret%21@192.168.50.32/stream1" }
  ],
  "Cameras": [
    {
      "Name": "Field Front",
      "Record": true,
      "Alive": false,
      "InputMode": "direct",
      "LastError": "ffmpeg: rtsp://camwatch:Sup3rSecret!@192.168.50.32/stream1 refused",
      "Password": "Sup3rSecret!",
      "Username": "camwatch"
    }
  ]
}`

// needles are the shapes a credential or an address can take in a response.
var needles = []string{
	"Sup3rSecret!",
	"Sup3rSecret%21",
	"camwatch:",
	"192.168.50.32",
	"127.0.0.1:8554",
}

func scan(body string) []string {
	var hits []string
	for _, needle := range needles {
		if strings.Contains(body, needle) {
			hits = append(hits, needle)
		}
	}
	return hits
}

func TestScannerPositiveControl(t *testing.T) {
	// A clean scan proves nothing unless the scanner can see a real leak.
	hits := scan(leakyStatus)
	if len(hits) != len(needles) {
		t.Fatalf("positive control did not light up: found %v, want all %v", hits, needles)
	}
}

func TestRedactRemovesCredentialsAndAddresses(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(leakyStatus), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	out, err := json.Marshal(Redact(parsed))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if hits := scan(string(out)); len(hits) != 0 {
		t.Fatalf("redacted status still contains %v\n%s", hits, out)
	}

	// Redaction must not destroy the data the page needs.
	var after map[string]any
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatalf("redacted output is not JSON: %v", err)
	}
	if after["State"] != "running" {
		t.Errorf("State lost: %v", after["State"])
	}
	cams, ok := after["Cameras"].([]any)
	if !ok || len(cams) != 1 {
		t.Fatalf("Cameras lost: %v", after["Cameras"])
	}
	cam := cams[0].(map[string]any)
	if cam["Name"] != "Field Front" {
		t.Errorf("camera name lost: %v", cam["Name"])
	}
	if _, present := cam["Password"]; present {
		t.Error("Password key survived redaction")
	}
	if got := cam["LastError"].(string); !strings.Contains(got, "ffmpeg") {
		t.Errorf("error text lost its meaning: %q", got)
	}
}

func TestStatusEndpointLeaksNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := os.WriteFile(path, []byte("\ufeff"+leakyStatus), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	mux := http.NewServeMux()
	NewAPI(Config{StatusFile: path}, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	response, err := http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	if hits := scan(string(body)); len(hits) != 0 {
		t.Fatalf("/api/status returned %v\n%s", hits, body)
	}

	var payload StatusResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if !payload.StatusFile.Present {
		t.Error("status file reported absent")
	}
	if payload.Status["State"] != "running" {
		t.Errorf("State missing from response: %v", payload.Status["State"])
	}
}

func TestStatusEndpointSurvivesMissingFile(t *testing.T) {
	mux := http.NewServeMux()
	NewAPI(Config{StatusFile: filepath.Join(t.TempDir(), "absent.json")}, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	response, err := http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if response.StatusCode != http.StatusOK {
		t.Fatalf("a missing status file must still render: got %d", response.StatusCode)
	}
	var payload StatusResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if payload.StatusFile.Present || payload.StatusFile.Error == "" {
		t.Errorf("missing file not reported: %+v", payload.StatusFile)
	}
}

func TestParseSchtasksList(t *testing.T) {
	// Taken verbatim from schtasks /FO LIST /V on this machine, including the
	// unescaped quotes in Task To Run that make the CSV format unusable and
	// the OEM-codepage byte in the localised date.
	out := "Folder: \\\r\n" +
		"HostName:                             MOS-EISLEY\r\n" +
		"TaskName:                             \\IPCCTV-Recorder\r\n" +
		"Status:                               Ready\r\n" +
		"Last Run Time:                        12/\xff\xfe/2026 18:55:25\r\n" +
		"Last Result:                          0\r\n" +
		"Task To Run:                          powershell.exe -File \"C:\\x\\start.ps1\"\r\n" +
		"Scheduled Task State:                 Enabled\r\n" +
		"Idle Time:                            Disabled\r\n"

	fields, err := parseSchtasksList(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fields["Status"] != "Ready" {
		t.Errorf("Status = %q", fields["Status"])
	}
	if fields["Last Result"] != "0" {
		t.Errorf("Last Result = %q", fields["Last Result"])
	}
	// The field after Task To Run is where the CSV reader went wrong.
	if fields["Scheduled Task State"] != "Enabled" {
		t.Errorf("Scheduled Task State = %q, want Enabled", fields["Scheduled Task State"])
	}
	if fields["TaskName"] != "\\IPCCTV-Recorder" {
		t.Errorf("TaskName = %q", fields["TaskName"])
	}
}
