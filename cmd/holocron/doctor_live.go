package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"holocron/internal/relay"
	"holocron/internal/web"
)

var liveTable = []doctorCheck{
	{id: "live.process", group: "live", run: liveProcess},
	{id: "live.login", group: "live", concurrent: true, timeout: 8 * time.Second, run: liveLogin},
	{id: "live.paths", group: "live", run: livePaths},
}

func init() { doctorTable = append(doctorTable, liveTable...) }

// liveOff is the one line of the live group when live view is off (the first check says it).
func liveOff(r *drun) []check {
	if r.e.cfg.LiveViewEnabled {
		return nil
	}
	return []check{mk(lvInfo, "live view", "off (LiveViewEnabled false): nothing to check")}
}

func liveProcess(_ context.Context, r *drun) []check {
	if off := liveOff(r); off != nil {
		return off
	}
	l := r.doc.Live
	const fix = "holocron live status ; holocron logs --component live --since 10m"
	switch {
	case l == nil || !l.Enabled:
		return []check{mk(lvFail, "live instance", "LiveViewEnabled is true, but the supervisor runs no live instance").fix("holocron live enable (applied at once while running)")}
	case !l.Alive:
		return []check{mk(lvFail, "live instance", "not alive%s (%d restart(s))", errText(l.LastError), l.RestartsTotal).fix(fix)}
	}
	return []check{mk(lvOK, "live instance", "alive%s, %d restart(s), WHEP on %s (the live instance has no control API by design: the login probe below talks to its WHEP server)", pidText(l.Pid), l.RestartsTotal, r.e.cfg.LiveWhepAddress)}
}

// liveSecretFiles are the two resolutions of the live secret, both through config.LiveSecretFile:
// the web role's (its WHEP proxy logs in with it) and the supervisor's (the live instance's login
// is made from it).
func (r *drun) liveSecretFiles() (webFile, supFile string) {
	wc := webBase(r.e)
	wc.LiveSecretDir = r.d.platform.Dirs().Auth
	return web.LiveSecretFileOf(wc), relay.LiveSecretPath(r.d.platform, r.e.cfg)
}

func liveLogin(ctx context.Context, r *drun) []check {
	if !r.e.cfg.LiveViewEnabled {
		return nil
	}
	webFile, supFile := r.liveSecretFiles()
	return liveLoginProbe(ctx, r, webFile, supFile)
}

// livePathName is one live path the probe may name (the first enabled camera's main path).
func (r *drun) livePathName() string {
	cams, _ := r.enabled()
	paths, err := relay.LivePaths(cams, r.e.cfg)
	if err != nil {
		return ""
	}
	for _, p := range paths {
		if p.Kind == relay.LiveMain {
			return p.Name
		}
	}
	return ""
}

// liveLoginProbe logs in to the live instance's WHEP server with the web role's secret, as the
// web role's proxy does (Basic camlive:<secret> on /<path>/whep), but with OPTIONS: MediaMTX checks
// the login before it answers and no session or source is started. A wrong login is answered only
// after a pause (about 1 s on v1.21.2), so a request that runs out of time is asked again without
// a login: an immediate 401 proves the instance is up and refused the login.
func liveLoginProbe(ctx context.Context, r *drun, webFile, supFile string) []check {
	title := "live login"
	data := map[string]any{"webSecretFile": webFile, "supervisorSecretFile": supFile, "sameFile": sameFile(webFile, supFile)}
	secret, err := readSecretFile(webFile)
	if err != nil {
		return []check{mk(lvFail, title, "the web role's live secret cannot be used: %v (the supervisor's is %s)", err, supFile).
			fix("set LiveSecretPath to %s in %s, then restart the web role%s", supFile, r.e.paths.Config, webRestartHint(r)).data(data)}
	}
	path := r.livePathName()
	if path == "" {
		return []check{mk(lvWarn, title, "no camera to name a live path for the probe").fix("holocron cam enable \"<camera>\"").data(data)}
	}
	u := "http://" + r.e.cfg.LiveWhepAddress + "/" + url.PathEscape(path) + "/whep"
	code, err := r.liveOptions(ctx, u, secret, 4*time.Second)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		if anon, aerr := r.liveOptions(ctx, u, "", time.Second); aerr == nil && (anon == http.StatusUnauthorized || anon == http.StatusForbidden) {
			code, err = anon, nil
		}
	}
	data["status"] = code
	switch {
	case err != nil:
		return []check{mk(lvFail, title, "the live instance does not answer on %s: %v", r.e.cfg.LiveWhepAddress, err).fix("holocron live status ; holocron logs --component live --since 10m").data(data)}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		if sameFile(webFile, supFile) {
			return []check{mk(lvFail, title, "live login refused, although the web role and the supervisor both use %s: the live instance runs with an older secret", supFile).
				fix("holocron restart --detach (the supervisor rewrites the live configuration from the file at start)").data(data)}
		}
		return []check{mk(lvFail, title, "live login refused (the web role and the supervisor use different secrets): the web role reads %s, the supervisor %s", webFile, supFile).
			fix("set LiveSecretPath to the same file in holocron.config.json (%s: \"LiveSecretPath\": %q), then restart the web role%s", r.e.paths.Config, supFile, webRestartHint(r)).data(data)}
	case code >= 200 && code < 300:
		note := "the same file"
		if !sameFile(webFile, supFile) {
			note = fmt.Sprintf("different files with the same secret: web %s, supervisor %s", webFile, supFile)
		}
		return []check{mk(lvOK, title, "the live instance accepts the web role's login on %s (%s)", path, note).data(data)}
	}
	return []check{mk(lvWarn, title, "the live instance answered HTTP %d for path %s", code, path).fix("holocron live status ; holocron logs --component live --since 10m").data(data)}
}

func sameFile(a, b string) bool {
	if goos() == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// webRestartHint is how to restart the web role alone: stop it by pid and the host starts it again.
func webRestartHint(r *drun) string {
	w := webStatus(r.e)
	if !w.Running {
		return " (holocron restart --detach)"
	}
	if goos() == "windows" {
		return fmt.Sprintf(" (taskkill /PID %d /F: the host starts it again in 5 s, recording goes on)", w.Pid)
	}
	return fmt.Sprintf(" (kill %d: the host starts it again in 5 s, recording goes on)", w.Pid)
}

func (r *drun) liveOptions(ctx context.Context, u, secret string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, u, nil)
	if err != nil {
		return 0, err
	}
	if secret != "" {
		req.SetBasicAuth(relay.LiveUser, secret)
	}
	var resp *http.Response
	if perr := r.probe(ctx, func() { resp, err = r.d.http.Do(req) }); perr != nil {
		return 0, perr
	}
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// livePaths: every enabled camera has its live paths in the supervisor's live view rows (main from
// the relay, sub from the camera when it differs).
func livePaths(_ context.Context, r *drun) []check {
	if !r.e.cfg.LiveViewEnabled {
		return nil
	}
	cams, _ := r.enabled()
	want, err := relay.LivePaths(cams, r.e.cfg)
	if err != nil {
		return []check{mk(lvFail, "live paths", "%v", err).fix("rename a camera in %s", r.e.paths.Cameras)}
	}
	var rows map[string]bool
	if l := r.doc.Live; l != nil {
		rows = map[string]bool{}
		for _, c := range l.Cameras {
			if c.Main {
				rows[c.MainPath] = true
			}
			if c.Sub {
				rows[c.SubPath] = true
			}
		}
	}
	var cs []check
	for _, cam := range cams {
		var have, missing []string
		for _, p := range want {
			if p.Camera.Name != cam.Name {
				continue
			}
			if rows[p.Name] {
				have = append(have, p.Name)
			} else {
				missing = append(missing, p.Name)
			}
		}
		if len(missing) > 0 {
			cs = append(cs, mk(lvFail, cam.Name, "live path(s) %s missing from the live instance (has %s)", strings.Join(missing, ", "), orDash(strings.Join(have, ", "))).
				fix("holocron live status ; holocron logs --component live --since 10m"))
			continue
		}
		cs = append(cs, mk(lvOK, cam.Name, "live path(s) %s present (started on demand when watched)", strings.Join(have, ", ")))
	}
	return cs
}
