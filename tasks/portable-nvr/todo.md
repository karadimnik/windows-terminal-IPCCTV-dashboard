# Portable NVR - checklist

Companion to `plan.md` (design, decisions, the Status board and the gap analysis). Written 2026-10-17 from what is merged and
pushed on `portable-nvr` (head `f90b987`). The original project's list is `tasks/ipcctv-recording/todo.md` on `master`; this
file never edits it. Tick an item only with evidence (a commit, a check run, an Operator confirmation).

Legend: [x] done and merged; [~] done in part; [ ] open; (A) added during the work, not in the first plan; (Op) needs the
Operator's own action or decision.

## Done

- [x] **P1** Stage 0 spikes: WebRTC in Chrome and Edge, hardware decode, G.711 audio, the MediaMTX cross-origin finding (`spikes/`).
- [x] **P2** Stage 1b (original repo, `master`): mpv password off the command line.
- [x] **P3** Stage 1A: dedicated live-view MediaMTX, login, on-demand sub-streams, flag off = byte-identical (`15e4bb3`).
- [x] **P4** Stage 1B: Live tab, same-origin WHEP proxy, session caps, scoped guard exception, `Live.Cameras` in `status.json` (`b8749e3`).
- [x] **P5** Stage 2a: tile diagnostics, `LiveAdvertiseHosts`, LAN `LiveUdpAddress`; Firefox verified on the real system (`87871d0`).
- [x] **P6** Stage 2b: HTTPS listener, local CA with name constraints, token, limiter, `-print-ca`, `-new-token` (`97fd3fe`).
- [x] **P7** Stage 2b.1: HTTPS listener view-only by default (`1e05dd8`).
- [x] **P8** (A) Stage 2c prep: demo `-Lan` / `-Tls`; a real phone confirmed by the Operator, 2026-10-17 (`e15dbb4`).
- [x] **P9** (A) Live grid that fills the window, camera modal, fullscreen (`3fa1979`).
- [x] **P10** (A) Live robustness: hidden-window grace (`liveHiddenGraceSeconds`), stall detection, recovery, diagnostics v3 (`629bb5e`).
- [x] **P11** (A) Fit / Fill toggle (`436b363`).
- [x] **P12** (A) Original repo, todo [92]: alert 110 floor so a healthy camera cannot trip it at a segment boundary
  (`master` `b69d06d`, in `portable-nvr` `f90b987`). Live in the running NVR after its next restart.
- [x] **P13** (A) Docs: grace settings table, Firefox needs a non-loopback media address, phone-test steps (`docs/`).

## Open: finish what the stages above left unproven

- [ ] **P20** One clean run of the FULL harness on `portable-nvr` (checks have been run individually since Stage 1A).
- [ ] **P21** Footage tab on a phone over the LAN: list, play, seek; concurrency limit for `/api/clip` (GAP L6).
- [ ] **P22** Two viewers on the same camera at once, and the camera's two-session limit with a phone app open (L8).
- [ ] **P23** Android and iOS: install the CA, play the tiles; record what the iOS 18 trust list does (L3). (Op, devices)
- [ ] **P24** Real-system numbers: CPU with six real cameras visible and hidden; the real sub-streams' H.264 profile. (Op to run, I read)
- [ ] **P25** A real PC sleep and resume with the wall open; the closest simulation recovered in about 12 s and the wake-up code did
  not improve it, cause not found. (Op present)
- [ ] **P26** Decide: drop the 24-hour tab soak from Stage 1's acceptance (proposed). (Op)
- [ ] **P27** Housekeeping: remove the phone-test firewall rule `IPCCTV camui HTTPS (TCP 8443)`; clean `%TEMP%\cam-tests` and
  `%LOCALAPPDATA%\IPCCTV\demo`. (Op for the rule)
- [ ] **P28** Find out why a `camui` outlived its closed window twice. Not explained.

## Open: fully over the LAN (see plan.md, section B)

- [ ] **P30** (L1) Install and autostart the portable build WITHOUT touching the original (own task names, own launcher folder, created disabled, a reversible switch between the two systems; Operator rule 2026-10-17), `camui` with the TLS config,
  token in an owner-only file, a firewall helper that asks for elevation, `-Status` checks all of it. Needs D10. (Op decision)
- [ ] **P31** (L2) Detect a LAN address change, regenerate the certificate and the advertised host, alert.
- [ ] **P32** (L3) "Connect a device" page with a QR code and per-device steps.
- [~] **P33** (L4) REPLACED by a login page with brute-force protection (commissioned 2026-10-17, branch `portable-nvr-login`); user management, roles, device list and audit trail are built on it later.
- [ ] **P34** (L5, D9) Control from the LAN with a separate credential and re-authentication, or stay view-only. (Op decision)
- [ ] **P35** (L7) Alerts to the phone (webhook or push), shared with OS5.
- [ ] **P36** (L9, D8) Hide camera addresses from LAN devices. (Op decision)

## Open: fully OS independent (see plan.md, section A; designs: `os-independent-hld.md`, `os-independent-lld.md`; specification: `spec/`)

- [ ] **P40** (OS9) Write `status.json` down as a versioned schema and test the PowerShell recorder against it. Prerequisite of Stage 4.
- [ ] **P41** (OS3, OS4, D12) Portable camera-secret design; recorder reads only through the relay. (Op decision)
- [ ] **P42** Stage 4: Go recorder in shadow mode beside the PowerShell one; prune rules, naming and `status.json` unchanged; Go tests on
  Windows and Linux. Shadow run per D5 (revised: not across 25 Oct 2026).
- [ ] **P43** Stage 5: Go supervisor, attached and service modes (D7), with S1 to S6 below as its specification.
- [ ] **P44** (OS5) Alert channels: webhook, push, email, journald and desktop notifications.
- [ ] **P45** (OS6) Install and autostart per OS: systemd, launchd, Windows service or task.
- [ ] **P46** (OS8) `camui` control backend interface so it no longer needs `powershell.exe` and `schtasks.exe`; default archive root from config.
- [ ] **P47** (OS7) The web app carries the health strip's information; mpv becomes optional and Windows-only.
- [ ] **P48** (OS11, OS12, D1, D2, D11) Own repository, name, CI matrix, release builds. At the end.

## Planned, not built: the stray-process guard (specification for P43)

- [ ] **S1** Single-instance lock (OS mutex or lock file plus the heartbeat check).
- [ ] **S2** Continuous census against an expected set, with an alert and a health chip.
- [ ] **S3** Reap policy for provable orphans only, off by default.
- [ ] **S4** Live-session ledger and an OS-level check that no camera session lingers.
- [ ] **S5** Leak assertion in tests and in every agent task (before/after process lists).
- [ ] **S6** Carry S1 to S3 into the Go supervisor.

## Stage 3 (internet)

- [ ] **P50** Security review first, then a private overlay (WireGuard or Tailscale), not port forwarding. Only after Stage 2 has been in use for a while.
