# How it works

## Credentials

No hardcoded passwords. The first run prompts for the camera credentials and encrypts them to a local XML file that only your Windows account can decrypt. The password is never written to a log and never written to the generated relay config, and on the relay path it reaches no command line at all, because consumers read `rtsp://127.0.0.1:8554/<path>`. A consumer that falls back to a direct camera input is the exception: `ffmpeg` and `mpv` then take the credential inside the RTSP URL on their own command line, as do the probes `-SelfTest` makes. That is the floor rather than an oversight - see [limitation 4](limitations.md).

## The grid

The column and row counts come from the camera count alone, so six cameras are a 3x2 grid whether the health strip is there or not. Pane size follows from the screen width and the video aspect, and an 8 px gutter separates the grid rows and the strip. There is deliberately no gutter at the screen edges: three 640 px panes fill a 1920 px screen exactly, so an edge gutter could only come out of the picture.

Each pane runs its own `mpv` in a `while` loop. When a stream drops, that loop prints `Stream interrupted. Restarting in 3 seconds...` and reconnects. The camera URL is built inside the pane rather than passed into it, which keeps the credential out of the Windows Terminal and PowerShell command lines it would otherwise appear in. A pane on the relay then hands `mpv` a loopback URL with no credential in it at all; a pane that had to take a direct input hands it the real one, which is on `mpv`'s own command line for as long as that pane stays direct - [limitation 4](limitations.md).

Two timeouts are what make a frozen stream exit instead of hanging forever: `--demuxer-lavf-o=timeout=5000000`, a five second socket timeout passed to libavformat's RTSP demuxer, and mpv's own `--network-timeout=3`. If you have seen `stimeout` recommended for this, note that libavformat renamed it to `timeout` at ffmpeg 5.0 and mpv ships a newer libavformat than that, so `stimeout` is silently dropped and protects nothing. `--msg-level=all=error` keeps mpv's routine chatter out of the gutter, where the terminal window behind the feeds shows through, while still letting real errors print.

## Recording

Recording is opt-in per camera. The supervisor keeps exactly one `ffmpeg` child per enabled camera and reconciles against `cameras.json` every `ReconcileSeconds`, so enabling, disabling or changing a camera's address takes effect without a restart. A child that exits is restarted with backoff, and one whose segment file stops growing for `StallFactor` x `SegmentSeconds` is killed and restarted by the watchdog. Only one supervisor can run at a time.

Streams are copied, never re-encoded, so recording costs almost no CPU and the footage is exactly what the camera sent. Segments are 15 minutes and aligned to the clock, give or take: a stream copy can only cut on a keyframe, so each boundary overshoots the mark by up to one GOP.

Footage lands in flat per-camera folders:

```
E:\cameras\
  Front_Entrance\Front_Entrance_20261011_223649+0300.mkv
  Rear_Balcony\  Rear_Balcony_20261011_223649+0300.mkv
  _state\status.json        what the health strip and -Status read
  _logs\recorder.log        the supervisor log, rotated at LogMaxBytes; a reader holding it
                            open only delays the rotation (logged once), never recording
  _logs\restreamer.log      MediaMTX output, redacted
  _logs\<Camera>.log        per-camera ffmpeg output, relayed by the supervisor, redacted,
                            rotated the same way
```

Two details in there are not preferences:

* **MKV, not MP4.** All six of these cameras emit `pcm_alaw` audio, and MP4 refuses `pcm_alaw` with stream copy. MKV is also the only container here that survives a crash: a file that was being written when the power went out still plays.
* **The `+0300` suffix.** ffmpeg's `strftime` formatting is local time only and ignores `TZ` on Windows, so without the UTC offset in the name the repeated hour at the end of daylight saving would silently overwrite the first one.

## Retention

The pruner runs every `PruneIntervalSeconds` and watches free space rather than age. When free space falls below `ReserveGB` it deletes oldest-first until `ReserveGB + PruneHysteresisGB` is free, which stops it thrashing at the boundary. It never deletes the segment being written. If nothing deletable is left and free space is still below `ReserveGB` it alerts once (id 121) and keeps recording. Set `MaxRetentionDays` if you also want an age cap; a run that would delete more than `MaxAgePruneFraction` (default half) of the archive by age deletes nothing by age and alerts once (id 122), so a mistyped retention cannot empty it.

**Do not plan from a number in a document.** The rate footage accrues at depends on how many cameras are up, which stream each one is on and how much the scenes move, so it drifts with the system rather than sitting still. `.\cam.recorder.ps1 -Status` and the health strip both print the rate measured over the archive as it stands, with the days of headroom left - that figure is the one to plan from, because it is the only one that is current.

## Transport: the relay

These cameras allow very few simultaneous RTSP sessions, which is the whole reason MediaMTX is here. With the relay running, each camera is pulled exactly once and MediaMTX serves both the dashboard and the recorder from that one session. The supervisor starts MediaMTX as its own child, so there is one thing to autostart rather than two, and it waits for the relay's paths to come up before starting the recorders.

The choice is made per start attempt, per consumer: if the relay is serving that camera it is used, and if it is not, a direct camera connection is used instead. A relay that dies is left behind at the next restart and one that comes back is picked up without relaunching anything. The health strip's `MODE` column tells you which input each recorder is actually on.

A consumer that had to take a direct connection gives the camera slot back on its own once the relay is serving that camera again. A recorder child is stopped and restarted by its supervisor; an `mpv` pane watches its own path every `ViewerPromoteCheckSeconds` and restarts itself onto the relay, which costs one brief blink in that pane, once per outage. Only a pane on a direct connection watches anything - a pane on the relay needs no watch, because if the relay dies `mpv` loses its input and the pane decides again by itself.

The generated `mediamtx.yml` contains no password. Every path is left as `source: publisher` in the file and the supervisor supplies the real camera URL on the child's environment block at launch.

## The health strip

The strip at the bottom of the screen is a Windows Terminal window that the dashboard places by pixel and fills with `cam.health.ps1`. It reads one file, `_state\status.json`, and nothing else, so it can never be the thing that costs you a camera slot. It renders four sections, which you can reorder or switch off individually with `HealthSections`: `status` (supervisor state and uptime, relay state, free space, days left), `cameras` (per camera: record flag, child alive, relay or direct, time since the last closed segment, restarts in the last hour, last error), `events` (the supervisor's most recent log lines, newest first) and `archive` (the span and size of the footage, the measured daily rate, and what the pruner has deleted). Set `HealthStripHeightPx` to 0 to switch the strip off entirely and get the original full-height grid back. The strip does not quite reach the screen's left and right edges, where a few pixels of desktop show: that cannot be fixed while the strip is a Windows Terminal window, because covering the full width needs a window rect wider than the screen and that is exactly what makes Windows Terminal mis-size itself. Top and bottom are exact and no video is affected.

## Autostart

There are **three** logon tasks, one per part, all put in place by [`-Install`](../README.md#install-and-uninstall). They are separate because their lifetimes are: the recorder is a headless service, the grid is a desktop artifact that needs an interactive session, and the UI is a local server that should outlive both. `cam.ps1 -Status` lists all three with their state and the launcher each one points at.

Each runs at logon as you, with an interactive, limited token. No elevation, no prompt. Reinstalling replaces the registration rather than adding a second one. The recorder's task has no execution time limit, because the default three day limit would otherwise kill a 24/7 supervisor on day three; it ignores a second instance, and it restarts up to three times at one minute intervals if it fails.

**The task opens the NVR window rather than running the supervisor directly, and that is not cosmetic.** Windows 11 hands every console created with a window to the default terminal application, which here is Windows Terminal, and with `windowingBehavior` set to `useExisting` the console arrives as a **tab in whichever terminal window already exists** - the Operator's. `-WindowStyle Hidden` does not prevent it, because the window belongs to Windows Terminal rather than to `powershell.exe`. Closing that window sent `CTRL_CLOSE_EVENT` to the supervisor, which PowerShell answers by terminating without running any `finally` block: no shutdown line in the log, and every ffmpeg plus MediaMTX left orphaned and still writing. It happened four times on 2026-10-12. Launching the supervisor as a pane in a window of its own removes the adoption, and the job object `cam.nvr` makes closing that window stop everything in it.

No task points at the repository directly. Each runs a small generated launcher in `%LOCALAPPDATA%\IPCCTV\` - the recorder's is `start-recorder.ps1`, which logs to `autostart.log` there and then forwards to `cam.recorder.ps1 -Nvr` - that waits for the repository's path to become reachable first. A repository kept inside WSL is reached over `\\wsl.localhost`, which does not resolve until WSL's file server is running, and a Scheduled Task has no way to wait for a path. The launchers hold no copy of the scripts, so they cannot drift from the repository.

## Under the hood

To prevent quoting and bracket parsing errors when passing complex `mpv` arguments into Windows Terminal splits, the dashboard generates each pane's PowerShell command and converts it to a Base64 `-EncodedCommand` string before execution. The recorder and the health strip share `cam.common.ps1`, which owns configuration loading, the camera list, redaction, the status file readers and the layout algorithm, so the three scripts can never disagree about what they are looking at.
