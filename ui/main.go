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
	"path/filepath"
	"syscall"
	"time"
)

//go:embed static
var staticFiles embed.FS

// applyScript is the settings writer, embedded so camui.exe stays a single
// self-contained binary. It is written to a temporary file for the duration of
// one call - see applySettings in settings.go.
//
//go:embed scripts/camui-apply.ps1
var applyScript string

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
		printCA    bool
		newTok     bool
	)
	flag.StringVar(&flags.Listen, "listen", "", "host:port to bind (default "+defaultListen+"; a non-loopback address requires a token)")
	flag.StringVar(&flags.ArchiveRoot, "archive-root", "", "recorder archive root (default: RootPath from recorder.config.json, else "+defaultArchiveRoot+")")
	flag.StringVar(&flags.StatusFile, "status-file", "", "status.json path (default <archive-root>\\_state\\status.json)")
	flag.StringVar(&flags.ScriptDir, "script-dir", "", "directory holding cam.ps1, cameras.json and recorder.config.json (default: the directory holding camui.exe)")
	flag.StringVar(&configPath, "config", "", "path to a JSON config file")
	flag.StringVar(&flags.TLSListen, "tls-listen", "", "private IPv4:port for the HTTPS LAN listener, e.g. 192.168.1.50:8443 (needs a token of at least 16 characters; off by default)")
	flag.StringVar(&flags.TLSDir, "tls-dir", "", "folder for the local CA and server certificate (default %LOCALAPPDATA%\\IPCCTV\\tls)")
	var tlsReadOnlyFlag string
	flag.StringVar(&tlsReadOnlyFlag, "tls-readonly", "", "true (default) or false: whether the HTTPS listener is view-only; false gives its token full control")
	flag.BoolVar(&printCA, "print-ca", false, "create the local CA if needed, print its file, SHA-256 fingerprint and how to install it on a device, and exit")
	flag.BoolVar(&newTok, "new-token", false, "print a fresh 32-byte hex token for tlsToken / CAMUI_TLS_TOKEN (writes nothing) and exit")
	flag.BoolVar(&showVer, "version", false, "print the version and exit")
	flag.Parse()

	if newTok {
		token, err := newToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "no randomness:", err)
			os.Exit(1)
		}
		fmt.Println(token)
		return
	}

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
	if value, ok := parseBoolSetting(tlsReadOnlyFlag); ok {
		cfg.TLSReadOnly = &value
	} else if tlsReadOnlyFlag != "" {
		fmt.Fprintln(os.Stderr, "-tls-readonly must be true or false")
		os.Exit(2)
	}
	if printCA {
		os.Exit(runPrintCA(cfg, os.Stdout, os.Stderr))
	}
	if err := cfg.ValidateTLS(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		log.Error("refusing to start", "tlsListen", cfg.TLSListen, "reason", "TLS guard")
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

// buildMux registers the API and the embedded UI.
func buildMux(cfg Config, log *slog.Logger) (*http.ServeMux, error) {
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("embedded UI: %w", err)
	}
	mux := http.NewServeMux()
	NewAPI(cfg, log).Routes(mux)
	mux.Handle("GET /", versioned(http.FileServerFS(static)))
	return mux, nil
}

// newHandler assembles the loopback listener's handler: the static page, the
// API, and the middleware around them. The token check is outermost so an
// unauthenticated request learns nothing else.
func newHandler(cfg Config, log *slog.Logger) (http.Handler, error) {
	mux, err := buildMux(cfg, log)
	if err != nil {
		return nil, err
	}
	return requireToken(cfg.Token, logRequests(log, newGuard(cfg, log)(mux))), nil
}

func run(cfg Config, log *slog.Logger) error {
	handler, err := newHandler(cfg, log)
	if err != nil {
		return err
	}

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

	// A missing script directory is not a reason to refuse to start - status
	// is still worth serving - but it has to be said, because every action and
	// every settings write will fail until it is right.
	if _, err := os.Stat(filepath.Join(cfg.ScriptDir, "cam.ps1")); err != nil {
		log.Warn("no cam.ps1 in the script directory: control actions and settings will fail",
			"scriptDir", cfg.ScriptDir)
	}

	tlsServer, tlsStop, err := startTLS(cfg, log)
	if err != nil {
		listener.Close()
		return err
	}

	log.Info("camui listening",
		"version", version,
		"address", listener.Addr().String(),
		"archiveRoot", cfg.ArchiveRoot,
		"statusFile", cfg.StatusFile,
		"scriptDir", cfg.ScriptDir,
		"auth", map[bool]string{true: "token required", false: "none (loopback only)"}[cfg.RequiresToken()],
	)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errc := make(chan error, 2)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()
	if tlsServer != nil {
		go func() {
			err := tlsStop()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errc <- err
		}()
	}

	select {
	case err := <-errc:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if tlsServer != nil {
			_ = tlsServer.Shutdown(ctx)
		}
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

// requireToken enforces the auth token on the loopback listener: accepted from
// a cookie, from an Authorization: Bearer header, or once from a ?token= query
// parameter, which is then moved into a cookie so it stops appearing in the
// address bar, in history and in any Referer header. The LAN listener uses the
// stricter requireTokenPolicy in token.go.
func requireToken(token string, next http.Handler) http.Handler {
	return requireTokenPolicy(tokenPolicy{Token: token}, next)
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
