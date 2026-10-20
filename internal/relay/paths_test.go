package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/config"
)

// Every row of the PowerShell corpus: Get-CamSafeName then Get-CamRelayPathName.
func TestPathNameMatchesTheCorpus(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(goldenRoot, "names", "safe-names.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Cases []struct {
			ID, Name    string
			OK          bool    `json:"ok"`
			RelayPath   *string `json:"relayPath"`
			RelayPathOK *bool   `json:"relayPathOk"`
		} `json:"cases"`
		Collisions []struct {
			RelayPath string
			IDs       []string
		} `json:"relayPathCollisions"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Cases) < 55 {
		t.Fatalf("%d cases", len(d.Cases))
	}
	byID := map[string]string{}
	valid, invalid := 0, 0
	for _, c := range d.Cases {
		byID[c.ID] = c.Name
		got, err := PathName(c.Name)
		if c.RelayPathOK != nil && *c.RelayPathOK {
			valid++
			if err != nil || got != *c.RelayPath {
				t.Errorf("%s (%q): got %q, %v; PowerShell says %q", c.ID, c.Name, got, err, *c.RelayPath)
			}
			continue
		}
		invalid++
		if err == nil {
			t.Errorf("%s (%q): got %q, PowerShell refuses the name", c.ID, c.Name, got)
		}
	}
	if valid < 40 || invalid < 15 {
		t.Errorf("corpus used: %d valid, %d refused", valid, invalid)
	}
	// every group of names sharing a path is refused as soon as two of them are cameras
	for _, grp := range d.Collisions {
		for i := 1; i < len(grp.IDs); i++ {
			_, err := RelayPaths([]config.Camera{{Name: byID[grp.IDs[0]]}, {Name: byID[grp.IDs[i]]}})
			if err == nil || !strings.Contains(err.Error(), "both map to the MediaMTX path '"+grp.RelayPath+"'") {
				t.Errorf("%s + %s: %v", grp.IDs[0], grp.IDs[i], err)
			}
		}
	}
}

func TestUnderscoreNeverReachesAPathName(t *testing.T) {
	for _, n := range []string{"side_door", "Side Door", "side-door", "_sidedoor_"} {
		p, err := PathName(n)
		if err != nil || strings.ContainsAny(p, "_- ") {
			t.Errorf("%q -> %q, %v", n, p, err)
		}
	}
}

func cams(q ...string) []config.Camera {
	names := []string{"Camera One", "Camera Two", "Camera Three"}
	var out []config.Camera
	for i, quality := range q {
		out = append(out, config.Camera{Name: names[i], IP: "192.0.2.1" + string(rune('1'+i)), Quality: quality})
	}
	return out
}

func names(p []LivePath) string {
	var s []string
	for _, e := range p {
		s = append(s, e.Name)
	}
	return strings.Join(s, ",")
}

func TestLivePaths(t *testing.T) {
	cfg := config.Config{LiveSubStream: "stream2"}
	got, err := LivePaths(cams("stream1", "stream2", ""), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// camera Two records the sub stream already: only main; a blank quality is stream1
	if want := "cameraonesub,cameraonemain,cameratwomain,camerathreesub,camerathreemain"; names(got) != want {
		t.Errorf("got %s want %s", names(got), want)
	}
	if got[0].Kind != LiveSub || got[1].Kind != LiveMain || got[1].RelayPath != "cameraone" {
		t.Errorf("%+v", got[:2])
	}
	// PowerShell's -ne is case-insensitive
	got, _ = LivePaths(cams("STREAM2"), cfg)
	if names(got) != "cameraonemain" {
		t.Errorf("case: %s", names(got))
	}
	// a different sub stream flips which cameras get a sub path
	got, _ = LivePaths(cams("stream1", "stream2"), config.Config{LiveSubStream: "stream1"})
	if names(got) != "cameraonemain,cameratwosub,cameratwomain" {
		t.Errorf("stream1: %s", names(got))
	}
}

func TestLiveNameEqualToARelayPathIsAnError(t *testing.T) {
	// camera "Door" has the live path "doormain"; a camera literally named "Door Main" owns that relay path
	_, err := LivePaths([]config.Camera{{Name: "Door", Quality: "stream1"}, {Name: "Door Main", Quality: "stream1"}}, config.Config{LiveSubStream: "stream2"})
	if err == nil || !strings.Contains(err.Error(), "'doormain'") || !strings.Contains(err.Error(), "recording relay's path") {
		t.Errorf("%v", err)
	}
	_, err = LivePaths([]config.Camera{{Name: "Door", Quality: "stream1"}, {Name: "Door Sub", Quality: "stream1"}}, config.Config{LiveSubStream: "stream2"})
	if err == nil || !strings.Contains(err.Error(), "'doorsub'") {
		t.Errorf("sub: %v", err)
	}
	// the relay's own collision comes first
	if _, err = LivePaths([]config.Camera{{Name: "A-B"}, {Name: "AB"}}, config.Config{}); err == nil {
		t.Error("relay collision not reported")
	}
}

func TestSourceEnvName(t *testing.T) {
	if SourceEnvName("cameraonesub") != "MTX_PATHS_CAMERAONESUB_SOURCE" {
		t.Error(SourceEnvName("cameraonesub"))
	}
}
