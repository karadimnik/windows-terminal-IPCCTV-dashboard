# alerts

The alert path (LLD 2.7, gap L7): the event model, a dispatcher whose `Raise` never blocks the recorder, and the delivery
channels `log`, `webhook`, `push`, `desktop`, `eventlog`. Standard library plus `internal/clock` and `internal/logx`; no
third-party dependency (the Windows event log uses `syscall` + advapi32, so `golang.org/x/sys` was NOT needed).

## API

    type Severity int  // Info, Warning, Error; String() "Info"|"Warning"|"Error"; ParseSeverity(string) (unknown = Error)
    type Event struct{ ID int; Severity Severity; Title, Message, Camera, Key string; At time.Time }
    const IDSupervisorStopped=100 ... IDRelayLoginRefused=130, IDTest=0; IDWebStopped=104 (constant only: raised by the host for the web child, Describe/Meanings do not list it yet); Describe(id) string; Meanings() []Meaning
    type Eventer interface{ AlertEvent() Event }                       // producers implement this later
    func FromFields(id int, severity, title, camera, message, key string) Event   // adapter for the string-typed producer structs

    type Channel interface{ Name() string; Send(ctx, Event) error; Enabled() bool }
    type Networked interface{ Network() bool }                         // retried with backoff (webhook, push)
    type SecretLookup func(name string) (string, bool)
    func BuildChannels(Config, Deps{Log, Secrets, Host}) ([]Channel, []error)     // webhook, push, desktop, eventlog

    func New(Config, []Channel, *slog.Logger) *Dispatcher
    func NewWith(Config, []Channel, *slog.Logger, ...Option) *Dispatcher           // WithClock, WithRedactor
    (d) Raise(Event); RaiseFrom(Eventer)               // return at once; never block, panic or error
    (d) Test(ctx, channel string) []Result             // "" = all; Result{Channel, Enabled, Err, Duration}
    (d) Close(ctx) error                               // flush within the deadline

Wiring: `d := alerts.NewWith(cfg, chans, lg.For("alerts"), alerts.WithRedactor(lg.Redactor().String))`.
Adapters (the callers write them, this package does not import the producers):
`supervise.Event` and `prune.Alert{ID, Level, Title, Body}` become `alerts.FromFields(e.ID, e.Severity, e.Title, e.Camera, e.Message, e.Key)` and
`alerts.FromFields(a.ID, a.Level, a.Title, "", a.Body, "prune:"+strconv.Itoa(a.ID))`, or implement `AlertEvent() Event` on them.

## Behaviour

- **Queue**: one bounded queue (default 256) and one worker goroutine per enabled channel. A full queue drops the OLDEST event of
  that channel with one WARN notice per episode (`alert queue full`); when the queue drains an INFO line gives the count.
- **Isolation**: a channel's `Send` runs under a per-channel timeout (default 10 s) on its own goroutine; a hung or panicking
  channel is reported as a failed delivery and delays no other channel. (A `Send` that ignores its context leaks its goroutine
  until it returns; the shipped channels all honour it.)
- **Retries**: only `Networked` channels. After the first failure the waits are 1 s, 5 s, 30 s (so up to 4 attempts), then the event
  is dropped with an ERROR. Other channels get one attempt and a WARN. `Close` skips the waits.
- **Dedupe**: an event with a `Key` identical to one raised less than `MinRepeat` (60 s) ago is dropped (DEBUG `result=deduped`).
  No Key, no dedupe. Per-episode rules of IR 6 stay in the producers.
- **Redaction**: with `WithRedactor`, Title, Message and Camera are masked before any channel or log sees them (as
  `Get-CamRedacted` did). Errors from the HTTP channels never contain the URL path, query or userinfo (`https://host/...`).
- **Logs** (component `alerts`): `action=alert.raise result=ok|deduped|closed` (INFO/DEBUG), and per delivery
  `action=alert.deliver target=<channel> result=ok|fail duration_ms=N` (INFO ok, WARN failed, ERROR after the final retry). The
  `log` channel's own record is `ALERT {title} - {message}` at ERROR/WARN/INFO by severity with `audit=true` (the PowerShell
  product's `ALERT` line). Note: a logx record costs about 25 us (0.4 ms under -race) because redaction runs before the level filter.
- **Test**: `Test` sends `TEST - Holocron alert test, ignore` straight to the channel(s): no queue, no dedupe, no retry, per-channel result.
- `Close`: stops accepting events, gives every queued event one attempt; if `ctx` ends first, in-flight sends are cancelled, the
  rest is dropped with a WARN, and `ctx.Err()` is returned. Idempotent. After `Close`, `Raise` is a no-op.

## Channels

| name | needs | notes |
|---|---|---|
| `log` | nothing | always on, added by `New` |
| `webhook` | `alerts.webhook.url` | JSON POST `{id, severity, title, message, camera, at (RFC 3339 UTC), host}`, `Content-Type: application/json`; https, or http only for loopback, private or link-local IPs and `localhost`; redirects are not followed; optional shared secret from the secrets store sent in header `X-Holocron-Secret` (or `secretHeader`); a configured secret that is missing is a failed delivery, never an unauthenticated POST |
| `push` | `alerts.push.topic` | ntfy-style: POST `<server>/<topic>`, headers `Title` (RFC 2047 when not ASCII), `Priority` (Error 4 high, Warning 3 default, Info 2 low), `Tags` (`rotating_light`/`warning`/`information_source`, `holocron`, `alert-<id>`), body = the message; optional `Authorization: Bearer <token>` from the secrets store. Reaches a phone running the ntfy app (Android, iOS) with a self-hosted ntfy server or ntfy.sh, no account and no dependency. On ntfy.sh the topic name is the only access control: make it long and random |
| `desktop` | the OS tool | Windows: a WinRT toast under the Windows PowerShell AUMID, the same mechanism as `Show-CamToast`; it needs `powershell.exe`, so **this adapter is the only place Holocron uses PowerShell** - only if present (PATH, then `%SystemRoot%\System32\WindowsPowerShell\v1.0`), `-NoProfile -NonInteractive -NoLogo -WindowStyle Hidden -EncodedCommand`, `CREATE_NO_WINDOW`, never elevated, hard 10 s timeout, alert text passed as base64 inside the script (no parsing of alert text as script). Linux: `notify-send --app-name=Holocron --urgency=... --icon=... -- title body` when on PATH (needs a desktop session bus; a headless host with libnotify installed will log a WARN per alert - set `desktop.enabled=false` there). macOS: `osascript` with title and body as run-handler arguments (compiled and command-line tested only, not run on a Mac). Otherwise `Enabled()=false` and a DEBUG line |
| `eventlog` | nothing | Windows: Application log, source `IPCCTV` if registered else `Application` (unelevated), text `IPCCTV: {title} - {message}`, event id = alert id, type by severity; the same technique as `Write-CamEventLogEntry` through advapi32 `ReportEventW`. Linux/macOS: `log/syslog` tag `holocron` (journald receives it), text `IPCCTV: {title} - {message} [alert N]`. If the log cannot be opened: one WARN and the channel disables itself for the run |

## Config keys needed (`internal/config` does not know them yet; `alerts.Config` has the JSON tags)

Object `alerts` in `holocron.config.json`, all optional, camelCase like `logging`:

| key | default | rule |
|---|---|---|
| `alerts.queueSize` | 256 | integer >= 1 (<= 0 means default) |
| `alerts.timeoutSeconds` | 10 | number > 0 |
| `alerts.minRepeatSeconds` | 60 | number > 0 |
| `alerts.retryBackoffSeconds` | `[1, 5, 30]` | array of numbers > 0; `[]` = no retries |
| `alerts.webhook.enabled`, `.url`, `.secretName`, `.secretHeader` | false, "", "", `X-Holocron-Secret` | url: https, or http to a private address; enabled needs a url |
| `alerts.push.enabled`, `.server`, `.topic`, `.tokenName` | false, `https://ntfy.sh`, "", "" | topic `[A-Za-z0-9_-]{1,64}` required when enabled; server rule as the webhook url |
| `alerts.desktop.enabled` | true | boolean |
| `alerts.eventlog.enabled` | true | boolean |

Secret names (`secretName`, `tokenName`) are looked up through `SecretLookup` at every send, so a rotated secret applies at once.

## Tests

`go test -race ./internal/alerts`: table tests per channel against fake servers on loopback ports 18680-18699 (200, 404, 500,
redirect, timeout, refused, missing secret), command-line builders for desktop (no toast is shown), a fake event-log writer,
queue overflow, isolation, 10,000 raises with a stalled channel, retries on a fake clock, dedupe window, redaction with positive
controls, `Close` flush and deadline, goroutine counts. Live Windows checks (`live_windows_test.go`) run only with
`HOLOCRON_LIVE=eventlog|toast` (and `WSLENV=HOLOCRON_LIVE` when started from WSL).
