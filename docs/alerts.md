# The alerts

An unexpected stop alerts on three channels, and an orderly `cam.ps1 -Stop` raises none of them: a **Windows toast** (WinRT under the Windows PowerShell AUMID - unelevated, does not steal focus, stays in the Action Center), an **Application-log entry** prefixed `IPCCTV: `, and **`StopKind` plus `LastUnexpectedStop` in `status.json`**, the latter carrying the timestamp, the reason and the pid, and carried across restarts so the NVR being back up does not erase the fact that it went down.

| id | Level | What it means |
| --- | --- | --- |
| `100` | Error | the supervisor stopped unexpectedly and is being restarted |
| `101` | Warning | it is recording again |
| `102` | Error | it stopped and the restart budget is spent, so it is staying down |
| `103` | Warning | the NVR window was closed, so nothing is recording and nothing will restart it |
| `110` | Warning | one camera has written nothing for `CameraDownAlertMinutes` |
| `120` | Error | pruning is disabled: `RootPath` is unsafe or not the recorder's own archive, so the disk will fill |
| `121` | Error | pruning is blocked: free space is below `ReserveGB` and nothing deletable is left, so the disk is filling. Once per episode; recording continues |
| `122` | Error | the age rule's circuit breaker held a run that would have deleted more than `MaxAgePruneFraction` of the archive. Once per episode; the free-space rule still runs |
| `130` | Error | the relay's control API refused the product's login, so which cameras it is serving cannot be seen; every consumer is kept on the relay. Once per episode; restart with `cam.ps1 -Stop` and `-Start` |

Registering a custom event source writes to `HKLM` and needs elevation, so by default the entries go out under the always-registered `Application` source. `.\cam.recorder.ps1 -InstallEventSource`, run elevated once ([Install and uninstall](../README.md#install-and-uninstall)), registers the `IPCCTV` source instead, with `EventCreate.exe` as the message resource so `.Message` renders. Everything works without it; that is what the prefix is for. Reading them back:

```powershell
Get-WinEvent -FilterHashtable @{ LogName = 'Application'; Id = 100,101,102,103,110,120,121,122,130 } |
  Select-Object TimeCreated, Id, LevelDisplayName, ProviderName,
    @{ n = 'Text'; e = { if ($_.Message) { $_.Message } else { [string]$_.Properties[0].Value } } } |
  Where-Object { $_.Text -like 'IPCCTV: *' }
```

That filters on the `IPCCTV: ` prefix, not on a provider, **on purpose**: the entries arrive under `Application` until `-InstallEventSource` has been run and under `IPCCTV` after, so pinning `ProviderName = 'Application'` would quietly return nothing after the install; the `ProviderName` column tells you which answered. The ids alone are not ours exclusively in a log every application writes to, and the `Properties[0].Value` fallback covers a source with no message resource, which leaves `.Message` empty. With no matching events `Get-WinEvent` reports "No events were found that match the specified selection criteria" - the quiet answer, not a fault.
