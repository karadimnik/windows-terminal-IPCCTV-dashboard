package ctlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"holocron/internal/cli"
)

// Client talks to a running supervisor's control endpoint.
type Client struct {
	base string
	key  string
	hc   *http.Client
}

var secretRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Dial reads <root>/_state/ctl.addr and ctl.secret. A missing or malformed file is ErrUnreachable
// (no supervisor serves one); the connection itself is made per call.
func Dial(root string) (*Client, error) {
	state := filepath.Join(root, "_state")
	addr, err := os.ReadFile(filepath.Join(state, AddrFile))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	key, err := os.ReadFile(filepath.Join(state, SecretFile))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read %s: %v", ErrUnreachable, SecretFile, err)
	}
	a, k := strings.TrimSpace(string(addr)), strings.TrimSpace(string(key))
	host, _, err := net.SplitHostPort(a)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || !secretRE.MatchString(k) {
		return nil, fmt.Errorf("%w: %s or %s is malformed", ErrUnreachable, AddrFile, SecretFile)
	}
	return &Client{base: "http://" + a, key: k, hc: &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// APIError is a non-2xx answer.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("control endpoint answered %d %s", e.Code, http.StatusText(e.Code))
	}
	return fmt.Sprintf("control endpoint answered %d: %s", e.Code, e.Message)
}

// Is maps the codes back onto the package errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrBusy:
		return e.Code == http.StatusConflict
	case ErrNotFound:
		return e.Code == http.StatusNotFound
	case ErrNotRunning:
		return e.Code == http.StatusServiceUnavailable
	case cli.ErrRefused:
		return e.Code == http.StatusUnprocessableEntity || e.Code == http.StatusBadRequest
	}
	return false
}

// call sends one request. The action id and the actor of ctx travel in headers, so the
// supervisor's records of this call carry the caller's action id.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set(HeaderKey, c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if id := cli.ActionIDFrom(ctx); id != "" {
		req.Header.Set(HeaderActionID, id)
	}
	if a := cli.ActorFrom(ctx); a != cli.ActorSystem {
		req.Header.Set(HeaderActor, a)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var rp struct {
		OK    bool            `json:"ok"`
		Error string          `json:"error"`
		Data  json.RawMessage `json:"data"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &rp)
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Code: resp.StatusCode, Message: rp.Error}
	}
	if out != nil && len(rp.Data) > 0 {
		return json.Unmarshal(rp.Data, out)
	}
	return nil
}

// State is GET /v1/state.
func (c *Client) State(ctx context.Context) (State, error) {
	var s State
	err := c.call(ctx, http.MethodGet, "/v1/state", nil, &s)
	return s, err
}

// Stop is POST /v1/stop: the ordered stop; it returns when the supervisor has stopped.
func (c *Client) Stop(ctx context.Context, kind string) (StopReport, error) {
	var r StopReport
	err := c.call(ctx, http.MethodPost, "/v1/stop", stopRequest{Kind: kind}, &r)
	return r, err
}

// Reload is POST /v1/reload.
func (c *Client) Reload(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/reload", struct{}{}, nil)
}

// SetRecord is POST /v1/cameras/{name}/record.
func (c *Client) SetRecord(ctx context.Context, camera string, enabled bool) (Changed, error) {
	var ch Changed
	err := c.call(ctx, http.MethodPost, "/v1/cameras/"+url.PathEscape(camera)+"/record", enabledRequest{Enabled: &enabled}, &ch)
	return ch, err
}

// SetLive is POST /v1/live.
func (c *Client) SetLive(ctx context.Context, enabled bool) (Changed, error) {
	var ch Changed
	err := c.call(ctx, http.MethodPost, "/v1/live", enabledRequest{Enabled: &enabled}, &ch)
	return ch, err
}

// Levels is GET /v1/log/level.
func (c *Client) Levels(ctx context.Context) (Levels, error) {
	var l Levels
	err := c.call(ctx, http.MethodGet, "/v1/log/level", nil, &l)
	return l, err
}

// SetLevel is POST /v1/log/level; component "all" sets the default.
func (c *Client) SetLevel(ctx context.Context, component, level string) (Levels, error) {
	var l Levels
	err := c.call(ctx, http.MethodPost, "/v1/log/level", levelRequest{Component: component, Level: level}, &l)
	return l, err
}
