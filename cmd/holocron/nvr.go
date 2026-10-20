package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"holocron/internal/alerts"
	"holocron/internal/app"
	"holocron/internal/osx"
	"holocron/internal/status"
	"holocron/internal/web"
)

// upWait is how long `up --detach` waits for a fresh heartbeat (G10, IS 3.1: 60 s).
var upWait = 60 * time.Second

func upFlags(e *env, name string) (detach, noWeb bool, err error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	d := fs.Bool("detach", false, "start in the background")
	only := fs.String("only", "", "host (no web app)")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return false, false, err
	}
	if err := wantArgs(name, pos, 0, 0); err != nil {
		return false, false, err
	}
	switch *only {
	case "":
	case "host":
		noWeb = true
	case "web":
		return false, false, usageErr("holocron %s --only web: run the web app alone with `holocron web`", name)
	default:
		return false, false, usageErr("holocron %s: --only takes host", name)
	}
	return *d, noWeb, nil
}

func (e *env) self() (string, error) {
	if e.exe != "" {
		return e.exe, nil
	}
	return os.Executable()
}

func cmdUp(ctx context.Context, e *env) error {
	detach, noWeb, err := upFlags(e, "up")
	if err != nil {
		return err
	}
	return up(ctx, e, detach, noWeb)
}

func up(ctx context.Context, e *env, detach, noWeb bool) error {
	if err := e.needConfig(); err != nil {
		return err
	}
	if doc, ok, _ := app.Running(e.paths, e.cfg, time.Now()); ok {
		fmt.Fprintf(e.out, "already running: supervisor pid %d (root %s)\n", doc.SupervisorPid, e.paths.Root)
		return nil
	}
	exe, err := e.self()
	if err != nil {
		return failed("cannot find this executable: %v", err)
	}
	if !detach {
		// attached: the host runs in this process; closing the terminal stops everything
		fmt.Fprintf(e.out, "holocron up: recording root %s; close this window or press Ctrl+C to stop\n", e.paths.Root)
		code := app.RunHost(ctx, app.HostOptions{Options: e.opts, Exe: exe, SupArgs: e.global, NoWeb: noWeb})
		if code != 0 {
			return &exitError{code: code, err: fmt.Errorf("the host ended with exit %d (see %s)", code, e.logFile())}
		}
		return nil
	}
	started := time.Now()
	args := append(append([]string{}, e.global...), "host", "--service")
	if noWeb {
		args = append(args, "--no-web")
	}
	pid, note, err := app.Detach(exe, args)
	if err != nil {
		return failed("could not start the background host: %v", err)
	}
	if note != "" {
		fmt.Fprintln(e.errw, "warning:", note)
	}
	fmt.Fprintf(e.out, "started the background host, pid %d; waiting up to %ds for the supervisor's heartbeat\n", pid, int(upWait.Seconds()))
	doc, ok, why := app.WaitRunning(ctx, e.paths, e.cfg, started, upWait, 500*time.Millisecond)
	if !ok {
		fmt.Fprintf(e.out, "NOT RECORDING: %s (see %s)\n", why, e.logFile())
		return quietFail("NOT RECORDING: %s", why)
	}
	rec := 0
	if cams, err := app.LoadCameras(e.paths.Cameras); err == nil {
		for _, c := range cams.List {
			if c.Record {
				rec++
			}
		}
	}
	fmt.Fprintf(e.out, "RECORDING: supervisor pid %d is running after %.1fs; %d camera(s) set to record (holocron status shows each)\n", doc.SupervisorPid, time.Since(started).Seconds(), rec)
	if !noWeb && e.cfg.Web.Enabled {
		fmt.Fprintln(e.out, webLine(e.cfg.Web.Listen, 10*time.Second))
	}
	return nil
}

// webLine waits briefly for the web app's listener and says what it found; it never fails `up`.
func webLine(listen string, wait time.Duration) string {
	deadline := time.Now().Add(wait)
	for {
		c, err := net.DialTimeout("tcp", listen, 500*time.Millisecond)
		if err == nil {
			c.Close()
			return "web app: http://" + listen + "/ is answering"
		}
		if time.Now().After(deadline) {
			return "web app: not answering on " + listen + " yet (the host restarts it; see holocron logs --component web)"
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (e *env) logFile() string { return e.paths.Logs() + string(os.PathSeparator) + "holocron.log" }

func cmdDown(ctx context.Context, e *env) error {
	if err := wantArgs("down", e.args, 0, 0); err != nil {
		return err
	}
	return down(ctx, e)
}

func down(ctx context.Context, e *env) error {
	res := app.Down(ctx, e.paths, e.cfg, func(root string) (app.Stopper, error) { return e.dial(root) }, e.inspector)
	if e.json {
		printJSON(e.out, map[string]any{"ok": res.OK(), "before": res.Before, "after": res.After, "via": res.Via, "apiError": res.APIError,
			"report": res.Report, "reaped": res.Reaped, "elapsedMs": res.Elapsed.Milliseconds()})
	} else {
		if res.Before.Count() == 0 {
			fmt.Fprintf(e.out, "nothing is running for %s\n", e.paths.Root)
			return nil
		}
		fmt.Fprintf(e.out, "before: %d process(es): %s\n", res.Before.Count(), strings.Join(res.Before.Lines(), "; "))
		for _, v := range res.Reaped {
			if v.Killed {
				fmt.Fprintf(e.out, "orphan reaped: %s pid %d (%s; %s)\n", v.Entry.Role, v.Entry.Pid, v.Entry.Image, v.Why)
			} else {
				fmt.Fprintf(e.out, "orphan entry left alone: %s pid %d (%s)\n", v.Entry.Role, v.Entry.Pid, v.Why)
			}
		}
		switch res.Via {
		case "reap":
		case "api":
			fmt.Fprintf(e.out, "stopped through the control API: %s (%s)\n", res.Report.Kind, res.Report.Reason)
			if len(res.Report.Survivors) > 0 {
				fmt.Fprintf(e.out, "the supervisor reports surviving children: %v\n", res.Report.Survivors)
			}
		default:
			fmt.Fprintf(e.out, "the control API is not reachable (%s): wrote %s\n", res.APIError, e.paths.StopFile())
		}
	}
	if !res.OK() {
		if !e.json {
			fmt.Fprintf(e.out, "DID NOT STOP after %.1fs: %s\n", res.Elapsed.Seconds(), strings.Join(res.After.Lines(), "; "))
		}
		return quietFail("DID NOT STOP: %s", strings.Join(res.After.Lines(), "; "))
	}
	if !e.json {
		fmt.Fprintf(e.out, "stopped: 0 survivors after %.1fs\n", res.Elapsed.Seconds())
	}
	return nil
}

func cmdRestart(ctx context.Context, e *env) error {
	detach, noWeb, err := upFlags(e, "restart")
	if err != nil {
		return err
	}
	if err := down(ctx, e); err != nil {
		return err
	}
	return up(ctx, e, detach, noWeb)
}

// statusReport is `status --json`.
type statusReport struct {
	Running   bool             `json:"running"`
	Reason    string           `json:"reason"`
	Root      string           `json:"root"`
	Host      *osx.ChildEntry  `json:"host"`
	Alerts    map[string]bool  `json:"alerts"`
	Autostart string           `json:"autostart"`
	Status    *status.Document `json:"status"`
	Web       webReport        `json:"web"`
}

// webReport is the web role as `status` sees it: found by identity in the host's aux registry
// (pid + start time + image), the addresses and the login from the configuration and the user file.
type webReport struct {
	Enabled   bool   `json:"enabled"`
	Running   bool   `json:"running"`
	Pid       int    `json:"pid,omitempty"`
	Listen    string `json:"listen"`
	TLS       bool   `json:"tls"`
	TLSListen string `json:"tlsListen,omitempty"`
	Login     bool   `json:"login"`
	LoginNote string `json:"loginNote,omitempty"`
}

func webStatus(e *env) webReport {
	w := webReport{Enabled: e.cfg.Web.Enabled, Listen: e.cfg.Web.Listen, TLS: e.cfg.Web.TLSListen != "", TLSListen: e.cfg.Web.TLSListen}
	entries, err := osx.NewRegistry(e.paths.AuxFile()).Load()
	if err == nil {
		for _, c := range entries {
			if alive, _ := osx.Verify(c, e.inspector, goos()); alive && c.Role == "web" {
				w.Running, w.Pid = true, c.Pid
			}
		}
	}
	n, err := web.LoginUsers(e.cfg.Web.AuthDir)
	w.Login = n > 0
	if err != nil {
		w.LoginNote = "the user file cannot be read: " + err.Error()
	}
	return w
}

func alertChannels(c alerts.Config) map[string]bool {
	on := func(b *bool) bool { return b == nil || *b }
	return map[string]bool{"log": true, "webhook": c.Webhook.Enabled, "push": c.Push.Enabled, "desktop": on(c.Desktop.Enabled), "eventlog": on(c.EventLog.Enabled)}
}

func cmdStatus(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "status"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("status", pos, 0, 0); err != nil {
		return err
	}
	doc, running, why := app.Running(e.paths, e.cfg, time.Now())
	if errors.Is(readErr(e), status.ErrNoStatus) {
		why = "no status file has been written yet"
	}
	rep := statusReport{Running: running, Reason: why, Root: e.paths.Root, Alerts: alertChannels(e.cfg.Alerts), Autostart: "not built yet", Status: doc, Web: webStatus(e)}
	if h, ok := app.ReadHostRecord(e.paths); ok {
		if alive, _ := osx.Verify(h, e.inspector, goos()); alive {
			rep.Host = &h
		}
	}
	if e.json {
		printJSON(e.out, rep)
	} else {
		printStatus(e, rep)
	}
	if !running {
		return quietFail("not running: %s", why)
	}
	return nil
}

func readErr(e *env) error {
	_, err := status.Read(e.paths.StatusFile())
	return err
}

func printStatus(e *env, r statusReport) {
	w := e.out
	fmt.Fprintf(w, "holocron status - root %s\n", r.Root)
	d := r.Status
	if r.Running {
		fmt.Fprintf(w, "supervisor: RUNNING pid %d since %s, heartbeat %s ago\n", d.SupervisorPid, stamp(d.StartedAt), ago(d.UpdatedAt))
	} else {
		fmt.Fprintf(w, "supervisor: NOT RUNNING (%s)\n", r.Reason)
		if d != nil && d.State == status.StateStopped {
			fmt.Fprintf(w, "  last stop: %s at %s\n", orDash(d.StopKind), stamp(d.UpdatedAt))
		}
	}
	if r.Host != nil {
		fmt.Fprintf(w, "host: pid %d\n", r.Host.Pid)
	} else {
		fmt.Fprintln(w, "host: not running")
	}
	printWeb(w, r.Web)
	if d == nil {
		fmt.Fprintf(w, "alerts: %s\nautostart: %s\n", channelText(r.Alerts), r.Autostart)
		return
	}
	fmt.Fprintf(w, "cameras (%d):\n", len(d.Cameras))
	for _, c := range d.Cameras {
		state := "off"
		switch {
		case c.Record && c.Alive:
			state = "recording"
		case c.Record:
			state = "DOWN"
		}
		line := fmt.Sprintf("  %-20s %-9s %-6s", c.Name, state, orDash(c.InputMode))
		if c.Pid != nil {
			line += fmt.Sprintf(" pid %d", *c.Pid)
		}
		line += fmt.Sprintf(" restarts %d (%d last hour)", c.RestartsTotal, c.RestartsLastHour)
		if c.Record && !c.Alive && c.LastError != "" {
			line += " last error: " + c.LastError
		}
		if c.Record && !c.Alive && c.SecondsSinceLastClosedSegment != nil {
			line += fmt.Sprintf("; last segment closed %.0fs ago", *c.SecondsSinceLastClosedSegment)
		}
		fmt.Fprintln(w, line)
	}
	if rs := d.Restreamer; rs != nil {
		if rs.Enabled {
			fmt.Fprintf(w, "relay: alive=%v%s, %d path(s)%s\n", rs.Alive, pidText(rs.Pid), len(rs.Paths), errText(rs.LastError))
		} else {
			fmt.Fprintln(w, "relay: off (cameras are read directly)")
		}
	}
	if l := d.Live; l != nil && l.Enabled {
		fmt.Fprintf(w, "live view: on, alive=%v%s%s\n", l.Alive, pidText(l.Pid), errText(l.LastError))
	} else {
		fmt.Fprintln(w, "live view: off")
	}
	if k := d.Disk; k != nil {
		line := fmt.Sprintf("disk: free %.2f GB, reserve %g GB", k.FreeGB, k.ReserveGB)
		if k.ArchiveGB != nil {
			line += fmt.Sprintf(", archive %.2f GB", *k.ArchiveGB)
		}
		if k.ProjectedDaysRemaining != nil {
			line += fmt.Sprintf(", about %.1f days left at %.2f GB/day", *k.ProjectedDaysRemaining, *k.MeasuredGBPerDay)
		} else {
			line += ", days left: not measured yet"
		}
		fmt.Fprintln(w, line)
	}
	if p := d.Prune; p != nil {
		fmt.Fprintf(w, "prune: last run %s, deleted %d, freed %.2f GB, blocked %v%s\n", stamp(p.LastRunAt), p.DeletedTotal, p.FreedGBTotal, p.Blocked, errText(p.LastMessage))
	}
	fmt.Fprintf(w, "alerts: %s\nautostart: %s\n", channelText(r.Alerts), r.Autostart)
}

func printWeb(w io.Writer, r webReport) {
	switch {
	case !r.Enabled:
		fmt.Fprintln(w, "web app: off (web.enabled is false)")
		return
	case r.Running:
		fmt.Fprintf(w, "web app: running pid %d on http://%s/", r.Pid, r.Listen)
	default:
		fmt.Fprintf(w, "web app: not running (would listen on http://%s/)", r.Listen)
	}
	if r.TLS {
		fmt.Fprintf(w, ", HTTPS login listener on %s", r.TLSListen)
	} else {
		fmt.Fprint(w, ", HTTPS listener off")
	}
	fmt.Fprintf(w, ", login %s\n", map[bool]string{true: "exists", false: "none yet"}[r.Login])
}

func channelText(m map[string]bool) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		v := "off"
		if m[k] {
			v = "on"
		}
		parts = append(parts, k+" "+v)
	}
	return strings.Join(parts, ", ")
}

func stamp(t status.Time) string {
	if t.IsNull() {
		return "-"
	}
	return t.Time().Format("2006-01-02 15:04:05 -07:00")
}

func ago(t status.Time) string {
	if t.IsNull() {
		return "-"
	}
	return time.Since(t.Time()).Round(100 * time.Millisecond).String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func pidText(p *int64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf(" pid %d", *p)
}

func errText(s string) string {
	if s == "" {
		return ""
	}
	return " - " + s
}

// reloadIfRunning asks a running supervisor to re-read its files now (same action id).
func reloadIfRunning(ctx context.Context, e *env) {
	if _, ok, _ := app.Running(e.paths, e.cfg, time.Now()); !ok {
		fmt.Fprintln(e.out, "the supervisor is not running; the change applies at the next start")
		return
	}
	c, err := e.dial(e.paths.Root)
	if err == nil {
		err = c.Reload(ctx)
	}
	if err != nil {
		fmt.Fprintf(e.out, "the supervisor did not take a reload (%v); it picks cameras.json up within %gs\n", err, e.cfg.ReconcileSeconds)
		return
	}
	fmt.Fprintln(e.out, "the running supervisor has reloaded")
}

// dialRunning returns a client when a supervisor is running and its endpoint answers files exist.
func dialRunning(e *env) (ctl, bool) {
	if _, ok, _ := app.Running(e.paths, e.cfg, time.Now()); !ok {
		return nil, false
	}
	c, err := e.dial(e.paths.Root)
	if err != nil {
		return nil, false
	}
	return c, true
}

func cmdRoleHost(ctx context.Context, e *env) error {
	fs := flag.NewFlagSet("host", flag.ContinueOnError)
	service := fs.Bool("service", false, "service mode (detached or under the OS service manager)")
	noWeb := fs.Bool("no-web", false, "do not start the web role")
	if _, err := parseFlags(fs, e.args); err != nil {
		return err
	}
	o := e.opts
	if !*service {
		o.Stderr = e.errw
	}
	exe, err := e.self()
	if err != nil {
		return failed("%v", err)
	}
	if code := app.RunHost(ctx, app.HostOptions{Options: o, Service: *service, NoWeb: *noWeb, Exe: exe, SupArgs: e.global}); code != 0 {
		return &exitError{code: code}
	}
	return nil
}

func cmdRoleSupervisor(ctx context.Context, e *env) error {
	fs := flag.NewFlagSet("supervisor", flag.ContinueOnError)
	attached := fs.Bool("attached", false, "mirror records to stderr")
	if _, err := parseFlags(fs, e.args); err != nil {
		return err
	}
	o := e.opts
	if *attached || o.Verbose > 0 {
		o.Stderr = e.errw
	}
	r, err := app.NewSupervisor(o)
	if err != nil {
		return err
	}
	// a signal to the supervisor itself is an interrupted stop, still in order (the host normally
	// stops it through stop.request)
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if code := r.Run(ctx); code != 0 {
		return &exitError{code: code}
	}
	return nil
}

func goos() string { return runtime.GOOS }
