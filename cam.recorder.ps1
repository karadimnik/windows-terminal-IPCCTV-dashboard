<#
.SYNOPSIS
    Continuous RTSP recorder for the IPCCTV dashboard cameras.

.DESCRIPTION
    -Run       Supervises one ffmpeg child per camera whose Record flag is true in
               cameras.json, writing clock-aligned segments with audio. Picks up
               Record changes live, writes status.json, and prunes the archive.
    -Enable    Sets Record to true for one camera or for all of them. A running
               supervisor starts recording it within ReconcileSeconds.
    -Disable   Sets Record to false. The child stops without a restart.
    -List      Every camera with its Record state and stream quality.
    -Status    Renders status.json, saying so when it is stale.
    -Stop      Asks a running supervisor to shut down and waits for it.
    -SelfTest  Pre-flight report: tool check, per-camera RTSP probe, measured
               concurrent-connection limit, crash-safety proof, disk projection.
               Add -Offline to skip every check that touches a camera.

    Targets Windows PowerShell 5.1.

.EXAMPLE
    .\cam.recorder.ps1 -SelfTest

.EXAMPLE
    .\cam.recorder.ps1 -Enable "Front Entrance"

.EXAMPLE
    .\cam.recorder.ps1 -Run
#>
[CmdletBinding(DefaultParameterSetName = 'Run')]
param(
    [Parameter(ParameterSetName = 'Run')]
    [switch]$Run,

    [Parameter(ParameterSetName = 'Enable', Mandatory = $true, Position = 0)]
    [string]$Enable,

    [Parameter(ParameterSetName = 'Disable', Mandatory = $true, Position = 0)]
    [string]$Disable,

    [Parameter(ParameterSetName = 'List')]
    [switch]$List,

    [Parameter(ParameterSetName = 'Status')]
    [switch]$Status,

    [Parameter(ParameterSetName = 'Stop')]
    [switch]$Stop,

    [Parameter(ParameterSetName = 'SelfTest')]
    [switch]$SelfTest,

    # Skips every check that opens an RTSP session, so the self-test can be run
    # while the dashboard is up. These cameras serve only two sessions each, and
    # the concurrency check measures 1 instead of 2 when a viewer holds one.
    [Parameter(ParameterSetName = 'SelfTest')]
    [switch]$Offline,

    # Highest number of simultaneous RTSP sessions the probe will attempt.
    [Parameter(ParameterSetName = 'SelfTest')]
    [ValidateRange(2, 16)]
    [int]$MaxConcurrency = 5
)

$ErrorActionPreference = 'Stop'

. (Join-Path -Path $PSScriptRoot -ChildPath 'cam.common.ps1')


# ===========================================================================
# Shared helpers
# ===========================================================================

function Write-CamHost {
    <# Console output. Always redacted - ffmpeg echoes the full RTSP URL on error. #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Message,
        [string]$Colour = 'Gray'
    )
    Write-Host (Get-CamRedacted -Text $Message) -ForegroundColor $Colour
}


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


function Invoke-CamNative {
    <#
        Runs a native command and returns its combined stdout and stderr as one
        string.

        $ErrorActionPreference = 'Stop' turns any line a native command writes to
        stderr into a terminating NativeCommandError, which would abort the
        self-test the moment ffmpeg reported an expected connection failure. The
        preference is therefore relaxed around the call and restored after it.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$Arguments
    )

    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $output = (& $FilePath @Arguments 2>&1 | Out-String)
    }
    finally {
        $ErrorActionPreference = $previous
    }
    return $output
}


function Get-CamResolvedCameras {
    <#
        Reads cameras.json and returns one record per camera with its safe name,
        directory, RTSP URL and Record state resolved. A camera with an unusable
        name is reported rather than silently skipped.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$Credential
    )

    $resolved = @()
    foreach ($camera in (Get-CamList)) {
        $record = [pscustomobject]@{
            Name     = [string]$camera.Name
            IP       = [string]$camera.IP
            Quality  = [string]$camera.Quality
            Record   = (Get-CamRecordEnabled -Camera $camera)
            SafeName = $null
            Url      = $null
            Error    = $null
        }
        try {
            $record.SafeName = Get-CamSafeName -Name $record.Name
            $record.Url = Get-CamRtspUrl -Camera $camera -Credential $Credential
        }
        catch {
            $record.Error = $_.Exception.Message
        }
        $resolved += $record
    }
    return , $resolved
}


function Start-CamProcess {
    <#
        Starts a hidden child process with stderr redirected to a file, returning
        the Process object. $p.Handle is touched immediately because ExitCode can
        otherwise come back as $null.
        Used by the self-test only; the supervisor needs a live stderr pipe.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [Parameter(Mandatory = $true)][string]$StdErrPath
    )

    $process = Start-Process -FilePath $FilePath `
        -ArgumentList (Get-CamCommandLine -Arguments $Arguments) `
        -RedirectStandardError $StdErrPath `
        -WindowStyle Hidden -PassThru
    [void]$process.Handle
    return $process
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


# ===========================================================================
# Status file: written by the supervisor, read by everything else
# ===========================================================================

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


# ===========================================================================
# CLI: -List, -Enable, -Disable
# ===========================================================================

function Get-CamMatchedCameras {
    <#
        Resolves a CLI target to camera records. Name matching is
        case-insensitive. An exact name match wins over the 'all' keyword, so a
        camera that happens to be called "all" stays addressable.
    #>
    param(
        [Parameter(Mandatory = $true)][object[]]$Cameras,
        [Parameter(Mandatory = $true)][string]$Target
    )

    $matched = @()
    foreach ($camera in $Cameras) {
        if ([string]$camera.Name -eq $Target) { $matched += $camera }
    }
    if ($matched.Count -eq 0 -and $Target -eq 'all') { $matched = @($Cameras) }

    return , $matched
}


function Show-CamValidTargets {
    param([Parameter(Mandatory = $true)][object[]]$Cameras)

    Write-CamHost -Message 'Valid targets:' -Colour 'Cyan'
    foreach ($camera in $Cameras) {
        Write-CamHost -Message ("  {0}" -f [string]$camera.Name)
    }
    Write-CamHost -Message '  all  (every camera in cameras.json)'
}


function Show-CamList {
    <# Every camera with its Record state and stream quality. #>
    param([Parameter(Mandatory = $true)]$Config)

    $cameras = Get-CamList

    Write-CamHost -Message ''
    Write-CamHost -Message ("{0,-22} {1,-8} {2,-9} {3}" -f 'CAMERA', 'RECORD', 'QUALITY', 'IP') -Colour 'Cyan'
    foreach ($camera in $cameras) {
        $record = 'off'
        $colour = 'Gray'
        if (Get-CamRecordEnabled -Camera $camera) { $record = 'ON'; $colour = 'Green' }

        $quality = [string]$camera.Quality
        if ([string]::IsNullOrWhiteSpace($quality)) { $quality = 'stream1' }

        $suffix = ''
        try { [void](Get-CamSafeName -Name ([string]$camera.Name)) }
        catch { $suffix = "  !! $($_.Exception.Message)" }

        Write-CamHost -Message ("{0,-22} {1,-8} {2,-9} {3}{4}" -f [string]$camera.Name, $record, $quality, [string]$camera.IP, $suffix) -Colour $colour
    }

    $enabled = 0
    foreach ($camera in $cameras) { if (Get-CamRecordEnabled -Camera $camera) { $enabled++ } }
    Write-CamHost -Message ''
    Write-CamHost -Message ("{0} of {1} camera(s) set to record. Archive root {2}." -f $enabled, @($cameras).Count, $Config.RootPath)

    $supervisor = Get-CamSupervisorState -Config $Config -Snapshot (Get-CamStatusSnapshot -Config $Config)
    if ($supervisor.Running) {
        Write-CamHost -Message ("Supervisor is running ({0})." -f $supervisor.Reason) -Colour 'Green'
    }
    else {
        Write-CamHost -Message ("Supervisor is NOT running: {0}." -f $supervisor.Reason) -Colour 'Yellow'
    }
    Write-CamHost -Message ''
}


function Set-CamRecordState {
    <#
        Persists Record for one camera or all of them, through the atomic writer
        so a crash mid-write can only leave the old file or the new one. Every
        pre-existing field survives: the objects Get-CamList returned are written
        back untouched apart from Record.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Target,
        [Parameter(Mandatory = $true)][bool]$Enabled
    )

    $cameras = Get-CamList
    $matched = Get-CamMatchedCameras -Cameras $cameras -Target $Target

    if (@($matched).Count -eq 0) {
        Write-CamHost -Message ("No camera named '{0}' in cameras.json." -f $Target) -Colour 'Red'
        Show-CamValidTargets -Cameras $cameras
        exit 1
    }

    # Refuse to enable a camera whose name cannot become a directory, rather
    # than letting the supervisor skip it later for a reason nobody sees.
    if ($Enabled) {
        foreach ($camera in $matched) { [void](Get-CamSafeName -Name ([string]$camera.Name)) }
    }

    $word = 'off'
    if ($Enabled) { $word = 'ON' }

    $changed = 0
    foreach ($camera in $matched) {
        $before = Get-CamRecordEnabled -Camera $camera
        if ($before -eq $Enabled) {
            Write-CamHost -Message ("  {0,-22} already {1}" -f [string]$camera.Name, $word)
            continue
        }

        $property = $camera.PSObject.Properties['Record']
        if ($property) { $camera.Record = $Enabled }
        else { $camera | Add-Member -MemberType NoteProperty -Name 'Record' -Value $Enabled }

        $changed++
        $colour = 'Yellow'
        if ($Enabled) { $colour = 'Green' }
        Write-CamHost -Message ("  {0,-22} record -> {1}" -f [string]$camera.Name, $word) -Colour $colour
    }

    if ($changed -eq 0) {
        Write-CamHost -Message 'Nothing to change; cameras.json left untouched.'
        return
    }

    Set-CamList -Cameras $cameras

    $supervisor = Get-CamSupervisorState -Config $Config -Snapshot (Get-CamStatusSnapshot -Config $Config)
    if ($supervisor.Running) {
        Write-CamHost -Message ("cameras.json updated. The running supervisor applies this within {0}s." -f $Config.ReconcileSeconds) -Colour 'Cyan'
    }
    else {
        Write-CamHost -Message 'cameras.json updated. No supervisor is running, so start one with -Run.' -Colour 'Cyan'
    }
}


# ===========================================================================
# Self-test
# ===========================================================================

$script:CamTestResults = @()

function Add-CamTestResult {
    param(
        [Parameter(Mandatory = $true)][string]$Check,
        [Parameter(Mandatory = $true)][ValidateSet('PASS', 'FAIL', 'INFO')][string]$Status,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Detail
    )

    $script:CamTestResults += [pscustomobject]@{ Check = $Check; Status = $Status; Detail = $Detail }

    $colour = 'Gray'
    if ($Status -eq 'PASS') { $colour = 'Green' }
    if ($Status -eq 'FAIL') { $colour = 'Red' }
    Write-CamHost -Message ("  [{0}] {1} {2}" -f $Status, $Check.PadRight(46), $Detail) -Colour $colour
}


function Test-CamTooling {
    <#
        Check (a): both binaries present, and this build takes -timeout rather
        than the pre-5.0 -stimeout. Verified against an unreachable RTSP URL,
        because -timeout is a protocol option and is rejected by demuxers that
        have no socket.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    foreach ($pair in @(@('ffmpeg', $Config.FfmpegPath), @('ffprobe', $Config.FfprobePath))) {
        if (Test-Path -LiteralPath $pair[1]) {
            $version = ((Invoke-CamNative -FilePath $pair[1] -Arguments @('-hide_banner', '-version')) -split "`r?`n")[0]
            Add-CamTestResult -Check "$($pair[0]) present" -Status 'PASS' -Detail ([string]$version)
        }
        else {
            Add-CamTestResult -Check "$($pair[0]) present" -Status 'FAIL' -Detail "not found at $($pair[1])"
            return $false
        }
    }

    $dead = 'rtsp://127.0.0.1:1/selftest'

    $accepted = Invoke-CamNative -FilePath $Config.FfmpegPath -Arguments @(
        '-hide_banner', '-loglevel', 'error', '-rtsp_transport', 'tcp',
        '-timeout', '1500000', '-i', $dead, '-f', 'null', '-')
    if ($accepted -match 'Unrecognized option' -or $accepted -match 'Option timeout not found') {
        Add-CamTestResult -Check '-timeout accepted by this build' -Status 'FAIL' -Detail (($accepted -split "`r?`n")[0])
        return $false
    }
    Add-CamTestResult -Check '-timeout accepted by this build' -Status 'PASS' `
        -Detail 'option reached the tcp layer (connection error, not an option error)'

    $rejected = Invoke-CamNative -FilePath $Config.FfmpegPath -Arguments @(
        '-hide_banner', '-loglevel', 'error', '-rtsp_transport', 'tcp',
        '-stimeout', '1500000', '-i', $dead, '-f', 'null', '-')
    if ($rejected -match "Unrecognized option 'stimeout'") {
        Add-CamTestResult -Check '-stimeout rejected by this build' -Status 'PASS' `
            -Detail 'renamed at ffmpeg 5.0, as expected'
    }
    else {
        Add-CamTestResult -Check '-stimeout rejected by this build' -Status 'FAIL' `
            -Detail 'build still accepts the pre-5.0 spelling; re-check the flag set'
    }

    return $true
}


function Test-CamStreams {
    <#
        Check (b): probe every camera and report resolution plus both codec
        names. The URL is never printed.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][object[]]$Cameras
    )

    $reachable = @()
    foreach ($camera in $Cameras) {
        if ($camera.Error) {
            Add-CamTestResult -Check "probe $($camera.Name)" -Status 'FAIL' -Detail $camera.Error
            continue
        }

        $raw = Invoke-CamNative -FilePath $Config.FfprobePath -Arguments @(
            '-v', 'error', '-rtsp_transport', 'tcp', '-timeout', '10000000',
            '-print_format', 'json', '-show_streams', '-i', $camera.Url)

        $video = $null
        $audio = $null
        try {
            foreach ($stream in @((ConvertFrom-Json -InputObject $raw).streams)) {
                if ($stream.codec_type -eq 'video' -and -not $video) { $video = $stream }
                if ($stream.codec_type -eq 'audio' -and -not $audio) { $audio = $stream }
            }
        }
        catch {
            $video = $null
        }

        if (-not $video) {
            $first = (($raw -split "`r?`n") | Where-Object { $_.Trim() } | Select-Object -First 1)
            Add-CamTestResult -Check "probe $($camera.Name)" -Status 'FAIL' -Detail ([string]$first)
            continue
        }

        $audioText = 'NONE (video will still record)'
        if ($audio) {
            $audioText = "$($audio.codec_name) $($audio.sample_rate) Hz, $($audio.channels) ch"
        }
        Add-CamTestResult -Check "probe $($camera.Name)" -Status 'PASS' `
            -Detail ("{0} {1}x{2}, audio {3}" -f $video.codec_name, $video.width, $video.height, $audioText)
        $reachable += $camera
    }

    return , $reachable
}


function Measure-CamConcurrency {
    <#
        Check (c): how many simultaneous RTSP sessions each camera actually
        serves. Opens 1, then 2, then 3 overlapping sessions and so on until one
        fails, then reports the highest count where all sessions succeeded.

        This is the measurement the whole direct-connection design rests on: the
        dashboard holds one session per camera and the recorder needs a second,
        so anything below 2 means a restreamer is mandatory.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][object[]]$Cameras,
        [Parameter(Mandatory = $true)][string]$WorkDir,
        [Parameter(Mandatory = $true)][int]$MaxConcurrency
    )

    $measured = @{}

    foreach ($camera in $Cameras) {
        $limit = 0
        $failureDetail = ''

        for ($level = 1; $level -le $MaxConcurrency; $level++) {
            $processes = @()
            $logs = @()
            for ($i = 1; $i -le $level; $i++) {
                $log = Join-Path -Path $WorkDir -ChildPath ("conc_{0}_{1}_{2}.err" -f $camera.SafeName, $level, $i)
                $logs += $log
                $arguments = @(
                    '-hide_banner', '-loglevel', 'error', '-nostdin',
                    '-rtsp_transport', 'tcp', '-timeout', '10000000',
                    '-i', $camera.Url, '-t', '6', '-f', 'null', '-'
                )
                $processes += (Start-CamProcess -FilePath $Config.FfmpegPath -Arguments $arguments -StdErrPath $log)
                # Stagger the starts so every session genuinely overlaps the others.
                Start-Sleep -Milliseconds 400
            }

            foreach ($process in $processes) { $process.WaitForExit() }

            $failed = 0
            foreach ($index in 0..($processes.Count - 1)) {
                if ($processes[$index].ExitCode -ne 0) {
                    $failed++
                    if (-not $failureDetail -and (Test-Path -LiteralPath $logs[$index])) {
                        $text = ([System.IO.File]::ReadAllText($logs[$index]) -split "`r?`n") |
                            Where-Object { $_.Trim() } | Select-Object -First 1
                        $failureDetail = [string]$text
                    }
                }
            }
            foreach ($process in $processes) { $process.Dispose() }

            if ($failed -gt 0) { break }
            $limit = $level
        }

        $measured[$camera.Name] = $limit
        $status = 'PASS'
        if ($limit -lt 2) { $status = 'FAIL' }

        $detail = "$limit simultaneous RTSP session(s)"
        if ($limit -ge $MaxConcurrency) { $detail += " (probe ceiling $MaxConcurrency, real limit may be higher)" }
        if ($failureDetail) { $detail += "; first failure: " + (Get-CamRedacted -Text $failureDetail) }
        Add-CamTestResult -Check "concurrency $($camera.Name)" -Status $status -Detail $detail
    }

    return $measured
}


function Test-CamCrashSafety {
    <#
        Check (d): record with a short segment time, hard-kill ffmpeg mid-segment
        with taskkill /f, then ffprobe every file produced. Both the completed
        segments and the interrupted one must report a readable h264 video stream
        and a pcm_alaw audio stream - that is what makes a power cut survivable.

        Returns the measured bytes-per-second so the disk projection uses real
        numbers from this camera rather than a table value.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$Camera,
        [Parameter(Mandatory = $true)][string]$WorkDir
    )

    $segmentSeconds = 20
    $killAfter = 70

    $testConfig = Get-CamConfig
    $testConfig.SegmentSeconds = $segmentSeconds
    $containerProfile = Get-CamContainerProfile -Config $testConfig

    $cameraDir = Join-Path -Path $WorkDir -ChildPath $Camera.SafeName
    [void](New-Item -ItemType Directory -Path $cameraDir -Force)

    $pattern = Get-CamSegmentPattern -CameraDir $cameraDir -SafeName $Camera.SafeName -ContainerProfile $containerProfile
    $arguments = Get-CamFfmpegArgs -Url $Camera.Url -OutputPattern $pattern -Config $testConfig -ContainerProfile $containerProfile

    Write-CamHost -Message ("  recording {0}s from {1} at {2}s segments, then taskkill /f" -f $killAfter, $Camera.Name, $segmentSeconds)

    $stdErr = Join-Path -Path $WorkDir -ChildPath 'crashtest.err'
    $started = [DateTime]::Now
    $process = Start-CamProcess -FilePath $Config.FfmpegPath -Arguments $arguments -StdErrPath $stdErr

    try {
        $deadline = $started.AddSeconds($killAfter)
        while ([DateTime]::Now -lt $deadline -and -not $process.HasExited) {
            Start-Sleep -Milliseconds 500
        }

        if ($process.HasExited) {
            $message = ''
            if (Test-Path -LiteralPath $stdErr) { $message = [System.IO.File]::ReadAllText($stdErr) }
            Add-CamTestResult -Check 'crash safety' -Status 'FAIL' `
                -Detail ("ffmpeg exited on its own after {0:n0}s: {1}" -f ([DateTime]::Now - $started).TotalSeconds, (($message -split "`r?`n")[0]))
            return 0
        }

        [void](Invoke-CamNative -FilePath 'taskkill.exe' -Arguments @('/f', '/pid', [string]$process.Id))
        $process.WaitForExit(10000) | Out-Null
    }
    finally {
        if (-not $process.HasExited) { $process.Kill() }
    }

    $elapsed = ([DateTime]::Now - $started).TotalSeconds
    $files = @(Get-ChildItem -LiteralPath $cameraDir -File | Sort-Object Name)

    if ($files.Count -lt 2) {
        Add-CamTestResult -Check 'crash safety' -Status 'FAIL' `
            -Detail "expected at least one completed and one interrupted segment, got $($files.Count) file(s)"
        return 0
    }

    $bad = @()
    foreach ($index in 0..($files.Count - 1)) {
        $file = $files[$index]
        $label = 'completed'
        if ($index -eq $files.Count - 1) { $label = 'interrupted' }

        $raw = Invoke-CamNative -FilePath $Config.FfprobePath -Arguments @(
            '-v', 'error', '-print_format', 'json', '-show_streams', '-i', $file.FullName)
        $video = $null
        $audio = $null
        try {
            foreach ($stream in @((ConvertFrom-Json -InputObject $raw).streams)) {
                if ($stream.codec_type -eq 'video' -and -not $video) { $video = $stream }
                if ($stream.codec_type -eq 'audio' -and -not $audio) { $audio = $stream }
            }
        }
        catch {
            $video = $null
        }

        $videoName = 'none'
        if ($video) { $videoName = [string]$video.codec_name }
        $audioName = 'none'
        if ($audio) { $audioName = [string]$audio.codec_name }

        $ok = ($videoName -eq 'h264' -and $audioName -eq 'pcm_alaw')
        if (-not $ok) { $bad += $file.Name }

        $status = 'PASS'
        if (-not $ok) { $status = 'FAIL' }
        Add-CamTestResult -Check ("  {0} {1}" -f $label, $file.Name) -Status $status `
            -Detail ("{0} bytes, video={1}, audio={2}" -f $file.Length, $videoName, $audioName)
    }

    $status = 'PASS'
    if ($bad.Count -gt 0) { $status = 'FAIL' }
    Add-CamTestResult -Check 'crash safety' -Status $status `
        -Detail ("{0} file(s) checked, {1} unreadable, killed with taskkill /f mid-segment" -f $files.Count, $bad.Count)

    $totalBytes = ($files | Measure-Object -Property Length -Sum).Sum
    if ($elapsed -le 0) { return 0 }
    return ($totalBytes / $elapsed)
}


function Show-CamDiskProjection {
    <#
        Check (e): turn the measured byte rate into GB per day and days of
        headroom against the reserve, for the cameras actually opted in.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][double]$BytesPerSecond,
        [Parameter(Mandatory = $true)][int]$EnabledCount,
        [Parameter(Mandatory = $true)][int]$TotalCount
    )

    if ($BytesPerSecond -le 0) {
        Add-CamTestResult -Check 'disk projection' -Status 'FAIL' -Detail 'no byte rate measured (the recording check did not complete)'
        return
    }

    $perCameraGBPerDay = ($BytesPerSecond * 86400) / 1GB
    $freeGB = Get-CamFreeSpaceGB -Path $Config.RootPath
    $usableGB = $freeGB - [double]$Config.ReserveGB

    Add-CamTestResult -Check 'measured rate (1 camera, main stream)' -Status 'INFO' `
        -Detail ("{0} KB/s = {1} GB/day" -f (Format-CamNumber -Value ($BytesPerSecond / 1KB) -Format '0'), (Format-CamNumber -Value $perCameraGBPerDay))

    foreach ($scenario in @(@('cameras with Record=true', $EnabledCount), @('all cameras in cameras.json', $TotalCount))) {
        $count = [int]$scenario[1]
        if ($count -le 0) {
            Add-CamTestResult -Check "projection, $($scenario[0])" -Status 'INFO' -Detail 'none'
            continue
        }
        $totalGBPerDay = $perCameraGBPerDay * $count
        $days = 0
        if ($totalGBPerDay -gt 0) { $days = $usableGB / $totalGBPerDay }
        Add-CamTestResult -Check "projection, $($scenario[0])" -Status 'INFO' `
            -Detail ("{0} camera(s) = {1} GB/day; {2} GB free - {3} GB reserve = {4} GB usable = {5} days" -f `
                $count, (Format-CamNumber -Value $totalGBPerDay), (Format-CamNumber -Value $freeGB),
            (Format-CamNumber -Value ([double]$Config.ReserveGB) -Format '0'),
            (Format-CamNumber -Value $usableGB), (Format-CamNumber -Value $days -Format '0.0'))
    }
}


function Test-CamListIntegrity {
    <#
        Check (f): cameras.json itself. Counts, the UTF-8 BOM, and the two name
        problems that would otherwise surface much later - a name that cannot
        become a directory, and two names that sanitise to the same directory,
        which would have two cameras writing into one folder.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $path = Join-Path -Path (Get-CamLibRoot) -ChildPath 'cameras.json'
    if (-not (Test-Path -LiteralPath $path)) {
        Add-CamTestResult -Check 'cameras.json present' -Status 'FAIL' -Detail "not found at $path"
        return $false
    }

    $head = New-Object byte[] 3
    $stream = [System.IO.File]::OpenRead($path)
    try { $read = $stream.Read($head, 0, 3) } finally { $stream.Dispose() }
    $hasBom = ($read -eq 3 -and $head[0] -eq 0xEF -and $head[1] -eq 0xBB -and $head[2] -eq 0xBF)

    $cameras = Get-CamList -Path $path
    Add-CamTestResult -Check 'cameras.json readable' -Status 'PASS' `
        -Detail ("{0} camera(s), UTF-8 BOM {1} (an atomic rewrite preserves whichever it is)" -f @($cameras).Count, $(if ($hasBom) { 'present' } else { 'absent' }))

    $ok = $true
    $safeNames = @{}
    foreach ($camera in $cameras) {
        $name = [string]$camera.Name
        $safe = $null
        try { $safe = Get-CamSafeName -Name $name }
        catch {
            Add-CamTestResult -Check "name '$name'" -Status 'FAIL' -Detail $_.Exception.Message
            $ok = $false
            continue
        }
        $key = $safe.ToUpperInvariant()
        if ($safeNames.ContainsKey($key)) {
            Add-CamTestResult -Check "name '$name'" -Status 'FAIL' `
                -Detail ("sanitises to '{0}', the same directory as '{1}'" -f $safe, $safeNames[$key])
            $ok = $false
            continue
        }
        $safeNames[$key] = $name
    }

    if ($ok) {
        Add-CamTestResult -Check 'every camera name is usable as a directory' -Status 'PASS' `
            -Detail ("{0} distinct directory name(s)" -f $safeNames.Count)
    }
    return $ok
}


function Test-CamRecordRoundTrip {
    <#
        Check (g): -Enable and -Disable write Record without losing anything
        else. Run against a copy in the work directory, never against the real
        file: a self-test must not change which cameras are recording.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$WorkDir
    )

    $source = Join-Path -Path (Get-CamLibRoot) -ChildPath 'cameras.json'
    $copy = Join-Path -Path $WorkDir -ChildPath 'cameras.json'
    Copy-Item -LiteralPath $source -Destination $copy -Force

    $original = Get-CamList -Path $source
    $ok = $true

    foreach ($pass in @($true, $false)) {
        $cameras = Get-CamList -Path $copy
        foreach ($camera in $cameras) {
            $property = $camera.PSObject.Properties['Record']
            if ($property) { $camera.Record = $pass }
            else { $camera | Add-Member -MemberType NoteProperty -Name 'Record' -Value $pass }
        }
        Set-CamList -Cameras $cameras -Path $copy

        $after = Get-CamList -Path $copy
        $word = 'false'
        if ($pass) { $word = 'true' }

        if (@($after).Count -ne @($original).Count) {
            Add-CamTestResult -Check "Record=$word round trip" -Status 'FAIL' `
                -Detail ("camera count changed: {0} -> {1}" -f @($original).Count, @($after).Count)
            $ok = $false
            continue
        }

        $problems = @()
        for ($i = 0; $i -lt @($original).Count; $i++) {
            $before = @($original)[$i]
            $now = @($after)[$i]

            if ((Get-CamRecordEnabled -Camera $now) -ne $pass) {
                $problems += "$($now.Name): Record is not $word"
            }
            foreach ($property in $before.PSObject.Properties) {
                if ($property.Name -eq 'Record') { continue }
                $target = $now.PSObject.Properties[$property.Name]
                if (-not $target) { $problems += "$($before.Name): lost field $($property.Name)"; continue }
                if ([string]$target.Value -ne [string]$property.Value) {
                    $problems += "$($before.Name): field $($property.Name) changed"
                }
            }
        }

        $head = New-Object byte[] 3
        $stream = [System.IO.File]::OpenRead($copy)
        try { [void]$stream.Read($head, 0, 3) } finally { $stream.Dispose() }
        $sourceHead = New-Object byte[] 3
        $stream = [System.IO.File]::OpenRead($source)
        try { [void]$stream.Read($sourceHead, 0, 3) } finally { $stream.Dispose() }
        if ("$($head[0]),$($head[1]),$($head[2])" -ne "$($sourceHead[0]),$($sourceHead[1]),$($sourceHead[2])") {
            $problems += 'the byte order mark was not preserved'
        }

        if ($problems.Count -gt 0) {
            Add-CamTestResult -Check "Record=$word round trip" -Status 'FAIL' -Detail ($problems -join '; ')
            $ok = $false
        }
        else {
            Add-CamTestResult -Check "Record=$word round trip" -Status 'PASS' `
                -Detail ("{0} camera(s) rewritten, every other field and the BOM preserved" -f @($after).Count)
        }
    }

    return $ok
}


function Test-CamAtomicWrite {
    <#
        Check (h): the crash-safety property of the camera-list writer. The new
        content is written to a temp file beside the target and the rename is
        the commit point, so an interrupted write leaves the original intact and
        no reader ever sees a partial file.
    #>
    param([Parameter(Mandatory = $true)][string]$WorkDir)

    $target = Join-Path -Path $WorkDir -ChildPath 'atomic.json'
    $temp = "$target.tmp"
    [System.IO.File]::WriteAllText($target, 'ORIGINAL')

    # Interrupted write: the temp file exists, the rename never happened.
    [System.IO.File]::WriteAllText($temp, 'REPLACEMENT')
    $survived = ([System.IO.File]::ReadAllText($target) -eq 'ORIGINAL')
    $sameDirectory = ([System.IO.Path]::GetDirectoryName($temp) -eq [System.IO.Path]::GetDirectoryName($target))

    # Completing it is one rename.
    Move-CamFileAtomic -TempPath $temp -Path $target
    $committed = ([System.IO.File]::ReadAllText($target) -eq 'REPLACEMENT')
    $tempGone = (-not (Test-Path -LiteralPath $temp))

    $ok = ($survived -and $sameDirectory -and $committed -and $tempGone)
    $status = 'PASS'
    if (-not $ok) { $status = 'FAIL' }
    Add-CamTestResult -Check 'atomic write survives an interrupted rewrite' -Status $status `
        -Detail ("interrupted write left the original intact={0}, temp beside the target={1}, rename committed={2}, no temp left={3}" -f `
            $survived, $sameDirectory, $committed, $tempGone)

    Remove-Item -LiteralPath $target -Force -ErrorAction SilentlyContinue
    return $ok
}


function Test-CamPruneGuards {
    <#
        Check (i): nothing outside the archive root can ever be a prune
        candidate. These are the cases that matter - a traversal out of a camera
        directory, a sibling directory whose name merely starts with the root's,
        and an absolute path somewhere else entirely.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $root = [string]$Config.RootPath
    $cases = @(
        @{ Path = (Join-Path -Path $root -ChildPath 'Front_Entrance\seg.mkv'); Expect = $true; Label = 'a segment in a camera directory' },
        @{ Path = (Join-Path -Path $root -ChildPath 'Front_Entrance\..\..\escaped.mkv'); Expect = $false; Label = 'a traversal out of the archive' },
        @{ Path = ($root.TrimEnd('\') + '_old\seg.mkv'); Expect = $false; Label = 'a sibling directory sharing the prefix' },
        @{ Path = 'C:\Windows\System32\drivers\etc\hosts'; Expect = $false; Label = 'an absolute path elsewhere' },
        @{ Path = $root; Expect = $false; Label = 'the archive root itself' }
    )

    $ok = $true
    foreach ($case in $cases) {
        $actual = Test-CamPathInside -Root $root -Path $case.Path
        if ($actual -ne $case.Expect) {
            Add-CamTestResult -Check "prune guard: $($case.Label)" -Status 'FAIL' `
                -Detail ("expected inside={0}, got {1} for {2}" -f $case.Expect, $actual, $case.Path)
            $ok = $false
        }
        else {
            $verdict = 'refused'
            if ($case.Expect) { $verdict = 'allowed' }
            Add-CamTestResult -Check "prune guard: $($case.Label)" -Status 'PASS' -Detail $verdict
        }
    }
    return $ok
}


function Show-CamPruneDryRun {
    <#
        Check (j): what the pruner would do right now, without deleting
        anything. Shows the watermark it works to, how many segments are
        candidates, and which file in each camera directory is protected as the
        one being written.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$WorkDir
    )

    $log = Join-Path -Path $WorkDir -ChildPath 'prune-dryrun.log'
    $result = Invoke-CamPrune -Config $Config -SupervisorLog $log -DryRun

    Add-CamTestResult -Check 'prune watermark' -Status 'INFO' `
        -Detail ("free {0} GB, reserve {1} GB, deletes until {2} GB" -f `
            (Format-CamNumber -Value $result.FreeGBBefore), (Format-CamNumber -Value $result.ReserveGB), (Format-CamNumber -Value $result.TargetGB))
    Add-CamTestResult -Check 'prune candidates' -Status 'INFO' `
        -Detail ("{0} deletable segment(s), {1} protected as in-progress, {2} refused by the path guards" -f `
            $result.Candidates, @($result.Protected).Count, @($result.Rejected).Count)

    $status = 'PASS'
    $detail = 'free space is above the reserve, so nothing would be deleted'
    if ($result.Deleted -gt 0) {
        $detail = "would delete {0} file(s), freeing {1} GB" -f $result.Deleted, (Format-CamNumber -Value ($result.FreedBytes / 1GB) -Format '0.000')
    }
    if ($result.Blocked) {
        $status = 'FAIL'
        $detail = $result.Message
    }
    Add-CamTestResult -Check 'prune dry run' -Status $status -Detail $detail

    foreach ($rejected in @($result.Rejected)) {
        Add-CamTestResult -Check '  guard refused' -Status 'INFO' -Detail $rejected
    }

    return (-not $result.Blocked)
}


function Test-CamStatusSurface {
    <#
        Check (k): the status file the health strip and -Status depend on. Every
        field either exists or the check names the one that is missing, so a
        change to the writer cannot quietly drop a field the readers need.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $path = Get-CamStatusFilePath -Config $Config
    $snapshot = Get-CamStatusSnapshot -Config $Config
    $state = Get-CamSupervisorState -Config $Config -Snapshot $snapshot

    if (-not $snapshot) {
        Add-CamTestResult -Check 'status.json' -Status 'INFO' `
            -Detail ("not written yet at {0}; -Status reports that rather than showing figures" -f $path)
        return $true
    }

    $missing = @()
    foreach ($field in @('Schema', 'State', 'SupervisorPid', 'StartedAt', 'UpdatedAt', 'UptimeSeconds', 'RootPath', 'Container', 'SegmentSeconds', 'Disk', 'Prune', 'Cameras')) {
        if (-not $snapshot.PSObject.Properties[$field]) { $missing += $field }
    }
    foreach ($field in @('FreeGB', 'ReserveGB', 'PruneHysteresisGB', 'ArchiveGB', 'ArchiveFiles', 'OldestSegment', 'NewestSegment', 'MeasuredGBPerDay', 'ProjectedDaysRemaining')) {
        if ($snapshot.Disk -and -not $snapshot.Disk.PSObject.Properties[$field]) { $missing += "Disk.$field" }
    }
    foreach ($field in @('LastRunAt', 'DeletedTotal', 'FreedGBTotal', 'Blocked', 'LastMessage')) {
        if ($snapshot.Prune -and -not $snapshot.Prune.PSObject.Properties[$field]) { $missing += "Prune.$field" }
    }
    $firstCamera = @($snapshot.Cameras) | Select-Object -First 1
    if ($firstCamera) {
        foreach ($field in @('Name', 'Record', 'Pid', 'Alive', 'CurrentSegment', 'CurrentSegmentBytes', 'SecondsSinceLastClosedSegment', 'RestartsLastHour', 'LastError')) {
            if (-not $firstCamera.PSObject.Properties[$field]) { $missing += "Cameras[].$field" }
        }
    }

    if ($missing.Count -gt 0) {
        Add-CamTestResult -Check 'status.json fields' -Status 'FAIL' -Detail ("missing: " + ($missing -join ', '))
        return $false
    }
    Add-CamTestResult -Check 'status.json fields' -Status 'PASS' -Detail 'every documented field is present'

    $running = 'no'
    if ($state.Running) { $running = 'yes' }
    Add-CamTestResult -Check 'supervisor running' -Status 'INFO' -Detail ("{0} - {1}" -f $running, $state.Reason)
    return $true
}


function Show-CamArchiveProjection {
    <#
        Disk projection measured from the archive on disk rather than from a
        fresh recording, so it needs no camera.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $archive = Get-CamArchiveScan -Root $Config.RootPath
    $freeGB = Get-CamFreeSpaceGB -Path $Config.RootPath
    $usableGB = $freeGB - [double]$Config.ReserveGB

    Add-CamTestResult -Check 'archive on disk' -Status 'INFO' `
        -Detail ("{0} file(s), {1} GB, oldest {2}, newest {3}" -f $archive.Files, (Format-CamNumber -Value ($archive.Bytes / 1GB)), `
            $(if ($archive.Oldest) { $archive.Oldest.ToString('yyyy-MM-dd HH:mm') } else { 'none' }), `
            $(if ($archive.Newest) { $archive.Newest.ToString('yyyy-MM-dd HH:mm') } else { 'none' }))

    if (-not $archive.GBPerDay) {
        Add-CamTestResult -Check 'projection from the archive' -Status 'INFO' `
            -Detail ("not enough history to measure a rate yet; {0} GB free - {1} GB reserve = {2} GB usable" -f `
                (Format-CamNumber -Value $freeGB), (Format-CamNumber -Value ([double]$Config.ReserveGB) -Format '0'), (Format-CamNumber -Value $usableGB))
        return
    }

    if ($usableGB -lt 0) {
        Add-CamTestResult -Check 'projection from the archive' -Status 'FAIL' `
            -Detail ("free space is already {0} GB below the {1} GB reserve, so the pruner has nothing left to give; lower ReserveGB or move RootPath" -f `
                (Format-CamNumber -Value ([math]::Abs($usableGB))), (Format-CamNumber -Value ([double]$Config.ReserveGB)))
        return
    }

    $days = 0
    if ([double]$archive.GBPerDay -gt 0) { $days = $usableGB / [double]$archive.GBPerDay }
    Add-CamTestResult -Check 'projection from the archive' -Status 'INFO' `
        -Detail ("{0} GB/day measured across the archive; {1} GB usable = {2} day(s) before the reserve is reached" -f `
            (Format-CamNumber -Value ([double]$archive.GBPerDay)), (Format-CamNumber -Value $usableGB), (Format-CamNumber -Value $days -Format '0.0'))
}


function Invoke-CamSelfTest {
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][int]$MaxConcurrency,
        # Skips every check that opens an RTSP session. The camera checks are
        # the only ones that need a camera; the control, status and retention
        # checks do not, and are worth running at any time.
        [switch]$Offline
    )

    Write-CamHost -Message ''
    Write-CamHost -Message '=== cam.recorder self-test ===' -Colour 'Cyan'
    Write-CamHost -Message ("PowerShell {0} | archive root {1} | container {2}" -f $PSVersionTable.PSVersion, $Config.RootPath, $Config.Container)
    if ($Offline) {
        Write-CamHost -Message 'OFFLINE MODE: no camera is contacted, so (b) streams, (c) concurrency and (d) crash safety are skipped.' -Colour 'Yellow'
    }
    Write-CamHost -Message ''

    $credential = Get-CamCredential
    $cameras = Get-CamResolvedCameras -Config $Config -Credential $credential
    $enabledCount = @($cameras | Where-Object { $_.Record }).Count

    $workDir = Join-Path -Path $Config.RootPath -ChildPath '_selftest'
    if (Test-Path -LiteralPath $workDir) { Remove-Item -LiteralPath $workDir -Recurse -Force }
    [void](New-Item -ItemType Directory -Path $workDir -Force)

    try {
        Write-CamHost -Message '(a) tooling' -Colour 'Cyan'
        if (-not (Test-CamTooling -Config $Config)) { return }

        if (-not $Offline) {
            Write-CamHost -Message '(b) camera streams' -Colour 'Cyan'
            $reachable = Test-CamStreams -Config $Config -Cameras $cameras

            if (@($reachable).Count -eq 0) {
                Add-CamTestResult -Check 'camera reachability' -Status 'FAIL' -Detail 'no camera answered, skipping the remaining checks'
                return
            }

            Write-CamHost -Message '(c) concurrent RTSP sessions per camera' -Colour 'Cyan'
            [void](Measure-CamConcurrency -Config $Config -Cameras $reachable -WorkDir $workDir -MaxConcurrency $MaxConcurrency)

            Write-CamHost -Message '(d) crash safety' -Colour 'Cyan'
            $bytesPerSecond = Test-CamCrashSafety -Config $Config -Camera $reachable[0] -WorkDir $workDir

            Write-CamHost -Message '(e) disk projection' -Colour 'Cyan'
            Show-CamDiskProjection -Config $Config -BytesPerSecond $bytesPerSecond `
                -EnabledCount $enabledCount -TotalCount @($cameras).Count
        }
        else {
            Write-CamHost -Message '(e) disk projection from the archive' -Colour 'Cyan'
            Show-CamArchiveProjection -Config $Config
        }

        Write-CamHost -Message '(f) camera list' -Colour 'Cyan'
        [void](Test-CamListIntegrity -Config $Config)

        Write-CamHost -Message '(g) Record flag round trip (on a copy)' -Colour 'Cyan'
        [void](Test-CamRecordRoundTrip -Config $Config -WorkDir $workDir)

        Write-CamHost -Message '(h) atomic camera-list write' -Colour 'Cyan'
        [void](Test-CamAtomicWrite -WorkDir $workDir)

        Write-CamHost -Message '(i) pruner path guards' -Colour 'Cyan'
        [void](Test-CamPruneGuards -Config $Config)

        Write-CamHost -Message '(j) pruner dry run' -Colour 'Cyan'
        [void](Show-CamPruneDryRun -Config $Config -WorkDir $workDir)

        Write-CamHost -Message '(k) status file' -Colour 'Cyan'
        [void](Test-CamStatusSurface -Config $Config)
    }
    finally {
        if (Test-Path -LiteralPath $workDir) {
            Remove-Item -LiteralPath $workDir -Recurse -Force -ErrorAction SilentlyContinue
        }
        $leftovers = @(Get-ChildItem -LiteralPath $Config.RootPath -Filter '_selftest' -ErrorAction SilentlyContinue)
        Write-CamHost -Message ''
        Write-CamHost -Message ("cleanup: _selftest removed = {0}" -f ($leftovers.Count -eq 0))
    }

    $passed = @($script:CamTestResults | Where-Object { $_.Status -eq 'PASS' }).Count
    $failed = @($script:CamTestResults | Where-Object { $_.Status -eq 'FAIL' }).Count

    Write-CamHost -Message ''
    Write-CamHost -Message '=== summary ===' -Colour 'Cyan'
    Write-CamHost -Message ("{0} passed, {1} failed" -f $passed, $failed) -Colour $(if ($failed -gt 0) { 'Red' } else { 'Green' })
    foreach ($result in @($script:CamTestResults | Where-Object { $_.Status -eq 'FAIL' })) {
        Write-CamHost -Message ("  FAIL  {0}: {1}" -f $result.Check.Trim(), $result.Detail) -Colour 'Red'
    }
    Write-CamHost -Message ''
    if ($failed -gt 0) { exit 1 }
}


# ===========================================================================
# Supervisor
# ===========================================================================

function New-CamChildEntry {
    param(
        [Parameter(Mandatory = $true)]$Camera,
        [Parameter(Mandatory = $true)][string]$CameraDir,
        [Parameter(Mandatory = $true)][string]$LogPath,
        [Parameter(Mandatory = $true)][string]$Pattern
    )

    return @{
        Name         = $Camera.Name
        SafeName     = $Camera.SafeName
        # Kept so a reconcile can tell a changed stream from an unchanged one
        # without comparing URLs, which carry the password.
        IP           = $Camera.IP
        Quality      = $Camera.Quality
        Url          = $Camera.Url
        Dir          = $CameraDir
        LogPath      = $LogPath
        Pattern      = $Pattern
        Process      = $null
        ReadBuffer   = $null
        ReadTask     = $null
        Decoder      = $null
        TextBuffer   = ''
        LogSeen      = @{}
        StartedAt    = [DateTime]::MinValue
        Backoff      = 0
        NextStartAt  = [DateTime]::Now
        RestartCount = 0
        # The pid of the most recent child, kept after it exits so -Stop can
        # tell an orphaned ffmpeg from a child that shut down with its parent.
        LastPid      = $null
        # Restart timestamps, so status.json can report restarts in the last hour
        # rather than only a total that never decays.
        Restarts     = New-Object System.Collections.ArrayList
        LastError    = ''
        LastErrorAt  = [DateTime]::MinValue
        Scan         = $null
    }
}


function Start-CamChild {
    <#
        Launches one ffmpeg child with stderr on a live pipe, read asynchronously
        so the supervisor never blocks and ffmpeg never stalls on a full pipe.

        Start-Process -RedirectStandardError is not used here: it takes a file
        path and truncates, which would lose the log on every restart and would
        also write unredacted credentials to disk.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$ContainerProfile,
        [Parameter(Mandatory = $true)][string]$SupervisorLog
    )

    $arguments = Get-CamFfmpegArgs -Url $Entry.Url -OutputPattern $Entry.Pattern -Config $Config -ContainerProfile $ContainerProfile

    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $Config.FfmpegPath
    $startInfo.Arguments = Get-CamCommandLine -Arguments $arguments
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardError = $true

    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $startInfo
    [void]$process.Start()
    [void]$process.Handle

    $Entry.Process = $process
    $Entry.LastPid = $process.Id
    $Entry.StartedAt = [DateTime]::Now
    $Entry.TextBuffer = ''
    $Entry.Decoder = [System.Text.Encoding]::UTF8.GetDecoder()
    $Entry.ReadBuffer = New-Object byte[] 8192
    $Entry.ReadTask = $process.StandardError.BaseStream.ReadAsync($Entry.ReadBuffer, 0, $Entry.ReadBuffer.Length)

    Write-CamLog -Path $SupervisorLog -Message ("start {0} pid {1}: ffmpeg {2}" -f $Entry.Name, $process.Id, $startInfo.Arguments) -MaxBytes $Config.LogMaxBytes
    Write-CamHost -Message ("start {0} pid {1}" -f $Entry.Name, $process.Id) -Colour 'Green'
}


function Write-CamChildLine {
    <#
        Writes one ffmpeg stderr line to the camera's log, collapsing repeats.

        These cameras send non-monotonic RTSP timestamps, so ffmpeg emits
        thousands of identical "Non-monotonic DTS" warnings per minute at
        -loglevel warning. Measured: 2457 of 2459 lines in a 3.5 minute
        recording were two such messages, which rotated the log once a minute
        and would have thrown away every real error with it. The recordings
        themselves are unaffected (900 frames in a 60 s segment at 15 fps).

        Lowering ffmpeg's loglevel would also lose the warnings that matter -
        packet loss, buffer overruns - so instead each distinct message shape is
        logged at most once per LogRepeatSuppressSeconds, carrying the count of
        what was suppressed.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Line,
        [Parameter(Mandatory = $true)][DateTime]$Now
    )

    # Message shape: numbers and pointer values carry no identity.
    $signature = [regex]::Replace($Line, '0x?[0-9a-fA-F]{6,}|\d+', '#')

    # A camera cannot produce unbounded distinct shapes; this only guards against
    # an unforeseen one growing the table for the life of the process.
    if ($Entry.LogSeen.Count -gt 200) { $Entry.LogSeen.Clear() }

    $state = $Entry.LogSeen[$signature]
    if (-not $state) {
        $state = @{ Count = 0; LastLogged = [DateTime]::MinValue }
        $Entry.LogSeen[$signature] = $state
    }

    if (($Now - $state.LastLogged).TotalSeconds -lt [double]$Config.LogRepeatSuppressSeconds) {
        $state.Count = $state.Count + 1
        return
    }

    $message = $Line
    if ($state.Count -gt 0) {
        $message = "$Line [+$($state.Count) more of this message suppressed]"
    }
    Write-CamLog -Path $Entry.LogPath -Message $message -MaxBytes $Config.LogMaxBytes
    $state.LastLogged = $Now
    $state.Count = 0
}


function Read-CamChildStderr {
    <#
        Drains whatever stderr has produced since the last tick, without
        blocking, and writes every complete line to the camera's own log through
        the redaction helper. ffmpeg prints the full RTSP URL - password included
        - on a connection error, so redaction here is not optional.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        # Wait briefly for the outstanding read instead of returning at once, so
        # the last lines a dying ffmpeg wrote still reach the log.
        [switch]$Final
    )

    $now = [DateTime]::Now

    while ($Entry.ReadTask) {
        if (-not $Entry.ReadTask.IsCompleted) {
            if (-not $Final) { break }
            try { if (-not $Entry.ReadTask.Wait(2000)) { break } } catch { $Entry.ReadTask = $null; break }
        }

        $count = 0
        try { $count = $Entry.ReadTask.Result }
        catch { $Entry.ReadTask = $null; break }

        if ($count -le 0) { $Entry.ReadTask = $null; break }

        # A persistent decoder, because a multi-byte character can straddle two reads.
        $chars = New-Object char[] $count
        $decoded = $Entry.Decoder.GetChars($Entry.ReadBuffer, 0, $count, $chars, 0)
        $Entry.TextBuffer = $Entry.TextBuffer + [string]::new($chars, 0, $decoded)

        while ($true) {
            $index = $Entry.TextBuffer.IndexOfAny([char[]]@("`r", "`n"))
            if ($index -lt 0) { break }
            $line = $Entry.TextBuffer.Substring(0, $index)
            $Entry.TextBuffer = $Entry.TextBuffer.Substring($index + 1)
            if ($line.Trim().Length -gt 0) {
                Write-CamChildLine -Entry $Entry -Config $Config -Line $line -Now $now
            }
        }

        $Entry.ReadTask = $Entry.Process.StandardError.BaseStream.ReadAsync($Entry.ReadBuffer, 0, $Entry.ReadBuffer.Length)
    }
}


function Test-CamChildStalled {
    <#
        ffmpeg can keep an RTSP session open and alive while moving no data, so a
        liveness check alone is not enough. The signal is the age of the newest
        segment write, measured from the later of process start and the last
        write, against StallFactor x SegmentSeconds.

        Newest-closed-segment age rather than in-progress file size, because NTFS
        does not refresh the directory entry of a file while a handle is open.
        The caller also counts the creation of the open segment as progress, for
        the same reason: a creation time is stamped once and never goes stale.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][DateTime]$Now,
        # Newest segment write time from the shared per-camera scan. Passing it
        # in avoids enumerating a directory of several hundred segments once per
        # second per camera for the life of the process; the scan the status
        # writer already does every few seconds is current enough for a
        # threshold measured in whole segment periods.
        $NewestWriteTime
    )

    $threshold = [double]$Config.SegmentSeconds * [double]$Config.StallFactor
    $reference = $Entry.StartedAt

    if ($null -ne $NewestWriteTime) {
        $supplied = [DateTime]$NewestWriteTime
        if ($supplied -gt $reference) { $reference = $supplied }
    }
    elseif (Test-Path -LiteralPath $Entry.Dir) {
        $newest = Get-ChildItem -LiteralPath $Entry.Dir -File -ErrorAction SilentlyContinue |
            Sort-Object -Property LastWriteTime -Descending | Select-Object -First 1
        if ($newest -and $newest.LastWriteTime -gt $reference) { $reference = $newest.LastWriteTime }
    }

    $age = ($Now - $reference).TotalSeconds
    return @{
        Stalled    = ($age -gt $threshold)
        AgeSeconds = $age
        Threshold  = $threshold
    }
}


function Remove-CamEmptySegment {
    <#
        Removes a 0-byte segment left behind when a child was stopped in the
        second after a segment rolled over.

        The matroska header is still in ffmpeg's write buffer at that point, so
        the file on disk holds no footage and no reader can open it. Anything
        with content starts with a complete header and stays readable through a
        hard kill, which is why only an exactly empty file qualifies.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$SupervisorLog
    )

    if (-not (Test-Path -LiteralPath $Entry.Dir)) { return }

    $empty = @(Get-ChildItem -LiteralPath $Entry.Dir -File -Force -ErrorAction SilentlyContinue |
        Where-Object { $_.Length -eq 0 })

    foreach ($file in $empty) {
        if (-not (Test-CamPathInside -Root ([string]$Config.RootPath) -Path $file.FullName)) { continue }
        try {
            Remove-Item -LiteralPath $file.FullName -Force
            Write-CamLog -Path $SupervisorLog -MaxBytes $Config.LogMaxBytes `
                -Message ("removed empty segment {0}: the child was stopped before its header reached disk" -f $file.Name)
        }
        catch {
            Write-CamLog -Path $SupervisorLog -MaxBytes $Config.LogMaxBytes `
                -Message ("could not remove empty segment {0}: {1}" -f $file.Name, $_.Exception.Message)
        }
    }
}


function Stop-CamChild {
    <#
        Hard-kills a child. A hard kill is acceptable because the container is
        crash-tolerant - the self-test proves the interrupted segment stays
        readable - and ffmpeg offers no graceful stop once -nostdin is set.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$SupervisorLog,
        [Parameter(Mandatory = $true)][string]$Reason
    )

    $process = $Entry.Process
    if (-not $process) { return }

    if (-not $process.HasExited) {
        Write-CamLog -Path $SupervisorLog -Message ("kill {0} pid {1}: {2}" -f $Entry.Name, $process.Id, $Reason) -MaxBytes $Config.LogMaxBytes
        Write-CamHost -Message ("kill {0} pid {1}: {2}" -f $Entry.Name, $process.Id, $Reason) -Colour 'Yellow'
        try { $process.Kill() } catch { }
        [void]$process.WaitForExit(10000)
    }

    Read-CamChildStderr -Entry $Entry -Config $Config -Final
    try { $process.Dispose() } catch { }

    $Entry.Process = $null
    $Entry.ReadTask = $null

    Remove-CamEmptySegment -Entry $Entry -Config $Config -SupervisorLog $SupervisorLog
}


function Set-CamChildBackoff {
    <#
        3 s doubling to a 60 s cap, reset once a child has stayed up for
        HealthyResetSeconds. ffmpeg has no RTSP reconnect of its own, so this is
        the reconnection mechanism.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][double]$RanForSeconds
    )

    $start = [int]$Config.BackoffStartSeconds
    $cap = [int]$Config.BackoffMaxSeconds

    if ($RanForSeconds -ge [double]$Config.HealthyResetSeconds) {
        $next = $start
    }
    else {
        $next = $Entry.Backoff * 2
        if ($next -lt $start) { $next = $start }
        if ($next -gt $cap) { $next = $cap }
    }

    $Entry.Backoff = $next
    $Entry.NextStartAt = [DateTime]::Now.AddSeconds($next)
    return $next
}


# ===========================================================================
# Archive scan, status file, pruner
# ===========================================================================

function Get-CamLiveFileLength {
    <#
        The true current size of a file another process is writing.

        NTFS does not refresh a directory entry while a handle is open, so
        FileInfo.Length reads stale for the segment ffmpeg is writing right now.
        Opening the file and asking the stream gives the real figure. Returns -1
        when the open is refused, which leaves the caller on the stale value.
    #>
    param([Parameter(Mandatory = $true)][string]$Path)

    try {
        $share = [System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete
        $stream = New-Object System.IO.FileStream($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, $share)
        try { return $stream.Length }
        finally { $stream.Dispose() }
    }
    catch { return -1 }
}


function Get-CamDirScan {
    <#
        One camera directory's segment picture, shared by the status writer and
        the stall watchdog so the directory is enumerated once per cycle rather
        than once per consumer.

        The in-progress segment is the most recently CREATED file, not the one
        with the newest write time: a creation time is stamped once and never
        goes stale, while the write time of an open file does. Everything that
        has to be certain which file ffmpeg holds - the status view and the
        pruner's "never delete the newest file" rule - depends on that.
    #>
    param([Parameter(Mandatory = $true)][string]$Path)

    $scan = @{
        Path              = $Path
        ScannedAt         = [DateTime]::Now
        Files             = 0
        Bytes             = 0
        Current           = $null
        CurrentCreated    = $null
        CurrentBytes      = 0
        CurrentBytesLive  = $false
        NewestClosedWrite = $null
        OldestWrite       = $null
        NewestWrite       = $null
    }

    if (-not (Test-Path -LiteralPath $Path)) { return $scan }

    $files = @(Get-ChildItem -LiteralPath $Path -File -Force -ErrorAction SilentlyContinue)
    if ($files.Count -eq 0) { return $scan }

    $current = $null
    foreach ($file in $files) {
        if ($null -eq $current -or $file.CreationTimeUtc -gt $current.CreationTimeUtc) { $current = $file }
    }

    $bytes = 0
    foreach ($file in $files) {
        $length = $file.Length
        if ($current -and $file.FullName -eq $current.FullName) {
            $live = Get-CamLiveFileLength -Path $file.FullName
            if ($live -ge 0) {
                $length = $live
                $scan.CurrentBytesLive = $true
            }
            $scan.CurrentBytes = $length
        }
        else {
            if ($null -eq $scan.NewestClosedWrite -or $file.LastWriteTime -gt $scan.NewestClosedWrite) {
                $scan.NewestClosedWrite = $file.LastWriteTime
            }
        }
        $bytes += $length
        if ($null -eq $scan.OldestWrite -or $file.LastWriteTime -lt $scan.OldestWrite) { $scan.OldestWrite = $file.LastWriteTime }
        if ($null -eq $scan.NewestWrite -or $file.LastWriteTime -gt $scan.NewestWrite) { $scan.NewestWrite = $file.LastWriteTime }
    }

    $scan.Files = $files.Count
    $scan.Bytes = $bytes
    if ($current) {
        $scan.Current = $current.Name
        $scan.CurrentCreated = $current.CreationTime
    }
    return $scan
}


function Get-CamSegmentDirectories {
    <#
        The archive's camera directories: every immediate subdirectory of the
        root whose name does not start with an underscore. Underscore names are
        reserved for the recorder's own folders (_logs, _state, _selftest), and
        a directory left behind by a camera that was removed from cameras.json
        still counts - otherwise its segments would never be pruned.

        A reparse point is never descended into. Its path looks like it is
        inside the archive while its content is somewhere else entirely, so
        deleting through one would escape the root no matter how carefully the
        path itself is checked.
    #>
    param([Parameter(Mandatory = $true)][string]$Root)

    $result = @{ Directories = @(); Skipped = @() }
    if (-not (Test-Path -LiteralPath $Root)) { return $result }

    foreach ($dir in @(Get-ChildItem -LiteralPath $Root -Directory -Force -ErrorAction SilentlyContinue)) {
        if ($dir.Name.StartsWith('_')) { continue }
        if (($dir.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            $result.Skipped += ("{0} (reparse point: its content is outside the archive)" -f $dir.FullName)
            continue
        }
        if (-not (Test-CamPathInside -Root $Root -Path $dir.FullName)) {
            $result.Skipped += ("{0} (resolves outside the archive root)" -f $dir.FullName)
            continue
        }
        $result.Directories += $dir
    }

    return $result
}


function Get-CamArchiveScan {
    <#
        Archive-wide figures: size, file count, the oldest and newest segment,
        and the GB/day the archive is actually growing at. The rate comes from
        the archive itself - total bytes over the span its segments cover - so
        the projection is measured rather than assumed, and it follows whatever
        the cameras are really producing.
    #>
    param([Parameter(Mandatory = $true)][string]$Root)

    $scan = @{
        ScannedAt   = [DateTime]::Now
        Files       = 0
        Bytes       = 0
        Oldest      = $null
        Newest      = $null
        GBPerDay    = $null
        Dirs        = @{}
        Skipped     = @()
    }

    $dirs = Get-CamSegmentDirectories -Root $Root
    $scan.Skipped = $dirs.Skipped

    foreach ($dir in $dirs.Directories) {
        $dirScan = Get-CamDirScan -Path $dir.FullName
        $scan.Dirs[$dir.Name] = $dirScan
        $scan.Files += $dirScan.Files
        $scan.Bytes += $dirScan.Bytes
        if ($dirScan.OldestWrite -and ($null -eq $scan.Oldest -or $dirScan.OldestWrite -lt $scan.Oldest)) { $scan.Oldest = $dirScan.OldestWrite }
        if ($dirScan.NewestWrite -and ($null -eq $scan.Newest -or $dirScan.NewestWrite -gt $scan.Newest)) { $scan.Newest = $dirScan.NewestWrite }
    }

    if ($scan.Oldest -and $scan.Newest) {
        $spanSeconds = ($scan.Newest - $scan.Oldest).TotalSeconds
        # Below ten minutes of history the rate says more about when recording
        # started than about how fast the disk fills, so report nothing.
        if ($spanSeconds -ge 600) {
            $scan.GBPerDay = [math]::Round((($scan.Bytes / 1GB) / ($spanSeconds / 86400)), 2)
        }
    }

    return $scan
}


function Write-CamStatusFile {
    <#
        Writes _state\status.json. This is the supervisor's only outward
        channel: -Status and the health strip read the file, so neither needs
        IPC and both work from any shell with no supervisor attached.

        Written through the same temp-then-rename path as cameras.json, so a
        reader never sees a half-written file, and the whole serialised document
        passes through redaction before it is written - belt and braces over the
        per-field redaction, because a status file is read by eye and pasted
        into reports.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Cameras,
        [Parameter(Mandatory = $true)]$Children,
        [Parameter(Mandatory = $true)][DateTime]$StartedAt,
        [Parameter(Mandatory = $true)][ValidateSet('running', 'stopped')][string]$State,
        $Archive,
        $PruneState
    )

    $now = [DateTime]::Now
    $freeGB = Get-CamFreeSpaceGB -Path $Config.RootPath
    $reserveGB = [double]$Config.ReserveGB

    $archiveGB = $null
    $archiveBytes = $null
    $archiveFiles = $null
    $oldest = $null
    $newest = $null
    $gbPerDay = $null
    $scannedAt = $null
    if ($Archive) {
        $archiveGB = [math]::Round($Archive.Bytes / 1GB, 2)
        $archiveBytes = $Archive.Bytes
        $archiveFiles = $Archive.Files
        $oldest = $Archive.Oldest
        $newest = $Archive.Newest
        $gbPerDay = $Archive.GBPerDay
        $scannedAt = $Archive.ScannedAt
    }

    $daysRemaining = $null
    if ($gbPerDay -and [double]$gbPerDay -gt 0) {
        $usableGB = $freeGB - $reserveGB
        # Already past the reserve means no days left, not a negative count.
        if ($usableGB -lt 0) { $usableGB = 0 }
        $daysRemaining = [math]::Round(($usableGB / [double]$gbPerDay), 2)
    }

    $rows = @()
    foreach ($camera in $Cameras) {
        $safe = [string]$camera.SafeName
        $entry = $null
        if ($safe -and $Children.ContainsKey($safe)) { $entry = $Children[$safe] }

        $scan = $null
        if ($entry -and $entry.Scan) { $scan = $entry.Scan }
        elseif ($Archive -and $safe -and $Archive.Dirs.ContainsKey($safe)) { $scan = $Archive.Dirs[$safe] }

        $processId = $null
        $lastPid = $null
        $alive = $false
        $startedAtCamera = $null
        $nextStartAt = $null
        $restartsTotal = 0
        $restartsLastHour = 0
        $lastError = ''
        $lastErrorAt = $null

        if ($entry) {
            $restartsTotal = $entry.RestartCount
            $cutoff = $now.AddHours(-1)
            foreach ($stamp in @($entry.Restarts)) { if ($stamp -gt $cutoff) { $restartsLastHour++ } }
            $lastPid = $entry.LastPid
            $lastError = [string]$entry.LastError
            $lastErrorAt = Format-CamTimestamp -Value $entry.LastErrorAt
            if ($entry.StartedAt -ne [DateTime]::MinValue) { $startedAtCamera = Format-CamTimestamp -Value $entry.StartedAt }
            if ($entry.Process) {
                $processId = $entry.Process.Id
                $alive = (-not $entry.Process.HasExited)
            }
            else {
                $nextStartAt = Format-CamTimestamp -Value $entry.NextStartAt
            }
        }

        $currentSegment = $null
        $currentBytes = $null
        $secondsSinceClosed = $null
        if ($scan -and $scan.Current) {
            $currentSegment = $scan.Current
            $currentBytes = $scan.CurrentBytes
        }
        if ($scan) {
            if ($scan.NewestClosedWrite) {
                $secondsSinceClosed = [math]::Round(($now - $scan.NewestClosedWrite).TotalSeconds, 1)
            }
        }

        $rows += [pscustomobject]@{
            Name                          = Get-CamRedacted -Text ([string]$camera.Name)
            SafeName                      = $safe
            Record                        = [bool]$camera.Record
            Pid                           = $processId
            LastPid                       = $lastPid
            Alive                         = $alive
            CurrentSegment                = $currentSegment
            CurrentSegmentBytes           = $currentBytes
            SecondsSinceLastClosedSegment = $secondsSinceClosed
            RestartsLastHour              = $restartsLastHour
            RestartsTotal                 = $restartsTotal
            StartedAt                     = $startedAtCamera
            NextStartAt                   = $nextStartAt
            LastError                     = Get-CamRedacted -Text $lastError
            LastErrorAt                   = $lastErrorAt
            ArchiveFiles                  = $(if ($scan) { $scan.Files } else { $null })
            ArchiveBytes                  = $(if ($scan) { $scan.Bytes } else { $null })
            ConfigError                   = Get-CamRedacted -Text ([string]$camera.Error)
        }
    }

    $prune = [pscustomobject]@{
        LastRunAt    = $null
        DeletedTotal = 0
        FreedGBTotal = 0
        Blocked      = $false
        LastMessage  = ''
    }
    if ($PruneState) {
        $prune = [pscustomobject]@{
            LastRunAt    = Format-CamTimestamp -Value $PruneState.LastRunAt
            DeletedTotal = $PruneState.DeletedTotal
            FreedGBTotal = [math]::Round($PruneState.FreedBytes / 1GB, 3)
            Blocked      = [bool]$PruneState.Blocked
            LastMessage  = Get-CamRedacted -Text ([string]$PruneState.LastMessage)
        }
    }

    $document = [pscustomobject]@{
        Schema        = 1
        State         = $State
        SupervisorPid = $PID
        StartedAt     = Format-CamTimestamp -Value $StartedAt
        UpdatedAt     = Format-CamTimestamp -Value $now
        UptimeSeconds = [math]::Round(($now - $StartedAt).TotalSeconds, 1)
        RootPath      = [string]$Config.RootPath
        Container     = [string]$Config.Container
        SegmentSeconds = [int]$Config.SegmentSeconds
        Disk          = [pscustomobject]@{
            FreeGB                 = $freeGB
            ReserveGB              = $reserveGB
            PruneHysteresisGB      = [double]$Config.PruneHysteresisGB
            ArchiveGB              = $archiveGB
            ArchiveBytes           = $archiveBytes
            ArchiveFiles           = $archiveFiles
            OldestSegment          = Format-CamTimestamp -Value $oldest
            NewestSegment          = Format-CamTimestamp -Value $newest
            MeasuredGBPerDay       = $gbPerDay
            ProjectedDaysRemaining = $daysRemaining
            ScannedAt              = Format-CamTimestamp -Value $scannedAt
        }
        Prune         = $prune
        Cameras       = $rows
    }

    $json = ConvertTo-Json -InputObject $document -Depth 6
    $json = Get-CamRedacted -Text $json

    $dir = Split-Path -Path $Path -Parent
    if ($dir -and -not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }

    $temp = "$Path.tmp"
    [System.IO.File]::WriteAllText($temp, $json + [Environment]::NewLine, (New-Object System.Text.UTF8Encoding($false)))
    Move-CamFileAtomic -TempPath $temp -Path $Path
}


function Invoke-CamPrune {
    <#
        Free-space watermark pruning, oldest segment first across every camera.

        Rules, in the order they bite:
          1. Never the newest file in any camera directory. That is the segment
             ffmpeg holds open, and for a directory with no running child it is
             one file's worth of caution for no real cost.
          2. MaxRetentionDays, when set, deletes by age regardless of free space.
          3. While free space is below ReserveGB, delete globally oldest first
             until it reaches ReserveGB + PruneHysteresisGB. The hysteresis is
             what stops the pruner firing again on the next cycle.
          4. Nothing outside the archive root, ever: every candidate passes
             Test-CamPathInside and no reparse point is followed.
          5. When nothing can be deleted, say so loudly and keep recording. An
             NVR that stops recording because the disk is full has failed twice.

        The loop is bounded by the candidate list, so an undeletable file is
        skipped rather than retried, and an archive that cannot be shrunk logs
        once per cycle instead of spinning.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][string]$SupervisorLog,
        $State,
        [switch]$DryRun
    )

    $root = [string]$Config.RootPath
    $reserveGB = [double]$Config.ReserveGB
    $targetGB = $reserveGB + [double]$Config.PruneHysteresisGB
    $maxBytes = [int]$Config.LogMaxBytes

    $result = @{
        FreeGBBefore = Get-CamFreeSpaceGB -Path $root
        FreeGBAfter  = 0
        ReserveGB    = $reserveGB
        TargetGB     = $targetGB
        Candidates   = 0
        Deleted      = 0
        FreedBytes   = 0
        Failed       = 0
        Blocked      = $false
        Protected    = @()
        Rejected     = @()
        Deletions    = @()
        Message      = ''
    }
    $result.FreeGBAfter = $result.FreeGBBefore

    $dirs = Get-CamSegmentDirectories -Root $root
    foreach ($skipped in $dirs.Skipped) {
        $result.Rejected += $skipped
        Write-CamLog -Path $SupervisorLog -Message ("prune guard: refusing to prune {0}" -f $skipped) -MaxBytes $maxBytes
    }

    $candidates = @()
    foreach ($dir in $dirs.Directories) {
        $files = @(Get-ChildItem -LiteralPath $dir.FullName -File -Force -ErrorAction SilentlyContinue)
        if ($files.Count -eq 0) { continue }

        $current = $null
        foreach ($file in $files) {
            if ($null -eq $current -or $file.CreationTimeUtc -gt $current.CreationTimeUtc) { $current = $file }
        }
        if ($current) { $result.Protected += $current.FullName }

        foreach ($file in $files) {
            if ($current -and $file.FullName -eq $current.FullName) { continue }
            if (($file.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                $result.Rejected += ("{0} (reparse point)" -f $file.FullName)
                Write-CamLog -Path $SupervisorLog -Message ("prune guard: refusing to delete reparse point {0}" -f $file.FullName) -MaxBytes $maxBytes
                continue
            }
            if (-not (Test-CamPathInside -Root $root -Path $file.FullName)) {
                $result.Rejected += ("{0} (outside the archive root)" -f $file.FullName)
                Write-CamLog -Path $SupervisorLog -Message ("prune guard: refusing to delete {0}, which is outside {1}" -f $file.FullName, $root) -MaxBytes $maxBytes
                continue
            }
            $candidates += $file
        }
    }

    # Globally oldest first, so retention is uniform across cameras instead of
    # emptying whichever camera happens to be scanned first.
    $candidates = @($candidates | Sort-Object -Property LastWriteTime)
    $result.Candidates = $candidates.Count

    $deleteOne = {
        param($file, $reason)

        if ($DryRun) {
            $result.Deletions += ("would delete {0} ({1} bytes, {2}, {3})" -f $file.FullName, $file.Length, $file.LastWriteTime.ToString('yyyy-MM-dd HH:mm:ss'), $reason)
            $result.Deleted = $result.Deleted + 1
            $result.FreedBytes = $result.FreedBytes + $file.Length
            return $true
        }

        try {
            $length = $file.Length
            Remove-Item -LiteralPath $file.FullName -Force
            $result.Deleted = $result.Deleted + 1
            $result.FreedBytes = $result.FreedBytes + $length
            $line = "prune: deleted {0} ({1} bytes, written {2}) - {3}" -f `
                $file.FullName, $length, $file.LastWriteTime.ToString('yyyy-MM-dd HH:mm:ss'), $reason
            $result.Deletions += $line
            Write-CamLog -Path $SupervisorLog -Message $line -MaxBytes $maxBytes
            return $true
        }
        catch {
            $result.Failed = $result.Failed + 1
            Write-CamLog -Path $SupervisorLog -Message ("prune: FAILED to delete {0}: {1}" -f $file.FullName, $_.Exception.Message) -MaxBytes $maxBytes
            return $false
        }
    }

    $consumed = @{}

    # (2) age rule
    $retentionDays = $null
    if ($null -ne $Config.MaxRetentionDays -and [string]$Config.MaxRetentionDays -ne '') {
        $retentionDays = [double]$Config.MaxRetentionDays
    }
    if ($retentionDays -and $retentionDays -gt 0) {
        $cutoff = [DateTime]::Now.AddDays(-$retentionDays)
        foreach ($file in $candidates) {
            if ($file.LastWriteTime -ge $cutoff) { continue }
            $consumed[$file.FullName] = $true
            [void](& $deleteOne $file ("older than MaxRetentionDays=$retentionDays"))
        }
    }

    # (3) watermark
    $free = Get-CamFreeSpaceGB -Path $root
    if ($free -lt $reserveGB) {
        $index = 0
        while ($free -lt $targetGB) {
            if ($index -ge $candidates.Count) {
                $result.Blocked = $true
                break
            }
            $file = $candidates[$index]
            $index++
            if ($consumed.ContainsKey($file.FullName)) { continue }

            $deleted = & $deleteOne $file ("free {0} GB below reserve {1} GB, target {2} GB" -f `
                (Format-CamNumber -Value $free), (Format-CamNumber -Value $reserveGB), (Format-CamNumber -Value $targetGB))
            if (-not $deleted) { continue }
            if ($DryRun) {
                # Nothing was actually freed, so re-reading free space would loop
                # forever. Estimate instead.
                $free = $free + ($file.Length / 1GB)
            }
            else {
                $free = Get-CamFreeSpaceGB -Path $root
            }
        }
    }

    $result.FreeGBAfter = Get-CamFreeSpaceGB -Path $root
    if ($DryRun) { $result.FreeGBAfter = $free }

    if ($result.Blocked -and -not $DryRun) {
        $result.Message = "PRUNE BLOCKED: free {0} GB is below the {1} GB reserve and nothing deletable is left ({2} candidate(s), {3} undeletable, {4} protected as in-progress). Recording continues. Lower ReserveGB, move RootPath to a larger volume, or free space by hand." -f `
            (Format-CamNumber -Value $result.FreeGBAfter), (Format-CamNumber -Value $reserveGB), $result.Candidates, $result.Failed, @($result.Protected).Count
        Write-CamLog -Path $SupervisorLog -Message $result.Message -MaxBytes $maxBytes
        Write-CamHost -Message $result.Message -Colour 'Red'
    }
    elseif ($result.Blocked) {
        $result.Message = "would be BLOCKED: free {0} GB is below the {1} GB reserve and only {2} segment(s) could be deleted, {3} protected as in-progress" -f `
            (Format-CamNumber -Value $result.FreeGBAfter), (Format-CamNumber -Value $reserveGB), $result.Candidates, @($result.Protected).Count
    }
    elseif ($result.Deleted -gt 0) {
        $result.Message = "prune: {0} file(s) deleted, {1} GB freed, free {2} -> {3} GB (reserve {4}, target {5})" -f `
            $result.Deleted, (Format-CamNumber -Value ($result.FreedBytes / 1GB) -Format '0.000'), `
            (Format-CamNumber -Value $result.FreeGBBefore), (Format-CamNumber -Value $result.FreeGBAfter), `
            (Format-CamNumber -Value $reserveGB), (Format-CamNumber -Value $targetGB)
        if (-not $DryRun) {
            Write-CamLog -Path $SupervisorLog -Message $result.Message -MaxBytes $maxBytes
            Write-CamHost -Message $result.Message -Colour 'Yellow'
        }
    }

    if ($State -and -not $DryRun) {
        $State.LastRunAt = [DateTime]::Now
        $State.DeletedTotal = $State.DeletedTotal + $result.Deleted
        $State.FreedBytes = $State.FreedBytes + $result.FreedBytes
        $State.Blocked = $result.Blocked
        if ($result.Message) { $State.LastMessage = $result.Message }
    }

    return $result
}


# ===========================================================================
# CLI: -Status, -Stop
# ===========================================================================

function Format-CamAgeText {
    param($Seconds)

    if ($null -eq $Seconds) { return '-' }
    $value = [double]$Seconds
    if ($value -lt 120) { return ("{0}s" -f [int]$value) }
    if ($value -lt 7200) { return ("{0}m" -f [int]($value / 60)) }
    return ("{0}h" -f [int]($value / 3600))
}


function Show-CamStatus {
    <#
        Renders status.json. Says plainly when no supervisor is behind it rather
        than presenting a stale snapshot as the current state - a health view
        that cannot be told apart from a working one is worse than none.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $snapshot = Get-CamStatusSnapshot -Config $Config
    $state = Get-CamSupervisorState -Config $Config -Snapshot $snapshot

    Write-CamHost -Message ''
    Write-CamHost -Message '=== cam.recorder status ===' -Colour 'Cyan'

    if (-not $snapshot) {
        Write-CamHost -Message ("NO STATUS FILE at {0}." -f (Get-CamStatusFilePath -Config $Config)) -Colour 'Red'
        Write-CamHost -Message 'The supervisor has never run against this archive root, or the root is wrong. Start it with -Run.' -Colour 'Yellow'
        Write-CamHost -Message ''
        return
    }

    if ($state.Running) {
        Write-CamHost -Message ("SUPERVISOR RUNNING - {0}, up {1}" -f $state.Reason, (Format-CamAgeText -Seconds $snapshot.UptimeSeconds)) -Colour 'Green'
    }
    else {
        Write-CamHost -Message ("STALE - NO SUPERVISOR IS RUNNING: {0}." -f $state.Reason) -Colour 'Red'
        Write-CamHost -Message ("Everything below is the last snapshot, written {0} ago at {1}. It is history, not the current state." -f `
            (Format-CamAgeText -Seconds $state.AgeSeconds), $snapshot.UpdatedAt) -Colour 'Yellow'
    }

    Write-CamHost -Message ("root {0} | container {1} | segments {2}s" -f $snapshot.RootPath, $snapshot.Container, $snapshot.SegmentSeconds)
    Write-CamHost -Message ''
    Write-CamHost -Message ("{0,-22} {1,-7} {2,-7} {3,-8} {4,-10} {5,-9} {6}" -f 'CAMERA', 'RECORD', 'ALIVE', 'PID', 'SEGMENT', 'RESTART/h', 'LAST ERROR') -Colour 'Cyan'

    foreach ($camera in @($snapshot.Cameras)) {
        $record = 'off'
        if ($camera.Record) { $record = 'ON' }

        $alive = '-'
        $colour = 'Gray'
        if ($camera.Record) {
            $alive = 'no'
            $colour = 'Red'
            if ($camera.Alive) { $alive = 'yes'; $colour = 'Green' }
        }
        if (-not $state.Running) { $colour = 'DarkGray' }

        $processId = '-'
        if ($camera.Pid) { $processId = [string]$camera.Pid }

        $segment = '-'
        if ($null -ne $camera.CurrentSegmentBytes -and $camera.CurrentSegment) {
            $segment = ("{0} MB" -f (Format-CamNumber -Value ([double]$camera.CurrentSegmentBytes / 1MB) -Format '0.0'))
        }

        $errorText = [string]$camera.LastError
        if ($camera.ConfigError) { $errorText = 'config: ' + [string]$camera.ConfigError }
        if ($errorText.Length -gt 46) { $errorText = $errorText.Substring(0, 46) + '...' }

        Write-CamHost -Message ("{0,-22} {1,-7} {2,-7} {3,-8} {4,-10} {5,-9} {6}" -f `
            $camera.Name, $record, $alive, $processId, $segment, $camera.RestartsLastHour, $errorText) -Colour $colour

        if ($camera.Record) {
            $detail = "    segment {0}, last closed segment {1} ago" -f `
                $(if ($camera.CurrentSegment) { $camera.CurrentSegment } else { 'none yet' }), `
                (Format-CamAgeText -Seconds $camera.SecondsSinceLastClosedSegment)
            # Only meaningful while the supervisor is actually running.
            if ($camera.NextStartAt -and $state.Running) { $detail += ", next start attempt $($camera.NextStartAt)" }
            Write-CamHost -Message $detail -Colour 'DarkGray'
        }
    }

    # Every figure below goes through Format-CamNumber: the machine's locale
    # writes a decimal comma, which makes GB figures ambiguous to read.
    $disk = $snapshot.Disk
    Write-CamHost -Message ''
    Write-CamHost -Message ("disk    free {0} GB | reserve {1} GB | hysteresis {2} GB" -f `
        (Format-CamNumber -Value ([double]$disk.FreeGB)), (Format-CamNumber -Value ([double]$disk.ReserveGB)), `
        (Format-CamNumber -Value ([double]$disk.PruneHysteresisGB)))

    $archiveSize = '0'
    if ($disk.ArchiveBytes) { $archiveSize = Format-CamNumber -Value ([double]$disk.ArchiveBytes / 1GB) -Format '0.000' }
    Write-CamHost -Message ("archive {0} GB in {1} file(s) | oldest {2} | newest {3}" -f `
        $archiveSize, $disk.ArchiveFiles, $disk.OldestSegment, $disk.NewestSegment)

    if ($disk.MeasuredGBPerDay) {
        Write-CamHost -Message ("rate    {0} GB/day measured | {1} day(s) until the reserve is reached" -f `
            (Format-CamNumber -Value ([double]$disk.MeasuredGBPerDay)), (Format-CamNumber -Value ([double]$disk.ProjectedDaysRemaining) -Format '0.0'))
    }
    else {
        Write-CamHost -Message 'rate    not measured yet: the archive needs at least ten minutes of history'
    }

    $prune = $snapshot.Prune
    $pruneColour = 'Gray'
    if ($prune.Blocked) { $pruneColour = 'Red' }
    Write-CamHost -Message ("prune   last run {0} | {1} file(s) deleted, {2} GB freed{3}" -f `
        $(if ($prune.LastRunAt) { $prune.LastRunAt } else { 'never' }), $prune.DeletedTotal, `
        (Format-CamNumber -Value ([double]$prune.FreedGBTotal) -Format '0.000'), `
        $(if ($prune.Blocked) { ' | BLOCKED' } else { '' })) -Colour $pruneColour
    if ($prune.LastMessage) { Write-CamHost -Message ("        {0}" -f $prune.LastMessage) -Colour $pruneColour }
    Write-CamHost -Message ''
}


function Invoke-CamStopSupervisor {
    <#
        Asks a running supervisor to stop and waits for it to finish.

        The request is a flag file the supervisor notices within one tick: it
        then kills its own children in order, drains their stderr into the logs
        and writes a final status file. Doing the teardown inside the supervisor
        is what guarantees no orphaned ffmpeg, which a kill from outside cannot
        promise. A hard kill of each child is safe because MKV stays readable
        through one - proved in Phase A with ffprobe on the interrupted segment.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $snapshot = Get-CamStatusSnapshot -Config $Config
    $state = Get-CamSupervisorState -Config $Config -Snapshot $snapshot
    $flag = Get-CamStopFlagPath -Config $Config

    if (-not $state.Running) {
        Write-CamHost -Message ("Nothing to stop: {0}." -f $state.Reason) -Colour 'Yellow'
        if (Test-Path -LiteralPath $flag) {
            Remove-Item -LiteralPath $flag -Force -ErrorAction SilentlyContinue
            Write-CamHost -Message 'Cleared a leftover stop request so the next -Run is not stopped at startup.'
        }
        return
    }

    $supervisorPid = [int]$state.Pid
    $stateDir = Get-CamStateDirPath -Config $Config
    if (-not (Test-Path -LiteralPath $stateDir)) { [void](New-Item -ItemType Directory -Path $stateDir -Force) }

    [System.IO.File]::WriteAllText($flag, ("stop requested at {0} by pid {1}" -f [DateTime]::Now.ToString('o'), $PID) + [Environment]::NewLine)
    Write-CamHost -Message ("stop requested; waiting for supervisor pid {0}" -f $supervisorPid) -Colour 'Cyan'

    $timeout = [double]$Config.StopTimeoutSeconds
    $deadline = [DateTime]::Now.AddSeconds($timeout)
    $stopped = $false
    while ([DateTime]::Now -lt $deadline) {
        if (-not (Get-Process -Id $supervisorPid -ErrorAction SilentlyContinue)) { $stopped = $true; break }
        Start-Sleep -Milliseconds 500
    }

    if (-not $stopped) {
        Write-CamHost -Message ("SUPERVISOR pid {0} DID NOT STOP within {1}s. The stop request is still in place, so it will shut down when it next ticks. Nothing was killed from here, because killing the supervisor would orphan its ffmpeg children." -f `
            $supervisorPid, [int]$timeout) -Colour 'Red'
        exit 1
    }

    Write-CamHost -Message ("supervisor pid {0} stopped" -f $supervisorPid) -Colour 'Green'

    # The supervisor's final status file lists the children it had. Anything of
    # those still alive is an orphan, and -Stop promises there are none.
    $final = Get-CamStatusSnapshot -Config $Config
    $orphans = @()
    if ($final) {
        foreach ($camera in @($final.Cameras)) {
            $childPid = $camera.LastPid
            if (-not $childPid) { $childPid = $camera.Pid }
            if (-not $childPid) { continue }
            $process = Get-Process -Id ([int]$childPid) -ErrorAction SilentlyContinue
            # Only an ffmpeg by that id counts: the number may have been reused.
            if ($process -and $process.Name -eq 'ffmpeg') { $orphans += $process }
        }
    }

    if ($orphans.Count -eq 0) {
        Write-CamHost -Message 'no ffmpeg child left behind' -Colour 'Green'
    }
    else {
        foreach ($process in $orphans) {
            Write-CamHost -Message ("ORPHAN ffmpeg pid {0} survived the shutdown; killing it" -f $process.Id) -Colour 'Red'
            try { $process.Kill() } catch { }
        }
    }

    if (Test-Path -LiteralPath $flag) { Remove-Item -LiteralPath $flag -Force -ErrorAction SilentlyContinue }
}


function New-CamChildFor {
    <#
        Builds the child entry for one resolved camera, creating its directory
        first. The segment muxer cannot create directories, so the directory has
        to exist before ffmpeg starts - including for a camera enabled while the
        supervisor is already running.
    #>
    param(
        [Parameter(Mandatory = $true)]$Camera,
        [Parameter(Mandatory = $true)]$Paths,
        [Parameter(Mandatory = $true)]$ContainerProfile
    )

    $cameraDir = Join-Path -Path $Paths.Root -ChildPath $Camera.SafeName
    if (-not (Test-Path -LiteralPath $cameraDir)) { [void](New-Item -ItemType Directory -Path $cameraDir -Force) }

    return New-CamChildEntry -Camera $Camera -CameraDir $cameraDir `
        -LogPath (Join-Path -Path $Paths.LogDir -ChildPath "$($Camera.SafeName).log") `
        -Pattern (Get-CamSegmentPattern -CameraDir $cameraDir -SafeName $Camera.SafeName -ContainerProfile $ContainerProfile)
}


function Invoke-CamReconcile {
    <#
        Makes the running set match the Record flags in cameras.json. This is
        the live pickup: -Enable and -Disable only write the file, and the
        supervisor notices within ReconcileSeconds.

        A deliberate stop never touches the backoff or the restart count. A
        disable is not a crash, and treating it as one would both report a false
        restart and delay the camera the next time it is enabled.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$Credential,
        [Parameter(Mandatory = $true)]$ContainerProfile,
        [Parameter(Mandatory = $true)]$Paths,
        [Parameter(Mandatory = $true)]$Children,
        [Parameter(Mandatory = $true)][string]$SupervisorLog,
        [switch]$First
    )

    $maxBytes = [int]$Config.LogMaxBytes
    $resolved = Get-CamResolvedCameras -Config $Config -Credential $Credential

    foreach ($camera in @($resolved | Where-Object { $_.Error })) {
        $message = "skipping {0}: {1}" -f $camera.Name, $camera.Error
        Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $maxBytes
        Write-CamHost -Message $message -Colour 'Red'
    }

    $enabled = @($resolved | Where-Object { $_.Record -and -not $_.Error })

    foreach ($key in @($Children.Keys)) {
        $entry = $Children[$key]

        $match = $null
        foreach ($camera in $enabled) {
            if ($camera.SafeName -eq $key) { $match = $camera; break }
        }

        if (-not $match) {
            Stop-CamChild -Entry $entry -Config $Config -SupervisorLog $SupervisorLog `
                -Reason 'no longer set to record in cameras.json'
            $Children.Remove($key)
            $message = "reconcile: {0} is no longer set to record, stopped it" -f $entry.Name
            Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $maxBytes
            Write-CamHost -Message $message -Colour 'Yellow'
            continue
        }

        # Compared on address and stream rather than on the URL, which carries
        # the password.
        if ([string]$match.IP -ne [string]$entry.IP -or [string]$match.Quality -ne [string]$entry.Quality) {
            Stop-CamChild -Entry $entry -Config $Config -SupervisorLog $SupervisorLog `
                -Reason 'address or stream changed in cameras.json'
            $Children.Remove($key)
            $message = "reconcile: {0} address or stream changed, restarting it" -f $entry.Name
            Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $maxBytes
            Write-CamHost -Message $message -Colour 'Yellow'
        }
    }

    foreach ($camera in $enabled) {
        if ($Children.ContainsKey($camera.SafeName)) { continue }

        $entry = New-CamChildFor -Camera $camera -Paths $Paths -ContainerProfile $ContainerProfile
        # No backoff on a fresh enable: the operator should see it recording on
        # the next tick, not after a wait inherited from some earlier failure.
        $entry.NextStartAt = [DateTime]::Now
        $Children[$camera.SafeName] = $entry

        if (-not $First) {
            $message = "reconcile: {0} is now set to record, starting it" -f $camera.Name
            Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $maxBytes
            Write-CamHost -Message $message -Colour 'Green'
        }
    }

    return , $resolved
}


function Invoke-CamSupervisor {
    param([Parameter(Mandatory = $true)]$Config)

    $credential = Get-CamCredential
    $containerProfile = Get-CamContainerProfile -Config $Config
    $paths = Get-CamPaths -Config $Config
    $supervisorLog = Join-Path -Path $paths.LogDir -ChildPath 'recorder.log'
    $statusPath = Get-CamStatusFilePath -Config $Config
    $stopFlag = Get-CamStopFlagPath -Config $Config
    $camerasPath = Join-Path -Path (Get-CamLibRoot) -ChildPath 'cameras.json'
    $maxBytes = [int]$Config.LogMaxBytes

    # One supervisor per archive. Two would each start a child per camera, and
    # these cameras serve only two RTSP sessions each - the second supervisor
    # would take the dashboard's slot and neither set would be healthy.
    $existing = Get-CamSupervisorState -Config $Config -Snapshot (Get-CamStatusSnapshot -Config $Config)
    if ($existing.Running) {
        Write-CamHost -Message ("A supervisor is already running: {0}. Refusing to start a second one - use -Stop first." -f $existing.Reason) -Colour 'Red'
        exit 1
    }

    if (Test-Path -LiteralPath $stopFlag) {
        Remove-Item -LiteralPath $stopFlag -Force -ErrorAction SilentlyContinue
        Write-CamLog -Path $supervisorLog -Message 'cleared a leftover stop request from an earlier run' -MaxBytes $maxBytes
    }

    $startedAt = [DateTime]::Now
    $children = @{}
    $cameras = @()
    $archive = $null
    $pruneState = @{ LastRunAt = $null; DeletedTotal = 0; FreedBytes = 0; Blocked = $false; LastMessage = '' }

    $listStamp = [DateTime]::MinValue
    $lastReconcile = [DateTime]::MinValue
    $lastStatus = [DateTime]::MinValue
    $lastPrune = [DateTime]::MinValue
    $lastArchiveScan = [DateTime]::MinValue
    $stopRequested = $false

    Write-CamHost -Message ''
    Write-CamHost -Message '=== cam.recorder supervisor ===' -Colour 'Cyan'
    $reserveText = Format-CamNumber -Value ([double]$Config.ReserveGB)
    Write-CamLog -Path $supervisorLog -Message ("supervisor starting: pid {0}, container {1}, segments {2}s, root {3}, reconcile {4}s, reserve {5} GB" -f `
            $PID, $Config.Container, $Config.SegmentSeconds, $Config.RootPath, $Config.ReconcileSeconds, $reserveText) -MaxBytes $maxBytes
    Write-CamHost -Message ("pid {0} | root {1} | reconcile {2}s | reserve {3} GB | stop with Ctrl+C or -Stop" -f `
            $PID, $Config.RootPath, $Config.ReconcileSeconds, $reserveText)

    try {
        while ($true) {
            $now = [DateTime]::Now

            if (Test-Path -LiteralPath $stopFlag) {
                $stopRequested = $true
                Write-CamLog -Path $supervisorLog -Message 'stop requested through _state\stop.request' -MaxBytes $maxBytes
                Write-CamHost -Message 'stop requested' -Colour 'Cyan'
                break
            }

            # --- live pickup: cameras.json changed since the last look?
            if (($now - $lastReconcile).TotalSeconds -ge [double]$Config.ReconcileSeconds) {
                $lastReconcile = $now

                $stamp = $listStamp
                $readable = $true
                try { $stamp = (Get-Item -LiteralPath $camerasPath -Force).LastWriteTimeUtc }
                catch { $readable = $false }

                if ($readable -and $stamp -ne $listStamp) {
                    $isFirst = ($listStamp -eq [DateTime]::MinValue)
                    try {
                        $cameras = Invoke-CamReconcile -Config $Config -Credential $credential `
                            -ContainerProfile $containerProfile -Paths $paths -Children $children `
                            -SupervisorLog $supervisorLog -First:$isFirst
                        $listStamp = $stamp

                        $recording = @($cameras | Where-Object { $_.Record -and -not $_.Error }).Count
                        $message = "reconcile: {0} of {1} camera(s) set to record, {2} child(ren) managed" -f `
                            $recording, @($cameras).Count, $children.Count
                        Write-CamLog -Path $supervisorLog -Message $message -MaxBytes $maxBytes
                        Write-CamHost -Message $message -Colour 'Cyan'

                        if ($children.Count -eq 0) {
                            Write-CamHost -Message 'No camera has Record set to true, so nothing is being recorded. Waiting for -Enable.' -Colour 'Yellow'
                        }
                    }
                    catch {
                        # A half-written camera list must not take the recorder
                        # down; keep the current children and look again later.
                        Write-CamLog -Path $supervisorLog -Message ("reconcile failed, keeping the current camera set: {0}" -f $_.Exception.Message) -MaxBytes $maxBytes
                    }
                }
            }

            $statusDue = (($now - $lastStatus).TotalSeconds -ge [double]$Config.StatusIntervalSeconds)

            foreach ($entry in @($children.Values)) {
                # One directory scan per cycle, shared by the status file and the
                # stall watchdog.
                if ($statusDue) { $entry.Scan = Get-CamDirScan -Path $entry.Dir }

                if ($entry.Process) {
                    Read-CamChildStderr -Entry $entry -Config $Config

                    if ($entry.Process.HasExited) {
                        $exitCode = $entry.Process.ExitCode
                        $ranFor = ($now - $entry.StartedAt).TotalSeconds
                        Read-CamChildStderr -Entry $entry -Config $Config -Final
                        try { $entry.Process.Dispose() } catch { }
                        $entry.Process = $null
                        $entry.ReadTask = $null
                        $entry.RestartCount = $entry.RestartCount + 1
                        [void]$entry.Restarts.Add($now)
                        $entry.LastError = "ffmpeg exited with code $exitCode after $([int]$ranFor)s"
                        $entry.LastErrorAt = $now

                        $wait = Set-CamChildBackoff -Entry $entry -Config $Config -RanForSeconds $ranFor
                        $message = "exit {0} after {1}s for {2}; restart #{3} in {4}s" -f `
                            $exitCode, [int]$ranFor, $entry.Name, $entry.RestartCount, $wait
                        Write-CamLog -Path $supervisorLog -Message $message -MaxBytes $maxBytes
                        Write-CamHost -Message $message -Colour 'Yellow'
                    }
                    else {
                        $newestWrite = $null
                        if ($entry.Scan) {
                            $newestWrite = $entry.Scan.NewestClosedWrite
                            # The creation of the segment ffmpeg currently holds
                            # counts as progress: it only opens a new file when it
                            # cuts one. Without this the watchdog restarts a
                            # perfectly healthy child whenever the pruner has just
                            # deleted its closed segments, which is exactly what
                            # happens on a disk that cannot be freed.
                            if ($entry.Scan.CurrentCreated -and ($null -eq $newestWrite -or $entry.Scan.CurrentCreated -gt $newestWrite)) {
                                $newestWrite = $entry.Scan.CurrentCreated
                            }
                            if ($null -eq $newestWrite) { $newestWrite = $entry.StartedAt }
                        }

                        $stall = Test-CamChildStalled -Entry $entry -Config $Config -Now $now -NewestWriteTime $newestWrite
                        if ($stall.Stalled) {
                            $reason = "stalled, no segment write for {0}s (limit {1}s) while the process was alive" -f `
                                [int]$stall.AgeSeconds, [int]$stall.Threshold
                            $ranFor = ($now - $entry.StartedAt).TotalSeconds
                            Write-CamLog -Path $supervisorLog -Message ("watchdog {0}: {1}" -f $entry.Name, $reason) -MaxBytes $maxBytes
                            Stop-CamChild -Entry $entry -Config $Config -SupervisorLog $supervisorLog -Reason $reason
                            $entry.RestartCount = $entry.RestartCount + 1
                            [void]$entry.Restarts.Add($now)
                            $entry.LastError = $reason
                            $entry.LastErrorAt = $now
                            $wait = Set-CamChildBackoff -Entry $entry -Config $Config -RanForSeconds $ranFor
                            $message = "watchdog restart #{0} for {1} in {2}s" -f $entry.RestartCount, $entry.Name, $wait
                            Write-CamLog -Path $supervisorLog -Message $message -MaxBytes $maxBytes
                            Write-CamHost -Message $message -Colour 'Yellow'
                        }
                    }
                }
                else {
                    if ($now -ge $entry.NextStartAt) {
                        try {
                            Start-CamChild -Entry $entry -Config $Config -ContainerProfile $containerProfile -SupervisorLog $supervisorLog
                        }
                        catch {
                            $entry.LastError = "failed to start: $($_.Exception.Message)"
                            $entry.LastErrorAt = $now
                            $wait = Set-CamChildBackoff -Entry $entry -Config $Config -RanForSeconds 0
                            Write-CamLog -Path $supervisorLog -Message ("failed to start {0}: {1}; retry in {2}s" -f $entry.Name, $_.Exception.Message, $wait) -MaxBytes $maxBytes
                            Write-CamHost -Message ("failed to start {0}: {1}" -f $entry.Name, $_.Exception.Message) -Colour 'Red'
                        }
                    }
                }
            }

            if ($statusDue) {
                $lastStatus = $now

                if (($now - $lastArchiveScan).TotalSeconds -ge [double]$Config.ArchiveScanSeconds) {
                    $lastArchiveScan = $now
                    try { $archive = Get-CamArchiveScan -Root $Config.RootPath }
                    catch { Write-CamLog -Path $supervisorLog -Message ("archive scan failed: {0}" -f $_.Exception.Message) -MaxBytes $maxBytes }
                }

                try {
                    Write-CamStatusFile -Config $Config -Path $statusPath -Cameras $cameras -Children $children `
                        -StartedAt $startedAt -State 'running' -Archive $archive -PruneState $pruneState
                }
                catch {
                    Write-CamLog -Path $supervisorLog -Message ("status write failed: {0}" -f $_.Exception.Message) -MaxBytes $maxBytes
                }
            }

            if (($now - $lastPrune).TotalSeconds -ge [double]$Config.PruneIntervalSeconds) {
                $lastPrune = $now
                try { [void](Invoke-CamPrune -Config $Config -SupervisorLog $supervisorLog -State $pruneState) }
                catch { Write-CamLog -Path $supervisorLog -Message ("prune failed: {0}" -f $_.Exception.Message) -MaxBytes $maxBytes }
            }

            Start-Sleep -Seconds 1
        }
    }
    finally {
        $reason = 'supervisor shutdown'
        if ($stopRequested) { $reason = 'stop requested' }

        Write-CamHost -Message 'shutting down, stopping children' -Colour 'Cyan'
        Write-CamLog -Path $supervisorLog -Message ("supervisor shutting down: {0}" -f $reason) -MaxBytes $maxBytes

        foreach ($entry in @($children.Values)) {
            try { Stop-CamChild -Entry $entry -Config $Config -SupervisorLog $supervisorLog -Reason $reason }
            catch { Write-CamLog -Path $supervisorLog -Message ("failed to stop {0}: {1}" -f $entry.Name, $_.Exception.Message) -MaxBytes $maxBytes }
        }

        # A final status file, so -Status and the health strip can say "stopped
        # cleanly at T" instead of showing a snapshot they cannot account for.
        try {
            Write-CamStatusFile -Config $Config -Path $statusPath -Cameras $cameras -Children $children `
                -StartedAt $startedAt -State 'stopped' -Archive $archive -PruneState $pruneState
        }
        catch { }

        if (Test-Path -LiteralPath $stopFlag) { Remove-Item -LiteralPath $stopFlag -Force -ErrorAction SilentlyContinue }

        Write-CamLog -Path $supervisorLog -Message 'supervisor stopped' -MaxBytes $maxBytes
        Write-CamHost -Message 'stopped' -Colour 'Cyan'
    }
}


# ===========================================================================
# Entry point
# ===========================================================================

$config = Get-CamConfig

if ($SelfTest) {
    Invoke-CamSelfTest -Config $config -MaxConcurrency $MaxConcurrency -Offline:$Offline
}
elseif ($Enable) {
    Set-CamRecordState -Config $config -Target $Enable -Enabled $true
}
elseif ($Disable) {
    Set-CamRecordState -Config $config -Target $Disable -Enabled $false
}
elseif ($List) {
    Show-CamList -Config $config
}
elseif ($Status) {
    Show-CamStatus -Config $config
}
elseif ($Stop) {
    Invoke-CamStopSupervisor -Config $config
}
elseif ($Run) {
    Invoke-CamSupervisor -Config $config
}
else {
    Write-CamHost -Message 'Usage:' -Colour 'Cyan'
    Write-CamHost -Message '  cam.recorder.ps1 -Run                  supervise every camera with Record=true'
    Write-CamHost -Message '  cam.recorder.ps1 -Enable  <name|all>   set Record=true, picked up live'
    Write-CamHost -Message '  cam.recorder.ps1 -Disable <name|all>   set Record=false, picked up live'
    Write-CamHost -Message '  cam.recorder.ps1 -List                 cameras with their Record state'
    Write-CamHost -Message '  cam.recorder.ps1 -Status               health, disk and projection'
    Write-CamHost -Message '  cam.recorder.ps1 -Stop                 stop a running supervisor'
    Write-CamHost -Message '  cam.recorder.ps1 -SelfTest [-Offline]  pre-flight checks'
}
