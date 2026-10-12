<#
.SYNOPSIS
    Test harness for the IPCCTV recorder. Run it from Windows PowerShell 5.1.

.DESCRIPTION
    Every check prints its own evidence as it runs, then a tally. Exit code 0
    means every check passed.

    By default only the checks that touch no camera run, so the harness is safe
    to run with the dashboard open. The camera-facing checks are opt-in:

        -Live   runs the relay lifecycle proof, which starts MediaMTX and
                ffmpeg against the real cameras in a sandbox.

    The primitives are in cam.testlib.ps1. Extend that file and add a check
    here; do not start another harness.

.EXAMPLE
    .\tests\cam.tests.ps1

.EXAMPLE
    .\tests\cam.tests.ps1 -Checks parse,layout,strip,sections

.EXAMPLE
    .\tests\cam.tests.ps1 -Live -Checks relay
#>
[CmdletBinding()]
param(
    # Which checks to run. Default: every check that touches no camera.
    [ValidateSet('parse', 'cameralist', 'inputs', 'selftest', 'layout', 'strip', 'sections', 'autostart', 'leak', 'procs', 'relay', 'viewer')]
    [string[]]$Checks,

    # Allows the checks that open RTSP sessions against the real cameras.
    [switch]$Live,

    # Seconds the relay lifecycle check lets the supervisor run before it kills
    # MediaMTX. 60 is enough for the startup wait plus a status tick.
    [ValidateRange(20, 600)]
    [int]$RelaySeconds = 60,

    # Where the relay check writes its captured evidence.
    [string]$EvidenceDir
)

$ErrorActionPreference = 'Stop'

. (Join-Path -Path $PSScriptRoot -ChildPath 'cam.testlib.ps1')

$repo = Get-CamTestRepo
$run = New-CamTestRun

$cameraFree = @('parse', 'cameralist', 'inputs', 'viewer', 'selftest', 'layout', 'strip', 'sections', 'autostart', 'leak', 'procs')
if (-not $Checks) { $Checks = $cameraFree }
if (-not $EvidenceDir) { $EvidenceDir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\evidence' }

Write-Host ''
Write-Host ("=== cam.recorder test harness ===") -ForegroundColor 'Cyan'
Write-Host ("repo {0}" -f $repo)
Write-Host ("powershell {0} | checks: {1}" -f $PSVersionTable.PSVersion.ToString(), ($Checks -join ', '))
Write-Host ''


function Invoke-CamCheckParse {
    param($Run)

    $result = Test-CamScriptParse
    Add-CamTestResult -Run $Run -Check 'parse' -Status $(if ($result.Ok) { 'PASS' } else { 'FAIL' }) `
        -Message ("{0} script(s) parsed under PowerShell {1}, {2} with errors" -f $result.Files, $result.Version, $result.Failed) `
        -Evidence $result.Lines
}


function Invoke-CamCheckCameraList {
    param($Run)

    $result = Test-CamCameraListIntegrity
    Add-CamTestResult -Run $Run -Check 'cameralist' -Status $(if ($result.Ok) { 'PASS' } else { 'FAIL' }) `
        -Message ("cameras.json {0} bytes, sha256 and BOM {1}" -f $result.Bytes, $(if ($result.Ok) { 'as expected' } else { 'CHANGED' })) `
        -Evidence @(("sha256 {0}" -f $result.Sha256), ("bom    {0}" -f $result.Bom))
}


function Invoke-CamCheckInputs {
    <#
        The relay-or-direct decision for a BATCH of consumers, asserted against
        an unreachable control API so every answer is attributable.

        The two properties that matter are opposites, and both were broken:

        - A batch must make NO per-camera probe. Six cameras each asking about
          their own path in the same instant is what put a recorder and two mpv
          panes on direct connections while their paths were serving perfectly
          well, each burning the spare camera slot the relay exists to protect.
          Proven by the clock: the API here is a black hole, so a probe costs
          the whole timeout and a batch that made six of them could not come
          back in a few milliseconds.

        - A LONE attempt must still probe, and must still fall through to
          direct. That is what leaves a relay that died mid-run behind on the
          next attempt rather than retrying forever against a dead address.
    #>
    param($Run)

    $evidence = @()
    $ok = $true

    # 10.254.254.254 is not routable, so every probe runs the clock out instead
    # of being refused. 400 ms keeps the check quick while staying far above the
    # few ms a decision needs when no probe is made.
    $config = Get-CamConfigDefault
    $config.RelayApiBase = 'http://10.254.254.254:9997'
    $config.RelayProbeTimeoutMs = 400
    $config.RelayReadyPollMs = 100
    $direct = 'rtsp://user:secret@192.168.50.15/stream1'

    function Measure-CamChoice {
        param([hashtable]$Arguments)
        $watch = [System.Diagnostics.Stopwatch]::StartNew()
        $choice = Get-CamInputUrl @Arguments
        $watch.Stop()
        return [pscustomobject]@{ Mode = $choice.Mode; Url = $choice.Url; Ms = [int]$watch.ElapsedMilliseconds }
    }

    function Assert-CamChoice {
        param([string]$Label, [hashtable]$Arguments, [string]$WantMode, [int]$MaxMs = 0, [int]$MinMs = 0)
        $result = Measure-CamChoice -Arguments $Arguments
        $bad = @()
        if ($result.Mode -ne $WantMode) { $bad += ("mode {0} want {1}" -f $result.Mode, $WantMode) }
        if ($MaxMs -gt 0 -and $result.Ms -gt $MaxMs) { $bad += ("took {0} ms, want at most {1}" -f $result.Ms, $MaxMs) }
        if ($MinMs -gt 0 -and $result.Ms -lt $MinMs) { $bad += ("took {0} ms, want at least {1}" -f $result.Ms, $MinMs) }
        $line = "{0,-62} {1,-7} {2,5} ms" -f $Label, $result.Mode, $result.Ms
        if ($bad.Count -gt 0) { $line += ('  FAIL: ' + ($bad -join '; ')) }
        $script:CamInputLine = $line
        return ($bad.Count -eq 0)
    }

    # The real camera list, one shared serving set, one camera missing from it:
    # exactly the cold start this check exists for. The whole batch is timed
    # together - six probes at 400 ms each could not fit in 200 ms.
    $cameras = Get-CamList -Path (Join-Path -Path $repo -ChildPath 'cameras.json')
    $paths = @(foreach ($camera in $cameras) { Get-CamRelayPathName -SafeName (Get-CamSafeName -Name $camera.Name) })
    $serving = @($paths | Select-Object -Skip 1)
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $modes = @(foreach ($path in $paths) {
        (Get-CamInputUrl -Config $config -RelayPathName $path -DirectUrl $direct -ReadyPaths $serving).Mode
    })
    $watch.Stop()
    $relayCount = @($modes | Where-Object { $_ -eq 'relay' }).Count
    $evidence += ("batch of {0} cameras against a serving set of {1}: {2} relay, {3} direct, decided in {4} ms with no probe" -f `
        $paths.Count, $serving.Count, $relayCount, ($paths.Count - $relayCount), [int]$watch.ElapsedMilliseconds)
    $evidence += ("  " + (($paths | ForEach-Object { $_ }) -join ', '))
    $evidence += ("  " + ($modes -join ', '))
    if ($relayCount -ne $serving.Count -or $modes[0] -ne 'direct') {
        $ok = $false
        $evidence += 'FAIL: the shared serving set did not decide every camera'
    }
    if ($watch.ElapsedMilliseconds -gt 200) {
        $ok = $false
        $evidence += 'FAIL: the batch spent long enough to have probed the API per camera'
    }

    $checks = @(
        @{ Label = 'in the batch set, unreachable API'
           Arguments = @{ Config = $config; RelayPathName = 'frontentrance'; DirectUrl = $direct; ReadyPaths = @('frontentrance', 'rearbalcony') }
           WantMode = 'relay'; MaxMs = 100 }
        @{ Label = 'not in the batch set, no wait'
           Arguments = @{ Config = $config; RelayPathName = 'frontentrance'; DirectUrl = $direct; ReadyPaths = @('rearbalcony') }
           WantMode = 'direct'; MaxMs = 100 }
        @{ Label = 'empty batch set, no wait'
           Arguments = @{ Config = $config; RelayPathName = 'frontentrance'; DirectUrl = $direct; ReadyPaths = @() }
           WantMode = 'direct'; MaxMs = 100 }
        @{ Label = 'lone attempt, no batch set: still probes, falls to direct'
           Arguments = @{ Config = $config; RelayPathName = 'frontentrance'; DirectUrl = $direct }
           WantMode = 'direct'; MinMs = 350 }
        @{ Label = 'not in the batch set, 1s wait: bounded, then direct'
           Arguments = @{ Config = $config; RelayPathName = 'frontentrance'; DirectUrl = $direct; ReadyPaths = @(); WaitSeconds = 1 }
           WantMode = 'direct'; MinMs = 900; MaxMs = 3000 }
        @{ Label = 'no relay path name at all'
           Arguments = @{ Config = $config; RelayPathName = ''; DirectUrl = $direct; ReadyPaths = @('frontentrance') }
           WantMode = 'direct'; MaxMs = 100 }
    )
    foreach ($check in $checks) {
        if (-not (Assert-CamChoice @check)) { $ok = $false }
        $evidence += $script:CamInputLine
    }

    # The restreamer switched off must never produce a relay URL, whatever the
    # batch says.
    $disabled = Get-CamConfigDefault
    $disabled.RestreamerEnabled = $false
    $offChoice = Get-CamInputUrl -Config $disabled -RelayPathName 'frontentrance' -DirectUrl $direct -ReadyPaths @('frontentrance')
    $evidence += ("RestreamerEnabled=false with the path in the batch set: {0}" -f $offChoice.Mode)
    if ($offChoice.Mode -ne 'direct' -or $offChoice.Url -ne $direct) {
        $ok = $false
        $evidence += 'FAIL: the relay was chosen with the restreamer switched off'
    }

    # Startup budget: the stagger is per consumer, so the last of six starts
    # five gaps in. Seconds, not minutes.
    $defaults = Get-CamConfigDefault
    $spreadMs = 5 * [int]$defaults.StartStaggerMs
    $evidence += ("StartStaggerMs {0} -> the sixth consumer starts {1} ms in; RelayReadyWaitSeconds {2}; RelayStartupWaitSeconds {3}" -f `
        $defaults.StartStaggerMs, $spreadMs, $defaults.RelayReadyWaitSeconds, $defaults.RelayStartupWaitSeconds)
    if ($spreadMs -gt 3000 -or [int]$defaults.RelayReadyWaitSeconds -gt 30) {
        $ok = $false
        $evidence += 'FAIL: the startup budget is no longer measured in seconds'
    }

    Add-CamTestResult -Run $Run -Check 'inputs' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'a batch of consumers decides relay-or-direct from one shared serving set with no per-camera probe, a path missing from it is waited for and bounded, and a lone attempt still probes and still falls back to direct' `
        -Evidence $evidence
}


function Invoke-CamCheckSelfTest {
    param($Run)

    $result = Invoke-CamTestScript -Path (Join-Path -Path $repo -ChildPath 'cam.recorder.ps1') `
        -Arguments @('-SelfTest', '-Offline') -TimeoutSeconds 300
    $summary = @($result.Lines | Where-Object { $_ -match 'passed|failed|FAIL' })
    Add-CamTestResult -Run $Run -Check 'selftest' -Status $(if ($result.ExitCode -eq 0) { 'PASS' } else { 'FAIL' }) `
        -Message ("cam.recorder.ps1 -SelfTest -Offline exited {0}" -f $result.ExitCode) `
        -Evidence @($summary | Select-Object -Last 6)
}


function Invoke-CamCheckLayout {
    <#
        The dashboard geometry, asserted without opening a window.

        The constraint that matters most is the one in the middle: the grid
        shape comes from the CAMERA COUNT ALONE, so reserving the strip must
        not change $Columns or $Rows. If it ever did, six cameras would tile
        3x3 instead of 3x2 and every feed would shrink.
    #>
    param($Run)

    $config = Get-CamConfigDefault
    $evidence = @()
    $ok = $true

    function Assert-Layout {
        param([string]$Label, $Actual, $Expected)
        $bad = @()
        foreach ($key in $Expected.Keys) {
            if ([string]$Actual.$key -ne [string]$Expected[$key]) {
                $bad += ("{0}={1} want {2}" -f $key, $Actual.$key, $Expected[$key])
            }
        }
        $script:CamLayoutLine = ("{0}: cols={1} rows={2} pane={3}x{4} gutter={5} grid={6} strip=({7},{8}) {9}x{10} mode={11}" -f `
            $Label, $Actual.Columns, $Actual.Rows, $Actual.PaneWidth, $Actual.PaneHeight, $Actual.GutterPx, `
            $Actual.GridHeight, $Actual.StripLeft, $Actual.StripTop, $Actual.StripWidth, $Actual.StripHeight, $Actual.Mode)
        if ($bad.Count -gt 0) { $script:CamLayoutLine += ('  MISMATCH: ' + ($bad -join '; ')) }
        return ($bad.Count -eq 0)
    }

    # The Operator's screen, six cameras, the strip derived from the aspect.
    $config.HealthStripHeightPx = -1
    $auto = Get-CamDashboardLayout -Config $config -CameraCount 6 -ScreenWidth 1920 -ScreenHeight 1032
    if (-not (Assert-Layout -Label 'auto, 6 cameras, 1920x1032' -Actual $auto -Expected @{
        Columns = 3; Rows = 2; PaneWidth = 640; PaneHeight = 360; GutterPx = 8
        GridHeight = 728; StripLeft = 0; StripTop = 736; StripWidth = 1920; StripHeight = 296
        StripEnabled = $true; Mode = 'aspect' })) { $ok = $false }
    $evidence += $script:CamLayoutLine

    # Full-bleed: the panes start at the top-left corner and the right-hand
    # column ends exactly on the screen edge, with the gutter only between the
    # two rows.
    $positions = @()
    for ($i = 0; $i -lt 6; $i++) {
        $position = Get-CamPanePosition -Layout $auto -Index $i
        $positions += ("+{0}+{1}" -f $position.X, $position.Y)
    }
    $evidence += ('pane positions: ' + ($positions -join ' '))
    $expectedPositions = '+0+0 +640+0 +1280+0 +0+368 +640+368 +1280+368'
    if (($positions -join ' ') -ne $expectedPositions) {
        $ok = $false
        $evidence += ("FAIL: want {0}" -f $expectedPositions)
    }
    $rightEdge = (Get-CamPanePosition -Layout $auto -Index 2).X + $auto.PaneWidth
    $evidence += ("right-hand column ends at x={0}, screen is 1920 wide; row 1 starts at y={1}, row 0 is {2} tall, so the gutter is {3} px and sits between the rows only" -f `
        $rightEdge, (Get-CamPanePosition -Layout $auto -Index 3).Y, $auto.PaneHeight, `
        ((Get-CamPanePosition -Layout $auto -Index 3).Y - $auto.PaneHeight))
    if ($rightEdge -ne 1920) { $ok = $false; $evidence += 'FAIL: the grid does not reach the right edge of the screen' }

    # The strip switched off restores the original full-height arithmetic.
    $config.HealthStripHeightPx = 0
    $off = Get-CamDashboardLayout -Config $config -CameraCount 6 -ScreenWidth 1920 -ScreenHeight 1032
    if (-not (Assert-Layout -Label 'strip off, 6 cameras' -Actual $off -Expected @{
        Columns = 3; Rows = 2; PaneWidth = 640; PaneHeight = 516; GutterPx = 0
        GridHeight = 1032; StripHeight = 0; StripEnabled = $false; Mode = 'off' })) { $ok = $false }
    $evidence += $script:CamLayoutLine
    $offPositions = @()
    for ($i = 0; $i -lt 6; $i++) {
        $position = Get-CamPanePosition -Layout $off -Index $i
        $offPositions += ("+{0}+{1}" -f $position.X, $position.Y)
    }
    $evidence += ('pane positions with the strip off: ' + ($offPositions -join ' '))
    if (($offPositions -join ' ') -ne '+0+0 +640+0 +1280+0 +0+516 +640+516 +1280+516') {
        $ok = $false
        $evidence += 'FAIL: the original full-height layout was not restored'
    }

    # THE constraint: the strip never reshapes the grid.
    if ($auto.Columns -ne $off.Columns -or $auto.Rows -ne $off.Rows) {
        $ok = $false
        $evidence += 'FAIL: reserving the strip changed the grid shape'
    }
    $evidence += ("grid shape with the strip {0}x{1}, without it {2}x{3} - the camera count alone decides it" -f `
        $auto.Columns, $auto.Rows, $off.Columns, $off.Rows)

    # A fixed strip height shrinks the panes rather than the grid.
    $config.HealthStripHeightPx = 200
    $fixed = Get-CamDashboardLayout -Config $config -CameraCount 6 -ScreenWidth 1920 -ScreenHeight 1032
    if (-not (Assert-Layout -Label 'strip fixed at 200 px' -Actual $fixed -Expected @{
        Columns = 3; Rows = 2; PaneWidth = 640; PaneHeight = 408; StripHeight = 200; Mode = 'fixed' })) { $ok = $false }
    $evidence += $script:CamLayoutLine

    # Nine cameras on the same screen: aspect-exact panes would overflow, so
    # the floor takes over and the panes shrink instead of the strip.
    $config.HealthStripHeightPx = -1
    $nine = Get-CamDashboardLayout -Config $config -CameraCount 9 -ScreenWidth 1920 -ScreenHeight 1032
    if (-not (Assert-Layout -Label 'auto, 9 cameras' -Actual $nine -Expected @{
        Columns = 3; Rows = 3; PaneWidth = 640; PaneHeight = 296; StripHeight = 120 })) { $ok = $false }
    $evidence += $script:CamLayoutLine
    if (-not $nine.Note) { $ok = $false; $evidence += 'FAIL: shrinking the panes was not reported' }
    else { $evidence += ('note: ' + $nine.Note) }

    Add-CamTestResult -Run $Run -Check 'layout' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'the dashboard reserves the strip without ever reshaping the grid, and a strip height of 0 restores the original full-height layout' `
        -Evidence $evidence
}


function Invoke-CamCheckSections {
    <#
        Every strip section can be switched off, including all of them.

        Rendered through the product script with a sandbox config, because
        HealthSections is a config key and a test that called the renderer's
        internals would not prove the config reaches it.
    #>
    param($Run)

    $cameras = @(
        [pscustomobject]@{ Name = 'Front Entrance'; IP = '10.254.254.254'; Quality = 'stream1'; Record = $true }
    )
    $evidence = @()
    $ok = $true

    function Invoke-SectionRender {
        param([string]$Name, $Sections, [hashtable]$Extra = @{})

        $settings = @{ HealthSections = $Sections }
        foreach ($key in $Extra.Keys) { $settings[$key] = $Extra[$key] }
        $sandbox = New-CamTestSandbox -Name ("sections-" + $Name) -Cameras $cameras -Config $settings

        $statusPath = Get-CamStatusFilePath -Config $sandbox.Config
        [void](New-Item -ItemType Directory -Path (Split-Path -Path $statusPath -Parent) -Force)
        $now = [DateTime]::Now
        $document = [pscustomobject]@{
            Schema = 1; State = 'running'; SupervisorPid = $PID
            StartedAt = (Format-CamTimestamp -Value $now.AddHours(-1)); UpdatedAt = (Format-CamTimestamp -Value $now)
            UptimeSeconds = 3600.0; RootPath = $sandbox.Root; Container = 'mkv'; SegmentSeconds = 900
            Disk = [pscustomobject]@{ FreeGB = 301.37; ReserveGB = 20.0; PruneHysteresisGB = 5.0
                ArchiveGB = 88.4; ArchiveBytes = 94908000000; ArchiveFiles = 412
                OldestSegment = (Format-CamTimestamp -Value $now.AddDays(-2.2))
                NewestSegment = (Format-CamTimestamp -Value $now.AddMinutes(-3))
                MeasuredGBPerDay = 99.7; ProjectedDaysRemaining = 2.82; ScannedAt = (Format-CamTimestamp -Value $now) }
            Prune = [pscustomobject]@{ LastRunAt = $null; DeletedTotal = 0; FreedGBTotal = 0; Blocked = $false; LastMessage = '' }
            Restreamer = [pscustomobject]@{ Enabled = $true; RelayBase = 'rtsp://127.0.0.1:8554'; Pid = 9100; LastPid = 9100
                Alive = $true; StartedAt = $null; RestartsTotal = 0; Paths = @('frontentrance'); LastError = ''; LastErrorAt = $null }
            Events = @([pscustomobject]@{ At = (Format-CamTimestamp -Value $now.AddSeconds(-30)); Text = 'start Front Entrance pid 4000 on the relay input' })
            Cameras = @([pscustomobject]@{ Name = 'Front Entrance'; SafeName = 'Front_Entrance'; Record = $true
                Pid = 4000; LastPid = 4000; Alive = $true; CurrentSegment = 'seg.mkv'; CurrentSegmentBytes = 1
                SecondsSinceLastClosedSegment = 42; InputMode = 'relay'; RestartsLastHour = 0; RestartsTotal = 0
                StartedAt = $null; NextStartAt = $null; LastError = ''; LastErrorAt = $null
                ArchiveFiles = 412; ArchiveBytes = 1; ConfigError = '' })
        }
        [System.IO.File]::WriteAllText($statusPath, (ConvertTo-Json -InputObject $document -Depth 6), (New-Object System.Text.UTF8Encoding($false)))

        $render = Invoke-CamTestScript -Path $sandbox.Health -Arguments @('-Once', '-Rows', '15', '-Width', '207')
        $lines = @($render.Output -split "`r?`n")
        if ($lines.Count -gt 0 -and $lines[-1] -eq '') { $lines = $lines[0..($lines.Count - 2)] }
        return [pscustomobject]@{ ExitCode = $render.ExitCode; Lines = $lines; Text = ($lines -join "`n"); Output = $render.Output }
    }

    # Each section alone, so "switched off" is proven section by section
    # rather than inferred from one combined render.
    $markers = @{
        status  = '(?m)^RUNNING pid '
        cameras = '(?m)^CAMERA\s+REC\s+ALIVE'
        events  = '(?m)^EVENTS\s+\d{2}:\d{2}:\d{2} '
        archive = '(?m)^ARCHIVE\s+\d{2}-\d{2} '
    }
    foreach ($only in @('status', 'cameras', 'events', 'archive')) {
        $result = Invoke-SectionRender -Name $only -Sections @($only)
        if ($result.ExitCode -ne 0 -or $result.Lines.Count -ne 15) { $ok = $false }
        $present = @()
        foreach ($name in $markers.Keys) {
            if ($result.Text -match $markers[$name]) { $present += $name }
        }
        # TREND is the archive section's second row and must not appear alone.
        if ($only -ne 'archive' -and $result.Text -match '(?m)^TREND ') { $present += 'archive-trend' }
        # Both sides into variables first: -join binds tighter than -ne, so
        # comparing them inline silently compares the wrong things.
        $presentText = (@($present | Sort-Object) -join ',')
        $wantedText = $only
        if ($presentText -ne $wantedText) {
            $ok = $false
            $evidence += ("FAIL: HealthSections=[{0}] rendered sections [{1}]" -f $only, $presentText)
        }
        $evidence += ("HealthSections=[{0}] -> {1} row(s), sections present: [{2}]" -f $only, $result.Lines.Count, $presentText)
    }

    # Every section off: fifteen rows of nothing.
    $empty = Invoke-SectionRender -Name 'empty' -Sections @()
    $blank = @($empty.Lines | Where-Object { $_.Trim().Length -eq 0 }).Count
    $evidence += ("HealthSections=[] -> exit {0}, {1} row(s), {2} of them blank" -f $empty.ExitCode, $empty.Lines.Count, $blank)
    if ($empty.ExitCode -ne 0 -or $empty.Lines.Count -ne 15 -or $blank -ne 15) {
        $ok = $false
        $evidence += 'FAIL: an empty HealthSections did not render an empty strip'
    }

    # Order is the config's, not the renderer's.
    $reordered = Invoke-SectionRender -Name 'reordered' -Sections @('archive', 'status')
    $firstLine = ''
    if ($reordered.Lines.Count -gt 0) { $firstLine = $reordered.Lines[0] }
    $evidence += ("HealthSections=[archive,status] -> first row: " + $firstLine.TrimEnd())
    if ($firstLine -notmatch '^ARCHIVE ') { $ok = $false; $evidence += 'FAIL: the configured order was not honoured' }

    # A typo is loud, not silently dropped.
    $typo = Invoke-SectionRender -Name 'typo' -Sections @('status', 'camera')
    $evidence += ("HealthSections=[status,camera] -> exit {0}" -f $typo.ExitCode)
    if ($typo.ExitCode -eq 0) { $ok = $false; $evidence += 'FAIL: an unknown section name was accepted' }

    Add-CamTestResult -Run $Run -Check 'sections' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'every strip section can be switched off independently, all of them at once gives an empty strip, and the configured order is honoured' `
        -Evidence $evidence
}


function Invoke-CamCheckAutostart {
    <#
        The logon Scheduled Task, end to end: install, reinstall, remove,
        remove again - plus the launcher stub the task actually points at.

        Skipped rather than run if a task of that name, or a launcher at that
        path, already exists: that would be the Operator's real autostart and
        this check would rewrite and then delete it.
    #>
    param($Run)

    $taskName = 'IPCCTV-Recorder'
    $stubDir = Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IPCCTV'
    $stubPath = Join-Path -Path $stubDir -ChildPath 'start-recorder.ps1'
    $stubLogPath = Join-Path -Path $stubDir -ChildPath 'autostart.log'

    if (-not (Get-Command -Name 'Get-ScheduledTask' -ErrorAction SilentlyContinue)) {
        Add-CamTestResult -Run $Run -Check 'autostart' -Status 'SKIP' `
            -Message 'the ScheduledTasks module is not available on this machine'
        return
    }
    if (@(Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue).Count -gt 0) {
        Add-CamTestResult -Run $Run -Check 'autostart' -Status 'SKIP' `
            -Message ("a task named {0} is already registered, which would be the Operator's own autostart" -f $taskName)
        return
    }
    if (Test-Path -LiteralPath $stubPath) {
        Add-CamTestResult -Run $Run -Check 'autostart' -Status 'SKIP' `
            -Message ("a launcher already exists at {0}, which would be the Operator's own autostart" -f $stubPath)
        return
    }

    $cameras = @([pscustomobject]@{ Name = 'Front Entrance'; IP = '10.254.254.254'; Quality = 'stream1'; Record = $false })
    $sandbox = New-CamTestSandbox -Name 'autostart' -Cameras $cameras
    $evidence = @()
    $ok = $true

    try {
        $before = @(Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue).Count
        $evidence += ("before install: {0} task(s) named {1}" -f $before, $taskName)

        # A no-op removal first: it must not be an error to remove nothing.
        $noop = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-RemoveAutostart')
        $evidence += ("-RemoveAutostart with nothing installed: exit {0}" -f $noop.ExitCode)
        $evidence += @($noop.Output -split "`r?`n" | Where-Object { $_.Trim() } | ForEach-Object { '  ' + $_.TrimEnd() })
        if ($noop.ExitCode -ne 0) { $ok = $false; $evidence += 'FAIL: removing nothing was not a clean no-op' }

        $install = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-InstallAutostart')
        $evidence += ("-InstallAutostart: exit {0}" -f $install.ExitCode)
        $evidence += @($install.Output -split "`r?`n" | Where-Object { $_.Trim() } | ForEach-Object { '  ' + $_.TrimEnd() })
        if ($install.ExitCode -ne 0) { $ok = $false }

        $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        if (-not $task) {
            $ok = $false
            $evidence += 'FAIL: no task was registered'
        }
        else {
            $xml = Export-ScheduledTask -TaskName $taskName
            $account = [Security.Principal.WindowsIdentity]::GetCurrent().Name
            $evidence += ("registered: state {0}, {1} trigger(s)" -f $task.State, @($task.Triggers).Count)
            foreach ($needle in @('<LogonTrigger>', ('<UserId>' + $account + '</UserId>'), '<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>',
                    '<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>',
                    '<RestartOnFailure>', '<Count>3</Count>', '<Interval>PT1M</Interval>',
                    'start-recorder.ps1')) {
                $found = ($xml -like ('*' + $needle + '*'))
                $evidence += ("  task XML contains {0}: {1}" -f $needle, $found)
                if (-not $found) { $ok = $false }
            }
            # It must run unelevated: HighestAvailable would mean a UAC prompt
            # at every logon, which is not an autostart.
            if ($xml -like '*<RunLevel>HighestAvailable</RunLevel>*') {
                $ok = $false
                $evidence += 'FAIL: the task asks for elevation'
            }
            # The task must NOT point at the recorder directly any more: a
            # \\wsl.localhost path is not resolvable at logon, which is the
            # whole reason the launcher exists.
            if ($xml -like '*cam.recorder.ps1*') {
                $ok = $false
                $evidence += 'FAIL: the task points straight at cam.recorder.ps1 instead of the launcher'
            }
        }

        # The launcher itself: present, forwarding to this sandbox's recorder,
        # and a forwarder rather than a copy of it.
        if (-not (Test-Path -LiteralPath $stubPath)) {
            $ok = $false
            $evidence += ("FAIL: no launcher was written at {0}" -f $stubPath)
        }
        else {
            $stub = Get-Content -LiteralPath $stubPath -Raw
            $stubBytes = (Get-Item -LiteralPath $stubPath).Length
            $evidence += ("launcher written: {0} ({1} bytes)" -f $stubPath, $stubBytes)
            foreach ($needle in @($sandbox.Recorder, '-Run', 'Test-StubTarget', 'Start-Sleep')) {
                $found = ($stub -like ('*' + $needle + '*'))
                $evidence += ("  launcher contains {0}: {1}" -f $needle, $found)
                if (-not $found) { $ok = $false }
            }
            if ($stubBytes -gt 8KB) {
                $ok = $false
                $evidence += 'FAIL: the launcher is big enough to be a copy of the recorder rather than a forwarder'
            }

            # The retry path, exercised rather than read: pointed at a path that
            # does not exist, with the wait collapsed, it must give up non-zero
            # after polling instead of hanging or sailing on.
            $retryStub = Join-Path -Path $sandbox.Dir -ChildPath 'retry-stub.ps1'
            $missing = Join-Path -Path $sandbox.Dir -ChildPath 'not-there\cam.recorder.ps1'
            $retryLog = Join-Path -Path $sandbox.Dir -ChildPath 'retry.log'
            # The copy logs into the sandbox, not into the Operator's own
            # launcher log: a test must not write to the file he reads when
            # autostart failed for real.
            $retryBody = $stub.
                Replace(("'" + $sandbox.Recorder + "'"), ("'" + $missing + "'")).
                Replace(("'" + $stubLogPath + "'"), ("'" + $retryLog + "'")).
                Replace('$TimeoutSeconds = 300', '$TimeoutSeconds = 6').
                Replace('$PollSeconds    = 5', '$PollSeconds    = 2')
            if ($retryBody -like ('*' + $stubLogPath + '*')) {
                $ok = $false
                $evidence += 'FAIL: the retry copy still points at the real launcher log'
            }
            Set-Content -LiteralPath $retryStub -Value $retryBody -Encoding UTF8
            $retry = Invoke-CamTestScript -Path $retryStub -Arguments @()
            $evidence += ("launcher against a missing target: exit {0}" -f $retry.ExitCode)
            $evidence += @($retry.Output -split "`r?`n" | Where-Object { $_.Trim() } | ForEach-Object { '  ' + $_.TrimEnd() })
            if ($retry.ExitCode -ne 2) {
                $ok = $false
                $evidence += 'FAIL: the launcher did not give up with exit 2 after its bounded wait'
            }
            if ($retry.Output -notlike '*did not appear within 6s*') {
                $ok = $false
                $evidence += 'FAIL: the launcher did not report the bounded wait it gave up on'
            }
        }

        $again = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-InstallAutostart')
        $count = @(Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue).Count
        $evidence += ("-InstallAutostart a second time: exit {0}, {1} task(s) registered" -f $again.ExitCode, $count)
        if ($again.ExitCode -ne 0 -or $count -ne 1) { $ok = $false; $evidence += 'FAIL: reinstalling was not idempotent' }

        $remove = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-RemoveAutostart')
        $left = @(Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue).Count
        $stubLeft = Test-Path -LiteralPath $stubPath
        $evidence += ("-RemoveAutostart: exit {0}, {1} task(s) left, launcher still there: {2}" -f $remove.ExitCode, $left, $stubLeft)
        if ($remove.ExitCode -ne 0 -or $left -ne 0) { $ok = $false; $evidence += 'FAIL: the task was not removed' }
        if ($stubLeft) { $ok = $false; $evidence += 'FAIL: the launcher was not removed' }

        $twice = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-RemoveAutostart')
        $evidence += ("-RemoveAutostart again: exit {0}" -f $twice.ExitCode)
        if ($twice.ExitCode -ne 0) { $ok = $false; $evidence += 'FAIL: removing twice was not a clean no-op' }
    }
    finally {
        # Never leave a task behind, whatever failed above.
        if (@(Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue).Count -gt 0) {
            Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
            $evidence += 'cleanup: unregistered a task the check left behind'
        }
        if (Test-Path -LiteralPath $stubPath) {
            Remove-Item -LiteralPath $stubPath -Force -ErrorAction SilentlyContinue
            $evidence += 'cleanup: deleted a launcher the check left behind'
        }
    }

    Add-CamTestResult -Run $Run -Check 'autostart' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'the logon task installs unelevated with restart-on-failure, points at a launcher that waits for the recorder path and gives up non-zero, is idempotent on reinstall, and removing it takes the launcher with it' `
        -Evidence $evidence
}


function Invoke-CamCheckStrip {
    <#
        Renders cam.health.ps1 against a status.json fixture, because the three
        cases that matter cannot be produced on demand from a live supervisor:
        a healthy one, a stale one, and no file at all.

        SupervisorPid is this process, so the fixture passes the same two-signal
        liveness test (fresh heartbeat AND a process that exists) that the real
        renderer applies - nothing is stubbed out.
    #>
    param($Run)

    $cameras = @(
        [pscustomobject]@{ Name = 'Front Entrance'; IP = '10.254.254.254'; Quality = 'stream1'; Record = $true }
    )
    $sandbox = New-CamTestSandbox -Name 'strip' -Cameras $cameras
    $statusPath = Get-CamStatusFilePath -Config $sandbox.Config
    [void](New-Item -ItemType Directory -Path (Split-Path -Path $statusPath -Parent) -Force)

    $names = @('Front Entrance', 'Rear Balcony', 'Field Front', 'Car Garage', 'Workshop Entrance', 'Workshop')
    $rows = @()
    for ($i = 0; $i -lt $names.Count; $i++) {
        $rows += [pscustomobject]@{
            Name                          = $names[$i]
            SafeName                      = ($names[$i] -replace ' ', '_')
            Record                        = ($i -lt 4)
            Pid                           = $(if ($i -lt 3) { 4000 + $i } else { $null })
            LastPid                       = 4000 + $i
            Alive                         = ($i -lt 3)
            CurrentSegment                = 'seg.mkv'
            CurrentSegmentBytes           = 12345678
            SecondsSinceLastClosedSegment = $(if ($i -lt 3) { 42 + $i } else { $null })
            InputMode                     = @('relay', 'relay', 'direct', '', '', '')[$i]
            RestartsLastHour              = @(0, 0, 2, 0, 0, 0)[$i]
            RestartsTotal                 = @(0, 0, 7, 0, 0, 0)[$i]
            LastError                      = $(if ($i -eq 3) { 'ffmpeg exited with code 1 after 4s: rtsp://***:***@10.254.254.254/stream1 Operation not permitted' } else { '' })
            LastErrorAt                   = $null
            ConfigError                   = ''
        }
    }

    function New-Fixture {
        param([string]$State, [DateTime]$UpdatedAt, [int]$SupervisorPid, [bool]$RelayAlive)
        return [pscustomobject]@{
            Schema         = 1
            State          = $State
            SupervisorPid  = $SupervisorPid
            StartedAt      = (Format-CamTimestamp -Value $UpdatedAt.AddHours(-2))
            UpdatedAt      = (Format-CamTimestamp -Value $UpdatedAt)
            UptimeSeconds  = 7245.0
            RootPath       = $sandbox.Root
            Container      = 'mkv'
            SegmentSeconds = 900
            Disk           = [pscustomobject]@{
                FreeGB = 301.37; ReserveGB = 20.0; PruneHysteresisGB = 5.0
                ArchiveGB = 88.4; ArchiveBytes = 94908000000; ArchiveFiles = 412
                OldestSegment = (Format-CamTimestamp -Value $UpdatedAt.AddDays(-2.2))
                NewestSegment = (Format-CamTimestamp -Value $UpdatedAt.AddMinutes(-3))
                MeasuredGBPerDay = 99.7; ProjectedDaysRemaining = 2.82; ScannedAt = (Format-CamTimestamp -Value $UpdatedAt)
            }
            Prune          = [pscustomobject]@{ LastRunAt = (Format-CamTimestamp -Value $UpdatedAt); DeletedTotal = 128; FreedGBTotal = 234.117; Blocked = $false; LastMessage = 'deleted 1 file, freed 1.83 GB' }
            Restreamer     = [pscustomobject]@{
                Enabled = $true; RelayBase = 'rtsp://127.0.0.1:8554'
                Pid = $(if ($RelayAlive) { 9100 } else { $null }); LastPid = 9100; Alive = $RelayAlive
                StartedAt = $null; RestartsTotal = 1; Paths = @('frontentrance', 'rearbalcony', 'fieldfront', 'cargarage', 'workshopentrance', 'workshop')
                LastError = ''; LastErrorAt = $null
            }
            Events         = @(
                [pscustomobject]@{ At = (Format-CamTimestamp -Value $UpdatedAt.AddSeconds(-12)); Text = 'prune: deleted workshop_2026-10-09_1400+0300.mkv, freed 1.83 GB' }
                [pscustomobject]@{ At = (Format-CamTimestamp -Value $UpdatedAt.AddSeconds(-95)); Text = 'watchdog Car Garage: no segment growth for 2250s, killing pid 4003' }
                [pscustomobject]@{ At = (Format-CamTimestamp -Value $UpdatedAt.AddSeconds(-220)); Text = 'Car Garage: ffmpeg exit 1 after 63s; restart #7 in 3s' }
            )
            Cameras        = $rows
        }
    }

    function Set-Fixture {
        param($Document)
        [System.IO.File]::WriteAllText($statusPath, (ConvertTo-Json -InputObject $Document -Depth 6), (New-Object System.Text.UTF8Encoding($false)))
    }

    $evidence = @()
    $ok = $true

    # --- healthy, at the size the dashboard's own strip window measures:
    #     1913x296 px of Windows Terminal in focus mode is 208x15 cells, and
    #     the renderer leaves one column spare so a full-width line cannot wrap.
    Set-Fixture -Document (New-Fixture -State 'running' -UpdatedAt ([DateTime]::Now) -SupervisorPid $PID -RelayAlive $true)
    $render = Invoke-CamTestScript -Path $sandbox.Health -Arguments @('-Once', '-Rows', '15', '-Width', '207')
    $lines = @($render.Output -split "`r?`n")
    if ($lines.Count -gt 0 -and $lines[-1] -eq '') { $lines = $lines[0..($lines.Count - 2)] }
    $widest = 0
    foreach ($line in $lines) { if ($line.Length -gt $widest) { $widest = $line.Length } }

    if ($render.ExitCode -ne 0) { $ok = $false }
    if ($lines.Count -ne 15) { $ok = $false }
    if ($widest -gt 207) { $ok = $false }
    if ($lines[0] -notmatch '^RUNNING pid') { $ok = $false }
    # The state line has to still END with its last segment, which is what
    # proves the whole line fits the width instead of being cut short.
    if ($lines[0].TrimEnd() -notmatch '(\dd left|days left -)$') { $ok = $false }
    $joined = $lines -join "`n"
    # Both input modes have to reach the strip: the whole point of the MODE
    # column is telling a camera on the relay from one that fell back.
    if ($joined -notmatch 'relay UP') { $ok = $false }
    if ($joined -notmatch '(?m)^\S.*\s+relay\s') { $ok = $false }
    if ($joined -notmatch '(?m)^\S.*\s+direct\s') { $ok = $false }
    # The two sections C2b added have to actually be on the strip, with their
    # figures, not just their labels.
    if ($joined -notmatch '(?m)^EVENTS\s+\d{2}:\d{2}:\d{2} ') { $ok = $false }
    if ($joined -notmatch '(?m)^ARCHIVE\s+\d{2}-\d{2} \d{2}:\d{2} -> \d{2}-\d{2} \d{2}:\d{2} \(2\.2d\)\s+88\.4 GB in 412 file') { $ok = $false }
    if ($joined -notmatch '(?m)^TREND\s+99\.7 GB/day\s+2\.8d left\s+pruned 128 file') { $ok = $false }
    # An event older than the newest must come second: newest first is what
    # makes a three-line window worth reading.
    if ($joined -notmatch '(?ms)^EVENTS\s+\d{2}:\d{2}:\d{2} prune:.*\n\s+\d{2}:\d{2}:\d{2} watchdog ') { $ok = $false }
    $evidence += ("healthy: exit {0}, {1} row(s), widest {2} col(s)" -f $render.ExitCode, $lines.Count, $widest)
    $evidence += @($lines | ForEach-Object { '  ' + $_.TrimEnd() })

    # --- stale: a clean shutdown is recorded, so no supervisor is behind it
    Set-Fixture -Document (New-Fixture -State 'stopped' -UpdatedAt ([DateTime]::Now.AddMinutes(-9)) -SupervisorPid $PID -RelayAlive $false)
    $render = Invoke-CamTestScript -Path $sandbox.Health -Arguments @('-Once', '-Rows', '15', '-Width', '207')
    $staleLines = @($render.Output -split "`r?`n")
    if ($staleLines.Count -gt 0 -and $staleLines[-1] -eq '') { $staleLines = $staleLines[0..($staleLines.Count - 2)] }
    if ($staleLines.Count -ne 15) { $ok = $false }
    if ($staleLines[0] -notmatch '^STALE - NO SUPERVISOR') { $ok = $false }
    if ($staleLines[0].TrimEnd() -notmatch 'last known free [\d.]+ GB \(reserve [\d.]+\)$') { $ok = $false }
    $evidence += ("stale: exit {0}, {1} row(s)" -f $render.ExitCode, $staleLines.Count)
    $evidence += @($staleLines | ForEach-Object { '  ' + $_.TrimEnd() })

    # --- no status file at all
    Remove-Item -LiteralPath $statusPath -Force
    $render = Invoke-CamTestScript -Path $sandbox.Health -Arguments @('-Once', '-Rows', '15', '-Width', '207')
    $missingLines = @($render.Output -split "`r?`n" | Where-Object { $_.Trim().Length -gt 0 })
    if ($missingLines.Count -eq 0 -or $missingLines[0] -notmatch '^NO STATUS FILE') { $ok = $false }
    $evidence += ("no status file: {0}" -f $missingLines[0].TrimEnd())

    Add-CamTestResult -Run $Run -Check 'strip' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'cam.health.ps1 renders 15 rows within 207 columns with every section, and says so when the snapshot is stale or absent' `
        -Evidence $evidence
}


function Invoke-CamCheckLeak {
    param($Run)

    if (-not (Test-Path -LiteralPath (Join-Path -Path $repo -ChildPath 'camera_creds.xml'))) {
        Add-CamTestResult -Run $Run -Check 'leak' -Status 'SKIP' `
            -Message 'no camera_creds.xml, so there is no needle to scan for'
        return
    }

    $result = Invoke-CamCredentialLeakScan
    $evidence = @(("roots: {0}" -f ($result.Roots -join ', ')), $result.Text, ("control file removed: {0}" -f $result.ControlRemoved))
    $evidence += @($result.PasswordHits | ForEach-Object { 'PASSWORD HIT ' + $_ })
    $evidence += @($result.UserinfoHits | ForEach-Object { 'USERINFO HIT ' + $_ })
    Add-CamTestResult -Run $Run -Check 'leak' -Status $(if ($result.Ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'no camera password and no credential in any URL anywhere under the scanned roots' -Evidence $evidence
}


function Invoke-CamCheckProcs {
    <#
        Asserts that no ffmpeg or MediaMTX is left over from a test run.

        It has to tell a leftover from the Operator's own recorder, which runs
        24/7 and legitimately owns five ffmpeg and a MediaMTX. Without that
        distinction the check fails on any machine where recording is up - so
        the one state the harness is REQUIRED to leave behind was also the one
        state it reported as a failure, which is noise that teaches everyone
        to ignore a red check.

        A live supervisor is identified by the work it owns, not by name:
        Get-CamProcessInventory finds it as the parent of every ffmpeg, and
        status.json has to agree that it is running. If both say yes, these
        processes have an owner and this check has nothing to say about them.
    #>
    param($Run)

    $counts = Get-CamTestProcessCount
    $evidence = @(("ffmpeg ids: {0}" -f (@($counts.FfmpegIds) -join ' ')),
        ("mediamtx ids: {0}" -f (@($counts.MediaMtxIds) -join ' ')))

    $config = Get-CamConfig
    $supervisor = Get-CamSupervisorState -Config $config -Snapshot (Get-CamStatusSnapshot -Config $config)
    $owned = @(Get-CamProcessInventory -Config $config | Where-Object { $_.Role -eq 'Supervisor' })
    if ($supervisor.Running -and $owned.Count -gt 0) {
        Add-CamTestResult -Run $Run -Check 'procs' -Status 'SKIP' `
            -Message ("the Operator's recorder is running, so these processes have an owner: {0}" -f $counts.Text) `
            -Evidence ($evidence + @(("live supervisor: {0}" -f $supervisor.Reason)))
        return
    }

    $ok = ($counts.Ffmpeg -eq 0 -and $counts.MediaMtx -eq 0)
    Add-CamTestResult -Run $Run -Check 'procs' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message ("no recorder process left running: {0}" -f $counts.Text) `
        -Evidence $evidence
}


function Invoke-CamCheckRelay {
    <#
        The relay lifecycle, end to end, against the real cameras in a sandbox:
        MediaMTX started as a supervisor child, killed, restarted on a logged
        backoff, then -Stop leaving no orphan.

        The sandbox keeps its own generated MediaMTX config, so nothing outside
        it is written. The camera credential is the real one because MediaMTX
        has to attempt a real pull; a failed authentication is a valid input
        here - starting, failing to pull, being monitored and being torn down is
        the behaviour under test.
    #>
    param($Run)

    $baseline = Get-CamTestProcessCount
    if ($baseline.MediaMtx -gt 0) {
        Add-CamTestResult -Run $Run -Check 'relay' -Status 'FAIL' `
            -Message ("a mediamtx is already running, so this check cannot tell its own child apart from it: {0}" -f $baseline.Text)
        return
    }

    $realConfig = Get-CamConfig
    $cameras = @()
    foreach ($camera in (Get-CamList)) {
        $cameras += [pscustomobject]@{
            Name    = [string]$camera.Name
            IP      = [string]$camera.IP
            Quality = [string]$camera.Quality
            # Two recorders, not six: enough to prove the input choice and the
            # orphan check without taking a slot on every camera at once.
            Record  = (@($cameras).Count -lt 2)
        }
    }

    $sandbox = New-CamTestSandbox -Name 'relay' -Cameras $cameras -UseRealCredential -Config @{
        RestreamerEnabled       = $true
        RestreamerExePath       = [string]$realConfig.RestreamerExePath
        RestreamerConfigPath    = (Join-Path -Path $env:TEMP -ChildPath 'cam-tests\relay\mediamtx.yml')
        RelayStartupWaitSeconds = 15
        SegmentSeconds          = 30
        StatusIntervalSeconds   = 5
    }

    [void](New-Item -ItemType Directory -Path $EvidenceDir -Force)
    $evidence = @()
    $ok = $true

    $install = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-InstallRestreamer')
    $evidence += ("-InstallRestreamer exited {0}" -f $install.ExitCode)
    if ($install.ExitCode -ne 0) {
        Add-CamTestResult -Run $Run -Check 'relay' -Status 'FAIL' -Message 'the sandbox could not generate a MediaMTX config' -Evidence $install.Lines
        return
    }

    $supervisor = $null
    try {
        $supervisor = Start-CamTestSupervisor -Sandbox $sandbox -TimeoutSeconds 120
        $evidence += ("supervisor powershell pid {0}" -f $supervisor.Id)

        Start-Sleep -Seconds $RelaySeconds

        $status = Get-CamTestStatusJson -Sandbox $sandbox
        if (-not $status) { throw 'the supervisor wrote no status file' }
        $relayPid = $status.Snapshot.Restreamer.Pid
        $evidence += ("status.json Restreamer: Enabled={0} Alive={1} Pid={2} Paths={3} Restarts={4}" -f `
            $status.Snapshot.Restreamer.Enabled, $status.Snapshot.Restreamer.Alive, $relayPid, `
            @($status.Snapshot.Restreamer.Paths).Count, $status.Snapshot.Restreamer.RestartsTotal)
        foreach ($camera in @($status.Snapshot.Cameras)) {
            $evidence += ("  camera {0}: Record={1} Alive={2} InputMode='{3}' RestartsLastHour={4}" -f `
                $camera.Name, $camera.Record, $camera.Alive, $camera.InputMode, $camera.RestartsLastHour)
        }
        [System.IO.File]::Copy($status.Path, (Join-Path -Path $EvidenceDir -ChildPath 'status-before-kill.json'), $true)

        if (-not $relayPid) {
            $ok = $false
            $evidence += 'FAIL: status.json reports no live restreamer pid, so there was nothing to kill'
        }
        else {
            $process = Get-Process -Id ([int]$relayPid) -ErrorAction SilentlyContinue
            if (-not $process -or $process.Name -ne 'mediamtx') {
                $ok = $false
                $evidence += ("FAIL: pid {0} is not a running mediamtx" -f $relayPid)
            }
            else {
                $evidence += ("killing mediamtx pid {0} with Stop-Process -Force" -f $relayPid)
                Stop-Process -Id ([int]$relayPid) -Force
                # The backoff is 3s and the supervisor ticks once a second, so a
                # restart plus its startup wait fits comfortably in this window.
                Start-Sleep -Seconds 45

                $after = Get-CamTestStatusJson -Sandbox $sandbox
                $newPid = $after.Snapshot.Restreamer.Pid
                $evidence += ("after the kill: Alive={0} Pid={1} Restarts={2}" -f `
                    $after.Snapshot.Restreamer.Alive, $newPid, $after.Snapshot.Restreamer.RestartsTotal)
                if (-not $after.Snapshot.Restreamer.Alive -or $newPid -eq $relayPid -or `
                    [int]$after.Snapshot.Restreamer.RestartsTotal -lt 1) {
                    $ok = $false
                    $evidence += 'FAIL: the restreamer was not restarted under a new pid'
                }
                [System.IO.File]::Copy($after.Path, (Join-Path -Path $EvidenceDir -ChildPath 'status-after-restart.json'), $true)
            }
        }
    }
    finally {
        $stop = $null
        if ($supervisor) { $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $supervisor }
        if ($stop) {
            $evidence += ("-Stop exited {0}" -f $stop.ExitCode)
            $evidence += @($stop.Lines | ForEach-Object { '  ' + $_ })
            if (-not $stop.Ok) { $ok = $false }
            foreach ($item in @($stop.Killed)) { $evidence += ("FAIL: had to hard-kill " + $item) }
        }

        # Give Windows a moment to reap the children before counting.
        Start-Sleep -Seconds 3
        $final = Get-CamTestProcessCount
        $evidence += ("process count after -Stop: {0} (baseline was {1})" -f $final.Text, $baseline.Text)
        if ($final.MediaMtx -ne 0 -or $final.Ffmpeg -gt $baseline.Ffmpeg) {
            $ok = $false
            $evidence += 'FAIL: a mediamtx or ffmpeg survived the shutdown'
        }

        foreach ($name in @('recorder.log', 'restreamer.log')) {
            $path = Join-Path -Path (Join-Path -Path $sandbox.Root -ChildPath '_logs') -ChildPath $name
            if (Test-Path -LiteralPath $path) {
                [System.IO.File]::Copy($path, (Join-Path -Path $EvidenceDir -ChildPath $name), $true)
            }
        }
        if (Test-Path -LiteralPath $sandbox.ConsoleLog) {
            [System.IO.File]::Copy($sandbox.ConsoleLog, (Join-Path -Path $EvidenceDir -ChildPath 'supervisor.console.log'), $true)
        }
        $evidence += ("evidence copied to {0}" -f $EvidenceDir)

        # The generated MediaMTX config and every log this run produced, scanned
        # for the credential: the relay's whole security claim is that the
        # camera URL stays in the child's environment block and never lands on
        # disk, and this run is the only time that claim is actually exercised.
        $scan = Invoke-CamCredentialLeakScan -Roots @($sandbox.Dir, $EvidenceDir)
        $evidence += ("leak scan over the sandbox: " + $scan.Text)
        $evidence += @($scan.PasswordHits | ForEach-Object { 'PASSWORD HIT ' + $_ })
        $evidence += @($scan.UserinfoHits | ForEach-Object { 'USERINFO HIT ' + $_ })
        if (-not $scan.Ok) { $ok = $false }

        $evidence += ("sandbox credential copy removed: {0}" -f (Remove-CamTestCredential -Sandbox $sandbox))
    }

    Add-CamTestResult -Run $Run -Check 'relay' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'MediaMTX runs as a supervisor child, is restarted on a logged backoff after a kill, and leaves no orphan on -Stop' `
        -Evidence $evidence
}


function Invoke-CamCheckViewer {
    <#
        The pane's return to the relay, and the two things about it that can
        only fail silently.

        The pane script is not a file: it travels to Windows Terminal as one
        base64 blob inside a here-string, so the repository-wide parse check
        cannot see it and a syntax error in it would surface as six panes that
        open and immediately die. It is substituted and parsed here.

        The mpv arguments now reach mpv through a command-line string rather
        than the call operator, so the quoting is asserted against the exact
        line the call operator produced - one mpv flag carries a space.

        The watch itself is asserted where no camera is involved: a relay pane
        watches nothing, a zero interval is an off switch, and the promotion
        is only exercised when the local relay is actually serving a path to
        answer about.
    #>
    param($Run)

    $evidence = @()
    $ok = $true

    # 1. the pane template, substituted exactly as the launcher substitutes it
    $launcher = Join-Path -Path $repo -ChildPath 'mpv.cam.autolayout.autorestart.ps1'
    $text = [System.IO.File]::ReadAllText($launcher)
    # Escaped rather than written inline: a $ in a double-quoted pattern is
    # interpolated by PowerShell before the regex ever sees it, which silently
    # matched a different here-string and handed this check 56 characters.
    $marker = [regex]::Escape("`$PaneTemplate = @'")
    $match = [regex]::Match($text, "(?s)" + $marker + "\r?\n(.*?)\r?\n'@")
    if (-not $match.Success -or $match.Groups[1].Value.Length -lt 800) {
        $ok = $false
        $evidence += ("FAIL: the pane template did not extract - matched {0} character(s), expected the whole here-string called " -f $match.Groups[1].Value.Length) + '$PaneTemplate'
    }
    else {
        $flags = "--no-border --msg-level=all=error --osd-level=1 --osd-msg1='Traffic: `${video-bitrate}' --geometry=640x360+0+0"
        $pane = $match.Groups[1].Value
        $pane = $pane.Replace('__LIB__', (Join-Path -Path $repo -ChildPath 'cam.common.ps1'))
        $pane = $pane.Replace('__NAME__', 'Workshop').Replace('__MPV__', 'C:\mpv\mpv.com').Replace('__FLAGS__', $flags)
        $pane = $pane.Replace('__READYPATHS__', "@('workshop')").Replace('__WAITSECONDS__', '15').Replace('__STAGGERMS__', '1500')
        $errors = $null
        [void][System.Management.Automation.Language.Parser]::ParseInput($pane, [ref]$null, [ref]$errors)
        $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($pane)).Length
        $evidence += ("pane template substituted: {0} chars, {1} parse error(s), {2} chars of command line for six panes" -f `
            $pane.Length, @($errors).Count, (6 * $encoded))
        foreach ($item in @($errors)) { $evidence += ("  line {0}: {1}" -f $item.Extent.StartLineNumber, $item.Message) }
        if (@($errors).Count -gt 0) { $ok = $false; $evidence += 'FAIL: the encoded pane script does not parse' }
        # Windows refuses a command line over 32767 and wt.exe then opens
        # nothing and says nothing. The panes are nearly all of it.
        if ((6 * $encoded) -gt 30000) {
            $ok = $false
            $evidence += 'FAIL: six panes no longer leave room under the 32767 character command line'
        }
        if ($pane -notmatch 'Invoke-CamViewer') {
            $ok = $false
            $evidence += 'FAIL: the pane no longer runs mpv through Invoke-CamViewer, so it cannot return to the relay'
        }
    }

    # 2. the quoting, against the line the call operator used to produce
    $tokens = @(& { $args } --no-border --msg-level=all=error --osd-level=1 --osd-msg1='Traffic: ${video-bitrate}' --geometry=640x360+0+0)
    $line = Get-CamArgumentLine -Tokens (@('rtsp://127.0.0.1:8554/frontentrance') + $tokens)
    $want = 'rtsp://127.0.0.1:8554/frontentrance --no-border --msg-level=all=error --osd-level=1 "--osd-msg1=Traffic: ${video-bitrate}" --geometry=640x360+0+0'
    $evidence += ('argument line: ' + $line)
    if ($line -ne $want) { $ok = $false; $evidence += 'FAIL: mpv would not receive the arguments it used to' }

    $default = [int](Get-CamConfigDefault).ViewerPromoteCheckSeconds
    $evidence += ("ViewerPromoteCheckSeconds default {0}s" -f $default)
    if ($default -le 0 -or $default -gt 60) { $ok = $false; $evidence += 'FAIL: the promotion check interval is not a sane default' }

    # 3. the watch, with a stand-in for mpv so no camera is touched. ping is a
    #    process that exits on its own, which is the other way the watch ends.
    $config = Get-CamConfigDefault
    $config.RelayApiBase = 'http://10.254.254.254:9997'
    $config.RelayProbeTimeoutMs = 400
    $stand = 'C:\Windows\System32\cmd.exe'
    $short = @('ping -n 4 127.0.0.1')

    function Measure-CamWatch {
        param($Config, [string]$Mode, [string]$Path, [string[]]$Arguments)
        $choice = [pscustomobject]@{ Mode = $Mode; Url = '/c'; WaitedSeconds = 0 }
        $watch = [System.Diagnostics.Stopwatch]::StartNew()
        $promoted = Invoke-CamViewer -Config $Config -Choice $choice -RelayPathName $Path -MpvPath $stand -MpvArgs $Arguments
        $watch.Stop()
        return [pscustomobject]@{ Promoted = $promoted; Seconds = $watch.Elapsed.TotalSeconds }
    }

    $config.ViewerPromoteCheckSeconds = 10
    $onRelay = Measure-CamWatch -Config $config -Mode 'relay' -Path 'frontentrance' -Arguments $short
    $evidence += ("relay pane, 3s process, 10s interval: promoted {0} after {1:N1}s - a relay pane watches nothing" -f $onRelay.Promoted, $onRelay.Seconds)
    if ($onRelay.Promoted) { $ok = $false; $evidence += 'FAIL: a pane already on the relay tried to promote itself' }

    $config.ViewerPromoteCheckSeconds = 0
    $off = Measure-CamWatch -Config $config -Mode 'direct' -Path 'frontentrance' -Arguments $short
    $evidence += ("direct pane, interval 0: promoted {0} after {1:N1}s - the off switch" -f $off.Promoted, $off.Seconds)
    if ($off.Promoted) { $ok = $false; $evidence += 'FAIL: a zero interval did not switch the watch off' }

    $config.ViewerPromoteCheckSeconds = 1
    $unreachable = Measure-CamWatch -Config $config -Mode 'direct' -Path 'frontentrance' -Arguments $short
    $evidence += ("direct pane, 1s interval, unreachable API: promoted {0} after {1:N1}s - it ends on mpv's own exit too" -f `
        $unreachable.Promoted, $unreachable.Seconds)
    if ($unreachable.Promoted) { $ok = $false; $evidence += 'FAIL: an unreachable control API was read as a serving path' }
    if ($unreachable.Seconds -gt 20) { $ok = $false; $evidence += 'FAIL: the watch did not notice the process exiting on its own' }

    # 4. the promotion itself, only when the local relay has a path to answer
    #    about. It is the one assertion that needs something outside the repo.
    $live = Get-CamConfigDefault
    $serving = @(Get-CamRelayReadyPaths -Config $live)
    if ($serving.Count -eq 0) {
        $evidence += 'the local relay is not serving anything, so the promotion itself was not exercised'
    }
    else {
        $live.ViewerPromoteCheckSeconds = 3
        $long = @('ping -n 60 127.0.0.1')
        $promotion = Measure-CamWatch -Config $live -Mode 'direct' -Path $serving[0] -Arguments $long
        $evidence += ("direct pane, 3s interval, {0} serving: promoted {1} after {2:N1}s, mpv stopped by the watch" -f `
            $serving[0], $promotion.Promoted, $promotion.Seconds)
        if (-not $promotion.Promoted) { $ok = $false; $evidence += 'FAIL: a serving path did not take the pane back' }
        # One interval plus the stop. Minutes would mean it waited for the
        # process instead of the path.
        if ($promotion.Seconds -gt 10) { $ok = $false; $evidence += 'FAIL: the promotion did not land on the first poll' }
    }

    Add-CamTestResult -Run $Run -Check 'viewer' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'the encoded pane script parses and still fits the command line, mpv receives the arguments it always did, and a pane on direct returns to the relay on the first poll while a pane on the relay watches nothing' `
        -Evidence $evidence
}


foreach ($check in $Checks) {
    if ($check -eq 'relay' -and -not $Live) {
        Add-CamTestResult -Run $run -Check 'relay' -Status 'SKIP' `
            -Message 'needs -Live: it opens RTSP sessions against the real cameras'
        continue
    }
    switch ($check) {
        'parse'      { Invoke-CamCheckParse      -Run $run }
        'cameralist' { Invoke-CamCheckCameraList -Run $run }
        'inputs'     { Invoke-CamCheckInputs     -Run $run }
        'viewer'     { Invoke-CamCheckViewer     -Run $run }
        'selftest'   { Invoke-CamCheckSelfTest   -Run $run }
        'layout'     { Invoke-CamCheckLayout     -Run $run }
        'strip'      { Invoke-CamCheckStrip      -Run $run }
        'sections'   { Invoke-CamCheckSections   -Run $run }
        'autostart'  { Invoke-CamCheckAutostart  -Run $run }
        'leak'       { Invoke-CamCheckLeak       -Run $run }
        'procs'      { Invoke-CamCheckProcs      -Run $run }
        'relay'      { Invoke-CamCheckRelay      -Run $run }
    }
}

exit (Write-CamTestReport -Run $run)
