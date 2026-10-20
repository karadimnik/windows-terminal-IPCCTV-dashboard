package config

import (
	"strings"

	"holocron/internal/alerts"
	"holocron/internal/osx"
)

// Config is the typed holocron.config.json. Field names are the JSON key names (the PowerShell
// recorder.config.json keys), so the same file works in both systems during the shadow phases.
type Config struct {
	// Paths and recording
	RootPath            string
	FfmpegPath          string // empty: resolve "ffmpeg" from PATH
	FfprobePath         string
	SegmentSeconds      int
	Container           string
	ContainerProfiles   map[string]ContainerProfile
	RtspTransport       string
	TimeoutMicroseconds int64

	// Retention
	ReserveGB            float64
	PruneHysteresisGB    float64
	PruneIntervalSeconds float64
	MaxRetentionDays     *float64 // nil: age rule off
	MaxAgePruneFraction  float64

	// Supervisor and timing
	ReconcileSeconds         float64
	StatusIntervalSeconds    float64
	ArchiveScanSeconds       float64
	CameraDownAlertMinutes   float64
	CameraDownRepeatHours    float64
	StopTimeoutSeconds       float64
	BackoffStartSeconds      int
	BackoffMaxSeconds        int
	HealthyResetSeconds      int
	StallFactor              float64
	LogMaxBytes              int
	LogRepeatSuppressSeconds float64
	StatusEventCount         int

	// Relay (MediaMTX)
	RestreamerEnabled       bool
	RestreamerExePath       string
	RestreamerConfigPath    string
	RelayBase               string
	RelayApiBase            string
	RelayProbeTimeoutMs     int
	RelayStartupWaitSeconds int
	RelayReadyWaitSeconds   int
	StartStaggerMs          int

	// Live view
	LiveViewEnabled       bool
	LiveExePath           string
	LiveConfigPath        string
	LiveWhepAddress       string
	LiveUdpAddress        string
	LiveAdvertiseHosts    []string
	LiveSubStream         string
	LiveCloseAfterSeconds int
	LiveSecretPath        string

	// Logging (new in holocron; LLD 2.13)
	Logging Logging
	// Alerts is the "alerts" object (internal/alerts/README.md).
	Alerts alerts.Config
	// Secrets is the "secrets" object.
	Secrets Secrets
}

// Logging is the "logging" object: the default level and per-component levels.
type Logging struct {
	Level  string            `json:"level"`
	Levels map[string]string `json:"levels"`
}

// ContainerProfile is one entry of ContainerProfiles.
type ContainerProfile struct {
	Extension            string
	SegmentFormat        string
	AudioArgs            []string
	ExtraArgs            []string
	SegmentFormatOptions string
}

// Profile returns the active container profile (the Container key, looked up case-insensitively).
func (c Config) Profile() (ContainerProfile, bool) {
	for name, p := range c.ContainerProfiles {
		if strings.EqualFold(name, c.Container) {
			return p, true
		}
	}
	return ContainerProfile{}, false
}

// Defaults are the in-code defaults of Get-CamConfigDefault. On Windows they are the PowerShell
// values verbatim; elsewhere the tool paths are resolved from PATH and the root and MediaMTX paths
// follow the platform's default directories (osx.Platform.Dirs).
func Defaults(p osx.Platform) Config {
	d := p.Dirs()
	c := Config{
		RootPath:       d.Root,
		FfmpegPath:     "ffmpeg",
		FfprobePath:    "ffprobe",
		SegmentSeconds: 900,
		Container:      "mkv",
		ContainerProfiles: map[string]ContainerProfile{
			"mkv": {Extension: ".mkv", SegmentFormat: "matroska", AudioArgs: []string{}, ExtraArgs: []string{}},
			"fmp4": {
				Extension: ".mp4", SegmentFormat: "mp4", AudioArgs: []string{"-c:a", "aac", "-b:a", "64k"}, ExtraArgs: []string{},
				SegmentFormatOptions: "movflags=+frag_keyframe+empty_moov+default_base_moof",
			},
		},
		RtspTransport:       "tcp",
		TimeoutMicroseconds: 10000000,

		ReserveGB:            20,
		PruneHysteresisGB:    5,
		PruneIntervalSeconds: 300,
		MaxAgePruneFraction:  0.5,

		ReconcileSeconds:         5,
		StatusIntervalSeconds:    5,
		ArchiveScanSeconds:       60,
		CameraDownAlertMinutes:   15,
		CameraDownRepeatHours:    12,
		StopTimeoutSeconds:       30,
		BackoffStartSeconds:      3,
		BackoffMaxSeconds:        60,
		HealthyResetSeconds:      300,
		StallFactor:              2.5,
		LogMaxBytes:              2097152,
		LogRepeatSuppressSeconds: 60,
		StatusEventCount:         20,

		RestreamerEnabled:       true,
		RelayBase:               "rtsp://127.0.0.1:8554",
		RelayApiBase:            "http://127.0.0.1:9997",
		RelayProbeTimeoutMs:     1500,
		RelayStartupWaitSeconds: 20,
		RelayReadyWaitSeconds:   15,
		StartStaggerMs:          300,

		LiveViewEnabled:       false,
		LiveWhepAddress:       "127.0.0.1:8889",
		LiveUdpAddress:        "127.0.0.1:8189",
		LiveAdvertiseHosts:    []string{},
		LiveSubStream:         "stream2",
		LiveCloseAfterSeconds: 10,

		Logging: Logging{Level: "info", Levels: map[string]string{}},
		Alerts:  defaultAlerts(),
		Secrets: Secrets{Backend: "auto"},
	}
	if p.Name() == "windows" {
		c.FfmpegPath = `C:\ffmpeg\bin\ffmpeg.exe`
		c.FfprobePath = `C:\ffmpeg\bin\ffprobe.exe`
		c.RestreamerExePath = `C:\mediamtx\mediamtx.exe`
		c.RestreamerConfigPath = `C:\mediamtx\mediamtx.yml`
		c.LiveConfigPath = `C:\mediamtx\mediamtx-live.yml`
	} else {
		c.RestreamerExePath = "mediamtx"
		c.RestreamerConfigPath = d.Config + "/mediamtx.yml"
		c.LiveConfigPath = d.Config + "/mediamtx-live.yml"
	}
	return c
}
