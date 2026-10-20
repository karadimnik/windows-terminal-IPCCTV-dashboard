package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/config"
)

func relayInputs(t *testing.T) (RelayInputs, []byte) {
	t.Helper()
	g := loadGolden(t)
	in := RelayInputs{Config: configFor(t, nil), Cameras: g.cameras(), API: Login{User: g.APILogin.User, Secret: g.APILogin.Password}}
	text, err := RelayConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	return in, text
}

func write(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mediamtx.yml")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigCurrent(t *testing.T) {
	in, text := relayInputs(t)
	good := string(text)

	r := ConfigCurrent(write(t, good), in)
	if !r.Current || !r.AuthCurrent || !r.Readable || r.NeedsRewrite() || len(r.Missing)+len(r.Extra) != 0 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Detail, "has a path for every camera") || !strings.Contains(r.Detail, "cameraone, camerathree, cameratwo") {
		t.Error(r.Detail)
	}

	// a new DHCP address only changes a comment: still current (the 2026-10-13 case)
	moved := in
	moved.Cameras = append([]config.Camera(nil), in.Cameras...)
	moved.Cameras[0].IP = "192.0.2.99"
	if r := ConfigCurrent(write(t, good), moved); !r.Current {
		t.Errorf("an IP change made the config stale: %s", r.Detail)
	}
	// a BOM and LF line endings do not matter either
	if r := ConfigCurrent(write(t, "\ufeff"+strings.ReplaceAll(good, "\r\n", "\n")), in); !r.Current {
		t.Errorf("BOM/LF: %s", r.Detail)
	}

	// an added, a removed and a renamed camera change the set
	added := in
	added.Cameras = append(append([]config.Camera(nil), in.Cameras...), config.Camera{Name: "Garage", IP: "192.0.2.20"})
	if r := ConfigCurrent(write(t, good), added); r.Current || !r.AuthCurrent || r.NeedsRewrite() || len(r.Missing) != 1 || r.Missing[0] != "garage" || !strings.Contains(r.Detail, "no path for garage") {
		t.Errorf("added: %+v", r)
	}
	removed := in
	removed.Cameras = in.Cameras[:2]
	if r := ConfigCurrent(write(t, good), removed); r.Current || len(r.Extra) != 1 || r.Extra[0] != "camerathree" || !strings.Contains(r.Detail, "a path for no camera: camerathree") {
		t.Errorf("removed: %+v", r)
	}
	// pathDefaults children are not read as cameras (they sit at the same indent)
	if r := ConfigCurrent(write(t, good), in); len(r.Found) != 3 {
		t.Errorf("found %v", r.Found)
	}

	// the login: missing block, other password, other user, no stored login
	noAuth := good[:strings.Index(good, "authMethod:")] + good[strings.Index(good, "# Localhost control API"):]
	for name, text := range map[string]string{
		"no auth block":  noAuth,
		"other password": strings.Replace(good, Login{Secret: in.API.Secret}.PassHash(), Login{Secret: "x"}.PassHash(), 1),
		"other user":     strings.Replace(good, "- user: camrelayapi", "- user: someoneelse", 1),
		"extra user":     strings.Replace(good, "ips: [\"127.0.0.1\", \"::1\"]", "ips: [\"0.0.0.0\"]", 1),
		"case differs":   strings.Replace(good, "sha256:fz6g", "sha256:FZ6G", 1),
	} {
		r := ConfigCurrent(write(t, text), in)
		if r.Current || r.AuthCurrent || !r.NeedsRewrite() || !strings.Contains(r.Detail, "the control API login is missing or not the stored one") {
			t.Errorf("%s: %+v", name, r)
		}
	}
	none := in
	none.API = Login{}
	if r := ConfigCurrent(write(t, good), none); r.Current || r.AuthCurrent || !strings.Contains(r.Detail, "no control API login exists yet") {
		t.Errorf("no login: %+v", r)
	}

	// unreadable
	r = ConfigCurrent(filepath.Join(t.TempDir(), "missing.yml"), in)
	if r.Readable || r.Current || !strings.Contains(r.Detail, "could not be read at") || len(r.Expected) != 3 {
		t.Errorf("%+v", r)
	}
}

func TestRewriteMessage(t *testing.T) {
	if got := RewriteMessage("C:\\m\\mediamtx.yml"); got != `rewrote C:\m\mediamtx.yml: its control API login was missing or not the stored one; the API now requires the product's login` {
		t.Error(got)
	}
}

// The whole start-up upgrade path: the file written without a login is rewritten to the stored one.
func TestRewriteBringsAnOldFileUpToTheStoredLogin(t *testing.T) {
	in, text := relayInputs(t)
	good := string(text)
	old := good[:strings.Index(good, "authMethod:")] + good[strings.Index(good, "# Localhost control API"):]
	path := write(t, old)
	if r := ConfigCurrent(path, in); !r.NeedsRewrite() {
		t.Fatal("old file not detected")
	}
	if wrote, err := WriteConfigIfChanged(path, text); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	if r := ConfigCurrent(path, in); !r.Current {
		t.Errorf("after the rewrite: %s", r.Detail)
	}
}

func TestGeneratorInputErrors(t *testing.T) {
	in, _ := relayInputs(t)
	mut := func(f func(*RelayInputs)) error {
		c := in
		c.Cameras = append([]config.Camera(nil), in.Cameras...)
		f(&c)
		_, err := RelayConfig(c)
		return err
	}
	for name, f := range map[string]func(*RelayInputs){
		"no host:port":     func(c *RelayInputs) { c.Config.RelayBase = "rtsp://" },
		"empty api base":   func(c *RelayInputs) { c.Config.RelayApiBase = "" },
		"injected address": func(c *RelayInputs) { c.Config.RelayBase = "rtsp://127.0.0.1:8554\nrtmp: yes" },
		"bad camera name":  func(c *RelayInputs) { c.Cameras[0].Name = "..." },
		"no letters":       func(c *RelayInputs) { c.Cameras[0].Name = "---" },
		"reserved name":    func(c *RelayInputs) { c.Cameras[0].Name = "CON" },
		"collision":        func(c *RelayInputs) { c.Cameras[1].Name = "camera-one" },
		"no secret":        func(c *RelayInputs) { c.API.Secret = "" },
		"injected user":    func(c *RelayInputs) { c.API.User = "a\n  - user: any" },
	} {
		if err := mut(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// a name with a line break cannot leave its comment
	c := in
	c.Cameras = []config.Camera{{Name: "Door\n    source: rtsp://evil/x", IP: "192.0.2.5\r\nrtmp: yes"}}
	text, err := RelayConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "\n    source: rtsp://evil") || strings.Contains(string(text), "\r\nrtmp: yes") {
		t.Errorf("a camera name wrote configuration:\n%s", text)
	}
	if got := configPathNames(string(text)); len(got) != 1 {
		t.Errorf("paths %v", got)
	}
}

func TestLiveGeneratorInputErrors(t *testing.T) {
	g := loadGolden(t)
	base := LiveInputs{Config: configFor(t, nil), Cameras: g.cameras(), Login: Login{User: "camlive", Secret: g.LiveLogin.Secret}}
	for name, f := range map[string]func(*LiveInputs){
		"injected whep":   func(l *LiveInputs) { l.Config.LiveWhepAddress = "127.0.0.1:8889\napi: yes" },
		"injected host":   func(l *LiveInputs) { l.Config.LiveAdvertiseHosts = []string{`1.2.3.4"], api: yes #`} },
		"bad name":        func(l *LiveInputs) { l.Cameras = []config.Camera{{Name: ":::"}} },
		"live collision":  func(l *LiveInputs) { l.Cameras = []config.Camera{{Name: "Door"}, {Name: "Door Sub"}} },
		"empty login":     func(l *LiveInputs) { l.Login = Login{} },
		"substream break": func(l *LiveInputs) { l.Config.LiveSubStream = "s\nx" },
	} {
		c := base
		f(&c)
		if _, err := LiveConfig(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
