package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"holocron/internal/config"
	"holocron/internal/osx"
	"holocron/internal/relay"
)

// The supervisor writes the live MediaMTX login from one file and the web role's WHEP proxy sends the
// login from another unless both resolve it alike. Every row builds both sides from the same
// holocron.config.json and the same environment and demands the same path.
func TestLiveSecretSameFileInBothRoles(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", local)
	t.Setenv("HOLOCRON_TEST_DIR", filepath.Join(local, "chosen"))

	rows := []struct {
		name, goos, json string
	}{
		{"windows, no key", "windows", `{}`},
		{"windows, blank key", "windows", `{"LiveSecretPath": "   "}`},
		{"windows, key set", "windows", `{"LiveSecretPath": ` + quoteJSON(filepath.Join(local, "x", "secret.txt")) + `}`},
		{"windows, key with variables", "windows", `{"LiveSecretPath": "%LOCALAPPDATA%/IPCCTV/live_view_secret.txt"}`},
		{"linux, no key", "linux", `{}`},
		{"linux, key set", "linux", `{"LiveSecretPath": ` + quoteJSON(filepath.Join(local, "x", "secret.txt")) + `}`},
		{"linux, key with variables", "linux", `{"LiveSecretPath": "%HOLOCRON_TEST_DIR%/secret.txt"}`},
		{"darwin, no key", "darwin", `{}`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "holocron.config.json")
			if err := os.WriteFile(file, []byte(r.json), 0o600); err != nil {
				t.Fatal(err)
			}
			plat := &osx.Fake{OS: r.goos, DirsValue: osx.DefaultDirs(r.goos, osx.Env{Getenv: os.Getenv, Home: local})}
			cfg, _, err := config.Load(file, plat)
			if err != nil {
				t.Fatal(err)
			}
			supervisor := relay.LiveSecretPath(plat, cfg)
			// The holocron web role: its config file, and the platform's auth folder as the default.
			web := (&API{cfg: Config{ConfigPath: file, LiveSecretDir: plat.Dirs().Auth}}).liveSettings().secretPath
			if supervisor == "" || supervisor != web {
				t.Errorf("supervisor opens %q, web opens %q", supervisor, web)
			}
		})
	}
}

// camui.exe keeps the PowerShell system's file when nothing is configured.
func TestLiveSecretLegacyCamuiDefault(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	file := filepath.Join(t.TempDir(), "recorder.config.json")
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := (&API{cfg: Config{ConfigPath: file}}).liveSettings().secretPath
	if want := filepath.Join(local, "IPCCTV", "live_view_secret.txt"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	t.Setenv("LOCALAPPDATA", "")
	if got := (&API{cfg: Config{ConfigPath: file}}).liveSettings().secretPath; got != "" {
		t.Errorf("without LOCALAPPDATA got %q, want none", got)
	}
}

func quoteJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
