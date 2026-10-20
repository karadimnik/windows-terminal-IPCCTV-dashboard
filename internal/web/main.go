// Package web is the local control UI of the recorder (the former camui): the loopback page, the
// LAN login on the TLS listener, Live, Footage and Settings.
//
// It owns no state. Status comes from reading the recorder's status.json; control actions go
// through a Controller (the Go supervisor's control API, or the PowerShell scripts for the old
// system). The UI is embedded in the binary, so there is nothing to deploy beside it and nothing
// to fetch from the internet at runtime.
//
// Run serves until its context ends; ParseFlags keeps every camui flag and config-file key;
// Main is camui.exe's whole main.
package web

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"holocron/internal/cli"
)

//go:embed static
var staticFiles embed.FS

// applyScript is the settings writer, embedded so the binary stays self-contained. It is written
// to a temporary file for the duration of one call - see applySettings in settings.go.
//
//go:embed scripts/camui-apply.ps1
var applyScript string

// version is stamped at build time with -ldflags "-X holocron/internal/web.version=...".
var version = "dev"

// Options is everything Run needs, resolved by ParseFlags (or built by a caller that already has a
// Config).
type Options struct {
	Config Config
	// Logs receives the web app's records (components web and auth) and runs its Actions; the
	// request lines are then DEBUG records of component web. Nil is camui.exe's way: text lines on
	// standard output, request lines at INFO.
	Logs cli.LogSource
	// Controller carries out Start/Stop/record toggles; nil chooses per request (control.go).
	Controller Controller
	// Launch lets the Start button run `holocron up --detach` (the holocron web role sets it).
	Launch *HolocronLaunch
	// GoSystem marks the holocron web role: with no supervisor answering, the page talks to the Go
	// system's controller (not PowerShell) and settings writes are refused (they are PowerShell).
	GoSystem bool
	// StopFile, when set, ends Run as soon as the file exists (how the host stops the web role on
	// every OS; removed by the host).
	StopFile string

	// One-shot commands of camui.exe, handled by RunOneShot before any listener starts.
	ShowVersion bool
	PrintCA     bool
	SetPassword string
	Unlock      string
}

// ParseFlags reads camui's command line and configuration (the -config file, CAMUI_TOKEN and the
// other environment variables, the defaults). A configuration or flag problem is a *UsageError.
func ParseFlags(args []string) (Options, error) { return parseFlags(args, io.Discard) }

// UsageError is a command line or configuration refusal; camui exits 2 on it.
type UsageError struct {
	Err    error
	Config bool // the configuration file or environment, logged as a record like camui always did
}

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

func parseFlags(args []string, output io.Writer) (Options, error) {
	var (
		o            Options
		flags        Config
		configPath   string
		tlsReadOnlyF string
	)
	fs := flag.NewFlagSet("camui", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&flags.Listen, "listen", "", "host:port to bind (default "+defaultListen+"; a non-loopback address requires a token)")
	fs.StringVar(&flags.ArchiveRoot, "archive-root", "", "recorder archive root (default: RootPath from recorder.config.json, else "+defaultArchiveRoot+")")
	fs.StringVar(&flags.StatusFile, "status-file", "", "status.json path (default <archive-root>\\_state\\status.json)")
	fs.StringVar(&flags.ScriptDir, "script-dir", "", "directory holding cam.ps1, cameras.json and recorder.config.json (default: the directory holding camui.exe)")
	fs.StringVar(&configPath, "config", "", "path to a JSON config file")
	fs.StringVar(&flags.TLSListen, "tls-listen", "", "private IPv4:port for the HTTPS LAN listener, e.g. 192.168.1.50:8443 (needs at least one user, see -set-password; off by default)")
	fs.StringVar(&flags.TLSDir, "tls-dir", "", "folder for the local CA and server certificate (default %LOCALAPPDATA%\\IPCCTV\\tls)")
	fs.StringVar(&tlsReadOnlyF, "tls-readonly", "", "true (default) or false: whether the HTTPS listener is view-only; false gives a signed-in user full control")
	fs.StringVar(&flags.AuthDir, "auth-dir", "", "folder for the LAN login's users.json, sessions.json and auth.log (default %LOCALAPPDATA%\\IPCCTV\\camui\\auth)")
	fs.StringVar(&o.SetPassword, "set-password", "", "create or update this user (admin) with a password read from standard input, end that user's sessions, and exit")
	fs.StringVar(&o.Unlock, "unlock", "", "clear this username's failed-login lockout in a running camui, and exit")
	fs.BoolVar(&o.PrintCA, "print-ca", false, "create the local CA if needed, print its file, SHA-256 fingerprint and how to install it on a device, and exit")
	fs.BoolVar(&o.ShowVersion, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.ShowVersion {
		return o, nil
	}
	cfg, err := LoadConfig(configPath, flags)
	if err != nil {
		return o, &UsageError{Err: err, Config: true}
	}
	if value, ok := parseBoolSetting(tlsReadOnlyF); ok {
		cfg.TLSReadOnly = &value
	} else if tlsReadOnlyF != "" {
		return o, &UsageError{Err: errors.New("-tls-readonly must be true or false")}
	}
	o.Config = cfg
	return o, nil
}

// RunOneShot performs the command-line-only requests (-version, -print-ca, -set-password, -unlock)
// and reports whether there was one; code is the process exit code then.
func RunOneShot(o Options, stdin io.Reader, stdout, stderr io.Writer) (code int, done bool) {
	switch {
	case o.ShowVersion:
		fmt.Fprintln(stdout, "camui "+version)
		return 0, true
	case o.PrintCA:
		return runPrintCA(o.Config, stdout, stderr), true
	case o.SetPassword != "":
		return runSetPassword(o.Config, o.SetPassword, stdin, stdout, stderr), true
	case o.Unlock != "":
		return runUnlock(o.Config, o.Unlock, stdout, stderr), true
	}
	return 0, false
}

// Refusal is a start refused by one of the guards (TLS settings, no user for the LAN listener,
// the loopback guard). Its text is for the Operator; camui exits 2.
type Refusal struct {
	Err    error
	Reason string
}

func (r *Refusal) Error() string { return r.Err.Error() }
func (r *Refusal) Unwrap() error { return r.Err }

// Validate applies the guards in camui's order: the TLS settings, a user for the LAN listener, the
// loopback guard. Run calls it first; nothing is bound before it passes.
func (o Options) Validate() error {
	cfg := o.Config
	if err := cfg.ValidateTLS(); err != nil {
		return &Refusal{Err: err, Reason: "TLS guard"}
	}
	if cfg.TLSListen != "" {
		if err := cfg.ValidateTLSUsers(newFileUserStore(cfg.AuthDir, time.Now)); err != nil {
			return &Refusal{Err: err, Reason: "no user"}
		}
	}
	if err := cfg.Validate(); err != nil {
		return &Refusal{Err: err, Reason: "loopback guard"}
	}
	return nil
}

// Main is camui.exe's main: flags, the one-shot commands, the guards, then Run until Ctrl+C or
// SIGTERM. It returns the exit code (0, 1 stopped with an error, 2 refused or misused).
func Main(args []string) int {
	o, err := parseFlags(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		var ue *UsageError
		if errors.As(err, &ue) {
			if ue.Config {
				newStdoutLogger().Error("configuration", "error", ue.Err)
			} else {
				fmt.Fprintln(os.Stderr, ue.Err.Error())
			}
			return 2
		}
		return 2 // the flag package already printed the problem and the usage
	}
	if code, done := RunOneShot(o, os.Stdin, os.Stdout, os.Stderr); done {
		return code
	}
	log := newStdoutLogger()
	if err := o.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		var rf *Refusal
		if errors.As(err, &rf) {
			switch rf.Reason {
			case "loopback guard":
				log.Error("refusing to start", "listen", o.Config.Listen, "reason", rf.Reason)
			default:
				log.Error("refusing to start", "tlsListen", o.Config.TLSListen, "reason", rf.Reason)
			}
		}
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, o); err != nil {
		log.Error("camui stopped", "error", err)
		return 1
	}
	return 0
}

func newStdoutLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// buildMux registers the API and the embedded UI.
func buildMux(cfg Config, log *slog.Logger, opts ...serveOpt) (*http.ServeMux, error) {
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, fmt.Errorf("embedded UI: %w", err)
	}
	mux := http.NewServeMux()
	api := NewAPI(cfg, log)
	newServeEnv(opts).apply(api)
	api.Routes(mux)
	mux.Handle("GET /", versioned(hideAuthPages(http.FileServerFS(static))))
	return mux, nil
}

// authPages are embedded beside the app but served only by their own routes: the
// login page on the LAN listener, the first-run page on loopback with no user.
// The general file server must never hand them out.
var authPages = map[string]bool{"/login.html": true, "/login.js": true, "/setup.html": true, "/setup.js": true}

func hideAuthPages(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authPages[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newHandler assembles the loopback listener's handler: the static page, the
// API, and the middleware around them. The optional legacy token check is
// outermost so an unauthenticated request learns nothing else. There is no login
// here; the only auth routes are the status answer and, while no user exists,
// the first-run page. With no AuthDir configured (a bare Config) the latter is off.
func newHandler(cfg Config, log *slog.Logger, opts ...serveOpt) (http.Handler, error) {
	env := newServeEnv(opts)
	mux, err := buildMux(cfg, log, opts...)
	if err != nil {
		return nil, err
	}
	lb := newLoopbackAuth(cfg.AuthDir, log)
	lb.runner = env.runner
	lb.routes(mux)
	return requireToken(cfg.Token, logRequestsAt(log, env.reqLevel, newGuard(cfg, log)(mux))), nil
}

// Run serves the loopback listener (and the TLS listener when configured) until ctx ends, then
// shuts both down within 5 s. The guards of Validate run first.
func Run(ctx context.Context, o Options) error {
	if err := o.Validate(); err != nil {
		return err
	}
	cfg := o.Config
	var log *slog.Logger
	opts := []serveOpt{withController(o.Controller), withLaunch(o.Launch, o.GoSystem)}
	if o.Logs != nil {
		log = o.Logs.For("web")
		opts = append(opts, withLogs(o.Logs), withRequestLevel(slog.LevelDebug))
	} else {
		log = newStdoutLogger()
	}
	handler, err := newHandler(cfg, log, opts...)
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

	tlsServer, tlsStop, err := startTLS(cfg, log, opts...)
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
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if tlsServer != nil {
			_ = tlsServer.Shutdown(sctx)
		}
		if err := server.Shutdown(sctx); err != nil {
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

// logRequests writes one line per request to stdout, which is what a service
// wrapper captures. The query string is never logged: it can carry the token.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return logRequestsAt(log, slog.LevelInfo, next)
}

// logRequestsAt is logRequests at a given level: DEBUG under holocron (LLD 2.13: request lines).
func logRequestsAt(log *slog.Logger, level slog.Level, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		log.Log(r.Context(), level, "request",
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
