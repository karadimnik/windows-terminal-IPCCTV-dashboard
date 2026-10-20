# internal/ffmpeg

The recording ffmpeg: the exact argument vector, the version check, the child process with redacted output, the
credential-free direct input, and the empty-segment cleanup. Standard library plus `archive`, `clock`, `config`,
`logx` and `osx`. Port of `Get-CamFfmpegArgs`, `Start-CamChild` (process part) and `Remove-CamEmptySegment`
(LLD 2.3 and section 11; IR 1, 3.5-3.8, 8).

## API

```go
// arguments (an argv, no quoting layer)
type Inputs struct { URL, RtspTransport string; TimeoutMicroseconds int64; SegmentSeconds int
                     Profile config.ContainerProfile; OutputPattern string; AllowCredentialArgs bool }
func BuildArgs(Inputs) ([]string, error)              // == testdata/golden/ffmpeg/*.json (11 variants); a URL with a credential
                                                      // is refused (ErrCredentialArgs) unless AllowCredentialArgs
func BuildArgsDirect(in Inputs, listPath string) ([]string, error) // -f concat -safe 0 -protocol_whitelist ... -i <list>, same tail
func RelayURL(base, path string) string               // base without trailing '/', then /path
func SegmentPattern(root, safe, ext string) string    // <root>/<safe>/<safe>_%Y%m%d_%H%M%S%z<ext>, every '%' doubled (see below)

// direct input without the password on the command line (SP1b)
func WriteInputList(p osx.Platform, dir, url string, timeoutUS int64, transport string) (path string, cleanup func(), err error)
func SweepInputLists(dir string, maxAge time.Duration) int   // ffin-*.ffconcat regular files older than maxAge (WriteInputList sweeps 1 h)

// build facts
func CheckVersion(path string) (Version, error)       // runs `<path> -version`; refuses < 5.0
func ParseVersion(output string) (Version, error)
func ChildEnv(base []string, goos string) []string    // removes TZ on windows only

// the child
type Starter interface { Start(StartConfig) (Process, error) }   // default ExecStarter{}; the supervisor supplies a job-object / process-group one
type Process interface { Pid() int; Wait() (code int, err error); Kill() error }
type Spec struct { Name, Exe string; Args []string; Dir string; Env []string; Starter Starter; Redactor *logx.Redactor
                   Log *slog.Logger; TailLines int; Clock clock.Clock; Cleanup func() }
func Start(ctx context.Context, spec Spec) (*Child, error)
func (*Child) Pid() int; StartedAt() time.Time; Done() <-chan struct{}; Wait() ExitInfo; Kill() error
type ExitInfo struct { Code int; RanFor time.Duration; LastLines []string; Err error }
func (ExitInfo) Message() string                      // "ffmpeg exited with code N after Ns"

// cleanup after an exit
func RemoveEmptySegment(dir, container string, o EmptyOptions) EmptyResult
```

## Rules and decisions

- **Argument order** is IR 1.1 verbatim; the golden test compares the whole array for every variant and a negative
  control proves a one-byte change is caught. The direct-input goldens contain the fake credential in the URL, so the
  golden test sets `AllowCredentialArgs`; the recorder must use `BuildArgsDirect`.
- **Minimum ffmpeg 5.0** (`MinMajor`): before it `-timeout` is the RTSP listen timeout and turns the demuxer into a server.
  Release builds (`7.1.1-essentials_build-www.gyan.dev`, `4.2.7-0ubuntu0.1`, `n6.1.1`) are compared by major version.
  Development builds are accepted with `Version.Warning` set (log it): `N-127579-g5cb9fcd211-20261016`,
  `2026-07-06-git-<hash>-...`, `git-YYYY-MM-DD-<hash>`; a DATED one older than the 5.0 release (2022-01-17) is refused.
  Anything else (`UNKNOWN`, `HEAD-x`, no `ffmpeg version` line) is an error. Real first lines are in `version_test.go`.
- **`TZ`**: removed from the child's environment on Windows (the Windows build honours a POSIX `TZ` but silently gives a
  wrong offset for IANA names), inherited elsewhere. `Spec.Env == nil` means `ChildEnv(os.Environ(), runtime.GOOS)`.
- **The child**: stdin closed, `CreateNoWindow` on Windows, stderr and stdout read line by line (split on `\r` or `\n`,
  blank lines dropped, invalid UTF-8 replaced, a line over 8192 bytes cut). Every line passes `logx.Redactor.String`
  (`scheme://user:pass@` becomes `scheme://***:***@`, plus registered secrets) BEFORE it reaches the log or the tail; the
  log is a `*slog.Logger` from `logx` (stderr at INFO, stdout at DEBUG, attrs `camera`, `stream`). The tail keeps the last
  `TailLines` (default 20; IR gives no number) redacted lines for `ExitInfo.LastLines`. `Wait` returns after the process has
  exited AND the output was drained, but at most `OutputDrain` (2 s, os/exec `WaitDelay`) after the exit. `Kill` is always
  hard. `ctx` cancel kills; nothing else here manages lifetime (backoff, restarts, stall watching, job objects are the
  supervisor's). `Spec.Cleanup` runs exactly once after the exit (or at once if the start fails): use it for the input-list
  `cleanup` and for `RemoveEmptySegment`.
- **Exit codes** on Windows are signed 32-bit like .NET's `Process.ExitCode` (`0xCFEF...` reads negative), so the message text
  is what the PowerShell product would print. A killed child reads 1 on Windows and -1 (signal) on Linux.
- **Direct input** (`WriteInputList`): the list is `ffconcat version 1.0`, `file '<url>'` (a `'` in the URL becomes `'\''`,
  checked against the real ffmpeg), `option rtsp_transport T`, `option timeout N`. The file is `ffin-<16 hex>.ffconcat`,
  created `O_EXCL` 0600 and restricted with `Platform.SetOwnerOnly` BEFORE the URL is written (failure removes it: fail
  closed). Delete it after the child has exited (Windows keeps it share-locked while ffmpeg runs); stale ones older than 1 h
  are swept on every `WriteInputList`. Honest limits: the password is in this process's memory and on disk until
  cleanup, and **ffmpeg still prints the credentialed URL in its own error text** (measured: `Impossible to open
  'rtsp://user:pass@...'`), so the redaction on stderr is mandatory and tested with a positive control.
- **`%` in names (measured with a real ffmpeg)**: `-strftime 1` expands the WHOLE output path, directories included
  (4.2.7: a folder `Cam%Y` was written as `Cam2026`). `SegmentPattern` therefore doubles `%` in the root, the folder and the
  file name; `archive.FormatSegmentPattern` already doubles it in the file name. With the doubling ffmpeg 8.x and the
  master build write `Cam%Y_20261020_....mkv` in folder `Cam%Y`. The PowerShell product has this defect for a root or a camera
  folder containing `%` (it would create the expanded folder, or fail when it does not exist).
- **First segment after a (re)connect over RTSP**: it contains media starting 2 to 3 s before the clock instant in its name
  (SP1 and W5); the name stays correct for archive and pruner purposes.
- **`RemoveEmptySegment`** removes every regular file of zero length in the camera folder that passes the pruner's file gate:
  not a link or reparse point, the folder is a real directory directly under `Root` (when given) and does not start with `_`,
  and the name is a strict segment name of the folder's camera in the container (`archive.Matcher.IsSegmentOf`). Anything
  else is counted as foreign and left. `RootRefusal` is the root gate (`archive.VerifyRoot(...)`): a refusal deletes nothing.
  Call it after the child has exited, never while it runs. The `Messages` are the PowerShell log lines.

## Tests

`go test ./internal/ffmpeg` is fast and offline (goldens, version table, TZ table, list-file and password-absent test with
positive control, child tests with a scripted `Starter` and with the test binary as a fake ffmpeg, cleanup table tests).

The real-ffmpeg check is opt-in (`HOLOCRON_IT_FFMPEG`, optional `HOLOCRON_IT_FFPROBE`, `HOLOCRON_IT_MEDIAMTX`,
`HOLOCRON_IT_TMP`) and skips with a message otherwise or when the ffmpeg is older than 5.0. It records lavfi
(`testsrc2` + `sine`, H.264 + `pcm_alaw`) with 10 s segments for the names `Front`, `Front Door` and `Cam%Y` under a root
with a space, checks every file with `archive.IsSegmentOf`/`ParseSegment`, ffprobe streams and durations, a hard kill
with `Scan` and `RemoveEmptySegment`, the ffconcat quoting, and (with MediaMTX) the product vector over a real RTSP relay
and the direct input through the list file against a second MediaMTX that requires a login (command lines read from the
live processes). MediaMTX is copied to the test's temp folder, uses loopback ports 18600-18639 only (18600 relay, 18601
camera) with every unneeded listener off (`api`, `metrics`, `pprof`, `playback`, `rtmp`, `hls`, `webrtc`, `srt`, `moq`
all `no`, RTSP over TCP only; on Windows the listener set is asserted per pid with `netstat -ano`), and is killed by pid.

    # Windows build, run from a Windows-local folder
    GOOS=windows go test -c -o /mnt/c/Windows/Temp/holocron-w5/ffmpeg.test.exe ./internal/ffmpeg
    cd /mnt/c/Windows/Temp/holocron-w5 && WSLENV=HOLOCRON_IT_FFMPEG:HOLOCRON_IT_FFPROBE:HOLOCRON_IT_MEDIAMTX:HOLOCRON_IT_TMP \
      HOLOCRON_IT_FFMPEG='C:\ffmpeg\bin\ffmpeg.exe' HOLOCRON_IT_FFPROBE='C:\ffmpeg\bin\ffprobe.exe' \
      HOLOCRON_IT_MEDIAMTX='C:\mediamtx\mediamtx.exe' HOLOCRON_IT_TMP='C:\Windows\Temp\holocron-w5\scratch' \
      ./ffmpeg.test.exe -test.v -test.run TestRealFfmpeg

    # Linux (needs ffmpeg >= 5.0; WSL cannot reach a Windows listener, so no MediaMTX shapes there)
    HOLOCRON_IT_FFMPEG=/path/to/ffmpeg go test -v -run TestRealFfmpeg ./internal/ffmpeg
