# Holocron cheat sheet

One page of lookups. First time here: [holocron-quickstart.md](holocron-quickstart.md). Every command accepts `--help` (`holocron up --help`, or `holocron help up`); global flags go before the command: `--config FILE`, `--root DIR`, `-v`/`-vv` (more detail for this run), `--json` (where a command has it).

## I want to...

| I want to... | Command |
| --- | --- |
| do the daily check (30 seconds) | `holocron status`, then `holocron archive stats`, then `holocron logs --level warn --since 24h` |
| start / stop / restart everything | `holocron up --detach` / `holocron down` / `holocron restart --detach` |
| start in this window (closing it stops everything) | `holocron up` |
| see status as JSON | `holocron status --json` |
| check everything when in doubt (recording, live view, connections, web, disk, processes) | `holocron doctor` ([health check](#health-check)) |
| only the problems / one group / for a script | `holocron doctor --quiet` / `holocron doctor --group recording` / `holocron doctor --json` |
| add a camera (records at once with `--record`) | `holocron cam add --name Garage --ip 192.168.1.50:8554 --record` |
| remove a camera (recordings stay) | `holocron cam remove Garage` |
| record / stop recording one camera | `holocron cam enable Garage` / `holocron cam disable Garage` |
| record / stop recording all cameras | `holocron cam enable all` / `holocron cam disable all` |
| change a camera's address or stream | `holocron cam set Garage --ip 192.168.1.50 --quality stream2` |
| list cameras / test one (TCP and RTSP OPTIONS, no login) | `holocron cam list` / `holocron cam test Garage` |
| read a config value / change / put back the default | `holocron config get ReserveGB` / `holocron config set ReserveGB 30` / `holocron config unset ReserveGB` |
| check both config files / see every effective value | `holocron config validate` / `holocron config show` |
| switch live view on or off (applied at once) | `holocron live enable` / `holocron live disable` |
| read warnings of the last day | `holocron logs --level warn --since 24h` |
| read one component or one action | `holocron logs --component web --since 1h`, `holocron logs --action camera.add` |
| follow the log | `holocron logs -f` |
| change a log level now (needs the supervisor running) | `holocron log level supervisor debug` (all components: `holocron log level all info`; keep it: add `--save`) |
| archive size, GB per day, days left | `holocron archive stats` |
| preview what the pruner would delete | `holocron prune --dry-run` (deleting: `holocron prune --now --yes`, only with no supervisor running) |
| compare two archives (read only) | `holocron archive compare --old E:\cameras --new D:\holocron-shadow` |
| add / change password / list users | `holocron user add anna`, `holocron user passwd anna`, `holocron user list` (password on standard input) |
| make a view-only user / change a role | `holocron user add bob --role viewer`, `holocron user role bob admin` |
| remove a user / lift a lockout | `holocron user remove bob`, `holocron user unlock bob` |
| show the CA to install on a phone / export it | `holocron tls ca` / `holocron tls print --out ca.crt` |
| store the camera login | `holocron secret set camera.user`, `holocron secret set camera.password` |
| take the login from the PowerShell system | `holocron secret import --from C:\old\camera_creds.xml` |
| list stored secret names (never values) | `holocron secret list` |
| start at logon: install / status / off / on / remove | `holocron install`, `holocron autostart status`, `holocron autostart disable`, `holocron autostart enable`, `holocron uninstall` |
| see what install would do | `holocron install --dry-run` |
| open the web app / print its address | `holocron open` / `holocron open --print` |
| test the alert channels | `holocron alert test` |
| see the version | `holocron version` |

Names with a space need quotes: `holocron cam enable "Front Door"`. `--since` takes `10m`, `24h` or a time like `2026-10-22T09:00:00+03:00`, not `1d`.

## Health check

`holocron doctor` is read-only: it starts, stops and restarts nothing, opens no camera session beyond RTSP OPTIONS and writes only two probe files it removes again. Every line is `ok`, `info`, `warn` or `fail`; every `warn` and `fail` has a `fix:` line under it with the command or the edit. The last line is `HEALTHY`, `DEGRADED (n warning(s))`, `BROKEN (n failure(s))` or `NOT RUNNING` (then each runtime group says `not checked: nothing is running`). Exit 1 when any line is `fail`, else 0.

| Group | Checks |
| --- | --- |
| `setup` | configuration, ffmpeg, MediaMTX, ports, folders, archive root, clock, camera login, status, autostart, the PowerShell tasks (`--setup-only` prints only these, as before W19) |
| `recording` | heartbeat age, each camera alive, input `relay` (`direct` warns), restarts in the last hour (warn 3, fail 10), the newest segment GROWS on disk in 2 s, gaps above 2 s in the last 24 h, the newest closed segment against the median rate of 2 h (`--deep`: ffprobe it for video and `pcm_alaw` audio) |
| `connections` | each camera answers TCP and RTSP OPTIONS (as `cam test`), the relay serves every camera's path (with its readers), the expected listeners answer |
| `live` | the live instance alive, the web role's live-secret login accepted by it (the 22 Oct 502), every camera's live paths |
| `web` | the web role alive, `GET /api/status` on loopback (control path), HTTPS handshake, certificate days left, the CA's fingerprint, a login user, web errors of the last hour |
| `storage` | free space against the reserve (at the reserve edge is normal), the pruner's last run (blocked fails, circuit breaker warns), the ownership marker, segments per camera in 24 h |
| `system` | stray or orphan processes, the old system's supervisor, two supervisors, autostart runs this executable with this config, log errors of 24 h, alert channels, version, `HOLOCRON_SANDBOX_ONLY` / camera login variables in this window |

`--json` prints one document: `schema` 1, `root`, `groups`, `checks` (`id`, `group`, `level`, `title`, `detail`, `fix`, `data`) and `summary` (`verdict`, `running`, counts, `elapsedMs`).

## Files and folders

| What | Where | Written by |
| --- | --- | --- |
| program and configuration | `holocron.exe`, `holocron.config.json`, `cameras.json` in one folder (Linux: `~/.config/holocron`; macOS: `~/Library/Application Support/holocron`); `--config FILE` overrides | you; `cam` and `config set` edit them atomically |
| status (single writer) | `<root>\_state\status.json` | the supervisor, every 5 s |
| control API address and key | `<root>\_state\ctl.addr`, `ctl.secret` (owner only) | the supervisor while it runs |
| one-supervisor lock, stop request | `<root>\_state\supervisor.lock`, `stop.request` | the supervisor; `down` |
| process records for `down` | `<root>\_state\host.json`, `children.json`, `webchild.json` | host, supervisor, host |
| ownership marker (pruner deletes only here) | `<root>\_state\archive-owner.json` | the supervisor claims the root |
| autostart record | `<config folder>\_state\install.json` | `holocron install` |
| logs | `<root>\_logs\holocron.log` (+ `.1`), `audit.log` | every role and command |
| recordings | `<root>\<Camera_Name>\<Camera_Name>_YYYYMMDD_HHMMSS+0300.mkv` | ffmpeg, 15-minute segments |
| web users, sessions, relay and live secrets | `%LOCALAPPDATA%\IPCCTV\camui\auth` (`users.json`, `sessions.json`, `relay_api_secret.txt`, `live_view_secret.txt`) | web role, supervisor; never open or copy them |
| local CA and server certificate | `%LOCALAPPDATA%\IPCCTV\tls` (`ca.crt`, `ca.key`, `leaf.crt`, `leaf.key`) | `tls ca`, first HTTPS start |
| camera login | Windows Credential Manager (`holocron/camera.user`, `holocron/camera.password`); file backend: `secrets.enc` + `secrets.key` in the auth folder | `secret set`, `secret import` |
| relay and live MediaMTX config | `RestreamerConfigPath`, `LiveConfigPath` (default `C:\mediamtx\mediamtx.yml`, `mediamtx-live.yml`) | the supervisor generates them |

## Ports

| Port | What | Key |
| --- | --- | --- |
| 8787 TCP | web app, loopback only, no sign-in | `web.listen` |
| 8443 TCP | web app over HTTPS on the LAN, sign-in; off until set | `web.tlsListen` (for example `192.168.1.50:8443`) |
| 8554 TCP | relay RTSP (cameras served once, recorders read it) | `RelayBase` |
| 9997 TCP | relay control API, loopback | `RelayApiBase` |
| 8889 TCP | live view WHEP, loopback | `LiveWhepAddress` |
| 8189 UDP | live view media | `LiveUdpAddress` |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | ok (`status`: running) |
| 1 | refused, blocked or failed: `NOT RECORDING`, survivors after `down`, a `doctor` fail, a refused `config set`, `status`: not running (no status file, stale heartbeat, stopped), `prune` refused, `archive compare` found differences |
| 2 | usage: unknown command, bad flag, missing input, sandbox guard |
| 20 | another supervisor holds the archive (not restarted) |
| 21 | configuration fault: no camera login, ffmpeg older than 5.0 (alert 102) |

## Alerts

| Id | Level | Action |
| --- | --- | --- |
| 100 | Error | supervisor stopped unexpectedly, being restarted: note it; a second one in a day is a rollback trigger |
| 101 | Warning | recording again: nothing |
| 102 | Error | restart budget spent (5 in 30 min) or configuration fault, it STAYS DOWN: `holocron logs --level warn --since 1h`, fix, `holocron up --detach`, or roll back |
| 103 | Warning | the console window was closed: `holocron up --detach` |
| 104 | Error | the web app crashed 5 times in 30 minutes, recording unaffected: `holocron logs --component web` |
| 110 | Warning | a camera has made no file progress for about 15.6 minutes: `holocron cam test "<name>"` |
| 120 | Error | pruning disabled, the root is unsafe: `holocron doctor`, `holocron prune --dry-run` |
| 121 | Error | free space below the reserve and nothing deletable: free disk, or lower `ReserveGB` |
| 122 | Error | the age rule's circuit breaker held a deletion: check `MaxRetentionDays` |
| 130 | Error | the relay refused Holocron's login: `holocron restart --detach` |

## The ten likeliest problems

Whatever the symptom, `holocron doctor --quiet` first: it prints each problem with its fix.

| Symptom | Fix |
| --- | --- |
| Live tiles stay "Connecting" / 502 | the supervisor and the web role read different live secrets (fixed in W17): update the build; on an old one set `LiveSecretPath` to the supervisor's file and `holocron restart --detach` |
| `already running: supervisor pid ...` or exit 20 and you think nothing runs | the old PowerShell system (or another Holocron) still holds the root: `.\cam.ps1 -Stop` / `holocron down`, then check ports ([cutover.md](cutover.md#c5-prove-nothing-of-the-old-system-is-left)) |
| `web app: not answering on 127.0.0.1:8787`, `doctor` warns port 8787 held | the old `camui.exe` runs: stop it by pid. `curl` from WSL never reaches it (its loopback is its own): use a Windows browser |
| `refused by the sandbox guard`, exit 2 | `HOLOCRON_SANDBOX_ONLY` is set in this window: `Remove-Item Env:HOLOCRON_SANDBOX_ONLY` |
| Defender blocks `holocron.exe` | change nothing in Defender; read Protection history, then [cutover.md](cutover.md#p3-the-program-in-a-permanent-folder) and [autostart.md](autostart.md#known-issue-microsoft-defender) |
| a camera is `DOWN` / alert 110 | `holocron cam test "<name>"`; check power and network; the supervisor keeps retrying; cameras serve only two RTSP sessions |
| disk below the reserve / alert 121 | `holocron archive stats`, `holocron prune --dry-run` for the reason; free space; lowering `ReserveGB` (`holocron config set`, then `holocron restart --detach`) is a decision, not a fix |
| `NOT RECORDING`, exit 21 | no camera login (`holocron secret set camera.user` and `camera.password`, or `secret import`), or ffmpeg older than 5.0. `HOLOCRON_CAMERA_USER` / `HOLOCRON_CAMERA_PASSWORD` in the environment win over the stored login: `dir Env:HOLOCRON*` |
| nothing records after you closed the window | alert 103: the visible-window logon task is the system: `holocron up --detach`, or `holocron install --window hidden` |
| `install` refuses | "transient": run `holocron.exe` from a permanent folder; "old tasks are enabled": disable `IPCCTV-*` first, never both systems |

## Rollback in four lines

Full procedure and the rehearsal: [rollback.md](rollback.md).

1. `holocron down` (expect `stopped: 0 survivors`), then `holocron autostart disable`.
2. In the old folder, start the old system as you wrote down in cutover P7 (this machine: `.\cam.recorder.ps1 -Nvr`, then `camui.exe`; not `.\cam.ps1 -Start`, see [rollback.md](rollback.md#r5-start-the-old-system)).
3. `.\cam.ps1 -Status` shows the supervisor, the ffmpeg children, the relay and the UI.
4. Leave the footage and the Holocron files where they are; both systems never record at once.
