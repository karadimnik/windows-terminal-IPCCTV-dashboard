# Windows Terminal IPCCTV Dashboard

A PowerShell toolkit that turns a Windows desktop into a camera wall and a 24/7 recorder: a self-healing grid of RTSP streams tiled with Windows Terminal and `mpv`, a health strip reporting what the recorder is doing, an independent supervisor recording every camera you opt in with audio to local disk, and a browser UI that drives the lot. Nothing is recorded until you ask for it, per camera. The viewer and the recorder are separate programs that share nothing but a configuration file, so closing the dashboard does not stop recording and a recorder that is not installed does not stop you watching.

## Quick reference

- **Just the monitor** - `.\cam.ps1 -Start -Dashboard`.
- **Just the recorder** - `.\cam.ps1 -Start -Recorder`. Never `-Run` from an ordinary terminal: that console is adopted as a tab in the window you already have, and closing that window killed the supervisor four times on 2026-10-12.
- **Everything** - `.\cam.ps1 -Start`. `-Stop` and `-Status` take the same `-Recorder` / `-Dashboard` scoping, and unscoped means all of it.
- **Best way to start everything** - log on: `.\cam.ps1 -Install` puts three logon tasks in place, one each for the recorder, the dashboard and the UI - see [Install and uninstall](#install-and-uninstall). Not the UI, which cannot start itself; `cam.ps1 -Start` is the manual equivalent.
- **The UI** - two processes serving <http://127.0.0.1:8787/>, started on demand or by its logon task, and deliberately left running by `cam.ps1 -Stop` so there is always something to start things from.
- **Closing a terminal window** - each tier is a Win32 job with `KILL_ON_JOB_CLOSE` whose only handle is held by a warden outside the window, so closing the window sweeps every member and grandchild. The UI's two processes survive by design, and a hand-started `-Run` supervisor is outside all of it.

![The dashboard: six live feeds in a 3x2 grid with the health strip below them](docs/dashboard.png)

## It is running - where do I look?

**<http://127.0.0.1:8787/>** - the control UI. Open it in a browser **on Windows**. Everything starts itself at logon, so the normal state is that you touch nothing and this URL is all you need.

A `curl` to that address **from inside WSL cannot reach it** - WSL's loopback is its own, not the Windows one, so the connection is refused and a perfectly healthy UI looks down.

### The three screens

* **Dashboard** - start and stop everything, the recorder alone or the viewers alone; a record toggle per camera with what that camera is actually doing; free space and the days of headroom left; the supervisor's recent events; and the state of each service. The actions that stop footage being written ask for confirmation first.
* **Settings** - the camera list (add, remove, rename, change the IP, the stream and the record flag) and the four recording keys it is allowed to change: segment length, reserve, hysteresis and retention days.
* **Footage** - camera, then date, then segment, with the player inline. A day's gaps are visible as gaps.

### The three commands

Everything the UI does is also a command, because the UI performs every action by calling the scripts. These three are the ones worth knowing:

```powershell
.\cam.ps1 -Status   # what is running: processes, the relay, the three logon tasks. Not disk.
.\cam.ps1 -Start    # recorder first, then the dashboard, in the order that keeps a camera slot free
.\cam.ps1 -Stop     # stops everything and verifies it, orphans included. Leaves the UI running.
```

For the disk figures - the rate footage is accruing at, free space and the days of headroom left - use `.\cam.recorder.ps1 -Status` or read the health strip. `cam.ps1 -Status` reports processes and services, not disk. Every other command is in [Reference](#reference).

Installing on a new machine is one command - [Install and uninstall](#install-and-uninstall).

## Something is wrong

| Symptom | Likely cause | What to do |
| --- | --- | --- |
| **Nothing is recording and nothing is coming back.** | The NVR window was closed. The window titled `cam.nvr` **is** the NVR: closing it stops the supervisor, MediaMTX, every `ffmpeg` and the viewers, and **nothing restarts them**. You get one alert, event id 103. | `.\cam.ps1 -Start` |
| **The strip says the supervisor is stale or missing.** | `status.json` is older than three status intervals, which means the supervisor is not running. | `.\cam.ps1 -Start`, or `.\cam.recorder.ps1 -Nvr` for the recorder alone, then read `_logs\recorder.log` for why it stopped. Do **not** start it with `-Run` from an ordinary terminal - see [Autostart](#autostart) for why. |
| **The supervisor crashed and came back by itself.** | Not a fault. The NVR pane is a host that restarts it, alerting as it goes (id 100) and again once it is recording (id 101), up to five times in thirty minutes. | Nothing. Out of budget it alerts once more (id 102) and stays down rather than looping all night; then `.\cam.ps1 -Start` after reading the log. |
| **One camera has gone quiet.** | It has written nothing for `CameraDownAlertMinutes`, so it alerted under id 110 and has a `DownSince` in `status.json`. It is judged on file progress alone: a camera that cannot be reached has its `ffmpeg` restarted every backoff period, so the process being alive says nothing. | Check the camera and the network. The supervisor keeps retrying on its own. |
| **The UI will not open.** | Either you are asking from inside WSL (see above), or `camui.exe` was never built, so `-Install` skipped the UI - see [Install and uninstall](#install-and-uninstall). | Open it in a Windows browser, or ask from Windows PowerShell. `.\cam.ps1 -Status` prints whether anything is serving `127.0.0.1:8787` and which pid owns the port. If the task ran and nothing is serving, `%LOCALAPPDATA%\IPCCTV\autostart-ui.log` says how far the launcher got and `camui.log` beside it holds camui's own output. |
| **`cam.ps1 -Stop` was silent.** | On purpose. An orderly stop is not a fault, so it raises no toast and writes no event. | Nothing. It still prints the component counts before and after and exits non-zero if anything survived. |
| **Everything records to direct inputs and the relay never engages.** | Both camera slots are already taken, so MediaMTX has no third one to pull with - limitation 2. | Restart the dashboard or the recorder; whichever restarts picks the relay up. |
| **Terminal flashes and closes immediately.** | `$MpvPath` points at `mpv.exe`. | Point it at `mpv.com`, the command-line executable. |
| **Streams will not connect.** | Wrong IP, or a camera whose URL shape is not `rtsp://user:pass@IP/<Quality>`. | Check the IPs in `cameras.json`. `Get-CamRtspUrl` in `cam.common.ps1` is the one place that builds the URL, for the dashboard and the recorder alike. |
| **`-SelfTest` reports a concurrency of 1 and the cameras look broken.** | The dashboard is open and holding a slot. | Close it, or run `-SelfTest -Offline`, which skips every camera-facing check. |
| **MediaMTX exits immediately with `ReadDirectoryChanges: Incorrect function`.** | `RestreamerConfigPath` is on the WSL share, which does not implement it. | Point it at a local NTFS path such as `C:\mediamtx\mediamtx.yml`. |
| **Autostart did nothing at logon.** | Usually the repository's path was not reachable yet - limitation 10. | Read `%LOCALAPPDATA%\IPCCTV\autostart.log`, and confirm the task with `schtasks /query /tn IPCCTV-Recorder /fo list`. |
| **The camera password needs changing.** | - | Delete `camera_creds.xml` and run the dashboard again for a fresh prompt. Tapo cameras keep a separate RTSP account from the TP-Link cloud login; changing the cloud password does nothing here. |

### The alerts

An unexpected stop alerts on three channels, and an orderly `cam.ps1 -Stop` raises none of them: a **Windows toast** (WinRT under the Windows PowerShell AUMID - unelevated, does not steal focus, stays in the Action Center), an **Application-log entry** prefixed `IPCCTV: `, and **`StopKind` plus `LastUnexpectedStop` in `status.json`**, the latter carrying the timestamp, the reason and the pid, and carried across restarts so the NVR being back up does not erase the fact that it went down.

| id | Level | What it means |
| --- | --- | --- |
| `100` | Error | the supervisor stopped unexpectedly and is being restarted |
| `101` | Warning | it is recording again |
| `102` | Error | it stopped and the restart budget is spent, so it is staying down |
| `103` | Warning | the NVR window was closed, so nothing is recording and nothing will restart it |
| `110` | Warning | one camera has written nothing for `CameraDownAlertMinutes` |
| `120` | Error | pruning is disabled: `RootPath` is unsafe or not the recorder's own archive, so the disk will fill |
| `121` | Error | pruning is blocked: free space is below `ReserveGB` and nothing deletable is left, so the disk is filling. Once per episode; recording continues |
| `122` | Error | the age rule's circuit breaker held a run that would have deleted more than `MaxAgePruneFraction` of the archive. Once per episode; the free-space rule still runs |

Registering a custom event source writes to `HKLM` and needs elevation, so by default the entries go out under the always-registered `Application` source. `.\cam.recorder.ps1 -InstallEventSource`, run elevated once ([Install and uninstall](#install-and-uninstall)), registers the `IPCCTV` source instead, with `EventCreate.exe` as the message resource so `.Message` renders. Everything works without it; that is what the prefix is for. Reading them back:

```powershell
Get-WinEvent -FilterHashtable @{ LogName = 'Application'; Id = 100,101,102,103,110,120,121,122 } |
  Select-Object TimeCreated, Id, LevelDisplayName, ProviderName,
    @{ n = 'Text'; e = { if ($_.Message) { $_.Message } else { [string]$_.Properties[0].Value } } } |
  Where-Object { $_.Text -like 'IPCCTV: *' }
```

That filters on the `IPCCTV: ` prefix, not on a provider, **on purpose**: the entries arrive under `Application` until `-InstallEventSource` has been run and under `IPCCTV` after, so pinning `ProviderName = 'Application'` would quietly return nothing after the install; the `ProviderName` column tells you which answered. The ids alone are not ours exclusively in a log every application writes to, and the `Properties[0].Value` fallback covers a source with no message resource, which leaves `.Message` empty. With no matching events `Get-WinEvent` reports "No events were found that match the specified selection criteria" - the quiet answer, not a fault.

## Install and uninstall

One command installs everything that makes the system start by itself, and one removes it. First, once - the prerequisites (Windows Terminal, `mpv`, `ffmpeg`, MediaMTX) are in [docs/install.md](docs/install.md), with the execution policy and the `cameras.json` schema:

1. Run `.\mpv.cam.autolayout.autorestart.ps1`. It asks for the camera login, saves it encrypted as `camera_creds.xml`, writes a `cameras.json` template and exits. Fill in your cameras.
2. Build the UI with `ui/build.sh`, from WSL ([docs/go-toolchain.md](docs/go-toolchain.md)). Skip it and `-Install` installs everything else and reports the UI as skipped.

```powershell
.\cam.ps1 -Install -WhatIf   # the plan: every prerequisite, then each component. Changes nothing.
.\cam.ps1 -Install           # install
.\cam.ps1 -Start             # -Install starts nothing: this starts the recorder and dashboard now
.\cam.ps1 -Uninstall         # remove it again
```

**`-Install`** checks every prerequisite first and lists every problem at once with its fix; any of them stops it before it changes anything, except a missing `camui.exe`, which only skips the UI. Then, in order: the MediaMTX config generated from `cameras.json`, and the logon tasks `IPCCTV-Recorder` (the NVR window), `IPCCTV-Dashboard` (the viewer grid) and `IPCCTV-Ui`. Each reports installed, already installed, skipped or failed, one failing does not stop the rest, and it exits 0 only when all four are in place. Anything already in place is left alone, so a second run changes nothing; after updating the scripts, `-Uninstall` then `-Install` rewrites the launchers. No elevation.

**`-Uninstall`** removes the three tasks, their launchers in `%LOCALAPPDATA%\IPCCTV\` and the generated MediaMTX config. It keeps the logs, the archive, `cameras.json`, `recorder.config.json` and the credentials, and it stops nothing: `.\cam.ps1 -Stop` does that, and leaves the UI running on purpose. A second run is a clean no-op.

**Optional, elevated, once:** `.\cam.recorder.ps1 -InstallEventSource` registers the `IPCCTV` event source for [the alerts](#the-alerts). `-Install` does it itself when run elevated and prints this line when not; alerting works without it. `Remove-EventLog -Source IPCCTV`, elevated, removes it.

### The granular switches

`-Install` and `-Uninstall` are made of these. They exist for scripts and recovery: each installs or removes one component and touches nothing else. All are unelevated except the event source, idempotent, a clean no-op when there is nothing to do, and every removal keeps its launcher's log.

| Command | What it does |
| --- | --- |
| `.\cam.recorder.ps1 -InstallRestreamer` / `-RemoveRestreamer` | Generates the MediaMTX config from `cameras.json`, one path per camera and no credential, and checks the binary is in place; or deletes it, and recording and viewing continue on the direct camera inputs. |
| `.\cam.recorder.ps1 -InstallAutostart` / `-RemoveAutostart` | Registers or removes `IPCCTV-Recorder` and its launcher. A supervisor already running keeps running. |
| `.\cam.ps1 -InstallDashboardAutostart` / `-RemoveDashboardAutostart` | Registers or removes `IPCCTV-Dashboard` and its launcher. A dashboard that is open stays open. |
| `.\cam.ps1 -InstallUi` / `-RemoveUi` | Registers or removes `IPCCTV-Ui` and its launcher. It refuses to install when `camui.exe` has not been built, and it does not start the UI - it prints `Start-ScheduledTask -TaskName IPCCTV-Ui`, which does. A `camui` that is running stays running, and the removal prints the pid serving it. |
| `.\cam.recorder.ps1 -InstallEventSource` | Registers the `IPCCTV` Application-log event source. Needs an elevated session, once, and is optional. |

## How it works

### Credentials

No hardcoded passwords. The first run prompts for the camera credentials and encrypts them to a local XML file that only your Windows account can decrypt. The password is never written to a log and never written to the generated relay config, and on the relay path it reaches no command line at all, because consumers read `rtsp://127.0.0.1:8554/<path>`. A consumer that falls back to a direct camera input is the exception: `ffmpeg` and `mpv` then take the credential inside the RTSP URL on their own command line, as do the probes `-SelfTest` makes. That is the floor rather than an oversight - see limitation 4.

### The grid

The column and row counts come from the camera count alone, so six cameras are a 3x2 grid whether the health strip is there or not. Pane size follows from the screen width and the video aspect, and an 8 px gutter separates the grid rows and the strip. There is deliberately no gutter at the screen edges: three 640 px panes fill a 1920 px screen exactly, so an edge gutter could only come out of the picture.

Each pane runs its own `mpv` in a `while` loop. When a stream drops, that loop prints `Stream interrupted. Restarting in 3 seconds...` and reconnects. The camera URL is built inside the pane rather than passed into it, which keeps the credential out of the Windows Terminal and PowerShell command lines it would otherwise appear in. A pane on the relay then hands `mpv` a loopback URL with no credential in it at all; a pane that had to take a direct input hands it the real one, which is on `mpv`'s own command line for as long as that pane stays direct - limitation 4.

Two timeouts are what make a frozen stream exit instead of hanging forever: `--demuxer-lavf-o=timeout=5000000`, a five second socket timeout passed to libavformat's RTSP demuxer, and mpv's own `--network-timeout=3`. If you have seen `stimeout` recommended for this, note that libavformat renamed it to `timeout` at ffmpeg 5.0 and mpv ships a newer libavformat than that, so `stimeout` is silently dropped and protects nothing. `--msg-level=all=error` keeps mpv's routine chatter out of the gutter, where the terminal window behind the feeds shows through, while still letting real errors print.

### Recording

Recording is opt-in per camera. The supervisor keeps exactly one `ffmpeg` child per enabled camera and reconciles against `cameras.json` every `ReconcileSeconds`, so enabling, disabling or changing a camera's address takes effect without a restart. A child that exits is restarted with backoff, and one whose segment file stops growing for `StallFactor` x `SegmentSeconds` is killed and restarted by the watchdog. Only one supervisor can run at a time.

Streams are copied, never re-encoded, so recording costs almost no CPU and the footage is exactly what the camera sent. Segments are 15 minutes and aligned to the clock, give or take: a stream copy can only cut on a keyframe, so each boundary overshoots the mark by up to one GOP.

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

### Retention

The pruner runs every `PruneIntervalSeconds` and watches free space rather than age. When free space falls below `ReserveGB` it deletes oldest-first until `ReserveGB + PruneHysteresisGB` is free, which stops it thrashing at the boundary. It never deletes the segment being written. If nothing deletable is left and free space is still below `ReserveGB` it alerts once (id 121) and keeps recording. Set `MaxRetentionDays` if you also want an age cap; a run that would delete more than `MaxAgePruneFraction` (default half) of the archive by age deletes nothing by age and alerts once (id 122), so a mistyped retention cannot empty it.

**Do not plan from a number in a document.** The rate footage accrues at depends on how many cameras are up, which stream each one is on and how much the scenes move, so it drifts with the system rather than sitting still. `.\cam.recorder.ps1 -Status` and the health strip both print the rate measured over the archive as it stands, with the days of headroom left - that figure is the one to plan from, because it is the only one that is current.

### Transport: the relay

These cameras allow very few simultaneous RTSP sessions, which is the whole reason MediaMTX is here. With the relay running, each camera is pulled exactly once and MediaMTX serves both the dashboard and the recorder from that one session. The supervisor starts MediaMTX as its own child, so there is one thing to autostart rather than two, and it waits for the relay's paths to come up before starting the recorders.

The choice is made per start attempt, per consumer: if the relay is serving that camera it is used, and if it is not, a direct camera connection is used instead. A relay that dies is left behind at the next restart and one that comes back is picked up without relaunching anything. The health strip's `MODE` column tells you which input each recorder is actually on.

A consumer that had to take a direct connection gives the camera slot back on its own once the relay is serving that camera again. A recorder child is stopped and restarted by its supervisor; an `mpv` pane watches its own path every `ViewerPromoteCheckSeconds` and restarts itself onto the relay, which costs one brief blink in that pane, once per outage. Only a pane on a direct connection watches anything - a pane on the relay needs no watch, because if the relay dies `mpv` loses its input and the pane decides again by itself.

The generated `mediamtx.yml` contains no password. Every path is left as `source: publisher` in the file and the supervisor supplies the real camera URL on the child's environment block at launch.

### The health strip

The strip at the bottom of the screen is a Windows Terminal window that the dashboard places by pixel and fills with `cam.health.ps1`. It reads one file, `_state\status.json`, and nothing else, so it can never be the thing that costs you a camera slot. It renders four sections, which you can reorder or switch off individually with `HealthSections`: `status` (supervisor state and uptime, relay state, free space, days left), `cameras` (per camera: record flag, child alive, relay or direct, time since the last closed segment, restarts in the last hour, last error), `events` (the supervisor's most recent log lines, newest first) and `archive` (the span and size of the footage, the measured daily rate, and what the pruner has deleted). Set `HealthStripHeightPx` to 0 to switch the strip off entirely and get the original full-height grid back. The strip does not quite reach the screen's left and right edges, where a few pixels of desktop show: that cannot be fixed while the strip is a Windows Terminal window, because covering the full width needs a window rect wider than the screen and that is exactly what makes Windows Terminal mis-size itself. Top and bottom are exact and no video is affected.

### The control UI

`camui.exe` is one Go binary with the whole UI embedded in it. **It owns no state.** Every number comes from `_state\status.json`, the same file the health strip reads, and every change goes through `cam.ps1` and `cam.recorder.ps1`. That is why the command line keeps working unchanged and why the two can never disagree about anything. Its Footage index is parsed from the filenames the recorder already writes, so there is no database and nothing opens a segment to list it; a clip is remuxed to fragmented MP4 on demand, video copied and only the 8 kHz audio transcoded, so playback keeps its sound.

**No camera credential ever reaches the browser.** Cameras are addressed by name, and no endpoint returns a relay or RTSP URL of any shape. The harness proves it rather than trusting it: the `uileak` check fetches every endpoint, a real clip included, and scans every byte for the password, for its URL-escaped form and for anything shaped like a credential in a URL, with a positive control proving the scanner can see one.

**Loopback only, deliberately.** `127.0.0.1` needs no `urlacl` and no elevation, and the binary **refuses to start** on any address that is not exclusively loopback unless an auth token of at least 16 characters is configured. Exposing it beyond this machine is therefore a deliberate act rather than something that can happen by accident. To do it, save this as `camui.config.json` next to `camui.exe` and restart the UI:

```json
{
  "listen": "0.0.0.0:8787",
  "token": "a-long-random-string-of-at-least-16-characters"
}
```

Nothing else has to change: the launcher passes that file if it exists, and `-Status` reports the address it found and where it read it from. Every request then needs the token, which can arrive once as `?token=...` and is moved into an HttpOnly cookie so it stops appearing in the address bar, in history and in any `Referer`. The scripts **do** read the token - `Get-CamUiRuntimeState` opens `camui.config.json` and looks at `CAMUI_TOKEN` to answer the one question `-Status` asks of it - but they never print it, log it or pass it on: `-Status` reports only whether one is configured. The token is never taken from a flag, because a command line is readable by every process on the machine.

**How it runs.** The `IPCCTV-Ui` logon task runs a generated launcher at `%LOCALAPPDATA%\IPCCTV\start-ui.ps1`. The launcher waits for `camui.exe` to be reachable, exactly as the recorder's launcher waits for the scripts, then starts it in a console of its **own** with no window. That last part is not cosmetic: a console created with a window is handed to Windows Terminal as a tab in the window that already exists, and closing that window would take the UI with it. It starts nothing at all if the address is already being served, so a second start cannot fight the first one for the port, and it exits non-zero if nothing is serving afterwards, which hands the retry to the task's restart policy.

camui logs to stdout only, so a thin `powershell.exe` holds the pipe and appends it to `%LOCALAPPDATA%\IPCCTV\camui.log`. **The roll happens in the launcher, not in a running UI:** at every start it checks that file, and if it is past 10 MB it renames it to `camui.log.prev` - replacing any previous `.prev` - and starts a fresh one. A UI that stays up for weeks therefore keeps appending to one file past 10 MB, with a line per request and nothing to bound it; stopping and starting it is what rolls the log. The launcher's own log is `autostart-ui.log` beside it, and that one has no rotation at all - it gets a handful of lines per start, so it grows slowly, but it grows forever. Delete either by hand when it is in the way; nothing reads them back.

The binary is never copied anywhere, so a rebuild takes effect the next time the UI starts, with no reinstall. Stop it before rebuilding and start it again afterwards: a running process keeps the code it started with, and with the repository on the WSL share the write is not refused the way Windows refuses to overwrite a running binary on a local drive. `cam.ps1 -Stop` does not stop the UI, which is the point - the thing you start everything from should still be there after you have stopped everything. When you do want it gone, `-Status` prints the pid that owns the port.

### Autostart

There are **three** logon tasks, one per part, all put in place by [`-Install`](#install-and-uninstall). They are separate because their lifetimes are: the recorder is a headless service, the grid is a desktop artifact that needs an interactive session, and the UI is a local server that should outlive both. `cam.ps1 -Status` lists all three with their state and the launcher each one points at.

Each runs at logon as you, with an interactive, limited token. No elevation, no prompt. Reinstalling replaces the registration rather than adding a second one. The recorder's task has no execution time limit, because the default three day limit would otherwise kill a 24/7 supervisor on day three; it ignores a second instance, and it restarts up to three times at one minute intervals if it fails.

**The task opens the NVR window rather than running the supervisor directly, and that is not cosmetic.** Windows 11 hands every console created with a window to the default terminal application, which here is Windows Terminal, and with `windowingBehavior` set to `useExisting` the console arrives as a **tab in whichever terminal window already exists** - the Operator's. `-WindowStyle Hidden` does not prevent it, because the window belongs to Windows Terminal rather than to `powershell.exe`. Closing that window sent `CTRL_CLOSE_EVENT` to the supervisor, which PowerShell answers by terminating without running any `finally` block: no shutdown line in the log, and every ffmpeg plus MediaMTX left orphaned and still writing. It happened four times on 2026-10-12. Launching the supervisor as a pane in a window of its own removes the adoption, and the job object `cam.nvr` makes closing that window stop everything in it.

No task points at the repository directly. Each runs a small generated launcher in `%LOCALAPPDATA%\IPCCTV\` - the recorder's is `start-recorder.ps1`, which logs to `autostart.log` there and then forwards to `cam.recorder.ps1 -Nvr` - that waits for the repository's path to become reachable first. A repository kept inside WSL is reached over `\\wsl.localhost`, which does not resolve until WSL's file server is running, and a Scheduled Task has no way to wait for a path. The launchers hold no copy of the scripts, so they cannot drift from the repository.

### When something stops unexpectedly

Two job objects answer two different questions, and neither is a polling loop:

| Job | Owner | Fires when |
| --- | --- | --- |
| `cam.nvr` | the warden, outside the window | the **window** closes |
| `cam.supervisor.<hash of root>` | the supervisor itself | the **supervisor** dies, however abruptly |

A job with `KILL_ON_JOB_CLOSE` terminates every process in it when the last handle closes, and process exit closes a handle whatever killed the process. So a supervisor that crashes takes MediaMTX and every `ffmpeg` with it - measured twice on the live system, down to zero in under one second with no orphans. Jobs nest, so the supervisor being inside `cam.nvr` already is not an obstacle.

The NVR pane is not the supervisor. It is a **host** that runs the supervisor as a child and restarts it when it stops without being asked to. That split exists because from outside, closing the window and a crash look identical - supervisor gone, `status.json` still saying `running`, window destroyed - and guessing wrong either way is bad. With the split they are different events: closing the window gives both processes `CTRL_CLOSE_EVENT` and they die together, while a crash leaves the host and the window untouched and the child exiting is the signal. Both halves were proved on the live system: a killed supervisor was brought back by its host in seconds, with no window churn and no intervention.

Expected or not is read from what the supervisor recorded, never from an exit code alone, because a killed process has no say in its exit code. The stop flag, `StopKind` of `requested` or `interrupted`, and exit 20 all mean do not restart. The budget is five restarts in thirty minutes; out of budget it alerts once more and stays down rather than looping all night.

**Closing the NVR window is the one stop that nothing recovers from.** It raises its own alert - event id 103, and a Warning rather than an Error, because nothing malfunctioned: somebody closed the window, and the supervisor, MediaMTX, every `ffmpeg` and the viewers went with it, which is exactly what that window is for. **Nothing restarts them.** The logon task ran to completion when it opened the window, so its restart policy has nothing to restart, and the next automatic start is the next logon. Start it again with `.\cam.ps1 -Start`. `status.json` records `StopKind` `window-closed` and a `LastWindowClose` timestamp, so the UI and the log agree about what happened.

### Under the hood

To prevent quoting and bracket parsing errors when passing complex `mpv` arguments into Windows Terminal splits, the dashboard generates each pane's PowerShell command and converts it to a Base64 `-EncodedCommand` string before execution. The recorder and the health strip share `cam.common.ps1`, which owns configuration loading, the camera list, redaction, the status file readers and the layout algorithm, so the three scripts can never disagree about what they are looking at.

## Reference

* **Configuration** - [docs/configuration.md](docs/configuration.md). Four keys are editable from the UI's Settings screen: `SegmentSeconds`, `ReserveGB`, `PruneHysteresisGB` and `MaxRetentionDays`. The rest is tuning nobody should need. Add, remove or modify cameras by editing `cameras.json`, or from the same screen.
* **The Go toolchain** for building the UI - [docs/go-toolchain.md](docs/go-toolchain.md).
* **Development notes** - [docs/development.md](docs/development.md). Harness invocation and the dashboard's diagnostic switches.

### Everything at once: `cam.ps1`

The control surface. It starts, stops and reports every part of the system, including processes it did not start itself.

| Command | What it does |
| --- | --- |
| `.\cam.ps1` or `-Status` | What is running, in one view: the supervisor and its `ffmpeg` children, the NVR window and its job, the relay and the paths it serves, the dashboard panes and their `mpv`, the control UI and its address, all three logon tasks, and every process this project owns with its role and parent. Orphans are named as orphans. It reports processes and services, not disk. |
| `-Start` | Brings the recorder up, waits for the relay to serve, then opens the dashboard. That order matters: a pane that decides before the relay is serving takes a direct camera connection and burns the spare session. |
| `-Stop` | Stops everything and verifies that it did, orphans from a previous crash included. A no-op when nothing is running, safe to run twice, and silent on the alerting channels by design. It does not stop the control UI. |
| `-Start -Recorder` / `-Stop -Recorder` | The recorder, its NVR window and MediaMTX only. |
| `-Start -Dashboard` / `-Stop -Dashboard` | The viewer panes, their `mpv` and the health strip only. |
| `-Install` / `-Uninstall` | Installs or removes everything that starts at logon - [Install and uninstall](#install-and-uninstall), with the granular switches behind them. Add `-WhatIf` to see the plan. |

Nothing here is identified by image name or window title. Every process is classified by the work it owns, and the recording supervisor and its children sit on a keep list that every stop honours.

### The recorder: `cam.recorder.ps1`

| Command | What it does |
| --- | --- |
| `-Status` | Renders `status.json` as a report - including the disk figures and the days of headroom left - and says so when the file is stale. |
| `-Enable <name\|all>` | Sets `Record` to true for one camera or all of them. A running supervisor starts recording it within `ReconcileSeconds`. |
| `-Disable <name\|all>` | Sets `Record` to false. That child stops and is not restarted. |
| `-List` | Every camera with its `Record` state and stream quality. |
| `-Nvr` | Opens the NVR: one Windows Terminal window of its own, titled `cam.nvr`, running the supervisor as its only pane with the log on screen. Closing that window stops the supervisor, MediaMTX, every ffmpeg and the viewers. Closing any other terminal does nothing. |
| `-Run` | Supervises one `ffmpeg` per enabled camera, writes `status.json`, prunes the archive, and starts and owns MediaMTX. This is the long-running process, in the **foreground**. Use `-Nvr` instead unless you specifically want it in the console you are in, which then owns it - see [Autostart](#autostart). |
| `-Stop` | Asks a running supervisor to shut down, waits for it, and reports any `ffmpeg` or MediaMTX left behind. |
| `-SelfTest [-Offline] [-MaxConcurrency <n>]` | Pre-flight report: tools, per-camera RTSP probe, measured connection limit, crash-safety proof, disk projection. `-Offline` skips every check that touches a camera. |

### The dashboard: `mpv.cam.autolayout.autorestart.ps1`

| Command | What it does |
| --- | --- |
| `.\mpv.cam.autolayout.autorestart.ps1` | Opens the grid and the health strip. Right-click the script and choose "Run with PowerShell", or run it from a terminal. Close the dashboard by closing its Windows Terminal window. |

### The health strip: `cam.health.ps1`

| Command | What it does |
| --- | --- |
| `.\cam.health.ps1` | Fits itself to the console window it is in and refreshes in place. The dashboard launches it this way. |
| `-Rows <n>` `-Width <n>` | Render at an exact size instead of fitting the window. 0 means fit. |
| `-RefreshSeconds <n>` | How often to redraw. Default 2. |
| `-Once` | Render one frame as plain text and exit. |

### The control UI: `camui.exe`

`-Install` puts its logon task in place, and letting that run it is the normal way. These are for building it and for the rare direct run.

| Command | What it does |
| --- | --- |
| `ui/build.sh [output]` | Cross-compiles `camui.exe` into the repository root, or to `output` if you name one. A bash command, so it needs WSL or another machine with bash, and **Go 1.27 or newer** - `go.mod` asks for it and an apt-packaged Go will refuse the build; [docs/go-toolchain.md](docs/go-toolchain.md) is the one-time install, and the script also finds a toolchain at `~/.local/go/bin`. It finds its own directory, so the directory you run it from does not matter. It runs `gofmt`, `go vet` and `go test` first and stops on any of them, so a build that completes is a build that passed. |
| `.\camui.exe` | Runs it in the foreground on `http://127.0.0.1:8787/`, with its log on screen. Useful when you want to watch what it is doing; Ctrl+C stops it. |
| `-listen <host:port>` | Where to bind. Default `127.0.0.1:8787`. It **refuses to start** on any non-loopback address unless a token of at least 16 characters is configured. |
| `-config <path>` | A JSON config file: `listen`, `archiveRoot`, `statusFile`, `scriptDir`, `token`. The installed launcher passes `camui.config.json` from beside the binary if that file exists, and nothing else needs to change to use it. |
| `-archive-root <path>` `-status-file <path>` `-script-dir <path>` | Override what it reads. Precedence for the archive root is this flag or `archiveRoot` in the config file, then `RootPath` in the `recorder.config.json` beside the binary, then the built-in `E:\cameras` - so moving the archive needs no second edit here. The status file defaults to `<archive root>\_state\status.json` and the script directory to the directory holding the binary. |
| `-version` | Prints the build stamp and exits. |

The binary has to sit beside the scripts: with no `-script-dir` it looks for `cam.ps1`, `cameras.json` and `recorder.config.json` in its own folder.

### The test harness: `tests\cam.tests.ps1`

| Command | What it does |
| --- | --- |
| `.\tests\cam.tests.ps1` | Runs every check that touches no camera - `-Checks` lists their names. Safe with the dashboard open. Exit code 0 means nothing failed. |
| `-Checks <names>` | Run only these. |
| `-Live -Checks relay` | Adds the relay lifecycle proof, which starts MediaMTX and `ffmpeg` against the real cameras in a sandbox. It is the only check `-Live` unlocks. |

**A check that skips is not a check that failed, and how many skip depends on the machine.** Exit code 0 means no check failed, so read the tally rather than counting on a fixed split. Four can skip: `autostart` and `procs` while the real recorder is installed and running (limitation 12), and `leak` and `uileak` when there is no `camera_creds.xml`, because then there is no password to scan for and a clean scan would prove nothing. `uileak` also skips when `camui.exe` has not been built or will not start.

### Known limitations

These are measured, not theoretical. None of them is hidden in the code.

1. **Cameras serve exactly two concurrent RTSP sessions.** Measured on all six here and consistent with TP-Link's documentation, so it is firmware policy rather than a bug. The dashboard plus the recorder uses both slots, leaving nothing for the phone app, a browser preview or an ad-hoc `ffprobe`. The relay exists to turn those two slots into one.
2. **If the dashboard and the recorder are both already on direct inputs, MediaMTX can never pull.** Both slots are taken and there is no third for the relay. Restart either consumer and it will pick the relay up.
3. **MediaMTX path names must be lowercase alphanumeric.** Its environment variable key is cut at the first underscore, so a path named `workshop_entrance` silently creates a stray `workshop` path and leaves the real one unserved, with nothing logged. The generator lowercases names and strips non-alphanumerics for exactly this reason. Do not hand-edit the generated config.
4. **The camera password reaches ffmpeg, mpv and MediaMTX in the clear.** That is the floor: none of them has an out-of-band credential source, so the URL has to arrive somehow. MediaMTX gets it on its environment block, which is not visible to the process listings any user can read. A consumer on a **direct** camera input gets it inside the RTSP URL on its own command line, which is: that covers `ffmpeg` and `mpv` whenever the relay is not serving that camera, and the `ffprobe` and `ffmpeg` calls `-SelfTest` makes, which always address the cameras directly. Running the relay is what keeps everything else off that path. Nothing is written to disk, and every log line and console message is redacted to `rtsp://***:***@host/stream1`.
5. **Segment boundaries overshoot the clock mark by up to one GOP**, because a stream copy can only cut on a keyframe.
6. **Tapo cameras keep a separate account for RTSP**, distinct from the TP-Link cloud login. Changing the cloud password does not affect RTSP. To change the RTSP credential, delete `camera_creds.xml` and run the dashboard again.
7. **Some paths have never actually fired here:** log rotation at `LogMaxBytes`, the pruner's first deletion, the age cap `MaxRetentionDays`, the backoff reset at `HealthyResetSeconds` and the stall watchdog. They are implemented and unit-checked, not soak-tested. The production segment length is no longer on that list: real 15-minute segments have been measured under `ffprobe` and hold the 900 s mark to within a tenth of a second.
8. **Several tuning keys are absent from `recorder.config.example.json`.** They are marked * in [docs/configuration.md](docs/configuration.md); add them by hand if you want to change one.
9. **The test harness leaves its sandbox behind on purpose**, under `%TEMP%\cam-tests\<check>`, so you can inspect what a check did. The credential copy it makes is always deleted; footage a live check recorded is not. Delete the folder when you are done with it.
10. **Autostart depends on the recorder's path being reachable at logon.** If the repository lives inside WSL, the launcher polls `\\wsl.localhost` for up to 300 s and the task then retries up to three times, but nothing records while WSL is down. Keeping the repository on a local drive avoids the wait entirely. The launcher log at `%LOCALAPPDATA%\IPCCTV\autostart.log` is the first thing to read if autostart did not work.
11. **The footage player cannot seek ahead of what has arrived.** A clip is remuxed on demand and streamed, so the response carries `Accept-Ranges: none` and no length, and the browser can only seek inside what it has already buffered. Nothing is lost, it is just a wait: once the whole clip has arrived it is all seekable.
12. **Two harness checks always skip on a live NVR.** `autostart` refuses to run while a task named `IPCCTV-Recorder` is registered, because it would rewrite and then delete the real autostart, and `procs` refuses while the recorder is running, because the processes it hunts for already have a legitimate owner. On this machine both skip permanently. How many skip is not a constant: `leak` and `uileak` skip as well on a machine with no `camera_creds.xml`, and `uileak` skips if `camui.exe` is not there to start - so read the tally the harness prints.
13. **`status.json` reports configured relay paths, not ready ones.** The strip says `N path(s)` and tells relay from direct per camera in its `MODE` column, which is the honest signal; do not read the count as readiness.
