package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Webhook is the "webhook" channel: a JSON POST to a URL the Operator controls.
type Webhook struct {
	enabled bool
	u       *url.URL
	hdr     string
	secret  string
	lookup  SecretLookup
	host    string
	client  *http.Client
}

// NewWebhook validates cfg. A disabled or URL-less config yields a disabled channel, not an error.
func NewWebhook(cfg WebhookConfig, lookup SecretLookup, host string) (*Webhook, error) {
	w := &Webhook{hdr: cfg.SecretHeader, secret: cfg.SecretName, lookup: lookup, host: host, client: httpClient()}
	if w.hdr == "" {
		w.hdr = DefaultSecretHeader
	}
	if !cfg.Enabled {
		return w, nil
	}
	u, err := checkURL(cfg.URL, "webhook.url")
	if err != nil {
		return nil, err
	}
	w.u, w.enabled = u, true
	return w, nil
}

func (*Webhook) Name() string    { return "webhook" }
func (w *Webhook) Enabled() bool { return w.enabled }
func (*Webhook) Network() bool   { return true }

// webhookBody is the posted document.
type webhookBody struct {
	ID       int    `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Message  string `json:"message"`
	Camera   string `json:"camera"`
	At       string `json:"at"`
	Host     string `json:"host"`
}

func (w *Webhook) Send(ctx context.Context, ev Event) error {
	body, err := json.Marshal(webhookBody{ev.ID, ev.Severity.String(), ev.Title, ev.Message, ev.Camera,
		ev.At.UTC().Format(time.RFC3339), w.host})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, w.u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST %s: bad request", safeURL(w.u))
	}
	req.Header.Set("Content-Type", "application/json")
	if w.secret != "" {
		v, ok := "", false
		if w.lookup != nil {
			v, ok = w.lookup(w.secret)
		}
		if !ok {
			return fmt.Errorf("secret %q for the webhook was not found in the secrets store", w.secret)
		}
		req.Header.Set(w.hdr, v)
	}
	return post(ctx, w.client, req, w.u)
}
