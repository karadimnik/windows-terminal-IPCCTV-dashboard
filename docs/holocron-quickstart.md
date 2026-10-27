# Holocron quickstart

Four steps from nothing to a recording NVR. Windows PowerShell 5.1, not elevated. Needs ffmpeg 5.0 or later (`C:\ffmpeg\bin`), MediaMTX (`C:\mediamtx\mediamtx.exe`) and cameras that answer RTSP. A line starting `#` is the result to expect.

## 1. The program, in a permanent folder

```powershell
# in WSL, from the repository: gofmt, vet and the tests run first, then holocron.exe appears in the repository root
./build-holocron.sh
mkdir C:\Holocron; copy .\holocron.exe C:\Holocron\; Set-Alias holocron C:\Holocron\holocron.exe
holocron version
# holocron <version> (status.json schema 1, windows/amd64, go1.27.2)
```

Not in Downloads or a temporary folder: `holocron install` refuses those. `Set-Alias` lasts for this window only; use `C:\Holocron\holocron.exe` elsewhere.

## 2. The two files, beside `holocron.exe`

```powershell
Set-Content C:\Holocron\holocron.config.json '{ "RootPath": "E:\\cameras" }'
Set-Content C:\Holocron\cameras.json '[ { "Name": "Front Door", "IP": "192.168.1.50", "Quality": "stream1", "Record": true } ]'
holocron config validate
# ok   C:\Holocron\holocron.config.json
# ok   C:\Holocron\cameras.json (1 camera(s))
```

`RootPath` is the only key you must set; every other key has a default (`internal/config/KEYS.md`). Add `FfmpegPath`, `FfprobePath` and `RestreamerExePath` only if those programs are not in the folders above. One entry per camera: `Name`, `IP` (or `host:port`), `Quality` (`stream1` main, `stream2` sub) and `Record` (`true` to record).

## 3. The camera login, then the check

```powershell
holocron secret set camera.user
holocron secret set camera.password
# stored camera.user (keyring store)    (type the value, Enter; nothing is echoed)
holocron doctor
# no line starts with "fail"; exit code 0
```

Coming from the PowerShell system: `holocron secret import --from C:\old\camera_creds.xml` prints `imported camera.user and camera.password` and leaves the old file untouched. A `fail` line names its own fix; `warn` lines about a new root or a missing status file are normal before the first start.

## 4. Start, look, start at logon

```powershell
holocron up --detach
# RECORDING: supervisor pid <n> is running after <x>s; 1 camera(s) set to record (holocron status shows each)
holocron open
# opens http://127.0.0.1:8787/#live in the browser (holocron open --print only prints the address)
holocron install
# installed and verified: logon task Holocron, enabled, console window visible, runs ...
```

`holocron install` starts nothing now and, with the visible window (default), closing that window at logon stops everything; `holocron install --window hidden` has no window. Stop it any time with `holocron down` (`stopped: 0 survivors after <x>s`).

## Linux and macOS

```bash
go build -o ~/.local/bin/holocron ./cmd/holocron          # in the repository; ffmpeg 5.0+ and mediamtx on PATH
mkdir -p ~/.config/holocron && printf '{ "RootPath": "%s/cameras" }\n' "$HOME" > ~/.config/holocron/holocron.config.json   # macOS: ~/Library/Application Support/holocron
printf '[ { "Name": "Front Door", "IP": "192.168.1.50", "Quality": "stream1", "Record": true } ]\n' > ~/.config/holocron/cameras.json
printf '%s\n' "$CAMERA_USER" | holocron secret set camera.user; printf '%s\n' "$CAMERA_PASSWORD" | holocron secret set camera.password
holocron doctor && holocron up --detach && holocron install   # systemd user unit; macOS launchd is UNVERIFIED
```

## Then

- **Phone or second PC over HTTPS**: add `"web": { "tlsListen": "192.168.1.50:8443" }` to the config, `holocron user add anna` (password on standard input, 12 characters or more), `holocron tls ca` (the certificate to install on the device), allow the port in the firewall, `holocron restart --detach`. Details: [ui.md](ui.md#https-on-the-lan-a-phone-or-a-second-pc). Viewers: `holocron user add bob --role viewer`.
- **Every command, file, port, exit code and alert on one page**: [holocron-cheatsheet.md](holocron-cheatsheet.md); the complete reference for AI agents: [ai/holocron-for-agents.md](ai/holocron-for-agents.md).
- **Moving from the PowerShell system, or back**: [cutover.md](cutover.md), [rollback.md](rollback.md) (going back includes `holocron archive flatten --now`: Holocron files closed segments into day folders that the old system does not read). Autostart in depth: [autostart.md](autostart.md). Problems and limits: [troubleshooting.md](troubleshooting.md), [limitations.md](limitations.md).
