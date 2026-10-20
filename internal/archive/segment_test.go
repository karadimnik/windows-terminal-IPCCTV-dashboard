package archive

import (
	"strings"
	"testing"
	"time"
)

type segGolden struct {
	Containers []struct {
		Container, Extension string
		Names                []struct {
			SafeName, Pattern string
			Samples           []struct{ Local, Offset, UTC, Name string }
			Accept, Reject    []struct {
				Name, Why string
				Trap      bool
			}
		}
	}
}

// Walks testdata/golden/names/segments.json. Non-trap accept rows must be accepted, reject rows
// and - DECISION (advisor, W2): the Go allowlist is stricter than PowerShell - the trap rows (a
// trailing newline, Arabic-Indic and fullwidth digits, which PowerShell accepts) must be REJECTED.
func TestSegmentsGolden(t *testing.T) {
	var d segGolden
	golden(t, "names/segments.json", &d)
	var traps, accepted, rejected, samples int
	for _, c := range d.Containers {
		for _, n := range c.Names {
			percent := strings.Contains(n.SafeName, "%")
			for _, s := range n.Samples {
				samples++
				local, err := time.Parse("2006-01-02 15:04:05", s.Local)
				if err != nil {
					t.Fatal(err)
				}
				utc, _ := time.Parse("2006-01-02 15:04:05", s.UTC)
				if !percent && s.Name != n.SafeName+"_"+local.Format("20060102_150405")+s.Offset+c.Extension {
					t.Errorf("sample %q does not fit the name layout", s.Name)
				}
				if percent {
					continue // the PowerShell product splices the offset into the prefix; see TestPercentInName
				}
				cam, start, ok := ParseSegment(s.Name)
				if !ok || cam != n.SafeName || !start.UTC().Equal(utc) || start.Format("2006-01-02 15:04:05") != s.Local {
					t.Errorf("ParseSegment(%q) = %q, %v, %v; want %q at %s", s.Name, cam, start, ok, n.SafeName, s.UTC)
				}
				if got := FormatSegmentName(n.SafeName, start, c.Extension); got != s.Name {
					t.Errorf("FormatSegmentName = %q, want %q", got, s.Name)
				}
			}
			if !percent && n.Pattern != FormatSegmentPattern(n.SafeName)+c.Extension {
				t.Errorf("pattern %q, golden %q", FormatSegmentPattern(n.SafeName)+c.Extension, n.Pattern)
			}
			for _, m := range []Matcher{{FoldCase: true}, {FoldCase: false}} { // Windows and Linux/macOS
				for _, a := range n.Accept {
					got := m.IsSegmentOf(a.Name, n.SafeName, c.Extension)
					differentCase := strings.Contains(a.Why, "lower case") || strings.Contains(a.Why, "upper case")
					switch {
					case a.Trap:
						if m.FoldCase {
							traps++
						}
						if got || m.IsSegmentName(a.Name, c.Extension) {
							t.Errorf("%v %s: TRAP %q (%s) must be rejected by the strict Go allowlist", m, n.SafeName, a.Name, a.Why)
						}
					case percent && !strings.Contains(a.Name, "%"):
						// PowerShell accepts the offset-spliced prefix here (it treats %z in the camera name as a
						// wildcard); Go matches the prefix literally. Covered by TestPercentInName.
						if got {
							t.Errorf("%s: %q accepted although its prefix is not the safe name", n.SafeName, a.Name)
						}
					case differentCase && !m.FoldCase: // case-sensitive filesystems: a differently-cased file is foreign
						if got {
							t.Errorf("case-sensitive matcher accepted %q", a.Name)
						}
					default:
						if m.FoldCase {
							accepted++
						}
						if !got {
							t.Errorf("%v %s: %q (%s) must be accepted", m, n.SafeName, a.Name, a.Why)
						}
					}
				}
			}
			for _, r := range n.Reject {
				rejected++
				for _, m := range []Matcher{{FoldCase: true}, {FoldCase: false}} {
					if m.IsSegmentOf(r.Name, n.SafeName, c.Extension) {
						t.Errorf("%v %s: %q (%s) must be rejected", m, n.SafeName, r.Name, r.Why)
					}
				}
			}
		}
	}
	if traps != 24 || samples != 56 || accepted != 18 || rejected != 152 {
		t.Errorf("walked %d traps, %d samples, %d accepted, %d rejected: the golden changed or the walk is broken", traps, samples, accepted, rejected)
	}
}

func TestTrapsAreNotFalseNegatives(t *testing.T) {
	// Negative control: the same names minus the trap, as the real ffmpeg writes them, are accepted.
	good := "Camera_One_20261025_033000+0300.mkv"
	if !IsSegmentOf(good, "Camera_One", ".mkv") {
		t.Fatal("the control name must be accepted")
	}
	for _, bad := range []string{
		good + "\n", good + "\r\n", good + "\x00",
		"Camera_One_٢٠٢٦" + "1025_033000+0300.mkv", // Arabic-Indic year
		"Camera_One_２０２６" + "1025_033000+0300.mkv", // fullwidth year
		"Camera_One_20261025_033000+٠٣٠٠.mkv",      // Arabic-Indic offset
		"Camera_One_20261025_٠٣٠٠٠٠+0300.mkv",      // Arabic-Indic time
	} {
		if IsSegmentOf(bad, "Camera_One", ".mkv") || IsSegmentName(bad, ".mkv") {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestSegmentShapeAndRealDates(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Cam_20261025_033000+0300.mkv", true},
		{"Cam_20261025_033000-0500.mkv", true},
		{"Cam_20261025_033000+0000.mkv", true},
		{"Cam_20261025_033000+1400.mkv", true},
		{"Cam_20240229_000000+0200.mkv", true},  // leap day
		{"Cam_20261325_033000+0300.mkv", false}, // month 13
		{"Cam_20260230_033000+0300.mkv", false}, // 30 February
		{"Cam_20250229_033000+0300.mkv", false}, // not a leap year
		{"Cam_20261025_243000+0300.mkv", false}, // hour 24
		{"Cam_20261025_036000+0300.mkv", false}, // minute 60
		{"Cam_20261025_033060+0300.mkv", false}, // second 60
		{"Cam_20261025_033000+1500.mkv", false}, // offset beyond +14:00
		{"Cam_20261025_033000+0360.mkv", false}, // offset minutes 60
		{"_20261025_033000+0300.mkv", false},    // no camera
		{"Cam_20261025_033000+0300.", false},    // empty extension
		{"Cam_20261025_033000+0300", false},     // no extension
		{"Cam_20261025_033000+0300.mkv.tmp", false},
		{"Cam_20261025_033000+0300.mp4", false},   // other container
		{"a/Cam_20261025_033000+0300.mkv", false}, // separator in the prefix
		{"a\\Cam_20261025_033000+0300.mkv", false},
		{"Cam 20261025_033000+0300.mkv", false},
	}
	for _, c := range cases {
		if got := IsSegmentName(c.name, ".mkv"); got != c.want {
			t.Errorf("IsSegmentName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
	for _, m := range []Matcher{{FoldCase: true}, {FoldCase: false}} { // both OS rules on any host
		if got := m.IsSegmentName("cam_20261025_033000+0300.MKV", ".mkv"); got != m.FoldCase {
			t.Errorf("%v: case-differing extension accepted=%v", m, got)
		}
		if got := m.IsSegmentOf("CAM_20261025_033000+0300.mkv", "Cam", ".mkv"); got != m.FoldCase {
			t.Errorf("%v: case-differing prefix accepted=%v", m, got)
		}
		if !m.IsSegmentOf("Cam_20261025_033000+0300.mkv", "Cam", ".mkv") {
			t.Errorf("%v: exact name refused", m)
		}
	}
	// the container may be given without its dot
	if !IsSegmentName("Cam_20261025_033000+0300.mkv", "mkv") || IsSegmentName("Cam_20261025_033000+0300.mkv", "") {
		t.Error("container normalisation")
	}
	// the camera must be the folder's: another camera, a leading underscore, a prefix sibling
	for _, other := range []string{"Zam", "Cam2", "_Cam", "Ca"} {
		if IsSegmentOf(other+"_20261025_033000+0300.mkv", "Cam", ".mkv") {
			t.Errorf("%s accepted as Cam", other)
		}
	}
	if IsSegmentOf("Cam_20261025_033000+0300.mkv", "", ".mkv") {
		t.Error("an empty safe name matches nothing")
	}
}

func TestParseSegmentKeepsWrittenOffset(t *testing.T) {
	cam, start, ok := ParseSegment("Front_Door_20261025_033000+0200.mkv")
	if !ok || cam != "Front_Door" {
		t.Fatal(cam, ok)
	}
	if _, off := start.Zone(); off != 7200 || start.UTC().Format("2006-01-02T15:04:05Z") != "2026-10-25T01:30:00Z" {
		t.Errorf("start %v", start)
	}
	// the repeated hour: same wall time, offsets tell the two passes apart, and the later pass sorts EARLIER by name
	_, first, _ := ParseSegment("C_20261025_033000+0300.mkv")
	_, second, _ := ParseSegment("C_20261025_033000+0200.mkv")
	if !second.After(first) {
		t.Errorf("first %v second %v", first, second)
	}
	if !("C_20261025_033000+0200.mkv" < "C_20261025_033000+0300.mkv") {
		t.Error("control: the later pass sorts earlier by name")
	}
}

func TestPercentInName(t *testing.T) {
	if got := FormatSegmentPattern("Cam%z"); got != "Cam%%z_%Y%m%d_%H%M%S%z" {
		t.Errorf("pattern %q", got)
	}
	if !IsSegmentOf("Cam%z_20261025_033000+0300.mkv", "Cam%z", ".mkv") {
		t.Error("the literal name must be accepted")
	}
	if IsSegmentOf("Cam+0300_20261025_033000+0300.mkv", "Cam%z", ".mkv") {
		t.Error("the PowerShell offset-spliced prefix must not match the literal name")
	}
}
