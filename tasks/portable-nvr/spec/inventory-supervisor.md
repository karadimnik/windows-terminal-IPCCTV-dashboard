# Behaviour inventory B: status contract, supervisor, control surface, relay/live, install, strip, tests

Scope: part B of two (part A covers ffmpeg command lines, naming, child lifecycle, config keys, pruner, alerts, time, logging).
Source tree: `<repo>`, branch portable-nvr. Read-only; nothing was run.

## 0. How to read this file

Abbreviations: `common` = `cam.common.ps1`, `rec` = `cam.recorder.ps1`, `cam` = `cam.ps1`, `health` = `cam.health.ps1`,
`tests` = `tests/cam.tests.ps1`, `testlib` = `tests/cam.testlib.ps1`, `todo` = `tasks/ipcctv-recording/todo.md` (items are written `[N]`),
`plan` = `tasks/portable-nvr/plan.md`. A reference `rec:2726` means line 2726 of `cam.recorder.ps1`. Go files are under `ui/`.

Evidence labels (nothing was executed in this session, so no label means "observed running"):

- **CODE** - read directly in the source at the cited line (this is the "CONFIRMED" of the commission, but confirmed by reading, never by running). The strongest label available here.
- **TODO-MEASURED** - a measurement the todo records as made on the Operator's machine; "documented-only" from this session's point of view.
- **TEST** - a harness check exists that asserts it (name given); whether it currently passes is UNKNOWN (not run).
- **UNVERIFIED** - inferred from code or comments, not checked against a running system.
- **UNKNOWN** - could not be determined from the sources.

---

## 1. `status.json`

### 1.1 Location, writers, atomicity, cadence

| Aspect | Rule | Source |
|---|---|---|
| Path | `<RootPath>\_state\status.json`; `_state` is created by `Get-CamPaths` (and by the writer if missing) | common:2555-2570; rec:3021-3022; common:1940,1947 |
| Writers (4, one file, no lock) | (a) the supervisor, full document every `StatusIntervalSeconds` (default 5); (b) the supervisor's `finally` block, final document with `State=stopped`; (c) the NVR host, patch `Set-CamStatusUnexpectedStop`; (d) the NVR warden, patch `Set-CamStatusWindowClosed` | rec:3891,4062-4115; rec:4180-4185; common:3083-3134; common:3137-3191 |
| Cadence | `statusDue` is evaluated once per loop pass, the loop sleeps 1 s per pass (`Start-Sleep -Seconds 1`), so real cadence is "first pass at or after 5 s since the last write". First write is on the first pass (`lastStatus` starts at MinValue) | rec:3815,3891,4123; common:424 |
| Blocking work delays the heartbeat | the loop is single-threaded: a relay (re)start blocks up to `RelayStartupWaitSeconds` (20 s) and child starts sleep `StartStaggerMs` (300 ms) each; the liveness limit is 15 s at defaults, so a relay restart can make the file look stale for a few seconds. UNVERIFIED at runtime (arithmetic from CODE) | rec:2192-2217; rec:4041-4044; common:3221-3222 |
| Atomic write | serialise, write `<path>.tmp` (UTF-8, no BOM, text + `Environment.NewLine`), then `Move-Item -Force` retried 10 x 100 ms. `File.Replace` is deliberately not used (the `\\wsl.localhost` 9P share rejects ReplaceFileW) | rec:3024-3026; common:2627-2661 |
| Reader | opens with `FileShare.ReadWrite or Delete`, 5 tries x 80 ms, strips a UTF-8 BOM; never throws, returns `$null` for missing/empty/invalid | common:2588-2624; common:3064-3080 |
| Go reader | `os.ReadFile` + BOM strip + `json.Unmarshal` into `map[string]any`, then redaction. Whether Go's Windows file open uses `FILE_SHARE_DELETE` is UNVERIFIED (matters because a reader without it blocks the writer's rename; the writer retries 10 x 100 ms) | ui/api.go:171-197 |
| Redaction | after `ConvertTo-Json -Depth 6` the WHOLE JSON text goes through `Get-CamRedacted`: (1) `scheme://userinfo@` becomes `scheme://***:***@`; (2) every registered secret of length >= 3 is replaced literally by `***`. Per-field redaction is applied earlier to Name, LastError, ConfigError, Restreamer/Live LastError, Prune.LastMessage, Events.Text. Registered secrets: camera user and password (raw and URI-escaped), relay API password and its base64 token, live secret. A literal replace over serialised JSON can alter keys or values that merely contain the secret text: UNVERIFIED consequence | rec:3018-3019; common:124-166,192-193,240,922,1916 |
| camui adds its own redaction | drops keys named `relaybase`, `password`, `username` (case-insensitive) at any depth; replaces every `scheme://...` token and every bare IPv4 literal in every string with `[redacted]`; drops `Live.Paths`, `Live.Cameras[].SubPath/MainPath` | ui/api.go:298-349; ui/live.go:581-596 |
| Timestamps | `Format-CamTimestamp`: .NET round-trip `o` format in LOCAL time with the offset in force, 7 fractional digits, e.g. `2026-10-25T03:30:00.1234567+02:00`; `$null` for "never" (also for `DateTime.MinValue`). All intervals are computed on UTC (`Get-CamWallClock`) | common:87-101; common:2756-2770 |
| Encoding specifics | PowerShell 5.1 `ConvertTo-Json` (indented, `'`-style escapes). No consumer depends on formatting; a Go writer only needs valid JSON | rec:3018 (UNVERIFIED output shape) |
| History | [8] writer + `-Status`; [56] StopKind/LastUnexpectedStop; [57] DownSince; [67] window-closed; [81]/[84] window records; [86] UTC clock | todo:53; todo:660; todo:663; todo:873 |
| Proving checks | `strip` (renders a fixture), `crashsafe` (StopKind/LastUnexpectedStop), `windowclose` (window-closed/LastWindowClose), `backoff` (RestartsLastHour), `livecfg` (key set with live off equals master's), `selftest` (`Test-CamStatusSurface` field presence, rec:1328-1373) | tests:628,1246,1372,4719,6547,226 |

### 1.2 Top-level fields

All timestamps below are "timestamp" = string in the `o` format or `null`. A fully written document always contains every field in this table except `LastWindowClose` and `Live`.

| Field | JSON type | Null? | Values / units | Written at |
|---|---|---|---|---|
| `Schema` | integer | no | constant `1`. No reader branches on it; only the self-test checks that the field is present (rec:1347) | rec:2987 |
| `State` | string | no | `"running"` or `"stopped"` (writer parameter is `ValidateSet`) | rec:2744,2988 |
| `StopKind` | string | no | `""` while `running`; otherwise one of `requested`, `interrupted`, `unexpected` (writer `ValidateSet`, rec:2755) or `window-closed` (only the warden patch can write it, common:3178) | rec:2989 |
| `LastUnexpectedStop` | object or null | yes | `{At: timestamp, Reason: string, SupervisorPid: int, DetectedBy: "supervisor"\|"nvr host"}`. Carried forward from the previous file at supervisor start; replaced by a later unexpected stop | rec:2990,3758-3759,4147-4154; common:3107-3112 |
| `LastWindowClose` | object (key ABSENT unless a close was recorded) | absent | `{At, Reason, WindowTitle, DetectedBy: "nvr warden"}`. Written ONLY by the warden patch. `Write-CamStatusFile` has no parameter for it and its `$document` does not contain it, so the next full write by a supervisor drops it (see 1.6, finding F1) | common:3164-3169,3179; rec:2986-3015 |
| `SupervisorPid` | integer | no | `$PID` of the supervisor (a PowerShell process). `0` in the minimal document created by the window-close patch when no file existed | rec:2991; common:3174 |
| `StartedAt` | timestamp | no (full doc) | supervisor start | rec:2992 |
| `UpdatedAt` | timestamp | no | the heartbeat. Full write: start of the write. Patches set it to the patch time | rec:2993; common:3123,3180 |
| `UptimeSeconds` | number | no (full doc) | `round(now - StartedAt, 1)` seconds | rec:2994 |
| `RootPath` | string | no (full doc) | archive root as configured | rec:2995 |
| `Container` | string | no (full doc) | active container profile key, e.g. `mkv`, `fmp4` | rec:2996 |
| `SegmentSeconds` | integer | no (full doc) | configured segment length | rec:2997 |
| `Disk` | object | no (full doc) | see 1.3 | rec:2998-3010 |
| `Prune` | object | no (full doc) | see 1.3 | rec:3011 |
| `Restreamer` | object | no (full doc) | see 1.3; present even when the relay is disabled | rec:3012 |
| `Live` | object (key ABSENT unless `LiveViewEnabled` and a live entry exists) | absent | see 1.3 | rec:3016 |
| `Events` | array of object | no | see 1.3 | rec:3013 |
| `Cameras` | array of object | no | one row per camera in `cameras.json`, see 1.4 | rec:3014 |

Minimal documents (no previous file when a patch runs): `Set-CamStatusUnexpectedStop` creates `{Schema, SupervisorPid, UpdatedAt}` then adds `State, StopKind, LastUnexpectedStop`; `Set-CamStatusWindowClosed` creates `{Schema, SupervisorPid:0, UpdatedAt}` then adds `State, StopKind, LastWindowClose`. These have no `Disk`, `Cameras`, etc.; readers must tolerate their absence (common:3116-3123, 3173-3180).

Patches are read-modify-write of the whole document (`Get-CamStatusSnapshot` then `Add-Member -Force` then rewrite through the same temp-and-rename), so everything else stays as "the last true reading" (common:3094-3096).

### 1.3 Sub-objects

**`Disk`** (rec:2998-3010; derivations rec:2775-2802, 2713-2720)

| Field | Type | Null? | Meaning |
|---|---|---|---|
| `FreeGB` | number | no | free space on the volume holding `RootPath`, `AvailableFreeSpace / 2^30`, 2 decimals, measured on EVERY write (common:2290-2298) |
| `ReserveGB` | number | no | config `ReserveGB` |
| `PruneHysteresisGB` | number | no | config |
| `ArchiveGB` | number | null until the first archive scan | `Bytes / 2^30`, 2 decimals |
| `ArchiveBytes` | integer | null until scan | sum of the length of EVERY file (any name) in every non-underscore, non-reparse directory under the root; the in-progress file counts at its true open-stream length (rec:2576-2642, 2680-2723) |
| `ArchiveFiles` | integer | null until scan | count of those files |
| `OldestSegment`, `NewestSegment` | timestamp | null | min / max `LastWriteTimeUtc` over those files |
| `MeasuredGBPerDay` | number | null | `(Bytes/2^30) / (span_days)`, 2 decimals, ONLY when newest-oldest >= 600 s |
| `ProjectedDaysRemaining` | number | null | `max(FreeGB - ReserveGB, 0) / MeasuredGBPerDay`, 2 decimals, only when the rate is > 0 |
| `ScannedAt` | timestamp | null | when the archive scan behind the figures ran. Scan cadence `ArchiveScanSeconds` (60 s, common:428; rec:4101-4105), so all archive figures lag up to 60 s while `FreeGB` is live |

**`Prune`** (rec:2878-2893; state mutated at rec:3122-3124, 3218-3220, 3382-3387)

| Field | Type | Meaning |
|---|---|---|
| `LastRunAt` | timestamp or null | last prune run (also set on a refused/blocked run) |
| `DeletedTotal` | integer | files deleted since THIS supervisor started (the state object is created per supervisor, rec:3784; not persisted) |
| `FreedGBTotal` | number | `FreedBytes / 2^30`, 3 decimals, same lifetime |
| `Blocked` | boolean | true while free space is under the reserve and nothing deletable is left, or pruning is refused |
| `LastMessage` | string | redacted last prune message ('' default) |

**`Restreamer`** (rec:2898-2925). Present always. When the relay is disabled the defaults are `Enabled=false, RelayBase=<cfg>, Pid=null, LastPid=null, Alive=false, StartedAt=null, RestartsTotal=0, Paths=[], LastError='', LastErrorAt=null`. When a relay entry exists: `Enabled` is hard-coded `true`.

| Field | Type | Meaning |
|---|---|---|
| `Enabled` | boolean | `RestreamerEnabled` (disabled shows as `false`; "should run and does not" shows `true` + `Alive=false`) |
| `RelayBase` | string | e.g. loopback RTSP base from config; camui deletes this key (ui/api.go:309-313) |
| `Pid` | integer or null | pid only while alive |
| `LastPid` | integer or null | last child pid kept after exit; `-Stop` uses it to prove no orphan (rec:3579-3589) |
| `Alive` | boolean | process exists and `HasExited` false |
| `StartedAt` | timestamp or null | start of the current/last child |
| `RestartsTotal` | integer | restarts since supervisor start |
| `Paths` | array of string | the CONFIGURED path names (`PathMap.Keys`), NOT the ready ones - open todo [32], docs/limitations.md:17 |
| `LastError` | string | e.g. `mediamtx exited with code N after Ns` (rec:2315) or `failed to start: ...` (rec:2331) |
| `LastErrorAt` | timestamp or null | |

**`Live`** (rec:2932-2962; only when live view is enabled; "with it off status.json is exactly what it was", rec:2927-2928; TEST `livecfg` asserts master's top-level key set)

| Field | Type | Meaning |
|---|---|---|
| `Enabled` | boolean | always `true` when the section exists |
| `Pid` / `LastPid` | integer or null | as for the relay |
| `Alive` | boolean | live MediaMTX process alive |
| `StartedAt`, `RestartsTotal`, `LastError`, `LastErrorAt` | as for the relay | |
| `Paths` | array of string | live path names (camui drops it) |
| `Cameras` | array of `{Name: string (redacted), Sub: bool, Main: bool, SubPath: string, MainPath: string}` | the one place the live path-naming rule is applied for readers; `SubPath`/`MainPath` are `''` when absent. camui drops the two path strings (ui/live.go:583-596) |

**`Events`** (rec:2964-2984). Array, NEWEST FIRST, length <= `StatusEventCount` (default 20, common:332); empty array when that is <= 0 or no log path. Each element `{At: string or null, Text: string}`. Source: the tail of `recorder.log` (last N non-blank lines, 64 KB window, first partial line discarded: common:2236-2287). `At` is parsed with `^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{2}:\d{2}) (.*)$`, so it is the LOG format `yyyy-MM-dd HH:mm:ss +03:00` (common:2136), NOT the `o` format; lines that do not match (e.g. a drop notice) get `At=null` and the whole line as `Text`. `Text` is redacted. Go's `parseStatusTime` accepts both layouts (ui/supervisor.go:115-125).

**`LastUnexpectedStop`**: `{At, Reason, SupervisorPid, DetectedBy}` (rec:4148-4153 supervisor-written; common:3107-3112 host-written). **`LastWindowClose`**: `{At, Reason, WindowTitle, DetectedBy}` (common:3164-3169).

### 1.4 `Cameras[]` (20 fields, rec:2854-2875)

Rows are built from the camera list resolved at the last SUCCESSFUL reconcile (rec:3857-3860), in `cameras.json` order, for every camera whether or not it records. Before the first reconcile it is `[]`. A reconcile that throws keeps the previous list (rec:3871-3875).

| Field | Type | Null? | Meaning / derivation |
|---|---|---|---|
| `Name` | string | no | redacted camera name |
| `SafeName` | string | null if the name is unusable | filesystem token (part A) |
| `Record` | boolean | no | `Record` flag from `cameras.json` (explicit true only, common:770-781) |
| `Pid` | integer | null | child pid while a process object exists (even if just exited and not yet reaped, then `Alive=false`); null between restarts and for non-recording cameras |
| `LastPid` | integer | null | last child pid, kept after exit |
| `Alive` | boolean | no | process exists and not `HasExited`; false for non-recording |
| `CurrentSegment` | string | null | file name of the most recently CREATED file in the camera directory (creation time never goes stale, rec:2583-2586,2611) |
| `CurrentSegmentBytes` | integer | null | true length of that file via an open stream; falls back to the directory-entry length if the open is refused (rec:2555-2573) |
| `SecondsSinceLastClosedSegment` | number | null | `now - max(LastWriteTimeUtc of every file except the current one)`, 1 decimal; null with <2 files |
| `InputMode` | string | no | `''` (never started), `relay` or `direct`: the input chosen at the last start (rec:1684) |
| `RestartsLastHour` | integer | no | restart stamps newer than 1 h (rec:2826-2827) |
| `RestartsTotal` | integer | no | restarts since supervisor start (exit restarts + watchdog restarts; NOT deliberate stops or relay promotions) |
| `StartedAt` | timestamp | null | current/last child start |
| `NextStartAt` | timestamp | null | only when no process: the backoff deadline |
| `LastError` | string | no | redacted; e.g. `ffmpeg exited with code N after Ns` (rec:3954), `stalled, no segment write for ...` (rec:3998), `failed to start: ...` (rec:4052) |
| `LastErrorAt` | timestamp | null | |
| `ArchiveFiles`, `ArchiveBytes` | integer | null | from the camera's 5-second scan if it has a child, else the 60-second archive scan, else null |
| `DownSince` | timestamp | null | set when the camera has made no file progress for the alert limit; cleared when it writes again; value is the reference time (last progress or supervisor start), see part A alert 110 (rec:4074-4099) |
| `ConfigError` | string | no | `''` or the resolve error (e.g. reserved device name) |

### 1.5 `State` / `StopKind` semantics (the stop taxonomy)

| `State` | `StopKind` | Written by (line) | Meaning | NVR host reaction | camui reaction |
|---|---|---|---|---|---|
| running | `""` | supervisor tick (rec:4108) | alive only if heartbeat fresh AND pid alive (1.7) | n/a | "Live" |
| stopped | `requested` | supervisor `finally`, stop flag seen (rec:4140,4181) | orderly `-Stop` (flag file) | host returns, no alert, no restart (common:5004-5008) | none; Reason "recorded a clean shutdown" |
| stopped | `interrupted` | supervisor `finally`, neither flag nor fault (rec:4139) | Ctrl+C in the pane or the tab/pipeline closing: "a human did it" | host returns, no restart, no alert | no record is written for it. If an older `LastUnexpectedStop` is still carried, the page shows THAT old record as the "current" notice (app.js:278,285-286); otherwise the fallback banner "The recorder stopped on its own. Its last status was interrupted." (app.js:287-290) |
| stopped | `unexpected` | (a) supervisor `finally` when the loop threw, exit 21 (rec:4126-4147,4195); (b) NVR host patch when the supervisor died without a final write (common:5024,3120-3122) | malfunction | alert id 100 (Error), restart after 5 s, budget 5 per 30 min, then alert 102 and stay down (common:5010-5053) | banner "stopped on its own", dismissible |
| stopped | `window-closed` | NVR warden patch after the NVR window disappears (common:4800,3178) | the cam.nvr window was closed (deliberate or accidental - the system cannot tell) | none: nothing restarts, by Operator decision [67]; alert id 103 (Warning) | banner "The NVR window was closed", dismissible |

Host expected-or-not rule (common:4946-4954,4999-5025): no restart if the stop flag file exists, or `StopKind` read back from the snapshot is `requested` or `interrupted`, or the child exit code is 20. Anything else is "unexpected". UNVERIFIED edge from CODE: the host reads the PERSISTED `StopKind`; if the previous run ended `requested` and the next supervisor dies before its first status write (e.g. missing credential file), the stale `requested` makes the host stop without restarting. Not covered by any check found.

Exit codes of `-Run`: `0` after the finally on a clean/requested path, `20` another supervisor already running (rec:3750-3753), `21` the loop threw (rec:4195). The exit code on an `interrupted` stop is UNKNOWN (the script is being terminated; `exit 0` at rec:4196 may not execute).

### 1.6 Findings that a rewrite must decide on (code vs stated intent)

- **F1 - `LastWindowClose` is not carried forward.** todo [78] (todo:1104-1107) and ui/notice.go:14-20 say `LastUnexpectedStop` AND `LastWindowClose` persist "after the recorder is running again". Only `LastUnexpectedStop` is carried (rec:3758-3759); `LastWindowClose` is added by the warden patch (common:3179) and is absent from `Write-CamStatusFile`'s document (rec:2986-3015). So the first full write after a restart drops it, and the "earlier" banner for it (app.js:411) can only be shown while the file is still the stopped one. CODE-evident, not run. Decide: reproduce (bug-compatible) or carry it (intent).
- **F2** - PowerShell's `Get-CamSupervisorState` says "recorded a clean shutdown" for every non-running State, including `unexpected` and `window-closed` (common:3216-3219); Go distinguishes them (ui/supervisor.go:83-89). The Go wording is the better one to keep.
- **F3** - ui/supervisor.go:45 documents `StopKind` as `requested | interrupted | unexpected` and omits `window-closed`, which the code does handle (supervisor.go:85-86).
- **F4** - `Restreamer.Paths` and `Live.Paths` list configured paths, not ready paths (todo [32], open).

### 1.7 Who reads which field, and how staleness is judged

Liveness rule (`Get-CamSupervisorState`, common:3194-3242), mirrored by Go `SupervisorStateOf` (ui/supervisor.go:60-110). Both signals must agree:

1. no snapshot -> not running, reason "no status file has been written yet";
2. `State != "running"` -> not running;
3. `limit = max(StatusIntervalSeconds x 3, 15)` seconds (default 15); `UpdatedAt` unparsable -> not running ("no usable heartbeat"); `age = now(UTC) - UpdatedAt` (1 decimal) `> limit` -> not running;
4. `SupervisorPid` must be a live process (`Get-Process -Id`; Go: `OpenProcess` + `GetExitCodeProcess == STILL_ACTIVE` on Windows, `kill(pid,0)` elsewhere) - no start-time/identity check, so a reused PID passes if the heartbeat is fresh.

Go tests for the verdict already exist: `TestStaleRunningStatusIsNotRunning`, `TestSupervisorStateOf`, `TestSupervisorStateWithNoStatusFile`, `TestHeartbeatLimitFollowsTheStatusInterval`, `TestProcessAliveOnThisProcess` (ui/supervisor_test.go:29,56,125,141,160); PowerShell side is exercised by `strip`.

Go reads `StatusIntervalSeconds` from `recorder.config.json` on every request (positive number, else 5; ui/settings.go:256-269). The file's own mtime is exposed to the page as `statusFile.ageSeconds` (ui/api.go:178-180) but is NOT part of the verdict.

| Reader | Fields read (and what for) | Source |
|---|---|---|
| PS `Get-CamSupervisorState` | `State`, `SupervisorPid`, `UpdatedAt` | common:3194-3242 |
| `rec -Status` (`Show-CamStatus`) | `UptimeSeconds`, `UpdatedAt`, `RootPath`, `Container`, `SegmentSeconds`; `Cameras[].{Name,Record,Alive,Pid,CurrentSegment,CurrentSegmentBytes,RestartsLastHour,LastError,ConfigError,SecondsSinceLastClosedSegment,NextStartAt}`; `Disk.{FreeGB,ReserveGB,PruneHysteresisGB,ArchiveBytes,ArchiveFiles,OldestSegment,NewestSegment,MeasuredGBPerDay,ProjectedDaysRemaining}`; `Prune.{Blocked,LastRunAt,DeletedTotal,FreedGBTotal,LastMessage}`. Says "STALE - NO SUPERVISOR IS RUNNING" and dims colours when the verdict is not running; always exits 0 | rec:3398-3500 |
| `rec -Stop` | after the supervisor exits: `Cameras[].LastPid/Pid` (kill a surviving `ffmpeg` by that pid), `Restreamer.LastPid`, `Live.LastPid` (kill a surviving `mediamtx`) | rec:3554-3602 |
| NVR host | `StopKind`, `LastUnexpectedStop.Reason` | common:5001-5019 |
| `cam.ps1 -Status` | verdict; `Live.{Alive,Pid,RestartsTotal,Paths,LastError}` (only when live enabled) | cam:239-249,376-392 |
| `health` (strip) | verdict; `UptimeSeconds`; `Restreamer.{Enabled,Alive,Pid,Paths,RestartsTotal}`; `Disk.{FreeGB,ReserveGB,ProjectedDaysRemaining,OldestSegment,NewestSegment,ArchiveGB,ArchiveFiles,MeasuredGBPerDay}`; `Cameras[].{Name,Record,Alive,InputMode,SecondsSinceLastClosedSegment,RestartsLastHour,LastError,ConfigError}`; `Events[].{At,Text}`; `Prune.{DeletedTotal,FreedGBTotal,Blocked,LastMessage}` | health:191-465 |
| camui Go | `State`, `StopKind`, `SupervisorPid`, `UpdatedAt` (verdict); `Live.Alive`, `Live.Cameras[].{Name,Sub,Main,SubPath,MainPath}`, `Cameras[].{Name,Alive,RestartsLastHour,LastError}` (live view); every other field is passed through to the browser after redaction as `status` | ui/supervisor.go:60-110; ui/live.go:169-217; ui/api.go:123-168 |
| camui page (`app.js`) | `Cameras[].{Name,Record,Alive,RestartsLastHour,InputMode,DownSince,LastError,ArchiveFiles,ArchiveBytes,SecondsSinceLastClosedSegment}`; `Disk.{ReserveGB,FreeGB,PruneHysteresisGB,ProjectedDaysRemaining,ArchiveGB,MeasuredGBPerDay,ArchiveFiles}`; `StopKind`; `LastUnexpectedStop.{At,Reason,DetectedBy}`; `LastWindowClose.{At,Reason,DetectedBy}`; `Events[].{At,Text}`; `RootPath`. Decision is on `supervisor.running`, never on `State` ("stale" = `state=="running"` but not live) | ui/static/app.js:176-380,401-423 |
| camui response envelope (NOT status.json) | `{schema:1, serverTime, statusFile:{path,present,modifiedAt,ageSeconds,error}, supervisor:{running,reason,state,stopKind,pid,pidAlive,heartbeatSeconds,heartbeatLimitSeconds}, status, tasks, acknowledged}` | ui/api.go:31-68; ui/supervisor.go:34-53 |
| Fields nobody reads | `Schema`, `StartedAt`, `Container`, `SegmentSeconds` (except `-Status`), `Disk.ScannedAt`, `Cameras[].{SafeName,Pid(camui),LastPid(camui),RestartsTotal,StartedAt,LastErrorAt}`, `Restreamer.{RelayBase,LastError,LastErrorAt,StartedAt}`, `LastUnexpectedStop.SupervisorPid` | grep of the readers above |

camui sidecar: acknowledgements are stored in `_state\camui-acknowledged.json` (`{schema:1, notices:{<key>:<At of the acknowledged event>}}`), keys limited to `LastUnexpectedStop` and `LastWindowClose`, temp-and-rename, never in status.json (ui/notice.go:35-60,154-189).

### 1.8 DRAFT JSON Schema (draft 2020-12)

DRAFT. Derived only from `Write-CamStatusFile`, the two patch functions and the readers listed above. `additionalProperties` is left open everywhere because the readers ignore unknown keys and the plan wants the schema versioned (plan:416). The `if/then` branch states the full-document requirement for a running file. Not validated against a real file (none was read).

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:ipcctv:status.json:draft-1",
  "title": "IPCCTV status.json (DRAFT, derived from cam.recorder.ps1 Write-CamStatusFile, Schema 1)",
  "type": "object",
  "required": ["Schema", "State", "SupervisorPid", "UpdatedAt"],
  "properties": {
    "Schema": { "const": 1 },
    "State": { "enum": ["running", "stopped"] },
    "StopKind": { "enum": ["", "requested", "interrupted", "unexpected", "window-closed"] },
    "LastUnexpectedStop": { "oneOf": [{ "type": "null" }, { "$ref": "#/$defs/lastUnexpectedStop" }] },
    "LastWindowClose": { "$ref": "#/$defs/lastWindowClose" },
    "SupervisorPid": { "type": "integer", "minimum": 0 },
    "StartedAt": { "$ref": "#/$defs/timestamp" },
    "UpdatedAt": { "$ref": "#/$defs/timestamp" },
    "UptimeSeconds": { "type": "number", "minimum": 0 },
    "RootPath": { "type": "string" },
    "Container": { "type": "string" },
    "SegmentSeconds": { "type": "integer", "minimum": 1 },
    "Disk": { "$ref": "#/$defs/disk" },
    "Prune": { "$ref": "#/$defs/prune" },
    "Restreamer": { "$ref": "#/$defs/process" },
    "Live": { "$ref": "#/$defs/live" },
    "Events": { "type": "array", "items": { "$ref": "#/$defs/event" } },
    "Cameras": { "type": "array", "items": { "$ref": "#/$defs/camera" } }
  },
  "if": { "properties": { "State": { "const": "running" } }, "required": ["State"] },
  "then": {
    "required": ["StopKind", "StartedAt", "UptimeSeconds", "RootPath", "Container", "SegmentSeconds",
                 "Disk", "Prune", "Restreamer", "Events", "Cameras", "LastUnexpectedStop"],
    "properties": { "StopKind": { "const": "" } }
  },
  "$defs": {
    "timestamp": {
      "description": "ISO 8601 round-trip ('o') in local time with offset, 7 fractional digits, or null for never.",
      "type": ["string", "null"], "format": "date-time"
    },
    "lastUnexpectedStop": {
      "type": "object",
      "required": ["At", "Reason", "SupervisorPid", "DetectedBy"],
      "properties": {
        "At": { "$ref": "#/$defs/timestamp" },
        "Reason": { "type": "string" },
        "SupervisorPid": { "type": "integer" },
        "DetectedBy": { "enum": ["supervisor", "nvr host"] }
      }
    },
    "lastWindowClose": {
      "type": "object",
      "required": ["At", "Reason", "WindowTitle", "DetectedBy"],
      "properties": {
        "At": { "$ref": "#/$defs/timestamp" },
        "Reason": { "type": "string" },
        "WindowTitle": { "type": "string" },
        "DetectedBy": { "const": "nvr warden" }
      }
    },
    "disk": {
      "type": "object",
      "required": ["FreeGB", "ReserveGB", "PruneHysteresisGB"],
      "properties": {
        "FreeGB": { "type": "number" },
        "ReserveGB": { "type": "number" },
        "PruneHysteresisGB": { "type": "number" },
        "ArchiveGB": { "type": ["number", "null"] },
        "ArchiveBytes": { "type": ["integer", "null"] },
        "ArchiveFiles": { "type": ["integer", "null"] },
        "OldestSegment": { "$ref": "#/$defs/timestamp" },
        "NewestSegment": { "$ref": "#/$defs/timestamp" },
        "MeasuredGBPerDay": { "type": ["number", "null"] },
        "ProjectedDaysRemaining": { "type": ["number", "null"] },
        "ScannedAt": { "$ref": "#/$defs/timestamp" }
      }
    },
    "prune": {
      "type": "object",
      "required": ["LastRunAt", "DeletedTotal", "FreedGBTotal", "Blocked", "LastMessage"],
      "properties": {
        "LastRunAt": { "$ref": "#/$defs/timestamp" },
        "DeletedTotal": { "type": "integer", "minimum": 0 },
        "FreedGBTotal": { "type": "number", "minimum": 0 },
        "Blocked": { "type": "boolean" },
        "LastMessage": { "type": "string" }
      }
    },
    "process": {
      "description": "Restreamer; Live is the same plus Cameras and requires Enabled=true.",
      "type": "object",
      "required": ["Enabled", "Alive", "RestartsTotal", "Paths"],
      "properties": {
        "Enabled": { "type": "boolean" },
        "RelayBase": { "type": "string" },
        "Pid": { "type": ["integer", "null"] },
        "LastPid": { "type": ["integer", "null"] },
        "Alive": { "type": "boolean" },
        "StartedAt": { "$ref": "#/$defs/timestamp" },
        "RestartsTotal": { "type": "integer", "minimum": 0 },
        "Paths": { "type": "array", "items": { "type": "string" } },
        "LastError": { "type": "string" },
        "LastErrorAt": { "$ref": "#/$defs/timestamp" }
      }
    },
    "live": {
      "allOf": [
        { "$ref": "#/$defs/process" },
        {
          "required": ["Cameras"],
          "properties": {
            "Enabled": { "const": true },
            "Cameras": {
              "type": "array",
              "items": {
                "type": "object",
                "required": ["Name", "Sub", "Main", "SubPath", "MainPath"],
                "properties": {
                  "Name": { "type": "string" },
                  "Sub": { "type": "boolean" },
                  "Main": { "type": "boolean" },
                  "SubPath": { "type": "string" },
                  "MainPath": { "type": "string" }
                }
              }
            }
          }
        }
      ]
    },
    "event": {
      "type": "object",
      "required": ["At", "Text"],
      "properties": {
        "At": {
          "description": "Log-line stamp 'yyyy-MM-dd HH:mm:ss +03:00' (NOT the 'o' format), or null when the log line did not match.",
          "type": ["string", "null"],
          "pattern": "^\\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}:\\d{2} [+-]\\d{2}:\\d{2}$"
        },
        "Text": { "type": "string" }
      }
    },
    "camera": {
      "type": "object",
      "required": ["Name", "Record", "Alive", "InputMode", "RestartsLastHour", "RestartsTotal", "LastError", "ConfigError"],
      "properties": {
        "Name": { "type": "string" },
        "SafeName": { "type": ["string", "null"] },
        "Record": { "type": "boolean" },
        "Pid": { "type": ["integer", "null"] },
        "LastPid": { "type": ["integer", "null"] },
        "Alive": { "type": "boolean" },
        "CurrentSegment": { "type": ["string", "null"] },
        "CurrentSegmentBytes": { "type": ["integer", "null"] },
        "SecondsSinceLastClosedSegment": { "type": ["number", "null"] },
        "InputMode": { "enum": ["", "relay", "direct"] },
        "RestartsLastHour": { "type": "integer", "minimum": 0 },
        "RestartsTotal": { "type": "integer", "minimum": 0 },
        "StartedAt": { "$ref": "#/$defs/timestamp" },
        "NextStartAt": { "$ref": "#/$defs/timestamp" },
        "LastError": { "type": "string" },
        "LastErrorAt": { "$ref": "#/$defs/timestamp" },
        "ArchiveFiles": { "type": ["integer", "null"] },
        "ArchiveBytes": { "type": ["integer", "null"] },
        "DownSince": { "$ref": "#/$defs/timestamp" },
        "ConfigError": { "type": "string" }
      }
    }
  }
}
```

Known gaps in the draft: `Container` is open-ended (a config key); `Cameras[].Pid` is an integer in the writer but PS 5.1 `ConvertTo-Json` of a `$null` or int is all that occurs (UNVERIFIED that no string pid ever appears); the `State` string comparisons in readers are exact (`-ne 'running'`, `!== "running"`).

---

## 2. Supervisor, host, warden, window semantics

### 2.1 The process tree today (Windows, attached mode)

```
Task Scheduler  IPCCTV-Recorder (at logon, interactive limited token)            rec:580-646; common:5739-5792
 `- powershell.exe -File %LOCALAPPDATA%\IPCCTV\start-recorder.ps1  (generated launcher stub)   common:5512-5630
     polls up to 300 s for the repo path, then runs:  cam.recorder.ps1 -Nvr    -> Start-CamNvrWindow   common:5181-5315
        |- NvrWarden: powershell.exe, CREATE_NO_WINDOW, own console (Start-CamDetachedProcess)      common:3868-3920,5268-5270
        |    creates job "cam.nvr" (CreateExclusive, KILL_ON_JOB_CLOSE), the ONLY handle to it      common:4005-4019,4707
        |    waits <=180 s for a terminal window titled "cam.nvr"; publishes _state\window-cam.nvr.json
        |    then polls IsWindow(hwnd) once a second                                                common:4718-4761
        `- wt.exe -w ipcctv-nvr new-tab --title cam.nvr --suppressApplicationTitle powershell.exe -NoExit -EncodedCommand <pane>
             `- NvrHost (the pane): joins job cam.nvr, then Start-CamNvrHost                     common:5284-5294,4218-4255,4919-5054
                  `- Supervisor: powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "& 'cam.recorder.ps1' -Run"
                       (inherits the pane's console and its cam.nvr job membership)                  common:4983-4991
                       |- creates + joins its own job "cam.supervisor.<hash>" BEFORE starting children  rec:3771-3778; common:4162-4215
                       |- mediamtx (recording relay)      env MTX_PATHS_<NAME>_SOURCE per camera       rec:2088-2218
                       |- mediamtx (live view, optional)                                                rec:2389-2449
                       `- ffmpeg x N (one per recording camera)                                         part A

Dashboard (separate, optional):  Task IPCCTV-Dashboard -> start-dashboard.ps1 -> cam.ps1 -Start -Dashboard -> mpv.cam.autolayout.autorestart.ps1
   DashboardWarden (job "cam.dashboard.panes", window "cam.dashboard"; also closes window "cam.health")  common:4851-4871
   six panes (powershell -> Invoke-CamViewer -> mpv.com -> mpv.exe) in wt -w cctv-grid; health strip in wt -w cctv-health  (mpv script:327,527)
UI:  Task IPCCTV-Ui -> start-ui.ps1 -> hidden powershell "pump" appending stdout to camui.log -> camui.exe              common:5943-6152
```

Job names: `cam.nvr` (common:4125-4135), `cam.dashboard.panes` (4099-4108), `cam.supervisor.<first 4 bytes of SHA-256 of the lower-cased RootPath, hex>` (4162-4187). Window titles `cam.nvr` (4138-4146), `cam.dashboard` (4111-4122), `cam.health`; wt window name `ipcctv-nvr` (4149-4159).

Role identification (`Get-CamProcessInventory`, common:4288-4426): Supervisor = the powershell.exe parent of any ffmpeg.exe or mediamtx.exe; NvrHost / NvrWarden / Warden / Pane / Health = a powershell.exe whose command line (or base64-decoded `-EncodedCommand` payload, common:4258-4285) contains `Start-CamNvrHost` / `Start-CamNvrWarden` / `Start-CamDashboardWarden` / `Invoke-CamViewer` / `cam.health`; Viewer = mpv* (owned by a pane or "no pane owns it"); Recorder = ffmpeg.exe; Relay = mediamtx.exe. Nothing is found by image name or window title alone (common:4296-4306). The keep list that every dashboard-side stop honours = Supervisor, Recorder, Relay, NvrWarden, NvrHost (common:4530-4550); `Stop-CamProcessList` also refuses its own pid and all its ancestors (4476-4527).

### 2.2 Guarantees as OS-neutral behaviours

"Windows accident" column: **mechanism** = the way it is done today is a Windows/Windows-Terminal artefact; **behaviour kept** = the behaviour itself is a requirement (plan:242-260).

| # | Guarantee (OS-neutral) | Mechanism today | Proving check | History (todo) | Windows accident? |
|---|---|---|---|---|---|
| G1 | Closing the NVR terminal stops the supervisor, relay, live relay, every recorder child and the viewers, with nothing orphaned | Job `cam.nvr` with KILL_ON_JOB_CLOSE; the warden holds the only handle outside the window; the pane joins it first so children inherit membership (common:4005-4019,4072-4085,4218-4255). On window loss the warden closes the handle FIRST (common:4770), alerts, patches status, then stops the viewers through `cam.ps1 -Stop -Dashboard` or terminates their job directly (4810-4827) and closes extra windows (4829-4832) | `windowclose` (members gone after the close) | [50] four supervisor deaths 2026-10-12 (console adopted as a tab; CTRL_CLOSE_EVENT kills PowerShell with no `finally`, todo:584-602); [52] orphans for 8 s until the handle close was moved first (todo:617-626, TODO-MEASURED); [51],[53] | Mechanism: YES (window as owner token, job objects, out-of-window warden because a pane cannot see its own window die, todo:442-447). Behaviour kept: YES, Operator requirement |
| G2 | A deliberate close is never "repaired": nothing restarts the NVR after a window close | Host and supervisor share the console and die with the window (common:4936-4939); the warden contains no restart code, only alert 103 and the status patch (4782-4808) | `windowclose` (point 4: no restart) | [67] Operator decision after 66 s with nothing recording (todo:877-881) | Behaviour kept. The "close looks identical to a crash" problem is a Windows one |
| G3 | The close raises one alert (id 103, Warning, worded as a close, not a crash); an orderly stop raises none | `-AlertOnWindowClose` only on the NVR warden (common:4874-4903). `Stop-CamNvr` terminates the warden WHILE the window is open so it never reaches the window-gone branch (5333-5340) | `windowclose` (alert on close; terminated warden raises nothing) | [67] "37 events before and 37 after" an orderly stop (todo:899-903) | Mechanism: YES. Behaviour kept |
| G4 | A supervisor that stops unexpectedly is restarted in place: alert 100 (Error), restart after 5 s, alert 101; at most 5 restarts in 30 min, then alert 102 and it stays down; never restarted when the stop was requested/interrupted or exit 20 | `Start-CamNvrHost` loop (common:4919-5054; defaults `MaxRestarts 5`, `RestartWindowMinutes 30`, `RestartDelaySeconds 5`); waits up to 20 s for the previous `cam.supervisor.*` job to vanish before restarting (5041-5045); patches `StopKind=unexpected` itself when the supervisor was killed (5017-5025) | `crashsafe` (restart, StopKind, LastUnexpectedStop with pid, exactly one id-100 Error alert, orderly stop not restarted and no alert). No check found for the budget or alert 102 | [55] recovered in 11 s and 13 s (todo:657-659); [56] | Mechanism: partly (a host process exists because the pane cannot tell close from crash). Restart policy: portable |
| G5 | If the supervisor dies, however abruptly, its children die with it | Supervisor creates + joins job `cam.supervisor.<hash>`, keeps the handle for the process life, BEFORE starting any child (rec:3771-3778; common:4043-4068,4190-4215). If creation fails (e.g. the name exists) it only logs a WARNING and records unprotected | NONE found. TODO-MEASURED only: "4 ffmpeg + 1 MediaMTX to zero in under one second", twice (todo:654-656,694-696). `procs` only checks for leftovers after tests | [54] | Mechanism: YES. Behaviour: portable (process group + parent-death signal on Linux, job on Windows; plan:251) |
| G6 | Orderly stop: one request mechanism, children stopped in order (cameras, then live relay, then relay), final status `stopped/requested`, no orphan | Flag file `_state\stop.request` created by `-Stop` (rec:3533); supervisor tests it at the top of each loop pass (rec:3838-3843) then runs the `finally` (rec:4133-4191); removes the flag (4187). A flag, not a signal: the supervisor normally has no console, and a console event would hit every ffmpeg at once (common:2573-2585) | `crashsafe` point 4; `relay` (opt-in, needs cameras) for the relay orphan; `procs` | [10] `-Stop` graceful | Flag-file mechanism is OS-neutral: candidate to KEEP |
| G7 | `-Stop` waits and verifies, never kills the supervisor | polls `Get-Process` every 500 ms up to `StopTimeoutSeconds` (30); on timeout prints DID NOT STOP, leaves the flag in place, `exit 1`, kills nothing (rec:3536-3548). After exit, kills any survivor of `Cameras[].LastPid` (only if it is named `ffmpeg`), `Restreamer.LastPid` / `Live.LastPid` (only if named `mediamtx`) (rec:3554-3602). If no supervisor is running: reports it, removes a leftover flag, returns 0 (3520-3527); a new supervisor also clears a leftover flag at start (3761-3764) | `crashsafe` (stop honoured), none for the survivor kill | [10], [63] | No |
| G8 | Only one supervisor per archive | At start: snapshot + heartbeat verdict; if running, message and `exit 20` (rec:3748-3753); the host reads 20 as "do not restart" (common:5010-5013). Secondary: the per-root job name | NONE found. plan:360-362 records it as check-then-act, blind when the status file is missing/unreadable/for another root; S1 (OS lock) is the stated Go spec | (design note rec:3745-3747) | Heartbeat approach is weak; the invariant is portable (OS lock) |
| G9 | `cam.ps1 -Stop` stops everything in scope and PROVES it by counting the same roles before and after; exit 1 if anything in scope survives | cam:526-633: dashboard first (its panes hold the second camera session), then the recorder's own `-Stop`, a 20 s wait, a direct kill of leftover Supervisor/Recorder/Relay by pid, then `Stop-CamNvr` last | no dedicated check found; timings in [79] (todo:1122-1125: stop everything 17.3 s) | [39],[40],[63] | Role census by process inspection: YES in mechanism |
| G10 | Start is honest: exit 1 + "NOT RECORDING" when the supervisor does not report running | cam:636-736: runs the recorder task via `schtasks /run`, waits up to 60 s for a fresh heartbeat (measured 18-32 s), then waits for the relay to serve (>= 1 path, `RelayReadyWaitSeconds` min 5 s, poll `RelayReadyPollMs`), then runs the dashboard script | no dedicated check found | [63] (false success + 20 s limit), [59] page reads status back | Task Scheduler indirection: YES |
| G11 | A start after an orderly stop always works; a stale owner record is never trusted | `Resolve-CamWindowStart` (common:5057-5178): recording + window found -> AlreadyOpen; recording + window not identifiable -> throw, change nothing; not recording but window/job/warden/host present -> clear the corpse (`Stop-CamWardenedWindow`, 5355-5422) then wait <= 10 s until none of its own remain, else throw. Window records valid only while the named warden pid is alive and started no later than the record (3642-3679) | `nvrstart`, `windowid` | [63], [81], [84] (5 min 34 s with nothing recording, todo:1249-1262) | YES, almost entirely (HWND records, titles, tab adoption). Portable lesson: validate an owner record by pid + start time |
| G12 | The job is armed only against a window that exists: if the window never appears within 180 s the warden DISARMS and leaves its members alone | common:4725-4730 (`Disarm`) | NONE found | design comment common:4655-4658 | YES |
| G13 | Roles are found by what a process does, never by image name or window title; dashboard stops can never touch the recorder | common:4288-4426,4530-4550,4476-4527 | `procs`; `nvrstart` | [39] six stale health processes; killing by window title destroyed the Operator's session (common:4296-4302) | YES in mechanism (CIM, base64 command-line decoding) |
| G14 | One control action at a time (camui) | `actionMu.TryLock` -> HTTP 409 (ui/control.go:112-116) | none found (no 409 assertion in `ui/*_test.go`) | [59] action lock held 4 min by inherited pipes (todo:762-767) | No |
| G15 | Logon start survives "repo path not mounted yet" and transient failure | launcher stub polls <= 300 s then exits 2; task `RestartCount 3` every 1 min; `ExecutionTimeLimit` 0; `MultipleInstances IgnoreNew` (common:5539-5601,5739-5792) | `autostart`, `installsandbox` | [35],[36] | YES (WSL share), Task Scheduler |

### 2.3 Facts that are easy to lose

- The NVR host decides expected-or-not from what the supervisor RECORDED (stop flag file, persisted `StopKind`, exit 20), "never from an exit code alone, because a killed process has no say in its exit code" (common:4946-4954).
- `Start-CamNvrWindow` returns without error and the `-Nvr` command exits 0 even when no window appeared (rec:535-539 print and `return`); a `Resolve-CamWindowStart` throw exits non-zero. UNVERIFIED exit codes (code read).
- The supervisor's `-Run` is only meant to be started by the host; running it from an ordinary terminal makes that terminal "the window that owns it" and, in a shared Windows Terminal window, closing it kills the recorder (docs/troubleshooting.md:8; rec:11-15).
- Known open items: [82] `Stop-CamTerminalWindow` resolves by title only (todo:1239-1245); the title of a Windows Terminal window follows its active tab, so a foreign console adopted into the NVR window hides it from a title search ([81]); `windowingBehavior` mismatch between code comments (`useExisting`) and the machine setting (`useNew`) is an open question in [84] (todo:1274-1276).

### 2.4 Per component: what each one guarantees

| Component | Guarantees it provides (OS-neutral) | Source |
|---|---|---|
| Launcher stub | waits for the target to exist; exits non-zero on failure so the task policy retries; reports the target's exit code; never copies the scripts | common:5539-5601 |
| NvrWarden | outlives the window by being outside it; arms the kill only when a window exists (else disarms); on window loss: sweeps all members first, alerts (103), records `window-closed`, stops the viewers, closes the health window; removes its own window record last; publishes the handle it watches | common:4634-4848,4874-4903 |
| `cam.nvr` job | everything started inside the NVR window dies when the last handle (the warden's) closes | common:4005-4019 |
| NvrHost | restarts an unexpectedly stopped supervisor in place (budget 5 per 30 min), classifies the stop from recorded state, patches `unexpected` into status.json when the supervisor could not, alerts 100/101/102, never restarts a requested/interrupted stop or exit 20 | common:4919-5054 |
| Supervisor | one per archive (refuses at start); owns its children and a job that kills them if it dies; reconciles `cameras.json` every 5 s; writes the heartbeat every 5 s; prunes; stops children in order on request and writes a truthful final status (`requested`/`interrupted`/`unexpected`); judges a camera down on file progress alone | rec:3708-4197 |
| `cam.supervisor.<hash>` job | the supervisor's death, however abrupt, takes MediaMTX, live MediaMTX and every ffmpeg | common:4162-4215 |
| Dashboard warden + `cam.dashboard.panes` job | closing the grid window stops all panes and their mpv, and closes the health window; independent of the recorder | common:4851-4871; todo [40] |
| `cam.ps1 -Stop` | everything in scope is stopped and counted before and after; the recorder is never killed by name or title | cam:526-633; common:4476-4550 |
| Scheduled Tasks (3) | start the NVR, the dashboard and camui at logon, once, without elevation, with no run-time limit | common:5739-5792 |
| camui | one state-changing action at a time; read-only status path; never starts or kills a process itself; redacts every response | ui/control.go:15-25,112-116; ui/api.go:298-349 |

---

## 3. Control surface

### 3.1 `cam.ps1` (the single place; `[CmdletBinding(DefaultParameterSetName='Status')]`, cam:94-133)

| Switch | Semantics | Exit code | Source |
|---|---|---|---|
| (none) / `-Status` | read-only report of every component: recorder (supervisor verdict, ffmpeg count, NVR host/warden counts, job, window by published handle or title, STALE warning), relay (mediamtx count, `RelayApi` denied/serving paths), live view (only when enabled, from status.json), dashboard (grid window, warden, panes, mpv, health, ORPHANS/WARNING lines), ui (camui serving/NOT serving/stopped, binary, address, auth, logon-task state), logon tasks, process table. Scope switches are accepted by the parameter set but IGNORED by the report | always 0 | cam:331-492,1361-1366 |
| `-Start [-Recorder\|-Dashboard]` | recorder: if heartbeat running -> "already running"; else requires task `IPCCTV-Recorder` (`schtasks /run`), waits <= 60 s for heartbeat. dashboard: if recorder in scope and relay enabled, waits for >= 1 relay path, then runs `mpv.cam.autolayout.autorestart.ps1`. Neither switch = both | 1 when the recorder problem is set (task missing, or no heartbeat in 60 s) else 0. A dashboard launch failure is not reflected (UNVERIFIED) | cam:636-736,1356 |
| `-Stop [-Recorder\|-Dashboard]` | see G9. Dashboard stop: terminate job `cam.dashboard.panes`, stop remaining Warden/Pane/Viewer/Health by pid with the keep-list guard, `WM_CLOSE` the `cam.dashboard` and `cam.health` windows (common:4553-4631). Recorder stop: recorder's own `-Stop`, 20 s orphan wait, direct kill of leftovers, then `Stop-CamNvr` (warden first, then host) | 0 if everything in scope is zero afterwards, else 1 | cam:526-633,1355 |
| `-Install [-WhatIf]` | preflight, then components in order restreamer config, recorder task, dashboard task, UI task; each: test, install via the granular switch, re-test. Starts nothing. See section 5 | 0 only when every component is in place; 1 on failed/skipped component or failed preflight | cam:1160-1264,1353 |
| `-Uninstall [-WhatIf]` | reverse order; removes tasks, launchers, MediaMTX config; keeps logs, archive, cameras.json, recorder.config.json, credentials; stops nothing; leaves the event source | 0 unless a removal failed | cam:1267-1326,1354 |
| `-InstallDashboardAutostart` / `-RemoveDashboardAutostart` | task `IPCCTV-Dashboard` -> launcher `start-dashboard.ps1` forwarding `-Start -Dashboard`; remove is a no-op when absent; never touches the recorder task | 0 | cam:739-833 |
| `-InstallUi` / `-RemoveUi` | task `IPCCTV-Ui` -> `start-ui.ps1` which starts `camui.exe` in its own windowless console and pumps its stdout to `camui.log`. `-InstallUi` returns 1 and installs nothing if `camui.exe` is missing. Remove never stops a running camui | 0 (install: 1 if binary missing) | cam:836-955 |
| `-Recorder`, `-Dashboard` | scope for `-Start`, `-Stop` (and accepted by `-Status`). Neither = both (cam:170-171) | | cam:123-132 |
| `-WhatIf` | accepted only with `-Install` / `-Uninstall`; any other action throws `-WhatIf previews -Install and -Uninstall only.` | throw | cam:1348-1350 |

Note: `cam.ps1` has no `-Nvr`, `-Run`, `-Enable`, `-Disable`; those belong to the recorder script.

### 3.2 `cam.recorder.ps1` (`DefaultParameterSetName='Run'`, rec:49-100)

| Switch | Semantics | Exit code | Source |
|---|---|---|---|
| `-Run` | the supervisor (foreground, long-running). Refuses when a supervisor is already running | 0 clean; 20 already running; 21 loop threw; interrupted: UNKNOWN | rec:3708-4197,4242 |
| `-Nvr` | open the NVR window (warden, wt, host, supervisor); idempotent: "already open" when the window is found; clears a dead NVR first; throws when recording but the window cannot be identified | 0 normally (also when no window appeared); non-zero on throw | rec:505-543; common:5181-5315 |
| `-Enable <name\|all>`, `-Disable <name\|all>` | set `Record` in `cameras.json` via atomic write; exact name match (PowerShell `-eq`, case-insensitive) beats the `all` keyword; unknown target prints valid targets; no-op "already ON/off" writes nothing; `-Enable` first checks the name can become a directory. The running supervisor applies it within `ReconcileSeconds` | 1 when the name matches nothing; else 0 | rec:209-343 |
| `-List` | table of cameras with Record, quality, IP, plus supervisor verdict | 0 | rec:241-277 |
| `-Status` | `Show-CamStatus` (section 1.7) | 0 | rec:3398-3500 |
| `-Stop` | orderly stop (G6, G7) | 1 on timeout, else 0 | rec:3503-3605 |
| `-InstallRestreamer` / `-RemoveRestreamer` | write / delete the generated MediaMTX config at `RestreamerConfigPath` (section 4); creates `relay_api_creds.xml` if absent; checks the binary; warns on non-loopback `RelayBase`; refuses a `\\wsl*` config path | 0; throw on wsl path | rec:383-484 |
| `-InstallAutostart` / `-RemoveAutostart` | task `IPCCTV-Recorder` + launcher forwarding `-Nvr`; remove keeps `autostart.log` | 0 | rec:580-684 |
| `-InstallEventSource` | register the `IPCCTV` Application-log source (needs elevation once; optional) | 0 | rec:546-577; common:2480-2519 |
| `-SelfTest [-Offline] [-MaxConcurrency 2..16, default 5]` | pre-flight report (tooling, camera streams, concurrency, crash safety, disk projection, restreamer, camera list, Record round trip, atomic write, pruner guards, prune dry run, status surface). `-Offline` skips every RTSP session | 1 if any FAIL after the summary; the early-return paths (tooling fails, no camera reachable) skip the summary and their exit code is UNVERIFIED | rec:1479-1574 |
| (no switch) | prints the usage list | 0 | rec:4245-4259 |

### 3.3 What `camui` calls and how it reads the result

| camui endpoint | What it runs / reads | Result handling | Source |
|---|---|---|---|
| `GET /api/status` | file read of status.json (no PowerShell); `schtasks.exe /Query /TN <IPCCTV-Recorder\|IPCCTV-Dashboard> /FO LIST /V` (cached 5 s, 10 s timeout); the ack sidecar | missing/invalid file -> `statusFile.error`, `status` null, verdict "no status file has been written yet" | ui/api.go:123-168,199-288 |
| `POST /api/control {scope: all\|recorder\|dashboard, action: start\|stop}` | `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File cam.ps1 -Start\|-Stop [-Recorder\|-Dashboard]`, 180 s timeout, console output to a temp FILE (not a pipe: inherited panes held the pipe open and froze the action lock for minutes, todo:762-767) | `ok = no error and exit 0`; HTTP 200 on ok, 502 on non-zero exit or error, 409 when another action runs, 422 bad scope/action; output scrubbed (URLs and IPv4 removed), max 200 lines. The page then reads `/api/status` back and reports "returned success, but the recorder is still not running" (todo:776-779) | ui/control.go:77-129,203-276 |
| `POST /api/cameras/{name}/record {enabled}` | `cam.recorder.ps1 -Enable\|-Disable <name>`, 60 s; camera existence pre-checked in Go (404) | same response shape | ui/control.go:131-191 |
| `PUT /api/cameras`, `PUT /api/config` | embedded `ui/scripts/camui-apply.ps1 -Repo <dir> -Kind cameras\|config -InputPath <json> -OutputPath <json>`, 60 s; the script dot-sources `cam.common.ps1` and writes through `Set-CamList` / `Move-CamFileAtomic`; editable config keys only `SegmentSeconds`, `ReserveGB`, `PruneHysteresisGB`, `MaxRetentionDays`; result `{ok, error, changed, message, path}` + exit 0/1. The recorder reads `recorder.config.json` only at startup; `cameras.json` is picked up within `ReconcileSeconds` | | ui/settings.go:526-570; ui/scripts/camui-apply.ps1:38-52,89-162,182-256 |
| `POST /api/acknowledge` | sidecar write only | | ui/notice.go:70-115 |
| Confirmation rule (page) | confirm exactly the actions that stop footage: stop everything, stop recorder, record off, remove camera row | | todo:755-758 |

Everything camui does that changes state is delegated to PowerShell ("a second implementation in Go would be a second thing to get wrong", ui/control.go:15-25). In the Go rewrite this indirection disappears; the response contract (`ok/exitCode/output`, 409 serialisation, read-back of status) is the part to keep.

### 3.4 Live pickup of the camera list

The supervisor stats `cameras.json` every `ReconcileSeconds` (5 s) and runs `Invoke-CamReconcile` when `LastWriteTimeUtc` changed (rec:3846-3877): children whose camera is no longer recording are stopped without touching restart count or backoff; a changed `IP` or `Quality` (compared on those fields, never on the URL) stops and recreates the child; new recording cameras start at once with `NextStartAt = now` (no inherited backoff) (rec:3661-3702). A reconcile that throws keeps the current set (rec:3871-3875). The first reconcile logs no "now set to record" lines (`-First`).

---

## 4. Relay (recording MediaMTX) and live-view MediaMTX

Checks that exercise this section: `inputs`, `relay` (opt-in, real cameras), `relayapi`, `livecfg`, `livesecret`, `livewhep`, `liveondemand` (opt-in). History: [17],[21],[22] adoption; [37] pane promotion; [88],[89] credential and control-API exposure; Stage 1A/1B/2 designs in `tasks/portable-nvr/live-*.md` (not read in detail).

### 4.1 Recording relay

| Topic | Rule | Source |
|---|---|---|
| Role | transport fan-out only: one camera session per camera pulled by MediaMTX and re-served on loopback so recorder and viewers do not each take one of the camera's two sessions. MediaMTX's own recorder is deliberately NOT used. Verified against MediaMTX v1.21.2 | mediamtx.template.yml:14-18; common:785-795; rec:432 |
| Path name | safe name lower-cased, every char outside `[a-z0-9]` removed; empty result is an error; two cameras mapping to the same path throw. Reason: MediaMTX reads a path's source from `MTX_PATHS_<NAME>_SOURCE` and its env loader cuts the key at the FIRST underscore (`workshop_entrance` silently becomes a stray `workshop` plus an offline path; TODO-MEASURED 2026-10-11) | common:812-867 |
| Source environment variable | `MTX_PATHS_<UPPER(path)>_SOURCE` = the full camera RTSP URL WITH credential, set only in the child's environment block; the file keeps `source: publisher`; never on a command line | common:870-875; rec:2157-2160 |
| Config generation | `mediamtx.template.yml` with five plain-text replacements: `__RTSP_ADDRESS__` and `__API_ADDRESS__` (host:port with the scheme stripped from `RelayBase` / `RelayApiBase`, defaults `rtsp://127.0.0.1:8554`, `http://127.0.0.1:9997`), `__API_USER__`, `__API_PASS__` (`sha256:` + base64 of SHA-256 of the password), `__PATHS__` (per camera: `  <path>:`, a comment line `# <Name> at <IP> - source supplied at launch by <ENV>`, `    source: publisher`). Line endings normalised to CRLF. Written UTF-8 without BOM to `<RestreamerConfigPath>.tmp`, then moved over the target (MediaMTX watches the file and reloads) | common:1462-1513; common:889-904,340-341; rec:358-380 |
| Where the file may live | a LOCAL NTFS path. `\\wsl*` is refused at install and in the preflight because MediaMTX watches its config with `ReadDirectoryChanges`, which the 9P share lacks (it exits at once, TODO-MEASURED) | rec:408-410; cam:1038-1040; mediamtx.template.yml:20-23 |
| Control-API login | `relay_api_creds.xml` beside the scripts: PSCredential, user `camrelayapi`, password = 32 random bytes as 64 hex chars, exported with `Export-Clixml` (DPAPI, current Windows user), created only when missing (`-Create`), written temp-then-rename; the password is registered for redaction, the user name is not | common:199-242 |
| API probe | `GET <RelayApiBase>/v3/paths/list` and `/v3/paths/get/<escaped name>` with `Authorization: Basic base64(user:pass)`, loopback only, system proxy bypassed, timeout `RelayProbeTimeoutMs` (default 1500, floor 100). Answers: `ok` (2xx JSON), `denied` (HTTP 401 or 403), `down` (anything else). A credentialed request that TIMES OUT is repeated anonymously, because MediaMTX delays a wrong login by up to ~4 s but refuses a missing one at once: an immediate 401 proves the relay is alive and wanted a login (TODO-MEASURED 2026-10-15: wrong login 704-3784 ms, none 0-1 ms, right <10 ms) | common:930-1019 |
| `denied` means "relay running" | readiness helpers return TRUE / every camera's path when denied, so no consumer is sent to a direct connection (that would put the password on a command line and spend the camera's second session). The supervisor alerts ONCE per episode (id 130, Error) and logs recovery | common:1032-1095; rec:2055-2085 |
| Ready paths | list items with a boolean `ready`; the supervisor does ONE list query per cycle, on a status tick or when any child is due to start, and shares it | common:1064-1095; rec:3899-3907 |
| Start | requires the exe and the config to exist (else throws -> backoff); builds the path map from `cameras.json` at EVERY start; ensures the login; if the file's auth block is not the stored one it REWRITES the config (upgrade path); a stale path set only logs a WARNING and starts anyway; env block set; stdout piped (redacted) and read asynchronously; stderr left redirected and drained after exit | rec:2088-2218 |
| Startup wait | blocks up to `RelayStartupWaitSeconds` (20) polling `v3/paths/list` every 500 ms until the answer is not `down` (denied counts), then logs how many paths serve. Waits for the API, NOT for every path (an offline camera's path never becomes ready) | rec:2180-2217 |
| Per-child hold | a child is held back from starting while its own path is not serving, until `relay.StartedAt + RelayReadyWaitSeconds` (15), only while the relay process is alive; after that it starts on the direct input and logs it. Measured why: killing MediaMTX and letting all recorders reconnect directly left 5 of 6 paths unable to serve again | rec:3909-3936,4013-4036 |
| Direct -> relay promotion | a running child on `direct` whose path becomes ready is stopped without touching restart count or backoff and restarted on the relay at once | rec:3963-3979 |
| Restart policy | relay exit: `RestartCount+1`, `LastError="mediamtx exited with code N after Ns"`, backoff `BackoffStartSeconds` 3 doubling to `BackoffMaxSeconds` 60, reset when it ran >= `HealthyResetSeconds` 300. Start failure: same backoff, one console line + log line. No stall watchdog (consumers ask the API per attempt) | rec:2279-2337; rec:2002-2029 |
| Stop | hard kill, wait <= 10 s, drain output/stderr; after cameras and live view | rec:2246-2276; rec:4164-4176 |
| Config currency | "current" = the SET of path names equals the camera list AND the auth block (from `authMethod:` to the end of `authInternalUsers:`) equals the rendered one, compared case-sensitively. The comment holding the camera IP is ignored (a DHCP change on 2026-10-13 made a text compare cry wolf) | common:1564-1649 |
| Removal | `-RemoveRestreamer` deletes the generated config, leaves the binary and `RestreamerEnabled` alone | rec:453-484 |

Keys that decide correctness in the relay config (all CODE, mediamtx.template.yml): `authMethod: internal`; `authInternalUsers` = user `any` (empty pass, `ips: []`, actions publish/read/playback) plus the API user (`ips: ["127.0.0.1","::1"]`, action `api` only) - without this block MediaMTX lets any local process call the API and read every camera's source URL including the password (todo [89]); `api: yes` + `apiAddress`; `rtsp: yes`, `rtspTransports: [tcp]`, `rtspAddress` (loopback; anonymous read is acceptable ONLY because of that); `rtmp hls webrtc srt moq metrics pprof playback: no`; `pathDefaults.rtspTransport: tcp`; `pathDefaults.sourceOnDemand: no` (hold the camera session continuously); `logDestinations: [stdout]` (a file destination would write the credentialed URL on the first connection error); `writeQueueSize: 2048`, read/write timeout 10 s; one path per camera with `source: publisher`.

Windows-specific vs portable: the environment-variable scheme, path naming, API protocol, readiness logic, backoff and config keys are all portable. Windows-specific: DPAPI storage of `relay_api_creds.xml` (Go cannot read Clixml, common:1868-1870), CRLF output, the `\\wsl` restriction, `CreateNoWindow` process start, the supervisor job that kills it.

### 4.2 Live-view instance (second MediaMTX; WebRTC only; off by default)

| Topic | Rule | Source |
|---|---|---|
| Enabled | `LiveViewEnabled` must be a JSON boolean `true`; checked strictly. All `Live*` keys are validated at config load even when off; an unknown key starting with `Live` is refused. Required: loopback WHEP address `127.0.0.1:<port>`, UDP media address loopback / `0.0.0.0` / one RFC 1918 IPv4, `LiveAdvertiseHosts` consistency (<= 4 private IPv4, empty only for loopback), `LiveSubStream` `^[A-Za-z0-9_\-]{1,32}$`, `LiveCloseAfterSeconds` 1-3600, not the relay's own address, no `\\` paths | common:508-595,1665-1677 |
| When it runs | only while the RECORDING relay process is alive; a failed start logs one redacted line and retries on the relay's backoff; recording never depends on it | rec:2480-2548 |
| Paths | per camera `<relaypath>sub` (camera stream `LiveSubStream`, default `stream2`, pulled on demand, credentialed URL via env `MTX_PATHS_<RELAYPATH>SUB_SOURCE`) and `<relaypath>main` (`source: <RelayBase>/<relaypath>`, no credential, no extra camera session). A camera whose `Quality` already equals `LiveSubStream` gets only `main`. A name colliding with a relay path or another live path throws | common:1689-1751 |
| Config | rendered in code (not from the template) every start; rewritten only if it differs (`-ceq`), temp-then-rename; keys that matter: `authMethod: internal` with ONE user (`camlive`, `sha256:` hash, `ips: ["127.0.0.1","::1"]`, action `read`) and NO anonymous user; `api/metrics/pprof/playback/rtsp/rtmp/hls/srt/moq: no`; `webrtc: yes`, `webrtcAddress` loopback, `webrtcEncryption: no`, `webrtcAllowOrigins: []` and `webrtcTrustedProxies: []` (no CORS, so a foreign page cannot POST), `webrtcLocalUDPAddress`, `webrtcLocalTCPAddress: ""`, `webrtcIPsFromInterfaces: no`, `webrtcAdditionalHosts: ["127.0.0.1", ...LiveAdvertiseHosts]`; `pathDefaults.sourceOnDemand: yes`, `sourceOnDemandStartTimeout: 10s`, `sourceOnDemandCloseAfter: <LiveCloseAfterSeconds>s`. Reason for a separate instance: WebRTC signalling answers any website's cross-origin request by default and a user allowed to read RTSP may read WebRTC | common:1754-1840; rec:2362-2386; common:1652-1660 |
| Secret | `LiveSecretPath` or `%LOCALAPPDATA%\IPCCTV\live_view_secret.txt` (local per-user: the repo share cannot hold an ACL). 32 random bytes as 64 lowercase hex, plain ASCII, no newline, so Go can `os.ReadFile` it. Created with an ACL naming only the current Windows user, inheritance removed, set AT CREATION (`FileStream` with a `FileSecurity`), then moved into place; a file not matching `^[0-9a-f]{64}$` is refused (delete it to regenerate). Registered for redaction; appears only as its SHA-256 in the config. camui reads it with the same regex, tolerating a trailing newline, and expands `%VAR%` in `LiveSecretPath` | common:1843-1918; ui/live.go:63-66,158-162,228-245 |
| Process | environment block carries only the sub URLs; stdout is the log (redacted); working directory = the config's folder; hard-killed before the relay on stop | rec:2389-2477 |
| Status | the `Live` section (1.3); `cam.ps1 -Status` "live view" section | rec:2932-2962; cam:376-392 |

Windows-specific vs portable: the whole config and the secret format are portable; the access control (ACL, `icacls` assertions in `livesecret`) is Windows; on Unix the equivalent is mode 0600.

---

## 5. Install and autostart

| Component (install order) | Creates | Idempotence test ("Current") | Source |
|---|---|---|---|
| 1 restreamer config | the rendered MediaMTX config at `RestreamerConfigPath`; creates `relay_api_creds.xml` if missing | file exists AND path-name set equals `cameras.json` AND auth block matches | cam:1081-1099; rec:383-450 |
| 2 recorder logon task | scheduled task `IPCCTV-Recorder` + launcher `%LOCALAPPDATA%\IPCCTV\start-recorder.ps1` + (on run) `autostart.log` | task registered, exactly ONE action equal to `powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "<stub>"`, stub exists, stub's `$Target` line equals the repo's `cam.recorder.ps1` path. The launcher text is NOT compared (an older launcher still counts) | cam:1100-1108; common:5687-5736,5655-5684; rec:497-502,580-646 |
| 3 dashboard logon task | task `IPCCTV-Dashboard` + `start-dashboard.ps1` forwarding `-Start -Dashboard`, log `autostart-dashboard.log` | same test, target `cam.ps1` | cam:1109-1117,739-795 |
| 4 UI logon task | task `IPCCTV-Ui` + `start-ui.ps1`; logs `autostart-ui.log`, `camui.log` (rolled to `camui.log.prev` above 10 MB at launcher start) | same test, target `camui.exe` (`$Exe` line) | cam:1118-1126,836-902; common:5943-6152 |
| optional event source | `IPCCTV` Application-log source (HKLM; elevation) | not part of the exit code; `-Install` registers it only when already elevated, otherwise prints the elevated command | cam:1235-1249; common:2480-2519 |

Task definition (all three, `Register-CamLogonTask`, common:5739-5792): trigger at logon of the current user; principal interactive logon, `RunLevel Limited` (no elevation); settings `AllowStartIfOnBatteries`, `DontStopIfGoingOnBatteries`, `ExecutionTimeLimit` zero (the default 3-day limit would kill a 24/7 recorder on day three), `MultipleInstances IgnoreNew`, `RestartCount 3`, `RestartInterval` 1 min; `-Force` replaces an existing registration (idempotent; "exactly one task with this name" is printed). `Register-ScheduledTask` is used instead of `schtasks /create` because the latter is denied for a standard user (TODO-MEASURED, common:5466-5471). A task points at a generated LAUNCHER, not at the repo script, because the repo may be on `\\wsl.localhost`, which does not exist at logon.

Launcher stub (script launcher, common:5539-5601): logs to its own log file; polls for the target path every `PollSeconds` (5) up to `TimeoutSeconds` (300), then `exit 2` ("so the task restart policy tries again"); runs `& <Target> <switches>` (switches must match `^-[A-Za-z][A-Za-z0-9]*$`, written as SOURCE TEXT because array splatting does not bind switch names, common:5603-5614); a thrown error logs and `exit 3`; otherwise exits with the target's `$LASTEXITCODE`. UI launcher (common:5983-6133): waits for the exe (exit 2), exits 0 if the listen address already answers (no second camui), rolls the log, starts a windowless pump `powershell` that appends camui's stdout to the log, waits <= 30 s for the address to serve (exit 4 on failure), exit 3 if the process cannot start. Listen address = `listen` in `camui.config.json` else `127.0.0.1:8787` (must equal `defaultListen`, common:5825; ui/config.go:21).

Remove: unregister (no-op when absent; re-queried, throws if one remains), delete the stub, delete its folder only if empty, KEEP the launcher logs (the only record of a failed logon start) (common:5633-5652,5795-5811). `-Uninstall` order is the reverse of install and verifies each removal by re-testing (cam:1267-1326).

Preflight (`Get-CamInstallPreflight`, cam:987-1063) - every row printed in one pass, then if any HARD row fails: "Nothing was installed or changed", exit 1:

| Row | Passes when | Hard? |
|---|---|---|
| `recorder.config.json` | loads (or absent: defaults) | yes |
| `cameras.json` | parses, lists >= 1 camera, relay path map builds without collisions | yes |
| camera credentials | `camera_creds.xml` EXISTS (never opened) | yes |
| ffmpeg | file at `FfmpegPath` | yes |
| MediaMTX | file at `RestreamerExePath` | yes |
| MediaMTX config path | not a `\\wsl*` path | yes |
| mpv | file at `$MpvPath` parsed from the dashboard script's AST | yes |
| Windows Terminal | `wt.exe` on PATH | yes |
| ScheduledTasks module | `Register-ScheduledTask` available | yes |
| `camui.exe` | file exists | NO: a missing binary skips only the UI component |

Components are independent: a failure does not stop the next; each install is verified by running the component's test again (cam:1160-1264). `-WhatIf` reports `would install` / `would remove`. Proving checks: `installsteps` (orchestration with stubs), `installplan` (real `-WhatIf` processes, tasks and launcher folders identical afterwards), `installpreflight` (all missing prerequisites listed in one run), `installsandbox` (real install / reinstall / uninstall / uninstall twice under renamed `CamTestSandbox-*` tasks), `autostart`. History: [13],[35] launcher, [36] restart policy, [53] `-Nvr` instead of `-Run`, [68] UI task, [75] one `-Install`.

Everything in this section is Windows/Task Scheduler/PowerShell-specific except the IDEMPOTENCE RULES and the PREFLIGHT idea (list all problems at once, change nothing, hard vs soft). The Go port needs an installer interface per OS (Scheduled Task / systemd / launchd), plan:231-233.

---

## 6. Dashboard and health strip: the information they carry

### 6.1 Health strip (`cam.health.ps1`; reads only status.json)

Refresh every `-RefreshSeconds` (2). Sections, in `HealthSections` order (default `status, cameras, events, archive`; a misspelt name throws): health:165-188; rows budget health:468-545.

| Section | Content | Source |
|---|---|---|
| status (1 row) | running: `RUNNING pid <SupervisorPid> up <age> \| <relay text> \| free <FreeGB> GB (reserve <ReserveGB>) \| <days>d left` (green). Relay text: `relay unknown` (no section), `relay off`, `relay UP pid N, M path(s), K restart(s)`, `relay DOWN, K restart(s)`. Not running: `STALE - NO SUPERVISOR (<age> ago): <reason> \| last known free ...` (red), reason gets the remaining width | health:191-260 |
| cameras (header + 1 row per camera) | columns `CAMERA(22) REC ALIVE MODE SEG AGE RST/h LAST ERROR`; REC `ON/off`; ALIVE `yes/no` only when Record, else `-`; MODE = `InputMode` of the RECORDER child (relay/direct, `-` empty); SEG AGE = `SecondsSinceLastClosedSegment` via the age formatter (`<120 s` -> `Ns`, `<7200 s` -> `Nm`, else `Nh`, null -> `-`); RST/h; error text = `config: <ConfigError>` else `LastError`. Colours: green alive, red not alive, dim grey when not recording or when the supervisor is not running. Too many cameras: last row `... N more camera(s) not shown` | health:263-336; common:75-84 |
| events (`HealthEventRows`, default 5, label row included) | newest first, `HH:mm:ss <Text>`; colour red for `fail|error|denied|refus|cannot|guard`, yellow for `kill|watchdog|restart|prune|stop|exit`; `nothing logged yet` | health:339-395 |
| archive (2 rows) | `ARCHIVE <oldest MM-dd HH:mm> -> <newest MM-dd HH:mm> (<days>d)  <ArchiveGB> GB in <N> file(s)`; `TREND <GB/day> GB/day  <days>d left  pruned N file(s), X.XXX GB` or red `PRUNE BLOCKED: <LastMessage>` | health:398-465 |
| no file | `NO STATUS FILE at <path>` + the hint that no supervisor has run | health:497-502 |

The strip's staleness is the same two-signal verdict (1.7). Cell/row sizing (`HealthStripHeightPx` -1/0/>0, `HealthStripMinPx`, `HealthCellWidthPx`...) is window-geometry only and not data.

### 6.2 `cam.recorder.ps1 -Status` (report)

Same data as the strip plus root, container, segment length, per-camera current segment and size (`<n> MB`), the `next start attempt` time while backing off, the `STALE` banner with age and the "history, not the current state" warning, `disk free/reserve/hysteresis`, archive size/files/oldest/newest, rate and days until the reserve, and prune last run / totals / BLOCKED message (rec:3398-3500).

### 6.3 mpv dashboard (`mpv.cam.autolayout.autorestart.ps1`)

Information shown per camera: the live picture (relay URL when the relay serves that path, else the direct URL), the mpv on-screen message `Traffic: ${video-bitrate}` (mpv script:359), and in the pane console one line per attempt `HH:mm:ss <camera> on the <relay|direct> input[ after waiting Ns]: <redacted URL>` and `Stream interrupted. Restarting in 3 seconds...` (script:476-480). Grid shape from the camera count alone, pane height from the 16:9 aspect, a reserved bottom band for the strip (`Get-CamDashboardLayout`, common:3256-3410, checks `layout`, `sections`). Behaviour the web app replaces: reconnect on exit, `--network-timeout=3` and `timeout=5000000` (5 s) to notice a frozen stream, relay-or-direct decision per attempt with a first-attempt wait of `RelayReadyWaitSeconds`, return from direct to relay every `ViewerPromoteCheckSeconds` (10) via one API call per direct pane (`Invoke-CamViewer`, common:1352-1458).

### 6.4 What camui carries today and what is missing

Already in the web UI (app.js): per-camera chip (Recording / Recovering / Camera offline / Not recording / Stopped), input chip (Relay/Direct), `DownSince` or `LastError` detail, last-closed-segment age, restarts per hour, record toggle, disk tiles (headroom days, free, archive, recording N/M), banners (stale, stopped, at reserve, cameras unreachable), persistent notices, events list, task states, supervisor verdict. NOT in the page (no read of these fields found in `app.js`): relay state and restart count, supervisor uptime/pid, prune totals and BLOCKED message, archive oldest/newest span, current segment name/size, `NextStartAt`, `ConfigError`. The strip shows several of those, so they are part of "what the web app must carry" if the strip is retired.

---

## 7. Tests (`tests/cam.tests.ps1`)

Harness facts (CODE): the whole harness is Windows PowerShell 5.1 (tests:3); `-Checks` takes a `ValidateSet` of 58 names (tests:30). The default run is the 48 camera-free names (tests:57); 10 are opt-in and only run when named: `relay`, `prunelive`, `soak`, `livewhep`, `liveondemand`, `livepage`, `liverobust`, `livegone`, `livelan`, `livetls`. `-Live` allows checks that open RTSP sessions to the real cameras (tests:33-34). Exit code 0 means every check passed (tests:6-7). Primitives live in `tests/cam.testlib.ps1` (not read in detail). No check was run in this session, so "passes today" is UNKNOWN for all.

Legend for "Needs": `none` = no camera, no browser; `synth` = a synthetic local camera (local ffmpeg/MediaMTX, no real camera); `REAL` = the real cameras; `BR` = Chrome and/or Edge driven in a real window; `W` = the behaviour under test is itself Windows-only (Task Scheduler, Windows Terminal, job objects, HWND, ACL, mpv, DPAPI). The harness being PowerShell is true of every row and is not repeated. "Go" = suggested counterpart: **unit** (pure function, fake clock/FS), **integ-fake** (integration with a fake camera: ffmpeg lavfi or a scripted RTSP publisher feeding a real or fake MediaMTX), **e2e-browser**, or **stays PS** (keep in PowerShell/Windows only, or drop with the component it tests).

| # | Check (line) | One-line purpose | Needs | Go counterpart |
|---|---|---|---|---|
| 1 | `parse` (68) | every product script parses under the running PowerShell | none | stays PS; Go: `go build` + `go vet` in CI |
| 2 | `cameralist` (78) | the real `cameras.json` is byte-unchanged (sha256, BOM): guards against harness damage | none | n/a; unit test of the atomic list writer keeping BOM and unknown fields |
| 3 | `inputs` (88) | relay-or-direct decision for a batch makes NO per-camera probe (API is a black hole; clock proves it) | none | unit with a fake API server |
| 4 | `selftest` (226) | `cam.recorder.ps1 -SelfTest -Offline` in a sandbox exits 0 | none | unit/integ of a Go preflight command, if one exists |
| 5 | `layout` (242) | dashboard grid/strip geometry; grid shape from camera count alone | none | unit if the layout is kept, else dropped with mpv |
| 6 | `strip` (628) | `cam.health` renders healthy / stale / no-file fixtures through the real two-signal verdict | none | unit: status fixtures through the verdict (partly exists: ui/supervisor_test.go) |
| 7 | `sections` (350) | every strip section can be switched off, including all | none | stays PS (strip only) |
| 8 | `autostart` (458) | task + launcher: install, reinstall, remove, remove again (skipped if the real task exists) | W | per-OS installer: unit with a fake backend + OS integration |
| 9 | `crashsafe` (1246) | killed supervisor is restarted by the host; `StopKind=unexpected` + `LastUnexpectedStop`; exactly one id-100 Error alert; orderly `-Stop` is not restarted and raises no alert | none (unreachable cameras); host is W | integ-fake: supervisor killed under a Go keeper/service loop, both OSes |
| 10 | `windowclose` (1372) | closing the watched window: id 103 alert worded as a close, `window-closed` + `LastWindowClose`, members gone; terminating the warden raises NOTHING | W | integ: attached mode, terminal close -> children stop, one alert, correct StopKind (pty/SIGHUP on Linux, job on Windows) |
| 11 | `windowid` (1508) | a window is identified by the warden's HWND, not by its current title (foreign tab adopted) | W | stays PS / drop (concept: identity record validated by pid + start time) |
| 12 | `nvrstart` (1716) | orderly stop then start always brings the NVR back, even with a stale window record and a shared window | W | integ: stop-then-start with a stale lock/pid file |
| 13 | `leak` (770) | no camera password / credential in any URL under the scanned roots (positive control; SKIP without `camera_creds.xml`) | none (needs the credential file) | integ-fake: leak scan over everything a fake run writes |
| 14 | `uileak` (788) | every camui endpoint (incl. settings, footage, clip) scanned for password, userinfo, any URL; `Restreamer.RelayBase` absent | none; needs `camui.exe` | unit: `httptest` over all routes with a positive control |
| 15 | `uiguard` (1107) | request guard on the real binary: cross-site POST, rebinding Host, foreign Origin, non-JSON POST refused; page's own and plain-script requests accepted | none; needs `camui.exe` | unit: ui/guard_test.go already exists |
| 16 | `procs` (1967) | no ffmpeg/MediaMTX left over from a test run, telling leftovers from the Operator's live recorder | none; role census is W | integ: process census (plan S5), both OSes |
| 17 | `relay` (2006) [opt-in] | MediaMTX as a supervisor child: killed, restarted on logged backoff, `-Stop` leaves no orphan | REAL (`-Live`) | integ-fake with a scripted publisher |
| 18 | `viewer` (2158) | pane returns to the relay; pane script parses (it travels as base64); mpv argument quoting | none; W (mpv panes) | stays PS; promotion logic is replaced by the web live path |
| 19 | `viewerpw` (2289) | camera password on no command line of any process (real mpv, hidden, vs a synthetic camera requiring fake credentials; control process proves the listing can see one) | synth; W | integ-fake: same assertion for ffmpeg and MediaMTX command lines on both OSes |
| 20 | `relayapi` (2579) | control API needs the product's login; a refused login is never a dead relay; upgrade creates the login | synth (real MediaMTX, ports 18554/18564/18567) | integ with the real MediaMTX binary; unit for the denied/down/ok classifier |
| 21 | `prunematrix` (2978) | age rule deletes exactly old segments among foreign names, hidden, reparse points, prefix-sibling dirs; manifest proves the rest untouched | none | unit with temp dirs (symlinks on Linux, junctions on Windows) |
| 22 | `pruneroots` (3096) | pruning refused (drive root, profile-like, no marker, foreign entry); alert once | none | unit |
| 23 | `pruneage` (3249) | `MaxRetentionDays`: oldest first across cameras, newest per folder never; `-DryRun` lists exactly what a real run deletes | none | unit |
| 24 | `prunewatermark` (3312) | free-space watermark: stops at `ReserveGB + PruneHysteresisGB`; blocked state loud, no throw | none | unit with a free-space seam |
| 25 | `pruneclock` (3387) | whole run refused on clock anomalies (future segments, clock far ahead) | none | unit with a clock seam |
| 26 | `prunebreaker` (3474) | `MaxAgePruneFraction` breaker: alert 122 once per episode; never holds back the watermark | none | unit |
| 27 | `pruneblocked` (3599) | blocked prune alerts once (id 121), again only after an unblocked run | none | unit |
| 28 | `pruneconfig` (3662) | `MaxAgePruneFraction` validated at load (0, -1, 1.5, text refused; default 0.5) | none | unit (config) |
| 29 | `pruneowner` (3711) | clean root claimed (marker), foreign entry not claimed: pruning disabled, alert 120 once, recording unaffected | none (real supervisor, unreachable camera) | integ-fake |
| 30 | `prunelive` (3795) [opt-in] | strictly read-only dry runs against the LIVE archive with tripwires and a metadata manifest | REAL archive (no camera session) | integ: dry run on a copy of an archive |
| 31 | `installsteps` (3922) | `-Install`/`-Uninstall` orchestration with every step stubbed | none | unit with fake components |
| 32 | `installplan` (4059) | `-Install -WhatIf` / `-Uninstall -WhatIf` as real processes change nothing | W | unit (dry-run flag) + OS integ |
| 33 | `installpreflight` (4143) | all missing prerequisites listed in one run with fixes, nothing changed; unloadable config reported beside the rest | none; W for the rows | unit |
| 34 | `installsandbox` (4205) | real install, reinstall, uninstall, uninstall in a sandbox under renamed tasks | W | OS integ in a VM/container |
| 35 | `docswitches` (6398) | every `-Switch` named in README/docs exists on the named script | none | lint over CLI flag definitions |
| 36 | `doclinks` (6498) | every relative link and `#anchor` in README/docs resolves (with positive controls) | none | CI link check |
| 37 | `logrotate` (4350) | `Write-CamLog` rolls at `MaxBytes` once; one previous generation; locked reader never throws, notice once | none | unit (lock semantics differ by OS) |
| 38 | `segments` (4552) | real supervisor + synthetic camera at `SegmentSeconds` 10: names pass the segment test, clock-aligned cuts, H.264+pcm_alaw, no zero-byte file, hard-killed child restarted | synth (ffmpeg) | integ-fake (core of the Stage 4 shadow acceptance) |
| 39 | `backoff` (4719) | waits 1,2,4,8,8 s then reset after `HealthyResetSeconds`; `RestartsLastHour` counts each | synth | unit with fake clock + integ |
| 40 | `logrotatelive` (4812) | rotation inside the real supervisor, free and held, no gap beyond `SegmentSeconds` | synth | integ-fake |
| 41 | `stallwatch` (5304) | suspended (alive, silent) child killed about `SegmentSeconds x StallFactor` after last progress; healthy never killed over 20+ rolls | synth | integ-fake (SIGSTOP on Linux; suspend on Windows) |
| 42 | `logheld` (5627) | `Write-CamLog` never throws when append fails; supervisor survives logs held with no sharing; one drop notice | synth | unit; Windows-specific lock semantics |
| 43 | `pruneloop` (5787) | retention and prune alerts through the real supervisor loop (age, breaker, blocked, refused) | synth | integ-fake |
| 44 | `dst` (5993) | every age/elapsed/"is it due" computation across both DST transitions | none | unit with fixed zones |
| 45 | `dstloop` (6316) | real supervisor through simulated fallback / spring-forward and clock steps: no restart, no alert, heartbeat never older than 3 x interval + 1.5 s | synth (clock seam in a sandbox copy) | integ with an injected clock |
| 46 | `clocklint` (6359) | no product script reads the local clock or a local file time for arithmetic (rule has positive and negative controls) | none | custom `go vet`/lint rule |
| 47 | `soak` (4948) [opt-in] | ~33 min at default thresholds with a noisy synthetic camera; rotation, restarts, gaps sampled | synth | scheduled soak job |
| 48 | `livecfg` (6547) | live view additive and off by default: relay config byte-identical to master with the flag on or off; `status.json` top-level key set equals master's with it off; live config properties; collisions refused; malformed/unknown `Live*` key refused | none | unit: golden files for both configs and the status key set |
| 49 | `livesecret` (6809) | secret created: 64 hex, ASCII, no newline; ACL names only the current user (positive control); reused; damaged file refused; config holds only the sha256 | none; ACL part W | unit + per-OS permission test (0600 / ACL) |
| 50 | `livewhep` (7052) [opt-in] | real supervisor + live instance + Chrome/Edge: WHEP 401 without/with wrong login, plays sub and main, audio after unmute | synth, BR | e2e-browser (headless Chrome via CDP) |
| 51 | `liveondemand` (7348) [opt-in] | camera's second session held only while someone watches (0 -> 1 -> 0 within `LiveCloseAfterSeconds`); main path adds none | synth, BR | e2e-browser against a fake camera counting sessions |
| 52 | `livepage` (7460) [opt-in] | the Live screen rendered: 6 tiles playing, click to main, audio, hidden closes connections, deep link, phone layout, off state, foreign-page refusal | synth, BR (needs camui build) | e2e-browser |
| 53 | `liverobust` (8349) [opt-in] | Live wall that stays on: quick hide/show, expiry, stall rebuild, recovery after server/camui/camera restart, cycles, off-screen | synth, BR (`CAMUI_TEST_EXE`) | e2e-browser |
| 54 | `livegone` (9016) [opt-in] | everything a viewer opened is gone when the viewer is (tab close, browser kill) | synth, BR | e2e-browser + process census |
| 55 | `livelan` (9224) [opt-in] | media works through a non-loopback address; a stuck tile explains itself | synth, BR (needs a private address) | e2e-browser |
| 56 | `livetls` (9424) [opt-in] | camui HTTPS on the LAN with token and local CA: route x token matrix, handshake, CA download holds no key, 429 limiter, browser trust per process (SKIP without an existing firewall rule; checks cert stores unchanged) | synth, BR, W (cert store, firewall) | unit (`httptest` TLS) + e2e-browser |
| 57 | `downalert` (5447) | alert 110 threshold = max(`CameraDownAlertMinutes`, segment + status interval + 30 s) on synthetic scans | none | unit (table) |
| 58 | `downalertrun` (5528) | alert 110 against the real supervisor: healthy rolls raise none; dark camera raises exactly one, not before the limit | synth | integ-fake |

Rows with the strongest tie to part B: 6, 9-12, 14-16, 20, 31-34, 48, 49, 57-58 (status, supervisor, window, control, relay, install). Rows 21-30, 37-47 belong to part A (pruner, logs, time) but are listed for completeness.

---

## 8. UNKNOWN and UNVERIFIED items

UNKNOWN (could not be determined from the sources read):

1. Exit code of `-Run` after an `interrupted` stop (Ctrl+C or tab close): `exit 0` at rec:4196 may never run while the pipeline is being stopped.
2. Exit codes of `-SelfTest` on its early-return paths (tooling failure, no camera reachable): the summary and `exit 1` at rec:1573 are skipped (rec:1507,1513-1516).
3. Whether a failed dashboard launch changes `cam.ps1 -Start`'s exit code (`& $DashboardScript` output is not inspected; cam:722).
4. Whether any check proves "one supervisor per archive" or "supervisor death takes its children" (none found; only TODO-MEASURED [54]); also none for the restart budget / alert 102, and none for the warden `Disarm`.
5. Whether `cam.ps1 -Stop` has a dedicated automated check (none found by name).
6. What the unread design documents add: `tasks/portable-nvr/live-1a/1b/2b/fit/robust-design.md`, `tasks/ipcctv-recording/relay-api-auth.md`, `docs/how-it-works.md` (long lines), `tests/cam.testlib.ps1`.
7. What a real `status.json` looks like on disk (size, the exact JSON text PowerShell 5.1 produced): none was read (it lives in the archive root, not in the checkout; data files were out of scope). The schema above is derived from the writer code only.
8. Whether any check currently passes: nothing was run.

UNVERIFIED (inferred from CODE or comments, not observed):

9. F1: `LastWindowClose` is dropped by the first full status write after a restart (CODE-evident; contradicts todo [78] intent).
10. The host's decision uses the PERSISTED `StopKind`; a stale `requested` plus a supervisor that dies before its first write stops the host without a restart (common:5001-5008).
11. A relay restart or a staggered multi-camera start can block the single-threaded loop longer than the 15 s heartbeat limit (rec:2192-2217,4041-4044).
12. Whole-JSON secret redaction can corrupt JSON or alter innocuous words that contain a registered secret (rec:3019).
13. `Get-CamSupervisorState` and Go accept a reused PID while the heartbeat is fresh (no start-time check).
14. Go's `os.ReadFile` on Windows might block the writer's rename (no `FILE_SHARE_DELETE`); the writer retries 10 x 100 ms.
15. MediaMTX `v3/paths/list` is paginated (default page size believed to be 100): `Get-CamRelayReadyPaths` reads one response only (common:1082-1094). documented-only knowledge, not checked.
16. `-Nvr` exits 0 when no window appeared (rec:535-539), and the launcher then reports success to the Task Scheduler.
17. `Cameras[].Pid`, `LastPid`, `Restreamer.Pid` are always integers or null (no string pid observed).
18. The `windowingBehavior` question in [84] (`useNew` on the machine vs `useExisting` in code comments).
19. Which Go tests exercise camui's 409 serialisation and read-back: none found.

---

## 9. Behaviours that look like Windows/PowerShell accidents (candidates NOT to reproduce)

Mechanism-level (reproduce the behaviour in G1-G15, not the mechanism):

- A1. Windows Terminal window as the owner token: `wt.exe -w ipcctv-nvr`, `--title`, `--suppressApplicationTitle`, `windowingBehavior`, title search, HWND window records `_state\window-<title>.json`, `Get-CamWindowPresence`, adopted-tab handling, `WM_CLOSE` close sequence (common:3528-3865).
- A2. The warden/job split: `cam.nvr` and `cam.dashboard.panes` jobs, `CreateExclusive`, `Disarm`, nested `cam.supervisor.<hash>` job, `Terminate` by name (common:3921-4255).
- A3. The host pane existing only to tell "window closed" from "crashed" (common:4924-4944); the Go supervisor can use a service manager or its own loop. The restart POLICY (5 in 30 min, 5 s delay, alerts 100/101/102, no restart on requested/interrupted/exit 20) is worth keeping.
- A4. `CTRL_CLOSE_EVENT` skipping `finally`, and the `interrupted` StopKind that exists because Ctrl+C and a tab close run it (rec:4134-4146).
- A5. Role discovery by Win32_Process, parent-of-ffmpeg inference and base64 `-EncodedCommand` decoding (common:4258-4426).
- A6. `Start-CamDetachedProcess` (`CREATE_NO_WINDOW`; `DETACHED_PROCESS` unusable) (common:3868-3920).
- A7. Launcher stubs that poll for a `\\wsl.localhost` path, exit codes 2/3/4, task restart 3 x 1 min (common:5539-5601,5943-6133).
- A8. Config only on a local NTFS path because of `ReadDirectoryChanges` on 9P; `Move-Item` rather than `File.Replace` (rec:408-410; common:2627-2661).
- A9. DPAPI `Export-Clixml` credential files and the ACL-at-creation secret file (common:199-242,1858-1918).
- A10. Toast under the PowerShell AUMID and Application-log sources with the `IPCCTV: ` prefix (common:2398-2519). The alert IDs 100-130 are portable and should be kept.
- A11. The NTFS "stale directory entry for an open file" workaround behind `CurrentSegmentBytes` (rec:2555-2573); on Linux a plain `stat` is accurate (part A owns the rest).
- A12. Named mutex per log file (`Local\cam.log.<hash>`, common:2153).
- A13. The single-threaded one-second loop: blocking relay start wait, sleeps for staggering, and the 15 s heartbeat limit it forces (rec:4041-4044,4123).
- A14. `schtasks.exe /Query ... /V` parsing in camui and its codepage handling (ui/api.go:217-288).
- A15. PID liveness without start time (`Get-Process -Id`); Go can add start-time identity.

Bugs or inconsistencies not to copy: F1 (`LastWindowClose` not carried), F2 ("clean shutdown" for every stop), `State`/`StopKind` writer `ValidateSet` omitting `window-closed`, `-Status` ignoring scope switches, `-Nvr` exit 0 without a window, `Restreamer.Paths` listing configured not ready paths.

---

## 10. The 10 behaviours most likely to be got wrong in a rewrite

1. **The liveness verdict**: BOTH a fresh heartbeat (`UpdatedAt`, limit `max(3 x StatusIntervalSeconds, 15)`) AND a live `SupervisorPid`, evaluated by every consumer identically (strip, `-Status`, camui, host). `State == running` alone is a lie after a kill or window close; patches also rewrite `UpdatedAt`.
2. **The stop taxonomy**: `requested` / `interrupted` / `unexpected` / `window-closed`, who writes each, and the alert rules (only `unexpected` and a window close alert; an orderly stop alerts nothing; a deliberate close is never restarted). The host infers expected-or-not from persisted state, not from exit codes.
3. **Persistence of the outage record**: `LastUnexpectedStop` survives restarts and camui dismisses it by comparing the record's `At` string byte-for-byte (`acknowledged[key] === record.At`), so the writer must re-emit stored timestamps verbatim; decide `LastWindowClose` deliberately (F1).
4. **Atomic multi-writer status file**: temp file in the same directory + rename, readers never blocking the rename, and three other writers (host patch, warden patch, final write) that read-modify-write the same document without a lock.
5. **`Cameras[]` null/zero/empty conventions** the UI keys on: one row per camera in file order whether recording or not, `Alive` false when not recording, `SecondsSinceLastClosedSegment` excluding the current (newest-created) segment, `CurrentSegmentBytes` the true open-file length, `InputMode` of the recorder (not the viewer), archive figures lagging up to 60 s.
6. **Orphan-free ordered stop with proof**: flag-file request, children stopped cameras then live then relay, final `stopped/requested` write, `-Stop` waits 30 s and never kills the supervisor, then verifies via `LastPid`; plus children dying with a dead supervisor (no check proves this today).
7. **One supervisor per archive as a real lock**, with refusal (exit 20) meaning "do not restart"; today it is a heartbeat check-then-act that is blind when the status file is missing.
8. **Relay `denied` is not `down`**: wrong-login delay (up to ~4 s), anonymous retry on timeout, `denied` keeps every consumer on the relay (never a credentialed direct URL, never the camera's second session), one list query per cycle, per-child hold of 15 s, direct-to-relay promotion without touching restart counts, path names strictly `[a-z0-9]` because of the env-var loader.
9. **Secrets never leave the process boundary**: camera URLs only in a child's environment block, hashes only in MediaMTX configs, whole-document redaction before `status.json` is written, and camui's second scrub (URLs and bare IPv4 become `[redacted]`, keys `relaybase/password/username` dropped); the live secret created with owner-only access at creation.
10. **Time and format contracts**: UTC for every interval and age; status timestamps are local-offset round-trip strings with 7 fractional digits (nullable); `Events[].At` is a different format (log stamp); DST and clock steps must not disturb the heartbeat (`dst`, `dstloop`, `clocklint`); the status key set with live view off must be exactly the master set (`livecfg`).

