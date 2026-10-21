# osx

Per-OS facts behind one interface. Process lifetime, the instance lock, the child registry and console events (W8) and the
Windows service manager seam (W12) are below; desktop notifications are not here yet.
Build-tagged files: `free_{unix,windows}.go`, `perm_{unix,windows}.go`, `mounts_{linux,darwin,windows}.go`. `env.go` holds
the pure builders (`DefaultDirs`, `ProtectedFor`) so every OS's table is tested on any host.

    type Protected struct{ Path, Why string }          // root may neither equal nor contain Path
    type Dirs struct{ Root, Config, State, Logs, Auth, TLS string }
    type Platform interface {
        Name() string                                  // GOOS value; selects path syntax in archive.CheckRoot
        DiskFree(path string) (uint64, error)          // bytes available to the current user
        ProtectedRoots() []Protected
        SetOwnerOnly(path string) error                // 0600 file / 0700 dir; Windows: icacls (same technique as ui/fileperm_windows.go)
        Dirs() Dirs
    }
    func Current() Platform
    type Env struct{ Getenv func(string) string; Home, ExeDir string; Mounts []string }
    func DefaultDirs(goos string, e Env) Dirs
    func ProtectedFor(goos string, e Env) []Protected
    type Fake struct{ OS string; Free map[string]uint64; DefaultFree uint64; Protected []Protected; DirsValue Dirs; FreeErr, OwnerErr error }
    func NewFake(goos string, e Env) *Fake             // plus (*Fake).OwnerOnlyCalls() []string

Defaults (LLD section 4): Windows root `E:\cameras`, config = the program's own folder (where `recorder.config.json` and `cameras.json`
live today), auth `%LOCALAPPDATA%\IPCCTV\camui\auth`, tls `%LOCALAPPDATA%\IPCCTV\tls`; Linux root `/srv/cameras`,
config `$XDG_CONFIG_HOME/holocron`, auth/tls under `$XDG_STATE_HOME/holocron`; macOS root `~/Movies/cameras`, everything under
`~/Library/Application Support/holocron`. `State` and `Logs` are `<Root>/_state` and `<Root>/_logs`.

Protected sets: Windows - Windows dir, Program Files (3 variants), user profile and its parent, the program's folder.
Linux/macOS - system folders, `$HOME` and its parent, mount points (`/proc/self/mountinfo`; `/Volumes/*`), the program's folder.
Drive roots, UNC share roots and `/` are judged structurally by `archive.CheckRoot`. macOS lists are unverified (compiled only).

## Process lifetime, lock, registry, console (W8; SP2/SP3, LLD 2.9 and 11.1)

    // scope: every child bound to this process's life
    type Cmd struct{ Path string; Args []string; Dir string; Env []string; Stdout, Stderr io.Writer; Role string } // stdin = null device
    type Proc interface{ Pid() int; Wait() (code int, err error); Kill() error } // satisfies ffmpeg.Process and host.Proc
    type ScopeOptions struct{ Registry *ChildRegistry; Log *slog.Logger }
    func NewScope(name string, o ScopeOptions) (*Scope, error)
    (s) Start(Cmd) (Proc, error); Live() []int; Stop(grace) (survivors []int); Close() error   // ErrScopeClosed after Close
    const OutputDrain = 2 * time.Second
    // instance lock (<state>/supervisor.lock)
    func LockPath(stateDir string) string
    func AcquireInstanceLock(path string) (*InstanceLock, error)     // *HeldError{Path, Owner LockOwner}; errors.Is(err, ErrHeld)
    (l) Release() error; Owner() LockOwner; Path() string
    type LockOwner struct{ Pid int; Started time.Time; Exe string }; func ReadLockOwner(path) (LockOwner, error)
    // child registry (<state>/children.json) and the reap
    type ChildEntry struct{ Pid int; Start, Image string; Group int; Role string }
    func RegistryPath(stateDir) string; func NewRegistry(path) *ChildRegistry
    (r) Load() ([]ChildEntry, error); Add(ChildEntry) error; Remove(pid) error; Entries() []ChildEntry; Reset() error
    type Inspector interface{ Inspect(pid) (ProcInfo, error); Stop(ChildEntry) error }   // OSInspector{}, FakeInspector{Procs}
    type ProcInfo struct{ Start, Image string; Group int }; var ErrNotRunning
    func Verify(e ChildEntry, in Inspector, goos string) (ok bool, why string)
    func Reap(reg *ChildRegistry, in Inspector, log *slog.Logger) ([]ReapVerdict, error)  // then the file is emptied
    // console
    type Reason string // close | logoff | shutdown | hangup | terminate | interrupt | console-gone
    func OnConsoleClose(fn func(Reason)) (stop func())
    func WatchConsole(fn func(Reason), interval time.Duration) (stop func(), err error)    // ErrNoConsole; no-op on Unix
    func ConsolePresent() (bool, string)
    const ConsoleBudget = 4 * time.Second; ProbeInterval = 250 * time.Millisecond

**Windows** (`scope_windows.go`): one unnamed job (default security: not inheritable) with `KILL_ON_JOB_CLOSE`, never
`BREAKAWAY_OK`. Each child: `syscall.CreateProcess` with `CREATE_SUSPENDED|CREATE_NO_WINDOW|CREATE_UNICODE_ENVIRONMENT|
EXTENDED_STARTUPINFO_PRESENT`, `AssignProcessToJobObject`, `ResumeThread`; a failed assignment kills the child and fails the start.
`PROC_THREAD_ATTRIBUTE_HANDLE_LIST` limits inheritance to the three standard handles (no other handle of the process, a caller's
pipe or the lock, can leak into ffmpeg). Command line by `syscall.EscapeArg` per argument (os/exec's rule), environment block with
case-insensitive last-wins and `SYSTEMROOT` added when missing, stdout/stderr through pipes copied to the writers, `Wait` drains at
most `OutputDrain` after the exit. Exit codes are signed 32-bit. `Stop` = `TerminateJobObject`; `Close` closes the job handle.
**Linux** (`scope_unix.go`, `scope_linux.go`): ONE goroutine locks its OS thread and creates every child, never returning before
`Close` (a returning locked starter ends its thread and Pdeathsig kills its children at once, SP3 l3). `Setpgid` (each child leads
its own group: a shared group cannot be joined once its leader is gone) + `Pdeathsig: SIGKILL`. The exit is first observed with
`waitid(WNOWAIT)` so the group id is pinned by the zombie; the rest of the group (grandchildren) is killed then, before the reap; a
`Kill` or `Stop` is `kill(-pgid, ...)` only while the leader is unreaped (no reused group id can be hit). `Stop` = SIGTERM to every
group, grace, SIGKILL. `Close` ends the starter thread, which takes what is left. **macOS**: `Setpgid` only, no Pdeathsig, no
`waitid(WNOWAIT)` in Go (a tiny window remains in which a group kill could race the reap); the reap cannot verify anything (start time
and image need sysctl/proc_pidinfo structures the standard library does not expose) so it leaves every entry alone with a WARN.
The stdin-pipe watcher was NOT implemented (it needs a self re-exec mode of the command tree, W9). Compiled and vetted for darwin; nothing
was run on a Mac.

**Lock**: Windows `CreateFile` (share read/write/delete) + `LockFileEx(EXCLUSIVE|FAIL_IMMEDIATELY)` on a byte far beyond the end of
the file (byte-range locks are mandatory; locking the content would hide the owner text); Unix `flock(LOCK_EX|LOCK_NB)` on an
`O_CLOEXEC` descriptor. The owner JSON is written after the lock is held and truncated on release; it is only ever used for messages.

**Registry/reap**: rewritten atomically (temp, fsync, rename; only the supervisor opens it, so the SP6 share-delete rename is not
needed) after every start and exit. An entry is stopped only if alive AND same start token AND same image (case-insensitive on
Windows) AND, when recorded, the same group; the kill re-checks on the handle it uses (Windows process handle, Linux pidfd). Tokens:
Windows creation FILETIME; Linux `<boot_id>:<starttime ticks>`. Never by name, never by pid alone.

**Console** (`console_windows.go`): Go maps close/logoff/shutdown to SIGTERM and, with a `signal.Notify` receiver, its handler BLOCKS
so the process lives until it exits itself or Windows ends it ~5.04 s after the event (runtime/os_windows.go; SP2). A raw
`SetConsoleCtrlHandler` callback, registered after the runtime's so it runs first, only records which event it was and returns FALSE.
Ctrl+C/Break arrive as SIGINT (`interrupt`). `WatchConsole` polls `GetConsoleProcessList` / `GetConsoleMode(stdin)` /
`GetConsoleWindow` at 4 Hz; gone = the process list is empty and one of the other two failed (stdin may be redirected). Unix:
SIGHUP / SIGTERM / SIGINT.

Tests: unit tests by default; real-process tests opt-in `HOLOCRON_OSX_REAL=1` (helper roles of the test binary; only pids the tests
start are killed, after identity verification). Measured 2026-10-20 (Windows 11, WSL2 Ubuntu 20.04):

| Test | Windows | Linux |
|---|---|---|
| `TestRealScopeHardKillLeavesNoSurvivors` (3 children + console hosts) | 0 survivors, 12 ms | 0, 21 ms |
| `TestRealScopeHardKillWithGrandchildren` | 0 of 13 | children 0; 3 grandchildren survive (SP3 l6, documented) |
| `TestRealScopeKillRacesStart` (kill during back-to-back starts) | 0 | 0 |
| `TestRealWindowsSuspendedClosesAssignGap` (300 ms before assign, suspended) | 0 | - |
| `TestRealWindowsAssignAfterStartLetsGrandchildEscape` (control, SP3 w2) | 3 grandchildren + console hosts escape | - |
| `TestRealLinuxReturningStarterKillsChildren` (control, SP3 l3) | - | child killed at once |
| `TestRealLinuxOrderlyStopKillsGroup` | - | child + grandchild gone 2 ms |
| `TestRealRegistryReapSparesDecoy` | 3 reaped, decoy alive | same |
| `TestRealLockTwoProcesses` | ErrHeld with owner; free 5 ms after the holder's kill | same, 3 ms |
| `TestRealWindowsConsoleProbeFires` / `...NoProbeMissesConsoleDeath` (control) / `...ConsoleCloseEvent` | 501 ms / no event in 3 s / 314 ms | - |
| `TestRealUnixConsoleSignals` | - | hangup, terminate |

## Windows service manager (W12)

    type ServiceStart string // auto | demand | disabled
    type ServiceConfig struct{ Name, DisplayName, Description, BinaryPath, Account string; Start ServiceStart; Delayed bool; RestartDelay time.Duration }
    type ServiceState struct{ Exists bool; Config ServiceConfig; Running bool; State string }
    type ServiceManager interface{ Query(name) (ServiceState, error); Create(c) error; Update(c) error; SetStart(name, start, delayed) error; Delete(name) error }
    func Services() ServiceManager                         // advapi32 on Windows; ErrNoServiceManager elsewhere
    func RunService(name, fn func(ctx) int) error           // StartServiceCtrlDispatcher; ErrNotService outside the manager
    type FakeSCM struct{ Services; Running; Fail map[string]error; Calls }   // a running service is only marked for deletion
    var ErrAccessDenied                                      // creating/changing a service without elevation

`service_windows.go`: `OpenSCManager`, `CreateService` (own process, error control normal, LocalSystem when Account is
empty), `ChangeServiceConfig`/`ChangeServiceConfig2` (description, delayed auto start, failure actions restart x3 after
RestartDelay, reset after a day, and the failure-actions flag so a non-zero exit counts), `QueryServiceConfig(2)`,
`QueryServiceStatus`, `DeleteService`. `RunService`: `StartServiceCtrlDispatcherW` with a `syscall.NewCallback` service
main that registers a `RegisterServiceCtrlHandlerExW` handler, reports running (accepting Stop, Shutdown, Preshutdown),
cancels fn's context on any of them (stop pending, checkpoint every 5 s) and reports stopped with fn's exit code as a
service-specific error. Standard library only. UNVERIFIED on a real system (needs elevation; never taken): the logic
above it is tested against FakeSCM in `internal/autostart`, this file is compiled and vetted for windows only.
