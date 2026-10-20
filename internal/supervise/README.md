# internal/supervise (W7a: the child lifecycle manager)

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
- `LastError` rounds the run time like PowerShell's `[int]` (half to even); `ffmpeg.ExitInfo.Message` truncates.
