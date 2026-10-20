package ffmpeg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"holocron/internal/config"
)

const goldenDir = "../../testdata/golden/ffmpeg"

type goldenVariant struct {
	ID     string `json:"id"`
	Inputs struct {
		SegmentSeconds      int
		RtspTransport       string `json:"rtspTransport"`
		TimeoutMicroseconds int64  `json:"timeoutMicroseconds"`
		URL                 string `json:"url"`
		OutputPattern       string `json:"outputPattern"`
		ContainerProfile    config.ContainerProfile
	} `json:"inputs"`
	Args []string `json:"args"`
}

func loadGoldens(t *testing.T) []goldenVariant {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	var out []goldenVariant
	for _, f := range files {
		if filepath.Base(f) == "quoting.json" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var g goldenVariant
		if err := json.Unmarshal(b, &g); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if g.ID+".json" != filepath.Base(f) {
			t.Fatalf("%s: id %q", f, g.ID)
		}
		out = append(out, g)
	}
	return out
}

func (g goldenVariant) inputs() Inputs {
	return Inputs{
		URL:                 g.Inputs.URL,
		RtspTransport:       g.Inputs.RtspTransport,
		TimeoutMicroseconds: g.Inputs.TimeoutMicroseconds,
		SegmentSeconds:      g.Inputs.SegmentSeconds,
		Profile:             g.Inputs.ContainerProfile,
		OutputPattern:       g.Inputs.OutputPattern,
		AllowCredentialArgs: true, // the direct goldens carry the fake credential in the URL
	}
}

func TestBuildArgsReproducesEveryGolden(t *testing.T) {
	goldens := loadGoldens(t)
	if len(goldens) != 11 {
		t.Fatalf("%d golden variants, want 11", len(goldens))
	}
	for _, g := range goldens {
		t.Run(g.ID, func(t *testing.T) {
			got, err := BuildArgs(g.inputs())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, g.Args) {
				t.Fatalf("argument vector differs\n got: %q\nwant: %q", got, g.Args)
			}
		})
	}
}

// Negative control: a one-byte change to any argument must be caught by the comparison, so the
// comparison above is not vacuous.
func TestGoldenComparisonDetectsOneByte(t *testing.T) {
	g := loadGoldens(t)[0]
	got, err := BuildArgs(g.inputs())
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		mut := append([]string(nil), got...)
		mut[i] += "x"
		if reflect.DeepEqual(mut, g.Args) {
			t.Fatalf("a changed argument %d still equals the golden", i)
		}
	}
	mut := append([]string(nil), got...)
	mut[0], mut[1] = mut[1], mut[0]
	if reflect.DeepEqual(mut, g.Args) {
		t.Fatal("a reordering still equals the golden")
	}
}

func TestBuildArgsRefusesCredentialByDefault(t *testing.T) {
	for _, g := range loadGoldens(t) {
		in := g.inputs()
		in.AllowCredentialArgs = false
		_, err := BuildArgs(in)
		direct := strings.Contains(g.Inputs.URL, "@")
		if direct && err != ErrCredentialArgs {
			t.Errorf("%s: direct URL accepted without AllowCredentialArgs: %v", g.ID, err)
		}
		if !direct && err != nil {
			t.Errorf("%s: relay URL refused: %v", g.ID, err)
		}
	}
}

func TestBuildArgsRequiredFields(t *testing.T) {
	ok := loadGoldens(t)[0].inputs()
	for name, mut := range map[string]func(*Inputs){
		"url":       func(i *Inputs) { i.URL = "" },
		"transport": func(i *Inputs) { i.RtspTransport = "" },
		"pattern":   func(i *Inputs) { i.OutputPattern = "" },
		"format":    func(i *Inputs) { i.Profile.SegmentFormat = "" },
		"seconds":   func(i *Inputs) { i.SegmentSeconds = 0 },
	} {
		in := ok
		mut(&in)
		if _, err := BuildArgs(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRelayURLAndSegmentPattern(t *testing.T) {
	if got := RelayURL("rtsp://127.0.0.1:8554/", "cameraone"); got != "rtsp://127.0.0.1:8554/cameraone" {
		t.Fatal(got)
	}
	root := t.TempDir()
	want := filepath.Join(root, "Camera_One", "Camera_One_%Y%m%d_%H%M%S%z.mkv")
	if got := SegmentPattern(root, "Camera_One", ".mkv"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	want = filepath.Join(root, "Cam%Y", "Cam%%Y_%Y%m%d_%H%M%S%z.mp4")
	if got := SegmentPattern(root, "Cam%Y", ".mp4"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
