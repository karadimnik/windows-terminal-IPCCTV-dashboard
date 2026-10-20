package prune

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
	"holocron/internal/osx"
)

// The differential comparison (LLD 6.3): every tree the PowerShell harness
// (tests/holocron/prune-diff.ps1) ran Invoke-CamPrune over is rebuilt here, the Go collector,
// planner and executor run the same steps, and the decisions are compared field by field.
// Differences are allowed only where the scenario names an expected difference
// (testdata/golden/prune/EXPECTED-DIFFERENCES.md).

const goldenDir = "../../testdata/golden/prune"

// goldenScale: the harness writes files 65536 times smaller than the sizes in a golden.
const goldenScale = 65536

type gEntry struct {
	Path, Kind, Target, Attrs string
	Bytes                     int64
	MTime, CTime              string
	Late                      bool
}

type gConfig struct {
	ReserveGB, PruneHysteresisGB, MaxAgePruneFraction float64
	MaxRetentionDays                                  *float64
}

type gStep struct {
	Op          string
	Dry, Fixed  bool
	Free, Shift float64
	State       string
	Config      gConfig
	Entries     []gEntry
	Paths       []string
}

type gDel struct {
	Path           string
	Bytes          int64
	Reason, Detail string
}

type gResult struct {
	Op                                             string
	Created                                        bool
	Problem                                        string
	Dry                                            bool
	Deletions                                      []gDel
	Removed                                        []string
	Changed                                        int
	Blocked                                        bool
	RootRefusal, ClockRefusal, AgeBreaker          string
	StopLine, Message                              string
	Protected, Rejected, Foreign                   []string
	Candidates, Deleted, Failed                    int
	FreeGBBefore, FreeGBAfter, ReserveGB, TargetGB float64
	Alerts                                         []struct {
		Id          int
		Type, Title string
	}
	State  struct{ RefusalAlerted, ClockRefused, BlockedAlerted, AgeBreakerTripped bool }
	Logged map[string]int
}

type gScenario struct {
	Name, Check, Note, Now, Container, Marker string
	Expect                                    []string
	SegmentSeconds                            int
	StallFactor                               float64
	Claim                                     *struct {
		Created bool
		Problem string
	}
	Entries []gEntry
	Steps   []gStep
	Results []gResult
}

func (s gScenario) tagged(tag string) bool {
	for _, t := range s.Expect {
		if t == tag {
			return true
		}
	}
	return false
}

func loadGoldens(t *testing.T) (table, corpus []gScenario) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(goldenDir, "table", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no table goldens under %s: %v", goldenDir, err)
	}
	for _, f := range files {
		if filepath.Base(f) == "pruneconfig.json" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s gScenario
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		table = append(table, s)
	}
	fh, err := os.Open(filepath.Join(goldenDir, "corpus.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var s gScenario
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("corpus line %d: %v", len(corpus)+1, err)
		}
		corpus = append(corpus, s)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return table, corpus
}

func parseTime(t *testing.T, s string) time.Time {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return v.UTC()
}

// replay is one scenario rebuilt and run by Go.
type replay struct {
	t        *testing.T
	s        gScenario
	fold     bool
	sandbox  string
	root     string
	now      time.Time
	fs       *scaledFS
	log      *recorder
	st       State
	rules    Rules
	noLinks  bool
	results  []goStep
	claimErr string
}

type goStep struct {
	state   State // after the step
	op      string
	created bool
	res     Result
	removed []string
	logged  map[string]int
}

func (r *replay) abs(rel string) string { return filepath.Join(r.sandbox, filepath.FromSlash(rel)) }

func (r *replay) add(e gEntry) {
	t := r.t
	p := r.abs(e.Path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	switch e.Kind {
	case "dir":
		os.MkdirAll(p, 0o755)
	case "file":
		n := e.Bytes / 65536
		b := make([]byte, n)
		for i := range b {
			b[i] = e.Path[i%len(e.Path)]
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		mt := parseTime(t, e.MTime)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		if err := setCreated(p, parseTime(t, e.CTime)); err != nil {
			t.Fatal(err)
		}
	case "dirlink", "filelink":
		if err := os.Symlink(r.abs(e.Target), p); err != nil {
			r.noLinks = true
		}
	default:
		t.Fatalf("entry kind %q", e.Kind)
	}
}

func (r *replay) marker(owner, rootPath string) {
	body := fmt.Sprintf(`{ "Owner": %q, "Schema": 1, "RootPath": %q }`, owner, rootPath)
	if err := os.MkdirAll(filepath.Join(r.root, "_state"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(archive.MarkerPath(r.root), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

var logKinds = []struct {
	kind  string
	match func(string) bool
}{
	{"refusal", func(m string) bool { return strings.HasPrefix(m, "PRUNING DISABLED") }},
	{"clockRefused", func(m string) bool { return strings.HasPrefix(m, "PRUNE REFUSED") }},
	{"clockResumed", func(m string) bool { return strings.Contains(m, "the clock and the archive agree again") }},
	{"held", func(m string) bool { return strings.HasPrefix(m, "PRUNE AGE RULE HELD") }},
	{"breakerReset", func(m string) bool { return strings.Contains(m, "circuit breaker is reset") }},
	{"blocked", func(m string) bool { return strings.HasPrefix(m, "PRUNE BLOCKED") }},
	{"deleted", func(m string) bool { return strings.HasPrefix(m, "prune: deleted ") }},
	{"summary", func(m string) bool { return strings.Contains(m, " file(s) deleted, ") }},
	{"stop", func(m string) bool { return strings.HasPrefix(m, "prune stop: ") }},
}

func (r *replay) run() {
	t := r.t
	r.sandbox = t.TempDir()
	r.root = filepath.Join(r.sandbox, "archive")
	if err := os.MkdirAll(r.root, 0o755); err != nil {
		t.Fatal(err)
	}
	r.now = parseTime(t, r.s.Now)
	r.fs = &scaledFS{OSFS: OSFS{Platform: osx.Current()}, Scale: goldenScale}
	r.log = &recorder{}
	ext := "." + r.s.Container
	if r.s.Container == "mkv" {
		ext = ".mkv"
	}
	r.rules = Rules{Container: ext, SegmentSeconds: float64(r.s.SegmentSeconds), StallFactor: r.s.StallFactor}
	for _, e := range r.s.Entries {
		if !e.Late {
			r.add(e)
		}
	}
	switch r.s.Marker {
	case "claim":
		res, err := archive.ClaimRoot(r.root, osx.Current(), ext, r.now)
		if err != nil {
			t.Fatal(err)
		}
		r.results = append(r.results, goStep{op: "initial-claim", created: res.Created})
	case "badowner":
		r.marker("someone else", r.root)
	case "copied":
		r.marker(archive.MarkerOwner, `C:\elsewhere\cameras`)
	}
	for _, e := range r.s.Entries {
		if e.Late {
			r.add(e)
		}
	}
	for _, step := range r.s.Steps {
		switch step.Op {
		case "claim":
			res, err := archive.ClaimRoot(r.root, osx.Current(), ext, r.now)
			if err != nil {
				t.Fatal(err)
			}
			r.results = append(r.results, goStep{op: "claim", created: res.Created})
		case "add":
			for _, e := range step.Entries {
				r.add(e)
			}
			r.results = append(r.results, goStep{op: "add"})
		case "remove":
			for _, p := range step.Paths {
				os.Remove(r.abs(p))
			}
			r.results = append(r.results, goStep{op: "remove"})
		case "prune":
			c := step.Config
			r.rules.ReserveGB, r.rules.PruneHysteresisGB, r.rules.MaxAgePruneFraction, r.rules.MaxRetentionDays = c.ReserveGB, c.PruneHysteresisGB, c.MaxAgePruneFraction, c.MaxRetentionDays
			if step.State == "fresh" {
				r.st = State{}
			}
			r.fs.Reset(step.Free)
			r.fs.Fixed = step.Fixed
			now := r.now.Add(time.Duration(step.Shift * float64(time.Second)))
			in, err := Collect(r.root, freePlatform{osx.Current(), r.fs}, r.rules, now, step.Dry)
			if err != nil {
				t.Fatal(err)
			}
			in.Matcher = archive.Matcher{FoldCase: r.fold}
			for i := range in.Cameras {
				for j := range in.Cameras[i].Files {
					in.Cameras[i].Files[j].Size *= goldenScale
				}
			}
			mark := len(r.log.recs)
			res, err := (&Executor{FS: r.fs, Log: slog.New(r.log), DryRun: step.Dry}).Run(in, &r.st)
			if err != nil {
				t.Fatal(err)
			}
			logged := map[string]int{}
			for _, x := range r.log.recs[mark:] {
				if x.Level < slog.LevelInfo {
					continue
				}
				for _, k := range logKinds {
					if k.match(x.Msg) {
						logged[k.kind]++
					}
				}
			}
			var removed []string
			for _, p := range r.fs.Removed {
				removed = append(removed, r.rel(p))
			}
			sort.Strings(removed)
			r.results = append(r.results, goStep{op: "prune", res: res, removed: removed, logged: logged, state: r.st})
		}
	}
}

func (r *replay) rel(p string) string {
	if rel, err := filepath.Rel(r.sandbox, p); err == nil {
		return filepath.ToSlash(rel)
	}
	return p
}

var stampRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)

// mask is the harness's ConvertTo-W3Masked: sandbox paths become sandbox-relative with '/'.
func (r *replay) mask(text string) string {
	prefix := r.sandbox + string(filepath.Separator)
	for {
		i := strings.Index(text, prefix)
		if i < 0 {
			return text
		}
		end := i + len(prefix)
		j := end
		for j < len(text) && !strings.ContainsRune(" :,)'", rune(text[j])) {
			j++
		}
		text = text[:i] + strings.ReplaceAll(text[end:j], `\`, "/") + text[j:]
	}
}

var (
	freedRE  = regexp.MustCompile(`\d+\.\d{3} GB freed`)
	breachRE = regexp.MustCompile(`and [\d.]+ of [\d.]+ GB \(`)
)

// unstamp masks what differs for reasons outside the pruner: local log stamps, and the two GB
// figures PowerShell computes from the physical (scaled-down) file lengths of the harness - the
// freed total of the summary and the GB pair of the breach text. Both sides use the same
// formulas on the logical sizes (TestNetFixed, TestPruneBreaker).
func unstamp(s string) string {
	s = stampRE.ReplaceAllString(s, "<stamp>")
	s = freedRE.ReplaceAllString(s, "<n> GB freed")
	return breachRE.ReplaceAllString(s, "and <x> of <y> GB (")
}

func refusalClass(gate, text string) string {
	switch {
	case gate == "" && text == "":
		return ""
	case gate == GateRoot || (gate == "" && strings.HasSuffix(text, "Point RootPath at a folder of its own") && !strings.Contains(text, "ownership marker")):
		return "root"
	case strings.Contains(text, "cannot claim the root"):
		return "unclaimable"
	case strings.Contains(text, "restart the recorder and it claims it"):
		return "claimable"
	}
	return "marker?" + text
}

func clockClass(s string) string {
	for _, k := range []string{"ahead of the newest segment", "in the future of the wall clock", "is later than the newest segment"} {
		if strings.Contains(s, k) {
			return k
		}
	}
	return s
}

var breachCounts = regexp.MustCompile(`delete (\d+) of (\d+) segments \(([\d.]+)%\) and .* GB \(([\d.]+)%\)`)

// compare returns the differences between PowerShell's result and Go's for one prune step.
func (r *replay) compare(ps gResult, g goStep) []string {
	var diff []string
	add := func(field string, p, q any) {
		diff = append(diff, fmt.Sprintf("%s: PowerShell %v, Go %v", field, p, q))
	}
	res := g.res
	gate, reason := "", ""
	if res.Refusal != nil {
		gate, reason = res.Refusal.Gate, res.Refusal.Reason
	}
	goRoot := ""
	if gate == GateRoot || gate == GateMarker {
		goRoot = refusalClass(gate, reason)
	}
	if p := refusalClass("", ps.RootRefusal); p != goRoot {
		add("root/marker refusal", p, goRoot)
	}
	goClock := ""
	if gate == GateClock {
		goClock = clockClass(reason)
	}
	if p := clockClass(ps.ClockRefusal); p != goClock {
		add("clock refusal", p, goClock)
	} else if ps.ClockRefusal != "" && unstamp(ps.ClockRefusal) != unstamp(r.mask(reason)) {
		add("clock refusal text", ps.ClockRefusal, r.mask(reason))
	}

	var psAge, psWater, goAge, goWater []string
	for _, d := range ps.Deletions {
		if d.Reason == ReasonAge {
			psAge = append(psAge, d.Path)
		} else {
			psWater = append(psWater, d.Path)
		}
	}
	for _, d := range res.Deletions {
		p := r.rel(d.Path)
		if d.Reason == ReasonAge {
			goAge = append(goAge, p)
		} else {
			goWater = append(goWater, p)
		}
	}
	if !eqList(psAge, goAge) {
		add("age deletions", psAge, goAge)
	}
	if ps.Dry {
		// The dry-run watermark estimates with the physical (scaled-down) file lengths in
		// PowerShell, so it is compared as a subset only.
		if !subset(goWater, psWater) {
			add("dry watermark deletions (Go must be a subset)", psWater, goWater)
		}
	} else {
		if !eqList(psWater, goWater) {
			add("watermark deletions", psWater, goWater)
		}
		if !eqList(sorted(ps.Removed), g.removed) && !(len(ps.Removed) == 0 && len(g.removed) == 0) {
			add("files removed", ps.Removed, g.removed)
		}
		for i, d := range ps.Deletions {
			if i < len(res.Deletions) && (d.Bytes != res.Deletions[i].Bytes || d.Detail != res.Deletions[i].Detail) {
				add("deletion "+d.Path, d.Bytes, fmt.Sprint(res.Deletions[i].Bytes, " ", res.Deletions[i].Detail, " vs ", d.Detail))
			}
		}
	}
	if (ps.AgeBreaker != "") != res.BreakerHeld {
		add("breaker held", ps.AgeBreaker != "", res.BreakerHeld)
	} else if ps.AgeBreaker != "" {
		if p, q := breachCounts.FindStringSubmatch(ps.AgeBreaker), breachCounts.FindStringSubmatch(res.Breaker); p == nil || q == nil || !eqList(p[1:], q[1:]) {
			add("breach counts", ps.AgeBreaker, res.Breaker)
		}
	}
	if ps.Candidates != res.Candidates {
		add("candidates", ps.Candidates, res.Candidates)
	}
	var gp []string
	for _, p := range res.Protected {
		gp = append(gp, r.rel(p))
	}
	if !subset(ps.Protected, gp) {
		add("protected (Go must protect at least these)", ps.Protected, gp)
	}
	var gf []string
	for _, f := range res.Foreign {
		gf = append(gf, r.mask(fmt.Sprintf("%d foreign file(s) left alone in %s", f.Count, f.Dir)))
	}
	if sorted(ps.Foreign) != nil || gf != nil {
		if !eqList(sorted(ps.Foreign), sorted(gf)) {
			add("foreign", ps.Foreign, gf)
		}
	}
	var gr []string
	for _, x := range res.Rejected {
		gr = append(gr, r.mask(x))
	}
	if !subset(ps.Rejected, gr) && !r.noLinks {
		add("rejected (Go must refuse at least these)", ps.Rejected, gr)
	}
	if !ps.Dry {
		if ps.Blocked != res.Blocked {
			add("blocked", ps.Blocked, res.Blocked)
		}
		if unstamp(ps.StopLine) != unstamp(r.mask(res.StopLine)) {
			add("stop line", ps.StopLine, r.mask(res.StopLine))
		}
		if goRoot == "" && unstamp(ps.Message) != unstamp(r.mask(res.Message)) {
			add("message", ps.Message, r.mask(res.Message))
		}
	} else {
		// A PowerShell dry run estimates free space from the physical (scaled-down) lengths:
		// only the refusals and the age part of its stop line are comparable.
		if (ps.RootRefusal != "" || ps.ClockRefusal != "") && ps.Blocked != res.Blocked {
			add("blocked", ps.Blocked, res.Blocked)
		}
		if p, q := strings.SplitN(unstamp(ps.StopLine), ";", 2)[0], strings.SplitN(unstamp(r.mask(res.StopLine)), ";", 2)[0]; p != q {
			add("dry stop line, age part", ps.StopLine, r.mask(res.StopLine))
		}
	}
	if !ps.Dry {
		if ps.FreeGBBefore != res.FreeGBBefore || ps.FreeGBAfter != res.FreeGBAfter || ps.ReserveGB != res.ReserveGB || ps.TargetGB != res.TargetGB {
			add("free/reserve/target GB", []float64{ps.FreeGBBefore, ps.FreeGBAfter, ps.ReserveGB, ps.TargetGB}, []float64{res.FreeGBBefore, res.FreeGBAfter, res.ReserveGB, res.TargetGB})
		}
		for k, n := range ps.Logged {
			if g.logged[k] != n {
				add("logged "+k, n, g.logged[k])
			}
		}
	}
	var pa, ga []string
	for _, a := range ps.Alerts {
		pa = append(pa, fmt.Sprintf("%d %s %s", a.Id, a.Type, a.Title))
	}
	for _, a := range res.Alerts {
		ga = append(ga, fmt.Sprintf("%d %s %s", a.ID, a.Level, a.Title))
	}
	if !eqList(pa, ga) {
		add("alerts", pa, ga)
	}
	st := g.state
	if ps.State.RefusalAlerted != (st.RefusalAlerted != "") || ps.State.ClockRefused != st.ClockRefused || ps.State.BlockedAlerted != st.BlockedAlerted || ps.State.AgeBreakerTripped != st.AgeBreakerTripped {
		add("state", ps.State, fmt.Sprintf("%+v", st))
	}
	return diff
}

func subset(a, b []string) bool {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

// outcome of one scenario under one matcher.
type outcome struct {
	diffs           []string // unexpected differences: failures
	expected        []string // differences under a declared tag
	psDel, goDel    int
	goExtra, goLess []string // deletions only Go made / only PowerShell made, over the real steps
	skipped         string
}

func replayScenario(t *testing.T, s gScenario, fold bool) outcome {
	r := &replay{t: t, s: s, fold: fold}
	r.run()
	var o outcome
	if r.noLinks && runtime.GOOS == "windows" {
		o.skipped = "this account may not create symbolic links"
		return o
	}
	gi := 0
	if s.Marker == "claim" {
		if s.Claim == nil || s.Claim.Created != r.results[0].created {
			o.diffs = append(o.diffs, fmt.Sprintf("initial claim: PowerShell %+v, Go created %v", s.Claim, r.results[0].created))
		}
		gi = 1
	}
	if len(r.results)-gi != len(s.Results) {
		t.Fatalf("%s: %d Go steps for %d PowerShell results", s.Name, len(r.results)-gi, len(s.Results))
	}
	for i, ps := range s.Results {
		g := r.results[gi+i]
		switch ps.Op {
		case "claim":
			if ps.Created != g.created {
				o.diffs = append(o.diffs, fmt.Sprintf("step %d claim: PowerShell created %v, Go %v", i, ps.Created, g.created))
			}
		case "prune":
			d := r.compare(ps, g)
			psSet, goSet := map[string]bool{}, map[string]bool{}
			for _, x := range ps.Deletions {
				psSet[x.Path] = true
			}
			for _, x := range g.res.Deletions {
				goSet[r.rel(x.Path)] = true
			}
			if !ps.Dry {
				o.psDel += len(ps.Deletions)
				o.goDel += len(g.res.Deletions)
				for p := range goSet {
					if !psSet[p] {
						o.goExtra = append(o.goExtra, p)
					}
				}
				for p := range psSet {
					if !goSet[p] {
						o.goLess = append(o.goLess, p)
					}
				}
			}
			for _, x := range d {
				line := fmt.Sprintf("step %d (dry %v): %s", i, ps.Dry, x)
				if firstTag(s, fold) != "" {
					o.expected = append(o.expected, line)
				} else {
					o.diffs = append(o.diffs, line)
				}
			}
		}
	}
	sort.Strings(o.goExtra)
	sort.Strings(o.goLess)
	return o
}

// TestGoldenDifferential compares every committed PowerShell decision with Go's.
func TestGoldenDifferential(t *testing.T) {
	table, corpus := loadGoldens(t)
	if len(corpus) < 200 {
		t.Errorf("the corpus has %d trees, want at least 200", len(corpus))
	}
	checks := map[string]bool{}
	var psTotal, goTotal, skipped int
	for _, group := range []struct {
		name string
		list []gScenario
	}{{"table", table}, {"corpus", corpus}} {
		for _, s := range group.list {
			checks[s.Check] = true
			for _, fold := range []bool{true, false} {
				o := replayScenario(t, s, fold)
				if o.skipped != "" {
					skipped++
					t.Logf("%s: skipped: %s", s.Name, o.skipped)
					break
				}
				for _, d := range o.diffs {
					t.Errorf("%s (fold case %v): %s", s.Name, fold, d)
				}
				if fold {
					psTotal += o.psDel
					goTotal += o.goDel
				}
				r := expectedRules[firstTag(s, fold)]
				if firstTag(s, fold) == "" {
					if len(o.goExtra)+len(o.goLess) > 0 {
						t.Errorf("%s (fold %v): deletion sets differ without a declared difference: only Go %v, only PowerShell %v", s.Name, fold, o.goExtra, o.goLess)
					}
					continue
				}
				if r == nil {
					t.Errorf("%s: undeclared expected difference %v", s.Name, s.Expect)
					continue
				}
				if err := r(s, o); err != "" {
					t.Errorf("%s (fold %v): expected difference %q does not hold: %s", s.Name, fold, firstTag(s, fold), err)
				}
				t.Logf("%s (fold %v, %s): only Go deleted %v, only PowerShell deleted %v; %d field differences", s.Name, fold, firstTag(s, fold), o.goExtra, o.goLess, len(o.expected))
				if testing.Verbose() {
					for _, e := range o.expected {
						t.Logf("    %s", e)
					}
				}
			}
		}
	}
	for _, c := range []string{"prunematrix", "pruneroots", "pruneage", "prunewatermark", "pruneclock", "prunebreaker", "pruneblocked", "pruneowner", "pruneloop"} {
		if !checks[c] {
			t.Errorf("no golden scenario for the check %s", c)
		}
	}
	t.Logf("%d table scenarios and %d corpus trees, each with case folding and without; real-run deletions: PowerShell %d, Go %d; %d skipped", len(table), len(corpus), psTotal, goTotal, skipped)
}

// firstTag is the expected difference that applies to a pass: "case-fold" only to the
// case-sensitive pass (with folding, Go must equal PowerShell there too).
func firstTag(s gScenario, fold bool) string {
	for _, t := range s.Expect {
		if t == "case-fold" {
			if fold {
				return ""
			}
			return t
		}
		if t != "" {
			return t
		}
	}
	return ""
}

// expectedRules: what each declared difference allows, and the direction it must keep.
var expectedRules = map[string]func(s gScenario, o outcome) string{
	// Go deletes a subset of what PowerShell deletes.
	"strict-allowlist": subsetRule,
	"case-fold":        subsetRule,
	"copied-marker":    subsetRule,
	"tie-order":        tieRule,
	// Go deletes a strict segment in place of a near-miss name PowerShell deleted: never a file
	// PowerShell protected or refused, never more files.
	"strict-allowlist-substitution": func(s gScenario, o outcome) string {
		if len(o.goLess) == 0 || o.goDel > o.psDel {
			return fmt.Sprintf("want a substitution with no more deletions, got only-Go %v only-PS %v (%d vs %d)", o.goExtra, o.goLess, o.goDel, o.psDel)
		}
		return protectedByPS(s, o.goExtra)
	},
	// After a copy or restore the creation times are the copy's. On Windows Go reads them and
	// protects the newest created as well, so it deletes a subset; elsewhere there is no creation
	// time and Go protects by the name instant and the write time only, which can delete what
	// PowerShell protects (EXPECTED-DIFFERENCES.md, "open-segment").
	"open-segment": func(s gScenario, o outcome) string {
		if runtime.GOOS == "windows" {
			return subsetRule(s, o)
		}
		return ""
	},
}

// tieRule: the same number of deletions at the same write times; only which of several files
// with an equal write time goes differs (IR U5), and never one PowerShell protected.
func tieRule(s gScenario, o outcome) string {
	at := map[string]string{}
	for _, e := range s.Entries {
		at[e.Path] = e.MTime
	}
	times := func(ps []string) []string {
		var out []string
		for _, p := range ps {
			out = append(out, at[p])
		}
		sort.Strings(out)
		return out
	}
	if o.goDel != o.psDel || !eqList(times(o.goExtra), times(o.goLess)) {
		return fmt.Sprintf("the difference is not a permutation among equal write times: only Go %v, only PowerShell %v", o.goExtra, o.goLess)
	}
	return protectedByPS(s, o.goExtra)
}

func subsetRule(s gScenario, o outcome) string {
	if len(o.goExtra) > 0 {
		return fmt.Sprintf("Go deleted files PowerShell kept: %v", o.goExtra)
	}
	return ""
}

func protectedByPS(s gScenario, files []string) string {
	for _, r := range s.Results {
		for _, p := range r.Protected {
			for _, f := range files {
				if f == p {
					return "Go deleted a file PowerShell protected: " + f
				}
			}
		}
	}
	return ""
}

// The pruneconfig golden: MaxAgePruneFraction validation at load.
func TestGoldenPruneConfig(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(goldenDir, "table", "pruneconfig.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Json  string
			Value *float64
			Error string
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) != 7 {
		t.Fatalf("%d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		cfg, _, err := config.Parse([]byte(c.Json), osx.Current())
		switch {
		case c.Error != "" && err == nil:
			t.Errorf("%s: PowerShell refused (%s), Go loaded %v", c.Json, c.Error, cfg.MaxAgePruneFraction)
		case c.Error == "" && (err != nil || c.Value == nil || cfg.MaxAgePruneFraction != *c.Value):
			t.Errorf("%s: PowerShell loaded %v, Go %v %v", c.Json, c.Value, cfg.MaxAgePruneFraction, err)
		case c.Error != "" && !strings.Contains(err.Error(), "MaxAgePruneFraction"):
			t.Errorf("%s: Go's refusal does not name the key: %v", c.Json, err)
		}
	}
}

// Negative controls: a planner with a deliberate defect must make the comparison fail.
func TestGoldenNegativeControls(t *testing.T) {
	table, _ := loadGoldens(t)
	for _, c := range []struct {
		name string
		set  func(bool)
	}{
		{"protect the oldest instead of the newest", func(on bool) { faults.protectOldest = on }},
		{"skip the clock gate", func(on bool) { faults.skipClock = on }},
		{"skip the circuit breaker", func(on bool) { faults.skipBreaker = on }},
	} {
		c.set(true)
		failed := 0
		for _, s := range table {
			if firstTag(s, true) != "" {
				continue
			}
			o := replayScenario(t, s, true)
			if len(o.diffs) > 0 || len(o.goExtra)+len(o.goLess) > 0 {
				failed++
			}
		}
		c.set(false)
		if failed == 0 {
			t.Errorf("%s: no scenario noticed", c.name)
		}
		t.Logf("%s: %d table scenarios differ", c.name, failed)
	}
}
