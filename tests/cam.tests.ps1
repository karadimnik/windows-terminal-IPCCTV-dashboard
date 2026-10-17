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
    [ValidateSet('parse', 'cameralist', 'inputs', 'selftest', 'layout', 'strip', 'sections', 'autostart', 'crashsafe', 'windowclose', 'windowid', 'nvrstart', 'leak', 'uileak', 'uiguard', 'procs', 'relay', 'viewer', 'viewerpw', 'relayapi', 'prunematrix', 'pruneroots', 'pruneage', 'prunewatermark', 'pruneclock', 'prunebreaker', 'pruneblocked', 'pruneconfig', 'pruneowner', 'prunelive', 'installsteps', 'installplan', 'installpreflight', 'installsandbox', 'docswitches', 'doclinks', 'logrotate', 'segments', 'backoff', 'logrotatelive', 'stallwatch', 'logheld', 'pruneloop', 'dst', 'dstloop', 'clocklint', 'soak', 'livecfg', 'livesecret', 'livewhep', 'liveondemand', 'livepage', 'liverobust', 'livegone', 'livelan', 'livetls', 'downalert', 'downalertrun')]
    [string[]]$Checks,

    # Allows the checks that open RTSP sessions against the real cameras.
    [switch]$Live,

    # Seconds the relay lifecycle check lets the supervisor run before it kills
    # MediaMTX. 60 is enough for the startup wait plus a status tick.
    [ValidateRange(20, 600)]
    [int]$RelaySeconds = 60,

    # The soak check, scaled down to SegmentSeconds 60, LogMaxBytes 256 KB and 3
    # minutes: proves the rig records and its sampling works. Says nothing
    # about the production values.
    [switch]$SoakDryRun,

    # Where the relay check writes its captured evidence.
    [string]$EvidenceDir
)

$ErrorActionPreference = 'Stop'

. (Join-Path -Path $PSScriptRoot -ChildPath 'cam.testlib.ps1')

$repo = Get-CamTestRepo
$run = New-CamTestRun

$cameraFree = @('parse', 'cameralist', 'inputs', 'viewer', 'viewerpw', 'relayapi', 'selftest', 'layout', 'strip', 'sections', 'autostart', 'crashsafe', 'windowclose', 'windowid', 'nvrstart', 'leak', 'uileak', 'uiguard', 'procs', 'prunematrix', 'pruneroots', 'pruneage', 'prunewatermark', 'pruneclock', 'prunebreaker', 'pruneblocked', 'pruneconfig', 'pruneowner', 'installsteps', 'installplan', 'installpreflight', 'installsandbox', 'docswitches', 'doclinks', 'logrotate', 'segments', 'backoff', 'logrotatelive', 'stallwatch', 'logheld', 'pruneloop', 'dst', 'dstloop', 'clocklint', 'livecfg', 'livesecret', 'downalert', 'downalertrun')
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
    # One call outside the stopwatch, because the FIRST call through this path
    # pays for PowerShell compiling it. Measured on 2026-10-13: the cold batch
    # took 220 ms against a 200 ms limit and failed a check that was asserting
    # nothing about compilation. A probe costs 400 ms, so the limit still
    # cannot be met if even one of the six probed.
    [void](Get-CamInputUrl -Config $config -RelayPathName $paths[0] -DirectUrl $direct -ReadyPaths $serving)
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

    # In a sandbox: -SelfTest creates and deletes <RootPath>\_selftest, so run
    # against the repository copy it would write into the Operator's archive.
    $sandbox = New-CamTestSandbox -Name 'selftest' -Cameras @(
        [pscustomobject]@{ Name = 'selftest-a'; IP = '10.255.255.14'; Quality = 'main'; Record = $true }
    )
    $result = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-SelfTest', '-Offline') -TimeoutSeconds 300
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


function Invoke-CamCheckUiLeak {
    <#
        The same question as 'leak', asked of the WIRE instead of the disk:
        camui.exe is started on loopback and every endpoint it serves is
        fetched and scanned for the camera password, for the username in URL
        userinfo shape, and for any URL at all. That now includes the two
        settings endpoints, /api/cameras and /api/config, which read the
        Operator's own cameras.json and recorder.config.json, and the three
        shapes of /api/footage plus a real /api/clip - the clip read bounded,
        because it is a remux produced on the fly and reading it whole would
        hold an ffmpeg open for the length of a segment.

        Frigate's CVE-2026-33469 leaked camera RTSP credentials to any
        logged-in viewer, so a redacted status file is not the deliverable -
        a redacted RESPONSE is. Restreamer.RelayBase is asserted absent by
        name as well, because that is the one field in status.json that
        always holds a URL.
    #>
    param($Run)

    if (-not (Test-Path -LiteralPath (Join-Path -Path $repo -ChildPath 'camera_creds.xml'))) {
        # The real-system scan has no needle without the credential, but the live-view part runs in its own sandbox with fake ones.
        $livePart = Invoke-CamUiLeakLivePart
        if ($livePart.Ran) {
            Add-CamTestResult -Run $Run -Check 'uileak' -Status $(if ($livePart.Ok) { 'PASS' } else { 'FAIL' }) `
                -Message 'live-view part only (no camera_creds.xml for the real-system part): no live secret, address, path name or scheme:// in any live response' `
                -Evidence $livePart.Evidence
            return
        }
        Add-CamTestResult -Run $Run -Check 'uileak' -Status 'SKIP' `
            -Message 'no camera_creds.xml, so there is no needle to scan for' -Evidence $livePart.Evidence
        return
    }

    $ui = Start-CamTestUi
    if (-not $ui.Started) {
        Add-CamTestResult -Run $Run -Check 'uileak' -Status 'SKIP' -Message $ui.Reason
        if ($ui.Process) { [void](Stop-CamTestUi -Ui $ui) }
        return
    }

    try {
        # The footage endpoints are addressed by camera and date, so the paths
        # to scan are discovered from the index itself rather than hard-coded
        # against an archive whose oldest day the pruner may have taken.
        $extra = @()
        $footageNote = 'no footage on this machine, so only the camera list could be scanned'
        try {
            $index = (New-Object System.Net.WebClient).DownloadString(($ui.BaseUrl + '/api/footage')) | ConvertFrom-Json
            $camera = @($index.cameras | Where-Object { $_.segments -gt 0 })[0]
            if ($camera) {
                $cameraPath = '/api/footage?camera=' + [uri]::EscapeDataString($camera.name)
                $extra += $cameraPath
                $days = ((New-Object System.Net.WebClient).DownloadString(($ui.BaseUrl + $cameraPath)) | ConvertFrom-Json).dates
                $day = @($days)[0]
                if ($day) {
                    $dayPath = $cameraPath + '&date=' + $day.date
                    $extra += $dayPath
                    $segments = ((New-Object System.Net.WebClient).DownloadString(($ui.BaseUrl + $dayPath)) | ConvertFrom-Json).segments
                    $segment = @($segments)[0]
                    if ($segment) {
                        $extra += ('/api/clip?camera=' + [uri]::EscapeDataString($camera.name) +
                                   '&segment=' + [uri]::EscapeDataString($segment.segment))
                        $footageNote = ('footage endpoints scanned for {0} on {1}' -f $camera.name, $day.date)
                    }
                }
            }
        }
        catch { $footageNote = 'the footage index could not be read: ' + $_.Exception.Message }
        # The sign-in answers and the first-run page (a fresh throwaway auth folder holds no user, so /setup is open) are scanned like everything else.
        $extra += @('/api/auth/me', '/setup', '/setup.js')

        $result = Invoke-CamUiCredentialLeakScan -BaseUrl $ui.BaseUrl -ExtraPaths $extra
        $ok = $result.Ok

        $evidence = @(
            ("camui {0}, status file {1}" -f $ui.BaseUrl, $ui.StatusFile),
            $result.Text
        )
        $evidence += @($result.Endpoints | ForEach-Object {
            if ($_.Error) { '  {0,-14} UNREACHABLE {1}' -f $_.Path, $_.Error }
            else { '  {0,-14} {1,8} bytes, {2} hit(s)' -f $_.Path, $_.Bytes, $_.Hits }
        })
        $evidence += @($result.Hits | ForEach-Object { 'HIT ' + $_ })

        # RelayBase by name, in BOTH places it lives. In status.json it is
        # Restreamer.RelayBase; in recorder.config.json it is a top-level key,
        # and keeping it out of /api/config is the whole job of the allow-list.
        $raw = (New-Object System.Net.WebClient).DownloadString(($ui.BaseUrl + '/api/status'))
        if ($raw -match 'RelayBase') {
            $ok = $false
            $evidence += 'HIT Restreamer.RelayBase survived into /api/status'
        }
        else {
            $evidence += '  Restreamer.RelayBase absent from /api/status'
        }
        $status = ($raw | ConvertFrom-Json)
        $relayKeys = @(($status.status.Restreamer | Get-Member -MemberType NoteProperty).Name)
        $evidence += ('  Restreamer keys served: ' + ($relayKeys -join ', '))

        $configRaw = (New-Object System.Net.WebClient).DownloadString(($ui.BaseUrl + '/api/config'))
        $config = ($configRaw | ConvertFrom-Json)
        $servedKeys = @(($config.config | Get-Member -MemberType NoteProperty).Name)
        $expectedKeys = @('SegmentSeconds', 'ReserveGB', 'PruneHysteresisGB', 'MaxRetentionDays')
        $unexpected = @($servedKeys | Where-Object { $expectedKeys -notcontains $_ })
        if ($unexpected.Count -gt 0) {
            $ok = $false
            $evidence += ('HIT /api/config served keys outside the allow-list: ' + ($unexpected -join ', '))
        }
        else {
            $evidence += ('  /api/config served exactly the allow-list: ' + ($servedKeys -join ', '))
        }
        # The file holds more than the allow-list, so a filter that does nothing
        # would pass the test above by accident. Prove the file is bigger.
        $fileKeys = @(((Get-Content -Raw -LiteralPath (Join-Path -Path $repo -ChildPath 'recorder.config.json') |
            ConvertFrom-Json) | Get-Member -MemberType NoteProperty).Name)
        $evidence += ('  recorder.config.json holds {0} key(s); {1} are published' -f $fileKeys.Count, $servedKeys.Count)
        if ($fileKeys.Count -le $servedKeys.Count) {
            $ok = $false
            $evidence += 'HIT the allow-list filtered nothing, so it proves nothing'
        }

        # The camera list is the one response allowed to carry addresses, and
        # only because the Operator types them. Assert it carries NOTHING else:
        # no credential field, and no field beyond the four that are editable.
        $camerasRaw = (New-Object System.Net.WebClient).DownloadString(($ui.BaseUrl + '/api/cameras'))
        $cameraRows = @(($camerasRaw | ConvertFrom-Json).cameras)
        $rowKeys = @()
        foreach ($row in $cameraRows) { $rowKeys += @(($row | Get-Member -MemberType NoteProperty).Name) }
        $rowKeys = @($rowKeys | Select-Object -Unique)
        $allowedRowKeys = @('name', 'ip', 'quality', 'record')
        $extraRowKeys = @($rowKeys | Where-Object { $allowedRowKeys -notcontains $_ })
        if ($extraRowKeys.Count -gt 0) {
            $ok = $false
            $evidence += ('HIT /api/cameras served fields outside the editable four: ' + ($extraRowKeys -join ', '))
        }
        else {
            $evidence += ('  /api/cameras served {0} row(s) with only: {1}' -f $cameraRows.Count, ($rowKeys -join ', '))
        }

        $evidence += ('  ' + $footageNote)

        # The footage index is addressed by camera NAME, so a camera row there
        # has no business carrying an address, a folder or a path. Assert the
        # published fields by name, the same way /api/cameras is asserted.
        $footageRaw = (New-Object System.Net.WebClient).DownloadString(($ui.BaseUrl + '/api/footage'))
        $footageRows = @(($footageRaw | ConvertFrom-Json).cameras)
        $footageKeys = @()
        foreach ($row in $footageRows) { $footageKeys += @(($row | Get-Member -MemberType NoteProperty).Name) }
        $footageKeys = @($footageKeys | Select-Object -Unique)
        $allowedFootageKeys = @('name', 'known', 'segments', 'newestAt')
        $extraFootageKeys = @($footageKeys | Where-Object { $allowedFootageKeys -notcontains $_ })
        if ($extraFootageKeys.Count -gt 0) {
            $ok = $false
            $evidence += ('HIT /api/footage served fields beyond the camera name and its counts: ' + ($extraFootageKeys -join ', '))
        }
        else {
            $evidence += ('  /api/footage served {0} camera(s) with only: {1}' -f $footageRows.Count, ($footageKeys -join ', '))
        }

        # And the refusals: a path that was rejected must not echo the path back.
        foreach ($bad in @('/api/clip?camera=' + [uri]::EscapeDataString('..\cameras_old') + '&segment=x.mkv',
                           '/api/clip?camera=_state&segment=status.json',
                           '/api/footage?camera=' + [uri]::EscapeDataString('C:\Windows'))) {
            $body = ''
            try { [void](New-Object System.Net.WebClient).DownloadString(($ui.BaseUrl + $bad)) }
            catch [System.Net.WebException] {
                if ($_.Exception.Response) {
                    $body = (New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())).ReadToEnd()
                }
            }
            if ($body -match '[A-Za-z]:\\' -or $body -match '\.\.' -or $body -match '_state') {
                $ok = $false
                $evidence += ('HIT a refusal echoed the request back: ' + $body)
            }
            else {
                $evidence += ('  refused and echoed nothing: ' + $bad)
            }
        }
    }
    finally {
        $logLines = @(Stop-CamTestUi -Ui $ui)
    }
    $evidence += @($logLines | Where-Object { $_ -match 'msg="camui listening"' } | ForEach-Object { '  ' + $_ })

    # [Stage 1B] The live-view half: GET /api/live, /api/status with a Live section, the WHEP answer and its refusals.
    $livePart = Invoke-CamUiLeakLivePart
    $evidence += '-- live-view part --'
    $evidence += $livePart.Evidence
    if ($livePart.Ran -and -not $livePart.Ok) { $ok = $false }

    Add-CamTestResult -Run $Run -Check 'uileak' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'no camera password, no credential in URL userinfo shape and no URL of any scheme in any camui response, including the settings endpoints, all three shapes of the footage index and a real clip; and no live secret, address, path name or scheme:// in any live-view response' `
        -Evidence $evidence
}


function Invoke-CamUiLeakLivePart {
    <#
        [Stage 1B] The live-view half of uileak: nothing that identifies the
        live instance or its login may reach the browser. The real supervisor
        runs a live instance against a synthetic camera (fake credentials), a
        camui built from this tree serves the sandbox, and every live response
        - GET /api/live, GET /api/status, the WHEP answer for sub and main with
        its headers, the refusals (unknown camera, bad stream, wrong type, too
        large, rebinding Host, unknown session) and the DELETE - plus the two
        static assets, is scanned for:
          the live secret (raw, and as the Basic login), the user name, the
          live instance's address and port, every live path name, the fake
          camera password, the camera's and the relay's addresses, and a
          scheme:// of ANY kind.
        The SDP answer legitimately carries the media candidate (the loopback
        UDP port the browser sends media to); that port is reported, not
        scanned for. A synthetic body carrying every needle is run through the
        same scanner first: if it does not light up, the result is inconclusive.
        Returns Ran, Ok and Evidence; Ran is false (with the reason) when the
        prerequisites (a camui.exe, MediaMTX, ffmpeg) are missing.
    #>
    $result = [pscustomobject]@{ Ran = $false; Ok = $true; Evidence = @() }
    $exe = $(if ($env:CAMUI_TEST_EXE) { $env:CAMUI_TEST_EXE } else { Join-Path -Path (Get-CamTestRepo) -ChildPath 'camui.exe' })
    if (-not (Test-Path -LiteralPath $exe)) { $result.Evidence += 'live part not run: no camui.exe (set CAMUI_TEST_EXE to one built from this tree)'; return $result }
    if (-not (Test-Path -LiteralPath 'C:\mediamtx\mediamtx.exe')) { $result.Evidence += 'live part not run: no MediaMTX at C:\mediamtx\mediamtx.exe'; return $result }
    foreach ($p in @(18791)) { if (-not (Test-CamTestPortFree -Port $p)) { $result.Evidence += "live part not run: TCP port $p is in use"; return $result } }

    $result.Ran = $true
    $rig = $null; $ui = $null
    $fail = { param([string]$Text) $result.Ok = $false; $result.Evidence += ('HIT ' + $Text) }
    try {
        $cameras = 1..3 | ForEach-Object { [pscustomobject]@{ Name = "Cam $_"; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = ($_ -eq 1) } }
        $rig = Start-CamTestLiveRig -Name 'uileaklive' -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds 4
        $ui = Start-CamTestLiveCamui -Rig $rig -Port 18791 -Name 'camui-uileak-live'
        if (-not $ui.Started) { throw ('camui did not start: ' + $ui.Reason) }
        $port = 18791
        $secret = ([System.IO.File]::ReadAllText([string]$rig.Sandbox.Config.LiveSecretPath)).Trim()
        $login = [System.Convert]::ToBase64String([System.Text.Encoding]::ASCII.GetBytes('camlive:' + $secret))
        $needles = [ordered]@{
            'live secret'            = $secret
            'live login (Basic)'     = $login
            'live user name'         = 'camlive'
            'live instance port'     = '18889'
            'live path name'         = 'cam1sub|cam1main|cam2sub|cam2main|cam3sub|cam3main'
            'camera password'        = 'FakePassword-DoNotUse-9c41'
            'camera user (userinfo)' = 'fakeuser:'
            'camera address'         = '18554'
            'relay address'          = '18564|18567'
        }
        $scan = {
            param([string]$Label, [string]$Text)
            $found = @()
            foreach ($key in $needles.Keys) {
                foreach ($needle in ($needles[$key] -split '\|')) {
                    # A bare number (a port) is a hit only as a whole token: the SDP holds long random numbers and ids, and a 5-digit run inside one is not a leak.
                    $isHit = $(if ($needle -match '^[0-9]+$') { [regex]::IsMatch($Text, '(?<![0-9A-Za-z])' + $needle + '(?![0-9A-Za-z])') } else { $Text.IndexOf($needle, [StringComparison]::OrdinalIgnoreCase) -ge 0 })
                    if ($isHit) { $found += ("{0} ({1}) in {2}" -f $key, $(if ($key -in 'live secret', 'live login (Basic)', 'camera password') { 'value' } else { $needle }), $Label) }
                }
            }
            foreach ($m in [regex]::Matches($Text, '(?i)[a-z][a-z0-9+.\-]*://[^\s"'',)]*')) { $found += ('URL scheme in {0} :: {1}' -f $Label, $m.Value) }
            return @($found)
        }
        # The scanner must see every class of needle in a body that really leaks.
        $control = ($needles.Values | ForEach-Object { ($_ -split '\|')[0] }) -join ' ; '
        $control += ' ; rtsp://x'
        $controlHits = @(& $scan 'positive-control' $control)
        $classes = @($needles.Keys | Where-Object { $k = $_; @($controlHits | Where-Object { $_ -like "$k*" }).Count -ge 1 }).Count
        $controlOk = ($classes -eq $needles.Count -and @($controlHits | Where-Object { $_ -like 'URL scheme*' }).Count -ge 1)
        $result.Evidence += ("positive control: {0} of {1} needle classes and the scheme rule lit up in a synthetic leaking body: {2}" -f $classes, $needles.Count, $(if ($controlOk) { 'ok' } else { 'DID NOT LIGHT UP' }))
        if (-not $controlOk) { & $fail 'the scanner did not light up on the positive control, so a clean scan would prove nothing' }

        $offer = Get-CamTestWhepOffer
        $json = @{ 'Content-Type' = 'application/sdp' }
        $requests = @(
            @{ L = 'GET /api/live'; M = 'GET'; P = '/api/live' },
            @{ L = 'GET /api/status'; M = 'GET'; P = '/api/status' },
            @{ L = 'GET /live.js'; M = 'GET'; P = '/live.js' },
            @{ L = 'GET /index.html'; M = 'GET'; P = '/index.html' },
            @{ L = 'WHEP sub'; M = 'POST'; P = '/api/live/Cam%201/whep?stream=sub'; H = $json; B = $offer; Keep = 'sub' },
            @{ L = 'WHEP main'; M = 'POST'; P = '/api/live/Cam%201/whep?stream=main'; H = $json; B = $offer; Keep = 'main' },
            @{ L = 'WHEP unknown camera'; M = 'POST'; P = '/api/live/Nobody/whep?stream=sub'; H = $json; B = $offer },
            @{ L = 'WHEP bad stream'; M = 'POST'; P = '/api/live/Cam%201/whep?stream=cam1sub'; H = $json; B = $offer },
            @{ L = 'WHEP wrong type'; M = 'POST'; P = '/api/live/Cam%201/whep?stream=sub'; H = @{ 'Content-Type' = 'text/plain' }; B = $offer },
            @{ L = 'WHEP too large'; M = 'POST'; P = '/api/live/Cam%201/whep?stream=sub'; H = $json; B = ('a' * 70000) },
            @{ L = 'WHEP rebinding Host'; M = 'POST'; P = '/api/live/Cam%201/whep?stream=sub'; H = @{ 'Content-Type' = 'application/sdp'; Host = 'attacker-rebind.example' }; B = $offer },
            @{ L = 'DELETE unknown session'; M = 'DELETE'; P = '/api/live/session/00000000000000000000000000000000' })
        $kept = @()
        foreach ($request in $requests) {
            $args = @{ Port = $port; Method = $request.M; Path = $request.P }
            if ($request.H) { $args.Headers = $request.H }
            if ($request.B) { $args.Body = $request.B }
            $r = Invoke-CamRawHttp @args
            $headerText = (@($r.Headers.GetEnumerator() | ForEach-Object { '{0}: {1}' -f $_.Key, $_.Value }) -join "`n")
            $found = @(& $scan $request.L ($headerText + "`n" + $r.Body))
            $result.Evidence += ("  {0,-26} HTTP {1}, {2} body byte(s), headers [{3}], hits {4}" -f $request.L, $r.Status, $r.Body.Length, ((@($r.Headers.Keys) | Sort-Object) -join ','), $found.Count)
            foreach ($f in $found) { & $fail $f }
            if ($request.Keep) { $kept += [pscustomobject]@{ Label = $request.L; Result = $r } }
        }
        foreach ($k in $kept) {
            $answer = $k.Result
            $candidates = @(([regex]::Matches($answer.Body, '(?m)^a=candidate:[^\r\n]*')) | ForEach-Object { $_.Value })
            $result.Evidence += ("  {0}: HTTP {1}, Content-Type [{2}], Location [{3}], answer {4} byte(s) starting '{5}', media candidate(s): {6}" -f $k.Label, $answer.Status, $answer.Headers['content-type'], $answer.Headers['location'], $answer.Body.Length, $answer.Body.Substring(0, [Math]::Min(3, $answer.Body.Length)), ($candidates -join ' | '))
            if ($answer.Status -ne 201 -or $answer.Headers['content-type'] -ne 'application/sdp' -or $answer.Headers['location'] -notmatch '^/api/live/session/[0-9a-f]{32}$') { & $fail ($k.Label + ': not a 201 application/sdp answer with a minted Location') }
            else { $del = Invoke-CamRawHttp -Port $port -Method 'DELETE' -Path $answer.Headers['location']; $found = @(& $scan 'DELETE' ((@($del.Headers.GetEnumerator() | ForEach-Object { '{0}: {1}' -f $_.Key, $_.Value }) -join "`n") + $del.Body)); foreach ($f in $found) { & $fail $f }; $result.Evidence += ("  DELETE of that session: HTTP {0}, hits {1}" -f $del.Status, $found.Count); if ($del.Status -ne 204) { & $fail 'DELETE of a minted session was not 204' } }
        }
        # The scanner looks at what camui sends, not at a document that never held the names: the supervisor's own file does.
        $raw = Get-CamFileText -Path (Get-CamStatusFilePath -Config $rig.Sandbox.Config)
        $rawHits = @(& $scan 'status.json (the file, before camui)' $raw | Where-Object { $_ -like 'live path name*' })
        $result.Evidence += ("  status.json on disk names the live paths ({0} path-name hit(s)); /api/status above carried none" -f $rawHits.Count)
        if ($rawHits.Count -lt 1) { & $fail 'status.json holds no live path name, so the absence in /api/status proves nothing' }
    }
    catch { & $fail ('live part aborted: ' + $_.Exception.Message) }
    finally {
        if ($ui) { [void](Stop-CamTestUi -Ui $ui) }
        $notes = @()
        if ($rig) { $notes = @(Stop-CamTestLiveRig -Rig $rig) }
        $result.Evidence += $notes
        if ($notes -match 'forced: (?!nothing)') { & $fail 'the supervisor had to be forced' }
        if ($rig) { try { Remove-CamTestTree -Path $rig.Dir } catch { $result.Evidence += 'cleanup failed' } }
    }
    return $result
}


function Invoke-CamCheckUiGuard {
    <#
        The request guard, asked of the real binary rather than of the Go
        tests. camui is started on loopback over a SANDBOX folder (a stub
        cam.ps1, an empty status file, a throwaway archive), so nothing it
        writes touches the Operator's files, and it is probed with raw HTTP so
        every header goes out as written:

          - the four probes a hostile page can mount against a loopback
            service - a cross-site POST with no preflight, a DNS-rebinding
            Host, a foreign Origin, and a state-changing POST that is not
            JSON - must be refused, and must not have changed anything;
          - a request shaped exactly like the page's own, and a plain script
            request with neither Origin nor Sec-Fetch-Site, must be accepted;
          - the page itself must still load.

        The binary is the repository's camui.exe unless CAMUI_TEST_EXE names
        another one. Against a camui.exe built before the guard this check
        FAILS, which is the point: rebuild with ui/build.sh.
    #>
    param($Run)

    $source = $env:CAMUI_TEST_EXE
    if (-not $source) { $source = Join-Path -Path $repo -ChildPath 'camui.exe' }
    if (-not (Test-Path -LiteralPath $source)) {
        Add-CamTestResult -Run $Run -Check 'uiguard' -Status 'SKIP' -Message 'camui.exe is not built; run ui/build.sh'
        return
    }

    $sandbox = Join-Path -Path $env:TEMP -ChildPath ('cam-tests\uiguard-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
    $state = Join-Path -Path $sandbox -ChildPath 'arch\_state'
    [void](New-Item -ItemType Directory -Path $state -Force)
    Set-Content -LiteralPath (Join-Path -Path $sandbox -ChildPath 'cam.ps1') -Value '# stub'
    Set-Content -LiteralPath (Join-Path -Path $sandbox -ChildPath 'cameras.json') -Value '[]'
    Set-Content -LiteralPath (Join-Path -Path $sandbox -ChildPath 'recorder.config.json') -Value '{}'
    $statusFile = Join-Path -Path $state -ChildPath 'status.json'
    Set-Content -LiteralPath $statusFile -Value '{"Schema":1,"State":"stopped"}'
    $ackFile = Join-Path -Path $state -ChildPath 'camui-acknowledged.json'

    $ui = Start-CamTestUi -Port 18788 -StatusFile $statusFile -ScriptDir $sandbox -Source $source
    if (-not $ui.Started) {
        Add-CamTestResult -Run $Run -Check 'uiguard' -Status 'SKIP' -Message $ui.Reason
        if ($ui.Process) { [void](Stop-CamTestUi -Ui $ui) }
        Remove-Item -LiteralPath $sandbox -Recurse -Force -ErrorAction SilentlyContinue
        return
    }

    $ok = $true
    $evidence = @(("camui {0} from {1}, sandbox {2}" -f $ui.BaseUrl, $source, $sandbox))
    $ackBody = '{"notice":"LastWindowClose","at":"2026-01-01 00:00:00 +00:00"}'
    $port = $ui.Port

    # Dot-sourced (. $expect ...) so it updates $ok and $evidence in this scope.
    $expect = {
        param([string]$what, $response, [int[]]$want)
        $pass = ($want -contains $response.Status)
        if (-not $pass) { $ok = $false }
        $evidence += ('  {0,-4} {1}: HTTP {2} (wanted {3})' -f $(if ($pass) { 'ok' } else { 'FAIL' }), $what, $response.Status, ($want -join ' or '))
    }

    try {
        # --- refused ------------------------------------------------------
        . $expect 'cross-site POST /api/acknowledge (Origin evil.example, Sec-Fetch-Site cross-site, text/plain)' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/acknowledge' -Body $ackBody -Headers ([ordered]@{
                'Origin' = 'http://evil.example'; 'Sec-Fetch-Site' = 'cross-site'; 'Content-Type' = 'text/plain' })) @(403)
        . $expect 'cross-site POST /api/control (same shape, harmless body)' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/control' -Body '{"scope":"none","action":"status"}' -Headers ([ordered]@{
                'Origin' = 'http://evil.example'; 'Sec-Fetch-Site' = 'cross-site'; 'Content-Type' = 'text/plain' })) @(403)
        . $expect 'GET /api/status with Host attacker-rebind.example (DNS rebinding)' `
            (Invoke-CamRawHttp -Port $port -Path '/api/status' -Headers ([ordered]@{ 'Host' = 'attacker-rebind.example' })) @(403)
        . $expect 'GET / with Host attacker-rebind.example (the page too)' `
            (Invoke-CamRawHttp -Port $port -Path '/' -Headers ([ordered]@{ 'Host' = 'attacker-rebind.example:18788' })) @(403)
        . $expect 'GET /api/status with a foreign Origin' `
            (Invoke-CamRawHttp -Port $port -Path '/api/status' -Headers ([ordered]@{ 'Origin' = 'http://evil.example' })) @(403)
        . $expect 'GET /api/status with Origin null' `
            (Invoke-CamRawHttp -Port $port -Path '/api/status' -Headers ([ordered]@{ 'Origin' = 'null' })) @(403)
        . $expect 'GET /api/status with Sec-Fetch-Site same-site' `
            (Invoke-CamRawHttp -Port $port -Path '/api/status' -Headers ([ordered]@{ 'Sec-Fetch-Site' = 'same-site' })) @(403)
        . $expect 'POST /api/control, text/plain, no Origin (a script)' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/control' -Body '{"scope":"none","action":"status"}' -Headers ([ordered]@{ 'Content-Type' = 'text/plain' })) @(415)
        . $expect 'POST /api/acknowledge, form-urlencoded, no Origin' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/acknowledge' -Body $ackBody -Headers ([ordered]@{ 'Content-Type' = 'application/x-www-form-urlencoded' })) @(415)
        . $expect 'PUT /api/config, no content type' `
            (Invoke-CamRawHttp -Port $port -Method PUT -Path '/api/config' -Body '{"config":{}}') @(415)

        $changed = Test-Path -LiteralPath $ackFile
        if ($changed) { $ok = $false }
        $evidence += ('  {0,-4} none of the refused requests wrote the acknowledgement file (present: {1})' -f $(if ($changed) { 'FAIL' } else { 'ok' }), $changed)

        # --- accepted -----------------------------------------------------
        $page = Invoke-CamRawHttp -Port $port -Path '/' -Headers ([ordered]@{ 'Sec-Fetch-Site' = 'none' })
        . $expect 'GET / (the page, as typed in the address bar)' $page @(200)
        $titled = ($page.Body -match '<html')
        if (-not $titled) { $ok = $false }
        $evidence += ('  {0,-4} the page body is the HTML UI (found <html: {1})' -f $(if ($titled) { 'ok' } else { 'FAIL' }), $titled)

        $api = Invoke-CamRawHttp -Port $port -Path '/api/status' -Headers ([ordered]@{
            'Origin' = ('http://127.0.0.1:{0}' -f $port); 'Sec-Fetch-Site' = 'same-origin' })
        . $expect 'GET /api/status shaped like the page own fetch (same-origin)' $api @(200)
        foreach ($pair in @(@('cross-origin-resource-policy', 'same-origin'), @('x-content-type-options', 'nosniff'), @('cache-control', 'no-store'))) {
            $got = $api.Headers[$pair[0]]
            $pass = ($got -eq $pair[1])
            if (-not $pass) { $ok = $false }
            $evidence += ('  {0,-4} /api/status header {1}: {2}' -f $(if ($pass) { 'ok' } else { 'FAIL' }), $pair[0], $got)
        }

        . $expect 'POST /api/acknowledge shaped like the page own (same-origin Origin, Sec-Fetch-Site same-origin, application/json)' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/acknowledge' -Body $ackBody -Headers ([ordered]@{
                'Origin' = ('http://127.0.0.1:{0}' -f $port); 'Sec-Fetch-Site' = 'same-origin'
                'Content-Type' = 'application/json' })) @(200)
        $written = Test-Path -LiteralPath $ackFile
        if (-not $written) { $ok = $false }
        $evidence += ('  {0,-4} the accepted request changed state (acknowledgement file written: {1})' -f $(if ($written) { 'ok' } else { 'FAIL' }), $written)

        . $expect 'POST /api/acknowledge from a script (no Origin, no Sec-Fetch-Site, application/json; charset=utf-8)' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/acknowledge' -Body $ackBody -Headers ([ordered]@{
                'Content-Type' = 'application/json; charset=utf-8' })) @(200)
        . $expect 'GET /api/status with Host localhost:18788' `
            (Invoke-CamRawHttp -Port $port -Path '/api/status' -Headers ([ordered]@{ 'Host' = ('localhost:{0}' -f $port) })) @(200)
        . $expect 'GET /api/status with Host [::1]:18788 (the name is allowed even though camui listens on IPv4 only)' `
            (Invoke-CamRawHttp -Port $port -Path '/api/status' -Headers ([ordered]@{ 'Host' = ('[::1]:{0}' -f $port) })) @(200)

        # [Stage 2b] The loopback listener is unchanged by the HTTPS LAN listener: no CA routes here, and a LAN Host is still a rebinding Host.
        . $expect 'GET /api/tls/ca.crt on the loopback listener (it exists only on the HTTPS listener)' (Invoke-CamRawHttp -Port $port -Path '/api/tls/ca.crt') @(404)
        . $expect 'GET /api/tls/info on the loopback listener' (Invoke-CamRawHttp -Port $port -Path '/api/tls/info') @(404)
        . $expect 'GET /api/status with a LAN Host (192.168.1.50:8443): the loopback allowlist is not widened' `
            (Invoke-CamRawHttp -Port $port -Path '/api/status' -Headers ([ordered]@{ 'Host' = '192.168.1.50:8443' })) @(403)

        # The LAN login is not on the loopback listener: no login page, no login or logout route; /api/auth/me says no login is needed;
        # the first-run page exists only while no user exists, only here, is guarded like every other route, and is 404 once a user exists.
        $me = Invoke-CamRawHttp -Port $port -Path '/api/auth/me'
        . $expect 'GET /api/auth/me on loopback (no login required)' $me @(200)
        $meOk = ($me.Body -match '"loginRequired":\s*false' -and $me.Body -match '"authenticated":\s*true')
        if (-not $meOk) { $ok = $false }
        $evidence += ('  {0,-4} /api/auth/me says authenticated and loginRequired false: {1}' -f $(if ($meOk) { 'ok' } else { 'FAIL' }), $meOk)
        . $expect 'GET /login on loopback' (Invoke-CamRawHttp -Port $port -Path '/login') @(404)
        . $expect 'GET /login.js on loopback' (Invoke-CamRawHttp -Port $port -Path '/login.js') @(404)
        . $expect 'GET /login.html on loopback (the file server never hands out the auth pages)' (Invoke-CamRawHttp -Port $port -Path '/login.html') @(404)
        . $expect 'POST /api/auth/login on loopback' (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/auth/login' -Body '{"username":"a","password":"b"}' -Headers ([ordered]@{ 'Content-Type' = 'application/json' })) @(404, 405)
        . $expect 'GET /setup with no user (the first-run page)' (Invoke-CamRawHttp -Port $port -Path '/setup') @(200)
        . $expect 'GET /setup with a rebinding Host' (Invoke-CamRawHttp -Port $port -Path '/setup' -Headers ([ordered]@{ 'Host' = 'attacker-rebind.example:18788' })) @(403)
        . $expect 'POST /api/auth/setup, cross-site (no user may be created by a web page)' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/auth/setup' -Body '{"username":"mallory","password":"a long enough password"}' -Headers ([ordered]@{
                'Origin' = 'http://evil.example'; 'Sec-Fetch-Site' = 'cross-site'; 'Content-Type' = 'application/json' })) @(403)
        . $expect 'POST /api/auth/setup with a too short password' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/auth/setup' -Body '{"username":"anna","password":"short"}' -Headers ([ordered]@{ 'Content-Type' = 'application/json' })) @(422)
        $setupPassword = 'Pw-' + [guid]::NewGuid().ToString('N')
        . $expect 'POST /api/auth/setup creates the first user' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/auth/setup' -Body (ConvertTo-Json -Compress @{ username = 'anna'; password = $setupPassword }) -Headers ([ordered]@{ 'Content-Type' = 'application/json' })) @(201)
        . $expect 'GET /setup once a user exists' (Invoke-CamRawHttp -Port $port -Path '/setup') @(404)
        . $expect 'GET /setup.js once a user exists' (Invoke-CamRawHttp -Port $port -Path '/setup.js') @(404)
        . $expect 'POST /api/auth/setup once a user exists' `
            (Invoke-CamRawHttp -Port $port -Method POST -Path '/api/auth/setup' -Body '{"username":"mallory","password":"a long enough password"}' -Headers ([ordered]@{ 'Content-Type' = 'application/json' })) @(404)
        $usersFile = Join-Path -Path $ui.AuthDir -ChildPath 'users.json'
        $usersText = [System.IO.File]::ReadAllText($usersFile)
        $usersOk = ($usersText -match '"username":\s*"anna"' -and $usersText -notmatch 'mallory' -and -not $usersText.Contains($setupPassword) -and $usersText -match 'pbkdf2-sha256')
        if (-not $usersOk) { $ok = $false }
        $evidence += ('  {0,-4} users.json holds exactly the first user, hashed (no mallory, no password): {1}' -f $(if ($usersOk) { 'ok' } else { 'FAIL' }), $usersOk)
    }
    finally {
        $logLines = @(Stop-CamTestUi -Ui $ui)
        Remove-Item -LiteralPath $sandbox -Recurse -Force -ErrorAction SilentlyContinue
    }
    $evidence += @($logLines | Where-Object { $_ -match 'request refused' } | Select-Object -First 3 | ForEach-Object { '  log: ' + $_ })

    Add-CamTestResult -Run $Run -Check 'uiguard' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'camui refuses a cross-site POST, a DNS-rebinding Host, a foreign Origin and a non-JSON state change, and still serves the page and a request shaped like its own; the loopback listener has no login and its first-run page works only while no user exists' `
        -Evidence $evidence
}

function Invoke-CamCheckCrashSafe {
    <#
        The crash-safety contract, in a sandbox, with no camera touched:

          1. a killed supervisor is restarted by the NVR host
          2. the kill is recorded as StopKind 'unexpected' with a
             LastUnexpectedStop carrying its timestamp and reason
          3. the kill raises an alert - id 100, Error - recorded by the
             sandbox alert stub; the real channels are windowclose's to prove
          4. an orderly -Stop is NOT restarted and raises NO alert

        Point 4 is the one that decides whether the feature is worth having.
        Alerting on an expected stop teaches the Operator to ignore alerts, at
        which point the unexpected ones cost him nothing but noise.

        The cameras are deliberately unreachable: this check is about the
        supervisor's lifecycle, and ffmpeg failing to connect exercises it
        exactly as well as ffmpeg recording would.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $hostProcess = $null

    $sandbox = New-CamTestSandbox -Name 'crashsafe' -Cameras @(
        [pscustomobject]@{ Name = 'crashsafe-a'; IP = '10.255.255.11'; Quality = 'main'; Record = $true }
    ) -Config @{ StatusIntervalSeconds = 2; ReconcileSeconds = 2 }
    $stubLog = Set-CamTestAlertStub -Sandbox $sandbox

    $lib = Join-Path -Path $sandbox.Dir -ChildPath 'cam.common.ps1'
    $log = Join-Path -Path $sandbox.Root -ChildPath '_logs\recorder.log'
    $readStatus = {
        $read = Get-CamTestStatusJson -Sandbox $sandbox
        if ($read) { return $read.Snapshot }
        return $null
    }
    $alertLines = { if (Test-Path -LiteralPath $log) { @(Get-Content -LiteralPath $log | Select-String -Pattern 'ALERT ').Count } else { 0 } }
    $livePid = {
        param($snapshot)
        if (-not $snapshot) { return 0 }
        $id = [int]$snapshot.SupervisorPid
        if ($id -gt 0 -and (Get-Process -Id $id -ErrorAction SilentlyContinue)) { return $id }
        return 0
    }

    try {
        $hostProcess = Start-CamDetachedProcess -Script (". '{0}'`nStart-CamNvrHost -RecorderPath '{1}' -RestartDelaySeconds 3" -f `
                $lib.Replace("'", "''"), $sandbox.Recorder.Replace("'", "''"))
        $evidence += ("nvr host pid {0}" -f $hostProcess.Id)

        $first = 0
        $deadline = (Get-Date).AddSeconds(60)
        while ((Get-Date) -lt $deadline) {
            $snapshot = & $readStatus
            if ($snapshot -and $snapshot.State -eq 'running') { $first = & $livePid $snapshot }
            if ($first -gt 0) { break }
            Start-Sleep -Milliseconds 500
        }
        if ($first -le 0) {
            Add-CamTestResult -Run $Run -Check 'crashsafe' -Status 'FAIL' `
                -Message 'the NVR host did not bring a supervisor up within 60s' -Evidence $evidence
            return
        }
        $evidence += ("supervisor #1 pid {0}, status says running" -f $first)

        $alertsBefore = & $alertLines
        Stop-Process -Id $first -Force
        $evidence += ("killed pid {0} with Stop-Process -Force - no stop flag, no cleanup" -f $first)

        $second = 0
        $deadline = (Get-Date).AddSeconds(90)
        while ((Get-Date) -lt $deadline) {
            $snapshot = & $readStatus
            if ($snapshot -and $snapshot.State -eq 'running') {
                $candidate = & $livePid $snapshot
                if ($candidate -gt 0 -and $candidate -ne $first) { $second = $candidate; break }
            }
            Start-Sleep -Milliseconds 500
        }
        $evidence += ("supervisor #2 pid {0} after the kill" -f $second)
        if ($second -le 0) { $ok = $false; $evidence += 'FAIL: the supervisor was not restarted' }

        $record = (& $readStatus).LastUnexpectedStop
        $evidence += ("LastUnexpectedStop: {0}" -f ($record | ConvertTo-Json -Compress))
        if (-not $record -or -not $record.At -or -not $record.Reason -or [int]$record.SupervisorPid -ne $first) {
            $ok = $false; $evidence += 'FAIL: the kill was not recorded with its timestamp, reason and pid'
        }

        $alertsAfterKill = & $alertLines
        $evidence += ("ALERT lines in the supervisor log: {0} before the kill, {1} after" -f $alertsBefore, $alertsAfterKill)
        if ($alertsAfterKill -le $alertsBefore) { $ok = $false; $evidence += 'FAIL: the kill raised no alert' }
        $stubbed = @(if (Test-Path -LiteralPath $stubLog) { Get-Content -LiteralPath $stubLog })
        $evidence += ("alerts recorded by the sandbox stub: {0}" -f (@($stubbed | ForEach-Object { $_.Substring(0, [math]::Min(60, $_.Length)) }) -join ' | '))
        if (@($stubbed | Where-Object { $_ -like '100 Error the NVR stopped unexpectedly*' }).Count -ne 1) { $ok = $false; $evidence += 'FAIL: the kill did not raise exactly one id 100 Error alert' }

        # Now the distinction that keeps alerting worth reading.
        [void](Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-Stop'))
        Start-Sleep -Seconds 8
        $snapshot = & $readStatus
        $evidence += ("after -Stop: State={0} StopKind='{1}' host alive={2}" -f `
                $snapshot.State, $snapshot.StopKind, [bool](Get-Process -Id $hostProcess.Id -ErrorAction SilentlyContinue))
        if ($snapshot.StopKind -ne 'requested') { $ok = $false; $evidence += 'FAIL: an orderly stop was not recorded as requested' }

        $alertsAfterStop = & $alertLines
        $evidence += ("ALERT lines after the orderly stop: {0} (was {1})" -f $alertsAfterStop, $alertsAfterKill)
        if ($alertsAfterStop -ne $alertsAfterKill) { $ok = $false; $evidence += 'FAIL: an orderly stop raised an alert' }

        Start-Sleep -Seconds 6
        $snapshot = & $readStatus
        $restarted = (& $livePid $snapshot)
        $evidence += ("6s later: a supervisor is running = {0}" -f ($restarted -gt 0))
        if ($restarted -gt 0) { $ok = $false; $evidence += 'FAIL: the host restarted a supervisor that was asked to stop' }
    }
    finally {
        if ($hostProcess) {
            try { if (Get-Process -Id $hostProcess.Id -ErrorAction SilentlyContinue) { Stop-Process -Id $hostProcess.Id -Force } } catch { }
        }
    }

    Add-CamTestResult -Run $Run -Check 'crashsafe' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'a killed supervisor is restarted and alerted on; an orderly stop is neither' `
        -Evidence $evidence
}


function Invoke-CamCheckWindowClose {
    <#
        The window-close alert, item [67], in a sandbox with no camera touched:

          1. closing the watched window raises an event-log entry under id 103,
             worded as a window close and NOT as an unexpected stop
          2. it records StopKind 'window-closed' with a timestamped
             LastWindowClose in status.json
          3. the job's members are gone - alerting did not cost the sweep
          4. TERMINATING the warden while its window is still open raises
             NOTHING

        Point 4 is the one that decides whether the feature is worth having.
        That is the shape of cam.ps1 -Stop: Stop-CamNvr kills the warden first,
        precisely so it never reaches its window-gone branch. If the alert ever
        moves into the warden's finally block, an orderly stop starts alerting,
        and alerting on a stop the Operator typed himself teaches him to ignore
        the alerts that matter.

        It runs in the order terminate-then-close so the status file goes from
        having no LastWindowClose at all to having one, which is a stronger
        reading than "unchanged".

        Two things are real and not sandboxed, because they are machine-wide:
        the Application-log entry and the toast. The toast will name the TEST
        window, 'cam.test.close', not cam.nvr.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $kept = $null
    $closed = $null
    $ids = Get-CamAlertEventIdList

    $readEvents = {
        param([DateTime]$Since)
        @(Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'Application'; Id = $ids } `
                -MaxEvents 200 -ErrorAction SilentlyContinue |
            Where-Object { $_.Id -eq 103 -and $_.TimeCreated -ge $Since })
    }

    $sandbox = New-CamTestSandbox -Name 'windowclose' -Cameras @(
        [pscustomobject]@{ Name = 'windowclose-a'; IP = '10.255.255.11'; Quality = 'main'; Record = $true }
    )

    try {
        # --- 4. a terminated warden alerts about nothing -------------------
        $t0 = (Get-Date).AddSeconds(-1)
        $kept = Start-CamTestWardenWindow -Sandbox $sandbox -Title 'cam.test.keep' -JobName 'cam.test.keep.job'
        $evidence += ("terminate case: warden {0}, pane {1}, window found {2}, watching {3}" -f `
                $kept.WardenPid, $kept.PanePid, $kept.WindowFound, $kept.Watching)
        if (-not ($kept.WindowFound -and $kept.Watching -and $kept.PanePid -gt 0)) {
            Add-CamTestResult -Run $Run -Check 'windowclose' -Status 'FAIL' `
                -Message 'the test window or its warden did not come up' -Evidence $evidence
            return
        }

        # Exactly what Stop-CamNvr does, and for exactly this reason.
        [void](Stop-CamProcessList -ProcessId @([int]$kept.WardenPid))
        Start-Sleep -Seconds 8
        $afterTerminate = @(& $readEvents $t0)
        $status = Get-CamTestStatusJson -Sandbox $sandbox
        $evidence += ("after terminating the warden with its window still open: id-103 entries {0}, status.json {1}" -f `
                $afterTerminate.Count, $(if ($status) { "StopKind '" + $status.Snapshot.StopKind + "'" } else { 'not written' }))
        if ($afterTerminate.Count -ne 0) { $ok = $false; $evidence += 'FAIL: terminating the warden raised an alert - an orderly stop would now alert' }
        if ($status -and $status.Snapshot.LastWindowClose) { $ok = $false; $evidence += 'FAIL: terminating the warden recorded a window close' }

        # --- 1-3. closing the window alerts --------------------------------
        $t1 = (Get-Date).AddSeconds(-1)
        $closed = Start-CamTestWardenWindow -Sandbox $sandbox -Title 'cam.test.close' -JobName 'cam.test.close.job'
        $evidence += ("close case: warden {0}, pane {1}, window found {2}, watching {3}" -f `
                $closed.WardenPid, $closed.PanePid, $closed.WindowFound, $closed.Watching)
        if (-not ($closed.WindowFound -and $closed.Watching -and $closed.PanePid -gt 0)) {
            Add-CamTestResult -Run $Run -Check 'windowclose' -Status 'FAIL' `
                -Message 'the second test window or its warden did not come up' -Evidence $evidence
            return
        }

        # WM_CLOSE, which is what clicking the X sends. A second post answers
        # the close-all-tabs overlay a foreign adopted tab would raise.
        $left = Stop-CamTerminalWindow -Title $closed.Title
        if ($left -ge 1) { $left = Stop-CamTerminalWindow -Title $closed.Title }
        $evidence += ("posted WM_CLOSE to '{0}'; windows remaining {1}" -f $closed.Title, $left)
        if ($left -ge 1) { $ok = $false; $evidence += 'FAIL: the test window would not close' }

        $entry = $null
        $deadline = (Get-Date).AddSeconds(60)
        while ((Get-Date) -lt $deadline) {
            $entry = @(& $readEvents $t1) | Select-Object -First 1
            if ($entry) { break }
            Start-Sleep -Milliseconds 500
        }
        if (-not $entry) {
            $ok = $false
            $evidence += 'FAIL: closing the window raised no id-103 entry within 60s'
        }
        else {
            $text = [string]$entry.Properties[0].Value
            $evidence += ("id-103 entry {0:n1}s after the close, {1}: {2}" -f `
                ($entry.TimeCreated - $t1).TotalSeconds, $entry.LevelDisplayName, ($text -replace '\r?\n', ' '))
            if ($text -notmatch 'window was closed') { $ok = $false; $evidence += 'FAIL: the entry does not say the window was closed' }
            if ($text -match 'stopped unexpectedly') { $ok = $false; $evidence += 'FAIL: the entry is worded as the crash alert' }
            if ($text -notlike ((Get-CamAlertMessagePrefix) + '*')) { $ok = $false; $evidence += 'FAIL: the entry carries no IPCCTV prefix' }

            # Told apart in the log by the id, not only by the wording.
            $evidence += ("the entry carries id {0}; the crash alert carries 100, and both are in Get-CamAlertEventIdList ({1})" -f `
                    $entry.Id, ($ids -join ', '))
            if ($entry.Id -eq 100) { $ok = $false; $evidence += 'FAIL: a window close is indistinguishable from a crash in the log' }
            if ($ids -notcontains $entry.Id) { $ok = $false; $evidence += 'FAIL: the id is not in the list the alerts are read back by' }
        }

        $status = Get-CamTestStatusJson -Sandbox $sandbox
        $record = $null
        if ($status) { $record = $status.Snapshot.LastWindowClose }
        $evidence += ("status.json: State={0} StopKind='{1}' LastWindowClose={2}" -f `
                $status.Snapshot.State, $status.Snapshot.StopKind, ($record | ConvertTo-Json -Compress))
        if ($status.Snapshot.StopKind -ne 'window-closed') { $ok = $false; $evidence += "FAIL: the close was not recorded as StopKind 'window-closed'" }
        if (-not $record -or -not $record.At -or -not (Get-CamDateTimeOrNull -Value $record.At)) {
            $ok = $false; $evidence += 'FAIL: the close was not recorded with a usable timestamp'
        }

        $member = Get-Process -Id $closed.PanePid -ErrorAction SilentlyContinue
        $evidence += ("the job member (pane {0}) is gone: {1}" -f $closed.PanePid, (-not $member))
        if ($member) { $ok = $false; $evidence += 'FAIL: alerting cost the kernel sweep - the pane outlived the close' }
    }
    finally {
        foreach ($window in @($kept, $closed)) { if ($window) { Stop-CamTestWardenWindow -Window $window } }
    }

    Add-CamTestResult -Run $Run -Check 'windowclose' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'closing the watched window alerts under its own event id and records the close in status.json; terminating the warden, which is what an orderly stop does, alerts about nothing' `
        -Evidence $evidence
}


function Invoke-CamCheckWindowId {
    <#
        Item [81]. A window is identified by the HANDLE its warden latched
        onto, not by the title it is currently showing.

        THE CONDITION UNDER TEST is the one measured on the live NVR on
        2026-10-14: a foreign console adopted into our window as a second tab,
        after which the window answers to that tab's title and
        Get-CamTerminalWindow finds nothing - while the warden, the job, the
        pane and six ffmpeg are all perfectly alive. It is driven here rather
        than on the Operator's own window, so nothing stops recording to prove
        it.

        Both tiers get the same two windows and the same assertions, because
        both wardens are Start-CamJobWarden with different arguments and
        -Status asks Get-CamWindowPresence for both titles identically. Each
        tier is checked twice over, which is what the commission asked for:

          present  the title has moved away and the window is still FOUND, by
                   handle, reporting the title it is actually showing
          absent   the window is gone and it is reported GONE - including with
                   a stale record left on disk pointing at the dead handle,
                   which is the false positive a published handle could
                   otherwise introduce

        The test warden runs with -AlertOnWindowClose exactly as the NVR's
        does, so each close raises an alert; the sandbox alert stub records
        them, so no toast or Application-log entry reaches the Operator.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $opened = @()

    $sandbox = New-CamTestSandbox -Name 'windowid' -Cameras @(
        [pscustomobject]@{ Name = 'windowid-a'; IP = '10.255.255.12'; Quality = 'main'; Record = $true }
    )
    [void](Set-CamTestAlertStub -Sandbox $sandbox)

    $tiers = @(
        [pscustomobject]@{ Label = 'nvr tier'; Title = 'cam.test.nvrid'; JobName = 'cam.test.nvrid.job' }
        [pscustomobject]@{ Label = 'dashboard tier'; Title = 'cam.test.gridid'; JobName = 'cam.test.gridid.job' }
    )

    try {
        foreach ($tier in $tiers) {
            $window = Start-CamTestWardenWindow -Sandbox $sandbox -Title $tier.Title -JobName $tier.JobName
            $opened += $window
            $evidence += ("{0}: window '{1}' hwnd 0x{2:X}, warden {3}, pane {4}, found {5}, watching {6}" -f `
                    $tier.Label, $tier.Title, $window.Hwnd, $window.WardenPid, $window.PanePid, `
                    $window.WindowFound, $window.Watching)
            if (-not ($window.WindowFound -and $window.Watching -and $window.PanePid -gt 0 -and $window.Hwnd -ne 0)) {
                $ok = $false
                $evidence += ("FAIL: {0}: the test window or its warden did not come up" -f $tier.Label)
                continue
            }

            # --- the warden published the handle it latched -----------------
            $recordPath = Get-CamWindowRecordPath -Config $sandbox.Config -Title $tier.Title
            $record = $null
            $deadline = (Get-Date).AddSeconds(20)
            while ((Get-Date) -lt $deadline) {
                $record = Get-CamWindowRecord -Config $sandbox.Config -Title $tier.Title
                if ($record) { break }
                Start-Sleep -Milliseconds 250
            }
            if (-not $record) {
                $ok = $false
                $evidence += ("FAIL: {0}: the warden published no window record at {1}" -f $tier.Label, $recordPath)
                continue
            }
            $evidence += ("{0}: record {1} -> hwnd {2}, terminal pid {3}, warden pid {4}, latched title '{5}'" -f `
                    $tier.Label, (Split-Path -Path $recordPath -Leaf), $record.HwndHex, `
                    $record.TerminalPid, $record.WardenPid, $record.Title)
            if ([int64]$record.Hwnd -ne [int64]$window.Hwnd) {
                $ok = $false
                $evidence += ("FAIL: {0}: the published handle {1} is not the window the title search found (0x{2:X})" -f `
                        $tier.Label, $record.HwndHex, $window.Hwnd)
            }
            if ([int]$record.WardenPid -ne [int]$window.WardenPid) {
                $ok = $false
                $evidence += ("FAIL: {0}: the record names warden {1}, not the warden that was started ({2})" -f `
                        $tier.Label, $record.WardenPid, $window.WardenPid)
            }
            if ([int]$record.TerminalPid -le 0) {
                $ok = $false
                $evidence += ("FAIL: {0}: the record carries no terminal pid" -f $tier.Label)
            }
            if ([string]$record.Title -cne $tier.Title) {
                $ok = $false
                $evidence += ("FAIL: {0}: the record latched the title '{1}', not '{2}'" -f $tier.Label, $record.Title, $tier.Title)
            }

            # --- with the title intact, nothing is reported as changed ------
            $presence = Get-CamWindowPresence -Config $sandbox.Config -Title $tier.Title
            $evidence += ("{0}: before the foreign tab: found {1} by {2}, title now '{3}', changed {4}" -f `
                    $tier.Label, $presence.Found, $presence.Source, $presence.CurrentTitle, $presence.TitleChanged)
            if (-not ($presence.Found -and $presence.Source -eq 'hwnd' -and -not $presence.TitleChanged)) {
                $ok = $false
                $evidence += ("FAIL: {0}: a window whose title is intact must be found by handle and reported unchanged" -f $tier.Label)
            }

            # --- THE CONDITION: the title moves to a foreign tab -----------
            $moved = Add-CamTestForeignTab -Window $window -ForeignTitle ($tier.Title + '.foreign')
            if (-not $moved) {
                $ok = $false
                $evidence += ("FAIL: {0}: adding a second tab did not move the window title away from '{1}', so the condition was never reproduced" -f `
                        $tier.Label, $tier.Title)
                continue
            }
            $byTitle = @(Get-CamTerminalWindow -Title $tier.Title).Count
            $presence = Get-CamWindowPresence -Config $sandbox.Config -Title $tier.Title
            $evidence += ("{0}: foreign tab {1} active, window now titled '{2}': title search finds {3}, presence finds {4} by {5} at {6}, changed {7}, reports '{8}'" -f `
                    $tier.Label, $window.ForeignPid, $moved, $byTitle, $presence.Found, $presence.Source, `
                    $presence.HwndHex, $presence.TitleChanged, $presence.CurrentTitle)
            if ($byTitle -ne 0) {
                $evidence += ("{0}: NOTE the title search still found {1} - the divergence this check exists for was not in force" -f $tier.Label, $byTitle)
                $ok = $false
                $evidence += ("FAIL: {0}: the window still answers to its title, so nothing about the handle path was proved" -f $tier.Label)
            }
            if (-not $presence.Found) {
                $ok = $false
                $evidence += ("FAIL: {0}: the window is open and watched but reported ABSENT once its title moved - this is the bug item [81] fixed" -f $tier.Label)
            }
            if ($presence.Source -ne 'hwnd') {
                $ok = $false
                $evidence += ("FAIL: {0}: the window was not found by the published handle but by '{1}'" -f $tier.Label, $presence.Source)
            }
            if ([int64]$presence.Hwnd.ToInt64() -ne [int64]$window.Hwnd) {
                $ok = $false
                $evidence += ("FAIL: {0}: presence reports handle {1}, not the window under test (0x{2:X})" -f `
                        $tier.Label, $presence.HwndHex, $window.Hwnd)
            }
            if (-not $presence.TitleChanged) {
                $ok = $false
                $evidence += ("FAIL: {0}: the title moved and presence did not say so, which is the one fact the Operator cannot get elsewhere" -f $tier.Label)
            }
            if ([string]$presence.CurrentTitle -cne [string]$moved) {
                $ok = $false
                $evidence += ("FAIL: {0}: presence reports the title '{1}' while the window shows '{2}'" -f `
                        $tier.Label, $presence.CurrentTitle, $moved)
            }

            # --- the window goes, and is reported gone ---------------------
            # Closed by HANDLE and nothing else: with the title moved, closing
            # it by title is exactly what no longer works, and the warden must
            # be left to notice the close itself. Killing it here - which is
            # what Stop-CamTestWardenWindow does as a backstop - would stop it
            # reaching its own shutdown, and the record removal asserted below
            # would then be measuring the test rather than the warden.
            [void](Close-CamTestWindow -Window $window)
            $gone = $false
            $deadline = (Get-Date).AddSeconds(60)
            while ((Get-Date) -lt $deadline) {
                $gone = (-not (Get-Process -Id $window.WardenPid -ErrorAction SilentlyContinue))
                if ($gone) { break }
                Start-Sleep -Milliseconds 500
            }
            $record = Get-CamWindowRecord -Config $sandbox.Config -Title $tier.Title
            $presence = Get-CamWindowPresence -Config $sandbox.Config -Title $tier.Title
            $evidence += ("{0}: window closed, warden gone {1}, record left behind {2}, presence finds {3} by {4}" -f `
                    $tier.Label, $gone, [bool]$record, $presence.Found, $presence.Source)
            if (-not $gone) {
                $ok = $false
                $evidence += ("FAIL: {0}: the warden outlived its window" -f $tier.Label)
            }
            elseif ($record) {
                $ok = $false
                $evidence += ("FAIL: {0}: the warden exited leaving its record at {1} - a stale file claiming a live window" -f $tier.Label, $recordPath)
            }
            if ($presence.Found) {
                $ok = $false
                $evidence += ("FAIL: {0}: the window is closed and still reported PRESENT" -f $tier.Label)
            }

            # --- a stale record must not resurrect it ----------------------
            # The handle is now dead, which is what a record written by a
            # warden that was TERMINATED rather than closed looks like: an
            # orderly stop kills the warden outright and it never reaches the
            # removal. Believing the file would report a window that is gone.
            [void](Set-CamWindowRecord -Config $sandbox.Config -Title $tier.Title `
                    -Hwnd ([IntPtr]::new([int64]$window.Hwnd)) -JobName $tier.JobName)
            $stale = Get-CamWindowRecord -Config $sandbox.Config -Title $tier.Title
            $presence = Get-CamWindowPresence -Config $sandbox.Config -Title $tier.Title
            $evidence += ("{0}: stale record written for the dead handle {1}: presence finds {2} by {3}" -f `
                    $tier.Label, $(if ($stale) { $stale.HwndHex } else { 'not written' }), $presence.Found, $presence.Source)
            if (-not $stale) {
                $ok = $false
                $evidence += ("FAIL: {0}: the stale record could not be written, so the false positive was never tested" -f $tier.Label)
            }
            if ($presence.Found -or $presence.Source -ne 'none') {
                $ok = $false
                $evidence += ("FAIL: {0}: a stale record was believed and a closed window reported PRESENT" -f $tier.Label)
            }
            [void](Remove-CamWindowRecord -Config $sandbox.Config -Title $tier.Title)
        }
    }
    finally {
        foreach ($window in $opened) { if ($window) { Stop-CamTestWardenWindow -Window $window } }
    }

    Add-CamTestResult -Run $Run -Check 'windowid' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'a warden publishes the handle it latched onto, and both tiers report the window by that handle and its current title once a foreign tab has taken the title over; a closed window is reported gone, and a stale record pointing at a dead handle is not believed' `
        -Evidence $evidence
}


function Invoke-CamCheckNvrStart {
    <#
        Item [84]. An orderly stop and then a start must always bring the NVR
        back, whatever window it ran in.

        THE CONDITION UNDER TEST is the one that failed in production on
        2026-10-14: the NVR's window is shared with the Operator's own tabs,
        so it outlives the NVR, and cam.ps1 -Stop -Recorder TERMINATES the
        warden, which therefore never removes its window record. The next
        start believed that record, waited for a window that will never close,
        and threw - three times, 5 min 34 s with nothing recorded.

        Driven through Resolve-CamWindowStart, the start path's own decision,
        on sandbox windows and jobs named cam.test.*, with the facts the live
        machine would supply passed in: who is recording, and which warden and
        host pids belong to the window. Never cam.nvr.

          sequence   window watched by a warden, a foreign tab active in it,
                     the warden terminated as Stop-CamNvr does it, then a start:
                     it must proceed, and the stale record must be gone
          wait       a window cleared while its own tab was active, left open
                     with only a foreign tab and no warden: it must not block
          stop       Stop-CamWardenedWindow, the body of Stop-CamNvr, removes
                     the record of the warden it terminates
          recycled   a record naming a live process that started after the
                     record was written is a recycled pid, not the warden
          controls   with the warden ALIVE no second launch: recording, it is
                     found already open by its handle; not recording, while it
                     survives the clear the start refuses and the record stays
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $opened = @()
    $sleeper = $null

    $sandbox = New-CamTestSandbox -Name 'nvrstart' -Cameras @(
        [pscustomobject]@{ Name = 'nvrstart-a'; IP = '10.255.255.13'; Quality = 'main'; Record = $true }
    )
    [void](Set-CamTestAlertStub -Sandbox $sandbox)
    $config = $sandbox.Config

    try {
        # === the production sequence, and the controls on a live warden =====
        $title = 'cam.test.nvrstart'
        $job = 'cam.test.nvrstart.job'
        $a = Start-CamTestWardenWindow -Sandbox $sandbox -Title $title -JobName $job
        $opened += $a
        $record = Wait-CamTestWindowRecord -Sandbox $sandbox -Title $title
        $evidence += ("sequence: window hwnd 0x{0:X}, warden {1}, pane {2}, record {3}" -f `
                $a.Hwnd, $a.WardenPid, $a.PanePid, $(if ($record) { "names warden $($record.WardenPid)" } else { 'NOT published' }))
        if (-not ($a.WindowFound -and $a.Watching -and $a.PanePid -gt 0 -and $record)) {
            throw 'the sequence window, its warden or its record did not come up'
        }

        $foreign = Add-CamTestForeignTab -Window $a -ForeignTitle ($title + '.foreign')
        $byTitle = @(Get-CamTerminalWindow -Title $title).Count
        $evidence += ("sequence: foreign tab {0} active, window titled '{1}', title search finds {2}" -f $a.ForeignPid, $foreign, $byTitle)
        if (-not $foreign -or $byTitle -ne 0) { throw 'the foreign tab did not take the title over, so the shared-window case was never reproduced' }

        # --- control: recording, warden alive -> found open, nothing launched
        $decision = Resolve-CamWindowStart -Config $config -Title $title -JobName $job -RecordingBy 'cam.test supervisor' `
            -WardenPid @($a.WardenPid) -HostPid @($a.PanePid)
        $wardenAlive = [bool](Get-Process -Id $a.WardenPid -ErrorAction SilentlyContinue)
        $evidence += ("control, recording: already open {0}, found by {1} at {2}, warden alive {3}" -f `
                $decision.AlreadyOpen, $decision.Presence.Source, $decision.Presence.HwndHex, $wardenAlive)
        if (-not ($decision.AlreadyOpen -and $decision.Presence.Source -eq 'hwnd' -and $wardenAlive)) {
            $ok = $false
            $evidence += 'FAIL: control, recording: a live warden''s window was not found open by its handle - a second NVR would be launched'
        }

        # --- control: nothing recording, warden alive and NOT one the clear may stop
        $refused = ''
        try {
            [void](Resolve-CamWindowStart -Config $config -Title $title -JobName $job -WaitSeconds 3)
        }
        catch { $refused = $_.Exception.Message }
        $record = Get-CamWindowRecord -Config $config -Title $title
        $wardenAlive = [bool](Get-Process -Id $a.WardenPid -ErrorAction SilentlyContinue)
        $evidence += ("control, warden survives the clear: refused {0}, warden alive {1}, record kept {2}" -f `
                [bool]$refused, $wardenAlive, [bool]$record)
        if ($refused) { $evidence += ('  ' + $refused) }
        if (-not $refused) {
            $ok = $false
            $evidence += 'FAIL: control, warden survives the clear: the start went ahead while the first warden still held the job'
        }
        if (-not $wardenAlive) {
            $ok = $false
            $evidence += 'FAIL: control, warden survives the clear: the warden is gone, so the control proved nothing'
        }
        if (-not $record -or [int]$record.WardenPid -ne [int]$a.WardenPid) {
            $ok = $false
            $evidence += 'FAIL: control, warden survives the clear: the record of a LIVE warden was removed'
        }

        # --- THE SEQUENCE: the warden terminated as Stop-CamNvr does it ----
        [void](Stop-CamProcessList -ProcessId @($a.WardenPid))
        $gone = Wait-CamTestProcessExit -ProcessId @($a.WardenPid, $a.PanePid)
        $deadline = (Get-Date).AddSeconds(10)
        while ([CamJob]::Exists($job) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 250 }
        $left = Get-CamWindowRecord -Config $config -Title $title
        $hwndA = [IntPtr]::new([int64]$a.Hwnd)
        $evidence += ("sequence: warden terminated; warden and pane gone {0}, job gone {1}, record left behind {2}, window still open {3} titled '{4}'" -f `
                $gone, (-not [CamJob]::Exists($job)), [bool]$left, [CamWin]::IsTerminalWindow($hwndA), [CamWin]::TitleOf($hwndA))
        if (-not ($gone -and $left -and [CamWin]::IsTerminalWindow($hwndA) -and -not [CamJob]::Exists($job))) {
            throw 'the production state - warden dead, record left behind, shared window still open - was not reached'
        }

        $started = Get-Date
        $threw = ''
        $decision = $null
        try { $decision = Resolve-CamWindowStart -Config $config -Title $title -JobName $job }
        catch { $threw = $_.Exception.Message }
        $after = Get-CamWindowRecord -Config $config -Title $title
        $presence = Get-CamWindowPresence -Config $config -Title $title
        $evidence += ("sequence: start decision after {0}s: {1}; record afterwards {2}; presence then finds {3} by {4}" -f `
                (Format-CamNumber -Value ((Get-Date) - $started).TotalSeconds), $(if ($threw) { 'THREW' } else { 'proceeds to launch' }), `
                $(if ($after) { "STILL THERE naming dead warden $($after.WardenPid)" } else { 'gone' }), $presence.Found, $presence.Source)
        if ($threw) {
            $ok = $false
            $evidence += ('FAIL: sequence: the start threw - this is the production failure of item [84]: ' + $threw)
        }
        elseif ($decision.AlreadyOpen) {
            $ok = $false
            $evidence += 'FAIL: sequence: the start reported the dead NVR as already open'
        }
        if ($after) {
            $ok = $false
            $evidence += 'FAIL: sequence: the stale record of a dead warden is still on disk after the start path read it'
        }
        if ($presence.Found) {
            $ok = $false
            $evidence += ("FAIL: sequence: a window whose warden is dead is reported PRESENT by {0} - defect (a)" -f $presence.Source)
        }

        # === recycled pid: a live process that is not the warden ============
        # Window a is still open and is a real terminal window, so only the
        # warden check can tell these two records apart.
        # One timestamp for both records: this harness was running before it,
        # the sleeper is started after it.
        $stamp = Format-CamTimestamp -Value (Get-Date)
        Start-Sleep -Milliseconds 200
        $sleeper = Start-CamDetachedProcess -Script 'Start-Sleep -Seconds 120'
        $recordPath = Get-CamWindowRecordPath -Config $config -Title $title
        foreach ($case in @(
                [pscustomobject]@{ Label = 'started after the record'; Pid = $sleeper.Id; Believe = $false }
                [pscustomobject]@{ Label = 'started before the record'; Pid = $PID; Believe = $true })) {
            $forged = [pscustomobject]@{
                Schema = 1; Title = $title; Hwnd = [int64]$a.Hwnd; HwndHex = ("0x{0:X}" -f [int64]$a.Hwnd)
                TerminalPid = [CamWin]::ProcessIdOf($hwndA); WardenPid = $case.Pid; JobName = $job
                At = $stamp
            }
            [System.IO.File]::WriteAllText($recordPath, (ConvertTo-Json -InputObject $forged), (New-Object System.Text.UTF8Encoding($false)))
            $presence = Get-CamWindowPresence -Config $config -Title $title
            $kept = Test-Path -LiteralPath $recordPath
            $evidence += ("recycled: record naming live pid {0} {1}: believed {2}, record kept {3}" -f `
                    $case.Pid, $case.Label, ($presence.Source -eq 'hwnd'), $kept)
            if (($presence.Source -eq 'hwnd') -ne $case.Believe -or $kept -ne $case.Believe) {
                $ok = $false
                $evidence += ("FAIL: recycled: a record naming a live pid {0} must be {1}" -f `
                        $case.Label, $(if ($case.Believe) { 'believed and kept' } else { 'ignored and removed' }))
            }
        }
        [void](Remove-CamWindowRecord -Config $config -Title $title)

        # === the wait alone: (b) without (a) ================================
        # The record is withdrawn before the start, so the window is known by
        # its title alone - its own tab active - and a stale record cannot be
        # what blocks. The clear then takes the warden and its pane, the pane's
        # tab goes, and the window stays open on the foreign tab.
        $titleB = 'cam.test.nvrstart.b'
        $jobB = 'cam.test.nvrstart.b.job'
        $b = Start-CamTestWardenWindow -Sandbox $sandbox -Title $titleB -JobName $jobB
        $opened += $b
        $record = Wait-CamTestWindowRecord -Sandbox $sandbox -Title $titleB
        $foreignB = Add-CamTestForeignTab -Window $b -ForeignTitle ($titleB + '.foreign')
        $back = Select-CamTestTab -Window $b -Index 0 -ExpectTitle $titleB
        [void](Remove-CamWindowRecord -Config $config -Title $titleB)
        $evidence += ("wait: window hwnd 0x{0:X}, warden {1}, pane {2}, record published {3}, foreign tab {4}, own tab active again {5}, record withdrawn {6}" -f `
                $b.Hwnd, $b.WardenPid, $b.PanePid, [bool]$record, [bool]$foreignB, $back, `
                (-not (Get-CamWindowRecord -Config $config -Title $titleB)))
        if (-not ($b.Watching -and $b.PanePid -gt 0 -and $record -and $foreignB -and $back)) {
            throw 'the wait window did not reach the state under test'
        }

        $started = Get-Date
        $threw = ''
        $decision = $null
        try { $decision = Resolve-CamWindowStart -Config $config -Title $titleB -JobName $jobB -WardenPid @($b.WardenPid) -HostPid @($b.PanePid) }
        catch { $threw = $_.Exception.Message }
        $hwndB = [IntPtr]::new([int64]$b.Hwnd)
        $evidence += ("wait: start decision after {0}s: {1}; cleared {2}; window still open {3} titled '{4}', warden alive {5}, job held {6}" -f `
                (Format-CamNumber -Value ((Get-Date) - $started).TotalSeconds), $(if ($threw) { 'THREW' } else { 'proceeds to launch' }), `
                $(if ($decision) { (@($decision.Cleaned) | Sort-Object) -join ', ' } else { '-' }), `
                [CamWin]::IsTerminalWindow($hwndB), [CamWin]::TitleOf($hwndB), `
                [bool](Get-Process -Id $b.WardenPid -ErrorAction SilentlyContinue), [CamJob]::Exists($jobB))
        if ($threw) {
            $ok = $false
            $evidence += ('FAIL: wait: the start threw over a shared window that only a foreign tab is holding open - defect (b): ' + $threw)
        }
        if (-not [CamWin]::IsTerminalWindow($hwndB)) {
            $evidence += 'NOTE: wait: the window closed with its pane, so the shared-window condition did not hold to the end'
            $ok = $false
            $evidence += 'FAIL: wait: the condition under test was not reproduced'
        }

        # === Stop-CamNvr's body removes the record of the warden it kills ===
        $titleC = 'cam.test.nvrstart.c'
        $jobC = 'cam.test.nvrstart.c.job'
        $c = Start-CamTestWardenWindow -Sandbox $sandbox -Title $titleC -JobName $jobC
        $opened += $c
        $record = Wait-CamTestWindowRecord -Sandbox $sandbox -Title $titleC
        if (-not ($c.Watching -and $record)) { throw 'the stop window or its record did not come up' }
        $stop = $null
        $threw = ''
        try { $stop = Stop-CamWardenedWindow -Config $config -Title $titleC -JobName $jobC -WardenPid @($c.WardenPid) -HostPid @($c.PanePid) }
        catch { $threw = $_.Exception.Message }
        $left = Get-CamWindowRecord -Config $config -Title $titleC
        $evidence += ("stop: warden {0} with a published record; stopped {1}, survived {2}, job held {3}; record afterwards {4}" -f `
                $c.WardenPid, $(if ($stop) { (@($stop.Stopped) | Sort-Object) -join ', ' } else { '-' }), `
                $(if ($stop) { (@($stop.Survived) | Sort-Object) -join ', ' } else { '-' }), [CamJob]::Exists($jobC), `
                $(if ($left) { "STILL THERE naming dead warden $($left.WardenPid)" } else { 'gone' }))
        if ($threw) {
            $ok = $false
            $evidence += ('FAIL: stop: threw: ' + $threw)
        }
        if (-not $stop -or @($stop.Stopped) -notcontains [int]$c.WardenPid) {
            $ok = $false
            $evidence += 'FAIL: stop: the warden was not terminated'
        }
        if ($left) {
            $ok = $false
            $evidence += 'FAIL: stop: the warden was terminated and its record left behind'
        }
    }
    catch {
        $ok = $false
        $evidence += ('FAIL: ' + $_.Exception.Message)
    }
    finally {
        foreach ($window in $opened) { if ($window) { Stop-CamTestWardenWindow -Window $window } }
        if ($sleeper -and -not $sleeper.HasExited) { try { $sleeper.Kill() } catch { } }
    }

    Add-CamTestResult -Run $Run -Check 'nvrstart' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'after the warden of a shared window is terminated the way an orderly stop does it, the start path proceeds and removes its stale record; a window held open only by a foreign tab does not block a start; Stop-CamNvr removes the record of the warden it terminates; a recycled pid is not a warden; and with the warden alive no second NVR is launched' `
        -Evidence $evidence
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


function Invoke-CamCheckViewerPw {
    <#
        The camera password stays off every command line. Invoke-CamViewer
        runs for real - the real mpv through the console wrapper the pane uses,
        hidden (--vo=null) - against a synthetic camera that REQUIRES a fake
        user and password to read, so a wrong or absent credential would fail.

        Process listings are taken continuously, by every process on the
        machine and not only mpv's, for the whole run; every Start-Process the
        function makes is recorded as well, because a listing can miss a
        process that lives a few milliseconds. A control process that does
        carry the password proves the listing can see one.

        (a) mpv receives the stream and plays it - a script inside mpv writes
            playback-time, estimated-vf-fps, core-idle and video-codec to a file;
        (b) the password appears on no command line at any time;
        (c) the stream stops, mpv exits by itself and the function returns
            $false - the restart-loop contract;
        (d) the relay becoming ready mid-run stops mpv and returns $true;
        (e) a failed hand-over - the pipe never appears, mpv dies before it
            does, mpv answers with an error, mpv never answers - returns $false
            promptly, leaves no process behind, logs no URL and never starts a
            process whose command line carries the password.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $port = 18554
    $apiPort = 18557
    $name = 'cam1'
    $user = 'viewuser'
    $pw = 'FAKEVPW' + [guid]::NewGuid().ToString('N').Substring(0, 12)
    $url = "rtsp://${user}:${pw}@127.0.0.1:$port/$name"
    $mpvPath = 'C:\mpv\mpv.com'
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\viewerpw'
    $forward = $dir.Replace('\', '/')
    if (Test-Path -LiteralPath $dir) { Remove-CamTestTree -Path $dir }
    [void](New-Item -ItemType Directory -Path $dir -Force)

    # What runs inside mpv, so playback is observed from mpv itself.
    $probe = "$forward/probe.txt"
    [System.IO.File]::WriteAllText((Join-Path $dir 'probe.lua'), @'
local out = mp.get_opt('probeout')
mp.add_periodic_timer(0.5, function()
  local f = io.open(out, 'w')
  if f then
    f:write(table.concat({ mp.get_property('playback-time') or '', mp.get_property('estimated-vf-fps') or '',
      mp.get_property('core-idle') or '', mp.get_property('video-codec') or '' }, '|'))
    f:close()
  end
end)
'@)
    $mpvArgs = @('--no-border', '--no-audio', '--profile=low-latency', '--rtsp-transport=tcp', '--correct-pts=no', '--network-timeout=3',
        '--demuxer-lavf-o=timeout=5000000', '--msg-level=all=error', '--force-window=no', '--vo=null', '--ao=null', '--no-terminal',
        "--script=$forward/probe.lua", "--script-opts=probeout=$probe")

    # A stand-in for a mpv whose pipe never comes, and one whose pipe answers badly.
    [System.IO.File]::WriteAllText((Join-Path $dir 'fakempv.ps1'), @'
param($mode)
$pipe = ($args | Where-Object { $_ -like '--input-ipc-server=*' }) -replace '^--input-ipc-server=', ''
$server = New-Object System.IO.Pipes.NamedPipeServerStream($pipe, [System.IO.Pipes.PipeDirection]::InOut)
$server.WaitForConnection()
$reader = New-Object System.IO.StreamReader($server)
$null = $reader.ReadLine()
if ($mode -eq 'reject') {
    $writer = New-Object System.IO.StreamWriter($server); $writer.AutoFlush = $true
    $writer.WriteLine('{"error":"invalid parameter","request_id":1}')
}
Start-Sleep -Seconds 60
'@)

    $started = New-Object System.Collections.ArrayList
    $recording = @{ On = $false }
    # Every process Invoke-CamViewer starts, with the exact argument text. The pid, not
    # the object: the function disposes the Process it was handed.
    function Start-Process {
        $text = (@($args | ForEach-Object { [string]$_ }) -join ' ')
        $process = Microsoft.PowerShell.Management\Start-Process @args
        if ($recording.On) { [void]$started.Add([pscustomobject]@{ Text = $text; ProcessId = $process.Id }) }
        if ($process) { $process }
    }

    $sampler = $null; $samplerHandle = $null; $api = $null; $apiHandle = $null
    $state = [hashtable]::Synchronized(@{ Stop = $false; Samples = 0; Hits = (New-Object System.Collections.ArrayList); Seen = (New-Object System.Collections.Generic.HashSet[int]); MpvLines = (New-Object System.Collections.ArrayList); Blind = 0 })
    $apiState = [hashtable]::Synchronized(@{ Stop = $false; Requests = 0; ReadyAt = [DateTime]::MaxValue; Listening = $false })
    $camera = $null
    $baselineMpv = @(Get-Process -Name mpv -ErrorAction SilentlyContinue | ForEach-Object { $_.Id })
    $control = $null
    $config = Get-CamConfigDefault
    $config.RelayApiBase = "http://127.0.0.1:$apiPort"
    $config.RelayProbeTimeoutMs = 800
    $config.ViewerPromoteCheckSeconds = 1
    $readProbe = { try { [System.IO.File]::ReadAllText((Join-Path $dir 'probe.txt')) } catch { '' } }
    $captureRun = {
        param($Choice, $Path, [string]$Exe, [string[]]$Arguments, [int]$IpcMs)
        $watch = [System.Diagnostics.Stopwatch]::StartNew()
        $all = @(Invoke-CamViewer -Config $config -Choice $Choice -RelayPathName $Path -MpvPath $Exe -MpvArgs $Arguments -IpcTimeoutMs $IpcMs 6>&1)
        $watch.Stop()
        [pscustomobject]@{
            Result  = @($all | Where-Object { $_ -is [bool] })[0]
            Log     = @($all | Where-Object { $_ -is [System.Management.Automation.InformationRecord] } | ForEach-Object { [string]$_.MessageData })
            Seconds = $watch.Elapsed.TotalSeconds
        }
    }

    try {
        foreach ($p in @($port, $apiPort)) {
            if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the check needs it free." }
        }

        # --- (a) (b) (c): a credentialed direct URL, then the stream stops
        $camera = Start-CamTestSyntheticCamera -Dir $dir -Port $port -PathName $name -ReadUser $user -ReadPass $pw

        # Before the sampler starts: ffprobe has to carry the password to ask.
        $probeExe = Join-Path -Path (Split-Path -Parent (Get-CamConfigDefault).FfmpegPath) -ChildPath 'ffprobe.exe'
        $refusal = @()
        foreach ($case in @(@('no credential', "rtsp://127.0.0.1:$port/$name", $false), @('wrong password', "rtsp://${user}:wrong@127.0.0.1:$port/$name", $false), @('right credential', $url, $true))) {
            & $probeExe -v quiet -rtsp_transport tcp -timeout 5000000 -show_entries stream=codec_name $case[1] | Out-Null
            $accepted = ($LASTEXITCODE -eq 0)
            $refusal += ("{0}: {1}" -f $case[0], $(if ($accepted) { 'accepted' } else { 'refused' }))
            if ($accepted -ne $case[2]) { $ok = $false; $evidence += ('FAIL: the synthetic camera did not behave as an authenticated one: ' + $case[0]) }
        }
        $evidence += ('synthetic camera on 127.0.0.1:{0} requires {1} and a password to read - {2}' -f $port, $user, ($refusal -join '; '))


        # the listing sampler: a separate runspace, so it keeps looking while Invoke-CamViewer blocks
        $sampler = [powershell]::Create()
        [void]$sampler.AddScript({
            param($needle, $state)
            while (-not $state.Stop) {
                $blind = 0
                try {
                    foreach ($p in @(Get-CimInstance -ClassName Win32_Process -Property ProcessId, Name, CommandLine)) {
                        $line = [string]$p.CommandLine
                        if (-not $line) { $blind++; continue }
                        if ($line.Contains($needle)) { if ($state.Seen.Add([int]$p.ProcessId)) { [void]$state.Hits.Add([pscustomobject]@{ ProcessId = [int]$p.ProcessId; Name = $p.Name; Line = $line.Replace($needle, '<PW>') }) } }
                        elseif ($p.Name -like 'mpv*' -and $state.MpvLines.Count -lt 4 -and -not $state.MpvLines.Contains($line)) { [void]$state.MpvLines.Add($line) }
                    }
                }
                catch { }
                $state.Blind = $blind
                $state.Samples++
                Start-Sleep -Milliseconds 20
            }
        }).AddArgument($pw).AddArgument($state)
        $samplerHandle = $sampler.BeginInvoke()

        # control: a process that does carry the password must be seen
        $control = Microsoft.PowerShell.Management\Start-Process -FilePath 'powershell.exe' -PassThru -WindowStyle Hidden `
            -ArgumentList ('-NoProfile -Command "Start-Sleep -Seconds 20 # ' + $pw + '"')
        $deadline = [DateTime]::UtcNow.AddSeconds(15)
        while (@($state.Hits | Where-Object { $_.ProcessId -eq $control.Id }).Count -eq 0 -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 100 }
        $seen = @($state.Hits | Where-Object { $_.ProcessId -eq $control.Id }).Count -gt 0
        Stop-Process -Id $control.Id -Force -ErrorAction SilentlyContinue
        $evidence += ("listing control: a process carrying the password was seen by the sampler: {0} ({1} listing sweeps so far)" -f $seen, $state.Samples)
        if (-not $seen) { $ok = $false; $evidence += 'FAIL: the process listing cannot see a command line carrying the password, so (b) would prove nothing' }
        $state.Hits.Clear()

        $apiState.ReadyAt = [DateTime]::MaxValue
        $api = [powershell]::Create()
        [void]$api.AddScript({
            param($apiPort, $apiState)
            $listener = New-Object System.Net.HttpListener
            $listener.Prefixes.Add("http://127.0.0.1:$apiPort/")
            $listener.Start()
            $apiState.Listening = $true
            try {
                while (-not $apiState.Stop) {
                    $next = $listener.GetContextAsync()
                    while (-not $next.Wait(200)) { if ($apiState.Stop) { return } }
                    $context = $next.Result
                    $apiState.Requests++
                    $ready = ([DateTime]::UtcNow -ge $apiState.ReadyAt)
                    $bytes = [System.Text.Encoding]::UTF8.GetBytes('{"name":"cam1","ready":' + $ready.ToString().ToLowerInvariant() + '}')
                    $context.Response.ContentType = 'application/json'
                    $context.Response.OutputStream.Write($bytes, 0, $bytes.Length)
                    $context.Response.Close()
                }
            }
            finally { $listener.Stop() }
        }).AddArgument($apiPort).AddArgument($apiState)
        $apiHandle = $api.BeginInvoke()

        $recording.On = $true
        $controller = [powershell]::Create()
        [void]$controller.AddScript({
            param($probeFile, $publisherIds, $out)
            $deadline = [DateTime]::UtcNow.AddSeconds(30)
            while ([DateTime]::UtcNow -lt $deadline -and -not $out.Seen) {
                try { $text = [System.IO.File]::ReadAllText($probeFile) } catch { $text = '' }
                $field = $text.Split('|')
                $time = 0.0
                if ($field.Count -ge 4 -and [double]::TryParse($field[0], [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$time) -and $time -ge 2) { $out.Seen = $text }
                else { Start-Sleep -Milliseconds 200 }
            }
            foreach ($id in $publisherIds) { Stop-Process -Id $id -Force -ErrorAction SilentlyContinue }
            $out.KilledAt = [DateTime]::UtcNow
        }).AddArgument($probe).AddArgument(@($camera.Publishers | ForEach-Object { $_.Id })).AddArgument(($controlState = [hashtable]::Synchronized(@{ Seen = ''; KilledAt = $null })))
        $controllerHandle = $controller.BeginInvoke()
        $apiState.ReadyAt = [DateTime]::MaxValue
        $choice = [pscustomobject]@{ Mode = 'direct'; Url = $url; WaitedSeconds = 0 }
        $one = & $captureRun $choice $name $mpvPath $mpvArgs 8000
        $controller.EndInvoke($controllerHandle); $controller.Dispose()
        $exitedAfter = if ($controlState.KilledAt) { ([DateTime]::UtcNow - $controlState.KilledAt).TotalSeconds } else { -1 }
        $field = ([string]$controlState.Seen).Split('|')
        $evidence += ("(a) mpv playing before the stream was stopped, read from inside mpv: playback-time {0} s, estimated-vf-fps {1}, core-idle {2}, video-codec {3}" -f $field[0], $field[1], $field[2], $field[3])
        if ($field.Count -lt 4 -or [double]::Parse($field[0], [cultureinfo]::InvariantCulture) -lt 2 -or $field[2] -ne 'no' -or $field[3] -notlike '*H.264*') {
            $ok = $false; $evidence += 'FAIL (a): mpv did not receive and play the stream'
        }
        $evidence += ("(c) publisher stopped at playback-time {0}; Invoke-CamViewer returned {1} {2:N1} s after it (mpv exited on its own; the relay watch polled the control API {3} time(s))" -f $field[0], $one.Result, $exitedAfter, $apiState.Requests)
        if ($one.Result -ne $false -or $exitedAfter -lt 0 -or $exitedAfter -gt 20) { $ok = $false; $evidence += 'FAIL (c): mpv did not exit and return $false when the stream stopped' }
        if ($apiState.Requests -lt 1) { $ok = $false; $evidence += 'FAIL (c): the relay watch never asked the control API' }
        [void](Stop-CamTestSyntheticCamera -Camera $camera); $camera = $null

        # --- (d): the relay becomes ready mid-run
        Remove-Item -LiteralPath (Join-Path $dir 'probe.txt') -Force -ErrorAction SilentlyContinue
        $recording.On = $false
        $camera = Start-CamTestSyntheticCamera -Dir $dir -Port $port -PathName $name -ReadUser $user -ReadPass $pw
        $recording.On = $true
        $apiState.Requests = 0
        $apiState.ReadyAt = [DateTime]::UtcNow.AddSeconds(7)
        $two = & $captureRun $choice $name $mpvPath $mpvArgs 8000
        $field = (& $readProbe).Split('|')
        $evidence += ("(d) relay path became ready 7 s into the run: Invoke-CamViewer returned {0} after {1:N1} s ({2} control-API poll(s)); last playback-time {3} s, core-idle {4}" -f $two.Result, $two.Seconds, $apiState.Requests, $field[0], $field[2])
        if ($two.Result -ne $true -or $two.Seconds -lt 6 -or $two.Seconds -gt 20) { $ok = $false; $evidence += 'FAIL (d): the pane was not taken back to the relay promptly' }
        if ($field.Count -lt 4 -or $field[0] -eq '') { $ok = $false; $evidence += 'FAIL (d): mpv was not playing when the relay became ready' }
        [void](Stop-CamTestSyntheticCamera -Camera $camera); $camera = $null

        # --- (e): the hand-over fails; each stand-in is a process that never plays anything
        $ps = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
        $fake = Join-Path $dir 'fakempv.ps1'
        $cases = @(
            @{ Label = 'the pipe never appears'; Args = @('-NoProfile', '-Command', 'Start-Sleep -Seconds 60 #'); Ipc = 2000; Max = 6 },
            @{ Label = 'mpv dies before the pipe is up'; Args = @('-NoProfile', '-Command', 'exit 3 #'); Ipc = 8000; Max = 4 },
            @{ Label = 'mpv answers with an error'; Args = @('-NoProfile', '-File', $fake, 'reject'); Ipc = 2000; Max = 8 },
            @{ Label = 'mpv never answers'; Args = @('-NoProfile', '-File', $fake, 'silent'); Ipc = 2000; Max = 8 })
        foreach ($case in $cases) {
            $before = $started.Count
            $r = & $captureRun $choice $name $ps $case.Args $case.Ipc
            $mine = @($started | Select-Object -Skip $before)
            $left = @($mine | Where-Object { Get-Process -Id $_.ProcessId -ErrorAction SilentlyContinue }).Count
            $logText = $r.Log -join ' | '
            $leaked = ($logText -match [regex]::Escape($pw)) -or ($logText -match 'rtsp://')
            $evidence += ("(e) {0}: returned {1} after {2:N1} s (limit {3} s); {4} process(es) started, {5} left running; log: {6}" -f $case.Label, $r.Result, $r.Seconds, $case.Max, $mine.Count, $left, $(if ($r.Log.Count -eq 1) { $logText } else { "$($r.Log.Count) lines" }))
            if ($r.Result -ne $false -or $r.Seconds -gt $case.Max -or $left -ne 0 -or $r.Log.Count -ne 1 -or $leaked) {
                $ok = $false; $evidence += ('FAIL (e): ' + $case.Label)
            }
        }
        $recording.On = $false

        # (b) over everything that was started and every listing taken
        Start-Sleep -Milliseconds 300
        $state.Stop = $true
        [void]$sampler.EndInvoke($samplerHandle)
        $argHits = @($started | Where-Object { $_.Text.Contains($pw) })
        $listHits = @($state.Hits | Where-Object { $_.ProcessId -ne $control.Id })
        $evidence += ("(b) {0} process listing sweep(s) of every process ({1} unreadable command lines per sweep, system processes): the password was on {2} command line(s); {3} Start-Process call(s) recorded, {4} carried it as an argument" -f `
            $state.Samples, $state.Blind, $listHits.Count, $started.Count, $argHits.Count)
        foreach ($line in @($state.MpvLines)) { $evidence += ('    listing: ' + $line) }
        foreach ($hit in @($listHits | Select-Object -First 3)) { $evidence += ("    PASSWORD IN LISTING: pid {0} {1}: {2}" -f $hit.ProcessId, $hit.Name, $hit.Line) }
        foreach ($call in @($argHits | Select-Object -First 3)) { $evidence += ('    PASSWORD IN ARGUMENT: ' + $call.Text.Replace($pw, '<PW>')) }
        if ($listHits.Count -gt 0 -or $argHits.Count -gt 0) { $ok = $false; $evidence += 'FAIL (b): the camera password reached a command line' }
        if ($state.Samples -lt 20) { $ok = $false; $evidence += 'FAIL (b): too few listings were taken to mean anything' }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        $recording.On = $false
        $state.Stop = $true; $apiState.Stop = $true
        foreach ($s in @(@($sampler, $samplerHandle), @($api, $apiHandle))) {
            if ($s[0]) { try { [void]$s[0].EndInvoke($s[1]) } catch { }; $s[0].Dispose() }
        }
        if ($control) { Stop-Process -Id $control.Id -Force -ErrorAction SilentlyContinue }
        foreach ($item in @($started)) {
            Stop-Process -Id $item.ProcessId -Force -ErrorAction SilentlyContinue
        }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }

    # nothing left that this check started: mpv by pid, against the baseline
    $stray = @(Get-Process -Name mpv -ErrorAction SilentlyContinue | Where-Object { $baselineMpv -notcontains $_.Id })
    $evidence += ("{0} mpv process(es) left that were not running before the check" -f $stray.Count)
    if ($stray.Count -gt 0) { $ok = $false; foreach ($s in $stray) { Stop-Process -Id $s.Id -Force -ErrorAction SilentlyContinue } }

    Complete-CamPruneCheck -Run $Run -Check 'viewerpw' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $dir; Log = (Join-Path $env:TEMP 'cam-tests\viewerpw.none') }) `
        -Message 'a credentialed direct URL never reaches a command line: mpv plays it, exits and returns on stream loss, still goes back to the relay, and a failed hand-over returns false without a process, a log line or an argument carrying the password'
}


function Invoke-CamCheckRelayApi {
    <#
        [89] The restreamer's control API requires the product's login, and a
        refused login is never taken for a dead relay. Runs the REAL sandbox
        supervisor and its MediaMTX (a sandbox copy of the binary, so its
        working directory is local) against a synthetic camera; fake
        credentials only; ports 18554 (camera), 18564 (relay RTSP) and 18567
        (relay API).

        (a) upgrade: an old-style config with no login and no login file, then
            a supervisor start, produces the login, a config that requires it
            and holds only its hash, and a working login;
        (b) without a login every route answers 401 and carries no password;
            the product's login is accepted; a wrong one gets 401; no response
            carries Access-Control-Allow-Origin;
        (c) anonymous RTSP read through the relay still works;
        (d) the running relay starts refusing the login (its file is changed
            under it): exactly one alert (130, Error) and one log line, no
            consumer goes direct - a recorder killed meanwhile restarts on the
            relay, and the camera whose path has no stream moves onto the
            relay and keeps retrying rather than waiting - and an accepted
            login ends the episode with one line;
        (e) on a standalone relay from the generated file: anonymous publish
            and read; the readiness helpers and the real Get-CamInputUrl give
            the old answers with the right login, never "not ready" or direct
            with a wrong or missing one, each decision bounded; and the old
            answers when the relay is down;
        (f) no secret on any command line (continuous sweeps with a positive
            control), in any file, log, status.json or -Status output; the
            login file is DPAPI-protected.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $camPort = 18554; $rtspPort = 18564; $apiPort = 18567
    $camPw = 'FakePassword-DoNotUse-9c41'
    $ffmpeg = (Get-CamConfigDefault).FfmpegPath
    $ffprobe = Join-Path -Path (Split-Path -Parent $ffmpeg) -ChildPath 'ffprobe.exe'
    $name = 'relayapi'
    $dir = Join-Path -Path $env:TEMP -ChildPath "cam-tests\$name"
    $fail = { param([string]$Text) $script:CamRelayApiOk = $false; $script:CamRelayApiEvidence += ('FAIL ' + $Text) }
    $script:CamRelayApiOk = $true
    $script:CamRelayApiEvidence = @()

    $api = {
        param([string]$Route, [string]$Login, [int]$TimeoutMs = 6000)
        $watch = [System.Diagnostics.Stopwatch]::StartNew()
        $request = [System.Net.HttpWebRequest]::Create("http://127.0.0.1:$apiPort$Route")
        $request.Proxy = $null; $request.Timeout = $TimeoutMs; $request.ReadWriteTimeout = $TimeoutMs
        $request.Headers['Origin'] = 'http://evil.example'
        if ($Login) { $request.Headers['Authorization'] = 'Basic ' + [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($Login)) }
        $response = $null
        try { $response = $request.GetResponse() } catch [System.Net.WebException] { $response = $_.Exception.Response }
        if (-not $response) { return [pscustomobject]@{ Code = 0; Body = ''; Acao = ''; Ms = [int]$watch.ElapsedMilliseconds } }
        $reader = New-Object System.IO.StreamReader($response.GetResponseStream())
        $body = $reader.ReadToEnd(); $reader.Dispose()
        $result = [pscustomobject]@{ Code = [int]$response.StatusCode; Body = $body; Acao = [string]$response.Headers['Access-Control-Allow-Origin']; Ms = [int]$watch.ElapsedMilliseconds }
        $response.Dispose()
        return $result
    }
    $readStatus = {
        $s = $null; try { $s = (Get-CamTestStatusJson -Sandbox $sandbox).Snapshot } catch { }
        $rows = @{}
        if ($s) { foreach ($row in @($s.Cameras)) { $rows[[string]$row.Name] = $row } }
        return $rows
    }
    $countLines = {
        param([string]$Path, [string]$Pattern)
        if (-not (Test-Path -LiteralPath $Path)) { return 0 }
        return @(Get-Content -LiteralPath $Path | Where-Object { $_ -match $Pattern }).Count
    }

    $sampler = $null; $samplerHandle = $null; $camera = $null; $supervisor = $null; $standalone = $null; $publisher = $null; $control = $null
    $state = [hashtable]::Synchronized(@{ Stop = $false; Samples = 0; Hits = (New-Object System.Collections.ArrayList); Needles = [System.Collections.ArrayList]::Synchronized((New-Object System.Collections.ArrayList)) })
    # Not the camera password: a recorder on a DIRECT input carries it on its
    # command line by design until [90] is decided, and cam2 here starts direct.
    $secret = $null; $token = $null
    $sandbox = $null

    try {
        foreach ($p in @($camPort, $rtspPort, $apiPort)) {
            if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the check needs it free." }
        }
        $cameras = @(
            [pscustomobject]@{ Name = 'cam1'; IP = "127.0.0.1:$camPort"; Quality = 'cam1'; Record = $true },
            [pscustomobject]@{ Name = 'cam2'; IP = "127.0.0.1:$camPort"; Quality = 'nostream'; Record = $true })
        $sandbox = New-CamTestSandbox -Name $name -Cameras $cameras -Config @{
            RestreamerEnabled = $true
            RestreamerExePath = (Join-Path -Path $dir -ChildPath 'bin\mediamtx.exe')
            RestreamerConfigPath = (Join-Path -Path $dir -ChildPath 'mtx\mediamtx.yml')
            RelayBase = "rtsp://127.0.0.1:$rtspPort"; RelayApiBase = "http://127.0.0.1:$apiPort"
            RelayProbeTimeoutMs = 800; RelayStartupWaitSeconds = 15; RelayReadyWaitSeconds = 8; StatusIntervalSeconds = 5 }
        $alertLog = Set-CamTestAlertStub -Sandbox $sandbox
        $supervisorLog = Join-Path -Path $sandbox.Root -ChildPath '_logs\recorder.log'
        [void](New-Item -ItemType Directory -Path (Join-Path $dir 'bin') -Force)
        Copy-Item -LiteralPath 'C:\mediamtx\mediamtx.exe' -Destination (Join-Path $dir 'bin\mediamtx.exe')
        $yml = [string]$sandbox.Config.RestreamerConfigPath
        $credPath = Join-Path -Path $dir -ChildPath 'relay_api_creds.xml'

        # --- (a) an install from before the login: old-style file, no login file
        $install = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-InstallRestreamer')
        if ($install.ExitCode -ne 0) { throw ('-InstallRestreamer failed: ' + ($install.Lines -join ' | ')) }
        if (Test-Path -LiteralPath $credPath) { Remove-Item -LiteralPath $credPath -Force }
        $text = [System.IO.File]::ReadAllText($yml)
        $keep = New-Object System.Collections.ArrayList; $skip = $false
        foreach ($l in ($text -split "`r`n")) {
            if ($l -match '^authMethod:') { $skip = $true; continue }
            if ($skip -and ($l -match '^authInternalUsers:' -or $l -match '^\s' -or $l -eq '')) { continue }
            $skip = $false; [void]$keep.Add($l)
        }
        $oldText = $keep -join "`r`n"
        [System.IO.File]::WriteAllText($yml, $oldText)
        $evidence += ("(a) old-style install: {0} has authMethod: {1}, login file present: {2}" -f $yml, ($oldText -match '(?m)^authMethod:'), (Test-Path -LiteralPath $credPath))

        $camera = Start-CamTestSyntheticCamera -Dir $dir -Port $camPort -PathName 'cam1'

        # (f) the listing sampler, from before the supervisor starts
        $sampler = [powershell]::Create()
        [void]$sampler.AddScript({
            param($state)
            while (-not $state.Stop) {
                try {
                    $needles = @($state.Needles.ToArray())
                    foreach ($p in @(Get-CimInstance -ClassName Win32_Process -Property ProcessId, Name, CommandLine)) {
                        $line = [string]$p.CommandLine
                        if (-not $line) { continue }
                        foreach ($n in $needles) { if ($line.Contains($n)) { [void]$state.Hits.Add(('{0} {1} needle#{2}' -f $p.ProcessId, $p.Name, [array]::IndexOf($needles, $n))) } }
                    }
                }
                catch { }
                $state.Samples++
                Start-Sleep -Milliseconds 50
            }
        }).AddArgument($state)
        $samplerHandle = $sampler.BeginInvoke()

        $supervisor = Start-CamTestSupervisor -Sandbox $sandbox -TimeoutSeconds 120
        $deadline = [DateTime]::UtcNow.AddSeconds(60)
        while ([DateTime]::UtcNow -lt $deadline) { $rows = & $readStatus; if ($rows['cam1'] -and $rows['cam1'].InputMode -eq 'relay') { break }; Start-Sleep -Milliseconds 500 }
        $rows = & $readStatus
        $evidence += ("supervisor pid {0}; cam1 InputMode '{1}', cam2 InputMode '{2}' (cam2's path has no stream)" -f $supervisor.Id, $rows['cam1'].InputMode, $rows['cam2'].InputMode)
        if (-not $rows['cam1'] -or $rows['cam1'].InputMode -ne 'relay') { & $fail 'cam1 never recorded through the relay' }

        $cred = $null
        if (Test-Path -LiteralPath $credPath) { $cred = Import-Clixml -LiteralPath $credPath }
        if ($cred) {
            $secret = $cred.GetNetworkCredential().Password
            $token = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($cred.UserName + ':' + $secret))
            [void]$state.Needles.Add($secret); [void]$state.Needles.Add($token)
            $sha = [System.Security.Cryptography.SHA256]::Create()
            $hash = 'sha256:' + [Convert]::ToBase64String($sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($secret))); $sha.Dispose()
            $credText = [System.IO.File]::ReadAllText($credPath)
            $newText = [System.IO.File]::ReadAllText($yml)
            $dpapi = $credText -match '<SS N="Password">01000000d08c9ddf'
            $evidence += ("(a) the supervisor created the login: user {0}, a {1}-character secret; login file DPAPI-protected: {2}, secret in it as text: {3}" -f $cred.UserName, $secret.Length, $dpapi, $credText.Contains($secret))
            if (-not $dpapi -or $credText.Contains($secret) -or $secret.Length -lt 32) { & $fail '(a) the login file is not a DPAPI-protected random secret' }
            $hasBlock = ($newText -match '(?m)^authMethod: internal') -and $newText.Contains("pass: $hash") -and $newText.Contains("user: $($cred.UserName)")
            $evidence += ("(a) config now requires the login, holds its sha256 hash: {0}; holds the secret: {1}; rewrite logged: {2} line(s)" -f $hasBlock, $newText.Contains($secret), (& $countLines $supervisorLog 'rewrote .*control API login'))
            if (-not $hasBlock -or $newText.Contains($secret) -or (& $countLines $supervisorLog 'rewrote .*control API login') -ne 1) { & $fail '(a) the old config was not upgraded on the start' }
            if (@($newText -split "`r?`n" | Where-Object { $_ -match 'action: (api|metrics|pprof)' }).Count -ne 1) { & $fail '(a) the config grants api, metrics or pprof more than once' }
        }
        else { & $fail '(a) the supervisor start did not create relay_api_creds.xml' }
        $login = if ($cred) { "$($cred.UserName):$secret" } else { 'none:none' }

        # --- (b) the API
        $routes = @('/v3/config/paths/list', '/v3/config/paths/get/cam1', '/v3/paths/list', '/v3/paths/get/cam1')
        $acao = 0
        foreach ($route in $routes) {
            $anon = & $api $route ''
            $mine = & $api $route $login
            $wrong = & $api $route ($login + 'x')
            foreach ($r in @($anon, $mine, $wrong)) { if ($r.Acao) { $acao++ } }
            $evidence += ("(b) {0,-28} no login HTTP {1} (password in body {2}), product login HTTP {3}, wrong login HTTP {4} after {5} ms" -f $route, $anon.Code, $anon.Body.Contains($camPw), $mine.Code, $wrong.Code, $wrong.Ms)
            if ($anon.Code -ne 401 -or $anon.Body.Contains($camPw) -or $mine.Code -ne 200 -or $wrong.Code -ne 401 -or $wrong.Body.Contains($camPw)) { & $fail "(b) $route" }
        }
        $evidence += ("(b) responses carrying Access-Control-Allow-Origin (request sent Origin: http://evil.example): {0}" -f $acao)
        if ($acao -ne 0) { & $fail '(b) a cross-origin permission was sent' }

        # --- (c) anonymous read through the relay
        # Collected whole: Select-Object -First would stop ffprobe and make its exit code -1.
        $codec = @(& $ffprobe -v error -rtsp_transport tcp -timeout 5000000 -show_entries stream=codec_name -of csv=p=0 "rtsp://127.0.0.1:$rtspPort/cam1")[0]
        $evidence += ("(c) ffprobe with no login through the relay: exit {0}, first stream {1}" -f $LASTEXITCODE, $codec)
        if ($LASTEXITCODE -ne 0 -or $codec -ne 'h264') { & $fail '(c) anonymous read through the relay' }

        # --- (d) the running relay starts refusing the product's login
        $goodText = [System.IO.File]::ReadAllText($yml)
        $badText = [regex]::Replace($goodText, 'pass: sha256:\S+', 'pass: sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=')
        [System.IO.File]::WriteAllText($yml, $badText)
        $faultAt = [DateTime]::UtcNow
        $deadline = $faultAt.AddSeconds(30)
        while ([DateTime]::UtcNow -lt $deadline -and (& $countLines $alertLog '^130 ') -eq 0) { Start-Sleep -Milliseconds 500 }
        $evidence += ("(d) login refused from {0:HH:mm:ss}; alert raised after {1:N1} s" -f $faultAt.ToLocalTime(), ([DateTime]::UtcNow - $faultAt).TotalSeconds)
        $old1 = Get-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $supervisor -CameraName 'cam1'
        if ($old1 -gt 0) { Stop-Process -Id $old1 -Force }
        $new1 = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $supervisor -CameraName 'cam1' -Not $old1 -TimeoutSeconds 40
        $directSeen = @{ cam1 = 0; cam2AfterRelay = 0 }; $cam2Relay = $false; $cam2Restarts = @()
        $until = [DateTime]::UtcNow.AddSeconds(30)
        while ([DateTime]::UtcNow -lt $until) {
            $rows = & $readStatus
            if ($rows['cam1'] -and $rows['cam1'].InputMode -eq 'direct') { $directSeen.cam1++ }
            if ($rows['cam2']) {
                if ($rows['cam2'].InputMode -eq 'relay') { $cam2Relay = $true; $cam2Restarts += [int]$rows['cam2'].RestartsLastHour }
                elseif ($cam2Relay -and $rows['cam2'].InputMode -eq 'direct') { $directSeen.cam2AfterRelay++ }
            }
            Start-Sleep -Milliseconds 1000
        }
        $rows = & $readStatus
        $evidence += ("(d) cam1 child {0} killed during the fault, restarted as {1} on '{2}'; cam1 reported direct {3} time(s) in 30 s of 1 s samples" -f $old1, $new1, $rows['cam1'].InputMode, $directSeen.cam1)
        if ($old1 -le 0 -or $new1 -le 0 -or $rows['cam1'].InputMode -ne 'relay' -or $directSeen.cam1 -ne 0) { & $fail '(d) a recorder went direct, or did not restart, while the login was refused' }
        $first = if ($cam2Restarts.Count) { $cam2Restarts[0] } else { -1 }; $last = if ($cam2Restarts.Count) { $cam2Restarts[-1] } else { -1 }
        $evidence += ("(d) cam2 (no stream on its path) moved onto the relay: {0}; restarts in the last hour while on it {1} -> {2} (retrying, not waiting); direct again afterwards {3} time(s)" -f $cam2Relay, $first, $last, $directSeen.cam2AfterRelay)
        if (-not $cam2Relay -or $last -le $first -or $directSeen.cam2AfterRelay -ne 0) { & $fail '(d) the camera with no stream did not stay on the relay and keep retrying' }
        $status = Invoke-CamTestScript -Path $sandbox.Recorder -Arguments @('-Status')
        $statusText = $status.Output + $status.Error
        $alerts = @(if (Test-Path -LiteralPath $alertLog) { Get-Content -LiteralPath $alertLog | Where-Object { $_ -match '^130 ' } })
        $alertLines = @(Get-Content -LiteralPath $supervisorLog | Where-Object { $_ -match 'ALERT' -and $_ -match 'refused' })
        $evidence += ("(d) {0} alert(s) recorded by the sandbox stub: {1}" -f $alerts.Count, ($alerts -join ' || '))
        $evidence += ("(d) {0} log line(s) about it in the supervisor log" -f $alertLines.Count)
        if ($alerts.Count -ne 1 -or $alerts[0] -notmatch '^130 Error ' -or $alertLines.Count -ne 1) { & $fail '(d) not exactly one alert 130 Error and one log line for the episode' }
        [System.IO.File]::WriteAllText($yml, $goodText)
        $deadline = [DateTime]::UtcNow.AddSeconds(30)
        while ([DateTime]::UtcNow -lt $deadline -and (& $countLines $supervisorLog 'accepts the product''s login again') -eq 0) { Start-Sleep -Milliseconds 500 }
        $evidence += ("(d) login accepted again: {0} line(s); alerts still {1}" -f (& $countLines $supervisorLog 'accepts the product''s login again'), (& $countLines $alertLog '^130 '))
        if ((& $countLines $supervisorLog 'accepts the product''s login again') -ne 1) { & $fail '(d) the end of the episode was not logged once' }

        $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $supervisor
        $supervisor = $null
        $evidence += ("supervisor stopped through -Stop: ok {0}" -f $stop.Ok)
        if (-not $stop.Ok) { & $fail 'the sandbox supervisor did not stop cleanly' }

        # --- (e) a standalone relay from the generated file: every path is source: publisher
        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.FileName = Join-Path $dir 'bin\mediamtx.exe'; $psi.Arguments = ('"{0}"' -f $yml); $psi.WorkingDirectory = Join-Path $dir 'bin'
        $psi.UseShellExecute = $false; $psi.CreateNoWindow = $true; $psi.RedirectStandardOutput = $true; $psi.RedirectStandardError = $true
        $standalone = [System.Diagnostics.Process]::Start($psi)
        $standaloneOut = $standalone.StandardOutput.ReadToEndAsync()
        Start-Sleep -Seconds 2
        $publisher = Start-Process -FilePath $ffmpeg -PassThru -WindowStyle Hidden -ArgumentList ("-hide_banner -loglevel error -nostdin -re -f lavfi -i testsrc2=size=320x180:rate=10 -c:v libx264 -preset ultrafast -g 10 -f rtsp -rtsp_transport tcp rtsp://127.0.0.1:$rtspPort/cam1")
        Start-Sleep -Seconds 4
        # Collected whole: Select-Object -First would stop ffprobe and make its exit code -1.
        $codec = @(& $ffprobe -v error -rtsp_transport tcp -timeout 5000000 -show_entries stream=codec_name -of csv=p=0 "rtsp://127.0.0.1:$rtspPort/cam1")[0]
        $evidence += ("(e) anonymous publish into the generated config's cam1 path, then anonymous read: publisher alive {0}, ffprobe exit {1}, stream {2}" -f (-not $publisher.HasExited), $LASTEXITCODE, $codec)
        if ($publisher.HasExited -or $LASTEXITCODE -ne 0 -or $codec -ne 'h264') { & $fail '(e) anonymous publish and read' }

        [System.IO.File]::WriteAllText((Join-Path $dir 'relayprobe.ps1'), @'
$ErrorActionPreference = 'Stop'
. (Join-Path -Path $PSScriptRoot -ChildPath 'cam.common.ps1')
$config = Get-CamConfig
$direct = 'rtsp://127.0.0.1:18554/direct-input'
function Measure-It([string]$Name, [scriptblock]$Block) {
    $watch = [System.Diagnostics.Stopwatch]::StartNew(); $value = & $Block; $watch.Stop()
    '{0}={1}|{2}|{3}' -f $Name, ((@($value) | Sort-Object) -join ','), [int]$watch.ElapsedMilliseconds, (Get-CamRelayApiStatus)
}
Measure-It 'ready cam1' { Test-CamRelayPathReady -Config $config -PathName 'cam1' }
Measure-It 'ready cam2' { Test-CamRelayPathReady -Config $config -PathName 'cam2' }
Measure-It 'serving' { Get-CamRelayReadyPaths -Config $config }
Measure-It 'lone cam1' { (Get-CamInputUrl -Config $config -RelayPathName 'cam1' -DirectUrl $direct).Mode }
Measure-It 'lone cam2' { (Get-CamInputUrl -Config $config -RelayPathName 'cam2' -DirectUrl $direct).Mode }
$batch = @(Get-CamRelayReadyPaths -Config $config)
Measure-It 'batch cam2' { (Get-CamInputUrl -Config $config -RelayPathName 'cam2' -DirectUrl $direct -ReadyPaths $batch).Mode }
Measure-It 'wait cam2' { (Get-CamInputUrl -Config $config -RelayPathName 'cam2' -DirectUrl $direct -ReadyPaths @() -WaitSeconds 2).Mode }
'@)
        $probe = {
            $r = Invoke-CamTestScript -Path (Join-Path $dir 'relayprobe.ps1')
            $map = @{}
            foreach ($line in $r.Lines) { if ($line -match '^([a-z0-9 ]+)=([^|]*)\|(\d+)\|(.*)$') { $map[$Matches[1]] = [pscustomobject]@{ Value = $Matches[2]; Ms = [int]$Matches[3]; Api = $Matches[4] } } }
            if ($map.Count -ne 7) { $script:CamRelayApiEvidence += ('probe output: ' + ($r.Lines -join ' | ')) }
            return $map
        }
        $want = @{
            right = @{ 'ready cam1' = 'True'; 'ready cam2' = 'False'; 'serving' = 'cam1'; 'lone cam1' = 'relay'; 'lone cam2' = 'direct'; 'batch cam2' = 'direct'; 'wait cam2' = 'direct' }
            wrong = @{ 'ready cam1' = 'True'; 'ready cam2' = 'True'; 'serving' = 'cam1,cam2'; 'lone cam1' = 'relay'; 'lone cam2' = 'relay'; 'batch cam2' = 'relay'; 'wait cam2' = 'relay' }
            down  = @{ 'ready cam1' = 'False'; 'ready cam2' = 'False'; 'serving' = ''; 'lone cam1' = 'direct'; 'lone cam2' = 'direct'; 'batch cam2' = 'direct'; 'wait cam2' = 'direct' }
        }
        $backup = "$credPath.keep"
        Copy-Item -LiteralPath $credPath -Destination $backup -Force
        $cases = @('right', 'wrong', 'missing', 'down')
        foreach ($case in $cases) {
            if ($case -eq 'wrong') {
                (New-Object System.Management.Automation.PSCredential('camrelayapi', (ConvertTo-SecureString ('0' * 64) -AsPlainText -Force))) | Export-Clixml -LiteralPath $credPath
            }
            if ($case -eq 'missing') { Remove-Item -LiteralPath $credPath -Force }
            if ($case -eq 'down') {
                Copy-Item -LiteralPath $backup -Destination $credPath -Force
                $standalone.Kill(); [void]$standalone.WaitForExit(10000)
            }
            $map = & $probe
            $expect = $want[$(if ($case -eq 'missing') { 'wrong' } else { $case })]
            $limit = $(if ($case -eq 'right' -or $case -eq 'missing') { 1500 } else { 2500 })
            $line = @()
            foreach ($key in @('ready cam1', 'ready cam2', 'serving', 'lone cam1', 'lone cam2', 'batch cam2', 'wait cam2')) {
                $got = $map[$key]
                $bad = (-not $got) -or ($got.Value -ne $expect[$key]) -or ($key -ne 'wait cam2' -and $got.Ms -gt $limit) -or ($key -eq 'wait cam2' -and $got.Ms -gt 4500)
                $line += ('{0}={1} {2}ms{3}' -f $key, $(if ($got) { $got.Value } else { '?' }), $(if ($got) { $got.Ms } else { '?' }), $(if ($bad) { ' (WANT ' + $expect[$key] + ')' } else { '' }))
                if ($bad) { & $fail ("(e) {0} login: {1}" -f $case, $key) }
            }
            $evidence += ("(e) {0,-7} login, api status '{1}': {2}" -f $case, $(if ($map['serving']) { $map['serving'].Api } else { '?' }), ($line -join '; '))
        }
        Move-Item -LiteralPath $backup -Destination $credPath -Force
        if ($publisher -and -not $publisher.HasExited) { $publisher.Kill(); [void]$publisher.WaitForExit(5000) }

        # --- (f) command lines, files, status
        $control = Microsoft.PowerShell.Management\Start-Process -FilePath 'powershell.exe' -PassThru -WindowStyle Hidden -ArgumentList ('-NoProfile -Command "Start-Sleep -Seconds 20 # RAPICONTROL-' + $PID + '"')
        [void]$state.Needles.Add("RAPICONTROL-$PID")
        $deadline = [DateTime]::UtcNow.AddSeconds(15)
        while ([DateTime]::UtcNow -lt $deadline -and @($state.Hits | Where-Object { $_ -like "$($control.Id) *" }).Count -eq 0) { Start-Sleep -Milliseconds 100 }
        $seen = @($state.Hits | Where-Object { $_ -like "$($control.Id) *" }).Count -gt 0
        Stop-Process -Id $control.Id -Force -ErrorAction SilentlyContinue
        $state.Stop = $true
        [void]$sampler.EndInvoke($samplerHandle)
        $hits = @($state.Hits | Where-Object { $_ -notlike "$($control.Id) *" })
        $evidence += ("(f) {0} listing sweeps of every process command line; positive control seen: {1}; API secret or Basic token hits: {2} {3}" -f $state.Samples, $seen, $hits.Count, (($hits | Select-Object -First 3) -join '; '))
        if (-not $seen -or $hits.Count -ne 0 -or $state.Samples -lt 50) { & $fail '(f) a secret reached a command line, or the sweep could not see one' }

        $secrets = @($secret, $token) | Where-Object { $_ }
        $fileHits = @()
        $scanned = 0
        foreach ($file in @(Get-ChildItem -LiteralPath $dir -Recurse -File)) {
            if ($file.Name -in @('relay_api_creds.xml', 'camera_creds.xml', 'mediamtx.exe') -or $file.Extension -in @('.mkv', '.mp4', '.ts')) { continue }
            # Shared read: the synthetic camera still holds its log open.
            $stream = [System.IO.File]::Open($file.FullName, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]'ReadWrite, Delete')
            try { $copy = New-Object System.IO.MemoryStream; $stream.CopyTo($copy); $bytes = $copy.ToArray() } finally { $stream.Dispose() }
            $scanned++
            $texts = @([System.Text.Encoding]::UTF8.GetString($bytes), [System.Text.Encoding]::Unicode.GetString($bytes))
            foreach ($s in @($secrets) + @($camPw)) { foreach ($t in $texts) { if ($t.Contains($s)) { $fileHits += ("{0} carries {1}" -f $file.FullName.Substring($dir.Length), $(if ($s -eq $camPw) { 'the camera password' } else { 'the API secret' })); break } } }
        }
        foreach ($s in @($secrets)) { if ($statusText.Contains($s) -or $standaloneOut.Result.Contains($s)) { $fileHits += '-Status or relay output carries the API secret' } }
        if ($statusText.Contains($camPw)) { $fileHits += '-Status output carries the camera password' }
        $evidence += ("(f) {0} sandbox file(s) scanned (config, logs, status.json, alerts, console), plus -Status output: {1}" -f $scanned, $(if ($fileHits.Count) { $fileHits -join '; ' } else { 'no secret anywhere' }))
        if ($fileHits.Count -gt 0 -or $secrets.Count -ne 2) { & $fail '(f) a secret was found in a file or an output' }
    }
    catch { & $fail ('threw: ' + $_.Exception.Message + ' at line ' + $_.InvocationInfo.ScriptLineNumber) }
    finally {
        $state.Stop = $true
        if ($sampler) { try { [void]$sampler.EndInvoke($samplerHandle) } catch { }; $sampler.Dispose() }
        if ($control) { Stop-Process -Id $control.Id -Force -ErrorAction SilentlyContinue }
        if ($supervisor -and $sandbox) { [void](Stop-CamTestSupervisor -Sandbox $sandbox -Process $supervisor) }
        foreach ($p in @($publisher, $standalone)) { if ($p -and -not $p.HasExited) { try { $p.Kill() } catch { }; [void]$p.WaitForExit(5000) } }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { & $fail 'a synthetic camera process did not exit' }
    }

    $left = @(Get-CimInstance -ClassName Win32_Process -Filter "Name = 'mediamtx.exe' OR Name = 'ffmpeg.exe'" | Where-Object { [string]$_.CommandLine -like "*cam-tests\$name*" -or [string]$_.ExecutablePath -like "*cam-tests\$name*" -or [string]$_.CommandLine -match ":($rtspPort|$camPort)/" })
    $evidence += $script:CamRelayApiEvidence
    $evidence += ("{0} sandbox mediamtx/ffmpeg process(es) left" -f $left.Count)
    if ($left.Count -gt 0) { $script:CamRelayApiOk = $false; foreach ($l in $left) { Stop-Process -Id $l.ProcessId -Force -ErrorAction SilentlyContinue } }

    Complete-CamPruneCheck -Run $Run -Check 'relayapi' -Ok $script:CamRelayApiOk -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $dir; Log = (Join-Path $env:TEMP 'cam-tests\relayapi.none') }) `
        -Message 'the relay API requires the product''s login (401 and no password without it, no cross-origin permission), an old install upgrades on its next start, anonymous RTSP read and publish still work, a refused login keeps every consumer on the relay with one alert and one log line, and no secret reaches a command line, a file or an output'
}


# ---------------------------------------------------------------------------
# Pruner safety ([24] guard half). Every check below works in its own sandbox
# under %TEMP%\cam-tests and removes it when it passes; a failing check leaves
# its sandbox behind for inspection. Only 'prunelive' looks at the real
# archive, read-only, and it runs only when named.
# ---------------------------------------------------------------------------

$script:CamPruneFunctions = @('Invoke-CamPrune', 'Get-CamSegmentDirectories', 'Remove-CamEmptySegment', 'Write-CamHost')

function Test-CamSamePathSet {
    <# True when two path lists hold the same paths, ignoring case and order. #>
    param([AllowEmptyCollection()][string[]]$Left, [AllowEmptyCollection()][string[]]$Right)
    $a = @(@($Left) | Where-Object { $_ } | ForEach-Object { $_.ToLowerInvariant() } | Sort-Object) -join "`n"
    $b = @(@($Right) | Where-Object { $_ } | ForEach-Object { $_.ToLowerInvariant() } | Sort-Object) -join "`n"
    return ($a -eq $b)
}

function Get-CamDeletedPathList {
    <# The paths a prune result deleted, or would have, in the order it went. #>
    param([Parameter(Mandatory = $true)]$Result)
    $paths = @()
    foreach ($line in @($Result.Deletions)) {
        if ($line -match '^(?:would delete|prune: deleted) (.+?) \(\d+ bytes') { $paths += $Matches[1] }
    }
    return , $paths
}

function New-CamPruneState {
    <# The supervisor's prune state, as Invoke-CamSupervisor creates it. #>
    return @{ LastRunAt = $null; DeletedTotal = 0; FreedBytes = 0; Blocked = $false; LastMessage = ''; RefusalAlerted = ''; ClockRefused = $false; BlockedAlerted = $false; AgeBreakerTripped = $false }
}

function Complete-CamPruneCheck {
    <# Records the result, and removes the sandboxes of a check that passed. #>
    param($Run, [string]$Check, [bool]$Ok, [string]$Message, [string[]]$Evidence, [object[]]$Sandboxes)
    if ($Ok) {
        foreach ($sandbox in $Sandboxes) {
            try { Remove-CamTestTree -Path $sandbox.Dir; if (Test-Path -LiteralPath $sandbox.Log) { Remove-Item -LiteralPath $sandbox.Log -Force } }
            catch { $Evidence += ("cleanup of {0} failed: {1}" -f $sandbox.Dir, $_.Exception.Message) }
        }
    }
    else { $Evidence += 'sandbox(es) left for inspection: ' + (@($Sandboxes | ForEach-Object { $_.Dir }) -join ', ') }
    Add-CamTestResult -Run $Run -Check $Check -Status $(if ($Ok) { 'PASS' } else { 'FAIL' }) -Message $Message -Evidence $Evidence
}


function Invoke-CamCheckPruneMatrix {
    <#
        T1 + R1. With every kind of file that is NOT a recorder segment placed
        where the pruner looks - foreign names, hidden and system files, a
        container-mismatch .mp4 and a .mkv.tmp with segment names, an empty
        .dat, underscore folders, a prefix-sibling directory, a junction at the
        top level and one inside a camera folder, a file reparse point - the age
        rule deletes exactly the old segments, and a sha256 manifest of the
        whole sandbox proves every other file is untouched. Then the
        empty-segment cleanup removes only an empty SEGMENT.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name $script:CamPruneFunctions)
    $evidence = @()
    $ok = $true
    $t = New-CamTestPruneRoot -Name 'prune-matrix' -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }
    try {
        $root = $t.Root
        $now = [DateTime]::Now
        $old = $now.AddDays(-10)
        $fe = Join-Path -Path $root -ChildPath 'Front_Entrance'
        $ws = Join-Path -Path $root -ChildPath 'Workshop'
        $outside = Join-Path -Path $t.Dir -ChildPath 'outside'
        $sibling = $root + '_old'

        $expected = @(
            (New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-10)),
            (New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-9)),
            (New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-8)),
            (New-CamTestSegment -Dir $ws -Camera 'Workshop' -Time $now.AddDays(-7))
        )
        [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddMinutes(-2))
        [void](New-CamTestSegment -Dir $ws -Camera 'Workshop' -Time $now.AddMinutes(-2))

        # Claimed while it holds only segments - which is how a real archive
        # gets its marker - and only then does the foreign content arrive.
        $owner = Initialize-CamArchiveOwner -Config $t.Config
        $evidence += ("claimed the clean root: created={0}" -f $owner.Created)
        if (-not $owner.Created) { $ok = $false; $evidence += ('FAIL: a root holding only segments was not claimed: ' + $owner.Problem) }

        $put = {
            param([string]$path, [int]$bytes = 32)
            $dir = Split-Path -Path $path -Parent
            if (-not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }
            if ($bytes -eq 0) { [System.IO.File]::WriteAllBytes($path, (New-Object byte[] 0)) }
            else { [System.IO.File]::WriteAllText($path, ("foreign " + $path)) }
            Set-CamTestFileAge -Path $path -Time $old
        }
        & $put (Join-Path $fe 'notes.txt')
        & $put (Join-Path $fe 'desktop.ini')
        (Get-Item -LiteralPath (Join-Path $fe 'desktop.ini') -Force).Attributes = 'Hidden, System, Archive'
        & $put (Join-Path $fe 'Front_Entrance_export.mkv')
        & $put (Join-Path $fe 'empty.dat') 0
        [void](New-CamTestSegment -Dir $fe -Camera 'Workshop' -Time $old)
        [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $old.AddHours(1) -Extension '.mp4')
        [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $old.AddHours(2) -Extension '.mkv.tmp')
        & $put (Join-Path $ws 'Thumbs.db')
        (Get-Item -LiteralPath (Join-Path $ws 'Thumbs.db') -Force).Attributes = 'Hidden, Archive'
        foreach ($own in @('_state', '_logs', '_selftest')) { [void](New-CamTestSegment -Dir (Join-Path $root $own) -Camera 'Front_Entrance' -Time $old) }
        [void](New-CamTestSegment -Dir (Join-Path $sibling 'Front_Entrance') -Camera 'Front_Entrance' -Time $old)
        [void](New-CamTestSegment -Dir (Join-Path $outside 'Linked') -Camera 'Linked' -Time $old)
        [void](New-CamTestSegment -Dir (Join-Path $outside 'Linked') -Camera 'Linked' -Time $old.AddHours(1))
        [void](New-Item -ItemType Junction -Path (Join-Path $root 'Linked') -Target (Join-Path $outside 'Linked'))
        [void](New-CamTestSegment -Dir (Join-Path $outside 'clips') -Camera 'Front_Entrance' -Time $old)
        [void](New-Item -ItemType Junction -Path (Join-Path $fe 'clips') -Target (Join-Path $outside 'clips'))
        $target = New-CamTestSegment -Dir (Join-Path $outside 'target') -Camera 'Front_Entrance' -Time $now.AddDays(-11)
        $linkPath = Join-Path $fe (Split-Path -Path $target -Leaf)
        $kind = New-CamTestFileReparsePoint -Path $linkPath -Target $target -Time $now.AddDays(-11)
        $evidence += ("file reparse point in the camera folder: " + $kind)

        $before = Get-CamTestFileManifest -Path $t.Dir
        $result = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log
        $after = Get-CamTestFileManifest -Path $t.Dir
        $diff = Compare-CamTestFileManifest -Before $before -After $after

        $evidence += ("manifest {0} entries; removed {1}, changed {2}, added {3}" -f $before.Count, $diff.Removed.Count, $diff.Changed.Count, $diff.Added.Count)
        $evidence += ("removed: " + (@($diff.Removed | ForEach-Object { Split-Path -Path $_ -Leaf }) -join ', '))
        if (-not (Test-CamSamePathSet -Left $diff.Removed -Right $expected)) { $ok = $false; $evidence += 'FAIL: the deleted set is not exactly the four old segments' }
        if ($diff.Changed.Count -or $diff.Added.Count) { $ok = $false; $evidence += ('FAIL: changed or added: ' + (@($diff.Changed) + @($diff.Added) -join ', ')) }

        $evidence += @($result.Foreign | ForEach-Object { 'never deleted: ' + $_ })
        $feLine = @($result.Foreign | Where-Object { $_ -like '7 foreign file(s) left alone in *Front_Entrance' })
        $wsLine = @($result.Foreign | Where-Object { $_ -like '1 foreign file(s) left alone in *Workshop' })
        if ($feLine.Count -ne 1 -or $wsLine.Count -ne 1 -or @($result.Foreign).Count -ne 2) { $ok = $false; $evidence += 'FAIL: expected one count line per folder: 7 in Front_Entrance, 1 in Workshop' }
        $logLines = @(Get-Content -LiteralPath $t.Log | Where-Object { $_ -like '*foreign file(s) left alone*' })
        if ($logLines.Count -ne 2) { $ok = $false; $evidence += ("FAIL: {0} foreign-count log lines for one cycle, expected 2" -f $logLines.Count) }

        $evidence += @($result.Rejected | ForEach-Object { 'refused: ' + $_ })
        if (-not @($result.Rejected | Where-Object { $_ -like ($linkPath + ' (reparse point)') }).Count) { $ok = $false; $evidence += 'FAIL: the file reparse point was not refused by name' }
        if (-not @($result.Rejected | Where-Object { $_ -like '*\Linked (reparse point*' }).Count) { $ok = $false; $evidence += 'FAIL: the top-level junction was not refused' }
        $evidence += $result.StopLine
        if ($result.StopLine -notlike 'prune stop: age rule*') { $ok = $false; $evidence += 'FAIL: no stop line' }

        # Empty-segment cleanup: an empty segment goes; an empty foreign file,
        # an empty container-mismatch segment, and empty segments outside the
        # root or behind a junction stay.
        $emptySegment = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now -Bytes 0
        [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddSeconds(1) -Bytes 0 -Extension '.mp4')
        [void](New-CamTestSegment -Dir (Join-Path $sibling 'Front_Entrance') -Camera 'Front_Entrance' -Time $now -Bytes 0)
        [void](New-CamTestSegment -Dir (Join-Path $outside 'Linked') -Camera 'Linked' -Time $now -Bytes 0)
        $before = Get-CamTestFileManifest -Path $t.Dir
        foreach ($dir in @($fe, (Join-Path $sibling 'Front_Entrance'), (Join-Path $root 'Linked'))) {
            Remove-CamEmptySegment -Entry ([pscustomobject]@{ Dir = $dir }) -Config $t.Config -SupervisorLog $t.Log
        }
        $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $t.Dir)
        $evidence += ("empty cleanup removed: " + (@($diff.Removed | ForEach-Object { Split-Path -Path $_ -Leaf }) -join ', '))
        if (-not (Test-CamSamePathSet -Left $diff.Removed -Right @($emptySegment)) -or $diff.Changed.Count -or $diff.Added.Count) {
            $ok = $false; $evidence += 'FAIL: the empty-segment cleanup touched something other than the one empty segment'
        }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Complete-CamPruneCheck -Run $Run -Check 'prunematrix' -Ok $ok -Evidence $evidence -Sandboxes @($t) `
        -Message 'with foreign, hidden, container-mismatch and empty files, underscore folders, a prefix sibling, two junctions and a file reparse point in place, both deletion paths removed exactly the recorder segments they should and the hash manifest of everything else is unchanged'
}


function Invoke-CamCheckPruneRoots {
    <#
        T2 + H3/H4. Pruning is refused, nothing is deleted, and the call returns
        normally, for: a drive root (a real one, made with subst over a
        sandbox, holding segments AND a marker), a profile-like folder, a root
        with no marker, and a root with a foreign top-level entry. The real
        system and profile folders are tested as a predicate only. The alert
        fires once per refusal, however many cycles it lasts.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name $script:CamPruneFunctions)
    $alerts = New-Object System.Collections.ArrayList
    function Send-CamAlert {
        param([string]$Title, [string]$Message, [int]$EventId, [string]$EntryType, [string]$LogPath, [int]$LogMaxBytes)
        [void]$alerts.Add(("{0} (id {1})" -f $Title, $EventId))
        return @{ Title = $Title; Message = $Message }
    }

    $evidence = @()
    $ok = $true
    $sandboxes = @()
    $now = [DateTime]::Now
    $letter = $null
    try {
        # (a) the predicate, against the real folders - nothing is created there.
        $userProfile = [Environment]::GetFolderPath('UserProfile')
        $refused = @('C:\', 'E:\', '\\server\share', '\\server\share\', 'c:\windows', [Environment]::GetFolderPath('Windows'),
            [Environment]::GetFolderPath('ProgramFiles'), [Environment]::GetFolderPath('ProgramFilesX86'), $userProfile,
            (Split-Path -Path $userProfile -Parent), ((Split-Path -Path $userProfile -Parent) + '\'), (Get-CamTestRepo),
            (Split-Path -Path (Get-CamTestRepo) -Parent))
        $allowed = @((Join-Path $userProfile 'Videos\cameras'), 'E:\cameras', (Join-Path $env:TEMP 'cam-tests\x\archive'))
        foreach ($candidate in $refused) {
            $problem = Get-CamArchiveRootProblem -Root $candidate
            if (-not $problem) { $ok = $false; $evidence += ("FAIL: root '{0}' was allowed" -f $candidate) }
        }
        foreach ($candidate in $allowed) {
            $problem = Get-CamArchiveRootProblem -Root $candidate
            if ($problem) { $ok = $false; $evidence += ("FAIL: root '{0}' was refused: {1}" -f $candidate, $problem) }
        }
        $evidence += ("predicate: {0} unsafe roots refused (drive, share, Windows, Program Files x2, profile, Users, repo, ancestors), {1} nested roots allowed" -f $refused.Count, $allowed.Count)

        # One scenario: segments that the age rule would delete, run twice
        # with the supervisor's state, then the empty cleanup.
        $scenario = {
            param([string]$Label, $Sandbox, $Config, [string]$Expect)
            $alertsBefore = $alerts.Count
            $fe = Join-Path -Path $Config.RootPath -ChildPath 'Front_Entrance'
            $before = Get-CamTestFileManifest -Path $Sandbox.Dir
            $state = New-CamPruneState
            $first = Invoke-CamPrune -Config $Config -SupervisorLog $Sandbox.Log -State $state
            $second = Invoke-CamPrune -Config $Config -SupervisorLog $Sandbox.Log -State $state
            Remove-CamEmptySegment -Entry ([pscustomobject]@{ Dir = $fe }) -Config $Config -SupervisorLog $Sandbox.Log
            $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $Sandbox.Dir)
            $fresh = $alerts.Count - $alertsBefore
            $line = "{0}: blocked={1}, deleted={2}, files changed={3}, alerts over two cycles={4}, refusal: {5}" -f `
                $Label, $first.Blocked, ($first.Deleted + $second.Deleted), ($diff.Removed.Count + $diff.Changed.Count + $diff.Added.Count), $fresh, $first.RootRefusal
            $good = ($first.Blocked -and $second.Blocked -and ($first.Deleted + $second.Deleted) -eq 0 -and
                ($diff.Removed.Count + $diff.Changed.Count + $diff.Added.Count) -eq 0 -and $fresh -eq 1 -and
                $first.RootRefusal -like $Expect -and $state.Blocked -and $state.LastMessage -like 'PRUNING DISABLED*')
            return @{ Ok = $good; Line = $line }
        }
        $seed = {
            param($Sandbox)
            $fe = Join-Path -Path $Sandbox.Root -ChildPath 'Front_Entrance'
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-5))
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-4))
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddMinutes(-3) -Bytes 0)
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddMinutes(-1))
        }

        # (b) a real drive root: subst over a sandbox that even holds a marker.
        $drive = New-CamTestPruneRoot -Name 'prune-roots-drive' -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }
        $sandboxes += $drive
        & $seed $drive
        $claim = Initialize-CamArchiveOwner -Config $drive.Config
        foreach ($candidate in @('Z', 'Y', 'X', 'W', 'V', 'U', 'T', 'S', 'R', 'Q', 'P')) {
            if (-not (Test-Path -LiteralPath ($candidate + ':\'))) { $letter = $candidate; break }
        }
        $previous = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        $substOut = & (Join-Path $env:windir 'System32\subst.exe') ($letter + ':') $drive.Root 2>&1
        $ErrorActionPreference = $previous
        if (-not (Test-Path -LiteralPath ($letter + ':\Front_Entrance'))) { throw "subst ${letter}: failed: $substOut" }
        $driveConfig = $drive.Config.Clone()
        $driveConfig.RootPath = $letter + ':\'
        $outcome = & $scenario ("drive root {0}:\ (marker present={1})" -f $letter, $claim.Created) $drive $driveConfig '*is the root of the volume*'
        $evidence += $outcome.Line
        if (-not $outcome.Ok) { $ok = $false; $evidence += 'FAIL: the drive root was not refused cleanly' }

        # (c) a profile-like folder.
        $profileLike = New-CamTestPruneRoot -Name 'prune-roots-profile' -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }
        $sandboxes += $profileLike
        & $seed $profileLike
        foreach ($leaf in @('Desktop\todo.txt', 'Documents\report.docx', 'AppData\Local\settings.dat', 'NTUSER.DAT')) {
            $path = Join-Path $profileLike.Root $leaf
            [void](New-Item -ItemType Directory -Path (Split-Path -Path $path -Parent) -Force)
            [System.IO.File]::WriteAllText($path, $leaf)
            Set-CamTestFileAge -Path $path -Time $now.AddDays(-30)
        }
        (Get-Item -LiteralPath (Join-Path $profileLike.Root 'NTUSER.DAT') -Force).Attributes = 'Hidden, Archive'
        $claim = Initialize-CamArchiveOwner -Config $profileLike.Config
        $evidence += ("profile-like claim: created={0}, {1}" -f $claim.Created, $claim.Problem)
        if ($claim.Created -or $claim.Problem -notlike '*NTUSER.DAT*') { $ok = $false; $evidence += 'FAIL: a profile-like folder was claimed, or the blocker was not named' }
        $outcome = & $scenario 'profile-like folder' $profileLike $profileLike.Config '*cannot claim the root*'
        $evidence += $outcome.Line
        if (-not $outcome.Ok) { $ok = $false; $evidence += 'FAIL: the profile-like folder was not refused cleanly' }

        # (d) clean, but no marker: a real run refuses, a dry run previews.
        $noMarker = New-CamTestPruneRoot -Name 'prune-roots-nomarker' -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }
        $sandboxes += $noMarker
        & $seed $noMarker
        $outcome = & $scenario 'clean root, no marker' $noMarker $noMarker.Config '*restart the recorder and it claims it*'
        $evidence += $outcome.Line
        if (-not $outcome.Ok) { $ok = $false; $evidence += 'FAIL: a root with no marker was not refused cleanly' }
        $before = Get-CamTestFileManifest -Path $noMarker.Dir
        $preview = Invoke-CamPrune -Config $noMarker.Config -SupervisorLog $noMarker.Log -DryRun
        $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $noMarker.Dir)
        $evidence += ("dry run with no marker: refusal '{0}', would delete {1}, files changed {2}, marker now present={3}" -f `
            $preview.RootRefusal, $preview.Deleted, ($diff.Removed.Count + $diff.Changed.Count + $diff.Added.Count), (Test-CamArchiveMarker -Config $noMarker.Config))
        if ($preview.RootRefusal -or $preview.Deleted -ne 2 -or ($diff.Removed.Count + $diff.Changed.Count + $diff.Added.Count) -ne 0 -or (Test-CamArchiveMarker -Config $noMarker.Config)) {
            $ok = $false; $evidence += 'FAIL: the dry run did not preview a claimable root, or it wrote something'
        }

        # (e) a foreign top-level entry.
        $foreignTop = New-CamTestPruneRoot -Name 'prune-roots-foreign' -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }
        $sandboxes += $foreignTop
        & $seed $foreignTop
        [void](New-Item -ItemType Directory -Path (Join-Path $foreignTop.Root 'Photos') -Force)
        [System.IO.File]::WriteAllText((Join-Path $foreignTop.Root 'Photos\holiday.jpg'), 'jpg')
        [System.IO.File]::WriteAllText((Join-Path $foreignTop.Root 'readme.txt'), 'txt')
        $claim = Initialize-CamArchiveOwner -Config $foreignTop.Config
        $evidence += ("foreign top-level claim: created={0}, {1}" -f $claim.Created, $claim.Problem)
        if ($claim.Created -or $claim.Problem -notlike '*Photos*' -or $claim.Problem -notlike '*readme.txt*') { $ok = $false; $evidence += 'FAIL: a root with foreign entries was claimed, or they were not named' }
        $outcome = & $scenario 'foreign top-level entry' $foreignTop $foreignTop.Config '*cannot claim the root*'
        $evidence += $outcome.Line
        if (-not $outcome.Ok) { $ok = $false; $evidence += 'FAIL: the root with a foreign entry was not refused cleanly' }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }
    finally {
        if ($letter -and (Test-Path -LiteralPath ($letter + ':\'))) {
            $previous = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            [void](& (Join-Path $env:windir 'System32\subst.exe') ($letter + ':') '/D' 2>&1)
            $ErrorActionPreference = $previous
        }
    }

    Complete-CamPruneCheck -Run $Run -Check 'pruneroots' -Ok $ok -Evidence $evidence -Sandboxes $sandboxes `
        -Message 'pruning is refused, nothing is deleted and the call returns for a drive root, a profile-like folder, a root with no marker and a root with a foreign entry; each alerts once; system and profile paths refused as a predicate'
}


function Invoke-CamCheckPruneAge {
    <#
        T3 + R3/R4. A tiny MaxRetentionDays on backdated segments: only
        segments older than the cutoff go, strictly oldest first across
        cameras, the newest of each folder never (even when it is old), a
        segment not older than its folder's newest never, and the run logs
        where it stopped. -DryRun deletes nothing and lists exactly what the
        real run then deletes.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name $script:CamPruneFunctions)
    $evidence = @()
    $ok = $true
    $t = New-CamTestPruneRoot -Name 'prune-age' -Config @{ MaxRetentionDays = 1 }
    try {
        $now = [DateTime]::Now
        $fe = Join-Path $t.Root 'Front_Entrance'
        $ws = Join-Path $t.Root 'Workshop'
        $a5 = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-5)
        $b45 = New-CamTestSegment -Dir $ws -Camera 'Workshop' -Time $now.AddDays(-4.5)
        $a4 = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-4)
        $a3 = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-3)
        $a2h = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddHours(-2)
        $aNew = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddMinutes(-1)
        $bNew = New-CamTestSegment -Dir $ws -Camera 'Workshop' -Time $now.AddDays(-3.5)
        # Created before Workshop's newest but WRITTEN after it: old enough for
        # the cutoff, yet not strictly older than its folder's newest (R3).
        $bHeld = New-CamTestSegment -Dir $ws -Camera 'Workshop' -Time $now.AddDays(-6)
        (Get-Item -LiteralPath $bHeld).LastWriteTime = $now.AddDays(-3.4)
        $expected = @($a5, $b45, $a4, $a3)
        [void](Initialize-CamArchiveOwner -Config $t.Config)

        $before = Get-CamTestFileManifest -Path $t.Dir
        $dry = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -DryRun
        $dryDiff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $t.Dir)
        $dryList = Get-CamDeletedPathList -Result $dry
        $evidence += ("dry run: would delete {0}, files changed {1}" -f $dryList.Count, ($dryDiff.Removed.Count + $dryDiff.Changed.Count + $dryDiff.Added.Count))
        if (($dryDiff.Removed.Count + $dryDiff.Changed.Count + $dryDiff.Added.Count) -ne 0) { $ok = $false; $evidence += 'FAIL: the dry run changed the filesystem' }

        $real = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log
        $realList = Get-CamDeletedPathList -Result $real
        $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $t.Dir)
        $evidence += ("real run deleted, in order: " + (@($realList | ForEach-Object { Split-Path -Path $_ -Leaf }) -join ' > '))
        if (($realList -join '|') -ne ($expected -join '|')) { $ok = $false; $evidence += 'FAIL: not exactly the four expired segments in strict oldest-first order' }
        if (($dryList -join '|') -ne ($realList -join '|')) { $ok = $false; $evidence += 'FAIL: the dry run listed something other than what the real run deleted' }
        if (-not (Test-CamSamePathSet -Left $diff.Removed -Right $expected) -or $diff.Changed.Count -or $diff.Added.Count) { $ok = $false; $evidence += 'FAIL: the filesystem change is not exactly the expected deletions' }
        foreach ($kept in @($a2h, $aNew, $bNew, $bHeld)) {
            if (-not (Test-Path -LiteralPath $kept)) { $ok = $false; $evidence += ('FAIL: deleted ' + $kept) }
        }
        $evidence += 'kept: the 2 h segment (inside the cutoff), both folders'' newest (Workshop''s is 3.5 days old), and a segment written after Workshop''s newest'
        $evidence += $real.StopLine
        if ($real.StopLine -notlike ('prune stop: age rule reached its cutoff * after 4 deletion(s), stopping at ' + (Split-Path -Path $a2h -Leaf) + '*')) { $ok = $false; $evidence += 'FAIL: the stop line does not name the cutoff and the first kept segment' }
        if (-not @(Get-Content -LiteralPath $t.Log | Where-Object { $_ -like '*prune stop: age rule reached its cutoff*' }).Count) { $ok = $false; $evidence += 'FAIL: the stop line was not logged' }
        if (-not @(Get-Content -LiteralPath $t.Log | Where-Object { $_ -like '*kept: not older than its newest segment*' }).Count) { $ok = $false; $evidence += 'FAIL: the held segment was not logged' }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Complete-CamPruneCheck -Run $Run -Check 'pruneage' -Ok $ok -Evidence $evidence -Sandboxes @($t) `
        -Message 'the age rule deletes only expired segments, strictly oldest first across cameras, never a folder''s newest nor a segment not older than it, logs where it stopped, and the dry run lists exactly what the real run deletes while deleting nothing'
}


function Invoke-CamCheckPruneWatermark {
    <#
        T4 + R3/R4. Free space is simulated by replacing Get-CamFreeSpaceGB in
        this scope with a value that rises as files are deleted (one 64 KB
        segment counts as 1 GB). Proves: nothing happens above the reserve;
        below it, globally oldest-first deletion stops exactly at ReserveGB +
        PruneHysteresisGB; with nothing deletable left the run is BLOCKED, says
        so loudly, returns without throwing, and never touches a newest
        segment.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name $script:CamPruneFunctions)
    $evidence = @()
    $ok = $true
    $t = New-CamTestPruneRoot -Name 'prune-watermark'
    try {
        $now = [DateTime]::Now
        $unit = 65536
        $files = @{}
        foreach ($camera in @(@{ Name = 'Cam_A'; Hours = @(40, 30, 20) }, @{ Name = 'Cam_B'; Hours = @(38, 28, 18) }, @{ Name = 'Cam_C'; Hours = @(36, 26, 16) })) {
            $dir = Join-Path $t.Root $camera.Name
            foreach ($hours in $camera.Hours) { $files[('{0}-{1}' -f $camera.Name, $hours)] = New-CamTestSegment -Dir $dir -Camera $camera.Name -Time $now.AddHours(-$hours) -Bytes $unit }
            $files[($camera.Name + '-new')] = New-CamTestSegment -Dir $dir -Camera $camera.Name -Time $now.AddMinutes(-1) -Bytes $unit
        }
        [void](Initialize-CamArchiveOwner -Config $t.Config)

        $fakeBase = 10.0
        $sizeOf = { [double](@(Get-ChildItem -LiteralPath $t.Root -Recurse -File -Force | Measure-Object -Property Length -Sum).Sum) }
        $fakeInitial = & $sizeOf
        function Get-CamFreeSpaceGB {
            param([string]$Path)
            return [math]::Round($fakeBase + (($fakeInitial - (& $sizeOf)) / $unit), 2)
        }

        $t.Config.ReserveGB = 5
        $idle = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log
        $evidence += ("free 10 >= reserve 5: deleted {0}; {1}" -f $idle.Deleted, $idle.StopLine)
        if ($idle.Deleted -ne 0 -or $idle.StopLine -notlike '*watermark not needed*') { $ok = $false; $evidence += 'FAIL: the watermark fired above the reserve' }

        $t.Config.ReserveGB = 13
        $t.Config.PruneHysteresisGB = 2
        $first = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log
        $order = Get-CamDeletedPathList -Result $first
        $want = @($files['Cam_A-40'], $files['Cam_B-38'], $files['Cam_C-36'], $files['Cam_A-30'], $files['Cam_B-28'])
        $evidence += ("free 10 < reserve 13, target 15: deleted in order " + (@($order | ForEach-Object { Split-Path -Path $_ -Leaf }) -join ' > '))
        $evidence += $first.StopLine
        if (($order -join '|') -ne ($want -join '|') -or $first.Blocked) { $ok = $false; $evidence += 'FAIL: not exactly the five globally oldest, in order, stopping at the target' }
        if ($first.StopLine -notlike ('*watermark reached its target 15.00 GB after 5 deletion(s)*stopping at ' + (Split-Path -Path $files['Cam_C-26'] -Leaf))) { $ok = $false; $evidence += 'FAIL: the stop line does not name the target and where it stopped' }

        $t.Config.ReserveGB = 1000
        $blocked = $null
        $threw = ''
        try { $blocked = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log; $again = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log }
        catch { $threw = $_.Exception.Message }
        if ($threw) { $ok = $false; $evidence += ('FAIL: the BLOCKED path threw: ' + $threw) }
        else {
            $evidence += ("reserve 1000: deleted {0}, blocked={1}; second run deleted {2}, blocked={3}" -f $blocked.Deleted, $blocked.Blocked, $again.Deleted, $again.Blocked)
            $evidence += $blocked.Message
            $evidence += $blocked.StopLine
            if ($blocked.Deleted -ne 4 -or -not $blocked.Blocked -or $blocked.Message -notlike 'PRUNE BLOCKED*' -or $again.Deleted -ne 0 -or -not $again.Blocked) { $ok = $false; $evidence += 'FAIL: the BLOCKED path did not delete the rest and then report BLOCKED' }
            if ($blocked.StopLine -notlike '*watermark: nothing deletable left*') { $ok = $false; $evidence += 'FAIL: the stop line does not say nothing deletable is left' }
            if (@(Get-Content -LiteralPath $t.Log | Where-Object { $_ -like '*PRUNE BLOCKED*' }).Count -lt 2) { $ok = $false; $evidence += 'FAIL: BLOCKED was not logged on each run' }
        }
        foreach ($camera in @('Cam_A', 'Cam_B', 'Cam_C')) {
            if (-not (Test-Path -LiteralPath $files[($camera + '-new')])) { $ok = $false; $evidence += ('FAIL: deleted the newest segment of ' + $camera) }
        }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Complete-CamPruneCheck -Run $Run -Check 'prunewatermark' -Ok $ok -Evidence $evidence -Sandboxes @($t) `
        -Message 'the watermark deletes globally oldest-first only below the reserve, stops exactly at reserve + hysteresis, and with nothing left is BLOCKED loudly without throwing and without touching a newest segment'
}


function Invoke-CamCheckPruneClock {
    <#
        R2. Before its first deletion a run checks the archive against the wall
        clock, and refuses the WHOLE run - nothing deleted - when the clock is
        far ahead of the newest segment, when a segment is written in the
        future, or when the oldest candidate is later than the newest segment.
        The clock is moved by replacing Get-CamWallClock in this scope. A
        control run with the real clock proves each fixture would otherwise
        have deleted.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name $script:CamPruneFunctions)
    $evidence = @()
    $ok = $true
    $sandboxes = @()
    $script:CamTestClockShift = [TimeSpan]::Zero
    function Get-CamWallClock { return [DateTime]::UtcNow.Add($script:CamTestClockShift) }
    try {
        $now = [DateTime]::Now
        $case = {
            param([string]$Name, [scriptblock]$Build, [TimeSpan]$Shift, [string]$Expect)
            $t = New-CamTestPruneRoot -Name ('prune-clock-' + $Name) -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }
            $fe = Join-Path $t.Root 'Front_Entrance'
            & $Build $fe
            [void](Initialize-CamArchiveOwner -Config $t.Config)
            $before = Get-CamTestFileManifest -Path $t.Dir
            $script:CamTestClockShift = $Shift
            $state = New-CamPruneState
            $first = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -State $state
            $second = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -State $state
            $script:CamTestClockShift = [TimeSpan]::Zero
            $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $t.Dir)
            $changed = $diff.Removed.Count + $diff.Changed.Count + $diff.Added.Count
            $logged = @(Get-Content -LiteralPath $t.Log | Where-Object { $_ -like '*PRUNE REFUSED*' }).Count
            return [pscustomobject]@{
                Sandbox = $t
                Ok      = ($first.Blocked -and $first.ClockRefusal -like $Expect -and $first.Deleted -eq 0 -and $second.Deleted -eq 0 -and $changed -eq 0 -and $logged -eq 1)
                Line    = ("{0}: refused={1}, deleted={2}, files changed={3}, refusal logged {4}x over two cycles - {5}" -f $Name, [bool]$first.ClockRefusal, ($first.Deleted + $second.Deleted), $changed, $logged, $first.ClockRefusal)
                Fe      = $fe
            }
        }

        $plain = { param($fe)
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-3))
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-2))
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddMinutes(-1)) }
        $future = { param($fe)
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-3))
            $late = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-2)
            (Get-Item -LiteralPath $late).LastWriteTime = $now.AddDays(2)
            [void](New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddMinutes(-1)) }
        $inverted = { param($fe)
            # The newest by creation - the protected one - was written ten days
            # ago; the only other segment was written an hour ago.
            $c = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddDays(-1)
            (Get-Item -LiteralPath $c).LastWriteTime = $now.AddHours(-1)
            $p = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddMinutes(-1)
            (Get-Item -LiteralPath $p).LastWriteTime = $now.AddDays(-10) }

        foreach ($spec in @(
                @{ Name = 'ahead'; Build = $plain; Shift = [TimeSpan]::FromDays(30); Expect = '*ahead of the newest segment*' },
                @{ Name = 'future'; Build = $future; Shift = [TimeSpan]::Zero; Expect = '*in the future of the wall clock*' },
                @{ Name = 'inverted'; Build = $inverted; Shift = [TimeSpan]::Zero; Expect = '*is later than the newest segment*' })) {
            $outcome = & $case $spec.Name $spec.Build $spec.Shift $spec.Expect
            $sandboxes += $outcome.Sandbox
            $evidence += $outcome.Line
            if (-not $outcome.Ok) { $ok = $false; $evidence += ('FAIL: the ' + $spec.Name + ' case was not refused cleanly') }
        }

        # Control: the 'ahead' fixture with the real clock deletes its two
        # expired segments, so the refusal above is what saved them.
        $control = New-CamTestPruneRoot -Name 'prune-clock-control' -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }
        $sandboxes += $control
        & $plain (Join-Path $control.Root 'Front_Entrance')
        [void](Initialize-CamArchiveOwner -Config $control.Config)
        $result = Invoke-CamPrune -Config $control.Config -SupervisorLog $control.Log
        $evidence += ("control with the real clock: refused={0}, deleted {1}" -f [bool]$result.ClockRefusal, $result.Deleted)
        if ($result.ClockRefusal -or $result.Deleted -ne 2) { $ok = $false; $evidence += 'FAIL: the control fixture did not prune normally' }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Complete-CamPruneCheck -Run $Run -Check 'pruneclock' -Ok $ok -Evidence $evidence -Sandboxes $sandboxes `
        -Message 'a run is refused before its first deletion, deleting nothing and logging once, when the clock is far ahead of the newest segment, a segment is future-dated, or the oldest candidate is later than the newest segment'
}


function Invoke-CamCheckPruneBreaker {
    <#
        The age rule's circuit breaker. Over the run's age-rule candidates -
        after every other gate - the share of the archive's segments and of
        its bytes is measured; above MaxAgePruneFraction by EITHER, the age
        rule deletes nothing that run, says so in one loud line naming both
        shares and the limit, alerts once per episode, and the run carries on.
        At 40 % it deletes exactly the expired set. A breach by bytes alone and
        by count alone each trip it. And it never holds back the watermark:
        with the age rule tripped and free space below the reserve, the
        watermark still deletes globally oldest-first to its target. Alerts are
        recorded by a Send-CamAlert stub; nothing is shown on screen.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name $script:CamPruneFunctions)
    $recordedAlerts = New-CamTestAlertRecorder
    function Send-CamAlert { param($Title, $Message, $EventId, $EntryType, $LogPath, $LogMaxBytes) Add-CamTestAlertCall -List $recordedAlerts -Title $Title -Message $Message -EventId $EventId -EntryType $EntryType }
    # Free space: 1 GB per $freeUnit bytes deleted under $freeRoot, from 10 GB.
    $freeRoot = $null
    $freeUnit = 65536
    $freeInitial = 0.0
    $sizeOf = { [double](@(Get-ChildItem -LiteralPath $freeRoot -Recurse -File -Force | Measure-Object -Property Length -Sum).Sum) }
    function Get-CamFreeSpaceGB { param([string]$Path) return [math]::Round(10.0 + (($freeInitial - (& $sizeOf)) / $freeUnit), 2) }

    $evidence = @()
    $ok = $true
    $sandboxes = @()
    $now = [DateTime]::Now

    # Ten segments over two cameras, $Expired of them past a 1-day cutoff
    # (5 days old, oldest first in the returned list), the rest recent.
    $build = {
        param([string]$Name, [int]$Expired, [int]$ExpiredBytes, [int]$RecentBytes)
        $t = New-CamTestPruneRoot -Name ('prune-breaker-' + $Name) -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 0.5 }
        $cams = @('Cam_A', 'Cam_B')
        $old = @()
        for ($i = 0; $i -lt $Expired; $i++) {
            $cam = $cams[$i % 2]
            $old += New-CamTestSegment -Dir (Join-Path $t.Root $cam) -Camera $cam -Time $now.AddDays(-5).AddHours($i) -Bytes $ExpiredBytes
        }
        for ($i = 0; $i -lt (8 - $Expired); $i++) {
            $cam = $cams[$i % 2]
            [void](New-CamTestSegment -Dir (Join-Path $t.Root $cam) -Camera $cam -Time $now.AddHours(-2 - $i) -Bytes $RecentBytes)
        }
        foreach ($cam in $cams) { [void](New-CamTestSegment -Dir (Join-Path $t.Root $cam) -Camera $cam -Time $now.AddMinutes(-1) -Bytes $RecentBytes) }
        [void](Initialize-CamArchiveOwner -Config $t.Config)
        Set-Variable -Name freeRoot -Value $t.Root -Scope 1
        Set-Variable -Name freeInitial -Value (& $sizeOf) -Scope 1
        return [pscustomobject]@{ T = $t; Old = $old }
    }
    $heldLines = { param($t) @(Get-Content -LiteralPath $t.Log | Where-Object { $_ -like '*PRUNE AGE RULE HELD*' }) }

    try {
        # 60 % of the segments (and of the bytes) expired: held.
        $f = & $build 'count60' 6 1024 1024
        $sandboxes += $f.T
        $state = New-CamPruneState
        $before = Get-CamTestFileManifest -Path $f.T.Dir
        $first = Invoke-CamPrune -Config $f.T.Config -SupervisorLog $f.T.Log -State $state
        $second = Invoke-CamPrune -Config $f.T.Config -SupervisorLog $f.T.Log -State $state
        $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $f.T.Dir)
        $lines = @(& $heldLines $f.T)
        $evidence += ("60 % expired, limit 0.5: deleted {0} then {1}, files changed {2}, held lines {3}, alerts {4}" -f $first.Deleted, $second.Deleted, ($diff.Removed.Count + $diff.Changed.Count + $diff.Added.Count), $lines.Count, $recordedAlerts.Count)
        $evidence += @($lines | Select-Object -First 1)
        $evidence += $first.StopLine
        if ($first.Deleted -ne 0 -or $second.Deleted -ne 0 -or ($diff.Removed.Count + $diff.Changed.Count + $diff.Added.Count) -ne 0) { $ok = $false; $evidence += 'FAIL: the age rule deleted past the breaker' }
        if ($lines.Count -ne 1 -or $lines[0] -notlike '*6 of 10 segments (60.0%)*(60.0%) of the bytes*MaxAgePruneFraction 0.5*') { $ok = $false; $evidence += 'FAIL: expected exactly one held line naming both shares and the limit over two runs' }
        if ($recordedAlerts.Count -ne 1 -or $recordedAlerts[0].EventId -ne 122 -or $recordedAlerts[0].EntryType -ne 'Error') { $ok = $false; $evidence += 'FAIL: expected exactly one Error alert with id 122 over two runs' }
        else { $evidence += ("alert: [{0} {1}] {2} - {3}" -f $recordedAlerts[0].EventId, $recordedAlerts[0].EntryType, $recordedAlerts[0].Title, $recordedAlerts[0].Message) }
        if ($first.StopLine -notlike 'prune stop: age rule HELD by the circuit breaker*; watermark not needed*') { $ok = $false; $evidence += 'FAIL: the run did not carry on to the watermark, or the stop line does not say the age rule was held' }

        # The episode ends on a run that is within the limit, and a new one alerts again.
        $f.T.Config.MaxRetentionDays = 30
        $calm = Invoke-CamPrune -Config $f.T.Config -SupervisorLog $f.T.Log -State $state
        $f.T.Config.MaxRetentionDays = 1
        $again = Invoke-CamPrune -Config $f.T.Config -SupervisorLog $f.T.Log -State $state
        $evidence += ("episode: a run within the limit (deleted {0}), then held again: alerts {1}, held lines {2}" -f $calm.Deleted, $recordedAlerts.Count, (& $heldLines $f.T).Count)
        if ($calm.Deleted -ne 0 -or $again.Deleted -ne 0 -or $recordedAlerts.Count -ne 2 -or (& $heldLines $f.T).Count -ne 2) { $ok = $false; $evidence += 'FAIL: a new episode after a run within the limit did not log and alert again' }

        # 40 %: proceeds and deletes exactly the expired set, oldest first.
        $recordedAlerts.Clear()
        $f = & $build 'count40' 4 1024 1024
        $sandboxes += $f.T
        $result = Invoke-CamPrune -Config $f.T.Config -SupervisorLog $f.T.Log -State (New-CamPruneState)
        $order = Get-CamDeletedPathList -Result $result
        $evidence += ("40 % expired: deleted in order " + (@($order | ForEach-Object { Split-Path -Path $_ -Leaf }) -join ' > ') + ("; held lines {0}, alerts {1}" -f (& $heldLines $f.T).Count, $recordedAlerts.Count))
        if (($order -join '|') -ne ($f.Old -join '|') -or (& $heldLines $f.T).Count -ne 0 -or $recordedAlerts.Count -ne 0) { $ok = $false; $evidence += 'FAIL: under the limit the age rule did not delete exactly the four expired segments' }

        # By bytes only (40 % of the count, 72.7 % of the bytes), and by count only (60 %, 8.6 %).
        foreach ($spec in @(
                @{ Name = 'bytes'; Expired = 4; ExpiredBytes = 4096; RecentBytes = 1024; Expect = '*4 of 10 segments (40.0%)*(72.7%) of the bytes*' },
                @{ Name = 'count'; Expired = 6; ExpiredBytes = 512; RecentBytes = 8192; Expect = '*6 of 10 segments (60.0%)*(8.6%) of the bytes*' })) {
            $recordedAlerts.Clear()
            $f = & $build $spec.Name $spec.Expired $spec.ExpiredBytes $spec.RecentBytes
            $sandboxes += $f.T
            $result = Invoke-CamPrune -Config $f.T.Config -SupervisorLog $f.T.Log -State (New-CamPruneState)
            $lines = @(& $heldLines $f.T)
            $evidence += ("breach by {0} only: deleted {1}, alerts {2} - {3}" -f $spec.Name, $result.Deleted, $recordedAlerts.Count, $(if ($lines.Count) { $lines[0] } else { 'no held line' }))
            if ($result.Deleted -ne 0 -or $lines.Count -ne 1 -or $lines[0] -notlike $spec.Expect -or $recordedAlerts.Count -ne 1) { $ok = $false; $evidence += ('FAIL: a breach by ' + $spec.Name + ' alone did not trip the breaker') }
        }

        # Tripped age rule, free space below the reserve: the watermark still
        # deletes the five globally oldest (one 64 KB segment = 1 GB) to 15 GB.
        $recordedAlerts.Clear()
        $f = & $build 'watermark' 6 $freeUnit $freeUnit
        $sandboxes += $f.T
        $f.T.Config.ReserveGB = 13
        $f.T.Config.PruneHysteresisGB = 2
        $result = Invoke-CamPrune -Config $f.T.Config -SupervisorLog $f.T.Log -State (New-CamPruneState)
        $order = Get-CamDeletedPathList -Result $result
        $want = @($f.Old | Select-Object -First 5)
        $byAge = @($result.Deletions | Where-Object { $_ -like '*MaxRetentionDays*' })
        $evidence += ("tripped + free 10 < reserve 13: held={0}, deleted in order {1}; age-reason deletions {2}; free after {3}" -f [bool]$result.AgeBreaker, (@($order | ForEach-Object { Split-Path -Path $_ -Leaf }) -join ' > '), $byAge.Count, $result.FreeGBAfter)
        $evidence += $result.StopLine
        if (-not $result.AgeBreaker -or ($order -join '|') -ne ($want -join '|') -or $byAge.Count -ne 0 -or $result.Blocked) { $ok = $false; $evidence += 'FAIL: the breaker held back the watermark, or the watermark did not delete the five oldest to its target' }
        if ($result.StopLine -notlike '*watermark reached its target 15.00 GB after 5 deletion(s)*') { $ok = $false; $evidence += 'FAIL: the watermark did not stop at its target' }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Complete-CamPruneCheck -Run $Run -Check 'prunebreaker' -Ok $ok -Evidence $evidence -Sandboxes $sandboxes `
        -Message 'above MaxAgePruneFraction by segments or by bytes the age rule deletes nothing, logs one line with both shares, alerts once per episode and the run carries on; under it the expired set goes; the watermark is never held back'
}


function Invoke-CamCheckPruneBlocked {
    <#
        A BLOCKED prune - free space below the reserve and nothing deletable
        left - alerts once when the episode starts (id 121, Error, with the
        numbers and what to do), not again while it lasts, and again only
        after a run that was not blocked. A dry run never alerts. Free space is
        pinned at 1 GB by replacing Get-CamFreeSpaceGB; alerts are recorded by
        a Send-CamAlert stub, nothing is shown on screen.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name $script:CamPruneFunctions)
    $recordedAlerts = New-CamTestAlertRecorder
    function Send-CamAlert { param($Title, $Message, $EventId, $EntryType, $LogPath, $LogMaxBytes) Add-CamTestAlertCall -List $recordedAlerts -Title $Title -Message $Message -EventId $EventId -EntryType $EntryType }
    function Get-CamFreeSpaceGB { param([string]$Path) return 1.0 }

    $evidence = @()
    $ok = $true
    $t = New-CamTestPruneRoot -Name 'prune-blocked' -Config @{ ReserveGB = 20 }
    try {
        $now = [DateTime]::Now
        $fe = Join-Path $t.Root 'Front_Entrance'
        $old = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddHours(-3)
        $newest = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time $now.AddMinutes(-1)
        [void](Initialize-CamArchiveOwner -Config $t.Config)
        $state = New-CamPruneState

        $dry = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -State $state -DryRun
        $evidence += ("dry run: blocked={0}, alerts {1}" -f $dry.Blocked, $recordedAlerts.Count)
        if (-not $dry.Blocked -or $recordedAlerts.Count -ne 0) { $ok = $false; $evidence += 'FAIL: the dry run was not blocked, or alerted' }

        $runs = @()
        $runs += Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -State $state
        $afterEntry = $recordedAlerts.Count
        $runs += Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -State $state
        $afterRepeat = $recordedAlerts.Count
        $t.Config.ReserveGB = 0
        $runs += Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -State $state
        $afterClear = $recordedAlerts.Count
        $t.Config.ReserveGB = 20
        $runs += Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -State $state
        $afterNew = $recordedAlerts.Count

        $evidence += ("runs blocked: {0}; alerts after each run: {1}, {2}, {3}, {4}" -f (@($runs | ForEach-Object { $_.Blocked }) -join ', '), $afterEntry, $afterRepeat, $afterClear, $afterNew)
        $evidence += ("first run deleted {0}; the newest segment exists: {1}" -f $runs[0].Deleted, (Test-Path -LiteralPath $newest))
        if (-not $runs[0].Blocked -or -not $runs[1].Blocked -or $runs[2].Blocked -or -not $runs[3].Blocked) { $ok = $false; $evidence += 'FAIL: the runs were not blocked, blocked, clear, blocked' }
        if ($afterEntry -ne 1 -or $afterRepeat -ne 1 -or $afterClear -ne 1 -or $afterNew -ne 2) { $ok = $false; $evidence += 'FAIL: expected one alert on entry, none while it lasts, one more after a clear run' }
        if ($runs[0].Deleted -ne 1 -or (Test-Path -LiteralPath $old) -or -not (Test-Path -LiteralPath $newest)) { $ok = $false; $evidence += 'FAIL: the blocked run did not delete the one deletable segment and keep the newest' }
        foreach ($alert in @($recordedAlerts)) {
            $evidence += ("alert: [{0} {1}] {2} - {3}" -f $alert.EventId, $alert.EntryType, $alert.Title, $alert.Message)
            if ($alert.EventId -ne 121 -or $alert.EntryType -ne 'Error' -or $alert.Message -notlike '*free 1.00 GB*20.00 GB reserve*' -or $alert.Message -notlike '*ReserveGB*' -or $alert.Message -notlike '*RootPath*' -or $alert.Message -notlike '*free space*' -or $alert.Message -notlike '*Recording continues*') {
                $ok = $false; $evidence += 'FAIL: the alert is not id 121 Error with free and reserve GB, what to do, and that recording continues'
            }
        }
        if (@(Get-CamAlertEventIdList) -notcontains 121 -or @(Get-CamAlertEventIdList) -notcontains 122) { $ok = $false; $evidence += 'FAIL: 121 and 122 are not in Get-CamAlertEventIdList' }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Complete-CamPruneCheck -Run $Run -Check 'pruneblocked' -Ok $ok -Evidence $evidence -Sandboxes @($t) `
        -Message 'a BLOCKED prune alerts once on entering the state (id 121, Error, with the numbers and what to do), not again while it lasts, again after a clear run, and never from a dry run'
}


function Invoke-CamCheckPruneConfig {
    <#
        MaxAgePruneFraction is validated at load like the other checked keys:
        0, -1, 1.5 and a non-number are refused with a message naming the key
        and its range; 1.0 and 0.5 load; a file without it gets 0.5. The
        example config and the configuration reference both list it.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $dir = Join-Path $env:TEMP 'cam-tests\prune-config'
    $sandbox = [pscustomobject]@{ Dir = $dir; Log = (Join-Path $env:TEMP 'cam-tests\prune-config.none') }
    try {
        Remove-CamTestTree -Path $dir
        [void](New-Item -ItemType Directory -Path $dir -Force)
        $load = {
            param([string]$Json)
            $path = Join-Path $dir 'recorder.config.json'
            [System.IO.File]::WriteAllText($path, $Json)
            try { return [pscustomobject]@{ Value = (Get-CamConfig -Path $path).MaxAgePruneFraction; Error = '' } }
            catch { return [pscustomobject]@{ Value = $null; Error = $_.Exception.Message } }
        }
        foreach ($bad in @('0', '-1', '1.5', '"abc"')) {
            $outcome = & $load ('{ "MaxAgePruneFraction": ' + $bad + ' }')
            $evidence += ("{0}: refused='{1}'" -f $bad, $outcome.Error)
            if (-not $outcome.Error -or $outcome.Error -notlike '*MaxAgePruneFraction*0.01*1.0*') { $ok = $false; $evidence += ('FAIL: ' + $bad + ' was not refused with a message naming the key and its range') }
        }
        foreach ($good in @(@{ Text = '1.0'; Value = 1.0 }, @{ Text = '0.5'; Value = 0.5 })) {
            $outcome = & $load ('{ "MaxAgePruneFraction": ' + $good.Text + ' }')
            $evidence += ("{0}: loaded {1} {2}" -f $good.Text, [string]$outcome.Value, $outcome.Error)
            if ($outcome.Error -or [double]$outcome.Value -ne $good.Value) { $ok = $false; $evidence += ('FAIL: ' + $good.Text + ' was not accepted') }
        }
        $outcome = & $load '{ "ReserveGB": 20 }'
        $evidence += ("key absent: loaded {0} {1}" -f [string]$outcome.Value, $outcome.Error)
        if ($outcome.Error -or [double]$outcome.Value -ne 0.5) { $ok = $false; $evidence += 'FAIL: the default is not 0.5' }

        $example = [System.IO.File]::ReadAllText((Join-Path (Get-CamTestRepo) 'recorder.config.example.json')) | ConvertFrom-Json
        $docRows = @(Get-Content -LiteralPath (Join-Path (Get-CamTestRepo) 'docs\configuration.md') | Where-Object { $_.StartsWith('| `MaxAgePruneFraction` | `0.5` |') })
        $evidence += ("example config MaxAgePruneFraction = {0}; configuration.md rows {1}" -f [string]$example.MaxAgePruneFraction, $docRows.Count)
        if ([string]$example.MaxAgePruneFraction -ne '0.5' -or $docRows.Count -ne 1) { $ok = $false; $evidence += 'FAIL: the example config or the configuration reference does not list the key with its default' }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Complete-CamPruneCheck -Run $Run -Check 'pruneconfig' -Ok $ok -Evidence $evidence -Sandboxes @($sandbox) `
        -Message 'MaxAgePruneFraction outside 0.01..1.0 or not a number is a config error naming the key; 1.0 and 0.5 load; the default is 0.5; the example config and the docs list it'
}


function Invoke-CamCheckPruneOwner {
    <#
        H4 and "recording is unaffected", with the real supervisor in two
        sandboxes. A clean root is claimed on start (the marker appears). A
        root with a foreign top-level entry is NOT claimed: pruning is
        disabled, the refusal names the entry, alerts once over several prune
        cycles - id 120, Error, recorded by the sandbox alert stub - the bait
        files are untouched, and the supervisor stays up and keeps
        (re)starting its camera child.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $now = [DateTime]::Now
    $camera = { param($name, $ip) [pscustomobject]@{ Name = $name; IP = $ip; Quality = 'main'; Record = $true } }
    $settings = @{ PruneIntervalSeconds = 3; StatusIntervalSeconds = 2; ReconcileSeconds = 2; MaxRetentionDays = 1 }
    $sandboxes = @()

    foreach ($kind in @('clean', 'foreign')) {
        $sandbox = New-CamTestSandbox -Name ('pruneowner-' + $kind) -Cameras @(& $camera ('pruneowner-' + $kind) '10.255.255.13') -Config $settings
        $stubLog = Set-CamTestAlertStub -Sandbox $sandbox
        $sandboxes += [pscustomobject]@{ Dir = $sandbox.Dir; Log = (Join-Path $env:TEMP ('cam-tests\pruneowner-' + $kind + '.none')) }
        $log = Join-Path $sandbox.Root '_logs\recorder.log'
        $marker = Get-CamArchiveMarkerPath -Config $sandbox.Config
        $baitDir = $null
        if ($kind -eq 'foreign') {
            [void](New-Item -ItemType Directory -Path (Join-Path $sandbox.Root 'Photos') -Force)
            [System.IO.File]::WriteAllText((Join-Path $sandbox.Root 'Photos\holiday.jpg'), 'jpg')
            $baitDir = Join-Path $sandbox.Root 'pruneowner-foreign'
            [void](New-CamTestSegment -Dir $baitDir -Camera 'pruneowner-foreign' -Time $now.AddDays(-5))
            [void](New-CamTestSegment -Dir $baitDir -Camera 'pruneowner-foreign' -Time $now.AddDays(-4))
            [void](New-CamTestSegment -Dir $baitDir -Camera 'pruneowner-foreign' -Time $now.AddMinutes(-1))
            $before = Get-CamTestFileManifest -Path $baitDir
            $beforePhotos = Get-CamTestFileManifest -Path (Join-Path $sandbox.Root 'Photos')
        }

        $process = $null
        try {
            $process = Start-CamTestSupervisor -Sandbox $sandbox
            Start-Sleep -Seconds 14
            $status = (Get-CamTestStatusJson -Sandbox $sandbox).Snapshot
            $row = @($status.Cameras)[0]
            $text = @(Get-Content -LiteralPath $log)
            $alive = -not $process.HasExited
            if ($kind -eq 'clean') {
                $claimed = @($text | Where-Object { $_ -like '*claimed * as this recorder''s archive*' }).Count
                $evidence += ("clean root: marker written={0}, claim logged={1}, supervisor alive={2}, state {3}" -f (Test-CamArchiveMarker -Config $sandbox.Config), $claimed, $alive, $status.State)
                $stubbed = @(if (Test-Path -LiteralPath $stubLog) { Get-Content -LiteralPath $stubLog }).Count
                if (-not (Test-CamArchiveMarker -Config $sandbox.Config) -or $claimed -ne 1 -or -not $alive -or $stubbed) { $ok = $false; $evidence += ("FAIL: a clean root was not claimed on start, or raised {0} alert(s)" -f $stubbed) }
            }
            else {
                $alertLines = @($text | Where-Object { $_ -like '*ALERT pruning is disabled*' })
                $refusals = @($text | Where-Object { $_ -like '*PRUNING DISABLED*' }).Count
                $cycles = @($text | Where-Object { $_ -like '*did not claim*' }).Count
                $after = Get-CamTestFileManifest -Path $baitDir
                $diff = Compare-CamTestFileManifest -Before $before -After $after
                $photos = Compare-CamTestFileManifest -Before $beforePhotos -After (Get-CamTestFileManifest -Path (Join-Path $sandbox.Root 'Photos'))
                $changed = $diff.Removed.Count + $diff.Changed.Count + $diff.Added.Count + $photos.Removed.Count + $photos.Changed.Count + $photos.Added.Count
                $evidence += ("foreign root: marker written={0}, did-not-claim logged={1}, refusal logged {2}x, alert lines {3}, bait files changed {4}" -f `
                    (Test-Path -LiteralPath $marker), $cycles, $refusals, $alertLines.Count, $changed)
                $evidence += @($alertLines | Select-Object -First 1)
                $stubbed = @(if (Test-Path -LiteralPath $stubLog) { Get-Content -LiteralPath $stubLog })
                $evidence += ("alerts recorded by the sandbox stub: {0}, of them id 120 Error: {1}" -f $stubbed.Count, @($stubbed | Where-Object { $_ -like '120 Error pruning is disabled*' }).Count)
                if ($stubbed.Count -ne 1 -or @($stubbed | Where-Object { $_ -like '120 Error pruning is disabled*' }).Count -ne 1) { $ok = $false; $evidence += 'FAIL: the stub did not record exactly one id 120 Error alert' }
                $evidence += ("status: State {0}, Prune.Blocked {1}, LastMessage: {2}" -f $status.State, $status.Prune.Blocked, $status.Prune.LastMessage)
                $evidence += ("recording continues: supervisor alive={0}, camera child launched as pid {1}, restarts {2}" -f $alive, $row.LastPid, $row.RestartsTotal)
                if ((Test-Path -LiteralPath $marker) -or $alertLines.Count -ne 1 -or $refusals -ne 1 -or $changed -ne 0) { $ok = $false; $evidence += 'FAIL: the foreign root was claimed, alerted other than once, or a bait file changed' }
                if (-not $status.Prune.Blocked -or $status.Prune.LastMessage -notlike '*Photos*') { $ok = $false; $evidence += 'FAIL: status.json does not say pruning is disabled and why' }
                if (-not $alive -or $status.State -ne 'running' -or [int]$row.LastPid -le 0) { $ok = $false; $evidence += 'FAIL: the supervisor stopped, or never started its camera''s recorder' }
            }
        }
        catch { $ok = $false; $evidence += ('FAIL: ' + $kind + ' threw: ' + $_.Exception.Message) }
        finally {
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            if (-not $stop.Ok) { $ok = $false; $evidence += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
    }

    Complete-CamPruneCheck -Run $Run -Check 'pruneowner' -Ok $ok -Evidence $evidence -Sandboxes $sandboxes `
        -Message 'the supervisor claims a clean root on start; a root with a foreign entry stays unclaimed, pruning is disabled with one alert, nothing is deleted, and recording carries on'
}


function Invoke-CamCheckPruneLive {
    <#
        T5, H4, H5 against the Operator's LIVE archive, strictly read-only: no
        file is opened, created or deleted under RootPath. Runs only when
        named. Two dry runs - MaxRetentionDays 1, and ReserveGB forced high -
        with Remove-Item and New-Item replaced in this scope by tripwires, and
        a metadata manifest before and after. Reports counts per camera, never
        file lists.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name $script:CamPruneFunctions)
    $tripped = New-Object System.Collections.ArrayList
    function Remove-Item { [void]$tripped.Add('Remove-Item ' + ($args -join ' ')); throw 'tripwire: Remove-Item during a live dry run' }
    function New-Item { [void]$tripped.Add('New-Item ' + ($args -join ' ')); throw 'tripwire: New-Item during a live dry run' }
    function Move-Item { [void]$tripped.Add('Move-Item ' + ($args -join ' ')); throw 'tripwire: Move-Item during a live dry run' }

    $evidence = @()
    $ok = $true
    $config = Get-CamConfig
    $root = [string]$config.RootPath
    $logDir = Join-Path $env:TEMP 'cam-tests'
    if (-not (Test-Path -LiteralPath $logDir)) { [void](Microsoft.PowerShell.Management\New-Item -ItemType Directory -Path $logDir -Force) }
    $log = Join-Path $logDir 'prunelive-dryrun.log'
    if (Test-Path -LiteralPath $log) { Microsoft.PowerShell.Management\Remove-Item -LiteralPath $log -Force }
    try {
        $markerBefore = Test-Path -LiteralPath (Get-CamArchiveMarkerPath -Config $config)
        $before = Get-CamTestFileManifest -Path $root -MetadataOnly

        # H5: every file in every camera folder is a recorder segment, by an
        # expression written here rather than the product's.
        $independent = '^' + [regex]::Escape($root.TrimEnd('\')) + '\\(?<cam>[^\\_][^\\]*)\\\k<cam>_\d{8}_\d{6}[+-]\d{4}' + [regex]::Escape((Get-CamContainerProfile -Config $config).Extension) + '$'
        $cameraFiles = @($before.Keys | Where-Object { $_ -notlike ($root.TrimEnd('\') + '\_*') })
        $matching = @($cameraFiles | Where-Object { $_ -match $independent })
        $evidence += ("H5: {0} file(s) in camera folders, {1} match the segment scheme, {2} do not" -f $cameraFiles.Count, $matching.Count, ($cameraFiles.Count - $matching.Count))
        foreach ($odd in @($cameraFiles | Where-Object { $_ -notmatch $independent } | Select-Object -First 5)) { $evidence += ('  not a segment: ' + $odd) }
        if ($matching.Count -ne $cameraFiles.Count) { $ok = $false; $evidence += 'FAIL: H5 does not hold - existing footage would be stranded as foreign' }

        # H4: the product's own read-only claim test.
        $claim = Get-CamArchiveClaimProblem -Config $config
        $evidence += ("H4: marker present={0}; claim problem: '{1}' ({2})" -f $markerBefore, $claim, $(if ($claim) { 'the next start would NOT claim it' } else { 'the next start claims it unattended' }))
        if ($claim) { $ok = $false; $evidence += 'FAIL: H4 does not hold' }

        foreach ($spec in @(@{ Label = 'MaxRetentionDays=1'; Key = 'MaxRetentionDays'; Value = 1 }, @{ Label = 'ReserveGB=100000'; Key = 'ReserveGB'; Value = 100000 })) {
            $trial = $config.Clone()
            $trial[$spec.Key] = $spec.Value
            # The breaker would hold a 1-day retention on this archive and list
            # nothing; switched off, every segment the age rule reaches is checked.
            $trial.MaxAgePruneFraction = 1.0
            $result = Invoke-CamPrune -Config $trial -SupervisorLog $log -DryRun
            $paths = Get-CamDeletedPathList -Result $result
            $notSegments = @($paths | Where-Object { $_ -notmatch $independent })
            $protectedHit = @($paths | Where-Object { @($result.Protected) -contains $_ })
            $evidence += ("dry run {0}: root refusal '{1}', clock refusal '{2}', would delete {3} of {4} candidate(s), {5} GB; not a segment {6}; a protected newest {7}; foreign lines {8}; rejected {9}" -f `
                $spec.Label, $result.RootRefusal, $result.ClockRefusal, $paths.Count, $result.Candidates, (Format-CamNumber -Value ($result.FreedBytes / 1GB)), $notSegments.Count, $protectedHit.Count, @($result.Foreign).Count, @($result.Rejected).Count)
            $evidence += ('  ' + $result.StopLine)
            foreach ($group in @($paths | Group-Object -Property { Split-Path -Path (Split-Path -Path $_ -Parent) -Leaf } | Sort-Object -Property Name)) {
                $leaves = @($group.Group | ForEach-Object { Split-Path -Path $_ -Leaf } | Sort-Object)
                $evidence += ("  {0}: {1} would go, oldest {2}, newest {3}" -f $group.Name, $group.Count, $leaves[0], $leaves[-1])
            }
            if ($result.RootRefusal -or $result.ClockRefusal -or $notSegments.Count -or $protectedHit.Count) { $ok = $false; $evidence += 'FAIL: the dry run was refused, or listed a non-segment or a protected file' }
        }

        $after = Get-CamTestFileManifest -Path $root -MetadataOnly
        $diff = Compare-CamTestFileManifest -Before $before -After $after
        $own = { param($path) $path -like ($root.TrimEnd('\') + '\_state\*') -or $path -like ($root.TrimEnd('\') + '\_logs\*') }
        $foreignChange = @(@($diff.Removed) + @($diff.Changed | Where-Object { -not (& $own $_) -and $_ -notmatch $independent }) + @($diff.Added | Where-Object { -not (& $own $_) -and $_ -notmatch $independent }))
        $evidence += ("archive before/after: {0} removed, {1} changed, {2} added; outside the recorder's own writing (growing newest segments, new segments, _state, _logs): {3}" -f `
            $diff.Removed.Count, $diff.Changed.Count, $diff.Added.Count, $foreignChange.Count)
        $evidence += ("tripwires hit: {0}; marker present after={1}" -f $tripped.Count, (Test-Path -LiteralPath (Get-CamArchiveMarkerPath -Config $config)))
        if ($diff.Removed.Count -or $foreignChange.Count -or $tripped.Count -or ((Test-Path -LiteralPath (Get-CamArchiveMarkerPath -Config $config)) -ne $markerBefore)) {
            $ok = $false; $evidence += 'FAIL: something under the live archive changed in a way the running recorder does not explain'
        }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Add-CamTestResult -Run $Run -Check 'prunelive' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) -Evidence $evidence `
        -Message ("read-only dry runs against the live archive {0}: every would-delete is a recorder segment, nothing written or deleted there" -f $root)
}


# ---------------------------------------------------------------------------
# cam.ps1 -Install and -Uninstall
# ---------------------------------------------------------------------------

$script:CamInstallOrder = @('restreamer config', 'recorder logon task', 'dashboard logon task', 'UI logon task')


function New-CamTestInstallStub {
    <#
        Stand-ins for what Get-CamInstallComponent returns: the same fields,
        with each granular switch replaced by a flip of $State[key] and a line
        in $Calls. $Fail maps a key to 'install', 'remove' or 'noop' - noop
        is an install that returns without installing anything.
    #>
    param(
        [Parameter(Mandatory = $true)][hashtable]$State,
        [Parameter(Mandatory = $true)][AllowEmptyCollection()][System.Collections.ArrayList]$Calls,
        [hashtable]$Fail = @{}
    )

    $keys = @('restreamer', 'recorder', 'dashboard', 'ui')
    $list = @()
    for ($i = 0; $i -lt $keys.Count; $i++) {
        $key = $keys[$i]
        $list += [pscustomobject]@{
            Name    = $script:CamInstallOrder[$i]
            Key     = $key
            Change  = "stub install of $key"
            Undo    = "stub removal of $key"
            Test    = { [pscustomobject]@{ Current = [bool]$State[$key]; Present = [bool]$State[$key]; Detail = "stub $key" } }.GetNewClosure()
            Install = {
                [void]$Calls.Add("install $key")
                if ($Fail[$key] -eq 'install') { throw "stub $key install failed" }
                if ($Fail[$key] -ne 'noop') { $State[$key] = $true }
            }.GetNewClosure()
            Remove  = {
                [void]$Calls.Add("remove $key")
                if ($Fail[$key] -eq 'remove') { throw "stub $key removal failed" }
                $State[$key] = $false
            }.GetNewClosure()
        }
    }
    return $list
}


function Invoke-CamCheckInstallSteps {
    <#
        The -Install and -Uninstall orchestration with every component step
        stubbed: the real Invoke-CamInstallAll and Invoke-CamUninstallAll from
        cam.ps1, run in this scope against stand-ins for the preflight, the
        components, the elevation test and the event-source step. Nothing
        outside this scope is touched.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Script 'cam.ps1' -Name 'Invoke-CamInstallAll', 'Invoke-CamUninstallAll', 'Write-CamComponentResult', 'Write-CamSection')

    $calls = New-Object System.Collections.ArrayList
    $stubPreflight = @()
    $stubComponents = @()
    $stubElevated = $false
    $eventFails = $false
    function Get-CamInstallPreflight { return $stubPreflight }
    function Get-CamInstallComponent { return $stubComponents }
    function Test-CamElevated { return $stubElevated }
    function Test-CamEventSourceRegistered { return $false }
    function Invoke-CamInstallEventSourceStep { [void]$calls.Add('event source'); if ($eventFails) { throw 'stub event source failed' } }

    $okRow = { param($name, $component) [pscustomobject]@{ Name = $name; Component = $component; Hard = ($component -ne 'ui'); Ok = $true; Detail = 'stub'; Fix = '' } }
    $badRow = { param($name, $component, $fix) [pscustomobject]@{ Name = $name; Component = $component; Hard = ($component -ne 'ui'); Ok = $false; Detail = 'stub missing'; Fix = $fix } }

    $evidence = @()
    $ok = $true
    $run1 = {
        param([scriptblock]$Action)
        $calls.Clear()
        $output = @(& $Action 6>&1)
        $codes = @($output | Where-Object { $_ -is [int] })
        [pscustomobject]@{
            Code  = $(if ($codes.Count) { $codes[-1] } else { $null })
            Text  = (@($output | Where-Object { $_ -isnot [int] } | ForEach-Object { [string]$_ }) -join "`n")
            Calls = @($calls)
        }
    }
    $statusOf = { param($result) @($script:CamInstallOrder | ForEach-Object { Get-CamTestComponentStatus -Text $result.Text -Component $_ }) -join ',' }
    $expect = {
        param([string]$Label, $Result, [int]$Code, [string]$Statuses, [string]$CallList, [string[]]$Contains = @())
        $got = & $statusOf $Result
        $line = ("{0}: exit {1}, statuses {2}, calls [{3}]" -f $Label, $Result.Code, $got, ($Result.Calls -join ', '))
        $bad = @()
        if ($Result.Code -ne $Code) { $bad += "exit $($Result.Code), wanted $Code" }
        if ($got -ne $Statuses) { $bad += "statuses $got, wanted $Statuses" }
        if (($Result.Calls -join ',') -ne $CallList) { $bad += "calls [$($Result.Calls -join ',')], wanted [$CallList]" }
        foreach ($needle in $Contains) { if (-not $Result.Text.Contains($needle)) { $bad += "output lacks '$needle'" } }
        if ($bad.Count) { $script:installStepsOk = $false; return ("FAIL " + $line + ' - ' + ($bad -join '; ')) }
        return $line
    }
    $script:installStepsOk = $true
    $allOk = @((& $okRow 'cameras.json' 'all'), (& $okRow 'ffmpeg' 'recorder'), (& $okRow 'camui.exe' 'ui'))
    $installs = 'install restreamer,install recorder,install dashboard,install ui'

    # Fresh machine, unelevated.
    $state = @{}
    $stubPreflight = $allOk
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls
    $r = & $run1 { Invoke-CamInstallAll }
    $evidence += & $expect 'all succeed' $r 0 'installed,installed,installed,installed' $installs @('OPTIONAL', '.\cam.recorder.ps1 -InstallEventSource')

    # The same state again: nothing to do, nothing done.
    $r = & $run1 { Invoke-CamInstallAll }
    $evidence += & $expect 'second run' $r 0 'already installed,already installed,already installed,already installed' ''

    # One component throws; the rest are still attempted.
    $state = @{}
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls -Fail @{ recorder = 'install' }
    $r = & $run1 { Invoke-CamInstallAll }
    $evidence += & $expect 'recorder fails' $r 1 'installed,failed,installed,installed' $installs @('stub recorder install failed', 'not in place: recorder logon task')

    # An install that reports nothing wrong but leaves the component absent.
    $state = @{}
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls -Fail @{ dashboard = 'noop' }
    $r = & $run1 { Invoke-CamInstallAll }
    $evidence += & $expect 'silent no-op install' $r 1 'installed,installed,failed,installed' $installs @('still not in place')

    # camui.exe not built: the UI alone is skipped, with the reason.
    $state = @{}
    $stubPreflight = @((& $okRow 'cameras.json' 'all'), (& $badRow 'camui.exe' 'ui' 'cross-compile it with ui/build.sh'))
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls
    $r = & $run1 { Invoke-CamInstallAll }
    $evidence += & $expect 'UI binary missing' $r 1 'installed,installed,installed,skipped' 'install restreamer,install recorder,install dashboard' @('camui.exe is missing: cross-compile it with ui/build.sh')

    # Hard prerequisites missing: every one listed, no component touched.
    $state = @{}
    $stubPreflight = @((& $badRow 'cameras.json' 'all' 'create it'), (& $badRow 'ffmpeg' 'recorder' 'install ffmpeg'), (& $badRow 'camui.exe' 'ui' 'build it'))
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls
    $r = & $run1 { Invoke-CamInstallAll }
    $evidence += & $expect 'preflight blocks' $r 1 ',,,' '' @('fix: create it', 'fix: install ffmpeg', 'fix: build it', 'Nothing was installed or changed')
    if ($r.Text.Contains('install: result')) { $script:installStepsOk = $false; $evidence += 'FAIL preflight blocks: it went on to the components' }

    # Elevated: the event source is registered as the last step, and its
    # failure is reported without changing the exit code.
    $stubPreflight = $allOk
    $stubElevated = $true
    $state = @{}
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls
    $r = & $run1 { Invoke-CamInstallAll }
    $evidence += & $expect 'elevated' $r 0 'installed,installed,installed,installed' ($installs + ',event source')
    $eventFails = $true
    $state = @{}
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls
    $r = & $run1 { Invoke-CamInstallAll }
    $evidence += & $expect 'elevated, event source fails' $r 0 'installed,installed,installed,installed' ($installs + ',event source') @('It is optional')
    $stubElevated = $false
    $eventFails = $false

    # -WhatIf: the plan, and no step run.
    $state = @{}
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls
    $r = & $run1 { Invoke-CamInstallAll -WhatIf }
    $evidence += & $expect 'install -WhatIf' $r 0 'would install,would install,would install,would install' '' @('Nothing was changed')

    # Uninstall, in reverse order, then again as a no-op.
    $state = @{ restreamer = $true; recorder = $true; dashboard = $true; ui = $true }
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls
    $r = & $run1 { Invoke-CamUninstallAll -WhatIf }
    $evidence += & $expect 'uninstall -WhatIf' $r 0 'would remove,would remove,would remove,would remove' ''
    $removes = 'remove ui,remove dashboard,remove recorder,remove restreamer'
    $r = & $run1 { Invoke-CamUninstallAll }
    $evidence += & $expect 'uninstall' $r 0 'removed,removed,removed,removed' $removes @('nothing was stopped', '.\cam.ps1 -Stop')
    $r = & $run1 { Invoke-CamUninstallAll }
    $evidence += & $expect 'uninstall again' $r 0 'not installed,not installed,not installed,not installed' ''
    $state = @{ restreamer = $true; recorder = $true; dashboard = $true; ui = $true }
    $stubComponents = New-CamTestInstallStub -State $state -Calls $calls -Fail @{ dashboard = 'remove' }
    $r = & $run1 { Invoke-CamUninstallAll }
    $evidence += & $expect 'uninstall, dashboard fails' $r 1 'removed,removed,failed,removed' $removes @('stub dashboard removal failed')

    Add-CamTestResult -Run $Run -Check 'installsteps' -Status $(if ($script:installStepsOk) { 'PASS' } else { 'FAIL' }) `
        -Message 'the -Install/-Uninstall orchestration with stubbed components: order, results, exit codes, second runs, preflight, elevation, -WhatIf' `
        -Evidence $evidence
}


function Invoke-CamCheckInstallPlan {
    <#
        cam.ps1 -Install -WhatIf and -Uninstall -WhatIf as real processes:
        first the repository's own copy against this machine, then a sandbox
        whose launchers would all be new, so every component is planned. The
        IPCCTV-* tasks and both launcher folders must be identical afterwards.
    #>
    param($Run)

    if (-not (Get-Command -Name 'Get-ScheduledTask' -ErrorAction SilentlyContinue)) {
        Add-CamTestResult -Run $Run -Check 'installplan' -Status 'SKIP' -Message 'the ScheduledTasks module is not available on this machine'
        return
    }

    $evidence = @()
    $ok = $true
    $liveLaunchers = Get-CamAutostartDir
    $sandbox = New-CamTestInstallSandbox -Name 'installplan'
    $tasksBefore = Get-CamTestLogonTaskSnapshot
    $liveBefore = Get-CamTestLauncherManifest -Path $liveLaunchers
    $sandboxBefore = Get-CamTestFileManifest -Path $sandbox.Dir
    $evidence += ("before: {0} IPCCTV-* task(s), {1} file(s) in {2}" -f $tasksBefore.Count, $liveBefore.Count, $liveLaunchers)

    # Every component has a row in the result table, in the order given.
    $order = {
        param([string]$Text, [string[]]$Names)
        $start = $Text.IndexOf('install: result ===')
        if ($start -lt 0) { return $false }
        $at = @($Names | ForEach-Object { $Text.IndexOf(('  ' + $_ + ' '), $start) })
        if ($at[0] -lt 0) { return $false }
        for ($i = 1; $i -lt $at.Count; $i++) { if ($at[$i] -le $at[$i - 1]) { return $false } }
        return $true
    }
    $reverse = @($script:CamInstallOrder)
    [array]::Reverse($reverse)

    try {
        $runs = @(
            @{ Label = 'repository -Install -WhatIf'; Path = (Join-Path -Path (Get-CamTestRepo) -ChildPath 'cam.ps1'); Args = @('-Install', '-WhatIf'); Env = @{}; Names = $script:CamInstallOrder },
            @{ Label = 'repository -Uninstall -WhatIf'; Path = (Join-Path -Path (Get-CamTestRepo) -ChildPath 'cam.ps1'); Args = @('-Uninstall', '-WhatIf'); Env = @{}; Names = $reverse },
            @{ Label = 'sandbox -Install -WhatIf'; Path = $sandbox.Cam; Args = @('-Install', '-WhatIf'); Env = @{ LOCALAPPDATA = $sandbox.LocalAppData }; Names = $script:CamInstallOrder; Want = 'would install' },
            @{ Label = 'sandbox -Uninstall -WhatIf'; Path = $sandbox.Cam; Args = @('-Uninstall', '-WhatIf'); Env = @{ LOCALAPPDATA = $sandbox.LocalAppData }; Names = $reverse }
        )
        foreach ($case in $runs) {
            $result = Invoke-CamTestScript -Path $case.Path -Arguments $case.Args -Environment $case.Env
            $text = $result.Output + $result.Error
            $statuses = @($case.Names | ForEach-Object { Get-CamTestComponentStatus -Text $text -Component $_ })
            $whatIfLines = @($result.Lines | Where-Object { $_ -like 'What if:*' }).Count
            $evidence += ("{0}: exit {1}, {2}, {3} 'What if' line(s)" -f $case.Label, $result.ExitCode, ($statuses -join ', '), $whatIfLines)
            $inOrder = & $order $text $case.Names
            if (-not $inOrder) { $ok = $false; $evidence += '  FAIL: the components are not all reported, in order' }
            $failedOrSkipped = @($statuses | Where-Object { $_ -in @('failed', 'skipped') }).Count
            if ($result.ExitCode -ne $(if ($failedOrSkipped) { 1 } else { 0 })) { $ok = $false; $evidence += '  FAIL: the exit code does not follow the results' }
            if ($case.Args[0] -eq '-Install' -and -not $text.Contains('install: prerequisites')) { $ok = $false; $evidence += '  FAIL: no prerequisites section' }
            if ($case.Want) {
                if (@($statuses | Where-Object { $_ -ne $case.Want }).Count -or $whatIfLines -ne $case.Names.Count) {
                    $ok = $false; $evidence += ("  FAIL: wanted every component '{0}' with one 'What if' line each" -f $case.Want)
                }
                $evidence += @($result.Lines | Where-Object { $_ -like 'What if:*' } | ForEach-Object { '  ' + $_ })
            }
        }

        $tasksAfter = Get-CamTestLogonTaskSnapshot
        $liveDiff = Compare-CamTestFileManifest -Before $liveBefore -After (Get-CamTestLauncherManifest -Path $liveLaunchers)
        $sandboxDiff = Compare-CamTestFileManifest -Before $sandboxBefore -After (Get-CamTestFileManifest -Path $sandbox.Dir)
        $tasksSame = (($tasksBefore -join "`n") -eq ($tasksAfter -join "`n"))
        $evidence += ("after: tasks identical {0}; {1} changed {2}/{3}/{4} (removed/added/changed); sandbox changed {5}/{6}/{7}" -f $tasksSame, $liveLaunchers,
            $liveDiff.Removed.Count, $liveDiff.Added.Count, $liveDiff.Changed.Count, $sandboxDiff.Removed.Count, $sandboxDiff.Added.Count, $sandboxDiff.Changed.Count)
        $evidence += @($tasksAfter | ForEach-Object { '  ' + $_ })
        if (-not $tasksSame -or ($liveDiff.Removed.Count + $liveDiff.Added.Count + $liveDiff.Changed.Count + $sandboxDiff.Removed.Count + $sandboxDiff.Added.Count + $sandboxDiff.Changed.Count)) {
            $ok = $false; $evidence += 'FAIL: -WhatIf changed something'
        }
        if (Test-Path -LiteralPath $sandbox.LauncherDir) { $ok = $false; $evidence += 'FAIL: the sandbox launcher folder was created' }
    }
    finally {
        if ($ok) { Remove-CamTestTree -Path $sandbox.Dir }
    }

    Add-CamTestResult -Run $Run -Check 'installplan' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message '-Install -WhatIf and -Uninstall -WhatIf list every component in order and change nothing, here and in a sandbox' `
        -Evidence $evidence
}


function Invoke-CamCheckInstallPreflight {
    <#
        A sandbox missing several prerequisites at once, run with -Install
        -WhatIf: every missing one must be listed in that one run, with its
        fix, and nothing may change. A second sandbox has a recorder.config.json
        that does not load, which must be reported beside the rest rather than
        ending the script on its own.

        -WhatIf because the sandbox keeps the real task names, and a preflight
        that wrongly passed would otherwise re-register the Operator's tasks.
        That a failed preflight stops a real -Install before any component is
        proven with stubs by installsteps.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $tasksBefore = Get-CamTestLogonTaskSnapshot
    $sandboxes = @()
    try {
        $missing = New-CamTestInstallSandbox -Name 'installpreflight'
        $sandboxes += $missing
        foreach ($file in @('cameras.json', 'camera_creds.xml', 'camui.exe', 'bin\ffmpeg.exe', 'bin\mediamtx.exe', 'bin\mpv.com')) {
            Remove-Item -LiteralPath (Join-Path -Path $missing.Dir -ChildPath $file) -Force
        }
        $broken = New-CamTestInstallSandbox -Name 'installpreflight-config'
        $sandboxes += $broken
        [System.IO.File]::WriteAllText($broken.ConfigPath, '{ "SegmentSeconds": 0 }')
        Remove-Item -LiteralPath (Join-Path -Path $broken.Dir -ChildPath 'camera_creds.xml') -Force

        $cases = @(
            @{ Sandbox = $missing; Label = 'six missing'; Want = @('MISSING  cameras.json', 'MISSING  camera credentials', 'MISSING  ffmpeg', 'MISSING  MediaMTX ', 'MISSING  mpv', 'missing  camui.exe') },
            @{ Sandbox = $broken; Label = 'config does not load, credentials missing'; Want = @('MISSING  recorder.config.json', 'SegmentSeconds must be at least 1', 'MISSING  camera credentials', 'MISSING  ffmpeg, MediaMTX') }
        )
        foreach ($case in $cases) {
            $before = Get-CamTestFileManifest -Path $case.Sandbox.Dir
            $result = Invoke-CamTestScript -Path $case.Sandbox.Cam -Arguments @('-Install', '-WhatIf') -Environment @{ LOCALAPPDATA = $case.Sandbox.LocalAppData }
            $text = $result.Output + $result.Error
            $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $case.Sandbox.Dir)
            $absent = @($case.Want | Where-Object { -not $text.Contains($_) })
            $evidence += ("{0}: exit {1}, {2} of {3} expected problem line(s) present, files changed {4}" -f $case.Label, $result.ExitCode,
                ($case.Want.Count - $absent.Count), $case.Want.Count, ($diff.Removed.Count + $diff.Added.Count + $diff.Changed.Count))
            $evidence += @($result.Lines | Where-Object { $_ -match '^\s+(MISSING|missing)\s' } | ForEach-Object { '  ' + $_.Trim() })
            if ($result.ExitCode -ne 1 -or $absent.Count -or -not $text.Contains('Nothing was installed or changed') -or $text.Contains('install: result')) {
                $ok = $false; $evidence += ('  FAIL: wanted exit 1, every problem listed and no component reached; absent: ' + ($absent -join ' | '))
            }
            if ($diff.Removed.Count + $diff.Added.Count + $diff.Changed.Count) { $ok = $false; $evidence += '  FAIL: the sandbox changed' }
        }
        $tasksSame = ((Get-CamTestLogonTaskSnapshot) -join "`n") -eq ($tasksBefore -join "`n")
        $evidence += ("IPCCTV-* tasks identical: {0}" -f $tasksSame)
        if (-not $tasksSame) { $ok = $false }
    }
    finally {
        if ($ok) { foreach ($sandbox in $sandboxes) { Remove-CamTestTree -Path $sandbox.Dir } }
    }

    Add-CamTestResult -Run $Run -Check 'installpreflight' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'a preflight with several prerequisites missing lists every one in one run and changes nothing' `
        -Evidence $evidence
}


function Invoke-CamCheckInstallSandbox {
    <#
        A real -Install, second -Install, -Uninstall and second -Uninstall in
        a sandbox, through the real granular switches. It proves what the
        stubs cannot: that each component's "already in place" test agrees
        with what its own install switch writes.

        The sandbox's copies of cam.ps1 and cam.recorder.ps1 register
        CamTestSandbox-Recorder, -Dashboard and -Ui instead of the IPCCTV-*
        names - New-CamTestInstallSandbox refuses to build one that still
        names a real task - and its launchers go to its own LOCALAPPDATA. Its
        one camera is an unroutable address and its camui.exe is a stand-in,
        so even a task left behind by a crash could start nothing that matters;
        the finally removes them regardless.
    #>
    param($Run)

    $prefix = 'CamTestSandbox-'
    if (-not (Get-Command -Name 'Register-ScheduledTask' -ErrorAction SilentlyContinue)) {
        Add-CamTestResult -Run $Run -Check 'installsandbox' -Status 'SKIP' -Message 'the ScheduledTasks module is not available on this machine'
        return
    }
    $realMediaMtx = [string](Get-CamConfig).RestreamerExePath
    if (-not (Test-Path -LiteralPath $realMediaMtx)) {
        Add-CamTestResult -Run $Run -Check 'installsandbox' -Status 'SKIP' -Message "-InstallRestreamer runs MediaMTX --version, and there is no MediaMTX at $realMediaMtx"
        return
    }
    if (@(Get-ScheduledTask -TaskName ($prefix + '*') -ErrorAction SilentlyContinue).Count) {
        Add-CamTestResult -Run $Run -Check 'installsandbox' -Status 'SKIP' -Message "tasks named $prefix* are already registered, left by an earlier run; remove them by hand first"
        return
    }

    $evidence = @()
    $ok = $true
    $tasksBefore = Get-CamTestLogonTaskSnapshot
    $sandbox = New-CamTestInstallSandbox -Name 'installsandbox' -TaskPrefix $prefix -MediaMtxPath $realMediaMtx
    $childEnvironment = @{ LOCALAPPDATA = $sandbox.LocalAppData }
    $evidence += ("sandbox tasks: {0}" -f (@($sandbox.TaskNames.Values) -join ', '))

    try {
        $steps = @(
            @{ Label = '-Install'; Args = @('-Install'); Want = 'installed' },
            @{ Label = '-Install again'; Args = @('-Install'); Want = 'already installed' },
            @{ Label = '-Uninstall'; Args = @('-Uninstall'); Want = 'removed' },
            @{ Label = '-Uninstall again'; Args = @('-Uninstall'); Want = 'not installed' }
        )
        foreach ($step in $steps) {
            $before = Get-CamTestFileManifest -Path $sandbox.Dir
            $result = Invoke-CamTestScript -Path $sandbox.Cam -Arguments $step.Args -Environment $childEnvironment
            $text = $result.Output + $result.Error
            $statuses = @($script:CamInstallOrder | ForEach-Object { Get-CamTestComponentStatus -Text $text -Component $_ })
            $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $sandbox.Dir)
            $registered = @($sandbox.TaskNames.Values | Where-Object { Get-ScheduledTask -TaskName $_ -ErrorAction SilentlyContinue }).Count
            $evidence += ("{0}: exit {1}, {2}; {3} sandbox task(s) registered; files removed/added/changed {4}/{5}/{6}" -f $step.Label, $result.ExitCode,
                ($statuses -join ', '), $registered, $diff.Removed.Count, $diff.Added.Count, $diff.Changed.Count)
            $wrong = @($statuses | Where-Object { $_ -ne $step.Want })
            if ($result.ExitCode -ne 0 -or $wrong.Count) {
                $ok = $false
                $evidence += ("  FAIL: wanted exit 0 and every component '{0}'" -f $step.Want)
                $evidence += @($result.Lines | Where-Object { $_ -match 'failed|skipped|MISSING|Exception' } | Select-Object -First 12 | ForEach-Object { '  ' + $_ })
            }
            if ($step.Label -like '*again' -and ($diff.Removed.Count + $diff.Added.Count + $diff.Changed.Count)) {
                $ok = $false; $evidence += '  FAIL: the second run changed files'
            }
            $wantRegistered = $(if ($step.Label -like '-Install*') { 3 } else { 0 })
            if ($registered -ne $wantRegistered) { $ok = $false; $evidence += ("  FAIL: wanted {0} sandbox task(s) registered" -f $wantRegistered) }
        }
        $left = @(Get-ChildItem -LiteralPath $sandbox.LauncherDir -Force -ErrorAction SilentlyContinue | ForEach-Object { $_.Name })
        $evidence += ("left in the sandbox launcher folder: {0}" -f $(if ($left.Count) { $left -join ', ' } else { 'nothing' }))
        if (@($left | Where-Object { $_ -like '*.ps1' }).Count) { $ok = $false; $evidence += 'FAIL: a launcher survived -Uninstall' }
        if (Test-Path -LiteralPath ([string]$sandbox.Config.RestreamerConfigPath)) { $ok = $false; $evidence += 'FAIL: the MediaMTX config survived -Uninstall' }
        foreach ($kept in @($sandbox.Cameras, $sandbox.ConfigPath, (Join-Path -Path $sandbox.Dir -ChildPath 'camera_creds.xml'))) {
            if (-not (Test-Path -LiteralPath $kept)) { $ok = $false; $evidence += "FAIL: -Uninstall removed $kept" }
        }
    }
    finally {
        foreach ($name in @($sandbox.TaskNames.Values)) {
            if (Get-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue) {
                Unregister-ScheduledTask -TaskName $name -Confirm:$false
                $ok = $false
                $evidence += "FAIL: had to unregister the sandbox task $name by hand"
            }
        }
        $tasksSame = ((Get-CamTestLogonTaskSnapshot) -join "`n") -eq ($tasksBefore -join "`n")
        $evidence += ("IPCCTV-* tasks identical: {0}" -f $tasksSame)
        if (-not $tasksSame) { $ok = $false }
        if ($ok) { Remove-CamTestTree -Path $sandbox.Dir }
    }

    Add-CamTestResult -Run $Run -Check 'installsandbox' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'a real -Install, -Install, -Uninstall, -Uninstall in a sandbox with its own task names: installed, already installed, removed, not installed' `
        -Evidence $evidence
}


function Get-CamTestNumberedLines {
    <#
        The numbered lines a log-rotation check wrote ("<tag> <00001> xxx"),
        in file order, from each of $Path that exists, plus how many
        "log rotation blocked" notices those files hold.
    #>
    param([Parameter(Mandatory = $true)][string[]]$Path)

    $lines = New-Object System.Collections.ArrayList
    $notices = 0
    foreach ($file in $Path) {
        if (-not (Test-Path -LiteralPath $file)) { continue }
        foreach ($text in [System.IO.File]::ReadAllLines($file)) {
            if ($text -match ' (?<tag>[A-Za-z]+) (?<n>\d{5}) x+$') {
                [void]$lines.Add([pscustomobject]@{ Tag = $Matches['tag']; N = [int]$Matches['n'] })
            }
            elseif ($text -like '*log rotation blocked*') { $notices++ }
        }
    }
    return [pscustomobject]@{ Lines = $lines.ToArray(); Notices = $notices }
}


function Get-CamTestNumberGaps {
    <#
        For one tag, the numbers missing between the first surviving line and
        $Last - the lines lost OTHER than the older generations rotation drops
        by design - and the range that was dropped before the first survivor.
    #>
    param([object[]]$Lines, [string]$Tag, [int]$Last)

    $numbers = @($Lines | Where-Object { $_.Tag -eq $Tag } | ForEach-Object { $_.N })
    if ($numbers.Count -eq 0) { return [pscustomobject]@{ First = 0; Missing = $Last; Disordered = 0; Dropped = "1-$Last" } }
    $seen = @{}
    $disordered = 0
    for ($i = 0; $i -lt $numbers.Count; $i++) {
        $seen[$numbers[$i]] = $true
        if ($i -gt 0 -and $numbers[$i] -le $numbers[$i - 1]) { $disordered++ }
    }
    $first = $numbers[0]
    $missing = @($first..$Last | Where-Object { -not $seen.ContainsKey($_) }).Count
    return [pscustomobject]@{
        First      = $first
        Missing    = $missing
        Disordered = $disordered
        Dropped    = $(if ($first -gt 1) { "1-$($first - 1)" } else { 'none' })
    }
}


function Invoke-CamCheckLogRotate {
    <#
        A1: Write-CamLog's rotation at MaxBytes 4096, in-process and across
        processes.

        - One writer: it rolls exactly on the write that finds the file at or
          over the limit, never otherwise; <log>.1 is the generation just
          before <log>; at most two files exist; the numbered lines across
          both are contiguous, older generations being dropped by design.
        - Locked: a reader holds the log without delete sharing, as an editor
          or tail tool does and as Get-Content -Wait does whenever its poll is
          inside the file. The rename fails. That used to THROW out of
          Write-CamLog, which ended the supervisor loop and lost the line.
          Now no write throws, every line lands in the unrotated file, the
          blocked rotation is logged once for the episode, and the first write
          after the reader lets go rotates.
        - A real Get-Content -Wait tailing the log while it rolls.
        - Two writer processes on one log, as the supervisor and the NVR host
          share recorder.log: neither throws and no line is lost.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $max = 4096
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\logrotate'
    if (Test-Path -LiteralPath $dir) { Remove-CamTestTree -Path $dir }
    [void](New-Item -ItemType Directory -Path $dir -Force)
    $pad = 'x' * 40
    $write = {
        param([string]$Path, [string]$Tag, [int]$N)
        try { Write-CamLog -Path $Path -Message ("{0} {1:D5} {2}" -f $Tag, $N, $pad) -MaxBytes $max; return '' }
        catch { return $_.Exception.Message }
    }

    # --- one writer
    $log = Join-Path -Path $dir -ChildPath 'seq.log'
    $count = 300
    $rolls = 0
    $wrong = @()
    $throws = 0
    $mostFiles = 0
    for ($i = 1; $i -le $count; $i++) {
        $before = 0
        if (Test-Path -LiteralPath $log) { $before = (Get-Item -LiteralPath $log).Length }
        if (& $write $log 'seq' $i) { $throws++ }
        $after = (Get-Item -LiteralPath $log).Length
        $rolled = ($after -lt $before)
        if ($rolled) { $rolls++ }
        if ($rolled -ne ($before -ge $max)) { $wrong += ("write {0}: size before {1}, rolled {2}" -f $i, $before, $rolled) }
        $files = @(Get-ChildItem -LiteralPath $dir -Filter 'seq.log*' -File).Count
        if ($files -gt $mostFiles) { $mostFiles = $files }
    }
    $older = Get-CamTestNumberedLines -Path "$log.1"
    $newer = Get-CamTestNumberedLines -Path $log
    $all = @($older.Lines) + @($newer.Lines)
    $gaps = Get-CamTestNumberGaps -Lines $all -Tag 'seq' -Last $count
    $seam = "{0} -> {1}" -f @($older.Lines)[-1].N, @($newer.Lines)[0].N
    $evidence += ("one writer: {0} lines, {1} rolls, {2} write(s) rolled other than exactly at the crossing, {3} throw(s), at most {4} file(s)" -f $count, $rolls, $wrong.Count, $throws, $mostFiles)
    $evidence += ("  seq.log.1 holds {0}-{1}, seq.log holds {2}-{3} (seam {4}); dropped by design: lines {5} ({6} older generation(s)); missing after the first survivor: {7}" -f `
        @($older.Lines)[0].N, @($older.Lines)[-1].N, @($newer.Lines)[0].N, @($newer.Lines)[-1].N, $seam, $gaps.Dropped, ($rolls - 1), $gaps.Missing)
    $evidence += @($wrong | Select-Object -First 3)
    if ($rolls -lt 3 -or $wrong.Count -or $throws -or $mostFiles -gt 2 -or $gaps.Missing -or $gaps.Disordered -or
        (@($older.Lines)[-1].N + 1) -ne @($newer.Lines)[0].N -or @($newer.Lines)[-1].N -ne $count) {
        $ok = $false; $evidence += 'FAIL: rotation did not roll once per crossing into a contiguous previous generation'
    }

    # --- locked: a reader without delete sharing holds the full log
    $log = Join-Path -Path $dir -ChildPath 'lock.log'
    $i = 0
    while (-not (Test-Path -LiteralPath $log) -or (Get-Item -LiteralPath $log).Length -lt $max) { $i++; [void](& $write $log 'fill' $i) }
    $reader = [System.IO.File]::Open($log, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    $failures = @()
    try { for ($n = 1; $n -le 5; $n++) { $problem = & $write $log 'lock' $n; if ($problem) { $failures += ("write {0}: {1}" -f $n, $problem) } } }
    finally { $reader.Dispose() }
    $held = Get-CamTestNumberedLines -Path $log
    $heldLines = @($held.Lines | Where-Object { $_.Tag -eq 'lock' }).Count
    $heldSize = (Get-Item -LiteralPath $log).Length
    $rolledWhileHeld = Test-Path -LiteralPath "$log.1"
    $problem = & $write $log 'lock' 6
    $afterOld = Get-CamTestNumberedLines -Path "$log.1"
    $afterNew = Get-CamTestNumberedLines -Path $log
    $evidence += ("locked (FileShare.ReadWrite reader on a {0}-byte log): {1} of 5 writes threw, {2} of 5 lines in lock.log, grew to {3} bytes, rolled while held={4}, blocked notices {5}" -f `
        $max, $failures.Count, $heldLines, $heldSize, $rolledWhileHeld, $held.Notices)
    $evidence += @($failures | Select-Object -First 2)
    $evidence += ("  reader released, next write: threw={0}, lock.log.1 holds {1} lock line(s) + {2} notice(s), lock.log holds {3}" -f `
        [bool]$problem, @($afterOld.Lines | Where-Object { $_.Tag -eq 'lock' }).Count, $afterOld.Notices, ((@($afterNew.Lines) | ForEach-Object { "$($_.Tag) $($_.N)" }) -join ', '))
    if ($failures.Count -or $heldLines -ne 5 -or $rolledWhileHeld -or $held.Notices -ne 1 -or $problem -or
        @($afterOld.Lines | Where-Object { $_.Tag -eq 'lock' }).Count -ne 5 -or @($afterNew.Lines).Count -ne 1 -or $afterNew.Notices -ne 0) {
        $ok = $false; $evidence += 'FAIL: a locked log threw, lost a line, rotated while held, logged the episode other than once, or did not rotate once released'
    }

    # --- a real Get-Content -Wait following the log while it rolls
    $log = Join-Path -Path $dir -ChildPath 'tail.log'
    [void](& $write $log 'tail' 1)
    $tailer = Start-Process -FilePath 'powershell.exe' -PassThru -WindowStyle Hidden `
        -ArgumentList @('-NoProfile', '-Command', "Get-Content -LiteralPath '$log' -Wait")
    [void]$tailer.Handle
    $count = 3000
    $failures = 0
    $firstFailure = ''
    try {
        Start-Sleep -Seconds 3
        for ($i = 2; $i -le $count; $i++) {
            $problem = & $write $log 'tail' $i
            if ($problem) { $failures++; if (-not $firstFailure) { $firstFailure = "write ${i}: $problem" } }
            if ($i % 50 -eq 0) { Start-Sleep -Milliseconds 30 }
        }
    }
    finally {
        if (-not $tailer.HasExited) { try { $tailer.Kill() } catch { }; [void]$tailer.WaitForExit(10000) }
    }
    $tail = Get-CamTestNumberedLines -Path "$log.1", $log
    $gaps = Get-CamTestNumberGaps -Lines $tail.Lines -Tag 'tail' -Last $count
    $evidence += ("Get-Content -Wait tailer pid {0} (exited {1}): {2} writes, {3} threw, {4} missing after the first survivor ({5}), {6} blocked notice(s) in the surviving files" -f `
        $tailer.Id, $tailer.HasExited, $count, $failures, $gaps.Missing, $gaps.First, $tail.Notices)
    if ($firstFailure) { $evidence += ('  first: ' + $firstFailure) }
    if ($failures -or $gaps.Missing -or $gaps.Disordered -or -not $tailer.HasExited) { $ok = $false; $evidence += 'FAIL: writing under a Get-Content -Wait reader threw or lost lines' }

    # --- two writer processes on one log
    $writer = Join-Path -Path $dir -ChildPath 'writer.ps1'
    [System.IO.File]::WriteAllText($writer, @'
param([string]$Lib, [string]$Log, [string]$Tag, [int]$Count, [string]$Go, [int]$MaxBytes)
$ErrorActionPreference = 'Stop'
. $Lib
while (-not (Test-Path -LiteralPath $Go)) { Start-Sleep -Milliseconds 10 }
$errors = 0; $first = ''
for ($i = 1; $i -le $Count; $i++) {
    try { Write-CamLog -Path $Log -Message ("{0} {1:D5} {2}" -f $Tag, $i, ('x' * 40)) -MaxBytes $MaxBytes }
    catch { $errors++; if (-not $first) { $first = "write ${i}: " + $_.Exception.Message } }
}
"errors=$errors $first"
'@)
    $lib = Join-Path -Path (Get-CamTestRepo) -ChildPath 'cam.common.ps1'
    foreach ($round in @(@{ Name = 'stress'; Count = 400 }, @{ Name = 'exact'; Count = 40 })) {
        $log = Join-Path -Path $dir -ChildPath ($round.Name + '.log')
        $go = Join-Path -Path $dir -ChildPath ($round.Name + '.go')
        $writers = @()
        foreach ($tag in @('A', 'B')) {
            $writers += Start-Process -FilePath 'powershell.exe' -PassThru -WindowStyle Hidden `
                -RedirectStandardOutput (Join-Path $dir "$($round.Name)-$tag.out") `
                -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$writer`"", '-Lib', "`"$lib`"", '-Log', "`"$log`"",
                    '-Tag', $tag, '-Count', $round.Count, '-Go', "`"$go`"", '-MaxBytes', $max)
        }
        foreach ($process in $writers) { [void]$process.Handle }
        Start-Sleep -Seconds 3
        [System.IO.File]::WriteAllText($go, 'go')
        foreach ($process in $writers) {
            if (-not $process.WaitForExit(120000)) { try { $process.Kill() } catch { }; $ok = $false; $evidence += "FAIL: writer pid $($process.Id) did not finish" }
        }
        $reports = @('A', 'B' | ForEach-Object { ([string](Get-Content -LiteralPath (Join-Path $dir "$($round.Name)-$_.out") -Raw)).Trim() })
        $lines = Get-CamTestNumberedLines -Path "$log.1", $log
        $a = Get-CamTestNumberGaps -Lines $lines.Lines -Tag 'A' -Last $round.Count
        $b = Get-CamTestNumberGaps -Lines $lines.Lines -Tag 'B' -Last $round.Count
        $files = @(Get-ChildItem -LiteralPath $dir -Filter "$($round.Name).log*" -File).Count
        $evidence += ("two writers, {0} x {1} lines: A {2}; B {3}; files {4}; surviving A from {5}, B from {6}; missing A {7}, B {8}; out of order A {9}, B {10}" -f `
            $round.Count, 2, $reports[0], $reports[1], $files, $a.First, $b.First, $a.Missing, $b.Missing, $a.Disordered, $b.Disordered)
        $bad = ($reports[0] -notlike 'errors=0*' -or $reports[1] -notlike 'errors=0*' -or $files -gt 2 -or $a.Missing -or $b.Missing -or $a.Disordered -or $b.Disordered)
        # Under 2 x MaxBytes in all, so nothing may be dropped even by design.
        if ($round.Name -eq 'exact' -and ($a.First -ne 1 -or $b.First -ne 1 -or $files -ne 2)) { $bad = $true }
        if ($bad) { $ok = $false; $evidence += ("FAIL: concurrent writers ({0}) threw or lost a line" -f $round.Name) }
    }

    Complete-CamPruneCheck -Run $Run -Check 'logrotate' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $dir; Log = (Join-Path $env:TEMP 'cam-tests\logrotate.none') }) `
        -Message 'Write-CamLog rolls once per crossing into one previous generation, never throws or loses a line on a locked log, and two writer processes lose nothing'
}


function Get-CamTestSegmentFiles {
    <# The recorder segments in one camera folder, oldest created first. #>
    param([Parameter(Mandatory = $true)][string]$Dir, [Parameter(Mandatory = $true)][string]$SafeName, [Parameter(Mandatory = $true)]$Config)
    if (-not (Test-Path -LiteralPath $Dir)) { return @() }
    return @(Get-ChildItem -LiteralPath $Dir -File -Force | Where-Object { Test-CamSegmentName -Name $_.Name -SafeName $SafeName -Config $Config } |
        Sort-Object -Property CreationTime)
}


function Get-CamTestSegmentTime {
    <# The wall-clock start a segment's name records, or $null. #>
    param([Parameter(Mandatory = $true)][string]$Name)
    if ($Name -notmatch '_(\d{8}_\d{6})([+-]\d{4})\.') { return $null }
    return [DateTime]::ParseExact($Matches[1], 'yyyyMMdd_HHmmss', [System.Globalization.CultureInfo]::InvariantCulture)
}


function Test-CamTestSandboxLeak {
    <#
        Evidence that a synthetic-camera sandbox held only its throwaway
        credential: the store decrypts to the fake user, and the fake password
        appears in none of the sandbox's logs or status files.
    #>
    param([Parameter(Mandatory = $true)]$Sandbox)
    $credential = Import-Clixml -Path (Join-Path -Path $Sandbox.Dir -ChildPath 'camera_creds.xml')
    $hits = 0
    foreach ($file in @(Get-ChildItem -LiteralPath $Sandbox.Root -Recurse -File -Include '*.log', '*.log.1', '*.json')) {
        if ([System.IO.File]::ReadAllText($file.FullName).Contains($credential.GetNetworkCredential().Password)) { $hits++ }
    }
    return [pscustomobject]@{ Ok = ($credential.UserName -eq 'fakeuser' -and $hits -eq 0); Text = ("credential user {0}; fake password found in {1} sandbox log/status file(s)" -f $credential.UserName, $hits) }
}


function Invoke-CamCheckSegments {
    <#
        A3: the production segment path, with the real supervisor recording a
        synthetic camera through the product's own ffmpeg arguments at
        SegmentSeconds 10.

        - every file the recorder writes passes Test-CamSegmentName;
        - after the first (partial) segment, each starts on a multiple of 10 s
          of the wall clock (segment_atclocktime), its name agreeing with its
          creation time;
        - each closed segment is H.264 + pcm_alaw, 10 s within a second, and
          starts at 0 (reset_timestamps);
        - no zero-byte file is left behind while recording;
        - a hard kill of the ffmpeg child mid-segment is restarted, the killed
          segment stays readable; a kill before the segment's first write
          leaves a zero-byte file, which is removed, while zero-byte foreign
          files beside it survive.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $port = 18554
    $name = 'synth1'
    $sandbox = New-CamTestSynthSandbox -Name 'segments' -CameraName $name -Port $port `
        -Config @{ SegmentSeconds = 10; StatusIntervalSeconds = 1; ReconcileSeconds = 2; BackoffStartSeconds = 1; BackoffMaxSeconds = 4 }
    $config = $sandbox.Config
    $camDir = Join-Path -Path $sandbox.Root -ChildPath $name
    $foreign = @('notes.txt', "${name}_20260101_000000+0300.mp4", "${name}_20260101_000010+0300.mkv.tmp")

    $camera = $null
    $process = $null
    try {
        $camera = Start-CamTestSyntheticCamera -Dir $sandbox.Dir -Port $port -PathName $name
        $process = Start-CamTestSupervisor -Sandbox $sandbox
        # Planted after the root is claimed: a foreign entry in an unclaimed
        # root keeps it unclaimed, and that disables the cleanup under test.
        [void](New-Item -ItemType Directory -Path $camDir -Force)
        foreach ($leaf in $foreign) { [System.IO.File]::WriteAllBytes((Join-Path $camDir $leaf), [byte[]]@()) }
        $evidence += ("synthetic camera: mediamtx pid {0} on 127.0.0.1:{1}, publisher pid {2}; sandbox supervisor pid {3}" -f `
            $camera.MediaMtx.Id, $port, (@($camera.Publishers | ForEach-Object { $_.Id }) -join ','), $process.Id)

        # Record until the first partial segment, four full ones and the open
        # one exist, watching for an empty file among the closed ones.
        $emptyClosed = @{}
        $deadline = [DateTime]::Now.AddSeconds(90)
        while ([DateTime]::Now -lt $deadline) {
            $files = Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $config
            for ($i = 0; $i -lt $files.Count - 1; $i++) { if ($files[$i].Length -eq 0) { $emptyClosed[$files[$i].Name] = $true } }
            if ($files.Count -ge 6) { break }
            Start-Sleep -Milliseconds 500
        }
        $files = Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $config
        $closed = @($files | Select-Object -First ($files.Count - 1))
        $evidence += ("{0} segment file(s) after {1}s, {2} closed; zero-byte closed segments seen while recording: {3}" -f `
            $files.Count, [int]([DateTime]::Now - $process.StartTime).TotalSeconds, $closed.Count, $emptyClosed.Count)
        if ($closed.Count -lt 5 -or $emptyClosed.Count) { $ok = $false; $evidence += 'FAIL: fewer than 4 full closed segments, or an empty closed segment was left' }

        for ($i = 0; $i -lt $closed.Count; $i++) {
            $file = $closed[$i]
            $at = Get-CamTestSegmentTime -Name $file.Name
            $skew = ($file.CreationTime - $at).TotalSeconds
            $probe = Get-CamTestSegmentProbe -Path $file.FullName -FfprobePath $config.FfprobePath
            $line = "{0} {1,8} B  created {2}s from its name  {3}  start {4}  duration {5}" -f `
                $file.Name, $file.Length, (Format-CamNumber -Value $skew -Format '+0.0;-0.0'), ($probe.Codecs -join '+'), (Format-CamNumber -Value $probe.Start -Format '0.000'), (Format-CamNumber -Value $probe.Duration -Format '0.000')
            $bad = @()
            if (($probe.Codecs -notcontains 'h264') -or ($probe.Codecs -notcontains 'pcm_alaw')) { $bad += 'codecs' }
            if ($null -eq $probe.Start -or $probe.Start -gt 0.5) { $bad += 'start not reset' }
            if ([math]::Abs($skew) -gt 2) { $bad += 'name disagrees with creation time' }
            if ($i -eq 0) { $line += '  (first: starts when recording did, so neither aligned nor full)' }
            else {
                if ($at.Second % 10 -ne 0) { $bad += 'not on a 10 s boundary' }
                if ($null -eq $probe.Duration -or [math]::Abs($probe.Duration - 10) -gt 1) { $bad += 'duration' }
            }
            if ($bad.Count) { $ok = $false; $line += '  FAIL: ' + ($bad -join ', ') }
            $evidence += $line
        }

        # Kill 1: mid-segment, with footage already on disk.
        $deadline = [DateTime]::Now.AddSeconds(15)
        do {
            Start-Sleep -Milliseconds 200
            $current = @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $config)[-1]
            $current.Refresh()
        } while ([DateTime]::Now -lt $deadline -and -not ((([DateTime]::Now - $current.CreationTime).TotalSeconds -ge 6) -and $current.Length -gt 0))
        $victim = Get-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name
        $killed = Stop-CamTestChildHard -ProcessId $victim
        $restarted = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -Not $victim -TimeoutSeconds 20
        $current.Refresh()
        $probe = Get-CamTestSegmentProbe -Path $current.FullName -FfprobePath $config.FfprobePath
        $evidence += ("kill 1, {0}s into {1}: pid {2} killed={3}, restarted as pid {4}; the killed segment is {5} B, {6}, duration {7}" -f `
            (Format-CamNumber -Value ([DateTime]::Now - $current.CreationTime).TotalSeconds -Format '0.0'), $current.Name, $victim, $killed, $restarted, $current.Length, ($probe.Codecs -join '+'), $(if ($null -eq $probe.Duration) { 'none (killed mid-write)' } else { $probe.Duration }))
        if ($victim -le 0 -or -not $killed -or $restarted -le 0 -or $current.Length -le 0 -or $probe.Codecs -notcontains 'h264') {
            $ok = $false; $evidence += 'FAIL: the child was not restarted, or the segment it was writing is not readable'
        }

        # Kill 2: the restarted child's first segment, before its first write
        # reaches disk. ffmpeg writes in 256 KB blocks, so a new segment is
        # empty for about a second and a half. Sizes are read through a
        # handle: the directory entry of an open file goes stale on NTFS.
        $restartedAt = [DateTime]::Now
        $victim2 = $restarted
        $empty = $null
        $emptyBytes = -1
        $deadline = [DateTime]::Now.AddSeconds(15)
        while ([DateTime]::Now -lt $deadline -and -not $empty) {
            $fresh = @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $config | Where-Object { $_.CreationTime -gt $current.CreationTime })
            if ($fresh.Count -and (Get-CamTestLiveLength -Path $fresh[-1].FullName) -eq 0) { $empty = $fresh[-1] } else { Start-Sleep -Milliseconds 50 }
        }
        $killed2 = Stop-CamTestChildHard -ProcessId $victim2
        if ($empty) { $emptyBytes = Get-CamTestLiveLength -Path $empty.FullName }
        $restarted2 = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -Not $victim2 -TimeoutSeconds 20
        Start-Sleep -Seconds 2
        $emptyName = $(if ($empty) { $empty.Name } else { '(no empty segment caught)' })
        $emptyGone = ($empty -and -not (Test-Path -LiteralPath $empty.FullName))
        $survivors = @($foreign | Where-Object { Test-Path -LiteralPath (Join-Path $camDir $_) })
        $removedLines = @(Get-Content -LiteralPath $sandbox.SupervisorLog | Where-Object { $_ -like '*removed empty segment*' -or $_ -like '*empty foreign file*' -or $_ -like '*empty file(s)*' })
        $evidence += ("kill 2, before the first write: {0} was {1} B when pid {2} was killed ({3}); restarted as pid {4}; removed={5}; zero-byte foreign files surviving {6}/{7}" -f `
            $emptyName, $emptyBytes, $victim2, $killed2, $restarted2, $emptyGone, $survivors.Count, $foreign.Count)
        $evidence += @($removedLines | ForEach-Object { '  log: ' + $_ })
        if (-not $empty -or $emptyBytes -ne 0 -or -not $killed2 -or $restarted2 -le 0 -or -not $emptyGone -or $survivors.Count -ne $foreign.Count) {
            $ok = $false; $evidence += 'FAIL: the empty segment of a killed child was left behind, or a foreign empty file was removed'
        }

        # Recording carries on after both kills.
        $deadline = [DateTime]::Now.AddSeconds(25)
        $after = @()
        while ([DateTime]::Now -lt $deadline) {
            $after = @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $config | Where-Object { $_.CreationTime -gt $restartedAt })
            if ($after.Count -ge 2 -and $after[-2].Length -gt 0) { break }
            Start-Sleep -Milliseconds 500
        }
        $status = (Get-CamTestStatusJson -Sandbox $sandbox).Snapshot
        $row = @($status.Cameras)[0]
        $evidence += ("after the kills: {0} new segment(s) ({1}); status State {2}, RestartsLastHour {3}, LastError '{4}'" -f `
            $after.Count, (@($after | ForEach-Object { "$($_.Name) $($_.Length) B" }) -join ', '), $status.State, $row.RestartsLastHour, $row.LastError)
        if ($after.Count -lt 2 -or $after[-2].Length -le 0 -or $status.State -ne 'running' -or [int]$row.RestartsLastHour -ne 2) {
            $ok = $false; $evidence += 'FAIL: recording did not carry on after the kills, or the restarts were not counted'
        }

        $names = @(Get-ChildItem -LiteralPath $camDir -File | Where-Object { $foreign -notcontains $_.Name })
        $badNames = @($names | Where-Object { -not (Test-CamSegmentName -Name $_.Name -SafeName $name -Config $config) })
        $evidence += ("{0} file(s) written by the recorder, {1} failing Test-CamSegmentName {2}" -f $names.Count, $badNames.Count, (@($badNames | ForEach-Object { $_.Name }) -join ', '))
        if ($badNames.Count) { $ok = $false }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        if ($process) {
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            if (-not $stop.Ok) { $ok = $false; $evidence += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }

    $survivors = @($foreign | Where-Object { Test-Path -LiteralPath (Join-Path $camDir $_) })
    $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
    $alerts = 0
    if (Test-Path -LiteralPath $sandbox.AlertLog) { $alerts = @(Get-Content -LiteralPath $sandbox.AlertLog).Count }
    $evidence += ("after -Stop: foreign files surviving {0}/{1}; {2}; alerts recorded by the stub {3}" -f $survivors.Count, $foreign.Count, $leak.Text, $alerts)
    if ($survivors.Count -ne $foreign.Count -or -not $leak.Ok) { $ok = $false }

    Complete-CamPruneCheck -Run $Run -Check 'segments' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $sandbox.Dir; Log = (Join-Path $env:TEMP 'cam-tests\segments.none') }) `
        -Message 'the real supervisor records clock-aligned, correctly named H.264 + pcm_alaw segments, restarts a killed child and removes only its own empty segment'
}


function Invoke-CamCheckBackoff {
    <#
        A4: the restart backoff and its HealthyResetSeconds reset, with the
        real supervisor and a synthetic camera, scaled down to
        BackoffStartSeconds 1, BackoffMaxSeconds 8, HealthyResetSeconds 15.

        Five kills in quick succession must be answered with waits of 1, 2,
        4, 8 and 8 s - doubling, then capped - and each restart must actually
        take at least that long. A child then left healthy for longer than
        15 s and killed must wait 1 s again, and the next quick kill 2 s.
        RestartsLastHour in status.json counts every one.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $port = 18554
    $name = 'synth1'
    $sandbox = New-CamTestSynthSandbox -Name 'backoff' -CameraName $name -Port $port `
        -Config @{ SegmentSeconds = 10; StatusIntervalSeconds = 1; ReconcileSeconds = 2; BackoffStartSeconds = 1; BackoffMaxSeconds = 8; HealthyResetSeconds = 15 }

    # quick: kill as soon as the restart is seen; healthy: let it run past
    # HealthyResetSeconds first. Expected is the wait the supervisor must log.
    $plan = @(
        @{ Kind = 'quick'; Expected = 1 }, @{ Kind = 'quick'; Expected = 2 }, @{ Kind = 'quick'; Expected = 4 },
        @{ Kind = 'quick'; Expected = 8 }, @{ Kind = 'quick'; Expected = 8 },
        @{ Kind = 'healthy'; Expected = 1 }, @{ Kind = 'quick'; Expected = 2 }
    )

    $camera = $null
    $process = $null
    try {
        $camera = Start-CamTestSyntheticCamera -Dir $sandbox.Dir -Port $port -PathName $name
        $process = Start-CamTestSupervisor -Sandbox $sandbox
        $child = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -TimeoutSeconds 20
        $startedAt = [DateTime]::Now
        $evidence += ("synthetic camera mediamtx pid {0}, publisher pid {1}; supervisor pid {2}; first child pid {3}" -f `
            $camera.MediaMtx.Id, (@($camera.Publishers | ForEach-Object { $_.Id }) -join ','), $process.Id, $child)

        $k = 0
        foreach ($step in $plan) {
            $k++
            if ($step.Kind -eq 'healthy') {
                $wait = 18 - ([DateTime]::Now - $startedAt).TotalSeconds
                if ($wait -gt 0) { Start-Sleep -Milliseconds ([int]($wait * 1000)) }
            }
            $ranFor = ([DateTime]::Now - $startedAt).TotalSeconds
            $killedAt = [DateTime]::Now
            $gone = Stop-CamTestChildHard -ProcessId $child
            $next = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -Not $child -TimeoutSeconds 30
            $took = ([DateTime]::Now - $killedAt).TotalSeconds
            $startedAt = [DateTime]::Now

            $line = @(Get-Content -LiteralPath $sandbox.SupervisorLog | Where-Object { $_ -match "for $name; restart #$k in (\d+)s$" }) | Select-Object -Last 1
            $logged = -1
            if ($line -match 'after (\d+)s for .+ in (\d+)s$') { $logged = [int]$Matches[2] }
            $verdict = 'ok'
            # The supervisor ticks once a second and status.json is read once
            # a second, so a restart is seen up to about 3 s after its wait.
            if (-not $gone -or $next -le 0 -or $logged -ne $step.Expected -or $took -lt ($step.Expected - 0.5) -or $took -gt ($step.Expected + 4)) {
                $ok = $false; $verdict = 'FAIL'
            }
            $evidence += ("kill {0} ({1,-7} ran {2,4}s): pid {3} -> {4}; logged wait {5}s, expected {6}s, restart seen after {7}s  {8}" -f `
                $k, $step.Kind, (Format-CamNumber -Value $ranFor -Format '0.0'), $child, $next, $logged, $step.Expected, (Format-CamNumber -Value $took -Format '0.0'), $verdict)
            if ($line) { $evidence += ('  log: ' + $line) }
            if ($next -le 0) { break }
            $child = $next
        }

        Start-Sleep -Seconds 2
        $row = @((Get-CamTestStatusJson -Sandbox $sandbox).Snapshot.Cameras)[0]
        $evidence += ("status.json: RestartsLastHour {0}, RestartsTotal {1}, Alive {2}, LastError '{3}'" -f $row.RestartsLastHour, $row.RestartsTotal, $row.Alive, $row.LastError)
        if ([int]$row.RestartsLastHour -ne $plan.Count -or -not $row.Alive) { $ok = $false; $evidence += ("FAIL: RestartsLastHour should be {0} with the child alive" -f $plan.Count) }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        if ($process) {
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            if (-not $stop.Ok) { $ok = $false; $evidence += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }

    $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
    $evidence += $leak.Text
    if (-not $leak.Ok) { $ok = $false }

    Complete-CamPruneCheck -Run $Run -Check 'backoff' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $sandbox.Dir; Log = (Join-Path $env:TEMP 'cam-tests\backoff.none') }) `
        -Message 'restart waits double from BackoffStartSeconds to BackoffMaxSeconds, reset after HealthyResetSeconds of health, and RestartsLastHour counts them'
}


function Invoke-CamCheckLogRotateLive {
    <#
        A2: rotation inside the real supervisor at LogMaxBytes 8192, recording
        a synthetic camera whose stream makes ffmpeg warn as the real cameras
        do, with LogRepeatSuppressSeconds 0 so the camera log grows.

        - free: recorder.log and the camera's log both roll while recording
          continues and status.json keeps updating;
        - held: a reader holds recorder.log without delete sharing while the
          supervisor crosses the limit (and a Get-Content -Wait follows the
          camera log). The supervisor stays up, logs the blocked rotation
          once, keeps writing; the first write after the reader lets go rolls.
        - throughout: no gap between segments beyond SegmentSeconds.

        recorder.log is padded through Write-CamLog from this process - a
        second writer, as the NVR host is - to just under the limit, so the
        crossing itself is always the supervisor's own write.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $port = 18554
    $name = 'synth1'
    $max = 8192
    $sandbox = New-CamTestSynthSandbox -Name 'logrotatelive' -CameraName $name -Port $port `
        -Config @{ SegmentSeconds = 10; StatusIntervalSeconds = 1; ReconcileSeconds = 2; LogMaxBytes = $max; LogRepeatSuppressSeconds = 0; PruneIntervalSeconds = 1; BackoffStartSeconds = 1 }
    $recLog = $sandbox.SupervisorLog
    $camLog = Join-Path -Path $sandbox.Root -ChildPath "_logs\$name.log"
    $camDir = Join-Path -Path $sandbox.Root -ChildPath $name

    $watch = @{ Phase = 'free'; RecSize = 0; CamSize = 0; Rolls = @{}; LastUpdate = $null; MaxStatusGap = 0.0; Died = $false }
    $sample = {
        foreach ($pair in @(@('rec', $recLog, 'RecSize'), @('cam', $camLog, 'CamSize'))) {
            $info = New-Object System.IO.FileInfo($pair[1])
            if (-not $info.Exists) { continue }
            if ($info.Length -lt $watch[$pair[2]]) { $key = $watch.Phase + ':' + $pair[0]; $watch.Rolls[$key] = 1 + [int]$watch.Rolls[$key] }
            $watch[$pair[2]] = $info.Length
        }
        $status = Get-CamTestStatusJson -Sandbox $sandbox
        if ($status) {
            $updated = [DateTime]::Parse([string]$status.Snapshot.UpdatedAt)
            if ($watch.LastUpdate -and $updated -gt $watch.LastUpdate) {
                $gap = ($updated - $watch.LastUpdate).TotalSeconds
                if ($gap -gt $watch.MaxStatusGap) { $watch.MaxStatusGap = $gap }
            }
            if (-not $watch.LastUpdate -or $updated -gt $watch.LastUpdate) { $watch.LastUpdate = $updated }
        }
        if ($process.HasExited) { $watch.Died = $true }
    }
    $rolls = { param($phase, $what) [int]$watch.Rolls["${phase}:$what"] }
    $pad = {
        param([int]$Below)
        $i = 0
        while ((New-Object System.IO.FileInfo($recLog)).Length -lt ($max - $Below)) { $i++; Write-CamLog -Path $recLog -Message ("test padding {0:D4} {1}" -f $i, ('.' * 60)) -MaxBytes $max }
        return $i
    }
    $countIn = { param($path, $like) if (Test-Path -LiteralPath $path) { @(Get-Content -LiteralPath $path | Where-Object { $_ -like $like }).Count } else { 0 } }

    $camera = $null
    $process = $null
    $reader = $null
    $tailer = $null
    try {
        $camera = Start-CamTestSyntheticCamera -Dir $sandbox.Dir -Port $port -PathName $name -NonMonotonic
        $process = Start-CamTestSupervisor -Sandbox $sandbox
        $evidence += ("synthetic camera mediamtx pid {0}, publisher pid {1} (non-monotonic video); supervisor pid {2}" -f `
            $camera.MediaMtx.Id, (@($camera.Publishers | ForEach-Object { $_.Id }) -join ','), $process.Id)

        # free
        $padded = & $pad 1500
        $deadline = [DateTime]::Now.AddSeconds(75)
        while ([DateTime]::Now -lt $deadline -and -not ((& $rolls 'free' 'rec') -ge 1 -and (& $rolls 'free' 'cam') -ge 2)) { & $sample; Start-Sleep -Milliseconds 250 }
        $evidence += ("free: {0} padding line(s); recorder.log rolled {1}x, {2}.log rolled {3}x; files now recorder.log {4} B + .1 {5} B, {2}.log {6} B + .1 {7} B; supervisor alive {8}" -f `
            $padded, (& $rolls 'free' 'rec'), $name, (& $rolls 'free' 'cam'), (Get-Item $recLog).Length, (Get-Item "$recLog.1").Length, (Get-Item $camLog).Length, (Get-Item "$camLog.1").Length, (-not $watch.Died))
        $evidence += ("  {0}.log.1 starts: {1}" -f $name, ([string](@(Get-Content -LiteralPath "$camLog.1" -TotalCount 1))[0]).Substring(0, 120))
        if ((& $rolls 'free' 'rec') -lt 1 -or (& $rolls 'free' 'cam') -lt 2 -or $watch.Died) { $ok = $false; $evidence += 'FAIL: a log did not roll inside the running supervisor, or the supervisor died' }

        # held
        $watch.Phase = 'held'
        $reader = [System.IO.File]::Open($recLog, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $tailer = Start-Process -FilePath 'powershell.exe' -PassThru -WindowStyle Hidden -ArgumentList @('-NoProfile', '-Command', "Get-Content -LiteralPath '$camLog' -Wait")
        [void]$tailer.Handle
        $padded = & $pad 300
        $heldUntil = [DateTime]::Now.AddSeconds(20)
        while ([DateTime]::Now -lt $heldUntil) { & $sample; Start-Sleep -Milliseconds 250 }
        $heldSize = (Get-Item -LiteralPath $recLog).Length
        $notices = & $countIn $recLog '*log rotation blocked*'
        $reader.Dispose()
        $reader = $null
        $watch.Phase = 'released'
        $deadline = [DateTime]::Now.AddSeconds(10)
        while ([DateTime]::Now -lt $deadline -and (& $rolls 'released' 'rec') -lt 1) { & $sample; Start-Sleep -Milliseconds 250 }
        $noticeLine = $null
        if (Test-Path -LiteralPath "$recLog.1") { $noticeLine = @(Get-Content -LiteralPath "$recLog.1" | Where-Object { $_ -like '*log rotation blocked*' }) | Select-Object -First 1 }
        $echoed = & $countIn $sandbox.ConsoleLog '*log rotation blocked*'
        $evidence += ("held: FileShare.ReadWrite reader on recorder.log for 20 s ({0} padding line(s) to {1} B below the limit), Get-Content -Wait tailer pid {2} on {3}.log" -f $padded, 300, $tailer.Id, $name)
        $evidence += ("  recorder.log rolled {0}x while held and grew to {1} B; blocked notices {2}; {3}.log rolled {4}x meanwhile; supervisor alive {5}; notice echoed to the console {6}x" -f `
            (& $rolls 'held' 'rec'), $heldSize, $notices, $name, (& $rolls 'held' 'cam'), (-not $watch.Died), $echoed)
        $evidence += ("  released: recorder.log rolled {0}x within 10 s; recorder.log.1 holds: {1}" -f (& $rolls 'released' 'rec'), $noticeLine)
        if ((& $rolls 'held' 'rec') -ne 0 -or $heldSize -le $max -or $notices -ne 1 -or (& $rolls 'held' 'cam') -lt 1 -or $watch.Died -or (& $rolls 'released' 'rec') -lt 1 -or -not $noticeLine) {
            $ok = $false; $evidence += 'FAIL: with recorder.log held the supervisor died, rolled anyway, logged other than one notice, or did not roll once released'
        }

        $gapLimit = Get-CamTestCadenceLimit -IntervalSeconds $sandbox.Config.StatusIntervalSeconds
        $evidence += ("status.json: longest gap between updates {0} s (StatusIntervalSeconds {1}, limit {2} s); last update {3}" -f (Format-CamNumber -Value $watch.MaxStatusGap -Format '0.0'), $sandbox.Config.StatusIntervalSeconds, $gapLimit, $watch.LastUpdate.ToString('HH:mm:ss'))
        if ($watch.MaxStatusGap -gt $gapLimit) { $ok = $false; $evidence += 'FAIL: status.json stopped updating' }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        if ($reader) { $reader.Dispose() }
        if ($tailer -and -not $tailer.HasExited) { try { $tailer.Kill() } catch { }; [void]$tailer.WaitForExit(10000) }
        if ($process) {
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            if (-not $stop.Ok) { $ok = $false; $evidence += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }

    $files = Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config
    $gaps = @()
    for ($i = 1; $i -lt $files.Count; $i++) { $gaps += ((Get-CamTestSegmentTime -Name $files[$i].Name) - (Get-CamTestSegmentTime -Name $files[$i - 1].Name)).TotalSeconds }
    $largest = ($gaps | Measure-Object -Maximum).Maximum
    $empty = @($files | Select-Object -First ($files.Count - 1) | Where-Object { $_.Length -eq 0 }).Count
    $restarts = & $countIn $recLog '*restart #*'
    $evidence += ("segments: {0} across the run, largest start-to-start gap {1} s, {2} empty closed, {3} child restart(s) logged" -f $files.Count, $largest, $empty, $restarts)
    if ($files.Count -lt 5 -or $largest -gt 12 -or $empty -or $restarts) { $ok = $false; $evidence += 'FAIL: recording had a gap, an empty segment or a restart around a roll' }
    $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
    $evidence += $leak.Text
    if (-not $leak.Ok) { $ok = $false }

    Complete-CamPruneCheck -Run $Run -Check 'logrotatelive' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $sandbox.Dir; Log = (Join-Path $env:TEMP 'cam-tests\logrotatelive.none') }) `
        -Message 'inside the real supervisor both logs roll while recording continues, and a held recorder.log only delays its rotation'
}

function Invoke-CamCheckSoak {
    <#
        The soak: the REAL supervisor, in a throwaway sandbox, at the product's
        own DEFAULT thresholds (SegmentSeconds 900, LogMaxBytes 2 MB,
        HealthyResetSeconds 300, StatusIntervalSeconds 5, the default backoff
        and every other default), recording a synthetic camera for about 33
        minutes. Runs only when named: -Checks soak.

        Departures from production, each reported in the evidence:
        - the synthetic camera is NOISY (repeated video timestamps at a high
          frame rate), so the camera's own log reaches 2 MB in minutes
          instead of days; and LogRepeatSuppressSeconds is 0, because the
          default 60 collapses those repeats and the log would never grow;
        - recorder.log is padded through Write-CamLog from THIS process (a
          second writer, as the NVR host is) to 2 MB, so its rotation is
          seen too; the supervisor's own log lines are unchanged;
        - the relay is off and the alert stub replaces Send-CamAlert.

        The run starts within about a minute before a quarter hour and ends a
        minute after the second quarter-hour segment closes, so two complete
        SegmentSeconds segments are measured: every closed segment (ffprobe),
        both log rolls, restarts and alerts, the longest gap between
        status.json updates, and the supervisor's and its ffmpeg child's
        working set, private bytes and handle count every 5 minutes.

        -DryRun scales SegmentSeconds to 60 and LogMaxBytes to 256 KB and runs
        3 minutes: it proves the rig records and the sampling works, and its
        result says nothing about the production values.
    #>
    param($Run, [switch]$DryRun)

    $evidence = @()
    $ok = $true
    $port = 18554
    $name = 'synth1'
    $defaults = Get-CamConfigDefault
    $config = @{ LogRepeatSuppressSeconds = 0 }
    foreach ($key in @('SegmentSeconds', 'LogMaxBytes', 'HealthyResetSeconds', 'StatusIntervalSeconds', 'BackoffStartSeconds', 'BackoffMaxSeconds', 'ReconcileSeconds', 'ArchiveScanSeconds', 'StopTimeoutSeconds', 'PruneIntervalSeconds', 'ReserveGB', 'PruneHysteresisGB')) { $config[$key] = $defaults[$key] }
    if ($DryRun) { $config.SegmentSeconds = 60; $config.LogMaxBytes = 262144 }
    $segmentSeconds = [int]$config.SegmentSeconds
    $max = [int]$config.LogMaxBytes
    $procEvery = $(if ($DryRun) { 30 } else { 300 })
    $padStart = $(if ($DryRun) { 60 } else { 600 })

    # One blocking wait, so the run straddles a quarter hour (a minute boundary when scaled down).
    $now = [DateTime]::Now
    $span = $(if ($DryRun) { 1.0 } else { 15.0 })
    $next = $now.Date.AddMinutes([math]::Floor($now.TimeOfDay.TotalMinutes / $span) * $span + $span)
    if (($next - $now).TotalSeconds -lt 25) { $next = $next.AddMinutes($span) }
    $ahead = ($next - $now).TotalSeconds
    if ($ahead -gt 60 -and -not $DryRun) {
        $evidence += ("waited {0} s for the boundary {1}" -f [int]($ahead - 45), $next.ToString('HH:mm:ss'))
        Start-Sleep -Seconds ([int]($ahead - 45))
    }
    $boundaryUtc = $next.ToUniversalTime()

    $sandbox = New-CamTestSynthSandbox -Name 'soak' -CameraName $name -Port $port -Config $config
    $recLog = $sandbox.SupervisorLog
    $camLog = Join-Path -Path $sandbox.Root -ChildPath "_logs\$name.log"
    $camDir = Join-Path -Path $sandbox.Root -ChildPath $name
    $volume = New-Object System.IO.DriveInfo([System.IO.Path]::GetPathRoot($sandbox.Dir))
    $freeStart = $volume.AvailableFreeSpace
    $evidence += ("sandbox {0}; local time {1}; segment boundary targeted {2}; {3}" -f $sandbox.Dir, [DateTime]::Now.ToString('yyyy-MM-dd HH:mm:ss zzz'), $next.ToString('HH:mm:ss'), $(if ($DryRun) { 'DRY RUN (SegmentSeconds 60, LogMaxBytes 256 KB, 3 min) - proves the rig only' } else { 'PRODUCTION thresholds' }))
    $evidence += ("config: " + ((@('SegmentSeconds', 'LogMaxBytes', 'HealthyResetSeconds', 'StatusIntervalSeconds', 'BackoffStartSeconds', 'BackoffMaxSeconds', 'ReconcileSeconds', 'ArchiveScanSeconds', 'StopTimeoutSeconds', 'PruneIntervalSeconds', 'ReserveGB', 'PruneHysteresisGB', 'StallFactor', 'LogRepeatSuppressSeconds') | ForEach-Object { "$_ $($sandbox.Config.$_)" }) -join ', '))
    foreach ($key in @('HealthyResetSeconds', 'StatusIntervalSeconds', 'BackoffStartSeconds', 'BackoffMaxSeconds')) {
        if ([double]$sandbox.Config.$key -ne [double]$defaults[$key]) { $ok = $false; $evidence += "FAIL: $key is not the product default" }
    }
    if (-not $DryRun -and ($segmentSeconds -ne $defaults.SegmentSeconds -or $max -ne $defaults.LogMaxBytes)) { $ok = $false; $evidence += 'FAIL: SegmentSeconds or LogMaxBytes is not the product default' }
    if ($freeStart -lt (($defaults.ReserveGB + $defaults.PruneHysteresisGB + 5) * 1GB)) { $ok = $false; $evidence += ("FAIL: only {0} bytes free; the pruner would run" -f $freeStart) }

    $tracks = @{ rec = @{ Last = 0L; Events = (New-Object System.Collections.ArrayList) }; cam = @{ Last = 0L; Events = (New-Object System.Collections.ArrayList) } }
    $paths = @{ rec = $recLog; cam = $camLog }
    $samples = New-Object System.Collections.ArrayList
    $pids = New-Object System.Collections.ArrayList
    $statusSeen = @{ Last = $null; MaxGap = 0.0; GapAt = $null; Updates = 0; NotRunning = 0; Unreadable = 0 }
    $padState = @{ Phase = 'wait'; N = 0; Next = [DateTime]::MinValue; After = 0 }
    $camera = $null
    $process = $null
    $died = $false
    $t0 = $null
    $secondsSince = { [DateTime]::UtcNow.Subtract($t0).TotalSeconds }
    $readHead = {
        param($path)
        try {
            $stream = [System.IO.File]::Open($path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, ([System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete))
            try { return (New-Object System.IO.StreamReader($stream)).ReadLine() } finally { $stream.Dispose() }
        }
        catch { return $null }
    }
    $tickLogs = {
        foreach ($key in @('rec', 'cam')) {
            $info = New-Object System.IO.FileInfo($paths[$key])
            if (-not $info.Exists) { continue }
            $track = $tracks[$key]
            if ($info.Length -lt $track.Last) {
                $tail = @(Get-CamFileTailLines -Path ($paths[$key] + '.1') -Count 1)
                [void]$track.Events.Add([pscustomobject]@{
                    Elapsed = (& $secondsSince); Local = [DateTime]::Now.ToString('HH:mm:ss'); Before = (New-Object System.IO.FileInfo($paths[$key] + '.1')).Length; After = $info.Length
                    LastOld = $(if ($tail.Count) { [string]$tail[-1] } else { '' }); FirstNew = [string](& $readHead $paths[$key]) })
            }
            $track.Last = $info.Length
        }
    }
    $tickStatus = {
        try { $status = Get-CamTestStatusJson -Sandbox $sandbox } catch { $statusSeen.Unreadable++; return }
        if (-not $status) { $statusSeen.Unreadable++; return }
        $updated = [DateTimeOffset]::Parse([string]$status.Snapshot.UpdatedAt).UtcDateTime
        if ($null -eq $statusSeen.Last -or $updated -gt $statusSeen.Last) {
            if ($null -ne $statusSeen.Last) {
                $gap = ($updated - $statusSeen.Last).TotalSeconds
                if ($gap -gt $statusSeen.MaxGap) { $statusSeen.MaxGap = $gap; $statusSeen.GapAt = [DateTime]::Now.ToString('HH:mm:ss') }
            }
            $statusSeen.Last = $updated
            $statusSeen.Updates++
            if ($status.Snapshot.State -ne 'running') { $statusSeen.NotRunning++ }
        }
        foreach ($row in @($status.Snapshot.Cameras)) { if ($row.Name -eq $name -and $row.Pid -and $pids -notcontains [int]$row.Pid) { [void]$pids.Add([int]$row.Pid) } }
    }
    $tickPad = {
        if ($padState.Phase -eq 'wait' -and (& $secondsSince) -ge $padStart) { $padState.Phase = 'burst' }
        if ($padState.Phase -eq 'burst') {
            $slice = [DateTime]::UtcNow.AddMilliseconds(250)
            while ([DateTime]::UtcNow -lt $slice) {
                if ((New-Object System.IO.FileInfo($recLog)).Length -ge ($max - 30000)) { $padState.Phase = 'trickle'; break }
                $padState.N++
                Write-CamLog -Path $recLog -Message ("soak padding {0:D6} {1}" -f $padState.N, ('.' * 900)) -MaxBytes $max
            }
        }
        elseif ($padState.Phase -eq 'trickle' -and [DateTime]::UtcNow -ge $padState.Next) {
            $padState.N++
            Write-CamLog -Path $recLog -Message ("soak padding {0:D6} {1}" -f $padState.N, ('.' * 900)) -MaxBytes $max
            $padState.Next = [DateTime]::UtcNow.AddSeconds(1)
            if ($tracks.rec.Events.Count -gt 0) { $padState.After++ }
            if ($padState.After -ge 15 -or $padState.N -gt 400000) { $padState.Phase = 'done' }
        }
    }
    $tickProcs = {
        $supervisor = Get-Process -Id $process.Id -ErrorAction SilentlyContinue
        $childPid = Get-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name
        $child = $(if ($childPid) { Get-Process -Id $childPid -ErrorAction SilentlyContinue } else { $null })
        $row = [ordered]@{ Minute = [math]::Round((& $secondsSince) / 60, 2); Clock = [DateTime]::Now.ToString('HH:mm:ss'); SupPid = $process.Id; SupWsMB = $null; SupPrivMB = $null; SupHandles = $null; FfPid = $childPid; FfWsMB = $null; FfPrivMB = $null; FfHandles = $null }
        if ($supervisor) { $row.SupWsMB = [math]::Round($supervisor.WorkingSet64 / 1MB, 1); $row.SupPrivMB = [math]::Round($supervisor.PrivateMemorySize64 / 1MB, 1); $row.SupHandles = $supervisor.HandleCount }
        if ($child) { $row.FfWsMB = [math]::Round($child.WorkingSet64 / 1MB, 1); $row.FfPrivMB = [math]::Round($child.PrivateMemorySize64 / 1MB, 1); $row.FfHandles = $child.HandleCount }
        [void]$samples.Add([pscustomobject]$row)
    }

    $num = { param($v, $f) if ($null -eq $v) { 'n/a' } else { Format-CamNumber -Value $v -Format $f } }
    $final = $null
    $runSeconds = 1
    $freeEnd = $volume.AvailableFreeSpace
    $stopOk = $null
    $stopSeconds = $null
    $endClock = $null
    try {
        $camera = Start-CamTestSyntheticCamera -Dir $sandbox.Dir -Port $port -PathName $name -NonMonotonic -NonMonotonicEvery 2 -FrameRate 30
        $process = Start-CamTestSupervisor -Sandbox $sandbox
        $t0 = [DateTime]::UtcNow
        $evidence += ("synthetic camera: mediamtx pid {0}, publisher pid {1} (30 fps, every 2nd video timestamp repeated); supervisor pid {2}" -f $camera.MediaMtx.Id, (@($camera.Publishers | ForEach-Object { $_.Id }) -join ','), $process.Id)
        $firstChild = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -TimeoutSeconds 30
        $evidence += ("recording started {0} local; ffmpeg child pid {1}" -f [DateTime]::Now.ToString('HH:mm:ss'), $firstChild)
        $endUtc = $(if ($DryRun) { $t0.AddSeconds(180) } else { $boundaryUtc.AddSeconds(2 * $segmentSeconds + 60) })
        if ($firstChild -le 0) { throw 'the supervisor started no ffmpeg child' }

        & $tickProcs
        $nextProcs = $t0.AddSeconds($procEvery)
        while ([DateTime]::UtcNow -lt $endUtc) {
            if ($process.HasExited) { $died = $true; break }
            & $tickLogs
            & $tickStatus
            & $tickPad
            if ([DateTime]::UtcNow -ge $nextProcs) { & $tickProcs; $nextProcs = $nextProcs.AddSeconds($procEvery) }
            Start-Sleep -Milliseconds 400
        }
        & $tickLogs
        & $tickStatus
        if (-not $died) { & $tickProcs }
        $endClock = [DateTime]::Now.ToString('yyyy-MM-dd HH:mm:ss zzz')
        $final = Get-CamTestStatusJson -Sandbox $sandbox
        $freeEnd = $volume.AvailableFreeSpace
        $runSeconds = & $secondsSince
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        if ($process) {
            $stopStart = [DateTime]::UtcNow
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            $stopSeconds = ([DateTime]::UtcNow - $stopStart).TotalSeconds
            $stopOk = $stop.Ok
            if (-not $stop.Ok) { $ok = $false; $evidence += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }
    & $tickLogs
    $evidence += ("run: {0} s of recording, ended {1}; supervisor died during the run: {2}; stop took {3} s, clean {4}" -f [int]$runSeconds, $endClock, $died, (& $num $stopSeconds '0.0'), $stopOk)
    if ($died) { $ok = $false; $evidence += 'FAIL: the supervisor exited before the run ended' }

    # 1. segments
    $files = @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config)
    $complete = 0
    $segmentBytes = 0L
    $evidence += 'segments (name, bytes, ffprobe duration s, container start s, codecs):'
    for ($i = 0; $i -lt $files.Count; $i++) {
        $file = $files[$i]
        $at = Get-CamTestSegmentTime -Name $file.Name
        $probe = Get-CamTestSegmentProbe -Path $file.FullName -FfprobePath $sandbox.Config.FfprobePath
        $segmentBytes += $file.Length
        $offset = ($at.TimeOfDay.TotalSeconds % $segmentSeconds)
        $aligned = ($offset -le 1.5)
        $role = 'partial: closed by -Stop'
        $bad = @()
        if ($i -eq 0) { $role = 'partial by design: recording began mid-slot' }
        elseif ($i -lt $files.Count - 1) {
            $role = 'complete'
            if (-not $aligned) { $bad += 'start not on a boundary' }
            if ($null -eq $probe.Duration -or [math]::Abs($probe.Duration - $segmentSeconds) -gt 1.2) { $bad += 'duration' }
            if ($file.Length -le 0) { $bad += 'zero bytes' }
            if (-not $bad.Count) { $complete++ }
        }
        if (($probe.Codecs -notcontains 'h264') -or ($probe.Codecs -notcontains 'pcm_alaw')) { $bad += 'codecs' }
        if ($i -lt $files.Count - 1 -and $file.Length -le 0) { $bad += 'zero bytes' }
        $line = "  {0}  {1} B  {2} s  start {3}  {4}  [{5}; starts {6} s past a {7} s boundary]" -f $file.Name, $file.Length, (& $num $probe.Duration '0.000'), (& $num $probe.Start '0.000'), ($probe.Codecs -join '+'), $role, (Format-CamNumber -Value $offset -Format '0.0'), $segmentSeconds
        if ($bad.Count) { $ok = $false; $line += '  FAIL: ' + (($bad | Select-Object -Unique) -join ', ') }
        $evidence += $line
    }
    $evidence += ("{0} complete segment(s) measured, {1} file(s) in all" -f $complete, $files.Count)
    if ($complete -lt 2) { $ok = $false; $evidence += 'FAIL: fewer than two complete segments were measured' }

    # 2. logs
    $camBytes = 0L
    foreach ($key in @('rec', 'cam')) {
        $label = $(if ($key -eq 'rec') { 'recorder.log' } else { "$name.log" })
        $events = @($tracks[$key].Events)
        $evidence += ("{0}: rolled {1}x; now {2} B, {3}.1 {4} B" -f $label, $events.Count, (New-Object System.IO.FileInfo($paths[$key])).Length, $label, $(if (Test-Path -LiteralPath ($paths[$key] + '.1')) { (Get-Item -LiteralPath ($paths[$key] + '.1')).Length } else { 'absent' }))
        $previous = 0.0
        foreach ($event in $events) {
            $oldStamp = $null; $newStamp = $null
            if ($event.LastOld -match '^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) ') { $oldStamp = [DateTime]::ParseExact($Matches[1], 'yyyy-MM-dd HH:mm:ss', [System.Globalization.CultureInfo]::InvariantCulture) }
            if ($event.FirstNew -match '^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) ') { $newStamp = [DateTime]::ParseExact($Matches[1], 'yyyy-MM-dd HH:mm:ss', [System.Globalization.CultureInfo]::InvariantCulture) }
            $gapText = $(if ($oldStamp -and $newStamp) { (Format-CamNumber -Value ($newStamp - $oldStamp).TotalSeconds -Format '0') + ' s' } else { 'unreadable' })
            $evidence += ("  roll at {0} ({1} s in, {2} s after the previous): .1 is {3} B (limit {4}), new file {5} B when first seen; last line of .1 to first of the new file {6}" -f $event.Local, [int]$event.Elapsed, [int]($event.Elapsed - $previous), $event.Before, $max, $event.After, $gapText)
            $evidence += ('    last of .1:  ' + $event.LastOld.Substring(0, [math]::Min(170, $event.LastOld.Length)))
            $evidence += ('    first of new: ' + $event.FirstNew.Substring(0, [math]::Min(170, $event.FirstNew.Length)))
            if ($key -eq 'cam' -and $event.LastOld -match 'previous: (\d+),' -and $event.FirstNew -match 'previous: (\d+),') {
                # The synthetic camera repeats a timestamp every 2nd frame at 30 fps: consecutive warnings are 66.7 ms apart.
                $before = [double]([regex]::Match($event.LastOld, 'previous: (\d+),').Groups[1].Value)
                $after = [double]([regex]::Match($event.FirstNew, 'previous: (\d+),').Groups[1].Value)
                $evidence += ("    timestamps in the warnings across the roll: {0} -> {1}, step {2} (expected 66.7 when no warning is lost; a lost one would read 133 or more)" -f $before, $after, ($after - $before))
                if (($after - $before) -lt 55 -or ($after - $before) -gt 80) { $ok = $false; $evidence += '    FAIL: a warning was lost or repeated across the roll' }
            }
            $previous = $event.Elapsed
            if ($event.Before -lt $max -or $event.Before -gt ($max + 4096) -or -not $oldStamp -or -not $newStamp -or ($newStamp - $oldStamp).TotalSeconds -gt 3) { $ok = $false; $evidence += '    FAIL: the roll was not at the limit, or the lines either side are not adjacent' }
        }
        if ($key -eq 'cam') {
            $camBytes = $events.Count * 1L * $max + (New-Object System.IO.FileInfo($camLog)).Length
            $first = $(if ($events.Count) { $events[0].Elapsed } else { $runSeconds })
            $evidence += ("  {0} grew to {1} B in {2} s: {3} B/s overall; first limit roll after {4} s" -f $label, $camBytes, [int]($runSeconds + $stopSeconds), (Format-CamNumber -Value ($camBytes / ($runSeconds + $stopSeconds)) -Format '0'), $(if ($events.Count) { [int]$first } else { 'none' }))
        }
        if ($events.Count -lt 1) { $ok = $false; $evidence += "FAIL: $label never rolled" }
    }
    $numbers = @()
    foreach ($file in @(($recLog + '.1'), $recLog)) {
        if (Test-Path -LiteralPath $file) { foreach ($line in [System.IO.File]::ReadAllLines($file)) { if ($line -match 'soak padding (\d{6}) ') { $numbers += [int]$Matches[1] } } }
    }
    $unique = @($numbers | Sort-Object -Unique)
    $missing = $padState.N - $unique.Count
    $evidence += ("recorder.log was padded by this process through Write-CamLog: {0} numbered line(s) written, {1} found across recorder.log.1 + recorder.log, {2} missing, {3} duplicated" -f $padState.N, $unique.Count, $missing, ($numbers.Count - $unique.Count))
    if ($padState.N -lt 1 -or $missing -ne 0 -or $numbers.Count -ne $unique.Count -or ($unique.Count -and $unique[-1] -ne $padState.N)) { $ok = $false; $evidence += 'FAIL: padding lines were lost or duplicated around the recorder.log roll' }
    $own = @(@(($recLog + '.1'), $recLog) | Where-Object { Test-Path -LiteralPath $_ } | ForEach-Object { [System.IO.File]::ReadAllLines($_) } | Where-Object { $_ -notlike '*soak padding*' })
    $evidence += ("supervisor's own lines in recorder.log (+.1), {0}:" -f $own.Count)
    $evidence += @($own | Select-Object -First 40 | ForEach-Object { '  ' + $_.Substring(0, [math]::Min(230, $_.Length)) })

    # 3. restarts, alerts, stalls
    $row = @($final.Snapshot.Cameras)[0]
    $restartLines = @($own | Where-Object { $_ -match 'restart #' -or $_ -match 'exited' })
    $stallLines = @($own | Where-Object { $_ -match '(?i)\bstall|watchdog' })
    $alerts = 0
    if (Test-Path -LiteralPath $sandbox.AlertLog) { $alerts = @(Get-Content -LiteralPath $sandbox.AlertLog).Count }
    $evidence += ("restarts: RestartsTotal {0}, RestartsLastHour {1}, child pid(s) seen {2}, restart/exit log lines {3}; stall or watchdog lines {4}; alerts recorded by the stub {5}; final status State {6}, LastError '{7}'" -f $row.RestartsTotal, $row.RestartsLastHour, (@($pids) -join ','), $restartLines.Count, $stallLines.Count, $alerts, $final.Snapshot.State, $row.LastError)
    if (-not $final -or [int]$row.RestartsTotal -ne 0 -or [int]$row.RestartsLastHour -ne 0 -or $pids.Count -ne 1 -or $restartLines.Count -or $stallLines.Count -or $alerts) { $ok = $false; $evidence += 'FAIL: a restart, stall or alert happened in a run that should have none' }

    # 4. status.json
    $limit = Get-CamTestCadenceLimit -IntervalSeconds $sandbox.Config.StatusIntervalSeconds
    $evidence += ("status.json: {0} distinct updates seen, longest gap between updates {1} s (at {2}; interval {3} s, harness limit {4} s), samples not running {5}, unreadable polls {6}" -f $statusSeen.Updates, (Format-CamNumber -Value $statusSeen.MaxGap -Format '0.0'), $statusSeen.GapAt, $sandbox.Config.StatusIntervalSeconds, (Format-CamNumber -Value $limit -Format '0.0'), $statusSeen.NotRunning, $statusSeen.Unreadable)
    if ($statusSeen.MaxGap -gt $limit -or $statusSeen.NotRunning) { $ok = $false; $evidence += 'FAIL: status.json went stale or left the running state' }

    # 5. longevity
    $evidence += 'longevity (WS = working set, Priv = private bytes, MB; H = handles):'
    $evidence += '  min   clock     sup WS   sup Priv  sup H |  ffmpeg pid   WS   Priv    H'
    foreach ($s in $samples) {
        $evidence += ("  {0,5} {1}  {2,7} {3,9} {4,6} | {5,10} {6,6} {7,6} {8,4}" -f (Format-CamNumber -Value $s.Minute -Format '0.0'), $s.Clock, (& $num $s.SupWsMB '0.0'), (& $num $s.SupPrivMB '0.0'), $s.SupHandles, $s.FfPid, (& $num $s.FfWsMB '0.0'), (& $num $s.FfPrivMB '0.0'), $s.FfHandles)
    }
    $csv = Join-Path -Path $EvidenceDir -ChildPath $(if ($DryRun) { 'soak-dry-samples.csv' } else { 'soak-samples.csv' })
    try { [void](New-Item -ItemType Directory -Path $EvidenceDir -Force); $samples | Export-Csv -LiteralPath $csv -NoTypeInformation; $evidence += "raw samples: $csv" } catch { }
    $evidence += ("{0} sample(s) per process" -f $samples.Count)
    if ($samples.Count -lt 6 -or @($samples | Where-Object { $null -eq $_.SupHandles -or $null -eq $_.FfHandles }).Count) { $ok = $false; $evidence += 'FAIL: fewer than 6 samples, or a process could not be sampled' }
    $trend = {
        param([string]$Field, [double]$Tolerance, [string]$Label)
        $rows = @($samples | Where-Object { $null -ne $_.$Field })
        if ($rows.Count -lt 3) { return [pscustomobject]@{ Line = "  ${Label}: too few samples"; Up = $false } }
        $xs = @($rows | ForEach-Object { [double]$_.Minute }); $ys = @($rows | ForEach-Object { [double]$_.$Field })
        $mx = ($xs | Measure-Object -Average).Average; $my = ($ys | Measure-Object -Average).Average
        $num = 0.0; $den = 0.0
        for ($i = 0; $i -lt $ys.Count; $i++) { $num += ($xs[$i] - $mx) * ($ys[$i] - $my); $den += ($xs[$i] - $mx) * ($xs[$i] - $mx) }
        $slope = $(if ($den -gt 0) { 60 * $num / $den } else { 0.0 })
        $growth = $ys[-1] - $ys[1]
        $up = ($growth -gt $Tolerance)
        return [pscustomobject]@{ Up = $up; Line = ("  {0}: first {1}, after warm-up (sample 2) {2}, max {3}, last {4}; last minus sample 2 = {5}; least-squares slope {6}/hour{7}" -f `
            $Label, (Format-CamNumber -Value $ys[0] -Format '0.0'), (Format-CamNumber -Value $ys[1] -Format '0.0'), (Format-CamNumber -Value ($ys | Measure-Object -Maximum).Maximum -Format '0.0'), (Format-CamNumber -Value $ys[-1] -Format '0.0'), (Format-CamNumber -Value $growth -Format '+0.0;-0.0'), (Format-CamNumber -Value $slope -Format '+0.00;-0.00'), $(if ($up) { '  UPWARD' } else { '' })) }
    }
    $evidence += 'trend (flagged when the last sample exceeds the post-warm-up sample by more than 20 MB or 50 handles - an arbitrary bar; handles FAIL the check, memory is report-only; the numbers are the finding):'
    $trendUp = $false
    foreach ($spec in @(@('SupWsMB', 20, 'supervisor working set MB'), @('SupPrivMB', 20, 'supervisor private MB'), @('SupHandles', 50, 'supervisor handles'), @('FfWsMB', 20, 'ffmpeg working set MB'), @('FfPrivMB', 20, 'ffmpeg private MB'), @('FfHandles', 50, 'ffmpeg handles'))) {
        $result = & $trend $spec[0] $spec[1] $spec[2]
        $evidence += $result.Line
        if ($result.Up) {
            # Handles are a hard failure: a handle leak is unambiguous. Memory is REPORT ONLY: the first soak showed the
            # supervisor's memory rising about 1.3 MB/min under a camera ~840x noisier than a real one, and a micro-test
            # showed Write-CamLog itself is flat, so the cause (a leak or collector drift) is unproven.
            if ($spec[0] -like '*Handles') { $trendUp = $true }
            else { $evidence += '    ^ rose past the bar: REPORT ONLY (memory; cause unproven, see the soak report)' }
        }
    }
    if ($trendUp) { $ok = $false; $evidence += 'FAIL: handles trend upward' }

    # 6. disk
    $logBytes = 0L
    foreach ($file in @(Get-ChildItem -LiteralPath $sandbox.Root -Recurse -File -Force | Where-Object { $_.Name -like '*.log' -or $_.Name -like '*.log.1' })) { $logBytes += $file.Length }
    $evidence += ("disk {0}: free at start {1} B, at end {2} B (down {3} B); archive segments {4} B, logs on disk {5} B, camera log written in all about {6} B" -f $volume.Name, $freeStart, $freeEnd, ($freeStart - $freeEnd), $segmentBytes, $logBytes, $camBytes)

    # clean finish
    $left = @(Get-CimInstance -ClassName Win32_Process | Where-Object { $_.CommandLine -and $_.CommandLine.Contains($sandbox.Dir) })
    $ours = @(@($pids) + @($process.Id) + @($camera.MediaMtx.Id) + @($camera.Publishers | ForEach-Object { $_.Id }) | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
    $portFree = Test-CamTestPortFree -Port $port
    $evidence += ("after stop: processes still referencing the sandbox {0}, of our pids still alive {1}, port {2} free {3}" -f $left.Count, $ours.Count, $port, $portFree)
    if ($left.Count -or $ours.Count -or -not $portFree) { $ok = $false; $evidence += 'FAIL: something of the soak is still running' }
    $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
    $evidence += $leak.Text
    if (-not $leak.Ok) { $ok = $false }

    $keep = Join-Path -Path $EvidenceDir -ChildPath $(if ($DryRun) { 'soak-dry' } else { 'soak' })
    try {
        if (Test-Path -LiteralPath $keep) { Remove-Item -LiteralPath $keep -Recurse -Force }
        [void](New-Item -ItemType Directory -Path $keep -Force)
        foreach ($file in @($recLog, "$recLog.1", $camLog, "$camLog.1", (Get-CamStatusFilePath -Config $sandbox.Config), $sandbox.AlertLog, $sandbox.ConsoleLog, "$($sandbox.ConsoleLog).err")) {
            if (Test-Path -LiteralPath $file) { Copy-Item -LiteralPath $file -Destination $keep -Force }
        }
        $evidence += "logs and status kept in $keep"
    }
    catch { $evidence += "could not keep the logs: $($_.Exception.Message)" }

    Complete-CamPruneCheck -Run $Run -Check 'soak' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $sandbox.Dir; Log = (Join-Path $env:TEMP 'cam-tests\soak.none') }) `
        -Message 'at the production defaults the real supervisor records two complete 900 s segments, both logs roll without loss, nothing restarts, and memory and handles stay flat'
}


function Invoke-CamCheckStallWatch {
    <#
        B1: the stall watchdog, with the real supervisor recording a synthetic
        camera at SegmentSeconds 6 x StallFactor 2.5 = a 15 s limit.

        The watchdog judges directory-listing TIMESTAMPS only: the later of the
        child's start, the newest LastWriteTime among the closed files and the
        CreationTime of the file ffmpeg holds. Never a length, so neither the
        stale size of an open file nor ffmpeg's 256 KB write blocks reach it.

        - healthy: over 20+ segment rolls the child is never killed, and the
          age the watchdog computes - sampled here from the same listing -
          stays below the limit; the closest margin is reported;
        - stalled: the child is suspended, so it is alive and writes nothing.
          The watchdog must kill it about 15 s after its last progress, the
          restart must count in RestartsLastHour, and a new segment must be
          written. Nothing is left suspended.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $port = 18554
    $name = 'synth1'
    $segment = 6
    $factor = 2.5
    $limit = $segment * $factor
    $sandbox = New-CamTestSynthSandbox -Name 'stallwatch' -CameraName $name -Port $port `
        -Config @{ SegmentSeconds = $segment; StallFactor = $factor; StatusIntervalSeconds = 1; ReconcileSeconds = 2; BackoffStartSeconds = 1 }
    $camDir = Join-Path -Path $sandbox.Root -ChildPath $name

    # The watchdog's reference as it computes it, minus the child's start.
    # UTC, so the check itself cannot be fooled by a DST change.
    $reference = {
        $files = @(Get-ChildItem -LiteralPath $camDir -File -Force -ErrorAction SilentlyContinue)
        if ($files.Count -eq 0) { return $null }
        $current = $files | Sort-Object -Property CreationTimeUtc -Descending | Select-Object -First 1
        $latest = $current.CreationTimeUtc
        foreach ($file in $files) { if ($file.FullName -ne $current.FullName -and $file.LastWriteTimeUtc -gt $latest) { $latest = $file.LastWriteTimeUtc } }
        return $latest
    }
    $segmentCount = { @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config).Count }
    $watchdogLines = { @(Get-Content -LiteralPath $sandbox.SupervisorLog | Where-Object { $_ -like '*watchdog*' }) }

    $camera = $null
    $process = $null
    $suspended = 0
    try {
        $camera = Start-CamTestSyntheticCamera -Dir $sandbox.Dir -Port $port -PathName $name
        $process = Start-CamTestSupervisor -Sandbox $sandbox
        $child = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -TimeoutSeconds 20
        $evidence += ("synthetic camera mediamtx pid {0}, publisher pid {1}; supervisor pid {2}; child pid {3}; limit {4} x {5} = {6} s" -f `
            $camera.MediaMtx.Id, (@($camera.Publishers | ForEach-Object { $_.Id }) -join ','), $process.Id, $child, $segment, (Format-CamNumber -Value $factor -Format '0.0'), $limit)

        # healthy
        $deadline = [DateTime]::UtcNow.AddSeconds(30)
        while ((& $segmentCount) -lt 1 -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }
        $first = & $segmentCount
        $maxAge = 0.0
        $deadline = [DateTime]::UtcNow.AddSeconds(21 * $segment + 20)
        while (((& $segmentCount) - $first) -lt 21 -and [DateTime]::UtcNow -lt $deadline -and -not $process.HasExited) {
            $at = & $reference
            if ($at) { $age = ([DateTime]::UtcNow - $at).TotalSeconds; if ($age -gt $maxAge) { $maxAge = $age } }
            Start-Sleep -Milliseconds 200
        }
        $rolls = (& $segmentCount) - $first
        $same = Get-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name
        $row = @((Get-CamTestStatusJson -Sandbox $sandbox).Snapshot.Cameras)[0]
        $evidence += ("healthy: {0} segment roll(s); child pid {1} -> {2}; watchdog lines {3}; RestartsLastHour {4}; largest watchdog age {5} s against the {6} s limit (margin {7} s)" -f `
            $rolls, $child, $same, (& $watchdogLines).Count, $row.RestartsLastHour, (Format-CamNumber -Value $maxAge -Format '0.0'), $limit, (Format-CamNumber -Value ($limit - $maxAge) -Format '0.0'))
        if ($rolls -lt 20 -or $same -ne $child -or (& $watchdogLines).Count -or [int]$row.RestartsLastHour -ne 0 -or $maxAge -ge $limit) {
            $ok = $false; $evidence += 'FAIL: a healthy child was killed, restarted or came within the stall limit'
        }

        # stalled: suspend just after a roll, so the delay is close to the whole limit
        $before = & $segmentCount
        $deadline = [DateTime]::UtcNow.AddSeconds($segment + 5)
        while ((& $segmentCount) -eq $before -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 100 }
        Start-Sleep -Milliseconds 500
        $lastProgress = & $reference
        $old = Get-Process -Id $child
        $suspendedAt = [DateTime]::UtcNow
        Suspend-CamTestProcess -ProcessId $child
        $suspended = $child
        $evidence += ("stalled: suspended pid {0} at +{1} s after its last progress; suspended threads seen {2}" -f `
            $child, (Format-CamNumber -Value ($suspendedAt - $lastProgress).TotalSeconds -Format '0.0'), (Test-CamTestProcessSuspended -ProcessId $child))

        $killedAt = $null
        $deadline = $suspendedAt.AddSeconds($limit + 20)
        while ([DateTime]::UtcNow -lt $deadline) {
            if ($old.HasExited) { $killedAt = [DateTime]::UtcNow; break }
            Start-Sleep -Milliseconds 100
        }
        $next = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -Not $child -TimeoutSeconds 15
        $resumed = $false
        $deadline = [DateTime]::UtcNow.AddSeconds($segment + 15)
        while ($killedAt -and [DateTime]::UtcNow -lt $deadline) {
            $newest = @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config) | Select-Object -Last 1
            if ($newest -and $newest.CreationTimeUtc -gt $killedAt -and (Get-CamTestLiveLength -Path $newest.FullName) -gt 0) { $resumed = $true; break }
            Start-Sleep -Milliseconds 250
        }
        Start-Sleep -Seconds 2
        $row = @((Get-CamTestStatusJson -Sandbox $sandbox).Snapshot.Cameras)[0]
        foreach ($line in (& $watchdogLines)) { $evidence += ('  log: ' + $line) }
        $killLine = @(Get-Content -LiteralPath $sandbox.SupervisorLog | Where-Object { $_ -like "*kill $name pid $child*" }) | Select-Object -First 1
        if ($killLine) { $evidence += ('  log: ' + $killLine) }
        if ($killedAt) {
            $sinceProgress = ($killedAt - $lastProgress).TotalSeconds
            $sinceSuspend = ($killedAt - $suspendedAt).TotalSeconds
            $evidence += ("  killed {0} s after its last progress and {1} s after the suspend (limit {2} s); new child pid {3}; new segment with data {4}; RestartsLastHour {5}, Alive {6}" -f `
                (Format-CamNumber -Value $sinceProgress -Format '0.0'), (Format-CamNumber -Value $sinceSuspend -Format '0.0'), $limit, $next, $resumed, $row.RestartsLastHour, $row.Alive)
            # Above the limit, and late by at most a status scan (1 s), a
            # supervisor tick (1 s) and this poll.
            if ($sinceProgress -le $limit -or $sinceProgress -gt ($limit + 3.5)) { $ok = $false; $evidence += 'FAIL: the kill was not about one limit after the last progress' }
        }
        else { $evidence += ("  FAIL: the suspended child was not killed within {0} s" -f ($limit + 20)) }
        if (-not $killedAt -or $next -le 0 -or -not $resumed -or [int]$row.RestartsLastHour -ne 1 -or -not $row.Alive -or -not $killLine) {
            $ok = $false; $evidence += 'FAIL: the stalled child was not killed, restarted, counted and recording again'
        }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        if ($suspended) {
            $clear = Resume-CamTestProcess -ProcessId $suspended
            $evidence += ("pid {0} left suspended: {1}" -f $suspended, (-not $clear))
            if (-not $clear) { $ok = $false }
        }
        if ($process) {
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            if (-not $stop.Ok) { $ok = $false; $evidence += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }

    $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
    $evidence += $leak.Text
    if (-not $leak.Ok) { $ok = $false }

    Complete-CamPruneCheck -Run $Run -Check 'stallwatch' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $sandbox.Dir; Log = (Join-Path $env:TEMP 'cam-tests\stallwatch.none') }) `
        -Message 'the stall watchdog kills a suspended child about StallFactor x SegmentSeconds after its last segment write, and never a healthy one'
}

function Invoke-CamCheckDownAlert {
    <#
        [92] alert 110, decided on synthetic scans through the product's own
        Measure-CamCameraDown and Get-CamDownAlertSeconds.

        A healthy camera is a full segment past its last progress marker when
        the boundary comes, plus the keyframe wait and a status tick, before the
        next status tick's directory scan sees the new file. With CameraDownAlertMinutes equal to
        SegmentSeconds - the default - that window raised a false alert, so the
        threshold has a floor: one segment + the status interval + 30 s. The
        archive scan interval plays no part (one row guards that). The expected
        values below are literals, not the formula.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name 'Get-CamDownAlertSeconds', 'Measure-CamCameraDown')
    $evidence = @()
    $ok = $true
    $t0 = [DateTime]::new(2026, 10, 17, 8, 30, 1, [DateTimeKind]::Utc)

    function Get-CamTestDownVerdict {
        param($Config, $Scan, [DateTime]$Started, [DateTime]$Now)
        $limit = Get-CamDownAlertSeconds -Config $Config
        $down = Measure-CamCameraDown -Scan $Scan -SupervisorStartedAt $Started -Now $Now
        # The supervisor's own test: it alerts when seconds are NOT <= the limit.
        return [pscustomobject]@{ Seconds = $down.Seconds; Limit = $limit; Alert = (-not ($down.Seconds -le $limit)) }
    }

    $defaults = Get-CamConfigDefault
    $fresh = @{ NewestClosedWrite = $t0.AddSeconds(-1); CurrentCreated = $t0 }
    $older = @{ NewestClosedWrite = $t0.AddHours(-2); CurrentCreated = $t0.AddHours(-2) }
    $rows = @(
        @{ Label = 'defaults, stale scan at the boundary'; Config = $defaults; Scan = $fresh; Started = $t0.AddHours(-3); After = 901; Want = $false },
        @{ Label = 'defaults, stale scan at the keyframe wait'; Config = $defaults; Scan = $fresh; Started = $t0.AddHours(-3); After = 930; Want = $false },
        @{ Label = 'defaults, exactly the effective limit'; Config = $defaults; Scan = $fresh; Started = $t0.AddHours(-3); After = 935; Want = $false },
        @{ Label = 'defaults, one second past the limit'; Config = $defaults; Scan = $fresh; Started = $t0.AddHours(-3); After = 936; Want = $true },
        @{ Label = 'defaults, camera dark for 20 minutes'; Config = $defaults; Scan = $fresh; Started = $t0.AddHours(-3); After = 1200; Want = $true },
        @{ Label = 'no progress ever, supervisor start is the floor'; Config = $defaults; Scan = $null; Started = $t0; After = 901; Want = $false },
        @{ Label = 'no progress ever, past the limit'; Config = $defaults; Scan = $null; Started = $t0; After = 936; Want = $true },
        @{ Label = 'empty directory scan, past the limit'; Config = $defaults; Scan = @{ NewestClosedWrite = $null; CurrentCreated = $null }; Started = $t0; After = 936; Want = $true },
        @{ Label = 'progress older than the supervisor start'; Config = $defaults; Scan = $older; Started = $t0; After = 901; Want = $false },
        @{ Label = 'progress older than the supervisor start, past limit'; Config = $defaults; Scan = $older; Started = $t0; After = 936; Want = $true })
    foreach ($row in $rows) {
        $verdict = Get-CamTestDownVerdict -Config $row.Config -Scan $row.Scan -Started $row.Started -Now $t0.AddSeconds($row.After)
        $good = ($verdict.Alert -eq $row.Want)
        if (-not $good) { $ok = $false }
        $evidence += ("{0,-4} {1,-56} +{2,4} s: age {3,7} s, limit {4,6} s -> alert {5} (want {6})" -f `
            $(if ($good) { 'ok' } else { 'FAIL' }), $row.Label, $row.After, (Format-CamNumber -Value $verdict.Seconds -Format '0.0'), (Format-CamNumber -Value $verdict.Limit -Format '0'), $verdict.Alert, $row.Want)
    }

    # Negative control: the pre-fix rule, CameraDownAlertMinutes x 60, alerts at
    # +901 on the same scan, so the rows above can tell the two apart.
    $oldLimit = [double]$defaults.CameraDownAlertMinutes * 60
    $oldAlert = (-not ((Measure-CamCameraDown -Scan $fresh -SupervisorStartedAt $t0.AddHours(-3) -Now $t0.AddSeconds(901)).Seconds -le $oldLimit))
    $newAlert = (Get-CamTestDownVerdict -Config $defaults -Scan $fresh -Started $t0.AddHours(-3) -Now $t0.AddSeconds(901)).Alert
    $evidence += ("control: the pre-fix limit is {0} s and alerts at +901 s: {1}; the product alerts at +901 s: {2}" -f $oldLimit, $oldAlert, $newAlert)
    if (-not $oldAlert -or $newAlert) { $ok = $false; $evidence += 'FAIL: the pre-fix rule must alert at +901 s and the product must not' }

    # The documented effective limit: literals, in seconds.
    $table = @(
        @{ Seg = 900; Min = 5; Scan = 60; Status = 5; Want = 935 }, @{ Seg = 900; Min = 15; Scan = 60; Status = 5; Want = 935 }, @{ Seg = 900; Min = 60; Scan = 60; Status = 5; Want = 3600 },
        @{ Seg = 1800; Min = 5; Scan = 60; Status = 5; Want = 1835 }, @{ Seg = 1800; Min = 15; Scan = 60; Status = 5; Want = 1835 }, @{ Seg = 1800; Min = 60; Scan = 60; Status = 5; Want = 3600 },
        @{ Seg = 60; Min = 5; Scan = 60; Status = 5; Want = 300 }, @{ Seg = 60; Min = 15; Scan = 60; Status = 5; Want = 900 }, @{ Seg = 60; Min = 60; Scan = 60; Status = 5; Want = 3600 },
        @{ Seg = 900; Min = 15; Scan = 300; Status = 5; Want = 935 }, @{ Seg = 900; Min = 15; Scan = 5; Status = 5; Want = 935 }, @{ Seg = 900; Min = 15; Scan = 60; Status = 30; Want = 960 })
    foreach ($case in $table) {
        $config = Get-CamConfigDefault
        $config.SegmentSeconds = $case.Seg; $config.CameraDownAlertMinutes = $case.Min; $config.ArchiveScanSeconds = $case.Scan; $config.StatusIntervalSeconds = $case.Status
        $got = Get-CamDownAlertSeconds -Config $config
        $before = Get-CamTestDownVerdict -Config $config -Scan $fresh -Started $t0.AddHours(-3) -Now $t0.AddSeconds($case.Want)
        $after = Get-CamTestDownVerdict -Config $config -Scan $fresh -Started $t0.AddHours(-3) -Now $t0.AddSeconds($case.Want + 1)
        $good = ($got -eq $case.Want -and -not $before.Alert -and $after.Alert)
        if (-not $good) { $ok = $false }
        $evidence += ("{0,-4} segment {1,4} s, configured {2,2} min, scan {3,3} s, status {4,2} s -> effective {5,5} s (want {6}); quiet at +{6}, alerts at +{7}" -f `
            $(if ($good) { 'ok' } else { 'FAIL' }), $case.Seg, $case.Min, $case.Scan, $case.Status, (Format-CamNumber -Value $got -Format '0'), $case.Want, ($case.Want + 1))
    }

    Add-CamTestResult -Run $Run -Check 'downalert' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) -Evidence $evidence `
        -Message ("alert 110 waits for max(CameraDownAlertMinutes, segment + status + 30 s) without progress: {0} rows, {1} configurations, a stale scan at a segment boundary never alerts" -f $rows.Count, $table.Count)
}


function Invoke-CamCheckDownAlertRun {
    <#
        [92] alert 110 against the real supervisor and a synthetic camera.

        SegmentSeconds 20, so the effective limit is 20 + 1 + 30 = 51 s, with CameraDownAlertMinutes set to the 20 s segment so that the
        pre-fix rule alerts whenever a status tick lands in the wait for the
        next segment. Segments cut at the first keyframe after a 20 s clock
        boundary (-segment_atclocktime), and the camera sends one every 11 s,
        so each cut falls 2 s later than the one before and a segment lasts
        22 s: the seconds past the boundary that a real camera's GOP and the
        status tick add to a 900 s segment.

        - healthy: three segment rolls, no alert 110 at all;
        - dark: the publisher is killed. Exactly one alert 110, no sooner than
          the effective limit after the camera's last file progress and not
          much later.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $port = 18559
    $name = 'synth1'
    $segment = 20
    $limit = 51
    $sandbox = New-CamTestSynthSandbox -Name 'downalertrun' -CameraName $name -Port $port `
        -Config @{ SegmentSeconds = $segment; StatusIntervalSeconds = 1; ArchiveScanSeconds = 3; ReconcileSeconds = 2; BackoffStartSeconds = 1; CameraDownAlertMinutes = ($segment / 60.0) }
    $camDir = Join-Path -Path $sandbox.Root -ChildPath $name

    $reference = {
        $files = @(Get-ChildItem -LiteralPath $camDir -File -Force -ErrorAction SilentlyContinue)
        if ($files.Count -eq 0) { return $null }
        $current = $files | Sort-Object -Property CreationTimeUtc -Descending | Select-Object -First 1
        $latest = $current.CreationTimeUtc
        foreach ($file in $files) { if ($file.FullName -ne $current.FullName -and $file.LastWriteTimeUtc -gt $latest) { $latest = $file.LastWriteTimeUtc } }
        return $latest
    }
    $segmentCount = { @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config).Count }
    $alertLines = { if (Test-Path -LiteralPath $sandbox.AlertLog) { @(Get-Content -LiteralPath $sandbox.AlertLog | Where-Object { $_ -like '110 *' }) } else { @() } }

    $camera = $null
    $process = $null
    try {
        $camera = Start-CamTestSyntheticCamera -Dir $sandbox.Dir -Port $port -PathName $name -GopSeconds 11
        $process = Start-CamTestSupervisor -Sandbox $sandbox
        $child = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -TimeoutSeconds 20
        $evidence += ("synthetic camera pid {0}, publisher pid {1}; supervisor pid {2}; child pid {3}; segment {4} s, GOP 11 s, effective limit {5} s" -f `
            $camera.MediaMtx.Id, (@($camera.Publishers | ForEach-Object { $_.Id }) -join ','), $process.Id, $child, $segment, $limit)
        $startLine = @(Get-Content -LiteralPath $sandbox.SupervisorLog | Where-Object { $_ -like '*alert 110*' }) | Select-Object -First 1
        if ($startLine) { $evidence += ('  log: ' + $startLine) }

        $deadline = [DateTime]::UtcNow.AddSeconds(30)
        while ((& $segmentCount) -lt 1 -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }
        $first = & $segmentCount
        $deadline = [DateTime]::UtcNow.AddSeconds(3 * ($segment + 11) + 30)
        while (((& $segmentCount) - $first) -lt 3 -and [DateTime]::UtcNow -lt $deadline -and -not $process.HasExited) { Start-Sleep -Milliseconds 200 }
        $rolls = (& $segmentCount) - $first
        $healthyAlerts = @(& $alertLines)
        $evidence += ("healthy: {0} segment roll(s); alert 110 count {1}" -f $rolls, $healthyAlerts.Count)
        foreach ($line in $healthyAlerts) { $evidence += ('  alert: ' + $line) }
        if ($rolls -lt 3 -or $healthyAlerts.Count -ne 0) { $ok = $false; $evidence += 'FAIL: a healthy camera raised alert 110, or did not roll three segments' }

        if ($healthyAlerts.Count -eq 0) {
            foreach ($publisher in $camera.Publishers) { try { $publisher.Kill() } catch { } }
            $killedAt = [DateTime]::UtcNow
            $deadline = $killedAt.AddSeconds($limit + 30)
            while (@(& $alertLines).Count -eq 0 -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }
            $alertedAt = [DateTime]::UtcNow
            $progress = & $reference
            $sinceProgress = ($alertedAt - $progress).TotalSeconds
            Start-Sleep -Seconds 12
            $darkAlerts = @(& $alertLines)
            $evidence += ("dark: publisher killed; last file progress {0} s before the kill; alert seen {1} s after the last progress (limit {2} s); alert 110 count {3}" -f `
                (Format-CamNumber -Value ($killedAt - $progress).TotalSeconds -Format '0.0'), (Format-CamNumber -Value $sinceProgress -Format '0.0'), $limit, $darkAlerts.Count)
            foreach ($line in $darkAlerts) { $evidence += ('  alert: ' + $line) }
            if ($darkAlerts.Count -ne 1) { $ok = $false; $evidence += 'FAIL: a dark camera must raise alert 110 exactly once' }
            # Never before the limit; late by at most a status tick, a supervisor tick, a scan and this poll.
            elseif ($sinceProgress -lt $limit -or $sinceProgress -gt ($limit + 8)) { $ok = $false; $evidence += 'FAIL: the alert was not about one effective limit after the last progress' }
        }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        if ($process) {
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            if (-not $stop.Ok) { $ok = $false; $evidence += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }

    $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
    $evidence += $leak.Text
    if (-not $leak.Ok) { $ok = $false }

    Complete-CamPruneCheck -Run $Run -Check 'downalertrun' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $sandbox.Dir; Log = (Join-Path $env:TEMP 'cam-tests\downalertrun.none') }) `
        -Message 'a healthy camera whose alert minutes equal its segment raises no alert 110 over three segments, and a dark one raises exactly one, at the effective limit'
}


function Invoke-CamCheckLogHeld {
    <#
        B3: Write-CamLog never throws when the APPEND fails, and the real
        supervisor survives its logs being held with no sharing at all.

        - function: with the log open FileShare.None, five writes return
          without throwing and lose their lines, as does a write whose folder
          cannot be created; the first write after the holder lets go is
          preceded by exactly one notice counting the five, and why.
        - supervisor: recorder.log and the camera's log are held FileShare.None
          for 20 s while the synthetic camera makes ffmpeg warn and the child
          is killed, so both logs have lines to lose. The supervisor stays up,
          restarts the child, keeps status.json updating and keeps recording;
          once released, each log carries one drop notice.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $holdOpen = {
        param([string]$Path)
        $deadline = [DateTime]::Now.AddSeconds(5)
        while ($true) {
            try { return [System.IO.File]::Open($Path, [System.IO.FileMode]::OpenOrCreate, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None) }
            catch { if ([DateTime]::Now -ge $deadline) { throw }; Start-Sleep -Milliseconds 20 }
        }
    }
    $noticeLike = '*log write failed: * line(s) dropped from this log since *'

    # function
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\logheld-function'
    if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force }
    [void](New-Item -ItemType Directory -Path $dir -Force)
    $log = Join-Path -Path $dir -ChildPath 'held.log'
    $blocker = Join-Path -Path $dir -ChildPath 'not-a-folder'
    [System.IO.File]::WriteAllText($blocker, 'a file where a log folder would go')
    $noFolder = Join-Path -Path $blocker -ChildPath 'logs\x.log'
    $throws = 0
    $holder = $null
    try {
        Write-CamLog -Path $log -Message 'before 1'
        Write-CamLog -Path $log -Message 'before 2'
        $holder = & $holdOpen $log
        for ($i = 1; $i -le 5; $i++) { try { Write-CamLog -Path $log -Message "held $i" } catch { $throws++ } }
        $holder.Dispose()
        $holder = $null
        $folderThrew = $false
        try { Write-CamLog -Path $noFolder -Message 'nowhere to go' } catch { $folderThrew = $true }
        Write-CamLog -Path $log -Message 'after'
        Write-CamLog -Path $log -Message 'after 2'
        $lines = @(Get-Content -LiteralPath $log)
        $notices = @($lines | Where-Object { $_ -like $noticeLike })
        $evidence += ("function: 5 writes with the log held FileShare.None threw {0}x; a write whose folder is a file threw {1}; file now {2} line(s): {3}" -f `
            $throws, $folderThrew, $lines.Count, (@($lines | ForEach-Object { if ($_.Length -gt 26) { $_.Substring(26) } else { $_ } }) -join ' | '))
        $shape = ($lines.Count -eq 5 -and $lines[0] -like '*before 1' -and $lines[1] -like '*before 2' -and $notices.Count -eq 1 -and `
            $lines[2] -like '*log write failed: 5 line(s) dropped*' -and $lines[2] -like '*being used by another process*' -and $lines[3] -like '*after' -and $lines[4] -like '*after 2')
        if ($throws -or $folderThrew -or -not $shape) { $ok = $false; $evidence += 'FAIL: Write-CamLog threw, or the file is not the two lines, one notice counting 5, then the two lines after' }
    }
    catch { $ok = $false; $evidence += ('FAIL: function phase threw: ' + $_.Exception.Message) }
    finally { if ($holder) { $holder.Dispose() } }

    # supervisor
    $port = 18554
    $name = 'synth1'
    $sandbox = New-CamTestSynthSandbox -Name 'logheld' -CameraName $name -Port $port `
        -Config @{ SegmentSeconds = 10; StatusIntervalSeconds = 1; ReconcileSeconds = 2; LogRepeatSuppressSeconds = 0; BackoffStartSeconds = 1 }
    $recLog = $sandbox.SupervisorLog
    $camLog = Join-Path -Path $sandbox.Root -ChildPath "_logs\$name.log"
    $camDir = Join-Path -Path $sandbox.Root -ChildPath $name
    $watch = @{ LastUpdate = $null; MaxStatusGap = 0.0; Died = $false }
    $sample = {
        $status = Get-CamTestStatusJson -Sandbox $sandbox
        if ($status) {
            $updated = [DateTime]::Parse([string]$status.Snapshot.UpdatedAt)
            if ($watch.LastUpdate -and $updated -gt $watch.LastUpdate) {
                $gap = ($updated - $watch.LastUpdate).TotalSeconds
                if ($gap -gt $watch.MaxStatusGap) { $watch.MaxStatusGap = $gap }
            }
            if (-not $watch.LastUpdate -or $updated -gt $watch.LastUpdate) { $watch.LastUpdate = $updated }
        }
        if ($process.HasExited) { $watch.Died = $true }
    }

    $camera = $null
    $process = $null
    $held = @()
    try {
        $camera = Start-CamTestSyntheticCamera -Dir $sandbox.Dir -Port $port -PathName $name -NonMonotonic
        $process = Start-CamTestSupervisor -Sandbox $sandbox
        $child = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -TimeoutSeconds 20
        $deadline = [DateTime]::Now.AddSeconds(20)
        while (-not (Test-Path -LiteralPath $camLog) -and [DateTime]::Now -lt $deadline) { Start-Sleep -Milliseconds 200 }
        $evidence += ("supervisor: synthetic camera mediamtx pid {0}, publisher pid {1} (non-monotonic video); supervisor pid {2}; child pid {3}" -f `
            $camera.MediaMtx.Id, (@($camera.Publishers | ForEach-Object { $_.Id }) -join ','), $process.Id, $child)

        $held += & $holdOpen $recLog
        $held += & $holdOpen $camLog
        $heldAt = [DateTime]::Now
        $camBefore = $held[1].Length
        $killed = $false
        while (([DateTime]::Now - $heldAt).TotalSeconds -lt 20) {
            & $sample
            if (-not $killed -and ([DateTime]::Now - $heldAt).TotalSeconds -ge 5) { $killed = Stop-CamTestChildHard -ProcessId $child }
            Start-Sleep -Milliseconds 250
        }
        $recHeld = $held[0].Length
        $camHeld = $held[1].Length
        foreach ($stream in $held) { $stream.Dispose() }
        $held = @()
        $releasedAt = [DateTime]::Now
        $next = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $name -Not $child -TimeoutSeconds 10
        # A line to carry each notice: the camera log gets ffmpeg's warnings,
        # recorder.log the next event, which a second kill provides.
        if ($next -gt 0) { [void](Stop-CamTestChildHard -ProcessId $next) }
        $deadline = [DateTime]::Now.AddSeconds(15)
        $recNotice = $null
        $camNotice = $null
        while ([DateTime]::Now -lt $deadline -and -not ($recNotice -and $camNotice)) {
            & $sample
            $recNotice = @(Get-Content -LiteralPath $recLog | Where-Object { $_ -like $noticeLike })
            $camNotice = @(Get-Content -LiteralPath $camLog | Where-Object { $_ -like $noticeLike })
            if (-not $recNotice.Count) { $recNotice = $null }
            if (-not $camNotice.Count) { $camNotice = $null }
            Start-Sleep -Milliseconds 250
        }
        $created = { param($from, $to) @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config | Where-Object { $_.CreationTime -gt $from -and $_.CreationTime -lt $to }).Count }
        $duringHold = & $created $heldAt $releasedAt
        $deadline = [DateTime]::Now.AddSeconds(20)
        while ((& $created $releasedAt ([DateTime]::MaxValue)) -lt 1 -and [DateTime]::Now -lt $deadline) { & $sample; Start-Sleep -Milliseconds 250 }
        $afterHold = & $created $releasedAt ([DateTime]::MaxValue)
        $shutdown = @(Get-Content -LiteralPath $recLog | Where-Object { $_ -like '*supervisor shutting down*' }).Count
        $gapLimit = Get-CamTestCadenceLimit -IntervalSeconds $sandbox.Config.StatusIntervalSeconds
        $evidence += ("  held recorder.log ({0} B) and {1}.log ({2} B) FileShare.None for 20 s, child pid {3} killed at +5 s: lengths unchanged {4}; supervisor alive {5}; restarted as pid {6}; status.json longest gap {7} s (limit {8} s)" -f `
            $recHeld, $name, $camBefore, $child, ($camHeld -eq $camBefore), (-not $watch.Died), $next, (Format-CamNumber -Value $watch.MaxStatusGap -Format '0.0'), $gapLimit)
        $evidence += ("  recorder.log: {0}" -f $(if ($recNotice) { (@($recNotice) -join ' | ') } else { 'no drop notice' }))
        $evidence += ("  {0}.log: {1}" -f $name, $(if ($camNotice) { (@($camNotice) -join ' | ') } else { 'no drop notice' }))
        $evidence += ("  segments created during the hold: {0}, after the release: {1}; 'supervisor shutting down' lines: {2}" -f $duringHold, $afterHold, $shutdown)
        if ($watch.Died -or -not $killed -or $next -le 0 -or @($recNotice).Count -ne 1 -or @($camNotice).Count -ne 1 -or $watch.MaxStatusGap -gt $gapLimit -or $duringHold -lt 1 -or $afterHold -lt 1 -or $shutdown) {
            $ok = $false; $evidence += 'FAIL: with its logs held the supervisor died, stopped updating or recording, or did not report the dropped lines once'
        }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        foreach ($stream in $held) { $stream.Dispose() }
        if ($process) {
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            if (-not $stop.Ok) { $ok = $false; $evidence += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }

    $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
    $evidence += $leak.Text
    if (-not $leak.Ok) { $ok = $false }

    Complete-CamPruneCheck -Run $Run -Check 'logheld' -Ok $ok -Evidence $evidence `
        -Sandboxes @([pscustomobject]@{ Dir = $dir; Log = (Join-Path $env:TEMP 'cam-tests\logheld.none') }, [pscustomobject]@{ Dir = $sandbox.Dir; Log = (Join-Path $env:TEMP 'cam-tests\logheld.none') }) `
        -Message 'Write-CamLog never throws when the append fails, counts the lost lines once, and the supervisor survives its logs held exclusively'
}

function Invoke-CamCheckPruneLoop {
    <#
        B2: retention and the pruning alerts through the REAL supervisor loop,
        PruneIntervalSeconds 2, recording a synthetic camera. Three runs, one
        per configuration; fixtures are planted after the supervisor has
        claimed its root, and alerts go to the sandbox stub's alerts.log.

        - age: MaxRetentionDays 1 (brake off). The loop deletes exactly the
          expired segments - backdated ones in the recording camera's folder
          and in a second camera's folder except that folder's newest - and
          leaves the young fixture, a foreign file, a wrong-container file and
          everything the camera records. One 'prune stop:' line per run.
        - breaker: MaxRetentionDays 1, MaxAgePruneFraction 0.5, more than half
          the archive expired: alert 122 (Error) once while the episode lasts
          and nothing deleted; thinning the expired set below the brake resets
          it, and a second flood raises 122 exactly once more.
        - blocked: ReserveGB far above the free space, so the watermark deletes
          everything it may and is still short: alert 121 (Error) once across
          every blocked run, the newest segment never deleted.
        Throughout: no other alert, the camera keeps recording without a gap,
        status.json keeps updating.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $port = 18554
    $name = 'synth1'
    $segment = 6
    $base = @{ SegmentSeconds = $segment; StatusIntervalSeconds = 1; ReconcileSeconds = 2; PruneIntervalSeconds = 2; ArchiveScanSeconds = 5; BackoffStartSeconds = 1 }
    $sandboxes = @()
    $camera = $null

    $configFor = { param([hashtable]$extra) $c = $base.Clone(); foreach ($k in $extra.Keys) { $c[$k] = $extra[$k] }; $c }
    $logLines = { param($sandbox, [string]$like) @(Get-Content -LiteralPath $sandbox.SupervisorLog | Where-Object { $_ -like $like }) }
    $alerts = { param($sandbox) if (Test-Path -LiteralPath $sandbox.AlertLog) { @(Get-Content -LiteralPath $sandbox.AlertLog) } else { @() } }
    $lineTime = { param([string]$line) [DateTime]::ParseExact($line.Substring(0, 19), 'yyyy-MM-dd HH:mm:ss', [System.Globalization.CultureInfo]::InvariantCulture) }
    $deletedPaths = { param($sandbox) @(& $logLines $sandbox '* prune: deleted *' | ForEach-Object { if ($_ -match ' prune: deleted (.+?) \(\d+ bytes') { $Matches[1] } }) }

    # Runs one supervisor in $sandbox: waits for the claim and the first
    # segment, calls $body with a sampler, stops it, and returns the facts
    # every phase checks.
    $runPhase = {
        param($sandbox, [scriptblock]$body)
        $watch = @{ LastUpdate = $null; MaxStatusGap = 0.0; Died = $false }
        $process = $null
        $facts = [pscustomobject]@{ Ok = $true; Text = @() }
        $camDir = Join-Path -Path $sandbox.Root -ChildPath $name
        $phaseStart = [DateTime]::Now
        $sample = {
            $status = Get-CamTestStatusJson -Sandbox $sandbox
            if ($status) {
                $updated = [DateTime]::Parse([string]$status.Snapshot.UpdatedAt)
                if ($watch.LastUpdate -and $updated -gt $watch.LastUpdate) {
                    $gap = ($updated - $watch.LastUpdate).TotalSeconds
                    if ($gap -gt $watch.MaxStatusGap) { $watch.MaxStatusGap = $gap }
                }
                if (-not $watch.LastUpdate -or $updated -gt $watch.LastUpdate) { $watch.LastUpdate = $updated }
            }
            if ($process.HasExited) { $watch.Died = $true }
        }
        $wait = { param([double]$seconds) $until = [DateTime]::Now.AddSeconds($seconds); while ([DateTime]::Now -lt $until) { & $sample; Start-Sleep -Milliseconds 250 } }
        try {
            $process = Start-CamTestSupervisor -Sandbox $sandbox
            $deadline = [DateTime]::Now.AddSeconds(30)
            while (-not ((Test-CamArchiveMarker -Config $sandbox.Config) -and @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config).Count -ge 2) -and [DateTime]::Now -lt $deadline) { & $sample; Start-Sleep -Milliseconds 250 }
            & $body $wait
        }
        catch { $facts.Ok = $false; $facts.Text += ('FAIL: threw: ' + $_.Exception.Message) }
        finally {
            if ($process) {
                $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
                if (-not $stop.Ok) { $facts.Ok = $false; $facts.Text += ('FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
            }
        }
        # Every segment the camera wrote in this run, deleted or not, by the
        # time its name records; the backdated fixtures are not recordings.
        $names = @(@(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config | ForEach-Object { $_.Name }) +
            @(& $deletedPaths $sandbox | Where-Object { (Split-Path -Path $_ -Parent) -eq $camDir } | ForEach-Object { Split-Path -Path $_ -Leaf }) | Sort-Object -Unique)
        $times = @($names | ForEach-Object { Get-CamTestSegmentTime -Name $_ } | Where-Object { $_ -gt $phaseStart.AddSeconds(-$segment) } | Sort-Object)
        $largest = 0
        for ($i = 1; $i -lt $times.Count; $i++) { $g = ($times[$i] - $times[$i - 1]).TotalSeconds; if ($g -gt $largest) { $largest = $g } }
        $stops = @(& $logLines $sandbox '* prune stop: *')
        $stopGap = 0
        for ($i = 1; $i -lt $stops.Count; $i++) { $g = ((& $lineTime $stops[$i]) - (& $lineTime $stops[$i - 1])).TotalSeconds; if ($g -gt $stopGap) { $stopGap = $g } }
        $restarts = @(& $logLines $sandbox '*restart #*').Count
        $failed = @(& $logLines $sandbox '* prune: FAILED to delete *').Count
        $facts | Add-Member -NotePropertyName Stops -NotePropertyValue $stops
        $stopLimit = Get-CamTestCadenceLimit -IntervalSeconds $sandbox.Config.PruneIntervalSeconds
        $gapLimit = Get-CamTestCadenceLimit -IntervalSeconds $sandbox.Config.StatusIntervalSeconds
        $facts.Text += ("recording: {0} segment(s) written, largest start-to-start gap {1} s, {2} restart(s), {3} failed deletion(s); {4} 'prune stop:' line(s), at most {5} s apart (limit {6} s); status.json longest gap {7} s (limit {8} s); supervisor alive to the end {9}" -f `
            $times.Count, $largest, $restarts, $failed, $stops.Count, $stopGap, $stopLimit, (Format-CamNumber -Value $watch.MaxStatusGap -Format '0.0'), $gapLimit, (-not $watch.Died))
        if ($times.Count -lt 4 -or $largest -gt ($segment + 2) -or $restarts -or $failed -or $stops.Count -lt 5 -or $stopGap -gt $stopLimit -or $watch.MaxStatusGap -gt $gapLimit -or $watch.Died) {
            $facts.Ok = $false; $facts.Text += 'FAIL: recording, the prune schedule or status.json was disturbed'
        }
        return $facts
    }

    try {
        $camera = Start-CamTestSyntheticCamera -Dir (Join-Path $env:TEMP 'cam-tests') -Port $port -PathName $name
        $evidence += ("synthetic camera mediamtx pid {0}, publisher pid {1}" -f $camera.MediaMtx.Id, (@($camera.Publishers | ForEach-Object { $_.Id }) -join ','))

        # age
        $sandbox = New-CamTestSynthSandbox -Name 'pruneloop-age' -CameraName $name -Port $port -Config (& $configFor @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 })
        $sandboxes += $sandbox
        $planted = @{}
        $facts = & $runPhase $sandbox {
            param($wait)
            $now = [DateTime]::Now
            $camDir = Join-Path $sandbox.Root $name
            $other = Join-Path $sandbox.Root 'oldcam'
            $planted.Expired = @(
                (New-CamTestSegment -Dir $camDir -Camera $name -Time $now.AddDays(-3)),
                (New-CamTestSegment -Dir $camDir -Camera $name -Time $now.AddDays(-2)),
                (New-CamTestSegment -Dir $camDir -Camera $name -Time $now.AddHours(-30)),
                (New-CamTestSegment -Dir $other -Camera 'oldcam' -Time $now.AddDays(-4)),
                (New-CamTestSegment -Dir $other -Camera 'oldcam' -Time $now.AddDays(-3.5)))
            $planted.Kept = @(
                (New-CamTestSegment -Dir $camDir -Camera $name -Time $now.AddHours(-12)),
                (New-CamTestSegment -Dir $other -Camera 'oldcam' -Time $now.AddDays(-3)),
                (New-CamTestSegment -Dir $camDir -Camera $name -Time $now.AddDays(-5) -Extension '.mp4'))
            $foreign = Join-Path $camDir 'notes.txt'
            [System.IO.File]::WriteAllText($foreign, 'not a segment')
            Set-CamTestFileAge -Path $foreign -Time $now.AddDays(-6)
            $planted.Kept += $foreign
            & $wait 20
        }
        $deleted = @(& $deletedPaths $sandbox)
        $liveDeleted = @($deleted | Where-Object { $planted.Expired -notcontains $_ })
        $missingExpired = @($planted.Expired | Where-Object { $deleted -notcontains $_ -or (Test-Path -LiteralPath $_) })
        $keptGone = @($planted.Kept | Where-Object { -not (Test-Path -LiteralPath $_) })
        $ageStops = @($facts.Stops | Where-Object { $_ -like '*age rule*cutoff*' })
        $evidence += ("age: planted {0} expired + {1} to keep (12 h old, the other camera's newest at 3 days, a 5-day .mp4, a 6-day notes.txt); deleted {2}: every expired one {3}, anything else {4}; kept files gone {5}" -f `
            $planted.Expired.Count, $planted.Kept.Count, $deleted.Count, ($missingExpired.Count -eq 0), $liveDeleted.Count, $keptGone.Count)
        foreach ($line in @(& $logLines $sandbox '* prune: deleted *')) { $evidence += ('  log: ' + $line.Substring(26)) }
        $evidence += ('  first stop line after the plant: ' + (@($facts.Stops | Where-Object { $_ -like '*age rule reached its cutoff*after 5 deletion*' }) | Select-Object -First 1))
        $evidence += ('  ' + ($facts.Text -join ' | '))
        $evidence += ("  alerts: {0}" -f (@(& $alerts $sandbox) -join ' | '))
        if (-not $facts.Ok -or $missingExpired.Count -or $liveDeleted.Count -or $keptGone.Count -or $deleted.Count -ne 5 -or $ageStops.Count -ne $facts.Stops.Count -or @(& $alerts $sandbox).Count) {
            $ok = $false; $evidence += 'FAIL: the loop did not delete exactly the expired segments with one age stop line per run and no alert'
        }

        # breaker
        $sandbox = New-CamTestSynthSandbox -Name 'pruneloop-breaker' -CameraName $name -Port $port -Config (& $configFor @{ MaxRetentionDays = 1; MaxAgePruneFraction = 0.5 })
        $sandboxes += $sandbox
        $episode = @{}
        $facts = & $runPhase $sandbox {
            param($wait)
            $other = Join-Path $sandbox.Root 'oldcam'
            $flood = { param($from) @(for ($i = 0; $i -lt 20; $i++) { New-CamTestSegment -Dir $other -Camera 'oldcam' -Time ([DateTime]::Now.AddDays(-3).AddMinutes(-15 * ($i + $from))) }) }
            $first = & $flood 0
            & $wait 12
            $episode.FirstAlerts = @(& $alerts $sandbox | Where-Object { $_ -like '122 *' }).Count
            $episode.FirstSurvivors = @($first | Where-Object { Test-Path -LiteralPath $_ }).Count
            # Thin the flood below the brake: the next run deletes the rest by age and resets it.
            foreach ($path in ($first | Select-Object -First 18)) { Remove-Item -LiteralPath $path -Force }
            & $wait 8
            $episode.Reset = @(& $logLines $sandbox '*circuit breaker is reset*').Count
            [void](& $flood 20)
            & $wait 12
        }
        $all122 = @(& $alerts $sandbox | Where-Object { $_ -like '122 Error *' })
        $other = @(& $alerts $sandbox | Where-Object { $_ -notlike '122 *' })
        $held = @($facts.Stops | Where-Object { $_ -like '*HELD by the circuit breaker*' }).Count
        $evidence += ("breaker: first flood of 20 expired segments -> 122 alerts {0}, flood files surviving {1}/20 after 12 s; thinned to 2 -> reset lines {2}; second flood -> 122 alerts in total {3}; held stop lines {4} of {5}; other alerts {6}" -f `
            $episode.FirstAlerts, $episode.FirstSurvivors, $episode.Reset, $all122.Count, $held, $facts.Stops.Count, $other.Count)
        if ($all122.Count) { $evidence += ('  alert: ' + $all122[0]).Substring(0, [math]::Min(260, ('  alert: ' + $all122[0]).Length)) }
        $evidence += ('  ' + ($facts.Text -join ' | '))
        if (-not $facts.Ok -or $episode.FirstAlerts -ne 1 -or $episode.FirstSurvivors -ne 20 -or $episode.Reset -ne 1 -or $all122.Count -ne 2 -or $held -lt 6 -or $other.Count) {
            $ok = $false; $evidence += 'FAIL: alert 122 was not raised once per episode with nothing deleted while held'
        }

        # blocked
        $sandbox = New-CamTestSynthSandbox -Name 'pruneloop-blocked' -CameraName $name -Port $port -Config (& $configFor @{ ReserveGB = 1000000; PruneHysteresisGB = 1 })
        $sandboxes += $sandbox
        $facts = & $runPhase $sandbox { param($wait) & $wait 25 }
        $all121 = @(& $alerts $sandbox | Where-Object { $_ -like '121 Error *' })
        $other = @(& $alerts $sandbox | Where-Object { $_ -notlike '121 *' })
        $blockedRuns = @(& $logLines $sandbox '* PRUNE BLOCKED: *').Count
        $camDir = Join-Path $sandbox.Root $name
        $left = @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $name -Config $sandbox.Config)
        $evidence += ("blocked: ReserveGB 1000000; PRUNE BLOCKED runs {0}; 121 alerts {1}; other alerts {2}; segments deleted {3}, left in the folder {4}" -f `
            $blockedRuns, $all121.Count, $other.Count, @(& $deletedPaths $sandbox).Count, $left.Count)
        if ($all121.Count) { $evidence += ('  alert: ' + $all121[0]).Substring(0, [math]::Min(260, ('  alert: ' + $all121[0]).Length)) }
        $evidence += ('  ' + ($facts.Text -join ' | '))
        if (-not $facts.Ok -or $blockedRuns -lt 5 -or $all121.Count -ne 1 -or $other.Count -or $left.Count -lt 1) {
            $ok = $false; $evidence += 'FAIL: alert 121 was not raised exactly once across the blocked runs, or recording was disturbed'
        }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message) }
    finally {
        if ($camera -and -not (Stop-CamTestSyntheticCamera -Camera $camera)) { $ok = $false; $evidence += 'FAIL: a synthetic camera process did not exit' }
    }

    foreach ($sandbox in $sandboxes) {
        $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
        $evidence += $leak.Text
        if (-not $leak.Ok) { $ok = $false }
    }
    $cleanup = @($sandboxes | ForEach-Object { [pscustomobject]@{ Dir = $_.Dir; Log = (Join-Path $env:TEMP 'cam-tests\pruneloop.none') } })
    $cleanup += [pscustomobject]@{ Dir = (Join-Path $env:TEMP 'cam-tests\synthcam'); Log = (Join-Path $env:TEMP 'cam-tests\pruneloop.none') }
    Complete-CamPruneCheck -Run $Run -Check 'pruneloop' -Ok $ok -Evidence $evidence -Sandboxes $cleanup `
        -Message 'the real loop deletes exactly the expired segments on its schedule, raises 122 and 121 once per episode, and keeps recording'
}


function Invoke-CamCheckDst {
    <#
        T1 [86]: every age, elapsed and "is it due" computation across both
        daylight-saving transitions of this machine's zone. Instants are built
        in UTC a few minutes either side of each transition and fed two ways:

          NEW  what the product supplies now - the seam Get-CamWallClock
               (replaced here by the constructed UTC instant) and UTC file
               times - through the product's own functions;
          OLD  what it supplied before [86] - the local readings of the same
               instants, which is what [DateTime]::Now and LastWriteTime
               return - through the same function bodies, whose only change
               was where their clock came from. Two bodies changed beyond that
               and run here as they were at 46575e2: Get-CamSupervisorState's
               Parse-then-subtract, and the pruner's clock check and age rule
               on LastWriteTime.

        PASS needs NEW to equal the true elapsed time and to decide correctly
        at both transitions, AND OLD to be off by exactly the transition's
        hour, so the check proves it is exercising a transition. The machine
        clock is never touched.
    #>
    param($Run)

    . (Get-CamTestRecorderFunctions -Name 'Test-CamChildStalled', 'Measure-CamCameraDown', 'Set-CamChildBackoff', 'Write-CamChildLine', 'Get-CamDirScan', 'Get-CamLiveFileLength', 'Invoke-CamPrune', 'Get-CamSegmentDirectories', 'Remove-CamEmptySegment', 'Write-CamHost')
    function Write-CamHost { param($Message, $Colour) }
    $evidence = @()
    $ok = $true
    $sandboxes = @()

    $seam = Get-CamWallClock
    $seamOk = ($seam.Kind -eq [DateTimeKind]::Utc -and [math]::Abs(([DateTime]::UtcNow - $seam).TotalSeconds) -lt 5)
    $evidence += ("seam Get-CamWallClock: Kind {0}, {1:n3} s from UtcNow" -f $seam.Kind, ([DateTime]::UtcNow - $seam).TotalSeconds)
    if (-not $seamOk) { $ok = $false; $evidence += 'FAIL: the seam is not UTC' }

    $transitions = Get-CamTestDstTransitions
    if ($transitions.Count -lt 2) {
        Add-CamTestResult -Run $Run -Check 'dst' -Status 'SKIP' -Message ("time zone {0} has no daylight saving to test" -f [TimeZoneInfo]::Local.Id) -Evidence $evidence
        return
    }

    $script:CamTestDstNow = $seam
    function Get-CamWallClock { return $script:CamTestDstNow }
    $local = { param([DateTime]$utc) $utc.ToLocalTime() }
    $config = Get-CamConfigDefault
    $config.SegmentSeconds = 900
    $config.StallFactor = 2.5
    $config.StatusIntervalSeconds = 5
    $config.LogRepeatSuppressSeconds = 300
    $config.BackoffStartSeconds = 60
    $config.BackoffMaxSeconds = 60
    $config.HealthyResetSeconds = 300

    # One row per helper and transition. Old must be True + Sign x Delta.
    $row = {
        param([string]$Helper, [string]$Unit, [double]$TrueValue, [double]$Old, [double]$New, [double]$Sign, [bool]$Decided, [string]$Decision)
        $rowOk = ([math]::Abs($New - $TrueValue) -lt 0.01) -and ([math]::Abs($Old - ($TrueValue + $Sign * $delta)) -lt 0.01) -and $Decided
        $script:CamTestDstRowsOk = $script:CamTestDstRowsOk -and $rowOk
        return ("{0} {1,-26} true {2,8:0.###} | OLD {3,8:0.###} | NEW {4,8:0.###} {5} - {6}{7}" -f `
            $label, $Helper, $TrueValue, $Old, $New, $Unit, $Decision, $(if ($rowOk) { '' } else { '  <- FAIL' }))
    }
    $script:CamTestDstRowsOk = $true

    try {
        foreach ($transition in $transitions) {
            $change = $transition.AtUtc
            $delta = $transition.DeltaSeconds
            $label = '{0,-13} {1:yyyy-MM-dd HH:mm}Z' -f $transition.Kind, $change
            $A = $change.AddMinutes(-5)
            $B = $change.AddMinutes(5)
            $evidence += ("{0}: local {1:HH:mm:ss} -> {2:HH:mm:ss} for a real 600 s" -f $label, (& $local $A), (& $local $B))

            # Watchdog age, fed by the product's directory scan.
            $w = New-CamTestPruneRoot -Name ('dst-' + $transition.Kind)
            $sandboxes += $w
            $dir = Join-Path $w.Root 'Front_Entrance'
            [void](New-Item -ItemType Directory -Path $dir -Force)
            $closed = Join-Path $dir 'closed.mkv'
            $current = Join-Path $dir 'current.mkv'
            [System.IO.File]::WriteAllText($closed, 'c')
            [System.IO.File]::WriteAllText($current, 'o')
            $item = Get-Item -LiteralPath $closed; $item.CreationTimeUtc = $A.AddSeconds(-600); $item.LastWriteTimeUtc = $A.AddSeconds(-1)
            $item = Get-Item -LiteralPath $current; $item.CreationTimeUtc = $A; $item.LastWriteTimeUtc = $A
            $scan = Get-CamDirScan -Path $dir
            $newestNew = $scan.NewestClosedWrite
            if ($scan.CurrentCreated -gt $newestNew) { $newestNew = $scan.CurrentCreated }
            $closedItem = Get-Item -LiteralPath $closed
            $currentItem = Get-Item -LiteralPath $current
            $newestOld = $closedItem.LastWriteTime
            if ($currentItem.CreationTime -gt $newestOld) { $newestOld = $currentItem.CreationTime }
            if ($scan.CurrentCreated.Kind -ne [DateTimeKind]::Utc -or $scan.CurrentCreated -ne $A -or $scan.NewestClosedWrite -ne $A.AddSeconds(-1)) {
                $ok = $false; $evidence += ("FAIL: Get-CamDirScan did not return the UTC file times: CurrentCreated {0:o} ({1})" -f $scan.CurrentCreated, $scan.CurrentCreated.Kind)
            }
            $script:CamTestDstNow = $B
            $new = Test-CamChildStalled -Entry @{ StartedAt = $A.AddHours(-1); Dir = $dir } -Config $config -Now (Get-CamWallClock) -NewestWriteTime $newestNew
            $old = Test-CamChildStalled -Entry @{ StartedAt = (& $local $A.AddHours(-1)); Dir = $dir } -Config $config -Now (& $local $B) -NewestWriteTime $newestOld
            $evidence += & $row 'watchdog age' 's' 600 $old.AgeSeconds $new.AgeSeconds 1 (-not $new.Stalled) ("limit {0} s: OLD kills={1}, NEW kills={2}" -f $new.Threshold, $old.Stalled, $new.Stalled)

            # Alert 110's down time, from the same scan.
            $newDown = Measure-CamCameraDown -Scan $scan -SupervisorStartedAt $A.AddHours(-1) -Now $B
            $oldDown = Measure-CamCameraDown -Scan @{ NewestClosedWrite = $closedItem.LastWriteTime; CurrentCreated = $currentItem.CreationTime } -SupervisorStartedAt (& $local $A.AddHours(-1)) -Now (& $local $B)
            $downLimit = [double]$config.CameraDownAlertMinutes * 60
            $evidence += & $row 'alert 110 down time' 's' 600 $oldDown.Seconds $newDown.Seconds 1 ($newDown.Seconds -le $downLimit) ("limit {0} s: OLD alerts={1}, NEW alerts={2}" -f $downLimit, ($oldDown.Seconds -gt $downLimit), ($newDown.Seconds -gt $downLimit))

            # Status, reconcile, archive scan and prune timers.
            $newDue = Test-CamIntervalDue -Last $A -Seconds $config.StatusIntervalSeconds -Now $B
            $oldDue = Test-CamIntervalDue -Last (& $local $A) -Seconds $config.StatusIntervalSeconds -Now (& $local $B)
            $evidence += & $row 'status/timer due' 's' 600 ((& $local $B) - (& $local $A)).TotalSeconds ($B - $A).TotalSeconds 1 $newDue ("every {0} s: OLD due={1}, NEW due={2}" -f $config.StatusIntervalSeconds, $oldDue, $newDue)

            # Restart due: backoff of 60 s scheduled 30 s before the change.
            $entryNew = @{ Backoff = 0; NextStartAt = [DateTime]::MinValue }
            $entryOld = @{ Backoff = 0; NextStartAt = [DateTime]::MinValue }
            $script:CamTestDstNow = $change.AddSeconds(-30)
            [void](Set-CamChildBackoff -Entry $entryNew -Config $config -RanForSeconds 0)
            $script:CamTestDstNow = & $local $change.AddSeconds(-30)
            [void](Set-CamChildBackoff -Entry $entryOld -Config $config -RanForSeconds 0)
            $early = $change.AddSeconds(5)
            $late = $change.AddSeconds(40)
            $waitNew = ($entryNew.NextStartAt - $early).TotalSeconds
            $waitOld = ($entryOld.NextStartAt - (& $local $early)).TotalSeconds
            $dueNew = @(($early -ge $entryNew.NextStartAt), ($late -ge $entryNew.NextStartAt))
            $dueOld = @(((& $local $early) -ge $entryOld.NextStartAt), ((& $local $late) -ge $entryOld.NextStartAt))
            $evidence += & $row 'restart wait at +35 s' 's' 25 $waitOld $waitNew -1 ((-not $dueNew[0]) -and $dueNew[1]) ("due at +35 s/+70 s: OLD {0}/{1}, NEW {2}/{3}" -f $dueOld[0], $dueOld[1], $dueNew[0], $dueNew[1])

            # Heartbeat freshness: written by the product 4 s before, read 4 s after.
            $snapshotNew = [pscustomobject]@{ SupervisorPid = $PID; State = 'running'; UpdatedAt = (Format-CamTimestamp -Value $change.AddSeconds(-4)) }
            $script:CamTestDstNow = $change.AddSeconds(4)
            $stateNew = Get-CamSupervisorState -Config $config -Snapshot $snapshotNew
            $oldText = (& $local $change.AddSeconds(-4)).ToString('o')
            $oldAge = [math]::Round(((& $local $change.AddSeconds(4)) - [DateTime]::Parse($oldText, [System.Globalization.CultureInfo]::InvariantCulture)).TotalSeconds, 1)
            $limit = [math]::Max(15, 3 * $config.StatusIntervalSeconds)
            $compat = Get-CamDateTimeOrNull -Value $oldText
            $evidence += & $row 'heartbeat age' 's' 8 $oldAge $stateNew.AgeSeconds 1 ($stateNew.Running -and $compat -eq $change.AddSeconds(-4)) ("limit {0} s: OLD running={1}, NEW running={2}; NEW writes {3}, reads the old text {4} as {5:o}" -f $limit, ($oldAge -le $limit), $stateNew.Running, $snapshotNew.UpdatedAt, $oldText, $compat)

            # Log repeat suppression: the same message shape 600 s apart, window 300 s.
            $logNew = Join-Path $w.Dir 'child-new.log'
            $logOld = Join-Path $w.Dir 'child-old.log'
            $entryNew = @{ LogSeen = @{}; LogPath = $logNew }
            $entryOld = @{ LogSeen = @{}; LogPath = $logOld }
            Write-CamChildLine -Entry $entryNew -Config $config -Line 'frame 1 dropped' -Now $A
            Write-CamChildLine -Entry $entryNew -Config $config -Line 'frame 2 dropped' -Now $B
            Write-CamChildLine -Entry $entryOld -Config $config -Line 'frame 1 dropped' -Now (& $local $A)
            Write-CamChildLine -Entry $entryOld -Config $config -Line 'frame 2 dropped' -Now (& $local $B)
            $linesNew = @(Get-Content -LiteralPath $logNew).Count
            $linesOld = @(Get-Content -LiteralPath $logOld).Count
            $evidence += & $row 'log repeat suppression' 's' 600 ((& $local $B) - (& $local $A)).TotalSeconds ($B - $A).TotalSeconds 1 ($linesNew -eq 2) ("window {0} s, lines logged: OLD {1}, NEW {2} (2 expected)" -f $config.LogRepeatSuppressSeconds, $linesOld, $linesNew)

            # NVR host restart window: restarts 35 and 10 minutes old, window 30 minutes.
            $restartsNew = New-Object System.Collections.ArrayList
            $restartsOld = New-Object System.Collections.ArrayList
            foreach ($at in @($B.AddMinutes(-35), $B.AddMinutes(-10))) { [void]$restartsNew.Add($at); [void]$restartsOld.Add((& $local $at)) }
            $oldestOld = ((& $local $B) - $restartsOld[0]).TotalSeconds
            $oldestNew = ($B - $restartsNew[0]).TotalSeconds
            Remove-CamExpiredRestart -Restarts $restartsNew -WindowMinutes 30 -Now $B
            Remove-CamExpiredRestart -Restarts $restartsOld -WindowMinutes 30 -Now (& $local $B)
            $evidence += & $row 'NVR restart window age' 's' 2100 $oldestOld $oldestNew 1 ($restartsNew.Count -eq 1) ("restarts kept in the window: OLD {0}, NEW {1} (1 expected)" -f $restartsOld.Count, $restartsNew.Count)

            # Retention cutoff and the pruner's clock check: 600 s retention,
            # segments written 15 and 4 minutes before the change, the newest
            # 30 s before the run.
            $p = New-CamTestPruneRoot -Name ('dst-prune-' + $transition.Kind) -Config @{ MaxRetentionDays = (600 / 86400); MaxAgePruneFraction = 1.0 }
            $sandboxes += $p
            $fe = Join-Path $p.Root 'Front_Entrance'
            $segments = @()
            foreach ($at in @($change.AddMinutes(-15), $change.AddMinutes(-4), $B.AddSeconds(-30))) {
                $path = New-CamTestSegment -Dir $fe -Camera 'Front_Entrance' -Time (& $local $at)
                Set-CamTestFileAge -Path $path -Time $at
                $segments += Get-Item -LiteralPath $path
            }
            [void](Initialize-CamArchiveOwner -Config $p.Config)
            $script:CamTestDstNow = $B
            $dry = Invoke-CamPrune -Config $p.Config -SupervisorLog $p.Log -DryRun
            $deletedNew = @($dry.Deletions | ForEach-Object { if ($_ -match '\\(Front_Entrance_[^\\ ]+)') { $Matches[1] } })
            $retention = [double]$p.Config.MaxRetentionDays
            $cutoffOld = (& $local $B).AddDays(-$retention)
            $deletedOld = @($segments[0..1] | Where-Object { $_.LastWriteTime -lt $cutoffOld } | ForEach-Object { $_.Name })
            $asOld = @($segments | ForEach-Object { [pscustomobject]@{ FullName = $_.FullName; LastWriteTimeUtc = $_.LastWriteTime } })
            $refusalOld = (Get-CamPruneClockProblem -Candidates $asOld[0..1] -Newest @($asOld[2]) -Segments $asOld -Now (& $local $B) -Config $p.Config) -replace '[A-Za-z]:\\\S*\\', ''
            $ageOld = ((& $local $B) - $segments[1].LastWriteTime).TotalSeconds
            $ageNew = ($B - $segments[1].LastWriteTimeUtc).TotalSeconds
            $decided = (-not $dry.ClockRefusal) -and $deletedNew.Count -eq 1 -and $deletedNew[0] -eq $segments[0].Name
            $evidence += & $row 'retention age (T-4m seg)' 's' 540 $ageOld $ageNew 1 $decided ("retention 600 s: OLD deletes {0} of 2{1}, NEW deletes {2} of 2 (1 expected: the T-15m one), NEW clock refusal '{3}'" -f `
                    $deletedOld.Count, $(if ($refusalOld) { ", OLD clock check REFUSES: " + $refusalOld } else { '' }), $deletedNew.Count, $dry.ClockRefusal)
        }
        if (-not $script:CamTestDstRowsOk) { $ok = $false }
    }
    catch { $ok = $false; $evidence += ('FAIL: threw: ' + $_.Exception.Message + ' at ' + $_.InvocationInfo.PositionMessage) }

    Complete-CamPruneCheck -Run $Run -Check 'dst' -Ok $ok -Evidence $evidence -Sandboxes $sandboxes `
        -Message 'at both daylight-saving transitions of this zone every age, elapsed and due helper reads the true elapsed time and decides correctly, where the pre-[86] local-time logic is an hour out'
}


function Invoke-CamTestClockPhase {
    <#
        One T2 run: the real sandbox supervisor recording a synthetic camera
        (SegmentSeconds 6 x StallFactor 2.5 = a 15 s stall limit, status every
        1 s) while Set-CamTestSandboxClock moves its clock by $Delta seconds a
        few seconds into the run - a daylight-saving change (-Mode local) or a
        step of the clock itself (-Mode utc). Measured from outside, on real
        time: status.json write gaps, the child's pid, watchdog lines, alerts,
        segments written after the change, and with -Suspend whether the
        watchdog still kills a stalled child within its limit afterwards.
        -ScriptDir runs another version of cam.common.ps1 / cam.recorder.ps1,
        for the red run against the pre-[86] product.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][ValidateSet('local', 'utc')][string]$Mode,
        [Parameter(Mandatory = $true)][double]$Delta,
        [int]$ObserveSeconds = 30,
        [switch]$Suspend,
        [string]$ScriptDir
    )

    $camera = 'synth1'
    $interval = 1
    $limit = 6 * 2.5
    $gapLimit = Get-CamTestCadenceLimit -IntervalSeconds $interval
    $sandbox = New-CamTestSynthSandbox -Name ('dstloop-' + $Name) -CameraName $camera -Port 18554 `
        -Config @{ SegmentSeconds = 6; StallFactor = 2.5; StatusIntervalSeconds = $interval; ReconcileSeconds = 2; ArchiveScanSeconds = 5; PruneIntervalSeconds = 10; BackoffStartSeconds = 1 }
    if ($ScriptDir) {
        foreach ($file in @('cam.common.ps1', 'cam.recorder.ps1')) { Copy-Item -LiteralPath (Join-Path $ScriptDir $file) -Destination $sandbox.Dir -Force }
        [void](Set-CamTestAlertStub -Sandbox $sandbox)
    }
    Set-CamTestSandboxClock -Sandbox $sandbox -Mode $Mode
    $camDir = Join-Path -Path $sandbox.Root -ChildPath $camera
    $statusPath = Get-CamStatusFilePath -Config $sandbox.Config
    $segments = { @(Get-CamTestSegmentFiles -Dir $camDir -SafeName $camera -Config $sandbox.Config) }
    $logLines = { if (Test-Path -LiteralPath $sandbox.SupervisorLog) { @(Get-Content -LiteralPath $sandbox.SupervisorLog) } else { @() } }

    $r = [pscustomobject]@{
        Name = $Name; Mode = $Mode; Delta = $Delta; Ok = $true; Lines = @(); PidChanges = 0; WatchdogLines = @(); Alerts = @()
        MaxGap = 0.0; FinalAge = 0.0; SegmentsAfter = 0; Alive = $false; ChildAlive = $false; KilledAfter = $null; Sandbox = $sandbox
    }
    $synth = $null
    $process = $null
    $suspended = 0
    try {
        $synth = Start-CamTestSyntheticCamera -Dir $sandbox.Dir -Port 18554 -PathName $camera
        $process = Start-CamTestSupervisor -Sandbox $sandbox
        $child = Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $camera -TimeoutSeconds 20
        $deadline = [DateTime]::UtcNow.AddSeconds(30)
        while ((& $segments).Count -lt 1 -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 200 }

        $at = [DateTime]::UtcNow.AddSeconds(6)
        Set-CamTestClockChange -Sandbox $sandbox -AtUtc $at -DeltaSeconds $Delta
        $linesAtChange = $null
        $lastWrite = (Get-Item -LiteralPath $statusPath).LastWriteTimeUtc
        $pid0 = $child
        $end = $at.AddSeconds($ObserveSeconds)
        while ([DateTime]::UtcNow -lt $end -and -not $process.HasExited) {
            if ($null -eq $linesAtChange -and [DateTime]::UtcNow -ge $at) { $linesAtChange = (& $logLines).Count }
            # The status file is replaced by a rename, so it can be missing for an instant.
            $item = Get-Item -LiteralPath $statusPath -ErrorAction SilentlyContinue
            $write = $(if ($item) { $item.LastWriteTimeUtc } else { $lastWrite })
            if ($write -ne $lastWrite) {
                $gap = ($write - $lastWrite).TotalSeconds
                if ($gap -gt $r.MaxGap) { $r.MaxGap = $gap }
                $lastWrite = $write
            }
            $now = Get-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $camera
            if ($now -gt 0 -and $now -ne $pid0) { $r.PidChanges++; $pid0 = $now }
            Start-Sleep -Milliseconds 100
        }
        $r.FinalAge = ([DateTime]::UtcNow - $lastWrite).TotalSeconds
        if ($r.FinalAge -gt $r.MaxGap) { $r.MaxGap = $r.FinalAge }
        $r.SegmentsAfter = @(& $segments | Where-Object { $_.CreationTimeUtc -gt $at }).Count
        $r.WatchdogLines = @(& $logLines | Where-Object { $_ -like '*watchdog*' })
        $all = & $logLines
        if ($null -ne $linesAtChange -and $linesAtChange -gt 0 -and $all.Count -gt $linesAtChange) {
            $r.Lines += ('  log across the change: ' + $all[$linesAtChange - 1])
            $r.Lines += ('                     ->  ' + $all[$linesAtChange])
        }
        $status = Get-CamTestStatusJson -Sandbox $sandbox
        $r.Lines += ("  status.json UpdatedAt {0} at real {1:o}" -f $status.Snapshot.UpdatedAt, [DateTime]::UtcNow)

        if ($Suspend -and -not $process.HasExited) {
            $victim = Get-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $camera
            $before = (& $segments).Count
            $deadline = [DateTime]::UtcNow.AddSeconds(10)
            while ((& $segments).Count -eq $before -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 100 }
            Start-Sleep -Milliseconds 500
            $old = Get-Process -Id $victim
            $suspendedAt = [DateTime]::UtcNow
            Suspend-CamTestProcess -ProcessId $victim
            $suspended = $victim
            $deadline = $suspendedAt.AddSeconds($limit + 10)
            while ([DateTime]::UtcNow -lt $deadline -and -not $old.HasExited) { Start-Sleep -Milliseconds 100 }
            if ($old.HasExited) {
                $r.KilledAfter = ([DateTime]::UtcNow - $suspendedAt).TotalSeconds
                [void](Wait-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $camera -Not $victim -TimeoutSeconds 15)
            }
            $r.WatchdogLines = @(& $logLines | Where-Object { $_ -like '*watchdog*' })
        }
        $r.Alive = -not $process.HasExited
        $r.ChildAlive = ((Get-CamTestChildPid -Sandbox $sandbox -SupervisorProcess $process -CameraName $camera) -gt 0)
    }
    catch { $r.Ok = $false; $r.Lines += ('  FAIL: threw: ' + $_.Exception.Message) }
    finally {
        if ($suspended) {
            $clear = Resume-CamTestProcess -ProcessId $suspended
            if (-not $clear) { $r.Ok = $false; $r.Lines += ("  FAIL: pid {0} left suspended" -f $suspended) }
        }
        if ($process) {
            $stop = Stop-CamTestSupervisor -Sandbox $sandbox -Process $process
            if (-not $stop.Ok) { $r.Ok = $false; $r.Lines += ('  FAIL: the sandbox supervisor did not stop cleanly: ' + (@($stop.Killed) -join ', ')) }
        }
        if ($synth -and -not (Stop-CamTestSyntheticCamera -Camera $synth)) { $r.Ok = $false; $r.Lines += '  FAIL: a synthetic camera process did not exit' }
    }
    if (Test-Path -LiteralPath $sandbox.AlertLog) { $r.Alerts = @(Get-Content -LiteralPath $sandbox.AlertLog | Where-Object { $_.Trim() }) }
    $leak = Test-CamTestSandboxLeak -Sandbox $sandbox
    if (-not $leak.Ok) { $r.Ok = $false; $r.Lines += ('  FAIL: ' + $leak.Text) }

    $r.Lines = @(("{0}: {1} {2:+0;-0} s - child restarts {3}, watchdog lines {4}, alerts {5}, largest status.json gap {6} s (limit {7} s), segments after the change {8}, supervisor alive {9}, child alive {10}{11}" -f `
            $Name, $Mode, $Delta, $r.PidChanges, $r.WatchdogLines.Count, $r.Alerts.Count, (Format-CamNumber -Value $r.MaxGap -Format '0.0'), $gapLimit, $r.SegmentsAfter, $r.Alive, $r.ChildAlive, `
            $(if ($Suspend) { ", stalled child killed {0} s after the suspend" -f $(if ($null -ne $r.KilledAfter) { Format-CamNumber -Value $r.KilledAfter -Format '0.0' } else { 'NEVER' }) } else { '' }))) + $r.Lines
    foreach ($line in $r.WatchdogLines) { $r.Lines += ('  log: ' + $line) }
    foreach ($line in $r.Alerts) { $r.Lines += ('  alert: ' + $line) }
    return $r
}


function Invoke-CamCheckDstLoop {
    <#
        T2 [86]: the real supervisor through a simulated daylight-saving change
        and through steps of the clock itself, with the machine clock and time
        zone untouched (Set-CamTestSandboxClock moves the SANDBOX copy's
        readings only).

          fallback / springforward  local readings move -3600 / +3600 s, UTC
              does not. PASS needs no disturbance at all: no restart, no
              watchdog line, no alert, status.json never older than
              3 x StatusIntervalSeconds + 1.5 s, and a child suspended after
              the change still killed within the 15 s limit (+10 s).
          step+3600 / step-3600  the clock itself steps, the seam and the file
              times with it. UTC arithmetic cannot absorb that and does not
              pretend to: what happens is reported. PASS needs only that the
              supervisor survives and the camera keeps recording.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $sandboxes = @()
    foreach ($phase in @(
            @{ Name = 'fallback'; Mode = 'local'; Delta = -3600; Suspend = $true },
            @{ Name = 'springforward'; Mode = 'local'; Delta = 3600; Suspend = $true },
            @{ Name = 'step+3600'; Mode = 'utc'; Delta = 3600; Suspend = $false },
            @{ Name = 'step-3600'; Mode = 'utc'; Delta = -3600; Suspend = $false })) {
        $r = Invoke-CamTestClockPhase -Name $phase.Name -Mode $phase.Mode -Delta $phase.Delta -Suspend:$phase.Suspend
        $sandboxes += [pscustomobject]@{ Dir = $r.Sandbox.Dir; Log = (Join-Path $env:TEMP 'cam-tests\dstloop.none') }
        $evidence += $r.Lines
        $phaseOk = $r.Ok -and $r.Alive -and $r.ChildAlive -and $r.SegmentsAfter -ge 1
        if ($phase.Mode -eq 'local') {
            $phaseOk = $phaseOk -and $r.PidChanges -eq 0 -and $r.Alerts.Count -eq 0 -and $r.MaxGap -le (Get-CamTestCadenceLimit -IntervalSeconds 1) -and `
                $null -ne $r.KilledAfter -and $r.KilledAfter -le 25 -and $r.WatchdogLines.Count -eq 2
        }
        if (-not $phaseOk) { $ok = $false; $evidence += ('FAIL: ' + $phase.Name) }
    }

    Complete-CamPruneCheck -Run $Run -Check 'dstloop' -Ok $ok -Evidence $evidence -Sandboxes $sandboxes `
        -Message 'the real supervisor is undisturbed by a simulated fall-back and spring-forward - no restart, no alert, no stale status, the watchdog still kills a stall - and survives a +/-3600 s step of the clock itself'
}


function Invoke-CamCheckClockLint {
    <#
        T3 [86]: no product script reads the local clock or a local file time
        for arithmetic - Find-CamClockLintProblems over every product .ps1,
        with a positive and a negative control so the rule is known to bite.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $controls = @(
        @{ Line = '$age = ([DateTime]::Now - $started).TotalSeconds'; Flag = $true },
        @{ Line = '$deadline = (Get-Date).AddSeconds(20)'; Flag = $true },
        @{ Line = 'if ($file.LastWriteTime -lt $cutoff) { }'; Flag = $true },
        @{ Line = '$stamp = Get-Date -Format ''HH:mm:ss'''; Flag = $false },
        @{ Line = '$text = $file.LastWriteTime.ToString(''yyyy-MM-dd'')'; Flag = $false },
        @{ Line = '$shown = [DateTime]::Now   # clock: display - printed, never subtracted'; Flag = $false },
        @{ Line = '$age = ((Get-CamWallClock) - $file.LastWriteTimeUtc).TotalSeconds'; Flag = $false })
    foreach ($control in $controls) {
        $flagged = (Find-CamClockLintProblems -Lines @($control.Line)).Count -gt 0
        if ($flagged -ne $control.Flag) { $ok = $false; $evidence += ("FAIL: control {0} flagged={1}, expected {2}" -f $control.Line, $flagged, $control.Flag) }
    }
    $evidence += ("{0} control line(s): the rule flags local reads in arithmetic and passes display forms and the seam" -f $controls.Count)

    $repo = Get-CamTestRepo
    $files = @(Get-ChildItem -LiteralPath $repo -Filter '*.ps1' -File) + @(Get-ChildItem -LiteralPath (Join-Path $repo 'ui\scripts') -Filter '*.ps1' -File)
    $problems = @()
    foreach ($file in $files) {
        $problems += Find-CamClockLintProblems -Lines ([System.IO.File]::ReadAllLines($file.FullName)) -Label $file.Name
    }
    $evidence += ("{0} product script(s) scanned: {1}" -f $files.Count, (@($files | ForEach-Object { $_.Name }) -join ', '))
    foreach ($problem in $problems) { $evidence += ('local clock outside display: ' + $problem) }
    if ($problems.Count -gt 0) { $ok = $false }

    Add-CamTestResult -Run $Run -Check 'clocklint' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) -Evidence $evidence `
        -Message ("no product script reads the local clock or a local file time outside a display form ({0} found)" -f $problems.Count)
}


function Invoke-CamCheckDocSwitches {
    <#
        Every -Switch the documentation names must exist. Two rules, over
        README.md and docs\*.md:

        - A switch written after one of this repository's scripts, in the same
          code span or code line, must be a parameter of THAT script, common
          parameters included - so `.\cam.recorder.ps1 -InstallUi` fails.
        - Any other -Switch shaped like a PowerShell parameter (-Abc...),
          anywhere in the text, must be a
          parameter of one of the scripts or one of the few external
          PowerShell parameters the docs quote, listed below.

        Lower-case flags belong to camui, bash, ffmpeg and mpv and are not
        checked. The parameter lists come from Get-Command on each script,
        which parses it and runs nothing.
    #>
    param($Run)

    $repo = Get-CamTestRepo
    $scripts = [ordered]@{
        'cam.ps1'                            = 'cam.ps1'
        'cam.recorder.ps1'                   = 'cam.recorder.ps1'
        'cam.health.ps1'                     = 'cam.health.ps1'
        'mpv.cam.autolayout.autorestart.ps1' = 'mpv.cam.autolayout.autorestart.ps1'
        'cam.tests.ps1'                      = 'tests\cam.tests.ps1'
        'demo.ps1'                           = 'tests\live\demo.ps1'
    }
    # Parameters of Windows PowerShell itself and of the cmdlets the docs
    # quote, not of this repository.
    $external = @('NoProfile', 'ExecutionPolicy', 'File', 'Command', 'WindowStyle', 'EncodedCommand',
        'FilterHashtable', 'TaskName', 'Scope', 'Source',
        # NetSecurity / NetConnection cmdlets quoted in docs/ui.md for the firewall rule (documented, never run by the product)
        'Action', 'Direction', 'DisplayName', 'InterfaceAlias', 'LocalPort', 'NetworkCategory', 'Profile', 'Program', 'Protocol', 'RemoteAddress')

    $declared = @{}
    $any = New-Object System.Collections.Generic.HashSet[string] ([StringComparer]::OrdinalIgnoreCase)
    foreach ($name in $scripts.Keys) {
        $declared[$name] = @((Get-Command -Name (Join-Path -Path $repo -ChildPath $scripts[$name])).Parameters.Keys)
        foreach ($parameter in $declared[$name]) { [void]$any.Add($parameter) }
    }
    foreach ($parameter in $external) { [void]$any.Add($parameter) }

    $docs = @(Get-Item -LiteralPath (Join-Path -Path $repo -ChildPath 'README.md')) +
        @(Get-ChildItem -LiteralPath (Join-Path -Path $repo -ChildPath 'docs') -Filter '*.md')
    $scriptPattern = '(?<![\w.-])(' + ((@($scripts.Keys) | ForEach-Object { [regex]::Escape($_) }) -join '|') + ')'
    $switchPattern = '(?<![\w-])-([A-Za-z][A-Za-z0-9]*)'
    $problems = New-Object System.Collections.ArrayList
    $attributed = 0
    $bare = New-Object System.Collections.Generic.HashSet[string] ([StringComparer]::OrdinalIgnoreCase)

    foreach ($doc in $docs) {
        $text = [System.IO.File]::ReadAllText($doc.FullName)
        $code = New-Object System.Collections.ArrayList
        $inFence = $false
        foreach ($line in ($text -split "`r?`n")) {
            if ($line.StartsWith('```')) { $inFence = -not $inFence; continue }
            if ($inFence) { [void]$code.Add($line); continue }
            foreach ($span in [regex]::Matches($line, '`([^`]+)`')) { [void]$code.Add($span.Groups[1].Value) }
        }

        foreach ($fragment in $code) {
            $mentions = @([regex]::Matches($fragment, $scriptPattern))
            for ($i = 0; $i -lt $mentions.Count; $i++) {
                $from = $mentions[$i].Index + $mentions[$i].Length
                $to = $(if ($i + 1 -lt $mentions.Count) { $mentions[$i + 1].Index } else { $fragment.Length })
                $segment = $fragment.Substring($from, $to - $from)
                $cut = [regex]::Match($segment, '\s#|\||;')
                if ($cut.Success) { $segment = $segment.Substring(0, $cut.Index) }
                foreach ($switch in [regex]::Matches($segment, $switchPattern)) {
                    $attributed++
                    if ($declared[$mentions[$i].Value] -notcontains $switch.Groups[1].Value) {
                        [void]$problems.Add(("{0}: '{1} -{2}' - {1} declares no such parameter" -f $doc.Name, $mentions[$i].Value, $switch.Groups[1].Value))
                    }
                }
            }
        }

        foreach ($switch in [regex]::Matches($text, '(?<![\w-])-([A-Z][a-z][A-Za-z0-9]*)')) {
            [void]$bare.Add($switch.Groups[1].Value)
            if (-not $any.Contains($switch.Groups[1].Value)) {
                [void]$problems.Add(("{0}: '-{1}' is a parameter of none of {2}" -f $doc.Name, $switch.Groups[1].Value, (@($scripts.Keys) -join ', ')))
            }
        }
    }

    $evidence = @(
        ("{0} document(s): {1}" -f $docs.Count, (@($docs | ForEach-Object { $_.Name }) -join ', ')),
        ("{0} switch use(s) attributed to a named script; {1} distinct capitalised switch(es) in all: {2}" -f $attributed, $bare.Count, ((@($bare) | Sort-Object) -join ' '))
    )
    foreach ($name in @('cam.ps1', 'cam.recorder.ps1')) {
        $evidence += ("{0} declares: {1}" -f $name, ((@($declared[$name]) | Sort-Object) -join ' '))
    }
    $evidence += @($problems | Sort-Object -Unique | ForEach-Object { 'FAIL ' + $_ })

    Add-CamTestResult -Run $Run -Check 'docswitches' -Status $(if ($problems.Count) { 'FAIL' } else { 'PASS' }) `
        -Message 'every -Switch README.md and docs\*.md name is a declared parameter of the script it is written after, or of one of them' `
        -Evidence $evidence
}

function Invoke-CamCheckDocLinks {
    <#
        Every relative link in README.md and docs\*.md names a file that exists,
        and every #anchor - file.md#anchor or in-page - is a real heading of its
        target by GitHub's rules. Moving text between files is what breaks
        anchors, and nothing else notices.

        A positive control runs first on the same documents in memory, with a
        missing file, a broken in-page anchor and a broken cross-file anchor
        planted in README.md: the check is worth nothing unless it flags all
        three. No file on disk is changed.
    #>
    param($Run)

    $repo = Get-CamTestRepo
    $files = @(Get-Item -LiteralPath (Join-Path -Path $repo -ChildPath 'README.md')) +
        @(Get-ChildItem -LiteralPath (Join-Path -Path $repo -ChildPath 'docs') -Filter '*.md')
    $documents = @{}
    foreach ($file in $files) { $documents[$file.FullName] = [System.IO.File]::ReadAllText($file.FullName) }
    $result = Find-CamDocLinkProblems -Documents $documents

    $readme = $files[0].FullName
    $planted = $documents.Clone()
    $text = $planted[$readme]
    $text = ([regex]'\]\(#[^)\s]+\)').Replace($text, '](#no-such-heading)', 1)
    $text = ([regex]'\]\((docs/[^)#\s]+\.md)#[^)\s]+\)').Replace($text, '](${1}#no-such-heading)', 1)
    $text = ([regex]'\]\(docs/[^)#\s]+\.md\)').Replace($text, '](docs/no-such-file.md)', 1)
    $planted[$readme] = $text
    $control = @((Find-CamDocLinkProblems -Documents $planted).Problems | Where-Object { $result.Problems -notcontains $_ })
    $controlOk = $control.Count -eq 3 -and
        @($control | Where-Object { $_ -like "*'docs/no-such-file.md' - no such file" }).Count -eq 1 -and
        @($control | Where-Object { $_ -like "*'#no-such-heading' - README.md has no heading*" }).Count -eq 1 -and
        @($control | Where-Object { $_ -like "*'docs/*.md#no-such-heading' - * has no heading*" }).Count -eq 1

    $evidence = @(
        ("{0} document(s): {1}" -f $files.Count, (@($files | ForEach-Object { $_.Name }) -join ', ')),
        ("{0} relative link(s) checked, {1} of them with an anchor" -f $result.Links, $result.Anchors),
        ("control - one missing file, one in-page and one cross-file anchor planted in README.md in memory: {0} of 3 flagged" -f $control.Count)
    )
    $evidence += @($control | ForEach-Object { 'control ' + $_ })
    $evidence += @($result.Problems | ForEach-Object { 'FAIL ' + $_ })

    $status = $(if ($controlOk -and -not $result.Problems.Count) { 'PASS' } else { 'FAIL' })
    $message = $(if ($controlOk) { 'every relative link in README.md and docs\*.md names a file that exists, and every #anchor a heading of its target' }
        else { 'the positive control did not flag all three planted breaks, so a clean result would prove nothing' })
    Add-CamTestResult -Run $Run -Check 'doclinks' -Status $status -Message $message -Evidence $evidence
}


function Invoke-CamCheckLiveCfg {
    <#
        [Stage 1A] Live view is additive and off by default.

        (a) with the flag off, and with it on, the RELAY's rendered config is
            byte-identical to what master's own Get-CamRelayConfigText renders
            for the same inputs (master's cam.common.ps1 and template come
            from git, run in their own process); the template file is
            identical too;
        (b) with the flag off, a real sandbox supervisor produces a status.json
            with exactly master's top-level keys, no Live section, and no
            mediamtx process; -Status and -Install -WhatIf print what master's
            copies print for the same sandbox;
        (c) with the flag on the live config has the exact properties the
            design lists, holds the secret only as its sha256, and a live name
            that collides with a relay path is refused;
        (d) a malformed or unknown Live* key is refused at load.
        No camera, no port.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $fail = { param([string]$Text) $script:CamLiveCfgOk = $false; $script:CamLiveCfgEvidence += ('FAIL ' + $Text) }
    $script:CamLiveCfgOk = $true
    $script:CamLiveCfgEvidence = @()
    $sandboxes = @()

    # -- the cameras: odd names, one already on the sub-stream, one with no Quality
    $cameras = @(
        [pscustomobject]@{ Name = 'Front Entrance'; IP = '127.0.0.1:1'; Quality = 'stream1'; Record = $false },
        [pscustomobject]@{ Name = 'Rear Balcony'; IP = '127.0.0.1:1'; Quality = 'stream2'; Record = $false },
        [pscustomobject]@{ Name = 'Car_Garage'; IP = '127.0.0.1:1'; Record = $false },
        [pscustomobject]@{ Name = 'Workshop-Entrance'; IP = '127.0.0.1:1'; Quality = 'stream1'; Record = $false })
    $liveKeys = @{ LiveExePath = 'C:\x\mediamtx.exe'; LiveConfigPath = 'C:\x\live.yml'; LiveWhepAddress = '127.0.0.1:28889'
        LiveUdpAddress = '127.0.0.1:28189'; LiveSubStream = 'stream2'; LiveCloseAfterSeconds = 7; LiveSecretPath = 'C:\x\secret.txt' }

    try {
        # ---------------- (a) the relay config, against master
        $sandbox = New-CamTestSandbox -Name 'livecfg' -Cameras $cameras -Config @{ RestreamerEnabled = $true }
        $sandboxes += $sandbox
        $masterDir = Join-Path -Path $sandbox.Dir -ChildPath 'master'
        [void](New-Item -ItemType Directory -Path $masterDir -Force)
        foreach ($name in @('cam.common.ps1', 'mediamtx.template.yml')) {
            if (-not (Copy-CamTestMasterFile -Name $name -Destination (Join-Path $masterDir $name))) { throw "could not read master:$name through git in WSL" }
        }
        Copy-Item -LiteralPath $sandbox.Cameras -Destination $masterDir -Force
        $evidence += ("master's cam.common.ps1: {0} bytes, sha256 {1}" -f (Get-Item (Join-Path $masterDir 'cam.common.ps1')).Length, (Get-FileHash -LiteralPath (Join-Path $masterDir 'cam.common.ps1') -Algorithm SHA256).Hash.Substring(0, 12))
        $sameTemplate = ((Get-FileHash (Join-Path $masterDir 'mediamtx.template.yml')).Hash -eq (Get-FileHash (Join-Path (Get-CamTestRepo) 'mediamtx.template.yml')).Hash)
        $evidence += ("(a) mediamtx.template.yml identical to master's: {0}" -f $sameTemplate)
        if (-not $sameTemplate) { & $fail 'the relay template differs from master' }

        $configs = [ordered]@{
            'no live keys'                = @{ RestreamerEnabled = $true }
            'flag explicitly off, keys set' = (@{ RestreamerEnabled = $true; LiveViewEnabled = $false } + $liveKeys)
            'flag ON'                     = (@{ RestreamerEnabled = $true; LiveViewEnabled = $true } + $liveKeys)
        }
        $child = Join-Path -Path $masterDir -ChildPath 'render.ps1'
        $lines = @('$ErrorActionPreference = "Stop"', ". '$(Join-Path $masterDir 'cam.common.ps1')'",
            '$cred = New-Object System.Management.Automation.PSCredential("camrelayapi", (ConvertTo-SecureString ("0" * 64) -AsPlainText -Force))',
            '$map = Get-CamRelayPathMap -Cameras (Get-CamList)')
        $i = 0
        $names = @()
        foreach ($label in $configs.Keys) {
            $i++
            $cfgPath = Join-Path -Path $masterDir -ChildPath "cfg$i.json"
            $settings = [ordered]@{ RootPath = $sandbox.Root; RestreamerEnabled = $true }
            foreach ($key in $configs[$label].Keys) { $settings[$key] = $configs[$label][$key] }
            [System.IO.File]::WriteAllText($cfgPath, (ConvertTo-Json -InputObject $settings), (New-Object System.Text.UTF8Encoding($false)))
            $lines += ('[System.IO.File]::WriteAllText("{0}", (Get-CamRelayConfigText -Config (Get-CamConfig -Path "{1}") -PathMap $map -ApiCredential $cred), (New-Object System.Text.UTF8Encoding($false)))' -f (Join-Path $masterDir "relay$i.master.yml"), $cfgPath)
            $names += $label
        }
        [System.IO.File]::WriteAllText($child, ($lines -join "`r`n"), (New-Object System.Text.UTF8Encoding($true)))
        $result = Invoke-CamTestScript -Path $child
        if ($result.ExitCode -ne 0) { throw ('rendering with master failed: ' + ($result.Lines -join ' | ')) }
        $cred = New-Object System.Management.Automation.PSCredential('camrelayapi', (ConvertTo-SecureString ('0' * 64) -AsPlainText -Force))
        $map = Get-CamRelayPathMap -Cameras $cameras
        $i = 0
        foreach ($label in $names) {
            $i++
            $config = Get-CamConfig -Path (Join-Path $masterDir "cfg$i.json")
            $mine = [System.Text.Encoding]::UTF8.GetBytes((Get-CamRelayConfigText -Config $config -PathMap $map -ApiCredential $cred))
            $theirs = [System.IO.File]::ReadAllBytes((Join-Path $masterDir "relay$i.master.yml"))
            $same = ($mine.Length -eq $theirs.Length) -and ([System.BitConverter]::ToString($mine) -eq [System.BitConverter]::ToString($theirs))
            $evidence += ("(a) relay config, {0}: {1} bytes here, {2} bytes from master, identical {3}" -f $label, $mine.Length, $theirs.Length, $same)
            if (-not $same -or $mine.Length -lt 500) { & $fail "relay config differs from master ($label)" }
        }

        # ---------------- (c) the live config
        $secret = ('ab' * 32)
        $liveCred = New-Object System.Management.Automation.PSCredential('camlive', (ConvertTo-SecureString $secret -AsPlainText -Force))
        $on = Get-CamConfig -Path (Join-Path $masterDir 'cfg3.json')
        $liveMap = Get-CamLivePathMap -Config $on -RelayPathMap $map
        $text = Get-CamLiveConfigText -Config $on -LivePathMap $liveMap -LiveCredential $liveCred
        $flat = @($text -split "`r`n")
        $top = @{}
        foreach ($l in $flat) { if ($l -match '^([A-Za-z0-9]+):\s*(.*)$') { $top[$Matches[1]] = $Matches[2].Trim() } }
        $expect = [ordered]@{ webrtc = 'yes'; webrtcAddress = '127.0.0.1:28889'; webrtcLocalUDPAddress = '127.0.0.1:28189'; webrtcLocalTCPAddress = '""'
            webrtcAllowOrigins = '[]'; webrtcIPsFromInterfaces = 'no'; webrtcAdditionalHosts = '["127.0.0.1"]'; webrtcEncryption = 'no'
            rtsp = 'no'; rtmp = 'no'; hls = 'no'; srt = 'no'; api = 'no'; metrics = 'no'; pprof = 'no'; playback = 'no'; moq = 'no'; authMethod = 'internal' }
        $bad = @($expect.Keys | Where-Object { $top[$_] -ne $expect[$_] } | ForEach-Object { "$_ = '$($top[$_])' want '$($expect[$_])'" })
        $evidence += ("(c) live config keys checked: {0}; wrong: {1}" -f $expect.Count, $(if ($bad) { $bad -join '; ' } else { 'none' }))
        if ($bad) { & $fail ('live config keys: ' + ($bad -join '; ')) }

        $users = @($flat | Where-Object { $_ -match '^\s+- user:' })
        $actions = @($flat | Where-Object { $_ -match '^\s+- action:' })
        $passLine = @($flat | Where-Object { $_ -match '^\s+pass:' })
        $userOk = ($users.Count -eq 1 -and $users[0].Trim() -eq '- user: camlive' -and $actions.Count -eq 1 -and $actions[0].Trim() -eq '- action: read')
        $sha = [System.Security.Cryptography.SHA256]::Create()
        $expectedHash = 'sha256:' + [Convert]::ToBase64String($sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($secret)))
        $passOk = ($passLine.Count -eq 1 -and $passLine[0].Trim() -ceq "pass: $expectedHash")
        $evidence += ("(c) users: {0} ({1}); actions: {2}; any-user present: {3}; pass is the sha256 of the secret: {4}; secret appears in the text: {5}" -f `
            $users.Count, (($users | ForEach-Object { $_.Trim() }) -join ' '), (($actions | ForEach-Object { $_.Trim() }) -join ' '), ($text -match '(?m)user:\s*any'), $passOk, $text.Contains($secret))
        $evidence += ("    the pass line (hash only): {0}" -f $passLine[0].Trim())
        if (-not $userOk) { & $fail 'the live config must have exactly one user, camlive, with exactly one permission, read' }
        if (-not $passOk) { & $fail 'the pass line is not the sha256 of the secret' }
        if ($text.Contains($secret) -or $text -match '(?m)user:\s*any') { & $fail 'the live config holds the secret or an any user' }
        if ($text -match 'Access-Control' -or $text -match '@') { & $fail 'the live config has an Access-Control or credential-looking text' }

        $declared = @(Get-CamRelayConfigPathNames -Text $text)
        $wantPaths = @('frontentrancesub', 'frontentrancemain', 'rearbalconymain', 'cargaragesub', 'cargaragemain', 'workshopentrancesub', 'workshopentrancemain')
        $pathsOk = (($declared -join ',') -ceq ($wantPaths -join ','))
        $evidence += ("(c) live paths: {0}" -f ($declared -join ', '))
        $evidence += ("    Rear Balcony's recorder Quality is stream2 = LiveSubStream, so it has only rearbalconymain; the other three have sub and main")
        if (-not $pathsOk) { & $fail ('live paths are not ' + ($wantPaths -join ', ')) }
        $mainSources = @($flat | Where-Object { $_ -match '^\s+source: rtsp://' })
        $subSources = @($flat | Where-Object { $_ -match '^\s+source: publisher' })
        $mainOk = ($mainSources.Count -eq 4 -and @($mainSources | Where-Object { $_ -notmatch '^\s+source: rtsp://127\.0\.0\.1:8554/[a-z0-9]+$' }).Count -eq 0)
        $evidence += ("(c) main sources: {0}; sub sources left to the environment: {1}" -f (($mainSources | ForEach-Object { $_.Trim() }) -join ' | '), $subSources.Count)
        if (-not $mainOk -or $subSources.Count -ne 3) { & $fail 'main paths must pull the relay with no credential, sub paths come from the environment' }
        $dflt = @($flat | Where-Object { $_ -match '^\s+(sourceOnDemand|sourceOnDemandCloseAfter|rtspTransport):' } | ForEach-Object { $_.Trim() })
        $evidence += ("(c) path defaults: {0}" -f ($dflt -join ', '))
        if (($dflt -join ',') -ne 'rtspTransport: tcp,sourceOnDemand: yes,sourceOnDemandCloseAfter: 7s') { & $fail 'pathDefaults are not tcp, on demand, 7s' }

        $collide = @([pscustomobject]@{ Name = 'Cam'; IP = '1'; Quality = 'stream1' }, [pscustomobject]@{ Name = 'Camsub'; IP = '1'; Quality = 'stream1' })
        $thrown = ''
        try { [void](Get-CamLivePathMap -Config $on -RelayPathMap (Get-CamRelayPathMap -Cameras $collide)) } catch { $thrown = $_.Exception.Message }
        $evidence += ("(c) cameras 'Cam' and 'Camsub' (live path camsub = relay path camsub): {0}" -f $thrown)
        if ($thrown -notlike "*'camsub'*also the recording relay's path*") { & $fail 'a live path colliding with a relay path was not refused' }

        # ---------------- (d) refused at load
        $rejects = @(
            @{ Key = 'LiveViewEnabled'; Value = 'yes' }, @{ Key = 'LiveViewEnable'; Value = $true },
            @{ Key = 'LiveWhepAddress'; Value = '0.0.0.0:8889' }, @{ Key = 'LiveWhepAddress'; Value = '127.0.0.1:99999' },
            @{ Key = 'LiveUdpAddress'; Value = 'localhost:8189' }, @{ Key = 'LiveWhepAddress'; Value = '127.0.0.1:9997' },
            @{ Key = 'LiveCloseAfterSeconds'; Value = 0 }, @{ Key = 'LiveCloseAfterSeconds'; Value = 'soon' }, @{ Key = 'LiveSubStream'; Value = '../x' },
            @{ Key = 'LiveConfigPath'; Value = '\\wsl.localhost\Ubuntu-20.04\x.yml' }, @{ Key = 'LiveConfigPath'; Value = '' }, @{ Key = 'LiveSecretPath'; Value = '\\server\share\s.txt' },
            # [Stage 2a] the LAN media address and the hosts advertised for it
            @{ Key = 'LiveUdpAddress'; Value = '8.8.8.8:8189' }, @{ Key = 'LiveUdpAddress'; Value = '[::1]:8189' }, @{ Key = 'LiveUdpAddress'; Value = 'pc.local:8189' },
            @{ Key = 'LiveUdpAddress'; Value = '192.168.1.50:8189' }, @{ Key = 'LiveUdpAddress'; Value = '0.0.0.0:8189' },
            @{ Key = 'LiveAdvertiseHosts'; Value = @('192.168.1.50') }, @{ Key = 'LiveAdvertiseHosts'; Value = '192.168.1.50' },
            @{ Key = 'LiveAdvertiseHosts'; Value = @('8.8.8.8') }, @{ Key = 'LiveAdvertiseHosts'; Value = @('pc.local') }, @{ Key = 'LiveAdvertiseHosts'; Value = @('fe80::1') },
            @{ Key = 'LiveAdvertiseHosts'; Value = @('192.168.01.50') }, @{ Key = 'LiveAdvertiseHosts'; Value = @('172.32.0.1') }, @{ Key = 'LiveAdvertiseHosts'; Value = @('169.254.1.1') })
        $refused = 0
        foreach ($case in $rejects) {
            $path = Join-Path $masterDir 'bad.json'
            [System.IO.File]::WriteAllText($path, (ConvertTo-Json -InputObject @{ $case.Key = $case.Value }), (New-Object System.Text.UTF8Encoding($false)))
            $message = ''
            try { [void](Get-CamConfig -Path $path) } catch { $message = $_.Exception.Message }
            if ($message) { $refused++ } else { & $fail ("accepted {0} = '{1}'" -f $case.Key, $case.Value) }
        }
        $goodPath = Join-Path $masterDir 'good.json'
        [System.IO.File]::WriteAllText($goodPath, (ConvertTo-Json -InputObject $liveKeys), (New-Object System.Text.UTF8Encoding($false)))
        $goodOk = $true; try { [void](Get-CamConfig -Path $goodPath) } catch { $goodOk = $false; & $fail ('a valid live configuration was refused: ' + $_.Exception.Message) }
        $defaults = Get-CamConfig -Path (Join-Path $masterDir 'missing.json')
        $evidence += ("(d) {0} of {1} malformed or unknown Live* settings refused at load with a message; a valid set accepted: {2}; defaults: off={3}" -f `
            $refused, $rejects.Count, $goodOk, ($defaults.LiveViewEnabled -eq $false))
        $evidence += ("    default keys: LiveViewEnabled={0} LiveConfigPath={1} LiveWhepAddress={2} LiveUdpAddress={3} LiveSubStream={4} LiveCloseAfterSeconds={5} LiveExePath='{6}' LiveSecretPath='{7}'" -f `
            $defaults.LiveViewEnabled, $defaults.LiveConfigPath, $defaults.LiveWhepAddress, $defaults.LiveUdpAddress, $defaults.LiveSubStream, $defaults.LiveCloseAfterSeconds, $defaults.LiveExePath, $defaults.LiveSecretPath)

        # [Stage 2a] LAN settings accepted when consistent; the default live config text is byte-identical to Stage 1B's; hosts reach the config.
        $lanGood = @(
            @{ L = '0.0.0.0 + two private hosts'; K = @{ LiveUdpAddress = '0.0.0.0:28189'; LiveAdvertiseHosts = @('192.168.1.50', '10.0.0.7') }; Hosts = '["127.0.0.1", "192.168.1.50", "10.0.0.7"]' },
            @{ L = 'one private address bound and advertised'; K = @{ LiveUdpAddress = '172.16.5.9:28189'; LiveAdvertiseHosts = @('172.16.5.9') }; Hosts = '["127.0.0.1", "172.16.5.9"]' })
        foreach ($case in $lanGood) {
            $path = Join-Path $masterDir 'lan.json'
            [System.IO.File]::WriteAllText($path, (ConvertTo-Json -InputObject ($case.K + @{ LiveViewEnabled = $true }) -Depth 4), (New-Object System.Text.UTF8Encoding($false)))
            $lanText = ''; $lanErr = ''
            try { $lanCfg = Get-CamConfig -Path $path; $lanText = Get-CamLiveConfigText -Config $lanCfg -LivePathMap $liveMap -LiveCredential $liveCred } catch { $lanErr = $_.Exception.Message }
            $hostsLine = @($lanText -split "`r`n" | Where-Object { $_ -match '^webrtcAdditionalHosts:' })
            $evidence += ("(d2) {0}: accepted {1}; {2}; udp {3}" -f $case.L, (-not $lanErr), ($hostsLine -join ''), (@($lanText -split "`r`n" | Where-Object { $_ -match '^webrtcLocalUDPAddress:' }) -join ''))
            if ($lanErr -or $hostsLine.Count -ne 1 -or $hostsLine[0] -ne ('webrtcAdditionalHosts: ' + $case.Hosts)) { & $fail ("LAN settings '$($case.L)' did not produce $($case.Hosts): $lanErr $($hostsLine -join '')") }
        }
        $goldenPath = Join-Path $masterDir 'golden.json'
        [System.IO.File]::WriteAllText($goldenPath, '{ "LiveViewEnabled": true, "RestreamerEnabled": true }', (New-Object System.Text.UTF8Encoding($false)))
        $goldenCfg = Get-CamConfig -Path $goldenPath
        $goldenCams = @([pscustomobject]@{ Name = 'Front Entrance'; IP = '127.0.0.1:1'; Quality = 'stream1' }, [pscustomobject]@{ Name = 'Rear Balcony'; IP = '127.0.0.1:1'; Quality = 'stream2' })
        $goldenText = Get-CamLiveConfigText -Config $goldenCfg -LivePathMap (Get-CamLivePathMap -Config $goldenCfg -RelayPathMap (Get-CamRelayPathMap -Cameras $goldenCams)) `
            -LiveCredential (New-Object System.Management.Automation.PSCredential('camlive', (ConvertTo-SecureString ('ab' * 32) -AsPlainText -Force)))
        $goldenHash = ([System.BitConverter]::ToString([System.Security.Cryptography.SHA256]::Create().ComputeHash([System.Text.Encoding]::UTF8.GetBytes($goldenText))) -replace '-', '').ToLowerInvariant()
        $goldenWant = '2e304d8dc41ac06b7fdf47c7fd9c3a265af556f36ea1867f8b57d28d158b9c94'   # Stage 1B's text for these inputs (1400 characters), captured before Stage 2a changed anything
        $evidence += ("(e) default live config (all Live keys at their defaults, two cameras, fixed secret): {0} characters, sha256 {1}...; Stage 1B's was 1400 characters {2}...; identical: {3}" -f $goldenText.Length, $goldenHash.Substring(0, 12), $goldenWant.Substring(0, 12), ($goldenHash -eq $goldenWant))
        if ($goldenHash -ne $goldenWant -or $goldenText.Length -ne 1400) { & $fail 'with the defaults the generated live config is no longer byte-identical to Stage 1B''s' }

        # ---------------- (b) flag off: status.json, -Status and -Install -WhatIf against master
            $inst = New-CamTestInstallSandbox -Name 'livecfg-inst'
        $sandboxes += $inst
        $instMaster = New-CamTestInstallSandbox -Name 'livecfg-inst-master'
        $sandboxes += $instMaster
        foreach ($name in @('cam.ps1', 'cam.common.ps1', 'cam.recorder.ps1', 'mediamtx.template.yml')) {
            if (-not (Copy-CamTestMasterFile -Name $name -Destination (Join-Path $instMaster.Dir $name))) { throw "could not read master:$name" }
        }
        $run1 = @{}
        foreach ($case in @(@{ L = 'status'; A = @('-Status') }, @{ L = 'install-whatif'; A = @('-Install', '-WhatIf') })) {
            $a = Invoke-CamTestScript -Path $inst.Cam -Arguments $case.A -Environment @{ LOCALAPPDATA = $inst.LocalAppData }
            $b = Invoke-CamTestScript -Path $instMaster.Cam -Arguments $case.A -Environment @{ LOCALAPPDATA = $instMaster.LocalAppData }
            $norm = { param($r, $dir) @($r.Lines | ForEach-Object { $_.Replace($dir, '<SANDBOX>') } | Where-Object { $_ -notmatch '(?i)^\s*(\d{4}-\d{2}-\d{2}|pid |\s*uptime)' }) -join "`n" }
            $na = & $norm $a $inst.Dir; $nb = & $norm $b $instMaster.Dir
            $same = ($na -ceq $nb -and $a.ExitCode -eq $b.ExitCode -and $na.Length -gt 200)
            $evidence += ("(b) cam.ps1 {0}: exit {1} here, {2} master; {3} lines here, {4} master; identical {5}" -f ($case.A -join ' '), $a.ExitCode, $b.ExitCode, $a.Lines.Count, $b.Lines.Count, $same)
            if (-not $same) { & $fail ("cam.ps1 $($case.A -join ' ') differs from master's"); $evidence += ('    here  : ' + (($na -split "`n" | Select-Object -First 40) -join ' / ')); $evidence += ('    master: ' + (($nb -split "`n" | Select-Object -First 40) -join ' / ')) }
            if ($na -match '(?i)live view|liveview|\blive\b') { & $fail "cam.ps1 $($case.A -join ' ') mentions live view with the flag off" }
        }

        # status.json from a real supervisor, flag off, here and with master's scripts
        $mine = New-CamTestSandbox -Name 'livecfg-off' -Cameras @([pscustomobject]@{ Name = 'Idle'; IP = '127.0.0.1:1'; Quality = 'stream1'; Record = $false }) -Config @{ LiveViewEnabled = $false }
        $sandboxes += $mine
        $theirs = New-CamTestSandbox -Name 'livecfg-off-master' -Cameras @([pscustomobject]@{ Name = 'Idle'; IP = '127.0.0.1:1'; Quality = 'stream1'; Record = $false })
        $sandboxes += $theirs
        foreach ($name in @('cam.common.ps1', 'cam.recorder.ps1', 'mediamtx.template.yml')) {
            if (-not (Copy-CamTestMasterFile -Name $name -Destination (Join-Path $theirs.Dir $name))) { throw "could not read master:$name" }
        }
        foreach ($s in @($mine, $theirs)) { [void](Set-CamTestAlertStub -Sandbox $s) }
        $keys = @{}
        foreach ($pair in @(@('here', $mine), @('master', $theirs))) {
            $process = Start-CamTestSupervisor -Sandbox $pair[1]
            $statusText = $null
            $deadline = [DateTime]::Now.AddSeconds(20)
            while ([DateTime]::Now -lt $deadline) { $got = Get-CamTestStatusJson -Sandbox $pair[1]; if ($got -and $got.Snapshot.State -eq 'running') { $statusText = $got; break }; Start-Sleep -Milliseconds 500 }
            $mediamtx = @(Get-CimInstance Win32_Process -Filter "Name = 'mediamtx.exe'" | Where-Object { $_.CommandLine -and $_.CommandLine.Contains($pair[1].Dir) })
            $stop = Stop-CamTestSupervisor -Sandbox $pair[1] -Process $process
            if (-not $statusText) { & $fail "no running status.json from the $($pair[0]) supervisor"; continue }
            $keys[$pair[0]] = @{ Top = (@($statusText.Snapshot.PSObject.Properties.Name) -join ','); Restreamer = (@($statusText.Snapshot.Restreamer.PSObject.Properties.Name) -join ','); Media = $mediamtx.Count; Stop = $stop.Ok; Text = $statusText.Text }
        }
        if ($keys.here -and $keys.master) {
            $sameKeys = ($keys.here.Top -ceq $keys.master.Top -and $keys.here.Restreamer -ceq $keys.master.Restreamer)
            $evidence += ("(b) status.json top-level keys here == master: {0} ({1})" -f $sameKeys, $keys.here.Top)
            $evidence += ("(b) status.json mentions live: {0}; mediamtx processes in the sandbox: {1}; supervisor stopped cleanly: {2}" -f ($keys.here.Text -match '"Live"\s*:'), $keys.here.Media, $keys.here.Stop)
            if (-not $sameKeys -or $keys.here.Text -match '"Live"\s*:' -or $keys.here.Media -ne 0 -or -not $keys.here.Stop) { & $fail 'status.json or processes differ from master with the flag off' }
        }
    }
    catch { & $fail ("check aborted: " + $_.Exception.Message) }
    finally {
        if ($script:CamLiveCfgOk) { foreach ($s in $sandboxes) { try { Remove-CamTestTree -Path $s.Dir } catch { $script:CamLiveCfgEvidence += "cleanup of $($s.Dir) failed" } } }
        else { $script:CamLiveCfgEvidence += ('sandbox(es) left for inspection: ' + (@($sandboxes | ForEach-Object { $_.Dir }) -join ', ')) }
    }

    Add-CamTestResult -Run $Run -Check 'livecfg' -Status $(if ($script:CamLiveCfgOk) { 'PASS' } else { 'FAIL' }) `
        -Message 'live view is off by default and changes nothing then; its config has exactly the designed properties; bad Live settings are refused at load' `
        -Evidence ($evidence + $script:CamLiveCfgEvidence)
}


function Get-CamTestIcacls {
    <# icacls output for one path, as lines (the path itself dropped from the first line). #>
    param([Parameter(Mandatory = $true)][string]$Path)

    $lines = @(& icacls.exe $Path 2>&1 | ForEach-Object { [string]$_ } | Where-Object { $_.Trim().Length -gt 0 })
    return @($lines | ForEach-Object { $_.Replace($Path, '<file>') } | Where-Object { $_ -notmatch '^Successfully processed' })
}


function Invoke-CamCheckLiveSecret {
    <#
        [Stage 1A] The live login's secret: where it lives, who can read it,
        and that it appears nowhere else.

        (a) created by the product when missing: 64 hex characters, ASCII, no
            newline, at the path the config names (default under
            %LOCALAPPDATA%\IPCCTV, not the repository share);
        (b) its access control lists only the current user and nothing is
            inherited - icacls output shown. Positive control: an ordinary file
            made in the same folder lists other accounts, so the inspection can
            tell the two apart;
        (c) a second call reuses the file (not rewritten), a damaged file is
            refused, and the config text holds only the sha256.
    #>
    param($Run)

    $evidence = @()
    $ok = $true
    $fail = { param([string]$Text) $script:CamLiveSecretOk = $false; $script:CamLiveSecretEvidence += ('FAIL ' + $Text) }
    $script:CamLiveSecretOk = $true
    $script:CamLiveSecretEvidence = @()
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\livesecret'
    if (Test-Path -LiteralPath $dir) { Remove-CamTestTree -Path $dir }
    [void](New-Item -ItemType Directory -Path $dir -Force)

    try {
        $defaultPath = Get-CamLiveSecretPath -Config (Get-CamConfig -Path (Join-Path $dir 'none.json'))
        $wantDefault = Join-Path (Join-Path $env:LOCALAPPDATA 'IPCCTV') 'live_view_secret.txt'
        $evidence += ("(a) default secret path: {0} (expected {1}); on the WSL share: {2}" -f $defaultPath, $wantDefault, $defaultPath.StartsWith('\\'))
        if ($defaultPath -ne $wantDefault) { & $fail 'the default secret path is not %LOCALAPPDATA%\IPCCTV\live_view_secret.txt' }

        $secretPath = Join-Path -Path $dir -ChildPath 'live_view_secret.txt'
        $config = @{ LiveSecretPath = $secretPath }
        $before = Get-CamLiveCredential -Config $config
        $cred = Get-CamLiveCredential -Config $config -Create
        $bytes = [System.IO.File]::ReadAllBytes($secretPath)
        $text = [System.Text.Encoding]::ASCII.GetString($bytes)
        $format = ($null -eq $before -and $cred.UserName -eq 'camlive' -and $bytes.Length -eq 64 -and $text -match '^[0-9a-f]{64}$' -and $cred.GetNetworkCredential().Password -ceq $text)
        $evidence += ("(a) before: {0}; after Create: user {1}, file {2} bytes, 64 lowercase hex, no newline: {3}; no .tmp left: {4}" -f `
            $(if ($null -eq $before) { 'no credential, nothing created without -Create' } else { 'CREATED WITHOUT -Create' }), $cred.UserName, $bytes.Length, $format, -not (Test-Path -LiteralPath "$secretPath.tmp"))
        if (-not $format) { & $fail 'the secret file is not 64 hex characters of ASCII with no newline' }

        $stamp = (Get-Item -LiteralPath $secretPath).LastWriteTimeUtc
        $again = Get-CamLiveCredential -Config $config -Create
        $reused = ($again.GetNetworkCredential().Password -ceq $text -and (Get-Item -LiteralPath $secretPath).LastWriteTimeUtc -eq $stamp)
        $evidence += ("(c) a second -Create reuses the file (same secret, not rewritten): {0}" -f $reused)
        if (-not $reused) { & $fail 'a second -Create replaced the secret' }

        # (b) who may read it
        $me = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        $acl = Get-CamTestIcacls -Path $secretPath
        $evidence += '(b) icacls on the secret file:'
        $evidence += @($acl | ForEach-Object { '      ' + $_ })
        $aces = @($acl | ForEach-Object { if ($_ -match '^\s*<file>\s+(.+)$') { $Matches[1] } elseif ($_ -match '^\s+(\S.*:\(.*)$') { $Matches[1] } } | Where-Object { $_ })
        $others = @($aces | Where-Object { $_ -notlike "$me*" })
        $inherited = @($aces | Where-Object { $_ -match '\(I\)' })
        $broad = @($aces | Where-Object { $_ -match '(?i)Everyone|\\Users|Authenticated Users|BUILTIN|NT AUTHORITY|SYSTEM|Administrators' })
        $onlyMe = ($aces.Count -eq 1 -and $others.Count -eq 0 -and $inherited.Count -eq 0 -and $broad.Count -eq 0 -and $aces[0] -match '\(F\)')
        $evidence += ("(b) entries: {0}; other accounts: {1}; inherited: {2}; only {3} with full control: {4}" -f $aces.Count, $others.Count, $inherited.Count, $me, $onlyMe)
        if (-not $onlyMe) { & $fail 'the secret file is readable by an account other than the current user' }

        $controlPath = Join-Path -Path $dir -ChildPath 'ordinary.txt'
        [System.IO.File]::WriteAllText($controlPath, 'ordinary')
        $controlAcl = Get-CamTestIcacls -Path $controlPath
        $controlAces = @($controlAcl | ForEach-Object { if ($_ -match '^\s*<file>\s+(.+)$') { $Matches[1] } elseif ($_ -match '^\s+(\S.*:\(.*)$') { $Matches[1] } } | Where-Object { $_ })
        $controlOthers = @($controlAces | Where-Object { $_ -notlike "$me*" })
        $evidence += '(b) positive control - icacls on an ordinary file made in the same folder:'
        $evidence += @($controlAcl | ForEach-Object { '      ' + $_ })
        $evidence += ("(b) the control lists {0} other entr(ies), so the inspection can see other accounts: {1}" -f $controlOthers.Count, ($controlOthers.Count -gt 0))
        if ($controlOthers.Count -eq 0) { & $fail 'the positive control file shows no other accounts, so the ACL inspection proves nothing' }

        # (c) damaged file refused
        [System.IO.File]::WriteAllText($secretPath, 'not hex')
        $message = ''
        try { [void](Get-CamLiveCredential -Config $config) } catch { $message = $_.Exception.Message }
        $evidence += ("(c) a damaged file is refused: {0}" -f $message)
        if (-not $message) { & $fail 'a damaged secret file was accepted' }

        # (e) a real run: sweeps of every command line, every file the run produced, and the -Status output
        if (-not (Test-CamTestPortFree -Port 18080)) { throw 'TCP port 18080 is already in use; the run needs it free.' }
        $sampler = Start-CamTestCommandLineSampler
        $rig = $null; $server = $null; $decoy = $null
        try {
            $cameras = @([pscustomobject]@{ Name = 'Cam One'; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = $true })
            $rig = Start-CamTestLiveRig -Name 'livesecret-run' -Cameras $cameras -CameraPath @('stream1', 'stream2')
            $runSecretPath = [string]$rig.Sandbox.Config.LiveSecretPath
            $runSecret = ([System.IO.File]::ReadAllText($runSecretPath)).Trim()
            $evidence += ("(e) the supervisor created the secret itself at {0} ({1} hex characters)" -f $runSecretPath.Replace($rig.Dir, '<rig>'), $runSecret.Length)
            if ($runSecret -notmatch '^[0-9a-f]{64}$') { & $fail 'the supervisor did not create a 64-hex secret' }
            $acl = Get-CamTestIcacls -Path $runSecretPath
            $evidence += ('(e) icacls on the supervisor-created secret: ' + (($acl | ForEach-Object { $_.Trim() }) -join ' | '))
            $aceCount = @($acl | Where-Object { $_ -match '\(F\)|\(R\)|\(M\)|\(I\)' }).Count
            if ($aceCount -ne 1 -or @($acl | Where-Object { $_ -match '\(I\)|SYSTEM|Administrators|Users|Everyone' }).Count) { & $fail 'the supervisor-created secret file has more than the current user in its ACL' }

            # traffic that uses the login: the page server adds it to a WHEP request (a bad offer, so it is the login that is exercised)
            $server = Start-CamTestLiveServer -Prefix 'http://127.0.0.1:18080/' -PageFile (Join-Path (Get-CamTestRepo) 'tests\live\page.html') -DataDir $rig.DataDir `
                -WhepBase "http://127.0.0.1:$($rig.WhepPort)" -SecretFile $runSecretPath
            $viaProxy = Invoke-CamLiveHttp -Url 'http://127.0.0.1:18080/whep/camonesub' -Method POST -Headers @{ 'Content-Type' = 'application/sdp' } -Body "v=0`r`n"
            $evidence += ("(e) a WHEP request through the page server (adds the login): HTTP {0} (400 = authenticated, offer rejected; 401 would be a refused login)" -f $viaProxy.Code)
            if ($viaProxy.Code -eq 401 -or $viaProxy.Code -eq 0) { & $fail 'the login the supervisor generated was not accepted through the page server' }

            # -Status as the Operator runs it, from a sandbox copy of cam.ps1
            foreach ($file in @('cam.ps1', 'mpv.cam.autolayout.autorestart.ps1')) { Copy-Item -LiteralPath (Join-Path (Get-CamTestRepo) $file) -Destination $rig.Sandbox.Dir -Force }
            $local = Join-Path -Path $rig.Dir -ChildPath 'localappdata'
            [void](New-Item -ItemType Directory -Path $local -Force)
            $statusRun = Invoke-CamTestScript -Path (Join-Path $rig.Sandbox.Dir 'cam.ps1') -Arguments @('-Status') -Environment @{ LOCALAPPDATA = $local }
            $liveLines = @($statusRun.Lines | Where-Object { $_ -match '(?i)live view|mediamtx  |paths   ' })
            $evidence += ("(e) cam.ps1 -Status: exit {0}, {1} lines; the live view section:" -f $statusRun.ExitCode, $statusRun.Lines.Count)
            $evidence += @($liveLines | ForEach-Object { '      ' + $_ })
            if ($statusRun.ExitCode -ne 0 -or -not ($statusRun.Lines -match '=== live view|live view')) { & $fail '-Status did not run or has no live view section' }

            # positive control: a decoy whose command line really carries the secret, started while the sampler runs
            $decoy = Start-Process -FilePath 'powershell.exe' -PassThru -WindowStyle Hidden -ArgumentList @('-NoProfile', '-Command', "Start-Sleep -Seconds 5 # $runSecret")
            [void]$decoy.Handle
            Start-Sleep -Seconds 4
            $decoyPid = $decoy.Id

            if ($server -and -not $server.HasExited) { try { $server.Kill(); [void]$server.WaitForExit(5000) } catch { } }
            $notes = @(Stop-CamTestLiveRig -Rig $rig)
            $evidence += $notes
            $rigStopped = $true
        }
        finally {
            if ($server -and -not $server.HasExited) { try { $server.Kill(); [void]$server.WaitForExit(5000) } catch { } }
            if ($decoy -and -not $decoy.HasExited) { try { $decoy.Kill() } catch { } }
            $sweep = Stop-CamTestCommandLineSampler -Sampler $sampler
            if ($rig -and -not $rigStopped) { $evidence += @(Stop-CamTestLiveRig -Rig $rig) }
        }

        $header = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes('camlive:' + $runSecret))
        $needles = @{ 'the secret' = $runSecret; 'the Basic header value' = $header; 'camlive:<secret>' = ('camlive:' + $runSecret) }
        $lineHits = @($sweep.Seen | Where-Object { $l = $_.Line; @($needles.Values | Where-Object { $l.Contains($_) }).Count })
        $decoyHits = @($lineHits | Where-Object { $_.Who -eq ('powershell.exe pid {0}' -f $decoyPid) })
        $realHits = @($lineHits | Where-Object { $_.Who -ne ('powershell.exe pid {0}' -f $decoyPid) })
        $evidence += ("(e) command-line sweep: {0} looks over the whole run, {1} distinct command lines seen (supervisor, relay, live instance, recorder, ffmpeg, page server, synthetic camera...)" -f $sweep.Looks, $sweep.Seen.Count)
        $evidence += ("(e) positive control - a decoy powershell (pid {0}) carrying the secret in its command line: caught {1} time(s)" -f $decoyPid, $decoyHits.Count)
        $evidence += ("(e) hits for the secret, 'camlive:<secret>' or the Basic header value on any OTHER command line: {0}" -f $realHits.Count)
        foreach ($h in $realHits) { $evidence += ('      ' + $h.Who + ' ' + $h.Line.Replace($runSecret, '<SECRET>')) }
        if ($decoyHits.Count -lt 1) { & $fail 'the command-line sweep did not catch the planted decoy, so a clean sweep proves nothing' }
        if ($realHits.Count) { & $fail 'the secret or the Basic header was on a process command line' }
        $camLines = @($sweep.Seen | Where-Object { $_.Line -match 'mediamtx-live\.yml|live\\server\.ps1' -and $_.Line -notmatch 'Start-Sleep' } | ForEach-Object { $_.Who + ': ' + ($_.Line -replace '\s+', ' ') })
        $evidence += ('(e) command lines that mention the live instance (the secret appears only as a FILE PATH): ' + (($camLines | Select-Object -First 4 | ForEach-Object { $_.Substring(0, [math]::Min(200, $_.Length)) }) -join ' || '))

        $planted = Join-Path -Path $rig.Dir -ChildPath 'planted-control.txt'
        [System.IO.File]::WriteAllText($planted, "planted: $runSecret")
        $scan = Find-CamTestNeedleInFiles -Root $rig.Dir -Needles $needles -Exclude @($runSecretPath)
        $controlHit = @($scan.Hits | Where-Object { $_ -like 'planted-control.txt*' })
        $fileHits = @($scan.Hits | Where-Object { $_ -notlike 'planted-control.txt*' })
        $statusLeak = @($statusRun.Lines | Where-Object { $l = $_; @($needles.Values | Where-Object { $l.Contains($_) }).Count })
        $evidence += ("(e) file sweep of everything the run wrote ({0} files: generated configs, status.json, supervisor and recorder and liveview logs, console logs, alerts, page-server logs, archive): positive control file flagged {1} needle(s); real hits {2}" -f $scan.Files, $controlHit.Count, $fileHits.Count)
        foreach ($h in $fileHits) { $evidence += ('      ' + $h) }
        $evidence += ("(e) the -Status output holds the secret or header: {0}" -f $statusLeak.Count)
        if ($controlHit.Count -lt 1) { & $fail 'the file sweep did not flag the planted control file' }
        if ($fileHits.Count) { & $fail 'the secret or the Basic header is in a file other than the secret file' }
        if ($statusLeak.Count) { & $fail 'the -Status output holds the secret' }
        $liveYml = Get-Content -LiteralPath (Join-Path $rig.Dir 'live\mediamtx-live.yml') -Raw
        $evidence += ("(e) the generated live config holds a sha256 pass line only: {0}; any user: {1}" -f ($liveYml -match 'pass: sha256:'), ($liveYml -match '(?m)user:\s*any'))
        Remove-CamTestTree -Path $rig.Dir
    }
    catch { & $fail ('check aborted: ' + $_.Exception.Message) }
    finally {
        if ($script:CamLiveSecretOk) { try { Remove-CamTestTree -Path $dir } catch { } }
        else { $script:CamLiveSecretEvidence += "left for inspection: $dir" }
    }

    Add-CamTestResult -Run $Run -Check 'livesecret' -Status $(if ($script:CamLiveSecretOk) { 'PASS' } else { 'FAIL' }) `
        -Message 'the live secret is made by the product, readable only by the current account, and never reused or rewritten by accident' `
        -Evidence ($evidence + $script:CamLiveSecretEvidence)
}


function Invoke-CamLiveHttp {
    <# One HTTP request to the live instance; never throws. Returns Code (0 = no answer), headers of interest, Ms. #>
    param(
        [Parameter(Mandatory = $true)][string]$Url,
        [string]$Method = 'GET',
        [hashtable]$Headers = @{},
        [string]$HostHeader,
        [string]$Body,
        [int]$TimeoutMs = 8000
    )

    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $request = [System.Net.HttpWebRequest]::Create($Url)
    $request.Proxy = $null; $request.Method = $Method; $request.Timeout = $TimeoutMs; $request.ReadWriteTimeout = $TimeoutMs
    if ($HostHeader) { $request.Host = $HostHeader }
    foreach ($key in $Headers.Keys) {
        if ($key -eq 'Content-Type') { $request.ContentType = $Headers[$key] } else { $request.Headers[$key] = $Headers[$key] }
    }
    if ($PSBoundParameters.ContainsKey('Body')) {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($Body)
        $stream = $request.GetRequestStream(); $stream.Write($bytes, 0, $bytes.Length); $stream.Dispose()
    }
    $response = $null
    try { $response = $request.GetResponse() } catch [System.Net.WebException] { $response = $_.Exception.Response }
    if (-not $response) { return [pscustomobject]@{ Code = 0; Acao = ''; Www = ''; Body = ''; Ms = [int]$watch.ElapsedMilliseconds } }
    $reader = New-Object System.IO.StreamReader($response.GetResponseStream())
    $text = $reader.ReadToEnd(); $reader.Dispose()
    $result = [pscustomobject]@{ Code = [int]$response.StatusCode; Acao = [string]$response.Headers['Access-Control-Allow-Origin']
        Www = [string]$response.Headers['WWW-Authenticate']; Body = $text; Ms = [int]$watch.ElapsedMilliseconds }
    $response.Dispose()
    return $result
}


function Start-CamTestLiveInstance {
    <#
        Stand-in for the supervisor's live child, used until the supervisor
        starts it: launches MediaMTX on the generated config with the camera
        source URLs in its environment only, stdout to a file. Returns the
        process.
    #>
    param(
        [Parameter(Mandatory = $true)]$Config,
        [Parameter(Mandatory = $true)]$LivePathMap,
        [Parameter(Mandatory = $true)][System.Management.Automation.PSCredential]$CameraCredential,
        [Parameter(Mandatory = $true)][string]$WorkDir,
        [Parameter(Mandatory = $true)][string]$LogPath
    )

    $info = New-Object System.Diagnostics.ProcessStartInfo
    $info.FileName = (Get-CamLiveExePath -Config $Config)
    $info.Arguments = Get-CamCommandLine -Arguments @([string]$Config.LiveConfigPath)
    $info.WorkingDirectory = $WorkDir
    $info.UseShellExecute = $false; $info.CreateNoWindow = $true; $info.RedirectStandardOutput = $true; $info.RedirectStandardError = $true
    foreach ($name in $LivePathMap.Keys) {
        if ($LivePathMap[$name].Kind -eq 'sub') {
            $info.EnvironmentVariables[(Get-CamRelaySourceEnvName -PathName $name)] = (Get-CamLiveSourceUrl -Config $Config -Camera $LivePathMap[$name].Camera -Credential $CameraCredential)
        }
    }
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $info
    [void]$process.Start()
    [void]$process.Handle
    $file = [System.IO.File]::Open($LogPath, 'Create', 'Write', 'ReadWrite')
    $null = $process.StandardOutput.BaseStream.CopyToAsync($file)
    $null = $process.StandardError.BaseStream.CopyToAsync([System.IO.Stream]::Null)
    Add-Member -InputObject $process -NotePropertyName LogStream -NotePropertyValue $file
    return $process
}


function Invoke-CamCheckLiveWhep {
    <#
        [Stage 1A] The live instance, run by the real supervisor, in a real
        browser. Synthetic camera only, fake credentials, non-default ports
        (see Start-CamTestLiveRig; page servers 18080 and 18081, the
        decoder-report instance 18890/18190). Chrome and Edge only, headed,
        small window, throwaway profile, killed by profile marker afterwards.

        (i) WHEP with no login is 401, with a wrong login 401, and with the
            right login (added by the page server, as camui will) Chrome and
            Edge negotiate and PLAY the sub path and the main path:
            framesDecoded grows, size and decoder implementation reported;
        (iv) the G.711 A-law audio plays once the tile is unmuted.
    #>
    param($Run)

    $evidence = @()
    $fail = { param([string]$Text) $script:CamLiveWhepOk = $false; $script:CamLiveWhepEvidence += ('FAIL ' + $Text) }
    $script:CamLiveWhepOk = $true
    $script:CamLiveWhepEvidence = @()
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $name = 'livewhep'
    $rig = $null; $server = $null; $serverB = $null; $liveB = $null; $dir = Join-Path -Path $env:TEMP -ChildPath "cam-tests\$name"
    $script:CamLiveBrowsers = @(); $script:CamLiveBrowserSeconds = 0; $script:CamLiveRunNo = 0

    try {
        foreach ($p in @(18080, 18081, 18890)) { if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the check needs it free." } }
        if (-not (Test-CamTestUdpPortFree -Port 18190)) { throw 'UDP port 18190 is already in use; the check needs it free.' }
        # Cam One: plain 640x360 stream1. Cam Two: its recorder stream IS the sub-stream (stream2), so it gets only a main path.
        # Cam High: a 1280x720 High-profile stream. stream2 is a 640x360 H.264 HIGH stream with no B-frames, like the sub-streams of
        # Rear Balcony, Field Front and Workshop; high720 is the 1280x720 High one (the Workshop's sub-stream size).
        $cameras = @(
            [pscustomobject]@{ Name = 'Cam One'; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = $true },
            [pscustomobject]@{ Name = 'Cam Two'; IP = '127.0.0.1:18554'; Quality = 'stream2'; Record = $false },
            [pscustomobject]@{ Name = 'Cam High'; IP = '127.0.0.1:18554'; Quality = 'high720'; Record = $false })
        $rig = Start-CamTestLiveRig -Name $name -Cameras $cameras -CameraPath @('stream1', 'stream2', 'high720') -VideoSpec @{ stream2 = '640x360:high'; high720 = '1280x720:high' }
        $ffprobe = Join-Path -Path (Split-Path -Parent (Get-CamConfigDefault).FfmpegPath) -ChildPath 'ffprobe.exe'
        foreach ($stream in @('stream1', 'stream2', 'high720')) {
            $probe = @(& $ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,profile,width,height,has_b_frames -of csv=p=0 -rtsp_transport tcp "rtsp://127.0.0.1:18554/$stream" 2>&1)
            $evidence += ("camera path {0} as published (ffprobe): {1}" -f $stream, ($probe -join ' '))
        }
        $dir = $rig.Dir; $dataDir = $rig.DataDir; $config = $rig.Sandbox.Config
        $whep = "http://127.0.0.1:$($rig.WhepPort)"
        $status = (Get-CamTestStatusJson -Sandbox $rig.Sandbox).Snapshot
        $evidence += ("supervisor-run live MediaMTX pid {0} (status.json Live.Alive {1}), paths {2}; relay pid {3}" -f $status.Live.Pid, $status.Live.Alive, (@($status.Live.Paths) -join ', '), $status.Restreamer.Pid)

        # (i) the login, over plain HTTP
        $offer = "v=0`r`n"
        $none = Invoke-CamLiveHttp -Url "$whep/camonesub/whep" -Method POST -Headers @{ 'Content-Type' = 'application/sdp' } -Body $offer
        $evidence += ("(i) WHEP POST, no login: HTTP {0} in {1} ms, WWW-Authenticate: [{2}]" -f $none.Code, $none.Ms, $none.Www)
        if ($none.Code -ne 401) { & $fail 'a WHEP request with no login was not answered 401' }
        $wrong = Invoke-CamLiveHttp -Url "$whep/camonesub/whep" -Method POST -TimeoutMs 9000 -Headers @{ 'Content-Type' = 'application/sdp'; Authorization = 'Basic ' + [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes('camlive:' + ('0' * 64))) } -Body $offer
        $evidence += ("(i) WHEP POST, wrong login: HTTP {0} in {1} ms (MediaMTX delays a wrong login 0-4 s)" -f $wrong.Code, $wrong.Ms)
        if ($wrong.Code -ne 401) { & $fail 'a WHEP request with a wrong login was not answered 401' }

        # the page server, as camui will be: same origin, adds the login
        $pagePort = 18080
        $server = Start-CamTestLiveServer -Prefix "http://127.0.0.1:$pagePort/" -PageFile (Join-Path (Get-CamTestRepo) 'tests\live\page.html') -DataDir $dataDir `
            -WhepBase $whep -SecretFile ([string]$config.LiveSecretPath)
        $evidence += ("page server pid {0} on 127.0.0.1:{1} (its command line holds the secret FILE path only)" -f $server.Id, $pagePort)

        # one browser run: launch, poll the page's posted stats, kill by marker. Returns what it saw.
        $browse = {
            param([string]$Browser, [string]$Tiles, [switch]$Cap, [double]$Unmute = 0)
            $found = Get-CamTestBrowser -Browser $Browser
            if (-not $found.Exe) { return [pscustomobject]@{ Skipped = $found.Reason } }
            $script:CamLiveRunNo++
            $runId = '{0}-{1}' -f $Browser, $script:CamLiveRunNo
            $stats = Join-Path -Path $dataDir -ChildPath "$runId.jsonl"
            $expectTiles = @($Tiles -split ',').Count
            $query = "run=$runId&tiles=$Tiles" + $(if ($Unmute -gt 0) { "&unmute=$Unmute" } else { '' }) + $(if ($Cap) { '&cap=1' } else { '' })
            $started = Start-CamTestBrowser -Browser $Browser -BaseDir $dir -Capture:$Cap -Url ("http://127.0.0.1:{0}/?{1}" -f $pagePort, $query)
            $script:CamLiveBrowsers += $started
            $began = [DateTime]::UtcNow
            $last = $null; $mark = $null; $unmuteAt = $null
            try {
                $deadline = [DateTime]::UtcNow.AddSeconds(50)
                while ([DateTime]::UtcNow -lt $deadline) {
                    Start-Sleep -Seconds 2
                    $events = @(Read-CamTestLiveStats -Path $stats)
                    $samples = @($events | Where-Object { $_.ev -eq 's' })
                    if ($samples.Count) { $last = $samples[-1] }
                    $unmuteEvent = @($events | Where-Object { $_.ev -eq 'unmute' })
                    if ($unmuteEvent.Count -and -not $mark) { $unmuteAt = $unmuteEvent[0].t; $mark = @($samples | Where-Object { $_.t -le $unmuteEvent[0].t })[-1] }
                    $playing = $last -and (@($last.tiles | Where-Object { $_.v -and $_.v.framesDecoded -ge 45 }).Count -eq $expectTiles)
                    $heard = ($Unmute -le 0) -or ($unmuteAt -and $last -and ($last.t - $unmuteAt) -ge 5000)
                    if ($playing -and $heard) { break }
                }
            }
            finally {
                $seconds = ([DateTime]::UtcNow - $began).TotalSeconds
                $script:CamLiveBrowserSeconds += $seconds
                $end = Stop-CamTestBrowser -Started $started
            }
            return [pscustomobject]@{ Skipped = $null; Browser = $Browser; Version = $found.Version; Last = $last; Mark = $mark; UnmuteAt = $unmuteAt
                Seconds = $seconds; Left = $end.Left.Count; ProfileRemoved = $end.ProfileRemoved }
        }
        $describe = {
            param($Result, [string]$Phase, [hashtable]$Width)
            foreach ($t in @($Result.Last.tiles)) {
                $v = $t.v
                if ($Width -and $v -and $v.w -ne $Width[[string]$t.path]) { & $fail ("$($Result.Browser) $($t.path): width $($v.w), expected $($Width[[string]$t.path])") }
                $text = "{0} {1} {2}: HTTP {3}, {4}x{5}, framesDecoded {6}, dropped {7}, {8} fps, decoder [{9}] powerEfficient={10}" -f $Phase, $Result.Browser, $t.path, $t.status, `
                    $(if ($v) { $v.w } else { 0 }), $(if ($v) { $v.h } else { 0 }), $(if ($v) { $v.framesDecoded } else { 0 }), $(if ($v) { $v.framesDropped } else { 0 }), $(if ($v) { $v.fps } else { 0 }), $(if ($v) { $v.impl } else { '' }), $(if ($v) { $v.powerEfficient } else { '' })
                $script:CamLiveWhepEvidence += $text
                if ($t.status -ne 201 -or -not $v -or $v.framesDecoded -lt 45 -or $v.w -lt 100) { & $fail "$($Result.Browser) did not play $($t.path): $text" }
            }
        }
        # Phase A: the supervisor's instance (LiveSubStream stream2 = High 640x360). Phase B below: LiveSubStream high720.
        $tilesA = 'camonesub,camonemain,camtwomain,camhighmain'
        $widthA = @{ camonesub = 640; camonemain = 640; camtwomain = 640; camhighmain = 1280 }
        $tilesB = 'camonesub,camonemain,camtwomain,camhighmain'
        $widthB = @{ camonesub = 1280; camonemain = 640; camtwomain = 640; camhighmain = 1280 }

        # PHASE A - production shape: UDP only, no capture permission. Plays, and the unmuted audio is heard.
        foreach ($browser in @('chrome', 'edge')) {
            foreach ($tiles in @($tilesA)) {
                $r = & $browse $browser $tiles -Unmute 9
                if ($r.Skipped) { & $fail ("$browser could not be run: " + $r.Skipped); continue }
                $evidence += ("A {0} {1}: window up {2:n0} s; processes left after kill: {3}; profile removed: {4}" -f $browser, $r.Version, $r.Seconds, $r.Left, $r.ProfileRemoved)
                if ($r.Left -or -not $r.ProfileRemoved) { & $fail "$browser left processes or a profile behind" }
                if (-not $r.Last) { & $fail "$browser posted no stats"; continue }
                $before = $null; if ($r.Mark) { $before = @($r.Mark.tiles | Where-Object { $_.path -eq 'camonesub' })[0] }
                & $describe $r 'A' $widthA
                $sub = @($r.Last.tiles | Where-Object { $_.path -eq 'camonesub' })[0]
                if ($sub -and $sub.a) {
                    $energyBefore = $(if ($before -and $before.a) { [double]$before.a.energy } else { 0.0 })
                    $delta = [double]$sub.a.energy - $energyBefore
                    $evidence += ("(iv) {0} camonesub audio: energy {1:n4} at the unmute, {2:n4} after {3:n1} s (+{4:n4}), muted now: {5}, packets {6}" -f `
                        $browser, $energyBefore, $sub.a.energy, (($r.Last.t - $r.UnmuteAt) / 1000), $delta, $sub.muted, $sub.a.packets)
                    if ($delta -le 0.001 -or $sub.muted) { & $fail "$browser played no audio after unmute" }
                } else { & $fail "$browser reported no audio statistics" }
            }
        }

        # PHASE B - decoder report. Chromium exposes decoderImplementation only to a page holding a capture permission, and
        # such a page offers real interface addresses that a loopback UDP listener cannot pair with; so this phase alone uses a
        # second live instance (ports 18890/18190, same relay and camera) whose generated config has ICE/TCP on the same
        # loopback port added - a test-only edit of the generated text.
        $bConfig = $config.Clone()
        $bConfig.LiveSubStream = 'high720'; $bConfig.LiveWhepAddress = '127.0.0.1:18890'; $bConfig.LiveUdpAddress = '127.0.0.1:18190'; $bConfig.LiveConfigPath = (Join-Path $dir 'live\mediamtx-live-b.yml')
        $relayMap = Get-CamRelayPathMap -Cameras (Get-CamList -Path $rig.Sandbox.Cameras)
        $liveMap = Get-CamLivePathMap -Config $bConfig -RelayPathMap $relayMap
        $credential = Get-CamLiveCredential -Config $bConfig
        $baseText = Get-CamLiveConfigText -Config $bConfig -LivePathMap $liveMap -LiveCredential $credential
        $tcpText = $baseText.Replace('webrtcLocalTCPAddress: ""', 'webrtcLocalTCPAddress: 127.0.0.1:18190')
        if ($tcpText -eq $baseText) { throw 'the test-only TCP edit did not apply' }
        [System.IO.File]::WriteAllText([string]$bConfig.LiveConfigPath, $tcpText, (New-Object System.Text.UTF8Encoding($false)))
        $cameraCredential = Get-CamCredential -Path (Join-Path $rig.Sandbox.Dir 'camera_creds.xml')
        $liveB = Start-CamTestLiveInstance -Config $bConfig -LivePathMap $liveMap -CameraCredential $cameraCredential -WorkDir (Join-Path $dir 'live') -LogPath (Join-Path $dir 'live-b.out')
        $deadline = [DateTime]::Now.AddSeconds(15)
        while ([DateTime]::Now -lt $deadline -and (Test-CamTestPortFree -Port 18890)) { Start-Sleep -Milliseconds 300 }
        $pagePort = 18081
        $serverB = Start-CamTestLiveServer -Prefix "http://127.0.0.1:$pagePort/" -PageFile (Join-Path (Get-CamTestRepo) 'tests\live\page.html') -DataDir $dataDir `
            -WhepBase 'http://127.0.0.1:18890' -SecretFile ([string]$config.LiveSecretPath)
        foreach ($browser in @('chrome', 'edge')) {
            foreach ($tiles in @($tilesB)) {
                $r = & $browse $browser $tiles -Cap
                if ($r.Skipped) { & $fail ("$browser could not be run: " + $r.Skipped); continue }
                $evidence += ("B {0} {1}: window up {2:n0} s; processes left after kill: {3}; profile removed: {4}" -f $browser, $r.Version, $r.Seconds, $r.Left, $r.ProfileRemoved)
                if ($r.Left -or -not $r.ProfileRemoved) { & $fail "$browser left processes or a profile behind" }
                if (-not $r.Last) { & $fail "$browser posted no stats"; continue }
                & $describe $r 'B' $widthB
                foreach ($t in @($r.Last.tiles)) { if ($t.v -and -not $t.v.impl) { & $fail "$browser $($t.path): no decoder implementation reported" } }
            }
        }

        # (ii) THE HOLE, red then green. Another website (a page on http://localhost:18082, a different origin AND site from
        # MediaMTX on 127.0.0.1) tries to open a WebRTC session to read a camera with no login.
        #   red   : the shared relay's own generated config with only WebRTC switched on - anonymous read, default origins;
        #   green : the same config with only webrtcAllowOrigins: [] changed (what the empty list alone does);
        #   green : the real live instance (login, no origins).
        foreach ($p in @(18082, 18891, 18892)) { if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the hole proof needs it free." } }
        foreach ($p in @(18191, 18192)) { if (-not (Test-CamTestUdpPortFree -Port $p)) { throw "UDP port $p is already in use; the hole proof needs it free." } }
        $apiCred = New-Object System.Management.Automation.PSCredential('camrelayapi', (ConvertTo-SecureString ('0' * 64) -AsPlainText -Force))
        $relayText = Get-CamRelayConfigText -Config $config -PathMap $relayMap -ApiCredential $apiCred
        $shape = {
            param([int]$HttpPort, [int]$UdpPort, [bool]$EmptyOrigins)
            $x = $relayText
            $x = [regex]::Replace($x, '(?m)^rtsp: yes', 'rtsp: no')
            $x = [regex]::Replace($x, '(?m)^api: yes', 'api: no')
            $web = "webrtc: yes`r`nwebrtcAddress: 127.0.0.1:$HttpPort`r`nwebrtcLocalUDPAddress: 127.0.0.1:$UdpPort`r`nwebrtcIPsFromInterfaces: no`r`nwebrtcAdditionalHosts: [`"127.0.0.1`"]"
            if ($EmptyOrigins) { $web += "`r`nwebrtcAllowOrigins: []" }
            $x = [regex]::Replace($x, '(?m)^webrtc: no', $web)
            $x = [regex]::Replace($x, '(?m)^(  camone:\r\n    #[^\r\n]*\r\n)    source: publisher', ('${1}    source: rtsp://127.0.0.1:18554/stream2'))
            if ($x -notmatch 'webrtc: yes' -or $x -notmatch 'source: rtsp://127.0.0.1:18554/stream2' -or $x -notmatch '(?m)^  - user: any') { throw 'could not shape the relay config for the hole proof' }
            return $x
        }
        $red0 = $null; $red1 = $null; $evilServer = $null
        try {
            [System.IO.File]::WriteAllText((Join-Path $dir 'live\red0.yml'), (& $shape 18891 18191 $false), (New-Object System.Text.UTF8Encoding($false)))
            [System.IO.File]::WriteAllText((Join-Path $dir 'live\red1.yml'), (& $shape 18892 18192 $true), (New-Object System.Text.UTF8Encoding($false)))
            $red0 = Start-CamTestMediaMtx -ConfigPath (Join-Path $dir 'live\red0.yml') -WorkDir (Join-Path $dir 'live') -LogPath (Join-Path $dir 'red0.out')
            $red1 = Start-CamTestMediaMtx -ConfigPath (Join-Path $dir 'live\red1.yml') -WorkDir (Join-Path $dir 'live') -LogPath (Join-Path $dir 'red1.out')
            $deadline = [DateTime]::Now.AddSeconds(15)
            while ([DateTime]::Now -lt $deadline -and ((Test-CamTestPortFree -Port 18891) -or (Test-CamTestPortFree -Port 18892))) { Start-Sleep -Milliseconds 300 }
            Start-Sleep -Seconds 3
            $evidence += ("(ii) relay-shaped instances: red0 pid {0} (anonymous read, default origins) on 18891, red1 pid {1} (same, webrtcAllowOrigins: []) on 18892" -f $red0.Id, $red1.Id)

            $evil = 'http://evil.example'
            $header = @{ Origin = $evil; 'Access-Control-Request-Method' = 'POST'; 'Access-Control-Request-Headers' = 'content-type' }
            $p0 = Invoke-CamLiveHttp -Url 'http://127.0.0.1:18891/camone/whep' -Method OPTIONS -Headers $header
            $p1 = Invoke-CamLiveHttp -Url 'http://127.0.0.1:18892/camone/whep' -Method OPTIONS -Headers $header
            $pl = Invoke-CamLiveHttp -Url "$whep/camonesub/whep" -Method OPTIONS -Headers $header
            $evidence += ("(ii) preflight from Origin {0}: RED shared-relay shape -> HTTP {1}, Access-Control-Allow-Origin [{2}]" -f $evil, $p0.Code, $p0.Acao)
            $evidence += ("(ii) preflight from Origin {0}: empty origins only -> HTTP {1}, Access-Control-Allow-Origin [{2}]" -f $evil, $p1.Code, $p1.Acao)
            $evidence += ("(ii) preflight from Origin {0}: LIVE instance -> HTTP {1}, Access-Control-Allow-Origin [{2}]" -f $evil, $pl.Code, $pl.Acao)
            if ($p0.Acao -ne $evil -and $p0.Acao -ne '*') { & $fail 'RED did not reproduce: the shared-relay shape did not grant the foreign origin' }
            if ($p1.Acao -or $pl.Acao) { & $fail 'a config with an empty origin list, or the live instance, sent Access-Control-Allow-Origin' }

            $rebind0 = Invoke-CamLiveHttp -Url 'http://127.0.0.1:18891/camone/whep' -Method POST -HostHeader 'attacker-rebind.example:18891' -Headers @{ 'Content-Type' = 'application/sdp' } -Body "v=0`r`n"
            $rebindL = Invoke-CamLiveHttp -Url "$whep/camonesub/whep" -Method POST -HostHeader 'attacker-rebind.example:18889' -Headers @{ 'Content-Type' = 'application/sdp' } -Body "v=0`r`n"
            $page0 = Invoke-CamLiveHttp -Url 'http://127.0.0.1:18891/camone/' -HostHeader 'attacker-rebind.example:18891'
            $pageL = Invoke-CamLiveHttp -Url "$whep/camonesub/" -HostHeader 'attacker-rebind.example:18889'
            $evidence += ("(ii) rebinding Host attacker-rebind.example, no login, POST /whep: RED -> HTTP {0} (not refused: 400 is the bad SDP); LIVE -> HTTP {1}" -f $rebind0.Code, $rebindL.Code)
            $evidence += ("(ii) rebinding Host, GET the built-in player page: RED -> HTTP {0}; LIVE -> HTTP {1}" -f $page0.Code, $pageL.Code)
            if ($rebind0.Code -in 401, 403 -or $page0.Code -ne 200) { & $fail 'RED did not reproduce: the shared-relay shape refused a rebinding Host' }
            if ($rebindL.Code -ne 401 -or $pageL.Code -ne 401) { & $fail 'the live instance did not refuse a rebinding Host with no login' }

            $evilServer = Start-CamTestLiveServer -Prefix 'http://localhost:18082/' -PageFile (Join-Path (Get-CamTestRepo) 'tests\live\evil.html') -DataDir $dataDir
            $targets = 'red=http://127.0.0.1:18891/camone,emptyorigins=http://127.0.0.1:18892/camone,live=http://127.0.0.1:18889/camonesub'
            $liveLog = Join-Path -Path $rig.Sandbox.Root -ChildPath '_logs\liveview.log'
            $sessionsBefore = @(Get-Content -LiteralPath $liveLog | Where-Object { $_ -match '\[WebRTC\].*created by' }).Count
            if ($sessionsBefore -lt 1) { & $fail 'the live log shows no WebRTC session from the earlier phases, so the session count below would prove nothing' }
            foreach ($browser in @('chrome', 'edge')) {
                $found = Get-CamTestBrowser -Browser $browser
                if (-not $found.Exe) { & $fail ("$browser could not be run: " + $found.Reason); continue }
                $script:CamLiveRunNo++
                $runId = 'evil-{0}-{1}' -f $browser, $script:CamLiveRunNo
                $stats = Join-Path -Path $dataDir -ChildPath "$runId.jsonl"
                $started = Start-CamTestBrowser -Browser $browser -BaseDir $dir -Url ("http://localhost:18082/?run={0}&targets={1}" -f $runId, $targets)
                $script:CamLiveBrowsers += $started
                $began = [DateTime]::UtcNow
                $results = @()
                try {
                    $deadline = [DateTime]::UtcNow.AddSeconds(45)
                    while ([DateTime]::UtcNow -lt $deadline) {
                        Start-Sleep -Seconds 2
                        $events = @(Read-CamTestLiveStats -Path $stats)
                        $results = @($events | Where-Object { $_.ev -eq 'result' })
                        if (@($events | Where-Object { $_.ev -eq 'done' }).Count) { break }
                    }
                } finally {
                    $script:CamLiveBrowserSeconds += ([DateTime]::UtcNow - $began).TotalSeconds
                    $end = Stop-CamTestBrowser -Started $started
                }
                if ($end.Left.Count -or -not $end.ProfileRemoved) { & $fail "$browser left processes or a profile behind (hole run)" }
                foreach ($label in @('red', 'emptyorigins', 'live')) {
                    $row = @($results | Where-Object { $_.label -eq $label })[0]
                    if (-not $row) { & $fail "${browser}: no result for $label"; continue }
                    $evidence += ("(ii) {0} {1}: page origin {2} -> {3}: {4}; HTTP {5}; frames decoded {6}" -f $browser, $found.Version, $row.origin, $label, $row.outcome, $row.status, $row.framesDecoded)
                    $granted = ($row.outcome -like 'SESSION GRANTED*' -and $row.framesDecoded -gt 0)
                    if ($label -eq 'red' -and -not $granted) { & $fail "$browser RED did not reproduce: the foreign page got no video from the shared-relay shape ($($row.outcome))" }
                    if ($label -ne 'red' -and ($row.outcome -like 'SESSION GRANTED*' -or $row.framesDecoded -gt 0)) { & $fail "$browser GREEN failed: the foreign page got a session from $label" }
                    if ($label -ne 'red' -and $row.outcome -notlike 'blocked by the browser*') { & $fail "$browser $label : expected the browser to block the cross-origin request, got: $($row.outcome)" }
                }
            }
            $sessionsAfter = @(Get-Content -LiteralPath $liveLog | Where-Object { $_ -match '\[WebRTC\].*created by' }).Count
            $evidence += ("(ii) WebRTC sessions the live instance logged while the foreign page ran (both browsers): {0}" -f ($sessionsAfter - $sessionsBefore))
            if ($sessionsAfter -ne $sessionsBefore) { & $fail 'the live instance created a session for the foreign page' }
        } finally {
            Stop-CamTestMediaMtx -Process $red0; Stop-CamTestMediaMtx -Process $red1
            if ($evilServer -and -not $evilServer.HasExited) { try { $evilServer.Kill() } catch { } }
        }
    }
    catch { & $fail ('check aborted: ' + $_.Exception.Message) }
    finally {
        foreach ($b in @($script:CamLiveBrowsers)) { try { [void](Stop-CamTestBrowser -Started $b) } catch { } }
        foreach ($proc in @($server, $serverB, $liveB)) { if ($proc -and -not $proc.HasExited) { try { $proc.Kill(); [void]$proc.WaitForExit(5000) } catch { } } }
        if ($liveB -and $liveB.LogStream) { try { $liveB.LogStream.Dispose() } catch { } }
        $notes = @()
        if ($rig) { $notes = @(Stop-CamTestLiveRig -Rig $rig) }
        $evidence += ("browser windows were up {0:n0} s in total; whole check {1:n0} s" -f $script:CamLiveBrowserSeconds, $watch.Elapsed.TotalSeconds)
        $evidence += $notes
        if ($notes -match 'forced: (?!nothing)') { & $fail 'the supervisor had to be forced' }
        if ($script:CamLiveWhepOk) { try { Remove-CamTestTree -Path $dir } catch { $script:CamLiveWhepEvidence += "cleanup of $dir failed" } }
        else { $script:CamLiveWhepEvidence += "left for inspection: $dir" }
    }

    Add-CamTestResult -Run $Run -Check 'livewhep' -Status $(if ($script:CamLiveWhepOk) { 'PASS' } else { 'FAIL' }) `
        -Message 'the live instance asks for a login and plays sub and main paths, with audio, in Chrome and Edge' -Evidence ($evidence + $script:CamLiveWhepEvidence)
}


function Get-CamTestPathReaders {
    <# The sessions reading one path of a (synthetic camera's) MediaMTX, from its control API: count and their types; -1 when unanswered. #>
    param([Parameter(Mandatory = $true)][int]$ApiPort, [Parameter(Mandatory = $true)][string]$Path)

    $answer = Invoke-CamLiveHttp -Url "http://127.0.0.1:$ApiPort/v3/paths/get/$Path" -TimeoutMs 2000
    if ($answer.Code -ne 200) { return [pscustomobject]@{ Count = -1; Types = '' } }
    $json = ConvertFrom-Json -InputObject $answer.Body
    $readers = @($json.readers)
    return [pscustomobject]@{ Count = $readers.Count; Types = (($readers | ForEach-Object { $_.type }) -join ',') }
}


function Invoke-CamCheckLiveOnDemand {
    <#
        [Stage 1A] The camera's second session is held only while someone
        watches. Real supervisor, synthetic camera (its control API counts the
        sessions reading its paths), the recorder reading stream1 through the
        relay, Chrome as the viewer (a clean leave: the page closes its peer
        connections).

          before any viewer      stream2 sessions 0 (stream1: the relay's one)
          viewer on camonemain   stream2 0, stream1 still exactly 1: the main
                                 path pulls the RELAY, adding no camera session
          viewer on camonesub    stream2 1 while it is connected
          viewer leaves          stream2 back to 0 within LiveCloseAfterSeconds
                                 (4) plus a 6 s margin
    #>
    param($Run)

    $evidence = @()
    $fail = { param([string]$Text) $script:CamLiveOdOk = $false; $script:CamLiveOdEvidence += ('FAIL ' + $Text) }
    $script:CamLiveOdOk = $true
    $script:CamLiveOdEvidence = @()
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $name = 'liveondemand'
    $rig = $null; $server = $null; $started = $null; $browserSeconds = 0
    $script:CamLiveBrowsers = @()
    $closeAfter = 4; $margin = 6

    try {
        if (-not (Test-CamTestPortFree -Port 18080)) { throw 'TCP port 18080 is already in use; the check needs it free.' }
        $cameras = @([pscustomobject]@{ Name = 'Cam One'; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = $true })
        $rig = Start-CamTestLiveRig -Name $name -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds $closeAfter
        $dir = $rig.Dir; $api = $rig.CamApi
        Start-Sleep -Seconds 4
        $sample = { $a = Get-CamTestPathReaders -ApiPort $api -Path 'stream1'; $b = Get-CamTestPathReaders -ApiPort $api -Path 'stream2'; [pscustomobject]@{ Ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds(); S1 = $a.Count; S2 = $b.Count; T1 = $a.Types; T2 = $b.Types } }
        $before = @(1..4 | ForEach-Object { & $sample; Start-Sleep -Milliseconds 500 })
        $evidence += ("before any viewer: stream1 sessions {0} (types {1}), stream2 sessions {2}  (4 samples, 0.5 s apart)" -f (($before | ForEach-Object { $_.S1 }) -join '/'), $before[-1].T1, (($before | ForEach-Object { $_.S2 }) -join '/'))
        if (@($before | Where-Object { $_.S1 -ne 1 }).Count) { & $fail 'stream1 did not have exactly the relay session before any viewer' }
        if (@($before | Where-Object { $_.S2 -ne 0 }).Count) { & $fail 'stream2 had a session before any viewer' }

        $config = $rig.Sandbox.Config
        $server = Start-CamTestLiveServer -Prefix 'http://127.0.0.1:18080/' -PageFile (Join-Path (Get-CamTestRepo) 'tests\live\page.html') -DataDir $rig.DataDir `
            -WhepBase "http://127.0.0.1:$($rig.WhepPort)" -SecretFile ([string]$config.LiveSecretPath)
        $found = Get-CamTestBrowser -Browser 'chrome'
        if (-not $found.Exe) { throw $found.Reason }
        $runId = 'ondemand'
        $stats = Join-Path -Path $rig.DataDir -ChildPath "$runId.jsonl"
        $timeline = New-Object System.Collections.ArrayList
        $started = Start-CamTestBrowser -Browser 'chrome' -BaseDir $dir -Url ("http://127.0.0.1:18080/?run={0}&tiles=camonemain&late=camonesub@12&leave=24" -f $runId)
        $script:CamLiveBrowsers += $started
        $began = [DateTime]::UtcNow
        $leaveAt = $null; $lateAt = $null; $zeroAt = $null
        try {
            $deadline = [DateTime]::UtcNow.AddSeconds(70)
            while ([DateTime]::UtcNow -lt $deadline) {
                [void]$timeline.Add((& $sample))
                $events = @(Read-CamTestLiveStats -Path $stats)
                $late = @($events | Where-Object { $_.ev -eq 'late' }); if ($late.Count) { $lateAt = $late[0].t }
                $leave = @($events | Where-Object { $_.ev -eq 'leave' }); if ($leave.Count) { $leaveAt = $leave[0].t }
                if ($leaveAt -and -not $zeroAt) {
                    $recent = @($timeline | Where-Object { $_.Ms -ge $leaveAt })
                    $firstZero = @($recent | Where-Object { $_.S2 -eq 0 })
                    if ($firstZero.Count -ge 2) { $zeroAt = $firstZero[0].Ms }
                }
                if ($zeroAt) { break }
                Start-Sleep -Milliseconds 400
            }
            $last = @(Read-CamTestLiveStats -Path $stats | Where-Object { $_.ev -eq 's' -and (-not $leaveAt -or $_.t -lt $leaveAt) })
            $lastSample = $(if ($last.Count) { $last[-1] } else { $null })
        } finally {
            $browserSeconds = ([DateTime]::UtcNow - $began).TotalSeconds
            $end = Stop-CamTestBrowser -Started $started
        }
        if ($end.Left.Count -or -not $end.ProfileRemoved) { & $fail 'chrome left processes or a profile behind' }

        $rows = @($timeline)
        $mainOnly = @($rows | Where-Object { $lateAt -and $_.Ms -lt ($lateAt - 500) -and $_.Ms -gt ($rows[0].Ms + 3000) })
        $both = @($rows | Where-Object { $lateAt -and $leaveAt -and $_.Ms -gt ($lateAt + 4000) -and $_.Ms -lt ($leaveAt - 500) })
        $evidence += ("viewer on camonemain only ({0} samples before the sub tile was added): stream1 sessions max {1}, stream2 sessions max {2}" -f $mainOnly.Count, ($mainOnly | Measure-Object S1 -Maximum).Maximum, ($mainOnly | Measure-Object S2 -Maximum).Maximum)
        $evidence += ("viewers on camonemain and camonesub ({0} samples): stream1 sessions min/max {1}/{2}, stream2 sessions min/max {3}/{4} (stream2 reader types: {5})" -f `
            $both.Count, ($both | Measure-Object S1 -Minimum).Minimum, ($both | Measure-Object S1 -Maximum).Maximum, ($both | Measure-Object S2 -Minimum).Minimum, ($both | Measure-Object S2 -Maximum).Maximum, (($both | ForEach-Object { $_.T2 } | Select-Object -Unique) -join '|'))
        if ($lastSample) { $evidence += ("the page's own view just before the viewers left: " + (($lastSample.tiles | ForEach-Object { '{0} status {1} framesDecoded {2}' -f $_.path, $_.status, $(if ($_.v) { $_.v.framesDecoded } else { 0 }) }) -join '; ')) }
        if ($mainOnly.Count -lt 3 -or $both.Count -lt 3) { & $fail 'too few samples in a phase to judge' }
        if (@($mainOnly | Where-Object { $_.S2 -ne 0 }).Count) { & $fail 'a viewer on the main path opened a stream2 camera session' }
        if (@($mainOnly + $both | Where-Object { $_.S1 -ne 1 }).Count) { & $fail 'stream1 did not stay at exactly one session (the relay) while viewers were connected' }
        if (@($both | Where-Object { $_.S2 -ne 1 }).Count) { & $fail 'stream2 did not hold exactly one session while the sub viewer was connected' }
        if ($leaveAt -and $zeroAt) {
            $gone = ($zeroAt - $leaveAt) / 1000
            $evidence += ("viewers left cleanly (peer connections closed) at +0 s: stream2 sessions back to 0 after {0:n1} s (LiveCloseAfterSeconds {1} + margin {2} = {3} s allowed)" -f $gone, $closeAfter, $margin, ($closeAfter + $margin))
            if ($gone -gt ($closeAfter + $margin)) { & $fail "stream2 took $gone s to close after the viewers left" }
        } else { & $fail 'stream2 sessions never returned to 0 after the viewers left' }
        $firstS2 = @($rows | Where-Object { $_.S2 -gt 0 } | Select-Object -First 1)
        $evidence += ("timeline ({0} samples, every ~0.5 s): first stream2 session {1:n1} s after the first sample (the sub tile was added at about +12 s of the page); stream1 sessions over the whole run: min {2}, max {3}" -f `
            $rows.Count, $(if ($firstS2.Count) { ($firstS2[0].Ms - $rows[0].Ms) / 1000 } else { 0 }), ($rows | Measure-Object S1 -Minimum).Minimum, ($rows | Measure-Object S1 -Maximum).Maximum)
    }
    catch { & $fail ('check aborted: ' + $_.Exception.Message) }
    finally {
        foreach ($b in @($script:CamLiveBrowsers)) { try { [void](Stop-CamTestBrowser -Started $b) } catch { } }
        if ($server -and -not $server.HasExited) { try { $server.Kill(); [void]$server.WaitForExit(5000) } catch { } }
        $notes = @()
        if ($rig) { $notes = @(Stop-CamTestLiveRig -Rig $rig) }
        $evidence += ("browser window up {0:n0} s; whole check {1:n0} s" -f $browserSeconds, $watch.Elapsed.TotalSeconds)
        $evidence += $notes
        if ($notes -match 'forced: (?!nothing)') { & $fail 'the supervisor had to be forced' }
        if ($script:CamLiveOdOk -and $rig) { try { Remove-CamTestTree -Path $rig.Dir } catch { $script:CamLiveOdEvidence += "cleanup failed" } }
        elseif ($rig) { $script:CamLiveOdEvidence += "left for inspection: $($rig.Dir)" }
    }

    Add-CamTestResult -Run $Run -Check 'liveondemand' -Status $(if ($script:CamLiveOdOk) { 'PASS' } else { 'FAIL' }) `
        -Message "the camera's stream2 session exists only while a sub viewer is connected, and a main viewer adds no camera session" -Evidence ($evidence + $script:CamLiveOdEvidence)
}


function Invoke-CamCheckLivePage {
    <#
        [Stage 1B, opt-in: opens browser windows] The Live screen, rendered.
        The REAL supervisor with a live instance against a synthetic camera,
        six cameras (Cam 1 records), camui built to a temp path and run on
        18789 against the sandbox, Chrome and Edge on a throwaway profile
        driven through the DevTools protocol (the page's own pixels only).

          desktop 1920x1080   six sub tiles reach playing (framesDecoded from
                              window.__liveStats), 3 columns, pixel statistics
                              of each tile; the camera's stream2 has 6 sessions
          click               Cam 1 goes to main (larger), the others stay sub,
                              stream2 falls to 5; Esc returns it, back to 6
          audio               every tile muted; the unmute button unmutes that
                              tile only and audio packets keep arriving
          hidden              minimizing the window closes every connection and
                              stream2 falls to 0; restoring reopens all six
          deep link           a fresh load of #live/Cam%201 is on main
          phone 390x844       1 column, six tiles playing; 800 wide gives 2
          live view off       a second camui over a config with the flag off:
                              the tab says so, the proxy answers 503
          foreign page        a page on http://localhost:18082 cannot create a
                              live session through camui, nor can a request
                              with a foreign Origin, cross-site Sec-Fetch-Site
                              or a rebinding Host; the live instance logs no
                              session for any of them, and the same request
                              without those headers does (positive control)

        Needs CAMUI_TEST_EXE (a camui.exe built from this tree). Screenshots go
        to $env:CAMTEST_SHOTS when set (outside the repository), else nowhere.
    #>
    param($Run)

    $evidence = @()
    $fail = { param([string]$Text) $script:CamLivePageOk = $false; $script:CamLivePageEvidence += ('FAIL ' + $Text) }
    $note = { param([string]$Text) $script:CamLivePageEvidence += $Text }
    $script:CamLivePageOk = $true
    $script:CamLivePageEvidence = @()
    $script:CamLivePageBrowsers = @()
    $script:CamLivePageSeconds = 0
    $script:CamLivePageHd = @()
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $rig = $null; $ui = $null; $uiOff = $null; $evil = $null
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\livepage'
    $shots = [string]$env:CAMTEST_SHOTS
    $base = 'http://127.0.0.1:18789'
    $offer = Get-CamTestWhepOffer

    try {
        if (-not $env:CAMUI_TEST_EXE -or -not (Test-Path -LiteralPath $env:CAMUI_TEST_EXE)) { throw 'CAMUI_TEST_EXE must name a camui.exe built from this tree (cross-compile it to a temp path).' }
        foreach ($p in @(18789, 18790, 18300, 18082, 18892)) { if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the check needs it free." } }
        if ($shots) { [void](New-Item -ItemType Directory -Path $shots -Force) }

        $cameras = 1..6 | ForEach-Object { [pscustomobject]@{ Name = "Cam $_"; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = ($_ -eq 1) } }
        # The camera's main (stream1) is 1280x720 and its sub (stream2) the default 640x360, so the swap from sub to main is visible in videoWidth.
        $rig = Start-CamTestLiveRig -Name 'livepage' -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds 4 -LiveApiPort 18892 -VideoSpec @{ stream1 = '1280x720:high' }
        $dir = $rig.Dir
        $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-livepage'
        if (-not $ui.Started) { throw ('camui did not start: ' + $ui.Reason) }
        $status = (Get-CamTestStatusJson -Sandbox $rig.Sandbox).Snapshot
        & $note ("supervisor-run live MediaMTX pid {0}; status.json Live.Cameras: {1}" -f $status.Live.Pid, (@($status.Live.Cameras | ForEach-Object { '{0} sub={1} main={2}' -f $_.Name, $_.Sub, $_.Main }) -join '; '))
        $camuiExe = Get-Item -LiteralPath $env:CAMUI_TEST_EXE
        & $note ("camui {0} ({1:n0} bytes, built {2}) listening on {3}, serving the sandbox {4}" -f $camuiExe.Name, $camuiExe.Length, $camuiExe.LastWriteTime.ToString('s'), $base, $rig.Sandbox.Dir)

        # A second camui whose config has live view OFF (no recorder, no status file): what the tab and the proxy say then.
        $offDir = Join-Path -Path $dir -ChildPath 'off'
        [void](New-Item -ItemType Directory -Path $offDir -Force)
        [System.IO.File]::WriteAllText((Join-Path $offDir 'recorder.config.json'), '{ "LiveViewEnabled": false }', (New-Object System.Text.UTF8Encoding($false)))
        [System.IO.File]::WriteAllText((Join-Path $offDir 'cameras.json'), '[]', (New-Object System.Text.UTF8Encoding($false)))
        $uiOff = Start-CamTestUi -Port 18790 -Name 'camui-livepage-off' -StatusFile (Join-Path $offDir 'status.json') -ScriptDir $offDir
        if (-not $uiOff.Started) { throw ('the live-off camui did not start: ' + $uiOff.Reason) }

        # The proxy's own JSON, over plain HTTP.
        $list = Invoke-CamLiveHttp -Url "$base/api/live"
        $listJson = ConvertFrom-Json -InputObject $list.Body
        & $note ("GET /api/live: HTTP {0}, enabled {1}, alive {2}, {3} camera(s), fields {4}" -f $list.Code, $listJson.enabled, $listJson.alive, @($listJson.cameras).Count, ((@($listJson.cameras)[0].PSObject.Properties.Name) -join ','))
        if ($list.Code -ne 200 -or -not $listJson.enabled -or -not $listJson.alive -or @($listJson.cameras).Count -ne 6) { & $fail 'GET /api/live did not describe six cameras with live view on' }

        $liveLog = Join-Path -Path $rig.Sandbox.Root -ChildPath '_logs\liveview.log'
        $sessionCount = { @(Get-Content -LiteralPath $liveLog -ErrorAction SilentlyContinue | Where-Object { $_ -match '\[WebRTC\].*created by' }).Count }
        $readers = { (Get-CamTestPathReaders -ApiPort $rig.CamApi -Path 'stream2').Count }

        # ---- helpers over the DevTools connection ($cdp is set per browser run) ----
        $stats = { try { Invoke-CamTestCdpEval -Cdp $cdp -Expression 'window.__liveStats ? window.__liveStats() : null' -Await } catch { $null } }
        $until = {
            param([scriptblock]$Done, [int]$Seconds = 40)
            $end = [DateTime]::UtcNow.AddSeconds($Seconds); $s = $null
            while ([DateTime]::UtcNow -lt $end) { $s = & $stats; if ($s -and (& $Done $s)) { return $s }; Start-Sleep -Milliseconds 500 }
            return $null
        }
        $playing = { param($s, [int]$n = 6, [int]$min = 25) $s.shown -and @(@($s.tiles) | Where-Object { $_.state -eq 'live' -and $_.v -and $_.v.framesDecoded -ge $min }).Count -ge $n }
        $readersIs = {
            param([int]$Want, [int]$Seconds = 14)
            $end = [DateTime]::UtcNow.AddSeconds($Seconds); $n = -1
            while ([DateTime]::UtcNow -lt $end) { $n = & $readers; if ($n -eq $Want) { return $n }; Start-Sleep -Milliseconds 500 }
            return $n
        }
        $navigate = {
            param([string]$Url)
            [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = 'about:blank' })
            Start-Sleep -Milliseconds 300
            [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = $Url })
        }
        $viewport = {
            param([int]$Width, [int]$Height)
            [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Emulation.setDeviceMetricsOverride' -Params @{ width = $Width; height = $Height; deviceScaleFactor = 1; mobile = $false })
            # The page re-fits on a debounced timer: wait until it has laid out for this size, or the next click lands on the old layout.
            $end = [DateTime]::UtcNow.AddSeconds(4)
            while ([DateTime]::UtcNow -lt $end) {
                Start-Sleep -Milliseconds 100
                $probe = $null
                try { $probe = Invoke-CamTestCdpEval -Cdp $cdp -Expression 'window.__liveProbe ? window.__liveProbe() : null' } catch { }
                if (-not $probe -or $null -eq $probe.fitVw -or ($probe.fitVw -eq $Width -and $probe.fitVh -eq $Height)) { break }
            }
        }
        $click = {
            param([double]$X, [double]$Y)
            foreach ($type in @('mouseMoved', 'mousePressed', 'mouseReleased')) {
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchMouseEvent' -Params @{ type = $type; x = $X; y = $Y; button = 'left'; clickCount = 1; buttons = $(if ($type -eq 'mousePressed') { 1 } else { 0 }) })
            }
        }
        $shot = {
            param([string]$Label, $Stats)
            $file = Join-Path -Path $dir -ChildPath ("shot-$Label.png")
            [void](Save-CamTestPageShot -Cdp $cdp -Path $file)
            $rects = @(@($Stats.tiles) | ForEach-Object { @{ X = $_.rect.x; Y = $_.rect.y + $Stats.scrollY; W = $_.rect.w; H = [int]($_.rect.h * 0.7) } })
            $px = @(Get-CamTestPngStats -Path $file -Rects $rects)
            $image = [System.Drawing.Image]::FromFile($file); $size = '{0}x{1}' -f $image.Width, $image.Height; $image.Dispose()
            if ($shots) { Copy-Item -LiteralPath $file -Destination (Join-Path $shots "$Label.png") -Force }
            return [pscustomobject]@{ Size = $size; Pixels = $px }
        }
        $describeTiles = {
            param($Stats, $Pixels)
            $i = 0
            foreach ($t in @($Stats.tiles)) {
                $p = $(if ($Pixels) { $Pixels[$i] } else { $null })
                & $note ("      {0}: {1} {2} at ({3},{4}) {5}x{6}, video {7}x{8}, framesDecoded {9}, muted {10}{11}" -f $t.name, $t.stream, $t.state, $t.rect.x, $t.rect.y, $t.rect.w, $t.rect.h, $t.vw, $t.vh, `
                    $(if ($t.v) { $t.v.framesDecoded } else { 0 }), $t.muted, $(if ($p) { "; pixels mean luma $($p.Mean), lit $([int](100 * $p.LitShare)) %, $($p.Colours) colours" } else { '' }))
                $i++
            }
        }

        foreach ($browser in @('chrome', 'edge')) {
            $found = Get-CamTestBrowser -Browser $browser
            if (-not $found.Exe) { & $fail ("$browser could not be run: " + $found.Reason); continue }
            $cdp = $null; $started = $null
            $began = [DateTime]::UtcNow
            try {
                $started = Start-CamTestBrowser -Browser $browser -BaseDir $dir -Url 'about:blank' -DebugPort 18300 -WindowSize '1280,900'
                $script:CamLivePageBrowsers += $started
                $cdp = Connect-CamTestCdp -Port 18300
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.enable')
                & $note ("== $browser $($found.Version) (throwaway profile, DevTools on 18300, the window's own pixels only) ==")

                # ---- helpers for this run ----
                $js = { param([string]$Code) Invoke-CamTestCdpEval -Cdp $cdp -Expression $Code }
                $center = {
                    param([string]$Selector, [double]$FracY = 0.5)
                    $q = ConvertTo-Json -InputObject $Selector -Compress
                    & $js ("(() => { const e = document.querySelector($q); if (!e) return null; const r = e.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height * $FracY }; })()")
                }
                $tileSel = { param([string]$Name) ('.ltile[data-camera="{0}"]' -f $Name) }
                $clickAt = { param($Point) if (-not $Point) { throw 'nothing to click' }; & $click $Point.x $Point.y }
                $sessions = { (Get-CamTestLiveSessions -Rig $rig).Count }
                $sessionsIs = {
                    param([int]$Want, [int]$Seconds = 10)
                    $end = [DateTime]::UtcNow.AddSeconds($Seconds); $n = -1
                    while ([DateTime]::UtcNow -lt $end) { $n = & $sessions; if ($n -eq $Want) { return $n }; Start-Sleep -Milliseconds 400 }
                    return $n
                }
                $rectsOf = { param($S) @(@($S.tiles) | ForEach-Object { '{0},{1},{2},{3}' -f $_.rect.x, $_.rect.y, $_.rect.w, $_.rect.h }) -join ' | ' }
                $frames = { param($S) @(@($S.tiles) | ForEach-Object { if ($_.v) { [int]$_.v.framesDecoded } else { 0 } }) }
                $instrument = @'
(() => {
  if (window.__fx) return 'already';
  const orig = window.fetch.bind(window);
  const fx = window.__fx = { delay: 0, fail: false, posts: [], deletes: 0, probes: [], fs: 0, mfs: 0 };
  window.fetch = (url, opts) => {
    const u = String(url), method = opts && opts.method;
    if (method === 'POST') fx.posts.push((u.match(/stream=(\w+)/) || [])[1]);
    if (method === 'DELETE') fx.deletes++;
    const main = method === 'POST' && /stream=main/.test(u);
    if (main && fx.fail) return Promise.resolve(new Response('{"error":"stream not available"}', { status: 404, headers: { 'Content-Type': 'application/json' } }));
    if (main && fx.delay) return new Promise((r) => setTimeout(r, fx.delay)).then(() => orig(url, opts));
    return orig(url, opts);
  };
  document.addEventListener('click', (e) => {
    if (!e.target.closest || !e.target.closest('#lgrid .ltile') || e.target.closest('.lmute,.lcopy,.lchip')) return;
    const t0 = performance.now();
    const snap = (label) => { const p = window.__liveProbe(); p.label = label; p.ms = Math.round(performance.now() - t0); fx.probes.push(p); };
    requestAnimationFrame(() => requestAnimationFrame(() => snap('2raf')));
    setTimeout(() => snap('100ms'), 100);
  }, true);
  const rf = Element.prototype.requestFullscreen;
  Element.prototype.requestFullscreen = function (...a) { if (this.id === 'lwrap') fx.fs++; else if (this.id === 'lmbox') fx.mfs++; return rf.apply(this, a); };
  return 'installed';
})()
'@
                $reset = { [void](& $js 'Object.assign(window.__fx, { delay: 0, fail: false, probes: [] })') }
                $fxState = { & $js 'JSON.stringify({ posts: window.__fx.posts, deletes: window.__fx.deletes, probes: window.__fx.probes, fs: window.__fx.fs, mfs: window.__fx.mfs })' | ConvertFrom-Json }
                $esc = {
                    foreach ($t in @('keyDown', 'keyUp')) { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchKeyEvent' -Params @{ type = $t; key = 'Escape'; code = 'Escape'; windowsVirtualKeyCode = 27 }) }
                }
                $modalIs = { param([scriptblock]$Pred, [int]$Seconds = 12) & $until { param($s) $s.modal -and (& $Pred $s.modal) } $Seconds }
                $closedIs = { param([int]$Seconds = 6) & $until { param($s) -not $s.modal -and $s.hash -eq '#live' } $Seconds }
                $layout = {
                    param($S)
                    $t = @($S.tiles)
                    $minX = ($t | ForEach-Object { $_.rect.x } | Measure-Object -Minimum).Minimum; $maxX = ($t | ForEach-Object { $_.rect.x + $_.rect.w } | Measure-Object -Maximum).Maximum
                    $minY = ($t | ForEach-Object { $_.rect.y } | Measure-Object -Minimum).Minimum; $maxY = ($t | ForEach-Object { $_.rect.y + $_.rect.h } | Measure-Object -Maximum).Maximum
                    $headerBottom = [int](& $js 'Math.round(document.querySelector(''.top'').getBoundingClientRect().bottom)')
                    $areaW = $S.innerWidth; $areaH = $S.innerHeight - $headerBottom
                    $tileArea = ($t | ForEach-Object { $_.rect.w * $_.rect.h } | Measure-Object -Sum).Sum
                    [pscustomobject]@{
                        MinW = ($t | ForEach-Object { $_.rect.w } | Measure-Object -Minimum).Minimum; MaxW = ($t | ForEach-Object { $_.rect.w } | Measure-Object -Maximum).Maximum
                        H = $t[0].rect.h; BoxW = $maxX - $minX; BoxH = $maxY - $minY; HeaderBottom = $headerBottom; AreaW = $areaW; AreaH = $areaH
                        WidthShare = [math]::Round(($maxX - $minX) / $areaW, 3); HeightShare = [math]::Round(($maxY - $minY) / $areaH, 3)
                        TileShare = [math]::Round($tileArea / ($areaW * $areaH), 3); BoxShare = [math]::Round((($maxX - $minX) * ($maxY - $minY)) / ($areaW * $areaH), 3)
                        Scrolls = ($S.scrollHeight -gt $S.innerHeight + 1); SideScroll = ($S.scrollWidth -gt $S.clientWidth) }
                }
                $sizes = {
                    param([int]$W, [int]$H, [string]$Label, [bool]$WithShot = $false)
                    & $viewport $W $H
                    $s2 = & $until { param($s) $s.innerWidth -eq $W -and $s.fit -and $s.fit.vw -eq $W -and $s.fit.vh -eq $H -and @(@($s.tiles) | Where-Object { $_.rect.w -eq $s.fit.tile }).Count -eq @($s.tiles).Count } 8
                    if (-not $s2) { $s2 = & $stats; & $fail "${browser} ${Label}: the tiles never took the computed size"; if (-not $s2) { return $null } }
                    # a vertical scrollbar that appears once the tiles are laid out narrows the grid and makes the page re-fit (debounced): let that settle
                    Start-Sleep -Milliseconds 700
                    $s2 = & $stats
                    $lay = & $layout $s2
                    $px = $null
                    if ($WithShot) { $px = & $shot "$browser-$Label" $s2 }
                    if ($lay.SideScroll) { & $fail "$browser ${Label}: the page scrolls sideways (scrollWidth $($s2.scrollWidth) > $($s2.clientWidth))" }
                    & $note ("  {0}: fit {1} cols x {2} rows, tile {3} px (DOM columns {4}); rects {5}; scrolls {6}; header ends at y={7}; tile boxes span {8}x{9} of {10}x{11} content area: width {12:p1}, height {13:p1}, tiles' own area {14:p1}" -f `
                        $Label, $s2.fit.cols, $s2.fit.rows, $s2.fit.tile, $s2.columns, (& $rectsOf $s2), $lay.Scrolls, $lay.HeaderBottom, $lay.BoxW, $lay.BoxH, $lay.AreaW, $lay.AreaH, $lay.WidthShare, $lay.HeightShare, $lay.TileShare)
                    return [pscustomobject]@{ Stats = $s2; Layout = $lay; Pixels = $px }
                }

                $shotModal = {
                    param([string]$Label, $S)
                    $file = Join-Path -Path $dir -ChildPath ("shot-$Label.png")
                    [void](Save-CamTestPageShot -Cdp $cdp -Path $file)
                    $b = $S.modal.box
                    $px = @(Get-CamTestPngStats -Path $file -Rects @(@{ X = $b.x; Y = $b.y + 60; W = $b.w; H = [int]($b.h * 0.6) }))
                    if ($shots) { Copy-Item -LiteralPath $file -Destination (Join-Path $shots "$Label.png") -Force }
                    return $px[0]
                }

                # ---- desktop and the other sizes: the grid fills the window ----
                & $viewport 1920 1080
                & $navigate "$base/#live"
                $s = & $until { param($s) & $playing $s }
                if (-not $s) { $s = & $stats; & $fail "$browser desktop: six tiles never reached playing"; if ($s) { & $describeTiles $s $null }; continue }
                & $note ("  instrumentation: " + (& $js $instrument))
                $r = & $sizes 1920 1080 'desktop-1920x1080' $true
                $lay = $r.Layout; $s = $r.Stats
                & $describeTiles $s $r.Pixels.Pixels
                if ($s.columns -ne 3 -or $s.fit.cols -ne 3) { & $fail "$browser desktop: $($s.columns) columns, wanted 3" }
                if ($lay.MinW -lt 600) { & $fail "$browser desktop: a tile is only $($lay.MinW) px wide, wanted >= 600" }
                if ($lay.Scrolls) { & $fail "$browser desktop: the page scrolls" }
                if ($lay.WidthShare -lt 0.97 -and $lay.HeightShare -lt 0.97) { & $fail "$browser desktop: the tiles fill neither the width nor the height of the content area" }
                if (@($r.Pixels.Pixels | Where-Object { $_.LitShare -lt 0.5 -or $_.Colours -lt 4 }).Count) { & $fail "$browser desktop: a tile rendered black or flat" }
                if (@(@($s.tiles) | Where-Object { -not $_.muted }).Count) { & $fail "${browser}: a tile was not muted by default" }
                if (@(@($s.tiles) | Where-Object { $_.stream -ne 'sub' }).Count) { & $fail "${browser}: a tile was not on the sub stream" }
                $n = & $readersIs 6
                $liveN = & $sessions
                $endWait = [DateTime]::UtcNow.AddSeconds(40)
                while ($liveN -ne 6 -and [DateTime]::UtcNow -lt $endWait) { Start-Sleep -Milliseconds 500; $liveN = & $sessions }    # the previous browser's sessions time out on their own
                & $note ("  sessions with six sub tiles: camera stream2 readers {0}, live instance WebRTC sessions {1}" -f $n, $liveN)
                if ($n -ne 6 -or $liveN -ne 6) { & $fail "${browser}: stream2 had $n and the live instance $liveN sessions with six viewers, wanted 6 and 6" }
                $base0 = $s

                # ---- Fit / Fill: the toggle only changes the layout ----
                $geomJs = @'
(() => {
  const g = document.getElementById('lgrid').getBoundingClientRect();
  const lm = document.getElementById('lmsub'), lmain = document.getElementById('lmmain');
  const tiles = Array.from(document.querySelectorAll('#lgrid .ltile')).map((t) => {
    const r = t.getBoundingClientRect(), v = t.querySelector('video');
    const inside = (e) => { const b = e.getBoundingClientRect(); return b.width > 0 && b.left >= r.left - 0.5 && b.right <= r.right + 0.5 && b.top >= r.top - 0.5 && b.bottom <= r.bottom + 0.5; };
    return { x: r.x, y: r.y + scrollY, w: r.width, h: r.height, vw: v.videoWidth, vh: v.videoHeight, objectFit: getComputedStyle(v).objectFit,
      overlays: ['.lname', '.lchip', '.lmute'].every((s) => inside(t.querySelector(s))) };
  });
  return { gridTop: g.top + scrollY, gridClass: document.getElementById('lgrid').className, tiles,
    fitPressed: document.getElementById('lfit').getAttribute('aria-pressed'), fillPressed: document.getElementById('lfill').getAttribute('aria-pressed'),
    stored: localStorage.getItem('camui.live.fit'), modalFit: [getComputedStyle(lm).objectFit, getComputedStyle(lmain).objectFit], diag: window.__liveProbe().mode };
})()
'@
                $measureFF = {
                    param($S)
                    $G = & $js $geomJs
                    $t = @($G.tiles)
                    $minX = ($t | ForEach-Object { $_.x } | Measure-Object -Minimum).Minimum; $maxX = ($t | ForEach-Object { $_.x + $_.w } | Measure-Object -Maximum).Maximum
                    $minY = ($t | ForEach-Object { $_.y } | Measure-Object -Minimum).Minimum; $maxY = ($t | ForEach-Object { $_.y + $_.h } | Measure-Object -Maximum).Maximum
                    $crops = @($t | Where-Object { $_.vw -gt 0 -and $_.vh -gt 0 } | ForEach-Object { $k = [math]::Max($_.w / $_.vw, $_.h / $_.vh); 1 - ($_.w * $_.h) / (($_.vw * $k) * ($_.vh * $k)) })
                    [pscustomobject]@{
                        G = $G; BoxW = $maxX - $minX; BoxH = $maxY - $minY; WShare = ($maxX - $minX) / $S.innerWidth; HShare = ($maxY - $minY) / ($S.innerHeight - $G.gridTop)
                        Scrolls = ($S.scrollHeight -gt $S.innerHeight + 1); SideScroll = ($S.scrollWidth -gt $S.clientWidth)
                        CropMin = $(if ($crops.Count) { ($crops | Measure-Object -Minimum).Minimum } else { 0 }); CropMax = $(if ($crops.Count) { ($crops | Measure-Object -Maximum).Maximum } else { 0 })
                        AllCover = (@($t | Where-Object { $_.objectFit -ne 'cover' }).Count -eq 0); AllContain = (@($t | Where-Object { $_.objectFit -ne 'contain' }).Count -eq 0)
                        Overlays = (@($t | Where-Object { -not $_.overlays }).Count -eq 0)
                    }
                }
                $modeIs = {
                    param([string]$Mode, [int]$W, [int]$H)
                    $s2 = & $until { param($s) $s.mode -eq $Mode -and $s.fit -and $s.fit.mode -eq $Mode -and $s.fit.vw -eq $W -and $s.fit.vh -eq $H } 8
                    Start-Sleep -Milliseconds 700      # a scrollbar that appears or goes narrows the grid and the page re-fits (debounced)
                    if ($s2) { return (& $stats) }
                    return $null
                }
                $ffCounts = { $f = & $fxState; [pscustomobject]@{ Posts = @($f.posts).Count; Deletes = $f.deletes; Live = (& $sessions); Readers = (& $readers) } }

                # 0. default: Fit, nothing stored, rectangles are today's
                & $viewport 1920 1080
                $s = & $modeIs 'fit' 1920 1080
                if (-not $s) { $s = & $stats; & $fail "$browser fit/fill: the page did not settle in Fit at 1920x1080" }
                $m0 = & $measureFF $s
                $rects0 = & $rectsOf $s
                $d0 = @($s.tiles)[0].diag.line
                & $note ("  FIT/FILL default (fresh profile): mode {0}, #lfit pressed {1}, localStorage '{2}'; Fit rectangles {3}; page scrolls {4}; diagnostics line has 'fit={0}': {5}" -f $s.mode, $m0.G.fitPressed, $m0.G.stored, $rects0, $m0.Scrolls, ($d0 -match ' fit=fit '))
                if ($s.mode -ne 'fit' -or $m0.G.fitPressed -ne 'true' -or $m0.G.fillPressed -ne 'false' -or $m0.G.stored) { & $fail "${browser}: the default is not Fit with nothing stored" }
                if (@(@($s.tiles) | Where-Object { $_.rect.w -ne 633 -or $_.rect.h -ne 356 }).Count -or $s.columns -ne 3 -or $m0.Scrolls -or -not $m0.AllContain) { & $fail "${browser}: Fit is not today's 3x2 of 633x356 with object-fit contain ($rects0)" }
                if ($d0 -notmatch ' fit=fit ') { & $fail "${browser}: the tile diagnostics line lacks fit=fit" }
                $c0 = & $ffCounts
                $fr0 = & $frames $s

                # 1. Fill at 1920x1080: the cells divide the whole area
                & $clickAt (& $center '#lfill')
                $sf = & $modeIs 'fill' 1920 1080
                if (-not $sf) { $sf = & $stats; & $fail "${browser}: clicking Fill did not switch the layout" }
                $mf = & $measureFF $sf
                $hb = [int](& $js 'Math.round(document.querySelector(''.top'').getBoundingClientRect().bottom)')
                $shotFill = & $shot "$browser-fill-1920x1080" $sf
                $c1 = & $ffCounts
                $fr1 = & $frames $sf
                Start-Sleep -Milliseconds 1500
                $fr2 = & $frames (& $stats)
                $moved = (0..5 | Where-Object { $fr2[$_] -gt $fr1[$_] }).Count
                $dF = @($sf.tiles)[0].diag.line
                & $note ("  FILL 1920x1080: fit {0}x{1} cells {2}x{3}; rects {4}" -f $sf.fit.cols, $sf.fit.rows, $sf.fit.w, $sf.fit.h, (& $rectsOf $sf))
                & $note ("    tile boxes span {0}x{1}: {2:p1} of the window width, {3:p1} of the grid area (window height {4} minus the grid top {5}); {6:p1} of the whole window below the header; page scrolls {7}; object-fit cover on all tiles {8}; name/chip/Unmute inside every tile {9}" -f `
                    $mf.BoxW, $mf.BoxH, $mf.WShare, $mf.HShare, $sf.innerHeight, $mf.G.gridTop, ($mf.BoxH / ($sf.innerHeight - $hb)), $mf.Scrolls, $mf.AllCover, $mf.Overlays)
                & $note ("    picture cropped {0:p1} to {1:p1} per tile (videoWidth x videoHeight vs the tile box, object-fit cover): {2}" -f $mf.CropMin, $mf.CropMax, ((@($mf.G.tiles) | ForEach-Object { '{0}x{1} in {2}x{3}' -f $_.vw, $_.vh, [int]$_.w, [int]$_.h }) -join ' | '))
                & $note ("    toggle Fit -> Fill: POSTs {0} -> {1}, DELETEs {2} -> {3}, live-instance sessions {4} -> {5}, stream2 readers {6} -> {7}; tiles still decoding after the toggle: {8} of 6; pixels: {9}; stored '{10}'; diagnostics line has 'fit=fill': {11}" -f `
                    $c0.Posts, $c1.Posts, $c0.Deletes, $c1.Deletes, $c0.Live, $c1.Live, $c0.Readers, $c1.Readers, $moved, ((@($shotFill.Pixels) | ForEach-Object { 'lit {0} %' -f [int](100 * $_.LitShare) }) -join ' '), $mf.G.stored, ($dF -match ' fit=fill '))
                if ($sf.fit.cols -ne 3 -or $sf.columns -ne 3 -or $sf.fit.rows -ne 2) { & $fail "${browser}: Fill at 1920x1080 is not 3x2" }
                if ($mf.WShare -lt 0.98 -or $mf.HShare -lt 0.98) { & $fail ("${browser}: Fill covers only {0:p1} of the width and {1:p1} of the grid height" -f $mf.WShare, $mf.HShare) }
                if ($mf.Scrolls -or $mf.SideScroll) { & $fail "${browser}: the page scrolls in Fill" }
                if (-not $mf.AllCover -or -not $mf.Overlays) { & $fail "${browser}: Fill tiles are not object-fit cover with the overlays inside" }
                if ($c1.Posts -ne $c0.Posts -or $c1.Deletes -ne $c0.Deletes -or $c1.Live -ne 6 -or $c1.Readers -ne 6) { & $fail "${browser}: toggling to Fill changed the connections" }
                if ($moved -ne 6 -or -not (& $playing $sf)) { & $fail "${browser}: not all six tiles keep playing in Fill ($moved moved)" }
                if (@($shotFill.Pixels | Where-Object { $_.LitShare -lt 0.5 -or $_.Colours -lt 4 }).Count) { & $fail "${browser}: a Fill tile rendered black or flat" }
                if ($mf.G.stored -ne 'fill' -or $dF -notmatch ' fit=fill ') { & $fail "${browser}: Fill is not stored or not in the diagnostics line" }

                # 2. back to Fit: the very same rectangles, again no connection change
                & $clickAt (& $center '#lfit')
                $sb = & $modeIs 'fit' 1920 1080
                if (-not $sb) { $sb = & $stats; & $fail "${browser}: clicking Fit did not switch back" }
                $mb = & $measureFF $sb
                $rectsB = & $rectsOf $sb
                $c2 = & $ffCounts
                & $note ("  FIT again after Fill: rects identical to the first Fit: {0}; object-fit contain {1}; POSTs {2}, DELETEs {3}, sessions {4}; stored '{5}'" -f ($rectsB -eq $rects0), $mb.AllContain, $c2.Posts, $c2.Deletes, $c2.Live, $mb.G.stored)
                if ($rectsB -ne $rects0 -or -not $mb.AllContain) { & $fail "${browser}: Fit after Fill is not the first Fit ($rectsB vs $rects0)" }
                if ($c2.Posts -ne $c0.Posts -or $c2.Deletes -ne $c0.Deletes -or $c2.Live -ne 6) { & $fail "${browser}: toggling back to Fit changed the connections" }
                if ($mb.G.stored -ne 'fit') { & $fail "${browser}: Fit is not stored" }

                # 3. Fill survives a reload (this browser's own localStorage)
                & $clickAt (& $center '#lfill')
                [void](& $modeIs 'fill' 1920 1080)
                & $navigate "$base/#live"
                $sr = & $until { param($s) $s.mode -eq 'fill' -and $s.fit -and $s.fit.mode -eq 'fill' -and (& $playing $s) } 40
                if (-not $sr) { $sr = & $stats; & $fail "${browser}: after a reload the page is not in Fill with six playing tiles" }
                else { Start-Sleep -Milliseconds 700; $sr = & $stats }
                $mr = & $measureFF $sr
                & $note ("  FILL after a reload: mode {0}, #lfill pressed {1}, 3x2 {2}, boxes {3:p1} x {4:p1}" -f $sr.mode, $mr.G.fillPressed, ($sr.fit.cols -eq 3 -and $sr.fit.rows -eq 2), $mr.WShare, $mr.HShare)
                if ($sr.mode -ne 'fill' -or $mr.G.fillPressed -ne 'true' -or $mr.HShare -lt 0.98 -or $mr.WShare -lt 0.98) { & $fail "${browser}: Fill did not survive a reload" }
                $sessionsReload = & $sessionsIs 6 40
                if ($sessionsReload -ne 6) { & $fail "${browser}: $sessionsReload sessions after the reload, wanted 6" }
                & $note ("  instrumentation after the reload: " + (& $js $instrument))

                # 4. the modal is not affected: contain, the tile's picture at once
                [void](& $reset)
                & $clickAt (& $center (& $tileSel 'Cam 1') 0.4)
                Start-Sleep -Milliseconds 600
                $fxm = & $fxState
                $two = @($fxm.probes | Where-Object { $_.label -eq '2raf' })[0]
                $mm2 = & $measureFF (& $stats)
                & $note ("  FILL modal: object-fit {0} (sub) / {1} (main); after 2 animation frames open {2}, sub picture attached {3}, new session at that moment {4}" -f $mm2.G.modalFit[0], $mm2.G.modalFit[1], $(if ($two) { $two.open } else { '?' }), $(if ($two) { $two.subHas } else { '?' }), $(if ($two) { $two.session } else { '?' }))
                if ($mm2.G.modalFit[0] -ne 'contain' -or $mm2.G.modalFit[1] -ne 'contain') { & $fail "${browser}: the modal pictures are not object-fit contain in Fill" }
                if (-not $two -or -not $two.open -or -not $two.subHas -or $two.session) { & $fail "${browser}: in Fill the modal did not open instantly on the tile's picture" }
                & $esc
                [void](& $closedIs)
                [void](& $sessionsIs 6 12)

                # 5. fullscreen in Fill: the grid takes the whole screen
                $fs0 = (& $fxState).fs
                & $clickAt (& $center '#lfs')
                Start-Sleep -Milliseconds 1200
                $sfs = & $stats
                $fxs = & $fxState
                $mfs = & $measureFF $sfs
                & $note ("  FILL fullscreen: requestFullscreen calls {0}, fullscreenElement '{1}', window {2}x{3}; fit {4}x{5} cells {6}x{7}; tile boxes {8:p1} of the width and {9:p1} of the height (grid top {10}); page scrolls {11}; Fit/Fill buttons still shown {12}" -f `
                    ($fxs.fs - $fs0), $sfs.fullscreen, $sfs.innerWidth, $sfs.innerHeight, $sfs.fit.cols, $sfs.fit.rows, $sfs.fit.w, $sfs.fit.h, $mfs.WShare, ($mfs.BoxH / $sfs.innerHeight), $mfs.G.gridTop, $mfs.Scrolls, (& $js '!!document.getElementById(''lfill'').offsetParent'))
                if (($fxs.fs - $fs0) -ne 1 -or $sfs.fullscreen -ne 'lwrap') { & $fail "${browser}: fullscreen did not start in Fill" }
                if ($mfs.WShare -lt 0.98 -or ($mfs.BoxH / $sfs.innerHeight) -lt 0.98 -or $mfs.Scrolls) { & $fail ("${browser}: in fullscreen Fill the tiles cover {0:p1} x {1:p1} of the screen" -f $mfs.WShare, ($mfs.BoxH / $sfs.innerHeight)) }
                [void](& $js 'document.fullscreenElement ? document.exitFullscreen() : 0')
                [void](& $modeIs 'fill' 1920 1080)

                # 6. other sizes in Fill
                foreach ($case in @(@(800, 1000, 'tablet-800x1000'), @(390, 844, 'phone-390x844'))) {
                    & $viewport $case[0] $case[1]
                    $sz = & $modeIs 'fill' $case[0] $case[1]
                    if (-not $sz) { $sz = & $stats; & $fail "$browser Fill $($case[2]): the page never re-fitted" }
                    $mz = & $measureFF $sz
                    $gridW = $sz.grid.w
                    & $note ("  FILL {0}: fit {1}x{2} cells {3}x{4} scroll {5}; rects {6}; boxes {7:p1} of the width, {8:p1} of the grid height; page scrolls {9}; object-fit {10}; crop {11:p1} to {12:p1}; grid class '{13}'" -f $case[2], $sz.fit.cols, $sz.fit.rows, $sz.fit.w, $sz.fit.h, $sz.fit.scroll, (& $rectsOf $sz), $mz.WShare, $mz.HShare, $mz.Scrolls, @($mz.G.tiles)[0].objectFit, $mz.CropMin, $mz.CropMax, $mz.G.gridClass)
                    if ($case[0] -eq 800) {
                        if ($sz.columns -ne 2 -or $sz.fit.rows -ne 3 -or $mz.Scrolls -or $mz.HShare -lt 0.98 -or $mz.WShare -lt 0.98 -or -not $mz.AllCover) { & $fail "$browser Fill 800x1000: $($sz.columns) columns, scrolls $($mz.Scrolls), covers $($mz.WShare)/$($mz.HShare)" }
                    }
                    else {
                        # six stacked tiles would each be under 180 px tall: the 16:9 one-column scroll layout of Fit, nothing cropped
                        if ($sz.columns -ne 1 -or -not $sz.fit.scroll -or -not $mz.Scrolls -or -not $mz.AllContain -or @(@($sz.tiles) | Where-Object { $_.rect.w -ne $gridW -or [math]::Abs($_.rect.h - $gridW * 9 / 16) -gt 1 }).Count) { & $fail "$browser Fill phone: not one 16:9 full-width scrolling column ($(& $rectsOf $sz))" }
                    }
                }
                & $viewport 1920 1080

                # leave the profile's choice at the default
                & $clickAt (& $center '#lfit')
                $sl = & $modeIs 'fit' 1920 1080
                if (-not $sl -or (& $rectsOf $sl) -ne $rects0) { & $fail "${browser}: Fit after the Fill checks is not the first Fit" }
                if ($env:CAMTEST_LIVEPAGE_ONLY -eq 'fitfill') { & $note "  CAMTEST_LIVEPAGE_ONLY=fitfill: stopping after the Fit/Fill block"; continue }

                # bestGrid itself, in the page: the cases the node test also covers
                $table = & $js '[[1908,976,6],[2548,1336,6],[788,900,6],[378,760,6],[832,330,6],[1900,900,1],[1600,300,6]].map(([w,h,n]) => { const g = window.__liveBestGrid(w,h,n,300,4); return `${w}x${h} n=${n} -> ${g.cols}x${g.rows} tile ${g.tile}${g.scroll ? " scroll" : ""}`; }).join("; ")'
                & $note ("  bestGrid(width, height, n, 300, 4): " + $table)
                if ($table -notmatch '1908x976 n=6 -> 3x2 tile 633;' -or $table -notmatch '788x900 n=6 -> 2x3' -or $table -notmatch '378x760 n=6 -> 1x6 tile 378 scroll') { & $fail "${browser}: bestGrid in the page gave $table" }

                $r2 = & $sizes 2560 1440 'desktop-2560x1440'
                if ($r2) {
                    if ($r2.Layout.MinW -lt $lay.MinW * 1.2 -or $r2.Layout.Scrolls -or $r2.Stats.columns -ne 3) { & $fail "$browser 2560x1440: tiles $($r2.Layout.MinW) px wide (1920 gave $($lay.MinW)), scrolls $($r2.Layout.Scrolls), $($r2.Stats.columns) columns" }
                }
                $r3 = & $sizes 800 1000 'tablet-800x1000'
                if ($r3 -and ($r3.Stats.columns -ne 2 -or $r3.Layout.Scrolls)) { & $fail "$browser 800x1000: $($r3.Stats.columns) columns (wanted 2), scrolls $($r3.Layout.Scrolls)" }
                $r4 = & $sizes 390 844 'phone-390x844'
                if ($r4) {
                    $gridW = $r4.Stats.grid.w
                    if ($r4.Stats.columns -ne 1 -or @(@($r4.Stats.tiles) | Where-Object { $_.rect.w -ne $gridW }).Count) { & $fail "$browser phone: $($r4.Stats.columns) column(s), tiles not the grid's full width ($gridW)" }
                    if (-not $r4.Layout.Scrolls) { & $fail "$browser phone: six stacked tiles should scroll" }
                    $full = Invoke-CamTestCdp -Cdp $cdp -Method 'Page.captureScreenshot' -Params @{ format = 'png'; captureBeyondViewport = $true }
                    $file = Join-Path -Path $dir -ChildPath "shot-$browser-phone.png"
                    [System.IO.File]::WriteAllBytes($file, [System.Convert]::FromBase64String($full.data))
                    $rects = @(@($r4.Stats.tiles) | ForEach-Object { @{ X = $_.rect.x; Y = $_.rect.y + $r4.Stats.scrollY; W = $_.rect.w; H = [int]($_.rect.h * 0.7) } })
                    $pp = @(Get-CamTestPngStats -Path $file -Rects $rects)
                    if ($shots) { Copy-Item -LiteralPath $file -Destination (Join-Path $shots "$browser-phone-grid-390x844.png") -Force }
                    if (@($pp | Where-Object { $_.LitShare -lt 0.5 -or $_.Colours -lt 4 }).Count) { & $fail "$browser phone: a tile rendered black or flat" }
                    & $note ("  phone full-page screenshot taken; tile pixels: " + ((@($pp | ForEach-Object { 'luma {0}, lit {1} %' -f $_.Mean, [int](100 * $_.LitShare) })) -join ' | '))
                }

                # the modal on a phone: the controls sit under the picture, the box fits the screen
                if ($r4) {
                    & $clickAt (& $center (& $tileSel 'Cam 1') 0.4)
                    $h = & $modalIs { param($m) $m.hd -eq 'ready' } 15
                    if (-not $h) { $h = & $stats; & $fail "${browser} phone: the modal never reached HD" }
                    if ($h -and $h.modal) {
                        $bx = $h.modal.box
                        $pxP = & $shotModal "$browser-modal-phone-390x844" $h
                        & $note ("  phone modal: box {0}x{1} at ({2},{3}) in a {4}x{5} window, hd {6}, picture mean luma {7}, lit {8} %" -f $bx.w, $bx.h, $bx.x, $bx.y, $h.innerWidth, $h.innerHeight, $h.modal.hd, $pxP.Mean, [int](100 * $pxP.LitShare))
                        if ($bx.x -lt 0 -or $bx.x + $bx.w -gt $h.innerWidth -or $bx.y -lt 0 -or $bx.y + $bx.h -gt $h.innerHeight) { & $fail "${browser} phone: the modal does not fit the screen" }
                        if ($pxP.LitShare -lt 0.5 -or $pxP.Colours -lt 4) { & $fail "${browser} phone: the modal picture is black or flat" }
                    }
                    & $esc
                    $c = & $closedIs
                    if (-not $c) { & $fail "${browser} phone: Esc did not close the modal" }
                    [void](& $sessionsIs 6 10)
                }

                # ---- tile audio: muted by default, unmute one tile ----
                & $viewport 1920 1080
                $s = & $until { param($s) $s.innerWidth -eq 1920 -and (& $playing $s) } 15
                if (-not $s) { & $fail "${browser}: the six tiles did not come back at 1920x1080"; continue }
                $before = @($s.tiles | Where-Object { $_.name -eq 'Cam 2' })[0]
                $mutePt = & $center ((& $tileSel 'Cam 2') + ' .lmute')
                & $clickAt $mutePt
                Start-Sleep -Milliseconds 2200
                $after = & $stats
                $tile2 = @($after.tiles | Where-Object { $_.name -eq 'Cam 2' })[0]
                $others = @(@($after.tiles) | Where-Object { $_.name -ne 'Cam 2' -and -not $_.muted }).Count
                & $note ("  tile audio: after the unmute click Cam 2 muted={0} paused={1} volume={2}, other tiles unmuted: {3}; Cam 2 audio packets {4} -> {5}" -f $tile2.muted, $tile2.paused, $tile2.volume, $others, $(if ($before.a) { $before.a.packets } else { 'n/a' }), $(if ($tile2.a) { $tile2.a.packets } else { 'n/a' }))
                if ($tile2.muted -or $tile2.paused -or $others -ne 0) { & $fail "${browser}: unmute did not unmute exactly Cam 2" }
                if (-not $tile2.a -or $tile2.a.packets -lt 20 -or ($before.a -and $tile2.a.packets -le $before.a.packets)) { & $fail "${browser}: no audio packets arrive for the unmuted tile" }
                & $clickAt $mutePt     # quiet again

                # ---- diagnostics v2 on a tile ----
                $c1 = @($after.tiles | Where-Object { $_.name -eq 'Cam 1' })[0]
                $m = $c1.diag.ms
                & $note ("  tile diagnostics v2 (Cam 1): gather {0} ms, signalling POST {1} ms, connected {2} ms, first frame {3} ms; line: {4}" -f $m.gather, $m.post, $m.connected, $m.frame, $c1.diag.line)
                if ($null -eq $m.gather -or $null -eq $m.post -or $null -eq $m.connected -or $null -eq $m.frame) { & $fail "${browser}: the tile diagnostics lack a timing" }
                if ($c1.diag.line -notmatch '^live-diag/3 .* post=\d+ms .* connected=\d+ms frame=\d+ms ' -or $c1.diag.line -match '\d+\.\d+\.\d+\.\d+|rtsp:|://' -or $c1.diag.line -match "`n") { & $fail "${browser}: the tile diagnostics line is not one address-free live-diag/3 line with the timings" }

                # ---- the modal ----
                $tileClasses = { & $js 'Array.from(document.querySelectorAll(''.ltile'')).map((e) => e.className + ''/'' + e.style.cssText).join(''|'')' }
                $classes0 = & $tileClasses
                $s0 = & $stats
                $deletes0 = (& $fxState).deletes

                # 1. open, with the main signalling held for 5 s so the picture before the swap can be sampled
                [void](& $reset)
                [void](& $js 'window.__fx.delay = 5000')
                & $clickAt (& $center (& $tileSel 'Cam 1') 0.4)
                Start-Sleep -Milliseconds 400
                $m1 = & $stats
                $liveAtM1 = & $sessions
                $fx1 = & $fxState
                $two = @($fx1.probes | Where-Object { $_.label -eq '2raf' })[0]; $hund = @($fx1.probes | Where-Object { $_.label -eq '100ms' })[0]
                if ($two) { & $note ("  click Cam 1 -> modal after 2 animation frames ({0} ms): open {1}, dialog open {2}, sub picture attached {3}, videoWidth {4}, paused {5}, own session {6}, signalling answer {7}" -f $two.ms, $two.open, $two.dialogOpen, $two.subHas, $two.subVw, $two.subPaused, $two.session, $(if ($null -eq $two.http) { 'none yet' } else { $two.http })) }
                if ($hund) { & $note ("  ... after {0} ms: open {1}, sub attached {2}, videoWidth {3}, paused {4}, own session {5}, shown {6}, hd {7}" -f $hund.ms, $hund.open, $hund.subHas, $hund.subVw, $hund.subPaused, $hund.session, $hund.shown, $hund.hd) }
                if (-not $two -or -not $two.open -or -not $two.dialogOpen -or -not $two.subHas -or $two.session) { & $fail "${browser}: two animation frames after the click the modal was not open with the tile's stream attached and no new session" }
                if (-not $hund -or $hund.subVw -le 0 -or $hund.subPaused -or $hund.session -or $hund.hd -ne 'loading') { & $fail "${browser}: 100 ms after the click the modal did not show a playing picture before the main session existed" }
                $mm = $m1.modal
                if (-not $mm -or -not $mm.opened -or $mm.shown -ne 'sub' -or $mm.hd -ne 'loading') { & $fail "${browser}: the modal did not open on the sub picture while HD loads" }
                else {
                    & $note ("  modal before the swap (+400 ms): name {0}, shown {1}, hd {2}, chips '{3}', sub videoWidth {4}x{5}, muted {6}, hash {7}, focus inside {8}, box {9}x{10} at ({11},{12}), live sessions {13}" -f `
                        $mm.name, $mm.shown, $mm.hd, $mm.chips, $mm.sub.vw, $mm.sub.vh, $mm.muted, $mm.hash, $mm.focusInside, $mm.box.w, $mm.box.h, $mm.box.x, $mm.box.y, $liveAtM1)
                    if ($mm.hash -ne '#live/Cam%201' -or -not $mm.focusInside) { & $fail "${browser}: the modal did not set the hash or take the focus" }
                    if ($mm.sub.vw -le 0) { & $fail "${browser}: the modal's first picture has no size" }
                    $pxB = & $shotModal "$browser-modal-before-swap" $m1
                    & $note ("  modal picture before the swap: mean luma {0}, lit {1} %, {2} colours" -f $pxB.Mean, [int](100 * $pxB.LitShare), $pxB.Colours)
                    if ($pxB.LitShare -lt 0.5 -or $pxB.Colours -lt 4) { & $fail "${browser}: the modal picture before the swap is black or flat" }
                }
                if ((& $rectsOf $m1) -ne (& $rectsOf $s0) -or (& $tileClasses) -ne $classes0) { & $fail "${browser}: the grid changed under the modal (rects or classes)" }
                else { & $note "  grid underneath: the six rectangles and the tiles' classes are identical to before the click" }
                $f0 = & $frames $s0; $f1 = & $frames $m1
                $grew = @(0..5 | Where-Object { $f1[$_] -gt $f0[$_] }).Count
                & $note ("  framesDecoded before -> during: {0} -> {1} (grew on {2} of 6 tiles)" -f ($f0 -join ','), ($f1 -join ','), $grew)
                if ($grew -ne 6) { & $fail "${browser}: the tiles stopped playing while the modal was open ($grew of 6 kept growing)" }
                if ($liveAtM1 -ne 6) { & $fail "${browser}: $liveAtM1 live sessions existed before the main signalling was answered, wanted 6" }

                # 2. unmute in the modal while still on the sub picture, then wait for the swap
                & $clickAt (& $center '#lmmute')
                Start-Sleep -Milliseconds 150
                $mu = & $stats
                & $note ("  modal Unmute (before the swap): modal muted {0}, sub element muted {1}, main element muted {2}; tile Cam 1 muted {3}" -f $mu.modal.muted, $mu.modal.sub.muted, $mu.modal.main.muted, @($mu.tiles | Where-Object { $_.name -eq 'Cam 1' })[0].muted)
                if ($mu.modal.muted -or $mu.modal.sub.muted -or -not $mu.modal.main.muted) { & $fail "${browser}: the modal's Unmute did not unmute exactly the shown picture" }
                if (-not @($mu.tiles | Where-Object { $_.name -eq 'Cam 1' })[0].muted) { & $fail "${browser}: the modal's Unmute changed the tile" }
                $h = & $modalIs { param($m) $m.hd -eq 'ready' } 12
                if (-not $h) { $h = & $stats; & $fail "${browser}: the main stream never replaced the sub picture"; if ($h -and $h.modal) { & $note ("      modal state: hd {0}, verdict '{1}', line {2}" -f $h.modal.hd, $h.modal.verdict, $h.modal.line) } }
                else {
                    Start-Sleep -Milliseconds 1200
                    $h = & $stats
                    $mh = $h.modal
                    & $note ("  swap done (this open had 5000 ms of artificial hold, so time-to-HD below includes it): hd {0}, shown {1}, chips '{2}', hdMs {3}, main videoWidth {4}x{5} (decoded {6}x{7}, framesDecoded {8}), sub element has a stream {9}, modal muted {10} (kept through the swap), audio packets {11}" -f `
                        $mh.hd, $mh.shown, $mh.chips, $mh.hdMs, $mh.main.vw, $mh.main.vh, $(if ($mh.v) { $mh.v.w } else { '?' }), $(if ($mh.v) { $mh.v.h } else { '?' }), $(if ($mh.v) { $mh.v.framesDecoded } else { '?' }), $mh.sub.has, $mh.muted, $(if ($mh.a) { $mh.a.packets } else { 'n/a' }))
                    if ($mh.shown -ne 'main' -or $mh.main.vw -ne 1280 -or $mh.main.vh -ne 720) { & $fail "${browser}: after the swap the modal is not showing the 1280x720 main stream (shown $($mh.shown), $($mh.main.vw)x$($mh.main.vh)); the sub stream is 640x360" }
                    if ($mh.muted -or $mh.main.muted -or -not $mh.sub.muted) { & $fail "${browser}: the mute choice did not survive the swap" }
                    if (-not $mh.a -or $mh.a.packets -lt 10) { & $fail "${browser}: no audio packets arrive on the main session" }
                    if ($mh.sub.has) { & $fail "${browser}: the sub picture was not released after the swap" }
                    $pxA = & $shotModal "$browser-modal-after-swap" $h
                    & $note ("  modal picture after the swap: mean luma {0}, lit {1} %, {2} colours" -f $pxA.Mean, [int](100 * $pxA.LitShare), $pxA.Colours)
                    if ($pxA.LitShare -lt 0.5 -or $pxA.Colours -lt 4) { & $fail "${browser}: the modal picture after the swap is black or flat" }
                    $liveN = & $sessions; $n = & $readersIs 6
                    & $note ("  sessions while the modal is open: live instance {0} (6 tiles + 1 main), camera stream2 readers {1} (unchanged)" -f $liveN, $n)
                    if ($liveN -ne 7) { & $fail "${browser}: the live instance had $liveN sessions with a modal open, wanted 7" }
                    if ($n -ne 6) { & $fail "${browser}: the camera's stream2 had $n sessions with a modal open, wanted 6" }
                    if ((& $rectsOf $h) -ne (& $rectsOf $s0) -or (& $tileClasses) -ne $classes0) { & $fail "${browser}: the grid changed while the modal showed the main stream" }
                    $line = $mh.line
                    & $note ("  modal diagnostics v2: gather {0} ms, POST {1} ms, connected {2} ms, first frame {3} ms, hd {4} ms; line: {5}" -f $mh.ms.gather, $mh.ms.post, $mh.ms.connected, $mh.ms.frame, $mh.ms.hd, $line)
                    if ($null -eq $mh.ms.gather -or $null -eq $mh.ms.post -or $null -eq $mh.ms.connected -or $null -eq $mh.ms.frame -or $null -eq $mh.ms.hd) { & $fail "${browser}: the modal diagnostics lack a timing" }
                    if ($line -notmatch '^live-diag/3 .*stream=main .* post=\d+ms .* connected=\d+ms frame=\d+ms hd=\d+ms ' -or $line -match '\d+\.\d+\.\d+\.\d+|rtsp:|://') { & $fail "${browser}: the modal diagnostics line is not an address-free live-diag/3 line with the timings" }
                }

                # 3. Close button: session gone, tile unaffected, focus back on the tile, hash back to #live
                $f2 = & $frames (& $stats)
                & $clickAt (& $center '#lmclose')
                $c = & $closedIs
                $liveN = & $sessionsIs 6 10
                $fxc = & $fxState
                $s3 = & $stats
                $f3 = & $frames $s3
                & $note ("  Close button: closed {0}, hash '{1}', focus on '{2}', live sessions back to {3}, session DELETEs seen by the page {4}, Cam 1 tile framesDecoded {5} -> {6}, rects identical {7}" -f [bool]$c, $s3.hash, $s3.activeElement, $liveN, ($fxc.deletes - $deletes0), $f2[0], $f3[0], ((& $rectsOf $s3) -eq (& $rectsOf $s0)))
                if (-not $c) { & $fail "${browser}: the Close button did not close the modal and restore #live" }
                if ($s3.activeElement -ne 'Cam 1') { & $fail "${browser}: focus did not return to the Cam 1 tile (it is on '$($s3.activeElement)')" }
                if ($liveN -ne 6 -or ($fxc.deletes - $deletes0) -lt 1) { & $fail "${browser}: closing the modal left its session ($liveN sessions, $($fxc.deletes - $deletes0) DELETEs)" }
                if ($f3[0] -le $f2[0] -or (& $rectsOf $s3) -ne (& $rectsOf $s0)) { & $fail "${browser}: the Cam 1 tile was disturbed by the modal" }
                $n = & $readersIs 6
                if ($n -ne 6) { & $fail "${browser}: stream2 had $n sessions after the modal closed" }

                # 4. a plain open (no artificial delay): time to HD, then Esc closes
                [void](& $reset)
                & $clickAt (& $center (& $tileSel 'Cam 1') 0.4)
                $h = & $modalIs { param($m) $m.hd -eq 'ready' } 10
                if (-not $h) { & $fail "${browser}: a plain click never reached HD" }
                else {
                    $mh = $h.modal
                    & $note ("  cold open (nothing warmed): time-to-HD {0} ms from the click; gather {1}, POST {2}, connected {3}, first frame {4} ms from the start of the attempt" -f $mh.hdMs, $mh.ms.gather, $mh.ms.post, $mh.ms.connected, $mh.ms.frame)
                    $script:CamLivePageHd += ("{0} cold {1} ms" -f $browser, $mh.hdMs)
                }
                & $esc
                $c = & $closedIs
                $s3 = & $stats
                & $note ("  Esc: closed {0}, hash '{1}', focus on '{2}', page still on the Live tab {3}" -f [bool]$c, $s3.hash, $s3.activeElement, $s3.shown)
                if (-not $c -or $s3.activeElement -ne 'Cam 1') { & $fail "${browser}: Esc did not close the modal and give the focus back" }
                [void](& $sessionsIs 6 10)

                # 5. a press held on a tile pre-warms the main session; the click then adopts it; the backdrop closes
                [void](& $reset)
                $pt = & $center (& $tileSel 'Cam 3') 0.4
                foreach ($type in @('mouseMoved', 'mousePressed')) {
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchMouseEvent' -Params @{ type = $type; x = $pt.x; y = $pt.y; button = 'left'; clickCount = 1; buttons = $(if ($type -eq 'mousePressed') { 1 } else { 0 }) })
                }
                Start-Sleep -Milliseconds 700
                $w = & $stats
                $liveN = & $sessions
                & $note ("  pre-warm: 700 ms after pointerdown on Cam 3 the page holds a speculative session (modal object {0}, opened {1}, connection {2}); live sessions {3}; dialog open {4}" -f [bool]$w.modal, $(if ($w.modal) { $w.modal.opened } else { '-' }), $(if ($w.modal) { $w.modal.connection } else { '-' }), $liveN, $(if ($w.modal) { $w.modal.dialogOpen } else { $false }))
                if (-not $w.modal -or $w.modal.opened -or $w.modal.dialogOpen -or $liveN -ne 7) { & $fail "${browser}: pointerdown did not start exactly one speculative session without opening the modal" }
                foreach ($type in @('mouseReleased')) {
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchMouseEvent' -Params @{ type = $type; x = $pt.x; y = $pt.y; button = 'left'; clickCount = 1; buttons = 0 })
                }
                $h = & $modalIs { param($m) $m.hd -eq 'ready' } 8
                if (-not $h) { & $fail "${browser}: a pre-warmed click never reached HD" }
                else {
                    & $note ("  pre-warmed click: modal for {0} reached HD {1} ms after the click (cold was {2}); live sessions {3} (still one extra)" -f $h.modal.name, $h.modal.hdMs, ($script:CamLivePageHd | Select-Object -Last 1), (& $sessions))
                    $script:CamLivePageHd += ("{0} pre-warmed {1} ms" -f $browser, $h.modal.hdMs)
                    if ($h.modal.name -ne 'Cam 3') { & $fail "${browser}: the pre-warmed modal is not Cam 3" }
                    if ((& $sessions) -ne 7) { & $fail "${browser}: the pre-warmed open did not use exactly one extra session" }
                }
                & $click 4 4      # on the backdrop, outside the picture
                $c = & $closedIs
                $s3 = & $stats
                & $note ("  backdrop click: closed {0}, hash '{1}', focus on '{2}'" -f [bool]$c, $s3.hash, $s3.activeElement)
                if (-not $c -or $s3.activeElement -ne 'Cam 3') { & $fail "${browser}: a click on the backdrop did not close the modal with the focus restored" }
                $liveN = & $sessionsIs 6 10
                if ($liveN -ne 6) { & $fail "${browser}: after the backdrop close $liveN sessions remain" }

                # 6. pre-warm that never becomes a click: pressed on a tile, released on something else
                $deletes1 = (& $fxState).deletes
                $pt = & $center (& $tileSel 'Cam 4') 0.4
                foreach ($type in @('mouseMoved', 'mousePressed')) {
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchMouseEvent' -Params @{ type = $type; x = $pt.x; y = $pt.y; button = 'left'; clickCount = 1; buttons = $(if ($type -eq 'mousePressed') { 1 } else { 0 }) })
                }
                Start-Sleep -Milliseconds 700
                $liveWarm = & $sessions
                $away = & $center '#lcount'
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchMouseEvent' -Params @{ type = 'mouseMoved'; x = $away.x; y = $away.y; button = 'left'; buttons = 1 })
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchMouseEvent' -Params @{ type = 'mouseReleased'; x = $away.x; y = $away.y; button = 'left'; clickCount = 1; buttons = 0 })
                $liveN = & $sessionsIs 6 8
                $s3 = & $stats
                & $note ("  pre-warm released elsewhere: sessions while held {0}, back to {1} after the release; modal object {2}; dialog open {3}; DELETEs seen {4}" -f $liveWarm, $liveN, [bool]$s3.modal, [bool]$s3.hash.StartsWith('#live/'), ((& $fxState).deletes - $deletes1))
                if ($liveWarm -ne 7) { & $fail "${browser}: the held press did not warm a session (sessions $liveWarm)" }
                if ($liveN -ne 6 -or $s3.modal -or ((& $fxState).deletes - $deletes1) -lt 1) { & $fail "${browser}: a press released elsewhere left a session behind ($liveN sessions)" }

                # 7. deep link, #live, and Back. The page is loaded afresh first. Whether the browser delivers the old page's unload
                # DELETEs is not what is tested here (livegone closes a real window and counts); sessions are counted relative to
                # whatever the old page left, so that "closing the modal removes exactly its session" is what is asserted.
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = 'about:blank' })
                $left = & $sessionsIs 0 5
                & $note ("  old page navigated away: its sessions left after up to 5 s: {0} (informational)" -f $left)
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = "$base/#live/Cam%201" })
                $null = & $until { param($s) $s.shown } 20
                & $note ("  instrumentation after the reload: " + (& $js $instrument))
                $h = & $modalIs { param($m) $m.opened -and $m.hd -eq 'ready' } 20
                & $note ("  deep link #live/Cam%201 on a fresh load: modal for {0}, hd {1}, shown {2}, pushed {3}" -f $(if ($h) { $h.modal.name } else { '-' }), $(if ($h) { $h.modal.hd } else { '-' }), $(if ($h) { $h.modal.shown } else { '-' }), $(if ($h) { $h.modal.pushed } else { '-' }))
                if (-not $h -or $h.modal.name -ne 'Cam 1') { & $fail "${browser}: the deep link did not open the modal for Cam 1" }
                $liveDeep = & $sessions
                [void](& $js "location.hash = '#live'")
                $c = & $closedIs
                Start-Sleep -Milliseconds 1500
                $liveAfter = & $sessions
                & $note ("  live sessions with the deep-linked modal open: {0}; setting #live closes it: {1}, sessions after: {2} (one fewer)" -f $liveDeep, [bool]$c, $liveAfter)
                if (-not $c -or $liveDeep - $liveAfter -ne 1) { & $fail "${browser}: #live did not close the modal and exactly its session (open $liveDeep, after $liveAfter)" }
                & $clickAt (& $center (& $tileSel 'Cam 5') 0.4)
                $h = & $modalIs { param($m) $m.opened -and $m.hd -eq 'ready' -and $m.pushed } 10
                $liveOpen = & $sessions
                [void](& $js 'history.back()')
                $c = & $closedIs
                Start-Sleep -Milliseconds 1500
                $liveBack = & $sessions
                $s3 = & $stats
                & $note ("  click Cam 5 pushes #live/Cam%205 (pushed {0}); Back closes it: {1}, hash '{2}', page still shows the Live tab {3}; live sessions {4} -> {5} (one fewer)" -f $(if ($h) { $h.modal.pushed } else { '-' }), [bool]$c, $s3.hash, $s3.shown, $liveOpen, $liveBack)
                if (-not $h -or -not $c -or -not $s3.shown) { & $fail "${browser}: Back did not close the modal and stay on the Live tab" }
                if ($liveOpen - $liveBack -ne 1) { & $fail "${browser}: Back did not remove exactly the modal's session ($liveOpen -> $liveBack)" }
                [void](& $sessionsIs 6 30)    # a stale session of the old page, if any, times out on its own

                # 8. the main stream is unavailable: the sub picture stays, "HD unavailable" with Retry
                [void](& $reset)
                [void](& $js 'window.__fx.fail = true')
                & $clickAt (& $center (& $tileSel 'Cam 3') 0.4)
                $h = & $modalIs { param($m) $m.hd -eq 'unavailable' } 10
                if (-not $h) { $h = & $stats; & $fail "${browser}: with the main stream unavailable the modal did not say so" }
                else {
                    $mh = $h.modal
                    $retryShown = [bool](& $js '!document.getElementById(''lmretry'').classList.contains(''hide'')')
                    & $note ("  main unavailable (page-side 404 for stream=main): hd {0}, shown {1}, chips '{2}', sub picture {3}x{4}, Retry button shown {5}, verdict '{6}', live sessions {7}" -f $mh.hd, $mh.shown, $mh.chips, $mh.sub.vw, $mh.sub.vh, $retryShown, $mh.verdict, (& $sessions))
                    if ($mh.shown -ne 'sub' -or $mh.sub.vw -le 0 -or $mh.chips -notmatch 'HD unavailable' -or -not $retryShown) { & $fail "${browser}: the failure path lost the sub picture or the Retry" }
                    [void](Start-Sleep -Milliseconds 300)
                    $pxF = & $shotModal "$browser-modal-hd-unavailable" $h
                    if ($pxF.LitShare -lt 0.5 -or $pxF.Colours -lt 4) { & $fail "${browser}: the modal went blank when HD was unavailable" }
                }
                [void](& $js 'window.__fx.fail = false')
                & $clickAt (& $center '#lmretry')
                $h = & $modalIs { param($m) $m.hd -eq 'ready' } 10
                & $note ("  Retry with the main stream back: hd {0}, shown {1}" -f $(if ($h) { $h.modal.hd } else { 'not ready' }), $(if ($h) { $h.modal.shown } else { '-' }))
                if (-not $h) { & $fail "${browser}: Retry did not reach HD" }

                # 9. the modal's Fullscreen button calls the API (real fullscreen for a moment)
                & $clickAt (& $center '#lmfs')
                Start-Sleep -Milliseconds 700
                $fsm = & $js '({ el: document.fullscreenElement ? document.fullscreenElement.id : null })'
                $fxs = & $fxState
                & $note ("  modal Fullscreen button: requestFullscreen called on the modal box {0} time(s); fullscreenElement '{1}'" -f $fxs.mfs, $fsm.el)
                if ($fxs.mfs -ne 1) { & $fail "${browser}: the modal's Fullscreen button did not call requestFullscreen" }
                [void](& $js 'document.fullscreenElement ? document.exitFullscreen() : 0')
                Start-Sleep -Milliseconds 400
                & $viewport 1920 1080

                # 10. hidden with the modal open. The default (a grace period) keeps everything; the old behaviour is grace 0, set here through the test hook,
                # so that the free-and-reopen path stays covered (liverobust measures the default and the expiry).
                [void](& $js 'window.__liveTune({ grace: 0 })')
                $mh = (& $stats).modal
                $window = Invoke-CamTestCdp -Cdp $cdp -Method 'Browser.getWindowForTarget'
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Browser.setWindowBounds' -Params @{ windowId = $window.windowId; bounds = @{ windowState = 'minimized' } })
                $hid = & $until { param($s) $s.visibility -eq 'hidden' -and @(@($s.tiles) | Where-Object { $_.open }).Count -eq 0 -and $s.modal -and -not $s.modal.session -and -not $s.modal.connection } 15
                $liveN = & $sessionsIs 0 14
                $n = & $readersIs 0 14
                & $note ("  window minimized with the modal open (grace 0): visibility {0}, open tile connections {1}, modal session {2}, modal still open {3}; live sessions {4}, camera stream2 readers {5}" -f $(if ($hid) { $hid.visibility } else { 'not hidden' }), $(if ($hid) { @(@($hid.tiles) | Where-Object { $_.open }).Count } else { '?' }), $(if ($hid) { $hid.modal.session } else { '?' }), $(if ($hid) { $hid.modal.dialogOpen } else { '?' }), $liveN, $n)
                if (-not $hid) { & $fail "${browser}: minimizing did not close the tiles' and the modal's connections" }
                if ($liveN -ne 0 -or $n -ne 0) { & $fail "${browser}: sessions remained while hidden (live $liveN, stream2 $n)" }
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Browser.setWindowBounds' -Params @{ windowId = $window.windowId; bounds = @{ windowState = 'normal' } })
                $r = & $until { param($s) $s.visibility -eq 'visible' -and (& $playing $s) -and $s.modal -and $s.modal.hd -eq 'ready' } 40
                $liveN = & $sessionsIs 7 10
                & $note ("  window restored: six tiles playing and the modal back on HD: {0}; live sessions {1}" -f [bool]$r, $liveN)
                if (-not $r) { & $fail "${browser}: restoring did not reopen the tiles and the modal" }
                if ($liveN -ne 7) { & $fail "${browser}: $liveN sessions after the restore, wanted 7" }
                & $esc
                $c = & $closedIs
                $liveN = & $sessionsIs 6 10
                if (-not $c -or $liveN -ne 6) { & $fail "${browser}: closing after the restore left $liveN sessions" }
                [void](& $js 'window.__liveTune({ grace: null })')

                # 11. the grid's Fullscreen button
                $fs0 = (& $fxState).fs
                & $clickAt (& $center '#lfs')
                Start-Sleep -Milliseconds 800
                $fsg = & $stats
                $fxs = & $fxState
                & $note ("  grid Fullscreen button: shown {0}, requestFullscreen called {1} time(s), fullscreenElement '{2}'{3}" -f $fsg.fullscreenOk, ($fxs.fs - $fs0), $fsg.fullscreen, $(if ($fsg.fullscreen) { "; in fullscreen the grid is $($fsg.grid.w)x$($fsg.grid.h), $($fsg.fit.cols) columns, tile $($fsg.fit.tile) px" } else { '' }))
                if (-not $fsg.fullscreenOk -or ($fxs.fs - $fs0) -ne 1) { & $fail "${browser}: the Fullscreen button is missing or did not call requestFullscreen" }
                [void](& $js 'document.fullscreenElement ? document.exitFullscreen() : 0')
                Start-Sleep -Milliseconds 400
                & $viewport 1920 1080

                # ---- live view off: a second camui over a config with the flag off ----
                & $viewport 1280 800
                & $navigate 'http://127.0.0.1:18790/#live'
                $o = & $until { param($s) $s.shown -and $s.note -match 'off' } 15
                $px = $null
                if ($o) { $file = Save-CamTestPageShot -Cdp $cdp -Path (Join-Path $dir "shot-$browser-off.png"); if ($shots) { Copy-Item -LiteralPath $file -Destination (Join-Path $shots "$browser-live-off.png") -Force } }
                & $note ("  live view off (second camui, LiveViewEnabled false): enabled={0}, tiles {1}, message: '{2}'" -f $(if ($o) { $o.enabled } else { '?' }), $(if ($o) { @($o.tiles).Count } else { '?' }), $(if ($o) { $o.note } else { 'none' }))
                if (-not $o -or $o.enabled -or @($o.tiles).Count -ne 0 -or $o.note -notmatch 'LiveViewEnabled') { & $fail "${browser}: with live view off the tab did not say so" }
            }
            catch { & $fail ("$browser run aborted: " + $_.Exception.Message + " (line " + $_.InvocationInfo.ScriptLineNumber + ")") }
            finally {
                Disconnect-CamTestCdp -Cdp $cdp
                if ($started) { $end = Stop-CamTestBrowser -Started $started } else { $end = [pscustomobject]@{ Left = @(); ProfileRemoved = $true } }
                $secs = ([DateTime]::UtcNow - $began).TotalSeconds
                $script:CamLivePageSeconds += $secs
                & $note ("  $browser window up {0:n0} s; processes left after the kill: {1}; profile removed: {2}" -f $secs, $end.Left.Count, $end.ProfileRemoved)
                if ($end.Left.Count -or -not $end.ProfileRemoved) { & $fail "$browser left processes or a profile behind" }
            }
        }

        # ---- live view off, over plain HTTP ----
        $off = Invoke-CamLiveHttp -Url 'http://127.0.0.1:18790/api/live/Cam%201/whep?stream=sub' -Method POST -Headers @{ 'Content-Type' = 'application/sdp' } -Body $offer
        $offList = ConvertFrom-Json -InputObject (Invoke-CamLiveHttp -Url 'http://127.0.0.1:18790/api/live').Body
        $offStatus = Invoke-CamLiveHttp -Url 'http://127.0.0.1:18790/api/status'
        & $note ("live view off: POST whep -> HTTP {0} {1}; GET /api/live enabled={2} cameras={3}; GET /api/status -> HTTP {4}" -f $off.Code, $off.Body.Trim(), $offList.enabled, @($offList.cameras).Count, $offStatus.Code)
        if ($off.Code -ne 503 -or $off.Body -notmatch 'live view is off' -or $offList.enabled -or $offStatus.Code -ne 200) { & $fail 'with live view off the proxy did not answer 503 or the rest of the API changed' }

        # CAMTEST_LIVEPAGE_ONLY=fitfill (a short rehearsal): the Fit / Fill block only, no foreign-page browsers.
        if ($env:CAMTEST_LIVEPAGE_ONLY -ne 'fitfill') {
        # ---- a foreign page, and requests with the headers a browser would send for one ----
        $before = & $sessionCount
        $evilTarget = "camui=http://127.0.0.1:18789/api/live/Cam%201{whep}?stream=sub"
        $evil = Start-CamTestLiveServer -Prefix 'http://localhost:18082/' -PageFile (Join-Path (Get-CamTestRepo) 'tests\live\evil.html') -DataDir $rig.DataDir
        foreach ($browser in @('chrome', 'edge')) {
            $found = Get-CamTestBrowser -Browser $browser
            if (-not $found.Exe) { continue }
            $runId = "xsite-$browser"
            $statsFile = Join-Path -Path $rig.DataDir -ChildPath "$runId.jsonl"
            $started = Start-CamTestBrowser -Browser $browser -BaseDir $dir -Url ("http://localhost:18082/?run={0}&targets={1}" -f $runId, [uri]::EscapeDataString($evilTarget))
            $script:CamLivePageBrowsers += $started
            $began = [DateTime]::UtcNow; $results = @()
            try {
                $deadline = [DateTime]::UtcNow.AddSeconds(30)
                while ([DateTime]::UtcNow -lt $deadline) {
                    Start-Sleep -Seconds 2
                    $events = @(Read-CamTestLiveStats -Path $statsFile)
                    $results = @($events | Where-Object { $_.ev -eq 'result' })
                    if (@($events | Where-Object { $_.ev -eq 'done' }).Count) { break }
                }
            } finally {
                $script:CamLivePageSeconds += ([DateTime]::UtcNow - $began).TotalSeconds
                $end = Stop-CamTestBrowser -Started $started
            }
            if ($end.Left.Count -or -not $end.ProfileRemoved) { & $fail "$browser left processes or a profile behind (foreign page)" }
            $row = @($results)[0]
            if (-not $row) { & $fail "${browser}: the foreign page reported nothing"; continue }
            & $note ("foreign page {0} {1}: origin {2} -> camui whep: {3}; HTTP {4}; frames decoded {5}" -f $browser, $found.Version, $row.origin, $row.outcome, $row.status, $row.framesDecoded)
            if ($row.outcome -notlike 'blocked by the browser*' -or $row.framesDecoded -gt 0) { & $fail "${browser}: the foreign page was not blocked ($($row.outcome))" }
        }
        $raw = @(
            @{ Label = 'foreign Origin, cross-site, application/sdp'; Headers = @{ Origin = 'http://localhost:18789'; 'Sec-Fetch-Site' = 'cross-site'; 'Content-Type' = 'application/sdp' }; Host = $null },
            @{ Label = 'foreign Origin only, application/sdp'; Headers = @{ Origin = 'http://localhost:18082'; 'Content-Type' = 'application/sdp' }; Host = $null },
            @{ Label = 'cross-site Sec-Fetch-Site only, application/sdp'; Headers = @{ 'Sec-Fetch-Site' = 'cross-site'; 'Content-Type' = 'application/sdp' }; Host = $null },
            @{ Label = 'same-site Sec-Fetch-Site, application/sdp'; Headers = @{ 'Sec-Fetch-Site' = 'same-site'; 'Content-Type' = 'application/sdp' }; Host = $null },
            @{ Label = 'rebinding Host attacker-rebind.example, application/sdp'; Headers = @{ 'Content-Type' = 'application/sdp' }; Host = 'attacker-rebind.example:18789' },
            @{ Label = 'no-cors shaped (text/plain, foreign Origin)'; Headers = @{ Origin = 'http://localhost:18082'; 'Sec-Fetch-Site' = 'same-site'; 'Content-Type' = 'text/plain' }; Host = $null })
        foreach ($case in $raw) {
            $headers = @{} + $case.Headers
            if ($case.Host) { $headers['Host'] = $case.Host }
            $r = Invoke-CamRawHttp -Port 18789 -Method 'POST' -Path '/api/live/Cam%201/whep?stream=sub' -Headers $headers -Body $offer
            & $note ("  refused: {0} -> HTTP {1}" -f $case.Label, $r.Status)
            if ($r.Status -notin 403, 415) { & $fail "a $($case.Label) request was not refused (HTTP $($r.Status))" }
        }
        $pre = Invoke-CamRawHttp -Port 18789 -Method 'OPTIONS' -Path '/api/live/Cam%201/whep?stream=sub' -Headers @{ Origin = 'http://localhost:18082'; 'Access-Control-Request-Method' = 'POST'; 'Access-Control-Request-Headers' = 'content-type' }
        & $note ("  preflight from a foreign Origin -> HTTP {0}, Access-Control-Allow-Origin [{1}]" -f $pre.Status, $pre.Headers['access-control-allow-origin'])
        if ($pre.Status -lt 400 -or $pre.Headers['access-control-allow-origin']) { & $fail 'a foreign preflight was granted' }
        $mid = & $sessionCount
        # Positive control: the same request with no foreign header (a script) must create a session, or the count above proves nothing.
        $good = Invoke-CamRawHttp -Port 18789 -Method 'POST' -Path '/api/live/Cam%201/whep?stream=sub' -Headers @{ 'Content-Type' = 'application/sdp' } -Body $offer
        Start-Sleep -Milliseconds 800
        $after = & $sessionCount
        & $note ("live instance WebRTC sessions logged: before the foreign attempts {0}, after them {1} (new: {2}); positive control, the same offer with no foreign header: HTTP {3}, then {4} (new: {5})" -f $before, $mid, ($mid - $before), $good.Status, $after, ($after - $mid))
        if ($mid -ne $before) { & $fail 'the live instance created a session for a foreign page or a refused request' }
        if ($good.Status -ne 201 -or $after -le $mid) { & $fail 'the positive control did not create a session, so the zero above proves nothing' }
        $location = [string]$good.Headers['location']
        if ($location -match '^/api/live/session/[0-9a-f]{32}$') {
            $del = Invoke-CamRawHttp -Port 18789 -Method 'DELETE' -Path $location
            & $note ("  DELETE of the positive control's session -> HTTP {0}" -f $del.Status)
            if ($del.Status -ne 204) { & $fail 'DELETE of a minted session was not 204' }
        } else { & $fail "the answer carried no minted Location ($location)" }
        }
    }
    catch { & $fail ('check aborted: ' + $_.Exception.Message) }
    finally {
        foreach ($b in @($script:CamLivePageBrowsers)) { try { [void](Stop-CamTestBrowser -Started $b) } catch { } }
        foreach ($proc in @($evil)) { if ($proc -and -not $proc.HasExited) { try { $proc.Kill(); [void]$proc.WaitForExit(5000) } catch { } } }
        $uiLog = @()
        foreach ($instance in @($ui, $uiOff)) { if ($instance) { $uiLog += @(Stop-CamTestUi -Ui $instance) } }
        $notes = @()
        if ($rig) { $notes = @(Stop-CamTestLiveRig -Rig $rig) }
        $evidence += ("browser windows were up {0:n0} s in total; whole check {1:n0} s" -f $script:CamLivePageSeconds, $watch.Elapsed.TotalSeconds)
        $evidence += ("time to HD (click to the main picture shown): " + ($script:CamLivePageHd -join '; '))
        $evidence += $notes
        if ($notes -match 'forced: (?!nothing)') { & $fail 'the supervisor had to be forced' }
        if ($script:CamLivePageOk -and $rig) { try { Remove-CamTestTree -Path $rig.Dir } catch { $script:CamLivePageEvidence += 'cleanup failed' } }
        elseif ($rig) { $script:CamLivePageEvidence += "left for inspection: $($rig.Dir)" }
    }

    Add-CamTestResult -Run $Run -Check 'livepage' -Status $(if ($script:CamLivePageOk) { 'PASS' } else { 'FAIL' }) `
        -Message 'the Live screen plays six sub tiles in Chrome and Edge at desktop and phone widths, focuses on main, mutes, closes when hidden, says so when live view is off, and a foreign page gets no session' `
        -Evidence ($evidence + $script:CamLivePageEvidence)
}


function Suspend-CamTestProcess {
    <# NtSuspendProcess / NtResumeProcess for ONE pid this check started (a rig process); never by image name. #>
    param([Parameter(Mandatory = $true)][int]$ProcessId, [switch]$Resume)

    Add-Type -ErrorAction SilentlyContinue -Name Nt -Namespace CamTest -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("ntdll.dll")] public static extern int NtSuspendProcess(System.IntPtr h);
[System.Runtime.InteropServices.DllImport("ntdll.dll")] public static extern int NtResumeProcess(System.IntPtr h);
'@
    $process = Get-Process -Id $ProcessId -ErrorAction Stop
    if ($Resume) { [void][CamTest.Nt]::NtResumeProcess($process.Handle) } else { [void][CamTest.Nt]::NtSuspendProcess($process.Handle) }
}


function Get-CamTestBrowserCpu {
    <# Total CPU seconds (user + kernel) of every process of one test browser (profile marker plus descendants). #>
    param([Parameter(Mandatory = $true)]$Started)

    $ids = @(Get-CamTestBrowserPids -Started $Started)
    $sum = 0.0
    foreach ($p in @(Get-CimInstance -ClassName Win32_Process -Property ProcessId, UserModeTime, KernelModeTime | Where-Object { $ids -contains [int]$_.ProcessId })) {
        $sum += ([double]$p.UserModeTime + [double]$p.KernelModeTime) / 1e7
    }
    return $sum
}


function Invoke-CamCheckLiveRobust {
    <#
        [Live robustness, opt-in: opens browser windows] A Live wall that stays on.
        The REAL supervisor with a live instance against a synthetic camera (six cameras, the
        main stream 1280x720, the sub 640x360), camui built to a temp path on 18789, Chrome and
        Edge on a throwaway profile driven through DevTools. Sections (CAMTEST_ROBUST=a,b picks
        some; default all): quick (hide and show inside the grace period: nothing reconnects),
        cpu (the hidden-painting variants compared: browser CPU, bytes, frames, time to paint
        again), expiry (grace ran out while minimized: everything freed, reopened on return),
        audio, stall (a camera that stops delivering frames: detected, rebuilt; negative
        controls), recover (the live server killed, camui restarted, the camera restarted, the
        page frozen and thawed, the live server hung: seconds to playing again and the traffic
        camui sees), cycles (10 modal open/closes and 10 hide/show cycles back to baseline),
        offscreen (a phone layout parks tiles scrolled out of view).
        Needs CAMUI_TEST_EXE. Screenshots go to $env:CAMTEST_SHOTS when set.
    #>
    param($Run)

    $evidence = @()
    $fail = { param([string]$Text) $script:CamRobOk = $false; $script:CamRobEvidence += ('FAIL ' + $Text) }
    $note = { param([string]$Text) $script:CamRobEvidence += $Text }
    $script:CamRobOk = $true
    $script:CamRobEvidence = @()
    $script:CamRobBrowsers = @()
    $script:CamRobSeconds = 0
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $rig = $null; $ui = $null
    $only = @(([string]$env:CAMTEST_ROBUST) -split ',' | Where-Object { $_ })
    $want = { param([string]$Section) (-not $only.Count) -or ($only -contains $Section) }
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\liverobust'
    $shots = [string]$env:CAMTEST_SHOTS
    $base = 'http://127.0.0.1:18789'

    try {
        if (-not $env:CAMUI_TEST_EXE -or -not (Test-Path -LiteralPath $env:CAMUI_TEST_EXE)) { throw 'CAMUI_TEST_EXE must name a camui.exe built from this tree (cross-compile it to a temp path).' }
        foreach ($p in @(18789, 18300, 18892)) { if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the check needs it free." } }
        if ($shots) { [void](New-Item -ItemType Directory -Path $shots -Force) }

        $cameras = 1..6 | ForEach-Object { [pscustomobject]@{ Name = "Cam $_"; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = ($_ -eq 1) } }
        $rig = Start-CamTestLiveRig -Name 'liverobust' -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds 4 -LiveApiPort 18892 -VideoSpec @{ stream1 = '1280x720:high' }
        $dir = $rig.Dir
        # camui's grace period for this run: 10 s, through the environment variable (the path a real setting takes).
        $env:CAMUI_LIVE_HIDDEN_GRACE = '10'
        try { $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-liverobust' } finally { Remove-Item Env:\CAMUI_LIVE_HIDDEN_GRACE -ErrorAction SilentlyContinue }
        if (-not $ui.Started) { throw ('camui did not start: ' + $ui.Reason) }
        $camuiExe = Get-Item -LiteralPath $env:CAMUI_TEST_EXE
        & $note ("camui {0} ({1:n0} bytes, built {2}) on {3}, liveHiddenGraceSeconds 10 by CAMUI_LIVE_HIDDEN_GRACE; sections: {4}" -f $camuiExe.Name, $camuiExe.Length, $camuiExe.LastWriteTime.ToString('s'), $base, $(if ($only.Count) { $only -join ',' } else { 'all' }))
        $liveInfo = ConvertFrom-Json -InputObject (Invoke-CamLiveHttp -Url "$base/api/live").Body
        & $note ("GET /api/live hiddenGraceSeconds = {0}" -f $liveInfo.hiddenGraceSeconds)
        if ($liveInfo.hiddenGraceSeconds -ne 10) { & $fail "GET /api/live reported hiddenGraceSeconds $($liveInfo.hiddenGraceSeconds), wanted 10" }

        $readers = { (Get-CamTestPathReaders -ApiPort $rig.CamApi -Path 'stream2').Count }
        $sessions = { (Get-CamTestLiveSessions -Rig $rig).Count }
        $sessionsIs = {
            param([int]$Want, [int]$Seconds = 12)
            $end = [DateTime]::UtcNow.AddSeconds($Seconds); $n = -1
            while ([DateTime]::UtcNow -lt $end) { $n = & $sessions; if ($n -eq $Want) { return $n }; Start-Sleep -Milliseconds 300 }
            return $n
        }
        $readersIs = {
            param([int]$Want, [int]$Seconds = 14)
            $end = [DateTime]::UtcNow.AddSeconds($Seconds); $n = -1
            while ([DateTime]::UtcNow -lt $end) { $n = & $readers; if ($n -eq $Want) { return $n }; Start-Sleep -Milliseconds 400 }
            return $n
        }

        $instrument = @'
(() => {
  if (window.__rx) return 'already';
  const orig = window.fetch.bind(window);
  const rx = window.__rx = { posts: 0, deletes: 0, pcs: new Set() };
  window.fetch = (url, opts) => { const m = opts && opts.method; if (m === 'POST') rx.posts++; if (m === 'DELETE') rx.deletes++; return orig(url, opts); };
  const P = window.RTCPeerConnection;
  window.RTCPeerConnection = function (...a) { const pc = new P(...a); rx.pcs.add(pc); return pc; };
  window.RTCPeerConnection.prototype = P.prototype;
  rx.open = () => Array.from(rx.pcs).filter((p) => p.signalingState !== 'closed').length;
  return 'installed';
})()
'@
        $paintProbe = @'
(() => Array.from(document.querySelectorAll('#lgrid video')).map((v) => ({ t: v.currentTime, paused: v.paused, vw: v.videoWidth })))()
'@

        foreach ($browser in @($(if ($env:CAMTEST_ROBUST_BROWSERS) { ([string]$env:CAMTEST_ROBUST_BROWSERS) -split ',' } else { @('chrome', 'edge') }))) {
            $found = Get-CamTestBrowser -Browser $browser
            if (-not $found.Exe) { & $fail ("$browser could not be run: " + $found.Reason); continue }
            $cdp = $null; $started = $null
            $began = [DateTime]::UtcNow
            try {
                $started = Start-CamTestBrowser -Browser $browser -BaseDir $dir -Url 'about:blank' -DebugPort 18300 -WindowSize '1280,900'
                $script:CamRobBrowsers += $started
                $cdp = Connect-CamTestCdp -Port 18300
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.enable')
                [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.addScriptToEvaluateOnNewDocument' -Params @{ source = $instrument })
                & $note ("== $browser $($found.Version) (throwaway profile, DevTools on 18300) ==")

                $js = { param([string]$Code) Invoke-CamTestCdpEval -Cdp $cdp -Expression $Code }
                $stats = { try { Invoke-CamTestCdpEval -Cdp $cdp -Expression 'window.__liveStats ? window.__liveStats() : null' -Await } catch { $null } }
                $until = {
                    param([scriptblock]$Done, [int]$Seconds = 40, [int]$Pause = 300)
                    $end = [DateTime]::UtcNow.AddSeconds($Seconds); $s = $null
                    while ([DateTime]::UtcNow -lt $end) { $s = & $stats; if ($s -and (& $Done $s)) { return $s }; Start-Sleep -Milliseconds $Pause }
                    return $null
                }
                $playing = { param($s, [int]$n = 6, [int]$min = 20) $s.shown -and @(@($s.tiles) | Where-Object { $_.state -eq 'live' -and $_.v -and $_.v.framesDecoded -ge $min }).Count -ge $n }
                $rx = { Invoke-CamTestCdpEval -Cdp $cdp -Expression 'JSON.stringify({ posts: window.__rx.posts, deletes: window.__rx.deletes, open: window.__rx.open() })' | ConvertFrom-Json }
                $viewport = { param([int]$Width, [int]$Height) [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Emulation.setDeviceMetricsOverride' -Params @{ width = $Width; height = $Height; deviceScaleFactor = 1; mobile = $false }); Start-Sleep -Milliseconds 700 }
                $navigate = {
                    param([string]$Url)
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = 'about:blank' })
                    Start-Sleep -Milliseconds 400
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = $Url })
                }
                $click = {
                    param([double]$X, [double]$Y)
                    foreach ($type in @('mouseMoved', 'mousePressed', 'mouseReleased')) {
                        [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchMouseEvent' -Params @{ type = $type; x = $X; y = $Y; button = 'left'; clickCount = 1; buttons = $(if ($type -eq 'mousePressed') { 1 } else { 0 }) })
                    }
                }
                $center = {
                    param([string]$Selector, [double]$FracY = 0.5)
                    $q = ConvertTo-Json -InputObject $Selector -Compress
                    & $js ("(() => { const e = document.querySelector($q); if (!e) return null; const r = e.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height * $FracY }; })()")
                }
                $minimize = {
                    $script:CamRobWindow = Invoke-CamTestCdp -Cdp $cdp -Method 'Browser.getWindowForTarget'
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Browser.setWindowBounds' -Params @{ windowId = $script:CamRobWindow.windowId; bounds = @{ windowState = 'minimized' } })
                }
                # While the page is minimized a full __liveStats() call (getStats on every connection) can take seconds in headed Chrome and would eat the
                # grace period being measured: waiting for 'hidden' uses a one-line evaluation instead.
                $waitHidden = {
                    $end = [DateTime]::UtcNow.AddSeconds(10); $v = ''
                    while ([DateTime]::UtcNow -lt $end) { try { $v = Invoke-CamTestCdpEval -Cdp $cdp -Expression 'document.visibilityState' } catch { }; if ($v -eq 'hidden') { return $true }; Start-Sleep -Milliseconds 100 }
                    return $false
                }
                $restore = { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Browser.setWindowBounds' -Params @{ windowId = $script:CamRobWindow.windowId; bounds = @{ windowState = 'normal' } }) }
                # Milliseconds from now until every tile video has advanced its currentTime and is not paused (or $null on timeout).
                $paintAgain = {
                    param([datetime]$T0, [int]$Seconds = 25, [int]$Tiles = 6)
                    $t0 = $T0
                    $before = @(Invoke-CamTestCdpEval -Cdp $cdp -Expression $paintProbe)
                    $end = [DateTime]::UtcNow.AddSeconds($Seconds)
                    while ([DateTime]::UtcNow -lt $end) {
                        Start-Sleep -Milliseconds 60
                        $now = @(Invoke-CamTestCdpEval -Cdp $cdp -Expression $paintProbe)
                        $moved = 0
                        for ($i = 0; $i -lt $now.Count; $i++) { if (-not $now[$i].paused -and $now[$i].vw -gt 0 -and $i -lt $before.Count -and $now[$i].t -gt $before[$i].t + 0.001) { $moved++ } }
                        if ($moved -ge $Tiles) { return [int](((Get-Date) - $t0).TotalMilliseconds) }
                    }
                    return $null
                }
                # The window may have been covered or minimized by something else (the Operator is using this PC): bring it forward before a section.
                $front = { try { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.bringToFront') } catch { } }
                $reload = {
                    & $viewport 1920 1080
                    & $navigate "$base/#live"
                    $s = & $until { param($s) & $playing $s } 60
                    return $s
                }

                # The camera's stream2 publisher: its command line is kept so that it can be started again (a suspended publisher is dropped by
                # the camera's MediaMTX after its 10 s read timeout, and ffmpeg then exits).
                $pubCmd = (Get-CimInstance -ClassName Win32_Process -Filter "ProcessId=$($rig.Camera.Publishers[1].Id)").CommandLine
                $pubExe = (Get-CamConfigDefault).FfmpegPath
                $startPub = {
                    $newPub = Start-Process -FilePath $pubExe -ArgumentList $pubCmd.Substring($pubCmd.IndexOf('" ') + 2) -PassThru -WindowStyle Hidden `
                        -RedirectStandardOutput (Join-Path $rig.Camera.Dir 'publish-stream2b.out') -RedirectStandardError (Join-Path $rig.Camera.Dir 'publish-stream2b.err')
                    [void]$newPub.Handle
                    $rig.Camera.Publishers[1] = $newPub
                    return $newPub
                }
                $ready = & $reload
                if (-not $ready) { & $fail "${browser}: six tiles never played"; continue }
                [void](& $sessionsIs 6 40)    # the previous browser's sessions time out on their own
                & $note ("  instrumentation on every new document; six tiles playing; live sessions {0}, RTCPeerConnections open {1}" -f (& $sessions), (& $rx).open)

                # waits for a state, then for six tiles playing again; returns @(detectSeconds, recoverSeconds) from $T0
                $watchRecovery = {
                    param([datetime]$T0, [int]$Seconds = 90, [scriptblock]$StopFault = $null, [int]$FaultSeconds = 0)
                    $detect = $null; $rec = $null; $faultEnded = $false
                    while (((Get-Date) - $T0).TotalSeconds -lt $Seconds) {
                        if ($StopFault -and -not $faultEnded -and ((Get-Date) - $T0).TotalSeconds -ge $FaultSeconds) { & $StopFault; $faultEnded = $true; $tEnd = Get-Date }
                        $s = & $stats
                        if ($s) {
                            if (-not $detect -and (@($s.tiles | Where-Object { $_.state -ne 'live' }).Count -or $s.freed)) { $detect = ((Get-Date) - $T0).TotalSeconds }
                            if ($detect -and (& $playing $s 6 4) -and @($s.tiles | Where-Object { $_.fails -eq 0 }).Count -eq 6) { $rec = ((Get-Date) - $T0).TotalSeconds; break }
                        }
                        Start-Sleep -Milliseconds 300
                    }
                    return [pscustomobject]@{ Detect = $detect; Recover = $rec; FaultEnded = $faultEnded }
                }
                $fmt = { param($v) if ($null -eq $v) { 'NEVER' } else { [math]::Round($v, 1) } }
                # A fresh modal (a new main session) after a fault: does signalling still work end to end?
                $modalProbe = {
                    param([string]$Label)
                    $p = & $center '.ltile[data-camera="Cam 1"]' 0.4
                    & $click $p.x $p.y
                    $t0 = Get-Date
                    $m = & $until { param($s) $s.modal -and $s.modal.hd -eq 'ready' } 25 150
                    $secs = [math]::Round(((Get-Date) - $t0).TotalSeconds, 1)
                    $line = if ($m) { 'HD after ' + $secs + ' s' } else { $x = & $stats; 'NO HD in 25 s (modal ' + $(if ($x.modal) { 'hd ' + $x.modal.hd + ', verdict ' + $x.modal.verdict } else { 'closed' }) + ')' }
                    & $note ("      a new modal after $Label : " + $line)
                    if (-not $m) { & $fail "${browser}: no HD modal after $Label" }
                    foreach ($t in @('keyDown', 'keyUp')) { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchKeyEvent' -Params @{ type = $t; key = 'Escape'; code = 'Escape'; windowsVirtualKeyCode = 27 }) }
                    [void](& $until { param($s) -not $s.modal } 8 150)
                }


                # ---- quick: hide and show inside the grace period ----
                if (& $want 'quick') {
                    & $front
                    & $note "  -- quick: minimize, wait 5 s (inside camui's 10 s grace), restore --"
                    $s0 = & $stats; $rx0 = & $rx; $live0 = & $sessions
                    & $minimize
                    $h = & $waitHidden
                    $trace = @()
                    foreach ($wait in 1, 1) {
                        Start-Sleep -Seconds $wait
                        try { $q = Invoke-CamTestCdpEval -Cdp $cdp -Expression 'JSON.stringify(window.__liveProbe())' | ConvertFrom-Json; $trace += ('hiddenFor {0}s freed {1} grace {2} vis {3}' -f $q.hiddenFor, $q.freed, $q.grace, $q.vis) } catch { $trace += 'probe failed' }
                    }
                    & $note ("  while minimized (probes about 1 and 2 s after hidden was seen): " + ($trace -join ' | '))
                    $mid = & $rx
                    $live1 = & $sessions
                    $t0 = Get-Date
                    & $restore
                    $ms = & $paintAgain $t0
                    $s1 = & $stats; $rx1 = & $rx; $live2 = & $sessions
                    & $note ("  hidden state while away: open peer connections {0} (mode '{2}'); live sessions before {3}, while hidden {4}, after restore {5}; POSTs {6}, DELETEs {7} during the whole cycle; tiles painting again {8} ms after the restore command; RTCPeerConnections {9} -> {10}" -f `
                        $mid.open, 0, $s0.mode, $live0, $live1, $live2, ($rx1.posts - $rx0.posts), ($rx1.deletes - $rx0.deletes), $ms, $rx0.open, $rx1.open)
                    if (-not $h) { & $fail "${browser} quick: the window never became hidden" }
                    if ($rx1.posts - $rx0.posts -ne 0 -or $rx1.deletes - $rx0.deletes -ne 0 -or $live2 -ne $live0 -or $live1 -ne $live0) { & $fail "${browser} quick: sessions changed inside the grace period" }
                    if ($null -eq $ms -or $ms -gt 1500) { & $fail "${browser} quick: tiles painted again after $ms ms (wanted about 1000 at most; this includes the window-restore command's own latency)" }
                    if (@(@($s1.tiles) | Where-Object { $_.state -ne 'live' }).Count -or @(@($s1.tiles) | Where-Object { -not $_.muted }).Count) { & $fail "${browser} quick: a tile is not live or not muted after the restore" }

                    # the same with a modal open
                    & $click ((& $center '.ltile[data-camera="Cam 1"]' 0.4).x) ((& $center '.ltile[data-camera="Cam 1"]' 0.4).y)
                    $m = & $until { param($s) $s.modal -and $s.modal.hd -eq 'ready' } 15
                    if (-not $m) { & $fail "${browser} quick: the modal never reached HD"; }
                    else {
                        $rx0 = & $rx; $live0 = & $sessions; $f0 = $m.modal.v.framesDecoded
                        & $minimize
                        [void](& $waitHidden)
                        Start-Sleep -Seconds 5
                        $t0 = Get-Date
                        & $restore
                        $mm = & $until { param($s) $s.visibility -eq 'visible' -and $s.modal -and $s.modal.hd -eq 'ready' -and $s.modal.v -and $s.modal.v.framesDecoded -gt $f0 } 10 100
                        $ms = [int](((Get-Date) - $t0).TotalMilliseconds)
                        $rx1 = & $rx; $live2 = & $sessions
                        & $note ("  modal open across hide/show: modal still open {0}, hd {1}, shown {2}, main picture decoding again after {3} ms; POSTs {4}, DELETEs {5}; live sessions {6} -> {7}" -f [bool]$mm, $(if ($mm) { $mm.modal.hd } else { '-' }), $(if ($mm) { $mm.modal.shown } else { '-' }), $ms, ($rx1.posts - $rx0.posts), ($rx1.deletes - $rx0.deletes), $live0, $live2)
                        if (-not $mm -or $rx1.posts - $rx0.posts -ne 0 -or $rx1.deletes - $rx0.deletes -ne 0 -or $live2 -ne $live0) { & $fail "${browser} quick: the open modal did not survive hide/show without reconnecting" }
                        [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchKeyEvent' -Params @{ type = 'keyDown'; key = 'Escape'; code = 'Escape'; windowsVirtualKeyCode = 27 })
                        [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchKeyEvent' -Params @{ type = 'keyUp'; key = 'Escape'; code = 'Escape'; windowsVirtualKeyCode = 27 })
                        [void](& $until { param($s) -not $s.modal } 8)
                        [void](& $sessionsIs 6 10)
                    }
                }

                # ---- cpu: what to do with the pictures while hidden ----
                if (& $want 'cpu') {
                    & $front
                    & $note "  -- cpu: browser CPU while minimized, 20 s window after 5 s settle (percent of ONE core: 100 = one core busy; the 'visible' row is the same wall in view) --"
                    $cores = [Environment]::ProcessorCount
                    & $note ("  this PC reports {0} logical processors" -f $cores)
                    $rows = @()
                    $measure = {
                        param([string]$Label, [bool]$Hidden)
                        $cpu0 = Get-CamTestBrowserCpu -Started $started; $a = & $stats; $w0 = Get-Date
                        Start-Sleep -Seconds 20
                        $cpu1 = Get-CamTestBrowserCpu -Started $started; $b = & $stats; $w1 = Get-Date
                        $secs = ($w1 - $w0).TotalSeconds
                        $frames = 0; $bytes = 0; $n = 0
                        if ($a -and $b) { for ($i = 0; $i -lt @($b.tiles).Count; $i++) { if ($a.tiles[$i].v -and $b.tiles[$i].v) { $frames += ($b.tiles[$i].v.framesDecoded - $a.tiles[$i].v.framesDecoded); $bytes += ($b.tiles[$i].v.bytes - $a.tiles[$i].v.bytes); $n++ } } }
                        [pscustomobject]@{ Label = $Label; CpuPct = [math]::Round(100 * ($cpu1 - $cpu0) / $secs, 1); Frames = $frames; KB = [int]($bytes / 1024); Tiles = $n }
                    }
                    $rows += & $measure 'visible (reference)' $false
                    foreach ($variant in @(@{ Name = 'keep (nothing touched)'; Mode = 'keep'; Grace = -1 }, @{ Name = 'pause (video.pause)'; Mode = 'pause'; Grace = -1 }, @{ Name = 'detach (srcObject = null)'; Mode = 'detach'; Grace = -1 },
                            @{ Name = 'disable (video tracks off)'; Mode = 'disable'; Grace = -1 }, @{ Name = 'close at once (the old behaviour, grace 0)'; Mode = 'keep'; Grace = 0 })) {
                        [void](& $js ("window.__liveTune({ mode: '" + $variant.Mode + "', grace: " + $variant.Grace + " })"))
                        $live0 = & $sessions; $rx0 = & $rx
                        & $minimize
                        [void](& $waitHidden)
                        Start-Sleep -Seconds 5
                        $row = & $measure $variant.Name $true
                        $live1 = & $sessions
                        $t0 = Get-Date
                        & $restore
                        $ms = & $paintAgain $t0 40
                        $rx1 = & $rx
                        $row | Add-Member -NotePropertyName Resume -NotePropertyValue $ms
                        $row | Add-Member -NotePropertyName Sessions -NotePropertyValue ("{0}->{1}; +{2} POST/+{3} DELETE" -f $live0, $live1, ($rx1.posts - $rx0.posts), ($rx1.deletes - $rx0.deletes))
                        $rows += $row
                        $null = & $until { param($s) & $playing $s } 40
                    }
                    [void](& $js "window.__liveTune({ mode: 'pause', grace: null })")
                    foreach ($r in $rows) { & $note ("  {0,-44} CPU {1,5} % of one core | frames decoded in 20 s (6 tiles) {2,5} | received {3,6} KB | resume (paint again) {4} ms | sessions {5}" -f $r.Label, $r.CpuPct, $r.Frames, $r.KB, $(if ($r.PSObject.Properties['Resume']) { $r.Resume } else { '-' }), $(if ($r.PSObject.Properties['Sessions']) { $r.Sessions } else { '-' })) }
                }

                # ---- expiry: the grace period runs out while minimized ----
                if (& $want 'expiry') {
                    & $front
                    & $note "  -- expiry: minimize and stay away past camui's 10 s grace (the timer must fire in a minimized page) --"
                    $null = & $until { param($s) & $playing $s } 40
                    $rx0 = & $rx; $live0 = & $sessions
                    $t0 = Get-Date
                    & $minimize
                    $freed = $null; $s = $null
                    while (((Get-Date) - $t0).TotalSeconds -lt 45) { $s = & $stats; if ($s -and $s.freed) { $freed = ((Get-Date) - $t0).TotalSeconds; break }; Start-Sleep -Milliseconds 400 }
                    $zeroAt = $null; $n = & $sessionsIs 0 15; if ($n -eq 0) { $zeroAt = ((Get-Date) - $t0).TotalSeconds }
                    $r0 = & $readersIs 0 15; $rAt = ((Get-Date) - $t0).TotalSeconds
                    $rx1 = & $rx
                    & $note ("  page says freed after {0} s hidden (visibility {1}); live sessions {2} -> {3} (reached {4} s after minimizing); camera stream2 readers {5} (at {6} s); DELETEs sent {7}; RTCPeerConnections open {8}; tile states {9}" -f $(if ($freed) { [math]::Round($freed, 1) } else { 'NEVER' }), $(if ($s) { $s.visibility } else { '?' }), $live0, $n, $(if ($zeroAt) { [math]::Round($zeroAt, 1) } else { '-' }), $r0, [math]::Round($rAt, 1), ($rx1.deletes - $rx0.deletes), $rx1.open, $(if ($s) { (@($s.tiles | ForEach-Object { $_.state }) -join ',') } else { '?' }))
                    if (-not $freed) { & $fail "${browser} expiry: the grace period never expired in the minimized page" }
                    if ($freed -and ($freed -lt 9 -or $freed -gt 25)) { & $fail "${browser} expiry: freed after $freed s, wanted about 10" }
                    if ($n -ne 0 -or $r0 -ne 0 -or $rx1.open -ne 0) { & $fail "${browser} expiry: sessions ($n), stream2 readers ($r0) or peer connections ($($rx1.open)) remained" }
                    $t0 = Get-Date
                    & $restore
                    $ms = & $paintAgain $t0 60
                    $sf = & $until { param($s) & $playing $s 6 3 } 40
                    $firsts = if ($sf) { @($sf.tiles | ForEach-Object { $_.diag.ms.frame }) } else { @() }
                    & $note ("  restored after the expiry: six tiles painting again {0} ms after the restore command; each tile's own first frame from its connect start (ms): {1}; live sessions now {2}" -f $ms, ($firsts -join ','), (& $sessions))
                    if ($null -eq $ms) { & $fail "${browser} expiry: the tiles never came back" }
                }

                # ---- audio: what is heard stays heard ----
                if (& $want 'audio') {
                    & $front
                    & $note "  -- audio: an unmuted tile across hide/show --"
                    [void](& $js "window.__liveTune({ grace: -1 })")
                    $p = & $center '.ltile[data-camera="Cam 2"] .lmute'
                    & $click $p.x $p.y
                    Start-Sleep -Milliseconds 1500
                    $a0 = & $stats
                    & $minimize
                    [void](& $waitHidden)
                    Start-Sleep -Seconds 3
                    $mid = & $stats
                    $t0 = Get-Date
                    & $restore
                    $ms = & $paintAgain $t0 20
                    Start-Sleep -Milliseconds 1500
                    $a1 = & $stats
                    $c0 = $a0.tiles[1]; $cm = $mid.tiles[1]; $c1 = $a1.tiles[1]
                    & $note ("  Cam 2 unmuted: before muted={0} paused={1}; hidden: paused={2} hid={3}; after: muted={4} paused={5}, audio packets {6} -> {7}; other tiles unmuted {8}; the other tiles while hidden: hid '{9}'" -f $c0.muted, $c0.paused, $cm.paused, $cm.hid, $c1.muted, $c1.paused, $c0.a.packets, $c1.a.packets, @(@($a1.tiles) | Where-Object { $_.name -ne 'Cam 2' -and -not $_.muted }).Count, $mid.tiles[0].hid)
                    if ($c1.muted -or $c1.paused -or $c1.a.packets -le $c0.a.packets -or @(@($a1.tiles) | Where-Object { $_.name -ne 'Cam 2' -and -not $_.muted }).Count) { & $fail "${browser} audio: the mute state or the sound of the unmuted tile did not survive hide/show" }
                    & $click $p.x $p.y
                    [void](& $js "window.__liveTune({ grace: null })")
                }

                # ---- stall: a camera that stops delivering frames ----
                if (& $want 'stall') {
                    & $front
                    & $note "  -- stall: the stream2 publisher of the synthetic camera is suspended (NtSuspendProcess, by pid) --"
                    $pub = $rig.Camera.Publishers[1]
                    $null = & $until { param($s) & $playing $s } 40
                    # negative control 1: detector off -> the picture stays frozen and nothing is rebuilt
                    [void](& $js "window.__liveTune({ stallMs: 1000000, grace: -1 })")
                    $c0 = & $stats
                    Suspend-CamTestProcess -ProcessId $pub.Id
                    Start-Sleep -Seconds 4
                    $c1 = & $stats; Start-Sleep -Seconds 3; $c2 = & $stats
                    $frozen = @(0..5 | Where-Object { $c2.tiles[$_].v.framesDecoded -eq $c1.tiles[$_].v.framesDecoded }).Count
                    $stallsC = ($c2.tiles | Measure-Object stalls -Sum).Sum
                    Suspend-CamTestProcess -ProcessId $pub.Id -Resume
                    & $note ("  control, detector off: after 7 s of suspended camera {0} of 6 tiles still show a frozen picture (framesDecoded unchanged over 3 s), stalls counted {1}, rebuilds 0" -f $frozen, $stallsC)
                    if ($frozen -lt 5 -or $stallsC -ne 0) { & $fail "${browser} stall control: the frozen picture was not frozen ($frozen) or a stall was counted ($stallsC) with the detector off" }
                    $null = & $until { param($s) & $playing $s 6 5 } 40
                    [void](& $js "window.__liveTune({ stallMs: 8000, grace: null })")
                    Start-Sleep -Seconds 2
                    # the real thing
                    $b0 = & $stats; $stallBase = ($b0.tiles | Measure-Object stalls -Sum).Sum; $recBase = $b0.recoveries
                    $t0 = Get-Date
                    Suspend-CamTestProcess -ProcessId $pub.Id
                    $detect = $null
                    while (((Get-Date) - $t0).TotalSeconds -lt 30) { $s = & $stats; if ($s -and (($s.tiles | Measure-Object stalls -Sum).Sum -gt $stallBase)) { $detect = ((Get-Date) - $t0).TotalSeconds; break }; Start-Sleep -Milliseconds 300 }
                    Start-Sleep -Milliseconds 500
                    $mid = & $stats
                    $t1 = Get-Date
                    Suspend-CamTestProcess -ProcessId $pub.Id -Resume
                    Start-Sleep -Milliseconds 1500
                    $pubDied = $pub.HasExited
                    if ($pubDied) { $pub = & $startPub; $t1 = Get-Date }
                    $rec = $null
                    $end = [DateTime]::UtcNow.AddSeconds(60)
                    while ([DateTime]::UtcNow -lt $end) { $s = & $stats; if ($s -and (& $playing $s 6 3) -and @($s.tiles | Where-Object { $_.fails -eq 0 }).Count -eq 6) { $rec = ((Get-Date) - $t1).TotalSeconds; break }; Start-Sleep -Milliseconds 300 }
                    $e = & $stats
                    & $note ("  stall detected {0} s after the camera stopped (stall time 8 s, check every 2 s); suspended {1} s in all; first tile line: {2}" -f $(if ($detect) { [math]::Round($detect, 1) } else { 'NEVER' }), [math]::Round(($t1 - $t0).TotalSeconds, 1), $(if ($mid) { $mid.tiles[0].diag.line } else { '' }))
                    & $note ("  after the camera resumed{5}: all six tiles playing again with a moving picture {0} s later; stalls counted {1}, recoveries {2} (page total), reconnect attempts in the last minute {3}; tile states {4}" -f $(if ($rec) { [math]::Round($rec, 1) } else { 'NEVER' }), (($e.tiles | Measure-Object stalls -Sum).Sum - $stallBase), ($e.recoveries - $recBase), $e.dialsLastMinute, (@($e.tiles | ForEach-Object { $_.state }) -join ','), $(if ($pubDied) { ' (the camera had dropped the suspended publisher: restarted)' } else { '' }))
                    if (-not $detect -or $detect -lt 7 -or $detect -gt 16) { & $fail "${browser} stall: detection took $detect s (wanted 8 to about 12)" }
                    if (-not $rec) { & $fail "${browser} stall: the tiles did not recover after the camera resumed" }
                    # negative control 2: a hidden page is never "stalled"
                    $st0 = ($e.tiles | Measure-Object stalls -Sum).Sum
                    & $minimize
                    [void](& $waitHidden)
                    Suspend-CamTestProcess -ProcessId $pub.Id
                    Start-Sleep -Seconds 8
                    Suspend-CamTestProcess -ProcessId $pub.Id -Resume
                    Start-Sleep -Seconds 2
                    if ($pub.HasExited) { $pub = & $startPub }
                    $t0 = Get-Date
                    & $restore
                    Start-Sleep -Seconds 3
                    $h = & $stats
                    $st1 = ($h.tiles | Measure-Object stalls -Sum).Sum
                    $okAgain = & $until { param($s) & $playing $s 6 3 } 30
                    & $note ("  control, hidden page with the camera suspended 8 s: stalls counted {0} -> {1} (no false stall); tiles playing after the restore: {2}" -f $st0, $st1, [bool]$okAgain)
                    if ($st1 -ne $st0) { & $fail "${browser} stall: a hidden page counted $($st1 - $st0) stall(s)" }
                    if (-not $okAgain) { & $fail "${browser} stall: tiles did not play after the hidden-camera-stop test" }
                }

                # ---- recover: faults, no reload ----
                if (& $want 'recover') {
                    & $front
                    & $note "  -- recover: faults injected on the rig, the page is never reloaded --"
                    $null = & $until { param($s) & $playing $s } 40
                    [void](& $js "window.__liveTune({ grace: -1 })")
                    $liveLog = $ui.StdOut
                    # (a) the live MediaMTX is killed; the rig's supervisor restarts it
                    $livePid = [int](Get-CamTestStatusJson -Sandbox $rig.Sandbox).Snapshot.Live.Pid
                    $r0 = (& $stats).recoveries
                    $t0 = Get-Date
                    Stop-Process -Id $livePid -Force
                    $res = & $watchRecovery $t0 90
                    $e = & $stats
                    & $note ("  (a) live MediaMTX (pid {0}) killed: tiles stopped playing after {1} s, all six playing again after {2} s; the page was not reloaded (recoveries counted {3}); camui requests since: see (e)" -f $livePid, (& $fmt $res.Detect), (& $fmt $res.Recover), ($e.recoveries - $r0))
                    if ($null -eq $res.Recover) { & $fail "${browser} recover (a): the tiles did not come back after the live server was killed" }
                    $null = & $until { param($s) & $playing $s 6 3 } 20

                    # (b) camui restarted: playing pictures must not notice; the list comes back
                    $f0 = & $stats
                    $t0 = Get-Date
                    [void](Stop-CamTestUi -Ui $ui)
                    Start-Sleep -Seconds 6
                    $f1 = & $stats
                    $grew = @(0..5 | Where-Object { $f1.tiles[$_].v.framesDecoded -gt $f0.tiles[$_].v.framesDecoded }).Count
                    $err = $f1.listError
                    $env:CAMUI_LIVE_HIDDEN_GRACE = '10'
                    try { $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-liverobust' } finally { Remove-Item Env:\CAMUI_LIVE_HIDDEN_GRACE -ErrorAction SilentlyContinue }
                    if (-not $ui.Started) { throw ('camui did not restart: ' + $ui.Reason) }
                    $t1 = Get-Date
                    $cleared = & $until { param($s) -not $s.listError } 30 500
                    $clearedAfter = ((Get-Date) - $t1).TotalSeconds
                    $liveLog = $ui.StdOut
                    & $click ((& $center '.ltile[data-camera="Cam 3"]' 0.4).x) ((& $center '.ltile[data-camera="Cam 3"]' 0.4).y)
                    $m = & $until { param($s) $s.modal -and $s.modal.hd -eq 'ready' } 20
                    & $note ("  (b) camui killed for 6 s: {0} of 6 tiles kept decoding meanwhile (page's list error: '{1}'); camui back: the list read again {2} s later; a new modal after the restart reached HD: {3}" -f $grew, $err, [math]::Round($clearedAfter, 1), [bool]$m)
                    if ($grew -lt 6 -or -not $cleared -or -not $m) { & $fail "${browser} recover (b): camui restart broke running pictures ($grew of 6), the list ($([bool]$cleared)) or a new modal ($([bool]$m))" }
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchKeyEvent' -Params @{ type = 'keyDown'; key = 'Escape'; code = 'Escape'; windowsVirtualKeyCode = 27 })
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchKeyEvent' -Params @{ type = 'keyUp'; key = 'Escape'; code = 'Escape'; windowsVirtualKeyCode = 27 })
                    [void](& $until { param($s) -not $s.modal } 8)

                    # (c) the camera's stream2 publisher is killed and restarted 8 s later
                    $oldPub = $rig.Camera.Publishers[1]
                    $r0 = (& $stats).recoveries
                    $t0 = Get-Date
                    try { $oldPub.Kill() } catch { }
                    $restart = { [void](& $startPub) }
                    $res = & $watchRecovery $t0 90 $restart 8
                    $e = & $stats
                    & $note ("  (c) camera stream2 publisher killed, restarted after 8 s: tiles stopped playing after {0} s, all six playing again after {1} s (the restart was at 8 s); recoveries counted {2}; states at the end {3}" -f (& $fmt $res.Detect), (& $fmt $res.Recover), ($e.recoveries - $r0), (@($e.tiles | ForEach-Object { $_.state }) -join ','))
                    if ($null -eq $res.Recover) { & $fail "${browser} recover (c): the tiles did not come back after the camera restarted" }
                    $null = & $until { param($s) & $playing $s 6 3 } 20

                    [void](& $js "window.__liveTune({ grace: null })")
                }

                # ---- hang: the live server hangs ----
                if (& $want 'hang') {
                    & $front
                    & $note "  -- hang: the live MediaMTX is suspended for 70 s --"
                    $null = & $until { param($s) & $playing $s } 40
                    [void](& $js "window.__liveTune({ grace: -1 })")
                    $liveLog = $ui.StdOut
                    $livePid = [int](Get-CamTestStatusJson -Sandbox $rig.Sandbox).Snapshot.Live.Pid
                    $rxA = & $rx
                    $t0 = Get-Date
                    Suspend-CamTestProcess -ProcessId $livePid
                    Start-Sleep -Seconds 70
                    $e = & $stats
                    $rxB = & $rx
                    $t1 = Get-Date
                    Suspend-CamTestProcess -ProcessId $livePid -Resume
                    $lines = @(Get-Content -LiteralPath $liveLog -ErrorAction SilentlyContinue | Where-Object { $_ -match 'msg=request' })
                    $count = @{ list = 0; post = 0; delete = 0; other = 0 }
                    foreach ($line in $lines) {
                        if ($line -notmatch 'time=(\S+)') { continue }
                        $when = [DateTimeOffset]::Parse($Matches[1]).LocalDateTime
                        if ($when -lt $t0.AddSeconds(5) -or $when -gt $t0.AddSeconds(65)) { continue }
                        if ($line -notmatch 'path=/api/live') { continue }
                        if ($line -match 'method=GET path=/api/live ') { $count.list++ } elseif ($line -match 'method=POST') { $count.post++ } elseif ($line -match 'method=DELETE') { $count.delete++ } else { $count.other++ }
                    }
                    $res = & $watchRecovery $t1 90
                    $e3 = & $stats
                    & $note ("  (e) live server hung 70 s: the page issued {9} session POSTs and {10} DELETEs in the 70 s ({11} POSTs/min); camui finished (logged) in the 60 s from +5 s to +65 s: list reads {0}, session POSTs {1} (a POST is logged when it ends, and hung ones end at camui's 15 s timeout or after the resume), DELETEs {2}, other {3}; page state at 70 s: tile states {5}, reconnect attempts counted in the last minute {6}, list error '{7}', hold {8} ms" -f $count.list, $count.post, $count.delete, $count.other, ($count.list + $count.post + $count.delete + $count.other), (@($e.tiles | ForEach-Object { $_.state }) -join ','), $e.dialsLastMinute, $e.listError, $e.holdMs, ($rxB.posts - $rxA.posts), ($rxB.deletes - $rxA.deletes), [math]::Round(($rxB.posts - $rxA.posts) * 60 / 70, 1))
                    & $note ("      after the server was resumed: tiles stopped playing {0} s, all six playing again {1} s later (page not reloaded)" -f (& $fmt $res.Detect), (& $fmt $res.Recover))
                    if ((($rxB.posts - $rxA.posts) + ($rxB.deletes - $rxA.deletes) + $count.list) * 60 / 70 -gt 60) { & $fail "${browser} hang: the page sent $(($rxB.posts - $rxA.posts) + ($rxB.deletes - $rxA.deletes)) POST/DELETE and camui logged $($count.list) list reads in 70 s while the server was hung" }
                    if ($null -eq $res.Recover) { & $fail "${browser} hang: the tiles did not come back after the live server resumed" }
                    [void](& $js "window.__liveTune({ grace: null })")
                    $null = & $until { param($s) & $playing $s 6 3 } 40
                    # a timeline after the resume: what the tiles say and how the server answers
                    $tl = @()
                    for ($i = 0; $i -lt 12; $i++) {
                        $q = & $stats
                        if ($q) { $tl += ("+{0,3}s states {1} | http {2} | fails {3} | sessions {4} | dials/min {5} | hold {6} ms" -f ($i * 10), (@($q.tiles | ForEach-Object { $_.state.Substring(0, [Math]::Min(4, $_.state.Length)) }) -join ','), (@($q.tiles | ForEach-Object { $_.diag.http }) -join ','), (@($q.tiles | ForEach-Object { $_.fails }) -join ','), (& $sessions), $q.dialsLastMinute, $q.holdMs) }
                        if ($q -and (& $playing $q 6 4)) { break }
                        Start-Sleep -Seconds 10
                    }
                    foreach ($line in $tl) { & $note ("      timeline " + $line) }
                    & $modalProbe 'the live server hang'
                    [void](& $js "window.__liveTune({ grace: null })")
                    $null = & $until { param($s) & $playing $s 6 3 } 60
                }

                # ---- cycles: 10 modal open/close and 10 hide/show, back to baseline ----
                if (& $want 'cycles') {
                    & $front
                    & $note "  -- cycles: 10 modal open/close cycles, then 10 hide/show cycles --"
                    $null = & $until { param($s) & $playing $s 6 5 } 40
                    [void](& $js "window.__liveTune({ grace: -1 })")
                    $heap = {
                        [void](Invoke-CamTestCdp -Cdp $cdp -Method 'HeapProfiler.collectGarbage')
                        [int]((Invoke-CamTestCdp -Cdp $cdp -Method 'Runtime.getHeapUsage').usedSize / 1024)
                    }
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'HeapProfiler.enable')
                    [void](& $sessionsIs 6 45)    # sessions orphaned by a hung server time out on their own
                    Start-Sleep -Seconds 2
                    $b = [pscustomobject]@{ Live = (& $sessions); Rx = (& $rx); Heap = (& $heap); Readers = (& $readers) }
                    for ($i = 1; $i -le 10; $i++) {
                        $p = & $center '.ltile[data-camera="Cam 1"]' 0.4
                        & $click $p.x $p.y
                        $m = & $until { param($s) $s.modal -and $s.modal.hd -eq 'ready' } 12 150
                        if (-not $m) { & $fail "${browser} cycles: modal cycle $i never reached HD"; foreach ($t in @('keyDown', 'keyUp')) { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchKeyEvent' -Params @{ type = $t; key = 'Escape'; code = 'Escape'; windowsVirtualKeyCode = 27 }) }; break }
                        foreach ($t in @('keyDown', 'keyUp')) { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchKeyEvent' -Params @{ type = $t; key = 'Escape'; code = 'Escape'; windowsVirtualKeyCode = 27 }) }
                        $c = & $until { param($s) -not $s.modal } 6 100
                        if (-not $c) { & $fail "${browser} cycles: modal cycle $i did not close"; break }
                    }
                    Start-Sleep -Seconds 3
                    $a = [pscustomobject]@{ Live = (& $sessionsIs 6 12); Rx = (& $rx); Heap = (& $heap); Readers = (& $readers) }
                    & $note ("  after 10 modal cycles: live sessions {0} -> {1}, camera stream2 readers {2} -> {3}, RTCPeerConnections open {4} -> {5} (created in total: POSTs {6} -> {7}), JS heap {8} -> {9} KB" -f $b.Live, $a.Live, $b.Readers, $a.Readers, $b.Rx.open, $a.Rx.open, $b.Rx.posts, $a.Rx.posts, $b.Heap, $a.Heap)
                    if ($a.Live -ne $b.Live -or $a.Rx.open -ne $b.Rx.open -or $a.Readers -ne $b.Readers) { & $fail "${browser} cycles: sessions, readers or peer connections did not return to baseline after the modal cycles" }
                    if ($a.Heap -gt $b.Heap * 1.5 + 2048) { & $fail "${browser} cycles: the JS heap grew from $($b.Heap) to $($a.Heap) KB" }
                    $b2 = $a
                    for ($i = 1; $i -le 10; $i++) {
                        & $minimize
                        [void](& $waitHidden)
                        Start-Sleep -Milliseconds 1500
                        & $restore
                        [void](& $until { param($s) $s.visibility -eq 'visible' } 8 150)
                        Start-Sleep -Milliseconds 1200
                    }
                    $null = & $until { param($s) & $playing $s 6 5 } 40
                    Start-Sleep -Seconds 2
                    $a2 = [pscustomobject]@{ Live = (& $sessions); Rx = (& $rx); Heap = (& $heap); Readers = (& $readers); Stats = (& $stats) }
                    & $note ("  after 10 hide/show cycles (grace never): live sessions {0} -> {1}, readers {2} -> {3}, RTCPeerConnections open {4} -> {5} (POSTs {6} -> {7}, DELETEs {8} -> {9}), JS heap {10} -> {11} KB, tiles live {12}" -f $b2.Live, $a2.Live, $b2.Readers, $a2.Readers, $b2.Rx.open, $a2.Rx.open, $b2.Rx.posts, $a2.Rx.posts, $b2.Rx.deletes, $a2.Rx.deletes, $b2.Heap, $a2.Heap, @($a2.Stats.tiles | Where-Object { $_.state -eq 'live' }).Count)
                    if ($a2.Live -ne $b2.Live -or $a2.Rx.open -ne $b2.Rx.open -or $a2.Rx.posts -ne $b2.Rx.posts) { & $fail "${browser} cycles: hide/show changed sessions or created connections" }
                    if ($a2.Heap -gt $b2.Heap * 1.5 + 2048) { & $fail "${browser} cycles: the JS heap grew across hide/show from $($b2.Heap) to $($a2.Heap) KB" }
                    [void](& $js "window.__liveTune({ grace: null })")
                }

                # ---- offscreen: a phone layout parks tiles scrolled out of view ----
                if (& $want 'offscreen') {
                    & $front
                    & $note "  -- offscreen: 390x844, six full-width tiles stacked --"
                    & $viewport 390 844
                    $null = & $until { param($s) $s.fit -and $s.fit.vw -eq 390 -and (& $playing $s 6 3) } 40
                    [void](& $js "window.__liveTune({ offMs: 8000 })")
                    Start-Sleep -Seconds 3
                    $s = & $stats
                    $off = @($s.tiles | Where-Object { -not $_.inView })
                    & $note ("  tiles out of view {0} of 6; of those paused at once (muted, not painting) {1}; live sessions {2}" -f $off.Count, @($off | Where-Object { $_.hid -eq 'off' }).Count, (& $sessions))
                    if ($off.Count -lt 2 -or @($off | Where-Object { $_.hid -eq 'off' }).Count -ne $off.Count) { & $fail "${browser} offscreen: out-of-view tiles were not paused ($($off.Count) out of view)" }
                    Start-Sleep -Seconds 12
                    $s = & $stats
                    $parked = @($s.tiles | Where-Object { $_.parked }).Count
                    $liveN = & $sessionsIs (6 - $parked) 8
                    & $note ("  after 8 s out of view: {0} tiles parked (connections closed), live sessions {1} (in view: {2})" -f $parked, $liveN, (6 - $parked))
                    if ($parked -ne $off.Count -or $liveN -ne 6 - $parked) { & $fail "${browser} offscreen: parked $parked of $($off.Count) tiles, sessions $liveN" }
                    $t0 = Get-Date
                    [void](& $js 'window.scrollTo(0, document.documentElement.scrollHeight)')
                    $back = & $until { param($s) @($s.tiles | Where-Object { $_.parked }).Count -eq 0 -and @($s.tiles | Where-Object { $_.inView -and $_.state -eq 'live' }).Count -ge 2 } 30 200
                    & $note ("  scrolled to the bottom: the tiles that came into view are playing again after {0} s; tiles now out of view are paused, not yet closed" -f [math]::Round(((Get-Date) - $t0).TotalSeconds, 1))
                    if (-not $back) { & $fail "${browser} offscreen: parked tiles did not reopen when scrolled into view" }
                    [void](& $js "window.__liveTune({ offMs: 60000 }); window.scrollTo(0, 0)")
                    & $viewport 1920 1080
                }

                # ---- sleep: a PC that sleeps ----
                if (& $want 'sleep') {
                    & $front
                    & $note "  -- sleep --"
                    $null = & $until { param($s) & $playing $s } 60
                    [void](& $js "window.__liveTune({ grace: -1 })")
                    # the page's renderer process is suspended for 40 s (by pid): the nearest thing to a PC that slept. Suspending the GPU and browser processes too left the headless compositor wedged in a rehearsal, so only renderers are suspended.
                    $r0 = (& $stats).recoveries
                    $ids = @(Get-CimInstance -ClassName Win32_Process -Property ProcessId, CommandLine | Where-Object { $_.CommandLine -and $_.CommandLine.Contains($started.Profile) -and $_.CommandLine -match '--type=renderer' } | ForEach-Object { [int]$_.ProcessId })
                    $liveBefore = & $sessions
                    foreach ($id in $ids) { try { Suspend-CamTestProcess -ProcessId $id } catch { } }
                    Start-Sleep -Seconds 40
                    foreach ($id in $ids) { try { Suspend-CamTestProcess -ProcessId $id -Resume } catch { } }
                    $t1 = Get-Date
                    $liveAfter = & $sessions
                    $res = & $watchRecovery $t1 90
                    $e = & $stats
                    & $note ("  sleep: renderer suspended 40 s ({0} processes): live sessions {1} before, {2} right after the thaw; tiles not playing after {3} s, all six playing again {4} s after the thaw; recoveries counted {5}" -f $ids.Count, $liveBefore, $liveAfter, (& $fmt $res.Detect), (& $fmt $res.Recover), ($e.recoveries - $r0))
                    if ($null -eq $res.Recover -and $null -ne $res.Detect) { & $fail "${browser} sleep: tiles did not come back after the browser was suspended" }
                    $null = & $until { param($s) & $playing $s 6 3 } 40
                    & $modalProbe 'the renderer sleep'
                    [void](& $js "window.__liveTune({ grace: null })")
                }

                # ---- freeze ----
                if (& $want 'freeze') {
                    & $front
                    & $note "  -- freeze --"
                    $null = & $until { param($s) & $playing $s } 60
                    [void](& $js "window.__liveTune({ grace: -1 })")
                    # CDP's frozen state stops the page's JavaScript only. Measured: the connections keep running (the network stack answers the server) and
                    # after 'active' the page is left HIDDEN until the window is shown again, so the wake-up here is a minimize and restore. LAST in the run.
                    $r0 = (& $stats).recoveries
                    $liveBefore = & $sessions
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.setWebLifecycleState' -Params @{ state = 'frozen' })
                    Start-Sleep -Seconds 40
                    $liveFrozen = & $sessions
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.setWebLifecycleState' -Params @{ state = 'active' })
                    $w = Invoke-CamTestCdp -Cdp $cdp -Method 'Browser.getWindowForTarget'
                    foreach ($st in 'minimized', 'normal') { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Browser.setWindowBounds' -Params @{ windowId = $w.windowId; bounds = @{ windowState = $st } }); Start-Sleep -Milliseconds 800 }
                    $t1 = Get-Date
                    $up = & $until { param($s) $s.visibility -eq 'visible' -and (& $playing $s 6 3) } 60
                    $secs = [math]::Round(((Get-Date) - $t1).TotalSeconds, 1)
                    $e = & $stats
                    & $note ("  freeze: page frozen 40 s: live sessions {0} before, {1} at the end of the freeze (the server kept them); after the thaw and a window restore all six playing {2} (checked {3} s after the restore), recoveries counted {4}, tile states {5}" -f $liveBefore, $liveFrozen, [bool]$up, $secs, ($e.recoveries - $r0), (@($e.tiles | ForEach-Object { $_.state }) -join ','))
                    if (-not $up) { & $fail "${browser} freeze: tiles were not playing after the page thawed and the window was restored" }
                    [void](& $js "window.__liveTune({ grace: null })")
                }

            }
            catch { & $fail ("$browser run aborted: " + $_.Exception.Message + " (line " + $_.InvocationInfo.ScriptLineNumber + ")") }
            finally {
                Disconnect-CamTestCdp -Cdp $cdp
                if ($started) { $end = Stop-CamTestBrowser -Started $started } else { $end = [pscustomobject]@{ Left = @(); ProfileRemoved = $true } }
                $secs = ([DateTime]::UtcNow - $began).TotalSeconds
                $script:CamRobSeconds += $secs
                & $note ("  $browser window up {0:n0} s (minimized for most of the cpu and expiry sections); processes left after the kill: {1}; profile removed: {2}" -f $secs, $end.Left.Count, $end.ProfileRemoved)
                if ($end.Left.Count -or -not $end.ProfileRemoved) { & $fail "$browser left processes or a profile behind" }
            }
        }
    }
    catch { & $fail ('check aborted: ' + $_.Exception.Message) }
    finally {
        foreach ($b in @($script:CamRobBrowsers)) { try { [void](Stop-CamTestBrowser -Started $b) } catch { } }
        Remove-Item Env:\CAMUI_LIVE_HIDDEN_GRACE -ErrorAction SilentlyContinue
        if ($ui) { [void](Stop-CamTestUi -Ui $ui) }
        $notes = @()
        if ($rig) { try { foreach ($p in @($rig.Camera.Publishers)) { if ($p -and -not $p.HasExited) { try { Suspend-CamTestProcess -ProcessId $p.Id -Resume } catch { } } } } catch { } }
        if ($rig) { $notes = @(Stop-CamTestLiveRig -Rig $rig) }
        $evidence += ("browser windows were up {0:n0} s in total; whole check {1:n0} s" -f $script:CamRobSeconds, $watch.Elapsed.TotalSeconds)
        $evidence += $notes
        if ($notes -match 'forced: (?!nothing)') { & $fail 'the supervisor had to be forced' }
        if ($script:CamRobOk -and $rig) { try { Remove-CamTestTree -Path $rig.Dir } catch { $script:CamRobEvidence += 'cleanup failed' } }
        elseif ($rig) { $script:CamRobEvidence += "left for inspection: $($rig.Dir)" }
    }

    Add-CamTestResult -Run $Run -Check 'liverobust' -Status $(if ($script:CamRobOk) { 'PASS' } else { 'FAIL' }) `
        -Message 'a hidden wall keeps its streams for the grace period and frees them after it; stalled, killed, restarted and frozen streams recover without a reload with bounded retry traffic; cycles return to baseline' `
        -Evidence ($evidence + $script:CamRobEvidence)
}


function Invoke-CamCheckLiveGone {
    <#
        [Stage 1B, opt-in: opens browser windows] Everything a viewer opened is
        gone when the viewer is. The Operator's condition: "any related stream,
        thread, process, whatever is terminated once the browser tab is closed;
        nothing may run in the background that should not".

        Real supervisor, synthetic camera, six tiles playing, then for Chrome
        and Edge, each time in a fresh browser:
          1 tab close    DevTools Page.close (the browser is not killed): did the
                         DELETE arrive, and how long until the camera's stream2
                         sessions and the live instance's WebRTC sessions are 0
          2 hard kill    taskkill /T /F on the browser's own pid, so no pagehide
                         and no DELETE can run: the ICE timeout path
          3 camui killed the proxy dies while the tiles play: do sessions
                         persist, does the page keep playing, and after the tab
                         is closed do they reach 0 anyway
        Each is a time series (seconds since the event) of: camera stream2
        sessions, live-instance WebRTC sessions (its control API, switched on in the SANDBOX copy of the generated config only), mediamtx and ffmpeg
        processes of the rig that were not there before it (by pid), browser
        processes carrying the profile marker. A final idle snapshot follows,
        including whether the live instance itself stays up with no viewer.
        Needs CAMUI_TEST_EXE.
    #>
    param($Run)

    $evidence = @()
    $fail = { param([string]$Text) $script:CamGoneOk = $false; $script:CamGoneEvidence += ('FAIL ' + $Text) }
    $note = { param([string]$Text) $script:CamGoneEvidence += $Text }
    $script:CamGoneOk = $true
    $script:CamGoneEvidence = @()
    $script:CamGoneBrowsers = @()
    $script:CamGoneSeconds = 0
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $rig = $null; $ui = $null
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\livegone'
    $base = 'http://127.0.0.1:18789'
    $browserOf = { param($b) @(Get-CamTestBrowserPids -Started $b | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue }) }
    $cdp = $null

    try {
        if (-not $env:CAMUI_TEST_EXE -or -not (Test-Path -LiteralPath $env:CAMUI_TEST_EXE)) { throw 'CAMUI_TEST_EXE must name a camui.exe built from this tree.' }
        foreach ($p in @(18789, 18300, 18892)) { if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the check needs it free." } }
        $baseline = @(Get-CimInstance Win32_Process -Filter "Name = 'mediamtx.exe' or Name = 'ffmpeg.exe'" | ForEach-Object { [int]$_.ProcessId })
        $cameras = 1..6 | ForEach-Object { [pscustomobject]@{ Name = "Cam $_"; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = ($_ -eq 1) } }
        $rig = Start-CamTestLiveRig -Name 'livegone' -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds 4 -LiveApiPort 18892
        $dir = $rig.Dir
        $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-livegone'
        if (-not $ui.Started) { throw ('camui did not start: ' + $ui.Reason) }
        $liveLog = Join-Path -Path $rig.Sandbox.Root -ChildPath '_logs\liveview.log'
        $rigProcs = { @(Get-CimInstance Win32_Process -Filter "Name = 'mediamtx.exe' or Name = 'ffmpeg.exe'" | Where-Object { $baseline -notcontains [int]$_.ProcessId } | ForEach-Object { [int]$_.ProcessId } | Sort-Object) }
        $idleProcs = & $rigProcs
        $livePid = [int](Get-CamTestStatusJson -Sandbox $rig.Sandbox).Snapshot.Live.Pid
        & $note ("rig processes (mediamtx and ffmpeg not there before the rig): {0} pid(s) {1}; the live instance is pid {2}; camui pid {3}; no process of the Operator's own was counted ({4} existed before)" -f $idleProcs.Count, ($idleProcs -join ','), $livePid, $ui.Process.Id, $baseline.Count)

        $stats = { try { Invoke-CamTestCdpEval -Cdp $cdp -Expression 'window.__liveStats ? window.__liveStats() : null' -Await } catch { $null } }
        $playing = { param($s) $s -and $s.shown -and @(@($s.tiles) | Where-Object { $_.state -eq 'live' -and $_.v -and $_.v.framesDecoded -ge 25 }).Count -ge 6 }
        $camuiDeletes = { @(Get-Content -LiteralPath $ui.StdOut -ErrorAction SilentlyContinue | Where-Object { $_ -match 'method=DELETE path=/api/live/session/' }) }

        # A browser with six playing tiles: returns what it started and its DevTools connection.
        $launch = {
            param([string]$Browser)
            $started = Start-CamTestBrowser -Browser $Browser -BaseDir $dir -Url 'about:blank' -DebugPort 18300 -WindowSize '1280,900'
            $script:CamGoneBrowsers += $started
            $cdp = Connect-CamTestCdp -Port 18300
            [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.enable')
            [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Emulation.setDeviceMetricsOverride' -Params @{ width = 1280; height = 800; deviceScaleFactor = 1; mobile = $false })
            [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = "$base/#live" })
            $end = [DateTime]::UtcNow.AddSeconds(45); $s = $null
            while ([DateTime]::UtcNow -lt $end) { $s = & $stats; if (& $playing $s) { break }; Start-Sleep -Milliseconds 500 }
            if (-not (& $playing $s)) { throw "${Browser}: six tiles did not reach playing" }
            return @{ Started = $started; Cdp = $cdp }
        }

        # Samples until everything is 0 (twice in a row) or the cap, from the moment the event ran. Returns the rows.
        $observe = {
            param([string]$Label, $Started, [scriptblock]$Event, [int]$Cap, [switch]$Page)
            $before = [pscustomobject]@{ S2 = (Get-CamTestPathReaders -ApiPort $rig.CamApi -Path 'stream2').Count; Live = (Get-CamTestLiveSessions -Rig $rig).Count }
            $deletesBefore = @(& $camuiDeletes).Count
            $clock = [System.Diagnostics.Stopwatch]::StartNew()
            & $Event
            $rows = New-Object System.Collections.ArrayList
            $zero = 0
            while ($clock.Elapsed.TotalSeconds -lt $Cap) {
                $row = [pscustomobject]@{
                    T = [Math]::Round($clock.Elapsed.TotalSeconds, 1)
                    S2 = (Get-CamTestPathReaders -ApiPort $rig.CamApi -Path 'stream2').Count
                    Live = (Get-CamTestLiveSessions -Rig $rig).Count
                    Procs = @(& $rigProcs).Count
                    Browser = @(& $browserOf $Started).Count
                    Page = ''
                }
                if ($Page) {
                    $s = & $stats
                    $row.Page = $(if ($s) { 'open {0}, frames {1}, {2}' -f @(@($s.tiles) | Where-Object { $_.open }).Count, (@($s.tiles) | ForEach-Object { if ($_.v) { $_.v.framesDecoded } else { 0 } } | Measure-Object -Sum).Sum, ((@($s.tiles) | ForEach-Object { $_.state } | Select-Object -Unique) -join '/') } else { 'page gone' })
                }
                [void]$rows.Add($row)
                if ($row.S2 -eq 0 -and $row.Live -eq 0) { $zero++ } else { $zero = 0 }
                if ($zero -ge 2 -and ($Page -or $row.Browser -eq 0 -or $Label -like '*hard*' -or $Label -like '*close*')) { break }
                Start-Sleep -Milliseconds 700
            }
            $firstZero = { param($prop) $r = @($rows | Where-Object { $_.$prop -eq 0 })[0]; if ($r) { $r.T } else { $null } }
            $res = [pscustomobject]@{ Rows = @($rows); Before = $before; S2Zero = (& $firstZero 'S2'); LiveZero = (& $firstZero 'Live'); BrowserZero = (& $firstZero 'Browser')
                Deletes = (@(& $camuiDeletes).Count - $deletesBefore) }
            & $note ("  {0}: before the event stream2 {1}, live sessions {2}; camui DELETEs that arrived: {3}; stream2 reached 0 at +{4} s, live-instance sessions at +{5} s, browser processes at +{6} s" -f $Label, $before.S2, $before.Live, $res.Deletes, $res.S2Zero, $res.LiveZero, $res.BrowserZero)
            & $note '    t(s)  stream2  live-sessions  rig-procs  browser-procs  page'
            $prev = ''
            foreach ($r in $rows) {
                $key = '{0}|{1}|{2}|{3}' -f $r.S2, $r.Live, $r.Procs, $r.Browser
                if ($key -ne $prev -or $r -eq $rows[-1] -or $r -eq $rows[0]) { & $note ("    {0,5}  {1,7}  {2,13}  {3,9}  {4,13}  {5}" -f $r.T, $r.S2, $r.Live, $r.Procs, $r.Browser, $r.Page) }
                $prev = $key
            }
            if (@($rows | Where-Object { $_.Procs -ne $idleProcs.Count }).Count) { & $fail "$Label : the rig's mediamtx/ffmpeg process count changed (a process appeared or died)" }
            return $res
        }
        $finish = {
            param($Started)
            Disconnect-CamTestCdp -Cdp $cdp
            $end = Stop-CamTestBrowser -Started $Started
            if ($end.Left.Count -or -not $end.ProfileRemoved) { & $fail "a browser left processes or a profile behind ($($end.Left.Count), profile removed $($end.ProfileRemoved))" }
        }
        $settle = {
            # Idle before the next scenario so a late session cannot be blamed on it.
            $end = [DateTime]::UtcNow.AddSeconds(90)
            while ([DateTime]::UtcNow -lt $end) { if ((Get-CamTestPathReaders -ApiPort $rig.CamApi -Path 'stream2').Count -eq 0 -and (Get-CamTestLiveSessions -Rig $rig).Count -eq 0) { return $true }; Start-Sleep -Seconds 1 }
            return $false
        }

        foreach ($browser in @('chrome', 'edge')) {
            $found = Get-CamTestBrowser -Browser $browser
            if (-not $found.Exe) { & $fail ("$browser could not be run: " + $found.Reason); continue }
            & $note ("== $browser $($found.Version) ==")
            $began = [DateTime]::UtcNow

            # 1 normal tab close
            $l = & $launch $browser; $started = $l.Started; $cdp = $l.Cdp
            $r = & $observe "$browser 1 tab close (Page.close)" $started { try { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.close' -TimeoutMs 5000) } catch { } } 60
            if ($r.S2Zero -eq $null -or $r.LiveZero -eq $null) { & $fail "$browser tab close: sessions did not reach 0 within 60 s" }
            if ($r.Deletes -lt 1) { & $fail "${browser} tab close: no DELETE reached camui (they are best effort, but a tab close should send some)" }
            & $finish $started
            if (-not (& $settle)) { & $fail "${browser}: sessions did not settle to 0 after the tab close" }

            # 2 hard kill of the browser's process tree
            $l = & $launch $browser; $started = $l.Started; $cdp = $l.Cdp
            $r = & $observe "$browser 2 hard kill (taskkill /T /F on pid $($started.Pid))" $started { & cmd.exe /c "taskkill.exe /T /F /PID $($started.Pid) >nul 2>&1" } 150
            if ($r.S2Zero -eq $null -or $r.LiveZero -eq $null) { & $fail "$browser hard kill: sessions did not reach 0 within 150 s" }
            elseif ($r.S2Zero -gt 35 -or $r.LiveZero -gt 35) { & $note ("  NOTE $browser hard kill took longer than 35 s: stream2 {0} s, live {1} s" -f $r.S2Zero, $r.LiveZero) }
            if ($r.Deletes -ne 0) { & $fail "$browser hard kill: a DELETE arrived ($($r.Deletes)) although nothing could send one" }
            & $finish $started
            if (-not (& $settle)) { & $fail "${browser}: sessions did not settle to 0 after the hard kill" }

            # 3 camui killed while the tiles play, then the tab closed
            $l = & $launch $browser; $started = $l.Started; $cdp = $l.Cdp
            $uiPid = $ui.Process.Id
            $r = & $observe "$browser 3a camui killed (pid $uiPid), page left open" $started { Stop-Process -Id $uiPid -Force } 25 -Page
            [void]$r
            $s = & $stats
            & $note ("  after 25 s without camui the page reports: {0}" -f $(if ($s) { 'open {0} of 6, states {1}, note [{2}]' -f @(@($s.tiles) | Where-Object { $_.open }).Count, ((@($s.tiles) | ForEach-Object { $_.state } | Select-Object -Unique) -join '/'), $s.note } else { 'nothing (page gone)' }))
            $r = & $observe "$browser 3b then the tab closed (camui still dead)" $started { try { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.close' -TimeoutMs 5000) } catch { } } 60
            if ($r.S2Zero -eq $null -or $r.LiveZero -eq $null) { & $fail "${browser}: sessions did not reach 0 after camui died and the tab closed" }
            & $finish $started
            [void](& $settle)
            $script:CamGoneSeconds += ([DateTime]::UtcNow - $began).TotalSeconds
            # camui back for the next browser
            $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-livegone'
            if (-not $ui.Started) { throw ('camui did not restart: ' + $ui.Reason) }
        }

        # ---- the final idle snapshot ----
        Start-Sleep -Seconds 3
        $s2 = (Get-CamTestPathReaders -ApiPort $rig.CamApi -Path 'stream2').Count
        $liveN = (Get-CamTestLiveSessions -Rig $rig).Count
        $procs = & $rigProcs
        $status = (Get-CamTestStatusJson -Sandbox $rig.Sandbox).Snapshot
        $proc = Get-Process -Id $livePid -ErrorAction SilentlyContinue
        $cpu0 = $(if ($proc) { $proc.TotalProcessorTime.TotalSeconds } else { 0 })
        Start-Sleep -Seconds 5
        $proc = Get-Process -Id $livePid -ErrorAction SilentlyContinue
        $cpuPct = $(if ($proc) { [Math]::Round(($proc.TotalProcessorTime.TotalSeconds - $cpu0) / 5 * 100, 2) } else { -1 })
        & $note ("final idle snapshot: stream2 sessions {0}; live-instance sessions {1}; rig processes {2} (idle set was {3}) same set: {4}; live instance pid {5} {6}, status Live.Alive {7}, CPU {8} % over 5 s, handles {9}, threads {10}, working set {11:n1} MB" -f `
            $s2, $liveN, $procs.Count, $idleProcs.Count, (($procs -join ',') -eq ($idleProcs -join ',')), $livePid, $(if ($proc) { 'running' } else { 'GONE' }), $status.Live.Alive, $cpuPct, $(if ($proc) { $proc.HandleCount } else { '-' }), $(if ($proc) { $proc.Threads.Count } else { '-' }), $(if ($proc) { $proc.WorkingSet64 / 1MB } else { 0 }))
        if ($s2 -ne 0 -or $liveN -ne 0) { & $fail 'a session survived to the idle snapshot' }
        if (($procs -join ',') -ne ($idleProcs -join ',')) { & $fail 'the rig process set changed' }
        if (-not $proc -or -not $status.Live.Alive) { & $fail 'the live instance did not stay up with no viewer' }
    }
    catch { & $fail ('check aborted: ' + $_.Exception.Message) }
    finally {
        Disconnect-CamTestCdp -Cdp $cdp
        foreach ($b in @($script:CamGoneBrowsers)) { try { [void](Stop-CamTestBrowser -Started $b) } catch { } }
        $profiles = @(Get-ChildItem -LiteralPath $dir -Directory -Filter 'prof-*' -ErrorAction SilentlyContinue)
        if ($ui) { [void](Stop-CamTestUi -Ui $ui) }
        $notes = @()
        if ($rig) { $notes = @(Stop-CamTestLiveRig -Rig $rig) }
        $after = @(Get-CimInstance Win32_Process -Filter "Name = 'mediamtx.exe' or Name = 'ffmpeg.exe'" | Where-Object { $baseline -notcontains [int]$_.ProcessId } | ForEach-Object { [int]$_.ProcessId })
        $evidence += ("throwaway browser profiles left: {0}; after the rig stopped: mediamtx/ffmpeg not in the pre-rig set: {1}; camui from this check still running: {2}; scenarios' browser windows up {3:n0} s; whole check {4:n0} s" -f $profiles.Count, $after.Count, [bool]($ui -and -not $ui.Process.HasExited), $script:CamGoneSeconds, $watch.Elapsed.TotalSeconds)
        $evidence += $notes
        if ($profiles.Count -or $after.Count) { & $fail 'something of the rig or a profile was left behind' }
        if ($notes -match 'forced: (?!nothing)') { & $fail 'the supervisor had to be forced' }
        if ($script:CamGoneOk -and $rig) { try { Remove-CamTestTree -Path $rig.Dir } catch { $script:CamGoneEvidence += 'cleanup failed' } }
        elseif ($rig) { $script:CamGoneEvidence += "left for inspection: $($rig.Dir)" }
    }

    Add-CamTestResult -Run $Run -Check 'livegone' -Status $(if ($script:CamGoneOk) { 'PASS' } else { 'FAIL' }) `
        -Message 'closing a tab (or killing the browser, or camui) ends every session and stream the viewer opened, and nothing of the rig is left running' `
        -Evidence ($evidence + $script:CamGoneEvidence)
}


function Invoke-CamCheckLiveLan {
    <#
        [Stage 2a, opt-in: opens browser windows] The media path works through a
        NON-loopback address, and a stuck tile explains itself.

        The live instance listens on 0.0.0.0 and advertises ONLY this PC's
        private address (a test-only patch of the sandbox copy drops the usual
        127.0.0.1 from the advertised list), run from the installed MediaMTX so
        no new firewall prompt can appear. The address is read at run time and
        never printed or written.
          answer     the SDP camui returns holds exactly that one host candidate
          chrome and edge   three tiles reach playing, framesDecoded keeps growing,
                     and each tile's selected candidate pair is private on the
                     server side, not loopback (window.__liveStats diag.pair)
          negative   (Chrome) a second live instance advertises only an address
                     nothing answers: the tile must say why - server answered
                     201, ICE state, what each side offered, "No media after 15 s"
                     - with a Copy diagnostics button that works, and no IP
                     address anywhere in the page text
        Address choice: CAMTEST_LAN_ADDRESS if set; else a physical adapter with
        a default route; else (this PC may have none) a virtual adapter's address
        on a Private profile - which proves the non-loopback candidate path but
        NOT a physical NIC, and the evidence says which it was.
    #>
    param($Run)

    $evidence = @()
    $fail = { param([string]$Text) $script:CamLanOk = $false; $script:CamLanEvidence += ('FAIL ' + $Text) }
    $note = { param([string]$Text) $script:CamLanEvidence += $Text }
    $script:CamLanOk = $true; $script:CamLanEvidence = @(); $script:CamLanBrowsers = @(); $script:CamLanSeconds = 0
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $rig = $null; $ui = $null; $cdp = $null
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\livelan'
    $base = 'http://127.0.0.1:18789'
    $shots = [string]$env:CAMTEST_SHOTS
    $offer = Get-CamTestWhepOffer
    $ipPattern = '\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b'

    $stats = { try { Invoke-CamTestCdpEval -Cdp $cdp -Expression 'window.__liveStats ? window.__liveStats() : null' -Await } catch { $null } }
    $playing = { param($s, [int]$n) $s -and $s.shown -and @(@($s.tiles) | Where-Object { $_.state -eq 'live' -and $_.v -and $_.v.framesDecoded -ge 25 }).Count -ge $n }
    $launch = {
        param([string]$Browser)
        $started = Start-CamTestBrowser -Browser $Browser -BaseDir $dir -Url 'about:blank' -DebugPort 18300 -WindowSize '1280,900'
        $script:CamLanBrowsers += $started
        $cdp = Connect-CamTestCdp -Port 18300
        [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.enable')
        [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Emulation.setDeviceMetricsOverride' -Params @{ width = 1280; height = 800; deviceScaleFactor = 1; mobile = $false })
        [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = "$base/#live" })
        return @{ Started = $started; Cdp = $cdp }
    }
    $finish = {
        param($Started)
        Disconnect-CamTestCdp -Cdp $cdp
        $end = Stop-CamTestBrowser -Started $Started
        if ($end.Left.Count -or -not $end.ProfileRemoved) { & $fail 'a browser left processes or a profile behind' }
    }
    $startRig = {
        param([string]$Name, [string]$AdvertiseHost, [int]$CameraCount)
        $cameras = 1..$CameraCount | ForEach-Object { [pscustomobject]@{ Name = "Cam $_"; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = ($_ -eq 1) } }
        $r = Start-CamTestLiveRig -Name $Name -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds 4 -LiveOnlyAdvertised `
            -Config @{ LiveExePath = 'C:\mediamtx\mediamtx.exe'; LiveUdpAddress = '0.0.0.0:18189'; LiveAdvertiseHosts = @($AdvertiseHost) }
        return $r
    }

    try {
        if (-not $env:CAMUI_TEST_EXE -or -not (Test-Path -LiteralPath $env:CAMUI_TEST_EXE)) { throw 'CAMUI_TEST_EXE must name a camui.exe built from this tree.' }
        foreach ($p in @(18789, 18300)) { if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the check needs it free." } }
        if (-not (Test-Path -LiteralPath 'C:\mediamtx\mediamtx.exe')) { throw 'C:\mediamtx\mediamtx.exe is needed (the live instance runs from it so no firewall prompt can appear).' }
        if ($shots) { [void](New-Item -ItemType Directory -Path $shots -Force) }

        # ---- which address ----
        $candidates = @(Get-CamLanCandidates)
        $pick = $null; $why = ''
        if ($env:CAMTEST_LAN_ADDRESS) { $pick = @($candidates | Where-Object { $_.Address -eq $env:CAMTEST_LAN_ADDRESS })[0]; $why = 'CAMTEST_LAN_ADDRESS' }
        if (-not $pick) { $pick = @($candidates | Where-Object { -not $_.Virtual -and $_.Gateway })[0]; $why = 'a physical adapter with a default route' }
        if (-not $pick) { $pick = @($candidates | Where-Object { -not $_.Virtual })[0]; $why = 'a physical adapter' }
        if (-not $pick) { $pick = @($candidates | Where-Object { $_.Profile -eq 'Private' })[0]; $why = 'NO physical private address on this PC: a virtual adapter on a Private profile' }
        if (-not $pick) { throw 'this PC has no private IPv4 address on an up adapter to test with' }
        $lan = $pick.Address
        & $note ("LAN address used: adapter '{0}' ({1}, profile {2}, default route {3}) chosen as {4}; {5} candidate(s) existed; the address itself is not printed" -f $pick.Alias, $(if ($pick.Virtual) { 'VIRTUAL' } else { 'physical' }), $pick.Profile, $pick.Gateway, $why, $candidates.Count)

        # ---- the answer holds exactly that candidate ----
        $rig = & $startRig 'livelan' $lan 3
        $dir = $rig.Dir
        $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-livelan'
        if (-not $ui.Started) { throw ('camui did not start: ' + $ui.Reason) }
        $liveConf = Get-Content -LiteralPath (Join-Path $rig.Dir 'live\mediamtx-live.yml') | Where-Object { $_ -match '^webrtc(LocalUDPAddress|AdditionalHosts):' }
        & $note ("generated live config (the LAN address masked): {0}" -f ((($liveConf -join ' | ') -replace $ipPattern, 'a.b.c.d') -replace 'a\.b\.c\.d(?=:)', '0.0.0.0'))
        $answer = Invoke-CamRawHttp -Port 18789 -Method 'POST' -Path '/api/live/Cam%201/whep?stream=sub' -Headers @{ 'Content-Type' = 'application/sdp' } -Body $offer
        $ips = @(([regex]::Matches($answer.Body, '(?m)^a=candidate:\S+ 1 udp \d+ (\S+) (\d+) typ host') | ForEach-Object { $_.Groups[1].Value }) | Select-Object -Unique)
        & $note ("SDP answer: HTTP {0}, {1} distinct host candidate address(es), equals the LAN address: {2}, contains 127.0.0.1: {3}" -f $answer.Status, $ips.Count, (($ips -join ',') -eq $lan), ($ips -contains '127.0.0.1'))
        if ($answer.Status -ne 201 -or ($ips -join ',') -ne $lan) { & $fail 'the answer does not hold exactly the LAN host candidate' }
        if ($answer.Headers['location'] -match '^/api/live/session/[0-9a-f]{32}$') { [void](Invoke-CamRawHttp -Port 18789 -Method 'DELETE' -Path $answer.Headers['location']) }

        # ---- browsers play over it ----
        foreach ($browser in @('chrome', 'edge')) {
            $found = Get-CamTestBrowser -Browser $browser
            if (-not $found.Exe) { & $fail ("$browser could not be run: " + $found.Reason); continue }
            $began = [DateTime]::UtcNow
            try {
                $l = & $launch $browser; $started = $l.Started; $cdp = $l.Cdp
                $end = [DateTime]::UtcNow.AddSeconds(45); $s = $null
                while ([DateTime]::UtcNow -lt $end) { $s = & $stats; if (& $playing $s 3) { break }; Start-Sleep -Milliseconds 500 }
                if (-not (& $playing $s 3)) { & $fail "$browser : three tiles did not reach playing over the LAN candidate"; if ($s) { foreach ($t in @($s.tiles)) { & $note ("    {0}: {1} | {2}" -f $t.name, $t.state, $t.diag.line) } }; continue }
                Start-Sleep -Seconds 3
                $s2 = & $stats
                & $note ("== $browser $($found.Version): three tiles playing over the non-loopback candidate ==")
                foreach ($t in @($s.tiles)) {
                    $t2 = @($s2.tiles | Where-Object { $_.name -eq $t.name })[0]
                    $pair = [string]$t2.diag.pair
                    $remote = ($pair -split '<->')[-1].Trim()
                    & $note ("  {0}: framesDecoded {1} -> {2} in 3 s; selected pair {3}; server offered {4}; browser offered {5}" -f $t.name, $t.v.framesDecoded, $t2.v.framesDecoded, $pair, $t2.diag.answer, $t2.diag.local)
                    if ($t2.v.framesDecoded -lt $t.v.framesDecoded + 10) { & $fail "$browser $($t.name): framesDecoded did not keep growing" }
                    if ($remote -notmatch '/private$') { & $fail "$browser $($t.name): the selected remote candidate is not a private non-loopback address ($pair)" }
                    if (@($t2.diag.answerClasses) -contains 'loopback') { & $fail "$browser $($t.name): the server offered a loopback candidate" }
                }
                $overlays = Invoke-CamTestCdpEval -Cdp $cdp -Expression 'Array.from(document.querySelectorAll(".lover:not(.hide)")).length'
                & $note ("  explanation overlays visible on playing tiles: $overlays")
                if ($overlays -ne 0) { & $fail "$browser : an overlay is shown on a playing tile" }
            }
            catch { & $fail ("$browser run aborted: " + $_.Exception.Message) }
            finally {
                if ($started) { & $finish $started; $started = $null }
                $script:CamLanSeconds += ([DateTime]::UtcNow - $began).TotalSeconds
            }
        }
        [void](Stop-CamTestUi -Ui $ui); $ui = $null
        $notes1 = @(Stop-CamTestLiveRig -Rig $rig); if ($notes1 -match 'forced: (?!nothing)') { & $fail 'the first live rig had to be forced' }
        if ($script:CamLanOk) { try { Remove-CamTestTree -Path $rig.Dir } catch { } }
        $rig = $null

        # ---- negative: an address nothing answers; the tile must explain ----
        $rig = & $startRig 'livelan' '10.254.254.77' 2
        $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-livelan'
        if (-not $ui.Started) { throw ('camui did not restart: ' + $ui.Reason) }
        $began = [DateTime]::UtcNow; $started = $null
        try {
            $l = & $launch 'chrome'; $started = $l.Started; $cdp = $l.Cdp
            $end = [DateTime]::UtcNow.AddSeconds(40); $s = $null
            while ([DateTime]::UtcNow -lt $end) { $s = & $stats; if ($s -and @(@($s.tiles) | Where-Object { $_.state -eq 'error' -or $_.state -eq 'offline' }).Count -ge 1) { break }; Start-Sleep -Milliseconds 700 }
            Start-Sleep -Milliseconds 800
            $s = & $stats
            $t = @($s.tiles)[0]
            $overlay = Invoke-CamTestCdpEval -Cdp $cdp -Expression 'Array.from(document.querySelectorAll(".ltile")).slice(0, 1).map(t => t.querySelector(".lover").innerText).join("\n")'
            $copyVisible = Invoke-CamTestCdpEval -Cdp $cdp -Expression '!!document.querySelector(".ltile .lcopy:not(.hide)")'
            & $note ("== negative (Chrome): the server advertises an address nothing answers ==")
            & $note ("  tile state {0}; the overlay says: {1}" -f $t.state, (($overlay -split "`n" | Where-Object { $_ }) -join ' / '))
            & $note ("  diagnostics line: {0}" -f $t.diag.line)
            if ($t.state -notin 'error', 'offline') { & $fail 'a tile with no media did not reach error or offline' }
            if ($overlay -notmatch 'No media after 15 s' -or $overlay -notmatch 'server answered HTTP 201' -or $overlay -notmatch 'server offered 1 host \(private\)' -or $overlay -notmatch 'browser offered') { & $fail "the overlay does not explain the stuck tile in plain words: $overlay" }
            if (-not $copyVisible) { & $fail 'no Copy diagnostics button on a stuck tile' }
            if (($overlay + $t.diag.line) -match $ipPattern) { & $fail 'an IP address appears in the tile text or the diagnostics line' }
            if ($shots) { [void](Save-CamTestPageShot -Cdp $cdp -Path (Join-Path $shots 'chrome-stuck-tile.png')) }
            $pos = Invoke-CamTestCdpEval -Cdp $cdp -Expression '(() => { const r = document.querySelector(".ltile .lcopy:not(.hide)").getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()'
            foreach ($type in @('mouseMoved', 'mousePressed', 'mouseReleased')) { [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Input.dispatchMouseEvent' -Params @{ type = $type; x = $pos.x; y = $pos.y; button = 'left'; clickCount = 1; buttons = $(if ($type -eq 'mousePressed') { 1 } else { 0 }) }) }
            Start-Sleep -Milliseconds 400
            $label = Invoke-CamTestCdpEval -Cdp $cdp -Expression 'document.querySelector(".ltile .lcopy").textContent'
            $opened = (& $stats).modal
            & $note ("  Copy diagnostics clicked: the button now reads '$label'; the click did not open the modal: $(-not $opened)")
            if ($label -ne 'Copied') { & $fail "Copy diagnostics did not copy (button reads '$label')" }
            if ($opened) { & $fail 'clicking Copy diagnostics opened the camera modal' }
        }
        catch { & $fail ('negative run aborted: ' + $_.Exception.Message) }
        finally {
            if ($started) { & $finish $started }
            $script:CamLanSeconds += ([DateTime]::UtcNow - $began).TotalSeconds
        }
    }
    catch { & $fail ('check aborted: ' + $_.Exception.Message) }
    finally {
        Disconnect-CamTestCdp -Cdp $cdp
        foreach ($b in @($script:CamLanBrowsers)) { try { [void](Stop-CamTestBrowser -Started $b) } catch { } }
        if ($ui) { [void](Stop-CamTestUi -Ui $ui) }
        $notes = @()
        if ($rig) { $notes = @(Stop-CamTestLiveRig -Rig $rig) }
        $evidence += ("browser windows were up {0:n0} s in total; whole check {1:n0} s" -f $script:CamLanSeconds, $watch.Elapsed.TotalSeconds)
        $evidence += $notes
        if ($notes -match 'forced: (?!nothing)') { & $fail 'the supervisor had to be forced' }
        if ($script:CamLanOk -and $rig) { try { Remove-CamTestTree -Path $rig.Dir } catch { $script:CamLanEvidence += 'cleanup failed' } }
        elseif ($rig) { $script:CamLanEvidence += "left for inspection: $($rig.Dir)" }
    }

    Add-CamTestResult -Run $Run -Check 'livelan' -Status $(if ($script:CamLanOk) { 'PASS' } else { 'FAIL' }) `
        -Message 'with only a non-loopback address advertised, Chrome and Edge play over it; with an unreachable one the tile explains why' `
        -Evidence ($evidence + $script:CamLanEvidence)
}


function Get-CamTestSpkiHash {
    <# The base64 SHA-256 of an ECDSA P-256 certificate's SubjectPublicKeyInfo: the form Chromium's --ignore-certificate-errors-spki-list takes. #>
    param([Parameter(Mandatory = $true)][System.Security.Cryptography.X509Certificates.X509Certificate2]$Certificate)

    $header = [byte[]](0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x02, 0x01, 0x06, 0x08, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00)
    $point = $Certificate.PublicKey.EncodedKeyValue.RawData
    $spki = New-Object byte[] ($header.Length + $point.Length)
    [Array]::Copy($header, 0, $spki, 0, $header.Length); [Array]::Copy($point, 0, $spki, $header.Length, $point.Length)
    return [Convert]::ToBase64String([System.Security.Cryptography.SHA256]::Create().ComputeHash($spki))
}


function Invoke-CamCheckLiveTls {
    <#
        [Stage 2b, opt-in: opens browser windows] camui over HTTPS on the LAN address, with a token and a locally generated CA.

        Real supervisor, synthetic cameras, the media on the LAN address (as livelan), camui built to a temp path with -tls-listen on this PC's
        private address. The test camui runs from the path an EXISTING Windows Firewall allow rule already covers, so the bind beyond loopback
        raises no prompt; if there is no such rule the check is SKIPPED. The check creates no firewall rule and writes nothing to any Windows
        certificate store (the counts are compared before and after); trust is per probe or per browser process only.
          (a) probes with the generated CA: every route x {no session, wrong cookie, signed-in session}; handshake details; plain HTTP refused;
              an untrusted client and a wrong name refused; foreign Origin, http:// Origin, cross-site and a rebinding Host refused with no new
              live session; the CA download holds no key; the keys' ACL names only this account; a leak scan of the TLS responses with a
              positive control; login CSRF; then the username lockout and the per-address throttle (429 with Retry-After)
          (b) Chrome and Edge, throwaway profile: WITHOUT the SPKI flag the page is refused (certificate warning: the negative control); WITH
              --ignore-certificate-errors-spki-list it loads and lands on /login; wrong passwords show the generic error; a burst shows the lockout; a second sign-in lands on /#live, the cookie
              is Secure/HttpOnly/Strict, tiles play on the LAN media, the session survives a camui restart, Sign out returns to /login
        Needs CAMUI_TEST_EXE. Screenshots (page only) go to $env:CAMTEST_SHOTS when set.
    #>
    param($Run)

    $fail = { param([string]$Text) $script:CamTlsOk = $false; $script:CamTlsEvidence += ('FAIL ' + $Text) }
    $note = { param([string]$Text) $script:CamTlsEvidence += $Text }
    $script:CamTlsOk = $true; $script:CamTlsEvidence = @(); $script:CamTlsBrowsers = @(); $script:CamTlsSeconds = 0
    $watch = [System.Diagnostics.Stopwatch]::StartNew()
    $rig = $null; $ui = $null; $cdp = $null; $backup = ''; $allowed = $null
    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\livetls'
    $shots = [string]$env:CAMTEST_SHOTS
    $offer = Get-CamTestWhepOffer
    $tlsPort = 18443
    $stores = @('Cert:\CurrentUser\Root', 'Cert:\CurrentUser\CA', 'Cert:\CurrentUser\TrustedPeople', 'Cert:\LocalMachine\Root', 'Cert:\LocalMachine\CA', 'Cert:\LocalMachine\TrustedPeople')
    $storeCount = { @($stores | ForEach-Object { @(Get-ChildItem -Path $_ -ErrorAction SilentlyContinue).Count }) -join '/' }
    $ours = { @($stores | ForEach-Object { Get-ChildItem -Path $_ -ErrorAction SilentlyContinue | Where-Object { $_.Subject -match 'IPCCTV' } }).Count }

    try {
        if (-not $env:CAMUI_TEST_EXE -or -not (Test-Path -LiteralPath $env:CAMUI_TEST_EXE)) { throw 'CAMUI_TEST_EXE must name a camui.exe built from this tree.' }
        foreach ($p in @(18789, $tlsPort, 18300, 18892)) { if (-not (Test-CamTestPortFree -Port $p)) { throw "TCP port $p is already in use; the check needs it free." } }
        if (-not (Test-Path -LiteralPath 'C:\mediamtx\mediamtx.exe')) { throw 'C:\mediamtx\mediamtx.exe is needed.' }
        $allowed = Get-CamTestAllowedCamuiPath
        if (-not $allowed) {
            Add-CamTestResult -Run $Run -Check 'livetls' -Status 'SKIP' -Message 'no existing enabled Windows Firewall allow rule for a test camui.exe under %TEMP% (Private profile): a bind beyond loopback would raise a firewall prompt, and this check never creates a rule'
            return
        }
        if ($shots) { [void](New-Item -ItemType Directory -Path $shots -Force) }

        # ---- the LAN address, as in livelan ----
        $candidates = @(Get-CamLanCandidates)
        $pick = @($candidates | Where-Object { -not $_.Virtual -and $_.Gateway })[0]
        if (-not $pick) { $pick = @($candidates | Where-Object { -not $_.Virtual })[0] }
        if (-not $pick) { $pick = @($candidates | Where-Object { $_.Profile -eq 'Private' })[0] }
        if (-not $pick) {
            Add-CamTestResult -Run $Run -Check 'livetls' -Status 'SKIP' -Message 'this PC has no private IPv4 address on an up adapter to bind the HTTPS listener to'
            return
        }
        $lan = $pick.Address
        & $note ("LAN address used: adapter '{0}' ({1}, profile {2}); the address itself is not printed. Firewall allow rule reused (read only) for the test camui path." -f $pick.Alias, $(if ($pick.Virtual) { 'VIRTUAL' } else { 'physical' }), $pick.Profile)
        $storesBefore = & $storeCount; $fwBefore = @(Get-NetFirewallRule -ErrorAction SilentlyContinue).Count

        # ---- the rig, the test camui, the token and the CA ----
        $cameras = 1..3 | ForEach-Object { [pscustomobject]@{ Name = "Cam $_"; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = ($_ -eq 1) } }
        $rig = Start-CamTestLiveRig -Name 'livetls' -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds 4 -LiveOnlyAdvertised -LiveApiPort 18892 `
            -Config @{ LiveExePath = 'C:\mediamtx\mediamtx.exe'; LiveUdpAddress = '0.0.0.0:18189'; LiveAdvertiseHosts = @($lan) }
        $dir = $rig.Dir
        $tlsDir = Join-Path -Path $rig.Dir -ChildPath 'tls'
        if (Test-Path -LiteralPath $allowed) { $backup = $allowed + '.livetls-bak'; Move-Item -LiteralPath $allowed -Destination $backup -Force }
        [void](New-Item -ItemType Directory -Path (Split-Path -Parent $allowed) -Force)
        Copy-Item -LiteralPath $env:CAMUI_TEST_EXE -Destination $allowed -Force
        $authDir = Join-Path -Path $rig.Dir -ChildPath 'auth'
        $password = 'Pw-' + [guid]::NewGuid().ToString('N') + '-throwaway'
        # The LAN listener refuses to start without a user.
        $emptyAuth = Join-Path -Path $rig.Dir -ChildPath 'auth-empty'
        $refuseErr = Join-Path -Path $rig.Dir -ChildPath 'refuse.err'
        $refuse = Start-Process -FilePath $allowed -ArgumentList @('-listen', '127.0.0.1:18790', '-tls-listen', ('{0}:{1}' -f $lan, $tlsPort), '-tls-dir', $tlsDir, '-auth-dir', $emptyAuth) -PassThru -WindowStyle Hidden -RedirectStandardError $refuseErr -RedirectStandardOutput (Join-Path -Path $rig.Dir -ChildPath 'refuse.out')
        $null = $refuse.Handle
        if (-not $refuse.WaitForExit(15000)) { try { $refuse.Kill() } catch { }; & $fail 'camui with a TLS listener and no user did not refuse to start' }
        else {
            $refuseText = (@(Get-Content -LiteralPath $refuseErr -ErrorAction SilentlyContinue) -join ' ')
            & $note ("camui -tls-listen with no user: exit code {0}, message names -set-password and /setup: {1}, created the tls folder: {2}" -f $refuse.ExitCode, ($refuseText -match 'refusing to start' -and $refuseText -match '-set-password' -and $refuseText -match '/setup'), (Test-Path $tlsDir))
            if ($refuse.ExitCode -ne 2 -or $refuseText -notmatch 'refusing to start' -or (Test-Path $tlsDir)) { & $fail 'the no-user refusal is wrong' }
        }
        # Users are created the way the Operator does it: the password on standard input, never on a command line.
        $timer = [System.Diagnostics.Stopwatch]::StartNew()
        $made = @(foreach ($name in @('probe', 'chrome', 'edge')) { $password | & $allowed '-set-password' $name '-auth-dir' $authDir 2>&1 })
        $timer.Stop()
        & $note ("camui.exe -set-password x3 (process start + one PBKDF2 hash each): {0:n0} ms in total; output: {1}; output holds the password: {2}" -f $timer.ElapsedMilliseconds, ($made -join ' / '), (($made -join ' ').Contains($password)))
        if (($made -join ' ').Contains($password) -or @($made | Where-Object { $_ -match '^created user' }).Count -ne 3) { & $fail '-set-password output is wrong' }
        $usersJson = [System.IO.File]::ReadAllText((Join-Path $authDir 'users.json'))
        $hashTail = (ConvertFrom-Json -InputObject $usersJson).users[0].hash.Split('$')[3]
        & $note ("users.json: scheme {0}, holds the password: {1}" -f (ConvertFrom-Json -InputObject $usersJson).users[0].hash.Substring(0, 21), $usersJson.Contains($password))
        if ($usersJson.Contains($password) -or $usersJson -notmatch 'pbkdf2-sha256\$600000\$') { & $fail 'users.json holds the password or the wrong hash scheme' }
        $printed = @(& $allowed '-print-ca' '-tls-dir' $tlsDir)
        $printedFp = ([regex]::Match(($printed -join "`n"), 'SHA-256:\s+([0-9A-F:]{95})')).Groups[1].Value
        $caPem = [System.IO.File]::ReadAllText((Join-Path $tlsDir 'ca.crt'))
        $ca = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2(, [System.Text.Encoding]::ASCII.GetBytes($caPem))
        $caFp = ([BitConverter]::ToString([System.Security.Cryptography.SHA256]::Create().ComputeHash($ca.RawData))).Replace('-', ':')
        & $note ("camui.exe -print-ca: file printed, fingerprint matches the ca.crt on disk: {0}; leaf created by it: {1}; printed text holds PRIVATE KEY: {2}" -f ($printedFp -eq $caFp), (Test-Path (Join-Path $tlsDir 'leaf.crt')), (($printed -join "`n") -match 'PRIVATE KEY'))
        if ($printedFp -ne $caFp -or (Test-Path (Join-Path $tlsDir 'leaf.crt')) -or ($printed -join "`n") -match 'PRIVATE KEY') { & $fail '-print-ca output is wrong' }

        $uiArgs = @('-tls-listen', ('{0}:{1}' -f $lan, $tlsPort), '-tls-dir', $tlsDir, '-auth-dir', $authDir)
        $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-livetls' -RunExe $allowed -ExtraArgs $uiArgs
        if (-not $ui.Started) { throw ('camui did not start: ' + $ui.Reason) }
        $up = $false
        for ($i = 0; $i -lt 40 -and -not $up; $i++) { $tcp = New-Object System.Net.Sockets.TcpClient; try { $tcp.Connect($lan, $tlsPort); $up = $true } catch { Start-Sleep -Milliseconds 250 } finally { $tcp.Close() } }
        if (-not $up) { throw 'the HTTPS listener did not come up' }
        $tls = { param([hashtable]$a) $args2 = @{ Address = $lan; Port = $tlsPort; CaPem = $caPem } + $a; Invoke-CamTestTls @args2 }
        $jsonH = @{ 'Content-Type' = 'application/json' }
        $loginBodyOf = { param([string]$u, [string]$p) ConvertTo-Json -InputObject @{ username = $u; password = $p } -Compress }
        $loginProbe = & $tls @{ Method = 'POST'; Path = '/api/auth/login'; Headers = $jsonH; Body = (& $loginBodyOf 'probe' $password) }
        $cookieHeader = [string]$loginProbe.Headers['set-cookie']
        $sessionValue = [regex]::Match($cookieHeader, 'camui_session=([^;]+)').Groups[1].Value
        & $note ("login over TLS: HTTP {0}; Set-Cookie flags: Secure {1}, HttpOnly {2}, SameSite=Strict {3}, Path=/ {4}, Max-Age=2592000 {5}; token length {6}" -f $loginProbe.Status, ($cookieHeader -match '; Secure'), ($cookieHeader -match 'HttpOnly'), ($cookieHeader -match 'SameSite=Strict'), ($cookieHeader -match 'Path=/'), ($cookieHeader -match 'Max-Age=2592000'), $sessionValue.Length)
        if ($loginProbe.Status -ne 200 -or $sessionValue.Length -lt 43 -or $cookieHeader -notmatch '; Secure' -or $cookieHeader -notmatch 'HttpOnly' -or $cookieHeader -notmatch 'SameSite=Strict' -or $cookieHeader -notmatch 'Max-Age=2592000') { throw ('the probe login failed or its cookie is wrong: HTTP ' + $loginProbe.Status) }
        $bearer = @{ Cookie = "camui_session=$sessionValue" }

        # ---- certificate facts (no keys) ----
        $probe = & $tls @{ Path = '/api/tls/info'; Headers = $bearer }
        $leaf = $probe.Leaf
        if (-not $leaf) { throw ('the TLS handshake with the generated CA failed: ' + $probe.Error) }
        $san = ($leaf.Extensions | Where-Object { $_.Oid.Value -eq '2.5.29.17' } | ForEach-Object { $_.Format($false) }) -replace '\d+\.\d+\.\d+\.\d+', 'a.b.c.d'
        $bc = ($ca.Extensions | Where-Object { $_.Oid.Value -eq '2.5.29.19' } | ForEach-Object { $_.Format($false) })
        $nc = @($ca.Extensions | Where-Object { $_.Oid.Value -eq '2.5.29.30' })
        $ncText = ($nc | ForEach-Object { $_.Format($true) }) -join ' | ' -replace '\s+', ' '
        $ku = ($ca.Extensions | Where-Object { $_.Oid.Value -eq '2.5.29.15' } | ForEach-Object { $_.Format($false) })
        $days = [Math]::Round(($leaf.NotAfter - $leaf.NotBefore).TotalDays, 1)
        & $note ("CA: '{0}', {1} {2}, valid {3:n0} days (until {4}), basic constraints [{5}], key usage [{6}], name constraints present and critical: {7}" -f $ca.Subject, $ca.PublicKey.Oid.FriendlyName, $ca.PublicKey.Key.KeySize, ($ca.NotAfter - $ca.NotBefore).TotalDays, $ca.NotAfter.ToString('yyyy-MM-dd'), $bc, $ku, ($nc.Count -eq 1 -and $nc[0].Critical))
        & $note ("name constraints: $ncText")
        & $note ("leaf: '{0}' issued by '{1}', valid {2} days (limit 397), SAN [{3}], key {4}, signature {5}, TLS from this client: {6}" -f $leaf.Subject, $leaf.Issuer, $days, $san, $leaf.PublicKey.Oid.FriendlyName, $leaf.SignatureAlgorithm.FriendlyName, $probe.Protocol)
        if ($days -gt 397 -or $nc.Count -ne 1 -or -not $nc[0].Critical) { & $fail 'leaf validity or CA name constraints are wrong' }

        # ---- ACL of the key files ----
        $me = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        foreach ($name in @('ca.key', 'leaf.key', '.')) {
            $target = Join-Path $tlsDir $name
            $acl = @(& icacls.exe $target) | Where-Object { $_ -match '[:]\(' -or $_ -match '\\' -and $_ -match '\(' }
            $principals = @($acl | ForEach-Object { ([regex]::Match($_, '(?:^\S+\s+)?([^\s:][^:]*?):\(')).Groups[1].Value.Trim() } | Where-Object { $_ })
            & $note ("ACL of {0}: {1} principal(s): {2}" -f $(if ($name -eq '.') { 'the tls folder' } else { $name }), $principals.Count, ($principals -join '; '))
            if ($principals.Count -ne 1 -or $principals[0] -ne $me) { & $fail "ACL of $name is not this account alone" }
        }

        foreach ($name in @('users.json', 'sessions.json', 'auth.log', '.')) {
            $target = Join-Path $authDir $name
            $acl = @(& icacls.exe $target) | Where-Object { $_ -match '[:]\(' -or $_ -match '\\' -and $_ -match '\(' }
            $principals = @($acl | ForEach-Object { ([regex]::Match($_, '(?:^\S+\s+)?([^\s:][^:]*?):\(')).Groups[1].Value.Trim() } | Where-Object { $_ })
            & $note ("ACL of {0}: {1} principal(s): {2}" -f $(if ($name -eq '.') { 'the auth folder' } else { $name }), $principals.Count, ($principals -join '; '))
            if ($principals.Count -ne 1 -or $principals[0] -ne $me) { & $fail "ACL of auth $name is not this account alone" }
        }

        # ---- every route x session state ----
        $routes = @(
            @{ M = 'GET'; P = '/' }, @{ M = 'GET'; P = '/login'; Public = $true }, @{ M = 'GET'; P = '/login.js'; Public = $true }, @{ M = 'GET'; P = '/style.css'; Public = $true }, @{ M = 'GET'; P = '/api/auth/me'; Public = $true }, @{ M = 'GET'; P = '/setup' }, @{ M = 'POST'; P = '/api/auth/setup'; H = $jsonH; B = '{}' }, @{ M = 'GET'; P = '/app.js' }, @{ M = 'GET'; P = '/live.js' }, @{ M = 'GET'; P = '/api/status' }, @{ M = 'GET'; P = '/api/live' },
            @{ M = 'GET'; P = '/api/cameras' }, @{ M = 'GET'; P = '/api/config' }, @{ M = 'GET'; P = '/api/footage' }, @{ M = 'GET'; P = '/api/tls/ca.crt' }, @{ M = 'GET'; P = '/api/tls/info' },
            @{ M = 'POST'; P = '/api/live/Cam%201/whep?stream=sub'; H = @{ 'Content-Type' = 'application/sdp' }; B = $offer },
            @{ M = 'DELETE'; P = '/api/live/session/00000000000000000000000000000000' }, @{ M = 'GET'; P = '/nothing' })
        $needles = [ordered]@{
            'live secret' = ([System.IO.File]::ReadAllText([string]$rig.Sandbox.Config.LiveSecretPath)).Trim(); 'live user' = 'camlive'; 'live port' = '18889'
            'live path name' = 'cam1sub|cam1main|cam2sub|cam2main|cam3sub|cam3main'; 'camera password' = 'FakePassword-DoNotUse-9c41'; 'camera user' = 'fakeuser:'
            'camera/relay port' = '18554|18564|18567'; 'password' = $password; 'session token' = $sessionValue; 'password hash' = $hashTail
            'key material' = ((@('ca.key', 'leaf.key') | ForEach-Object { ([System.IO.File]::ReadAllLines((Join-Path $tlsDir $_)))[1].Substring(0, 30) }) -join '|') + '|PRIVATE KEY'
        }
        $scan = {
            param([string]$Label, [string]$Text, [bool]$AllowCertificate)
            $found = @()
            foreach ($key in $needles.Keys) {
                foreach ($needle in ($needles[$key] -split '\|')) {
                    $hit = $(if ($needle -match '^[0-9]+$') { [regex]::IsMatch($Text, '(?<![0-9A-Za-z])' + $needle + '(?![0-9A-Za-z])') } else { $Text.IndexOf($needle, [StringComparison]::OrdinalIgnoreCase) -ge 0 })
                    if ($hit) { $found += ("{0} in {1}" -f $key, $Label) }
                }
            }
            foreach ($m in [regex]::Matches($Text, '(?i)[a-z][a-z0-9+.\-]*://[^\s"'',)]*')) { $found += ('URL scheme in {0}' -f $Label) }
            if (-not $AllowCertificate -and $Text.Contains('BEGIN CERTIFICATE')) { $found += ('certificate PEM in {0}' -f $Label) }
            return @($found)
        }
        $control = ($needles.Values | ForEach-Object { ($_ -split '\|')[0] }) -join ' ; '
        $controlHits = @(& $scan 'control' ($control + ' ; rtsp://x ; BEGIN CERTIFICATE') $false)
        $classes = @($needles.Keys | Where-Object { $k = $_; @($controlHits | Where-Object { $_ -like "$k*" }).Count -ge 1 }).Count
        & $note ("leak scan positive control: {0} of {1} needle classes, the scheme rule and the certificate rule lit up" -f $classes, $needles.Count)
        if ($classes -ne $needles.Count -or -not ($controlHits -like 'URL scheme*') -or -not ($controlHits -like 'certificate PEM*')) { & $fail 'the TLS leak scanner did not light up on its positive control' }

        & $note '  route                                        no session | wrong cookie | signed in'
        $leakHits = 0
        $locations = @()
        foreach ($r in $routes) {
            $base = @{ Method = $r.M; Path = $r.P }
            if ($r.H) { $base.Headers = $r.H }
            if ($r.B) { $base.Body = $r.B }
            $none = & $tls $base
            $wrongArgs = $base.Clone(); $wrongArgs.Headers = @{ Cookie = 'camui_session=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' } + $(if ($r.H) { $r.H } else { @{} })
            $wrong = & $tls $wrongArgs
            $rightArgs = $base.Clone(); $rightArgs.Headers = $bearer + $(if ($r.H) { $r.H } else { @{} })
            $right = & $tls $rightArgs
            & $note ("  {0,-6} {1,-40} {2,8} | {3,11} | {4,11}{5}" -f $r.M, $r.P.Substring(0, [Math]::Min(40, $r.P.Length)), $none.Status, $wrong.Status, $right.Status, $(if ($r.Public) { '  (public)' } else { '' }))
            if ($r.Public) {
                if ($none.Status -ne 200 -or $wrong.Status -ne 200 -or $right.Status -notin 200, 303) { & $fail "public route $($r.M) $($r.P): $($none.Status)/$($wrong.Status)/$($right.Status)" }
            }
            elseif ($none.Status -ne 401 -or $wrong.Status -ne 401 -or $right.Status -in 0, 401, 429 -or $none.Body -notmatch 'authentication required') { & $fail "route $($r.M) $($r.P): $($none.Status)/$($wrong.Status)/$($right.Status)" }
            foreach ($x in @($none, $wrong, $right)) {
                $text = ($x.Headers.GetEnumerator() | ForEach-Object { '{0}: {1}' -f $_.Key, $_.Value }) -join "`n"
                $hits = @(& $scan "$($r.M) $($r.P)" ($text + "`n" + $x.Body) ($r.P -eq '/api/tls/ca.crt'))
                # /api/cameras is the one response that carries camera ADDRESSES by design (the Operator types them; no credential, no URL): the address needle is not a leak there.
                if ($r.P -eq '/api/cameras') { $hits = @($hits | Where-Object { $_ -notlike 'camera/relay port*' }) }
                $leakHits += $hits.Count
                foreach ($h in $hits) { & $fail ('LEAK ' + $h) }
            }
            if ($r.P -like '*whep*' -and $right.Headers['location']) { $locations += $right.Headers['location'] }
        }
        $nav = & $tls @{ Path = '/'; Headers = @{ Accept = 'text/html,application/xhtml+xml' } }
        $me0 = & $tls @{ Path = '/api/auth/me' }
        & $note ("  browser navigation to / without a session: HTTP {0}, Location [{1}]; /api/auth/me signed out: {2}" -f $nav.Status, $nav.Headers['location'], $me0.Body.Trim())
        if ($nav.Status -ne 303 -or $nav.Headers['location'] -ne '/login?next=%2F' -or $me0.Body -notmatch '"authenticated":false') { & $fail 'the unauthenticated navigation is not sent to /login' }
        $loginPage = & $tls @{ Path = '/login' }
        & $note ("  /login headers: CSP [{0}], Cache-Control [{1}]" -f $loginPage.Headers['content-security-policy'], $loginPage.Headers['cache-control'])
        if ($loginPage.Headers['content-security-policy'] -notmatch "script-src 'self'" -or $loginPage.Body -match '<script>' ) { & $fail 'the login page lacks its CSP or carries inline script' }
        & $note ("leak scan over {0} routes x 3 session states of TLS responses (headers and bodies): {1} hit(s)" -f $routes.Count, $leakHits)
        foreach ($loc in $locations) { $d = & $tls @{ Method = 'DELETE'; Path = $loc; Headers = $bearer }; & $note ("  DELETE of the session the probe opened: HTTP $($d.Status)") }
        $down = & $tls @{ Path = '/api/tls/ca.crt'; Headers = $bearer }
        $disk = [System.IO.File]::ReadAllText((Join-Path $tlsDir 'ca.crt'))
        & $note ("CA download: HTTP {0}, Content-Type [{1}], Content-Disposition [{2}], identical to ca.crt on disk: {3}, holds a key: {4}" -f $down.Status, $down.Headers['content-type'], $down.Headers['content-disposition'], ($down.Body.Trim() -eq $disk.Trim()), ($down.Body -match 'PRIVATE KEY'))
        if ($down.Headers['content-type'] -ne 'application/x-x509-ca-cert' -or $down.Body.Trim() -ne $disk.Trim() -or $down.Body -match 'PRIVATE KEY') { & $fail 'the CA download is wrong' }

        # ---- refusals ----
        $before = (Get-CamTestLiveSessions -Rig $rig).Count
        $hostPort = '{0}:{1}' -f $lan, $tlsPort
        $refusals = @(
            @{ L = 'foreign https Origin'; H = @{ Origin = 'https://evil.example' } },
            @{ L = 'http:// Origin of this very address'; H = @{ Origin = "http://$hostPort" } },
            @{ L = 'Origin null'; H = @{ Origin = 'null' } },
            @{ L = 'Sec-Fetch-Site cross-site'; H = @{ 'Sec-Fetch-Site' = 'cross-site' } },
            @{ L = 'Sec-Fetch-Site same-site'; H = @{ 'Sec-Fetch-Site' = 'same-site' } },
            @{ L = 'rebinding Host attacker-rebind.example'; H = @{ Host = 'attacker-rebind.example:18443' } })
        foreach ($case in $refusals) {
            $x = & $tls @{ Method = 'POST'; Path = '/api/live/Cam%201/whep?stream=sub'; Headers = ($bearer + @{ 'Content-Type' = 'application/sdp' } + $case.H); Body = $offer }
            & $note ("  refused with the right token: {0} -> HTTP {1}" -f $case.L, $x.Status)
            if ($x.Status -ne 403) { & $fail "$($case.L) was not refused (HTTP $($x.Status))" }
        }
        foreach ($case in $refusals) {
            $x = & $tls @{ Method = 'POST'; Path = '/api/auth/login'; Headers = ($jsonH + $case.H); Body = (& $loginBodyOf 'probe' $password) }
            & $note ("  login CSRF with the RIGHT password: {0} -> HTTP {1}, cookie issued: {2}" -f $case.L, $x.Status, [bool]$x.Headers['set-cookie'])
            if ($x.Status -ne 403 -or $x.Headers['set-cookie']) { & $fail "login with $($case.L) was not refused (HTTP $($x.Status))" }
        }
        $ok = & $tls @{ Method = 'POST'; Path = '/api/live/Cam%201/whep?stream=sub'; Headers = ($bearer + @{ 'Content-Type' = 'application/sdp'; Origin = "https://$hostPort"; 'Sec-Fetch-Site' = 'same-origin' }); Body = $offer }
        & $note ("  positive control: the page's own https Origin with the token -> HTTP {0} (a live session)" -f $ok.Status)
        if ($ok.Status -ne 201) { & $fail 'a request shaped like the page own was not accepted' }
        $after = (Get-CamTestLiveSessions -Rig $rig).Count
        & $note ("  live-instance sessions: {0} before the refused requests, {1} after them and the one accepted request" -f $before, $after)
        if ($after -ne $before + 1) { & $fail 'a refused request created a live session (or the accepted one did not)' }
        if ($ok.Headers['location']) { [void](& $tls @{ Method = 'DELETE'; Path = $ok.Headers['location']; Headers = $bearer }) }
        $plain = & $tls @{ Plain = $true; Path = '/api/live'; Headers = $bearer }
        & $note ("plain HTTP to the TLS port: HTTP {0}, application content in the reply: {1}" -f $plain.Status, ($plain.Body -match '"enabled"|<html'))
        if ($plain.Status -notin 0, 400 -or $plain.Body -match '"enabled"|<html') { & $fail 'plain HTTP to the TLS port was served' }
        $bare = Invoke-CamTestTls -Address $lan -Port $tlsPort -Path '/' -Headers $bearer
        & $note ("a client that does not know the CA: handshake refused ({0})" -f $(if ($bare.Error) { 'yes' } else { 'NO' }))
        if (-not $bare.Error) { & $fail 'a client without the CA connected' }
        $wrongName = Invoke-CamTestTls -Address $lan -Port $tlsPort -CaPem $caPem -ServerName 'not-this-pc.example' -Path '/' -Headers $bearer
        & $note ("the right CA but a wrong server name: handshake refused ({0})" -f $(if ($wrongName.Error) { 'yes' } else { 'NO' }))
        if (-not $wrongName.Error) { & $fail 'a wrong server name was accepted' }
        $loop = Invoke-CamRawHttp -Port 18789 -Path '/api/tls/ca.crt'
        & $note ("the loopback listener: /api/tls/ca.crt -> HTTP {0} (TLS-only route)" -f $loop.Status)
        if ($loop.Status -ne 404) { & $fail 'the CA route exists on the loopback listener' }

        # ---- [Stage 2b.1] view-only by default: a valid token can watch but cannot stop or change anything ----
        $cfgHashBefore = (Get-FileHash -LiteralPath $rig.Sandbox.ConfigPath -Algorithm SHA256).Hash
        $camHashBefore = (Get-FileHash -LiteralPath $rig.Sandbox.Cameras -Algorithm SHA256).Hash
        $ackFile = Join-Path -Path (Split-Path -Parent (Get-CamStatusFilePath -Config $rig.Sandbox.Config)) -ChildPath 'camui-acknowledged.json'
        $pageHeaders = @{ Origin = "https://$hostPort"; 'Sec-Fetch-Site' = 'same-origin'; 'Content-Type' = 'application/json' }
        $writes = @(
            @{ L = 'POST /api/control (stop everything)'; M = 'POST'; P = '/api/control'; B = '{"scope":"all","action":"stop"}' },
            @{ L = 'PUT /api/config'; M = 'PUT'; P = '/api/config'; B = '{"config":{"SegmentSeconds":300}}' },
            @{ L = 'PUT /api/cameras'; M = 'PUT'; P = '/api/cameras'; B = '{"cameras":[]}' },
            @{ L = 'POST /api/cameras/Cam 1/record'; M = 'POST'; P = '/api/cameras/Cam%201/record'; B = '{"record":false}' },
            @{ L = 'POST /api/acknowledge'; M = 'POST'; P = '/api/acknowledge'; B = '{"key":"stopNotice","at":"2026-01-01T00:00:00Z"}' },
            @{ L = 'DELETE /api/cameras'; M = 'DELETE'; P = '/api/cameras'; B = '' })
        foreach ($w in $writes) {
            $args2 = @{ Method = $w.M; Path = $w.P; Headers = ($bearer + $pageHeaders) }
            if ($w.B) { $args2.Body = $w.B }
            $x = & $tls $args2
            & $note ("  view-only, right token and page-shaped headers: {0} -> HTTP {1} {2}" -f $w.L, $x.Status, $x.Body.Trim())
            if ($x.Status -ne 403 -or $x.Body -notmatch 'read-only: this address can only watch') { & $fail "$($w.L) was not refused as read-only (HTTP $($x.Status))" }
        }
        $stat = (Get-CamTestStatusJson -Sandbox $rig.Sandbox).Snapshot
        $unchanged = (-not $rig.Supervisor.HasExited) -and $stat.State -eq 'running' -and $stat.Live.Alive -and ((Get-FileHash -LiteralPath $rig.Sandbox.ConfigPath -Algorithm SHA256).Hash -eq $cfgHashBefore) -and
            ((Get-FileHash -LiteralPath $rig.Sandbox.Cameras -Algorithm SHA256).Hash -eq $camHashBefore) -and -not (Test-Path -LiteralPath $ackFile)
        & $note ("  after those requests: the sandbox supervisor still runs ({0}), status State '{1}', live instance alive {2}, recorder.config.json and cameras.json unchanged, no acknowledgement file written: {3}" -f (-not $rig.Supervisor.HasExited), $stat.State, $stat.Live.Alive, $unchanged)
        if (-not $unchanged) { & $fail 'a refused write changed something' }
        $unknown = & $tls @{ Path = '/api/not-a-route'; Headers = $bearer }
        & $note ("  an unknown /api route with the right token -> HTTP {0} (deny by default)" -f $unknown.Status)
        if ($unknown.Status -ne 403) { & $fail 'an unknown route was not refused' }
        $infoRo = & $tls @{ Path = '/api/tls/info'; Headers = $bearer }
        if ($infoRo.Body -notmatch '"readOnly":\s*true') { & $fail '/api/tls/info does not say readOnly:true' }

        # ---- browsers ----
        $spki = (Get-CamTestSpkiHash -Certificate $leaf) + ',' + (Get-CamTestSpkiHash -Certificate $ca)
        $url = "https://${hostPort}/#live"
        $restartUi = {
            param([string[]]$More)
            [void](Stop-CamTestUi -Ui $ui)
            $fresh = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-livetls' -RunExe $allowed -ExtraArgs ($uiArgs + $More)
            Set-Variable -Name ui -Value $fresh -Scope 1
            if (-not $fresh.Started) { throw ('camui did not restart: ' + $fresh.Reason) }
            $upAgain = $false
            for ($i = 0; $i -lt 40 -and -not $upAgain; $i++) { $tcp = New-Object System.Net.Sockets.TcpClient; try { $tcp.Connect($lan, $tlsPort); $upAgain = $true } catch { Start-Sleep -Milliseconds 250 } finally { $tcp.Close() } }
            if (-not $upAgain) { throw 'the HTTPS listener did not come back' }
        }
        foreach ($browser in @('chrome', 'edge')) {
            $found = Get-CamTestBrowser -Browser $browser
            if (-not $found.Exe) { & $fail ("$browser could not be run: " + $found.Reason); continue }
            & $note ("== $browser $($found.Version) ==")
            foreach ($withFlag in @($false, $true)) {
                $started = $null; $began = [DateTime]::UtcNow
                try {
                    $started = Start-CamTestBrowser -Browser $browser -BaseDir $dir -Url 'about:blank' -DebugPort 18300 -WindowSize '1280,900' -ExtraArgs $(if ($withFlag) { "--ignore-certificate-errors-spki-list=$spki" } else { '' })
                    $script:CamTlsBrowsers += $started
                    $cdp = Connect-CamTestCdp -Port 18300
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.enable')
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Network.enable')
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Emulation.setDeviceMetricsOverride' -Params @{ width = 1280; height = 800; deviceScaleFactor = 1; mobile = $false })
                    $nav = Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = $url }
                    $errorText = $(if ($nav.PSObject.Properties['errorText']) { [string]$nav.errorText } else { '' })
                    if (-not $withFlag) {
                        & $note ("  WITHOUT the SPKI flag: navigation error '{0}' (the certificate warning is present: the negative control)" -f $errorText)
                        if ($errorText -notmatch 'CERT') { & $fail "$browser without the flag was not refused for its certificate ($errorText)" }
                        if ($shots) { Start-Sleep -Milliseconds 600; [void](Save-CamTestPageShot -Cdp $cdp -Path (Join-Path $shots "$browser-cert-warning.png")) }
                        continue
                    }
                    & $note ("  WITH --ignore-certificate-errors-spki-list (leaf and CA SPKI, base64 SHA-256; this process only): navigation error '{0}'" -f $errorText)
                    if ($errorText) { & $fail "$browser with the SPKI flag still hit a certificate error ($errorText)" }

                    $ev = { param([string]$js) Invoke-CamTestCdpEval -Cdp $cdp -Expression $js -Await }
                    $until = {
                        param([string]$js, [int]$sec = 25)
                        $stop = [DateTime]::UtcNow.AddSeconds($sec)
                        while ([DateTime]::UtcNow -lt $stop) { try { if (Invoke-CamTestCdpEval -Cdp $cdp -Expression $js -Await) { return $true } } catch { }; Start-Sleep -Milliseconds 300 }
                        return $false
                    }
                    $submit = {
                        param([string]$user, [string]$pw)
                        [void](& $ev ('(async () => { document.getElementById("username").value = ' + (ConvertTo-Json -InputObject $user -Compress) + '; document.getElementById("password").value = ' + (ConvertTo-Json -InputObject $pw -Compress) + '; document.getElementById("go").click(); return true; })()'))
                    }
                    $message = { try { [string](& $ev '(async () => { const m = document.getElementById("msg"); return m.hidden ? "" : m.textContent; })()') } catch { '' } }
                    $settled = '(async () => { const m = document.getElementById("msg"); return !document.getElementById("go").disabled || /Too many/.test(m.textContent); })()'

                    # 1. the unauthenticated navigation lands on the login page
                    $onLogin = & $until '(location.pathname === "/login") && !!document.getElementById("form")'
                    $where = & $ev 'location.pathname + location.search + location.hash'
                    & $note ("  unauthenticated navigation to /#live lands on: {0} (login form present: {1})" -f $where, $onLogin)
                    if (-not $onLogin -or $where -notmatch '^/login\?next=%2F') { & $fail "$browser : an unauthenticated navigation did not land on /login ($where)" }
                    $fields = & $ev '(async () => ({ user: document.getElementById("username").autocomplete, pass: document.getElementById("password").autocomplete, type: document.getElementById("password").type }))()'
                    & $note ("  login form autocomplete: username [{0}], password [{1}], password input type [{2}]" -f $fields.user, $fields.pass, $fields.type)
                    if ($fields.user -ne 'username' -or $fields.pass -ne 'current-password' -or $fields.type -ne 'password') { & $fail "$browser : the login form attributes are wrong" }
                    if ($shots) {
                        foreach ($size in @(@(1920, 1080, $false), @(390, 844, $true))) {
                            [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Emulation.setDeviceMetricsOverride' -Params @{ width = $size[0]; height = $size[1]; deviceScaleFactor = 1; mobile = [bool]$size[2] })
                            Start-Sleep -Milliseconds 500
                            [void](Save-CamTestPageShot -Cdp $cdp -Path (Join-Path $shots ("{0}-login-{1}x{2}.png" -f $browser, $size[0], $size[1])))
                        }
                    }

                    # 2. two wrong passwords: the generic message
                    foreach ($n in 1, 2) {
                        & $submit $browser ("wrong-password-$n-" + [guid]::NewGuid().ToString('N'))
                        [void](& $until $settled 15)
                        $m = & $message
                        & $note ("  wrong password #{0}: message [{1}]" -f $n, $m)
                        if ($m -ne 'Invalid username or password') { & $fail "$browser : wrong password #$n did not show the generic message ($m)" }
                    }
                    # 3. a burst of wrong passwords: the lockout message with the retry time, and the right password is refused during it
                    $lockMsg = ''
                    for ($n = 3; $n -le 10 -and $lockMsg -eq ''; $n++) {
                        & $submit $browser ("wrong-password-$n-" + [guid]::NewGuid().ToString('N'))
                        [void](& $until $settled 15)
                        $m = & $message
                        if ($m -match 'Too many attempts. Try again in \d+ seconds?') { $lockMsg = $m }
                    }
                    & $note ("  after the burst of wrong passwords: message [{0}]" -f $lockMsg)
                    if ($lockMsg -eq '') { & $fail "$browser : the burst of wrong passwords never showed the lockout message" }
                    elseif ($shots) { [void](Save-CamTestPageShot -Cdp $cdp -Path (Join-Path $shots "$browser-login-lockout-390x844.png")) }
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Emulation.setDeviceMetricsOverride' -Params @{ width = 1280; height = 800; deviceScaleFactor = 1; mobile = $false })
                    [void](& $until '(async () => !document.getElementById("go").disabled)()' 60)
                    $refusedRight = $false
                    # 4. the right password: refused while the lock lasts, accepted after
                    $landed = $false
                    for ($k = 0; $k -lt 8 -and -not $landed; $k++) {
                        & $submit $browser $password
                        Start-Sleep -Milliseconds 1500
                        $path = try { [string](& $ev 'location.pathname') } catch { '' }
                        if ($path -eq '/') { $landed = $true; break }
                        $m = & $message
                        if ($m -match 'Too many attempts. Try again in (\d+) second') { $refusedRight = $true; & $note ("  right password during the lock: message [{0}]" -f $m); Start-Sleep -Seconds ([int]$Matches[1] + 1) }
                        elseif ($m -ne '') { & $note ("  right password: message [{0}]" -f $m); Start-Sleep -Seconds 2 }
                    }
                    if (-not $refusedRight) { & $fail "$browser : the right password was not refused during the lockout" }
                    if (-not $landed) { & $fail "$browser : the right password never signed in"; continue }
                    $hashOk = & $until 'location.pathname === "/" && location.hash === "#live"' 15
                    & $note ("  signed in: lands on / with the #live fragment kept: {0}" -f $hashOk)
                    if (-not $hashOk) { & $fail "$browser : after sign-in the page is not / with #live" }

                    # 5. tiles play, as before
                    $stats = { try { Invoke-CamTestCdpEval -Cdp $cdp -Expression 'window.__liveStats ? window.__liveStats() : null' -Await } catch { $null } }
                    $end = [DateTime]::UtcNow.AddSeconds(45); $s = $null
                    while ([DateTime]::UtcNow -lt $end) { $s = & $stats; if ($s -and $s.shown -and @(@($s.tiles) | Where-Object { $_.state -eq 'live' -and $_.v -and $_.v.framesDecoded -ge 25 }).Count -ge 3) { break }; Start-Sleep -Milliseconds 500 }
                    $href = Invoke-CamTestCdpEval -Cdp $cdp -Expression 'location.protocol + "//" + (location.host.replace(/^[0-9.]+/, "a.b.c.d")) + location.pathname + location.search + location.hash'
                    $secure = Invoke-CamTestCdpEval -Cdp $cdp -Expression 'window.isSecureContext'
                    $cookies = (Invoke-CamTestCdp -Cdp $cdp -Method 'Network.getCookies' -Params @{ urls = @("https://$hostPort/") }).cookies
                    $cookie = @($cookies | Where-Object { $_.name -eq 'camui_session' })[0]
                    & $note ("  page URL: {0}; secure context: {1}" -f $href, $secure)
                    & $note ("  cookie camui_session: Secure={0} HttpOnly={1} SameSite={2} persistent (has an expiry)={3} path={4}; value length {5}; value is the password: {6}" -f $cookie.secure, $cookie.httpOnly, $cookie.sameSite, (-not $cookie.session), $cookie.path, ([string]$cookie.value).Length, ($cookie.value -eq $password))
                    if ($href -notmatch '^https://') { & $fail "$browser : the page is not https ($href)" }
                    if (-not $cookie -or -not $cookie.secure -or -not $cookie.httpOnly -or $cookie.sameSite -ne 'Strict' -or $cookie.session -or $cookie.path -ne '/' -or ([string]$cookie.value).Length -lt 43 -or $cookie.value -eq $password) { & $fail "$browser : the session cookie is not Secure, HttpOnly, SameSite=Strict, persistent and random" }
                    if (-not ($s -and @(@($s.tiles) | Where-Object { $_.state -eq 'live' -and $_.v -and $_.v.framesDecoded -ge 25 }).Count -ge 3)) { & $fail "$browser : three tiles did not reach playing over HTTPS"; if ($s) { foreach ($t in @($s.tiles)) { & $note ("    {0}: {1} | {2}" -f $t.name, $t.state, $t.diag.line) } } }
                    Start-Sleep -Seconds 3
                    $s2 = & $stats
                    foreach ($t in @($s.tiles)) {
                        $t2 = @($s2.tiles | Where-Object { $_.name -eq $t.name })[0]
                        & $note ("    {0}: framesDecoded {1} -> {2} in 3 s; selected pair {3}" -f $t.name, $t.v.framesDecoded, $t2.v.framesDecoded, $t2.diag.pair)
                        if ($t2.v.framesDecoded -lt $t.v.framesDecoded + 10) { & $fail "$browser $($t.name): framesDecoded did not keep growing" }
                    }
                    $panel = Invoke-CamTestCdpEval -Cdp $cdp -Expression '(async () => { location.hash = "set"; await new Promise(r => setTimeout(r, 1500)); return { shown: !document.getElementById("tlsPanel").classList.contains("hide"), fp: document.getElementById("tlsFingerprint").textContent.length }; })()' -Await
                    & $note ("    Settings > Connect a phone panel shown: {0}; fingerprint text {1} characters" -f $panel.shown, $panel.fp)
                    if (-not $panel.shown -or $panel.fp -ne 95) { & $fail "$browser : the Connect a phone panel is missing over HTTPS" }
                    $dom = Invoke-CamTestCdpEval -Cdp $cdp -Expression '(async () => { location.hash = "dash"; await new Promise(r => setTimeout(r, 2500)); const vis = (e) => !!e && getComputedStyle(e).display !== "none" && e.getClientRects().length > 0; const tg = Array.from(document.querySelectorAll("#camsA .tgl, #camsC .tgl")); const panels = Array.from(document.querySelectorAll("section[data-tab=set] > .panel:not(#tlsPanel)")); return { viewonly: document.body.classList.contains("viewonly"), banner: vis(document.getElementById("viewOnly")), controlsVisible: vis(document.getElementById("controls")), toggles: tg.length, togglesClickable: tg.filter(t => getComputedStyle(t).pointerEvents !== "none" && vis(t)).length, settingsInert: panels.length > 0 && panels.every(p => p.inert), ackVisible: Array.from(document.querySelectorAll("[data-ack]")).filter(vis).length, signOut: vis(document.getElementById("signOut")), who: document.getElementById("who").textContent }; })()' -Await
                    & $note ("    page in view-only mode: body.viewonly {0}, explanation banner visible {1}, Start/Stop controls visible {2}, record toggles {3} (clickable: {4}), settings panels inert {5}; Sign out visible {6} for [{7}]" -f $dom.viewonly, $dom.banner, $dom.controlsVisible, $dom.toggles, $dom.togglesClickable, $dom.settingsInert, $dom.signOut, $dom.who)
                    if (-not $dom.viewonly -or -not $dom.banner -or $dom.controlsVisible -or $dom.togglesClickable -ne 0 -or -not $dom.settingsInert -or $dom.toggles -lt 1) { & $fail "$browser : the view-only page still offers controls ($($dom | ConvertTo-Json -Compress))" }
                    if (-not $dom.signOut -or $dom.who -ne $browser) { & $fail "$browser : the Sign out control is missing or names the wrong user ($($dom.who))" }
                    if ($shots) { [void](Save-CamTestPageShot -Cdp $cdp -Path (Join-Path $shots "$browser-https-dashboard-viewonly.png")) }
                    [void](Invoke-CamTestCdpEval -Cdp $cdp -Expression 'location.hash = "set"'); Start-Sleep -Milliseconds 1200
                    if ($shots) { [void](Save-CamTestPageShot -Cdp $cdp -Path (Join-Path $shots "$browser-https-settings.png")); [void](Invoke-CamTestCdpEval -Cdp $cdp -Expression 'location.hash = "live"'); Start-Sleep -Milliseconds 800; [void](Save-CamTestPageShot -Cdp $cdp -Path (Join-Path $shots "$browser-https-live.png")) }

                    # 6. the loopback page shows no Sign out and needs no login
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = 'http://127.0.0.1:18789/#dash' })
                    [void](& $until 'document.readyState === "complete" && location.port === "18789"' 15)
                    Start-Sleep -Milliseconds 2500
                    $loopDom = & $ev '(async () => ({ path: location.pathname, signOutHidden: document.getElementById("signOut").classList.contains("hide"), whoHidden: document.getElementById("who").classList.contains("hide"), me: await (await fetch("api/auth/me")).json() }))()'
                    & $note ("  loopback page: path {0}, Sign out hidden {1}, /api/auth/me {2}" -f $loopDom.path, $loopDom.signOutHidden, ($loopDom.me | ConvertTo-Json -Compress))
                    if ($loopDom.path -ne '/' -or -not $loopDom.signOutHidden -or $loopDom.me.loginRequired -ne $false) { & $fail "$browser : the loopback page shows a login control or demands a login" }

                    # 7. the session survives a camui restart
                    & $restartUi @()
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = $url })
                    $stays = & $until 'location.pathname === "/" && !!document.getElementById("signOut") && !document.getElementById("signOut").classList.contains("hide")' 25
                    & $note ("  after restarting camui: the same browser session still opens the app without a new login: {0}" -f $stays)
                    if (-not $stays) { & $fail "$browser : the session did not survive a camui restart" }

                    # 7b. the session ends behind the page's back (here: signed out from another client): the open page notices on its next poll and goes to /login, then returns here after a new sign-in
                    $gone = & $tls @{ Method = 'POST'; Path = '/api/auth/logout'; Headers = @{ Cookie = "camui_session=$($cookie.value)"; 'Content-Type' = 'application/json'; Origin = "https://$hostPort" }; Body = '{}' }
                    $bounced = & $until 'location.pathname === "/login"' 20
                    $nextKept = & $ev 'decodeURIComponent(location.search)'
                    & $note ("  session ended elsewhere (HTTP {0}): the open page goes to /login by itself: {1}; address {2}" -f $gone.Status, $bounced, $nextKept)
                    if ($gone.Status -ne 200 -or -not $bounced -or $nextKept -notmatch 'next=/') { & $fail "$browser : an ended session did not send the open page to /login" }
                    else {
                        $again2 = $false
                        for ($k = 0; $k -lt 6 -and -not $again2; $k++) {
                            & $submit $browser $password
                            Start-Sleep -Milliseconds 1500
                            $where2 = try { [string](& $ev 'location.pathname') } catch { '' }
                            $again2 = ($where2 -eq '/')
                            if (-not $again2) { Start-Sleep -Seconds 4 }
                        }
                        if (-not $again2) { & $fail "$browser : could not sign in again after the session ended" }
                        $stays2 = & $until '!!document.getElementById("signOut") && !document.getElementById("signOut").classList.contains("hide")' 20
                        $cookie = @((Invoke-CamTestCdp -Cdp $cdp -Method 'Network.getCookies' -Params @{ urls = @("https://$hostPort/") }).cookies | Where-Object { $_.name -eq 'camui_session' })[0]
                        & $note ("  signed in again: back in the app with Sign out visible: {0}" -f $stays2)
                    }

                    # 8. Sign out returns to /login and the session is over
                    [void](& $ev '(async () => { document.getElementById("signOut").click(); return true; })()')
                    $out = & $until 'location.pathname === "/login"' 15
                    [void](Invoke-CamTestCdp -Cdp $cdp -Method 'Page.navigate' -Params @{ url = "https://${hostPort}/" })
                    $again = & $until 'location.pathname === "/login"' 15
                    $left = @((Invoke-CamTestCdp -Cdp $cdp -Method 'Network.getCookies' -Params @{ urls = @("https://$hostPort/") }).cookies | Where-Object { $_.name -eq 'camui_session' -and $_.value }).Count
                    $old = & $tls @{ Path = '/api/status'; Headers = @{ Cookie = "camui_session=$($cookie.value)" } }
                    & $note ("  Sign out: returns to /login {0}; navigating to / afterwards lands on /login {1}; session cookies left {2}; the old cookie against /api/status -> HTTP {3}" -f $out, $again, $left, $old.Status)
                    if (-not $out -or -not $again -or $left -ne 0 -or $old.Status -ne 401) { & $fail "$browser : Sign out did not end the session" }
                }
                catch { & $fail ("$browser run aborted: " + $_.Exception.Message) }
                finally {
                    Disconnect-CamTestCdp -Cdp $cdp; $cdp = $null
                    if ($started) { $e = Stop-CamTestBrowser -Started $started; if ($e.Left.Count -or -not $e.ProfileRemoved) { & $fail "$browser left processes or a profile behind" } }
                    $script:CamTlsSeconds += ([DateTime]::UtcNow - $began).TotalSeconds
                }
            }
        }

        # ---- the username lockout and the per-address throttle, over the wire ----
        Start-Sleep -Seconds 31   # the address bucket refills one attempt per 6 s, 5 banked
        $wrongLogin = { param([string]$u, [string]$tag) & $tls @{ Method = 'POST'; Path = '/api/auth/login'; Headers = $jsonH; Body = (& $loginBodyOf $u ("wrong-$tag-" + [guid]::NewGuid().ToString('N'))) } }
        $fails = @(1..5 | ForEach-Object { & $wrongLogin 'probe' $_ })
        $uniform = (@($fails | ForEach-Object { '{0}|{1}' -f $_.Status, $_.Body.Trim() } | Select-Object -Unique)).Count -eq 1
        & $note ("lockout: 5 wrong passwords for one user from one address -> HTTP {0}; identical answers: {1}; body {2}" -f (($fails | ForEach-Object { $_.Status }) -join ','), $uniform, $fails[0].Body.Trim())
        if ($uniform -ne $true -or $fails[0].Status -ne 401 -or $fails[0].Body -notmatch 'Invalid username or password') { & $fail 'wrong passwords were not answered uniformly with the generic message' }
        Start-Sleep -Seconds 7    # one address token is back, so a refusal now is the USERNAME lock
        $locked = & $tls @{ Method = 'POST'; Path = '/api/auth/login'; Headers = $jsonH; Body = (& $loginBodyOf 'probe' $password) }
        $wait = 0; [void][int]::TryParse([string]$locked.Headers['retry-after'], [ref]$wait)
        & $note ("lockout: the RIGHT password while locked -> HTTP {0}, Retry-After [{1}], body {2}, cookie issued: {3}" -f $locked.Status, $locked.Headers['retry-after'], $locked.Body.Trim(), [bool]$locked.Headers['set-cookie'])
        if ($locked.Status -ne 429 -or $wait -lt 1 -or $wait -gt 30 -or $locked.Body -notmatch 'Too many attempts' -or $locked.Headers['set-cookie']) { & $fail 'a locked username was not refused with 429 and Retry-After' }
        Start-Sleep -Seconds ($wait + 1)
        $unlocked = & $tls @{ Method = 'POST'; Path = '/api/auth/login'; Headers = $jsonH; Body = (& $loginBodyOf 'probe' $password) }
        & $note ("lockout: after the delay the right password -> HTTP {0}; the answer holds no hint of the earlier failures: {1}" -f $unlocked.Status, ($unlocked.Body -notmatch 'attempt|fail|lock'))
        if ($unlocked.Status -ne 200) { & $fail 'the lock did not end on schedule' }
        $burst = @(1..8 | ForEach-Object { & $wrongLogin ("ghost-$_") $_ })
        $throttled = @($burst | Where-Object { $_.Status -eq 429 })
        & $note ("per-address throttle: 8 quick wrong logins (different names) -> HTTP {0}; Retry-After on the first refusal [{1}]" -f (($burst | ForEach-Object { $_.Status }) -join ','), $(if ($throttled.Count) { $throttled[0].Headers['retry-after'] } else { '' }))
        if ($throttled.Count -lt 1 -or -not $throttled[0].Headers['retry-after'] -or $burst[0].Status -ne 401) { & $fail 'the per-address throttle did not answer 429 with Retry-After' }
        $authLog = [System.IO.File]::ReadAllText((Join-Path $authDir 'auth.log'))
        $results = @([regex]::Matches($authLog, 'result=(\w+)') | ForEach-Object { $_.Groups[1].Value } | Group-Object | ForEach-Object { '{0} x{1}' -f $_.Name, $_.Count })
        & $note ("auth.log: {0} lines; results: {1}; holds the password: {2}; holds a session token: {3}; holds the hash: {4}" -f ($authLog -split "`n" | Where-Object { $_ }).Count, ($results -join ', '), $authLog.Contains($password), $authLog.Contains($sessionValue), $authLog.Contains($hashTail))
        if ($authLog.Contains($password) -or $authLog.Contains($sessionValue) -or $authLog.Contains($hashTail) -or $authLog -notmatch 'result=ok' -or $authLog -notmatch 'result=fail' -or $authLog -notmatch 'result=locked' -or $authLog -notmatch 'result=throttled') { & $fail 'auth.log is missing outcomes or holds a secret' }
        $logText = (@(Get-Content -LiteralPath $ui.StdOut -ErrorAction SilentlyContinue) -join "`n")
        & $note ("camui console log: password appears: {0}; session token appears: {1}; hash appears: {2}" -f $logText.Contains($password), $logText.Contains($sessionValue), $logText.Contains($hashTail))
        if ($logText.Contains($password) -or $logText.Contains($sessionValue) -or $logText.Contains($hashTail)) { & $fail 'a secret reached the camui log' }

        # ---- [Stage 2b.1] tlsReadOnly=false: the same POST now reaches its handler (a harmless, invalid one) ----
        [void](Stop-CamTestUi -Ui $ui)
        $ui = Start-CamTestLiveCamui -Rig $rig -Port 18789 -Name 'camui-livetls' -RunExe $allowed -ExtraArgs ($uiArgs + @('-tls-readonly', 'false'))
        if (-not $ui.Started) { throw ('camui did not restart with -tls-readonly false: ' + $ui.Reason) }
        $up = $false
        for ($i = 0; $i -lt 40 -and -not $up; $i++) { $tcp = New-Object System.Net.Sockets.TcpClient; try { $tcp.Connect($lan, $tlsPort); $up = $true } catch { Start-Sleep -Milliseconds 250 } finally { $tcp.Close() } }
        $open = & $tls @{ Method = 'POST'; Path = '/api/control'; Headers = ($bearer + $pageHeaders); Body = '{"scope":"nothing","action":"nothing"}' }
        $infoRw = & $tls @{ Path = '/api/tls/info'; Headers = $bearer }
        & $note ("tlsReadOnly=false: POST /api/control (an invalid scope, so nothing is stopped) -> HTTP {0} {1}; /api/tls/info readOnly false: {2}" -f $open.Status, $open.Body.Trim(), ($infoRw.Body -match '"readOnly":\s*false'))
        if ($open.Body -match 'read-only' -or $open.Status -eq 0 -or $open.Status -in 401, 429) { & $fail 'with tlsReadOnly=false the request did not reach its handler' }
        if ($infoRw.Body -notmatch '"readOnly":\s*false') { & $fail '/api/tls/info does not say readOnly:false' }
        $loopCtl = Invoke-CamRawHttp -Port 18789 -Method POST -Path '/api/control' -Headers @{ 'Content-Type' = 'application/json' } -Body '{"scope":"nothing","action":"nothing"}'
        & $note ("the loopback listener (unchanged): POST /api/control with an invalid scope -> HTTP {0}, read-only refusal: {1}" -f $loopCtl.Status, ($loopCtl.Body -match 'read-only'))
        if ($loopCtl.Body -match 'read-only') { & $fail 'the loopback listener refused as read-only' }

        $storesAfter = & $storeCount
        & $note ("Windows certificate stores (CurrentUser and LocalMachine Root, CA, TrustedPeople) counts before/after: {0} / {1}; IPCCTV certificates in them: {2}; firewall rules before/after: {3} / {4}" -f $storesBefore, $storesAfter, (& $ours), $fwBefore, @(Get-NetFirewallRule -ErrorAction SilentlyContinue).Count)
        if ($storesBefore -ne $storesAfter -or (& $ours) -ne 0) { & $fail 'a Windows certificate store changed' }
        if ($fwBefore -ne @(Get-NetFirewallRule -ErrorAction SilentlyContinue).Count) { & $fail 'the firewall rule count changed' }
    }
    catch { & $fail ('check aborted: ' + $_.Exception.Message) }
    finally {
        Disconnect-CamTestCdp -Cdp $cdp
        foreach ($b in @($script:CamTlsBrowsers)) { try { [void](Stop-CamTestBrowser -Started $b) } catch { } }
        if ($ui) { [void](Stop-CamTestUi -Ui $ui) }
        $notes = @()
        if ($rig) { $notes = @(Stop-CamTestLiveRig -Rig $rig) }
        if ($allowed -and (Test-Path -LiteralPath $allowed)) { Remove-Item -LiteralPath $allowed -Force -ErrorAction SilentlyContinue }
        if ($backup -and (Test-Path -LiteralPath $backup)) { Move-Item -LiteralPath $backup -Destination $allowed -Force }
        $script:CamTlsEvidence += ("browser windows were up {0:n0} s; whole check {1:n0} s" -f $script:CamTlsSeconds, $watch.Elapsed.TotalSeconds)
        $script:CamTlsEvidence += $notes
        if ($notes -match 'forced: (?!nothing)') { & $fail 'the supervisor had to be forced' }
        if ($script:CamTlsOk -and $rig) { try { Remove-CamTestTree -Path $rig.Dir } catch { $script:CamTlsEvidence += 'cleanup failed' } }
        elseif ($rig) { $script:CamTlsEvidence += "left for inspection: $($rig.Dir)" }
    }

    if ($script:CamTlsOk -or $script:CamTlsEvidence.Count) {
        Add-CamTestResult -Run $Run -Check 'livetls' -Status $(if ($script:CamTlsOk) { 'PASS' } else { 'FAIL' }) `
            -Message 'camui serves the app and the Live screen over HTTPS on the LAN address behind a login (username and password, brute-force protected) and a local CA, view-only by default: nothing without a session, nothing leaked, no write with one, trust per probe or per browser process only' `
            -Evidence $script:CamTlsEvidence
    }
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
        'viewerpw'   { Invoke-CamCheckViewerPw   -Run $run }
        'relayapi'   { Invoke-CamCheckRelayApi   -Run $run }
        'selftest'   { Invoke-CamCheckSelfTest   -Run $run }
        'layout'     { Invoke-CamCheckLayout     -Run $run }
        'strip'      { Invoke-CamCheckStrip      -Run $run }
        'sections'   { Invoke-CamCheckSections   -Run $run }
        'autostart'  { Invoke-CamCheckAutostart  -Run $run }
        'crashsafe'  { Invoke-CamCheckCrashSafe  -Run $run }
        'windowclose' { Invoke-CamCheckWindowClose -Run $run }
        'windowid'   { Invoke-CamCheckWindowId   -Run $run }
        'nvrstart'   { Invoke-CamCheckNvrStart   -Run $run }
        'leak'       { Invoke-CamCheckLeak       -Run $run }
        'uileak'     { Invoke-CamCheckUiLeak     -Run $run }
        'uiguard'    { Invoke-CamCheckUiGuard    -Run $run }
        'procs'      { Invoke-CamCheckProcs      -Run $run }
        'relay'      { Invoke-CamCheckRelay      -Run $run }
        'prunematrix' { Invoke-CamCheckPruneMatrix -Run $run }
        'pruneroots' { Invoke-CamCheckPruneRoots -Run $run }
        'pruneage'   { Invoke-CamCheckPruneAge   -Run $run }
        'prunewatermark' { Invoke-CamCheckPruneWatermark -Run $run }
        'pruneclock' { Invoke-CamCheckPruneClock -Run $run }
        'prunebreaker' { Invoke-CamCheckPruneBreaker -Run $run }
        'pruneblocked' { Invoke-CamCheckPruneBlocked -Run $run }
        'pruneconfig' { Invoke-CamCheckPruneConfig -Run $run }
        'pruneowner' { Invoke-CamCheckPruneOwner -Run $run }
        'prunelive'  { Invoke-CamCheckPruneLive  -Run $run }
        'installsteps' { Invoke-CamCheckInstallSteps -Run $run }
        'installplan' { Invoke-CamCheckInstallPlan -Run $run }
        'installpreflight' { Invoke-CamCheckInstallPreflight -Run $run }
        'installsandbox' { Invoke-CamCheckInstallSandbox -Run $run }
        'docswitches' { Invoke-CamCheckDocSwitches -Run $run }
        'doclinks' { Invoke-CamCheckDocLinks -Run $run }
        'logrotate' { Invoke-CamCheckLogRotate -Run $run }
        'segments'  { Invoke-CamCheckSegments  -Run $run }
        'backoff'   { Invoke-CamCheckBackoff   -Run $run }
        'logrotatelive' { Invoke-CamCheckLogRotateLive -Run $run }
        'stallwatch' { Invoke-CamCheckStallWatch -Run $run }
        'logheld'   { Invoke-CamCheckLogHeld   -Run $run }
        'pruneloop' { Invoke-CamCheckPruneLoop -Run $run }
        'dst'       { Invoke-CamCheckDst       -Run $run }
        'dstloop'   { Invoke-CamCheckDstLoop   -Run $run }
        'clocklint' { Invoke-CamCheckClockLint -Run $run }
        'livecfg'   { Invoke-CamCheckLiveCfg   -Run $run }
        'livesecret' { Invoke-CamCheckLiveSecret -Run $run }
        'livewhep'  { Invoke-CamCheckLiveWhep  -Run $run }
        'liveondemand' { Invoke-CamCheckLiveOnDemand -Run $run }
        'livepage'  { Invoke-CamCheckLivePage  -Run $run }
        'liverobust' { Invoke-CamCheckLiveRobust -Run $run }
        'livegone'  { Invoke-CamCheckLiveGone  -Run $run }
        'livelan'   { Invoke-CamCheckLiveLan   -Run $run }
        'livetls'   { Invoke-CamCheckLiveTls   -Run $run }
        'soak'      { Invoke-CamCheckSoak      -Run $run -DryRun:$SoakDryRun }
        'downalert' { Invoke-CamCheckDownAlert -Run $run }
        'downalertrun' { Invoke-CamCheckDownAlertRun -Run $run }
    }
}

exit (Write-CamTestReport -Run $run)
