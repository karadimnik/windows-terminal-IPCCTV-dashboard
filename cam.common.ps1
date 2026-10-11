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
        HealthStripHeightPx  = 140
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
