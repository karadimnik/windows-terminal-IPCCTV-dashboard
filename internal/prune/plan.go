package prune

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"holocron/internal/archive"
	"holocron/internal/config"
)

// Deletion reasons.
const (
	ReasonAge       = "age"
	ReasonWatermark = "watermark"
)

// Gates that refuse a whole run, in the order they are checked.
const (
	GateRoot   = "root"
	GateMarker = "marker"
	GateClock  = "clock"
)

// futureSkew is how far ahead of the wall clock a segment may be written (Get-CamPruneClockProblem).
const futureSkew = 60 * time.Second

// File is one directory entry of a camera folder or of one of its day folders (never a
// subdirectory: those are not entered).
type File struct {
	Name    string
	Day     string    // the day folder holding it (decision D20), "" in the camera folder (the spool)
	MTime   time.Time // UTC
	Size    int64
	Reparse bool      // a symbolic link, junction, any reparse point, or not a regular file
	Created time.Time // creation time where the OS reports it (Windows); zero elsewhere
}

// Camera is one immediate subfolder of the root, its files and its day folders' files.
type Camera struct {
	Dir     string // full path; its base name is the camera's safe name
	Reparse bool   // the folder itself is a link or reparse point
	Files   []File
	Days    []string // the day folders listed (archive.Entry.IsDay), oldest first: the executor removes an empty one
	Newest  string   // archive.Layout's Newest for this folder: always protected
}

// Rules are the configuration the pruner reads.
type Rules struct {
	ReserveGB           float64
	PruneHysteresisGB   float64
	MaxRetentionDays    *float64 // nil or <= 0: age rule off
	MaxAgePruneFraction float64
	Container           string  // the configured container's extension, ".mkv"
	SegmentSeconds      float64 // with StallFactor, the clock-sanity lag bound
	StallFactor         float64
}

// RulesFrom takes the prune rules out of a loaded configuration. An undefined container profile
// gives an empty extension, which no file matches, so nothing is deletable.
func RulesFrom(c config.Config) Rules {
	r := Rules{
		ReserveGB: c.ReserveGB, PruneHysteresisGB: c.PruneHysteresisGB, MaxRetentionDays: c.MaxRetentionDays,
		MaxAgePruneFraction: c.MaxAgePruneFraction, SegmentSeconds: float64(c.SegmentSeconds), StallFactor: c.StallFactor,
	}
	if p, ok := c.Profile(); ok {
		r.Container = p.Extension
	}
	return r
}

// Inputs is everything Plan decides from. Collect builds it from the filesystem.
type Inputs struct {
	Root          string
	Matcher       archive.Matcher
	Cameras       []Camera
	Skipped       []string // top-level entries refused before listing ("<path> (reparse point: ...)")
	FreeBytes     uint64   // available to the user on the root's volume
	Rules         Rules
	Now           time.Time // the one wall-clock reading of the run, UTC
	RootProblem   string    // archive.CheckRoot
	MarkerProblem string    // archive.VerifyRoot
}

// Deletion is one planned (or performed) removal.
type Deletion struct {
	Path   string
	Camera string // folder name
	Name   string
	Bytes  int64
	MTime  time.Time
	Reason string // ReasonAge or ReasonWatermark
	Detail string // the PowerShell reason text
	// Guard is the protected newest segment of the same folder that this file is strictly older
	// than; the executor re-checks it before removing.
	Guard      string
	GuardMTime time.Time
}

// Refusal is why a whole run deletes nothing.
type Refusal struct {
	Gate   string // GateRoot, GateMarker, GateClock
	Reason string
}

// Count is a per-folder count with the file it refers to.
type Count struct {
	Dir    string
	Count  int
	Newest string // for Kept: the protected newest segment
}

// Decision is the outcome of Plan (and, with the real numbers, of Executor.Run).
type Decision struct {
	Root      string
	Refusal   *Refusal
	Deletions []Deletion // in the order they go
	Blocked   bool       // a gate refused, or the watermark ran out of candidates below its target
	Short     bool       // executor only: the plan ran out before the target (the next run continues)

	AgeRule     bool   // MaxRetentionDays is on
	Watermark   bool   // free space was below the reserve
	BreakerHeld bool   // the age rule was held by the circuit breaker
	Breaker     string // the breach text

	Protected  []string // every protected newest segment
	Rejected   []string // refused links and paths, "<path> (<why>)"
	Foreign    []Count  // files left alone because they are not footage of the folder: Dir is the camera folder or a day folder in it
	Kept       []Count  // segments not strictly older than the folder's newest
	Messages   []string // what PowerShell logs at the gates, in order
	Candidates int
	Folders    int // camera folders holding segments: one in-progress file each
	Failed     int // executor only

	FreeGBBefore, FreeGBAfter, ReserveGB, TargetGB float64
	FreedBytes                                     int64
	StopLine, Message                              string
}

// faults are deliberate planner defects, set only by the negative-control tests.
var faults struct {
	skipClock     bool // do not run the clock gate
	protectOldest bool // protect each folder's oldest segment instead of its newest
	skipBreaker   bool // never hold the age rule
}

// seg is a segment that passed the per-file gate.
type seg struct {
	dir, folder, path string // dir is the camera folder; path is in it or in its day folder f.Day
	f                 File
	start             time.Time // the instant in its name
	guard             *seg
}

// actor performs the deletions and reads free space: simulated in Plan, real in the executor.
type actor interface {
	free() float64
	// remove deletes s; stop means it is outside what may be deleted and the run must end here.
	remove(s *seg, reason, detail string) (ok, stop bool)
}

type simulator struct {
	base  float64
	freed float64
}

func (s *simulator) free() float64 { return roundGB(s.base + s.freed) }
func (s *simulator) remove(c *seg, _, _ string) (bool, bool) {
	s.freed += float64(c.f.Size)
	return true, false
}

// Plan decides one prune run without touching anything: which files go, in which order and why,
// or why nothing may go. The gate order is the contract (IR section 5 and section 11 risk 3): the
// root, the ownership marker, the per-file allowlist, clock sanity, the age-rule circuit breaker,
// strictly older than the folder's protected newest, then the free-space watermark. The free space
// after each deletion is predicted as the reading plus the bytes freed, rounded like
// Get-CamFreeSpaceGB; the executor re-reads it instead.
func Plan(in Inputs) Decision {
	return run(in, &simulator{base: float64(in.FreeBytes)})
}

func (in Inputs) same(a, b string) bool {
	if in.Matcher.FoldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// layout is the archive layout Plan and the executor classify paths with.
func (in Inputs) layout() archive.Layout {
	return archive.Layout{Root: in.Root, Matcher: in.Matcher, Container: in.Rules.Container}
}

// footageOf: c is a segment of camera folder that is where it belongs, in the spool (day "") or
// filed under its own date in the day folder day, as Collect listed it. The camera folder is a
// direct child of the root and does not start with "_" (Classify says so).
func footageOf(c archive.Class, folder, day string) bool {
	return c.Footage() && c.Camera == folder && c.Day == day
}

// validLeaf: a directory entry name that cannot step out of its folder.
func validLeaf(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`+"\x00")
}

func run(in Inputs, a actor) Decision {
	r := in.Rules
	d := Decision{Root: in.Root, ReserveGB: r.ReserveGB, TargetGB: r.ReserveGB + r.PruneHysteresisGB}
	d.FreeGBBefore = a.free()
	d.FreeGBAfter = d.FreeGBBefore

	refuse := func(gate, reason string) Decision {
		d.Refusal = &Refusal{Gate: gate, Reason: reason}
		d.Blocked = true
		d.Message = fmt.Sprintf("PRUNING DISABLED: %s. Nothing is deleted until this is fixed, so the disk will fill. Recording continues.", reason)
		return d
	}
	// 1 and 2: the root and its ownership marker (Get-CamPruneRootRefusal).
	if in.RootProblem != "" {
		reason := in.RootProblem
		if !strings.HasSuffix(reason, "Point RootPath at a folder of its own") {
			reason += ". Point RootPath at a folder of its own"
		}
		return refuse(GateRoot, reason)
	}
	if in.MarkerProblem != "" {
		return refuse(GateMarker, in.MarkerProblem)
	}

	// 3: the per-file allowlist (Get-CamSegmentDirectories, Get-CamPruneRefusal).
	for _, s := range in.Skipped {
		d.Rejected = append(d.Rejected, s)
		d.Messages = append(d.Messages, "prune guard: refusing to prune "+s)
	}
	layout := in.layout()
	var candidates, unprotected, newest, seen []*seg
	for _, cam := range in.Cameras {
		folder := filepath.Base(cam.Dir)
		if strings.HasPrefix(folder, "_") {
			continue // the recorder's own folders, never a camera
		}
		if cam.Reparse {
			s := cam.Dir + " (reparse point: its content is outside the archive)"
			d.Rejected = append(d.Rejected, s)
			d.Messages = append(d.Messages, "prune guard: refusing to prune "+s)
			continue
		}
		if len(cam.Files) == 0 {
			continue
		}
		var segs []*seg
		foreign := map[string]int{} // by day folder, "" the spool
		var foreignOrder []string
		for _, f := range cam.Files {
			path := filepath.Join(cam.Dir, f.Day, f.Name)
			switch {
			case f.Reparse:
				d.Rejected = append(d.Rejected, path+" (reparse point)")
				d.Messages = append(d.Messages, "prune guard: refusing to delete "+path+": reparse point")
				continue
			case !validLeaf(f.Name) || (f.Day != "" && !validLeaf(f.Day)):
				d.Rejected = append(d.Rejected, path+" (outside the archive root)")
				d.Messages = append(d.Messages, "prune guard: refusing to delete "+path+": outside the archive root")
				continue
			}
			_, start, ok := archive.ParseSegment(f.Name)
			if !ok || !footageOf(layout.Classify(path), folder, f.Day) {
				// not a segment of the folder, or in a day folder that is not the date in its name
				// (misfiled): never a candidate; the doctor names it (storage.unseen)
				if foreign[f.Day] == 0 {
					foreignOrder = append(foreignOrder, f.Day)
				}
				foreign[f.Day]++
				continue
			}
			segs = append(segs, &seg{dir: cam.Dir, folder: folder, path: path, f: f, start: start})
		}
		for _, day := range foreignOrder {
			dir := filepath.Join(cam.Dir, day)
			d.Foreign = append(d.Foreign, Count{Dir: dir, Count: foreign[day]})
			d.Messages = append(d.Messages, fmt.Sprintf("prune: %d foreign file(s) left alone in %s", foreign[day], dir))
		}
		if len(segs) == 0 {
			continue
		}
		if cam.Newest != "" && !hasName(segs, cam.Newest, in) {
			// The scan saw a newest segment this listing does not hold: the two readings disagree.
			d.Messages = append(d.Messages, fmt.Sprintf("prune guard: skipping %s: its newest segment %s is not in the listing", cam.Dir, cam.Newest))
			continue
		}
		d.Folders++
		seen = append(seen, segs...)

		// 4: protection. PowerShell protects the newest by NTFS creation time. Here the newest is
		// the latest instant in the name (when ffmpeg created the file) and, where the OS reports
		// creation time (Windows), also the latest created - they differ only for files copied or
		// restored. Every file tied at either is protected and the strictly-older bound is the
		// earliest write among them, so this protects at least what PowerShell protects. The
		// newest by write time (the file ffmpeg holds open, archive.Scan's Newest) is protected too.
		named := append(nameNewest(segs), createdNewest(segs)...)
		if faults.protectOldest {
			named = []*seg{segs[0]}
			for _, s := range segs {
				if s.start.Before(named[0].start) {
					named = []*seg{s}
				}
			}
		}
		written := writeNewest(segs)
		rep := named[0]
		for _, s := range named {
			if s.f.MTime.Before(rep.f.MTime) {
				rep = s
			}
		}
		protected := map[*seg]bool{}
		for _, s := range named {
			protected[s] = true
		}
		if !faults.protectOldest {
			protected[written] = true
		}
		for _, s := range segs {
			if cam.Newest != "" && in.same(s.f.Name, cam.Newest) && !faults.protectOldest {
				protected[s] = true
			}
		}
		d.Protected = append(d.Protected, rep.path)
		for _, s := range segs {
			if protected[s] && s != rep {
				d.Protected = append(d.Protected, s.path)
			}
		}
		newest = append(newest, rep)

		kept := 0
		for _, s := range segs {
			if isNamed(named, s) {
				continue
			}
			unprotected = append(unprotected, s)
			if !protected[s] && (s.f.MTime.Before(rep.f.MTime) || faults.protectOldest) {
				s.guard = rep
				candidates = append(candidates, s)
			} else {
				kept++
			}
		}
		if kept > 0 {
			d.Kept = append(d.Kept, Count{Dir: cam.Dir, Count: kept, Newest: rep.f.Name})
			d.Messages = append(d.Messages, fmt.Sprintf("prune: %d segment(s) in %s kept: not older than its newest segment %s", kept, cam.Dir, rep.f.Name))
		}
	}

	// Globally oldest first by write time; equal times by name, then folder (IR U5).
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.f.MTime.Equal(b.f.MTime) {
			return a.f.MTime.Before(b.f.MTime)
		}
		if a.f.Name != b.f.Name {
			return a.f.Name < b.f.Name
		}
		if a.dir != b.dir {
			return a.dir < b.dir
		}
		return a.f.Day < b.f.Day
	})
	d.Candidates = len(candidates)

	ageRule := r.MaxRetentionDays != nil && *r.MaxRetentionDays > 0
	d.AgeRule = ageRule
	free := a.free()
	watermark := free < r.ReserveGB
	now := in.Now.UTC()

	// 5: clock sanity, before anything that could delete.
	if len(unprotected) > 0 && (ageRule || watermark) && !faults.skipClock {
		if why := clockProblem(unprotected, newest, seen, now, r); why != "" {
			d.Refusal = &Refusal{Gate: GateClock, Reason: why}
			d.Blocked = true
			d.Message = fmt.Sprintf("PRUNE REFUSED: %s. Nothing deleted this run; recording continues.", why)
			d.StopLine = "prune stop: refused before the first deletion - " + why
			return d
		}
	}

	record := func(s *seg, reason, detail string) (bool, bool) {
		ok, stop := a.remove(s, reason, detail)
		if ok {
			d.Deletions = append(d.Deletions, Deletion{
				Path: s.path, Camera: s.folder, Name: s.f.Name, Bytes: s.f.Size, MTime: s.f.MTime,
				Reason: reason, Detail: detail, Guard: s.guard.path, GuardMTime: s.guard.f.MTime,
			})
			d.FreedBytes += s.f.Size
		}
		return ok, stop
	}

	// 6: the age rule and its circuit breaker.
	consumed := map[*seg]bool{}
	ageStop := "age rule off"
	var cutoff time.Time
	if ageRule {
		cutoff = now.Add(-time.Duration(math.Round(*r.MaxRetentionDays*86400000)) * time.Millisecond)
		var expired []*seg
		for _, c := range candidates {
			if c.f.MTime.Before(cutoff) {
				expired = append(expired, c)
			}
		}
		if !faults.skipBreaker {
			d.Breaker = breach(expired, seen, r.MaxAgePruneFraction)
		}
	}
	if d.Breaker != "" {
		d.BreakerHeld = true
		ageStop = "age rule HELD by the circuit breaker, nothing deleted by age: " + d.Breaker
	} else if ageRule {
		ageDeleted := 0
		var firstKept *seg
		short := false
		for _, c := range candidates {
			if !c.f.MTime.Before(cutoff) {
				firstKept = c
				break
			}
			consumed[c] = true
			ok, stop := record(c, ReasonAge, "older than MaxRetentionDays="+days(*r.MaxRetentionDays))
			if stop {
				short = true
				break
			}
			if ok {
				ageDeleted++
			}
		}
		switch {
		case short:
			d.Short = true
			ageStop = fmt.Sprintf("age rule stopped at the end of the plan after %d deletion(s)", ageDeleted)
		case firstKept != nil:
			ageStop = fmt.Sprintf("age rule reached its cutoff %s after %d deletion(s), stopping at %s (written %s)",
				stamp(cutoff), ageDeleted, firstKept.f.Name, stamp(firstKept.f.MTime))
		default:
			ageStop = fmt.Sprintf("age rule: %d deletion(s), every candidate was older than the cutoff %s - nothing deletable left", ageDeleted, stamp(cutoff))
		}
	}

	// 7: the free-space watermark, from the free space the age rule left.
	if len(d.Deletions) > 0 {
		free = a.free()
		watermark = free < r.ReserveGB
	}
	d.Watermark = watermark
	waterStop := fmt.Sprintf("watermark not needed: free %s GB is at or above the %s GB reserve", n2(free), n2(r.ReserveGB))
	if watermark && d.Short {
		waterStop = "watermark not run: the age rule ended at the end of the plan; the next run continues"
	}
	if watermark && !d.Short {
		index, waterDeleted := 0, 0
		for free < d.TargetGB {
			if index >= len(candidates) {
				d.Blocked = true
				break
			}
			c := candidates[index]
			index++
			if consumed[c] {
				continue
			}
			ok, stop := record(c, ReasonWatermark, fmt.Sprintf("free %s GB below reserve %s GB, target %s GB", n2(free), n2(r.ReserveGB), n2(d.TargetGB)))
			if stop {
				d.Short = true
				break
			}
			if !ok {
				continue
			}
			waterDeleted++
			free = a.free()
		}
		switch {
		case d.Blocked:
			waterStop = fmt.Sprintf("watermark: nothing deletable left after %d deletion(s), free %s GB is still below the %s GB reserve", waterDeleted, n2(free), n2(r.ReserveGB))
		case d.Short:
			waterStop = fmt.Sprintf("watermark stopped at the end of the plan after %d deletion(s), free %s GB is below the %s GB target; the next run continues", waterDeleted, n2(free), n2(d.TargetGB))
		default:
			waterStop = fmt.Sprintf("watermark reached its target %s GB after %d deletion(s), free %s GB", n2(d.TargetGB), waterDeleted, n2(free))
			if index < len(candidates) {
				waterStop += ", stopping at " + candidates[index].f.Name
			}
		}
	}
	d.StopLine = fmt.Sprintf("prune stop: %s; %s", ageStop, waterStop)
	d.FreeGBAfter = a.free()
	d.Message = finalMessage(d, false)
	return d
}

// finalMessage is the run's one-line outcome, as Invoke-CamPrune words it.
func finalMessage(d Decision, dry bool) string {
	switch {
	case d.Refusal != nil:
		if d.Refusal.Gate == GateClock {
			return fmt.Sprintf("PRUNE REFUSED: %s. Nothing deleted this run; recording continues.", d.Refusal.Reason)
		}
		return fmt.Sprintf("PRUNING DISABLED: %s. Nothing is deleted until this is fixed, so the disk will fill. Recording continues.", d.Refusal.Reason)
	case d.Blocked && !dry:
		return fmt.Sprintf("PRUNE BLOCKED: free %s GB is below the %s GB reserve and nothing deletable is left (%d candidate(s), %d undeletable, %d protected as in-progress). Recording continues. Lower ReserveGB, move RootPath to a larger volume, or free space by hand.",
			n2(d.FreeGBAfter), n2(d.ReserveGB), d.Candidates, d.Failed, d.Folders)
	case d.Blocked:
		return fmt.Sprintf("would be BLOCKED: free %s GB is below the %s GB reserve and only %d segment(s) could be deleted, %d protected as in-progress",
			n2(d.FreeGBAfter), n2(d.ReserveGB), d.Candidates, d.Folders)
	case len(d.Deletions) > 0:
		return fmt.Sprintf("prune: %d file(s) deleted, %s GB freed, free %s -> %s GB (reserve %s, target %s)",
			len(d.Deletions), netFixed(float64(d.FreedBytes)/gib, 3), n2(d.FreeGBBefore), n2(d.FreeGBAfter), n2(d.ReserveGB), n2(d.TargetGB))
	}
	return ""
}

func hasName(segs []*seg, name string, in Inputs) bool {
	for _, s := range segs {
		if in.same(s.f.Name, name) {
			return true
		}
	}
	return false
}

// nameNewest is every segment at the latest start instant of its name.
func nameNewest(segs []*seg) []*seg {
	var out []*seg
	for _, s := range segs {
		switch {
		case len(out) == 0 || s.start.After(out[0].start):
			out = []*seg{s}
		case s.start.Equal(out[0].start):
			out = append(out, s)
		}
	}
	return out
}

// createdNewest is every segment at the latest creation time, where it is known.
func createdNewest(segs []*seg) []*seg {
	var out []*seg
	for _, s := range segs {
		switch {
		case s.f.Created.IsZero():
		case len(out) == 0 || s.f.Created.After(out[0].f.Created):
			out = []*seg{s}
		case s.f.Created.Equal(out[0].f.Created):
			out = append(out, s)
		}
	}
	return out
}

// writeNewest is archive.Scan's Newest rule: the latest write time, the later name on a tie.
func writeNewest(segs []*seg) *seg {
	var w *seg
	for _, s := range segs {
		if w == nil || s.f.MTime.After(w.f.MTime) || (s.f.MTime.Equal(w.f.MTime) && s.f.Name > w.f.Name) {
			w = s
		}
	}
	return w
}

func isNamed(named []*seg, s *seg) bool {
	for _, n := range named {
		if n == s {
			return true
		}
	}
	return false
}

// clockProblem is Get-CamPruneClockProblem: why the timestamps of this run cannot be trusted to
// delete by, or "". unprotected are every segment but each folder's protected newest, newest the
// protected newest of each folder, seen every segment that passed the file gate.
func clockProblem(unprotected, newest, seen []*seg, now time.Time, r Rules) string {
	if len(unprotected) == 0 || len(newest) == 0 {
		return ""
	}
	oldest := unprotected[0]
	for _, s := range unprotected {
		if s.f.MTime.Before(oldest.f.MTime) {
			oldest = s
		}
	}
	top := newest[0]
	for _, s := range newest {
		if s.f.MTime.After(top.f.MTime) {
			top = s
		}
	}
	if oldest.f.MTime.After(top.f.MTime) {
		return fmt.Sprintf("the oldest candidate %s (written %s) is later than the newest segment %s (written %s)",
			oldest.path, stamp(oldest.f.MTime), top.path, stamp(top.f.MTime))
	}
	limit := now.Add(futureSkew)
	for _, s := range seen {
		if s.f.MTime.After(limit) {
			return fmt.Sprintf("%s is written %s, in the future of the wall clock %s", s.path, stamp(s.f.MTime), stamp(now))
		}
	}
	bound := r.SegmentSeconds * r.StallFactor
	lag := now.Sub(top.f.MTime).Seconds()
	if lag > bound {
		return fmt.Sprintf("the wall clock %s is %d s ahead of the newest segment %s (written %s), more than the %d s stall threshold - the clock jumped or nothing is recording",
			stamp(now), int64(math.RoundToEven(lag)), top.path, stamp(top.f.MTime), int64(math.RoundToEven(bound)))
	}
	return ""
}

// breach is Get-CamAgePruneBreach: the age rule may not run when what it would delete is more
// than limit of the archive's segments by count OR by bytes (equal passes).
func breach(expired, seen []*seg, limit float64) string {
	if len(expired) == 0 {
		return ""
	}
	var eb, tb float64
	for _, s := range expired {
		eb += float64(s.f.Size)
	}
	for _, s := range seen {
		tb += float64(s.f.Size)
	}
	countShare := float64(len(expired)) / float64(len(seen))
	byteShare := 0.0
	if tb > 0 {
		byteShare = eb / tb
	}
	if countShare <= limit && byteShare <= limit {
		return ""
	}
	return fmt.Sprintf("the age rule would delete %d of %d segments (%s%%) and %s of %s GB (%s%%) of the bytes in the archive, more than MaxAgePruneFraction %s allows",
		len(expired), len(seen), netFixed(countShare*100, 1), n2(eb/gib), n2(tb/gib), netFixed(byteShare*100, 1), netTrim(limit, 2))
}
