package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"holocron/internal/web"
)

var webTable = []doctorCheck{
	{id: "web.process", group: "web", run: webProcess},
	{id: "web.http", group: "web", concurrent: true, run: webHTTP},
	{id: "web.tls", group: "web", concurrent: true, run: webTLS},
	{id: "web.users", group: "web", run: webUsers},
	{id: "web.errors", group: "web", run: webErrors},
}

func init() { doctorTable = append(doctorTable, webTable...) }

// certWarnDays: below this the leaf certificate warns (the web role renews it at 30 days itself).
const certWarnDays = 30

func webOff(r *drun) []check {
	if r.e.cfg.Web.Enabled {
		return nil
	}
	return []check{mk(lvInfo, "web app", "off (web.enabled false): nothing to check")}
}

// tlsOff is the one line of the HTTPS checks when the listener is off (webTLS says it).
func tlsOff(r *drun) []check {
	if !r.e.cfg.Web.Enabled {
		return []check{}
	}
	if r.e.cfg.Web.TLSListen == "" {
		return []check{mk(lvInfo, "https", "the HTTPS listener is off (web.tlsListen is not set)")}
	}
	return nil
}

func webProcess(_ context.Context, r *drun) []check {
	if off := webOff(r); off != nil {
		return off
	}
	w := webStatus(r.e)
	if !w.Running {
		return []check{mk(lvFail, "web role", "not running (the host should run it beside the supervisor)").fix("holocron logs --component web --since 10m ; holocron restart --detach")}
	}
	return []check{mk(lvOK, "web role", "running, pid %d", w.Pid)}
}

// webHTTP: the loopback listener answers GET /api/status (loopback needs no login; no credential is
// sent) and says which control path the web app uses.
func webHTTP(ctx context.Context, r *drun) []check {
	if !r.e.cfg.Web.Enabled {
		return nil
	}
	u := "http://" + dialable(r.e.cfg.Web.Listen) + "/api/status"
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return []check{mk(lvFail, "web http", "%v", err).fix("correct web.listen in %s", r.e.paths.Config)}
	}
	var resp *http.Response
	if perr := r.probe(ctx, func() { resp, err = r.d.http.Do(req) }); perr != nil {
		err = perr
	}
	const fix = "holocron logs --component web --since 10m ; holocron restart --detach"
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return []check{mk(lvFail, "web http", "GET %s: %v", u, err).fix(fix)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return []check{mk(lvFail, "web http", "GET %s answered HTTP %d", u, resp.StatusCode).fix(fix)}
	}
	var st struct {
		Controller string `json:"controller"`
	}
	json.Unmarshal(body, &st)
	return []check{mk(lvOK, "web http", "GET %s answered 200; control path %s", u, orDash(st.Controller)).data(map[string]any{"controller": st.Controller})}
}

// webTLS: the HTTPS listener completes a handshake and its server certificate is not near expiry.
func webTLS(ctx context.Context, r *drun) []check {
	if off := tlsOff(r); off != nil {
		return off
	}
	addr := dialable(r.e.cfg.Web.TLSListen)
	var chain []*x509.Certificate
	var err error
	if perr := r.probe(ctx, func() {
		var c net.Conn
		if c, err = r.dialTimeout(ctx, addr); err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(probeTimeout))
		host, _, _ := net.SplitHostPort(addr)
		tc := tls.Client(c, &tls.Config{ServerName: host, InsecureSkipVerify: true}) // the certificate is read, not trusted
		if err = tc.HandshakeContext(ctx); err == nil {
			chain = tc.ConnectionState().PeerCertificates
		}
	}); perr != nil {
		err = perr
	}
	const logs = "holocron logs --component web --since 24h"
	switch {
	case err != nil:
		return []check{mk(lvFail, "https", "no TLS handshake on %s: %v", addr, err).fix("%s ; holocron restart --detach", logs)}
	case len(chain) == 0:
		return []check{mk(lvFail, "https", "%s sent no certificate", addr).fix("%s", logs)}
	}
	leaf := chain[0]
	left := leaf.NotAfter.Sub(r.now)
	days := int(left.Hours() / 24)
	data := map[string]any{"notAfter": leaf.NotAfter, "daysLeft": days}
	var c check
	switch {
	case left <= 0:
		c = mk(lvFail, "https", "the server certificate of %s EXPIRED on %s", addr, leaf.NotAfter.Format("2006-01-02")).fix("restart the web role (it issues a new one at start) ; %s", logs)
	case days < certWarnDays:
		c = mk(lvWarn, "https", "the server certificate of %s expires in %d day(s) (%s); the web role should have renewed it", addr, days, leaf.NotAfter.Format("2006-01-02")).fix("%s ; restart the web role (it renews at start)", logs)
	default:
		c = mk(lvOK, "https", "%s answers TLS; server certificate valid %d more day(s) (until %s); the HTTPS listener is view-only unless the web role runs with -tls-readonly false", addr, days, leaf.NotAfter.Format("2006-01-02"))
	}
	return []check{c.data(data), caCheck(r, chain)}
}

// caCheck: the local CA the listener presents after its certificate (the one devices install):
// its fingerprint, that it signed the certificate, that it is not expired, and its file.
func caCheck(r *drun, chain []*x509.Certificate) check {
	const rotate = "holocron tls rotate --yes, restart the web role, then install the new CA on each device (holocron tls ca)"
	if len(chain) < 2 {
		return mk(lvWarn, "local CA", "the HTTPS listener presents no CA after its certificate").fix("holocron tls ca")
	}
	ca := chain[1]
	fp := fingerprintOf(ca.Raw)
	data := map[string]any{"fingerprint": fp, "notAfter": ca.NotAfter}
	switch {
	case chain[0].CheckSignatureFrom(ca) != nil:
		return mk(lvFail, "local CA", "the certificate is not signed by the CA it presents (SHA-256 %s)", fp).fix("%s", rotate).data(data)
	case r.now.After(ca.NotAfter):
		return mk(lvFail, "local CA", "SHA-256 %s EXPIRED on %s", fp, ca.NotAfter.Format("2006-01-02")).fix("%s", rotate).data(data)
	}
	file := "in the web app's default TLS folder (holocron tls ca names it)"
	if dir := r.e.cfg.Web.TLSDir; dir != "" {
		file = filepath.Join(dir, "ca.crt")
		if _, err := os.Stat(file); err != nil {
			return mk(lvWarn, "local CA", "SHA-256 %s, but %s: %v", fp, file, err).fix("holocron tls ca").data(data)
		}
	}
	data["file"] = file
	return mk(lvOK, "local CA", "SHA-256 %s, valid until %s, file %s", fp, ca.NotAfter.Format("2006-01-02"), file).data(data)
}

// fingerprintOf is the SHA-256 of a DER certificate in the colon form `holocron tls ca` prints.
func fingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(parts, ":")
}

func webUsers(_ context.Context, r *drun) []check {
	if !r.e.cfg.Web.Enabled || r.e.cfg.Web.TLSListen == "" {
		return nil
	}
	n, err := web.LoginUsers(r.e.cfg.Web.AuthDir)
	switch {
	case err != nil:
		return []check{mk(lvFail, "users", "the user file cannot be read: %v", err).fix("holocron user list")}
	case n == 0:
		return []check{mk(lvFail, "users", "no login user: the HTTPS listener lets nobody in").fix("holocron user add <name> (reads the password from standard input)")}
	}
	return []check{mk(lvOK, "users", "%d login user(s) (holocron user list)", n)}
}

// webErrors: ERROR records of the web role in the last hour.
func webErrors(_ context.Context, r *drun) []check {
	if !r.e.cfg.Web.Enabled {
		return nil
	}
	since := r.now.Add(-time.Hour)
	n, first := 0, ""
	for _, l := range r.logLines() {
		if l.Level == "ERROR" && strings.EqualFold(l.Component, "web") && !l.Time.Before(since) {
			if n == 0 {
				first = l.Text
			}
			n++
		}
	}
	if n > 0 {
		return []check{mk(lvWarn, "web errors", "%d ERROR record(s) of the web role in the last hour, the first: %s", n, first).fix("holocron logs --component web --level error --since 1h")}
	}
	return []check{mk(lvOK, "web errors", "no ERROR record of the web role in the last hour")}
}
