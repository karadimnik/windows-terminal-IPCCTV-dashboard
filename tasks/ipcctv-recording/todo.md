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

- [ ] **[11]** `cam.health.ps1` strip renderer.
- [ ] **[12]** Modify `mpv.cam.autolayout.autorestart.ps1` - reserve the strip, launch the named
      health window, keep camera geometry derived from camera count only, reuse `cam.common.ps1`.
- [ ] **[13]** `-InstallAutostart` / `-RemoveAutostart` logon Scheduled Task.

## Phase D - Documentation and handover

- [ ] **[14]** README recording section incl. self-test procedure and all known limitations.
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
- [ ] **[25]** `recorder.config.example.json` lacks `StatusIntervalSeconds`, `ArchiveScanSeconds`,
      `StopTimeoutSeconds`. Document in the Phase D README.

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
- [ ] **[26]** Re-run the camera-dependent C1 criteria after the credential refresh: D2 (one-session
      proof), D3 (relay MKV carries pcm_alaw), D6 (MediaMTX restart with backoff), and the relay-up
      halves of D1 and D4.
- [ ] **[27]** README must document the MediaMTX download and `C:\mediamtx\` placement, plus the
      transition state in [28] below.
- [ ] **[28]** README line: if the dashboard AND recorder are both already on direct inputs, both
      camera slots are taken and MediaMTX can never pull. Restarting either consumer resolves it.
- [ ] **[29]** TECHNICAL DEBT, after the feature is verified, never mid-feature: `cam.recorder.ps1` is
      now 3,228 lines / 140 KB in one file and is a measurable tax on every edit (exact-match patching
      cost the C1 agent turns). Split it in a dedicated commission once the feature is complete and
      green. Do NOT refactor verified code while the feature is still being built.
- [ ] **[30]** Commission deliverables should include a COMMITTED test harness rather than a
      throwaway scratchpad one. Three agents rebuilt equivalent harnesses from scratch; the C1 agent
      rebuilt what A2 had already written. A `tests/` script the next commission extends would have
      saved more turns than any amount of re-scoping.
