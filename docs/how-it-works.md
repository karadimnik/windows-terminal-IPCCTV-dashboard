# How it works

## Credentials

No hardcoded passwords. The first run prompts for the camera credentials and encrypts them to a local XML file that only your Windows account can decrypt. The password is never written to a log and never written to the generated relay config, and on the relay path it reaches no command line at all, because consumers read `rtsp://127.0.0.1:8554/<path>`. A consumer that falls back to a direct camera input is the exception: `ffmpeg` then takes the credential inside the RTSP URL on its own command line, as do the probes `-SelfTest` makes. `mpv` no longer does: a viewer pane starts it idle and hands it the URL over a private control pipe, so the password is never on `mpv`'s command line. The rest is the floor rather than an oversight - see [limitation 4](limitations.md).

## The grid

The column and row counts come from the camera count alone, so six cameras are a 3x2 grid whether the health strip is there or not. Pane size follows from the screen width and the video aspect, and an 8 px gutter separates the grid rows and the strip. There is deliberately no gutter at the screen edges: three 640 px panes fill a 1920 px screen exactly, so an edge gutter could only come out of the picture.

Each pane runs its own `mpv` in a `while` loop. When a stream drops, that loop prints `Stream interrupted. Restarting in 3 seconds...` and reconnects. The camera URL is built inside the pane rather than passed into it, which keeps the credential out of the Windows Terminal and PowerShell command lines it would otherwise appear in. A pane on the relay then hands `mpv` a loopback URL with no credential in it at all; a pane that had to take a direct input hands it the real one over a private control pipe once `mpv` is running - never on its command line - and closes that pipe straight afterwards, so nothing can reconnect to read it back. If the hand-over fails the pane stays black and retries; it never falls back to the command line.

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
                            open only delays the rotation (logged once), never recording;
                            one that blocks writing loses those lines, counted in one
                            "log write failed: N line(s) dropped" notice afterwards
  _logs\restreamer.log      MediaMTX output, redacted
  _logs\<Camera>.log        per-camera ffmpeg output, relayed by the supervisor, redacted,
                            rotated the same way
```

Holocron also reads day folders inside a camera folder (decision D20): `Front_Entrance\2026-10-26\Front_Entrance_20261026_...mkv`, the folder being the local date written in the file names, so the repeated hour of the clock change stays in one folder with both offsets. ffmpeg keeps writing into the camera folder itself (the spool); [the mover](#the-mover) files the closed segments into the day folders. The pruner already reads them: a filed segment is deleted in the same oldest-first order, under the same gates, as it would be in the camera folder, and nothing in a day folder that is not footage is ever deleted. A day folder it empties, or finds empty, is removed once its date is past. Every reader goes through one library: the Footage tab, `holocron archive stats`, `archive compare`, the doctor and the disk figures read the spool and every day folder; the status tick, alert 110 and the stall watchdog read only the camera folder while it holds the open and the last closed segment, one listing per camera however many days are filed. A file under a day folder that is not the date in its name is misfiled: it is not footage, and `archive stats` and the doctor report it. A folder named like a day that is not a real date, a link or a junction is never entered.

Three details in there are not preferences:

* **MKV, not MP4.** All six of these cameras emit `pcm_alaw` audio, and MP4 refuses `pcm_alaw` with stream copy. MKV is also the only container here that survives a crash: a file that was being written when the power went out still plays.
* **The `+0300` suffix.** ffmpeg's `strftime` formatting is local time only and ignores `TZ` on Windows, so without the UTC offset in the name the repeated hour at the end of daylight saving would silently overwrite the first one.
* **Every interval is measured in UTC.** `DateTime` subtraction ignores the offset, so a supervisor timed on local time loses an hour when daylight saving ends - every timer stalls, `status.json` goes stale and the watchdog goes blind - and gains one when it starts, when the watchdog kills every healthy child and alert 110 fires for every camera. Every timer, age and deadline reads one UTC clock and UTC file times instead. Local time is only ever shown: log stamps, segment names, and the timestamps in `status.json`, which carry their offset (`+03:00`) and are read back as exact instants.

## Retention

The pruner runs every `PruneIntervalSeconds` and watches free space rather than age. When free space falls below `ReserveGB` it deletes oldest-first until `ReserveGB + PruneHysteresisGB` is free, which stops it thrashing at the boundary. It never deletes the segment being written. If nothing deletable is left and free space is still below `ReserveGB` it alerts once (id 121) and keeps recording. Set `MaxRetentionDays` if you also want an age cap; a run that would delete more than `MaxAgePruneFraction` (default half) of the archive by age deletes nothing by age and alerts once (id 122), so a mistyped retention cannot empty it.

**Do not plan from a number in a document.** The rate footage accrues at depends on how many cameras are up, which stream each one is on and how much the scenes move, so it drifts with the system rather than sitting still. `.\cam.recorder.ps1 -Status` and the health strip both print the rate measured over the archive as it stands, with the days of headroom left - that figure is the one to plan from, because it is the only one that is current.

## The mover

ffmpeg cannot create folders, so it writes every segment flat into the camera folder (the spool). The mover, a loop of the supervisor that runs every `MoveIntervalSeconds` (60) once the supervisor has claimed the root, files each closed segment into the day folder of the date in its name: `Front_Entrance\Front_Entrance_20261026_101500+0200.mkv` becomes `Front_Entrance\2026-10-26\Front_Entrance_20261026_101500+0200.mkv`. The camera folder keeps only the segment being written, the newest closed one and one younger than the margin.

What it moves: a plain file (never a link, junction or folder) named as a segment of that camera, that is not one of the two newest segments of the camera folder, and whose last write is at least `MoveMarginSeconds` (120) old. A zero-byte segment is filed like any other. Oldest first, at most `MoveMaxPerCycle` (200) per camera per cycle, so a first run over a large flat archive spreads its disk work over a few minutes. A day folder is created inside the camera folder, never anywhere else. Each file is checked again just before it moves (still a plain file, same size and write time, its folders not links), and the move is a rename on the same disk, which never replaces a file: if the day folder already holds that name, both files stay as they are and the log says so on every cycle until someone resolves it.

Failure is harmless: a segment that does not move stays in the camera folder, where the Footage tab, the status and the pruner see it exactly as before. On Windows a file another program holds open is retried on the next cycle; a cycle that only met such files or errors waits twice as long, up to ten intervals. A rename either happened or did not, so a crash or power cut mid-cycle leaves nothing to repair. The mover and the pruner never run at the same moment. The log lines are action `archive.file` (component prune): info when a cycle moved something, warnings for conflicts and errors. `holocron doctor` warns (`storage.backlog`) when a camera folder holds more than 4 segments once the supervisor has run for 15 minutes.

`MoveToDayFolders` false switches the mover off at the next reconcile, without a restart; day folders already filled stay and are still shown and pruned. `holocron archive migrate` files a whole flat archive at once by the same rules (a dry run first; `--now` applies, and is safe while the recorder runs: both movers rename without replacing, and a file the other moved first is skipped, not an error); `holocron archive flatten` puts every filed segment back into its camera folder for the PowerShell system, and refuses while Holocron runs.

## Transport: the relay

These cameras allow very few simultaneous RTSP sessions, which is the whole reason MediaMTX is here. With the relay running, each camera is pulled exactly once and MediaMTX serves both the dashboard and the recorder from that one session. The supervisor starts MediaMTX as its own child, so there is one thing to autostart rather than two, and it waits for the relay's paths to come up before starting the recorders.

The choice is made per start attempt, per consumer: if the relay is serving that camera it is used, and if it is not, a direct camera connection is used instead. A relay that dies is left behind at the next restart and one that comes back is picked up without relaunching anything. The health strip's `MODE` column tells you which input each recorder is actually on.

A consumer that had to take a direct connection gives the camera slot back on its own once the relay is serving that camera again. A recorder child is stopped and restarted by its supervisor; an `mpv` pane watches its own path every `ViewerPromoteCheckSeconds` and restarts itself onto the relay, which costs one brief blink in that pane, once per outage. Only a pane on a direct connection watches anything - a pane on the relay needs no watch, because if the relay dies `mpv` loses its input and the pane decides again by itself.

The generated `mediamtx.yml` contains no password. Every path is left as `source: publisher` in the file and the supervisor supplies the real camera URL on the child's environment block at launch.

The relay's control API, which the consumers ask whether a path is serving, needs a login. The product generates one, a random secret it keeps DPAPI-protected in `relay_api_creds.xml` beside the scripts, and `mediamtx.yml` holds only its SHA-256; every caller sends it in-process, never on a command line. Reading and publishing RTSP on loopback still needs no login. An install from before the login upgrades itself: the next restreamer start creates the login if it is missing and rewrites `mediamtx.yml` when its login block is missing or not the stored one.

**A refused login is not a dead relay.** If the API answers 401 the relay is still running and serving, so every consumer is kept on the relay - a refused login could otherwise send every recorder and pane to a direct camera input, with the password on its command line and the camera's second session gone. What is lost is only the view of which paths are serving: a camera whose path has no stream is retried on the relay at each restart instead of falling back to direct, until the login is fixed. The supervisor raises alert 130 once per episode and logs when the login is accepted again; `cam.ps1 -Status` shows the refusal instead of a path count. Restarting with `cam.ps1 -Stop` and `-Start` fixes it, because the restreamer start rewrites the config to the stored login.

## The health strip

The strip at the bottom of the screen is a Windows Terminal window that the dashboard places by pixel and fills with `cam.health.ps1`. It reads one file, `_state\status.json`, and nothing else, so it can never be the thing that costs you a camera slot. It renders four sections, which you can reorder or switch off individually with `HealthSections`: `status` (supervisor state and uptime, relay state, free space, days left), `cameras` (per camera: record flag, child alive, relay or direct, time since the last closed segment, restarts in the last hour, last error), `events` (the supervisor's most recent log lines, newest first) and `archive` (the span and size of the footage, the measured daily rate, and what the pruner has deleted). Set `HealthStripHeightPx` to 0 to switch the strip off entirely and get the original full-height grid back. The strip does not quite reach the screen's left and right edges, where a few pixels of desktop show: that cannot be fixed while the strip is a Windows Terminal window, because covering the full width needs a window rect wider than the screen and that is exactly what makes Windows Terminal mis-size itself. Top and bottom are exact and no video is affected.

## Autostart

There are **three** logon tasks, one per part, all put in place by [`-Install`](../README.md#install-and-uninstall). They are separate because their lifetimes are: the recorder is a headless service, the grid is a desktop artifact that needs an interactive session, and the UI is a local server that should outlive both. `cam.ps1 -Status` lists all three with their state and the launcher each one points at.

Each runs at logon as you, with an interactive, limited token. No elevation, no prompt. Reinstalling replaces the registration rather than adding a second one. The recorder's task has no execution time limit, because the default three day limit would otherwise kill a 24/7 supervisor on day three; it ignores a second instance, and it restarts up to three times at one minute intervals if it fails.

**The task opens the NVR window rather than running the supervisor directly, and that is not cosmetic.** Windows 11 hands every console created with a window to the default terminal application, which here is Windows Terminal, and with `windowingBehavior` set to `useExisting` the console arrives as a **tab in whichever terminal window already exists** - the Operator's. `-WindowStyle Hidden` does not prevent it, because the window belongs to Windows Terminal rather than to `powershell.exe`. Closing that window sent `CTRL_CLOSE_EVENT` to the supervisor, which PowerShell answers by terminating without running any `finally` block: no shutdown line in the log, and every ffmpeg plus MediaMTX left orphaned and still writing. It happened four times on 2026-10-12. Launching the supervisor as a pane in a window of its own removes the adoption, and the job object `cam.nvr` makes closing that window stop everything in it.

No task points at the repository directly. Each runs a small generated launcher in `%LOCALAPPDATA%\IPCCTV\` - the recorder's is `start-recorder.ps1`, which logs to `autostart.log` there and then forwards to `cam.recorder.ps1 -Nvr` - that waits for the repository's path to become reachable first. A repository kept inside WSL is reached over `\\wsl.localhost`, which does not resolve until WSL's file server is running, and a Scheduled Task has no way to wait for a path. The launchers hold no copy of the scripts, so they cannot drift from the repository.

## Under the hood

To prevent quoting and bracket parsing errors when passing complex `mpv` arguments into Windows Terminal splits, the dashboard generates each pane's PowerShell command and converts it to a Base64 `-EncodedCommand` string before execution. The recorder and the health strip share `cam.common.ps1`, which owns configuration loading, the camera list, redaction, the status file readers and the layout algorithm, so the three scripts can never disagree about what they are looking at.
