package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var apiLogin = Login{User: "camrelayapi", Secret: strings.Repeat("ab", 32)}

const twoReady = `{"itemCount":3,"pageCount":1,"items":[
 {"name":"cameraone","confName":"cameraone","source":{"type":"rtspSource","id":""},"ready":true,"readyTime":"2026-10-20T08:00:00Z","tracks":["H264"],"bytesReceived":10,"bytesSent":0,"readers":[]},
 {"name":"cameratwo","confName":"cameratwo","source":null,"ready":false,"readyTime":null,"tracks":[],"readers":[]},
 {"name":"camerathree","confName":"camerathree","source":{"type":"rtspSource","id":""},"ready":true,"readyTime":"2026-10-20T08:00:01Z","tracks":["H264","MPEG-4 Audio"],"readers":[]}]}`

type fakeRelay struct {
	t        *testing.T
	requests atomic.Int32
	anon     atomic.Int32
	// handler decides; it gets whether the request carried the right login.
	handle func(w http.ResponseWriter, r *http.Request, authed bool)
}

func (f *fakeRelay) start() (*httptest.Server, *API) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.URL.Path != "/v3/paths/list" {
			f.t.Errorf("unexpected route %s", r.URL.Path)
		}
		h := r.Header.Get("Authorization")
		if h == "" {
			f.anon.Add(1)
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(apiLogin.User+":"+apiLogin.Secret))
		f.handle(w, r, h == want)
	}))
	f.t.Cleanup(srv.Close)
	return srv, NewAPI(srv.URL, apiLogin, 300*time.Millisecond)
}

func serve(body string) func(http.ResponseWriter, *http.Request, bool) {
	return func(w http.ResponseWriter, r *http.Request, authed bool) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

func TestReadyTable(t *testing.T) {
	cases := []struct {
		name      string
		handle    func(http.ResponseWriter, *http.Request, bool)
		state     State
		ready     []string
		errIs     error
		wantHas   map[string]bool
		wantCalls int32
	}{
		{"ready", serve(twoReady), OK, []string{"cameraone", "camerathree"}, nil,
			map[string]bool{"cameraone": true, "cameratwo": false, "camerathree": true, "unknown": false}, 1},
		{"empty", serve(`{"itemCount":0,"pageCount":0,"items":[]}`), OK, []string{}, nil, map[string]bool{"cameraone": false}, 1},
		{"items null", serve(`{"itemCount":0,"pageCount":0,"items":null}`), OK, []string{}, nil, nil, 1},
		{"denied 401", func(w http.ResponseWriter, r *http.Request, _ bool) {
			w.Header().Set("WWW-Authenticate", "Basic")
			w.WriteHeader(401)
		}, Denied, []string{}, nil, map[string]bool{"cameraone": true, "anything": true}, 1},
		{"denied 403", func(w http.ResponseWriter, r *http.Request, _ bool) { w.WriteHeader(403) }, Denied, []string{}, nil,
			map[string]bool{"cameraone": true}, 1},
		{"server error", func(w http.ResponseWriter, r *http.Request, _ bool) { w.WriteHeader(500) }, Unavailable, nil, ErrUnavailable,
			map[string]bool{"cameraone": false}, 1},
		{"not found", func(w http.ResponseWriter, r *http.Request, _ bool) { w.WriteHeader(404) }, Unavailable, nil, ErrUnavailable, nil, 1},
		{"redirect is not followed", func(w http.ResponseWriter, r *http.Request, _ bool) { http.Redirect(w, r, "http://127.0.0.1:1/", 302) }, Unavailable, nil, ErrUnavailable, nil, 1},
		{"malformed: not json", serve(`<html>hello</html>`), Unavailable, nil, ErrMalformed, nil, 1},
		{"malformed: empty body", serve(``), Unavailable, nil, ErrMalformed, nil, 1},
		{"malformed: no items", serve(`{"itemCount":0}`), Unavailable, nil, ErrMalformed, nil, 1},
		{"malformed: items not a list", serve(`{"items":{"a":1}}`), Unavailable, nil, ErrMalformed, nil, 1},
		{"malformed: truncated", serve(`{"items":[{"name":"cam`), Unavailable, nil, ErrMalformed, nil, 1},
		{"no login at all and the api open", func(w http.ResponseWriter, r *http.Request, authed bool) { serve(twoReady)(w, r, authed) }, OK, []string{"cameraone", "camerathree"}, nil, nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRelay{t: t, handle: tc.handle}
			_, api := f.start()
			set, err := api.Ready(context.Background())
			if set.State != tc.state {
				t.Fatalf("state %v, want %v (err %v)", set.State, tc.state, err)
			}
			if tc.errIs != nil && !errors.Is(err, tc.errIs) {
				t.Errorf("err %v, want %v", err, tc.errIs)
			}
			if tc.errIs == nil && err != nil {
				t.Errorf("err %v", err)
			}
			if tc.errIs != nil && !errors.Is(err, ErrUnavailable) {
				t.Errorf("every failure must satisfy ErrUnavailable: %v", err)
			}
			if tc.ready != nil && !reflect.DeepEqual(set.Names(), tc.ready) {
				t.Errorf("names %v, want %v", set.Names(), tc.ready)
			}
			for p, want := range tc.wantHas {
				if set.Has(p) != want {
					t.Errorf("Has(%s) = %v", p, !want)
				}
			}
			if n := f.requests.Load(); n != tc.wantCalls {
				t.Errorf("%d requests, want %d (one list request per cycle)", n, tc.wantCalls)
			}
			if err != nil && strings.Contains(err.Error(), apiLogin.Secret) {
				t.Error("the error carries the secret")
			}
		})
	}
}

// MediaMTX answers a wrong login only after a pause; the credentialed request times out and the
// anonymous one, answered at once, proves the relay alive and wanting a login.
func TestWrongLoginDelayedIsDeniedNotDown(t *testing.T) {
	release := make(chan struct{})
	f := &fakeRelay{t: t, handle: func(w http.ResponseWriter, r *http.Request, authed bool) {
		if r.Header.Get("Authorization") != "" {
			select {
			case <-release:
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
		w.WriteHeader(401)
	}}
	_, api := f.start()
	api.Timeout = 150 * time.Millisecond
	t.Cleanup(func() { close(release) })

	start := time.Now()
	set, err := api.Ready(context.Background())
	took := time.Since(start)
	if err != nil || set.State != Denied || !set.Has("cameraone") {
		t.Fatalf("%v %v", set.State, err)
	}
	if took < 140*time.Millisecond || took > 2*time.Second {
		t.Errorf("took %v: expected about the request timeout, not the server's delay", took)
	}
	if f.anon.Load() != 1 || f.requests.Load() != 2 {
		t.Errorf("%d anonymous of %d requests", f.anon.Load(), f.requests.Load())
	}
}

// With a long enough budget the delayed 401 itself arrives and is Denied without the second request.
func TestDelayedDenialWithinTheBudget(t *testing.T) {
	f := &fakeRelay{t: t, handle: func(w http.ResponseWriter, r *http.Request, authed bool) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(401)
	}}
	_, api := f.start()
	api.Timeout = 2 * time.Second
	set, err := api.Ready(context.Background())
	if err != nil || set.State != Denied || f.requests.Load() != 1 {
		t.Errorf("%v %v %d", set.State, err, f.requests.Load())
	}
}

// A relay that hangs on both requests is down, after two timeouts and no more.
func TestSilentRelayIsUnavailable(t *testing.T) {
	f := &fakeRelay{t: t, handle: func(w http.ResponseWriter, r *http.Request, authed bool) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}}
	_, api := f.start()
	api.Timeout = 100 * time.Millisecond
	start := time.Now()
	set, err := api.Ready(context.Background())
	if set.State != Unavailable || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v %v", set.State, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %v", took)
	}
	if f.requests.Load() != 2 {
		t.Errorf("%d requests", f.requests.Load())
	}
}

func TestRefusedConnectionIsUnavailableAtOnce(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	api := NewAPI(base, apiLogin, time.Second)
	start := time.Now()
	set, err := api.Ready(context.Background())
	if set.State != Unavailable || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v %v", set.State, err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("a refused connection must not wait for the timeout")
	}
	if strings.Contains(err.Error(), base) {
		t.Errorf("the error repeats the URL: %v", err)
	}
}

func TestContextCancelIsReportedAsIs(t *testing.T) {
	f := &fakeRelay{t: t, handle: func(w http.ResponseWriter, r *http.Request, authed bool) { <-r.Context().Done() }}
	_, api := f.start()
	api.Timeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := api.Ready(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v", err)
	}
	if f.anon.Load() != 0 {
		t.Error("a cancelled cycle must not fall back to an anonymous request")
	}
}

func TestRequestCarriesTheLoginInTheHeaderOnly(t *testing.T) {
	var gotHeader, gotQuery string
	f := &fakeRelay{t: t, handle: func(w http.ResponseWriter, r *http.Request, authed bool) {
		gotHeader, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		serve(twoReady)(w, r, authed)
	}}
	_, api := f.start()
	if _, err := api.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotHeader != "Basic "+base64.StdEncoding.EncodeToString([]byte("camrelayapi:"+apiLogin.Secret)) {
		t.Errorf("header %q", gotHeader)
	}
	if strings.Contains(gotQuery, apiLogin.Secret) {
		t.Error("login in the query")
	}
}

func TestPagedListIsFollowed(t *testing.T) {
	f := &fakeRelay{t: t}
	f.handle = func(w http.ResponseWriter, r *http.Request, authed bool) {
		if r.URL.Query().Get("page") == "0" {
			serve(`{"pageCount":2,"items":[{"name":"a","ready":true}]}`)(w, r, authed)
			return
		}
		serve(`{"pageCount":2,"items":[{"name":"b","ready":true},{"name":"c","ready":false}]}`)(w, r, authed)
	}
	_, api := f.start()
	set, err := api.Ready(context.Background())
	if err != nil || !reflect.DeepEqual(set.Names(), []string{"a", "b"}) {
		t.Errorf("%v %v", set.Names(), err)
	}
}

func TestTimeoutFloorAndDefault(t *testing.T) {
	if (&API{}).timeout() != DefaultProbeTimeout || (&API{Timeout: time.Millisecond}).timeout() != 100*time.Millisecond {
		t.Error("timeout defaults")
	}
}

func TestLoginWatchAlertsOncePerEpisode(t *testing.T) {
	var w LoginWatch
	steps := []struct {
		in   State
		want Transition
	}{
		{OK, NoChange}, {Denied, LoginRefused}, {Denied, NoChange}, {Unavailable, NoChange}, {Denied, NoChange},
		{OK, LoginAccepted}, {OK, NoChange}, {Denied, LoginRefused},
	}
	for i, s := range steps {
		if got := w.Observe(s.in); got != s.want {
			t.Errorf("step %d (%v): got %v want %v", i, s.in, got, s.want)
		}
	}
	title, msg := LoginRefusedAlert("http://127.0.0.1:9997")
	if title == "" || !strings.Contains(msg, "http://127.0.0.1:9997") || AlertLoginRefused != 130 {
		t.Error(title, msg)
	}
}

func TestWaitStarted(t *testing.T) {
	var calls atomic.Int32
	f := &fakeRelay{t: t, handle: func(w http.ResponseWriter, r *http.Request, authed bool) {
		if calls.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		serve(twoReady)(w, r, authed)
	}}
	_, api := f.start()
	pol := ReadinessPolicy{StartupWait: 5 * time.Second, Poll: 20 * time.Millisecond}
	set, ok := api.WaitStarted(context.Background(), pol, nil)
	if !ok || set.State != OK || calls.Load() != 3 {
		t.Errorf("%v %v %d", ok, set.State, calls.Load())
	}

	// a refused login is an answer
	g := &fakeRelay{t: t, handle: func(w http.ResponseWriter, r *http.Request, _ bool) { w.WriteHeader(401) }}
	_, api = g.start()
	if set, ok := api.WaitStarted(context.Background(), pol, nil); !ok || set.State != Denied {
		t.Errorf("denied: %v %v", ok, set.State)
	}

	// the budget runs out, without spinning
	h := &fakeRelay{t: t, handle: func(w http.ResponseWriter, r *http.Request, _ bool) { w.WriteHeader(500) }}
	_, api = h.start()
	start := time.Now()
	_, ok = api.WaitStarted(context.Background(), ReadinessPolicy{StartupWait: 300 * time.Millisecond, Poll: 100 * time.Millisecond}, nil)
	if ok || time.Since(start) < 250*time.Millisecond {
		t.Errorf("budget: %v after %v", ok, time.Since(start))
	}
	if n := h.requests.Load(); n < 2 || n > 6 {
		t.Errorf("%d polls in 300 ms at 100 ms", n)
	}

	// the child exiting ends the wait at once
	exited := make(chan struct{})
	close(exited)
	start = time.Now()
	if _, ok := api.WaitStarted(context.Background(), ReadinessPolicy{StartupWait: 5 * time.Second, Poll: time.Second}, exited); ok || time.Since(start) > time.Second {
		t.Errorf("exited: %v after %v", ok, time.Since(start))
	}
}

func TestNewReadySet(t *testing.T) {
	s := NewReadySet([]string{"front", "back", ""})
	if s.State != OK || !s.Has("front") || !s.Has("back") || s.Has("side") || s.Has("") {
		t.Fatalf("ok set: %+v", s)
	}
	if got := s.Names(); len(got) != 2 || got[0] != "back" || got[1] != "front" {
		t.Fatalf("names %q", got)
	}
	if e := NewReadySet(nil); e.State != OK || e.Has("front") || len(e.Names()) != 0 {
		t.Fatalf("empty ok set: %+v", e)
	}
	d := DeniedReadySet()
	if d.State != Denied || !d.Has("anything") || len(d.Names()) != 0 {
		t.Fatalf("denied: %+v", d)
	}
	u := UnavailableReadySet()
	if u.State != Unavailable || u.Has("front") {
		t.Fatalf("unavailable: %+v", u)
	}
}

func TestReadyCountsReaders(t *testing.T) {
	f := &fakeRelay{t: t, handle: serve(`{"pageCount":1,"items":[{"name":"a","ready":true,"readers":[{"type":"rtspSession","id":"1"},{"type":"webRTCSession","id":"2"}]},{"name":"b","ready":true,"readers":[]},{"name":"c","ready":false,"readers":[{"type":"x"}]}]}`)}
	_, api := f.start()
	set, err := api.Ready(context.Background())
	if err != nil || set.Readers("a") != 2 || set.Readers("b") != 0 || set.Readers("c") != 0 || set.Readers("zz") != 0 {
		t.Errorf("readers: %v a=%d b=%d c=%d", err, set.Readers("a"), set.Readers("b"), set.Readers("c"))
	}
}
