# internal/supervise

Two layers: the **Manager** (W7a, below the supervisor section), a deterministic per-camera state machine, and the
**Supervisor** (W7b), the goroutines that drive it, run the relay and live-view MediaMTX, write `status.json`,
schedule the pruner and stop in order. Port of `cam.recorder.ps1 -Run` (R:3708-4197) with the relay and live ticks,
`Write-CamStatusFile` and the teardown.

## Supervisor (W7b)

```go
func NewSupervisor(SupervisorDeps) *Supervisor
func (*Supervisor) Run(ctx) (StopReport, error) // returns after the ordered stop; ctx cancel = interrupted
func (*Supervisor) Stop(kind string) StopReport // requested | interrupted | unexpected; waits for the stop
func (*Supervisor) Done() <-chan struct{}
type StopReport struct{ Kind, Reason string; Survivors []int; Final bool }
func ExitCode(err error) int                    // 0 clean, 20 ErrAnotherSupervisor (lock held), 21 *ConfigFault, 1 else (ErrUnexpectedStop, a lock I/O or permission fault)
type AnotherSupervisorError struct{ Owner string } // errors.Is(err, ErrAnotherSupervisor); the message names the owner
type ConfigFault struct{ Err error }

type SupervisorDeps struct {                    // injected; nil fields get the production default
    Source ConfigSource; Lock InstanceLock      // required
    Status *status.Writer; StatusPath string; ReadPrevious func() (*status.Document, error); Processes status.ProcessInspector
    Clock clock.Clock; Platform osx.Platform; Starter ffmpeg.Starter; Scope ProcessScope
    Scanner RootScanner; Pruner Pruner; Alerts Alerts
    Credentials relay.CredentialSource; APILogin, LiveLogin func() (relay.Login, error); NewRelayAPI func(config.Config, relay.Login) RelayAPI
    CheckFfmpeg func(path string) (ffmpeg.Version, error) // default ffmpeg.CheckVersion
    Logs LogSource; ChildLog func(component, path string) *slog.Logger; Redactor *logx.Redactor; Redactors []Registrar; Events *EventRing
    Pid int64; Location *time.Location
}
type ConfigSource interface{ Config() (config.Config, error); Cameras() ([]config.Camera, error) } // FileSource{ConfigPath, CamerasPath, Platform}
type RelayAPI interface{ Ready(ctx) (relay.ReadySet, error); WaitStarted(ctx, relay.ReadinessPolicy, <-chan struct{}) (relay.ReadySet, bool) } // *relay.API
type RootScanner interface{ Scanner; ScanRoot(root string, now time.Time) (ArchiveScan, error) } // ArchiveScanner
type Pruner interface{ Claim(now) (archive.ClaimResult, error); Run(now, *prune.State) (prune.Result, error) } // ArchivePruner
type InstanceLock interface{ Acquire() (release func(), owner string, err error) } // OSLock in production, MemoryLock in tests
type ProcessScope interface{ Open(ffmpeg.Starter) (ffmpeg.Starter, error); Close() error } // NoScope for tests
func NewProduction(SupervisorDeps, root string) *Supervisor  // W8: Lock = OSLock{<root>/_state/supervisor.lock}, Scope = &OSScope{StateDir}
type OSLock struct{ Path string }                            // osx.AcquireInstanceLock; the owner is named on refusal
type OSScope struct{ StateDir; Log; Inspector osx.Inspector; StopGrace } // Open: osx.Reap then osx.NewScope; Close: Stop + Close
type ScopedStarter struct{ Scope *osx.Scope }               // ffmpeg.Starter inside the scope (here because ffmpeg imports osx)
type EventRing  // logx.Sink: wire it into the process logger; NewEventRing(); Newest(n) []status.Event
func FileChildLogs(maxBytes int64, red *logx.Redactor) func(component, path string) *slog.Logger
```

### Goroutines and channels

```
 Run (caller)            supervise: starts everything, waits for ONE of: ctx.Done (interrupted), reqCh (Stop, stop.request:
   |                     requested), faultCh (a recovered panic anywhere: unexpected); then teardown
   |
   +-- heartbeat ------- status.Heartbeater on clockTicker: every StatusIntervalSeconds document() -> status.Writer
   |                     document() reads ONLY state published under s.mu (+ DiskFree, Events ring): never waits on work
   |
   +-- loop (owns the Manager) every 1 s: stop.request? -> reqCh
   |      |  reconcile every ReconcileSeconds (cameras.json; config change noticed), relay Ready ONCE -> LoginWatch
   |      |  (alert 130) -> RelayView, status tick: Refresh + Down (alert 110), archive scan every ArchiveScanSeconds,
   |      |  Plan -> Start / Kill, publish rows
   |      |<-- startCh <-- start goroutine per Start: sleep Delay (stagger), ffmpeg.Start via the scope's Starter
   |      |<-- exitCh  <-- waiter goroutine per child: Child.Wait (cleanup: input list, RemoveEmptySegment)
   |
   +-- relay mtxChild --- wait for the first camera list, render+write config, SourceEnv, start, WaitStarted
   |                      (blocks only itself), watch: exit -> backoff 3..60 s (reset after 300 s); stale login -> rewrite + restart
   +-- live mtxChild ---- only while the relay process is alive; same restart policy; Live section only when enabled
   +-- pruner ----------- Claim at start, Run at start and every PruneIntervalSeconds; alerts 120/121/122 -> Alerts

 teardown: stopping=true, close(stopCh), loop ends -> Manager.Shutdown -> kill cameras (hard), wait <= 10 s
           -> kill live, wait -> kill relay, wait (all within StopTimeoutSeconds) -> survivors = children not reaped
           -> heartbeat stops -> final document (stopped, StopKind, LastUnexpectedStop for unexpected) -> remove
           stop.request -> Run releases the lock and closes the scope
```

### Rules -> source -> tests (supervisor)

| Rule | Source | Test |
|---|---|---|
| Six cameras on the relay, 300 ms stagger, after the relay; held at a cold start while the relay starts | R:3909-4045 | `TestSupervisorStartsSixWithStagger` |
| Exit backoff 3, 6, 12, 24 s visible in the rows (count, LastError, NextStartAt) | R:3941-3961 | `TestSupervisorBackoffVisibleInRows` |
| Stall kill after SegmentSeconds x StallFactor, counted; a writing child never killed | R:3981-4010 | `TestSupervisorStalledChildKilled` |
| Hold 15 s, direct through the list file (no password in argv), move to the relay uncounted | R:3963-4036, SP1b | `TestSupervisorDirectThenRelay` |
| Alert 110 once per episode, recovery line, second episode | R:4074-4099 | `TestSupervisorDownAlertEpisode` |
| Heartbeat independent of a 20 s relay start (readiness wait or process start) | IS 1.1 | `TestSupervisorHeartbeatNotBlockedByRelayStart` |
| Pruner at start and every PruneIntervalSeconds; alerts from the result | R:4117-4121 | `TestSupervisorPrunerSchedule` |
| Refused root: no claim, nothing deleted, 120 once; clean root claimed and pruned; outside the sandbox refused | R:3786-3801, IR 5.2 | `TestSupervisorPruner*` |
| Alert 130 once per episode; unavailable does not end it; denied stays on the relay | R:2055-2085 | `TestSupervisorLoginAlertOncePerEpisode` |
| stop.request: requested, removed; a leftover one cleared at start | R:3761-3764, R:3838-3843 | `TestSupervisorStopRequestFile`, `TestSupervisorClearsLeftoverStopRequest` |
| Ordered stop cameras, live, relay; final document; nothing left; lock released | R:4133-4191, G6 | `TestSupervisorOrderedStop` |
| Survivor reported (pid, DID NOT STOP line), the rest still stopped | G7 | `TestSupervisorReportsSurvivor` |
| Panic anywhere: unexpected + LastUnexpectedStop{DetectedBy: supervisor}, ErrUnexpectedStop | R:4126-4154, G6 | `TestSupervisorPanicIsUnexpected` |
| Exit 20 (lock or live heartbeat, owner named), 21, lock released on every path | G8, IS 1.5 | `TestSupervisorLockPaths` |
| ffmpeg below 5.0 or not found: exit 21 with the ffmpeg message, no lock, no child | LLD 11 | `TestSupervisorFfmpegVersionCheck` |
| A lock fault that is not `ErrHeld` (I/O, permission) is exit 1 (restartable), `ErrHeld` stays 20 | W9a | `TestSupervisorLockErrorExitCodes` |
| D19: LastUnexpectedStop (At byte for byte) and LastWindowClose carried across runs | IS 1.6 F1 | `TestSupervisorCarryForwardAcrossRuns` |
| Key sets equal the golden PowerShell documents (live off = master's; live on; stopped; unexpected) | IS 1.2, `livecfg` | `TestSupervisorLiveOffKeySetEqualsMaster`, `TestSupervisorOrderedStop`, `TestSupervisorPanicIsUnexpected` |
| Events: own INFO+ records, newest first, StatusEventCount, log stamp, redacted; whole document redacted | IS 1.3, 1.1 | `TestEventRingParsesRecords`, `TestSupervisorEventsAndRedaction` |
| Every document built and the file on disk validate against `status.schema.json` | W4 | every scenario (`validateAll`) |
| Real ffmpeg + throwaway MediaMTX: record, drop, restart with backoff, recover, ordered stop | brief | `TestSupervisorRealIntegration` (`HOLOCRON_IT=1`) |

Negative controls (code mutations, each made and reverted): the relay start holding `s.mu` (heartbeat sharing the
start's wait) gives 1 beat in 19 s instead of >= 4; relay stopped before live; no carry-forward; survivors not
collected; no cold-start hold (10 starts, cameras went direct first) - each fails its test.

The tests run the real goroutines on a fake clock; `settle` steps the clock only when every goroutine of the test binary
is blocked (goroutine dump), so a woken goroutine acts at the virtual time it was woken.

### Decisions (supervisor)

- The relay config is rendered from `cameras.json` and written (temp + rename, only when it changed) at every relay
  start, like the live config in PowerShell; PowerShell rewrote the relay file only when its login block was stale.
  While running, a config whose login block no longer matches is rewritten and the relay restarted (not counted).
- The ready set is fetched once per 1 s tick (PowerShell: on a status tick or when a start is due) and shared.
- At a cold start the cameras are held (as if the relay were young) until the relay's first start attempt ends.
- `holocron.config.json` is re-read every ReconcileSeconds only to log a change once; the running values stay (PowerShell
  reads it once). A `cameras.json` that cannot be read keeps the current set (any error, including a wrong type).
- Exit codes follow the brief: an unexpected stop is 1 (`ErrUnexpectedStop`), 21 is a configuration fault (PowerShell
  used 21 for "the loop threw").
- The final rows keep LastPid and the counters (PowerShell keeps the entries); NextStartAt is null after the stop.
- The ffmpeg version check (`CheckFfmpeg`, default `ffmpeg.CheckVersion` on `FfmpegPath`) runs after the configuration
  is read and before the lock and any child: below 5.0 or not runnable is a `ConfigFault` (exit 21) with the ffmpeg
  package's message; a development build's warning is logged. It runs outside the process scope (a short `-version`). The OS lock and process scope are wired by `NewProduction` (W8).

# Manager (W7a: the child lifecycle manager)

A deterministic, clock-injected state machine that decides, call by call, when each recording camera's ffmpeg is
started, killed or left alone, and produces the `Cameras[]` rows and alert 110. It starts no process, runs no loop and
writes no file: the supervisor loop (W7b) drives it and performs every action. Port of the per-child part of
`cam.recorder.ps1 -Run` (R:3708-4197), `Invoke-CamReconcile`, `Set-CamChildBackoff`, `Test-CamChildStalled`,
`Measure-CamCameraDown`, `Get-CamDownAlertSeconds` (IR 3, IR 6, IS 1.4, LLD 2.4).

## API (signatures)

```go
type Scanner interface{ Scan(dir string, now time.Time) (archive.DirScan, error) }
type ArchiveScanner struct{ Matcher archive.Matcher; Container string; SegmentSeconds int } // production Scanner
type Alerts interface{ Raise(Event) }
type Event struct{ ID int; Severity, Title, Camera, Message, Key string }      // 110 only; Key "110:<SafeName>"
type Ready interface{ Has(path string) bool }                                  // relay.ReadySet satisfies it
type Deps struct{ Clock clock.Clock; Scanner Scanner; Alerts Alerts; Log *slog.Logger; Tick time.Duration; Location *time.Location }
type Settings struct{ ... }                                                     // durations and limits, see manager.go
func SettingsFrom(config.Config) Settings
func New(Settings, Deps) *Manager                                               // supervisor start = Clock.Now()

func (*Manager) Reconcile(cams []config.Camera)          // entries for Record==true, usable, non-colliding safe names
func (*Manager) Refresh()                                // one Scanner.Scan per recording camera (status tick)
func (*Manager) Plan(RelayView) []Action                 // call every Tick (1 s)
type RelayView struct{ Ready Ready; Alive bool; StartedAt clock.Mono } // ONE ready set per cycle; nil = nothing serving
type Action struct{ Kind ActionKind /* ActStart|ActKill */; Entry *Entry; Input Input /* relay|direct */; Delay time.Duration; Reason string }
func (*Manager) Started(e *Entry, pid int64)             // answer to a Start
func (*Manager) StartFailed(e *Entry, err error)         // answer to a Start that produced no process
func (*Manager) Exited(e *Entry, ffmpeg.ExitInfo) Cleanup // any exit, asked for or not; Cleanup{SafeName, Dir} -> RemoveEmptySegment
func (*Manager) Down() []DownEvent                       // alert 110 evaluation (status tick, after Refresh)
func (*Manager) Rows(archiveScans map[string]archive.DirScan) []status.Camera // the 20 IS 1.4 fields, cameras.json order
func (*Manager) Shutdown()                               // afterwards Plan hands out only Kill "stop"
func (*Manager) Active() int; Entry(safe string) *Entry
func DownAlertSeconds(Settings) float64; FormatAge(seconds float64) string
```

Contract for the caller: every Start is performed `Delay` after the Plan call and answered with `Started` or
`StartFailed`; every Kill is performed at once (hard kill) and answered with `Exited` when the process has gone. Until
then the entry is pending and Plan hands out nothing else for it. Kill reasons: `stalled`, `moving to the relay`, `stop`.
Nothing here is safe for concurrent use: the loop goroutine owns the Manager.

## Rules -> source -> tests

| Rule | IR / PowerShell | Test |
|---|---|---|
| Child only for `Record == true` and a usable safe name; collision refused with a ConfigError (V5) | IR 3.4, R:3631-3707 | `TestReconcileRecordOnly`, `TestReconcileCollisionRefused` |
| New entry due at once, zero counters; IP/Quality change recreates and loses counters (V12, kept) | IR 3.5, R:3680, R:3693-3696 | `TestReconcileChangeRecreates`, `TestReconcileDisableDrops` |
| Backoff 3, 6, 12, 24, 48, 60, 60; reset to 3 only after a run >= 300 s, at exit time | IR 3.5, R:2002-2029 | `TestBackoffSequenceDefaults`, `TestBackoffHealthyReset` (300 / 299 s) |
| Check `backoff` scaled 1/8/15: 1,2,4,8,8; healthy 18 s -> 1; quick -> 2; RestartsLastHour 7; log shape | T:4759-4839 | `TestBackoffCheckScaled` |
| Every exit counts, code 0 included; `ffmpeg exited with code N after Ns` ([int] rounding) | IR 3.5, R:3941-3961 | `TestExitAlwaysCounts` |
| Failed start backs off (ranFor 0), not counted; `failed to start: msg` | IR 3.4, R:4047-4057 | `TestStartFailedNotCounted` |
| RestartsLastHour from a capped ring pruned on write | IR 3.10, LLD 2.4 | `TestRestartsRing` |
| Stall: `now - max(StartedAt, newest closed write, open first-seen) > SegmentSeconds x StallFactor` (2250 s), strict | IR 3.6, R:1871-1914, R:3981-4010 | `TestStallFromStartedAt` (2249/2250 quiet, 2251 kill), `TestStallMarkers`, `TestStallYoungChild` |
| Check `stallwatch` 6 x 2.5 = 15 s: healthy never killed over 21 rolls, suspended killed in (limit, limit+3.5 s], counted | T:5364-5475 | `TestStallWatchCheck` |
| StartedAt age monotonic; wall step and blind gap (no Plan for > 3 x Tick) extend deadlines | LLD 2.1, IR 11 risk 7, brief | `TestStallWallStep`, `TestStallBlindGap` |
| One shared ready set; path not in it / empty / no relay path / relay off -> direct; denied counts as ready | IR 3.3, T:173-207 | `TestPlanInputChoice` |
| Hold while the relay is younger than 15 s and the path is not ready (strict); then direct, logged once | IR 3.3, R:4022-4036 | `TestPlanRelayHold` (held at 14 s, started at 15 s, by 16 s), `TestPlanNoHold` |
| Stagger 300 ms, non-blocking, across Plan calls, earliest due first then name; 6 starts within 1500 ms | R:4041-4045, T:209-218 | `TestPlanStaggerSix`, `TestPlanStaggerAcrossCallsAndOrder` |
| Never a Start before NextStartAt; each action handed out once | R:3900-3902 | `TestPlanWaitsForBackoff`, `TestPlanStartOnce` |
| Move off direct: kill, restart on the relay at once, no count, backoff untouched | IR 3.3, R:3970-3979 | `TestPlanMoveOffDirect` |
| Reconcile stops and shutdown: Kill `stop`, never counted; never two children for one folder | R:3637-3639, IR 3.7 | `TestPlanRetire`, `TestShutdown` |
| Empty-segment cleanup requested after every exit and kill | IR 3.8 | `TestExitAlwaysCounts`, `TestPlanRetire` (Cleanup) |
| Alert 110 limit `max(min x 60, seg + status + 30)`; quiet at the limit, alert one second later | IR 3.9, R:1818-1868, T:5480-5556 | `TestDownAlertRows`, `TestDownAlertLimitTable` (900/1800/60) |
| One alert per episode, repeat every 12 h, "is writing again after N down" with no alert | IR 3.9, R:4074-4099 | `TestDownAlertRepeatAndRecovery` |
| D19: floor also the camera's first start (its enable while never started) | LLD 2.4, IR 3.9 latent V6 | `TestDownAlertLateEnableD19` (PowerShell reference alerts, Go does not) |
| Check `downalertrun` 20 s segments, limit 51 s: healthy quiet, dark exactly one alert in [51, 59] s | T:5560-5654 | `TestDownAlertRunCheck` |
| `Format-CamAgeText` | cam.common.ps1:75-84 | `TestFormatAge` |
| The 20 `Cameras[]` fields; shape equals the golden status documents | IS 1.4, R:2804-2875 | `TestRowsFields`, `TestRowsGoldenShape` |
| Determinism: no dependence on insertion order | LLD 2.4 | `TestPlanDeterministic` |
| Invariants over a seeded random run (seed 20261020, 5000 steps) | brief | `TestRandomInvariants` |

Each rule test also carries a negative control (a mutated constant must change the outcome); code mutations of every
rule (strict stall, D19 floor, move-off count, one child per folder, failed-start count, exit 0, open-segment recency,
hold, stagger, repeat, blind gap, 300 s reset, [int] rounding, denied) each fail at least one test.

## Decisions and deliberate differences from PowerShell

- **D19 floor**: `max(supervisor start, first start by this manager (or the enable time while it never started), newest
  closed write, open-segment first-seen)`. PowerShell uses the supervisor start, so a camera enabled late alerts at once
  and then logs "writing again".
- **Monotonic intervals**: StartedAt age, backoff deadlines, relay hold and the 110 repeat use `clock.Mono`; file times
  are wall times. A gap of more than 3 x Tick between Plan calls on either clock is blind and not counted by the stall
  watchdog or alert 110 (PowerShell has no such rule).
- **Stagger** is a schedule (`Action.Delay`), not a 300 ms sleep that blocks the loop.
- **Open segment**: its first-seen time (`archive.Tracker`) counts only while the scan says `Recent`; PowerShell
  counts NTFS creation time of the newest-created file. Without a scan the stall marker is StartedAt (PowerShell
  enumerates the folder).
- **Restarts** are a ring (last hour, at most 4096); PowerShell keeps every stamp for the process life.
- **Reconcile stops** are handed out as Kill and the replacing entry of the same folder waits for the old child to exit.
- `LastError` rounds the run time like PowerShell's `[int]` (half to even), as `ffmpeg.ExitInfo.Message` now does.
