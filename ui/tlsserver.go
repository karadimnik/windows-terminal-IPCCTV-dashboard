package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// The HTTPS listener for the LAN. It is a second listener with its own handler
// chain; the loopback listener is untouched. Here the token is mandatory and
// outermost, so no request without it reaches any handler, static file included.
// ---------------------------------------------------------------------------

// serverTLSConfig is the TLS side: TLS 1.2 at the very least (Go negotiates 1.3
// with any current client), the certificate from the manager, HTTP/2 and HTTP/1.1.
func serverTLSConfig(m *certManager) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

// tlsGuardConfig is the Config the guard sees for the LAN listener: its own
// address and certificate names are the allowed Hosts, and the loopback
// listener's AllowedHosts are NOT carried over.
func tlsGuardConfig(cfg Config, names certNames) Config {
	guard := Config{Listen: cfg.TLSListen, Token: cfg.EffectiveTLSToken()}
	for _, ip := range names.IPs {
		guard.AllowedHosts = append(guard.AllowedHosts, ip.String())
	}
	guard.AllowedHosts = append(guard.AllowedHosts, names.DNS...)
	return guard
}

// newTLSHandler assembles the LAN listener's chain: token (strict, rate limited),
// response headers, request log, guard (https origins only), then the mux with the
// two TLS-only routes.
func newTLSHandler(cfg Config, log *slog.Logger, mgr *certManager, names certNames, limiter *failureLimiter) (http.Handler, error) {
	mux, err := buildMux(cfg, log)
	if err != nil {
		return nil, err
	}
	mux.HandleFunc("GET /api/tls/ca.crt", func(w http.ResponseWriter, r *http.Request) {
		data, err := mgr.CACertPEM()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the certificate authority is not available")
			return
		}
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="ipcctv-local-ca.crt"`)
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /api/tls/info", func(w http.ResponseWriter, r *http.Request) {
		info, err := mgr.CAInfo()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the certificate authority is not available")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"caFingerprintSha256": info.Fingerprint,
			"caSubject":           info.Subject,
			"caNotAfter":          info.NotAfter.Format("2006-01-02"),
			"download":            "api/tls/ca.crt",
		})
	})
	guarded := logRequests(log, newGuardScheme(tlsGuardConfig(cfg, names), log, "https")(mux))
	policy := tokenPolicy{Token: cfg.EffectiveTLSToken(), Strict: true, Limiter: limiter, Log: log}
	protected := requireTokenPolicy(policy, guarded)
	// Outermost, so the redirect after ?token= and every refusal carry it too.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		protected.ServeHTTP(w, r)
	}), nil
}

// startTLS prepares and binds the LAN listener. With TLSListen empty it returns
// nils. The returned func serves until the server is shut down.
func startTLS(cfg Config, log *slog.Logger) (*http.Server, func() error, error) {
	if cfg.TLSListen == "" {
		return nil, nil, nil
	}
	ip, _, err := parseTLSListen(cfg.TLSListen)
	if err != nil {
		return nil, nil, err
	}
	names, err := buildCertNames(ip, cfg.TLSHosts)
	if err != nil {
		return nil, nil, err
	}
	mgr := newCertManager(cfg.TLSDir, log)
	note, err := mgr.Prepare(names)
	if err != nil {
		return nil, nil, fmt.Errorf("TLS certificate: %w", err)
	}
	handler, err := newTLSHandler(cfg, log, mgr, names, newFailureLimiter(10, time.Minute, 1024))
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp", cfg.TLSListen)
	if err != nil {
		return nil, nil, fmt.Errorf("bind %s: %w", cfg.TLSListen, err)
	}
	server := &http.Server{
		Handler:           handler,
		TLSConfig:         serverTLSConfig(mgr),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	info, _ := mgr.CAInfo()
	log.Info("camui listening (HTTPS, token required)", "address", listener.Addr().String(), "certificate", note,
		"names", len(names.IPs)+len(names.DNS), "caFingerprintSha256", info.Fingerprint)
	return server, func() error { return server.ServeTLS(listener, "", "") }, nil
}

// runPrintCA implements -print-ca: it creates the CA if needed and prints where
// it is, its fingerprint and the install steps. It prints no key and no token.
func runPrintCA(cfg Config, out, errOut io.Writer) int {
	mgr := newCertManager(cfg.TLSDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	info, err := mgr.CAInfo()
	if err != nil {
		fmt.Fprintln(errOut, "cannot prepare the local CA:", err)
		return 1
	}
	fmt.Fprintf(out, "IPCCTV local CA\n  file:         %s\n  name:         %s\n  valid until:  %s\n  SHA-256:      %s\n\n", info.Path, info.Subject, info.NotAfter.Format("2006-01-02"), info.Fingerprint)
	fmt.Fprintln(out, "Install it once on each device (this file is the CA CERTIFICATE only; its key stays in the same folder and must stay private):")
	fmt.Fprintln(out, "  1. Get the file to the device: open https://<this PC's address>:<port>/api/tls/ca.crt there (after the first warning, with the token), or copy the file.")
	fmt.Fprintln(out, "  2. Check the SHA-256 shown by the device against the line above before trusting it.")
	fmt.Fprintln(out, "  3. iPhone: Settings > General > VPN & Device Management > install, then Settings > General > About > Certificate Trust Settings > enable it.")
	fmt.Fprintln(out, "     Android: Settings > Security > Install a certificate > CA certificate. Windows: double-click > Install > Local Machine/Current User > Trusted Root Certification Authorities.")
	fmt.Fprintln(out, "  Details and removal: docs/ui.md.")
	return 0
}
