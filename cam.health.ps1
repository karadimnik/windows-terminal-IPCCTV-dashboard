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

    Four sections, each switchable in recorder.config.json through
    HealthSections, in the order that key lists them:

        status   one line - supervisor, relay, free space, days left
        cameras  the per-camera table with its column header
        events   the supervisor's most recent events, newest first
        archive  footage span and size, then the disk trend and pruning

    Remove a name to drop that section. An empty list renders an empty strip.

    When no supervisor is behind the file it says so in the first row rather than
    presenting history as the current state, exactly as cam.recorder.ps1 -Status
    does.

    Targets Windows PowerShell 5.1.

.EXAMPLE
    .\cam.health.ps1
    Fits itself to the console window it is in, refreshing in place.

.EXAMPLE
    .\cam.health.ps1 -Once -Rows 13 -Width 209
    One frame as plain text at an exact size - what the test harness and a
    report use.
#>
[CmdletBinding()]
param(
    # Total lines written per frame. 0 means "fit the console window", which is
    # what the dashboard wants: it places the strip window by pixel and the row
    # count then follows from whatever font Windows Terminal is using.
    [ValidateRange(0, 60)]
    [int]$Rows = 0,

    # Columns to pad every row to. 0 means "fit the console window".
    [ValidateRange(0, 400)]
    [int]$Width = 0,

    [ValidateRange(1, 300)]
    [int]$RefreshSeconds = 2,

    # Render one frame and exit, without cursor tricks or colour positioning.
    [switch]$Once
)

$ErrorActionPreference = 'Stop'

. (Join-Path -Path $PSScriptRoot -ChildPath 'cam.common.ps1')


$script:CamStripRowFormat = '{0,-22} {1,-4} {2,-5} {3,-6} {4,-8} {5,-5} {6}'

# Every section this renderer knows about. HealthSections selects from these.
$script:CamStripSectionNames = @('status', 'cameras', 'events', 'archive')

# Label column for the sections that lead with one, so their data lines up.
$script:CamStripLabelWidth = 8

# Used only when the console cannot be measured, which is what happens when the
# output is redirected to a file and no size was given.
$script:CamStripFallbackRows = 13
$script:CamStripFallbackWidth = 120


function Write-CamStripLine {
    <#
        One row, redacted and padded to the strip width. Padding is what lets
        the renderer overwrite the previous frame in place instead of clearing
        the console, which would flicker on every refresh.
    #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [string]$Colour = 'Gray',
        [Parameter(Mandatory = $true)][int]$Width,
        # The last row of a frame drawn in place. Without this the newline
        # after the bottom row scrolls the console by one and the frame walks
        # off the top.
        [switch]$NoNewline
    )

    $line = Get-CamRedacted -Text $Text
    if ($line.Length -gt $Width) { $line = $line.Substring(0, $Width) }
    Write-Host $line.PadRight($Width) -ForegroundColor $Colour -NoNewline:$NoNewline
}


function Get-CamStripText {
    <# Truncates to a column budget, marking that something was cut. #>
    param([AllowEmptyString()][string]$Text, [Parameter(Mandatory = $true)][int]$Limit)

    if ($null -eq $Text) { return '' }
    if ($Limit -lt 1) { return '' }
    if ($Text.Length -le $Limit) { return $Text }
    if ($Limit -le 3) { return $Text.Substring(0, $Limit) }
    return $Text.Substring(0, $Limit - 3) + '...'
}


function Get-CamStripLabelled {
    <# A section line: fixed label column, then the text truncated to fit. #>
    param(
        [AllowEmptyString()][string]$Label,
        [AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][int]$Width
    )

    $budget = $Width - $script:CamStripLabelWidth - 1
    return ("{0,-$($script:CamStripLabelWidth)} {1}" -f $Label, (Get-CamStripText -Text $Text -Limit $budget))
}


function Resolve-CamStripSize {
    <#
        The row and column budget for one frame.

        A 0 for either means "fit the console", re-measured on every frame so
        the strip follows the window the dashboard resizes it to after Windows
        Terminal has started, and follows the Operator if he resizes it later.

        One column is deliberately left unused: writing a line the full width
        of the console makes it wrap on some hosts, which costs a whole row.
    #>
    param([int]$Rows, [int]$Width)

    $resolvedRows = $Rows
    $resolvedWidth = $Width

    if ($resolvedRows -le 0 -or $resolvedWidth -le 0) {
        $size = $null
        try { $size = $Host.UI.RawUI.WindowSize } catch { }
        if ($resolvedRows -le 0) {
            $resolvedRows = $script:CamStripFallbackRows
            if ($size -and $size.Height -gt 0) { $resolvedRows = [int]$size.Height }
        }
        if ($resolvedWidth -le 0) {
            $resolvedWidth = $script:CamStripFallbackWidth
            if ($size -and $size.Width -gt 1) { $resolvedWidth = [int]$size.Width - 1 }
        }
    }

    if ($resolvedRows -lt 1) { $resolvedRows = 1 }
    if ($resolvedRows -gt 60) { $resolvedRows = 60 }
    if ($resolvedWidth -lt 40) { $resolvedWidth = 40 }
    if ($resolvedWidth -gt 400) { $resolvedWidth = 400 }

    return [pscustomobject]@{ Rows = $resolvedRows; Width = $resolvedWidth }
}


function Resolve-CamStripSections {
    <#
        The sections to render, in the order HealthSections lists them,
        de-duplicated. An empty or missing list renders an empty strip, which
        is the point: every section can be switched off.

        An unrecognised name throws rather than being ignored, because a typo
        that silently drops a section would have the Operator looking for
        output that is never coming.
    #>
    param([Parameter(Mandatory = $true)]$Config)

    $names = @()
    foreach ($entry in @($Config.HealthSections)) {
        $name = ([string]$entry).Trim().ToLowerInvariant()
        if (-not $name) { continue }
        if ($script:CamStripSectionNames -notcontains $name) {
            throw ("HealthSections contains '{0}', which is not a section name. Valid names are: {1}." -f `
                $entry, ($script:CamStripSectionNames -join ', '))
        }
        if ($names -notcontains $name) { $names += $name }
    }
    return $names
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


function Get-CamStripStatusRows {
    <#
        The state line: one row, carrying the staleness warning on its left
        where truncation can never reach it.
    #>
    param(
        [Parameter(Mandatory = $true)]$Snapshot,
        [Parameter(Mandatory = $true)]$State,
        [Parameter(Mandatory = $true)][int]$Width
    )

    $disk = $Snapshot.Disk
    $diskText = ("free {0} GB (reserve {1})" -f `
        (Format-CamNumber -Value ([double]$disk.FreeGB)), (Format-CamNumber -Value ([double]$disk.ReserveGB)))
    # '-' for an unknown figure, the same convention the table below uses. The
    # rate needs about ten minutes of archive history before it exists.
    $daysText = 'days left -'
    if ($disk.ProjectedDaysRemaining) {
        $daysText = ("{0}d left" -f (Format-CamNumber -Value ([double]$disk.ProjectedDaysRemaining) -Format '0.0'))
    }

    if ($State.Running) {
        return @(@{
            Text   = ("RUNNING pid {0} up {1} | {2} | {3} | {4}" -f `
                $State.Pid, (Format-CamAgeText -Seconds $Snapshot.UptimeSeconds), `
                (Get-CamStripRelayText -Restreamer $Snapshot.Restreamer), $diskText, $daysText)
            Colour = 'Green'
        })
    }

    # Loud and first, where truncation can never reach it: a stale strip that
    # cannot be told apart from a live one is worse than no strip.
    #
    # No relay state here. MediaMTX is a child of the supervisor, so with no
    # supervisor there is no relay, and saying otherwise from a stale file
    # would be a guess.
    #
    # The reason gets whatever the fixed parts of the line leave, rather than a
    # fixed 42 columns, so a wide strip shows the whole sentence and a narrow
    # one still keeps the warning and the disk figure intact.
    $template = 'STALE - NO SUPERVISOR ({0} ago): {1} | last known {2}'
    $ageText = Format-CamAgeText -Seconds $State.AgeSeconds
    $reasonBudget = $Width - ($template -f $ageText, '', $diskText).Length
    if ($reasonBudget -lt 12) { $reasonBudget = 12 }

    return @(@{
        Text   = ($template -f $ageText, (Get-CamStripText -Text $State.Reason -Limit $reasonBudget), $diskText)
        Colour = 'Red'
    })
}


function Get-CamStripCameraRows {
    <#
        The column header plus one row per camera, within $MaxRows. When the
        cameras do not fit, one row is given up to say how many were dropped -
        a table that silently ends early would read as "that is all of them".
    #>
    param(
        [Parameter(Mandatory = $true)]$Snapshot,
        [Parameter(Mandatory = $true)]$State,
        [Parameter(Mandatory = $true)][int]$MaxRows,
        [Parameter(Mandatory = $true)][int]$Width
    )

    if ($MaxRows -lt 1) { return @() }

    $rows = @(@{
        Text   = ($script:CamStripRowFormat -f 'CAMERA', 'REC', 'ALIVE', 'MODE', 'SEG AGE', 'RST/h', 'LAST ERROR')
        Colour = 'Cyan'
    })

    # Width already consumed by the fixed columns, so the error column takes
    # exactly the rest of the line.
    $errorWidth = $Width - ($script:CamStripRowFormat -f '', '', '', '', '', '', '').Length
    if ($errorWidth -lt 10) { $errorWidth = 10 }

    $cameras = @($Snapshot.Cameras)
    $slots = $MaxRows - 1
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
        if (-not $State.Running) { $colour = 'DarkGray' }

        $rows += @{
            Text   = ($script:CamStripRowFormat -f `
                (Get-CamStripText -Text ([string]$camera.Name) -Limit 22), $record, $alive, $mode, `
                (Format-CamAgeText -Seconds $camera.SecondsSinceLastClosedSegment), `
                [int]$camera.RestartsLastHour, (Get-CamStripText -Text $errorText -Limit $errorWidth))
            Colour = $colour
        }
    }

    if ($shown -lt $cameras.Count -and $rows.Count -lt $MaxRows) {
        $rows += @{
            Text   = ("... {0} more camera(s) not shown - give the strip {1} rows for this section" -f `
                ($cameras.Count - $shown), ($cameras.Count + 1))
            Colour = 'Yellow'
        }
    }

    if ($rows.Count -gt $MaxRows) { $rows = @($rows[0..($MaxRows - 1)]) }
    return $rows
}


function Get-CamStripEventColour {
    <# Events worth noticing get a colour; routine ones stay quiet. #>
    param([AllowEmptyString()][string]$Text)

    if ($Text -match '(?i)(fail|error|denied|refus|cannot|guard)') { return 'Red' }
    if ($Text -match '(?i)(kill|watchdog|restart|prune|stop|exit)') { return 'Yellow' }
    return 'Gray'
}


function Get-CamStripEventRows {
    <#
        The supervisor's most recent events, newest first, from the Events list
        in status.json - which is the tail of the supervisor log, already
        timestamped and redacted by the writer.

        Only the clock is shown. The date is almost always today and the full
        timestamp would cost a fifth of the line.

        Always exactly $MaxRows rows, padded when there are fewer events, so
        the sections below it never move between frames.
    #>
    param(
        [Parameter(Mandatory = $true)]$Snapshot,
        [Parameter(Mandatory = $true)][int]$MaxRows,
        [Parameter(Mandatory = $true)][int]$Width
    )

    if ($MaxRows -lt 1) { return @() }

    $events = @($Snapshot.Events | Where-Object { $_ })
    $rows = @()

    if ($events.Count -eq 0) {
        $rows += @{
            Text   = (Get-CamStripLabelled -Label 'EVENTS' -Text 'nothing logged yet' -Width $Width)
            Colour = 'DarkGray'
        }
    }
    else {
        for ($i = 0; $i -lt $MaxRows -and $i -lt $events.Count; $i++) {
            # Not $event: PowerShell uses $Event as an automatic variable.
            $entry = $events[$i]
            $clock = '--:--:--'
            if ([string]$entry.At -match '\d{2}:\d{2}:\d{2}') { $clock = $Matches[0] }
            $label = ''
            if ($i -eq 0) { $label = 'EVENTS' }
            $rows += @{
                Text   = (Get-CamStripLabelled -Label $label -Text ("{0} {1}" -f $clock, [string]$entry.Text) -Width $Width)
                Colour = (Get-CamStripEventColour -Text ([string]$entry.Text))
            }
        }
    }

    while ($rows.Count -lt $MaxRows) { $rows += @{ Text = ''; Colour = 'Gray' } }
    return $rows
}


function Get-CamStripArchiveRows {
    <#
        Two rows: what footage exists, and where the disk is heading.

        Every figure is read from status.json's Disk and Prune blocks, which
        the supervisor measured on its own archive scan. Nothing here walks the
        filesystem - the strip refreshes every couple of seconds and the
        archive is tens of thousands of files.
    #>
    param(
        [Parameter(Mandatory = $true)]$Snapshot,
        [Parameter(Mandatory = $true)][int]$MaxRows,
        [Parameter(Mandatory = $true)][int]$Width
    )

    if ($MaxRows -lt 1) { return @() }

    $disk = $Snapshot.Disk
    $prune = $Snapshot.Prune

    $oldest = Get-CamDateTimeOrNull -Value $disk.OldestSegment
    $newest = Get-CamDateTimeOrNull -Value $disk.NewestSegment

    $spanText = 'no footage measured yet'
    if ($oldest -and $newest) {
        $spanText = ("{0} -> {1} ({2}d)" -f $oldest.ToLocalTime().ToString('MM-dd HH:mm'), $newest.ToLocalTime().ToString('MM-dd HH:mm'), `
            (Format-CamNumber -Value ($newest - $oldest).TotalDays -Format '0.0'))
    }

    $sizeText = 'size -'
    if ($null -ne $disk.ArchiveGB) {
        $sizeText = ("{0} GB in {1} file(s)" -f (Format-CamNumber -Value ([double]$disk.ArchiveGB) -Format '0.0'), [int]$disk.ArchiveFiles)
    }

    $rateText = 'rate -'
    if ($disk.MeasuredGBPerDay) {
        $rateText = ("{0} GB/day" -f (Format-CamNumber -Value ([double]$disk.MeasuredGBPerDay) -Format '0.0'))
    }
    $daysText = 'days left -'
    if ($disk.ProjectedDaysRemaining) {
        $daysText = ("{0}d left" -f (Format-CamNumber -Value ([double]$disk.ProjectedDaysRemaining) -Format '0.0'))
    }

    $pruneText = 'pruned nothing'
    $pruneColour = 'Gray'
    if ($prune) {
        if ([int]$prune.DeletedTotal -gt 0) {
            $pruneText = ("pruned {0} file(s), {1} GB" -f [int]$prune.DeletedTotal, `
                (Format-CamNumber -Value ([double]$prune.FreedGBTotal) -Format '0.000'))
        }
        if ($prune.Blocked) {
            $pruneText = ("PRUNE BLOCKED: {0}" -f [string]$prune.LastMessage)
            $pruneColour = 'Red'
        }
    }

    $rows = @(@{
        Text   = (Get-CamStripLabelled -Label 'ARCHIVE' -Text ("{0}  {1}" -f $spanText, $sizeText) -Width $Width)
        Colour = 'Gray'
    })
    if ($rows.Count -lt $MaxRows) {
        $rows += @{
            Text   = (Get-CamStripLabelled -Label 'TREND' -Text ("{0}  {1}  {2}" -f $rateText, $daysText, $pruneText) -Width $Width)
            Colour = $pruneColour
        }
    }
    return $rows
}


function Get-CamStripFrame {
    <#
        Builds the whole frame as @{ Text; Colour } rows, always exactly $Rows
        of them. Separated from the writing so the harness can assert on the
        text without a console.

        Row budget: the fixed-height sections take what they need and the
        camera table absorbs whatever is left, because the table is the part
        that grows with the camera count and the part the Operator reads first.
        Sections render in the order HealthSections lists them, and if even the
        fixed ones do not fit, the ones at the end are the ones that lose rows.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][int]$Rows,
        [Parameter(Mandatory = $true)][int]$Width
    )

    $sections = Resolve-CamStripSections -Config $Config
    $frame = New-Object System.Collections.ArrayList

    if ($sections.Count -eq 0) {
        while ($frame.Count -lt $Rows) { [void]$frame.Add(@{ Text = ''; Colour = 'Gray' }) }
        return $frame
    }

    $snapshot = Get-CamStatusSnapshot -Config $Config
    $state = Get-CamSupervisorState -Config $Config -Snapshot $snapshot

    if (-not $snapshot) {
        [void]$frame.Add(@{ Text = ("NO STATUS FILE at {0}" -f (Get-CamStatusFilePath -Config $Config)); Colour = 'Red' })
        [void]$frame.Add(@{ Text = 'No supervisor has ever run against this archive root, or the root is wrong. Start it with: cam.recorder.ps1 -Run'; Colour = 'Yellow' })
        while ($frame.Count -lt $Rows) { [void]$frame.Add(@{ Text = ''; Colour = 'Gray' }) }
        return $frame
    }

    # What each fixed-height section costs, so the camera table can be given
    # the remainder before anything is rendered.
    $eventRows = [int]$Config.HealthEventRows
    if ($eventRows -lt 1) { $eventRows = 1 }
    $fixedCost = @{ status = 1; events = $eventRows; archive = 2 }

    $reserved = 0
    foreach ($section in $sections) {
        if ($fixedCost.ContainsKey($section)) { $reserved += $fixedCost[$section] }
    }
    $cameraBudget = $Rows - $reserved

    foreach ($section in $sections) {
        $remaining = $Rows - $frame.Count
        if ($remaining -le 0) { break }

        # Not $rows: PowerShell variable names are case-insensitive, so that
        # would assign to this function's own [int]$Rows parameter.
        $sectionRows = @()
        switch ($section) {
            'status' { $sectionRows = Get-CamStripStatusRows -Snapshot $snapshot -State $state -Width $Width }
            'cameras' {
                $budget = [math]::Min($cameraBudget, $remaining)
                $sectionRows = Get-CamStripCameraRows -Snapshot $snapshot -State $state -MaxRows $budget -Width $Width
            }
            'events' {
                $sectionRows = Get-CamStripEventRows -Snapshot $snapshot -MaxRows ([math]::Min($eventRows, $remaining)) -Width $Width
            }
            'archive' {
                $sectionRows = Get-CamStripArchiveRows -Snapshot $snapshot -MaxRows ([math]::Min(2, $remaining)) -Width $Width
            }
        }

        foreach ($row in $sectionRows) {
            if ($frame.Count -ge $Rows) { break }
            [void]$frame.Add($row)
        }
    }

    while ($frame.Count -lt $Rows) { [void]$frame.Add(@{ Text = ''; Colour = 'Gray' }) }
    return $frame
}


function Show-CamStrip {
    <# Writes one frame. #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)][int]$Rows,
        [Parameter(Mandatory = $true)][int]$Width,
        # Omit the newline after the bottom row, which is what stops a frame
        # that exactly fills the console from scrolling it.
        [switch]$NoTrailingNewline
    )

    $frame = @(Get-CamStripFrame -Config $Config -Rows $Rows -Width $Width)
    for ($i = 0; $i -lt $frame.Count; $i++) {
        $last = ($NoTrailingNewline -and $i -eq ($frame.Count - 1))
        Write-CamStripLine -Text $frame[$i].Text -Colour $frame[$i].Colour -Width $Width -NoNewline:$last
    }
}


$config = Get-CamConfig

if ($Once) {
    $size = Resolve-CamStripSize -Rows $Rows -Width $Width
    Show-CamStrip -Config $config -Rows $size.Rows -Width $size.Width
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
    $lastSize = $null
    while ($true) {
        $size = Resolve-CamStripSize -Rows $Rows -Width $Width
        if ($canPosition) {
            # A resized window leaves rows of the old frame behind it, so the
            # console is cleared once on the change and then never again.
            if ($lastSize -and ($size.Rows -ne $lastSize.Rows -or $size.Width -ne $lastSize.Width)) {
                try { Clear-Host } catch { }
            }
            try { [Console]::SetCursorPosition(0, 0) } catch { $canPosition = $false }
        }
        $lastSize = $size
        Show-CamStrip -Config $config -Rows $size.Rows -Width $size.Width -NoTrailingNewline:$canPosition
        Start-Sleep -Seconds $RefreshSeconds
    }
}
finally {
    if ($canPosition) { try { [Console]::CursorVisible = $true } catch { } }
}
