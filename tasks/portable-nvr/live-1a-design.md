# Stage 1A - the dedicated live-view MediaMTX: what the official docs say, and the design

Branch `portable-nvr-1a`. Everything below was read from the `v1.21.2` tag of https://github.com/bluenviron/mediamtx
(raw files fetched 2026-10-16): `mediamtx.yml` (the reference config), `docs/2-features/06-authentication.md`,
`docs/4-read/03-webrtc.md`, `internal/protocols/httpp/server.go` and `handler_origin.go`,
`internal/servers/webrtc/http_server.go`. Where a claim is also measured, the Stage 1A checks (`livewhep`,
`liveondemand`, `livesecret`, `livecfg`) are the measurement.

## What the docs and source say

| Question | Answer, with the file it comes from |
|---|---|
| What does an empty `webrtcAllowOrigins` do? | `handler_origin.go`: `isOriginAllowed` returns false when the list is empty, and `Access-Control-Allow-Origin` is set only when it returns true. So an empty list sends NO cross-origin permission at all. The reference value is `["*"]`, which matches any origin and is echoed back (the hole found in Stage 0). |
| Any other cross-origin guard? | Same file: a POST whose `Sec-Fetch-Site` is `same-site` or `cross-site` is answered 403 unless the origin is allowed. With an empty list every browser POST that is not same-origin is refused, even with a correct login. Consequence for the design: the browser must reach WHEP through a same-origin proxy (`camui`, Stage 1B), exactly as the plan says. The test page does the same through a tiny proxy. |
| Host check? | None in `server.go` or `http_server.go` (only `handlerFilterRequests`: empty or non-`/` paths are 400). A rebinding `Host` is therefore not refused by MediaMTX; what protects the live instance is the login and, in Stage 1B, `camui`'s guard. |
| How does WHEP authenticate? | `http_server.go`: `httpp.Credentials(request)` - `Authorization: Basic`, `Authorization: Bearer user:pass`, or `user`/`pass` query parameters; a missing or wrong login is 401, and a 401 with no credentials carries `WWW-Authenticate: Basic`. The action checked is `read` for protocol `webrtc` (`06-authentication.md`). |
| Hashed password? | `06-authentication.md`: `pass: sha256:<base64 of the SHA-256>` is accepted (same form as the relay's API login). |
| Can a user be limited to WebRTC? | No: permissions are `publish`, `read`, `playback`, `api`, `metrics`, `pprof` with an optional path; there is no protocol field. A `read` user can read over every enabled protocol, so every other server is switched off. |
| Servers to switch off | Reference config keys: `api`, `metrics`, `pprof`, `playback`, `rtsp`, `rtmp`, `hls`, `srt`, `moq` (all `true` or `false` in the reference; the product writes `no`). |
| WebRTC addresses | `webrtcAddress` (HTTP signaling, TCP), `webrtcLocalUDPAddress` (ICE/UDP), `webrtcLocalTCPAddress` (ICE/TCP, "" = disabled), `webrtcIPsFromInterfaces` (default true), `webrtcIPsFromInterfacesList` / `...ExcludeList`, `webrtcAdditionalHosts`. Loopback-only without naming an interface: `webrtcIPsFromInterfaces: no` and `webrtcAdditionalHosts: ['127.0.0.1']`; proven in `livewhep`. |
| On-demand | `mediamtx.yml` path settings: `sourceOnDemand`, `sourceOnDemandStartTimeout` (10s), `sourceOnDemandCloseAfter` (10s). The source is opened on the first reader and closed that long after the last. |
| Source from the environment | The relay's own mechanism (verified for the relay): `MTX_PATHS_<NAME>_SOURCE`, the name cut at the first underscore, so path names are lowercase alphanumeric. |

## The design as built

- Keys (`recorder.config.json`, all default off or documented): `LiveViewEnabled` (false), `LiveExePath` ("" = the relay's
  `RestreamerExePath`), `LiveConfigPath` (`C:\mediamtx\mediamtx-live.yml`, local NTFS), `LiveWhepAddress`
  (`127.0.0.1:8889`), `LiveUdpAddress` (`127.0.0.1:8189`), `LiveSubStream` (`stream2`), `LiveCloseAfterSeconds` (10),
  and `LiveSecretPath` ("" = `%LOCALAPPDATA%\IPCCTV\live_view_secret.txt`; see below for why this one was added).
- Config rendered by `Get-CamLiveConfigText` in code (the relay's template and `Get-CamRelayConfigText` are untouched).
  One user `camlive` with `read` only and `pass: sha256:`; no `any` user; `webrtcAllowOrigins: []`; every other server off.
- Paths per camera: `<relaypath>sub` (source supplied by `MTX_PATHS_<NAME>_SOURCE` from the camera's `LiveSubStream`,
  on demand) and `<relaypath>main` (`rtsp://127.0.0.1:8554/<relaypath>`, the recording relay, no credential, on demand).
  A camera whose recorder `Quality` already equals `LiveSubStream` gets only `<relaypath>main`: the recorder is already
  reading that stream through the relay, so a second pull of it from the camera would add a session for nothing.
  A live name that collides with a relay path or another live path is an error, like the relay's own map.

## Why the secret is not "next to camera_creds.xml"

The brief asked for a user-only ACL file next to `camera_creds.xml`. Measured 2026-10-16: on this machine the product's
scripts run from the repository on the WSL share (`\\wsl.localhost\...`), and `icacls` and `Get-Acl` on a file there fail
("Incorrect function"), because that share has no NTFS ACLs. A file there cannot be restricted to one Windows account and
the restriction cannot be proven. The secret therefore lives in a local per-user folder, `%LOCALAPPDATA%\IPCCTV\`
(already used for the generated launchers, see `Get-CamAutostartDir`), created with an explicit protected ACL granting
only the current user. `LiveSecretPath` exists so the sandbox checks, and an install that wants another folder, can name it.
`camui` (Go, standard library) reads the same file: 64 hex characters, no newline.

DPAPI (`Export-Clixml`, like `relay_api_creds.xml`) was the alternative: it also protects against another Windows
account, but Go cannot read the blob without parsing the CLIXML and calling `CryptUnprotectData`, which is not in the
standard library. Trade-off accepted: a plain file is readable by every process of the SAME account (DPAPI is too, since
any process of the account can decrypt it), and is not readable by any other account, which is the property that matters.
