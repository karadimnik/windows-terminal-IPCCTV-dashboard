package alerts

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://example.com/hook", true},
		{"https://203.0.113.5/hook", true},
		{"http://127.0.0.1:18680/hook", true},
		{"http://localhost:8080/", true},
		{"http://[::1]:8080/", true},
		{"http://10.1.2.3/x", true},
		{"http://192.168.0.7/x", true},
		{"http://172.16.5.5/x", true},
		{"http://169.254.1.1/x", true},
		{"http://example.com/hook", false},
		{"http://8.8.8.8/hook", false},
		{"http://172.32.0.1/x", false}, // just outside 172.16/12
		{"http://127.0.0.1.example.com/x", false},
		{"ftp://127.0.0.1/x", false},
		{"//127.0.0.1/x", false},
		{"", false},
		{"https://", false},
		{"not a url", false},
	} {
		_, err := checkURL(tc.url, "webhook.url")
		if (err == nil) != tc.ok {
			t.Errorf("checkURL(%q) err=%v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

func newWebhook(t *testing.T, url string, mod func(*WebhookConfig), lookup SecretLookup) *Webhook {
	t.Helper()
	cfg := WebhookConfig{Enabled: true, URL: url}
	if mod != nil {
		mod(&cfg)
	}
	w, err := NewWebhook(cfg, lookup, "nvr-host")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWebhookPostsJSON(t *testing.T) {
	rec := &recorder{}
	ts := fakeServer(t, rec.handler(200))
	secrets := func(n string) (string, bool) { return "shared-secret-value", n == "hook-secret" }
	w := newWebhook(t, ts.URL+"/in?x=1", func(c *WebhookConfig) { c.SecretName = "hook-secret" }, secrets)
	if !w.Enabled() || w.Name() != "webhook" || !w.Network() {
		t.Fatal("identity")
	}
	if err := w.Send(context.Background(), testEvent()); err != nil {
		t.Fatal(err)
	}
	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if r.method != "POST" || r.path != "/in" || r.header.Get("Content-Type") != "application/json" {
		t.Errorf("%+v", r)
	}
	if r.header.Get("X-Holocron-Secret") != "shared-secret-value" {
		t.Errorf("secret header: %v", r.header)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": float64(110), "severity": "Warning", "title": testEvent().Title, "message": testEvent().Message,
		"camera": "cam1", "at": "2026-10-20T12:00:00Z", "host": "nvr-host"}
	if len(m) != len(want) {
		t.Errorf("keys: %v", m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
}

func TestWebhookCustomSecretHeader(t *testing.T) {
	rec := &recorder{}
	ts := fakeServer(t, rec.handler(204))
	w := newWebhook(t, ts.URL, func(c *WebhookConfig) { c.SecretName = "s"; c.SecretHeader = "X-Token-Thing" },
		func(string) (string, bool) { return "v", true })
	if err := w.Send(context.Background(), testEvent()); err != nil {
		t.Fatal(err)
	}
	if rec.all()[0].header.Get("X-Token-Thing") != "v" {
		t.Errorf("%v", rec.all()[0].header)
	}
}

func TestWebhookFailures(t *testing.T) {
	const secret = "qs3cretvalue99"
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		timeout time.Duration
		url     func(base string) string
		want    string
	}{
		{"500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, time.Second, nil, "HTTP 500"},
		{"404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }, time.Second, nil, "HTTP 404"},
		{"redirect not followed", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1:1/x", 302) }, time.Second, nil, "HTTP 302"},
		{"timeout", func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) }, 50 * time.Millisecond, nil, "deadline exceeded"},
		{"refused", nil, time.Second, func(string) string { return "http://" + refusedAddr(t) + "/h/" + secret + "?token=" + secret }, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := ""
			if tc.handler != nil {
				base = fakeServer(t, tc.handler).URL
			}
			url := base + "/hook?key=" + secret
			if tc.url != nil {
				url = tc.url(base)
			}
			w := newWebhook(t, url, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			err := w.Send(ctx, testEvent())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("secret in error: %v", err)
			}
		})
	}
}

func TestWebhookMissingSecretIsAnErrorNotAnUnauthenticatedPost(t *testing.T) {
	rec := &recorder{}
	ts := fakeServer(t, rec.handler(200))
	w := newWebhook(t, ts.URL, func(c *WebhookConfig) { c.SecretName = "absent" }, func(string) (string, bool) { return "", false })
	err := w.Send(context.Background(), testEvent())
	if err == nil || !strings.Contains(err.Error(), `"absent"`) {
		t.Errorf("err = %v", err)
	}
	if len(rec.all()) != 0 {
		t.Errorf("request was sent without its secret")
	}
	w = newWebhook(t, ts.URL, func(c *WebhookConfig) { c.SecretName = "absent" }, nil) // nil lookup
	if w.Send(context.Background(), testEvent()) == nil {
		t.Errorf("nil lookup must fail")
	}
}

func TestWebhookConfigValidation(t *testing.T) {
	if w, err := NewWebhook(WebhookConfig{}, nil, "h"); err != nil || w.Enabled() {
		t.Errorf("zero config: %v enabled=%v", err, w.Enabled())
	}
	if w, err := NewWebhook(WebhookConfig{URL: "http://example.com"}, nil, "h"); err != nil || w.Enabled() {
		t.Errorf("disabled config with a bad URL must not error: %v", err)
	}
	if _, err := NewWebhook(WebhookConfig{Enabled: true, URL: "http://example.com/x"}, nil, "h"); err == nil {
		t.Errorf("public http must be refused")
	}
	if _, err := NewWebhook(WebhookConfig{Enabled: true}, nil, "h"); err == nil {
		t.Errorf("enabled without URL must be refused")
	}
}
