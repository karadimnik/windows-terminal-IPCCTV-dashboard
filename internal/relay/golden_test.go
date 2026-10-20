package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/config"
	"holocron/internal/osx"
)

const goldenRoot = "../../testdata/golden"

type goldenInputs struct {
	Cameras []struct {
		Name, IP, Quality string
		Record            bool
	} `json:"cameras"`
	APILogin struct {
		User, Password, PasswordHash string
	} `json:"apiLogin"`
	LiveLogin struct {
		User, Secret, SecretHash string
	} `json:"liveLogin"`
	TemplateSha256 string `json:"templateSha256"`
	Variants       []struct {
		File, Kind string
		Settings   map[string]json.RawMessage
	} `json:"variants"`
}

func loadGolden(t *testing.T) goldenInputs {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenRoot, "relay-live-inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g goldenInputs
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g goldenInputs) cameras() []config.Camera {
	var out []config.Camera
	for _, c := range g.Cameras {
		out = append(out, config.Camera{Name: c.Name, IP: c.IP, Quality: c.Quality, Record: c.Record})
	}
	return out
}

// configFor is Get-CamConfigDefault with the variant's settings applied.
func configFor(t *testing.T, settings map[string]json.RawMessage) config.Config {
	t.Helper()
	cfg := config.Defaults(&osx.Fake{OS: "windows"})
	for k, raw := range settings {
		var err error
		switch k {
		case "LiveViewEnabled":
			err = json.Unmarshal(raw, &cfg.LiveViewEnabled)
		case "LiveSubStream":
			err = json.Unmarshal(raw, &cfg.LiveSubStream)
		case "LiveCloseAfterSeconds":
			err = json.Unmarshal(raw, &cfg.LiveCloseAfterSeconds)
		case "RelayBase":
			err = json.Unmarshal(raw, &cfg.RelayBase)
		case "RelayApiBase":
			err = json.Unmarshal(raw, &cfg.RelayApiBase)
		case "LiveWhepAddress":
			err = json.Unmarshal(raw, &cfg.LiveWhepAddress)
		case "LiveUdpAddress":
			err = json.Unmarshal(raw, &cfg.LiveUdpAddress)
		case "LiveAdvertiseHosts":
			err = json.Unmarshal(raw, &cfg.LiveAdvertiseHosts)
		default:
			t.Fatalf("golden setting %s is not mapped", k)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func (g goldenInputs) generate(t *testing.T, kind string, settings map[string]json.RawMessage) []byte {
	t.Helper()
	cfg := configFor(t, settings)
	var (
		out []byte
		err error
	)
	switch kind {
	case "relay":
		out, err = RelayConfig(RelayInputs{Config: cfg, Cameras: g.cameras(), API: Login{User: g.APILogin.User, Secret: g.APILogin.Password}})
	case "live":
		out, err = LiveConfig(LiveInputs{Config: cfg, Cameras: g.cameras(), Login: Login{User: g.LiveLogin.User, Secret: g.LiveLogin.Secret}})
	default:
		t.Fatalf("kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// firstDifference is "" when a and b are byte-identical, otherwise where they first differ.
func firstDifference(a, b []byte) string {
	if bytes.Equal(a, b) {
		return ""
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("byte %d: %q vs %q", i, a[i], b[i])
		}
	}
	return fmt.Sprintf("length %d vs %d", len(a), len(b))
}

func TestGoldensAreReproducedByteForByte(t *testing.T) {
	g := loadGolden(t)
	if len(g.Variants) != 7 {
		t.Fatalf("%d variants", len(g.Variants))
	}
	for _, v := range g.Variants {
		t.Run(v.File, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(goldenRoot, filepath.FromSlash(v.File)))
			if err != nil {
				t.Fatal(err)
			}
			got := g.generate(t, v.Kind, v.Settings)
			if d := firstDifference(got, want); d != "" {
				t.Errorf("generated config differs from the PowerShell golden: %s", d)
			}
		})
	}
}

// The negative control of the test above: one changed byte, in either direction, is reported.
func TestOneByteChangeIsDetected(t *testing.T) {
	g := loadGolden(t)
	for _, v := range g.Variants {
		got := g.generate(t, v.Kind, v.Settings)
		for _, pos := range []int{0, len(got) / 2, len(got) - 1} {
			mut := append([]byte(nil), got...)
			mut[pos] ^= 0x01
			if firstDifference(got, mut) == "" {
				t.Errorf("%s: a flipped byte at %d went unnoticed", v.File, pos)
			}
		}
		if firstDifference(got, got[:len(got)-1]) == "" || firstDifference(got, append(append([]byte(nil), got...), '\n')) == "" {
			t.Errorf("%s: a length change went unnoticed", v.File)
		}
	}
}

func TestRelayConfigDoesNotDependOnTheLiveFlag(t *testing.T) {
	g := loadGolden(t)
	off := g.generate(t, "relay", map[string]json.RawMessage{"LiveViewEnabled": json.RawMessage("false"), "LiveSubStream": json.RawMessage(`"stream2"`)})
	on := g.generate(t, "relay", map[string]json.RawMessage{"LiveViewEnabled": json.RawMessage("true"), "LiveSubStream": json.RawMessage(`"stream1"`), "LiveCloseAfterSeconds": json.RawMessage("3")})
	def := g.generate(t, "relay", nil)
	if !bytes.Equal(off, def) || !bytes.Equal(on, def) {
		t.Error("the relay config changed with the live keys")
	}
}

func TestTemplateCopyIsTheRepositoryTemplate(t *testing.T) {
	g := loadGolden(t)
	repo, err := os.ReadFile("../../mediamtx.template.yml")
	if err != nil {
		t.Fatal(err)
	}
	if template != string(repo) {
		t.Error("internal/relay/mediamtx.template.yml differs from mediamtx.template.yml: copy it again")
	}
	sum := sha256.Sum256([]byte(template))
	if hex.EncodeToString(sum[:]) != g.TemplateSha256 {
		t.Errorf("template sha256 %x, the golden inputs say %s: regenerate the goldens", sum, g.TemplateSha256)
	}
}

// ---- credentials never appear in a generated config ----

// leakForms lists the forms in which a secret could be found in text: raw and URL-escaped (query,
// path and userinfo).
func leakForms(secret string) []string {
	userinfo := strings.TrimPrefix(url.UserPassword("u", secret).String(), "u:")
	return []string{secret, url.QueryEscape(secret), url.PathEscape(secret), userinfo}
}

func findLeaks(text []byte, secrets ...string) []string {
	var found []string
	for _, s := range secrets {
		for _, f := range leakForms(s) {
			if f != "" && bytes.Contains(text, []byte(f)) {
				found = append(found, f)
			}
		}
	}
	return found
}

func TestNoCredentialAppearsInAnyGeneratedConfig(t *testing.T) {
	g := loadGolden(t)
	// a camera login with characters that are escaped in a URL, so the escaped forms are scanned too
	camUser, camPass := "fakeuser", "fake pass/with@odd:chars%"
	secrets := []string{camUser, camPass, g.APILogin.Password, g.LiveLogin.Secret}
	src := fixedCreds{user: camUser, pass: camPass}

	var texts [][]byte
	for _, v := range g.Variants {
		texts = append(texts, g.generate(t, v.Kind, v.Settings))
	}
	for _, text := range texts {
		if leaks := findLeaks(text, secrets...); len(leaks) > 0 {
			t.Errorf("a generated config contains %q", leaks)
		}
	}

	// positive control: the scan does find the secrets where they are supposed to be (the environment)
	env, err := SourceEnv(g.cameras(), src)
	if err != nil {
		t.Fatal(err)
	}
	liveEnv, err := LiveSourceEnv(g.cameras(), configFor(t, nil), src)
	if err != nil {
		t.Fatal(err)
	}
	joined := []byte(strings.Join(append(env, liveEnv...), "\n"))
	if leaks := findLeaks(joined, camPass); len(leaks) == 0 {
		t.Error("positive control: the scan did not find the camera password in the source environment")
	}
	planted := append(append([]byte(nil), texts[0]...), []byte("# "+g.APILogin.Password)...)
	if leaks := findLeaks(planted, secrets...); len(leaks) == 0 {
		t.Error("positive control: a planted API password was not found")
	}
	planted = append(append([]byte(nil), texts[4]...), []byte("# "+url.PathEscape(camPass))...)
	if leaks := findLeaks(planted, secrets...); len(leaks) == 0 {
		t.Error("positive control: a planted escaped camera password was not found")
	}
	// the only mention of a login in the configs is a hash, and it is the hash of the fake secret
	if !bytes.Contains(texts[0], []byte(g.APILogin.PasswordHash)) || !bytes.Contains(texts[4], []byte(g.LiveLogin.SecretHash)) {
		t.Error("the configs lack the recorded hashes")
	}
}

type fixedCreds struct{ user, pass string }

func (f fixedCreds) CameraCredential(config.Camera) (string, string, error) { return f.user, f.pass, nil }
