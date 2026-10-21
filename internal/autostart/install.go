package autostart

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"holocron/internal/logx"
)

// backend is one mode's entry on the system.
type backend interface {
	// entry generates and validates the entry for s.
	entry(s Spec) (Entry, error)
	// current reads the entry named name; nil when there is none.
	current(name string) (*Entry, State, error)
	// apply creates want, or replaces prev with it.
	apply(o *Ops, want Entry, prev *Entry) error
	remove(o *Ops, cur Entry) error
	enable(o *Ops, cur Entry, on bool) error
}

func backendFor(m Mode, env Env) (backend, error) {
	switch m {
	case Logon:
		return logonTask{env}, nil
	case Service:
		return winService{env}, nil
	case Systemd:
		return systemdUnit{env}, nil
	case Launchd:
		return launchdAgent{env}, nil
	}
	return nil, usage("unknown mode %q", m)
}

// Generate returns the entry `install` would create for s, without looking at the system.
func Generate(env Env, s Spec) (Entry, error) {
	b, err := backendFor(s.Mode, env)
	if err != nil {
		return Entry{}, err
	}
	return b.entry(s)
}

// Result is what Install did.
type Result struct {
	Entry     Entry
	Created   bool     // there was no entry before
	Changed   bool     // the entry was created or replaced (false: already installed, unchanged)
	Changes   []string // what differed from the entry that was there
	HadRecord bool
}

// diff lists what differs between the entry that is there and the one wanted.
func diff(want, cur Entry) []string {
	var d []string
	if w, c := want.Command(), cur.Command(); w != c {
		d = append(d, fmt.Sprintf("command: %s -> %s", c, w))
	}
	same := want.WorkDir == cur.WorkDir || (want.Mode == Logon || want.Mode == Service) && strings.EqualFold(want.WorkDir, cur.WorkDir)
	if !same {
		d = append(d, fmt.Sprintf("working folder: %s -> %s", orNone(cur.WorkDir), orNone(want.WorkDir)))
	}
	if want.Key != cur.Key {
		d = append(d, fmt.Sprintf("settings: %s -> %s", cur.Key, want.Key))
	}
	if want.File != "" && want.Content != cur.Content {
		d = append(d, "content of "+want.File)
	}
	if want.Window != cur.Window {
		d = append(d, fmt.Sprintf("console window: %s -> %s", orNone(cur.Window), orNone(want.Window)))
	}
	if want.Enabled != cur.Enabled {
		d = append(d, fmt.Sprintf("enabled: %v -> %v", cur.Enabled, want.Enabled))
	}
	return d
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// Install creates the entry for s, or updates it, verifies it by reading it back and records it
// in <config dir>/_state/install.json. Idempotent: an entry that already matches is left alone. A
// failing step undoes what this run did (the entry it created is removed, the one it replaced is
// put back).
func Install(o *Ops, s Spec, version string) (Result, error) {
	b, err := backendFor(s.Mode, o.Env)
	if err != nil {
		return Result{}, err
	}
	want, err := b.entry(s)
	if err != nil {
		return Result{}, err
	}
	recPath := s.RecordFile()
	_, had, _ := ReadRecord(recPath)
	res := Result{Entry: want, HadRecord: had}
	cur, _, err := b.current(s.Name)
	if err != nil {
		return res, fmt.Errorf("cannot read the current %s %s: %w", kind(s.Mode), s.Name, err)
	}
	rec := Record{Mode: s.Mode, Name: s.Name, Exe: s.Exe, Command: want.Command(), File: want.File, Window: want.Window, InstalledAt: time.Now(), Version: version}
	if cur != nil {
		res.Changes = diff(want, *cur)
		if len(res.Changes) == 0 {
			fmt.Fprintf(o.Out, "already installed, unchanged: %s %s runs %s\n", kind(s.Mode), s.Name, want.Command())
			if !had {
				if o.Dry {
					fmt.Fprintln(o.Out, "would write the install record", recPath)
				} else if err := WriteRecord(recPath, rec); err != nil {
					return res, fmt.Errorf("the entry is in place but the install record %s could not be written: %w", recPath, err)
				} else {
					fmt.Fprintln(o.Out, "it had no install record; recorded it in", recPath)
				}
			}
			return res, nil
		}
		fmt.Fprintf(o.Out, "updating %s %s:\n", kind(s.Mode), s.Name)
		for _, c := range res.Changes {
			fmt.Fprintln(o.Out, "  "+c)
		}
		if !had {
			fmt.Fprintf(o.Out, "  (it was there without an install record; it carries holocron's own name, so it is replaced)\n")
		}
	} else {
		res.Created = true
		fmt.Fprintf(o.Out, "creating %s %s: %s\n", kind(s.Mode), s.Name, want.Command())
	}
	res.Changed = true
	fail := func(step error) (Result, error) {
		if rb := rollback(o, b, want, cur); rb != nil {
			return res, fmt.Errorf("%w; the rollback failed too: %v", step, rb)
		}
		return res, fmt.Errorf("%w (rolled back: %s)", step, map[bool]string{true: "the new entry was removed", false: "the previous entry was put back"}[cur == nil])
	}
	if err := b.apply(o, want, cur); err != nil {
		return fail(err)
	}
	if o.Dry {
		fmt.Fprintln(o.Out, "would write the install record", recPath)
		return res, nil
	}
	got, _, err := b.current(s.Name)
	switch {
	case err != nil:
		return fail(fmt.Errorf("verification: cannot read the entry back: %w", err))
	case got == nil:
		return fail(errors.New("verification: the entry is not there after it was created"))
	default:
		if d := diff(want, *got); len(d) > 0 {
			return fail(fmt.Errorf("verification: the entry read back differs: %s", strings.Join(d, "; ")))
		}
	}
	if err := WriteRecord(recPath, rec); err != nil {
		return fail(fmt.Errorf("the install record %s could not be written: %w", recPath, err))
	}
	verb := "created"
	if !res.Created {
		verb = "updated"
	}
	o.logger().Info(fmt.Sprintf("%s %s %s, verified by reading it back: runs %s, enabled %v", verb, kind(s.Mode), s.Name, want.Command(), want.Enabled),
		logx.Action("install.entry"), logx.Target(s.Name), logx.Result(logx.ResultOK))
	return res, nil
}

func rollback(o *Ops, b backend, want Entry, prev *Entry) error {
	if o.Dry {
		return nil
	}
	// undo against what is there now: a step may have failed before or after its change
	now, _, err := b.current(want.Name)
	switch {
	case err != nil:
	case prev == nil && now != nil:
		err = b.remove(o, *now)
	case prev != nil:
		err = b.apply(o, *prev, now)
	}
	if err != nil {
		o.logger().Error("rollback failed: "+err.Error(), logx.Action("install.rollback"), logx.Target(want.Name), logx.Result(logx.ResultFail), logx.Err(err))
	} else {
		o.logger().Warn("rolled back "+want.Name, logx.Action("install.rollback"), logx.Target(want.Name), logx.Result(logx.ResultOK))
	}
	return err
}

// Remove deletes the entry and verifies it is gone. removed is false when it was not there.
func Remove(o *Ops, m Mode, name string) (removed bool, err error) {
	b, err := backendFor(m, o.Env)
	if err != nil {
		return false, err
	}
	cur, _, err := b.current(name)
	if err != nil {
		return false, fmt.Errorf("cannot read %s %s: %w", kind(m), name, err)
	}
	if cur == nil {
		return false, nil
	}
	fmt.Fprintf(o.Out, "removing %s %s (runs %s)\n", kind(m), name, cur.Command())
	if err := b.remove(o, *cur); err != nil {
		return false, err
	}
	if o.Dry {
		return true, nil
	}
	if got, _, err := b.current(name); err != nil || got != nil {
		if m == Service && err == nil {
			return false, fmt.Errorf("the Windows service %s is marked for deletion and goes when it stops (holocron down, or sc stop %s); run holocron uninstall again then", name, name)
		}
		return false, fmt.Errorf("verification: %s %s is still there after the removal (%v)", kind(m), name, err)
	}
	o.logger().Info(fmt.Sprintf("removed %s %s, verified gone", kind(m), name), logx.Action("install.remove"), logx.Target(name), logx.Result(logx.ResultOK))
	return true, nil
}

// SetEnabled switches an installed entry on or off. changed is false when it already was.
func SetEnabled(o *Ops, m Mode, name string, on bool) (changed bool, err error) {
	b, err := backendFor(m, o.Env)
	if err != nil {
		return false, err
	}
	cur, _, err := b.current(name)
	if err != nil {
		return false, fmt.Errorf("cannot read %s %s: %w", kind(m), name, err)
	}
	if cur == nil {
		return false, fmt.Errorf("%s %s is not installed (holocron install creates it)", kind(m), name)
	}
	if cur.Enabled == on {
		return false, nil
	}
	if err := b.enable(o, *cur, on); err != nil {
		return false, err
	}
	if o.Dry {
		return true, nil
	}
	got, _, err := b.current(name)
	if err != nil || got == nil || got.Enabled != on {
		return false, fmt.Errorf("verification: %s %s did not switch (%v)", kind(m), name, err)
	}
	o.logger().Info(fmt.Sprintf("%s %s %s, verified", kind(m), name, map[bool]string{true: "enabled", false: "disabled"}[on]),
		logx.Action("install.enable"), logx.Target(name), logx.Result(logx.ResultOK))
	return true, nil
}

// Status reads the entry as it is on the system (read-only).
func Status(env Env, m Mode, name string) State {
	st := State{Mode: m, Name: name}
	b, err := backendFor(m, env)
	if err != nil {
		st.Detail = err.Error()
		return st
	}
	cur, s, err := b.current(name)
	if err != nil {
		s.Mode, s.Name, s.Detail = m, name, err.Error()
		return s
	}
	if cur == nil {
		s.Mode, s.Name = m, name
		return s
	}
	s.Mode, s.Name, s.Installed, s.Enabled = m, name, true, cur.Enabled
	s.Command, s.Exe, s.File, s.Window = cur.Command(), cur.Exe, cur.File, cur.Window
	s.ExeExists = cur.Exe != "" && env.stat(cur.Exe)
	return s
}

// ---- the record ----

// ReadRecord reads the install record; ok is false when there is none.
func ReadRecord(path string) (Record, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return r, r.Name != "", nil
}

// WriteRecord writes it atomically.
func WriteRecord(path string, r Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveRecord deletes it (a missing record is fine).
func RemoveRecord(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
