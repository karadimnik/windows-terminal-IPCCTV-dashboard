package main

import (
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
)

// listenAt rewrites the rig's web.listen and returns the address.
func (r *rig) listenAt(addr string) {
	cfg, _ := os.ReadFile(r.cfg)
	os.WriteFile(r.cfg, []byte(regexp.MustCompile(`"listen": "[^"]*"`).ReplaceAllString(string(cfg), `"listen": "`+addr+`"`)), 0o644)
}

func TestLiveURL(t *testing.T) {
	for _, c := range []struct{ listen, camera, want string }{
		{"127.0.0.1:8787", "", "http://127.0.0.1:8787/#live"},
		{"127.0.0.1:18821", "Front", "http://127.0.0.1:18821/#live/Front"},
		{"127.0.0.1:8787", "Side Door/é#?", "http://127.0.0.1:8787/#live/Side%20Door%2F%C3%A9%23%3F"},
		{"0.0.0.0:9000", "", "http://127.0.0.1:9000/#live"},
		{"[::]:9000", "", "http://127.0.0.1:9000/#live"},
		{"localhost:8787", "", "http://localhost:8787/#live"},
		{"garbage", "", "http://127.0.0.1:8787/#live"},
	} {
		if got := liveURL(c.listen, c.camera); got != c.want {
			t.Errorf("liveURL(%q,%q) = %q want %q", c.listen, c.camera, got, c.want)
		}
	}
}

func TestBrowserCommandPerOS(t *testing.T) {
	const u = "http://127.0.0.1:8787/#live"
	for goos, want := range map[string]string{
		"windows": "rundll32 url.dll,FileProtocolHandler " + u,
		"darwin":  "open " + u,
		"linux":   "xdg-open " + u,
	} {
		cmd := browserCommand(goos, u)
		if got := strings.Join(append([]string{cmd.Args[0]}, cmd.Args[1:]...), " "); got != want {
			t.Errorf("%s: %q want %q", goos, got, want)
		}
	}
}

func TestOpenPrintsTheAddressAndNeverOpensWithPrint(t *testing.T) {
	r := newRig(t)
	out, errw, code := r.run("open", "--print")
	if code != 0 || strings.TrimSpace(out) != "http://127.0.0.1:18821/#live" {
		t.Fatalf("%d %q %q", code, out, errw)
	}
	out, _, code = r.run("open", "front", "--print")
	if code != 0 || strings.TrimSpace(out) != "http://127.0.0.1:18821/#live/Front" {
		t.Errorf("the camera name must be the canonical one: %d %q", code, out)
	}
	if _, errw, code := r.run("open", "Nope", "--print"); code != 1 || !strings.Contains(errw, "Nope") {
		t.Errorf("unknown camera: %d %q", code, errw)
	}
	if _, _, code := r.run("open", "a", "b", "--print"); code != 2 {
		t.Errorf("two cameras: %d", code)
	}
	if len(r.opened) != 0 {
		t.Errorf("--print opened %v", r.opened)
	}
}

func TestOpenNeedsTheWebAppAnswering(t *testing.T) {
	r := newRig(t)
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	r.listenAt(addr)
	_, errw, code := r.run("open")
	if code != 1 || !strings.Contains(errw, "not answering on "+addr) || !strings.Contains(errw, "holocron up") {
		t.Errorf("not answering: %d %q", code, errw)
	}
	if len(r.opened) != 0 {
		t.Error("a browser was opened for a web app that is not there")
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	r.listenAt(l.Addr().String())
	out, errw, code := r.run("open", "Back")
	want := "http://" + l.Addr().String() + "/#live/Back"
	if code != 0 || len(r.opened) != 1 || r.opened[0] != want || !strings.Contains(out, want) {
		t.Errorf("answering: %d %q %q opened %v", code, out, errw, r.opened)
	}
}
