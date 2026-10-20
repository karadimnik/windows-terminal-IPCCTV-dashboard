# ctlapi

The supervisor's local control endpoint (LLD 2.11) and its Go client. Standard library plus `cli`, `logx`, `osx`, `status`.
It runs INSIDE the supervisor process; `supervise` does not import it (the supervisor sees it as `supervise.Control`).

## Wire

- Listens on `127.0.0.1:0` only (any other address is refused). The chosen `host:port` goes to `<root>/_state/ctl.addr`, a fresh
  32-byte secret (64 lowercase hex, no newline) to `<root>/_state/ctl.secret`; both written temp + owner-only (`osx.SetOwnerOnly`)
  + rename, and removed when the server stops (first step of the supervisor's ordered stop).
- Every request: `Host` must be `127.0.0.1:<port>` or `localhost:<port>` (DNS-rebinding guard) and no `Origin` header (no
  browser), else 403 with no body; then header `X-Holocron-Key` (constant-time compare), else 401 with no body. No CORS headers.
- POST bodies: `Content-Type: application/json` (415 otherwise), at most 64 KB (413), one JSON object, unknown fields refused (400).
- `X-Action-Id` (1-64 of `[A-Za-z0-9._-]`) is adopted as the action id of the handler's records and echoed; otherwise one is
  generated. `X-Holocron-Actor` (`cli:...`/`web:...`) becomes the records' actor (default `ctlapi`).
- One state-changing request at a time: a second concurrent one gets **409** and runs nothing (G14). Reads are not serialised.
- Body of every answer: `{"ok": bool, "action_id": "...", "error": "...", "data": ...}`. 404 not found, 422/400 refused,
  503 not running, 500 other failure.

| Route | Action (component `ctlapi`) | Body | data |
|---|---|---|---|
| `GET /v1/state` | `ctlapi.state` | - | `State{running, pid, started, updated, cameras[{name, safeName, record, alive, mode, pid, restarts, restartsLastHour, lastError}], relay, live, prune, disk}` |
| `POST /v1/stop` | `ctlapi.stop` (mutating) | `{"kind": "requested"\|"interrupted"}` (default requested) | `StopReport{kind, reason, survivors[], final}` after the ordered stop |
| `POST /v1/reload` | `ctlapi.reload` (mutating) | `{}` | - (cameras.json and the configuration re-read now; LiveViewEnabled applied) |
| `POST /v1/cameras/{name}/record` | `ctlapi.camera.record` (mutating) | `{"enabled": bool}` | `Changed{target, enabled, changed}` (cameras.json edited, validated, saved, then reload) |
| `POST /v1/live` | `ctlapi.live` (mutating) | `{"enabled": bool}` | `Changed` (LiveViewEnabled edited, then reload starts/stops the live MediaMTX) |
| `GET /v1/log/level` | `ctlapi.log.level.get` | - | `Levels{default, components}` |
| `POST /v1/log/level` | `ctlapi.log.level.set` (mutating) | `{"component": "<name>"\|"all", "level": "..."}` | `Levels` (immediate; `all` sets the default and clears overrides) |

Every handler is a registered `cli.Action` run by `cli.Runner` (begin DEBUG, result INFO/DEBUG/WARN/ERROR, mutating ones also in
the audit stream); `Routes` is the exposed list and `TestEveryRouteIsAnAction` asserts it.

## API

    type Backend interface{ Snapshot() *status.Document; Stop(kind) StopReport; Reload(ctx) error
                            SetRecord(ctx, camera, enabled) (Changed, error); SetLive(ctx, enabled) (Changed, error) }
    func NewServer(Options{StateDir, Backend, Levels *logx.LevelController, Runner *cli.Runner, Platform, Log, Redact func(string)}) *Server
    (s) Start() error; Stop()                    // Stop: no new requests, files removed, in-flight ones (a stop request) finish
    (s) Wait(ctx) error; Addr() string
    func Dial(root string) (*Client, error)      // ErrUnreachable when the files are missing or malformed
    (c) State, Stop(ctx, kind), Reload, SetRecord(ctx, name, on), SetLive(ctx, on), Levels, SetLevel(ctx, component, level)
    errors: ErrUnreachable, ErrBusy (409), ErrNotFound (404), ErrNotRunning (503), cli.ErrRefused (400/422); *APIError{Code, Message}
    func StateFrom(*status.Document) State; ApplyLevel(*logx.LevelController, component, level); LevelsFrom(logx.LevelSnapshot)

The client sends the ctx's `cli.ActionIDFrom` and `cli.ActorFrom` as headers, so `holocron cam disable x` and the supervisor's
records of it share one action id. The secret is registered with the logger's redactor; it never reaches a log.

## Tests

`go test -race ./internal/ctlapi`: auth (missing, wrong, prefix key; rebinding Host, other port, Origin), body limit, content type,
strict body, 409 with three concurrent mutations (bounded; negative control: without the lock the test fails), action id and actor
propagation into the backend, every record and the audit stream, every endpoint, a stop request that outlives the server's
own Stop (the supervisor's teardown), file mode and removal, a fresh secret per start, loopback-only listen, the leak scan at
TRACE with a positive control. On Windows `TestFilesOwnerOnlyAndRemoved` checks the ACL with `icacls` (no Users, Administrators,
SYSTEM or Everyone entry):

    GOOS=windows go test -c -o /mnt/c/Windows/Temp/holocron-w9a/ctlapi.test.exe ./internal/ctlapi
    cd /mnt/c/Windows/Temp/holocron-w9a && ./ctlapi.test.exe -test.v
