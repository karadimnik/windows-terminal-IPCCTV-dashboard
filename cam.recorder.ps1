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
    -InstallRestreamer
               Generates the MediaMTX config from cameras.json, one path per
               camera, and checks the binary is in place. The supervisor then
               owns the process. No credential is written to disk.
    -RemoveRestreamer
               Deletes that generated config. Recording and viewing continue on
               the direct camera inputs.
    -InstallAutostart
               Registers a Scheduled Task that runs -Run at logon as the current
               user. Needs no elevation and is idempotent.
    -RemoveAutostart
               Unregisters that task. A no-op when it is not installed.
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

    [Parameter(ParameterSetName = 'InstallRestreamer')]
    [switch]$InstallRestreamer,

    [Parameter(ParameterSetName = 'RemoveRestreamer')]
    [switch]$RemoveRestreamer,

    [Parameter(ParameterSetName = 'InstallAutostart')]
    [switch]$InstallAutostart,

    [Parameter(ParameterSetName = 'RemoveAutostart')]
    [switch]$RemoveAutostart,

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


function Show-CamRestreamerPlan {
    <# One line per camera: path name, the variable its URL arrives in, the URL consumers use. #>
    param([Parameter(Mandatory = $true)]$Config, [Parameter(Mandatory = $true)]$PathMap)

    foreach ($pathName in $PathMap.Keys) {
        $camera = $PathMap[$pathName]
        Write-CamHost -Message ("  {0,-20} {1,-18} {2,-40} {3}" -f `
            $pathName, [string]$camera.IP, (Get-CamRelayUrl -Config $Config -PathName $pathName), (Get-CamRelaySourceEnvName -PathName $pathName))
    }
}


function Invoke-CamInstallRestreamer {
    <#
        Generates the live MediaMTX config from cameras.json: one path per
        camera, named by its safe name, addresses taken from recorder.config.json.

        No credential is written. Every path keeps source: publisher in the file
        and the supervisor supplies the real URL on the child's environment
        block at launch, which is the only reason the camera password stays off
        the disk.

        The binary itself is not installed here. It is a 55 MB download that
        must not live in the repository, so this command verifies it is in place
        and says exactly what to do when it is not.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $exePath = [string]$Config.RestreamerExePath
    $configPath = [string]$Config.RestreamerConfigPath

    Write-CamHost -Message ''
    Write-CamHost -Message '=== install the restreamer ===' -Colour 'Cyan'

    if ($configPath -match '^\\\\wsl') {
        throw "RestreamerConfigPath '$configPath' is on the WSL share. MediaMTX watches its config with ReadDirectoryChanges, which that share does not implement, and exits immediately with 'ReadDirectoryChanges: Incorrect function.' Use a local NTFS path such as C:\mediamtx\mediamtx.yml."
    }

    $rtspHost = Get-CamRelayHostPort -Url ([string]$Config.RelayBase)
    if ($rtspHost -notmatch '^(127\.0\.0\.1|localhost|\[::1\])(:|$)') {
        Write-CamHost -Message ("WARNING: RelayBase binds MediaMTX to {0}, not loopback. The generated config allows anonymous access, which is only acceptable because nothing off this machine can reach 127.0.0.1. Configure MediaMTX authentication before binding it to a LAN address." -f $rtspHost) -Colour 'Red'
    }

    $pathMap = Get-CamRelayPathMap -Cameras (Get-CamList)
    $text = Get-CamRelayConfigText -Config $Config -PathMap $pathMap

    $dir = Split-Path -Path $configPath -Parent
    if ($dir -and -not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }

    # Temp-then-rename: MediaMTX watches this file and reloads on change, so it
    # must never read a half-written one.
    $temp = "$configPath.tmp"
    if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Force }
    [System.IO.File]::WriteAllText($temp, $text, (New-Object System.Text.UTF8Encoding($false)))
    Move-CamFileAtomic -TempPath $temp -Path $configPath

    Write-CamHost -Message ("wrote {0} ({1} path(s), no credentials)" -f $configPath, $pathMap.Count) -Colour 'Green'
    Write-CamHost -Message ''
    Write-CamHost -Message ("  {0,-20} {1,-18} {2,-40} {3}" -f 'path', 'camera', 'consumers use', 'source arrives in')
    Show-CamRestreamerPlan -Config $Config -PathMap $pathMap
    Write-CamHost -Message ''

    if (-not (Test-Path -LiteralPath $exePath)) {
        Write-CamHost -Message ("MediaMTX IS NOT INSTALLED at {0}." -f $exePath) -Colour 'Red'
        Write-CamHost -Message '  Download mediamtx_<version>_windows_amd64.zip from'
        Write-CamHost -Message '  https://github.com/bluenviron/mediamtx/releases and extract mediamtx.exe to that path.'
        Write-CamHost -Message '  Verified on v1.21.2. Do not put it in Program Files: that needs administrator rights.'
        Write-CamHost -Message '  The recorder and the dashboard keep working on their direct camera inputs until it is there.' -Colour 'Yellow'
        return
    }

    $version = '(not reported)'
    $probe = Invoke-CamNative -FilePath $exePath -Arguments @('--version')
    if (-not [string]::IsNullOrWhiteSpace($probe)) {
        $version = (($probe -split "`r?`n") | Where-Object { $_.Trim() } | Select-Object -First 1).Trim()
    }
    Write-CamHost -Message ("MediaMTX present: {0} {1}" -f $exePath, $version) -Colour 'Green'

    if (-not (Get-CamRelayEnabled -Config $Config)) {
        Write-CamHost -Message 'RestreamerEnabled is false in recorder.config.json, so nothing will start or use it. Set it to true.' -Colour 'Yellow'
        return
    }

    Write-CamHost -Message 'The supervisor owns the process: cam.recorder.ps1 -Run starts it, -Stop stops it.' -Colour 'Cyan'
}


function Invoke-CamRemoveRestreamer {
    <#
        Deletes the generated config. The binary is deliberately left alone - it
        is a download, not something this script put there - and so is
        RestreamerEnabled in recorder.config.json, because silently rewriting a
        file the Operator maintains is worse than printing the one line to
        change.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $configPath = [string]$Config.RestreamerConfigPath

    Write-CamHost -Message ''
    Write-CamHost -Message '=== remove the restreamer ===' -Colour 'Cyan'

    $state = Get-CamSupervisorState -Config $Config -Snapshot (Get-CamStatusSnapshot -Config $Config)
    if ($state.Running) {
        Write-CamHost -Message ("A supervisor is running ({0}) and owns the MediaMTX process. Run -Stop first, or it will keep running until the supervisor shuts down." -f $state.Reason) -Colour 'Yellow'
    }

    if (Test-Path -LiteralPath $configPath) {
        Remove-Item -LiteralPath $configPath -Force
        Write-CamHost -Message ("removed {0}" -f $configPath) -Colour 'Green'
    }
    else {
        Write-CamHost -Message ("nothing to remove: {0} does not exist" -f $configPath) -Colour 'Yellow'
    }

    Write-CamHost -Message ("the binary at {0} is left in place" -f [string]$Config.RestreamerExePath)
    Write-CamHost -Message 'Set RestreamerEnabled to false in recorder.config.json to stop the supervisor trying to start it.'
    Write-CamHost -Message 'Recording and viewing continue on the direct camera inputs.' -Colour 'Cyan'
}


# ===========================================================================
# Autostart: a logon Scheduled Task for the supervisor
# ===========================================================================
# Registered through the ScheduledTasks module rather than schtasks.exe.
# Measured on this machine: schtasks /create /sc onlogon fails with "Access is
# denied" for a standard user, in the root folder and in a subfolder alike,
# while Register-ScheduledTask with a per-user LogonTrigger and an interactive,
# limited principal succeeds unelevated. Reading the task back with
# schtasks /query needs no elevation either way.

$script:CamAutostartTaskName = 'IPCCTV-Recorder'

# The Scheduled Task does not point at cam.recorder.ps1 directly. It points at a
# small generated launcher on a local Windows path, because the repo can live on
# \\wsl.localhost, which does not resolve until WSL's file server is up - and
# that is not true at logon. A task cannot wait for a path; the stub can.
$script:CamAutostartStubDir = Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IPCCTV'
$script:CamAutostartStubPath = Join-Path -Path $script:CamAutostartStubDir -ChildPath 'start-recorder.ps1'
$script:CamAutostartStubLogPath = Join-Path -Path $script:CamAutostartStubDir -ChildPath 'autostart.log'
# How long the stub waits for the recorder's path, and how often it looks.
$script:CamAutostartWaitSeconds = 300
$script:CamAutostartPollSeconds = 5
# Restart-on-failure for the task itself: a supervisor that dies should come
# back without waiting for the next logon.
$script:CamAutostartRestartCount = 3
$script:CamAutostartRestartMinutes = 1


function Get-CamAutostartTask {
    <# The registered task, or $null. Never throws. #>
    if (-not (Get-Command -Name 'Get-ScheduledTask' -ErrorAction SilentlyContinue)) { return $null }
    return (Get-ScheduledTask -TaskName $script:CamAutostartTaskName -ErrorAction SilentlyContinue |
        Select-Object -First 1)
}


function Get-CamAutostartAccount {
    <# DOMAIN\user for the account the task will run as - the current user. #>
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    if ($identity) { return $identity }
    return ("{0}\{1}" -f $env:USERDOMAIN, $env:USERNAME)
}


function New-CamAutostartStub {
    <#
        Writes the launcher the Scheduled Task runs, and returns its path.

        The stub polls for the real cam.recorder.ps1 until its path resolves,
        logs what happened, then invokes it. It FORWARDS, never copies: a local
        copy of the scripts would drift from the repo silently.

        Overwritten on every install, deleted by -RemoveAutostart, and not
        meant to be edited by hand.

        Exiting non-zero when the wait runs out is deliberate. It is what lets
        the task's restart-on-failure policy try the whole thing again, which is
        the right answer when WSL simply was not up yet.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$TargetScript
    )

    # Every substituted value lands inside a single-quoted literal in the
    # generated file, so a single quote in a path has to be doubled.
    $template = @'
# IPCCTV recorder autostart launcher - GENERATED FILE, do not edit.
# Written by cam.recorder.ps1 -InstallAutostart, deleted by -RemoveAutostart.
#
# Why it exists: the recorder can live on a path that is not there yet when the
# logon task fires (\\wsl.localhost resolves only once WSL is running). This
# waits for the path, then forwards to the real script. It holds no copy of it.

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

Write-StubLog ('target resolved after {0}s, starting the supervisor' -f $waited)

& $Target -Run
$code = $LASTEXITCODE
if ($null -eq $code) { $code = 0 }
Write-StubLog ('the supervisor exited with {0}' -f $code)
exit $code
'@

    $body = $template.
        Replace('__TARGET__', $TargetScript.Replace("'", "''")).
        Replace('__LOG__', $script:CamAutostartStubLogPath.Replace("'", "''")).
        Replace('__TIMEOUT__', [string]$script:CamAutostartWaitSeconds).
        Replace('__POLL__', [string]$script:CamAutostartPollSeconds)

    if (-not (Test-Path -LiteralPath $script:CamAutostartStubDir)) {
        New-Item -ItemType Directory -Path $script:CamAutostartStubDir -Force | Out-Null
    }
    Set-Content -LiteralPath $script:CamAutostartStubPath -Value $body -Encoding UTF8
    return $script:CamAutostartStubPath
}


function Remove-CamAutostartStub {
    <#
        Deletes the generated launcher. Returns $true if there was one.

        The launcher's log is deliberately left behind: it is the only record of
        a logon start that failed, and it is the first thing to read when
        autostart did not work. Its folder goes only when it is empty, so an
        install and removal with nothing logged leaves no trace at all.
    #>
    if (-not (Test-Path -LiteralPath $script:CamAutostartStubPath)) { return $false }
    Remove-Item -LiteralPath $script:CamAutostartStubPath -Force
    if (-not @(Get-ChildItem -LiteralPath $script:CamAutostartStubDir -Force -ErrorAction SilentlyContinue).Count) {
        Remove-Item -LiteralPath $script:CamAutostartStubDir -Force -ErrorAction SilentlyContinue
    }
    return $true
}


function Invoke-CamInstallAutostart {
    <#
        Registers a Scheduled Task that starts the supervisor at logon as the
        current user, with no elevation anywhere in the path.

        Idempotent: -Force replaces the registration rather than adding a
        second task, so running this twice leaves exactly one.

        Three settings are not defaults and are load-bearing for a 24/7
        recorder: ExecutionTimeLimit zero, because the default three-day limit
        would otherwise kill the supervisor on day three; MultipleInstances
        IgnoreNew, so a second logon cannot start a second supervisor competing
        for the same two camera slots; and restart-on-failure, so a supervisor
        that dies comes back in a minute instead of at the next logon.

        The task runs the generated launcher, not cam.recorder.ps1 itself. See
        New-CamAutostartStub for why.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    Write-CamHost -Message ''
    Write-CamHost -Message '=== install the logon autostart task ===' -Colour 'Cyan'

    if (-not (Get-Command -Name 'Register-ScheduledTask' -ErrorAction SilentlyContinue)) {
        throw 'The ScheduledTasks module is not available on this machine, so the task cannot be registered. Windows 8 or later provides it.'
    }

    $scriptPath = Join-Path -Path $PSScriptRoot -ChildPath 'cam.recorder.ps1'
    if (-not (Test-Path -LiteralPath $scriptPath)) {
        throw "Cannot find $scriptPath to point the task at."
    }

    $stubPath = New-CamAutostartStub -TargetScript $scriptPath

    $powershell = Join-Path -Path $env:SystemRoot -ChildPath 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $arguments = ('-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "{0}"' -f $stubPath)
    $account = Get-CamAutostartAccount

    $existing = Get-CamAutostartTask

    $action = New-ScheduledTaskAction -Execute $powershell -Argument $arguments
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $account
    $principal = New-ScheduledTaskPrincipal -UserId $account -LogonType Interactive -RunLevel Limited
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
        -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
        -RestartCount $script:CamAutostartRestartCount `
        -RestartInterval (New-TimeSpan -Minutes $script:CamAutostartRestartMinutes)

    $task = Register-ScheduledTask -TaskName $script:CamAutostartTaskName -Action $action `
        -Trigger $trigger -Principal $principal -Settings $settings `
        -Description 'Starts the IPCCTV continuous recorder at logon.' -Force

    if ($existing) {
        Write-CamHost -Message ("updated the existing task '{0}'" -f $script:CamAutostartTaskName) -Colour 'Green'
    }
    else {
        Write-CamHost -Message ("created the task '{0}'" -f $script:CamAutostartTaskName) -Colour 'Green'
    }

    Write-CamHost -Message ("  runs      {0} {1}" -f $powershell, $arguments)
    Write-CamHost -Message ("  launcher  {0}" -f $stubPath)
    Write-CamHost -Message ("  forwards  {0} -Run (waits up to {1}s for that path)" -f $scriptPath, $script:CamAutostartWaitSeconds)
    Write-CamHost -Message ("  log       {0}" -f $script:CamAutostartStubLogPath)
    Write-CamHost -Message ("  as        {0} (interactive token, no elevation)" -f $account)
    Write-CamHost -Message ("  trigger   at logon of {0}" -f $account)
    Write-CamHost -Message ("  limits    no execution time limit, a second instance is ignored, restart up to {0}x every {1} min on failure" -f $script:CamAutostartRestartCount, $script:CamAutostartRestartMinutes)
    Write-CamHost -Message ("  state     {0}" -f $task.State)
    Write-CamHost -Message ("  registered task(s) with this name: {0}" -f @(Get-ScheduledTask -TaskName $script:CamAutostartTaskName -ErrorAction SilentlyContinue).Count)

    if ($scriptPath.StartsWith('\\')) {
        Write-CamHost -Message ''
        Write-CamHost -Message ("WARNING: the recorder lives on a network path ({0})." -f $scriptPath) -Colour 'Yellow'
        Write-CamHost -Message '         A \\wsl.localhost path exists only while WSL is running, so at logon it may' -Colour 'Yellow'
        Write-CamHost -Message ("         not be there yet. The launcher polls for it for up to {0}s and the task" -f $script:CamAutostartWaitSeconds) -Colour 'Yellow'
        Write-CamHost -Message '         retries after that, but recording only starts once WSL is up. Nothing' -Colour 'Yellow'
        Write-CamHost -Message '         records while that path is unreachable, and the wait is logged at' -Colour 'Yellow'
        Write-CamHost -Message ("         {0}." -f $script:CamAutostartStubLogPath) -Colour 'Yellow'
    }

    Write-CamHost -Message ''
    Write-CamHost -Message 'Verify it with:  schtasks /query /tn IPCCTV-Recorder /fo list' -Colour 'Cyan'
    Write-CamHost -Message 'Remove it with:  cam.recorder.ps1 -RemoveAutostart' -Colour 'Cyan'
}


function Invoke-CamRemoveAutostart {
    <#
        Unregisters the logon task and deletes the generated launcher. A clean
        no-op when neither is there: Unregister-ScheduledTask throws on a
        missing task, so the task is looked up first.

        The launcher's log file is left behind on purpose - see
        Remove-CamAutostartStub.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    Write-CamHost -Message ''
    Write-CamHost -Message '=== remove the logon autostart task ===' -Colour 'Cyan'

    $existing = Get-CamAutostartTask
    if ($existing) {
        Unregister-ScheduledTask -TaskName $script:CamAutostartTaskName -Confirm:$false
        $left = @(Get-ScheduledTask -TaskName $script:CamAutostartTaskName -ErrorAction SilentlyContinue).Count
        if ($left -ne 0) {
            throw "Unregistered '$($script:CamAutostartTaskName)' but $left task(s) with that name are still registered."
        }
        Write-CamHost -Message ("removed the task '{0}'" -f $script:CamAutostartTaskName) -Colour 'Green'
    }
    else {
        Write-CamHost -Message ("no task named '{0}' is registered" -f $script:CamAutostartTaskName) -Colour 'Yellow'
    }

    if (Remove-CamAutostartStub) {
        Write-CamHost -Message ("removed the launcher '{0}'" -f $script:CamAutostartStubPath) -Colour 'Green'
    }
    else {
        Write-CamHost -Message ("no launcher at '{0}'" -f $script:CamAutostartStubPath) -Colour 'Yellow'
    }

    if (Test-Path -LiteralPath $script:CamAutostartStubLogPath) {
        Write-CamHost -Message ("kept the launcher log '{0}' - delete it by hand if you do not want it" -f $script:CamAutostartStubLogPath)
    }

    if (-not $existing) { return }

    Write-CamHost -Message 'A supervisor that is running now keeps running. Stop it with -Stop.'
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


function Test-CamRestreamer {
    <#
        Whether the relay is installed, current and serving. Touches no camera -
        the only network call is to the loopback control API - so it runs in
        -Offline mode too.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    if (-not (Get-CamRelayEnabled -Config $Config)) {
        Add-CamTestResult -Check 'restreamer' -Status 'INFO' -Detail 'RestreamerEnabled is false: the recorder and the dashboard use the direct camera inputs'
        return $false
    }

    $exePath = [string]$Config.RestreamerExePath
    $configPath = [string]$Config.RestreamerConfigPath

    $exeOk = Test-Path -LiteralPath $exePath
    Add-CamTestResult -Check 'restreamer binary' -Status $(if ($exeOk) { 'PASS' } else { 'FAIL' }) `
        -Detail $(if ($exeOk) { $exePath } else { "missing at $exePath - download it or set RestreamerEnabled to false" })

    $pathMap = $null
    try { $pathMap = Get-CamRelayPathMap -Cameras (Get-CamList) }
    catch {
        Add-CamTestResult -Check 'restreamer paths' -Status 'FAIL' -Detail $_.Exception.Message
        return $false
    }
    Add-CamTestResult -Check 'restreamer paths' -Status 'PASS' -Detail (($pathMap.Keys | Sort-Object) -join ', ')

    if (-not (Test-Path -LiteralPath $configPath)) {
        Add-CamTestResult -Check 'restreamer config' -Status 'FAIL' -Detail "missing at $configPath - run -InstallRestreamer"
    }
    else {
        $expected = Get-CamRelayConfigText -Config $Config -PathMap $pathMap
        $actual = ''
        try { $actual = [System.IO.File]::ReadAllText($configPath) } catch { }
        $current = ($actual -eq $expected)
        Add-CamTestResult -Check 'restreamer config' -Status $(if ($current) { 'PASS' } else { 'FAIL' }) `
            -Detail $(if ($current) { "$configPath matches cameras.json" } else { "$configPath is out of date with cameras.json - run -InstallRestreamer" })

        # Only the password could leak here, and it is never written, so this
        # asserts the shape the generated file must always have.
        $hasUserinfo = [regex]::IsMatch($actual, '(?i)rtsp://[^/\s@]+@')
        Add-CamTestResult -Check 'restreamer config has no credential' -Status $(if ($hasUserinfo) { 'FAIL' } else { 'PASS' }) `
            -Detail $(if ($hasUserinfo) { 'a URL with userinfo is in the generated config' } else { 'no rtsp:// userinfo in the generated config' })
    }

    $ready = @(Get-CamRelayReadyPaths -Config $Config)
    if ($ready.Count -eq 0) {
        Add-CamTestResult -Check 'restreamer serving' -Status 'INFO' -Detail ("not reachable at {0}: consumers will use the direct camera inputs" -f [string]$Config.RelayApiBase)
    }
    else {
        Add-CamTestResult -Check 'restreamer serving' -Status 'INFO' -Detail ("{0} of {1} path(s) ready: {2}" -f $ready.Count, $pathMap.Count, (($ready | Sort-Object) -join ', '))
    }

    return $true
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

        Write-CamHost -Message '(f) restreamer' -Colour 'Cyan'
        [void](Test-CamRestreamer -Config $Config)

        Write-CamHost -Message '(g) camera list' -Colour 'Cyan'
        [void](Test-CamListIntegrity -Config $Config)

        Write-CamHost -Message '(h) Record flag round trip (on a copy)' -Colour 'Cyan'
        [void](Test-CamRecordRoundTrip -Config $Config -WorkDir $workDir)

        Write-CamHost -Message '(i) atomic camera-list write' -Colour 'Cyan'
        [void](Test-CamAtomicWrite -WorkDir $workDir)

        Write-CamHost -Message '(j) pruner path guards' -Colour 'Cyan'
        [void](Test-CamPruneGuards -Config $Config)

        Write-CamHost -Message '(k) pruner dry run' -Colour 'Cyan'
        [void](Show-CamPruneDryRun -Config $Config -WorkDir $workDir)

        Write-CamHost -Message '(l) status file' -Colour 'Cyan'
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

function New-CamProcessEntry {
    <#
        The fields the supervisor's start, read, backoff and stop machinery
        touches, shared by the ffmpeg children and the restreamer child so both
        are driven by exactly the same discipline.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$LogPath
    )

    return @{
        Name         = $Name
        LogPath      = $LogPath
        Process      = $null
        # The pipe Read-CamChildOutput drains: ffmpeg's stderr, MediaMTX's stdout.
        ReadStream   = $null
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
        # tell an orphan from a child that shut down with its parent.
        LastPid      = $null
        # Restart timestamps, so status.json can report restarts in the last hour
        # rather than only a total that never decays.
        Restarts     = New-Object System.Collections.ArrayList
        LastError    = ''
        LastErrorAt  = [DateTime]::MinValue
    }
}


function New-CamChildEntry {
    param(
        [Parameter(Mandatory = $true)]$Camera,
        [Parameter(Mandatory = $true)][string]$CameraDir,
        [Parameter(Mandatory = $true)][string]$LogPath,
        [Parameter(Mandatory = $true)][string]$Pattern,
        [AllowEmptyString()][string]$RelayPathName = ''
    )

    $entry = New-CamProcessEntry -Name $Camera.Name -LogPath $LogPath

    $entry.SafeName = $Camera.SafeName
    # Kept so a reconcile can tell a changed stream from an unchanged one
    # without comparing URLs, which carry the password.
    $entry.IP = $Camera.IP
    $entry.Quality = $Camera.Quality
    # The direct camera URL, with credentials. The relay URL has none and is
    # rebuilt from RelayPath at every start.
    $entry.Url = $Camera.Url
    $entry.RelayPath = $RelayPathName
    # The bounded wait for this camera's OWN relay path, spent across the
    # supervisor's cycles rather than blocking inside one - which is what
    # keeps an offline camera, whose path can never become ready, from holding
    # up every other child. FIRST START ONLY: a restart has to fall straight
    # through to direct, because that is what leaves a relay that died mid-run
    # behind instead of retrying against a dead address.
    $entry.RelayWait = ($RelayPathName -ne '')
    $entry.RelayWaitUntil = $null
    # Which input the most recent start actually used, for the log and for
    # status.json. Empty until the child has been started once.
    $entry.InputMode = ''
    $entry.Dir = $CameraDir
    $entry.Pattern = $Pattern
    $entry.Scan = $null

    return $entry
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
        [Parameter(Mandatory = $true)][string]$SupervisorLog,
        # The serving set the supervisor's cycle fetched ONCE for every child.
        # Null means ask for this one path, which is what a lone restart
        # between cycles wants.
        [AllowNull()][string[]]$ReadyPaths
    )

    # Chosen here rather than at reconcile time, so every restart re-decides:
    # a child started while the restreamer was down moves onto the relay at its
    # next restart, and one started on the relay goes direct if it has gone.
    $choiceArgs = @{ Config = $Config; RelayPathName = $Entry.RelayPath; DirectUrl = $Entry.Url }
    if ($null -ne $ReadyPaths) { $choiceArgs['ReadyPaths'] = @($ReadyPaths) }
    $choice = Get-CamInputUrl @choiceArgs
    $Entry.InputMode = $choice.Mode

    $arguments = Get-CamFfmpegArgs -Url $choice.Url -OutputPattern $Entry.Pattern -Config $Config -ContainerProfile $ContainerProfile

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
    $Entry.ReadStream = $process.StandardError.BaseStream
    $Entry.ReadTask = $Entry.ReadStream.ReadAsync($Entry.ReadBuffer, 0, $Entry.ReadBuffer.Length)

    Write-CamLog -Path $SupervisorLog -Message ("start {0} pid {1} on the {2} input: ffmpeg {3}" -f $Entry.Name, $process.Id, $choice.Mode, $startInfo.Arguments) -MaxBytes $Config.LogMaxBytes
    Write-CamHost -Message ("start {0} pid {1} on the {2} input" -f $Entry.Name, $process.Id, $choice.Mode) -Colour 'Green'
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


function Read-CamChildOutput {
    <#
        Drains whatever the child has written since the last tick, without
        blocking, and writes every complete line to that child's own log through
        the redaction helper. ffmpeg prints the full RTSP URL - password included
        - on a connection error, so redaction here is not optional.

        The stream is whichever one the starter put on the entry: ffmpeg logs to
        stderr, MediaMTX to stdout.
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
        if (-not $Entry.ReadStream) { $Entry.ReadTask = $null; break }

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

        $Entry.ReadTask = $Entry.ReadStream.ReadAsync($Entry.ReadBuffer, 0, $Entry.ReadBuffer.Length)
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

    Read-CamChildOutput -Entry $Entry -Config $Config -Final
    try { $process.Dispose() } catch { }

    $Entry.Process = $null
    $Entry.ReadTask = $null
    $Entry.ReadStream = $null

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
# Restreamer child: MediaMTX as a transport fan-out
# ===========================================================================
# Owned by the supervisor as a child rather than by a Scheduled Task of its
# own, so there is one thing to autostart and one place that logs. Every
# consumer falls back to a direct camera connection while it is down, so its
# downtime is degraded-but-working rather than an outage.

function New-CamRelayEntry {
    <# The restreamer's supervisor entry. One per supervisor, not per camera. #>
    param([Parameter(Mandatory = $true)]$Paths)

    $entry = New-CamProcessEntry -Name 'restreamer' -LogPath (Join-Path -Path $Paths.LogDir -ChildPath 'restreamer.log')
    # Path name -> camera, read from cameras.json at every start so an
    # -Enable or an IP change is picked up by the next restart.
    $entry.PathMap = $null
    return $entry
}


function Start-CamRelayChild {
    <#
        Launches MediaMTX with one path per camera in cameras.json - every
        camera, not only the recorded ones, because the dashboard shares the
        same relay.

        Each camera URL is handed over in the child's environment block
        (MTX_PATHS_<PATHNAME>_SOURCE), never in the config file and never on the
        command line. That is the whole credential win: one readable copy in one
        process's environment instead of one per consumer command line.

        stdout is the log, piped line by line through the redactor, because
        MediaMTX prints the source URL on a connection error. stderr is left
        redirected but unread until exit: MediaMTX logs nothing there, and the
        only thing that would arrive is a Go panic, which is read in
        Write-CamRelayStderr once the process is gone and ReadToEnd cannot
        block.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$Credential,
        [Parameter(Mandatory = $true)][string]$SupervisorLog
    )

    $exePath = [string]$Config.RestreamerExePath
    $configPath = [string]$Config.RestreamerConfigPath

    if (-not (Test-Path -LiteralPath $exePath)) {
        throw "MediaMTX is not installed at '$exePath'. Install the binary there, or set RestreamerEnabled to false in recorder.config.json to record and view directly."
    }
    if (-not (Test-Path -LiteralPath $configPath)) {
        throw "MediaMTX config '$configPath' is missing. Run cam.recorder.ps1 -InstallRestreamer to generate it from cameras.json."
    }

    $pathMap = Get-CamRelayPathMap -Cameras (Get-CamList)
    $Entry.PathMap = $pathMap

    # The generated config is rendered from cameras.json, so re-rendering it is
    # the whole staleness test - no YAML parsing, and one source of truth. A
    # camera missing from the file would come up with no path at all and would
    # quietly stay on the direct input, so this is said loudly and started
    # anyway: a stale relay still serves the cameras it does know.
    $expected = Get-CamRelayConfigText -Config $Config -PathMap $pathMap
    $actual = ''
    try { $actual = [System.IO.File]::ReadAllText($configPath) } catch { }
    if ($actual -ne $expected) {
        $message = "WARNING: '$configPath' no longer matches cameras.json. Run cam.recorder.ps1 -InstallRestreamer. Starting the restreamer with the file as it is."
        Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $Config.LogMaxBytes
        Write-CamHost -Message $message -Colour 'Yellow'
    }

    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $exePath
    $startInfo.Arguments = Get-CamCommandLine -Arguments @($configPath)
    $startInfo.WorkingDirectory = (Split-Path -Path $exePath -Parent)
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true

    foreach ($pathName in $pathMap.Keys) {
        $url = Get-CamRtspUrl -Camera $pathMap[$pathName] -Credential $Credential
        $startInfo.EnvironmentVariables[(Get-CamRelaySourceEnvName -PathName $pathName)] = $url
    }

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
    $Entry.ReadStream = $process.StandardOutput.BaseStream
    $Entry.ReadTask = $Entry.ReadStream.ReadAsync($Entry.ReadBuffer, 0, $Entry.ReadBuffer.Length)

    $message = "start restreamer pid {0}: {1} serving {2} path(s): {3}" -f `
        $process.Id, $exePath, $pathMap.Count, (($pathMap.Keys | Sort-Object) -join ', ')
    Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $Config.LogMaxBytes
    Write-CamHost -Message ("start restreamer pid {0} serving {1} path(s)" -f $process.Id, $pathMap.Count) -Colour 'Green'

    # Block here, bounded, until the control API answers. This is what stops
    # the recorders taking a direct camera slot each at a cold start: a camera
    # already serving the dashboard has exactly one slot left, and if a recorder
    # takes it the restreamer can never pull that camera at all, which would
    # leave the whole arrangement stuck on direct inputs. Blocking is safe here
    # because nothing else in the supervisor is due in the same instant.
    #
    # It waits for the API, NOT for every path to be serving. A camera that is
    # simply offline has a path that can never become ready, so waiting for the
    # whole set spent the entire budget on every cold start and delayed all the
    # healthy recorders behind the broken one. Each child waits for its own path
    # instead, in the supervisor's loop, where waiting costs nothing.
    $waitSeconds = 20
    try { $waitSeconds = [int]$Config.RelayStartupWaitSeconds } catch { }
    if ($waitSeconds -gt 0 -and $pathMap.Count -gt 0) {
        $deadline = [DateTime]::Now.AddSeconds($waitSeconds)
        $answered = $false
        while ([DateTime]::Now -lt $deadline) {
            if ($process.HasExited) { break }
            if ($null -ne (Get-CamRelayApiJson -Config $Config -Route 'v3/paths/list')) { $answered = $true; break }
            Start-Sleep -Milliseconds 500
        }
        $ready = @()
        if ($answered) { $ready = @(Get-CamRelayReadyPaths -Config $Config) }
        $waited = ([DateTime]::Now - $Entry.StartedAt).TotalSeconds
        $message = "restreamer control API {0} after {1}s, serving {2} of {3} path(s): {4}" -f `
            $(if ($answered) { 'answered' } else { 'did not answer' }), `
            (Format-CamNumber -Value $waited -Format '0.0'), `
            $ready.Count, $pathMap.Count, (($ready | Sort-Object) -join ', ')
        Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $Config.LogMaxBytes
        Write-CamHost -Message $message -Colour 'Cyan'
    }
}


function Write-CamRelayStderr {
    <#
        Drains the restreamer's stderr after it has exited. Safe to call only
        then: ReadToEnd on a live child would block the supervisor.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config
    )

    $process = $Entry.Process
    if (-not $process) { return }
    if (-not $process.HasExited) { return }

    $text = ''
    try { $text = $process.StandardError.ReadToEnd() } catch { return }
    if ([string]::IsNullOrWhiteSpace($text)) { return }

    foreach ($line in ($text -split "`r?`n")) {
        if ($line.Trim().Length -eq 0) { continue }
        Write-CamLog -Path $Entry.LogPath -Message ("stderr: " + $line) -MaxBytes $Config.LogMaxBytes
    }
}


function Stop-CamRelayChild {
    <#
        Hard-kills the restreamer. Nothing is lost by a hard kill: it holds no
        file and writes nothing to disk. Camera children are stopped before this
        is called, so no recorder is left reading a relay that is going away.
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
        $message = "kill restreamer pid {0}: {1}" -f $process.Id, $Reason
        Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $Config.LogMaxBytes
        Write-CamHost -Message $message -Colour 'Yellow'
        try { $process.Kill() } catch { }
        [void]$process.WaitForExit(10000)
    }

    Read-CamChildOutput -Entry $Entry -Config $Config -Final
    Write-CamRelayStderr -Entry $Entry -Config $Config
    try { $process.Dispose() } catch { }

    $Entry.Process = $null
    $Entry.ReadTask = $null
    $Entry.ReadStream = $null
}


function Invoke-CamRelayTick {
    <#
        One supervisor cycle for the restreamer: drain its log, notice an exit,
        restart it on the same backoff the camera children use.

        No stall watchdog here. A camera child needs one because a wedged ffmpeg
        keeps running while writing nothing, which only the archive can reveal.
        The restreamer needs none, because every consumer asks the control API
        whether its own path is serving before each attempt and goes direct when
        the answer is no.
    #>
    param(
        [Parameter(Mandatory = $true)]$Entry,
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$Credential,
        [Parameter(Mandatory = $true)][string]$SupervisorLog,
        [Parameter(Mandatory = $true)][DateTime]$Now
    )

    $maxBytes = [int]$Config.LogMaxBytes

    if ($Entry.Process) {
        Read-CamChildOutput -Entry $Entry -Config $Config
        if (-not $Entry.Process.HasExited) { return }

        $exitCode = $Entry.Process.ExitCode
        $ranFor = ($Now - $Entry.StartedAt).TotalSeconds
        Read-CamChildOutput -Entry $Entry -Config $Config -Final
        Write-CamRelayStderr -Entry $Entry -Config $Config
        try { $Entry.Process.Dispose() } catch { }
        $Entry.Process = $null
        $Entry.ReadTask = $null
        $Entry.ReadStream = $null

        $Entry.RestartCount = $Entry.RestartCount + 1
        [void]$Entry.Restarts.Add($Now)
        $Entry.LastError = "mediamtx exited with code $exitCode after $([int]$ranFor)s"
        $Entry.LastErrorAt = $Now

        $wait = Set-CamChildBackoff -Entry $Entry -Config $Config -RanForSeconds $ranFor
        $message = "restreamer exit {0} after {1}s; restart #{2} in {3}s" -f `
            $exitCode, [int]$ranFor, $Entry.RestartCount, $wait
        Write-CamLog -Path $SupervisorLog -Message $message -MaxBytes $maxBytes
        Write-CamHost -Message $message -Colour 'Yellow'
        return
    }

    if ($Now -lt $Entry.NextStartAt) { return }

    try {
        Start-CamRelayChild -Entry $Entry -Config $Config -Credential $Credential -SupervisorLog $SupervisorLog
    }
    catch {
        $Entry.LastError = "failed to start: $($_.Exception.Message)"
        $Entry.LastErrorAt = $Now
        $wait = Set-CamChildBackoff -Entry $Entry -Config $Config -RanForSeconds 0
        Write-CamLog -Path $SupervisorLog -Message ("failed to start the restreamer: {0}; retry in {1}s. Consumers stay on the direct camera inputs until it is up." -f $_.Exception.Message, $wait) -MaxBytes $maxBytes
        Write-CamHost -Message ("failed to start the restreamer: {0}; retry in {1}s" -f $_.Exception.Message, $wait) -Colour 'Red'
    }
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
        $PruneState,
        $Relay,
        # The supervisor log. Its tail is published as the Events list so the
        # health strip can show recent restarts, watchdog kills, prune
        # deletions and failures without opening a second file. Every line in
        # that log is already timestamped and redacted, and the log is only
        # written when something happens, so its tail IS the event stream.
        [string]$SupervisorLogPath
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
            InputMode                     = [string]$(if ($entry) { $entry.InputMode } else { '' })
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

    # Reported even when the restreamer is off, so a reader can tell "disabled"
    # from "should be running and is not". LastPid is what -Stop uses to prove
    # it left no mediamtx behind holding a session on every camera.
    $restreamer = [pscustomobject]@{
        Enabled       = (Get-CamRelayEnabled -Config $Config)
        RelayBase     = [string]$Config.RelayBase
        Pid           = $null
        LastPid       = $null
        Alive         = $false
        StartedAt     = $null
        RestartsTotal = 0
        Paths         = @()
        LastError     = ''
        LastErrorAt   = $null
    }
    if ($Relay) {
        $relayAlive = $false
        if ($Relay.Process) { $relayAlive = (-not $Relay.Process.HasExited) }
        $restreamer = [pscustomobject]@{
            Enabled       = $true
            RelayBase     = [string]$Config.RelayBase
            Pid           = $(if ($relayAlive) { $Relay.Process.Id } else { $null })
            LastPid       = $Relay.LastPid
            Alive         = $relayAlive
            StartedAt     = Format-CamTimestamp -Value $Relay.StartedAt
            RestartsTotal = $Relay.RestartCount
            Paths         = @($(if ($Relay.PathMap) { @($Relay.PathMap.Keys) } else { @() }))
            LastError     = Get-CamRedacted -Text ([string]$Relay.LastError)
            LastErrorAt   = Format-CamTimestamp -Value $Relay.LastErrorAt
        }
    }

    # Newest first: the strip has room for a handful of lines and the newest
    # are the ones worth the space.
    $events = @()
    $eventCount = [int]$Config.StatusEventCount
    if ($eventCount -gt 0 -and $SupervisorLogPath) {
        $tail = @(Get-CamFileTailLines -Path $SupervisorLogPath -Count $eventCount)
        for ($i = $tail.Count - 1; $i -ge 0; $i--) {
            $line = [string]$tail[$i]
            $at = $null
            $text = $line
            # "yyyy-MM-dd HH:mm:ss zzz message", the shape Write-CamLog emits.
            if ($line -match '^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{2}:\d{2}) (.*)$') {
                $at = $Matches[1]
                $text = $Matches[2]
            }
            $events += [pscustomobject]@{
                At   = $at
                Text = Get-CamRedacted -Text $text
            }
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
        Restreamer    = $restreamer
        Events        = $events
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

    # The same promise for the restreamer, and it matters more: one surviving
    # mediamtx holds an RTSP session on every camera at once.
    if ($final -and $final.PSObject.Properties['Restreamer'] -and $final.Restreamer.LastPid) {
        $relayPid = [int]$final.Restreamer.LastPid
        $relayProcess = Get-Process -Id $relayPid -ErrorAction SilentlyContinue
        if ($relayProcess -and $relayProcess.Name -eq 'mediamtx') {
            Write-CamHost -Message ("ORPHAN mediamtx pid {0} survived the shutdown; killing it" -f $relayPid) -Colour 'Red'
            try { $relayProcess.Kill() } catch { }
        }
        else {
            Write-CamHost -Message 'no mediamtx left behind' -Colour 'Green'
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
        -Pattern (Get-CamSegmentPattern -CameraDir $cameraDir -SafeName $Camera.SafeName -ContainerProfile $ContainerProfile) `
        -RelayPathName (Get-CamRelayPathName -SafeName $Camera.SafeName)
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

    # Batch-startup tuning, read once: how long a child waits for its own relay
    # path before taking a direct slot, and the gap left between consecutive
    # child starts.
    $relayReadyWait = 15
    try { $relayReadyWait = [int]$Config.RelayReadyWaitSeconds } catch { }
    if ($relayReadyWait -lt 0) { $relayReadyWait = 0 }
    $staggerMs = 300
    try { $staggerMs = [int]$Config.StartStaggerMs } catch { }
    if ($staggerMs -lt 0) { $staggerMs = 0 }

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

    # The restreamer is a supervisor child like any other: one thing to
    # autostart, one place that logs, the same backoff discipline. Every
    # consumer falls back to a direct camera connection while it is down.
    $relay = $null
    if (Get-CamRelayEnabled -Config $Config) { $relay = New-CamRelayEntry -Paths $paths }

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

            if ($relay) {
                Invoke-CamRelayTick -Entry $relay -Config $Config -Credential $credential `
                    -SupervisorLog $supervisorLog -Now $now
                # A relay start waits for its paths, so the clock has moved.
                $now = [DateTime]::Now
            }

            $statusDue = (($now - $lastStatus).TotalSeconds -ge [double]$Config.StatusIntervalSeconds)

            # ONE control-API list query per cycle, shared by every child,
            # rather than one probe per camera. Asked on a status tick, and
            # also whenever a child is due to start: six children deciding
            # relay-or-direct used to fire six near-simultaneous probes, and a
            # single flake among them put that camera on a direct connection
            # for its whole run.
            $pendingStart = $false
            foreach ($candidate in @($children.Values)) {
                if (-not $candidate.Process -and $now -ge $candidate.NextStartAt) { $pendingStart = $true; break }
            }
            $relayReady = $null
            if ($relay -and ($statusDue -or $pendingStart)) { $relayReady = @(Get-CamRelayReadyPaths -Config $Config) }

            $startedThisCycle = 0

            foreach ($entry in @($children.Values)) {
                # One directory scan per cycle, shared by the status file and the
                # stall watchdog.
                if ($statusDue) { $entry.Scan = Get-CamDirScan -Path $entry.Dir }

                if ($entry.Process) {
                    Read-CamChildOutput -Entry $entry -Config $Config

                    if ($entry.Process.HasExited) {
                        $exitCode = $entry.Process.ExitCode
                        $ranFor = ($now - $entry.StartedAt).TotalSeconds
                        Read-CamChildOutput -Entry $entry -Config $Config -Final
                        try { $entry.Process.Dispose() } catch { }
                        $entry.Process = $null
                        $entry.ReadTask = $null
                        $entry.ReadStream = $null
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
                        # A child that had to start on its direct input moves
                        # onto the relay as soon as the relay is serving it,
                        # which is what gives back the second camera slot after
                        # a cold start or a restreamer outage. The other
                        # direction needs no code: when the relay goes away its
                        # reader loses the input, ffmpeg exits, and the restart
                        # picks direct.
                        if ($null -ne $relayReady -and $entry.InputMode -eq 'direct' -and `
                                $entry.RelayPath -and ($relayReady -contains $entry.RelayPath)) {
                            $reason = 'the restreamer is now serving this camera, moving it off the direct input'
                            Stop-CamChild -Entry $entry -Config $Config -SupervisorLog $supervisorLog -Reason $reason
                            # Not a failure, so neither the restart count nor
                            # the backoff is touched - the next tick starts it.
                            $entry.NextStartAt = $now
                            Write-CamLog -Path $supervisorLog -Message ("{0}: {1}" -f $entry.Name, $reason) -MaxBytes $maxBytes
                            continue
                        }

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
                        # A camera starting for the FIRST time waits, bounded,
                        # for its own relay path to come up before it decides.
                        # Waiting here rather than inside the decision costs
                        # nothing and blocks nobody: this cycle simply moves on
                        # to the next child, so the five healthy cameras start
                        # while the offline one is still being waited for.
                        if ($relay -and $entry.RelayWait) {
                            if ($null -eq $entry.RelayWaitUntil) {
                                $entry.RelayWaitUntil = $now.AddSeconds($relayReadyWait)
                            }
                            $pathServing = ($null -ne $relayReady -and @($relayReady) -contains $entry.RelayPath)
                            if (-not $pathServing -and $now -lt $entry.RelayWaitUntil) { continue }
                            if (-not $pathServing) {
                                $message = "{0}: the restreamer is still not serving '{1}' after {2}s, starting it on the direct input" -f `
                                    $entry.Name, $entry.RelayPath, $relayReadyWait
                                Write-CamLog -Path $supervisorLog -Message $message -MaxBytes $maxBytes
                                Write-CamHost -Message $message -Colour 'Yellow'
                            }
                            $entry.RelayWait = $false
                        }

                        # Stagger: six ffmpeg children opening their inputs in
                        # the same instant is a burst the relay and the cameras
                        # do not need, and the Operator asked for the spacing.
                        if ($startedThisCycle -gt 0 -and $staggerMs -gt 0) {
                            Start-Sleep -Milliseconds $staggerMs
                            $now = [DateTime]::Now
                        }
                        $startedThisCycle = $startedThisCycle + 1

                        try {
                            Start-CamChild -Entry $entry -Config $Config -ContainerProfile $containerProfile `
                                -SupervisorLog $supervisorLog -ReadyPaths $relayReady
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
                        -StartedAt $startedAt -State 'running' -Archive $archive -PruneState $pruneState -Relay $relay `
                        -SupervisorLogPath $supervisorLog
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

        # After the recorders, never before: a relay torn down first would make
        # every child lose its input and log a failure on the way out. A
        # surviving mediamtx would hold a session on all six cameras.
        if ($relay) {
            try { Stop-CamRelayChild -Entry $relay -Config $Config -SupervisorLog $supervisorLog -Reason $reason }
            catch { Write-CamLog -Path $supervisorLog -Message ("failed to stop the restreamer: {0}" -f $_.Exception.Message) -MaxBytes $maxBytes }
        }

        # A final status file, so -Status and the health strip can say "stopped
        # cleanly at T" instead of showing a snapshot they cannot account for.
        try {
            Write-CamStatusFile -Config $Config -Path $statusPath -Cameras $cameras -Children $children `
                -StartedAt $startedAt -State 'stopped' -Archive $archive -PruneState $pruneState -Relay $relay `
                -SupervisorLogPath $supervisorLog
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
elseif ($InstallRestreamer) {
    Invoke-CamInstallRestreamer -Config $config
}
elseif ($RemoveRestreamer) {
    Invoke-CamRemoveRestreamer -Config $config
}
elseif ($InstallAutostart) {
    Invoke-CamInstallAutostart -Config $config
}
elseif ($RemoveAutostart) {
    Invoke-CamRemoveAutostart -Config $config
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
    Write-CamHost -Message '  cam.recorder.ps1 -InstallRestreamer    generate the MediaMTX config from cameras.json'
    Write-CamHost -Message '  cam.recorder.ps1 -RemoveRestreamer     delete it again'
    Write-CamHost -Message '  cam.recorder.ps1 -InstallAutostart     run -Run at logon as this user'
    Write-CamHost -Message '  cam.recorder.ps1 -RemoveAutostart      remove that logon task'
}
