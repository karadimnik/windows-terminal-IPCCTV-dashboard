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
	audit    bool // read-only but security-relevant: also in the audit stream
	role     bool // hidden internal role (host, supervisor)
	noRoot   bool // touches no root (no log file either)
	run      func(ctx context.Context, e *env) error
}

var commands []*command

func init() {
	commands = []*command{
		{path: "up", usage: "[--detach] [--only host]", about: "start everything: host, supervisor, relay, live relay and the web app (attached: closing the terminal stops it; --detach: in the background, waits for an honest start; --only host: no web app)", action: "nvr.up", mutating: true, run: cmdUp},
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
		{path: "doctor", usage: "[--quiet] [--json] [--group G[,G...]] [--setup-only]", about: "the health check: ok/warn/fail lines with a fix for each problem, grouped (setup, recording, connections, live, web, storage, system), then HEALTHY, DEGRADED, BROKEN or NOT RUNNING; exit 1 on any fail; read-only; --setup-only is the set-up preflight alone", action: "doctor.run", run: cmdDoctor},
		{path: "version", about: "build and spec versions", action: "version.show", noRoot: true, run: cmdVersion},
		{path: "help", about: "this list", action: "help.show", noRoot: true, run: func(ctx context.Context, e *env) error { printHelp(e.out); return nil }},

		{path: "web", usage: "[-listen H:P] [-tls-listen IP:P] [-tls-dir D] [-auth-dir D] [-tls-readonly true|false] [-config FILE] [-print-ca] [-set-password USER] [-unlock USER] [-version]", about: "run the web app alone (normally started by up): login page, Live grid, Footage, Settings; camui's flags and config file all work", action: "web.serve", run: cmdWeb},
		{path: "open", usage: "[camera] [--print]", about: "open the Live grid (or one camera) in the default browser; --print only prints the address", action: "web.open", run: cmdOpen},

		{path: "user add", usage: "<name> [--role admin|viewer] [--auth-dir D]", about: "add a login user (default role admin; a viewer watches live and footage only); the password is read from standard input", action: "user.add", mutating: true, run: func(ctx context.Context, e *env) error { return cmdUserSet(ctx, e, "add") }},
		{path: "user passwd", usage: "<name> [--auth-dir D]", about: "change a user's password (read from standard input) and end that user's sessions", action: "user.passwd", mutating: true, run: func(ctx context.Context, e *env) error { return cmdUserSet(ctx, e, "passwd") }},
		{path: "user role", usage: "<name> admin|viewer [--auth-dir D]", about: "change a user's role and end that user's sessions (the last admin cannot be demoted)", action: "user.role", mutating: true, run: cmdUserRole},
		{path: "user remove", usage: "<name> [--force] [--auth-dir D]", about: "remove a user and its sessions (not the last admin while other users remain; the last user only with --force while web.tlsListen is set)", action: "user.remove", mutating: true, run: cmdUserRemove},
		{path: "user list", usage: "[--json] [--auth-dir D]", about: "names, role and creation time (never hashes)", action: "user.list", audit: true, run: cmdUserList},
		{path: "user unlock", usage: "<name> [--auth-dir D]", about: "lift a sign-in lockout in a running web app", action: "user.unlock", mutating: true, run: cmdUserUnlock},

		{path: "tls ca", usage: "[--json] [--tls-dir D]", about: "the local CA: file, SHA-256 fingerprint and the steps to install it on a device (created when absent)", action: "tls.ca", mutating: true, run: cmdTLSCA},
		{path: "tls print", usage: "[--out FILE] [--tls-dir D]", about: "write the CA certificate (never a key) to standard output or a file", action: "tls.print", audit: true, run: cmdTLSPrint},
		{path: "tls rotate", usage: "--yes [--tls-dir D]", about: "delete the CA and server certificate; a fresh pair is made at the next web start and every device must reinstall the CA", action: "tls.rotate", mutating: true, run: cmdTLSRotate},

		{path: "secret set", usage: "<name>", about: "store a secret read from standard input (camera.user, camera.password, ...)", action: "secret.set", mutating: true, run: cmdSecretSet},
		{path: "secret delete", usage: "<name>", about: "delete a secret", action: "secret.delete", mutating: true, run: cmdSecretDelete},
		{path: "secret import", usage: "[--from camera_creds.xml]", about: "Windows: move the old camera_creds.xml login into the store", action: "secret.import", mutating: true, run: cmdSecretImport},
		{path: "secret list", usage: "[--json]", about: "secret names (never values)", action: "secret.list", audit: true, run: cmdSecretList},

		{path: "prune", usage: "[--dry-run | --now --yes] [--json]", about: "show the pruner's decision (default), or run it now when no supervisor is running", action: "prune.run", mutating: true, run: cmdPrune},
		{path: "archive stats", usage: "[--json]", about: "per camera: segments, size, oldest and newest; GB per day, free space, projected days", action: "archive.stats", run: cmdArchiveStats},
		{path: "archive compare", usage: "--old ROOT --new ROOT [--camera NAME ...] [--since TIME] [--json] [--ffprobe PATH]", about: "read-only: prove the shadow archive equals the existing one (names, clock boundaries, sizes, ffprobe durations, audio, gaps); exit 0 equal, 1 differences, 2 usage", action: "archive.compare", noRoot: true, run: cmdArchiveCompare},
		{path: "install", usage: "[--mode logon|service|systemd|launchd] [--window visible|hidden] [--dry-run] [--allow-temp] [--" + oldTasksFlag + "] [--yes] [--account A]", about: "create the autostart entry for this OS (logon task, Windows service, systemd user unit, launchd agent) and verify it; idempotent; never touches the PowerShell system's tasks", action: "install.run", mutating: true, run: cmdInstall},
		{path: "uninstall", usage: "[--dry-run] [--force]", about: "remove exactly what install created and recorded", action: "uninstall.run", mutating: true, run: cmdUninstall},
		{path: "autostart enable", usage: "[--dry-run] [--" + oldTasksFlag + "]", about: "switch the installed autostart entry on", action: "autostart.enable", mutating: true, run: func(ctx context.Context, e *env) error { return cmdAutostartSet(ctx, e, true) }},
		{path: "autostart disable", usage: "[--dry-run]", about: "switch it off without removing it", action: "autostart.disable", mutating: true, run: func(ctx context.Context, e *env) error { return cmdAutostartSet(ctx, e, false) }},
		{path: "autostart status", usage: "[--json]", about: "installed, enabled, what it runs and whether that executable exists; the PowerShell system's tasks (read only)", action: "autostart.status", audit: true, run: cmdAutostartStatus},
		{path: "host", about: "internal role: the host process that `up` starts; not for direct use", action: "role.host", role: true, run: cmdRoleHost},
		{path: "supervisor", about: "internal role: the supervisor that the host starts; not for direct use", action: "role.supervisor", role: true, run: cmdRoleSupervisor},
		{path: "service", about: "internal role: the Windows service entry point; not for direct use", action: "role.service", role: true, run: cmdRoleService},
	}
	for _, c := range commands {
		c := c
		cli.Register(cli.Action{Name: c.action, Component: "cli", Mutating: c.mutating, Audit: c.audit, Run: func(ctx context.Context, in cli.Input) (cli.Output, error) {
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
