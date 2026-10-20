package ctlapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"holocron/internal/cli"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/status"
)

// Backend is what the endpoint acts on: the running supervisor and the configuration edits (the
// app package wires it). Every method is called inside a cli Action, so it is logged.
type Backend interface {
	Snapshot() *status.Document                                                  // nil when not running
	Stop(kind string) StopReport                                                 // the ordered stop; waits for it
	Reload(ctx context.Context) error                                            // re-read cameras.json and the configuration now
	SetRecord(ctx context.Context, camera string, enabled bool) (Changed, error) // edit cameras.json, then reload
	SetLive(ctx context.Context, enabled bool) (Changed, error)                  // edit LiveViewEnabled, then reload
}

// Options configure a Server.
type Options struct {
	StateDir string                // <root>/_state: ctl.addr and ctl.secret
	Backend  Backend               // required
	Levels   *logx.LevelController // GET|POST /v1/log/level
	Runner   *cli.Runner           // runs every handler as an Action (required)
	Platform osx.Platform          // SetOwnerOnly; default osx.Current()
	Log      *slog.Logger          // lifecycle lines (component ctlapi); default discard
	Redact   func(secret string)   // registers the secret with the log redactor; may be nil
	// ListenAddr is for tests; default 127.0.0.1:0. Anything but a loopback address is refused.
	ListenAddr string
}

// Server is the loopback control endpoint inside the supervisor (LLD 2.11).
type Server struct {
	o      Options
	secret string
	ln     net.Listener
	hs     *http.Server
	port   string
	mut    sync.Mutex // one state-changing action at a time (G14)
	shut   chan struct{}
	once   sync.Once
	served chan struct{}
}

// NewServer prepares a server; nothing listens until Start.
func NewServer(o Options) *Server {
	if o.Platform == nil {
		o.Platform = osx.Current()
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.ListenAddr == "" {
		o.ListenAddr = "127.0.0.1:0"
	}
	return &Server{o: o, shut: make(chan struct{}), served: make(chan struct{})}
}

// Addr is the listening address (host:port) once started.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Start listens on loopback, writes ctl.secret (owner-only) and ctl.addr atomically, and serves.
func (s *Server) Start() error {
	if s.o.Backend == nil || s.o.Runner == nil {
		return errors.New("ctlapi: a Backend and a Runner are required")
	}
	host, _, err := net.SplitHostPort(s.o.ListenAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("ctlapi: refusing to listen on %q: loopback only", s.o.ListenAddr)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	s.secret = hex.EncodeToString(b[:])
	if s.o.Redact != nil {
		s.o.Redact(s.secret)
	}
	ln, err := net.Listen("tcp", s.o.ListenAddr)
	if err != nil {
		return err
	}
	s.ln = ln
	_, s.port, _ = net.SplitHostPort(ln.Addr().String())
	if err := os.MkdirAll(s.o.StateDir, 0o700); err != nil {
		ln.Close()
		return err
	}
	if err := s.writeFile(SecretFile, s.secret); err != nil {
		ln.Close()
		return fmt.Errorf("ctlapi: writing %s: %w", SecretFile, err)
	}
	if err := s.writeFile(AddrFile, ln.Addr().String()); err != nil {
		ln.Close()
		s.removeFiles()
		return fmt.Errorf("ctlapi: writing %s: %w", AddrFile, err)
	}
	s.hs = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      5 * time.Minute, // a stop waits for the whole ordered stop
		IdleTimeout:       30 * time.Second,
		ErrorLog:          nil,
	}
	go func() {
		defer close(s.served)
		_ = s.hs.Serve(ln)
	}()
	s.o.Log.Info("control endpoint listening on "+ln.Addr().String(), logx.Action("ctlapi.start"), logx.Result(logx.ResultOK))
	return nil
}

// writeFile is temp + owner-only + rename in StateDir.
func (s *Server) writeFile(name, content string) error {
	f, err := os.CreateTemp(s.o.StateDir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := s.o.Platform.SetOwnerOnly(tmp); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.o.StateDir, name)); err != nil {
		return err
	}
	ok = true
	return nil
}

func (s *Server) removeFiles() {
	for _, n := range []string{AddrFile, SecretFile} {
		_ = os.Remove(filepath.Join(s.o.StateDir, n))
	}
}

// Stop stops accepting requests and removes ctl.addr and ctl.secret at once; requests in flight
// (a stop request among them) finish. Wait blocks until they have.
func (s *Server) Stop() {
	s.once.Do(func() {
		close(s.shut)
		s.removeFiles()
		if s.hs == nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			_ = s.hs.Shutdown(ctx)
		}()
		s.o.Log.Info("control endpoint closed", logx.Action("ctlapi.stop"), logx.Result(logx.ResultOK))
	})
}

// Wait returns when every request has finished after Stop, or when ctx ends.
func (s *Server) Wait(ctx context.Context) error {
	if s.hs == nil {
		return nil
	}
	select {
	case <-s.served:
	case <-ctx.Done():
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		_ = s.hs.Shutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Route is one exposed endpoint and the Action that serves it.
type Route struct {
	Method, Pattern, Action string
}

// Routes are every endpoint the server exposes (the registry test checks each is an Action).
var Routes = []Route{
	{"GET", "/v1/state", ActState},
	{"POST", "/v1/stop", ActStop},
	{"POST", "/v1/reload", ActReload},
	{"POST", "/v1/cameras/{name}/record", ActRecord},
	{"POST", "/v1/live", ActLive},
	{"GET", "/v1/log/level", ActLevelGet},
	{"POST", "/v1/log/level", ActLevelSet},
}

var actionIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range Routes {
		rt := rt
		mux.HandleFunc(rt.Method+" "+rt.Pattern, func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, rt) })
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// order: rebinding guard, then the key; neither says why
		if !s.hostOK(r.Host) || r.Header.Get("Origin") != "" {
			http.Error(w, "", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(HeaderKey)), []byte(s.secret)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) hostOK(host string) bool {
	return host == "127.0.0.1:"+s.port || host == "localhost:"+s.port
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, rt Route) {
	act, ok := s.o.Runner.Registry.Lookup(rt.Action)
	if !ok {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	id := r.Header.Get(HeaderActionID)
	if !actionIDRE.MatchString(id) {
		id = cli.NewActionID()
	}
	w.Header().Set(HeaderActionID, id)
	var body []byte
	if r.Method == http.MethodPost {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			s.reply(w, http.StatusUnsupportedMediaType, id, nil, errors.New("Content-Type must be application/json"))
			return
		}
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				s.reply(w, http.StatusRequestEntityTooLarge, id, nil, fmt.Errorf("the request body is over %d bytes", MaxBody))
				return
			}
			s.reply(w, http.StatusBadRequest, id, nil, errors.New("the request body could not be read"))
			return
		}
	}
	if act.Mutating {
		if !s.mut.TryLock() {
			s.reply(w, http.StatusConflict, id, nil, ErrBusy)
			return
		}
		defer s.mut.Unlock()
	}
	actor := "ctlapi"
	if a := r.Header.Get(HeaderActor); (strings.HasPrefix(a, "cli:") || strings.HasPrefix(a, "web:")) && len(a) <= 128 {
		actor = a
	}
	ctx := cli.WithActor(context.WithValue(r.Context(), serverKey{}, s), actor)
	out, err := s.o.Runner.Do(ctx, rt.Action, cli.Input{ActionID: id, Target: r.PathValue("name"), Args: map[string]any{"body": body}})
	s.reply(w, codeFor(err), id, out.Data, err)
}

func codeFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, errBadRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrNotRunning):
		return http.StatusServiceUnavailable
	case errors.Is(err, cli.ErrRefused):
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

func (s *Server) reply(w http.ResponseWriter, code int, id string, data any, err error) {
	rp := reply{OK: err == nil, ActionID: id, Data: data}
	if err != nil {
		rp.Error = err.Error()
	}
	b, _ := json.Marshal(rp)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(code)
	_, _ = w.Write(b)
}
