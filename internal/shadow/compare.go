package shadow

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"holocron/internal/archive"
)

// SchemaVersion is the version of the JSON report; it changes only when a field is removed or its
// meaning changes.
const SchemaVersion = 1

// Options are what one comparison needs. Zero values take the documented defaults (Defaults).
type Options struct {
	Old, New string   // the two archive roots (read only)
	Cameras  []string // folder names (safe names); empty = every camera folder of New
	Since    time.Time

	Now               time.Time
	SegmentSeconds    int           // clock multiple the segments are cut at; 900
	StartTolerance    time.Duration // pair by start time, boundary jitter; 5 s
	DurationTolerance time.Duration // ffprobe durations of a pair; 2 s
	GapTolerance      time.Duration // a gap is longer than this; 2 s
	SizeMin, SizeMax  float64       // new/old size ratio of a comparable pair; 0.9 and 1.1
	Quiet             time.Duration // files written within this long are still open and skipped; 90 s (the newest of a folder: one segment and a minute)
	Container         string        // ".mkv"
	Audio             string        // expected audio "codec:rate:channels" or "any"; pcm_alaw:8000:1

	Prober   Prober
	Matcher  archive.Matcher
	Progress func(string) // optional: one line per camera step
}

// Defaults fills the zero values of o.
func (o *Options) Defaults() {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.SegmentSeconds <= 0 {
		o.SegmentSeconds = 900
	}
	if o.StartTolerance <= 0 {
		o.StartTolerance = 5 * time.Second
	}
	if o.DurationTolerance <= 0 {
		o.DurationTolerance = 2 * time.Second
	}
	if o.GapTolerance <= 0 {
		o.GapTolerance = 2 * time.Second
	}
	if o.SizeMin <= 0 {
		o.SizeMin = 0.9
	}
	if o.SizeMax <= 0 {
		o.SizeMax = 1.1
	}
	if o.Quiet <= 0 {
		o.Quiet = 90 * time.Second
	}
	if o.Container == "" {
		o.Container = ".mkv"
	}
	if o.Audio == "" {
		o.Audio = "pcm_alaw:8000:1"
	}
	if o.Progress == nil {
		o.Progress = func(string) {}
	}
}

// Params are the settings a report was made with.
type Params struct {
	SegmentSeconds       int     `json:"segment_seconds"`
	StartToleranceSec    float64 `json:"start_tolerance_seconds"`
	DurationToleranceSec float64 `json:"duration_tolerance_seconds"`
	GapToleranceSec      float64 `json:"gap_tolerance_seconds"`
	SizeRatioMin         float64 `json:"size_ratio_min"`
	SizeRatioMax         float64 `json:"size_ratio_max"`
	QuietSeconds         float64 `json:"quiet_seconds"`
	Container            string  `json:"container"`
	Audio                string  `json:"audio"`
	Since                *string `json:"since"`
}

// Report is the whole result (the JSON document of `holocron archive compare --json`).
type Report struct {
	Schema      int            `json:"schema"`
	Old         string         `json:"old"`
	New         string         `json:"new"`
	CheckedAt   string         `json:"checked_at"`
	Params      Params         `json:"params"`
	Equal       bool           `json:"equal"`
	Differences int            `json:"differences"`
	Cameras     []CameraReport `json:"cameras"`
}

// CameraReport is one camera folder.
type CameraReport struct {
	Camera       string    `json:"camera"`
	Equal        bool      `json:"equal"`
	Window       *Window   `json:"window"`
	Old          Side      `json:"old"`
	New          Side      `json:"new"`
	Pairs        []Pair    `json:"pairs"`
	Partials     []Partial `json:"partials"`
	UnmatchedOld []string  `json:"unmatched_old"`
	UnmatchedNew []string  `json:"unmatched_new"`
	Size         SizeStats `json:"size_ratio"`
	Differences  []string  `json:"differences"`
	Notes        []string  `json:"notes"`
}

// Window is the compared time span (RFC 3339).
type Window struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Seconds float64 `json:"seconds"`
}

// Side is what one archive contributed for one camera.
type Side struct {
	Closed   int      `json:"closed_segments"`   // all closed segments found in the folder
	Compared int      `json:"compared_segments"` // those inside the window
	Bytes    int64    `json:"compared_bytes"`
	Open     []string `json:"skipped_open"`
	Gaps     []Gap    `json:"gaps"`
}

// Gap is the time between the end of one segment and the start of the next, above the tolerance.
type Gap struct {
	After   string  `json:"after"`
	Before  string  `json:"before"`
	Seconds float64 `json:"seconds"`
	Shared  bool    `json:"shared"` // the other side has a gap over the same time
	from    time.Time
	to      time.Time
}

// Pair is an old and a new segment that start together (within the start tolerance).
type Pair struct {
	Old           string   `json:"old"`
	New           string   `json:"new"`
	Kind          string   `json:"kind"` // full | short | partial_end | mismatch
	StartDiffSec  float64  `json:"start_diff_seconds"`
	OldBytes      int64    `json:"old_bytes"`
	NewBytes      int64    `json:"new_bytes"`
	SizeRatio     *float64 `json:"size_ratio"` // new/old; null for partial_end and mismatch
	OldDurSec     float64  `json:"old_duration_seconds"`
	NewDurSec     float64  `json:"new_duration_seconds"`
	DurationDiffS float64  `json:"duration_diff_seconds"`
	OldLayout     string   `json:"old_layout"`
	NewLayout     string   `json:"new_layout"`
	Flags         []string `json:"flags"`
}

// Partial is a segment that has no start partner but lies inside a segment of the other side: the
// shadow's first segment (it starts when it was started), a segment cut short by a restart.
type Partial struct {
	Side         string  `json:"side"` // old | new
	Name         string  `json:"name"`
	Within       string  `json:"within"`
	DurationSec  float64 `json:"duration_seconds"`
	EndDiffSec   float64 `json:"end_diff_seconds"` // its end minus the end of the segment it lies in
	Layout       string  `json:"layout"`
	EndsOnBounds bool    `json:"ends_with_partner"`
}

// SizeStats summarises the new/old size ratio of the comparable pairs.
type SizeStats struct {
	Pairs  int      `json:"pairs"`
	Median *float64 `json:"median"`
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
}

type seg struct {
	name  string
	path  string
	start time.Time
	size  int64
	mod   time.Time
	probe *Probe
	perr  string
	dur   float64 // seconds: ffprobe, or the estimate until probed (and when the file has no duration)
	noDur bool    // readable, but never finalized
}

func (s *seg) end() time.Time { return s.start.Add(time.Duration(s.dur * float64(time.Second))) }

// estimate is the end of the segment before it is probed: its last write time.
func (s *seg) estimate(segSeconds int) time.Time {
	if s.mod.After(s.start) {
		return s.mod
	}
	return s.start.Add(time.Duration(segSeconds) * time.Second)
}

func layout(p *Probe) string {
	if p == nil {
		return "unreadable"
	}
	v, a := "no video", "no audio"
	if p.Video != nil {
		v = fmt.Sprintf("%s %dx%d", p.Video.Codec, p.Video.Width, p.Video.Height)
	}
	if p.Audio != nil {
		a = fmt.Sprintf("%s %d Hz %dch", p.Audio.Codec, p.Audio.SampleRate, p.Audio.Channels)
	}
	return v + ", " + a
}

// Compare runs the comparison. It returns an error only when it cannot compare at all (a root
// that is not a folder, a cancelled context); differences are the Report.
func Compare(ctx context.Context, o Options) (Report, error) {
	o.Defaults()
	if o.Prober == nil {
		return Report{}, errors.New("shadow: no ffprobe")
	}
	for _, r := range []struct{ what, path string }{{"old", o.Old}, {"new", o.New}} {
		fi, err := os.Stat(r.path)
		if err != nil {
			return Report{}, fmt.Errorf("the %s root %s: %v", r.what, r.path, err)
		}
		if !fi.IsDir() {
			return Report{}, fmt.Errorf("the %s root %s is not a folder", r.what, r.path)
		}
	}
	if a, b := absClean(o.Old), absClean(o.New); strings.EqualFold(a, b) {
		return Report{}, errors.New("--old and --new are the same folder")
	}
	rep := Report{Schema: SchemaVersion, Old: o.Old, New: o.New, CheckedAt: o.Now.Format(time.RFC3339), Cameras: []CameraReport{}}
	rep.Params = Params{SegmentSeconds: o.SegmentSeconds, StartToleranceSec: o.StartTolerance.Seconds(), DurationToleranceSec: o.DurationTolerance.Seconds(),
		GapToleranceSec: o.GapTolerance.Seconds(), SizeRatioMin: o.SizeMin, SizeRatioMax: o.SizeMax, QuietSeconds: o.Quiet.Seconds(), Container: o.Container, Audio: o.Audio}
	if !o.Since.IsZero() {
		s := o.Since.Format(time.RFC3339)
		rep.Params.Since = &s
	}
	folders := o.Cameras
	if len(folders) == 0 {
		var err error
		if folders, err = cameraFolders(o.New); err != nil {
			return Report{}, err
		}
	}
	if len(folders) == 0 {
		return Report{}, fmt.Errorf("the new root %s has no camera folder", o.New)
	}
	for _, f := range folders {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		cr, err := compareCamera(ctx, &o, f)
		if err != nil {
			return rep, err
		}
		cr.Equal = len(cr.Differences) == 0
		rep.Differences += len(cr.Differences)
		rep.Cameras = append(rep.Cameras, cr)
	}
	rep.Equal = rep.Differences == 0
	return rep, nil
}

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return filepath.Clean(p)
}

// cameraFolders lists the folders of root that are not the recorder's own ("_" prefix).
func cameraFolders(root string) ([]string, error) {
	ents, err := archive.Layout{Root: root}.Folders()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

type listing struct {
	closed []*seg
	open   []string
	other  []string
}

func list(o *Options, root, folder string) (listing, error) {
	var l listing
	ents, err := archive.Layout{Root: root}.Entries(folder)
	if err != nil {
		return l, err
	}
	var all []*seg
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !o.Matcher.IsSegmentOf(name, folder, o.Container) {
			l.other = append(l.other, name)
			continue
		}
		_, start, _ := archive.ParseSegment(name)
		// os.Stat, not DirEntry.Info: on Windows the listing's times and sizes of a file that is
		// being written can be stale
		path := e.Path
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		all = append(all, &seg{name: name, path: path, start: start, size: fi.Size(), mod: fi.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if !a.start.Equal(b.start) {
			return a.start.Before(b.start)
		}
		return a.name < b.name
	})
	for i, s := range all {
		age := o.Now.Sub(s.mod) // negative: a time in the future, still being written
		// The newest segment of a folder may still be written even when its time looks old (a
		// writer's time can lag), so it counts as open while it is younger than one segment and a minute.
		newestOpen := i == len(all)-1 && age < time.Duration(o.SegmentSeconds)*time.Second+time.Minute
		if age < o.Quiet || newestOpen {
			l.open = append(l.open, s.name)
			continue
		}
		l.closed = append(l.closed, s)
	}
	sort.Strings(l.open)
	sort.Strings(l.other)
	return l, nil
}

type camCmp struct {
	o   *Options
	rep *CameraReport
}

func (c *camCmp) diff(format string, a ...any) {
	c.rep.Differences = append(c.rep.Differences, fmt.Sprintf(format, a...))
}

func compareCamera(ctx context.Context, o *Options, folder string) (CameraReport, error) {
	rep := CameraReport{Camera: folder, Pairs: []Pair{}, Partials: []Partial{}, UnmatchedOld: []string{}, UnmatchedNew: []string{},
		Differences: []string{}, Notes: []string{}, Old: Side{Open: []string{}, Gaps: []Gap{}}, New: Side{Open: []string{}, Gaps: []Gap{}}}
	c := &camCmp{o: o, rep: &rep}

	oldL, oerr := list(o, o.Old, folder)
	newL, nerr := list(o, o.New, folder)
	if oerr != nil {
		c.diff("the old root has no readable folder %q: %v", folder, oerr)
	}
	if nerr != nil {
		c.diff("the new root has no readable folder %q: %v", folder, nerr)
	}
	if oerr != nil || nerr != nil {
		return rep, nil
	}
	rep.Old.Closed, rep.New.Closed = len(oldL.closed), len(newL.closed)
	rep.Old.Open, rep.New.Open = nonNil(oldL.open), nonNil(newL.open)
	if len(oldL.other) > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("old folder holds %d file(s) that are not segment names (ignored): %s", len(oldL.other), sample(oldL.other)))
	}
	if len(newL.other) > 0 {
		c.diff("the new folder holds %d file(s) that are not segment names of this camera: %s", len(newL.other), sample(newL.other))
	}
	if len(oldL.closed) == 0 || len(newL.closed) == 0 {
		c.diff("nothing to compare: %d closed segment(s) in old, %d in new", len(oldL.closed), len(newL.closed))
		return rep, nil
	}

	// the overlap of the two archives, from the closed segments only
	segSec := o.SegmentSeconds
	ws := later(oldL.closed[0].start, newL.closed[0].start)
	if !o.Since.IsZero() {
		ws = later(ws, o.Since)
	}
	we := earlier(oldL.closed[len(oldL.closed)-1].estimate(segSec), newL.closed[len(newL.closed)-1].estimate(segSec))
	if !we.After(ws.Add(o.StartTolerance)) {
		c.diff("the two archives share no time to compare (old %s to %s, new %s to %s)",
			stamp(oldL.closed[0].start), stamp(oldL.closed[len(oldL.closed)-1].estimate(segSec)), stamp(newL.closed[0].start), stamp(newL.closed[len(newL.closed)-1].estimate(segSec)))
		return rep, nil
	}
	in := func(l []*seg) []*seg {
		var out []*seg
		for _, s := range l {
			if s.estimate(segSec).After(ws.Add(o.StartTolerance)) && s.start.Before(we.Add(-o.StartTolerance)) {
				out = append(out, s)
			}
		}
		return out
	}
	oldW, newW := in(oldL.closed), in(newL.closed)
	rep.Window = &Window{From: ws.Format(time.RFC3339), To: we.Format(time.RFC3339), Seconds: we.Sub(ws).Seconds()}
	rep.Old.Compared, rep.New.Compared = len(oldW), len(newW)
	for _, s := range oldW {
		rep.Old.Bytes += s.size
	}
	for _, s := range newW {
		rep.New.Bytes += s.size
	}

	// ffprobe, one file at a time
	for _, side := range []struct {
		label string
		segs  []*seg
	}{{"old", oldW}, {"new", newW}} {
		for i, s := range side.segs {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			p, err := o.Prober.Probe(ctx, s.path)
			if err != nil {
				if ctx.Err() != nil {
					return rep, ctx.Err()
				}
				s.perr = err.Error()
				s.dur = s.estimate(segSec).Sub(s.start).Seconds()
				c.diff("%s %s cannot be read: %v", side.label, s.name, err)
				continue
			}
			s.probe, s.dur = &p, p.Duration
			if p.NoDuration {
				s.noDur, s.dur = true, s.estimate(segSec).Sub(s.start).Seconds()
			}
			if i%50 == 49 {
				o.Progress(fmt.Sprintf("%s: %s %d/%d segments probed", folder, side.label, i+1, len(side.segs)))
			}
		}
	}

	c.names(oldW, newW)
	c.unfinalized("old", oldW)
	c.unfinalized("new", newW)
	c.pairing(oldW, newW, we)
	c.boundaries("old", oldW)
	c.boundaries("new", newW)
	c.streams("old", oldW)
	c.streams("new", newW)
	c.gaps(oldW, newW)
	if len(rep.Pairs) == 0 && len(rep.Partials) == 0 {
		c.diff("no segment of the old side has a partner on the new side in the window")
	}
	return rep, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func sample(names []string) string {
	if len(names) > 5 {
		return strings.Join(names[:5], ", ") + ", ..."
	}
	return strings.Join(names, ", ")
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func stamp(t time.Time) string { return t.Format("2006-01-02 15:04:05") }

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// unfinalized: a segment whose recorder was killed inside it has no duration (the writer never
// closed the file). That is expected for the last file of a side (a recorder stopped inside it; its
// length is then estimated from its write time) and a fault anywhere else.
func (c *camCmp) unfinalized(label string, segs []*seg) {
	for i, s := range segs {
		switch {
		case !s.noDur:
		case i == len(segs)-1:
			c.rep.Notes = append(c.rep.Notes, fmt.Sprintf("%s %s was not finalized (no duration; its recorder was stopped inside it); its length is estimated from its write time", label, s.name))
		default:
			c.diff("%s %s has no duration: its recorder was killed inside it, and the next segment follows", label, s.name)
		}
	}
}

// names checks the name pattern: the camera prefix is the folder (checked by the listing), the
// offset suffix of two systems on one machine is the same, and no two names of a side are one instant.
func (c *camCmp) names(oldW, newW []*seg) {
	for _, side := range []struct {
		label string
		segs  []*seg
	}{{"old", oldW}, {"new", newW}} {
		seen := map[int64]string{}
		for _, s := range side.segs {
			if prev, dup := seen[s.start.Unix()]; dup {
				c.diff("%s holds two segments that start at the same second: %s and %s", side.label, prev, s.name)
			}
			seen[s.start.Unix()] = s.name
		}
	}
}

func (c *camCmp) pairing(oldW, newW []*seg, we time.Time) {
	o := c.o
	segF := float64(o.SegmentSeconds)
	tol := o.StartTolerance
	used := map[*seg]bool{}
	paired := map[*seg]bool{}
	type pr struct{ o, n *seg }
	var prs []pr
	for _, n := range newW {
		var best *seg
		var bestD time.Duration
		for _, ol := range oldW {
			if used[ol] {
				continue
			}
			if d := absDur(ol.start.Sub(n.start)); d <= tol && (best == nil || d < bestD) {
				best, bestD = ol, d
			}
		}
		if best != nil {
			used[best], paired[n] = true, true
			prs = append(prs, pr{best, n})
		}
	}
	var sizes []float64
	for _, p := range prs {
		oSeg, nSeg := p.o, p.n
		pair := Pair{Old: oSeg.name, New: nSeg.name, Flags: []string{},
			StartDiffSec: round3(nSeg.start.Sub(oSeg.start).Seconds()), OldBytes: oSeg.size, NewBytes: nSeg.size,
			OldDurSec: round3(oSeg.dur), NewDurSec: round3(nSeg.dur), DurationDiffS: round3(nSeg.dur - oSeg.dur),
			OldLayout: layout(oSeg.probe), NewLayout: layout(nSeg.probe)}
		flag := func(format string, a ...any) {
			m := fmt.Sprintf(format, a...)
			pair.Flags = append(pair.Flags, m)
			c.diff("%s / %s: %s", oSeg.name, nSeg.name, m)
		}
		if offsetOf(oSeg.start) != offsetOf(nSeg.start) {
			flag("the time zone suffix of the names differs (%s and %s)", oSeg.start.Format("-0700"), nSeg.start.Format("-0700"))
		}
		dd := math.Abs(oSeg.dur - nSeg.dur)
		switch {
		case oSeg.perr != "" || nSeg.perr != "":
			pair.Kind = "mismatch"
			pair.Flags = append(pair.Flags, "a file could not be read")
		case dd <= o.DurationTolerance.Seconds():
			pair.Kind = "short"
			if oSeg.dur >= segF-tol.Seconds() && nSeg.dur >= segF-tol.Seconds() {
				pair.Kind = "full"
			}
			if oSeg.size > 0 {
				r := round3(float64(nSeg.size) / float64(oSeg.size))
				pair.SizeRatio = &r
				sizes = append(sizes, float64(nSeg.size)/float64(oSeg.size))
				if r < o.SizeMin || r > o.SizeMax {
					flag("size ratio new/old %.3f is outside %.2f to %.2f (%d and %d bytes)", r, o.SizeMin, o.SizeMax, nSeg.size, oSeg.size)
				}
			} else {
				flag("the old file is empty")
			}
		default:
			short := nSeg
			if oSeg.dur < nSeg.dur {
				short = oSeg
			}
			if !short.end().Before(we.Add(-tol)) {
				pair.Kind = "partial_end"
			} else {
				pair.Kind = "mismatch"
				flag("durations differ by %.1f s (old %.1f s, new %.1f s) and the shorter one does not end at the end of the window", dd, oSeg.dur, nSeg.dur)
			}
		}
		if oSeg.probe != nil && nSeg.probe != nil {
			for _, m := range layoutDiff(oSeg.probe, nSeg.probe) {
				flag("%s", m)
			}
		}
		c.rep.Pairs = append(c.rep.Pairs, pair)
	}
	sort.Slice(c.rep.Pairs, func(i, j int) bool { return c.rep.Pairs[i].New < c.rep.Pairs[j].New })
	c.rep.Size = sizeStats(sizes)

	// segments without a start partner: inside a segment of the other side, or really unmatched
	nest := func(s *seg, others []*seg) *seg {
		for _, t := range others {
			if !t.start.After(s.start.Add(tol)) && !s.end().After(t.end().Add(tol)) {
				return t
			}
		}
		return nil
	}
	covered := map[string]bool{} // segments that hold a partial of the other side
	var lonelyNew, lonelyOld []*seg
	for _, s := range newW {
		if paired[s] {
			continue
		}
		if t := nest(s, oldW); t != nil {
			c.rep.Partials = append(c.rep.Partials, partial("new", s, t, tol))
			covered[t.name] = true
		} else {
			lonelyNew = append(lonelyNew, s)
		}
	}
	for _, s := range oldW {
		if used[s] {
			continue
		}
		if t := nest(s, newW); t != nil {
			c.rep.Partials = append(c.rep.Partials, partial("old", s, t, tol))
			covered[t.name] = true
		} else {
			lonelyOld = append(lonelyOld, s)
		}
	}
	for _, s := range lonelyNew {
		if !covered[s.name] {
			c.rep.UnmatchedNew = append(c.rep.UnmatchedNew, s.name)
			c.diff("extra in new, no old segment starts with it: %s", s.name)
		}
	}
	for _, s := range lonelyOld {
		if !covered[s.name] {
			c.rep.UnmatchedOld = append(c.rep.UnmatchedOld, s.name)
			c.diff("missing from new, no new segment starts with it: %s", s.name)
		}
	}
	sort.Slice(c.rep.Partials, func(i, j int) bool { return c.rep.Partials[i].Name < c.rep.Partials[j].Name })
}

func partial(side string, s, within *seg, tol time.Duration) Partial {
	d := s.end().Sub(within.end())
	return Partial{Side: side, Name: s.name, Within: within.name, DurationSec: round3(s.dur), EndDiffSec: round3(d.Seconds()),
		Layout: layout(s.probe), EndsOnBounds: absDur(d) <= tol}
}

func offsetOf(t time.Time) int { _, off := t.Zone(); return off }

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func sizeStats(r []float64) SizeStats {
	st := SizeStats{Pairs: len(r)}
	if len(r) == 0 {
		return st
	}
	s := append([]float64(nil), r...)
	sort.Float64s(s)
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	lo, hi, m := round3(s[0]), round3(s[len(s)-1]), round3(med)
	st.Min, st.Max, st.Median = &lo, &hi, &m
	return st
}

func layoutDiff(o, n *Probe) []string {
	var out []string
	switch {
	case o.Video == nil || n.Video == nil:
		if (o.Video == nil) != (n.Video == nil) {
			out = append(out, "video stream present on only one side")
		}
	case o.Video.Codec != n.Video.Codec || o.Video.Width != n.Video.Width || o.Video.Height != n.Video.Height:
		out = append(out, fmt.Sprintf("video differs: old %s %dx%d, new %s %dx%d", o.Video.Codec, o.Video.Width, o.Video.Height, n.Video.Codec, n.Video.Width, n.Video.Height))
	}
	switch {
	case o.Audio == nil || n.Audio == nil:
		if (o.Audio == nil) != (n.Audio == nil) {
			out = append(out, "audio stream present on only one side")
		}
	case *o.Audio != *n.Audio:
		out = append(out, fmt.Sprintf("audio differs: old %s %d Hz %dch, new %s %d Hz %dch", o.Audio.Codec, o.Audio.SampleRate, o.Audio.Channels, n.Audio.Codec, n.Audio.SampleRate, n.Audio.Channels))
	}
	if o.Audios != n.Audios || o.Videos != n.Videos {
		out = append(out, fmt.Sprintf("stream count differs: old %d video + %d audio, new %d video + %d audio", o.Videos, o.Audios, n.Videos, n.Audios))
	}
	return out
}

// streams checks every probed segment against the expected audio and for a video stream.
func (c *camCmp) streams(label string, segs []*seg) {
	want := c.o.Audio
	for _, s := range segs {
		if s.probe == nil {
			continue
		}
		if s.probe.Video == nil {
			c.diff("%s %s has no video stream", label, s.name)
		}
		if want == "any" {
			continue
		}
		a := s.probe.Audio
		if a == nil {
			c.diff("%s %s has no audio stream (expected %s)", label, s.name, want)
			continue
		}
		if got := fmt.Sprintf("%s:%d:%d", a.Codec, a.SampleRate, a.Channels); got != want {
			c.diff("%s %s has audio %s, expected %s", label, s.name, got, want)
		}
	}
}

// aligned: the name's own clock time is a multiple of the segment length, within the start tolerance.
func aligned(t time.Time, segSeconds int, tol time.Duration) bool {
	_, off := t.Zone()
	rem := (t.Unix() + int64(off)) % int64(segSeconds)
	if rem < 0 {
		rem += int64(segSeconds)
	}
	r := time.Duration(rem) * time.Second
	return r <= tol || time.Duration(segSeconds)*time.Second-r <= tol
}

// boundaries: a segment that follows its predecessor without a gap starts on a clock multiple. The
// first segment of a side and the first one after a gap (a start or restart) may start anywhere.
func (c *camCmp) boundaries(label string, segs []*seg) {
	for i := 1; i < len(segs); i++ {
		s, prev := segs[i], segs[i-1]
		if s.start.Sub(prev.end()) > c.o.GapTolerance {
			continue
		}
		if !aligned(s.start, c.o.SegmentSeconds, c.o.StartTolerance) {
			c.diff("%s %s starts off the %d s clock boundary and follows %s without a gap", label, s.name, c.o.SegmentSeconds, prev.name)
		}
	}
}

func (c *camCmp) gaps(oldW, newW []*seg) {
	find := func(segs []*seg) []Gap {
		out := []Gap{}
		for i := 1; i < len(segs); i++ {
			prev, s := segs[i-1], segs[i]
			g := s.start.Sub(prev.end())
			if g > c.o.GapTolerance {
				out = append(out, Gap{After: prev.name, Before: s.name, Seconds: round3(g.Seconds()), from: prev.end(), to: s.start})
			}
		}
		return out
	}
	og, ng := find(oldW), find(newW)
	overlaps := func(g Gap, others []Gap) bool {
		for _, h := range others {
			if g.from.Before(h.to.Add(c.o.StartTolerance)) && h.from.Before(g.to.Add(c.o.StartTolerance)) {
				return true
			}
		}
		return false
	}
	for i := range og {
		og[i].Shared = overlaps(og[i], ng)
	}
	for i := range ng {
		ng[i].Shared = overlaps(ng[i], og)
	}
	for _, g := range ng {
		if g.Shared {
			c.rep.Notes = append(c.rep.Notes, fmt.Sprintf("new gap of %.1f s after %s is also a gap in old (a camera or relay outage)", g.Seconds, g.After))
		} else {
			c.diff("gap of %.1f s on the new side between %s and %s; the old side has none there", g.Seconds, g.After, g.Before)
		}
	}
	for _, g := range og {
		if g.Shared {
			c.rep.Notes = append(c.rep.Notes, fmt.Sprintf("old gap of %.1f s after %s is also a gap in new", g.Seconds, g.After))
		} else {
			c.diff("gap of %.1f s on the old side between %s and %s; the new side has none there", g.Seconds, g.After, g.Before)
		}
	}
	c.rep.Old.Gaps, c.rep.New.Gaps = og, ng
}

// ResolveCameras maps what the user typed (a camera name or a folder name) to folder names.
func ResolveCameras(names []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		f := n
		if safe, err := archive.SafeName(n); err == nil {
			f = safe
		} else if strings.TrimSpace(n) == "" {
			return nil, fmt.Errorf("an empty --camera")
		}
		if !seen[strings.ToLower(f)] {
			seen[strings.ToLower(f)] = true
			out = append(out, f)
		}
	}
	return out, nil
}
