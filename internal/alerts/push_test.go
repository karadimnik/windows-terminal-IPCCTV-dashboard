package alerts

import (
	"context"
	"mime"
	"strings"
	"testing"
)

func newPush(t *testing.T, mod func(*PushConfig), lookup SecretLookup) *Push {
	t.Helper()
	cfg := PushConfig{Enabled: true, Topic: "holocron-abc123"}
	if mod != nil {
		mod(&cfg)
	}
	p, err := NewPush(cfg, lookup)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPushRequestShape(t *testing.T) {
	rec := &recorder{}
	ts := fakeServer(t, rec.handler(200))
	p := newPush(t, func(c *PushConfig) { c.Server = ts.URL + "/"; c.TokenName = "tok" },
		func(n string) (string, bool) { return "tk_abcdef", n == "tok" })
	if !p.Enabled() || p.Name() != "push" || !p.Network() {
		t.Fatal("identity")
	}
	if err := p.Send(context.Background(), testEvent()); err != nil {
		t.Fatal(err)
	}
	r := rec.all()[0]
	if r.method != "POST" || r.path != "/holocron-abc123" {
		t.Errorf("%s %s", r.method, r.path)
	}
	if r.body != testEvent().Message {
		t.Errorf("body %q", r.body)
	}
	h := r.header
	if h.Get("Title") != testEvent().Title || h.Get("Priority") != "3" || h.Get("Tags") != "warning,holocron,alert-110" {
		t.Errorf("headers: %v", h)
	}
	if h.Get("Authorization") != "Bearer tk_abcdef" {
		t.Errorf("auth: %q", h.Get("Authorization"))
	}
}

func TestPushPriorityAndTagsFollowSeverity(t *testing.T) {
	for _, tc := range []struct {
		sev  Severity
		prio string
		tag  string
	}{{Error, "4", "rotating_light,holocron,alert-120"}, {Warning, "3", "warning,holocron,alert-120"}, {Info, "2", "information_source,holocron,alert-120"}} {
		rec := &recorder{}
		ts := fakeServer(t, rec.handler(200))
		p := newPush(t, func(c *PushConfig) { c.Server = ts.URL }, nil)
		e := testEvent()
		e.ID, e.Severity = 120, tc.sev
		if err := p.Send(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		h := rec.all()[0].header
		if h.Get("Priority") != tc.prio || h.Get("Tags") != tc.tag {
			t.Errorf("%v: %v", tc.sev, h)
		}
		if h.Get("Authorization") != "" {
			t.Errorf("no token configured, no Authorization header expected")
		}
	}
}

func TestPushTitleHeaderIsSafe(t *testing.T) {
	rec := &recorder{}
	ts := fakeServer(t, rec.handler(200))
	p := newPush(t, func(c *PushConfig) { c.Server = ts.URL }, nil)
	e := testEvent()
	e.Title = "the NVR stopped\r\nX-Injected: 1 – café"
	if err := p.Send(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	h := rec.all()[0].header
	if h.Get("X-Injected") != "" {
		t.Errorf("header injection")
	}
	dec, err := new(mime.WordDecoder).DecodeHeader(h.Get("Title"))
	if err != nil || dec != "the NVR stopped  X-Injected: 1 – café" {
		t.Errorf("Title %q -> %q (%v)", h.Get("Title"), dec, err)
	}
	e.Message = ""
	rec2 := &recorder{}
	ts2 := fakeServer(t, rec2.handler(200))
	p2 := newPush(t, func(c *PushConfig) { c.Server = ts2.URL }, nil)
	if err := p2.Send(context.Background(), e); err != nil || rec2.all()[0].body == "" {
		t.Errorf("empty message must fall back to the title: %v", err)
	}
}

func TestPushFailures(t *testing.T) {
	ts := fakeServer(t, (&recorder{}).handler(500))
	p := newPush(t, func(c *PushConfig) { c.Server = ts.URL }, nil)
	if err := p.Send(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("500: %v", err)
	}
	p = newPush(t, func(c *PushConfig) { c.Server = "http://" + refusedAddr(t) }, nil)
	if err := p.Send(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("refused: %v", err)
	}
	if strings.Contains(p.u.String(), "...") || strings.Contains(safeURL(p.u), "holocron-abc123") {
		t.Errorf("the topic must not appear in a safe URL: %s", safeURL(p.u))
	}
	p = newPush(t, func(c *PushConfig) { c.Server = ts.URL; c.TokenName = "absent" }, func(string) (string, bool) { return "", false })
	if err := p.Send(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), `"absent"`) {
		t.Errorf("missing token: %v", err)
	}
}

func TestPushConfigValidation(t *testing.T) {
	p, err := NewPush(PushConfig{Enabled: true, Topic: "abc"}, nil)
	if err != nil || p.u.String() != "https://ntfy.sh/abc" {
		t.Errorf("default server: %v %v", p.u, err)
	}
	if p, err := NewPush(PushConfig{}, nil); err != nil || p.Enabled() {
		t.Errorf("zero config")
	}
	for _, c := range []PushConfig{
		{Enabled: true},                                                 // no topic
		{Enabled: true, Topic: "has space"},                             // bad topic
		{Enabled: true, Topic: "a/b"},                                   // path injection
		{Enabled: true, Topic: strings.Repeat("a", 65)},                 // too long
		{Enabled: true, Topic: "ok", Server: "http://ntfy.example.com"}, // clear text to a public host
	} {
		if _, err := NewPush(c, nil); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	if p, err := NewPush(PushConfig{Enabled: true, Topic: "ok", Server: "http://192.168.1.10:8080/ntfy/?x=1"}, nil); err != nil || p.u.String() != "http://192.168.1.10:8080/ntfy/ok" {
		t.Errorf("self-hosted on the LAN: %v %v", p.u, err)
	}
}
