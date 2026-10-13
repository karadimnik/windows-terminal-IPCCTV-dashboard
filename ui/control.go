package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Control actions
//
// Every state change goes through the existing scripts. camui starts nothing,
// stops nothing and kills nothing itself: cam.ps1 already owns the process
// inventory, the keep list and the ordering that a stop has to respect, and a
// second implementation of that in Go would be a second thing to get wrong.
//
// Shelling out costs about a second of PowerShell startup, which is why it is
// reserved for ACTIONS. Status is a file read - see api.go.
// ---------------------------------------------------------------------------

// controlTimeout bounds cam.ps1. A start waits for the relay to serve before it
// opens the dashboard, and a stop gives the orderly path 20 s before it stops
// whatever outlived it, so the budget has to be generous.
const controlTimeout = 180 * time.Second

// recordToggleTimeout bounds cam.recorder.ps1 -Enable/-Disable, which rewrites
// cameras.json and returns.
const recordToggleTimeout = 60 * time.Second

// settingsTimeout bounds the settings writer.
const settingsTimeout = 60 * time.Second

// maxOutputLines caps what an action's console output can add to a response.
const maxOutputLines = 200

// ActionRequest is the body of POST /api/control.
type ActionRequest struct {
	Scope  string `json:"scope"`
	Action string `json:"action"`
}

// RecordRequest is the body of POST /api/cameras/{name}/record.
type RecordRequest struct {
	Enabled *bool `json:"enabled"`
}

// ActionResponse reports what was run and what it said. The output is scrubbed
// like every other response: cam.ps1 -Status prints the relay's rtsp:// base,
// and an action's error text can carry a camera address.
type ActionResponse struct {
	Ok         bool     `json:"ok"`
	Action     string   `json:"action"`
	Scope      string   `json:"scope,omitempty"`
	Camera     string   `json:"camera,omitempty"`
	Command    string   `json:"command"`
	ExitCode   int      `json:"exitCode"`
	DurationMs int64    `json:"durationMs"`
	Output     []string `json:"output"`
	Error      string   `json:"error,omitempty"`
}

// psResult is the outcome of one PowerShell invocation.
type psResult struct {
	Command  string
	ExitCode int
	Output   []string
	Err      error
	Duration time.Duration
}

func (a *API) handleControl(w http.ResponseWriter, r *http.Request) {
	var request ActionRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}

	scope := strings.ToLower(strings.TrimSpace(request.Scope))
	action := strings.ToLower(strings.TrimSpace(request.Action))

	var scopeFlag []string
	switch scope {
	case "all":
		scopeFlag = nil
	case "recorder":
		scopeFlag = []string{"-Recorder"}
	case "dashboard":
		scopeFlag = []string{"-Dashboard"}
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "scope must be all, recorder or dashboard")
		return
	}

	var actionFlag string
	switch action {
	case "start":
		actionFlag = "-Start"
	case "stop":
		actionFlag = "-Stop"
	default:
		writeAPIError(w, http.StatusUnprocessableEntity, "action must be start or stop")
		return
	}

	// One action at a time. Two stops racing each other, or a start racing a
	// stop, is how a half-stopped system happens.
	if !a.actionMu.TryLock() {
		writeAPIError(w, http.StatusConflict, "another action is still running; wait for it to finish")
		return
	}
	defer a.actionMu.Unlock()

	result := a.runScript(controlTimeout, "cam.ps1", append([]string{actionFlag}, scopeFlag...)...)
	writeJSON(w, actionStatus(result), ActionResponse{
		Ok:         result.Err == nil && result.ExitCode == 0,
		Action:     action,
		Scope:      scope,
		Command:    result.Command,
		ExitCode:   result.ExitCode,
		DurationMs: result.Duration.Milliseconds(),
		Output:     result.Output,
		Error:      errorText(result),
	})
}

func (a *API) handleRecordToggle(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if strings.TrimSpace(name) == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "no camera name in the path")
		return
	}

	var request RecordRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	if request.Enabled == nil {
		writeAPIError(w, http.StatusUnprocessableEntity, `the body must be {"enabled": true} or {"enabled": false}`)
		return
	}

	// Resolved here rather than left to the script, which exits 1 and prints a
	// list of valid targets - useful at a prompt, not to a page.
	cameras, _, err := a.readCameras()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if findCamera(cameras, name) < 0 {
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("no camera named %q is in cameras.json", name))
		return
	}

	flag := "-Disable"
	if *request.Enabled {
		flag = "-Enable"
	}

	if !a.actionMu.TryLock() {
		writeAPIError(w, http.StatusConflict, "another action is still running; wait for it to finish")
		return
	}
	defer a.actionMu.Unlock()

	result := a.runScript(recordToggleTimeout, "cam.recorder.ps1", flag, name)
	writeJSON(w, actionStatus(result), ActionResponse{
		Ok:         result.Err == nil && result.ExitCode == 0,
		Action:     map[bool]string{true: "enable", false: "disable"}[*request.Enabled],
		Camera:     name,
		Command:    result.Command,
		ExitCode:   result.ExitCode,
		DurationMs: result.Duration.Milliseconds(),
		Output:     result.Output,
		Error:      errorText(result),
	})
}

func actionStatus(result psResult) int {
	if result.Err != nil {
		return http.StatusBadGateway
	}
	if result.ExitCode != 0 {
		return http.StatusBadGateway
	}
	return http.StatusOK
}

func errorText(result psResult) string {
	if result.Err != nil {
		return ScrubText(result.Err.Error())
	}
	if result.ExitCode != 0 {
		return fmt.Sprintf("the script exited with code %d", result.ExitCode)
	}
	return ""
}

// runPowerShell runs one of the repository's scripts under Windows PowerShell
// and returns its exit code and scrubbed console output.
//
// -File rather than -Command: every argument here is a switch or a single
// string, and -File passes those without a quoting layer. (An ARRAY parameter
// would need -Command: powershell.exe -File binds -Checks a,b as one string.)
// -NonInteractive turns a prompt into an error instead of a hang, which is what
// a request waiting on it needs.
func runPowerShell(scriptDir string, timeout time.Duration, script string, args ...string) psResult {
	// An absolute path is used as given: the settings writer is embedded in the
	// binary and runs from a temporary directory, not from the repository.
	path := script
	if !filepath.IsAbs(path) {
		path = filepath.Join(scriptDir, script)
	}
	full := append([]string{
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path,
	}, args...)

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, powerShellExe, full...)
	cmd.Dir = scriptDir
	hideChildWindow(cmd)

	raw, err := cmd.CombinedOutput()
	result := psResult{
		Command:  filepath.Base(script) + " " + strings.Join(args, " "),
		Output:   scrubLines(string(raw)),
		Duration: time.Since(started),
	}
	if ctx.Err() != nil {
		result.Err = fmt.Errorf("%s did not finish within %s", script, timeout)
		return result
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exit.ExitCode()
		} else {
			result.Err = err
		}
	}
	return result
}

// powerShellExe is Windows PowerShell 5.1 by name. It is resolved by PATH; the
// scripts are 5.1 and must not be handed to pwsh.
const powerShellExe = "powershell.exe"

// scrubLines turns console output into bounded, redacted, valid-UTF-8 lines.
// schtasks and ffmpeg both print in the console codepage, so a byte sequence
// that is not UTF-8 reaches here and would otherwise break the JSON encoder.
func scrubLines(text string) []string {
	lines := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, ScrubText(strings.ToValidUTF8(line, "")))
		if len(lines) == maxOutputLines {
			lines = append(lines, fmt.Sprintf("... output truncated at %d lines", maxOutputLines))
			break
		}
	}
	return lines
}

// ---------------------------------------------------------------------------
// Shared HTTP helpers
// ---------------------------------------------------------------------------

// maxRequestBody bounds a PUT. The camera list and the config subset are both
// small; anything larger is a mistake or an attack, not a settings change.
const maxRequestBody = 256 * 1024

// APIError is the shape every refusal takes, so the page has one place to look
// for something to show the Operator.
type APIError struct {
	Ok      bool         `json:"ok"`
	Error   string       `json:"error"`
	Invalid []FieldError `json:"invalid,omitempty"`
}

// FieldError names the field that was wrong and says what would be right.
type FieldError struct {
	Row     int    `json:"row"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeAPIError(w, http.StatusBadRequest, "the request body is not the JSON this endpoint expects: "+ScrubText(err.Error()))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, APIError{Error: message})
}

func writeFieldErrors(w http.ResponseWriter, message string, invalid []FieldError) {
	writeJSON(w, http.StatusUnprocessableEntity, APIError{Error: message, Invalid: invalid})
}
