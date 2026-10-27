# Rollback: from Holocron back to the PowerShell system

This page undoes [the cutover](cutover.md): it stops Holocron, proves it is gone, switches its autostart off and starts the old system (`cam.ps1`, `cam.recorder.ps1`, `camui.exe`) again, on the same archive. The old system was never changed by the cutover; the one thing to restore is the flat archive: Holocron files closed segments into day folders (decision D20), which the old system does not read, so step R2a puts them back with `holocron archive flatten` before it starts. Otherwise rolling back is "stop one, start the other". It takes minutes: Holocron's `down` took 0.4 s with fake cameras (CONFIRMED, cmd/holocron README, not timed with six real ones), and the old system's own start chain measured 18 to 32 s (the old project's notes, 2026-10-13).

Same labels as the cutover page: **CONFIRMED** (a recorded run), **source-checked** (read in this branch's code, not run for this page), **documented-only**, **UNVERIFIED**.

## When to roll back

Decide before you need it. "Now" means do not wait to see if it recovers.

| Trigger | When |
|---|---|
| `holocron up --detach` says `NOT RECORDING` twice in a row after you fixed the cause once | now |
| Alert 102: the restart budget is spent and Holocron STAYS DOWN | now |
| Alert 120, 121 or 122: pruning disabled, blocked below the reserve, or the age-rule breaker held a run (the disk is at risk) | now |
| The pruner deleted something it should not have (`prune: ... deleted N` with an age reason you did not set, the newest file of a camera gone) | now, and keep the log |
| A camera has no new segment two boundaries (30 minutes) after it should, and `holocron cam test "<name>"` says the camera answers | now |
| Segments are missing audio, are far smaller than the old ones, or have a different codec or layout (`ffprobe`) | now |
| Alert 100 (unexpected stop, restarted) more than once in a day | after the second |
| It did not come back after a logoff, a reboot or a resume (autostart failed) and one `holocron up --detach` plus the log does not explain it | after one retry |
| The web app or Live cannot do what you need and a restart of the web role (`holocron restart`) does not fix it | your call |
| You simply do not trust it | any time; this is your system |

These are suggestions for a first week, not measured thresholds; change them if you want, but write the change down. A camera outage that the old system would also have had (a gap both sides share) is not a trigger.

## What you need

- The notes from cutover P7: the folder and the exact commands you use to start the old system, and the launcher stub's `Target` line.
- A PowerShell window (Windows PowerShell 5.1, not elevated). Define the same two variables as in the cutover, and open `<old folder>` as the place you run the old commands from:

  ```powershell
  $h = 'C:\Holocron\holocron.exe'
  $c = 'C:\Holocron\holocron.config.json'
  ```

- `HOLOCRON_SANDBOX_ONLY` unset in this window. If `HOLOCRON_CAMERA_USER` or `HOLOCRON_CAMERA_PASSWORD` exist here, they do not matter for `down`.

## The sequence

Run these in order. Each step has `Expect` and `Stop if`. The old system's footage and settings are not touched by any of them.

### R1. Stop Holocron and prove it

```powershell
& $h --config $c down
```

**Expect:** `before: <n> process(es): ...`, then `stopped through the control API: requested (...)` and `stopped: 0 survivors after <x>s`, exit 0. With nothing running it says `nothing is running for E:\cameras` and exits 0. `down` finds its processes by pid, start time and image from its own registries (host, supervisor, children, the web child), never by name, and stops the web child and the relay with the rest (source-checked, `cmd/holocron/nvr.go`, `internal/app`). It kills `ffmpeg` as the old stop does, so each camera's last file is cut at that moment and still plays.

**Stop if:** `DID NOT STOP after <x>s: ...` with a list, exit 1. Do NOT start the old system yet. Run `down` once more. If a survivor remains, the list names its role and pid: stop exactly those pids with `Stop-Process <pid>` (not by name, not anything else), then `down` again. This escalation is source-reasoned and UNVERIFIED on a real machine; if anything about it looks wrong, stop and ask rather than guess.

### R2. Confirm that nothing is left, and that the ports are free

```powershell
& $h --config $c status
netstat -ano | findstr ":8554 :9997 :8787 :8443 :8889"
netstat -ano -p UDP | findstr ":8189"
tasklist /fi "imagename eq ffmpeg.exe"
tasklist /fi "imagename eq mediamtx.exe"
tasklist /fi "imagename eq holocron.exe"
```

**Expect:** `status` exits 1 and prints `supervisor: NOT RUNNING (...)` with `last stop: requested ...`, `host: not running` and `web app: not running (would listen on ...)`. `netstat` prints nothing. Each `tasklist` prints `INFO: No tasks are running which match the specified criteria.` (standard Windows tools; the `netstat` output also matches a remote port, so a line with `ESTABLISHED` to a camera's port is not a listener of yours). **Stop if:** anything is listed: R1 was not finished. Why this is not optional: both systems read and write `<archive root>\_state\status.json`, and each decides "already running" from it. If Holocron were still alive, the old `-Start` would see a fresh heartbeat and say it is already running, or the two would fight for the cameras and ports. Never run both on one root.

### R2a. Put the archive back flat

The old system lists only the files directly in each camera folder. With Holocron stopped (R1), put every filed segment back (the command refuses while Holocron runs):

```powershell
& $h --config $c archive flatten
& $h --config $c archive flatten --now
& $h --config $c archive stats
```

**Expect:** the dry run lists per camera what comes back and which day folders go; `--now` ends with `total: put back N segment(s) ...: done` and exit 0; `archive stats` shows no camera with segments filed in day folders. Files listed as left in a day folder are not footage (misfiled or foreign) and the old system ignores them. Exit 1 with a conflict: the camera folder already holds that name; compare the two files, keep one, run `--now` again.

### R3. Switch Holocron's autostart off

```powershell
& $h --config $c autostart disable
& $h --config $c autostart status
```

**Expect:** `disabled and verified: logon task Holocron, DISABLED, console window ..., runs ...`; `autostart status` then shows `DISABLED` and the three old tasks' state. It keeps the entry (and Holocron's record), so you can come back with `autostart enable`; neither command starts or stops a running `holocron`. If you will never return, `holocron uninstall` removes exactly what `install` created and recorded (add `--dry-run` to see the commands first); a record that is missing needs `--force`. **Stop if:** `nothing was installed by holocron install (no record ...)`: the entry was never created or the record was deleted. Look with `schtasks /Query /TN Holocron`; nothing else is needed for the old system. Do this BEFORE R4, so the two are never both enabled (`doctor` warns "both systems would start at logon" otherwise).

### R4. Bring the old system's autostart back (the Operator's choice)

Your three old logon tasks are disabled on purpose today. Two ways back; pick one and keep it consistent with what you wrote down in P7.

**A. Documented path - enable the three tasks and start through them:**

```powershell
Enable-ScheduledTask -TaskName IPCCTV-Recorder,IPCCTV-Dashboard,IPCCTV-Ui
Get-ScheduledTask -TaskName IPCCTV-Recorder,IPCCTV-Dashboard,IPCCTV-Ui | Format-Table TaskName, State
```

`schtasks /Change /TN IPCCTV-Recorder /ENABLE` (and the same for `IPCCTV-Dashboard` and `IPCCTV-Ui`) does the same, as [autostart.md](autostart.md#switching-between-the-powershell-system-and-holocron) lists it. **Expect:** the three tasks show `Ready`. This puts the old system back in charge of logon too: from now on it starts by itself at logon.

**B. The exact state before the cutover - the tasks stay disabled:** skip the commands above and start the old system by hand with the commands you wrote down. Whether `cam.ps1 -Start` works when `IPCCTV-Recorder` is disabled is UNVERIFIED (it runs `schtasks /run` on it, source-checked); if you use B, start with the call the launcher stub makes, as in R5.

**Stop if:** `Enable-ScheduledTask` fails (a different account, or a policy): use `schtasks /Change` once; if it fails too, use B.

### R5. Start the old system

The pitfall first. `cam.ps1 -Start` does not run the scripts of the folder you type it in. It calls `schtasks /run IPCCTV-Recorder` (source-checked, `cam.ps1`); that task runs the launcher stub `%LOCALAPPDATA%\IPCCTV\start-recorder.ps1`, and the stub's `$Target` is the `cam.recorder.ps1` of the repository where the tasks were installed. Observed 2026-10-16: `-Start -Recorder` typed in a worktree ran the ORIGINAL repository's script, so the worktree's code never ran. So the code that starts is the one in the `Target` line from P7, whichever folder you are in. To start a different checkout's code, run `.\cam.ps1 -Stop`, then that checkout's `.\cam.recorder.ps1 -Nvr` directly (the call the stub makes); never `-Install` from it, which re-points the three tasks. (Documented-only for the Operator's installation: the memory is of an incident, and his installed `Target` is not known to this page.)

```powershell
cd <old folder>
.\cam.ps1 -Start
Start-ScheduledTask -TaskName IPCCTV-Ui
.\cam.ps1 -Status
```

`-Start` brings the recorder up (with MediaMTX), waits for the relay to serve, then opens the dashboard; the UI has its own task and `-Start` does not start it. **Expect:** `starting the 'IPCCTV-Recorder' task`, then `up after <n>s - ...` (the chain measured 18 to 32 s), a line `<n> of <m> path(s) serving after the wait: ...`, exit 0. **Stop if:** `NOT RECORDING: ...` and exit 1: the task started and no supervisor reported. Read `_logs\recorder.log` and `%LOCALAPPDATA%\IPCCTV\autostart.log`, run `.\cam.recorder.ps1 -Nvr` directly to see what it says, and see [troubleshooting.md](troubleshooting.md#when-something-stops-unexpectedly). If the old system will not start at all, you have a problem to solve with the cameras unrecorded: do not touch `E:\cameras\_state` by hand; ask.

### R6. Verify the old system is healthy

```powershell
.\cam.ps1 -Status
.\cam.recorder.ps1 -Status
```

Then wait for the next :00, :15, :30 or :45 and list the newest files:

```powershell
dir E:\cameras\Front_Door | sort LastWriteTime -desc | select -first 3
```

**Expect:** `-Status` shows the supervisor running, the relay serving every path, each recording camera with a live `ffmpeg` (input `relay`), the UI serving `127.0.0.1:8787` and the tasks as you set them; `cam.recorder.ps1 -Status` shows a fresh report with the disk figures (free space above the reserve, a measured rate, the days of headroom); after the boundary each camera has a new segment named like `Front_Door_20261027_121501+0200.mkv`. Open `http://127.0.0.1:8787/` in a browser ON WINDOWS: the Dashboard says the cameras record. **Stop if:** a camera is not recording: `.\cam.recorder.ps1 -List` shows its `Record` flag; the cause is usually a change made in Holocron's copy of `cameras.json` (see "Carry the changes back").

### R7. Carry back what you changed while on Holocron

Holocron works on its own copies, `C:\Holocron\holocron.config.json` and `C:\Holocron\cameras.json`; the old system's `recorder.config.json` and `cameras.json` are exactly as you left them. If you changed anything meanwhile, repeat it on the old side:

```powershell
& $h --config $c cam list
& $h --config $c config show
```

compare with the old files, and apply the differences with the old system's tools (`.\cam.recorder.ps1 -Enable <name|all>` and `-Disable`, or the Settings screen of the old UI for cameras, segment length, reserve, hysteresis and retention days). `cam list` and `config show` read Holocron's copies and work with Holocron stopped. If you changed the camera login on the cameras, `camera_creds.xml` is stale: update it the way [install.md](install.md#credentials) says; this page does not automate it.

## What happens to the footage Holocron wrote

Nothing needs doing. Holocron's segments are in the same folders with the same names and format as the old system's (`<Camera>_YYYYMMDD_HHMMSS+HHMM.mkv`, decision D18): the old pruner recognises them by name and container alone and deletes oldest first by the same rules, so they age out like any other file. The Footage screen of the old UI lists them by file name, with no database. Two honest details:

- Each camera has ONE gap where the systems swapped (cutover C3 to C7) and another where you swap back; the segment around each swap is a partial one, shorter than 15 minutes. Both are normal.
- The last file Holocron wrote before `down` was cut when its `ffmpeg` was killed. It has its streams but no duration in the header; it plays (MKV is crash-safe) but a tool that wants a duration may show none (CONFIRMED for the shadow, shadow.md). Whether the old UI's clip player handles such a file is UNVERIFIED; the old system's own stop produces the same kind of file.

Files Holocron leaves in the archive root that the old system does not use: `_logs\holocron.log` and `audit.log`, and in `_state` its lock file `supervisor.lock` and possibly some of its other state files. An orderly `down` removes `ctl.addr`, `ctl.secret` and `host.json` (CONFIRMED in the end-to-end test); what else stays is UNVERIFIED. Folders whose name starts with `_` are never looked at by the pruner of either system (source-checked, `internal/prune`). The ownership marker `_state\archive-owner.json` is the same file both systems use and stays valid. Whether the old supervisor tolerates every leftover Holocron state file is UNVERIFIED; delete none of them by hand.

Other things Holocron created, all outside the archive and all harmless to the old system: the Credential Manager entries `holocron/camera.user` and `holocron/camera.password` (remove them with `holocron secret delete camera.user` and `... camera.password` only when you are sure you will not return), `relay_api_secret.txt` and `live_view_secret.txt` in the web auth folder, `C:\Holocron\_state\install.json`, and (if you set the keys as the cutover says) `C:\Holocron\mediamtx.yml` and `mediamtx-live.yml`. If you did NOT give Holocron its own relay config paths, it wrote its own login hash into the old `C:\mediamtx\mediamtx.yml`; the old system rewrites that file at its next start when the block is not its own (documented-only, [how-it-works.md](how-it-works.md#transport-the-relay)); if the old relay's API login is refused after the rollback, the old system raises alert 130 and `.\cam.ps1 -Stop` then `-Start` fixes it.

## What NOT to do

- **Do not start the old system before R1 and R2 proved Holocron gone.** Two systems on one root fight for the cameras' two RTSP sessions, the ports and the pruner.
- **Do not kill processes by name** (`ffmpeg`, `mediamtx`, `holocron`, `camui`). Stop by the system's own command, or by the exact pid it names.
- **Do not run `.\cam.ps1 -Install` or `-Uninstall`** as part of a rollback. The tasks already exist; `-Install` from another folder re-points them, `-Uninstall` deletes them and their launchers. `-Start` is enough.
- **Do not delete, move or edit** `E:\cameras\_state`, `_logs`, the ownership marker, `status.json`, `camera_creds.xml`, `relay_api_creds.xml`, `live_view_secret.txt` or any footage to "clean up". A missing marker makes the pruner refuse (alert 120) until a restart re-claims the root.
- **Do not run `holocron prune --now --yes`** at any time during a rollback. It refuses while a supervisor answers, and it deletes recordings.
- **Do not run `holocron tls rotate`.** It deletes the CA the old `camui.exe` uses too (the folder is shared); every device would have to reinstall it.
- **Do not use the web page's Start and Stop buttons to switch systems.** The page drives the supervisor's control API when one answers and falls back to the PowerShell scripts when none does (docs/ui.md); a stopped Holocron cannot be started from it (Start answers 409). Use the commands on this page.
- **Do not disable Microsoft Defender** to make Holocron run, and do not add `--i-know-old-tasks-are-enabled` to any command to make a refusal go away.
- **Do not set `HOLOCRON_SANDBOX_ONLY`** in the window where you operate the real system: it refuses `E:\cameras` on purpose.

## Rehearse the rollback

Do this once before the cutover, with the old system running and nothing cut over. What is safe, and what is not:

**Safe, read-only on the live system (do these):**

```powershell
cd <old folder>
.\cam.ps1 -Status
Get-ScheduledTask -TaskName IPCCTV-Recorder,IPCCTV-Dashboard,IPCCTV-Ui | Format-Table TaskName, State
schtasks /Query /TN IPCCTV-Recorder /V /FO LIST
Get-Content "$env:LOCALAPPDATA\IPCCTV\start-recorder.ps1" | Select-String 'Target'
Test-Path <old folder>\camui.exe
```

This proves the inputs of R4 and R5: the three tasks exist and their state, the stub's `Target` is the folder you mean, `camui.exe` is where `Start-ScheduledTask -TaskName IPCCTV-Ui` expects it, and you know the old commands. Read your own R1 to R7 against the output.

**Safe, on the sandbox only** (the cutover's P8 config: set `$env:HOLOCRON_SANDBOX_ONLY = '1'` and `$rc` as there, in a window of its own): the Holocron half of the rollback up to the point where it would change real state.

```powershell
& $h --config $rc down                  # expect: nothing is running for D:\holocron-rehearsal  (exit 0)
& $h --config $rc status                # expect: supervisor: NOT RUNNING (...), host: not running; exit 1 is the right answer here
& $h --config $rc uninstall --dry-run   # expect: nothing to remove: no install record ... (before the real install)
& $h --config $rc autostart status      # expect: not installed, plus the three old tasks' state
& $h --config $rc autostart disable --dry-run   # expect: refused: nothing was installed by holocron install (no record ...)
```

These show the wording of R1 to R3 and change nothing. The real `down` on a started Holocron was exercised by the shadow (its `down` proved 0 survivors, shadow.md). `holocron install`/`uninstall`/`autostart` against a real task were exercised only for test-named entries and one unsigned build on one machine ([autostart.md](autostart.md)); after the cutover's real `install`, run `autostart disable --dry-run` and `uninstall --dry-run` against the real entry (they print every command and change nothing), then, if you wish, one full `autostart disable` and `autostart enable` cycle while Holocron is stopped.

**Not safe on the live system (do not rehearse like this):** `.\cam.ps1 -Stop`, `.\cam.ps1 -Start`, `Enable-ScheduledTask`, `Start-ScheduledTask`, `holocron down` or `up` against the real config, and anything that stops the old stack. They interrupt recording or change real state; whether `Enable-ScheduledTask` honours `-WhatIf` is UNVERIFIED, so do not rely on it.

**Optional full rehearsal of the old start path** (your decision): at a quiet moment, run `.\cam.ps1 -Stop`, then R4 and R5 and R6 exactly as written, and time them. It costs one gap in the recording about as long as the old start chain (18 to 32 s measured, plus the stop) and it is the only way to prove the old start path on this machine. If you do it, start the old system the way you will start it in the real rollback and write down how long it took.

## After a rollback

- Holocron stays on disk, switched off. You can cut over again later: re-run the cutover from P4 on (a fresh window, the checks, C5 onwards), `holocron autostart enable` if you kept the entry (it refuses while an old task is enabled; disable those first with `schtasks /Change /TN IPCCTV-Recorder /DISABLE` and the other two), and `holocron up --detach`.
- Keep the Holocron logs (`holocron logs --since 7d`, or the files in `<archive root>\_logs`) and the alerts that made you roll back: they are the evidence for what to fix. The 3-day shadow comparison and the first week are the gates; a rollback is a result, not a failure.
- Nothing about a rollback needs the elevated prompt.
