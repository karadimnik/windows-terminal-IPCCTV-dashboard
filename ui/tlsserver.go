package main

import "crypto/tls"

// serverTLSConfig is the TLS side of the LAN listener: TLS 1.2 at the very
// least (Go negotiates 1.3 with any current client), the certificate from the
// manager, HTTP/2 and HTTP/1.1.
func serverTLSConfig(m *certManager) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}
