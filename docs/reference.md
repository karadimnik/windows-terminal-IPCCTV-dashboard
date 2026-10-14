# Command reference

Every command, by script. The control UI's own commands are in [ui.md](ui.md#the-commands-camuiexe), and the configuration keys in [configuration.md](configuration.md).

## Everything at once: `cam.ps1`

The control surface. It starts, stops and reports every part of the system, including processes it did not start itself.

| Command | What it does |
| --- | --- |
| `.\cam.ps1` or `-Status` | What is running, in one view: the supervisor and its `ffmpeg` children, the NVR window and its job, the relay and the paths it serves, the dashboard panes and their `mpv`, the control UI and its address, all three logon tasks, and every process this project owns with its role and parent. Orphans are named as orphans. It reports processes and services, not disk. |
| `-Start` | Brings the recorder up, waits for the relay to serve, then opens the dashboard. That order matters: a pane that decides before the relay is serving takes a direct camera connection and burns the spare session. |
| `-Stop` | Stops everything and verifies that it did, orphans from a previous crash included. A no-op when nothing is running, safe to run twice, and silent on the alerting channels by design. It does not stop the control UI. |
| `-Start -Recorder` / `-Stop -Recorder` | The recorder, its NVR window and MediaMTX only. |
| `-Start -Dashboard` / `-Stop -Dashboard` | The viewer panes, their `mpv` and the health strip only. |
| `-Install` / `-Uninstall` | Installs or removes everything that starts at logon - [Install and uninstall](../README.md#install-and-uninstall), with the granular switches behind them. Add `-WhatIf` to see the plan. |

Nothing here is identified by image name or window title. Every process is classified by the work it owns, and the recording supervisor and its children sit on a keep list that every stop honours.

## The granular switches

`-Install` and `-Uninstall` are made of these. They exist for scripts and recovery: each installs or removes one component and touches nothing else. All are unelevated except the event source, idempotent, a clean no-op when there is nothing to do, and every removal keeps its launcher's log.

| Command | What it does |
| --- | --- |
| `.\cam.recorder.ps1 -InstallRestreamer` / `-RemoveRestreamer` | Generates the MediaMTX config from `cameras.json`, one path per camera and no credential, and checks the binary is in place; or deletes it, and recording and viewing continue on the direct camera inputs. |
| `.\cam.recorder.ps1 -InstallAutostart` / `-RemoveAutostart` | Registers or removes `IPCCTV-Recorder` and its launcher. A supervisor already running keeps running. |
| `.\cam.ps1 -InstallDashboardAutostart` / `-RemoveDashboardAutostart` | Registers or removes `IPCCTV-Dashboard` and its launcher. A dashboard that is open stays open. |
| `.\cam.ps1 -InstallUi` / `-RemoveUi` | Registers or removes `IPCCTV-Ui` and its launcher. It refuses to install when `camui.exe` has not been built, and it does not start the UI - it prints `Start-ScheduledTask -TaskName IPCCTV-Ui`, which does. A `camui` that is running stays running, and the removal prints the pid serving it. |
| `.\cam.recorder.ps1 -InstallEventSource` | Registers the `IPCCTV` Application-log event source. Needs an elevated session, once, and is optional. |

## The recorder: `cam.recorder.ps1`

| Command | What it does |
| --- | --- |
| `-Status` | Renders `status.json` as a report - including the disk figures and the days of headroom left - and says so when the file is stale. |
| `-Enable <name\|all>` | Sets `Record` to true for one camera or all of them. A running supervisor starts recording it within `ReconcileSeconds`. |
| `-Disable <name\|all>` | Sets `Record` to false. That child stops and is not restarted. |
| `-List` | Every camera with its `Record` state and stream quality. |
| `-Nvr` | Opens the NVR: one Windows Terminal window of its own, titled `cam.nvr`, running the supervisor as its only pane with the log on screen. Closing that window stops the supervisor, MediaMTX, every ffmpeg and the viewers. Closing any other terminal does nothing. |
| `-Run` | Supervises one `ffmpeg` per enabled camera, writes `status.json`, prunes the archive, and starts and owns MediaMTX. This is the long-running process, in the **foreground**. Use `-Nvr` instead unless you specifically want it in the console you are in, which then owns it - see [Autostart](how-it-works.md#autostart). |
| `-Stop` | Asks a running supervisor to shut down, waits for it, and reports any `ffmpeg` or MediaMTX left behind. |
| `-SelfTest [-Offline] [-MaxConcurrency <n>]` | Pre-flight report: tools, per-camera RTSP probe, measured connection limit, crash-safety proof, disk projection. `-Offline` skips every check that touches a camera. |

## The dashboard: `mpv.cam.autolayout.autorestart.ps1`

| Command | What it does |
| --- | --- |
| `.\mpv.cam.autolayout.autorestart.ps1` | Opens the grid and the health strip. Right-click the script and choose "Run with PowerShell", or run it from a terminal. Close the dashboard by closing its Windows Terminal window. |

## The health strip: `cam.health.ps1`

| Command | What it does |
| --- | --- |
| `.\cam.health.ps1` | Fits itself to the console window it is in and refreshes in place. The dashboard launches it this way. |
| `-Rows <n>` `-Width <n>` | Render at an exact size instead of fitting the window. 0 means fit. |
| `-RefreshSeconds <n>` | How often to redraw. Default 2. |
| `-Once` | Render one frame as plain text and exit. |

## The test harness: `tests\cam.tests.ps1`

| Command | What it does |
| --- | --- |
| `.\tests\cam.tests.ps1` | Runs every check that touches no camera - `-Checks` lists their names. Safe with the dashboard open. Exit code 0 means nothing failed. |
| `-Checks <names>` | Run only these. |
| `-Live -Checks relay` | Adds the relay lifecycle proof, which starts MediaMTX and `ffmpeg` against the real cameras in a sandbox. It is the only check `-Live` unlocks. |

**A check that skips is not a check that failed, and how many skip depends on the machine.** Exit code 0 means no check failed, so read the tally rather than counting on a fixed split. Four can skip: `autostart` and `procs` while the real recorder is installed and running ([limitation 12](limitations.md)), and `leak` and `uileak` when there is no `camera_creds.xml`, because then there is no password to scan for and a clean scan would prove nothing. `uileak` also skips when `camui.exe` has not been built or will not start.
