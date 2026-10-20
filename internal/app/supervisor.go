package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"holocron/internal/alerts"
	"holocron/internal/cli"
	"holocron/internal/config"
	"holocron/internal/ctlapi"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/status"
	"holocron/internal/supervise"
)

// Options are what every role and command shares.
type Options struct {
	Root       string // --root (overrides RootPath)
	ConfigPath string // --config
	Platform   osx.Platform
	Getenv     func(string) string
	Verbose    int
	Stderr     io.Writer // attached mode or -v: records mirrored here
}

func (o *Options) defaults() {
	if o.Platform == nil {
		o.Platform = osx.Current()
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
}

// SupervisorRole is the production supervisor process: every merged package wired in.
type SupervisorRole struct {
	Paths  Paths
	Config config.Config
	Log    *logx.Logger
	Sup    *supervise.Supervisor
	Ctl    *ctlapi.Server
	Alerts *alerts.Dispatcher
}

// NewSupervisor builds the supervisor role: the configuration, the logger (holocron.log,
// audit.log, the status Events ring, stderr when attached), the alerts dispatcher, the camera
// credentials, the control endpoint and supervise.NewProduction (lock, process scope, pruner with
// the root claim, status writer, relay client). A configuration that cannot run is a
// *supervise.ConfigFault (exit 21).
func NewSupervisor(o Options) (*SupervisorRole, error) {
	o.defaults()
	p, cfg, warns, err := Resolve(o.Root, o.ConfigPath, o.Platform)
	if err != nil {
		return nil, &supervise.ConfigFault{Err: err}
	}
	ring := supervise.NewEventRing()
	lg, logErrs := NewLogger(LogOptions{Paths: p, Config: cfg, Getenv: o.Getenv, Verbose: o.Verbose, Stderr: o.Stderr, Extra: []logx.Sink{ring}})
	sl := lg.For("supervisor")
	for _, e := range logErrs {
		sl.Warn("logging: " + e.Error())
	}
	for _, w := range warns {
		sl.Warn("configuration: "+w.String(), logx.Action("config.load"))
	}
	creds, err := CredentialsFor(cfg, o.Getenv)
	if err == nil {
		err = CheckCredentials(creds)
	}
	if err != nil {
		sl.Error(err.Error(), logx.Action("supervisor.start"), logx.Result(logx.ResultRefused))
		lg.Close()
		var cf *supervise.ConfigFault
		if !errors.As(err, &cf) {
			err = &supervise.ConfigFault{Err: err}
		}
		return nil, err
	}
	chans, chErrs := alerts.BuildChannels(cfg.Alerts, alerts.Deps{Log: lg.For("alerts")})
	for _, e := range chErrs {
		lg.For("alerts").Warn("alert channel not built: "+e.Error(), logx.Action("alerts.config"), logx.Result(logx.ResultFail))
	}
	disp := alerts.NewWith(cfg.Alerts, chans, lg.For("alerts"), alerts.WithRedactor(lg.Redactor().String))

	r := &SupervisorRole{Paths: p, Config: cfg, Log: lg, Alerts: disp}
	be := &backend{paths: p, plat: o.Platform}
	r.Ctl = ctlapi.NewServer(ctlapi.Options{StateDir: p.State(), Backend: be, Levels: lg.Levels(), Runner: cli.NewRunner(nil, lg),
		Platform: o.Platform, Log: lg.For("ctlapi"), Redact: lg.Redactor().Register})
	r.Sup = supervise.NewProduction(supervise.SupervisorDeps{
		Source:      rootSource{FileSource: supervise.FileSource{ConfigPath: p.Config, CamerasPath: p.Cameras, Platform: o.Platform}, root: p.Root},
		Platform:    o.Platform,
		Alerts:      supAlerts{disp},
		Credentials: relaySource{creds},
		Logs:        lg,
		Redactor:    lg.Redactor(),
		Events:      ring,
		Control:     r.Ctl,
	}, p.Root)
	be.sup = r.Sup
	return r, nil
}

// Run runs the supervisor until its ordered stop and returns its exit code (0, 1, 20, 21).
func (r *SupervisorRole) Run(ctx context.Context) int {
	rep, err := r.Sup.Run(ctx)
	code := supervise.ExitCode(err)
	l := r.Log.For("supervisor")
	if err != nil {
		l.Error(fmt.Sprintf("supervisor exit %d: %v", code, err), logx.Action("supervisor.exit"), logx.Result(logx.ResultFail), logx.Err(err))
	} else {
		l.Info(fmt.Sprintf("supervisor exit 0 (%s)", rep.Kind), logx.Action("supervisor.exit"), logx.Result(logx.ResultOK))
	}
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.Ctl.Stop()
	_ = r.Ctl.Wait(wctx)
	_ = r.Alerts.Close(wctx)
	r.Log.Close()
	return code
}

// rootSource is FileSource with --root applied to every read, so a reload compares like with like.
type rootSource struct {
	supervise.FileSource
	root string
}

func (s rootSource) Config() (config.Config, error) {
	c, err := s.FileSource.Config()
	if err == nil {
		c.RootPath = s.root
	}
	return c, err
}

// supAlerts adapts the supervisor's events to the dispatcher.
type supAlerts struct{ d *alerts.Dispatcher }

func (a supAlerts) Raise(e supervise.Event) {
	a.d.Raise(alerts.FromFields(e.ID, e.Severity, e.Title, e.Camera, e.Message, e.Key))
}

// backend is the control endpoint's view of the supervisor and the configuration edits.
type backend struct {
	paths Paths
	plat  osx.Platform
	sup   *supervise.Supervisor
}

func (b *backend) Snapshot() *status.Document { return b.sup.Snapshot() }

func (b *backend) Stop(kind string) ctlapi.StopReport {
	rep := b.sup.Stop(kind)
	return ctlapi.StopReport{Kind: rep.Kind, Reason: rep.Reason, Survivors: rep.Survivors, Final: rep.Final}
}

func (b *backend) Reload(ctx context.Context) error {
	if err := b.sup.Reload(ctx); err != nil {
		if errors.Is(err, supervise.ErrNotRunning) {
			return fmt.Errorf("%w", ctlapi.ErrNotRunning)
		}
		return err
	}
	return nil
}

func (b *backend) SetRecord(ctx context.Context, camera string, on bool) (ctlapi.Changed, error) {
	ch, err := SetRecord(b.paths.Cameras, camera, on)
	if err != nil {
		return ctlapi.Changed{}, err
	}
	if len(ch) != 1 {
		return ctlapi.Changed{}, cli.Refuse("one camera expected, %q matched %d", camera, len(ch))
	}
	if ch[0].Changed {
		return ch[0], b.Reload(ctx)
	}
	return ch[0], nil
}

func (b *backend) SetLive(ctx context.Context, on bool) (ctlapi.Changed, error) {
	ed, err := EditConfig(b.paths.Config, "LiveViewEnabled", ParseValue(fmt.Sprint(on)), b.plat)
	if err != nil {
		return ctlapi.Changed{}, err
	}
	ch := ctlapi.Changed{Target: "LiveViewEnabled", Enabled: on, Changed: ed.Changed}
	return ch, b.Reload(ctx) // the running state may differ from the file even when the file did not change
}
