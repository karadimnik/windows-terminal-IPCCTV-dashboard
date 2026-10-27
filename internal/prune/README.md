# prune

The archive pruner (W3): a pure planner and a guarded executor. Port of `Invoke-CamPrune`
(cam.recorder.ps1) with its gates, numbers and episode rules; the PowerShell code is the
specification and `testdata/golden/prune/` holds its recorded decisions.

    // inputs
    type File struct { Name, Day string /* the day folder holding it, "" in the spool */; MTime time.Time; Size int64; Reparse bool; Created time.Time /* Windows only, else zero */ }
    type Camera struct { Dir string; Reparse bool; Files []File; Days []string /* the day folders listed */; Newest string /* archive.Layout's Newest: always protected */ }
    type Rules struct { ReserveGB, PruneHysteresisGB float64; MaxRetentionDays *float64; MaxAgePruneFraction float64
                        Container string /* ".mkv" */; SegmentSeconds, StallFactor float64 }
    func RulesFrom(c config.Config) Rules
    type Inputs struct { Root string; Matcher archive.Matcher; Cameras []Camera; Skipped []string; FreeBytes uint64
                         Rules Rules; Now time.Time; RootProblem /* archive.CheckRoot */, MarkerProblem /* archive.VerifyRoot */ string }
    func Collect(root string, p osx.Platform, r Rules, now time.Time, dryRun bool) (Inputs, error)   // read-only
                                                 // archive.Layout: the spool and every day folder (D20), every file listed; Plan classifies

    // the planner (pure)
    func Plan(in Inputs) Decision
    type Decision struct { Root string; Refusal *Refusal /* Gate: "root" | "marker" | "clock" */; Deletions []Deletion
                           Blocked, Short, AgeRule, Watermark, BreakerHeld bool; Breaker string
                           Protected, Rejected []string; Foreign, Kept []Count; Messages []string; Candidates, Folders, Failed int
                           FreeGBBefore, FreeGBAfter, ReserveGB, TargetGB float64; FreedBytes int64; StopLine, Message string }
    type Deletion struct { Path, Camera, Name string; Bytes int64; MTime time.Time; Reason /* "age" | "watermark" */, Detail string
                           Guard string; GuardMTime time.Time /* the protected newest it must stay strictly older than */ }

    // the executor
    type FS interface { Remove(path string) error; RemoveDir(path string) error; Stat(path string) (Info, error) /* lstat */; FreeBytes(root string) (uint64, error) }
    type OSFS struct{ Platform osx.Platform }          // Remove = unlink / DeleteFile: never a directory, never recursive
                                                       // RemoveDir = rmdir / RemoveDirectory: one EMPTY directory, ErrDirNotEmpty otherwise
    type Executor struct { FS FS; Log *slog.Logger; Actor string; DryRun bool; Sandbox string }
    func (e *Executor) Run(in Inputs, st *State) (Result, error)
    type Result struct { Decision /* what was done */; Plan Decision; DryRun bool; Skipped []Skip; Alerts []Alert; Episode Episode; DaysRemoved []string }

    // episodes and alerts (returned, not sent)
    type State struct { RefusalAlerted string; ClockRefused, BlockedAlerted, AgeBreakerTripped bool; DeletedTotal int; FreedBytes int64; Blocked bool; LastMessage string }
    func (s *State) Settle(d Decision, dryRun bool) ([]Alert, Episode)
    type Alert struct { ID int /* 120 | 121 | 122 */; Level, Title, Body string }

    // the sandbox guard
    const SandboxEnv = "SANDBOX_ONLY"                  // "1": os.TempDir(); otherwise the prefix itself
    func CheckSandbox(path, prefix string) error       // ErrOutsideSandbox unless strictly inside (also after resolving links)

## Gates, in order (the contract)

1. Root (`RootProblem`), 2. ownership marker (`MarkerProblem`): refuse the whole run, `Blocked`,
   alert 120 once per distinct message (re-armed by a passing run).
3. Per file: links and reparse points refused by name; underscore folders and files directly in the
   root never looked at; a file is a segment only where `archive.Layout.Classify` says it is footage of
   its folder: in the camera folder (an immediate child of the root) or in a day folder of the date in
   its name directly inside it (D20), and `archive.Matcher.IsSegmentOf(name, folder, container)`.
   Anything else, a misfiled segment or a foreign file in a day folder included, is foreign (counted
   per folder, never deleted). Filed segments take part in everything after this gate exactly as
   spooled ones: protection, the clock, the breaker's shares and the oldest-first order.
4. Protection, per folder: every segment at the latest name instant, every segment at the latest
   creation time where the OS reports one (Windows), the newest by write time (the file ffmpeg holds
   open) and `Camera.Newest`. A candidate must be strictly older (write time) than the earliest write
   among the name/creation newest. A folder whose `Newest` is missing from the listing is skipped.
5. Clock sanity (only when something is unprotected and a rule could delete): the oldest unprotected
   later than the newest protected; any segment written more than 60 s ahead of `Now`; `Now` more than
   `SegmentSeconds x StallFactor` (2250 s) ahead of the newest protected. Refuses the run, no alert.
6. Age rule (`MaxRetentionDays > 0`): cutoff `Now - days` (milliseconds rounded as .NET `AddDays`);
   the circuit breaker holds the whole age run when the expired share of all accepted segments is above
   `MaxAgePruneFraction` by count OR by bytes (alert 122 once per episode).
7. Watermark: when the free figure `round(bytes / 2^30, 2)` (half to even, as `[math]::Round`) is below
   `ReserveGB`, delete globally oldest first (equal write times: name, then folder) until it is at least
   `ReserveGB + PruneHysteresisGB`; candidates running out first is `Blocked` (also between reserve and
   target), alert 121 once per episode, re-armed by a run that was not blocked.

`Plan` predicts free space after each deletion as the reading plus the bytes freed. The executor
re-reads it after every removal, deletes only planned files (in order), re-checks each file just
before removing it (exists, plain file, same size and write time, its camera folder and day folder
still plain folders and not links or junctions, still passes the allowlist, still strictly older than
its guard) and skips it otherwise; when the plan runs out before the target the
run is `Short` (not `Blocked`) and the next run continues. Every deletion is logged at INFO with
`action=prune.delete`, `target`, `bytes`, `reason`, `result`; each run ends with its stop line.

Empty day folders (D20). After a real run that no gate refused, the executor removes each listed day
folder dated before today's local date that the run emptied (every file listed in it was deleted) or
that was already empty when Collect listed it: re-checked first (inside the sandbox, a plain folder
named like a real date, directly in a plain camera folder directly under the root), then `RemoveDir`,
which refuses anything that is not empty. Today's and later days stay (the mover may be filing into
them), as do the camera folder and every other folder. A folder it keeps or cannot remove is logged
(`action=prune.rmdir`: not empty at DEBUG, a failed re-check or removal at WARN) and tried again by
the next run; removed ones are logged at INFO and listed in `Result.DaysRemoved`.

## Tests

- `checks_test.go`: the ten `prune*` checks of `tests/cam.tests.ps1` with their fixtures and assertions.
- `golden_test.go`: the differential comparison with the PowerShell decisions in `testdata/golden/prune`
  (31 table scenarios, 200 generated trees), each run with and without case folding; the differences
  allowed are only those in `testdata/golden/prune/EXPECTED-DIFFERENCES.md`; negative controls.
- `property_test.go`: 4000 seeded trees (seed 20261020) against rules re-derived in the test, plus the
  executor over an in-memory filesystem that changes files under it; negative controls.
- `mixed_test.go` (L3): every golden scenario and 60 generated archives (the fall-back day, midnight, the
  retention cutoff inside a day folder) with most closed segments filed into day folders decide exactly
  as the flat original; the awkward trees of `archivetest.Mixed` delete only footage; each hazard
  (misfiled, foreign, nested, non-day folder, link) beside a positive control; a day folder swapped for
  a junction after the plan; the newest survives any pressure.
- `units_test.go`: rounding tie, .NET number formatting, tie order, executor re-checks, directories
  never removed, dry run, sandbox guard.
- Every test runs with `SANDBOX_ONLY=1` (TestMain): the executor refuses anything outside the OS temp dir.
