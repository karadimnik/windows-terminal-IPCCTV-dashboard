# holocron

The one executable (rule R1). Every command is a `cli.Action` registered once (`commands.go`) and run by `cli.Runner`, so
its begin (DEBUG) and result (INFO mutating ok, DEBUG read-only ok, WARN refused, ERROR failed) are logged with action,
actor `cli:<os user>`, target, action_id, pid, duration (rule R2). `TestEveryCommandIsAnAction` asserts the exposed list.

    holocron [--root DIR] [--config FILE] [-v|-vv] [--json] <command> [arguments]

`--root` overrides `RootPath`; `--config` defaults to `holocron.config.json` in the platform's configuration folder and
`cameras.json` sits next to it. `-v`/`-vv` set DEBUG/TRACE for that invocation and mirror the records to stderr. Records go
to `<root>/_logs/holocron.log`, mutating ones also to `audit.log`. With `HOLOCRON_SANDBOX_ONLY=1` a root that is a drive or
filesystem root, has a folder named `cameras` in its path, or holds PowerShell recorder files is refused (exit 2).

| Command | Does | Action |
|---|---|---|
| `up [--detach] [--only host]` | attached: the host runs in this process, re-runs this binary as the hidden `supervisor` (in the host's process scope); closing the terminal stops everything. `--detach`: a background `host --service` (Windows `DETACHED_PROCESS\|CREATE_NEW_PROCESS_GROUP\|CREATE_NO_WINDOW` + breakaway from the terminal's job when allowed; Unix `Setsid`), then waits up to 60 s for a fresh heartbeat: `RECORDING ...` or `NOT RECORDING` exit 1 (G10). Already running: says so, exit 0 | `nvr.up` |
| `down` | census before (host by `_state/host.json`, supervisor by its instance lock or heartbeat, children by `_state/children.json`, all by pid + start time + image), `POST /v1/stop`, else `_state/stop.request`; waits up to `StopTimeoutSeconds`; `0 survivors` or `DID NOT STOP` + list, exit 1. Kills nothing, never by name; nothing running: exit 0 | `nvr.down` |
| `restart [--detach]` | down, then up | `nvr.restart` |
| `status [--json]` | verdict, supervisor and host pids, cameras (state, input, pid, restarts, last error, last closed segment), relay, live view, disk and projected days, prune, alert channels, autostart (not built yet); exit 0 running, 1 not | `nvr.status` |
| `cam list\|show\|add\|remove\|set\|enable\|disable\|test` | `cameras.json` through `config.Cameras` (BOM and unknown fields kept, atomic); add/set validate the edited camera (name as a folder, address host only, stream, folder collisions); enable checks the name, disable writes only; `all` when no camera has that name; then `POST /v1/reload` when running. `test`: TCP + RTSP `OPTIONS` (5 s, no login) to the camera's address (port 554 unless the address carries one) | `camera.*` |
| `config get\|set\|unset\|validate\|show` | allow-list = KEYS.md `Editable` column (`app.EditableKeys`, test-tied); the edited document is validated with `config.Parse` before an atomic write that keeps every other key, the order and the BOM; refusal = file untouched (byte-compared in tests); `show`/`get` are the effective values, URLs with a login masked | `config.*` |
| `live enable\|disable\|status` | running: `POST /v1/live` (the supervisor edits `LiveViewEnabled` and starts/stops the live MediaMTX at once); else the file | `live.*` |
| `log level <component\|all> <level> [--save]` | `POST /v1/log/level` (immediate); `--save` writes `logging.levels.<c>` / `logging.level` | `log.level` |
| `logs [-f] [--component c] [--level l] [--since d] [--action a]` | reads `holocron.log.1` then `holocron.log`, filters the text format, follows by polling (500 ms, rotation-aware) | `logs.read` |
| `alert test [channel]` | `alerts.Dispatcher.Test` per channel: ok / off / FAIL | `alert.test` |
| `doctor` | ffmpeg version (>= 5.0), MediaMTX `--version`, configured ports free (or ours), folders writable, `archive.CheckRoot` + marker, clock, camera login present (values never shown), status | `doctor.run` |
| `version`, `help` | | `version.show`, `help.show` |
| `web`, `open`, `user ...`, `tls ...`, `secret ...`, `install`, `uninstall`, `autostart ...`, `prune`, `archive stats` | exit 1 `not built yet` (later tasks) | `web.serve`, `user.add`, ... |
| hidden `host [--service]`, `supervisor [--attached]` | the roles `up` starts (`app.RunHost`, `app.NewSupervisor`); the supervisor exits with its typed code | `role.host`, `role.supervisor` |

Exit codes: 0 ok, 1 failure, 2 usage (also a sandbox refusal), 20 another supervisor holds the archive, 21 configuration
fault (a missing camera login names `HOLOCRON_CAMERA_USER` / `HOLOCRON_CAMERA_PASSWORD`; ffmpeg below 5.0).

The camera login comes from `HOLOCRON_CAMERA_USER` and `HOLOCRON_CAMERA_PASSWORD` (supervisor process only) until the
secrets store is plugged into `app.CredentialProvider`. No credential is ever on a command line or in a file this command
writes. The control API is described in `internal/ctlapi/README.md`.

## Tests

`go test ./cmd/holocron` runs the unit tests with a fake control client and status files in `t.TempDir()`.
The end-to-end run is opt-in and was run on Windows (WSL builds both binaries; MediaMTX is a copy in the test's temp folder,
ports 18740-18742 only, every other listener off; the relay's listener set is checked with `netstat -ano`):

    GOOS=windows go build -o /mnt/c/Windows/Temp/holocron-w9a/holocron.exe ./cmd/holocron
    GOOS=windows go test -c -o /mnt/c/Windows/Temp/holocron-w9a/holocron.test.exe ./cmd/holocron
    cd /mnt/c/Windows/Temp/holocron-w9a && HOLOCRON_IT=1 HOLOCRON_IT_FFMPEG='C:\ffmpeg\bin\ffmpeg.exe' \
      HOLOCRON_IT_MEDIAMTX='C:\mediamtx\mediamtx.exe' HOLOCRON_IT_EXE='C:\Windows\Temp\holocron-w9a\holocron.exe' \
      TMP='C:\Windows\Temp\holocron-w9a\scratch' TEMP='C:\Windows\Temp\holocron-w9a\scratch' \
      WSLENV=HOLOCRON_IT:HOLOCRON_IT_FFMPEG:HOLOCRON_IT_MEDIAMTX:HOLOCRON_IT_EXE:TMP:TEMP ./holocron.test.exe -test.v -test.run TestEndToEnd

Scenario: `up --detach` -> both lavfi cameras alive and writing segments -> `cam disable cam2` stops it, cam1 goes on ->
`log level supervisor debug` makes DEBUG supervisor records appear -> `config set MaxRetentionDays 30` -> `down` with
0 survivors, final `stopped/requested`, `ctl.addr`, `ctl.secret`, `host.json` gone, the fake password in no file -> a
second `down` reports nothing running. Measured 2026-10-20 (Windows 11, from WSL): first valid status 1.7 s after `up`,
stop 0.4 s, 0 survivors, whole test 31.8 s. Launched from WSL interop the terminal's job refuses breakaway; `up` then says
so (the background copy stays in that job).
