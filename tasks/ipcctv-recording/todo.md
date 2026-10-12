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
- [ ] **[38]** The six pane commands are 26987 of the 32767 characters Windows allows on a
      command line, because each pane travels as a base64 blob on one `wt.exe` invocation. Seven
      cameras would not fit. The launch now prints the count and throws over 32000 instead of
      silently opening nothing, which is what it did at 42040. A real fix is moving the pane loop
      into `cam.common.ps1` and passing arguments instead of a script.


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