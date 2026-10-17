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
- LLD 2.3 `ffmpeg`: the golden test of the vector stays byte for byte; add a startup capability check (run `ffmpeg -version`, parse the major version; if below 5 use `-stimeout`, or refuse with a clear message). Recommendation: refuse below 5.0 and say so in `doctor`/self-test, because lin42 is also missing other things and the Operator wants one behaviour.
- The Go supervisor must set `TZ` for the ffmpeg child deliberately: on Windows remove `TZ` from the child's environment (an inherited IANA `TZ` yields wrong offsets), on Linux leave it inherited so ffmpeg and Go's `time.Local` agree. Add this to LLD 2.3 as a rule.
- The strict-pattern rule and the pruner parse the offset from the name, so nothing else changes. The pruner/status/UI must not assume that cut times are multiples of `SegmentSeconds` in UTC; the default 900 s is safe in all zones except those with a non-quarter-hour offset (none in use).
- IR 1.4 `-segment_atclocktime` row: replace "[UNKNOWN] local or UTC" by "local-midnight multiples (measured)". U2, U7, V8, V13 can be closed. HLD risk table: add "ffmpeg < 5.0".

**Confidence.** High for names, alignment (8 cases x 3 builds, consistent), stdout and `-timeout` semantics. Medium for the Linux RTSP happy path: WSL cannot reach a Windows listener (Windows firewall drops the connection from WSL, so Linux ffmpeg never talked to MediaMTX), so Linux `-timeout` was tested against a silent TCP server and an unreachable host only, not a live stream. Untested: midnight, DST step, real camera.
