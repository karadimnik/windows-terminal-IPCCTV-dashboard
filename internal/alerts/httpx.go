package alerts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// checkURL accepts https, and http only for a loopback or private address (a literal IP or
// "localhost"): an alert and its secret header must not cross the open internet in clear text.
func checkURL(raw, what string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("alerts.%s: not a usable URL", what)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !privateHost(u.Hostname()) {
			return nil, fmt.Errorf("alerts.%s: http is allowed only for a loopback or private address, use https", what)
		}
	default:
		return nil, fmt.Errorf("alerts.%s: scheme must be https (or http for a private address)", what)
	}
	return u, nil
}

func privateHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()
}

// safeURL is a URL fit for a log or an error: scheme and host only. Path, query and userinfo can
// all carry a secret (a Slack webhook path, an ntfy topic, ?token=).
func safeURL(u *url.URL) string { return u.Scheme + "://" + u.Host + "/..." }

// httpClient never follows a redirect: the secret header must go only where it was configured.
func httpClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// post sends req and turns every failure into an error that cannot contain the request URL's
// secrets.
func post(ctx context.Context, c *http.Client, req *http.Request, u *url.URL) error {
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		msg := strings.ReplaceAll(err.Error(), u.String(), safeURL(u))
		if u.RawQuery != "" {
			msg = strings.ReplaceAll(msg, u.RawQuery, "***")
		}
		return fmt.Errorf("POST %s: %s", safeURL(u), msg)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("POST %s: HTTP %d", safeURL(u), resp.StatusCode)
	}
	return nil
}
