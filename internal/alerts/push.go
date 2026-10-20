package alerts

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var topicRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Push is the "push" channel: an ntfy-style HTTP POST that reaches a phone running the ntfy app
// (ntfy.sh or a self-hosted server). No dependency, no account needed on ntfy.sh.
type Push struct {
	enabled bool
	u       *url.URL
	token   string
	lookup  SecretLookup
	client  *http.Client
}

// NewPush validates cfg. A disabled config yields a disabled channel, not an error.
func NewPush(cfg PushConfig, lookup SecretLookup) (*Push, error) {
	p := &Push{token: cfg.TokenName, lookup: lookup, client: httpClient()}
	if !cfg.Enabled {
		return p, nil
	}
	server := cfg.Server
	if server == "" {
		server = DefaultPushServer
	}
	if !topicRE.MatchString(cfg.Topic) {
		return nil, fmt.Errorf("alerts.push.topic: 1-64 letters, digits, _ or -")
	}
	su, err := checkURL(server, "push.server")
	if err != nil {
		return nil, err
	}
	u := *su
	u.Path = strings.TrimRight(su.Path, "/") + "/" + cfg.Topic
	u.RawQuery, u.Fragment = "", ""
	p.u, p.enabled = &u, true
	return p, nil
}

func (*Push) Name() string    { return "push" }
func (p *Push) Enabled() bool { return p.enabled }
func (*Push) Network() bool   { return true }

// pushPriority maps severity to ntfy priority 2 (low), 3 (default), 4 (high).
func pushPriority(s Severity) int {
	switch s {
	case Error:
		return 4
	case Warning:
		return 3
	}
	return 2
}

func pushTags(ev Event) string {
	sev := map[Severity]string{Error: "rotating_light", Warning: "warning", Info: "information_source"}[ev.Severity]
	return sev + ",holocron,alert-" + strconv.Itoa(ev.ID)
}

// headerText makes s safe for an HTTP header value: no line breaks, RFC 2047 when not ASCII
// (ntfy decodes it), bounded.
func headerText(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	if utf8.RuneCountInString(s) > maxHeaderOrTitleRunes {
		s = string([]rune(s)[:maxHeaderOrTitleRunes])
	}
	return mime.QEncoding.Encode("utf-8", s)
}

func (p *Push) Send(ctx context.Context, ev Event) error {
	body := ev.Message
	if body == "" {
		body = ev.Title
	}
	req, err := http.NewRequest(http.MethodPost, p.u.String(), strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST %s: bad request", safeURL(p.u))
	}
	req.Header.Set("Title", headerText(ev.Title))
	req.Header.Set("Priority", strconv.Itoa(pushPriority(ev.Severity)))
	req.Header.Set("Tags", pushTags(ev))
	if p.token != "" {
		v, ok := "", false
		if p.lookup != nil {
			v, ok = p.lookup(p.token)
		}
		if !ok {
			return fmt.Errorf("secret %q for push was not found in the secrets store", p.token)
		}
		req.Header.Set("Authorization", "Bearer "+v)
	}
	return post(ctx, p.client, req, p.u)
}
