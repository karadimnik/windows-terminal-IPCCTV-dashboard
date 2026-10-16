# Stage 2b - camui over HTTPS on the LAN: design

Branch `portable-nvr-2b`. Goal: the web app and the Live screen reach a phone or second PC on the home Wi-Fi over HTTPS
with a token, using a locally generated certificate authority the Operator installs once per device. Standard library
only (crypto/tls, crypto/x509), built into camui: no extra always-running program.

## What camui does today
`run` opens one plain-HTTP listener (`Listen`, default 127.0.0.1:8787). `newHandler` chains `requireToken` (token optional on
loopback, mandatory for a non-loopback bind), `logRequests`, the guard (Host allowlist, Origin/Sec-Fetch-Site, JSON content
type; one exemption for the WHEP route), then the mux. The cookie holds the raw token. The guard's `sameOrigin` compares
only the host:port of an Origin, not its scheme.

## Decisions
1. **Two listeners, two handler chains, one mux builder.** The loopback listener is unchanged. An optional TLS listener
   (`TLSListen`, a PRIVATE IPv4:port such as 192.168.1.50:8443; default empty = off) gets its own chain in which the token is
   mandatory. No HTTP on the LAN address, no redirect listener.
2. **A separate TLS token.** `TLSToken` (or env `CAMUI_TLS_TOKEN`; at least 16 characters) falls back to `Token`. Reason: with
   one shared `Token`, switching the LAN listener on would force a token onto the loopback dashboard too, which the brief
   says must stay exactly as it is. With only `Token` set it is used for both.
3. **Local CA**, ECDSA P-256, 10 years, `MaxPathLen 0`, CertSign+CRLSign, distinct CN, with X.509 NAME CONSTRAINTS (permitted
   IPs: 10/8, 172.16/12, 192.168/16, and 127/8 for the loopback SAN; permitted DNS: `localhost`, `.lan`, `.local`,
   `.home.arpa`, `.internal`), marked critical. A leaked CA key therefore cannot mint a certificate for a public site; the
   tests prove the Go verifier refuses `example.com` and a public IP through the CA's own chain. 127/8 and the private DNS
   suffixes go beyond the brief's list (needed for the loopback SAN and for `TLSHosts` names).
4. **Leaf**: ECDSA P-256, server-auth, 390 days (iOS/Safari limit 398), SAN = TLSListen IP + 127.0.0.1 + localhost +
   `TLSHosts`. Regenerated when absent, expired, within 30 days of expiry, or when the SAN set differs; otherwise reused. The CA
   and the devices' trust survive a changed LAN address.
5. **Keys**: written to `TLSDir` (default `%LOCALAPPDATA%\IPCCTV\tls`) through a temp file whose access is restricted BEFORE the
   key is written (0600; on Windows `icacls /inheritance:r /grant:r <user>:F`), then renamed. Failure to restrict refuses to
   start the TLS listener. Only the CA certificate is ever served.
6. **Token hardening (TLS listener)**: cookie `Secure; HttpOnly; SameSite=Strict`; the cookie holds `HMAC-SHA256(token,
   "camui-tls-cookie-v1")`, not the token (stateless, so it survives restarts and tabs; loopback keeps today's raw cookie);
   constant-time comparison; a per-source-IP failure limiter (10 failures per minute, then 429 with Retry-After; at most 1024
   tracked sources, expired entries purged, overflow shares one bucket); failures are logged without any token.
7. **Origin scheme**: on the TLS listener the guard requires the Origin scheme to be `https`; the loopback guard is unchanged.
8. **Hand-over**: `camui.exe -print-ca` (path, SHA-256 fingerprint, install steps; creates the CA if absent),
   `camui.exe -new-token` (prints a fresh 32-byte hex token, writes nothing), `GET /api/tls/ca.crt` and `GET /api/tls/info`
   on the TLS listener only (token-gated; 404 on loopback), and a "Connect a phone" panel in Settings that appears only when
   served over TLS.
