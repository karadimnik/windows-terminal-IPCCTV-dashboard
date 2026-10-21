// Package app is the production wiring of holocron: paths, logging, credentials, the supervisor
// and host roles with every merged package plugged in, the configuration and camera edits shared
// by the command line and the control API, and the process census of `holocron down`.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"holocron/internal/config"
	"holocron/internal/osx"
)

// SandboxEnv: when "1", every root is checked by CheckSandboxRoot (tests set it).
const SandboxEnv = "HOLOCRON_SANDBOX_ONLY"

// Paths are the files one invocation works on.
type Paths struct {
	Root    string // the archive root (RootPath, or --root)
	Config  string // holocron.config.json
	Cameras string // cameras.json, next to the configuration
}

func (p Paths) State() string      { return filepath.Join(p.Root, "_state") }
func (p Paths) Logs() string       { return filepath.Join(p.Root, "_logs") }
func (p Paths) StatusFile() string { return filepath.Join(p.State(), "status.json") }
func (p Paths) StopFile() string   { return filepath.Join(p.State(), "stop.request") }
func (p Paths) HostFile() string   { return filepath.Join(p.State(), "host.json") }
func (p Paths) AuxFile() string    { return filepath.Join(p.State(), "webchild.json") } // the host's own children (the web role)
func (p Paths) LockFile() string   { return osx.LockPath(p.State()) }

// DefaultConfigPath is holocron.config.json in the platform's configuration folder.
func DefaultConfigPath(plat osx.Platform) string {
	return filepath.Join(plat.Dirs().Config, "holocron.config.json")
}

// Resolve loads the configuration (a missing file is the defaults) and settles the paths: root
// overrides RootPath when given. The returned Config carries the effective RootPath.
func Resolve(root, configPath string, plat osx.Platform) (Paths, config.Config, []config.Warning, error) {
	if configPath == "" {
		configPath = DefaultConfigPath(plat)
	}
	configPath = absPath(configPath)
	cfg, warns, err := config.Load(configPath, plat)
	p := Paths{Config: configPath, Cameras: filepath.Join(filepath.Dir(configPath), "cameras.json")}
	if err != nil {
		return p, cfg, warns, fmt.Errorf("%s: %w", configPath, err)
	}
	if root != "" {
		cfg.RootPath = absPath(root)
	}
	p.Root = cfg.RootPath
	if os.Getenv(SandboxEnv) == "1" {
		if err := CheckSandboxRoot(p.Root); err != nil {
			return p, cfg, warns, err
		}
	}
	return p, cfg, warns, nil
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// ErrSandbox is a root refused by the sandbox guard.
var ErrSandbox = errors.New("refused by the sandbox guard (" + SandboxEnv + "=1)")

// CheckSandboxRoot refuses a root that could be the Operator's: a drive or filesystem root, a
// drive-relative path (C:, C:x: wherever that drive's current folder is), a path
// with a folder named "cameras" in it (E:\cameras, /srv/cameras), or a folder that holds what the
// PowerShell recorder writes (recorder.log, camera_creds.xml).
func CheckSandboxRoot(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("%w: no root", ErrSandbox)
	}
	if v := filepath.VolumeName(root); len(v) == 2 && !filepath.IsAbs(root) {
		return fmt.Errorf("%w: %s is relative to the current folder of drive %s", ErrSandbox, root, v)
	}
	clean := filepath.Clean(absPath(root))
	if vol := filepath.VolumeName(clean); clean == vol+string(filepath.Separator) || clean == vol || clean == string(filepath.Separator) {
		return fmt.Errorf("%w: %s is a drive or filesystem root", ErrSandbox, root)
	}
	for _, part := range strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, "cameras") {
			return fmt.Errorf("%w: %s has a folder named %q (an Operator archive)", ErrSandbox, root, part)
		}
	}
	for _, f := range []string{filepath.Join("_logs", "recorder.log"), "recorder.log", "camera_creds.xml", filepath.Join("_state", "camui.log")} {
		if _, err := os.Lstat(filepath.Join(clean, f)); err == nil {
			return fmt.Errorf("%w: %s holds %s (Operator data)", ErrSandbox, root, f)
		}
	}
	return nil
}
