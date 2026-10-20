package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"holocron/internal/alerts"
	"holocron/internal/host"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/supervise"
)

// HostOptions add the role's own choices to Options.
type HostOptions struct {
	Options
	Service bool     // --service (detached / OS service): every OS stop is "requested"
	NoWeb   bool     // --no-web (up --only host): the web role is not started
	Exe     string   // the holocron executable to re-run as the supervisor; default os.Executable()
	SupArgs []string // global flags passed on to the supervisor (--root, --config, -v)
}

// RunHost is the host role: it runs the supervisor as a child (`<exe> <SupArgs> supervisor`) inside
// its own process scope and restarts it within the budget (internal/host). It writes
// <state>/host.json for the census and returns the host's exit code.
func RunHost(ctx context.Context, o HostOptions) int {
	o.defaults()
	p, cfg, warns, err := Resolve(o.Root, o.ConfigPath, o.Platform)
	if err != nil {
		fmt.Fprintln(os.Stderr, "holocron host:", err)
		return supervise.ExitConfigFault
	}
	lg, _ := NewLogger(LogOptions{Paths: p, Config: cfg, Getenv: o.Getenv, Verbose: o.Verbose, Stderr: o.Stderr})
	defer lg.Close()
	hl := lg.For("host")
	for _, w := range warns {
		hl.Warn("configuration: "+w.String(), logx.Action("config.load"))
	}
	exe := o.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			hl.Error("cannot find the holocron executable: "+err.Error(), logx.Action("host.start"), logx.Result(logx.ResultFail))
			return 1
		}
	}
	scope, err := osx.NewScope("host", osx.ScopeOptions{Log: hl})
	if err != nil {
		hl.Error("cannot create the host's process scope: "+err.Error(), logx.Action("host.start"), logx.Result(logx.ResultFail), logx.Err(err))
		return 1
	}
	defer scope.Close()
	var aux []host.Aux
	if cfg.Web.Enabled && !o.NoWeb {
		aux = append(aux, webAux(p, scope, exe, o.SupArgs, hl))
	}
	removeRecord, err := WriteHostRecord(p, osx.OSInspector{})
	if err != nil {
		hl.Warn("host.json not written ("+err.Error()+"): `holocron down` cannot count the host", logx.Action("host.start"))
	}
	defer removeRecord()

	chans, _ := alerts.BuildChannels(cfg.Alerts, alerts.Deps{Log: lg.For("alerts")})
	disp := alerts.NewWith(cfg.Alerts, chans, lg.For("alerts"), alerts.WithRedactor(lg.Redactor().String))
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		disp.Close(c)
	}()

	args := append(append([]string{}, o.SupArgs...), "supervisor")
	if !o.Service {
		args = append(args, "--attached") // its records are mirrored to this terminal
	}
	sp := &host.ExecSpawner{Scope: scope, Path: exe, Args: args}
	if !o.Service && o.Stderr != nil {
		sp.Stdout, sp.Stderr = o.Stderr, o.Stderr
	}
	mode := host.Attached
	if o.Service {
		mode = host.Service
	}
	stop := time.Duration(cfg.StopTimeoutSeconds*float64(time.Second)) + 10*time.Second
	h := host.New(host.Config{StateDir: p.State(), WindowTitle: "holocron up", StopTimeout: stop},
		host.Deps{Spawner: sp, Alerts: hostAlerts{disp}, Log: hl, Mode: mode, Aux: aux})
	res, err := h.Run(ctx)
	if err != nil {
		hl.Error("host: "+err.Error(), logx.Action("host.stop"), logx.Result(logx.ResultFail), logx.Err(err))
		return 1
	}
	hl.Info(fmt.Sprintf("host done: %s (%s), %d life(s), %d restart(s), exit %d", res.Outcome, res.Reason, res.Lives, res.Restarts, res.ExitCode()),
		logx.Action("host.stop"), logx.Result(logx.ResultOK))
	return res.ExitCode()
}

type hostAlerts struct{ d *alerts.Dispatcher }

func (a hostAlerts) Raise(e host.Event) {
	a.d.Raise(alerts.FromFields(e.ID, e.Severity, e.Title, "", e.Message, e.Key))
}
