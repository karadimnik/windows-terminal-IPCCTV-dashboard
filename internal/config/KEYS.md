# holocron.config.json keys

Source: `Get-CamConfigDefault` (cam.common.ps1) and inventory IR section 4. A test (`TestDefaultsMatchPowerShellSource`) reads the
PowerShell function and fails if a key is neither kept nor listed as dropped, or if a scalar default differs (Windows values).
Defaults shown are the PowerShell/Windows ones; `Defaults(p)` uses `p.Dirs()`/PATH elsewhere (root `/srv/cameras`, `ffmpeg`, `mediamtx`).

Types are strict: a JSON string is never accepted for a boolean or a number (PowerShell casts them; IR 10 item 9). `null` is accepted only for
`MaxRetentionDays`. Rules marked **Go** are additions the PowerShell loader did not check (it failed at first use instead).

## Kept

| Key | Default | Rule | Why kept | Editable |
|---|---|---|---|---|
| RootPath | `E:\cameras` (`/srv/cameras`) | non-blank string without `%` | archive root | no |
| FfmpegPath | `C:\ffmpeg\bin\ffmpeg.exe` (`ffmpeg`) | string; empty = look up on PATH | recorder | no |
| FfprobePath | `C:\ffmpeg\bin\ffprobe.exe` (`ffprobe`) | string | kept for diagnostics; cheap | no |
| SegmentSeconds | 900 | whole number >= 1 | segment length | yes |
| Container | `mkv` | must name an entry of ContainerProfiles (**Go**) | container choice | no |
| ContainerProfiles | mkv + fmp4 | object; the file's map REPLACES the defaults; each: Extension `.` + 1-8 alnum (**Go**: it decides what the pruner may delete), SegmentFormat non-blank, AudioArgs/ExtraArgs string arrays, SegmentFormatOptions string | config-driven container | no |
| RtspTransport | `tcp` | one of tcp, udp, udp_multicast, http, https (**Go**) | ffmpeg input | no |
| TimeoutMicroseconds | 10000000 | whole number >= 1 (**Go**) | ffmpeg input | no |
| ReserveGB | 20 | number >= 0 (**Go**) | pruner watermark | yes |
| PruneHysteresisGB | 5 | number >= 0 (**Go**) | pruner target | yes |
| PruneIntervalSeconds | 300 | number > 0 (**Go**) | pruner cycle | no |
| MaxRetentionDays | null | null (off) or number > 0 (**Go**: PowerShell treated <= 0 as off) | age rule | yes |
| MaxAgePruneFraction | 0.5 | number 0.01-1.0 (PowerShell also parsed a numeric string; Go wants a number) | age-rule breaker | yes |
| ReconcileSeconds, StatusIntervalSeconds, ArchiveScanSeconds | 5, 5, 60 | number > 0 (**Go**) | supervisor loops | no |
| CameraDownAlertMinutes, CameraDownRepeatHours, StopTimeoutSeconds | 15, 12, 30 | number > 0 (**Go**) | alerts, stop wait | yes |
| BackoffStartSeconds, BackoffMaxSeconds, HealthyResetSeconds | 3, 60, 300 | whole number >= 1; Max >= Start (**Go**) | restart backoff | no |
| StallFactor | 2.5 | number > 1 | stall watchdog | no |
| LogMaxBytes | 2097152 | whole number >= 1 (**Go**) | log rotation | no |
| LogRepeatSuppressSeconds | 60 | number >= 0 (**Go**) | repeat collapse | no |
| StatusEventCount | 20 | whole number >= 0 (**Go**) | events in status.json | no |
| RestreamerEnabled | true | JSON boolean | relay on/off | no |
| RestreamerExePath, RestreamerConfigPath | `C:\mediamtx\...` | string | relay | no |
| RelayBase, RelayApiBase | `rtsp://127.0.0.1:8554`, `http://127.0.0.1:9997` | string; if set, needs a host:port | relay | no |
| RelayProbeTimeoutMs | 1500 | whole number; below 100 is raised to 100 with a warning (PowerShell clamped at use) | relay probe | no |
| RelayStartupWaitSeconds | 20 | whole number >= 0 (**Go**) | relay wait | no |
| RelayReadyWaitSeconds, StartStaggerMs | 15, 300 | whole number; below 0 is raised to 0 with a warning | relay wait, stagger | no |
| LiveViewEnabled | false | JSON boolean | live view | yes |
| LiveExePath, LiveSecretPath | `` | string, not starting with `\\` | live view | no |
| LiveConfigPath | `C:\mediamtx\mediamtx-live.yml` | non-blank, not starting with `\\` | live view | no |
| LiveWhepAddress | `127.0.0.1:8889` | `127.0.0.1:<1-65535>`; not the relay's host:port | live view | no |
| LiveUdpAddress | `127.0.0.1:8189` | `127.0.0.1`, `0.0.0.0` or one private IPv4, port 1-65535 | live view | yes |
| LiveAdvertiseHosts | `[]` | <= 4 unique private IPv4 literals; consistency with LiveUdpAddress | live view | yes |
| LiveSubStream | `stream2` | `^[A-Za-z0-9_-]{1,32}$` | live view | yes |
| LiveCloseAfterSeconds | 10 | whole number 1-3600 | live view | yes |
| logging.level | `info` | ERROR, WARN, INFO, DEBUG or TRACE (any case) | NEW in holocron (LLD 2.13): the default level | yes |
| logging.levels | `{}` | object component to level, each as logging.level; `holocron config set logging.levels.<component> <level>` | NEW: per-component levels | yes |
| alerts.queueSize | 256 | whole number >= 1 | NEW (internal/alerts): queue per channel | yes |
| alerts.timeoutSeconds | 10 | number > 0 | NEW: one delivery attempt | yes |
| alerts.minRepeatSeconds | 60 | number > 0 | NEW: dedupe window | yes |
| alerts.retryBackoffSeconds | `[1, 5, 30]` | array of numbers > 0; `[]` = no retries | NEW: network channel retries | yes |
| alerts.webhook.enabled, .url, .secretName, .secretHeader | false, "", "", `X-Holocron-Secret` | url: https, or http to a loopback/private address (checked whenever set); enabled needs a url; the rest strings | NEW: webhook channel | yes |
| alerts.push.enabled, .server, .topic, .tokenName | false, `https://ntfy.sh`, "", "" | topic `[A-Za-z0-9_-]{1,64}` required when enabled (checked whenever set); server rule as the webhook url | NEW: ntfy-style push | yes |
| alerts.desktop.enabled | true | JSON boolean | NEW: desktop notification | yes |
| alerts.eventlog.enabled | true | JSON boolean | NEW: Windows event log / syslog | yes |
| secrets.backend | `auto` | one of auto, keyring, file, env | NEW: camera credential store (internal/secrets) | yes |
| secrets.file, secrets.keyFile | `` | strings; local paths (not starting with `\\`, no control characters); empty = `secrets.enc` / `secrets.key` in the auth folder | NEW: the file backend's encrypted store and its owner-only key file (the key may come from `HOLOCRON_SECRET_KEY` instead); changing one does not move a stored login | yes |
| web.enabled | true | JSON boolean | NEW: `holocron up` starts the web role (false: recorder only) | yes |
| web.listen | `127.0.0.1:8787` | host:port, port 1-65535; the web app's loopback guard decides at start (a non-loopback address needs CAMUI_TOKEN of 16+ characters) | NEW: the web app's listener | no |
| web.tlsListen | `` | empty (off) or IPv4:port, port 1-65535; the web app requires a private address and at least one user | NEW: the LAN HTTPS listener (login) | no |
| web.tlsDir, web.authDir | `` | strings; empty = the web app's default folders under `%LOCALAPPDATA%\IPCCTV` (never inside the repository) | NEW: local CA and certificate; users.json, sessions.json | no |

Editable = the allow-list of `holocron config set|unset` (rule R1); the other keys are changed by hand or by the install.
Sub-keys of `logging`, `alerts`, `secrets` and `web` are camelCase like the JSON; an unknown sub-key is a warning.

An unknown key starting with `Live` (any case) is an ERROR (a misspelled `LiveViewEnabled` would otherwise leave live view off silently), as
in PowerShell. Any other unknown key is a warning. Key names are case-insensitive; two spellings of one key are an error.

## Dropped (accepted and ignored without a warning, so an existing file loads unchanged)

| Key | Default | Why dropped |
|---|---|---|
| HealthStripHeightPx | -1 | dashboard layout; read only by the PowerShell health strip and camui |
| HealthStripMinPx | 120 | same |
| GridGutterPx | 8 | same |
| VideoAspectWidth, VideoAspectHeight | 16, 9 | same |
| HealthCellWidthPx, HealthCellHeightPx | 9, 20 | same |
| HealthSections | status, cameras, events, archive | same |
| HealthEventRows | 5 | same |
| RelayReadyPollMs | 500 | read only by the mpv panes (PowerShell-only viewer) |
| ViewerPromoteCheckSeconds | 10 | same |

Not preserved on rewrite: this package only reads `holocron.config.json`; the `holocron config set` task must edit the document in place
(it needs to keep dropped and unknown keys).

# cameras.json

Array of objects. `Name`, `IP`, `Quality` (strings; blank/absent Quality = `stream1`), `Record` (JSON boolean; absent = off). Every other field is
kept, in place, across `Save`. Types are checked at load (all problems at once, with `cameras[i].Field` paths); `Validate` checks the meaning
(a name that `archive.SafeName` refuses, an address, a stream name, folder collisions). The web app's extra limits (48-character names, a required
stream) stay in the web layer. `Save` is atomic (temp file in the same folder, fsync, rename) and writes a UTF-8 BOM exactly when the file on
disk has one (a new file: when the list was read from a file that had one).
