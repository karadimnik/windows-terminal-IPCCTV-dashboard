# Running Holocron in shadow beside the PowerShell system

Before Holocron records the real footage it runs next to the PowerShell system for a while (HLD phase 2, decision D5:
three days counting the start day, two cameras; revised by the Operator on 21 Oct 2026). It writes its own copy of two cameras to a separate folder under a small size cap, and
`holocron archive compare` checks that this copy has the same names, boundaries, sizes, durations and audio as the
PowerShell system's, with no gaps. This page is the whole procedure. Both systems keep working; the shadow can be stopped
and deleted at any time without touching the real archive.

## What runs, and what does not

| | |
|---|---|
| Runs | `holocron up --detach`: the host, the supervisor and one `ffmpeg` per shadow camera. Nothing else |
| Does not run | no MediaMTX (the PowerShell system's relay is shared), no live view, no web app, no autostart, no scheduled task, no install |
| Camera sessions | none opened. Each Tapo camera allows two RTSP sessions and the PowerShell system already uses both (relay and live view). The shadow reads the same relay path the PowerShell recorder reads, `rtsp://127.0.0.1:8554/<path>`, which the relay serves without a login |
| Files | everything under one folder, the shadow root (below), plus the `holocron.exe` copy next to it |
| Ports | only the control API, on `127.0.0.1` and a port the system picks (written to `<root>\_state\ctl.addr`). Not 8554, 9997, 8889, 8189, 8787 or 8443. No LAN binds, so no firewall prompt |
| Started and stopped by | `holocron up --detach` and `holocron down`, by hand. It is detached: closing the terminal does not stop it, and nothing restarts it after a logoff or a reboot |

## How the shadow reads the relay

The PowerShell recorder reads `RelayBase/<path>`, where `<path>` is the camera's safe name in lowercase letters and digits
(`Front Door` becomes `frontdoor`; `Get-CamRelayPathName`). Holocron's own way of doing the same is to start its own
MediaMTX, which here would fight the PowerShell relay for port 8554, rewrite its config and use its API login. So the shadow
sets `RelayExternal` true and `RestreamerEnabled` false: a relay that Holocron does not run already serves every camera path
at `RelayBase` (`rtsp://127.0.0.1:8554`, the default). Holocron then starts, stops and configures no MediaMTX, never touches
`C:\mediamtx`, the relay's API login or anything in `%LOCALAPPDATA%\IPCCTV`, treats the relay as alive and serving every path,
and records each camera with the production relay input, the same vector the PowerShell recorder uses:
`-rtsp_transport tcp -timeout 10000000 -fflags +genpts+discardcorrupt -i rtsp://127.0.0.1:8554/<path>` followed by the segment
output. (`RelayExternal` with `RestreamerEnabled` true is refused by `config validate` and at start.)

Two consequences:

- **A camera login is still required to start, and is never used.** No camera with a relay path reads anything but the relay,
  so use made-up values, in the environment of the shell that runs `holocron up` (`HOLOCRON_CAMERA_USER`,
  `HOLOCRON_CAMERA_PASSWORD`). Do not store them (`holocron secret set`), and never put a real camera login there.
- **No fallback to the camera.** If the relay cannot be read (below) the camera's `ffmpeg` exits and is restarted on the relay
  with the backoff; Holocron never switches it to the camera's own address, which would open a third RTSP session. Only a camera
  whose name has no letter or digit (no relay path) would start on its direct input, as with Holocron's own relay; the
  `cameras.json` below points even that at the relay.

## The shadow root, its size and its cap

Use a local fixed NTFS drive that is not `C:` and not the PowerShell archive's drive (`E:`; it has about 25 GB free and its own
20 GB reserve, so it cannot hold a shadow). The root is `<drive>:\holocron-shadow`. On this PC that is `D:`, a separate
physical disk from `E:`, with about 300 GB free.

The pruner can cap the shadow by age or by free space on the drive. Free space cannot cap a folder, so the age rule does it:

```
per camera: 12 to 16 GB per day (15-minute segments of 120 to 170 MB); two cameras: 24 to 32 GB per day
measured in the pilot, two different cameras (2560x1440 at 15 fps, 2304x1296 at 25 fps): 28.55 GB per day
MaxRetentionDays 1.5  ->  36 to 48 GB at most, 42.8 GB with the measured rate
```

Nothing is pruned for the first 1.5 days; after that the pruner deletes segments older than 1.5 days, a few at a time, and the
shadow stays at its cap. `ReserveGB` is a second guard: it keeps 100 GB free on the drive, and if the drive ever fills from some
other cause the pruner removes the shadow's own oldest segments (never anything else; it only touches files that match the
segment pattern, in camera folders, under a root that carries its ownership marker). The age rule also has its circuit breaker
(`MaxAgePruneFraction`, 0.5): if one run would delete more than half of the archive it deletes nothing and says so. That happens
when a pilot's footage is more than a day older than a new run started on top of it; delete the old footage first or let it age.

## Setting it up

Everything below uses generic names. The real camera names and addresses go only into the shadow's own `cameras.json`, which
is not in the repository.

1. Pick two cameras that record and are alive in the PowerShell system's `status.json`, preferably different (resolution, frame
   rate). Their relay paths are their safe names in lowercase letters and digits. Check one reads: `ffprobe -rtsp_transport tcp
   rtsp://127.0.0.1:8554/frontdoor` should list a video and an audio stream without any login.
2. Create the folders and copy the program. The root must NOT contain the program (the root guard refuses it), so the program
   sits beside the root:

   ```
   D:\holocron-shadow\_config\            holocron.config.json, cameras.json (names starting with _ are the recorder's own)
   D:\holocron-shadow-bin\holocron.exe    build-holocron.sh output
   ```

3. `D:\holocron-shadow\_config\holocron.config.json`:

   ```json
   {
     "RootPath": "D:\\holocron-shadow",
     "FfmpegPath": "C:\\ffmpeg\\bin\\ffmpeg.exe",
     "FfprobePath": "C:\\ffmpeg\\bin\\ffprobe.exe",
     "SegmentSeconds": 900,
     "RestreamerEnabled": false,
     "RelayExternal": true,
     "LiveViewEnabled": false,
     "MaxRetentionDays": 1.5,
     "ReserveGB": 100,
     "alerts": { "desktop": { "enabled": false }, "eventlog": { "enabled": false } },
     "web": { "enabled": false }
   }
   ```

   `SegmentSeconds`, `FfmpegPath` and the other recorder keys must equal the PowerShell system's `recorder.config.json`, or the
   comparison finds differences that are only settings. `web.enabled` is false because the web app's default folders
   (`%LOCALAPPDATA%\IPCCTV`) are the PowerShell system's UI data and `8787` is its port. The alerts are off so the shadow raises
   no toast and writes no Windows event.

4. `D:\holocron-shadow\_config\cameras.json` (the example uses generic names; use the cameras' real names, which decide the
   folder and file names, spelled exactly as in the PowerShell system's `cameras.json`):

   ```json
   [
     { "Name": "Front Door", "IP": "127.0.0.1:8554", "Quality": "frontdoor", "Record": true },
     { "Name": "Back Yard",  "IP": "127.0.0.1:8554", "Quality": "backyard",  "Record": true }
   ]
   ```

   With `RelayExternal` the cameras read `RelayBase/<path>` and `IP` and `Quality` are not used; keep `IP` at `127.0.0.1:8554`
   and `Quality` at the relay path anyway, so nothing in the shadow's files is a camera's address.

5. Check, then start:

   ```powershell
   $env:HOLOCRON_CAMERA_USER = 'shadow'; $env:HOLOCRON_CAMERA_PASSWORD = 'shadow'
   $h = 'D:\holocron-shadow-bin\holocron.exe'; $c = 'D:\holocron-shadow\_config\holocron.config.json'
   & $h --config $c config validate
   & $h --config $c doctor
   & $h --config $c up --detach
   & $h --config $c status
   ```

   `up --detach` waits for an honest start and prints the pids. Started from a WSL shell or another job that forbids breakaway it
   warns that the background copy "may end with the terminal"; in the pilot it did not (it ran 31 minutes after the shell
   returned), but start it from a normal PowerShell window for the long run. `doctor` reports `warn` for ffmpeg being a development build and
   for the root not yet claimed; both are expected. The supervisor claims the root (writes the ownership marker
   `_state\archive-owner.json`) at its first start.

Every later command needs the same `--config`. `holocron down` stops the shadow in order and proves it (exit 1 and the survivors
if anything remains).

## Checking on it

```powershell
& $h --config $c status --json       # cameras alive, current segment, input mode "relay", disk, prune
& $h --config $c archive stats       # segments and GB per camera, GB per day
& $h --config $c logs --since 30m
```

`status` should show both cameras alive with `InputMode` `relay` and the `Restreamer` section with `Enabled` false (Holocron runs
no relay of its own), and the segment names should be the same shape as the PowerShell system's,
`Front_Door_20261021_183001+0300.mkv`. `CurrentSegmentBytes` grows during the segment (the open file's size is read through
an open handle; a Windows directory listing shows 0 until ffmpeg closes it) and `SecondsSinceLastClosedSegment` is the age of
the current segment, 0 to about 900. `status` and `doctor` print `relay: external (RelayExternal: the cameras read rtsp://127.0.0.1:8554/<path> from a relay this system does not run)`, with the configured `RelayBase`.

## Looking at the web app on the shadow's data (optional)

The shadow runs no web app (step 3 above). To look at the new web app on what the shadow records, without touching the shadow's
own configuration, run a second, separate process from a copy of the configuration. This is not part of the shadow's process
tree, and the shadow does not need it.

1. Copy `D:\holocron-shadow\_config\holocron.config.json` to `D:\holocron-shadow\_config\web-ui.config.json` and change only the
   `web` object (everything else, `RootPath` included, stays so the page reads the shadow's archive and status):

   ```json
   "web": {
     "enabled": true,
     "listen": "127.0.0.1:8788",
     "authDir": "D:\\holocron-shadow\\_config\\web\\auth",
     "tlsDir":  "D:\\holocron-shadow\\_config\\web\\tls"
   }
   ```

   The two folders sit under `<root>\_config\web\` so the page never uses `%LOCALAPPDATA%\IPCCTV`, the PowerShell system's UI data.
   Leave `tlsListen` out: only the loopback page is wanted. Use a port that is free (`8787` is the PowerShell system's).

2. Start it in the background and keep its pid:

   ```powershell
   $h = 'D:\holocron-shadow-bin\holocron.exe'; $w = 'D:\holocron-shadow\_config\web-ui.config.json'
   $p = [System.Diagnostics.Process]::Start($h, "--config $w web")
   $p.Id
   ```

   (it opens a console window of its own; or run `& $h --config $w web` in a window of your own and stop it with Ctrl+C).
   Open `http://127.0.0.1:8788/`.

3. **Warning: the loopback page is an admin and its buttons act on the SHADOW.** There is no sign-in on the loopback address.
   Start and Stop, the record toggle of a camera and the Settings save all go through the control API to the shadow's running
   supervisor, and the Settings save also writes the shadow's `cameras.json` and `holocron.config.json`. For a look, use
   Dashboard, Footage and the status; press nothing else.

4. **The Live tab does not work**: the shadow has no live relay (`LiveViewEnabled` is false, and the relay is the PowerShell
   system's).

5. Stop it by the pid you noted, never by the image name (the shadow's own `holocron.exe` processes share it):

   ```powershell
   $p.Kill()
   ```

   (in a new window: `[System.Diagnostics.Process]::GetProcessById(<pid>).Kill()`).

   Then delete `web-ui.config.json` and `D:\holocron-shadow\_config\web` if you do not need them again.

## Comparing

`holocron archive compare` only reads both roots. It needs `ffprobe` (found like `ffmpeg`: `--ffprobe`, else `FfprobePath` of
`--config`, else PATH) and runs it one file at a time, hidden, with a 30 second limit.

```
holocron archive compare --old E:\cameras --new D:\holocron-shadow [--camera NAME ...] [--since TIME] [--json]
                         [--ffprobe PATH] [--audio pcm_alaw:8000:1|any] [--segment-seconds N]
                         [--start-tolerance 5s] [--duration-tolerance 2s] [--gap-tolerance 2s]
```

`--config` (before the command, as always) supplies `SegmentSeconds`, the container and `FfprobePath`; without it the segment
length is 900 and the container `.mkv`. `compare` creates no log file and no folder under either root. Exit codes: `0` equal
within the tolerances, `1` differences (the report is printed), `2` usage (a missing or equal root, a bad value, no ffprobe).

What it does, per camera (every camera folder of `--new`, or the `--camera` names; a name with a space is accepted):

- **Closed segments only.** A file written within the last 90 seconds is open and is skipped, and listed as such. So is the newest
  segment of each camera folder until it is a segment length and a minute old, because on Windows the time a directory listing
  shows for a file that is still being written can lag (so a recorder that was stopped less than that ago has its last file skipped).
- **The window** is the overlap of the two archives: from the later of the two first starts to the earlier of the two last ends,
  both taken from closed segments. `--since` moves the start later. The two recorders start their first partial segment at
  different instants, so the part of the older archive before the shadow existed is never compared.
- **Pairs.** A new and an old segment whose start times differ by at most 5 seconds are a pair. For each pair: the sizes
  (new divided by old, median, minimum and maximum over the camera; outside 0.90 to 1.10 is a difference), the ffprobe durations
  (within 2 seconds), the time zone suffix of the two names, the video codec and size, and the audio (`pcm_alaw`, 8000 Hz, one
  channel on both sides unless `--audio any`; the two sides must agree either way).
- **Partial segments are handled explicitly.** A new segment without a start partner that lies inside an old one is a *partial*
  (the shadow's first file, or the part after a restart) and is reported, not counted as missing. A pair whose durations differ
  because the shorter one ends where the window ends (the shadow was stopped, or still running) is `partial_end` and is not
  compared for size or duration. A shorter segment anywhere else is a difference.
- **A file that was never finalized.** `holocron down` kills `ffmpeg` (a hard kill, as the PowerShell stop does), so the file
  the shadow was writing has its streams but no duration (and loses its last buffered seconds). `ffprobe` shows no duration for it.
  As the last file of a side that is a note and its length is estimated from its write time (the pair is `partial_end`); anywhere
  else it is a difference.
- **Names and boundaries.** Segment names must be `<Camera>_YYYYMMDD_HHMMSS+ZZZZ.mkv` of the camera's folder (a foreign file in
  the new folder is a difference, in the old one a note). Segments are cut on multiples of the segment length of the name's own
  clock time; a segment that follows the one before it without a gap and does not start on such a boundary (within 5 seconds)
  is a difference. After a restart a segment may start anywhere. Names are compared as instants, so the clock changes in
  October and March (`+0300` becomes `+0200` and the names are not in text order) are handled.
- **Gaps.** The time from the end of one segment (its start plus its ffprobe duration) to the start of the next, over 2 seconds,
  on each side. A gap that both sides have over the same time (a camera or relay outage) is a note; a gap on one side only is a
  difference.
- **Missing and extra.** An old segment in the window with no new partner (and not holding a partial) is `missing from new`; the
  reverse is `extra in new`.
- No time to compare (no overlap, no closed segment on a side, an unreadable file) is a difference, never "equal".

### The JSON report (`--json`, schema 1)

Stable: a key is never renamed or removed without a new `schema`; lists are `[]`, never `null`; times are RFC 3339; sizes in bytes,
durations and differences in seconds.

| Key | Meaning |
|---|---|
| `schema`, `old`, `new`, `checked_at`, `params` | version (1), the two roots, the time of the run, the settings used (`segment_seconds`, `start_tolerance_seconds`, `duration_tolerance_seconds`, `gap_tolerance_seconds`, `size_ratio_min`, `size_ratio_max`, `quiet_seconds`, `container`, `audio`, `since`) |
| `equal`, `differences` | the verdict and the number of differences over all cameras |
| `cameras[]` | one object per camera, below |

Per camera: `camera` (folder), `equal`, `window` (`from`, `to`, `seconds`, or `null`), `old` and `new` (`closed_segments` found,
`compared_segments` in the window, `compared_bytes`, `skipped_open`, `gaps[]` of `after`, `before`, `seconds`, `shared`), `pairs[]`
(`old`, `new`, `kind` `full`, `short`, `partial_end` or `mismatch`, `start_diff_seconds`, `old_bytes`, `new_bytes`, `size_ratio` or
`null`, `old_duration_seconds`, `new_duration_seconds`, `duration_diff_seconds`, `old_layout`, `new_layout`, `flags[]`),
`partials[]` (`side`, `name`, `within`, `duration_seconds`, `end_diff_seconds`, `layout`, `ends_with_partner`), `unmatched_old[]`,
`unmatched_new[]`, `size_ratio` (`pairs`, `median`, `min`, `max`), `differences[]` (the sentences, empty when equal) and `notes[]`.

## When the PowerShell system restarts, or the relay goes away

The relay is shared. If the PowerShell system is restarted (`cam.ps1 -Stop` and `-Start`, a reboot, a crash of its MediaMTX) the
relay disappears for a while and every reader of it gets a closed connection: the PowerShell recorder's `ffmpeg` and the
shadow's alike. The shadow does nothing special and needs nothing from you: its `ffmpeg` exits or fails to connect, the
supervisor restarts it on the relay with its backoff (3 s, doubling to 60 s, reset after a healthy 300 s) and it records again as soon as the
path is served. It does not crash-loop and the supervisor stays up (proved by `TestShadowRigSurvivesARelayThatIsGoneAndRecovers`
in `internal/app`, with `RelayExternal`: the relay input vector, against a relay that refuses a few starts in seven seconds
and a running supervisor; recording resumes by itself when the relay returns). The gap this leaves is a gap in the PowerShell system's archive too, and `compare` reports it
as a shared note, not a difference. If the shadow itself was down while the PowerShell system recorded, the same gap is a
difference, which is correct.

Stopping the shadow never touches the relay or the PowerShell recorder, and the PowerShell system's `-Stop` does not stop the
shadow (it is a different process tree, started and stopped by `holocron` alone).

## Stopping, aborting and deleting

```powershell
& $h --config $c down          # ordered stop, proves 0 survivors
& $h --config $c status        # not running
```

To abort and delete everything the shadow wrote, stop it first, then remove exactly its two folders (File Explorer does as
well). Check the paths in the commands before you press Enter; they are the shadow's only:

```
cmd /c rmdir /s /q D:\holocron-shadow
cmd /c rmdir /s /q D:\holocron-shadow-bin
```

The pruner is the only part of Holocron that deletes footage, and only inside the shadow root. `holocron prune --dry-run` shows
what it would delete. Nothing else of the PowerShell system, `E:\cameras`, `C:\mediamtx` or `%LOCALAPPDATA%\IPCCTV` is touched
by any step on this page.

## Not covered

The shadow's DST behaviour is covered by the comparison's handling of changing `%z` names (a unit test runs a fall-back night),
not by a run across a clock change; the next one is 25 Oct 2026. The 3-day shadow (21 to 23 Oct 2026) ends before it, so that night is NOT covered by a
run; cut over after 26 Oct or extend the shadow past it. The web app, the live view and the alerts are not part of the shadow. Real cameras, sleep and resume are
Operator checks.

## Result of the first run (21 Oct 2026 20:38 to 22 Oct 10:00, Athens)

Two cameras (A: 2560x1440 at 15 fps, B: 2304x1296 at 25 fps), 13.4 hours, one midnight rollover, read through the PowerShell
system's relay with `RelayExternal`. `holocron down` stopped it with 0 survivors. `holocron archive compare` against the
PowerShell archive:

| | Camera A | Camera B |
|---|---|---|
| Segments compared | 54 pairs | 53 pairs |
| Start times | all +0.0 s | all +0.0 s |
| Size new/old | median 0.999, min 0.999, max 1.000 | median 0.999, min 0.999, max 1.002 |
| Durations, layout, audio | equal, h264 + pcm_alaw 8000 Hz mono | equal, same |
| Result | 1 difference, explained below | EQUAL within tolerances |

The one difference: at 06:05 the relay dropped camera A's path (404) for both systems. Holocron's ffmpeg exited and the
supervisor restarted it after 3 s and then 6 s; the gap was 25.3 s against the PowerShell recorder's 21.3 s, so the first
segment after the outage started 4 s later and is 2.3 s shorter than the old one (tolerance 2 s). Both end on the same
clock boundary. It is the restart backoff, not a recording fault.

Not covered by this run: the pruner acting on a full cap (it first acts after 1.5 days), the 25 Oct 2026 clock change, the
four cameras that were not shadowed, the live view and the alerts. The pilot and run footage stays in `D:\holocron-shadow`
until the Operator deletes it.
