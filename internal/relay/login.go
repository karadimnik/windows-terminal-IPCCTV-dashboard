package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"holocron/internal/config"
	"holocron/internal/osx"
)

const (
	// APIUser is the product's own login for the relay's control API.
	APIUser = "camrelayapi"
	// LiveUser is the one user of the live instance.
	LiveUser = "camlive"
)

// Login is a user name and the secret behind it. The secret never reaches a config file (only its
// hash does), a command line or a log: printing a Login shows the user alone.
type Login struct {
	User   string
	Secret string
}

// PassHash is Get-CamRelayApiPassHash: "sha256:" plus the base64 of the SHA-256 of the UTF-8 secret
// (the form MediaMTX documents for a hashed internal password). It is the hash of the 64-character
// hex text, not of the 32 raw bytes.
func (l Login) PassHash() string {
	sum := sha256.Sum256([]byte(l.Secret))
	return "sha256:" + base64.StdEncoding.EncodeToString(sum[:])
}

func (l Login) String() string   { return "relay.Login{" + l.User + "}" }
func (l Login) GoString() string { return l.String() }

// Zero reports whether there is no login yet.
func (l Login) Zero() bool { return l.User == "" && l.Secret == "" }

// ErrNoSecret is returned when a secret file does not exist and creation was not asked for.
var ErrNoSecret = errors.New("relay: secret file does not exist")

var secretRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DefaultAPICredentialPath is where the API login's secret lives when nothing says otherwise: the
// platform's auth folder (owner-only, local). PowerShell kept it beside the scripts, DPAPI protected.
func DefaultAPICredentialPath(p osx.Platform) string {
	return filepath.Join(p.Dirs().Auth, "relay_api_secret.txt")
}

// LiveSecretPath is Get-CamLiveSecretPath: LiveSecretPath, or live_view_secret.txt in the platform's
// auth folder. (PowerShell used %LOCALAPPDATA%\IPCCTV on Windows; set LiveSecretPath to keep sharing
// that file with camui during the shadow phase.)
func LiveSecretPath(p osx.Platform, cfg config.Config) string {
	if strings.TrimSpace(cfg.LiveSecretPath) != "" {
		return cfg.LiveSecretPath
	}
	return filepath.Join(p.Dirs().Auth, "live_view_secret.txt")
}

// LoadAPICredential is Get-CamRelayApiCredential: user camrelayapi, the secret 32 random bytes as 64
// hex characters in an owner-only file. With create, a missing file is generated once and reused by
// every later call, install and restart; without it a missing file is ErrNoSecret.
func LoadAPICredential(p osx.Platform, path string, create bool) (Login, error) {
	secret, err := loadSecret(p, path, create)
	if err != nil {
		return Login{}, err
	}
	return Login{User: APIUser, Secret: secret}, nil
}

// LoadLiveSecret is Get-CamLiveCredential: user camlive, 32 random bytes as 64 lowercase hex
// characters in a plain ASCII file with no newline (camui reads it with os.ReadFile), owner-only.
func LoadLiveSecret(p osx.Platform, path string, create bool) (Login, error) {
	secret, err := loadSecret(p, path, create)
	if err != nil {
		return Login{}, err
	}
	return Login{User: LiveUser, Secret: secret}, nil
}

// loadSecret returns the secret in path, creating it when asked. A file whose content is not 64
// lowercase hex characters (a trailing newline is tolerated) is rejected, never rewritten: the
// operator deletes it to get a new one, as with PowerShell. A file readable by others is repaired
// (made owner-only) before its content is trusted; if that fails the secret is refused.
func loadSecret(p osx.Platform, path string, create bool) (string, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if !create {
			return "", ErrNoSecret
		}
		if data, err = createSecret(p, path); err != nil {
			return "", err
		}
	default:
		return "", err
	}

	secret := strings.TrimSpace(string(data))
	if !secretRE.MatchString(secret) {
		return "", fmt.Errorf("The secret file '%s' does not hold 64 hex characters. Delete it and a new one is created at the next start.", path)
	}
	if err := ensureOwnerOnly(p, path); err != nil {
		return "", fmt.Errorf("relay: secret file %s is not owner-only and could not be restricted: %w", path, err)
	}
	return secret, nil
}

// ensureOwnerOnly repairs permissions that let anyone but the owner in. On Unix a mode check avoids
// a needless chmod; on Windows the ACL is not cheap to read back, so SetOwnerOnly (which resets
// explicit entries too) runs every time.
func ensureOwnerOnly(p osx.Platform, path string) error {
	if p.Name() != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		if fi.Mode().Perm()&0o077 == 0 {
			return nil
		}
	}
	return p.SetOwnerOnly(path)
}

// createSecret writes a new secret. The temp file is created with mode 0600 and restricted to its
// owner while still empty, so there is no moment at which the secret sits in a file others can read;
// then it is renamed into place.
func createSecret(p osx.Platform, path string) ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	text := []byte(hex.EncodeToString(raw))

	if dir := filepath.Dir(path); dir != "" {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) ([]byte, error) {
		f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := p.SetOwnerOnly(tmp); err != nil {
		return fail(err)
	}
	if _, err := f.Write(text); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return text, nil
}

// replaceFile is Move-CamFileAtomic: rename over the target, retrying while a reader that opened the
// destination without share-delete holds it (10 tries, 100 ms apart).
func replaceFile(tmp, path string) error {
	var err error
	for attempt := 1; attempt <= 10; attempt++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// WriteConfigIfChanged writes a generated config (UTF-8, no BOM, exact bytes) to path through a temp
// file and a rename, because MediaMTX watches the file and must never read a half-written one. It
// does nothing, and returns false, when the file already holds exactly these bytes. A missing folder
// is created.
func WriteConfigIfChanged(path string, text []byte) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(text) {
		return false, nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(text); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return false, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}
