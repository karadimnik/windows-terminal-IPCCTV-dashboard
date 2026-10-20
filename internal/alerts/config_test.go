package alerts

import (
	"encoding/json"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	r := Config{}.resolve()
	if r.queue != 256 || r.timeout != 10*time.Second || r.minRepeat != 60*time.Second {
		t.Errorf("%+v", r)
	}
	if len(r.backoff) != 3 || r.backoff[0] != time.Second || r.backoff[1] != 5*time.Second || r.backoff[2] != 30*time.Second {
		t.Errorf("backoff %v", r.backoff)
	}
	r = Config{RetryBackoffSeconds: []float64{}}.resolve()
	if len(r.backoff) != 0 {
		t.Errorf("an explicit empty list means no retries: %v", r.backoff)
	}
	r = Config{QueueSize: -1, TimeoutSeconds: -3, MinRepeatSeconds: 0.5, RetryBackoffSeconds: []float64{2, -1, 0.5}}.resolve()
	if r.queue != 256 || r.timeout != 10*time.Second || r.minRepeat != 500*time.Millisecond || len(r.backoff) != 2 {
		t.Errorf("%+v", r)
	}
}

func TestConfigJSONKeys(t *testing.T) {
	const doc = `{"queueSize":8,"timeoutSeconds":3,"minRepeatSeconds":30,"retryBackoffSeconds":[1,2],
	 "webhook":{"enabled":true,"url":"https://example.com/h","secretName":"alerts.webhook","secretHeader":"X-A"},
	 "push":{"enabled":true,"server":"https://ntfy.example.com","topic":"abc","tokenName":"alerts.push"},
	 "desktop":{"enabled":false},"eventlog":{"enabled":true}}`
	var c Config
	if err := json.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatal(err)
	}
	if c.QueueSize != 8 || c.Webhook.SecretHeader != "X-A" || c.Push.TokenName != "alerts.push" || enabledOr(c.Desktop.Enabled, true) || !enabledOr(c.EventLog.Enabled, false) {
		t.Errorf("%+v", c)
	}
}

func TestBuildChannels(t *testing.T) {
	chans, errs := BuildChannels(Config{}, Deps{})
	if len(errs) != 0 || len(chans) != 4 {
		t.Fatalf("%d channels, errs %v", len(chans), errs)
	}
	names := map[string]bool{}
	for _, c := range chans {
		names[c.Name()] = c.Enabled()
	}
	if names["webhook"] || names["push"] || len(names) != 4 {
		t.Errorf("webhook and push must be off by default: %v", names)
	}
	// one bad key leaves the others working
	chans, errs = BuildChannels(Config{Webhook: WebhookConfig{Enabled: true, URL: "http://example.com"},
		Push: PushConfig{Enabled: true, Topic: "t"}}, Deps{})
	if len(errs) != 1 || len(chans) != 3 {
		t.Fatalf("%d channels, errs %v", len(chans), errs)
	}
	for _, c := range chans {
		if c.Name() == "push" && !c.Enabled() {
			t.Errorf("push should be enabled")
		}
	}
}
