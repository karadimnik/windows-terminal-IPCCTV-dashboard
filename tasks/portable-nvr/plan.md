# A portable NVR, grown beside the current one

Status: DRAFT for the Operator, 2026-10-15. Nothing here is built. Origin: the Operator's direction in
`tasks/ipcctv-recording/todo.md` [87].

## The brief, in the Operator's words

> "We currently have a web app that serves as a configuration page, partly as a dashboard and playback. We
> need to fully empower that, starting with adding the ability to view the streams from the page. Next step
> would be running the recorder through an OS independent platform, then supervisor etc. But slowly ... ensure
> that NOTHING is removed! Everything would work as is, we are forking this solution to create a new one."
> Where to watch from: "my PC to begin with, then other devices on my LAN, then through the internet."

**End goal:** one OS-independent system that records, supervises and shows the cameras, driven from a browser,
reachable from the PC, then the LAN, then (safely) the internet. Every stage ships value on its own, so the
work can stop after any of them.

## Rules that govern every stage

1. **Nothing is removed and nothing existing changes behaviour.** The PowerShell recorder, supervisor, mpv
   grid, health strip, `cam.ps1` and `camui` keep working exactly as today. New behaviour is additive: new
   screens, new endpoints, new binaries, new config keys that default to off. The full harness (40 checks
   today) must stay green at every merge. No stage deletes a file.
2. **Credentials never reach a browser.** No camera URL, password or `rtsp://` string appears in any response
   or page. This is a test (`uileak`), extended with every new endpoint. (Frigate CVE-2026-33469 leaked
   camera credentials to any logged-in viewer.)
3. **Safety rules port one for one**, not "in spirit": the pruner's allowlist, ownership marker, root guard,
   oldest-versus-newest check and age circuit breaker; UTC timing from the first line (no local-time arithmetic);
   a logger that cannot throw; alerts once per episode.
4. **The new system never writes into `E:\cameras` until the Operator switches it there.** It writes to its own
   root and runs in shadow beside the old one first.
5. **Evidence, not assurance.** Each stage has acceptance criteria, a red-then-green test where cheap, a
   rendered check for anything visual, and a gate where the Operator decides. Verified by running, in a
   sandbox with a synthetic camera first (the rig from [24] exists).
6. **Cameras allow two sessions each.** Today the recorder reads through the MediaMTX relay (one session) and
   nothing else may add a third. Viewers in the new system never connect to a camera directly.
7. **Model and budget:** each commission states its model choice (Opus only for delete paths, timing and
   supervision; cheaper tiers for the rest). A weekly usage limit applies: heavy agent runs wait for the reset
   (20 Oct 2026, 01:00 Bucharest).

## What is known (verified) and what is not

Verified earlier on the Operator's cameras: H.264 Main profile, **no B-frames**, **8 kHz G.711 A-law** audio.
WebRTC makes G.711 mandatory, and no B-frames is the other classic trap, so WebRTC playback is viable.
MediaMTX is already in the stack as the relay. Its README lists WebRTC (WHEP) support for H.264 and G.711.

NOT known, and therefore measured in Stage 0 instead of assumed:
- how many simultaneous H.264 tiles this PC's browser decodes smoothly (a 4-thread i5). The sources found give
  mechanisms and failure modes, not a number; hardware decode can silently fall back to the CPU.
- whether each camera exposes a usable low-resolution **sub-stream** (`stream2`), its codec and resolution.
- MediaMTX WebRTC authentication behaviour for LAN use, against its OFFICIAL documentation (the only description
  found was from a third-party fork).
- whether MediaMTX's WebRTC is good enough, or go2rtc (used inside Frigate, WebRTC and MSE) would serve a grid
  better. Default is MediaMTX: it is already installed and already fans out one camera session.

## Stages

### Stage 0 - spikes and decisions (small, no product change)

- **Spike A - one live tile.** Enable WebRTC on a throwaway MediaMTX instance (non-default ports) fed by the
  synthetic camera, then by one real relay path, and watch it in Chrome/Edge/Firefox on this PC. Measure
  glass-to-glass latency, browser CPU, audio.
- **Spike B - six tiles.** Six simultaneous WHEP tiles at the sub-stream and at the main stream. Measure CPU per
  thread and GPU decode use, with hardware decode on and off, in each browser. Decide the tile policy.
- **Spike C - sub-streams.** Read-only `ffprobe` of each camera's second stream, through the relay only so that
  no extra camera session is opened beyond the cap; record resolution, codec, audio.
- **Decide D1 (location), D2 (name), D3 (tile policy)** from the table at the end.
- Acceptance: a short measured report per spike, with numbers; a recommendation; no change to the product.

### Stage 1 - live view in the web app (the first thing the Operator asked for)

Additive to `camui`; the existing Dashboard, Settings and Footage screens are untouched.
- 1.1 **Relay:** a config flag `LiveViewEnabled` (default off) makes `-InstallRestreamer` also generate a
  sub-stream path per camera and enables WebRTC on loopback. With the flag off the generated config is
  byte-identical to today's.
- 1.2 **A "Live" screen** in `camui`. The browser talks only to `camui` (same origin); `camui` proxies WHEP to
  MediaMTX. One auth point, MediaMTX's port is never exposed, no camera URL in any response.
- 1.3 **Tile policy:** the grid uses sub-streams; clicking a tile focuses it on the main stream; audio muted by
  default, unmuted per tile.
- 1.4 **State:** each tile shows what `status.json` already knows (alive, restarts, last error).
- Acceptance: `uileak` extended and green; six tiles render in the PC's browser within the CPU budget set in
  Spike B; rendered checks at desktop and phone widths; a 24-hour tab soak with no growth in memory; with the
  flag off, everything behaves as before; the full harness green.
- **Gate:** the Operator compares it with the mpv grid. Both keep working.

### Stage 1b - the mpv password exposure (independent, small, queued)

Verified 2026-10-15 by experiment: starting mpv idle (`--idle=once --input-ipc-server=<pipe>`) and sending the
URL through the JSON IPC `loadfile` keeps the password off mpv's command line; the current direct-fallback
launch exposes it. The change is one function (`cam.common.ps1`, the mpv launch near line 1043). Caveats: the
pipe's access control was not tested with a second user; relay panes carry no credential and are unaffected.
Needs the usage limit to reset before it can be commissioned.

### Stage 2 - the LAN

- `camui` bound to the LAN address using the token guard it already has (it refuses a non-loopback bind
  without a token); MediaMTX `webrtcAdditionalHosts` set to the LAN address; a phone on the Wi-Fi shows the grid.
- Decide transport security (plain HTTP with a token is sniffable on a LAN): a small TLS reverse proxy, or a
  private overlay network.
- Acceptance: a device without the token gets 401 on every endpoint and every stream; `uileak` re-run against
  the LAN bind; no credential in any response; the firewall rule scoped to the LAN profile.

### Stage 3 - the internet

- No inbound port forwarding as the first answer. Prefer a private overlay (WireGuard or Tailscale) so cameras
  are never publicly reachable. If real public exposure is ever wanted it needs TLS, authentication, rate
  limiting, TURN for WebRTC through NAT, and a security review before it goes live.
- **Gate:** a dedicated security review. Decision deferred until Stage 2 has run for a while.

### Stage 4 - a portable recorder (OS independent)

A new Go recorder, built beside the PowerShell one, ffmpeg still an external dependency.
- **Specification = today's behaviour,** captured by the existing checks: the ffmpeg arguments and segment
  naming unchanged; the prune rules of rule 3; the `status.json` schema unchanged, so `camui` and the strip
  work with either recorder; UTC or monotonic timing; log rotation that never throws; alert ids 100-122.
- **Strangler, not rewrite-and-swap:** the new recorder reads the SAME relay paths (no extra camera session)
  and writes to its own root in **shadow mode** beside the old one for several days. Compare segment counts,
  boundaries, sizes and gaps. Only then may the Operator point `RootPath` at it, and the old one stays
  installable and working.
- **Portability test bed:** this machine has WSL2 Ubuntu, so the Go tests can run on Windows AND Linux here.
- OS differences isolated behind small interfaces: process control (Job Objects versus process groups),
  alert channels (toast and event log versus journal and desktop notifications, plus a generic webhook),
  autostart (Scheduled Task versus systemd and launchd).
- Acceptance: the recorder-facing checks re-expressed as Go tests, green on both OSes; the shadow comparison
  clean for N days (N set by the Operator); the first real prune happens only under the ported guards.

### Stage 5 - a portable supervisor and control plane

- Replaces the host, wardens and windows of the PowerShell stack with one supervisor: crash policy, one place
  to stop everything (`stop` from the UI and the command line), and the Operator's standing requirement kept:
  running it in a foreground terminal and closing that terminal stops everything it owns.
- Acceptance: the fault-injection checks of [24] and [84] re-expressed and green: held logs, stalled child,
  killed supervisor, stop-then-start, clock steps.

### Not planned: retiring the PowerShell stack

"Nothing is removed" stands. If the Operator ever wants the old system retired, that is a separate decision
with its own deprecation period, not a step in this plan.

## Order and what each stage depends on

Stage 0 -> Stage 1 -> (Stage 1b any time) -> Stage 2 -> Stage 4 -> Stage 5, with Stage 3 only after Stage 2 has
been used for a while. Stages 4 and 5 do not need Stages 2 and 3 and could start earlier if the Operator prefers
portability before remote access; the order above follows what he asked for.

## Decisions needed from the Operator

| # | Decision | Recommendation |
|---|---|---|
| D1 | Where the new code lives | Stage 1 inside `ui/` (additive, behind a new screen). Stages 4-5 in a new Go module in this repo (working name `nvr/`) so the harness and docs stay shared. A separate repository only if and when he wants one. |
| D2 | Name of the new system | He chooses; none is needed before Stage 4. |
| D3 | Tile policy | Sub-stream in the grid, main stream on focus, subject to Spike B. |
| D4 | Order of Stages 2-5 | As listed; portability before the LAN is possible. |
| D5 | Shadow-run length for Stage 4 | At least one full retention cycle, and across the 25 Oct clock change if possible. |
| D6 | Keep the PowerShell recorder as a permanent fallback | Yes: it costs nothing and follows "nothing is removed". |

## Sources used for this plan

- MediaMTX project, codec table and WebRTC (WHEP) support: https://github.com/aler9/mediamtx
- WHEP protocol overview: https://http.dev/whep and https://dev.to/dolbyio/what-is-whep-intro-to-webrtc-streaming-part-2-3d99
- go2rtc as an alternative (MSE and WebRTC, used by Frigate): https://hometechops.com/cameras/go2rtc-low-latency-live-view and https://korben.info/en/go2rtc-swiss-army-knife-video-streaming.html
- Hardware H.264 decode in WebRTC and its fallback behaviour: https://bloggeek.me/webrtc-h264-video-codec-hardware-support/
