package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"holocron/internal/config"
	"holocron/internal/secrets"
)

func fileStoreConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults(plat)
	cfg.Secrets = config.Secrets{Backend: "file", File: filepath.Join(dir, "s.enc"), KeyFile: filepath.Join(dir, "s.key")}
	return cfg
}

func TestCredentialsPreferTheEnvironmentThenTheStore(t *testing.T) {
	t.Setenv("HOLOCRON_SECRET_KEY", "")
	cfg := fileStoreConfig(t)
	p, err := OpenSecrets(cfg, plat, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Put(secrets.CameraUserName, "store-user"); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(secrets.CameraPasswordName, "store-pass-1"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	get := func(k string) string { return env[k] }

	c, err := CredentialsWith(cfg, plat, get, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u, pw, err := c.CameraLogin(); err != nil || u != "store-user" || pw != "store-pass-1" {
		t.Fatalf("store: %q %q %v", u, pw, err)
	}
	env[EnvCameraUser], env[EnvCameraPassword] = "env-user", "env-pass"
	if u, pw, err := c.CameraLogin(); err != nil || u != "env-user" || pw != "env-pass" {
		t.Fatalf("the environment must win: %q %q %v", u, pw, err)
	}
	// one variable alone is a mistake, not a fallback
	delete(env, EnvCameraPassword)
	if _, _, err := c.CameraLogin(); err == nil || !strings.Contains(err.Error(), EnvCameraPassword) {
		t.Fatalf("half an environment login: %v", err)
	}
}

func TestMissingCredentialNamesBothPlaces(t *testing.T) {
	t.Setenv("HOLOCRON_SECRET_KEY", "")
	cfg := fileStoreConfig(t)
	c, _ := CredentialsWith(cfg, plat, func(string) string { return "" }, nil)
	err := CheckCredentials(c)
	if err == nil {
		t.Fatal("no login anywhere must fail")
	}
	for _, want := range []string{EnvCameraUser, EnvCameraPassword, "holocron secret set camera.user", "file secrets store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %v", want, err)
		}
	}
	// an unusable store is reported as such
	bad := cfg
	bad.Secrets.Backend = "keyring"
	if plat.Name() != "windows" {
		c, _ = CredentialsWith(bad, plat, func(string) string { return "" }, nil)
		if err := CheckCredentials(c); err == nil || !errors.Is(err, secrets.ErrUnsupported) {
			t.Errorf("keyring off Windows: %v", err)
		}
	}
}

type recordingRedactor struct{ got []string }

func (r *recordingRedactor) Register(s string) { r.got = append(r.got, s) }

func TestCredentialValuesAreRegisteredWithTheRedactor(t *testing.T) {
	cfg := fileStoreConfig(t)
	red := &recordingRedactor{}
	c, _ := CredentialsWith(cfg, plat, func(k string) string {
		return map[string]string{EnvCameraUser: "u", EnvCameraPassword: "env-secret-pw"}[k]
	}, red)
	if _, _, err := c.CameraLogin(); err != nil || len(red.got) != 1 || red.got[0] != "env-secret-pw" {
		t.Fatalf("%v %v", red.got, err)
	}
}
