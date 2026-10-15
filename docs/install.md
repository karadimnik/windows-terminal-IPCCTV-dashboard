# Installing from scratch

Most readers never need this page. The system autostarts at logon, and the normal state is that you
touch nothing - see [the README](../README.md) for the running system. This is the first-time setup
on a new machine.

## Prerequisites

1. **Windows 10 or 11** with [Windows Terminal](https://apps.microsoft.com/store/detail/windows-terminal/9N0DX20HK701) installed. Windows PowerShell 5.1 is what everything targets and it ships with Windows.
2. **[mpv media player](https://mpv.io/)**: download the Windows build and extract it so that `mpv.com` is at `C:\mpv\mpv.com`.
3. **[ffmpeg](https://www.gyan.dev/ffmpeg/builds/)**, for recording only: extract so that `ffmpeg.exe` and `ffprobe.exe` are in `C:\ffmpeg\bin`.
4. **[MediaMTX](https://github.com/bluenviron/mediamtx/releases)**, optional but recommended: download `mediamtx_<version>_windows_amd64.zip` and extract `mediamtx.exe` to `C:\mediamtx\mediamtx.exe`. Verified on v1.21.2. Do not put it in `Program Files`, which would need administrator rights. The 55 MB binary is deliberately not in this repository.
5. **RTSP compatible cameras** (TP-Link Tapo, Amcrest, Reolink, Unifi and anything else that serves `rtsp://user:pass@IP/stream1`).
6. **Nothing extra for the control UI.** `camui.exe` has the web UI embedded in it, needs no runtime, no web server and no internet access, and is **not** in this repository - it is gitignored like the MediaMTX binary. Build it once with `ui/build.sh`, which needs a Go toolchain on the **build** machine only; [`go-toolchain.md`](go-toolchain.md) is the one-time install. The NVR machine itself never needs Go.

All three tool paths are configurable: `$MpvPath` at the top of `mpv.cam.autolayout.autorestart.ps1`, and `FfmpegPath` / `FfprobePath` / `RestreamerExePath` in `recorder.config.json`. See [configuration.md](configuration.md).

## One thing to settle before step 1: the execution policy

With nothing set in any scope, the effective policy on Windows **client** editions is `Restricted`, which runs no `.ps1` at all - so step 1 below fails with `cannot be loaded because running scripts is disabled on this system` and nothing in this repository works. Check it with `Get-ExecutionPolicy -List`, and if it is `Restricted` or `Undefined` everywhere, this is enough and needs no elevation:

```powershell
Set-ExecutionPolicy -Scope CurrentUser RemoteSigned
```

If the repository arrived as a downloaded `.zip`, Windows marks every file in it as coming from the internet and `RemoteSigned` will still refuse them; `Get-ChildItem *.ps1 | Unblock-File` in the repository folder clears the mark. Cloning with git does not set it. This matters only for the commands **you** type: everything the project launches for itself - the panes, the health strip, the NVR window, all three logon-task launchers - already passes `-NoProfile -ExecutionPolicy Bypass`, so once it is installed the policy cannot stop it starting.

## The steps

Clone or download the repository to a dedicated folder, then work through this order. Steps 1 to 5 prepare the cameras and the recorder, step 6 builds the UI, step 7 installs everything that starts at logon - [Install and uninstall](../README.md#install-and-uninstall) in the README says what it does - and step 8 starts it.

```powershell
# 1. Launch the dashboard once. It prompts for the camera credentials, writes
#    camera_creds.xml, generates a cameras.json template and exits.
.\mpv.cam.autolayout.autorestart.ps1

# 2. Edit cameras.json with your real camera names, IPs and stream names, then
#    launch it again. This time the grid comes up.
.\mpv.cam.autolayout.autorestart.ps1

# 3. Copy the recorder config template and set RootPath to where footage goes.
copy recorder.config.example.json recorder.config.json

# 4. Pre-flight the recorder. Close the dashboard first: this probes the
#    cameras, and they allow only two sessions each. Leave it closed until
#    step 8, which brings it back up in the right order; or reopen it by
#    hand with the step 2 command if you want it now.
.\cam.recorder.ps1 -SelfTest

# 5. Opt cameras in. Nothing records until you do this.
.\cam.recorder.ps1 -Enable all

# 6. Build the control UI. A bash command, so run it from WSL or any machine
#    with bash and Go 1.27 or newer - the Go in Debian/Ubuntu's apt is far
#    older and will not build it; docs/go-toolchain.md installs a current one.
#    camui.exe is gitignored, so it does not arrive with the repository. The
#    script can be run from any directory and puts the binary beside the
#    scripts as .\camui.exe, which is where it has to be: with no -script-dir
#    it looks for cam.ps1, cameras.json and recorder.config.json in its own
#    folder. Built anywhere else, copy it here.
#      ui/build.sh

# 7. Install: the relay config generated from cameras.json (no credential is
#    written to it) and the three logon tasks - recorder, dashboard and UI.
#    -WhatIf first shows every prerequisite and the plan and changes nothing.
#    It starts nothing.
.\cam.ps1 -Install -WhatIf
.\cam.ps1 -Install

# 8. Start everything now rather than at the next logon: the recorder - its
#    supervisor also starts and owns MediaMTX - then the dashboard. Then open
#    the UI in a browser ON WINDOWS - http://127.0.0.1:8787/ - once its task
#    has run (Start-ScheduledTask -TaskName IPCCTV-Ui does it now). A curl
#    from inside WSL cannot reach it: WSL's loopback is its own, not the
#    Windows one, so it fails to connect and the UI looks down when it is
#    serving perfectly. Then check everything in one view.
.\cam.ps1 -Start
Start-ScheduledTask -TaskName IPCCTV-Ui
.\cam.ps1 -Status
```

`cameras.json`, `recorder.config.json`, `camera_creds.xml` and `relay_api_creds.xml` (the relay's control API login, generated by the product) are local to your machine and are not tracked by git.

**Not using MediaMTX?** `-Install` treats it as a prerequisite. Without it, install the pieces with the granular switches instead - `.\cam.recorder.ps1 -InstallAutostart`, `.\cam.ps1 -InstallDashboardAutostart` and `.\cam.ps1 -InstallUi` - and set `RestreamerEnabled` to false in `recorder.config.json`. Left true with no relay to start, the supervisor retries it on a backoff and logs the failure every time, and each camera spends `RelayReadyWaitSeconds` (15 s) on its first start waiting for a relay path that will never serve.

## cameras.json

```json
[
  {
    "Name": "Front Entrance",
    "IP": "10.0.1.15",
    "Quality": "stream1",
    "Record": true
  },
  {
    "Name": "Rear Balcony",
    "IP": "10.0.1.16",
    "Quality": "stream2"
  }
]
```

`Name` is what you see on the dashboard and what the footage folder is named after. `Quality` is the path on the camera: `stream1` is the main stream, `stream2` the substream on most firmware. `Record` is optional and defaults to false, which is why nothing records after an upgrade until you say so. Use `-Enable` and `-Disable` rather than editing it by hand; a running supervisor picks the change up within five seconds.

## Credentials

The first dashboard run prompts for the camera username and password and encrypts them to `camera_creds.xml` with Windows DPAPI, so only your Windows account on that machine can read them back. The recorder uses the same file and tells you to run the dashboard once if it is missing. To change them, delete `camera_creds.xml` and run the dashboard again.

Tapo cameras keep a **separate account for RTSP**, distinct from the TP-Link cloud login. Changing the cloud password does not affect RTSP.
