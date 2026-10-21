# autostart

The OS entry that starts holocron at logon or boot, for `holocron install`, `uninstall` and `autostart ...`
(rule R1; operator guide: [docs/autostart.md](../../docs/autostart.md)). Standard library only; every OS tool
goes through the `Exec` seam and the Windows service manager through `osx.ServiceManager`.

    type Mode string                       // logon | service | systemd | launchd
    func DefaultMode(goos) (Mode, error); func ParseMode(s, goos) (Mode, error)   // ErrUsage: exit 2
    func DefaultName(Mode) string; func CheckName(name, Mode) error              // own names, or "-TEST-" names
    type Exec func(name string, args ...string) ([]byte, error)                // RealExec: stdin null, 30 s timeout
    type Env struct{ GOOS; Exec; SCM osx.ServiceManager; Home, ConfigHome, User, UID, SystemRoot; TempDirs; TempFile; Stat }
    func RealEnv() Env
    type Spec struct{ Mode; Name, Exe, Config, Root string; Disabled bool; Account, Record string }
    type Ops struct{ Env; Out io.Writer; Log *slog.Logger; Dry bool }           // Dry: print mutating steps, run queries
    func Generate(Env, Spec) (Entry, error)                                     // task XML / unit / plist / service settings
    func Install(*Ops, Spec, version) (Result, error)                           // idempotent, verified, recorded, rolled back on failure
    func Remove(*Ops, Mode, name) (removed bool, err error)
    func SetEnabled(*Ops, Mode, name, on) (changed bool, err error)
    func Status(Env, Mode, name) State                                          // read-only; State.Summary()
    func QueryOldTasks(Env) []OldTask                                           // IPCCTV-* tasks, schtasks /Query only
    func TransientExe(exe, Env) string; func Linger(Env, user) string
    type Record; func RecordPath(configDir) string; ReadRecord / WriteRecord / RemoveRecord
    type FakeTools                                                              // stateful fake schtasks/systemctl/launchctl/loginctl
    func Mutating(calls []string) []string                                      // the calls a dry run must never make

| Mode | Created with | Read back with | Switched with | Removed with |
|---|---|---|---|---|
| logon | `schtasks /Create /TN <n> /XML <UTF-16 file> [/F]` | `schtasks /Query /TN <n> /XML` (UTF-16 or code-page text declaring UTF-16) | `schtasks /Change /TN <n> /ENABLE\|/DISABLE` | `schtasks /Delete /TN <n> /F` |
| service | `CreateService` + `ChangeServiceConfig2` (description, delayed start, restart after 60 s x3, also on a non-zero exit) | `QueryServiceConfig(2)`, `QueryServiceStatus` | `ChangeServiceConfig` start type auto (delayed) / disabled | `DeleteService` |
| systemd | write `<config>/systemd/user/<n>.service`, `systemctl --user daemon-reload`, `enable` | the file, `is-enabled`, `is-active` | `systemctl --user enable\|disable` | `disable`, delete, `daemon-reload` |
| launchd | write `~/Library/LaunchAgents/<n>.plist`, `launchctl enable gui/<uid>/<n>` | the file, `print-disabled`, `print` | `launchctl enable\|disable` | `bootout` when loaded, delete |

Install compares the generated entry with the one read back on the command line, the working folder, the enabled
state, the settings key (logon: trigger, logon type, instance policy, battery, start-when-available, time limit,
priority; service: account, delayed start, recovery, display name) and, for files, the exact content. Paths: Windows
paths must be absolute without `"` or `%` (the Task Scheduler expands `%VAR%` with no escape) and are quoted by the
`CommandLineToArgvW` rules; `&` and `<` are XML-escaped; systemd arguments are quoted with `%%`, `$$`, `\\`, `\"`.

Tests (`go test ./internal/autostart`, `-update` rewrites `testdata/`): golden task XML, unit and plist; command
sequences of every mode; idempotence and update; rollback after a failed verification, a failed `enable` and a failed
create; dry runs make no mutating call and write no file; old tasks only queried; quoting of spaces, `&`, `%`, `"`;
UTF-16 decoding; service mode against `osx.FakeSCM` (create, update, switch, delete, access denied, a running
service only marked for deletion, accounts).

Verified for real (2026-10-21): systemd user unit `holocron-TEST-*` in WSL2 (install, status, enable, disable,
second install unchanged, `systemd-analyze --user verify` clean, uninstall); Windows logon task `Holocron-TEST-*`
created disabled without elevation from a path with a space and `&`, read back equal; Microsoft Defender then blocked
the unsigned test binary (`Behavior:Win32/Persistence.A!ml`), so enable, disable, the second install and the uninstall
through holocron were not run on Windows (the test task was deleted with `schtasks /Delete`). UNVERIFIED: the Windows
service (needs elevation), macOS.
