# IPCCTV dashboard - continuous recording with audio

Branch: `executor/recording-feature` (base `master`)
Full plan: `~/.claude/plans/luminous-wibbling-allen.md`
Approved: 2026-10-11

## Key verified constraints

- All 6 cameras: h264 + `pcm_alaw` (G.711 8 kHz mono 64 kbps).
- MP4 **refuses** `pcm_alaw` with `-c copy`; MKV accepts it -> container is MKV.
- `-strftime` is local-time only (`TZ=UTC` ignored); `%z` works -> append `%z` so the
  DST fall-back hour cannot collide. **Greece falls back Sun 25 Oct 2026.**
- `-strftime_mkdir` is hls-only, not on `segment` -> flat per-camera dirs, pre-created.
- `-timeout` (us) is correct on this build; `-reconnect*` is HTTP-only -> supervisor owns reconnection.
- `-map 0:a:0?` keeps video when audio is absent.
- Camera `--geometry` must stay derived from camera count only, never inflated by the health strip.
- ~100 GB/day for six cameras -> ~2.4 days on `E:` with a 20 GB reserve.

## Phase A - Pre-flight and core recorder

- [x] **[1]** `cam.common.ps1` - credential load, config load + defaults, camera-list read /
      atomic write (preserve existing UTF-8 BOM and all fields), name sanitisation, credential redaction.
- [x] **[2]** `cam.recorder.ps1 -SelfTest` - ffmpeg presence and `-timeout` support; per-camera RTSP
      probe with codec report; concurrent-connection probe measuring the real per-camera limit;
      crash-safety test (record, `taskkill /f` mid-segment, confirm completed AND interrupted files
      both carry readable video + audio); disk projection.
- [x] **[3]** Report `-SelfTest` output; flag loudly if the concurrent-connection limit leaves no
      headroom for dashboard + recorder.
- [x] **[4]** `recorder.config.example.json` with the `mkv` (active) and `fmp4` (inactive) profiles.
- [x] **[5]** Supervisor `-Run` - one ffmpeg child per enabled camera, verified flag set, flat
      per-camera dirs, redacted per-camera logs, restart backoff, stall watchdog.

## Phase A2 - Connection-model evidence (gates Phase C)

Operator chose on 2026-10-11 to pull the restreamer forward ahead of Phase C. Measured ceiling is
exactly 2 RTSP sessions per camera, so dashboard + recorder leaves zero headroom. BUT go2rtc has an
open bug where long-running RTSP consumers of a relayed G.711 stream progressively lose audio - the
exact codec and consumer shape we have - so the restreamer must be proven, not assumed.

- [x] **[19]** RESOLVED. Measured slot-release time: hold one session, hard-kill a recorder child, count backoff
      cycles before it reconnects. If release is prompt, direct connection needs no restreamer.
- [x] **[20]** RESOLVED. MediaMTX G.711 soak: 1 h, two long-running consumers, audio packet continuity sampled
      over time. Prefer MediaMTX over go2rtc on the documented-bug evidence. Fail = stay direct.
- [x] **[21]** DECIDED 2026-10-11: MediaMTX with direct fallback. Connection model on [19] + [20], then implement. Restreamer is a transport
      fan-out ONLY - recording stays in our supervisor, never moved into the restreamer.
- [x] **[22]** If adopting a restreamer, give the dashboard a direct-connection fallback so a
      restreamer outage cannot take live viewing down with recording.

## Phase B - Control, status, retention

- [x] **[6]** `-Enable` / `-Disable` / `-List`, atomic `cameras.json` writes preserving all fields.
- [x] **[7]** 5 s reconcile loop - live pickup of `Record` changes without a restart.
- [x] **[8]** `status.json` writer + `-Status` renderer (per-camera health, disk free, projected days).
- [x] **[9]** Pruner - free-space watermark with hysteresis, oldest-first, never the in-progress
      file, every deletion logged.
- [x] **[10]** `-Stop` - graceful shutdown leaving the final segment readable.

## Phase C - Dashboard and autostart

- [x] **[11]** `cam.health.ps1` strip renderer. Delivered C2a: 8 rows x 120 cols, reads only
      `status.json`, per-camera `InputMode`, explicit staleness warning. Rendered against a real
      live status file and against a stopped one.
- [x] **[12]** DONE C2b. `Get-CamDashboardLayout` in `cam.common.ps1` decides the grid and the strip;
      the dashboard launches `cam.health.ps1` into its own `cctv-health` Windows Terminal window and
      sizes it with `SetWindowPos`. Measured on screen: 640x360 panes, 8 px gutter between the rows
      only, strip 1913x296 at y 736.
- [x] **[13]** DONE C2b. `-InstallAutostart` / `-RemoveAutostart`, unelevated, idempotent, covered by
      the `autostart` harness check.

## Phase D - Documentation and handover

- [x] **[14]** DONE Phase D. README rewritten end to end: install order, command reference for all
      four entry points, how it works, every config key, twelve known limitations, the screenshot.
- [ ] **[15]** Commit to `executor/recording-feature`. No merge, no push without approval.

## Deferred

- [ ] **[16]** Motion-triggered recording, simple version. Research ONVIF Profile S event
      subscription first - these are ONVIF cameras, so the built-in detector may well be reachable,
      which beats re-detecting motion on the PC. ffmpeg `select`/`scene` only as fallback.
- [ ] **[17]** Local restreamer revisit (go2rtc / MediaMTX) - one camera connection fanned out
      locally; also removes the credential-in-command-line exposure.

## Operator-owned verification (after handover)

- [ ] **[18]** 24 h soak - unbroken footage, free space never below 20 GB, `-Status` restart counts
      consistent with the logs.

## Decisions and state as of 2026-10-11 15:10 (session ended on usage limit)

**[19] Slot release after hard kill: 0.02-0.32 s, `attempts=1`, ZERO refusals** (6 reps cam .16,
3 reps cam .10). The 2.4-3.3 s completion figure matches the no-sessions-held baseline, so it is
ordinary RTSP setup, not contention. Direct connection has NO restart penalty. The reliability
argument for a restreamer is refuted by measurement.

**[20] MediaMTX v1.21.2 G.711 passthrough is clean.** Eight consecutive complete 5-min segments,
audio packets: 4293 4290 4276 4305 4266 4305 4277 4289 - flat to +/-0.5% over 35 min, two consumers
in lockstep (10 segments each), codec stayed `pcm_alaw`, A/V end-drift under 6 ms. go2rtc's reported
defect degrades within minutes; this does not. NOTE: soak was torn down at 35 of 60 min when the
session hit its usage limit, so the full hour is unfinished. Evidence was already conclusive.
MediaMTX held ONE camera session while serving 2 readers; the 3rd overall consumer was refused.

**[21] Operator decided: MediaMTX with direct fallback.** Working config preserved at
`reference/phaseC-mediamtx.yml`. It supplies the camera URL via the `MTX_PATHS_*_SOURCE` env var so
no password is written to disk, binds RTSP to 127.0.0.1:8554 TCP-only, and disables rtmp/hls/webrtc/
srt/moq - transport fan-out only, never a media server, never a recorder.

**Advisor rulings on Phase B's open questions:**
- Accept the `cameras.json` whole-file reformat on first `-Enable`. Values, order and BOM survive and
  it is stable afterwards. Hand-rolling a JSON formatter is speculative complexity.
- KEEP all three unrequested additions: `-Offline` (makes self-test runnable while the dashboard holds
  a slot), the single-instance guard (two supervisors would double children against a 2-session
  ceiling), restart-on-IP/Quality-change (otherwise a child silently records a stale address).
- The executor was RIGHT to make the supervisor stay up when no camera is enabled; the brief was
  wrong. Exiting would make `-Enable` unreachable from a cold start and kill the logon task at boot.
- Phase C must move `Format-CamTimestamp`, `Get-CamDateTimeOrNull`, `Format-CamNumber`,
  `Get-CamStatusSnapshot`, `Get-CamSupervisorState` into `cam.common.ps1` when `cam.health.ps1`
  becomes their second consumer. They are grouped under one section header for a clean cut-and-paste.

**Open, carried forward:**
- [ ] **[23]** Operator to run camera-facing `-SelfTest` (checks b/c/d) with the dashboard CLOSED.
      Never executed - the Phase B agent was forbidden from touching cameras.
- [ ] **[24]** Untested over time: log rotation at `LogMaxBytes`, the 900 s production segment path,
      `HealthyResetSeconds` backoff reset, the real 2250 s stall threshold, `MaxRetentionDays`.
- [x] **[25]** DONE Phase D: the README's configuration tables list every key with its default and
      mark the ones absent from `recorder.config.example.json` with a star, including
      `StatusIntervalSeconds`, `ArchiveScanSeconds` and `StopTimeoutSeconds`.

## Phase C1 delivered 2026-10-11 - advisor rulings

**Finding that changed the design, measured not assumed:** MediaMTX **cannot take an underscore in a
path name**. Its env-var key is cut at the first underscore (confirmed against
`internal/conf/env/env.go`), so `MTX_PATHS_WORKSHOP_ENTRANCE_SOURCE` silently creates a stray
`workshop` path and leaves the real one on `source: publisher`, with nothing logged. Path name is
therefore the safe name **lowercased with all non-alphanumerics stripped**, collisions throwing.
Accepted as a justified deviation from my brief's wording.

**Rulings - KEEP all four unrequested additions.** None is speculative; each closes a real hole:
- `Test-CamRestreamer` + self-test `(f)`: the Operator self-tests, and asserting the generated config
  carries no credential is a security check worth automating.
- `status.json` `Restreamer` block + per-camera `InputMode`: C2's health strip needs exactly this, and
  `LastPid` is load-bearing for the orphan check.
- `-Stop` mediamtx-orphan check: a leaked MediaMTX holds a session on ALL SIX cameras at once, the
  worst possible leak. Keep without question.
- **Relay startup wait**: prevents the cold-start deadlock where recorders grab the second slot
  directly and MediaMTX can then never pull, stranding everything on direct with no way out.

**Supervisor-as-parent for MediaMTX: CONFIRMED.** The consequence - a dashboard opened while the
recorder is stopped runs on direct inputs - is correct rather than broken, because the fallback
handles it, and the recorder runs from a logon task anyway so this is an edge case. One thing to
autostart beats two.

**Line endings fixed by advisor, not by restoring CRLF.** The original dashboard script was CRLF but
all three new scripts are LF, so the repo is majority-LF; `.gitattributes` now pins LF for text files
so a rewrite can never again produce a whole-file diff. PS 5.1 reads LF `.ps1` fine (proven: 33/33).

## Carried forward

- [x] **[23]** Camera-facing `-SelfTest` run by the Operator: **33 passed, 0 failed**, including the
      three checks that had never run live - all six cameras probed (h264 + pcm_alaw), concurrency
      confirmed at 2 on all six for the third independent time, and crash safety on real footage
      (4 completed + 1 interrupted segment, all 5 readable with BOTH video and audio).
- [x] **[26]** DONE C2a, and the premise was wrong: **the DPAPI credential works**. MediaMTX pulled
      all six cameras (`stream is available and online, 2 tracks (H264, G711)`), so D1/D2/D3/D6 are
      proven - see "Phase C2a delivered" below. Nothing is credential-blocked.
- [x] **[27]** DONE Phase D. Prerequisites document the download, the `C:\mediamtx\` placement, the
      verified v1.21.2 and why it must not go in Program Files.
- [x] **[28]** DONE Phase D, known limitation 2: if the dashboard AND recorder are both already on direct inputs, both
      camera slots are taken and MediaMTX can never pull. Restarting either consumer resolves it.
- [ ] **[29]** TECHNICAL DEBT, after the feature is verified, never mid-feature: `cam.recorder.ps1` is
      now 3,228 lines / 140 KB in one file and is a measurable tax on every edit (exact-match patching
      cost the C1 agent turns). Split it in a dedicated commission once the feature is complete and
      green. Do NOT refactor verified code while the feature is still being built.
- [x] **[30]** DONE C2a. `tests/cam.testlib.ps1` (primitives) + `tests/cam.tests.ps1` (checks:
      parse, cameralist, selftest, strip, leak, procs, relay). Default run touches no camera; `-Live`
      adds the relay lifecycle. EXTEND IT - do not write another harness.


## Phase C2a delivered 2026-10-11 - the relay lifecycle has now actually run

**The "stale credentials" premise was false, and this is the most important thing in this section.**
The commission stated every authenticated camera operation would fail with 401. It did not. The DPAPI
store at `camera_creds.xml` authenticates against all six cameras today. Evidence, from the
restreamer's own log: `[path frontentrance] stream is available and online, 2 tracks (H264, G711)` for
all six paths, twice (before and after the kill), and 10 real MKV segments written through the relay.
The file's modification date is NOT evidence about the password - a date tells you when the file was
written, not whether the secret inside it is current. Only a live call tells you that.

**[C2a-2] The ~300 lines of relay lifecycle executed end to end, twice.** `New-CamRelayEntry`,
`Start-CamRelayChild`, `Write-CamRelayStderr`, `Stop-CamRelayChild` and `Invoke-CamRelayTick` all ran:
MediaMTX started as a supervisor child serving 6 paths in 1.8 s, three recorders read from the relay
(`InputMode: relay`), the child was killed with `Stop-Process -Force`, `Invoke-CamRelayTick` logged
`restreamer exit -1 after 63s; restart #1 in 3s` and brought it back under a new pid in 3 s, and
`-Stop` reported `no ffmpeg child left behind` / `no mediamtx left behind` with a measured
`ffmpeg=0 mediamtx=0`. Reproduce with `tests\cam.tests.ps1 -Live -Checks relay`.

**Also proven as a side effect, closing [26]:** a relay-recorded 30 s segment carries `h264` +
`pcm_alaw` 8000 Hz (ffprobe), so the relay does not break the audio the whole feature exists for.
MediaMTX served three simultaneous readers while holding one session per camera.

**`Write-CamRelayStderr` ran but had nothing to drain.** MediaMTX logs to stdout
(`logDestinations: [stdout]`), which `Read-CamChildOutput` captures and redacts; stderr only carries a
Go panic, and none occurred. The function was called on every exit and handled empty stderr
correctly. The redaction path is proven by the credential leak scan over the sandbox (0 hits, positive
control lit), not by a redacted stderr line.

**Format-CamAgeText moved too**, beyond the five helpers the brief named. Same reason as the other
five: `cam.health.ps1` is its second consumer, and two age formatters would let `-Status` and the
strip drift. Flagged as a deliberate deviation.

## Carried forward after C2a

- [x] **[31]** DONE C2b. The real strip is 207 columns, not 120, so the stale reason now gets
      whatever the fixed parts of the state line leave rather than a fixed 42 chars. The whole
      sentence is readable.
- [ ] **[32]** `Restreamer.Paths` in `status.json` lists CONFIGURED paths, not READY ones. The strip
      reports it as `N path(s)` and tells relay from direct per camera with `InputMode`, which is
      honest, but a reader could misread the count as readiness. Consider a `ReadyPaths` count in a
      later phase; do not have the strip probe the API for it.
- [x] **[33]** DONE Phase D, known limitation 9. The harness leaves its sandbox under `%TEMP%\cam-tests\<name>` and its evidence under
      `%TEMP%\cam-tests\evidence` on purpose, for inspection. The credential copy is deleted; real
      footage from a live run is not. Mention the cleanup in the Phase D README.


## Phase C2b delivered 2026-10-11 - the strip is reserved, placed and measured on screen

**Geometry, measured from a screenshot of the running dashboard, not computed.** Video ends at
y=359, a uniform 8 px band runs y=360..367, video resumes at y=368 and ends at y=727; another 8 px
band to y=735; the strip's painted top border is at y=736 and its bottom at y=1031, which is the
last row of the 1032 px working area. All six mpv windows report `vis == winrect == 640x360` at
(0,0) (640,0) (1280,0) (0,368) (640,368) (1280,368) - `--no-border` mpv has no DWM inset, so the
feeds really are full-bleed with no gap at the left, right or top edge.

**Three Windows mechanics that had to be measured, and are now encoded in the script:**
- A resizable window's rect includes an invisible DWM resize border, 7 px here on each side and the
  bottom. Sizing the window rect to the strip leaves a visible gap along the bottom of the screen.
  The placement iterates on the difference between `GetWindowRect` and
  `DWMWA_EXTENDED_FRAME_BOUNDS`, so it also self-corrects under a different theme or DPI.
- Windows Terminal mis-sizes itself when the window rect runs past the right edge of the screen: a
  request for 1934x303 at x=-7 came back as 1936x563. The width is clamped to the screen edge, not
  the position, so no text is pushed off the left. The cost is the one open cosmetic item below.
- `wt --focus` drops the tab bar and is worth two text rows: the same 1913x296 window carries 208x15
  cells with it and 208x13 without.

**`schtasks /create /sc onlogon` is denied to a standard user**, in the root folder and in a
subfolder alike. `Register-ScheduledTask` with a per-user `LogonTrigger` and an interactive, limited
principal succeeds unelevated, which is what `-InstallAutostart` uses. `schtasks /query` reads it
back fine either way.

**The events section reads the supervisor log's tail, published by `Write-CamStatusFile`**, rather
than a new in-memory ring. The log is already timestamped and redacted, is only written when
something happens, and survives a supervisor restart; instrumenting the forty `Write-CamLog` call
sites would have been forty edits for a worse result. The strip still reads exactly one file.

## Carried forward after C2b

- [x] **[34]** NOT FIXED, DOCUMENTED Phase D as known limitation 11. The Operator accepted the
      sliver rather than replacing the terminal with a custom window host. The strip paints
      x 6..1906 of 1920, so about 6 px of desktop shows at its
      left edge and 13 px at its right. Top and bottom are exact and no video is affected. It cannot
      be closed while the strip is a Windows Terminal window, because covering the full width needs
      a window rect wider than the screen and that is exactly what makes WT mis-size itself.
- [x] **[35]** DONE Phase D, and the repo does NOT move. `-InstallAutostart` now writes a launcher
      at `%LOCALAPPDATA%\IPCCTV\start-recorder.ps1` that polls for the recorder path for up to
      300 s, logs to `autostart.log` beside it, then forwards to `cam.recorder.ps1 -Run`. It
      never copies the scripts. The task points at the launcher and `-RemoveAutostart` deletes
      both. Known limitation 10 states the remaining WSL dependency. Original note: the task
      points at `\\wsl.localhost\...` when the repo lives in WSL, and that
      path does not exist until WSL is running. `-InstallAutostart` warns about it. The README should
      tell the Operator to copy the scripts to a local drive before relying on autostart.
- [x] **[36]** DONE Phase D: the task registers `-RestartCount 3 -RestartInterval 1 minute`,
      verified in the exported XML (`<RestartOnFailure><Interval>PT1M</Interval><Count>3</Count>`).
      Original note: the logon task had no restart-on-failure, so if the supervisor process died the task did
      not bring it back until the next logon. Deliberately out of C2b's scope. Consider
      `-RestartCount`/`-RestartInterval` in a later phase.


## Phase D delivered 2026-10-11 - the fixes and the README

**mpv is quiet and `stimeout` is gone.** `--msg-level=all=error` keeps mpv's chatter out of the 8 px
gutter, which shows the terminal window behind the feeds; real errors and the pane loop's `Stream
interrupted` message still print. `stimeout=6000000` was removed from `$Flags` because it never did
anything: libavformat renamed it to `timeout` at ffmpeg 5.0, mpv ships `avformat-63.dll`, and the
option was silently dropped. The comment claiming it was what made mpv exit on a frozen stream is
corrected in the script and in the README - `timeout=5000000` plus `--network-timeout=3` are what
protect. Before and after are visible in the same 50 px crop of the gutter: the old shot reads
`File tags:` and `VO: [gpu-next] 2560x1440 yuv420p`, the new one is a clean dark band.

**Autostart goes through a launcher now.** The task points at
`%LOCALAPPDATA%\IPCCTV\start-recorder.ps1`, generated at install, which polls for the recorder's
path for up to 300 s, logs to `autostart.log` beside it, then forwards to `cam.recorder.ps1 -Run`.
It forwards and never copies, so it cannot drift. Giving up with exit 2 is deliberate: that is what
hands the problem to the task's new restart-on-failure policy. The repo did not move.

**The screenshot is real, not a fixture.** `docs/dashboard.png` was taken with a live supervisor:
six recorders writing off the relay, the strip reporting that supervisor's own state, events and
archive. Reduced to a 256-colour palette, 894 KB instead of 2.4 MB, strip text unchanged.

## Carried forward after Phase D

- [ ] **[29]** TECHNICAL DEBT, unchanged and now due: `cam.recorder.ps1` is 3,432 lines. The feature
      is complete and green, so the split can be commissioned.
- [ ] **[32]** `Restreamer.Paths` still counts CONFIGURED paths, not READY ones. Documented as README
      limitation 12 rather than fixed.
- [ ] **[23]/[24]** The long-run paths named in [24] remain unexercised: log rotation at
      `LogMaxBytes`, the full 900 s segment, `HealthyResetSeconds`, the 2250 s stall watchdog,
      `MaxRetentionDays`. README limitation 7 says so plainly.


## Relay-readiness race fixed 2026-10-12 - both consumers decide from one measurement

**The fault, measured twice.** `Get-CamInputUrl` probed the MediaMTX control API once per
camera with no retry, so a six-camera start fired six near-simultaneous probes and took whatever
each one said. Workshop was started one second before MediaMTX had its paths up and recorded
direct for its whole run; on a dashboard launch where five of six paths were confirmed serving,
two panes still chose direct. Each one burns the spare camera session the relay exists to free.

**One measurement per batch.** `-ReadyPaths` answers from the set the batch fetched once with
`Get-CamRelayReadyPaths` and makes no API call at all; `-WaitSeconds` keeps asking while the
answer is still no. Both optional, and absent is exactly the old single probe, so a lone restart
and the per-attempt fallback are unchanged. The harness asserts the no-probe property by the
clock against an unroutable API (`-Checks inputs`).

**What decides whether a recorder child waits: the RESTREAMER'S age, not the child's.** First
attempt was the obvious rule and it was wrong. Kill MediaMTX and every child correctly
reconnects directly - and the second camera session is then taken, so MediaMTX cannot pull that
camera when it returns, the path never serves again and nothing comes back to the relay.
Measured: five of six paths dead 90 s after a kill. A child now waits for its own path while the
restreamer is younger than `RelayReadyWaitSeconds`, which is exactly when paths are still coming
up - cold start or restart. An older restreamer, or none, decides on the spot.

**`Start-CamRelayChild` waits for the control API, not for every path.** Workshop is offline, so
its path can never become ready and waiting for the whole set spent the entire 20 s budget on
every cold start, behind a camera that was never coming. Each child waits for its own path
instead, so a straggler delays nothing but itself.

**Three config keys**: `RelayReadyWaitSeconds` 15, `RelayReadyPollMs` 500, `StartStaggerMs` 300.
The stagger applies to both consumers: the supervisor sleeps it between consecutive child starts,
and each mpv pane sleeps its index times it before deciding.

**Evidence, four cold starts of both consumers.** Five serving paths at `readers=2` every time,
all five recorders on the relay within 3 s of each other, Workshop on direct after its bounded
wait and delaying nobody. On the third start only THREE of six paths were serving when the API
answered, and Field Front and Car Garage both waited and got the relay - pre-fix they were two
more direct connections.

## Carried forward after the relay-readiness fix

- [x] **[37]** DONE 2026-10-12: an mpv pane that chose direct now watches its own relay path and
      returns to the relay on its own. See the section below.
- [ ] **[38]** The six pane commands are **28382** of the 32767 characters Windows allows on a
      command line, because each pane travels as a base64 blob on one `wt.exe` invocation. Seven
      cameras would not fit. The launch now prints the count and throws over 32000 instead of
      silently opening nothing, which is what it did at 42040. A real fix is moving the pane loop
      into `cam.common.ps1` and passing arguments instead of a script. The lifecycle work added only
      334 characters - `Add-CamJobMember` is one 16-character line because the function itself lives
      in the library - so the move was not needed to fit and was left undone.


## Viewer promotion added 2026-10-12 - item [37]

**The asymmetry.** A recorder child promotes itself off a direct connection because its supervisor
restarts it and re-decides. A pane only re-decides when mpv exits, and mpv on a healthy DIRECT
stream never exits, so a pane that fell back during a relay outage held that camera's second RTSP
session until the dashboard was relaunched. Measured before the fix: after a MediaMTX kill and
recovery the serving paths sat at `readers=1`, the recorder back and the viewer not.

**The shape.** `Invoke-CamViewer` in `cam.common.ps1` runs mpv as a child and, only when this
attempt chose direct, asks every `ViewerPromoteCheckSeconds` (default 10) whether this ONE path is
serving again. When it is, `Stop-CamViewer` closes mpv and the function returns $true so the loop
decides again at once instead of printing an interruption and sleeping. A pane already on the relay
watches nothing: if the relay dies mpv loses its input and exits by itself. The agreed cost is one
brief blink in that pane, once per outage.

**Two details that are easy to get wrong.** The wait is `Process.WaitForExit($ms)`, so the poll
keeps to the clock, costs nothing between polls, and still notices mpv's own exit in the same call -
that is what makes the permanently offline Workshop pane safe to leave polling for a day.
`mpv.com` is a console wrapper that re-execs `mpv.exe`: the video window belongs to the CHILD, so
stopping the wrapper would orphan a window still holding the camera. `Stop-CamViewer` closes the
child's window (measured: child out in ~220 ms, wrapper ~140 ms later), and only kills - by handle,
never by image name - if the close is ignored.

**Command-line budget.** The pane template grew 51 characters and lost the stderr comment it no
longer needs. The `wt.exe` invocation measures **27803** of Windows' 32767, up from 26987, with the
launcher's own guard still throwing above 32000.
## Viewer promotion verified by the advisor, 2026-10-12

The executor delivered the fix but could not run J1-J3: it had closed the Windows Terminal window
while the six pane processes survived it, leaving a headless dashboard, and it was denied permission
to stop those processes. It then asked the advisor to run the denied command on its behalf. That was
REFUSED as permission laundering and surfaced to the Operator, who authorised the cleanup himself.

Cleanup was done by process tree rather than by name, after identifying that pid 4432 - the autostart
launcher - was the parent of every ffmpeg and therefore the recorder supervisor. 13 orphaned panes
(6 from the current launch, 6 stale from earlier test launches, 1 cam.health) and 8 mpv were stopped;
recording was never interrupted.

**Relaunch on the fixed code**: all five healthy cameras chose `relay`, Workshop `direct` (camera
offline). Command line 27803 of 32767. Strip measured 1913x296 at +0+736.

**J1/J2/J3 PASS - two kill-and-recover cycles, run by the advisor:**

```
cycle 1  13:56:11 BEFORE  cargar=2 fieldf=2 fronte=2 rearba=2 worksh=2
         +  5s  RELAY DOWN
         + 10s  cargar=1 fieldf=1 worksh=1      relay back, viewers still direct
         + 20s  all five back to 2              viewers promoted themselves
         stable to +120s

cycle 2  13:58:32 BEFORE  all five at 2
         + 10s  cargar=0 fieldf=1 fronte=1 rearba=1 worksh=1   all five fell back
         + 20s  all five back to 2
         stable to +60s
```

Recovery is ~20 s, automatic, with no dashboard relaunch. Cycle 2 is the stronger case: every viewer
fell back and every one returned.

- [x] **[39]** DONE 2026-10-12: the dashboard stops any previous `cam.health` by process identity
      before starting a new one. Root cause was that the old code closed a WINDOW found by title and
      never touched a process - and the title does not hold, because when the renderer exits
      `-NoExit` returns to a prompt and the title reverts to the shell default. See the section below.
- [x] **[40]** DONE 2026-10-12: closing the window now stops the panes and their mpv, held by a
      Win32 job object with `KILL_ON_JOB_CLOSE` owned by a warden that watches the window. See the
      section below.


## One control surface and a hard pane lifecycle, 2026-10-12 - items [39] and [40]

**`cam.ps1` is the single place.** `-Status`, `-Start`, `-Stop`, scoped with `-Recorder` or
`-Dashboard` and meaning everything when unscoped. `-Stop` counts the same components before and
after from one shared snapshot and exits non-zero if anything in scope survived.

**Nothing is identified by image name or window title.** `Get-CamProcessInventory` labels each
process by the work it owns: the supervisor is the parent of every ffmpeg, and panes, the health
strip and the warden are found by DECODING the `-EncodedCommand` they were handed, since a pane's
command line otherwise says nothing but `powershell.exe -NoExit -EncodedCommand JABF...`. The
recording supervisor and its children sit on a keep list every stop honours.

### [40] - why the panes survived, and what holds them now

**A pane cannot see its own window close.** Measured three ways: `GetConsoleScreenBufferInfo`,
`GetConsoleMode` and `GetConsoleProcessList` all kept succeeding for 60 s after the window was
gone, and all seven `OpenConsole.exe` pty hosts were still alive. Windows Terminal leaks the pty
when the window closes with processes still running, so there is no signal inside the pane to watch
- and the loop is blocked inside mpv for hours anyway. A single-pane window is different: closing
one DOES kill its pane in under a second, which is what made the six-pane case look inconsistent.

**So the kernel holds the dependency.** A warden process creates a named job object with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` and holds the only handle. Each pane's first act is
`Add-CamJobMember`, before mpv exists, so mpv inherits membership. Window gone -> warden exits ->
last handle closes -> every pane and every mpv terminated at once.

**The trap that cost an hour: a member must CLOSE ITS HANDLE after assigning itself.** Membership
is not a handle. With six panes each holding an open handle the owner's death closed nothing and
all six survived; with the handles closed, killing the owner took every member and child in under
two seconds.

**The window needs a stable identity.** `-w cctv-grid` gives the grid its own window - without it
the six panes arrive as a TAB in whatever terminal was last used, because this machine's
`windowingBehavior` is `useExisting`, and the Operator's own shell was a candidate. `--title` plus
`--suppressApplicationTitle` on EVERY pane keeps the title put; `--title` alone is overwritten by
the shell and the window title then follows whichever pane has focus. The flags belong to the
`new-tab`/`split-pane` subcommand, not among the global options - put up with the globals, `wt.exe`
parses nothing and opens no window, silently.

**K4 measured:**

```
BEFORE  window=1 warden=1 panes=6 mpv=12 health=1 | ffmpeg=5 mediamtx=1 job=True
+2s     window=0 warden=0 panes=0 mpv=0  health=0 | ffmpeg=5 mediamtx=1 job=False
```

### A supervisor can outlive its own children's ability to be stopped

Observed this session: the supervisor died with five ffmpeg still writing and MediaMTX still
serving, `status.json` still saying `running`, and their parent pid pointing at nothing. The
recorder's own `-Stop` has no supervisor to signal in that state, so nothing could stop them.
`cam.ps1 -Stop` now gives the orderly path 20 seconds and then stops whatever outlived it. The
cause of the supervisor's exit is NOT established - no log line, no Application event - but it was
not a job termination, since MediaMTX was in the same parent and survived.

### Harness

`procs` asserted zero ffmpeg and zero MediaMTX, so it failed on any machine where the Operator's
recorder was running - which is the state the harness is required to leave behind. It now skips
when a live supervisor owns them, identified by the ffmpeg it parents plus an agreeing heartbeat.
9 passed, 0 failed, 2 skipped.


## A stop that never steals focus, 2026-10-12 - item [42]

- [x] **[42]** DONE 2026-10-12: stopping the dashboard no longer touches the foreground window.
      `Stop-CamTerminalWindow` has no `-ConfirmKeystroke` path left, and `CamWin` no longer imports
      `SetForegroundWindow` or `keybd_event` - nothing in the project synthesises input.

**The focus steal was a wrong conclusion, not a necessary cost.** The old code posted `WM_CLOSE`,
polled ONCE 400 ms later, found the window still there, and inferred that Windows Terminal's "close
all tabs?" overlay must be waiting for a keystroke - so it took the foreground and sent an Enter.
The window had simply not finished closing.

Measured on the live six-pane grid, posting `WM_CLOSE` and sending NOTHING:

```
job terminated first (panes 6 -> 0, mpv 12 -> 0 in under a second), window still open 6 s later
then WM_CLOSE        window gone in under 800 ms, no overlay
panes still running  window gone in under 1500 ms, no overlay, warden then stopped the panes
```

So this window never raises that confirmation: it is ONE tab holding six panes, and the
confirmation is about closing several TABS. The note in the [40] section that a multi-pane window
answers `WM_CLOSE` with the overlay is wrong, and this supersedes it.

`cam.ps1 -Stop -Dashboard` measured with the foreground window sampled every 100 ms from a separate
process, with a terminal window of our own holding focus the way the Operator's shell would: 59
samples across the 4.9 s stop, ONE unbroken run on the same HWND. Nothing moved.

The ordering the Operator asked for is what makes it work and it was already in place: terminate
the job, let the kernel take every pane and every mpv, THEN ask the window to close. The window
close follows; it never drives.


## U1 delivered 2026-10-12 - the Go toolchain, the skeleton, the guard and the main screen

First commission of the V1 control UI (plan `~/.claude/plans/luminous-wibbling-allen.md`). The U0
mockup items **[41]-[45]** were delivered as `ui/mockup/mockup.html` + `ui/mockup/fixture.json` in
27d4273 but were never written into this file; note also that **[42]** is used twice here, once for
the U0 settings mockup in the plan and once for the focus-steal fix above.

- [x] **[46]** Go 1.27.2 installed in WSL under `~/.local/go` from go.dev (apt has only 1.13), sha256
      verified against go.dev's published checksum. `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build`
      produces a 7.2 MB `camui.exe` that runs on Windows. Install and build documented in
      `docs/go-toolchain.md`; `ui/build.sh` runs `gofmt -l`, `go vet` and `go test` before building.
- [x] **[47]** `ui/` skeleton: `main.go` (flags, config, logging, graceful stop), `config.go` (config
      file + flags + `CAMUI_TOKEN`, and the guard), `api.go` (`GET /api/status` and redaction),
      `static/` embedded with `go:embed`. No external files at runtime, no front-end build step, no
      CDN - the front end is plain CSS and JS with no dependencies. Listen address, archive root and
      status file come from flags or a JSON config file and default to loopback and `E:\cameras`.
      Logging is one `log/slog` text line per event on stdout; query strings are never logged because
      one of them can carry the token.
- [x] **[48]** The loopback guard: any listen address that is not exclusively loopback is **refused**
      (exit 2) unless a token of at least 16 characters is configured, and when one is, it is required
      on every request. Nine table cases in `config_test.go` plus the refusal and the successful bind
      demonstrated on Windows.
- [x] **[49]** The main screen wired to `/api/status`: the hero figure (days of footage headroom) and
      three stat tiles, the camera table with status, input mode, last segment age, restarts and a
      record toggle, recent events, and a services panel for both Scheduled Tasks. Layout A above
      760 px, layout C below it, chosen by CSS media query. Every state carries a dot AND a word.
      The record toggle and the global action buttons render disabled - U2 wires them.

### Two defects found and fixed in this commission

**`schtasks /FO CSV /V` cannot be parsed.** It does not escape the quotes inside `Task To Run`
(`powershell.exe -File "C:\...\start-recorder.ps1"`), so a CSV reader silently shifts every column
after it: `Scheduled Task State` came back holding the `Idle Time` value and both tasks reported as
`Disabled` while `cam.ps1 -Status` correctly said `Ready`. `/FO LIST /V` is line-oriented and immune.
Caught only by comparing the API against the CLI - the API was self-consistent and wrong.

**`schtasks` prints in the console's OEM codepage**, so `Last Run Time` arrives as mojibake on this
machine's Greek locale (`12/??/2026 18:55:25`). That field is no longer surfaced at all; `Last Result`
says the same thing in digits, and every schtasks-derived string goes through `strings.ToValidUTF8`.

### Credential containment

Redaction runs over the whole decoded status document, at any depth: keys named `RelayBase`,
`Password` or `Username` are dropped, every `scheme://...` token is reduced to `scheme://[redacted]`
and every bare IPv4 literal is replaced. `Restreamer.RelayBase` is therefore absent from
`/api/status`, and no camera address or credential can reach the browser even inside ffmpeg error
text. Proven by a Go test with a positive control and by `-Checks uileak` in the harness.


## The NVR window, 2026-10-12 - items [50]-[53]

- [x] **[50]** DONE 2026-10-12: the supervisor no longer dies when an unrelated terminal window is
      closed. Root cause below - it was never "an unrelated terminal".
- [x] **[51]** DONE 2026-10-12: `cam.recorder.ps1 -Nvr` opens the NVR: one Windows Terminal window
      of its own, titled `cam.nvr`, with the supervisor as its only pane and the supervisor log live
      on screen.
- [x] **[52]** DONE 2026-10-12: closing that window stops the supervisor, MediaMTX, every ffmpeg and
      the viewers, with zero orphans. Held by the job object `cam.nvr` and a warden.
- [x] **[53]** DONE 2026-10-12: the `IPCCTV-Recorder` logon task forwards `-Nvr` instead of `-Run`.
      Modified, not duplicated - one registered task.

### Root cause: the supervisor's console was a TAB in the Operator's terminal

Windows Terminal is this machine's default terminal application, so a console created WITH a window
is handed off to it. Measured: creating one activated `OpenConsole.exe -Embedding` and the console
appeared as a tab in an existing Windows Terminal window, because `windowingBehavior` is
`useExisting`. The `IPCCTV-Recorder` task created exactly such a console for the supervisor -
`-WindowStyle Hidden` does not prevent it, because the window belongs to Windows Terminal, not to
powershell.exe. The Operator's "unrelated" window was hosting the recorder.

Two signatures, one cause:

- **Closing the window** sends `CTRL_CLOSE_EVENT`. PowerShell terminates without running any
  `finally`, so there is no shutdown line and the children - which have consoles of their own - are
  orphaned and keep writing. That is 20:30:47.
- **Closing the tab, or Ctrl+C in it**, stops the pipeline instead: the `finally` runs in full and
  logs `supervisor shutting down: supervisor shutdown` with `$stopRequested` false, and the launcher
  stub's own pipeline is stopped too, which is why `autostart.log` has no "the supervisor exited
  with N" line for any of them. That is 16:57:03, 19:06:17 and 20:18:56. All four deaths lack that
  stub line, which is what ties them together.

### What holds it now

`Start-CamNvrWindow` starts a warden, waits for it to create and arm the job `cam.nvr`, then runs
`wt.exe -w ipcctv-nvr new-tab --title cam.nvr --suppressApplicationTitle`. The pane joins the job as
its first statement and only then becomes the supervisor, so MediaMTX and every ffmpeg inherit
membership at creation. No console is adopted anywhere, so no other window can reach the supervisor.

`Start-CamJobWarden` is the dashboard warden generalised - one warden per window that owns a job.
`Start-CamDetachedProcess` replaces `Start-Process -WindowStyle Hidden` for both wardens: measured,
`Start-Process` leaves the child attached to the LAUNCHER's console, so closing the terminal a
launch was typed into would have closed the last handle to the job and killed everything it
protected. `CREATE_NO_WINDOW` gives the child its own console, no window and no handoff.

**T2 measured:**

```
BEFORE  ffmpeg=4 mediamtx=1 supervisor=[69900] nvrWindow=1 nvrWarden=1 job=True
+1s     ffmpeg=4 mediamtx=1 supervisor=[]      orphanParent=[69900]      <- CTRL_CLOSE took the pane
+9s     ffmpeg=0 mediamtx=0 supervisor=[]      nvrWindow=0 nvrWarden=0 job=False
```

The eight seconds of orphaned ffmpeg were the warden stopping the viewers before closing its handle.
It now closes the handle first; the viewers follow.

### The one residual risk - a foreign console can still be adopted INTO the NVR window

Measured on 2026-10-12 with a surrogate started the way the task used to start the supervisor: its
console was adopted as a tab in the **`cam.nvr` window**, and while it was the active tab the NVR
window's title read `NVR-REPRO-SURROGATE`, so `Get-CamTerminalWindow -Title cam.nvr` found nothing.
`--suppressApplicationTitle` pins our own pane's title and cannot pin a tab Windows Terminal adds.

The warden is unaffected - it resolves the HWND once and then polls `IsWindow` - so recording is not
at risk. What is at risk is the Operator closing what he thinks is a stray console window and
closing the NVR with it, which is the original complaint wearing a different hat.

The fix is one line in the Operator's own Windows Terminal settings and is his call, not ours:
`"windowingBehavior": "useNew"` makes an adopted console open its own window instead of joining one.
Setting the default terminal application to Windows Console Host would do it too. Neither was
changed here.

### Also fixed in passing

The supervisor log is echoed to the NVR window by `Set-CamLogEcho`, registered by `-Run` for
`recorder.log` only - the per-camera logs carry raw ffmpeg stderr and would bury it. Fifteen
`Write-CamHost` calls that printed the same event without a timestamp were removed; the echo covers
them.


## Crash safety and alerting, 2026-10-13 - items [54]-[58]

- [x] **[54]** DONE 2026-10-13: the supervisor owns a job object of its own, so its death takes
      MediaMTX and every ffmpeg with it. Measured twice on the live NVR: 4 ffmpeg + 1 MediaMTX to
      zero in under one second, zero orphans.
- [x] **[55]** DONE 2026-10-13: the NVR pane is now a HOST that runs the supervisor as a child and
      restarts it when it stops without being asked to. Recovered in 11s and 13s in the two live
      tests, with no intervention.
- [x] **[56]** DONE 2026-10-13: an unexpected stop alerts on three channels - a Windows toast, an
      Application-log entry, and `StopKind` plus `LastUnexpectedStop` in `status.json`. An orderly
      `cam.ps1 -Stop` raises none of them.
- [x] **[57]** DONE 2026-10-13: a camera that has written nothing for `CameraDownAlertMinutes`
      alerts on the same three channels and publishes `DownSince` per camera.
- [x] **[58]** DONE 2026-10-13: `cam.recorder.ps1 -InstallEventSource` registers the custom
      `IPCCTV` event source when run elevated. Everything works without it.

### Why the pane had to stop BEING the supervisor

From outside, a deliberate window close and a supervisor crash were the same event: supervisor
gone, `status.json` still saying `running`, window destroyed. The warden watching that window
cannot tell them apart, and guessing wrong is bad either way - restart on a deliberate close and
the Operator cannot stop his own NVR; stay down on a crash and the cameras record nothing.

So the pane is now `Start-CamNvrHost`, which runs the supervisor as a child:

- **window closed** - host and supervisor share a console, both take `CTRL_CLOSE_EVENT`, both die,
  and the `cam.nvr` warden sweeps the rest exactly as before. Unchanged.
- **supervisor died** - the host is untouched, the window is still open, and the child exiting IS
  the signal. The restart happens in place: no window churn, no job handed between wardens, no race.

Expected or not is read from what the supervisor recorded, never from an exit code alone, because a
killed process has no say in its exit code. The stop flag, `StopKind` `requested` or `interrupted`,
and exit 20 ("another supervisor is already running") all mean do not restart. Budget is 5 restarts
in 30 minutes; out of budget it alerts once more and stays down rather than looping all night.

### The two job objects are different questions

| job | owner | fires when |
|---|---|---|
| `cam.nvr` | the warden, outside the window | the WINDOW closes |
| `cam.supervisor.<hash of root>` | the supervisor itself | the SUPERVISOR dies, however abruptly |

Jobs nest, so the supervisor being inside `cam.nvr` already is not an obstacle - measured
2026-10-13, the outer job was unaffected and the inner one swept both children in under a second.
The supervisor's job is named per archive root, so a test sandbox cannot collide with the live NVR.
`CamJob::CreateAndJoin` is the mirror image of `JoinCurrentProcess`: the owner KEEPS its handle,
and process exit closing that handle is the whole mechanism.

### The event log needed measuring

Registering a custom event source writes to HKLM. Measured on this machine as a standard user:
`eventcreate.exe`, `New-EventLog` and a direct registry write all return **Access denied**. Writing
to the Application log under an **already registered** source does not. So alerts go out under the
`Application` source with an `IPCCTV: ` prefix and need no elevation anywhere;
`-InstallEventSource` registers the custom source when run elevated and is optional.

Two further traps: `[EventLog]::SourceExists()` THROWS unelevated because it cannot open the
Security log, so the source probe reads the registry instead. And a source with no message resource
leaves `Get-WinEvent`'s `.Message` EMPTY - the text is in `.Properties[0].Value`:

    Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'Application'; Id = 100,101,102,110 } |
      Select-Object TimeCreated, Id, LevelDisplayName, @{ n = 'Text'; e = { if ($_.Message) { $_.Message } else { $_.Properties[0].Value } } }

`-InstallEventSource` registers with `EventCreate.exe` as the message resource, which makes
`.Message` render, and prints the right reader command either way.

### Camera-down is judged on file progress, never on process liveness

A camera that cannot be reached has its ffmpeg restarted every backoff period, so it is alive half
the time and its `StartedAt` is always recent. Segments either appear or they do not. The
supervisor's own start is the floor, so a camera that was already dead before this run is not
reported as down for longer than there has been anything watching it.

### Known gap, not fixed here

After an orderly `cam.ps1 -Stop -Recorder` the NVR window stays open with its pane at a prompt, and
`cam.ps1 -Start` then finds a window titled `cam.nvr` and reports "there is nothing to do" without
starting anything. Pre-existing - the pane could not be made to run a second supervisor before
either. The restore today is to close the window and run `-Nvr` again.


## U2 delivered 2026-10-13 - control actions and settings editing

Second commission of the V1 control UI (plan `~/.claude/plans/luminous-wibbling-allen.md`). The plan
numbers these U2 items **[50]-[53]**, but those numbers were already spent on the NVR window above,
so they are recorded here as **[59]-[62]** in plan order. (This file already notes [42] being used
twice for the same reason.)

- [x] **[59]** (plan [50]) `POST /api/control` taking `{scope: all|recorder|dashboard, action:
      start|stop}` and shelling to `cam.ps1`, plus `POST /api/cameras/{name}/record` taking
      `{enabled}` and shelling to `cam.recorder.ps1 -Enable|-Disable`. One action at a time: a second
      request while one is running is answered 409 rather than allowed to race it. The page shows the
      pressed button as `Stopping...`, disables every other action, and keeps polling.
- [x] **[60]** (plan [51]) `GET/PUT /api/cameras`. Name non-empty, filesystem-safe, not a reserved
      Windows device name, no duplicate and no two names that would collide to the same archive
      FOLDER; address a valid IP literal or RFC 1123 hostname with no scheme, port, path or
      userinfo; stream name non-empty and conservative. Any invalid row refuses the WHOLE request
      with 422 and a per-field message, and the writer is not invoked at all. Writes go through
      `Set-CamList` by way of `ui/scripts/camui-apply.ps1`, which is embedded in the binary.
- [x] **[61]** (plan [52]) `GET/PUT /api/config` over the allow-list `SegmentSeconds`, `ReserveGB`,
      `PruneHysteresisGB`, `MaxRetentionDays`, with ranges and a message that says what would be
      right. The GET is built key by key FROM the allow-list rather than by removing keys from the
      document, so a key added to `recorder.config.json` later cannot leak through it.
- [x] **[62]** (plan [53]) Confirmation on exactly the actions that stop footage being written: stop
      everything, stop the recorder, turn a camera's recording off, remove a camera row, and saving a
      camera list that does either. Starting anything and stopping the VIEWERS are not confirmed -
      a prompt on a harmless action teaches the Operator to dismiss prompts.

### Three defects found by running it, not by reading it

**A control action hung on the processes it started.** `POST /api/control
{scope:dashboard,action:start}` brought six viewer panes up in 20 s and then did not return for over
four minutes, holding the action lock so the next six requests were all answered 409.
`cmd.CombinedOutput` gives the child a PIPE and Wait blocks until every holder closes it - and the
panes inherit it and live for hours. The child's output now goes to a temp `*os.File`, which Go hands
over directly with no copying goroutine, so Wait returns when the script exits.

**The camera writer matched no rows at all.** `@(Get-CamList)` nests: the function ends in
`return , @($parsed)` to stop a one-camera list collapsing to a scalar, and wrapping that CALL in
`@()` yields one element whose value is the whole list. Every name comparison then ran against a
space-joined string of all six names, so a valid save was refused with "No camera named 'X' is in
cameras.json". Assign first, then wrap. Already in the executor memory store as
`powershell-array-return-nesting.md`, and it fired exactly as written there.

**A start was reported as success when nothing started.** `cam.ps1 -Start -Recorder` exited 0 having
printed "did not report running within 20s". The page now reads the outcome back from `/api/status`
and says "...returned success, but the recorder is still not running: <reason>. Nothing is being
captured."

### The known gap above, measured - and it is not in camui

- [ ] **[63]** NOT FIXED, and it bit this commission: after `cam.ps1 -Stop -Recorder`, the NVR's
      pane and warden survive, so the next `-Start -Recorder` cannot bring the supervisor back and
      exits 0 anyway. Measured today with the Operator's dashboard open, which makes it worse: the
      NVR tab had been ADOPTED into the dashboard's Windows Terminal window, so no window titled
      `cam.nvr` existed at all and `-Nvr` refused with "the job 'cam.nvr' already exists but no
      window titled 'cam.nvr' is open". **`cam.ps1 -Start -Recorder` run directly from a plain
      PowerShell failed identically, so this is a recorder-side gap, not a UI one.** Recovery was to
      stop the `NvrHost` and `NvrWarden` by pid from `Get-CamProcessInventory` and run `-Nvr` again.
      Two fixes, neither in U2's scope: `"windowingBehavior": "useNew"` in the Operator's own Windows
      Terminal settings (already his to make), and `-Stop -Recorder` taking the NVR host down with
      the supervisor so `-Start` has nothing stale to trip over.
