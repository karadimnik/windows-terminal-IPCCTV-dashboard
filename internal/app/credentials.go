package app

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"holocron/internal/config"
	"holocron/internal/osx"
	"holocron/internal/secrets"
	"holocron/internal/supervise"
)

// Environment variables holding the camera login. They win over the secrets store (precedence:
// both variables, then the store `secrets.backend` selects).
const (
	EnvCameraUser     = "HOLOCRON_CAMERA_USER"
	EnvCameraPassword = "HOLOCRON_CAMERA_PASSWORD"
)

// Credentials hands out the camera login (one login for every camera, as camera_creds.xml today).
type Credentials interface {
	CameraLogin() (user, password string, err error)
}

// CredentialProvider overrides the credential source (tests and embedders): it returns the store
// to use, or (nil, nil) for the default order. nil means the default.
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

// SecretsConfig is the secrets.Config the configuration describes (the process environment
// supplies HOLOCRON_SECRET_KEY and the env backend's variables).
func SecretsConfig(cfg config.Config, plat osx.Platform, red secrets.Redactor) secrets.Config {
	return secrets.Config{Backend: cfg.Secrets.Backend, File: cfg.Secrets.File, KeyFile: cfg.Secrets.KeyFile, Redactor: red, Platform: plat}
}

// OpenSecrets opens the store `secrets.backend` selects (red learns every value read or stored).
func OpenSecrets(cfg config.Config, plat osx.Platform, red secrets.Redactor) (secrets.Provider, error) {
	return secrets.Open(SecretsConfig(cfg, plat, red))
}

// storeCredentials is the camera login of the environment when both variables are set, else of
// the secrets store (opened on first use). A missing login names both places.
type storeCredentials struct {
	env  EnvCredentials
	red  secrets.Redactor
	open func() (secrets.Provider, error)

	once sync.Once
	p    secrets.Provider
	err  error
}

func (c *storeCredentials) CameraLogin() (string, string, error) {
	get := c.env.Getenv
	if get == nil {
		get = os.Getenv
	}
	user, pass := get(EnvCameraUser), get(EnvCameraPassword)
	switch {
	case user != "" && pass != "":
		if c.red != nil {
			c.red.Register(pass)
		}
		return user, pass, nil
	case user != "":
		return "", "", fmt.Errorf("no camera password: %s is set but %s is not", EnvCameraUser, EnvCameraPassword)
	case pass != "":
		return "", "", fmt.Errorf("no camera login: %s is set but %s is not", EnvCameraPassword, EnvCameraUser)
	}
	c.once.Do(func() { c.p, c.err = c.open() })
	if c.err != nil {
		return "", "", fmt.Errorf("no camera login: %s and %s are not set and the secrets store cannot be opened: %w", EnvCameraUser, EnvCameraPassword, c.err)
	}
	u, p, err := secrets.CameraLogin(c.p)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return "", "", fmt.Errorf("no camera login: set %s and %s, or store it in the %s secrets store with `holocron secret set %s` and `holocron secret set %s` (Windows: `holocron secret import`)",
				EnvCameraUser, EnvCameraPassword, c.p.Backend(), secrets.CameraUserName, secrets.CameraPasswordName)
		}
		return "", "", fmt.Errorf("no camera login: the %s secrets store failed: %w", c.p.Backend(), err)
	}
	return u, p, nil
}

// CredentialsFor picks the camera credential source for cfg: the two environment variables, then
// the configured secrets store.
func CredentialsFor(cfg config.Config, getenv func(string) string) (Credentials, error) {
	return CredentialsWith(cfg, osx.Current(), getenv, nil)
}

// CredentialsWith is CredentialsFor with the platform and a redactor for the values read.
func CredentialsWith(cfg config.Config, plat osx.Platform, getenv func(string) string, red secrets.Redactor) (Credentials, error) {
	if CredentialProvider != nil {
		c, err := CredentialProvider(cfg)
		if err != nil || c != nil {
			return c, err
		}
	}
	return &storeCredentials{env: EnvCredentials{Getenv: getenv}, red: red, open: func() (secrets.Provider, error) {
		return OpenSecrets(cfg, plat, red)
	}}, nil
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
