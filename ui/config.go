package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// minTokenLength is the shortest token that counts as "a token is configured"
// for the loopback guard. A one-character token would satisfy the letter of the
// guard while defeating its purpose.
const minTokenLength = 16

// defaultListen keeps the server on the loopback interface unless the Operator
// changes it deliberately.
const defaultListen = "127.0.0.1:8787"

// defaultArchiveRoot is the last resort, used only when nothing is configured
// here and the recorder's own RootPath cannot be read. It is the recorder's own
// built-in default, so the two agree on a machine that has configured neither.
const defaultArchiveRoot = `E:\cameras`

// Config is the whole of camui's configuration. It comes from a JSON file, from
// flags, and - for the token only - from the environment.
type Config struct {
	// Listen is the host:port to bind. Anything that does not resolve
	// exclusively to loopback addresses requires Token.
	Listen string `json:"listen"`

	// ArchiveRoot is the recorder's RootPath. Left unset it is read from
	// RootPath in recorder.config.json, so moving the archive needs no
	// second edit here. StatusFile defaults to
	// <ArchiveRoot>\_state\status.json.
	ArchiveRoot string `json:"archiveRoot"`

	// StatusFile overrides the derived status file path.
	StatusFile string `json:"statusFile"`

	// ScriptDir is the directory holding cam.ps1, cam.recorder.ps1 and
	// cam.common.ps1, and - beside them - cameras.json and
	// recorder.config.json. Control actions and settings writes are performed
	// by those scripts, never by camui itself, so that the CLI stays the one
	// implementation of every state change.
	ScriptDir string `json:"scriptDir"`

	// Token, when set, is required on every request. It is read from the
	// config file or from CAMUI_TOKEN, never from a flag: a command line is
	// readable by every process on the machine.
	Token string `json:"token"`
}

// CamerasFile is the camera list the scripts read, beside them.
func (c Config) CamerasFile() string { return filepath.Join(c.ScriptDir, "cameras.json") }

// RecorderConfigFile is the recorder's own config, beside the scripts.
func (c Config) RecorderConfigFile() string {
	return filepath.Join(c.ScriptDir, "recorder.config.json")
}

// LoadConfig merges the config file (if any), the flag values, and CAMUI_TOKEN,
// then fills in defaults. A flag wins over the file; an empty flag means unset.
//
// The archive root has three sources in this order: whatever is set here by
// flag or config file, then RootPath from the recorder's own
// recorder.config.json, then defaultArchiveRoot. The middle one exists because
// the recorder is the authority on where it writes, and a UI pointed at the
// wrong root does not fail - it shows an empty archive and says nothing.
func LoadConfig(configPath string, flags Config) (Config, error) {
	cfg := Config{}

	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", configPath, err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", configPath, err)
		}
	}

	if flags.Listen != "" {
		cfg.Listen = flags.Listen
	}
	if flags.ArchiveRoot != "" {
		cfg.ArchiveRoot = flags.ArchiveRoot
	}
	if flags.StatusFile != "" {
		cfg.StatusFile = flags.StatusFile
	}
	if flags.ScriptDir != "" {
		cfg.ScriptDir = flags.ScriptDir
	}
	if env := strings.TrimSpace(os.Getenv("CAMUI_TOKEN")); env != "" {
		cfg.Token = env
	}

	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	// The script directory is resolved first because the recorder's config -
	// the next source for the archive root - is found relative to it.
	if cfg.ScriptDir == "" {
		// The binary is built into the repository root beside the scripts, so
		// its own directory is the right default and needs no configuration on
		// the Operator's machine.
		exe, err := os.Executable()
		if err != nil {
			return cfg, fmt.Errorf("locate own executable to default the script directory: %w", err)
		}
		cfg.ScriptDir = filepath.Dir(exe)
	}
	if cfg.ArchiveRoot == "" {
		cfg.ArchiveRoot = recorderRootPath(cfg.RecorderConfigFile())
	}
	if cfg.ArchiveRoot == "" {
		cfg.ArchiveRoot = defaultArchiveRoot
	}
	if cfg.StatusFile == "" {
		cfg.StatusFile = filepath.Join(cfg.ArchiveRoot, "_state", "status.json")
	}
	cfg.Token = strings.TrimSpace(cfg.Token)

	return cfg, nil
}

// recorderRootPath returns RootPath from the recorder's own configuration, or
// "" when the file is absent, unreadable, not JSON, or carries no RootPath.
// Every one of those falls through to the next default rather than refusing to
// start: a recorder that has not been configured yet is not a reason to serve
// nothing, and the file is the Operator's to get right.
func recorderRootPath(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal(stripBOM(raw), &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(jsonString(parsed["RootPath"]))
}

// Validate enforces the loopback guard: camui refuses to start on any address
// that is not exclusively loopback unless a token of real length is configured.
// That makes exposing it beyond this machine a deliberate act rather than an
// accident, and makes unauthenticated exposure impossible rather than merely
// discouraged.
func (c Config) Validate() error {
	loopback, err := listenIsLoopback(c.Listen)
	if err != nil {
		return err
	}
	if loopback {
		return nil
	}
	if len(c.Token) >= minTokenLength {
		return nil
	}

	detail := "no auth token is configured"
	if c.Token != "" {
		detail = fmt.Sprintf("the configured auth token is %d characters, shorter than the %d required", len(c.Token), minTokenLength)
	}
	return fmt.Errorf("refusing to start: listen address %q is not loopback and %s.\n"+
		"  Bind 127.0.0.1 to keep camui local, or set a token of at least %d characters\n"+
		"  (CAMUI_TOKEN in the environment, or \"token\" in the config file) to expose it deliberately",
		c.Listen, detail, minTokenLength)
}

// RequiresToken reports whether requests must carry the token.
func (c Config) RequiresToken() bool { return c.Token != "" }

// listenIsLoopback reports whether every address the listen host resolves to is
// a loopback address. An empty host ("" or ":8787") means every interface and is
// therefore not loopback.
func listenIsLoopback(listen string) (bool, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return false, fmt.Errorf("listen address %q is not host:port: %w", listen, err)
	}
	if port == "" {
		return false, fmt.Errorf("listen address %q has no port", listen)
	}
	if host == "" {
		return false, nil
	}

	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback(), nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return false, fmt.Errorf("cannot resolve listen host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return false, errors.New("listen host " + host + " resolved to no addresses")
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false, nil
		}
	}
	return true, nil
}
