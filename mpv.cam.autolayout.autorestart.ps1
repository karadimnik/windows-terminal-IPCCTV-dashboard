<#
.SYNOPSIS
    Tiles one borderless mpv window per camera across the screen in Windows
    Terminal, each in its own restart loop.

.DESCRIPTION
    Each pane prefers the local MediaMTX relay and falls back to a direct
    camera connection when the relay is not serving that camera. The choice is
    made inside the pane's own loop, per attempt, so a restreamer that dies is
    left behind on the next restart and one that comes back is picked up
    without relaunching the dashboard.

    The FIRST attempt is the one that needs help, because all six panes make it
    at once. This script measures the serving set ONCE and hands the same
    answer to every pane; a pane whose path is missing from it keeps asking for
    up to RelayReadyWaitSeconds before settling for a direct connection; and
    the panes are started StartStaggerMs apart. Six simultaneous one-shot
    probes with no retry is what left two panes on direct connections, each
    burning a camera slot the relay exists to give back.

    The camera URL is built inside the pane rather than passed to it, which
    keeps the credential out of the Windows Terminal and PowerShell command
    lines it would otherwise appear in.

    A strip is reserved along the bottom of the screen for cam.health.ps1,
    which is launched into a Windows Terminal window of its own. The strip is
    never a grid cell: the column and row counts come from the CAMERA COUNT
    ALONE, so six cameras are a 3x2 grid whether the strip is there or not.
    Counting the health window as a seventh stream would reshape the grid to
    3x3 and shrink every feed.

    Set HealthStripHeightPx to 0 in recorder.config.json to turn the strip off
    and get the original full-height layout back.

.PARAMETER PrintUrls
    Prints the input each camera would use right now, redacted, and the
    computed layout, then exits without opening any window.

.PARAMETER NoHealthStrip
    Launches the grid with the strip reserved but no health window in it.
#>
param(
    [switch]$PrintUrls,
    [switch]$NoHealthStrip
)

# 1. Define Paths and Settings
# mpv.com path (adjust if your mpv installation is elsewhere)
$MpvPath = "C:\mpv\mpv.com"


# Determine the directory where this script is currently located
$ScriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { $PWD.Path }

# Credential loading, JSON reading, name sanitisation, relay path naming,
# redaction and the relay-or-direct decision all live in the shared library,
# which is also what the recorder uses - one implementation, one behaviour.
$LibPath = Join-Path -Path $ScriptDir -ChildPath "cam.common.ps1"
. $LibPath

$CredPath = Join-Path -Path $ScriptDir -ChildPath "camera_creds.xml"

# Auto-Setup: If credentials don't exist, prompt and save them securely
if (-not (Test-Path $CredPath)) {
    Write-Host "=== First Time Setup ===" -ForegroundColor Cyan
    Write-Host "Please enter your camera username and password. This will be encrypted and saved locally." -ForegroundColor Yellow
    $NewCredential = Get-Credential
    $NewCredential | Export-Clixml -Path $CredPath
    Write-Host "Credentials successfully secured in $CredPath" -ForegroundColor Green
}

# Load and decrypt the credentials into memory. Get-CamCredential also
# registers them for redaction, so nothing printed below can leak them.
$Credential = Get-CamCredential -Path $CredPath

$JsonPath = Join-Path -Path $ScriptDir -ChildPath "cameras.json"

# Auto-Setup: If the camera config doesn't exist, create a template and exit
if (-not (Test-Path $JsonPath)) {
    Write-Host "=== Camera Config Missing ===" -ForegroundColor Cyan
    Write-Host "Creating a JSON template file at: $JsonPath" -ForegroundColor Yellow

    # Create an array of hashtables and convert it to nicely formatted JSON
    $TemplateArray = @(
        @{ Name = "Front Entrance"; IP = "10.0.0.15"; Quality = "stream1" },
        @{ Name = "Rear Balcony"; IP = "10.0.0.13"; Quality = "stream1" }
    )
    $TemplateArray | ConvertTo-Json -Depth 3 | Out-File -FilePath $JsonPath -Encoding UTF8

    Write-Host "Please open cameras.json, update it with your actual camera IPs, and run this script again." -ForegroundColor Green
    exit
}

$Config = Get-CamConfig

# Read the camera list and work out, per camera, the relay path name and the
# direct URL. Get-CamList is called without @() around it: it returns its array
# with a leading comma, and @() around a function call would nest that one level
# deeper and give a single element holding every camera.
$Streams = [ordered]@{}
foreach ($Camera in (Get-CamList -Path $JsonPath)) {
    $Streams[[string]$Camera.Name] = [pscustomobject]@{
        RelayPath = (Get-CamRelayPathName -SafeName (Get-CamSafeName -Name $Camera.Name))
        DirectUrl = (Get-CamRtspUrl -Camera $Camera -Credential $Credential)
    }
}

# The serving set, measured ONCE for all six panes. Six panes each asking the
# control API about their own path in the same instant is what put two of them
# on direct connections on 2026-10-11, on a launch where five of the six paths
# had just been confirmed serving: one flaked probe is one burnt camera slot.
# Every pane is handed this set and only asks for itself if its own path is
# missing from it.
$ReadyPaths = @()
if (Get-CamRelayEnabled -Config $Config) { $ReadyPaths = @(Get-CamRelayReadyPaths -Config $Config) }

# What each pane would use if it started now. A snapshot, not a promise: every
# pane re-decides on every attempt of its own loop.
Write-Host ""
Write-Host "=== camera inputs ===" -ForegroundColor Cyan
foreach ($Stream in $Streams.GetEnumerator()) {
    $Choice = Get-CamInputUrl -Config $Config -RelayPathName $Stream.Value.RelayPath `
        -DirectUrl $Stream.Value.DirectUrl -ReadyPaths $ReadyPaths
    Write-Host ("  {0,-20} {1,-7} {2}" -f $Stream.Key, $Choice.Mode, (Get-CamRedacted -Text $Choice.Url))
}
Write-Host ("  relay serving {0} path(s): {1}" -f $ReadyPaths.Count, (($ReadyPaths | Sort-Object) -join ', '))
$RelayReadyWait = [int]$Config.RelayReadyWaitSeconds
$StaggerMs = [int]$Config.StartStaggerMs
Write-Host ("  a pane whose path is not in that set waits up to {0}s for it, then takes a direct slot; panes start {1} ms apart" -f `
    $RelayReadyWait, $StaggerMs)
Write-Host ""

# 3. Screen real estate and the reserved strip
#
# $Streams holds CAMERAS ONLY. The health window is placed into a reserved
# band afterwards and is never added here: $Streams.Count drives --geometry,
# so a seventh entry would turn a 3x2 grid into 3x3 and shrink every feed.
Add-Type -AssemblyName System.Windows.Forms
$Screen = [System.Windows.Forms.Screen]::PrimaryScreen.WorkingArea

$Layout = Get-CamDashboardLayout -Config $Config -CameraCount $Streams.Count `
    -ScreenWidth $Screen.Width -ScreenHeight $Screen.Height `
    -ScreenLeft $Screen.Left -ScreenTop $Screen.Top

Write-Host "=== layout ===" -ForegroundColor Cyan
Write-Host ("  screen       {0}x{1} at +{2}+{3} (working area)" -f $Screen.Width, $Screen.Height, $Screen.Left, $Screen.Top)
Write-Host ("  cameras      {0} -> {1} column(s) x {2} row(s)" -f $Streams.Count, $Layout.Columns, $Layout.Rows)
Write-Host ("  pane         {0}x{1}, gutter {2} px between rows only" -f $Layout.PaneWidth, $Layout.PaneHeight, $Layout.GutterPx)
Write-Host ("  grid         {0} px tall, rows at y {1}" -f $Layout.GridHeight, `
    ((0..($Layout.Rows - 1) | ForEach-Object { $Screen.Top + ($_ * ($Layout.PaneHeight + $Layout.GutterPx)) }) -join ', '))
if ($Layout.StripEnabled) {
    Write-Host ("  health strip {0}x{1} at +{2}+{3} (mode {4})" -f `
        $Layout.StripWidth, $Layout.StripHeight, $Layout.StripLeft, $Layout.StripTop, $Layout.Mode) -ForegroundColor Green
}
else {
    Write-Host '  health strip off - full-height panes, no health window' -ForegroundColor Yellow
}
if ($Layout.Note) { Write-Host ("  note         {0}" -f $Layout.Note) -ForegroundColor Yellow }
Write-Host ""

if ($PrintUrls) {
    Write-Host "-PrintUrls: nothing launched." -ForegroundColor Yellow
    exit
}

# Health strip window
# -------------------
# The strip is its own Windows Terminal window, not a pane of the grid, so the
# camera geometry can never be affected by it.
#
# Windows Terminal's --size is in CHARACTER CELLS, not pixels, so it cannot be
# asked for an exact pixel height. The window is placed with --pos, given a
# rough cell size so it does not flash up over the grid, and then sized
# exactly with SetWindowPos.

$script:CamWinLoaded = $false

function Import-CamWindowApi {
    <#
        The few user32/dwmapi calls needed to place one window by pixel.

        FindByTitle enumerates top-level windows rather than reading
        Process.MainWindowTitle, because Windows Terminal serves several
        windows from ONE process: MainWindowHandle is then whichever window
        Windows enumerates first, not the one whose title matched, and killing
        the process would take every terminal window on the desktop with it.
    #>

    if ($script:CamWinLoaded) { return }
    Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

public static class CamWin
{
    private delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr p);
    [DllImport("user32.dll")] private static extern bool EnumWindows(EnumWindowsProc cb, IntPtr p);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] private static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll")] private static extern int GetWindowTextLength(IntPtr h);
    [DllImport("user32.dll")] private static extern bool IsWindowVisible(IntPtr h);
    [DllImport("user32.dll", SetLastError = true)] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
    [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll", SetLastError = true)] public static extern bool PostMessage(IntPtr h, uint msg, IntPtr w, IntPtr l);
    [DllImport("dwmapi.dll")] public static extern int DwmGetWindowAttribute(IntPtr h, int attr, out RECT r, int size);

    [StructLayout(LayoutKind.Sequential)]
    public struct RECT { public int Left; public int Top; public int Right; public int Bottom; }

    public static IntPtr[] FindByTitle(string title)
    {
        List<IntPtr> found = new List<IntPtr>();
        EnumWindows(delegate(IntPtr h, IntPtr p) {
            if (!IsWindowVisible(h)) { return true; }
            int len = GetWindowTextLength(h);
            if (len == 0) { return true; }
            StringBuilder sb = new StringBuilder(len + 2);
            GetWindowText(h, sb, sb.Capacity);
            if (string.Equals(sb.ToString(), title, StringComparison.Ordinal)) { found.Add(h); }
            return true;
        }, IntPtr.Zero);
        return found.ToArray();
    }
}
'@
    $script:CamWinLoaded = $true
}


function Get-CamWindowFrame {
    <#
        A window's rect and its VISIBLE frame.

        They are not the same: a resizable window's rect includes an invisible
        DWM resize border, measured at 7 px on each side and the bottom here,
        so sizing the window rect to the strip would leave a visible gap along
        the bottom of the screen. DWMWA_EXTENDED_FRAME_BOUNDS is 9.
    #>
    param([Parameter(Mandatory = $true)][IntPtr]$Hwnd)

    $windowRect = New-Object CamWin+RECT
    [void][CamWin]::GetWindowRect($Hwnd, [ref]$windowRect)
    $frameRect = New-Object CamWin+RECT
    $hr = [CamWin]::DwmGetWindowAttribute($Hwnd, 9, [ref]$frameRect,
        [System.Runtime.InteropServices.Marshal]::SizeOf($frameRect))
    if ($hr -ne 0) { $frameRect = $windowRect }

    return [pscustomobject]@{
        Window = $windowRect
        Frame  = $frameRect
        Left   = $frameRect.Left
        Top    = $frameRect.Top
        Width  = ($frameRect.Right - $frameRect.Left)
        Height = ($frameRect.Bottom - $frameRect.Top)
    }
}


function Set-CamWindowFrame {
    <#
        Moves a window so its VISIBLE frame lands on the given pixel rect.

        Iterates on the measured error rather than assuming a border width, so
        a different theme, DPI or Windows version corrects itself.

        The window rect is clamped to the right edge of the screen. Measured:
        Windows Terminal mis-sizes itself when the window rect runs past that
        edge - a request for 1934x303 at x=-7 came back as 1936x563. The width
        is what gets clamped, not the position, so no text is pushed off the
        left edge of the screen.
    #>
    param(
        [Parameter(Mandatory = $true)][IntPtr]$Hwnd,
        [Parameter(Mandatory = $true)][int]$X,
        [Parameter(Mandatory = $true)][int]$Y,
        [Parameter(Mandatory = $true)][int]$Width,
        [Parameter(Mandatory = $true)][int]$Height,
        [Parameter(Mandatory = $true)][int]$ScreenRight
    )

    # SWP_NOACTIVATE 0x0010 | SWP_SHOWWINDOW 0x0040. SWP_NOZORDER is left out
    # and hwndInsertAfter is HWND_TOP, so the strip comes up above the grid's
    # own terminal window without taking focus.
    $flags = 0x0050
    $measured = $null

    for ($pass = 1; $pass -le 3; $pass++) {
        $current = Get-CamWindowFrame -Hwnd $Hwnd
        $insetLeft = $current.Frame.Left - $current.Window.Left
        $insetTop = $current.Frame.Top - $current.Window.Top
        $insetRight = $current.Window.Right - $current.Frame.Right
        $insetBottom = $current.Window.Bottom - $current.Frame.Bottom

        $windowX = $X - $insetLeft
        $windowY = $Y - $insetTop
        $windowWidth = $Width + $insetLeft + $insetRight
        $windowHeight = $Height + $insetTop + $insetBottom
        if (($windowX + $windowWidth) -gt $ScreenRight) { $windowWidth = $ScreenRight - $windowX }

        [void][CamWin]::SetWindowPos($Hwnd, [IntPtr]::Zero, $windowX, $windowY, $windowWidth, $windowHeight, $flags)
        Start-Sleep -Milliseconds 500

        $measured = Get-CamWindowFrame -Hwnd $Hwnd
        if ($measured.Left -eq $X -and $measured.Top -eq $Y -and
            $measured.Width -eq $Width -and $measured.Height -eq $Height) { break }
    }

    return $measured
}


function Stop-CamHealthWindow {
    <#
        Closes any existing strip window, so relaunching the dashboard replaces
        it instead of leaving two.

        WM_CLOSE to that one window. Never Stop-Process: one Windows Terminal
        process can own several windows, including the Operator's own.
    #>
    Import-CamWindowApi

    $existing = @([CamWin]::FindByTitle('cam.health'))
    if ($existing.Count -eq 0) { return 0 }

    foreach ($hwnd in $existing) { [void][CamWin]::PostMessage($hwnd, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero) }
    for ($i = 0; $i -lt 10; $i++) {
        Start-Sleep -Milliseconds 400
        if (@([CamWin]::FindByTitle('cam.health')).Count -eq 0) { break }
    }
    return $existing.Count
}


function Start-CamHealthWindow {
    <#
        Launches cam.health.ps1 into its own named Windows Terminal window and
        sizes that window to the reserved strip.

        The renderer is given no -Rows or -Width: it reads its real row and
        column count from the console it ends up in, which is the only way to
        know them once the window has been sized in pixels.
    #>
    param(
        [Parameter(Mandatory = $true)]$Layout,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$ScriptDir,
        [Parameter(Mandatory = $true)][int]$ScreenRight
    )

    $healthPath = Join-Path -Path $ScriptDir -ChildPath 'cam.health.ps1'
    if (-not (Test-Path -LiteralPath $healthPath)) {
        Write-Host ("no health strip: {0} is missing" -f $healthPath) -ForegroundColor Yellow
        return
    }

    Import-CamWindowApi
    $closed = Stop-CamHealthWindow
    if ($closed -gt 0) { Write-Host ("closed {0} earlier health window(s)" -f $closed) -ForegroundColor Yellow }

    # The title is set here as well as inside cam.health.ps1, so the window can
    # still be found and placed if the renderer fails on startup.
    $template = @'
$Host.UI.RawUI.WindowTitle = 'cam.health'
& '__HEALTH__'
'@
    $script = $template.Replace('__HEALTH__', $healthPath.Replace("'", "''"))
    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($script))

    # Cells, not pixels - a launch hint only. The exact size comes from
    # SetWindowPos below.
    $hintColumns = [int][math]::Max(40, [math]::Floor($Layout.StripWidth / [double]$Config.HealthCellWidthPx))
    $hintRows = [int][math]::Max(4, [math]::Floor($Layout.StripHeight / [double]$Config.HealthCellHeightPx))

    # --focus drops the tab bar, which is dead weight on a one-pane status
    # strip. Measured: the same 1913x296 window carries 15 text rows with it
    # and 13 without, so the flag is worth two rows of the Operator's budget.
    & wt.exe -w cctv-health --focus --pos ("{0},{1}" -f $Layout.StripLeft, $Layout.StripTop) `
        --size ("{0},{1}" -f $hintColumns, $hintRows) `
        powershell.exe -NoExit -EncodedCommand $encoded

    $hwnd = [IntPtr]::Zero
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 400
        $found = @([CamWin]::FindByTitle('cam.health'))
        if ($found.Count -ge 1) { $hwnd = $found[0]; break }
    }
    if ($hwnd -eq [IntPtr]::Zero) {
        Write-Host 'the health window did not appear within 16 s, so it was not placed' -ForegroundColor Yellow
        return
    }

    $measured = Set-CamWindowFrame -Hwnd $hwnd -X $Layout.StripLeft -Y $Layout.StripTop `
        -Width $Layout.StripWidth -Height $Layout.StripHeight -ScreenRight $ScreenRight

    $wanted = ("{0}x{1} at +{2}+{3}" -f $Layout.StripWidth, $Layout.StripHeight, $Layout.StripLeft, $Layout.StripTop)
    $got = ("{0}x{1} at +{2}+{3}" -f $measured.Width, $measured.Height, $measured.Left, $measured.Top)
    if ($wanted -eq $got) {
        Write-Host ("health strip placed at {0}" -f $got) -ForegroundColor Green
    }
    else {
        Write-Host ("health strip wanted {0}, measured {1}" -f $wanted, $got) -ForegroundColor Yellow
    }
}


# 2. Define the exact text overlay & flags
$OverlayOpts = "--osd-level=1 --osd-msg1='Traffic: `${video-bitrate}'"

# A frozen or dropped stream is caught by two timeouts, and neither of them is
# stimeout. libavformat renamed stimeout to timeout at ffmpeg 5.0, mpv ships
# avformat-63.dll, and its RTSP demuxer has no -stimeout option at all - passing
# it was silently dropped, with the connection using only timeout=5000000. So the
# protection is --demuxer-lavf-o=timeout=5000000 (5 s socket timeout, microseconds)
# plus mpv's own --network-timeout=3; when they fire, mpv exits and the restart
# loop below reconnects.
#
# --msg-level=all=error keeps mpv's routine chatter (file tags, the VO line) out of
# the gutter between the grid rows, which shows the terminal window behind the
# feeds. Real errors still print, which --really-quiet would also have hidden.
$Flags = "--no-border --no-audio --profile=low-latency --rtsp-transport=tcp --correct-pts=no --display-fps-override=1 --network-timeout=3 --demuxer-lavf-o=timeout=5000000 --msg-level=all=error --force-window=immediate"

# The grid shape and pane size, from the camera count alone.
$TotalStreams = $Streams.Count
$Columns = $Layout.Columns
$Rows = $Layout.Rows
$WindowWidth = $Layout.PaneWidth
$WindowHeight = $Layout.PaneHeight

# 4. Build MPV commands and encode them for absolute safety
#
# What each pane is told about the relay: the serving set this script measured
# once, rendered as a literal array; how long it may keep asking about its own
# path if that set does not have it; and how far into the stagger it sits.
$ReadyPathsLiteral = '@()'
if ($ReadyPaths.Count -gt 0) {
    $ReadyPathsLiteral = '@(' + (($ReadyPaths | ForEach-Object { "'" + ([string]$_).Replace("'", "''") + "'" }) -join ', ') + ')'
}

$EncodedCommands = @()
$i = 0

foreach ($Stream in $Streams.GetEnumerator()) {
    # Grid position and exact screen coordinates. The row pitch is the pane
    # height plus the gutter, so the gutter falls BETWEEN the rows and never
    # at the top or bottom edge of the grid.
    $Position = Get-CamPanePosition -Layout $Layout -Index $i -ScreenLeft $Screen.Left -ScreenTop $Screen.Top
    $Col = $Position.Column
    $Row = $Position.Row
    $X = $Position.X
    $Y = $Position.Y

    $Geometry = "--geometry=${WindowWidth}x${WindowHeight}+$X+$Y"

    # The pane's own script. It loads the shared library and builds its URL
    # itself, so the credential never appears in a command line, and it decides
    # between the relay and the camera on every iteration rather than once.
    # Built by substitution into a single-quoted template: nothing in it is
    # interpolated by this script except the placeholders.
    $PaneTemplate = @'
$ErrorActionPreference = 'Stop'
. '__LIB__'
$camName = '__NAME__'
$config = Get-CamConfig
$credential = Get-CamCredential
$camera = $null
foreach ($candidate in (Get-CamList)) {
    if ([string]$candidate.Name -eq $camName) { $camera = $candidate; break }
}
if (-not $camera) { throw "Camera '$camName' is no longer in cameras.json." }
$relayPath = Get-CamRelayPathName -SafeName (Get-CamSafeName -Name $camera.Name)
$directUrl = Get-CamRtspUrl -Camera $camera -Credential $credential

# This pane's place in the stagger, so six panes do not open six RTSP sessions
# and six control-API probes in the same instant.
Start-Sleep -Milliseconds __STAGGERMS__

# The serving set the dashboard measured ONCE for every pane, and the budget
# for asking about this one path if it is missing from that set. Both belong to
# the FIRST attempt only: from the second iteration on, the loop decides on the
# spot with no wait, which is what leaves a relay that died behind and picks one
# that came back up.
$readyPaths = __READYPATHS__
$waitSeconds = __WAITSECONDS__

# mpv reports progress on stderr, which would be a terminating error under Stop.
$ErrorActionPreference = 'Continue'
while ($true) {
    if ($null -ne $readyPaths) {
        $choice = Get-CamInputUrl -Config $config -RelayPathName $relayPath -DirectUrl $directUrl `
            -ReadyPaths $readyPaths -WaitSeconds $waitSeconds
        if ($choice.Mode -eq 'direct' -and $choice.WaitedSeconds -gt 0) {
            Write-Host ("{0} {1}: the relay never served '{2}' in {3}s, taking a direct camera slot" -f (Get-Date -Format 'HH:mm:ss'), $camName, $relayPath, [int]$choice.WaitedSeconds) -ForegroundColor Yellow
        }
        $readyPaths = $null
        $waitSeconds = 0
    }
    else {
        $choice = Get-CamInputUrl -Config $config -RelayPathName $relayPath -DirectUrl $directUrl
    }
    Write-Host ("{0} {1} on the {2} input: {3}" -f (Get-Date -Format 'HH:mm:ss'), $camName, $choice.Mode, (Get-CamRedacted -Text $choice.Url)) -ForegroundColor Cyan
    & '__MPV__' $choice.Url __FLAGS__
    Write-Host 'Stream interrupted. Restarting in 3 seconds...' -ForegroundColor Yellow
    Start-Sleep -Seconds 3
}
'@

    $ScriptBlock = $PaneTemplate.Replace('__LIB__', $LibPath.Replace("'", "''"))
    $ScriptBlock = $ScriptBlock.Replace('__NAME__', ([string]$Stream.Key).Replace("'", "''"))
    $ScriptBlock = $ScriptBlock.Replace('__MPV__', $MpvPath.Replace("'", "''"))
    $ScriptBlock = $ScriptBlock.Replace('__FLAGS__', "$Flags $OverlayOpts $Geometry")
    $ScriptBlock = $ScriptBlock.Replace('__READYPATHS__', $ReadyPathsLiteral)
    $ScriptBlock = $ScriptBlock.Replace('__WAITSECONDS__', $RelayReadyWait.ToString([cultureinfo]::InvariantCulture))
    $ScriptBlock = $ScriptBlock.Replace('__STAGGERMS__', ($i * $StaggerMs).ToString([cultureinfo]::InvariantCulture))
    
    # Encode to Base64 to completely bypass Windows Terminal quoting/bracket errors
    $Bytes = [System.Text.Encoding]::Unicode.GetBytes($ScriptBlock)
    $EncodedCmd = [Convert]::ToBase64String($Bytes)
    
    $EncodedCommands += $EncodedCmd
    $i++
}

# 5. Build Windows Terminal argument array using the encoded commands
$WtArgs = @("new-tab", "powershell.exe", "-NoExit", "-EncodedCommand", $EncodedCommands[0])

for ($j = 1; $j -lt $EncodedCommands.Count; $j++) {
    $WtArgs += ";"
    $Col = $j % $Columns
    $Row = [math]::Floor($j / $Columns)

    if ($j -lt $Columns) {
        # STEP A: Building the very first row. We split vertically.
        $SizeVal = ($Columns - $j) / ($Columns - $j + 1)
        $SizeStr = [math]::Round($SizeVal, 4).ToString([cultureinfo]::InvariantCulture)

        $WtArgs += "split-pane", "-V", "-s", $SizeStr, "powershell.exe", "-NoExit", "-EncodedCommand", $EncodedCommands[$j]
    }
    else {
        # STEP B: Building subsequent rows underneath.
        if ($Col -eq 0) {
            # Start of a new row: jump focus all the way back to the far-left column
            for ($m = 0; $m -lt ($Columns - 1); $m++) {
                $WtArgs += "move-focus", "left", ";"
            }
        } else {
            # Middle of a row: just shift focus one pane to the right
            $WtArgs += "move-focus", "right", ";"
        }
        
        # Calculate what percentage the new bottom pane needs to ensure equal rows
        $SizeVal = ($Rows - $Row) / ($Rows - $Row + 1)
        $SizeStr = [math]::Round($SizeVal, 4).ToString([cultureinfo]::InvariantCulture)

        # Split downward using the encoded command
        $WtArgs += "split-pane", "-H", "-s", $SizeStr, "powershell.exe", "-NoExit", "-EncodedCommand", $EncodedCommands[$j]
    }
}

# 6. Execute Windows Terminal directly
& wt.exe $WtArgs

# 7. The health strip, into its own window, after the grid so it comes up on
#    top of the grid's terminal window rather than under it.
if ($Layout.StripEnabled -and -not $NoHealthStrip) {
    Start-CamHealthWindow -Layout $Layout -Config $Config -ScriptDir $ScriptDir `
        -ScreenRight ($Screen.Left + $Screen.Width)
}
elseif ($NoHealthStrip) {
    Write-Host '-NoHealthStrip: the strip is reserved but empty.' -ForegroundColor Yellow
}