<#
  SP7 probe: drive Invoke-CamPrune over one synthetic archive and record its decisions, through the same mechanism the
  prune* checks use (tests\cam.testlib.ps1: Get-CamTestRecorderFunctions, New-CamTestPruneRoot, New-CamTestSegment, a
  Get-CamFreeSpaceGB defined in the caller's scope). New file; edits nothing. Run on a COPY of cam.common.ps1,
  cam.recorder.ps1 and tests\cam.testlib.ps1 (see -Repo), with TEMP pointed at a scratch folder.
  usage: powershell -NoProfile -ExecutionPolicy Bypass -File sp7-probe.ps1 -Repo C:\Windows\Temp\holocron-sp\sp7repo -Temp C:\Windows\Temp\holocron-sp\sp7tmp
#>
param([Parameter(Mandatory = $true)][string]$Repo, [Parameter(Mandatory = $true)][string]$Temp)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $Temp | Out-Null
$env:TEMP = $Temp; $env:TMP = $Temp
. (Join-Path $Repo 'tests\cam.testlib.ps1')
. (Get-CamTestRecorderFunctions -Name @('Invoke-CamPrune', 'Get-CamSegmentDirectories', 'Remove-CamEmptySegment', 'Write-CamHost'))

$unit = 16MB   # one segment; a unit of fake free space is its length in GB, because the dry run estimates with Length/1GB
$t = New-CamTestPruneRoot -Name 'sp7' -Config @{ MaxRetentionDays = 1; MaxAgePruneFraction = 1.0; ReserveGB = 10.085; PruneHysteresisGB = 0.02 }
$now = [DateTime]::Now
$files = [ordered]@{}
foreach ($c in @(@{ N = 'Cam_A'; H = @(40, 30, 20, 15) }, @{ N = 'Cam_B'; H = @(38, 28, 18, 14) })) {
  $dir = Join-Path $t.Root $c.N
  foreach ($h in $c.H) { $files["$($c.N)-$h"] = New-CamTestSegment -Dir $dir -Camera $c.N -Time $now.AddHours(-$h) -Bytes $unit }
  $files["$($c.N)-new"] = New-CamTestSegment -Dir $dir -Camera $c.N -Time $now.AddMinutes(-1) -Bytes $unit
}
[void](Initialize-CamArchiveOwner -Config $t.Config)   # claim first: a foreign file in the root would block the claim (that is a guard, not a bug)
Set-Content -LiteralPath (Join-Path $t.Root 'Cam_A\notes.txt') -Value 'foreign file'

$fakeBase = 10.0
$sizeOf = { [double](@(Get-ChildItem -LiteralPath $t.Root -Recurse -File -Force | Where-Object { $_.Extension -eq '.mkv' } | Measure-Object -Property Length -Sum).Sum) }
$initial = & $sizeOf
function Get-CamFreeSpaceGB { param([string]$Path) return $fakeBase + (($initial - (& $sizeOf)) / 1GB) }

function Show($label, $r) {
  "== $label"
  "Candidates=$($r.Candidates) Deleted=$($r.Deleted) FreedBytes=$($r.FreedBytes) Blocked=$($r.Blocked) Failed=$($r.Failed) RootRefusal='$($r.RootRefusal)' ClockRefusal='$($r.ClockRefusal)'"
  "FreeGBBefore=$($r.FreeGBBefore) FreeGBAfter=$($r.FreeGBAfter) Reserve=$($r.ReserveGB) Target=$($r.TargetGB)"
  "Protected: " + (($r.Protected | ForEach-Object { Split-Path $_ -Leaf }) -join ', ')
  "Foreign: " + ($r.Foreign -join ' | ')
  foreach ($d in $r.Deletions) { "  " + ($d -replace [regex]::Escape($t.Root), '<root>') }
  "StopLine: " + $r.StopLine
  "Message: " + $r.Message
}
$before = Get-CamTestFileManifest -Path $t.Dir
$dry = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log -DryRun
$dryDiff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $t.Dir)
Show 'DRY RUN' $dry
"filesystem changed by the dry run: removed=$($dryDiff.Removed.Count) changed=$($dryDiff.Changed.Count) added=$($dryDiff.Added.Count)"
$real = Invoke-CamPrune -Config $t.Config -SupervisorLog $t.Log
$diff = Compare-CamTestFileManifest -Before $before -After (Get-CamTestFileManifest -Path $t.Dir)
Show 'REAL RUN (inside the sandbox)' $real
"filesystem removed by the real run (oldest first by name): " + (($diff.Removed | ForEach-Object { Split-Path $_ -Leaf } | Sort-Object) -join ', ')
$names = { param($r) ($r.Deletions | ForEach-Object { if ($_ -match '^(?:would delete|prune: deleted) (.+?) \(\d+ bytes') { Split-Path $Matches[1] -Leaf } }) -join ' > ' }
"dry order : " + (& $names $dry)
"real order: " + (& $names $real)
"same order: " + ((& $names $dry) -eq (& $names $real))
"== supervisor log written by the pruner (" + $t.Log + ")"
Get-Content -LiteralPath $t.Log | ForEach-Object { $_ -replace [regex]::Escape($t.Root), '<root>' }
"== result keys: " + (($real.Keys | Sort-Object) -join ', ')
Remove-CamTestTree -Path $t.Dir
if (Test-Path -LiteralPath $t.Log) { Remove-Item -LiteralPath $t.Log -Force }
'sandbox removed'
