package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"holocron/internal/web"
)

// webBase is the web role's starting configuration; the flags and the camui config file override it.
// LiveSecretDir is the supervisor's auth folder, so both roles open the same live-view secret.
func webBase(e *env) web.Config {
	return web.Config{
		Listen: e.cfg.Web.Listen, TLSListen: e.cfg.Web.TLSListen, TLSDir: e.cfg.Web.TLSDir, AuthDir: e.cfg.Web.AuthDir,
		ArchiveRoot: e.paths.Root, CamerasPath: e.paths.Cameras, ConfigPath: e.paths.Config,
		LiveSecretDir: e.opts.Platform.Dirs().Auth,
	}
}

// cmdWeb is the web role: `holocron up` starts it as the host's managed child, and it can be run
// by hand for diagnostics. It takes every camui flag and config-file key; the defaults come from
// holocron.config.json (web.listen, web.tlsListen, web.tlsDir, web.authDir, RootPath), the flags
// and the camui config file override them.
func cmdWeb(ctx context.Context, e *env) error {
	out := e.errw
	for _, a := range e.args {
		if a == "-h" || a == "-help" || a == "--help" {
			out = e.out
		}
	}
	o, err := web.ParseFlagsTo(e.args, out, webBase(e))
	if err != nil {
		var ue *web.UsageError
		switch {
		case errors.Is(err, flag.ErrHelp):
			return nil
		case errors.As(err, &ue):
			return usageErr("holocron web: %v", ue.Err)
		}
		return &exitError{code: exitUsage, quiet: true, err: fmt.Errorf("holocron web: %v", err)}
	}
	if code, done := web.RunOneShot(o, os.Stdin, e.out, e.errw); done {
		if code != 0 {
			return &exitError{code: code, quiet: true, err: fmt.Errorf("holocron web: exit %d", code)}
		}
		return nil
	}
	exe, err := e.self()
	if err != nil {
		return failed("%v", err)
	}
	if o.Config.Token != "" {
		e.lg.Redactor().Register(o.Config.Token)
	}
	o.Logs = e.lg
	o.GoSystem = true
	o.Settings = webSettings{e: e}
	o.Admin = newWebAdmin(e)
	o.Launch = &web.HolocronLaunch{Exe: exe, Args: e.global}
	if err := o.Validate(); err != nil {
		return usageErr("%v", err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := web.Run(ctx, o); err != nil {
		return failed("the web app stopped: %v", err)
	}
	return nil
}
