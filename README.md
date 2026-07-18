# Windows Terminal IPCCTV Dashboard

A robust, portable PowerShell script that creates a dynamic, self-healing grid of RTSP camera streams. By leveraging Windows Terminal and the `mpv` media player, this tool builds a lightweight, highly customizable security dashboard right on your desktop.

## Features

* **Dynamic Grid Layout:** No manual window positioning. The script mathematically calculates your screen real estate and automatically tiles the Windows Terminal panes to perfectly fit your active streams.
* **Self-Healing Streams:** Built-in `mpv` network timeouts and a PowerShell `while($true)` loop ensure that if a camera drops or the network stutters, the individual stream automatically restarts without breaking the whole dashboard.
* **Secure Credentials (DPAPI):** Say goodbye to hardcoded passwords. If the credentials file is missing, the script prompts for your camera credentials and securely encrypts them to a local XML file using Windows DPAPI. Only your Windows user account can decrypt them.
* **Portable JSON Configuration:** Add, remove, or modify cameras simply by editing a `cameras.json` file. The script dynamically adapts the grid to however many cameras you define.
* **Low Latency:** Uses optimized `mpv` flags specifically tuned for real-time RTSP streaming over TCP.

## Prerequisites

1. **Windows 10 or 11** with [Windows Terminal](https://apps.microsoft.com/store/detail/windows-terminal/9N0DX20HK701) installed.
2. **[mpv media player](https://mpv.io/)**: Download and extract the Windows build. Ensure you know the path to `mpv.com`.
3. **RTSP Compatible Cameras**: Works with any camera that outputs an RTSP stream (e.g., TP-Link Tapo, Amcrest, Reolink, Unifi).

## Installation

1. Clone this repository or download the script to a dedicated folder.
2. Open the script in a text editor and verify the `$MpvPath` variable points to your `mpv.com` executable (default is `C:\mpv\mpv.com`).
3. Run the script! 


## First-Run Setup Process

This script is designed to set itself up automatically the first time you run it.

**Step 1: Secure Credentials Prompt**
When you launch the script, it checks for a `camera_creds.xml` file. If it is missing, a standard Windows prompt will appear asking for your camera Username and Password. Enter them, and the script will immediately encrypt and save them locally.

**Step 2: The Config Template**
Next, the script checks for your camera list. If it cannot find a `cameras.json` file, it will automatically generate a template in the same folder and exit. 

Open the newly created `cameras.json` and update it with your actual camera details:

```json
[
  {
    "Name": "Front Entrance",
    "IP": "10.0.1.15",
    "Quality": "stream1"
  },
  {
    "Name": "Rear Balcony",
    "IP": "10.0.1.16",
    "Quality": "stream2"
  }
]

```

**Step 3: Launch**
Run the script again. It will automatically load your secure credentials, read your updated JSON file, calculate the grid, and launch your security dashboard.


## How to Run

**Right-click the script and select "Run with PowerShell" OR execute `.\mpv.cam.autolayout.autorestart.ps1` from a terminal.**


## Under the Hood

To prevent quoting and bracket parsing errors when passing complex `mpv` arguments into Windows Terminal splits, this script dynamically generates the PowerShell commands and converts them into Base64 `-EncodedCommand` strings before execution.

---

## Troubleshooting

* **Terminal flashes and closes immediately:** Ensure your `$MpvPath` is pointing specifically to `mpv.com` (the command-line executable), not `mpv.exe`.
* **Streams won't connect or show errors:** Double-check your camera IPs in `cameras.json`. Ensure your camera brand uses the `rtsp://user:pass@IP/stream` format (adjust the `$RTSPUrl` variable in the script if your manufacturer uses a different path structure).
* **Need to change passwords?** Delete `camera_creds.xml` from the script folder and run the script again to get a new credential prompt.
* **Script exits immediately after asking for config:** This is normal on the first run. Edit `cameras.json` with your details and run the script again.