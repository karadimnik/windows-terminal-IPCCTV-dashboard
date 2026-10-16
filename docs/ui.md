# The control UI

Its four screens - Dashboard, Settings, Footage and Live - are described in the [README](../README.md#day-to-day); Live has its own section below.

## How it works

`camui.exe` is one Go binary with the whole UI embedded in it. **It owns no state.** Every number comes from `_state\status.json`, the same file the health strip reads, and every change goes through `cam.ps1` and `cam.recorder.ps1`. That is why the command line keeps working unchanged and why the two can never disagree about anything. Its Footage index is parsed from the filenames the recorder already writes, so there is no database and nothing opens a segment to list it; a clip is remuxed to fragmented MP4 on demand, video copied and only the 8 kHz audio transcoded, so playback keeps its sound.

**No camera credential ever reaches the browser.** Cameras are addressed by name, and no endpoint returns a relay or RTSP URL of any shape. The harness proves it rather than trusting it: the `uileak` check fetches every endpoint, a real clip included, and scans every byte for the password, for its URL-escaped form and for anything shaped like a credential in a URL, with a positive control proving the scanner can see one.

**Loopback only, deliberately.** `127.0.0.1` needs no `urlacl` and no elevation, and the binary **refuses to start** on any address that is not exclusively loopback unless an auth token of at least 16 characters is configured. Exposing it beyond this machine is therefore a deliberate act rather than something that can happen by accident. To do it, save this as `camui.config.json` next to `camui.exe` and restart the UI:

```json
{
  "listen": "0.0.0.0:8787",
  "token": "a-long-random-string-of-at-least-16-characters"
}
```

Nothing else has to change: the launcher passes that file if it exists, and `-Status` reports the address it found and where it read it from. Every request then needs the token, which can arrive once as `?token=...` and is moved into an HttpOnly cookie so it stops appearing in the address bar, in history and in any `Referer`. The scripts **do** read the token - `Get-CamUiRuntimeState` opens `camui.config.json` and looks at `CAMUI_TOKEN` to answer the one question `-Status` asks of it - but they never print it, log it or pass it on: `-Status` reports only whether one is configured. The token is never taken from a flag, because a command line is readable by every process on the machine.

**A web page you visit cannot drive it.** The UI sits on a fixed local address that a page in your own browser can reach, so every request passes a guard before anything else. The `Host` header must name `localhost`, `127.0.0.1` or `::1`; that is what stops DNS rebinding, where an attacker's name is pointed at `127.0.0.1` so that the browser treats camui as that page's own site. On `/api/` a request carrying `Sec-Fetch-Site` other than `same-origin` or `none`, or an `Origin` that is not the address it was sent to (`null` included), is refused with 403. A state-changing request (POST, PUT, PATCH, DELETE) that carries a body must be `application/json`, otherwise 415 - a browser cannot send that type to another site without a preflight, which camui never answers. A request with neither header, such as a script, `curl` or the test harness, passes these checks and is still subject to the token if one is set. Responses under `/api/` carry `Cross-Origin-Resource-Policy: same-origin`, `X-Content-Type-Options: nosniff` and, unless the handler sets its own, `Cache-Control: no-store`. The page and its assets are subject only to the `Host` check. A refusal is logged once, with the method, path and reason, and never a header value or a body. The `uiguard` check proves all of it against the real binary.

In the exposed setup above (non-loopback `listen` and a token) the `listen` host is allowed as a `Host` too. A wildcard bind (`0.0.0.0` or an empty host) has no single address to name, so any IP-literal `Host` is allowed instead - an IP literal cannot be rebound. To reach it by a **name** (`nvr.lan`), list the name, port ignored, in `allowedHosts`:

```json
{
  "listen": "0.0.0.0:8787",
  "token": "a-long-random-string-of-at-least-16-characters",
  "allowedHosts": ["nvr.lan"]
}
```

`allowedHosts` is optional, empty by default and read from the config file only. Browsers on other machines reach camui by a name or an address you chose, so a name that is missing from the list gets a 403 from the guard rather than a login prompt.

**How it runs.** The `IPCCTV-Ui` logon task runs a generated launcher at `%LOCALAPPDATA%\IPCCTV\start-ui.ps1`. The launcher waits for `camui.exe` to be reachable, exactly as the recorder's launcher waits for the scripts, then starts it in a console of its **own** with no window. That last part is not cosmetic: a console created with a window is handed to Windows Terminal as a tab in the window that already exists, and closing that window would take the UI with it. It starts nothing at all if the address is already being served, so a second start cannot fight the first one for the port, and it exits non-zero if nothing is serving afterwards, which hands the retry to the task's restart policy.

camui logs to stdout only, so a thin `powershell.exe` holds the pipe and appends it to `%LOCALAPPDATA%\IPCCTV\camui.log`. **The roll happens in the launcher, not in a running UI:** at every start it checks that file, and if it is past 10 MB it renames it to `camui.log.prev` - replacing any previous `.prev` - and starts a fresh one. A UI that stays up for weeks therefore keeps appending to one file past 10 MB, with a line per request and nothing to bound it; stopping and starting it is what rolls the log. The launcher's own log is `autostart-ui.log` beside it, and that one has no rotation at all - it gets a handful of lines per start, so it grows slowly, but it grows forever. Delete either by hand when it is in the way; nothing reads them back.

The binary is never copied anywhere, so a rebuild takes effect the next time the UI starts, with no reinstall. Stop it before rebuilding and start it again afterwards: a running process keeps the code it started with, and with the repository on the WSL share the write is not refused the way Windows refuses to overwrite a running binary on a local drive. `cam.ps1 -Stop` does not stop the UI, which is the point - the thing you start everything from should still be there after you have stopped everything. When you do want it gone, `-Status` prints the pid that owns the port.

## The commands: `camui.exe`

`-Install` puts its logon task in place, and letting that run it is the normal way. These are for building it and for the rare direct run.

| Command | What it does |
| --- | --- |
| `ui/build.sh [output]` | Cross-compiles `camui.exe` into the repository root, or to `output` if you name one. A bash command, so it needs WSL or another machine with bash, and **Go 1.27 or newer** - `go.mod` asks for it and an apt-packaged Go will refuse the build; [docs/go-toolchain.md](go-toolchain.md) is the one-time install, and the script also finds a toolchain at `~/.local/go/bin`. It finds its own directory, so the directory you run it from does not matter. It runs `gofmt`, `go vet` and `go test` first and stops on any of them, so a build that completes is a build that passed. |
| `.\camui.exe` | Runs it in the foreground on `http://127.0.0.1:8787/`, with its log on screen. Useful when you want to watch what it is doing; Ctrl+C stops it. |
| `-listen <host:port>` | Where to bind. Default `127.0.0.1:8787`. It **refuses to start** on any non-loopback address unless a token of at least 16 characters is configured. |
| `-config <path>` | A JSON config file: `listen`, `archiveRoot`, `statusFile`, `scriptDir`, `token`, `allowedHosts`. The installed launcher passes `camui.config.json` from beside the binary if that file exists, and nothing else needs to change to use it. |
| `-archive-root <path>` `-status-file <path>` `-script-dir <path>` | Override what it reads. Precedence for the archive root is this flag or `archiveRoot` in the config file, then `RootPath` in the `recorder.config.json` beside the binary, then the built-in `E:\cameras` - so moving the archive needs no second edit here. The status file defaults to `<archive root>\_state\status.json` and the script directory to the directory holding the binary. |
| `-version` | Prints the build stamp and exits. |

The binary has to sit beside the scripts: with no `-script-dir` it looks for `cam.ps1`, `cameras.json` and `recorder.config.json` in its own folder.

## Live view

The **Live** tab (beside Dashboard, Settings and Footage) shows the cameras as a tile grid in the browser: 3 columns on a wide window, 2 on a medium one, 1 on a phone. It needs live view switched on and running; with `LiveViewEnabled` false (the default) the tab says so and nothing else changes - no process is started, no camera session is used, and every other screen behaves as before.

**What it shows.** Each tile plays the camera's second stream (`LiveSubStream`, small and cheap) with the camera name and a state chip: **Connecting**, **Live**, **Offline** (no picture from that camera, or the connection was lost), **Error**. Click a tile to watch it on the **main** stream, larger, while the other tiles stay on their sub-streams; only one main stream is open at a time. Click the tile again or press Esc to go back. Sound is **muted** until you press that tile's **Unmute** button. A failed connection retries with a growing delay (1 to 15 seconds).

**Closed when nobody looks.** Every connection is closed while the page is hidden (a minimized window, another browser tab) and while another camui tab is selected, and reopened when the page is back: a hidden tab keeps decoding at a reduced rate, which costs this PC for nothing, and every open tile holds a camera session. Closing the page or navigating away sends a best-effort `DELETE` for each session. Because the camera's second stream is pulled on demand, its session exists only while a tile is open.

**Deep links.** `#live` opens the tab and `#live/<camera name>` (percent-encoded, case-insensitive) opens it with that camera focused on the main stream, for example `http://127.0.0.1:8787/#live/Front%20Entrance`.

**Needs** (`recorder.config.json`, details in [configuration.md](configuration.md#live-view)): `LiveViewEnabled` `true` (and `RestreamerEnabled`), restart the recorder; optionally `LiveWhepAddress` (default `127.0.0.1:8889`, loopback only) and `LiveSecretPath` (default `%LOCALAPPDATA%\IPCCTV\live_view_secret.txt`). camui reads these keys and the secret file; it never returns any of them.

**How it works.** The browser talks only to camui. camui forwards the WebRTC offer to the live instance with the login added server-side and returns the answer; the picture itself travels from the live instance to the browser directly over UDP on loopback. The browser never sees the instance's address, its login, a path name or a camera address, and no response of the endpoints below carries one: the `uileak` check scans `GET /api/live`, `GET /api/status`, the SDP answer with its headers, every refusal and the static assets for all of them, with a positive control.

| Endpoint | Behaviour |
| --- | --- |
| `GET /api/live` | `{ "enabled", "alive", "cameras": [ { "name", "sub", "main", "camAlive", "restarts", "lastError" } ] }` from `status.json` (redacted). Camera names only; `cameras` is empty when live view is off. |
| `POST /api/live/{camera}/whep?stream=sub\|main` | The body is the browser's SDP offer, `Content-Type: application/sdp`, at most 64 KB. Answers `201`, `Content-Type: application/sdp`, the answer SDP and `Location: /api/live/session/<id>`, an id camui minted. Errors are JSON with generic text: `400` bad `stream` or empty offer, `404` unknown camera or stream not offered or not available, `413` offer too large, `415` not `application/sdp`, `429` too many sessions, `502 live view is misconfigured` (the live instance refused the login, or the secret file is missing or not 64 hex characters, or `LiveWhepAddress` is not loopback), `503 live view is off` or `503 unavailable` (not running, or not reachable), `504` the live instance did not answer in 15 s (a camera source can take up to 10 s to start). An upstream body or header is never returned. |
| `DELETE /api/live/session/{id}` | Closes that session at the live instance and forgets the id: `204`; an id camui did not mint (or one older than an hour) is `404`. A client can only name a session by an id camui minted, never an address. |

- **Caps.** At most **24** live sessions at a time and **6** per camera (a page opens six sub tiles plus one focused main stream, so two viewers fit); more is `429`. A session id is forgotten one hour after it was created: the live instance ends the session itself when the browser's connection closes, and the table only serves the tidy-up `DELETE` and the caps.
- **What goes upstream.** `POST http://<LiveWhepAddress>/<path>/whep` with only `Content-Type: application/sdp` and the Basic login. No browser header (`Cookie`, `Authorization`, `Origin`, `Referer`, `User-Agent`) is forwarded, no proxy from the environment is used, and redirects are not followed. The secret is read from its file at each use and is never logged, returned or stored. A refused login logs **one** redacted line per episode (until the next success), not one per tile.
- **The guard exception.** The request guard requires `application/json` on every state-changing request with a body. A WebRTC offer is `application/sdp`, so exactly one route is exempt: `POST /api/live/{camera}/whep`, with one non-empty `{camera}` segment, and only for that content type. Every other route, method and content type is refused with `415` as before, and the `Host`, `Origin` and `Sec-Fetch-Site` checks apply to the live routes like any other; a page on another origin, a cross-site request or a rebinding `Host` gets `403` and no session reaches the live instance. The `livepage` check proves it with a foreign page in Chrome and Edge and the same requests sent raw, with a positive control.
- **`/api/status` and path names.** `status.json`'s `Live` section carries a `Cameras` list (`Name`, `Sub`, `Main`, and the two path names camui needs). `/api/status` drops `Live.Paths` and those two path names before answering, so no live path name reaches a browser.
- **Checks.** `go test` covers the endpoints against a fake upstream; `uileak` scans the live responses; `livepage` (opt-in, it opens Chrome and Edge windows) plays the six tiles at 1920x1080 and 390x844 in a real browser and checks columns, focus, mute, hiding and the camera's session count. `livepage` needs `CAMUI_TEST_EXE` to name a `camui.exe` built from this tree.

**Try it without touching the real system.** `tests\live\demo.ps1` starts a throwaway copy of the rig (the real supervisor with live view on, six synthetic cameras, a camui built to a temp path, non-default ports, fake credentials) and prints the address to open in your own browser.
From a local directory in Windows PowerShell (not from the `\\wsl.localhost` share):
`cd C:\Windows\Temp` then `powershell -NoProfile -ExecutionPolicy Bypass -File <repo>\tests\live\demo.ps1 -Minutes 15`.
Open `http://127.0.0.1:18789/#live`; press Enter in that window, or wait out `-Minutes` (default 15), and it stops exactly what it started and deletes its temp folders.
It builds camui through WSL unless `-CamuiExe` names an exe built from this tree, refuses to start if one of its own ports (18789, 18554, 18557, 18564, 18567, 18889, UDP 18189) is busy, and never uses or touches 8787, 8554, 9997, 8889 or 8189.
