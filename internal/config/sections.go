package config

import (
	"encoding/json"
	"math"
	"sort"
	"strings"

	"holocron/internal/alerts"
)

// Secrets is the "secrets" object: which store holds the camera credentials.
type Secrets struct {
	Backend string `json:"backend"` // auto | keyring | file | env
	File    string `json:"file"`    // the encrypted store of the file backend; "" = <auth dir>/secrets.enc
	KeyFile string `json:"keyFile"` // its owner-only key file; "" = <auth dir>/secrets.key
}

// SecretBackends are the accepted values of secrets.backend.
var SecretBackends = []string{"auto", "keyring", "file", "env"}

// defaultAlerts are the defaults of internal/alerts/README.md, spelled out so that a reader of
// the effective configuration sees every value.
func defaultAlerts() alerts.Config {
	on := func() *bool { b := true; return &b }
	return alerts.Config{
		QueueSize:           alerts.DefaultQueueSize,
		TimeoutSeconds:      alerts.DefaultTimeout.Seconds(),
		MinRepeatSeconds:    alerts.DefaultMinRepeat.Seconds(),
		RetryBackoffSeconds: []float64{1, 5, 30},
		Webhook:             alerts.WebhookConfig{SecretHeader: alerts.DefaultSecretHeader},
		Push:                alerts.PushConfig{Server: alerts.DefaultPushServer},
		Desktop:             alerts.DesktopConfig{Enabled: on()},
		EventLog:            alerts.EventLogConfig{Enabled: on()},
	}
}

// object decodes a JSON object into its keys (sorted) with lower-cased lookups; ok is false (and a
// problem recorded) when raw is not an object.
func (d *decoder) object(path string, raw json.RawMessage) (keys []string, fields map[string]json.RawMessage, ok bool) {
	if jsonKind(raw) != "an object" {
		d.problem(path, "must be an object, got %s", jsonKind(raw))
		return nil, nil, false
	}
	_ = json.Unmarshal(raw, &fields)
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, fields, true
}

func (d *decoder) subBool(path string, raw json.RawMessage, dst *bool) bool {
	if jsonKind(raw) != "a boolean" {
		d.problem(path, "must be true or false (a JSON boolean), got %s", jsonKind(raw))
		return false
	}
	_ = json.Unmarshal(raw, dst)
	return true
}

// subNumber reads a number greater than min (strict) or at least min.
func (d *decoder) subNumber(path string, raw json.RawMessage, dst *float64, min float64, strict bool) bool {
	v, ok := d.number(path, raw)
	if !ok {
		return false
	}
	if v < min || (strict && v == min) {
		if strict {
			d.problem(path, "must be greater than %s, got %s", num(min), num(v))
		} else {
			d.problem(path, "must be at least %s, got %s", num(min), num(v))
		}
		return false
	}
	*dst = v
	return true
}

// alerts reads the "alerts" object (keys and rules of internal/alerts/README.md). The URL and
// topic rules are the channel constructors' own, so the file and the dispatcher cannot disagree.
func (d *decoder) alerts(c *Config) {
	raw, ok := d.raw["alerts"]
	if !ok {
		return
	}
	keys, fields, ok := d.object("alerts", raw)
	if !ok {
		return
	}
	a := &c.Alerts
	for _, k := range keys {
		v, p := fields[k], "alerts."+k
		switch strings.ToLower(k) {
		case "queuesize":
			var n float64
			if d.subNumber(p, v, &n, 1, false) {
				if n != math.Trunc(n) || n > 1<<31 {
					d.problem(p, "must be a whole number, got %s", num(n))
				} else {
					a.QueueSize = int(n)
				}
			}
		case "timeoutseconds":
			d.subNumber(p, v, &a.TimeoutSeconds, 0, true)
		case "minrepeatseconds":
			d.subNumber(p, v, &a.MinRepeatSeconds, 0, true)
		case "retrybackoffseconds":
			var items []json.RawMessage
			if jsonKind(v) != "an array" || json.Unmarshal(v, &items) != nil {
				d.problem(p, "must be an array of numbers of seconds, got %s", jsonKind(v))
				continue
			}
			out := []float64{}
			for i, it := range items {
				var s float64
				if d.subNumber(p+"["+itoa(i)+"]", it, &s, 0, true) {
					out = append(out, s)
				}
			}
			a.RetryBackoffSeconds = out
		case "webhook":
			d.webhook(p, v, &a.Webhook)
		case "push":
			d.push(p, v, &a.Push)
		case "desktop":
			d.enabledOnly(p, v, &a.Desktop.Enabled)
		case "eventlog":
			d.enabledOnly(p, v, &a.EventLog.Enabled)
		default:
			d.warn(p, "unknown key, ignored")
		}
	}
	if d.hasProblemUnder("alerts") {
		return
	}
	wh := a.Webhook
	wh.Enabled = wh.Enabled || wh.URL != "" // a URL is checked even while the channel is off
	if _, err := alerts.NewWebhook(wh, nil, "check"); err != nil {
		d.problem("alerts.webhook.url", "%s", strings.TrimPrefix(err.Error(), "alerts.webhook.url: "))
	}
	pc := a.Push
	pc.Enabled = pc.Enabled || pc.Topic != ""
	if _, err := alerts.NewPush(pc, nil); err != nil {
		path := "alerts.push.server"
		if strings.HasPrefix(err.Error(), "alerts.push.topic") {
			path = "alerts.push.topic"
		}
		d.problem(path, "%s", strings.TrimPrefix(strings.TrimPrefix(err.Error(), path), ": "))
	}
}

func (d *decoder) webhook(path string, raw json.RawMessage, w *alerts.WebhookConfig) {
	keys, fields, ok := d.object(path, raw)
	if !ok {
		return
	}
	for _, k := range keys {
		v, p := fields[k], path+"."+k
		switch strings.ToLower(k) {
		case "enabled":
			d.subBool(p, v, &w.Enabled)
		case "url":
			d.subStr(p, v, &w.URL)
		case "secretname":
			d.subStr(p, v, &w.SecretName)
		case "secretheader":
			d.subStr(p, v, &w.SecretHeader)
		default:
			d.warn(p, "unknown key, ignored")
		}
	}
}

func (d *decoder) push(path string, raw json.RawMessage, pc *alerts.PushConfig) {
	keys, fields, ok := d.object(path, raw)
	if !ok {
		return
	}
	for _, k := range keys {
		v, p := fields[k], path+"."+k
		switch strings.ToLower(k) {
		case "enabled":
			d.subBool(p, v, &pc.Enabled)
		case "server":
			d.subStr(p, v, &pc.Server)
		case "topic":
			d.subStr(p, v, &pc.Topic)
		case "tokenname":
			d.subStr(p, v, &pc.TokenName)
		default:
			d.warn(p, "unknown key, ignored")
		}
	}
}

func (d *decoder) enabledOnly(path string, raw json.RawMessage, dst **bool) {
	keys, fields, ok := d.object(path, raw)
	if !ok {
		return
	}
	for _, k := range keys {
		v, p := fields[k], path+"."+k
		if strings.ToLower(k) != "enabled" {
			d.warn(p, "unknown key, ignored")
			continue
		}
		var b bool
		if d.subBool(p, v, &b) {
			*dst = &b
		}
	}
}

func (d *decoder) secrets(c *Config) {
	raw, ok := d.raw["secrets"]
	if !ok {
		return
	}
	keys, fields, ok := d.object("secrets", raw)
	if !ok {
		return
	}
	for _, k := range keys {
		v, p := fields[k], "secrets."+k
		switch strings.ToLower(k) {
		case "backend":
			var s string
			d.subStr(p, v, &s)
			if !d.lastProblemOn(p) {
				found := false
				for _, b := range SecretBackends {
					found = found || s == b
				}
				if !found {
					d.problem(p, "must be one of %s, got %q", strings.Join(SecretBackends, ", "), s)
					continue
				}
				c.Secrets.Backend = s
			}
		case "file", "keyfile":
			var s string
			d.subStr(p, v, &s)
			if d.lastProblemOn(p) {
				continue
			}
			s = strings.TrimSpace(s)
			if strings.HasPrefix(s, `\\`) || strings.ContainsAny(s, "\x00\n\r") {
				d.problem(p, "must be a local file path (no network share, no control characters)")
				continue
			}
			if strings.ToLower(k) == "file" {
				c.Secrets.File = s
			} else {
				c.Secrets.KeyFile = s
			}
		default:
			d.warn(p, "unknown key, ignored")
		}
	}
}

func itoa(i int) string { return num(float64(i)) }
