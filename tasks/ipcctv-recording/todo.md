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

## Phase B - Control, status, retention

- [ ] **[6]** `-Enable` / `-Disable` / `-List`, atomic `cameras.json` writes preserving all fields.
- [ ] **[7]** 5 s reconcile loop - live pickup of `Record` changes without a restart.
- [ ] **[8]** `status.json` writer + `-Status` renderer (per-camera health, disk free, projected days).
- [ ] **[9]** Pruner - free-space watermark with hysteresis, oldest-first, never the in-progress
      file, every deletion logged.
- [ ] **[10]** `-Stop` - graceful shutdown leaving the final segment readable.

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
