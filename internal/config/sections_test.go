package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"holocron/internal/alerts"
)

func TestLoggingAlertsSecretsDefaults(t *testing.T) {
	c := Defaults(linP)
	if c.Logging.Level != "info" || c.Logging.Levels == nil || len(c.Logging.Levels) != 0 {
		t.Errorf("logging %+v", c.Logging)
	}
	if c.Secrets.Backend != "auto" {
		t.Errorf("secrets %+v", c.Secrets)
	}
	a := c.Alerts
	if a.QueueSize != 256 || a.TimeoutSeconds != 10 || a.MinRepeatSeconds != 60 || !reflect.DeepEqual(a.RetryBackoffSeconds, []float64{1, 5, 30}) {
		t.Errorf("alerts numbers %+v", a)
	}
	if a.Webhook.Enabled || a.Webhook.URL != "" || a.Webhook.SecretName != "" || a.Webhook.SecretHeader != "X-Holocron-Secret" {
		t.Errorf("webhook %+v", a.Webhook)
	}
	if a.Push.Enabled || a.Push.Server != "https://ntfy.sh" || a.Push.Topic != "" || a.Push.TokenName != "" {
		t.Errorf("push %+v", a.Push)
	}
	if a.Desktop.Enabled == nil || !*a.Desktop.Enabled || a.EventLog.Enabled == nil || !*a.EventLog.Enabled {
		t.Errorf("desktop/eventlog %+v %+v", a.Desktop, a.EventLog)
	}
	// a file without the sections gives exactly the defaults
	p, _, err := Parse([]byte(`{"SegmentSeconds":60}`), linP)
	if err != nil || !reflect.DeepEqual(p.Alerts, a) || p.Secrets != c.Secrets || p.Logging.Level != "info" {
		t.Errorf("parse without sections: %v %+v", err, p.Alerts)
	}
}

func TestAlertsAndSecretsParse(t *testing.T) {
	doc := `{"alerts":{"queueSize":8,"timeoutSeconds":2.5,"minRepeatSeconds":30,"retryBackoffSeconds":[],
		"webhook":{"enabled":true,"url":"https://hooks.example.org/x","secretName":"hook","secretHeader":"X-Key"},
		"push":{"enabled":true,"server":"http://192.168.1.9:8080","topic":"holocron_ab-12","tokenName":"ntfy"},
		"desktop":{"enabled":false},"eventlog":{"enabled":false}},
		"secrets":{"backend":"file"},"logging":{"levels":{"supervisor":"debug"}}}`
	c, warns, err := Parse([]byte(doc), linP)
	if err != nil || len(warns) != 0 {
		t.Fatalf("%v %v", err, warns)
	}
	a := c.Alerts
	if a.QueueSize != 8 || a.TimeoutSeconds != 2.5 || a.MinRepeatSeconds != 30 || a.RetryBackoffSeconds == nil || len(a.RetryBackoffSeconds) != 0 {
		t.Errorf("numbers %+v", a)
	}
	if a.Webhook != (alerts.WebhookConfig{Enabled: true, URL: "https://hooks.example.org/x", SecretName: "hook", SecretHeader: "X-Key"}) {
		t.Errorf("webhook %+v", a.Webhook)
	}
	if a.Push != (alerts.PushConfig{Enabled: true, Server: "http://192.168.1.9:8080", Topic: "holocron_ab-12", TokenName: "ntfy"}) {
		t.Errorf("push %+v", a.Push)
	}
	if *a.Desktop.Enabled || *a.EventLog.Enabled {
		t.Error("desktop/eventlog not switched off")
	}
	if c.Secrets.Backend != "file" || c.Logging.Level != "info" || c.Logging.Levels["supervisor"] != "debug" {
		t.Errorf("secrets %+v logging %+v", c.Secrets, c.Logging)
	}
	// a partial object keeps the other defaults
	c, _, err = Parse([]byte(`{"alerts":{"push":{"topic":"t1"}}}`), linP)
	if err != nil || c.Alerts.Push.Server != "https://ntfy.sh" || c.Alerts.QueueSize != 256 || !*c.Alerts.Desktop.Enabled {
		t.Errorf("partial: %v %+v", err, c.Alerts)
	}
	_, warns, _ = Parse([]byte(`{"alerts":{"colour":1,"webhook":{"x":1}},"secrets":{"y":2}}`), linP)
	if len(warns) != 3 {
		t.Errorf("warnings %v", warns)
	}
}

func TestAlertsAndSecretsValidation(t *testing.T) {
	for _, c := range []struct{ doc, path, text string }{
		{`{"alerts":[]}`, "alerts", "object"},
		{`{"alerts":{"queueSize":0}}`, "alerts.queueSize", "at least 1"},
		{`{"alerts":{"queueSize":1.5}}`, "alerts.queueSize", "whole number"},
		{`{"alerts":{"queueSize":"8"}}`, "alerts.queueSize", "number"},
		{`{"alerts":{"timeoutSeconds":0}}`, "alerts.timeoutSeconds", "greater than 0"},
		{`{"alerts":{"minRepeatSeconds":-1}}`, "alerts.minRepeatSeconds", "greater than 0"},
		{`{"alerts":{"retryBackoffSeconds":5}}`, "alerts.retryBackoffSeconds", "array"},
		{`{"alerts":{"retryBackoffSeconds":[1,0]}}`, "alerts.retryBackoffSeconds[1]", "greater than 0"},
		{`{"alerts":{"webhook":{"enabled":"yes"}}}`, "alerts.webhook.enabled", "boolean"},
		{`{"alerts":{"webhook":{"enabled":true}}}`, "alerts.webhook.url", "usable URL"},
		{`{"alerts":{"webhook":{"enabled":true,"url":"http://hooks.example.org/x"}}}`, "alerts.webhook.url", "private"},
		{`{"alerts":{"webhook":{"url":"ftp://10.0.0.1/x"}}}`, "alerts.webhook.url", "scheme"},
		{`{"alerts":{"push":{"enabled":true}}}`, "alerts.push.topic", "1-64"},
		{`{"alerts":{"push":{"enabled":true,"topic":"bad topic"}}}`, "alerts.push.topic", "1-64"},
		{`{"alerts":{"push":{"topic":"ok","server":"http://ntfy.example.org"}}}`, "alerts.push.server", "private"},
		{`{"alerts":{"desktop":{"enabled":1}}}`, "alerts.desktop.enabled", "boolean"},
		{`{"alerts":{"eventlog":true}}`, "alerts.eventlog", "object"},
		{`{"secrets":{"backend":"vault"}}`, "secrets.backend", "auto, keyring, file, env"},
		{`{"secrets":{"backend":1}}`, "secrets.backend", "string"},
		{`{"secrets":"env"}`, "secrets", "object"},
		{`{"logging":{"level":"loud"}}`, "logging.level", ""},
	} {
		_, _, err := Parse([]byte(c.doc), linP)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: no validation error (%v)", c.doc, err)
			continue
		}
		found := false
		for _, p := range ve.Problems {
			if p.Path == c.path && strings.Contains(p.Message, c.text) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want a problem on %s mentioning %q, got %v", c.doc, c.path, c.text, ve.Problems)
		}
	}
}

func TestWebSection(t *testing.T) {
	if w := Defaults(linP).Web; w != (Web{Enabled: true, Listen: "127.0.0.1:8787"}) {
		t.Errorf("defaults %+v", w)
	}
	c, warns, err := Parse([]byte(`{"web":{"enabled":false,"listen":"127.0.0.1:18820","tlsListen":"192.168.1.50:8443","tlsDir":"/t","authDir":"/a","x":1}}`), linP)
	if err != nil || len(warns) != 1 || c.Web != (Web{Enabled: false, Listen: "127.0.0.1:18820", TLSListen: "192.168.1.50:8443", TLSDir: "/t", AuthDir: "/a"}) {
		t.Errorf("%+v %v %v", c.Web, warns, err)
	}
	for _, bad := range []struct{ doc, path, text string }{
		{`{"web":[]}`, "web", "object"},
		{`{"web":{"enabled":"yes"}}`, "web.enabled", "boolean"},
		{`{"web":{"listen":"8787"}}`, "web.listen", "host:port"},
		{`{"web":{"listen":"127.0.0.1:0"}}`, "web.listen", "1 to 65535"},
		{`{"web":{"listen":"127.0.0.1:70000"}}`, "web.listen", "1 to 65535"},
		{`{"web":{"listen":7}}`, "web.listen", "string"},
		{`{"web":{"tlsListen":"192.168.1.50"}}`, "web.tlsListen", "IPv4:port"},
	} {
		_, _, err := Parse([]byte(bad.doc), linP)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Problems[0].Path != bad.path || !strings.Contains(ve.Problems[0].Message, bad.text) {
			t.Errorf("%s: %v", bad.doc, err)
		}
	}
}
