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

# The one log file Write-CamLog also echoes to the console, set by
# Set-CamLogEcho. Null means file only, which is what every script other than
# the NVR supervisor wants.
$script:CamLogEchoPath = $null
if (-not $script:CamLibRoot) { $script:CamLibRoot = (Get-Location).Path }

# Literal strings that must never reach a log, a status file or the console.
$script:CamSecrets = New-Object System.Collections.ArrayList

# The relay control API's Basic authorization header value, loaded once from
# relay_api_creds.xml by the first API call that finds the file; and what the
# most recent call in this process got back: ok, denied or down.
$script:CamRelayApiAuthorization = $null
$script:CamRelayApiStatus = $null

# Filenames that NTFS accepts but Win32 reserves as device names.
$script:CamReservedNames = @(
    'CON', 'PRN', 'AUX', 'NUL',
    'COM1', 'COM2', 'COM3', 'COM4', 'COM5', 'COM6', 'COM7', 'COM8', 'COM9',
    'LPT1', 'LPT2', 'LPT3', 'LPT4', 'LPT5', 'LPT6', 'LPT7', 'LPT8', 'LPT9'
)

$script:CamDefaultLogMaxBytes = 2MB

# Logs whose rotation is currently failing in this process, so Write-CamLog
# says so once per episode rather than on every line.
$script:CamLogRotationBlocked = @{}

# Lines Write-CamLog could not append, per log, reported by the next line that
# does get written: Lines, Since (the first drop's timestamp) and Reason.
$script:CamLogDropped = @{}


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
    <#
        Round-trippable timestamp, or $null for "never": local time with the
        offset in force at that instant (2026-10-25T03:30:00+02:00), so people
        read local time and every reader still gets the exact instant. Taken
        from the UTC instant because a local DateTime in the repeated hour of a
        fall-back cannot always say which of the two offsets it had.
    #>
    param($Value)

    if ($null -eq $Value) { return $null }
    $when = [DateTime]$Value
    if ($when -eq [DateTime]::MinValue) { return $null }
    return ([DateTimeOffset]$when.ToUniversalTime()).ToLocalTime().ToString('o')
}


function Get-CamDateTimeOrNull {
    <#
        Parses a timestamp written by Format-CamTimestamp as the instant it
        names, returned in UTC so it can be subtracted from Get-CamWallClock.
        Parsing to a local DateTime would drop the offset and put a time from
        before a daylight-saving change an hour out.
    #>
    param($Value)

    if ($null -eq $Value) { return $null }
    $text = [string]$Value
    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    try { return [DateTimeOffset]::Parse($text, [System.Globalization.CultureInfo]::InvariantCulture).UtcDateTime }
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


function Get-CamRelayApiCredential {
    <#
        The product's own login for the restreamer's control API, or $null when
        there is none yet and -Create was not given.

        Generated, never chosen: 32 random bytes as 64 hex characters, user
        camrelayapi, written with Export-Clixml so the password is protected
        by DPAPI for this Windows user exactly like camera_creds.xml. Only its
        SHA-256 goes into mediamtx.yml (see Get-CamRelayApiPassHash). -Create
        writes the file only when it is missing, so a second call, a second
        -InstallRestreamer or a restreamer restart reuses the same login.

        The password is registered for redaction; the user name is not a
        secret and is deliberately left out, because the redactor would
        otherwise blank every word that happened to match it.
    #>
    param(
        [string]$Path,
        [switch]$Create
    )

    if (-not $Path) { $Path = Join-Path -Path $script:CamLibRoot -ChildPath 'relay_api_creds.xml' }

    if (-not (Test-Path -LiteralPath $Path)) {
        if (-not $Create) { return $null }

        $bytes = New-Object byte[] 32
        $random = [System.Security.Cryptography.RandomNumberGenerator]::Create()
        try { $random.GetBytes($bytes) } finally { $random.Dispose() }
        $secure = New-Object System.Security.SecureString
        foreach ($byte in $bytes) { foreach ($char in $byte.ToString('x2').ToCharArray()) { $secure.AppendChar($char) } }

        $temp = "$Path.tmp"
        New-Object System.Management.Automation.PSCredential('camrelayapi', $secure) | Export-Clixml -LiteralPath $temp
        Move-CamFileAtomic -TempPath $temp -Path $Path
    }

    $cred = Import-Clixml -LiteralPath $Path
    if (-not ($cred -is [System.Management.Automation.PSCredential])) {
        throw "'$Path' does not contain a PSCredential."
    }
    Register-CamSecret -Value $cred.GetNetworkCredential().Password
    return $cred
}


function Get-CamRelayApiPassHash {
    <#
        The form MediaMTX documents for a hashed internal password: sha256:
        followed by the base64 of the SHA-256 of the password. A hash of 256
        random bits cannot be reversed or guessed, so the generated config can
        hold it in place of the password.
    #>
    param([Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$Credential)

    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $digest = $sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($Credential.GetNetworkCredential().Password))
    }
    finally { $sha.Dispose() }
    return ('sha256:' + [Convert]::ToBase64String($digest))
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
        # The age rule's circuit breaker: a run whose MaxRetentionDays would
        # delete more than this share of the archive - by segment count or by
        # bytes - deletes nothing by age. 1.0 switches the brake off. The
        # free-space watermark is never held back by it.
        MaxAgePruneFraction  = 0.5

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
        # Live view in a browser: a SECOND MediaMTX, dedicated to WebRTC and
        # separate from the recording relay above, which never speaks WebRTC.
        # Off by default; with it off nothing here is read, started or written.
        # Its paths are <relaypath>sub (the camera's LiveSubStream, pulled on
        # demand) and <relaypath>main (the recording relay's own path, so no
        # extra camera session). Read access needs a login that only the
        # supervisor and camui can read; see tasks/portable-nvr/live-1a-design.md.
        LiveViewEnabled      = $false
        # Empty means the relay's RestreamerExePath.
        LiveExePath          = ''
        # A LOCAL NTFS path, like RestreamerConfigPath.
        LiveConfigPath       = 'C:\mediamtx\mediamtx-live.yml'
        # Loopback only: WebRTC signaling (TCP) and the ICE media port (UDP).
        LiveWhepAddress      = '127.0.0.1:8889'
        LiveUdpAddress       = '127.0.0.1:8189'
        # The camera stream the live grid pulls, on demand.
        LiveSubStream        = 'stream2'
        # How long after the last viewer leaves the camera session is closed.
        LiveCloseAfterSeconds = 10
        # The login's secret. Empty means %LOCALAPPDATA%\IPCCTV\live_view_secret.txt:
        # a local per-user folder, because the repository share cannot hold an ACL.
        LiveSecretPath       = ''
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
        # How long a camera may write nothing before the Operator is alerted,
        # and how long before the same camera alerts again while still down.
        # Measured from the later of the supervisor's own start and the
        # camera's newest segment - NEVER from the child's StartedAt, which a
        # camera that is failing to connect refreshes on every retry.
        CameraDownAlertMinutes = 15
        CameraDownRepeatHours  = 12
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
    $fraction = 0.0
    $parsed = [double]::TryParse([string]$config.MaxAgePruneFraction, [System.Globalization.NumberStyles]::Float,
        [System.Globalization.CultureInfo]::InvariantCulture, [ref]$fraction)
    if (-not $parsed -or -not ($fraction -ge 0.01 -and $fraction -le 1.0)) {
        throw "MaxAgePruneFraction must be a number from 0.01 to 1.0 - the largest share of the archive one age-rule run may delete, 1.0 to switch the brake off - got '$($config.MaxAgePruneFraction)'."
    }
    $config.MaxAgePruneFraction = $fraction

    Assert-CamLiveConfig -Config $config

    return $config
}


function Assert-CamLiveConfig {
    <#
        Rejects a malformed live-view setting at load rather than at the first
        start. Checked whether or not LiveViewEnabled is true, so a typo cannot
        sit unnoticed until the day it is switched on. Any key starting with
        Live that is not one of the known ones is refused too: a misspelled
        LiveViewEnabled would otherwise leave live view off without a word.
    #>
    param([Parameter(Mandatory = $true)][hashtable]$Config)

    $known = @('LiveViewEnabled', 'LiveExePath', 'LiveConfigPath', 'LiveWhepAddress', 'LiveUdpAddress', 'LiveSubStream', 'LiveCloseAfterSeconds', 'LiveSecretPath')
    foreach ($key in @($Config.Keys)) {
        if ($key -match '^(?i)live' -and $known -cnotcontains $key) {
            throw "Unknown live-view setting '$key'. The known ones are: $($known -join ', ')."
        }
    }

    if ($Config.LiveViewEnabled -isnot [bool]) {
        throw "LiveViewEnabled must be true or false, got '$($Config.LiveViewEnabled)'."
    }
    foreach ($key in @('LiveExePath', 'LiveConfigPath', 'LiveSecretPath')) {
        if ($Config[$key] -isnot [string]) { throw "$key must be a path string, got '$($Config[$key])'." }
    }
    if ([string]::IsNullOrWhiteSpace([string]$Config.LiveConfigPath)) { throw 'LiveConfigPath must not be empty.' }
    foreach ($key in @('LiveExePath', 'LiveConfigPath', 'LiveSecretPath')) {
        if (([string]$Config[$key]).StartsWith('\\')) {
            throw "$key '$($Config[$key])' is a network or WSL share path. MediaMTX cannot watch its config there and a share cannot hold the secret's access control. Use a local path."
        }
    }

    $ports = @{}
    foreach ($key in @('LiveWhepAddress', 'LiveUdpAddress')) {
        $match = [regex]::Match([string]$Config[$key], '^127\.0\.0\.1:([0-9]{1,5})$')
        if (-not $match.Success -or [int]$match.Groups[1].Value -lt 1 -or [int]$match.Groups[1].Value -gt 65535) {
            throw "$key must be 127.0.0.1 and a port such as 127.0.0.1:8889, got '$($Config[$key])'. Live view is loopback only."
        }
        $ports[$key] = [string]$Config[$key]
    }

    if ([string]$Config.LiveSubStream -notmatch '^[A-Za-z0-9_\-]{1,32}$') {
        throw "LiveSubStream must be a camera stream name such as stream2 (letters, digits, - and _), got '$($Config.LiveSubStream)'."
    }

    $seconds = 0
    if (-not [int]::TryParse([string]$Config.LiveCloseAfterSeconds, [ref]$seconds) -or $seconds -lt 1 -or $seconds -gt 3600) {
        throw "LiveCloseAfterSeconds must be a whole number from 1 to 3600, got '$($Config.LiveCloseAfterSeconds)'."
    }
    $Config.LiveCloseAfterSeconds = $seconds

    # The live instance must not be told to take an address the relay owns.
    $relayAddresses = @()
    foreach ($key in @('RelayBase', 'RelayApiBase')) {
        if ($Config[$key]) { $relayAddresses += (Get-CamRelayHostPort -Url ([string]$Config[$key])) }
    }
    if ($relayAddresses -contains $ports.LiveWhepAddress) {
        throw "LiveWhepAddress '$($ports.LiveWhepAddress)' is the address of the recording relay (RelayBase or RelayApiBase)."
    }
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


function Get-CamRelayApiAuthorization {
    <#
        The Authorization header value for the control API, or $null while
        relay_api_creds.xml does not exist. Read once per process and then
        kept; a missing file is looked for again on the next call, so a
        consumer started before the supervisor created it picks it up.

        The base64 token is registered for redaction as well as the password:
        it decodes to the password, so it is the same secret.
    #>
    if ($null -eq $script:CamRelayApiAuthorization) {
        $credential = Get-CamRelayApiCredential
        if ($credential) {
            $token = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes(
                    $credential.UserName + ':' + $credential.GetNetworkCredential().Password))
            Register-CamSecret -Value $token
            $script:CamRelayApiAuthorization = 'Basic ' + $token
        }
    }
    return $script:CamRelayApiAuthorization
}


function Invoke-CamRelayApi {
    <#
        GETs one route from the MediaMTX control API. Returns Status and Data:

          ok      the API answered 2xx with JSON; Data is the parsed object
          denied  the relay is running but refused the product's login
          down    anything else - not running, path unknown, timeout, garbage

        Never throws: a reachability probe must not break a caller that is
        about to start a recording. Every call records its Status for
        Get-CamRelayApiStatus.

        WHY DENIED IS ITS OWN ANSWER. A refused login is a configuration fault,
        not a dead relay: RTSP needs no login, so the relay is still serving.
        If it read as down, every consumer would take a DIRECT camera input -
        the password back on ffmpeg's command line, and the camera's second
        session gone to a recorder while the relay holds the first.

        Why the second, anonymous request. MediaMTX answers a WRONG login only
        after a random pause of up to 4 s (brute-force damping), so with this
        probe's budget a wrong login usually looks like a timeout. A request
        with no login at all is refused at once. So a credentialed request
        that runs out of time is asked again without one: an immediate 401
        proves the relay is alive and wanted a login the product did not
        satisfy. Measured on v1.21.2, 2026-10-15: wrong login 704-3784 ms, no
        login 0-1 ms, right login under 10 ms.

        The login goes in an Authorization header on this process's own
        request, never on a command line. HttpWebRequest rather than
        Invoke-RestMethod: this needs a sub-second timeout (Invoke-RestMethod's
        -TimeoutSec is whole seconds) and must ignore any system proxy, which
        would otherwise be asked to route a loopback call.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Route
    )

    $timeout = 1500
    try { $timeout = [int]$Config.RelayProbeTimeoutMs } catch { }
    if ($timeout -lt 100) { $timeout = 100 }

    $uri = "{0}/{1}" -f ([string]$Config.RelayApiBase).TrimEnd('/'), $Route.TrimStart('/')
    $authorization = Get-CamRelayApiAuthorization

    $send = {
        param([string]$Header)
        $answer = [pscustomobject]@{ Status = 'down'; Data = $null; TimedOut = $false }
        try {
            $request = [System.Net.HttpWebRequest]::Create($uri)
            $request.Method = 'GET'
            $request.Proxy = $null
            $request.Timeout = $timeout
            $request.ReadWriteTimeout = $timeout
            if ($Header) { $request.Headers['Authorization'] = $Header }

            $response = $request.GetResponse()
            try {
                $reader = New-Object System.IO.StreamReader($response.GetResponseStream())
                try { $text = $reader.ReadToEnd() } finally { $reader.Dispose() }
            }
            finally { $response.Dispose() }

            if (-not [string]::IsNullOrWhiteSpace($text)) {
                $answer.Data = ConvertFrom-Json -InputObject $text
                $answer.Status = 'ok'
            }
        }
        catch [System.Net.WebException] {
            $failure = $_.Exception
            if ($failure.Response) {
                $code = [int]$failure.Response.StatusCode
                $failure.Response.Dispose()
                if ($code -eq 401 -or $code -eq 403) { $answer.Status = 'denied' }
            }
            elseif ($failure.Status -eq [System.Net.WebExceptionStatus]::Timeout) { $answer.TimedOut = $true }
        }
        catch { }
        return $answer
    }

    $answer = & $send $authorization
    if ($answer.TimedOut -and $authorization) {
        $anonymous = & $send $null
        if ($anonymous.Status -ne 'down') { $answer = $anonymous }
    }

    $script:CamRelayApiStatus = $answer.Status
    return [pscustomobject]@{ Status = $answer.Status; Data = $answer.Data }
}


function Get-CamRelayApiStatus {
    <#
        What the most recent control-API call in this process got back: ok,
        denied, down, or $null before the first one. Lets a status display or
        the supervisor say "the login was refused" without a second call.
    #>
    return $script:CamRelayApiStatus
}


function Test-CamRelayPathReady {
    <#
        True when MediaMTX is up AND this camera's path is actually serving.
        The narrower question matters: a plain TCP connect to 8554 would pass
        while the camera behind the path is unreachable, and the consumer would
        then sit on a dead relay instead of going direct.

        Also true when the relay refuses the product's login: it is running,
        and a consumer sent direct by a configuration fault would carry the
        password on its command line and take the camera's second session.
        See Invoke-CamRelayApi.

        Deliberately not an RTSP session - a probe that opened one would itself
        consume a reader slot and add latency to every restart.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$PathName
    )

    $route = 'v3/paths/get/' + [System.Uri]::EscapeDataString($PathName)
    $answer = Invoke-CamRelayApi -Config $Config -Route $route
    if ($answer.Status -eq 'denied') { return $true }
    $info = $answer.Data
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

        When the relay refuses the product's login, every camera's path is
        returned: the relay is running and serving without a login, and only
        the readiness view is lost. Reporting none would send every consumer
        direct - see Invoke-CamRelayApi. A caller that needs to tell the two
        apart asks Get-CamRelayApiStatus afterwards.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $answer = Invoke-CamRelayApi -Config $Config -Route 'v3/paths/list'
    if ($answer.Status -eq 'denied') { return @((Get-CamRelayPathMap -Cameras (Get-CamList)).Keys) }
    $list = $answer.Data
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

    $startedAt = Get-CamWallClock
    $deadline = $startedAt.AddSeconds($WaitSeconds)
    while ((Get-CamWallClock) -lt $deadline) {
        Start-Sleep -Milliseconds $pollMs
        if (Test-CamRelayPathReady -Config $Config -PathName $RelayPathName) {
            $relay.WaitedSeconds = ((Get-CamWallClock) - $startedAt).TotalSeconds
            return $relay
        }
    }

    $direct.WaitedSeconds = ((Get-CamWallClock) - $startedAt).TotalSeconds
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

    try {
        $delivered = $false
        foreach ($target in $targets) {
            try { if ($target.CloseMainWindow()) { $delivered = $true } } catch { }
        }

        # Wait only for a close that was actually delivered. CloseMainWindow
        # finds no window at all if mpv is younger than its own window -
        # measured as the whole grace spent on a half-second-old mpv - and
        # waiting out a message nobody received just leaves the pane black
        # for longer.
        if ($delivered -and $Process.WaitForExit($GraceMs)) { return }

        foreach ($target in $targets) {
            try { if (-not $target.HasExited) { $target.Kill() } } catch { }
        }
        [void]$Process.WaitForExit(2000)
    }
    finally {
        # Every Get-Process here opened a kernel handle. The caller's own
        # object is not ours to dispose.
        foreach ($target in $targets) {
            if ($target -eq $Process) { continue }
            try { $target.Dispose() } catch { }
        }
    }
}


function Send-CamViewerUrl {
    <#
        Hands a stream URL to a running mpv over its JSON IPC pipe, so the URL
        never appears on mpv's command line, where every local process can read
        it. $true only when mpv acknowledged the loadfile.

        mpv was started idle with --input-ipc-server=$PipeName. The pipe takes a
        moment to exist, so the connect is retried until $TimeoutMs, and it
        gives up at once if mpv has already exited. After the load is
        acknowledged the pipe is switched off again: its default access lets
        any local user connect, and mpv would answer the `path` property with
        the URL. A refusal of that last step is not a failure of the hand-over.

        Nothing here logs, and nothing throws: any fault is a $false.
    #>
    param(
        [Parameter(Mandatory = $true)]$Process,
        [Parameter(Mandatory = $true)][string]$PipeName,
        [Parameter(Mandatory = $true)][string]$Url,
        [int]$TimeoutMs = 8000
    )

    $clock = [System.Diagnostics.Stopwatch]::StartNew()
    $client = $null
    try {
        while ($true) {
            if ($Process.HasExited -or $clock.ElapsedMilliseconds -ge $TimeoutMs) { return $false }
            $attempt = New-Object System.IO.Pipes.NamedPipeClientStream('.', $PipeName, [System.IO.Pipes.PipeDirection]::InOut)
            try { $attempt.Connect(250); $client = $attempt; break }
            catch { $attempt.Dispose() }
        }

        $writer = New-Object System.IO.StreamWriter($client)
        $writer.AutoFlush = $true
        $reader = New-Object System.IO.StreamReader($client)

        $sent = 0
        foreach ($command in @(
                @{ command = @('loadfile', $Url); request_id = 1 },
                @{ command = @('set_property', 'input-ipc-server', ''); request_id = 2 })) {
            $writer.WriteLine(($command | ConvertTo-Json -Compress))
            $sent++
            # mpv interleaves events with replies, and only the reply to this
            # request id says whether it worked.
            $reply = $null
            while ($null -eq $reply) {
                $left = $TimeoutMs - $clock.ElapsedMilliseconds
                if ($left -lt 500) { $left = 500 }
                $line = $reader.ReadLineAsync()
                if (-not $line.Wait([int]$left) -or $null -eq $line.Result) { break }
                if ($line.Result -notmatch '"request_id"\s*:\s*' + $sent + '\b') { continue }
                $reply = $line.Result | ConvertFrom-Json
            }
            if ($sent -eq 1 -and (($null -eq $reply) -or ([string]$reply.error -ne 'success'))) { return $false }
        }
        return $true
    }
    catch { return $false }
    finally {
        if ($client) { $client.Dispose() }
    }
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

        A URL that carries credentials is the one exception to "the URL is
        mpv's first argument", because any local process can read a command
        line and the direct URL holds the camera password. mpv is started idle
        with a private IPC pipe and the URL is sent to it afterwards
        (Send-CamViewerUrl). --idle=once keeps the rest of this function
        unchanged: mpv idles only until that first load and then exits when
        playback ends or fails, as it does without the option. If the hand-over
        fails the answer is $false and mpv is stopped - never a retry with the
        URL as an argument. A relay URL has no credentials and keeps the old
        path.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,

        # What Get-CamInputUrl decided: Mode and Url.
        [Parameter(Mandatory = $true)]$Choice,

        [AllowEmptyString()][string]$RelayPathName,

        [Parameter(Mandatory = $true)][string]$MpvPath,

        [AllowNull()][AllowEmptyCollection()][string[]]$MpvArgs,

        [int]$IpcTimeoutMs = 8000
    )

    # Any '@' after the scheme counts, not only a well-formed user:pass@: a
    # password containing a slash would slip past the redactor's own pattern,
    # and the cost of a false positive is only an IPC hand-over.
    $viaIpc = ([string]$Choice.Url -match '^[a-z][a-z0-9+.\-]*://.*@')
    if ($viaIpc) {
        $pipeName = 'camviewer-' + [guid]::NewGuid().ToString('N')
        $line = Get-CamArgumentLine -Tokens (@($MpvArgs) + @('--idle=once', "--input-ipc-server=$pipeName"))
    }
    else {
        $line = Get-CamArgumentLine -Tokens (@([string]$Choice.Url) + @($MpvArgs))
    }

    # -NoNewWindow keeps mpv's console output in the pane it belongs to, the
    # way the call operator did. Its stderr is not captured, so it can no
    # longer become a NativeCommandError.
    $process = Start-Process -FilePath $MpvPath -ArgumentList $line -NoNewWindow -PassThru
    try { $null = $process.Handle } catch { }

    try {
        if ($viaIpc -and -not (Send-CamViewerUrl -Process $process -PipeName $pipeName -Url ([string]$Choice.Url) -TimeoutMs $IpcTimeoutMs)) {
            Stop-CamViewer -Process $process
            Write-Host (Get-CamRedacted -Text 'The viewer could not hand the stream to mpv over its control pipe; mpv was stopped and the pane will retry.') -ForegroundColor Yellow
            return $false
        }

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
    finally {
        # A pane calls this once per mpv attempt and never exits, so the
        # kernel handle behind each Process object has to go back explicitly.
        # Measured over 120 attempts: handles flat with this, drifting until
        # a collection without it.
        try { $process.Dispose() } catch { }
    }
}


function Get-CamRelayConfigText {
    <#
        Renders mediamtx.template.yml for the current camera list: one path per
        camera, the addresses taken from recorder.config.json so they cannot
        drift from the URLs the consumers probe.

        No source URL is ever written here. Each path keeps source: publisher
        in the file and is overridden at launch by MTX_PATHS_<NAME>_SOURCE,
        which is the only reason the camera password stays off the disk.

        Nor is the control API's password: the file holds the product's login
        as its user name and the SHA-256 of its password only, see
        Get-CamRelayApiCredential.

        Used by -InstallRestreamer to write the file and by the supervisor to
        tell whether the file on disk still matches cameras.json and the login.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$PathMap,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$ApiCredential,
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
    $text = $text.Replace('__API_USER__', $ApiCredential.UserName)
    $text = $text.Replace('__API_PASS__', (Get-CamRelayApiPassHash -Credential $ApiCredential))
    $text = $text.Replace('__PATHS__', ($lines -join "`r`n"))

    # Normalised to CRLF: the generated file lands on NTFS and the Operator
    # reads it in a Windows editor.
    $text = [regex]::Replace($text, "`r`n|`n", "`r`n")
    return $text
}


function Get-CamRelayConfigPathNames {
    <#
        The path names declared in a rendered MediaMTX config, in file order.

        Only the paths: block is read. pathDefaults: has children at the same
        indent and would otherwise be collected as two phantom cameras, so the
        block is entered on its own key and left again at the next top-level
        key.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)

    $names = New-Object System.Collections.ArrayList
    $inPaths = $false
    foreach ($line in [regex]::Split($Text, "`r`n|`n")) {
        if (-not $inPaths) {
            if ($line -match '^paths:\s*$') { $inPaths = $true }
            continue
        }
        if ($line -match '^\S') { break }
        if ($line -match '^  ([A-Za-z0-9_]+):\s*$') { [void]$names.Add($Matches[1]) }
    }
    return @($names)
}


function Get-CamRelayConfigAuthBlock {
    <#
        The authentication settings of a rendered MediaMTX config: from the
        authMethod: line through the end of the authInternalUsers: list, with
        line endings normalised. Empty when the file has none, which is what
        every config written before the control API needed a login looks like.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)

    $block = New-Object System.Collections.ArrayList
    $inBlock = $false
    foreach ($line in [regex]::Split($Text, "`r`n|`n")) {
        if (-not $inBlock) {
            if ($line -match '^authMethod:') { $inBlock = $true; [void]$block.Add($line.TrimEnd()) }
            continue
        }
        if ($line -match '^\S' -and $line -notmatch '^authInternalUsers:') { break }
        [void]$block.Add($line.TrimEnd())
    }
    return (($block -join "`r`n").TrimEnd())
}


function Test-CamRelayConfigCurrent {
    <#
        Whether the MediaMTX config on disk still covers exactly the cameras in
        cameras.json - and nothing more.

        WHAT "CURRENT" MEANS HERE, and why it is not the whole file. The only
        thing in that file that depends on cameras.json is the SET OF PATH
        NAMES, one per camera, each named from the camera's NAME. Nothing else
        about a camera is written: every path keeps `source: publisher` and the
        real source URL is handed to MediaMTX at launch in
        MTX_PATHS_<NAME>_SOURCE, which is the only reason the password never
        reaches the disk.

        Comparing the rendered text instead reports a file that is working
        perfectly as out of date, because the generator writes the camera's
        current IP into a COMMENT beside its path. Two cameras took new DHCP
        addresses on 2026-10-13 and both the self-test and the supervisor
        started claiming the relay config was stale while all six paths were
        serving. A check that cries wolf about a correct file teaches the
        Operator to ignore it, which is worse than not having it.

        A renamed, added or removed camera DOES change the set, and that is the
        case that matters: a camera with no path is never pulled and quietly
        stays on a direct connection, taking the second of its two sessions.

        The other thing that must match is the control API's login, reported
        separately as AuthCurrent because it is fixed differently: the
        authentication block in the file must be exactly the one
        Get-CamRelayConfigText renders for relay_api_creds.xml. A file written
        before the API needed a login has no block and leaves the API open; a
        file whose login is not the stored one locks the product out of its
        own API. Without relay_api_creds.xml it cannot be current. Current is
        true only when both are.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$PathMap,
        [Parameter(Mandatory = $true)][string]$ConfigPath
    )

    $result = [pscustomobject]@{
        Current     = $false
        AuthCurrent = $false
        Readable    = $false
        Expected    = @($PathMap.Keys)
        Found       = @()
        Missing     = @()
        Extra       = @()
        Detail      = ''
    }

    $actual = $null
    try { $actual = [System.IO.File]::ReadAllText($ConfigPath) } catch { }
    if ($null -eq $actual) {
        $result.Detail = "could not be read at $ConfigPath"
        return $result
    }
    $result.Readable = $true

    $result.Found = @(Get-CamRelayConfigPathNames -Text $actual)
    $result.Missing = @($result.Expected | Where-Object { $result.Found -notcontains $_ })
    $result.Extra = @($result.Found | Where-Object { $result.Expected -notcontains $_ })
    $pathsCurrent = ($result.Missing.Count -eq 0 -and $result.Extra.Count -eq 0)

    $apiCredential = Get-CamRelayApiCredential
    if ($apiCredential) {
        $expected = Get-CamRelayConfigText -Config $Config -PathMap $PathMap -ApiCredential $apiCredential
        $result.AuthCurrent = ((Get-CamRelayConfigAuthBlock -Text $actual) -ceq (Get-CamRelayConfigAuthBlock -Text $expected))
    }
    $result.Current = ($pathsCurrent -and $result.AuthCurrent)

    if ($result.Current) {
        $result.Detail = '{0} has a path for every camera in cameras.json and the control API login: {1}' -f `
            $ConfigPath, (($result.Found | Sort-Object) -join ', ')
    }
    else {
        $parts = New-Object System.Collections.ArrayList
        if ($result.Missing.Count -gt 0) { [void]$parts.Add('no path for ' + (($result.Missing | Sort-Object) -join ', ')) }
        if ($result.Extra.Count -gt 0) { [void]$parts.Add('a path for no camera: ' + (($result.Extra | Sort-Object) -join ', ')) }
        if (-not $apiCredential) { [void]$parts.Add('no control API login exists yet') }
        elseif (-not $result.AuthCurrent) { [void]$parts.Add('the control API login is missing or not the stored one') }
        $result.Detail = '{0} is out of date - {1}. Run cam.recorder.ps1 -InstallRestreamer' -f `
            $ConfigPath, ($parts -join '; ')
    }
    return $result
}


# ---------------------------------------------------------------------------
# Live view: a dedicated WebRTC MediaMTX beside the recording relay
# ---------------------------------------------------------------------------
# MediaMTX's WebRTC signaling port answers any website's cross-origin request
# when origins are the default, has no Host check, and a user allowed to read
# RTSP may read WebRTC too. So WebRTC never goes on the shared relay (whose
# read is anonymous). This second instance has one named user with a random
# secret, no cross-origin permission and nothing else listening. Design and
# the docs behind each key: tasks/portable-nvr/live-1a-design.md.

$script:CamLiveUser = 'camlive'


function Get-CamLiveEnabled {
    <# False (the default) means no live instance exists: nothing starts, nothing is written. #>
    param([Parameter(Mandatory = $true)]$Config)

    if ($Config -is [System.Collections.IDictionary]) {
        if (-not $Config.Contains('LiveViewEnabled')) { return $false }
        return ($Config['LiveViewEnabled'] -is [bool] -and $Config['LiveViewEnabled'] -eq $true)
    }

    $property = $Config.PSObject.Properties['LiveViewEnabled']
    if (-not $property) { return $false }
    return ($property.Value -is [bool] -and $property.Value -eq $true)
}


function Get-CamLiveExePath {
    <# LiveExePath, or the relay's executable when it is empty. #>
    param([Parameter(Mandatory = $true)]$Config)

    if (-not [string]::IsNullOrWhiteSpace([string]$Config.LiveExePath)) { return [string]$Config.LiveExePath }
    return [string]$Config.RestreamerExePath
}


function Get-CamLivePathMap {
    <#
        Ordered live path name -> entry, from the relay's path map (so a live
        name is checked against every relay name):

          <relaypath>sub   the camera's LiveSubStream, pulled on demand. Its
                           source URL carries the camera credential and is
                           supplied at launch by Get-CamLiveSourceUrl, never
                           written to the config.
          <relaypath>main  the recording relay's own loopback path, which
                           holds the one session the recorder already uses, so
                           it adds none.

        A camera whose recorder Quality already IS LiveSubStream gets only
        <relaypath>main: the relay already pulls that stream for the
        recorder, so a second pull from the camera would only spend its
        second session on the same picture.

        A collision throws, like Get-CamRelayPathMap, rather than serving one
        camera under another's name.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$RelayPathMap
    )

    $map = New-Object System.Collections.Specialized.OrderedDictionary
    foreach ($relayPath in $RelayPathMap.Keys) {
        $camera = $RelayPathMap[$relayPath]
        $quality = [string]$camera.Quality
        if ([string]::IsNullOrWhiteSpace($quality)) { $quality = 'stream1' }

        $entries = New-Object System.Collections.ArrayList
        if ($quality -ne [string]$Config.LiveSubStream) {
            [void]$entries.Add([pscustomobject]@{ Name = "${relayPath}sub"; Kind = 'sub'; RelayPath = $relayPath; Camera = $camera })
        }
        [void]$entries.Add([pscustomobject]@{ Name = "${relayPath}main"; Kind = 'main'; RelayPath = $relayPath; Camera = $camera })

        foreach ($entry in $entries) {
            if ($RelayPathMap.Contains($entry.Name)) {
                throw "The live path '$($entry.Name)' for camera '$($camera.Name)' is also the recording relay's path for camera '$($RelayPathMap[$entry.Name].Name)'. Rename a camera in cameras.json."
            }
            if ($map.Contains($entry.Name)) {
                throw "The live path '$($entry.Name)' for camera '$($camera.Name)' is also the live path of camera '$($map[$entry.Name].Camera.Name)'. Rename a camera in cameras.json."
            }
            $map[$entry.Name] = $entry
        }
    }
    return $map
}


function Get-CamLiveSourceUrl {
    <# The camera URL a <relaypath>sub path pulls (carries the credential: environment only). #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$Camera,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$Credential
    )

    $sub = [pscustomobject]@{ IP = $Camera.IP; Quality = [string]$Config.LiveSubStream }
    return (Get-CamRtspUrl -Camera $sub -Credential $Credential)
}


function Get-CamLiveConfigText {
    <#
        The live instance's whole config, rendered in code (the relay's
        template and Get-CamRelayConfigText are not involved and not changed).

        What is deliberately absent matters as much as what is there: no
        anonymous user, so every read needs the login; no origin in
        webrtcAllowOrigins, so no Access-Control-Allow-Origin is ever sent
        and a browser POST from another origin is refused; every server but
        WebRTC off; WebRTC bound to loopback with the media port advertised
        as 127.0.0.1 only (webrtcIPsFromInterfaces off, so no interface name
        has to be spelled). The secret appears only as its SHA-256.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$LivePathMap,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$LiveCredential
    )

    $udp = [string]$Config.LiveUdpAddress
    $lines = New-Object System.Collections.ArrayList
    foreach ($name in $LivePathMap.Keys) {
        $entry = $LivePathMap[$name]
        [void]$lines.Add("  ${name}:")
        if ($entry.Kind -eq 'sub') {
            $envName = Get-CamRelaySourceEnvName -PathName $name
            [void]$lines.Add("    # $($entry.Camera.Name) $($Config.LiveSubStream) - source supplied at launch by $envName")
            [void]$lines.Add('    source: publisher')
        }
        else {
            [void]$lines.Add("    # $($entry.Camera.Name) through the recording relay - no credential, no extra camera session")
            [void]$lines.Add("    source: $(Get-CamRelayUrl -Config $Config -PathName $entry.RelayPath)")
        }
    }

    $text = @"
# Generated by cam.recorder.ps1 for live view. Do not edit: the supervisor rewrites it at every start.
logLevel: info
logDestinations: [stdout]
readTimeout: 10s
writeTimeout: 10s
writeQueueSize: 512

authMethod: internal
authInternalUsers:
  - user: $($LiveCredential.UserName)
    pass: $(Get-CamRelayApiPassHash -Credential $LiveCredential)
    ips: ["127.0.0.1", "::1"]
    permissions:
      - action: read

api: no
metrics: no
pprof: no
playback: no
rtsp: no
rtmp: no
hls: no
srt: no
moq: no

webrtc: yes
webrtcAddress: $([string]$Config.LiveWhepAddress)
webrtcEncryption: no
webrtcAllowOrigins: []
webrtcTrustedProxies: []
webrtcLocalUDPAddress: $udp
webrtcLocalTCPAddress: ""
webrtcIPsFromInterfaces: no
webrtcIPsFromInterfacesList: []
webrtcAdditionalHosts: ["$(($udp -replace ':[0-9]+$', ''))"]
webrtcICEServers2: []

pathDefaults:
  rtspTransport: tcp
  sourceOnDemand: yes
  sourceOnDemandStartTimeout: 10s
  sourceOnDemandCloseAfter: $([int]$Config.LiveCloseAfterSeconds)s

paths:
$($lines -join "`n")
"@

    return ([regex]::Replace($text, "`r`n|`n", "`r`n"))
}


function Get-CamLiveSecretPath {
    <#
        Where the live login's secret lives: LiveSecretPath, or by default
        %LOCALAPPDATA%\IPCCTV\live_view_secret.txt. Local and per user on
        purpose - the repository normally sits on the WSL share, which holds
        no NTFS access control at all (icacls answers "Incorrect function"),
        so a file there could not be kept from other accounts.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    if (-not [string]::IsNullOrWhiteSpace([string]$Config.LiveSecretPath)) { return [string]$Config.LiveSecretPath }
    return (Join-Path -Path (Get-CamAutostartDir) -ChildPath 'live_view_secret.txt')
}


function Get-CamLiveCredential {
    <#
        The live login (user camlive, the secret as password), or $null when
        there is no secret file yet and -Create was not given.

        The secret is 32 random bytes as 64 hex characters in a plain ASCII
        file with no newline, so camui (Go, standard library only) can read it
        with os.ReadFile. It is created with an access control list that
        names only the current Windows user and no inherited entry, set at the
        moment the file is created (never a window in which it is readable by
        others), then moved into place atomically. DPAPI like
        relay_api_creds.xml would also keep other accounts out, but Go cannot
        open an Export-Clixml blob without a CLIXML parser and CryptUnprotectData.

        The secret is registered for redaction. Never put it on a command
        line, in the generated config (only its sha256 goes there) or in any
        log or status output.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [switch]$Create
    )

    $path = Get-CamLiveSecretPath -Config $Config

    if (-not (Test-Path -LiteralPath $path)) {
        if (-not $Create) { return $null }

        $dir = Split-Path -Path $path -Parent
        if ($dir -and -not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }

        $bytes = New-Object byte[] 32
        $random = [System.Security.Cryptography.RandomNumberGenerator]::Create()
        try { $random.GetBytes($bytes) } finally { $random.Dispose() }
        $hex = (($bytes | ForEach-Object { $_.ToString('x2') }) -join '')

        $security = New-Object System.Security.AccessControl.FileSecurity
        $security.SetAccessRuleProtection($true, $false)
        $me = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
        $security.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule(
                    $me, [System.Security.AccessControl.FileSystemRights]::FullControl, [System.Security.AccessControl.AccessControlType]::Allow)))

        $temp = "$path.tmp"
        if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Force }
        $stream = New-Object System.IO.FileStream($temp, [System.IO.FileMode]::CreateNew, [System.Security.AccessControl.FileSystemRights]::FullControl,
            [System.IO.FileShare]::None, 4096, [System.IO.FileOptions]::None, $security)
        try {
            $data = [System.Text.Encoding]::ASCII.GetBytes($hex)
            $stream.Write($data, 0, $data.Length)
        }
        finally { $stream.Dispose() }
        Move-CamFileAtomic -TempPath $temp -Path $path
    }

    $secret = ([System.IO.File]::ReadAllText($path)).Trim()
    if ($secret -notmatch '^[0-9a-f]{64}$') {
        throw "The live-view secret file '$path' does not hold 64 hex characters. Delete it and the supervisor creates a new one at its next start."
    }
    Register-CamSecret -Value $secret
    return (New-Object System.Management.Automation.PSCredential($script:CamLiveUser, (ConvertTo-SecureString $secret -AsPlainText -Force)))
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

        Never throws over rotation. A reader that holds the file without
        delete sharing - an editor, a tail tool, Get-Content -Wait whenever
        its poll is inside the file - makes the rename fail, and that
        exception used to end the supervisor loop and lose the line. The line
        is written to the unrotated file instead, the next write retries, and
        the first failure of each episode is logged.

        The check, rotate and append run under one named mutex per file,
        because the supervisor and the NVR host both write recorder.log:
        AppendAllText opens with FileShare.Read, so two writers in the same
        instant made one of them throw, and two rotating at once dropped a
        whole generation.

        Never throws over the append either. A reader holding the file with
        no write sharing, a full disk or a vanished directory loses that one
        line rather than the recorder; the drops are counted and the next line
        that is written is preceded by one notice saying how many and why.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Message,
        [int]$MaxBytes = 0,
        [switch]$NoTimestamp
    )

    if ($MaxBytes -le 0) { $MaxBytes = $script:CamDefaultLogMaxBytes }

    $stamp = ''
    if (-not $NoTimestamp) { $stamp = "$([DateTime]::Now.ToString('yyyy-MM-dd HH:mm:ss zzz')) " }
    $line = $stamp + (Get-CamRedacted -Text $Message)
    $text = $line + [Environment]::NewLine
    $notice = $null
    $dropNotice = $null
    $fullPath = $Path

    try {
        $dir = Split-Path -Path $Path -Parent
        if ($dir -and -not (Test-Path -LiteralPath $dir)) {
            [void](New-Item -ItemType Directory -Path $dir -Force)
        }

        $fullPath = [System.IO.Path]::GetFullPath($Path)
        $sha = [System.Security.Cryptography.SHA256]::Create()
        try { $hash = $sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($fullPath.ToLowerInvariant())) }
        finally { $sha.Dispose() }
        $mutex = New-Object System.Threading.Mutex($false, ('Local\cam.log.' + [System.BitConverter]::ToString($hash, 0, 8).Replace('-', '')))
        $owned = $false
        try {
            # A writer killed while holding it leaves it abandoned, which still
            # hands ownership to the next waiter. A timeout means a writer is
            # stuck; writing unserialised beats losing the line.
            try { $owned = $mutex.WaitOne(5000) }
            catch { $owned = ($_.Exception.InnerException -is [System.Threading.AbandonedMutexException]) }

            $info = New-Object System.IO.FileInfo($fullPath)
            if ($info.Exists -and $info.Length -ge $MaxBytes) {
                $rolled = "$fullPath.1"
                try {
                    [System.IO.File]::Delete($rolled)
                    [System.IO.File]::Move($fullPath, $rolled)
                    $script:CamLogRotationBlocked.Remove($fullPath)
                }
                catch {
                    if (-not $script:CamLogRotationBlocked.ContainsKey($fullPath)) {
                        $script:CamLogRotationBlocked[$fullPath] = $true
                        $reason = $_.Exception.Message
                        if ($_.Exception.InnerException) { $reason = $_.Exception.InnerException.Message }
                        $notice = "{0}log rotation blocked: {1} is {2} bytes, over its {3}-byte limit, and could not be renamed to {4}.1 - {5} Writing continues in place and rotation is retried on every write." -f `
                            $stamp, $info.Name, $info.Length, $MaxBytes, $info.Name, $reason
                        $text = $notice + [Environment]::NewLine + $text
                    }
                }
            }

            $dropped = $script:CamLogDropped[$fullPath]
            if ($dropped) {
                $dropNotice = "{0}log write failed: {1} line(s) dropped from this log since {2} - {3}" -f `
                    $stamp, $dropped.Lines, $dropped.Since, $dropped.Reason
                $text = $dropNotice + [Environment]::NewLine + $text
            }

            [System.IO.File]::AppendAllText($fullPath, $text, (New-Object System.Text.UTF8Encoding($false)))
            if ($dropped) { $script:CamLogDropped.Remove($fullPath) }
        }
        finally {
            if ($owned) { $mutex.ReleaseMutex() }
            $mutex.Dispose()
        }
    }
    catch {
        # This line is lost. A rotation notice it carried is not: clearing the
        # episode makes the next write that still cannot rotate say so again.
        if ($notice) { $script:CamLogRotationBlocked.Remove($fullPath) }
        $notice = $null
        $dropNotice = $null
        $reason = $_.Exception.Message
        if ($_.Exception.InnerException) { $reason = $_.Exception.InnerException.Message }
        $dropped = $script:CamLogDropped[$fullPath]
        if ($dropped) { $dropped.Lines = $dropped.Lines + 1 }
        else { $script:CamLogDropped[$fullPath] = @{ Lines = 1; Since = [DateTime]::Now.ToString('yyyy-MM-dd HH:mm:ss zzz'); Reason = $reason.Trim() } }
    }

    # The NVR window is the supervisor's own console, and the Operator asked
    # to see the log in it. Echoing here rather than at the call sites means
    # every line that reaches the file reaches the screen, with its timestamp
    # and already redacted, and nothing has to be remembered when a new log
    # line is added. Only the one registered file echoes: the per-camera logs
    # carry raw ffmpeg stderr and would bury it.
    if ($script:CamLogEchoPath -and $Path -eq $script:CamLogEchoPath) {
        if ($dropNotice) { Write-Host $dropNotice }
        if ($notice) { Write-Host $notice }
        Write-Host $line
    }
}


function Set-CamLogEcho {
    <#
        Registers one log file whose lines Write-CamLog also writes to the
        console, or clears the registration when called with no path.
    #>
    param([AllowEmptyString()][string]$Path = '')

    if ([string]::IsNullOrEmpty($Path)) { $script:CamLogEchoPath = $null }
    else { $script:CamLogEchoPath = $Path }
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
# Alerting
# ---------------------------------------------------------------------------
#
# Three channels, because a 24/7 recorder that stops has to reach the Operator
# whether he is at the machine, looking at the dashboard, or reading back
# afterwards:
#
#   toast      he finds out without looking
#   event log  a durable, greppable record that outlives every log rotation
#   status     a field the UI can render
#
# The status field is written by whoever writes status.json. The other two live
# here. Nothing in this section throws: an alert that fails must never be the
# thing that takes the recorder down.

function Get-CamAlertEventSource {
    <# The custom Application-log source this project writes under. #>
    return 'IPCCTV'
}


function Get-CamAlertFallbackEventSource {
    <#
        Where alerts go when the custom source is not registered.

        Registering an event source writes to HKLM and needs elevation, which
        an unelevated recorder does not have and must not demand. Measured
        2026-10-13 on this machine as a standard user: creating the source
        fails every way round - eventcreate.exe, New-EventLog and a direct
        registry write all return Access denied - but WRITING to the
        Application log under an ALREADY registered source succeeds. 'Application'
        is registered on every Windows install, so it is the one source that is
        always there and always writable.

        Every message carries the 'IPCCTV: ' prefix either way, so one grep
        finds them under either provider.
    #>
    return 'Application'
}


function Get-CamAlertMessagePrefix {
    return 'IPCCTV: '
}


function Test-CamAlertEventSource {
    <#
        Whether the custom event source is registered.

        Read from the registry, NOT with [EventLog]::SourceExists: that method
        walks every event log and an unelevated caller cannot open Security, so
        it THROWS "the source was not found, but some or all event logs could
        not be searched" instead of answering false. Measured 2026-10-13.
    #>
    param([string]$Source = (Get-CamAlertEventSource))

    try { return [bool](Test-Path -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\$Source") }
    catch { return $false }
}


function Write-CamEventLogEntry {
    <#
        Writes one Application-log entry under the custom source when it is
        registered and under the fallback source when it is not, and reports
        which. Never throws.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Message,
        [int]$EventId = 100,
        [ValidateSet('Information', 'Warning', 'Error')][string]$EntryType = 'Warning'
    )

    $result = @{ Written = $false; Source = ''; Registered = $false; Problem = '' }
    $text = (Get-CamAlertMessagePrefix) + (Get-CamRedacted -Text $Message)

    $sources = @()
    if (Test-CamAlertEventSource) { $result.Registered = $true; $sources += (Get-CamAlertEventSource) }
    $sources += (Get-CamAlertFallbackEventSource)

    foreach ($source in $sources) {
        try {
            Write-EventLog -LogName 'Application' -Source $source -EventId $EventId `
                -EntryType $EntryType -Message $text -ErrorAction Stop
            $result.Written = $true
            $result.Source = $source
            return $result
        }
        catch { $result.Problem = $_.Exception.Message }
    }

    return $result
}


function Show-CamToast {
    <#
        One Windows toast, unelevated, with no dependency to install.

        WinRT's ToastNotificationManager rather than BurntToast or msg.exe: it
        is in the box, it does not steal focus - which the Operator asked for
        explicitly - and the toast stays in the Action Center afterwards, so an
        alert raised while he was away is still there when he comes back.

        The AppUserModelID is Windows PowerShell's own. A toast has to be shown
        under an AUMID that has a Start menu entry, and inventing one would
        need a shortcut installed first. The cost is that the toast is headed
        "Windows PowerShell"; the body says what it is. Measured rendering
        2026-10-13.

        Never throws: a machine with notifications turned off must not take the
        recorder down.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Title,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Message
    )

    $result = @{ Shown = $false; Problem = '' }
    try {
        [void][Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
        [void][Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom, ContentType = WindowsRuntime]

        $xml = '<toast><visual><binding template="ToastText02"><text id="1">{0}</text><text id="2">{1}</text></binding></visual></toast>' -f `
            [System.Security.SecurityElement]::Escape((Get-CamRedacted -Text $Title)), `
            [System.Security.SecurityElement]::Escape((Get-CamRedacted -Text $Message))

        $document = New-Object Windows.Data.Xml.Dom.XmlDocument
        $document.LoadXml($xml)
        $toast = New-Object Windows.UI.Notifications.ToastNotification $document
        $aumid = '{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe'
        [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($aumid).Show($toast)
        $result.Shown = $true
    }
    catch { $result.Problem = $_.Exception.Message }

    return $result
}


function Send-CamAlert {
    <#
        Raises one alert on the toast and event-log channels and records it in
        the supervisor log. The caller publishes the state for the UI, because
        only the caller knows which document the fact belongs in.

        Returns what each channel did, so the log line can say so and a test
        can assert on it rather than on "an alert was sent".
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Title,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Message,
        [int]$EventId = 100,
        [ValidateSet('Information', 'Warning', 'Error')][string]$EntryType = 'Warning',
        [AllowEmptyString()][string]$LogPath = '',
        [int]$LogMaxBytes = 0
    )

    $title = Get-CamRedacted -Text $Title
    $body = Get-CamRedacted -Text $Message

    $toast = Show-CamToast -Title $title -Message $body
    $entry = Write-CamEventLogEntry -Message ("{0} - {1}" -f $title, $body) -EventId $EventId -EntryType $EntryType

    $channels = "toast {0}, event log {1}" -f `
        $(if ($toast.Shown) { 'shown' } else { 'FAILED (' + $toast.Problem + ')' }), `
        $(if ($entry.Written) { "written as source '" + $entry.Source + "'" } else { 'FAILED (' + $entry.Problem + ')' })

    if (-not [string]::IsNullOrEmpty($LogPath)) {
        try { Write-CamLog -Path $LogPath -Message ("ALERT {0} - {1} [{2}]" -f $title, $body, $channels) -MaxBytes $LogMaxBytes }
        catch { }
    }

    return @{ Title = $title; Message = $body; Toast = $toast; Event = $entry; Channels = $channels }
}


function Install-CamAlertEventSource {
    <#
        Registers the custom Application-log source. Needs elevation ONCE, and
        says so rather than failing obscurely when it does not have it.

        Everything works without it - alerts go to the Application log under
        the fallback source with the same prefix and the same text - so this is
        a tidiness switch, not a prerequisite.
    #>
    $source = Get-CamAlertEventSource
    $result = @{ Source = $source; AlreadyRegistered = $false; Registered = $false; Elevated = $false; Problem = '' }

    $result.Elevated = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)

    if (Test-CamAlertEventSource -Source $source) {
        $result.AlreadyRegistered = $true
        $result.Registered = $true
        return $result
    }

    if (-not $result.Elevated) {
        $result.Problem = "registering an event source writes to HKLM and needs an elevated session. Alerts still reach the Application log under the '$(Get-CamAlertFallbackEventSource)' source with the '$(Get-CamAlertMessagePrefix)' prefix until this is run elevated."
        return $result
    }

    try {
        # The message resource matters. A source with no message DLL leaves
        # Get-WinEvent's .Message EMPTY and the text only reachable through
        # .Properties[0].Value - measured 2026-10-13. EventCreate.exe carries a
        # generic resource covering event IDs 1-1000, which is the same one the
        # eventcreate tool points its own sources at, so the entry renders.
        New-EventLog -LogName 'Application' -Source $source `
            -MessageResourceFile (Join-Path -Path $env:SystemRoot -ChildPath 'System32\EventCreate.exe') -ErrorAction Stop
        $result.Registered = $true
    }
    catch { $result.Problem = $_.Exception.Message }

    return $result
}


function Get-CamAlertEventIdList {
    <#
        The event IDs this project writes. Needed to read the alerts back: the
        fallback source is shared, so the IDs are what separate our entries
        from everyone else's.

          100  the supervisor stopped unexpectedly and is being restarted
          101  it is recording again
          102  it stopped and the restart budget is spent
          103  the NVR window was closed, so nothing is recording
          110  one camera has stopped recording
          120  pruning is disabled: RootPath is unsafe or not the recorder's
               own archive, so the disk will fill
          121  pruning is blocked: free space is below ReserveGB and nothing
               deletable is left, so the disk is filling
          122  the age rule's circuit breaker held a run that would have
               deleted more than MaxAgePruneFraction of the archive
          130  the restreamer's control API refused the product's login, so
               readiness is not visible and every consumer is kept on the
               relay

        103 is its own ID rather than another 100 because the log is where the
        Operator reads back what happened, and "somebody closed the window" and
        "the supervisor died" need different answers from him.
    #>
    return @(100, 101, 102, 103, 110, 120, 121, 122, 130)
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


function Test-CamSegmentName {
    <#
        True only when $Name is a segment the recorder writes for the camera
        whose safe name is $SafeName: exactly the leaf Get-CamSegmentPattern
        builds for that camera under the CONFIGURED container, every strftime
        field matched as the digits ffmpeg writes. Any other extension - another
        container's, a .tmp, a second extension - is not a segment.

        Matched case-insensitively, like the filesystem it describes.
    #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Name,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$SafeName,
        [Parameter(Mandatory = $true)]$Config
    )

    if ([string]::IsNullOrEmpty($Name) -or [string]::IsNullOrEmpty($SafeName)) { return $false }

    $containerProfile = Get-CamContainerProfile -Config $Config
    $leaf = [System.IO.Path]::GetFileName((Get-CamSegmentPattern -CameraDir '.' -SafeName $SafeName -ContainerProfile $containerProfile))
    $regex = [regex]::Escape($leaf).Replace('%Y', '\d{4}').Replace('%m', '\d{2}').Replace('%d', '\d{2}').
        Replace('%H', '\d{2}').Replace('%M', '\d{2}').Replace('%S', '\d{2}').Replace('%z', '[+-]\d{4}')
    return ($Name -match ('^' + $regex + '$'))
}


function Get-CamPruneRefusal {
    <#
        Why $File must never be deleted, or '' when it may be. The ONE predicate
        both deletion paths - the pruner and the empty-segment cleanup - call,
        so neither can delete under looser rules than the other.

        A file may be deleted only when all of these hold:
          - neither it nor its folder is a reparse point
          - it resolves inside RootPath (Test-CamPathInside)
          - its folder is an immediate child of RootPath and not
            underscore-named - a camera folder
          - its name is a segment of that camera (Test-CamSegmentName)

        Returns a fixed reason so the caller can count foreign files instead of
        logging every one of them every cycle:
          'reparse point'             the file, or its folder, is one
          'outside the archive root'  Test-CamPathInside refused it
          'foreign'                   anything else the recorder did not write
    #>
    param(
        [Parameter(Mandatory = $true)][System.IO.FileInfo]$File,
        [Parameter(Mandatory = $true)]$Config
    )

    $reparse = [System.IO.FileAttributes]::ReparsePoint
    $root = [string]$Config.RootPath

    if (($File.Attributes -band $reparse) -ne 0) { return 'reparse point' }
    if (-not (Test-CamPathInside -Root $root -Path $File.FullName)) { return 'outside the archive root' }

    $folder = $File.Directory
    if (($folder.Attributes -band $reparse) -ne 0) { return 'reparse point' }
    $rootFull = [System.IO.Path]::GetFullPath($root).TrimEnd('\')
    $parent = [System.IO.Path]::GetDirectoryName($folder.FullName.TrimEnd('\')).TrimEnd('\')
    if (-not [string]::Equals($parent, $rootFull, [System.StringComparison]::OrdinalIgnoreCase)) { return 'foreign' }
    if ($folder.Name.StartsWith('_')) { return 'foreign' }
    if (-not (Test-CamSegmentName -Name $File.Name -SafeName $folder.Name -Config $Config)) { return 'foreign' }
    return ''
}


function Get-CamWallClock {
    <#
        The one clock every interval, age, deadline and "is it due" is measured
        on, in UTC. DateTime subtraction ignores the offset, so on local time a
        daylight-saving change moves every interval by an hour: at fall-back
        every timer stalls for an hour, at spring-forward every age jumps by
        3600 s. Compare its readings with file times only through
        LastWriteTimeUtc / CreationTimeUtc, and with stored timestamps only
        through Get-CamDateTimeOrNull. Local time is for display alone - log
        stamps, Format-CamTimestamp, segment names.

        Its own function so a test can move the clock.
    #>
    return [DateTime]::UtcNow
}


function Test-CamIntervalDue {
    <# True once $Seconds have passed since $Last, both readings of Get-CamWallClock. #>
    param(
        [Parameter(Mandatory = $true)][DateTime]$Last,
        [Parameter(Mandatory = $true)][double]$Seconds,
        [Parameter(Mandatory = $true)][DateTime]$Now
    )

    return (($Now - $Last).TotalSeconds -ge $Seconds)
}


function Get-CamPruneClockProblem {
    <#
        Why the timestamps of this run cannot be trusted to delete by, or ''
        when they can. Checked before the first deletion of every run that
        could delete anything; a problem refuses the whole run.

        $Candidates are every segment except the newest of each folder -
        before the per-folder strictly-older filter, so a disagreement between
        the two is caught here rather than filtered away - $Newest the newest
        segment of each folder (protected, never deleted), $Segments every
        segment the run looked at. All times are LastWriteTimeUtc, the one the
        age rule and the oldest-first order use, against a UTC $Now.

          1. The oldest candidate is not later than the newest segment. If it
             is, the order the pruner deletes in disagrees with which file it
             protects.
          2. No segment is written in the future of the wall clock, beyond one
             minute of timestamp skew.
          3. The wall clock is no further ahead of the newest segment than one
             stall threshold, SegmentSeconds x StallFactor - the age at which
             the watchdog already calls a camera stalled. A recorder writes its
             newest segment continuously, so a larger gap means the clock jumped
             forward or nothing is recording, and either would make the age
             rule delete footage that is not old.
    #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Candidates,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Newest,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Segments,
        [Parameter(Mandatory = $true)][DateTime]$Now,
        [Parameter(Mandatory = $true)]$Config
    )

    if ($Candidates.Count -eq 0 -or $Newest.Count -eq 0) { return '' }
    $stamp = { param($time) $time.ToLocalTime().ToString('yyyy-MM-dd HH:mm:ss', [System.Globalization.CultureInfo]::InvariantCulture) }

    $oldest = $Candidates[0]
    foreach ($file in $Candidates) { if ($file.LastWriteTimeUtc -lt $oldest.LastWriteTimeUtc) { $oldest = $file } }
    $newestFile = $Newest[0]
    foreach ($file in $Newest) { if ($file.LastWriteTimeUtc -gt $newestFile.LastWriteTimeUtc) { $newestFile = $file } }

    if ($oldest.LastWriteTimeUtc -gt $newestFile.LastWriteTimeUtc) {
        return ("the oldest candidate {0} (written {1}) is later than the newest segment {2} (written {3})" -f `
            $oldest.FullName, (& $stamp $oldest.LastWriteTimeUtc), $newestFile.FullName, (& $stamp $newestFile.LastWriteTimeUtc))
    }

    $limit = $Now.AddSeconds(60)
    foreach ($file in $Segments) {
        if ($file.LastWriteTimeUtc -gt $limit) {
            return ("{0} is written {1}, in the future of the wall clock {2}" -f $file.FullName, (& $stamp $file.LastWriteTimeUtc), (& $stamp $Now))
        }
    }

    $bound = [double]$Config.SegmentSeconds * [double]$Config.StallFactor
    $lag = ($Now - $newestFile.LastWriteTimeUtc).TotalSeconds
    if ($lag -gt $bound) {
        return ("the wall clock {0} is {1} s ahead of the newest segment {2} (written {3}), more than the {4} s stall threshold - the clock jumped or nothing is recording" -f `
            (& $stamp $Now), [int64]$lag, $newestFile.FullName, (& $stamp $newestFile.LastWriteTimeUtc), [int64]$bound)
    }
    return ''
}


function Get-CamAgePruneBreach {
    <#
        Why the age rule must not run this time, or '' when it may. $Expired
        are the files it would delete - past every other gate - and $Segments
        every segment the file gate accepted, which is the archive the share
        is taken of. A share above $Limit by count OR by bytes is a breach:
        a retention that suddenly reaches that far back is far more likely a
        mistyped MaxRetentionDays than an intended one.
    #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Expired,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Segments,
        [Parameter(Mandatory = $true)][double]$Limit
    )

    if ($Expired.Count -eq 0) { return '' }
    $expiredBytes = [double](@($Expired | Measure-Object -Property Length -Sum).Sum)
    $totalBytes = [double](@($Segments | Measure-Object -Property Length -Sum).Sum)
    $countShare = $Expired.Count / $Segments.Count
    $byteShare = $(if ($totalBytes -gt 0) { $expiredBytes / $totalBytes } else { 0.0 })
    if ($countShare -le $Limit -and $byteShare -le $Limit) { return '' }

    return ("the age rule would delete {0} of {1} segments ({2}%) and {3} of {4} GB ({5}%) of the bytes in the archive, more than MaxAgePruneFraction {6} allows" -f `
        $Expired.Count, $Segments.Count, (Format-CamNumber -Value ($countShare * 100) -Format '0.0'),
        (Format-CamNumber -Value ($expiredBytes / 1GB)), (Format-CamNumber -Value ($totalBytes / 1GB)),
        (Format-CamNumber -Value ($byteShare * 100) -Format '0.0'), (Format-CamNumber -Value $Limit -Format '0.##'))
}


function Get-CamArchiveRootProblem {
    <#
        Why $Root is unsafe to prune, or '' when it is not.

        The pruner treats every non-underscore folder of the root as a camera
        folder, so a root set too high by mistake would turn unrelated folders
        into camera folders. Refused:
          - a drive root (E:\) or a UNC share root (\\server\share)
          - the Windows directory, either Program Files, the user profile, the
            folder holding the profiles, or the repository directory - or ANY
            ancestor of one of them
        A folder inside one of those, such as the profile's Videos folder, is
        not refused here; the ownership marker decides that case.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Root)

    if ([string]::IsNullOrWhiteSpace($Root)) { return 'RootPath is empty' }

    try { $full = [System.IO.Path]::GetFullPath($Root) }
    catch { return ("RootPath '{0}' is not a usable path ({1})" -f $Root, $_.Exception.Message) }

    $ignoreCase = [System.StringComparison]::OrdinalIgnoreCase
    $trimmed = $full.TrimEnd('\')
    $volume = [System.IO.Path]::GetPathRoot($full)
    if ([string]::Equals($trimmed, $volume.TrimEnd('\'), $ignoreCase)) {
        return ("RootPath '{0}' is the root of the volume {1}" -f $Root, $volume)
    }

    $userProfile = [Environment]::GetFolderPath('UserProfile')
    $protected = @(
        @{ Label = 'the Windows directory'; Path = [Environment]::GetFolderPath('Windows') },
        @{ Label = 'Program Files'; Path = $env:ProgramW6432 },
        @{ Label = 'Program Files'; Path = [Environment]::GetFolderPath('ProgramFiles') },
        @{ Label = 'Program Files (x86)'; Path = [Environment]::GetFolderPath('ProgramFilesX86') },
        @{ Label = 'the user profile'; Path = $userProfile },
        @{ Label = 'the folder holding the user profiles'; Path = $(if ($userProfile) { Split-Path -Path $userProfile -Parent } else { '' }) },
        @{ Label = 'the recorder''s own directory'; Path = (Get-CamLibRoot) }
    )

    foreach ($entry in $protected) {
        if ([string]::IsNullOrWhiteSpace([string]$entry.Path)) { continue }
        $path = [System.IO.Path]::GetFullPath([string]$entry.Path).TrimEnd('\')
        if ([string]::Equals($path, $trimmed, $ignoreCase)) {
            return ("RootPath '{0}' is {1}" -f $Root, $entry.Label)
        }
        if ($path.StartsWith($trimmed + '\', $ignoreCase)) {
            return ("RootPath '{0}' contains {1} ({2})" -f $Root, $entry.Label, $path)
        }
    }
    return ''
}


function Get-CamArchiveMarkerPath {
    <# The file that marks RootPath as an archive this recorder created. #>
    param([Parameter(Mandatory = $true)]$Config)
    return (Join-Path -Path (Get-CamStateDirPath -Config $Config) -ChildPath 'archive-owner.json')
}


function Test-CamArchiveMarker {
    <# True when the ownership marker exists and is one this recorder wrote. #>
    param([Parameter(Mandatory = $true)]$Config)

    $path = Get-CamArchiveMarkerPath -Config $Config
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { return $false }
    try { $marker = ConvertFrom-Json -InputObject (Get-CamFileText -Path $path) }
    catch { return $false }
    return ($null -ne $marker -and [string]$marker.Owner -eq 'IPCCTV recorder')
}


function Get-CamArchiveClaimProblem {
    <#
        Why the recorder may not claim RootPath as its own archive, or '' when
        it may. Claimable means the root passes Get-CamArchiveRootProblem and
        holds nothing the recorder did not write: every top-level entry is
        underscore-named (the recorder's own folders) or a real folder holding
        only files Get-CamPruneRefusal accepts. An empty root qualifies.

        Read-only. Lists the blocking entries so the Operator can see what to
        move.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $root = [string]$Config.RootPath
    $problem = Get-CamArchiveRootProblem -Root $root
    if ($problem) { return $problem }
    if (-not (Test-Path -LiteralPath $root -PathType Container)) { return ("RootPath '{0}' does not exist" -f $root) }

    $blockers = New-Object System.Collections.ArrayList
    foreach ($entry in @(Get-ChildItem -LiteralPath $root -Force -ErrorAction Stop)) {
        if ($entry.Name.StartsWith('_')) { continue }
        if (-not $entry.PSIsContainer) { [void]$blockers.Add(("{0} (a file at the top level)" -f $entry.Name)); continue }
        if (($entry.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            [void]$blockers.Add(("{0} (a reparse point)" -f $entry.Name)); continue
        }
        foreach ($child in @(Get-ChildItem -LiteralPath $entry.FullName -Force -ErrorAction Stop)) {
            if ($child.PSIsContainer) {
                [void]$blockers.Add(("{0}\{1} (a folder inside a camera folder)" -f $entry.Name, $child.Name)); break
            }
            if (Get-CamPruneRefusal -File $child -Config $Config) {
                [void]$blockers.Add(("{0}\{1} (not a recorder segment)" -f $entry.Name, $child.Name)); break
            }
        }
    }

    if ($blockers.Count -eq 0) { return '' }
    $shown = @($blockers | Select-Object -First 5) -join ', '
    return ("RootPath '{0}' holds {1} top-level entr{2} the recorder did not write: {3}" -f `
        $root, $blockers.Count, $(if ($blockers.Count -eq 1) { 'y' } else { 'ies' }), $shown)
}


function Initialize-CamArchiveOwner {
    <#
        Claims RootPath as this recorder's archive by writing the ownership
        marker - only when the root is claimable (Get-CamArchiveClaimProblem)
        and only when no marker exists yet. The pruner deletes nothing under a
        root without one.

        Returns Owned (a marker is in place), Created (this call wrote it) and
        Problem (why it is not owned).
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $result = @{ Owned = $false; Created = $false; Problem = ''; Path = (Get-CamArchiveMarkerPath -Config $Config) }
    if (Test-CamArchiveMarker -Config $Config) { $result.Owned = $true; return $result }

    $result.Problem = Get-CamArchiveClaimProblem -Config $Config
    if ($result.Problem) { return $result }

    $marker = [ordered]@{
        Owner     = 'IPCCTV recorder'
        Schema    = 1
        RootPath  = [string]$Config.RootPath
        CreatedAt = Format-CamTimestamp -Value (Get-CamWallClock)
        Note      = 'The recorder prunes footage only under a root holding this file. It wrote it because the root held nothing but its own files. Do not copy it into another folder.'
    }
    $dir = Split-Path -Path $result.Path -Parent
    if (-not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }
    $temp = "$($result.Path).tmp"
    [System.IO.File]::WriteAllText($temp, (ConvertTo-Json -InputObject $marker) + [Environment]::NewLine, (New-Object System.Text.UTF8Encoding($false)))
    Move-CamFileAtomic -TempPath $temp -Path $result.Path

    $result.Owned = $true
    $result.Created = $true
    return $result
}


function Get-CamPruneRootRefusal {
    <#
        Why nothing at all may be deleted under RootPath, or '' when the
        per-file rules may run. Called by both deletion paths before they look
        at a single file:
          - the root must pass Get-CamArchiveRootProblem
          - it must hold the ownership marker
        -DryRun previews the next start: a root with no marker that the next
        start WOULD claim is not refused, and nothing is written.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [switch]$DryRun
    )

    $problem = Get-CamArchiveRootProblem -Root ([string]$Config.RootPath)
    if ($problem) { return ("{0}. Point RootPath at a folder of its own" -f $problem) }
    if (Test-CamArchiveMarker -Config $Config) { return '' }

    $marker = Get-CamArchiveMarkerPath -Config $Config
    $claim = Get-CamArchiveClaimProblem -Config $Config
    if ($claim) {
        return ("there is no ownership marker at {0} and the recorder cannot claim the root: {1}. Move those entries out of RootPath, or point RootPath at a folder of its own, then restart the recorder" -f $marker, $claim)
    }
    if ($DryRun) { return '' }
    return ("there is no ownership marker at {0}. The root qualifies - restart the recorder and it claims it" -f $marker)
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


function Set-CamStatusUnexpectedStop {
    <#
        Marks the status file as having stopped unexpectedly, in place.

        Written by the NVR host rather than by the supervisor, because the case
        that matters is the one where the supervisor was killed and never got
        to write anything - the file is left saying "running" with a dead pid,
        which is how 20:30 on 2026-10-12 looked for half an hour.

        Patches rather than rewrites: everything else in the document - the
        camera rows, the disk figures, the event tail - is the last true
        reading and the UI still wants it. Through the same temp-then-rename
        path as every other writer, so no reader sees a half-written file.

        Never throws. Returns $true when the file was patched.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Reason,
        [int]$SupervisorPid = 0,
        [string]$DetectedBy = 'nvr host'
    )

    $path = Get-CamStatusFilePath -Config $Config
    $record = [pscustomobject]@{
        At            = Format-CamTimestamp -Value (Get-CamWallClock)
        Reason        = Get-CamRedacted -Text $Reason
        SupervisorPid = $SupervisorPid
        DetectedBy    = $DetectedBy
    }

    try {
        $document = Get-CamStatusSnapshot -Config $Config
        if (-not $document) {
            $document = [pscustomobject]@{ Schema = 1; SupervisorPid = $SupervisorPid; UpdatedAt = $record.At }
        }

        $document | Add-Member -NotePropertyName 'State' -NotePropertyValue 'stopped' -Force
        $document | Add-Member -NotePropertyName 'StopKind' -NotePropertyValue 'unexpected' -Force
        $document | Add-Member -NotePropertyName 'LastUnexpectedStop' -NotePropertyValue $record -Force
        $document | Add-Member -NotePropertyName 'UpdatedAt' -NotePropertyValue $record.At -Force

        $json = Get-CamRedacted -Text (ConvertTo-Json -InputObject $document -Depth 6)
        $dir = Split-Path -Path $path -Parent
        if ($dir -and -not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }
        $temp = "$path.tmp"
        [System.IO.File]::WriteAllText($temp, $json + [Environment]::NewLine, (New-Object System.Text.UTF8Encoding($false)))
        Move-CamFileAtomic -TempPath $temp -Path $path
        return $true
    }
    catch { return $false }
}


function Set-CamStatusWindowClosed {
    <#
        Marks the status file as stopped because the NVR window was closed.

        Written by the warden, which is the only thing that sees the window
        go: the supervisor took CTRL_CLOSE_EVENT with the window and the
        kernel has already taken it, so it never got to write anything and the
        file is left claiming it is running.

        A SEPARATE record from LastUnexpectedStop, not a reuse of it. The two
        say different things - one is a malfunction, one is a deliberate close
        the system cannot tell from an accidental one - and a reader that
        cannot distinguish them cannot tell the Operator whether to worry.

        Patches rather than rewrites, through the same temp-then-rename path as
        every other writer, exactly as Set-CamStatusUnexpectedStop does.

        Never throws. Returns $true when the file was patched.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Reason,
        [AllowEmptyString()][string]$WindowTitle = '',
        [string]$DetectedBy = 'nvr warden'
    )

    $path = Get-CamStatusFilePath -Config $Config
    $record = [pscustomobject]@{
        At          = Format-CamTimestamp -Value (Get-CamWallClock)
        Reason      = Get-CamRedacted -Text $Reason
        WindowTitle = $WindowTitle
        DetectedBy  = $DetectedBy
    }

    try {
        $document = Get-CamStatusSnapshot -Config $Config
        if (-not $document) {
            $document = [pscustomobject]@{ Schema = 1; SupervisorPid = 0; UpdatedAt = $record.At }
        }

        $document | Add-Member -NotePropertyName 'State' -NotePropertyValue 'stopped' -Force
        $document | Add-Member -NotePropertyName 'StopKind' -NotePropertyValue 'window-closed' -Force
        $document | Add-Member -NotePropertyName 'LastWindowClose' -NotePropertyValue $record -Force
        $document | Add-Member -NotePropertyName 'UpdatedAt' -NotePropertyValue $record.At -Force

        $json = Get-CamRedacted -Text (ConvertTo-Json -InputObject $document -Depth 6)
        $dir = Split-Path -Path $path -Parent
        if ($dir -and -not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }
        $temp = "$path.tmp"
        [System.IO.File]::WriteAllText($temp, $json + [Environment]::NewLine, (New-Object System.Text.UTF8Encoding($false)))
        Move-CamFileAtomic -TempPath $temp -Path $path
        return $true
    }
    catch { return $false }
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
    if ($updated) { $state.AgeSeconds = [math]::Round(((Get-CamWallClock) - $updated).TotalSeconds, 1) }

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


# ===========================================================================
# Process identity, window handles and the dashboard's lifecycle
#
# Everything below answers one question: which processes belong to this
# project, and how do they get stopped on purpose rather than by hand.
#
# The recorder has always had a supervisor, a status file and a working -Stop.
# The viewers never did: each pane ran its own `while ($true)` loop with
# nothing owning it, so closing the Windows Terminal window left six panes
# running headless, each still holding an mpv and one of the camera's two
# RTSP sessions. This section is the missing half.
#
# The identification rule throughout is: a process is what it OWNS, never
# what it is called. Both ways of getting that wrong have already cost this
# project a live session - see the comments on Get-CamProcessInventory.
# ===========================================================================


function Import-CamWindowApi {
    <#
        The user32/dwmapi calls this project needs to find, place and close
        one window.

        FindByTitle enumerates top-level windows rather than reading
        Process.MainWindowTitle, because Windows Terminal serves several
        windows from ONE process: MainWindowHandle is then whichever window
        Windows enumerates first, not the one whose title matched, and
        stopping the process would take every terminal window on the desktop
        with it - including the Operator's own, which is exactly what happened
        on 2026-10-12.

        The window class is checked as well as the title so a stray window of
        some other application that happens to carry our title can never be
        sent a close.
    #>

    # A type cannot be added twice in one process, and this library is
    # dot-sourced by the launcher, the warden and the control surface alike.
    if ('CamWin' -as [type]) { return }

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
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] private static extern int GetClassName(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll")] private static extern bool IsWindowVisible(IntPtr h);
    [DllImport("user32.dll")] public static extern bool IsWindow(IntPtr h);
    [DllImport("user32.dll", SetLastError = true)] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
    [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll", SetLastError = true)] public static extern bool PostMessage(IntPtr h, uint msg, IntPtr w, IntPtr l);
    [DllImport("user32.dll")] private static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
    [DllImport("dwmapi.dll")] public static extern int DwmGetWindowAttribute(IntPtr h, int attr, out RECT r, int size);

    [StructLayout(LayoutKind.Sequential)]
    public struct RECT { public int Left; public int Top; public int Right; public int Bottom; }

    // Windows Terminal's top-level window class. Checked alongside the title
    // so only a terminal window is ever a candidate for a close.
    public const string TerminalClass = "CASCADIA_HOSTING_WINDOW_CLASS";

    private static string TextOf(IntPtr h)
    {
        int len = GetWindowTextLength(h);
        if (len == 0) { return string.Empty; }
        StringBuilder sb = new StringBuilder(len + 2);
        GetWindowText(h, sb, sb.Capacity);
        return sb.ToString();
    }

    private static string ClassOf(IntPtr h)
    {
        StringBuilder sb = new StringBuilder(256);
        GetClassName(h, sb, sb.Capacity);
        return sb.ToString();
    }

    public static IntPtr[] FindByTitle(string title)
    {
        List<IntPtr> found = new List<IntPtr>();
        EnumWindows(delegate(IntPtr h, IntPtr p) {
            if (!IsWindowVisible(h)) { return true; }
            if (string.Equals(TextOf(h), title, StringComparison.Ordinal)) { found.Add(h); }
            return true;
        }, IntPtr.Zero);
        return found.ToArray();
    }

    // The same search restricted to Windows Terminal's own window class.
    public static IntPtr[] FindTerminalByTitle(string title)
    {
        List<IntPtr> found = new List<IntPtr>();
        EnumWindows(delegate(IntPtr h, IntPtr p) {
            if (!IsWindowVisible(h)) { return true; }
            if (ClassOf(h).IndexOf(TerminalClass, StringComparison.OrdinalIgnoreCase) < 0) { return true; }
            if (string.Equals(TextOf(h), title, StringComparison.Ordinal)) { found.Add(h); }
            return true;
        }, IntPtr.Zero);
        return found.ToArray();
    }

    public static string TitleOf(IntPtr h) { return TextOf(h); }

    // Everything FindTerminalByTitle requires of a candidate, asked of ONE
    // handle instead of searched for: the window still exists, it is visible,
    // and it is Windows Terminal's. A handle latched minutes ago is only still
    // a terminal window if all three hold - IsWindow alone would believe a
    // handle value the desktop has since recycled onto something else.
    public static bool IsTerminalWindow(IntPtr h)
    {
        if (h == IntPtr.Zero) { return false; }
        if (!IsWindow(h)) { return false; }
        if (!IsWindowVisible(h)) { return false; }
        return ClassOf(h).IndexOf(TerminalClass, StringComparison.OrdinalIgnoreCase) >= 0;
    }

    public static int ProcessIdOf(IntPtr h)
    {
        uint pid;
        GetWindowThreadProcessId(h, out pid);
        return (int)pid;
    }
}
'@
}


function Get-CamTerminalWindow {
    <# Every visible Windows Terminal window carrying exactly this title. #>
    param([Parameter(Mandatory = $true)][string]$Title)

    Import-CamWindowApi
    return @([CamWin]::FindTerminalByTitle($Title))
}


# ---------------------------------------------------------------------------
# The window record: which HWND a warden is actually watching
#
# A Windows Terminal window's title follows its ACTIVE TAB, and this machine's
# windowingBehavior is useExisting, so a foreign console can be adopted into
# one of our windows as a second tab and the window then answers to that tab's
# title instead of ours. Measured on 2026-10-14: the NVR warden was healthily
# watching hwnd 0x1190726 while that window was titled 'nk', and
# Get-CamTerminalWindow -Title cam.nvr found nothing - so -Status reported the
# NVR window as gone with the host, the warden, the job and six ffmpeg all
# alive, and the Operator had no way to tell which window he must not close.
#
# The warden already resolves the HWND once and polls IsWindow on it, which is
# why safety was never affected. It just never said which handle that was.
# These functions are that missing channel, and nothing more: the warden
# publishes the handle it latched, readers prefer it over the title search.
# Item [81].
# ---------------------------------------------------------------------------

function Get-CamWindowRecordPath {
    <#
        Where the warden for this window title publishes its HWND. One file
        per title, in the same _state directory as status.json, so a reader
        needs nothing but the config to find it.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Title
    )

    $safe = ($Title -replace '[^A-Za-z0-9._-]', '_')
    return (Join-Path -Path (Get-CamStateDirPath -Config $Config) -ChildPath ("window-{0}.json" -f $safe))
}


function Set-CamWindowRecord {
    <#
        Publishes the handle a warden has latched onto, with the title the
        window carried at the time and the pids on both ends of the watch.

        Through the same temp-then-rename path as status.json, so no reader
        ever sees a half-written record.

        Never throws. Returns $true when the record was written. The caller is
        a warden whose real duty is holding a job handle, and an observation
        channel must never be able to cost it that.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Title,
        [Parameter(Mandatory = $true)][IntPtr]$Hwnd,
        [AllowEmptyString()][string]$JobName = ''
    )

    try {
        Import-CamWindowApi
        $path = Get-CamWindowRecordPath -Config $Config -Title $Title
        $record = [pscustomobject]@{
            Schema = 1
            # The title as it was when the handle was latched. The whole point
            # of the record is that the CURRENT title may differ from it.
            Title       = $Title
            Hwnd        = $Hwnd.ToInt64()
            HwndHex     = ("0x{0:X}" -f $Hwnd.ToInt64())
            TerminalPid = [CamWin]::ProcessIdOf($Hwnd)
            WardenPid   = $PID
            JobName     = $JobName
            At          = Format-CamTimestamp -Value (Get-CamWallClock)
        }

        $json = ConvertTo-Json -InputObject $record -Depth 4
        $dir = Split-Path -Path $path -Parent
        if ($dir -and -not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }
        $temp = "$path.tmp"
        [System.IO.File]::WriteAllText($temp, $json + [Environment]::NewLine, (New-Object System.Text.UTF8Encoding($false)))
        Move-CamFileAtomic -TempPath $temp -Path $path
        return $true
    }
    catch { return $false }
}


function Get-CamWindowRecord {
    <#
        The published record for this window title, or $null when there is
        nothing readable there. Never throws: absence is the normal state for
        a window no warden is watching.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Title
    )

    $path = Get-CamWindowRecordPath -Config $Config -Title $Title
    if (-not (Test-Path -LiteralPath $path)) { return $null }

    try { $text = Get-CamFileText -Path $path }
    catch { return $null }

    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    try { return (ConvertFrom-Json -InputObject $text) }
    catch { return $null }
}


function Test-CamWindowRecordLive {
    <#
        Whether the warden a record names is still running: that pid is alive
        AND that process was already running when the record was written, so
        a pid the system has since recycled onto something else is not taken
        for the warden.

        This, and not the handle alone, is what makes a record valid. A
        handle can outlive the NVR indefinitely: the NVR can be a TAB in the
        Operator's own window, which stays open with his tabs in it after the
        NVR is gone. Believing the record on the handle alone reported that
        window as the NVR's after an orderly stop, and the start path then
        waited for it to close and threw - three times on 2026-10-14, 5 min
        34 s with nothing recorded. Item [84].

        Creation time from Win32_Process rather than Get-Process: StartTime
        needs a handle to the process and comes back empty for one this
        process may not open, which would read as "not the warden".

        A query that FAILS is not an answer, and returns $true: a record is
        deleted on the strength of this, and deleting a live warden's record
        costs -Status the only name it has for the window.
    #>
    param([Parameter(Mandatory = $true)]$Record)

    $wardenPid = 0
    if (-not [int]::TryParse([string]$Record.WardenPid, [ref]$wardenPid) -or $wardenPid -le 0) { return $false }
    $at = Get-CamDateTimeOrNull -Value $Record.At
    if ($null -eq $at) { return $false }

    try {
        $process = @(Get-CimInstance -ClassName Win32_Process -Filter ("ProcessId = {0}" -f $wardenPid) -ErrorAction Stop)
    }
    catch { return $true }

    if ($process.Count -eq 0 -or $null -eq $process[0].CreationDate) { return $false }
    return ($process[0].CreationDate.ToUniversalTime() -le $at.ToUniversalTime())
}


function Remove-CamWindowRecord {
    <#
        Drops the record. Called by the warden on its way out, so a file left
        behind cannot be mistaken for a live window.

        It is only half the defence, and deliberately so: a warden that was
        TERMINATED - which is exactly what an orderly stop does to it - never
        runs this at all. That is why every reader re-checks the warden with
        Test-CamWindowRecordLive rather than believing the file, and removes
        what it finds stale.

        -Expected removes the record only while it is still THAT record - the
        same warden and the same publication time. A reader that found a stale
        record must not delete the one a new warden published in between.

        Never throws. Returns $true when the record it was asked to remove is
        no longer at the path.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Title,
        $Expected = $null
    )

    try {
        $path = Get-CamWindowRecordPath -Config $Config -Title $Title
        if (-not (Test-Path -LiteralPath $path)) { return $true }
        if ($Expected) {
            $current = Get-CamWindowRecord -Config $Config -Title $Title
            if ($current -and ([string]$current.WardenPid -cne [string]$Expected.WardenPid -or [string]$current.At -cne [string]$Expected.At)) {
                return $true
            }
        }
        Remove-Item -LiteralPath $path -Force
        return $true
    }
    catch { return $false }
}


function Get-CamWindowPresence {
    <#
        Is that window there, and which window is it? By published HWND when a
        warden has published one that is still a terminal window, and by the
        title search otherwise.

        Returned rather than printed, because -Status, the launcher's
        already-open test and the launcher's post-clear wait all need the same
        answer and used to each ask the title search for it.

        THE FALLBACK IS ON LIVENESS, not merely on the record existing, and on
        two kinds of it. A record whose WARDEN is dead is stale even when its
        window is open - the window can be shared with the Operator's own tabs
        and outlive the NVR - so it is removed here; see
        Test-CamWindowRecordLive, item [84]. A record whose HANDLE is dead is
        ignored but left for its warden, which is about to notice and remove
        it. Either way the title search runs. That matters during a restart:
        the old record can still be on disk for the seconds between the window
        going and the new warden publishing, and reporting "absent" there -
        with a new window plainly open and correctly titled - would be a fresh
        way of being wrong rather than a fix.

        CurrentTitle is read from the window every time. When it differs from
        the title asked for, the window is ours and something else is showing
        in it, which is the one fact the Operator cannot get any other way.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Title
    )

    Import-CamWindowApi

    $record = $null
    try { $record = Get-CamWindowRecord -Config $Config -Title $Title } catch { $record = $null }

    if ($record -and -not (Test-CamWindowRecordLive -Record $record)) {
        [void](Remove-CamWindowRecord -Config $Config -Title $Title -Expected $record)
        $record = $null
    }

    if ($record -and $record.Hwnd) {
        $hwnd = [IntPtr]::new([int64]$record.Hwnd)
        if ([CamWin]::IsTerminalWindow($hwnd)) {
            $current = [CamWin]::TitleOf($hwnd)
            return [pscustomobject]@{
                Title        = $Title
                Found        = $true
                Count        = 1
                Hwnd         = $hwnd
                HwndHex      = ("0x{0:X}" -f $hwnd.ToInt64())
                CurrentTitle = $current
                TitleChanged = ($current -cne $Title)
                Source       = 'hwnd'
                TerminalPid  = [CamWin]::ProcessIdOf($hwnd)
                WardenPid    = [int]$record.WardenPid
                RecordAt     = [string]$record.At
            }
        }
    }

    $found = @([CamWin]::FindTerminalByTitle($Title))
    $hwnd = [IntPtr]::Zero
    if ($found.Count -ge 1) { $hwnd = $found[0] }

    return [pscustomobject]@{
        Title        = $Title
        Found        = ($found.Count -ge 1)
        Count        = $found.Count
        Hwnd         = $hwnd
        HwndHex      = $(if ($hwnd -ne [IntPtr]::Zero) { "0x{0:X}" -f $hwnd.ToInt64() } else { '' })
        CurrentTitle = $(if ($hwnd -ne [IntPtr]::Zero) { $Title } else { '' })
        TitleChanged = $false
        Source       = $(if ($found.Count -ge 1) { 'title' } else { 'none' })
        TerminalPid  = $(if ($hwnd -ne [IntPtr]::Zero) { [CamWin]::ProcessIdOf($hwnd) } else { 0 })
        WardenPid    = 0
        RecordAt     = ''
    }
}


function Stop-CamTerminalWindow {
    <#
        Closes every Windows Terminal window with this title, and returns how
        many were still there afterwards.

        WM_CLOSE and then wait. Nothing else: this never touches the
        foreground window and never synthesises input.

        An earlier version did. It polled once 400 ms after the post, found
        the window still there, concluded that Windows Terminal's "close all
        tabs?" overlay must be waiting for a keystroke, and stole the
        foreground to send it an Enter - about a second of the Operator's
        focus on every stop. The conclusion was wrong: the window had simply
        not finished closing. Measured on 2026-10-12 against the live
        six-pane grid, posting WM_CLOSE and sending NOTHING:

          panes already dead   window gone in under 800 ms, no overlay
          panes still running  window gone in under 1500 ms, no overlay,
                               and the warden then stopped the panes

        So a window of OURS never raises that confirmation: it is one tab,
        holding six panes in the grid's case, and the confirmation is about
        closing several TABS.

        It does raise it once a foreign console has been ADOPTED into the
        window as a second tab. Measured on 2026-10-13 against the NVR window,
        which had acquired a tab named 'nk': the first WM_CLOSE left the
        window open with the overlay waiting, 25 s later nothing had closed,
        and the window was still there three hours afterwards. A second
        WM_CLOSE answered the overlay and the close then completed in under a
        second. So a return of 1 from this function can mean "an overlay is
        waiting for an answer", and calling it again is a reasonable response -
        but it is the Operator's windowingBehavior that decides whether a
        foreign tab can land there at all.

        A window that will not close within the timeout is REPORTED, never
        forced.

        Never Stop-Process on the window's owner. One Windows Terminal process
        owns every terminal window on the desktop.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Title,

        [int]$TimeoutSeconds = 10
    )

    Import-CamWindowApi
    $existing = @(Get-CamTerminalWindow -Title $Title)
    if ($existing.Count -eq 0) { return 0 }

    foreach ($hwnd in $existing) {
        [void][CamWin]::PostMessage($hwnd, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero)
    }

    $deadline = (Get-CamWallClock).AddSeconds($TimeoutSeconds)
    while ((Get-CamWallClock) -lt $deadline) {
        Start-Sleep -Milliseconds 250
        if (@(Get-CamTerminalWindow -Title $Title).Count -eq 0) { return 0 }
    }

    return @(Get-CamTerminalWindow -Title $Title).Count
}


function Start-CamDetachedProcess {
    <#
        Starts powershell.exe running one script in a console of its OWN with
        no window at all, and returns the Process object.

        Every background helper in this project is launched this way, and the
        reason is measured rather than stylistic. The obvious
        Start-Process -WindowStyle Hidden gets both halves wrong:

        - Start-Process uses ShellExecute and passes no console flag, so the
          child INHERITS the launcher's console. It is then one of the
          processes attached to whatever terminal the launcher was typed into,
          and closing that terminal delivers CTRL_CLOSE_EVENT to the child
          too. For a warden holding the only handle to a KILL_ON_JOB_CLOSE
          job, that means an unrelated terminal window closing takes down
          everything the job protects.

        - A console created WITH a window is handed to the default terminal
          application. Measured on 2026-10-12 on this machine: creating a
          console with a window activated OpenConsole.exe -Embedding and the
          console arrived as a TAB in the Operator's existing Windows Terminal
          window, because windowingBehavior is useExisting. Closing that
          window then kills the process. That is exactly how the recording
          supervisor died four times in one day.

        CREATE_NO_WINDOW, which is what ProcessStartInfo.CreateNoWindow sets
        when UseShellExecute is false, avoids both. Measured the same day, the
        child reporting on itself: GetConsoleProcessList returns only its own
        pid, GetConsoleWindow returns NULL, no OpenConsole -Embedding is
        activated and no tab appears. Write-Host still works.

        DETACHED_PROCESS was measured too and is not usable: powershell.exe
        with no console at all never reached its first statement.
    #>
    param(
        # The script to run, as source text. Passed as -EncodedCommand, so it
        # may be several lines and may contain quotes.
        [Parameter(Mandatory = $true)][string]$Script
    )

    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($Script))
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = Join-Path -Path $env:SystemRoot -ChildPath 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $psi.Arguments = ('-NoProfile -ExecutionPolicy Bypass -EncodedCommand {0}' -f $encoded)
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $process = [System.Diagnostics.Process]::Start($psi)

    # Touching Handle first: ExitCode and HasExited can otherwise come back
    # null on a process this one never waits for.
    [void]$process.Handle
    return $process
}
function Import-CamJobApi {
    <#
        Win32 job objects - the purpose-built primitive for "these processes
        die when their owner does".

        A job with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE terminates every process
        in it the moment the LAST handle to the job closes. Children inherit
        membership at creation, so a pane's mpv joins without being told.

        The trap, measured on 2026-10-12: a process that opens the job to
        assign itself must CLOSE ITS HANDLE AFTERWARDS. Membership is not a
        handle. With six panes each holding an open handle, the owner's death
        closed nothing and all six survived; with the handles closed, killing
        the owner took all six panes and their mpv in under two seconds.
    #>

    if ('CamJob' -as [type]) { return }

    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class CamJob
{
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateJobObject(IntPtr sec, string name);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr OpenJobObject(uint access, bool inherit, string name);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetInformationJobObject(IntPtr job, int infoClass, IntPtr info, uint len);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool AssignProcessToJobObject(IntPtr job, IntPtr proc);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool IsProcessInJob(IntPtr proc, IntPtr job, out bool result);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool TerminateJobObject(IntPtr job, uint exitCode);
    [DllImport("kernel32.dll", SetLastError = true)]
    public static extern bool CloseHandle(IntPtr h);
    [DllImport("kernel32.dll")] private static extern IntPtr GetCurrentProcess();

    private const int ExtendedLimitInformation = 9;
    private const uint KillOnJobClose = 0x2000;
    private const int AlreadyExists = 183;

    // JOB_OBJECT_ALL_ACCESS, minus nothing we need: assign, set, query, terminate.
    private const uint AllAccess = 0x1F001F;

    [StructLayout(LayoutKind.Sequential)]
    private struct BASIC_LIMIT {
        public long PerProcessUserTimeLimit, PerJobUserTimeLimit;
        public uint LimitFlags;
        public UIntPtr MinimumWorkingSetSize, MaximumWorkingSetSize;
        public uint ActiveProcessLimit;
        public UIntPtr Affinity;
        public uint PriorityClass, SchedulingClass;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct IO_COUNTERS { public ulong R, W, O, RT, WT, OT; }

    [StructLayout(LayoutKind.Sequential)]
    private struct EXTENDED_LIMIT {
        public BASIC_LIMIT BasicLimitInformation;
        public IO_COUNTERS IoInfo;
        public UIntPtr ProcessMemoryLimit, JobMemoryLimit, PeakProcessMemoryUsed, PeakJobMemoryUsed;
    }

    private static void SetKillOnClose(IntPtr job, bool armed)
    {
        EXTENDED_LIMIT ext = new EXTENDED_LIMIT();
        ext.BasicLimitInformation.LimitFlags = armed ? KillOnJobClose : 0;
        int len = Marshal.SizeOf(typeof(EXTENDED_LIMIT));
        IntPtr buffer = Marshal.AllocHGlobal(len);
        try {
            Marshal.StructureToPtr(ext, buffer, false);
            if (!SetInformationJobObject(job, ExtendedLimitInformation, buffer, (uint)len)) {
                throw new InvalidOperationException("SetInformationJobObject failed, error " + Marshal.GetLastWin32Error());
            }
        }
        finally { Marshal.FreeHGlobal(buffer); }
    }

    // Creates the job and arms it. Refuses an existing one: a second warden
    // must never take over the panes of the first.
    public static IntPtr CreateExclusive(string name)
    {
        IntPtr job = CreateJobObject(IntPtr.Zero, name);
        int err = Marshal.GetLastWin32Error();
        if (job == IntPtr.Zero) {
            throw new InvalidOperationException("CreateJobObject failed, error " + err);
        }
        if (err == AlreadyExists) {
            CloseHandle(job);
            throw new InvalidOperationException("a job object named '" + name + "' already exists");
        }
        try { SetKillOnClose(job, true); }
        catch { CloseHandle(job); throw; }
        return job;
    }

    // Lets the owner walk away without killing anything - used when the
    // warden cannot establish what it is supposed to be watching.
    public static void Disarm(IntPtr job) { SetKillOnClose(job, false); }

    public static bool Exists(string name)
    {
        IntPtr job = OpenJobObject(AllAccess, false, name);
        if (job == IntPtr.Zero) { return false; }
        CloseHandle(job);
        return true;
    }

    // Creates the job, arms it, puts THIS process in it and KEEPS the handle.
    // The opposite of JoinCurrentProcess: here the caller IS the owner, so the
    // handle must stay open for as long as it lives. Its death - however
    // abrupt - closes the handle, and that close is what takes everything it
    // started afterwards.
    //
    // A process already inside another job can still do this: jobs nest, and
    // the new job becomes a child of the one it is already in. Measured
    // 2026-10-13 - the outer job was unaffected and the inner one swept both
    // children in under a second.
    public static IntPtr CreateAndJoin(string name)
    {
        IntPtr job = CreateJobObject(IntPtr.Zero, name);
        int err = Marshal.GetLastWin32Error();
        if (job == IntPtr.Zero) {
            throw new InvalidOperationException("CreateJobObject failed, error " + err);
        }
        if (err == AlreadyExists) {
            CloseHandle(job);
            throw new InvalidOperationException("a job object named '" + name + "' already exists");
        }
        try { SetKillOnClose(job, true); }
        catch { CloseHandle(job); throw; }
        if (!AssignProcessToJobObject(job, GetCurrentProcess())) {
            int assignError = Marshal.GetLastWin32Error();
            CloseHandle(job);
            throw new InvalidOperationException("AssignProcessToJobObject failed, error " + assignError);
        }
        bool inJob;
        IsProcessInJob(GetCurrentProcess(), job, out inJob);
        if (!inJob) {
            CloseHandle(job);
            throw new InvalidOperationException("assign reported success but membership did not take");
        }
        return job;
    }

    // Called by each pane. Closing our handle is what makes the owner's
    // handle the last one, and the last one is what arms the kill.
    public static string JoinCurrentProcess(string name)
    {
        IntPtr job = OpenJobObject(AllAccess, false, name);
        if (job == IntPtr.Zero) { return "no such job (error " + Marshal.GetLastWin32Error() + ")"; }
        try {
            if (!AssignProcessToJobObject(job, GetCurrentProcess())) {
                return "assign failed (error " + Marshal.GetLastWin32Error() + ")";
            }
            bool inJob;
            IsProcessInJob(GetCurrentProcess(), job, out inJob);
            return inJob ? string.Empty : "assign reported success but membership did not take";
        }
        finally { CloseHandle(job); }
    }

    public static bool Terminate(string name)
    {
        IntPtr job = OpenJobObject(AllAccess, false, name);
        if (job == IntPtr.Zero) { return false; }
        try { return TerminateJobObject(job, 1); }
        finally { CloseHandle(job); }
    }
}
'@
}


function Get-CamDashboardJobName {
    <#
        The job every dashboard pane joins.

        A fixed name, not one per launch, so the control surface can find the
        panes with no state file to go stale. The launcher stops the previous
        dashboard before starting a new one, so there is only ever one.
    #>
    return 'cam.dashboard.panes'
}


function Get-CamDashboardWindowTitle {
    <#
        The title the dashboard's Windows Terminal window carries.

        Set with wt.exe's --title AND --suppressApplicationTitle on every
        pane. Measured on 2026-10-12: --title alone is overwritten by the
        shell's own title, and the window title then follows whichever pane
        has focus, so it cannot be used to find the window. With the title
        suppressed it stays put across focus changes.
    #>
    return 'cam.dashboard'
}


function Get-CamNvrJobName {
    <#
        The job the NVR window owns: the supervisor pane, and so MediaMTX and
        every ffmpeg the supervisor starts afterwards.

        A fixed name, not one per launch, so the control surface can find the
        NVR with no state file to go stale. CreateExclusive refuses a second
        owner, which is what stops two NVR windows sharing one job.
    #>
    return 'cam.nvr'
}


function Get-CamNvrWindowTitle {
    <#
        The title the NVR window carries, set with wt.exe's --title AND
        --suppressApplicationTitle. Without the second one the shell
        overwrites it and the title follows whatever the pane is doing, so it
        cannot be used to find the window.
    #>
    return 'cam.nvr'
}


function Get-CamNvrWtWindowName {
    <#
        The wt.exe -w window name for the NVR.

        Load-bearing: windowingBehavior is useExisting on this machine, so
        without -w the NVR pane arrives as a TAB in whatever terminal window
        was last used - the Operator's own - and closing that window would
        stop the whole system. With -w the NVR is a window of its own.
    #>
    return 'ipcctv-nvr'
}


function Get-CamSupervisorJobName {
    <#
        The job the SUPERVISOR owns and holds the only handle to, so that its
        own death - a crash, a kill, anything that skips the finally block -
        takes MediaMTX and every ffmpeg with it.

        Distinct from the cam.nvr job, which is owned by the warden and fires
        on the WINDOW closing. The supervisor is a member of both: cam.nvr
        covers "the Operator closed the NVR", this one covers "the supervisor
        died on its own". Before it existed, a supervisor that crashed left
        four ffmpeg writing with no pruner, no watchdog and no status.

        Named per archive root, because "one supervisor per archive" is the
        invariant the whole design rests on. A test sandbox recording
        somewhere else therefore gets a job of its own and does not collide
        with the live NVR.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $root = [string]$Config.RootPath
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { $hash = $sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($root.ToLowerInvariant())) }
    finally { $sha.Dispose() }
    $short = ([System.BitConverter]::ToString($hash, 0, 4)).Replace('-', '').ToLowerInvariant()
    return "cam.supervisor.$short"
}


function Enter-CamSupervisorJob {
    <#
        Puts the calling supervisor into a job object of its own and keeps the
        handle for the life of the process. Call it BEFORE starting MediaMTX or
        any ffmpeg - membership is inherited at creation, so anything started
        before the join stays outside and would survive.

        Never throws. Crash safety is worth a great deal and recording is worth
        more, so a supervisor that cannot create its job says so loudly and
        records anyway.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    Import-CamJobApi
    $name = Get-CamSupervisorJobName -Config $Config
    $result = @{ JobName = $name; Joined = $false; Problem = '' }
    try {
        # Held in a script-scoped variable so the intent is visible. The handle
        # is a value type with no finaliser, so nothing but process exit can
        # close it - which is exactly the lifetime wanted.
        $script:CamSupervisorJobHandle = [CamJob]::CreateAndJoin($name)
        $result.Joined = $true
    }
    catch { $result.Problem = $_.Exception.Message }
    return $result
}


function Add-CamJobMember {
    <#
        Puts THIS process, and so every process it starts afterwards, into the
        job named. Called once by each member, before it starts anything
        that must die with it - a pane before its mpv, the NVR supervisor
        before MediaMTX and ffmpeg.

        Writes one line rather than returning a value, so a pane can call it
        as a bare statement - every character of a pane's script costs 2.67
        characters of a shared 32767-character command line, six times over.

        A pane that cannot join says so and carries on. The job is what makes
        closing the window stop the panes; the control surface finds them by
        process identity either way, so a failed join costs the window-close
        path and nothing else.
    #>
    param(
        [string]$JobName = (Get-CamDashboardJobName),

        # The warden creates the job before the window opens, but a pane that
        # wins that race should wait rather than give up.
        [int]$TimeoutSeconds = 15
    )

    Import-CamJobApi
    $deadline = (Get-CamWallClock).AddSeconds($TimeoutSeconds)
    $problem = 'not attempted'
    while ((Get-CamWallClock) -lt $deadline) {
        $problem = [CamJob]::JoinCurrentProcess($JobName)
        if ([string]::IsNullOrEmpty($problem)) {
            Write-Host ("{0} joined the job '{1}'" -f (Get-Date -Format 'HH:mm:ss'), $JobName) -ForegroundColor DarkGray
            return
        }
        Start-Sleep -Milliseconds 500
    }
    Write-Host ("{0} NOT in the job '{2}': {1}. Closing the window will not stop this process." -f `
        (Get-Date -Format 'HH:mm:ss'), $problem, $JobName) -ForegroundColor Yellow
}


function Get-CamDecodedCommand {
    <#
        Every base64 blob on a command line, decoded from UTF-16 and joined.

        A pane's whole script travels as -EncodedCommand, so the command line
        says "powershell.exe -NoExit -EncodedCommand JABFAHIA..." and nothing
        about cameras. The script inside it is the only honest description of
        what the process is doing, which makes this the identity check.

        Any long base64-looking token is tried rather than parsing the switch
        name, because -EncodedCommand can be abbreviated to -e and a parser
        that misses the abbreviation silently misclassifies the process.
    #>
    param([AllowNull()][string]$CommandLine)

    if ([string]::IsNullOrEmpty($CommandLine)) { return '' }

    $parts = New-Object System.Collections.ArrayList
    foreach ($match in [regex]::Matches($CommandLine, '[A-Za-z0-9+/]{40,}={0,2}')) {
        try {
            $bytes = [Convert]::FromBase64String($match.Value)
            if (($bytes.Length % 2) -ne 0) { continue }
            [void]$parts.Add([System.Text.Encoding]::Unicode.GetString($bytes))
        }
        catch { }
    }
    return ($parts -join "`n")
}


function Get-CamProcessInventory {
    <#
        Every process this project owns right now, each labelled with the role
        it is actually performing.

        Roles: Supervisor, Recorder (an ffmpeg), Relay (MediaMTX), Warden,
        Pane, Viewer (an mpv), Health.

        HOW EACH ROLE IS IDENTIFIED, and why not by name. Two incidents on
        2026-10-12 came from identifying a process by something that was not
        about the work it was doing:

        - Killing by window title destroyed the Operator's own session,
          because Windows Terminal serves every window from one process. A
          title identifies a WINDOW, a pid identifies a PROCESS.
        - Killing by image name would have stopped the 24/7 recording
          supervisor, which is a powershell.exe indistinguishable from the
          six panes and was started by a launcher stub, so its own command
          line does not even name the script it supervises.

        So the supervisor is found as THE PARENT OF EVERY ffmpeg - the work it
        owns is the only thing that identifies it - and the panes and the
        health strip are found by decoding the script they were handed.

        Orphaned mpv and panes from a previous crash are included: they are
        the processes that hurt, because each one holds one of a camera's two
        RTSP sessions until somebody finds it.
    #>
    param([AllowNull()]$Config)

    $all = @(Get-CimInstance -ClassName Win32_Process -ErrorAction Stop |
        Select-Object ProcessId, ParentProcessId, Name, CreationDate, CommandLine)

    $byId = @{}
    foreach ($process in $all) { $byId[[int]$process.ProcessId] = $process }

    $childrenOf = @{}
    foreach ($process in $all) {
        $parent = [int]$process.ParentProcessId
        if (-not $childrenOf.ContainsKey($parent)) { $childrenOf[$parent] = New-Object System.Collections.ArrayList }
        [void]$childrenOf[$parent].Add([int]$process.ProcessId)
    }

    $ffmpeg = @($all | Where-Object { $_.Name -eq 'ffmpeg.exe' })
    $mediamtx = @($all | Where-Object { $_.Name -eq 'mediamtx.exe' })

    # The supervisor owns every ffmpeg, and owns MediaMTX when the relay is
    # enabled. Nothing else about it is distinctive.
    $supervisorIds = New-Object System.Collections.ArrayList
    foreach ($candidate in @($ffmpeg) + @($mediamtx)) {
        $parent = [int]$candidate.ParentProcessId
        if ($parent -le 0) { continue }
        if (-not $byId.ContainsKey($parent)) { continue }
        if ([string]$byId[$parent].Name -notlike 'powershell*') { continue }
        if (-not $supervisorIds.Contains($parent)) { [void]$supervisorIds.Add($parent) }
    }

    $items = New-Object System.Collections.ArrayList
    function Add-InventoryItem([string]$role, $process, [string]$detail) {
        [void]$items.Add([pscustomobject]@{
            Role            = $role
            ProcessId       = [int]$process.ProcessId
            ParentProcessId = [int]$process.ParentProcessId
            Name            = [string]$process.Name
            StartedAt       = $process.CreationDate
            Detail          = $detail
        })
    }

    $paneIds = New-Object System.Collections.ArrayList
    foreach ($process in $all) {
        if ([string]$process.Name -notlike 'powershell*') { continue }
        $id = [int]$process.ProcessId

        if ($supervisorIds.Contains($id)) {
            Add-InventoryItem 'Supervisor' $process 'parent of every ffmpeg'
            continue
        }

        $line = [string]$process.CommandLine
        $decoded = Get-CamDecodedCommand -CommandLine $line
        $text = $line + "`n" + $decoded

        if ($text -match 'Start-CamNvrHost') {
            # The NVR pane. Not the supervisor - that is its child - but
            # killing it would stop the restarts, so it is on the keep list
            # with the rest of the recorder.
            Add-InventoryItem 'NvrHost' $process 'the NVR pane, restarts the supervisor if it dies'
            continue
        }
        if ($text -match 'Start-CamNvrWarden') {
            # A role of its own, deliberately. Stop-CamDashboard stops every
            # 'Warden' it finds, and the NVR warden holds the only handle to
            # the recorder's job - stopping it would take the supervisor,
            # MediaMTX and every ffmpeg with it.
            Add-InventoryItem 'NvrWarden' $process ('owns the job ' + (Get-CamNvrJobName))
            continue
        }
        if ($text -match 'Start-CamDashboardWarden') {
            Add-InventoryItem 'Warden' $process ('owns the job ' + (Get-CamDashboardJobName))
            continue
        }
        if ($decoded -match 'Invoke-CamViewer') {
            $camera = ''
            if ($decoded -match "\`$camName\s*=\s*'([^']*)'") { $camera = $Matches[1] }
            [void]$paneIds.Add($id)
            Add-InventoryItem 'Pane' $process $camera
            continue
        }
        if ($decoded -match 'cam\.health') {
            Add-InventoryItem 'Health' $process 'the health strip'
            continue
        }
    }

    # An mpv belongs to the pane that started it. mpv.com is a console wrapper
    # that re-execs mpv.exe, so the window-owning process is a grandchild of
    # the pane and both have to be accounted for.
    $claimed = New-Object System.Collections.ArrayList
    foreach ($paneId in $paneIds) {
        foreach ($descendant in (Get-CamDescendantId -ChildrenOf $childrenOf -ProcessId $paneId)) {
            if ($byId.ContainsKey($descendant) -and ([string]$byId[$descendant].Name -like 'mpv*')) {
                [void]$claimed.Add($descendant)
                Add-InventoryItem 'Viewer' $byId[$descendant] ('pane ' + $paneId)
            }
        }
    }
    foreach ($process in $all) {
        if ([string]$process.Name -notlike 'mpv*') { continue }
        $id = [int]$process.ProcessId
        if ($claimed.Contains($id)) { continue }
        Add-InventoryItem 'Viewer' $process 'no pane owns it'
    }

    foreach ($process in $ffmpeg) { Add-InventoryItem 'Recorder' $process ('supervisor ' + $process.ParentProcessId) }
    foreach ($process in $mediamtx) { Add-InventoryItem 'Relay' $process 'MediaMTX' }

    return @($items)
}


function Get-CamDescendantId {
    <# Every pid below this one, breadth first. #>
    param(
        [Parameter(Mandatory = $true)][hashtable]$ChildrenOf,
        [Parameter(Mandatory = $true)][int]$ProcessId
    )

    $found = New-Object System.Collections.ArrayList
    $queue = New-Object System.Collections.Queue
    $queue.Enqueue($ProcessId)
    while ($queue.Count -gt 0) {
        $current = [int]$queue.Dequeue()
        if (-not $ChildrenOf.ContainsKey($current)) { continue }
        foreach ($child in $ChildrenOf[$current]) {
            if ($found.Contains([int]$child)) { continue }
            [void]$found.Add([int]$child)
            $queue.Enqueue([int]$child)
        }
    }
    return @($found)
}


function Get-CamAncestorId {
    <#
        Every pid above this one.

        Used to build the guard list: the control surface must never stop a
        process it is itself running inside, which on this project means the
        shell that invoked it and whatever launched that.
    #>
    param([Parameter(Mandatory = $true)][int]$ProcessId)

    $found = New-Object System.Collections.ArrayList
    $current = $ProcessId
    for ($hop = 0; $hop -lt 32; $hop++) {
        $process = Get-CimInstance -ClassName Win32_Process -Filter ("ProcessId=" + $current) -ErrorAction SilentlyContinue
        if (-not $process) { break }
        $parent = [int]$process.ParentProcessId
        if ($parent -le 0 -or $found.Contains($parent)) { break }
        [void]$found.Add($parent)
        $current = $parent
    }
    return @($found)
}


function Stop-CamProcessList {
    <#
        Stops exactly these pids, and reports what it refused to touch.

        The keep list is the point of this function. It is built from the
        recording supervisor and everything below it, plus this process and
        every ancestor of it, and it is honoured even when a caller names one
        of those pids explicitly. A guard that can never fire costs nothing;
        this one has had to fire twice already.
    #>
    param(
        [AllowEmptyCollection()][int[]]$ProcessId = @(),

        # Extra pids that must survive no matter what the caller asked for.
        [AllowEmptyCollection()][int[]]$Keep = @(),

        [int]$TimeoutSeconds = 10
    )

    $result = [pscustomobject]@{
        Requested = @($ProcessId | Sort-Object -Unique)
        Stopped   = @()
        Refused   = @()
        Survived  = @()
    }
    if ($result.Requested.Count -eq 0) { return $result }

    $guard = New-Object System.Collections.ArrayList
    [void]$guard.Add([int]$PID)
    foreach ($ancestor in (Get-CamAncestorId -ProcessId $PID)) { [void]$guard.Add([int]$ancestor) }
    foreach ($keeper in $Keep) { if (-not $guard.Contains([int]$keeper)) { [void]$guard.Add([int]$keeper) } }

    $targets = New-Object System.Collections.ArrayList
    $refused = New-Object System.Collections.ArrayList
    foreach ($id in $result.Requested) {
        if ($guard.Contains([int]$id)) { [void]$refused.Add([int]$id) } else { [void]$targets.Add([int]$id) }
    }
    $result.Refused = @($refused)

    foreach ($id in $targets) { Stop-Process -Id $id -Force -ErrorAction SilentlyContinue }

    $deadline = (Get-CamWallClock).AddSeconds($TimeoutSeconds)
    $alive = @($targets)
    while ($alive.Count -gt 0 -and (Get-CamWallClock) -lt $deadline) {
        Start-Sleep -Milliseconds 300
        $alive = @($alive | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
    }

    $result.Survived = @($alive)
    $result.Stopped = @($targets | Where-Object { $alive -notcontains $_ })
    return $result
}


function Get-CamRecorderKeepList {
    <#
        The recording supervisor, every process below it, and the warden
        that holds the NVR job open.

        Recording runs 24/7 from a Scheduled Task and is stopped only through
        the recorder's own -Stop, which tears its children down in order. No
        dashboard operation may touch any of it, so every stop in this project
        passes this list as its guard.
    #>
    param([AllowNull()]$Inventory)

    if (-not $Inventory) { $Inventory = Get-CamProcessInventory }
    $keep = New-Object System.Collections.ArrayList
    foreach ($item in $Inventory) {
        if ($item.Role -in @('Supervisor', 'Recorder', 'Relay', 'NvrWarden', 'NvrHost')) {
            if (-not $keep.Contains([int]$item.ProcessId)) { [void]$keep.Add([int]$item.ProcessId) }
        }
    }
    return @($keep)
}


function Stop-CamDashboard {
    <#
        Stops the whole viewer side: the window, the warden, the job, the six
        panes, every mpv they own, the health strip, and any orphan of the
        same shape left by an earlier crash. Leaves the recorder alone.

        The order matters. The job is terminated first because that is one
        kernel call that takes every pane and every mpv at once, including the
        mpv a pane was about to restart. Enumeration then catches whatever was
        not in the job - panes from a launch before this mechanism existed, or
        a pane that lost the race to join - and the window is closed last, when
        there is nothing left in it to prompt about.

        Safe on a machine where nothing is running, and safe to run twice: a
        job that does not exist, a window that is not there and an empty pid
        list are all ordinary answers here, not errors.
    #>
    param(
        [AllowNull()]$Config,
        [switch]$Quiet
    )

    Import-CamJobApi
    $log = {
        param([string]$message, [string]$colour = 'Gray')
        if (-not $Quiet) { Write-Host $message -ForegroundColor $colour }
    }

    $jobName = Get-CamDashboardJobName
    $report = [pscustomobject]@{
        JobTerminated = $false
        Stopped       = @()
        Refused       = @()
        Survived      = @()
        WindowsLeft   = 0
    }

    if ([CamJob]::Exists($jobName)) {
        $report.JobTerminated = [CamJob]::Terminate($jobName)
        & $log ("terminated the dashboard job '{0}': {1}" -f $jobName, $report.JobTerminated) 'DarkGray'
        Start-Sleep -Milliseconds 700
    }

    # Whatever is left after the kernel's own sweep, by role.
    $inventory = Get-CamProcessInventory -Config $Config
    $keep = Get-CamRecorderKeepList -Inventory $inventory
    $targets = @($inventory |
        Where-Object { $_.Role -in @('Warden', 'Pane', 'Viewer', 'Health') } |
        ForEach-Object { [int]$_.ProcessId })

    if ($targets.Count -gt 0) {
        & $log ("stopping {0} viewer-side process(es): {1}" -f $targets.Count, (($targets | Sort-Object) -join ', '))
        $stop = Stop-CamProcessList -ProcessId $targets -Keep $keep
        $report.Stopped = $stop.Stopped
        $report.Refused = $stop.Refused
        $report.Survived = $stop.Survived
        if ($stop.Refused.Count -gt 0) {
            & $log ("refused by the guard (recorder or this shell): {0}" -f (($stop.Refused | Sort-Object) -join ', ')) 'Yellow'
        }
        if ($stop.Survived.Count -gt 0) {
            & $log ("still alive after {0}s: {1}" -f 10, (($stop.Survived | Sort-Object) -join ', ')) 'Red'
        }
    }
    else {
        & $log 'no viewer-side processes were running' 'DarkGray'
    }

    # The windows go last, with nothing alive left inside them, and a plain
    # WM_CLOSE is all it takes: measured 2026-10-12, under 800 ms and no
    # confirmation overlay. No keystroke is synthesised and the foreground
    # window is not touched, so a stop cannot interrupt what the Operator is
    # doing - which it used to, for about a second, every time.
    $left = Stop-CamTerminalWindow -Title (Get-CamDashboardWindowTitle)
    $left += Stop-CamTerminalWindow -Title 'cam.health'
    $report.WindowsLeft = $left
    if ($left -gt 0) { & $log ("{0} terminal window(s) would not close" -f $left) 'Yellow' }

    return $report
}


function Start-CamJobWarden {
    <#
        The hard dependency between a terminal window and the processes that
        belong to it. One of these runs per window that owns a job.

        A process inside a Windows Terminal window cannot reliably see its own
        window close: measured on 2026-10-12, a pane's console survives the
        window's death intact - GetConsoleScreenBufferInfo still succeeds, the
        pty host is still running - and a pane blocked inside mpv or inside a
        supervisor loop would not notice anyway.

        So the dependency is held outside the window, by the kernel. This
        process creates the job its members join and holds the only handle to
        it. When the window goes, this process goes, the last handle closes,
        and JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE terminates every member and
        every process a member started - no polling inside the members,
        nothing to miss while one of them is blocking for hours.

        It runs detached with a console of its own and no window (see
        Start-CamDetachedProcess) and costs one IsWindow call a second.

        If the window never appears, the job is DISARMED before this process
        exits, so a launch that failed somewhere else cannot have its members
        killed by a warden that simply never found what it was watching.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$JobName,
        [Parameter(Mandatory = $true)][string]$WindowTitle,

        # The window has to open and every member has to start and reach its
        # first real work. Generous, because the cost of being wrong is
        # killing a working system.
        [int]$AppearTimeoutSeconds = 180,

        [int]$PollMs = 1000,

        [AllowEmptyString()][string]$LogPath = '',

        # Other windows to close once the watched one has gone. A window that
        # is not part of this job but belongs to the same thing on screen -
        # the health strip is one - would otherwise be left showing panes
        # whose processes were killed under it.
        [AllowEmptyCollection()][string[]]$AlsoCloseWindowTitle = @(),

        # The viewers are a job of their own with a warden of their own, so
        # they are not swept by this job closing. The NVR window owns the
        # whole system, so when it goes they go with it. Stopped through the
        # one control surface when its path is given and resolves, which is
        # the audited path; by terminating their job directly when it does
        # not, because the repo can live on a share that is not reachable.
        [switch]$StopViewers,
        [AllowEmptyString()][string]$ControlSurface = '',

        # Raise an alert when the watched window goes. Opt-in, because only the
        # NVR window closing means nothing is being recorded - the viewer grid
        # closing costs the Operator nothing he cannot see for himself, and
        # alerting on it would be the noise that teaches him to ignore the
        # alerts that matter. Item [67].
        [switch]$AlertOnWindowClose
    )

    Import-CamJobApi
    Import-CamWindowApi

    $say = {
        param([string]$message)
        $line = "{0} warden[{1}] {2}" -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $PID, $message
        Write-Host $line
        if (-not [string]::IsNullOrEmpty($LogPath)) {
            try { $line | Out-File -FilePath $LogPath -Encoding ascii -Append } catch { }
        }
    }

    $job = [CamJob]::CreateExclusive($JobName)
    & $say ("created and armed the job '{0}', watching for the window '{1}'" -f $JobName, $WindowTitle)

    try {
        $hwnd = [IntPtr]::Zero
        # Set only once a record has actually been published, because the
        # finally block removes the record ONLY when this warden wrote it: the
        # record is keyed by window title, and a warden that disarmed because
        # its window never appeared must not delete the record of a warden
        # that is watching one.
        $recordConfig = $null
        $deadline = (Get-CamWallClock).AddSeconds($AppearTimeoutSeconds)
        while ((Get-CamWallClock) -lt $deadline) {
            $found = @([CamWin]::FindTerminalByTitle($WindowTitle))
            if ($found.Count -ge 1) { $hwnd = $found[0]; break }
            Start-Sleep -Milliseconds $PollMs
        }

        if ($hwnd -eq [IntPtr]::Zero) {
            & $say ("no terminal window titled '{0}' appeared within {1}s - disarming the job and leaving its members alone" -f `
                $WindowTitle, $AppearTimeoutSeconds)
            [CamJob]::Disarm($job)
            return
        }

        & $say ("watching window 0x{0:X} (terminal pid {1})" -f $hwnd.ToInt64(), [CamWin]::ProcessIdOf($hwnd))

        # PUBLISH THE HANDLE. This warden is the only thing that knows which
        # window the job belongs to, and a title search cannot recover it: the
        # window answers to its active tab's title, so an adopted foreign tab
        # hides it completely while this loop goes on watching it perfectly
        # well. Writing it down is what lets -Status name the window instead
        # of counting titles. Item [81].
        #
        # It is written AFTER the handle is latched and BEFORE the watch loop,
        # so the record never claims a window this process is not yet watching,
        # and it cannot throw - Set-CamWindowRecord swallows its own failures
        # and the try here covers resolving the config - because everything
        # below this line is the warden's actual duty.
        try {
            $recordConfig = Get-CamConfig
            if (Set-CamWindowRecord -Config $recordConfig -Title $WindowTitle -Hwnd $hwnd -JobName $JobName) {
                & $say ("published the window record to {0}" -f (Get-CamWindowRecordPath -Config $recordConfig -Title $WindowTitle))
            }
            else {
                $recordConfig = $null
                & $say 'could not publish the window record - -Status will fall back to searching for the title'
            }
        }
        catch {
            $recordConfig = $null
            & $say ('could not publish the window record: ' + $_.Exception.Message)
        }

        while ([CamWin]::IsWindow($hwnd)) { Start-Sleep -Milliseconds $PollMs }
        & $say ("window 0x{0:X} is gone - stopping everything it owned" -f $hwnd.ToInt64())

        # The job handle closes FIRST, before anything slower. Between the
        # window going and this line the job's members are orphaned - their
        # parent took CTRL_CLOSE_EVENT with the window and is already gone -
        # and anything that runs before the kernel sweep extends that window.
        # Measured on 2026-10-12: stopping the viewers first left four ffmpeg
        # and MediaMTX parentless for eight seconds; this way it is under one.
        [void][CamJob]::CloseHandle($job)
        $job = [IntPtr]::Zero
        & $say 'closed the last handle to the job - the kernel has taken its members'

        # The alert comes next, before the viewers are stopped: that call goes
        # through cam.ps1 and takes the better part of twenty seconds, and the
        # Operator finding out is worth more than the grid closing a second
        # sooner. It cannot come BEFORE the handle close - anything that runs
        # there leaves the job's members orphaned for as long as it takes.
        #
        # Nothing here is allowed to throw. The warden's remaining duty is
        # stopping the viewers, and an alert that failed must not cost them.
        if ($AlertOnWindowClose) {
            try {
                $reason = "the NVR window '{0}' was closed at {1}, so the supervisor, MediaMTX, every ffmpeg and the viewers were stopped" -f `
                    $WindowTitle, (Get-Date -Format 'HH:mm:ss')

                # A window close is not a crash, and the wording has to say so
                # or the Operator cannot tell at a glance whether something is
                # broken. Nothing is: the system did exactly what closing that
                # window means. What he needs to know is that it is down and
                # that nothing will bring it back - deliberately, so one alert
                # with the command in it rather than a restart he did not ask
                # for.
                $alert = Send-CamAlert -Title 'the NVR window was closed - nothing is recording' `
                    -Message ("{0}. Nothing will restart it: run cam.ps1 -Start to bring it back." -f $reason) `
                    -EventId 103 -EntryType 'Warning'
                & $say ('alerted: the window was closed and nothing is recording [' + $alert.Channels + ']')

                $config = Get-CamConfig
                if (Set-CamStatusWindowClosed -Config $config -Reason $reason -WindowTitle $WindowTitle) {
                    & $say ("recorded the close in {0} as StopKind 'window-closed'" -f (Get-CamStatusFilePath -Config $config))
                }
                else {
                    & $say 'could not record the close in status.json'
                }
            }
            catch { & $say ('alerting on the window close failed: ' + $_.Exception.Message) }
        }

        if ($StopViewers) {
            $stopped = $false
            if (-not [string]::IsNullOrEmpty($ControlSurface)) {
                try {
                    if (Test-Path -LiteralPath $ControlSurface) {
                        & $ControlSurface -Stop -Dashboard | Out-Null
                        $stopped = $true
                        & $say 'stopped the viewers through cam.ps1 -Stop -Dashboard'
                    }
                }
                catch { & $say ('cam.ps1 -Stop -Dashboard failed: ' + $_.Exception.Message) }
            }
            if (-not $stopped) {
                [void][CamJob]::Terminate((Get-CamDashboardJobName))
                [void](Stop-CamTerminalWindow -Title (Get-CamDashboardWindowTitle))
                & $say 'stopped the viewers by terminating their job directly'
            }
        }

        foreach ($title in $AlsoCloseWindowTitle) {
            if ([string]::IsNullOrEmpty($title)) { continue }
            [void](Stop-CamTerminalWindow -Title $title)
        }
    }
    finally {
        # Only reached when the job was not closed above: the window never
        # appeared and it was disarmed, or something threw on the way. Closing
        # it is what kills the members, so it is never left to process exit.
        if ($job -ne [IntPtr]::Zero) { [void][CamJob]::CloseHandle($job) }

        # The record is this warden's claim to be watching that window, so it
        # goes when the warden does. It is removed HERE, last of everything,
        # and never anywhere near the handle close above: nothing is allowed
        # between the window going and the kernel sweep, and the record being
        # a few seconds stale costs nothing because every reader re-checks the
        # handle itself.
        if ($recordConfig) { [void](Remove-CamWindowRecord -Config $recordConfig -Title $WindowTitle) }
    }
}


function Start-CamDashboardWarden {
    <#
        The warden for the viewer grid: closing the grid window stops all six
        panes and every mpv they own. Item [40].

        The health strip is a window of its own, so the grid closing does not
        take it; it is closed here, before the job goes, or it would be left
        showing panes whose processes were killed under it.
    #>
    param(
        [string]$JobName = (Get-CamDashboardJobName),
        [string]$WindowTitle = (Get-CamDashboardWindowTitle),
        [int]$AppearTimeoutSeconds = 180,
        [int]$PollMs = 1000,
        [AllowEmptyString()][string]$LogPath = ''
    )

    Start-CamJobWarden -JobName $JobName -WindowTitle $WindowTitle `
        -AppearTimeoutSeconds $AppearTimeoutSeconds -PollMs $PollMs -LogPath $LogPath `
        -AlsoCloseWindowTitle @('cam.health')
}


function Start-CamNvrWarden {
    <#
        The warden for the NVR window: closing it stops the supervisor, the
        relay, every ffmpeg and the viewers.

        The supervisor runs as the NVR window's only pane and joins this job
        before it starts anything, so MediaMTX and every ffmpeg inherit
        membership at creation and the kernel takes the lot in one sweep. That
        is what makes "no orphans" structural rather than a matter of the
        supervisor getting a chance to clean up - a console close does not let
        it run its finally block at all.

        It also ALERTS when the window goes, because the system cannot tell an
        accidental click from a deliberate close and nothing brings the NVR
        back. The close stays absolute - no restart - the Operator just finds
        out it happened. Item [67].
    #>
    param(
        [string]$JobName = (Get-CamNvrJobName),
        [string]$WindowTitle = (Get-CamNvrWindowTitle),
        [int]$AppearTimeoutSeconds = 180,
        [int]$PollMs = 1000,
        [AllowEmptyString()][string]$LogPath = '',
        [AllowEmptyString()][string]$ControlSurface = ''
    )

    Start-CamJobWarden -JobName $JobName -WindowTitle $WindowTitle `
        -AppearTimeoutSeconds $AppearTimeoutSeconds -PollMs $PollMs -LogPath $LogPath `
        -StopViewers -ControlSurface $ControlSurface -AlertOnWindowClose
}


function Remove-CamExpiredRestart {
    <# Drops the restarts older than the window from the front of an oldest-first list of Get-CamWallClock readings. #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][System.Collections.ArrayList]$Restarts,
        [Parameter(Mandatory = $true)][double]$WindowMinutes,
        [Parameter(Mandatory = $true)][DateTime]$Now
    )

    $cutoff = $Now.AddMinutes(-$WindowMinutes)
    while ($Restarts.Count -gt 0 -and $Restarts[0] -lt $cutoff) { $Restarts.RemoveAt(0) }
}


function Start-CamNvrHost {
    <#
        The NVR pane. Runs the supervisor as a child, waits for it, and starts
        it again when it stopped without being asked to.

        WHY A HOST PROCESS AT ALL, when the pane could simply BE the
        supervisor, as it was until now. Because from outside, a deliberate
        window close and a crash look identical: both end with the supervisor
        gone, the status file still saying "running" and the window destroyed,
        and the warden watching that window cannot tell which it was. Guessing
        wrong either way is bad - restart on a deliberate close and the
        Operator cannot stop his own NVR; stay down on a crash and the cameras
        record nothing until somebody looks.

        Splitting the pane from the supervisor makes the two cases different
        events rather than one ambiguous one:

          window closed   this process and the supervisor share a console, so
                          both take CTRL_CLOSE_EVENT and both die. The window
                          goes. The cam.nvr warden sweeps the rest, exactly as
                          before.
          supervisor died this process is untouched, the window is still open,
                          and the child exiting IS the signal. No window churn,
                          no job handed over, no race with the warden: the
                          restart happens here, in place, and the window keeps
                          its log.

        Expected or not is decided from what the supervisor recorded, never
        from an exit code alone - a killed process has no say in its exit code:

          stop flag present          somebody asked for this, even if the
                                     orderly path had to be finished by a kill
          StopKind requested         the supervisor saw the flag itself
          StopKind interrupted       Ctrl+C or the tab, so a human did it
          exit 20                    it refused to start, restarting cannot help
          anything else              unexpected: alert and restart

        The restart budget exists because a supervisor that dies on startup
        would otherwise be restarted for ever. Out of budget, it alerts once
        more and stops, leaving the window open with the reason on screen.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$RecorderPath,

        # Enough to ride out a transient fault, few enough that a supervisor
        # which cannot start is not restarted all night.
        [int]$MaxRestarts = 5,
        [int]$RestartWindowMinutes = 30,
        [int]$RestartDelaySeconds = 5
    )

    Import-CamJobApi
    $config = Get-CamConfig
    $paths = Get-CamPaths -Config $config
    $log = Join-Path -Path $paths.LogDir -ChildPath 'recorder.log'
    $maxBytes = [int]$config.LogMaxBytes
    $stopFlag = Get-CamStopFlagPath -Config $config
    $jobName = Get-CamSupervisorJobName -Config $config
    $powershell = Join-Path -Path $env:SystemRoot -ChildPath 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $restarts = New-Object System.Collections.ArrayList

    Write-CamLog -Path $log -Message ("nvr host pid {0} starting the supervisor; it will be restarted if it stops without being asked to" -f $PID) -MaxBytes $maxBytes

    while ($true) {
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.FileName = $powershell
        # -Command rather than -File: -File binds an array parameter as one
        # string, and this console is inherited either way, which is what puts
        # the supervisor's log on the NVR window.
        $psi.Arguments = ("-NoProfile -ExecutionPolicy Bypass -Command ""& '{0}' -Run""" -f $RecorderPath.Replace("'", "''"))
        $psi.UseShellExecute = $false
        $psi.CreateNoWindow = $false
        $child = [System.Diagnostics.Process]::Start($psi)
        [void]$child.Handle
        $startedAt = Get-CamWallClock

        $child.WaitForExit()
        $code = $child.ExitCode
        $ranFor = ((Get-CamWallClock) - $startedAt).TotalSeconds

        $requested = (Test-Path -LiteralPath $stopFlag)
        $stopKind = ''
        $snapshot = Get-CamStatusSnapshot -Config $config
        if ($snapshot) { $stopKind = [string]$snapshot.StopKind }

        if ($requested -or $stopKind -eq 'requested' -or $stopKind -eq 'interrupted') {
            Write-CamLog -Path $log -Message ("the supervisor (pid {0}) stopped as asked after {1}s, exit {2} ({3}). The NVR host is done." -f `
                    $child.Id, [int]$ranFor, $code, $(if ($stopKind) { $stopKind } else { 'stop flag present' })) -MaxBytes $maxBytes
            return
        }

        if ($code -eq 20) {
            Write-CamLog -Path $log -Message ("the supervisor refused to start: another one is already running. Restarting would not help, so the NVR host is done." -f $code) -MaxBytes $maxBytes
            return
        }

        # Everything left is a stop nobody asked for.
        $reason = "the supervisor (pid {0}) stopped after {1}s without being asked to (exit {2})" -f $child.Id, [int]$ranFor, $code
        if ($stopKind -eq 'unexpected' -and $snapshot -and $snapshot.LastUnexpectedStop) {
            $reason = "{0}: {1}" -f $reason, [string]$snapshot.LastUnexpectedStop.Reason
        }
        else {
            # It was killed outright, so it never recorded anything. The status
            # file still claims it is running; say otherwise before alerting,
            # so the UI and the alert agree.
            [void](Set-CamStatusUnexpectedStop -Config $config -Reason $reason -SupervisorPid $child.Id)
        }

        Remove-CamExpiredRestart -Restarts $restarts -WindowMinutes $RestartWindowMinutes -Now (Get-CamWallClock)

        if ($restarts.Count -ge $MaxRestarts) {
            [void](Send-CamAlert -Title 'the NVR has stopped and is NOT coming back' `
                    -Message ("{0}. It has already been restarted {1} times in {2} minutes, so it is not being restarted again. Nothing is recording." -f `
                        $reason, $restarts.Count, $RestartWindowMinutes) `
                    -EventId 102 -EntryType 'Error' -LogPath $log -LogMaxBytes $maxBytes)
            return
        }

        [void](Send-CamAlert -Title 'the NVR stopped unexpectedly' `
                -Message ("{0}. MediaMTX and every ffmpeg stopped with it. Restarting it in {1}s." -f $reason, $RestartDelaySeconds) `
                -EventId 100 -EntryType 'Error' -LogPath $log -LogMaxBytes $maxBytes)

        # The next supervisor creates a job of the same name, so wait for the
        # kernel to finish sweeping the last one rather than letting it start
        # without crash safety.
        $deadline = (Get-CamWallClock).AddSeconds(20)
        while ([CamJob]::Exists($jobName) -and (Get-CamWallClock) -lt $deadline) { Start-Sleep -Milliseconds 250 }

        Start-Sleep -Seconds $RestartDelaySeconds
        [void]$restarts.Add((Get-CamWallClock))
        [void](Send-CamAlert -Title 'the NVR is recording again' `
                -Message ("restarted the supervisor after an unexpected stop; this is restart {0} of {1} allowed in {2} minutes." -f `
                    $restarts.Count, $MaxRestarts, $RestartWindowMinutes) `
                -EventId 101 -EntryType 'Warning' -LogPath $log -LogMaxBytes $maxBytes)
    }
}


function Resolve-CamWindowStart {
    <#
        The start path's decision for one wardened window, taken before
        anything is launched: the window is already open, or what is here is
        a corpse to clear first, or the way is clear.

        Apart from Start-CamNvrWindow so that the decision itself can be driven
        against a sandbox window and job. Everything it needs to know about the
        machine is passed in - who is recording, and which warden and host
        processes belong to this window - because the NVR's own sources for
        those, the process tree, the heartbeat and the inventory, describe the
        live NVR wherever they are read from. Item [84].

        Returns AlreadyOpen with the presence the window was found by, or the
        pids cleared on the way to a launch. Throws when it must not launch.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Title,
        [Parameter(Mandatory = $true)][string]$JobName,

        # Who is recording, named for the refusal; empty when nothing is.
        [AllowEmptyString()][string]$RecordingBy = '',

        # This window's own warden and host processes: what a clear stops.
        [AllowEmptyCollection()][int[]]$WardenPid = @(),
        [AllowEmptyCollection()][int[]]$HostPid = @(),

        [int]$WaitSeconds = 10
    )

    Import-CamJobApi
    Import-CamWindowApi

    $result = [pscustomobject]@{
        AlreadyOpen = $false
        Presence    = $null
        Cleaned     = @()
    }

    # By the warden's published HWND first, by the title only when there is no
    # live handle to go on. The title is not a reliable name for this window -
    # it follows the active tab - and this is the call site where that cost
    # the most: a title search that came back zero while the NVR was running
    # used to produce the refusal below, and a title search that came back
    # zero while it was NOT running used to skip the clearing path and leave a
    # dead window for the next launch to collide with. Item [81].
    $presence = Get-CamWindowPresence -Config $Config -Title $Title
    $result.Presence = $presence
    $windows = $presence.Count

    if ($RecordingBy) {
        if ($presence.Found) {
            $result.AlreadyOpen = $true
            return $result
        }

        # Recording, and the window cannot be identified at all: no warden has
        # published a live handle and nothing carries the title. Clearing the
        # job here would kill a working supervisor, MediaMTX and six ffmpeg.
        # Refuse, and name the pids doing the work.
        #
        # Until item [81] this was also the adopted-foreign-tab case, which is
        # common on a machine whose windowingBehavior is useExisting and is
        # now handled above by the published handle. What is left is a window
        # whose warden never published and whose title has moved, or no window
        # at all with a supervisor somehow still running outside one.
        throw ("The NVR IS running ({0}) but its window could not be identified - no warden has published a live window handle to '{1}' and no window carries the title '{2}', so a second one cannot be started. Nothing has been changed and recording is unaffected. Stop the whole NVR with cam.ps1 -Stop -Recorder first if you really want to restart it." -f `
                $RecordingBy, (Get-CamWindowRecordPath -Config $Config -Title $Title), $Title)
    }

    # Nothing is recording, so any window or job still here is a corpse, not an
    # NVR. This is the state cam.ps1 -Stop -Recorder used to leave behind: the
    # host pane sitting at a -NoExit prompt with its window still open and the
    # warden still holding the job, which made the window look open and the job
    # look taken while nothing recorded. Clearing it is the published recovery,
    # so do it here rather than leave a system its own start command cannot
    # start. Item [63].
    #
    # A warden or host still running counts on its own: neither may be left
    # behind a new launch, whether or not anything still answers to the title.
    $owned = @(@($WardenPid) + @($HostPid))
    if ($windows -ge 1 -or [CamJob]::Exists($JobName) -or $owned.Count -gt 0) {
        $stale = Stop-CamWardenedWindow -Config $Config -Title $Title -JobName $JobName -WardenPid $WardenPid -HostPid $HostPid
        $result.Cleaned = $stale.Stopped
        Write-Host ("cleared a dead NVR: {0} window(s) ({1}) and job '{2}' held with nothing recording; stopped pid(s) {3}" -f `
                $windows, `
                $(if ($presence.Found) { "{0}, titled '{1}'" -f $presence.HwndHex, $presence.CurrentTitle } else { "none titled '$Title'" }), `
                $JobName, `
                $(if ($stale.Stopped.Count -gt 0) { ($stale.Stopped | Sort-Object) -join ', ' } else { 'none' })) -ForegroundColor Yellow

        # Wait for what is the NVR's OWN to be gone: its warden and host, its
        # job, and any window that still answers to it - a live warden's
        # published handle, or its own tab still active under its title. That
        # is the protection: no second NVR while the first one's warden, host
        # or job is alive, and no new warden left to latch onto the old tab.
        #
        # NOT the window the record named. The NVR can be a tab in a window
        # shared with the Operator's own tabs, which stays open after the NVR
        # has gone and will never close; the next launch adds a tab to it like
        # any other. Waiting for that handle to close threw at every start
        # after an orderly stop, item [84].
        $alive = @()
        $job = $false
        $deadline = (Get-CamWallClock).AddSeconds($WaitSeconds)
        while ($true) {
            $alive = @($owned | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
            $windows = (Get-CamWindowPresence -Config $Config -Title $Title).Count
            $job = [CamJob]::Exists($JobName)
            if ($alive.Count -eq 0 -and $windows -eq 0 -and -not $job) { break }
            if ((Get-CamWallClock) -ge $deadline) {
                throw ("Nothing is recording, but the NVR is still there after stopping its host and warden: {0} window(s) answering to '{1}', job '{2}' held: {3}, its own process(es) still alive: {4} (stopped: {5}; survived: {6}). Something outside this project holds them. Run cam.ps1 -Status, which names the window by handle and by the title it is actually showing, and the processes by role, then try again." -f `
                        $windows, $Title, $JobName, $job, `
                        $(if ($alive.Count -gt 0) { ($alive | Sort-Object) -join ', ' } else { 'none' }), `
                        (($stale.Stopped | Sort-Object) -join ', '), (($stale.Survived | Sort-Object) -join ', '))
            }
            Start-Sleep -Milliseconds 250
        }
    }

    return $result
}


function Start-CamNvrWindow {
    <#
        Opens the NVR: one Windows Terminal window of its own, titled
        cam.nvr, running the recording supervisor as its only pane with the
        supervisor log on screen.

        That window IS the NVR. Closing it deliberately stops the supervisor,
        MediaMTX, every ffmpeg and the viewers. Closing any other terminal
        does nothing, which is the whole point of launching it this way:
        before this existed the supervisor was a hidden console created by the
        Scheduled Task, and on a machine where Windows Terminal is the default
        terminal application with windowingBehavior useExisting, such a
        console is ADOPTED as a tab in whatever terminal window already
        exists. The Operator's. Closing it sent CTRL_CLOSE_EVENT to the
        supervisor, which PowerShell answers by terminating without running
        any finally block - no shutdown line in the log, and four ffmpeg plus
        MediaMTX left orphaned and still writing. Measured four times on
        2026-10-12.

        Order matters. The warden starts BEFORE wt.exe, because the pane joins
        the job as its first statement and the job has to exist by then: a
        supervisor that joined late would already have started MediaMTX and
        ffmpeg outside the job, and those are what must not be left behind.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,

        # The warden creates the job within a few hundred ms. Waiting is
        # cheap and the alternative is a window whose close does nothing.
        [int]$JobWaitSeconds = 10,

        [int]$WindowWaitSeconds = 30
    )

    Import-CamJobApi
    Import-CamWindowApi

    $jobName = Get-CamNvrJobName
    $title = Get-CamNvrWindowTitle
    $repo = $script:CamLibRoot
    $libPath = Join-Path -Path $repo -ChildPath 'cam.common.ps1'
    $recorderPath = Join-Path -Path $repo -ChildPath 'cam.recorder.ps1'
    $controlPath = Join-Path -Path $repo -ChildPath 'cam.ps1'

    $report = [pscustomobject]@{
        AlreadyOpen = $false
        WardenPid   = 0
        JobReady    = $false
        WindowFound = $false
        Title       = $title
        # The handle the window was found by and the title it is actually
        # showing, which is not always $title - see Get-CamWindowPresence.
        Hwnd         = ''
        CurrentTitle = ''
        JobName      = $jobName
        Cleaned      = @()
    }

    # WHAT IS ALREADY THERE, and what that means. Three facts are read before
    # anything is started: whether a supervisor is actually recording, whether
    # a window carries the NVR's title, and whether the job is held. They do
    # not move together, and item [63] was the cost of treating a window or a
    # job as proof that the NVR was alive.
    #
    # Recording is read from the PROCESS TREE first - the supervisor is the
    # parent of every ffmpeg - and from the heartbeat second. Never from the
    # status file alone: it still says "running" after a supervisor has been
    # killed.
    $inventory = @(Get-CamProcessInventory -Config $Config)
    $live = @($inventory | Where-Object { $_.Role -eq 'Supervisor' })
    $heartbeat = Get-CamSupervisorState -Config $Config -Snapshot (Get-CamStatusSnapshot -Config $Config)
    $recordingBy = ''
    if ($live.Count -gt 0) { $recordingBy = 'supervisor ' + (($live | ForEach-Object { $_.ProcessId }) -join ', ') }
    elseif ($heartbeat.Running) { $recordingBy = "supervisor pid $($heartbeat.Pid) by heartbeat" }

    $start = Resolve-CamWindowStart -Config $Config -Title $title -JobName $jobName -RecordingBy $recordingBy `
        -WardenPid @($inventory | Where-Object { $_.Role -eq 'NvrWarden' } | ForEach-Object { [int]$_.ProcessId }) `
        -HostPid @($inventory | Where-Object { $_.Role -eq 'NvrHost' } | ForEach-Object { [int]$_.ProcessId })
    if ($start.AlreadyOpen) {
        $report.AlreadyOpen = $true
        $report.WindowFound = $true
        $report.Hwnd = $start.Presence.HwndHex
        $report.CurrentTitle = $start.Presence.CurrentTitle
        return $report
    }
    $report.Cleaned = $start.Cleaned
    $wardenLog = Join-Path -Path (Get-CamStateDirPath -Config $Config) -ChildPath 'nvr-warden.log'
    $wardenScript = ". '{0}'`nStart-CamNvrWarden -LogPath '{1}' -ControlSurface '{2}'" -f `
        $libPath.Replace("'", "''"), $wardenLog.Replace("'", "''"), $controlPath.Replace("'", "''")
    $warden = Start-CamDetachedProcess -Script $wardenScript
    $report.WardenPid = $warden.Id

    $deadline = (Get-CamWallClock).AddSeconds($JobWaitSeconds)
    while ((Get-CamWallClock) -lt $deadline) {
        if ([CamJob]::Exists($jobName)) { $report.JobReady = $true; break }
        Start-Sleep -Milliseconds 250
    }

    # The pane joins the job, then hosts the supervisor as a child so that a
    # crash is distinguishable from the Operator closing the window - see
    # Start-CamNvrHost. The supervisor inherits this pane's job membership and
    # its console, so MediaMTX and every ffmpeg are still swept by the window
    # closing and the supervisor log is still on screen.
    $paneScript = ". '{0}'`nAdd-CamJobMember -JobName '{1}'`nStart-CamNvrHost -RecorderPath '{2}'" -f `
        $libPath.Replace("'", "''"), $jobName.Replace("'", "''"), $recorderPath.Replace("'", "''")
    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($paneScript))

    # -w gives the NVR a window of its own; --title plus
    # --suppressApplicationTitle keep the title put, which is how the warden
    # and the control surface find it. -NoExit leaves the final log lines on
    # screen when the supervisor stops, instead of the window vanishing.
    $wtArgs = @('-w', (Get-CamNvrWtWindowName), 'new-tab', '--title', $title, '--suppressApplicationTitle',
        'powershell.exe', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-NoExit', '-EncodedCommand', $encoded)
    & wt.exe $wtArgs

    $deadline = (Get-CamWallClock).AddSeconds($WindowWaitSeconds)
    while ((Get-CamWallClock) -lt $deadline) {
        # The window we just launched carries the title - its tab is the active
        # one and nothing has been adopted into it yet - so the title search
        # answers here. Asking through the presence reader anyway means this
        # wait also succeeds on the warden's record, which is the stronger
        # signal of the two: it proves a warden latched onto the window rather
        # than only that a window appeared.
        $launched = Get-CamWindowPresence -Config $Config -Title $title
        if ($launched.Found) {
            $report.WindowFound = $true
            $report.Hwnd = $launched.HwndHex
            $report.CurrentTitle = $launched.CurrentTitle
            break
        }
        Start-Sleep -Milliseconds 500
    }

    return $report
}


function Stop-CamNvr {
    <#
        Takes the NVR window itself down: the warden that holds the cam.nvr
        job, and the host pane the window runs. Call it only AFTER the
        recorder's own orderly stop has finished.

        Why it has to exist. The supervisor stopping does not end the NVR. The
        host returns once the stop was asked for, and the pane's powershell
        was launched with -NoExit so the window stays open with the last log
        lines on screen - which is wanted when a human is reading it, and
        fatal for a scripted stop: the warden is still holding the cam.nvr
        job, so the next launch finds the job present with a window that the
        control surface may no longer be able to match, refuses to create a
        second one, and nothing records. Measured on 2026-10-13, item [63].

        THE WARDEN GOES FIRST, and by termination:

        - its handle is the LAST handle to cam.nvr, so closing it is what
          fires KILL_ON_JOB_CLOSE and has the kernel take the host pane and
          anything else still inside the job in one sweep;
        - a terminated warden never reaches its "the window is gone" branch,
          which stops the viewers through cam.ps1 -Stop -Dashboard. Stopping
          the recorder must not stop the dashboard that was not asked about.

        Anything still alive after the sweep is stopped directly: a host that
        failed to join the job is not a member of it and the kernel will not
        take it.
    #>
    param([AllowNull()]$Config)

    $inventory = @(Get-CamProcessInventory -Config $Config)
    return (Stop-CamWardenedWindow -Config $Config -Title (Get-CamNvrWindowTitle) -JobName (Get-CamNvrJobName) `
        -WardenPid @($inventory | Where-Object { $_.Role -eq 'NvrWarden' } | ForEach-Object { [int]$_.ProcessId }) `
        -HostPid @($inventory | Where-Object { $_.Role -eq 'NvrHost' } | ForEach-Object { [int]$_.ProcessId }))
}


function Stop-CamWardenedWindow {
    <#
        The body of Stop-CamNvr, for one window whose warden and host pids are
        already known: the warden by termination first, then any host the
        kernel sweep did not take. Apart from Stop-CamNvr so that it can be
        driven against a sandbox window - Stop-CamNvr finds its pids in the
        machine-wide inventory, which names the live NVR. Item [84].

        A terminated warden never reaches the finally that removes its window
        record, so the record of a warden stopped here is removed here.
    #>
    param(
        [AllowNull()]$Config,
        # The title the warden watches, which is what its window record is keyed by.
        [Parameter(Mandatory = $true)][string]$Title,
        [Parameter(Mandatory = $true)][string]$JobName,
        [AllowEmptyCollection()][int[]]$WardenPid = @(),
        [AllowEmptyCollection()][int[]]$HostPid = @()
    )

    Import-CamJobApi
    $wardens = @($WardenPid)
    $hosts = @($HostPid)

    $report = [pscustomobject]@{
        Wardens   = $wardens
        Hosts     = $hosts
        Stopped   = @()
        Survived  = @()
        JobExists = [CamJob]::Exists($jobName)
        JobName   = $jobName
    }
    if ($wardens.Count -eq 0 -and $hosts.Count -eq 0) { return $report }

    $stopped = New-Object System.Collections.ArrayList
    if ($wardens.Count -gt 0) {
        $result = Stop-CamProcessList -ProcessId $wardens
        foreach ($id in $result.Stopped) { [void]$stopped.Add([int]$id) }

        # The kernel sweep is immediate once the handle is gone, but the
        # warden has to die first and a terminated process is reaped
        # asynchronously.
        $deadline = (Get-CamWallClock).AddSeconds(10)
        while ((Get-CamWallClock) -lt $deadline) {
            $alive = @($hosts | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
            if ($alive.Count -eq 0 -and -not [CamJob]::Exists($jobName)) { break }
            Start-Sleep -Milliseconds 250
        }
    }

    $left = @($hosts | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
    if ($left.Count -gt 0) {
        $result = Stop-CamProcessList -ProcessId $left
        foreach ($id in $result.Stopped) { [void]$stopped.Add([int]$id) }
    }

    if ($Config) {
        $record = Get-CamWindowRecord -Config $Config -Title $Title
        if ($record -and @($stopped) -contains [int]$record.WardenPid -and -not (Test-CamWindowRecordLive -Record $record)) {
            [void](Remove-CamWindowRecord -Config $Config -Title $Title -Expected $record)
        }
    }

    $report.Stopped = @($stopped)
    $report.Survived = @(@($wardens) + @($hosts) | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
    $report.JobExists = [CamJob]::Exists($jobName)
    return $report
}


function Stop-CamHealthStrip {
    <#
        Stops every health strip - the PROCESS, then its window - and returns
        how many processes it stopped.

        This is item [39]. Six stale cam.health processes had accumulated by
        the end of 2026-10-12, windowless and invisible, one per relaunch. The
        reason is the whole lesson of this file: the old version closed a
        WINDOW found by its title and never touched a process, so it worked
        only while the title held. It does not hold. cam.health.ps1 sets the
        title, and when the renderer exits, PowerShell -NoExit returns to a
        prompt and the title reverts to the shell default - measured on
        2026-10-12 as a live strip window reading "Windows PowerShell". From
        then on every relaunch closed nothing and left one more process behind.

        So the strip is found the way everything else here is found: by the
        script it was handed. The window close is cosmetic cleanup afterwards.
    #>
    param([AllowNull()]$Config)

    $strips = @(Get-CamProcessInventory -Config $Config | Where-Object { $_.Role -eq 'Health' })
    if ($strips.Count -gt 0) {
        $keep = Get-CamRecorderKeepList
        [void](Stop-CamProcessList -ProcessId @($strips | ForEach-Object { [int]$_.ProcessId }) -Keep $keep)
    }

    # Whatever window is left, by whichever title it now carries.
    [void](Stop-CamTerminalWindow -Title 'cam.health')
    return $strips.Count
}


# ===========================================================================
# Logon tasks and the launcher stubs they point at
#
# Two things run at logon: the recorder, which is a headless 24/7 service,
# and the dashboard, which is a desktop artifact needing an interactive
# session. They are two Scheduled Tasks on purpose - folding them into one
# would couple a service to a session, and cam.ps1 -Start already runs the
# recorder's task, so a single task pointing at cam.ps1 would invoke itself.
#
# Both are registered through the ScheduledTasks module rather than
# schtasks.exe. Measured on this machine: schtasks /create /sc onlogon fails
# with "Access is denied" for a standard user, in the root folder and in a
# subfolder alike, while Register-ScheduledTask with a per-user LogonTrigger
# and an interactive, limited principal succeeds unelevated. Reading a task
# back with schtasks /query needs no elevation either way.
#
# Neither task points at a script in the repo. The repo can live on
# \\wsl.localhost, which resolves only once WSL's file server is up, and that
# is not true at logon. A task cannot wait for a path; a launcher stub on a
# local path can, so each task runs one of those and the stub forwards.
# ===========================================================================

# Restart-on-failure, applied to both tasks: a logon start that failed
# because WSL was not up yet should try again rather than wait for the next
# logon. Accepted unelevated; lands in the exported XML as RestartOnFailure.
$script:CamLogonTaskRestartCount = 3
$script:CamLogonTaskRestartMinutes = 1


function Get-CamAutostartDir {
    <#
        Where the generated launchers and their logs live: a local path that
        is present at logon, which the repo may not be.
    #>
    return (Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IPCCTV')
}


function Get-CamLogonTask {
    <# The registered task of this name, or $null. Never throws. #>
    param([Parameter(Mandatory = $true)][string]$TaskName)

    if (-not (Get-Command -Name 'Get-ScheduledTask' -ErrorAction SilentlyContinue)) { return $null }
    return (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue | Select-Object -First 1)
}


function Get-CamLogonTaskAccount {
    <# DOMAIN\user for the account a task will run as - the current user. #>
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    if ($identity) { return $identity }
    return ("{0}\{1}" -f $env:USERDOMAIN, $env:USERNAME)
}


function New-CamLauncherStub {
    <#
        Writes the launcher a logon task runs, and returns its path.

        The stub polls for the real script until its path resolves, logs what
        happened, then invokes it with the arguments given. It FORWARDS, never
        copies: a local copy of the scripts would drift from the repo
        silently.

        Overwritten on every install, deleted by the matching remove switch,
        and not meant to be edited by hand.

        Exiting non-zero when the wait runs out is deliberate. It is what lets
        the task's restart-on-failure policy try the whole thing again, which
        is the right answer when WSL simply was not up yet.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$StubPath,
        [Parameter(Mandatory = $true)][string]$TargetScript,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][string[]]$TargetArgument,
        [Parameter(Mandatory = $true)][string]$LogPath,
        [int]$TimeoutSeconds = 300,
        [int]$PollSeconds = 5
    )

    # Every substituted value lands inside a single-quoted literal in the
    # generated file, so a single quote in a path has to be doubled.
    $template = @'
# IPCCTV autostart launcher - GENERATED FILE, do not edit.
# Written by an install switch on cam.ps1 or cam.recorder.ps1, and deleted by
# the matching remove switch.
#
# Why it exists: the script it starts can live on a path that is not there yet
# when the logon task fires (\\wsl.localhost resolves only once WSL is
# running). This waits for the path, then forwards to the real script. It holds
# no copy of it.

$Target         = '__TARGET__'
$LogPath        = '__LOG__'
$TimeoutSeconds = __TIMEOUT__
$PollSeconds    = __POLL__

function Write-StubLog {
    param([string]$Message)
    $line = '{0} {1}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $Message
    try {
        $dir = Split-Path -Path $LogPath -Parent
        if (-not (Test-Path -LiteralPath $dir)) {
            New-Item -ItemType Directory -Path $dir -Force | Out-Null
        }
        Add-Content -LiteralPath $LogPath -Value $line -Encoding UTF8
    }
    catch { }
    Write-Host $line
}

function Test-StubTarget {
    # A path on a share that is not up can fail rather than answer false.
    try { return [bool](Test-Path -LiteralPath $Target) } catch { return $false }
}

Write-StubLog ('launcher starting, waiting up to {0}s for {1}' -f $TimeoutSeconds, $Target)

$waited = 0
while (-not (Test-StubTarget)) {
    if ($waited -ge $TimeoutSeconds) {
        Write-StubLog ('FAILED: {0} did not appear within {1}s. If that is a \\wsl.localhost path, WSL was not running. Exiting 2 so the task restart policy tries again.' -f $Target, $TimeoutSeconds)
        exit 2
    }
    Start-Sleep -Seconds $PollSeconds
    $waited += $PollSeconds
}

Write-StubLog ('target resolved after {0}s, starting {1} __ARGS__' -f $waited, $Target)

try {
    & $Target __ARGS__
}
catch {
    # Anything that stops the forward dead - a binding error, an unreadable
    # script - must be visible in the log and must exit non-zero, or a task
    # that started nothing at all looks like a clean run.
    Write-StubLog ('FAILED to start it: ' + $_.Exception.Message)
    exit 3
}
$code = $LASTEXITCODE
if ($null -eq $code) { $code = 0 }
Write-StubLog ('it exited with {0}' -f $code)
exit $code
'@

    # The arguments are written into the generated script as SOURCE TEXT -
    # "& $Target -Start -Dashboard" - so each one has to be a plain switch
    # name. Passing them as an array for the stub to splat looks tidier and
    # does not work: array splatting hands the elements over as positional
    # values rather than parameter names, so the forward fails to bind and
    # nothing starts. Measured on 2026-10-12, with the dashboard's task
    # logging a clean exit 0 having opened nothing.
    foreach ($argument in $TargetArgument) {
        if ($argument -notmatch '^-[A-Za-z][A-Za-z0-9]*$') {
            throw "A launcher stub forwards its arguments as source text, so each one must be a plain switch name such as -Run. Got '$argument'."
        }
    }
    $argumentLine = ($TargetArgument -join ' ')

    $body = $template.
        Replace('__TARGET__', $TargetScript.Replace("'", "''")).
        Replace('__ARGS__', $argumentLine).
        Replace('__LOG__', $LogPath.Replace("'", "''")).
        Replace('__TIMEOUT__', [string]$TimeoutSeconds).
        Replace('__POLL__', [string]$PollSeconds)

    $dir = Split-Path -Path $StubPath -Parent
    if (-not (Test-Path -LiteralPath $dir)) {
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
    }
    Set-Content -LiteralPath $StubPath -Value $body -Encoding UTF8
    return $StubPath
}


function Remove-CamLauncherStub {
    <#
        Deletes a generated launcher. Returns $true if there was one.

        The launcher's log is deliberately left behind: it is the only record
        of a logon start that failed, and it is the first thing to read when
        autostart did not work. Its folder goes only when nothing else is left
        in it, so an install and removal with nothing logged leaves no trace
        at all, while the other task's launcher and log are untouched.
    #>
    param([Parameter(Mandatory = $true)][string]$StubPath)

    if (-not (Test-Path -LiteralPath $StubPath)) { return $false }
    Remove-Item -LiteralPath $StubPath -Force
    $dir = Split-Path -Path $StubPath -Parent
    if (-not @(Get-ChildItem -LiteralPath $dir -Force -ErrorAction SilentlyContinue).Count) {
        Remove-Item -LiteralPath $dir -Force -ErrorAction SilentlyContinue
    }
    return $true
}


function Get-CamLogonTaskAction {
    <#
        What a logon task runs for one launcher stub: powershell.exe, hidden,
        with the stub as its -File. Register-CamLogonTask registers exactly
        this and Test-CamLogonTaskInstalled expects exactly this, so the two
        cannot disagree about what a correctly installed task looks like.
    #>
    param([Parameter(Mandatory = $true)][string]$StubPath)

    return [pscustomobject]@{
        Execute   = (Join-Path -Path $env:SystemRoot -ChildPath 'System32\WindowsPowerShell\v1.0\powershell.exe')
        Arguments = ('-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "{0}"' -f $StubPath)
    }
}


function Get-CamLauncherStubTarget {
    <#
        The path a generated launcher starts - the $Target line of a script
        launcher, the $Exe line of the UI's - or $null when there is no stub
        or no such line. Reads the file only.
    #>
    param([Parameter(Mandatory = $true)][string]$StubPath)

    if (-not (Test-Path -LiteralPath $StubPath)) { return $null }
    $text = [System.IO.File]::ReadAllText($StubPath)
    $match = [regex]::Match($text, '(?m)^\$(?:Target|Exe)\s*=\s*''((?:[^'']|'''')*)''\s*$')
    if (-not $match.Success) { return $null }
    return $match.Groups[1].Value.Replace("''", "'")
}


function Test-CamLogonTaskInstalled {
    <#
        Whether one logon task is in place and starts what it should, without
        changing anything. Current means all four hold: the task is
        registered, it runs exactly the action Register-CamLogonTask gives it,
        its launcher exists, and the launcher starts Target - so a task left
        pointing at a repository that has since moved is not current. Present
        means there is something to remove: the task, the launcher, or both.

        It does not compare the launcher's text with what today's template
        would write. A launcher from an older version of these scripts still
        counts as current; the granular install switch, or -Uninstall then
        -Install, rewrites it.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$TaskName,
        [Parameter(Mandatory = $true)][string]$StubPath,
        [Parameter(Mandatory = $true)][string]$Target
    )

    $task = Get-CamLogonTask -TaskName $TaskName
    $stubExists = Test-Path -LiteralPath $StubPath
    $result = [pscustomobject]@{ Current = $false; Present = ([bool]$task -or $stubExists); Detail = '' }

    if (-not $task) {
        $result.Detail = $(if ($stubExists) { "no task named '$TaskName', but its launcher $StubPath is there" } else { "no task named '$TaskName'" })
        return $result
    }

    $expected = Get-CamLogonTaskAction -StubPath $StubPath
    $actions = @($task.Actions)
    if ($actions.Count -ne 1 -or $actions[0].Execute -ne $expected.Execute -or $actions[0].Arguments -ne $expected.Arguments) {
        $result.Detail = ("the task '{0}' runs '{1}', not the launcher {2}" -f $TaskName,
            ((@($actions | ForEach-Object { '{0} {1}' -f $_.Execute, $_.Arguments })) -join '; '), $StubPath)
        return $result
    }
    if (-not $stubExists) {
        $result.Detail = "the task '$TaskName' is registered but its launcher $StubPath is missing"
        return $result
    }
    $actual = Get-CamLauncherStubTarget -StubPath $StubPath
    if ($actual -ne $Target) {
        $result.Detail = ("the launcher {0} starts '{1}', not {2}" -f $StubPath, $actual, $Target)
        return $result
    }

    $result.Current = $true
    $result.Detail = ("task '{0}' ({1}) -> {2} -> {3}" -f $TaskName, [string]$task.State, $StubPath, $Target)
    return $result
}


function Register-CamLogonTask {
    <#
        Registers a Scheduled Task that runs one launcher stub at logon as the
        current user, with no elevation anywhere in the path. Returns what it
        did, for the caller to report.

        Idempotent: -Force replaces the registration rather than adding a
        second task, so running an install twice leaves exactly one.

        Three settings are not defaults and are load-bearing. ExecutionTimeLimit
        zero, because the default three-day limit would kill a 24/7 recorder on
        day three - and because the Task Scheduler ending a task tears down what
        it started, which for the dashboard would be the warden that holds its
        panes alive. MultipleInstances IgnoreNew, so a second logon cannot start
        a competing instance. And restart-on-failure, so a start that failed
        because WSL was not up yet is retried.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$TaskName,
        [Parameter(Mandatory = $true)][string]$StubPath,
        [Parameter(Mandatory = $true)][string]$Description
    )

    if (-not (Get-Command -Name 'Register-ScheduledTask' -ErrorAction SilentlyContinue)) {
        throw 'The ScheduledTasks module is not available on this machine, so the task cannot be registered. Windows 8 or later provides it.'
    }

    $taskAction = Get-CamLogonTaskAction -StubPath $StubPath
    $powershell = $taskAction.Execute
    $arguments = $taskAction.Arguments
    $account = Get-CamLogonTaskAccount
    $existing = Get-CamLogonTask -TaskName $TaskName

    $action = New-ScheduledTaskAction -Execute $powershell -Argument $arguments
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $account
    $principal = New-ScheduledTaskPrincipal -UserId $account -LogonType Interactive -RunLevel Limited
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
        -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
        -RestartCount $script:CamLogonTaskRestartCount `
        -RestartInterval (New-TimeSpan -Minutes $script:CamLogonTaskRestartMinutes)

    $task = Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
        -Principal $principal -Settings $settings -Description $Description -Force

    return [pscustomobject]@{
        Task       = $task
        Existed    = [bool]$existing
        Account    = $account
        Executable = $powershell
        Arguments  = $arguments
        Policy     = ("no execution time limit, a second instance is ignored, restart up to {0}x every {1} min on failure" -f `
            $script:CamLogonTaskRestartCount, $script:CamLogonTaskRestartMinutes)
    }
}


function Unregister-CamLogonTask {
    <#
        Unregisters a logon task. $true if there was one, $false if there was
        not - Unregister-ScheduledTask throws on a missing task, so it is
        looked up first and removal is a clean no-op.
    #>
    param([Parameter(Mandatory = $true)][string]$TaskName)

    if (-not (Get-CamLogonTask -TaskName $TaskName)) { return $false }

    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    $left = @(Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue).Count
    if ($left -ne 0) {
        throw "Unregistered '$TaskName' but $left task(s) with that name are still registered."
    }
    return $true
}


# ---------------------------------------------------------------------------
# The control UI (camui.exe)
#
# camui owns no state: it reads status.json and shells the scripts. These
# helpers are what cam.ps1 needs to install it, to report it, and to say where
# it is - nothing here starts, stops or configures it.
# ---------------------------------------------------------------------------

# Must match defaultListen in ui/config.go. camui takes its listen address from
# camui.config.json or its own default; this is the same answer, for reporting
# and for the launcher's readiness probe.
$script:CamUiDefaultListen = '127.0.0.1:8787'


function Get-CamUiListenAddress {
    <#
        The host:port camui will bind, from camui.config.json if there is one,
        otherwise camui's own default.

        Reads the "listen" key and nothing else. A token, if one is
        configured, is never read here and never printed anywhere.
    #>
    param([Parameter(Mandatory = $true)][string]$ConfigPath)

    if (Test-Path -LiteralPath $ConfigPath) {
        try {
            $parsed = Get-Content -LiteralPath $ConfigPath -Raw | ConvertFrom-Json
            if ($parsed -and $parsed.listen) { return [string]$parsed.listen }
        }
        catch {
            # A malformed config is camui's problem to report when it starts;
            # here it only means the address is the default one.
        }
    }
    return $script:CamUiDefaultListen
}


function Split-CamUiListenAddress {
    <#
        host:port split into something connectable. A wildcard host - empty,
        0.0.0.0 or :: - means every interface, and every interface includes
        loopback, so that is the address to probe and the one to print. An
        IPv6 literal keeps its brackets off.
    #>
    param([Parameter(Mandatory = $true)][string]$Address)

    $separator = $Address.LastIndexOf(':')
    if ($separator -lt 0) { throw "The camui listen address '$Address' is not host:port." }
    $listenHost = $Address.Substring(0, $separator).Trim('[', ']')
    $port = 0
    if (-not [int]::TryParse($Address.Substring($separator + 1), [ref]$port)) {
        throw "The camui listen address '$Address' has no numeric port."
    }
    $probeHost = $listenHost
    if (-not $probeHost -or $probeHost -eq '0.0.0.0' -or $probeHost -eq '::') { $probeHost = '127.0.0.1' }

    return [pscustomobject]@{
        Address   = $Address
        Host      = $listenHost
        ProbeHost = $probeHost
        Port      = $port
        Url       = ("http://{0}:{1}/" -f $probeHost, $port)
    }
}


function Get-CamUiRuntimeState {
    <#
        What the control UI is doing: where it would be, whether anything is
        serving there, and which process owns that listening socket.

        The process is identified by the socket it OWNS, never by image name -
        the same rule the process inventory follows. That also makes the answer
        honest when something else has taken the port: the pid is reported
        either way and the caller can see it is not camui.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$ExePath,
        [Parameter(Mandatory = $true)][string]$ConfigPath
    )

    $address = Get-CamUiListenAddress -ConfigPath $ConfigPath
    $parsed = $null
    $problem = ''
    try { $parsed = Split-CamUiListenAddress -Address $address }
    catch { $problem = $_.Exception.Message }

    $ownerPid = 0
    $ownerPath = ''
    $listening = $false
    if ($parsed) {
        $connection = @(Get-NetTCPConnection -LocalPort $parsed.Port -State Listen -ErrorAction SilentlyContinue) |
            Select-Object -First 1
        if ($connection) {
            $listening = $true
            $ownerPid = [int]$connection.OwningProcess
            try { $ownerPath = [string](Get-Process -Id $ownerPid -ErrorAction Stop).Path } catch { $ownerPath = '' }
        }
    }

    # A token being configured is reported; the token itself is not read.
    $tokenConfigured = $false
    if (Test-Path -LiteralPath $ConfigPath) {
        try {
            $config = Get-Content -LiteralPath $ConfigPath -Raw | ConvertFrom-Json
            if ($config -and $config.token -and ([string]$config.token).Trim()) { $tokenConfigured = $true }
        }
        catch { }
    }
    if ($env:CAMUI_TOKEN -and $env:CAMUI_TOKEN.Trim()) { $tokenConfigured = $true }

    return [pscustomobject]@{
        Address         = $address
        Url             = $(if ($parsed) { $parsed.Url } else { '' })
        Problem         = $problem
        Listening       = $listening
        OwnerPid        = $ownerPid
        OwnerPath       = $ownerPath
        IsCamUi         = ($ownerPath -and ($ownerPath -eq $ExePath))
        ExePath         = $ExePath
        ExeBuilt        = (Test-Path -LiteralPath $ExePath)
        ConfigPath      = $ConfigPath
        ConfigExists    = (Test-Path -LiteralPath $ConfigPath)
        TokenConfigured = $tokenConfigured
    }
}


function New-CamUiLauncherStub {
    <#
        Writes the launcher the UI's logon task runs, and returns its path.

        It is not New-CamLauncherStub because the target is not a script to
        forward to. Two things follow from camui being a long-lived console
        program:

        - It must outlive this launcher and every terminal window on the
          machine, so it is started in a console of its OWN with no window -
          CreateNoWindow with UseShellExecute false. Start-Process
          -WindowStyle Hidden would hand it this launcher's console instead,
          and a console created WITH a window is handed to the default
          terminal application, which on this machine makes it a TAB in the
          Operator's Windows Terminal window and kills it when that window is
          closed. That is measured, and it is how the recording supervisor
          died four times in one day.

        - camui logs to stdout only, so a thin powershell.exe holds the pipe
          and appends it to a log file. That is the one process that waits for
          camui, which is also what makes camui identifiable from its parent.

        The binary is NOT copied anywhere. The launcher waits for its path the
        same way the other launchers wait for a script's, so a rebuilt
        camui.exe takes effect the next time it starts, with no reinstall.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$StubPath,
        [Parameter(Mandatory = $true)][string]$ExePath,
        [Parameter(Mandatory = $true)][string]$ConfigPath,
        [Parameter(Mandatory = $true)][string]$LogPath,
        [Parameter(Mandatory = $true)][string]$UiLogPath,
        [int]$TimeoutSeconds = 300,
        [int]$PollSeconds = 5,
        [int]$ServingWaitSeconds = 30,
        [int]$UiLogMaxBytes = 10485760
    )

    # Every substituted value lands inside a single-quoted literal in the
    # generated file, so a single quote in a path has to be doubled.
    $template = @'
# IPCCTV camui launcher - GENERATED FILE, do not edit.
# Written by .\cam.ps1 -InstallUi and deleted by .\cam.ps1 -RemoveUi.
#
# What this has to get right, and why it is not one line:
#
#   camui.exe can live on a path that is not there yet when the logon task
#   fires (\\wsl.localhost resolves only once WSL is running), so this waits
#   for it rather than failing.
#
#   camui is a console program that must outlive this launcher and every
#   terminal window on the machine, so it is started in a console of its OWN
#   with no window. A console created WITH a window is handed to the default
#   terminal application and arrives as a TAB in an existing Windows Terminal
#   window, which kills it when that window is closed.
#
#   camui logs to stdout only, so a thin powershell.exe holds the pipe and
#   appends it to the UI log.
#
#   Starting a second camui would only fail to bind the port, so if the
#   address is already being served this exits without starting anything.

$Exe                = '__EXE__'
$ConfigPath         = '__CONFIG__'
$DefaultListen      = '__LISTEN__'
$LogPath            = '__LOG__'
$UiLogPath          = '__UILOG__'
$TimeoutSeconds     = __TIMEOUT__
$PollSeconds        = __POLL__
$ServingWaitSeconds = __SERVINGWAIT__
$UiLogMaxBytes      = __UILOGMAX__

function Write-StubLog {
    param([string]$Message)
    $line = '{0} {1}' -f (Get-Date -Format 'yyyy-MM-dd HH:mm:ss'), $Message
    try {
        $dir = Split-Path -Path $LogPath -Parent
        if (-not (Test-Path -LiteralPath $dir)) {
            New-Item -ItemType Directory -Path $dir -Force | Out-Null
        }
        Add-Content -LiteralPath $LogPath -Value $line -Encoding UTF8
    }
    catch { }
    Write-Host $line
}

function Test-StubTarget {
    # A path on a share that is not up can fail rather than answer false.
    try { return [bool](Test-Path -LiteralPath $Exe) } catch { return $false }
}

function Get-StubListenAddress {
    # camui.config.json wins if it is there, exactly as camui itself reads it.
    if (Test-Path -LiteralPath $ConfigPath) {
        try {
            $parsed = Get-Content -LiteralPath $ConfigPath -Raw | ConvertFrom-Json
            if ($parsed -and $parsed.listen) { return [string]$parsed.listen }
        }
        catch {
            Write-StubLog ('could not read {0}: {1}' -f $ConfigPath, $_.Exception.Message)
        }
    }
    return $DefaultListen
}

function Test-StubServing {
    # Something answering on the address is camui already up. The listening
    # socket is the only honest test; an image name is never one.
    param([string]$Address)

    $separator = $Address.LastIndexOf(':')
    if ($separator -lt 0) { return $false }
    $probeHost = $Address.Substring(0, $separator).Trim('[', ']')
    if (-not $probeHost -or $probeHost -eq '0.0.0.0' -or $probeHost -eq '::') { $probeHost = '127.0.0.1' }
    $port = 0
    if (-not [int]::TryParse($Address.Substring($separator + 1), [ref]$port)) { return $false }

    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $attempt = $client.BeginConnect($probeHost, $port, $null, $null)
        if (-not $attempt.AsyncWaitHandle.WaitOne(1000)) { return $false }
        $client.EndConnect($attempt)
        return $true
    }
    catch { return $false }
    finally { $client.Close() }
}

Write-StubLog ('launcher starting, waiting up to {0}s for {1}' -f $TimeoutSeconds, $Exe)

$waited = 0
while (-not (Test-StubTarget)) {
    if ($waited -ge $TimeoutSeconds) {
        Write-StubLog ('FAILED: {0} did not appear within {1}s. If that is a \\wsl.localhost path, WSL was not running; if it is never there, build it with ui/build.sh. Exiting 2 so the task restart policy tries again.' -f $Exe, $TimeoutSeconds)
        exit 2
    }
    Start-Sleep -Seconds $PollSeconds
    $waited += $PollSeconds
}

$listen = Get-StubListenAddress
if (Test-StubServing -Address $listen) {
    Write-StubLog ('{0} is already being served, so nothing was started' -f $listen)
    exit 0
}

# One log, rolled when it gets large: camui writes a line per request and
# nothing else would ever bound it.
try {
    $existing = Get-Item -LiteralPath $UiLogPath -ErrorAction SilentlyContinue
    if ($existing -and $existing.Length -gt $UiLogMaxBytes) {
        Move-Item -LiteralPath $UiLogPath -Destination ($UiLogPath + '.prev') -Force
        Write-StubLog ('rolled {0} at {1} bytes' -f $UiLogPath, $existing.Length)
    }
}
catch { Write-StubLog ('could not roll {0}: {1}' -f $UiLogPath, $_.Exception.Message) }

$arguments = ''
if (Test-Path -LiteralPath $ConfigPath) {
    $arguments = " -config '" + $ConfigPath.Replace("'", "''") + "'"
}
$pump = "& '" + $Exe.Replace("'", "''") + "'" + $arguments + " *>> '" + $UiLogPath.Replace("'", "''") + "'"
$encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($pump))

$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = Join-Path -Path $env:SystemRoot -ChildPath 'System32\WindowsPowerShell\v1.0\powershell.exe'
$psi.Arguments = ('-NoProfile -ExecutionPolicy Bypass -EncodedCommand {0}' -f $encoded)
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
try {
    $process = [System.Diagnostics.Process]::Start($psi)
    [void]$process.Handle
}
catch {
    Write-StubLog ('FAILED to start it: ' + $_.Exception.Message)
    exit 3
}
Write-StubLog ('started camui under pid {0}, output appending to {1}' -f $process.Id, $UiLogPath)

$waitedForServing = 0
while (-not (Test-StubServing -Address $listen)) {
    if ($waitedForServing -ge $ServingWaitSeconds) {
        Write-StubLog ('FAILED: nothing is serving {0} after {1}s. Read {2}. Exiting 4 so the task restart policy tries again.' -f $listen, $ServingWaitSeconds, $UiLogPath)
        exit 4
    }
    Start-Sleep -Seconds 1
    $waitedForServing += 1
}
Write-StubLog ('camui is serving {0} after {1}s' -f $listen, $waitedForServing)
exit 0
'@

    $body = $template.
        Replace('__EXE__', $ExePath.Replace("'", "''")).
        Replace('__CONFIG__', $ConfigPath.Replace("'", "''")).
        Replace('__LISTEN__', $script:CamUiDefaultListen.Replace("'", "''")).
        Replace('__LOG__', $LogPath.Replace("'", "''")).
        Replace('__UILOG__', $UiLogPath.Replace("'", "''")).
        Replace('__TIMEOUT__', [string]$TimeoutSeconds).
        Replace('__POLL__', [string]$PollSeconds).
        Replace('__SERVINGWAIT__', [string]$ServingWaitSeconds).
        Replace('__UILOGMAX__', [string]$UiLogMaxBytes)

    $dir = Split-Path -Path $StubPath -Parent
    if (-not (Test-Path -LiteralPath $dir)) {
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
    }
    Set-Content -LiteralPath $StubPath -Value $body -Encoding UTF8
    return $StubPath
}
