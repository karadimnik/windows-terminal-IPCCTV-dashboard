package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"holocron/internal/config"
)

// ---------------------------------------------------------------------------
// Live view: a same-origin door to the dedicated live MediaMTX.
//
// The browser sends its WebRTC offer here; camui forwards it to the live
// instance with the login the browser never has, and returns the answer. The
// browser therefore never learns the instance's address, its login, or any
// path name: it names a camera and sub|main, and camui looks the path up in
// status.json (where the supervisor wrote it, so the naming rule lives in one
// place). The media itself flows browser to MediaMTX over ICE.
//
// What this file refuses to do matters as much as what it does: no browser
// header goes upstream, no upstream header or body comes back, and a client
// can only name a session by an id camui minted.
// ---------------------------------------------------------------------------

var (
	// liveUpstreamTimeout bounds the upstream call. An on-demand camera source
	// may take up to 10 s to start (sourceOnDemandStartTimeout).
	liveUpstreamTimeout = 15 * time.Second

	// liveSessionTTL is how long camui remembers a session id. It only serves
	// the tidy-up DELETE and the caps: MediaMTX closes the session itself when
	// the peer connection closes.
	liveSessionTTL = time.Hour
)

const (
	// A page opens six sub tiles and at most one main; two viewers fit.
	liveMaxSessions          = 24
	liveMaxSessionsPerCamera = 6

	liveMaxOfferBytes = 64 << 10
	liveDeleteTimeout = 5 * time.Second

	liveUser           = "camlive"
	liveDefaultAddress = "127.0.0.1:8889"
)

var (
	livePathPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	liveSecretPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	liveSessionID     = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// liveSettings are the recorder.config.json keys live view needs.
type liveSettings struct {
	enabled    bool
	address    string
	secretPath string
}

type liveCamera struct {
	name      string
	sub, main bool
	subPath   string
	mainPath  string
	camAlive  bool
	restarts  int
	lastError string
}

type liveState struct {
	enabled bool
	alive   bool
	cameras []liveCamera
}

type liveSession struct {
	camera   string
	upstream string
	expires  time.Time
}

// liveHub holds the session table and the per-episode log flags.
type liveHub struct {
	client *http.Client

	mu           sync.Mutex
	sessions     map[string]*liveSession
	pending      map[string]int
	pendingTotal int
	logged       map[string]bool
}

func newLiveHub() *liveHub {
	return &liveHub{
		client: &http.Client{
			// No proxy from the environment, no redirects, no compression:
			// the call is one plain loopback request.
			Transport: &http.Transport{Proxy: nil, DisableCompression: true, DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		sessions: map[string]*liveSession{},
		pending:  map[string]int{},
		logged:   map[string]bool{},
	}
}

func (a *API) hub() *liveHub {
	a.liveOnce.Do(func() { a.live = newLiveHub() })
	return a.live
}

func (a *API) liveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/live", a.handleLiveList)
	handleAction(mux, a.runner, "POST /api/live/{camera}/whep", ActionLiveCreate, a.handleLiveWhep)
	handleAction(mux, a.runner, "DELETE /api/live/session/{id}", ActionLiveDelete, a.handleLiveDelete)
}

// ---------------------------------------------------------------------------
// Reading the settings, the status and the secret
// ---------------------------------------------------------------------------

func (a *API) liveSettings() liveSettings {
	settings := liveSettings{address: liveDefaultAddress}
	secretDir := a.cfg.LiveSecretDir
	if secretDir == "" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			secretDir = filepath.Join(local, "IPCCTV")
		}
	}
	if secretDir != "" {
		settings.secretPath = config.LiveSecretFile("", secretDir)
	}

	raw, err := os.ReadFile(a.cfg.RecorderConfigFile())
	if err != nil {
		return settings
	}
	var parsed map[string]any
	if err := json.Unmarshal(stripBOM(raw), &parsed); err != nil {
		return settings
	}
	settings.enabled, _ = parsed["LiveViewEnabled"].(bool)
	if value := strings.TrimSpace(jsonString(parsed["LiveWhepAddress"])); value != "" {
		settings.address = value
	}
	if value := strings.TrimSpace(jsonString(parsed["LiveSecretPath"])); value != "" {
		settings.secretPath = config.LiveSecretFile(value, "")
	}
	return settings
}

// LiveSecretFileOf is the live-view secret file a web role started with cfg opens for its WHEP
// proxy (`holocron doctor` logs in with it to prove the live instance accepts it). Read only.
func LiveSecretFileOf(cfg Config) string { return (&API{cfg: cfg}).liveSettings().secretPath }

// liveSnapshot reads what the supervisor knows about live view. Everything
// comes from the redacted status.json; the path names stay inside this
// package.
func (a *API) liveSnapshot(settings liveSettings) liveState {
	state := liveState{enabled: settings.enabled, cameras: []liveCamera{}}
	if !settings.enabled {
		return state
	}
	status, _, err := a.readStatus()
	if err != nil {
		return state
	}
	live, ok := status["Live"].(map[string]any)
	if !ok {
		return state
	}
	verdict := SupervisorStateOf(status, a.statusIntervalSeconds(), time.Now(), a.alive)
	alive, _ := live["Alive"].(bool)
	state.alive = verdict.Running && alive

	recorder := map[string]map[string]any{}
	if rows, ok := status["Cameras"].([]any); ok {
		for _, row := range rows {
			if fields, ok := row.(map[string]any); ok {
				recorder[jsonString(fields["Name"])] = fields
			}
		}
	}
	rows, _ := live["Cameras"].([]any)
	for _, row := range rows {
		fields, ok := row.(map[string]any)
		if !ok {
			continue
		}
		camera := liveCamera{name: jsonString(fields["Name"])}
		if camera.name == "" {
			continue
		}
		sub, _ := fields["Sub"].(bool)
		main, _ := fields["Main"].(bool)
		camera.subPath, camera.mainPath = jsonString(fields["SubPath"]), jsonString(fields["MainPath"])
		camera.sub = sub && livePathPattern.MatchString(camera.subPath)
		camera.main = main && livePathPattern.MatchString(camera.mainPath)
		if rec, ok := recorder[camera.name]; ok {
			camera.camAlive, _ = rec["Alive"].(bool)
			camera.restarts = jsonInt(rec["RestartsLastHour"])
			camera.lastError = jsonString(rec["LastError"])
		}
		state.cameras = append(state.cameras, camera)
	}
	return state
}

func (s liveState) find(name string) *liveCamera {
	for i := range s.cameras {
		if strings.EqualFold(s.cameras[i].name, name) {
			return &s.cameras[i]
		}
	}
	return nil
}

// readLiveSecret returns the secret from its file, or "" when the file is
// missing or does not hold exactly 64 lowercase hex characters (a trailing
// newline from a hand edit is tolerated). It is read at each use and held
// only in the caller's local variable.
func readLiveSecret(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	secret := strings.TrimSpace(string(raw))
	if !liveSecretPattern.MatchString(secret) {
		return ""
	}
	return secret
}

// liveBase returns the scheme and host of the live instance, or false when the
// address is not a loopback host:port. The login travels in clear HTTP, so it
// must never be sent anywhere else.
func liveBase(address string) (string, bool) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", false
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return "", false
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && !strings.EqualFold(host, "localhost") {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

// ---------------------------------------------------------------------------
// GET /api/live
// ---------------------------------------------------------------------------

type liveCameraJSON struct {
	Name      string `json:"name"`
	Sub       bool   `json:"sub"`
	Main      bool   `json:"main"`
	CamAlive  bool   `json:"camAlive"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"lastError"`
}

type liveListJSON struct {
	Enabled bool `json:"enabled"`
	Alive   bool `json:"alive"`
	// HiddenGraceSeconds is camui's liveHiddenGraceSeconds: how long the page keeps
	// its streams after it is hidden (0 frees at once, -1 never).
	HiddenGraceSeconds int              `json:"hiddenGraceSeconds"`
	Cameras            []liveCameraJSON `json:"cameras"`
}

func (a *API) handleLiveList(w http.ResponseWriter, r *http.Request) {
	state := a.liveSnapshot(a.liveSettings())
	body := liveListJSON{Enabled: state.enabled, Alive: state.alive, HiddenGraceSeconds: a.cfg.LiveHiddenGraceEffective(), Cameras: []liveCameraJSON{}}
	for _, camera := range state.cameras {
		body.Cameras = append(body.Cameras, liveCameraJSON{
			Name: camera.name, Sub: camera.sub, Main: camera.main,
			CamAlive: camera.camAlive, Restarts: camera.restarts, LastError: camera.lastError,
		})
	}
	writeJSON(w, http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// POST /api/live/{camera}/whep
// ---------------------------------------------------------------------------

func (a *API) handleLiveWhep(w http.ResponseWriter, r *http.Request) {
	setTarget(r, r.PathValue("camera"))
	hub := a.hub()

	streams := r.URL.Query()["stream"]
	if len(streams) != 1 || (streams[0] != "sub" && streams[0] != "main") {
		writeAPIError(w, http.StatusBadRequest, "stream must be sub or main")
		return
	}
	main := streams[0] == "main"

	settings := a.liveSettings()
	if !settings.enabled {
		writeAPIError(w, http.StatusServiceUnavailable, "live view is off")
		return
	}
	state := a.liveSnapshot(settings)
	if !state.alive {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	camera := state.find(r.PathValue("camera"))
	if camera == nil {
		writeAPIError(w, http.StatusNotFound, "unknown camera")
		return
	}
	path, offered := camera.subPath, camera.sub
	if main {
		path, offered = camera.mainPath, camera.main
	}
	if !offered {
		writeAPIError(w, http.StatusNotFound, "stream not available")
		return
	}

	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/sdp" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported media type: application/sdp required")
		return
	}
	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, liveMaxOfferBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "offer too large")
		} else {
			writeAPIError(w, http.StatusBadRequest, "offer unreadable")
		}
		return
	}
	if len(bytes.TrimSpace(offer)) == 0 {
		writeAPIError(w, http.StatusBadRequest, "empty offer")
		return
	}

	if !hub.reserve(camera.name) {
		writeAPIError(w, http.StatusTooManyRequests, "too many live sessions")
		return
	}
	committed := false
	defer func() {
		if !committed {
			hub.release(camera.name)
		}
	}()

	base, ok := liveBase(settings.address)
	if !ok {
		a.logLiveOnce("address", "LiveWhepAddress is not a loopback host:port; live view refuses to send its login anywhere else")
		writeAPIError(w, http.StatusBadGateway, "live view is misconfigured")
		return
	}
	secret := readLiveSecret(settings.secretPath)
	if secret == "" {
		a.logLiveOnce("secret", "the live view secret is missing or is not 64 hex characters; check LiveSecretPath")
		writeAPIError(w, http.StatusBadGateway, "live view is misconfigured")
		return
	}

	target := "http://" + base + "/" + url.PathEscape(path) + "/whep"
	ctx, cancel := context.WithTimeout(r.Context(), liveUpstreamTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(offer))
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "live view error")
		return
	}
	request.Header.Set("Content-Type", "application/sdp")
	request.Header.Set("User-Agent", "")
	request.SetBasicAuth(liveUser, secret)

	response, err := hub.client.Do(request)
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			// The browser gave up; there is nobody to answer.
		case errors.Is(err, context.DeadlineExceeded):
			writeAPIError(w, http.StatusGatewayTimeout, "live view timed out")
		default:
			writeAPIError(w, http.StatusServiceUnavailable, "unavailable")
		}
		return
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, liveMaxOfferBytes))

	switch response.StatusCode {
	case http.StatusCreated:
	case http.StatusUnauthorized, http.StatusForbidden:
		a.logLiveOnce("login", "live view login was refused by the live instance; check LiveSecretPath and restart the recorder so both use the same secret")
		writeAPIError(w, http.StatusBadGateway, "live view is misconfigured")
		return
	case http.StatusNotFound:
		writeAPIError(w, http.StatusNotFound, "stream not available")
		return
	case http.StatusBadRequest:
		writeAPIError(w, http.StatusBadRequest, "offer rejected")
		return
	default:
		writeAPIError(w, http.StatusBadGateway, "live view error")
		return
	}
	if !bytes.HasPrefix(bytes.TrimSpace(answer), []byte("v=0")) {
		writeAPIError(w, http.StatusBadGateway, "live view error")
		return
	}

	id := hub.commit(camera.name, upstreamSessionURL(request.URL, response.Header.Get("Location"), path))
	committed = true
	hub.clearLogged()

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/api/live/session/"+id)
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(answer)
}

// upstreamSessionURL resolves the Location the live instance returned and
// keeps it only when it is a session of this same path on this same host.
// Anything else is dropped, so a stored URL can never point elsewhere.
func upstreamSessionURL(base *url.URL, location, path string) string {
	if location == "" {
		return ""
	}
	resolved, err := base.Parse(location)
	if err != nil || resolved.Scheme != "http" || !strings.EqualFold(resolved.Host, base.Host) ||
		resolved.User != nil || resolved.RawQuery != "" ||
		!strings.HasPrefix(resolved.Path, "/"+path+"/whep/") {
		return ""
	}
	return resolved.String()
}

// ---------------------------------------------------------------------------
// DELETE /api/live/session/{id}
// ---------------------------------------------------------------------------

func (a *API) handleLiveDelete(w http.ResponseWriter, r *http.Request) {
	hub := a.hub()
	id := r.PathValue("id")
	setTarget(r, id)
	session := hub.take(id)
	if session == nil {
		writeAPIError(w, http.StatusNotFound, "unknown session")
		return
	}
	if session.upstream != "" {
		if secret := readLiveSecret(a.liveSettings().secretPath); secret != "" {
			ctx, cancel := context.WithTimeout(r.Context(), liveDeleteTimeout)
			defer cancel()
			if request, err := http.NewRequestWithContext(ctx, http.MethodDelete, session.upstream, nil); err == nil {
				request.Header.Set("User-Agent", "")
				request.SetBasicAuth(liveUser, secret)
				if response, err := hub.client.Do(request); err == nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, liveMaxOfferBytes))
					response.Body.Close()
				}
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// The session table
// ---------------------------------------------------------------------------

// purgeLocked forgets expired sessions. The caller holds mu.
func (h *liveHub) purgeLocked(now time.Time) {
	for id, session := range h.sessions {
		if now.After(session.expires) {
			delete(h.sessions, id)
		}
	}
}

func (h *liveHub) countLocked(camera string) (total, perCamera int) {
	total = len(h.sessions) + h.pendingTotal
	perCamera = h.pending[camera]
	for _, session := range h.sessions {
		if session.camera == camera {
			perCamera++
		}
	}
	return total, perCamera
}

// reserve claims a slot before the upstream call, so concurrent offers cannot
// overshoot the caps.
func (h *liveHub) reserve(camera string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeLocked(time.Now())
	total, perCamera := h.countLocked(camera)
	if total >= liveMaxSessions || perCamera >= liveMaxSessionsPerCamera {
		return false
	}
	h.pending[camera]++
	h.pendingTotal++
	return true
}

func (h *liveHub) release(camera string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseLocked(camera)
}

func (h *liveHub) releaseLocked(camera string) {
	if h.pending[camera] > 0 {
		h.pending[camera]--
		h.pendingTotal--
	}
}

// commit turns a reserved slot into a session and returns its minted id.
func (h *liveHub) commit(camera, upstream string) string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic("no randomness: " + err.Error())
	}
	id := hex.EncodeToString(raw)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.releaseLocked(camera)
	h.sessions[id] = &liveSession{camera: camera, upstream: upstream, expires: time.Now().Add(liveSessionTTL)}
	return id
}

// take removes and returns a session, or nil when the id is unknown or expired.
func (h *liveHub) take(id string) *liveSession {
	if !liveSessionID.MatchString(id) {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeLocked(time.Now())
	session := h.sessions[id]
	delete(h.sessions, id)
	return session
}

// logLiveOnce writes one line per kind of misconfiguration until a session
// succeeds again, so a wrong secret is one line per episode, not one per tile.
// The message carries no value from the configuration.
func (a *API) logLiveOnce(kind, message string) {
	hub := a.hub()
	hub.mu.Lock()
	seen := hub.logged[kind]
	hub.logged[kind] = true
	hub.mu.Unlock()
	if !seen {
		a.log.Warn(message)
	}
}

func (h *liveHub) clearLogged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	clear(h.logged)
}

// dropLivePaths removes the live path names from a status document bound for
// the browser. They are not secrets, but nothing in the page needs them.
func dropLivePaths(status map[string]any) {
	live, ok := status["Live"].(map[string]any)
	if !ok {
		return
	}
	delete(live, "Paths")
	cameras, _ := live["Cameras"].([]any)
	for _, row := range cameras {
		if fields, ok := row.(map[string]any); ok {
			delete(fields, "SubPath")
			delete(fields, "MainPath")
		}
	}
}
