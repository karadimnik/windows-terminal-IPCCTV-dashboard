// Command holocron is the single executable of the Holocron NVR (rule R1): every action - start,
// stop, status, cameras, configuration, logs, log levels, diagnostics - is a subcommand, and every
// subcommand is a cli.Action, so its start and result are logged (rule R2). See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"

	"holocron/internal/app"
	"holocron/internal/cli"
	"holocron/internal/config"
	"holocron/internal/ctlapi"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/supervise"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes (LLD 2.12).
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitAnother     = supervise.ExitAnotherSupervisor // 20
	exitConfigFault = supervise.ExitConfigFault       // 21
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, nil))
}

// ctl is the part of the control client the commands use (*ctlapi.Client; a fake in tests).
type ctl interface {
	State(ctx context.Context) (ctlapi.State, error)
	Stop(ctx context.Context, kind string) (ctlapi.StopReport, error)
	Reload(ctx context.Context) error
	SetLive(ctx context.Context, on bool) (ctlapi.Changed, error)
	Levels(ctx context.Context) (ctlapi.Levels, error)
	SetLevel(ctx context.Context, component, level string) (ctlapi.Levels, error)
}

// env is one invocation: the global flags, the resolved paths and configuration, the seams.
type env struct {
	opts    app.Options
	json    bool
	out     io.Writer
	errw    io.Writer
	args    []string // the command's own arguments
	global  []string // the global flags as given (passed on to the roles)
	paths   app.Paths
	cfg     config.Config
	warns   []config.Warning
	loadErr error

	dial      func(root string) (ctl, error)
	inspector osx.Inspector
	exe       string // this executable (re-run as host / supervisor)
	lg        *logx.Logger
	stdin     io.Reader              // secrets are read from here and nowhere else
	opener    func(url string) error // opens a browser (a fake in tests)
}

type envKey struct{}

func envFrom(ctx context.Context) *env { return ctx.Value(envKey{}).(*env) }

// exitError carries an exit code; its message is printed unless quiet (already explained on
// stdout) or empty. The result record always carries it.
type exitError struct {
	code  int
	err   error
	quiet bool
}

func (e *exitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}
func (e *exitError) Unwrap() error { return e.err }

func usageErr(format string, a ...any) error {
	return &exitError{code: exitUsage, err: fmt.Errorf(format, a...)}
}

func failed(format string, a ...any) error {
	return &exitError{code: exitFailure, err: fmt.Errorf(format, a...)}
}

// quietFail is a failure whose explanation was already printed; msg goes to the result record.
func quietFail(format string, a ...any) error {
	return &exitError{code: exitFailure, err: fmt.Errorf(format, a...), quiet: true}
}

// ErrNotBuilt: the command belongs to a later task.
var ErrNotBuilt = errors.New("not built yet")

// codeOf maps an action's error to the exit code.
func codeOf(err error) int {
	var ee *exitError
	var cf *supervise.ConfigFault
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, app.ErrSandbox):
		return exitUsage
	case errors.As(err, &cf):
		return exitConfigFault
	case errors.Is(err, supervise.ErrAnotherSupervisor):
		return exitAnother
	}
	return exitFailure
}

// seams lets tests replace the dial, the inspector and the executable.
type seams struct {
	dial      func(root string) (ctl, error)
	inspector osx.Inspector
	exe       string
	platform  osx.Platform
	getenv    func(string) string
	stdin     io.Reader
	opener    func(url string) error
}

func run(ctx context.Context, argv []string, out, errw io.Writer, s *seams) int {
	g := flag.NewFlagSet("holocron", flag.ContinueOnError)
	g.SetOutput(io.Discard)
	root := g.String("root", "", "archive root (default: RootPath of the configuration)")
	cfgPath := g.String("config", "", "holocron.config.json (cameras.json sits next to it)")
	v := g.Bool("v", false, "DEBUG for this invocation, records mirrored to stderr")
	vv := g.Bool("vv", false, "TRACE for this invocation, records mirrored to stderr")
	jsonOut := g.Bool("json", false, "JSON output (read commands)")
	help := g.Bool("help", false, "show the command tree")
	g.BoolVar(help, "h", false, "show the command tree")
	if err := g.Parse(argv); err != nil {
		fmt.Fprintf(errw, "holocron: %v\n\n", err)
		printHelp(errw)
		return exitUsage
	}
	rest := g.Args()
	if *help || len(rest) == 0 {
		printHelp(out)
		if len(rest) == 0 && !*help {
			return exitUsage
		}
		return exitOK
	}
	cmd, args := matchCommand(rest)
	if cmd == nil {
		fmt.Fprintf(errw, "holocron: unknown command %q\n\n", strings.Join(rest, " "))
		printHelp(errw)
		return exitUsage
	}
	if s == nil {
		s = &seams{}
	}
	e := &env{opts: app.Options{Root: *root, ConfigPath: *cfgPath, Platform: s.platform, Getenv: s.getenv}, json: *jsonOut,
		out: out, errw: errw, args: args, global: globalArgs(*root, *cfgPath, *v, *vv), dial: s.dial, inspector: s.inspector, exe: s.exe, stdin: s.stdin, opener: s.opener}
	if *vv {
		e.opts.Verbose = 2
	} else if *v {
		e.opts.Verbose = 1
	}
	if e.opts.Platform == nil {
		e.opts.Platform = osx.Current()
	}
	if e.opts.Getenv == nil {
		e.opts.Getenv = os.Getenv
	}
	if e.dial == nil {
		e.dial = func(root string) (ctl, error) { return ctlapi.Dial(root) }
	}
	if e.inspector == nil {
		e.inspector = osx.OSInspector{}
	}
	if e.stdin == nil {
		e.stdin = os.Stdin
	}
	var lg *logx.Logger
	if cmd.noRoot {
		lg, _ = app.NewLogger(app.LogOptions{NoFile: true, Verbose: e.opts.Verbose, Stderr: verboseStderr(e), Getenv: e.opts.Getenv, Config: config.Defaults(e.opts.Platform)})
	} else {
		e.paths, e.cfg, e.warns, e.loadErr = app.Resolve(e.opts.Root, e.opts.ConfigPath, e.opts.Platform)
		if errors.Is(e.loadErr, app.ErrSandbox) || (e.loadErr == nil && e.paths.Root == "") {
			fmt.Fprintln(errw, "holocron:", e.loadErr)
			return exitUsage
		}
		if e.loadErr != nil {
			e.cfg = config.Defaults(e.opts.Platform)
			if e.opts.Root != "" {
				e.cfg.RootPath = e.opts.Root
			}
			if e.paths.Root == "" {
				e.paths.Root = e.cfg.RootPath
			}
		}
		lg, _ = app.NewLogger(app.LogOptions{Paths: e.paths, Config: e.cfg, Getenv: e.opts.Getenv, Verbose: e.opts.Verbose, Stderr: verboseStderr(e)})
	}
	defer lg.Close()
	e.lg = lg
	runner := cli.NewRunner(nil, lg)
	actx := cli.WithActor(context.WithValue(ctx, envKey{}, e), cli.CLIActor())
	_, err := runner.Do(actx, cmd.action, cli.Input{Target: targetOf(cmd, args), Args: map[string]any{"args": args}})
	var ee *exitError
	if err != nil && err.Error() != "" && !(errors.As(err, &ee) && ee.quiet) {
		fmt.Fprintln(errw, "holocron:", err)
	}
	return codeOf(err)
}

func verboseStderr(e *env) io.Writer {
	if e.opts.Verbose > 0 {
		return e.errw
	}
	return nil
}

func globalArgs(root, cfg string, v, vv bool) []string {
	var a []string
	if root != "" {
		a = append(a, "--root", root)
	}
	if cfg != "" {
		a = append(a, "--config", cfg)
	}
	if vv {
		a = append(a, "-vv")
	} else if v {
		a = append(a, "-v")
	}
	return a
}

// targetOf is what the result record names: the first positional argument, if any.
func targetOf(c *command, args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, "holocron %s - the one executable of the Holocron NVR\n\n", version)
	fmt.Fprintln(w, "usage: holocron [--root DIR] [--config FILE] [-v|-vv] [--json] <command> [arguments]")
	fmt.Fprintln(w, "\ncommands:")
	var lines []string
	width := 0
	for _, c := range commands {
		if c.role {
			continue
		}
		u := c.path
		if c.usage != "" {
			u += " " + c.usage
		}
		if len(u) > width {
			width = len(u)
		}
	}
	for _, c := range commands {
		if c.role {
			continue
		}
		u := c.path
		if c.usage != "" {
			u += " " + c.usage
		}
		about := c.about
		if c.notBuilt {
			about += " (not built yet)"
		}
		lines = append(lines, fmt.Sprintf("  %-*s  %s", width, u, about))
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	fmt.Fprintln(w, "\nexit codes: 0 ok, 1 failure (NOT RECORDING, survivors after down), 2 usage, 20 another supervisor holds the archive, 21 configuration fault")
	fmt.Fprintf(w, "logs: <root>/_logs/holocron.log and audit.log; HOLOCRON_LOG=debug or supervisor=debug,web=info; %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

// sortedKeys is a helper for stable output.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
