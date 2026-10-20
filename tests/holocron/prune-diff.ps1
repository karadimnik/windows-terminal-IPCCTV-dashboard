<#
    prune-diff.ps1 - the differential harness of the Holocron pruner (W3, LLD 6.3).

    Builds throwaway archives under %TEMP%\holocron-w3-<guid>, runs the PowerShell pruner
    Invoke-CamPrune over each of them (dry run and real run, inside the sandbox only, free space
    and the wall clock faked in this scope as spike SP7 describes) and writes its DECISIONS as
    JSON goldens to testdata\golden\prune\. The Go test internal/prune/golden_test.go rebuilds
    every tree, runs the Go planner and executor and compares, so Linux CI needs no PowerShell.

        powershell.exe -NoProfile -ExecutionPolicy Bypass -File tests\holocron\prune-diff.ps1 -Regenerate
        powershell.exe -NoProfile -ExecutionPolicy Bypass -File tests\holocron\prune-diff.ps1 -Verify

    -Verify regenerates into a temporary folder and fails on any byte difference from the
    committed goldens. Start it from a local folder (C:\Windows\Temp), never from a
    \\wsl.localhost directory. Needs Windows PowerShell 5.1 in the zone GTB Standard Time.

    SANDBOX_ONLY: every root the pruner is given and every path this script creates or removes
    must lie inside the sandbox folder it created; anything else throws before it is touched.
    Nothing real is read: no archive, config, credential, port, process or task.

    ASCII only (Windows PowerShell 5.1 reads a BOM-less script in the ANSI code page).
#>
param([switch]$Regenerate, [switch]$Verify, [int]$CorpusCount = 200, [int]$Seed = 20261020)

$ErrorActionPreference = 'Stop'
. (Join-Path -Path $PSScriptRoot -ChildPath 'holocron-lib.ps1')
. (Get-CamTestRecorderFunctions -Name @('Invoke-CamPrune', 'Get-CamSegmentDirectories'))
Assert-HolocronEnvironment
$env:SANDBOX_ONLY = '1'

# ---------------------------------------------------------------------------
# The sandbox and its guard
# ---------------------------------------------------------------------------

$script:W3Temp = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath()).TrimEnd('\')
$script:W3Base = Join-Path -Path $script:W3Temp -ChildPath ('holocron-w3-' + [guid]::NewGuid().ToString('N'))

function Assert-W3Sandbox {
    <# Throws unless $Path lies strictly inside this run's sandbox folder. #>
    param([Parameter(Mandatory = $true)][string]$Path)
    $base = $script:W3Base
    if (-not $base -or -not (Split-Path -Path $base -Leaf).StartsWith('holocron-w3-') -or
        -not $base.StartsWith($script:W3Temp + '\', [System.StringComparison]::OrdinalIgnoreCase)) {
        throw 'SANDBOX_ONLY: the sandbox folder is not set up'
    }
    $full = [System.IO.Path]::GetFullPath($Path)
    if ($full -match '^[eE]:' -or -not $full.StartsWith($base + '\', [System.StringComparison]::OrdinalIgnoreCase)) {
        throw ("SANDBOX_ONLY: refusing '{0}', it is outside {1}" -f $Path, $base)
    }
    return $full
}

function Test-W3SandboxGuard {
    <# The guard must refuse the real archive, the repository and the profile before anything runs. #>
    $refused = 0
    $probes = @('E:\cameras', 'E:\', 'C:\', (Get-HolocronRepo), $env:USERPROFILE, $script:W3Temp, ($script:W3Base + '_other\archive'), (Join-Path $script:W3Base '..\x'))
    foreach ($probe in $probes) {
        try { [void](Assert-W3Sandbox -Path $probe) } catch { $refused++ }
    }
    if ($refused -ne $probes.Count) { throw "the sandbox guard let a real path through ($refused of $($probes.Count) refused)" }
    [void](Assert-W3Sandbox -Path (Join-Path $script:W3Base 's\archive'))
    return $probes.Count
}

function Remove-W3Tree {
    param([Parameter(Mandatory = $true)][string]$Path)
    [void](Assert-W3Sandbox -Path $Path)
    Remove-CamTestTree -Path $Path
}

# ---------------------------------------------------------------------------
# Fakes, defined in this scope so the dot-sourced pruner calls them (SP7)
# ---------------------------------------------------------------------------

$script:W3 = @{ Now = [DateTime]::MinValue; Free = 0.0; Fixed = $false; Live = @(); Alerts = New-Object System.Collections.ArrayList }

# Physical bytes per logical GiB: a golden's sizes are logical, the files on disk are 65536 times
# smaller, and the free-space fake counts one logical GiB for every 16384 physical bytes removed.
$script:W3Scale = 65536
$script:W3PhysPerGB = 16384

function Get-CamFreeSpaceGB {
    param([string]$Path)
    if ($script:W3.Fixed) { return [double]$script:W3.Free }
    $gone = 0.0
    foreach ($f in $script:W3.Live) { if (-not [System.IO.File]::Exists($f.Full)) { $gone += $f.Phys } }
    return [math]::Round($script:W3.Free + $gone / $script:W3PhysPerGB, 2)
}

function Get-CamWallClock { return $script:W3.Now }

function Send-CamAlert {
    param([string]$Title, [string]$Message, [int]$EventId, [string]$EntryType, [string]$LogPath, [int]$LogMaxBytes)
    [void]$script:W3.Alerts.Add([ordered]@{ Id = $EventId; Type = $EntryType; Title = $Title })
    return @{ Title = $Title; Message = $Message; Channels = 'recorded by the harness' }
}

function Write-CamHost { param([string]$Message, [string]$Colour) }

# ---------------------------------------------------------------------------
# Tree vocabulary. Paths are relative to the sandbox with '/'; the root is 'archive'.
# Times are UTC; a segment is named for its start in the local zone (as New-CamTestSegment
# does) and created at that instant, which is also its write time unless -Written says otherwise.
# ---------------------------------------------------------------------------

$script:W3Epoch = [DateTime]::new(2026, 10, 20, 6, 0, 0, [DateTimeKind]::Utc)
$script:MiB64 = 1024L * 65536   # the default segment: 1024 physical bytes, as in the checks
$script:GiB = 1073741824L

function Get-W3Stamp {
    param([DateTime]$Utc, [string]$Offset)
    if ($Offset) {
        $sign = $(if ($Offset[0] -eq '-') { -1 } else { 1 })
        $span = [TimeSpan]::new([int]$Offset.Substring(1, 2), [int]$Offset.Substring(3, 2), 0)
        if ($sign -lt 0) { $span = $span.Negate() }
    }
    else { $span = [TimeZoneInfo]::Local.GetUtcOffset($Utc) }
    $local = [DateTime]::SpecifyKind($Utc, [DateTimeKind]::Unspecified).Add($span)
    $sign = $(if ($span -lt [TimeSpan]::Zero) { '-' } else { '+' })
    return ('{0}{1}{2:00}{3:00}' -f $local.ToString('yyyyMMdd_HHmmss', [System.Globalization.CultureInfo]::InvariantCulture), $sign, [math]::Abs($span.Hours), [math]::Abs($span.Minutes))
}

function New-W3Seg {
    <# A segment of $Cam in folder $Folder (default $Cam) started $Age seconds before $Now. #>
    param([string]$Cam, [double]$Age, [long]$Bytes = $script:MiB64, [string]$Ext = '.mkv', [string]$Folder,
        $Written = $null, [switch]$Late, [string]$Offset, [string]$Under = 'archive', $Now = $script:W3Epoch, $Created = $null)
    if (-not $Folder) { $Folder = $Cam }
    $start = $Now.AddSeconds(-$Age)
    $name = '{0}_{1}{2}' -f $Cam, (Get-W3Stamp -Utc $start -Offset $Offset), $Ext
    $mtime = $(if ($null -ne $Written) { $Now.AddSeconds(-[double]$Written) } else { $start })
    $ctime = $(if ($null -ne $Created) { $Now.AddSeconds(-[double]$Created) } else { $start })
    return [ordered]@{ Path = "$Under/$Folder/$name"; Kind = 'file'; Bytes = $Bytes; MTime = $mtime; CTime = $ctime; Late = [bool]$Late }
}

function New-W3File {
    param([string]$Path, [long]$Bytes = 32L * 65536, [double]$Age = 0, [switch]$Late, [string]$Attrs, $Now = $script:W3Epoch)
    $t = $Now.AddSeconds(-$Age)
    $e = [ordered]@{ Path = $Path; Kind = 'file'; Bytes = $Bytes; MTime = $t; CTime = $t; Late = [bool]$Late }
    if ($Attrs) { $e.Attrs = $Attrs }
    return $e
}

function New-W3Link {
    <# A link at $Path to the sandbox-relative $Target: 'dirlink' (a junction) or 'filelink'. #>
    param([string]$Path, [string]$Target, [string]$Kind, [double]$Age = 0, [switch]$Late)
    return [ordered]@{ Path = $Path; Kind = $Kind; Target = $Target; MTime = $script:W3Epoch.AddSeconds(-$Age); Late = [bool]$Late }
}

function New-W3Step {
    param([string]$Op = 'prune', [switch]$Dry, [double]$Free = 0, [switch]$Fixed, [double]$Shift = 0, [string]$State = 'carry',
        [hashtable]$Config = @{}, [object[]]$Entries = @(), [string[]]$Paths = @())
    $s = [ordered]@{ Op = $Op }
    if ($Op -eq 'prune') { $s.Dry = [bool]$Dry; $s.Free = $Free; $s.Fixed = [bool]$Fixed; $s.Shift = $Shift; $s.State = $State; $s.Config = $Config }
    if ($Op -eq 'add') { $s.Entries = $Entries }
    if ($Op -eq 'remove') { $s.Paths = $Paths }
    return $s
}

# ---------------------------------------------------------------------------
# Building a tree and running the steps
# ---------------------------------------------------------------------------

function ConvertTo-W3Rel {
    param([string]$Path, [string]$Sandbox)
    if ($Path.StartsWith($Sandbox + '\', [System.StringComparison]::OrdinalIgnoreCase)) { $Path = $Path.Substring($Sandbox.Length + 1) }
    return $Path.Replace('\', '/')
}

function ConvertTo-W3Masked {
    <# Sandbox paths become sandbox-relative with '/', so a golden does not depend on where it ran. #>
    param([string]$Text, [string]$Sandbox)
    if (-not $Text) { return '' }
    $out = $Text
    $i = $out.IndexOf($Sandbox + '\', [System.StringComparison]::OrdinalIgnoreCase)
    while ($i -ge 0) {
        $end = $i + $Sandbox.Length + 1
        $j = $end
        while ($j -lt $out.Length -and $out[$j] -ne ' ' -and $out[$j] -ne ':' -and $out[$j] -ne ',' -and $out[$j] -ne ')' -and $out[$j] -ne "'") { $j++ }
        $rest = $out.Substring($end, $j - $end).Replace('\', '/')
        $out = $out.Substring(0, $i) + $rest + $out.Substring($j)
        $i = $out.IndexOf($Sandbox + '\', [System.StringComparison]::OrdinalIgnoreCase)
    }
    return $out
}

function Add-W3Entry {
    param($Entry, [string]$Sandbox)
    $full = Assert-W3Sandbox -Path (Join-Path $Sandbox $Entry.Path.Replace('/', '\'))
    $dir = Split-Path -Path $full -Parent
    if (-not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }
    switch ($Entry.Kind) {
        'dir' { [void](New-Item -ItemType Directory -Path $full -Force) }
        'file' {
            $phys = [long]($Entry.Bytes / $script:W3Scale)
            $content = New-Object byte[] $phys
            $stamp = [System.Text.Encoding]::ASCII.GetBytes($Entry.Path)
            for ($k = 0; $k -lt $phys; $k++) { $content[$k] = $stamp[$k % $stamp.Length] }
            [System.IO.File]::WriteAllBytes($full, $content)
            [System.IO.File]::SetCreationTimeUtc($full, $Entry.CTime)
            [System.IO.File]::SetLastWriteTimeUtc($full, $Entry.MTime)
            if ($Entry.Contains('Attrs')) { (Get-Item -LiteralPath $full -Force).Attributes = $Entry.Attrs }
        }
        'dirlink' {
            $target = Assert-W3Sandbox -Path (Join-Path $Sandbox $Entry.Target.Replace('/', '\'))
            [void](New-Item -ItemType Junction -Path $full -Target $target)
        }
        'filelink' {
            $target = Assert-W3Sandbox -Path (Join-Path $Sandbox $Entry.Target.Replace('/', '\'))
            [void](New-CamTestFileReparsePoint -Path $full -Target $target -Time $Entry.MTime.ToLocalTime())
        }
    }
}

function Get-W3Live {
    <# The regular files under the root now, with their physical sizes, for the free-space fake. #>
    param([string]$Root)
    $live = @()
    foreach ($f in @(Get-ChildItem -LiteralPath $Root -Recurse -File -Force -ErrorAction SilentlyContinue)) {
        if (($f.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
        if (-not $f.FullName.StartsWith($Root + '\', [System.StringComparison]::OrdinalIgnoreCase)) { continue }
        $live += [pscustomobject]@{ Full = $f.FullName; Phys = [double]$f.Length }
    }
    return $live
}

function New-W3State {
    return @{ LastRunAt = $null; DeletedTotal = 0; FreedBytes = 0; Blocked = $false; LastMessage = ''; RefusalAlerted = ''; ClockRefused = $false; BlockedAlerted = $false; AgeBreakerTripped = $false }
}

$script:W3LogKinds = [ordered]@{
    refusal = '*PRUNING DISABLED*'; clockRefused = '*PRUNE REFUSED*'; clockResumed = '*the clock and the archive agree again*'
    held = '*PRUNE AGE RULE HELD*'; breakerReset = '*circuit breaker is reset*'; blocked = '*PRUNE BLOCKED*'
    deleted = '* prune: deleted *'; summary = '* file(s) deleted, *'; stop = '* prune stop: *'
}

function Invoke-W3Scenario {
    <# Builds the scenario's tree in a fresh sandbox, runs its steps, returns the golden document. #>
    param([Parameter(Mandatory = $true)]$Scenario, [Parameter(Mandatory = $true)][int]$Number)

    $sandbox = Assert-W3Sandbox -Path (Join-Path $script:W3Base ('s{0:000}' -f $Number))
    $log = Assert-W3Sandbox -Path (Join-Path $script:W3Base ('s{0:000}.log' -f $Number))
    $root = Join-Path $sandbox 'archive'
    [void](New-Item -ItemType Directory -Path $root -Force)
    $now = $script:W3Epoch
    if ($Scenario.Contains('Now')) { $now = $Scenario.Now }

    $config = Get-CamConfigDefault
    $config.RootPath = Assert-W3Sandbox -Path $root
    $config.ReserveGB = 0
    $config.PruneHysteresisGB = 0
    $config.MaxRetentionDays = $null
    if ($Scenario.Contains('Config')) { foreach ($k in $Scenario.Config.Keys) { $config[$k] = $Scenario.Config[$k] } }

    foreach ($e in @($Scenario.Entries | Where-Object { -not $_.Late })) { Add-W3Entry -Entry $e -Sandbox $sandbox }
    $claim = $null
    $script:W3.Now = $now
    switch ($Scenario.Marker) {
        'claim' { $c = Initialize-CamArchiveOwner -Config $config; $claim = [ordered]@{ Created = [bool]$c.Created; Problem = (ConvertTo-W3Masked -Text $c.Problem -Sandbox $sandbox) } }
        'badowner' { Save-HolocronText -Path (Join-Path $root '_state\archive-owner.json') -Text ('{ "Owner": "someone else", "Schema": 1, "RootPath": "' + $root.Replace('\', '\\') + '" }' + "`n") }
        'copied' { Save-HolocronText -Path (Join-Path $root '_state\archive-owner.json') -Text '{ "Owner": "IPCCTV recorder", "Schema": 1, "RootPath": "C:\\elsewhere\\cameras" }' }
    }
    foreach ($e in @($Scenario.Entries | Where-Object { $_.Late })) { Add-W3Entry -Entry $e -Sandbox $sandbox }

    $state = New-W3State
    $results = @()
    $stepConfigs = @()
    foreach ($step in $Scenario.Steps) {
        $res = [ordered]@{ Op = $step.Op }
        switch ($step.Op) {
            'claim' {
                $script:W3.Now = $now
                $c = Initialize-CamArchiveOwner -Config $config
                $res.Created = [bool]$c.Created
                $res.Problem = ConvertTo-W3Masked -Text $c.Problem -Sandbox $sandbox
            }
            'add' { foreach ($e in $step.Entries) { Add-W3Entry -Entry $e -Sandbox $sandbox } }
            'remove' {
                foreach ($p in $step.Paths) {
                    $full = Assert-W3Sandbox -Path (Join-Path $sandbox $p.Replace('/', '\'))
                    [System.IO.File]::Delete($full)
                }
            }
            'prune' {
                foreach ($k in $step.Config.Keys) { $config[$k] = $step.Config[$k] }
                if ($step.State -eq 'fresh') { $state = New-W3State }
                $script:W3.Now = $now.AddSeconds($step.Shift)
                $script:W3.Free = [double]$step.Free
                $script:W3.Fixed = [bool]$step.Fixed
                $script:W3.Live = @(Get-W3Live -Root $root)
                $script:W3.Alerts.Clear()
                [void](Assert-W3Sandbox -Path $config.RootPath)
                $linesBefore = $(if (Test-Path -LiteralPath $log) { @(Get-Content -LiteralPath $log).Count } else { 0 })
                $before = Get-CamTestFileManifest -Path $sandbox
                $r = Invoke-CamPrune -Config $config -SupervisorLog $log -State $state -DryRun:([bool]$step.Dry)
                $diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $sandbox)
                $lines = @(if (Test-Path -LiteralPath $log) { @(Get-Content -LiteralPath $log) | Select-Object -Skip $linesBefore })

                $deletions = @()
                foreach ($line in @($r.Deletions)) {
                    if ($line -notmatch '^(?:would delete|prune: deleted) (.+?) \((\d+) bytes, (?:written )?\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:, (.*)\)|\) - (.*))$') { throw "unparsed deletion line: $line" }
                    $why = $(if ($Matches[3]) { $Matches[3] } else { $Matches[4] })
                    $deletions += [ordered]@{
                        Path = (ConvertTo-W3Rel -Path $Matches[1] -Sandbox $sandbox); Bytes = ([long]$Matches[2] * $script:W3Scale)
                        Reason = $(if ($why -like 'older than MaxRetentionDays*') { 'age' } else { 'watermark' }); Detail = $why
                    }
                }
                $logged = [ordered]@{}
                foreach ($kind in $script:W3LogKinds.Keys) { $logged[$kind] = @($lines | Where-Object { $_ -like $script:W3LogKinds[$kind] }).Count }

                $res.Dry = [bool]$step.Dry
                $res.Deletions = $deletions
                $res.Removed = @($diff.Removed | ForEach-Object { ConvertTo-W3Rel -Path $_ -Sandbox $sandbox } | Sort-Object)
                $res.Changed = @($diff.Changed).Count + @($diff.Added).Count
                $res.Blocked = [bool]$r.Blocked
                $res.RootRefusal = ConvertTo-W3Masked -Text $r.RootRefusal -Sandbox $sandbox
                $res.ClockRefusal = ConvertTo-W3Masked -Text $r.ClockRefusal -Sandbox $sandbox
                $res.AgeBreaker = [string]$r.AgeBreaker
                $res.StopLine = ConvertTo-W3Masked -Text $r.StopLine -Sandbox $sandbox
                $res.Message = ConvertTo-W3Masked -Text $r.Message -Sandbox $sandbox
                $res.Protected = @(@($r.Protected) | ForEach-Object { ConvertTo-W3Rel -Path $_ -Sandbox $sandbox } | Sort-Object)
                $res.Rejected = @(@($r.Rejected) | ForEach-Object { ConvertTo-W3Masked -Text $_ -Sandbox $sandbox } | Sort-Object)
                $res.Foreign = @(@($r.Foreign) | ForEach-Object { ConvertTo-W3Masked -Text $_ -Sandbox $sandbox } | Sort-Object)
                $res.Candidates = [int]$r.Candidates
                $res.Deleted = [int]$r.Deleted
                $res.Failed = [int]$r.Failed
                $res.FreeGBBefore = [double]$r.FreeGBBefore
                $res.FreeGBAfter = [double]$r.FreeGBAfter
                $res.ReserveGB = [double]$r.ReserveGB
                $res.TargetGB = [double]$r.TargetGB
                $res.Alerts = @($script:W3.Alerts | ForEach-Object { $_ })
                $res.State = [ordered]@{ RefusalAlerted = [bool]$state.RefusalAlerted; ClockRefused = [bool]$state.ClockRefused; BlockedAlerted = [bool]$state.BlockedAlerted; AgeBreakerTripped = [bool]$state.AgeBreakerTripped }
                $res.Logged = $logged
                $stepConfigs += [ordered]@{ ReserveGB = [double]$config.ReserveGB; PruneHysteresisGB = [double]$config.PruneHysteresisGB; MaxRetentionDays = $(if ($null -eq $config.MaxRetentionDays) { $null } else { [double]$config.MaxRetentionDays }); MaxAgePruneFraction = [double]$config.MaxAgePruneFraction }
            }
        }
        $results += $res
    }
    Remove-W3Tree -Path $sandbox
    if (Test-Path -LiteralPath $log) { Remove-Item -LiteralPath (Assert-W3Sandbox -Path $log) -Force }

    # The document: the tree and steps as given (resolved configs), then the results.
    $steps = @()
    $ci = 0
    foreach ($step in $Scenario.Steps) {
        $s = [ordered]@{}
        foreach ($k in $step.Keys) { $s[$k] = $step[$k] }
        if ($step.Op -eq 'prune') { $s.Config = $stepConfigs[$ci]; $ci++ }
        if ($step.Op -eq 'add') { $s.Entries = @($step.Entries | ForEach-Object { ConvertTo-W3EntryDoc $_ }) }
        $steps += $s
    }
    return [ordered]@{
        Name = $Scenario.Name; Check = $Scenario.Check; Expect = @($Scenario.Expect | Where-Object { $_ }); Note = [string]$Scenario.Note
        Now = $now.ToString('o'); SegmentSeconds = [int]$config.SegmentSeconds; StallFactor = [double]$config.StallFactor; Container = [string]$config.Container
        Marker = [string]$Scenario.Marker; Claim = $claim
        Entries = @($Scenario.Entries | ForEach-Object { ConvertTo-W3EntryDoc $_ })
        Steps = $steps; Results = $results
    }
}

function ConvertTo-W3EntryDoc {
    param($e)
    $d = [ordered]@{ Path = $e.Path; Kind = $e.Kind }
    if ($e.Kind -eq 'file') { $d.Bytes = [long]$e.Bytes; $d.CTime = $e.CTime.ToString('o') }
    $d.MTime = $e.MTime.ToString('o')
    if ($e.Contains('Target')) { $d.Target = $e.Target }
    if ($e.Late) { $d.Late = $true }
    if ($e.Contains('Attrs')) { $d.Attrs = $e.Attrs }
    return $d
}

function ConvertTo-W3Json {
    <# ConvertTo-Json with every character above U+007E escaped, one trailing newline. #>
    param([Parameter(Mandatory = $true)]$InputObject, [switch]$Compress)
    $json = ConvertTo-Json -InputObject $InputObject -Depth 12 -Compress:$Compress
    $b = New-Object System.Text.StringBuilder
    foreach ($ch in $json.ToCharArray()) { if ([int]$ch -gt 126) { [void]$b.AppendFormat('\u{0:x4}', [int]$ch) } else { [void]$b.Append($ch) } }
    return $b.ToString()
}

# ---------------------------------------------------------------------------
# The table: every scenario of the prune* checks in tests\cam.tests.ps1, plus the corners the
# brief names (rounding tie, reserve-to-target corner, tie order, the expected differences).
# ---------------------------------------------------------------------------

function W3D([double]$days) { return $days * 86400 }
function W3H([double]$hours) { return $hours * 3600 }
function W3M([double]$minutes) { return $minutes * 60 }

function Get-W3Table {
    $t = New-Object System.Collections.ArrayList
    $age1 = @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }

    # prunematrix: every kind of non-segment where the pruner looks.
    $old = W3D 10
    $e = @(
        (New-W3Seg 'Front_Entrance' (W3D 10)), (New-W3Seg 'Front_Entrance' (W3D 9)), (New-W3Seg 'Front_Entrance' (W3D 8)), (New-W3Seg 'Workshop' (W3D 7)),
        (New-W3Seg 'Front_Entrance' (W3M 2)), (New-W3Seg 'Workshop' (W3M 2)),
        (New-W3File 'archive/Front_Entrance/notes.txt' -Age $old -Late), (New-W3File 'archive/Front_Entrance/desktop.ini' -Age $old -Late -Attrs 'Hidden, System, Archive'),
        (New-W3File 'archive/Front_Entrance/Front_Entrance_export.mkv' -Age $old -Late), (New-W3File 'archive/Front_Entrance/empty.dat' -Bytes 0 -Age $old -Late),
        (New-W3Seg 'Workshop' $old -Folder 'Front_Entrance' -Late), (New-W3Seg 'Front_Entrance' ($old - 3600) -Ext '.mp4' -Late),
        (New-W3Seg 'Front_Entrance' ($old - 7200) -Ext '.mkv.tmp' -Late), (New-W3File 'archive/Workshop/Thumbs.db' -Age $old -Late -Attrs 'Hidden, Archive'),
        (New-W3Seg 'Front_Entrance' $old -Folder '_state' -Late), (New-W3Seg 'Front_Entrance' $old -Folder '_logs' -Late), (New-W3Seg 'Front_Entrance' $old -Folder '_selftest' -Late),
        (New-W3Seg 'Front_Entrance' $old -Under 'archive_old' -Late),
        (New-W3Seg 'Linked' $old -Under 'outside' -Late), (New-W3Seg 'Linked' ($old - 3600) -Under 'outside' -Late),
        (New-W3Link 'archive/Linked' 'outside/Linked' 'dirlink' -Late),
        (New-W3Seg 'Front_Entrance' $old -Folder 'clips' -Under 'outside' -Late),
        (New-W3Link 'archive/Front_Entrance/clips' 'outside/clips' 'dirlink' -Late)
    )
    $target = New-W3Seg 'Front_Entrance' (W3D 11) -Folder 'target' -Under 'outside' -Late
    $leaf = Split-Path -Path $target.Path -Leaf
    $e += $target
    $e += New-W3Link ('archive/Front_Entrance/' + $leaf) $target.Path 'filelink' -Age (W3D 11) -Late
    [void]$t.Add(@{ Name = 'prunematrix'; Check = 'prunematrix'; Marker = 'claim'; Config = $age1; Entries = $e; Steps = @((New-W3Step -Dry), (New-W3Step)) })

    # pruneroots: refused roots (the drive-root case is a Go table test: the guard keeps the
    # harness inside its sandbox, so it never points the pruner at a real drive root).
    $rootSeed = @((New-W3Seg 'Front_Entrance' (W3D 5)), (New-W3Seg 'Front_Entrance' (W3D 4)), (New-W3Seg 'Front_Entrance' (W3M 3) -Bytes 0), (New-W3Seg 'Front_Entrance' (W3M 1)))
    $profileLike = $rootSeed + @((New-W3File 'archive/Desktop/todo.txt' -Age (W3D 30)), (New-W3File 'archive/Documents/report.docx' -Age (W3D 30)),
        (New-W3File 'archive/AppData/Local/settings.dat' -Age (W3D 30)), (New-W3File 'archive/NTUSER.DAT' -Age (W3D 30) -Attrs 'Hidden, Archive'))
    [void]$t.Add(@{ Name = 'pruneroots-profile'; Check = 'pruneroots'; Marker = 'claim'; Config = $age1; Entries = $profileLike; Steps = @((New-W3Step), (New-W3Step), (New-W3Step -Dry)) })
    [void]$t.Add(@{ Name = 'pruneroots-nomarker'; Check = 'pruneroots'; Marker = 'none'; Config = $age1; Entries = $rootSeed; Steps = @((New-W3Step), (New-W3Step), (New-W3Step -Dry)) })
    $foreign = $rootSeed + @((New-W3File 'archive/Photos/holiday.jpg'), (New-W3File 'archive/readme.txt'))
    [void]$t.Add(@{ Name = 'pruneroots-foreign'; Check = 'pruneroots'; Marker = 'claim'; Config = $age1; Entries = $foreign; Steps = @((New-W3Step), (New-W3Step), (New-W3Step -Dry)) })

    # pruneage: strictly oldest first across cameras; a segment created early but written after
    # its folder's newest is kept.
    $e = @((New-W3Seg 'Front_Entrance' (W3D 5)), (New-W3Seg 'Workshop' (W3D 4.5)), (New-W3Seg 'Front_Entrance' (W3D 4)), (New-W3Seg 'Front_Entrance' (W3D 3)),
        (New-W3Seg 'Front_Entrance' (W3H 2)), (New-W3Seg 'Front_Entrance' (W3M 1)), (New-W3Seg 'Workshop' (W3D 3.5)), (New-W3Seg 'Workshop' (W3D 6) -Written (W3D 3.4)))
    [void]$t.Add(@{ Name = 'pruneage'; Check = 'pruneage'; Marker = 'claim'; Config = @{ MaxRetentionDays = 1 }; Entries = $e; Steps = @((New-W3Step -Dry), (New-W3Step)) })

    # prunewatermark: one segment = 1 GB; free 10 GB, growing by what is deleted.
    $e = @()
    foreach ($c in @(@{ N = 'Cam_A'; H = @(40, 30, 20) }, @{ N = 'Cam_B'; H = @(38, 28, 18) }, @{ N = 'Cam_C'; H = @(36, 26, 16) })) {
        foreach ($h in $c.H) { $e += New-W3Seg $c.N (W3H $h) -Bytes $script:GiB }
        $e += New-W3Seg $c.N (W3M 1) -Bytes $script:GiB
    }
    [void]$t.Add(@{ Name = 'prunewatermark'; Check = 'prunewatermark'; Marker = 'claim'; Entries = $e; Steps = @(
            (New-W3Step -Free 10 -Config @{ ReserveGB = 5 }), (New-W3Step -Free 10 -Config @{ ReserveGB = 13; PruneHysteresisGB = 2 }),
            (New-W3Step -Free 15 -Config @{ ReserveGB = 1000 }), (New-W3Step -Free 19)) })

    # pruneclock: the clock 30 days ahead, a segment written in the future, an inverted pair; control.
    $plain = @((New-W3Seg 'Front_Entrance' (W3D 3)), (New-W3Seg 'Front_Entrance' (W3D 2)), (New-W3Seg 'Front_Entrance' (W3M 1)))
    [void]$t.Add(@{ Name = 'pruneclock-ahead'; Check = 'pruneclock'; Marker = 'claim'; Config = $age1; Entries = $plain; Steps = @((New-W3Step -Shift (W3D 30)), (New-W3Step -Shift (W3D 30))) })
    $future = @((New-W3Seg 'Front_Entrance' (W3D 3)), (New-W3Seg 'Front_Entrance' (W3D 2) -Written (W3D -2)), (New-W3Seg 'Front_Entrance' (W3M 1)))
    [void]$t.Add(@{ Name = 'pruneclock-future'; Check = 'pruneclock'; Marker = 'claim'; Config = $age1; Entries = $future; Steps = @((New-W3Step), (New-W3Step)) })
    $inverted = @((New-W3Seg 'Front_Entrance' (W3D 1) -Written (W3H 1)), (New-W3Seg 'Front_Entrance' (W3M 1) -Written (W3D 10)))
    [void]$t.Add(@{ Name = 'pruneclock-inverted'; Check = 'pruneclock'; Marker = 'claim'; Config = $age1; Entries = $inverted; Steps = @((New-W3Step), (New-W3Step)) })
    [void]$t.Add(@{ Name = 'pruneclock-control'; Check = 'pruneclock'; Marker = 'claim'; Config = $age1; Entries = $plain; Steps = @((New-W3Step), (New-W3Step -Shift (W3D 30)), (New-W3Step)) })

    # prunebreaker: ten segments over two cameras, some five days old.
    $breaker = {
        param([int]$Expired, [long]$ExpiredBytes, [long]$RecentBytes)
        $cams = @('Cam_A', 'Cam_B')
        $list = @()
        for ($i = 0; $i -lt $Expired; $i++) { $list += New-W3Seg $cams[$i % 2] ((W3D 5) - (W3H $i)) -Bytes $ExpiredBytes }
        for ($i = 0; $i -lt (8 - $Expired); $i++) { $list += New-W3Seg $cams[$i % 2] (W3H (2 + $i)) -Bytes $RecentBytes }
        foreach ($c in $cams) { $list += New-W3Seg $c (W3M 1) -Bytes $RecentBytes }
        return , $list
    }
    $b1 = @{ MaxRetentionDays = 1; MaxAgePruneFraction = 0.5 }
    [void]$t.Add(@{ Name = 'prunebreaker-count60'; Check = 'prunebreaker'; Marker = 'claim'; Config = $b1; Entries = (& $breaker 6 (1024L * 65536) (1024L * 65536)); Steps = @(
            (New-W3Step), (New-W3Step), (New-W3Step -Config @{ MaxRetentionDays = 30 }), (New-W3Step -Config @{ MaxRetentionDays = 1 }), (New-W3Step -Dry)) })
    [void]$t.Add(@{ Name = 'prunebreaker-count40'; Check = 'prunebreaker'; Marker = 'claim'; Config = $b1; Entries = (& $breaker 4 (1024L * 65536) (1024L * 65536)); Steps = @((New-W3Step -Dry), (New-W3Step)) })
    [void]$t.Add(@{ Name = 'prunebreaker-bytes'; Check = 'prunebreaker'; Marker = 'claim'; Config = $b1; Entries = (& $breaker 4 (4096L * 65536) (1024L * 65536)); Steps = @((New-W3Step)) })
    [void]$t.Add(@{ Name = 'prunebreaker-countonly'; Check = 'prunebreaker'; Marker = 'claim'; Config = $b1; Entries = (& $breaker 6 (512L * 65536) (8192L * 65536)); Steps = @((New-W3Step)) })
    [void]$t.Add(@{ Name = 'prunebreaker-watermark'; Check = 'prunebreaker'; Marker = 'claim'; Config = @{ MaxRetentionDays = 1; MaxAgePruneFraction = 0.5; ReserveGB = 13; PruneHysteresisGB = 2 }
            Entries = (& $breaker 6 $script:GiB $script:GiB); Steps = @((New-W3Step -Free 10)) })

    # pruneblocked: free pinned at 1 GB below a 20 GB reserve.
    $e = @((New-W3Seg 'Front_Entrance' (W3H 3)), (New-W3Seg 'Front_Entrance' (W3M 1)))
    [void]$t.Add(@{ Name = 'pruneblocked'; Check = 'pruneblocked'; Marker = 'claim'; Config = @{ ReserveGB = 20 }; Entries = $e; Steps = @(
            (New-W3Step -Dry -Free 1 -Fixed), (New-W3Step -Free 1 -Fixed), (New-W3Step -Free 1 -Fixed),
            (New-W3Step -Free 1 -Fixed -Config @{ ReserveGB = 0 }), (New-W3Step -Free 1 -Fixed -Config @{ ReserveGB = 20 })) })

    # pruneowner: a clean root is claimed; a root with a foreign top-level entry is not, and
    # pruning stays disabled with one alert over three cycles.
    [void]$t.Add(@{ Name = 'pruneowner-clean'; Check = 'pruneowner'; Marker = 'none'; Config = @{ MaxRetentionDays = 1 }
            Entries = @((New-W3Seg 'pruneowner-clean' (W3D 2)), (New-W3Seg 'pruneowner-clean' (W3M 1))); Steps = @((New-W3Step -Op 'claim'), (New-W3Step), (New-W3Step)) })
    [void]$t.Add(@{ Name = 'pruneowner-foreign'; Check = 'pruneowner'; Marker = 'none'; Config = @{ MaxRetentionDays = 1 }
            Entries = @((New-W3File 'archive/Photos/holiday.jpg'), (New-W3Seg 'pruneowner-foreign' (W3D 5)), (New-W3Seg 'pruneowner-foreign' (W3D 4)), (New-W3Seg 'pruneowner-foreign' (W3M 1)))
            Steps = @((New-W3Step -Op 'claim'), (New-W3Step), (New-W3Step), (New-W3Step)) })

    # pruneloop: the supervisor loop's three phases as consecutive runs over one archive, with a
    # camera that is recording (6 s segments, newest written 2 s ago).
    $rec = @()
    for ($i = 4; $i -ge 0; $i--) { $rec += New-W3Seg 'synth1' ($i * 6 + 8) -Written ($i * 6 + 2) }
    $loop = @{ SegmentSeconds = 6 }
    $ageCfg = @{ SegmentSeconds = 6; MaxRetentionDays = 1; MaxAgePruneFraction = 1.0 }
    $planted = @((New-W3Seg 'synth1' (W3D 3) -Late), (New-W3Seg 'synth1' (W3D 2) -Late), (New-W3Seg 'synth1' (W3H 30) -Late), (New-W3Seg 'oldcam' (W3D 4) -Late), (New-W3Seg 'oldcam' (W3D 3.5) -Late),
        (New-W3Seg 'synth1' (W3H 12) -Late), (New-W3Seg 'oldcam' (W3D 3) -Late), (New-W3Seg 'synth1' (W3D 5) -Ext '.mp4' -Late), (New-W3File 'archive/synth1/notes.txt' -Age (W3D 6) -Late))
    [void]$t.Add(@{ Name = 'pruneloop-age'; Check = 'pruneloop'; Marker = 'claim'; Config = $ageCfg; Entries = ($rec + $planted); Steps = @((New-W3Step), (New-W3Step), (New-W3Step)) })
    $flood = @()
    for ($i = 0; $i -lt 8; $i++) { $flood += New-W3Seg 'synth1' ((W3D 3) - (W3H $i)) -Late }
    $flood2 = @()
    for ($i = 0; $i -lt 8; $i++) { $flood2 += New-W3Seg 'synth1' ((W3H 50) - (W3H $i)) }
    $bCfg = @{ SegmentSeconds = 6; MaxRetentionDays = 1; MaxAgePruneFraction = 0.5 }
    [void]$t.Add(@{ Name = 'pruneloop-breaker'; Check = 'pruneloop'; Marker = 'claim'; Config = $bCfg; Entries = ($rec + $flood); Steps = @(
            (New-W3Step), (New-W3Step), (New-W3Step), (New-W3Step -Op 'remove' -Paths @($flood[0..5] | ForEach-Object { $_.Path })), (New-W3Step),
            (New-W3Step -Op 'add' -Entries $flood2), (New-W3Step), (New-W3Step), (New-W3Step)) })
    [void]$t.Add(@{ Name = 'pruneloop-blocked'; Check = 'pruneloop'; Marker = 'claim'; Config = @{ SegmentSeconds = 6; ReserveGB = 1000000 }
            Entries = ($rec + @((New-W3Seg 'oldcam' (W3D 2) -Late), (New-W3Seg 'oldcam' (W3D 1) -Late))); Steps = @((New-W3Step -Free 10), (New-W3Step -Free 10), (New-W3Step -Free 10), (New-W3Step -Free 10)) })

    # Corners: the banker's-rounding tie that feeds the watermark (10.125 GB free is 10.12, below
    # a 10.13 GB reserve; half up would say 10.13, not below), and its neighbour 10.140625 (10.14).
    $e = @((New-W3Seg 'Cam_A' (W3H 2) -Bytes (16L * 1048576)), (New-W3Seg 'Cam_A' (W3H 1) -Bytes (16L * 1048576)), (New-W3Seg 'Cam_A' (W3M 1) -Bytes (16L * 1048576)))
    [void]$t.Add(@{ Name = 'rounding-tie'; Check = 'corner'; Marker = 'claim'; Config = @{ ReserveGB = 10.13 }; Entries = $e; Steps = @((New-W3Step -Free 10.125 -State 'fresh'), (New-W3Step -Free 10.140625 -State 'fresh')) })
    # V7: the candidates run out between the reserve and the target - BLOCKED although above the reserve.
    $e = @((New-W3Seg 'Cam_A' (W3H 3) -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3H 2) -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3M 1) -Bytes $script:GiB))
    [void]$t.Add(@{ Name = 'reserve-target-corner'; Check = 'corner'; Marker = 'claim'; Config = @{ ReserveGB = 10; PruneHysteresisGB = 5 }; Entries = $e; Steps = @((New-W3Step -Free 9), (New-W3Step -Free 11)) })
    # IR U5: equal write times across cameras, the watermark cut falling between them. Sort-Object
    # in Windows PowerShell is not stable (three equal keys come out reversed); Go orders by name.
    $e = @((New-W3Seg 'Cam_B' (W3H 5) -Written (W3H 4) -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3H 6) -Written (W3H 4) -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3H 5) -Written (W3H 4) -Bytes $script:GiB),
        (New-W3Seg 'Cam_A' (W3M 1) -Bytes $script:GiB), (New-W3Seg 'Cam_B' (W3M 1) -Bytes $script:GiB))
    [void]$t.Add(@{ Name = 'tie-order'; Check = 'difference'; Expect = @('tie-order'); Marker = 'claim'; Config = @{ ReserveGB = 11 }; Entries = $e; Steps = @((New-W3Step -Free 9 -State 'fresh'), (New-W3Step -Free 10 -State 'fresh')) })
    # DST fall-back 2026-10-25 04:00 +0300 -> 03:00 +0200: the name order is wrong for an hour,
    # the instants in the names (and creation) are right.
    $fb = [DateTime]::new(2026, 10, 25, 1, 20, 0, [DateTimeKind]::Utc)
    $e = @((New-W3Seg 'Cam_A' (W3M 50) -Now $fb -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3M 35) -Now $fb -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3M 20) -Now $fb -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3M 5) -Now $fb -Written (W3M 0.5) -Bytes $script:GiB))
    [void]$t.Add(@{ Name = 'dst-fallback'; Check = 'corner'; Marker = 'claim'; Now = $fb; Config = @{ ReserveGB = 12 }; Entries = $e; Steps = @((New-W3Step -Free 10)) })

    # Expected differences (testdata/golden/prune/EXPECTED-DIFFERENCES.md).
    $u = [string][char]0x0663
    $nm = @((New-W3Seg 'Cam_A' (W3D 5)), (New-W3Seg 'Cam_A' (W3D 4)), (New-W3Seg 'Cam_A' (W3M 1)),
        (New-W3File ('archive/Cam_A/Cam_A_2026101' + $u + '_101010+0300.mkv') -Age (W3D 6) -Late), (New-W3File 'archive/Cam_A/Cam_A_20261345_101010+0300.mkv' -Age (W3D 7) -Late),
        (New-W3File 'archive/Cam_A/Cam_A_20261012_256161+0300.mkv' -Age (W3D 8) -Late), (New-W3File 'archive/Cam_A/Cam_A_20261012_101010+9900.mkv' -Age (W3D 9) -Late))
    [void]$t.Add(@{ Name = 'strict-allowlist-age'; Check = 'difference'; Expect = @('strict-allowlist'); Marker = 'claim'; Config = $age1; Entries = $nm; Steps = @((New-W3Step -Dry), (New-W3Step)) })
    $nmw = @((New-W3Seg 'Cam_A' (W3H 5) -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3H 4) -Bytes $script:GiB), (New-W3Seg 'Cam_A' (W3M 1) -Bytes $script:GiB),
        (New-W3File 'archive/Cam_A/Cam_A_20261345_101010+0300.mkv' -Bytes $script:GiB -Age (W3H 6) -Late))
    [void]$t.Add(@{ Name = 'strict-allowlist-watermark'; Check = 'difference'; Expect = @('strict-allowlist-substitution'); Marker = 'claim'; Config = @{ ReserveGB = 11 }; Entries = $nmw; Steps = @((New-W3Step -Free 10)) })
    $cf = @((New-W3Seg 'Cam_A' (W3D 5)), (New-W3Seg 'Cam_A' (W3M 1)), (New-W3Seg 'cam_a' (W3D 4) -Folder 'Cam_A' -Late), (New-W3Seg 'Cam_A' (W3D 3) -Ext '.MKV' -Late))
    [void]$t.Add(@{ Name = 'case-fold'; Check = 'difference'; Expect = @('case-fold'); Marker = 'claim'; Config = $age1; Entries = $cf; Steps = @((New-W3Step)) })
    [void]$t.Add(@{ Name = 'copied-marker'; Check = 'difference'; Expect = @('copied-marker'); Marker = 'copied'; Config = $age1; Entries = $rootSeed; Steps = @((New-W3Step), (New-W3Step -Dry)) })
    [void]$t.Add(@{ Name = 'bad-owner-marker'; Check = 'pruneroots'; Marker = 'badowner'; Config = $age1; Entries = $rootSeed; Steps = @((New-W3Step), (New-W3Step -Dry)) })
    # A copied or restored archive: creation times are the copy's, in no useful order.
    $ca = @((New-W3Seg 'Cam_A' (W3D 5) -Created (W3H 1)), (New-W3Seg 'Cam_A' (W3D 4) -Created (W3H 3)), (New-W3Seg 'Cam_A' (W3D 3) -Created (W3H 2)), (New-W3Seg 'Cam_A' (W3M 1) -Created (W3H 4)))
    [void]$t.Add(@{ Name = 'copied-archive-creation-times'; Check = 'difference'; Expect = @('open-segment'); Marker = 'claim'; Config = $age1; Entries = $ca; Steps = @((New-W3Step)) })
    return $t
}

# ---------------------------------------------------------------------------
# The generated corpus: realistic archives (creation = the instant in the name, distinct write
# times, only names both sides judge alike), seeded, with the anomalies the gates exist for.
# ---------------------------------------------------------------------------

function Get-W3Corpus {
    param([int]$Count, [int]$Seed)
    $rng = New-Object System.Random($Seed)
    $pool = @('Cam_A', 'Front_Entrance', 'Workshop', 'Yard_2')
    $list = New-Object System.Collections.ArrayList
    for ($n = 0; $n -lt $Count; $n++) {
        $tick = 0
        $entries = @()
        $cams = @($pool | Sort-Object { $rng.Next() } | Select-Object -First ($rng.Next(1, 4)))
        foreach ($cam in $cams) {
            $last = $rng.Next(0, 120)
            if ($rng.Next(0, 8) -eq 0) { $last = $rng.Next(2, 6) * 86400 }
            $segCount = $rng.Next(1, 9)
            for ($i = 0; $i -lt $segCount; $i++) {
                $start = $last + 8 + $i * 900 + $rng.Next(0, 5)
                if ($i -gt 0 -and $rng.Next(0, 6) -eq 0) { $start += $rng.Next(1, 120) * 3600 }
                $written = $(if ($i -eq 0) { $last } else { $start - 900 })
                switch ($rng.Next(0, 30)) {
                    0 { $written = - $rng.Next(1, 72) * 3600 }
                    1 { $written = $rng.Next(1, 240) * 3600 }
                }
                $tick++
                $written = $written - $tick / 1000.0
                $entries += New-W3Seg $cam $start -Written $written -Bytes ([long]$rng.Next(4096, 32769) * 65536)
            }
            if ($rng.Next(0, 3) -eq 0) { $entries += New-W3File ("archive/$cam/notes.txt") -Age ($rng.Next(1, 10) * 86400) -Late }
            if ($rng.Next(0, 4) -eq 0) { $entries += New-W3Seg $cam ($rng.Next(1, 10) * 86400) -Ext '.mp4' -Late }
            if ($rng.Next(0, 5) -eq 0) { $entries += New-W3Seg $cam ($rng.Next(1, 10) * 86400 + 1) -Ext '.mkv.tmp' -Late }
            if ($rng.Next(0, 6) -eq 0) { $entries += New-W3Seg 'Other' ($rng.Next(1, 10) * 86400 + 2) -Folder $cam -Late }
        }
        if ($rng.Next(0, 5) -eq 0) { $entries += New-W3Seg 'Cam_A' (5 * 86400) -Folder '_state' -Late }
        $cfg = @{ MaxAgePruneFraction = @(0.3, 0.5, 1.0)[$rng.Next(0, 3)]; PruneHysteresisGB = @(0, 0.5, 2, 5)[$rng.Next(0, 4)] }
        if ($rng.Next(0, 2) -eq 0) { $cfg.MaxRetentionDays = @(0.5, 1, 2, 3)[$rng.Next(0, 4)] }
        $free = $rng.Next(64, 64 * 40) / 64.0
        $cfg.ReserveGB = [math]::Max(0, [math]::Floor($free) + $rng.Next(-24, 25) / 4.0)
        $shift = $(if ($rng.Next(0, 20) -eq 0) { 30 * 86400 } else { 0 })
        [void]$list.Add(@{ Name = ('corpus-{0:000}' -f $n); Check = 'corpus'; Marker = 'claim'; Config = $cfg; Entries = $entries
                Steps = @((New-W3Step -Dry -Free $free -Shift $shift), (New-W3Step -Free $free -Shift $shift)) })
    }
    return $list
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

$guarded = Test-W3SandboxGuard
$goldenRoot = Join-Path -Path (Get-HolocronRepo) -ChildPath 'testdata\golden\prune'
$target = $goldenRoot
$compare = $null
if ($Verify) { $compare = New-HolocronSandbox; $target = $compare }
[void](New-Item -ItemType Directory -Path $script:W3Base -Force)
$started = [DateTime]::Now
try {
    $tableDir = Join-Path $target 'table'
    if (-not $Verify -and (Test-Path -LiteralPath $tableDir)) { Get-ChildItem -LiteralPath $tableDir -Filter '*.json' | Remove-Item -Force }
    [void](New-Item -ItemType Directory -Path $tableDir -Force)
    $number = 0
    $table = Get-W3Table
    foreach ($scenario in $table) {
        $number++
        $doc = Invoke-W3Scenario -Scenario $scenario -Number $number
        Save-HolocronText -Path (Join-Path $tableDir ($scenario.Name + '.json')) -Text ((ConvertTo-W3Json -InputObject $doc) + "`n")
    }

    # pruneconfig: MaxAgePruneFraction validation at load (Get-CamConfig over sandbox files).
    $cfgDir = Assert-W3Sandbox -Path (Join-Path $script:W3Base 'config')
    [void](New-Item -ItemType Directory -Path $cfgDir -Force)
    $cases = @()
    foreach ($text in @('0', '-1', '1.5', '"abc"', '1.0', '0.5', $null)) {
        $path = Join-Path $cfgDir 'recorder.config.json'
        $json = $(if ($null -eq $text) { '{ "ReserveGB": 20 }' } else { '{ "MaxAgePruneFraction": ' + $text + ' }' })
        [System.IO.File]::WriteAllText($path, $json)
        $case = [ordered]@{ Json = $json; Value = $null; Error = '' }
        try { $case.Value = [double](Get-CamConfig -Path $path).MaxAgePruneFraction } catch { $case.Error = $_.Exception.Message }
        $cases += $case
    }
    Remove-W3Tree -Path $cfgDir
    Save-HolocronText -Path (Join-Path $tableDir 'pruneconfig.json') -Text ((ConvertTo-W3Json -InputObject ([ordered]@{ Name = 'pruneconfig'; Check = 'pruneconfig'; Cases = $cases })) + "`n")

    # The generated corpus, one tree per line.
    $lines = New-Object System.Text.StringBuilder
    foreach ($scenario in (Get-W3Corpus -Count $CorpusCount -Seed $Seed)) {
        $number++
        $doc = Invoke-W3Scenario -Scenario $scenario -Number $number
        [void]$lines.Append((ConvertTo-W3Json -InputObject $doc -Compress)).Append("`n")
    }
    Save-HolocronText -Path (Join-Path $target 'corpus.jsonl') -Text $lines.ToString()

    $left = @(Get-ChildItem -LiteralPath $script:W3Base -Force -ErrorAction SilentlyContinue)
    Write-Host ("prune-diff: {0} table scenarios, {1} corpus trees (seed {2}), sandbox guard refused {3} real paths, {4} sandbox entries left, {5:0} s" -f `
        $table.Count, $CorpusCount, $Seed, $guarded, $left.Count, ([DateTime]::Now - $started).TotalSeconds)

    if ($Verify) {
        $differences = 0
        $checked = 0
        foreach ($file in @(Get-ChildItem -LiteralPath $target -Recurse -File)) {
            $relative = $file.FullName.Substring($target.Length).TrimStart('\')
            $committed = Join-Path $goldenRoot $relative
            $checked++
            if (-not (Test-Path -LiteralPath $committed)) { Write-Host "DIFF  $relative : not committed"; $differences++; continue }
            if ((Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash -ne (Get-FileHash -LiteralPath $committed -Algorithm SHA256).Hash) { Write-Host "DIFF  $relative : bytes differ"; $differences++ }
        }
        foreach ($file in @(Get-ChildItem -LiteralPath $goldenRoot -Recurse -File | Where-Object { $_.Extension -eq '.json' -or $_.Extension -eq '.jsonl' })) {
            $relative = $file.FullName.Substring($goldenRoot.Length).TrimStart('\')
            if (-not (Test-Path -LiteralPath (Join-Path $target $relative))) { Write-Host "DIFF  $relative : committed but not produced"; $differences++ }
        }
        Write-Host ("verify: {0} generated files compared, {1} differences" -f $checked, $differences)
        if ($differences -gt 0) { exit 1 }
    }
}
finally {
    if (Test-Path -LiteralPath $script:W3Base) { Remove-CamTestTree -Path $script:W3Base }
    if ($compare) { Remove-HolocronSandbox -Path $compare }
}
