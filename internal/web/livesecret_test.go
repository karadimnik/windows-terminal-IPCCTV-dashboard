package web

import (
	"os"
	"path/filepath"
	"testing"
)

// LiveSecretFileOf is the web role's own resolution: LiveSecretDir, overridden by LiveSecretPath in
// the recorder configuration.
func TestLiveSecretFileOf(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "holocron.config.json")
	os.WriteFile(cfgPath, []byte(`{"LiveViewEnabled": true}`), 0o644)
	if got := LiveSecretFileOf(Config{LiveSecretDir: dir, ConfigPath: cfgPath}); got != filepath.Join(dir, "live_view_secret.txt") {
		t.Errorf("default: %q", got)
	}
	other := filepath.Join(dir, "other.txt")
	os.WriteFile(cfgPath, []byte(`{"LiveSecretPath": "`+filepath.ToSlash(other)+`"}`), 0o644)
	if got := LiveSecretFileOf(Config{LiveSecretDir: dir, ConfigPath: cfgPath}); filepath.Clean(got) != other {
		t.Errorf("LiveSecretPath: %q", got)
	}
}
