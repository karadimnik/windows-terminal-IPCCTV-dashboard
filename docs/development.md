# Development notes

Mechanics that matter when you are working on this repository rather than using it.

## Passing a list of checks to the test harness

`powershell.exe -File .\tests\cam.tests.ps1 -Checks parse,strip` binds the whole list as a single string and fails validation. Use `powershell.exe -Command "& '.\tests\cam.tests.ps1' -Checks parse,strip"`.

That is a PowerShell parameter-binding quirk rather than anything about this project: `-File` hands each argument to the script as a literal string with no parser behind it, so a comma-separated list never becomes an array. Everything the harness runs and which checks can skip is in the [README](../README.md#the-test-harness-testscamtestsps1).

## Dashboard diagnostic switches

`mpv.cam.autolayout.autorestart.ps1` takes two switches that answer a question rather than run the wall, which is why they are here and not in the README's reference.

| Switch | What it does |
| --- | --- |
| `-PrintUrls` | Prints the input each camera would use right now, redacted, and the computed layout, then exits without opening a window. The quickest way to see whether the relay is being used. |
| `-NoHealthStrip` | Reserves the strip but launches no health window in it. |
