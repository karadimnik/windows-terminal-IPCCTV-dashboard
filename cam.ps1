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

.PARAMETER Install
    Installs everything that makes the system start by itself: the MediaMTX
    config, then the recorder's, the dashboard's and the UI's logon tasks.
    Every prerequisite is checked first and every problem is printed at once;
    a missing one stops it before anything changes, except camui.exe, which
    only skips the UI. A component already in place is left alone, so a
    second run changes nothing. Starts nothing - -Start does that. Exits 0
    only when every component is in place. Add -WhatIf to see the plan.

.PARAMETER Uninstall
    Removes what -Install installed: the three logon tasks, their launchers
    and the MediaMTX config. Keeps the logs, the archive, cameras.json,
    recorder.config.json and the credentials, stops nothing and leaves the
    event source alone. A second run is a clean no-op. Add -WhatIf to see
    what it would remove.

.PARAMETER InstallDashboardAutostart
    Registers a logon task that opens the dashboard, and leaves the
    recorder's own task completely alone. Idempotent, unelevated.

.PARAMETER RemoveDashboardAutostart
    Removes that task and its launcher. A clean no-op when they are not
    there, and it never touches the recorder's task either.

.PARAMETER InstallUi
    Registers a logon task that starts the control UI, camui.exe, and leaves
    the recorder's and the dashboard's tasks completely alone. Idempotent,
    unelevated. It does not start the UI; it prints the one command that does.

.PARAMETER RemoveUi
    Removes that task and its launcher. A clean no-op when they are not
    there, and it never touches the other two tasks. A camui already running
    keeps running.

.EXAMPLE
    .\cam.ps1 -Install -WhatIf
    .\cam.ps1 -Install
    .\cam.ps1 -Uninstall
    .\cam.ps1
    .\cam.ps1 -Stop
    .\cam.ps1 -Stop -Dashboard
    .\cam.ps1 -Start
    .\cam.ps1 -InstallDashboardAutostart
    .\cam.ps1 -InstallUi
#>
# The bare invocation is -Status, which needs saying explicitly: with several
# parameter sets and no mandatory parameter in any of them, PowerShell cannot
# pick one from no arguments at all and fails with "parameter set cannot be
# resolved" instead. Measured on 2026-10-12 - .\cam.ps1 had never worked.
# SupportsShouldProcess is for -Install and -Uninstall; the dispatch refuses
# -WhatIf on every other action rather than half-honour it.
[CmdletBinding(DefaultParameterSetName = 'Status', SupportsShouldProcess = $true)]
param(
    [Parameter(ParameterSetName = 'Status')]
    [switch]$Status,

    [Parameter(ParameterSetName = 'Start')]
    [switch]$Start,

    [Parameter(ParameterSetName = 'Stop')]
    [switch]$Stop,

    [Parameter(ParameterSetName = 'Install')]
    [switch]$Install,

    [Parameter(ParameterSetName = 'Uninstall')]
    [switch]$Uninstall,

    [Parameter(ParameterSetName = 'InstallDashboardAutostart')]
    [switch]$InstallDashboardAutostart,

    [Parameter(ParameterSetName = 'RemoveDashboardAutostart')]
    [switch]$RemoveDashboardAutostart,

    [Parameter(ParameterSetName = 'InstallUi')]
    [switch]$InstallUi,

    [Parameter(ParameterSetName = 'RemoveUi')]
    [switch]$RemoveUi,

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
$RecorderStubPath = Join-Path -Path (Get-CamAutostartDir) -ChildPath 'start-recorder.ps1'
$DashboardTaskName = 'IPCCTV-Dashboard'
$DashboardStubPath = Join-Path -Path (Get-CamAutostartDir) -ChildPath 'start-dashboard.ps1'
$DashboardStubLogPath = Join-Path -Path (Get-CamAutostartDir) -ChildPath 'autostart-dashboard.log'
# How long the launcher waits for this script's path, and how often it looks.
$DashboardStubWaitSeconds = 300
$DashboardStubPollSeconds = 5

# The third task: the control UI. camui.exe is built into this directory by
# ui/build.sh and is not in the repository, camui.config.json is optional and
# is where a listen address and a token go, and camui's own output is pumped
# into camui.log beside the launcher logs.
$UiTaskName = 'IPCCTV-Ui'
$UiExePath = Join-Path -Path $ScriptDir -ChildPath 'camui.exe'
$UiConfigPath = Join-Path -Path $ScriptDir -ChildPath 'camui.config.json'
$UiStubPath = Join-Path -Path (Get-CamAutostartDir) -ChildPath 'start-ui.ps1'
$UiStubLogPath = Join-Path -Path (Get-CamAutostartDir) -ChildPath 'autostart-ui.log'
$UiLogPath = Join-Path -Path (Get-CamAutostartDir) -ChildPath 'camui.log'
$UiStubWaitSeconds = 300
$UiStubPollSeconds = 5

# Neither scope switch means both, so the bare verb always means everything.
$WantRecorder = $Recorder -or -not ($Recorder -or $Dashboard)
$WantDashboard = $Dashboard -or -not ($Recorder -or $Dashboard)

# -Install reports a recorder.config.json that does not load as one of its
# prerequisites, beside every other problem, rather than dying on it alone.
$ConfigProblem = $null
try { $Config = Get-CamConfig }
catch {
    if ($PSCmdlet.ParameterSetName -ne 'Install') { throw }
    $Config = $null
    $ConfigProblem = $_.Exception.Message
}


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
            Launcher  = $RecorderStubPath
            InstallBy = '.\cam.recorder.ps1 -InstallAutostart'
        },
        [pscustomobject]@{
            Name      = $DashboardTaskName
            What      = 'the viewer dashboard'
            Launcher  = $DashboardStubPath
            InstallBy = '.\cam.ps1 -InstallDashboardAutostart'
        },
        [pscustomobject]@{
            Name      = $UiTaskName
            What      = 'the control UI'
            Launcher  = $UiStubPath
            InstallBy = '.\cam.ps1 -InstallUi'
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
        $relayApi = Get-CamRelayApiStatus
    }

    # Live view, from status.json alone (no network call); $null when it is off.
    $liveStatus = $null
    if ((Get-CamLiveEnabled -Config $Config) -and $snapshot -and $snapshot.PSObject.Properties['Live']) { $liveStatus = $snapshot.Live }

    # By the handle its warden published, not by the title it is showing. A
    # Windows Terminal window's title follows its active tab, so counting
    # titles reported the NVR window as gone every time a foreign console was
    # adopted into it - with the host, the warden, the job and six ffmpeg all
    # alive. Item [81]. The health strip has no warden and therefore no
    # published handle, so it is still counted by title; its own window title
    # is known to be unreliable too, which is why Stop-CamHealthStrip stops it
    # by process identity and treats the window as cosmetic cleanup.
    $nvrWindow = Get-CamWindowPresence -Config $Config -Title (Get-CamNvrWindowTitle)
    $gridWindow = Get-CamWindowPresence -Config $Config -Title (Get-CamDashboardWindowTitle)

    return [pscustomobject]@{
        Inventory       = $inventory
        Supervisor      = @($inventory | Where-Object { $_.Role -eq 'Supervisor' })
        NvrHost         = @($inventory | Where-Object { $_.Role -eq 'NvrHost' })
        NvrWarden       = @($inventory | Where-Object { $_.Role -eq 'NvrWarden' })
        RecorderChild   = @($inventory | Where-Object { $_.Role -eq 'Recorder' })
        Relay           = @($inventory | Where-Object { $_.Role -eq 'Relay' })
        Warden          = @($inventory | Where-Object { $_.Role -eq 'Warden' })
        Pane            = @($inventory | Where-Object { $_.Role -eq 'Pane' })
        Viewer          = @($inventory | Where-Object { $_.Role -eq 'Viewer' })
        Health          = @($inventory | Where-Object { $_.Role -eq 'Health' })
        LogonTasks      = @(Get-CamLogonTaskReport)
        Ui              = (Get-CamUiRuntimeState -ExePath $UiExePath -ConfigPath $UiConfigPath)
        SupervisorState = (Get-CamSupervisorState -Config $Config -Snapshot $snapshot)
        JobExists       = [CamJob]::Exists((Get-CamDashboardJobName))
        NvrJobExists    = [CamJob]::Exists((Get-CamNvrJobName))
        NvrWindow       = $nvrWindow
        GridWindow      = $gridWindow
        NvrWindows      = $nvrWindow.Count
        GridWindows     = $gridWindow.Count
        HealthWindows   = @(Get-CamTerminalWindow -Title 'cam.health').Count
        ReadyPaths      = $readyPaths
        RelayApi        = $relayApi
        LiveStatus      = $liveStatus
    }
}


function Write-CamWindowLine {
    <#
        One line naming the window, so "which window must I not close" is
        answerable from -Status alone.

        The handle is printed because it is the only name this window reliably
        has. The title is printed as it is NOW, and when that differs from the
        title we gave it the line says so outright: the window is ours, with
        something else showing in it, and that is the state in which closing
        what looks like a stray console takes the NVR down with it.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Label,
        [Parameter(Mandatory = $true)]$Presence
    )

    if (-not $Presence.Found) {
        Write-Host ("  {0} ABSENT - no window handle published and none titled '{1}'" -f `
            $Label.PadRight(12), $Presence.Title) -ForegroundColor Yellow
        return
    }

    if ($Presence.Source -eq 'title') {
        Write-Host ("  {0} PRESENT - {1} titled '{2}', found by title; no warden has published a handle" -f `
            $Label.PadRight(12), $Presence.HwndHex, $Presence.Title)
        return
    }

    if ($Presence.TitleChanged) {
        Write-Host ("  {0} PRESENT - {1}, NOW TITLED '{2}' not '{3}': another tab is active in it. THAT is the window not to close (terminal pid {4}, warden {5})" -f `
            $Label.PadRight(12), $Presence.HwndHex, $Presence.CurrentTitle, $Presence.Title, `
            $Presence.TerminalPid, $Presence.WardenPid) -ForegroundColor Cyan
        return
    }

    Write-Host ("  {0} PRESENT - {1} titled '{2}' (terminal pid {3}, warden {4})" -f `
        $Label.PadRight(12), $Presence.HwndHex, $Presence.CurrentTitle, `
        $Presence.TerminalPid, $Presence.WardenPid)
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
    # The NVR window and the job it owns. A job with no window is the state
    # that used to make the recorder unstartable - item [63].
    Write-Host ("  nvr          {0} host, {1} warden, job '{2}' exists: {3}" -f `
        $State.NvrHost.Count, $State.NvrWarden.Count, (Get-CamNvrJobName), $State.NvrJobExists)
    Write-CamWindowLine -Label 'nvr window' -Presence $State.NvrWindow
    if ($State.NvrJobExists -and $State.NvrWindows -eq 0 -and $State.Supervisor.Count -eq 0) {
        Write-Host ("  STALE        the '{0}' job is held with no window and nothing recording. -Start -Recorder clears it." -f `
            (Get-CamNvrJobName)) -ForegroundColor Yellow
    }

    Write-CamSection 'relay'
    if (-not (Get-CamRelayEnabled -Config $Config)) {
        Write-Host '  disabled in recorder.config.json' -ForegroundColor DarkGray
    }
    else {
        Write-Host ("  mediamtx     {0} process(es)" -f $State.Relay.Count)
        if ($State.RelayApi -eq 'denied') {
            Write-Host "  serving      unknown: the control API REFUSED the product's login, so every consumer is kept on the relay." -ForegroundColor Red
            Write-Host '               Run .\cam.recorder.ps1 -InstallRestreamer, then .\cam.ps1 -Stop and -Start.' -ForegroundColor Yellow
        }
        else {
            Write-Host ("  serving      {0} path(s): {1}" -f $State.ReadyPaths.Count, (($State.ReadyPaths | Sort-Object) -join ', '))
        }
    }

    # Only when live view is switched on, so with it off -Status reads as it always did.
    if (Get-CamLiveEnabled -Config $Config) {
        Write-CamSection 'live view'
        if (-not $supervisorState.Running) {
            Write-Host '  supervisor not running, so live view is not either' -ForegroundColor DarkGray
        }
        elseif (-not $State.LiveStatus) {
            Write-Host '  no live-view section in status.json yet (the supervisor writes it once it has started)' -ForegroundColor Yellow
        }
        else {
            $live = $State.LiveStatus
            $liveColour = $(if ($live.Alive) { 'Gray' } else { 'Yellow' })
            Write-Host ("  mediamtx     {0}, {1} restart(s)" -f $(if ($live.Alive) { "alive, pid $($live.Pid)" } else { 'NOT running' }), $live.RestartsTotal) -ForegroundColor $liveColour
            Write-Host ("  paths        {0}" -f ((@($live.Paths) | Sort-Object) -join ', '))
            if ($live.LastError) { Write-Host ("  last error   {0}" -f $live.LastError) -ForegroundColor Yellow }
        }
    }

    Write-CamSection 'dashboard'
    Write-CamWindowLine -Label 'grid window' -Presence $State.GridWindow
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

    Write-CamSection 'ui'
    $ui = $State.Ui
    $uiTask = @($State.LogonTasks | Where-Object { $_.Name -eq $UiTaskName } | Select-Object -First 1)[0]
    if ($ui.Problem) {
        Write-Host ("  camui        cannot tell: {0}" -f $ui.Problem) -ForegroundColor Red
    }
    elseif ($ui.Listening -and $ui.IsCamUi) {
        Write-Host ("  camui        SERVING {0} - pid {1}" -f $ui.Url, $ui.OwnerPid) -ForegroundColor Green
    }
    elseif ($ui.Listening) {
        # The port is taken by something that is not camui. Saying which pid
        # owns it is the whole value of reporting this at all.
        $owner = $ui.OwnerPath
        if (-not $owner) { $owner = 'its path is not readable from here' }
        Write-Host ("  camui        NOT serving - pid {0} ({1}) is listening on {2} instead" -f `
            $ui.OwnerPid, $owner, $ui.Address) -ForegroundColor Red
    }
    else {
        Write-Host ("  camui        stopped - nothing is listening on {0}" -f $ui.Address) -ForegroundColor Yellow
    }
    Write-Host ("  binary       {0}{1}" -f $ui.ExePath, `
        $(if ($ui.ExeBuilt) { '' } else { '  MISSING - cross-compile it with ui/build.sh' }))
    $addressFrom = "camui's own default, with no camui.config.json"
    if ($ui.ConfigExists) { $addressFrom = $ui.ConfigPath }
    Write-Host ("  address      {0}, from {1}" -f $ui.Address, $addressFrom)
    if ($ui.TokenConfigured) {
        Write-Host '  auth         a token is configured, so every request needs it'
    }
    else {
        Write-Host '  auth         none, and loopback only - camui refuses a non-loopback bind without a token'
    }
    Write-Host ("  logon task   {0}" -f $uiTask.State)
    if (-not $ui.Listening) {
        if ($uiTask.Registered) {
            Write-Host ("  start it     Start-ScheduledTask -TaskName {0}" -f $UiTaskName) -ForegroundColor Cyan
        }
        else {
            Write-Host '  install it   .\cam.ps1 -InstallUi' -ForegroundColor Cyan
        }
    }

    Write-CamSection 'logon tasks'
    foreach ($task in $State.LogonTasks) {
        $colour = 'Green'
        if (-not $task.Registered) { $colour = 'Yellow' }
        Write-Host ("  {0,-17} {1,-15} {2}" -f $task.Name, $task.State, $task.What) -ForegroundColor $colour
        if ($task.Registered) {
            Write-Host ("  {0,-17} launcher {1}{2}" -f '', $task.Launcher,
                $(if ($task.LauncherExists) { '' } else { '  MISSING - .\cam.ps1 -Install rewrites it' })) -ForegroundColor DarkGray
        }
        else {
            Write-Host ("  {0,-17} install it with {1}" -f '', $task.InstallBy) -ForegroundColor DarkGray
        }
    }
    if (@($State.LogonTasks | Where-Object { -not $_.Registered -or -not $_.LauncherExists }).Count -gt 0) {
        Write-Host '  .\cam.ps1 -Install installs every one that is missing, and leaves the rest alone' -ForegroundColor Cyan
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
        NvrHost    = $State.NvrHost.Count
        NvrWarden  = $State.NvrWarden.Count
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

    # nvr is host/warden: the NVR window's pane and the process holding its job.
    Write-Host ("  {0,-7} nvr={1}/{2} supervisor={3} ffmpeg={4} mediamtx={5} warden={6} panes={7} mpv={8} health={9}" -f `
        $Label, $Count.NvrHost, $Count.NvrWarden, $Count.Supervisor, $Count.Ffmpeg, $Count.MediaMtx, `
        $Count.Warden, $Count.Panes, $Count.Mpv, $Count.Health)
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
        $deadline = (Get-CamWallClock).AddSeconds(20)
        while ($true) {
            $orphans = @(Get-CamProcessInventory -Config $Config |
                Where-Object { $_.Role -in @('Supervisor', 'Recorder', 'Relay') })
            if ($orphans.Count -eq 0 -or (Get-CamWallClock) -ge $deadline) { break }
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

        # Last, the NVR window itself. The supervisor stopping does not end it:
        # the host pane stays at a prompt with the log on screen and the warden
        # goes on holding the cam.nvr job, which is what made the next -Start
        # refuse and record nothing - item [63]. It is last on purpose, after
        # the orderly stop and after the sweep above, because releasing the job
        # is a kernel kill of everything still inside it.
        $nvr = Stop-CamNvr -Config $Config
        if ($nvr.Wardens.Count -eq 0 -and $nvr.Hosts.Count -eq 0) {
            Write-Host '  the NVR window was not open' -ForegroundColor DarkGray
        }
        else {
            Write-Host ("  stopped the NVR window: {0} host, {1} warden; the '{2}' job is released: {3}" -f `
                $nvr.Hosts.Count, $nvr.Wardens.Count, $nvr.JobName, (-not $nvr.JobExists))
            if ($nvr.Survived.Count -gt 0) {
                Write-Host ("  still alive: {0}" -f (($nvr.Survived | Sort-Object) -join ', ')) -ForegroundColor Red
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
        foreach ($name in @('NvrHost', 'NvrWarden', 'Supervisor', 'Ffmpeg', 'MediaMtx')) {
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

    # A start that started nothing must not report success. Before item [63]
    # this path printed "did not report running within 20s" and exited 0, so a
    # stop followed by a start left the cameras recording nothing and said it
    # had worked - and the UI now presses this as a button.
    $recorderProblem = ''

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
                $recorderProblem = "the '$RecorderTaskName' task is not registered, so there is nothing to start. Install it with .\cam.recorder.ps1 -InstallAutostart"
            }
            else {
                Write-Host ("  starting the '{0}' task" -f $RecorderTaskName)
                & schtasks.exe /run /tn $RecorderTaskName | Out-Null

                # 60s, because the chain from the task to the first heartbeat
                # is long and measured at 20-30s: the task starts a launcher
                # stub which polls for the repo path, which runs -Nvr, which
                # starts a warden, waits for its job, calls wt.exe, and only
                # then does the pane start the supervisor. The old limit was
                # 20s and sat right on that figure - four launches on
                # 2026-10-13 were reported as failures while recording in fact
                # began seconds later. Now that this exit code is acted on, a
                # false alarm costs as much as the silence it replaced.
                $waitSeconds = 60
                $started = Get-CamWallClock
                $deadline = $started.AddSeconds($waitSeconds)
                while ((Get-CamWallClock) -lt $deadline) {
                    Start-Sleep -Milliseconds 500
                    $state = Get-CamSupervisorState -Config $Config -Snapshot (Get-CamStatusSnapshot -Config $Config)
                    if ($state.Running) { break }
                }
                $took = [int]((Get-CamWallClock) - $started).TotalSeconds
                if ($state.Running) {
                    Write-Host ("  up after {0}s - {1}" -f $took, $state.Reason) -ForegroundColor Green
                }
                else {
                    Write-Host ("  did not report running within {0}s - {1}" -f $waitSeconds, $state.Reason) -ForegroundColor Red
                    $recorderProblem = "the supervisor did not report running within {0}s - {1}" -f $waitSeconds, $state.Reason
                }
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
            $deadline = (Get-CamWallClock).AddSeconds($seconds)
            $ready = @()
            while ((Get-CamWallClock) -lt $deadline) {
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

    if ($recorderProblem) {
        Write-Host ''
        Write-Host ("NOT RECORDING: {0}" -f $recorderProblem) -ForegroundColor Red
        Write-Host ("  the task forwards to '.\cam.recorder.ps1 -Nvr', which opens the NVR window. Run that directly to see what it says,") -ForegroundColor Yellow
        Write-Host ("  and read the launcher's own log at {0}." -f `
            (Join-Path -Path (Get-CamAutostartDir) -ChildPath 'autostart.log')) -ForegroundColor Yellow
        Write-Host '  cam.ps1 -Status shows the NVR host, its warden and whether the job is still held.' -ForegroundColor Yellow
        return 1
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


function Invoke-CamInstallUi {
    <#
        Registers the logon task that starts the control UI, camui.exe.

        A third task, for the same reason there are already two: the recorder
        is a headless service, the dashboard is a desktop artifact, and the UI
        is a local web server that must outlive both. One task for several of
        them would couple lifetimes that have nothing to do with each other.

        It registers IPCCTV-Ui and nothing else. The recorder's and the
        dashboard's tasks are not read for anything but a line of reporting,
        not re-registered and not removed.

        It does not start camui. The task is the one start path - running it by
        hand is exactly what logon does - so this prints that command rather
        than starting the binary a second way that could drift from it.
    #>

    Write-CamSection 'install the control UI logon task'

    if (-not (Test-Path -LiteralPath $UiExePath)) {
        Write-Host ("  camui.exe is not here: {0}" -f $UiExePath) -ForegroundColor Red
        Write-Host '  The binary is deliberately not in the repository. Cross-compile it with ui/build.sh' -ForegroundColor Red
        Write-Host '  (see docs/go-toolchain.md for the one-time Go install), then run this again.' -ForegroundColor Red
        Write-Host '  Nothing was installed.' -ForegroundColor Red
        return 1
    }

    $stubPath = New-CamUiLauncherStub -StubPath $UiStubPath -ExePath $UiExePath `
        -ConfigPath $UiConfigPath -LogPath $UiStubLogPath -UiLogPath $UiLogPath `
        -TimeoutSeconds $UiStubWaitSeconds -PollSeconds $UiStubPollSeconds

    $registration = Register-CamLogonTask -TaskName $UiTaskName -StubPath $stubPath `
        -Description 'Starts the IPCCTV control UI (camui) at logon.'

    if ($registration.Existed) {
        Write-Host ("  updated the existing task '{0}'" -f $UiTaskName) -ForegroundColor Green
    }
    else {
        Write-Host ("  created the task '{0}'" -f $UiTaskName) -ForegroundColor Green
    }
    $ui = Get-CamUiRuntimeState -ExePath $UiExePath -ConfigPath $UiConfigPath
    Write-Host ("  runs      {0} {1}" -f $registration.Executable, $registration.Arguments)
    Write-Host ("  launcher  {0}" -f $stubPath)
    Write-Host ("  starts    {0} (waits up to {1}s for that path)" -f $UiExePath, $UiStubWaitSeconds)
    Write-Host ("  serves    {0}" -f $ui.Url)
    Write-Host ("  log       {0}, and camui's own output in {1}" -f $UiStubLogPath, $UiLogPath)
    Write-Host ("  as        {0} (interactive token, no elevation)" -f $registration.Account)
    Write-Host ("  trigger   at logon of {0}" -f $registration.Account)
    Write-Host ("  limits    {0}" -f $registration.Policy)
    Write-Host ("  state     {0}" -f $registration.Task.State)
    Write-Host ("  registered task(s) with this name: {0}" -f `
        @(Get-ScheduledTask -TaskName $UiTaskName -ErrorAction SilentlyContinue).Count)
    foreach ($other in @($RecorderTaskName, $DashboardTaskName)) {
        $task = Get-CamLogonTask -TaskName $other
        $state = 'not registered'
        if ($task) { $state = [string]$task.State }
        Write-Host ("  the '{0}' task is untouched by this: {1}" -f $other, $state) -ForegroundColor DarkGray
    }

    Write-Host ''
    Write-Host ("Start it now with:  Start-ScheduledTask -TaskName {0}" -f $UiTaskName) -ForegroundColor Cyan
    Write-Host ("Then open:          {0}" -f $ui.Url) -ForegroundColor Cyan
    Write-Host 'Verify it with:     schtasks /query /tn IPCCTV-Ui /fo list' -ForegroundColor Cyan
    Write-Host 'Remove it with:     .\cam.ps1 -RemoveUi' -ForegroundColor Cyan
    return 0
}


function Invoke-CamRemoveUi {
    <#
        Unregisters the control UI's logon task and deletes its launcher. A
        clean no-op when neither is there, and it never touches the recorder's
        or the dashboard's task.

        A camui that is running now keeps running: nothing here stops a
        process. -Status prints the pid that owns the port when you want it
        gone.
    #>

    Write-CamSection 'remove the control UI logon task'

    if (Unregister-CamLogonTask -TaskName $UiTaskName) {
        Write-Host ("  removed the task '{0}'" -f $UiTaskName) -ForegroundColor Green
    }
    else {
        Write-Host ("  no task named '{0}' is registered" -f $UiTaskName) -ForegroundColor Yellow
    }

    if (Remove-CamLauncherStub -StubPath $UiStubPath) {
        Write-Host ("  removed the launcher '{0}'" -f $UiStubPath) -ForegroundColor Green
    }
    else {
        Write-Host ("  no launcher at '{0}'" -f $UiStubPath) -ForegroundColor Yellow
    }

    foreach ($log in @($UiStubLogPath, $UiLogPath)) {
        if (Test-Path -LiteralPath $log) {
            Write-Host ("  kept the log '{0}' - delete it by hand if you do not want it" -f $log)
        }
    }

    foreach ($other in @($RecorderTaskName, $DashboardTaskName)) {
        $task = Get-CamLogonTask -TaskName $other
        $state = 'not registered'
        if ($task) { $state = [string]$task.State }
        Write-Host ("  the '{0}' task is untouched by this: {1}" -f $other, $state) -ForegroundColor DarkGray
    }

    $ui = Get-CamUiRuntimeState -ExePath $UiExePath -ConfigPath $UiConfigPath
    Write-Host ''
    if ($ui.Listening -and $ui.IsCamUi) {
        Write-Host ("A camui is still serving {0} as pid {1} and stays up. Stop it with:  Stop-Process -Id {1}" -f `
            $ui.Url, $ui.OwnerPid) -ForegroundColor Cyan
    }
    else {
        Write-Host ("Nothing is serving {0}." -f $ui.Address) -ForegroundColor Cyan
    }
    return 0
}


# ---------------------------------------------------------------------------
# -Install and -Uninstall: every component in one command each
#
# These orchestrate the granular switches, they do not reimplement them: each
# component's install and remove IS that switch, so the two paths cannot
# drift. What is added here is the preflight, the "already in place" test
# that makes a second run change nothing, and one result per component.
# ---------------------------------------------------------------------------

function Get-CamDashboardMpvPath {
    <#
        The mpv the dashboard runs, read from the dashboard script's own
        $MpvPath assignment so there is one place that names it. $null when
        that assignment is not a plain string.
    #>
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($DashboardScript, [ref]$null, [ref]$null)
    $assignment = $ast.Find({
            param($node)
            $node -is [System.Management.Automation.Language.AssignmentStatementAst] -and
            $node.Left -is [System.Management.Automation.Language.VariableExpressionAst] -and
            $node.Left.VariablePath.UserPath -eq 'MpvPath'
        }, $false)
    if (-not $assignment) { return $null }
    $value = $assignment.Right.Expression
    if ($value -is [System.Management.Automation.Language.StringConstantExpressionAst]) { return $value.Value }
    return $null
}


function Get-CamInstallPreflight {
    <#
        Every prerequisite of every component, checked without changing
        anything. One row per check: whether it holds, what was found, the
        fix, and the component that needs it. Hard is false only for the UI,
        which is optional - a missing camui.exe skips the UI and nothing else.

        The credential file is tested for existence and never opened.
    #>
    $rows = New-Object System.Collections.ArrayList
    function Add-Row([string]$Name, [string]$Component, [bool]$Ok, [string]$Detail, [string]$Fix) {
        [void]$rows.Add([pscustomobject]@{
                Name = $Name; Component = $Component; Hard = ($Component -ne 'ui')
                Ok = $Ok; Detail = $Detail; Fix = $Fix
            })
    }

    $configPath = Join-Path -Path $ScriptDir -ChildPath 'recorder.config.json'
    if ($ConfigProblem) {
        Add-Row 'recorder.config.json' 'all' $false ("does not load: {0}" -f $ConfigProblem) `
            'fix the value it names; every key is described in docs/configuration.md'
    }
    else {
        Add-Row 'recorder.config.json' 'all' $true $(if (Test-Path -LiteralPath $configPath) { "loads: $configPath" } else { 'not present, so every default applies' }) ''
    }

    $camerasPath = Join-Path -Path $ScriptDir -ChildPath 'cameras.json'
    try {
        $cameras = Get-CamList -Path $camerasPath
        if ($cameras.Count -eq 0) { throw 'it lists no camera' }
        [void](Get-CamRelayPathMap -Cameras $cameras)
        Add-Row 'cameras.json' 'all' $true ("{0} camera(s) in {1}" -f $cameras.Count, $camerasPath) ''
    }
    catch {
        Add-Row 'cameras.json' 'all' $false ("{0}: {1}" -f $camerasPath, $_.Exception.Message) `
            'create or correct it - the schema is in docs/install.md'
    }

    $credentialPath = Join-Path -Path $ScriptDir -ChildPath 'camera_creds.xml'
    Add-Row 'camera credentials' 'all' (Test-Path -LiteralPath $credentialPath) $credentialPath `
        'run .\mpv.cam.autolayout.autorestart.ps1 once: it asks for the camera login and saves it encrypted (docs/install.md)'

    if ($Config) {
        $ffmpeg = [string]$Config.FfmpegPath
        Add-Row 'ffmpeg' 'recorder' (Test-Path -LiteralPath $ffmpeg) $ffmpeg `
            'install ffmpeg there, or set FfmpegPath in recorder.config.json'

        $mediamtx = [string]$Config.RestreamerExePath
        Add-Row 'MediaMTX' 'restreamer' (Test-Path -LiteralPath $mediamtx) $mediamtx `
            'extract mediamtx.exe from a mediamtx_<version>_windows_amd64.zip release there, or set RestreamerExePath'

        $relayConfig = [string]$Config.RestreamerConfigPath
        Add-Row 'MediaMTX config path' 'restreamer' ($relayConfig -notmatch '^\\\\wsl') $relayConfig `
            'set RestreamerConfigPath to a local NTFS path such as C:\mediamtx\mediamtx.yml - MediaMTX cannot watch a file on the WSL share'
    }
    else {
        Add-Row 'ffmpeg, MediaMTX' 'recorder' $false 'cannot be checked until recorder.config.json loads, because it names their paths' `
            'fix recorder.config.json first'
    }

    $mpv = Get-CamDashboardMpvPath
    Add-Row 'mpv' 'dashboard' ($mpv -and (Test-Path -LiteralPath $mpv)) $(if ($mpv) { $mpv } else { "no `$MpvPath in $DashboardScript" }) `
        'install mpv there, or change $MpvPath at the top of mpv.cam.autolayout.autorestart.ps1'

    $wt = Get-Command -Name 'wt.exe' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    Add-Row 'Windows Terminal' 'all' ([bool]$wt) $(if ($wt) { $wt.Source } else { 'wt.exe is not on PATH' }) `
        'install Windows Terminal from the Microsoft Store; the NVR and the dashboard open in it'

    $scheduler = [bool](Get-Command -Name 'Register-ScheduledTask' -ErrorAction SilentlyContinue)
    Add-Row 'ScheduledTasks module' 'all' $scheduler $(if ($scheduler) { 'available' } else { 'not available' }) `
        'needs Windows 8 or later'

    Add-Row 'camui.exe' 'ui' (Test-Path -LiteralPath $UiExePath) $UiExePath `
        'cross-compile it with ui/build.sh (docs/go-toolchain.md is the one-time Go install), then run -Install again'

    return $rows.ToArray()
}


function Get-CamInstallComponent {
    <#
        The components, in install order. Each one is a granular switch plus
        a read-only test of whether it is already in place:

            Test     returns Current (in place and starting the right thing),
                     Present (something to remove) and Detail
            Install  the granular install switch
            Remove   the granular remove switch
            Change   what installing it writes, for -WhatIf
            Undo     what removing it deletes, for -WhatIf
    #>
    $relayConfig = $(if ($Config) { [string]$Config.RestreamerConfigPath } else { '' })

    return @(
        [pscustomobject]@{
            Name    = 'restreamer config'
            Key     = 'restreamer'
            Change  = "write $relayConfig from cameras.json"
            Undo    = "delete $relayConfig"
            Test    = {
                $path = [string]$Config.RestreamerConfigPath
                if (-not (Test-Path -LiteralPath $path)) {
                    return [pscustomobject]@{ Current = $false; Present = $false; Detail = "no MediaMTX config at $path" }
                }
                try {
                    $check = Test-CamRelayConfigCurrent -Config $Config -PathMap (Get-CamRelayPathMap -Cameras (Get-CamList)) -ConfigPath $path
                    return [pscustomobject]@{ Current = $check.Current; Present = $true; Detail = $check.Detail }
                }
                catch { return [pscustomobject]@{ Current = $false; Present = $true; Detail = $_.Exception.Message } }
            }
            Install = { & $RecorderScript -InstallRestreamer }
            Remove  = { & $RecorderScript -RemoveRestreamer }
        },
        [pscustomobject]@{
            Name    = 'recorder logon task'
            Key     = 'recorder'
            Change  = "register the task '$RecorderTaskName' and write its launcher $RecorderStubPath"
            Undo    = "unregister the task '$RecorderTaskName' and delete its launcher $RecorderStubPath"
            Test    = { Test-CamLogonTaskInstalled -TaskName $RecorderTaskName -StubPath $RecorderStubPath -Target $RecorderScript }
            Install = { & $RecorderScript -InstallAutostart }
            Remove  = { & $RecorderScript -RemoveAutostart }
        },
        [pscustomobject]@{
            Name    = 'dashboard logon task'
            Key     = 'dashboard'
            Change  = "register the task '$DashboardTaskName' and write its launcher $DashboardStubPath"
            Undo    = "unregister the task '$DashboardTaskName' and delete its launcher $DashboardStubPath"
            Test    = { Test-CamLogonTaskInstalled -TaskName $DashboardTaskName -StubPath $DashboardStubPath -Target (Join-Path -Path $ScriptDir -ChildPath 'cam.ps1') }
            Install = { Invoke-CamInstallDashboardAutostart }
            Remove  = { Invoke-CamRemoveDashboardAutostart }
        },
        [pscustomobject]@{
            Name    = 'UI logon task'
            Key     = 'ui'
            Change  = "register the task '$UiTaskName' and write its launcher $UiStubPath"
            Undo    = "unregister the task '$UiTaskName' and delete its launcher $UiStubPath"
            Test    = { Test-CamLogonTaskInstalled -TaskName $UiTaskName -StubPath $UiStubPath -Target $UiExePath }
            Install = { Invoke-CamInstallUi }
            Remove  = { Invoke-CamRemoveUi }
        }
    )
}


function Test-CamElevated {
    return ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
}


function Test-CamEventSourceRegistered {
    return (Test-CamAlertEventSource -Source (Get-CamAlertEventSource))
}


function Invoke-CamInstallEventSourceStep {
    & $RecorderScript -InstallEventSource
}


function Write-CamComponentResult {
    <# The per-component table both -Install and -Uninstall end with. #>
    param([Parameter(Mandatory = $true)][AllowEmptyCollection()][object[]]$Result)

    foreach ($row in $Result) {
        $colour = 'Green'
        if ($row.Status -in @('failed', 'skipped')) { $colour = 'Red' }
        elseif ($row.Status -like 'would *') { $colour = 'Cyan' }
        Write-Host ("  {0,-22} {1,-18} {2}" -f $row.Name, $row.Status, $row.Detail) -ForegroundColor $colour
    }
}


function Invoke-CamInstallAll {
    <#
        -Install: preflight, then every component in order, then one result
        per component. Returns the exit code: 0 only when every component is
        in place, 1 when any failed or was skipped, or when the preflight
        stopped it.

        Components are independent, so a failure in one does not stop the
        next. Each install is verified by running the component's test again
        afterwards; the granular switch saying it worked is not the evidence.
    #>
    [CmdletBinding(SupportsShouldProcess = $true)]
    param()

    Write-CamSection 'install: prerequisites'
    $checks = @(Get-CamInstallPreflight)
    foreach ($check in $checks) {
        if ($check.Ok) {
            Write-Host ("  ok       {0,-22} {1}" -f $check.Name, $check.Detail)
            continue
        }
        $label = $(if ($check.Hard) { 'MISSING' } else { 'missing' })
        $colour = $(if ($check.Hard) { 'Red' } else { 'Yellow' })
        Write-Host ("  {0,-8} {1,-22} {2}" -f $label, $check.Name, $check.Detail) -ForegroundColor $colour
        Write-Host ("  {0,-8} {1,-22} fix: {2}" -f '', '', $check.Fix) -ForegroundColor $colour
    }

    $blocking = @($checks | Where-Object { -not $_.Ok -and $_.Hard })
    if ($blocking.Count -gt 0) {
        Write-Host ''
        Write-Host ("{0} prerequisite(s) missing. Nothing was installed or changed. Fix them and run -Install again." -f $blocking.Count) -ForegroundColor Red
        return 1
    }

    $results = New-Object System.Collections.ArrayList
    foreach ($component in @(Get-CamInstallComponent)) {
        $missing = @($checks | Where-Object { -not $_.Ok -and $_.Component -eq $component.Key })
        if ($missing.Count -gt 0) {
            [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'skipped'
                    Detail = ('{0} is missing: {1}' -f $missing[0].Name, $missing[0].Fix) })
            continue
        }

        try {
            $before = & $component.Test
            if ($before.Current) {
                [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'already installed'; Detail = $before.Detail })
                continue
            }
            if (-not $PSCmdlet.ShouldProcess($component.Change, ('install the ' + $component.Name))) {
                if ($WhatIfPreference) {
                    [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'would install'; Detail = $before.Detail })
                }
                else {
                    [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'skipped'; Detail = 'declined at the prompt' })
                }
                continue
            }
            [void](& $component.Install)
            $after = & $component.Test
            if ($after.Current) {
                [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'installed'; Detail = $after.Detail })
            }
            else {
                [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'failed'; Detail = ('still not in place: ' + $after.Detail) })
            }
        }
        catch {
            [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'failed'; Detail = $_.Exception.Message })
        }
    }

    Write-CamSection 'install: result'
    Write-CamComponentResult -Result $results.ToArray()

    # Optional, and never part of the exit code: without it every alert still
    # reaches the Application log under the fallback source.
    Write-Host ''
    if (Test-CamEventSourceRegistered) {
        Write-Host '  event source           already registered' -ForegroundColor Green
    }
    elseif (-not (Test-CamElevated)) {
        Write-Host '  OPTIONAL  the IPCCTV event source needs an elevated session once. From an elevated PowerShell here:' -ForegroundColor DarkGray
        Write-Host '              .\cam.recorder.ps1 -InstallEventSource' -ForegroundColor DarkGray
        Write-Host '            Alerts reach the Application log without it.' -ForegroundColor DarkGray
    }
    elseif ($PSCmdlet.ShouldProcess('the IPCCTV Application-log event source', 'register')) {
        try { Invoke-CamInstallEventSourceStep }
        catch { Write-Host ("  event source           not registered: {0}. It is optional." -f $_.Exception.Message) -ForegroundColor Yellow }
    }

    $bad = @($results | Where-Object { $_.Status -in @('failed', 'skipped') })
    Write-Host ''
    if ($bad.Count -gt 0) {
        Write-Host ("{0} component(s) not in place: {1}" -f $bad.Count, (($bad | ForEach-Object { $_.Name }) -join ', ')) -ForegroundColor Red
        return 1
    }
    if ($WhatIfPreference) {
        Write-Host 'That is the plan. Nothing was changed.' -ForegroundColor Cyan
    }
    else {
        Write-Host 'Everything is installed. Nothing was started: .\cam.ps1 -Start does that now, and logon does it from here on.' -ForegroundColor Cyan
    }
    return 0
}


function Invoke-CamUninstallAll {
    <#
        -Uninstall: removes every component, in the reverse of install order,
        and verifies each removal by testing again. Returns 0 unless a removal
        failed. A component with nothing left of it is reported as not
        installed, which is what makes a second run a clean no-op.
    #>
    [CmdletBinding(SupportsShouldProcess = $true)]
    param()

    $components = @(Get-CamInstallComponent)
    [array]::Reverse($components)

    $results = New-Object System.Collections.ArrayList
    foreach ($component in $components) {
        try {
            $before = & $component.Test
            if (-not $before.Present) {
                [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'not installed'; Detail = $before.Detail })
                continue
            }
            if (-not $PSCmdlet.ShouldProcess($component.Undo, ('remove the ' + $component.Name))) {
                $status = $(if ($WhatIfPreference) { 'would remove' } else { 'kept' })
                [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = $status; Detail = $before.Detail })
                continue
            }
            [void](& $component.Remove)
            $after = & $component.Test
            if ($after.Present) {
                [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'failed'; Detail = ('still there: ' + $after.Detail) })
            }
            else {
                [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'removed'; Detail = '' })
            }
        }
        catch {
            [void]$results.Add([pscustomobject]@{ Name = $component.Name; Status = 'failed'; Detail = $_.Exception.Message })
        }
    }

    Write-CamSection 'uninstall: result'
    Write-CamComponentResult -Result $results.ToArray()
    Write-Host ''
    Write-Host ("  kept      the logs in {0}, the archive, cameras.json, recorder.config.json and the credentials" -f (Get-CamAutostartDir)) -ForegroundColor DarkGray
    Write-Host '  running   nothing was stopped. .\cam.ps1 -Stop stops the recorder and the dashboard; it leaves the UI' -ForegroundColor DarkGray
    Write-Host '            running on purpose, and -Status prints the pid serving it.' -ForegroundColor DarkGray
    if (Test-CamEventSourceRegistered) {
        Write-Host '  alerts    the IPCCTV event source is left registered. Removing it needs an elevated session:' -ForegroundColor DarkGray
        Write-Host '              Remove-EventLog -Source IPCCTV' -ForegroundColor DarkGray
    }

    $failed = @($results | Where-Object { $_.Status -eq 'failed' })
    Write-Host ''
    if ($failed.Count -gt 0) {
        Write-Host ("{0} component(s) could not be removed: {1}" -f $failed.Count, (($failed | ForEach-Object { $_.Name }) -join ', ')) -ForegroundColor Red
        return 1
    }
    if ($WhatIfPreference) { Write-Host 'That is the plan. Nothing was changed.' -ForegroundColor Cyan }
    return 0
}


function Get-CamReturnedCode {
    <#
        The exit code one of the Invoke-Cam* functions returned.

        Each ends in `return 0` or `return 1`, but each also calls another
        script on the way - the recorder's own -Stop, the dashboard launcher -
        and anything either of those leaves on the pipeline comes back as part
        of the same return value. Taking the last integer keeps the exit code
        honest whatever else came past, which matters now that a caller acts on
        it: exit 0 from a start that started nothing is item [63].
    #>
    param([AllowEmptyCollection()][AllowNull()]$Returned)

    $codes = @(@($Returned) | Where-Object { $_ -is [int] })
    if ($codes.Count -eq 0) { return 0 }
    return $codes[-1]
}


if ($PSBoundParameters.ContainsKey('WhatIf') -and $PSCmdlet.ParameterSetName -notin @('Install', 'Uninstall')) {
    throw '-WhatIf previews -Install and -Uninstall only.'
}

switch ($PSCmdlet.ParameterSetName) {
    'Install' { exit (Get-CamReturnedCode -Returned (Invoke-CamInstallAll)) }
    'Uninstall' { exit (Get-CamReturnedCode -Returned (Invoke-CamUninstallAll)) }
    'Stop' { exit (Get-CamReturnedCode -Returned (Invoke-CamStopAll)) }
    'Start' { exit (Get-CamReturnedCode -Returned (Invoke-CamStartAll)) }
    'InstallDashboardAutostart' { exit (Invoke-CamInstallDashboardAutostart) }
    'RemoveDashboardAutostart' { exit (Invoke-CamRemoveDashboardAutostart) }
    'InstallUi' { exit (Invoke-CamInstallUi) }
    'RemoveUi' { exit (Invoke-CamRemoveUi) }
    default {
        # -Status, and the bare invocation, which is the same question.
        Show-CamStatus -State (Get-CamComponentState)
        Write-Host ''
        exit 0
    }
}
