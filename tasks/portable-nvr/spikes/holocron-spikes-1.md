# Holocron spikes, round 1 (SP1, SP1b, SP4, SP6, SP7)

Run 2026-10-17 on the Operator's machine (Windows 11 build 22621, WSL2 Ubuntu 20.04, bridged networking, system zone GTB = +03:00 on the day).
Experiment code: `spikes/` (Go, standard library only), drivers in `tests/holocron/`. Raw run output was kept under the scratch folders and is
summarised in the tables; the scratch folders were removed at the end.

Binaries under test (this matters for every ffmpeg finding):

| Label | Binary | Version |
|---|---|---|
| win8 | `C:\ffmpeg\bin\ffmpeg.exe` | 2026-07-06 git (gyan essentials), FFmpeg 8.x line (libavformat 63) |
| lin42 | `/usr/bin/ffmpeg` (WSL Ubuntu 20.04 package) | 4.2.7 (libavformat 58.29) |
| lin8 | BtbN static build `N-127579-g5cb9fcd211-20261016`, downloaded into a scratch directory and run only as `ffmpeg`/`ffprobe` | FFmpeg master (8.x line) |

Deviation from the brief: the brief allowed a download only when WSL had no ffmpeg. WSL has one, but it is 4.2.7, five major versions behind
the Windows build, so every Windows-versus-Linux difference would have been confounded with a version difference. lin8 was downloaded to
separate the two. lin42 results are kept because Ubuntu 20.04 users really have it.

## SP1 - ffmpeg arguments on Linux versus Windows

**Question.** Same names, boundaries and `%z` on Linux and Windows for the segment-muxer vector of IR 1.1? Local or UTC multiples for
`-segment_atclocktime`, also at 7 s and at +05:30 / -02:30? Does `TZ` matter (U2, V8)? `-timeout` semantics (V13)? Anything on stdout (U7)?

**Method.** The vector of IR 1.1 with the two lavfi sources of the brief. Additions: `-re` on both inputs (lavfi otherwise runs faster than real
time and the names would all land in one second), `-map '1:a:0?'` (the audio is input 1 here), `-t 45` (10 s runs) / `-t 35` (7 s runs) so that
at least three full segments are written. Everything from `-f segment` to `-segment_format matroska` and the pattern
`Test_%Y%m%d_%H%M%S%z.mkv` is byte for byte the product's. Eight cases per ffmpeg (4 zones x 2 lengths) run in parallel, one folder each.

```
ffmpeg -hide_banner -loglevel warning -nostdin -re -f lavfi -i testsrc2=size=320x180:rate=10 -re -f lavfi -i sine=frequency=440:sample_rate=8000 \
  -map 0:v:0 -map 1:a:0? -c:v libx264 -preset ultrafast -g 10 -c:a pcm_alaw -ar 8000 -ac 1 -t 45 \
  -f segment -segment_time 10 -segment_atclocktime 1 -reset_timestamps 1 -strftime 1 -segment_format matroska <dir>/Test_%Y%m%d_%H%M%S%z.mkv
```

Linux: `bash tests/holocron/sp1-run.sh linux <ffmpeg> <outdir>` with `TZ` set per case. Windows: `tests/holocron/sp1-run.ps1 -Ffmpeg C:\ffmpeg\bin\ffmpeg.exe
-Out <dir> -Cases sys,utc,bucI,kolI` and `-Cases stjI,bucP,kolP,stjP` (hidden `Process`, per-process `TZ`; `sys` = `TZ` removed). Analysis:
`python3 -I tests/holocron/sp1-analyze.py <dir> <case>...` (ffprobe duration; name time modulo N as local seconds-of-day, as UTC seconds-of-day,
and as epoch seconds). Windows cases use two spellings of each zone: `I` = IANA (`Europe/Bucharest`), `P` = POSIX (`EET-2EEST`, `IST-5:30`, `NST3:30NDT`).

**Raw results.** Segment names, 10 s runs (first file is the partial one; the last file is the one cut short by `-t`):

| ffmpeg | TZ | names (first four) | all cuts at |
|---|---|---|---|
| lin42 | UTC | `..113646+0000`, `113650`, `113700`, `113710` | whole 10 s of the local day |
| lin42 | Europe/Bucharest | `..143646+0300`, `143650`, `143700` | same |
| lin42 | Asia/Kolkata | `..170646+0530`, `170650`, `170700` | same |
| lin42 | America/St_Johns | `..090646-0230`, `090650`, `090700` | same |
| lin8 | the four above | identical pattern (`141231+0000`, `171231+0300`, `194231+0530`, `114231-0230`; then `...40`, `...50`, `...00`) | same |
| win8 | unset (system zone) | `..170820+0300`, `170830`, `170840` | same |
| win8 | `UTC` | `..140820+0000`, ... | same |
| win8 | POSIX `EET-2EEST` / `IST-5:30` / `NST3:30NDT` | `+0300` / `+0530` / `-0230`, all offsets right | same |
| win8 | IANA `Europe/Bucharest`, `Asia/Kolkata`, `America/St_Johns` | all three give `+0100`, wall clock 15:08 while UTC was 14:08 and the system zone 17:08: the name is mis-parsed as "3-letter zone name + DST rule", not honoured | same |

Segment length 7 s, name seconds-of-day modulo 7 (after the first, partial file; identical on lin42, lin8 and win8):

| Zone | local-seconds-of-day mod 7 | UTC-seconds-of-day mod 7 | epoch mod 7 |
|---|---|---|---|
| UTC | 0 | 0 | 5 (const) |
| +03:00 | 0 | 1 | 6 (const) |
| +05:30 | 0 | 3 | 1 (const) |
| -02:30 | 0 | 5 | 3 (const) |

For 10 s the three measures coincide except epoch, which is also 0 here because 10 divides the day and all four offsets are multiples of 10 s.
Durations (ffprobe, closed files): 10 s runs 10.008-10.112 s (min 10.00, max 10.11); 7 s runs 7.000-7.120 s. Overshoot is below one GOP (1 s), as IR 1.4 says.
First and last files are partial (3-9 s, 1-7 s) by construction.

`-timeout` against RTSP (silent TCP server that accepts and never answers, 3 s value; plus MediaMTX for the happy path on Windows):

| ffmpeg | `-timeout 3000000` | `-stimeout 3000000` |
|---|---|---|
| win8 | gives up after 3.4 s, exit 183 "Failed reading RTSP data" | rejected: `Unrecognized option 'stimeout'`, exit 8 |
| lin8 | gives up after 3.1 s, exit 183 "Connection timed out" | rejected, exit 8 |
| lin42 | **fails at once (0.1 s)**: `Unable to open RTSP for listening ... Address already in use` (the option is the *listen* timeout in seconds; any value turns the demuxer into a server) | works: gives up after 3.3 s |

A 10 s value to an unreachable host: lin8 10.1 s (`Connection timed out`), lin42 with `-stimeout` 10.2 s. `-timeout 3` (not microseconds) on win8/lin8 times out at once (0.2-0.4 s). Without any timeout option lin42 waited indefinitely (cut by my 20 s cap).

stdout: 0 bytes in all 16 lavfi runs on win8, lin42, lin8 (stderr 0 bytes on Linux with `-loglevel warning`; on Windows `-re` printed `Resumed reading ... lag` warnings from the audio lavfi source, a test-rig artefact). Also 0 bytes of stdout (and 0 of stderr) for the exact product vector (`-rtsp_transport tcp -timeout 10000000 -fflags +genpts+discardcorrupt -i rtsp://127.0.0.1:18554/t -map 0:v:0 -map 0:a:0? -c copy -f segment -segment_time 7 ...`) against a throwaway MediaMTX with win8: 4 files `Cam_20261017_172154+0300.mkv`, `..57`, `..204`, `..211`, durations 6.4, 7.007, 7.047, 1.7 s.

**Conclusion.**
1. Names, boundaries and `%z` are the same on Windows and Linux for ffmpeg 8.x, and for 4.2.7 as far as the segment muxer goes. Name pattern `<Safe>_%Y%m%d_%H%M%S%z.mkv` renders as `+0300`, `+0530`, `-0230`, `+0000` on both.
2. **U2 answered: `-segment_atclocktime 1` aligns to multiples of `SegmentSeconds` counted from LOCAL midnight (local seconds-of-day), not UTC, not epoch.** At 10 s and 900 s every zone coincides; at 7 s (and any N that does not divide the offset) the cuts are on local-midnight multiples: in +03:00 they fall on epoch seconds congruent to 6 mod 7. The alignment is computed once at start from the local time of day; it is not re-derived at midnight or across a DST step (not tested: no clock changes allowed; this is from the lack of any re-derivation in the observed behaviour only, so treat as unverified for midnight/DST).
3. **V8 answered: Linux ffmpeg honours `TZ` for strftime and `%z`** (all four zones, both versions). **The Windows build is not "ignores TZ"**: with `TZ` unset it uses the system zone, with a POSIX-form `TZ` (`IST-5:30`) it honours it, with an IANA name it silently produces a wrong offset (`+0100`). So the comment at `cam.common.ps1:1968` is true for the Operator's normal environment (no `TZ`) and false when `TZ` is set.
4. **U7 answered: no stdout** in segment mode (0 bytes, 16 lavfi runs on three builds plus one RTSP run).
5. **V13 answered, and it is a trap for the Linux claim: `-timeout <microseconds>` exists only in ffmpeg 5.0 or later.** In 4.x the same flag means "listen timeout in seconds" and turns the RTSP demuxer into a server (immediate failure); the socket timeout there is `-stimeout`. An Ubuntu 20.04 `apt` ffmpeg (4.2.7) therefore cannot run the product's vector.

**What it changes.**
- LLD 2.3 `ffmpeg`: the golden test of the vector stays byte for byte; add a startup capability check (run `ffmpeg -version`, parse the major version; if below 5 use `-stimeout`, or refuse with a clear message). Recommendation: refuse below 5.0 and say so in `doctor`/self-test, so that behaviour is one on every machine.
- The Go supervisor must set `TZ` for the ffmpeg child deliberately: on Windows remove `TZ` from the child's environment (an inherited IANA `TZ` yields wrong offsets), on Linux leave it inherited so ffmpeg and Go's `time.Local` agree. Add this to LLD 2.3 as a rule.
- The strict-pattern rule and the pruner parse the offset from the name, so nothing else changes. The pruner/status/UI must not assume that cut times are multiples of `SegmentSeconds` in UTC; the default 900 s is safe in all zones except those with a non-quarter-hour offset (none in use).
- IR 1.4 `-segment_atclocktime` row: replace "[UNKNOWN] local or UTC" by "local-midnight multiples (measured)". U2, U7, V8, V13 can be closed. HLD risk table: add "ffmpeg < 5.0".

**Confidence.** High for names, alignment (8 cases x 3 builds, consistent), stdout and `-timeout` semantics. Medium for the Linux RTSP happy path: WSL cannot reach a Windows listener (Windows firewall drops the connection from WSL, so Linux ffmpeg never talked to MediaMTX), so Linux `-timeout` was tested against a silent TCP server and an unreachable host only, not a live stream. Untested: midnight, DST step, real camera.

## SP1b - credentials off the command line

**Question.** Can the direct-input camera credential reach ffmpeg's RTSP demuxer without appearing in the process command line (todo [90])?

**Method.** `ffmpeg -h demuxer=rtsp` on win8 and lin42: the RTSP demuxer has no `user`/`password`/`auth` option in either (options: `rtsp_transport`, `rtsp_flags`, `timeout`, `user_agent`, TLS options...). Then the candidates were run:
- Windows: a throwaway MediaMTX (copy of `C:\mediamtx\mediamtx.exe`, own config, `127.0.0.1:18554`, internal auth, only user `fakeuser`/`fakepass`, a lavfi publisher with those credentials), `tests/holocron/sp1b-windows.ps1`; command line read with `Get-CimInstance Win32_Process` 1.5 s after start, stderr captured.
- Linux (lin8): WSL cannot reach any Windows listener (firewall), so a stub RTSP server (`spikes/sp1b`, Go: answers OPTIONS, challenges the first DESCRIBE with Basic auth, logs whether the retry carried the right credential, never the password) on `127.0.0.1:18555`; command line read from `/proc/<pid>/cmdline`; `tests/holocron/sp1b-linux.sh`.

**Raw results.**

| Method | Windows (win8 + MediaMTX) | Linux (lin8 + stub) |
|---|---|---|
| M0 `-i rtsp://fakeuser:fakepass@host/t` (baseline) | records 4.096 s; **password in Win32_Process command line** | authenticates; **password in `/proc/<pid>/cmdline`** |
| M1 `-/i file` (value loaded from file) | `Unrecognized option '/i'` | same |
| M2 `-f concat -i list.txt` with `file 'rtsp://u:p@host/t'`, `-rtsp_transport tcp` outside | 401 not hit, but SETUP over UDP fails (461) and `Option rtsp_transport not found`: the concat demuxer does not hand input options to the inner RTSP open | authenticates (stub), no password in cmdline |
| **M2b `-f concat -safe 0 -protocol_whitelist file,rtsp,tcp,rtp,udp -i list.ffconcat`** with list `ffconcat version 1.0` / `file 'rtsp://u:p@host/t'` / `option rtsp_transport tcp` / `option timeout 10000000` | **records 4.096 s of h264+pcm_alaw; password absent from the command line (only the list path)**; also with the product vector: `-fflags +genpts+discardcorrupt ... -c copy -f segment -segment_time 7 ...` gives `Cam_20261017_172626+0300`, `..30`, `..37`, `..44` (durations 7.8, 7.03, 7.07, 0.4 s), stdout and stderr 0 bytes | authenticates (stub: `user_ok=true pass_ok=true`), password absent from cmdline; `option timeout` reaches the socket (`tcp ...?timeout=3000000` in the log) |
| M3 environment (`RTSP_USER`/`RTSP_PASSWORD`/`USER`/`PASSWORD`) | 401, no auth | 401, no auth |
| M4 `~/.netrc` | not run (no netrc support in the Windows build either) | 401, no auth |
| M5 `-i pipe:0` carrying the URL on stdin | n/a | `Invalid data found when processing input` (stdin is media bytes, not a URL) |
| no credential at all | 401 | 401 |

Side findings: (a) ffmpeg prints the full credentialed URL in its own error text when it fails to open the input (`Impossible to open 'rtsp://fakeuser:fakepass@...'`, and `Error opening input file rtsp://user:pass@...` in M0), so the existing redactor (`scheme://***:***@`) must stay on the stderr path. (b) On Windows the list file stays open (share-locked) for the life of the ffmpeg process: `Remove-Item` while it runs fails ("being used by another process"); delete it after the process has exited. On Linux unlinking an open file is always allowed.

**Conclusion.** A working method exists: **an ffconcat list file (M2b)**. The credentialed URL and the two input options go into a file written by the supervisor immediately before each start, `-f concat -safe 0 -protocol_whitelist file,rtsp,tcp,rtp,udp -i <list>` replaces `-rtsp_transport tcp -timeout N -i <url>` on the command line (the transport and timeout move into the list as `option` lines; the other input flags, `-fflags +genpts+discardcorrupt`, stay on the command line and still apply). The file is a plaintext secret on disk: it needs the same protection as any credential file (owner-only: mode 0600 on Linux/macOS, an owner-only ACL on Windows), must live under `_state` or a per-process runtime directory, and be deleted when the child exits (Windows: not before, the child holds it open). It removes the password from the process list on both OSes; it does not remove it from the supervisor's memory or from ffmpeg's error text. No other route (environment, netrc, stdin, `-/i`, demuxer options) works.
Not tested: reconnect behaviour of the concat wrapper over a long run (a long-lived live source through concat), a camera that uses Digest (MediaMTX/the stub offered Basic; ffmpeg handles Digest in the same code path but this was not exercised), a password containing `'` or `\` (the ffconcat quoting rules apply: `'` needs `'\''` escaping), and a camera password with `@`/`:` (the product already does not URL-escape these; unchanged).

**What it changes.** HLD/LLD: the direct path may stay available behind the existing switch but with the list-file method as the only credentialed mode; the "direct path disabled by default" fallback is no longer the only answer. The `ffmpeg` package needs a second argument-builder variant (concat wrapper) with its own golden test, and a `secrets`-backed writer for the list file. Recommendation: keep relay-only as the default (plan OS4) and implement the list-file variant only if a direct input stays in scope; it is a small addition, not a design change. Todo [90] can be closed as "mitigated by M2b", with the residual ffmpeg-error-text redaction.

**Confidence.** High that M2b authenticates and hides the password on Windows against a real MediaMTX (full recording) and on Linux against the stub (credential delivery only; no media). High that the other routes do not exist in these builds (win8, lin8; option lists and runs). Medium on long-run stability (untested).

## SP4 - "the open segment" by name and recency, NTFS and ext4

**Question.** Does "the newest file whose name matches the strict segment pattern, with mtime within `SegmentSeconds`+60 s" identify the file ffmpeg holds open (HLD decision 1)? During a recording, after a hard kill, across a camera drop/restart, under a clock step? Do size/mtime of an open file update in directory listings on NTFS (IR 10 item 3)? Does file creation time exist on ext4?

**Method.** `spikes/sp4` (Go): `pick` implements the rule (strict pattern `^<Safe>_\d{8}_\d{6}[+-]\d{4}\.mkv$`; newest = greatest mtime, ties by greatest name; accepted only if `now - mtime <= SegmentSeconds+60 s`; a name-sort-last candidate is reported beside it); `watch` polls every 2 s and, as ground truth, tests whether the picked file is held open (Windows: a share-none `CreateFile` fails with a sharing violation; Linux: scan of `/proc/*/fd`), and prints size/age from the directory listing (`os.ReadDir` entry info, which on Windows is the cached `FindNextFile`/`FILE_ID_BOTH_DIR_INFO` data, `dir_windows.go:226`) next to `os.Stat`. `sim` builds directories with set mtimes. Scenario (`tests/holocron/sp4-run.sh`): lavfi source with the product's segment flags at 5 s, ffmpeg A started, hard-killed after 20 s (Linux `kill -9`, Windows `Stop-Process -Force` = TerminateProcess), 10 s gap, ffmpeg B started (the restart after a camera drop), hard-killed after 20 s, watch continues for 65+ s. NTFS: `C:\Windows\Temp\holocron-sp\sp4` with win8; ext4: WSL root filesystem with lin8. Growth check (`sp4-growth.sh`): 30 s segments, 50 s recording, poll every 3 s. Segment length for the age window was set to 5 (window 65 s).

**Raw results.**

| Phase | ext4 (lin8) | NTFS (win8) |
|---|---|---|
| ffmpeg running | pick = file held open at 17 of 19 polls while ffmpeg ran; the 2 misses are the instant a 5 s segment closed before the next file existed (pick = the just-closed newest file) | pick = file held open at 21 of 22 polls while ffmpeg ran (the miss is the poll at the instant of the kill) |
| first poll after the restart (gap 10 s, A killed 17:29:02, B started 17:29:12) | pick moved to the new file within 0.7 s of its creation | pick moved to the new file within 1.1 s |
| after hard kill | pick stays on the last file, `heldOpen=false`, age grows 2.2 s ... 63.3 s | same, age 4.8 ... 64.2 s |
| ageing out | `pick=""` from the poll at age >= 65 s (17:30:36 for a kill at 17:29:32) | `pick=""` at age >= 65 s (17:33:07 for a kill at 17:32:03) |
| size of the last file after a hard kill | 0 bytes (the first Matroska cluster was never written) | 0 bytes |
| open file size/mtime, 30 s segments | grow in steps of 262144 bytes every 3-9 s; mtime age at a poll 0.8-5.9 s | identical pattern; **directory-listing values equal `os.Stat` values at every poll** (sizes 262144, 524288, 786432, ... with ages 0.6-5.8 s) |
| creation time | exists (`statx` btime returned, equal to the time the file was created); coreutils 8.30 `stat --format=%w` prints `-` on this Ubuntu even though the kernel and ext4 report it, so the shell check understates it | exists, equal to the time in the file name (+/- 1 s) |
| clock step simulated with set mtimes (`sp4 sim`, window for 900 s = 960 s) | forward 1 h: `pick=""`; back 1 h (mtimes 1 h in the future): pick = newest file; DST fall-back (`035000+0300`, `035500+0300`, then `030000+0200`): rule picks the `+0200` file (newest by mtime), the name-sort-last would pick the wrong `035500+0300`; foreign names (`.mkv.tmp`, other camera prefix, lower-case prefix): never picked | identical on all five |

**Conclusion.**
1. The rule works on both filesystems: it picks the held-open file while recording, keeps picking the last file after a hard kill until it ages out at SegmentSeconds+60 s, and picks the new file within about 1 s of a restart. The only non-held pick is the instant at a boundary when the finished file is the newest and the next one does not exist yet, which is harmless (still the newest segment).
2. **Order by mtime with the name as tie-break, not by name**, because after a DST fall-back the name order is wrong for an hour (the `%z` suffix is what prevents collisions, not what orders them). Verified with the DST fixture.
3. **The age window must apply only to "is something being written" decisions (stall watchdog, down alert), not to the pruner's protection.** After a forward clock step of more than the window the rule returns nothing, which for the pruner would remove the protection of the open file; the PowerShell pruner protects the newest segment of each folder with no age test (IR 5, rule 1) and the Go planner should keep that. A backward step leaves mtimes in the future: treat a negative age as "within the window" and flag a clock anomaly (the existing `Get-CamPruneClockProblem` is the place).
4. **IR 10 item 3 (directory entries go stale on NTFS): not reproduced.** On this machine (Windows 11 22621, local NTFS C:) the directory-listing size and mtime of a file ffmpeg is writing tracked `os.Stat` exactly. So `Get-CamLiveFileLength` is not needed for the Go port on C:. Not tested: the `E:` archive volume (writing there is forbidden for this spike) and network shares; if the archive can sit on other filesystems, keep one `os.Stat` per decision, not a cached listing entry.
5. An open Matroska segment shows size 0 until the first buffer is flushed (about 3-9 s at this test's ~0.3 Mbit/s; the product's segments are 900 s so only the first seconds). mtime of the open file lags real time by up to the flush interval; for a very low-bitrate camera the lag is larger (256 KB per step), so a stall watchdog built on mtime must use a threshold well above that (the product's current threshold is a multiple of the segment length; keep it).
6. File creation time works on NTFS and (via `statx`) on ext4 but Go's standard library does not expose it on Linux and other filesystems may not report it; the mtime rule above needs neither.

**What it changes.** HLD decision 1 stands with two amendments: ordering is mtime-then-name, and the recency window belongs to the watchdog/alert consumers only (the pruner always protects the newest strict-pattern file). Drop `Get-CamLiveFileLength` from the port (IR 10 item 3). The strict pattern is matched case-sensitively here; the PowerShell code is case-insensitive (IR 2.3), so on Windows (case-insensitive filesystem) match case-insensitively, on Linux decide explicitly (recommendation: case-sensitive, ffmpeg writes the prefix exactly as configured).

**Limits and confidence.** High for the recording / hard-kill / restart / age-out behaviour (two filesystems, one run each, consistent). The system clock was not changed; clock steps and the DST fall-back were simulated only by setting file mtimes and names, which proves the rule's arithmetic and ordering, not ffmpeg's behaviour during a real step (ffmpeg's `-segment_atclocktime` clock reading across a step is untested). Not tested: segment length 900 s (65 s window run only at 5 s and 30 s for the growth), `E:` volume, a real camera drop (simulated by killing and restarting ffmpeg), exFAT/network/overlay filesystems.

## SP6 - atomic replace of the status file with a concurrent reader (Windows)

**Question.** Writing `status.json` by temp file + rename while readers poll: does Go's Windows file open allow the writer's rename-over (FILE_SHARE_DELETE)? What flags and retries do the Go reader and writer need (IS 1.1 "Go reader")? What happens on a `\\wsl.localhost` path?

**Method.** `spikes/sp6` (Go, cross-compiled `GOOS=windows`, run from WSL interop with the scratch folder `C:\Windows\Temp\holocron-sp\sp6`). Writer: an 8 KB indented JSON document (`Seq`, timestamp, padding) written to `status.json.tmp` and renamed over `status.json` every 100 ms for 10-12 s; counts rename failures by Win32 error. Reader: tight loop (about 10 000 reads/s) or open-hold-close with a 2 s hold; counts good reads, invalid/partial JSON, sequence regressions and errors. Readers: Go goroutine in the writer process; a second Go process (`-open go` = plain `os.Open`, `-open sd` = `CreateFile` with `FILE_SHARE_READ|WRITE|DELETE`); Windows PowerShell 5.1 `Get-Content -Raw`, `[IO.File]::OpenRead` (FileShare.Read), `FileStream` with `FileShare.ReadWrite` and with `ReadWrite|Delete` (`tests/holocron/sp6-reader.ps1`). Three rename implementations in the writer: `mfe` = Go's `os.Rename` (MoveFileEx with REPLACE_EXISTING, read from `internal/syscall/windows/syscall_windows.go:357`), `posix` = `SetFileInformationByHandle(FileRenameInfoEx, REPLACE_IF_EXISTS | POSIX_SEMANTICS)`, `replacefile` = `ReplaceFileW`. Matrices: `tests/holocron/sp6-run.sh`, `sp6-run2.sh`, `sp6-run3.sh`. The Go toolchain's source shows `os.Open` on Windows uses share mode `FILE_SHARE_READ|FILE_SHARE_WRITE` only (`syscall/syscall_windows.go:395`).

**Raw results** (rename failures / writer cycles; reader errors; 0 partial or invalid reads and 0 sequence regressions in every run except the ReplaceFileW PowerShell run):

| Writer | Reader | Writer failed | Reader problems |
|---|---|---|---|
| mfe | Go goroutine, tight, `os.Open` | 42/65 ACCESS_DENIED | 19 NOT_FOUND, 56 SHARING_VIOLATION of 16 683 reads |
| mfe | Go process, tight, `os.Open` | 30/74 ACCESS_DENIED | 4 SHARING_VIOLATION |
| mfe | Go process, tight, **FILE_SHARE_DELETE** | **28/73 ACCESS_DENIED** | none |
| mfe | PS `Get-Content` tight / `OpenRead` tight / FileStream RW tight / FileStream RW+Delete tight | 5/105, 21/89, 22/91, 21/90 | none for RW+Delete; 14, 38, 18 IOException for the others |
| mfe | Go hold 2 s (`os.Open` or share-delete), PS RW hold 2 s, PS RW+Delete hold 2 s, PS `OpenRead` hold 2 s | 48/67, 48/68, 47/70, 47/71, 44/71 | none |
| mfe with 10 or 30 retries of 100 ms | Go hold 2 s | 89 and 94 failed attempts; a cycle succeeds only when the reader has let go | none |
| mfe, no reader | - | 0/96 | - |
| **posix** | Go tight / hold 2 s, **share-delete** | **0/78, 0/94** | **none, no NOT_FOUND** (9 943 reads) |
| posix | PS FileStream RW+Delete hold 2 s | 0/94 | none |
| posix | Go tight / hold 2 s, plain `os.Open` | 28/61, 44/60 SHARING_VIOLATION | 10 SHARING_VIOLATION |
| posix | PS `Get-Content` tight (RW, no Delete) | 9/81 SHARING_VIOLATION | 2 IOException |
| posix, no reader | - | 0/80 | - |
| ReplaceFileW | Go tight share-delete | 0/64 | **155 NOT_FOUND + 562 SHARING_VIOLATION**: not atomic for readers |
| ReplaceFileW | PS `Get-Content` tight | 4/71 | 310 IOException and 25 invalid/empty reads |
| mfe, writer only, `\\wsl.localhost\Ubuntu-20.04\...` (9P) | - | 0/41 | - |
| mfe on the 9P share with a tight Go reader / with a 2 s holder | - | 12/39, 24/35 ACCESS_DENIED | 13 SHARING_VIOLATION |
| posix on the 9P share | share-delete Go reader | 17/17 `ERROR_INVALID_PARAMETER` (87) | none |
| ReplaceFileW on the 9P share | share-delete Go reader | 18/18 `ERROR_NOT_SUPPORTED` (50) | none |

**Conclusion.**
1. **Go's `os.Open` does not allow the writer to rename over the file, and neither does opening with FILE_SHARE_DELETE if the writer uses Go's `os.Rename`.** `os.Rename` on Windows is `MoveFileEx(REPLACE_EXISTING)`; any open handle on the target makes it fail with `ACCESS_DENIED` (5) whatever the reader's share flags (rows "mfe + share-delete"). The IS 1.1 UNVERIFIED about `FILE_SHARE_DELETE` is answered: the flag alone is not enough.
2. **What works: a rename with POSIX semantics** (`FileRenameInfoEx`, flags `REPLACE_IF_EXISTS|POSIX_SEMANTICS`, Windows 10 1607+; this machine is 22621) **together with readers that open with `FILE_SHARE_DELETE`.** 0 failures in 266 cycles against tight and 2 s-holding readers, and no reader ever saw a missing or partial file. A reader without FILE_SHARE_DELETE (Go `os.Open`, PowerShell `Get-Content`, `[IO.File]::OpenRead`) blocks even this writer with SHARING_VIOLATION (32).
3. Temp file + rename never produced a partial or invalid read, with any writer implementation except ReplaceFileW (which opens a gap in which the file is missing: do not use it).
4. `\\wsl.localhost` (9P): plain `os.Rename` works when nobody has the target open (0/41) and fails the same way as on NTFS when a reader has it open; the POSIX-semantics rename and ReplaceFileW are not supported there (errors 87 and 50). So the writer must fall back to `os.Rename` on those two errors, as the PowerShell code already does for the same reason.
5. A reader holding the file for 2 s costs the writer every cycle that falls inside the hold (47/70). Retries of 10 x 100 ms (the PowerShell policy) rescue almost none of them against a 2 s holder. The status writer must therefore treat a failed replace as "skip this cycle" (the next one is 5 s later), never block, and count consecutive failures.

**Rules for `internal/status` on Windows.**
- Reader: open with `syscall.CreateFile(GENERIC_READ, FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE, OPEN_EXISTING)` and wrap with `os.NewFile` (as `spikes/sp6/open_windows.go`); read fully, close at once (never hold across a poll). On `SHARING_VIOLATION`/`NOT_FOUND` retry 5 x 80 ms as today (cheap insurance for the PowerShell producer, which still replaces with MoveFileEx). Strip a UTF-8 BOM. `camui`'s `os.ReadFile` must change to this open, otherwise it blocks the Go writer.
- Writer: temp file in the same directory, write, `Sync`, close; then replace with `FileRenameInfoEx(REPLACE_IF_EXISTS|POSIX_SEMANTICS)` via `SetFileInformationByHandle` on a handle opened with `DELETE` access and full sharing (`spikes/sp6/rename_windows.go:renamePosix` is a working 40-line implementation, standard library only); on error 87 or 50 (9P, some network shares) or an API-not-available error fall back to `os.Rename`; on SHARING_VIOLATION/ACCESS_DENIED retry a few times quickly, then give up for this cycle and log at debug. On Linux and macOS plain `os.Rename` is atomic and readers need nothing.
- During the strangler migration the PowerShell reader (already `FileShare.ReadWrite|Delete`) is compatible with the POSIX-semantics writer (0/94 failures against it), and the PowerShell writer (MoveFileEx) is compatible with the Go reader opened as above only while the Go reader does not hold the file (a polling reader at 1-5 Hz holds it for microseconds).

**What it changes.** LLD 2.5: replace "readers open with share-delete where available and retry 5 x 80 ms as today" by the exact rules above; add "writer uses POSIX-semantics rename with MoveFileEx fallback" to `osx` (one Windows file, one portable `os.Rename` file). HLD decision 2 is settled as: share-delete readers plus POSIX-semantics rename, with skip-on-failure. The single-instance lock file is unaffected.

**Confidence.** High for the failure/success pattern (each cell is a 7-12 s run of 60-100 cycles, one run per cell; the pattern is binary and consistent across ten cells). Untested: Windows versions older than 10 1607, a reader on another machine over SMB, antivirus filter drivers, FAT/exFAT volumes, `E:` (not written to), a long soak.

## SP7 - can the PowerShell pruner be driven as a dry run over a synthetic tree?

**Question.** Is there a way to run `Invoke-CamPrune` over a throwaway tree, record its decisions without deleting anything real, and is a differential harness against a Go planner practical?

**Method.** Read `Invoke-CamPrune` (`cam.recorder.ps1:3030-3391`), `Get-CamPruneRefusal`, `Get-CamPruneClockProblem`, and the prune checks in `tests/cam.tests.ps1` (`prunematrix` 2994, `pruneroots` 3107, `pruneage` 3260, `prunewatermark` 3324, `pruneclock`, `prunebreaker`, `pruneblocked`, `pruneconfig`, `pruneowner`, `prunelive`; plus `pruneloop` which needs a synthetic camera). Then ran one small tree through the same mechanism as the checks, in a new file that edits nothing: `tests/holocron/sp7-probe.ps1`, run on a COPY of `cam.common.ps1`, `cam.recorder.ps1` and `tests/cam.testlib.ps1` placed in `C:\Windows\Temp\holocron-sp\sp7repo`, with `TEMP` pointed at `C:\Windows\Temp\holocron-sp\sp7tmp`:
`powershell -NoProfile -ExecutionPolicy Bypass -File sp7-probe.ps1 -Repo C:\Windows\Temp\holocron-sp\sp7repo -Temp C:\Windows\Temp\holocron-sp\sp7tmp`.
Tree: two cameras, `Cam_A` with segments 40/30/20/15 h old plus a newest one minute old and a foreign `notes.txt`; `Cam_B` 38/28/18/14 h plus a newest; each segment 16 MB; `MaxRetentionDays=1`, `MaxAgePruneFraction=1.0`, `ReserveGB=10.085`, `PruneHysteresisGB=0.02`; free space faked by a `Get-CamFreeSpaceGB` defined in the caller's scope that returns 10 GB plus the bytes deleted so far.

**Mechanism (what the existing checks use).**
- Entry point: `Invoke-CamPrune -Config <hashtable> -SupervisorLog <path> [-State <hashtable>] [-DryRun]`. It is a plain function; the checks load it without running the recorder with `. (Get-CamTestRecorderFunctions -Name 'Invoke-CamPrune','Get-CamSegmentDirectories','Remove-CamEmptySegment','Write-CamHost')` (`tests/cam.testlib.ps1:1693`, AST extraction of named functions), i.e. dot-sourced into the check's scope. `cam.testlib.ps1` itself dot-sources `cam.common.ps1`, which holds the other functions the pruner calls.
- Inputs: a config hashtable (start from `Get-CamConfigDefault`, as `New-CamTestPruneRoot` does: `RootPath`, `ReserveGB`, `PruneHysteresisGB`, `MaxRetentionDays` (null = off), `MaxAgePruneFraction`, `LogMaxBytes`, `Container`, `SegmentSeconds`, `StallFactor`); an archive root holding the ownership marker (`Initialize-CamArchiveOwner -Config` BEFORE any foreign file is placed, otherwise the root is refused as "cannot claim" - a guard, shown by my first run); segment files named `<Safe>_yyyyMMdd_HHmmss+hhmm.mkv` with mtime AND creation time set (`New-CamTestSegment` + `Set-CamTestFileAge`); a supervisor log path; free space through an overriding `Get-CamFreeSpaceGB` in the calling scope; the clock through `Get-CamWallClock` (overridable the same way, not overridden here).
- Where `-DryRun` stands: it deletes nothing and returns the same decisions (`Deletions` entries start with `would delete` instead of `prune: deleted`). It estimates freed space as `Length / 1GB` per file, so a dry run needs realistically large files (or the free-space fake must use the same scale; I used 16 MB files and 0.0156 GB units); the real-run checks use 64 KB files and a unit-based fake instead.

**Raw result (one tree, dry run then real run in the sandbox).**
- `Candidates=8 Deleted=7 FreedBytes=117440512 Blocked=False Failed=0`; `Protected` = the two newest segments; one foreign-file line.
- Order dry = order real: `Cam_A 40h > Cam_B 38h > Cam_A 30h > Cam_B 28h` (age rule, "older than MaxRetentionDays=1") `> Cam_A 20h > Cam_B 18h > Cam_A 15h` (watermark, "free 10.06 GB below reserve 10.09 GB, target 10.11 GB"); `Cam_B 14h` and both newest kept.
- Stop line: `prune stop: age rule reached its cutoff 2026-10-16 17:42:53 after 4 deletion(s), stopping at Cam_A_20261016_214126+0300.mkv (written 2026-10-16 21:41:26); watermark reached its target 10.11 GB after 3 deletion(s), free 10.11 GB, stopping at ...`.
- The dry run changed no file (sha256 manifest diff: removed 0, changed 0, added 0); the real run removed exactly the seven files.
- Both runs also appended to the sandbox log (`prune: 1 foreign file(s) left alone in <root>\Cam_A`, the stop line; the real run adds one `prune: deleted ...` line per file and a summary). So `-DryRun` is not log-silent.
- The returned hashtable has 19 keys: `AgeBreaker, Blocked, Candidates, ClockRefusal, Deleted, Deletions, Failed, Foreign, FreedBytes, FreeGBAfter, FreeGBBefore, Marker, Message, Protected, Rejected, ReserveGB, RootRefusal, StopLine, TargetGB`.
- Sandbox removed afterwards (`Remove-CamTestTree`); nothing outside `C:\Windows\Temp\holocron-sp\sp7tmp` was touched.

**Conclusion.** Yes. The pruner can be driven over a synthetic tree in a sandbox under `TEMP`, and its decisions are captured three ways: the return hashtable (`Deletions` strings in order with reason, `Protected`, `Rejected`/`Foreign`, `Candidates`, `StopLine`, `Blocked`, `RootRefusal`, `ClockRefusal`, `AgeBreaker`), the log lines, and (real run in the sandbox) a before/after sha256 manifest. There is no structured deletion record; the existing checks parse the strings with `^(?:would delete|prune: deleted) (.+?) \(\d+ bytes` (`Get-CamDeletedPathList`, `cam.tests.ps1:2949`). A differential harness against a Go planner is practical for: age rule, watermark order and stopping point, protection of the newest segment of each folder, "strictly older than the newest of its own folder", foreign-file and refusal classification, the clock gate and the age circuit breaker. Two known semantic differences it will immediately expose, both deliberate: the PowerShell code picks each folder's newest segment by `CreationTimeUtc` while SP4 recommends mtime-then-name for the Go side, and name matching is case-insensitive in PowerShell.

**What would have to be added (new files only, nothing in `cam.tests.ps1`/`cam.testlib.ps1` edited).**
1. `tests/holocron/sp7-diff.ps1` (productised `sp7-probe.ps1`): takes a scenario JSON (cameras, per-file name, size, UTC mtime and creation time, config keys, fake free-space base in GB, optional fixed `Now`), builds the tree in a sandbox, runs `Invoke-CamPrune` dry and (optionally) real, and writes a decision JSON (ordered deletions with path, bytes, reason class `age`/`watermark`, protected list, foreign count, `Blocked`, `StopLine`, refusals). The `Deletions`-string parser lives here.
2. A `Get-CamWallClock` override in that script for a fixed `Now` (the existing checks generate files relative to the real clock instead), and a free-space fake scaled for the dry-run estimate (`Length/1GB`) or only real runs inside the sandbox (the sandbox deletes are harmless).
3. A scenario corpus (JSON) covering the ten prune checks' cases plus the DST fall-back, the clock-jump and breaker cases; the Go side reads the same JSON and compares its plan with the decision JSON, so the PowerShell run is the golden generator and can be done once per corpus change on Windows, with the decision files committed.
4. Nothing needed in the product scripts. The `-DryRun` switch, the marker requirement and the fake-free-space seam already exist.

**What it changes.** The differential-harness plan in the LLD (section 6) is confirmed workable; it needs only the new files above, and the golden outputs can be committed so Linux CI never needs PowerShell. Put the two intentional differences (creation time versus mtime; case folding) in the corpus as expected-diff entries.

**Confidence.** High that the mechanism works (one tree, both modes, outputs above). Not run: the ten shipped prune checks themselves, `pruneloop` (needs a live synthetic camera), `prunelive` (reads the real archive), unusual roots (junctions, drive roots).

## Process hygiene and run record

- Processes of the Operator (ffmpeg, MediaMTX, camui, PowerShell) were listed before and after; none of them was stopped or touched. Everything started here was killed by pid: throwaway MediaMTX copies (own config, loopback or one short run on a bridged address, ports 18554-18557 only), lavfi publisher, stub servers, sp4/sp6 programs.
- One incident worth recording: a first Windows SP4 scenario hung because the helper that started ffmpeg returned its pid on stdout while the child inherited the pipe; the stuck ffmpeg was killed by pid and the scenario rerun with a pid file (`tests/holocron/sp-start.ps1` now writes `<Out>.pid`).
- A throwaway MediaMTX was briefly bound to the Windows bridge address `192.168.1.2:18554` (to test whether WSL could reach it; the Windows firewall dropped the connection) and then restarted on `127.0.0.1:18554`. Its default MoQ listener on 8892/8893 appeared for a few seconds in the first start (outside the allowed port range) and was disabled with `moq: no` in the next config; the first instance was killed within a minute.
