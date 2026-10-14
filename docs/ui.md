# The control UI

Its three screens - Dashboard, Settings and Footage - are described in the [README](../README.md#day-to-day).

## How it works

`camui.exe` is one Go binary with the whole UI embedded in it. **It owns no state.** Every number comes from `_state\status.json`, the same file the health strip reads, and every change goes through `cam.ps1` and `cam.recorder.ps1`. That is why the command line keeps working unchanged and why the two can never disagree about anything. Its Footage index is parsed from the filenames the recorder already writes, so there is no database and nothing opens a segment to list it; a clip is remuxed to fragmented MP4 on demand, video copied and only the 8 kHz audio transcoded, so playback keeps its sound.

**No camera credential ever reaches the browser.** Cameras are addressed by name, and no endpoint returns a relay or RTSP URL of any shape. The harness proves it rather than trusting it: the `uileak` check fetches every endpoint, a real clip included, and scans every byte for the password, for its URL-escaped form and for anything shaped like a credential in a URL, with a positive control proving the scanner can see one.

**Loopback only, deliberately.** `127.0.0.1` needs no `urlacl` and no elevation, and the binary **refuses to start** on any address that is not exclusively loopback unless an auth token of at least 16 characters is configured. Exposing it beyond this machine is therefore a deliberate act rather than something that can happen by accident. To do it, save this as `camui.config.json` next to `camui.exe` and restart the UI:

```json
{
  "listen": "0.0.0.0:8787",
  "token": "a-long-random-string-of-at-least-16-characters"
}
```

Nothing else has to change: the launcher passes that file if it exists, and `-Status` reports the address it found and where it read it from. Every request then needs the token, which can arrive once as `?token=...` and is moved into an HttpOnly cookie so it stops appearing in the address bar, in history and in any `Referer`. The scripts **do** read the token - `Get-CamUiRuntimeState` opens `camui.config.json` and looks at `CAMUI_TOKEN` to answer the one question `-Status` asks of it - but they never print it, log it or pass it on: `-Status` reports only whether one is configured. The token is never taken from a flag, because a command line is readable by every process on the machine.

**How it runs.** The `IPCCTV-Ui` logon task runs a generated launcher at `%LOCALAPPDATA%\IPCCTV\start-ui.ps1`. The launcher waits for `camui.exe` to be reachable, exactly as the recorder's launcher waits for the scripts, then starts it in a console of its **own** with no window. That last part is not cosmetic: a console created with a window is handed to Windows Terminal as a tab in the window that already exists, and closing that window would take the UI with it. It starts nothing at all if the address is already being served, so a second start cannot fight the first one for the port, and it exits non-zero if nothing is serving afterwards, which hands the retry to the task's restart policy.

camui logs to stdout only, so a thin `powershell.exe` holds the pipe and appends it to `%LOCALAPPDATA%\IPCCTV\camui.log`. **The roll happens in the launcher, not in a running UI:** at every start it checks that file, and if it is past 10 MB it renames it to `camui.log.prev` - replacing any previous `.prev` - and starts a fresh one. A UI that stays up for weeks therefore keeps appending to one file past 10 MB, with a line per request and nothing to bound it; stopping and starting it is what rolls the log. The launcher's own log is `autostart-ui.log` beside it, and that one has no rotation at all - it gets a handful of lines per start, so it grows slowly, but it grows forever. Delete either by hand when it is in the way; nothing reads them back.

The binary is never copied anywhere, so a rebuild takes effect the next time the UI starts, with no reinstall. Stop it before rebuilding and start it again afterwards: a running process keeps the code it started with, and with the repository on the WSL share the write is not refused the way Windows refuses to overwrite a running binary on a local drive. `cam.ps1 -Stop` does not stop the UI, which is the point - the thing you start everything from should still be there after you have stopped everything. When you do want it gone, `-Status` prints the pid that owns the port.

## The commands: `camui.exe`

`-Install` puts its logon task in place, and letting that run it is the normal way. These are for building it and for the rare direct run.

| Command | What it does |
| --- | --- |
| `ui/build.sh [output]` | Cross-compiles `camui.exe` into the repository root, or to `output` if you name one. A bash command, so it needs WSL or another machine with bash, and **Go 1.27 or newer** - `go.mod` asks for it and an apt-packaged Go will refuse the build; [docs/go-toolchain.md](go-toolchain.md) is the one-time install, and the script also finds a toolchain at `~/.local/go/bin`. It finds its own directory, so the directory you run it from does not matter. It runs `gofmt`, `go vet` and `go test` first and stops on any of them, so a build that completes is a build that passed. |
| `.\camui.exe` | Runs it in the foreground on `http://127.0.0.1:8787/`, with its log on screen. Useful when you want to watch what it is doing; Ctrl+C stops it. |
| `-listen <host:port>` | Where to bind. Default `127.0.0.1:8787`. It **refuses to start** on any non-loopback address unless a token of at least 16 characters is configured. |
| `-config <path>` | A JSON config file: `listen`, `archiveRoot`, `statusFile`, `scriptDir`, `token`. The installed launcher passes `camui.config.json` from beside the binary if that file exists, and nothing else needs to change to use it. |
| `-archive-root <path>` `-status-file <path>` `-script-dir <path>` | Override what it reads. Precedence for the archive root is this flag or `archiveRoot` in the config file, then `RootPath` in the `recorder.config.json` beside the binary, then the built-in `E:\cameras` - so moving the archive needs no second edit here. The status file defaults to `<archive root>\_state\status.json` and the script directory to the directory holding the binary. |
| `-version` | Prints the build stamp and exits. |

The binary has to sit beside the scripts: with no `-script-dir` it looks for `cam.ps1`, `cameras.json` and `recorder.config.json` in its own folder.
