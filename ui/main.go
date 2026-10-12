// camui is the local control UI for the IPCCTV recorder.
//
// It owns no state. Status comes from reading the recorder's status.json;
// control actions (added in a later step) go through cam.ps1. The UI is
// embedded in the binary, so there is nothing to deploy beside it and nothing
// to fetch from the internet at runtime.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed static
var staticFiles embed.FS

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

// tokenCookie carries the token after a one-time ?token= bootstrap so the
// Operator does not have to keep it in the address bar.
const tokenCookie = "camui_token"

func main() {
	var (
		flags      Config
		configPath string
		showVer    bool
	)
	flag.StringVar(&flags.Listen, "listen", "", "host:port to bind (default "+defaultListen+"; a non-loopback address requires a token)")
	flag.StringVar(&flags.ArchiveRoot, "archive-root", "", "recorder archive root (default "+defaultArchiveRoot+")")
	flag.StringVar(&flags.StatusFile, "status-file", "", "status.json path (default <archive-root>\\_state\\status.json)")
	flag.StringVar(&configPath, "config", "", "path to a JSON config file")
	flag.BoolVar(&showVer, "version", false, "print the version and exit")
	flag.Parse()

	if showVer {
		fmt.Println("camui " + version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := LoadConfig(configPath, flags)
	if err != nil {
		log.Error("configuration", "error", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		// The loopback guard. Printed to stderr as well as logged, because
		// this message has to be impossible to miss.
		fmt.Fprintln(os.Stderr, err.Error())
		log.Error("refusing to start", "listen", cfg.Listen, "reason", "loopback guard")
		os.Exit(2)
	}

	if err := run(cfg, log); err != nil {
		log.Error("camui stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg Config, log *slog.Logger) error {
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return fmt.Errorf("embedded UI: %w", err)
	}

	mux := http.NewServeMux()
	NewAPI(cfg.StatusFile, log).Routes(mux)
	mux.Handle("GET /", versioned(http.FileServerFS(static)))

	handler := requireToken(cfg.Token, logRequests(log, mux))

	// Bind before announcing, so a port already in use fails loudly instead
	// of leaving a server that is not listening.
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("bind %s: %w", cfg.Listen, err)
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Info("camui listening",
		"version", version,
		"address", listener.Addr().String(),
		"statusFile", cfg.StatusFile,
		"auth", map[bool]string{true: "token required", false: "none (loopback only)"}[cfg.RequiresToken()],
	)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errc := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errc
	}
}

// versioned tags the embedded assets with the build version as their ETag and
// asks the browser to revalidate every time. An embedded file has no useful
// modification time, so without this the browser gets no validator at all,
// falls back to heuristic caching, and serves yesterday's CSS after an upgrade.
// http.ServeContent answers If-None-Match from this header, so revalidation
// costs a 304 rather than a refetch.
func versioned(next http.Handler) http.Handler {
	etag := `"` + version + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// requireToken enforces the auth token when one is configured. The token is
// accepted from a cookie, from an Authorization: Bearer header, or once from a
// ?token= query parameter, which is then moved into a cookie so it stops
// appearing in the address bar, in history and in any Referer header.
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
			http.SetCookie(w, &http.Cookie{
				Name:     tokenCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			clean := *r.URL
			params := clean.Query()
			params.Del("token")
			clean.RawQuery = params.Encode()
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

// logRequests writes one line per request to stdout, which is what a service
// wrapper captures. The query string is never logged: it can carry the token.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"ms", time.Since(started).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}
