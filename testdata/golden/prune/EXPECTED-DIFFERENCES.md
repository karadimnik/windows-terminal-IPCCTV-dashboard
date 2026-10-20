# Expected differences between the Go pruner and Invoke-CamPrune

Recorded up front for the differential comparison (`internal/prune/golden_test.go`). A scenario
names its difference in the golden's `Expect` field; `expectedRules` in the test says what the
difference allows. Every other difference fails the test.

Safety direction legend: **subset** = Go deletes only files PowerShell also deletes;
**substitution** = Go may delete a different strict segment in place of one PowerShell deleted;
**more** = Go can delete a file PowerShell keeps.

| Tag | Difference | Why | Direction | Scenario |
|---|---|---|---|---|
| `strict-allowlist` | Go accepts only ASCII digits, the name must end exactly at the extension (no trailing newline) and the digits must be a real date, time and offset within +/-14:00 (W2 advisor decision, `archive.IsSegmentOf`). PowerShell's `-match` takes Unicode digits, `$` before a final newline and impossible dates. | ffmpeg never writes such names; a stricter allowlist cannot make a foreign file deletable. | age rule and gates: **subset** (`strict-allowlist-age`: PowerShell deletes 6, Go the 2 real segments). | `strict-allowlist-age` |
| `strict-allowlist-substitution` | The same rule under the free-space watermark: PowerShell frees space by deleting the near-miss file, Go skips it and deletes the next-oldest real segment. The circuit breaker's shares are taken over fewer files, so it can hold or release differently. | Same cause. | **substitution**: never a near-miss name, never a file PowerShell protects or refuses; with unequal file sizes Go may need more files to reach the target. Not delete-less in set terms: open question for the Operator. | `strict-allowlist-watermark` |
| `case-fold` | Linux and macOS match the camera prefix and extension case-sensitively (`archive.MatcherFor`); Windows folds ASCII case like PowerShell. | ffmpeg writes the pattern exactly; a differently cased file on a case-sensitive filesystem is not a recorder file. | Windows: identical (checked). Linux/macOS age rule: **subset**; under the watermark the same **substitution** as above. | `case-fold` (the case-sensitive pass) |
| `copied-marker` | `archive.MarkerValid` also compares the `RootPath` inside the marker with the root; PowerShell checks `Owner` only, so a marker copied into another folder passes there. | The marker's own note says it must not be copied. | **subset** (Go refuses the run). | `copied-marker` |
| `open-segment` | PowerShell protects each folder's newest by NTFS creation time. Go protects every segment at the latest name instant, the latest created where the OS reports creation time (Windows), the newest by write time (archive.Scan's Newest) and bounds "strictly older" by the earliest write among the name/creation newest (SP4). For files ffmpeg wrote the name instant is the creation time, so this is the same file. | Creation time does not exist portably; name order is wrong across a DST fall-back (SP4, the `dst-fallback` scenario matches exactly). | Windows: **subset** (creation read and protected too). Linux/macOS: after a copy or restore (creation times are the copy's) PowerShell may hold or refuse where Go deletes by age: **more** (the scenario: PowerShell refuses with "is later than the newest segment", Go deletes 3). Open question. | `copied-archive-creation-times` |
| `tie-order` | Equal write times: Go orders by file name, then folder (IR U5). Windows PowerShell's `Sort-Object` is not stable: measured, 2 and 3 equal keys come out reversed, 4 as 3,4,1,2. | Deterministic, documented order. | Same number of deletions at the same write times; which of the tied files differs (none is protected). | `tie-order` |

Differences the comparison absorbs by construction (no tag; listed so none is silent):

- **Dry-run watermark estimate.** A PowerShell dry run adds `Length / 1GB` to free space without
  rounding; the harness's files are 65536 times smaller than the sizes they stand for, so its dry-run
  watermark deletes far more than it would on real files. Dry runs are compared on refusals, the age
  rule (exactly), the age part of the stop line and alerts/state; dry watermark deletions must be a
  **subset**. Go's dry run returns the plan, which predicts with the rounded figure a real run reads.
- **GB figures computed from file lengths** in two messages (the summary's "X GB freed" and the
  breach text's "x of y GB") use the scaled-down lengths in PowerShell; they are masked. Local log
  stamps are masked.
- **Refusal wording.** Root and marker refusals come from `archive.CheckRoot`/`VerifyRoot` and are
  compared by class (root problem, claimable, not claimable). A non-number `MaxAgePruneFraction` is
  refused by `internal/config` as "must be a number" without the range PowerShell names (`pruneconfig`).
- **Refused links are listed by name more often.** A directory junction or link inside a camera folder
  is not listed by PowerShell's `Get-ChildItem -File`; Go lists it as a refused reparse point. A file
  link at the top level is skipped and listed. The test requires Go's refused list to contain
  PowerShell's. Nothing is deleted either way.
- **Protected list.** Go's contains PowerShell's (it protects more); the test requires the inclusion.
- **Executor.** Go deletes only files of the plan, in order, re-checking each one; when the plan runs
  out before the target (the disk freed less than predicted, or a file changed and was skipped) it
  stops (`Short`, not `Blocked`) where PowerShell walks on down the candidate list. A read-only file
  is not deleted (`DeleteFile` fails, counted as failed) where `Remove-Item -Force` would delete it.
  An unreadable free-space figure skips the watermark (PowerShell throws and the run is lost). All
  **subset**.
- **The drive-root case of `pruneroots`** is a Go table test only (`TestPruneRoots`): the harness
  guard (`SANDBOX_ONLY`) refuses any root outside its own sandbox, so it never points the PowerShell
  pruner at a real drive root.
- **`prunelive`** reads the real archive and is not part of the harness. `pruneowner` and `pruneloop`
  are replayed as consecutive runs over a static tree (no supervisor, no synthetic camera); their
  supervisor-side assertions belong to the supervisor's tests.
