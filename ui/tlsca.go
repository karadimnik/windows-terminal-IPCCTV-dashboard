package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// The local certificate authority and the server certificate camui serves on
// the LAN. See tasks/portable-nvr/live-2b-design.md.
//
// The CA is generated once and installed by the Operator on each device. It
// carries X.509 name constraints, so even a leaked CA key can only vouch for
// private addresses and private names, never for a public site. The server
// certificate (the "leaf") is cheap to replace: it is regenerated when absent,
// near expiry, or when the set of names it must cover changes, and the devices
// keep trusting the CA throughout.
// ---------------------------------------------------------------------------

const (
	caValidity    = 10 * 365 * 24 * time.Hour
	leafValidity  = 390 * 24 * time.Hour // Apple's limit is 398 days
	leafRenewWith = 30 * 24 * time.Hour  // renew when this close to expiry
	maxTLSHosts   = 4

	caCertFile   = "ca.crt"
	caKeyFile    = "ca.key"
	leafCertFile = "leaf.crt"
	leafKeyFile  = "leaf.key"
)

// permittedDNSSuffixes are the only DNS names the CA can vouch for.
var permittedDNSSuffixes = []string{"localhost", ".lan", ".local", ".home.arpa", ".internal"}

// permittedIPNets are the only addresses the CA can vouch for: RFC 1918, plus
// loopback for the 127.0.0.1 name every certificate carries.
var permittedIPNets = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8"} {
		_, n, _ := net.ParseCIDR(cidr)
		nets = append(nets, n)
	}
	return nets
}()

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// isPrivateIPv4 reports whether ip is an IPv4 address in RFC 1918 space.
func isPrivateIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 10 || (v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) || (v4[0] == 192 && v4[1] == 168)
}

// parseTLSListen validates TLSListen: a private IPv4 literal and a port.
// 0.0.0.0, hostnames, IPv6, loopback and public addresses are refused.
func parseTLSListen(listen string) (net.IP, string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, "", fmt.Errorf("TLSListen %q is not host:port such as 192.168.1.50:8443", listen)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || strings.Contains(host, ":") || !isPrivateIPv4(ip) {
		return nil, "", fmt.Errorf("TLSListen host %q must be a private IPv4 address (10.x, 172.16-31.x, 192.168.x) of this PC: not 0.0.0.0, a name, IPv6, loopback or a public address", host)
	}
	if n := portNumber(port); n < 1 || n > 65535 {
		return nil, "", fmt.Errorf("TLSListen port %q must be 1 to 65535", port)
	}
	return ip.To4(), port, nil
}

func portNumber(port string) int {
	n := 0
	for _, c := range port {
		if c < '0' || c > '9' || n > 100000 {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	if port == "" {
		return -1
	}
	return n
}

func dnsNameAllowed(name string) bool {
	if name == "localhost" {
		return true
	}
	for _, suffix := range permittedDNSSuffixes {
		if strings.HasPrefix(suffix, ".") && strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			for _, label := range strings.Split(strings.TrimSuffix(name, suffix), ".") {
				if !dnsLabel.MatchString(label) {
					return false
				}
			}
			return true
		}
	}
	return false
}

// certNames is the validated set of names a leaf covers.
type certNames struct {
	IPs []net.IP
	DNS []string
}

// buildCertNames returns the names for a TLSListen address and the extra
// TLSHosts: the listen IP, 127.0.0.1 and localhost always, then the extras
// (private IPv4 literals or names under a permitted private suffix).
func buildCertNames(listenIP net.IP, extra []string) (certNames, error) {
	if len(extra) > maxTLSHosts {
		return certNames{}, fmt.Errorf("TLSHosts may list at most %d names", maxTLSHosts)
	}
	names := certNames{IPs: []net.IP{listenIP.To4(), net.IPv4(127, 0, 0, 1).To4()}, DNS: []string{"localhost"}}
	for _, raw := range extra {
		host := strings.ToLower(strings.TrimSpace(raw))
		if ip := net.ParseIP(host); ip != nil {
			if ip.To4() == nil || strings.Contains(host, ":") || !isPrivateIPv4(ip) {
				return certNames{}, fmt.Errorf("TLSHosts entry %q must be a private IPv4 literal or a name ending in .lan, .local, .home.arpa or .internal", raw)
			}
			if !slices.ContainsFunc(names.IPs, ip.Equal) {
				names.IPs = append(names.IPs, ip.To4())
			}
			continue
		}
		if !dnsNameAllowed(host) {
			return certNames{}, fmt.Errorf("TLSHosts entry %q must be a private IPv4 literal or a name ending in .lan, .local, .home.arpa or .internal", raw)
		}
		if !slices.Contains(names.DNS, host) {
			names.DNS = append(names.DNS, host)
		}
	}
	return names, nil
}

// key is the canonical, order-independent form used to compare name sets.
func (n certNames) key() string {
	var parts []string
	for _, ip := range n.IPs {
		parts = append(parts, "ip:"+ip.String())
	}
	for _, d := range n.DNS {
		parts = append(parts, "dns:"+d)
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

func certKey(cert *x509.Certificate) string {
	return certNames{IPs: cert.IPAddresses, DNS: cert.DNSNames}.key()
}

// fingerprint is the SHA-256 of the DER certificate in the colon form devices display.
func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(parts, ":")
}

// spkiHash is the base64 SHA-256 of the SubjectPublicKeyInfo, the form Chromium's
// --ignore-certificate-errors-spki-list takes.
func spkiHash(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// writeFileAtomic writes a public file through a temp file and a rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(name)
		return errors.Join(werr, cerr)
	}
	if err := os.Chmod(name, perm); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// writePrivateFile writes a secret: the temp file is restricted to the owner
// BEFORE the secret goes into it, then renamed into place.
func writePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := restrictToOwner(name, false); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(name)
		return errors.Join(werr, cerr)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// certManager owns the TLS directory.
type certManager struct {
	dir string
	now func() time.Time
	log *slog.Logger

	mu       sync.Mutex
	leaf     *tls.Certificate
	leafNote string
	names    certNames
	checked  time.Time
}

func newCertManager(dir string, log *slog.Logger) *certManager {
	return &certManager{dir: dir, now: time.Now, log: log}
}

func (m *certManager) path(name string) string { return filepath.Join(m.dir, name) }

func (m *certManager) ensureDir() error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	return restrictToOwner(m.dir, true)
}

// loadCA reads the CA certificate and key; os.ErrNotExist when there is none.
func (m *certManager) loadCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(m.path(caCertFile))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(m.path(caKeyFile))
	if err != nil {
		return nil, nil, err
	}
	return parseCertAndKey(certPEM, keyPEM)
}

func parseCertAndKey(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, errors.New("not a PEM key")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, errors.New("the key does not belong to the certificate")
	}
	return cert, key, nil
}

// EnsureCA returns the CA, creating it (and the directory) when absent.
func (m *certManager) EnsureCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if err := m.ensureDir(); err != nil {
		return nil, nil, fmt.Errorf("prepare the TLS folder %s: %w", m.dir, err)
	}
	cert, key, err := m.loadCA()
	if err == nil {
		if m.now().After(cert.NotAfter) {
			return nil, nil, fmt.Errorf("the local CA expired on %s: delete the TLS folder %s, restart, and reinstall the new CA on each device", cert.NotAfter.Format("2006-01-02"), m.dir)
		}
		return cert, key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("the local CA in %s is unreadable (%v): delete the folder to start over", m.dir, err)
	}
	return m.createCA()
}

func (m *certManager) createCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, nil, err
	}
	now := m.now()
	template := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "IPCCTV local CA " + hex.EncodeToString(suffix), Organization: []string{"IPCCTV"}},
		NotBefore:                   now.Add(-5 * time.Minute),
		NotAfter:                    now.Add(caValidity),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedIPRanges:           permittedIPNets,
		PermittedDNSDomains:         permittedDNSSuffixes,
		PermittedDNSDomainsCritical: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := pemKey(key)
	if err != nil {
		return nil, nil, err
	}
	// Key first, certificate second: a crash between them leaves an unusable key, never a CA certificate without its key.
	if err := writePrivateFile(m.path(caKeyFile), keyPEM); err != nil {
		return nil, nil, fmt.Errorf("write the CA key: %w", err)
	}
	if err := writeFileAtomic(m.path(caCertFile), pemCert(der), 0o644); err != nil {
		return nil, nil, fmt.Errorf("write the CA certificate: %w", err)
	}
	return cert, key, nil
}

// issueLeaf signs a server certificate for names. It does not validate the
// names: tests use it to prove the CA's own constraints refuse bad ones.
func (m *certManager) issueLeaf(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, names certNames) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := m.now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "IPCCTV camui", Organization: []string{"IPCCTV"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		IPAddresses:           names.IPs,
		DNSNames:              names.DNS,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyPEM, err := pemKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePrivateFile(m.path(leafKeyFile), keyPEM); err != nil {
		return nil, fmt.Errorf("write the server key: %w", err)
	}
	if err := writeFileAtomic(m.path(leafCertFile), pemCert(der), 0o644); err != nil {
		return nil, fmt.Errorf("write the server certificate: %w", err)
	}
	parsed, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der, caCert.Raw}, PrivateKey: key, Leaf: parsed}, nil
}

// leafReason says why the stored leaf cannot be used, or "" when it can.
func (m *certManager) leafReason(caCert *x509.Certificate, names certNames) (*tls.Certificate, string) {
	certPEM, err := os.ReadFile(m.path(leafCertFile))
	if err != nil {
		return nil, "absent"
	}
	keyPEM, err := os.ReadFile(m.path(leafKeyFile))
	if err != nil {
		return nil, "its key is missing"
	}
	leaf, key, err := parseCertAndKey(certPEM, keyPEM)
	if err != nil {
		return nil, "unreadable"
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	now := m.now()
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		if now.After(leaf.NotAfter) {
			return nil, "expired"
		}
		return nil, "not signed by this CA"
	}
	if leaf.NotAfter.Sub(now) < leafRenewWith {
		return nil, "near expiry"
	}
	if certKey(leaf) != names.key() {
		return nil, "the names changed"
	}
	return &tls.Certificate{Certificate: [][]byte{leaf.Raw, caCert.Raw}, PrivateKey: key, Leaf: leaf}, ""
}

// EnsureLeaf returns a usable server certificate for names, creating or
// replacing it when needed. The note says what happened: "created", "reused"
// or "renewed (<reason>)".
func (m *certManager) EnsureLeaf(names certNames) (*tls.Certificate, string, error) {
	caCert, caKey, err := m.EnsureCA()
	if err != nil {
		return nil, "", err
	}
	if cert, reason := m.leafReason(caCert, names); reason == "" {
		return cert, "reused", nil
	} else {
		issued, err := m.issueLeaf(caCert, caKey, names)
		if err != nil {
			return nil, "", err
		}
		if reason == "absent" {
			return issued, "created", nil
		}
		return issued, "renewed (" + reason + ")", nil
	}
}

// Prepare loads or creates the leaf for names and remembers it for GetCertificate.
func (m *certManager) Prepare(names certNames) (string, error) {
	cert, note, err := m.EnsureLeaf(names)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.leaf, m.leafNote, m.names, m.checked = cert, note, names, m.now()
	m.mu.Unlock()
	return note, nil
}

// GetCertificate serves the current leaf, re-checking it at most hourly so a
// long-running camui renews it before it expires.
func (m *certManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leaf == nil {
		return nil, errors.New("no certificate prepared")
	}
	if m.now().Sub(m.checked) > time.Hour {
		m.checked = m.now()
		if m.leaf.Leaf.NotAfter.Sub(m.now()) < leafRenewWith {
			if cert, note, err := m.EnsureLeaf(m.names); err == nil {
				m.leaf = cert
				m.log.Info("server certificate renewed", "result", note)
			} else {
				m.log.Error("server certificate renewal failed", "error", err)
			}
		}
	}
	return m.leaf, nil
}

// CACertPEM is the CA certificate and nothing else: the one file that may leave this machine.
func (m *certManager) CACertPEM() ([]byte, error) {
	cert, _, err := m.loadCA()
	if err != nil {
		return nil, err
	}
	return pemCert(cert.Raw), nil
}

// CAInfo describes the CA for display: never any key material.
type CAInfo struct {
	Path        string
	Fingerprint string
	Subject     string
	NotAfter    time.Time
	SPKI        string
}

func (m *certManager) CAInfo() (CAInfo, error) {
	cert, _, err := m.EnsureCA()
	if err != nil {
		return CAInfo{}, err
	}
	return CAInfo{Path: m.path(caCertFile), Fingerprint: fingerprint(cert), Subject: cert.Subject.CommonName, NotAfter: cert.NotAfter, SPKI: spkiHash(cert)}, nil
}

// containsKeyMaterial reports whether data holds anything but certificates.
func containsKeyMaterial(data []byte) bool {
	if bytes.Contains(data, []byte("PRIVATE KEY")) {
		return true
	}
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return len(bytes.TrimSpace(rest)) > 0
		}
		if block.Type != "CERTIFICATE" {
			return true
		}
	}
}
