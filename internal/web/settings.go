package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Settings: the camera list and the editable slice of the recorder config.
//
// Reads are file reads. Writes go through camui-apply.ps1, which dot-sources
// cam.common.ps1 and uses Set-CamList and Move-CamFileAtomic - the recorder's
// own writers. That is deliberate: those writers preserve the UTF-8 byte-order
// mark cameras.json ships with, keep every field camui has never heard of, and
// replace the file atomically on the \\wsl.localhost share, where the obvious
// .NET call fails. Reimplementing the three of them in Go would be three new
// chances to get it wrong.
//
// Validation is here, in front of the write, and the whole request is refused
// if any row is wrong. A half-applied camera list is a camera that silently
// stops recording.
// ---------------------------------------------------------------------------

// CameraRow is one camera as the browser sees it. These four fields are what
// the Operator edits; every other field in the file is preserved by the writer
// without ever being sent to the page.
type CameraRow struct {
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Quality string `json:"quality"`
	Record  bool   `json:"record"`
}

// CameraPut is one row coming back. OriginalName identifies which row in the
// file this is, so a rename is a rename rather than a delete and an insert -
// which is what keeps the unknown fields attached to the right camera. It is
// empty for a camera being added.
type CameraPut struct {
	OriginalName string `json:"originalName"`
	Name         string `json:"name"`
	IP           string `json:"ip"`
	Quality      string `json:"quality"`
	Record       bool   `json:"record"`
}

// CamerasResponse is the body of GET /api/cameras and of a successful PUT.
//
// The addresses in it are the only camera addresses that cross this API, and
// they are here because the Operator types them: cameras.json is his file and
// an editor that cannot show what it is editing is not an editor. Everything
// else stays redacted - there are no credentials in this file (they are in the
// DPAPI store, which camui never reads) and no relay or RTSP URL.
type CamerasResponse struct {
	Ok      bool        `json:"ok"`
	Path    string      `json:"path"`
	Cameras []CameraRow `json:"cameras"`
	Changed int         `json:"changed,omitempty"`
	Message string      `json:"message,omitempty"`
}

// CamerasRequest is the body of PUT /api/cameras: the whole desired list, in
// the order it should appear in the file.
type CamerasRequest struct {
	Cameras []CameraPut `json:"cameras"`
}

// ConfigResponse is the body of GET /api/config and of a successful PUT. It
// carries the allow-listed keys and nothing else - not RelayBase, not a path,
// not a credential - so a key that is added to recorder.config.json later
// cannot appear here by accident.
type ConfigResponse struct {
	Ok       bool           `json:"ok"`
	Path     string         `json:"path"`
	Config   map[string]any `json:"config"`
	Editable []string       `json:"editable"`
	Changed  int            `json:"changed,omitempty"`
	Message  string         `json:"message,omitempty"`
}

// ConfigRequest is the body of PUT /api/config.
type ConfigRequest struct {
	Config map[string]any `json:"config"`
}

// editableConfigKeys is the allow-list. A key not on it is not readable and not
// writable through this API. camui-apply.ps1 holds the same list and refuses on
// its own authority, because it is the thing that writes.
var editableConfigKeys = []string{"SegmentSeconds", "ReserveGB", "PruneHysteresisGB", "MaxRetentionDays"}

func (a *API) handleGetCameras(w http.ResponseWriter, r *http.Request) {
	cameras, path, err := a.readCameras()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, CamerasResponse{Ok: true, Path: path, Cameras: cameras})
}

func (a *API) handlePutCameras(w http.ResponseWriter, r *http.Request) {
	setTarget(r, "cameras")
	var request CamerasRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if request.Cameras == nil {
		writeAPIError(w, http.StatusUnprocessableEntity, `the body must be {"cameras": [...]}`)
		return
	}

	if invalid := validateCameras(request.Cameras); len(invalid) > 0 {
		// Nothing is written. The whole request is refused, because applying
		// the valid rows would leave the Operator with a list he did not ask
		// for and no way to tell which half took effect.
		writeFieldErrors(w, plural(len(invalid), "row needs", "rows need")+" attention. Nothing was written.", invalid)
		return
	}

	if !a.actionMu.TryLock() {
		writeAPIError(w, http.StatusConflict, "another action is still running; wait for it to finish")
		return
	}
	defer a.actionMu.Unlock()

	applied, err := a.applySettings("cameras", request)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Re-read, so what the page shows next is what is on disk, not what the
	// page hoped it had written.
	cameras, path, err := a.readCameras()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, CamerasResponse{
		Ok: true, Path: path, Cameras: cameras,
		Changed: applied.Changed, Message: applied.Message,
	})
}

func (a *API) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	config, path, err := a.readRecorderConfig()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ConfigResponse{
		Ok: true, Path: path, Config: config, Editable: editableConfigKeys,
	})
}

func (a *API) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	setTarget(r, "config")
	var request ConfigRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if len(request.Config) == 0 {
		writeAPIError(w, http.StatusUnprocessableEntity, `the body must be {"config": {...}} with at least one key`)
		return
	}

	if invalid := validateRecorderConfig(request.Config); len(invalid) > 0 {
		writeFieldErrors(w, plural(len(invalid), "setting needs", "settings need")+" attention. Nothing was written.", invalid)
		return
	}

	if !a.actionMu.TryLock() {
		writeAPIError(w, http.StatusConflict, "another action is still running; wait for it to finish")
		return
	}
	defer a.actionMu.Unlock()

	applied, err := a.applySettings("config", request)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}

	config, path, err := a.readRecorderConfig()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ConfigResponse{
		Ok: true, Path: path, Config: config, Editable: editableConfigKeys,
		Changed: applied.Changed, Message: applied.Message,
	})
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

func (a *API) readCameras() ([]CameraRow, string, error) {
	path := a.cfg.CamerasFile()

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("read the camera list: %w", scrubErr(err))
	}

	var parsed []map[string]any
	if err := json.Unmarshal(stripBOM(raw), &parsed); err != nil {
		return nil, path, fmt.Errorf("the camera list is not a JSON array of cameras: %w", scrubErr(err))
	}

	rows := make([]CameraRow, 0, len(parsed))
	for _, camera := range parsed {
		record, _ := camera["Record"].(bool)
		rows = append(rows, CameraRow{
			Name:    jsonString(camera["Name"]),
			IP:      jsonString(camera["IP"]),
			Quality: jsonString(camera["Quality"]),
			Record:  record,
		})
	}
	return rows, path, nil
}

// readRecorderConfig returns the allow-listed keys only. A key absent from the
// file comes back as null, which is what MaxRetentionDays legitimately is.
func (a *API) readRecorderConfig() (map[string]any, string, error) {
	path := a.cfg.RecorderConfigFile()

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("read the recorder configuration: %w", scrubErr(err))
	}

	var parsed map[string]any
	if err := json.Unmarshal(stripBOM(raw), &parsed); err != nil {
		return nil, path, fmt.Errorf("the recorder configuration is not a JSON object: %w", scrubErr(err))
	}

	// Built key by key from the allow-list, never by removing keys from the
	// document: a filter that works by subtraction leaks whatever is added to
	// the file next.
	config := make(map[string]any, len(editableConfigKeys))
	for _, key := range editableConfigKeys {
		config[key] = parsed[key]
	}
	return config, path, nil
}

// statusIntervalSeconds is the supervisor's heartbeat cadence, which sets the
// liveness limit. Read from the recorder's config so the page and the CLI use
// the same number; the in-code default matches Get-CamConfigDefault.
func (a *API) statusIntervalSeconds() float64 {
	raw, err := os.ReadFile(a.cfg.RecorderConfigFile())
	if err != nil {
		return defaultStatusIntervalSeconds
	}
	var parsed map[string]any
	if err := json.Unmarshal(stripBOM(raw), &parsed); err != nil {
		return defaultStatusIntervalSeconds
	}
	if value, ok := parsed["StatusIntervalSeconds"].(float64); ok && value > 0 {
		return value
	}
	return defaultStatusIntervalSeconds
}

func findCamera(cameras []CameraRow, name string) int {
	for i, camera := range cameras {
		if strings.EqualFold(camera.Name, name) {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

var (
	// illegalNameChars are the characters Windows forbids in a path segment.
	illegalNameChars = regexp.MustCompile(`[<>:"/\\|?*]`)

	// controlChars catches anything unprintable, which a paste can carry.
	controlChars = regexp.MustCompile(`[\x00-\x1f\x7f]`)

	whitespaceRun = regexp.MustCompile(`\s+`)

	// streamName is the stream path component (cameras.json calls it Quality).
	// It ends up both in an RTSP path and in the generated relay path, so it is
	// held to a conservative set.
	streamName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

	// hostLabel is one RFC 1123 label.
	hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

	// reservedNames mirrors $script:CamReservedNames in cam.common.ps1.
	reservedNames = map[string]bool{
		"CON": true, "PRN": true, "AUX": true, "NUL": true,
		"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
		"COM6": true, "COM7": true, "COM8": true, "COM9": true,
		"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
		"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	}
)

// maxCameraNameLength keeps the name short enough that the segment filenames
// built from it stay well inside the Windows path limit.
const maxCameraNameLength = 48

// validateCameras checks every row and returns every problem found, so the page
// can mark all the bad fields at once instead of one per attempt.
func validateCameras(rows []CameraPut) []FieldError {
	invalid := []FieldError{}
	names := map[string]int{}
	safeNames := map[string]int{}

	for i, row := range rows {
		if message := validateCameraName(row.Name); message != "" {
			invalid = append(invalid, FieldError{Row: i, Field: "name", Message: message})
		} else {
			key := strings.ToUpper(strings.TrimSpace(row.Name))
			safe := strings.ToUpper(safeCameraName(row.Name))

			if first, seen := names[key]; seen {
				invalid = append(invalid, FieldError{Row: i, Field: "name",
					Message: fmt.Sprintf("another camera on row %d already has this name", first+1)})
			} else if first, seen := safeNames[safe]; seen {
				// Checked only when the names themselves differ, so a plain
				// duplicate is reported once rather than twice: two names that
				// differ only in a character the filesystem cannot keep would
				// still share one archive directory, and the second camera's
				// footage would land in the first camera's folder.
				invalid = append(invalid, FieldError{Row: i, Field: "name",
					Message: fmt.Sprintf("this name and the one on row %d would share the same folder on disk (%s)", first+1, safeCameraName(row.Name))})
			} else {
				names[key] = i
				safeNames[safe] = i
			}
		}

		if message := validateHost(row.IP); message != "" {
			invalid = append(invalid, FieldError{Row: i, Field: "ip", Message: message})
		}
		if !streamName.MatchString(strings.TrimSpace(row.Quality)) {
			invalid = append(invalid, FieldError{Row: i, Field: "quality",
				Message: "a stream name is required, and may use only letters, digits, dot, dash and underscore (the cameras serve stream1 and stream2)"})
		}
	}
	return invalid
}

func validateCameraName(name string) string {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return "a name is required"
	case len(trimmed) > maxCameraNameLength:
		return fmt.Sprintf("a name may be at most %d characters", maxCameraNameLength)
	case illegalNameChars.MatchString(trimmed):
		return `a name may not contain < > : " / \ | ? *, because it is also a folder name on disk`
	case controlChars.MatchString(trimmed):
		return "a name may not contain control characters"
	case strings.HasSuffix(trimmed, "."):
		return "a name may not end with a dot, because Windows drops it from the folder name"
	}

	safe := safeCameraName(name)
	if safe == "" {
		return "a name must contain at least one character that is legal in a folder name"
	}
	stem := safe
	if dot := strings.Index(stem, "."); dot >= 0 {
		stem = stem[:dot]
	}
	if reservedNames[strings.ToUpper(stem)] {
		return fmt.Sprintf("%q is a reserved Windows device name and cannot be a folder", stem)
	}
	return ""
}

// safeCameraName mirrors Get-CamSafeName in cam.common.ps1. It exists here only
// to detect two names that would collide on disk; the recorder remains the thing
// that actually derives the folder name.
func safeCameraName(name string) string {
	safe := whitespaceRun.ReplaceAllString(strings.TrimSpace(name), "_")
	safe = illegalNameChars.ReplaceAllString(safe, "")
	safe = controlChars.ReplaceAllString(safe, "")
	return strings.TrimRight(safe, ". ")
}

// validateHost accepts an IP literal or a plausible hostname. It is deliberately
// not a reachability test: a camera that is unplugged still belongs in the list.
func validateHost(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "an address is required - the camera's IP or hostname"
	}
	if net.ParseIP(trimmed) != nil {
		return ""
	}
	if strings.ContainsAny(trimmed, `/\@: `) {
		return "an address is the host only - no scheme, port, path or credentials"
	}
	if len(trimmed) > 253 {
		return "a hostname may be at most 253 characters"
	}
	for _, label := range strings.Split(strings.TrimSuffix(trimmed, "."), ".") {
		if !hostLabel.MatchString(label) {
			return "not a valid IP address or hostname"
		}
	}
	return ""
}

// configRange is the accepted range of one allow-listed numeric key.
type configRange struct {
	min, max float64
	integral bool
	nullable bool
	unit     string
	why      string
}

// configRanges bounds every editable key. The reasons are the ones that make a
// value outside the range a mistake, and they are sent to the page as the
// message, so the Operator is told what would be right rather than only that
// what he typed was wrong.
var configRanges = map[string]configRange{
	"SegmentSeconds": {
		min: 60, max: 3600, integral: true, unit: "seconds",
		why: "shorter than a minute makes thousands of files a day; longer than an hour makes one corrupt segment an hour of lost footage",
	},
	"ReserveGB": {
		min: 1, max: 100000, unit: "GB",
		why: "this is the free space the pruner protects",
	},
	"PruneHysteresisGB": {
		min: 0, max: 10000, unit: "GB",
		why: "this is how far above the reserve the pruner frees before it stops, so it is not re-triggered by every new segment",
	},
	"MaxRetentionDays": {
		min: 1, max: 365, integral: true, nullable: true, unit: "days",
		why: "blank means no age limit, and the reserve alone decides what is kept",
	},
}

func validateRecorderConfig(config map[string]any) []FieldError {
	invalid := []FieldError{}

	// Sorted by the allow-list, not by map order, so the page lists the
	// problems in the order the form shows the fields.
	for _, key := range editableConfigKeys {
		value, present := config[key]
		if !present {
			continue
		}
		if message := validateConfigValue(key, value); message != "" {
			invalid = append(invalid, FieldError{Field: key, Message: message})
		}
	}

	// Anything not on the allow-list is refused by name rather than ignored:
	// silently dropping a key would report success for a change that did not
	// happen.
	for key := range config {
		if _, ok := configRanges[key]; !ok {
			invalid = append(invalid, FieldError{Field: key,
				Message: fmt.Sprintf("%q is not an editable setting; this page can change only %s", key, strings.Join(editableConfigKeys, ", "))})
		}
	}
	return invalid
}

func validateConfigValue(key string, value any) string {
	limits := configRanges[key]

	if value == nil {
		if limits.nullable {
			return ""
		}
		return fmt.Sprintf("a value is required (%s)", limits.why)
	}

	number, ok := value.(float64)
	if !ok {
		return fmt.Sprintf("must be a number of %s", limits.unit)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return fmt.Sprintf("must be a number of %s", limits.unit)
	}
	if limits.integral && number != math.Trunc(number) {
		return fmt.Sprintf("must be a whole number of %s", limits.unit)
	}
	if number < limits.min || number > limits.max {
		return fmt.Sprintf("must be between %s and %s %s - %s",
			trimNumber(limits.min), trimNumber(limits.max), limits.unit, limits.why)
	}
	return ""
}

// trimNumber renders a bound without a trailing .0, and never through the
// locale: strconv is invariant where a %v of a float under a Greek locale in
// some languages is not.
func trimNumber(value float64) string {
	text := fmt.Sprintf("%.2f", value)
	text = strings.TrimRight(text, "0")
	return strings.TrimSuffix(text, ".")
}

func plural(count int, one, many string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, one)
	}
	return fmt.Sprintf("%d %s", count, many)
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// applyResult is what camui-apply.ps1 writes back.
type applyResult struct {
	Ok      bool   `json:"ok"`
	Error   string `json:"error"`
	Changed int    `json:"changed"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

// applySettings runs the embedded writer with the request in a file and the
// result in another.
//
// Files rather than arguments and stdout: no value then has to survive a
// command line, a quoting layer or the console codepage - which on this machine
// is a Greek OEM page that turns any non-ASCII output into mojibake.
//
// The script is written to a fresh temporary directory per call and removed
// afterwards, so camui.exe stays a single self-contained binary with nothing to
// deploy beside it.
func (a *API) applySettings(kind string, request any) (applyResult, error) {
	dir, err := os.MkdirTemp("", "camui-apply-")
	if err != nil {
		return applyResult{}, fmt.Errorf("create a working directory for the settings writer: %w", scrubErr(err))
	}
	defer os.RemoveAll(dir)

	scriptPath := filepath.Join(dir, "camui-apply.ps1")
	inputPath := filepath.Join(dir, "request.json")
	outputPath := filepath.Join(dir, "result.json")

	if err := os.WriteFile(scriptPath, []byte(applyScript), 0o600); err != nil {
		return applyResult{}, fmt.Errorf("write the settings writer: %w", scrubErr(err))
	}
	body, err := json.Marshal(request)
	if err != nil {
		return applyResult{}, fmt.Errorf("encode the settings request: %w", scrubErr(err))
	}
	if err := os.WriteFile(inputPath, body, 0o600); err != nil {
		return applyResult{}, fmt.Errorf("write the settings request: %w", scrubErr(err))
	}

	result := a.runScript(settingsTimeout, scriptPath,
		"-Repo", a.cfg.ScriptDir, "-Kind", kind, "-InputPath", inputPath, "-OutputPath", outputPath)

	applied := applyResult{}
	if raw, readErr := os.ReadFile(outputPath); readErr == nil {
		if err := json.Unmarshal(stripBOM(raw), &applied); err != nil {
			a.log.Error("settings writer result unreadable", "kind", kind, "error", err)
		}
	}

	switch {
	case applied.Error != "":
		// The script's own message, which names the actual problem.
		return applied, fmt.Errorf("%s", ScrubText(applied.Error))
	case result.Err != nil:
		return applied, fmt.Errorf("the settings writer could not be run: %s", ScrubText(result.Err.Error()))
	case result.ExitCode != 0 || !applied.Ok:
		detail := strings.Join(result.Output, "; ")
		if detail == "" {
			detail = fmt.Sprintf("exit code %d with no output", result.ExitCode)
		}
		return applied, fmt.Errorf("the settings writer failed: %s", detail)
	}

	a.log.Info("settings written", "kind", kind, "changed", applied.Changed)
	return applied, nil
}

// scrubErr keeps a path or address in a filesystem error from reaching the page
// with a camera address in it.
func scrubErr(err error) error {
	return fmt.Errorf("%s", ScrubText(err.Error()))
}
