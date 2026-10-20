package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"holocron/internal/cli"
)

// Rule R2 for the web app: every handler that changes something is a cli.Action and runs through
// cli.Runner, so each request leaves an action.begin (DEBUG) and one action.result (INFO for a
// success, WARN for a refusal, ERROR for a failure) carrying action, actor, target, result,
// duration_ms and audit=true. Read handlers are covered by the request line at DEBUG (logRequests).
//
// The actor is web:<username> for a signed-in LAN user, web:loopback for the loopback listener
// (which has no login) and web:anonymous for an unauthenticated request to the LAN listener (a
// sign-in attempt).

// Action names of the mutating web routes. TestEveryRouteIsClassified keeps routes and actions
// together: a mutating route registered without handleAction fails.
const (
	ActionControl     = "web.control"
	ActionRecord      = "web.record"
	ActionCamerasPut  = "web.cameras.put"
	ActionConfigPut   = "web.config.put"
	ActionAcknowledge = "web.acknowledge"
	ActionLogin       = "web.login"
	ActionLogout      = "web.logout"
	ActionSetup       = "web.setup"
	ActionLiveCreate  = "web.live.create"
	ActionLiveDelete  = "web.live.delete"
)

func init() {
	for _, name := range []string{ActionControl, ActionRecord, ActionCamerasPut, ActionConfigPut, ActionAcknowledge,
		ActionLogin, ActionLogout, ActionSetup, ActionLiveCreate, ActionLiveDelete} {
		cli.Register(cli.Action{Name: name, Component: "web", Mutating: true, Run: runHTTPCall})
	}
}

// httpCall carries one request through the Runner to the real handler.
type httpCall struct {
	h      http.HandlerFunc
	w      *statusRecorder
	r      *http.Request
	target string
	done   bool            // the handler returned (it did not panic)
	ctx    context.Context // the Runner's context: actor and action id
}

type callKey struct{}

// setTarget names what the request acts on (a camera, a scope and action); safe to call from any
// handler, and a no-op outside handleAction.
func setTarget(r *http.Request, target string) {
	if c, ok := r.Context().Value(callKey{}).(*httpCall); ok {
		c.target = target
	}
}

func runHTTPCall(ctx context.Context, in cli.Input) (cli.Output, error) {
	c := in.Args["call"].(*httpCall)
	c.ctx = ctx
	c.h(c.w, c.r)
	c.done = true
	out := cli.Output{Target: c.target}
	switch s := c.w.status; {
	case s >= 500:
		return out, fmt.Errorf("answered %d %s", s, http.StatusText(s))
	case s >= 400:
		return out, cli.Refuse("answered %d %s", s, http.StatusText(s))
	}
	return out, nil
}

// actionCtx is the context for work that outlives the request's connection (a stop takes up to
// 30 s): the Runner's values (actor, action id), no cancellation by a client that goes away.
func actionCtx(r *http.Request) context.Context {
	if c, ok := r.Context().Value(callKey{}).(*httpCall); ok && c.ctx != nil {
		return context.WithoutCancel(c.ctx)
	}
	return context.WithoutCancel(r.Context())
}

// actorOf names who is behind a request.
func actorOf(r *http.Request) string {
	if u, ok := userFrom(r); ok {
		return cli.WebActor(u.Username)
	}
	if r.TLS != nil {
		return cli.WebActor("anonymous")
	}
	return cli.WebActor("loopback")
}

// fixedLogs serves one logger for every component (a bare *slog.Logger in tests and in camui.exe).
type fixedLogs struct{ l *slog.Logger }

func (f fixedLogs) For(string) *slog.Logger { return f.l }

var (
	defaultRunnerMu sync.Mutex
	defaultRunners  = map[*slog.Logger]*cli.Runner{}
)

// runnerFor is the Runner over a plain logger (cached so a test creating many APIs stays cheap).
func runnerFor(log *slog.Logger) *cli.Runner {
	defaultRunnerMu.Lock()
	defer defaultRunnerMu.Unlock()
	if r, ok := defaultRunners[log]; ok {
		return r
	}
	r := cli.NewRunner(nil, fixedLogs{log})
	defaultRunners[log] = r
	return r
}

// action wraps a handler so that it runs as the named Action.
func action(runner *cli.Runner, name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call := &httpCall{h: h, w: &statusRecorder{ResponseWriter: w, status: http.StatusOK}}
		call.r = r.WithContext(context.WithValue(r.Context(), callKey{}, call))
		ctx := cli.WithActor(r.Context(), actorOf(r))
		// the error is already in the result record and the HTTP answer was written by the handler
		_, _ = runner.Do(ctx, name, cli.Input{Args: map[string]any{"call": call}})
		if !call.done { // the Runner recovered a panic: net/http would have dropped the connection
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}
}

// handleAction registers a mutating route. Every mutating route goes through it, never through
// mux.HandleFunc directly (the source scan of TestEveryRouteIsClassified enforces that).
func handleAction(mux *http.ServeMux, runner *cli.Runner, pattern, name string, h http.HandlerFunc) {
	mux.HandleFunc(pattern, action(runner, name, h))
}
