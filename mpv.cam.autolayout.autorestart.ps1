# 1. Define Paths and Settings
# mpv.com path (adjust if your mpv installation is elsewhere)
$MpvPath = "C:\mpv\mpv.com"


# Determine the directory where this script is currently located
$ScriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { $PWD.Path }
$CredPath = Join-Path -Path $ScriptDir -ChildPath "camera_creds.xml"

# Auto-Setup: If credentials don't exist, prompt and save them securely
if (-not (Test-Path $CredPath)) {
    Write-Host "=== First Time Setup ===" -ForegroundColor Cyan
    Write-Host "Please enter your camera username and password. This will be encrypted and saved locally." -ForegroundColor Yellow
    $Credential = Get-Credential
    $Credential | Export-Clixml -Path $CredPath
    Write-Host "Credentials successfully secured in $CredPath" -ForegroundColor Green
}

# Load and decrypt the credentials into memory
$SecureCreds = Import-Clixml -Path $CredPath
$username = $SecureCreds.UserName
$password = $SecureCreds.GetNetworkCredential().Password


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

# Read the JSON and dynamically build the Streams dictionary
# Note: -Raw is important here so ConvertFrom-Json parses the whole file as one object
$JsonData = Get-Content -Path $JsonPath -Raw | ConvertFrom-Json
$Streams = [ordered]@{}

foreach ($Camera in $JsonData) {
    # Inject the username, password, IP, and quality into the RTSP format
    $RTSPUrl = "rtsp://${Username}:${Password}@$($Camera.IP)/$($Camera.Quality)"
    $Streams[$Camera.Name] = $RTSPUrl
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
    
    # Construct the exact script block as a clean string
    $ScriptBlock = "while (`$true) { & `"$MpvPath`" `"$($Stream.Value)`" $Flags $OverlayOpts $Geometry; Write-Host 'Stream interrupted. Restarting in 3 seconds...' -ForegroundColor Yellow; Start-Sleep -Seconds 3 }"
    
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