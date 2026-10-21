package shadow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const camera = "Front_Door"

func hasDiff(rep CameraReport, sub string) bool {
	for _, d := range rep.Differences {
		if strings.Contains(d, sub) {
			return true
		}
	}
	return false
}

func TestCompareTable(t *testing.T) {
	// the 12:45 file of the old side is still being written in the two "open" cases
	tests := []struct {
		name      string
		old, new  []spec
		opts      func(*Options)
		wantEqual bool
		wantDiffs []string // every substring must appear in some difference
		wantPairs int
		wantPart  int
	}{
		{
			name: "identical", wantEqual: true, wantPairs: 4,
			old: series(at(12, 0, 0), 4, 900, 0), new: series(at(12, 0, 0), 4, 900, 0),
		},
		{
			// the two recorders open each file a second or two after the boundary, not in the same second
			name: "start jitter inside the tolerance", wantEqual: true, wantPairs: 4,
			old: series(at(12, 0, 0), 4, 900, 1), new: series(at(12, 0, 0), 4, 900, 3),
		},
		{
			name: "a segment missing from new", wantPairs: 4,
			old:       series(at(12, 0, 0), 5, 900, 0),
			new:       append(series(at(12, 0, 0), 2, 900, 0), series(at(12, 45, 0), 2, 900, 0)...),
			wantDiffs: []string{"missing from new", "gap of 900.0 s on the new side"},
		},
		{
			name: "an extra segment in new", wantPairs: 4,
			old:       append(series(at(12, 0, 0), 2, 900, 0), series(at(12, 45, 0), 2, 900, 0)...),
			new:       series(at(12, 0, 0), 5, 900, 0),
			wantDiffs: []string{"extra in new", "gap of 900.0 s on the old side"},
		},
		{
			name: "an extra gap in new", wantPairs: 3, wantPart: 1,
			old:       series(at(12, 0, 0), 4, 900, 0),
			new:       []spec{{start: at(12, 0, 0)}, {start: at(12, 15, 20), dur: 880}, {start: at(12, 30, 0)}, {start: at(12, 45, 0)}},
			wantDiffs: []string{"gap of 20.0 s on the new side"},
		},
		{
			name: "a different size", wantPairs: 3,
			old:       series(at(12, 0, 0), 3, 900, 0),
			new:       []spec{{start: at(12, 0, 0)}, {start: at(12, 15, 0), size: 150000 * 900 * 13 / 10}, {start: at(12, 30, 0)}},
			wantDiffs: []string{"size ratio new/old 1.300 is outside 0.90 to 1.10"},
		},
		{
			name: "a size ratio at the edge passes", wantEqual: true, wantPairs: 2,
			old: series(at(12, 0, 0), 2, 900, 0),
			new: []spec{{start: at(12, 0, 0), size: 150000 * 900 * 109 / 100}, {start: at(12, 15, 0), size: 150000 * 900 * 91 / 100}},
		},
		{
			name: "no audio in new", wantPairs: 2,
			old:       series(at(12, 0, 0), 2, 900, 0),
			new:       []spec{{start: at(12, 0, 0)}, {start: at(12, 15, 0), noAudio: true}},
			wantDiffs: []string{"audio stream present on only one side", "new Front_Door_20261021_121500+0300.mkv has no audio stream"},
		},
		{
			name: "no audio on either side is still a difference", wantPairs: 1,
			old: []spec{{start: at(12, 0, 0), noAudio: true}}, new: []spec{{start: at(12, 0, 0), noAudio: true}},
			wantDiffs: []string{"has no audio stream (expected pcm_alaw:8000:1)"},
		},
		{
			name: "no audio on either side passes with --audio any", wantEqual: true, wantPairs: 1,
			old: []spec{{start: at(12, 0, 0), noAudio: true}}, new: []spec{{start: at(12, 0, 0), noAudio: true}},
			opts: func(o *Options) { o.Audio = "any" },
		},
		{
			name: "another audio format", wantPairs: 1,
			old: []spec{{start: at(12, 0, 0)}}, new: []spec{{start: at(12, 0, 0), audio: &Stream{Codec: "aac", SampleRate: 16000, Channels: 2}}},
			wantDiffs: []string{"audio differs: old pcm_alaw 8000 Hz 1ch, new aac 16000 Hz 2ch", "has audio aac:16000:2, expected pcm_alaw:8000:1"},
		},
		{
			name: "boundaries of another length", wantPart: 2,
			old:       series(at(12, 0, 0), 4, 900, 0),
			new:       series(at(12, 0, 0), 6, 600, 0),
			wantDiffs: []string{"starts off the 900 s clock boundary"},
		},
		{
			// the shadow was started at 12:07:30: its first file is partial and ends on the old file's boundary
			name: "a partial first segment", wantEqual: true, wantPairs: 2, wantPart: 1,
			old: series(at(12, 0, 0), 3, 900, 0),
			new: []spec{{start: at(12, 7, 30), dur: 450}, {start: at(12, 15, 1), dur: 899}, {start: at(12, 30, 1), dur: 899}},
		},
		{
			// the shadow was stopped at 12:20:01, inside the 12:15 file
			name: "a partial last segment", wantEqual: true, wantPairs: 2,
			old: series(at(12, 0, 0), 3, 900, 0),
			new: []spec{{start: at(12, 0, 1), dur: 899}, {start: at(12, 15, 1), dur: 300}},
		},
		{
			name: "a short segment in the middle is a difference", wantPairs: 4,
			old:       series(at(12, 0, 0), 4, 900, 0),
			new:       []spec{{start: at(12, 0, 0)}, {start: at(12, 15, 0), dur: 300}, {start: at(12, 30, 0)}, {start: at(12, 45, 0)}},
			wantDiffs: []string{"durations differ by 600.0 s"},
		},
		{
			name: "durations inside the tolerance", wantEqual: true, wantPairs: 2,
			old: []spec{{start: at(12, 0, 0), dur: 900.5}, {start: at(12, 15, 0), dur: 901.5}},
			new: []spec{{start: at(12, 0, 0), dur: 900}, {start: at(12, 15, 0), dur: 900}},
		},
		{
			name: "durations outside the tolerance", wantPairs: 3,
			old:       series(at(12, 0, 0), 3, 900, 0),
			new:       []spec{{start: at(12, 0, 0)}, {start: at(12, 15, 0), dur: 897}, {start: at(12, 30, 0)}},
			wantDiffs: []string{"durations differ by 3.0 s"},
		},
		{
			name: "a restart inside a segment is a gap and a partial", wantPairs: 3, wantPart: 1,
			old:       series(at(12, 0, 0), 3, 900, 0),
			new:       []spec{{start: at(12, 0, 0)}, {start: at(12, 15, 0), dur: 300}, {start: at(12, 20, 12), dur: 588}, {start: at(12, 30, 0)}},
			wantDiffs: []string{"gap of 12.0 s on the new side"},
		},
		{
			name: "the same instant under another zone suffix", wantPairs: 2,
			old:       []spec{{start: at(12, 0, 0)}, {start: at(12, 15, 0)}},
			new:       []spec{{start: at(12, 0, 0).In(zone2)}, {start: at(12, 15, 0)}},
			wantDiffs: []string{"time zone suffix of the names differs (+0300 and +0200)"},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			sb.putAll(sb.old, camera, tc.old)
			sb.putAll(sb.new, camera, tc.new)
			o := sb.options()
			if tc.opts != nil {
				tc.opts(&o)
			}
			rep, err := Compare(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			c := rep.Cameras[0]
			if c.Equal != tc.wantEqual || rep.Equal != tc.wantEqual || (rep.Differences == 0) != tc.wantEqual {
				t.Fatalf("equal = %v, want %v\ndifferences: %q\nnotes: %q", c.Equal, tc.wantEqual, c.Differences, c.Notes)
			}
			for _, w := range tc.wantDiffs {
				if !hasDiff(c, w) {
					t.Errorf("no difference contains %q; have %q", w, c.Differences)
				}
			}
			if tc.wantPairs != 0 && len(c.Pairs) != tc.wantPairs {
				t.Errorf("pairs = %d, want %d: %+v", len(c.Pairs), tc.wantPairs, c.Pairs)
			}
			if len(c.Partials) != tc.wantPart {
				t.Errorf("partials = %d, want %d: %+v", len(c.Partials), tc.wantPart, c.Partials)
			}
		})
	}
}

func TestPartialLastIsPartialEnd(t *testing.T) {
	sb := newSandbox(t)
	sb.putAll(sb.old, camera, series(at(12, 0, 0), 3, 900, 0))
	sb.putAll(sb.new, camera, []spec{{start: at(12, 0, 1), dur: 899}, {start: at(12, 15, 1), dur: 300}})
	rep, _ := Compare(context.Background(), sb.options())
	c := rep.Cameras[0]
	if len(c.Pairs) != 2 || c.Pairs[1].Kind != "partial_end" || c.Pairs[1].SizeRatio != nil || c.Pairs[0].Kind != "full" {
		t.Fatalf("%+v", c.Pairs)
	}
	if c.Size.Pairs != 1 {
		t.Errorf("only the full pair has a size ratio: %+v", c.Size)
	}
}

func TestOpenSegmentsAreSkippedAndNeverProbed(t *testing.T) {
	sb := newSandbox(t)
	sb.putAll(sb.old, camera, series(at(12, 0, 0), 3, 900, 0))
	sb.putAll(sb.new, camera, series(at(12, 0, 0), 3, 900, 0))
	// the newest file of each side was written a few seconds ago: it is open
	open := testNow.Add(-30 * time.Second)
	sb.put(sb.old, camera, spec{start: at(12, 45, 0), mod: open})
	sb.put(sb.new, camera, spec{start: at(12, 45, 0), mod: open})
	rep, err := Compare(context.Background(), sb.options())
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Cameras[0]
	if !c.Equal || len(c.Pairs) != 3 || c.Old.Closed != 3 || len(c.Old.Open) != 1 || len(c.New.Open) != 1 {
		t.Fatalf("%+v", c)
	}
	for _, p := range sb.fp.log {
		if strings.Contains(p, "124500") {
			t.Errorf("an open segment was probed: %s", p)
		}
	}
	if len(sb.fp.log) != 6 {
		t.Errorf("probed %d files, want 6 (once each)", len(sb.fp.log))
	}
}

func TestOpenRules(t *testing.T) {
	closedOf := func(mods ...time.Duration) (closed int, open []string) {
		sb := newSandbox(t)
		for i, ago := range mods {
			sb.put(sb.old, camera, spec{start: at(16, 0, 0).Add(time.Duration(i) * 15 * time.Minute), mod: testNow.Add(-ago)})
		}
		sb.put(sb.new, camera, spec{start: at(12, 0, 0)})
		sb.put(sb.new, camera, spec{start: at(12, 15, 0)})
		rep, err := Compare(context.Background(), sb.options())
		if err != nil {
			t.Fatal(err)
		}
		return rep.Cameras[0].Old.Closed, rep.Cameras[0].Old.Open
	}
	// not the newest: the 90 second rule alone decides
	if c, o := closedOf(89*time.Second, 2*time.Hour); c != 1 || len(o) != 1 {
		t.Errorf("89 s on an older file is open: closed %d open %v", c, o)
	}
	if c, o := closedOf(91*time.Second, 2*time.Hour); c != 2 || len(o) != 0 {
		t.Errorf("91 s on an older file is closed: closed %d open %v", c, o)
	}
	// the newest file of a folder stays open for a segment and a minute even when its time looks old
	if c, o := closedOf(2*time.Hour, 5*time.Minute); c != 1 || len(o) != 1 {
		t.Errorf("newest written 5 min ago: closed %d open %v", c, o)
	}
	if c, o := closedOf(2*time.Hour, 17*time.Minute); c != 2 || len(o) != 0 {
		t.Errorf("newest written 17 min ago (900 s + 60 s have passed): closed %d open %v", c, o)
	}
}

func TestOutsideTheOverlapIsNotCompared(t *testing.T) {
	sb := newSandbox(t)
	// old has two days; the shadow only the last hour
	sb.putAll(sb.old, camera, series(at(9, 0, 0), 12, 900, 0))
	sb.putAll(sb.new, camera, series(at(11, 0, 0), 4, 900, 0))
	rep, _ := Compare(context.Background(), sb.options())
	c := rep.Cameras[0]
	if !c.Equal || len(c.Pairs) != 4 || c.Old.Closed != 12 || c.Old.Compared != 4 {
		t.Fatalf("%+v", c)
	}
	if c.Window.From != at(11, 0, 0).Format(time.RFC3339) || c.Window.To != at(12, 0, 0).Format(time.RFC3339) {
		t.Errorf("window %+v", c.Window)
	}
	if len(sb.fp.log) != 8 {
		t.Errorf("probed %d files, want 8", len(sb.fp.log))
	}
}

func TestSinceNarrowsTheWindow(t *testing.T) {
	sb := newSandbox(t)
	sb.putAll(sb.old, camera, series(at(12, 0, 0), 6, 900, 0))
	n := series(at(12, 0, 0), 6, 900, 0)
	n[1].size = 150000 * 900 * 2 // a bad pair early in the run
	sb.putAll(sb.new, camera, n)
	o := sb.options()
	rep, _ := Compare(context.Background(), o)
	if rep.Equal {
		t.Fatal("the bad pair should be seen")
	}
	o.Since = at(12, 30, 0)
	rep, _ = Compare(context.Background(), o)
	c := rep.Cameras[0]
	if !c.Equal || len(c.Pairs) != 4 {
		t.Fatalf("%q %+v", c.Differences, c.Pairs)
	}
}

func TestNoOverlapAndEmptySidesAreDifferences(t *testing.T) {
	sb := newSandbox(t)
	sb.putAll(sb.old, camera, series(at(8, 0, 0), 2, 900, 0))
	sb.putAll(sb.new, camera, series(at(12, 0, 0), 2, 900, 0))
	rep, _ := Compare(context.Background(), sb.options())
	if rep.Equal || !hasDiff(rep.Cameras[0], "share no time to compare") || rep.Cameras[0].Window != nil {
		t.Fatalf("%+v", rep.Cameras[0])
	}
	// only open files on the new side: nothing closed, never "equal"
	sb2 := newSandbox(t)
	sb2.putAll(sb2.old, camera, series(at(12, 0, 0), 2, 900, 0))
	sb2.put(sb2.new, camera, spec{start: at(12, 0, 0), mod: testNow})
	rep, _ = Compare(context.Background(), sb2.options())
	if rep.Equal || !hasDiff(rep.Cameras[0], "nothing to compare: 2 closed segment(s) in old, 0 in new") {
		t.Fatalf("%+v", rep.Cameras[0].Differences)
	}
}

func TestUnreadableFileIsADifference(t *testing.T) {
	sb := newSandbox(t)
	sb.putAll(sb.old, camera, series(at(12, 0, 0), 3, 900, 0))
	paths := []string{}
	for _, sp := range series(at(12, 0, 0), 3, 900, 0) {
		paths = append(paths, sb.put(sb.new, camera, sp))
	}
	sb.fp.errs[paths[1]] = errors.New("ffprobe: invalid data")
	rep, _ := Compare(context.Background(), sb.options())
	c := rep.Cameras[0]
	if c.Equal || !hasDiff(c, "cannot be read: ffprobe: invalid data") {
		t.Fatalf("%q", c.Differences)
	}
}

func TestForeignFilesAndMissingFolders(t *testing.T) {
	sb := newSandbox(t)
	sb.putAll(sb.old, camera, series(at(12, 0, 0), 2, 900, 0))
	sb.putAll(sb.new, camera, series(at(12, 0, 0), 2, 900, 0))
	os.WriteFile(filepath.Join(sb.old, camera, "notes.txt"), []byte("x"), 0o644)
	rep, _ := Compare(context.Background(), sb.options())
	c := rep.Cameras[0]
	if !c.Equal || len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "notes.txt") {
		t.Fatalf("a foreign file in old is a note: %+v", c)
	}
	os.WriteFile(filepath.Join(sb.new, camera, "Front_Door_%Y%m%d_%H%M%S%z.mkv"), []byte("x"), 0o644) // an unexpanded pattern
	rep, _ = Compare(context.Background(), sb.options())
	if rep.Equal || !hasDiff(rep.Cameras[0], "not segment names of this camera") {
		t.Fatalf("a foreign file in new is a difference: %+v", rep.Cameras[0].Differences)
	}

	// a camera the new root has and the old does not
	sb.put(sb.new, "Back_Door", spec{start: at(12, 0, 0)})
	rep, _ = Compare(context.Background(), sb.options())
	if len(rep.Cameras) != 2 || rep.Cameras[0].Camera != "Back_Door" || rep.Cameras[0].Equal || !hasDiff(rep.Cameras[0], `the old root has no readable folder "Back_Door"`) {
		t.Fatalf("%+v", rep.Cameras)
	}
	// --camera narrows the set
	os.Remove(filepath.Join(sb.new, camera, "Front_Door_%Y%m%d_%H%M%S%z.mkv"))
	o := sb.options()
	o.Cameras = []string{camera}
	rep, _ = Compare(context.Background(), o)
	if len(rep.Cameras) != 1 || !rep.Equal {
		t.Fatalf("%+v", rep)
	}
}

func TestClockChangeNames(t *testing.T) {
	// the fall-back night: wall clock 04:00 +0300 becomes 03:00 +0200; the names go +0300 then +0200
	// and sort wrongly as text, but the instants are consecutive
	mk := func(h, m, off int) time.Time {
		return time.Date(2026, 10, 25, h, m, 0, 0, time.FixedZone("", off*3600))
	}
	segs := []spec{{start: mk(3, 30, 3)}, {start: mk(3, 45, 3)}, {start: mk(3, 0, 2)}, {start: mk(3, 15, 2)}, {start: mk(3, 30, 2)}, {start: mk(3, 45, 2)}}
	sb := newSandbox(t)
	sb.putAll(sb.old, camera, segs)
	sb.putAll(sb.new, camera, segs)
	o := sb.options()
	o.Now = time.Date(2026, 10, 26, 0, 0, 0, 0, zone2)
	rep, err := Compare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Cameras[0]
	if !c.Equal || len(c.Pairs) != 6 || len(c.Old.Gaps) != 0 || len(c.New.Gaps) != 0 {
		t.Fatalf("%q %+v", c.Differences, c)
	}
	// negative control: the shadow dropped the 03:00 +0200 file
	sb2 := newSandbox(t)
	sb2.putAll(sb2.old, camera, segs)
	sb2.putAll(sb2.new, camera, append(append([]spec{}, segs[:2]...), segs[3:]...))
	rep, _ = Compare(context.Background(), o2(sb2, o.Now))
	if rep.Equal || !hasDiff(rep.Cameras[0], "missing from new") || !hasDiff(rep.Cameras[0], "gap of 900.0 s on the new side") {
		t.Fatalf("%q", rep.Cameras[0].Differences)
	}
}

func o2(sb *sandbox, now time.Time) Options {
	o := sb.options()
	o.Now = now
	return o
}

func TestSharedOutageIsANoteNotADifference(t *testing.T) {
	sb := newSandbox(t)
	segs := []spec{{start: at(12, 0, 0)}, {start: at(12, 30, 0)}, {start: at(12, 45, 0)}}
	sb.putAll(sb.old, camera, segs)
	sb.putAll(sb.new, camera, segs)
	rep, _ := Compare(context.Background(), sb.options())
	c := rep.Cameras[0]
	if !c.Equal || len(c.Old.Gaps) != 1 || !c.Old.Gaps[0].Shared || !c.New.Gaps[0].Shared || len(c.Notes) != 2 {
		t.Fatalf("%q %+v", c.Differences, c)
	}
}

func TestResolveCameras(t *testing.T) {
	got, err := ResolveCameras([]string{"Front Door", "front door", "Back_Door"})
	if err != nil || !reflect.DeepEqual(got, []string{"Front_Door", "Back_Door"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ResolveCameras([]string{" "}); err == nil {
		t.Error("an empty name is refused")
	}
}

func TestCompareRefusals(t *testing.T) {
	sb := newSandbox(t)
	for name, o := range map[string]Options{
		"missing old": {Old: filepath.Join(sb.old, "nope"), New: sb.new},
		"same folder": {Old: sb.old, New: sb.old},
		"no cameras":  {Old: sb.old, New: sb.new},
	} {
		o.Prober = sb.fp
		if _, err := Compare(context.Background(), o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	o := sb.options()
	o.Prober = nil
	if _, err := Compare(context.Background(), o); err == nil {
		t.Error("no prober")
	}
}

func TestCompareOnlyReads(t *testing.T) {
	sb := newSandbox(t)
	sb.putAll(sb.old, camera, series(at(12, 0, 0), 4, 900, 0))
	sb.putAll(sb.new, camera, series(at(12, 0, 0), 4, 905, 2)) // differences on purpose
	before := snapshot(t, sb.old, sb.new)
	if _, err := Compare(context.Background(), sb.options()); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, sb.old, sb.new); !reflect.DeepEqual(before, after) {
		t.Fatalf("the trees changed:\nbefore %v\nafter  %v", before, after)
	}
}

func snapshot(t *testing.T, roots ...string) []string {
	var out []string
	for _, r := range roots {
		filepath.Walk(r, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				out = append(out, p+"|"+fi.ModTime().String()+"|"+string(rune(fi.Size()%1000)))
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 21, 18, 0, 0, 0, zone3)
	for in, want := range map[string]time.Time{
		"2026-10-21T12:00:00+03:00": time.Date(2026, 10, 21, 12, 0, 0, 0, zone3),
		"2026-10-21 12:30":          time.Date(2026, 10, 21, 12, 30, 0, 0, zone3),
		"2026-10-20":                time.Date(2026, 10, 20, 0, 0, 0, 0, zone3),
		"36h":                       now.Add(-36 * time.Hour),
	} {
		got, err := ParseSince(in, now, zone3)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "yesterday", "-3h", "2026-13-01"} {
		if _, err := ParseSince(bad, now, zone3); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestAligned(t *testing.T) {
	for _, tc := range []struct {
		t    time.Time
		want bool
	}{
		{at(12, 15, 0), true}, {at(12, 15, 4), true}, {at(12, 14, 57), true}, {at(12, 15, 6), false}, {at(12, 7, 30), false},
		{time.Date(2026, 10, 21, 12, 15, 0, 0, time.FixedZone("", 5*3600+1800)), true}, // +0530: wall-clock multiples
	} {
		if got := aligned(tc.t, 900, 5*time.Second); got != tc.want {
			t.Errorf("%s: %v", tc.t, got)
		}
	}
}
