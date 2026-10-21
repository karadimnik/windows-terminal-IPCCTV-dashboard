# Cutover: from the PowerShell system to Holocron

This page moves the real NVR - all cameras, the existing archive, the web app - from the PowerShell system (`cam.ps1`, `cam.recorder.ps1`, `camui.exe`) to the Go program `holocron.exe`, one step at a time, with the command, what to look for and when to stop. The way back is [rollback.md](rollback.md); read it first and rehearse it once (it has a section for that) before you do anything here. The old system is never changed, moved or deleted by this page: it stays installed, its three logon tasks stay as they are, and `rollback.md` brings it back in minutes.

> **WARNING - the clock change of Sunday 25 Oct 2026.** The 3-day shadow run (21 to 23 Oct 2026, see [shadow.md](shadow.md)) does NOT cover the clock change: segment names go from `+0300` to `+0200` and one hour repeats. Holocron's handling of it is covered by unit tests and a synthetic clock, never by a run across a real change, and the first real one it would meet is that night.
>
> **Rule: do not cut over between 23 and 26 Oct 2026 (both included) without an explicit decision of the Operator.** Read 27 Oct 2026 as the first ordinary day. If you decide to cut over inside the window anyway, write the decision down and read the "clock change" part of [the first week](#the-first-week) before you start. The next change after that is 28 Mar 2027 (the spring one); see the watch list.

## How to read this page

- **Steps.** Each step has the command, `Expect` (what a good result looks like) and `Stop if` (what to do instead of going on). A step is done only when its `Expect` holds. If a `Stop if` applies, do not continue: nothing so far has changed the old system's files, and `rollback.md` is the way back at any point after step C3.
- **Labels.** **CONFIRMED** - observed in a run that a repository document records. **source-checked** - read in this branch's code or README; the author of this page ran no command on your machine, so the wording of an `Expect` line may differ by a space but its meaning is what to test. **documented-only** - stated in a document and not re-checked. **UNVERIFIED** - not checked at all; treat as a guess.
- **Shell.** Windows PowerShell 5.1, not elevated, in a window you open for this (a fresh one: see P4). Two variables keep the commands short:

  ```powershell
  $h = 'C:\Holocron\holocron.exe'
  $c = 'C:\Holocron\holocron.config.json'
  ```

  Every `holocron` command takes the configuration first: `& $h --config $c <command>`. `<old folder>` is the folder that holds `cam.ps1`, and `<archive root>` is the old `RootPath` (the examples say `E:\cameras`).
- **Scope.** All cameras at once. A subset would need two supervisors writing one archive root, which the design forbids ("the two never write the same archive at the same time", HLD section 1). Windows is the main path; Linux and macOS are in [the appendix](#appendix-linux-and-macos).

## What stays and what changes

| | PowerShell system | Holocron |
|---|---|---|
| Archive root, folder and file names, MKV, 15-minute segments, the `+0300` suffix | the same | the same (decision D18); one folder per camera under `RootPath` |
| Pruner | free-space watermark, optional age rule | the same rules and gates, ported; same ownership marker file (see C6) |
| Program | scripts + `camui.exe` | `holocron.exe` only (rule R1); the web app is its web role |
| Settings | `recorder.config.json`, `cameras.json` | `holocron.config.json` (same key names, it loads a copy of the old file unchanged) and `cameras.json` beside it |
| Camera login | `camera_creds.xml` (DPAPI) | the secrets store (Windows Credential Manager by default) |
| Autostart | tasks `IPCCTV-Recorder`, `IPCCTV-Dashboard`, `IPCCTV-Ui` | one task `Holocron` ([autostart.md](autostart.md)); the old three are only read, never changed |
| Viewing | `mpv` grid + health strip + web app | the web app's Live tab (`holocron open`); the `mpv` grid and the strip are not part of Holocron |
| Web users, certificates | `%LOCALAPPDATA%\IPCCTV` folders | the same default folders (see P9) |

## Pre-flight checklist

Nothing in this part changes the old system or touches `<archive root>`. Everything that would write there waits until the old system is stopped (step C6 on).

### P1. The date

**Do:** check the date against the warning above.
**Expect:** today is 27 Oct 2026 or later, or you have written down an explicit decision for a date between 23 and 26 Oct.
**Stop if:** neither.

### P2. The shadow comparison

The shadow run is Holocron beside the PowerShell system on two cameras, writing a copy to `D:\holocron-shadow` ([shadow.md](shadow.md)). The comparison is read-only on both roots and needs `ffprobe`. Stop the shadow first so every segment is closed: with it still running, the newest segment of each camera is skipped as open.

```powershell
$hs = 'D:\holocron-shadow-bin\holocron.exe'
$cs = 'D:\holocron-shadow\_config\holocron.config.json'
& $hs --config $cs down
& $hs --config $cs archive compare --old E:\cameras --new D:\holocron-shadow
$LASTEXITCODE
```

(Use the paths of your shadow. Add `--json` to `archive compare` for the machine-readable report, schema 1, and keep it with your notes.) The shadow's `holocron down` is also the proof that `down` works on a real machine (shadow.md: "ordered stop, proves 0 survivors").

**Expect:** `down` ends with `stopped: 0 survivors` (or `nothing is running for D:\holocron-shadow`), and `archive compare` exits `0` (equal within the tolerances; `$LASTEXITCODE` prints `0`). Exit `1` means differences (the report lists them), `2` a usage problem such as a missing root or no `ffprobe`. In the report (the JSON keys below; the text form says the same in sentences) read these:
- `equal` is `true` and `differences` is `0`;
- for every camera, `window.seconds` is long (hours, not seconds) and `new.compared_segments` and `old.compared_segments` are above zero. No overlap or no closed segment is reported as a difference, never as equal;
- `size_ratio` `median` close to 1 and `min`/`max` inside 0.90 to 1.10; no `mismatch` in `pairs[].kind`;
- `notes[]` may hold gaps that BOTH sides share (a camera or relay outage) and partial first/last segments: those are expected. A gap on one side only is a difference.

**Stop if:** exit `1` or `2`, or a camera with no compared segments. Do not cut over on a report you cannot explain; a difference is a finding about Holocron, not about your cameras.

**Coverage, honestly.** The shadow used two cameras (shadow.md), so four of six never ran on Holocron before cutover. Their folders are created by the same code from `cameras.json`, but nothing compared them. `docs/shadow.md` still says seven days: it was written before the shadow was shortened to 3 days (plan decision D5, revised); the 3 days are the Operator's decision.

### P3. The program, in a permanent folder

Put `holocron.exe` where it will stay: `C:\Holocron` (not `C:\Windows\Temp`, not a Downloads or temporary folder; not inside `<archive root>`, which the root guard refuses). Use the SAME build you shadowed, so what you compare is what you run:

```powershell
mkdir C:\Holocron
Copy-Item D:\holocron-shadow-bin\holocron.exe C:\Holocron\holocron.exe
Get-FileHash D:\holocron-shadow-bin\holocron.exe, C:\Holocron\holocron.exe
& 'C:\Holocron\holocron.exe' version
```

**Expect:** the two hashes are equal; `version` prints `holocron <version> (status.json schema 1, windows/amd64, go...)`. If you must use a newer build, write down its `version` line and accept that it was not the one shadowed. To build one: `./build-holocron.sh` from WSL (it runs gofmt, vet and the tests first and puts `holocron.exe` in the repository root; CI compiles but does not publish builds, [ci.md](ci.md)).

**Stop if:** `install` later refuses with "looks transient": you installed from a temporary folder. Copy to the permanent folder and run it from there.

**Defender.** On 2026-10-21 Microsoft Defender flagged an unsigned test build that had just created a (disabled) logon task, as `Behavior:Win32/Persistence.A!ml`, and blocked it; that build ran from `C:\Windows\Temp`. The same day a build from a permanent folder under the user profile did the whole install/update/enable/disable/uninstall cycle with no detection (CONFIRMED, one machine, one day; [autostart.md](autostart.md#known-issue-microsoft-defender)). It is not a guarantee, and a signed binary is the durable fix. If Defender flags `C:\Holocron\holocron.exe` anyway:
1. Do nothing to Defender first. Open Windows Security > Virus & threat protection > Protection history, and read what was blocked and when. Write the threat name down (general Windows behaviour; not re-checked on your machine - UNVERIFIED).
2. The old system is unaffected: nothing has been stopped yet if it happens in P3 to P11. If it happens after C3, go to `rollback.md`.
3. Your choices, all yours and none done by Holocron: restore the item and "allow on device" for that one file; or add a Defender exclusion for the folder `C:\Holocron` only (it lowers protection for that folder, so use it only for a binary you built yourself); or build again and re-copy. Do not turn Defender off. This page does not create exclusions and does not need an elevated prompt.

### P4. A clean environment

The shadow set made-up `HOLOCRON_CAMERA_USER` and `HOLOCRON_CAMERA_PASSWORD` in its shell. Those two variables WIN over the stored login (cmd/holocron README), so a window that still has them would start every camera with the fake login.

```powershell
dir Env:HOLOCRON*
```

**Expect:** nothing is listed. **Stop if:** anything is: close the window and open a fresh one. If they come back in a fresh window they are set in your account's environment (System Properties > Environment Variables): remove them there first. Also: `HOLOCRON_SANDBOX_ONLY` must NOT be set in the cutover window (it refuses `E:\cameras` by design). It is used only in the rehearsal window (P8).

### P5. ffmpeg and MediaMTX

```powershell
& C:\ffmpeg\bin\ffmpeg.exe -version
```

**Expect:** the first line shows version 5.0 or later (`doctor` fails below 5.0 and a cutover would stop with exit 21). A development build number is accepted with a `warn` from `doctor` (shadow.md). MediaMTX is the same `C:\mediamtx\mediamtx.exe` the old system runs; Holocron does not replace it. **Stop if:** older than 5.0.

### P6. Disk

```powershell
cd <old folder>
.\cam.recorder.ps1 -Status
```

**Expect:** the report shows free space, the measured rate and the days of headroom. Free space must be at or above `ReserveGB` (20 by default): below it the first prune cycle under Holocron deletes oldest footage at once, exactly as the old pruner would, but you should choose that, not be surprised by it. Note the measured GB/day: the first-week watch list compares Holocron's rate with it.

### P7. Write down the way back (inputs of the rollback)

The rollback starts the OLD system with its own commands, so record what they depend on while the old system still runs. All read-only:

```powershell
cd <old folder>
.\cam.ps1 -Status
Get-ScheduledTask -TaskName IPCCTV-Recorder,IPCCTV-Dashboard,IPCCTV-Ui | Format-Table TaskName, State
Get-Content "$env:LOCALAPPDATA\IPCCTV\start-recorder.ps1" | Select-String 'Target'
Test-Path <old folder>\camui.exe
```

**Expect:** the three tasks exist and (as you set them) are `Disabled`; the `Target` line names the `cam.recorder.ps1` of the repository the tasks were installed from. **Write down:** (1) the exact folder and the exact commands you use today to start the old system by hand; (2) that `Target` path. Why it matters: `cam.ps1 -Start` does not run the scripts of the folder you type it in. It runs `schtasks /run IPCCTV-Recorder` (source-checked, `cam.ps1`), which starts the launcher stub in `%LOCALAPPDATA%\IPCCTV\`, and the stub's `$Target` is the `cam.recorder.ps1` of the folder where the tasks were installed (observed 2026-10-16 with a worktree; the exact text of the `Target` line is UNVERIFIED here). Never run `.\cam.ps1 -Install` from another folder to "fix" this: it re-points the three tasks. **Stop if:** the `Target` is not the folder you intend to roll back to; settle that now, not during a rollback.

### P8. Rehearsal sandbox: tools, credentials, the install plan

A throwaway root beside nothing real, so `holocron` commands can be tried while the old system runs. With `HOLOCRON_SANDBOX_ONLY=1` set, `holocron` refuses a root that is a drive root, has a folder named `cameras` in its path, or holds `recorder.log` or `camera_creds.xml` (source-checked), so a typo cannot reach `E:\cameras`.

Create `D:\holocron-rehearsal\_config\holocron.config.json`:

```json
{
  "RootPath": "D:\\holocron-rehearsal",
  "FfmpegPath": "C:\\ffmpeg\\bin\\ffmpeg.exe",
  "FfprobePath": "C:\\ffmpeg\\bin\\ffprobe.exe",
  "RestreamerEnabled": true,
  "RestreamerExePath": "C:\\mediamtx\\mediamtx.exe",
  "RelayBase": "rtsp://127.0.0.1:18554",
  "RelayApiBase": "http://127.0.0.1:19997",
  "LiveViewEnabled": false,
  "alerts": { "desktop": { "enabled": false }, "eventlog": { "enabled": false } },
  "web": { "enabled": false }
}
```

and `D:\holocron-rehearsal\_config\cameras.json` with one camera that records nothing:

```json
[ { "Name": "Rehearsal", "IP": "127.0.0.1:9", "Quality": "stream1", "Record": false } ]
```

(The relay ports 18554 and 19997 differ from the real 8554 and 9997 so `doctor` can check them while the old relay runs.) Then, in a window where `HOLOCRON_SANDBOX_ONLY` is set:

```powershell
$env:HOLOCRON_SANDBOX_ONLY = '1'
$rc = 'D:\holocron-rehearsal\_config\holocron.config.json'
& $h --config $rc config validate
& $h --config $rc secret import --from '<old folder>\camera_creds.xml'
& $h --config $rc secret list
& $h --config $rc doctor
& $h --config $rc install --dry-run
```

The `secret import` is YOURS to run, as the Windows user who owns `camera_creds.xml` (it is DPAPI-protected, so no one else can read it, and no agent is asked to). It reads the old file through PowerShell's own `Import-Clixml` in a short child, never changes or deletes the file, and prints only the two names. The default file is `camera_creds.xml` beside `cameras.json`, which is not `<old folder>` here, hence `--from`. The store is the same for every config that keeps `secrets.backend` at its default (`auto`: the Credential Manager, entries `holocron/camera.user` and `holocron/camera.password`), so the real config finds the login too. If you change `secrets.backend`, `secrets.file` or `secrets.keyFile` in the real config, it will NOT find it.

**Expect:**
- `config validate`: `ok   ...holocron.config.json` and `ok   ...cameras.json (1 camera(s))`; `warn` lines for unknown keys are fine.
- `secret import`: `imported camera.user and camera.password`. `secret list`: `2 secret(s) in the keyring store (names only)` (or `file store`) with `camera.password` and `camera.user`. The values are never shown.
- `doctor`: no line starting `fail`. In this sandbox expect `ok` for configuration, ffmpeg, mediamtx, the two relay ports (`tcp 127.0.0.1:18554 is free`), folders and clock; `ok credentials` (`a camera login is present`); `warn archive root` ("not claimed yet": normal for a new root); `warn status` (no status file yet); `ok autostart` (not installed) and `ok old tasks` (the three, `disabled`). A `warn` on ffmpeg for a development build is expected.
- `install --dry-run`: prints `PowerShell system tasks (read only, never changed): IPCCTV-Recorder disabled, IPCCTV-Dashboard disabled, IPCCTV-Ui disabled`, the `schtasks` commands and the task XML, and `dry run: nothing is changed`. Look at the XML once: it should run `holocron.exe --config ... up` and no old script.

**Stop if:** `secret import` fails (a different Windows account, a missing file, a Credential Manager that is unusable - the last falls back to the file store under `auto`); any `fail` from `doctor`; `install --dry-run` says one of the old tasks is ENABLED (you wanted them disabled: find out why before going on). Then close this window.

### P9. The LAN login and the phone (skip if you do not use the HTTPS listener)

The web app's users and certificate authority live in the same default folders as `camui.exe`'s (`%LOCALAPPDATA%\IPCCTV\camui\auth` and `...\tls`, `internal/config/KEYS.md` says "the web app's default folders"), so what you already made for the old UI is probably already there. Look first, in the rehearsal window (these commands touch no archive root):

```powershell
& $h --config $rc user list
& $h --config $rc tls ca
```

**Expect:** `user list` shows your existing users, or `no users (add one with: holocron user add <name>)`. `tls ca` prints the CA file, its SHA-256 fingerprint and the install steps; compare the fingerprint with the one your phone trusts. If it printed a new fingerprint, there was no CA and one was created: the phone must install it ([ui.md](ui.md#https-on-the-lan-a-phone-or-a-second-pc)).

If there is no user, create one (the password is read from standard input, never an argument; 12 to 128 characters; ASCII in Windows PowerShell 5.1):

```powershell
$p = Get-Credential anna
$p.GetNetworkCredential().Password | & $h --config $rc user add anna
```

**Expect:** `created user anna (role admin) in <auth folder>`. `holocron user add anna --role viewer` makes a view-only user. Never run `tls rotate` casually: it deletes the CA the old `camui.exe` uses as well, and every device must reinstall it.

**Firewall** (only for the HTTPS listener or LAN live media; nothing here is applied for you). Windows filters by program, and `holocron.exe` is a new program, so the rule `camui.exe` has does not cover it. In an elevated PowerShell, adjusted from [ui.md](ui.md#https-on-the-lan-a-phone-or-a-second-pc) (the same cmdlets, the `-Program` swapped; not run on your machine - UNVERIFIED):

```powershell
New-NetFirewallRule -DisplayName 'Holocron web HTTPS (TCP 8443)' -Direction Inbound -Action Allow -Protocol TCP -LocalPort 8443 -Program 'C:\Holocron\holocron.exe' -Profile Private -RemoteAddress LocalSubnet
Remove-NetFirewallRule -DisplayName 'Holocron web HTTPS (TCP 8443)'   # to take it away again
```

The MediaMTX program rule for live media already covers `C:\mediamtx\mediamtx.exe` ([ui.md](ui.md#live-view-on-the-lan-media-firewall-and-stuck-tiles)). Without a rule Windows asks the first time `holocron.exe` listens beyond loopback; that prompt is the thing the rule avoids.

### P10. The real configuration

Copy, never move, so the old files stay where they are:

```powershell
Copy-Item <old folder>\recorder.config.json C:\Holocron\holocron.config.json
Copy-Item <old folder>\cameras.json C:\Holocron\cameras.json
```

Open `C:\Holocron\holocron.config.json` and check:
1. `RootPath` is the same as the old one (`E:\cameras`), spelled the same. The ownership marker names this path (see C6).
2. Keep every other key as it is: `SegmentSeconds`, `ReserveGB`, `MaxRetentionDays`, `FfmpegPath`, the live keys. The keys are the same names (an existing file "loads unchanged", KEYS.md), and keeping them equal is what the shadow compared. `MaxRetentionDays` especially: a value you did not mean turns on the age rule, whose circuit breaker holds a run above half the archive.
3. Add two lines so Holocron never rewrites the OLD system's MediaMTX files. The default paths are `C:\mediamtx\mediamtx.yml` and `C:\mediamtx\mediamtx-live.yml` (source-checked), the files the old system generates; Holocron writes its own relay login hash into whichever it is given:

   ```json
   "RestreamerConfigPath": "C:\\Holocron\\mediamtx.yml",
   "LiveConfigPath": "C:\\Holocron\\mediamtx-live.yml"
   ```

   (Not left out? Then the old system rewrites its `mediamtx.yml` at its next start when the login block is not its own: documented in [how-it-works.md](how-it-works.md#transport-the-relay) and untested by this page; it costs a restart, not footage.) Holocron keeps its relay login in `relay_api_secret.txt` and, unless the copied config sets `LiveSecretPath`, its live login in `live_view_secret.txt`, both in the auth folder (source-checked), not in the old `relay_api_creds.xml`; leave `LiveSecretPath` as it is (if it names the old system's file, Holocron shares that file, which its code allows but this page has not tried).
4. For the phone, the HTTPS address lives in THIS file, not in `camui.config.json`: `"web": { "tlsListen": "192.168.1.50:8443" }` (use the address from the old `camui.config.json`). `web.listen` stays `127.0.0.1:8787`.

```powershell
& $h --config $c config validate
```

**Expect:** `ok   C:\Holocron\holocron.config.json` and `ok   C:\Holocron\cameras.json (6 camera(s))` (your count). **Stop if:** `FAIL` lines: fix the named key. This command is read-only (source-checked: a read-only command logs at DEBUG, so at the default level it writes no log line; whether it creates an empty `_logs\holocron.log` is UNVERIFIED and harmless: underscore folders are never pruned).

### P11. Read the rollback and rehearse it

Open [rollback.md](rollback.md), do its "Rehearse the rollback" section once, and keep the page open in a second window during the cutover. **Stop if:** you could not finish that rehearsal's checks.

### P12. Choose the window

Pick a time you can give 30 minutes, with the cameras reachable. The recording gap is the time from the end of the old system's stop (C3) to the `RECORDING` line (C9); it is as short as your typing, so have C3 to C9 ready to paste in order. The old system's own start chain measured 18 to 32 s (the old project's notes, 2026-10-13); Holocron's `up --detach` reached the honest start in 2.2 s with fake cameras (CONFIRMED, cmd/holocron README) and has not been timed against six real cameras (UNVERIFIED).

## The cutover

Open a fresh PowerShell window (not elevated, no `HOLOCRON_*` variables, `HOLOCRON_SANDBOX_ONLY` unset), define `$h` and `$c`, and keep `rollback.md` open.

### C1. Look at the old system

```powershell
cd <old folder>
.\cam.ps1 -Status
```

**Expect:** the supervisor, `ffmpeg` children, the relay with its paths, the control UI with its address and the pid serving the port, and the three tasks (disabled). Write the UI's pid down. **Stop if:** it shows trouble you would have to explain later; a cutover on a sick system proves nothing.

### C2. Stop the shadow (if it still runs)

```powershell
& $hs --config $cs down
```

**Expect:** `stopped: 0 survivors after ...s` or `nothing is running for D:\holocron-shadow`. It reads the old relay, so stopping the old system first would send it into its retry backoff. **Stop if:** `DID NOT STOP`: read the survivors it lists; do not go on.

### C3. Stop the old system the way its own docs say

```powershell
.\cam.ps1 -Stop
```

**Expect:** `everything in scope is stopped.` and exit 0 (source-checked). `-Stop` stops the recorder, MediaMTX, every `ffmpeg`, the viewer panes and the health strip, orphans included, verifies it, and deliberately leaves the UI running ([README](../README.md#day-to-day)). The recorder's stop kills `ffmpeg` (`holocron down` does the same, shadow.md), so each camera's last file is cut at that moment; MKV survives that and the file still plays. **Stop if:** `STILL RUNNING:` with a list: run `.\cam.ps1 -Status`, then `-Stop` once more. Do not kill by name.

### C4. Stop the old UI

The Holocron web app listens on the same address, `127.0.0.1:8787`, and a second listener would fail and retry. The old docs' way is the pid `-Status` reports (also printed by `.\cam.ps1 -RemoveUi`):

```powershell
Stop-Process <pid from C1>
```

(`Stop-Process` takes the process id positionally.) **Expect:** no error. The UI's logon task stays disabled and unchanged. **Stop if:** access denied or the pid is not `camui.exe`: re-read `.\cam.ps1 -Status` for the current pid.

### C5. Prove nothing of the old system is left

```powershell
.\cam.ps1 -Status
netstat -ano | findstr ":8554 :9997 :8787 :8443 :8889"
netstat -ano -p UDP | findstr ":8189"
tasklist /fi "imagename eq ffmpeg.exe"
tasklist /fi "imagename eq mediamtx.exe"
tasklist /fi "imagename eq camui.exe"
```

**Expect:** `-Status` reports nothing running; `netstat` prints no line (these are the relay RTSP and API, the web app, its HTTPS port, the live WHEP port and the live media port; drop any you do not use); each `tasklist` prints `INFO: No tasks are running which match the specified criteria.` (standard Windows tools, not run here). **Stop if:** anything remains. This is the step that matters most: Holocron decides "already running" from `<archive root>\_state\status.json`, the SAME file the PowerShell supervisor writes, and checks only that the pid in it is alive. With the old supervisor still alive, `holocron up` prints `already running: supervisor pid <the OLD one> (root ...)`, exits 0 and starts nothing (source-checked, `cmd/holocron/nvr.go`, `internal/status`). Treat that line as a failed cutover.

### C6. Check the real root (the first commands that touch it)

```powershell
Test-Path E:\cameras\_state\archive-owner.json
& $h --config $c doctor
& $h --config $c prune --dry-run
& $h --config $c archive stats
```

**Ownership, precisely** (`internal/archive`, `internal/prune` READMEs, source-checked). The pruner deletes only under a root that carries the marker `<root>\_state\archive-owner.json`. The PowerShell recorder writes that file with `Owner` `IPCCTV recorder`, and Holocron writes and accepts the same content, so a root the PowerShell system claimed is already owned. Holocron is stricter in one way: the `RootPath` inside the marker must name the configured root (a copied marker does not count). At its first start, `holocron up` calls the claim: with a valid marker it is a no-op and writes nothing; with no valid marker it writes one only if the root holds nothing but recorder files (every top-level entry starts with `_`, or is a camera folder whose files are all segments of that camera in the configured container, e.g. `Front_Door_20261027_120001+0200.mkv`). A foreign file or folder at the top of the root, a symlink or junction, or a drive root blocks the claim, and then the pruner refuses everything (alert 120, the disk will fill) while recording is not stopped by it. Existing segments are accepted as they are, and the newest file of every camera is always protected.

**Expect:**
- `Test-Path` prints `True`. (`False`: the old system never claimed it, which is unusual; the claim above decides.)
- `doctor`: no line starting `fail`. Now that the old stack is down, `ok relay RTSP  tcp 127.0.0.1:8554 is free`, `ok relay API ... :9997 is free` (and the live ports if `LiveViewEnabled`); `ok archive root  E:\cameras, claimed`; `ok credentials`; `ok autostart` not installed; `ok old tasks ... disabled`; `warn status  not running: ...` (the old supervisor's clean stop). `fail archive root` names the blocker (for example a file at the top of the root): move that entry out of `<archive root>` and run `doctor` again. Doctor creates `_state` and `_logs` if missing and writes then deletes one temporary file in the root, `_state` and `_logs` (source-checked): tiny, and it is why it is not run earlier.
- `prune --dry-run` (a preview, deletes nothing, though it does write a line to `_logs\holocron.log` and `audit.log`, both Holocron's own and ignored by the old system): `prune (dry run: nothing is deleted): E:\cameras`, the free/reserve/target line, and `would delete N file(s)`. Compare with what the old pruner would do (oldest first, only if free space is below the reserve or the age rule applies, never the newest file). **Stop if:** `REFUSED at the root|marker gate`, deletions with reason `age` when you did not intend an age rule, or `age rule HELD by the circuit breaker`.
- `archive stats`: your cameras' folders with segment counts and the newest segment of each, `free ... GB, reserve ... GB`. Note the figures: they are the "before".

**Stop if:** any of the above. Rolling back from here is C3's mirror: [rollback.md](rollback.md).

### C7. Start

```powershell
& $h --config $c up --detach
```

Detached means closing the window does not stop it; `holocron down` does. (Attached `holocron up` keeps it in the window - closing it stops everything - and prints no honest-start verdict, which is why this runbook uses `--detach` now. The logon task you create next has its own choice.) It waits for the supervisor's first heartbeat.

**Expect:**

```
started the background host, pid <n>; waiting up to <n>s for the supervisor's heartbeat
RECORDING: supervisor pid <n> is running after <x>s; 6 camera(s) set to record (holocron status shows each)
web app: http://127.0.0.1:8787/ is answering
```

Run from a window launched through WSL it warns that the background copy "may end with the terminal" (shadow.md); you are in a normal PowerShell window, so it should not appear.

**Stop if:**
- `NOT RECORDING: <reason> (see ...\_logs\holocron.log)`, exit 1: read the reason, then `& $h --config $c logs --level warn --since 10m`; the log carries the cause. A configuration fault is the supervisor's exit code 21 (a missing camera login names `HOLOCRON_CAMERA_USER` and `HOLOCRON_CAMERA_PASSWORD`; or ffmpeg below 5.0), and "a supervisor is already running" is exit 20: another process holds the archive, so you skipped C5. Fix the cause once; if it fails again, run `& $h --config $c down` and go to `rollback.md`.
- `already running: supervisor pid ...` - see C5.
- `web app: not answering on 127.0.0.1:8787 yet`: the port is taken (the old UI still runs) or the web role crashed; recording is not affected (a web crash never stops the supervisor), but read `logs --component web`.

### C8. Verify

```powershell
& $h --config $c status
& $h --config $c doctor
& $h --config $c status --json | ConvertFrom-Json | ForEach-Object { $_.status.Cameras } | Format-Table Name, Record, Alive, InputMode, RestartsLastHour, SecondsSinceLastClosedSegment
```

**Expect (`status`, exit 0):**

```
holocron status - root E:\cameras
supervisor: RUNNING pid <n> since <time>, heartbeat <x>s ago
host: pid <m>
web app: running pid <k> on http://127.0.0.1:8787/, HTTPS listener off, login exists   (or: login none yet)
cameras (6):
  Front Door           recording relay  pid <p> restarts 0 (0 last hour)
  ...
relay: alive=true pid <r>, 6 path(s)
live view: on, alive=true pid <l>            (or: live view: off)
disk: free <x> GB, reserve 20 GB, archive <y> GB, days left: not measured yet
prune: last run ..., deleted 0, freed 0.00 GB, blocked false
alerts: desktop on, eventlog on, log on, push off, webhook off
autostart: not installed (...)
```

The three that prove it is Holocron and not the old system: `host: pid <m>` is present (the old system has no Holocron host), the `web app` line says `running`, and every camera shows `relay` (a `direct` camera takes the camera's second RTSP session; it should not happen with the relay up). `days left` reads "not measured yet" for the first 10 minutes. The `doctor` now shows `ok status  running, supervisor pid <n>`; the JSON table shows `Alive` true everywhere, `InputMode` `relay`, `RestartsLastHour` 0 and `SecondsSinceLastClosedSegment` 0 to about 900 (the age of the current segment). The JSON key names are source-checked (`internal/status`); the exact table is UNVERIFIED.

**Web app and Live.** In a browser on Windows (not from WSL: its loopback is its own), open `http://127.0.0.1:8787/`, or `& $h --config $c open` (`open --print` only prints the address). **Expect:** the Dashboard shows the cameras as recording; the Live tab ("Live", `#live`) shows the tiles going from Connecting to Live (it needs `LiveViewEnabled`, which came with your copied config). Check the phone at `https://192.168.1.50:8443/` if you use it. **Stop if:** a camera is `DOWN`: `& $h --config $c cam test "Front Door"` (TCP and RTSP OPTIONS to the camera, no login) tells camera from Holocron.

**A segment closes on the next boundary.** Segments are cut on multiples of 15 minutes of the clock, give or take one keyframe (how-it-works.md). Wait for the next :00, :15, :30 or :45, then:

```powershell
& $h --config $c archive stats
dir E:\cameras\Front_Door | sort LastWriteTime -desc | select -first 3
& C:\ffmpeg\bin\ffprobe.exe -v error -show_entries stream=codec_type,codec_name,sample_rate,channels -of compact=p=0 "E:\cameras\Front_Door\<the segment that just closed>.mkv"
```

**Expect:** each camera's NEWEST segment in `archive stats` moves to the new boundary (names like `Front_Door_20261027_121501+0200.mkv`); the one before it, written by Holocron from your start to the boundary, is a partial and shorter than a full one - normal; the next full one is about the size of the old system's full segments for that camera (shadow criterion: ratio 0.90 to 1.10, UNVERIFIED for the four cameras that were not shadowed). `ffprobe` shows a video stream and an audio stream `pcm_alaw`, `sample_rate=8000`, `channels=1`, as in the old files (CONFIRMED for the shadowed cameras, shadow.md). The exact `ffprobe` formatting is UNVERIFIED. Expect ONE gap in each camera's timeline, between the old system's last file and Holocron's first: that is the swap, not a fault. **Stop if:** a camera has no new segment two boundaries later, a segment is far smaller than its neighbours, or the audio is missing. That is a rollback trigger ([rollback.md](rollback.md#when-to-roll-back)).

### C9. Install the logon task

```powershell
& $h --config $c install --dry-run
& $h --config $c install
& $h --config $c autostart status
```

Choose the window now. `--window visible` (the default): at logon `holocron up` opens in a console window on your desktop; that window IS the system, as `cam.nvr` was, and closing it stops everything in order (a "window closed" stop, one alert 103, nothing restarts it). `--window hidden`: `conhost.exe --headless ... up --detach`, no window to close by accident; `holocron down` stops it. A first install is visible; a later `install` without `--window` keeps the choice. Add `--window hidden` to the commands above if you want that. Service mode (elevated, boot-time) is not part of this runbook; see [autostart.md](autostart.md).

**Expect:** `install` prints `PowerShell system tasks (read only, never changed): IPCCTV-Recorder disabled, IPCCTV-Dashboard disabled, IPCCTV-Ui disabled`, then `installed and verified: logon task Holocron, enabled, console window visible, runs ...`, `recorded in C:\Holocron\_state\install.json`, and `it starts holocron at your next logon (nothing was started now; holocron up --detach starts it now)`. `autostart status` repeats the entry and ends with the three old tasks' state. **Stop if:** `install` refuses with a transient-folder message (P3) or lists an old task as ENABLED: do NOT add `--i-know-old-tasks-are-enabled`; both systems would start at logon. `install` starts nothing now: Holocron is already running from C7. Verify the entry yourself if you like: `schtasks /Query /TN Holocron /V /FO LIST`.

### C10. Write it down

Note the cutover time, the `holocron version` line, the "before" `archive stats` and the first `status`. The old system stays exactly as it was: tasks disabled, files in place, `camera_creds.xml` untouched.

## The first week

Daily, one minute:

```powershell
& $h --config $c status
& $h --config $c archive stats
& $h --config $c logs --level warn --since 24h
```

**Expect:** `status` exit 0 with every camera `recording relay` and few restarts; `archive stats` showing the NEWEST segment of each camera within the last 15 minutes, free space above the reserve, and a GB/day close to what P6 recorded; `logs` empty or explained. Also read the alerts: the same query as in [alerts.md](alerts.md), with `104` added to the ids (Holocron's web app stopped repeatedly; the eventlog text keeps the `IPCCTV: ` prefix, source-checked).

| Alert | Level | Do |
|---|---|---|
| 100 / 101 | Error / Warning | the supervisor stopped unexpectedly / is recording again: note it; more than one a day is a rollback trigger |
| 102 | Error | restart budget spent, it STAYS DOWN: roll back now |
| 103 | Warning | the console window was closed (visible mode): `holocron up --detach` |
| 104 | Error | the web app crashed 5 times in 30 minutes; recording is unaffected: `logs --component web` |
| 110 | Warning | a camera made no file progress for about 15.6 minutes: check the camera, `cam test` |
| 120 / 121 / 122 | Error | pruning disabled / blocked below the reserve / age-rule breaker held: the disk is at risk; roll back now |
| 130 | Error | the relay refused Holocron's own login: `holocron restart` |

(Meanings from [alerts.md](alerts.md) and the Go README; 104 is Holocron's only.)

**Checks to do once, in the first two days** (the ones the shadow could not cover: "real cameras, sleep and resume are Operator checks", shadow.md):
1. **Disk and the pruner.** When free space first reaches the reserve, `status` shows `prune: ... deleted N, freed X GB`, `archive stats` shows the OLDEST segment advance, and the newest never disappears. Preview anytime with `holocron prune --dry-run`.
2. **Logoff and logon.** Sign out and in again (not a restart). **Expect:** Holocron is recording again without you (`status`), console window visible or not as chosen.
3. **Reboot.** Same, after a restart.
4. **Sleep and resume** if the PC ever sleeps: `status` after resume shows `recording`, and `logs --since 1h` explains a gap.
5. **Closing the window** (visible mode) is a real stop; you can try it once on purpose: expect alert 103 and no recording until `holocron up --detach`. Optional, and it costs a gap.

**The clock change.** A cutover on or after 27 Oct 2026 does not meet it; the next one is **28 Mar 2027 (spring)**, covered only by unit tests and a synthetic clock (UNVERIFIED in production). Names carry the UTC offset, so the repeated hour of the autumn change does not overwrite files, and intervals are measured in UTC. If you cut over inside the closed window by explicit decision: on the night of 24 to 25 Oct watch the cameras' newest segment names change from `+0300` to `+0200`, check there is no overwrite and no gap beyond a normal boundary, and keep the rollback ready.

**When it has run a week** the migration plan's gate is met. Removing the old system is a separate decision this page does not take: nothing here deletes it.

## Known gaps of this page

- UNVERIFIED: `up --detach` against six real cameras on the Operator's machine; every `Expect` wording is from source, not from a run here.
- UNVERIFIED: whether the four cameras that were not shadowed behave like the two that were.
- UNVERIFIED: the Defender steps (general Windows UI), the firewall rule for `holocron.exe`, and the exact `Target` line of the launcher stub.
- UNVERIFIED: that read-only commands never create `_logs\holocron.log` on the old root.
- Not covered: the Windows service mode (needs elevation, not tested; [autostart.md](autostart.md)), a real clock change.

## Appendix: Linux and macOS

There is no PowerShell system to leave on these systems; this is the same program, started and stopped the same way, with its OS entry. Commands are `holocron ...` with `--config` as above (the default configuration folder is `$XDG_CONFIG_HOME/holocron` on Linux).

**Linux (systemd).** The unit installation was exercised in WSL2 with a test-named unit (install, status, enable, disable, second install unchanged, `systemd-analyze --user verify` clean, uninstall; CONFIRMED, 2026-10-21, [autostart.md](autostart.md)); it has not been run on a real Linux host with real cameras (UNVERIFIED).

```bash
holocron --config ~/.config/holocron/holocron.config.json config validate
holocron --config ~/.config/holocron/holocron.config.json doctor
printf '%s\n' "$CAMERA_USER" | holocron --config ... secret set camera.user       # value from standard input only
holocron --config ... install --dry-run && holocron --config ... install
loginctl enable-linger "$USER"        # only if it must start at boot without a login; install prints it, never runs it
systemctl --user start holocron.service
systemctl --user status holocron.service
journalctl --user -u holocron.service
```

Stop and way back: `systemctl --user stop holocron.service` (an ordered, requested stop, the same as `holocron down`), then `holocron autostart disable` or `holocron uninstall`. The unit runs `holocron --config ... host --service`; systemd restarts it on failure after 10 s but never after exit 20 (another supervisor holds the archive) or 21 (configuration fault). It is a user unit, never `sudo`.

**macOS (launchd), not tested on a Mac - UNVERIFIED.** `holocron install` writes `~/Library/LaunchAgents/com.holocron.nvr.plist` and enables it; launchd loads it at your next login (`launchctl bootstrap gui/<uid> <plist>` loads it now). The process-lifetime guarantees are weaker on macOS (no kernel kill-on-close; documented limit, `internal/osx`). Use the same checks as above with `holocron status`, `doctor` and `archive stats`, and do not rely on this page for a macOS cutover without testing it first.
