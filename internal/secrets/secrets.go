package secrets

import (
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// Errors. Messages never carry a secret value.
var (
	ErrNotFound     = errors.New("secrets: not found")
	ErrReadOnly     = errors.New("secrets: this backend is read-only")
	ErrUnsupported  = errors.New("secrets: backend not supported on this OS")
	ErrInvalidName  = errors.New("secrets: invalid name (want [a-z0-9._-], 1 to 64 characters)")
	ErrInvalidValue = errors.New("secrets: invalid value (want 1 to 4096 bytes of UTF-8)")
	ErrAuth         = errors.New("secrets: the store failed authentication (tampered, or the wrong key)")
	ErrFormat       = errors.New("secrets: the store file is malformed")
	ErrNoKey        = errors.New("secrets: the store exists but its key is missing")
)

const (
	MaxNameLen  = 64
	MaxValueLen = 4096
)

// Backend names, as returned by Provider.Backend and accepted in Config.Backend.
const (
	BackendAuto    = "auto"
	BackendKeyring = "keyring"
	BackendFile    = "file"
	BackendEnv     = "env"
)

// Names of the camera login, shared by every camera (camera_creds.xml held one login).
const (
	CameraUserName     = "camera.user"
	CameraPasswordName = "camera.password"
)

// Provider is a store of named secrets. Safe for concurrent use.
type Provider interface {
	// Get returns the value; an absent name gives an error wrapping ErrNotFound.
	Get(name string) (string, error)
	// Put creates or overwrites; the env backend returns ErrReadOnly.
	Put(name, value string) error
	// Delete removes; an absent name gives an error wrapping ErrNotFound.
	Delete(name string) error
	// List returns the names, sorted, never values.
	List() ([]string, error)
	// Backend is "keyring", "file" or "env".
	Backend() string
}

// Redactor is told every secret that is read or stored so later log lines mask it.
// *logx.Redactor satisfies it.
type Redactor interface{ Register(secret string) }

// ValidName reports whether name matches [a-z0-9._-]{1,64}.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > MaxNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func checkName(name string) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	return nil
}

func checkValue(v string) error {
	if len(v) == 0 || len(v) > MaxValueLen || !utf8.ValidString(v) {
		return ErrInvalidValue
	}
	return nil
}

func notFound(name string) error { return fmt.Errorf("secrets: %s: %w", name, ErrNotFound) }

func sorted(names []string) []string {
	sort.Strings(names)
	return names
}
