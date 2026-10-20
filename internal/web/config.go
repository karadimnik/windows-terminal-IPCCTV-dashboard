package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
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

	// AllowedHosts are extra Host header names (port ignored) the request
	// guard accepts beside localhost, 127.0.0.1, ::1 and the listen host. See
	// guard.go. Config file only.
	AllowedHosts []string `json:"allowedHosts"`

	// TLSListen, when set, adds an HTTPS listener on a private IPv4:port of this
	// PC (for example 192.168.1.50:8443) for phones and other PCs on the LAN. The
	// loopback listener above is unaffected. Off (empty) by default. Config file
	// or -tls-listen only.
	TLSListen string `json:"tlsListen"`

	// TLSHosts are extra names the server certificate covers (private IPv4
	// literals or names ending .lan, .local, .home.arpa, .internal). The TLSListen
	// address, 127.0.0.1 and localhost are always covered.
	TLSHosts []string `json:"tlsHosts"`

	// TLSDir holds the local CA and the server certificate with their keys.
	// Default %LOCALAPPDATA%\IPCCTV\tls. Never inside the repository.
	TLSDir string `json:"tlsDir"`

	// AuthDir holds the LAN listener's users.json, sessions.json and auth.log,
	// owner-only. Default %LOCALAPPDATA%\IPCCTV\camui\auth. Never inside the
	// repository. Config file or -auth-dir only.
	AuthDir string `json:"authDir"`

	// TLSReadOnly makes the HTTPS listener view-only: it serves the Live and
	// Footage screens and the read-only API, and refuses everything that
	// changes state (start/stop, settings, record toggles, acknowledgements).
	// Absent means true. Only `false` gives a signed-in user full control. It never
	// affects the loopback listener.
	TLSReadOnly *bool `json:"tlsReadOnly"`

	// LiveHiddenGraceSeconds is how long the Live screen keeps its streams
	// connected after its window is hidden or another camui tab is selected,
	// before it frees them. Absent means 300; 0 frees at once (the pre-1C
	// behaviour); -1 never frees. The page reads it from GET /api/live.
	LiveHiddenGraceSeconds *int `json:"liveHiddenGraceSeconds"`

	// CamerasPath and ConfigPath point the web app at cameras.json and the recorder configuration
	// when they are not "cameras.json" and "recorder.config.json" in ScriptDir. The holocron web
	// role sets them to its own files (same key names); they are not part of the camui config file.
	CamerasPath string `json:"-"`
	ConfigPath  string `json:"-"`
}

// defaultLiveHiddenGrace is the grace period used when nothing is configured.
const defaultLiveHiddenGrace = 300

// maxLiveHiddenGrace caps a configured value: a day is "never" in practice and
// a typo such as 3000000 must not be taken literally.
const maxLiveHiddenGrace = 86400

// LiveHiddenGraceEffective is LiveHiddenGraceSeconds with its default, clamped to
// -1 (never free) .. one day.
func (c Config) LiveHiddenGraceEffective() int {
	if c.LiveHiddenGraceSeconds == nil {
		return defaultLiveHiddenGrace
	}
	value := *c.LiveHiddenGraceSeconds
	if value < -1 {
		return -1
	}
	if value > maxLiveHiddenGrace {
		return maxLiveHiddenGrace
	}
	return value
}

// TLSReadOnlyEffective is TLSReadOnly with its default (true).
func (c Config) TLSReadOnlyEffective() bool { return c.TLSReadOnly == nil || *c.TLSReadOnly }

// parseBoolSetting reads a boolean from an environment variable or flag value.
func parseBoolSetting(text string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// CamerasFile is the camera list the scripts read, beside them.
func (c Config) CamerasFile() string {
	if c.CamerasPath != "" {
		return c.CamerasPath
	}
	return filepath.Join(c.ScriptDir, "cameras.json")
}

// RecorderConfigFile is the recorder's own config, beside the scripts.
func (c Config) RecorderConfigFile() string {
	if c.ConfigPath != "" {
		return c.ConfigPath
	}
	return filepath.Join(c.ScriptDir, "recorder.config.json")
}

// AcknowledgedFile is camui's own sidecar, beside status.json in _state. It is
// the one file camui writes itself, and it is separate from status.json
// because the supervisor rewrites that document every few seconds - see
// notice.go.
func (c Config) AcknowledgedFile() string {
	return filepath.Join(filepath.Dir(c.StatusFile), acknowledgedFileName)
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
	return LoadConfigOver(Config{}, configPath, flags)
}

// LoadConfigOver is LoadConfig starting from base instead of the empty configuration: the config
// file overrides base, the flags override both, the defaults fill what is still empty.
func LoadConfigOver(base Config, configPath string, flags Config) (Config, error) {
	cfg := base

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
	if flags.TLSListen != "" {
		cfg.TLSListen = flags.TLSListen
	}
	if flags.TLSDir != "" {
		cfg.TLSDir = flags.TLSDir
	}
	if flags.AuthDir != "" {
		cfg.AuthDir = flags.AuthDir
	}
	if env := strings.TrimSpace(os.Getenv("CAMUI_TOKEN")); env != "" {
		cfg.Token = env
	}
	if flags.TLSReadOnly != nil {
		cfg.TLSReadOnly = flags.TLSReadOnly
	}
	if value, ok := parseBoolSetting(os.Getenv("CAMUI_TLS_READONLY")); ok {
		cfg.TLSReadOnly = &value
	}

	if text := strings.TrimSpace(os.Getenv("CAMUI_LIVE_HIDDEN_GRACE")); text != "" {
		if value, err := strconv.Atoi(text); err == nil {
			cfg.LiveHiddenGraceSeconds = &value
		}
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
	cfg.TLSListen = strings.TrimSpace(cfg.TLSListen)
	if cfg.TLSDir == "" {
		cfg.TLSDir = defaultTLSDir()
	}
	if cfg.AuthDir == "" {
		cfg.AuthDir = defaultAuthDir()
	}

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

// defaultTLSDir is %LOCALAPPDATA%\IPCCTV\tls: a per-user local folder.
func defaultTLSDir() string {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "IPCCTV", "tls")
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "IPCCTV", "tls")
	}
	return filepath.Join(".", "tls")
}

// defaultAuthDir is %LOCALAPPDATA%\IPCCTV\camui\auth: a per-user local folder.
func defaultAuthDir() string {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "IPCCTV", "camui", "auth")
	}
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "IPCCTV", "camui", "auth")
	}
	return filepath.Join(".", "auth")
}

// ValidateTLS checks the LAN listener's settings. With TLSListen empty there is
// nothing to check.
func (c Config) ValidateTLS() error {
	if c.TLSListen == "" {
		return nil
	}
	ip, _, err := parseTLSListen(c.TLSListen)
	if err != nil {
		return err
	}
	_, err = buildCertNames(ip, c.TLSHosts)
	return err
}

// ValidateTLSUsers refuses to start the LAN listener until someone can sign in
// to it: at least one user must exist. An unreadable user file refuses too.
func (c Config) ValidateTLSUsers(store UserStore) error {
	if c.TLSListen == "" {
		return nil
	}
	users, err := store.List()
	if err != nil {
		return fmt.Errorf("refusing to start: the TLS listener %q is on the LAN and the user file cannot be read: %v", c.TLSListen, err)
	}
	if len(users) > 0 {
		return nil
	}
	return fmt.Errorf("refusing to start: the TLS listener %q is on the LAN and no user exists to sign in with (looked in %s).\n"+
		"  Create one: `holocron user add <name>` or `camui.exe -set-password <username>` (the password is read from standard input),\n"+
		"  or start camui without tlsListen and open http://127.0.0.1:8787/setup on this PC; or remove tlsListen", c.TLSListen, c.AuthDir)
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
