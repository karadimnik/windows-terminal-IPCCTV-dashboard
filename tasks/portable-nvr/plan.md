# A portable NVR, grown beside the current one

Status: DRAFT for the Operator, 2026-10-15. Nothing here is built. Origin: the Operator's direction in
`tasks/ipcctv-recording/todo.md` [87].

## WHERE THIS LIVES - the Operator's rule (2026-10-15)

**This project never reaches `master`/`main` of the `windows-terminal-IPCCTV-dashboard` repository.** It is developed
on the branch `portable-nvr`, which starts from that repository's master, and **when it is finished it moves to its
own repository.** This plan exists only on that branch. The original repository keeps working exactly as it is.
A local `pre-push` hook in the original repository refuses any push that names this branch or carries a commit
touching `tasks/portable-nvr/`. Because it is a fork, the new system's copy of `camui` (`ui/`) evolves on this branch
and the original `ui/` on master stays untouched.

**Order agreed with the Operator:** the soak, then Stage 1b (the mpv password fix, done in the ORIGINAL repository on
master) are finished before Stage 0 of this project starts.

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

1. **Nothing is removed and nothing existing changes behaviour.** The PowerShell recorder, supervisor, mpv grid, health
   strip, `cam.ps1` and `camui` on master keep working exactly as today, because this work happens on a separate
   branch and ends in a separate repository. Inside this fork the same discipline applies stage by stage: new behaviour
   is additive (new screens, endpoints, binaries, config keys that default to off), and the full harness inherited
   from master (40 checks today) stays green on the branch. No stage deletes a file.
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
- 1.1 **A dedicated live-view MediaMTX instance, NOT the recording relay** (design change, 2026-10-15, see the finding
  below). The relay that the recorder reads through is not touched and gains no WebRTC. A second instance, started and
  supervised the same way as the relay and only when `LiveViewEnabled` is true (default false), serves WebRTC on
  loopback for the browser. Its paths: `<cam>sub` pulls the camera's `stream2` on demand (`sourceOnDemand`, so the
  camera's second session is held only while someone watches); `<cam>main` pulls the recording relay's anonymous
  loopback RTSP path (NO extra camera session). WebRTC read on it requires a login: no anonymous `any` user, one named
  user whose random secret only the supervisor and `camui` can read, `webrtcAllowOrigins` empty, RTSP/API/HLS/other
  listeners off. With the flag off nothing changes anywhere.
- 1.2 **A "Live" screen** in `camui`. The browser talks only to `camui` (same origin; the Host and Origin guard from [91]
  protects it); `camui` proxies WHEP to the live instance, adding the login server-side. No camera URL, no MediaMTX
  address and no credential in any response.
- 1.3 **Tile policy:** the grid uses sub-streams; clicking a tile focuses it on the main stream; audio muted by
  default, unmuted per tile.
- 1.4 **State:** each tile shows what `status.json` already knows (alive, restarts, last error).
- Acceptance: `uileak` extended and green; six tiles render in the PC's browser within the CPU budget set in
  Spike B; rendered checks at desktop and phone widths; a 24-hour tab soak with no growth in memory; with the
  flag off, everything behaves as before; the full harness green.
- **Status (2026-10-16).** 1.1 done and merged (Stage 1A). 1.2 to 1.4 built on branch `portable-nvr-1b` (not merged):
  the Live tab, the same-origin WHEP proxy with its session caps, `GET /api/live`, one scoped guard exception, `Live.Cameras`
  in `status.json`; design in `live-1b-design.md`, user documentation in `docs/ui.md`. Proven in Chrome and Edge against the
  real supervisor and a synthetic camera: six sub tiles play at 1920x1080 (3 columns) and 390x844 (1 column), a click or
  `#live/<name>` focuses a tile on main, audio is muted until unmuted, hiding the window closes every connection, the camera's
  `stream2` session count follows the viewers, `uileak` covers the live responses and the SDP answer, and a foreign page, a
  cross-site request or a rebinding Host gets no session. **Not yet done:** the 24-hour tab soak, the six-tile CPU figure on
  the real cameras (the synthetic ones are 640x360 only), the real-camera check that H.264 High plays (1A proved High on
  synthetic clips), the full harness, and the Operator's comparison with the mpv grid.
- **Gate:** the Operator compares it with the mpv grid. Both keep working.

### Stage 1b - the mpv password exposure (in the ORIGINAL repository, done BEFORE Stage 0)

Verified 2026-10-15 by experiment: starting mpv idle (`--idle=once --input-ipc-server=<pipe>`) and sending the
URL through the JSON IPC `loadfile` keeps the password off mpv's command line; the current direct-fallback
launch exposes it. The change is one function (`cam.common.ps1`, the mpv launch near line 1043). Caveats: the
pipe's access control was not tested with a second user; relay panes carry no credential and are unaffected.
Commissioned on master of the original repository, ahead of this project, as the Operator asked.

### Stage 2 - the LAN

- `camui` bound to the LAN address using the token guard it already has (it refuses a non-loopback bind
  without a token); MediaMTX `webrtcAdditionalHosts` set to the LAN address; a phone on the Wi-Fi shows the grid.
- Decide transport security (plain HTTP with a token is sniffable on a LAN): a small TLS reverse proxy, or a
  private overlay network.
- Acceptance: a device without the token gets 401 on every endpoint and every stream; `uileak` re-run against
  the LAN bind; no credential in any response; the firewall rule scoped to the LAN profile.
- **Status (2026-10-16), step 2a on branch `portable-nvr-2a` (not merged): the media path and the diagnostics only.**
  A tile that is not playing explains itself (signalling result, ICE states, candidate counts and kinds, "No media after
  15 s") with a Copy diagnostics line. New keys `LiveAdvertiseHosts` (private IPv4 literals, default empty) and a LAN
  `LiveUdpAddress` (`0.0.0.0` or one private address), both off by default: the default live config is byte-identical
  (`livecfg`, golden hash). `livelan` shows Chrome and Edge playing when the answer holds ONLY a non-loopback private
  candidate (on this PC a virtual adapter's, since it had no physical LAN address at test time). Firewall commands are
  documented, none applied. Prompted by the Operator's report that tiles stay on "Connecting" in Firefox: the loopback-only
  hypothesis fits (MediaMTX offers only the loopback host candidate) and was CONFIRMED by the Operator on 2026-10-16: with
  `LiveUdpAddress` 0.0.0.0:8189 and `LiveAdvertiseHosts` set to the LAN address, Firefox plays on the real system.
  Still open for Stage 2: a physical-NIC and phone test on the real system.
- **Status (2026-10-16), step 2b on branch `portable-nvr-2b` (not merged): camui over HTTPS on the LAN.** An optional second
  listener (`tlsListen`, a private IPv4:port, off by default) serves the whole app and the Live screen with a MANDATORY token
  (`tlsToken` / `CAMUI_TLS_TOKEN`, separate from the loopback `token`), a Secure/HttpOnly/SameSite=Strict cookie holding a
  derived value, a per-address failure limiter and https-only origins; the loopback listener is unchanged. A locally generated
  ECDSA P-256 CA with X.509 name constraints (private addresses and .lan-style names only) signs a 390-day server certificate
  that renews itself; keys are owner-only (icacls); `camui.exe -print-ca` and `-new-token` hand over the CA and a token; only the
  CA certificate is served. Proven by Go tests (every route x token state, certificate matrix, limiter) and `livetls`: Chrome and
  Edge, trusting the CA per process only (`--ignore-certificate-errors-spki-list`; no Windows store touched, no firewall rule
  created), play the tiles over HTTPS; without the flag they show the certificate warning. NOT done (2c): a real phone and a second
  PC (the Operator's steps are in docs/ui.md), a physical NIC, the firewall rules (documented, never applied), HSTS/CSP hardening,
  and the Operator's decision whether a LAN device may see camera addresses through /api/cameras.
- **Status (2026-10-16), step 2b.1 on branch `portable-nvr-2b1` (not merged): the HTTPS listener is VIEW-ONLY by default** (`tlsReadOnly`,
  default true). After the token check (401 beats 403) a deny-by-default allow-list gate lets through only the page, the read-only
  API and the Live screen's own WHEP/session calls; start/stop, settings, camera edits, record toggles and acknowledgements get 403
  before any handler. A test fails when a new route is not classified. The front end hides the controls and says so. `false` makes
  the token full control (documented). The loopback listener is unchanged.
- **Status (2026-10-16), Live fit on branch `portable-nvr-livefit` (not merged): the Live tab fills the window and a click opens a modal.**
  Front end only (`live.js`, `style.css`, `index.html`; no Go change). `bestGrid` picks the column count that maximises the 16:9
  tiles (1920x1080: 3x2 of 633x356, no scrolling; 2560x1440: 846x476; 800x1000: 2 columns; 390x844: 1 full-width column), there is a
  Fullscreen button, and a click opens a `<dialog>` with the tile's own stream at once and ONE main session swapped in when it has drawn
  a frame, with no change to the grid (design `live-fit-design.md`, docs `ui.md`). Proven by `livepage` in Chrome and Edge (measured
  rectangles, instant picture, swap to the 1280x720 main, grid untouched, close/Esc/backdrop/deep link/Back, pre-warm leaves no session,
  HD-unavailable path, hidden window, fullscreen calls) and `tests/live/bestgrid.test.js`.
- **Status (2026-10-17), Live robustness A on branch `portable-nvr-liverobust` (not merged): a wall that stays on.** A hidden window or
  another camui tab keeps its streams for a grace period (camui `liveHiddenGraceSeconds`, default 300, 0 = free at once, -1 = never;
  env `CAMUI_LIVE_HIDDEN_GRACE`; `hiddenGraceSeconds` in `GET /api/live`) and frees them after it; a stalled picture (no decoded frames for 8 s) is
  rebuilt; killed servers, restarted cameras and camui, a sleeping page and a hung server recover without a reload with capped retry
  traffic; tiles scrolled out of view are parked; `live-diag/3`. Design `live-robust-design.md`; proven by the new opt-in `liverobust`
  check in Chrome and Edge (docs `ui.md`).
- **Status (2026-10-17), Live Fit / Fill on branch `portable-nvr-fitfill` (not merged): a toggle for the whole screen.** Front end only. Fit (default,
  unchanged: 3x2 of 633x356 at 1920x1080, ~69 % of the height) or Fill (`bestGridFill`: the cells divide the whole area, `object-fit: cover`,
  1920x1080 3x2 of 633x495, ~99 % covered, ~28 % of each picture cropped); persisted in `localStorage` `camui.live.fit`; layout only, no
  new sessions; phone rule falls back to Fit's scrolling column below 180 px per tile; proven by `bestgrid.test.js` and the `livepage` Fit/Fill block (docs `ui.md`).

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

Replaces the host, wardens and windows of the PowerShell stack with one supervisor: crash policy, one place to
stop everything, and the Operator's standing requirement kept.

**How the terminal is involved.** Today the NVR runs as a tab in a Windows Terminal window, and closing that window
stops everything: a Win32 job holds every child, and its only handle is held by a small warden process that lives
OUTSIDE the window, because a process inside a window cannot reliably see its own window close (measured on this
machine: the pane's console survived the window's death). The portable supervisor keeps the behaviour and drops
the Windows Terminal dependency: "terminal" means any console (cmd, PowerShell, bash, a Linux or macOS terminal).
It runs in one of two modes, the same binary:

| Mode | Lifetime | How everything stops | Typical use |
|---|---|---|---|
| **Attached** | Owned by the terminal it was started in | Close the terminal, or Ctrl+C: Windows - the supervisor and its children sit in a job object with kill-on-close, its handle held by a tiny keeper outside the console; Linux and macOS - children in the supervisor's process group, SIGHUP on terminal close, parent-death signal as a backstop | Starting it by hand; the Operator's current habit of closing windows on purpose |
| **Service** | Independent of any terminal | `stop` from the command line or the UI Stop button; the service manager (Scheduled Task, systemd, launchd) restarts it after a crash or reboot | Autostart at logon or boot |

In both modes there is exactly one place that stops everything (`stop`, and the UI's button), and a stopped system
stays stopped unless something is asked to start it: a deliberate close is never "repaired". The keeper design is the
part to prove in a spike, because the Windows lesson was that the obvious in-process approach fails.
Decision **D7** below.
- Acceptance: the fault-injection checks of [24] and [84] re-expressed and green: held logs, stalled child, killed
  supervisor, stop-then-start, clock steps; in attached mode, closing the terminal stops every child within a set
  time on Windows and on Linux, with nothing orphaned; in service mode, a terminal closing changes nothing.

### Not planned: retiring the PowerShell stack

"Nothing is removed" stands. If the Operator ever wants the old system retired, that is a separate decision
with its own deprecation period, not a step in this plan.

## Stage 0 results (2026-10-15) and what they change

Full report: `tasks/portable-nvr/spikes/stage0-report.md` (tables in `stage0-tables.md`, raw data in `spikes/data/`).
Stage 0 is **partial**: the six-tile matrix for Chrome and Edge is complete and valid (UDP); the one-tile numbers ran
over ICE-TCP by accident and are transport-limited; the UDP rerun and Firefox did not run; the real-camera spike was
run by the Operator himself (below).

**Measured on this PC (i5-6600K, GTX 1070 Ti):** with hardware decode on, six 2560x1440 tiles cost 65 to 67% of one core
and use the GTX 1070 Ti's decoder (D3D11), smooth; with it off they are unusable. Six 640x360 tiles cost 53 to 55%
either way. Today's mpv grid (software decode, as the dashboard runs it) costs 141% at six mains and 38% at six
sub-streams. Hidden tabs keep decoding at a reduced rate (20 fps down to about 5), so the page must close its
connections on `visibilitychange`. Absolute CPU is pessimistic (the PC was at 75% before any browser started); the
comparison between cells is the safer reading. mpv was not tried with hardware decode, so "browser cheaper than mpv" is
not established, only "browser with hardware decode is no worse".

**Spike C, run by the Operator on the real cameras (2026-10-15):**

| Camera | stream1 | stream2 (sub) | Notes |
|---|---|---|---|
| Front Entrance | 2560x1440 Main, 15 fps, 1747 kbps | 640x360 Main, 15 fps, 91 kbps | |
| Rear Balcony | 2560x1440 High, 14 fps, 1223 kbps | 640x360 High, 14 fps, 95 kbps | High profile |
| Field Front | 2304x1296 High, 15 fps, 671 kbps | 640x360 High, 15 fps, 116 kbps | High profile |
| Car Garage | 2304x1296 Main, 15 fps, 1178 kbps | 640x360 Main, 15 fps, 182 kbps | |
| Workshop Entrance | 2560x1440 Main, 15 fps, 1458 kbps | 640x360 Main, 15 fps, 130 kbps | |
| Workshop | 2304x1296 High, 16.67 fps, 1701 kbps | **1280x720** High, 16.67 fps, 142 kbps | High profile; larger sub-stream |

All twelve streams: H.264, **no B-frames**, audio `pcm_alaw` 8 kHz mono (on the sub-streams too).

What that settles and what it opens:
- **D3 is confirmed:** the grid uses `stream2`, the main stream only on focus. Every camera has one. The sub-streams are very
  low bit rate (91 to 182 kbps), so expect soft pictures at tile size; that is the price of the cheap grid.
- **Frame rate is 14 to 16.67, not the 20 the Stage 0 clips used.** Stage 0's decode costs are therefore a little
  conservative.
- **OPEN, must be proven in Stage 1: H.264 High over WebRTC.** The Stage 0 clips were Main profile; three of six cameras
  are High. Browsers advertise only certain profile-level-ids for WebRTC H.264. Publish a High-profile clip through the
  same rig and watch it in Chrome and Edge before trusting the grid.
- **Camera sessions: it fits exactly, with no spare.** Each camera allows two. The recorder reads `stream1` through the
  relay (one session); live view reads `stream2` through the relay (the second). While anyone watches live, the phone
  app has none for that camera. Mitigation to build in Stage 1: the sub-stream relay path starts **on demand**
  (`sourceOnDemand`), so the second session is held only while a viewer is connected. The old mpv dashboard on the
  relay shares the recorder's path and adds no session; a direct fallback would be a third and is refused.
- **The recorder keeps using `stream1`** (unchanged).

## Finding that changed Stage 1 (2026-10-15): MediaMTX's WebRTC port is open to any website

Reproduced against a throwaway MediaMTX with WebRTC and the product's anonymous-read user: a CORS preflight from
`Origin: http://evil.example` to `/cam1/whep` returned 204 with `Access-Control-Allow-Origin: http://evil.example` (an
echo; the default `webrtcAllowOrigins` is `*`), a request with a rebinding `Host` and no credentials was not refused
(404 only because that sandbox had no stream), and the built-in player page was served to a rebinding `Host`. MediaMTX has
no Host check, and its internal users cannot be limited by protocol, so `read` allowed for RTSP is `read` allowed for
WebRTC. Putting WebRTC on the shared relay would therefore let a web page the Operator visits negotiate a session and
watch any path the relay serves. Not shown: a whole stream viewed cross-site (the Stage 1A test makes that a
red-then-green test). Hence the dedicated instance: WebRTC read there needs a login the browser never has, and the
recording relay never speaks WebRTC.

## WebRTC access control (design for Stages 1 and 2)

Stage 0 found that MediaMTX's WebRTC read is anonymous today (the relay's `any` user may `read` from any address) and that
the product's control-API login does not govern it. That is acceptable on loopback only. The design:

1. **`camui` is the only door for signaling.** MediaMTX's WebRTC HTTP listener stays bound to `127.0.0.1`. The browser
   talks only to `camui` at `/api/live/<camera>/whep` (same origin as the page); `camui` checks the viewer's token (Stage 2)
   and forwards the request to MediaMTX server-side. The camera name maps to a path through `cameras.json`; an unknown
   name is a 404. `camui` forwards no arbitrary path, no query string and no `Authorization` header, and it never returns
   a MediaMTX address, so the browser has no way to name another stream. The SDP answer carries ICE candidates and
   credentials for that one session and no camera URL.
2. **Media is protected by the signaling.** Video travels browser to MediaMTX directly over ICE/DTLS-SRTP. A peer only gets
   media after presenting the one-time ICE username and password issued in the answer to an authenticated signaling
   request, then completing the DTLS handshake. So for the LAN (Stage 2) the UDP media port is reachable on the LAN
   address (`webrtcAdditionalHosts` set to it), but a device that never passed `camui`'s token cannot receive video.
   Verify in a Stage 2 spike that a STUN probe with a wrong username draws no media and no useful response.
3. **Belt and braces in MediaMTX:** restrict the `any` user's `publish`, `read` and `playback` to loopback
   (`ips: [127.0.0.1, ::1]`) in the generated config. `camui`'s forwarded calls come from loopback; a misbound listener
   would then still refuse a LAN client.
4. **Firewall:** one inbound UDP rule for the media port, on the Private profile only, scoped to the LAN subnet. It is
   created by an explicit install step with a documented removal, never silently, because creating it triggers a Windows
   Firewall prompt.
5. **Tokens:** `camui` already refuses a non-loopback bind without a token. Add constant-time comparison, a per-device token
   later, rate limiting of failed attempts, `Cache-Control: no-store`, and TLS for the LAN stage (a small proxy or a
   private overlay network).
6. **Acceptance for the stage that builds it:** `uileak` extended to every `/api/live/*` response; a device without the token
   gets 401 on every live endpoint; a direct request to MediaMTX's WebRTC HTTP port from the LAN address is refused; no
   response or SDP contains a camera URL or credential; `Authorization` never reaches the browser; the wrong-ufrag probe
   yields no media.

## Cross-cutting: no stray processes (Operator's rule, 2026-10-16) - SPECIFICATION FOR THE PORTABLE SUPERVISOR, NOT BUILT IN POWERSHELL

**The rule.** Nothing may run that should not be running, and the Operator must be told when something does. This
section is the task list only; nothing here is implemented. It was raised after leftovers were found in the session
(stuck agent shells, a test browser left up, a `camui` that outlived its window) and the Operator asked how he would
know if three recorders ran, or if live-view tabs left things behind.

**What exists today (verified in the code, 2026-10-16).**
- One supervisor per archive: a check of the status file's heartbeat and recorded pid, refused at start
  (`cam.recorder.ps1`, "One supervisor per archive"). It is check-then-act, not an OS lock, so it can race in the first
  seconds and is blind if the status file is missing, unreadable or points at another root.
- Kernel-enforced children: the supervisor owns a job object (`cam.supervisor.<hash>`) that holds MediaMTX, the live
  instance and every ffmpeg; the NVR warden owns `cam.nvr`. Killing the supervisor kills them.
- `cam.ps1 -Status` lists the process roles and flags orphaned dashboard panes and mpv; `-Stop` reaps orphans.
- The live-view control API is OFF on purpose: MediaMTX permissions are per action, not per endpoint, and the `api`
  action would also expose the path configuration (the relay-API leak of item [89]).

**What does not exist.** No continuous comparison of the running processes with what should be running; no alert; nothing
on the health strip; no detection of a duplicate ffmpeg for one camera, an extra MediaMTX or `camui`; no count of live
view sessions or their ages. Today he finds out only by running `-Status` and reading the table.

**Operator decision (2026-10-16): S1 to S5 are the specification for the portable (Go) supervisor and are NOT built in the PowerShell
scripts.** Nothing in `cam.recorder.ps1`, `cam.common.ps1` or `cam.ps1` changes for them.

**Decisions.** Alert first; never kill by process name alone (the Operator may run other `ffmpeg` or `mediamtx`
processes); no stress tests (feasibility scenarios, one clean run each, each detector with a negative control that can
fail). The Operator declined a change to his shell aliases (`cp`, `rm`, `mv` are interactive there): agents keep using
the explicit force forms.

| Task | What | Acceptance |
|---|---|---|
| **S1 Hard single-instance lock** | A named OS mutex `Global\IPCCTV.supervisor.<hash of RootPath>` held for the supervisor's life, plus an exclusive lock file in `_state`; an abandoned mutex (previous owner died) is taken, not refused; the heartbeat check stays as the second signal. | Two starts at once: exactly one runs. Kill the supervisor hard, the next start succeeds. Delete or corrupt `status.json`: a second start is still refused. Refusal message names the owner pid. |
| **S2 Continuous census** | The expected set as data: 1 NVR host, 1 warden, 1 supervisor, 1 relay MediaMTX, 1 live MediaMTX when enabled, exactly one ffmpeg per recording camera (matched by its relay path in the command line), at most one `camui`. Identity by pid plus start time and job membership. A violation (extra, duplicate, missing, not in our job) must persist for 2 cycles. Output: `Census` in `status.json`, a `-Status` section, a chip on the health strip, a new alert id (next free after 130) on the event log and toast. | Scenarios, each with a negative control: duplicate supervisor start, a second ffmpeg for one camera, an extra MediaMTX, an extra `camui`, an ffmpeg orphaned by a hard supervisor kill (the job must already have removed it: the census confirms zero), a missing camera child. Normal start, stop and restart raise nothing. |
| **S3 Reap policy** | `cam.ps1 -Clean -WhatIf` lists provable orphans only: parent gone, not in our job, image path equal to the configured executable, command line naming our relay or live path. Reaping needs an explicit setting (default off) or an explicit `-Clean`. | A look-alike process of the Operator's (same image name, different command line) is never listed. |
| **S4 Live-session hygiene** | `camui` keeps a ledger of the sessions it minted with their ages (exposed in `/api/live` as counts and oldest age, no ids); a session older than a limit raises a flag. An independent OS check: with an empty ledger for 60 s, the live MediaMTX must hold no connection to a camera's RTSP port. Optional `LiveIdleMinutes` (default 0 = off) closes a forgotten foreground tab's streams. | Open and close tabs in Chrome and Edge, kill a browser, close the tab mid-connect: the ledger and the OS check return to zero within the measured bounds (about 5 s on tab close, 42 s on a hard kill) and no flag is raised; a deliberately leaked session raises one. |
| **S5 Leak assertion for tests and agents** | `Assert-CamNoStrays` in the harness: snapshot before and after each check, by marker and by image path, and fail on anything extra; sandbox rigs run inside a job object so they die with the test; a final sweep at the end of a `-Checks` run. The commission template asks every agent to list processes before and after and report the diff. | A check that leaves a process behind fails, a clean check passes, and the sweep removes only marked rig processes. |
| **S6 Carry into the Go supervisor (Stage 5)** | S1 to S3 are the specification of the portable supervisor's lock, process group or job, and census, on Windows and Linux. | Re-expressed as Go tests, green on both. |

**Closed (2026-10-16).** The Operator decided against building S1 to S5 in PowerShell; a backport to the original system on `master`
would be a separate commission with his approval.

## Order and what each stage depends on

Stage 0 -> Stage 1 -> (Stage 1b any time) -> Stage 2 -> Stage 4 -> Stage 5, with Stage 3 only after Stage 2 has
been used for a while. Stages 4 and 5 do not need Stages 2 and 3 and could start earlier if the Operator prefers
portability before remote access; the order above follows what he asked for.

## Decisions needed from the Operator

| # | Decision | Recommendation |
|---|---|---|
| D1 | Where the new code lives | **DECIDED by the Operator:** on the branch `portable-nvr`, started from master, never merged into master, moved to its own repository when finished. |
| D2 | Name of the new system | He chooses; none is needed before Stage 4. |
| D3 | Tile policy | **CONFIRMED by Stage 0 and Spike C:** sub-stream (`stream2`) in the grid, main stream on focus; every camera has a usable sub-stream. |
| D4 | Order of Stages 2-5 | As listed; portability before the LAN is possible. |
| D5 | Shadow-run length for Stage 4 | At least one full retention cycle, and across the 25 Oct clock change if possible. |
| D6 | Keep the PowerShell recorder as a permanent fallback | Yes: it costs nothing and follows "nothing is removed". |
| D7 | Autostart in attached or service mode (Stage 5) | Support both; the Operator picks the default at install. Attached keeps today's behaviour (a visible window you close to stop); service is headless and UI-controlled. |

## Sources used for this plan

- MediaMTX project, codec table and WebRTC (WHEP) support: https://github.com/aler9/mediamtx
- WHEP protocol overview: https://http.dev/whep and https://dev.to/dolbyio/what-is-whep-intro-to-webrtc-streaming-part-2-3d99
- go2rtc as an alternative (MSE and WebRTC, used by Frigate): https://hometechops.com/cameras/go2rtc-low-latency-live-view and https://korben.info/en/go2rtc-swiss-army-knife-video-streaming.html
- Hardware H.264 decode in WebRTC and its fallback behaviour: https://bloggeek.me/webrtc-h264-video-codec-hardware-support/
