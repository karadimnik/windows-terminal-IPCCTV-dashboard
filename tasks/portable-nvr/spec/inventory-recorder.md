# Inventory A - the recording core of the PowerShell NVR (behaviour specification for the Go rewrite)

Checkout read: `<repo>` (branch portable-nvr). Read-only research; nothing was run, started or edited. Part B (status.json schema, supervisor/host semantics, control CLI, relay/live config, full test list) is a different document.

## 0. Conventions

Citation shorthand (always `file:line`):

| Tag | File |
| --- | --- |
| `R:` | `cam.recorder.ps1` (4260 lines) |
| `C:` | `cam.common.ps1` (6152 lines) |
| `T:` | `tests/cam.tests.ps1` |
| `TL:` | `tests/cam.testlib.ps1` |
| `TODO:` | `tasks/ipcctv-recording/todo.md` |
| `D-hiw`, `D-cfg`, `D-alerts`, `D-lim`, `D-ref` | `docs/how-it-works.md`, `configuration.md`, `alerts.md`, `limitations.md`, `reference.md` |
| `UI:` | `ui/*.go` (the existing Go control UI; a second consumer of the same names) |

Evidence labels used on claims:

* **[SRC]** read directly in the code at the cited line.
* **[TEST name]** a named check in `T:` asserts it. I did NOT run any check (read-only brief), so this means "the check's assertions say so", not "it passed today". `TODO:1328` records a full harness run of 40 passed / 0 failed / 1 skipped at the time of item [86].
* **[DOC]** documented-only (a code comment, a doc or the todo says so; no check proves it).
* **[UNVERIFIED]** my inference or an unmeasured assumption.
* **[UNKNOWN]** the sources do not say.

Check names are the values of `-Checks` in `T:30`. The default (camera-free) set is `T:57`. `soak`, `prunelive`, `relay`, `livepage` and friends run only when named.

Windows-only vocabulary used below: "reparse point" = junction or symlink; "NTFS creation time" = file birth time.

---

## 1. The ffmpeg command line

### 1.1 The single builder

Every recording ffmpeg of the product (supervisor children AND the self-test crash-safety recording) is built by one function: `Get-CamFfmpegArgs` (`C:1984-2044`), called at `R:1686` (supervisor child) and `R:908` (self-test crash safety, with `SegmentSeconds` forced to 20, `R:897-902`). Returned as `[string[]]` (`C:2043`). The exact construction, with variable names:

```
$arguments = @()
$arguments += '-hide_banner', '-loglevel', 'warning', '-nostdin'              # C:2014
$arguments += '-rtsp_transport', [string]$Config.RtspTransport                # C:2015  default 'tcp'
$arguments += '-timeout', [string][int64]$Config.TimeoutMicroseconds          # C:2016  default 10000000
$arguments += '-fflags', '+genpts+discardcorrupt'                              # C:2017
$arguments += '-i', $Url                                                       # C:2018
$arguments += '-map', '0:v:0', '-map', '0:a:0?'                                # C:2019
$arguments += '-c', 'copy'                                                     # C:2020
$arguments += Get-CamProfileArgList -ContainerProfile $ContainerProfile -Name 'AudioArgs'   # C:2021
$arguments += Get-CamProfileArgList -ContainerProfile $ContainerProfile -Name 'ExtraArgs'   # C:2022
$arguments += '-f', 'segment'                                                  # C:2023
$arguments += '-segment_time', [string][int]$Config.SegmentSeconds             # C:2024
$arguments += '-segment_atclocktime', '1'                                      # C:2025
$arguments += '-reset_timestamps', '1'                                         # C:2026
$arguments += '-strftime', '1'                                                 # C:2027
$arguments += '-segment_format', [string]$ContainerProfile.SegmentFormat       # C:2028
# only when SegmentFormatOptions is non-blank (C:2030-2035):
$arguments += '-segment_format_options', $formatOptions
$arguments += $OutputPattern                                                   # C:2037
```

Argument order is exactly this; `-c copy` precedes the profile `AudioArgs` so an `-c:a` in `AudioArgs` overrides the audio copy.

Output pattern (`Get-CamSegmentPattern`, `C:1964-1981`): `Join-Path $CameraDir "${SafeName}_%Y%m%d_%H%M%S%z$($ContainerProfile.Extension)"` where `$CameraDir = Join-Path RootPath SafeName` (`R:3621`). On Windows that is `E:\cameras\<Safe>\<Safe>_%Y%m%d_%H%M%S%z.mkv`.

### 1.2 Variants (the only places the line differs)

| Variant | `$Url` (`-i`) | Profile-driven arguments | Proven by |
| --- | --- | --- | --- |
| **mkv, relay input** (production default) | `"{0}/{1}" -f RelayBase.TrimEnd('/'), <relayPath>` = `rtsp://127.0.0.1:8554/<relayPath>` (`C:878-886`, no credential) | `AudioArgs=@()`, `ExtraArgs=@()`, `-segment_format matroska`, no `-segment_format_options` (`C:398-408`) | `relayapi` (`T:2579`, in the default set): real sandbox supervisor + its own MediaMTX + synthetic camera, asserts `cam1 InputMode 'relay'` (`T:2718-2721`) and anonymous RTSP read through the relay; `relay` (`T:2006`, only with `-Live`, real cameras) |
| **mkv, direct input** | `rtsp://${user}:${pass}@$($Camera.IP)/$stream` (`C:766`); `$stream = Camera.Quality`, or `stream1` when blank (`C:760-761`). User and password are NOT URL-escaped; `IP` may carry a port (the synthetic sandbox uses `127.0.0.1:<port>`, `TL:1248`) | same as above | `segments`, `backoff`, `stallwatch`, `downalertrun` (all record a direct synthetic camera, relay off, `TL:1238`) |
| **fmp4, either input** | same two URLs | `AudioArgs = -c:a aac -b:a 64k`, `-segment_format mp4`, `-segment_format_options movflags=+frag_keyframe+empty_moov+default_base_moof`, extension `.mp4` (`C:409-415`) | NOTHING [UNVERIFIED]: no check in `T:` selects `fmp4` (grep of `tests/` for `fmp4` finds none) |

Container selection is the single key `Container` (`C:396`), resolved by `Get-CamContainerProfile` (`C:598-631`); a profile must have non-blank `Extension` and `SegmentFormat` (`C:624-628`), `AudioArgs`/`ExtraArgs`/`SegmentFormatOptions` are optional (`C:634-645`, `C:2031-2032`). Unknown profile name throws `Container profile '<n>' is not defined in ContainerProfiles.` (`C:622`).

The relay/direct decision is made per start attempt (section 3.3). Relay URL and direct URL are the only difference between those two variants; every other flag (including `-rtsp_transport tcp` and `-timeout`) is sent for the relay input as well [SRC `C:2015-2016` takes no input-mode argument].

Fully expanded default line, mkv + relay (placeholders in angle brackets):

```
ffmpeg -hide_banner -loglevel warning -nostdin -rtsp_transport tcp -timeout 10000000 -fflags +genpts+discardcorrupt -i rtsp://127.0.0.1:8554/<relayPath> -map 0:v:0 -map 0:a:0? -c copy -f segment -segment_time 900 -segment_atclocktime 1 -reset_timestamps 1 -strftime 1 -segment_format matroska <Root>/<Safe>/<Safe>_%Y%m%d_%H%M%S%z.mkv
```

fmp4 adds `-c:a aac -b:a 64k` after `-c copy`, and `-segment_format mp4 -segment_format_options movflags=+frag_keyframe+empty_moov+default_base_moof` plus `.mp4`.

### 1.3 Process launch (what the Go runner must reproduce)

`Start-CamChild` (`R:1658-1710`): `FileName = Config.FfmpegPath`, arguments joined by `Get-CamCommandLine` (`C:2089-2096`, CommandLineToArgvW quoting - a Windows artefact, a Go arg vector replaces it), `UseShellExecute=false`, `CreateNoWindow=true`, ONLY stderr redirected (`R:1688-1693`); stdout is not read (ffmpeg writes nothing to stdout in segment mode [UNVERIFIED]); stdin is protected by `-nostdin`. stderr is read asynchronously in 8192-byte chunks (`R:1705-1707`) so ffmpeg never blocks on a full pipe. Start is logged: `start {Name} pid {pid} on the {relay|direct} input: ffmpeg {arguments}` (`R:1709`) through the redactor, so a direct input's URL shows as `rtsp://***:***@host/stream`. The credential is a single account shared by all cameras (`R:3711`, `C:750-767`).

### 1.4 What each flag guarantees

| Flag | Guarantee / reason | Label |
| --- | --- | --- |
| `-hide_banner -loglevel warning` | quiet stderr but keep packet loss / buffer overrun warnings; lower levels "would also lose the warnings that matter" (`R:1724-1726`) | [SRC] |
| `-nostdin` | "ffmpeg otherwise steals stdin from a shared console and can take down sibling processes" (`C:1990-1992`). Also: ffmpeg offers no graceful stop once set, hence the hard kill (`R:1971-1973`) | [DOC] |
| `-rtsp_transport tcp` | interleaved RTSP over TCP (config `RtspTransport`, `C:421`, `D-cfg:18`) | [DOC] |
| `-timeout 10000000` | RTSP socket I/O timeout in microseconds = 10 s. "The correct RTSP socket timeout since ffmpeg 5.0; `-stimeout` is rejected by this build" (`C:1993-1994`). Proven on the Operator's build by self-test check (a): `-timeout` accepted, `-stimeout` rejected (`R:729-751`); `TODO:121` and `TODO:159` record the camera-facing self-test run (33 passed, 0 failed). Presumably a stalled network ends ffmpeg with an error exit, which the supervisor then restarts [UNVERIFIED inference] | [SRC]/[DOC] |
| `-fflags +genpts+discardcorrupt` | generate missing PTS, drop corrupt packets. NO rationale is recorded anywhere in the repo (grep of all `*.md` finds no mention) | flag meaning documented-only (ffmpeg manual, not re-read here); rationale [UNKNOWN] |
| `-i <url>` | one input, relay or direct | [SRC] |
| `-map 0:v:0 -map 0:a:0?` | first video stream, first audio stream if present; "the trailing ? keeps video recording when the source has no audio track" (`C:1995-1996`) | [DOC] |
| `-c copy` | no re-encode; "recording costs almost no CPU and the footage is exactly what the camera sent" (`D-hiw:19`) | [DOC] |
| `-f segment -segment_time N` | rotate output every N seconds (`N = SegmentSeconds`, default 900) | [SRC] |
| `-segment_atclocktime 1` | cut on multiples of N of the wall clock rather than N after start. Proven: after the first (partial) segment each segment's NAME time is on a multiple of 10 s, name within 2 s of file creation time, closed segment duration within 1 s of 10 s [TEST segments `T:4608-4628`]. A copy cut can only happen on a keyframe, so each boundary overshoots the mark by up to one GOP (`D-hiw:19`, `D-lim:9`); real 900 s segments measured within 0.1 s of 900 (`D-lim:11`). Whether the multiples are taken in local or UTC time is not stated anywhere [UNKNOWN]; local and UTC coincide whenever the zone offset is a multiple of N | mixed |
| `-reset_timestamps 1` | each segment's timestamps start at 0: proven by "start not reset" assertion `probe.Start > 0.5` [TEST segments `T:4619`] | [TEST] |
| `-strftime 1` | the output name is expanded with strftime. "strftime here is local time: this build has no UTC option and ignores TZ" (`C:1968-1969`; `TODO:11`) | [DOC] (Windows build) |
| `%z` in the pattern | appends the UTC offset (`+0300`), "legal in a Windows filename" and "what stops the repeated hour at the DST fall-back from overwriting the earlier segments of the same wall-clock hour" (`C:1968-1972`; `D-hiw:40`) | [DOC] |
| `-segment_format matroska\|mp4` | inner muxer | [SRC] |
| `-segment_format_options` | inner-muxer options MUST go here: "a top-level -movflags is silently ignored under -f segment and produces an unfragmented, crash-fragile MP4" (`C:1997-2000`) | [DOC] |

Deliberately absent flags: no `-reconnect*` ("HTTP-only and do nothing for rtsp://. Reconnection is the supervisor's job", `C:2001-2002`; `TODO:14`); no `-use_wallclock_as_timestamps`; no `-strftime_mkdir` ("hls-only", `C:1926-1927`, `TODO:13`); explicit `-c:a copy` is deliberately NOT added to the mkv profile because ffmpeg "warns about duplicate codec options on every start" (`C:401-404`).

### 1.5 Edge cases and historical decisions

* **Directories must pre-exist.** The segment muxer cannot create them (`C:1926-1927`). Verified in `TODO:1247` ([83]): a missing date folder gives "Failed to open segment", exit -2, and `-strftime_mkdir` has no effect. Hence the flat per-camera layout. What happens if a camera folder is deleted while running: the supervisor does not re-create it (`New-CamChildFor` runs only on reconcile, `R:3608-3628`); outcome [UNVERIFIED].
* **MKV, not MP4:** all six cameras emit `pcm_alaw` which MP4 refuses with stream copy (`TODO:9-10`, `D-hiw:39`); MKV also survives a crash (a segment being written when power went out still plays; proven by `selftest` check (d) camera-facing and by [TEST segments] kill-mid-segment readable). The `fmp4` profile re-encodes audio to AAC to get around it and is untested.
* **Non-monotonic DTS spam:** the cameras make ffmpeg print thousands of identical "Non-monotonic DTS" warnings per minute at `-loglevel warning` (2457 of 2459 lines in a 3.5 min recording, `R:1717-1727`). Handled in the log layer (section 8.4), not by changing ffmpeg flags.
* **Direct path puts the credentialed URL on ffmpeg's command line** (`TODO:1370-1372` [90], still open); plan OS4 (`tasks/portable-nvr/plan.md:411`) says the portable recorder should read only through the relay.
* **Other ffmpeg/ffprobe invocations of the product** (self-test only): tool check `-hide_banner -loglevel error -rtsp_transport tcp -timeout 1500000 -i rtsp://127.0.0.1:1/selftest -f null -` and the same with `-stimeout` expecting rejection (`R:729-751`); stream probe `ffprobe ... -print_format json -show_streams -i <url>` (`R:776`); concurrency probe `... -i <url> -t 6 -f null -` (`R:841`); ffprobe of recorded files `-v error -print_format json -show_streams -i <file>` (`R:953`). `ui/footage.go` has its own clip-remux ffmpeg calls (out of scope here).
* **No test asserts the literal argument array.** Only behaviour through `segments` is checked; a Go port should add a golden test of the array [UNVERIFIED gap].

---

## 2. Naming and layout

### 2.1 Layout

```
<RootPath>/
  <Safe>/<Safe>_YYYYMMDD_HHMMSS+HHMM.<ext>     one folder per camera, flat (no date subfolders), ext from the container profile
  _logs/recorder.log (+ .1)                      supervisor and NVR host
  _logs/<Safe>.log (+ .1)                        per-camera ffmpeg stderr
  _logs/restreamer.log, _logs/liveview.log       MediaMTX output (part B)
  _state/status.json                             part B
  _state/stop.request                            stop flag (part B)
  _state/archive-owner.json                      ownership marker (2.5)
  _selftest/                                     self-test scratch (R:1501), removed afterwards
```

`Get-CamPaths` (`C:1925-1961`) creates `Root`, `_logs`, `_state` and every camera directory with `New-Item -Force` before anything starts. It is called at supervisor start (`R:3713`). A camera enabled while running gets its directory in `New-CamChildFor` (`R:3621-3622`). The date-partitioned layout was proposed and DROPPED by the Operator on 2026-10-14 (`TODO:1247` [83]): "the archive stays one folder per camera".

Name of a segment (from `C:1979`): `${SafeName}_%Y%m%d_%H%M%S%z${Extension}`, e.g. `<Safe>_20261011_223649+0300.mkv`. Date, time and offset are LOCAL wall-clock plus the offset in force at that instant. The date/time parts come from ffmpeg's strftime at the moment the segment opens.

### 2.2 Name sanitising: `Get-CamSafeName` (`C:720-747`)

Applied in this order, per camera `Name`:

1. `Trim()` (Unicode whitespace).
2. `\s+` replaced by `_` (`C:730`).
3. Remove every char in `< > : " / \ | ? *` (`C:731`).
4. Remove control chars `\x00-\x1F` and `\x7F` (`C:732`).
5. `TrimEnd('.', ' ')` (`C:733`).
6. Empty result throws `Camera name '<n>' contains no characters that are legal in a Windows filename.`
7. The part before the first `.` is compared case-insensitively against the reserved device names `CON PRN AUX NUL COM1-9 LPT1-9` (`C:32-36`); a hit throws (`C:739-744`).

Not done: no lowercasing, no length limit in the recorder (the UI limits names to 48 chars, `UI: settings.go:313`), no check for a leading `_`, no check for `%`. A camera whose name cannot be sanitised is reported and skipped at reconcile: `skipping {Name}: {error}` (`R:3654-3657`; `Get-CamResolvedCameras` catches per camera, `R:170-176`). The UI mirrors the algorithm (`UI: settings.go:386-394`) and rejects names whose sanitised, upper-cased forms collide (`UI: settings.go:329-343`); the recorder itself does NOT detect collisions: children are keyed by SafeName (`R:3666`, `R:3690`) so the second camera of a colliding pair is silently ignored [SRC, collision outcome UNVERIFIED at runtime].

Latent defects found by reading (not recorded in the todo; all [UNVERIFIED] at runtime):

* A name starting with `_` produces a folder the pruner and archive scan treat as the recorder's own (`R:2664`, `C:2750`): its footage is never pruned and `Get-CamPruneRefusal` calls its files foreign.
* A `%` in a name is passed unescaped into the strftime pattern (nothing strips it), so ffmpeg may expand it.
* `.NET \s` is Unicode-aware; Go's RE2 `\s` is ASCII-only. A rewrite must reproduce the .NET class to keep existing folder names stable.

Relay path name (separate rule, used only to address MediaMTX): `lowercase(SafeName)` with every char outside `[a-z0-9]` removed; empty throws (`C:812-837`). Reason measured 2026-10-11 on MediaMTX v1.21.2: an underscore in a path name makes MediaMTX create a stray path and leave the real one offline with no error (`C:818-828`; `D-lim:7`). Two cameras mapping to one path name throw in `Get-CamRelayPathMap` (`C:840-867`).

### 2.3 The allowlist `Test-CamSegmentName` (`C:2689-2712`)

True only when `$Name` is a file this recorder writes for camera `$SafeName` under the CONFIGURED container. Construction: take the leaf of `Get-CamSegmentPattern`, `[regex]::Escape` it, then replace `%Y -> \d{4}`, `%m -> \d{2}`, `%d -> \d{2}`, `%H -> \d{2}`, `%M -> \d{2}`, `%S -> \d{2}`, `%z -> [+-]\d{4}`, anchor with `^...$`, match with PowerShell `-match` (CASE-INSENSITIVE, Unicode `\d`). The effective expression for the default container:

```
^<Escape(Safe)>_\d{4}\d{2}\d{2}_\d{2}\d{2}\d{2}[+-]\d{4}\.mkv$      (case-insensitive)
```

Not validated: that the digits form a real date/time or that the offset is plausible. Accepts exactly one extension - the configured container's - so segments left from another container, `.tmp`, a second extension, another camera's prefix, or malformed digits are all "foreign". Golden cases (`R:1234-1243`, run by self-test (i) and so by [TEST selftest `T:226`]): accepted `Front_Entrance_20261012_105730+0300.mkv`; refused `..._105730-0500.mp4` (other container), `....mkv.tmp`, `Workshop_...mkv` (other camera), `Front_Entrance_2026101_105730+0300.mkv`, `....mkv.txt`, `notes.txt`, `desktop.ini`. The test `prunematrix` adds an empty `.dat`, a segment-named file with another camera's prefix inside a folder, a `.mp4` and a `.mkv.tmp` with valid stamps, and a `Front_Entrance_export.mkv` [TEST prunematrix `T:3027-3036`].

The existing Go UI parses names with a looser, independent rule: after stripping the camera prefix, `^_(\d{8})_(\d{6})([+-]\d{4})\.(?:mkv|mp4)$` (`UI: footage.go:73`); it places each segment in absolute time by its own offset (`UI: footage.go:7-12`) and treats a segment's mtime as its end (`UI: footage.go:14-19`). Those are consumer-side dependencies of the naming and mtime behaviour.

### 2.4 `Get-CamPruneRefusal` - the one file predicate for ALL deletions (`C:2715-2753`)

Both deleters (pruner `R:3148`, empty-segment cleanup `R:1951`) and the root-claim test (`C:2978`) call it. A file may be deleted only if ALL hold, returning in this order:

1. file is not a reparse point -> else `'reparse point'`
2. `Test-CamPathInside(root, file)` (`C:2664-2686`: both `GetFullPath`d, root gets a trailing separator, `StartsWith` OrdinalIgnoreCase) -> else `'outside the archive root'`
3. its folder is not a reparse point -> else `'reparse point'`
4. its folder's parent equals the root (OrdinalIgnoreCase, trailing `\` trimmed) -> else `'foreign'`
5. folder name does not start with `_` -> else `'foreign'`
6. `Test-CamSegmentName(file name, folder name)` -> else `'foreign'`

Return `''` = may delete. Reasons are fixed strings so callers count foreign files instead of logging each one (`C:2728-2732`).

### 2.5 Archive root marker / owner file

* Path `<RootPath>/_state/archive-owner.json` (`C:2930-2934`).
* Content written by `Initialize-CamArchiveOwner` (`C:3009-3020`), ordered keys: `Owner = 'IPCCTV recorder'`, `Schema = 1`, `RootPath`, `CreatedAt` (local timestamp with offset, `Format-CamTimestamp`), `Note = 'The recorder prunes footage only under a root holding this file. It wrote it because the root held nothing but its own files. Do not copy it into another folder.'` Written UTF-8 no BOM + newline via temp file and atomic move.
* Valid iff the file exists as a leaf, parses as JSON, and `Owner -eq 'IPCCTV recorder'` (`C:2937-2946`). The `RootPath` field inside is NOT compared with the real root [SRC] - copying the file into another folder would satisfy the check, despite the Note.
* Claim happens once per supervisor start (`R:3790-3801`): no-op when a valid marker exists; else only when `Get-CamArchiveClaimProblem` is empty (`C:2949-2988`) = the root passes `Get-CamArchiveRootProblem` AND exists AND every top-level entry is either `_`-prefixed or a real (non-reparse) folder whose children are all files accepted by `Get-CamPruneRefusal` (a subfolder inside a camera folder, or any foreign file, blocks the claim). An empty root qualifies. Up to 5 blockers are named in the message. Never stops recording: a failed claim leaves pruning disabled (alert 120 on the first prune cycle).
* `Get-CamArchiveRootProblem` (`C:2877-2927`) refuses: empty path, unusable path, a volume/drive root (including `\\server\share` shares as roots), the Windows directory, `Program Files` (three variants), the user profile, the folder holding profiles, the recorder's own directory, and any ANCESTOR of any of these (`StartsWith(trimmed + '\')`). A folder inside them (e.g. `Videos\cameras`) is allowed; the marker decides. Tests: `pruneroots` (predicate against real folders `T:3121-3136`).
* `Get-CamPruneRootRefusal` (`C:3028-3054`) = root problem, else marker present -> `''`; else claim problem text, else (unless `-DryRun`) "there is no ownership marker at ... The root qualifies - restart the recorder and it claims it". `-DryRun` of an unmarked-but-claimable root is NOT refused (previews the next start, writes nothing).

---

## 3. Child lifecycle

### 3.1 Constants

| Constant | Default | Config key | Where used |
| --- | --- | --- | --- |
| Supervisor tick | 1 s fixed (`Start-Sleep -Seconds 1`) | - | `R:4123` |
| Reconcile check | 5 s | `ReconcileSeconds` | `R:3846` |
| Status tick (also drives per-camera dir scan, alert-110 check, relay-ready fetch) | 5 s | `StatusIntervalSeconds` | `R:3891` |
| Archive-wide scan (inside a status tick) | 60 s | `ArchiveScanSeconds` | `R:4101` |
| Prune cycle | 300 s | `PruneIntervalSeconds` | `R:4117` |
| Backoff start / cap | 3 s / 60 s | `BackoffStartSeconds`, `BackoffMaxSeconds` | `R:2014-2015` |
| Healthy reset | 300 s | `HealthyResetSeconds` | `R:2017` |
| Stall threshold | `SegmentSeconds x StallFactor` = 900 x 2.5 = 2250 s | `StallFactor` 2.5 | `R:1895` |
| Down-alert limit | `max(CameraDownAlertMinutes*60, SegmentSeconds+StatusIntervalSeconds+30)` = 935 s | `CameraDownAlertMinutes` 15 | `R:1836-1838` |
| Down-alert repeat | 12 h | `CameraDownRepeatHours` | `R:3743`, `R:4090` |
| Child kill wait | 10 s (`WaitForExit(10000)`) | fixed | `R:1988` |
| Final stderr drain wait | 2 s | fixed | `R:1789` |
| Start stagger | 300 ms, floor 0 | `StartStaggerMs` | `R:3732-3734` |
| Relay hold window | 15 s, floor 0 | `RelayReadyWaitSeconds` | `R:3729-3731` |
| Relay API startup wait | 20 s (<=0 disables) | `RelayStartupWaitSeconds` | `R:2192-2194` |

### 3.2 One supervisor iteration (`R:3835-4124`), in order

1. `now = Get-CamWallClock` (UTC). If `_state/stop.request` exists: log `stop requested through _state\stop.request`, break to teardown (`R:3838-3843`).
2. **Reconcile** if `ReconcileSeconds` elapsed since the last (first pass immediately, `lastReconcile = MinValue`): read `cameras.json` LastWriteTimeUtc; ONLY if it differs from the last seen stamp (`R:3851-3854`) run `Invoke-CamReconcile`; failures are caught and logged `reconcile failed, keeping the current camera set: ...` (`R:3871-3875`). Config (`recorder.config.json`) and the camera credential are read ONCE at start - never reloaded [SRC `R:3711`, `R:4204`].
3. Relay tick, live tick (part B). Relay start blocks up to `RelayStartupWaitSeconds` polling the control API every 500 ms (`R:2192-2217`); `now` is refreshed afterwards.
4. `statusDue`; `pendingStart` (any child with no process and `now >= NextStartAt`, `R:3900-3902`). If the relay is enabled and (`statusDue` or `pendingStart`): ONE `Get-CamRelayReadyPaths` call per cycle shared by all children (`R:3903-3907`).
5. Compute `relayWaitDeadline = relay.StartedAt + RelayReadyWaitSeconds` only while the relay process is alive (`R:3926-3929`).
6. For each child (see 3.4-3.7). Directory scan `Get-CamDirScan` is taken first when `statusDue` (`R:3936`) and kept on the entry until the next status tick.
7. If `statusDue`: down-alert evaluation (3.9), archive scan if due, `Write-CamStatusFile` (part B), `lastStatus = now`.
8. Prune if due (section 5); an exception is caught and logged `prune failed: ...` (`R:4119-4120`).
9. Sleep 1 s.

Exit of the whole loop: stop flag -> `requested`; exception -> `unexpected` (exit 21); otherwise `interrupted` (Ctrl+C) (`R:4139-4146`, `R:4195`). Refusal to start because another supervisor runs: exit 20 (`R:3752`). Teardown order: children (hard kill each), live, relay, final `status.json` State `stopped`, delete stop flag (`R:4159-4189`). Part B owns the host's restart policy.

### 3.3 Relay vs direct: `Get-CamInputUrl` (`C:1098-1180`) as the recorder uses it

* `RestreamerEnabled` false (or absent), or no relay path -> `direct` immediately (`C:1142-1143`; `D-cfg:55`). [TEST inputs `T:198-207`]
* Else a batch `ReadyPaths` was supplied (supervisor passes the set fetched in step 4 of 3.2, `R:4048-4049`; `Start-CamChild` binds it only when non-null, `R:1681-1683`): path in set -> `relay`; not in set -> `direct`, NO per-path probe and NO wait (the supervisor passes no `WaitSeconds`). [TEST inputs `T:173-192`: an unreachable API with a 400 ms timeout, batch decisions in <100 ms, empty set -> direct]
* No batch set (lone attempt): ask `v3/paths/get/<path>` once (`Test-CamRelayPathReady`, `C:1032-1061`); `ready == true` -> relay, `denied` (HTTP 401/403) counts as READY, else direct. [TEST inputs: "lone attempt ... still probes, falls to direct", >= 350 ms with a 400 ms timeout]
* The wait loop in `Get-CamInputUrl` (`WaitSeconds`, poll `RelayReadyPollMs` floor 50) is used by mpv panes only, never by the recorder (`C:1121-1128`, `C:1162-1179`).
* `Get-CamRelayReadyPaths` (`C:1064-1095`): `v3/paths/list`, paths with `ready == true`; relay down -> empty; **login refused (denied) -> ALL camera paths** (so the relay is kept even though readiness is invisible; alert 130 in section 6).
* API call (`C:930-1019`): GET with 1500 ms timeout (`RelayProbeTimeoutMs`, floor 100), no proxy, never throws; Status `ok`/`denied`/`down`; a timed-out credentialed call is retried once anonymously and an immediate 401 proves the relay alive. [TEST relayapi `T:2579`, part B]
* The decision is re-made at EVERY start of the child, so a dead relay is left on the next restart and a returning relay is picked up (`R:1678-1680`). The chosen mode is stored in `Entry.InputMode`.
* **Hold while the relay comes up** (`R:4022-4036`): for a child that is due to start and whose relay path is NOT in the ready set, while the relay process is alive and `now < relay.StartedAt + RelayReadyWaitSeconds`, the child is skipped this cycle (`continue`) and `RelayHeldSince` is set. After the deadline it starts on direct and logs once `{Name}: the restreamer did not serve '{path}' within {N}s, starting it on the direct input`. Applies to any (re)start while the relay is younger than 15 s, not only the first [SRC]; `D-cfg:62` describes it as first-attempt-only. Rationale: if all recorders take direct slots at a cold start the relay can never pull (cameras serve exactly 2 RTSP sessions, `D-lim:5-6`; measured "kill MediaMTX, let all six recorders reconnect directly, and five of six paths never serve again", `R:3918-3920`). No relay process -> no hold (`R:3927`).
* **Stagger** (`R:4041-4045`): before every start after the first within one cycle the supervisor `Start-Sleep`s `StartStaggerMs` (300 ms), blocking the whole loop. [TEST inputs `T:209-218` bounds the startup budget: 5 x 300 ms <= 3000 ms, `RelayReadyWaitSeconds <= 30`]
* **Move off direct** (`R:3970-3979`): a LIVE child with `InputMode == 'direct'` whose path appears in the ready set is hard-killed with reason `the restreamer is now serving this camera, moving it off the direct input`, `NextStartAt = now`, and `continue`; restart count and backoff are NOT touched (documented as "not a failure"). The reverse direction needs no code: relay death ends ffmpeg and the next start picks direct.

### 3.4 Start rules

* A child exists only for cameras with `Record` exactly true (`Get-CamRecordEnabled`, `C:770-781`: missing field = off; `[bool]$value -eq $true`), without a naming error. New entries are created with `NextStartAt = now` (no inherited backoff, `R:3693-3696`) and `RestartCount=0`, `Backoff=0` (`R:1592-1615`).
* A due child (`now >= NextStartAt`) is started inside try/catch (`R:4047-4057`). A thrown start (e.g. missing ffmpeg): `LastError = "failed to start: <msg>"`, `Set-CamChildBackoff -RanForSeconds 0`, log `failed to start {Name}: {msg}; retry in {n}s`. This does NOT increment `RestartCount` and does NOT add to `Restarts`.
* `Entry.StartedAt` is taken after the process starts (`R:1702`).

### 3.5 Exit handling and backoff (`R:3941-3961`, `R:2002-2029`)

* **Every exit counts as a failure, whatever the code.** No exit code is classified as expected or benign: grep of `R:` for `ExitCode` finds only the logging use (`R:3942`, `R:3954`, also `R:852` in the self-test). Exit 0 after 5 s is a failure too; there is no "clean exit" path because the segment muxer is not supposed to exit.
* On exit: `ranFor = now - StartedAt`; final stderr drain (2 s); dispose; `Remove-CamEmptySegment` (3.8); `RestartCount++`; `Restarts.Add(now)`; `LastError = "ffmpeg exited with code <n> after <int ranFor>s"`; backoff; log `exit {code} after {s}s for {Name}; restart #{n} in {wait}s` (`R:3958-3960`; the `backoff` check parses exactly `for <name>; restart #<k> in <N>s$`).
* **Backoff** (`Set-CamChildBackoff`): if `ranFor >= HealthyResetSeconds` -> `next = BackoffStartSeconds`; else `next = Backoff*2`, raised to at least `start`, capped at `cap`. Initial `Backoff = 0`. So after a quick failure series the waits are 3, 6, 12, 24, 48, 60, 60...; a run of >= 300 s before the exit resets the NEXT wait to 3 (the reset is evaluated at exit time, not during uptime). `NextStartAt = now + next`. The effective restart delay is next + up to 1 s tick. [TEST backoff `T:4719`: scaled 1/8/15, plan kills 5 quick -> waits 1,2,4,8,8; healthy 18 s run -> 1; quick -> 2; `RestartsLastHour` = 7; restart seen between expected-0.5 s and expected+4 s]
* A watchdog kill passes its own `ranFor` (usually > 300 s), so a stall restart waits only `BackoffStartSeconds` unless the child had run < 300 s [SRC `R:4000`, `R:4007`].
* Reconcile-driven stops (disable, IP/Quality change, move off direct) never touch backoff or restart count (`R:3637-3639`).
* A changed `IP` or `Quality` (string compare, `R:3680`) hard-kills and removes the entry; a new entry is created in the same pass -> `RestartCount`, `Restarts` and `LastError` are lost [SRC]. A camera `Name` change changes SafeName -> old child stopped as "no longer set to record" and a NEW folder is used; old footage stays in the old folder (still pruned, 5.3).

### 3.6 Stall detection (`Test-CamChildStalled`, `R:1871-1914`; caller `R:3981-4010`)

* `threshold = SegmentSeconds x StallFactor` (2250 s). `StallFactor` is validated `> 1` only.
* **Progress marker** = the latest of: child `StartedAt`; the newest closed segment's `LastWriteTimeUtc`; the open segment's `CreationTimeUtc` (`R:3982-3993`). "Closed" = every file in the directory except the one with the newest `CreationTimeUtc` (`Get-CamDirScan`, `R:2609-2634`) - ALL files, not only valid segment names. The creation time of the open file counts as progress because a creation time is stamped once and never goes stale, and so the watchdog does not kill a healthy child just after the pruner deleted its closed segments (`R:3984-3989`). Rationale for not using file length: NTFS does not refresh the directory entry of an open file (`R:1878-1879`).
* The scan comes from `Entry.Scan`, refreshed only on a status tick (<= 5 s stale), with the fallback of enumerating the directory when no scan exists (`R:1902-1906`).
* `age = now - marker`; stalled iff `age > threshold` (strictly). Action: log `watchdog {Name}: stalled, no segment write for {int age}s (limit {int thr}s) while the process was alive`, `Stop-CamChild`, `RestartCount++`, `Restarts.Add`, `LastError = reason`, backoff, log `watchdog restart #{n} for {Name} in {wait}s` (`R:3998-4009`).
* [TEST stallwatch `T:5304`]: SegmentSeconds 6 x 2.5 = 15 s; over 21 rolls the healthy child is never killed (largest age below the limit); a suspended child is killed `limit < t <= limit + 3.5 s` after its last progress, counted in `RestartsLastHour` and recording resumes. [TEST dstloop `T:6316`] repeats the kill under simulated DST changes. Documented limit: a `StallFactor` below about 1.01 would restart healthy children at boundaries (`TODO:1423-1424`).

### 3.7 Stop (`Stop-CamChild`, `R:1969-1999`)

Hard kill only: log `kill {Name} pid {pid}: {reason}`, `Kill()`, `WaitForExit(10000)`, final stderr drain, dispose, null the entry fields, then `Remove-CamEmptySegment`. "A hard kill is acceptable because the container is crash-tolerant... and ffmpeg offers no graceful stop once -nostdin is set" (`R:1971-1973`). Used on: disable/removal, address or stream change, watchdog, move-off-direct, supervisor shutdown. The recorded behaviour is therefore a kill without finalising the Matroska file; a graceful SIGINT/SIGTERM in Go would be a deliberate departure.

### 3.8 Empty-segment cleanup (`Remove-CamEmptySegment`, `R:1917-1966`)

* When: after EVERY child exit and EVERY `Stop-CamChild` (the exit branch gained it in [24] part A, `TODO:1292-1294`). NOT at supervisor start.
* What: files in the camera directory (`-File -Force`) whose `Length -eq 0`. Cause: a child stopped in the second after a roll leaves a 0-byte file because the Matroska header is still in ffmpeg's write buffer ("a ... new segment is empty for about a second and a half", `T:4648-4650`); anything with content starts with a full header and stays readable after a kill.
* Gates: root must pass `Get-CamPruneRootRefusal` (no marker or unsafe root -> log `left {n} empty file(s) in {dir} alone: pruning is disabled - {reason}` and delete nothing); each file must pass `Get-CamPruneRefusal` (a non-segment empty file is counted and left: `{n} empty foreign file(s) left alone in {dir}: not recorder segments`). Success log: `removed empty segment {name}: the child was stopped before its header reached disk`; failure: `could not remove empty segment {name}: {msg}`.
* [TEST segments `T:4630-4674`]: kill 2 before the first write -> the empty segment is removed, three zero-byte foreign files (`notes.txt`, a `.mp4` with a segment name, a `.mkv.tmp`) survive; [TEST prunematrix `T:3072-3087`]: an empty `.mp4`, an empty file in a prefix-sibling dir and in a junction dir survive, the empty `.mkv` segment goes.

### 3.9 Camera-down detection (alert 110; `R:1818-1868`, `R:4074-4099`)

Per child on each status tick: `reference = max(SupervisorStartedAt, NewestClosedWrite, CurrentCreated)`, `seconds = now - reference`. Process liveness is deliberately ignored ("a camera that cannot be reached has its child restarted every backoff period so its StartedAt is always recent", `R:4065-4072`).

* `seconds <= limit` (935 s): if `DownSince` was set log `{Name} is writing again after {Format-CamAgeText} down`; clear `DownSince` and `DownAlertedAt`.
* else: `DownSince = reference` (first time), alert when `DownAlertedAt` is null or >= `CameraDownRepeatHours*3600` s ago; set `DownAlertedAt = now`.
* The limit is floored (never lowered) at `SegmentSeconds + StatusIntervalSeconds + 30` because at every boundary a healthy camera is a full segment past its last marker until the next status tick's scan sees the new file (incident [92], `TODO:1404-1424`, 2026-10-17: false alert at 08:45:01 and 09:00:01 with the old 900 s limit). When the floor raised the configured value the supervisor logs once at start `camera-down alert 110: configured {x} min, effective {y} min ({s} s) - ...` (`R:3828-3832`). [TEST downalert `T:5447`: 10 rows + 12-row table, literal expectations 901/930/935 quiet, 936 alert, table values 935/935/3600/1835/1835/3600/300/900/3600/935/935/960; TEST downalertrun `T:5528`: real supervisor, healthy 3 rolls no alert, dark camera exactly one alert in `[limit, limit+8]` s after last progress]
* Latent behaviour [SRC, UNVERIFIED at runtime]: the floor is the SUPERVISOR's start, not the child's. A camera enabled when the supervisor is already older than 935 s, with no file yet at its first status tick, computes `seconds` from the supervisor start and can alert 110 immediately, followed by "is writing again".

### 3.10 `RestartsLastHour` and totals

`RestartsTotal = Entry.RestartCount`; `RestartsLastHour` = number of `Entry.Restarts` timestamps `> now - 1 h`, evaluated at each status write (`R:2825-2827`). Counted: ffmpeg exits and watchdog kills. Not counted: failed starts, move-off-direct, reconcile stops. `Restarts` is never pruned (grows for the life of the process) [SRC]. [TEST backoff/segments/stallwatch assert the counts: 7, 2, 1]

---

## 4. Configuration

### 4.1 Loader (`Get-CamConfig`, `C:448-493`)

* Path `<repo>/recorder.config.json` unless given. A missing file or one that is empty/whitespace is fine (defaults only). Text via `ReadAllText` (BOM-tolerant), parsed with `ConvertFrom-Json`; a parse error throws and ends the command.
* Overlay: every property in the file REPLACES the default of the same name (shallow; no deep merge: a `ContainerProfiles` in the file replaces both profiles). Unknown keys are kept (`C:451-452`) except keys starting with `Live` (case-insensitive prefix) that are not in the known list: those throw (`C:518-523`). JSON `null` replaces a default with null.
* Explicit validation (everything else is NOT validated at load):

| Key | Rule | Failure message (verbatim start) |
| --- | --- | --- |
| `SegmentSeconds` | cast `[int]` then `>= 1` (`C:471-474`) | `SegmentSeconds must be at least 1, got N.` (non-numeric: cast error) |
| `StallFactor` | cast `[double]` then `> 1` (`C:475-478`) | `StallFactor must be greater than 1, got N.` |
| `RootPath` | not blank (`C:479-481`) | `RootPath must not be empty.` |
| `MaxAgePruneFraction` | invariant-culture `double.TryParse` of the string form, in `[0.01, 1.0]`; stored as double (`C:482-488`) | `MaxAgePruneFraction must be a number from 0.01 to 1.0 - ...` [TEST pruneconfig `T:3662`: 0, -1, 1.5, "abc" refused with key + `0.01` + `1.0`; 1.0 and 0.5 load; absent -> 0.5] |
| `Live*` (9 keys) | see `Assert-CamLiveConfig` `C:508-595`; checked whether or not live view is on | many; part B |

* A thrown error aborts the whole script run (`$ErrorActionPreference='Stop'`, `R:102`), including `-Status`, `-List`, etc. (`R:4204`).
* A wrong type elsewhere (e.g. `"ReserveGB": "abc"`) surfaces at first use as an exception: in the supervisor loop that goes to the outer catch -> fault stop -> exit 21 (`R:4126-4132`); at start-up it ends the run [SRC reading, UNVERIFIED by test].
* Editable by the Go UI with narrower ranges (a second validation layer, `UI: settings.go:433-450`): `SegmentSeconds` 60-3600 integral; `ReserveGB` 1-100000; `PruneHysteresisGB` 0-10000; `MaxRetentionDays` 1-365 integral, nullable. Segment lengths 6, 10, 20, 60 are used by the checks and accepted by the loader.

### 4.2 Every key of `Get-CamConfigDefault` (`C:267-445`)

"Go" column: KEEP = recording core must honour it; KEEP-B = needed only if part B's feature is ported; DROP = not read by the recorder (verified by listing every `$Config.<key>` read in `R:` and the code paths above) or Windows-only. Types are those the code casts to.

**Paths and recording**

| Key | Default (line) | Type / floor | Bad value | Go |
| --- | --- | --- | --- | --- |
| `RootPath` | `E:\cameras` (273) | string, non-empty | load error | KEEP (default is a Windows accident) |
| `FfmpegPath` | `C:\ffmpeg\bin\ffmpeg.exe` (274) | path | start-failed + backoff | KEEP (Go can resolve from PATH) |
| `FfprobePath` | `C:\ffmpeg\bin\ffprobe.exe` (275) | path | only self-test and tests | KEEP only if self-test ported |
| `SegmentSeconds` | 900 (276) | int >= 1 | load error | KEEP |
| `Container` | `mkv` (396) | string | `Get-CamContainerProfile` throws at supervisor start | KEEP |
| `ContainerProfiles` | mkv + fmp4 (397-416) | map of {Extension, SegmentFormat, AudioArgs[], ExtraArgs[], SegmentFormatOptions} | missing Extension/SegmentFormat throws | KEEP (config-driven container) |
| `RtspTransport` | `tcp` (421) | string | passed to ffmpeg as is | KEEP |
| `TimeoutMicroseconds` | 10000000 (422) | int64 | cast error at start | KEEP |

**Retention**

| Key | Default | Type / use | Go |
| --- | --- | --- | --- |
| `ReserveGB` | 20 (277) | double; `free < ReserveGB` triggers the watermark (`R:3078`) | KEEP |
| `PruneHysteresisGB` | 5 (278) | double; target = reserve + hysteresis (`R:3079`) | KEEP |
| `PruneIntervalSeconds` | 300 (279) | double (`R:4117`) | KEEP |
| `MaxRetentionDays` | null (280) | null or `''` = off; else `[double]`, `> 0` turns the age rule on (`R:3191-3195`) | KEEP |
| `MaxAgePruneFraction` | 0.5 (285) | validated above | KEEP |

**Supervisor / timing**

| Key | Default | Use | Go |
| --- | --- | --- | --- |
| `ReconcileSeconds` | 5 (423) | `R:3846` | KEEP |
| `StatusIntervalSeconds` | 5 (424) | `R:3891`, down floor | KEEP |
| `ArchiveScanSeconds` | 60 (428) | `R:4101` | KEEP (feeds disk figures only, `R:1828-1832`) |
| `CameraDownAlertMinutes` | 15 (434) | `R:1836` | KEEP |
| `CameraDownRepeatHours` | 12 (435) | `R:3743` | KEEP |
| `StopTimeoutSeconds` | 30 (437) | `-Stop` wait (part B) | KEEP-B |
| `BackoffStartSeconds`, `BackoffMaxSeconds`, `HealthyResetSeconds` | 3, 60, 300 (438-440) | section 3.5 | KEEP |
| `StallFactor` | 2.5 (441) | section 3.6 | KEEP |
| `LogMaxBytes` | `2MB` = 2097152 (442) | rotation (section 8) | KEEP |
| `LogRepeatSuppressSeconds` | 60 (443) | repeat collapse (section 8.4) | KEEP |
| `StatusEventCount` | 20 (332) | lines of the supervisor log published in status.json | KEEP-B |

**Relay (the recorder reads these)**

| Key | Default | Use | Go |
| --- | --- | --- | --- |
| `RestreamerEnabled` | true (337) | `[bool]... -eq $true` | KEEP |
| `RestreamerExePath` / `RestreamerConfigPath` | `C:\mediamtx\...` (338-339) | start, config check; must be a local NTFS path (`D-cfg:57`) | KEEP-B |
| `RelayBase` | `rtsp://127.0.0.1:8554` (340) | consumer URL prefix (`C:885`) | KEEP |
| `RelayApiBase` | `http://127.0.0.1:9997` (341) | readiness probe | KEEP |
| `RelayProbeTimeoutMs` | 1500 (344), floor 100 | probe budget | KEEP |
| `RelayStartupWaitSeconds` | 20 (352) | supervisor waits for the API | KEEP |
| `RelayReadyWaitSeconds` | 15 (357), floor 0 | hold window | KEEP |
| `StartStaggerMs` | 300 (370), floor 0 | stagger | KEEP |
| `RelayReadyPollMs` | 500 (359), floor 50 | wait loop used by mpv panes, not the recorder | DROP for the recorder |
| `ViewerPromoteCheckSeconds` | 10 (366) | mpv panes only | DROP |

**Live view** (`C:378-395`, validated `C:508-595`; features in part B): `LiveViewEnabled` false (bool required), `LiveExePath` '', `LiveConfigPath` `C:\mediamtx\mediamtx-live.yml` (non-empty, not starting with `\\`), `LiveWhepAddress` `127.0.0.1:8889` (loopback + port 1-65535), `LiveUdpAddress` `127.0.0.1:8189` (127.0.0.1, 0.0.0.0 or one RFC 1918 IPv4; port 1-65535), `LiveAdvertiseHosts` `[]` (array, <= 4 unique private IPv4, consistency rules with the UDP bind), `LiveSubStream` `stream2` (`^[A-Za-z0-9_\-]{1,32}$`), `LiveCloseAfterSeconds` 10 (integer 1-3600), `LiveSecretPath` ''. Also refused: `LiveWhepAddress` equal to a relay address. Go: KEEP-B. [TEST livecfg `T:6547`]

**Dashboard layout / health strip** - DROP from the recorder, none is read by `cam.recorder.ps1`: `HealthStripHeightPx` -1 (296; present in the example config), `HealthStripMinPx` 120, `GridGutterPx` 8, `VideoAspectWidth` 16, `VideoAspectHeight` 9, `HealthCellWidthPx` 9, `HealthCellHeightPx` 20, `HealthSections` (status, cameras, events, archive), `HealthEventRows` 5. (The strip and camui still read them, so the file format keeps them.)

Camera list read by the recorder (`cameras.json`, real data not opened): `Name`, `IP`, `Quality` (stream path, blank = `stream1`), `Record` (bool; absent = off), other fields preserved untouched on write (`C:678-717`); a leading BOM is preserved when present. Read with a share-friendly reader with 5 retries of 80 ms (`C:2588-2624`).

---

## 5. The pruner (`Invoke-CamPrune`, `R:3030-3391`)

### 5.1 Constants

| Constant | Value | Where |
| --- | --- | --- |
| Free-space reserve | `ReserveGB` 20 | `R:3078` |
| Hysteresis / target | target = reserve + `PruneHysteresisGB` (5) = 25 GB | `R:3079` |
| Cycle | 300 s, first run on the first loop pass | `R:4117` |
| Age rule | `MaxRetentionDays` null (off) | `R:3191-3195` |
| `MaxAgePruneFraction` | 0.5 (0.01-1.0, 1.0 = brake off) | `C:482-488` |
| Clock-sanity future tolerance | 60 s | `C:2831` |
| Clock-sanity lag bound | `SegmentSeconds x StallFactor` = 2250 s | `C:2838-2840` |
| Free space unit | `[math]::Round(AvailableFreeSpace / 1GB, 2)` with 1GB = 2^30; `AvailableFreeSpace` is the per-user available figure of the volume holding the root | `C:2290-2298` |

### 5.2 Algorithm, in the order the code executes it

1. **Root gate** (`R:3105-3129`): `Get-CamPruneRootRefusal`. If refused: `Blocked=true`, message `PRUNING DISABLED: {refusal}. Nothing is deleted until this is fixed, so the disk will fill. Recording continues.`; when not a dry run and the message differs from `State.RefusalAlerted`: log it, console it (red), send alert 120 (Error, title `pruning is disabled - the disk will fill`, body = the refusal text) when a State exists, store `RefusalAlerted`; set `State.LastRunAt/Blocked/LastMessage`; return. Once per distinct reason per supervisor life; `RefusalAlerted` is cleared on the first passing run (`R:3129`). [TEST pruneroots `T:3096`: a real `subst` drive root even holding a marker, a profile-like folder, a root without a marker, a root with foreign top-level entries; two cycles, exactly 1 alert each, nothing changed; TEST pruneowner `T:3711`: real supervisor, clean root claimed on start with no alert, foreign root not claimed, exactly one 120 Error, bait files untouched, supervisor stays up and keeps starting the child]
2. **Directory enumeration** (`Get-CamSegmentDirectories`, `R:2645-2677`): immediate subdirectories of the root, hidden included; skipped: names starting with `_`, reparse points (listed in `Skipped` as `... (reparse point: its content is outside the archive)`), directories resolving outside the root. Each skip is logged every cycle: `prune guard: refusing to prune {x}`. A folder left by a camera removed from `cameras.json` still counts.
3. **File gate per directory** (`R:3141-3184`): `Get-ChildItem -File -Force` (subdirectories and their contents are never entered). Per file `Get-CamPruneRefusal`: `''` -> segment; `'foreign'` -> counted, logged once per folder per cycle as `prune: {n} foreign file(s) left alone in {dir}`; `'reparse point'` / `'outside the archive root'` -> `Rejected`, logged each cycle `prune guard: refusing to delete {path}: {reason}`. A directory with no segments is skipped.
4. **Protection**: within the directory's SEGMENTS, `current` = greatest `CreationTimeUtc` (strict `>`, so the first enumerated wins a tie) (`R:3164-3167`); it is added to `Protected` and `newest`. Foreign files never take protection ("a foreign file written later must not take the protection off the one ffmpeg holds open", `R:3162-3163`). Every other segment is `unprotected`; it becomes a **candidate** only if `LastWriteTimeUtc < current.LastWriteTimeUtc` (strictly older than its own folder's newest); otherwise `kept++` and the log `prune: {n} segment(s) in {dir} kept: not older than its newest segment {name}` (`R:3171-3183`). [TEST pruneage `T:3249` case `bHeld`: created earlier but written after the folder's newest -> kept]
5. **Global ordering**: all candidates from all cameras sorted ascending by `LastWriteTimeUtc` (`R:3188`) - uniform retention across cameras. Tie order depends on `Sort-Object` stability [UNVERIFIED].
6. **Rule inputs** (`R:3191-3197`): `ageRule = MaxRetentionDays > 0`; `free = Get-CamFreeSpaceGB(root)`; `watermark = free < reserve`.
7. **Clock sanity** (`R:3202-3228`, `Get-CamPruneClockProblem` `C:2785-2845`), evaluated only when there is at least one unprotected segment and (`ageRule` or `watermark`), with ONE `Get-CamWallClock` reading used for the rest of the run. The whole run is refused when: (1) the oldest unprotected segment (by `LastWriteTimeUtc`) is LATER than the newest of the per-folder protected segments (ordering disagrees with protection); (2) any segment is written more than 60 s in the future; (3) `now - newest protected LastWriteTimeUtc > SegmentSeconds x StallFactor` ("the clock jumped forward or nothing is recording"). Result: `Blocked=true`, message `PRUNE REFUSED: {why}. Nothing deleted this run; recording continues.`, StopLine `prune stop: refused before the first deletion - {why}`; logged and consoled only when the refusal STARTS (`State.ClockRefused` false->true); **no alert is sent** for this refusal [SRC `R:3210-3223`]; cleared with the log `prune: the clock and the archive agree again, pruning resumes`. [TEST pruneclock `T:3387`: clock 30 days ahead, a segment dated +2 days, an inverted pair; each refused once over two cycles, nothing deleted; control with the real clock deletes 2.] Consequence [SRC]: while nothing has been recorded for > 2250 s (all cameras disabled or dark), neither rule deletes anything.
8. **Age rule** (`R:3261-3303`): `cutoff = now - days`; `expired` = candidates with `LastWriteTimeUtc < cutoff`. Circuit breaker `Get-CamAgePruneBreach(expired, segmentsSeen, MaxAgePruneFraction)` (`C:2848-2874`): with `countShare = expired.Count / segments.Count` and `byteShare = expiredBytes / totalBytes` over ALL gate-accepted segments (including each folder's protected newest); breach when EITHER share is `> limit` (equal passes). On breach: nothing is deleted by age; the age stop text becomes `age rule HELD by the circuit breaker, nothing deleted by age: {message}`; when not a dry run and `State.AgeBreakerTripped` is false: log `PRUNE AGE RULE HELD by the circuit breaker: {message}. Nothing is deleted by age this run; the free-space watermark still runs and recording continues. If MaxRetentionDays is mistyped, correct it; if it is meant, lower it in steps or raise MaxAgePruneFraction (1.0 switches the brake off).`, console, alert 122 (Error, title `the age rule was held - MaxRetentionDays would delete most of the archive`), set `AgeBreakerTripped`. Reset (`State.AgeBreakerTripped=false` + log `prune: the age rule is within MaxAgePruneFraction again, the circuit breaker is reset`) on the first non-dry run with no breach (`R:3300-3303`). Breach message: `the age rule would delete {n} of {m} segments ({p}%) and {x} of {y} GB ({q}%) of the bytes in the archive, more than MaxAgePruneFraction {f} allows`. Without breach: walk candidates oldest first, stop at the first with `LastWriteTimeUtc >= cutoff`; each deleted file is marked `consumed`; reason `older than MaxRetentionDays={days}`. [TEST prunebreaker `T:3474`: 60% by count -> held, one log line over two runs, exactly one 122 Error; reset by a calm run then a new episode alerts again; 40% deletes exactly the expired set oldest first; breach by bytes alone (72.7%) and by count alone (60%) each trip it; the watermark still deletes 5 oldest to 15 GB with the breaker tripped. TEST pruneage: 4 expired deleted oldest-first across two cameras, 2 h segment kept, each folder's newest kept even when 3.5 days old, dry-run list equals real list and changes nothing, StopLine `prune stop: age rule reached its cutoff * after 4 deletion(s), stopping at <first kept>`]
9. **Watermark** (`R:3305-3346`): if the age rule deleted anything, free space is re-read (dry run: estimated by adding freed bytes) and `watermark` recomputed. When `free < reserve`: loop `while free < target`: next candidate (skip `consumed`); `deleteOne`; on success re-read free space (dry run: add the file's size); a failed delete is skipped (`continue`) and counted in `Failed`. If the candidate list is exhausted while `free < target`, `Blocked=true` and stop. StopLines: `watermark not needed: free {x} GB is at or above the {r} GB reserve`; `watermark reached its target {t} GB after {n} deletion(s), free {f} GB[, stopping at {next name}]`; `watermark: nothing deletable left after {n} deletion(s), free {f} GB is still below the {r} GB reserve`. Reason string `free {x} GB below reserve {r} GB, target {t} GB`. [TEST prunewatermark `T:3312`: above reserve nothing; reserve 13 hysteresis 2 on 10 GB deletes exactly the 5 globally oldest in order and stops at the target with the exact StopLine; reserve 1000 deletes the 4 remaining then BLOCKED with no throw and a second run BLOCKED with 0 deletes; no newest segment ever touched]
10. **Always log the stop line** (`R:3348-3349`): every run that gets past the gates writes `prune stop: {ageStop}; {waterStop}` to the supervisor log (so one line per 300 s); then `FreeGBAfter` re-read.
11. **Blocked** (`R:3354-3369`): not dry run: message `PRUNE BLOCKED: free {f} GB is below the {r} GB reserve and nothing deletable is left ({c} candidate(s), {failed} undeletable, {p} protected as in-progress). Recording continues. Lower ReserveGB, move RootPath to a larger volume, or free space by hand.` logged and consoled EVERY blocked run; alert 121 (Error, title `pruning is blocked - the disk is filling`) once per episode via `State.BlockedAlerted`, which clears on the first non-blocked run (`R:3386`). Dry run message: `would be BLOCKED: ...`, no alert. [TEST pruneblocked `T:3599`: blocked, blocked, clear, blocked -> alerts 1,1,1,2; dry run never alerts; the one deletable segment is deleted and the newest kept.]
   * Quirk [SRC `R:3316-3319`]: `Blocked` is set whenever candidates run out while `free < target`, i.e. also when free space ended between reserve and target; the message still says "below the reserve". No check covers that corner [UNVERIFIED].
12. **Deletion** (`deleteOne`, `R:3230-3256`): `Remove-Item -Force`; success log `prune: deleted {FullName} ({len} bytes, written {LastWriteTime local 'yyyy-MM-dd HH:mm:ss'}) - {reason}`; failure `prune: FAILED to delete {FullName}: {msg}`, `Failed++`, never throws. Dry run only records `would delete ...`.
13. **Summary** (`R:3370-3379`): if anything was deleted and not blocked: `prune: {n} file(s) deleted, {gb} GB freed, free {a} -> {b} GB (reserve {r}, target {t})` (3 decimals for freed GB) logged and consoled.
14. **State update** (`R:3381-3388`, non-dry-run): `LastRunAt`, `DeletedTotal`, `FreedBytes` accumulate, `Blocked`, `LastMessage` (only overwritten when the run produced a message, so a stale blocked text can outlive the block [SRC]).

Never raises: a throw is caught by the loop (`R:4119-4120`).

### 5.3 What is never deleted (complete list)

* the newest segment (by creation time) of each camera folder, and any segment of the folder not strictly older than it (by write time);
* anything not matching `Test-CamSegmentName` for the CONFIGURED container - including segments of another container left from a previous setting (a switch from mkv to mp4 strands the old `.mkv` files forever) [SRC];
* any file that is a reparse point, or lives in a reparse-point folder, or resolves outside the root; reparse-point top-level folders are not entered at all;
* files directly in the root; anything in `_`-prefixed folders; subfolders inside camera folders and their contents; the folders themselves (never removed, even when empty);
* anything at all when: the root fails `Get-CamArchiveRootProblem`, the marker is missing/invalid, the clock check refuses, or (age rule only) the breaker holds the run;
* anything in a `-DryRun`.
Proven by [TEST prunematrix `T:2978`]: with foreign, hidden, system, container-mismatch and `.tmp` files, an empty `.dat`, underscore folders, a prefix-sibling `<root>_old`, two junctions and a file reparse point in place, exactly the four old segments disappear and a sha256 manifest of everything else is unchanged; `Foreign` counts are `7` in one folder and `1` in another, logged once each. [TEST prunelive `T:3795`, only when named: read-only dry runs on the live archive with tripwires; asserts that every would-delete matches an independent regex `^<root>\\(?<cam>[^\\_][^\\]*)\\\k<cam>_\d{8}_\d{6}[+-]\d{4}<ext>$`.]

Per-camera vs global: protection is per camera; ordering and the free-space target are global.

### 5.4 Loop integration

[TEST pruneloop `T:5787`]: the real supervisor at `PruneIntervalSeconds 2` - age rule deletes exactly the expired fixtures and leaves the young one, a foreign and a wrong-container file and everything the camera records; the breaker raises 122 once per episode and again for a second flood; blocked mode raises 121 once across every blocked run; no other alert, no gap in recording, status keeps updating.

---

## 6. Alerts

Event ids come from `Get-CamAlertEventIdList` (`C:2547`: 100, 101, 102, 103, 110, 120, 121, 122, 130). Channels per `Send-CamAlert` (`C:2443-2477`): (1) Windows toast, (2) Application event-log entry under source `IPCCTV` when registered else `Application`, text prefixed `IPCCTV: `, `{title} - {body}`, (3) the supervisor log line `ALERT {title} - {body} [toast shown|FAILED (...), event log written as source '<s>'|FAILED (...)]`; the fourth "channel" is a status.json field written by the caller (part B). Title and body are redacted. Nothing in this path throws (`C:2312-2315`). The plan's Stage 4 text says "alert ids 100-122" (`tasks/portable-nvr/plan.md:225`); 130 exists too.

| Id | Level | Trigger condition | Repeat / dedupe | Title and message pattern | Source | Proof |
| --- | --- | --- | --- | --- | --- | --- |
| 100 | Error | the host's supervisor child ended and it was NOT asked to (no stop flag, StopKind not `requested`/`interrupted`, exit code not 20; `C:5004-5013`); the restart budget (5 restarts in a 30 minute window) is not spent | one per unexpected stop | `the NVR stopped unexpectedly` - `{reason}. MediaMTX and every ffmpeg stopped with it. Restarting it in 5s.`; reason `the supervisor (pid {n}) stopped after {s}s without being asked to (exit {code})[: {LastUnexpectedStop.Reason}]` | `C:5016-5039` | crashsafe `T:1340` (exactly one `100 Error the NVR stopped unexpectedly*` via the stub); orderly `-Stop` raises none |
| 101 | Warning | after the 5 s delay the supervisor is restarted | one per restart | `the NVR is recording again` - `restarted the supervisor after an unexpected stop; this is restart {k} of 5 allowed in 30 minutes.` | `C:5047-5052` | none found (grep of `T:` for ids 100-103 finds assertions only for 100 and 103) [UNVERIFIED] |
| 102 | Error | `restarts.Count >= MaxRestarts (5)` inside `RestartWindowMinutes (30)` | then the host exits (alerts once) | `the NVR has stopped and is NOT coming back` - `{reason}. It has already been restarted {n} times in 30 minutes, so it is not being restarted again. Nothing is recording.` | `C:5029-5035` | none found [UNVERIFIED] |
| 103 | Warning | the NVR window is closed (warden) | once | `the NVR window was closed - nothing is recording` - `the NVR window '{title}' was closed at {HH:mm:ss}, so the supervisor, MediaMTX, every ffmpeg and the viewers were stopped. Nothing will restart it: run cam.ps1 -Start to bring it back.` | `C:4782-4796` | windowclose `T:1372` (reads the real Application log for id 103 within 60 s, and that it is not id 100, `T:1411-1480`) |
| 110 | Warning | camera with no file progress > effective limit (935 s default), evaluated every status tick | first alert, then every `CameraDownRepeatHours` (12 h) while still down; cleared (with an info log, no alert) when progress resumes within the limit | `a camera has stopped recording` - `{Name} has written nothing for {Format-CamAgeText}. Last error: {LastError or 'none recorded'}` | `R:4094-4098` | downalert, downalertrun |
| 120 | Error | pruning disabled: unsafe root, no marker and cannot claim | once per distinct refusal text per supervisor life; re-arms after a passing run | `pruning is disabled - the disk will fill` - body = refusal text | `R:3116-3118` | pruneroots, pruneowner |
| 121 | Error | PRUNE BLOCKED (see 5.2 item 11) | once per episode; re-arms after a non-blocked run; never from a dry run | `pruning is blocked - the disk is filling` - body = the BLOCKED message | `R:3361-3363` | pruneblocked, pruneloop |
| 122 | Error | age-rule circuit breaker holds a run | once per episode; re-arms after a run with no breach | `the age rule was held - MaxRetentionDays would delete most of the archive` - body = the HELD line | `R:3276-3278` | prunebreaker, pruneloop |
| 130 | Error | relay control API refused the product's login (HTTP 401/403) | once per episode; the episode ends on the first `ok` answer (logged `the restreamer's control API accepts the product's login again`); a relay going down does not end it | `the restreamer refused the recorder's login` - `The MediaMTX control API at {RelayApiBase} refused the product's login (HTTP 401). Viewers and recorders are kept on the relay, but whether each camera is being served cannot be seen. Restart with cam.ps1 -Stop and -Start: ...` | `R:2075-2084` | relayapi `T:2579` |

Levels also in `D-alerts:7-15`. Not alerted: clock-sanity refusal (log + status only), a failed ffmpeg start, a watchdog kill, pruning that succeeds, an orderly `-Stop`. In tests alerts are replaced by a stub that records `EventId EntryType Title` (`TL` `Set-CamTestAlertStub`; `T:3109-3113`, `T:3491`), so the channel implementations (toast, event log) are proved only by `windowclose` (`TODO:1302`).

---

## 7. Time

### 7.1 The seam

`Get-CamWallClock` (`C:2756-2770`) returns `[DateTime]::UtcNow` and is "its own function so a test can move the clock". Every interval, age, deadline and "is it due" reads it; `Test-CamIntervalDue` (`C:2773-2782`) is `(Now - Last).TotalSeconds -ge Seconds`. File times are compared only through `LastWriteTimeUtc` / `CreationTimeUtc`; stored timestamps only through `Get-CamDateTimeOrNull` (`C:104-118`: `DateTimeOffset.Parse(..., Invariant).UtcDateTime`, null on failure). The `clocklint` check [TEST `T:6359`] scans every product `.ps1` and fails on `[DateTime]::Now`, `Get-Date` arithmetic or non-UTC file times used for arithmetic (display forms such as `.ToString('yyyy-MM-dd')` are allowed).

### 7.2 Where LOCAL time is used (display only)

* Log stamps `[DateTime]::Now.ToString('yyyy-MM-dd HH:mm:ss zzz')` (`C:2136`), pruner log lines (`LastWriteTime` local `yyyy-MM-dd HH:mm:ss`, `R:3234`, `R:3246`).
* `Format-CamTimestamp` (`C:87-101`): `([DateTimeOffset]$when.ToUniversalTime()).ToLocalTime().ToString('o')` - local time with the offset in force at that instant (round-trip 'o' format: seven fractional digits; the docstring example omits them). Built from the UTC instant because a local DateTime inside the repeated fall-back hour cannot say which offset it had. `MinValue` and `$null` -> `$null`.
* Segment names (ffmpeg `strftime` + `%z`).

### 7.3 DST pitfalls recorded in the todo and how they were found

* **[86]** (`TODO:1307-1333`) deadline 2026-10-25 04:00 -> 03:00 (zone GTB; next 2027-03-28 03:00 -> 04:00). DST breaks a supervisor on local `[DateTime]::Now` because `DateTime` arithmetic ignores the offset. Measured 2026-10-15: a real 600 s gap reads +4200 s at spring-forward and -3000 s at fall-back; through UTC it is 600 s in both.
  * Fall-back: every interval timer (status, reconcile, archive scan, prune) stalls for an hour, `status.json` goes stale, the watchdog is blind, and a restart scheduled in the last 60 s before the change is blocked for an hour.
  * Spring-forward: the watchdog kills every healthy child (3600 > 2250) and alert 110 fires for every camera.
  * "A watchdog-only fix is WRONG": at fall-back its input scan is frozen by the local status timer and it would false-kill healthy children after about 37.5 minutes.
  * Decision (2026-10-15): UTC for all interval and elapsed arithmetic through one seam; `*Utc` file times; local time only for display, names and `%z`.
* **[24] part B** (`TODO:1303-1305`) first found it; harness widened status-gap limits to `3 x interval + 1.5 s` (`TODO:1319`, `TL` `Get-CamTestCadenceLimit`).
* **[92]** (`TODO:1404`) not DST but the same family: the alert-110 limit had to be floored for segment-boundary timing.
* Known limits kept (`D-lim:18`, `TODO:1329-1333`): a step of the UTC clock itself (not DST) is NOT absorbed. Forward 1 h: one false watchdog kill and one alert 110 per camera, then normal. Backward 1 h: status, reconcile, archive-scan and prune timers freeze until the clock catches up and status.json goes stale while ffmpeg keeps recording. A warden created inside the repeated hour reads 1 h old (part B). Live behaviour at a real transition is "STILL INFERRED" (`TODO:1329`); the 2026-10-25 change was not yet observed.
* **Segment names across the transition**: `%z` keeps the repeated hour unique; UI places each segment in absolute time by its own offset (`UI: footage.go:7-12`).
* **Tests** (machine clock never touched): `dst` [TEST `T:5993`] feeds the product's own helpers instants 5 minutes either side of both transitions both ways (OLD local readings must be off by exactly the transition's hour, NEW must equal the true elapsed time): watchdog age, alert-110 down time, status/timer due, restart wait, heartbeat age, log repeat suppression, NVR restart window, retention cutoff and pruner clock check; `dstloop` [TEST `T:6316`] runs the real supervisor through simulated fall-back (-3600), spring-forward (+3600) and +/-3600 steps of the clock itself: for DST, zero child restarts, no alerts, no stale status (gap <= `3 x interval + 1.5 s`), a suspended child still killed within 25 s, exactly 2 watchdog lines.

### 7.4 What a Go version must do to be equivalent

(These are requirements derived from the above; they are not claims about Go internals.)

1. Intervals, backoff deadlines, ages and "due" checks must be independent of the zone and of DST. Using the monotonic clock for interval timers (Go's `time.Time` carries one) satisfies this AND absorbs the +/-3600 s step that the PowerShell version does not; that is a deliberate improvement over "today's behaviour" and should be a conscious decision.
2. Comparisons against file times must use wall-clock UTC instants (mtime/ctime as `time.Time` in UTC). A stall decision uses wall-clock vs mtime, so a wall-clock step still affects it - unavoidable unless the progress marker is tracked internally.
3. Persisted timestamps keep local time with numeric offset (RFC 3339 / `o`-style) and are parsed back as instants (UTC), never as local clock readings.
4. Segment naming: if ffmpeg still writes the files, keep `-strftime 1 ... %z` and verify on Linux that ffmpeg's strftime honours `TZ` (the todo's "ignores TZ" is a statement about the Windows build, `C:1968-1969`) [UNVERIFIED for Linux]. If Go ever names files itself: local time, `20060102_150405` plus numeric zone `-0700`-style.
5. Port the `dst` table as a unit test over the same helpers (5 minutes either side of a spring-forward and a fall-back in the configured zone) and a loop test with an injectable clock (`Get-CamWallClock` is the model).

---

## 8. Logging

### 8.1 Files

| File | Writer | Content |
| --- | --- | --- |
| `<RootPath>/_logs/recorder.log` | supervisor and the NVR host (two processes; `C:2115-2119`) | supervisor events, `ALERT` lines, prune lines |
| `<RootPath>/_logs/<Safe>.log` | supervisor, per camera | ffmpeg stderr, line by line, redacted, repeat-collapsed (`R:1758`) |
| `_logs/restreamer.log`, `_logs/liveview.log` | supervisor | MediaMTX output (part B) |
| each of the above `.1` | rotation | the previous generation (exactly one) |

Camera log path `Join-Path LogDir "$SafeName.log"` (`R:3625`). A SafeName equal to `recorder`/`restreamer`/`liveview` would collide with those logs [UNVERIFIED, not guarded].

### 8.2 `Write-CamLog` (`C:2103-2221`)

* Line: `"{yyyy-MM-dd HH:mm:ss zzz} "` + redacted message + `Environment.NewLine`, UTF-8 without BOM, appended with `AppendAllText`. (`-NoTimestamp` exists but no caller uses it.)
* Default max `$script:CamDefaultLogMaxBytes = 2MB` (`C:38`) when `MaxBytes <= 0`; callers pass `Config.LogMaxBytes` (default 2097152).
* Rotation is checked on every write BEFORE the append: if the file exists and `Length >= MaxBytes`: delete `<log>.1`, move `<log>` to `<log>.1`. So a file can exceed the limit by one line; exactly one previous generation exists; older ones are dropped by design. [TEST logrotate `T:4350`: rolls exactly on the write that finds the file at/over the limit, at most two files, contiguous numbering]
* Cross-process safety: named mutex `Local\cam.log.<first 8 bytes of SHA256(lowercase full path), hex>`, waited 5 s; a timeout or abandoned mutex writes anyway (`C:2153-2160`). Needed because the host and the supervisor both write `recorder.log`.
* The directory is created if missing (`C:2145-2147`).

### 8.3 "Never throws" guarantees and their mechanics

1. **Rotation never throws.** If the rename/delete fails (a reader without delete sharing, e.g. an editor or `Get-Content -Wait`), the line is written to the unrotated file and rotation is retried on EVERY write; the first failure of an episode prepends a notice `{stamp}log rotation blocked: {name} is {len} bytes, over its {max}-byte limit, and could not be renamed to {name}.1 - {reason} Writing continues in place and rotation is retried on every write.` Episode state is per full path in memory (`$script:CamLogRotationBlocked`), cleared on a successful roll. History: this bug ended the real supervisor (exit 1/21) and left status.json saying `running` (`TODO:1290-1292`). [TEST logrotate (locked and `Get-Content -Wait` and two writer processes), logrotatelive `T:4812` (real supervisor, `LogMaxBytes 8192`)]
2. **The append never throws.** A failing append (log held `FileShare.None`, full disk, vanished or unwritable folder) loses that line; per-path counters `{Lines, Since, Reason}` accumulate; the next write that succeeds is preceded by `{stamp}log write failed: {n} line(s) dropped from this log since {since} - {reason}` (`C:2184-2186`). The first failure had killed the supervisor within a second (`TODO:1300-1301`). [TEST logheld `T:5627`: five drops and a folder-is-a-file case do not throw; the file is `before1, before2, notice(5), after, after2`; real supervisor survives 20 s of both logs held, restarts its child, one notice per log afterwards]. Counters are in memory only.
3. **Echo to console**: only the one path registered by `Set-CamLogEcho` (the supervisor log) is also written to the host (`C:2216-2220`).
4. Other no-throw helpers: `Send-CamAlert` and both channels (`C:2312-2315`), `Get-CamFileTailLines` (returns `@()` on any failure, 64 KB tail window, `C:2236-2287`), `Get-CamStatusSnapshot` (`C:3064-3080`), `Invoke-CamRelayApi` (`C:930-1019`). The supervisor loop wraps reconcile, relay/live ticks' starts, archive scan, status write and prune in try/catch (`R:3856-3875`, `R:4103-4120`, `R:4107-4114`); an exception that escapes the loop is a fault stop (exit 21) which the host restarts and alerts (100).

### 8.4 Child output handling and repeat collapse

* `Read-CamChildOutput` (`R:1764-1815`): non-blocking drain each tick (`Final` waits up to 2 s for the last read), UTF-8 decoding with a persistent decoder so a multi-byte char split across reads survives, lines split on `\r` or `\n`, blank lines dropped.
* `Write-CamChildLine` (`R:1713-1761`): message "shape" = line with every run matching `0x?[0-9a-fA-F]{6,}|\d+` replaced by `#`. Per shape (table cleared when it exceeds 200 shapes): if less than `LogRepeatSuppressSeconds` (60) since the shape was last logged, increment a counter and drop the line; otherwise log it, appending ` [+{n} more of this message suppressed]` when the counter is > 0, reset. Suppressed counts for a shape that never recurs are never reported [SRC]. Elapsed time uses the UTC seam [TEST dst row "log repeat suppression"].
* Redaction: `Get-CamRedacted` (`C:145-166`) blanks any `scheme://userinfo@` to `scheme://***:***@` (regex `(?i)([a-z][a-z0-9+.\-]*://)[^/\s@]+@`), then replaces every registered secret (raw and URI-escaped form, skipping secrets shorter than 3 chars) with `***`. Applied to every log line, alert, console line and the whole serialised status.json. Registered at load: the camera user and password and the relay-API password (`C:192-193`, `C:240`). [TEST leak `T:770` needs a credentials file, skipped otherwise]

### 8.5 Other log-adjacent behaviour

Log-related config: `LogMaxBytes`, `LogRepeatSuppressSeconds` (`C:442-443`). The supervisor's start line: `supervisor starting: pid {pid}, container {c}, segments {n}s, root {path}, reconcile {n}s, reserve {x} GB` (`R:3824-3825`). Logs that appear every cycle by design (noise to budget for in Go): `prune stop: ...` every 300 s, per-folder `foreign file(s)` lines, per-reparse-point refusals.

---

## 9. UNKNOWN and UNVERIFIED items (count at the end)

UNKNOWN (the sources do not say):

1. U1. Rationale of `-fflags +genpts+discardcorrupt` (no mention in any `*.md`, none in a code comment).
2. U2. Whether `-segment_atclocktime` aligns to local or UTC multiples, and how it behaves for `SegmentSeconds` that does not divide the zone offset. Only 10 s and 900 s in a +03:00 zone were observed.
3. U3. Meaning of the test labels `T1..T5`, `R1..R4`, `H1..H5` in the prune-check headers (`T:2978`, `T:3096`, `T:3249`, `T:3312`, `T:3387`, `T:3711`, `T:3795`): they refer to a hardening plan that is not among the listed sources (`TODO:1247` names its parts only in prose: container-only allowlist, name pattern, root guard, ownership marker, oldest-versus-newest sanity check).
4. U4. Which check proves alerts 101 and 102 (none found: `T:` asserts ids 100 and 103 only, `T:1340`, `T:1411-1480`).
5. U5. Tie order for equal `LastWriteTimeUtc` (depends on `Sort-Object`).
6. U6. What the Operator's real `recorder.config.json` overrides (deliberately not opened).
7. U7. Whether ffmpeg writes anything to stdout in segment mode (the supervisor does not redirect it).

UNVERIFIED (inferences, or claimed but not exercised):

8. V1. The `fmp4` profile's exact line works (no check selects it; mkv is the only exercised container).
9. V2. Behaviour when a camera folder disappears while its child runs.
10. V3. A `%` in a camera name is expanded by strftime.
11. V4. A camera folder name starting with `_` is never pruned (derived from `R:2664`, `C:2750`).
12. V5. Two cameras with the same SafeName: the second is silently ignored.
13. V6. Immediate alert 110 for a camera enabled after the supervisor has run > 935 s with no file yet.
14. V7. `Blocked`/alert 121 when free space ends between reserve and target.
15. V8. Linux ffmpeg honours `TZ` for `-strftime` and `%z`; the todo's "ignores TZ" is the Windows build.
16. V9. Non-numeric values for non-validated config keys surface as a fault stop (exit 21) at first use.
17. V10. A literal-array golden test of the ffmpeg arguments does not exist.
18. V11. Real-transition (2026-10-25) behaviour is inferred, not observed (`TODO:1329`).
19. V12. `RestartCount`/`Restarts` are lost on an IP/Quality change (entry recreated).
20. V13. The `-timeout` value semantics on the Linux ffmpeg build (self-test proved it only on the Operator's Windows build).

**Count: 7 UNKNOWN, 13 UNVERIFIED = 20.**

---

## 10. Behaviours that look like Windows/PowerShell accidents (candidates NOT to reproduce)

1. `Get-CamQuotedArgument` / `Get-CamCommandLine` (CommandLineToArgvW quoting, `C:2047-2096`): a Go argument vector makes it moot.
2. NTFS `CreationTime` as the "which file is open" signal (`R:2609-2611`, `R:3164-3167`, `R:3981-3993`). Linux birth time is not guaranteed on every filesystem; choose another definition (e.g. the segment whose name sorts last, or the one with the newest mtime plus an open-file check) and test it. This is a spec point, not just an accident - see top-10 #2.
3. `Get-CamLiveFileLength` (opening the file to read its real length, `R:2555-2573`): NTFS directory-entry staleness; `os.Stat` is live on Linux.
4. FileShare / mutex / retry machinery: `Get-CamFileText` retries (`C:2588-2624`), `Move-CamFileAtomic` retries and the `Move-Item` choice for the `\\wsl.localhost` share (`C:2627-2661`), the per-file `Local\` mutex (`C:2153`). `rename(2)` and a single writer remove the need; keep atomic temp+rename for status/marker files.
5. Reserved device names list (`C:32-36`) and trimming trailing dot/space (`C:733`): Windows rules; keep only if archives must stay Windows-compatible (the UI mirrors them).
6. `Get-CamArchiveRootProblem` Windows folders (Windows dir, Program Files, user profile, `\\server\share`, drive root): needs a Linux/macOS equivalent set (`/`, `/usr`, `/etc`, `$HOME`, parent of `$HOME`, mount points, the program's own dir and ancestors).
7. Reparse-point attribute checks (`R:2665`, `C:2742-2746`): translate to symlink / mount-point checks (lstat); a hard-link or bind-mount hazard has no PowerShell counterpart [UNVERIFIED].
8. Case-insensitive comparisons (`OrdinalIgnoreCase` path tests `C:2685`, `C:2749`; `-match` for `Test-CamSegmentName`; `\d` matching Unicode digits): on a case-sensitive filesystem decide explicitly; ffmpeg writes ASCII digits.
9. `[bool]` and `-eq $true` casts (`C:780`, `C:803-808`): a JSON string `"false"` for `Record` would be truthy in PowerShell; a Go decoder should accept only JSON booleans.
10. `.NET \s` and `Trim()` Unicode semantics in `Get-CamSafeName` - reproduce only to keep existing folder names stable.
11. `Restarts` list growing without bound (`R:3953`) - use a ring or prune on write.
12. Toast (WinRT, borrowed PowerShell AUMID, `C:2433`) and event-log source registration (HKLM, elevation, `C:2480-2519`): channel implementations, replaced by an interface (plan OS5).
13. DPAPI credential files (`Import-Clixml`, `C:187`, `C:232`), job objects, hidden-window flags, `Start-Process`/`.Handle` touching (`R:200`), `ErrorActionPreference` handling of native stderr (`R:136-143`).
14. `2MB` literal (2097152) is PowerShell syntax; the value is what matters.
15. `[math]::Round(x, 2)` is banker's rounding (round-half-to-even); the 2-decimal GB figure that feeds the watermark inherits it. A Go port should round half-to-even to match, or accept a one-cent GB difference [UNVERIFIED impact].
16. CRLF line ends in logs (`Environment.NewLine`); UTF-8 BOM preservation on `cameras.json` (a documented compatibility with the PowerShell/dashboard readers, not a recorder need).
17. Stagger implemented as a blocking sleep inside the supervisor loop (`R:4042`): it delays the status tick; any non-blocking schedule giving the same spacing is equivalent.

---

## 11. The ten behaviours most likely to be got wrong in a rewrite

1. **The exact ffmpeg argument order and the `-segment_format_options` placement** (and `-segment_atclocktime 1`, `-reset_timestamps 1`, `-strftime 1`, `%z` in the pattern). No literal test exists; one mis-ordered or missing flag changes naming, alignment or crash-fragility silently. Add a golden test of section 1.1.
2. **Which file is "the one ffmpeg holds open".** The pruner, the stall watchdog's progress marker, the down-alert marker and the status "current segment" all use "newest by CreationTimeUtc" over ALL files (scan) or over SEGMENT-named files (pruner). Reproducing "newest by birth time" on Linux, or replacing it, must keep: never delete it, and its creation counts as progress.
3. **Pruner gate ordering and the "refuse everything" modes**: root problem -> marker -> per-file allowlist (configured container only, strict regex, case-insensitive, folder parent == root, no `_` folders, no symlinks) -> clock sanity (60 s future, 2250 s lag, inversion) -> breaker (age only, either share > 0.5) -> `strictly older than the folder's newest`. Missing any one turns a bug into data loss.
4. **Watermark semantics**: trigger `free < reserve`, stop at `free >= reserve + hysteresis`, global oldest-first by write time, re-read free space after each deletion, `Blocked` when candidates run out (including the reserve-to-target corner), alert 121 once per episode with re-arm only after a non-blocked run.
5. **Backoff**: starts at 0, 3/6/12/24/48/60/60, reset to 3 only when the run lasted >= 300 s and evaluated at exit time; EVERY exit (even code 0) counts; failed starts back off but are not counted; watchdog and reconcile paths differ in what they count.
6. **Stall vs down-alert thresholds are different on purpose**: stall 2250 s from `max(StartedAt, newest closed write, open file creation)`; alert 110 at `max(15 min, segment+status+30) = 935 s` from `max(supervisor start, same progress markers)`, repeating every 12 h; a stale per-status-tick scan is part of the design (incident [92]).
7. **Time**: all intervals on one UTC/monotonic seam; `%z` in names; offsets persisted with timestamps; the +/-3600 s step limitation; the `dst` table as a test.
8. **Relay-vs-direct per start with ONE shared ready-set per cycle**, hold while the relay is younger than 15 s, 300 ms stagger, move-off-direct without counting a restart, "denied counts as ready", and no per-camera probe in a batch (the past flake that put cameras on direct for a whole run).
9. **Never-throw logging**: rotation at `>= LogMaxBytes` before append with one `.1` generation, blocked-rotation and dropped-line notices (once per episode), redaction of EVERY line and of status.json, and the repeat collapse keyed on a digit-stripped shape with `[+N more ...]` suffix. A recorder that dies from a log error is a recorded production bug.
10. **Sanitised camera names and archive adoption**: `Get-CamSafeName` produces folder names that already exist on disk and are matched by the pruner's allowlist and the UI's parser; any difference (Unicode whitespace, trailing dot, `%`, leading `_`, collisions) orphans footage from pruning or breaks the marker's "root holds only our files" claim test. Also: the marker claim only on a root holding nothing but recorder files (including no subfolders in camera folders), done at every start and never blocking recording.
