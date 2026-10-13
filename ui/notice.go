package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Acknowledging a persistent notice
//
// Two records in status.json outlive the event they describe, on purpose: a
// stop nobody asked for must not be erased by the recovery that followed it,
// or an outage at 08:38 is an outage nobody ever hears about. The cost is a
// banner with no way to say "seen", which is what this fixes.
//
// The acknowledgement is kept in a sidecar beside status.json and never in
// status.json itself. The supervisor rewrites that document every few seconds
// and camui is not one of its writers; anything camui added there would be
// clobbered, and could clobber in turn.
//
// It is kept on the SERVER rather than in the browser so it holds from any
// browser on the machine or the LAN, and survives a reload.
//
// Each acknowledgement stores the acknowledged event's OWN timestamp. A later
// event carries a later timestamp, does not match, and shows - so this
// acknowledges one event and is never a mute switch on the notice.
// ---------------------------------------------------------------------------

// acknowledgedFileName is the sidecar, written beside status.json in _state.
const acknowledgedFileName = "camui-acknowledged.json"

// acknowledgedSchema versions the sidecar, as every other document here is
// versioned.
const acknowledgedSchema = 1

// maxNoticeTimestamp bounds the stored value. The recorder's timestamps are
// about 33 characters ("2026-10-13T08:38:34.4746831+03:00").
const maxNoticeTimestamp = 64

// acknowledgeableNotices are the status.json keys a dismissal may name. A
// closed list, so the sidecar cannot be filled with arbitrary keys by anything
// that can reach the endpoint.
var acknowledgeableNotices = map[string]bool{
	"LastUnexpectedStop": true,
	"LastWindowClose":    true,
}

// acknowledgedDocument is the sidecar's shape: a notice key to the timestamp
// of the event that was acknowledged for it.
type acknowledgedDocument struct {
	Schema  int               `json:"schema"`
	Notices map[string]string `json:"notices"`
}

// AcknowledgeRequest is the body of POST /api/acknowledge. The timestamp is
// sent back by the page rather than looked up here, so what is recorded is the
// event the Operator actually saw - not whatever the file says by the time the
// request lands.
type AcknowledgeRequest struct {
	Notice string `json:"notice"`
	At     string `json:"at"`
}

func (a *API) handleAcknowledge(w http.ResponseWriter, r *http.Request) {
	var request AcknowledgeRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}

	notice := strings.TrimSpace(request.Notice)
	if !acknowledgeableNotices[notice] {
		writeAPIError(w, http.StatusUnprocessableEntity,
			"notice must be one of "+strings.Join(noticeNames(), ", "))
		return
	}
	at := strings.TrimSpace(request.At)
	if at == "" || len(at) > maxNoticeTimestamp {
		writeAPIError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("at must be the notice's own timestamp, 1 to %d characters", maxNoticeTimestamp))
		return
	}

	path := a.cfg.AcknowledgedFile()

	a.ackMu.Lock()
	defer a.ackMu.Unlock()

	document, err := readAcknowledged(path)
	if err != nil {
		// A sidecar that cannot be read is replaced rather than left to block
		// every future dismissal. The only thing lost is an earlier "seen",
		// which the Operator can say again.
		a.log.Warn("replacing an unreadable acknowledgement sidecar", "path", path, "error", err)
		document = acknowledgedDocument{}
	}
	if document.Notices == nil {
		document.Notices = map[string]string{}
	}
	document.Notices[notice] = at

	if err := writeAcknowledged(path, document); err != nil {
		a.log.Error("write acknowledgement sidecar", "path", path, "error", err)
		writeAPIError(w, http.StatusInternalServerError,
			"could not record that as seen: "+ScrubText(err.Error()))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notices": document.Notices})
}

// acknowledgedNotices returns what has been acknowledged, for GET /api/status.
// A missing sidecar is the normal case and is not an error.
func (a *API) acknowledgedNotices() (map[string]string, error) {
	a.ackMu.Lock()
	defer a.ackMu.Unlock()

	document, err := readAcknowledged(a.cfg.AcknowledgedFile())
	if err != nil {
		return nil, err
	}
	return document.Notices, nil
}

// readAcknowledged decodes the sidecar. An absent file is an empty document:
// nothing has been acknowledged yet. Any other failure is returned, and both
// callers then fail in the safe direction - the banner shows.
func readAcknowledged(path string) (acknowledgedDocument, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return acknowledgedDocument{}, nil
	}
	if err != nil {
		return acknowledgedDocument{}, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}

	var document acknowledgedDocument
	if err := json.Unmarshal(stripBOM(raw), &document); err != nil {
		return acknowledgedDocument{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return document, nil
}

// writeAcknowledged replaces the sidecar atomically: a temporary file in the
// same directory, then a rename - which on Windows is MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, the same guarantee the scripts get from
// Move-Item -Force. The temporary file has to share the directory or the
// rename becomes a cross-volume copy.
func writeAcknowledged(path string, document acknowledgedDocument) error {
	document.Schema = acknowledgedSchema

	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	temp, err := os.CreateTemp(dir, acknowledgedFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tempPath := temp.Name()

	_, writeErr := temp.Write(raw)
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tempPath)
		if writeErr != nil {
			return fmt.Errorf("write %s: %w", tempPath, writeErr)
		}
		return fmt.Errorf("close %s: %w", tempPath, closeErr)
	}

	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// noticeNames is the closed list in a stable order, for an error message.
func noticeNames() []string {
	names := make([]string, 0, len(acknowledgeableNotices))
	for name := range acknowledgeableNotices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
