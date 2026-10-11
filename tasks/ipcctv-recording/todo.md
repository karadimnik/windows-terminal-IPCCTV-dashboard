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

- [ ] **[19]** Measure slot-release time: hold one session, hard-kill a recorder child, count backoff
      cycles before it reconnects. If release is prompt, direct connection needs no restreamer.
- [ ] **[20]** MediaMTX G.711 soak: 1 h, two long-running consumers, audio packet continuity sampled
      over time. Prefer MediaMTX over go2rtc on the documented-bug evidence. Fail = stay direct.
- [ ] **[21]** Decide the connection model on [19] + [20], then implement. Restreamer is a transport
      fan-out ONLY - recording stays in our supervisor, never moved into the restreamer.
- [ ] **[22]** If adopting a restreamer, give the dashboard a direct-connection fallback so a
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
