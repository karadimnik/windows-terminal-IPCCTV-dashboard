package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"holocron/internal/osx"
)

var winP = &osx.Fake{OS: "windows", DirsValue: osx.DefaultDirs("windows", osx.Env{Home: `C:\Users\op`, ExeDir: `C:\holocron`})}
var linP = &osx.Fake{OS: "linux", DirsValue: osx.DefaultDirs("linux", osx.Env{Home: "/home/op", ExeDir: "/opt/h"})}

func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every scalar default of Get-CamConfigDefault (cam.common.ps1, read only) must be either a kept key
// with the same Go default (Windows values) or listed as dropped. Nothing may be silently missing.
func TestDefaultsMatchPowerShellSource(t *testing.T) {
	src := string(repoFile(t, "cam.common.ps1"))
	start := strings.Index(src, "function Get-CamConfigDefault")
	end := strings.Index(src[start:], "function Get-CamConfig {")
	block := src[start : start+end]
	line := regexp.MustCompile(`(?m)^ {8}(\w+)\s*=\s*(.+?)\s*(#.*)?$`)
	kept := map[string]bool{}
	for _, k := range KeptKeys() {
		kept[k] = true
	}
	dropped := map[string]bool{}
	for _, k := range DroppedKeys() {
		dropped[k] = true
	}
	cfg := reflect.ValueOf(Defaults(winP))
	seen, compared := 0, 0
	for _, m := range line.FindAllStringSubmatch(block, -1) {
		key, lit := m[1], m[2]
		seen++
		if !kept[key] && !dropped[key] {
			t.Errorf("PowerShell key %s is neither kept nor documented as dropped", key)
			continue
		}
		if dropped[key] || strings.HasPrefix(lit, "@") {
			continue
		}
		f := cfg.FieldByName(key)
		if !f.IsValid() {
			t.Errorf("kept key %s has no field", key)
			continue
		}
		var want any
		switch {
		case lit == "$null":
			if !f.IsNil() {
				t.Errorf("%s default should be null", key)
			}
			compared++
			continue
		case lit == "$true", lit == "$false":
			want = lit == "$true"
		case lit == "2MB":
			want = int64(2 << 20)
		case strings.HasPrefix(lit, "'"):
			want = strings.Trim(lit, "'")
		default:
			n, err := strconv.ParseFloat(lit, 64)
			if err != nil {
				t.Fatalf("cannot read literal %q of %s", lit, key)
			}
			want = n
		}
		var got any
		switch f.Kind() {
		case reflect.String:
			got = f.String()
		case reflect.Bool:
			got = f.Bool()
		case reflect.Int, reflect.Int64:
			if _, isNum := want.(float64); isNum {
				got = float64(f.Int())
			} else {
				got = f.Int()
			}
		case reflect.Float64:
			got = f.Float()
		}
		if got != want {
			t.Errorf("default %s = %v, PowerShell %v", key, got, want)
		}
		compared++
	}
	if seen < 50 || compared < 40 {
		t.Errorf("read %d keys, compared %d: the extraction is broken", seen, compared)
	}
	if got := len(KeptKeys()) + len(DroppedKeys()); got < seen {
		t.Errorf("%d keys documented for %d PowerShell keys", got, seen)
	}
}

func TestExampleConfigLoadsUnchanged(t *testing.T) {
	cfg, warns, err := Parse(repoFile(t, "recorder.config.example.json"), winP)
	if err != nil || len(warns) != 0 {
		t.Fatalf("example config: %v, warnings %v (HealthStripHeightPx is a dropped key and must be silent)", err, warns)
	}
	if !reflect.DeepEqual(cfg, Defaults(winP)) {
		t.Errorf("the example file differs from the defaults:\n%+v\n%+v", cfg, Defaults(winP))
	}
}

func TestPlatformDefaults(t *testing.T) {
	l := Defaults(linP)
	if l.RootPath != "/srv/cameras" || l.FfmpegPath != "ffmpeg" || l.RestreamerExePath != "mediamtx" || !strings.HasSuffix(l.LiveConfigPath, "/holocron/mediamtx-live.yml") {
		t.Errorf("linux defaults: %+v", l)
	}
	if w := Defaults(winP); w.RootPath != `E:\cameras` || w.FfmpegPath != `C:\ffmpeg\bin\ffmpeg.exe` {
		t.Errorf("windows defaults: %+v", w)
	}
	if p, ok := Defaults(linP).Profile(); !ok || p.Extension != ".mkv" || p.SegmentFormat != "matroska" {
		t.Errorf("profile %+v", p)
	}
}

func TestParseOverlayAndLoad(t *testing.T) {
	doc := "\xef\xbb\xbf" + `{"RootPath":"/data/cams","SegmentSeconds":60,"MaxRetentionDays":30,"ReserveGB":12.5,"rtsptransport":"udp",
		"LiveAdvertiseHosts":["192.168.1.50"],"LiveUdpAddress":"192.168.1.50:8189","logging":{"level":"debug","levels":{"supervisor":"trace"}}}`
	cfg, warns, err := Parse([]byte(doc), linP)
	if err != nil || len(warns) != 0 {
		t.Fatalf("%v %v", err, warns)
	}
	d := Defaults(linP)
	if cfg.RootPath != "/data/cams" || cfg.SegmentSeconds != 60 || *cfg.MaxRetentionDays != 30 || cfg.ReserveGB != 12.5 || cfg.RtspTransport != "udp" ||
		cfg.PruneHysteresisGB != d.PruneHysteresisGB || cfg.Logging.Level != "debug" || cfg.Logging.Levels["supervisor"] != "trace" ||
		!reflect.DeepEqual(cfg.LiveAdvertiseHosts, []string{"192.168.1.50"}) {
		t.Errorf("overlay: %+v", cfg)
	}
	for _, empty := range []string{"", "  \r\n ", "{}", "\xef\xbb\xbf"} {
		got, w, err := Parse([]byte(empty), linP)
		if err != nil || len(w) != 0 || !reflect.DeepEqual(got, d) {
			t.Errorf("%q should give the defaults: %v", empty, err)
		}
	}

	dir := t.TempDir()
	if got, _, err := Load(filepath.Join(dir, "missing.json"), linP); err != nil || !reflect.DeepEqual(got, d) {
		t.Errorf("missing file must give defaults: %v", err)
	}
	p := filepath.Join(dir, "holocron.config.json")
	os.WriteFile(p, []byte(`{"SegmentSeconds": 30}`), 0o644)
	if got, _, err := Load(p, linP); err != nil || got.SegmentSeconds != 30 {
		t.Errorf("Load: %v %d", err, got.SegmentSeconds)
	}
	os.WriteFile(p, []byte(`{"SegmentSeconds": 0}`), 0o644)
	if _, _, err := Load(p, linP); err == nil {
		t.Error("Load must report an invalid file")
	}
}

func TestWarnings(t *testing.T) {
	_, warns, err := Parse([]byte(`{"HealthStripHeightPx":-1,"Surprise":1,"ContainerProfiles":{"mkv":{"Extension":".mkv","SegmentFormat":"matroska","Oddity":1}},"logging":{"colour":true}}`), linP)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, w := range warns {
		paths = append(paths, w.Path)
	}
	want := []string{"Surprise", "ContainerProfiles.mkv.Oddity", "logging.colour"}
	if len(paths) != 3 {
		t.Fatalf("warnings %v, want %v (and nothing for the dropped HealthStripHeightPx)", paths, want)
	}
	for _, w := range want {
		found := false
		for _, p := range paths {
			found = found || p == w
		}
		if !found {
			t.Errorf("no warning for %s: %v", w, paths)
		}
	}
	// "Clamped" keys warn instead of failing, as the PowerShell consumers clamp at use
	cfg, warns, err := Parse([]byte(`{"RelayProbeTimeoutMs":50,"StartStaggerMs":-5,"RelayReadyWaitSeconds":-1}`), linP)
	if err != nil || cfg.RelayProbeTimeoutMs != 100 || cfg.StartStaggerMs != 0 || cfg.RelayReadyWaitSeconds != 0 || len(warns) != 3 {
		t.Errorf("clamps: %v %+v %v", err, cfg, warns)
	}
}

// Each validation class has a negative control: a document that must fail with the key path in the message.
func TestValidationClasses(t *testing.T) {
	bad := []struct {
		name string
		doc  string
		path string // a problem with this path must exist
		text string // and mention this
	}{
		{"boolean as string (IR 10.9)", `{"RestreamerEnabled":"false"}`, "RestreamerEnabled", "JSON boolean"},
		{"live boolean as string", `{"LiveViewEnabled":"true"}`, "LiveViewEnabled", "JSON boolean"},
		{"boolean as number", `{"RestreamerEnabled":0}`, "RestreamerEnabled", "boolean"},
		{"null boolean", `{"RestreamerEnabled":null}`, "RestreamerEnabled", "boolean"},
		{"segment seconds zero", `{"SegmentSeconds":0}`, "SegmentSeconds", "at least 1"},
		{"segment seconds string", `{"SegmentSeconds":"900"}`, "SegmentSeconds", "must be a number"},
		{"segment seconds fraction", `{"SegmentSeconds":1.5}`, "SegmentSeconds", "whole number"},
		{"stall factor 1", `{"StallFactor":1}`, "StallFactor", "greater than 1"},
		{"stall factor low", `{"StallFactor":0.5}`, "StallFactor", "greater than 1"},
		{"root with percent", `{"RootPath":"/srv/Cam%Y"}`, "RootPath", "must not contain '%'"},
		{"root empty", `{"RootPath":""}`, "RootPath", "must not be empty"},
		{"root blank", `{"RootPath":"   "}`, "RootPath", "must not be empty"},
		{"root number", `{"RootPath":5}`, "RootPath", "must be a string"},
		{"fraction zero", `{"MaxAgePruneFraction":0}`, "MaxAgePruneFraction", "0.01 to 1.0"},
		{"fraction negative", `{"MaxAgePruneFraction":-1}`, "MaxAgePruneFraction", "0.01 to 1.0"},
		{"fraction high", `{"MaxAgePruneFraction":1.5}`, "MaxAgePruneFraction", "0.01 to 1.0"},
		{"fraction text", `{"MaxAgePruneFraction":"abc"}`, "MaxAgePruneFraction", "must be a number"},
		{"retention zero", `{"MaxRetentionDays":0}`, "MaxRetentionDays", "null"},
		{"retention text", `{"MaxRetentionDays":"30"}`, "MaxRetentionDays", "number"},
		{"reserve negative", `{"ReserveGB":-1}`, "ReserveGB", "at least 0"},
		{"interval zero", `{"PruneIntervalSeconds":0}`, "PruneIntervalSeconds", "greater than 0"},
		{"mover switch as string", `{"MoveToDayFolders":"false"}`, "MoveToDayFolders", "JSON boolean"},
		{"mover interval below 10", `{"MoveIntervalSeconds":9}`, "MoveIntervalSeconds", "at least 10"},
		{"mover margin below 30", `{"MoveMarginSeconds":29.5}`, "MoveMarginSeconds", "at least 30"},
		{"mover max zero", `{"MoveMaxPerCycle":0}`, "MoveMaxPerCycle", "at least 1"},
		{"backoff inverted", `{"BackoffStartSeconds":30,"BackoffMaxSeconds":10}`, "BackoffMaxSeconds", "less than BackoffStartSeconds"},
		{"transport typo", `{"RtspTransport":"tpc"}`, "RtspTransport", "one of tcp"},
		{"relay url without host", `{"RelayBase":"rtsp://"}`, "RelayBase", "no host:port"},
		{"external relay with the default own relay", `{"RelayExternal":true}`, "RelayExternal", "needs RestreamerEnabled false"},
		{"external relay with an own relay", `{"RelayExternal":true,"RestreamerEnabled":true}`, "RelayExternal", "needs RestreamerEnabled false"},
		{"external relay as string", `{"RelayExternal":"true","RestreamerEnabled":false}`, "RelayExternal", "JSON boolean"},
		{"container missing", `{"Container":"avi"}`, "Container", "not defined in ContainerProfiles"},
		{"profile no extension", `{"ContainerProfiles":{"mkv":{"SegmentFormat":"matroska"}}}`, "ContainerProfiles.mkv.Extension", "required"},
		{"profile bad extension", `{"ContainerProfiles":{"mkv":{"Extension":"mkv","SegmentFormat":"matroska"}}}`, "ContainerProfiles.mkv.Extension", "dot"},
		{"profile extension with path", `{"ContainerProfiles":{"mkv":{"Extension":"/../x","SegmentFormat":"m"}}}`, "ContainerProfiles.mkv.Extension", "dot"},
		{"profile no format", `{"ContainerProfiles":{"mkv":{"Extension":".mkv"}}}`, "ContainerProfiles.mkv.SegmentFormat", "required"},
		{"profile args not array", `{"ContainerProfiles":{"mkv":{"Extension":".mkv","SegmentFormat":"m","AudioArgs":"-c copy"}}}`, "ContainerProfiles.mkv.AudioArgs", "array of strings"},
		{"profile arg not string", `{"ContainerProfiles":{"mkv":{"Extension":".mkv","SegmentFormat":"m","ExtraArgs":["a",1]}}}`, "ContainerProfiles.mkv.ExtraArgs[1]", "string"},
		{"profiles replace the defaults", `{"ContainerProfiles":{"x":{"Extension":".x","SegmentFormat":"m"}}}`, "Container", "not defined"},
		{"unknown live key", `{"LiveViewEnabeld":true}`, "LiveViewEnabeld", "unknown live-view setting"},
		{"live config empty", `{"LiveConfigPath":""}`, "LiveConfigPath", "must not be empty"},
		{"live share path", `{"LiveSecretPath":"\\\\nas\\x"}`, "LiveSecretPath", "share path"},
		{"whep not loopback", `{"LiveWhepAddress":"0.0.0.0:8889"}`, "LiveWhepAddress", "127.0.0.1"},
		{"whep port 0", `{"LiveWhepAddress":"127.0.0.1:0"}`, "LiveWhepAddress", "127.0.0.1"},
		{"whep is the relay", `{"LiveWhepAddress":"127.0.0.1:9997"}`, "LiveWhepAddress", "recording relay"},
		{"udp public", `{"LiveUdpAddress":"8.8.8.8:8189"}`, "LiveUdpAddress", "private"},
		{"udp name", `{"LiveUdpAddress":"cam.local:8189"}`, "LiveUdpAddress", "IPv4"},
		{"lan udp without advertise", `{"LiveUdpAddress":"0.0.0.0:8189"}`, "LiveAdvertiseHosts", "empty"},
		{"advertise with loopback udp", `{"LiveAdvertiseHosts":["192.168.1.5"]}`, "LiveAdvertiseHosts", "only loopback"},
		{"advertise public", `{"LiveUdpAddress":"0.0.0.0:8189","LiveAdvertiseHosts":["8.8.8.8"]}`, "LiveAdvertiseHosts[0]", "private"},
		{"advertise duplicate", `{"LiveUdpAddress":"0.0.0.0:8189","LiveAdvertiseHosts":["10.0.0.1","10.0.0.1"]}`, "LiveAdvertiseHosts", "twice"},
		{"advertise too many", `{"LiveUdpAddress":"0.0.0.0:8189","LiveAdvertiseHosts":["10.0.0.1","10.0.0.2","10.0.0.3","10.0.0.4","10.0.0.5"]}`, "LiveAdvertiseHosts", "at most 4"},
		{"advertise other than bound", `{"LiveUdpAddress":"10.0.0.1:8189","LiveAdvertiseHosts":["10.0.0.2"]}`, "LiveAdvertiseHosts", "only 10.0.0.1"},
		{"advertise not array", `{"LiveAdvertiseHosts":"10.0.0.1"}`, "LiveAdvertiseHosts", "array"},
		{"substream bad", `{"LiveSubStream":"stream 2"}`, "LiveSubStream", "stream name"},
		{"close after 0", `{"LiveCloseAfterSeconds":0}`, "LiveCloseAfterSeconds", "at least 1"},
		{"close after 3601", `{"LiveCloseAfterSeconds":3601}`, "LiveCloseAfterSeconds", "1 to 3600"},
		{"bad log level", `{"logging":{"level":"loud"}}`, "logging.level", ""},
		{"bad component level", `{"logging":{"levels":{"web":"x"}}}`, "logging.levels.web", ""},
		{"duplicate keys by case", `{"RootPath":"/a","rootpath":"/b"}`, "rootpath", "twice"},
		{"not an object", `[1,2]`, "", "not a JSON object"},
		{"broken json", `{"RootPath":`, "", "not a JSON object"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse([]byte(tc.doc), linP)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("accepted (err = %v)", err)
			}
			for _, p := range ve.Problems {
				if p.Path == tc.path && strings.Contains(p.Message, tc.text) {
					return
				}
			}
			t.Errorf("no problem %q containing %q in %v", tc.path, tc.text, ve.Problems)
		})
	}

	good := []string{
		`{"RootPath":"/srv/cams"}`, `{"MaxAgePruneFraction":1.0}`, `{"MaxAgePruneFraction":0.5}`, `{"MaxAgePruneFraction":0.01}`, `{"MaxRetentionDays":null}`, `{"StallFactor":1.01}`,
		`{"LiveViewEnabled":true,"LiveUdpAddress":"0.0.0.0:8189","LiveAdvertiseHosts":["192.168.1.50","10.0.0.2"]}`,
		`{"LiveAdvertiseHosts":null}`, `{"LiveCloseAfterSeconds":3600}`, `{"RtspTransport":"udp"}`,
		`{"Container":"fmp4"}`, `{"ContainerProfiles":{"x":{"Extension":".ts","SegmentFormat":"mpegts"}},"Container":"X"}`,
	}
	for _, doc := range good {
		if _, _, err := Parse([]byte(doc), linP); err != nil {
			t.Errorf("control %s refused: %v", doc, err)
		}
	}
}

// Every problem is reported, not just the first.
func TestAllProblemsAtOnce(t *testing.T) {
	_, _, err := Parse([]byte(`{"SegmentSeconds":0,"StallFactor":1,"RootPath":"","RestreamerEnabled":"false","MaxAgePruneFraction":7,"LiveSubStream":"a b","Nope":1}`), linP)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 6 {
		t.Fatalf("problems: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "6 problems: ") {
		t.Errorf("message %q", err.Error())
	}
	for _, key := range []string{"SegmentSeconds", "StallFactor", "RootPath", "RestreamerEnabled", "MaxAgePruneFraction", "LiveSubStream"} {
		if !strings.Contains(err.Error(), key+": ") {
			t.Errorf("%s missing from %q", key, err.Error())
		}
	}
}

// The mover keys (D20): defaults on, 60 s, 120 s, 200; the floors are accepted.
func TestMoverKeys(t *testing.T) {
	for _, p := range []osx.Platform{winP, linP} {
		d := Defaults(p)
		if !d.MoveToDayFolders || d.MoveIntervalSeconds != 60 || d.MoveMarginSeconds != 120 || d.MoveMaxPerCycle != 200 {
			t.Errorf("defaults: %v %v %v %v", d.MoveToDayFolders, d.MoveIntervalSeconds, d.MoveMarginSeconds, d.MoveMaxPerCycle)
		}
	}
	c, w, err := Parse([]byte(`{"MoveToDayFolders":false,"MoveIntervalSeconds":10,"MoveMarginSeconds":30,"MoveMaxPerCycle":1}`), linP)
	if err != nil || c.MoveToDayFolders || c.MoveIntervalSeconds != 10 || c.MoveMarginSeconds != 30 || c.MoveMaxPerCycle != 1 || len(w) != 0 {
		t.Errorf("parsed %v %v %v %v, warnings %v, err %v", c.MoveToDayFolders, c.MoveIntervalSeconds, c.MoveMarginSeconds, c.MoveMaxPerCycle, w, err)
	}
}

// RelayExternal defaults to false; with RestreamerEnabled false it is accepted as is.
func TestRelayExternal(t *testing.T) {
	if Defaults(winP).RelayExternal || Defaults(linP).RelayExternal {
		t.Error("RelayExternal must default to false")
	}
	for _, doc := range []string{`{"RelayExternal":true,"RestreamerEnabled":false}`, `{"RelayExternal":false}`, `{"RelayExternal":false,"RestreamerEnabled":false}`} {
		c, warns, err := Parse([]byte(doc), linP)
		if err != nil || len(warns) != 0 {
			t.Errorf("%s: %v %v", doc, err, warns)
			continue
		}
		want := strings.Contains(doc, `"RelayExternal":true`)
		if c.RelayExternal != want || (want && c.RestreamerEnabled) {
			t.Errorf("%s: RelayExternal %v RestreamerEnabled %v", doc, c.RelayExternal, c.RestreamerEnabled)
		}
	}
}
