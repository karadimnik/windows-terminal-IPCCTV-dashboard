package main

import (
	"net"
	"os"
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

var _ = net.Listen
