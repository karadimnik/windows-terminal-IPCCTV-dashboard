package secrets

import (
	"errors"
	"strings"
	"testing"
)

func envOf(kv ...string) func() []string { return func() []string { return kv } }

func TestEnvBackend(t *testing.T) {
	p := newEnv(envOf(
		"HOLOCRON_SECRET_PUSH_TOKEN=tok-1",
		"HOLOCRON_SECRET_API_KEY_V2=k2",
		"HOLOCRON_SECRET_CAMERA_USER=primary-user",
		"HOLOCRON_CAMERA_USER=legacy-user",
		"HOLOCRON_CAMERA_PASSWORD=legacy-pass",
		"HOLOCRON_SECRET_KEY="+keyA,
		"HOLOCRON_SECRET_EMPTY=",
		"=C:=C:\\odd",
		"PATH=/bin",
	))
	if p.Backend() != "env" {
		t.Fatal(p.Backend())
	}
	cases := []struct{ name, want string }{
		{"push.token", "tok-1"}, {"push-token", "tok-1"}, {"push_token", "tok-1"},
		{"api-key.v2", "k2"},
		{"camera.user", "primary-user"}, // the new variable wins over the legacy one
		{"camera.password", "legacy-pass"},
	}
	for _, c := range cases {
		if got, err := p.Get(c.name); err != nil || got != c.want {
			t.Errorf("Get(%s) = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	for _, n := range []string{"missing", "empty", "key", "path"} { // key is the file backend's key, never a secret
		if _, err := p.Get(n); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound", n, err)
		}
	}
	if err := p.Put("push.token", "x-value"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Put: %v", err)
	}
	if err := p.Delete("push.token"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete: %v", err)
	}
	if err := p.Put("BAD", "x-value"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Put bad name: %v", err)
	}
	names, err := p.List()
	if err != nil || strings.Join(names, ",") != "api_key_v2,camera.password,camera.user,push_token" {
		t.Errorf("List = %v, %v", names, err)
	}
	if _, err := p.Get("Bad Name"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Get bad name: %v", err)
	}
}

func TestEnvLegacyOnly(t *testing.T) {
	p := newEnv(envOf("HOLOCRON_CAMERA_USER=u1", "HOLOCRON_CAMERA_PASSWORD=p1-secret"))
	u, pw, err := CameraLogin(p)
	if err != nil || u != "u1" || pw != "p1-secret" {
		t.Fatalf("CameraLogin = %q, %q, %v", u, pw, err)
	}
	if _, _, err := CameraLogin(newEnv(envOf("HOLOCRON_CAMERA_USER=u1"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("half a login: %v", err)
	}
}
