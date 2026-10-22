package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"holocron/internal/app"
	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/ffmpeg"
	"holocron/internal/osx"
	"holocron/internal/shadow"
	"holocron/internal/status"
)

// Levels of a doctor line. info never counts: it is context, not a problem.
const (
	lvOK   = "ok"
	lvInfo = "info"
	lvWarn = "warn"
	lvFail = "fail"
)

// doctorGroups in output order. The runtime groups need a running supervisor.
var doctorGroups = []string{"setup", "recording", "connections", "live", "web", "storage", "system"}

var runtimeGroups = map[string]bool{"recording": true, "connections": true, "live": true, "web": true, "storage": true}

// check is one doctor line (and one entry of `doctor --json`).
type check struct {
	ID     string `json:"id"`
	Group  string `json:"group"`
	Level  string `json:"level"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	Data   any    `json:"data,omitempty"`
}

func mk(level, title, format string, a ...any) check {
	return check{Level: level, Title: title, Detail: fmt.Sprintf(format, a...)}
}

func (c check) fix(format string, a ...any) check { c.Fix = fmt.Sprintf(format, a...); return c }
func (c check) data(d any) check                  { c.Data = d; return c }

// doctorCheck is one row of the check table. A concurrent check (a network probe, or one that
// waits) runs in the bounded pool inside the network budget; the others run one after another.
type doctorCheck struct {
	id, group  string
	concurrent bool
	timeout    time.Duration // 0: doctorDeps.checkTimeout
	run        func(ctx context.Context, r *drun) []check
}

// doctorTable is the check list, in output order within each group.
var doctorTable []doctorCheck

func init() {
	doctorTable = append(doctorTable, setupTable...)
}

// doctorDeps are the seams of every check: tests replace them (seams.doctor) to run without a live
// system.
type doctorDeps struct {
	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration) error
	dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	http         *http.Client
	lookPath     func(string) (string, error)
	ffmpeg       func(path string) (ffmpeg.Version, error)
	mediamtx     func(exe string) check
	liveSize     func(path string) (int64, error)
	prober       func(cfg config.Config) shadow.Prober
	platform     osx.Platform  // its default folders: the relay and live secrets
	checkTimeout time.Duration // per check
	netBudget    time.Duration // every concurrent check together
	parallel     int
}

func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		},
		dial: (&net.Dialer{}).DialContext,
		http: &http.Client{
			Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		lookPath:     exec.LookPath,
		ffmpeg:       ffmpeg.CheckVersion,
		mediamtx:     mediamtx,
		liveSize:     archive.LiveSize,
		prober:       shadowProber,
		checkTimeout: 3 * time.Second,
		netBudget:    10 * time.Second,
		parallel:     4,
	}
}

// drun is one doctor run: what every check reads, resolved once.
type drun struct {
	e       *env
	d       doctorDeps
	now     time.Time
	doc     *status.Document
	running bool
	why     string // app.Running's reason
	downWhy string // the same for the not-running notice of the runtime groups
	deep    bool
	sem     chan struct{} // the network probes' pool

	mu    sync.Mutex
	cache map[string]*onceValue
}

type onceValue struct {
	once sync.Once
	v    any
}

// once returns the value build makes for key, built by the first caller only; checks run
// concurrently, and a slow build (the growth sample) blocks only the callers of its own key.
func (r *drun) once(key string, build func() any) any {
	r.mu.Lock()
	ov, ok := r.cache[key]
	if !ok {
		ov = &onceValue{}
		r.cache[key] = ov
	}
	r.mu.Unlock()
	ov.once.Do(func() { ov.v = build() })
	return ov.v
}

type doctorSummary struct {
	Verdict   string `json:"verdict"` // HEALTHY | DEGRADED | BROKEN | NOT RUNNING
	Running   bool   `json:"running"`
	OK        int    `json:"ok"`
	Info      int    `json:"info"`
	Warnings  int    `json:"warnings"`
	Failures  int    `json:"failures"`
	ElapsedMs int64  `json:"elapsedMs"`
}

// doctorReport is `doctor --json`.
type doctorReport struct {
	Schema  int           `json:"schema"`
	Root    string        `json:"root"`
	Groups  []string      `json:"groups"`
	Checks  []check       `json:"checks"`
	Summary doctorSummary `json:"summary"`
}

func cmdDoctor(ctx context.Context, e *env) error {
	fs := newFlags(e, "doctor")
	quiet := fs.Bool("quiet", false, "only the lines that are not ok, and the summary")
	group := fs.String("group", "", "only these groups (comma-separated): "+strings.Join(doctorGroups, ", "))
	setupOnly := fs.Bool("setup-only", false, "only the set-up checks, printed as before")
	deep := fs.Bool("deep", false, "also ffprobe the newest closed segment of each camera")
	pos, err := parseFlags(fs, e.args)
	if err != nil {
		return err
	}
	if err := wantArgs("doctor", pos, 0, 0); err != nil {
		return err
	}
	sel := map[string]bool{}
	switch {
	case *setupOnly:
		sel["setup"] = true
	case *group == "":
		for _, g := range doctorGroups {
			sel[g] = true
		}
	default:
		for _, g := range strings.Split(*group, ",") {
			g = strings.TrimSpace(g)
			if !contains(doctorGroups, g) {
				return usageErr("holocron doctor: --group takes %s, got %q", strings.Join(doctorGroups, ", "), g)
			}
			sel[g] = true
		}
	}
	d := defaultDoctorDeps()
	d.platform = e.opts.Platform
	if e.doctor != nil {
		e.doctor(&d)
	}
	r := &drun{e: e, d: d, deep: *deep, cache: map[string]*onceValue{}, sem: make(chan struct{}, max(d.parallel, 1))}
	start := d.now()
	r.now = start
	r.doc, r.running, r.why = app.Running(e.paths, e.cfg, start)
	r.downWhy = r.why
	if errors.Is(readErr(e), status.ErrNoStatus) {
		r.downWhy = "no status file has been written yet"
	}
	rep := doctorReport{Schema: 1, Root: e.paths.Root, Checks: r.runAll(ctx, sel)}
	for _, g := range doctorGroups {
		if sel[g] {
			rep.Groups = append(rep.Groups, g)
		}
	}
	rep.Summary = summarize(rep.Checks, r.running, d.now().Sub(start))
	switch {
	case e.json:
		printJSON(e.out, rep)
	case *setupOnly:
		printSetupOnly(e.out, rep.Checks)
	default:
		printDoctor(e.out, rep, *quiet)
	}
	if n := rep.Summary.Failures; n > 0 {
		return quietFail("%d doctor check(s) failed", n)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// runAll runs the selected checks: the sequential ones in table order, the concurrent ones at the
// same time inside d.netBudget (their network probes share a pool of d.parallel, see probe). A
// runtime group of a system that is not running is one warning instead of its checks.
func (r *drun) runAll(ctx context.Context, sel map[string]bool) []check {
	results := make([][]check, len(doctorTable))
	budget, cancel := context.WithTimeout(ctx, r.d.netBudget)
	defer cancel()
	var wg sync.WaitGroup
	skipped := map[string]bool{}
	for i, c := range doctorTable {
		switch {
		case !sel[c.group]:
		case runtimeGroups[c.group] && !r.running:
			if !skipped[c.group] {
				skipped[c.group] = true
				results[i] = []check{mk(lvWarn, c.group, "not checked: nothing is running (%s)", r.downWhy).fix("holocron up --detach")}
				results[i][0].ID = c.group + ".running"
			}
		case c.concurrent:
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = r.one(budget, c)
			}()
		default:
			results[i] = r.one(ctx, c)
		}
	}
	wg.Wait()
	var out []check
	for _, g := range doctorGroups {
		for i, c := range doctorTable {
			if c.group != g {
				continue
			}
			for _, res := range results[i] {
				if res.ID == "" {
					res.ID = c.id
				}
				res.Group = c.group
				out = append(out, res)
			}
		}
	}
	return out
}

// probe runs one network probe in the run's pool of d.parallel; it gives up when ctx ends first.
func (r *drun) probe(ctx context.Context, fn func()) error {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
		fn()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// one runs a check with its timeout; a check that does not return in time is a warning, so doctor
// never hangs on a dead disk or a silent peer.
func (r *drun) one(parent context.Context, c doctorCheck) []check {
	t := c.timeout
	if t == 0 {
		t = r.d.checkTimeout
	}
	ctx, cancel := context.WithTimeout(parent, t)
	defer cancel()
	done := make(chan []check, 1)
	go func() { done <- c.run(ctx, r) }()
	var out []check
	select {
	case out = <-done:
	case <-ctx.Done():
		select {
		case out = <-done: // the check saw its context end and says what it found
		case <-time.After(200 * time.Millisecond):
			out = []check{timedOut(c, t)}
		}
	}
	log := r.e.lg.For("doctor")
	for _, res := range out {
		id := res.ID
		if id == "" {
			id = c.id
		}
		log.Debug("doctor check "+id+": "+res.Level, "check", id, "level", res.Level, "title", res.Title, "detail", res.Detail)
	}
	return out
}

func timedOut(c doctorCheck, t time.Duration) check {
	return mk(lvWarn, c.id[strings.LastIndexByte(c.id, '.')+1:], "not checked: no answer within %s", t).fix("run holocron doctor --group %s again; if it repeats, holocron logs --level warn --since 10m", c.group)
}

func summarize(cs []check, running bool, elapsed time.Duration) doctorSummary {
	s := doctorSummary{Running: running, ElapsedMs: elapsed.Milliseconds()}
	notRunning := false
	for _, c := range cs {
		switch c.Level {
		case lvOK:
			s.OK++
		case lvInfo:
			s.Info++
		case lvWarn:
			s.Warnings++
		case lvFail:
			s.Failures++
		}
		if strings.HasSuffix(c.ID, ".running") && runtimeGroups[c.Group] {
			notRunning = true
		}
	}
	switch {
	case notRunning:
		s.Verdict = "NOT RUNNING"
	case s.Failures > 0:
		s.Verdict = "BROKEN"
	case s.Warnings > 0:
		s.Verdict = "DEGRADED"
	default:
		s.Verdict = "HEALTHY"
	}
	return s
}

func (s doctorSummary) line() string {
	var counts []string
	if s.Failures > 0 {
		counts = append(counts, fmt.Sprintf("%d failure(s)", s.Failures))
	}
	if s.Warnings > 0 {
		counts = append(counts, fmt.Sprintf("%d warning(s)", s.Warnings))
	}
	if len(counts) == 0 {
		return s.Verdict
	}
	return s.Verdict + " (" + strings.Join(counts, ", ") + ")"
}

func printDoctor(w io.Writer, rep doctorReport, quiet bool) {
	group := ""
	for _, c := range rep.Checks {
		if quiet && (c.Level == lvOK || c.Level == lvInfo) {
			continue
		}
		if c.Group != group && !quiet {
			if group != "" {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "== %s ==\n", c.Group)
		}
		group = c.Group
		fmt.Fprintf(w, "%-4s %-14s %s\n", c.Level, c.Title, c.Detail)
		if c.Fix != "" && (c.Level == lvWarn || c.Level == lvFail) {
			fmt.Fprintf(w, "     fix: %s\n", c.Fix)
		}
	}
	if !quiet {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, rep.Summary.line())
}

// printSetupOnly is the doctor of before W19: the set-up lines and, on a failure, the count.
func printSetupOnly(w io.Writer, cs []check) {
	fails := 0
	for _, c := range cs {
		if c.Level == lvFail {
			fails++
		}
		fmt.Fprintf(w, "%-4s %-14s %s\n", c.Level, c.Title, c.Detail)
	}
	if fails > 0 {
		fmt.Fprintf(w, "%d check(s) failed\n", fails)
	}
}
