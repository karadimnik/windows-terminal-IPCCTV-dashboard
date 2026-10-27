package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"holocron/internal/app"
	"holocron/internal/archive"
	"holocron/internal/logx"
	"holocron/internal/osx"
	"holocron/internal/prune"
)

// `archive migrate` and `archive flatten` (decision D20, task L4b): the Operator's commands over the
// mover library. migrate files the flat history into day folders (archive.FileDays, the rules of the
// supervisor's mover); flatten puts every filed segment back into its camera folder
// (archive.UnfileDays), the layout the PowerShell system reads. Both are dry runs unless --now. Both
// only rename on the one volume and never replace a name, so a run stopped anywhere leaves every
// file whole in one place or the other, and running it again carries on.

// layoutSchema versions the --json document of both commands.
const layoutSchema = 1

type layoutIssue struct {
	Camera string `json:"camera"`
	Path   string `json:"path"`
	Why    string `json:"why"`
}

type layoutMove struct {
	Camera string `json:"camera"`
	From   string `json:"from"`
	To     string `json:"to"`
	Bytes  int64  `json:"bytes"`
}

type layoutCamera struct {
	Camera string `json:"camera"`
	Moves  int    `json:"moves"`
	Bytes  int64  `json:"bytes"`
	// Days: migrate, the day folders the moves go into; flatten, those they come out of.
	Days        []string `json:"days"`
	NewDays     []string `json:"newDays"`     // migrate: of Days, those that did not exist before the run
	RemovedDays []string `json:"removedDays"` // flatten: day folders removed because the run emptied them
	InSpool     int      `json:"inSpool"`     // segments of the camera in its camera folder after the run
	Other       int      `json:"otherFiles"`  // other entries of the camera folder (not its segments): never moved
}

// layoutReport is one run of either command; a dry run reports what --now would do.
type layoutReport struct {
	Schema        int            `json:"schema"`
	Command       string         `json:"command"`
	Root          string         `json:"root"`
	DryRun        bool           `json:"dryRun"`
	Refused       string         `json:"refused,omitempty"`
	Fix           string         `json:"fix,omitempty"`
	MarginSeconds float64        `json:"marginSeconds,omitempty"`
	Cycles        int            `json:"cycles"`
	Moved         int            `json:"moved"`
	Bytes         int64          `json:"bytes"`
	Cameras       []layoutCamera `json:"cameras"`
	Moves         []layoutMove   `json:"moves"`
	Conflicts     []layoutIssue  `json:"conflicts"`
	Errors        []layoutIssue  `json:"errors"`
	Busy          []layoutIssue  `json:"busy"`
	Skipped       []layoutIssue  `json:"skipped"`
	Left          []layoutIssue  `json:"left"`
	Deferred      int            `json:"deferred"`
	Complete      bool           `json:"complete"`
	Note          string         `json:"note,omitempty"`
	Next          string         `json:"next"`
}

func newLayoutReport(command, root string, dry bool) *layoutReport {
	return &layoutReport{Schema: layoutSchema, Command: command, Root: root, DryRun: dry, Cameras: []layoutCamera{}, Moves: []layoutMove{},
		Conflicts: []layoutIssue{}, Errors: []layoutIssue{}, Busy: []layoutIssue{}, Skipped: []layoutIssue{}, Left: []layoutIssue{}}
}

// archiveLayout is the Layout of this invocation's root.
func archiveLayout(e *env) archive.Layout {
	container := "." + strings.TrimPrefix(prune.RulesFrom(e.cfg).Container, ".")
	return archive.Layout{Root: e.paths.Root, Matcher: archive.MatcherFor(e.opts.Platform), Container: container}
}

// layoutCameras are the camera folders a run covers: the one --camera names (a camera of
// cameras.json or a camera folder of the root), else every camera folder (links and junctions are
// never entered).
func layoutCameras(e *env, l archive.Layout, name string) ([]string, error) {
	if name != "" && (name != filepath.Base(name) || strings.ContainsAny(name, `/\:`) || name == "." || name == ".." || strings.HasPrefix(name, "_")) {
		return nil, usageErr("--camera %q: give a camera name or the name of a camera folder, not a path", name)
	}
	folders, err := l.Cameras()
	if err != nil {
		return nil, failed("cannot read the archive root %s: %v", l.Root, err)
	}
	var plain []string
	for _, f := range folders {
		if !f.Link {
			plain = append(plain, f.Name)
		}
	}
	if name == "" {
		return plain, nil
	}
	want := name
	if cams, err := app.LoadCameras(e.paths.Cameras); err == nil {
		for _, c := range cams.List {
			if safe, err := c.SafeName(); err == nil && (strings.EqualFold(c.Name, name) || strings.EqualFold(safe, name)) {
				want = safe
				break
			}
		}
	}
	for _, f := range plain {
		if f == want {
			return []string{f}, nil
		}
	}
	if l.Matcher.FoldCase {
		for _, f := range plain {
			if strings.EqualFold(f, want) {
				return []string{f}, nil
			}
		}
	}
	none := "none"
	if len(plain) > 0 {
		none = strings.Join(plain, ", ")
	}
	return nil, usageErr("--camera %q: no camera folder of that name in %s (camera folders: %s)", name, l.Root, none)
}

// recorderRunning reports whether a Holocron recorder runs on this root, by the signals `status`
// and `prune` read: the supervisor's status file (app.Running), its control endpoint, and a live
// host process (which would start the supervisor again).
func recorderRunning(ctx context.Context, e *env) (bool, string) {
	if doc, ok, _ := app.Running(e.paths, e.cfg, time.Now()); ok {
		return true, fmt.Sprintf("the supervisor (pid %d) reports running", doc.SupervisorPid)
	}
	if supervisorAnswers(ctx, e) {
		return true, "a supervisor answers on the control endpoint"
	}
	if h, ok := app.ReadHostRecord(e.paths); ok {
		if alive, _ := osx.Verify(h, e.inspector, goos()); alive {
			return true, fmt.Sprintf("the host process (pid %d) is running and would start the supervisor again", h.Pid)
		}
	}
	return false, ""
}

// spoolState is a camera folder as one listing shows it.
type spoolState struct {
	days        map[string]bool
	segs, other int
}

func spoolOf(l archive.Layout, cam string) spoolState {
	s := spoolState{days: map[string]bool{}}
	files, days, err := l.Spool(cam)
	if err != nil {
		return s
	}
	for _, d := range days {
		s.days[d.Name()] = true
	}
	for _, f := range files {
		if f.Type().IsRegular() && l.Classify(f.Path).Kind == archive.Spooled {
			s.segs++
		} else {
			s.other++
		}
	}
	return s
}

func spoolsOf(l archive.Layout, cams []string) map[string]spoolState {
	out := map[string]spoolState{}
	for _, c := range cams {
		out[c] = spoolOf(l, c)
	}
	return out
}

// tally folds the runs of one command into a report: moves accumulate, and for every other file the
// latest outcome stands (a file that was busy in one cycle and moved in a later one is a move).
type tally struct {
	rep     *layoutReport
	pending map[string]pendingIssue
	removed []string // day folders UnfileDays removed
}

type pendingIssue struct {
	list  *[]layoutIssue
	issue layoutIssue
}

func newTally(rep *layoutReport) *tally { return &tally{rep: rep, pending: map[string]pendingIssue{}} }

func (t *tally) add(r archive.MoveReport) {
	for _, m := range r.Moved {
		delete(t.pending, m.From)
		t.rep.Moves = append(t.rep.Moves, layoutMove{Camera: m.Camera, From: m.From, To: m.To, Bytes: m.Bytes})
		t.rep.Moved++
		t.rep.Bytes += m.Bytes
	}
	for _, g := range []struct {
		to   *[]layoutIssue
		from []archive.MoveSkip
	}{{&t.rep.Conflicts, r.Conflicts}, {&t.rep.Errors, r.Errors}, {&t.rep.Busy, r.Busy}, {&t.rep.Skipped, r.Skipped}, {&t.rep.Left, r.Left}} {
		for _, s := range g.from {
			t.pending[s.Path] = pendingIssue{g.to, layoutIssue{Camera: s.Camera, Path: s.Path, Why: s.Why}}
		}
	}
	t.rep.Deferred = r.Deferred
	t.removed = append(t.removed, r.DaysRemoved...)
}

// finish files the outcomes that stand, in path order, and sets Complete: nothing that should
// move was left behind (refused, a conflict, an error, a busy or changed file, a deferred one).
func (t *tally) finish() {
	paths := make([]string, 0, len(t.pending))
	for p := range t.pending {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		x := t.pending[p]
		*x.list = append(*x.list, x.issue)
	}
	r := t.rep
	r.Complete = r.Refused == "" && len(r.Conflicts)+len(r.Errors)+len(r.Busy)+len(r.Skipped) == 0 && r.Deferred == 0
}

// predictConflicts turns the planned moves of a dry run whose destination already exists into the
// conflicts --now would meet (the rename never replaces).
func predictConflicts(r *archive.MoveReport) {
	var kept []archive.Move
	var n int64
	for _, m := range r.Moved {
		if _, err := os.Lstat(m.To); err == nil {
			r.Conflicts = append(r.Conflicts, archive.MoveSkip{Camera: m.Camera, Path: m.From, Why: "the destination " + m.To + " already exists; --now leaves both files as they are"})
			continue
		}
		kept = append(kept, m)
		n += m.Bytes
	}
	r.Moved, r.Bytes = kept, n
}

// summarize fills the per-camera lines from the moves, the issues and the camera folders: before is
// each camera folder as listed before the run (a dry run reads nothing after it).
func (t *tally) summarize(l archive.Layout, cams []string, before map[string]spoolState, migrate bool) {
	r := t.rep
	idx := map[string]int{}
	for i, c := range cams {
		r.Cameras = append(r.Cameras, layoutCamera{Camera: c, Days: []string{}, NewDays: []string{}, RemovedDays: []string{}})
		idx[c] = i
	}
	days := map[string]map[string]bool{}    // camera -> day folders of its moves
	emptied := map[string]map[string]bool{} // camera -> day folders a dry flatten would empty
	stays := map[string]bool{}              // folders where an entry stays
	for _, list := range [][]layoutIssue{r.Conflicts, r.Errors, r.Busy, r.Skipped, r.Left} {
		for _, x := range list {
			stays[filepath.Dir(x.Path)] = true
		}
	}
	for _, m := range r.Moves {
		i, ok := idx[m.Camera]
		if !ok {
			continue
		}
		c := &r.Cameras[i]
		c.Moves++
		c.Bytes += m.Bytes
		dayDir := filepath.Dir(m.To)
		if !migrate {
			dayDir = filepath.Dir(m.From)
		}
		if days[c.Camera] == nil {
			days[c.Camera], emptied[c.Camera] = map[string]bool{}, map[string]bool{}
		}
		days[c.Camera][filepath.Base(dayDir)] = true
		if !migrate && r.DryRun && !stays[dayDir] && r.Deferred == 0 {
			emptied[c.Camera][filepath.Base(dayDir)] = true
		}
	}
	for _, p := range t.removed {
		if i, ok := idx[filepath.Base(filepath.Dir(p))]; ok {
			r.Cameras[i].RemovedDays = append(r.Cameras[i].RemovedDays, filepath.Base(p))
		}
	}
	for i := range r.Cameras {
		c := &r.Cameras[i]
		for d := range days[c.Camera] {
			c.Days = append(c.Days, d)
			if migrate && !before[c.Camera].days[d] {
				c.NewDays = append(c.NewDays, d)
			}
		}
		for d := range emptied[c.Camera] {
			c.RemovedDays = append(c.RemovedDays, d)
		}
		sort.Strings(c.Days)
		sort.Strings(c.NewDays)
		sort.Strings(c.RemovedDays)
		st := before[c.Camera]
		switch {
		case !r.DryRun:
			st = spoolOf(l, c.Camera)
			c.InSpool = st.segs
		case migrate:
			c.InSpool = st.segs - c.Moves
		default:
			c.InSpool = st.segs + c.Moves
		}
		c.Other = st.other
	}
}

// logRun writes the levelled records of an applied run: one debug record per move, a warning per
// conflict or error, and an info summary (component prune, as the supervisor's mover).
func logRun(e *env, action string, r *layoutReport) {
	lg := e.lg.For("prune")
	for _, m := range r.Moves {
		lg.Debug("moved "+m.From+" to "+m.To, logx.Action(action), logx.Target(m.From), logx.Result(logx.ResultOK), slog.Int64("bytes", m.Bytes))
	}
	for _, x := range r.Conflicts {
		lg.Warn("not moved "+x.Path+": "+x.Why, logx.Action(action), logx.Target(x.Path), logx.Result(logx.ResultRefused), slog.String("reason", "conflict"))
	}
	for _, x := range r.Errors {
		lg.Warn("not moved "+x.Path+": "+x.Why, logx.Action(action), logx.Target(x.Path), logx.Result(logx.ResultFail))
	}
	res := logx.ResultOK
	if len(r.Conflicts)+len(r.Errors) > 0 {
		res = logx.ResultFail
	}
	lg.Info(fmt.Sprintf("%s: moved %d segment(s), %d byte(s), in %d cycle(s); %d conflict(s), %d error(s), %d busy, %d changed or gone, %d left in day folders",
		r.Command, r.Moved, r.Bytes, r.Cycles, len(r.Conflicts), len(r.Errors), len(r.Busy), len(r.Skipped), len(r.Left)),
		logx.Action(action), logx.Target(r.Root), logx.Result(res), slog.Int("moved", r.Moved), slog.Int64("bytes", r.Bytes))
}

// finishLayout prints the report and maps it to the exit code: 1 when refused or when a conflict or
// an error stands (dry run: would stand), 0 otherwise.
func finishLayout(e *env, r *layoutReport, print func(*env, *layoutReport)) error {
	if e.json {
		if err := printJSON(e.out, r); err != nil {
			return err
		}
	} else {
		print(e, r)
	}
	switch {
	case r.Refused != "":
		return quietFail("%s refused: %s", r.Command, r.Refused)
	case len(r.Conflicts)+len(r.Errors) > 0:
		return quietFail("%s: %d conflict(s), %d error(s)", r.Command, len(r.Conflicts), len(r.Errors))
	}
	return nil
}

// sizeText is a byte count for a reader: GB, MB under a tenth of a GB, bytes under a MB.
func sizeText(n int64) string {
	switch {
	case n >= gib/10:
		return fmt.Sprintf("%.2f GB", float64(n)/gib)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// ---------------------------------------------------------------------------
// archive migrate
// ---------------------------------------------------------------------------

func cmdArchiveMigrate(ctx context.Context, e *env) error {
	fs := newFlags(e, "archive migrate")
	now := fs.Bool("now", false, "file the segments (without it: a dry run that shows the plan and moves nothing)")
	camera := fs.String("camera", "", "only this camera (a camera of cameras.json, or a camera folder of the root)")
	margin := fs.Float64("margin", e.cfg.MoveMarginSeconds, "seconds: a segment written less than this long ago stays in the camera folder (at least 30)")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("archive migrate", pos, 0, 0); err != nil {
		return err
	}
	if *margin < 30 {
		return usageErr("holocron archive migrate: --margin %g: at least 30 seconds (a younger segment may still be written)", *margin)
	}
	if err := e.needConfig(); err != nil {
		return err
	}
	l := archiveLayout(e)
	r := newLayoutReport("archive migrate", e.paths.Root, !*now)
	r.MarginSeconds = *margin
	cams, err := layoutCameras(e, l, *camera)
	if err != nil {
		return err
	}
	before := spoolsOf(l, cams)
	o := archive.MoveOptions{Platform: e.opts.Platform, Now: time.Now(), Margin: time.Duration(*margin * float64(time.Second)), DryRun: true}
	plan := archive.FileDays(l, o, cams...) // the plan, and the sandbox and marker gates
	if plan.Refused != "" {
		r.Refused, r.Fix = plan.Refused, migrateFix(plan.Refused)
		r.Next = r.Fix
		return finishLayout(e, r, printMigrate)
	}
	t := newTally(r)
	if r.DryRun {
		predictConflicts(&plan)
		t.add(plan)
	} else {
		o.DryRun, o.Max = false, e.cfg.MoveMaxPerCycle
		for {
			o.Now = time.Now()
			rep := archive.FileDays(l, o, cams...)
			if rep.Refused != "" { // the marker went away mid-run
				r.Refused, r.Fix = rep.Refused, migrateFix(rep.Refused)
				break
			}
			r.Cycles++
			t.add(rep)
			retry := len(rep.Errors) + len(rep.Busy) + len(rep.Skipped)
			line := fmt.Sprintf("cycle %d: filed %d segment(s), %s (so far %d, %s)", r.Cycles, len(rep.Moved), sizeText(rep.Bytes), r.Moved, sizeText(r.Bytes))
			if rep.Deferred > 0 {
				line += fmt.Sprintf("; %d more eligible", rep.Deferred)
			}
			e.lg.For("prune").Info("archive migrate "+line, logx.Action("archive.file"), logx.Target(e.paths.Root), logx.Result(logx.ResultOK), slog.Int("moved", len(rep.Moved)))
			if !e.json {
				fmt.Fprintln(e.out, line)
			}
			// another cycle while this one made progress and left something it may still move
			if len(rep.Moved) == 0 || (rep.Deferred == 0 && retry == 0) {
				break
			}
		}
	}
	t.finish()
	t.summarize(l, cams, before, true)
	if !r.DryRun {
		logRun(e, "archive.file", r)
	}
	if !e.cfg.MoveToDayFolders {
		r.Note = "MoveToDayFolders is false: the recorder's mover is off, so new segments stay in the camera folders until it is true again"
	}
	r.Next = migrateNext(r)
	return finishLayout(e, r, printMigrate)
}

func migrateFix(refused string) string {
	if strings.Contains(refused, "not owned") {
		return "holocron up claims the root at its start (an ownership marker in _state); run the migration after that, or check that --root is the archive"
	}
	return "run it on the archive root this recorder owns (" + archive.SandboxEnv + " restricts the mover to a test folder)"
}

func migrateNext(r *layoutReport) string {
	switch {
	case len(r.Conflicts)+len(r.Errors) > 0:
		return "resolve the files listed above (a conflict: compare the two files and keep one), then run holocron archive migrate --now again"
	case r.DryRun && r.Moved == 0:
		return "nothing to file; holocron archive stats shows per camera what is filed and what is in the camera folder"
	case r.DryRun:
		return "holocron archive migrate --now applies this plan (safe while the recorder runs; stop it at any time and run it again)"
	case !r.Complete:
		return "run holocron archive migrate --now again later for the files above (the recorder's mover also files them), then holocron archive stats"
	}
	return "holocron archive stats shows per camera how many segments are filed and how many are in the camera folder"
}

func printMigrate(e *env, r *layoutReport) {
	w := e.out
	verb := "filed"
	if r.DryRun {
		verb = "would file"
		fmt.Fprintf(w, "archive migrate - DRY RUN, nothing is moved: %s\n", r.Root)
	} else {
		fmt.Fprintf(w, "archive migrate --now: %s\n", r.Root)
	}
	if r.Refused != "" {
		fmt.Fprintf(w, "REFUSED: %s\nnothing was moved\nfix: %s\n", r.Refused, r.Fix)
		return
	}
	fmt.Fprintf(w, "each closed segment in a camera folder goes into the day folder of the date in its name; the two newest segments of a camera and any written in the last %g s stay\n", r.MarginSeconds)
	for _, c := range r.Cameras {
		line := fmt.Sprintf("  %-20s nothing to file", c.Camera)
		if c.Moves > 0 {
			line = fmt.Sprintf("  %-20s %s %d segment(s), %s, into %d day folder(s) (%d new)", c.Camera, verb, c.Moves, sizeText(c.Bytes), len(c.Days), len(c.NewDays))
		}
		line += fmt.Sprintf("; %d segment(s) stay in the camera folder", c.InSpool)
		if c.Other > 0 {
			line += fmt.Sprintf("; %d other file(s) there are not segments of this camera and are left alone", c.Other)
		}
		fmt.Fprintln(w, line)
	}
	printIssues(w, r)
	if r.DryRun {
		fmt.Fprintf(w, "total: would file %d segment(s), %s\n", r.Moved, sizeText(r.Bytes))
	} else {
		state := "done, nothing more is eligible"
		if !r.Complete {
			state = "INCOMPLETE, see above"
		}
		fmt.Fprintf(w, "total: filed %d segment(s), %s, in %d cycle(s): %s\n", r.Moved, sizeText(r.Bytes), r.Cycles, state)
	}
	if r.Note != "" {
		fmt.Fprintln(w, "note:", r.Note)
	}
	fmt.Fprintln(w, "next:", r.Next)
}

// printIssues lists every file a run did not move, grouped by why, at most 20 per group.
func printIssues(w io.Writer, r *layoutReport) {
	would := ""
	if r.DryRun {
		would = "would "
	}
	for _, g := range []struct {
		title string
		list  []layoutIssue
	}{
		{"CONFLICT, both files stay as they are", r.Conflicts},
		{"ERROR, the file stays where it is", r.Errors},
		{"in use by another program, " + would + "stay where it is (the next run tries again)", r.Busy},
		{"changed or gone while listed, " + would + "stay where it is (the next run looks again)", r.Skipped},
		{"left in a day folder, never moved (not a filed segment of that camera); the PowerShell recorder does not see it", r.Left},
	} {
		if len(g.list) == 0 {
			continue
		}
		fmt.Fprintf(w, "%d file(s) %s:\n", len(g.list), g.title)
		for i, x := range g.list {
			if i == 20 {
				fmt.Fprintf(w, "  ... and %d more (--json lists them all)\n", len(g.list)-20)
				break
			}
			fmt.Fprintf(w, "  %s: %s\n", x.Path, x.Why)
		}
	}
	if r.Deferred > 0 {
		fmt.Fprintf(w, "%d eligible file(s) were not reached in this run\n", r.Deferred)
	}
}

// ---------------------------------------------------------------------------
// archive flatten
// ---------------------------------------------------------------------------

func cmdArchiveFlatten(ctx context.Context, e *env) error {
	fs := newFlags(e, "archive flatten")
	now := fs.Bool("now", false, "put the segments back (without it: a dry run that shows the plan and moves nothing)")
	camera := fs.String("camera", "", "only this camera (a camera of cameras.json, or a camera folder of the root)")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("archive flatten", pos, 0, 0); err != nil {
		return err
	}
	if err := e.needConfig(); err != nil {
		return err
	}
	l := archiveLayout(e)
	r := newLayoutReport("archive flatten", e.paths.Root, !*now)
	running, why := recorderRunning(ctx, e)
	o := archive.MoveOptions{Platform: e.opts.Platform, Now: time.Now(), DryRun: !*now, RecorderRunning: running}
	if running { // refused before anything is read: the library refuses the same way
		rep := archive.UnfileDays(l, o)
		r.Refused, r.Fix = rep.Refused+" ("+why+")", "holocron down"
		r.Next = "holocron down, then holocron archive flatten again"
		return finishLayout(e, r, printFlatten)
	}
	cams, err := layoutCameras(e, l, *camera)
	if err != nil {
		return err
	}
	before := spoolsOf(l, cams)
	rep := archive.UnfileDays(l, o, cams...)
	if rep.Refused != "" {
		r.Refused, r.Fix = rep.Refused, migrateFix(rep.Refused)
		r.Next = r.Fix
		return finishLayout(e, r, printFlatten)
	}
	if r.DryRun {
		predictConflicts(&rep)
	} else {
		r.Cycles = 1
	}
	t := newTally(r)
	t.add(rep)
	t.finish()
	t.summarize(l, cams, before, false)
	if !r.DryRun {
		logRun(e, "archive.unfile", r)
	}
	switch {
	case len(r.Conflicts)+len(r.Errors) > 0:
		r.Next = "resolve the files listed above (a conflict: the camera folder already holds that name; compare the two files and keep one), then run holocron archive flatten --now again"
	case r.DryRun && r.Moved == 0:
		r.Next = "nothing is filed: the archive is flat already; holocron archive stats shows it"
	case r.DryRun:
		r.Next = "holocron archive flatten --now applies this plan (the recorder must stay stopped until it is done)"
	case !r.Complete:
		r.Next = "run holocron archive flatten --now again for the files above, then holocron archive stats"
	default:
		r.Next = "holocron archive stats should show 0 filed for every camera; then go on with the rollback (docs/rollback.md). holocron up files the segments again unless MoveToDayFolders is false"
	}
	return finishLayout(e, r, printFlatten)
}

func printFlatten(e *env, r *layoutReport) {
	w := e.out
	verb, removed := "put back", "removed"
	if r.DryRun {
		verb, removed = "would put back", "would be removed"
		fmt.Fprintf(w, "archive flatten - DRY RUN, nothing is moved: %s\n", r.Root)
	} else {
		fmt.Fprintf(w, "archive flatten --now: %s\n", r.Root)
	}
	if r.Refused != "" {
		fmt.Fprintf(w, "REFUSED: %s\nnothing was moved\nfix: %s\n", r.Refused, r.Fix)
		return
	}
	fmt.Fprintln(w, "every filed segment goes back into its camera folder (the flat layout the PowerShell recorder reads), and each day folder this empties is removed")
	for _, c := range r.Cameras {
		line := fmt.Sprintf("  %-20s nothing filed", c.Camera)
		if c.Moves > 0 {
			line = fmt.Sprintf("  %-20s %s %d segment(s), %s, from %d day folder(s); %d day folder(s) %s", c.Camera, verb, c.Moves, sizeText(c.Bytes), len(c.Days), len(c.RemovedDays), removed)
		}
		fmt.Fprintf(w, "%s; %d segment(s) in the camera folder after\n", line, c.InSpool)
	}
	printIssues(w, r)
	if r.DryRun {
		fmt.Fprintf(w, "total: would put back %d segment(s), %s\n", r.Moved, sizeText(r.Bytes))
	} else {
		state := "done, the archive is flat"
		if !r.Complete {
			state = "INCOMPLETE, see above"
		} else if len(r.Left) > 0 {
			state = "done; the files left in day folders above are not footage and the PowerShell recorder ignores them"
		}
		fmt.Fprintf(w, "total: put back %d segment(s), %s: %s\n", r.Moved, sizeText(r.Bytes), state)
	}
	fmt.Fprintln(w, "next:", r.Next)
}
