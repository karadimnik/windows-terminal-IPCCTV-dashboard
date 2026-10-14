# The Go toolchain for `camui.exe`

The control UI (`ui/`) is a single Go binary, cross-compiled from WSL and run on Windows. **The NVR
machine needs no toolchain** - the binary is statically linked and the whole front end is embedded in
it with `go:embed`, so there is nothing to install beside it and nothing to fetch at runtime.

`apt` on this distribution offers only Go 1.13 (2019), which is far too old to build this, so the
toolchain comes from go.dev.

## One-time install, in WSL

Installed without `sudo` under `~/.local/go`, because this account has no passwordless sudo.

```bash
version=go1.27.2                      # latest at the time of writing: curl -s "https://go.dev/VERSION?m=text"
curl -sSLO "https://go.dev/dl/${version}.linux-amd64.tar.gz"

# Verify against the checksum go.dev publishes, not against the file you just downloaded.
curl -s "https://go.dev/dl/?mode=json" | grep -A2 "${version}.linux-amd64.tar.gz"
sha256sum "${version}.linux-amd64.tar.gz"

mkdir -p ~/.local
rm -rf ~/.local/go
tar -C ~/.local -xzf "${version}.linux-amd64.tar.gz"
~/.local/go/bin/go version
```

`go1.27.2.linux-amd64.tar.gz` sha256
`ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5`, matching go.dev.

To get `go` on the PATH for interactive use, add this to `~/.zshrc`:

```bash
export PATH="$HOME/.local/go/bin:$PATH"
```

`ui/build.sh` does not need that: it falls back to `~/.local/go/bin/go` when `go` is not on the PATH.

## Building

```bash
ui/build.sh                 # writes <repo>/camui.exe
ui/build.sh /tmp/camui.exe  # or somewhere else
```

The script runs `gofmt -l`, `go vet` and `go test` before it builds, then cross-compiles with

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=..."
```

`CGO_ENABLED=0` keeps the result free of any C runtime dependency. `-s -w` drops the symbol table and
DWARF data, which this binary has no use for; the result is about 7.2 MB.

`camui.exe` is gitignored, like the MediaMTX binary. Build it, do not commit it.

## Running

```powershell
camui.exe                                           # 127.0.0.1:8787, archive root E:\cameras
camui.exe -listen 127.0.0.1:9000
camui.exe -archive-root E:\cameras
camui.exe -status-file E:\cameras\_state\status.json
camui.exe -config camui.config.json
camui.exe -version
```

### The loopback guard

`camui.exe` **refuses to start** on any address that is not exclusively loopback unless an auth token
of at least 16 characters is configured, and exits 2 with a message naming the remedy. That makes the
later LAN step a deliberate act and makes unauthenticated exposure impossible rather than merely
discouraged.

The token is read from `CAMUI_TOKEN` in the environment or from `"token"` in the config file, never
from a flag - a command line is readable by every process on the machine. When a token is set it is
required on every request, as `Authorization: Bearer <token>`, as the `camui_token` cookie, or once as
`?token=<token>`, which is then moved into an HttpOnly cookie and redirected away so it stops
appearing in the address bar.

Note for testing from WSL: `CAMUI_TOKEN=x camui.exe` does **not** reach a Windows process, because WSL
only forwards the variables named in `WSLENV`. Set it on the Windows side instead:
`powershell.exe -NoProfile -Command "$env:CAMUI_TOKEN='...'; & camui.exe -listen 0.0.0.0:8788"`.

### Logging

One `log/slog` text line per event on stdout, which is what a service wrapper captures:

```
time=2026-10-12T19:25:35.684+03:00 level=INFO msg="camui listening" version=27d4273-20261012 address=127.0.0.1:8787 statusFile=E:\cameras\_state\status.json auth="none (loopback only)"
time=2026-10-12T19:25:39.459+03:00 level=INFO msg=request method=GET path=/api/status status=200 bytes=8459 ms=152
```

Query strings are never logged, because one of them can carry the token.

## Screenshotting the UI

Chrome headless on Windows **clamps a window to a 500 CSS px minimum**. `--window-size=390,2000`
therefore renders the page at 500 px and crops the image to 390, which looks exactly like a layout
that overflows. Measured with a probe page printing `window.innerWidth`: asking for 390 reported
`innerWidth=500`.

Above 500 px, screenshot the page directly:

```powershell
& 'C:\Program Files\Google\Chrome\Application\chrome.exe' --headless=new --disable-gpu `
  --hide-scrollbars --user-data-dir="$env:TEMP\camui-shots" --virtual-time-budget=12000 `
  --window-size=1440,2000 --screenshot=desktop.png 'http://127.0.0.1:8787/'
```

Below it, render the page inside an iframe of the exact width and screenshot the host page. The
iframe gets a real viewport of that width, so the media queries evaluate correctly:

```html
<style>html,body{margin:0;background:#000}iframe{width:390px;height:2100px;border:0;display:block}</style>
<iframe src="http://127.0.0.1:8787/"></iframe>
```

Give it `--virtual-time-budget=30000`: the budget has to cover the subframe's own `fetch` of
`/api/status`, and a shorter one captures the shell with no data in it. Always pass `--user-data-dir`
pointing at a throwaway directory, so a screenshot run never touches the Operator's Chrome profile.
