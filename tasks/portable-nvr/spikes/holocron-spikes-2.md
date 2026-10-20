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
