package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"holocron/internal/app"
	"holocron/internal/autostart"
	"holocron/internal/cli"
	"holocron/internal/logx"
	"holocron/internal/osx"
)

// EnvInstallName overrides the entry name; like the hidden --name flag it accepts only names with
// autostart.TestInfix, so a test can never create, switch or remove the real entry.
const EnvInstallName = "HOLOCRON_INSTALL_NAME"

const oldTasksFlag = "i-know-old-tasks-are-enabled"

// installFlags are the flags install, uninstall and autostart share. The hidden ones (--name,
// --test-disabled) are for tests and are not in the help.
type installFlags struct {
	fs           *flag.FlagSet
	dry          *bool
	name         *string
	oldOK        *bool
	testDisabled *bool
}

func newInstallFlags(e *env, cmd string) installFlags {
	fs := newFlags(e, cmd)
	return installFlags{fs: fs,
		dry:          fs.Bool("dry-run", false, "print every command and generated file, change nothing"),
		name:         fs.String("name", "", "(tests) an entry name with -TEST- in it"),
		oldOK:        fs.Bool(oldTasksFlag, false, "go on although a PowerShell system task is enabled"),
		testDisabled: fs.Bool("test-disabled", false, "(tests) create the entry switched off"),
	}
}

func (f installFlags) parse(e *env, cmd string) error {
	pos, err := parseFlags(f.fs, e.args)
	if err != nil {
		return err
	}
	return wantArgs(cmd, pos, 0, 0)
}

// usageOf turns an autostart usage fault into exit 2.
func usageOf(err error) error {
	if errors.Is(err, autostart.ErrUsage) {
		return usageErr("%s", strings.TrimPrefix(err.Error(), autostart.ErrUsage.Error()+": "))
	}
	return err
}

// entryName is the entry's name: the mode's own, or a test name (--name or HOLOCRON_INSTALL_NAME).
func (e *env) entryName(flagName string, m autostart.Mode) (string, error) {
	n := flagName
	if n == "" {
		n = e.opts.Getenv(EnvInstallName)
	}
	if n == "" {
		return autostart.DefaultName(m), nil
	}
	return n, usageOf(autostart.CheckName(n, m))
}

func (e *env) recordPath() string { return autostart.RecordPath(filepath.Dir(e.paths.Config)) }

func (e *env) ops(dry bool) *autostart.Ops {
	return &autostart.Ops{Env: e.auto, Out: e.out, Log: e.lg.For("install"), Dry: dry}
}

// oldTasksGate reads (never changes) the PowerShell system's tasks; an enabled one refuses the
// command unless the Operator says he knows.
func (e *env) oldTasksGate(override bool) error {
	if e.auto.GOOS != "windows" {
		return nil
	}
	olds := autostart.QueryOldTasks(e.auto)
	var on []string
	for _, o := range olds {
		e.lg.For("install").Debug(fmt.Sprintf("old task %s: found %v enabled %v %s", o.Name, o.Found, o.Enabled, o.Detail), logx.Action("install.oldtasks"), logx.Target(o.Name))
		if o.Enabled {
			on = append(on, o.Name)
		}
	}
	fmt.Fprintf(e.out, "PowerShell system tasks (read only, never changed): %s\n", oldTasksText(olds))
	if len(on) == 0 {
		return nil
	}
	fmt.Fprintf(e.out, "WARNING: %s %s ENABLED: the PowerShell system and holocron would both start at logon and fight for the ports and the cameras.\n",
		strings.Join(on, ", "), map[bool]string{true: "is", false: "are"}[len(on) == 1])
	fmt.Fprintln(e.out, "holocron never changes those tasks. To switch the old system's autostart off yourself (reversible with /ENABLE):")
	for _, n := range on {
		fmt.Fprintf(e.out, "  schtasks /Change /TN %s /DISABLE\n", n)
	}
	if !override {
		return cli.Refuse("the PowerShell system task(s) %s are enabled; disable them or add --%s", strings.Join(on, ", "), oldTasksFlag)
	}
	fmt.Fprintf(e.out, "going on because of --%s\n", oldTasksFlag)
	return nil
}

func oldTasksText(olds []autostart.OldTask) string {
	var parts []string
	for _, o := range olds {
		s := "not found"
		switch {
		case o.Found && o.Detail != "":
			s = "unreadable (" + o.Detail + ")"
		case o.Enabled:
			s = "ENABLED"
		case o.Found:
			s = "disabled"
		}
		parts = append(parts, o.Name+" "+s)
	}
	return strings.Join(parts, ", ")
}

func cmdInstall(ctx context.Context, e *env) error {
	f := newInstallFlags(e, "install")
	mode := f.fs.String("mode", "", "logon|service (Windows), systemd (Linux), launchd (macOS)")
	allowTemp := f.fs.Bool("allow-temp", false, "install an executable that is under a temporary folder")
	yes := f.fs.Bool("yes", false, "(service mode) do not ask for confirmation")
	account := f.fs.String("account", "", "(service mode) the service account (default LocalSystem)")
	window := f.fs.String("window", "", "(Windows logon task) visible: the attached up in a console window (default); hidden: no window, up --detach")
	if err := f.parse(e, "install"); err != nil {
		return err
	}
	m, err := autostart.ParseMode(*mode, e.auto.GOOS)
	if err != nil {
		return usageOf(err)
	}
	name, err := e.entryName(*f.name, m)
	if err != nil {
		return err
	}
	if *f.testDisabled && name == autostart.DefaultName(m) {
		return usageErr("holocron install --test-disabled is for test entries only (a name with %s)", autostart.TestInfix)
	}
	if *account != "" && m != autostart.Service {
		return usageErr("holocron install --account is for --mode service only")
	}
	if err := autostart.CheckWindow(*window, m); err != nil {
		return usageOf(err)
	}
	if m == autostart.Service {
		if err := autostart.CheckAccount(*account); err != nil {
			return usageOf(err)
		}
	}
	if err := e.needConfig(); err != nil {
		return err
	}
	exe, err := e.self()
	if err != nil {
		return failed("cannot find this executable: %v", err)
	}
	if a, err := filepath.Abs(exe); err == nil {
		exe = a
	}
	if why := autostart.TransientExe(exe, e.auto); why != "" {
		if !*allowTemp {
			return cli.Refuse("%s looks transient (%s): an autostart entry pointing at it would break when it is gone; copy holocron to its permanent folder and run install from there, or add --allow-temp", exe, why)
		}
		fmt.Fprintf(e.out, "note: %s is transient (%s); installing anyway because of --allow-temp\n", exe, why)
	}
	if err := e.oldTasksGate(*f.oldOK); err != nil {
		return err
	}
	spec := autostart.Spec{Mode: m, Name: name, Exe: exe, Config: e.paths.Config, Disabled: *f.testDisabled, Account: *account, Record: e.recordPath(), Window: *window}
	if m == autostart.Logon && spec.Window == "" {
		// without --window a reinstall keeps the recorded choice; a first install is visible
		spec.Window = autostart.WindowVisible
		if rec, ok, _ := autostart.ReadRecord(e.recordPath()); ok && rec.Mode == m && rec.Name == name && rec.Window != "" {
			spec.Window = rec.Window
		}
	}
	if e.opts.Root != "" {
		spec.Root = e.paths.Root
	}
	if m == autostart.Service {
		if err := e.serviceGate(spec, *f.dry, *yes); err != nil {
			return err
		}
	}
	if *f.dry {
		fmt.Fprintln(e.out, "dry run: nothing is changed")
	}
	res, err := autostart.Install(e.ops(*f.dry), spec, version)
	if err != nil {
		return failed("holocron install: %v", usageOf(err))
	}
	if *f.dry {
		return nil
	}
	if res.Changed {
		fmt.Fprintf(e.out, "installed and verified: %s\n", e.recorded(m, name).Summary())
	}
	fmt.Fprintln(e.out, "recorded in", e.recordPath())
	e.afterInstall(m, name, res.Entry)
	return nil
}

// afterInstall says when it starts and what was deliberately not done.
func (e *env) afterInstall(m autostart.Mode, name string, en autostart.Entry) {
	switch m {
	case autostart.Logon:
		fmt.Fprintln(e.out, "it starts holocron at your next logon (nothing was started now; holocron up --detach starts it now)")
		if en.Window == autostart.WindowHidden {
			fmt.Fprintln(e.out, "console window: hidden (conhost.exe --headless runs holocron up --detach; holocron down stops it)")
		} else {
			fmt.Fprintln(e.out, "console window: visible - at logon holocron runs in a console window on your desktop; closing that window stops everything in order")
			fmt.Fprintln(e.out, "  (a \"window closed\" stop with one alert 103, not an outage: nothing restarts it until the next logon or holocron up); --window hidden runs it without a window")
		}
	case autostart.Service:
		fmt.Fprintf(e.out, "the service starts at the next boot (delayed); nothing was started now (sc start %s starts it)\n", name)
	case autostart.Systemd:
		fmt.Fprintf(e.out, "it starts holocron when your systemd user manager starts (your login); nothing was started now (systemctl --user start %s.service starts it)\n", name)
		user := e.auto.User
		if autostart.Linger(e.auto, user) != "yes" {
			fmt.Fprintf(e.out, "to start it at boot without logging in, systemd needs lingering for %s; holocron does not switch it on by itself:\n  loginctl enable-linger %s\n", user, user)
		}
	case autostart.Launchd:
		fmt.Fprintf(e.out, "launchd loads it at your next login; nothing was started now (launchctl bootstrap gui/%s %s starts it)\n", e.auto.UID, en.File)
	}
}

func cmdUninstall(ctx context.Context, e *env) error {
	f := newInstallFlags(e, "uninstall")
	force := f.fs.Bool("force", false, "remove holocron's own entry although install did not record it")
	if err := f.parse(e, "uninstall"); err != nil {
		return err
	}
	rp := e.recordPath()
	rec, ok, err := autostart.ReadRecord(rp)
	if err != nil {
		return failed("the install record cannot be read: %v", err)
	}
	if ok {
		if n, err := e.entryName(*f.name, rec.Mode); err != nil {
			return err
		} else if (*f.name != "" || e.opts.Getenv(EnvInstallName) != "") && n != rec.Name {
			return cli.Refuse("the install record %s names %s %s, not %s", rp, rec.Mode, rec.Name, n)
		}
	} else {
		m, err := autostart.DefaultMode(e.auto.GOOS)
		if err != nil {
			return usageOf(err)
		}
		n, err := e.entryName(*f.name, m)
		if err != nil {
			return err
		}
		st := autostart.Status(e.auto, m, n)
		if !st.Installed {
			fmt.Fprintf(e.out, "nothing to remove: no install record at %s and no %s %s\n", rp, m, n)
			return nil
		}
		if !*force {
			fmt.Fprintf(e.out, "found %s\n", st.Summary())
			return cli.Refuse("%s %s exists but holocron install did not record it in %s; if it is holocron's and should go: holocron uninstall --force", m, n, rp)
		}
		fmt.Fprintf(e.out, "no install record; removing %s %s because of --force (it carries holocron's own name)\n", m, n)
		rec = autostart.Record{Mode: m, Name: n}
	}
	if *f.dry {
		fmt.Fprintln(e.out, "dry run: nothing is changed")
	}
	removed, err := autostart.Remove(e.ops(*f.dry), rec.Mode, rec.Name)
	if err != nil {
		return failed("holocron uninstall: %v (the install record is kept)", err)
	}
	if *f.dry {
		if ok {
			fmt.Fprintln(e.out, "would delete the install record", rp)
		}
		return nil
	}
	if err := autostart.RemoveRecord(rp); err != nil {
		return failed("the entry is gone but the install record %s could not be deleted: %v", rp, err)
	}
	if removed {
		fmt.Fprintf(e.out, "removed and verified gone: %s %s\n", rec.Mode, rec.Name)
	} else {
		fmt.Fprintf(e.out, "%s %s was already gone\n", rec.Mode, rec.Name)
	}
	if ok {
		fmt.Fprintln(e.out, "deleted the install record", rp)
	}
	fmt.Fprintln(e.out, "a holocron that is running now keeps running (holocron down stops it)")
	return nil
}

func cmdAutostartSet(ctx context.Context, e *env, on bool) error {
	verb := map[bool]string{true: "enable", false: "disable"}[on]
	f := newInstallFlags(e, "autostart "+verb)
	if err := f.parse(e, "autostart "+verb); err != nil {
		return err
	}
	return autostartSet(e, on, *f.dry, *f.oldOK, *f.name)
}

// autostartSet is `autostart enable|disable` once the flags are parsed (the Admin tab calls it
// with no dry run and no test name).
func autostartSet(e *env, on, dry, oldOK bool, nameFlag string) error {
	verb := map[bool]string{true: "enable", false: "disable"}[on]
	rec, ok, err := autostart.ReadRecord(e.recordPath())
	if err != nil {
		return failed("the install record cannot be read: %v", err)
	}
	if !ok {
		return cli.Refuse("nothing was installed by holocron install (no record at %s): run holocron install first", e.recordPath())
	}
	if n, err := e.entryName(nameFlag, rec.Mode); err != nil {
		return err
	} else if n != rec.Name && (nameFlag != "" || e.opts.Getenv(EnvInstallName) != "") {
		return cli.Refuse("the install record names %s, not %s", rec.Name, n)
	}
	if on {
		if err := e.oldTasksGate(oldOK); err != nil {
			return err
		}
	}
	if dry {
		fmt.Fprintln(e.out, "dry run: nothing is changed")
	}
	changed, err := autostart.SetEnabled(e.ops(dry), rec.Mode, rec.Name, on)
	if err != nil {
		return failed("holocron autostart %s: %v", verb, err)
	}
	switch {
	case dry:
	case changed:
		fmt.Fprintf(e.out, "%sd and verified: %s\n", verb, e.recorded(rec.Mode, rec.Name).Summary())
	default:
		fmt.Fprintf(e.out, "already %sd, unchanged: %s\n", verb, e.recorded(rec.Mode, rec.Name).Summary())
	}
	return nil
}

// recorded is the state of an entry the install record names.
func (e *env) recorded(m autostart.Mode, name string) autostart.State {
	st := autostart.Status(e.auto, m, name)
	st.Recorded = true
	return st
}

// autostartState is the entry `status`, `doctor` and `autostart status` report: the recorded
// one, else the OS default under holocron's own name (or the test name).
func (e *env) autostartState() autostart.State {
	rec, ok, err := autostart.ReadRecord(e.recordPath())
	if ok {
		return e.recorded(rec.Mode, rec.Name)
	}
	m, merr := autostart.DefaultMode(e.auto.GOOS)
	if merr != nil {
		return autostart.State{Detail: merr.Error()}
	}
	n, nerr := e.entryName("", m)
	if nerr != nil {
		n = autostart.DefaultName(m)
	}
	st := autostart.Status(e.auto, m, n)
	if err != nil {
		st.Detail = strings.TrimSpace(st.Detail + " the install record cannot be read: " + err.Error())
	}
	return st
}

type autostartReport struct {
	Autostart autostart.State     `json:"autostart"`
	Record    string              `json:"record"`
	OldTasks  []autostart.OldTask `json:"oldTasks,omitempty"`
}

// autostartReportOf is what `autostart status` and the Admin tab report (the old tasks read only).
func autostartReportOf(e *env) autostartReport {
	rep := autostartReport{Autostart: e.autostartState(), Record: e.recordPath()}
	if e.auto.GOOS == "windows" {
		rep.OldTasks = autostart.QueryOldTasks(e.auto)
	}
	return rep
}

func cmdAutostartStatus(ctx context.Context, e *env) error {
	f := newInstallFlags(e, "autostart status")
	if err := f.parse(e, "autostart status"); err != nil {
		return err
	}
	rep := autostartReportOf(e)
	if e.json {
		return printJSON(e.out, rep)
	}
	fmt.Fprintln(e.out, "autostart:", rep.Autostart.Summary())
	if rep.Autostart.Recorded {
		fmt.Fprintln(e.out, "install record:", rep.Record)
	} else {
		fmt.Fprintln(e.out, "install record: none at", rep.Record)
	}
	if rep.OldTasks != nil {
		fmt.Fprintln(e.out, "PowerShell system tasks:", oldTasksText(rep.OldTasks))
	}
	return nil
}

// autostartChecks are doctor's lines: the entry, and (Windows) the old system's tasks.
func autostartChecks(e *env) []check {
	st := e.autostartState()
	var cs []check
	switch {
	case st.Detail != "" && !st.Installed:
		cs = append(cs, mk(lvWarn, "autostart", "%s", st.Summary()).fix("holocron autostart status, then holocron install"))
	case !st.Installed:
		cs = append(cs, mk(lvOK, "autostart", "%s", st.Summary()))
	case !st.ExeExists || !st.Recorded:
		cs = append(cs, mk(lvWarn, "autostart", "%s", st.Summary()).fix("holocron install (run from the executable that should start at logon)"))
	default:
		cs = append(cs, mk(lvOK, "autostart", "%s", st.Summary()))
	}
	if e.auto.GOOS != "windows" {
		return cs
	}
	olds := autostart.QueryOldTasks(e.auto)
	v := lvOK
	for _, o := range olds {
		if o.Enabled && st.Installed && st.Enabled {
			v = lvWarn
		}
	}
	text := oldTasksText(olds)
	c := mk(v, "old tasks", "%s", text)
	if v == lvWarn {
		c.Detail += " - both systems would start at logon"
		c = c.fix("switch the enabled PowerShell task off yourself: schtasks /Change /TN <task> /DISABLE (holocron never changes it)")
	}
	return append(cs, c)
}

// serviceGate is what --mode service says and checks BEFORE it asks: elevation, the account's
// own profile (no Credential Manager, other default folders), the file secrets store with explicit
// paths and a key that account can read; then the confirmation (--yes skips it; a dry run asks
// nothing).
func (e *env) serviceGate(s autostart.Spec, dry, yes bool) error {
	acct := autostart.ServiceAccount(s.Account)
	w := e.out
	fmt.Fprintf(w, "--mode service creates a Windows service %s that the system starts at boot (delayed), before anyone logs on, as %s:\n", s.Name, acct)
	fmt.Fprintln(w, "  - install, uninstall and autostart enable|disable need an elevated (Run as administrator) prompt")
	fmt.Fprintf(w, "  - %s has no Windows Credential Manager and its own profile: the camera login must be in the encrypted file store (secrets.backend file, with secrets.file and secrets.keyFile set to absolute paths)\n", acct)
	fmt.Fprintf(w, "  - HOLOCRON_SECRET_KEY (in the service's environment) or the key file must be readable by %s; a key in your own profile is not\n", acct)
	fmt.Fprintf(w, "  - the archive root, the configuration and the web app's folders (web.authDir, web.tlsDir; their defaults are in YOUR profile) must be usable by %s\n", acct)
	fmt.Fprintln(w, "  - there is no console: closing a window does not stop it; holocron down or sc stop does, and the system restarts it 60 s after a failure")
	sc := e.cfg.Secrets
	switch {
	case sc.Backend != "file":
		return cli.Refuse("--mode service needs secrets.backend file (it is %q): holocron config set secrets.backend file, set secrets.file and secrets.keyFile in %s to absolute paths %s can read, store the login with holocron secret set camera.user and camera.password, then install again", sc.Backend, e.paths.Config, acct)
	case !isAbs(sc.File) || !isAbs(sc.KeyFile):
		return cli.Refuse("--mode service needs secrets.file and secrets.keyFile as absolute paths in %s (the defaults are in the installing user's profile, which %s does not see); now secrets.file %q, secrets.keyFile %q", e.paths.Config, acct, sc.File, sc.KeyFile)
	}
	fmt.Fprintf(w, "secrets: file store %s, key file %s (or HOLOCRON_SECRET_KEY)\n", sc.File, sc.KeyFile)
	if dry || yes {
		return nil
	}
	fmt.Fprint(w, "type yes to create the service: ")
	line, _, err := readLine(e.stdin, 64)
	fmt.Fprintln(w)
	if err != nil || strings.TrimSpace(line) != "yes" {
		return cli.Refuse("not confirmed (type yes, or add --yes)")
	}
	return nil
}

func isAbs(p string) bool {
	return filepath.IsAbs(p) || len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// cmdRoleService is what the Windows service manager starts (`holocron service`, hidden): the host
// in service mode; Stop, Shutdown and Preshutdown are an ordered, requested stop (as `down`).
func cmdRoleService(ctx context.Context, e *env) error {
	if err := wantArgs("service", e.args, 0, 0); err != nil {
		return err
	}
	exe, err := e.self()
	if err != nil {
		return failed("%v", err)
	}
	code := 0
	err = osx.RunService(autostart.ServiceName, func(sctx context.Context) int {
		code = app.RunHost(sctx, app.HostOptions{Options: e.opts, Service: true, Exe: exe, SupArgs: e.global})
		return code
	})
	switch {
	case errors.Is(err, osx.ErrNotService):
		return usageErr("holocron service is the role the Windows service manager starts (holocron install --mode service); run holocron up instead")
	case err != nil:
		return failed("%v", err)
	case code != 0:
		return &exitError{code: code}
	}
	return nil
}
