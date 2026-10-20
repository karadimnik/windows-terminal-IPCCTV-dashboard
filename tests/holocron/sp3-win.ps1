# SP3 driver (Windows): one case per call. The supervisor (spikes/sp3) is hard-killed with TerminateProcess (Stop-Process -Force) and the pids of
# its whole tree, snapshotted before the kill, are watched for 5 s. Only pids read from the supervisor's own state file or found as
# descendants of it are ever touched; survivors are killed by pid at the end. No window is opened (everything is hidden/redirected).
param(
  [Parameter(Mandatory)][ValidateSet('w1','w2','w3','w4','w5','w6','w7','w8','w9','w2a','w2b')][string]$Case,
  [string]$Exe = 'C:\Windows\Temp\holocron-sp2\sp3.exe',
  [string]$Base = 'C:\Windows\Temp\holocron-sp2\sp3',
  [int]$KillOffsetMs = 0
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'sp23-lib.ps1')
$dir = Join-Path $Base $Case; New-Item -ItemType Directory -Force $dir | Out-Null
Get-ChildItem $dir -ErrorAction SilentlyContinue | Remove-Item -Force
$sup = @('supervise','-dir',$dir,'-tag',$Case)
$settle = 1500; $wrap = $false; $reg = $false
switch ($Case) {
  'w1' { $sup += '-n','3','-assign','suspended' }
  'w2' { $sup += '-n','3','-kind','grand','-assign','after','-gap','300' }
  'w2a' { $sup += '-n','3','-kind','direct','-assign','after','-gap','300' }
  'w2b' { $sup += '-n','3','-kind','grand','-assign','after','-gap','0' }
  'w3' { $sup += '-n','3','-kind','grand','-assign','suspended','-gap','300' }
  'w4' { $sup += '-n','3','-kind','grand','-assign','joblist','-gap','300' }
  'w5' { $sup += '-n','3','-kind','leafbreak','-breakaway' }
  'w6' { $sup += '-n','3','-kind','leafbreak' }
  'w7' { $sup += '-n','3','-kind','grand','-assign','suspended'; $wrap = $true }
  'w8' { $sup += '-n','3','-short','1','-shortms','1500'; $settle = 0 }
  'w9' { $sup += '-n','3','-nojob','-reg' }
}
if ($Case -eq 'w9') { $sup += '-assign','none' }
$sp3 = $Exe
function Start-Hidden([string[]]$a, [string]$name) {
  $p = Start-Process $sp3 -ArgumentList $a -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $dir "$name.out") -RedirectStandardError (Join-Path $dir "$name.err")
  $p
}
if ($wrap) { $h = Start-Hidden (@('jobwrap','-dir',$dir,'--',$sp3) + $sup) 'jobwrap' } else { $h = Start-Hidden $sup 'sup' }
$state = Join-Path $dir "$Case.state.json"
$dl = (Get-SpNow) + 15000
while (-not (Test-Path $state) -and (Get-SpNow) -lt $dl) { Start-Sleep -Milliseconds 20 }
if (-not (Test-Path $state)) { throw 'no state file' }
$st = Get-Content $state -Raw | ConvertFrom-Json
$supPid = [int]$st.supervisor
"CASE $Case supervisor pid $supPid  children $($st.children -join ',') roles $($st.roles -join ',')"
Start-Sleep -Milliseconds $settle
$tree = @(Get-SpTree $supPid)
$procs = @([pscustomobject]@{ ProcessId = $supPid; ParentProcessId = 0; Name = 'sp3.exe(supervisor)' }) + $tree
# grandchildren are also taken from the leaves' own logs (their pids are recorded by the leaf itself), in case the CIM parent link is gone
foreach ($f in Get-ChildItem $dir -Filter 'leaf-*.log') { foreach ($l in Get-Content $f.FullName) { if ($l -match 'spawn grandchild .* pid=(\d+)') { $g = [int]$Matches[1]
  if ($g -and -not ($procs | ? { $_.ProcessId -eq $g }) -and (Get-Process -Id $g -ErrorAction SilentlyContinue)) { $procs += [pscustomobject]@{ ProcessId = $g; ParentProcessId = 'leaflog'; Name = 'grandchild(leaf log)' } } } } }
$ids = [int[]]($procs | % { [int]$_.ProcessId })
"snapshot before kill: " + (($procs | % { "$($_.Name)($($_.ProcessId)<-$($_.ParentProcessId))" }) -join ' ')
if ($wrap) { Start-Sleep -Milliseconds 3000 }
$watch = New-Object SpWatch (,$ids)
if ($Case -eq 'w8') { # kill at the moment the short child's own life ends (its own log gives its start instant)
  $lf = Join-Path $dir "leaf-$($st.children[0]).log"
  while (-not (Test-Path $lf)) { Start-Sleep -Milliseconds 5 }
  $target = [long]((Get-Content $lf -TotalCount 1) -split ' ')[0] + 1500 + $KillOffsetMs
  while ((Get-SpNow) -lt $target) { Start-Sleep -Milliseconds 1 }
}
$t0 = Get-SpNow
Stop-Process -Id $supPid -Force
$watch.Poll(5000)
"--- 5 s after TerminateProcess(supervisor):"
$surv = @()
for ($i = 0; $i -lt $ids.Count; $i++) {
  $d = $watch.DeadAt[$i]
  "{0,-34} pid {1,6}  {2}" -f $procs[$i].Name, $ids[$i], $(if ($d -gt 0) { "died +$($d - $t0) ms" } elseif ($d -eq 0) { 'already gone before the kill' } else { $surv += $ids[$i]; 'SURVIVED' })
}
$total = $ids.Count - 1
"RESULT $Case : $(@($surv).Count) survivors of $total non-supervisor processes in the snapshot"
foreach ($f in Get-ChildItem $dir -Filter '*.log') { "--- $($f.Name):"; Get-Content $f.FullName | % { $e,$m = $_ -split ' ',2; if ($e -match '^\d{13}$') { "{0,8}  {1}" -f ([long]$e - $t0), $m } else { $_ } } }
if ($Case -eq 'w8') { $ll = Get-ChildItem $dir -Filter 'leaf-*.log' | % { Get-Content $_.FullName } | ? { $_ -match 'life over' }; "short child exit line present (it exited by itself before being killed): " + [bool]$ll }
if ($Case -eq 'w9') {
  "=== registry reap by a NEW supervisor (children.json written by the killed one) ==="
  $regf = Join-Path $dir "$Case.children.json"
  $decoy = Start-Process 'C:\Windows\System32\PING.EXE' -ArgumentList '-t','127.0.0.1' -WindowStyle Hidden -PassThru
  "decoy (an unrelated ping I started, recorded) pid $($decoy.Id)"
  & $sp3 regfake -reg $regf -pid $decoy.Id -kind wrongstart
  & $sp3 regfake -reg $regf -pid $decoy.Id -kind wrongimage
  & $sp3 regfake -reg $regf -pid $decoy.Id -kind dead
  Get-Content $regf -Raw
  & $sp3 reap -reg $regf
  Start-Sleep -Milliseconds 500
  "after reap: orphans alive = " + @($surv | ? { Get-Process -Id $_ -ErrorAction SilentlyContinue }).Count + " ; decoy alive = " + [bool](Get-Process -Id $decoy.Id -ErrorAction SilentlyContinue)
  Stop-Process -Id $decoy.Id -Force -ErrorAction SilentlyContinue; "cleanup: decoy $($decoy.Id) killed by pid"
}
foreach ($s in $surv) { if (Get-Process -Id $s -ErrorAction SilentlyContinue) { Stop-Process -Id $s -Force; "cleanup: killed survivor pid $s" } }
Start-Sleep -Milliseconds 300
if ($wrap -and (Get-Process -Id $h.Id -ErrorAction SilentlyContinue)) { Stop-Process -Id $h.Id -Force; "cleanup: killed jobwrap $($h.Id)" }
