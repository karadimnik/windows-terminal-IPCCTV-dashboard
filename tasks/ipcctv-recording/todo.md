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
- [x] **[15]** DONE (everything is committed and merged locally). Commit to `executor/recording-feature`. No merge, no push without approval.

## Deferred

- [ ] **[16]** Motion-triggered recording, simple version. Research ONVIF Profile S event
      subscription first - these are ONVIF cameras, so the built-in detector may well be reachable,
      which beats re-detecting motion on the PC. ffmpeg `select`/`scene` only as fallback.
- [x] **[17]** DONE: the MediaMTX relay has been the production input path since 2026-10-12. Local restreamer revisit (go2rtc / MediaMTX) - one camera connection fanned out
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
- [x] **[23]** Operator to run DONE 2026-10-15, reported "done ok" by the Operator (camera-facing -SelfTest).  camera-facing `-SelfTest` (checks b/c/d) with the dashboard CLOSED.
      Never executed - the Phase B agent was forbidden from touching cameras.
- [x] **[24]** Untested over time: DONE 2026-10-15: parts A and B and the production soak exercised every path named here in a sandbox. log rotation at `LogMaxBytes`, the 900 s production segment path,
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

- [x] **[29]** TECHNICAL DEBT, unchanged and now due: PRIORITY LOWERED by the Operator 2026-10-15: "very low priority, especially if we switch architecture and make it OS independent". Do not commission.  `cam.recorder.ps1` is 3,432 lines. The feature
      is complete and green, so the split can be commissioned.
- [ ] **[32]** `Restreamer.Paths` still counts CONFIGURED paths, not READY ones. Documented as README
      limitation 14 rather than fixed.
- [x] **[23]/[24]** The long-run paths named in [24] remain unexercised: [23] DONE by the Operator 2026-10-15; the [24] long-run paths have since been exercised in a sandbox (parts A and B); only the production-values soak remains.  log rotation at
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

- [x] **[63]** FIXED 2026-10-13. `-Stop -Recorder` now takes the NVR window down with the
      supervisor (`Stop-CamNvr`: the warden first, because its handle is the last one on the
      `cam.nvr` job, so the kernel sweeps the host pane with it and a terminated warden never
      reaches the branch that would also stop the dashboard). `-Start -Recorder` exits 1 and says
      NOT RECORDING when the supervisor does not report in, and its wait went from 20s to 60s
      because the measured launch takes 18-32s and 20s was itself producing false alarms.
      `Start-CamNvrWindow` now reads what is actually recording from the process tree, clears a
      dead NVR itself, and refuses only when clearing would kill a working supervisor whose window
      cannot be matched. Verified: two stop-then-start cycles leave 6/6 recording and exit 0; a
      `cam.nvr` job held by a stray warden with no window is cleared automatically by
      `-Start -Recorder`; a job held by something that is neither the host nor the warden exits 1
      and names the recovery. As measured, for the record:

      The original report: after `cam.ps1 -Stop -Recorder`, the NVR's
      pane and warden survived, so the next `-Start -Recorder` cannot bring the supervisor back and
      exits 0 anyway. Measured today with the Operator's dashboard open, which makes it worse: the
      NVR tab had been ADOPTED into the dashboard's Windows Terminal window, so no window titled
      `cam.nvr` existed at all and `-Nvr` refused with "the job 'cam.nvr' already exists but no
      window titled 'cam.nvr' is open". **`cam.ps1 -Start -Recorder` run directly from a plain
      PowerShell failed identically, so this is a recorder-side gap, not a UI one.** Recovery was to
      stop the `NvrHost` and `NvrWarden` by pid from `Get-CamProcessInventory` and run `-Nvr` again.
      Two fixes, neither in U2's scope: `"windowingBehavior": "useNew"` in the Operator's own Windows
      Terminal settings (already his to make), and `-Stop -Recorder` taking the NVR host down with
      the supervisor so `-Start` has nothing stale to trip over.

---

## U3 delivered 2026-10-13 - footage browsing and playback

Third commission of the V1 control UI. The plan numbers these U3 items **[54]-[56]**, but those
numbers were spent on crash safety above, so they are recorded here as **[64]-[66]** in plan order.

- [x] **[64]** (plan [54]) `GET /api/footage`, answering in three shapes: the cameras that have
      footage, one camera's dates, and one date's segments with its gaps. The whole index is parsed
      from the filenames the recorder already writes - no database, and nothing opens a segment.
      A segment's duration is its last-write time minus the start in its name, because ffmpeg stops
      writing when the segment ends; checked against `ffprobe` on three real 15-minute segments
      (900.047 vs 900.050, 900.018 vs 900.018, 900.047 vs 900.050 s). That is what makes gaps
      visible: a segment the camera cut short ends early, and the silence until the next one starts
      is the gap, with the leading and trailing silence of the day reported the same way. Each
      segment is placed in absolute time by its OWN `%z` offset, so the hour that repeats on
      25 October is one continuous recording rather than an overlap and that day is 25 hours long.
- [x] **[65]** (plan [55]) `GET /api/clip`, streaming one segment as fragmented MP4 - video copied,
      only the 8 kHz G.711 audio transcoded. Nothing is buffered; the first bytes are read before
      the status is committed, so a failed remux is an error rather than a 200 with an empty video,
      and the request's own context kills ffmpeg when the client goes away. Measured: 176,845,326
      bytes in 8.6 s for a 15-minute segment, `h264` + `aac` under ffprobe, and the remux gone
      0.42 s after an aborted request.
- [x] **[66]** (plan [56]) The footage screen from the approved mockup: camera list, date list,
      segment list, player and the empty state. The grid collapses to one column at 880 px and the
      pickers are capped below the 760 px phone breakpoint so the player stays reachable, with every
      row a 44 px tap target. The tab is driven by the URL fragment, so `#foot` opens the footage
      browser directly - a headless browser has no way to click a tab, so without it the screen
      could not be verified by rendering.

### The in-progress segment: served, not hidden

The newest file of every recording camera is open in ffmpeg right now. It is listed, flagged
`inProgress`, and played like any other - measured safe rather than assumed: a remux of the live
`Front_Entrance_20261013_193001+0300.mkv` returned HTTP 200 with `h264` + `aac` and 344 s of the
340 s that existed when the request started, ffmpeg exited 0 with only `File ended prematurely` on
stderr, and the recorder was untouched throughout - 6/6 recording, the same file growing
62,652,416 -> 63,700,992 -> 67,108,864 bytes across the request and the twenty seconds after it.
Excluding it would have hidden the footage the Operator is most likely to want.

### Two defects found by running it, not by reading it

**Two segments were flagged as recording at once.** A 90 s write window alone marks the segment that
rolled over thirty seconds ago as still being written. ffmpeg writes exactly one file per camera at
a time and it is always the newest by start time, so the question is only ever asked of that one.

**The empty state talked about a date that was not selected.** A camera with no footage at all has
no date, and "No footage for Front Entrance on this date" reads as a bug. Only a render of an empty
archive showed it.

### The screenshot instrument had to be rebuilt

`--window-size=390` renders at 500 CSS px and crops - re-confirmed here against `--headless`,
`--headless=new` and `--headless=old`, all three reporting `innerWidth=500`. The documented iframe
workaround did NOT work for this page: the subframe's three sequential fetches never completed
before the capture, under a 30 s budget, a 120 s budget, no budget at all, and with the host's load
event held open. Every one of those produced a page with its data regions empty, which looks exactly
like a broken screen. The capture is now driven over the DevTools protocol instead -
`Emulation.setDeviceMetricsOverride` gives a real 390 px viewport with no clamp, and the wait is
explicit rather than a virtual-time budget racing the page. The page reported `innerWidth=390`.

---

## The window close now tells him - item [67], 2026-10-13

- [x] **[67]** DONE 2026-10-13. Closing the NVR window raises one alert on all three channels and
      **nothing auto-restarts** - the Operator's explicit decision. The close stays deliberate and
      absolute; he just finds out it happened.

      The gap it closes was measured the same day. At 19:00:36 something closed the NVR window, the
      warden did exactly what it is designed to do, and the machine sat with nothing recording and
      no dashboard for 66 seconds until a human noticed. The `IPCCTV-Recorder` task had already run
      to completion when it opened the window, so its restart-on-failure policy had nothing to
      restart and the only automatic recovery was the next logon.

      `Start-CamJobWarden` gained `-AlertOnWindowClose`, passed only by `Start-CamNvrWarden`: the
      viewer grid closing costs nothing the Operator cannot see for himself, and alerting on it
      would be the noise that teaches him to ignore the alerts that matter. The alert goes after
      the job handle closes, never before - anything that runs there leaves the job's members
      orphaned for as long as it takes - and before the viewers are stopped, because that call goes
      through `cam.ps1` and takes twenty seconds.

      **Event id 103, not another 100.** 100 is "the supervisor stopped unexpectedly" and the log is
      where he reads back what happened; a close and a crash need different answers from him. The
      wording says the window was closed rather than that something stopped unexpectedly, because
      nothing malfunctioned, and the entry is a Warning where the crash is an Error. `status.json`
      gets `StopKind` `window-closed` and a `LastWindowClose` record with its timestamp, written by
      the warden because the supervisor took `CTRL_CLOSE_EVENT` with the window and never got to
      write anything. `ui/` was not touched: `supervisor.go` already reports any non-requested
      StopKind and `app.js` already renders it.

      **`cam.ps1 -Stop` still raises nothing, and it is structural rather than a condition to get
      right.** `Stop-CamNvr` terminates the warden while the window is still open, so it never
      reaches the window-gone branch at all. Measured across an orderly stop: 37 event entries
      before and 37 after, 18 `ALERT` lines in `recorder.log` before and 18 after, and
      `StopKind` `requested` with no `LastWindowClose`.

      Verified live on the Operator's own NVR, twice. The toast rendered (screenshotted), the
      id-103 entry landed 1.4s after the close, `status.json` recorded
      `2026-10-13T20:34:24.98+03:00`, and across +15s, +30s, +60s, +90s and +120s after a second
      close nothing came back: 0 ffmpeg, 0 supervisor, 0 mediamtx, 0 host, 0 warden, 0 windows, the
      job released, and no "recording again" alert. It was started by hand and was recording 6/6
      twenty seconds later. The new camera-free `windowclose` check in the harness covers both the
      close and the terminate-the-warden case that must stay silent.


---

## U4 delivered 2026-10-13 - the UI installs like everything else, and the README covers it

Last commission of the V1 control UI. The plan numbers these U4 items **[57]-[58]**, but those numbers
were spent on the camera-down alert and the event source above, so they are recorded here as
**[68]-[69]** in plan order. **V1 is complete.**

- [x] **[68]** (plan [57]) `cam.ps1 -InstallUi` / `-RemoveUi`, on the launcher-stub and logon-task
      pattern the recorder and the dashboard already use. The stub is NOT `New-CamLauncherStub`:
      camui is not a script to forward to but a long-lived console program, so the launcher starts it
      in a console of its OWN with no window (`CreateNoWindow` with `UseShellExecute` false). A
      console created WITH a window is adopted as a tab in the Operator's Windows Terminal, which is
      the measured failure that killed the supervisor four times on 2026-10-12. camui logs to stdout
      only, so a thin `powershell.exe` holds the pipe and appends it to
      `%LOCALAPPDATA%\IPCCTV\camui.log`, rolled past 10 MB.

      The binary is never copied: the launcher waits for its path exactly as the other two wait for a
      script's, so a rebuilt `camui.exe` takes effect on the next start with no reinstall. Starting a
      second camui would only fail to bind, so the launcher exits without starting anything when the
      address is already served, and exits non-zero when nothing is serving afterwards, which hands
      the retry to the task's restart policy. `-InstallUi` refuses to install when `camui.exe` has not
      been built, and does not start the UI - it prints the one command that does, which is the same
      path logon takes.

      Measured: install, reinstall and remove leave exactly one task and one launcher or none;
      `-RemoveUi` with nothing installed is a clean exit 0 twice over; and `IPCCTV-Recorder`'s and
      `IPCCTV-Dashboard`'s exported task XML is **byte-identical** (same md5) before the install,
      after it and after the removal. A real `camui.config.json` was exercised end to end: the
      launcher passed it, camui bound 127.0.0.1:8788 instead of the default, `-Status` reported the
      new address and named the file it came from, and removing the file put it back on 8787.

- [x] **[69]** (plan [58]) `cam.ps1 -Status` reports the UI - serving or stopped, its URL, the pid
      that owns the port, the binary, where its address came from, whether a token is configured and
      its logon task - and the README now documents the whole system. `cam.ps1` had no command
      reference at all before this. Added: the UI as a feature and a prerequisite note, install steps
      9 to 11, a `cam.ps1` and a `camui.exe` command reference, how the UI works including the
      loopback guard and the exact config that exposes it with a token, the three logon tasks, the
      five event ids with their levels, a "read this first" block at the top of Troubleshooting, and
      limitations 12 and 13 (no seeking ahead of the buffer, and the two harness checks that skip
      permanently on a live NVR). Limitation 7 corrected: the 900 s production segment is no longer
      an unexercised path.

      The UI is identified by the socket it OWNS, never by image name, so `-Status` stays honest when
      something else has the port: it names that pid instead. It reads whether a token is configured,
      never the token.

---

## The README audited against the code, 2026-10-13 - items [70]-[73]

An independent read-only audit walked the README against the code and found 36 issues. These four
items cover the ones that were wrong or dangerous, plus the one functional gap the audit exposed.
Everything here is documentation except [72], which is a code change. Nothing in the running system
was touched: the Operator was using it throughout.

- [x] **[70]** The false security claim and the dangerous `-Run` advice. The features list said the
      password "is never written to a command line, a log or the generated relay config". The first
      third was false: `Get-CamFfmpegArgs` puts `-i rtsp://user:pass@IP/stream1` on ffmpeg's command
      line, `Invoke-CamViewer` does the same for mpv, and `-SelfTest`'s ffprobe probes the cameras
      directly the same way. Only the RELAY path is credential-free on the command line, because
      consumers read `rtsp://127.0.0.1:8554/<path>`. The claim now says which paths are clean and
      which is not, limitation 4 was broadened from "one copy in MediaMTX's environment block" to
      cover the command line as well, and the grid section says a direct pane still has it on mpv's
      own line. **The behaviour is unchanged** - it is the accepted floor, because nothing here has
      an out-of-band credential source.

      The troubleshooting entry for a stale supervisor said to start it with `-Run`. `-Run` is the
      FOREGROUND supervisor, so from an ordinary terminal its console is adopted as a tab in the
      window that already exists and closing that window kills it with no cleanup and orphaned
      children - the failure this same README records happening four times on 2026-10-12. It now
      says `.\cam.ps1 -Start`, or `.\cam.recorder.ps1 -Nvr` for the recorder alone, and the `-Run`
      row in the command reference carries the warning.

- [x] **[71]** Four factual errors corrected. The harness runs **fourteen** camera-free checks, not
      the nine listed - `inputs`, `viewer`, `crashsafe`, `windowclose` and `uileak` were missing -
      and a skip is not a failure, with how many skip depending on the machine. `camui.log` is
      rolled by the LAUNCHER at start and not by the running UI, so a long-lived UI appends past
      10 MB unbounded; `camui.log.prev` is named and `autostart-ui.log` is called out as having no
      rotation at all. "The token itself is never read by the scripts" was wrong in its first half,
      since `Get-CamUiRuntimeState` reads `camui.config.json` and `CAMUI_TOKEN` to answer whether
      one is configured; it never prints it, which is the half that was right. And the event-log
      readback hard-coded `ProviderName = 'Application'`, which returns NOTHING once
      `-InstallEventSource` registers the `IPCCTV` source - it now filters on the `IPCCTV: ` prefix
      every message carries either way, verified live against this machine's Application log.

      The measured rate was quoted as "92 to 100 GB per day" against a `status.json` measuring 47.9
      that evening. A fixed number is how a document starts lying, so it is now an observed range
      with the reason it moves, pointing at `-Status` for the live figure. One self-inflicted error
      caught in the same pass: the replacement sentence named `cam.ps1 -Status` as a place to read
      the rate, and `cam.ps1` prints no disk figures at all.

- [x] **[72]** `camui` resolves its archive root from the recorder's own config. `ui/config.go` had
      `const defaultArchiveRoot = E:\cameras` under a comment claiming it matched `RootPath` in
      `recorder.config.json`, but nothing ever read `RootPath`: the two agreed by coincidence, and
      moving the archive would have left the UI showing an empty archive with no error and no clue
      why. Precedence is now the `-archive-root` flag or `archiveRoot` in `camui.config.json`, then
      `RootPath`, then the constant as a last resort, with every failure to read `RootPath` falling
      through rather than refusing to start. Reading reuses `stripBOM` and `jsonString`, the pair
      the footage code already uses on the same file. The script directory is resolved first,
      because the recorder's config is found relative to it, and the startup log line gained
      `archiveRoot`.

      `TestArchiveRootPrecedence` covers all three sources, a UTF-8 BOM, a missing file, a missing
      key, a blank value and a malformed file; a second test keeps an explicit `-status-file`
      independent of the root. `gofmt`, `go vet` and `go test` clean, and the package cross-compiles
      for windows/amd64. **`camui.exe` was deliberately NOT rebuilt** - the Operator was using the
      running UI - so the fix reaches him at the next UI start, with no reinstall.

- [x] **[73]** Seven gaps filled. Execution policy was never mentioned: it works here only because
      CurrentUser and LocalMachine are both `Bypass`, and with nothing set the effective policy on a
      Windows client is `Restricted`, which runs no `.ps1` at all and fails step 1. `ui/build.sh`
      needs bash and Go 1.27+, which apt's Go is not. A `curl` from inside WSL cannot reach the
      Windows UI - measured, connection refused - and that looks exactly like a UI that is down. The
      `IPCCTV-Dashboard` logon task was promised in the features list with no install step, and now
      shares step 8. Step 4 said to close the dashboard and never said to reopen it. Skipping
      MediaMTX now also says to set `RestreamerEnabled` false, with what it costs if you do not. And
      five real keys were missing from the configuration tables: `RelayReadyWaitSeconds`,
      `RelayReadyPollMs`, `ViewerPromoteCheckSeconds`, `StartStaggerMs` and `ContainerProfiles`.

      One audit claim was **wrong and not acted on as written**: that `ui/build.sh` only runs from
      the repository root. It resolves its own directory from `BASH_SOURCE`, and running it from
      `/tmp` with an explicit output path built cleanly. The real constraint is where the BINARY
      ends up, since `camui.exe` defaults its script directory to its own folder, and that is what
      the README now says.

      Not fixed, deliberately: the remaining audit findings are wording, ordering and completeness
      preferences rather than statements the code contradicts, and item **[29]** stays deferred.
      One code comment is still inaccurate - `Get-CamUiRuntimeState` is prefaced "the token itself
      is not read" - and was left alone because this commission's boundary was documentation plus
      the archive root. It is worth one line in a later pass.

## [74] SIMPLIFY - the Operator's feedback, 2026-10-13 - [74], [76] and [77] done

> "I went through the readme - super complex stuff and commands. Are all these needed? I doubt it,
> we need to simplify"

He is right. The README is 443 lines with eleven install steps, seven Install/Remove command pairs
and about forty config keys. Almost none of it is needed day to day: everything autostarts at logon
and the normal state is that he touches nothing.

The job is to work out what a person actually needs and demote the rest, NOT to delete capability.

- [x] **[74]** Restructured, 478 lines to 314. The document now opens with the running system:
      `http://127.0.0.1:8787/` is on **line 9**, followed by the three screens and the three
      commands anyone actually types - `-Status`, `-Start`, `-Stop`. Then one symptom table that
      folds the old two-part troubleshooting and the alert-id table together, then How it works,
      then a pointer to `docs/install.md`, then the command tables and the limitations as
      Reference. The eleven-step install no longer opens the document, and the Features list is
      gone: it was marketing labels for things How it works already explains properly, and it
      pushed the URL to line 101. Nothing was deleted. Verified token by token and sentence by
      sentence against the previous revision - every backticked identifier still exists somewhere,
      and three facts that condensing had cost were put back: the `--msg-level=all=error` gutter
      explanation, `EventCreate.exe` as the message resource, and the proof that the NVR host does
      restart a killed supervisor. Every hard-won correction survives verbatim: the credential
      claim with its direct-input exception, the `-Run` warning in both places it appeared, the
      fourteen harness checks with the skip count explicitly not fixed, `camui.log` rolling only
      at launcher start, the scripts reading the token but never printing it, and the event-log
      readback filtering on the `IPCCTV: ` prefix rather than a provider.
- [x] **[75]** DONE b1010e8 + cac6bdd (merged locally, not pushed). Collapse the command surface. Seven `-Install*`/`-Remove*` pairs is a lot to carry.
      Consider one `-Install` that does the lot and one `-Uninstall`, with the granular switches kept
      but demoted to a reference section. Do not remove them - scripts and recovery paths use them.
- [x] **[76]** `docs/configuration.md` (77 lines) holds all five configuration tables, and marks
      the four keys the UI's Settings screen can edit - `SegmentSeconds`, `ReserveGB`,
      `PruneHysteresisGB`, `MaxRetentionDays`. The README names those four in Reference and links
      the rest. `docs/install.md` (117 lines) took the prerequisites, the execution-policy note,
      the eleven steps, the `cameras.json` schema and the credential setup. Limitation 8's "marked
      * in the tables above" now points at the configuration page.
- [x] **[77]** Out: the GB/day range, the harness's twelve-passes-two-skips tally, the 170 MB
      clip size, the computed 2250 s stall window, the health strip's 6 px and 13 px edge offsets,
      the 900.02-900.05 s segment measurement, "free space has never fallen to `ReserveGB`", and
      the 11 and 13 second recovery timings. The rule applied: a number that describes the current
      state of a running system goes, and the prose points at `.\cam.recorder.ps1 -Status` or the
      health strip instead; a configuration default stays, because it is an input rather than a
      reading. Retention now says "do not plan from a number in a document" and names where the
      live figure is. One correction carried forward from [71]: `cam.ps1 -Status` prints no disk
      figures, so both places that mention the disk say which `-Status` to use.

Judge the result by a simple test: can someone who has never seen the project get to the UI and
understand what they are looking at within two minutes of opening the README?

## Two UI defects the Operator found by using it, 2026-10-14 - items [78]-[79]

> "the screenshot shows a persisting notification at the top, why is that? Such critical errors must
> be logged, I must be notified accordingly but I need to also be able to close that notification
> once I see it."
>
> "When clicking start, stop etc, it takes a while but the 'loading' animation is not showing that
> well and it seems that it's stuck"

- [x] **[78]** The persistent banner can be dismissed. `status.json` keeps `LastUnexpectedStop` and
      `LastWindowClose` after the recorder is running again, deliberately - an outage must not be
      erased by the recovery that followed it - so the banner stayed for ever with no way to say
      "seen". Each now carries a Dismiss control.

      The acknowledgement is keyed by the **event's own timestamp**, so it acknowledges one event
      and is not a mute switch: a later stop carries a later timestamp, does not match, and appears.
      Proved by constructing exactly that case against a test status file. It is stored server-side
      in `_state\camui-acknowledged.json`, so it holds from any browser and survives a reload, and
      never in `status.json`, which the supervisor rewrites every few seconds and which camui is not
      a writer of. `POST /api/acknowledge` takes a closed list of notice keys; `GET /api/status`
      reports what has been acknowledged. An unreadable sidecar reports nothing acknowledged, so the
      banner shows - the safe direction for a notice about an outage.

      `LastWindowClose` had a record written for it by the warden and nothing that ever rendered it.
      It is now shown and dismissed by the same code path, which is what "every banner of this
      class" required.

- [x] **[79]** A long action now shows its elapsed seconds. Measured here: start everything 52.1s,
      start recorder 27.3s, stop recorder 19.7s, stop everything 17.3s, stop viewers 15.2s, start
      viewers 12.5s. The old indicator was a static toast reading "this takes a few seconds" plus a
      sweep animation, neither of which changes over 52 seconds.

      The seconds tick once a second on the pressed button and in a progress line beside it, which
      also says how long that particular action usually takes, says plainly when it has run longer
      than usual, and says plainly when it has passed the three-minute ceiling `controlTimeout`
      allows. **No progress bar**: nothing can know what fraction of a start is done, and a bar
      stalled at 80% is a worse lie than an honest counter. Every other action control and every
      camera toggle stays disabled for the duration, as before.

      Verified in one browser session driven over DevTools: the counter read 10s, 40s, 60s and 70s
      at those moments, with the over-expectation message appearing past 52.1s.

Deliberately NOT done, and left here rather than built: the camera record toggle still shows a
static "…" while it is in flight. It was left alone because the commission named the six control
actions and supplied measured durations only for those, and a toggle is one config rewrite rather
than a process lifecycle operation. Its real duration has not been measured.

## Four further documentation cuts, 2026-10-14 - item [80]

The [74] restructure listed four things it had *moved* which it believed should have been *deleted*.
The Operator approved all four. Documentation only; no code touched.

- [x] **[80]** All four cut. README 314 to 310 lines, `docs/configuration.md` 77 to 83,
      new `docs/development.md` at 18.

      1. The Supervisor, Relay and Layout tables in `docs/configuration.md` keep every key -
         capability was not being removed - but each section now opens with one line saying these
         are tuning constants and that a wrong value degrades a working system rather than
         configuring it. The Relay and Layout lines scope the warning to the `*` keys, because the
         unstarred ones above them (the paths, the addresses and `HealthStripHeightPx`) are real
         configuration. The suggested shortening of the per-key prose was **not** done, and that is
         a deliberate answer rather than an omission: of the starred keys, all eleven Supervisor
         entries are already key plus default plus one short phrase, and the few that carry a
         second sentence carry a *reason* - `ArchiveScanSeconds` is separate because it touches
         every file, `RelayReadyWaitSeconds` is spent only on a first attempt, `HealthSections`
         errors on an unknown name - which is the only content that helps the rare reader who has
         to change one. The header line fixes what was actually wrong (a reader could not tell
         tuning from configuration) without deleting a why.
      2. Limitation 11, the health strip's screen-edge gap, is now two sentences at the end of the
         health-strip section, keeping every fact: the few pixels of desktop, that top and bottom
         are exact and no video is affected, and that it cannot be fixed while the strip is a
         Windows Terminal window. Limitations 12-14 renumbered to 11-13, and the one cross-
         reference - the harness skip paragraph's "(limitation 13)" - repointed to 12.
      3. and 4. New `docs/development.md` holds the `-File` versus `-Command` quoting trap, which
         is a PowerShell parameter-binding quirk rather than a fact about this NVR, and the two
         dashboard switches `-PrintUrls` and `-NoHealthStrip`, which were the only reference
         entries that tied to no symptom in the troubleshooting table. Both switch descriptions
         moved verbatim; the capability is documented, just not in the README a person opens to
         find out whether their cameras are recording. Linked from Reference so nothing is orphaned.

      Verified mechanically rather than by re-reading, per the method [74] used: all 333 backticked
      spans from the previous revision still resolve in the new file set, 0 of 336 old sentences
      flag on the 0.30 distinctive-word threshold, and a stricter verbatim-sentence diff listed 14
      changed runs - every one either a deliberate cut above or an artifact of where the
      concatenation boundary between files fell. All 15 internal links and anchors resolve, proved
      with a negative control. The six hard-won corrections were each grepped and quoted from the
      final document. `cameras.json` byte-identical; only `.md` files changed.

      Note for later readers: the limitation numbers in items **[32]** and **[34]** above are the
      old ones. [34]'s "known limitation 11" is now the paragraph in the README's health-strip
      section, and [32]'s "limitation 14" is now limitation 13.

- [x] **[81]** `cam.ps1 -Status` resolves the NVR and dashboard windows by **current title**
      (`Get-CamTerminalWindow`), but a Windows Terminal window's title follows its *active tab*. Observed
      2026-10-14: the NVR warden was healthily watching hwnd `0x1190726` while that window was titled
      `nk`, so `-Status` printed `0 window(s) titled 'cam.nvr'` with the host, warden and job all alive.
      Two harms: `-Status` reads as "the NVR window is gone" when nothing is wrong, and the Operator
      cannot identify which window owns the recorder before closing it. The warden already knows the
      HWND - have it publish it to `_state`, and have `-Status` report that HWND's liveness via
      `IsWindow` plus its *current* title, falling back to the title search only when no HWND is
      published. Safety is unaffected either way; this is reporting and identification only.

      FIXED 2026-10-14. `Start-CamJobWarden` now writes `_state\window-<title>.json` - handle,
      terminal pid, the title at latch time, its own pid and the job name - between latching the
      handle and entering the watch loop, and removes it in its `finally`, last of everything, so
      nothing comes between the window going and the kernel sweep. Both tiers get it from the one
      function. Publishing cannot throw into the warden: `Set-CamWindowRecord` swallows its own
      failures and the config resolution is wrapped, because everything below that line is the
      warden's actual duty. The record is removed only by the warden that wrote it, since the file
      is keyed by title and a warden that disarmed must not delete a live one's.

      `Get-CamWindowPresence` is the one reader: the published handle when `IsTerminalWindow` says
      it is still a visible Windows Terminal window, the title search otherwise. **The fallback is
      on liveness, not on the record merely existing** - a record whose handle is dead is treated
      as no record, which is what keeps a restart's few seconds of overlap from reporting a plainly
      open window as absent, and what makes a stale file left by a TERMINATED warden harmless.
      `-Status` prints the handle, the terminal pid, the warden pid and the title the window is
      showing NOW, and says outright when that is not the title we gave it.

      Three of the four other `Get-CamTerminalWindow` call sites moved too, all in
      `Start-CamNvrWindow`, and one of them was a correctness bug rather than a cosmetic one: the
      dead-NVR clearing path *stops processes* on a window count, so a wrongly-zero count skipped
      the clear and left a dead window for the next launch to collide with. The post-clear wait
      re-checks the handle it found before the clear as well as a fresh read, because the dying
      warden takes its record with it. The adopted-tab refusal survives but is no longer reachable
      from an adopted tab - that case is now simply "already open". `cam.ps1`'s health-strip count
      stays on the title search: no warden watches that window, so there is no handle to publish,
      and `Stop-CamHealthStrip` already stops it by process identity for the same reason.
      `Stop-CamTerminalWindow` was reviewed and deliberately left alone - it is a kill path, and
      teaching it to close a window the title search does not find would change kill semantics,
      which this item is not.

      Harness grew a fifteenth check, `windowid`, which drives the real condition in a sandbox
      rather than on the Operator's window: a warden window per tier, a second tab added so the
      title moves to it, then the title search must find nothing while the presence reader still
      finds the window by handle and reports the title it is actually showing. Each tier is then
      checked for the absent case twice - the window closed, and a stale record hand-written for
      the dead handle. Two new testlib primitives, `Add-CamTestForeignTab` and a
      `Stop-CamTestWardenWindow` that closes by handle when the title no longer finds the window.

      Note for whoever reads `-Status` next: the live wardens were started before this change and
      publish nothing, so until the NVR and the dashboard are next restarted both windows are
      reported through the title fallback - correctly, and saying so on the line.

- [ ] **[82]** `Stop-CamTerminalWindow` still resolves its target by **title only**, so when a window's
      title has moved (another tab active - see [81]) it finds nothing and returns 0, which its callers
      read as "closed". Nothing is broken today: both live callers, `Stop-CamDashboard` (~3158) and
      `Stop-CamHealthStrip` (~3791), back the close with a process-identity stop, and `Stop-CamNvr` does
      not use it at all - it stops the warden by inventory identity, which is title-independent. Left out
      of [81] on purpose because teaching it the published handle **changes kill semantics**, and [81] was
      scoped to reporting and identification. Decide whether to close that blind spot.

- [x] **[83]** DROPPED by the Operator 2026-10-14. A date-partitioned layout (`<Cam>\<YYYY-MM-DD>\...`) was proposed so old days could be deleted safely, then withdrawn: it adds no deletion safety once the pruner only touches recording-container files whose names match the recorder's own scheme, and it would cost a midnight failure mode - ffmpeg's segment muxer does not create a missing date folder (verified: "Failed to open segment", exit -2; `-strftime_mkdir` has no effect), so the supervisor would have to pre-create tomorrow's folder or recording dies at midnight - plus changes to seven readers. The archive stays one folder per camera. Safety comes from the [24] hardening instead: container-only allowlist, name pattern, root guard, ownership marker, and an oldest-versus-newest sanity check before any deletion.

- [x] **[84]** DONE 7b5c1e1 (merged, not yet exercised on the live NVR). **REGRESSION in [81], hit in production 2026-10-14 20:03.** An orderly `cam.ps1 -Stop -Recorder`
      followed by `-Start -Recorder` failed three times and left nothing recording for 5 min 34 s
      (last segment closed 20:03:17, next began 20:08:51). Two defects, both in the start path's
      "clear a dead NVR" branch (`Start-CamNvrWindow`, `cam.common.ps1`), the path flagged as "reasoned
      and code-verified, untested at runtime" when [81] merged:
      (a) `Stop-CamNvr` terminates the warden, so the warden's `finally` never runs and its published
      record `_state\window-cam.nvr.json` is left behind. The record names the NVR window by handle,
      and that handle is the Operator's own shared Windows Terminal window (`nk`, 0x1190726, hosting
      his tabs), which is alive. The presence reader only checks that the window exists, so a dead
      warden's record reports the NVR window as PRESENT.
      (b) The wait loop after the clear re-checks the remembered handle with `IsTerminalWindow` and
      expects it to close. A window shared with the Operator's tabs never closes, so the start throws
      "Nothing is recording, but 1 window(s) ... are still there" after 10 s, every time.
      Recovered by hand: stale record backed up and removed, then `-Start -Recorder` succeeded.
      Fix: a record is valid only while the warden it names is alive (and the job exists); `Stop-CamNvr`
      removes the record it killed; the post-clear wait must not wait for a window that is not the NVR's
      own. Prove with a sandbox reproduction of exactly this sequence (shared window, foreign tab active,
      warden terminated) driving the real start-path decision, never the live NVR.

      Fixed: a window record is valid only while the warden it names is alive and started no later than
      the record (`Test-CamWindowRecordLive`); the reader deletes a stale record; `Stop-CamNvr` removes the
      record of the warden it kills; the post-clear wait no longer waits for a window the NVR does not own
      (`Resolve-CamWindowStart`). Reproduced red then green by the harness check `nvrstart`. NOT yet run
      against the live NVR: the next orderly restart is its first production run. `Stop-CamDashboard` still
      leaves `window-cam.dashboard.json` behind - harmless for -Status, not removed.
      OPEN QUESTION: settings.json has `"windowingBehavior": "useNew"`, not `useExisting` as the code comments
      say. How the NVR came to be a tab in window `nk` (0x1190726), and whether the other tabs there are the
      Operator's own, is unverified.

- [x] **[85]** DONE d2348bf (merged locally, not pushed): README 7,128 -> 1,516 words, reference moved verbatim to docs/, 439-sentence nothing-lost check, doclinks check added. README is still a manual, not a front door: 7,128 words after [75] (it was 7,000). The install
      section is good (309 words) but the Operator's complaint was that the whole README is "super complex stuff
      and commands". Heaviest sections: Known limitations 738, The control UI 736 plus a second camui.exe section
      332, Something is wrong 628, The alerts 454, When something stops unexpectedly 418, Autostart 361.
      Plan agreed with the Operator 2026-10-14: split, do not delete. README becomes a front door of about
      1,500-2,000 words (what it is, quick reference, install/uninstall, where to look); the reference material
      moves verbatim into docs/ with links. Nothing lost, proven by a sentence-level check; `docswitches` stays
      green. [75] lesson: a size target given in lines passed at -3 lines while words grew; give word counts.

- **[24] progress, 2026-10-15.** Part A DONE and merged locally (not pushed): log rotation, the segment path
  (scaled 900 s -> 10 s) and the backoff / `HealthyResetSeconds` reset have fired in a sandbox against a
  synthetic camera (`logrotate`, `segments`, `backoff`, `logrotatelive`). Two product bugs found and fixed:
  (1) `Write-CamLog` threw when a reader held a log open at the rotation threshold, which ended the real
  supervisor (exit 1 / 21) and left `status.json` saying `running`; now serialised by a per-file mutex, never
  throws over rotation, retries on the next write, logs one notice per episode. (2) A child that died on its
  own left a 0-byte segment until the next supervisor-initiated stop; the child-exit branch now calls
  `Remove-CamEmptySegment`. STILL OPEN under [24], commissioned next as part B: the 2250 s stall watchdog,
  `MaxRetentionDays` and alerts 121/122 through the real supervisor loop, a logger that never throws even on
  a failed append, `pruneowner` raising a real toast every run, and one soak at production thresholds.
- **[24] part B progress, 2026-10-15** (branch `executor/runtime-b`, not merged). B1 `stallwatch`: the watchdog
  judges directory-listing timestamps only (newest closed LastWriteTime, CreationTime of the open file, child
  start), never a length; a suspended child is killed 15.4 s after its last write at a 15 s limit, a healthy
  one over 21 rolls peaks at 6.0 s. B3 `logheld`: `Write-CamLog` no longer throws when the append fails (it
  killed the supervisor within a second); lost lines are counted in one notice. B2 `pruneloop`: retention and
  alerts 121/122 fire correctly through the real loop. B4: harness alerts stubbed except `windowclose`.
  FOUND, NOT FIXED, to be commissioned: the supervisor loop runs on local `[DateTime]::Now`, so DST moves
  every interval by an hour (fall-back 2026-10-25: timers stall 1 h; spring-forward 2027-03-28: the watchdog
  kills every child and alert 110 fires for every camera). B5 soak not run.

- [x] **[86]** DONE 372f86c, merged locally 2026-10-15 (not pushed; not yet run against the real cameras). **DEADLINE 2026-10-25 04:00 -> 03:00 (this machine, GTB Standard Time; next 2027-03-28 03:00 -> 04:00).**
      DST breaks the supervisor loop because it runs on local `[DateTime]::Now` and local file times, and
      `DateTime` arithmetic ignores the offset. Re-measured 2026-10-15: a real 600 s gap reads +4200 s at
      spring-forward and -3000 s at fall-back; through `ToUniversalTime()` it is 600 s in both. Effects (the
      first measured, the rest read from the code): fall-back stalls every interval timer for an hour (status
      writes, reconcile, archive scan, prune), leaving `status.json` stale and the watchdog blind, and a
      restart scheduled in the last 60 s before the change is blocked for an hour; spring-forward makes the
      watchdog kill every healthy child (3600 > 2250) and raises a false alert 110 for every camera.
      Decided with the Operator 2026-10-15: UTC for all interval and elapsed arithmetic through one clock
      seam; `LastWriteTimeUtc`/`CreationTimeUtc` for file times; local time kept for display, segment names
      and `%z`. A watchdog-only fix is WRONG: at fall-back its input scan is frozen by the local status timer
      and it would false-kill healthy children after about 37.5 minutes. Also widen the 3 s status-gap
      assertions in the new runtime checks (one flaked at 3.7 s under load) to a multiple of the status interval.
      Implemented 2026-10-15 on `executor/dst-utc` (not merged): `Get-CamWallClock` returns UTC and every
      interval, age and deadline in `cam.common.ps1`, `cam.recorder.ps1` and `cam.ps1` reads it; file times feed
      arithmetic only as `*Utc`; `Format-CamTimestamp` keeps the local+offset text and `Get-CamDateTimeOrNull`
      reads it as an instant. Harness checks `dst` (T1, red against the old logic at both transitions), `dstloop`
      (T2, the real supervisor through a simulated fall-back and spring-forward and a +/-3600 s clock step) and
      `clocklint` (T3). Status-gap limits are now 3 x interval + 1.5 s. A real transition has not been observed
      live; a step of the UTC clock itself is still not absorbed (docs/limitations.md item 14).

      Verified independently: full harness 40 passed / 0 failed / 1 skipped; grep finds no local-clock arithmetic.
      STILL INFERRED: live behaviour at 04:00 on 2026-10-25 (the machine clock cannot be moved). KNOWN LIMITS, documented:
      a +/-3600 s step of the UTC clock itself (not DST) still disturbs the timers (forward: one false child kill and one
      alert 110; backward: timers frozen until the clock catches up) - a monotonic Stopwatch would absorb it, not built;
      a warden created inside the repeated hour reads 1 h old so -Status loses the window name for that hour (recording
      unaffected). One flaky phase seen once by the agent (dstloop step-3600, Access denied on a test file rename).

- [ ] **[87]** DIRECTION, Operator 2026-10-15, not yet decided: a browser-based monitor in place of mpv panes, which
      would also make the viewer OS independent and make [29] largely moot. His reasoning for the mpv password
      exposure (a pane that falls back to a direct connection puts the camera password on mpv's command line):
      "cannot be fixed in this architecture, but a browser monitor probably can; unless there is nothing more
      robust than mpv; even if mpv had an API that would do". Earlier research already cleared the technical traps
      for MediaMTX WebRTC live view: no B-frames (7 I / 165 P / 0 B, profile Main) and 8 kHz G.711 (RFC 7874 makes
      PCMA mandatory). It is the plan's V3. TO DO: (a) test whether mpv's JSON IPC (`--input-ipc-server`, then
      `loadfile`) keeps the URL off the process command line - an in-architecture fix; (b) interview the Operator
      on the browser direction before any build: scope, which OS, what to keep of the grid and health strip.

- **RULE (Operator, 2026-10-15): the portable NVR NEVER reaches `master`/`main` of this repository.** It is
  developed on the branch `portable-nvr`, started from this repository's master, and when finished it moves to its
  own repository. Its plan lives only on that branch. Do not merge `portable-nvr` into master, do not copy its
  files here, do not push it to this repository's `master`. This repository keeps working exactly as it is.

- **[24] soak DONE 2026-10-15** (merged locally, `-Checks soak`, named only). 33 min at the production defaults: two
  complete 900 s segments (900.018 s, 900.117 s, quarter-hour aligned), `recorder.log` and the camera log rolled at 2 MB
  with nothing lost, 0 restarts, 0 alerts, status gap max 6.7 s, handles flat, ffmpeg flat. ONE OPEN OBSERVATION:
  supervisor memory rose about 1.3 MB/min between minutes 10 and 30 (then levelled), under a camera about 840x noisier
  than a real one. A micro-test showed `Write-CamLog` (old and new) is flat over 45,000 calls, so the cause is unproven
  (a leak elsewhere, or collector drift). Worst case scaled to real log rates is about 2 MB/day. The check reports
  memory but only fails on handles. FOLLOW-UP (cheap, report-only): a 60-minute soak on a QUIET camera.

- [x] **[88]** DONE 2026-10-15 (merged locally, not pushed): the camera password no longer reaches mpv's command line. A
      viewer pane starts mpv `--idle=once` with a random private IPC pipe and hands it the credentialed URL over
      `loadfile`, then unsets the pipe so nothing can reconnect to read the `path` back. A failed hand-over never falls
      back to the command line. Check `viewerpw`: password on 0 of 104 process-listing sweeps, red against the old
      function. Audit of the remaining exposures: [89], and the ffmpeg direct path below.
- [x] **[89]** DONE 2026-10-15 on branch `executor/relay-api-auth` (not merged; design in `relay-api-auth.md`, check `relayapi`). SECURITY, found by the [88] audit and reproduced 2026-10-15: MediaMTX's control API (`api: yes`, no
      `authMethod`, loopback) returns a camera's full `rtsp://user:PASSWORD@...` source from `/v3/config/paths/list` and
      `/v3/config/paths/get/<name>` to ANY local process or Windows account; the routes the product uses
      (`/v3/paths/*`) do not leak it. No `Access-Control-Allow-Origin` is sent, so an ordinary web page cannot read it
      (DNS rebinding untested). Decided with the Operator: fix now, before the portable project: enable API
      authentication with a login the product holds like the camera credential, keep the anonymous RTSP read/publish
      behaviour, and make sure a failing readiness probe can NOT push viewers or recorders onto direct inputs.
- [ ] **[90]** The recorder's ffmpeg children put the credentialed URL on their command line on the DIRECT path
      (relay off, or its path not ready), and `-SelfTest` probes always do. Not fixed; the relay path avoids it.
      Candidate: refuse direct input for recording when the relay is enabled, or accept as the floor.

- **[89] merged locally 2026-10-15** (`executor/relay-api-auth`, alert id 130, login file `relay_api_creds.xml`). The first live
  start after it rewrites `C:\mediamtx\mediamtx.yml` with the auth block and creates the login; not yet run against the
  real cameras. Trade-off, by design: while the relay refuses the product's login, consumers stay on the relay, so a camera
  whose path has no stream is not recorded until the login is fixed (alert 130 says so) instead of falling back to direct.

- **Testing preference (Operator, 2026-10-15): no more stress or endurance tests; feasibility (functionality) tests only.**
  Dropped as a result: the 60-minute quiet-camera soak that was to settle the supervisor memory observation under [24]
  (the observation stays recorded, worst case about 2 MB a day at real log rates), and the quiet-PC UDP rerun of the
  portable project's one-tile spike. A functionality check for something new is still expected.

- [x] **[91]** DONE 2026-10-15 (`executor/camui-guard`, merged; effective once camui is rebuilt and restarted). SECURITY, found 2026-10-15 while preparing portable Stage 1, reproduced against a throwaway copy of the built
      `camui.exe` (non-default port, stub folder): the web app has NO Host, Origin or Sec-Fetch-Site check anywhere and its
      JSON handlers decode the body without checking the content type. Proven: a cross-site POST (`Origin: evil.example`,
      `Sec-Fetch-Site: cross-site`, `Content-Type: text/plain`, which a browser sends without a preflight) to `/api/acknowledge`
      and `/api/control` was accepted past the origin and rejected only by the app's own body validation (HTTP 422, not 403);
      `GET /api/status` with `Host: attacker-rebind.example` returned 200. Consequence while camui runs on 127.0.0.1:8787: a web
      page the Operator visits could make his browser start or stop the recorder (an orderly stop raises no alert by design),
      change cameras or settings, and, via DNS rebinding, read recorded footage. Not proven here: the PUT handlers' content-type
      handling. Decided with the Operator: fix on master first, then merge into the portable branch. Fix: a request guard in camui
      (Host allowlist, cross-site rejection by Origin and Sec-Fetch-Site on `/api/*`, JSON content type on state-changing
      requests, `Cross-Origin-Resource-Policy: same-origin`). Takes effect when the Operator rebuilds `ui/build.sh` and restarts
      camui; the running camui (pid 3576) is the old binary.

      Fix: `ui/guard.go` - Host allowlist on every request (loopback names, the LAN listen host, `allowedHosts`; any IP literal on a
      wildcard LAN bind), Sec-Fetch-Site and Origin checks on `/api/*`, `application/json` on state-changing requests, CORP,
      nosniff and no-store on `/api/*`. Verified independently: the four original probes went from 422/200/200/422 to 403 on a
      throwaway build; `uiguard` (new harness check) and the Go table tests were red on the old binary and green on the new; the
      page and every front-end call (all through one JSON helper) still work. Not proven: a real-browser cross-site POST (headless
      Chrome gated loopback itself), LAN mode with the real binary.

- [x] **[92]** DONE 2026-10-17 on branch `executor/camera-down-threshold` (not merged; checks `downalert`, `downalertrun`). FALSE ALERT 110
      on a healthy camera. `E:\cameras\_logs\recorder.log` 2026-10-17: Rear Balcony raised "a camera has stopped recording" at
      08:45:01 and 09:00:01, each followed 5-6 s later by "is writing again after 15m down"; the footage is intact, one 137 MB segment
      every 15 minutes (`Rear_Balcony_20261017_0830/0845/0900...mkv`). Cause: `CameraDownAlertMinutes` (15) equals `SegmentSeconds`
      (900), so at every boundary a healthy camera is 900 s past its newest progress marker (`Measure-CamCameraDown`) until the
      status tick whose directory scan sees the new file, a few seconds later (the keyframe wait plus the tick); any tick in that
      window alerted. Only Rear Balcony showed it because of its timing offsets.

      Fix: `Get-CamDownAlertSeconds` returns `max(CameraDownAlertMinutes x 60, SegmentSeconds + 2 x ArchiveScanSeconds +
      StatusIntervalSeconds + 30)` - 1055 s (17.6 min) with the defaults - and the status tick uses it; the supervisor logs one line at
      start when the floor raised the value. The configuration is never refused, only raised. A dark camera still alerts at the
      effective limit, 155 s later than before with the defaults. `CameraDownRepeatHours`, segmenting, scanning and the stall
      watchdog are unchanged. Documented in `docs/alerts.md`, `docs/configuration.md`, `docs/troubleshooting.md`.

      Verified: `downalert` (unit table on the product's own functions) was red on the old formula (901 s, 960 s and the whole
      effective-limit table failed) and is green; `downalertrun` (real supervisor, synthetic camera, SegmentSeconds 20, alert minutes
      equal to the segment, 11 s GOP) raised 3 false alerts in 3 segment rolls on the old rule and none on the fix, then exactly one
      alert 57.8 s after the camera went dark (limit 57 s). The scan lag is one status tick, not the archive scan; the
      `ArchiveScanSeconds` term is deliberate extra slack. `Test-CamChildStalled` (2.5 x segment) has no such window at its default.
