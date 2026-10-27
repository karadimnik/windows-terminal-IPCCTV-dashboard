package prune

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"holocron/internal/archive"
	"holocron/internal/logx"
	"holocron/internal/osx"
)

// Info is what the executor needs to know about a path, read WITHOUT following a final link.
type Info struct {
	Size    int64
	MTime   time.Time // UTC
	Dir     bool
	Regular bool
	Reparse bool
}

// FS is the filesystem the executor deletes through: OSFS in production, a fake in tests.
type FS interface {
	Remove(path string) error              // one file; never a directory, never recursive
	RemoveDir(path string) error           // one empty directory (rmdir); never a file, never recursive; ErrDirNotEmpty when it holds anything
	Stat(path string) (Info, error)        // lstat semantics: a link is reported, not followed
	FreeBytes(root string) (uint64, error) // available to the user on the root's volume
}

// OSFS is the real filesystem.
type OSFS struct{ Platform osx.Platform }

// Remove unlinks a single non-directory entry (removeFile refuses directories).
func (OSFS) Remove(path string) error { return removeFile(path) }

// RemoveDir removes one empty directory (removeDir: rmdir / RemoveDirectory, which refuse a
// directory that holds anything, atomically).
func (OSFS) RemoveDir(path string) error { return removeDir(path) }

// ErrDirNotEmpty is RemoveDir's answer for a directory that holds anything.
var ErrDirNotEmpty = errors.New("the folder is not empty")

func (OSFS) Stat(path string) (Info, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Info{}, err
	}
	return Info{Size: fi.Size(), MTime: fi.ModTime().UTC(), Dir: fi.IsDir(), Regular: fi.Mode().IsRegular(), Reparse: archive.IsReparse(fi)}, nil
}

func (o OSFS) FreeBytes(root string) (uint64, error) { return o.Platform.DiskFree(root) }

// SandboxEnv names the test switch: when set, the executor refuses any root or file outside the
// sandbox prefix. "1" means the OS temporary directory (where t.TempDir lives); any other value
// is the prefix itself. The prune tests set it for their whole run (TestMain).
const SandboxEnv = "SANDBOX_ONLY"

// ErrOutsideSandbox is returned when a sandbox is in force and a path lies outside it.
var ErrOutsideSandbox = errors.New("outside the sandbox")

// CheckSandbox returns nil only when path lies strictly inside prefix, judged on the absolute,
// cleaned paths and again with existing links resolved.
func CheckSandbox(path, prefix string) error {
	if prefix == "" {
		return nil
	}
	inside := func(p, base string) bool {
		rel, err := filepath.Rel(base, p)
		return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	ap, err1 := filepath.Abs(path)
	bp, err2 := filepath.Abs(prefix)
	if err1 != nil || err2 != nil || !inside(ap, bp) {
		return fmt.Errorf("%w: %s is not inside %s", ErrOutsideSandbox, path, prefix)
	}
	rp, rb := resolve(ap), resolve(bp)
	if !inside(rp, rb) {
		return fmt.Errorf("%w: %s resolves to %s, not inside %s", ErrOutsideSandbox, path, rp, rb)
	}
	return nil
}

// resolve follows links in the longest existing prefix of p.
func resolve(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Executor performs a plan. Before each removal it checks that one file again - it still exists,
// is still a plain file in a plain camera folder directly under the root (or in a plain day folder
// of its own date directly in that camera folder), still passes the allowlist, still has the planned size and write time, and is still strictly older than its
// folder's protected newest - and skips it on any change (a recorder may be writing). It deletes
// only files of the plan, re-reads free space after each one and stops at the target.
type Executor struct {
	FS      FS
	Log     *slog.Logger // component "prune"; nil discards
	Actor   string       // logged as actor; default "supervisor"
	DryRun  bool         // return the plan, remove nothing
	Sandbox string       // when set (or SANDBOX_ONLY is), the only tree it may delete in
}

// Skip is a planned deletion the executor did not perform.
type Skip struct {
	Deletion
	Why string
}

// Result is a finished run: Decision holds what was actually done.
type Result struct {
	Decision
	Plan    Decision
	DryRun  bool
	Skipped []Skip
	Alerts  []Alert
	Episode Episode
	// DaysRemoved are the empty day folders the run removed (see Executor.Run).
	DaysRemoved []string
}

func (e *Executor) sandbox() string {
	if e.Sandbox != "" {
		return e.Sandbox
	}
	switch v := os.Getenv(SandboxEnv); v {
	case "", "0":
		return ""
	case "1":
		return os.TempDir()
	default:
		return v
	}
}

func (e *Executor) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (e *Executor) actor() string {
	if e.Actor != "" {
		return e.Actor
	}
	return "supervisor"
}

// Run plans one prune run from in and performs it, updating st. Alerts are returned, not sent.
// An error means nothing was attempted (a root outside the sandbox). After a real run that no gate
// refused it removes each listed day folder (decision D20) dated before today's local date that
// the run emptied or that was already empty when Collect listed it - only when it is still a plain
// folder of a plain camera folder directly under the root, and empty (never recursively): the
// mover may be filing into today's. A folder it cannot remove stays for the next run.
func (e *Executor) Run(in Inputs, st *State) (Result, error) {
	log := e.logger()
	prefix := e.sandbox()
	if err := CheckSandbox(in.Root, prefix); err != nil {
		log.Error("prune: refused to run", logx.Action("prune.run"), logx.Actor(e.actor()), logx.Target(in.Root), logx.Result(logx.ResultRefused), logx.Err(err))
		return Result{}, err
	}
	plan := Plan(in)
	log.Debug("prune: plan", logx.Action("prune.plan"), logx.Target(in.Root), slog.Int("cameras", len(in.Cameras)),
		slog.Uint64("free_bytes", in.FreeBytes), slog.Int("candidates", plan.Candidates), slog.Int("planned", len(plan.Deletions)),
		slog.Bool("blocked", plan.Blocked), slog.String("stop", plan.StopLine))

	var res Result
	if e.DryRun || plan.Refusal != nil {
		res = Result{Decision: plan, Plan: plan, DryRun: e.DryRun}
		res.Message = finalMessage(plan, e.DryRun)
	} else {
		act := &realActor{e: e, in: in, prefix: prefix, planned: map[string]bool{}, emptied: map[string]int{}}
		for _, d := range plan.Deletions {
			act.planned[d.Path+"|"+d.Reason] = true
		}
		d := run(in, act)
		d.Failed = act.failed
		d.Message = finalMessage(d, false)
		res = Result{Decision: d, Plan: plan, Skipped: act.skipped, DaysRemoved: act.removeEmptyDays()}
	}
	if st == nil {
		st = &State{}
	}
	res.Alerts, res.Episode = st.Settle(res.Decision, e.DryRun)
	e.report(res)
	return res, nil
}

func (e *Executor) report(res Result) {
	log := e.logger()
	attrs := func(result string, extra ...any) []any {
		return append([]any{logx.Action("prune.run"), logx.Actor(e.actor()), logx.Target(res.Root), logx.Result(result)}, extra...)
	}
	for _, m := range res.Messages {
		if strings.HasPrefix(m, "prune guard:") {
			log.Warn(m, attrs(logx.ResultRefused)...)
		} else {
			log.Debug(m, attrs(logx.ResultOK)...)
		}
	}
	if res.DryRun {
		log.Debug(res.StopLine, attrs(logx.ResultOK, slog.Bool("dry_run", true), slog.Int("would_delete", len(res.Deletions)))...)
		if res.Message != "" {
			log.Debug(res.Message, attrs(logx.ResultOK, slog.Bool("dry_run", true))...)
		}
		return
	}
	if res.Refusal != nil {
		switch {
		case res.Refusal.Gate != GateClock && res.Episode.RefusalStarted:
			log.Error(res.Message, attrs(logx.ResultRefused, slog.String("gate", res.Refusal.Gate))...)
		case res.Refusal.Gate == GateClock && res.Episode.ClockStarted:
			log.Warn(res.Message, attrs(logx.ResultRefused, slog.String("gate", res.Refusal.Gate))...)
		default:
			log.Debug(res.Message, attrs(logx.ResultRefused, slog.String("gate", res.Refusal.Gate))...)
		}
		return
	}
	if res.Episode.ClockCleared {
		log.Info("prune: the clock and the archive agree again, pruning resumes", attrs(logx.ResultOK)...)
	}
	if res.Episode.BreakerStarted {
		log.Error(heldLine(res.Breaker), attrs(logx.ResultRefused, slog.String("gate", "breaker"))...)
	}
	if res.Episode.BreakerReset {
		log.Info("prune: the age rule is within MaxAgePruneFraction again, the circuit breaker is reset", attrs(logx.ResultOK)...)
	}
	log.Info(res.StopLine, attrs(logx.ResultOK, slog.Int("deleted", len(res.Deletions)), slog.Int64("freed_bytes", res.FreedBytes),
		slog.Int("failed", res.Failed), slog.Int("skipped", len(res.Skipped)), slog.Bool("blocked", res.Blocked))...)
	switch {
	case res.Blocked:
		log.Error(res.Message, attrs(logx.ResultFail)...)
	case res.Message != "":
		log.Info(res.Message, attrs(logx.ResultOK)...)
	}
}

type realActor struct {
	e       *Executor
	in      Inputs
	prefix  string
	planned map[string]bool
	failed  int
	skipped []Skip
	freeErr bool
	emptied map[string]int // deletions per day folder path
}

func (a *realActor) free() float64 {
	b, err := a.e.FS.FreeBytes(a.in.Root)
	if err != nil {
		// Unknown free space must not delete by the watermark: report it as plenty.
		if !a.freeErr {
			a.e.logger().Error("prune: cannot read free space; the watermark is skipped", logx.Action("prune.run"), logx.Target(a.in.Root), logx.Result(logx.ResultFail), logx.Err(err))
			a.freeErr = true
		}
		return math.Inf(1)
	}
	return roundGB(float64(b))
}

func (a *realActor) remove(s *seg, reason, detail string) (bool, bool) {
	key := s.path + "|" + reason
	if !a.planned[key] {
		return false, true
	}
	delete(a.planned, key)
	log := a.e.logger()
	d := Deletion{Path: s.path, Camera: s.folder, Name: s.f.Name, Bytes: s.f.Size, MTime: s.f.MTime, Reason: reason, Detail: detail, Guard: s.guard.path, GuardMTime: s.guard.f.MTime}
	why := ""
	if err := CheckSandbox(s.path, a.prefix); err != nil {
		why = err.Error()
	} else {
		why = a.revalidate(s)
	}
	if why != "" {
		a.skipped = append(a.skipped, Skip{Deletion: d, Why: why})
		log.Warn("prune: skipped "+s.path+": "+why, logx.Action("prune.delete"), logx.Actor(a.e.actor()), logx.Target(s.path), logx.Result(logx.ResultRefused), slog.String("reason", reason))
		return false, false
	}
	if err := a.e.FS.Remove(s.path); err != nil {
		a.failed++
		log.Error("prune: FAILED to delete "+s.path, logx.Action("prune.delete"), logx.Actor(a.e.actor()), logx.Target(s.path), logx.Result(logx.ResultFail), slog.String("reason", reason), logx.Err(err))
		return false, false
	}
	if s.f.Day != "" {
		a.emptied[filepath.Join(s.dir, s.f.Day)]++
	}
	log.Info("prune: deleted "+s.path, logx.Action("prune.delete"), logx.Actor(a.e.actor()), logx.Target(s.path), logx.Result(logx.ResultOK),
		slog.String("file", s.f.Name), slog.Int64("bytes", s.f.Size), slog.String("reason", reason), slog.String("detail", detail), slog.Time("written", s.f.MTime))
	return true, false
}

// revalidate re-applies every per-file rule to s just before it is removed; "" means it may go.
func (a *realActor) revalidate(s *seg) string {
	fsys, in := a.e.FS, a.in
	fi, err := fsys.Stat(s.path)
	switch {
	case err != nil:
		return "it is gone"
	case fi.Reparse:
		return "it became a link or reparse point"
	case fi.Dir || !fi.Regular:
		return "it is not a plain file"
	case fi.Size != s.f.Size:
		return fmt.Sprintf("its size changed from %d to %d bytes (a recorder may be writing)", s.f.Size, fi.Size)
	case !fi.MTime.Equal(s.f.MTime):
		return "it was modified after the plan (a recorder may be writing)"
	}
	di, err := fsys.Stat(s.dir)
	if err != nil || !di.Dir || di.Reparse {
		return "its folder is gone or became a link"
	}
	if s.f.Day != "" {
		if di, err := fsys.Stat(filepath.Join(s.dir, s.f.Day)); err != nil || !di.Dir || di.Reparse {
			return "its day folder is gone or became a link"
		}
	}
	if !in.same(filepath.Dir(filepath.Clean(s.dir)), filepath.Clean(in.Root)) || strings.HasPrefix(s.folder, "_") ||
		!footageOf(in.layout().Classify(s.path), s.folder, s.f.Day) {
		return "it fails the allowlist"
	}
	gi, err := fsys.Stat(s.guard.path)
	if err != nil || gi.Reparse || !gi.Regular {
		return "its folder's protected newest segment " + s.guard.f.Name + " is gone"
	}
	if !s.f.MTime.Before(gi.MTime) {
		return "it is no longer strictly older than its folder's newest segment " + s.guard.f.Name
	}
	return ""
}

// removeEmptyDays removes the day folders Run describes and returns them.
func (a *realActor) removeEmptyDays() []string {
	log := a.e.logger()
	today := a.in.Now.In(time.Local).Format("2006-01-02")
	var out []string
	for _, cam := range a.in.Cameras {
		listed := map[string]int{}
		for _, f := range cam.Files {
			listed[f.Day]++
		}
		for _, day := range cam.Days {
			dir := filepath.Join(cam.Dir, day)
			if day >= today || listed[day] != a.emptied[dir] {
				continue // today's or later, or something in it was not deleted
			}
			attrs := []any{logx.Action("prune.rmdir"), logx.Actor(a.e.actor()), logx.Target(dir)}
			if why := a.revalidateDay(cam, day); why != "" {
				log.Warn("prune: kept the day folder "+dir+": "+why, append(attrs, logx.Result(logx.ResultRefused))...)
				continue
			}
			if err := a.e.FS.RemoveDir(dir); err != nil {
				if errors.Is(err, ErrDirNotEmpty) {
					log.Debug("prune: kept the day folder "+dir+": it is not empty", append(attrs, logx.Result(logx.ResultOK))...)
				} else {
					log.Warn("prune: could not remove the empty day folder "+dir+"; the next run tries again", append(attrs, logx.Result(logx.ResultFail), logx.Err(err))...)
				}
				continue
			}
			log.Info("prune: removed the empty day folder "+dir, append(attrs, logx.Result(logx.ResultOK))...)
			out = append(out, dir)
		}
	}
	return out
}

// revalidateDay re-checks a day folder just before it is removed; "" means it may go.
func (a *realActor) revalidateDay(cam Camera, day string) string {
	fsys, in := a.e.FS, a.in
	dir := filepath.Join(cam.Dir, day)
	if err := CheckSandbox(dir, a.prefix); err != nil {
		return err.Error()
	}
	if cam.Reparse || strings.HasPrefix(filepath.Base(cam.Dir), "_") || !archive.IsDayName(day) ||
		!in.same(filepath.Dir(filepath.Clean(cam.Dir)), filepath.Clean(in.Root)) {
		return "it fails the allowlist"
	}
	if ci, err := fsys.Stat(cam.Dir); err != nil || !ci.Dir || ci.Reparse {
		return "its camera folder is gone or became a link"
	}
	if di, err := fsys.Stat(dir); err != nil || !di.Dir || di.Reparse {
		return "it is gone or became a link"
	}
	return ""
}
