package relay

import (
	"fmt"
	"math"
	"strings"
	"time"

	"holocron/internal/config"
	"holocron/internal/osx"
)

// RestartPolicy are the values of Set-CamChildBackoff for a MediaMTX child: the wait before a restart
// starts at BackoffStart, doubles up to BackoffMax, and goes back to BackoffStart once the child ran
// for HealthyReset. The relay and the live view use the same ones as the camera children. KillWait is
// how long a hard kill is waited for.
type RestartPolicy struct {
	BackoffStart time.Duration
	BackoffMax   time.Duration
	HealthyReset time.Duration
	KillWait     time.Duration
}

// ExitMessage is the LastError text after the relay exits. The seconds round half to even, like
// PowerShell's [int] and ffmpeg.ExitInfo.Message.
func ExitMessage(code int, ranFor time.Duration) string {
	return fmt.Sprintf("mediamtx exited with code %d after %ds", code, int64(math.RoundToEven(ranFor.Seconds())))
}

// ChildSpec is how to start one MediaMTX child; starting it belongs to the supervisor.
type ChildSpec struct {
	Name string // "restreamer" or "liveview"
	// Exe is the MediaMTX executable (a bare name is resolved from PATH by the starter).
	Exe  string
	Args []string
	// Dir is the working directory: a LOCAL, absolute directory, the config file's. MediaMTX writes
	// auto-generated certificate files into its working directory and exits when it cannot, which a
	// UNC or WSL share (Stage 1A) does not allow.
	Dir string
	// Env is the complete environment: the parent's without MTX_* plus the source entries.
	Env []string
	// LogPath receives the child's stdout, line by line through the redactor (it prints the camera
	// URL on a connection error). Nothing is passed to MediaMTX to write a log itself.
	LogPath string
	Restart RestartPolicy
	// ConfigPath is the file MediaMTX watches; Args holds it as the only argument.
	ConfigPath string
}

// RelayChildSpec is the start specification of the recording relay: RestreamerExePath with
// RestreamerConfigPath, its source environment on top of parentEnv. Working directory: the config's
// folder (PowerShell used the executable's folder, which is the same in the default install; a
// system-installed binary's folder is not writable).
func RelayChildSpec(p osx.Platform, cfg config.Config, logPath string, parentEnv, sourceEnv []string) (ChildSpec, error) {
	return childSpec(p, cfg, "restreamer", cfg.RestreamerExePath, cfg.RestreamerConfigPath, logPath, parentEnv, sourceEnv)
}

// LiveChildSpec is the live instance's: LiveExePath (the relay's executable when empty) with LiveConfigPath.
func LiveChildSpec(p osx.Platform, cfg config.Config, logPath string, parentEnv, sourceEnv []string) (ChildSpec, error) {
	exe := cfg.LiveExePath
	if strings.TrimSpace(exe) == "" {
		exe = cfg.RestreamerExePath
	}
	return childSpec(p, cfg, "liveview", exe, cfg.LiveConfigPath, logPath, parentEnv, sourceEnv)
}

func childSpec(p osx.Platform, cfg config.Config, name, exe, configPath, logPath string, parentEnv, sourceEnv []string) (ChildSpec, error) {
	if strings.TrimSpace(exe) == "" {
		return ChildSpec{}, fmt.Errorf("relay: no MediaMTX executable is configured for the %s", name)
	}
	if strings.TrimSpace(configPath) == "" {
		return ChildSpec{}, fmt.Errorf("relay: no MediaMTX config path is configured for the %s", name)
	}
	dir, err := localDir(p.Name(), configPath)
	if err != nil {
		return ChildSpec{}, err
	}
	sec := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	return ChildSpec{
		Name:       name,
		Exe:        exe,
		Args:       []string{configPath},
		Dir:        dir,
		Env:        ChildEnv(parentEnv, sourceEnv),
		LogPath:    logPath,
		ConfigPath: configPath,
		Restart: RestartPolicy{
			BackoffStart: sec(float64(cfg.BackoffStartSeconds)),
			BackoffMax:   sec(float64(cfg.BackoffMaxSeconds)),
			HealthyReset: sec(float64(cfg.HealthyResetSeconds)),
			KillWait:     10 * time.Second,
		},
	}, nil
}

// localDir is the folder holding configPath, which must be an absolute local path. A UNC or WSL share
// path (\\server\share, \\wsl.localhost\...) is refused on Windows, as the installer and preflight do.
func localDir(goos, configPath string) (string, error) {
	if goos == "windows" {
		if strings.HasPrefix(configPath, `\\`) || strings.HasPrefix(configPath, "//") {
			return "", fmt.Errorf("relay: %s is a network or WSL share path; MediaMTX needs a local working directory (it writes certificate files there and cannot watch its config on a share)", configPath)
		}
		if len(configPath) < 3 || !isLetter(configPath[0]) || configPath[1] != ':' || (configPath[2] != '\\' && configPath[2] != '/') {
			return "", fmt.Errorf("relay: %s is not an absolute path", configPath)
		}
		i := strings.LastIndexAny(configPath, `\/`)
		if i == 2 {
			return configPath[:3], nil
		}
		return configPath[:i], nil
	}
	if !strings.HasPrefix(configPath, "/") {
		return "", fmt.Errorf("relay: %s is not an absolute path", configPath)
	}
	i := strings.LastIndexByte(configPath, '/')
	if i == 0 {
		return "/", nil
	}
	return configPath[:i], nil
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// ReadinessPolicy is the timing of the relay's readiness, all from configuration.
type ReadinessPolicy struct {
	// StartupWait: after the relay starts, wait this long for the control API to answer (RelayStartupWaitSeconds, 20).
	StartupWait time.Duration
	// ReadyWait: a camera's consumer is held back this long after the relay started while its own
	// path is not serving (RelayReadyWaitSeconds, 15), then goes direct.
	ReadyWait time.Duration
	// Poll is the pause between API polls while waiting for startup (500 ms).
	Poll time.Duration
	// ProbeTimeout is one API request's budget (RelayProbeTimeoutMs, 1500 ms, floor 100 ms).
	ProbeTimeout time.Duration
}

// NewReadinessPolicy reads the policy from a validated configuration.
func NewReadinessPolicy(cfg config.Config) ReadinessPolicy {
	probe := time.Duration(cfg.RelayProbeTimeoutMs) * time.Millisecond
	if probe < minProbeTimeout {
		probe = minProbeTimeout
	}
	return ReadinessPolicy{
		StartupWait:  time.Duration(cfg.RelayStartupWaitSeconds) * time.Second,
		ReadyWait:    time.Duration(cfg.RelayReadyWaitSeconds) * time.Second,
		Poll:         500 * time.Millisecond,
		ProbeTimeout: probe,
	}
}

// ShouldWaitAtStart: PowerShell waits only for a positive budget and at least one path.
func (p ReadinessPolicy) ShouldWaitAtStart(pathCount int) bool {
	return p.StartupWait > 0 && pathCount > 0
}

// HoldChild is the per-child hold: a consumer whose path is not serving waits until
// relayStartedAt+ReadyWait, only while the relay process is alive; afterwards it starts on the
// direct input. (Killing MediaMTX and letting every recorder reconnect directly left 5 of 6 paths
// unable to serve again - hence the hold.)
func (p ReadinessPolicy) HoldChild(now, relayStartedAt time.Time, relayAlive, pathServing bool) bool {
	return relayAlive && !pathServing && now.Before(relayStartedAt.Add(p.ReadyWait))
}
