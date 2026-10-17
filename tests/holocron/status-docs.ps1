<#
    status-docs.ps1 - produces status.json documents with the PowerShell product's
    own writers: Write-CamStatusFile (cam.recorder.ps1) and the two patch
    functions Set-CamStatusUnexpectedStop / Set-CamStatusWindowClosed
    (cam.common.ps1). Dot-source holocron-lib.ps1 first.

    The inputs are synthetic (cameras 'Camera One' ..., documentation addresses
    192.0.2.x, user fakeuser / password fakepass). No real archive, camera list,
    config or credential is read, and nothing is written outside the folders
    given.

    -Deterministic replaces the three machine readings - the clock, the free
    space, and (afterwards, as text) the supervisor's own pid - with fixed
    values, which is what the committed fixtures need. Without it the documents
    carry the real clock, real free space and the real pid: that is the
    producer check (status-conformance.ps1), which proves the writer as it
    really runs satisfies the schema.
#>

function New-HolocronFakeProcess {
    param([int]$Id, [bool]$Exited = $false)
    return [pscustomobject]@{ Id = $Id; HasExited = $Exited }
}


function New-HolocronFakeScan {
    param([string]$Current, $CurrentBytes, [DateTime]$NewestClosedWrite, [int]$Files, [long]$Bytes)
    $scan = [pscustomobject]@{ Current = $Current; CurrentBytes = $CurrentBytes; NewestClosedWrite = $NewestClosedWrite; Files = $Files; Bytes = $Bytes }
    if ($NewestClosedWrite -eq [DateTime]::MinValue) { $scan.NewestClosedWrite = $null }
    return $scan
}


function Edit-HolocronText {
    <# One regex replacement, the first match at or after -After (a literal marker), failing when nothing matched. #>
    param([string]$Text, [string]$Pattern, [string]$Replacement, [string]$After = '')
    $start = 0
    if ($After) {
        $start = $Text.IndexOf($After, [StringComparison]::Ordinal)
        if ($start -lt 0) { throw "marker '$After' not found" }
    }
    $regex = New-Object System.Text.RegularExpressions.Regex($Pattern)
    $match = $regex.Match($Text, $start)
    if (-not $match.Success) { throw "pattern '$Pattern' not found" }
    return $Text.Substring(0, $match.Index) + $match.Result($Replacement) + $Text.Substring($match.Index + $match.Length)
}


function New-HolocronStatusDocuments {
    <#
        Writes the status documents into -OutDir and returns an object with what
        was observed about the two known differences (F1 and the missing
        'window-closed' in the writer's ValidateSet).
    #>
    param(
        [Parameter(Mandatory = $true)][string]$OutDir,
        [switch]$Deterministic
    )

    . (Get-CamTestRecorderFunctions -Name 'Write-CamStatusFile')

    $script:HolocronClock = [DateTime]::UtcNow
    if ($Deterministic) {
        $script:HolocronClock = [DateTime]::new(2026, 10, 17, 9, 30, 0, [DateTimeKind]::Utc).AddTicks(1234567)
        function Get-CamWallClock { return $script:HolocronClock }
        function Get-CamFreeSpaceGB { param($Path) return 123.45 }
    }
    $pidText = $(if ($Deterministic) { '4242' } else { [string]$PID })

    Register-CamSecret -Value 'fakepass'

    $sandbox = New-HolocronSandbox
    try {
        $root = $(if ($Deterministic) { 'D:\holocron-fixture\cameras' } else { Join-Path $sandbox 'cameras' })
        $statePath = Join-Path $sandbox '_state\status.json'
        $patchConfig = @{ RootPath = $sandbox }
        $logPath = Join-Path $sandbox 'recorder.log'
        $stamp = { param([DateTime]$utc) $utc.ToLocalTime().ToString('yyyy-MM-dd HH:mm:ss zzz') }

        $config = Get-CamConfigDefault
        $config.RootPath = $root
        $config.RestreamerEnabled = $true
        $config.ReserveGB = 20
        $config.PruneHysteresisGB = 5

        $now = $script:HolocronClock
        $startedAt = $now.AddHours(-2).AddSeconds(-7)

        # The supervisor log whose tail becomes Events (newest first in the document).
        $logLines = @(
            ((& $stamp $now.AddMinutes(-40)) + ' supervisor started, pid ' + $pidText),
            ((& $stamp $now.AddMinutes(-39)) + ' camera Camera One: ffmpeg started on the relay'),
            ((& $stamp $now.AddMinutes(-12)) + ' camera Camera Two: ffmpeg exited with code 1 after 3s (rtsp://fakeuser:fakepass@192.0.2.12/stream1)'),
            'a line that does not carry the log time shape',
            ((& $stamp $now.AddMinutes(-2)) + ' pruned 3 files, freed 1.2 GB; used fakepass for nothing'))
        Save-HolocronText -Path $logPath -Text (($logLines -join "`n") + "`n")

        # -- the cameras and what the supervisor knows about each
        $camList = @(
            [pscustomobject]@{ Name = 'Camera One'; IP = '192.0.2.11'; Quality = 'stream1'; Record = $true },
            [pscustomobject]@{ Name = 'Camera Two'; IP = '192.0.2.12'; Quality = 'stream2'; Record = $true },
            [pscustomobject]@{ Name = 'Camera Three'; IP = '192.0.2.13'; Record = $false })
        $resolved = @()
        foreach ($c in $camList) {
            $resolved += [pscustomobject]@{ Name = $c.Name; SafeName = (Get-CamSafeName -Name $c.Name); Record = $c.Record; Error = '' }
        }
        $resolved += [pscustomobject]@{ Name = 'CON'; SafeName = ''; Record = $false; Error = "Camera name 'CON' resolves to the reserved Windows device name 'CON'. Rename the camera in cameras.json." }

        $scanOne = New-HolocronFakeScan -Current 'Camera_One_20261017_121500+0300.mkv' -CurrentBytes 52428800 -NewestClosedWrite $now.AddSeconds(-312.4) -Files 12 -Bytes 1288490188
        $scanThree = New-HolocronFakeScan -Current $null -CurrentBytes $null -NewestClosedWrite $now.AddDays(-3) -Files 4 -Bytes 402653184
        $children = @{
            'Camera_One' = [pscustomobject]@{
                Scan = $scanOne; RestartCount = 2; Restarts = @($now.AddMinutes(-50), $now.AddMinutes(-90)); LastPid = 4101
                LastError = 'ffmpeg exited with code 1 after 3s'; LastErrorAt = $now.AddMinutes(-50); StartedAt = $now.AddMinutes(-39)
                Process = (New-HolocronFakeProcess -Id 4101); NextStartAt = [DateTime]::MinValue; InputMode = 'relay'; DownSince = $null }
            'Camera_Two' = [pscustomobject]@{
                Scan = $null; RestartCount = 7; Restarts = @($now.AddMinutes(-12), $now.AddMinutes(-5)); LastPid = 4102
                LastError = 'ffmpeg exited with code 1 after 3s (rtsp://fakeuser:fakepass@192.0.2.12/stream1)'; LastErrorAt = $now.AddMinutes(-5); StartedAt = $now.AddMinutes(-5)
                Process = $null; NextStartAt = $now.AddSeconds(24); InputMode = 'direct'; DownSince = $now.AddMinutes(-20) }
        }
        $archive = [pscustomobject]@{
            Bytes = 5368709120; Files = 40; Oldest = $now.AddDays(-4); Newest = $now.AddSeconds(-20); GBPerDay = 1.25; ScannedAt = $now.AddSeconds(-33)
            Dirs = @{ 'Camera_Three' = $scanThree } }
        $pruneState = [pscustomobject]@{ LastRunAt = $now.AddMinutes(-2); DeletedTotal = 3; FreedBytes = 1288490188; Blocked = $false; LastMessage = 'pruned 3 files' }

        $relayMap = Get-CamRelayPathMap -Cameras $camList
        $relay = [pscustomobject]@{
            Process = (New-HolocronFakeProcess -Id 4301); LastPid = 4301; StartedAt = $now.AddHours(-2); RestartCount = 1
            PathMap = $relayMap; LastError = 'mediamtx exited with code 1 after 5s'; LastErrorAt = $now.AddHours(-1) }

        $liveConfig = Get-CamConfigDefault
        $liveConfig.LiveViewEnabled = $true
        $livePathMap = Get-CamLivePathMap -Config $liveConfig -RelayPathMap $relayMap
        $live = [pscustomobject]@{
            Process = (New-HolocronFakeProcess -Id 4401); LastPid = 4401; StartedAt = $now.AddHours(-2); RestartCount = 0
            PathMap = $livePathMap; LastError = ''; LastErrorAt = $null }

        $saveDoc = {
            param([string]$Name, [bool]$FromWriter = $true)
            $text = Read-HolocronText -Path $statePath
            if ($Deterministic -and $FromWriter) {
                $text = [regex]::Replace($text, '(?m)^(    "SupervisorPid":\s+)' + $PID + '(?=,)', '${1}4242')
                if ($text -notmatch '"SupervisorPid":\s+4242') { throw 'could not normalise SupervisorPid' }
            }
            Save-HolocronText -Path (Join-Path $OutDir $Name) -Text $text
            return $text
        }
        $put = { param([string]$Name) [System.IO.File]::WriteAllText($statePath, (Read-HolocronText -Path (Join-Path $OutDir $Name)), (New-Object System.Text.UTF8Encoding($false))) }
        $reset = { if (Test-Path -LiteralPath $statePath) { Remove-Item -LiteralPath $statePath -Force } }

        # ---- running documents
        $full = @{ Config = $config; Path = $statePath; Cameras = $resolved; Children = $children; StartedAt = $startedAt; State = 'running'
            Archive = $archive; PruneState = $pruneState; Relay = $relay; SupervisorLogPath = $logPath; LastUnexpectedStop = $null }
        Write-CamStatusFile @full
        [void](& $saveDoc 'running-full.json')

        $liveFull = $full.Clone(); $liveFull.Config = $liveConfig.Clone(); $liveFull.Config.RootPath = $root; $liveFull.Config.RestreamerEnabled = $true; $liveFull.Live = $live
        Write-CamStatusFile @liveFull
        [void](& $saveDoc 'running-live.json')

        $first = @{ Config = $config.Clone(); Path = $statePath; Cameras = @(); Children = @{}; StartedAt = $now.AddSeconds(-1); State = 'running'; LastUnexpectedStop = $null }
        $first.Config.RestreamerEnabled = $false
        Write-CamStatusFile @first
        [void](& $saveDoc 'running-first-write.json')

        $script:HolocronClock = [DateTime]::new(2026, 11, 3, 10, 15, 30, 500, [DateTimeKind]::Utc)
        if (-not $Deterministic) { $script:HolocronClock = [DateTime]::UtcNow }
        Write-CamStatusFile @full
        [void](& $saveDoc 'running-winter-offset.json')
        $script:HolocronClock = $now

        # ---- final documents of the supervisor
        $carried = [pscustomobject]@{ At = (Format-CamTimestamp -Value $now.AddDays(-1)); Reason = 'supervisor process 3900 is gone without a final status'; SupervisorPid = 3900; DetectedBy = 'nvr host' }
        $stopped = $full.Clone(); $stopped.State = 'stopped'
        $stopped.StopKind = 'requested'
        Write-CamStatusFile @stopped
        [void](& $saveDoc 'stopped-requested.json')
        $stopped.StopKind = 'interrupted'; $stopped.LastUnexpectedStop = $carried
        Write-CamStatusFile @stopped
        [void](& $saveDoc 'stopped-interrupted-carrying-unexpected.json')
        $own = [pscustomobject]@{ At = (Format-CamTimestamp -Value $now); Reason = 'the supervisor loop threw: synthetic failure'; SupervisorPid = $PID; DetectedBy = 'supervisor' }
        if ($Deterministic) { $own.SupervisorPid = 4242 }
        $stopped.StopKind = 'unexpected'; $stopped.LastUnexpectedStop = $own
        Write-CamStatusFile @stopped
        [void](& $saveDoc 'stopped-unexpected-by-supervisor.json')

        # ---- patches by the NVR host and the warden, with and without a file to patch
        $script:HolocronClock = $now.AddMinutes(1)
        & $reset
        if (-not (Set-CamStatusUnexpectedStop -Config $patchConfig -Reason 'supervisor process 4242 is gone without a final status' -SupervisorPid 4242)) { throw 'Set-CamStatusUnexpectedStop returned false' }
        [void](& $saveDoc 'patched-unexpected-minimal.json' $false)

        & $put 'running-full.json'
        if (-not (Set-CamStatusUnexpectedStop -Config $patchConfig -Reason 'supervisor process 4242 is gone without a final status (rtsp://fakeuser:fakepass@192.0.2.12/x)' -SupervisorPid 4242)) { throw 'Set-CamStatusUnexpectedStop returned false' }
        [void](& $saveDoc 'patched-unexpected-over-running.json' $false)

        & $reset
        if (-not (Set-CamStatusWindowClosed -Config $patchConfig -Reason 'the NVR window was closed' -WindowTitle 'cam.nvr')) { throw 'Set-CamStatusWindowClosed returned false' }
        [void](& $saveDoc 'patched-windowclose-minimal.json' $false)

        & $put 'running-full.json'
        if (-not (Set-CamStatusWindowClosed -Config $patchConfig -Reason 'the NVR window was closed' -WindowTitle 'cam.nvr')) { throw 'Set-CamStatusWindowClosed returned false' }
        $closed = & $saveDoc 'patched-windowclose-over-running.json' $false

        # ---- F1: what the supervisor writes after it is started again over that file.
        # The two carry lines are the supervisor's own (cam.recorder.ps1 ~3757-3759).
        $script:HolocronClock = $now.AddMinutes(5)
        $previous = Get-CamStatusSnapshot -Config $patchConfig
        $lastUnexpectedStop = $null
        if ($previous) { $lastUnexpectedStop = $previous.LastUnexpectedStop }
        $restart = $full.Clone(); $restart.LastUnexpectedStop = $lastUnexpectedStop
        Write-CamStatusFile @restart
        $after = & $saveDoc 'restarted-after-windowclose.json'
        $f1 = [pscustomobject]@{
            BeforeHadLastWindowClose = ($closed -match '"LastWindowClose"')
            AfterHasLastWindowClose  = ($after -match '"LastWindowClose"')
        }

        # ---- the writer's ValidateSet: can it write StopKind 'window-closed'?
        $rejected = $false
        $message = ''
        try {
            $probe = $stopped.Clone(); $probe.StopKind = 'window-closed'
            Write-CamStatusFile @probe
        }
        catch { $rejected = $true; $message = $_.Exception.Message }

        # ---- a forward-compatible document: unknown keys at the top level and in an object
        $running = Read-HolocronText -Path (Join-Path $OutDir 'running-full.json')
        $extra = Edit-HolocronText -Text $running -Pattern '"Schema":\s+1,' -Replacement '"Schema":  1,

  "FutureTopLevelField":  { "a": [ 1, 2 ] },'
        $extra = Edit-HolocronText -Text $extra -Pattern '"FreeGB":' -Replacement '"FutureDiskField":  true,
    "FreeGB":' -After '"Disk"'
        Save-HolocronText -Path (Join-Path $OutDir 'running-extra-keys.json') -Text $extra

        return [pscustomobject]@{
            F1BeforeHadLastWindowClose = $f1.BeforeHadLastWindowClose
            F1AfterHasLastWindowClose  = $f1.AfterHasLastWindowClose
            WindowClosedRejectedByWriter = $rejected
            WindowClosedRejection = $message
        }
    }
    finally { Remove-HolocronSandbox -Path $sandbox }
}


function New-HolocronNegativeStatusDocuments {
    <#
        Documents that must FAIL the schema, each broken in exactly one way and
        named for the reason, made by editing the text of a positive fixture
        (-SourceDir). The expected violation of each is listed in
        internal/status/conformance_test.go.
    #>
    param([Parameter(Mandatory = $true)][string]$SourceDir, [Parameter(Mandatory = $true)][string]$OutDir)

    $running = Read-HolocronText -Path (Join-Path $SourceDir 'running-full.json')
    $live = Read-HolocronText -Path (Join-Path $SourceDir 'running-live.json')
    $unexpected = Read-HolocronText -Path (Join-Path $SourceDir 'patched-unexpected-over-running.json')
    $closed = Read-HolocronText -Path (Join-Path $SourceDir 'patched-windowclose-over-running.json')
    $edit = ${function:Edit-HolocronText}
    $out = {
        param([string]$Name, [string]$Text)
        Save-HolocronText -Path (Join-Path $OutDir ("neg-" + $Name + ".json")) -Text $Text
    }

    & $out 'missing-disk' (& $edit $running '"Disk":' '"DiskRenamed":')
    & $out 'missing-camera-field' (& $edit $running '"InputMode":' '"InputModeRenamed":' '"Cameras"')
    & $out 'missing-live-camera-field' (& $edit $live '"MainPath":' '"MainPathRenamed":' '"Live"')
    & $out 'wrong-type-pid' (& $edit $running '"SupervisorPid":(\s+)4242' '"SupervisorPid":${1}"4242"')
    & $out 'wrong-type-bool' (& $edit $running '"Alive":(\s+)true' '"Alive":${1}"true"' '"Cameras"')
    & $out 'wrong-type-integer-fraction' (& $edit $running '"SegmentSeconds":(\s+)900' '"SegmentSeconds":${1}900.5')
    & $out 'null-not-allowed' (& $edit $running '"Name":(\s+)"Camera One"' '"Name":${1}null' '"Cameras"')
    & $out 'bad-enum-state' (& $edit $running '"State":(\s+)"running"' '"State":${1}"paused"')
    & $out 'bad-enum-stopkind' (& $edit $unexpected '"StopKind":(\s+)"unexpected"' '"StopKind":${1}"crashed"')
    & $out 'bad-enum-inputmode' (& $edit $running '"InputMode":(\s+)"relay"' '"InputMode":${1}"tcp"' '"Cameras"')
    & $out 'bad-enum-detectedby' (& $edit $unexpected '"DetectedBy":(\s+)"nvr host"' '"DetectedBy":${1}"somebody"')
    & $out 'running-with-stopkind' (& $edit $running '"StopKind":(\s+)""' '"StopKind":${1}"requested"')
    & $out 'bad-timestamp-shape' (& $edit $running '"UpdatedAt":(\s+)"[^"]+"' '"UpdatedAt":${1}"2026-10-17 12:30:00"')
    & $out 'bad-timestamp-six-digits' (& $edit $running '"UpdatedAt":(\s+)"[^"]+"' '"UpdatedAt":${1}"2026-10-17T12:30:00.123456+03:00"')
    & $out 'bad-timestamp-zulu' (& $edit $running '"UpdatedAt":(\s+)"[^"]+"' '"UpdatedAt":${1}"2026-10-17T09:30:00.1234567Z"')
    & $out 'bad-timestamp-date' (& $edit $running '"UpdatedAt":(\s+)"[^"]+"' '"UpdatedAt":${1}"2026-13-45T09:30:00.1234567+03:00"')
    & $out 'bad-event-stamp' (& $edit $running '"At":(\s+)"20[^"]+"' '"At":${1}"2026-10-17T12:30:00.1234567+03:00"' '"Events"')
    & $out 'live-not-enabled' (& $edit $live '"Enabled":(\s+)true' '"Enabled":${1}false' '"Live"')
    & $out 'bad-windowclose-field' (& $edit $closed '"WindowTitle":' '"WindowTitleRenamed":')
    & $out 'negative-count' (& $edit $running '"RestartsTotal":(\s+)2' '"RestartsTotal":${1}-2' '"Cameras"')
}
