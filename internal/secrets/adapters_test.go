package secrets_test

import (
	"errors"
	"testing"

	"holocron/internal/alerts"
	"holocron/internal/config"
	"holocron/internal/logx"
	"holocron/internal/relay"
	"holocron/internal/secrets"
)

// Compile-time assertions: the adapters satisfy the interfaces of their consumers.
var (
	_ relay.CredentialSource = secrets.CredentialSource(nil)
	_ alerts.SecretLookup    = secrets.Lookup(nil)
	_ secrets.Redactor       = (*logx.Redactor)(nil)
)

func envProvider(t *testing.T, kv ...string) secrets.Provider {
	t.Helper()
	p, err := secrets.Open(secrets.Config{Backend: "env", Environ: func() []string { return kv }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRelayAdapterGivesEveryCameraTheSameLogin(t *testing.T) {
	p := envProvider(t, "HOLOCRON_CAMERA_USER=admin", "HOLOCRON_CAMERA_PASSWORD=pw-for-all")
	var src relay.CredentialSource = secrets.CredentialSource(p)
	for _, cam := range []config.Camera{{Name: "a", IP: "10.0.0.1"}, {Name: "b", IP: "10.0.0.2"}} {
		u, pw, err := src.CameraCredential(cam)
		if err != nil || u != "admin" || pw != "pw-for-all" {
			t.Errorf("%s: %q %q %v", cam.Name, u, pw, err)
		}
	}
	env, err := relay.SourceEnv([]config.Camera{{Name: "front", IP: "10.0.0.9"}}, src)
	if err != nil || len(env) != 1 {
		t.Fatalf("SourceEnv = %v, %v", env, err)
	}
	if _, _, err := secrets.CredentialSource(envProvider(t)).CameraCredential(config.Camera{}); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("no login: %v", err)
	}
}

func TestLookupAdapter(t *testing.T) {
	var look alerts.SecretLookup = secrets.Lookup(envProvider(t, "HOLOCRON_SECRET_PUSH_TOKEN=tok-9"))
	if v, ok := look("push.token"); !ok || v != "tok-9" {
		t.Errorf("found = %q, %v", v, ok)
	}
	if _, ok := look("absent"); ok {
		t.Error("absent found")
	}
	if _, ok := look("Bad Name"); ok {
		t.Error("an invalid name must be not found")
	}
}
