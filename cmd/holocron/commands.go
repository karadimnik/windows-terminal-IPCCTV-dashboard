package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"holocron/internal/cli"
)

// command is one entry of the tree. Every command is registered as a cli.Action named action.
type command struct {
	path     string // words after "holocron", e.g. "cam add"
	usage    string
	about    string
	action   string
	mutating bool
	role     bool // hidden internal role (host, supervisor)
	noRoot   bool // touches no root (no log file either)
	notBuilt bool
	run      func(ctx context.Context, e *env) error
}

var commands []*command

func init() {
	commands = []*command{
		{path: "up", usage: "[--detach] [--only host]", about: "start everything (attached: closing the terminal stops it; --detach: in the background, waits for an honest start)", action: "nvr.up", mutating: true, run: cmdUp},
		{path: "down", about: "stop everything in order and prove it (exit 1 and the survivors if anything remains)", action: "nvr.down", mutating: true, run: cmdDown},
		{path: "restart", usage: "[--detach] [--only host]", about: "down, then up", action: "nvr.restart", mutating: true, run: cmdRestart},
		{path: "status", usage: "[--json]", about: "roles, pids, cameras, disk, prune, live view, alerts; exit 0 running, 1 not", action: "nvr.status", run: cmdStatus},

		{path: "cam list", usage: "[--json]", about: "the cameras of cameras.json", action: "camera.list", run: cmdCamList},
		{path: "cam show", usage: "<name> [--json]", about: "one camera and its recorder state", action: "camera.show", run: cmdCamShow},
		{path: "cam add", usage: "--name N --ip HOST [--quality STREAM] [--record]", about: "add a camera (validated; no credentials here)", action: "camera.add", mutating: true, run: cmdCamAdd},
		{path: "cam remove", usage: "<name>", about: "remove a camera (its recordings stay)", action: "camera.remove", mutating: true, run: cmdCamRemove},
		{path: "cam set", usage: "<name> [--name N] [--ip HOST] [--quality STREAM] [--record true|false]", about: "change a camera", action: "camera.set", mutating: true, run: cmdCamSet},
		{path: "cam enable", usage: "<name|all>", about: "record a camera", action: "camera.enable", mutating: true, run: func(ctx context.Context, e *env) error { return cmdCamRecord(ctx, e, true) }},
		{path: "cam disable", usage: "<name|all>", about: "stop recording a camera", action: "camera.disable", mutating: true, run: func(ctx context.Context, e *env) error { return cmdCamRecord(ctx, e, false) }},
		{path: "cam test", usage: "<name>", about: "TCP connect and RTSP OPTIONS to the camera (5 s, no login)", action: "camera.test", run: cmdCamTest},

		{path: "config get", usage: "<key> [--json]", about: "an effective value", action: "config.get", run: cmdConfigGet},
		{path: "config set", usage: "<key> <value>", about: "change an editable key (validated first, written atomically)", action: "config.set", mutating: true, run: cmdConfigSet},
		{path: "config unset", usage: "<key>", about: "back to the default", action: "config.unset", mutating: true, run: cmdConfigUnset},
		{path: "config validate", about: "check holocron.config.json and cameras.json", action: "config.validate", run: cmdConfigValidate},
		{path: "config show", usage: "[--json]", about: "the effective configuration (secrets masked)", action: "config.show", run: cmdConfigShow},

		{path: "live enable", about: "switch live view on (applied at once when running)", action: "live.enable", mutating: true, run: func(ctx context.Context, e *env) error { return cmdLive(ctx, e, true) }},
		{path: "live disable", about: "switch live view off", action: "live.disable", mutating: true, run: func(ctx context.Context, e *env) error { return cmdLive(ctx, e, false) }},
		{path: "live status", usage: "[--json]", about: "live view setting and process", action: "live.status", run: cmdLiveStatus},

		{path: "log level", usage: "<component|all> <level> [--save]", about: "change a log level now through the control API (--save: also in the configuration)", action: "log.level", mutating: true, run: cmdLogLevel},
		{path: "logs", usage: "[-f] [--component C] [--level L] [--since D] [--action A]", about: "read, filter and follow holocron.log", action: "logs.read", run: cmdLogs},
		{path: "alert test", usage: "[channel]", about: "send a test alert through one or every channel", action: "alert.test", mutating: true, run: cmdAlertTest},
		{path: "doctor", about: "ffmpeg, MediaMTX, ports, folders, root, clock, credentials, status: ok/warn/fail", action: "doctor.run", run: cmdDoctor},
		{path: "version", about: "build and spec versions", action: "version.show", noRoot: true, run: cmdVersion},
		{path: "help", about: "this list", action: "help.show", noRoot: true, run: func(ctx context.Context, e *env) error { printHelp(e.out); return nil }},

		{path: "host", action: "role.host", role: true, run: cmdRoleHost},
		{path: "supervisor", action: "role.supervisor", role: true, run: cmdRoleSupervisor},
	}
	for _, nb := range []struct{ path, usage, about, action string }{
		{"web", "[--listen ...] [--tls-listen ...]", "run only the web role", "web.serve"},
		{"open", "[camera]", "open the Live grid in the browser", "web.open"},
		{"user add", "<name>", "add a login user", "user.add"},
		{"user remove", "<name>", "remove a login user", "user.remove"},
		{"user passwd", "<name>", "set a user's password", "user.passwd"},
		{"user list", "", "list login users", "user.list"},
		{"user unlock", "<name>", "lift a lockout", "user.unlock"},
		{"tls ca", "", "create the local certificate authority", "tls.ca"},
		{"tls print", "", "print the CA certificate", "tls.print"},
		{"tls rotate", "", "renew the server certificate", "tls.rotate"},
		{"secret set", "<name>", "store a secret", "secret.set"},
		{"secret delete", "<name>", "delete a secret", "secret.delete"},
		{"secret import", "", "import camera_creds.xml", "secret.import"},
		{"secret list", "", "secret names (never values)", "secret.list"},
		{"install", "[--mode logon|service|systemd|launchd] [--dry-run]", "create the autostart entry", "install.run"},
		{"uninstall", "[--dry-run]", "remove what install created", "uninstall.run"},
		{"autostart enable", "", "switch the autostart entry on", "autostart.enable"},
		{"autostart disable", "", "switch the autostart entry off", "autostart.disable"},
		{"autostart status", "", "autostart state", "autostart.status"},
		{"prune", "[--dry-run]", "show or run the pruner's decision now", "prune.run"},
		{"archive stats", "", "disk, retention, per-camera size and age", "archive.stats"},
	} {
		nb := nb
		commands = append(commands, &command{path: nb.path, usage: nb.usage, about: nb.about, action: nb.action, noRoot: true, notBuilt: true,
			run: func(context.Context, *env) error {
				return failed("holocron %s: %v (it arrives with a later task of the OS-independent track)", nb.path, ErrNotBuilt)
			}})
	}
	for _, c := range commands {
		c := c
		cli.Register(cli.Action{Name: c.action, Component: "cli", Mutating: c.mutating, Run: func(ctx context.Context, in cli.Input) (cli.Output, error) {
			return cli.Output{}, c.run(ctx, envFrom(ctx))
		}})
	}
}

// exposed is every command name reachable from the command line (hidden roles included).
func exposed() cli.Exposed {
	var out cli.Exposed
	for _, c := range commands {
		out = append(out, c.action)
	}
	return out
}

// matchCommand finds the longest command path that prefixes rest.
func matchCommand(rest []string) (*command, []string) {
	var best *command
	bestLen := 0
	for _, c := range commands {
		words := strings.Fields(c.path)
		if len(words) > len(rest) || len(words) <= bestLen {
			continue
		}
		ok := true
		for i, w := range words {
			if rest[i] != w {
				ok = false
				break
			}
		}
		if ok {
			best, bestLen = c, len(words)
		}
	}
	if best == nil {
		return nil, nil
	}
	return best, rest[bestLen:]
}

// parseFlags parses flags anywhere among the arguments (the standard flag package stops at the
// first positional one) and returns the positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageErr("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// newFlags is a flag set with --json bound to the env's switch.
func newFlags(e *env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.BoolVar(&e.json, "json", e.json, "JSON output")
	return fs
}

func wantArgs(name string, pos []string, min, max int) error {
	if len(pos) < min || (max >= 0 && len(pos) > max) {
		return usageErr("holocron %s: wrong number of arguments (see holocron --help)", name)
	}
	return nil
}

func printJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// needConfig fails a command that edits or depends on a configuration that does not load.
func (e *env) needConfig() error {
	if e.loadErr != nil {
		return failed("%v", e.loadErr)
	}
	return nil
}
