package web

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"holocron/internal/cli"
)

// The Admin tab's API (/api/admin/*, admins only: access.go). Every route is a cli.Action named
// web.admin.<noun>.<verb> and calls the helper the matching `holocron` command calls: UserAdmin and
// the TLS facade of this package, and AdminTools for what lives with the command line (the
// configuration allow-list, the secrets store, the pruner, the archive, log levels, autostart).
// Secrets travel only in POST bodies and never come back: no password, hash, key or camera login
// is ever in an answer.

// Action names of the Admin tab. adminActions registers them; Audit marks the reads that are
// security-relevant (who has an account, the certificate authority, the secret names, autostart).
const (
	ActionAdminConfigList   = "web.admin.config.list"
	ActionAdminConfigSet    = "web.admin.config.set"
	ActionAdminConfigUnset  = "web.admin.config.unset"
	ActionAdminUsersList    = "web.admin.users.list"
	ActionAdminUsersAdd     = "web.admin.users.add"
	ActionAdminUsersPasswd  = "web.admin.users.passwd"
	ActionAdminUsersRole    = "web.admin.users.role"
	ActionAdminUsersRemove  = "web.admin.users.remove"
	ActionAdminUsersUnlock  = "web.admin.users.unlock"
	ActionAdminSecretStatus = "web.admin.secret.status"
	ActionAdminSecretSet    = "web.admin.secret.set"
	ActionAdminTLSCA        = "web.admin.tls.ca"
	ActionAdminTLSPrint     = "web.admin.tls.print"
	ActionAdminTLSRotate    = "web.admin.tls.rotate"
	ActionAdminPrune        = "web.admin.prune.preview"
	ActionAdminArchive      = "web.admin.archive.stats"
	ActionAdminLogLevels    = "web.admin.log.levels"
	ActionAdminLogLevel     = "web.admin.log.level"
	ActionAdminAutostart    = "web.admin.autostart.status"
	ActionAdminAutostartSet = "web.admin.autostart.set"
)

// adminActions: name, mutating, audited read.
var adminActions = []struct {
	name            string
	mutating, audit bool
}{
	{ActionAdminConfigList, false, false}, {ActionAdminConfigSet, true, true}, {ActionAdminConfigUnset, true, true},
	{ActionAdminUsersList, false, true}, {ActionAdminUsersAdd, true, true}, {ActionAdminUsersPasswd, true, true},
	{ActionAdminUsersRole, true, true}, {ActionAdminUsersRemove, true, true}, {ActionAdminUsersUnlock, true, true},
	{ActionAdminSecretStatus, false, true}, {ActionAdminSecretSet, true, true},
	{ActionAdminTLSCA, false, true}, {ActionAdminTLSPrint, false, true}, {ActionAdminTLSRotate, true, true},
	{ActionAdminPrune, false, false}, {ActionAdminArchive, false, false},
	{ActionAdminLogLevels, false, false}, {ActionAdminLogLevel, true, true},
	{ActionAdminAutostart, false, true}, {ActionAdminAutostartSet, true, true},
}

func init() {
	for _, a := range adminActions {
		cli.Register(cli.Action{Name: a.name, Component: "web", Mutating: a.mutating, Audit: a.audit, Run: runHTTPCall})
	}
}

// AdminTools is what the Admin tab needs from the command line's own helpers. The holocron web
// role supplies it (cmd/holocron, over the functions its commands call); camui.exe has none, and
// those sections answer 501. An error that IsInputError answers 422, one wrapping cli.ErrRefused
// 409, any other 500. Messages are the command line's own text.
type AdminTools interface {
	// ConfigKeys is the allow-list of `holocron config set` with values, defaults and rules.
	ConfigKeys(ctx context.Context) ([]ConfigKey, error)
	// SetConfigKey is `holocron config set <key> <value>` (value nil: `config unset <key>`); a
	// non-empty invalid means the document would not validate and nothing was written.
	SetConfigKey(ctx context.Context, key string, value *string) (message string, invalid []FieldError, err error)
	// CameraLogin says whether camera.user and camera.password are in the store: never a value.
	CameraLogin(ctx context.Context) (CameraLoginState, error)
	// SetCameraLogin is `holocron secret set camera.user|camera.password` (an empty one is kept).
	SetCameraLogin(ctx context.Context, user, password string) (message string, err error)
	// PrunePreview is `holocron prune --dry-run --json`: the decision, nothing deleted.
	PrunePreview(ctx context.Context) (any, error)
	// ArchiveStats is `holocron archive stats --json`.
	ArchiveStats(ctx context.Context) (any, error)
	// LogLevels are the running supervisor's levels and the configured ones.
	LogLevels(ctx context.Context) (any, error)
	// SetLogLevel is `holocron log level <component|all> <level> [--save]`.
	SetLogLevel(ctx context.Context, component, level string, save bool) (message string, err error)
	// Autostart is `holocron autostart status --json`.
	Autostart(ctx context.Context) (any, error)
	// SetAutostart is `holocron autostart enable|disable [--i-know-old-tasks-are-enabled]`.
	SetAutostart(ctx context.Context, on, oldTasksOK bool) (message string, err error)
}

// ConfigKey is one row of the settings-keys section.
type ConfigKey struct {
	Key     string `json:"key"`
	Value   any    `json:"value"`   // the effective value (URLs with a login masked)
	Default any    `json:"default"` // the built-in default
	Set     bool   `json:"set"`     // the file names it (unset goes back to the default)
	Rule    string `json:"rule"`
	Restart bool   `json:"restart"`           // a running supervisor applies it only at its next start
	Pattern bool   `json:"pattern,omitempty"` // logging.levels.*: the page asks for the component
	Warning string `json:"warning,omitempty"`
}

// CameraLoginState is what the page may know about the camera login.
type CameraLoginState struct {
	Backend    string `json:"backend"`
	User       bool   `json:"user"`
	Password   bool   `json:"password"`
	FromEnv    bool   `json:"fromEnvironment"` // HOLOCRON_CAMERA_USER and _PASSWORD are set and win over the store
	StoreError string `json:"storeError,omitempty"`
	ReadOnly   bool   `json:"readOnly"` // the env backend: nothing can be stored from here
}

// BadInput is an AdminTools refusal of what the caller supplied (422).
func BadInput(format string, args ...any) error { return inputError{fmt.Sprintf(format, args...)} }

// adminMaxSecret is the secrets store's value limit (as `holocron secret set`).
const adminMaxSecret = 4096

func (a *API) adminRoutes(mux *http.ServeMux) {
	handleAction(mux, a.runner, "GET /api/admin/config", ActionAdminConfigList, a.handleAdminConfigList)
	handleAction(mux, a.runner, "POST /api/admin/config/set", ActionAdminConfigSet, a.handleAdminConfigSet)
	handleAction(mux, a.runner, "POST /api/admin/config/unset", ActionAdminConfigUnset, a.handleAdminConfigUnset)
	handleAction(mux, a.runner, "GET /api/admin/users", ActionAdminUsersList, a.handleAdminUsers)
	handleAction(mux, a.runner, "POST /api/admin/users/add", ActionAdminUsersAdd, a.handleAdminUserAdd)
	handleAction(mux, a.runner, "POST /api/admin/users/password", ActionAdminUsersPasswd, a.handleAdminUserPassword)
	handleAction(mux, a.runner, "POST /api/admin/users/role", ActionAdminUsersRole, a.handleAdminUserRole)
	handleAction(mux, a.runner, "POST /api/admin/users/remove", ActionAdminUsersRemove, a.handleAdminUserRemove)
	handleAction(mux, a.runner, "POST /api/admin/users/unlock", ActionAdminUsersUnlock, a.handleAdminUserUnlock)
	handleAction(mux, a.runner, "GET /api/admin/camera-login", ActionAdminSecretStatus, a.handleAdminCameraLogin)
	handleAction(mux, a.runner, "POST /api/admin/camera-login", ActionAdminSecretSet, a.handleAdminCameraLoginSet)
	handleAction(mux, a.runner, "GET /api/admin/tls", ActionAdminTLSCA, a.handleAdminTLS)
	handleAction(mux, a.runner, "GET /api/admin/tls/ca.crt", ActionAdminTLSPrint, a.handleAdminTLSCert)
	handleAction(mux, a.runner, "POST /api/admin/tls/rotate", ActionAdminTLSRotate, a.handleAdminTLSRotate)
	handleAction(mux, a.runner, "GET /api/admin/prune", ActionAdminPrune, a.handleAdminPrune)
	handleAction(mux, a.runner, "GET /api/admin/archive", ActionAdminArchive, a.handleAdminArchive)
	handleAction(mux, a.runner, "GET /api/admin/log", ActionAdminLogLevels, a.handleAdminLogLevels)
	handleAction(mux, a.runner, "POST /api/admin/log/level", ActionAdminLogLevel, a.handleAdminLogLevel)
	handleAction(mux, a.runner, "GET /api/admin/autostart", ActionAdminAutostart, a.handleAdminAutostart)
	handleAction(mux, a.runner, "POST /api/admin/autostart", ActionAdminAutostartSet, a.handleAdminAutostartSet)
}

// adminFailed answers an error of a helper with its status and its own words.
func adminFailed(w http.ResponseWriter, err error) {
	msg := strings.TrimPrefix(err.Error(), cli.ErrRefused.Error()+": ")
	switch {
	case IsInputError(err):
		writeAPIError(w, http.StatusUnprocessableEntity, msg)
	case errors.Is(err, ErrNoSuchUser):
		writeAPIError(w, http.StatusNotFound, msg)
	case errors.Is(err, ErrUserExists), errors.Is(err, ErrLastAdmin), errors.Is(err, cli.ErrRefused):
		writeAPIError(w, http.StatusConflict, msg)
	default:
		writeAPIError(w, http.StatusInternalServerError, ScrubText(msg))
	}
}

func adminDone(w http.ResponseWriter, message string, extra map[string]any) {
	body := map[string]any{"ok": true, "message": strings.TrimSpace(message)}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, http.StatusOK, body)
}

// tools answers 501 when no AdminTools was supplied (camui.exe).
func (a *API) tools(w http.ResponseWriter) (AdminTools, bool) {
	if a.admin == nil {
		writeAPIError(w, http.StatusNotImplemented, "this section needs the holocron web app (holocron up); camui.exe has no Admin tools")
		return nil, false
	}
	return a.admin, true
}

// exclusive runs one change at a time, sharing the lock of Settings and the controls.
func (a *API) exclusive(w http.ResponseWriter) (func(), bool) {
	if !a.actionMu.TryLock() {
		writeAPIError(w, http.StatusConflict, "another action is still running; wait for it to finish")
		return nil, false
	}
	return a.actionMu.Unlock, true
}

// ---------------------------------------------------------------------------
// Settings keys
// ---------------------------------------------------------------------------

func (a *API) handleAdminConfigList(w http.ResponseWriter, r *http.Request) {
	t, ok := a.tools(w)
	if !ok {
		return
	}
	keys, err := t.ConfigKeys(actionCtx(r))
	if err != nil {
		adminFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": keys, "path": a.cfg.RecorderConfigFile()})
}

type configKeyRequest struct {
	Key   string  `json:"key"`
	Value *string `json:"value"`
}

func (a *API) handleAdminConfigSet(w http.ResponseWriter, r *http.Request) { a.configKey(w, r, true) }

func (a *API) handleAdminConfigUnset(w http.ResponseWriter, r *http.Request) {
	a.configKey(w, r, false)
}

func (a *API) configKey(w http.ResponseWriter, r *http.Request, set bool) {
	var req configKeyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	setTarget(r, cleanLoggedName(req.Key))
	switch {
	case strings.TrimSpace(req.Key) == "":
		writeAPIError(w, http.StatusUnprocessableEntity, "name the key")
		return
	case set && req.Value == nil:
		writeAPIError(w, http.StatusUnprocessableEntity, `give a value ("value": "...", as typed after holocron config set <key>)`)
		return
	case !set:
		req.Value = nil
	}
	t, ok := a.tools(w)
	if !ok {
		return
	}
	unlock, ok := a.exclusive(w)
	if !ok {
		return
	}
	defer unlock()
	msg, invalid, err := t.SetConfigKey(actionCtx(r), req.Key, req.Value)
	switch {
	case len(invalid) > 0:
		writeFieldErrors(w, "The configuration would not be valid. Nothing was written.", invalid)
	case err != nil:
		adminFailed(w, err)
	default:
		adminDone(w, msg, nil)
	}
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// users is the facade the Admin tab uses: over the LAN listener's own running service when there
// is one (lockouts visible, an unlock applied at once), else over the auth folder; acting for the
// request, whose own session survives a change of the caller's password or role.
func (a *API) users(r *http.Request) *UserAdmin {
	a.usersOnce.Do(func() {
		if a.auth != nil {
			a.userAdmin = &UserAdmin{svc: a.auth, live: true}
		} else {
			a.userAdmin = usersWith(a.cfg.AuthDir, a.log, defaultPasswordHasher)
		}
		a.userAdmin.web = true
	})
	u := a.userAdmin
	if _, signedIn := userFrom(r); signedIn {
		if c, err := r.Cookie(sessionCookie); err == nil {
			return u.sparing(c.Value)
		}
	}
	return u
}

func (a *API) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	list, err := a.users(r).List()
	if err != nil {
		adminFailed(w, err)
		return
	}
	self := ""
	if u, ok := userFrom(r); ok {
		self = u.Username
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "users": list, "self": self, "roles": []string{roleAdmin, roleViewer}, "lockoutsKnown": a.auth != nil})
}

type userRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (a *API) userRequest(w http.ResponseWriter, r *http.Request) (userRequest, bool) {
	var req userRequest
	if !decodeJSONBody(w, r, &req) {
		return req, false
	}
	setTarget(r, cleanLoggedName(req.Username))
	return req, true
}

func (a *API) handleAdminUserAdd(w http.ResponseWriter, r *http.Request) {
	req, ok := a.userRequest(w, r)
	if !ok {
		return
	}
	role := req.Role
	if role == "" {
		role = roleAdmin
	}
	if err := a.users(r).AddRole(actorOf(r), req.Username, req.Password, role); err != nil {
		adminFailed(w, err)
		return
	}
	adminDone(w, fmt.Sprintf("created user %s (role %s)", req.Username, strings.ToLower(role)), nil)
}

func (a *API) handleAdminUserPassword(w http.ResponseWriter, r *http.Request) {
	req, ok := a.userRequest(w, r)
	if !ok {
		return
	}
	if err := a.users(r).SetPassword(actorOf(r), req.Username, req.Password); err != nil {
		adminFailed(w, err)
		return
	}
	adminDone(w, "changed the password of "+req.Username+"; that user's other sessions were ended", nil)
}

func (a *API) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	req, ok := a.userRequest(w, r)
	if !ok {
		return
	}
	changed, err := a.users(r).SetRole(actorOf(r), req.Username, req.Role)
	if err != nil {
		adminFailed(w, err)
		return
	}
	msg := req.Username + " has that role already; nothing changed"
	if changed {
		msg = req.Username + " is now " + strings.ToLower(req.Role) + "; that user's other sessions were ended"
	}
	adminDone(w, msg, map[string]any{"changed": changed})
}

func (a *API) handleAdminUserRemove(w http.ResponseWriter, r *http.Request) {
	req, ok := a.userRequest(w, r)
	if !ok {
		return
	}
	if err := a.users(r).Remove(actorOf(r), req.Username); err != nil {
		adminFailed(w, err)
		return
	}
	adminDone(w, "removed user "+req.Username+" and ended its sessions", nil)
}

func (a *API) handleAdminUserUnlock(w http.ResponseWriter, r *http.Request) {
	req, ok := a.userRequest(w, r)
	if !ok {
		return
	}
	if err := a.users(r).Unlock(actorOf(r), req.Username); err != nil {
		adminFailed(w, err)
		return
	}
	msg := "unlock requested for " + req.Username + ": the running web app forgets its failed sign-ins at the next attempt"
	if a.auth != nil {
		msg = "unlocked " + req.Username + ": its failed sign-ins are forgotten"
	}
	adminDone(w, msg, nil)
}

// ---------------------------------------------------------------------------
// Camera login
// ---------------------------------------------------------------------------

func (a *API) handleAdminCameraLogin(w http.ResponseWriter, r *http.Request) {
	t, ok := a.tools(w)
	if !ok {
		return
	}
	st, err := t.CameraLogin(actionCtx(r))
	if err != nil {
		adminFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cameraLogin": st})
}

type cameraLoginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func (a *API) handleAdminCameraLoginSet(w http.ResponseWriter, r *http.Request) {
	var req cameraLoginRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	setTarget(r, "camera login")
	for _, v := range []string{req.User, req.Password} {
		if len(v) > adminMaxSecret || strings.ContainsAny(v, "\r\n\x00") {
			writeAPIError(w, http.StatusUnprocessableEntity, fmt.Sprintf("a value must be one line of at most %d bytes", adminMaxSecret))
			return
		}
	}
	if req.User == "" && req.Password == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "type the camera user name, the password, or both")
		return
	}
	t, ok := a.tools(w)
	if !ok {
		return
	}
	unlock, ok := a.exclusive(w)
	if !ok {
		return
	}
	defer unlock()
	msg, err := t.SetCameraLogin(actionCtx(r), req.User, req.Password)
	if err != nil {
		adminFailed(w, err)
		return
	}
	adminDone(w, msg, nil)
}

// ---------------------------------------------------------------------------
// TLS
// ---------------------------------------------------------------------------

func (a *API) tlsDir() string {
	if a.cfg.TLSDir == "" {
		return defaultTLSDir()
	}
	return a.cfg.TLSDir
}

// CAIfPresent describes the CA of the folder without creating one; present is false when there is none.
func CAIfPresent(tlsDir string) (info CAInfo, present bool, err error) {
	m := tlsManager(tlsDir)
	raw, err := os.ReadFile(m.path(caCertFile))
	if errors.Is(err, os.ErrNotExist) {
		return CAInfo{Path: m.path(caCertFile)}, false, nil
	}
	if err != nil {
		return CAInfo{}, false, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return CAInfo{}, false, errors.New("the CA file does not hold a certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CAInfo{}, false, err
	}
	return CAInfo{Path: m.path(caCertFile), Fingerprint: fingerprint(cert), Subject: cert.Subject.CommonName, NotAfter: cert.NotAfter, SPKI: spkiHash(cert)}, true, nil
}

func (a *API) handleAdminTLS(w http.ResponseWriter, r *http.Request) {
	info, present, err := CAIfPresent(a.tlsDir())
	if err != nil {
		adminFailed(w, err)
		return
	}
	body := map[string]any{"ok": true, "present": present, "file": info.Path, "lanListener": a.cfg.TLSListen != "", "readOnly": a.cfg.TLSReadOnlyEffective()}
	if present {
		body["sha256"], body["name"], body["notAfter"] = info.Fingerprint, info.Subject, info.NotAfter.Format("2006-01-02")
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *API) handleAdminTLSCert(w http.ResponseWriter, r *http.Request) {
	data, err := CACertificatePEM(a.tlsDir())
	if errors.Is(err, os.ErrNotExist) {
		writeAPIError(w, http.StatusNotFound, "there is no local CA yet: it is created when the web app starts with web.tlsListen (or by holocron tls ca)")
		return
	}
	if err != nil {
		adminFailed(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="ipcctv-local-ca.crt"`)
	_, _ = w.Write(data)
}

// tlsRotateWord is what the page asks to be typed before the CA is deleted.
const tlsRotateWord = "ROTATE"

func (a *API) handleAdminTLSRotate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	setTarget(r, a.tlsDir())
	if req.Confirm != tlsRotateWord {
		writeAPIError(w, http.StatusUnprocessableEntity, "type "+tlsRotateWord+" to confirm: every device must install the new CA afterwards")
		return
	}
	unlock, ok := a.exclusive(w)
	if !ok {
		return
	}
	defer unlock()
	removed, left, err := RotateCA(a.tlsDir())
	if err != nil {
		adminFailed(w, err)
		return
	}
	msg := "there was no CA to delete; a new one is created when the web app starts with web.tlsListen (or by holocron tls ca)"
	if len(removed) > 0 {
		msg = "the old CA is gone: a fresh CA and server certificate are made when the web app next starts (holocron restart); until then the LAN listener keeps the old one. EVERY device must install the new CA."
	}
	adminDone(w, msg, map[string]any{"removed": len(removed), "leftAlone": len(left)})
}

// ---------------------------------------------------------------------------
// Maintenance and autostart
// ---------------------------------------------------------------------------

// adminRead answers one of the AdminTools reports.
func (a *API) adminRead(w http.ResponseWriter, r *http.Request, key string, read func(AdminTools, context.Context) (any, error)) {
	t, ok := a.tools(w)
	if !ok {
		return
	}
	v, err := read(t, actionCtx(r))
	if err != nil {
		adminFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, key: v})
}

func (a *API) handleAdminPrune(w http.ResponseWriter, r *http.Request) {
	a.adminRead(w, r, "prune", AdminTools.PrunePreview)
}

func (a *API) handleAdminArchive(w http.ResponseWriter, r *http.Request) {
	a.adminRead(w, r, "archive", AdminTools.ArchiveStats)
}

func (a *API) handleAdminLogLevels(w http.ResponseWriter, r *http.Request) {
	a.adminRead(w, r, "levels", AdminTools.LogLevels)
}

func (a *API) handleAdminAutostart(w http.ResponseWriter, r *http.Request) {
	a.adminRead(w, r, "autostart", AdminTools.Autostart)
}

func (a *API) handleAdminLogLevel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Component string `json:"component"`
		Level     string `json:"level"`
		Save      bool   `json:"save"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	setTarget(r, cleanLoggedName(req.Component+"="+req.Level))
	if req.Component == "" || req.Level == "" || strings.HasPrefix(req.Component, "-") || strings.HasPrefix(req.Level, "-") {
		writeAPIError(w, http.StatusUnprocessableEntity, "name a component (or all) and a level")
		return
	}
	t, ok := a.tools(w)
	if !ok {
		return
	}
	msg, err := t.SetLogLevel(actionCtx(r), req.Component, req.Level, req.Save)
	if err != nil {
		adminFailed(w, err)
		return
	}
	adminDone(w, msg, nil)
}

func (a *API) handleAdminAutostartSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled    *bool `json:"enabled"`
		OldTasksOK bool  `json:"oldTasksOk"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeAPIError(w, http.StatusUnprocessableEntity, `say {"enabled": true} or {"enabled": false}`)
		return
	}
	setTarget(r, map[bool]string{true: "enable", false: "disable"}[*req.Enabled])
	t, ok := a.tools(w)
	if !ok {
		return
	}
	unlock, ok := a.exclusive(w)
	if !ok {
		return
	}
	defer unlock()
	msg, err := t.SetAutostart(actionCtx(r), *req.Enabled, req.OldTasksOK)
	if err != nil {
		adminFailed(w, err)
		return
	}
	adminDone(w, msg, nil)
}
