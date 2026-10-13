<#
.SYNOPSIS
    One place to start, stop and inspect everything: the recorder, the relay,
    the six viewer panes, their mpv and the health strip.

.DESCRIPTION
    These cameras serve exactly TWO concurrent RTSP sessions each. Every
    process holding a session that nobody can find is a slot lost until it is
    hunted down in Task Manager, so there has to be one command that accounts
    for all of them and one command that stops all of them.

    -Stop leaves nothing behind, from any component, including orphans from a
    previous crash that this script never started. It is a no-op on a machine
    where nothing is running and it is safe to run twice.

    Nothing here is identified by image name or window title. Both have
    already cost this project something real: killing by window title took
    out the Operator's own terminal, because Windows Terminal serves every
    window from ONE process, and killing by image name would have stopped the
    24/7 recording supervisor, which is a powershell.exe indistinguishable
    from a pane. Every process is classified by the work it owns -
    Get-CamProcessInventory in cam.common.ps1 - and the recording supervisor
    and its children sit on a keep list that every stop here honours.

.PARAMETER Status
    What is running, in one view. The default when no action is given.

.PARAMETER Start
    Brings the recorder up, waits for the relay to serve, then opens the
    dashboard. That order matters: a pane that decides before the relay is
    serving takes a direct camera connection and burns the spare session the
    relay exists to keep free.

.PARAMETER Stop
    Stops everything, and verifies that it did.

.PARAMETER Recorder
    Limits the action to the recorder and MediaMTX.

.PARAMETER Dashboard
    Limits the action to the viewer panes, their mpv and the health strip.

.PARAMETER InstallDashboardAutostart
    Registers a logon task that opens the dashboard, and leaves the
    recorder's own task completely alone. Idempotent, unelevated.

.PARAMETER RemoveDashboardAutostart
    Removes that task and its launcher. A clean no-op when they are not
    there, and it never touches the recorder's task either.

.EXAMPLE
    .\cam.ps1
    .\cam.ps1 -Stop
    .\cam.ps1 -Stop -Dashboard
    .\cam.ps1 -Start
    .\cam.ps1 -InstallDashboardAutostart
#>
# The bare invocation is -Status, which needs saying explicitly: with several
# parameter sets and no mandatory parameter in any of them, PowerShell cannot
# pick one from no arguments at all and fails with "parameter set cannot be
# resolved" instead. Measured on 2026-10-12 - .\cam.ps1 had never worked.
[CmdletBinding(DefaultParameterSetName = 'Status')]
param(
    [Parameter(ParameterSetName = 'Status')]
    [switch]$Status,

    [Parameter(ParameterSetName = 'Start')]
    [switch]$Start,

    [Parameter(ParameterSetName = 'Stop')]
    [switch]$Stop,

    [Parameter(ParameterSetName = 'InstallDashboardAutostart')]
    [switch]$InstallDashboardAutostart,

    [Parameter(ParameterSetName = 'RemoveDashboardAutostart')]
    [switch]$RemoveDashboardAutostart,

    # Scope. Neither given means everything, which is the point of the script.
    [Parameter(ParameterSetName = 'Start')]
    [Parameter(ParameterSetName = 'Stop')]
    [Parameter(ParameterSetName = 'Status')]
    [switch]$Recorder,

    [Parameter(ParameterSetName = 'Start')]
    [Parameter(ParameterSetName = 'Stop')]
    [Parameter(ParameterSetName = 'Status')]
    [switch]$Dashboard
)

$ErrorActionPreference = 'Stop'

$ScriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { $PWD.Path }
. (Join-Path -Path $ScriptDir -ChildPath 'cam.common.ps1')

$RecorderScript = Join-Path -Path $ScriptDir -ChildPath 'cam.recorder.ps1'
$DashboardScript = Join-Path -Path $ScriptDir -ChildPath 'mpv.cam.autolayout.autorestart.ps1'

# Two logon tasks, one per side. The recorder's is registered and removed by
# cam.recorder.ps1 -InstallAutostart / -RemoveAutostart and is NEVER touched
# from here; this script only reads its state and runs it. The dashboard's is
# registered and removed here.
$RecorderTaskName = 'IPCCTV-Recorder'
$DashboardTaskName = 'IPCCTV-Dashboard'
$DashboardStubPath = Join-Path -Path (Get-CamAutostartDir) -ChildPath 'start-dashboard.ps1'
$DashboardStubLogPath = Join-Path -Path (Get-CamAutostartDir) -ChildPath 'autostart-dashboard.log'
# How long the launcher waits for this script's path, and how often it looks.
$DashboardStubWaitSeconds = 300
$DashboardStubPollSeconds = 5

# Neither scope switch means both, so the bare verb always means everything.
$WantRecorder = $Recorder -or -not ($Recorder -or $Dashboard)
$WantDashboard = $Dashboard -or -not ($Recorder -or $Dashboard)

$Config = Get-CamConfig


function Write-CamSection {
    param([Parameter(Mandatory = $true)][string]$Title)
    Write-Host ''
    Write-Host ("=== {0} ===" -f $Title) -ForegroundColor Cyan
}


function Get-CamLogonTaskReport {
    <#
        Both logon tasks, whether each is registered, and the launcher it
        points at. Reads only - nothing here registers or removes anything.
    #>

    $rows = @(
        [pscustomobject]@{
            Name      = $RecorderTaskName
            What      = 'the 24/7 recorder'
            Launcher  = (Join-Path -Path (Get-CamAutostartDir) -ChildPath 'start-recorder.ps1')
            InstallBy = '.\cam.recorder.ps1 -InstallAutostart'
        },
        [pscustomobject]@{
            Name      = $DashboardTaskName
            What      = 'the viewer dashboard'
            Launcher  = $DashboardStubPath
            InstallBy = '.\cam.ps1 -InstallDashboardAutostart'
        }
    )

    foreach ($row in $rows) {
        $task = Get-CamLogonTask -TaskName $row.Name
        $state = 'not registered'
        if ($task) { $state = [string]$task.State }
        Add-Member -InputObject $row -NotePropertyName 'Registered' -NotePropertyValue ([bool]$task)
        Add-Member -InputObject $row -NotePropertyName 'State' -NotePropertyValue $state
        Add-Member -InputObject $row -NotePropertyName 'LauncherExists' -NotePropertyValue (Test-Path -LiteralPath $row.Launcher)
    }
    return $rows
}


function Get-CamComponentState {
    <#
        One snapshot of everything, shared by -Status and by -Stop's before
        and after counts, so the two can never disagree about what counts as
        running.
    #>

    Import-CamJobApi
    $inventory = @(Get-CamProcessInventory -Config $Config)
    $snapshot = Get-CamStatusSnapshot -Config $Config

    $readyPaths = @()
    if (Get-CamRelayEnabled -Config $Config) {
        try { $readyPaths = @(Get-CamRelayReadyPaths -Config $Config) } catch { $readyPaths = @() }
    }

    return [pscustomobject]@{
        Inventory       = $inventory
        Supervisor      = @($inventory | Where-Object { $_.Role -eq 'Supervisor' })
        NvrHost         = @($inventory | Where-Object { $_.Role -eq 'NvrHost' })
        RecorderChild   = @($inventory | Where-Object { $_.Role -eq 'Recorder' })
        Relay           = @($inventory | Where-Object { $_.Role -eq 'Relay' })
        Warden          = @($inventory | Where-Object { $_.Role -eq 'Warden' })
        Pane            = @($inventory | Where-Object { $_.Role -eq 'Pane' })
        Viewer          = @($inventory | Where-Object { $_.Role -eq 'Viewer' })
        Health          = @($inventory | Where-Object { $_.Role -eq 'Health' })
        LogonTasks      = @(Get-CamLogonTaskReport)
        SupervisorState = (Get-CamSupervisorState -Config $Config -Snapshot $snapshot)
        JobExists       = [CamJob]::Exists((Get-CamDashboardJobName))
        GridWindows     = @(Get-CamTerminalWindow -Title (Get-CamDashboardWindowTitle)).Count
        HealthWindows   = @(Get-CamTerminalWindow -Title 'cam.health').Count
        ReadyPaths      = $readyPaths
    }
}


function Show-CamStatus {
    <#
        Every component in one view, including the ones this script did not
        start. An orphan is called an orphan: a pane with no window to belong
        to is the exact thing that silently holds a camera slot.
    #>
    param([Parameter(Mandatory = $true)]$State)

    Write-CamSection 'recorder'
    $supervisorState = $State.SupervisorState
    if ($supervisorState.Running) {
        Write-Host ("  supervisor   RUNNING - {0}" -f $supervisorState.Reason) -ForegroundColor Green
    }
    else {
        Write-Host ("  supervisor   stopped - {0}" -f $supervisorState.Reason) -ForegroundColor Yellow
    }
    Write-Host ("  ffmpeg       {0} recording child(ren)" -f $State.RecorderChild.Count)
    foreach ($item in $State.Supervisor) {
        Write-Host ("               pid {0}, {1}" -f $item.ProcessId, $item.Detail) -ForegroundColor DarkGray
    }

    Write-CamSection 'relay'
    if (-not (Get-CamRelayEnabled -Config $Config)) {
        Write-Host '  disabled in recorder.config.json' -ForegroundColor DarkGray
    }
    else {
        Write-Host ("  mediamtx     {0} process(es)" -f $State.Relay.Count)
        Write-Host ("  serving      {0} path(s): {1}" -f $State.ReadyPaths.Count, (($State.ReadyPaths | Sort-Object) -join ', '))
    }

    Write-CamSection 'dashboard'
    Write-Host ("  grid window  {0} titled '{1}'" -f $State.GridWindows, (Get-CamDashboardWindowTitle))
    Write-Host ("  warden       {0} process(es), job '{1}' exists: {2}" -f `
        $State.Warden.Count, (Get-CamDashboardJobName), $State.JobExists)
    Write-Host ("  panes        {0}" -f $State.Pane.Count)
    # Two per pane: mpv.com is a console wrapper that re-execs mpv.exe, and
    # the video window belongs to the child.
    Write-Host ("  mpv          {0} process(es), wrapper plus child per pane" -f $State.Viewer.Count)
    Write-Host ("  health       {0} process(es), {1} window(s)" -f $State.Health.Count, $State.HealthWindows)

    # The warnings are the reason this view exists.
    if ($State.Pane.Count -gt 0 -and $State.GridWindows -eq 0) {
        Write-Host ("  ORPHANS      {0} pane(s) are running with no dashboard window. They still hold camera sessions." -f `
            $State.Pane.Count) -ForegroundColor Red
    }
    if ($State.Pane.Count -gt 0 -and -not $State.JobExists) {
        Write-Host '  WARNING      the panes are not held by a job, so closing the window will not stop them.' -ForegroundColor Yellow
    }
    if ($State.Health.Count -gt 1) {
        Write-Host ("  WARNING      {0} health strips are running; there should be one." -f $State.Health.Count) -ForegroundColor Yellow
    }
    $unowned = @($State.Viewer | Where-Object { $_.Detail -eq 'no pane owns it' })
    if ($unowned.Count -gt 0) {
        Write-Host ("  ORPHANS      {0} mpv with no pane: {1}" -f $unowned.Count, (($unowned.ProcessId | Sort-Object) -join ', ')) -ForegroundColor Red
    }

    Write-CamSection 'logon tasks'
    foreach ($task in $State.LogonTasks) {
        $colour = 'Green'
        if (-not $task.Registered) { $colour = 'Yellow' }
        Write-Host ("  {0,-17} {1,-15} {2}" -f $task.Name, $task.State, $task.What) -ForegroundColor $colour
        if ($task.Registered) {
            Write-Host ("  {0,-17} launcher {1}{2}" -f '', $task.Launcher,
                $(if ($task.LauncherExists) { '' } else { '  MISSING - reinstall the task' })) -ForegroundColor DarkGray
        }
        else {
            Write-Host ("  {0,-17} install it with {1}" -f '', $task.InstallBy) -ForegroundColor DarkGray
        }
    }

    Write-CamSection 'processes'
    if ($State.Inventory.Count -eq 0) {
        Write-Host '  nothing from this project is running' -ForegroundColor DarkGray
    }
    else {
        $State.Inventory |
            Sort-Object @{ Expression = { @('NvrHost', 'Supervisor', 'Recorder', 'Relay', 'NvrWarden', 'Warden', 'Pane', 'Viewer', 'Health').IndexOf($_.Role) } }, ProcessId |
            Format-Table -AutoSize @{ Label = 'role'; Expression = { $_.Role } },
                @{ Label = 'pid'; Expression = { $_.ProcessId } },
                @{ Label = 'parent'; Expression = { $_.ParentProcessId } },
                @{ Label = 'image'; Expression = { $_.Name } },
                @{ Label = 'what it is'; Expression = { $_.Detail } } |
            Out-String -Width 160 | Write-Host
    }
}


function Get-CamStopCount {
    <# The one number -Stop has to drive to zero, per component. #>
    param([Parameter(Mandatory = $true)]$State)

    return [pscustomobject]@{
        Supervisor = $State.Supervisor.Count
        Ffmpeg     = $State.RecorderChild.Count
        MediaMtx   = $State.Relay.Count
        Warden     = $State.Warden.Count
        Panes      = $State.Pane.Count
        Mpv        = $State.Viewer.Count
        Health     = $State.Health.Count
    }
}


function Show-CamStopCount {
    param(
        [Parameter(Mandatory = $true)][string]$Label,
        [Parameter(Mandatory = $true)]$Count
    )

    Write-Host ("  {0,-7} supervisor={1} ffmpeg={2} mediamtx={3} warden={4} panes={5} mpv={6} health={7}" -f `
        $Label, $Count.Supervisor, $Count.Ffmpeg, $Count.MediaMtx, $Count.Warden, `
        $Count.Panes, $Count.Mpv, $Count.Health)
}


function Invoke-CamStopAll {
    <#
        Stops what the scope asked for and proves it, by counting the same
        things before and after.

        The dashboard goes first. Its panes hold the second RTSP session of
        every camera, and a pane whose mpv is restarting will happily open a
        new one while the recorder is being torn down beside it.
    #>

    $before = Get-CamComponentState
    Write-Host ''
    Write-Host 'stopping' -ForegroundColor Cyan
    Show-CamStopCount -Label 'before' -Count (Get-CamStopCount -State $before)

    if ($WantDashboard) {
        Write-Host '  dashboard:' -ForegroundColor Cyan
        $report = Stop-CamDashboard -Config $Config
        if ($report.Refused.Count -gt 0) {
            Write-Host ("  the guard refused {0}" -f (($report.Refused | Sort-Object) -join ', ')) -ForegroundColor Yellow
        }
    }

    if ($WantRecorder) {
        Write-Host '  recorder:' -ForegroundColor Cyan
        if ($before.SupervisorState.Running -or $before.Supervisor.Count -gt 0 -or $before.Relay.Count -gt 0) {
            # Through the recorder's own -Stop: it writes the stop flag and
            # lets the supervisor tear its children down in order, which is
            # the only shutdown that closes the ffmpeg files cleanly.
            & $RecorderScript -Stop
        }
        else {
            Write-Host '  the recorder was not running' -ForegroundColor DarkGray
        }

        # A supervisor that died without tearing its children down leaves
        # ffmpeg and MediaMTX running with nothing to stop them: their parent
        # pid points at a process that no longer exists, so the recorder's own
        # -Stop has no supervisor to signal and the stop flag is read by
        # nobody. Observed on 2026-10-12, with five ffmpeg still writing and
        # MediaMTX still serving half an hour after the supervisor had gone.
        #
        # Each of those holds a camera session, which is the whole reason this
        # script exists, so they are stopped directly once the orderly path
        # has had its turn.
        # The orderly stop is given its time first. An ffmpeg killed partway
        # through its own shutdown is a truncated segment, and the supervisor
        # closes them in order precisely to avoid that.
        $orphans = @()
        $deadline = (Get-Date).AddSeconds(20)
        while ($true) {
            $orphans = @(Get-CamProcessInventory -Config $Config |
                Where-Object { $_.Role -in @('Supervisor', 'Recorder', 'Relay') })
            if ($orphans.Count -eq 0 -or (Get-Date) -ge $deadline) { break }
            Start-Sleep -Milliseconds 500
        }
        if ($orphans.Count -gt 0) {
            Write-Host ("  {0} recorder process(es) outlived the orderly stop; stopping them directly" -f `
                $orphans.Count) -ForegroundColor Yellow
            $stop = Stop-CamProcessList -ProcessId @($orphans | ForEach-Object { [int]$_.ProcessId })
            if ($stop.Survived.Count -gt 0) {
                Write-Host ("  still alive: {0}" -f (($stop.Survived | Sort-Object) -join ', ')) -ForegroundColor Red
            }
        }
    }

    $after = Get-CamComponentState
    $counts = Get-CamStopCount -State $after
    Show-CamStopCount -Label 'after' -Count $counts

    $left = New-Object System.Collections.ArrayList
    if ($WantDashboard) {
        foreach ($name in @('Warden', 'Panes', 'Mpv', 'Health')) {
            if ($counts.$name -gt 0) { [void]$left.Add(("{0}={1}" -f $name, $counts.$name)) }
        }
    }
    if ($WantRecorder) {
        foreach ($name in @('Supervisor', 'Ffmpeg', 'MediaMtx')) {
            if ($counts.$name -gt 0) { [void]$left.Add(("{0}={1}" -f $name, $counts.$name)) }
        }
    }

    Write-Host ''
    if ($left.Count -eq 0) {
        Write-Host 'everything in scope is stopped.' -ForegroundColor Green
        return 0
    }
    Write-Host ("STILL RUNNING: {0}. Run -Status for the pids." -f ($left -join ' ')) -ForegroundColor Red
    return 1
}


function Invoke-CamStartAll {
    <#
        Recorder first, then the dashboard, with a wait in between.

        The wait is not politeness. Both consumers choose between the relay
        and a direct camera connection when they start, and a pane that
        chooses before MediaMTX is serving takes a direct connection - which
        is the camera's second and last session, the one the relay exists to
        keep free. Five of six paths serving has already been measured as two
        panes on direct connections.
    #>

    if ($WantRecorder) {
        Write-CamSection 'recorder'
        $state = Get-CamSupervisorState -Config $Config -Snapshot (Get-CamStatusSnapshot -Config $Config)
        if ($state.Running) {
            Write-Host ("  already running - {0}" -f $state.Reason) -ForegroundColor Green
        }
        else {
            $task = Get-ScheduledTask -TaskName $RecorderTaskName -ErrorAction SilentlyContinue
            if (-not $task) {
                Write-Host ("  the '{0}' task is not registered. Install it with:" -f $RecorderTaskName) -ForegroundColor Yellow
                Write-Host ("    .\cam.recorder.ps1 -InstallAutostart") -ForegroundColor Yellow
            }
            else {
                Write-Host ("  starting the '{0}' task" -f $RecorderTaskName)
                & schtasks.exe /run /tn $RecorderTaskName | Out-Null
                for ($wait = 0; $wait -lt 40; $wait++) {
                    Start-Sleep -Milliseconds 500
                    $state = Get-CamSupervisorState -Config $Config -Snapshot (Get-CamStatusSnapshot -Config $Config)
                    if ($state.Running) { break }
                }
                if ($state.Running) { Write-Host ("  up - {0}" -f $state.Reason) -ForegroundColor Green }
                else { Write-Host ("  did not report running within 20s - {0}" -f $state.Reason) -ForegroundColor Yellow }
            }
        }
    }

    if ($WantDashboard) {
        if ($WantRecorder -and (Get-CamRelayEnabled -Config $Config)) {
            Write-CamSection 'waiting for the relay to serve'
            # Get-CamList is passed without @(): it returns its array with a
            # leading comma, and @() around a function call nests that one
            # level deeper into a single element holding every camera.
            $wanted = (Get-CamRelayPathMap -Cameras (Get-CamList)).Count
            $seconds = [int]$Config.RelayReadyWaitSeconds
            if ($seconds -lt 5) { $seconds = 5 }
            $deadline = (Get-Date).AddSeconds($seconds)
            $ready = @()
            while ((Get-Date) -lt $deadline) {
                try { $ready = @(Get-CamRelayReadyPaths -Config $Config) } catch { $ready = @() }
                if ($ready.Count -ge 1) { break }
                Start-Sleep -Milliseconds ([int]$Config.RelayReadyPollMs)
            }
            Write-Host ("  {0} of {1} path(s) serving after the wait: {2}" -f `
                $ready.Count, $wanted, (($ready | Sort-Object) -join ', '))
            Write-Host '  a pane whose path is not serving yet waits for it on its own, then takes a direct slot.' -ForegroundColor DarkGray
        }

        Write-CamSection 'dashboard'
        & $DashboardScript
    }

    return 0
}


function Invoke-CamInstallDashboardAutostart {
    <#
        Registers the logon task that opens the dashboard.

        Why a SECOND task rather than a switch on the recorder's: the recorder
        is a headless service that must run whether anybody is logged on or
        not, and the dashboard is a desktop artifact that needs an interactive
        session - one task for both would couple the two. And -Start here runs
        the recorder's task by name, so a task pointing at this script with no
        scope would invoke itself.

        It registers IPCCTV-Dashboard and nothing else. The recorder's task is
        not read, not re-registered and not removed by this.

        No ordering is coordinated with the recorder's task. Both fire at
        logon, and each pane waits up to RelayReadyWaitSeconds for its own
        relay path before settling for a direct camera connection, which is
        the same wait that covers a cold relay at any other time.
    #>

    Write-CamSection 'install the dashboard logon task'

    $stubPath = New-CamLauncherStub -StubPath $DashboardStubPath `
        -TargetScript (Join-Path -Path $ScriptDir -ChildPath 'cam.ps1') `
        -TargetArgument @('-Start', '-Dashboard') -LogPath $DashboardStubLogPath `
        -TimeoutSeconds $DashboardStubWaitSeconds -PollSeconds $DashboardStubPollSeconds

    $registration = Register-CamLogonTask -TaskName $DashboardTaskName -StubPath $stubPath `
        -Description 'Opens the IPCCTV viewer dashboard at logon.'

    if ($registration.Existed) {
        Write-Host ("  updated the existing task '{0}'" -f $DashboardTaskName) -ForegroundColor Green
    }
    else {
        Write-Host ("  created the task '{0}'" -f $DashboardTaskName) -ForegroundColor Green
    }
    Write-Host ("  runs      {0} {1}" -f $registration.Executable, $registration.Arguments)
    Write-Host ("  launcher  {0}" -f $stubPath)
    Write-Host ("  forwards  {0} -Start -Dashboard (waits up to {1}s for that path)" -f `
        (Join-Path -Path $ScriptDir -ChildPath 'cam.ps1'), $DashboardStubWaitSeconds)
    Write-Host ("  log       {0}" -f $DashboardStubLogPath)
    Write-Host ("  as        {0} (interactive token, no elevation)" -f $registration.Account)
    Write-Host ("  trigger   at logon of {0}" -f $registration.Account)
    Write-Host ("  limits    {0}" -f $registration.Policy)
    Write-Host ("  state     {0}" -f $registration.Task.State)
    Write-Host ("  registered task(s) with this name: {0}" -f `
        @(Get-ScheduledTask -TaskName $DashboardTaskName -ErrorAction SilentlyContinue).Count)
    $recorderTask = Get-CamLogonTask -TaskName $RecorderTaskName
    $recorderState = 'not registered'
    if ($recorderTask) { $recorderState = [string]$recorderTask.State }
    Write-Host ("  the '{0}' task is untouched by this: {1}" -f $RecorderTaskName, $recorderState) -ForegroundColor DarkGray

    Write-Host ''
    Write-Host 'Verify it with:  schtasks /query /tn IPCCTV-Dashboard /fo list' -ForegroundColor Cyan
    Write-Host 'Remove it with:  .\cam.ps1 -RemoveDashboardAutostart' -ForegroundColor Cyan
    return 0
}


function Invoke-CamRemoveDashboardAutostart {
    <#
        Unregisters the dashboard's logon task and deletes its launcher. A
        clean no-op when neither is there, and it never touches the
        recorder's task.
    #>

    Write-CamSection 'remove the dashboard logon task'

    if (Unregister-CamLogonTask -TaskName $DashboardTaskName) {
        Write-Host ("  removed the task '{0}'" -f $DashboardTaskName) -ForegroundColor Green
    }
    else {
        Write-Host ("  no task named '{0}' is registered" -f $DashboardTaskName) -ForegroundColor Yellow
    }

    if (Remove-CamLauncherStub -StubPath $DashboardStubPath) {
        Write-Host ("  removed the launcher '{0}'" -f $DashboardStubPath) -ForegroundColor Green
    }
    else {
        Write-Host ("  no launcher at '{0}'" -f $DashboardStubPath) -ForegroundColor Yellow
    }

    if (Test-Path -LiteralPath $DashboardStubLogPath) {
        Write-Host ("  kept the launcher log '{0}' - delete it by hand if you do not want it" -f $DashboardStubLogPath)
    }

    $recorderTask = Get-CamLogonTask -TaskName $RecorderTaskName
    $recorderState = 'not registered'
    if ($recorderTask) { $recorderState = [string]$recorderTask.State }
    Write-Host ("  the '{0}' task is untouched by this: {1}" -f $RecorderTaskName, $recorderState) -ForegroundColor DarkGray

    Write-Host ''
    Write-Host 'A dashboard that is open now stays open. Close it with:  .\cam.ps1 -Stop -Dashboard' -ForegroundColor Cyan
    return 0
}


switch ($PSCmdlet.ParameterSetName) {
    'Stop' { exit (Invoke-CamStopAll) }
    'Start' { exit (Invoke-CamStartAll) }
    'InstallDashboardAutostart' { exit (Invoke-CamInstallDashboardAutostart) }
    'RemoveDashboardAutostart' { exit (Invoke-CamRemoveDashboardAutostart) }
    default {
        # -Status, and the bare invocation, which is the same question.
        Show-CamStatus -State (Get-CamComponentState)
        Write-Host ''
        exit 0
    }
}
