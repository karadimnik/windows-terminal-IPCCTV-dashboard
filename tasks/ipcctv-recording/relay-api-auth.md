# [89] Relay control API authentication - documentation findings and design

Branch `executor/relay-api-auth`, 2026-10-15. MediaMTX v1.21.2 (the installed `C:\mediamtx\mediamtx.exe`).

## What the official documentation says

Sources, all at tag `v1.21.2` of https://github.com/bluenviron/mediamtx (fetched 2026-10-15):

- `docs/2-features/06-authentication.md` (published as https://mediamtx.org/docs/features/authentication)
- `mediamtx.yml`, the reference configuration, section "Global settings -> Authentication"
- source read to confirm the documented behaviour: `internal/conf/credential.go`, `internal/auth/manager.go`,
  `internal/auth/log_and_delay_error.go`, `internal/api/api.go`

Documented:

- `authMethod: internal` is the default. `authInternalUsers` is a list of users, each with `user`, `pass`, `ips`
  and `permissions`. `user: any` means any user including anonymous ones, and its `pass` is not used. `ips: []`
  means any IP.
- Permission actions are exactly `publish`, `read`, `playback`, `api`, `metrics`, `pprof`. A `path` narrows
  `publish`, `read` and `playback` only (empty means any path, `~` prefix means a regular expression).
- The built-in default list is two users: `any` from any IP with `publish`, `read`, `playback`; and `any` from
  `127.0.0.1` and `::1` with `api`, `metrics`, `pprof`. That second entry is what made the API anonymous. Setting
  `authInternalUsers` replaces the whole list, so the first entry has to be restated.
- `pass` (and `user`) accept a hash: `sha256:` followed by the base64 of the SHA-256 of the password, or `argon2:`
  followed by an Argon2id/Argon2i encoding. The comparison is constant time (`credential.go`, `Check`). A plain
  credential is restricted to `A-Z a-z 0-9 ! $ ( ) * + . ; < = > [ ] ^ _ - { } @ # &`.
- HTTP-based actions (the API among them) take the login from an `Authorization: Basic` header.

Read in the source and then measured (scratchpad `ra-exp1.out`):

- A request with NO credentials is refused at once with `401` and `WWW-Authenticate: Basic realm="mediamtx"`.
- A request with WRONG credentials is logged (`WAR [API] [conn ...] failed to authenticate: authentication
  failed`, no credential in the line) and answered `401` only after a random pause of 0 to 4 s
  (`LogAndDelayError`, brute-force damping). Measured: 704, 1189, 2668, 3784 ms; with the product's 1500 ms probe
  timeout, 4 of 4 wrong-login probes timed out. **This is the trap: a wrong login looks like a dead relay.**
- Right login: `200` within a few ms on every route, including `/v3/config/*` (an authenticated caller may read
  the source URLs; that is the API's job).
- Changing `authInternalUsers` in the file is hot-reloaded ("reloading configuration (file changed)"); the old
  login is refused, the new one accepted, and a connected RTSP publisher stays connected.
- No `Access-Control-Allow-Origin` on any response, including the `401`s and the preflight (`apiAllowOrigins:
  []` is the default).
- Anonymous RTSP publish and read on loopback keep working with the `any` entry restated.

## Design

1. **The login.** `relay_api_creds.xml` next to `camera_creds.xml`, gitignored: a PSCredential written with
   `Export-Clixml`, so the password is DPAPI-protected for the current Windows user like the camera credential.
   User `camrelayapi`; password 64 hex characters from 32 bytes of `RandomNumberGenerator`. Created by
   `Get-CamRelayApiCredential -Create`, which `-InstallRestreamer` and every restreamer start in the supervisor
   call; it never replaces an existing file. The password is registered with the redactor, and so is the base64
   Basic token, which is equivalent to it.
2. **The config.** The template gains an auth block: the `any` user restated with `publish`, `read`, `playback`
   from any IP (exactly the default), and the API user restricted to `127.0.0.1`/`::1` with `api` only. No entry
   grants `api`, `metrics` or `pprof` to `any`. The file holds `pass: sha256:<base64>`, never the password.
   Hash in the file rather than an environment variable: the documentation supports it, the SHA-256 of a 256-bit
   random secret cannot be reversed or guessed (Argon2's cost only matters for human-chosen passwords), an
   environment variable is readable by any process of the same account anyway, and a file that is complete on its
   own lets "is the file current" stay a plain text comparison.
3. **The callers.** One helper, `Invoke-CamRelayApi`, is the only code that talks to the API. It sends the Basic
   header in-process on the existing `HttpWebRequest` (no child process, no command line) and returns `ok`,
   `denied` or `down` with the parsed body.
4. **401 is not down.** `denied` is a 401/403 to the request as sent. Because a wrong login is answered slowly, a
   credentialed probe that times out is followed by one anonymous probe with the same budget: an immediate 401
   proves the relay is alive and demanding a login the product did not satisfy, so the answer is `denied`; a 200
   (an old config that still takes anonymous calls) is used as the answer; anything else is `down` as before.
   On `denied` the readiness helpers report every path as served - `Test-CamRelayPathReady` returns true and
   `Get-CamRelayReadyPaths` returns every camera's path - so `Get-CamInputUrl` keeps every consumer on the relay
   and never sends one direct. RTSP itself needs no login, so the relay keeps serving; what is lost during the
   fault is only the readiness view, which is what the alert is for.
5. **One alert per episode.** The supervisor, which owns the relay, watches the result of its per-cycle list
   query. The first `denied` after start or after an `ok` raises `Send-CamAlert` with event ID 130, severity
   Error, and its ALERT line in the supervisor log is the one log line; nothing more is logged or raised until an
   `ok` ends the episode, which is logged once. Neither line carries the login.
6. **Upgrade.** `Test-CamRelayConfigCurrent` also reports `AuthCurrent`: the auth block in the file equals the
   one `Get-CamRelayConfigText` renders for the stored login. A restreamer start that finds it not current (an old
   file, or a login that was regenerated) rewrites the file from `Get-CamRelayConfigText` before launching, the
   same render `-InstallRestreamer` writes, and logs that it did. Nothing else about an out-of-date path set
   changes: that is still a warning and a start with the file as it is.
