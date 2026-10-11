<#
.SYNOPSIS
    Continuous RTSP recorder for the IPCCTV dashboard cameras.

.DESCRIPTION
    -Run       Supervises one ffmpeg child per camera whose Record flag is true in
               cameras.json, writing clock-aligned segments with audio.
    -SelfTest  Pre-flight report: tool check, per-camera RTSP probe, measured
               concurrent-connection limit, crash-safety proof, disk projection.

    Targets Windows PowerShell 5.1.

.EXAMPLE
    .\cam.recorder.ps1 -SelfTest

.EXAMPLE
    .\cam.recorder.ps1 -Run
#>
[CmdletBinding(DefaultParameterSetName = 'Run')]
param(
    [Parameter(ParameterSetName = 'Run')]
    [switch]$Run,

    [Parameter(ParameterSetName = 'SelfTest')]
    [switch]$SelfTest,

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


function Invoke-CamSelfTest {
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][int]$MaxConcurrency
    )

    Write-CamHost -Message '' 
    Write-CamHost -Message '=== cam.recorder self-test ===' -Colour 'Cyan'
    Write-CamHost -Message ("PowerShell {0} | archive root {1} | container {2}" -f $PSVersionTable.PSVersion, $Config.RootPath, $Config.Container)
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
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][DateTime]$Now
    )

    $threshold = [double]$Config.SegmentSeconds * [double]$Config.StallFactor
    $reference = $Entry.StartedAt

    if (Test-Path -LiteralPath $Entry.Dir) {
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


function Invoke-CamSupervisor {
    param([Parameter(Mandatory = $true)]$Config)

    $credential = Get-CamCredential
    $cameras = Get-CamResolvedCameras -Config $Config -Credential $credential
    $containerProfile = Get-CamContainerProfile -Config $Config

    foreach ($camera in @($cameras | Where-Object { $_.Error })) {
        Write-CamHost -Message ("skipping {0}: {1}" -f $camera.Name, $camera.Error) -Colour 'Red'
    }

    # Record must be explicitly true. A camera without the field is off, so
    # adding the recorder to an existing install records nothing until asked.
    $enabled = @($cameras | Where-Object { $_.Record -and -not $_.Error })

    $paths = Get-CamPaths -Config $Config -CameraSafeNames @($enabled | ForEach-Object { $_.SafeName })
    $supervisorLog = Join-Path -Path $paths.LogDir -ChildPath 'recorder.log'

    Write-CamHost -Message ''
    Write-CamHost -Message '=== cam.recorder supervisor ===' -Colour 'Cyan'
    Write-CamLog -Path $supervisorLog -Message ("supervisor starting: {0} of {1} camera(s) enabled, container {2}, segments {3}s, root {4}" -f `
            $enabled.Count, @($cameras).Count, $Config.Container, $Config.SegmentSeconds, $Config.RootPath) -MaxBytes $Config.LogMaxBytes
    foreach ($camera in $cameras) {
        $state = 'off'
        if ($camera.Record) { $state = 'RECORD' }
        Write-CamHost -Message ("  {0,-22} {1}" -f $camera.Name, $state)
    }

    if ($enabled.Count -eq 0) {
        Write-CamHost -Message 'No camera has Record set to true. Nothing to do.' -Colour 'Yellow'
        Write-CamLog -Path $supervisorLog -Message 'no camera enabled, exiting' -MaxBytes $Config.LogMaxBytes
        return
    }

    $children = @()
    foreach ($camera in $enabled) {
        $cameraDir = $paths.Cameras[$camera.SafeName]
        $children += New-CamChildEntry -Camera $camera -CameraDir $cameraDir `
            -LogPath (Join-Path -Path $paths.LogDir -ChildPath "$($camera.SafeName).log") `
            -Pattern (Get-CamSegmentPattern -CameraDir $cameraDir -SafeName $camera.SafeName -ContainerProfile $containerProfile)
    }

    Write-CamHost -Message ("recording {0} camera(s). Ctrl+C to stop." -f $children.Count) -Colour 'Cyan'

    try {
        while ($true) {
            $now = [DateTime]::Now

            foreach ($entry in $children) {
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

                        $wait = Set-CamChildBackoff -Entry $entry -Config $Config -RanForSeconds $ranFor
                        $message = "exit {0} after {1}s for {2}; restart #{3} in {4}s" -f `
                            $exitCode, [int]$ranFor, $entry.Name, $entry.RestartCount, $wait
                        Write-CamLog -Path $supervisorLog -Message $message -MaxBytes $Config.LogMaxBytes
                        Write-CamHost -Message $message -Colour 'Yellow'
                    }
                    else {
                        $stall = Test-CamChildStalled -Entry $entry -Config $Config -Now $now
                        if ($stall.Stalled) {
                            $reason = "stalled, no segment write for {0}s (limit {1}s) while the process was alive" -f `
                                [int]$stall.AgeSeconds, [int]$stall.Threshold
                            $ranFor = ($now - $entry.StartedAt).TotalSeconds
                            Write-CamLog -Path $supervisorLog -Message ("watchdog {0}: {1}" -f $entry.Name, $reason) -MaxBytes $Config.LogMaxBytes
                            Stop-CamChild -Entry $entry -Config $Config -SupervisorLog $supervisorLog -Reason $reason
                            $entry.RestartCount = $entry.RestartCount + 1
                            $wait = Set-CamChildBackoff -Entry $entry -Config $Config -RanForSeconds $ranFor
                            $message = "watchdog restart #{0} for {1} in {2}s" -f $entry.RestartCount, $entry.Name, $wait
                            Write-CamLog -Path $supervisorLog -Message $message -MaxBytes $Config.LogMaxBytes
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
                            $wait = Set-CamChildBackoff -Entry $entry -Config $Config -RanForSeconds 0
                            Write-CamLog -Path $supervisorLog -Message ("failed to start {0}: {1}; retry in {2}s" -f $entry.Name, $_.Exception.Message, $wait) -MaxBytes $Config.LogMaxBytes
                            Write-CamHost -Message ("failed to start {0}: {1}" -f $entry.Name, $_.Exception.Message) -Colour 'Red'
                        }
                    }
                }
            }

            Start-Sleep -Seconds 1
        }
    }
    finally {
        Write-CamHost -Message 'shutting down, stopping children' -Colour 'Cyan'
        Write-CamLog -Path $supervisorLog -Message 'supervisor shutting down' -MaxBytes $Config.LogMaxBytes
        foreach ($entry in $children) {
            Stop-CamChild -Entry $entry -Config $Config -SupervisorLog $supervisorLog -Reason 'supervisor shutdown'
        }
        Write-CamLog -Path $supervisorLog -Message 'supervisor stopped' -MaxBytes $Config.LogMaxBytes
        Write-CamHost -Message 'stopped' -Colour 'Cyan'
    }
}


# ===========================================================================
# Entry point
# ===========================================================================

$config = Get-CamConfig

if ($SelfTest) {
    Invoke-CamSelfTest -Config $config -MaxConcurrency $MaxConcurrency
}
elseif ($Run) {
    Invoke-CamSupervisor -Config $config
}
else {
    Write-CamHost -Message 'Usage: cam.recorder.ps1 -SelfTest | -Run' -Colour 'Cyan'
}
