package golden

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zone database travels with the test: no dependency on the machine's

	"holocron/internal/status"
)

const root = "../../testdata/golden"

func read(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func load(t *testing.T, rel string, into any) {
	t.Helper()
	if err := json.Unmarshal(read(t, rel), into); err != nil {
		t.Fatalf("%s does not parse: %v", rel, err)
	}
}

func glob(t *testing.T, pattern string, atLeast int) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
	if len(files) < atLeast {
		t.Fatalf("%s: %d files, want at least %d", pattern, len(files), atLeast)
	}
	return files
}

type ffmpegGolden struct {
	Fixture string
	ID      string `json:"id"`
	Inputs  struct {
		Container      string
		SegmentSeconds int
		InputMode      string
		URL            string `json:"url"`
		OutputPattern  string
	}
	Args        []string `json:"args"`
	CommandLine string   `json:"commandLine"`
}

func TestFfmpegVectorsParseAndAreConsistent(t *testing.T) {
	files := glob(t, "ffmpeg/*.json", 12)
	variants := 0
	for _, f := range files {
		rel := "ffmpeg/" + filepath.Base(f)
		if filepath.Base(f) == "quoting.json" {
			var q struct {
				Rows []struct{ Argument, Quoted string }
			}
			load(t, rel, &q)
			if len(q.Rows) < 10 {
				t.Errorf("quoting table has %d rows", len(q.Rows))
			}
			continue
		}
		variants++
		var g ffmpegGolden
		load(t, rel, &g)
		if !strings.Contains(g.Fixture, "SYNTHETIC FIXTURE") || g.ID+".json" != filepath.Base(f) {
			t.Errorf("%s: fixture marker or id wrong", rel)
		}
		if len(g.Args) < 20 || g.CommandLine == "" {
			t.Errorf("%s: empty vector", rel)
		}
		if g.Args[len(g.Args)-1] != g.Inputs.OutputPattern {
			t.Errorf("%s: the last argument is not the output pattern", rel)
		}
		urlAt := -1
		for i, a := range g.Args {
			if a == "-i" {
				urlAt = i + 1
			}
		}
		if urlAt < 0 || g.Args[urlAt] != g.Inputs.URL {
			t.Errorf("%s: -i is not followed by the input URL", rel)
		}
		if got := strings.Contains(g.Inputs.URL, "fakeuser:fakepass@"); got != (g.Inputs.InputMode == "direct") {
			t.Errorf("%s: credential in URL = %v for input mode %s", rel, got, g.Inputs.InputMode)
		}
		if !strings.Contains(g.CommandLine, "-segment_atclocktime 1 -reset_timestamps 1 -strftime 1") {
			t.Errorf("%s: segment options missing from the command line", rel)
		}
	}
	if variants != 11 {
		t.Errorf("%d ffmpeg variants, want 11", variants)
	}
}

func TestConfigGoldensAreNotEmptyAndRelayIgnoresLiveFlag(t *testing.T) {
	var inputs struct {
		APILogin  struct{ Password, PasswordHash string }    `json:"apiLogin"`
		LiveLogin struct{ Secret, SecretHash string }        `json:"liveLogin"`
		Variants  []struct{ File, Kind, Description string } `json:"variants"`
	}
	load(t, "relay-live-inputs.json", &inputs)
	if len(inputs.Variants) != 7 {
		t.Fatalf("%d config variants listed, want 7", len(inputs.Variants))
	}
	hash := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return "sha256:" + base64.StdEncoding.EncodeToString(sum[:])
	}
	if inputs.APILogin.PasswordHash != hash(inputs.APILogin.Password) || inputs.LiveLogin.SecretHash != hash(inputs.LiveLogin.Secret) {
		t.Error("the recorded hashes are not sha256: + base64(sha256(fake secret))")
	}
	texts := map[string][]byte{}
	for _, v := range inputs.Variants {
		b := read(t, v.File)
		texts[v.File] = b
		if len(b) < 500 || bytes.Count(b, []byte("\r\n")) == 0 || bytes.Count(b, []byte("\n")) != bytes.Count(b, []byte("\r\n")) {
			t.Errorf("%s: empty or not CRLF", v.File)
		}
		if !bytes.Contains(b, []byte("authMethod: internal")) || !bytes.Contains(b, []byte("paths:")) {
			t.Errorf("%s: missing authMethod or paths", v.File)
		}
		if bytes.Contains(b, []byte(inputs.APILogin.Password)) || bytes.Contains(b, []byte(inputs.LiveLogin.Secret)) {
			t.Errorf("%s: a fake secret appears in clear", v.File)
		}
		switch v.Kind {
		case "relay":
			if !bytes.Contains(b, []byte(inputs.APILogin.PasswordHash)) {
				t.Errorf("%s: relay config lacks the API password hash", v.File)
			}
		case "live":
			if !bytes.Contains(b, []byte(inputs.LiveLogin.SecretHash)) || !bytes.Contains(b, []byte("webrtc: yes")) {
				t.Errorf("%s: live config lacks the secret hash or webrtc", v.File)
			}
		default:
			t.Errorf("%s: kind %q", v.File, v.Kind)
		}
	}
	for _, f := range []string{"relay/live-off.yml", "relay/live-on.yml"} {
		if !bytes.Equal(texts[f], texts["relay/default.yml"]) {
			t.Errorf("%s differs from relay/default.yml: the relay config must not depend on the live-view flag", f)
		}
	}
	if bytes.Equal(texts["relay/custom-addresses.yml"], texts["relay/default.yml"]) {
		t.Error("custom addresses did not change the relay config")
	}
}

func TestSafeNameCorpusParses(t *testing.T) {
	var d struct {
		Cases []struct {
			ID, Name string
			OK       bool    `json:"ok"`
			SafeName *string `json:"safeName"`
			ErrKind  string  `json:"errorKind"`
		} `json:"cases"`
		Folders []struct{ Folder string }    `json:"caseInsensitiveFolderCollisions"`
		Paths   []struct{ RelayPath string } `json:"relayPathCollisions"`
	}
	load(t, "names/safe-names.json", &d)
	if len(d.Cases) < 55 {
		t.Fatalf("%d cases", len(d.Cases))
	}
	kinds := map[string]int{}
	for _, c := range d.Cases {
		if c.OK == (c.SafeName == nil) || (c.OK && *c.SafeName == "") {
			t.Errorf("%s: ok=%v safeName=%v", c.ID, c.OK, c.SafeName)
		}
		if !c.OK {
			kinds[c.ErrKind]++
		}
	}
	if kinds["reserved-device-name"] < 5 || kinds["no-legal-characters"] < 5 || kinds["other"] != 0 {
		t.Errorf("error kinds %v", kinds)
	}
	if len(d.Folders) == 0 || len(d.Paths) == 0 {
		t.Error("collision groups are empty")
	}
}

func TestSegmentNamesParse(t *testing.T) {
	var d struct {
		Containers []struct {
			Container string
			Names     []struct {
				SafeName, Pattern string
				Samples           []struct{ Name, Local, Offset, UTC string }
				Accept, Reject    []struct {
					Name string
					Trap bool
				}
			}
		}
	}
	load(t, "names/segments.json", &d)
	if len(d.Containers) != 2 {
		t.Fatalf("%d containers", len(d.Containers))
	}
	traps := 0
	for _, c := range d.Containers {
		for _, n := range c.Names {
			if len(n.Samples) != 7 || len(n.Accept) < 3 || len(n.Reject) < 15 {
				t.Errorf("%s/%s: %d samples, %d accept, %d reject", c.Container, n.SafeName, len(n.Samples), len(n.Accept), len(n.Reject))
			}
			for _, s := range n.Samples {
				// the sample is the pattern with the instant substituted: an independent re-derivation
				l, err := time.Parse("2006-01-02 15:04:05", s.Local)
				if err != nil {
					t.Fatal(err)
				}
				want := strings.NewReplacer("%Y", l.Format("2006"), "%m", l.Format("01"), "%d", l.Format("02"),
					"%H", l.Format("15"), "%M", l.Format("04"), "%S", l.Format("05"), "%z", s.Offset).Replace(n.Pattern)
				if want != s.Name {
					t.Errorf("%s: sample %q, re-derived %q", n.SafeName, s.Name, want)
				}
			}
			for _, a := range append(n.Accept, n.Reject...) {
				if a.Trap {
					traps++
				}
			}
		}
	}
	if traps != 24 {
		t.Errorf("%d trap rows, want 24 (3 per name per container)", traps)
	}
}

// The DST table is reproduced by Go's own time package and the IANA zone.
func TestDSTTableMatchesGoTimeZone(t *testing.T) {
	var d struct {
		Zone        struct{ Windows, IANA string }
		Transitions []struct {
			Kind, ChangeUTC string
			DeltaSeconds    int
			Instants        []struct {
				SecondsFromChange int
				UTC, Formatted    string
				ParsedBackUTC     string
				LocalWall         string
				UTCOffsetSeconds  int
				SegmentSuffix     string
				LogStamp          string
			}
			Interval struct {
				UTCElapsedSeconds, LocalNaiveElapsedSeconds, LocalNaiveErrorSeconds float64
				DueUTC                                                              bool `json:"dueAfter5sUsingUtc"`
				DueLocal                                                            bool `json:"dueAfter5sUsingLocalNaive"`
				HeartbeatAgeSeconds                                                 float64
				From                                                                string `json:"fromUtc"`
				To                                                                  string `json:"toUtc"`
			} `json:"interval"`
		}
	}
	load(t, "time/dst.json", &d)
	loc, err := time.LoadLocation(d.Zone.IANA)
	if err != nil || d.Zone.IANA != "Europe/Bucharest" || d.Zone.Windows != "GTB Standard Time" {
		t.Fatalf("zone %+v: %v", d.Zone, err)
	}
	if len(d.Transitions) != 2 || d.Transitions[0].Kind != "FallBack" || d.Transitions[1].Kind != "SpringForward" {
		t.Fatalf("transitions %+v", d.Transitions)
	}
	const utcLayout = "2006-01-02T15:04:05.0000000Z"
	zulu := func(s string) time.Time {
		v, err := time.Parse(utcLayout, s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		return v
	}
	for _, tr := range d.Transitions {
		if len(tr.Instants) != 11 {
			t.Errorf("%s: %d instants", tr.Kind, len(tr.Instants))
		}
		change := zulu(tr.ChangeUTC)
		for _, i := range tr.Instants {
			utc := zulu(i.UTC)
			if utc != change.Add(time.Duration(i.SecondsFromChange)*time.Second) {
				t.Errorf("%s %d: utc column inconsistent", tr.Kind, i.SecondsFromChange)
			}
			local := utc.In(loc)
			if got := local.Format(status.TimeLayout); got != i.Formatted {
				t.Errorf("%s %d: Go writes %s, PowerShell wrote %s", tr.Kind, i.SecondsFromChange, got, i.Formatted)
			}
			var parsed status.Time
			if err := json.Unmarshal([]byte(`"`+i.Formatted+`"`), &parsed); err != nil || !parsed.Time().Equal(utc) || !zulu(i.ParsedBackUTC).Equal(utc) {
				t.Errorf("%s %d: %s does not read back as the same instant", tr.Kind, i.SecondsFromChange, i.Formatted)
			}
			_, off := local.Zone()
			if off != i.UTCOffsetSeconds || local.Format("2006-01-02 15:04:05") != i.LocalWall ||
				local.Format("20060102_150405-0700") != i.SegmentSuffix || local.Format("2006-01-02 15:04:05 -07:00") != i.LogStamp {
				t.Errorf("%s %d: wall/offset/suffix/log stamp differ from Go's zone", tr.Kind, i.SecondsFromChange)
			}
		}
		iv := tr.Interval
		a, b := zulu(iv.From).In(loc), zulu(iv.To).In(loc)
		naive := b.Sub(a).Seconds()
		wallA, _ := time.Parse("2006-01-02 15:04:05", a.Format("2006-01-02 15:04:05"))
		wallB, _ := time.Parse("2006-01-02 15:04:05", b.Format("2006-01-02 15:04:05"))
		if iv.UTCElapsedSeconds != 600 || iv.HeartbeatAgeSeconds != 8 || naive != 600 {
			t.Errorf("%s: UTC elapsed %v, heartbeat %v", tr.Kind, iv.UTCElapsedSeconds, iv.HeartbeatAgeSeconds)
		}
		if wall := wallB.Sub(wallA).Seconds(); wall != iv.LocalNaiveElapsedSeconds || wall-600 != iv.LocalNaiveErrorSeconds || iv.LocalNaiveErrorSeconds != float64(tr.DeltaSeconds) {
			t.Errorf("%s: wall-clock elapsed %v vs recorded %v (delta %d)", tr.Kind, wall, iv.LocalNaiveElapsedSeconds, tr.DeltaSeconds)
		}
		if !iv.DueUTC || (tr.Kind == "FallBack") == iv.DueLocal {
			t.Errorf("%s: due flags %v %v", tr.Kind, iv.DueUTC, iv.DueLocal)
		}
	}
}

// Every file that carries the fake password must say it is synthetic, so a
// leak scan can tell the fixture from a real credential.
func TestFakeCredentialsAreMarked(t *testing.T) {
	marker := regexp.MustCompile(`SYNTHETIC`)
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(filepath.ToSlash(path), "/generated/") {
			return err
		}
		b, _ := os.ReadFile(path)
		if bytes.Contains(b, []byte("fakepass")) || bytes.Contains(b, []byte("FAKE-API-PASSWORD")) || bytes.Contains(b, []byte("FAKE-LIVE-SECRET")) {
			count++
			if !marker.Match(b) {
				t.Errorf("%s carries a fake credential without the SYNTHETIC marker", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count < 5 {
		t.Errorf("only %d files carry the fake credential: the scan looks broken", count)
	}
}
