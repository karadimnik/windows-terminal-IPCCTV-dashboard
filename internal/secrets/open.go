package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"holocron/internal/osx"
)

// Config is the "secrets" section of holocron.config.json (keys below). The config package does
// not know it yet; the JSON tags and these comments are the contract for when it does.
type Config struct {
	// Backend is secrets.backend: "auto" (default when empty), "keyring", "file" or "env".
	// auto: Windows uses the keyring when the Credential Manager answers, else the file;
	// Linux and macOS use the file (no Secret Service without a dependency).
	Backend string `json:"backend"`
	// File is secrets.file: the encrypted store; default <auth dir>/secrets.enc.
	File string `json:"file"`
	// KeyFile is secrets.keyFile: the owner-only key file used when HOLOCRON_SECRET_KEY is not
	// set; default <auth dir>/secrets.key.
	KeyFile string `json:"keyFile"`

	// Redactor, when set, receives every value that is read or stored.
	Redactor Redactor `json:"-"`
	// Platform supplies Dirs and SetOwnerOnly; default osx.Current().
	Platform osx.Platform `json:"-"`
	// Environ lists the environment ("K=V"); default os.Environ.
	Environ func() []string `json:"-"`

	keyringPrefix string // tests only; default "holocron/"
}

func (c Config) platform() osx.Platform {
	if c.Platform != nil {
		return c.Platform
	}
	return osx.Current()
}

func (c Config) environ() func() []string {
	if c.Environ != nil {
		return c.Environ
	}
	return os.Environ
}

func (c Config) prefix() string {
	if c.keyringPrefix != "" {
		return c.keyringPrefix
	}
	return "holocron/"
}

// Open builds the backend selected by cfg.Backend, wrapped so that values are registered with
// cfg.Redactor.
func Open(cfg Config) (Provider, error) {
	var p Provider
	var err error
	switch strings.ToLower(cfg.Backend) {
	case "", BackendAuto:
		p, err = openAuto(cfg)
	case BackendKeyring:
		p, err = newKeyring(cfg.prefix())
	case BackendFile:
		p, err = newFile(cfg)
	case BackendEnv:
		p = newEnv(cfg.environ())
	default:
		return nil, fmt.Errorf("secrets: unknown backend %q (want auto, keyring, file or env)", cfg.Backend)
	}
	if err != nil {
		return nil, err
	}
	return WithRedactor(p, cfg.Redactor), nil
}

func openAuto(cfg Config) (Provider, error) {
	if cfg.platform().Name() == "windows" {
		p, err := newKeyring(cfg.prefix())
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrUnsupported) && !errors.Is(err, errKeyringUnusable) {
			return nil, err
		}
	}
	return newFile(cfg)
}

func (c Config) paths() (file, key string, err error) {
	file, key = c.File, c.KeyFile
	if file == "" || key == "" {
		auth := c.platform().Dirs().Auth
		if auth == "" {
			return "", "", errors.New("secrets: no auth directory known for this OS; set secrets.file and secrets.keyFile")
		}
		if file == "" {
			file = filepath.Join(auth, "secrets.enc")
		}
		if key == "" {
			key = filepath.Join(auth, "secrets.key")
		}
	}
	return file, key, nil
}

var errKeyringUnusable = errors.New("secrets: the Credential Manager is not usable in this session")

type redacting struct {
	Provider
	r Redactor
}

// WithRedactor registers every successfully read or stored value with r. A nil r returns p.
func WithRedactor(p Provider, r Redactor) Provider {
	if r == nil {
		return p
	}
	return redacting{p, r}
}

func (w redacting) Get(name string) (string, error) {
	v, err := w.Provider.Get(name)
	if err == nil {
		w.r.Register(v)
	}
	return v, err
}

func (w redacting) Put(name, value string) error {
	err := w.Provider.Put(name, value)
	if err == nil {
		w.r.Register(value)
	}
	return err
}
