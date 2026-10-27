package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"holocron/internal/app"
	"holocron/internal/archive"
	"holocron/internal/cli"
	"holocron/internal/prune"
)

const gib = 1 << 30

// supervisorAnswers reports whether a supervisor answers on the control endpoint of this root.
func supervisorAnswers(ctx context.Context, e *env) bool {
	c, err := e.dial(e.paths.Root)
	if err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err = c.State(cctx)
	return err == nil
}

type pruneDeletion struct {
	Path   string    `json:"path"`
	Camera string    `json:"camera"`
	Bytes  int64     `json:"bytes"`
	Reason string    `json:"reason"`
	Detail string    `json:"detail,omitempty"`
	MTime  time.Time `json:"written"`
}

type pruneReport struct {
	Root       string          `json:"root"`
	DryRun     bool            `json:"dryRun"`
	Refused    string          `json:"refused,omitempty"`
	Gate       string          `json:"gate,omitempty"`
	Blocked    bool            `json:"blocked"`
	FreeGB     float64         `json:"freeGB"`
	FreeAfter  float64         `json:"freeGBAfter"`
	ReserveGB  float64         `json:"reserveGB"`
	TargetGB   float64         `json:"targetGB"`
	AgeRule    bool            `json:"ageRule"`
	Watermark  bool            `json:"belowReserve"`
	Held       string          `json:"breakerHeld,omitempty"`
	Deletions  []pruneDeletion `json:"deletions"`
	FreedBytes int64           `json:"freedBytes"`
	Skipped    []string        `json:"skipped,omitempty"`
	Failed     int             `json:"failed"`
	Protected  int             `json:"protected"`
	Foreign    int             `json:"foreignFolders"`
	Messages   []string        `json:"messages,omitempty"`
	StopLine   string          `json:"stopLine"`
	Note       string          `json:"note,omitempty"`
}

func cmdPrune(ctx context.Context, e *env) error {
	fs := newFlags(e, "prune")
	dry := fs.Bool("dry-run", false, "show the decision, delete nothing (the default)")
	now := fs.Bool("now", false, "run the pruner now: delete what the decision says (needs --yes)")
	yes := fs.Bool("yes", false, "confirm --now")
	limit := fs.Int("list", 20, "show at most this many deletions in the text output (0: all)")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("prune", pos, 0, 0); err != nil {
		return err
	}
	if *dry && *now {
		return usageErr("holocron prune: --dry-run and --now exclude each other")
	}
	if *now && !*yes {
		return usageErr("holocron prune --now deletes recordings; add --yes to confirm (holocron prune alone shows the decision)")
	}
	if err := e.needConfig(); err != nil {
		return err
	}
	if *now && supervisorAnswers(ctx, e) {
		return cli.Refuse("a supervisor is running for %s and prunes on its own schedule (every PruneIntervalSeconds); stop it first, or let it do its work", e.paths.Root)
	}
	rep, res, err := pruneDecide(ctx, e, *now)
	if err != nil {
		return err
	}
	if e.json {
		if err := printJSON(e.out, rep); err != nil {
			return err
		}
	} else {
		printPrune(e, rep, *limit)
	}
	switch {
	case res.Refusal != nil:
		return quietFail("prune refused (%s): %s", res.Refusal.Gate, res.Refusal.Reason)
	case res.Blocked:
		return quietFail("prune blocked: %s", res.Message)
	case res.Failed > 0:
		return quietFail("prune: %d deletion(s) failed", res.Failed)
	}
	return nil
}

// pruneDecide runs the pruner's planner over the archive: a dry run deletes nothing (what
// `holocron prune` and the Admin tab's preview show), now deletes what it decides.
func pruneDecide(ctx context.Context, e *env, now bool) (pruneReport, prune.Result, error) {
	rules := prune.RulesFrom(e.cfg)
	in, err := prune.Collect(e.paths.Root, e.opts.Platform, rules, time.Now(), !now)
	if err != nil {
		return pruneReport{}, prune.Result{}, failed("cannot read the archive: %v", err)
	}
	ex := prune.Executor{FS: prune.OSFS{Platform: e.opts.Platform}, Log: e.lg.For("prune"), Actor: cli.ActorFrom(ctx), DryRun: !now}
	res, err := ex.Run(in, &prune.State{})
	if err != nil {
		return pruneReport{}, res, cli.Refuse("%v", err)
	}
	rep := pruneReportOf(e, res, !now)
	if !now {
		if ok, why := archive.MarkerValid(e.paths.Root, e.opts.Platform); !ok && res.Refusal == nil {
			rep.Note = "the root is not claimed yet (" + why + "): the supervisor claims it at start, and --now refuses until then"
		}
	}
	return rep, res, nil
}

func pruneReportOf(e *env, res prune.Result, dry bool) pruneReport {
	d := res.Decision
	rep := pruneReport{Root: d.Root, DryRun: dry, Blocked: d.Blocked, FreeGB: d.FreeGBBefore, FreeAfter: d.FreeGBAfter, ReserveGB: d.ReserveGB,
		TargetGB: d.TargetGB, AgeRule: d.AgeRule, Watermark: d.Watermark, Held: d.Breaker, Deletions: []pruneDeletion{}, FreedBytes: d.FreedBytes,
		Failed: d.Failed, Protected: len(d.Protected), Foreign: len(d.Foreign), Messages: d.Messages, StopLine: d.StopLine}
	if d.Refusal != nil {
		rep.Refused, rep.Gate = d.Refusal.Reason, d.Refusal.Gate
	}
	for _, x := range d.Deletions {
		rep.Deletions = append(rep.Deletions, pruneDeletion{Path: x.Path, Camera: x.Camera, Bytes: x.Bytes, Reason: x.Reason, Detail: x.Detail, MTime: x.MTime.UTC()})
	}
	for _, s := range res.Skipped {
		rep.Skipped = append(rep.Skipped, s.Path+": "+s.Why)
	}
	return rep
}

func printPrune(e *env, r pruneReport, limit int) {
	mode := "would delete"
	title := "prune (dry run: nothing is deleted)"
	if !r.DryRun {
		mode, title = "deleted", "prune (run now)"
	}
	fmt.Fprintf(e.out, "%s: %s\n", title, r.Root)
	if r.Refused != "" {
		fmt.Fprintf(e.out, "REFUSED at the %s gate: %s\nnothing is deleted; with this the disk is not pruned\n", r.Gate, r.Refused)
		return
	}
	fmt.Fprintf(e.out, "free %.2f GB, reserve %.2f GB, target %.2f GB; age rule %s; below the reserve: %v\n", r.FreeGB, r.ReserveGB, r.TargetGB, map[bool]string{true: "on", false: "off"}[r.AgeRule], r.Watermark)
	if r.Held != "" {
		fmt.Fprintf(e.out, "age rule HELD by the circuit breaker: %s\n", r.Held)
	}
	fmt.Fprintf(e.out, "%s %d file(s), %.2f GB; free after %.2f GB\n", mode, len(r.Deletions), float64(r.FreedBytes)/gib, r.FreeAfter)
	for i, d := range r.Deletions {
		if limit > 0 && i >= limit {
			fmt.Fprintf(e.out, "  ... and %d more (--list 0 shows all, --json has them)\n", len(r.Deletions)-limit)
			break
		}
		fmt.Fprintf(e.out, "  %s  %.3f GB  %s  %s\n", d.Path, float64(d.Bytes)/gib, d.Reason, d.Detail)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(e.out, "skipped %s\n", s)
	}
	fmt.Fprintf(e.out, "%d newest segment(s) protected; %d folder(s) with files that are not segments are left alone\n", r.Protected, r.Foreign)
	for _, m := range r.Messages {
		fmt.Fprintln(e.out, " ", m)
	}
	if r.Blocked {
		fmt.Fprintln(e.out, "BLOCKED: below the reserve with nothing deletable left")
	}
	if r.Note != "" {
		fmt.Fprintln(e.out, "note:", r.Note)
	}
	fmt.Fprintln(e.out, r.StopLine)
}

// ---------------------------------------------------------------------------
// archive stats
// ---------------------------------------------------------------------------

type camStats struct {
	Folder   string     `json:"folder"`
	Camera   string     `json:"camera,omitempty"`
	Segments int        `json:"segments"`
	Bytes    int64      `json:"bytes"`
	Oldest   *time.Time `json:"oldest,omitempty"`
	Newest   *time.Time `json:"newest,omitempty"`
}

type archiveStats struct {
	Root          string     `json:"root"`
	Cameras       []camStats `json:"cameras"`
	Segments      int        `json:"segments"`
	Bytes         int64      `json:"bytes"`
	FreeGB        float64    `json:"freeGB"`
	ReserveGB     float64    `json:"reserveGB"`
	RetentionDays *float64   `json:"maxRetentionDays"`
	GBPerDay      *float64   `json:"measuredGBPerDay"`
	ProjectedDays *float64   `json:"projectedDaysRemaining"`
	Note          string     `json:"note,omitempty"`
}

func collectStats(e *env) (archiveStats, error) {
	st := archiveStats{Root: e.paths.Root, Cameras: []camStats{}, ReserveGB: e.cfg.ReserveGB, RetentionDays: e.cfg.MaxRetentionDays}
	container := "." + strings.TrimPrefix(prune.RulesFrom(e.cfg).Container, ".")
	m := archive.MatcherFor(e.opts.Platform)
	known := map[string]string{}
	if cams, err := app.LoadCameras(e.paths.Cameras); err == nil {
		for _, c := range cams.List {
			if safe, err := c.SafeName(); err == nil {
				known[strings.ToLower(safe)] = c.Name
			}
		}
	}
	l := archive.Layout{Root: e.paths.Root, Matcher: m, Container: container}
	entries, err := l.Folders()
	if err != nil {
		return st, fmt.Errorf("cannot read the archive root: %w", err)
	}
	var oldest, newest time.Time
	for _, entry := range entries { // the recorder's own "_" folders are not listed
		if !entry.IsDir() {
			continue
		}
		cs := camStats{Folder: entry.Name(), Camera: known[strings.ToLower(entry.Name())]}
		files, err := l.Files(entry.Name())
		if err != nil {
			continue
		}
		var lo, hi time.Time
		for _, f := range files {
			if !m.IsSegmentOf(f.Name(), entry.Name(), container) {
				continue
			}
			_, start, ok := archive.ParseSegment(f.Name())
			if !ok {
				continue
			}
			cs.Segments++
			cs.Bytes += f.Info.Size()
			if lo.IsZero() || start.Before(lo) {
				lo = start
			}
			if hi.IsZero() || start.After(hi) {
				hi = start
			}
		}
		if cs.Segments > 0 {
			lo, hi := lo, hi
			cs.Oldest, cs.Newest = &lo, &hi
			if oldest.IsZero() || lo.Before(oldest) {
				oldest = lo
			}
			if newest.IsZero() || hi.After(newest) {
				newest = hi
			}
		}
		st.Segments += cs.Segments
		st.Bytes += cs.Bytes
		st.Cameras = append(st.Cameras, cs)
	}
	sort.Slice(st.Cameras, func(i, j int) bool { return st.Cameras[i].Folder < st.Cameras[j].Folder })
	if free, err := e.opts.Platform.DiskFree(e.paths.Root); err == nil {
		st.FreeGB = math.Round(float64(free)/gib*100) / 100
	}
	// the same arithmetic as the status document: needs 10 minutes of history
	if span := newest.Sub(oldest).Seconds(); !oldest.IsZero() && span >= 600 {
		rate := math.Round(float64(st.Bytes)/gib/(span/86400)*100) / 100
		st.GBPerDay = &rate
		if rate > 0 {
			days := math.Round(math.Max(st.FreeGB-st.ReserveGB, 0)/rate*100) / 100
			st.ProjectedDays = &days
		}
	} else {
		st.Note = "less than 10 minutes of recordings: no rate to project from yet"
	}
	return st, nil
}

func cmdArchiveStats(ctx context.Context, e *env) error {
	pos, err := parseFlags(newFlags(e, "archive stats"), e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("archive stats", pos, 0, 0); err != nil {
		return err
	}
	if err := e.needConfig(); err != nil {
		return err
	}
	st, err := collectStats(e)
	if err != nil {
		return failed("%v", err)
	}
	if e.json {
		return printJSON(e.out, st)
	}
	fmt.Fprintf(e.out, "archive %s\n", st.Root)
	fmt.Fprintf(e.out, "%-22s %9s %10s  %-19s  %s\n", "FOLDER", "SEGMENTS", "GB", "OLDEST", "NEWEST")
	for _, c := range st.Cameras {
		oldest, newest := "-", "-"
		if c.Oldest != nil {
			oldest, newest = c.Oldest.Format("2006-01-02 15:04:05"), c.Newest.Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(e.out, "%-22s %9d %10.2f  %-19s  %s\n", c.Folder, c.Segments, float64(c.Bytes)/gib, oldest, newest)
	}
	fmt.Fprintf(e.out, "total %d segment(s), %.2f GB; free %.2f GB, reserve %.2f GB", st.Segments, float64(st.Bytes)/gib, st.FreeGB, st.ReserveGB)
	if st.RetentionDays != nil {
		fmt.Fprintf(e.out, ", retention %g days", *st.RetentionDays)
	}
	fmt.Fprintln(e.out)
	if st.GBPerDay != nil {
		line := fmt.Sprintf("measured %.2f GB/day", *st.GBPerDay)
		if st.ProjectedDays != nil {
			line += fmt.Sprintf(", about %.1f days until the reserve", *st.ProjectedDays)
		}
		fmt.Fprintln(e.out, line)
	} else if st.Note != "" {
		fmt.Fprintln(e.out, st.Note)
	}
	return nil
}
