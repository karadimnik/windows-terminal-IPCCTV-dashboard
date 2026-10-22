# Development notes

Mechanics that matter when you are working on this repository rather than using it.

## Passing a list of checks to the test harness

`powershell.exe -File .\tests\cam.tests.ps1 -Checks parse,strip` binds the whole list as a single string and fails validation. Use `powershell.exe -Command "& '.\tests\cam.tests.ps1' -Checks parse,strip"`.

That is a PowerShell parameter-binding quirk rather than anything about this project: `-File` hands each argument to the script as a literal string with no parser behind it, so a comma-separated list never becomes an array. Everything the harness runs and which checks can skip is in the [command reference](reference.md#the-test-harness-testscamtestsps1).

## Where the web app's code lives

The web app (`camui`) is the package `internal/web` of the repository's one Go module; the nested `ui/` module is gone. `cmd/camui` is a four-line `main` that calls `web.Main`, and `holocron web` calls the same package. Tests: `go test ./internal/web/...` (about 20 s; `-race` doubles it). `ui/build.sh [output]` keeps its old path and interface (gofmt, vet and test for `./internal/web/...` and `./cmd/camui`, then the `camui.exe` build), because `cam.ps1`, the harness and the docs call it. Two thin shims stay at old paths: `ui/scripts/camui-apply.ps1` (the settings writer moved to `internal/web/scripts/`, where `go:embed` needs it; the harness's `clocklint` still scans `ui\scripts`) and `ui/static/live.js` (a symlink to `internal/web/static/live.js` for `tests/live/bestgrid.test.js`).

## Time arithmetic

Measure every interval, age and deadline on `Get-CamWallClock`, which returns UTC, and compare it only with `LastWriteTimeUtc` / `CreationTimeUtc` or with a timestamp parsed by `Get-CamDateTimeOrNull`; local time is for display. The `clocklint` check fails on `[DateTime]::Now`, `Get-Date`, `.LastWriteTime` or `.CreationTime` anywhere but a form that can only produce text (`.ToString(...)`, or `Get-Date` with its format parameter); a line that is display only for another reason says so with a `# clock: display` comment. `dst` and `dstloop` prove the arithmetic across this zone's next two transitions without touching the machine clock: unit checks replace `Get-CamWallClock`, and `Set-CamTestSandboxClock` rewrites the clock readings of a sandbox copy for the real supervisor.

## Dashboard diagnostic switches

`mpv.cam.autolayout.autorestart.ps1` takes two switches that answer a question rather than run the wall, which is why they are here and not in the [command reference](reference.md).

| Switch | What it does |
| --- | --- |
| `-PrintUrls` | Prints the input each camera would use right now, redacted, and the computed layout, then exits without opening a window. The quickest way to see whether the relay is being used. |
| `-NoHealthStrip` | Reserves the strip but launches no health window in it. |

## Continuous integration

The Holocron track has a GitHub Actions workflow on the `portable-nvr` branch that runs gofmt, vet for three operating systems, the race suite on Linux and Windows (macOS best effort) and the cross-builds. What it covers, how to read it, how to run the same checks locally and what it leaves out on purpose: [ci.md](ci.md).

## Moving to Holocron, and back

The Go program `holocron` runs beside the PowerShell system first ([shadow.md](shadow.md)); its autostart is [autostart.md](autostart.md). Moving the real NVR over, step by step with the stop conditions, is [cutover.md](cutover.md), and the way back, which leaves the old system untouched, is [rollback.md](rollback.md).

## Documentation of the Holocron system, and its checks

The guides are [holocron-quickstart.md](holocron-quickstart.md) and [holocron-cheatsheet.md](holocron-cheatsheet.md); the complete reference for AI agents is [ai/holocron-for-agents.md](ai/holocron-for-agents.md), indexed by `llms.txt` and with the repository rules in `AGENTS.md`, both at the repository root. Two checks keep them true. `cmd/holocron/docs_test.go` (part of `go test ./cmd/holocron`) fails when a `holocron ...` command in those documents, the README or AGENTS.md is not in the command registry, or when the command table of the agent reference differs from the registry (`HOLOCRON_DOCS_UPDATE=1 go test ./cmd/holocron -run TestAgentDocCommandTable` rewrites the table). The links and PowerShell switches of `README.md` and `docs/*.md` are checked by the harness: `powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "& '.\tests\cam.tests.ps1' -Checks doclinks,docswitches"`.
