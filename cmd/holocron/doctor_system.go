package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"holocron/internal/app"
	"holocron/internal/osx"
)

var systemTable = []doctorCheck{
	{id: "system.processes", group: "system", timeout: 10 * time.Second, run: sysProcesses},
	{id: "system.autostart", group: "system", timeout: 10 * time.Second, run: sysAutostart},
	{id: "system.log", group: "system", run: sysLog},
	{id: "system.alerts", group: "system", run: sysAlerts},
	{id: "system.version", group: "system", run: sysVersion},
	{id: "system.env", group: "system", run: sysEnv},
}

func init() { doctorTable = append(doctorTable, systemTable...) }

// aliveEntries are the entries of a registry file that are alive by identity.
func (r *drun) aliveEntries(path string) []osx.ChildEntry {
	entries, err := osx.NewRegistry(path).Load()
	if err != nil {
		return nil
	}
	var out []osx.ChildEntry
	for _, c := range entries {
		if alive, _ := osx.Verify(c, r.e.inspector, goos()); alive {
			out = append(out, c)
		}
	}
	return out
}

// sysProcesses is the census `status` and `down` take, read only (no lock is probed): the host by
// its record, the supervisor by its heartbeat, the children by the registries, all by pid + start
// time + image; then the OS process list for an ffmpeg or MediaMTX of this system's executables
// that no registry lists. Nothing is ever stopped.
func sysProcesses(_ context.Context, r *drun) []check {
	p, in := r.e.paths, r.e.inspector
	var host *osx.ChildEntry
	if h, ok := app.ReadHostRecord(p); ok {
		if alive, _ := osx.Verify(h, in, goos()); alive {
			host = &h
		}
	}
	children, aux := r.aliveEntries(osx.RegistryPath(p.State())), r.aliveEntries(p.AuxFile())
	known := map[int]bool{os.Getpid(): true}
	if host != nil {
		known[host.Pid] = true
	}
	if r.running {
		known[int(r.doc.SupervisorPid)] = true
	}
	for _, c := range append(append([]osx.ChildEntry{}, children...), aux...) {
		known[c.Pid] = true
	}
	var cs []check
	const down = "holocron down (it stops what it started and reaps only provable orphans; never by name)"
	switch owner, _ := osx.ReadLockOwner(p.LockFile()); {
	case r.running && host == nil:
		cs = append(cs, mk(lvFail, "processes", "supervisor pid %d writes status.json, but no holocron host runs: it was not started by holocron up (the PowerShell system's supervisor?), and holocron up would only print \"already running\"", r.doc.SupervisorPid).
			fix(`stop that system: .\cam.ps1 -Stop in the PowerShell system's folder (another Holocron: holocron --config <its config> down); then holocron up --detach`))
	case r.running && owner.Pid > 0 && owner.Pid != int(r.doc.SupervisorPid) && pidAlive(in, owner.Pid):
		cs = append(cs, mk(lvFail, "processes", "two supervisors: pid %d holds the instance lock, pid %d writes status.json", owner.Pid, r.doc.SupervisorPid).fix("%s ; then holocron up --detach", down))
	case !r.running && host != nil:
		cs = append(cs, mk(lvWarn, "processes", "host pid %d runs without a supervisor (it restarts one after 5 s, and gives up after 5 restarts in 30 minutes)", host.Pid).fix("holocron status ; holocron logs --component host --since 10m"))
	}
	if !r.running {
		for _, c := range children {
			cs = append(cs, mk(lvFail, "orphan", "%s pid %d (%s) is alive with no supervisor", orDash(c.Role), c.Pid, c.Image).fix("%s", down))
		}
	}
	if host == nil {
		for _, c := range aux {
			cs = append(cs, mk(lvFail, "orphan", "%s pid %d (%s) is alive with no host", orDash(c.Role), c.Pid, c.Image).fix("%s", down))
		}
	}
	cs = append(cs, r.strays(known)...)
	if len(cs) == 0 {
		hostText := "no host"
		if host != nil {
			hostText = fmt.Sprintf("host pid %d", host.Pid)
		}
		supText := "no supervisor"
		if r.running {
			supText = fmt.Sprintf("supervisor pid %d", r.doc.SupervisorPid)
		}
		cs = append(cs, mk(lvOK, "processes", "%s, %s, %d child(ren) by identity; no stray ffmpeg or MediaMTX", hostText, supText, len(children)+len(aux)))
	}
	return cs
}

func pidAlive(in osx.Inspector, pid int) bool {
	_, err := in.Inspect(pid)
	return err == nil
}

// strays: processes of this system's ffmpeg and MediaMTX executables that no registry lists and
// whose parent is none of this system's processes (the web role's own ffmpeg is a child of it).
func (r *drun) strays(known map[int]bool) []check {
	cfg := r.e.cfg
	exes := map[string]string{} // resolved image -> kind
	add := func(kind, path, def string) {
		if path == "" {
			path = def
		}
		if res, err := r.d.lookPath(path); err == nil {
			exes[res] = kind
		}
	}
	add("ffmpeg", cfg.FfmpegPath, "ffmpeg")
	if cfg.RestreamerEnabled {
		add("MediaMTX", cfg.RestreamerExePath, "mediamtx")
	}
	if cfg.LiveViewEnabled {
		add("MediaMTX", cfg.LiveExePath, cfg.RestreamerExePath)
	}
	procs, err := r.d.listProcs()
	if err != nil {
		return []check{mk(lvInfo, "strays", "the process list is not read on this OS (%v): stray processes not checked", err)}
	}
	names := map[string]bool{}
	for img := range exes {
		names[strings.ToLower(filepath.Base(img))] = true
	}
	level := lvFail
	if cfg.RelayExternal { // the shadow rig: the other system's recorders run beside this one on purpose
		level = lvInfo
	}
	var cs []check
	for _, pr := range procs {
		if known[pr.Pid] || known[pr.PPid] || !names[strings.ToLower(pr.Name)] {
			continue
		}
		info, err := r.e.inspector.Inspect(pr.Pid)
		if err != nil {
			continue // gone, or another user's: not ours to judge
		}
		for img, kind := range exes {
			if !sameFile(info.Image, img) {
				continue
			}
			finder := fmt.Sprintf("ps -o pid,ppid,args -p %d", pr.Pid)
			if goos() == "windows" {
				finder = fmt.Sprintf(`tasklist /FI "PID eq %d" /V`, pr.Pid)
			}
			cs = append(cs, mk(level, "stray", "%s pid %d (parent %d, %s) belongs to no registry of this system", kind, pr.Pid, pr.PPid, info.Image).
				fix("find what started it (%s) and stop that program (the PowerShell system: .\\cam.ps1 -Stop); doctor and holocron down never stop a process they cannot prove is theirs", finder))
		}
	}
	return cs
}

// sysAutostart: the entry exists, is on and starts this executable with this configuration (a
// moved binary or another config file is the classic breakage).
func sysAutostart(_ context.Context, r *drun) []check {
	st := r.e.autostartState()
	self, _ := r.e.self()
	cfgPath := r.e.paths.Config
	runsCfg := strings.Contains(st.Command, cfgPath) || (!strings.Contains(st.Command, "--config") && sameFile(app.DefaultConfigPath(r.e.opts.Platform), cfgPath))
	switch {
	case !st.Installed:
		return []check{mk(lvWarn, "autostart", "not installed: nothing starts Holocron at logon or boot (%s %s)", st.Mode, st.Name).fix("holocron --config %s install", cfgPath)}
	case !st.Enabled:
		return []check{mk(lvWarn, "autostart", "%s is DISABLED", st.Name).fix("holocron autostart enable")}
	case !st.ExeExists:
		return []check{mk(lvWarn, "autostart", "%s runs %s, which does not exist (a moved or deleted binary never starts)", st.Name, st.Exe).fix("holocron --config %s install (from %s)", cfgPath, self)}
	case !sameFile(st.Exe, self):
		return []check{mk(lvWarn, "autostart", "%s runs %s, but this holocron is %s", st.Name, st.Exe, self).fix("holocron --config %s install (from %s)", cfgPath, self)}
	case !runsCfg:
		return []check{mk(lvWarn, "autostart", "%s runs %q, not with %s", st.Name, st.Command, cfgPath).fix("holocron --config %s install", cfgPath)}
	}
	return []check{mk(lvOK, "autostart", "%s, enabled, runs this executable with this configuration", st.Name)}
}

var attrRE = regexp.MustCompile(`\s[a-z_]+=`)

// sysLog: ERROR and WARN records of the last 24 h, the 3 most frequent messages.
func sysLog(_ context.Context, r *drun) []check {
	since := r.now.Add(-24 * time.Hour)
	counts := map[string]int{}
	var errs, warns int
	for _, l := range r.logLines() {
		if l.Time.Before(since) || (l.Level != "ERROR" && l.Level != "WARN") {
			continue
		}
		if l.Level == "ERROR" {
			errs++
		} else {
			warns++
		}
		msg := l.Text
		if loc := attrRE.FindStringIndex(msg); loc != nil {
			msg = msg[:loc[0]]
		}
		if len(msg) > 100 {
			msg = msg[:100] + "..."
		}
		counts["["+l.Component+"] "+msg]++
	}
	keys := sortedCounts(counts)
	var top []string
	for i, k := range keys {
		if i == 3 {
			break
		}
		top = append(top, fmt.Sprintf("%dx %s", counts[k], k))
	}
	text := fmt.Sprintf("%d ERROR and %d WARN record(s) in the last 24 h", errs, warns)
	if len(top) > 0 {
		text += "; most frequent: " + strings.Join(top, " | ")
	}
	return []check{mk(lvInfo, "log", "%s", text).data(map[string]any{"errors": errs, "warnings": warns, "top": top})}
}

// sortedCounts orders a count map by count (then name), largest first.
func sortedCounts(m map[string]int) []string {
	keys := sortedKeys(m)
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && m[keys[j]] > m[keys[j-1]]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// sysAlerts: the channels as the configuration has them; nothing is sent.
func sysAlerts(_ context.Context, r *drun) []check {
	return []check{mk(lvOK, "alerts", "%s (as configured; nothing sent here - holocron alert test sends a test)", channelText(alertChannels(r.e.cfg.Alerts)))}
}

func sysVersion(_ context.Context, r *drun) []check {
	return []check{mk(lvInfo, "version", "holocron %s, %s/%s, %s", version, runtime.GOOS, runtime.GOARCH, runtime.Version())}
}

// envFix removes variables from this window's environment.
func envFix(names ...string) string {
	if goos() == "windows" {
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = "Env:" + n
		}
		return "Remove-Item " + strings.Join(parts, ", ") + " (PowerShell; cmd: set " + names[0] + "=)"
	}
	return "unset " + strings.Join(names, " ")
}

// sysEnv: variables in this window that change what holocron does here (values never shown).
func sysEnv(_ context.Context, r *drun) []check {
	get := r.e.opts.Getenv
	if r.inWeb {
		return sysWebEnv(get)
	}
	var cs []check
	if get(app.SandboxEnv) != "" {
		cs = append(cs, mk(lvWarn, "environment", "%s is set in this window: every holocron started here refuses a root that could be the Operator's (tests only)", app.SandboxEnv).fix("%s", envFix(app.SandboxEnv)))
	}
	var login []string
	for _, k := range []string{app.EnvCameraUser, app.EnvCameraPassword} {
		if get(k) != "" {
			login = append(login, k)
		}
	}
	if len(login) > 0 {
		cs = append(cs, mk(lvWarn, "environment", "%s set in this window: they override the stored camera login for every holocron started here (values not shown)", strings.Join(login, " and ")).fix("%s", envFix(login...)))
	}
	if len(cs) == 0 {
		cs = append(cs, mk(lvOK, "environment", "no %s, %s or %s in this window", app.SandboxEnv, app.EnvCameraUser, app.EnvCameraPassword))
	}
	return cs
}

// sysWebEnv is the environment line inside the web role: the same variables, but they are the web
// process's, not the Operator's shell's, so it is context (info) and never a warning.
func sysWebEnv(get func(string) string) []check {
	var set []string
	for _, k := range []string{app.SandboxEnv, app.EnvCameraUser, app.EnvCameraPassword} {
		if get(k) != "" {
			set = append(set, k)
		}
	}
	if len(set) == 0 {
		return []check{mk(lvInfo, "environment", "environment of the web role: no %s, %s or %s", app.SandboxEnv, app.EnvCameraUser, app.EnvCameraPassword)}
	}
	return []check{mk(lvInfo, "environment", "environment of the web role: %s set (values not shown); run holocron doctor in your own window to check that one", strings.Join(set, " and "))}
}

// sandboxRefused is doctor's whole report when the sandbox guard refused the root: one failure
// with the variable's fix, and nothing of the root touched.
func sandboxRefused(err error) check {
	c := mk(lvFail, "sandbox guard", "%v: %s is set in this window", err, app.SandboxEnv).fix("%s (it is for tests; then run holocron doctor again)", envFix(app.SandboxEnv))
	c.ID, c.Group = "setup.sandbox", "setup"
	return c
}
