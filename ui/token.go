package main

import (
	"crypto/subtle"
	"net/http"
)

// tokenCookie carries the loopback listener's optional token after a one-time
// ?token= bootstrap so the Operator does not have to keep it in the address bar.
// The LAN listener does not use a token at all: it has a login (auth_service.go).
const tokenCookie = "camui_token"

// requireToken enforces the optional auth token on the loopback listener:
// accepted from a cookie, from an Authorization: Bearer header, or once from a
// ?token= query parameter, which is then moved into a cookie so it stops
// appearing in the address bar, in history and in any Referer header. With no
// token configured it passes everything through.
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(tokenCookie); err == nil && tokenMatches(token, cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if bearer, ok := bearerToken(r); ok && tokenMatches(token, bearer) {
			next.ServeHTTP(w, r)
			return
		}
		if query := r.URL.Query().Get("token"); query != "" && tokenMatches(token, query) {
			http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			clean := *r.URL
			params := clean.Query()
			params.Del("token")
			clean.RawQuery = params.Encode()
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "a token is required", http.StatusUnauthorized)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if len(value) > len(prefix) && value[:len(prefix)] == prefix {
		return value[len(prefix):], true
	}
	return "", false
}

func tokenMatches(want, got string) bool {
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
