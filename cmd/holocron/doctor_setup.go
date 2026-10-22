package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"holocron/internal/app"
	"holocron/internal/archive"
	"holocron/internal/config"
)

// setupTable is the doctor of before W19: the environment a start needs.
var setupTable = []doctorCheck{
	{id: "setup.configuration", group: "setup", run: setupConfiguration},
	{id: "setup.ffmpeg", group: "setup", timeout: 10 * time.Second, run: setupFfmpeg},
	{id: "setup.mediamtx", group: "setup", timeout: 10 * time.Second, run: setupMediamtx},
	{id: "setup.ports", group: "setup", run: setupPorts},
	{id: "setup.webports", group: "setup", run: func(_ context.Context, r *drun) []check { return webPortChecks(r.e, r.d.listen) }},
	{id: "setup.folders", group: "setup", run: setupFolders},
	{id: "setup.root", group: "setup", run: setupRoot},
	{id: "setup.clock", group: "setup", run: setupClock},
	{id: "setup.credentials", group: "setup", run: setupCredentials},
	{id: "setup.status", group: "setup", run: setupStatus},
	{id: "setup.autostart", group: "setup", timeout: 10 * time.Second, run: func(_ context.Context, r *drun) []check { return autostartChecks(r.e) }},
}

func setupConfiguration(_ context.Context, r *drun) []check {
	e := r.e
	if e.loadErr != nil {
		return []check{mk(lvFail, "configuration", "%v", e.loadErr).fix("holocron config validate, then correct %s", e.paths.Config)}
	}
	return []check{mk(lvOK, "configuration", "%s (%d warning(s))", e.paths.Config, len(e.warns))}
}

func setupFfmpeg(_ context.Context, r *drun) []check {
	ff := r.e.cfg.FfmpegPath
	if ff == "" {
		ff = "ffmpeg"
	}
	const fix = "install ffmpeg 5.0 or newer and point the configuration at it: holocron config set FfmpegPath <path to ffmpeg>"
	if v, err := r.d.ffmpeg(ff); err != nil {
		return []check{mk(lvFail, "ffmpeg", "%s: %v", ff, err).fix(fix)}
	} else if v.Warning != "" {
		return []check{mk(lvWarn, "ffmpeg", "%s %s: %s", ff, v, v.Warning).fix(fix)}
	} else {
		return []check{mk(lvOK, "ffmpeg", "%s %s", ff, v)}
	}
}

func setupMediamtx(_ context.Context, r *drun) []check {
	cfg := r.e.cfg
	switch {
	case cfg.RelayExternal:
		return []check{mk(lvOK, "mediamtx", "%s", relayExternalText(cfg))}
	case cfg.RestreamerEnabled:
		return []check{r.d.mediamtx(cfg.RestreamerExePath)}
	}
	return []check{mk(lvOK, "mediamtx", "the relay is off (RestreamerEnabled false)")}
}

func mediamtx(exe string) check {
	path := exe
	if path == "" {
		path = "mediamtx"
	}
	const fix = "install MediaMTX and point the configuration at it: holocron config set RestreamerExePath <path to mediamtx>"
	resolved, err := exec.LookPath(path)
	if err != nil {
		return mk(lvFail, "mediamtx", "%s: %v", path, err).fix(fix)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, resolved, "--version").CombinedOutput()
	v := strings.TrimSpace(firstLine(string(out)))
	if err != nil || v == "" {
		return mk(lvWarn, "mediamtx", "%s found, but --version said %q (%v)", resolved, v, err).fix(fix)
	}
	return mk(lvOK, "mediamtx", "%s %s", resolved, v)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func setupPorts(_ context.Context, r *drun) []check {
	cfg := r.e.cfg
	var cs []check
	for _, a := range []struct{ what, url string }{{"relay RTSP", cfg.RelayBase}, {"relay API", cfg.RelayApiBase}} {
		if !cfg.RestreamerEnabled || a.url == "" {
			continue
		}
		hp, err := config.HostPort(a.url)
		if err != nil {
			cs = append(cs, mk(lvFail, a.what, "%v", err).fix("correct the address in %s", r.e.paths.Config))
			continue
		}
		cs = append(cs, portCheck(r.d.listen, a.what, "tcp", hp, r.running))
	}
	if cfg.LiveViewEnabled {
		cs = append(cs, portCheck(r.d.listen, "live WHEP", "tcp", cfg.LiveWhepAddress, r.running))
		cs = append(cs, portCheck(r.d.listen, "live media", "udp", cfg.LiveUdpAddress, r.running))
	}
	return cs
}

// portFinder is the OS command that names the program holding a port.
func portFinder(port string) string {
	switch goos() {
	case "windows":
		return "netstat -ano | findstr :" + port
	case "darwin":
		return "lsof -nP -i :" + port
	}
	return "ss -ltnup | grep :" + port
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return port
}

// bindFree binds the address for a moment (doctor's listen seam).
func bindFree(network, addr string) error {
	if network == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err == nil {
			c.Close()
		}
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err == nil {
		l.Close()
	}
	return err
}

// portCheck binds the address for a moment; in use is fine while our own supervisor runs.
func portCheck(listen func(network, addr string) error, what, network, addr string, running bool) check {
	err := listen(network, addr)
	switch {
	case err == nil:
		return mk(lvOK, what, "%s %s is free", network, addr)
	case running:
		return mk(lvOK, what, "%s %s is in use (the running supervisor's)", network, addr)
	}
	return mk(lvFail, what, "%s %s cannot be used: %v", network, addr, err).fix("find the program holding it (%s) and stop it, or change the address in the configuration", portFinder(portOf(addr)))
}

// webPortChecks warns (never fails) when web.listen or web.tlsListen is held by something that is
// not this Holocron's web role: up would start a web role that cannot bind and crash-loops. Skipped
// when the web app is off; a live web role of this root owns its ports.
func webPortChecks(e *env, listen func(network, addr string) error) []check {
	if !e.cfg.Web.Enabled || webStatus(e).Running {
		return nil
	}
	var cs []check
	for _, a := range []struct{ what, addr string }{{"web listen", e.cfg.Web.Listen}, {"web tlsListen", e.cfg.Web.TLSListen}} {
		if a.addr == "" {
			continue
		}
		err := listen("tcp", a.addr)
		if err == nil {
			cs = append(cs, mk(lvOK, a.what, "tcp %s is free", a.addr))
			continue
		}
		port := portOf(a.addr)
		cs = append(cs, mk(lvWarn, a.what, "port %s (%s) is held by another program (for example the PowerShell system's camui.exe): holocron up would crash-loop its web role (alert 104) (%v)", port, a.addr, err).
			fix("find the program (%s) and stop it by its pid, or move %s to a free address in the configuration", portFinder(port), strings.Replace(a.what, " ", ".", 1)))
	}
	return cs
}

func setupFolders(_ context.Context, r *drun) []check {
	p := r.e.paths
	var cs []check
	for _, d := range []string{p.Root, p.State(), p.Logs(), filepath.Dir(p.Config)} {
		cs = append(cs, writable(d))
	}
	return cs
}

func writable(dir string) check {
	fix := "give this user write access to " + dir + ", or choose another root (--root or RootPath)"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return mk(lvFail, "folder", "%s: %v", dir, err).fix("%s", fix)
	}
	f, err := os.CreateTemp(dir, ".holocron-doctor-*")
	if err != nil {
		return mk(lvFail, "folder", "%s is not writable: %v", dir, err).fix("%s", fix)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return mk(lvOK, "folder", "%s is writable", dir)
}

func setupRoot(_ context.Context, r *drun) []check {
	e := r.e
	if p := archive.CheckRoot(e.paths.Root, e.opts.Platform); p != "" {
		return []check{mk(lvFail, "archive root", "%s", p).fix("choose a folder on a local fixed disk: holocron config set RootPath <folder>")}
	} else if ok, why := archive.MarkerValid(e.paths.Root, e.opts.Platform); !ok {
		return []check{mk(lvWarn, "archive root", "%s is usable; not claimed yet (%s) - the supervisor claims it at start", e.paths.Root, why).fix("holocron up --detach (the supervisor claims the root)")}
	}
	return []check{mk(lvOK, "archive root", "%s, claimed", e.paths.Root)}
}

func setupClock(_ context.Context, r *drun) []check {
	now := r.now
	_, offset := now.Zone()
	if now.Year() < 2025 {
		return []check{mk(lvFail, "clock", "the clock says %s: segment names and the pruner's age rule would be wrong", now.Format(time.RFC3339)).fix("set the system clock and its time zone, then run doctor again")}
	}
	return []check{mk(lvOK, "clock", "%s (UTC offset %+d min, zone %s)", now.Format(time.RFC3339), offset/60, time.Local)}
}

func setupCredentials(_ context.Context, r *drun) []check {
	cfg := r.e.cfg
	const fix = "holocron secret set camera.user ; holocron secret set camera.password (each reads the value from standard input)"
	if c, err := app.CredentialsFor(cfg, r.e.opts.Getenv); err != nil {
		return []check{mk(lvFail, "credentials", "%v", err).fix(fix)}
	} else if _, _, err := c.CameraLogin(); err != nil {
		return []check{mk(lvFail, "credentials", "%v", err).fix(fix)}
	}
	return []check{mk(lvOK, "credentials", "a camera login is present (backend %s; values not shown)", cfg.Secrets.Backend)}
}

func setupStatus(_ context.Context, r *drun) []check {
	doc, now := r.doc, r.now
	switch {
	case r.running:
		cs := []check{mk(lvOK, "status", "running, supervisor pid %d", doc.SupervisorPid)}
		if doc.UpdatedAt.Time().After(now.Add(time.Minute)) {
			cs = append(cs, mk(lvWarn, "clock", "status.json was written %s in the future", doc.UpdatedAt.Time().Sub(now).Round(time.Second)).fix("set the system clock; the supervisor's heartbeat is ahead of this clock"))
		}
		return cs
	case doc == nil:
		return []check{mk(lvWarn, "status", "%s", r.why).fix("holocron up --detach")}
	}
	return []check{mk(lvWarn, "status", "not running: %s (last state %s %s)", r.why, doc.State, doc.StopKind).fix("holocron up --detach")}
}
