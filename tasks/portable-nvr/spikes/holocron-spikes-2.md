# Holocron spikes, round 2 (SP2, SP3)

Run 2026-10-20 on the Operator's machine (Windows 11 build 22621, Windows Terminal 1.24.12741, WSL2 Ubuntu 20.04, Go 1.27.2). Experiment code:
`spikes/sp2`, `spikes/sp3` (Go, standard library only, no `golang.org/x/sys`), drivers in `tests/holocron/` (`sp23-lib.ps1`, `sp2-run.ps1`, ...).
All timings are wall-clock milliseconds on one machine (the driver and the program read the same clock); "t0" is the instant before the
action (the `PostMessage`, the `TerminateProcess`, the call that sends the ctrl event). Each route was run ONCE unless a row says otherwise.
Scratch folders (`C:\Windows\Temp\holocron-sp2`, WSL `/tmp/holocron-sp3`) were removed at the end.

## SP2 - closing the terminal window (Windows Terminal and conhost)

**Question.** When the window of a console program is closed, does a Go program get a termination signal, how long does it have, can its
handler do cleanup in that time, and do its children (in a kill-on-close job) die? What happens when the terminal itself dies abruptly
(no close message)? (HLD 5, LLD 2.9 `OnConsoleClose`, IS G1 G2, todo [50] [54] [66] [67] [81] [84].)

**What the Go runtime does (read in the toolchain, `$GOROOT/src/runtime/os_windows.go:1047`, `syscall/types_windows.go:62`).** The runtime installs
one console handler at start. `CTRL_C_EVENT` and `CTRL_BREAK_EVENT` become `SIGINT`; `CTRL_CLOSE_EVENT`, `CTRL_LOGOFF_EVENT` and `CTRL_SHUTDOWN_EVENT`
become `SIGTERM`. If a `signal.Notify` receiver exists for that signal the handler delivers it and, **for SIGTERM, blocks the handler
thread forever** ("Windows terminates the process after this handler returns"), so the OS timeout is the only limit; with no receiver it returns
FALSE and the default action ends the process at once. **There is no mapping to `SIGHUP`**: `Notify(SIGHUP)` is harmless but never fires on Windows.

**Method.** `spikes/sp2` (`sp2.exe -dir D -tag T`): `signal.Notify(os.Interrupt, SIGTERM, SIGHUP)` plus a second, independent
`SetConsoleCtrlHandler` registered through `syscall.NewCallback` (registered after Go's, so it runs first, logs the raw event number and
returns FALSE so Go's handler still runs). Every event writes a timestamped line; on the Go signal the program writes "cleanup" lines and then
keeps ticking every 250 ms for 10 s more (so survival time is read from the last tick), exiting normally if it survives. At start it creates a
job with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` (handle held, non-inheritable), starts two `ping -t 127.0.0.1` children into it, one sharing
sp2's console (`shared`) and one with `CREATE_NO_WINDOW` (own hidden console, `own`), and writes the three pids to a file. It also logs
whether `user32.dll` is loaded and, from the second round of the abrupt routes on, a console probe (`GetConsoleProcessList`,
`GetConsoleMode(stdin)`, `GetConsoleWindow`, logged when the answer changes). Driver: `tests/holocron/sp2-run.ps1 -Route <r>` run from
`C:\Windows\Temp` via `powershell.exe -NoProfile -ExecutionPolicy Bypass -File`.
- `wt`: `wt.exe -w holocron-sp2 new-tab --title holocron-sp2 --suppressApplicationTitle sp2.exe -dir ... -tag wt -max 35` (started minimized, minimized again by handle the
  moment it appears; `--suppressApplicationTitle` was added to the brief's command, otherwise the console's own title replaces the tab title),
  closed by `PostMessage(hwnd, WM_CLOSE)`. The handle is found by EXACT class `CASCADIA_HOSTING_WINDOW_CLASS` and title `holocron-sp2`, and
  closed only if its owner process is `WindowsTerminal.exe` and sp2's parent is that process.
- `conhost`: `cmd /c start "holocron-sp2c" /min sp2.exe ...`, closed by `WM_CLOSE`. Proof it is ours: sp2's own `GetConsoleWindow()` equals the handle found by title.
- `wtkill`, `conhostkill` (abrupt): terminate the console host of the tab/window: for WT the one new `OpenConsole.exe` since launch (child of the WT
  process, born within 3 s of sp2), for conhost the one new `conhost.exe` whose parent is sp2. The `WindowsTerminal.exe` process itself was NOT terminated: see the finding below.
- `ctrlc`, `ctrlbreak`: sp2 in its own hidden console (`Start-Process -WindowStyle Hidden`, no window at all); `sp2.exe -sendctrl PID c|break` frees its console, `AttachConsole(PID)` and `GenerateConsoleCtrlEvent(event, 0)`.
- Tab close with Ctrl+Shift+W: **not run**. Windows Terminal has no command-line or message that closes one tab, and key injection needs foreground focus on the Operator's session (a mis-aimed keystroke would hit a real window).

**Process model found (matters for every WT test).** Windows Terminal 1.24 runs ALL non-elevated windows in ONE `WindowsTerminal.exe` process
(pid 57244, which also hosts the Operator's windows; a second, elevated instance is a separate process). `wt -w holocron-sp2` did not start a
new process: sp2's parent was that same process and the window was an additional top-level window of it; closing the window left the process
running. Every tab has its own `OpenConsole.exe` child (the pty host). So "terminate the terminal process" is impossible without taking the
Operator's windows with it; the pty host of the tab is the closest safe analogue. Consequence for the product: `cam.nvr`'s window is one window of a
shared process; a window close never exits the terminal process.

**Raw results (ms after t0).**

| Route | Event seen by sp2 (raw / Go) | Cleanup line written | sp2 died | `shared` child died | `own` child died | notes |
|---|---|---|---|---|---|---|
| 1. WT, WM_CLOSE | +56 `CTRL_CLOSE_EVENT` (2) / +66 `SIGTERM` | +68 (2 ms after the signal) | **+5093** (5037 after the event); ticks seen until +4919 | +83 | **+5093** (same ms as sp2) | window gone; WT process alive |
| 3. conhost, WM_CLOSE | +6 `CTRL_CLOSE_EVENT` / +6 `SIGTERM` | +12 | **+5043** (5037 after the event); ticks until +4705 | +6 | **+5043** | window gone |
| 5a. CTRL_C (hidden console) | +44 `CTRL_C_EVENT` (0) / +44 `SIGINT` | +44 | survived by design, normal exit +10074 | +352 (ping dies on Ctrl+C) | +10094 (20 ms after sp2) | t0 includes spawning the sender |
| 5b. CTRL_BREAK (hidden console) | +29 `CTRL_BREAK_EVENT` (1) / +30 `SIGINT` | +30 | normal exit +10286 | not killed by the event (ping prints statistics); died +10302 | +10296 | |
| 4a. WT, kill the tab's `OpenConsole.exe` (run 1) | **none** | - | **alive after 25 s** (killed by pid) | alive after 25 s | alive after 25 s | tab and window vanished |
| 4b. conhost, kill that window's `conhost.exe` (run 1) | **none** | - | **alive after 25 s** (killed by pid) | alive | alive | window vanished |
| 4a'. as 4a, run 2 with the console probe | none; probe changed at **+802** | - | alive after 15 s | alive | alive | probe: `GetConsoleProcessList=0` "No process is on the other end of the pipe", `GetConsoleMode(stdin)` same error, `GetConsoleWindow=0x0` |
| 4b'. as 4b, run 2 with the probe | none; probe changed at **+476** | - | alive after 15 s | alive | alive | same three signs |

Other measurements. A bare Go console program does not load `user32.dll` (`user32Loaded=false` in every run). The raw handler (via
`syscall.NewCallback`, called on a thread the OS creates) worked and ran 0 to 10 ms before the Go signal arrived. The first WT attempt failed in
the driver before anything was closed (a lookup bug; nothing was closed or killed; the window closed itself when sp2's failsafe expired); it is not
counted as a route. Route 5b (hidden console, second run with the probe: sp2 +10296, children +10318 and +10296) and both routes 4 (probe added) were run twice and agree.

**Visible window time used.** Windows Terminal ignores the minimized start request and shows its window until the driver minimizes it:
15 ms (route 1), 4 ms (4a), 900 ms (4a'); the classic console windows started minimized (0). Windows existed minimized for about 3 to 6 s per
route. The failed first WT attempt left a window up for at most 90 s (sp2's default failsafe; whether it was minimized was not recorded, so the whole
90 s is counted). Total visible time: at most about 92 s.

**Conclusion.**
1. **An in-process console handler suffices for a window close, in Windows Terminal and in conhost.** Go delivers the close as `SIGTERM`
   (mapped from `CTRL_CLOSE_EVENT`) within 6 to 66 ms of the close message, and the process then has **5.04 s** (5.037 s after the event, both
   routes; the OS value, measured, is the same in the two hosts) before it is terminated. The handler goroutine can run ordinary code: the final
   line was written 2 ms after the signal. It is Go's own handler that keeps the process alive for those 5 s (it blocks), so cleanup must
   finish inside the budget and the process should `os.Exit` itself when done.
2. **The children die with the supervisor, by the job, not by the console event.** The child in its own console survived the event and died
   in the same millisecond as sp2 (+5093 and +5093; +5043 and +5043), which is the kill-on-close job working on sp2's death. The child that shares
   the console is killed by the event itself within 6 to 83 ms (ping; ffmpeg's own handling of `CTRL_CLOSE` is untested). So with shared-console
   children there is NO ordered stop on a window close: every child gets the event at once (the reason the PowerShell system uses a stop-flag
   file, IS G6). If the Go supervisor wants an ordered stop inside the 5 s budget, start children in their own console (`CREATE_NO_WINDOW` worked) so
   that only the supervisor sees the close.
3. **An in-process handler does NOT suffice when the console host dies without a close message** (the abrupt routes 4a/4b): no event at all, sp2
   and both children keep running indefinitely (at least 25 s here, and until killed by pid) with a dead console: this is the failure the PowerShell
   warden exists for. It is detectable in-process, however: with a console probe polled at 4 Hz the loss showed within 0.5 to 0.8 s as
   `GetConsoleProcessList()==0` / `GetConsoleMode(stdin)` failing with ERROR_BAD_PIPE / `GetConsoleWindow()==0`. On probe failure the supervisor can
   run the same ordered stop as for a close.
4. Ctrl+C and Ctrl+Break both arrive as `SIGINT` (raw 0 and 1 are distinguishable only with a raw handler); `SIGHUP` never arrives on Windows.

**What it changes for W8.** `OnConsoleClose` on Windows = `signal.Notify(SIGTERM, SIGINT)` (plus no-op SIGHUP) AND a console-liveness probe
(about 4 Hz: `GetConsoleProcessList` returning 0, or the console window handle becoming 0) that feeds the same handler; record
`StopKind=window-closed` in both cases. **A keeper (warden) process is not needed for window-close semantics**, provided the supervisor binds its
children with a kill-on-close job (SP3) and carries the probe. The 5 s budget is the hard limit for the ordered stop in attached mode; stop
order and the final status write must fit in it (the stop-request path with 20 s+ timeouts is for service mode). A keeper would still be the only
defence if the supervisor itself is hung or killed while the console host stays up; that case is covered by the job and the host's restart rule, not by the window. Do
not load `user32.dll`/`gdi32.dll` into the supervisor process: the SetConsoleCtrlHandler documentation (recalled from memory, not re-read, and not tested here) says such processes do not get the logoff and shutdown
events; the bare Go binary does not load them. Windows Terminal hosts every window in one process, so a design that waits for "the
terminal process" to exit would never fire; use the console events and the probe.

**Confidence.** High for routes 1 and 3 (single runs, but two hosts agree to 6 ms on the 5 s budget and on the job behaviour) and for the abrupt
no-event result (two runs each, four of four agree). Medium for generalising "abrupt" to a killed `WindowsTerminal.exe` (not run: shared with the
Operator's windows; the pty host kill is the proxy), and for a tab close in a multi-tab window (not run; likely the same ConPTY close path, unverified). Untested: logoff/shutdown events, a hung
supervisor, ffmpeg's reaction to `CTRL_CLOSE`, elevated windows, `windowingBehavior` other than the machine's current setting.

## SP3 - children die with the supervisor

**Question.** Does the kernel-level binding work from Go: on Windows a kill-on-close job object (with and without nesting, with a
grandchild, with the creation race), on Linux `Pdeathsig` with a locked OS thread (and the unlocked pitfall), process groups for the
orderly stop; what does the registry reap add, does it spare an unrelated process that merely has a recorded pid; what is left on macOS? (IS G5,
which has no check today; HLD 5; LLD 2.9.)

**Method.** `spikes/sp3` (one Go binary, standard library only, no `x/sys`; builds and vets for windows, linux and darwin).
`sp3 supervise` starts N children and holds the binding; `sp3 leaf` is a child that can start a grandchild at once (`-spawn`), live a fixed
time (`-life`) or ask for breakaway (`-breakaway`); `sp3 reap` is what a NEW supervisor does at start; `sp3 regfake` writes the negative cases;
`sp3 wrap` is the stdin-pipe shim; `sp3 jobwrap` runs a command inside an outer job. Long-running harmless processes: `PING.EXE -t 127.0.0.1`
(Windows, always `CREATE_NO_WINDOW`) and `/usr/bin/sleep 600` (Linux). Drivers: `tests/holocron/sp3-win.ps1 -Case w1..w9` and
`tests/holocron/sp3-lin.sh l1..l10` (cases below). Each case: start the supervisor, wait for its state file, settle 1.5 s, snapshot the whole process tree (Windows: CIM parent links plus the
grandchild pids the leaves log themselves; Linux: `ps` parent links), hard-kill the supervisor (`Stop-Process -Force` = `TerminateProcess`;
`kill -9`), watch every snapshot pid for 5 s (Windows: handles opened before the kill, so a reused pid cannot be mistaken for a survivor;
Linux: `/proc/<pid>/stat`, a zombie counts as dead), kill survivors by pid. Each case ran once (two Windows cases were re-run after a driver/test-program bug, listed under "Corrections" below).
Windows details: the job is created with `CreateJobObjectW(NULL, NULL)` (unnamed, default security: the driver reads the handle flags and
logs `inheritable=false`), `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` (`0x2000`), plus `JOB_OBJECT_LIMIT_BREAKAWAY_OK` (`0x800`) in case w5. **Every
Windows case ran with the supervisor already inside a job** (the supervisor logs `IsProcessInJob`: true in every run; the harness the commands
run under supplies an outer job, and it does not permit breakaway, so a non-nested control could not be produced), so "nested jobs" is the normal case here, not only w7.
Registry entry = `{pid, start, image, role}`: Windows `start` = creation FILETIME (`GetProcessTimes`), image = `QueryFullProcessImageNameW`
(compared case-insensitively); Linux `start` = `<boot_id>:<starttime ticks>` from `/proc/<pid>/stat` field 22, image = `readlink /proc/<pid>/exe`.

**Raw results, Windows** (survivors counted over the snapshot, which also contains each child's console host `conhost.exe`; "ms" = first
observation after `TerminateProcess` returned to the driver, so it includes the driver's own call latency of about 30 to 60 ms: read it as an upper bound):

| Case | Binding variant | Snapshot (excl. supervisor) | Survivors in 5 s | Gone after |
|---|---|---|---|---|
| w1 | job, child created `CREATE_SUSPENDED`, assigned, resumed; 3 x ping | 7 (3 ping, 4 conhost) | **0** | 49 ms |
| w2 | job, child created running, assigned AFTER a 300 ms gap on purpose; each child = `leaf -spawn` (starts a ping grandchild at once) | 13 | **6 = the 3 grandchildren + their 3 console hosts** (the 3 leaves, assigned late, died) | - |
| w2a | as w2 but the children are ping itself (nothing to spawn) | 7 | 0 | 36 ms |
| w2b | as w2 with NO gap (assignment right after `CreateProcess`, the natural case) | 13 | 0: assignment finished in the same millisecond, before the Go leaf (about 10 ms of startup) could spawn | 94 ms |
| w3 | `CREATE_SUSPENDED` + the same 300 ms gap, leaf with grandchild | 13 | **0** | 60 ms |
| w4 | `PROC_THREAD_ATTRIBUTE_JOB_LIST` (job set at creation), 300 ms gap, leaf with grandchild | 13 | **0** | 42 ms |
| w5 | job with `BREAKAWAY_OK`; leaf starts its grandchild with `CREATE_BREAKAWAY_FROM_JOB` | 13 | **6 = 3 breakaway grandchildren + 3 console hosts** | - |
| w6 | job WITHOUT `BREAKAWAY_OK`; leaf asks for breakaway | 7 | 0; every `CreateProcess` with the flag failed `Access is denied` (nothing escaped, nothing started) | 45 ms |
| w7 | outer job (plain, created by `jobwrap`) holding the supervisor, which has its own kill-on-close job; leaf with grandchild | 13 | **0** (outer job pid list = supervisor, 3 leaves, 3 grandchildren and their console hosts, 14 pids) | 66 ms |
| w8 | kill landed with the short child (life 1500 ms) exiting: its own exit line is stamped +11 ms after t0; 2 long pings | 7 | **0** | 58 ms |
| w9 | NO job (control), registry written | 7 | **6 = 3 ping + 3 console hosts** (orphans; used for the reap below) | - |

**Registry reap, Windows (w9) and Linux (l9).** Orphans left by the unprotected supervisor: 3 of 3 killed by a new `sp3 reap` run (each
verified: pid alive, same start time, same image path). Negative cases added to the file by `regfake` before the reap, all pointing at an unrelated
process I started and recorded (a ping / a sleep, "decoy"): `wrongstart` (decoy pid with a different start time = a pid reused by another process), `wrongimage` (right
start time, different image path), `dead` (a pid that does not exist). Verdicts, identical on both OSes: `skip: start time differs (pid reused)`,
`skip: image path differs`, `skip: not running`; **the decoy was still alive after the reap on both** (Windows `decoy alive = True`, Linux `True`), then killed by pid.

**Raw results, Linux (WSL2, Ubuntu 20.04, kernel 6.18; `kill -9` of the supervisor unless noted):**

| Case | Variant | Result |
|---|---|---|
| l1 | `Pdeathsig=SIGKILL` + `Setpgid` (one shared group); children created by the MAIN goroutine locked with `runtime.LockOSThread` for the supervisor's life | **0 survivors**, children gone +12..+22 ms (supervisor +7) |
| l2 | same, children created by an UNLOCKED plain goroutine that then returns | **0 survivors** (+16..+29 ms): works, because Go does not end ordinary threads |
| l3 | PITFALL: the starter goroutine locks its thread, starts the children and RETURNS holding the lock, with the main goroutine pinned (so the starter is not on the main thread) | **all 3 children killed within 1 ms of starting, while the supervisor was alive** (supervisor log: `child exited: signal: killed` at the same millisecond as `started`) |
| l3b | same starter but the main goroutine is NOT pinned | children survived (the starter ran on the main thread, which Go never ends: separate check below); 0 survivors after the `kill -9` (+11..+19 ms): the outcome depends on which thread the scheduler gave the starter |
| l4 | control: starter locks, starts, UNLOCKS, returns (main pinned) | children alive for the 1.5 s while the supervisor lived; 0 survivors after `kill -9` (+11..+20 ms) |
| l5 | control: no `Pdeathsig` | **3 survivors** (orphans, reparented to init) |
| l6 | l1 plus each child starts a grandchild (`sleep`) | the 3 children die (+13..+28 ms), **the 3 grandchildren survive**: `Pdeathsig` is not inherited by the children's own children |
| l7 | orderly stop of l6: `SIGTERM` to the supervisor, which sends `kill(-pgid, SIGTERM)`, waits 1.5 s, `kill(-pgid, SIGKILL)` | **0 survivors, grandchildren included**: all 6 gone +9..+31 ms after the TERM; the supervisor exited 0 at +1507 ms (its own fixed grace) |
| l8 | `kill -9` with the short child (1.5 s life) just gone: its exit stamped 2 ms BEFORE the kill (the reaper had logged it) | 0 survivors (+12..+20 ms) |
| l9 | no `Pdeathsig`, registry, then reap + negative cases | 3 orphans killed by the reap, decoy untouched, three fakes skipped (see above) |
| l10 | no `Pdeathsig`, children started through `sp3 wrap` with a stdin pipe held by the supervisor | **0 survivors**: each wrapper saw EOF and killed its child's group; wrapper + child gone +16..+51 ms |

Thread check behind l3/l3b (`/tmp/.../tidtest`): a goroutine that locks its thread and exits on a non-main thread ends the thread
(`/proc/self/task/<tid>` gone); when it happens to run on the main thread the thread stays. The runtime source says why
(`goexit0`, "Kill it rather than returning it to the thread pool"; the main thread is never exited).

**Corrections made during the run (all before the final numbers).** (a) The first Windows run crashed the supervisor with a Go
"all goroutines are asleep" deadlock (`select {}` with no other goroutine): the children died with the crashed process, which is
accidentally a fourth proof of the job, and the program now sleeps in a loop instead. (b) The same bug in the leaf made the first w2/w3 runs bogus (the
leaf died after 300 ms, so the late assignment returned `Access is denied` on an exited process; the grandchildren it had started survived for that reason);
w2 and w3 were re-run after the fix, and the leftover pings from the bogus run were killed by pid. (c) w8 was first timed from the supervisor's clock and
landed 215 ms before the child's exit; it was re-run timed from the child's own start stamp (the table row). (d) The Linux case l3 was first run
without pinning the main goroutine and showed the children surviving; that led to the thread check and the l3/l3b split.

**Conclusion.**
1. **Windows: the kill-on-close job works from Go and holds under a hard kill.** In every protected case (w1, w3, w4, w7, w8, w2a, w2b) all
   descendants were gone within about 100 ms of `TerminateProcess` (upper bound, see above). It works with the supervisor already inside another
   job (nested jobs, Windows 8+; observed in all runs and with an explicit outer job in w7), including grandchildren: a child's own children inherit
   membership in the child's job automatically.
2. **The only hole is the gap between `CreateProcess` and `AssignProcessToJobObject`.** A child that starts a process before it is assigned leaves that
   process outside the job for good (w2: 3 of 3 escaped with a 300 ms gap). Without an artificial gap the race was not lost in 3 of 3 (w2b), but that depends on
   the child's startup time and is not a guarantee. Both fixes are verified: **create suspended, assign, resume (w3)**, or **create inside the job with the job-list
   attribute (w4, atomic)**. Go's `os/exec` can set `CREATE_SUSPENDED` but throws away the thread handle, so resuming needs `syscall.CreateProcess` directly
   (as the spike does) or a Toolhelp thread lookup; the job-list attribute needs a hand-built `STARTUPINFOEX` (about 40 lines, standard library only, as the
   spike shows; Windows 10+, recalled and consistent with this Windows 11 machine, not tested on older Windows).
3. **`JOB_OBJECT_LIMIT_BREAKAWAY_OK` must not be set** on the supervisor's job: with it a child can start a grandchild that leaves the job and survives the supervisor
   (w5: 3 of 3). Without it a breakaway request fails with access denied and nothing escapes (w6). The job handle is not inheritable.
4. **Linux: `Pdeathsig=SIGKILL` + `Setpgid` takes the direct children with `kill -9` of the supervisor (l1, l2: 0 survivors in about 30 ms), but the guarantee is
   bound to the creating thread, not the process.** If the thread that forked the child ends, the child is killed at once, supervisor alive (l3). A locked
   goroutine that returns without unlocking ends its thread, which is how this happens in practice; an unlocked starter works only because Go
   does not retire threads, which is an implementation detail (and l3b shows the outcome can depend on where the scheduler put the goroutine). The rule is: one dedicated
   goroutine creates every child (requests arrive on a channel), calls `runtime.LockOSThread()` first and never returns or unlocks until the supervisor shuts down.
5. **`Pdeathsig` does not reach grandchildren** (l6: 3 of 3 survive), and a hard kill leaves them. The orderly stop catches them through the shared process group
   (`kill(-pgid)`, l7). For a hard kill only the registry (by group or by recorded descendants) or the init system (systemd `KillMode=control-group`, not tested) can.
6. **The registry reap is safe and effective on both OSes.** It killed 3 of 3 provable orphans, and spared a live unrelated process whose pid was recorded with a wrong start time or a wrong
   image, and ignored a dead pid. It is the only mechanism that works without a kernel binding (macOS), at the price of acting at the NEXT start.
7. The stdin-pipe shim works (l10: 0 survivors, +16..+51 ms) on Linux, and is portable code (builds for darwin; not run there).

**Per-OS guarantee table.**

| | Windows | Linux | macOS (paper analysis, nothing run) |
|---|---|---|---|
| Supervisor dies abruptly, direct children | **Guaranteed** by the kill-on-close job once each child is in it (w1, w3, w4, w7), about 0.1 s | **Guaranteed** by `Pdeathsig` if created by one locked, never-returning goroutine/thread (l1); NOT if that thread ends (l3) | **Not guaranteed**: SIGKILL of the supervisor leaves every child running (reparented to launchd) |
| Grandchildren | Guaranteed (membership inherited), unless `BREAKAWAY_OK` is set (w5) | **Not guaranteed** (l6); orderly stop reaches them via the group (l7) | Orderly stop only, via the group |
| Race window | Between `CreateProcess` and assignment: real (w2). Closed by `CREATE_SUSPENDED` (w3) or `PROC_THREAD_ATTRIBUTE_JOB_LIST` (w4) | None for the first child; but the supervisor must not die before the child exists: nothing to protect then | Between `fork` and the registry write: an unregistered orphan on a hard kill |
| Already inside another job / container | Works (nested jobs; every run here) | n/a (cgroups/containers do not interfere with `Pdeathsig`; untested) | n/a |
| Orderly stop | Close or terminate the job (all members); stop order is the supervisor's own | `kill(-pgid, SIGTERM)`, grace, `kill(-pgid, SIGKILL)` (l7) | Same as Linux (`Setpgid`, `kill(-pgid)`; POSIX behaviour, unverified on macOS) |
| What the registry reap adds | Orphans after any failure of the binding (assign error, job creation failure) and from builds that skipped the job (w9) | Same, and the grandchildren if the entry records the group id (not implemented in the spike) | The only recovery, at next start; also stops provable orphans of an older crashed supervisor |
| Residual | A child that cannot be assigned (`AssignProcessToJobObject` fails) is unprotected: treat as fatal for that child | Thread-end pitfall; grandchildren; `Pdeathsig` is cleared on exec of a set-uid file and on credential changes (man page, not tested) | Hard-kill gap until the next start unless the shim or launchd is used |

**macOS (no run possible).** What `Setpgid` gives: children in their own process group, so an orderly stop is one `kill(-pgid, ...)` that also reaches
grandchildren which stayed in the group; it also keeps terminal-generated signals (Ctrl-C to the foreground group) away from children, so the supervisor
decides the stop order (POSIX semantics, not verified on macOS). What it cannot give: any kernel action when the supervisor dies: there is no `Pdeathsig`, no job object and no
subreaper that kills. What the registry reap gives: recovery of provable orphans when the next supervisor starts (pid + start time via `sysctl`/`proc_pidinfo`
+ image path via `proc_pidpath`, same verification as the spike), never by name; it cannot protect the interval before that start, and a child that called `setsid` or changed
its process group escapes the group kill. What the stdin-pipe shim gives (verified on Linux, l10): a small wrapper per child that watches its stdin and kills the child's
group when the supervisor's pipe end closes, i.e. within about 50 ms of a supervisor SIGKILL; it costs one extra process per child, the child's stdin is the shim's, and a
SIGKILL of the shim itself orphans the child (the registry then covers it). `kqueue` `EVFILT_PROC`/`NOTE_EXIT` on the parent pid from the shim would do the same without the pipe (not tried).
If the supervisor runs as a launchd agent, launchd cleans up the job's remaining processes when the main process exits unless `AbandonProcessGroup` is set (recalled
from launchd.plist(5), not verified): then the service manager supplies the missing guarantee. Recommendation: Setpgid + registry reap + the shim for the long-lived children, state the residual (a hard kill of the shim) in the docs.

**What it changes for W8 (precise).**
1. **No keeper process.** Window close is handled in-process (SP2: handler + console probe, 5 s budget) and the children are bound by the kernel (this spike).
   A keeper's only unique value would be surviving the supervisor to clean up after a hard kill, and the job (Windows) and `Pdeathsig` (Linux) already do that without a second process.
2. **Windows `osx.NewProcessScope`:** create the unnamed kill-on-close job FIRST, handle held in the supervisor for its whole life and never inheritable, no `BREAKAWAY_OK`.
   Start every child with **`syscall.CreateProcess` + `CREATE_SUSPENDED` | `CREATE_NO_WINDOW`, `AssignProcessToJobObject`, then `ResumeThread`** (works on every
   Windows with nested jobs; verified). The job-list attribute is an acceptable alternative where Windows 10+ is guaranteed; keep one path, the suspended one,
   so there is a single code path to test. Treat an assign failure as fatal for that child (kill it, do not run it unprotected). Do not use `os/exec` Start then Assign.
3. **Linux:** `Setpgid` (one shared group per scope) + `Pdeathsig: SIGKILL`; create ALL children from one dedicated goroutine that calls `runtime.LockOSThread()` at its start and
   does not return until the scope is closed (feed it start requests over a channel); never rely on an unlocked starter. Orderly stop = `kill(-pgid, SIGTERM)`, grace, `kill(-pgid, SIGKILL)`.
   Grandchildren are covered only by the group kill; add the group id to the registry entry if any child is known to spawn long-lived helpers.
4. **Registry on every OS:** write `_state/children.json` (pid, start-time token, image path, plus group id on Unix, boot id inside the token on Linux) atomically right after each start; at
   supervisor start, BEFORE starting children, verify each entry (alive, same start, same image, case-insensitive image on Windows) and stop provable orphans; skip and log everything else (the three
   negative cases pass). On macOS this is the primary mechanism; on the other two it is the second layer.
5. **macOS:** document the limit (hard kill orphans until the next start, or until the shim notices) and ship the shim for ffmpeg and MediaMTX children; launchd `AbandonProcessGroup=false` (the default) as an additional net, to be verified on a Mac.

**Confidence.** High for the Windows and Linux binding results (each case run once, 13 of 13 outcomes unambiguous, mechanisms confirmed by the child logs and by the Go runtime source). Medium for "time to death":
the Windows figures include the driver's `TerminateProcess` latency; the Linux figures include a 10 ms poll. Not covered: Windows older than 10 (job-list attribute), a non-nested supervisor (could not be produced),
real `ffmpeg.exe`/`mediamtx.exe` (ping and sleep stand in; ffmpeg's own children, if any, were not examined), many children (N=3 only), the supervisor dying during `CreateProcess` itself,
set-uid children, containers/cgroups, systemd `KillMode`, anything on macOS (paper only; the shim and the registry code compile for darwin).

## Process hygiene and run record

Before and after lists of `sp2`, `sp3`, `ping`, `sleep`, `wt`/`WindowsTerminal`, `conhost`, `OpenConsole` were taken (the "before" list of the SP2 session was lost when the PC rebooted
during the work; the list taken at the start of SP3 and the list at the end contain no `sp2`, `sp3`, `ping` or `sleep` of mine; the `conhost`/`OpenConsole`/`WindowsTerminal` entries are
the Operator's). Every process started by the experiments was recorded by pid and, where it outlived its supervisor on purpose (negative controls w2, w5, w9, l5, l6, l9), killed by pid at the end of its case.
