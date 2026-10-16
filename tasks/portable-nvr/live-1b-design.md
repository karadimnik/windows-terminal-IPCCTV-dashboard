# Stage 1B - the browser side of live view: design

Branch `portable-nvr-1b`. Builds on Stage 1A (`live-1a-design.md`).

## What camui does today (before 1B)

- Go, standard library only, in `ui/`. `newHandler` chains `requireToken(logRequests(newGuard(mux)))`; routes are Go 1.22
  patterns registered in `API.Routes`. Cameras are addressed by name only.
- `guard.go`: Host allowlist on every request; on `/api/*` refuses cross-site requests (`Sec-Fetch-Site`, `Origin`) and
  requires `application/json` on POST/PUT/PATCH/DELETE bodies (415 otherwise).
- `readStatus` reads `status.json` and passes it through `Redact` (drops `password`/`username`/`relaybase` keys, replaces any
  `scheme://...` token and bare IPv4 address). `/api/status` returns it whole, so Stage 1A's `Live.Paths` (the live path names)
  already passes through today.
- Front end: vanilla JS in `ui/static/` (`index.html` tabs Dashboard, Settings, Footage; `app.js`; `style.css`); no build step.

## Decisions

1. **Path names have one source.** The supervisor writes `Live.Cameras = [{Name, Sub, Main, SubPath, MainPath}]` in
   `status.json`. `camui` reads the path names from it and never re-implements the PowerShell naming rule.
2. **Path names stay off the browser.** `/api/status` now drops `Live.Paths` and `Live.Cameras[*].SubPath/MainPath` (they are
   not secrets, but nothing in the page needs them, so "no live path name in any response" is a plain invariant).
   `GET /api/live` returns camera names only. The relay's own `Restreamer.Paths` is pre-existing and untouched.
3. **Endpoints** (`ui/live.go`), all behind the existing token, Host and Origin guard:
   - `GET /api/live` -> `{enabled, alive, cameras:[{name, sub, main, camAlive, restarts, lastError}]}`.
   - `POST /api/live/{camera}/whep?stream=sub|main`, body `application/sdp` <= 64 KB. Order of checks: stream (400), live off
     (503 `live view is off`), live not running (503 `unavailable`), camera unknown (404), stream not offered for that camera
     (404), session caps (429), body (413/415/400), then the upstream call. Answer: `201`, `application/sdp`, `Location:
     /api/live/session/<id>` with a minted random id.
   - `DELETE /api/live/session/{id}` -> forwards a DELETE to the stored upstream URL, drops the id, `204`; unknown id `404`.
4. **Guard exception.** Only `POST /api/live/{camera}/whep` (one non-empty segment) may carry `application/sdp`; every other
   route keeps the JSON rule; Host, Origin and Sec-Fetch-Site still apply.
5. **Caps and lifetime.** 24 live sessions in total and 6 per camera (a page opens 6 sub tiles plus one focused main; two
   viewers fit). An id lives 1 hour from creation, then is forgotten (the peer connection closing ends the MediaMTX session
   anyway; the map only serves the tidy-up DELETE and the caps).
6. **Upstream.** `POST http://<LiveWhepAddress>/<path>/whep` with only `Content-Type: application/sdp` and Basic `camlive`
   plus the secret; no proxy, no redirects, no compression, no User-Agent; timeout 15 s. The address must be a loopback
   host (the login travels in clear HTTP). The secret is read from `LiveSecretPath` (default
   `%LOCALAPPDATA%\IPCCTV\live_view_secret.txt`) at each use, validated as 64 lowercase hex, never stored, logged or returned.
7. **Failure mapping.** upstream 401/403 or an unusable secret: 502 `live view is misconfigured` and ONE log line per episode;
   upstream 404: 404 `stream not available`; upstream 400: 400 `offer rejected`; other upstream status: 502 `live view
   error`; timeout: 504; cannot connect: 503 `unavailable`. Upstream bodies and headers are never returned.
8. **Front end.** A Live tab: grid of tiles (3 / 2 / 1 columns by width), sub-stream per tile, click or Esc to focus on
   main, muted by default with an unmute button, non-trickle `RTCPeerConnection` with `iceServers: []`, every connection
   closed on `visibilitychange` hidden and reopened when visible, DELETE with `fetch keepalive` on unload, deep link
   `#live` / `#live/<camera>`, `window.__liveStats()` hook for the rendered test.
