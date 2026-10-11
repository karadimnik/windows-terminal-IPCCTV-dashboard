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
    .\tests\cam.tests.ps1 -Checks parse,strip

.EXAMPLE
    .\tests\cam.tests.ps1 -Live -Checks relay
#>
[CmdletBinding()]
param(
    # Which checks to run. Default: every check that touches no camera.
    [ValidateSet('parse', 'cameralist', 'selftest', 'strip', 'leak', 'procs', 'relay')]
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

$cameraFree = @('parse', 'cameralist', 'selftest', 'strip', 'leak', 'procs')
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


function Invoke-CamCheckSelfTest {
    param($Run)

    $result = Invoke-CamTestScript -Path (Join-Path -Path $repo -ChildPath 'cam.recorder.ps1') `
        -Arguments @('-SelfTest', '-Offline') -TimeoutSeconds 300
    $summary = @($result.Lines | Where-Object { $_ -match 'passed|failed|FAIL' })
    Add-CamTestResult -Run $Run -Check 'selftest' -Status $(if ($result.ExitCode -eq 0) { 'PASS' } else { 'FAIL' }) `
        -Message ("cam.recorder.ps1 -SelfTest -Offline exited {0}" -f $result.ExitCode) `
        -Evidence @($summary | Select-Object -Last 6)
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
                OldestSegment = $null; NewestSegment = $null
                MeasuredGBPerDay = 99.7; ProjectedDaysRemaining = 2.82; ScannedAt = $null
            }
            Prune          = [pscustomobject]@{ LastRunAt = $null; DeletedTotal = 0; FreedGBTotal = 0; Blocked = $false; LastMessage = '' }
            Restreamer     = [pscustomobject]@{
                Enabled = $true; RelayBase = 'rtsp://127.0.0.1:8554'
                Pid = $(if ($RelayAlive) { 9100 } else { $null }); LastPid = 9100; Alive = $RelayAlive
                StartedAt = $null; RestartsTotal = 1; Paths = @('frontentrance', 'rearbalcony', 'fieldfront', 'cargarage', 'workshopentrance', 'workshop')
                LastError = ''; LastErrorAt = $null
            }
            Cameras        = $rows
        }
    }

    function Set-Fixture {
        param($Document)
        [System.IO.File]::WriteAllText($statusPath, (ConvertTo-Json -InputObject $Document -Depth 6), (New-Object System.Text.UTF8Encoding($false)))
    }

    $evidence = @()
    $ok = $true

    # --- healthy
    Set-Fixture -Document (New-Fixture -State 'running' -UpdatedAt ([DateTime]::Now) -SupervisorPid $PID -RelayAlive $true)
    $render = Invoke-CamTestScript -Path $sandbox.Health -Arguments @('-Once', '-Rows', '8', '-Width', '120')
    $lines = @($render.Output -split "`r?`n")
    if ($lines.Count -gt 0 -and $lines[-1] -eq '') { $lines = $lines[0..($lines.Count - 2)] }
    $widest = 0
    foreach ($line in $lines) { if ($line.Length -gt $widest) { $widest = $line.Length } }

    if ($render.ExitCode -ne 0) { $ok = $false }
    if ($lines.Count -ne 8) { $ok = $false }
    if ($widest -gt 120) { $ok = $false }
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
    $evidence += ("healthy: exit {0}, {1} row(s), widest {2} col(s)" -f $render.ExitCode, $lines.Count, $widest)
    $evidence += @($lines | ForEach-Object { '  ' + $_.TrimEnd() })

    # --- stale: a clean shutdown is recorded, so no supervisor is behind it
    Set-Fixture -Document (New-Fixture -State 'stopped' -UpdatedAt ([DateTime]::Now.AddMinutes(-9)) -SupervisorPid $PID -RelayAlive $false)
    $render = Invoke-CamTestScript -Path $sandbox.Health -Arguments @('-Once', '-Rows', '8', '-Width', '120')
    $staleLines = @($render.Output -split "`r?`n")
    if ($staleLines.Count -gt 0 -and $staleLines[-1] -eq '') { $staleLines = $staleLines[0..($staleLines.Count - 2)] }
    if ($staleLines.Count -ne 8) { $ok = $false }
    if ($staleLines[0] -notmatch '^STALE - NO SUPERVISOR') { $ok = $false }
    if ($staleLines[0].TrimEnd() -notmatch 'last known free [\d.]+ GB \(reserve [\d.]+\)$') { $ok = $false }
    $evidence += ("stale: exit {0}, {1} row(s)" -f $render.ExitCode, $staleLines.Count)
    $evidence += @($staleLines | ForEach-Object { '  ' + $_.TrimEnd() })

    # --- no status file at all
    Remove-Item -LiteralPath $statusPath -Force
    $render = Invoke-CamTestScript -Path $sandbox.Health -Arguments @('-Once', '-Rows', '8', '-Width', '120')
    $missingLines = @($render.Output -split "`r?`n" | Where-Object { $_.Trim().Length -gt 0 })
    if ($missingLines.Count -eq 0 -or $missingLines[0] -notmatch '^NO STATUS FILE') { $ok = $false }
    $evidence += ("no status file: {0}" -f $missingLines[0].TrimEnd())

    Add-CamTestResult -Run $Run -Check 'strip' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message 'cam.health.ps1 renders 8 rows within 120 columns, and says so when the snapshot is stale or absent' `
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
    param($Run)

    $counts = Get-CamTestProcessCount
    $ok = ($counts.Ffmpeg -eq 0 -and $counts.MediaMtx -eq 0)
    Add-CamTestResult -Run $Run -Check 'procs' -Status $(if ($ok) { 'PASS' } else { 'FAIL' }) `
        -Message ("no recorder process left running: {0}" -f $counts.Text) `
        -Evidence @(("ffmpeg ids: {0}" -f (@($counts.FfmpegIds) -join ' ')), ("mediamtx ids: {0}" -f (@($counts.MediaMtxIds) -join ' ')))
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


foreach ($check in $Checks) {
    if ($check -eq 'relay' -and -not $Live) {
        Add-CamTestResult -Run $run -Check 'relay' -Status 'SKIP' `
            -Message 'needs -Live: it opens RTSP sessions against the real cameras'
        continue
    }
    switch ($check) {
        'parse'      { Invoke-CamCheckParse      -Run $run }
        'cameralist' { Invoke-CamCheckCameraList -Run $run }
        'selftest'   { Invoke-CamCheckSelfTest   -Run $run }
        'strip'      { Invoke-CamCheckStrip      -Run $run }
        'leak'       { Invoke-CamCheckLeak       -Run $run }
        'procs'      { Invoke-CamCheckProcs      -Run $run }
        'relay'      { Invoke-CamCheckRelay      -Run $run }
    }
}

exit (Write-CamTestReport -Run $run)
