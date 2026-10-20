package alerts

import (
	"context"
	"log/slog"
	"os"
)

// Channel is one way of telling the Operator. Send must honour ctx (the dispatcher's per-channel
// timeout) and may be called from one goroutine at a time.
type Channel interface {
	Name() string
	Send(ctx context.Context, ev Event) error
	// Enabled reports whether Send can be expected to work. A disabled channel is skipped.
	Enabled() bool
}

// Networked is implemented by channels whose failures are worth retrying (webhook, push).
type Networked interface{ Network() bool }

// SecretLookup returns a secret by name (the secrets store). The value is never logged.
type SecretLookup func(name string) (string, bool)

// Deps are what BuildChannels needs besides the configuration.
type Deps struct {
	Log     *slog.Logger // the "alerts" component logger; nil discards
	Secrets SecretLookup // nil: no secret is found
	Host    string       // sent in the webhook; default os.Hostname()
}

func (d Deps) logger() *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Log
}

func (d Deps) host() string {
	if d.Host != "" {
		return d.Host
	}
	h, _ := os.Hostname()
	return h
}

// BuildChannels makes the configured delivery channels except "log", which the Dispatcher always
// adds itself. A channel whose configuration is invalid is left out and its error returned, so one
// bad key cannot silence the others. Disabled or unsupported channels are included but report
// Enabled() == false.
func BuildChannels(cfg Config, d Deps) ([]Channel, []error) {
	var chans []Channel
	var errs []error
	if w, err := NewWebhook(cfg.Webhook, d.Secrets, d.host()); err != nil {
		errs = append(errs, err)
	} else {
		chans = append(chans, w)
	}
	if p, err := NewPush(cfg.Push, d.Secrets); err != nil {
		errs = append(errs, err)
	} else {
		chans = append(chans, p)
	}
	chans = append(chans, NewDesktop(cfg.Desktop, d.logger()), NewEventLog(cfg.EventLog, d.logger()))
	return chans, errs
}
