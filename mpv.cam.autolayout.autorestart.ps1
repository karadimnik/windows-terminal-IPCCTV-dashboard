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

# Anything left of a previous dashboard goes BEFORE the relay is measured.
#
# Relaunching used to leave the old run behind: six pane processes survive the
# window being closed, and six stale cam.health processes had accumulated
# windowless and invisible by the end of 2026-10-12. Each old pane still holds
# the second of its camera's two RTSP sessions, which is also the session
# MediaMTX needs to pull that camera - so a stale pane does not just waste a
# slot, it keeps the relay from recovering the path this launch is about to
# measure. Stopping first is what makes the measurement below mean anything.
#
# -PrintUrls must not touch a running dashboard: it answers a question.
if (-not $PrintUrls) {
    Write-Host '=== stopping any previous dashboard ===' -ForegroundColor Cyan
    [void](Stop-CamDashboard -Config $Config)
    Write-Host ''
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
    $closed = Stop-CamHealthStrip -Config $Config
    if ($closed -gt 0) { Write-Host ("stopped {0} earlier health strip(s)" -f $closed) -ForegroundColor Yellow }

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
    # --suppressApplicationTitle keeps this window findable. Without it the
    # title reverts to the shell default the moment the renderer exits, which
    # is how six stale strips went unnoticed - see Stop-CamHealthStrip.
    # --pos and --size are GLOBAL options and come before the subcommand;
    # --title and --suppressApplicationTitle belong to new-tab and have to go
    # after it. Measured: with the two title flags up among the global options
    # wt.exe parsed nothing and opened no window at all, silently, so the
    # subcommand is written out rather than left implicit.
    & wt.exe -w cctv-health --focus `
        --pos ("{0},{1}" -f $Layout.StripLeft, $Layout.StripTop) `
        --size ("{0},{1}" -f $hintColumns, $hintRows) `
        new-tab --title 'cam.health' --suppressApplicationTitle `
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
    #
    # KEEP THIS TEMPLATE SHORT, and comment it here rather than inside it.
    # Every character is UTF-16 encoded and base64'd, which costs 2.67 bytes of
    # ONE shared Windows command line, six times over - and that line cannot
    # exceed 32767 characters. Measured: five extra comment lines in here took
    # the wt.exe invocation to 42040 characters, at which point nothing
    # launched at all and nothing said why. The guard below the loop is what
    # turns that back into an error message.
    #
    # Add-CamJobMember is the one line that makes closing this window stop
    # this pane. It puts the pane into the Win32 job the warden owns, before
    # mpv is ever started so mpv inherits the membership, and the kernel kills
    # the lot when the warden lets its handle go. Sixteen characters here
    # rather than a watchdog inside the loop because a pane CANNOT see its own
    # window close: measured on 2026-10-12, the console survives it intact and
    # every console call still succeeds, and in any case the loop is blocked
    # inside mpv for hours at a time.
    #
    # What the three batch-startup placeholders do:
    #   __STAGGERMS__   this pane's place in the launch stagger, so six panes
    #                   do not open six RTSP sessions and fire six control-API
    #                   probes in the same instant.
    #   __READYPATHS__  @('<this path>') when the dashboard measured this
    #                   camera's path serving just now, @() when it did not. A
    #                   pane only ever asks about its own path, so one shared
    #                   measurement is all it needs and no probe is made.
    #   __WAITSECONDS__ how long the pane keeps asking for its own path when
    #                   that measurement was @().
    # All three apply to the FIRST attempt only. From the second iteration the
    # loop decides on the spot with no wait, which is what leaves a relay that
    # died behind and picks up one that came back.
    #
    # Why the mpv call is Invoke-CamViewer and not the call operator any more:
    # the loop only re-decides when mpv exits, and mpv on a healthy DIRECT
    # stream never exits, so a pane that fell back during a relay outage stayed
    # on direct - holding the second of that camera's two RTSP sessions - until
    # the whole dashboard was relaunched. Invoke-CamViewer runs mpv as a child
    # and, only when this attempt chose direct, asks every
    # ViewerPromoteCheckSeconds whether this one path is serving again; when it
    # is, it stops mpv and returns $true so the loop decides again immediately
    # instead of printing an interruption and sleeping. The cost is one brief
    # blink in that pane, once per outage. A pane already on the relay is
    # unchanged and watches nothing: if the relay dies mpv exits by itself.
    #
    # $mpvArgs is the flag text tokenised by PowerShell's own argument parser -
    # & { $args } hands back exactly what the call operator used to pass - so
    # the arguments mpv receives are unchanged. It is hoisted out of the loop
    # because it never varies, and it is what let the stderr comment that used
    # to sit here go: Invoke-CamViewer does not capture mpv's output, so it can
    # no longer turn into a NativeCommandError, though Continue is still set
    # for the rest of the loop.
    $PaneTemplate = @'
$ErrorActionPreference = 'Stop'
. '__LIB__'
Add-CamJobMember
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
Start-Sleep -Milliseconds __STAGGERMS__
$batch = @{ ReadyPaths = __READYPATHS__; WaitSeconds = __WAITSECONDS__ }
$mpvArgs = @(& { $args } __FLAGS__)
$ErrorActionPreference = 'Continue'
while ($true) {
    $choice = Get-CamInputUrl -Config $config -RelayPathName $relayPath -DirectUrl $directUrl @batch
    $batch = @{}
    $waited = ''
    if ($choice.WaitedSeconds -gt 0) { $waited = ' after waiting {0}s' -f [int]$choice.WaitedSeconds }
    Write-Host ("{0} {1} on the {2} input{3}: {4}" -f (Get-Date -Format 'HH:mm:ss'), $camName, $choice.Mode, $waited, (Get-CamRedacted -Text $choice.Url)) -ForegroundColor Cyan
    if (Invoke-CamViewer -Config $config -Choice $choice -RelayPathName $relayPath -MpvPath '__MPV__' -MpvArgs $mpvArgs) { continue }
    Write-Host 'Stream interrupted. Restarting in 3 seconds...' -ForegroundColor Yellow
    Start-Sleep -Seconds 3
}
'@

    $ScriptBlock = $PaneTemplate.Replace('__LIB__', $LibPath.Replace("'", "''"))
    $ScriptBlock = $ScriptBlock.Replace('__NAME__', ([string]$Stream.Key).Replace("'", "''"))
    $ScriptBlock = $ScriptBlock.Replace('__MPV__', $MpvPath.Replace("'", "''"))
    $ScriptBlock = $ScriptBlock.Replace('__FLAGS__', "$Flags $OverlayOpts $Geometry")
    # A pane only ever asks about its own path, so the shared measurement
    # reaches it as one name or as nothing at all. Rendered as a literal rather
    # than passed as data because the whole pane script travels as one
    # base64 blob on the wt.exe command line.
    $ServingLiteral = '@()'
    if ($ReadyPaths -contains $Stream.Value.RelayPath) {
        $ServingLiteral = "@('" + ([string]$Stream.Value.RelayPath).Replace("'", "''") + "')"
    }
    $ScriptBlock = $ScriptBlock.Replace('__READYPATHS__', $ServingLiteral)
    $ScriptBlock = $ScriptBlock.Replace('__WAITSECONDS__', $RelayReadyWait.ToString([cultureinfo]::InvariantCulture))
    $ScriptBlock = $ScriptBlock.Replace('__STAGGERMS__', ($i * $StaggerMs).ToString([cultureinfo]::InvariantCulture))
    
    # Encode to Base64 to completely bypass Windows Terminal quoting/bracket errors
    $Bytes = [System.Text.Encoding]::Unicode.GetBytes($ScriptBlock)
    $EncodedCmd = [Convert]::ToBase64String($Bytes)
    
    $EncodedCommands += $EncodedCmd
    $i++
}

# 5. Build Windows Terminal argument array using the encoded commands
#
# -w cctv-grid gives the grid a window of its OWN. Without it the dashboard
# obeys the user's windowingBehavior setting, which is "useExisting" on this
# machine, and the six panes arrive as a TAB inside whatever terminal window
# was last used - the Operator's own included. A dashboard that cannot be
# closed without closing his shell is not a dashboard.
#
# --title plus --suppressApplicationTitle on EVERY pane is what gives the
# window a stable name for the warden to watch. Measured on 2026-10-12:
# --title alone is overwritten by the shell's own title, and the window title
# then follows whichever pane has focus, so it changed on every move-focus.
# With the application title suppressed it stays put.
# Every pane, not just the first. The window title follows whichever pane has
# focus, so one untitled pane out of six is enough to lose the window: measured
# with the flags on new-tab alone, the grid came up titled "powershell.exe" and
# the warden sat waiting for a window that never appeared.
$PaneTitleArgs = @("--title", (Get-CamDashboardWindowTitle), "--suppressApplicationTitle")

$WtArgs = @("-w", "cctv-grid", "new-tab") + $PaneTitleArgs +
    @("powershell.exe", "-NoExit", "-EncodedCommand", $EncodedCommands[0])

for ($j = 1; $j -lt $EncodedCommands.Count; $j++) {
    $WtArgs += ";"
    $Col = $j % $Columns
    $Row = [math]::Floor($j / $Columns)

    if ($j -lt $Columns) {
        # STEP A: Building the very first row. We split vertically.
        $SizeVal = ($Columns - $j) / ($Columns - $j + 1)
        $SizeStr = [math]::Round($SizeVal, 4).ToString([cultureinfo]::InvariantCulture)

        $WtArgs += @("split-pane", "-V", "-s", $SizeStr) + $PaneTitleArgs +
            @("powershell.exe", "-NoExit", "-EncodedCommand", $EncodedCommands[$j])
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
        $WtArgs += @("split-pane", "-H", "-s", $SizeStr) + $PaneTitleArgs +
            @("powershell.exe", "-NoExit", "-EncodedCommand", $EncodedCommands[$j])
    }
}

# 6. Execute Windows Terminal directly
#
# Every pane travels inside this ONE command line, and Windows refuses to
# create a process from more than 32767 characters of it. Over that limit
# wt.exe does not start, does not complain and leaves no window, so the
# dashboard simply never appears - measured at 42040 characters, which is what
# five extra comment lines in the pane template cost once base64 had tripled
# them six times over. An explicit limit is the difference between a diagnosis
# and a mystery.
$CommandLineChars = ($WtArgs | Measure-Object -Property Length -Sum).Sum + $WtArgs.Count + 7
Write-Host ("wt.exe command line: {0} of the 32767 characters Windows allows, {1} pane(s)" -f `
    $CommandLineChars, $EncodedCommands.Count)
if ($CommandLineChars -gt 32000) {
    throw ("The pane commands need {0} characters of command line and Windows allows 32767. Shorten the pane template in this script - every character in it costs 2.67 bytes per pane - or reduce the camera count." -f $CommandLineChars)
}

# The warden starts BEFORE wt.exe, because a pane joins the job as its first
# act and the job has to exist by then - a pane that joined late would already
# have an mpv outside the job, and that mpv is what holds the camera session.
#
# It is a detached, windowless process holding the only handle to the job. It
# waits for the window titled cam.dashboard to appear, then polls it once a
# second; when the window goes, so does the warden, the last handle closes and
# JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE terminates every pane and every mpv at
# once. If the window never appears it DISARMS the job and leaves, so a launch
# that failed elsewhere cannot have its panes killed by a warden that simply
# never found what it was watching.
$WardenScript = ". '{0}'`nStart-CamDashboardWarden -LogPath '{1}'" -f `
    $LibPath.Replace("'", "''"), `
    (Join-Path -Path (Get-CamStateDirPath -Config $Config) -ChildPath 'dashboard-warden.log').Replace("'", "''")
#
# Started through Start-CamDetachedProcess, not Start-Process -WindowStyle
# Hidden: the latter leaves the warden attached to the console of whatever
# terminal this script was run from, so closing that terminal would deliver
# CTRL_CLOSE_EVENT to the warden, close the last handle to the job and kill
# every pane and every mpv. See the measurements in Start-CamDetachedProcess.
$Warden = Start-CamDetachedProcess -Script $WardenScript

Import-CamJobApi
$JobName = Get-CamDashboardJobName
$JobReady = $false
for ($Attempt = 0; $Attempt -lt 40; $Attempt++) {
    if ([CamJob]::Exists($JobName)) { $JobReady = $true; break }
    Start-Sleep -Milliseconds 250
}
if ($JobReady) {
    Write-Host ("warden pid {0} owns the job '{1}'; closing the dashboard window will stop the panes" -f `
        $Warden.Id, $JobName) -ForegroundColor Green
}
else {
    Write-Host ("the warden (pid {0}) did not create the job '{1}' within 10s. The dashboard will still open, but closing its window will NOT stop the panes - use cam.ps1 -Stop." -f `
        $Warden.Id, $JobName) -ForegroundColor Yellow
}

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