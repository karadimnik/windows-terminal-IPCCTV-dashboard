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

    The camera URL is built inside the pane rather than passed to it, which
    keeps the credential out of the Windows Terminal and PowerShell command
    lines it would otherwise appear in.

.PARAMETER PrintUrls
    Prints the input each camera would use right now, redacted, and exits
    without opening any window.
#>
param([switch]$PrintUrls)

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

# What each pane would use if it started now. A snapshot, not a promise: every
# pane re-decides on every attempt of its own loop.
Write-Host ""
Write-Host "=== camera inputs ===" -ForegroundColor Cyan
foreach ($Stream in $Streams.GetEnumerator()) {
    $Choice = Get-CamInputUrl -Config $Config -RelayPathName $Stream.Value.RelayPath -DirectUrl $Stream.Value.DirectUrl
    Write-Host ("  {0,-20} {1,-7} {2}" -f $Stream.Key, $Choice.Mode, (Get-CamRedacted -Text $Choice.Url))
}
Write-Host ""

if ($PrintUrls) {
    Write-Host "-PrintUrls: nothing launched." -ForegroundColor Yellow
    exit
}

# 2. Define the exact text overlay & flags
$OverlayOpts = "--osd-level=1 --osd-msg1='Traffic: `${video-bitrate}'"

# Added stimeout=10000000 (10 seconds) to force MPV to exit if the stream freezes/drops
$Flags = "--no-border --no-audio --profile=low-latency --rtsp-transport=tcp --correct-pts=no --display-fps-override=1 --network-timeout=3 --demuxer-lavf-o=timeout=5000000,stimeout=6000000 --force-window=immediate"

# 3. Dynamically Calculate Screen Real Estate
Add-Type -AssemblyName System.Windows.Forms
$Screen = [System.Windows.Forms.Screen]::PrimaryScreen.WorkingArea

# Determine optimal grid size based on the number of active streams
$TotalStreams = $Streams.Count
$Columns = [math]::Ceiling([math]::Sqrt($TotalStreams))
$Rows = [math]::Ceiling($TotalStreams / $Columns)

# Calculate exact pixel dimensions for each window to tile perfectly
$WindowWidth = [math]::Floor($Screen.Width / $Columns)
$WindowHeight = [math]::Floor($Screen.Height / $Rows)

# 4. Build MPV commands and encode them for absolute safety
$EncodedCommands = @()
$i = 0

foreach ($Stream in $Streams.GetEnumerator()) {
    # Calculate grid position dynamically
    $Col = $i % $Columns
    $Row = [math]::Floor($i / $Columns)

    # Calculate exact screen coordinates
    $X = $Screen.Left + ($Col * $WindowWidth)
    $Y = $Screen.Top + ($Row * $WindowHeight)

    $Geometry = "--geometry=${WindowWidth}x${WindowHeight}+$X+$Y"

    # The pane's own script. It loads the shared library and builds its URL
    # itself, so the credential never appears in a command line, and it decides
    # between the relay and the camera on every iteration rather than once.
    # Built by substitution into a single-quoted template: nothing in it is
    # interpolated by this script except the four placeholders.
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

# mpv reports progress on stderr, which would be a terminating error under Stop.
$ErrorActionPreference = 'Continue'
while ($true) {
    $choice = Get-CamInputUrl -Config $config -RelayPathName $relayPath -DirectUrl $directUrl
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