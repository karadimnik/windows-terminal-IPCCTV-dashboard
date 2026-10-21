package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"holocron/internal/alerts"
	"holocron/internal/host"
	"holocron/internal/logx"
	"holocron/internal/osx"
)

// WebStopFile is <state>/web.stop: the host creates it to end the web role (a file works the same
// on every OS, and the web child, with its own hidden console on Windows, receives no signal).
func (p Paths) WebStopFile() string { return filepath.Join(p.State(), "web.stop") }

// webAux is the web role as the host's auxiliary child: `<exe> <global flags> web --stop-file ...`
// in the host's own scope, restarted in place (internal/host Aux), listed in <state>/webchild.json
// for the census of `down`. Before it starts, the web child a crashed earlier host left behind is
// reaped when it is a provable orphan and no other host is alive.
func webAux(p Paths, scope *osx.Scope, exe string, global []string, log *slog.Logger) host.Aux {
	stop := p.WebStopFile()
	_ = os.Remove(stop)
	reg := osx.NewRegistry(p.AuxFile())
	in := osx.OSInspector{}
	reapStaleWeb(p, reg, in, log)
	args := append(append([]string{}, global...), "web", "--stop-file", stop)
	return host.Aux{
		Name:    "web",
		Spawner: &host.ExecSpawner{Scope: scope, Path: exe, Args: args},
		Restart: host.AuxRestart{AlertID: alerts.IDWebStopped, AlertTitle: "the web app stopped unexpectedly and is being restarted"},
		Stop: func(host.Proc) {
			_ = os.MkdirAll(p.State(), 0o700)
			_ = os.WriteFile(stop, []byte(fmt.Sprintf("requested by the host pid %d at %s\n", os.Getpid(), time.Now().Format(time.RFC3339))), 0o600)
		},
		StopWait:  5 * time.Second,
		Registry:  reg,
		Inspector: in,
	}
}

// reapStaleWeb stops what an earlier host's aux registry still lists, only when it is alive AND the
// same process (osx.Reap) and no other host is alive to own it.
func reapStaleWeb(p Paths, reg *osx.ChildRegistry, in osx.Inspector, log *slog.Logger) {
	if h, ok := ReadHostRecord(p); ok && h.Pid != os.Getpid() {
		if alive, _ := osx.Verify(h, in, runtime.GOOS); alive {
			return
		}
	}
	if entries, err := reg.Load(); err != nil || len(entries) == 0 {
		return
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if _, err := osx.Reap(reg, in, log); err != nil {
		log.Warn("web orphan reap: "+err.Error(), logx.Action("web.reap"), logx.Result(logx.ResultFail))
	}
}
