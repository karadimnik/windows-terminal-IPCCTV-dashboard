package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"holocron/internal/cli"
)

// sessionCookie is the LAN listener's cookie. It holds a random token whose
// SHA-256 is all the server keeps (auth_sessions.go).
const sessionCookie = "camui_session"

const (
	maxLoginBody = 4 << 10
	// A login costs real CPU (a password hash); a few at a time is plenty for a
	// household and keeps a flood of attempts from starving the video proxy.
	maxConcurrentHashes = 4

	loginErrorMessage   = "Invalid username or password"
	loginRequiredHeader = "X-Login-Required"
)

// authService is the LAN listener's sign-in: users, sessions, throttling and the
// handlers and middleware around them.
type authService struct {
	dir      string
	users    UserStore
	sessions *sessionStore
	limiter  *ipLimiter
	lock     *lockout
	hasher   passwordHasher
	authLog  *authLog
	log      *slog.Logger
	now      func() time.Time
	runner   *cli.Runner // the mutating routes run as Actions; nil: over log

	hashSlots chan struct{}
	dummyOnce sync.Once
	dummy     string
}

func newAuthService(dir string, users UserStore, log *slog.Logger, now func() time.Time, hasher passwordHasher) *authService {
	return &authService{
		dir:       dir,
		users:     users,
		sessions:  newSessionStore(dir, now),
		limiter:   newIPLimiter(loginRatePerMinute, loginBurst, maxIPEntries, now),
		lock:      newLockout(users, now),
		hasher:    hasher,
		authLog:   newAuthLog(dir, now),
		log:       log,
		now:       now,
		hashSlots: make(chan struct{}, maxConcurrentHashes),
	}
}

// sourceIP is the peer address, without port. Forwarding headers are never trusted:
// there is no proxy in front of this listener.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Every auth action leaves two records with the same facts: a line in auth.log
// (for the Operator, never rotated away by console noise) and a structured log
// record for the console: component, action, actor, target, result, duration.
// Levels: INFO for what succeeded, WARN for a failed, throttled or locked sign-in,
// ERROR for a store failure, DEBUG for limiter decisions. Never a password, a hash
// or a token.
const authComponent = "auth"

var authActions = map[string]string{
	"login": "auth.login", "logout": "auth.logout", "unlock": "user.unlock",
	"set-password": "user.set_password", "setup": "user.create",
}

func resultLevel(result string) slog.Level {
	switch result {
	case "fail", "throttled", "locked":
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// emit logs one auth action to the console log.
func emit(log *slog.Logger, level slog.Level, action, actor, target, result string, started time.Time, extra ...any) {
	attrs := append([]any{"component", authComponent, "action", action, "actor", cleanLoggedName(actor), "target", cleanLoggedName(target), "result", result, "duration", time.Since(started).Round(time.Millisecond)}, extra...)
	log.Log(context.Background(), level, "auth", attrs...)
}

// audit records a request-driven action: auth.log and the console. The actor of a
// sign-in attempt is "anonymous" (nobody is signed in yet); of a sign-out, the user.
func (a *authService) audit(r *http.Request, started time.Time, actor, target, event, result string) {
	ip := sourceIP(r)
	if err := a.authLog.record(ip, target, event, result); err != nil {
		emit(a.log, slog.LevelError, "auth.audit_write", actor, target, "fail", started, "error", err.Error())
	}
	emit(a.log, resultLevel(result), authActions[event], actor, target, result, started, "from", ip)
}

// storeFailed reports a store failure at ERROR.
func (a *authService) storeFailed(action, target string, started time.Time, err error) {
	emit(a.log, slog.LevelError, action, "system", target, "fail", started, "error", err.Error())
}

// ---------------------------------------------------------------------------
// What needs no session on the LAN listener. Everything else is refused by
// default, so a route added later is protected until someone decides otherwise.
// ---------------------------------------------------------------------------

func publicRoute(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		switch path {
		case "/login", "/login.js", "/style.css", "/api/auth/me":
			return true
		}
	case http.MethodPost:
		return path == "/api/auth/login"
	}
	return false
}

type ctxKey struct{}

// signedIn is the user behind a valid session cookie.
func (a *authService) signedIn(r *http.Request) (User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, false
	}
	sess, ok := a.sessions.Lookup(c.Value)
	if !ok {
		return User{}, false
	}
	user, found, err := a.users.GetByID(sess.UserID)
	if err != nil || !found {
		return User{}, false
	}
	return user, true
}

// gate is the outermost check: a request with no valid session reaches nothing
// but the public routes. A browser navigation is sent to the login page; an API
// call gets 401 JSON and no redirect.
func (a *authService) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := a.signedIn(r)
		if ok {
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, user))
		}
		if ok || publicRoute(r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/login?next="+url.QueryEscape(safeNext(r.URL.RequestURI())), http.StatusSeeOther)
			return
		}
		w.Header().Set(loginRequiredHeader, "1")
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
	})
}

func userFrom(r *http.Request) (User, bool) {
	u, ok := r.Context().Value(ctxKey{}).(User)
	return u, ok
}

// safeNext reduces a post-login target to a same-origin path: it must start with
// one "/" and then not "/" or "\", carry no control character, no backslash and
// no scheme, and must not point at the login page itself. Anything else is "/".
func safeNext(raw string) string {
	if raw == "" || len(raw) > 1024 || raw[0] != '/' {
		return "/"
	}
	if len(raw) > 1 && (raw[1] == '/' || raw[1] == '\\') {
		return "/"
	}
	for _, r := range raw {
		if r == '\\' || r == 0x7f || r == ' ' || r == ' ' || unicode.IsControl(r) {
			return "/"
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return "/"
	}
	if u.Path == "/login" || strings.HasPrefix(u.Path, "/login/") {
		return "/"
	}
	return raw
}

func (a *authService) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (a *authService) routes(mux *http.ServeMux) {
	if a.runner == nil {
		a.runner = runnerFor(a.log)
	}
	mux.HandleFunc("GET /login", a.handleLoginPage)
	mux.HandleFunc("GET /login.js", serveAsset("login.js", "text/javascript; charset=utf-8", nil))
	handleAction(mux, a.runner, "POST /api/auth/login", ActionLogin, a.handleLogin)
	handleAction(mux, a.runner, "POST /api/auth/logout", ActionLogout, a.handleLogout)
	mux.HandleFunc("GET /api/auth/me", a.handleMe)
}

// pageCSP is for the pages that hold a password field: scripts and styles only
// from this origin, no framing, forms only to this origin.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func authPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", pageCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
}

func serveAsset(name, contentType string, headers func(http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		static, err := fs.Sub(staticFiles, "static")
		var data []byte
		if err == nil {
			data, err = fs.ReadFile(static, name)
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if headers != nil {
			headers(w)
		}
		_, _ = w.Write(data)
	}
}

func (a *authService) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFrom(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	serveAsset("login.html", "text/html; charset=utf-8", authPageHeaders)(w, r)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Next     string `json:"next"`
}

func tooManyAttempts(w http.ResponseWriter, wait time.Duration) {
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":      fmt.Sprintf("Too many attempts. Try again in %d seconds.", seconds),
		"retryAfter": seconds,
	})
}

func (a *authService) handleLogin(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var req loginRequest
	body := http.MaxBytesReader(w, r.Body, maxLoginBody)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "bad request")
		return
	}
	name := req.Username
	setTarget(r, cleanLoggedName(name))

	ok, wait := a.limiter.allow(sourceIP(r))
	emit(a.log, slog.LevelDebug, "auth.limiter", "anonymous", name, map[bool]string{true: "allowed", false: "throttled"}[ok], started, "from", sourceIP(r), "waitMs", wait.Milliseconds())
	if !ok {
		a.audit(r, started, "anonymous", name, "login", "throttled")
		tooManyAttempts(w, wait)
		return
	}
	if a.consumeUnlock(name) {
		a.lock.reset(name)
		a.audit(r, started, "local", name, "unlock", "applied")
	}
	if wait, locked := a.lock.locked(name); locked {
		a.audit(r, started, "anonymous", name, "login", "locked")
		tooManyAttempts(w, wait)
		return
	}

	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		a.audit(r, started, "anonymous", name, "login", "throttled")
		tooManyAttempts(w, time.Second)
		return
	}

	user, found, err := a.users.Get(name)
	if err != nil {
		a.storeFailed("auth.login", name, started, err)
		writeAPIError(w, http.StatusInternalServerError, "sign-in is unavailable")
		return
	}
	// An unknown user costs the same hash as a wrong password, so the time says nothing.
	hash := a.dummyHash()
	if found {
		hash = user.Hash
	}
	if !verifyPassword(hash, req.Password) || !found {
		st := a.lock.fail(name)
		a.audit(r, started, "anonymous", name, "login", "fail")
		if delay := lockoutDelay(st.Count); delay > 0 {
			emit(a.log, slog.LevelWarn, "auth.lockout", "anonymous", name, "locked", started, "failures", st.Count, "lockSeconds", int(delay.Seconds()))
		}
		writeAPIError(w, http.StatusUnauthorized, loginErrorMessage)
		return
	}

	a.lock.reset(name)
	if a.hasher.needsRehash(user.Hash) {
		a.upgradeHash(user, req.Password, started)
	}
	if old, err := r.Cookie(sessionCookie); err == nil {
		_ = a.sessions.Delete(old.Value) // a new login never inherits an earlier session
	}
	token, err := a.sessions.Create(user.ID)
	if err != nil {
		a.storeFailed("auth.login", user.Username, started, err)
		writeAPIError(w, http.StatusInternalServerError, "sign-in is unavailable")
		return
	}
	a.audit(r, started, "anonymous", user.Username, "login", "ok")
	a.setSessionCookie(w, token, int(sessionLifetime.Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": user.Username, "role": user.Role, "next": safeNext(req.Next)})
}

func (a *authService) dummyHash() string {
	a.dummyOnce.Do(func() { a.dummy, _ = a.hasher.Hash("dummy password for unknown users") })
	return a.dummy
}

func (a *authService) upgradeHash(user User, password string, started time.Time) {
	hash, err := a.hasher.Hash(password)
	if err == nil {
		user.Hash = hash
		err = a.users.Put(user)
	}
	if err != nil {
		a.storeFailed("user.rehash", user.Username, started, err)
	}
}

func (a *authService) handleLogout(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	user, _ := userFrom(r)
	setTarget(r, cleanLoggedName(user.Username))
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.sessions.Delete(c.Value)
	}
	a.audit(r, started, user.Username, user.Username, "logout", "ok")
	a.setSessionCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *authService) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false, "loginRequired": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": user.Username, "role": user.Role, "loginRequired": true, "next": safeNext(r.URL.Query().Get("next"))})
}

// ---------------------------------------------------------------------------
// The loopback listener: no login, but the first user can be created from the PC
// itself while none exists. Anyone at the PC's loopback can already control
// everything, so this adds no exposure.
// ---------------------------------------------------------------------------

type loopbackAuth struct {
	users  UserStore
	log    *slog.Logger
	hasher passwordHasher
	now    func() time.Time
	audit  *authLog
	mu     sync.Mutex
	runner *cli.Runner // nil: over log
}

// newLoopbackAuth with an empty dir (a bare Config in a test) has no first-run page at all.
func newLoopbackAuth(dir string, log *slog.Logger) *loopbackAuth {
	l := &loopbackAuth{log: log, hasher: defaultPasswordHasher, now: time.Now}
	if dir != "" {
		l.users, l.audit = newFileUserStore(dir, l.now), newAuthLog(dir, l.now)
	}
	return l
}

func (l *loopbackAuth) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "loginRequired": false})
	})
	if l.users == nil {
		return
	}
	mux.HandleFunc("GET /setup", l.ifNoUsers(serveAsset("setup.html", "text/html; charset=utf-8", authPageHeaders)))
	mux.HandleFunc("GET /setup.js", l.ifNoUsers(serveAsset("setup.js", "text/javascript; charset=utf-8", nil)))
	if l.runner == nil {
		l.runner = runnerFor(l.log)
	}
	handleAction(mux, l.runner, "POST /api/auth/setup", ActionSetup, l.handleSetup)
}

// open reports whether the first-run page may be used: no user exists. An
// unreadable user file counts as closed.
func (l *loopbackAuth) open() bool {
	users, err := l.users.List()
	return err == nil && len(users) == 0
}

func (l *loopbackAuth) ifNoUsers(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.open() {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}
}

func (l *loopbackAuth) handleSetup(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.open() {
		http.NotFound(w, r)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad request")
		return
	}
	setTarget(r, cleanLoggedName(req.Username))
	user, err := newUser(l.hasher, req.Username, req.Password, l.now())
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := l.users.Put(user); err != nil {
		emit(l.log, slog.LevelError, "user.create", "local", user.Username, "fail", started, "error", err.Error())
		writeAPIError(w, http.StatusInternalServerError, "could not save the user")
		return
	}
	_ = l.audit.record(sourceIP(r), user.Username, "setup", "ok")
	emit(l.log, slog.LevelInfo, "user.create", "local", user.Username, "ok", started, "from", sourceIP(r))
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "username": user.Username})
}

// newUser validates a name and password and returns a ready admin record.
func newUser(hasher passwordHasher, username, password string, now time.Time) (User, error) {
	if !validUsername(username) {
		return User{}, errors.New("the username must be 1 to 64 characters from A-Z a-z 0-9 . _ -")
	}
	if err := checkPasswordPolicy(username, password); err != nil {
		return User{}, err
	}
	hash, err := hasher.Hash(password)
	if err != nil {
		return User{}, err
	}
	return User{ID: newID(), Username: username, Hash: hash, Role: roleAdmin, CreatedAt: now.UTC()}, nil
}

// ---------------------------------------------------------------------------
