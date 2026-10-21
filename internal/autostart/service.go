package autostart

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"holocron/internal/logx"
	"holocron/internal/osx"
)

// winService is a real Windows service through the osx.ServiceManager seam (advapi32). UNVERIFIED
// on a real system: it needs an elevated prompt, which the tests never take; FakeSCM covers it.
type winService struct{ env Env }

// ServiceRestartDelay is the recovery action: restart after a failure (also a non-zero exit).
const ServiceRestartDelay = 60 * time.Second

// Accounts that need no password.
var serviceAccounts = []string{"LocalSystem", `NT AUTHORITY\LocalService`, `NT AUTHORITY\NetworkService`}

// ServiceAccount is the account a Spec's service runs as.
func ServiceAccount(a string) string {
	if a == "" {
		return "LocalSystem"
	}
	return a
}

// CheckAccount accepts the service accounts that need no password.
func CheckAccount(a string) error {
	a = ServiceAccount(a)
	for _, ok := range serviceAccounts {
		if strings.EqualFold(a, ok) {
			return nil
		}
	}
	if strings.HasPrefix(strings.ToUpper(a), `NT SERVICE\`) && len(a) > len(`NT SERVICE\`) {
		return nil
	}
	return usage("--account %q: only accounts without a password are supported (LocalSystem, NT AUTHORITY\\LocalService, NT AUTHORITY\\NetworkService, NT SERVICE\\<name>)", a)
}

func (w winService) entry(s Spec) (Entry, error) {
	for _, c := range [][2]string{{"the executable", s.Exe}, {"the configuration", s.Config}, {"the root", s.Root}} {
		if c[1] == "" && c[0] == "the root" {
			continue
		}
		if err := checkWinPath(c[0], c[1]); err != nil {
			return Entry{}, err
		}
	}
	if err := CheckAccount(s.Account); err != nil {
		return Entry{}, err
	}
	argv := append([]string{s.Exe}, s.holocronArgs("service")...)
	display := "Holocron NVR"
	if s.Name != ServiceName {
		display += " (" + s.Name + ")"
	}
	c := osx.ServiceConfig{Name: s.Name, DisplayName: display, BinaryPath: winJoin(argv), Account: ServiceAccount(s.Account), Start: osx.StartAuto, Delayed: true, RestartDelay: ServiceRestartDelay,
		Description: "Holocron NVR: the recorder, relay, live view and web app. Created by holocron install --mode service; holocron uninstall removes it."}
	if s.Disabled {
		c.Start = osx.StartDisabled
	}
	return serviceEntry(c), nil
}

func serviceEntry(c osx.ServiceConfig) Entry {
	argv := splitWin(c.BinaryPath)
	e := Entry{Mode: Service, Name: c.Name, Argv: argv, Enabled: c.Start != osx.StartDisabled, Service: &c, Content: serviceText(c),
		Key: fmt.Sprintf("account=%s delayed=%v restart=%s display=%s", strings.ToLower(ServiceAccount(c.Account)), c.Delayed && c.Start == osx.StartAuto, c.RestartDelay, c.DisplayName)}
	if len(argv) > 0 {
		e.Exe = argv[0]
	}
	return e
}

func serviceText(c osx.ServiceConfig) string {
	return fmt.Sprintf("service name:  %s\ndisplay name:  %s\ncommand line:  %s\naccount:       %s\nstart:         %s%s\nrecovery:      restart after %s on a failure (also a non-zero exit), 3 times, counter reset after 1 day\ndescription:   %s\n",
		c.Name, c.DisplayName, c.BinaryPath, ServiceAccount(c.Account), c.Start, map[bool]string{true: " (delayed)", false: ""}[c.Delayed && c.Start == osx.StartAuto],
		c.RestartDelay, c.Description)
}

func (w winService) scm() (osx.ServiceManager, error) {
	if w.env.SCM == nil {
		return nil, osx.ErrNoServiceManager
	}
	return w.env.SCM, nil
}

func (w winService) current(name string) (*Entry, State, error) {
	m, err := w.scm()
	if err != nil {
		return nil, State{}, err
	}
	st, err := m.Query(name)
	if err != nil || !st.Exists {
		return nil, State{}, err
	}
	c := st.Config
	c.Name = name
	e := serviceEntry(c)
	return &e, State{Running: st.State}, nil
}

// scmCall runs one change through the manager; a dry run prints it.
func (o *Ops) scmCall(what string, fn func() error) error {
	if o.Dry {
		fmt.Fprintln(o.Out, "would call the service control manager:", what)
		return nil
	}
	o.logger().Debug("scm: "+what, logx.Action("install.scm"))
	if err := fn(); err != nil {
		o.logger().Error("scm failed: "+what, logx.Action("install.scm"), logx.Result(logx.ResultFail), logx.Err(err))
		if errors.Is(err, osx.ErrAccessDenied) {
			return err
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func (w winService) apply(o *Ops, want Entry, prev *Entry) error {
	m, err := w.scm()
	if err != nil {
		return err
	}
	c := *want.Service
	if o.Dry {
		fmt.Fprintf(o.Out, "the Windows service would be:\n%s\n", indent(want.Content))
	} else {
		o.logger().Debug("service settings for "+want.Name, logx.Action("install.scm"), logx.Target(want.Name))
	}
	if prev == nil {
		if err := o.scmCall("CreateService "+c.Name+" + ChangeServiceConfig2 (description, delayed start, recovery)", func() error { return m.Create(c) }); err != nil {
			return err
		}
	} else if err := o.scmCall("ChangeServiceConfig "+c.Name+" + ChangeServiceConfig2", func() error { return m.Update(c) }); err != nil {
		return err
	}
	o.info(fmt.Sprintf("service %s configured (start %s, account %s)", c.Name, c.Start, ServiceAccount(c.Account)))
	return nil
}

func (w winService) remove(o *Ops, cur Entry) error {
	m, err := w.scm()
	if err != nil {
		return err
	}
	if err := o.scmCall("DeleteService "+cur.Name, func() error { return m.Delete(cur.Name) }); err != nil {
		return err
	}
	o.info("service " + cur.Name + " marked for deletion (a running service goes when it stops)")
	return nil
}

func (w winService) enable(o *Ops, cur Entry, on bool) error {
	m, err := w.scm()
	if err != nil {
		return err
	}
	start := map[bool]osx.ServiceStart{true: osx.StartAuto, false: osx.StartDisabled}[on]
	if err := o.scmCall(fmt.Sprintf("ChangeServiceConfig %s start=%s", cur.Name, start), func() error { return m.SetStart(cur.Name, start, true) }); err != nil {
		return err
	}
	o.info(fmt.Sprintf("service %s start type %s", cur.Name, start))
	return nil
}
