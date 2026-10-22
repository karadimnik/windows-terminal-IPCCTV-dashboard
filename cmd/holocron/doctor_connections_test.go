package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rtspDial answers RTSP OPTIONS on the addresses in up and refuses every other.
func rtspDial(up ...string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		for _, u := range up {
			if u == addr {
				a, b := net.Pipe()
				go func() {
					bufio.NewReader(b).ReadString('\n')
					fmt.Fprint(b, "RTSP/1.0 401 Unauthorized\r\nCSeq: 1\r\n\r\n")
					b.Close()
				}()
				return a, nil
			}
		}
		return nil, errRefusedInTest
	}
}

func lineWith(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func TestDoctorCameraReachability(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name      string
		reachable bool
		grow      bool
		want      string
	}{
		{"reachable", true, false, "connections.camera ok   Front          reachable: 192.0.2.10:554 answered \"RTSP/1.0 401 Unauthorized\""},
		{"unreachable but recording", false, true, "connections.camera warn Front          unreachable (TCP 192.0.2.10:554: connection refused (test)), but its recording still grows"},
		{"unreachable and not recording", false, false, "connections.camera fail Front          unreachable (TCP 192.0.2.10:554: connection refused (test)) and not recording"},
	} {
		r := newRig(t)
		r.running()
		cur := r.segment("Front", now.Add(-time.Minute), now.Add(-time.Minute), 10)
		var up []string
		if c.reachable {
			up = []string{"192.0.2.10:554"}
		}
		r.doctorTools(func(d *doctorDeps) {
			d.dial = rtspDial(up...)
			d.sleep = func(context.Context, time.Duration) error {
				if c.grow {
					os.WriteFile(cur, make([]byte, 100), 0o644)
				}
				return nil
			}
		})
		out, _, _ := r.run("--json", "doctor", "--group", "connections,recording")
		if got := lineWith(textOf(t, out), "connections.camera "); !strings.HasPrefix(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// relayServer is a relay control API that wants the test login and serves the given paths.
func relayServer(t *testing.T, secret string, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("camrelayapi:"+secret)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoctorRelayPaths(t *testing.T) {
	good := strings.Repeat("ab", 32)
	for _, c := range []struct {
		name, fileSecret, body string
		want                   []string
	}{
		{"ready", good, `{"pageCount":1,"items":[{"name":"front","ready":true,"readers":[{"type":"rtspSession"},{"type":"rtspSession"}]}]}`,
			[]string{"connections.relay ok   relay          relay API http://", "connections.relay ok   Front          relay path front ready, 2 reader(s)"}},
		{"path missing", good, `{"pageCount":1,"items":[{"name":"other","ready":true}]}`,
			[]string{"connections.relay fail Front          relay path front is not ready"}},
		{"login refused", strings.Repeat("cd", 32), `{}`,
			[]string{"connections.relay fail relay          relay API http://", "refused this system's login (alert 130)"}},
		{"no login file", "", `{}`, []string{"connections.relay fail relay          the relay API login cannot be read"}},
	} {
		r := newRig(t)
		auth := r.platformAuth()
		if c.fileSecret != "" {
			os.WriteFile(filepath.Join(auth, "relay_api_secret.txt"), []byte(c.fileSecret), 0o600)
		}
		srv := relayServer(t, good, c.body)
		r.setConfig(`"RelayApiBase": "` + srv.URL + `",`)
		r.running()
		r.doctorTools(func(d *doctorDeps) { d.http = srv.Client() })
		out, _, _ := r.run("--json", "doctor", "--group", "connections")
		text := textOf(t, out)
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, text)
			}
		}
		if c.name == "path missing" && !strings.Contains(out, `"fix": "holocron cam test \"Front\" ; holocron logs --component relay --since 10m"`) {
			t.Errorf("fix:\n%s", out)
		}
	}
}

func TestDoctorListenersAnswer(t *testing.T) {
	r := newRig(t)
	r.setConfig(`"LiveViewEnabled": true,`)
	r.running()
	r.doctorTools(func(d *doctorDeps) { d.dial = rtspDial("127.0.0.1:8554", "127.0.0.1:8889", "127.0.0.1:18821") })
	out, _, _ := r.run("--json", "doctor", "--group", "connections")
	text := textOf(t, out)
	for _, w := range []string{
		"connections.port ok   relay RTSP     tcp 127.0.0.1:8554 answers",
		"connections.port fail relay API      nothing answers on tcp 127.0.0.1:9997",
		"connections.port ok   live WHEP      tcp 127.0.0.1:8889 answers",
		"connections.port ok   web listen     tcp 127.0.0.1:18821 answers",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("no %q in\n%s", w, text)
		}
	}
	if !strings.Contains(out, "holocron logs --component relay --since 10m ; holocron restart --detach") {
		t.Errorf("fix:\n%s", out)
	}
}

// A dialer that never answers cannot hold doctor past its network budget.
func TestDoctorBlockingDialerStaysInTheBudget(t *testing.T) {
	r := newRig(t)
	r.setConfig(`"LiveViewEnabled": true,`)
	r.running()
	block := func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	r.doctorTools(func(d *doctorDeps) { d.dial, d.netBudget = block, time.Second })
	start := time.Now()
	out, _, code := r.run("doctor")
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("doctor took %s with a blocking dialer and a 1 s budget", el)
	}
	if code != 1 || !strings.Contains(out, "fail Front          unreachable (") || !strings.Contains(out, "context deadline exceeded) and not recording") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}
