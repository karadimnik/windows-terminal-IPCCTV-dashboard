# Pruner goldens (W3)

Decisions of the PowerShell pruner `Invoke-CamPrune`, recorded by `tests/holocron/prune-diff.ps1`
over throwaway archives under `%TEMP%\holocron-w3-<guid>` and compared with the Go pruner by
`internal/prune/golden_test.go` (Linux CI needs no PowerShell).

| Path | What |
|---|---|
| `table/<name>.json` | one scenario each: every `prune*` check of `tests/cam.tests.ps1` (`prunematrix`, `pruneroots-*`, `pruneage`, `prunewatermark`, `pruneclock-*`, `prunebreaker-*`, `pruneblocked`, `pruneowner-*`, `pruneloop-*`), the corners (`rounding-tie`, `reserve-target-corner`, `dst-fallback`) and the expected differences (`strict-allowlist-*`, `case-fold`, `copied-marker`, `bad-owner-marker`, `copied-archive-creation-times`, `tie-order`) |
| `table/pruneconfig.json` | `Get-CamConfig` over `MaxAgePruneFraction` values: value or error |
| `corpus.jsonl` | 200 generated archives (seed 20261020), one JSON document per line |
| `EXPECTED-DIFFERENCES.md` | every difference the comparison allows, with its safety direction |

A document holds the tree (`Entries`: sandbox-relative path, kind, logical size, UTC write and
creation times, `Late` = placed after the ownership claim), the marker mode, the steps (`prune`
with the resolved rule keys, the free space at the start of the step, a clock shift, dry or real,
fresh or carried state; `claim`, `add`, `remove`) and one result per step: ordered deletions with
reason, the files that actually disappeared, refusals, breaker, stop line, message, protected,
refused and foreign lists, free figures, alerts raised (by a `Send-CamAlert` stub) and the state
flags after the step, and how often each key line was logged.

Files on disk are 65536 times smaller than the sizes in a golden; the free-space fake counts one GB
per 16384 physical bytes removed and rounds like `Get-CamFreeSpaceGB`. The wall clock is fixed at
2026-10-20T06:00:00Z (scenario `Now`). Paths in texts are sandbox-relative with `/`.

Regenerate or verify (Windows PowerShell 5.1, zone GTB Standard Time, from a local folder):

    cd C:\Windows\Temp
    powershell.exe -NoProfile -ExecutionPolicy Bypass -File <repo>\tests\holocron\prune-diff.ps1 -Regenerate
    powershell.exe -NoProfile -ExecutionPolicy Bypass -File <repo>\tests\holocron\prune-diff.ps1 -Verify

The harness sets `SANDBOX_ONLY=1` and refuses, before touching it, any path outside its own sandbox
folder (it proves the refusal for `E:\cameras`, `E:\`, `C:\`, the repository, the profile and the
temp folder itself at start). It reads nothing real.
