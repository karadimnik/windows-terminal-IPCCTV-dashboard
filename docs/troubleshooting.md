# Troubleshooting

**Holocron** (the Go system): run `holocron doctor --quiet` first. It checks recording, live view, connections, the web app, the disk and the processes, and prints a `fix:` under every problem ([health check](holocron-cheatsheet.md#health-check)). The first table below is the PowerShell system's; [Holocron: archive layout](#holocron-archive-layout) covers the spool and day folders.

## Something is wrong

| Symptom | Likely cause | What to do |
| --- | --- | --- |
| **Nothing is recording and nothing is coming back.** | The NVR window was closed. The window titled `cam.nvr` **is** the NVR: closing it stops the supervisor, MediaMTX, every `ffmpeg` and the viewers, and **nothing restarts them**. You get one alert, event id 103. | `.\cam.ps1 -Start` |
| **The strip says the supervisor is stale or missing.** | `status.json` is older than three status intervals, which means the supervisor is not running. | `.\cam.ps1 -Start`, or `.\cam.recorder.ps1 -Nvr` for the recorder alone, then read `_logs\recorder.log` for why it stopped. Do **not** start it with `-Run` from an ordinary terminal - see [Autostart](how-it-works.md#autostart) for why. |
| **The supervisor crashed and came back by itself.** | Not a fault. The NVR pane is a host that restarts it, alerting as it goes (id 100) and again once it is recording (id 101), up to five times in thirty minutes. | Nothing. Out of budget it alerts once more (id 102) and stays down rather than looping all night; then `.\cam.ps1 -Start` after reading the log. |
| **One camera has gone quiet.** | It has made no file progress for max(`CameraDownAlertMinutes`, one segment + one status tick + 30 s) - see [Alerts](alerts.md) - so it alerted under id 110 and has a `DownSince` in `status.json`. It is judged on file progress alone: a camera that cannot be reached has its `ffmpeg` restarted every backoff period, so the process being alive says nothing. | Check the camera and the network. The supervisor keeps retrying on its own. |
| **The UI will not open.** | Either you are asking from inside WSL (see [Day to day](../README.md#day-to-day)), or `camui.exe` was never built, so `-Install` skipped the UI - see [Install and uninstall](../README.md#install-and-uninstall). | Open it in a Windows browser, or ask from Windows PowerShell. `.\cam.ps1 -Status` prints whether anything is serving `127.0.0.1:8787` and which pid owns the port. If the task ran and nothing is serving, `%LOCALAPPDATA%\IPCCTV\autostart-ui.log` says how far the launcher got and `camui.log` beside it holds camui's own output. |
| **`cam.ps1 -Stop` was silent.** | On purpose. An orderly stop is not a fault, so it raises no toast and writes no event. | Nothing. It still prints the component counts before and after and exits non-zero if anything survived. |
| **Everything records to direct inputs and the relay never engages.** | Both camera slots are already taken, so MediaMTX has no third one to pull with - [limitation 2](limitations.md). | Restart the dashboard or the recorder; whichever restarts picks the relay up. |
| **Terminal flashes and closes immediately.** | `$MpvPath` points at `mpv.exe`. | Point it at `mpv.com`, the command-line executable. |
| **Streams will not connect.** | Wrong IP, or a camera whose URL shape is not `rtsp://user:pass@IP/<Quality>`. | Check the IPs in `cameras.json`. `Get-CamRtspUrl` in `cam.common.ps1` is the one place that builds the URL, for the dashboard and the recorder alike. |
| **`-SelfTest` reports a concurrency of 1 and the cameras look broken.** | The dashboard is open and holding a slot. | Close it, or run `-SelfTest -Offline`, which skips every camera-facing check. |
| **MediaMTX exits immediately with `ReadDirectoryChanges: Incorrect function`.** | `RestreamerConfigPath` is on the WSL share, which does not implement it. | Point it at a local NTFS path such as `C:\mediamtx\mediamtx.yml`. |
| **Autostart did nothing at logon.** | Usually the repository's path was not reachable yet - [limitation 10](limitations.md). | Read `%LOCALAPPDATA%\IPCCTV\autostart.log`, and confirm the task with `schtasks /query /tn IPCCTV-Recorder /fo list`. |
| **The camera password needs changing.** | - | Delete `camera_creds.xml` and run the dashboard again for a fresh prompt. Tapo cameras keep a separate RTSP account from the TP-Link cloud login; changing the cloud password does nothing here. |

## Holocron: archive layout

Holocron keeps `<root>\<Camera>\` for the segment being written and the newest closed one, and files the rest into `<root>\<Camera>\YYYY-MM-DD\` ([how it works](how-it-works.md#the-mover)). Three things go wrong there; `holocron doctor` (group `storage`) names each, with the `fix:` line quoted below.

| Symptom | Cause | What to do |
| --- | --- | --- |
| `warn spool backlog`: `the mover is not keeping up ...: more than 4 segments in the spool of <camera> N` (`storage.backlog`) | The mover is not filing. The archive is still read and pruned, but the camera folder grows again. Reasons are in the log: the supervisor is not running, the mover is refused (no valid ownership marker), a conflict (`not filed <path>: the destination ... already exists; neither file was touched`), or a file another program holds open (busy, retried each cycle, debug level). With `MoveToDayFolders` false the check is `info`, not a warning. | `fix: holocron logs --component prune --since 1h (action archive.file names the reason) ; holocron status (is the supervisor running?)`. For a conflict compare the two files and keep one; `holocron archive migrate --now` files what is left and reports it. |
| `warn unseen files`: `N file(s) the pruner cannot see, so it never deletes them and they fill the disk` (`storage.unseen`); or a camera line in `storage.cameras` saying `N segment(s) are MISFILED` | A segment is in a day folder that is not the date in its name, there is something else in a day folder (a foreign file, another camera's segment), or a segment-like name sits deeper than a day folder or in a folder that is not a day. None of them is footage, so none is deleted or counted. | `fix: holocron archive stats ; move each segment into E:\cameras\<camera> or its YYYY-MM-DD day folder, and move anything else out of the camera folders` (your root in place of `E:\cameras`). For `storage.cameras` the fix is `holocron archive stats ; move each to the YYYY-MM-DD folder of its name in E:\cameras\<camera>`. Move by hand only these strays; never write into a day folder otherwise. |
| `holocron archive migrate --now` lists files as `in use by another program` (busy), `changed or gone`, or ends `INCOMPLETE, see above` | Windows: another process holds the file open, so the rename was refused (a sharing violation). Nothing was lost; the file stays in the camera folder. | Close whatever holds it and run `holocron archive migrate --now` again (or wait: the mover retries every cycle). A `CONFLICT` or `ERROR` (exit 1) names the files: compare, keep one, run again. |
| `holocron archive flatten` says `REFUSED: the recorder is running: stop it before putting the day folders back into the spool (...)` | Holocron (supervisor, control endpoint or host process) still runs, and its mover would race the files coming back. | `fix: holocron down`, then `holocron archive flatten --now`. |

The PowerShell system sees none of this: it lists only the files directly in a camera folder, so it never shows or prunes a filed day. Before it runs again, `holocron archive flatten --now` ([rollback R2a](rollback.md#r2a-put-the-archive-back-flat)). Windows Explorer shows the day folders as ordinary folders inside each camera folder.

## When something stops unexpectedly

Two job objects answer two different questions, and neither is a polling loop:

| Job | Owner | Fires when |
| --- | --- | --- |
| `cam.nvr` | the warden, outside the window | the **window** closes |
| `cam.supervisor.<hash of root>` | the supervisor itself | the **supervisor** dies, however abruptly |

A job with `KILL_ON_JOB_CLOSE` terminates every process in it when the last handle closes, and process exit closes a handle whatever killed the process. So a supervisor that crashes takes MediaMTX and every `ffmpeg` with it - measured twice on the live system, down to zero in under one second with no orphans. Jobs nest, so the supervisor being inside `cam.nvr` already is not an obstacle.

The NVR pane is not the supervisor. It is a **host** that runs the supervisor as a child and restarts it when it stops without being asked to. That split exists because from outside, closing the window and a crash look identical - supervisor gone, `status.json` still saying `running`, window destroyed - and guessing wrong either way is bad. With the split they are different events: closing the window gives both processes `CTRL_CLOSE_EVENT` and they die together, while a crash leaves the host and the window untouched and the child exiting is the signal. Both halves were proved on the live system: a killed supervisor was brought back by its host in seconds, with no window churn and no intervention.

Expected or not is read from what the supervisor recorded, never from an exit code alone, because a killed process has no say in its exit code. The stop flag, `StopKind` of `requested` or `interrupted`, and exit 20 all mean do not restart. The budget is five restarts in thirty minutes; out of budget it alerts once more and stays down rather than looping all night.

**Closing the NVR window is the one stop that nothing recovers from.** It raises its own alert - event id 103, and a Warning rather than an Error, because nothing malfunctioned: somebody closed the window, and the supervisor, MediaMTX, every `ffmpeg` and the viewers went with it, which is exactly what that window is for. **Nothing restarts them.** The logon task ran to completion when it opened the window, so its restart policy has nothing to restart, and the next automatic start is the next logon. Start it again with `.\cam.ps1 -Start`. `status.json` records `StopKind` `window-closed` and a `LastWindowClose` timestamp, so the UI and the log agree about what happened.
