# osx (first slice)

Per-OS facts behind one interface. Process lifetime, console events, services and notifications are NOT here yet (W8).
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
