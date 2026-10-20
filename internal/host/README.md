# internal/host

The NVR host: runs the supervisor as a CHILD process and restarts it in place after an unexpected stop. Port of
`Start-CamNvrHost` (cam.common.ps1 ~4919-5054) plus the window-close duty of the NVR warden (~4634-4903), without a
warden process (SP2/SP3, LLD 11.1). Standard library plus `clock`, `logx`, `osx`, `status`.

## API

```go
type Spawner interface{ Spawn(ctx) (Proc, error) }          // ExecSpawner{Scope *osx.Scope; Path; Args; Env; Dir; Stdout, Stderr}
type Proc interface{ Pid() int; Wait() (int, error); Kill() error } // osx.Proc
type Alerts interface{ Raise(Event) }                       // adapter: alerts.FromFields(e.ID, e.Severity, e.Title, "", e.Message, e.Key)
type Event struct{ ID int; Severity, Title, Message, Key string }
type Locker interface{ Acquire() (release func(), owner string, err error) } // FileLock{Path} (osx lock); supervise.OSLock fits too
type Console interface{ OnConsoleClose(func(osx.Reason)) func(); WatchConsole(func(osx.Reason)) (func(), error) } // OSConsole{}
type Mode int                                               // Attached | Service
type Config struct{ StateDir string; MaxRestarts int /*5*/; RestartWindow /*30m*/, RestartDelay /*5s*/, LockWait /*20s*/,
                    AttachedStopBudget /*osx.ConsoleBudget 4s*/, StopTimeout /*40s*/, StatusPoll /*1s*/ time.Duration; WindowTitle string }
type Deps struct{ Spawner; Alerts; Clock clock.Clock; Log *slog.Logger; Status *status.Writer; ReadStatus func() (*status.Document, error);
                  Lock Locker; Console Console; Mode Mode; Location *time.Location }
func New(Config, Deps) *Host
func (*Host) Run(ctx) (Result, error)                       // ctx cancel = requested stop
type Result struct{ Outcome Outcome; Reason string; Lives, Restarts int }; func (Result) ExitCode() int // 0, 20, 21, 1
type Outcome string // requested | interrupted | window-closed | another-supervisor | config-fault | budget-spent | stop-failed
```

Wiring (W9): `scope, _ := osx.NewScope("host", osx.ScopeOptions{})`; `ExecSpawner{Scope: scope, Path: <self>, Args: ["supervisor", ...]}`;
the supervisor process uses `supervise.NewProduction(deps, root)`. The supervisor runs inside the host's scope (Windows job / Linux
locked starter + Pdeathsig): if the host dies, however abruptly, the supervisor dies, and its own scope takes its children. On Windows
the supervisor gets its own hidden console (`CREATE_NO_WINDOW`), so a window close reaches only the host.

## Rules

- **Classification from recorded state only** (IS 2.3): the stop-request file, the `StopKind` the supervisor persisted, and the two
  exit codes that are records themselves (20 another supervisor, 21 configuration fault). A persisted `StopKind` counts only when the
  document was written by THIS supervisor process (`SupervisorPid` equals the child's pid): a stale `requested` from an earlier run
  cannot hide a crash (the IS 1.5 "UNVERIFIED edge"; PowerShell has it). Exit 0 with nothing recorded is unexpected.
- **No restart** for requested, interrupted, window-closed, exit 20, exit 21. Exit 21 after a restart raises 102 (alert 100 had
  promised a restart). Exit 20 raises nothing (another supervisor is recording).
- **Unexpected stop**: wait up to 20 s for the instance lock (the OS frees a dead holder's lock at once; still held after 20 s =
  another supervisor: no restart, no write, no alert); holding the lock, write `State=stopped, StopKind=unexpected,
  LastUnexpectedStop{At, Reason, SupervisorPid, DetectedBy:"nvr host"}` over the last document (everything else, LastWindowClose
  included, carried; D19) unless the supervisor recorded its own; then budget check: 5 restarts per 30 min (monotonic clock) -> else
  alert 102 (Error) and stay down, ERROR log; otherwise alert 100 (Error), WARN log, 5 s delay, respawn. Alert 101 (Warning) when
  the restarted supervisor's own document reports `running` (PowerShell raises 101 right after the start; here "recording again" is
  observed). A spawn failure counts like a crash.
- **Attached mode, window close** (CTRL_CLOSE/LOGOFF/SHUTDOWN, SIGHUP, SIGTERM, the console probe's `console-gone`): write
  `stop.request`, wait for the supervisor within `AttachedStopBudget` minus a reserve (Windows ends the process 5.04 s after the
  event, SP2); a supervisor still running then is killed (its job/group takes its children). Then, holding the lock, write
  `StopKind=window-closed` + `LastWindowClose` (DetectedBy `"nvr warden"`, the schema's only value), raise 103 ONCE, return; never
  restart. A close during the restart delay also ends without restart (and 103). Ctrl+C is an orderly stop (`requested`, no alert).
- **Service mode / ctx**: every OS stop is `requested`: stop.request, wait `StopTimeout`, no alert. A supervisor that does not stop
  in time is NOT killed (G7): ERROR "DID NOT STOP", the request stays in place, `stop-failed`, exit 1.
- Logs: component `host`, fields action/actor/target/result/duration_ms/pid; INFO starts/stops, WARN restarts, ERROR budget,
  refusals and failed stops; every alert also as an `ALERT <id>` line.

## Guarantees -> tests

| IS 2.2 | Guarantee | Tests |
|---|---|---|
| G1 | Closing the terminal stops everything, nothing orphaned | `TestHostWindowCloseStopsOnceWithoutRestart` (close, console-gone, hangup, logoff), `TestHostWindowCloseKillsHungSupervisorAtBudget`; real: `TestRealHostWindowCloseEvent`, `TestRealHostConsoleDeathIsWindowClose` (Windows), `TestRealHostHangupIsWindowClose` (Linux); osx `TestRealWindowsConsoleCloseEvent`, `TestRealWindowsConsoleProbeFires` |
| G2 | A deliberate close is never repaired | `TestHostWindowCloseStopsOnceWithoutRestart`, `TestHostWindowCloseDuringRestartDelay`, `TestHostNeverRestartsDeliberateOrRefusedStops/window-closed` |
| G3 | One alert 103 on a close; an orderly stop raises none | `TestHostWindowCloseStopsOnceWithoutRestart`, `TestHostOrderlyStopRaisesNoAlert`, real `checkWindowClosed` (alerts == [103]) |
| G4 | Unexpected stop: 100, restart after 5 s, 101; 5 per 30 min then 102 and down; never requested/interrupted/window-closed/20 | `TestClassifyFromRecordedStateOnly`, `TestHostRestartsKilledSupervisor`, `TestHostBudgetSpentAtSixthCrash`, `TestHostBudgetRefillsAfterWindow`, `TestHostWaitsForPreviousLock`, `TestHostNeverRestartsDeliberateOrRefusedStops`, `TestHostKeepsSupervisorsOwnRecord`, `TestHostUnexpectedMinimalDocumentValidates`, `TestHostConfigFaultAfterRestartRaises102`, `TestHostSpawnFailureCountsAgainstBudget`; real `TestRealHostRestartsAfterHardKill`, `TestFullChainHostSupervisorFfmpeg` |
| G5 | Supervisor dies however abruptly -> its children die | osx `TestRealScopeHardKillLeavesNoSurvivors`, `TestRealScopeHardKillWithGrandchildren`, `TestRealScopeKillRacesStart`, `TestRealWindowsSuspendedClosesAssignGap`; `TestRealHostRestartsAfterHardKill`, `TestRealHostHardKillTakesEverything`, `TestFullChainHostSupervisorFfmpeg` |
| G6 | Orderly stop, no orphan | osx `TestRealLinuxOrderlyStopKillsGroup`; stop legs of `TestRealHostRestartsAfterHardKill` and `TestFullChainHostSupervisorFfmpeg`; supervise `TestSupervisorOrderedStop` (W7b) |
| G7 | The stop waits and verifies, never kills the supervisor | `TestHostServiceStopWaitsAndNeverKills` |
| G8 | One supervisor per archive | osx `TestRealLockTwoProcesses`, `TestLockInProcessSecondHolderRefused`; supervise `TestOSLockOneHolder`; `TestRealHostAnotherSupervisorNotRestarted`, `TestHostNeverRestartsDeliberateOrRefusedStops/exit_20` |

Every host-written document is validated against `status.schema.json` and its key set compared with the PowerShell patch goldens
(`patched-unexpected-over-running.json`, `patched-unexpected-minimal.json`, `patched-windowclose-over-running.json`).

Negative controls (code mutations, each made and reverted; every one fails its test): StopKind trusted without the pid check;
budget 6; window-closed restarted; exit 0 treated as clean; service stop kills on timeout; 103 raised twice; document written
without the lock. OS-level controls are in `internal/osx` (assign-after-start escape, returning starter thread, no probe, pid-only reap).

## Running

Deterministic tests run by default (fake spawner, fake clock, fake alerts and lock, real status files). Real-process tests re-execute
the test binary as a stand-in supervisor (`fakesup`: lock, reap, own scope with 2 children, heartbeat, honours stop.request) or as
the host itself; opt-in:

    HOLOCRON_HOST_REAL=1 go test -p 1 -run TestReal -v ./internal/host
    GOOS=windows go test -c -o /mnt/c/Windows/Temp/holocron-w8/host.test.exe ./internal/host
    cd /mnt/c/Windows/Temp/holocron-w8 && HOLOCRON_HOST_REAL=1 TMP='C:\Windows\Temp\holocron-w8\scratch' TEMP=... \
      WSLENV=HOLOCRON_HOST_REAL:TMP:TEMP ./host.test.exe -test.v -test.run TestReal

The deterministic tests read the status goldens from `testdata/`: run a Windows build of them with the package folder as the working
directory (`cd internal/host && /mnt/c/.../host.test.exe`). The Windows console tests start the host helper in a NEW hidden console of its own, find its `conhost.exe` by parent pid and its window
from the helper's own `GetConsoleWindow()` (no window is enumerated); they skip if the console was handed to a terminal.

Full chain (`HOLOCRON_IT=1`, `HOLOCRON_IT_FFMPEG` >= 5.0, skips otherwise): host + `supervise.NewProduction` + two real ffmpeg pointed
at silent loopback listeners 18700-18701 (MediaMTX is not used: the only copy on the test machine is the Operator's). Measured
2026-10-20 on Windows: supervisor and both ffmpeg gone 16 ms after the hard kill, restarted 6.1 s after it, alerts [100 101],
requested stop, nothing left.

## Deliberate differences from PowerShell

- StopKind trusted only from the dying supervisor's own document (see Rules); exit 21 does not restart (here 21 means configuration
  fault, PowerShell used it for "the loop threw"); 101 on observed recovery; window close handled in the host (no warden), with a
  console probe for the abrupt death of the console host; Ctrl+C in attached mode is an ordered stop recorded `requested`.
