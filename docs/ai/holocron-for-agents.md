# Holocron for agents

Self-contained reference for an AI agent working on, or operating, this repository with no memory of the project. Facts carry a file path; "UNVERIFIED" marks what nobody has run. Written 2026-10-22 against branch `portable-nvr`. Operator-facing pages: [quickstart](../holocron-quickstart.md), [cheat sheet](../holocron-cheatsheet.md). Repository rules for agents: [AGENTS.md](../../AGENTS.md).

## 1. What Holocron is

- A network video recorder in Go: records RTSP cameras with ffmpeg's segment muxer into one folder per camera, serves a web app (login, Live grid, Footage, Settings, Admin), supervises its own processes, alerts, prunes by free space. Go module `holocron`, `go 1.27`, no third-party dependency (`go.mod`).
- One executable, `holocron` (`holocron.exe` on Windows), built from `cmd/holocron`. `./build-holocron.sh` runs gofmt, vet, tests, then cross-compiles `holocron.exe` into the repository root (gitignored).
- Live since 2026-10-22 on the Operator's Windows PC, replacing the PowerShell system (`cam.ps1`, `cam.recorder.ps1`, `camui.exe`), which stays in the repository as the legacy fallback and is never modified by Holocron ([cutover record](../../tasks/portable-nvr/cutover-plan.md)). The archive layout and file names are unchanged (decision D18, [HLD](../../tasks/portable-nvr/os-independent-hld.md) section 9), so both systems can read the same footage, but never record to one root at once.
- Documents describing `cam.ps1` (install.md, reference.md, how-it-works.md, configuration.md, troubleshooting.md, limitations.md, go-toolchain.md) are the legacy system. Holocron-era documents: [cutover.md](../cutover.md), [rollback.md](../rollback.md), [shadow.md](../shadow.md), [autostart.md](../autostart.md), [ci.md](../ci.md), [ui.md](../ui.md) (web app), [alerts.md](../alerts.md) (ids shared), this folder.
- Branch policy: the work lives on `portable-nvr`; it never reaches `master` ([HLD](../../tasks/portable-nvr/os-independent-hld.md) intro).

## 2. The two hard rules

**R1. One executable, everything managed from it.** No script, second program or helper for any action. Starting and stopping is `holocron up` / `holocron down`; the OS autostart entry runs `holocron up` and nothing else. The internal roles (host, supervisor, web) are hidden subcommands that `holocron` starts as its own children, kept as separate processes so a fault on the LAN-facing web page cannot stop a recording. Every web action has the same command and both call the same code (`cmd/holocron/webadmin.go`, `cmd/holocron/settings.go`).

**R2. Levelled logging for every action.** Every command, control-API handler and web handler is a `cli.Action` run by `cli.Runner`, which writes the begin record (DEBUG) and one result record (INFO mutating ok, DEBUG read-only ok, WARN refused, ERROR failed) with action, actor, target, action id, pid and duration. Levels ERROR, WARN, INFO (default), DEBUG, TRACE per component, changed at run time by `holocron log level`. Redaction sits in the log handler below every level. Enforced by `TestEveryCommandIsAnAction` (`cmd/holocron/cmd_test.go`), `TestEveryRouteIsAnAction` (`internal/ctlapi/ctlapi_test.go`), `TestEveryRouteIsClassified` (`internal/web/readonly_test.go`), `TestEveryAdminRouteIsAnAuditedAction` (`internal/web/admin_api_test.go`). Source: [HLD](../../tasks/portable-nvr/os-independent-hld.md) section 0, [logx](../../internal/logx/README.md), [cli](../../internal/cli/doc.go).

## 3. Architecture

```
operator, or the OS autostart entry  --->  holocron up [--detach]
  HOST role (hidden: holocron host)            internal/host, internal/app/host.go
   |  restarts the supervisor in place (5 per 30 min), classifies every stop from recorded state,
   |  raises alerts 100-103; writes _state/host.json; exits with the supervisor's typed code
   |-- WEB role (hidden: holocron web)         internal/web; restarted by the host after 5/10/20/40/60 s, alert 104 at 5 crashes in 30 min
   |     stateless: reads status.json, changes things only through the control API; listens web.listen (loopback) and web.tlsListen (LAN, login)
   '-- SUPERVISOR role (hidden: holocron supervisor)   internal/supervise
        |-- OS instance lock _state/supervisor.lock (second supervisor: exit 20)
        |-- ffmpeg x N          one per recording camera, segment muxer, reads the relay path (direct input as fallback)   internal/ffmpeg
        |-- MediaMTX relay      serves each camera once (RTSP :8554, API :9997)                                           internal/relay
        |-- MediaMTX live       WHEP :8889, media UDP :8189, only when LiveViewEnabled
        |-- pruner schedule     internal/prune;  alerts: internal/alerts
        |-- status.json         written ONLY by the supervisor while it runs (atomic replace)                             internal/status
        '-- control API         127.0.0.1:<random port> in _state/ctl.addr, key in _state/ctl.secret, header X-Holocron-Key   internal/ctlapi
short-lived: holocron <command>   reads files and status.json; mutates through the control API when a supervisor answers, else through files
```

Why separate processes and one writer: [HLD](../../tasks/portable-nvr/os-independent-hld.md) section 3. Process-lifetime mechanics per OS: section 10 below.

## 4. Package map

`cmd/*` and `internal/*`; each links its README (or `doc.go` where there is none).

| Package | One line |
| --- | --- |
| [cmd/holocron](../../cmd/holocron/README.md) | the command tree, flag parsing, the registry `commands` (`commands.go`), output and exit codes |
| [cmd/camui](../../cmd/camui/main.go) | four-line main of the legacy stand-alone web app; the code is `internal/web` |
| [internal/alerts](../../internal/alerts/README.md) | alert events, non-blocking dispatcher, channels log, webhook, push, desktop, eventlog |
| [internal/app](../../internal/app/README.md) | production wiring: paths, config resolve and edit helpers, census, `Down`, `RunHost`, `Running` |
| [internal/archive](../../internal/archive/README.md) | safe names, segment names, scans, root guards and the ownership marker |
| [internal/autostart](../../internal/autostart/README.md) | logon task, Windows service, systemd user unit, launchd agent; install record |
| [internal/cli](../../internal/cli/doc.go) | the action contract: `Action`, `Registry`, `Runner`, `AssertAllExposedAreActions` |
| [internal/clock](../../internal/clock/README.md) | the one time seam: UTC wall clock plus monotonic intervals, fakes |
| [internal/config](../../internal/config/README.md) | types, defaults and validation of `holocron.config.json` and `cameras.json` ([KEYS.md](../../internal/config/KEYS.md)) |
| [internal/ctlapi](../../internal/ctlapi/README.md) | the supervisor's loopback control endpoint and its client |
| [internal/ffmpeg](../../internal/ffmpeg/README.md) | recording argument vector (golden), version check (5.0+), child process, redacted output |
| [internal/golden](../../internal/golden/doc.go) | tests that load `testdata/golden` (generated from the PowerShell product) |
| [internal/host](../../internal/host/README.md) | supervisor restart budget, stop classification, window-close handling, auxiliary children |
| [internal/logx](../../internal/logx/README.md) | levelled structured logging on `log/slog`, rotation, redaction |
| [internal/osx](../../internal/osx/README.md) | everything per OS: scopes, instance lock, child registry, console events, service manager, default folders |
| [internal/prune](../../internal/prune/README.md) | pure planner and guarded executor of the archive pruner |
| [internal/relay](../../internal/relay/README.md) | both MediaMTX configurations, per-camera source environment, secrets, control-API client |
| [internal/secrets](../../internal/secrets/README.md) | camera login and named secrets: keyring, encrypted file, environment backends |
| [internal/shadow](../../internal/shadow/doc.go) | `archive compare`: two archives equal within tolerances |
| [internal/status](../../internal/status/README.md) | the `status.json` contract: schema, atomic writer, share-delete reader, liveness verdict |
| [internal/supervise](../../internal/supervise/README.md) | the supervisor loop, per-camera manager, ordered stop, pruner schedule |
| [internal/web](../../internal/web/README.md) | the web app: auth, roles, TLS, Live proxy, Footage, Settings, Admin |

## 5. Commands

Registry: the slice `commands` in `cmd/holocron/commands.go`; `holocron --help` prints it. The table is generated from that slice and `TestAgentDocCommandTable` (`cmd/holocron/docs_test.go`) fails when it drifts; regenerate with `HOLOCRON_DOCS_UPDATE=1 go test ./cmd/holocron -run TestAgentDocCommandTable`. Global flags before the command: `--root DIR` (overrides `RootPath`), `--config FILE`, `-v`/`-vv`, `--json`. `holocron <command> --help` and `holocron help <command>` print one command's description and usage without running it (`TestEveryCommandHasHelp`). Hidden internal roles, not for direct use: `host`, `supervisor`, `service` (Windows service entry).

<!-- commands:begin (generated from the registry; do not edit by hand) -->
| Command | Does |
| --- | --- |
| `up [--detach] [--only host]` | start everything: host, supervisor, relay, live relay and the web app (attached: closing the terminal stops it; --detach: in the background, waits for an honest start; --only host: no web app) |
| `down` | stop everything in order and prove it (exit 1 and the survivors if anything remains) |
| `restart [--detach] [--only host]` | down, then up |
| `status [--json]` | roles, pids, cameras, disk, prune, live view, alerts; exit 0 running, 1 not |
| `cam list [--json]` | the cameras of cameras.json |
| `cam show <name> [--json]` | one camera and its recorder state |
| `cam add --name N --ip HOST [--quality STREAM] [--record]` | add a camera (validated; no credentials here) |
| `cam remove <name>` | remove a camera (its recordings stay) |
| `cam set <name> [--name N] [--ip HOST] [--quality STREAM] [--record true\|false]` | change a camera |
| `cam enable <name\|all>` | record a camera |
| `cam disable <name\|all>` | stop recording a camera |
| `cam test <name>` | TCP connect and RTSP OPTIONS to the camera (5 s, no login) |
| `config get <key> [--json]` | an effective value |
| `config set <key> <value>` | change an editable key (validated first, written atomically) |
| `config unset <key>` | back to the default |
| `config validate` | check holocron.config.json and cameras.json |
| `config show [--json]` | the effective configuration (secrets masked) |
| `live enable` | switch live view on (applied at once when running) |
| `live disable` | switch live view off |
| `live status [--json]` | live view setting and process |
| `log level <component\|all> <level> [--save]` | change a log level now through the control API (--save: also in the configuration) |
| `logs [-f] [--component C] [--level L] [--since D] [--action A]` | read, filter and follow holocron.log |
| `alert test [channel]` | send a test alert through one or every channel |
| `doctor [--quiet] [--json] [--group G[,G...]] [--setup-only] [--deep]` | the health check: ok/warn/fail lines with a fix for each problem, grouped (setup, recording, connections, live, web, storage, system), then HEALTHY, DEGRADED, BROKEN or NOT RUNNING; exit 1 on any fail; read-only; --setup-only is the set-up preflight alone; --deep also ffprobes the newest closed segments |
| `version` | build and spec versions |
| `help` | this list |
| `web [-listen H:P] [-tls-listen IP:P] [-tls-dir D] [-auth-dir D] [-tls-readonly true\|false] [-config FILE] [-print-ca] [-set-password USER] [-unlock USER] [-version]` | run the web app alone (normally started by up): login page, Live grid, Footage, Settings; camui's flags and config file all work |
| `open [camera] [--print]` | open the Live grid (or one camera) in the default browser; --print only prints the address |
| `user add <name> [--role admin\|viewer] [--auth-dir D]` | add a login user (default role admin; a viewer watches live and footage only); the password is read from standard input |
| `user passwd <name> [--auth-dir D]` | change a user's password (read from standard input) and end that user's sessions |
| `user role <name> admin\|viewer [--auth-dir D]` | change a user's role and end that user's sessions (the last admin cannot be demoted) |
| `user remove <name> [--force] [--auth-dir D]` | remove a user and its sessions (not the last admin while other users remain; the last user only with --force while web.tlsListen is set) |
| `user list [--json] [--auth-dir D]` | names, role and creation time (never hashes) |
| `user unlock <name> [--auth-dir D]` | lift a sign-in lockout in a running web app |
| `tls ca [--json] [--tls-dir D]` | the local CA: file, SHA-256 fingerprint and the steps to install it on a device (created when absent) |
| `tls print [--out FILE] [--tls-dir D]` | write the CA certificate (never a key) to standard output or a file |
| `tls rotate --yes [--tls-dir D]` | delete the CA and server certificate; a fresh pair is made at the next web start and every device must reinstall the CA |
| `secret set <name>` | store a secret read from standard input (camera.user, camera.password, ...) |
| `secret delete <name>` | delete a secret |
| `secret import [--from camera_creds.xml]` | Windows: move the old camera_creds.xml login into the store |
| `secret list [--json]` | secret names (never values) |
| `prune [--dry-run \| --now --yes] [--json]` | show the pruner's decision (default), or run it now when no supervisor is running |
| `archive stats [--json]` | per camera: segments, size, oldest and newest; GB per day, free space, projected days |
| `archive compare --old ROOT --new ROOT [--camera NAME ...] [--since TIME] [--json] [--ffprobe PATH]` | read-only: prove the shadow archive equals the existing one (names, clock boundaries, sizes, ffprobe durations, audio, gaps); exit 0 equal, 1 differences, 2 usage |
| `install [--mode logon\|service\|systemd\|launchd] [--window visible\|hidden] [--dry-run] [--allow-temp] [--i-know-old-tasks-are-enabled] [--yes] [--account A]` | create the autostart entry for this OS (logon task, Windows service, systemd user unit, launchd agent) and verify it; idempotent; never touches the PowerShell system's tasks |
| `uninstall [--dry-run] [--force]` | remove exactly what install created and recorded |
| `autostart enable [--dry-run] [--i-know-old-tasks-are-enabled]` | switch the installed autostart entry on |
| `autostart disable [--dry-run]` | switch it off without removing it |
| `autostart status [--json]` | installed, enabled, what it runs and whether that executable exists; the PowerShell system's tasks (read only) |
<!-- commands:end -->

Exit codes (`cmd/holocron/main.go`, `TestExitCodes`): 0 ok; 1 failure (`NOT RECORDING`, survivors after `down`, a refusal, a failed `doctor`, `status` when not running); 2 usage (also the sandbox guard, except `doctor`, which reports it as a fail and exits 1); 20 another supervisor holds the archive (the host never restarts it); 21 configuration fault (no camera login, ffmpeg older than 5.0). Alert ids: [alerts.md](../alerts.md) (100-103, 110, 120-122, 130) plus 104 (web role crash loop, `internal/alerts/README.md`).

### `holocron doctor` (the health check)

The Admin tab's **Health** section shows the same result (R1 in both directions): `GET /api/admin/health[?group=...]` (admin only; `web.admin.health`, read-only, not audited) returns the `doctor --json` document under `health` plus `checkedAt`. It runs the one `doctorTable` through `buildDoctor` inside the web role (`webAdmin.Health`, `cmd/holocron/doctor_health.go`): concurrent requests share one run, the last result answers for 5 s, a disconnecting browser cancels the run; the `environment` line is `info` ("environment of the web role"), the web role is no stray, the sandbox guard cannot fire.

Run it when in doubt; a `HEALTHY` result means every enabled camera's newest segment grew on disk during the run, the relay serves every camera, live view accepts the web role's login, the web app answers and the pruner keeps up. It is read-only (starts, stops and restarts nothing; RTSP OPTIONS is the only camera contact; writes only the folder probe files, removed again; with the sandbox guard refusing the root it touches nothing of it). Lines are `ok|info|warn|fail <title> <detail>` with a `fix:` under every warn/fail; the summary is `HEALTHY`, `DEGRADED (n warning(s))`, `BROKEN (n failure(s))` or `NOT RUNNING`; exit 1 only on a fail. `--quiet` (problems only), `--group setup|recording|connections|live|web|storage|system` (comma-separated), `--setup-only` (the pre-W19 output), `--deep` (ffprobe the newest closed segments), `--json` (`schema` 1, `root`, `groups`, `checks[]` with `id`, `group`, `level`, `title`, `detail`, `fix`, `data`, and `summary` with `verdict`, `running`, counts, `elapsedMs`).

Check ids (one table, `doctorTable` in `cmd/holocron/doctor*.go`): `setup.configuration`, `.ffmpeg` (below 5.0 or unreadable: fail; a development build: info, not a warning), `.mediamtx`, `.ports`, `.webports`, `.folders`, `.root`, `.clock`, `.credentials`, `.status`, `.autostart` (`setup.sandbox` when the guard refused the root); `recording.heartbeat`, `.camera`, `.growth`, `.gaps`, `.segment`; `connections.camera`, `.relay`, `.port`; `live.process`, `.login`, `.paths`; `web.process`, `.http`, `.tls`, `.users`, `.errors`; `storage.disk`, `.prune`, `.marker`, `.cameras`; `system.processes`, `.autostart`, `.log`, `.alerts`, `.version`, `.env`; `<group>.running` is the not-running notice. Thresholds: heartbeat warn above 3 status intervals, fail above 6 (beyond 3 intervals and 15 s the verdict is not running anyway); restarts warn 3, fail 10 in the last hour; growth sampled every 2 s until the newest segment grows, at most 10 s (the muxer writes in bursts), a closed segment within SegmentSeconds + 60 s excuses no growth (warn); gaps above 2 s; segment rate 0.5-1.5 times the 2 h median; HTTPS leaf warn under 30 days; storage warns below the reserve only after two prune intervals without a run. To add a check: a function `func(ctx, *drun) []check` and one row in the table; seams are `doctorDeps` (clock, sleep, dial, HTTP client, live size, prober, platform folders, process list), replaced in tests through `seams.doctor`; tests never dial a real port (the rig refuses by default).

## 6. Guarantees G1 to G15 and where each is tested

Source of the numbering: [inventory-supervisor.md](../../tasks/portable-nvr/spec/inventory-supervisor.md) section 2.2; the tables for G1 to G8 are in [internal/host/README.md](../../internal/host/README.md).

| Id | Guarantee | Tests |
| --- | --- | --- |
| G1 | closing the terminal stops everything, nothing orphaned | `internal/host`: `TestHostWindowCloseStopsOnceWithoutRestart`, real `TestRealHostWindowCloseEvent`; `internal/osx`: `TestRealWindowsConsoleCloseEvent` |
| G2 | a deliberate close is never repaired | `internal/host`: `TestHostNeverRestartsDeliberateOrRefusedStops` |
| G3 | one alert 103 on a close, none on an orderly stop | `internal/host`: `TestHostOrderlyStopRaisesNoAlert`, `TestHostWindowCloseStopsOnceWithoutRestart` |
| G4 | unexpected stop restarted (100, 101), 5 per 30 min then 102 | `internal/host`: `TestHostRestartsKilledSupervisor`, `TestHostBudgetSpentAtSixthCrash`, `TestHostBudgetRefillsAfterWindow` |
| G5 | supervisor dies however abruptly, its children die | `internal/osx`: `TestRealScopeHardKillLeavesNoSurvivors`, `TestRealScopeHardKillWithGrandchildren`; `internal/host`: `TestRealHostHardKillTakesEverything` |
| G6 | orderly stop in order, no orphan | `internal/supervise`: `TestSupervisorOrderedStop`; `internal/osx`: `TestRealLinuxOrderlyStopKillsGroup` |
| G7 | stop waits and verifies, never kills the supervisor | `internal/host`: `TestHostServiceStopWaitsAndNeverKills` |
| G8 | one supervisor per archive | `internal/osx`: `TestRealLockTwoProcesses`; `internal/supervise`: `TestOSLockOneHolder`; `internal/host`: `TestRealHostAnotherSupervisorNotRestarted` |
| G9 | `down` proves what it stopped (census before, survivors listed, exit 1) | `internal/app`: `TestDownPaths`, `TestDownReapsProvableOrphansWithoutSupervisor`; `cmd/holocron`: `TestDownWithoutAPIUsesTheStopFileAndReportsSurvivors`, opt-in `TestEndToEnd` |
| G10 | start is honest: `NOT RECORDING` and exit 1 unless a fresh heartbeat | `internal/app`: `TestWaitRunningNeedsAFreshHeartbeat`; opt-in `TestEndToEnd` |
| G11 | a start after an orderly stop works; a stale owner record is never trusted | `internal/supervise`: `TestOSScopeReapsThenStartsInside`; `internal/host`: `TestHostWaitsForPreviousLock`; `internal/osx`: `TestReapVerifiesEveryEntry` |
| G12 | the warden disarms when no window appears | not applicable: the Go host replaces the warden (`internal/host/README.md`, "without a warden process"); no test |
| G13 | roles are found by identity (pid, start time, image), never by name | `internal/app`: `TestCensusByIdentity`; `internal/osx`: `TestReapByPidOnlyWouldKillDecoy`, `TestRealRegistryReapSparesDecoy` |
| G14 | one state-changing control action at a time (409) | `internal/ctlapi`: `TestConcurrentMutationsGet409` |
| G15 | logon start is robust | partial: the generated task has `IgnoreNew` and no time limit (`internal/autostart/logon.go`, `TestGeneratedEntriesGolden`); the PowerShell launcher's wait for a mounted path is not ported (the program lives on a local drive) |

G9 to G15 rows are this document's mapping from the test names, not a table kept elsewhere; check a name with `grep -rn "func TestName" internal cmd` before relying on it.

## 7. Configuration model

- Two JSON files side by side: `holocron.config.json` (keys: [internal/config/KEYS.md](../../internal/config/KEYS.md), the authority for every default, rule and whether `config set` may edit it) and `cameras.json` (array of `Name`, `IP` or `host:port`, `Quality` default `stream1`, `Record` default off; other fields are kept, `internal/config/README.md`). A missing config file means all defaults; a file with only `RootPath` is valid (checked 2026-10-22).
- Location: `--config FILE`, else `holocron.config.json` in the program's own folder on Windows, `$XDG_CONFIG_HOME/holocron` on Linux, `~/Library/Application Support/holocron` on macOS (`internal/osx/README.md`). `--root` overrides `RootPath`.
- Types are strict (a JSON string is never a number or boolean); an unknown key is a warning, except one starting with `Live` (an error); key names are case-insensitive. Keys of `logging`, `alerts`, `secrets` and `web` are camelCase.
- Editable allow-list: the `Editable` column of KEYS.md, enforced by `app.EditableKeys` and `TestEditableKeysMatchKeysMd` (`internal/app/app_test.go`): SegmentSeconds, ReserveGB, PruneHysteresisGB, MaxRetentionDays, MaxAgePruneFraction, CameraDownAlertMinutes, CameraDownRepeatHours, StopTimeoutSeconds, LiveViewEnabled, LiveUdpAddress, LiveAdvertiseHosts, LiveSubStream, LiveCloseAfterSeconds, logging.level, logging.levels.*, alerts.*, secrets.*, web.enabled. `RootPath`, binaries, ports (`web.listen`, `web.tlsListen`, `RelayBase`, ...) are edited by hand.
- `config set` validates the whole edited document with `config.Parse`, writes atomically, keeps every other key, the order and a BOM; a running supervisor keeps its values until `holocron restart`, except `LiveViewEnabled` (applied at once) and levels (`holocron log level`). `cam` edits reload the supervisor through `POST /v1/reload`.
- Environment: `HOLOCRON_CAMERA_USER` and `HOLOCRON_CAMERA_PASSWORD` (both set: they win over the store; missing from both is exit 21), `HOLOCRON_LOG` (`debug` or `supervisor=debug,web=info`), `HOLOCRON_SANDBOX_ONLY=1` (refuses a drive-root, a path with a folder named `cameras`, or a root holding PowerShell recorder files; exit 2; `cmd/holocron/README.md`), `HOLOCRON_SECRET_KEY` and `HOLOCRON_SECRET_<NAME>` (secrets file key, env backend; `internal/secrets/README.md`).
- Secrets: `secrets.backend` `auto` is the Windows Credential Manager when it answers, else the encrypted file; Linux and macOS use the file ([secrets](../../internal/secrets/README.md)). `secret import` (Windows) reads the old `camera_creds.xml` through PowerShell's `Import-Clixml`; the file is never parsed in Go.

## 8. State files, writers and readers

`<root>` is `RootPath`. Paths from `internal/app/paths.go`, `internal/osx/README.md`, `internal/ctlapi/README.md`.

| File | Writer | Readers |
| --- | --- | --- |
| `<root>/_state/status.json` | the supervisor, every `StatusIntervalSeconds`, atomically; the host writes the unexpected-stop or window-closed record only while it holds the instance lock and no supervisor runs | `holocron status`, `doctor`, `down`, the web role, the host (`internal/status`) |
| `<root>/_state/ctl.addr`, `ctl.secret` | the control server inside the supervisor (owner only; removed first in the ordered stop) | `ctlapi.Dial` in the CLI and the web role |
| `<root>/_state/supervisor.lock` | the supervisor (OS lock held for its life, owner pid recorded) | the host (waits up to 20 s), `down`, a second supervisor (exit 20) |
| `<root>/_state/stop.request` | `down` when no API answers; the host on a window close or service stop | the supervisor's loop, which removes it |
| `<root>/_state/host.json` | the host (pid, start token, image) | `down`, `status` |
| `<root>/_state/children.json` | the supervisor's scope registry (emptied by its start-up reap) | the census of `down`, the next start's reap |
| `<root>/_state/webchild.json`, `web.stop` | the host (the web child's registry; the stop file the web role watches) | `down`, `status` |
| `<root>/_state/archive-owner.json` | the supervisor claims the root (`internal/archive/marker.go`) | the pruner gates, `doctor` |
| `<config folder>/_state/install.json` | `holocron install` | `uninstall`, `autostart`, `status` |
| `<root>/_logs/holocron.log` (+ `.1`), `audit.log` | every role and command (`logx`, rotation at `LogMaxBytes`) | `holocron logs` |
| `<root>/<Camera_Name>/<Camera_Name>_YYYYMMDD_HHMMSS+hhmm.mkv` | ffmpeg | the pruner, Footage, `archive stats` |
| `holocron.config.json`, `cameras.json` | the operator; `cam`, `config`, the web Settings page through `app.PutCameras` / `app.EditConfigMany` | every role (supervisor re-reads every `ReconcileSeconds`) |
| auth folder: `users.json`, `sessions.json`, `auth.log`, `relay_api_secret.txt`, `live_view_secret.txt` | the web auth service; `user` commands; the supervisor generates the two secrets (`internal/relay/login.go`) | the web role, the supervisor (both resolve the live secret by one rule since W17) |
| tls folder: `ca.crt`, `ca.key`, `leaf.crt`, `leaf.key` | `tls ca`, the web role at first HTTPS start | the web role |

Windows defaults: auth `%LOCALAPPDATA%\IPCCTV\camui\auth`, tls `%LOCALAPPDATA%\IPCCTV\tls`. Never open, print or copy a secret, key or credential file.

## 9. Roles and access classes

Users are `admin` or `viewer` (`holocron user add --role`); a record without a role is an admin. The single table `routeAccess` in `internal/web/access.go` gives every registered route a class; the LAN listener's session gate asks the mux which pattern a request will run, so path tricks are judged as their target; an unclassified route is refused at run time (fail closed).

| Class | Who | Routes |
| --- | --- | --- |
| public | no session | `GET /login`, `/login.js`, `/style.css`, `POST /api/auth/login`, `GET /api/auth/me` |
| viewer | any signed-in user | `GET /`, `POST /api/auth/logout`, `GET /api/status`, `/api/footage`, `/api/clip`, `/api/live`, `POST /api/live/{camera}/whep`, `DELETE /api/live/session/{id}`, `GET /api/tls/ca.crt`, `/api/tls/info` |
| admin | admins only | start/stop (`POST /api/control`), record toggles, acknowledge, `GET`/`PUT /api/cameras` and `/api/config`, every `/api/admin/*` route (config, users, camera login, tls, prune, archive, log level, autostart), `/admin.js`; loopback-only first-run `/setup` |

Rules: the loopback listener (`web.listen`) has no login and counts as admin; the LAN listener (`web.tlsListen`) needs a login, refuses to start with no user, and is view-only by default (`tlsReadOnly`: every change refused 403 even for admins); order of checks: no session 401, role 403, view-only 403, Host/Origin guard; the role is re-read from `users.json` on every request; there is always an admin. Tests: `TestEveryRouteHasAnAccessClass`, `TestRoleMatrixOverEveryRoute` (`internal/web/access_test.go`). Full model: [ui.md](../ui.md#roles-admins-and-viewers).

## 10. Process-lifetime rules per OS

`internal/osx/README.md` and [HLD](../../tasks/portable-nvr/os-independent-hld.md) section 5.

| | Windows | Linux | macOS |
| --- | --- | --- | --- |
| children die with the supervisor | one unnamed job object with `KILL_ON_JOB_CLOSE`, no breakaway; children created suspended, assigned, resumed | `Pdeathsig: SIGKILL` set from one goroutine that locks its OS thread and never returns before `Close`; each child leads its own process group | `Setpgid` only, no kernel guarantee; the start-up reap leaves registry entries alone with a WARN (cannot verify start time and image) |
| attached `up`, terminal closed | console close, logoff, shutdown events become an ordered stop; `StopKind` `window-closed`, alert 103 | `SIGHUP`/`SIGTERM` the same | the same, UNVERIFIED |
| `up --detach` | `DETACHED_PROCESS`, `CREATE_NEW_PROCESS_GROUP`, `CREATE_NO_WINDOW`, breakaway from the terminal's job when allowed | `Setsid` | `Setsid` |
| autostart | logon task `Holocron` (default), optional service | systemd user unit | launchd agent, UNVERIFIED |
| secrets | Credential Manager or file | file | file |

A pid registry applies on every OS: each child's pid, start time and image are written to `children.json`; the next supervisor verifies all three before it stops an orphan (never by name). Launched from WSL interop, the terminal's job refuses breakaway; `up --detach` then says the background copy may end with the terminal.

## 11. Testing

- Everything CI runs, locally (from [ci.md](../ci.md)): `test -z "$(gofmt -l cmd internal)"`; `GOOS=linux|windows|darwin go vet ./...`; `go test -race -count=1 -p 1 ./...` (one package at a time: the web and real-process suites are heavy). `go.mod` pins Go 1.27; on the Operator's WSL it is `~/.local/go/bin` (`build-holocron.sh`).
- Kinds: unit tests with fakes (fake clock, spawner, control client, service manager, status files in `t.TempDir()`); golden files under `testdata/golden` generated from the PowerShell product by `tests/holocron/regen-goldens.ps1` ([README](../../testdata/golden/README.md)); differential prune harness `tests/holocron/prune-diff.ps1` ([prune README](../../internal/prune/README.md)); `status.json` conformance against `internal/status/status.schema.json`.
- Opt-in real-process tests (skipped with a loud message otherwise): `HOLOCRON_OSX_REAL=1` (`internal/osx`), `HOLOCRON_HOST_REAL=1` (`internal/host`), `HOLOCRON_IT=1` with `HOLOCRON_IT_FFMPEG`, `HOLOCRON_IT_FFPROBE`, `HOLOCRON_IT_MEDIAMTX`, `HOLOCRON_IT_EXE`, `HOLOCRON_IT_ROOT`, `HOLOCRON_IT_TMP` (end-to-end `TestEndToEnd`, needs MediaMTX), `HOLOCRON_LIVE=eventlog|toast` (`internal/alerts`, touches the OS), `HOLOCRON_SKIP_POWERSHELL=1` (skips `TestPowerShellProducerOutputConforms`). Recipes: the Tests sections of `cmd/holocron/README.md` and `internal/host/README.md`.
- CI matrix ([ci.md](../ci.md), `.github/workflows/ci.yml`): `lint-vet` (ubuntu), `test` on ubuntu and windows (blocking) and macOS (`continue-on-error`), `build` for windows/amd64, linux/amd64, linux/arm64, darwin/arm64. The PowerShell harness (`tests/cam.tests.ps1`) is not in CI.
- Windows rules: AUX, CON, NUL, PRN, COM1 and LPT1 are reserved device names, so no file may be named `aux.go` or `aux.json` (this broke `git checkout` on windows-latest; ledger commit `c359133`); `%TEMP%` may be an 8.3 short name (`USERNA~1`), which `archive.CheckRoot` resolves through the final path of an open handle (`internal/archive/README.md`), so compare resolved paths; real-process tests run from a short local folder with `TMP`/`TEMP` set and `WSLENV` passing the variables (recipe in `cmd/holocron/README.md`); run PowerShell from a local folder, never from `\\wsl.localhost`.
- Windows PowerShell 5.1 quirks that bit this project: `-File` binds a comma list as one string, so use `-Command "& 'script.ps1' -Checks a,b"` ([development.md](../development.md)); it pipes ASCII to a program, so a password piped to `holocron user add` must be ASCII ([ui.md](../ui.md)).
- Documentation checks: `powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "& '<path>\tests\cam.tests.ps1' -Checks doclinks,docswitches"` (every relative link and anchor in `README.md` and `docs/*.md`; capitalised `-Switch` tokens must be declared script parameters) and the Go drift guard `cmd/holocron/docs_test.go` (every `holocron ...` in the quickstart, cheat sheet, this file, `README.md` and `AGENTS.md` names a registered command; the table in section 5 equals the registry; a line marked `# ignore` is skipped).
- Never test against the Operator's live system: use a throwaway root, `HOLOCRON_SANDBOX_ONLY=1`, and the freshly built binary.

## 12. How to add a command

1. Add an entry to `commands` in `cmd/holocron/commands.go`: `path`, `usage`, `about`, `action` (`<area>.<verb>`), `mutating`, `audit` (read-only but security relevant), `noRoot` (touches no root), `run`. The init loop registers it as a `cli.Action` (component `cli`), so R2 logging is automatic; the help text comes from the same entry.
2. Implement `run` as `func(ctx, e *env) error`; return `usageErr` (exit 2), `quietFail` or a plain error (exit 1); read passwords and secrets from standard input only.
3. Tests: `TestEveryCommandIsAnAction` and `TestEveryCommandHasHelp` cover the registration by themselves; add the command to the `want` list of the first if it matters, and write its behaviour tests in `cmd/holocron` (rig in `cmd_test.go`).
4. Documentation in the same commit: a row in `cmd/holocron/README.md`, regenerate the table in section 5 (`HOLOCRON_DOCS_UPDATE=1 go test ./cmd/holocron -run TestAgentDocCommandTable`), and a line in the cheat sheet if the Operator will type it.

## 13. How to add a web admin route

1. Register the handler in `internal/web` with `handleAction(mux, runner, "POST /api/admin/<thing>", ActionName, handler)` (`admin_api.go`, `actions.go`): the handler runs as a `web.admin.*` action, so begin and result records, actor `web:<user>` and the audit flag come for free; mutating actions are audited.
2. Add the exact pattern to `routeAccess` in `internal/web/access.go` with its class (public, viewer, admin). A route without a class fails `TestEveryRouteHasAnAccessClass` and is refused at run time.
3. Do the work through the `web.AdminTools` interface, implemented by `webAdmin` in `cmd/holocron/webadmin.go`, which calls the function the matching command calls (R1: one code path). Take the one-at-a-time lock the other mutating routes take (409 while another runs).
4. Tests: `TestRoleMatrixOverEveryRoute`, `TestEveryRouteIsClassified`, `TestEveryAdminRouteIsAnAuditedAction`, plus a section test in `internal/web/admin_api_test.go`. Document it in [ui.md](../ui.md).

## 14. Commits, branches and the ledger

- Base branch `portable-nvr`. One branch per commission, named `<role>/<task-slug>` (for example `executor/holocron-docs`), created from the base; never commit to the base directly, never push or merge without the Operator's approval.
- One commit per logical change, each building and passing tests alone: a change with its tests and its doc line together; unrelated fixes and refactors are separate commits. Subject `<area>: <imperative summary>`; body gives why, what and how verified; trailers `Task: <id>` and `Decision: <id>` when relevant. Never add an agent as co-author.
- Integrate with `git merge --no-ff`, subject `Merge Holocron <id>: <summary>`. Never squash or rewrite pushed history. Undo a whole task with `git revert -m 1 <merge sha>`; the commits inside stay cherry-pickable (`git log <sha>^1..<sha>^2`).
- [tasks/portable-nvr/ledger.md](../../tasks/portable-nvr/ledger.md) lists every merge, regenerated in the same push as the merge: `git log --first-parent --merges --reverse --date=short --format="%h|%ad|%s" portable-nvr`.
- No em dash anywhere (use `-`). `rm`, `cp` and `mv` may be interactive aliases on the Operator's machines: use `command rm -f`.

## 15. Known limits and UNVERIFIED items

- Windows service mode (`install --mode service`): tested against a fake service manager only; no elevated real install ([autostart.md](../autostart.md)). UNVERIFIED.
- macOS: compiled and vetted, nothing run on a Mac; no kernel kill-on-close, the reap leaves entries alone, launchd and protected-root lists unverified ([osx README](../../internal/osx/README.md)). Keychain is not implemented.
- Linux on real hardware: the systemd user unit was exercised only in WSL2 with a test-named unit ([cutover.md](../cutover.md#appendix-linux-and-macos)).
- The clock change of 25 Oct 2026 is not spanned by any real run; the next is 28 Mar 2027. Covered by unit tests and a synthetic clock only ([cutover.md](../cutover.md)).
- Microsoft Defender once blocked an unsigned build run from `C:\Windows\Temp`; install from a permanent folder; a signed binary is the durable fix ([autostart.md](../autostart.md#known-issue-microsoft-defender)).
- Cameras serve exactly two concurrent RTSP sessions; the relay exists to turn them into one; MediaMTX path names are lowercase alphanumeric ([limitations.md](../limitations.md)).
- Segments are cut on a keyframe, so a boundary can overshoot by one GOP; the first and last segment of a run are partial.
- `status` shows days left only after 10 minutes of history. Windows event log entries go under source `IPCCTV` only if it was registered (elevated, once, by the PowerShell system's `-InstallEventSource`), otherwise under `Application`, always prefixed `IPCCTV: `.
- `up --detach` from a terminal started through WSL interop may end with that terminal (job breakaway refused).
- The live-view 502 of the first cutover hour (supervisor and web role read different secret files) is fixed in W17; builds older than `e17efc6` still have it ([cutover-plan.md](../../tasks/portable-nvr/cutover-plan.md)).
- Not run anywhere: the HTTPS listener's Windows firewall rule for `holocron.exe`; the Defender steps ([cutover.md](../cutover.md) "Known gaps").

## 16. Glossary

- **segment**: one recording file, a clock-aligned slice (default 900 s) named `<Camera_Name>_YYYYMMDD_HHMMSS+hhmm.mkv`; the newest of a camera is open and always protected from the pruner.
- **root**: `RootPath`, the archive folder holding one folder per camera, `_state` and `_logs`; the pruner acts only on a root that carries the marker.
- **marker**: `<root>/_state/archive-owner.json`, the ownership claim the pruner requires; written by the supervisor, compatible with the PowerShell recorder's.
- **relay path**: the per-camera lowercase alphanumeric MediaMTX path (`rtsp://127.0.0.1:8554/<path>`) that recorders and viewers read so each camera is pulled once.
- **host / supervisor / web role**: the three internal processes of section 3.
- **census**: the list of processes `down` finds by pid, start time and image from `host.json`, `children.json`, `webchild.json` and the lock.
- **scope**: the OS mechanism that binds children to a process's life (job object, process group); section 10.
- **heartbeat**: the supervisor's `status.json` write; running means a fresh heartbeat plus a live pid with the recorded start time.
- **StopKind**: how the last stop was classified: `requested`, `interrupted`, `unexpected`, `window-closed`.
- **shadow**: Holocron recording a copy beside the PowerShell system into a separate root, read from the same relay, then proved equal by `archive compare` ([shadow.md](../shadow.md)).
- **cutover**: the move of the real NVR from the PowerShell system to Holocron on 2026-10-22 ([cutover.md](../cutover.md), record in [cutover-plan.md](../../tasks/portable-nvr/cutover-plan.md)); **rollback** is the way back ([rollback.md](../rollback.md)).
- **sandbox**: a throwaway root used with `HOLOCRON_SANDBOX_ONLY=1`; never the Operator's real root.
