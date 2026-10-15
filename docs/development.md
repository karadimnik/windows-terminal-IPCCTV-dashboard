# Development notes

Mechanics that matter when you are working on this repository rather than using it.

## Passing a list of checks to the test harness

`powershell.exe -File .\tests\cam.tests.ps1 -Checks parse,strip` binds the whole list as a single string and fails validation. Use `powershell.exe -Command "& '.\tests\cam.tests.ps1' -Checks parse,strip"`.

That is a PowerShell parameter-binding quirk rather than anything about this project: `-File` hands each argument to the script as a literal string with no parser behind it, so a comma-separated list never becomes an array. Everything the harness runs and which checks can skip is in the [command reference](reference.md#the-test-harness-testscamtestsps1).

## Time arithmetic

Measure every interval, age and deadline on `Get-CamWallClock`, which returns UTC, and compare it only with `LastWriteTimeUtc` / `CreationTimeUtc` or with a timestamp parsed by `Get-CamDateTimeOrNull`; local time is for display. The `clocklint` check fails on `[DateTime]::Now`, `Get-Date`, `.LastWriteTime` or `.CreationTime` anywhere but a form that can only produce text (`.ToString(...)`, or `Get-Date` with its format parameter); a line that is display only for another reason says so with a `# clock: display` comment. `dst` and `dstloop` prove the arithmetic across this zone's next two transitions without touching the machine clock: unit checks replace `Get-CamWallClock`, and `Set-CamTestSandboxClock` rewrites the clock readings of a sandbox copy for the real supervisor.

## Dashboard diagnostic switches

`mpv.cam.autolayout.autorestart.ps1` takes two switches that answer a question rather than run the wall, which is why they are here and not in the [command reference](reference.md).

| Switch | What it does |
| --- | --- |
| `-PrintUrls` | Prints the input each camera would use right now, redacted, and the computed layout, then exits without opening a window. The quickest way to see whether the relay is being used. |
| `-NoHealthStrip` | Reserves the strip but launches no health window in it. |
