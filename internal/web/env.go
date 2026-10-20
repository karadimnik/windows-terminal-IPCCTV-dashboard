package web

import (
	"log/slog"

	"holocron/internal/cli"
)

// serveEnv is what the handler constructors take beyond Config and the logger. The constructors
// accept it as options so the many callers that need none stay as they are.
type serveEnv struct {
	runner   *cli.Runner
	ctl      Controller
	launch   *HolocronLaunch
	goSystem bool
	reqLevel slog.Level
}

type serveOpt func(*serveEnv)

func newServeEnv(opts []serveOpt) serveEnv {
	e := serveEnv{reqLevel: slog.LevelInfo}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func withLogs(src cli.LogSource) serveOpt {
	return func(e *serveEnv) { e.runner = cli.NewRunner(nil, src) }
}
func withController(c Controller) serveOpt { return func(e *serveEnv) { e.ctl = c } }
func withLaunch(l *HolocronLaunch, goSystem bool) serveOpt {
	return func(e *serveEnv) { e.launch, e.goSystem = l, goSystem }
}
func withRequestLevel(l slog.Level) serveOpt { return func(e *serveEnv) { e.reqLevel = l } }

// apply configures an API.
func (e serveEnv) apply(a *API) {
	if e.runner != nil {
		a.runner = e.runner
	}
	a.ctlFixed, a.launch, a.goSystem = e.ctl, e.launch, e.goSystem
}
