# Portable NVR - Stage 0 report (measured spikes, no product change)

Date 2026-10-15. Branch `portable-nvr-stage0`. Machine: i5-6600K (4C/4T), 32 GB, GTX 1070 Ti + HD 530, Windows, Chrome 155, Edge 155, Firefox 157.0.1 (now 158.0, see finding C). Everything below was measured on THIS PC while the Operator's normal desktop was running.

## status: PARTIAL - stopped by the Operator's order before the last queued reruns

| Part | State |
|---|---|
| Rig (clips, sandbox MediaMTX WebRTC, test page, collector, sampler) | COMPLETE, committed |
| Spike B, Chrome + Edge, 4 layouts x HW decode on/off (16 cells) | COMPLETE, UDP transport, valid |
| Hidden-tab runs, Chrome + Edge | COMPLETE |
| mpv baseline (640x360 and 2560x1440) | COMPLETE |
| Decoder-implementation runs (I2-*, 8 cells, 10 s each) | COMPLETE (implementation valid; their CPU figures are not, they ran over ICE-TCP) |
| Spike A, Chrome + Edge (A-*) | COMPLETE but TRANSPORT-LIMITED: ran over ICE-over-TCP by accident, NOT representative of real UDP viewing (finding A) |
| Spike A rerun over UDP (A2-*) | DID NOT RUN (cut off by the stop; the partial A2 data was discarded) |
| Firefox, any cell | DID NOT RUN: Firefox never connected (finding B). One 20 s attempt is recorded (A-firefox-clean) |
| Spike C (`spike-c.ps1`) | Written, parses, tested against the sandbox only; NOT run against real cameras, as instructed |
| First, failed matrix attempt and first I-* runs | Discarded (cause: finding A); not in the data folder |

## Findings that matter more than the tables

**A. The early Spike A data (A-*) and the capture ("cap") runs used ICE-over-TCP - treat as invalid for UDP viewing.** The rig briefly had the loopback WebRTC TCP listener switched on (I had enabled it to try to get Firefox to connect and did not restart MediaMTX when I reverted it). A Chromium page holding a capture permission (needed to make Chromium expose `decoderImplementation` in getStats) offers real interface addresses instead of mDNS names and then connects ONLY over that TCP candidate; with TCP off, such pages fail ICE (14 of 14 attempts failed, which is how I found it). So A-* (one tile, Chrome/Edge) and I2-* ran over TCP. The 16 Spike B cells and the mpv baseline ran WITHOUT capture and with the TCP listener off, over UDP (MediaMTX log in the sub-stream cells: `remote candidate: prflx/udp/127.0.0.1`). The decoder identification in I2-* does not depend on transport and stands; the latency, first-frame and CPU figures in A-* are labelled transport-limited. The UDP rerun that would replace them (A2) did not run.

**B. Firefox could not connect to the loopback-only sandbox, so there are NO Firefox numbers.** Firefox 158 gathers only the LAN address (this PC's own LAN address) and never offers a loopback candidate, and it will not pair that with MediaMTX's 127.0.0.1 candidate; ICE ends `failed`. Tried without success: `media.peerconnection.ice.loopback`, `...obfuscate_host_addresses=false`, `...default_address_only=false`, `...no_host=false`, `...link_local`, a forced interface name, and a TCP listener on 127.0.0.1. The only remaining fix is a MediaMTX listener bound to the LAN address (or `0.0.0.0`) with `webrtcAdditionalHosts` set; that makes Windows Firewall raise an "allow mediamtx.exe" prompt on the Operator's screen and can create a persistent rule, so I did not do it. This is also a pointer for Stage 2: Firefox will need a LAN-reachable candidate anyway.

**C. My first Firefox launch applied a pending update to the Operator's REAL Firefox install (157.0.1 -> 158.0, 20:33).** What I launched: `C:\Program Files\Mozilla Firefox\firefox.exe -no-remote -profile "%TEMP%\stage0\prof-<run>-<id>" -new-window "http://127.0.0.1:18080/?run=...&tiles=..."`, started through ShellExecute from my measurement script, with a throwaway profile whose `user.js` set media and ICE prefs (and `app.update.auto=false`, which does NOT stop an already staged update being applied at start). The updater window ("Firefox is installing your updates and will start in a few moments") appeared in my test window. I killed the browser tree by pid at the end of the sampling window, roughly 4 s after the install files' last write times (20:33:36-20:33:52); the install is at 158.0 and starts (the coordinator verified `--version`, updater present, no half-applied status; my later Firefox launches ran normally). Effect on his open Firefox (about 50 processes started from 157.0.1): they still run from the old files in memory; until he restarts it, newly spawned content processes or tabs may mismatch the replaced files and misbehave or crash, and Firefox will prompt for a restart (expectation from how Firefox updates work, NOT verified here). How to avoid: before launching an installed browser for tests, check for a staged update (`updates\` and `updated\` folders under the install, `update.status`) and let the Operator apply it first, or test against a separate portable copy of Firefox.

**D. Duration and why it overran.** Work ran from about 20:10 to 22:41 (about 2.5 h of wall time, the browser windows on the Operator's screen for most of it). The matrix itself took 78.5 min (16 browser cells at ~3.7 min, 2 mpv cells, 2 hidden-tab runs, noise floor, 8 impl runs) as planned from the 30 s + 180 s rule. The overrun was in building the rig: the display was asleep (every browser reported the page hidden and screenshots were a stale frame; fixed by holding the display awake for the life of my process and nudging input once, and making the test window TOPMOST), Chromium capture/ICE-TCP confusion (finding A), the Firefox attempts (B), and a first matrix attempt of about 10 min that connected nothing and was discarded.

**E. The numbers were taken on a busy PC.** With no browser at all (rig + my sampler only) the whole machine was at **75%** CPU on average, and 85-100% during every cell. My sampler itself costs ~46% of one core (11% of the machine); the rest is the Operator's own desktop (Claude app, SystemInformer, LibreHardwareMonitor, his Firefox, etc.). So absolute CPU figures include contention and are pessimistic for a quiet PC; comparisons between cells are the safer reading. CPU below is the SUM over all processes of the test browser (or of the six mpv processes), in % of ONE core (400 = the whole machine). GPU figures are the test browser's own engine use; engines were mapped to adapters by the LUID in the counter name (`luid_0x00000000_0x000149B4` = GTX 1070 Ti, `0x000145E4` = HD 530, from `HKLM\SOFTWARE\Microsoft\DirectX\*\AdapterLuid`). The HD 530 showed 0 in every cell: both browsers and mpv used only the GTX 1070 Ti. GPU counters arrive every ~3 s (the wildcard counter read takes ~2.5 s), CPU every ~2-4 s.

## What the completed cells say

* **Decoder in use** (single tile, `getStats().decoderImplementation`): HW decode on -> `ExternalDecoder (D3D11VideoDecoder)`, power-efficient = true, on the **GTX 1070 Ti** video-decode engine (HD 530 video decode 0), identical in Chrome 155 and Edge 155, at 640x360 and 2560x1440. HW decode off (`--disable-accelerated-video-decode`) -> `FFmpeg` (software), VideoDecode engine 0.
* **Six 2560x1440 mains, HW decode on: works.** Chrome 65% / Edge 67% of one core, NVDEC 39-42%, 20 fps on all tiles, decoder drops 0.01-0.07%, a handful of sub-second freezes in 3 min: verdict smooth, on a PC already at 87-89% load.
* **Six 2560x1440 mains, HW decode off: unusable.** Chrome 150% CPU, 16.5 fps, 14.7% frames dropped, 142 s of freeze across tiles; Edge 186% CPU (peak 525%), 2.8% dropped, 39 s of freeze. The whole machine hit 100%.
* **Six 640x360 sub-streams: smooth either way** (HW on or off): 53-59% of one core in Chrome and Edge, NVDEC 7% when on; software decode of small streams costs almost nothing extra (so HW on/off barely matters). Edge HW-on had a burst of short freezes (47 freezes, 10.7 s over 6 tiles, 10.8% render-dropped) and is marked marginal; its HW-off twin was clean, so I read that as contention noise, not a decoder property, but I did not repeat it.
* **Six 1280x720:** smooth with HW on (60% CPU, NVDEC 20%); with HW off Chrome marginal (91% CPU, 25 decoder drops, 5 freezes), Edge smooth (92% CPU).
* **5 sub + 1 main (3x3, main spans 2x2):** smooth in all four cells: 54-55% CPU HW on (NVDEC 17.5%), 73-75% HW off. Only ~+1 point of CPU against six subs when HW is on.
* **mpv baseline (dashboard flags, software decode, `--ontop` added):** six 640x360 38% CPU, 0 drops; six 2560x1440 **141% CPU**, 0 drops, 3D engine 22% (its own window rendering). So today's grid at six mains costs about twice the browser's CPU with HW decode on, and is cheaper than the browser WITHOUT HW decode (mpv's ffmpeg decode is much more efficient than the browser's software path: 141% vs 150-186% while also dropping nothing). For six sub-streams mpv (38%) beats the browser (53-59%) by about 15-20 points of one core, the browser's JS/page overhead.
* **Hidden / minimized window does NOT stop decode, it throttles it** (6 x 1440p, HW on): after minimizing, `visibilityState` = hidden, decoded rate fell from 20 fps to about 5-6 fps per tile in Chrome (5-10 in Edge), CPU 65% -> 21% (Chrome) and 67% -> 23% (Edge), NVDEC 31% -> 14% (Chrome) and 40% -> 18% (Edge). The streams keep being received and decoded at a reduced rate, so a hidden tab still costs CPU, GPU and bandwidth. A product that must save resources for hidden tabs needs to close the peer connections itself on `visibilitychange`.
* **Audio:** with the tile unmuted Chrome and Edge play and report the G.711 A-law tone (`audioLevel` 0.038, `totalAudioEnergy` rising); while muted `audioLevel` stays 0. (Over TCP; no reason to expect a difference over UDP.)
* **Latency (limits stated):** not measured to the millisecond. Components, Chrome/Edge one tile: jitter-buffer delay 140-300 ms (the dominant part), decode 1.5-5 ms/frame, receive-to-render-callback median 243 ms (Chrome) / 212 ms (Edge). A burned-in frame-number barcode decoded in the page and compared with the publisher's start time gave a glass-to-glass median of 0.25 s (Chrome) / 0.20 s (Edge) (p10-p90 0.10-0.27 s). That estimate has an uncertainty of about 0.1-0.5 s because the clip's time zero is only known to within the publisher's start-up (process start to path-ready was 0.55 s), loopback only, over ICE-TCP, with no camera or network in it. Read it as "a few tenths of a second in the browser, loopback", not as a camera-to-glass figure.
* **Time to first frame** 1.2-2.3 s (page start to first rendered frame, including WHEP signalling and up to 2 s of ICE gathering; measured gather 0.15-0.7 s).
* **Timestamp wrap:** `-stream_loop -1 -c copy` of the 30 s clips played through WebRTC and mpv for minutes with no stall or desync at the loop point; no special handling was needed (the burned-in frame counter restarts per loop, the RTP timestamps continue).

## Recommendation

**D3 (tile policy): keep "sub-stream in the grid, main stream on focus", for these reasons from the completed cells.** (1) A grid of mains only works here because HW decode is available and on; with it off, six mains are unusable in both browsers (150-186% CPU, 3-15% dropped), and a phone, a laptop or a browser that falls back to software cannot be assumed to have it. (2) Sub-streams cost the same ~53-59% whether decode is hardware or software, so the policy has no fragile dependency; the focused layout (5 sub + 1 main) was smooth in all four cells at ~54% CPU with HW on. (3) Mains in the grid multiply LAN bandwidth by ~3x (and every future remote viewer's). The Operator can still offer "all mains" as an option on this PC: it was smooth with HW decode on. **Missing evidence before D3 is final:** whether the real cameras' `stream2` exists and how good it is (Spike C, not run - no one has seen a real sub-stream); the real cameras' texture and frame rate (my clips are synthetic, flat colour bars plus grain, 20 fps assumed, so real decode cost may differ); Firefox; any non-loopback viewer (phone, LAN, Wi-Fi); a quieter PC to remove the contention.

**MediaMTX WebRTC versus go2rtc: MediaMTX is adequate on this evidence; do not spike go2rtc yet.** Over UDP on loopback MediaMTX delivered 20 fps to six simultaneous tiles with 0.01-0.07% decoder drops in the HW-on cells, and its own cost was ~7-13% of one core for the rig (one MediaMTX plus six publishers). Nothing measured points at MediaMTX as the weak part; the failures were on the browser side (software decode) or in connectivity (Firefox). A go2rtc spike is worth its cost only if one of these shows up: (a) Firefox or a phone cannot connect once MediaMTX is bound to a LAN address with `webrtcAdditionalHosts` (the likely first real test, Stage 2), (b) the 0.2-0.3 s in-browser latency or the 140-300 ms jitter buffer proves too high for the Operator when measured against real cameras on the LAN, or (c) a codec or container the cameras produce is not deliverable by WebRTC (not the case for the Operator's H.264 Main + G.711 A-law). The deciding experiment would be the same six-tile test through go2rtc WebRTC and MSE from a LAN phone and Firefox.

**WebRTC authentication in the official MediaMTX v1.21.2 docs (read from the `v1.21.2` tag, `docs/2-features/06-authentication.md`, `docs/4-read/03-webrtc.md`, `docs/4-read/07-web-browsers.md`, `internal/servers/webrtc/reader.js`, `http_server.go`).** The WHEP URL is `http://host:8889/<path>/whep` (verified working here as `/<path>/whep` on 18889). WHEP read is the action `read` with protocol `webrtc`, checked against the SAME `authInternalUsers` (or HTTP/JWT method) as every other protocol. Credentials arrive as an `Authorization: Basic base64(user:pass)` header, or `Authorization: Bearer user:pass` / a JWT; the bundled `reader.js` takes `user`/`pass`/`token` options and sends exactly that; the built-in iframe page cannot be given credentials (the browser asks the viewer), and the server answers an unauthenticated request with `401 WWW-Authenticate: Basic realm="mediamtx"`. **How the product's new control-API login relates:** the product's template already has `user: any` (empty password, `ips: []` = any address) with `publish`, `read` and `playback`, and a second, named API user restricted to `127.0.0.1`/`::1` with only `api`. The API login therefore neither grants nor blocks WHEP: WHEP read is anonymous today, exactly as RTSP read is. That is acceptable only while the WebRTC listener is on loopback. Before Stage 2 (LAN) `any`'s `ips` must be limited to loopback, and either a named read-only user (SHA-256 hash in the config, password in DPAPI like the API one) or the planned `camui` proxy that adds the `Authorization` header server-side must carry browser access, so no camera URL or MediaMTX credential reaches a page. Another finding for the config: with `webrtc: yes` MediaMTX refuses to start unless `webrtcIPsFromInterfaces` or `webrtcAdditionalHosts` is non-empty; the loopback sandbox needed `webrtcIPsFromInterfaces: yes` with `webrtcIPsFromInterfacesList: ['Loopback Pseudo-Interface 1']` (so `webrtcAdditionalHosts` stayed empty as briefed).

## Spike C: `tasks/portable-nvr/spikes/spike-c.ps1`

Not run (as instructed). Parses with 0 errors. The real script text was run against the sandbox with only `Get-CamList`/`Get-CamCredential`/`Get-CamRtspUrl` stubbed: it printed resolution, codec/profile, fps, measured kbps, B-frame yes/no (correctly False on the Main clip and True on a High profile clip with B-frames I generated) and the audio codec, and nothing else. Operator runs it with the recorder, relay and dashboard stopped: `powershell -NoProfile -ExecutionPolicy Bypass -File "<portable checkout>\tasks\portable-nvr\spikes\spike-c.ps1"` (or the same path from a local checkout). It dot-sources `cam.common.ps1` from the ORIGINAL repository (read only), opens one camera session at a time (stream1 then stream2 per camera, 15 s hard timeout), discards ffprobe's stderr, never prints the URL or credential. Documented residual (also in the script header): the credentialed URL is on ffprobe's command line for the duration of each probe (about 4-15 s per stream).

## Method, assumptions and limits

* Clips: libx264 Main, `-bf 0 -g 40`, 20 fps assumed (Spike C reports the real value), pcm_alaw 8 kHz mono; 2560x1440 at 1.5 Mbps, 1280x720 at 1.0 Mbps, 640x360 at 0.5 Mbps (measured 1.56 / 1.09 / 0.58 Mbps including 64 kbps audio). Content is `testsrc2` plus temporal noise: lower texture than a real camera scene, so decode cost may be understated.
* Publishers `ffmpeg -re -stream_loop -1 -c copy -f rtsp` into a sandbox MediaMTX v1.21.2 (WHEP 127.0.0.1:18889, UDP 18189, RTSP 18554, API 18997, collector 18080; started from a local temp folder; the Operator's 8554/9997 untouched).
* Cells: Chrome/Edge launched with a throwaway `--user-data-dir`, window forced to 0,0,1920,1080, TOPMOST, display held awake. 30 s warm-up then 180 s sampled. Test-browser processes identified by the unique profile path / descendants of the launched root pid, never by image name; killed by pid. Page stats posted every 2 s.
* Verdict rule (mine, applied by `analyze.py`): smooth = every tile >= 19 fps decoded, <= 1% frames dropped by the decoder, <= 2% dropped at render, worst tile frozen <= 0.5% of the window; marginal = >= 15 fps, <= 5% dropped, freezes <= 3%; otherwise unusable. "Freeze" is Chrome's definition (inter-frame gap far above the average), so scheduling or relay hiccups count, not only decoder faults.
* Single run per cell, no repeats: differences of a few points between cells are inside the noise (finding E).
* Not measured: Firefox; any LAN/Wi-Fi/phone viewer; real cameras; real relay path; tab memory over 24 h; the rig's `webrtcAdditionalHosts` behaviour.

## Evidence and files

Scripts and data beside this file: `common.ps1 sampler.ps1 measure.ps1 pub.ps1 rig-up.ps1 rig-down.ps1 collector.ps1 make-clips.ps1 run-spike-a.ps1 run-matrix.ps1 run-extra.ps1 mpv-baseline.ps1 spike-c.ps1 analyze.py make-tables.py page/index.html`, `stage0-tables.md` (all tables, generated), `data/` (per cell: `.meta.json`, `.summary.json`, gzipped raw page stats, CPU samples and GPU samples). `run-extra.ps1` was only partly executed (its I2 part ran, its A2 part did not).

Cleanup was verified after the stop (listing at the end of the final message): no process tied to the runtime folder, ports free, throwaway profiles removed, the Operator's three tasks `Ready`.


# Tables (all cells with data)

Spike A rows are over ICE-TCP (finding A); Spike B, hidden-tab and mpv rows are over UDP. "HW decode" column shows the decoder implementation from the I2 runs. CPU % is of ONE core summed over the browser's processes.

## Spike B - six tiles, 3 min sampled after 30 s warm-up (CPU is the sum over ALL processes of the test browser; GPU is the test browser's own engine use)

### (ii) six 640x360 sub-stream stand-ins

| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| chrome | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 53.4 / 129.7 | 19.9 / 45.8 | 6.6 | 0.9 | 742 | 20.0 | 2 (0.01%) | 234 (1.1%) | 0 (0.0 s) | smooth |
| chrome | off (FFmpeg) | 59.3 / 206.2 | 20.6 / 108 | 0.0 | 2.7 | 697 | 20.0 | 6 (0.03%) | 67 (0.3%) | 0 (0.0 s) | smooth |
| edge | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 54.7 / 143.4 | 19.8 / 48.3 | 6.7 | 1.1 | 617 | 20.0 | 0 (0.00%) | 2358 (10.8%) | 47 (10.7 s) | marginal |
| edge | off (FFmpeg) | 52.9 / 150.5 | 16.4 / 36.1 | 0.0 | 2.6 | 550 | 20.0 | 0 (0.00%) | 69 (0.3%) | 2 (0.5 s) | smooth |
| mpv (dashboard flags) | software, hwdec=no | 38.3 / 92.6 | 4.7 / 13.7 | 0.0 | 8.5 | 443 | - | 0 (VO+decoder) | - | - | smooth |

### (i) six 2560x1440 mains

| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| chrome | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 64.7 / 143.5 | 25.3 / 54.4 | 41.9 | 0.6 | 772 | 20.0 | 15 (0.07%) | 229 (1.0%) | 2 (0.6 s) | smooth |
| chrome | off (FFmpeg) | 150.1 / 255.8 | 34.5 / 58.5 | 0.0 | 21.9 | 1542 | 16.5 | 3566 (14.71%) | 1611 (7.8%) | 350 (142.2 s) | unusable |
| edge | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 67.4 / 178.1 | 26.9 / 72.7 | 38.9 | 0.6 | 654 | 20.0 | 3 (0.01%) | 281 (1.3%) | 1 (0.2 s) | smooth |
| edge | off (FFmpeg) | 185.6 / 525.2 | 54.6 / 161 | 0.0 | 24.0 | 1090 | 18.6 | 648 (2.78%) | 1026 (4.5%) | 102 (38.8 s) | unusable |
| mpv (dashboard flags) | software, hwdec=no | 140.6 / 378.3 | 18.1 / 57.2 | 0.0 | 21.7 | 660 | - | 0 (VO+decoder) | - | - | smooth |

### (iii) six 1280x720

| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| chrome | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 60.5 / 139.2 | 23.1 / 48 | 19.9 | 2.0 | 738 | 20.0 | 0 (0.00%) | 258 (1.1%) | 1 (0.2 s) | smooth |
| chrome | off (FFmpeg) | 90.9 / 175.3 | 26.9 / 52.6 | 0.0 | 6.6 | 1762 | 19.9 | 25 (0.11%) | 145 (0.7%) | 5 (1.2 s) | marginal |
| edge | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 60.2 / 121 | 23.9 / 42.7 | 19.5 | 1.6 | 621 | 20.0 | 8 (0.04%) | 160 (0.7%) | 0 (0.0 s) | smooth |
| edge | off (FFmpeg) | 92.4 / 198.9 | 26.7 / 63.1 | 0.0 | 6.1 | 633 | 20.0 | 3 (0.01%) | 19 (0.1%) | 0 (0.0 s) | smooth |

### (iv) 5 x 640x360 + 1 x 2560x1440 focused (3x3, main spans 2x2)

| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| chrome | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 54.1 / 112.8 | 20.4 / 47.5 | 17.5 | 1.8 | 733 | 20.0 | 5 (0.02%) | 151 (0.7%) | 0 (0.0 s) | smooth |
| chrome | off (FFmpeg) | 74.9 / 225.1 | 18.9 / 42.7 | 0.0 | 6.1 | 744 | 20.0 | 6 (0.03%) | 78 (0.3%) | 0 (0.0 s) | smooth |
| edge | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 54.7 / 96.2 | 20.1 / 34.5 | 17.5 | 1.7 | 614 | 20.0 | 4 (0.02%) | 158 (0.7%) | 0 (0.0 s) | smooth |
| edge | off (FFmpeg) | 72.9 / 144.1 | 15.8 / 34.7 | 0.0 | 6.3 | 639 | 19.9 | 12 (0.05%) | 64 (0.3%) | 0 (0.0 s) | smooth |

Noise floor (the sampler process alone, no browser; rig = MediaMTX + publishers running): sampler CPU 45.9% of a core, rig CPU 6.3%, WHOLE-MACHINE CPU 74.9%.

## Whole-machine CPU during each cell (%, 4 logical CPUs = 100)

| cell | machine CPU % | rig CPU % of 1 core |
|---|---|---|
| focus-chrome-hwoff | 92.7 | 8.5 |
| focus-chrome-hwon | 87.7 | 8.7 |
| focus-edge-hwoff | 93.1 | 8.6 |
| focus-edge-hwon | 89.0 | 8.9 |
| hd6-chrome-hwoff | 96.6 | 10.5 |
| hd6-chrome-hwon | 89.8 | 10.5 |
| hd6-edge-hwoff | 93.3 | 11.7 |
| hd6-edge-hwon | 86.8 | 10.9 |
| idle | 74.9 | 6.3 |
| main6-chrome-hwoff | 100.0 | 10.6 |
| main6-chrome-hwon | 89.0 | 13.2 |
| main6-edge-hwoff | 100.0 | 11.7 |
| main6-edge-hwon | 87.4 | 13.6 |
| main6-mpv | 100.0 | 12.8 |
| sub6-chrome-hwoff | 85.8 | 8.0 |
| sub6-chrome-hwon | 86.2 | 8.3 |
| sub6-edge-hwoff | 87.3 | 7.4 |
| sub6-edge-hwon | 85.6 | 8.3 |
| sub6-mpv | 83.0 | 6.7 |

## Hidden-tab runs (6 x 1440p, HW decode on): window minimized after 60 s

| browser | phase | CPU % mean | 1070 Ti VideoDecode % | decoded fps per tile | page visibilityState |
|---|---|---|---|---|---|
| chrome | visible | 64.5 | 31.5 | 19.9-20.0 | visible |
| chrome | hidden | 20.6 | 14.5 | 5.2-6.0 | hidden |
| edge | visible | 67.1 | 40.2 | 19.1-19.6 | visible |
| edge | hidden | 23.3 | 17.9 | 4.9-9.9 | hidden |

## Spike A - one 2560x1440 tile

| run | browser | decoder implementation | CPU % mean / max | busiest thread % | 1070 Ti VideoDecode % | 1070 Ti 3D % | WS MB | fps | dropped / freezes | jitter-buffer ms | decode ms/frame | time to first frame ms (incl. ICE gather) | note |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| A-chrome-clean | chrome | ExternalDecoder (D3D11VideoDecoder) | 19.2 / 115.1 | 4.6 | 12.2 | 0.7 | 628 | 20.0 | 0 / 0 | 295.1 | 1.74 | 2119 |  |
| A-chrome-probe | chrome | ExternalDecoder (D3D11VideoDecoder) | 75.6 / 125.7 | 34.7 | 10.1 | 5.5 | 822 | 20.0 | 0 / 0 | 205.6 | 3.47 | 1182 | canvas readback + unmute: CPU/GPU inflated by the probe; barcode latency median 0.246 s (p10 0.174, p90 0.272); receive-to-callback median 243.2 ms; audio energy +0.09573, level 0.038087099826044496 |
| A-edge-clean | edge | ExternalDecoder (D3D11VideoDecoder) | 21.4 / 76.8 | 4.9 | 11.0 | 0.5 | 518 | 20.0 | 0 / 0 | 137.0 | 1.46 | 2279 |  |
| A-edge-probe | edge | ExternalDecoder (D3D11VideoDecoder) | 82.8 / 130.9 | 41.0 | 13.1 | 7.1 | 611 | 20.0 | 0 / 0 | 174.2 | 4.84 | 1532 | canvas readback + unmute: CPU/GPU inflated by the probe; barcode latency median 0.198 s (p10 0.101, p90 0.265); receive-to-callback median 211.5 ms; audio energy +0.08994, level 0.038087099826044496 |
| A-firefox-clean | firefox | None | 41.3 / 152.1 | 12.7 | 0.0 | 0.0 | 779 | 0.0 | 0 / 0 | None | None | None | never connected (ICE failed) |

## Decoder implementation runs (single tile, capture on so getStats exposes it)

| run | implementation | power-efficient | 1070 Ti VideoDecode % | HD 530 VideoDecode % |
|---|---|---|---|---|
| I2-chrome-hwoff-m1 | FFmpeg | False | 0.0 | 0.0 |
| I2-chrome-hwoff-s1 | FFmpeg | False | 0.0 | 0.0 |
| I2-chrome-hwon-m1 | ExternalDecoder (D3D11VideoDecoder) | True | 12.8 | 0.0 |
| I2-chrome-hwon-s1 | ExternalDecoder (D3D11VideoDecoder) | True | 0.5 | 0.0 |
| I2-edge-hwoff-m1 | FFmpeg | False | 0.0 | 0.0 |
| I2-edge-hwoff-s1 | FFmpeg | False | 0.0 | 0.0 |
| I2-edge-hwon-m1 | ExternalDecoder (D3D11VideoDecoder) | True | 12.7 | 0.0 |
| I2-edge-hwon-s1 | ExternalDecoder (D3D11VideoDecoder) | True | 0.5 | 0.0 |
