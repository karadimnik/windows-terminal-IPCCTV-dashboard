# Continuous integration

`.github/workflows/ci.yml` runs on every push to the `portable-nvr` branch and on a manual run (Actions tab, "Run workflow"). It lives on that branch only. It has no secrets, a read-only token (`permissions: contents: read`), no pull-request or schedule trigger, and cancels an older run of the same ref when a newer push arrives. Third-party actions are pinned to a commit SHA, with the version in a comment.

## What runs

| Job | Where | What it proves |
| --- | --- | --- |
| `lint-vet` | ubuntu | `gofmt -l cmd internal` prints nothing; `go vet ./...` is clean for `GOOS=linux`, `windows` and `darwin`. |
| `test (ubuntu-latest)` and `test (windows-latest)` | hosted runners | ffmpeg installed, then `go test -race -count=1 -p 1 -v ./...`, then the opt-in real-process and real-ffmpeg tests (below). A failure here fails the run. |
| `test (macos-latest)` | hosted runner | The same steps with `continue-on-error`: macOS is paper-only today, so it reports and never fails the run. |
| `build (target)` | ubuntu | `holocron` and `camui` compile for `windows/amd64`, `linux/amd64`, `linux/arm64`, `darwin/arm64`. Nothing is uploaded. |

Go comes from `go.mod` (`go-version-file`), so the runner builds with the same minor release the module asks for. `GOTOOLCHAIN=local` keeps `go` from fetching a different one. ffmpeg comes from `apt` (Ubuntu), a pinned Chocolatey package (Windows) and Homebrew (macOS); the product needs 5.0 or later and the step prints the version that was installed.

### The opt-in tests

Some tests only run when asked (`HOLOCRON_OSX_REAL`, `HOLOCRON_HOST_REAL`, `HOLOCRON_IT`, `HOLOCRON_IT_FFMPEG`; see their skip messages). The second test step sets them and runs `./internal/ffmpeg ./internal/osx ./internal/host` with `-race` (the `Cmd.Wait` race in the `internal/osx` test helpers was fixed in W16). It is blocking on Linux, where it was run clean, and report-only on Windows and macOS until a run says they are green; flip `continue-on-error` in the workflow when they are. Tests that need MediaMTX (`HOLOCRON_IT_MEDIAMTX`) are not run in CI.

## How to read a run

- A red `lint-vet`: the log names the file (`gofmt`) or the package (`vet`) and the OS it failed for.
- A red `test`: the full `-v` output is in the log; the failing test is the `--- FAIL` line.
- **Skips are visible.** Each test job ends with a "Test and skip counts" step. Its summary (on the run's summary page) lists pass, fail and skip counts and every skipped test by name, per OS. A skip-everything run shows up as a large skip list instead of passing quietly. Two further steps fail the job if the ffmpeg-gated tests (`TestRealMediaScenarios`, `TestFFprobeFailures`, `TestRealFfmpeg`, `TestFullChainHostSupervisorFfmpeg`) did not pass, so a missing ffmpeg cannot go unnoticed.
- Expected skips: tests for another OS, MediaMTX tests, the end-to-end run (needs MediaMTX), and `TestPowerShellProducerOutputConforms` (CI sets `HOLOCRON_SKIP_POWERSHELL=1`, see below).

## Run the same thing locally

From WSL or Linux, in the repository root, with Go from `go.mod` ([go-toolchain.md](go-toolchain.md)):

```bash
test -z "$(gofmt -l cmd internal)"
for os in linux windows darwin; do GOOS=$os go vet ./...; done
go test -race -count=1 -p 1 ./...
for t in windows/amd64 linux/amd64 linux/arm64 darwin/arm64; do CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go build ./cmd/holocron ./cmd/camui; done
```

`build-holocron.sh` runs the first three and builds `holocron.exe`. To see what a clean runner sees, run the tests under `env -i` with a throwaway `HOME` and `TMPDIR` and a `PATH` of Go plus `/usr/bin:/bin` (no `/mnt/c`, so no `powershell.exe`), and once with `TZ` set to a zone that is just past midnight; the suite does not depend on the machine's zone, clock, home folder or tools. The opt-in step is: set `HOLOCRON_OSX_REAL=1 HOLOCRON_HOST_REAL=1 HOLOCRON_IT=1 HOLOCRON_IT_FFMPEG=/path/to/ffmpeg HOLOCRON_IT_FFPROBE=/path/to/ffprobe` and run `go test -count=1 -p 1 ./internal/ffmpeg ./internal/osx ./internal/host`.

## What is deliberately not in CI

- **The PowerShell harness** (`tests/cam.tests.ps1` and `tests/holocron/*`). It needs a Windows PowerShell setup and writes generated files into the checkout. `HOLOCRON_SKIP_POWERSHELL=1` makes the one Go test that calls into it (`TestPowerShellProducerOutputConforms`, which runs `tests/holocron/status-conformance.ps1`) skip loudly. Run the harness by hand ([development.md](development.md)).
- **Real cameras, a real relay, the shadow run** ([shadow.md](shadow.md)) and anything that records to a real archive: no camera, address or machine name is in the repository, and CI has none to give.
- **Windows service and scheduled-task install**, autostart and the Event Log or toast alerts: they change the machine and need elevation. Their logic is covered by the fakes in the Go tests.
- **MediaMTX** and the full end-to-end run (`TestEndToEnd`, `TestSupervisorRealIntegration`, `TestThrowawayMediaMTX`).
- **Release artifacts.** Builds are compiled to prove they compile and are not uploaded.
