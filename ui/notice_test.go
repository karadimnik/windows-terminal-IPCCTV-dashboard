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

// stoppedStatus carries both persistent notices, each with its own timestamp.
// That is the shape the recorder leaves behind: Set-CamStatusUnexpectedStop and
// Set-CamStatusWindowClosed each patch one record into the live document.
const stoppedStatus = `{
  "Schema": 1,
  "State": "running",
  "StopKind": "",
  "SupervisorPid": 85316,
  "UpdatedAt": "2026-10-14T00:25:35.6298897+03:00",
  "LastUnexpectedStop": {
    "At": "2026-10-13T08:38:34.4746831+03:00",
    "Reason": "the supervisor (pid 14156) stopped after 31s without being asked to (exit -1)",
    "SupervisorPid": 14156,
    "DetectedBy": "nvr host"
  },
  "LastWindowClose": {
    "At": "2026-10-12T20:30:11.1020304+03:00",
    "Reason": "the NVR window was closed",
    "WindowTitle": "IPCCTV NVR",
    "DetectedBy": "nvr warden"
  }
}`

const firstStopAt = "2026-10-13T08:38:34.4746831+03:00"
const windowCloseAt = "2026-10-12T20:30:11.1020304+03:00"

// noticeFixture writes a status file and returns a config pointing at it,
// exactly as the running camui is configured: the sidecar is derived from the
// status file's own directory.
func noticeFixture(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "_state", "status.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(stoppedStatus), 0o600); err != nil {
		t.Fatalf("write status: %v", err)
	}
	return Config{StatusFile: path}
}

// serve starts a server over cfg. Called twice in a test it gives two
// independent API instances over the same files, which is what "a different
// browser session" reduces to on the server side.
func serve(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewAPI(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func acknowledge(t *testing.T, server *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	response, err := http.Post(server.URL+"/api/acknowledge", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("response is not JSON (%d): %s", response.StatusCode, raw)
	}
	return response.StatusCode, payload
}

func statusAcknowledged(t *testing.T, server *httptest.Server) map[string]string {
	t.Helper()
	response, err := http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, raw)
	}

	var payload StatusResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return payload.Acknowledged
}

// A dismissal has to outlive the browser that made it, so it is stored beside
// status.json and read back by any later session.
func TestAcknowledgementSurvivesAFreshSession(t *testing.T) {
	cfg := noticeFixture(t)

	if got := statusAcknowledged(t, serve(t, cfg)); len(got) != 0 {
		t.Fatalf("nothing has been acknowledged yet, got %v", got)
	}

	code, payload := acknowledge(t, serve(t, cfg),
		`{"notice":"LastUnexpectedStop","at":"`+firstStopAt+`"}`)
	if code != http.StatusOK || payload["ok"] != true {
		t.Fatalf("acknowledge returned %d: %v", code, payload)
	}

	// A new API instance over the same files - no shared memory, no cookie,
	// no browser storage - must see it.
	got := statusAcknowledged(t, serve(t, cfg))
	if got["LastUnexpectedStop"] != firstStopAt {
		t.Fatalf("a fresh session does not see the dismissal: %v", got)
	}
}

// The criterion the whole design turns on: a dismissal names ONE event, so a
// later event is not hidden by it.
func TestAcknowledgementIsKeyedToTheEventTimestamp(t *testing.T) {
	cfg := noticeFixture(t)
	server := serve(t, cfg)

	if code, payload := acknowledge(t, server,
		`{"notice":"LastUnexpectedStop","at":"`+firstStopAt+`"}`); code != http.StatusOK {
		t.Fatalf("acknowledge returned %d: %v", code, payload)
	}

	// A later unexpected stop replaces the record in status.json, as the
	// recorder's own patch does.
	const laterStopAt = "2026-10-14T03:04:05.1234567+03:00"
	raw, err := os.ReadFile(cfg.StatusFile)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	updated := strings.Replace(string(raw), firstStopAt, laterStopAt, 1)
	if updated == string(raw) {
		t.Fatal("the fixture timestamp was not replaced")
	}
	if err := os.WriteFile(cfg.StatusFile, []byte(updated), 0o600); err != nil {
		t.Fatalf("write status: %v", err)
	}

	got := statusAcknowledged(t, server)
	if got["LastUnexpectedStop"] != firstStopAt {
		t.Fatalf("the stored acknowledgement changed: %v", got)
	}
	if got["LastUnexpectedStop"] == laterStopAt {
		t.Fatal("the later event counts as acknowledged, which would make this a mute switch")
	}
}

// Both notices are of the same class and each is acknowledged independently.
func TestEachNoticeIsAcknowledgedIndependently(t *testing.T) {
	cfg := noticeFixture(t)
	server := serve(t, cfg)

	acknowledge(t, server, `{"notice":"LastWindowClose","at":"`+windowCloseAt+`"}`)
	got := statusAcknowledged(t, server)
	if got["LastWindowClose"] != windowCloseAt {
		t.Fatalf("window close not acknowledged: %v", got)
	}
	if _, present := got["LastUnexpectedStop"]; present {
		t.Fatalf("dismissing one notice acknowledged the other: %v", got)
	}

	acknowledge(t, server, `{"notice":"LastUnexpectedStop","at":"`+firstStopAt+`"}`)
	got = statusAcknowledged(t, server)
	if got["LastUnexpectedStop"] != firstStopAt || got["LastWindowClose"] != windowCloseAt {
		t.Fatalf("the second dismissal lost the first: %v", got)
	}
}

// camui must never write to the recorder's status document: the supervisor
// rewrites it every few seconds and would clobber anything added there.
func TestAcknowledgementNeverTouchesStatusJSON(t *testing.T) {
	cfg := noticeFixture(t)
	before, err := os.ReadFile(cfg.StatusFile)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}

	server := serve(t, cfg)
	acknowledge(t, server, `{"notice":"LastUnexpectedStop","at":"`+firstStopAt+`"}`)
	acknowledge(t, server, `{"notice":"LastWindowClose","at":"`+windowCloseAt+`"}`)

	after, err := os.ReadFile(cfg.StatusFile)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("status.json was modified")
	}

	// The sidecar is beside it, and is the only new file.
	sidecar := cfg.AcknowledgedFile()
	if sidecar != filepath.Join(filepath.Dir(cfg.StatusFile), "camui-acknowledged.json") {
		t.Fatalf("sidecar is not beside status.json: %s", sidecar)
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.StatusFile))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 {
		t.Fatalf("expected status.json and the sidecar, found %v", names)
	}
}

func TestAcknowledgeRefusesWhatItCannotStore(t *testing.T) {
	server := serve(t, noticeFixture(t))

	for _, probe := range []struct {
		name string
		body string
	}{
		{"an unknown notice", `{"notice":"Everything","at":"` + firstStopAt + `"}`},
		{"no notice", `{"at":"` + firstStopAt + `"}`},
		{"no timestamp", `{"notice":"LastUnexpectedStop"}`},
		{"a blank timestamp", `{"notice":"LastUnexpectedStop","at":"   "}`},
		{"an overlong timestamp", `{"notice":"LastUnexpectedStop","at":"` + strings.Repeat("x", maxNoticeTimestamp+1) + `"}`},
	} {
		code, payload := acknowledge(t, server, probe.body)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s returned %d, want 422: %v", probe.name, code, payload)
		}
	}

	if got := statusAcknowledged(t, server); len(got) != 0 {
		t.Fatalf("a refused request still wrote something: %v", got)
	}
}

// An unreadable sidecar must fail in the direction that shows the banner: a
// notice about an outage is not something to lose to a parse error.
func TestACorruptSidecarAcknowledgesNothingAndIsReplaced(t *testing.T) {
	cfg := noticeFixture(t)
	if err := os.WriteFile(cfg.AcknowledgedFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	server := serve(t, cfg)
	if got := statusAcknowledged(t, server); len(got) != 0 {
		t.Fatalf("a corrupt sidecar reported acknowledgements: %v", got)
	}

	// And it must not block every future dismissal.
	if code, payload := acknowledge(t, server,
		`{"notice":"LastUnexpectedStop","at":"`+firstStopAt+`"}`); code != http.StatusOK {
		t.Fatalf("acknowledge over a corrupt sidecar returned %d: %v", code, payload)
	}
	if got := statusAcknowledged(t, server); got["LastUnexpectedStop"] != firstStopAt {
		t.Fatalf("the replacement sidecar does not hold the dismissal: %v", got)
	}
}

func TestSidecarIsValidJSONWithItsSchema(t *testing.T) {
	cfg := noticeFixture(t)
	acknowledge(t, serve(t, cfg), `{"notice":"LastUnexpectedStop","at":"`+firstStopAt+`"}`)

	raw, err := os.ReadFile(cfg.AcknowledgedFile())
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var document acknowledgedDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("sidecar is not JSON: %v\n%s", err, raw)
	}
	if document.Schema != acknowledgedSchema {
		t.Errorf("schema %d, want %d", document.Schema, acknowledgedSchema)
	}
	if document.Notices["LastUnexpectedStop"] != firstStopAt {
		t.Errorf("sidecar holds %v", document.Notices)
	}
}
