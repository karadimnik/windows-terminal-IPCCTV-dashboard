package status

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) *Document {
	t.Helper()
	return mustRead(t, filepath.Join(goldenStatus, name))
}

// F1 (KNOWN-DIFFERENCES.md): the PowerShell producer drops LastWindowClose at
// the first full write after a restart; the Go producer carries it (D19).
func TestCarryFromKeepsLastWindowCloseF1(t *testing.T) {
	prev := fixture(t, "patched-windowclose-over-running.json")
	if prev.LastWindowClose == nil || prev.StopKind != StopKindWindowClosed {
		t.Fatal("fixture precondition")
	}
	next := fixture(t, "running-full.json")
	next.LastWindowClose = nil
	next.CarryFrom(prev)
	if next.LastWindowClose == nil || *next.LastWindowClose != *prev.LastWindowClose {
		t.Fatalf("LastWindowClose not carried: %+v", next.LastWindowClose)
	}
	if next.LastWindowClose == prev.LastWindowClose {
		t.Error("carried record shares memory with the previous document")
	}
	ps := fixture(t, "restarted-after-windowclose.json") // what PowerShell wrote: key gone
	if ps.LastWindowClose != nil {
		t.Error("fixture of the PowerShell behaviour changed")
	}
}

func TestCarryFromReemitsUnexpectedStopAtByteForByte(t *testing.T) {
	for _, name := range []string{"stopped-unexpected-by-supervisor.json", "patched-unexpected-over-running.json", "stopped-interrupted-carrying-unexpected.json"} {
		prev := fixture(t, name)
		if prev.LastUnexpectedStop.Stop == nil {
			t.Fatalf("%s: precondition", name)
		}
		next := fixture(t, "running-first-write.json")
		next.LastUnexpectedStop = MaybeStop{}
		next.CarryFrom(prev)

		raw := marshalString(t, next)
		out := filepath.Join(t.TempDir(), "s.json")
		if err := NewWriter(out, Options{}).Write(next); err != nil {
			t.Fatal(err)
		}
		written := string(readFile(t, out))
		wantAt := `"At": "` + atText(t, name) + `"`
		if !strings.Contains(raw, wantAt) || !strings.Contains(written, wantAt) {
			t.Errorf("%s: At text %s not re-emitted", name, wantAt)
		}
		if got := mustRead(t, out).LastUnexpectedStop.Stop; got == nil || got.Reason != prev.LastUnexpectedStop.Stop.Reason || got.SupervisorPid != prev.LastUnexpectedStop.Stop.SupervisorPid {
			t.Errorf("%s: record changed: %+v", name, got)
		}
	}
}

var lastUnexpectedAtRE = regexp.MustCompile(`"LastUnexpectedStop":\s*\{\s*"At":\s*"([^"]+)"`)

// atText is the At string exactly as the fixture file spells it.
func atText(t *testing.T, fixtureName string) string {
	t.Helper()
	m := lastUnexpectedAtRE.FindSubmatch(readFile(t, filepath.Join(goldenStatus, fixtureName)))
	if m == nil {
		t.Fatalf("%s: no LastUnexpectedStop.At", fixtureName)
	}
	return string(m[1])
}

func TestCarryFromRules(t *testing.T) {
	// nothing to carry: key present as null, window close absent
	next := fixture(t, "running-first-write.json")
	next.LastUnexpectedStop = MaybeStop{}
	next.CarryFrom(nil)
	if !next.LastUnexpectedStop.Present || next.LastUnexpectedStop.Stop != nil || next.LastWindowClose != nil {
		t.Errorf("nil prev: %+v", next)
	}
	if !strings.Contains(marshalString(t, next), `"LastUnexpectedStop": null`) {
		t.Error("key not emitted as null")
	}

	// a newer notice in the new document wins
	prev := fixture(t, "stopped-unexpected-by-supervisor.json")
	fresh := &UnexpectedStop{Reason: "new", SupervisorPid: 1, DetectedBy: "supervisor"}
	next = fixture(t, "running-first-write.json")
	next.LastUnexpectedStop = MaybeStop{Present: true, Stop: fresh}
	next.CarryFrom(prev)
	if next.LastUnexpectedStop.Stop != fresh {
		t.Error("carry overwrote a newer notice")
	}

	// carrying twice is stable (restart after restart)
	again := fixture(t, "running-first-write.json")
	again.LastUnexpectedStop = MaybeStop{}
	again.CarryFrom(prev)
	third := fixture(t, "running-first-write.json")
	third.LastUnexpectedStop = MaybeStop{}
	third.CarryFrom(again)
	if marshalString(t, again) != marshalString(t, third) {
		t.Error("second carry changed the notice")
	}
}

func TestStopKindWindowClosedIsAccepted(t *testing.T) {
	doc := fixture(t, "patched-windowclose-minimal.json")
	if doc.State != StateStopped || doc.StopKind != StopKindWindowClosed {
		t.Fatalf("fixture: %s %s", doc.State, doc.StopKind)
	}
	out := filepath.Join(t.TempDir(), "s.json")
	if err := NewWriter(out, Options{}).Write(doc); err != nil {
		t.Fatal(err)
	}
	if vs, err := Validate(readFile(t, out)); err != nil || len(vs) != 0 {
		t.Errorf("window-closed document rejected: %v %v", violationKeys(vs), err)
	}
	if got := mustRead(t, out); got.StopKind != "window-closed" || got.LastWindowClose == nil {
		t.Errorf("round trip: %+v", got)
	}
}
