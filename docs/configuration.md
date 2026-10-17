# Configuration reference

`recorder.config.json` overlays the built-in defaults; any key you leave out keeps its default, and the file is read by the recorder, the health strip and the dashboard alike. Keys marked * are not in `recorder.config.example.json` and have to be added by hand if you want to change them.

**Four keys are editable from the control UI's Settings screen** - `SegmentSeconds`, `ReserveGB`, `PruneHysteresisGB` and `MaxRetentionDays`, marked **UI** below. Everything else on this page is tuning that nobody should need, and is here for when you do.

Values written by the UI go through the same code the CLI uses: validated, written atomically, and the file keeps its UTF-8 BOM.

## Paths and recording

| Key | Default | Meaning |
| --- | --- | --- |
| `RootPath` | `E:\cameras` | Where footage, `_state` and `_logs` live. |
| `FfmpegPath` / `FfprobePath` | `C:\ffmpeg\bin\...` | The recording tools. |
| `SegmentSeconds` **UI** | `900` | Segment length, aligned to the clock. |
| `Container` | `mkv` | Which entry of `ContainerProfiles` to use. `fmp4` re-encodes audio to AAC. |
| `ContainerProfiles` | `mkv` and `fmp4` | The profiles themselves: per profile an `Extension`, a `SegmentFormat`, `SegmentFormatOptions`, and `AudioArgs` / `ExtraArgs` added to the ffmpeg command line. Add a profile here to add a container; there is no other place that knows about one. |
| `RtspTransport` * | `tcp` | Transport for the recorder's inputs. |
| `TimeoutMicroseconds` * | `10000000` | ffmpeg's socket timeout. |

## Retention

| Key | Default | Meaning |
| --- | --- | --- |
| `ReserveGB` **UI** | `20` | Free space the pruner defends. |
| `PruneHysteresisGB` **UI** | `5` | How much extra it frees once it starts, so it does not thrash. |
| `PruneIntervalSeconds` | `300` | How often it looks. |
| `MaxRetentionDays` **UI** | `null` | Optional age cap, on top of the space rule. |
| `MaxAgePruneFraction` | `0.5` | Circuit breaker on `MaxRetentionDays`. A run that would delete more than this share of the archive by age - by segment count or by bytes - deletes nothing by age, logs one `PRUNE AGE RULE HELD` line with both shares and alerts once (event id 122), so a mistyped retention cannot empty the archive. Lower `MaxRetentionDays` in steps to shorten retention a long way. Must be from `0.01` to `1.0`; `1.0` switches the brake off. Never holds back the free-space rule: a disk below `ReserveGB` keeps pruning. |

## Supervisor

Every key here is a tuning constant rather than a setting: the defaults are the values this system was measured on, and a wrong one will degrade a working supervisor rather than configure it.

| Key | Default | Meaning |
| --- | --- | --- |
| `ReconcileSeconds` * | `5` | How quickly an `-Enable` or an edited IP is picked up. |
| `StatusIntervalSeconds` * | `5` | How often `status.json` is rewritten. |
| `ArchiveScanSeconds` * | `60` | How often the whole archive is measured for the disk figures. Separate because it touches every file. |
| `StopTimeoutSeconds` * | `30` | How long `-Stop` waits for a clean teardown. |
| `BackoffStartSeconds` / `BackoffMaxSeconds` * | `3` / `60` | Restart backoff for a failing child. |
| `HealthyResetSeconds` * | `300` | How long a child must stay up before its backoff resets. |
| `StallFactor` * | `2.5` | Multiplied by `SegmentSeconds` for the stall watchdog. |
| `CameraDownAlertMinutes` * | `15` | How long a camera may make no file progress before it raises an alert (id 110), at least `SegmentSeconds + StatusIntervalSeconds + 30` seconds - 935 s (15.6 min) with the defaults - because a healthy camera is a whole segment past its last progress at every boundary, plus the keyframe wait and a status tick. A lower value is raised to that and logged once at start. |
| `CameraDownRepeatHours` * | `12` | How long before the same camera alerts again while still down. |
| `LogMaxBytes` * | `2 MB` | Log rotation threshold. |
| `LogRepeatSuppressSeconds` * | `60` | Collapses a repeating log line. |

## Relay

The paths and addresses here are configuration; the keys marked * are tuning constants, set from measurement, and a wrong one will degrade a working relay rather than configure it.

| Key | Default | Meaning |
| --- | --- | --- |
| `RestreamerEnabled` | `true` | Set false to run on direct camera inputs only. |
| `RestreamerExePath` | `C:\mediamtx\mediamtx.exe` | The binary. Must be on a local drive. |
| `RestreamerConfigPath` | `C:\mediamtx\mediamtx.yml` | The generated config. Must be on a local NTFS path: MediaMTX watches it with `ReadDirectoryChanges`, which the WSL share does not implement, and exits immediately if it cannot. |
| `RelayBase` | `rtsp://127.0.0.1:8554` | Where consumers read. Loopback only; the generated config allows anonymous access precisely because nothing off the machine can reach it. |
| `RelayApiBase` | `http://127.0.0.1:9997` | The control API the readiness probe uses. |
| `RelayProbeTimeoutMs` | `1500` | Budget for one probe. |
| `RelayStartupWaitSeconds` * | `20` | How long the supervisor waits for the relay's control API to answer before starting any recorder. The API only, not every path: one offline camera's path can never become ready and would otherwise hold up the healthy ones. |
| `RelayReadyWaitSeconds` * | `15` | How long **one** consumer waits for **its own** relay path before giving up and taking a direct camera slot. Spent only on a first attempt at batch startup; a later attempt decides on the spot, so a relay that died is left behind immediately. |
| `RelayReadyPollMs` * | `500` | How often that wait re-asks. Floored at 50. |
| `ViewerPromoteCheckSeconds` * | `10` | How often an `mpv` pane that is on a direct connection asks whether its camera is back on the relay, so it can hand the second session back. One control-API call per direct pane per interval. `0` switches the watch off and leaves such a pane direct until the dashboard is relaunched. |
| `StartStaggerMs` * | `300` | Delay between consecutive consumer starts, recorder children and `mpv` panes alike, so six of them do not burst onto the relay, the control API and the cameras in the same instant. |

## Live view

Off by default. With `LiveViewEnabled` false nothing here is read, started or written, and `status.json`, `cam.ps1 -Status` and the relay's generated config are exactly what they were. It is the server half of watching the cameras in a browser (the Live tab of the control UI, see [ui.md](ui.md#live-view)): a **second** MediaMTX, dedicated to WebRTC, supervised exactly like the relay and stopped with it. The recording relay never speaks WebRTC, because MediaMTX's WebRTC port answers any website's cross-origin request when its origins are the default and has no `Host` check, and a user allowed to read RTSP may read WebRTC too. The design and the reasoning behind each generated key are in [tasks/portable-nvr/live-1a-design.md](../tasks/portable-nvr/live-1a-design.md).

| Key | Default | Meaning |
| --- | --- | --- |
| `LiveViewEnabled` | `false` | Starts the live instance under the supervisor, once the relay is running. It needs `RestreamerEnabled`; without it the supervisor logs why and does not start it. A failed start logs one redacted line and recording carries on. |
| `LiveExePath` | `""` | The MediaMTX binary. Empty means `RestreamerExePath`. |
| `LiveConfigPath` | `C:\mediamtx\mediamtx-live.yml` | The generated config, rewritten by the supervisor at every start. A local path: a network or WSL share path is refused at load. |
| `LiveWhepAddress` | `127.0.0.1:8889` | Where WebRTC signaling listens. Loopback only: any other host is refused at load. |
| `LiveUdpAddress` | `127.0.0.1:8189` | The WebRTC media port (UDP). `127.0.0.1:<port>` (loopback only), `0.0.0.0:<port>` or one private IPv4 of this PC such as `192.168.1.50:8189`. Never a name, IPv6 or a public address. |
| `LiveAdvertiseHosts` | `[]` | Private IPv4 literals (at most 4) browsers are told to send media to, in addition to `127.0.0.1`, e.g. `["192.168.1.50"]`. Empty (the default) keeps the generated live config byte-for-byte what it was. A non-loopback `LiveUdpAddress` needs at least one entry and a non-empty list needs a non-loopback `LiveUdpAddress`; with a single bound address the list may name only that address. |
| `LiveSubStream` | `stream2` | The camera stream the live grid pulls, on demand. |
| `LiveCloseAfterSeconds` | `10` | How long after the last viewer leaves the camera's session is closed. |
| `LiveSecretPath` | `""` | The login's secret. Empty means `%LOCALAPPDATA%\IPCCTV\live_view_secret.txt`: a local per-user folder, because the repository share holds no access control list at all. |

A malformed value, or any key starting with `Live` that is not in this table, is refused when the configuration loads, whether or not live view is on.

- **Paths.** For each camera `<name>sub` pulls the camera's `LiveSubStream` only while someone watches (its second camera session), and `<name>main` pulls the recording relay's own loopback path, which adds no camera session. A camera whose recorder `Quality` already is `LiveSubStream` gets only `<name>main`. A live name that equals a relay path is an error.
- **LAN media (off by default).** Signaling stays on loopback (`LiveWhepAddress` is always `127.0.0.1`; browsers reach it through camui's proxy, never directly). Only the media can be offered on a LAN address. In MediaMTX v1.21.2 the host candidates in the answer come from `webrtcAdditionalHosts` (here `127.0.0.1` plus `LiveAdvertiseHosts`), each paired with the port of the UDP listener, because `webrtcIPsFromInterfaces` is off and every other host candidate is removed (`peer_connection.go`, `removeUnwantedCandidates` and `addAdditionalCandidates`). The listener binds `LiveUdpAddress`; an advertised address is reachable only if that bind covers it, hence `0.0.0.0` (all interfaces) or the one LAN address. Example for a PC at `192.168.1.50`: `"LiveUdpAddress": "0.0.0.0:8189", "LiveAdvertiseHosts": ["192.168.1.50"]`, then restart the recorder. A browser on this PC still works through the loopback candidate; one that ignores loopback candidates uses the LAN one (that Firefox does is an unverified hypothesis until it is tested). Listening beyond loopback means a Windows Firewall rule is needed for OTHER devices; see [ui.md](ui.md#live-view-on-the-lan-media-firewall-and-stuck-tiles). Nothing is created for you.
- **camui settings for the Live tab (not in `recorder.config.json`).** The page's behaviour when its window is hidden is camui's own setting, in `camui.config.json` beside `camui.exe` (restart camui to change it) or in the environment variable `CAMUI_LIVE_HIDDEN_GRACE`, which wins:

  | `liveHiddenGraceSeconds` | Meaning |
  |---|---|
  | `300` (default) | A minimized or covered window, or another camui tab, keeps its streams for 5 minutes, then frees them. Coming back inside that time shows the pictures again with no reconnect. |
  | `60` | Frees after one minute: the cameras' second session and the CPU are released sooner. |
  | `0` | Frees at once (the behaviour before the grace period existed); every return reconnects. |
  | `-1` | Never frees: the wall keeps decoding while hidden, about 21 to 31% of one core for six tiles on the measured PC. |

  Values are whole seconds, capped at 86400. Example `camui.config.json`: `{ "liveHiddenGraceSeconds": 60 }`. Details and measurements: [ui.md](ui.md#live-view).
- **Login.** One user, `camlive`, with `read` only; there is no anonymous user. The secret is 32 random bytes (64 hex characters) in a plain ASCII file the supervisor creates when it is missing, with an access control list naming only the current Windows user and nothing inherited, so no other account can read it; `camui` reads the same file. The generated config holds only its SHA-256 (`pass: sha256:...`). The secret is on no command line and in no log, `status.json` or `-Status` output. A web page cannot learn it, and `camui` adds the login server-side.
- **No cross-origin permission.** `webrtcAllowOrigins` is empty, so MediaMTX sends no `Access-Control-Allow-Origin`, and a browser POST that is not same-origin is refused even with a correct login. The browser therefore reaches the live instance only through a same-origin proxy such as `camui`. Every other MediaMTX server (RTSP, RTMP, HLS, SRT, API, metrics, pprof, playback) is off.
- **Status.** While live view is on, `status.json` has a `Live` section (`Enabled`, `Alive`, `Pid`, `LastPid`, `StartedAt`, `RestartsTotal`, `Paths` and `Cameras` as names only, `LastError`) and `cam.ps1 -Status` a `live view` section. With it off neither exists. Its log is `liveview.log` in the archive's `_logs` folder. Cameras added to `cameras.json` while it runs appear at its next start.
- **Camera sessions.** Each camera allows two. The recorder holds one through the relay; live view may hold the second, only while a `<name>sub` viewer is connected. While anyone watches a sub-stream, the phone app has no free session on that camera.

## Dashboard layout and health strip

`HealthStripHeightPx` is the one setting here - it is how you resize the strip or switch it off. The keys marked * are tuning constants that describe the layout this system already computes correctly, and a wrong one will degrade a working grid rather than configure it.

| Key | Default | Meaning |
| --- | --- | --- |
| `HealthStripHeightPx` | `-1` | `-1` derives the strip height from the video aspect, `0` switches the strip off, a positive number is that many pixels with the panes shrunk to fit. |
| `HealthStripMinPx` * | `120` | The strip never gets less than this; the panes shrink instead. |
| `GridGutterPx` * | `8` | Gap between grid rows and above the strip. Never at the screen edges. |
| `VideoAspectWidth` / `VideoAspectHeight` * | `16` / `9` | The shape of the video, which fixes pane height at a given width. |
| `HealthSections` * | `status, cameras, events, archive` | Which sections the strip renders, in this order. An unknown name is an error, not a silent skip. |
| `HealthEventRows` * | `5` | Rows the events section occupies, its label row included. |
| `StatusEventCount` * | `20` | How many recent log lines `status.json` publishes for the strip. |
| `HealthCellWidthPx` / `HealthCellHeightPx` * | `9` / `20` | Approximate font cell size, used only to ask Windows Terminal for a roughly right window before it is placed exactly. Neither can affect correctness. |

## The camera list

`cameras.json` is a separate file with its own schema - see [install.md](install.md#camerasjson). The control UI's Settings screen edits it too: add, remove, rename, change the IP, the stream and the record flag.
