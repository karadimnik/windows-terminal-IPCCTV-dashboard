package alerts

import "time"

// Defaults of the dispatcher.
const (
	DefaultQueueSize      = 256
	DefaultTimeout        = 10 * time.Second
	DefaultMinRepeat      = 60 * time.Second
	DefaultPushServer     = "https://ntfy.sh"
	DefaultSecretHeader   = "X-Holocron-Secret"
	DesktopTimeout        = 10 * time.Second // hard limit of the desktop adapter, whatever the config says
	eventLogPrefix        = "IPCCTV: "
	maxHeaderOrTitleRunes = 200
)

// DefaultBackoff are the waits before retry 1, 2 and 3 of a network channel.
var DefaultBackoff = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

// Config is the "alerts" object of holocron.config.json. internal/config does not know it yet; the
// keys it needs are listed in README.md. A zero value is a valid configuration: log, desktop and
// eventlog on, webhook and push off.
type Config struct {
	// QueueSize bounds the queue of each channel (default 256). When full the OLDEST event is dropped.
	QueueSize int `json:"queueSize,omitempty"`
	// TimeoutSeconds is the limit of one delivery attempt on any channel (default 10).
	TimeoutSeconds float64 `json:"timeoutSeconds,omitempty"`
	// MinRepeatSeconds: an event whose Key was already sent less than this long ago is not sent
	// again (default 60). Events without a Key are never deduplicated.
	MinRepeatSeconds float64 `json:"minRepeatSeconds,omitempty"`
	// RetryBackoffSeconds are the waits before each retry of a failed delivery on a network channel
	// (webhook, push). Absent: 1, 5, 30. Empty list: no retries.
	RetryBackoffSeconds []float64 `json:"retryBackoffSeconds,omitempty"`

	Webhook  WebhookConfig  `json:"webhook"`
	Push     PushConfig     `json:"push"`
	Desktop  DesktopConfig  `json:"desktop"`
	EventLog EventLogConfig `json:"eventlog"`
}

// WebhookConfig: a JSON POST {id, severity, title, message, camera, at, host}.
type WebhookConfig struct {
	Enabled bool `json:"enabled"`
	// URL: https, or http only for a loopback or private address.
	URL string `json:"url"`
	// SecretName names a secret in the secrets store; its value is sent in SecretHeader.
	SecretName string `json:"secretName,omitempty"`
	// SecretHeader defaults to X-Holocron-Secret.
	SecretHeader string `json:"secretHeader,omitempty"`
}

// PushConfig: ntfy-style push to a phone. POST <server>/<topic> with Title, Priority and Tags
// headers and the message as the body.
type PushConfig struct {
	Enabled bool `json:"enabled"`
	// Server defaults to https://ntfy.sh; a self-hosted ntfy is the private option.
	Server string `json:"server,omitempty"`
	// Topic is 1-64 of A-Z a-z 0-9 _ -. On ntfy.sh anyone who knows the name can read it: make it long and random.
	Topic string `json:"topic"`
	// TokenName names a secret holding an access token, sent as a bearer token.
	TokenName string `json:"tokenName,omitempty"`
}

// DesktopConfig: a toast on the machine the recorder runs on. Enabled defaults to true.
type DesktopConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// EventLogConfig: the Windows Application log, syslog/journald elsewhere. Enabled defaults to true.
type EventLogConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

func enabledOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// resolved is Config with its defaults applied.
type resolved struct {
	queue     int
	timeout   time.Duration
	minRepeat time.Duration
	backoff   []time.Duration
}

func (c Config) resolve() resolved {
	r := resolved{queue: c.QueueSize, timeout: seconds(c.TimeoutSeconds), minRepeat: seconds(c.MinRepeatSeconds)}
	if r.queue <= 0 {
		r.queue = DefaultQueueSize
	}
	if r.timeout <= 0 {
		r.timeout = DefaultTimeout
	}
	if r.minRepeat <= 0 {
		r.minRepeat = DefaultMinRepeat
	}
	if c.RetryBackoffSeconds == nil {
		r.backoff = DefaultBackoff
	} else {
		for _, s := range c.RetryBackoffSeconds {
			if s > 0 {
				r.backoff = append(r.backoff, seconds(s))
			}
		}
	}
	return r
}
