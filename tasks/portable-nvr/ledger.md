# Commission ledger (portable-nvr)

Every merge into `portable-nvr`, oldest first, from `git log --first-parent --merges`. Each merge commit is the whole task: revert it with `git revert -m 1 <sha>`; the commits inside it stay individually cherry-pickable (`git log <sha>^1..<sha>^2`). A reverted entry is marked `reverted by <sha>` in the Note column, never deleted. Rules: advisor memory `commissions-are-logically-scoped-and-traceable`.

Update this file in the same push as each merge: `git log --first-parent --merges --reverse --date=short --format="%h|%ad|%s" portable-nvr`.

## Merges

| Merge | Date | Task and summary | Note |
|---|---|---|---|
| 946fef4 | 2026-10-14 | Merge executor/recording-feature: 24/7 NVR with audio, relay, pruner, alerting, CLI and control UI |  |
| de1cbb3 | 2026-10-14 | Merge executor/prune-safety: prune only recorder segments inside a root the recorder owns |  |
| 8fe5717 | 2026-10-14 | Merge executor/nvr-start-regression: an orderly stop then start always brings the NVR back |  |
| 52fb73c | 2026-10-14 | Merge executor/prune-alert-breaker: alert when pruning is blocked, circuit breaker on the age rule |  |
| 75d4116 | 2026-10-14 | Merge executor/install-uninstall: cam.ps1 -Install and -Uninstall, one command each |  |
| 1313752 | 2026-10-14 | Merge executor/readme-split: README becomes a front door, reference moves to docs/ |  |
| d109291 | 2026-10-15 | Merge executor/runtime-a: log rotation never stops the recorder, empty-segment cleanup after a crash, four runtime checks |  |
| b6318d9 | 2026-10-15 | Merge executor/runtime-b: stall watchdog, loop retention and alerts exercised; the logger can never throw |  |
| e59c032 | 2026-10-15 | Merge executor/dst-utc: every interval, age and deadline on one UTC clock |  |
| 752527d | 2026-10-15 | Merge master into portable-nvr: start Stage 1 on the current original system |  |
| 26acb81 | 2026-10-15 | Merge master into portable-nvr: the camui request guard ([91]) for Stage 1 to build on |  |
| 15e4bb3 | 2026-10-16 | Merge Stage 1A: a dedicated, login-protected live-view MediaMTX (portable project, not for master) |  |
| b8749e3 | 2026-10-16 | Merge Stage 1B: the Live screen and the same-origin WHEP proxy |  |
| 87871d0 | 2026-10-16 | Merge Stage 2a: tile diagnostics, LiveAdvertiseHosts and a LAN media address |  |
| 97fd3fe | 2026-10-16 | Merge Stage 2b: camui over HTTPS on the LAN with a local CA and a token |  |
| 1e05dd8 | 2026-10-16 | Merge Stage 2b.1: the HTTPS listener is view-only by default |  |
| e15dbb4 | 2026-10-16 | Merge Stage 2c prep: demo -Tls for a phone test with fake cameras |  |
| 3fa1979 | 2026-10-16 | Merge live grid and modal: a screen-filling grid, a camera modal with an instant picture, diagnostics v2 |  |
| 748ae72 | 2026-10-16 | Merge plan: cross-cutting stray-process guard tasks S1 to S6 (not built) |  |
| 629bb5e | 2026-10-17 | Merge live robustness: hidden grace period, stall detection, recovery without a reload |  |
| 436b363 | 2026-10-17 | Merge live grid Fit/Fill toggle |  |
| 6890b24 | 2026-10-17 | Merge docs: live grace settings and Firefox verified |  |
| f90b987 | 2026-10-17 | Merge master into portable-nvr: alert 110 floor (todo [92]); tests lists and the synthetic camera's encoder arguments keep both sides |  |
| d4c73eb | 2026-10-17 | Merge plan: status board, gap analysis, portable todo |  |
| a88b08a | 2026-10-17 | Merge OS-independent design: Holocron (one executable, levelled logging), inventories, high-level and low-level plans |  |
| 54ce1d3 | 2026-10-17 | Merge Holocron Go module skeleton |  |
| 59237bd | 2026-10-17 | Merge login page with brute-force protection: replaces the LAN token |  |
| 0bfacb5 | 2026-10-17 | Merge Holocron logging (logx) and the action contract (cli) |  |
| 29dd4ef | 2026-10-17 | Merge Holocron W0: status.json contract (schema, types, conformance) and golden files from the PowerShell product |  |
| ce57193 | 2026-10-17 | Merge Holocron spikes SP1, SP1b, SP4, SP6, SP7: findings and experiment programs |  |
| 497d38d | 2026-10-17 | Merge LLD spike decisions |  |
| cd0a977 | 2026-10-20 | Merge Holocron W2: clock, config, archive, osx first slice, logx audit follow-ups |  |
| c2edde3 | 2026-10-20 | Merge Holocron W4: status reader, atomic writer, carry-forward, liveness verdict; camui share-delete status read |  |
| 62278b5 | 2026-10-20 | Merge Holocron W6: relay package (both MediaMTX configs byte-identical, env sources, secrets, API client, child specs) |  |
| e433dbe | 2026-10-20 | Merge Holocron W5: ffmpeg package (exact arguments, version and TZ rules, credential-free direct input, redacted child, empty-segment cleanup) |  |
| 15c2ff2 | 2026-10-20 | Merge Holocron hardening: complete owner-only ACLs, userinfo-escaped redaction, config helper export and root percent rule, doc corrections |  |
| aa05daf | 2026-10-20 | Merge Holocron W3: pruner (pure planner, guarded executor, sandbox guard) with PowerShell differential goldens |  |
| d1dee7e | 2026-10-20 | Merge Holocron W7a: child lifecycle manager (backoff, stall, relay-or-direct, down alert, status rows) |  |
| 06d45fb | 2026-10-20 | Merge Holocron W10: alerts (non-blocking dispatcher, log/webhook/push/desktop/event-log channels) |  |
| 7f16cf1 | 2026-10-20 | Merge Holocron W7b: supervisor (loop, relay and live children, status assembly, ordered stop, panic handling) |  |
| 247359f | 2026-10-20 | Merge Holocron spikes SP2 and SP3: console close and process lifetime findings |  |
| f237a52 | 2026-10-20 | Merge Holocron W8: process lifetime (job objects, locked starter, instance lock, child registry, console close) and the host |  |
| a94e307 | 2026-10-20 | Merge Holocron W11: secrets store (env, encrypted file, Windows Credential Manager) and the legacy camera_creds.xml importer |  |
| f0ce424 | 2026-10-20 | Merge Holocron W9a: control API and the single holocron command tree (up, down, status, cam, config, live, logs, log level, alert test, doctor, version) |  |
| 18749a2 | 2026-10-21 | Merge Holocron W9b1: the web app is part of the holocron module (web role, managed by up, control through the API) |  |
| fec055e | 2026-10-21 | Merge Holocron W9c: user, tls, secret, open, prune, archive stats commands and Go-side settings writes |  |
| 3aff33c | 2026-10-21 | Merge Holocron W12: install, uninstall, autostart (logon task, systemd, launchd, Windows service) |  |
| 0ad7f7f | 2026-10-21 | Merge Holocron W12b: install --window visible/hidden (visible console by default), Defender test result |  |
| 22943e7 | 2026-10-21 | Merge Holocron W9d: viewer role and the Admin tab in the web app |  |
| 0e80374 | 2026-10-21 | Merge Holocron W13: shadow rig runbook and holocron archive compare |  |
| ebe5fe5 | 2026-10-21 | Merge Holocron W13b: RelayExternal mode and live size of the open segment |  |
| 55d0dbb | 2026-10-21 | Merge Holocron W14: GitHub Actions CI (Linux, Windows, macOS best effort) |  |
| 8093a96 | 2026-10-21 | Merge Holocron W15: cutover and rollback runbooks |  |
| ccd36bd | 2026-10-21 | Merge Holocron W14b: suite green natively on Windows; CheckRoot follows junctions and 8.3 names |  |
| eeae710 | 2026-10-22 | Merge Holocron W16: Phone tab for viewers, osx test race, external-relay text, doctor port check, per-command help, shadow web recipe |  |
| e17efc6 | 2026-10-22 | Merge Holocron W17: supervisor and web role resolve the live-view secret file the same way |  |
| 6445ed7 | 2026-10-22 | Merge Holocron W18: quickstart, cheat sheet, AI reference with llms.txt and AGENTS.md, README front door, drift guard |  |
| 81edcbc | 2026-10-22 | Merge Holocron W19: doctor is a full health check (recording, connections, live, web, storage, system) with fix lines and JSON |  |
| be8a0ef | 2026-10-22 | Merge Holocron W19b: doctor growth check waits for a burst, a development ffmpeg build is info |  |
| 54bace1 | 2026-10-22 | Merge Holocron W20: Health section in the Admin tab, the same result as holocron doctor |  |

## Direct commits on the integration branch (advisor, one concern each)

| Commit | Date | Summary |
|---|---|---|
| 636b6c0 | 2026-07-18 | Initial commit |
| 0ae3fa2 | 2026-07-18 | feat: initial release of Windows Terminal CCTV dashboard |
| 171c17a | 2026-10-14 | Stop tracking .claude/ - agent working notes are local |
| 4425420 | 2026-10-14 | [83] Date-partitioned layout dropped by the Operator |
| f508870 | 2026-10-14 | [84] Log the start-path regression hit in production on 14 Oct |
| 4e54cd5 | 2026-10-14 | [84] Mark done, record what is still unproven |
| 40a5763 | 2026-10-14 | [75] Mark done; log [85], the README split |
| 193b69a | 2026-10-14 | [85] Mark done |
| 80d1314 | 2026-10-15 | [24] Record part A progress and what is still open |
| 46575e2 | 2026-10-15 | [86] Log the DST deadline and the agreed design |
| abb81d0 | 2026-10-15 | [86] Mark done and record what is still inferred |
| 2029da1 | 2026-10-15 | Todo: [23] done, [29] lowered, log the browser-monitor direction as [87] |
| cc023fc | 2026-10-15 | Plan: a portable NVR grown beside the current one, in stages; log the decisions in [87] |
| 02400d1 | 2026-10-15 | Plan: the project lives on its own branch and then its own repository; Stage 1b first; how the terminal is involved in Stage 5; decision D7 |
| 31b73c0 | 2026-10-15 | portable-nvr stage 0: spike rig (clips, sandbox MediaMTX WebRTC, test page, collector, sampler) |
| 2e63467 | 2026-10-15 | portable-nvr stage 0: Spike A data, spike-c.ps1 (not run), mpv baseline and matrix scripts |
| 7036105 | 2026-10-15 | portable-nvr stage 0: Spike B matrix, mpv baseline, report, tables and raw data |
| a0bad08 | 2026-10-15 | Plan: Stage 0 results, the real cameras' sub-streams, and the WebRTC access-control design; D3 confirmed |
| 312d7b9 | 2026-10-15 | Stage 0: no personal paths or addresses in the report; spike-c.ps1 finds the original repository beside this checkout |
| 91ce192 | 2026-10-15 | Ignore Python bytecode in the portable project |
| 7c6f125 | 2026-10-15 | Plan: Stage 1 uses a dedicated live-view MediaMTX; the shared relay must not speak WebRTC (open to any website, reproduced) |
| 3550a23 | 2026-10-20 | Spike script: no machine-specific scratch path |
| 222cdfd | 2026-10-20 | spikes/sp4: builds on macOS (stub for the held-open and birth probes) |
| ae25c70 | 2026-10-20 | regen-goldens -Verify: the prune goldens belong to prune-diff.ps1 |
| 2ee2f60 | 2026-10-20 | LLD: process-lifetime decisions from SP2 and SP3 |
| 536340c | 2026-10-21 | secrets README: the config package now reads the secrets keys |
| d0027fd | 2026-10-21 | todo: W9d admin area in the web app for the management functions |
| baea8a2 | 2026-10-21 | todo: W9d done, viewer phone-panel follow-up noted |
| 79f5794 | 2026-10-21 | plan: D5 revised to 3 days by the Operator, DST of 25 Oct not spanned |
| c359133 | 2026-10-21 | Rename aux.go and aux.json: AUX is a reserved device name on Windows (git checkout failed on windows-latest) |
| a685e21 | 2026-10-21 | ci: failed tests as an annotation so a failure is readable without a login |
| c01b90a | 2026-10-21 | ci: failed-test annotations in chunks, never fail the step |
| 236e49e | 2026-10-21 | ci: annotate only the output of failed tests, not passing test logs |
| 45e9587 | 2026-10-21 | ci: annotate the output printed before each failing test |
| 6e371a7 | 2026-10-21 | docs: D5 shadow window ends 22 Oct 2026 10:00 Athens |
| 985884e | 2026-10-21 | docs: commission ledger of every merge into portable-nvr |
| 6478ee1 | 2026-10-21 | web: pin the Content-Type of .js and .css whatever the host says |
| bda60ae | 2026-10-21 | web: the owner-only ACL test accepts SYSTEM and Administrators entries |
| baf78b0 | 2026-10-22 | ci: run the real-process and real-ffmpeg step with -race |
| 63a1a2a | 2026-10-22 | docs: ledger gains the W14b, W14c, W15 and W16 entries |
| 9bd63b7 | 2026-10-22 | docs/shadow: result of the first shadow run |
| fae099c | 2026-10-22 | docs: cutover plan with owners, preconditions, stop rules and this machine's rollback |
| eb4ee94 | 2026-10-22 | docs: record of the 22 Oct 2026 cutover and its first-hour findings |
| 7ff18e3 | 2026-10-22 | docs: ledger gains the W17 merge |
| ed57b1a | 2026-10-22 | docs: ledger gains the W18 merge |
| 415feed | 2026-10-22 | docs: ledger gains the W19 merge |
| db7a4a6 | 2026-10-22 | docs: ledger gains the W19b merge |
| 86622c0 | 2026-10-27 | Merge Holocron L1: archive.Layout library and scan cache, flat layout, behaviour unchanged (D20 step 1; revert with `git revert -m 1`) |  |
| 7f55c1f | 2026-10-27 | Merge Holocron L2: every reader reads the spool and the day folders; pruner pinned to the spool view until L3 (D20 step 2) |  |
| 87899aa | 2026-10-27 | Merge Holocron L3: the pruner prunes the spool and the day folders alike, empty day folders removed, doctor check for unseen files (D20 step 3) |  |
| c77c235 | 2026-10-27 | Merge Holocron L4a: mover library (archive.FileDays/UnfileDays), supervisor mover loop, four config keys, doctor backlog check (D20 step 4a) |  |
