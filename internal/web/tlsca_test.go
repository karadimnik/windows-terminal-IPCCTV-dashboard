package web

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testManager(t *testing.T) *certManager {
	t.Helper()
	m := newCertManager(filepath.Join(t.TempDir(), "tls"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m
}

func lanNames(t *testing.T, extra ...string) certNames {
	t.Helper()
	names, err := buildCertNames(net.ParseIP("192.168.1.50"), extra)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func verifyFor(t *testing.T, leaf, ca *x509.Certificate, host string, at time.Time) error {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func TestCAProperties(t *testing.T) {
	m := testManager(t)
	ca, key, err := m.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.BasicConstraintsValid || ca.MaxPathLen != 0 || !ca.MaxPathLenZero {
		t.Errorf("CA constraints: IsCA=%v MaxPathLen=%d zero=%v", ca.IsCA, ca.MaxPathLen, ca.MaxPathLenZero)
	}
	if ca.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Errorf("CA key usage %v", ca.KeyUsage)
	}
	if pub, ok := ca.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve.Params().Name != "P-256" {
		t.Errorf("CA key is not ECDSA P-256")
	}
	if !strings.HasPrefix(ca.Subject.CommonName, "IPCCTV local CA ") || len(ca.Subject.CommonName) < len("IPCCTV local CA ")+8 {
		t.Errorf("CA name %q", ca.Subject.CommonName)
	}
	if life := ca.NotAfter.Sub(ca.NotBefore); life < 9*365*24*time.Hour || life > 11*365*24*time.Hour {
		t.Errorf("CA life %v", life)
	}
	if !ca.PermittedDNSDomainsCritical || len(ca.PermittedIPRanges) != 4 || len(ca.PermittedDNSDomains) == 0 {
		t.Errorf("CA name constraints missing or not critical: %v %v", ca.PermittedIPRanges, ca.PermittedDNSDomains)
	}
	if key == nil {
		t.Error("no key")
	}
	// Two CAs differ in name.
	other, _, _ := testManager(t).EnsureCA()
	if other.Subject.CommonName == ca.Subject.CommonName {
		t.Error("two CAs share a name")
	}
}

func TestLeafChainAndNames(t *testing.T) {
	m := testManager(t)
	names := lanNames(t, "10.0.0.7", "nvr.lan")
	leaf, note, err := m.EnsureLeaf(names)
	if err != nil || note != "created" {
		t.Fatalf("EnsureLeaf: %v %q", err, note)
	}
	ca, _, _ := m.EnsureCA()
	for _, host := range []string{"192.168.1.50", "127.0.0.1", "localhost", "10.0.0.7", "nvr.lan"} {
		if err := verifyFor(t, leaf.Leaf, ca, host, time.Now()); err != nil {
			t.Errorf("%s refused: %v", host, err)
		}
	}
	for _, host := range []string{"192.168.1.51", "evil.example", "example.com", "10.0.0.8"} {
		if err := verifyFor(t, leaf.Leaf, ca, host, time.Now()); err == nil {
			t.Errorf("%s accepted by a leaf that does not name it", host)
		}
	}
	if leaf.Leaf.IsCA || leaf.Leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Error("leaf is a CA")
	}
	if len(leaf.Leaf.ExtKeyUsage) != 1 || leaf.Leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("leaf usages %v", leaf.Leaf.ExtKeyUsage)
	}
	if life := leaf.Leaf.NotAfter.Sub(leaf.Leaf.NotBefore); life > 397*24*time.Hour {
		t.Errorf("leaf valid %v, over 397 days", life)
	}
	if pub, ok := leaf.Leaf.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve.Params().Name != "P-256" {
		t.Error("leaf key is not ECDSA P-256")
	}
}

func TestNameConstraintsRefusePublicNames(t *testing.T) {
	m := testManager(t)
	ca, key, err := m.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		names certNames
		allow bool
		host  string
	}{
		{"private ip", certNames{IPs: []net.IP{net.ParseIP("192.168.1.50").To4()}}, true, "192.168.1.50"},
		{"172.16 range", certNames{IPs: []net.IP{net.ParseIP("172.31.0.1").To4()}}, true, "172.31.0.1"},
		{"loopback", certNames{IPs: []net.IP{net.ParseIP("127.0.0.1").To4()}}, true, "127.0.0.1"},
		{"lan name", certNames{DNS: []string{"nvr.lan"}}, true, "nvr.lan"},
		{"public name", certNames{DNS: []string{"example.com"}}, false, "example.com"},
		{"public name under a permitted-looking label", certNames{DNS: []string{"lan.example.com"}}, false, "lan.example.com"},
		{"public ip", certNames{IPs: []net.IP{net.ParseIP("8.8.8.8").To4()}}, false, "8.8.8.8"},
		{"just outside 172.16/12", certNames{IPs: []net.IP{net.ParseIP("172.32.0.1").To4()}}, false, "172.32.0.1"},
		{"ipv6", certNames{IPs: []net.IP{net.ParseIP("2001:db8::1")}}, false, "2001:db8::1"},
		{"a public name next to a private one", certNames{IPs: []net.IP{net.ParseIP("192.168.1.50").To4()}, DNS: []string{"example.com"}}, false, "192.168.1.50"},
	}
	for _, c := range cases {
		leaf, err := m.issueLeaf(ca, key, c.names)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		err = verifyFor(t, leaf.Leaf, ca, c.host, time.Now())
		if c.allow && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.allow && err == nil {
			t.Errorf("%s: the CA's own chain accepted a name outside its constraints", c.name)
		}
	}
}

func TestLeafRenewal(t *testing.T) {
	type step struct {
		name  string
		setup func(m *certManager)
		names certNames
		want  string
	}
	base := lanNames(t)
	steps := []step{
		{"absent -> created", func(m *certManager) {}, base, "created"},
		{"same names -> reused", func(m *certManager) {}, base, "reused"},
		{"a name added -> renewed", func(m *certManager) {}, lanNames(t, "nvr.lan"), "renewed (the names changed)"},
		{"the LAN address changed -> renewed", func(m *certManager) {}, func() certNames { n, _ := buildCertNames(net.ParseIP("192.168.1.77"), []string{"nvr.lan"}); return n }(), "renewed (the names changed)"},
		{"unchanged again -> reused", func(m *certManager) {}, func() certNames { n, _ := buildCertNames(net.ParseIP("192.168.1.77"), []string{"nvr.lan"}); return n }(), "reused"},
		{"within 30 days of expiry -> renewed", func(m *certManager) { clockAtLeafEnd(t, m, -10*24*time.Hour) }, func() certNames { n, _ := buildCertNames(net.ParseIP("192.168.1.77"), []string{"nvr.lan"}); return n }(), "renewed (near expiry)"},
		{"expired -> renewed", func(m *certManager) { clockAtLeafEnd(t, m, 24*time.Hour) }, func() certNames { n, _ := buildCertNames(net.ParseIP("192.168.1.77"), []string{"nvr.lan"}); return n }(), "renewed (expired)"},
		{"leaf file corrupted -> renewed", func(m *certManager) {
			m.now = time.Now
			_ = os.WriteFile(m.path(leafCertFile), []byte("garbage"), 0o644)
		}, func() certNames { n, _ := buildCertNames(net.ParseIP("192.168.1.77"), []string{"nvr.lan"}); return n }(), "renewed (unreadable)"},
		{"leaf key deleted -> renewed", func(m *certManager) { _ = os.Remove(m.path(leafKeyFile)) }, func() certNames { n, _ := buildCertNames(net.ParseIP("192.168.1.77"), []string{"nvr.lan"}); return n }(), "renewed (its key is missing)"},
	}
	m := testManager(t)
	ca1, _, err := m.EnsureCA()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		s.setup(m)
		_, note, err := m.EnsureLeaf(s.names)
		if err != nil || note != s.want {
			t.Errorf("%s: got %q (%v), want %q", s.name, note, err, s.want)
		}
	}
	ca2, _, _ := m.EnsureCA()
	if fingerprint(ca1) != fingerprint(ca2) {
		t.Error("the CA changed while only the leaf was renewed")
	}
	// A leaf from another CA is replaced.
	if err := os.Remove(m.path(caCertFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.path(caKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, note, err := m.EnsureLeaf(steps[len(steps)-1].names); err != nil || note != "renewed (not signed by this CA)" {
		t.Errorf("after a new CA: %q %v", note, err)
	}
}

// clockAtLeafEnd sets the manager's clock relative to the stored leaf's expiry.
func clockAtLeafEnd(t *testing.T, m *certManager, offset time.Duration) {
	t.Helper()
	data, err := os.ReadFile(m.path(leafCertFile))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	at := leaf.NotAfter.Add(offset)
	m.now = func() time.Time { return at }
}

func TestReuseDoesNotRewriteFiles(t *testing.T) {
	m := testManager(t)
	names := lanNames(t)
	if _, _, err := m.EnsureLeaf(names); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(m.path(leafCertFile))
	info1, _ := os.Stat(m.path(leafKeyFile))
	time.Sleep(20 * time.Millisecond)
	if _, note, _ := m.EnsureLeaf(names); note != "reused" {
		t.Fatalf("note %q", note)
	}
	after, _ := os.ReadFile(m.path(leafCertFile))
	info2, _ := os.Stat(m.path(leafKeyFile))
	if string(before) != string(after) || !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("a reused leaf was rewritten")
	}
}

func TestExpiredCAIsRefusedWithGuidance(t *testing.T) {
	m := testManager(t)
	if _, _, err := m.EnsureCA(); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Now().Add(caValidity + 48*time.Hour) }
	if _, _, err := m.EnsureCA(); err == nil || !strings.Contains(err.Error(), "delete the TLS folder") {
		t.Errorf("expired CA: %v", err)
	}
}

func TestCADownloadHoldsNoKey(t *testing.T) {
	m := testManager(t)
	if _, _, err := m.EnsureLeaf(lanNames(t)); err != nil {
		t.Fatal(err)
	}
	pemData, err := m.CACertPEM()
	if err != nil {
		t.Fatal(err)
	}
	if containsKeyMaterial(pemData) {
		t.Error("the CA download holds key material")
	}
	// Positive control: the detector sees a key.
	keyBytes, _ := os.ReadFile(m.path(caKeyFile))
	leafKey, _ := os.ReadFile(m.path(leafKeyFile))
	if !containsKeyMaterial(keyBytes) || !containsKeyMaterial(leafKey) || !containsKeyMaterial(append(append([]byte{}, pemData...), keyBytes...)) {
		t.Error("the key-material detector did not light up on a real key")
	}
	ca, _, _ := m.EnsureCA()
	info, _ := m.CAInfo()
	if info.Fingerprint != fingerprint(ca) || strings.Count(info.Fingerprint, ":") != 31 {
		t.Errorf("fingerprint %q", info.Fingerprint)
	}
}

func TestKeyFilesArePrivateAndWritesAtomic(t *testing.T) {
	m := testManager(t)
	if _, _, err := m.EnsureLeaf(lanNames(t)); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{caKeyFile, leafKeyFile} {
			info, err := os.Stat(m.path(name))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v (%v), want 0600", name, info.Mode().Perm(), err)
			}
		}
		if info, _ := os.Stat(m.dir); info.Mode().Perm() != 0o700 {
			t.Errorf("tls folder mode %v", info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 4 {
		t.Errorf("tls folder holds %d files, want 4", len(entries))
	}
}

func TestParseTLSListen(t *testing.T) {
	good := []string{"192.168.1.50:8443", "10.1.2.3:443", "172.16.0.9:9443"}
	bad := []string{"", "0.0.0.0:8443", ":8443", "localhost:8443", "[::1]:8443", "[fd00::1]:8443", "127.0.0.1:8443", "8.8.8.8:8443",
		"192.168.1.50", "192.168.1.50:0", "192.168.1.50:99999", "192.168.1.50:abc", "172.32.0.1:8443", "169.254.1.1:8443", "nvr.lan:8443", "192.168.1.50:8443 "}
	for _, g := range good {
		if _, _, err := parseTLSListen(g); err != nil {
			t.Errorf("%q refused: %v", g, err)
		}
	}
	for _, b := range bad {
		if _, _, err := parseTLSListen(b); err == nil {
			t.Errorf("%q accepted", b)
		}
	}
}

func TestBuildCertNames(t *testing.T) {
	ip := net.ParseIP("192.168.1.50")
	bad := [][]string{{"example.com"}, {"8.8.8.8"}, {"2001:db8::1"}, {"nvr"}, {"-x.lan"}, {".lan"}, {"a.b.c.d.e"}, {"1.lan", "2.lan", "3.lan", "4.lan", "5.lan"}}
	for _, extra := range bad {
		if _, err := buildCertNames(ip, extra); err == nil {
			t.Errorf("%v accepted", extra)
		}
	}
	n, err := buildCertNames(ip, []string{"NVR.LAN", "nvr.lan", "192.168.1.50", "10.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(n.IPs) != 3 || len(n.DNS) != 2 {
		t.Errorf("names not deduplicated: %v %v", n.IPs, n.DNS)
	}
	if a, b := lanNames(t, "nvr.lan", "10.0.0.2"), lanNames(t, "10.0.0.2", "nvr.lan"); a.key() != b.key() {
		t.Error("the name key depends on order")
	}
}

func TestServerConfigHandshake(t *testing.T) {
	m := testManager(t)
	if _, err := m.Prepare(lanNames(t)); err != nil {
		t.Fatal(err)
	}
	conf := serverTLSConfig(m)
	if conf.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion %x", conf.MinVersion)
	}
	ca, _, _ := m.EnsureCA()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	client, server := net.Pipe()
	go func() {
		s := tls.Server(server, conf)
		_ = s.Handshake()
		s.Close()
	}()
	c := tls.Client(client, &tls.Config{RootCAs: roots, ServerName: "192.168.1.50"})
	if err := c.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if v := c.ConnectionState().Version; v != tls.VersionTLS13 {
		t.Errorf("negotiated %x, want TLS 1.3 with a modern client", v)
	}
	c.Close()
	// TLS 1.1 is refused.
	client2, server2 := net.Pipe()
	go func() { s := tls.Server(server2, conf); _ = s.Handshake(); s.Close() }()
	client2.SetDeadline(time.Now().Add(2 * time.Second))
	old := tls.Client(client2, &tls.Config{RootCAs: roots, ServerName: "192.168.1.50", MaxVersion: tls.VersionTLS11, MinVersion: tls.VersionTLS10})
	if err := old.Handshake(); err == nil {
		t.Error("a TLS 1.1 client connected")
	}
	// A wrong name is refused by the client's verification against this CA.
	client3, server3 := net.Pipe()
	go func() { s := tls.Server(server3, conf); _ = s.Handshake(); s.Close() }()
	wrong := tls.Client(client3, &tls.Config{RootCAs: roots, ServerName: "192.168.1.99"})
	if err := wrong.Handshake(); err == nil {
		t.Error("a wrong host name was accepted")
	}
}
