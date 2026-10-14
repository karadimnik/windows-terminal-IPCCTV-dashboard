# Configuration reference

`recorder.config.json` overlays the built-in defaults; any key you leave out keeps its default, and the file is read by the recorder, the health strip and the dashboard alike. Keys marked * are not in `recorder.config.example.json` and have to be added by hand if you want to change them.

**Four keys are editable from the control UI's Settings screen** - `SegmentSeconds`, `ReserveGB`, `PruneHysteresisGB` and `MaxRetentionDays`, marked **UI** below. Everything else on this page is tuning that nobody should need, and is here for when you do.

Values written by the UI go through the same code the CLI uses: validated, written atomically, and the file keeps its UTF-8 BOM.

## Paths and recording

| Key | Default | Meaning |
| --- | --- | --- |
| `RootPath` | `E:\cameras` | Where footage, `_state` and `_logs` live. |
| `FfmpegPath` / `FfprobePath` | `C:\ffmpeg\bin\...` | The recording tools. |
| `SegmentSeconds` **UI** | `900` | Segment length, aligned to the clock. |
| `Container` | `mkv` | Which entry of `ContainerProfiles` to use. `fmp4` re-encodes audio to AAC. |
| `ContainerProfiles` | `mkv` and `fmp4` | The profiles themselves: per profile an `Extension`, a `SegmentFormat`, `SegmentFormatOptions`, and `AudioArgs` / `ExtraArgs` added to the ffmpeg command line. Add a profile here to add a container; there is no other place that knows about one. |
| `RtspTransport` * | `tcp` | Transport for the recorder's inputs. |
| `TimeoutMicroseconds` * | `10000000` | ffmpeg's socket timeout. |

## Retention

| Key | Default | Meaning |
| --- | --- | --- |
| `ReserveGB` **UI** | `20` | Free space the pruner defends. |
| `PruneHysteresisGB` **UI** | `5` | How much extra it frees once it starts, so it does not thrash. |
| `PruneIntervalSeconds` | `300` | How often it looks. |
| `MaxRetentionDays` **UI** | `null` | Optional age cap, on top of the space rule. |
| `MaxAgePruneFraction` | `0.5` | Circuit breaker on `MaxRetentionDays`. A run that would delete more than this share of the archive by age - by segment count or by bytes - deletes nothing by age, logs one `PRUNE AGE RULE HELD` line with both shares and alerts once (event id 122), so a mistyped retention cannot empty the archive. Lower `MaxRetentionDays` in steps to shorten retention a long way. Must be from `0.01` to `1.0`; `1.0` switches the brake off. Never holds back the free-space rule: a disk below `ReserveGB` keeps pruning. |

## Supervisor

Every key here is a tuning constant rather than a setting: the defaults are the values this system was measured on, and a wrong one will degrade a working supervisor rather than configure it.

| Key | Default | Meaning |
| --- | --- | --- |
| `ReconcileSeconds` * | `5` | How quickly an `-Enable` or an edited IP is picked up. |
| `StatusIntervalSeconds` * | `5` | How often `status.json` is rewritten. |
| `ArchiveScanSeconds` * | `60` | How often the whole archive is measured for the disk figures. Separate because it touches every file. |
| `StopTimeoutSeconds` * | `30` | How long `-Stop` waits for a clean teardown. |
| `BackoffStartSeconds` / `BackoffMaxSeconds` * | `3` / `60` | Restart backoff for a failing child. |
| `HealthyResetSeconds` * | `300` | How long a child must stay up before its backoff resets. |
| `StallFactor` * | `2.5` | Multiplied by `SegmentSeconds` for the stall watchdog. |
| `CameraDownAlertMinutes` * | `15` | How long a camera may write nothing before it raises an alert. |
| `CameraDownRepeatHours` * | `12` | How long before the same camera alerts again while still down. |
| `LogMaxBytes` * | `2 MB` | Log rotation threshold. |
| `LogRepeatSuppressSeconds` * | `60` | Collapses a repeating log line. |

## Relay

The paths and addresses here are configuration; the keys marked * are tuning constants, set from measurement, and a wrong one will degrade a working relay rather than configure it.

| Key | Default | Meaning |
| --- | --- | --- |
| `RestreamerEnabled` | `true` | Set false to run on direct camera inputs only. |
| `RestreamerExePath` | `C:\mediamtx\mediamtx.exe` | The binary. Must be on a local drive. |
| `RestreamerConfigPath` | `C:\mediamtx\mediamtx.yml` | The generated config. Must be on a local NTFS path: MediaMTX watches it with `ReadDirectoryChanges`, which the WSL share does not implement, and exits immediately if it cannot. |
| `RelayBase` | `rtsp://127.0.0.1:8554` | Where consumers read. Loopback only; the generated config allows anonymous access precisely because nothing off the machine can reach it. |
| `RelayApiBase` | `http://127.0.0.1:9997` | The control API the readiness probe uses. |
| `RelayProbeTimeoutMs` | `1500` | Budget for one probe. |
| `RelayStartupWaitSeconds` * | `20` | How long the supervisor waits for the relay's control API to answer before starting any recorder. The API only, not every path: one offline camera's path can never become ready and would otherwise hold up the healthy ones. |
| `RelayReadyWaitSeconds` * | `15` | How long **one** consumer waits for **its own** relay path before giving up and taking a direct camera slot. Spent only on a first attempt at batch startup; a later attempt decides on the spot, so a relay that died is left behind immediately. |
| `RelayReadyPollMs` * | `500` | How often that wait re-asks. Floored at 50. |
| `ViewerPromoteCheckSeconds` * | `10` | How often an `mpv` pane that is on a direct connection asks whether its camera is back on the relay, so it can hand the second session back. One control-API call per direct pane per interval. `0` switches the watch off and leaves such a pane direct until the dashboard is relaunched. |
| `StartStaggerMs` * | `300` | Delay between consecutive consumer starts, recorder children and `mpv` panes alike, so six of them do not burst onto the relay, the control API and the cameras in the same instant. |

## Dashboard layout and health strip

`HealthStripHeightPx` is the one setting here - it is how you resize the strip or switch it off. The keys marked * are tuning constants that describe the layout this system already computes correctly, and a wrong one will degrade a working grid rather than configure it.

| Key | Default | Meaning |
| --- | --- | --- |
| `HealthStripHeightPx` | `-1` | `-1` derives the strip height from the video aspect, `0` switches the strip off, a positive number is that many pixels with the panes shrunk to fit. |
| `HealthStripMinPx` * | `120` | The strip never gets less than this; the panes shrink instead. |
| `GridGutterPx` * | `8` | Gap between grid rows and above the strip. Never at the screen edges. |
| `VideoAspectWidth` / `VideoAspectHeight` * | `16` / `9` | The shape of the video, which fixes pane height at a given width. |
| `HealthSections` * | `status, cameras, events, archive` | Which sections the strip renders, in this order. An unknown name is an error, not a silent skip. |
| `HealthEventRows` * | `5` | Rows the events section occupies, its label row included. |
| `StatusEventCount` * | `20` | How many recent log lines `status.json` publishes for the strip. |
| `HealthCellWidthPx` / `HealthCellHeightPx` * | `9` / `20` | Approximate font cell size, used only to ask Windows Terminal for a roughly right window before it is placed exactly. Neither can affect correctness. |

## The camera list

`cameras.json` is a separate file with its own schema - see [install.md](install.md#camerasjson). The control UI's Settings screen edits it too: add, remove, rename, change the IP, the stream and the record flag.
