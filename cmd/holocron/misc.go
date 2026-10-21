package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"holocron/internal/alerts"
	"holocron/internal/app"
	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/ffmpeg"
	"holocron/internal/logx"
)

func cmdVersion(ctx context.Context, e *env) error {
	if err := wantArgs("version", e.args, 0, 0); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "holocron %s (status.json schema 1, %s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return nil
}

// ---- logs ----

// logLine is one parsed text record: "<time> <LEVEL> [<component>] <msg> k=v ...".
type logLine struct {
	Time      time.Time
	Level     string
	Component string
	Text      string
	Raw       string
}

var lineRE = regexp.MustCompile(`^(\S+) (ERROR|WARN|INFO|DEBUG|TRACE)\s+\[([^\]]*)\] (.*)$`)

func parseLogLine(s string) (logLine, bool) {
	m := lineRE.FindStringSubmatch(s)
	if m == nil {
		return logLine{Raw: s}, false
	}
	t, err := time.Parse("2006-01-02T15:04:05.999999999Z07:00", m[1])
	if err != nil {
		return logLine{Raw: s}, false
	}
	return logLine{Time: t, Level: m[2], Component: m[3], Text: m[4], Raw: s}, true
}

type logFilter struct {
	component string
	level     string
	since     time.Time
	action    string
}

func (f logFilter) match(l logLine, ok bool) bool {
	if !ok {
		return f.component == "" && f.level == "" && f.since.IsZero() && f.action == ""
	}
	if f.component != "" && !strings.EqualFold(l.Component, f.component) && !strings.HasPrefix(strings.ToLower(l.Component), strings.ToLower(f.component)+":") {
		return false
	}
	if f.level != "" {
		want, _ := logx.ParseLevel(f.level)
		have, _ := logx.ParseLevel(l.Level)
		if have < want {
			return false
		}
	}
	if !f.since.IsZero() && l.Time.Before(f.since) {
		return false
	}
	if f.action != "" && !strings.Contains(" "+l.Text+" ", " action="+f.action+" ") {
		return false
	}
	return true
}

// parseSince takes a duration back from now (10m, 2h) or an RFC 3339 time.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since takes a duration (10m, 2h) or an RFC 3339 time, got %q", s)
}

// followPoll is the follow interval of `logs -f`.
var followPoll = 500 * time.Millisecond

func cmdLogs(ctx context.Context, e *env) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow")
	var f logFilter
	fs.StringVar(&f.component, "component", "", "only this component (ffmpeg matches ffmpeg:<camera>)")
	fs.StringVar(&f.level, "level", "", "this level and above")
	since := fs.String("since", "", "a duration back (10m) or an RFC 3339 time")
	fs.StringVar(&f.action, "action", "", "only this action (camera.add, ...)")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("logs", pos, 0, 0); err != nil {
		return err
	}
	if f.level != "" {
		if _, err := logx.ParseLevel(f.level); err != nil {
			return usageErr("holocron logs: %v", err)
		}
	}
	if f.since, err = parseSince(*since, time.Now()); err != nil {
		return usageErr("holocron logs: %v", err)
	}
	path := e.logFile()
	var off int64
	for _, p := range []string{path + ".1", path} {
		n, err := copyMatching(e.out, p, 0, f)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if p == path {
			off = n
		}
	}
	if !*follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(followPoll):
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if fi.Size() < off { // rotated: the rest of the old file is in .1, new lines start at 0
			off = 0
		}
		if fi.Size() > off {
			if n, err := copyMatching(e.out, path, off, f); err == nil {
				off = n
			}
		}
	}
}

// copyMatching writes the matching complete lines of path from offset on; it returns the offset
// after the last complete line.
func copyMatching(w io.Writer, path string, from int64, f logFilter) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return from, err
	}
	defer file.Close()
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		return from, err
	}
	r := bufio.NewReaderSize(file, 64<<10)
	off := from
	for {
		line, err := r.ReadString('\n')
		if err != nil { // a partial last line is read again next time
			return off, nil
		}
		off += int64(len(line))
		text := strings.TrimRight(line, "\r\n")
		if l, ok := parseLogLine(text); f.match(l, ok) {
			fmt.Fprintln(w, text)
		}
	}
}

// ---- alert test ----

func cmdAlertTest(ctx context.Context, e *env) error {
	pos, err := parseFlags(flag.NewFlagSet("alert test", flag.ContinueOnError), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("alert test", pos, 0, 1); err != nil {
		return err
	}
	if err := e.needConfig(); err != nil {
		return err
	}
	name := ""
	if len(pos) == 1 {
		name = pos[0]
	}
	lg, _ := app.NewLogger(app.LogOptions{Paths: e.paths, Config: e.cfg, Getenv: e.opts.Getenv, Verbose: e.opts.Verbose, Stderr: verboseStderr(e)})
	defer lg.Close()
	chans, errs := alerts.BuildChannels(e.cfg.Alerts, alerts.Deps{Log: lg.For("alerts")})
	for _, err := range errs {
		fmt.Fprintf(e.out, "FAIL configuration: %v\n", err)
	}
	d := alerts.NewWith(e.cfg.Alerts, chans, lg.For("alerts"), alerts.WithRedactor(lg.Redactor().String))
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d.Close(c)
	}()
	res := d.Test(ctx, name)
	if len(res) == 0 {
		return failed("no alert channel named %q (log, webhook, push, desktop, eventlog)", name)
	}
	bad := len(errs) > 0
	for _, r := range res {
		switch {
		case !r.Enabled:
			fmt.Fprintf(e.out, "off  %s\n", r.Channel)
		case r.Err != nil:
			bad = true
			fmt.Fprintf(e.out, "FAIL %s: %v (%s)\n", r.Channel, r.Err, r.Duration.Round(time.Millisecond))
		default:
			fmt.Fprintf(e.out, "ok   %s (%s)\n", r.Channel, r.Duration.Round(time.Millisecond))
		}
	}
	if bad {
		return quietFail("an alert channel failed")
	}
	return nil
}

// ---- doctor ----

type check struct {
	verdict string // ok | warn | fail
	what    string
	detail  string
}

func cmdDoctor(ctx context.Context, e *env) error {
	if err := wantArgs("doctor", e.args, 0, 0); err != nil {
		return err
	}
	var cs []check
	add := func(v, what, format string, a ...any) { cs = append(cs, check{v, what, fmt.Sprintf(format, a...)}) }
	cfg := e.cfg
	if e.loadErr != nil {
		add("fail", "configuration", "%v", e.loadErr)
	} else {
		add("ok", "configuration", "%s (%d warning(s))", e.paths.Config, len(e.warns))
	}
	// ffmpeg
	ff := cfg.FfmpegPath
	if ff == "" {
		ff = "ffmpeg"
	}
	if v, err := ffmpeg.CheckVersion(ff); err != nil {
		add("fail", "ffmpeg", "%s: %v", ff, err)
	} else if v.Warning != "" {
		add("warn", "ffmpeg", "%s %s: %s", ff, v, v.Warning)
	} else {
		add("ok", "ffmpeg", "%s %s", ff, v)
	}
	// MediaMTX
	if cfg.RestreamerEnabled {
		cs = append(cs, mediamtx(cfg.RestreamerExePath))
	} else {
		add("ok", "mediamtx", "the relay is off (RestreamerEnabled false)")
	}
	// ports
	_, running, _ := app.Running(e.paths, cfg, time.Now())
	for _, a := range []struct{ what, url string }{{"relay RTSP", cfg.RelayBase}, {"relay API", cfg.RelayApiBase}} {
		if !cfg.RestreamerEnabled || a.url == "" {
			continue
		}
		hp, err := config.HostPort(a.url)
		if err != nil {
			add("fail", a.what, "%v", err)
			continue
		}
		cs = append(cs, portCheck(a.what, "tcp", hp, running))
	}
	if cfg.LiveViewEnabled {
		cs = append(cs, portCheck("live WHEP", "tcp", cfg.LiveWhepAddress, running))
		cs = append(cs, portCheck("live media", "udp", cfg.LiveUdpAddress, running))
	}
	// folders
	for _, d := range []string{e.paths.Root, e.paths.State(), e.paths.Logs(), filepath.Dir(e.paths.Config)} {
		cs = append(cs, writable(d))
	}
	// root
	if p := archive.CheckRoot(e.paths.Root, e.opts.Platform); p != "" {
		add("fail", "archive root", "%s", p)
	} else if ok, why := archive.MarkerValid(e.paths.Root, e.opts.Platform); !ok {
		add("warn", "archive root", "%s is usable; not claimed yet (%s) - the supervisor claims it at start", e.paths.Root, why)
	} else {
		add("ok", "archive root", "%s, claimed", e.paths.Root)
	}
	// clock
	now := time.Now()
	_, offset := now.Zone()
	switch {
	case now.Year() < 2025:
		add("fail", "clock", "the clock says %s: segment names and the pruner's age rule would be wrong", now.Format(time.RFC3339))
	default:
		add("ok", "clock", "%s (UTC offset %+d min, zone %s)", now.Format(time.RFC3339), offset/60, time.Local)
	}
	// credentials
	if c, err := app.CredentialsFor(cfg, e.opts.Getenv); err != nil {
		add("fail", "credentials", "%v", err)
	} else if _, _, err := c.CameraLogin(); err != nil {
		add("fail", "credentials", "%v", err)
	} else {
		add("ok", "credentials", "a camera login is present (backend %s; values not shown)", cfg.Secrets.Backend)
	}
	// status
	doc, ok, why := app.Running(e.paths, cfg, now)
	switch {
	case ok:
		add("ok", "status", "running, supervisor pid %d", doc.SupervisorPid)
		if doc.UpdatedAt.Time().After(now.Add(time.Minute)) {
			add("warn", "clock", "status.json was written %s in the future", doc.UpdatedAt.Time().Sub(now).Round(time.Second))
		}
	case doc == nil:
		add("warn", "status", "%s", why)
	default:
		add("warn", "status", "not running: %s (last state %s %s)", why, doc.State, doc.StopKind)
	}
	cs = append(cs, autostartChecks(e)...)
	fails := 0
	for _, c := range cs {
		if c.verdict == "fail" {
			fails++
		}
		fmt.Fprintf(e.out, "%-4s %-14s %s\n", c.verdict, c.what, c.detail)
	}
	if fails > 0 {
		fmt.Fprintf(e.out, "%d check(s) failed\n", fails)
		return quietFail("%d doctor check(s) failed", fails)
	}
	return nil
}

func mediamtx(exe string) check {
	path := exe
	if path == "" {
		path = "mediamtx"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return check{"fail", "mediamtx", fmt.Sprintf("%s: %v", path, err)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, resolved, "--version").CombinedOutput()
	v := strings.TrimSpace(firstLine(string(out)))
	if err != nil || v == "" {
		return check{"warn", "mediamtx", fmt.Sprintf("%s found, but --version said %q (%v)", resolved, v, err)}
	}
	return check{"ok", "mediamtx", fmt.Sprintf("%s %s", resolved, v)}
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// portCheck binds the address for a moment; in use is fine while our own supervisor runs.
func portCheck(what, network, addr string, running bool) check {
	var err error
	if network == "udp" {
		var c net.PacketConn
		if c, err = net.ListenPacket("udp", addr); err == nil {
			c.Close()
		}
	} else {
		var l net.Listener
		if l, err = net.Listen("tcp", addr); err == nil {
			l.Close()
		}
	}
	switch {
	case err == nil:
		return check{"ok", what, fmt.Sprintf("%s %s is free", network, addr)}
	case running:
		return check{"ok", what, fmt.Sprintf("%s %s is in use (the running supervisor's)", network, addr)}
	}
	return check{"fail", what, fmt.Sprintf("%s %s cannot be used: %v", network, addr, err)}
}

func writable(dir string) check {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return check{"fail", "folder", fmt.Sprintf("%s: %v", dir, err)}
	}
	f, err := os.CreateTemp(dir, ".holocron-doctor-*")
	if err != nil {
		return check{"fail", "folder", fmt.Sprintf("%s is not writable: %v", dir, err)}
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return check{"ok", "folder", fmt.Sprintf("%s is writable", dir)}
}
