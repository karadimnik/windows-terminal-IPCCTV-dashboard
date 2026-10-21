package web

import (
	"context"
	"log/slog"
	"net/http"

	"holocron/internal/cli"
	"holocron/internal/logx"
)

// Who may reach a route. Every pattern the muxes register has a class in routeAccess, and the LAN
// listener's session gate (authService.gate) decides by the pattern the mux will actually run, so
// a path trick (`/api/admin/../config`, a doubled slash, a trailing slash) is judged as the route
// it ends up at. TestEveryRouteHasAnAccessClass fails for a pattern without a class, and the gate
// refuses one at run time (fail closed).
//
// The loopback listener has no login: whoever reaches it is the box owner and an admin, as is the
// legacy token mode of camui.exe. View-only mode (tlsreadonly) still refuses every change on the
// LAN listener, an admin's included.
type access int

const (
	accessUnclassified access = iota
	accessPublic              // no session: the sign-in page and what it needs
	accessViewer              // any signed-in user: the page, status, live, footage, sign-out
	accessAdmin               // admins only
)

func (a access) String() string {
	return [...]string{"unclassified", "public", "viewer", "admin"}[a]
}

// routeAccess classifies every registered pattern. A viewer watches: status, live, footage and the
// certificate panel. Everything that starts, stops, records, acknowledges or configures is admin,
// and so is reading the camera list and the settings (they are the Settings screen's).
var routeAccess = map[string]access{
	// the sign-in page
	"GET /login":           accessPublic,
	"GET /login.js":        accessPublic,
	"GET /style.css":       accessPublic,
	"POST /api/auth/login": accessPublic,
	"GET /api/auth/me":     accessPublic,

	// watching
	"GET /":                         accessViewer, // the page and its assets
	"POST /api/auth/logout":         accessViewer,
	"GET /api/status":               accessViewer,
	"GET /api/footage":              accessViewer,
	"GET /api/clip":                 accessViewer,
	"GET /api/live":                 accessViewer,
	"POST /api/live/{camera}/whep":  accessViewer, // opening and closing one's own viewing session
	"DELETE /api/live/session/{id}": accessViewer,
	"GET /api/tls/ca.crt":           accessViewer, // the CA certificate, never a key
	"GET /api/tls/info":             accessViewer,

	// managing
	"GET /admin.js":                   accessAdmin, // the Admin tab's code
	"POST /api/acknowledge":           accessAdmin,
	"POST /api/control":               accessAdmin, // start and stop
	"POST /api/cameras/{name}/record": accessAdmin,
	"GET /api/cameras":                accessAdmin,
	"PUT /api/cameras":                accessAdmin,
	"GET /api/config":                 accessAdmin,
	"PUT /api/config":                 accessAdmin,

	// loopback only (the first-run page): never registered on the LAN listener
	"GET /setup":           accessAdmin,
	"GET /setup.js":        accessAdmin,
	"POST /api/auth/setup": accessAdmin,
}

// accessOf is the class of the route the mux will run for r, and its pattern. A request that
// matches nothing (the mux answers 404 or 405) needs a session like any other: accessViewer.
func accessOf(mux *http.ServeMux, r *http.Request) (access, string) {
	_, pattern := mux.Handler(r)
	if pattern == "" {
		return accessViewer, ""
	}
	return routeAccess[pattern], pattern
}

// denied records a refused request in the audit stream: who, what, and why.
func denied(log *slog.Logger, r *http.Request, user User, pattern, why string) {
	log.LogAttrs(context.Background(), slog.LevelWarn, "access.denied",
		slog.String("component", authComponent), logx.Action("web.access"), logx.Actor(cli.WebActor(cleanLoggedName(user.Username))),
		logx.Target(r.Method+" "+cleanLoggedName(r.URL.Path)), logx.Result("denied"), slog.String("role", effectiveRole(user.Role)),
		slog.String("route", pattern), slog.String("reason", why), slog.String("from", sourceIP(r)), logx.Audit())
}
