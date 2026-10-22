package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holocron/internal/osx"
)

// served is what the web role's HTTPS listener presents: a leaf valid until leafNotAfter and the
// local CA that signed it; it returns the CA's DER.
func served(t *testing.T, leafNotAfter time.Time) (tls.Certificate, []byte) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Holocron test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(3650 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "192.168.1.50"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: leafNotAfter,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}, caDER
}

// webRunning lists this test process as the host's web child, alive by the inspector.
func (r *rig) webRunning() {
	r.t.Helper()
	insp := &osx.FakeInspector{Procs: map[int]osx.ProcInfo{4545: {Start: "s", Image: "/nonexistent/holocron"}}}
	r.insp = insp
	if err := osx.NewRegistry(filepath.Join(r.root, "_state", "webchild.json")).Add(osx.ChildEntry{Pid: 4545, Start: "s", Image: "/nonexistent/holocron", Role: "web"}); err != nil {
		r.t.Fatal(err)
	}
}

func TestDoctorWebHTTPAndProcess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/status" || req.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		fmt.Fprint(w, `{"schema":1,"controller":"api"}`)
	}))
	t.Cleanup(srv.Close)
	r := newRig(t)
	r.listenOn(strings.TrimPrefix(srv.URL, "http://"), "")
	r.running()
	r.doctorTools(func(d *doctorDeps) { d.http = srv.Client() })
	text := textOf(t, mustRun(r, "--json", "doctor", "--group", "web"))
	for _, w := range []string{
		"web.process fail web role       not running",
		"web.http ok   web http       GET http://" + strings.TrimPrefix(srv.URL, "http://") + "/api/status answered 200; control path api",
		"web.tls info https          the HTTPS listener is off",
		"web.errors ok   web errors     no ERROR record",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("no %q in\n%s", w, text)
		}
	}
	r.webRunning()
	if text := textOf(t, mustRun(r, "--json", "doctor", "--group", "web")); !strings.Contains(text, "web.process ok   web role       running, pid 4545") {
		t.Errorf("%s", text)
	}
}

func mustRun(r *rig, args ...string) string {
	out, _, _ := r.run(args...)
	return out
}

func TestDoctorWebHTTPS(t *testing.T) {
	for _, c := range []struct {
		name     string
		leafDays int
		user     bool
		want     []string
	}{
		{"healthy", 200, true, []string{"web.tls ok   https          127.0.0.1:", "valid 199 more day(s)", "view-only", "web.tls ok   local CA       SHA-256 ", "web.users ok   users          1 login user(s)"}},
		{"near expiry and no user", 10, false, []string{"web.tls warn https", "expires in 9 day(s)", "web.users fail users          no login user"}},
		{"expired", -1, true, []string{"web.tls fail https", "EXPIRED"}},
	} {
		leaf, caDER := served(t, time.Now().Add(time.Duration(c.leafDays)*24*time.Hour))
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{leaf}})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { conn.(*tls.Conn).Handshake(); conn.Close() }()
			}
		}()
		t.Cleanup(func() { ln.Close() })
		r := newRig(t)
		tlsDir := filepath.Join(r.base(), "tls")
		os.MkdirAll(tlsDir, 0o700)
		os.WriteFile(filepath.Join(tlsDir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644)
		r.listenOn("127.0.0.1:18821", `, "tlsListen": "`+ln.Addr().String()+`", "tlsDir": "`+filepath.ToSlash(tlsDir)+`"`)
		if c.user {
			r.stdin = goodPassword + "\n"
			if _, errw, code := r.run("user", "add", "alice"); code != 0 {
				t.Fatal(errw)
			}
		}
		r.running()
		addr := ln.Addr().String()
		r.doctorTools(func(d *doctorDeps) {
			d.dial = func(ctx context.Context, n, a string) (net.Conn, error) {
				if a != addr {
					return nil, errRefusedInTest
				}
				return (&net.Dialer{}).DialContext(ctx, n, a)
			}
		})
		text := textOf(t, mustRun(r, "--json", "doctor", "--group", "web"))
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, text)
			}
		}
		if c.name == "healthy" && !strings.Contains(text, fingerprintOf(caDER)) {
			t.Errorf("fingerprint missing:\n%s", text)
		}
	}
}

func TestDoctorWebErrorsInTheLastHour(t *testing.T) {
	r := newRig(t)
	r.running()
	os.MkdirAll(filepath.Join(r.root, "_logs"), 0o755)
	stamp := time.Now().Add(-10 * time.Minute).Format("2006-01-02T15:04:05.000000Z07:00")
	old := time.Now().Add(-2 * time.Hour).Format("2006-01-02T15:04:05.000000Z07:00")
	os.WriteFile(filepath.Join(r.root, "_logs", "holocron.log"), []byte(old+" ERROR [web] too old\n"+stamp+" ERROR [web] listen tcp: bind refused\n"+stamp+" ERROR [web] second\n"+stamp+" ERROR [relay] not web\n"), 0o644)
	r.doctorTools(nil)
	out := mustRun(r, "--json", "doctor", "--group", "web")
	if text := textOf(t, out); !strings.Contains(text, "web.errors warn web errors     2 ERROR record(s) of the web role in the last hour, the first: listen tcp: bind refused") {
		t.Errorf("%s", text)
	}
	if !strings.Contains(out, "holocron logs --component web --level error --since 1h") {
		t.Errorf("fix:\n%s", out)
	}
}
