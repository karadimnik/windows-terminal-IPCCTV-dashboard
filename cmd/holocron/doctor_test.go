package main

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"holocron/internal/ffmpeg"
)

// setConfig adds top-level keys (a JSON fragment such as `"A": 1,`) to the rig's configuration.
func (r *rig) setConfig(fragment string) {
	r.t.Helper()
	b, _ := os.ReadFile(r.cfg)
	if err := os.WriteFile(r.cfg, []byte(strings.Replace(string(b), `"web"`, fragment+` "web"`, 1)), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

const externalRelay = `"RestreamerEnabled": false, "RelayExternal": true, "RelayBase": "rtsp://127.0.0.1:18890",`

// With RelayExternal the relay is not "off": the cameras read it from somewhere else, and both
// commands say so with the configured RelayBase.
func TestStatusAndDoctorNameAnExternalRelay(t *testing.T) {
	const want = "relay: external (RelayExternal: the cameras read rtsp://127.0.0.1:18890/<path> from a relay this system does not run)"
	r := newRig(t)
	r.setConfig(externalRelay)
	r.running()
	out, _, _ := r.run("status")
	if !strings.Contains(out, want) || strings.Contains(out, "relay: off") {
		t.Errorf("status:\n%s", out)
	}
	out, _, _ = r.run("doctor")
	if !strings.Contains(out, want) || strings.Contains(out, "the relay is off") {
		t.Errorf("doctor:\n%s", out)
	}
}

// Without RelayExternal the old texts stay (status prints the relay of its document).
func TestStatusAndDoctorStillSayRelayOff(t *testing.T) {
	r := newRig(t)
	r.setConfig(`"RestreamerEnabled": false,`)
	r.running()
	if out, _, _ := r.run("status"); !strings.Contains(out, "relay: alive=true") || strings.Contains(out, "external") {
		t.Errorf("status:\n%s", out)
	}
	if out, _, _ := r.run("doctor"); !strings.Contains(out, "the relay is off (RestreamerEnabled false)") {
		t.Errorf("doctor:\n%s", out)
	}
}

// holdPort binds a free port of the test range and keeps it until the test ends.
func holdPort(t *testing.T) (net.Listener, string) {
	t.Helper()
	for p := 18880; p <= 18899; p++ {
		addr := "127.0.0.1:" + strconv.Itoa(p)
		if l, err := net.Listen("tcp", addr); err == nil {
			t.Cleanup(func() { l.Close() })
			return l, addr
		}
	}
	t.Skip("no free port in 18880-18899")
	return nil, ""
}

func (r *rig) listenOn(addr, extra string) {
	r.t.Helper()
	b, _ := os.ReadFile(r.cfg)
	s := strings.Replace(string(b), `"listen": "127.0.0.1:18821"`, `"listen": "`+addr+`"`+extra, 1)
	if err := os.WriteFile(r.cfg, []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func warnLine(out, what string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "warn") && strings.Contains(l, what) {
			return l
		}
	}
	return ""
}

func TestDoctorWarnsWhenTheWebPortIsHeld(t *testing.T) {
	_, addr := holdPort(t)
	_, port, _ := net.SplitHostPort(addr)
	r := newRig(t)
	r.listenOn(addr, "")
	out, _, _ := r.run("doctor")
	line := warnLine(out, "web listen") // a warn line, so not a failure
	for _, want := range []string{"port " + port, "another program", "camui.exe", "crash-loop", "alert 104"} {
		if !strings.Contains(line, want) {
			t.Errorf("doctor warning lacks %q:\n%s", want, out)
		}
	}
}

// The HTTPS listener is checked too: here it is the held one and the plain listener is free.
func TestDoctorWarnsWhenTheTLSPortIsHeld(t *testing.T) {
	_, held := holdPort(t)
	free, freeAddr := holdPort(t)
	free.Close()
	r := newRig(t)
	r.listenOn(freeAddr, `, "tlsListen": "`+held+`"`)
	out, _, _ := r.run("doctor")
	if warnLine(out, "web listen") != "" || !strings.Contains(warnLine(out, "web tlsListen"), "alert 104") {
		t.Errorf("tlsListen held:\n%s", out)
	}
}

func TestDoctorSkipsThePortCheckWhenTheWebAppIsOff(t *testing.T) {
	_, addr := holdPort(t)
	r := newRig(t)
	r.listenOn(addr, `, "enabled": false`)
	if out, _, _ := r.run("doctor"); strings.Contains(out, "web listen") {
		t.Errorf("web.enabled false still checked the port:\n%s", out)
	}
}

func TestDoctorSaysFreeWebPortIsFree(t *testing.T) {
	held, addr := holdPort(t)
	held.Close()
	r := newRig(t)
	r.listenOn(addr, "")
	out, _, _ := r.run("doctor")
	if !strings.Contains(out, "ok   web listen") || warnLine(out, "web listen") != "" {
		t.Errorf("free port:\n%s", out)
	}
}

// A development build is accepted by the product (its number is unknown, 5.0 or later is assumed):
// info, no fix, not a warning. A release older than 5.0 and an unreadable version stay failures.
func TestDoctorFfmpegVersionLevels(t *testing.T) {
	for _, c := range []struct {
		name, version, level string
		fix                  bool
	}{
		{"development build", "N-127579-g5cb9fcd211-20261016", lvInfo, false},
		{"dated development build", "2026-07-06-git-c6498178bb-essentials_build", lvInfo, false},
		{"release", "7.1.1-essentials_build", lvOK, false},
		{"too old", "4.2.7", lvFail, true},
		{"old development build", "git-2020-05-01-abcdef0", lvFail, true},
		{"unreadable", "banana", lvFail, true},
	} {
		r := newRig(t)
		r.doctorTools(func(d *doctorDeps) {
			d.ffmpeg = func(string) (ffmpeg.Version, error) {
				return ffmpeg.ParseVersion("ffmpeg version " + c.version + " Copyright")
			}
		})
		var rep doctorReport
		out, _, _ := r.run("doctor", "--json", "--group", "setup")
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		var got *check
		for i, ck := range rep.Checks {
			if ck.ID == "setup.ffmpeg" {
				got = &rep.Checks[i]
			}
		}
		if got == nil || got.Level != c.level || (got.Fix != "") != c.fix {
			t.Errorf("%s: %+v, want level %s and fix %v", c.name, got, c.level, c.fix)
			continue
		}
		if c.level == lvInfo && !strings.HasSuffix(got.Detail, "development build; its release number is unknown, assumed 5.0 or later") {
			t.Errorf("%s: detail %q", c.name, got.Detail)
		}
	}
}
