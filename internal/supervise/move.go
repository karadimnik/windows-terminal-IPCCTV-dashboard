package supervise

import (
	"fmt"
	"log/slog"
	"time"

	"holocron/internal/archive"
	"holocron/internal/logx"
	"holocron/internal/osx"
)

// Mover is the archive mover as the supervisor drives it (decision D20).
type Mover interface {
	// File is one cycle: closed spool segments filed into day folders (archive.FileDays).
	File(now time.Time) archive.MoveReport
}

// ArchiveMover is the production Mover: archive.FileDays over every camera folder of Root, which
// refuses a root without a valid ownership marker or outside the sandbox (Sandbox, or SANDBOX_ONLY
// in the environment).
type ArchiveMover struct {
	Root      string
	Platform  osx.Platform
	Container string // extension, ".mkv"
	Margin    time.Duration
	Max       int
	Sandbox   string
}

func (a ArchiveMover) File(now time.Time) archive.MoveReport {
	l := archive.Layout{Root: a.Root, Matcher: archive.MatcherFor(a.Platform), Container: a.Container}
	return archive.FileDays(l, archive.MoveOptions{Platform: a.Platform, Now: now, Margin: a.Margin, Max: a.Max, Sandbox: a.Sandbox})
}

// moveLoop runs a mover cycle at start and every MoveIntervalSeconds on its own goroutine, so a
// slow or hung file system never holds the status tick or recording. A cycle and a prune run
// never overlap (archMu). While MoveToDayFolders is false (read at each reconcile) a cycle does
// nothing. A cycle that moved nothing but met busy files or errors doubles the wait, up to ten
// intervals; a clean cycle resets it.
func (s *Supervisor) moveLoop(done chan<- struct{}) {
	defer close(done)
	defer s.recoverFault("the mover")
	interval := seconds(s.cfg.MoveIntervalSeconds)
	wait := interval
	for {
		if s.moveOnce() {
			wait = min(2*wait, 10*interval)
		} else {
			wait = interval
		}
		select {
		case <-s.clk.After(wait):
		case <-s.stopCh:
			return
		}
	}
}

// moveOnce is one cycle; true when it should back off.
func (s *Supervisor) moveOnce() bool {
	now := s.clk.Now()
	if !s.moveOn.Load() {
		return false
	}
	s.archMu.Lock()
	rep := s.d.Mover.File(now)
	s.archMu.Unlock()
	s.reportMove(rep)
	return len(rep.Moved) == 0 && (len(rep.Busy) > 0 || len(rep.Errors) > 0)
}

// reportMove logs a cycle (action archive.file). status.json is unchanged: its key set is pinned
// to the PowerShell system's (testdata/golden/status), so the counts live in the log.
func (s *Supervisor) reportMove(rep archive.MoveReport) {
	root := s.cfg.RootPath
	attrs := []any{logx.Action("archive.file"), logx.Target(root)}
	msg := fmt.Sprintf("filed %d segment(s), %d byte(s), into day folders; %d busy, %d conflict(s), %d error(s), %d deferred to the next cycle",
		len(rep.Moved), rep.Bytes, len(rep.Busy), len(rep.Conflicts), len(rep.Errors), rep.Deferred)
	switch {
	case rep.Refused != "":
		msg = "the mover is refused: " + rep.Refused
		s.plog.Warn(msg, append(attrs, logx.Result(logx.ResultRefused))...)
	case len(rep.Moved) > 0:
		s.plog.Info(msg, append(attrs, logx.Result(logx.ResultOK), slog.Int("moved", len(rep.Moved)), slog.Int64("bytes", rep.Bytes))...)
	default:
		s.plog.Debug(msg, append(attrs, logx.Result(logx.ResultOK))...)
	}
	for _, m := range rep.Moved {
		s.plog.Debug("filed "+m.From+" into "+m.To, logx.Action("archive.file"), logx.Target(m.From), logx.Result(logx.ResultOK), slog.Int64("bytes", m.Bytes))
	}
	for _, x := range rep.Conflicts {
		s.plog.Warn("not filed "+x.Path+": "+x.Why, logx.Action("archive.file"), logx.Target(x.Path), logx.Result(logx.ResultRefused), slog.String("reason", "conflict"))
	}
	for _, x := range rep.Skipped {
		s.plog.Debug("not filed "+x.Path+": "+x.Why, logx.Action("archive.file"), logx.Target(x.Path), logx.Result(logx.ResultRefused))
	}
	for _, x := range rep.Busy {
		s.plog.Debug("not filed "+x.Path+" (in use, the next cycle tries again): "+x.Why, logx.Action("archive.file"), logx.Target(x.Path), logx.Result(logx.ResultRefused), slog.String("reason", "busy"))
	}
	for _, x := range rep.Errors {
		s.plog.Warn("not filed "+x.Path+": "+x.Why, logx.Action("archive.file"), logx.Target(x.Path), logx.Result(logx.ResultFail))
	}
}
