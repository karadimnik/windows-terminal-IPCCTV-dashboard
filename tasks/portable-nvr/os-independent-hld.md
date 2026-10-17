# OS-independent NVR: high-level design

Status: PROPOSAL for the Operator, 2026-10-17. Nothing here is built. Companion documents: `os-independent-lld.md` (the
implementation plan), `spec/inventory-recorder.md` and `spec/inventory-supervisor.md` (the source-cited behaviour of the
PowerShell system, which is this design's specification), `plan.md` (stages, status board, gap list) and `todo.md`.

Rules that bind this design (from the Operator): the original `windows-terminal-IPCCTV-dashboard` repository and its
Windows installation stay untouched and working; everything is built beside it in the portable project; nothing is removed;
no autostart of anything without being asked; feasibility tests only, no stress tests; the portable project never reaches `master`.

## 0. Hard architecture rules (Operator, 2026-10-17)

These bind every task in `os-independent-lld.md`. A change that breaks one is rejected in review.

**R1. One executable, one entry point, one exit point.** The system is a single program, `holocron` (`holocron.exe` on
Windows), and it is the only thing the Operator ever runs. Everything is a subcommand of it: starting and stopping the
whole system, configuring it, adding and removing cameras, serving the web app and the stream grid, managing users and
certificates, reading logs, and installing or uninstalling the autostart (a logon task, a Windows service, a systemd unit
or a launchd agent). No PowerShell scripts, no second executable and no helper program for any action. In particular:
- `holocron up` is the one way to start everything (host, supervisor, relay, live relay, web app) and `holocron down` is the
  one way to stop everything and prove it. The OS autostart entry that `holocron install` creates runs `holocron up` and
  nothing else.
- The internal roles (host, supervisor, web server) are hidden subcommands that `holocron` starts as its own child processes.
  They stay separate processes on purpose (a fault or an attack on the LAN-facing web page must not stop a recording), but
  the Operator never starts or stops them individually and never sees a second executable.
- Every action in the web app has the same command on the command line, and both call the same code path.
- The existing `camui.exe` and the PowerShell scripts remain only as the fallback system (D6) until cutover; the new system
  has nothing to do with them at run time.

**R2. Logging for every action, at a level the Operator controls.** Every action the system takes is logged with a level
(ERROR, WARN, INFO, DEBUG, TRACE), a component, an action name, who or what caused it, the target and the outcome. The level
is set per component, from configuration, from an environment variable or at run time with `holocron log level`, without a
restart. A test fails if any command, control-API handler or web handler can run without logging its start and its result
(`LLD 2.13`). Enabling DEBUG or TRACE never exposes a secret: redaction sits in the logging handler, below every level.

## 1. What "OS independent" means here

One Go codebase that records, supervises and serves the cameras on Windows, Linux and macOS, with the same observable
behaviour as the PowerShell system today, driven from the browser app that already exists. Concretely:

- ffmpeg and MediaMTX stay external programs (both ship for all three OSes). We do not write a muxer.
- The archive on disk is unchanged: one folder per camera, the same file names, so footage, the pruner's safety rules and
  the web app's Footage screen work with either implementation.
- `status.json` becomes a written, versioned contract. The web app, the CLI and any other tool read only that.
- The PowerShell system remains installable and working as a permanent fallback (decision D6). The two never write the same
  archive at the same time.

Non-goals: object detection or motion recording (todo [16]), cloud features, multi-site, an internet-facing service
(Stage 3, separately gated), and bundling ffmpeg or MediaMTX.

## 2. What the research established

1. The behaviour to reproduce is now written down: 1,359 lines, every claim cited to a source line, with 39 open or
   unverified items, at least 17 recorder-side behaviours that are Windows accidents (do not copy) and two top-10 lists of rewrite risks.
2. The supervisor's job is a set of 15 OS-neutral guarantees (G1 to G15 in `spec/inventory-supervisor.md` section 2.2): a
   deliberate close is never "repaired", an unexpected stop is restarted with a budget, one supervisor per archive, children die
   with their supervisor, an orderly stop is verified and orphan-free, start is honest. Six of the 15 have no proving check today
   (G5 children die with the supervisor, G8 one supervisor per archive, G9 stop proves itself, G10 honest start, G12 disarm
   when no window appears, G14 one action at a time), and G7 is only partly covered.
3. Three facts decide the architecture: (a) the Windows mechanisms (job objects, wardens, Windows Terminal windows) implement
   behaviours that are requirements; the mechanisms are not; (b) the "open file" signal the pruner, stall watchdog and down alert
   rely on is NTFS creation time, which Linux does not guarantee; (c) `status.json` has three read-modify-write writers and no lock.
4. Prior art agrees with the recorder choice. Frigate and Viseron record with one ffmpeg per camera; Moonfire NVR avoids
   ffmpeg but stores only H.264 and has no audio path we need; an earlier finding in this project showed MediaMTX's own recorder
   drops G.711 audio. Keeping ffmpeg's segment muxer is what keeps our audio.
5. Current load matters for design: complete 15-minute segments are 172 MB, 137 MB and 125 MB for three cameras (about 16, 13
   and 12 GB per day each), so six cameras write on the order of 60 to 70 GB per day on a drive with about 125 GB free. Retention is
   about two days. A shadow run must be sized with that in mind (section 7).

## 3. Architecture

```
   The Operator runs only:  holocron <command>             (up, down, status, cam, config, install, ...)

                +-------------------------------- one executable: holocron --------------------------------+
                |                                                                                          |
  autostart --> |  holocron up  (host role)   restarts the supervisor in place, budget 5 per 30 min,       |
  (task/unit)   |     |                       classifies stops, raises alerts 100/101/102/103              |
  or a terminal |     +-- supervisor role     reconciles cameras.json every 5 s, owns the children, prunes, |
                |     |    |                  writes status.json, serves the control API                  |
                |     |    +-- ffmpeg x N         one per recording camera (segment muxer)                |
                |     |    +-- mediamtx (relay)   serves the cameras once; recorders read it              |
                |     |    +-- mediamtx (live)    WebRTC for the browser, behind the login page           |
                |     +-- web role            login page, Live grid, Footage, Settings; reads status.json,  |
                |                             changes things only through the control API                |
                |                                                                                          |
                |  holocron <command>  (short-lived)   cam add|remove|set, config, user, tls, log level,   |
                |                                      install|uninstall, logs, doctor, down, status ...   |
                +------------------------------------------------------------------------------------------+

  state on disk:  <archive>/<camera>/*.mkv   <archive>/_state/status.json + control files   <archive>/_logs
                  config: cameras.json, holocron.config.json   secrets: OS keyring or encrypted file   auth: users.json, sessions.json
```

Why this shape:

- **Host and supervisor stay two processes.** Today's NvrHost restarts a crashed supervisor and decides "expected or not" from
  what the supervisor recorded, never from an exit code (G4). Doing the same in Go gives identical semantics on every OS
  and does not depend on each OS service manager's restart rules. The OS service manager only has to keep the host alive.
- **The web app stays a separate process.** A bug or an attack on the LAN-facing page must not be able to stop a recording.
  It stays stateless: it reads `status.json`, and it changes things only through the supervisor's control API.
- **One executable, hidden roles** (rule R1): `holocron` starts its own host, supervisor and web processes; they are internal
  subcommands, not things the Operator runs. One artifact per OS to install and sign.
- **Single writer for `status.json`.** The supervisor is the only writer. The three read-modify-write patchers of today (the
  window-close patch, the host's `unexpected` patch, the record toggle) become control-API calls or are written by the
  supervisor itself. This removes a class of races that PowerShell had to defend with retries and mutexes.

### Components

| Component | Responsibility | Replaces |
|---|---|---|
| `host` | Spawn the supervisor, restart on unexpected stop within the budget, never restart a requested, interrupted, window-closed stop or exit 20, raise 100/101/102 | `Start-CamNvrHost` |
| `supervisor` | Reconcile loop, child lifecycle with backoff and stall watchdog, relay-or-direct input choice, pruner, heartbeat and status writer, down alert, ordered stop | `cam.recorder.ps1 -Run` |
| `archive` | Safe names, segment names, scans, the pruner's rules and root guards | `Get-CamSafeName`, `Test-CamSegmentName`, `Invoke-CamPrune` |
| `ffmpeg` | Builds the argument vector (golden-tested), runs the child, drains and redacts stderr | `Get-CamFfmpegArguments`, `Start-CamChild` |
| `relay` | Generates both MediaMTX configs, env-var sources, control-API client, ready-set polling | `Get-CamRelayConfigText`, `Get-CamLiveConfigText` |
| `status` | Schema types, atomic write, readers, the one liveness verdict used by everyone | `Write-CamStatusFile`, `Get-CamSupervisorState` |
| `alerts` | Event model, dedupe and repeat rules, channel interface | `Send-CamAlert` |
| `secrets` | Camera credential store behind an interface | `camera_creds.xml` (DPAPI) |
| `osx` | Everything that differs per OS, behind build tags (section 5) | job objects, wardens, Windows Terminal, `schtasks` |
| `ctlapi` | Local control endpoint for stop, record toggle, reload, live state | `cam.ps1`/`cam.recorder.ps1` switches called by `camui` |
| `web` | The existing `camui` plus login, TLS, Live, Footage | unchanged in spirit |
| `cli` | The whole command surface of rule R1 (`up`, `down`, `status`, `cam`, `config`, `user`, `tls`, `install`, `logs`, ...), each command a thin call into the same code the web app uses | `cam.ps1`, `cam.recorder.ps1`, `camui` flags |
| `logx` | Levelled, structured, per-component logging with run-time level changes, redaction in the handler and an action contract (rule R2) | `Write-CamLog` |

## 4. Technology choices

Principle: standard library first. Every third-party dependency needs a one-line justification here, and the first one
needs the Operator's approval (decision D14). `camui` has zero dependencies today.

| Concern | Choice | Alternatives considered and why not |
|---|---|---|
| Language | Go 1.27 (already required by `camui`) | Rust (no gain; the existing web app and tests are Go), keeping PowerShell 7 (cross-platform but slow start, weak process control on Linux, no static binary) |
| Recording | ffmpeg segment muxer to Matroska, as today | MediaMTX recorder (drops G.711), a Go muxer (large, audio and timestamps risk), Moonfire-style own storage (new archive format; breaks "nothing is removed") |
| Camera fan-out and live | MediaMTX relay plus a second live instance, as today | go2rtc (comparable, no reason to switch while MediaMTX works and is already hardened here) |
| Process lifetime | Per-OS kernel mechanism plus a pid registry (section 5) | A pure user-space watchdog (cannot survive its own death) |
| Service install | A thin interface with three implementations; `kardianos/service` evaluated as the single dependency that provides Windows service, systemd and launchd install and run | Hand-written unit files and SCM calls (more code to maintain per OS) |
| Windows autostart | Default: a logon task for the current user (no password, same account as the archive and the web app); option: a real Windows service | A service only (needs the user's password or a service account, and cannot show a console in attached mode; Session 0 isolation) |
| Secrets | `secrets.Provider` with backends: OS keyring (`go-keyring` evaluated; `99designs/keyring` is reported abandoned), AES-256-GCM encrypted file with a key from an environment variable or an owner-only key file (headless Linux), plain environment (containers) | DPAPI file only (Windows-bound), plaintext file (rejected) |
| Alerts | Channel interface; first channels: log, webhook, ntfy-style HTTP push, desktop notification (Windows toast through the existing mechanism, `notify-send`, `osascript`), Windows event log and journald where present | Embedding a notification framework (`shoutrrr`) is possible later; webhook plus ntfy covers a phone with no dependency |
| Config | JSON with a JSON Schema, same keys and defaults as today (inventory A section 4 lists keep and drop) | TOML or YAML (no benefit; the web app edits JSON) |
| Logging | `log/slog` with levels ERROR, WARN, INFO, DEBUG and a TRACE level below DEBUG, per-component levels, run-time level change, structured fields (action, actor, target, result, duration, action id), an in-house rotating writer that cannot fail the caller (rotation at max size, one `.1` generation, once-per-episode notices), and redaction in the handler | A logging library (the never-throw and action-contract rules are specific) |
| Time | UTC for all stored and compared instants, monotonic for intervals, `%z` offsets in file names and persisted timestamps; the DST table from the PowerShell tests is ported as a test | Local-time arithmetic (the source of the DST defects fixed on 2026-10-15) |
| Filesystem | `os` plus per-OS free-space calls (`x/sys` or `syscall`); "the open segment" is defined by name and recency, not creation time (section 6) | NTFS creation time (Linux does not guarantee it) |
| Passwords (web login) | PBKDF2-HMAC-SHA256, 600,000 iterations, standard library; self-describing hash for a later move to Argon2id | Argon2id now (the stronger algorithm, but the first dependency) |
| Web | Existing Go `camui`, vanilla JS, login page, TLS with the local CA | A JS framework (not needed) |
| Tests | Go tests with table-driven rules, golden files, a fake-camera rig (ffmpeg lavfi into MediaMTX), a differential harness against the PowerShell code, CI on Windows and Linux | Re-expressing the 13,000-line PowerShell harness (it stays as the fallback's own tests) |

## 5. What differs per OS (the `osx` layer)

| Capability | Windows | Linux | macOS |
|---|---|---|---|
| Children die with the supervisor | Job object with kill-on-close, handle held for the supervisor's life (what `cam.supervisor.<hash>` does) | `Pdeathsig` with the starting goroutine locked to its OS thread, children in their own process group | No kernel guarantee: process group plus a pid registry reaped at the next start, and a stdin-pipe liveness shim if the spike shows it is needed |
| "Closing the terminal stops everything" (attached mode) | Console close and logoff events arrive as a termination signal in Go; supervisor and children are in one job (spike SP2 must prove this in Windows Terminal, where it failed for PowerShell) | `SIGHUP` on terminal close, then the same ordered stop | `SIGHUP`, same |
| Background mode | Logon task (default) or Windows service | systemd user or system unit | launchd agent |
| Orderly stop | Control-API call, with a stop-request file as the fallback when the supervisor's API is unreachable | Same | Same |
| Credentials | Credential Manager through the keyring backend, or encrypted file | Secret Service if present, else encrypted file | Keychain |
| Desktop notice | Toast | `notify-send` (best effort) | `osascript` |
| Free space, root guards, ACLs | Win32 calls, protected-folder list, owner-only ACL | `statfs`, a list of system directories and mount points, mode 0600/0700 | Same as Linux |
| Paths | `%LOCALAPPDATA%`, `E:\cameras` style roots | XDG directories, `/srv/cameras` style roots | `~/Library/Application Support` |

A pid registry applies on every OS as a second layer: each child's pid, start time and image path are written to
`_state/children.json`; a supervisor that starts after an abrupt death verifies each entry (pid alive, same start time, same image
path) and stops provable orphans before starting its own. This is the portable form of task S3 and never kills by name alone.

## 6. Decisions inside the design

1. **"The open segment" is the newest file whose name matches the strict segment pattern**, confirmed by a modification time
   within the last segment period, not NTFS creation time. File names are written by ffmpeg with a clock-aligned pattern and
   sort chronologically, so the rule works on every filesystem. It feeds the pruner's protection, the stall watchdog and the down
   alert; spike SP4 proves it on NTFS and ext4.
2. **One writer, atomic.** `status.json` is written by temp file, flush and rename. On Windows a rename over a file a reader has open
   can fail; spike SP6 settles whether the readers need share-delete semantics or short retries.
3. **A real single-instance lock** (an exclusive lock file under `_state`, held for the supervisor's life) in addition to the
   heartbeat check, so a missing or unreadable status file cannot let a second supervisor start. Exit code 20 keeps its meaning
   "do not restart".
4. **The liveness verdict has one implementation**: a fresh heartbeat (age at most max(3 x status interval, 15 s)) and a live
   supervisor pid with a matching start time. The supervisor, the CLI and the web app all call it.
5. **Hard-kill versus graceful stop of ffmpeg.** Today children are always killed hard and the muxer's partial last segment is
   left on disk. The Go version keeps that (it is what the pruner and the empty-segment cleanup are tested against); a graceful
   stop is a later option.
6. **Reproduce the documented intent, not the accidents.** Where the inventories found a defect (for example
   `LastWindowClose` is dropped on a supervisor restart although todo [78] says it is carried) the Go version implements the
   intent and says so in its release notes; each is a numbered conformance test.

## 7. Migration: a strangler, not a rewrite

Each phase ships value, ends at an Operator gate, and can be abandoned without harm.

| Phase | What | Gate / proof |
|---|---|---|
| 0 Contract | Write `status.json` as a versioned JSON Schema, add a conformance test that the PowerShell writer satisfies it, add a golden test of the ffmpeg argument vector against the PowerShell output | Schema and golden files committed; no behaviour change |
| 1 Libraries | Go `archive`, `status`, `clock` and the pruner as pure, table-tested code, plus a differential harness that feeds the same synthetic archive trees to the PowerShell pruner and the Go pruner and compares the delete lists | Zero differences over the ported prune matrix and a generated corpus |
| 2 Recorder in shadow | The Go recorder and supervisor run beside the PowerShell system on one or two cameras, reading the SAME relay paths (no extra camera session), writing to a separate shadow root with its own small retention cap, its own `status.json` and its own web instance on another port | Segment boundaries, names, sizes, ffprobe durations and audio match the PowerShell output; no gaps; pruner behaves under the cap |
| 3 Supervisor guarantees | Host, ordered stop, window/terminal close, single-instance lock, orphan registry, fault injection (kill the supervisor, kill a camera, drop the relay) | G1 to G15 each have a passing Go test, including the six that have none today |
| 4 Cutover per Operator choice | Point the real archive at the Go system for all cameras or a subset; the PowerShell system stays installable and is one command away | A written rollback; one week of use |
| 5 Other OSes | The same system on Linux (the Ubuntu WSL on this PC for CI, then real hardware) and macOS best effort | Fake-camera integration suite green on each OS in CI |

Shadow sizing (computed from the 15-minute segments seen on 2026-10-17): one camera costs about 12 to 16 GB per day, so a
shadow on two cameras with a 40 GB cap fits beside the real archive on a drive with about 125 GB free and exercises the Go
pruner for real. A shadow run therefore cannot span the clock change on 25 Oct 2026 (8 days away, and no Go recorder exists);
the practical target is the change on 28 Mar 2027, with the DST table test and a synthetic clock covering it in the meantime.

## 8. Risks and how the design answers them

| Risk | Answer |
|---|---|
| ffmpeg argument order or `-segment_atclocktime` behaviour differs on Linux (U2, V8 in the inventories) | Golden test of the vector; spike SP1 runs the same arguments on Linux with synthetic input and compares names and boundaries |
| Pruner deletes the wrong thing | The pruner is a pure function from (files, free space, rules) to decisions, so it is table-tested and differential-tested before it ever touches a disk; gate order preserved exactly; first real prune only under the ported guards (plan Stage 4 acceptance) |
| Children survive a dead supervisor | Kernel mechanism per OS plus the pid registry; fault-injection tests on Windows and Linux; macOS limits stated openly |
| Closing the terminal does not stop the system on Windows Terminal | Spike SP2 before any host code; the fallback is a small keeper process outside the console, as PowerShell needed |
| Clock changes and DST | One time seam, `%z` names, the ported DST table, monotonic intervals |
| Behaviour drift during the rewrite | The inventories are the specification; every conformance item cites its source line; unknowns become spikes |
| Operator workload | Phases end at gates with a short demo; agents run feasibility checks only |

## 9. Decisions

Decided by the Operator on 2026-10-17 (D7 to D18 "ok" as recommended; D2 and D5 explained below).

| # | Decision | Outcome |
|---|---|---|
| D2 | Name | **Holocron** (executable `holocron`). A Jedi holocron is a device that stores recorded information and plays it back, which is what an NVR archive is. See the note below on the trademark |
| D5 | Shadow window | **7 days of shadow recording on two cameras**, then the cutover decision. Explained below |
| D7 | Background mode on Windows | Default logon task, optional service; attached mode kept for "close the window to stop". All installed and removed by `holocron install` and `holocron uninstall` |
| D12 | Camera secret store | Interface with keyring and encrypted-file backends; recorders read only through the relay |
| D13 | One binary with roles | One executable (now rule R1) |
| D14 | Third-party dependencies | At most `golang.org/x/sys`, one service-install library and one keyring library, each pinned and reviewed; everything else standard library |
| D15 | macOS | Best effort; Windows and Linux are first-class |
| D16 | New repository | At the end (D1) |
| D17 | The Go system manages both MediaMTX instances | Yes |
| D18 | Archive layout and file names | Unchanged, so both systems can share footage |
| D19 | Reproduce documented intent where the PowerShell code differs (`LastWindowClose` carried across a restart; no immediate alert 110 for a late-enabled camera) | Recommended yes. Still to be confirmed |

**What D5 is.** Before the Operator trusts the new recorder with the real footage, it runs next to the old one for a while:
it reads the same relay (so the cameras see no extra connection), writes its own copy into a separate folder under a small
size cap, and a comparison tool checks that its files have the same names, boundaries, sizes, durations and audio as the
old system's, with no gaps. D5 is how long that comparison runs. Seven days covers a full week of different conditions
(about three and a half of today's retention cycles, since the disk holds roughly two days) and exercises the new
pruner under its cap. The next clock change, 28 Mar 2027, cannot be waited for; it is covered by the ported DST test table
and then observed in production with the PowerShell system still installable as the fallback.

**Name note.** Star Wars names are trademarks of Lucasfilm. "Holocron" is a common word in many open-source projects and is
fine for a personal system or a public repository; if the project were ever sold as a product, it should be renamed.

## 10. How long, honestly

Estimates are agent time, in hours, with review gates in between and no stress tests: Phase 0 about 6; Phase 1 about 12; the
spikes about 8; Phase 2 about 25; Phase 3 about 25; Phase 4 about 6 plus a week of watching; Phase 5 about 15. Roughly 100
agent-hours before the Operator would consider cutover, spread over several weeks of calendar time. The first visible
product of this work is Phase 0, which also makes the existing system safer to change.
