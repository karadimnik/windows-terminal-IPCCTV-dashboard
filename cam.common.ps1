<#
    cam.common.ps1 - shared helpers for the IPCCTV dashboard and the recorder.

    Dot-source it, do not run it:

        . "$PSScriptRoot\cam.common.ps1"

    Targets Windows PowerShell 5.1 (the README tells users to right-click
    "Run with PowerShell", which is 5.1). No PowerShell 7 only syntax.
#>

# Directory this library lives in. cameras.json, camera_creds.xml and
# recorder.config.json are all expected beside it.
$script:CamLibRoot = $PSScriptRoot
if (-not $script:CamLibRoot) { $script:CamLibRoot = (Get-Location).Path }

# Literal strings that must never reach a log, a status file or the console.
$script:CamSecrets = New-Object System.Collections.ArrayList

# Filenames that NTFS accepts but Win32 reserves as device names.
$script:CamReservedNames = @(
    'CON', 'PRN', 'AUX', 'NUL',
    'COM1', 'COM2', 'COM3', 'COM4', 'COM5', 'COM6', 'COM7', 'COM8', 'COM9',
    'LPT1', 'LPT2', 'LPT3', 'LPT4', 'LPT5', 'LPT6', 'LPT7', 'LPT8', 'LPT9'
)

$script:CamDefaultLogMaxBytes = 2MB


function Get-CamLibRoot {
    <# Directory containing this library and the config files beside it. #>
    return $script:CamLibRoot
}


# ---------------------------------------------------------------------------
# Formatting: invariant numbers, round-trippable timestamps
# ---------------------------------------------------------------------------
# Shared rather than private to the recorder because status.json is written by
# the supervisor and read by -Status and by the health strip, so every side has
# to format and parse the same way.

function Format-CamNumber {
    <#
        Formats a number with an invariant decimal point. The machine's locale
        uses a comma, which makes measurements awkward to read and compare.
    #>
    param(
        [Parameter(Mandatory = $true)][double]$Value,
        [string]$Format = '0.00'
    )
    return $Value.ToString($Format, [System.Globalization.CultureInfo]::InvariantCulture)
}


function Format-CamAgeText {
    <# A duration as the shortest readable unit, or - for "never". #>
    param($Seconds)

    if ($null -eq $Seconds) { return '-' }
    $value = [double]$Seconds
    if ($value -lt 120) { return ("{0}s" -f [int]$value) }
    if ($value -lt 7200) { return ("{0}m" -f [int]($value / 60)) }
    return ("{0}h" -f [int]($value / 3600))
}


function Format-CamTimestamp {
    <# Round-trippable local timestamp, or $null for "never". #>
    param($Value)

    if ($null -eq $Value) { return $null }
    $when = [DateTime]$Value
    if ($when -eq [DateTime]::MinValue) { return $null }
    return $when.ToString('o')
}


function Get-CamDateTimeOrNull {
    <# Parses a timestamp written by Format-CamTimestamp. #>
    param($Value)

    if ($null -eq $Value) { return $null }
    $text = [string]$Value
    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    try { return [DateTime]::Parse($text, [System.Globalization.CultureInfo]::InvariantCulture) }
    catch { return $null }
}

# ---------------------------------------------------------------------------
# Secret registration and redaction
# ---------------------------------------------------------------------------

function Register-CamSecret {
    <#
        Adds a literal string to the redaction list. Both the raw value and its
        URI-escaped form are registered, because an RTSP URL may carry either.
        Call this for every credential the process loads, before anything is
        written to a log.
    #>
    param([string]$Value)

    if ([string]::IsNullOrEmpty($Value)) { return }

    $forms = @($Value, [System.Uri]::EscapeDataString($Value))
    foreach ($form in $forms) {
        if ([string]::IsNullOrEmpty($form)) { continue }
        if (-not $script:CamSecrets.Contains($form)) {
            [void]$script:CamSecrets.Add($form)
        }
    }
}


function Get-CamRedacted {
    <#
        Returns $Text with credentials removed. Two independent passes, so a
        credential is caught whether or not it was registered:
          1. any "scheme://userinfo@host" userinfo is blanked;
          2. every registered secret is replaced literally.
        EVERY line written to any log, status file or console must pass through
        this function.
    #>
    param([string]$Text)

    if ([string]::IsNullOrEmpty($Text)) { return $Text }

    $out = [regex]::Replace($Text, '(?i)([a-z][a-z0-9+.\-]*://)[^/\s@]+@', '${1}***:***@')

    foreach ($secret in $script:CamSecrets) {
        if ($secret.Length -lt 3) { continue }
        $out = $out.Replace($secret, '***')
    }

    return $out
}


# ---------------------------------------------------------------------------
# Credentials
# ---------------------------------------------------------------------------

function Get-CamCredential {
    <#
        Loads the DPAPI-protected credential written by the dashboard's first
        run and registers it for redaction. The password is never returned as
        plain text by this function and must never be logged or displayed.
    #>
    param([string]$Path)

    if (-not $Path) { $Path = Join-Path -Path $script:CamLibRoot -ChildPath 'camera_creds.xml' }

    if (-not (Test-Path -LiteralPath $Path)) {
        throw "Camera credentials not found at '$Path'. Run mpv.cam.autolayout.autorestart.ps1 once to create them."
    }

    $cred = Import-Clixml -LiteralPath $Path
    if (-not ($cred -is [System.Management.Automation.PSCredential])) {
        throw "'$Path' does not contain a PSCredential."
    }

    Register-CamSecret -Value $cred.UserName
    Register-CamSecret -Value $cred.GetNetworkCredential().Password

    return $cred
}


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

function Get-CamConfigDefault {
    <#
        Every configuration key with its in-code default, so a missing or
        partial recorder.config.json still produces a working recorder.
    #>
    return @{
        RootPath             = 'E:\cameras'
        FfmpegPath           = 'C:\ffmpeg\bin\ffmpeg.exe'
        FfprobePath          = 'C:\ffmpeg\bin\ffprobe.exe'
        SegmentSeconds       = 900
        ReserveGB            = 20
        PruneHysteresisGB    = 5
        PruneIntervalSeconds = 300
        MaxRetentionDays     = $null

        # Dashboard layout. The strip the dashboard reserves along the bottom
        # of the screen for cam.health.ps1:
        #   -1  derive it from the video aspect - each pane is exactly as tall
        #       as the video is at the pane width, and the strip takes every
        #       pixel left over. This is what puts 640x360 video on a 1920 px
        #       screen with six cameras.
        #    0  no strip at all. The panes fill the screen exactly as they did
        #       before the strip existed and no health window is launched.
        #   >0  that many pixels, with the panes shrunk to fit.
        HealthStripHeightPx  = -1
        # Never give the strip less than this many pixels; shrink the panes
        # instead. A strip too short to read is worse than slightly smaller
        # video.
        HealthStripMinPx     = 120
        # Gutter BETWEEN the grid rows, and between the grid and the strip.
        # Never at the screen edges: three 640 px panes fill a 1920 px screen
        # exactly, so an edge gutter could only come out of the picture.
        GridGutterPx         = 8
        # Shape of the video, which fixes the pane height at a given pane
        # width. These cameras are 16:9.
        VideoAspectWidth     = 16
        VideoAspectHeight    = 9
        # Approximate cell size of the strip window's font. Used ONLY to ask
        # Windows Terminal for a roughly right size at launch so it cannot
        # flash up over the grid; the exact pixel geometry is applied with
        # SetWindowPos afterwards and the renderer reads its real row and
        # column count from the console it ends up with. Neither number can
        # affect correctness.
        HealthCellWidthPx    = 9
        HealthCellHeightPx   = 20

        # Sections the health strip renders, in this order. Remove a name to
        # switch that section off; an empty list renders an empty strip.
        #   status   one line - supervisor, relay, free space, days left
        #   cameras  the per-camera table with its column header
        #   events   the most recent supervisor events, newest first
        #   archive  footage span and size, then the disk trend and pruning
        HealthSections       = @('status', 'cameras', 'events', 'archive')
        # Rows the events section occupies, its own label row included. Five
        # fills the Operator's 15-row strip exactly alongside six cameras:
        # 1 status + 1 header + 6 cameras + 5 events + 2 archive. The camera
        # table absorbs any difference at another camera count.
        HealthEventRows      = 5
        # How many of the supervisor log's most recent lines status.json
        # publishes for the strip to read.
        StatusEventCount     = 20
        # Local restreamer (MediaMTX). Transport fan-out only: it frees the
        # second camera slot, keeps the camera password out of every consumer's
        # command line, and halves the load on camera and network. The paths
        # it serves are generated from cameras.json by -InstallRestreamer.
        RestreamerEnabled    = $true
        RestreamerExePath    = 'C:\mediamtx\mediamtx.exe'
        RestreamerConfigPath = 'C:\mediamtx\mediamtx.yml'
        RelayBase            = 'rtsp://127.0.0.1:8554'
        RelayApiBase         = 'http://127.0.0.1:9997'
        # Budget for one control-API probe. Spent once per recorder child
        # start and once per mpv pane attempt, both on loopback.
        RelayProbeTimeoutMs  = 1500
        # How long the supervisor waits for the restreamer to answer its
        # control API before it starts the recorders. Without the wait the
        # recorders take a direct slot each at a cold start, and a camera
        # already serving the dashboard would then have no slot left for the
        # restreamer to pull. It waits for the API only, not for every path:
        # each camera waits for its OWN path below, so one offline camera -
        # whose path can never become ready - cannot delay the others.
        RelayStartupWaitSeconds = 20
        # How long ONE consumer waits for its OWN relay path to start serving
        # before it gives up and takes a direct camera slot. Spent only on a
        # consumer's first attempt, at batch startup: a later attempt decides
        # on the spot so a relay that died is left behind immediately.
        RelayReadyWaitSeconds   = 15
        # How often that wait re-asks.
        RelayReadyPollMs        = 500
        # How often an mpv pane that is on a DIRECT connection asks whether
        # the relay has started serving its camera again, so it can hand the
        # second camera session back. One control-API call per pane per
        # interval, and only for a pane that is actually on direct. 0 switches
        # the watch off and leaves such a pane on direct until the dashboard
        # is relaunched, which is what it did before this existed.
        ViewerPromoteCheckSeconds = 10
        # Delay between consecutive consumer starts - recorder children and
        # mpv panes alike - so six of them do not burst onto the relay, the
        # control API and the cameras in the same instant.
        StartStaggerMs          = 300
        Container            = 'mkv'
        ContainerProfiles    = @{
            mkv  = [pscustomobject]@{
                Extension            = '.mkv'
                SegmentFormat        = 'matroska'
                # Empty: the global -c copy already copies audio. Adding an
                # explicit -c:a copy makes ffmpeg warn about duplicate codec
                # options on every start. Only a profile that changes the audio
                # codec needs AudioArgs, as fmp4 does.
                AudioArgs            = @()
                ExtraArgs            = @()
                SegmentFormatOptions = ''
            }
            fmp4 = [pscustomobject]@{
                Extension            = '.mp4'
                SegmentFormat        = 'mp4'
                AudioArgs            = @('-c:a', 'aac', '-b:a', '64k')
                ExtraArgs            = @()
                SegmentFormatOptions = 'movflags=+frag_keyframe+empty_moov+default_base_moof'
            }
        }

        # Input and supervisor tuning. Defaults are the values the plan fixed;
        # they are configuration keys so a camera-specific problem can be tuned
        # without a code change.
        RtspTransport        = 'tcp'
        TimeoutMicroseconds  = 10000000
        ReconcileSeconds     = 5
        StatusIntervalSeconds = 5
        # The archive-wide scan behind the status file's disk figures. Separate
        # from the status cadence because it touches every segment file, which
        # is too much work to repeat every 5 s.
        ArchiveScanSeconds   = 60
        # How long -Stop waits for the supervisor to tear its children down.
        StopTimeoutSeconds   = 30
        BackoffStartSeconds  = 3
        BackoffMaxSeconds    = 60
        HealthyResetSeconds  = 300
        StallFactor          = 2.5
        LogMaxBytes          = 2MB
        LogRepeatSuppressSeconds = 60
    }
}


function Get-CamConfig {
    <#
        Reads recorder.config.json and overlays it on the in-code defaults.
        A missing file is not an error. Unknown keys in the file are kept, so a
        later phase can add keys without touching this function.
    #>
    param([string]$Path)

    if (-not $Path) { $Path = Join-Path -Path $script:CamLibRoot -ChildPath 'recorder.config.json' }

    $config = Get-CamConfigDefault

    if (Test-Path -LiteralPath $Path) {
        $text = [System.IO.File]::ReadAllText($Path)
        if (-not [string]::IsNullOrWhiteSpace($text)) {
            $fromFile = ConvertFrom-Json -InputObject $text
            foreach ($property in $fromFile.PSObject.Properties) {
                $config[$property.Name] = $property.Value
            }
        }
    }

    # Fail loudly on a value that would silently break recording.
    $config.SegmentSeconds = [int]$config.SegmentSeconds
    if ($config.SegmentSeconds -lt 1) {
        throw "SegmentSeconds must be at least 1, got $($config.SegmentSeconds)."
    }
    $config.StallFactor = [double]$config.StallFactor
    if ($config.StallFactor -le 1) {
        throw "StallFactor must be greater than 1, got $($config.StallFactor)."
    }
    if ([string]::IsNullOrWhiteSpace([string]$config.RootPath)) {
        throw 'RootPath must not be empty.'
    }

    return $config
}


function Get-CamContainerProfile {
    <#
        Resolves a named container profile, defaulting to the active one named
        by the Container key. This is the single point where the container is
        chosen, which is what makes the container swappable by a config edit.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [string]$Name
    )

    if (-not $Name) { $Name = [string]$Config.Container }
    $profiles = $Config.ContainerProfiles

    $resolved = $null
    if ($profiles -is [System.Collections.IDictionary]) {
        if ($profiles.Contains($Name)) { $resolved = $profiles[$Name] }
    }
    else {
        $property = $profiles.PSObject.Properties[$Name]
        if ($property) { $resolved = $property.Value }
    }

    if (-not $resolved) {
        throw "Container profile '$Name' is not defined in ContainerProfiles."
    }
    foreach ($required in @('Extension', 'SegmentFormat')) {
        if ([string]::IsNullOrWhiteSpace([string]$resolved.$required)) {
            throw "Container profile '$Name' is missing '$required'."
        }
    }

    return $resolved
}


function Get-CamProfileArgList {
    <# Reads an optional string-array member of a container profile. #>
    param(
        [Parameter(Mandatory = $true)]$ContainerProfile,
        [Parameter(Mandatory = $true)][string]$Name
    )

    $property = $ContainerProfile.PSObject.Properties[$Name]
    if (-not $property) { return @() }
    if ($null -eq $property.Value) { return @() }
    return @($property.Value)
}


# ---------------------------------------------------------------------------
# Camera list
# ---------------------------------------------------------------------------

function Get-CamList {
    <#
        Reads cameras.json and always returns an array, even for one camera.
        ReadAllText honours the UTF-8 BOM the file ships with.
    #>
    param([string]$Path)

    if (-not $Path) { $Path = Join-Path -Path $script:CamLibRoot -ChildPath 'cameras.json' }

    if (-not (Test-Path -LiteralPath $Path)) {
        throw "Camera list not found at '$Path'."
    }

    # Get-CamFileText rather than ReadAllText: the supervisor re-reads this file
    # every few seconds while -Enable may be replacing it, and a plain read
    # would block that rename.
    $text = Get-CamFileText -Path $Path
    $parsed = ConvertFrom-Json -InputObject $text

    # ConvertFrom-Json does not enumerate its array down the pipeline, so the
    # result is wrapped rather than piped. The leading comma stops PowerShell
    # unrolling a single-camera array into a scalar on return.
    return , @($parsed)
}


function Set-CamList {
    <#
        Writes cameras.json atomically: serialise, write a temporary file in the
        same directory, then replace. A crash can therefore leave either the old
        file or the new one, never a truncated one.

        Every pre-existing field survives, because the objects read by
        Get-CamList are written back unchanged apart from what the caller
        altered. A UTF-8 BOM present on the existing file is preserved.
    #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Cameras,
        [string]$Path
    )

    if (-not $Path) { $Path = Join-Path -Path $script:CamLibRoot -ChildPath 'cameras.json' }

    $json = ConvertTo-Json -InputObject @($Cameras) -Depth 10
    # PowerShell 5.1 unwraps a one-element array, which would turn the camera
    # list into a bare object. Re-wrap it.
    if (@($Cameras).Count -le 1 -and -not $json.TrimStart().StartsWith('[')) {
        $json = "[$([Environment]::NewLine)$json$([Environment]::NewLine)]"
    }
    $json = $json + [Environment]::NewLine

    $writeBom = $true
    if (Test-Path -LiteralPath $Path) {
        $head = New-Object byte[] 3
        $stream = [System.IO.File]::OpenRead($Path)
        try { $read = $stream.Read($head, 0, 3) } finally { $stream.Dispose() }
        $writeBom = ($read -eq 3 -and $head[0] -eq 0xEF -and $head[1] -eq 0xBB -and $head[2] -eq 0xBF)
    }

    $encoding = New-Object System.Text.UTF8Encoding($writeBom)
    $temp = "$Path.tmp"
    if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Force }
    [System.IO.File]::WriteAllText($temp, $json, $encoding)

    Move-CamFileAtomic -TempPath $temp -Path $Path
}


function Get-CamSafeName {
    <#
        Turns a camera name into a filesystem-safe token usable as both a
        directory name and a filename prefix. Throws rather than guessing, so a
        bad name in cameras.json surfaces instead of producing a folder nobody
        can find.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Name)

    $safe = $Name.Trim()
    $safe = [regex]::Replace($safe, '\s+', '_')
    $safe = [regex]::Replace($safe, '[<>:"/\\|?*]', '')
    $safe = [regex]::Replace($safe, '[\x00-\x1F\x7F]', '')
    $safe = $safe.TrimEnd('.', ' ')

    if ([string]::IsNullOrEmpty($safe)) {
        throw "Camera name '$Name' contains no characters that are legal in a Windows filename."
    }

    $stem = $safe
    $dot = $stem.IndexOf('.')
    if ($dot -ge 0) { $stem = $stem.Substring(0, $dot) }
    if ($script:CamReservedNames -contains $stem.ToUpperInvariant()) {
        throw "Camera name '$Name' resolves to the reserved Windows device name '$stem'. Rename the camera in cameras.json."
    }

    return $safe
}


function Get-CamRtspUrl {
    <#
        Builds the RTSP URL for a camera. The result contains the password:
        never log it, never echo it, pass it only to the process that needs it.
    #>
    param(
        [Parameter(Mandatory = $true)]$Camera,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$Credential
    )

    $stream = [string]$Camera.Quality
    if ([string]::IsNullOrWhiteSpace($stream)) { $stream = 'stream1' }

    $user = $Credential.UserName
    $pass = $Credential.GetNetworkCredential().Password

    return "rtsp://${user}:${pass}@$($Camera.IP)/$stream"
}


function Get-CamRecordEnabled {
    <#
        True only when the camera's Record field is explicitly true. A camera
        with no Record field is off, so an upgrade never starts recording
        everything by surprise.
    #>
    param([Parameter(Mandatory = $true)]$Camera)

    $property = $Camera.PSObject.Properties['Record']
    if (-not $property) { return $false }
    return ([bool]$property.Value -eq $true)
}


# ---------------------------------------------------------------------------
# Local restreamer (MediaMTX): a transport fan-out in front of the cameras
# ---------------------------------------------------------------------------
# One camera session is pulled by MediaMTX and re-served on loopback, so the
# recorder and the dashboard share it instead of taking a camera slot each.
# These cameras serve exactly two concurrent RTSP sessions, so this is what
# leaves a slot free for the phone app. Recording, segmenting, pruning and
# redaction stay in cam.recorder.ps1: MediaMTX is transport only.
#
# Every consumer prefers the relay and falls back to the direct camera URL
# when the relay is not serving that path, which keeps viewing and recording
# working while the restreamer is down.

function Get-CamRelayEnabled {
    <# False turns the restreamer off completely: no child, no relay URLs. #>
    param([Parameter(Mandatory = $true)]$Config)

    if ($Config -is [System.Collections.IDictionary]) {
        if (-not $Config.Contains('RestreamerEnabled')) { return $false }
        return ([bool]$Config['RestreamerEnabled'] -eq $true)
    }

    $property = $Config.PSObject.Properties['RestreamerEnabled']
    if (-not $property) { return $false }
    return ([bool]$property.Value -eq $true)
}


function Get-CamRelayPathName {
    <#
        The MediaMTX path name for a camera: its safe name lowercased with
        every character that is not a letter or a digit removed.

        Why the characters have to go. MediaMTX takes each path's source from
        the environment variable MTX_PATHS_<NAME>_SOURCE, and its env loader
        cuts the map key at the FIRST underscore, requires that segment to be
        uppercase, and stores it lowercased. A path named workshop_entrance
        therefore produces a stray path "workshop" plus an "workshop_entrance"
        that keeps source: publisher and stays offline - with no error at all.
        Measured on v1.21.2, 2026-10-11:

            CASE 'workshopentrance'  -> ready=True  tracks=[H264,G711]
            CASE 'workshop_entrance' -> two paths, both ready=False, no tracks

        An alphanumeric name is what makes that mapping unambiguous.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$SafeName)

    $name = [regex]::Replace($SafeName.ToLowerInvariant(), '[^a-z0-9]', '')
    if ([string]::IsNullOrEmpty($name)) {
        throw "Camera name '$SafeName' contains no letter or digit, so it cannot name a MediaMTX path. Rename the camera in cameras.json."
    }
    return $name
}


function Get-CamRelayPathMap {
    <#
        Ordered path name -> camera map covering EVERY camera in the list, not
        only the ones being recorded: the dashboard views them all and shares
        the same relay.

        Pass (Get-CamList), never @(Get-CamList): Get-CamList returns its array
        with a leading comma, which @() around a function call nests one level
        deeper, and this loop would then run once over a single element holding
        every camera.

        A collision throws rather than resolving itself, because the quiet
        outcome would be one camera's stream served under two names while
        another camera is never pulled at all.
    #>
    param([Parameter(Mandatory = $true)][object[]]$Cameras)

    $map = New-Object System.Collections.Specialized.OrderedDictionary
    foreach ($camera in $Cameras) {
        $safe = Get-CamSafeName -Name ([string]$camera.Name)
        $path = Get-CamRelayPathName -SafeName $safe
        if ($map.Contains($path)) {
            throw "Cameras '$($map[$path].Name)' and '$($camera.Name)' both map to the MediaMTX path '$path'. Rename one in cameras.json."
        }
        $map[$path] = $camera
    }
    return $map
}


function Get-CamRelaySourceEnvName {
    <# The variable MediaMTX reads this path's source URL from. #>
    param([Parameter(Mandatory = $true)][string]$PathName)

    return "MTX_PATHS_$($PathName.ToUpperInvariant())_SOURCE"
}


function Get-CamRelayUrl {
    <# The loopback URL for one relayed camera. Carries no credential. #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$PathName
    )

    return ("{0}/{1}" -f ([string]$Config.RelayBase).TrimEnd('/'), $PathName)
}


function Get-CamRelayHostPort {
    <#
        Strips a URL down to the host:port MediaMTX wants in its own config, so
        the addresses have a single source of truth in recorder.config.json.
        Done with a regex rather than [System.Uri] because rtsp:// is not a
        scheme .NET parses consistently across versions.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Url)

    $text = [regex]::Replace([string]$Url, '(?i)^[a-z][a-z0-9+.\-]*://', '')
    $text = $text.TrimEnd('/')
    if ([string]::IsNullOrWhiteSpace($text)) {
        throw "'$Url' has no host:port to give MediaMTX."
    }
    return $text
}


function Get-CamRelayApiJson {
    <#
        GETs one route from the MediaMTX control API and returns the parsed
        object, or $null for every failure - relay down, path unknown, timeout,
        garbage. A reachability probe must never throw into a caller that is
        about to start a recording.

        HttpWebRequest rather than Invoke-RestMethod: this needs a sub-second
        timeout (Invoke-RestMethod's -TimeoutSec is whole seconds) and must
        ignore any system proxy, which would otherwise be asked to route a
        loopback call.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Route
    )

    $timeout = 1500
    try { $timeout = [int]$Config.RelayProbeTimeoutMs } catch { }
    if ($timeout -lt 100) { $timeout = 100 }

    $uri = "{0}/{1}" -f ([string]$Config.RelayApiBase).TrimEnd('/'), $Route.TrimStart('/')

    try {
        $request = [System.Net.HttpWebRequest]::Create($uri)
        $request.Method = 'GET'
        $request.Proxy = $null
        $request.Timeout = $timeout
        $request.ReadWriteTimeout = $timeout

        $response = $request.GetResponse()
        try {
            $reader = New-Object System.IO.StreamReader($response.GetResponseStream())
            try { $text = $reader.ReadToEnd() } finally { $reader.Dispose() }
        }
        finally { $response.Dispose() }

        if ([string]::IsNullOrWhiteSpace($text)) { return $null }
        return (ConvertFrom-Json -InputObject $text)
    }
    catch { return $null }
}


function Test-CamRelayPathReady {
    <#
        True only when MediaMTX is up AND this camera's path is actually
        serving. The narrower question matters: a plain TCP connect to 8554
        would pass while the camera behind the path is unreachable, and the
        consumer would then sit on a dead relay instead of going direct.

        Deliberately not an RTSP session - a probe that opened one would itself
        consume a reader slot and add latency to every restart.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$PathName
    )

    $route = 'v3/paths/get/' + [System.Uri]::EscapeDataString($PathName)
    $info = Get-CamRelayApiJson -Config $Config -Route $route
    if (-not $info) { return $false }

    $property = $info.PSObject.Properties['ready']
    if (-not $property) { return $false }
    return ([bool]$property.Value -eq $true)
}


function Get-CamRelayReadyPaths {
    <#
        The names of every path the relay is currently serving, in one call, or
        an empty array when the relay is down.

        One list query rather than a probe per camera: the supervisor asks this
        on every status tick to see whether a camera still on its direct input
        can move onto the relay, and six separate probes a tick would be
        needless chatter.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $list = Get-CamRelayApiJson -Config $Config -Route 'v3/paths/list'
    if (-not $list) { return @() }

    $names = New-Object System.Collections.ArrayList
    foreach ($item in @($list.items)) {
        if (-not $item) { continue }
        $property = $item.PSObject.Properties['ready']
        if (-not $property) { continue }
        if ([bool]$property.Value -eq $true) { [void]$names.Add([string]$item.name) }
    }
    return $names.ToArray()
}


function Get-CamInputUrl {
    <#
        Chooses the input for ONE consumer attempt: the relay when it is
        serving this path, the direct camera URL otherwise.

        Called per attempt - every recorder child start, every mpv pane
        iteration - rather than once at startup. That is what makes a
        restreamer that dies mid-run fall back on the next attempt instead of
        being retried forever against a dead address, and what lets a consumer
        return to the relay on its own once MediaMTX is back.

        Two optional parameters are what a BATCH of consumers starting together
        needs, and neither changes the single-attempt behaviour above when it
        is left out:

        -ReadyPaths is the serving set the batch fetched ONCE with
        Get-CamRelayReadyPaths. A path in it is served without any API call at
        all, so six cameras starting together cost one list query instead of
        six near-simultaneous per-path probes - any one of which could flake
        and silently send that camera direct for its whole run. Measured on
        2026-10-11: two of six mpv panes chose direct on a launch where five of
        the six paths had just been confirmed serving.

        -WaitSeconds is how long to keep asking the API while the answer is
        still no, for the other half of the same fault: a path that is not
        ready YET. It BLOCKS, so only a consumer with nothing else to do in the
        meantime may use it - an mpv pane, which is its own process. The
        supervisor must not, because one offline camera would then hold up
        every other child; it spreads its wait across its own loop instead.

        The returned Mode is logged by both consumers so the choice is always
        visible after the fact, and WaitedSeconds says how much of the budget
        the decision cost.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [AllowEmptyString()][string]$RelayPathName,
        [Parameter(Mandatory = $true)][string]$DirectUrl,
        [AllowNull()][AllowEmptyCollection()][string[]]$ReadyPaths,
        [int]$WaitSeconds = 0
    )

    $direct = [pscustomobject]@{ Mode = 'direct'; Url = $DirectUrl; WaitedSeconds = 0 }

    if (-not (Get-CamRelayEnabled -Config $Config)) { return $direct }
    if ([string]::IsNullOrEmpty($RelayPathName)) { return $direct }

    $relay = [pscustomobject]@{
        Mode          = 'relay'
        Url           = (Get-CamRelayUrl -Config $Config -PathName $RelayPathName)
        WaitedSeconds = 0
    }

    # The batch's own answer first: free, and not subject to a probe flake.
    if ($PSBoundParameters.ContainsKey('ReadyPaths') -and (@($ReadyPaths) -contains $RelayPathName)) {
        return $relay
    }

    # No batch answer means this is a lone attempt, so ask for this one path.
    # Skipped when the batch already said no, because it just asked.
    if (-not $PSBoundParameters.ContainsKey('ReadyPaths')) {
        if (Test-CamRelayPathReady -Config $Config -PathName $RelayPathName) { return $relay }
    }

    if ($WaitSeconds -le 0) { return $direct }

    $pollMs = 500
    try { $pollMs = [int]$Config.RelayReadyPollMs } catch { }
    if ($pollMs -lt 50) { $pollMs = 50 }

    $startedAt = [DateTime]::Now
    $deadline = $startedAt.AddSeconds($WaitSeconds)
    while ([DateTime]::Now -lt $deadline) {
        Start-Sleep -Milliseconds $pollMs
        if (Test-CamRelayPathReady -Config $Config -PathName $RelayPathName) {
            $relay.WaitedSeconds = ([DateTime]::Now - $startedAt).TotalSeconds
            return $relay
        }
    }

    $direct.WaitedSeconds = ([DateTime]::Now - $startedAt).TotalSeconds
    return $direct
}


function Get-CamArgumentLine {
    <#
        Joins argument tokens into the ONE command-line string .NET takes,
        quoting the way PowerShell's own native-command call does: a token
        containing whitespace or a quote is wrapped in double quotes and its
        inner quotes escaped, and every other token is passed through
        untouched.

        It exists because Start-Process takes a command line rather than an
        argument vector, and one mpv flag - the OSD message - carries a space
        inside it. Verified byte for byte against the command line the call
        operator produced for the same tokens before Invoke-CamViewer
        existed:

            ... --osd-level=1 "--osd-msg1=Traffic: ${video-bitrate}" --geometry=...

        No trailing-backslash handling, because these are mpv flags and a
        Windows path never ends in one here.
    #>
    param([AllowNull()][AllowEmptyCollection()][string[]]$Tokens)

    return (@(foreach ($token in @($Tokens)) {
        if ([string]$token -match '[\s"]') { '"' + ([string]$token -replace '"', '\"') + '"' } else { [string]$token }
    }) -join ' ')
}


function Stop-CamViewer {
    <#
        Stops one mpv the clean way: WM_CLOSE to its window, a bounded wait,
        then a kill aimed at that one process only if it ignored the close.

        mpv ships two binaries and the dashboard runs the console one,
        mpv.com, which is a wrapper that re-execs mpv.exe and waits for it.
        The video window therefore belongs to the CHILD. Stopping the wrapper
        alone leaves mpv.exe on screen still holding its RTSP session, which
        is the opposite of the point, so the children are closed first and the
        wrapper follows them down on its own. Measured 2026-10-12: closing the
        child's window exits the child in ~220 ms and the wrapper ~140 ms
        after that.

        Closing the window rather than killing it matters because mpv then
        tears its RTSP session down instead of leaving the camera to time the
        session out - and freeing that session is the whole reason for the
        move.

        Nothing is found by image name. The children are read from this
        process's own pid, so no other mpv on the desktop can be touched, and
        the pane's own terminal is never a candidate.
    #>
    param(
        [Parameter(Mandatory = $true)]$Process,

        # Long enough for a graceful quit, short enough that a pane does not
        # sit on a black tile if mpv is wedged.
        [int]$GraceMs = 5000
    )

    $targets = New-Object System.Collections.ArrayList
    $children = @()
    try {
        $children = @(Get-CimInstance -ClassName Win32_Process -Filter ("ParentProcessId=" + [int]$Process.Id))
    }
    catch { }
    foreach ($child in $children) {
        # The wrapper also has a conhost child, which owns a window and must
        # not be sent anything.
        if ([string]$child.Name -notlike 'mpv*') { continue }
        $handle = Get-Process -Id ([int]$child.ProcessId) -ErrorAction SilentlyContinue
        if ($handle) { [void]$targets.Add($handle) }
    }
    # Last, and harmless either way: an mpv.exe started directly owns its
    # window itself, while the console wrapper inside a Windows Terminal pane
    # has no window of its own for CloseMainWindow to find.
    [void]$targets.Add($Process)

    foreach ($target in $targets) {
        try { [void]$target.CloseMainWindow() } catch { }
    }

    if ($Process.WaitForExit($GraceMs)) { return }

    foreach ($target in $targets) {
        try { if (-not $target.HasExited) { $target.Kill() } } catch { }
    }
    [void]$Process.WaitForExit(2000)
}


function Invoke-CamViewer {
    <#
        Runs ONE mpv attempt for a dashboard pane and answers one question:
        did the relay take this pane back?

        $true  - the relay started serving this camera's path while mpv was on
                 the direct input, so mpv was stopped deliberately and the
                 caller should decide again at once.
        $false - mpv exited on its own, or there was nothing to watch for, so
                 the caller should back off and retry as it always has.

        Why a pane needs this. The pane loop re-decides relay-or-direct on
        every mpv exit, and mpv on a HEALTHY direct stream never exits. A pane
        that fell back to direct during a relay outage therefore stayed there
        until the dashboard was relaunched, holding the second of the camera's
        two RTSP sessions - the one the relay exists to keep free for the
        Operator's phone. Recorder children never had the fault because their
        supervisor restarts them and re-decides.

        Only a pane that chose DIRECT watches anything. A pane on the relay
        needs no watch: if the relay dies mpv loses its input and exits, and
        the loop decides again. That asymmetry is the whole design.

        The watch costs one control-API call per ViewerPromoteCheckSeconds and
        it sleeps inside Process.WaitForExit, so a camera whose path can never
        become ready - an offline one - keeps to the clock forever instead of
        spinning, and notices mpv's own exit in the same wait.

        The pane hands over the argument tokens exactly as PowerShell parsed
        them and Get-CamArgumentLine rejoins them, so the arguments mpv
        receives are the ones the call operator used to pass.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,

        # What Get-CamInputUrl decided: Mode and Url.
        [Parameter(Mandatory = $true)]$Choice,

        [AllowEmptyString()][string]$RelayPathName,

        [Parameter(Mandatory = $true)][string]$MpvPath,

        [AllowNull()][AllowEmptyCollection()][string[]]$MpvArgs
    )

    $line = Get-CamArgumentLine -Tokens (@([string]$Choice.Url) + @($MpvArgs))

    # -NoNewWindow keeps mpv's console output in the pane it belongs to, the
    # way the call operator did. Its stderr is not captured, so it can no
    # longer become a NativeCommandError.
    $process = Start-Process -FilePath $MpvPath -ArgumentList $line -NoNewWindow -PassThru
    try { $null = $process.Handle } catch { }

    $seconds = 10
    try { $seconds = [int]$Config.ViewerPromoteCheckSeconds } catch { }

    if (([string]$Choice.Mode -ne 'direct') -or ($seconds -le 0) -or
        [string]::IsNullOrEmpty($RelayPathName) -or -not (Get-CamRelayEnabled -Config $Config)) {
        $process.WaitForExit()
        return $false
    }

    while (-not $process.WaitForExit($seconds * 1000)) {
        if (Test-CamRelayPathReady -Config $Config -PathName $RelayPathName) {
            Stop-CamViewer -Process $process
            return $true
        }
    }
    return $false
}


function Get-CamRelayConfigText {
    <#
        Renders mediamtx.template.yml for the current camera list: one path per
        camera, the addresses taken from recorder.config.json so they cannot
        drift from the URLs the consumers probe.

        No source URL is ever written here. Each path keeps source: publisher
        in the file and is overridden at launch by MTX_PATHS_<NAME>_SOURCE,
        which is the only reason the camera password stays off the disk.

        Used by -InstallRestreamer to write the file and by the supervisor to
        tell whether the file on disk still matches cameras.json.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$PathMap,
        [string]$TemplatePath
    )

    if (-not $TemplatePath) {
        $TemplatePath = Join-Path -Path $script:CamLibRoot -ChildPath 'mediamtx.template.yml'
    }
    if (-not (Test-Path -LiteralPath $TemplatePath)) {
        throw "MediaMTX config template not found at '$TemplatePath'."
    }

    $lines = New-Object System.Collections.ArrayList
    foreach ($path in $PathMap.Keys) {
        $camera = $PathMap[$path]
        $envName = Get-CamRelaySourceEnvName -PathName $path
        [void]$lines.Add("  ${path}:")
        [void]$lines.Add("    # $($camera.Name) at $($camera.IP) - source supplied at launch by $envName")
        [void]$lines.Add('    source: publisher')
    }

    $text = [System.IO.File]::ReadAllText($TemplatePath)
    $text = $text.Replace('__RTSP_ADDRESS__', (Get-CamRelayHostPort -Url ([string]$Config.RelayBase)))
    $text = $text.Replace('__API_ADDRESS__', (Get-CamRelayHostPort -Url ([string]$Config.RelayApiBase)))
    $text = $text.Replace('__PATHS__', ($lines -join "`r`n"))

    # Normalised to CRLF: the generated file lands on NTFS and the Operator
    # reads it in a Windows editor.
    $text = [regex]::Replace($text, "`r`n|`n", "`r`n")
    return $text
}


# ---------------------------------------------------------------------------
# Paths and the ffmpeg command
# ---------------------------------------------------------------------------

function Get-CamPaths {
    <#
        Resolves and creates the archive layout. The segment muxer cannot create
        directories (-strftime_mkdir is on the hls muxer only), so every
        directory must exist before ffmpeg starts.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [string[]]$CameraSafeNames = @()
    )

    $root = [string]$Config.RootPath
    $paths = @{
        Root     = $root
        LogDir   = Join-Path -Path $root -ChildPath '_logs'
        StateDir = Join-Path -Path $root -ChildPath '_state'
        Cameras  = @{}
    }

    $toCreate = New-Object System.Collections.ArrayList
    [void]$toCreate.Add($paths.Root)
    [void]$toCreate.Add($paths.LogDir)
    [void]$toCreate.Add($paths.StateDir)
    foreach ($safe in $CameraSafeNames) {
        $dir = Join-Path -Path $root -ChildPath $safe
        $paths.Cameras[$safe] = $dir
        [void]$toCreate.Add($dir)
    }

    foreach ($dir in $toCreate) {
        if (-not (Test-Path -LiteralPath $dir)) {
            [void](New-Item -ItemType Directory -Path $dir -Force)
        }
    }

    return $paths
}


function Get-CamSegmentPattern {
    <#
        The -strftime output pattern for one camera.

        strftime here is local time: this build has no UTC option and ignores
        TZ. %z appends the UTC offset (+0300), which is legal in a Windows
        filename and is what stops the repeated hour at the DST fall-back from
        overwriting the earlier segments of the same wall-clock hour.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$CameraDir,
        [Parameter(Mandatory = $true)][string]$SafeName,
        [Parameter(Mandatory = $true)]$ContainerProfile
    )

    $leaf = "${SafeName}_%Y%m%d_%H%M%S%z$($ContainerProfile.Extension)"
    return (Join-Path -Path $CameraDir -ChildPath $leaf)
}


function Get-CamFfmpegArgs {
    <#
        Builds the argument list for one camera's recorder. Everything that
        depends on the container comes from the resolved profile, so switching
        the Container key is the only change needed to switch container.

        Notable choices, all verified against this ffmpeg build:
          -nostdin           ffmpeg otherwise steals stdin from a shared console
                             and can take down sibling processes.
          -timeout           the correct RTSP socket timeout since ffmpeg 5.0;
                             -stimeout is rejected by this build.
          -map 0:a:0?        the trailing ? keeps video recording when the
                             source has no audio track.
          -segment_format_options
                             inner-muxer options MUST go here. A top-level
                             -movflags is silently ignored under -f segment and
                             produces an unfragmented, crash-fragile MP4.
        No -reconnect* flags: they are HTTP-only and do nothing for rtsp://.
        Reconnection is the supervisor's job.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Url,
        [Parameter(Mandatory = $true)][string]$OutputPattern,
        [Parameter(Mandatory = $true)]$Config,
        $ContainerProfile
    )

    if (-not $ContainerProfile) { $ContainerProfile = Get-CamContainerProfile -Config $Config }

    $arguments = @()
    $arguments += '-hide_banner', '-loglevel', 'warning', '-nostdin'
    $arguments += '-rtsp_transport', [string]$Config.RtspTransport
    $arguments += '-timeout', [string][int64]$Config.TimeoutMicroseconds
    $arguments += '-fflags', '+genpts+discardcorrupt'
    $arguments += '-i', $Url
    $arguments += '-map', '0:v:0', '-map', '0:a:0?'
    $arguments += '-c', 'copy'
    $arguments += Get-CamProfileArgList -ContainerProfile $ContainerProfile -Name 'AudioArgs'
    $arguments += Get-CamProfileArgList -ContainerProfile $ContainerProfile -Name 'ExtraArgs'
    $arguments += '-f', 'segment'
    $arguments += '-segment_time', [string][int]$Config.SegmentSeconds
    $arguments += '-segment_atclocktime', '1'
    $arguments += '-reset_timestamps', '1'
    $arguments += '-strftime', '1'
    $arguments += '-segment_format', [string]$ContainerProfile.SegmentFormat

    $formatOptions = ''
    $property = $ContainerProfile.PSObject.Properties['SegmentFormatOptions']
    if ($property) { $formatOptions = [string]$property.Value }
    if (-not [string]::IsNullOrWhiteSpace($formatOptions)) {
        $arguments += '-segment_format_options', $formatOptions
    }

    $arguments += $OutputPattern

    # Returned without the leading comma used elsewhere in this file: a comma
    # makes the array arrive as a single object, which @(...) at the call site
    # would then nest one level deeper. An argument list is never one element,
    # so the plain form is both correct and safe to wrap.
    return [string[]]$arguments
}


function Get-CamQuotedArgument {
    <#
        Quotes one argument for a Windows command line, following the CommandLineToArgvW
        rules: backslashes are literal except when they precede a quote.

        Needed because .NET Framework's ProcessStartInfo has no ArgumentList (that is
        .NET Core only), so PowerShell 5.1 must hand Process.Start a single string.
        An RTSP password or an archive path containing a space would otherwise split
        into two arguments.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Value)

    if ($Value.Length -gt 0 -and $Value.IndexOfAny([char[]]@(' ', "`t", "`n", '"')) -lt 0) {
        return $Value
    }

    $builder = New-Object System.Text.StringBuilder
    [void]$builder.Append('"')
    $i = 0
    while ($i -lt $Value.Length) {
        $slashes = 0
        while ($i -lt $Value.Length -and $Value[$i] -eq '\') { $slashes++; $i++ }
        if ($i -ge $Value.Length) {
            # Trailing backslashes precede the closing quote, so they must be doubled.
            [void]$builder.Append('\', $slashes * 2)
        }
        elseif ($Value[$i] -eq '"') {
            [void]$builder.Append('\', $slashes * 2 + 1)
            [void]$builder.Append('"')
            $i++
        }
        else {
            [void]$builder.Append('\', $slashes)
            [void]$builder.Append($Value[$i])
            $i++
        }
    }
    [void]$builder.Append('"')
    return $builder.ToString()
}


function Get-CamCommandLine {
    <# Joins an argument list into a single correctly quoted command line. #>
    param([Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$Arguments)

    $quoted = @()
    foreach ($argument in $Arguments) { $quoted += Get-CamQuotedArgument -Value $argument }
    return ($quoted -join ' ')
}


# ---------------------------------------------------------------------------
# Logging and disk
# ---------------------------------------------------------------------------

function Write-CamLog {
    <#
        Appends one timestamped, redacted line, rotating the file once it
        exceeds MaxBytes. One previous generation is kept as <name>.1.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Message,
        [int]$MaxBytes = 0,
        [switch]$NoTimestamp
    )

    if ($MaxBytes -le 0) { $MaxBytes = $script:CamDefaultLogMaxBytes }

    $dir = Split-Path -Path $Path -Parent
    if ($dir -and -not (Test-Path -LiteralPath $dir)) {
        [void](New-Item -ItemType Directory -Path $dir -Force)
    }

    if (Test-Path -LiteralPath $Path) {
        $info = New-Object System.IO.FileInfo($Path)
        if ($info.Length -ge $MaxBytes) {
            $rolled = "$Path.1"
            if (Test-Path -LiteralPath $rolled) { Remove-Item -LiteralPath $rolled -Force }
            Move-Item -LiteralPath $Path -Destination $rolled -Force
        }
    }

    $line = Get-CamRedacted -Text $Message
    if (-not $NoTimestamp) {
        $line = "$([DateTime]::Now.ToString('yyyy-MM-dd HH:mm:ss zzz')) $line"
    }

    [System.IO.File]::AppendAllText($Path, $line + [Environment]::NewLine, (New-Object System.Text.UTF8Encoding($false)))
}


function Get-CamFileTailLines {
    <#
        The last $Count non-blank lines of a text file, oldest first, without
        loading the whole file: a supervisor log runs up to LogMaxBytes and
        this is read on every status write.

        Opened with FileShare.ReadWrite so reading it can never block the
        writer, and a missing or unreadable file returns nothing rather than
        throwing - every caller is reporting on the log, not depending on it.

        Returns a plain array. Wrap the call in @() if you need to index it.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][ValidateRange(1, 1000)][int]$Count,
        # Tail window. 64 KB holds far more than Count lines of this log.
        [ValidateRange(1024, 4194304)][int]$MaxBytes = 65536
    )

    if (-not (Test-Path -LiteralPath $Path)) { return @() }

    $buffer = $null
    $read = 0
    $partialFirstLine = $false
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        try {
            $length = $stream.Length
            $take = [int][math]::Min([long]$MaxBytes, $length)
            if ($take -le 0) { return @() }
            $partialFirstLine = ($take -lt $length)
            [void]$stream.Seek(($length - $take), [System.IO.SeekOrigin]::Begin)
            $buffer = New-Object byte[] $take
            $read = $stream.Read($buffer, 0, $take)
        }
        finally { $stream.Dispose() }
    }
    catch { return @() }

    if ($read -le 0) { return @() }

    $text = [System.Text.Encoding]::UTF8.GetString($buffer, 0, $read)
    $lines = @($text -split "`r?`n" | Where-Object { $_.Trim().Length -gt 0 })

    # The first line is cut in half when the tail window started mid-line.
    if ($partialFirstLine -and $lines.Count -gt 1) { $lines = @($lines[1..($lines.Count - 1)]) }
    elseif ($partialFirstLine) { $lines = @() }

    if ($lines.Count -le $Count) { return $lines }
    return @($lines[($lines.Count - $Count)..($lines.Count - 1)])
}


function Get-CamFreeSpaceGB {
    <# Free space in GB on the volume holding $Path. #>
    param([Parameter(Mandatory = $true)][string]$Path)

    $full = [System.IO.Path]::GetFullPath($Path)
    $root = [System.IO.Path]::GetPathRoot($full)
    $drive = New-Object System.IO.DriveInfo($root)
    return [math]::Round($drive.AvailableFreeSpace / 1GB, 2)
}


# ---------------------------------------------------------------------------
# State files shared between the supervisor and the CLI
# ---------------------------------------------------------------------------

function Get-CamStateDirPath {
    <# The _state directory path. Does not create it: readers must not. #>
    param([Parameter(Mandatory = $true)]$Config)
    return (Join-Path -Path ([string]$Config.RootPath) -ChildPath '_state')
}


function Get-CamStatusFilePath {
    <#
        status.json, the supervisor's only outward channel. -Status, -Stop and
        the health strip all read this file, which is why none of them needs IPC
        and all of them work from any shell.
    #>
    param([Parameter(Mandatory = $true)]$Config)
    return (Join-Path -Path (Get-CamStateDirPath -Config $Config) -ChildPath 'status.json')
}


function Get-CamStopFlagPath {
    <#
        The stop request flag. -Stop creates it, the supervisor notices it within
        one tick and shuts its children down itself.

        A flag file rather than a console control event: the supervisor normally
        runs from a Scheduled Task with no console to attach to, and a signal
        sent to a shared console group would hit every ffmpeg child at once
        instead of letting the supervisor tear them down in order.
    #>
    param([Parameter(Mandatory = $true)]$Config)
    return (Join-Path -Path (Get-CamStateDirPath -Config $Config) -ChildPath 'stop.request')
}


function Get-CamFileText {
    <#
        Reads a text file that another process may be replacing underneath us.

        FileShare.Delete is the point: without it this read would block the
        atomic rename in Move-CamFileAtomic, so the supervisor rewriting
        status.json would fail whenever -Status happened to be reading it. A
        UTF-8 BOM is detected and stripped, as ReadAllText would.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [int]$RetryCount = 5,
        [int]$RetryDelayMs = 80
    )

    $share = [System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete
    $attempt = 0
    while ($true) {
        $attempt++
        try {
            $stream = New-Object System.IO.FileStream($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, $share)
            try {
                $reader = New-Object System.IO.StreamReader($stream, [System.Text.Encoding]::UTF8, $true)
                try { return $reader.ReadToEnd() }
                finally { $reader.Dispose() }
            }
            finally { $stream.Dispose() }
        }
        catch [System.IO.IOException] {
            # A genuinely absent file is not a transient condition.
            if ($_.Exception -is [System.IO.FileNotFoundException]) { throw }
            if ($_.Exception -is [System.IO.DirectoryNotFoundException]) { throw }
            if ($attempt -ge $RetryCount) { throw }
            Start-Sleep -Milliseconds $RetryDelayMs
        }
    }
}


function Move-CamFileAtomic {
    <#
        Replaces $Path with $TempPath, retrying while a reader holds the
        destination open.

        Move-Item -Force, not [System.IO.File]::Replace: Replace() calls
        ReplaceFileW, which the \\wsl.localhost 9P share answers with "The
        request is not supported", and this repository is normally run from
        exactly that share. Move-Item goes through MoveFileEx with
        MOVEFILE_REPLACE_EXISTING, an atomic rename on NTFS that the share
        accepts too.

        The retry exists because a reader that opened the destination without
        FILE_SHARE_DELETE blocks the rename for as long as it holds the handle.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$TempPath,
        [Parameter(Mandatory = $true)][string]$Path,
        [int]$RetryCount = 10,
        [int]$RetryDelayMs = 100
    )

    $attempt = 0
    while ($true) {
        $attempt++
        try {
            Move-Item -LiteralPath $TempPath -Destination $Path -Force
            return
        }
        catch {
            if ($attempt -ge $RetryCount) { throw }
            Start-Sleep -Milliseconds $RetryDelayMs
        }
    }
}


function Test-CamPathInside {
    <#
        True only when $Path resolves inside $Root. The trailing separator on the
        root is what stops 'E:\cameras_old\x.mkv' counting as inside
        'E:\cameras', and GetFullPath collapses any '..' before the comparison.

        This is the pruner's last line of defence: nothing is ever deleted
        without passing through here.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Root,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Path
    )

    if ([string]::IsNullOrWhiteSpace($Path)) { return $false }

    $separator = [string][System.IO.Path]::DirectorySeparatorChar
    $rootFull = [System.IO.Path]::GetFullPath($Root)
    if (-not $rootFull.EndsWith($separator)) { $rootFull = $rootFull + $separator }

    $full = [System.IO.Path]::GetFullPath($Path)
    return $full.StartsWith($rootFull, [System.StringComparison]::OrdinalIgnoreCase)
}


# ---------------------------------------------------------------------------
# status.json: the read side
# ---------------------------------------------------------------------------
# The supervisor writes the file; -Status, -Stop and the health strip read it
# through these two functions, so "is a supervisor actually behind this snapshot"
# is decided in one place instead of once per consumer.

function Get-CamStatusSnapshot {
    <#
        Reads status.json, or $null when there is nothing readable there. Never
        throws: every caller has something sensible to say about absence.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $path = Get-CamStatusFilePath -Config $Config
    if (-not (Test-Path -LiteralPath $path)) { return $null }

    try { $text = Get-CamFileText -Path $path }
    catch { return $null }

    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    try { return (ConvertFrom-Json -InputObject $text) }
    catch { return $null }
}


function Get-CamSupervisorState {
    <#
        Decides whether the supervisor behind a snapshot is actually running.

        Both signals must agree: the heartbeat has to be fresh AND the recorded
        process has to still exist. A heartbeat alone would believe a supervisor
        that was killed a second ago; a live PID alone would believe a PID that
        the machine has since reused.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        $Snapshot
    )

    $state = @{ Running = $false; Reason = 'no status file has been written yet'; AgeSeconds = $null; Pid = $null }

    if (-not $Snapshot) { return $state }

    $state.Pid = $Snapshot.SupervisorPid
    $updated = Get-CamDateTimeOrNull -Value $Snapshot.UpdatedAt
    if ($updated) { $state.AgeSeconds = [math]::Round(([DateTime]::Now - $updated).TotalSeconds, 1) }

    if ([string]$Snapshot.State -ne 'running') {
        $state.Reason = "the supervisor recorded a clean shutdown at $($Snapshot.UpdatedAt)"
        return $state
    }

    $limit = [double]$Config.StatusIntervalSeconds * 3
    if ($limit -lt 15) { $limit = 15 }
    if ($null -eq $state.AgeSeconds) {
        $state.Reason = 'the status file carries no usable heartbeat'
        return $state
    }
    if ($state.AgeSeconds -gt $limit) {
        $state.Reason = "the heartbeat is $($state.AgeSeconds)s old (limit $([int]$limit)s)"
        return $state
    }

    $process = $null
    if ($state.Pid) { $process = Get-Process -Id ([int]$state.Pid) -ErrorAction SilentlyContinue }
    if (-not $process) {
        $state.Reason = "process $($state.Pid) is no longer running"
        return $state
    }

    $state.Running = $true
    $state.Reason = "pid $($state.Pid), heartbeat $($state.AgeSeconds)s old"
    return $state
}


# ---------------------------------------------------------------------------
# Dashboard layout
# ---------------------------------------------------------------------------
# One function decides where every mpv pane and the health strip go, so the
# geometry can be asserted by the test harness without opening a window.
#
# The grid is full-bleed: there is no gutter at the left, right or top edge of
# the screen, because three 640 px panes fill a 1920 px screen exactly and any
# edge gutter could only be taken out of the picture. The gutter sits BETWEEN
# the rows and between the bottom row and the strip.

function Get-CamDashboardLayout {
    <#
        Works out the pane grid and the health strip from the screen size, the
        camera count and the config. Pure arithmetic: it opens nothing and
        reads nothing but $Config.

        The column and row counts come from the CAMERA COUNT ALONE. The health
        strip is a reserved band, never a grid cell, so six cameras are a 3x2
        grid whether the strip is there or not - putting the strip in the grid
        would reshape it to 3x3 and shrink every feed.

        Returns one object:
          Columns, Rows                 grid shape, from the camera count
          PaneWidth, PaneHeight         pixel size of one mpv window
          GutterPx                      gutter between rows, 0 when disabled
          GridHeight                    rows and gutters together
          StripEnabled                  whether to launch a health window
          StripLeft/Top/Width/Height    the reserved band
          Mode                          off | aspect | fixed
          Note                          why the panes are not aspect-exact,
                                        or $null when they are
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][ValidateRange(1, 64)][int]$CameraCount,
        [Parameter(Mandatory = $true)][ValidateRange(64, 16384)][int]$ScreenWidth,
        [Parameter(Mandatory = $true)][ValidateRange(64, 16384)][int]$ScreenHeight,
        [int]$ScreenLeft = 0,
        [int]$ScreenTop = 0
    )

    # Camera count only. Unchanged from the original dashboard.
    $columns = [int][math]::Ceiling([math]::Sqrt($CameraCount))
    $rows = [int][math]::Ceiling($CameraCount / [double]$columns)
    $paneWidth = [int][math]::Floor($ScreenWidth / $columns)

    $stripSetting = [int]$Config.HealthStripHeightPx

    if ($stripSetting -eq 0) {
        # Exactly the original arithmetic, so turning the strip off restores
        # the layout the dashboard had before it existed.
        return [pscustomobject]@{
            Columns      = $columns
            Rows         = $rows
            PaneWidth    = $paneWidth
            PaneHeight   = [int][math]::Floor($ScreenHeight / $rows)
            GutterPx     = 0
            GridHeight   = ([int][math]::Floor($ScreenHeight / $rows)) * $rows
            StripEnabled = $false
            StripLeft    = $ScreenLeft
            StripTop     = $ScreenTop + $ScreenHeight
            StripWidth   = $ScreenWidth
            StripHeight  = 0
            Mode         = 'off'
            Note         = $null
        }
    }

    $gutter = [int]$Config.GridGutterPx
    if ($gutter -lt 0) { $gutter = 0 }

    $aspectWidth = [double]$Config.VideoAspectWidth
    $aspectHeight = [double]$Config.VideoAspectHeight
    if ($aspectWidth -le 0 -or $aspectHeight -le 0) {
        throw "VideoAspectWidth and VideoAspectHeight must both be greater than zero, got $($Config.VideoAspectWidth)x$($Config.VideoAspectHeight)."
    }

    # Every row costs one gutter: rows-1 between the rows, plus one between the
    # bottom row and the strip. So rows*paneHeight + rows*gutter + strip fills
    # the screen, which is the identity both branches below are solved from.
    $mode = 'aspect'
    $note = $null
    $paneHeight = [int][math]::Round($paneWidth * $aspectHeight / $aspectWidth)
    $stripHeight = $ScreenHeight - ($rows * $paneHeight) - ($rows * $gutter)

    if ($stripSetting -gt 0) {
        $mode = 'fixed'
        $stripHeight = $stripSetting
        $paneHeight = [int][math]::Floor(($ScreenHeight - $stripHeight - ($rows * $gutter)) / $rows)
        $note = 'pane height is set by the configured strip height, not by the video aspect'
    }

    $minStrip = [int]$Config.HealthStripMinPx
    if ($minStrip -lt 0) { $minStrip = 0 }
    if ($stripHeight -lt $minStrip) {
        $stripHeight = $minStrip
        $paneHeight = [int][math]::Floor(($ScreenHeight - $stripHeight - ($rows * $gutter)) / $rows)
        $note = "panes shrunk below the video aspect so the strip keeps its $minStrip px floor"
    }

    if ($paneHeight -lt 1) {
        throw ("A {0} px screen cannot fit {1} row(s) of panes plus a {2} px strip." -f $ScreenHeight, $rows, $stripHeight)
    }

    $gridHeight = ($rows * $paneHeight) + (($rows - 1) * $gutter)
    $stripTop = $ScreenTop + $gridHeight + $gutter
    # The strip absorbs whatever the pane rounding left over, so it always
    # reaches the bottom edge of the screen exactly.
    $stripHeight = ($ScreenTop + $ScreenHeight) - $stripTop

    return [pscustomobject]@{
        Columns      = $columns
        Rows         = $rows
        PaneWidth    = $paneWidth
        PaneHeight   = $paneHeight
        GutterPx     = $gutter
        GridHeight   = $gridHeight
        StripEnabled = ($stripHeight -gt 0)
        StripLeft    = $ScreenLeft
        StripTop     = $stripTop
        StripWidth   = $ScreenWidth
        StripHeight  = $stripHeight
        Mode         = $mode
        Note         = $note
    }
}


function Get-CamPanePosition {
    <# Top-left pixel of pane $Index in the layout's grid. #>
    param(
        [Parameter(Mandatory = $true)]$Layout,
        [Parameter(Mandatory = $true)][int]$Index,
        [int]$ScreenLeft = 0,
        [int]$ScreenTop = 0
    )

    $column = $Index % $Layout.Columns
    $row = [int][math]::Floor($Index / $Layout.Columns)
    return [pscustomobject]@{
        Column = $column
        Row    = $row
        X      = $ScreenLeft + ($column * $Layout.PaneWidth)
        Y      = $ScreenTop + ($row * ($Layout.PaneHeight + $Layout.GutterPx))
    }
}
