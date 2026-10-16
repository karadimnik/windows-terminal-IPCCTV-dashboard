package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// tokenPolicy configures requireTokenPolicy. The zero value (plus a Token) is
// the loopback listener's behaviour, unchanged: the cookie holds the token,
// nothing is rate limited.
type tokenPolicy struct {
	Token string

	// Strict is the LAN listener: the cookie is Secure and holds a one-way
	// derivation of the token (never the token), failures are logged without
	// the token and rate limited per source address.
	Strict  bool
	Limiter *failureLimiter
	Log     *slog.Logger
}

// cookieMaxAge keeps a phone signed in across browser restarts.
const cookieMaxAge = 30 * 24 * 60 * 60

// cookieValue is what the Strict cookie holds: HMAC-SHA256 of a fixed label
// keyed with the token. It is stateless, so it survives a restart and works
// across tabs, and a copy of the cookie does not reveal the token.
func cookieValue(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("camui-tls-cookie-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func requireTokenPolicy(p tokenPolicy, next http.Handler) http.Handler {
	if p.Token == "" {
		return next
	}
	expectedCookie := p.Token
	if p.Strict {
		expectedCookie = cookieValue(p.Token)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := sourceIP(r)
		if p.Limiter != nil {
			if retry, blocked := p.Limiter.blocked(ip); blocked {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
				return
			}
		}
		if cookie, err := r.Cookie(tokenCookie); err == nil && tokenMatches(expectedCookie, cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if bearer, ok := bearerToken(r); ok && tokenMatches(p.Token, bearer) {
			next.ServeHTTP(w, r)
			return
		}
		presented := false
		if query := r.URL.Query().Get("token"); query != "" {
			presented = true
			if tokenMatches(p.Token, query) {
				http.SetCookie(w, &http.Cookie{
					Name:     tokenCookie,
					Value:    expectedCookie,
					Path:     "/",
					HttpOnly: true,
					Secure:   p.Strict,
					SameSite: http.SameSiteStrictMode,
					MaxAge:   map[bool]int{true: cookieMaxAge, false: 0}[p.Strict],
				})
				clean := *r.URL
				params := clean.Query()
				params.Del("token")
				clean.RawQuery = params.Encode()
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
				return
			}
		}
		if p.Limiter != nil {
			p.Limiter.fail(ip)
		}
		if p.Log != nil {
			// Who and what kind of failure, never the token or any part of it.
			reason := "no token"
			if presented || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				reason = "wrong token"
			}
			p.Log.Warn("token refused", "from", ip, "method", r.Method, "path", r.URL.Path, "reason", reason)
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "a token is required", http.StatusUnauthorized)
	})
}

// sourceIP is the peer address, without port. Forwarding headers are never trusted.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newToken is 32 random bytes as 64 hex characters.
func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// failureLimiter counts failed token attempts per source address in a fixed
// window. At most maxEntries addresses are tracked; expired entries are purged
// when the table is full, and when it is still full every further new address
// shares one overflow bucket, so memory stays bounded however many addresses
// probe.
type failureLimiter struct {
	max        int
	window     time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	entries map[string]*failureEntry
}

type failureEntry struct {
	count int
	start time.Time
}

const overflowKey = "*overflow*"

func newFailureLimiter(max int, window time.Duration, maxEntries int) *failureLimiter {
	return &failureLimiter{max: max, window: window, maxEntries: maxEntries, now: time.Now, entries: map[string]*failureEntry{}}
}

func (l *failureLimiter) keyLocked(ip string) string {
	if _, ok := l.entries[ip]; ok || len(l.entries) < l.maxEntries {
		return ip
	}
	l.purgeLocked()
	if _, ok := l.entries[ip]; ok || len(l.entries) < l.maxEntries {
		return ip
	}
	return overflowKey
}

func (l *failureLimiter) purgeLocked() {
	now := l.now()
	for key, e := range l.entries {
		if now.Sub(e.start) >= l.window {
			delete(l.entries, key)
		}
	}
}

// blocked reports whether ip has used up its failures in the current window.
func (l *failureLimiter) blocked(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[l.keyLocked(ip)]
	if !ok {
		return 0, false
	}
	elapsed := l.now().Sub(e.start)
	if elapsed >= l.window {
		return 0, false
	}
	if e.count >= l.max {
		return l.window - elapsed, true
	}
	return 0, false
}

func (l *failureLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := l.keyLocked(ip)
	e, ok := l.entries[key]
	if !ok || l.now().Sub(e.start) >= l.window {
		l.entries[key] = &failureEntry{count: 1, start: l.now()}
		return
	}
	e.count++
}

func (l *failureLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
