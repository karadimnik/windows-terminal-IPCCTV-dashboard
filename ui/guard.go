package main

import (
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
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

// allowedHostSet is what the Host header may name: loopback always, the listen
// host in LAN mode (non-loopback listen and a token), and the configured
// allowedHosts.
func allowedHostSet(cfg Config) map[string]bool {
	allowed := map[string]bool{}
	for _, name := range loopbackHostNames {
		allowed[name] = true
	}
	if cfg.RequiresToken() {
		if loopback, err := listenIsLoopback(cfg.Listen); err == nil && !loopback {
			host, _, _ := net.SplitHostPort(cfg.Listen)
			if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
				allowed[hostName(host)] = true
			}
		}
	}
	for _, name := range cfg.AllowedHosts {
		if name = hostName(name); name != "" {
			allowed[name] = true
		}
	}
	return allowed
}

// newGuard returns the middleware. It sits inside the request logger and
// outside the mux, so it covers the static page and every API route.
func newGuard(cfg Config, log *slog.Logger) func(http.Handler) http.Handler {
	allowed := allowedHostSet(cfg)

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
			if !allowed[host] {
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
				if origin, present := r.Header["Origin"]; present && !sameOrigin(origin[0], r.Host) {
					refuse(http.StatusForbidden, "foreign origin", "forbidden: cross-origin request")
					return
				}
				if changesState(r.Method) && r.ContentLength != 0 && !isJSON(r.Header.Get("Content-Type")) {
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

func isJSON(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && media == "application/json"
}

// sameOrigin reports whether an Origin header names the host:port the request
// was sent to. "null" (sandboxed frames, redirects across origins) never does.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
