# Windows Terminal IPCCTV Dashboard

A robust, portable PowerShell toolkit that turns a Windows desktop into a camera wall and a 24/7 recorder. A dynamic, self-healing grid of RTSP streams is tiled across the screen with Windows Terminal and `mpv`, a health strip along the bottom reports what the recorder is doing, and an independent supervisor records every camera you opt in, with audio, to local disk.

![The dashboard: six live feeds in a 3x2 grid with the health strip below them](docs/dashboard.png)

Nothing is recorded until you ask for it, per camera. The viewer and the recorder are separate programs that share nothing but a configuration file, so closing the dashboard does not stop recording and a recorder that is not installed does not stop you watching.

## Features

* **Dynamic Grid Layout:** No manual window positioning. The script calculates your screen real estate and tiles the panes to fit the number of cameras you have, reserving a strip along the bottom for the health view.
* **Self-Healing Streams:** Network timeouts plus a per-pane restart loop mean a camera that drops or a network that stutters restarts that one stream without touching the rest of the dashboard.
* **Continuous Recording:** A supervisor keeps one `ffmpeg` per enabled camera, writing clock-aligned segments with the camera's audio, restarting a child that dies with backoff, and killing one that wedges.
* **Automatic Retention:** The archive is pruned oldest-first against a free-space watermark, so the disk never fills and recording never stops because of it.
* **One Camera Session for Everything:** An optional local MediaMTX relay pulls each camera once and serves the dashboard and the recorder from it, which matters because these cameras allow very few simultaneous connections.
* **Secure Credentials (DPAPI):** No hardcoded passwords. The first run prompts for the camera credentials and encrypts them to a local XML file that only your Windows account can decrypt. The password is never written to a command line, a log or the generated relay config.
* **Portable JSON Configuration:** Add, remove or modify cameras by editing `cameras.json`. Everything else is tunable in `recorder.config.json`.
* **Runs at Logon:** One command registers an unelevated Scheduled Task that starts the recorder when you log in and restarts it if it dies.

## Prerequisites

1. **Windows 10 or 11** with [Windows Terminal](https://apps.microsoft.com/store/detail/windows-terminal/9N0DX20HK701) installed. Windows PowerShell 5.1 is what everything targets and it ships with Windows.
2. **[mpv media player](https://mpv.io/)**: download the Windows build and extract it so that `mpv.com` is at `C:\mpv\mpv.com`.
3. **[ffmpeg](https://www.gyan.dev/ffmpeg/builds/)**, for recording only: extract so that `ffmpeg.exe` and `ffprobe.exe` are in `C:\ffmpeg\bin`.
4. **[MediaMTX](https://github.com/bluenviron/mediamtx/releases)**, optional but recommended: download `mediamtx_<version>_windows_amd64.zip` and extract `mediamtx.exe` to `C:\mediamtx\mediamtx.exe`. Verified on v1.21.2. Do not put it in `Program Files`, which would need administrator rights. The 55 MB binary is deliberately not in this repository.
5. **RTSP compatible cameras** (TP-Link Tapo, Amcrest, Reolink, Unifi and anything else that serves `rtsp://user:pass@IP/stream1`).

All three tool paths are configurable: `$MpvPath` at the top of `mpv.cam.autolayout.autorestart.ps1`, and `FfmpegPath` / `FfprobePath` / `RestreamerExePath` in `recorder.config.json`.

## Installation

Clone or download the repository to a dedicated folder, then work through this order. Steps 1 to 3 give you the dashboard; the rest add recording.

```powershell
# 1. Launch the dashboard once. It prompts for the camera credentials, writes
#    camera_creds.xml, generates a cameras.json template and exits.
.\mpv.cam.autolayout.autorestart.ps1

# 2. Edit cameras.json with your real camera names, IPs and stream names, then
#    launch it again. This time the grid comes up.
.\mpv.cam.autolayout.autorestart.ps1

# 3. Copy the recorder config template and set RootPath to where footage goes.
copy recorder.config.example.json recorder.config.json

# 4. Pre-flight the recorder. Close the dashboard first: this probes the
#    cameras, and they allow only two sessions each.
.\cam.recorder.ps1 -SelfTest

# 5. Generate the relay config from cameras.json (skip if you are not using
#    MediaMTX). No credential is written to it.
.\cam.recorder.ps1 -InstallRestreamer

# 6. Opt cameras in. Nothing records until you do this.
.\cam.recorder.ps1 -Enable all

# 7. Start recording. The supervisor also starts and owns MediaMTX.
.\cam.recorder.ps1 -Run

# 8. Optional: have it start itself at every logon instead.
.\cam.recorder.ps1 -InstallAutostart
```

`cameras.json`, `recorder.config.json` and `camera_creds.xml` are local to your machine and are not tracked by git.

### cameras.json

```json
[
  {
    "Name": "Front Entrance",
    "IP": "10.0.1.15",
    "Quality": "stream1",
    "Record": true
  },
  {
    "Name": "Rear Balcony",
    "IP": "10.0.1.16",
    "Quality": "stream2"
  }
]
```

`Name` is what you see on the dashboard and what the footage folder is named after. `Quality` is the path on the camera: `stream1` is the main stream, `stream2` the substream on most firmware. `Record` is optional and defaults to false, which is why nothing records after an upgrade until you say so. Use `-Enable` and `-Disable` rather than editing it by hand; a running supervisor picks the change up within five seconds.

### Credentials

The first dashboard run prompts for the camera username and password and encrypts them to `camera_creds.xml` with Windows DPAPI, so only your Windows account on that machine can read them back. The recorder uses the same file and tells you to run the dashboard once if it is missing. To change them, delete `camera_creds.xml` and run the dashboard again.

## Command reference

### The dashboard: `mpv.cam.autolayout.autorestart.ps1`

| Command | What it does |
| --- | --- |
| `.\mpv.cam.autolayout.autorestart.ps1` | Opens the grid and the health strip. |
| `-PrintUrls` | Prints the input each camera would use right now, redacted, and the computed layout, then exits without opening a window. The quickest way to see whether the relay is being used. |
| `-NoHealthStrip` | Reserves the strip but launches no health window in it. |

Right-click the script and choose "Run with PowerShell", or run it from a terminal. Close the dashboard by closing its Windows Terminal window.

### The recorder: `cam.recorder.ps1`

| Command | What it does |
| --- | --- |
| `-Run` | Supervises one `ffmpeg` per enabled camera, writes `status.json`, prunes the archive, and starts and owns MediaMTX. This is the long-running process. |
| `-Enable <name\|all>` | Sets `Record` to true for one camera or all of them. A running supervisor starts recording it within `ReconcileSeconds`. |
| `-Disable <name\|all>` | Sets `Record` to false. That child stops and is not restarted. |
| `-List` | Every camera with its `Record` state and stream quality. |
| `-Status` | Renders `status.json` as a report, and says so when the file is stale. |
| `-Stop` | Asks a running supervisor to shut down, waits for it, and reports any `ffmpeg` or MediaMTX left behind. |
| `-InstallRestreamer` | Generates the MediaMTX config from `cameras.json`, one path per camera, and checks the binary is in place. |
| `-RemoveRestreamer` | Deletes that generated config. Recording and viewing continue on the direct camera inputs. |
| `-InstallAutostart` | Registers the logon Scheduled Task. Unelevated and idempotent. |
| `-RemoveAutostart` | Unregisters the task and deletes the launcher it created. A clean no-op when nothing is installed. |
| `-SelfTest [-Offline] [-MaxConcurrency <n>]` | Pre-flight report: tools, per-camera RTSP probe, measured connection limit, crash-safety proof, disk projection. `-Offline` skips every check that touches a camera. |

### The health strip: `cam.health.ps1`

| Command | What it does |
| --- | --- |
| `.\cam.health.ps1` | Fits itself to the console window it is in and refreshes in place. The dashboard launches it this way. |
| `-Rows <n>` `-Width <n>` | Render at an exact size instead of fitting the window. 0 means fit. |
| `-RefreshSeconds <n>` | How often to redraw. Default 2. |
| `-Once` | Render one frame as plain text and exit. |

It reads one file, `_state\status.json`, and nothing else. It never opens an RTSP session, so it can never be the thing that costs you a camera slot.

### The test harness: `tests\cam.tests.ps1`

| Command | What it does |
| --- | --- |
| `.\tests\cam.tests.ps1` | Runs every check that touches no camera: `parse`, `cameralist`, `selftest`, `layout`, `strip`, `sections`, `autostart`, `leak`, `procs`. Safe with the dashboard open. Exit code 0 means everything passed. |
| `-Checks <names>` | Run only these. |
| `-Live -Checks relay` | Adds the relay lifecycle proof, which starts MediaMTX and `ffmpeg` against the real cameras in a sandbox. |

One trap worth knowing: `powershell.exe -File .\tests\cam.tests.ps1 -Checks parse,strip` binds the whole list as a single string and fails validation. Use `powershell.exe -Command "& '.\tests\cam.tests.ps1' -Checks parse,strip"`.

## How it works

### The grid

The column and row counts come from the camera count alone, so six cameras are a 3x2 grid whether the health strip is there or not. Pane size follows from the screen width and the video aspect, and an 8 px gutter separates the grid rows and the strip. There is deliberately no gutter at the screen edges: three 640 px panes fill a 1920 px screen exactly, so an edge gutter could only come out of the picture.

Each pane runs its own `mpv` in a `while` loop. When a stream drops, that loop prints `Stream interrupted. Restarting in 3 seconds...` and reconnects. The camera URL is built inside the pane rather than passed into it, which keeps the credential out of the Windows Terminal and PowerShell command lines it would otherwise appear in.

Two timeouts are what make a frozen stream exit instead of hanging forever: `--demuxer-lavf-o=timeout=5000000`, a five second socket timeout passed to libavformat's RTSP demuxer, and mpv's own `--network-timeout=3`. If you have seen `stimeout` recommended for this, note that libavformat renamed it to `timeout` at ffmpeg 5.0 and mpv ships a newer libavformat than that, so `stimeout` is silently dropped and protects nothing. `--msg-level=all=error` keeps mpv's routine chatter out of the gutter, where the terminal window behind the feeds shows through, while still letting real errors print.

### Recording

Recording is opt-in per camera. The supervisor keeps exactly one `ffmpeg` child per enabled camera and reconciles against `cameras.json` every five seconds, so enabling, disabling or changing a camera's address takes effect without a restart. A child that exits is restarted with backoff from 3 s to 60 s, and one whose segment file stops growing for `StallFactor x SegmentSeconds` (2250 s by default) is killed and restarted by the watchdog. Only one supervisor can run at a time.

Streams are copied, never re-encoded, so recording costs almost no CPU and the footage is exactly what the camera sent.

Footage lands in flat per-camera folders:

```
E:\cameras\
  Front_Entrance\Front_Entrance_20261011_223649+0300.mkv
  Rear_Balcony\  Rear_Balcony_20261011_223649+0300.mkv
  _state\status.json        what the health strip and -Status read
  _logs\recorder.log        the supervisor log, rotated at LogMaxBytes
  _logs\restreamer.log      MediaMTX output, redacted
  _logs\<Camera>.log        per-camera ffmpeg output, redacted
```

Two details in there are not preferences:

* **MKV, not MP4.** All six of these cameras emit `pcm_alaw` audio, and MP4 refuses `pcm_alaw` with stream copy. MKV is also the only container here that survives a crash: a file that was being written when the power went out still plays.
* **The `+0300` suffix.** ffmpeg's `strftime` formatting is local time only and ignores `TZ` on Windows, so without the UTC offset in the name the repeated hour at the end of daylight saving would silently overwrite the first one.

Segments are 15 minutes and aligned to the clock, give or take: a stream copy can only cut on a keyframe, so each boundary overshoots the mark by up to one GOP.

### Retention

The pruner runs every `PruneIntervalSeconds` and watches free space rather than age. When free space falls below `ReserveGB` it deletes oldest-first until `ReserveGB + PruneHysteresisGB` is free, which stops it thrashing at the boundary. It never deletes the segment being written. Set `MaxRetentionDays` if you also want an age cap.

Six cameras at `stream1` measured between 92 and 100 GB per day here, so plan on roughly three days per 300 GB of free space. `-Status` and the health strip both print the measured rate and the days of headroom left.

### Transport: the relay

These cameras allow very few simultaneous RTSP sessions, which is the whole reason MediaMTX is here. With the relay running, each camera is pulled exactly once and MediaMTX serves both the dashboard and the recorder from that one session. The supervisor starts MediaMTX as its own child, so there is one thing to autostart rather than two, and it waits for the relay's paths to come up before starting the recorders.

The choice is made per start attempt, per consumer: if the relay is serving that camera it is used, and if it is not, a direct camera connection is used instead. A relay that dies is left behind at the next restart and one that comes back is picked up without relaunching anything. The health strip's `MODE` column tells you which input each recorder is actually on.

A consumer that had to take a direct connection gives the camera slot back on its own once the relay is serving that camera again. A recorder child is stopped and restarted by its supervisor; an mpv pane watches its own path every `ViewerPromoteCheckSeconds` (10 by default, 0 to switch it off) and restarts itself onto the relay, which costs one brief blink in that pane, once per outage. Only a pane on a direct connection watches anything - a pane on the relay needs no watch, because if the relay dies mpv loses its input and the pane decides again by itself.

The generated `mediamtx.yml` contains no password. Every path is left as `source: publisher` in the file and the supervisor supplies the real camera URL on the child's environment block at launch.

### The health strip

The strip at the bottom of the screen is a Windows Terminal window that the dashboard places by pixel and fills with `cam.health.ps1`. It renders four sections, which you can reorder or switch off individually with `HealthSections`:

* `status` - one line: supervisor state and uptime, relay state, free space, days left.
* `cameras` - per camera: record flag, whether the child is alive, relay or direct, how long since the last closed segment, restarts in the last hour, and the last error.
* `events` - the supervisor's most recent log lines, newest first.
* `archive` - the span and size of the footage, the measured daily rate, and what the pruner has deleted.

Set `HealthStripHeightPx` to 0 to switch the strip off entirely and get the original full-height grid back.

### Autostart

`-InstallAutostart` registers a Scheduled Task that runs at logon as you, with an interactive, limited token. It needs no elevation and it never prompts. Reinstalling replaces the registration rather than adding a second one. The task has no execution time limit, because the default three day limit would otherwise kill a 24/7 supervisor on day three, it ignores a second instance, and it restarts the task up to three times at one minute intervals if it fails.

The task does not point at `cam.recorder.ps1` directly. It points at a small generated launcher at `%LOCALAPPDATA%\IPCCTV\start-recorder.ps1`, which waits for the recorder's path to become reachable, logs what it saw to `%LOCALAPPDATA%\IPCCTV\autostart.log`, and then forwards to `cam.recorder.ps1 -Run`. That indirection exists because a repository kept inside WSL is reached over `\\wsl.localhost`, which does not resolve until WSL's file server is running, and a Scheduled Task has no way to wait for a path. The launcher holds no copy of the scripts, so it cannot drift from the repository. `-RemoveAutostart` deletes the launcher along with the task, keeping only the log.

## Configuration

`recorder.config.json` overlays the built-in defaults; any key you leave out keeps its default, and the file is read by the recorder, the health strip and the dashboard alike. Keys marked * are not in `recorder.config.example.json` and have to be added by hand if you want to change them.

### Paths and recording

| Key | Default | Meaning |
| --- | --- | --- |
| `RootPath` | `E:\cameras` | Where footage, `_state` and `_logs` live. |
| `FfmpegPath` / `FfprobePath` | `C:\ffmpeg\bin\...` | The recording tools. |
| `SegmentSeconds` | `900` | Segment length, aligned to the clock. |
| `Container` | `mkv` | Which entry of `ContainerProfiles` to use. `fmp4` re-encodes audio to AAC. |
| `RtspTransport` * | `tcp` | Transport for the recorder's inputs. |
| `TimeoutMicroseconds` * | `10000000` | ffmpeg's socket timeout. |

### Retention

| Key | Default | Meaning |
| --- | --- | --- |
| `ReserveGB` | `20` | Free space the pruner defends. |
| `PruneHysteresisGB` | `5` | How much extra it frees once it starts, so it does not thrash. |
| `PruneIntervalSeconds` | `300` | How often it looks. |
| `MaxRetentionDays` | `null` | Optional age cap, on top of the space rule. |

### Supervisor

| Key | Default | Meaning |
| --- | --- | --- |
| `ReconcileSeconds` * | `5` | How quickly an `-Enable` or an edited IP is picked up. |
| `StatusIntervalSeconds` * | `5` | How often `status.json` is rewritten. |
| `ArchiveScanSeconds` * | `60` | How often the whole archive is measured for the disk figures. Separate because it touches every file. |
| `StopTimeoutSeconds` * | `30` | How long `-Stop` waits for a clean teardown. |
| `BackoffStartSeconds` / `BackoffMaxSeconds` * | `3` / `60` | Restart backoff for a failing child. |
| `HealthyResetSeconds` * | `300` | How long a child must stay up before its backoff resets. |
| `StallFactor` * | `2.5` | Multiplied by `SegmentSeconds` for the stall watchdog. |
| `LogMaxBytes` * | `2 MB` | Log rotation threshold. |
| `LogRepeatSuppressSeconds` * | `60` | Collapses a repeating log line. |

### Relay

| Key | Default | Meaning |
| --- | --- | --- |
| `RestreamerEnabled` | `true` | Set false to run on direct camera inputs only. |
| `RestreamerExePath` | `C:\mediamtx\mediamtx.exe` | The binary. Must be on a local drive. |
| `RestreamerConfigPath` | `C:\mediamtx\mediamtx.yml` | The generated config. Must be on a local NTFS path: MediaMTX watches it with `ReadDirectoryChanges`, which the WSL share does not implement, and exits immediately if it cannot. |
| `RelayBase` | `rtsp://127.0.0.1:8554` | Where consumers read. Loopback only; the generated config allows anonymous access precisely because nothing off the machine can reach it. |
| `RelayApiBase` | `http://127.0.0.1:9997` | The control API the readiness probe uses. |
| `RelayProbeTimeoutMs` | `1500` | Budget for one probe. |
| `RelayStartupWaitSeconds` * | `20` | How long the supervisor waits for the relay's paths before starting recorders. |

### Dashboard layout and health strip

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

## Known limitations

These are measured, not theoretical. None of them is hidden in the code.

1. **Cameras serve exactly two concurrent RTSP sessions.** Measured on all six here and consistent with TP-Link's documentation, so it is firmware policy rather than a bug. The dashboard plus the recorder uses both slots, leaving nothing for the phone app, a browser preview or an ad-hoc `ffprobe`. The relay exists to turn those two slots into one.
2. **If the dashboard and the recorder are both already on direct inputs, MediaMTX can never pull.** Both slots are taken and there is no third for the relay. Restart either consumer and it will pick the relay up.
3. **MediaMTX path names must be lowercase alphanumeric.** Its environment variable key is cut at the first underscore, so a path named `workshop_entrance` silently creates a stray `workshop` path and leaves the real one unserved, with nothing logged. The generator lowercases names and strips non-alphanumerics for exactly this reason. Do not hand-edit the generated config.
4. **One copy of the camera password lives in MediaMTX's environment block.** That is the floor: neither ffmpeg nor MediaMTX has an out-of-band credential source, so the URL has to reach it somehow. Nothing is written to disk, and every log line and console message is redacted to `rtsp://***:***@host/stream1`.
5. **Segment boundaries overshoot the clock mark by up to one GOP**, because a stream copy can only cut on a keyframe.
6. **Tapo cameras keep a separate account for RTSP**, distinct from the TP-Link cloud login. Changing the cloud password does not affect RTSP. To change the RTSP credential, delete `camera_creds.xml` and run the dashboard again.
7. **Some paths have never been exercised over a long run:** log rotation at `LogMaxBytes`, the full 900 s production segment, the backoff reset at `HealthyResetSeconds`, the 2250 s stall watchdog and `MaxRetentionDays`. They are implemented and unit-checked, not soak-tested.
8. **Several tuning keys are absent from `recorder.config.example.json`.** They are marked * in the tables above; add them by hand if you want to change one.
9. **The test harness leaves its sandbox behind on purpose**, under `%TEMP%\cam-tests\<check>`, so you can inspect what a check did. The credential copy it makes is always deleted; footage a live check recorded is not. Delete the folder when you are done with it.
10. **Autostart depends on the recorder's path being reachable at logon.** If the repository lives inside WSL, the launcher polls `\\wsl.localhost` for up to 300 s and the task then retries up to three times, but nothing records while WSL is down. Keeping the repository on a local drive avoids the wait entirely. The launcher log at `%LOCALAPPDATA%\IPCCTV\autostart.log` is the first thing to read if autostart did not work.
11. **The health strip does not quite reach the screen edges.** It paints x 6 to 1906 of 1920, so about 6 px of desktop shows at its left edge and 13 px at its right. Top and bottom are exact and no video is affected. It cannot be fixed while the strip is a Windows Terminal window: covering the full width needs a window rect wider than the screen, and that is exactly what makes Windows Terminal mis-size itself.
12. **`status.json` reports configured relay paths, not ready ones.** The strip says `N path(s)` and tells relay from direct per camera in its `MODE` column, which is the honest signal; do not read the count as readiness.

## Troubleshooting

* **Terminal flashes and closes immediately.** `$MpvPath` must point at `mpv.com`, the command-line executable, not `mpv.exe`.
* **Streams will not connect.** Check the IPs in `cameras.json`, and check your camera's URL shape: this project builds `rtsp://user:pass@IP/<Quality>`. If your manufacturer uses a different path structure, `Get-CamRtspUrl` in `cam.common.ps1` is the one place that builds it, for the dashboard and the recorder alike.
* **`-SelfTest` reports a concurrency of 1 and the cameras look broken.** The dashboard is open and holding a slot. Close it, or run `-SelfTest -Offline`, which skips every camera-facing check.
* **The strip says the supervisor is stale or missing.** `status.json` is older than three status intervals, which means the supervisor is not running. Start it with `-Run`, or check `_logs\recorder.log`.
* **Everything records to direct inputs and the relay never engages.** Both camera slots are already taken; see limitation 2. Restart the dashboard or the recorder.
* **MediaMTX exits immediately with `ReadDirectoryChanges: Incorrect function`.** `RestreamerConfigPath` is on the WSL share. Point it at a local NTFS path such as `C:\mediamtx\mediamtx.yml`.
* **Autostart did nothing at logon.** Read `%LOCALAPPDATA%\IPCCTV\autostart.log`, and confirm the task with `schtasks /query /tn IPCCTV-Recorder /fo list`.
* **Need to change the camera password?** Delete `camera_creds.xml` and run the dashboard again to get a fresh prompt.
* **Script exits immediately after asking for config.** Normal on a first run. Edit `cameras.json` and run it again.

## Under the hood

To prevent quoting and bracket parsing errors when passing complex `mpv` arguments into Windows Terminal splits, the dashboard generates each pane's PowerShell command and converts it to a Base64 `-EncodedCommand` string before execution. The recorder and the health strip share `cam.common.ps1`, which owns configuration loading, the camera list, redaction, the status file readers and the layout algorithm, so the three scripts can never disagree about what they are looking at.
