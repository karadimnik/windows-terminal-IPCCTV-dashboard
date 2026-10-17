# status.json: known differences of the PowerShell producer

The schema (`status.schema.json`) is the contract. The PowerShell writer
(`Write-CamStatusFile` in `cam.recorder.ps1`, patches `Set-CamStatusUnexpectedStop`
and `Set-CamStatusWindowClosed` in `cam.common.ps1`) is today's behaviour. Where
the two differ the item is listed here and pinned by an expected-failure test in
`conformance_test.go`, which passes while the PowerShell behaves as described and
fails the day it changes (the signal to update this file).

## F1 - `LastWindowClose` is dropped after a supervisor restart

- Intent: todo [78] and `ui/notice.go` say `LastUnexpectedStop` AND `LastWindowClose` survive a restart.
- Code: only the warden patch writes `LastWindowClose` (`cam.common.ps1`, `Add-Member ... 'LastWindowClose'`). The supervisor carries `$previous.LastUnexpectedStop` only (`cam.recorder.ps1` around line 3757) and `Write-CamStatusFile` has no parameter for it and no such key in `$document`. `cam.recorder.ps1` does not contain the word `LastWindowClose` at all.
- Confirmation: CONFIRMED by reading the code, and by running the product's writer in a sandbox with synthetic input: a `window-closed` document (`testdata/golden/status/patched-windowclose-over-running.json`) has the key; the document the writer writes next, with the supervisor's own two carry lines applied (`restarted-after-windowclose.json`), does not. The real supervisor was not run.
- Test: `TestKnownDifferenceF1LastWindowCloseIsNotCarried`. The Go producer is expected to carry the key (LLD 2.5, conformance item C-F1); that test will assert the opposite.

## G1 - the writer's `ValidateSet` lacks `window-closed`

- Code: `Write-CamStatusFile` declares `[ValidateSet('', 'requested', 'interrupted', 'unexpected')][string]$StopKind` (`cam.recorder.ps1` line 2755). `window-closed` is written only by the warden patch (`cam.common.ps1`), by `Add-Member`, which has no such check.
- Effect: harmless today (the supervisor never writes it). A Go supervisor that wants to write `window-closed` through its own writer is legal under the schema, which lists it.
- Confirmation: CONFIRMED by reading the code, and by calling `Write-CamStatusFile -StopKind 'window-closed'` in the sandbox: it throws `Cannot validate argument on parameter 'StopKind'. The argument "window-closed" does not belong to the set ",requested,interrupted,unexpected"`.
- Test: `TestKnownDifferenceWriterValidateSetLacksWindowClosed`.

## Where the schema follows the code instead of the draft in IS 1.8

| Item | IS 1.8 draft | Code (the schema) |
|---|---|---|
| `Cameras[].SafeName` for an unusable name | `string` or `null` (IS 1.4) | `""`: the writer casts to `[string]`, so null never appears; type is `string` |
| Required keys | only `Schema`, `State`, `SupervisorPid`, `UpdatedAt` are required, sub-objects mostly open | every key the writer always emits is required (nullable ones are present with `null`): `Disk` 11, `Prune` 5, `Restreamer` 10, `Live` 10, `Cameras[]` 20; top level `Schema`, `State`, `StopKind`, `SupervisorPid`, `UpdatedAt`, plus the full set when `State` is `running`; both patch functions add `State` and `StopKind` to the minimal document, so those two are required too |
| `Live` | `allOf` on the relay shape | its own shape: it has no `RelayBase` |
| Timestamps | `format: date-time` only | `format: date-time` AND a pattern: 7 fractional digits and a numeric offset (`+00:00`, never `Z`), because readers compare the text and the web app dismisses a notice by comparing `At` byte for byte |
| Number types | `FreeGB`, `ArchiveGB` etc. `number` | same; integer-valued numbers print without a decimal point (`259200`), so a Go reader must not require a fraction |

## Observed serialisation facts (Windows PowerShell 5.1, not part of the schema)

- `ConvertTo-Json` indents with 4 spaces, writes `"Key":  value` (two spaces), CRLF line breaks, and escapes `'`, `<`, `>`, `&` as `'`, `<`, ... Readers must not depend on any of it.
- Patches re-serialise the whole document after `ConvertFrom-Json`, so a patched document keeps all fields but may differ in layout from the supervisor's own.
- Verified against a running document: the live `status.json` of the Operator's system validates against this schema with 0 violations and decodes strictly into the Go types (reported as a result only; nothing from it is stored).
