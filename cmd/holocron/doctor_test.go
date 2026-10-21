package main

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
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
