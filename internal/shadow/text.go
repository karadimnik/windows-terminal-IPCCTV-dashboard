package shadow

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// ParseSince reads --since: an RFC 3339 time, "2006-01-02 15:04[:05]" or "2006-01-02" in loc, or a
// duration such as 36h meaning that long before now.
func ParseSince(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since %q: use a time (2026-10-21 18:30, RFC 3339) or a duration such as 36h", s)
}

func ratio(f *float64) string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%.3f", *f)
}

func short(name string) string {
	// the stamp part of Cam_YYYYMMDD_HHMMSS+ZZZZ.ext is what tells two names apart in a table
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	if len(name) > 20 {
		return name[len(name)-20:]
	}
	return name
}

// Render writes the human report.
func Render(w io.Writer, r Report) {
	fmt.Fprintf(w, "archive compare  old %s  new %s\n", r.Old, r.New)
	fmt.Fprintf(w, "checked %s; segments of %d s; tolerances: start %.0f s, duration %.0f s, gap %.0f s, size ratio %.2f to %.2f; audio %s\n",
		r.CheckedAt, r.Params.SegmentSeconds, r.Params.StartToleranceSec, r.Params.DurationToleranceSec, r.Params.GapToleranceSec, r.Params.SizeRatioMin, r.Params.SizeRatioMax, r.Params.Audio)
	for _, c := range r.Cameras {
		fmt.Fprintf(w, "\n== %s\n", c.Camera)
		if c.Window != nil {
			fmt.Fprintf(w, "window   %s to %s (%.0f s)\n", c.Window.From, c.Window.To, c.Window.Seconds)
		} else {
			fmt.Fprintln(w, "window   none")
		}
		fmt.Fprintf(w, "segments old %d compared of %d closed (%.2f GB), new %d compared of %d closed (%.2f GB); open and skipped: old %d, new %d\n",
			c.Old.Compared, c.Old.Closed, float64(c.Old.Bytes)/(1<<30), c.New.Compared, c.New.Closed, float64(c.New.Bytes)/(1<<30), len(c.Old.Open), len(c.New.Open))
		fmt.Fprintf(w, "pairs    %d start together, %d partial (inside a segment of the other side), unmatched old %d, new %d\n",
			len(c.Pairs), len(c.Partials), len(c.UnmatchedOld), len(c.UnmatchedNew))
		if c.Size.Pairs > 0 {
			fmt.Fprintf(w, "size     new/old over %d pair(s): median %s, min %s, max %s\n", c.Size.Pairs, ratio(c.Size.Median), ratio(c.Size.Min), ratio(c.Size.Max))
		}
		if len(c.Pairs) > 0 {
			fmt.Fprintf(w, "  %-20s %-20s %-11s %7s %8s %9s %9s  %s\n", "OLD", "NEW", "KIND", "DSTART", "RATIO", "DUR OLD", "DUR NEW", "LAYOUT (new)")
			for _, p := range c.Pairs {
				mark := ""
				if len(p.Flags) > 0 {
					mark = "  <-- " + strings.Join(p.Flags, "; ")
				}
				fmt.Fprintf(w, "  %-20s %-20s %-11s %+6.1fs %8s %8.1fs %8.1fs  %s%s\n", short(p.Old), short(p.New), p.Kind, p.StartDiffSec, ratio(p.SizeRatio), p.OldDurSec, p.NewDurSec, p.NewLayout, mark)
			}
		}
		for _, p := range c.Partials {
			fmt.Fprintf(w, "  partial %s %s (%.1f s) lies inside %s; its end is %+.1f s from that segment's end\n", p.Side, p.Name, p.DurationSec, p.Within, p.EndDiffSec)
		}
		fmt.Fprintf(w, "gaps     old %s, new %s\n", gapText(c.Old.Gaps), gapText(c.New.Gaps))
		for _, n := range c.Notes {
			fmt.Fprintf(w, "note     %s\n", n)
		}
		if c.Equal {
			fmt.Fprintln(w, "result   EQUAL within tolerances")
			continue
		}
		fmt.Fprintf(w, "result   %d DIFFERENCE(S)\n", len(c.Differences))
		for _, d := range c.Differences {
			fmt.Fprintf(w, "  - %s\n", d)
		}
	}
	if r.Equal {
		fmt.Fprintf(w, "\nEQUAL: %d camera(s) match within tolerances\n", len(r.Cameras))
	} else {
		fmt.Fprintf(w, "\nDIFFERENT: %d difference(s) over %d camera(s)\n", r.Differences, len(r.Cameras))
	}
}

func gapText(g []Gap) string {
	if len(g) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(g))
	for _, x := range g {
		s := fmt.Sprintf("%.1f s after %s", x.Seconds, short(x.After))
		if x.Shared {
			s += " (shared)"
		}
		parts = append(parts, s)
	}
	return fmt.Sprintf("%d: %s", len(g), strings.Join(parts, "; "))
}
