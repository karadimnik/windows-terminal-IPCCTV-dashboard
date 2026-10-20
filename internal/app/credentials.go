package app

import (
	"fmt"
	"os"

	"holocron/internal/config"
	"holocron/internal/supervise"
)

// Environment variables holding the camera login until the secrets store is plugged in.
const (
	EnvCameraUser     = "HOLOCRON_CAMERA_USER"
	EnvCameraPassword = "HOLOCRON_CAMERA_PASSWORD"
)

// Credentials hands out the camera login (one login for every camera, as camera_creds.xml today).
type Credentials interface {
	CameraLogin() (user, password string, err error)
}

// CredentialProvider is the seam where the secrets package plugs in (internal/secrets, built in
// parallel): it returns the store for the configuration's secrets.backend. nil, or a provider that
// returns (nil, nil), means the environment variables.
var CredentialProvider func(cfg config.Config) (Credentials, error)

// EnvCredentials reads EnvCameraUser and EnvCameraPassword.
type EnvCredentials struct{ Getenv func(string) string }

func (e EnvCredentials) CameraLogin() (string, string, error) {
	get := e.Getenv
	if get == nil {
		get = os.Getenv
	}
	user, pass := get(EnvCameraUser), get(EnvCameraPassword)
	switch {
	case user == "":
		return "", "", fmt.Errorf("no camera login: set %s (and %s) for the supervisor", EnvCameraUser, EnvCameraPassword)
	case pass == "":
		return "", "", fmt.Errorf("no camera password: set %s for the supervisor", EnvCameraPassword)
	}
	return user, pass, nil
}

// CredentialsFor picks the camera credential store for cfg.
func CredentialsFor(cfg config.Config, getenv func(string) string) (Credentials, error) {
	if CredentialProvider != nil {
		c, err := CredentialProvider(cfg)
		if err != nil || c != nil {
			return c, err
		}
	}
	return EnvCredentials{Getenv: getenv}, nil
}

// CheckCredentials loads the login once: a missing one is a configuration fault (exit 21) naming
// the variable, before the supervisor starts anything.
func CheckCredentials(c Credentials) error {
	if _, _, err := c.CameraLogin(); err != nil {
		return &supervise.ConfigFault{Err: err}
	}
	return nil
}

// relaySource adapts Credentials to relay.CredentialSource (every camera has the same login).
type relaySource struct{ c Credentials }

func (r relaySource) CameraCredential(config.Camera) (string, string, error) {
	return r.c.CameraLogin()
}
