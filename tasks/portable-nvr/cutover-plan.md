# Cutover plan (Operator-approved to execute; the date is the one open decision)

Procedure with every command and expected output: [docs/cutover.md](../../docs/cutover.md). Way back: [docs/rollback.md](../../docs/rollback.md).
This page is the schedule, the owners and the stop rules. `<old folder>` is the folder the old system is started from by hand
(the portable worktree), `<archive root>` is the old `RootPath`.

## Decisions

| | Decision | Status |
|---|---|---|
| Date | Recommended: 27 Oct 2026 (first ordinary day after the 25 Oct clock change, which the 13-hour shadow did not cover). Earlier needs the Operator's explicit word ("cut over now"); the risk is only that the autumn clock change then happens under Holocron, with rollback ready. | OPEN |
| Scope | All 6 cameras, one system. A subset would mean two supervisors on one root, which is forbidden. | decided |
| Window of the logon task | `visible` (the default): the console window IS the system, closing it stops everything. `hidden` is the alternative. | default visible, change on request |
| Phone / HTTPS listener | Optional. Needs a user (password chosen by the Operator), the CA installed on the phone and a firewall rule (elevated). | OPEN, Operator |
| Old system after cutover | Stays on disk exactly as it is; tasks stay disabled; removing it is a separate decision after one good week. | decided |

## Who does what

| Step | Owner | When | Notes |
|---|---|---|---|
| Program in `C:\Holocron`, config copied and validated, rehearsal sandbox, rollback commands rehearsed on the sandbox | Advisor | DONE 22 Oct | `holocron 9bd63b7`, config `ok`, 6 cameras, recording code identical to the shadowed build |
| Import the camera login into the keyring (`holocron secret import`) | **Operator** | before the cutover | DPAPI file: only the Operator's account can read it and no agent opens it. Exact command in the cutover message. |
| Choose the date; say "go" | **Operator** | - | Everything below starts on "go" |
| Optional phone setup: password for `holocron user add`, firewall rule (elevated), CA install on the phone | **Operator** (Advisor prepares the commands and the config line) | any day before | not needed for recording |
| C1 to C10 of docs/cutover.md: look at the old system, stop it, prove nothing is left, check the root, start, verify, install the logon task, write it down | Advisor, from WSL through PowerShell, with the stop rules below | on "go", about 30 minutes, gap = time from C3 to C7 | no step needs the Operator, who may watch the old `cam.nvr` window close |
| First-week daily check (`status`, `archive stats`, `logs --level warn`) and the two-day checks (pruner acts, logoff/logon, reboot) | Advisor when the session is active; Operator for logoff/logon and reboot (they need his desktop) | days 1 to 7 | |

## Preconditions the Advisor checks at "go" (all must hold)

1. `HOLOCRON_*` variables unset in the executing shell; `HOLOCRON_SANDBOX_ONLY` unset.
2. `holocron secret list` shows `camera.user` and `camera.password` (the import was done).
3. `C:\Holocron\holocron.exe version` is the recorded build; `config validate` ok on `C:\Holocron\holocron.config.json`.
4. Old system healthy by `cam.ps1 -Status` (supervisor, 6 ffmpeg, relay 6 paths, UI serving): cutting over a sick system proves nothing.
5. The old tasks are Disabled (read-only query). Free disk at or above the reserve.
6. The pid of the old UI (`camui.exe`) and of the old supervisor and its `cam.nvr` window are written down.

## Stop rules during C1 to C10

- Any `fail` from `doctor` on the real root, any `REFUSED` or `circuit breaker` in `prune --dry-run`: stop before C7, nothing has started, restart the old system only if C3 already ran.
- `NOT RECORDING` or exit 20/21 at C7: read the reason once, fix once; if it fails again, `holocron down` and roll back.
- A camera without a new segment two boundaries after C7, a far smaller segment, or missing audio at C8: roll back.
- Defender blocks the exe at any point: do nothing to Defender, roll back if recording stopped, tell the Operator.

## Rollback for this machine (what "old system" means in practice)

The three logon tasks point at the ORIGINAL repository folder through the launcher stubs, but the running system is started by
hand from `<old folder>` (the portable worktree: `cam.recorder.ps1 -Nvr`, then `camui.exe`). So the rollback is the hand start,
not `schtasks`:

1. `holocron down` (proof: `stopped: 0 survivors`), `holocron autostart disable`.
2. From `<old folder>` in a fresh PowerShell window: `.\cam.recorder.ps1 -Nvr`, then start `<old folder>\camui.exe`.
3. `.\cam.ps1 -Status` shows the supervisor, 6 ffmpeg, the relay with 6 paths and the UI serving.
Leave the three old tasks Disabled; do not use `cam.ps1 -Start` (it runs the installed stub's target, the original repository).

## Rollback triggers (any one)

Alert 102, 120, 121 or 122 at any time; more than one alert 100 a day; a camera not recording; audio missing; the disk
falling below the reserve without the pruner acting; the Operator says so.

## After cutover

Record the time, the `holocron version` line, the "before" `archive stats` and the first `status` in this file (C10).
Keep the old system untouched for one good week; the week ends with a separate decision about removing it.
