package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

	"holocron/internal/osx"
	"holocron/internal/status"
)

// WriteHostRecord writes <state>/host.json (pid, start token, image of this process) so `down` can
// count the host by identity, never by name. The returned function removes it.
func WriteHostRecord(p Paths, in osx.Inspector) (func(), error) {
	info, err := in.Inspect(os.Getpid())
	if err != nil {
		return func() {}, err
	}
	e := osx.ChildEntry{Pid: os.Getpid(), Start: info.Start, Image: info.Image, Role: "host"}
	b, _ := json.Marshal(e)
	if err := os.MkdirAll(p.State(), 0o700); err != nil {
		return func() {}, err
	}
	if err := writeAtomic(p.HostFile(), b); err != nil {
		return func() {}, err
	}
	return func() {
		if cur, ok := ReadHostRecord(p); ok && cur.Pid == e.Pid && cur.Start == e.Start {
			_ = os.Remove(p.HostFile())
		}
	}, nil
}

// ReadHostRecord reads host.json.
func ReadHostRecord(p Paths) (osx.ChildEntry, bool) {
	b, err := os.ReadFile(p.HostFile())
	if err != nil {
		return osx.ChildEntry{}, false
	}
	var e osx.ChildEntry
	if json.Unmarshal(b, &e) != nil || e.Pid <= 0 {
		return osx.ChildEntry{}, false
	}
	return e, true
}

// Census is what of holocron is alive for one root, each found by identity: the host by its
// record (pid + start time + image), the supervisor by its instance lock (held) or its own
// heartbeat (status.Verdict), the children by the supervisor's registry (osx.Verify).
type Census struct {
	Host          *osx.ChildEntry  `json:"host"`
	Supervisor    int              `json:"supervisor"` // pid, 0 when none (or -1: lock held, owner unknown)
	SupervisorWhy string           `json:"supervisorWhy,omitempty"`
	Children      []osx.ChildEntry `json:"children"`
}

// Count is every live process found.
func (c Census) Count() int {
	n := len(c.Children)
	if c.Host != nil {
		n++
	}
	if c.Supervisor != 0 {
		n++
	}
	return n
}

// Lines lists the survivors for the Operator.
func (c Census) Lines() []string {
	var out []string
	if c.Host != nil {
		out = append(out, fmt.Sprintf("host pid %d (%s)", c.Host.Pid, c.Host.Image))
	}
	if c.Supervisor > 0 {
		out = append(out, fmt.Sprintf("supervisor pid %d (%s)", c.Supervisor, c.SupervisorWhy))
	} else if c.Supervisor < 0 {
		out = append(out, "supervisor (lock held, owner unknown)")
	}
	for _, e := range c.Children {
		out = append(out, fmt.Sprintf("%s pid %d (%s)", e.Role, e.Pid, e.Image))
	}
	return out
}

// TakeCensus counts what is alive for p. It reads files and probes the lock; it stops nothing.
func TakeCensus(p Paths, in osx.Inspector, statusInterval float64) Census {
	c := Census{Children: []osx.ChildEntry{}}
	if e, ok := ReadHostRecord(p); ok {
		if alive, _ := osx.Verify(e, in, runtime.GOOS); alive {
			c.Host = &e
		}
	}
	if _, err := os.Stat(p.LockFile()); err == nil {
		lk, err := osx.AcquireInstanceLock(p.LockFile())
		switch {
		case err == nil:
			_ = lk.Release()
		case errors.Is(err, osx.ErrHeld):
			var he *osx.HeldError
			c.Supervisor, c.SupervisorWhy = -1, "holds the instance lock"
			if errors.As(err, &he) && he.Owner.Pid > 0 {
				c.Supervisor = he.Owner.Pid
			}
		}
	}
	if c.Supervisor == 0 {
		if doc, err := status.Read(p.StatusFile()); err == nil {
			if running, why := status.Verdict(doc, time.Now(), status.OSProcesses{}, status.VerdictConfig{StatusIntervalSeconds: statusInterval}); running {
				c.Supervisor, c.SupervisorWhy = int(doc.SupervisorPid), "fresh heartbeat: "+why
			}
		}
	}
	for _, path := range []string{osx.RegistryPath(p.State()), p.AuxFile()} {
		entries, err := osx.NewRegistry(path).Load()
		if err != nil {
			continue
		}
		for _, e := range entries {
			if alive, _ := osx.Verify(e, in, runtime.GOOS); alive {
				c.Children = append(c.Children, e)
			}
		}
	}
	return c
}

// ReapOrphans is the startup reap (osx.Reap: alive AND same start time AND same image AND same
// group, never by name) run by `down` when no supervisor is running: the children the supervisor's
// registry still lists and, only when no host is alive to own them, the host's aux children (the
// web role). It holds the instance lock so a supervisor cannot start under it; a held lock reaps
// nothing. Every entry is reported, killed or left alone.
func ReapOrphans(p Paths, in osx.Inspector, log *slog.Logger, withAux bool) ([]osx.ReapVerdict, error) {
	lk, err := osx.AcquireInstanceLock(p.LockFile())
	if err != nil {
		return nil, err
	}
	defer lk.Release()
	paths := []string{osx.RegistryPath(p.State())}
	if withAux {
		paths = append(paths, p.AuxFile())
	}
	var out []osx.ReapVerdict
	var errs []error
	for _, path := range paths {
		v, err := osx.Reap(osx.NewRegistry(path), in, log)
		out = append(out, v...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}
