<#
.SYNOPSIS
    Health strip for the IPCCTV dashboard: renders status.json in a few rows.

.DESCRIPTION
    A continuously refreshing, fixed-height table meant for the short wide strip
    the dashboard reserves along the bottom of the screen. Every row is sized to
    -Width and the output is always exactly -Rows lines, so the window never
    reflows and never scrolls.

    It reads ONE file, _state\status.json, and nothing else. It never opens an
    RTSP session, never touches the archive and never asks the restreamer
    anything: the supervisor already measured all of it, and a health view that
    competed for a camera slot would be the thing that broke recording. Two
    cameras' worth of slots is the whole budget.

    When no supervisor is behind the file it says so in the first row rather than
    presenting history as the current state, exactly as cam.recorder.ps1 -Status
    does.

    Targets Windows PowerShell 5.1.

.EXAMPLE
    .\cam.health.ps1

.EXAMPLE
    .\cam.health.ps1 -Once
    One frame as plain text - what the test harness and a report use.
#>
[CmdletBinding()]
param(
    # Total lines written per frame. The strip is about 140 px tall, which is
    # 6-8 console rows; two are the state line and the column header.
    [ValidateRange(4, 60)]
    [int]$Rows = 8,

    # Columns to pad every row to. 120 fits the 1920 px strip.
    [ValidateRange(60, 400)]
    [int]$Width = 120,

    [ValidateRange(1, 300)]
    [int]$RefreshSeconds = 2,

    # Render one frame and exit, without cursor tricks or colour positioning.
    [switch]$Once
)

$ErrorActionPreference = 'Stop'

. (Join-Path -Path $PSScriptRoot -ChildPath 'cam.common.ps1')


$script:CamStripRowFormat = '{0,-22} {1,-4} {2,-5} {3,-6} {4,-8} {5,-5} {6}'


function Write-CamStripLine {
    <#
        One row, redacted and padded to the strip width. Padding is what lets
        the renderer overwrite the previous frame in place instead of clearing
        the console, which would flicker on every refresh.
    #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [string]$Colour = 'Gray',
        [Parameter(Mandatory = $true)][int]$Width
    )

    $line = Get-CamRedacted -Text $Text
    if ($line.Length -gt $Width) { $line = $line.Substring(0, $Width) }
    Write-Host $line.PadRight($Width) -ForegroundColor $Colour
}


function Get-CamStripText {
    <# Truncates to a column budget, marking that something was cut. #>
    param([AllowEmptyString()][string]$Text, [Parameter(Mandatory = $true)][int]$Limit)

    if ($null -eq $Text) { return '' }
    if ($Text.Length -le $Limit) { return $Text }
    if ($Limit -le 3) { return $Text.Substring(0, $Limit) }
    return $Text.Substring(0, $Limit - 3) + '...'
}


function Get-CamStripRelayText {
    <#
        The restreamer in one phrase. Reported from status.json alone: whether
        each camera is actually on the relay is the per-camera MODE column, not
        a claim made here.
    #>
    param($Restreamer)

    if (-not $Restreamer) { return 'relay unknown' }
    if (-not $Restreamer.Enabled) { return 'relay off' }

    $restarts = [int]$Restreamer.RestartsTotal
    if ($Restreamer.Alive) {
        return ("relay UP pid {0}, {1} path(s), {2} restart(s)" -f `
            $Restreamer.Pid, @($Restreamer.Paths).Count, $restarts)
    }
    return ("relay DOWN, {0} restart(s)" -f $restarts)
}


function Get-CamStripFrame {
    <#
        Builds the whole frame as @{ Text; Colour } rows, always exactly $Rows
        of them. Separated from the writing so the harness can assert on the
        text without a console.

        Row 1 is the state line and carries the staleness warning on its left,
        where truncation can never reach it. Row 2 is the column header. Every
        remaining row is one camera, so an 8-row strip shows six cameras.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][int]$Rows,
        [Parameter(Mandatory = $true)][int]$Width
    )

    $snapshot = Get-CamStatusSnapshot -Config $Config
    $state = Get-CamSupervisorState -Config $Config -Snapshot $snapshot
    $frame = New-Object System.Collections.ArrayList

    if (-not $snapshot) {
        [void]$frame.Add(@{ Text = ("NO STATUS FILE at {0}" -f (Get-CamStatusFilePath -Config $Config)); Colour = 'Red' })
        [void]$frame.Add(@{ Text = 'No supervisor has ever run against this archive root, or the root is wrong. Start it with: cam.recorder.ps1 -Run'; Colour = 'Yellow' })
        while ($frame.Count -lt $Rows) { [void]$frame.Add(@{ Text = ''; Colour = 'Gray' }) }
        return $frame
    }

    $disk = $snapshot.Disk
    $diskText = ("free {0} GB (reserve {1})" -f `
        (Format-CamNumber -Value ([double]$disk.FreeGB)), (Format-CamNumber -Value ([double]$disk.ReserveGB)))
    # '-' for an unknown figure, the same convention the table below uses. The
    # rate needs about ten minutes of archive history before it exists.
    $daysText = 'days left -'
    if ($disk.ProjectedDaysRemaining) {
        $daysText = ("{0}d left" -f (Format-CamNumber -Value ([double]$disk.ProjectedDaysRemaining) -Format '0.0'))
    }

    if ($state.Running) {
        $headText = ("RUNNING pid {0} up {1} | {2} | {3} | {4}" -f `
            $state.Pid, (Format-CamAgeText -Seconds $snapshot.UptimeSeconds), `
            (Get-CamStripRelayText -Restreamer $snapshot.Restreamer), $diskText, $daysText)
        $headColour = 'Green'
    }
    else {
        # Loud and first, where truncation can never reach it: a stale strip
        # that cannot be told apart from a live one is worse than no strip.
        #
        # No relay state here. MediaMTX is a child of the supervisor, so with no
        # supervisor there is no relay, and saying otherwise from a stale file
        # would be a guess.
        $headText = ("STALE - NO SUPERVISOR ({0} ago): {1} | last known {2}" -f `
            (Format-CamAgeText -Seconds $state.AgeSeconds), `
            (Get-CamStripText -Text $state.Reason -Limit 42), $diskText)
        $headColour = 'Red'
    }
    [void]$frame.Add(@{ Text = $headText; Colour = $headColour })

    [void]$frame.Add(@{
        Text   = ($script:CamStripRowFormat -f 'CAMERA', 'REC', 'ALIVE', 'MODE', 'SEG AGE', 'RST/h', 'LAST ERROR')
        Colour = 'Cyan'
    })

    # Width already consumed by the fixed columns, so the error column takes
    # exactly the rest of the line.
    $errorWidth = $Width - ($script:CamStripRowFormat -f '', '', '', '', '', '', '').Length
    if ($errorWidth -lt 10) { $errorWidth = 10 }

    $cameras = @($snapshot.Cameras)
    $slots = $Rows - $frame.Count
    $shown = $cameras.Count
    if ($shown -gt $slots) { $shown = [math]::Max(0, $slots - 1) }

    for ($i = 0; $i -lt $shown; $i++) {
        $camera = $cameras[$i]

        $record = 'off'
        if ($camera.Record) { $record = 'ON' }

        $alive = '-'
        $colour = 'DarkGray'
        if ($camera.Record) {
            $alive = 'no'
            $colour = 'Red'
            if ($camera.Alive) { $alive = 'yes'; $colour = 'Green' }
        }

        $mode = [string]$camera.InputMode
        if (-not $mode) { $mode = '-' }

        $errorText = [string]$camera.LastError
        if ($camera.ConfigError) { $errorText = 'config: ' + [string]$camera.ConfigError }

        # Nothing below the state line can be trusted as current once the
        # heartbeat is gone, so the whole table drops to one dim colour.
        if (-not $state.Running) { $colour = 'DarkGray' }

        [void]$frame.Add(@{
            Text   = ($script:CamStripRowFormat -f `
                (Get-CamStripText -Text ([string]$camera.Name) -Limit 22), $record, $alive, $mode, `
                (Format-CamAgeText -Seconds $camera.SecondsSinceLastClosedSegment), `
                [int]$camera.RestartsLastHour, (Get-CamStripText -Text $errorText -Limit $errorWidth))
            Colour = $colour
        })
    }

    if ($shown -lt $cameras.Count) {
        [void]$frame.Add(@{
            Text   = ("... {0} more camera(s) not shown - run with -Rows {1}" -f ($cameras.Count - $shown), ($cameras.Count + 2))
            Colour = 'Yellow'
        })
    }

    while ($frame.Count -lt $Rows) { [void]$frame.Add(@{ Text = ''; Colour = 'Gray' }) }
    return $frame
}


function Show-CamStrip {
    <# Writes one frame. #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][int]$Rows,
        [Parameter(Mandatory = $true)][int]$Width
    )

    foreach ($row in (Get-CamStripFrame -Config $Config -Rows $Rows -Width $Width)) {
        Write-CamStripLine -Text $row.Text -Colour $row.Colour -Width $Width
    }
}


$config = Get-CamConfig

if ($Once) {
    Show-CamStrip -Config $config -Rows $Rows -Width $Width
    return
}

try { $Host.UI.RawUI.WindowTitle = 'cam.health' } catch { }

# Redrawing over the previous frame rather than clearing: Clear-Host on every
# refresh makes the strip flash, and the strip sits in the Operator's eyeline.
# Falls back to a plain scroll where the console cannot be addressed, which is
# what happens when the output is redirected to a file.
$canPosition = $true
try {
    [void][Console]::CursorTop
    [Console]::CursorVisible = $false
}
catch { $canPosition = $false }

try {
    while ($true) {
        if ($canPosition) {
            try { [Console]::SetCursorPosition(0, 0) } catch { $canPosition = $false }
        }
        Show-CamStrip -Config $config -Rows $Rows -Width $Width
        Start-Sleep -Seconds $RefreshSeconds
    }
}
finally {
    if ($canPosition) { try { [Console]::CursorVisible = $true } catch { } }
}
