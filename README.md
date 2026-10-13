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
* **Secure Credentials (DPAPI):** No hardcoded passwords. The first run prompts for the camera credentials and encrypts them to a local XML file that only your Windows account can decrypt. The password is never written to a log and never written to the generated relay config, and on the relay path it reaches no command line at all, because consumers read `rtsp://127.0.0.1:8554/<path>`. A consumer that falls back to a direct camera input is the exception: `ffmpeg` and `mpv` then take the credential inside the RTSP URL on their own command line, as do the probes `-SelfTest` makes. That is the floor rather than an oversight - see limitation 4.
* **Portable JSON Configuration:** Add, remove or modify cameras by editing `cameras.json`. Everything else is tunable in `recorder.config.json`.
* **Runs at Logon:** One command registers an unelevated Scheduled Task that starts the recorder when you log in and restarts it if it dies. The viewer grid and the control UI each have one of their own.
* **A Browser Control UI:** `camui.exe`, one self-contained binary, serves a local web UI that starts and stops everything, toggles recording per camera, edits the camera list and the recording settings, and finds and plays back footage with sound. It owns no state: every number comes from the same `status.json` the health strip reads, and every action is performed by the scripts below, so the UI and the command line can never disagree. It binds `127.0.0.1` and refuses to bind anything else without an auth token.

## Prerequisites

1. **Windows 10 or 11** with [Windows Terminal](https://apps.microsoft.com/store/detail/windows-terminal/9N0DX20HK701) installed. Windows PowerShell 5.1 is what everything targets and it ships with Windows.
2. **[mpv media player](https://mpv.io/)**: download the Windows build and extract it so that `mpv.com` is at `C:\mpv\mpv.com`.
3. **[ffmpeg](https://www.gyan.dev/ffmpeg/builds/)**, for recording only: extract so that `ffmpeg.exe` and `ffprobe.exe` are in `C:\ffmpeg\bin`.
4. **[MediaMTX](https://github.com/bluenviron/mediamtx/releases)**, optional but recommended: download `mediamtx_<version>_windows_amd64.zip` and extract `mediamtx.exe` to `C:\mediamtx\mediamtx.exe`. Verified on v1.21.2. Do not put it in `Program Files`, which would need administrator rights. The 55 MB binary is deliberately not in this repository.
5. **RTSP compatible cameras** (TP-Link Tapo, Amcrest, Reolink, Unifi and anything else that serves `rtsp://user:pass@IP/stream1`).
6. **Nothing extra for the control UI.** `camui.exe` has the web UI embedded in it, needs no runtime, no web server and no internet access, and is **not** in this repository - it is gitignored like the MediaMTX binary. Build it once with `ui/build.sh`, which needs a Go toolchain on the **build** machine only; `docs/go-toolchain.md` is the one-time install. The NVR machine itself never needs Go.

All three tool paths are configurable: `$MpvPath` at the top of `mpv.cam.autolayout.autorestart.ps1`, and `FfmpegPath` / `FfprobePath` / `RestreamerExePath` in `recorder.config.json`.

**One thing to settle before step 1: the execution policy.** With nothing set in any scope, the effective policy on Windows **client** editions is `Restricted`, which runs no `.ps1` at all - so step 1 below fails with `cannot be loaded because running scripts is disabled on this system` and nothing in this repository works. Check it with `Get-ExecutionPolicy -List`, and if it is `Restricted` or `Undefined` everywhere, this is enough and needs no elevation:

```powershell
Set-ExecutionPolicy -Scope CurrentUser RemoteSigned
```

If the repository arrived as a downloaded `.zip`, Windows marks every file in it as coming from the internet and `RemoteSigned` will still refuse them; `Get-ChildItem *.ps1 | Unblock-File` in the repository folder clears the mark. Cloning with git does not set it. This matters only for the commands **you** type: everything the project launches for itself - the panes, the health strip, the NVR window, all three logon-task launchers - already passes `-NoProfile -ExecutionPolicy Bypass`, so once it is installed the policy cannot stop it starting.

## Installation

Clone or download the repository to a dedicated folder, then work through this order. Steps 1 to 3 give you the dashboard, steps 4 to 8 add recording, and steps 9 to 11 add the browser UI.

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
#    cameras, and they allow only two sessions each. Leave it closed until
#    step 7, which brings it back up in the right order; or reopen it by
#    hand with the step 2 command if you want it now.
.\cam.recorder.ps1 -SelfTest

# 5. Generate the relay config from cameras.json. No credential is written
#    to it. Not using MediaMTX? Then skip this AND set RestreamerEnabled to
#    false in recorder.config.json - left true with no relay to start, the
#    supervisor retries it on a backoff and logs the failure every time,
#    and each camera spends RelayReadyWaitSeconds (15 s) on its first start
#    waiting for a relay path that will never serve.
.\cam.recorder.ps1 -InstallRestreamer

# 6. Opt cameras in. Nothing records until you do this.
.\cam.recorder.ps1 -Enable all

# 7. Start recording. The supervisor also starts and owns MediaMTX.
.\cam.recorder.ps1 -Nvr

# 8. Optional: have them start themselves at every logon instead. Two
#    separate tasks on purpose - the recorder is headless, the grid needs an
#    interactive desktop - and each switch touches only its own.
.\cam.recorder.ps1 -InstallAutostart
.\cam.ps1 -InstallDashboardAutostart

# 9. Build the control UI. A bash command, so run it from WSL or any machine
#    with bash and Go 1.27 or newer - the Go in Debian/Ubuntu's apt is far
#    older and will not build it; docs/go-toolchain.md installs a current one.
#    camui.exe is gitignored, so it does not arrive with the repository. The
#    script can be run from any directory and puts the binary beside the
#    scripts as .\camui.exe, which is where it has to be: with no -script-dir
#    it looks for cam.ps1, cameras.json and recorder.config.json in its own
#    folder. Built anywhere else, copy it here.
#      ui/build.sh

# 10. Install the UI's logon task, then start it without waiting for a logon.
.\cam.ps1 -InstallUi
Start-ScheduledTask -TaskName IPCCTV-Ui

# 11. Open the UI in a browser ON WINDOWS - http://127.0.0.1:8787/. A curl
#     from inside WSL cannot reach it: WSL's loopback is its own, not the
#     Windows one, so it fails to connect and the UI looks down when it is
#     serving perfectly. Then check everything in one view.
.\cam.ps1 -Status
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

### Everything at once: `cam.ps1`

The control surface. It starts, stops and reports every part of the system, including processes it did not start itself.

| Command | What it does |
| --- | --- |
| `.\cam.ps1` or `-Status` | What is running, in one view: the supervisor and its `ffmpeg` children, the NVR window and its job, the relay and the paths it serves, the dashboard panes and their `mpv`, the control UI and its address, all three logon tasks, and every process this project owns with its role and parent. Orphans are named as orphans. |
| `-Start` | Brings the recorder up, waits for the relay to serve, then opens the dashboard. That order matters: a pane that decides before the relay is serving takes a direct camera connection and burns the spare session. |
| `-Stop` | Stops everything and verifies that it did, orphans from a previous crash included. A no-op when nothing is running, safe to run twice, and silent on the alerting channels by design. It does not stop the control UI. |
| `-Start -Recorder` / `-Stop -Recorder` | The recorder, its NVR window and MediaMTX only. |
| `-Start -Dashboard` / `-Stop -Dashboard` | The viewer panes, their `mpv` and the health strip only. |
| `-InstallDashboardAutostart` | Registers the `IPCCTV-Dashboard` logon task that opens the viewer grid. Unelevated, idempotent, and it never touches the other two tasks. |
| `-RemoveDashboardAutostart` | Removes that task and its launcher, keeping the log. A clean no-op when they are not there, and a dashboard that is open stays open. |
| `-InstallUi` | Registers the `IPCCTV-Ui` logon task that starts `camui.exe`. Unelevated, idempotent, and it never touches the other two tasks. It refuses to install when `camui.exe` has not been built, and it does not start the UI - it prints the one command that does. |
| `-RemoveUi` | Removes that task and its launcher, keeping the logs. A clean no-op when they are not there. A `camui` that is running stays running, and the removal prints the pid that is serving so you can stop it if you want to. |

Nothing here is identified by image name or window title. Every process is classified by the work it owns, and the recording supervisor and its children sit on a keep list that every stop honours.

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
| `-Run` | Supervises one `ffmpeg` per enabled camera, writes `status.json`, prunes the archive, and starts and owns MediaMTX. This is the long-running process, in the **foreground**. Use `-Nvr` instead unless you specifically want it in the console you are in: a `-Run` started in an ordinary terminal has its console adopted as a tab in whichever terminal window already exists, and closing that window kills it with no cleanup and orphaned children. |
| `-Enable <name\|all>` | Sets `Record` to true for one camera or all of them. A running supervisor starts recording it within `ReconcileSeconds`. |
| `-Disable <name\|all>` | Sets `Record` to false. That child stops and is not restarted. |
| `-List` | Every camera with its `Record` state and stream quality. |
| `-Status` | Renders `status.json` as a report, and says so when the file is stale. |
| `-Stop` | Asks a running supervisor to shut down, waits for it, and reports any `ffmpeg` or MediaMTX left behind. |
| `-InstallRestreamer` | Generates the MediaMTX config from `cameras.json`, one path per camera, and checks the binary is in place. |
| `-RemoveRestreamer` | Deletes that generated config. Recording and viewing continue on the direct camera inputs. |
| `-Nvr` | Opens the NVR: one Windows Terminal window of its own, titled `cam.nvr`, running the supervisor as its only pane with the log on screen. Closing that window stops the supervisor, MediaMTX, every ffmpeg and the viewers. Closing any other terminal does nothing. |
| `-InstallAutostart` | Registers the logon Scheduled Task. Unelevated and idempotent. |
| `-InstallEventSource` | Registers the custom `IPCCTV` Application-log event source. Needs an elevated session, once, and is optional - alerting works without it. |
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

### The control UI: `camui.exe`

Install it with `.\cam.ps1 -InstallUi` and let the logon task run it - that is the normal way. These are for building it and for the rare direct run.

| Command | What it does |
| --- | --- |
| `ui/build.sh [output]` | Cross-compiles `camui.exe` into the repository root, or to `output` if you name one. A bash command, so it needs WSL or another machine with bash, and **Go 1.27 or newer** - `go.mod` asks for it and an apt-packaged Go will refuse the build; `docs/go-toolchain.md` is the one-time install, and the script also finds a toolchain at `~/.local/go/bin`. It finds its own directory, so the directory you run it from does not matter. It runs `gofmt`, `go vet` and `go test` first and stops on any of them, so a build that completes is a build that passed. |
| `.\camui.exe` | Runs it in the foreground on `http://127.0.0.1:8787/`, with its log on screen. Useful when you want to watch what it is doing; Ctrl+C stops it. |
| `-listen <host:port>` | Where to bind. Default `127.0.0.1:8787`. It **refuses to start** on any non-loopback address unless a token of at least 16 characters is configured. |
| `-config <path>` | A JSON config file: `listen`, `archiveRoot`, `statusFile`, `scriptDir`, `token`. The installed launcher passes `camui.config.json` from beside the binary if that file exists, and nothing else needs to change to use it. |
| `-archive-root <path>` `-status-file <path>` `-script-dir <path>` | Override what it reads. The archive root is taken from `RootPath` in the `recorder.config.json` beside the binary, so moving the archive needs no second edit here, and falls back to `E:\cameras` only when that file cannot be read; the status file defaults to `<archive root>\_state\status.json` and the script directory to the directory holding the binary. Precedence for the root is this flag or `archiveRoot` in the config file, then `RootPath`, then the built-in. |
| `-version` | Prints the build stamp and exits. |

The token is never taken from a flag, because a command line is readable by every process on the machine: it comes from the config file or from `CAMUI_TOKEN` in the environment.

### The test harness: `tests\cam.tests.ps1`

| Command | What it does |
| --- | --- |
| `.\tests\cam.tests.ps1` | Runs all **fourteen** checks that touch no camera: `parse`, `cameralist`, `inputs`, `viewer`, `selftest`, `layout`, `strip`, `sections`, `autostart`, `crashsafe`, `windowclose`, `leak`, `uileak`, `procs`. Safe with the dashboard open. Exit code 0 means nothing failed. |
| `-Checks <names>` | Run only these. |
| `-Live -Checks relay` | Adds the relay lifecycle proof, which starts MediaMTX and `ffmpeg` against the real cameras in a sandbox. It is the only check `-Live` unlocks. |

**A check that skips is not a check that failed, and how many skip depends on the machine.** Exit code 0 means no check failed, so read the tally rather than counting on a fixed split. Four can skip: `autostart` and `procs` while the real recorder is installed and running (limitation 13), and `leak` and `uileak` when there is no `camera_creds.xml`, because then there is no password to scan for and a clean scan would prove nothing. `uileak` also skips when `camui.exe` has not been built or will not start.

One trap worth knowing: `powershell.exe -File .\tests\cam.tests.ps1 -Checks parse,strip` binds the whole list as a single string and fails validation. Use `powershell.exe -Command "& '.\tests\cam.tests.ps1' -Checks parse,strip"`.

## How it works

### The grid

The column and row counts come from the camera count alone, so six cameras are a 3x2 grid whether the health strip is there or not. Pane size follows from the screen width and the video aspect, and an 8 px gutter separates the grid rows and the strip. There is deliberately no gutter at the screen edges: three 640 px panes fill a 1920 px screen exactly, so an edge gutter could only come out of the picture.

Each pane runs its own `mpv` in a `while` loop. When a stream drops, that loop prints `Stream interrupted. Restarting in 3 seconds...` and reconnects. The camera URL is built inside the pane rather than passed into it, which keeps the credential out of the Windows Terminal and PowerShell command lines it would otherwise appear in. A pane on the relay then hands `mpv` a loopback URL with no credential in it at all; a pane that had to take a direct input hands it the real one, which is on `mpv`'s own command line for as long as that pane stays direct - limitation 4.

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

**Do not plan from a number in this file.** The rate depends on how many cameras are up, which stream each one is on and how much the scenes move, so it drifts with the system rather than sitting still: observed here anywhere from the high forties to around 100 GB per day for six `stream1` cameras. `.\cam.recorder.ps1 -Status` and the health strip both print the rate measured over the archive as it stands, with the days of headroom left - that figure is the one to plan from, because it is the only one that is current. `cam.ps1 -Status` reports processes and services, not disk.

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

### The control UI

`camui.exe` is one Go binary with the whole UI embedded in it, serving three screens on `http://127.0.0.1:8787/`:

* **Dashboard** - start and stop everything, the recorder alone or the viewers alone; a record toggle per camera with what that camera is actually doing; free space and the days of headroom left; the supervisor's recent events; and the state of each service. The actions that stop footage being written ask for confirmation first.
* **Settings** - the camera list (add, remove, rename, change the IP, the stream and the record flag) and the recording keys it is allowed to change: segment length, reserve, hysteresis and retention days. Both files are validated, written atomically and keep their UTF-8 BOM, through the same code the CLI uses.
* **Footage** - camera, then date, then segment, with the player inline. The index is parsed from the filenames the recorder already writes, so there is no database and nothing opens a segment to list it, and a day's gaps are visible as gaps. A clip is remuxed to fragmented MP4 on demand, video copied and only the 8 kHz audio transcoded, so playback keeps its sound.

It owns no state. Every number comes from `_state\status.json`, the same file the health strip reads, and every change goes through `cam.ps1` and `cam.recorder.ps1`. That is why the command line keeps working unchanged and why the two can never disagree about anything.

**No camera credential ever reaches the browser.** Cameras are addressed by name, and no endpoint returns a relay or RTSP URL of any shape. The harness proves it rather than trusting it: the `uileak` check fetches every endpoint, a real clip included, and scans every byte for the password, for its URL-escaped form and for anything shaped like a credential in a URL, with a positive control proving the scanner can see one.

**Loopback only, deliberately.** `127.0.0.1` needs no `urlacl` and no elevation, and the binary **refuses to start** on any address that is not exclusively loopback unless an auth token of at least 16 characters is configured. Exposing it beyond this machine is therefore a deliberate act rather than something that can happen by accident. To do it, put a config file beside the binary:

```json
{
  "listen": "0.0.0.0:8787",
  "token": "a-long-random-string-of-at-least-16-characters"
}
```

Save that as `camui.config.json` next to `camui.exe`, restart the UI, and nothing else has to change: the launcher passes that file if it exists, and `-Status` reports the address it found and where it read it from. Every request then needs the token, which can arrive once as `?token=...` and is moved into an HttpOnly cookie so it stops appearing in the address bar, in history and in any `Referer`. The scripts do read the token - `Get-CamUiRuntimeState` opens `camui.config.json` and looks at `CAMUI_TOKEN` to answer the one question `-Status` asks of it - but they never print it, log it or pass it on: `-Status` reports only whether one is configured.

**How it runs.** `-InstallUi` registers `IPCCTV-Ui`, a logon task that runs a generated launcher at `%LOCALAPPDATA%\IPCCTV\start-ui.ps1`. The launcher waits for `camui.exe` to be reachable, exactly as the recorder's launcher waits for the scripts, then starts it in a console of its **own** with no window. That last part is not cosmetic: a console created with a window is handed to Windows Terminal as a tab in the window that already exists, and closing that window would take the UI with it. It starts nothing at all if the address is already being served, so a second start cannot fight the first one for the port, and it exits non-zero if nothing is serving afterwards, which hands the retry to the task's restart policy. camui logs to stdout only, so a thin `powershell.exe` holds the pipe and appends it to `%LOCALAPPDATA%\IPCCTV\camui.log`. **The roll happens in the launcher, not in a running UI:** at every start it checks that file, and if it is past 10 MB it renames it to `camui.log.prev` - replacing any previous `.prev` - and starts a fresh one. A UI that stays up for weeks therefore keeps appending to one file past 10 MB, with a line per request and nothing to bound it; stopping and starting it is what rolls the log. The launcher's own log is `autostart-ui.log` beside it, and that one has no rotation at all - it gets a handful of lines per start, so it grows slowly, but it grows forever. Delete either by hand when it is in the way; nothing reads them back.

The binary is never copied anywhere, so a rebuild takes effect the next time the UI starts, with no reinstall. Stop it before rebuilding and start it again afterwards: a running process keeps the code it started with, and with the repository on the WSL share the write is not refused the way Windows refuses to overwrite a running binary on a local drive.

`cam.ps1 -Stop` does not stop the UI, which is the point - the thing you start everything from should still be there after you have stopped everything. When you do want it gone, `-Status` prints the pid that owns the port.

### Autostart

`-InstallAutostart` registers a Scheduled Task that runs at logon as you, with an interactive, limited token. It needs no elevation and it never prompts. Reinstalling replaces the registration rather than adding a second one. The task has no execution time limit, because the default three day limit would otherwise kill a 24/7 supervisor on day three, it ignores a second instance, and it restarts the task up to three times at one minute intervals if it fails.

The task opens the NVR window rather than running the supervisor directly, and that is not cosmetic. Windows 11 hands every console created with a window to the default terminal application, which here is Windows Terminal, and with `windowingBehavior` set to `useExisting` the console arrives as a **tab in whichever terminal window already exists** - the Operator's. `-WindowStyle Hidden` does not prevent it, because the window belongs to Windows Terminal rather than to `powershell.exe`. Closing that window sent `CTRL_CLOSE_EVENT` to the supervisor, which PowerShell answers by terminating without running any `finally` block: no shutdown line in the log, and every ffmpeg plus MediaMTX left orphaned and still writing. It happened four times on 2026-10-12. Launching the supervisor as a pane in a window of its own removes the adoption, and the job object `cam.nvr` makes closing that window stop everything in it.

The task does not point at `cam.recorder.ps1` directly. It points at a small generated launcher at `%LOCALAPPDATA%\IPCCTV\start-recorder.ps1`, which waits for the recorder's path to become reachable, logs what it saw to `%LOCALAPPDATA%\IPCCTV\autostart.log`, and then forwards to `cam.recorder.ps1 -Nvr`. That indirection exists because a repository kept inside WSL is reached over `\\wsl.localhost`, which does not resolve until WSL's file server is running, and a Scheduled Task has no way to wait for a path. The launcher holds no copy of the scripts, so it cannot drift from the repository. `-RemoveAutostart` deletes the launcher along with the task, keeping only the log.

There are **three** tasks, one per part, and each install switch touches only its own: `IPCCTV-Recorder` from `.\cam.recorder.ps1 -InstallAutostart`, `IPCCTV-Dashboard` from `.\cam.ps1 -InstallDashboardAutostart`, and `IPCCTV-Ui` from `.\cam.ps1 -InstallUi`. They are separate because their lifetimes are: the recorder is a headless service, the grid is a desktop artifact that needs an interactive session, and the UI is a local server that should outlive both. `cam.ps1 -Status` lists all three with their state and the launcher each one points at.

### When something stops unexpectedly

Two job objects answer two different questions, and neither is a polling loop:

| Job | Owner | Fires when |
| --- | --- | --- |
| `cam.nvr` | the warden, outside the window | the **window** closes |
| `cam.supervisor.<hash of root>` | the supervisor itself | the **supervisor** dies, however abruptly |

A job with `KILL_ON_JOB_CLOSE` terminates every process in it when the last handle closes, and process exit closes a handle whatever killed the process. So a supervisor that crashes takes MediaMTX and every `ffmpeg` with it - measured twice on the live system, four `ffmpeg` and MediaMTX to zero in under one second with no orphans. Jobs nest, so the supervisor being inside `cam.nvr` already is not an obstacle.

The NVR pane is not the supervisor. It is a **host** that runs the supervisor as a child and restarts it when it stops without being asked to. That split exists because from outside, closing the window and a crash look identical - supervisor gone, `status.json` still saying `running`, window destroyed - and guessing wrong either way is bad. With the split they are different events: closing the window gives both processes `CTRL_CLOSE_EVENT` and they die together, while a crash leaves the host and the window untouched and the child exiting is the signal. Recovery took 11 and 13 seconds in the live tests, with no window churn and no intervention.

Expected or not is read from what the supervisor recorded, never from an exit code alone, because a killed process has no say in its exit code. The stop flag, `StopKind` of `requested` or `interrupted`, and exit 20 all mean do not restart. The budget is five restarts in thirty minutes; out of budget it alerts once more and stays down rather than looping all night.

**An unexpected stop alerts on three channels**, and an orderly `cam.ps1 -Stop` raises none of them:

- a **Windows toast**, through WinRT under the Windows PowerShell AUMID. In the box, unelevated, does not steal focus, and the alert stays in the Action Center.
- an **Application-log entry**, prefixed `IPCCTV: `. Registering a custom event source writes to `HKLM` and needs elevation, so by default the entries go out under the always-registered `Application` source and need no elevation anywhere. `-InstallEventSource`, run elevated once, registers the `IPCCTV` source instead.
- **`StopKind` and `LastUnexpectedStop` in `status.json`**, the latter carrying the timestamp, the reason and the pid, and carried across restarts so the NVR being back up does not erase the fact that it went down.

A camera that has written nothing for `CameraDownAlertMinutes` alerts the same three ways and publishes `DownSince` in its `status.json` row. It is judged on file progress alone: a camera that cannot be reached has its `ffmpeg` restarted every backoff period, so process liveness says nothing.

**Closing the NVR window is the one stop that nothing recovers from.** It raises its own alert - event id 103, and a Warning rather than an Error, because nothing malfunctioned: somebody closed the window, and the supervisor, MediaMTX, every `ffmpeg` and the viewers went with it, which is exactly what that window is for. **Nothing restarts them.** The logon task ran to completion when it opened the window, so its restart policy has nothing to restart, and the next automatic start is the next logon. Start it again with `.\cam.ps1 -Start`. `status.json` records `StopKind` `window-closed` and a `LastWindowClose` timestamp, so the UI and the log agree about what happened.

The five event ids, which are what separate these entries from everyone else's in a shared log:

| id | Level | What it means |
| --- | --- | --- |
| `100` | Error | the supervisor stopped unexpectedly and is being restarted |
| `101` | Warning | it is recording again |
| `102` | Error | it stopped and the restart budget is spent, so it is staying down |
| `103` | Warning | the NVR window was closed, so nothing is recording and nothing will restart it |
| `110` | Warning | one camera has written nothing for `CameraDownAlertMinutes` |

Reading the alerts back:

```powershell
Get-WinEvent -FilterHashtable @{ LogName = 'Application'; Id = 100,101,102,103,110 } |
  Select-Object TimeCreated, Id, LevelDisplayName, ProviderName,
    @{ n = 'Text'; e = { if ($_.Message) { $_.Message } else { [string]$_.Properties[0].Value } } } |
  Where-Object { $_.Text -like 'IPCCTV: *' }
```

It filters on the `IPCCTV: ` prefix rather than on a provider **on purpose**: the entries arrive under `Application` by default and under `IPCCTV` once `-InstallEventSource` has been run, so pinning `ProviderName = 'Application'` in the filter would quietly return nothing after the install. The prefix is on every message either way, which is what it is for, and the `ProviderName` column then tells you which source answered. Numbers alone are not enough to filter on: ids as low as 100 are not ours exclusively in a log every application writes to.

The `Properties[0].Value` fallback is needed because a source with no message resource leaves `Get-WinEvent`'s `.Message` empty. `-InstallEventSource` registers with `EventCreate.exe` as the message resource, which makes `.Message` render. With no matching events at all, `Get-WinEvent` reports "No events were found that match the specified selection criteria", which is the quiet answer rather than a fault.

## Configuration

`recorder.config.json` overlays the built-in defaults; any key you leave out keeps its default, and the file is read by the recorder, the health strip and the dashboard alike. Keys marked * are not in `recorder.config.example.json` and have to be added by hand if you want to change them.

### Paths and recording

| Key | Default | Meaning |
| --- | --- | --- |
| `RootPath` | `E:\cameras` | Where footage, `_state` and `_logs` live. |
| `FfmpegPath` / `FfprobePath` | `C:\ffmpeg\bin\...` | The recording tools. |
| `SegmentSeconds` | `900` | Segment length, aligned to the clock. |
| `Container` | `mkv` | Which entry of `ContainerProfiles` to use. `fmp4` re-encodes audio to AAC. |
| `ContainerProfiles` | `mkv` and `fmp4` | The profiles themselves: per profile an `Extension`, a `SegmentFormat`, `SegmentFormatOptions`, and `AudioArgs` / `ExtraArgs` added to the ffmpeg command line. Add a profile here to add a container; there is no other place that knows about one. |
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
| `CameraDownAlertMinutes` * | `15` | How long a camera may write nothing before it raises an alert. |
| `CameraDownRepeatHours` * | `12` | How long before the same camera alerts again while still down. |
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
| `RelayStartupWaitSeconds` * | `20` | How long the supervisor waits for the relay's control API to answer before starting any recorder. The API only, not every path: one offline camera's path can never become ready and would otherwise hold up the healthy ones. |
| `RelayReadyWaitSeconds` * | `15` | How long **one** consumer waits for **its own** relay path before giving up and taking a direct camera slot. Spent only on a first attempt at batch startup; a later attempt decides on the spot, so a relay that died is left behind immediately. |
| `RelayReadyPollMs` * | `500` | How often that wait re-asks. Floored at 50. |
| `ViewerPromoteCheckSeconds` * | `10` | How often an `mpv` pane that is on a direct connection asks whether its camera is back on the relay, so it can hand the second session back. One control-API call per direct pane per interval. `0` switches the watch off and leaves such a pane direct until the dashboard is relaunched. |
| `StartStaggerMs` * | `300` | Delay between consecutive consumer starts, recorder children and `mpv` panes alike, so six of them do not burst onto the relay, the control API and the cameras in the same instant. |

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
4. **The camera password reaches ffmpeg, mpv and MediaMTX in the clear.** That is the floor: none of them has an out-of-band credential source, so the URL has to arrive somehow. MediaMTX gets it on its environment block, which is not visible to the process listings any user can read. A consumer on a **direct** camera input gets it inside the RTSP URL on its own command line, which is: that covers `ffmpeg` and `mpv` whenever the relay is not serving that camera, and the `ffprobe` and `ffmpeg` calls `-SelfTest` makes, which always address the cameras directly. Running the relay is what keeps everything else off that path. Nothing is written to disk, and every log line and console message is redacted to `rtsp://***:***@host/stream1`.
5. **Segment boundaries overshoot the clock mark by up to one GOP**, because a stream copy can only cut on a keyframe.
6. **Tapo cameras keep a separate account for RTSP**, distinct from the TP-Link cloud login. Changing the cloud password does not affect RTSP. To change the RTSP credential, delete `camera_creds.xml` and run the dashboard again.
7. **Some paths have never actually fired here:** log rotation at `LogMaxBytes`, the pruner's first deletion (free space has never fallen to `ReserveGB` on this machine), the age cap `MaxRetentionDays`, the backoff reset at `HealthyResetSeconds` and the 2250 s stall watchdog. They are implemented and unit-checked, not soak-tested. The 900 s production segment is no longer on that list: real 15-minute segments have been measured at 900.02 to 900.05 s under `ffprobe`.
8. **Several tuning keys are absent from `recorder.config.example.json`.** They are marked * in the tables above; add them by hand if you want to change one.
9. **The test harness leaves its sandbox behind on purpose**, under `%TEMP%\cam-tests\<check>`, so you can inspect what a check did. The credential copy it makes is always deleted; footage a live check recorded is not. Delete the folder when you are done with it.
10. **Autostart depends on the recorder's path being reachable at logon.** If the repository lives inside WSL, the launcher polls `\\wsl.localhost` for up to 300 s and the task then retries up to three times, but nothing records while WSL is down. Keeping the repository on a local drive avoids the wait entirely. The launcher log at `%LOCALAPPDATA%\IPCCTV\autostart.log` is the first thing to read if autostart did not work.
11. **The health strip does not quite reach the screen edges.** It paints x 6 to 1906 of 1920, so about 6 px of desktop shows at its left edge and 13 px at its right. Top and bottom are exact and no video is affected. It cannot be fixed while the strip is a Windows Terminal window: covering the full width needs a window rect wider than the screen, and that is exactly what makes Windows Terminal mis-size itself.
12. **The footage player cannot seek ahead of what has arrived.** A clip is remuxed on demand and streamed, so the response carries `Accept-Ranges: none` and no length, and the browser can only seek inside what it has already buffered. Nothing is lost, it is just a wait: a 15-minute segment is about 170 MB and arrives in under ten seconds here, after which the whole clip is seekable.
13. **Two harness checks always skip on a live NVR.** `autostart` refuses to run while a task named `IPCCTV-Recorder` is registered, because it would rewrite and then delete the real autostart, and `procs` refuses while the recorder is running, because the processes it hunts for already have a legitimate owner. On this machine both skip permanently, so a green run of the fourteen camera-free checks reads twelve passes and two skips. That split is not a constant: `leak` and `uileak` skip as well on a machine with no `camera_creds.xml`, and `uileak` skips if `camui.exe` is not there to start.
14. **`status.json` reports configured relay paths, not ready ones.** The strip says `N path(s)` and tells relay from direct per camera in its `MODE` column, which is the honest signal; do not read the count as readiness.

## Troubleshooting

### If something is wrong, read this first

* **Nothing is recording and nothing is coming back.** The NVR window was closed. The window titled `cam.nvr` **is** the NVR: closing it stops the supervisor, MediaMTX, every `ffmpeg` and the viewers, and nothing restarts them. You get one alert, event id 103. Start it again with `.\cam.ps1 -Start`.
* **A supervisor that crashed comes back by itself.** The NVR pane is a host that restarts it, alerting as it goes (id 100) and again once it is recording (id 101), up to five times in thirty minutes. Out of budget it alerts once more (id 102) and stays down rather than looping all night.
* **One camera has gone quiet.** A camera that writes nothing for `CameraDownAlertMinutes`, 15 by default, alerts under id 110 and gets a `DownSince` in `status.json`. It is judged on file progress alone: a camera that cannot be reached has its `ffmpeg` restarted every backoff period, so the process being alive says nothing.
* **`cam.ps1 -Stop` is quiet on purpose.** An orderly stop is not a fault, so it raises no toast and writes no event. It still prints the component counts before and after and exits non-zero if anything survived.
* **Alerts land in the Application log prefixed `IPCCTV: `**, because registering a custom event source writes to `HKLM` and needs elevation. Run `.\cam.recorder.ps1 -InstallEventSource` once from an elevated session to get the `IPCCTV` source and readable `.Message` text instead. Everything works without it; that is what the prefix is for.
* **The UI will not open.** First, check you are asking from Windows: a `curl http://127.0.0.1:8787/` **from inside WSL** cannot reach it, because WSL's loopback is not the Windows loopback, and the connection is simply refused - which reads exactly like a UI that is down. Open it in a Windows browser, or ask from Windows PowerShell. Then `.\cam.ps1 -Status` prints whether anything is serving `127.0.0.1:8787` and which pid owns the port. If the task has run and nothing is serving, `%LOCALAPPDATA%\IPCCTV\autostart-ui.log` says how far the launcher got and `camui.log` beside it holds camui's own output. The common cause is `camui.exe` not being built: `-InstallUi` refuses to install a task that cannot work and tells you to run `ui/build.sh`.

### Other symptoms

* **Terminal flashes and closes immediately.** `$MpvPath` must point at `mpv.com`, the command-line executable, not `mpv.exe`.
* **Streams will not connect.** Check the IPs in `cameras.json`, and check your camera's URL shape: this project builds `rtsp://user:pass@IP/<Quality>`. If your manufacturer uses a different path structure, `Get-CamRtspUrl` in `cam.common.ps1` is the one place that builds it, for the dashboard and the recorder alike.
* **`-SelfTest` reports a concurrency of 1 and the cameras look broken.** The dashboard is open and holding a slot. Close it, or run `-SelfTest -Offline`, which skips every camera-facing check.
* **The strip says the supervisor is stale or missing.** `status.json` is older than three status intervals, which means the supervisor is not running. Start it with `.\cam.ps1 -Start`, or `.\cam.recorder.ps1 -Nvr` for the recorder alone, and check `_logs\recorder.log` for why it stopped. Do **not** start it with `-Run` from an ordinary terminal: that is the foreground supervisor, its console is adopted as a tab in the terminal window you already have, and closing that window kills it with no cleanup and orphaned children - see Autostart below for the four times that happened here.
* **Everything records to direct inputs and the relay never engages.** Both camera slots are already taken; see limitation 2. Restart the dashboard or the recorder.
* **MediaMTX exits immediately with `ReadDirectoryChanges: Incorrect function`.** `RestreamerConfigPath` is on the WSL share. Point it at a local NTFS path such as `C:\mediamtx\mediamtx.yml`.
* **Autostart did nothing at logon.** Read `%LOCALAPPDATA%\IPCCTV\autostart.log`, and confirm the task with `schtasks /query /tn IPCCTV-Recorder /fo list`.
* **Need to change the camera password?** Delete `camera_creds.xml` and run the dashboard again to get a fresh prompt.
* **Script exits immediately after asking for config.** Normal on a first run. Edit `cameras.json` and run it again.

## Under the hood

To prevent quoting and bracket parsing errors when passing complex `mpv` arguments into Windows Terminal splits, the dashboard generates each pane's PowerShell command and converts it to a Base64 `-EncodedCommand` string before execution. The recorder and the health strip share `cam.common.ps1`, which owns configuration loading, the camera list, redaction, the status file readers and the layout algorithm, so the three scripts can never disagree about what they are looking at.
