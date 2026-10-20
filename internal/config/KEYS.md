# holocron.config.json keys

Source: `Get-CamConfigDefault` (cam.common.ps1) and inventory IR section 4. A test (`TestDefaultsMatchPowerShellSource`) reads the
PowerShell function and fails if a key is neither kept nor listed as dropped, or if a scalar default differs (Windows values).
Defaults shown are the PowerShell/Windows ones; `Defaults(p)` uses `p.Dirs()`/PATH elsewhere (root `/srv/cameras`, `ffmpeg`, `mediamtx`).

Types are strict: a JSON string is never accepted for a boolean or a number (PowerShell casts them; IR 10 item 9). `null` is accepted only for
`MaxRetentionDays`. Rules marked **Go** are additions the PowerShell loader did not check (it failed at first use instead).

## Kept

| Key | Default | Rule | Why kept |
|---|---|---|---|
| RootPath | `E:\cameras` (`/srv/cameras`) | non-blank string without `%` | archive root |
| FfmpegPath | `C:\ffmpeg\bin\ffmpeg.exe` (`ffmpeg`) | string; empty = look up on PATH | recorder |
| FfprobePath | `C:\ffmpeg\bin\ffprobe.exe` (`ffprobe`) | string | kept for diagnostics; cheap |
| SegmentSeconds | 900 | whole number >= 1 | segment length |
| Container | `mkv` | must name an entry of ContainerProfiles (**Go**) | container choice |
| ContainerProfiles | mkv + fmp4 | object; the file's map REPLACES the defaults; each: Extension `.` + 1-8 alnum (**Go**: it decides what the pruner may delete), SegmentFormat non-blank, AudioArgs/ExtraArgs string arrays, SegmentFormatOptions string | config-driven container |
| RtspTransport | `tcp` | one of tcp, udp, udp_multicast, http, https (**Go**) | ffmpeg input |
| TimeoutMicroseconds | 10000000 | whole number >= 1 (**Go**) | ffmpeg input |
| ReserveGB | 20 | number >= 0 (**Go**) | pruner watermark |
| PruneHysteresisGB | 5 | number >= 0 (**Go**) | pruner target |
| PruneIntervalSeconds | 300 | number > 0 (**Go**) | pruner cycle |
| MaxRetentionDays | null | null (off) or number > 0 (**Go**: PowerShell treated <= 0 as off) | age rule |
| MaxAgePruneFraction | 0.5 | number 0.01-1.0 (PowerShell also parsed a numeric string; Go wants a number) | age-rule breaker |
| ReconcileSeconds, StatusIntervalSeconds, ArchiveScanSeconds | 5, 5, 60 | number > 0 (**Go**) | supervisor loops |
| CameraDownAlertMinutes, CameraDownRepeatHours, StopTimeoutSeconds | 15, 12, 30 | number > 0 (**Go**) | alerts, stop wait |
| BackoffStartSeconds, BackoffMaxSeconds, HealthyResetSeconds | 3, 60, 300 | whole number >= 1; Max >= Start (**Go**) | restart backoff |
| StallFactor | 2.5 | number > 1 | stall watchdog |
| LogMaxBytes | 2097152 | whole number >= 1 (**Go**) | log rotation |
| LogRepeatSuppressSeconds | 60 | number >= 0 (**Go**) | repeat collapse |
| StatusEventCount | 20 | whole number >= 0 (**Go**) | events in status.json |
| RestreamerEnabled | true | JSON boolean | relay on/off |
| RestreamerExePath, RestreamerConfigPath | `C:\mediamtx\...` | string | relay |
| RelayBase, RelayApiBase | `rtsp://127.0.0.1:8554`, `http://127.0.0.1:9997` | string; if set, needs a host:port | relay |
| RelayProbeTimeoutMs | 1500 | whole number; below 100 is raised to 100 with a warning (PowerShell clamped at use) | relay probe |
| RelayStartupWaitSeconds | 20 | whole number >= 0 (**Go**) | relay wait |
| RelayReadyWaitSeconds, StartStaggerMs | 15, 300 | whole number; below 0 is raised to 0 with a warning | relay wait, stagger |
| LiveViewEnabled | false | JSON boolean | live view |
| LiveExePath, LiveSecretPath | `` | string, not starting with `\\` | live view |
| LiveConfigPath | `C:\mediamtx\mediamtx-live.yml` | non-blank, not starting with `\\` | live view |
| LiveWhepAddress | `127.0.0.1:8889` | `127.0.0.1:<1-65535>`; not the relay's host:port | live view |
| LiveUdpAddress | `127.0.0.1:8189` | `127.0.0.1`, `0.0.0.0` or one private IPv4, port 1-65535 | live view |
| LiveAdvertiseHosts | `[]` | <= 4 unique private IPv4 literals; consistency with LiveUdpAddress | live view |
| LiveSubStream | `stream2` | `^[A-Za-z0-9_-]{1,32}$` | live view |
| LiveCloseAfterSeconds | 10 | whole number 1-3600 | live view |
| logging | absent | object `{level, levels{component: level}}` with ERROR/WARN/INFO/DEBUG/TRACE | NEW in holocron (LLD 2.13) |

An unknown key starting with `Live` (any case) is an ERROR (a misspelled `LiveViewEnabled` would otherwise leave live view off silently), as
in PowerShell. Any other unknown key is a warning. Key names are case-insensitive; two spellings of one key are an error.

## Dropped (accepted and ignored without a warning, so an existing file loads unchanged)

| Key | Default | Why dropped |
|---|---|---|
| HealthStripHeightPx | -1 | dashboard layout; read only by the PowerShell health strip and camui |
| HealthStripMinPx | 120 | same |
| GridGutterPx | 8 | same |
| VideoAspectWidth, VideoAspectHeight | 16, 9 | same |
| HealthCellWidthPx, HealthCellHeightPx | 9, 20 | same |
| HealthSections | status, cameras, events, archive | same |
| HealthEventRows | 5 | same |
| RelayReadyPollMs | 500 | read only by the mpv panes (PowerShell-only viewer) |
| ViewerPromoteCheckSeconds | 10 | same |

Not preserved on rewrite: this package only reads `holocron.config.json`; the `holocron config set` task must edit the document in place
(it needs to keep dropped and unknown keys).

# cameras.json

Array of objects. `Name`, `IP`, `Quality` (strings; blank/absent Quality = `stream1`), `Record` (JSON boolean; absent = off). Every other field is
kept, in place, across `Save`. Types are checked at load (all problems at once, with `cameras[i].Field` paths); `Validate` checks the meaning
(a name that `archive.SafeName` refuses, an address, a stream name, folder collisions). The web app's extra limits (48-character names, a required
stream) stay in the web layer. `Save` is atomic (temp file in the same folder, fsync, rename) and writes a UTF-8 BOM exactly when the file on
disk has one (a new file: when the list was read from a file that had one).
