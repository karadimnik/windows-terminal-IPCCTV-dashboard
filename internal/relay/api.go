package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// State is what the control API said.
type State int

const (
	// Unavailable: the relay did not answer usefully (refused, timed out, not 2xx, garbage).
	Unavailable State = iota
	// Denied: the relay is running and refused the product's login (HTTP 401 or 403, or the delayed
	// wrong-login answer). Denied is not down: RTSP needs no login, so the relay is still serving.
	Denied
	// OK: the API answered 2xx with a path list.
	OK
)

func (s State) String() string {
	switch s {
	case OK:
		return "ok"
	case Denied:
		return "denied"
	}
	return "unavailable"
}

var (
	// ErrUnavailable wraps every failure that leaves State Unavailable.
	ErrUnavailable = errors.New("relay: control API unavailable")
	// ErrMalformed is a 2xx answer that is not a path list. It is Unavailable too, as in PowerShell.
	ErrMalformed = errors.New("relay: control API answer is not a path list")
)

// ReadySet is the answer of one list request: the paths that are serving. When the login was Denied,
// Has is true for every path, because the relay is up and a consumer sent direct by a configuration
// fault would put the camera password on a command line and take the camera's second session.
type ReadySet struct {
	State State
	paths map[string]struct{}
}

// NewReadySet is an OK answer serving exactly the given paths (for callers and tests that build a
// set without a request).
func NewReadySet(paths []string) ReadySet {
	set := ReadySet{State: OK, paths: map[string]struct{}{}}
	for _, p := range paths {
		if p != "" {
			set.paths[p] = struct{}{}
		}
	}
	return set
}

// DeniedReadySet is the answer of a relay that refused the login: every path counts as ready.
func DeniedReadySet() ReadySet { return ReadySet{State: Denied} }

// UnavailableReadySet is the answer of a relay that did not answer: nothing is ready.
func UnavailableReadySet() ReadySet { return ReadySet{State: Unavailable} }

// Has reports whether a consumer may use the relay for this path.
func (r ReadySet) Has(path string) bool {
	if r.State == Denied {
		return true
	}
	_, ok := r.paths[path]
	return r.State == OK && ok
}

// Names are the serving paths, sorted. Empty when Denied or Unavailable (the view is lost).
func (r ReadySet) Names() []string {
	out := make([]string, 0, len(r.paths))
	for p := range r.paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// DefaultProbeTimeout is RelayProbeTimeoutMs' default; the floor is 100 ms.
const (
	DefaultProbeTimeout = 1500 * time.Millisecond
	minProbeTimeout     = 100 * time.Millisecond
	maxBody             = 8 << 20
	itemsPerPage        = 1000
	maxPages            = 20
)

// API is the control-API client: loopback, Basic auth, system proxy bypassed.
type API struct {
	// Base is RelayApiBase, for example http://127.0.0.1:9997.
	Base  string
	Login Login
	// Timeout bounds each request (RelayProbeTimeoutMs); below 100 ms it is raised to 100 ms.
	Timeout time.Duration
	// HTTP is optional; the default client has no proxy, no keep-alive and follows no redirect.
	HTTP *http.Client
}

// NewAPI returns a client for the given RelayApiBase and login with the default HTTP client.
func NewAPI(base string, login Login, timeout time.Duration) *API {
	return &API{Base: base, Login: login, Timeout: timeout}
}

func (a *API) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return defaultClient
}

var defaultClient = &http.Client{
	Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (a *API) timeout() time.Duration {
	if a.Timeout <= 0 {
		return DefaultProbeTimeout
	}
	if a.Timeout < minProbeTimeout {
		return minProbeTimeout
	}
	return a.Timeout
}

// Ready is Get-CamRelayReadyPaths: one list request (more only when the relay itself pages the list)
// returning the serving paths. The result is never an error for a Denied login; it is ErrUnavailable
// (State Unavailable) when the relay did not answer, and the error is ctx.Err() when ctx ended.
//
// MediaMTX answers a WRONG login only after a random pause of up to ~4 s but refuses a MISSING one
// at once (measured on v1.21.2: wrong 704-3784 ms, none 0-1 ms, right under 10 ms). So the request
// has a deadline (Timeout, not a spin), and one that runs out of time is asked again without a
// login: an immediate 401 or 403 proves the relay is alive and wanted a login the product did not
// satisfy.
func (a *API) Ready(ctx context.Context) (ReadySet, error) {
	auth := !a.Login.Zero()
	set, err := a.list(ctx, auth)
	if err != nil && auth && isTimeout(ctx, err) {
		anon, aerr := a.list(ctx, false)
		if aerr == nil || errors.Is(aerr, errDenied) {
			set, err = anon, aerr
		}
	}
	switch {
	case err == nil:
		return set, nil
	case errors.Is(err, errDenied):
		return ReadySet{State: Denied}, nil
	case ctx.Err() != nil:
		return ReadySet{}, ctx.Err()
	}
	return ReadySet{State: Unavailable}, err
}

// errDenied is internal: Ready turns it into State Denied.
var errDenied = errors.New("denied")

func isTimeout(parent context.Context, err error) bool {
	return parent.Err() == nil && errors.Is(err, context.DeadlineExceeded)
}

type pathList struct {
	PageCount int             `json:"pageCount"`
	Items     json.RawMessage `json:"items"` // absent: malformed; null: an empty list
}

type pathItem struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

func (a *API) list(ctx context.Context, auth bool) (ReadySet, error) {
	set := ReadySet{State: OK, paths: map[string]struct{}{}}
	for page := 0; ; page++ {
		body, err := a.get(ctx, fmt.Sprintf("v3/paths/list?itemsPerPage=%d&page=%d", itemsPerPage, page), auth)
		if err != nil {
			return ReadySet{}, err
		}
		var l pathList
		var items []pathItem
		if err := json.Unmarshal(body, &l); err != nil || len(l.Items) == 0 || json.Unmarshal(l.Items, &items) != nil {
			return ReadySet{}, fmt.Errorf("%w: %w", ErrUnavailable, ErrMalformed)
		}
		for _, it := range items {
			if it.Ready && it.Name != "" {
				set.paths[it.Name] = struct{}{}
			}
		}
		if page+1 >= l.PageCount || page+1 >= maxPages {
			return set, nil
		}
	}
}

func (a *API) get(ctx context.Context, route string, auth bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()
	u := strings.TrimRight(a.Base, "/") + "/" + strings.TrimLeft(route, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if auth {
		req.SetBasicAuth(a.Login.User, a.Login.Secret)
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, scrub(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, errDenied
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, scrub(err))
	}
	return body, nil
}

// scrub drops the request URL from a transport error; it holds no login, but a log line has no use for it.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// WaitStarted polls Ready every poll interval until the API answers at all (OK or Denied: a refused
// login is an answer, the relay is up), until wait has passed, ctx ends, or exited is closed (the
// child died). It waits for the API, not for every path: a camera that is offline has a path that
// never becomes ready. answered is false when the budget ran out.
func (a *API) WaitStarted(ctx context.Context, p ReadinessPolicy, exited <-chan struct{}) (set ReadySet, answered bool) {
	ctx, cancel := context.WithTimeout(ctx, p.StartupWait)
	defer cancel()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ReadySet{}, false
		case <-exited:
			return ReadySet{}, false
		case <-timer.C:
		}
		if s, err := a.Ready(ctx); err == nil {
			return s, true
		}
		timer.Reset(p.Poll)
	}
}

// Transition is what a LoginWatch observation changed.
type Transition int

const (
	NoChange Transition = iota
	// LoginRefused starts an episode: raise alert 130 (once) and log it.
	LoginRefused
	// LoginAccepted ends it: log "the restreamer's control API accepts the product's login again".
	LoginAccepted
)

// AlertLoginRefused is the alert id for the control API refusing the product's login (IR section 6).
const AlertLoginRefused = 130

// LoginWatch is Update-CamRelayLoginEpisode: one alert per episode of the API refusing the login. A
// relay that goes down meanwhile (Unavailable) does not end the episode, so a restart loop cannot
// repeat the alert. Not safe for concurrent use.
type LoginWatch struct{ refused bool }

// Observe feeds the latest API answer.
func (w *LoginWatch) Observe(s State) Transition {
	switch {
	case s == Denied && !w.refused:
		w.refused = true
		return LoginRefused
	case s == OK && w.refused:
		w.refused = false
		return LoginAccepted
	}
	return NoChange
}

// Refused reports whether an episode is open.
func (w *LoginWatch) Refused() bool { return w.refused }

// LoginRefusedAlert is the title and message of alert 130 (neither carries the login).
func LoginRefusedAlert(apiBase string) (title, message string) {
	return "the restreamer refused the recorder's login",
		fmt.Sprintf("The MediaMTX control API at %s refused the product's login (HTTP 401). Viewers and recorders are kept on the relay, but whether each camera is being served cannot be seen. Restart the supervisor: the restreamer start rewrites its config to the stored login.", apiBase)
}
