package main

import (
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The request guard stops a web page the Operator visits from driving camui
// through his own browser. Two attacks, two defences:
//
//   - Cross-site requests (a form or a no-preflight fetch from another origin)
//     are refused by Sec-Fetch-Site / Origin, and by requiring a JSON content
//     type on state-changing requests, which a browser cannot send cross-site
//     without a preflight that camui never answers.
//   - DNS rebinding (the attacker's name resolves to 127.0.0.1, so the browser
//     treats camui as same-origin with it) is refused by the Host allowlist:
//     the browser still sends the attacker's name as Host.
//
// A request carrying neither Origin nor Sec-Fetch-Site (a script, curl, the
// PowerShell harness) is not a browser acting for a page and passes these
// checks; the token, when one is configured, still applies to it.

// loopbackHostNames are always allowed.
var loopbackHostNames = []string{"localhost", "127.0.0.1", "::1"}

// maxLoggedHost bounds an attacker-chosen string before it reaches the log.
const maxLoggedHost = 100

// hostName returns the lower-cased host of a Host header or an allowedHosts
// entry, without port and without IPv6 brackets.
func hostName(hostport string) string {
	h := strings.ToLower(strings.TrimSpace(hostport))
	if strings.HasPrefix(h, "[") {
		if end := strings.Index(h, "]"); end > 0 {
			return h[1:end]
		}
		return h
	}
	// One colon is host:port; none is a bare host; several is a bare IPv6.
	if strings.Count(h, ":") == 1 {
		h, _, _ = strings.Cut(h, ":")
	}
	return h
}

// hostAllowlist is what the Host header may name.
type hostAllowlist struct {
	names map[string]bool
	// anyIP admits any IP-literal Host. Set only in LAN mode with a wildcard
	// bind ("0.0.0.0:8787", ":8787"), where there is no one listen host to
	// name and the address a client reaches camui by is not known in advance.
	// An IP literal cannot be DNS-rebound: rebinding needs the browser to
	// resolve an attacker-owned NAME.
	anyIP bool
}

func (a hostAllowlist) allows(host string) bool {
	return a.names[host] || (a.anyIP && net.ParseIP(host) != nil)
}

// newHostAllowlist admits loopback always, the listen host in LAN mode
// (non-loopback listen and a token), and the configured allowedHosts.
func newHostAllowlist(cfg Config) hostAllowlist {
	list := hostAllowlist{names: map[string]bool{}}
	for _, name := range loopbackHostNames {
		list.names[name] = true
	}
	if cfg.RequiresToken() {
		if loopback, err := listenIsLoopback(cfg.Listen); err == nil && !loopback {
			host, _, _ := net.SplitHostPort(cfg.Listen)
			if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
				list.anyIP = true
			} else {
				list.names[hostName(host)] = true
			}
		}
	}
	for _, name := range cfg.AllowedHosts {
		if name = hostName(name); name != "" {
			list.names[name] = true
		}
	}
	return list
}

// newGuard returns the middleware. It sits inside the request logger and
// outside the mux, so it covers the static page and every API route.
func newGuard(cfg Config, log *slog.Logger) func(http.Handler) http.Handler {
	return newGuardScheme(cfg, log, "")
}

// newGuardScheme is newGuard for a listener with a known scheme: with "https" an
// Origin must be https too (an http:// page cannot drive the HTTPS listener).
// An empty scheme keeps the loopback behaviour, which compares host and port only.
func newGuardScheme(cfg Config, log *slog.Logger, scheme string) func(http.Handler) http.Handler {
	allowed := newHostAllowlist(cfg)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			refuse := func(code int, reason, message string, attrs ...any) {
				log.Warn("request refused", append([]any{"method", r.Method, "path", r.URL.Path, "reason", reason}, attrs...)...)
				http.Error(w, message, code)
			}

			api := strings.HasPrefix(r.URL.Path, "/api/")
			if api {
				h := w.Header()
				h.Set("Cross-Origin-Resource-Policy", "same-origin")
				h.Set("X-Content-Type-Options", "nosniff")
				// A handler that sets its own cache header overrides this.
				h.Set("Cache-Control", "no-store")
			}

			host := hostName(r.Host)
			if !allowed.allows(host) {
				if len(host) > maxLoggedHost {
					host = host[:maxLoggedHost]
				}
				refuse(http.StatusForbidden, "host not allowed", "forbidden: host not allowed", "host", host)
				return
			}

			if api {
				if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
					refuse(http.StatusForbidden, "cross-site request", "forbidden: cross-site request")
					return
				}
				if origin, present := r.Header["Origin"]; present && !sameOrigin(origin[0], r.Host, scheme) {
					refuse(http.StatusForbidden, "foreign origin", "forbidden: cross-origin request")
					return
				}
				if changesState(r.Method) && r.ContentLength != 0 && !bodyTypeAllowed(r) {
					refuse(http.StatusUnsupportedMediaType, "content type is not application/json", "unsupported media type: application/json required")
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

func changesState(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// whepRoute is the ONE route that may carry a body that is not JSON: a
// browser's WebRTC offer is application/sdp. Nothing else is exempt - not
// another method on this path, not another path, not another type - and the
// Host, Origin and Sec-Fetch-Site checks above still apply to it.
var whepRoute = regexp.MustCompile(`^/api/live/[^/]+/whep$`)

func bodyTypeAllowed(r *http.Request) bool {
	contentType := r.Header.Get("Content-Type")
	if isJSON(contentType) {
		return true
	}
	return r.Method == http.MethodPost && whepRoute.MatchString(r.URL.Path) && isSDP(contentType)
}

func isSDP(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && media == "application/sdp"
}

func isJSON(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && media == "application/json"
}

// sameOrigin reports whether an Origin header names the host:port the request
// was sent to. "null" (sandboxed frames, redirects across origins) never does.
func sameOrigin(origin, host, scheme string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if scheme != "" && !strings.EqualFold(u.Scheme, scheme) {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
