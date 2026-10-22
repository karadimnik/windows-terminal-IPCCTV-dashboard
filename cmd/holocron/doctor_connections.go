package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/relay"
)

var connectionsTable = []doctorCheck{
	{id: "connections.camera", group: "connections", concurrent: true, timeout: 10 * time.Second, run: connCameras},
	{id: "connections.relay", group: "connections", concurrent: true, timeout: 6 * time.Second, run: connRelay},
	{id: "connections.port", group: "connections", concurrent: true, run: connPorts},
}

func init() { doctorTable = append(doctorTable, connectionsTable...) }

// probeTimeout bounds one network probe (a camera, a port, an HTTP request) inside a check.
const probeTimeout = 2500 * time.Millisecond

// connCameras: each enabled camera answers TCP and RTSP OPTIONS (cam test's code, no login).
// Unreachable is a warning while its recording still grows (the relay keeps its session), a
// failure when it does not.
func connCameras(ctx context.Context, r *drun) []check {
	cams, _ := r.enabled()
	out := make([]check, len(cams))
	var wg sync.WaitGroup
	for i, cam := range cams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var answer string
			var err error
			if perr := r.probe(ctx, func() { answer, err = probeRTSPWith(ctx, r.d.dial, cam, probeTimeout) }); perr != nil {
				err = perr
			}
			data := map[string]any{"camera": cam.Name, "address": rtspTarget(cam), "reachable": err == nil}
			if err == nil {
				out[i] = mk(lvOK, cam.Name, "reachable: %s answered %q", rtspTarget(cam), answer).data(data)
				return
			}
			safe, _ := archive.SafeName(cam.Name)
			hint := fmt.Sprintf(`check the camera's power and network, and its address in cameras.json (holocron cam show "%s")`, cam.Name)
			if r.growth(ctx)[cam.Name].grew() {
				out[i] = mk(lvWarn, cam.Name, "unreachable (%v), but its recording still grows (the relay holds its session)", err).fix(`holocron cam test "%s" ; %s`, cam.Name, hint).data(data)
				return
			}
			out[i] = mk(lvFail, cam.Name, "unreachable (%v) and not recording", err).fix("%s ; %s", camFix(cam, safe), hint).data(data)
		}()
	}
	wg.Wait()
	return out
}

var secretHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// readSecretFile reads a relay login secret without touching the file (relay.LoadAPICredential
// would repair its permissions; doctor changes nothing).
func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if !secretHex.MatchString(s) {
		return "", fmt.Errorf("%s does not hold 64 hex characters", path)
	}
	return s, nil
}

// connRelay: the relay's control API answers and serves every enabled camera's path.
func connRelay(ctx context.Context, r *drun) []check {
	cfg := r.e.cfg
	if !cfg.RestreamerEnabled && !cfg.RelayExternal {
		return []check{mk(lvInfo, "relay", "the relay is off (RestreamerEnabled false): the cameras are read directly")}
	}
	cams, _ := r.enabled()
	paths, err := relay.RelayPaths(cams)
	if err != nil {
		return []check{mk(lvFail, "relay", "%v", err).fix("correct the camera names in %s", r.e.paths.Cameras)}
	}
	var login relay.Login
	which := "external relay at " + cfg.RelayApiBase
	if cfg.RestreamerEnabled {
		which = "relay API " + cfg.RelayApiBase
		credPath := relay.DefaultAPICredentialPath(r.d.platform)
		secret, err := readSecretFile(credPath)
		if err != nil {
			return []check{mk(lvFail, "relay", "the relay API login cannot be read: %v", err).fix("holocron restart --detach (the supervisor creates %s at start)", credPath)}
		}
		login = relay.Login{User: relay.APIUser, Secret: secret}
	}
	timeout := time.Duration(cfg.RelayProbeTimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > probeTimeout {
		timeout = probeTimeout
	}
	api := &relay.API{Base: cfg.RelayApiBase, Login: login, Timeout: timeout, HTTP: r.d.http}
	var set relay.ReadySet
	if perr := r.probe(ctx, func() { set, err = api.Ready(ctx) }); perr != nil {
		err = perr
	}
	logs := "holocron logs --component relay --since 10m"
	if !cfg.RestreamerEnabled {
		logs = "check the relay this system does not run (RelayExternal)"
	}
	switch {
	case err != nil && cfg.RestreamerEnabled:
		return []check{mk(lvFail, "relay", "%s does not answer: %v", which, err).fix("%s ; holocron restart --detach", logs)}
	case err != nil:
		return []check{mk(lvWarn, "relay", "%s does not answer: %v", which, err).fix("check the relay this system does not run (RelayExternal), or set RelayApiBase")}
	case set.State == relay.Denied && cfg.RestreamerEnabled:
		return []check{mk(lvFail, "relay", "%s refused this system's login (alert 130): which paths it serves cannot be seen", which).fix("holocron restart --detach (the relay reloads the login)")}
	case set.State == relay.Denied:
		return []check{mk(lvInfo, "relay", "%s answers, but wants its own login: its paths cannot be listed", which)}
	}
	cs := []check{mk(lvOK, "relay", "%s answers, %d path(s) serving", which, len(set.Names()))}
	for _, p := range paths {
		data := map[string]any{"camera": p.Camera.Name, "path": p.Name, "ready": set.Has(p.Name), "readers": set.Readers(p.Name)}
		if !set.Has(p.Name) {
			cs = append(cs, mk(lvFail, p.Camera.Name, "relay path %s is not ready: the relay is not reading the camera", p.Name).
				fix(`holocron cam test "%s" ; %s`, p.Camera.Name, logs).data(data))
			continue
		}
		cs = append(cs, mk(lvOK, p.Camera.Name, "relay path %s ready, %d reader(s)", p.Name, set.Readers(p.Name)).data(data))
	}
	return cs
}

// dialable turns a listen address into one to dial (an unspecified host is loopback).
func dialable(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// connPorts: the listeners this system should have are there (the relay, live and web checks
// prove whose they are by their answers).
func connPorts(ctx context.Context, r *drun) []check {
	cfg := r.e.cfg
	type want struct{ what, addr, comp string }
	var ws []want
	if cfg.RestreamerEnabled || cfg.RelayExternal {
		if hp, err := config.HostPort(cfg.RelayBase); err == nil {
			ws = append(ws, want{"relay RTSP", hp, "relay"})
		}
	}
	if cfg.RestreamerEnabled {
		if hp, err := config.HostPort(cfg.RelayApiBase); err == nil {
			ws = append(ws, want{"relay API", hp, "relay"})
		}
	}
	if cfg.LiveViewEnabled {
		ws = append(ws, want{"live WHEP", cfg.LiveWhepAddress, "live"})
	}
	if cfg.Web.Enabled {
		ws = append(ws, want{"web listen", cfg.Web.Listen, "web"})
		if cfg.Web.TLSListen != "" {
			ws = append(ws, want{"web tlsListen", cfg.Web.TLSListen, "web"})
		}
	}
	out := make([]check, len(ws))
	var wg sync.WaitGroup
	for i, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr := dialable(w.addr)
			var err error
			if perr := r.probe(ctx, func() {
				c, cerr := r.dialTimeout(ctx, addr)
				if err = cerr; cerr == nil {
					c.Close()
				}
			}); perr != nil {
				err = perr
			}
			if err != nil {
				out[i] = mk(lvFail, w.what, "nothing answers on tcp %s: %v", addr, err).fix("holocron logs --component %s --since 10m ; holocron restart --detach", w.comp)
				return
			}
			out[i] = mk(lvOK, w.what, "tcp %s answers", addr)
		}()
	}
	wg.Wait()
	return out
}

func (r *drun) dialTimeout(ctx context.Context, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return r.d.dial(ctx, "tcp", addr)
}
