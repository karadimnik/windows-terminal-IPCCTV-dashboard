# SP2 driver: one close experiment per call. Routes: wt (WM_CLOSE to our Windows Terminal window), wtkill (kill our tab's OpenConsole),
# conhost (WM_CLOSE to a classic console window), conhostkill (kill that window's conhost.exe), ctrlc / ctrlbreak (GenerateConsoleCtrlEvent, hidden console).
# Safety: windows are found by EXACT title+class, closed only by handle, and only after the owner is proven ours; processes are touched only by
# pids read from the pid file sp2 wrote itself. Everything left over is killed by pid at the end.
param(
  [Parameter(Mandatory)][ValidateSet('wt','wtkill','conhost','conhostkill','ctrlc','ctrlbreak')][string]$Route,
  [string]$Exe = 'C:\Windows\Temp\holocron-sp2\sp2.exe',
  [string]$Base = 'C:\Windows\Temp\holocron-sp2'
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'sp23-lib.ps1')
$dir = Join-Path $Base "r-$Route"; New-Item -ItemType Directory -Force $dir | Out-Null
$title = if ($Route -like 'conhost*') { 'holocron-sp2c' } else { 'holocron-sp2' }
if ($Route -in 'wt','wtkill','conhost','conhostkill' -and @([SpWin]::Find($null, $title)).Count) { throw "a window titled $title already exists; refusing" }
$tag = $Route; $log = Join-Path $dir "sp2-$tag.log"; $pidf = Join-Path $dir "sp2-$tag.pids"
Remove-Item -Force -ErrorAction SilentlyContinue $log, $pidf
$spArgs = @('-dir', $dir, '-tag', $tag, '-max', '35')
$ocBefore = @(Get-Process OpenConsole -ErrorAction SilentlyContinue | % Id)
$chBefore = @(Get-Process conhost -ErrorAction SilentlyContinue | % Id)
$tLaunch = Get-SpNow
$visFirst = $null; $visMin = $null; $hwnd = 0; $owner = 0
switch -Wildcard ($Route) {
  'wt*'      { Start-Process wt.exe -WindowStyle Minimized -ArgumentList (@('-w','holocron-sp2','new-tab','--title','holocron-sp2','--suppressApplicationTitle',$Exe) + $spArgs) }
  'conhost*' { Start-Process cmd.exe -WindowStyle Hidden -ArgumentList (@('/c','start','"holocron-sp2c"','/min',$Exe) + $spArgs) }
  'ctrl*'    { $null = Start-Process $Exe -WindowStyle Hidden -ArgumentList $spArgs }
}
# wait for sp2 to report its pids; while waiting, minimize our window the moment it shows
$deadline = (Get-SpNow) + 20000
while (-not (Test-Path $pidf) -and (Get-SpNow) -lt $deadline) {
  if ($Route -notlike 'ctrl*') {
    foreach ($w in [SpWin]::Find($null, $title)) { $f = $w.Split('|'); $hwnd = [long]$f[0]; $owner = [int]$f[1]
      if ($f[5] -eq 'False' -and -not $visFirst) { $visFirst = Get-SpNow; [SpWin]::Minimize($hwnd); $visMin = Get-SpNow } }
  }
  Start-Sleep -Milliseconds 15
}
if (-not (Test-Path $pidf)) { throw 'sp2 never wrote its pid file' }
Start-Sleep -Milliseconds 600
if ($Route -notlike 'ctrl*') {
  foreach ($w in [SpWin]::Find($null, $title)) { $f = $w.Split('|'); $hwnd = [long]$f[0]; $owner = [int]$f[1]; $cls = $f[2]
    if ($f[5] -eq 'False') { if (-not $visFirst) { $visFirst = Get-SpNow }; [SpWin]::Minimize($hwnd); $visMin = Get-SpNow } }
}
$p = @{}; (Get-Content $pidf) -split ' ' | % { $k,$v = $_ -split '='; $p[$k] = [int]$v }
"ROUTE $Route  sp2=$($p.sp2) shared=$($p.shared) own=$($p.own)  launch->pidfile ok"
$logTxt = Get-Content $log
"--- sp2 start line:"; $logTxt | select -First 1
# prove ownership
$chain = @(); $q = $p.sp2
for ($i = 0; $i -lt 6 -and $q; $i++) { $pr = Get-SpProc $q; if (-not $pr) { break }; $chain += "$($pr.Name)($($pr.ProcessId))"; $q = $pr.ParentProcessId }
"ancestor chain of sp2: " + ($chain -join ' <- ')
$ownerName = if ($owner) { (Get-Process -Id $owner -ErrorAction SilentlyContinue).ProcessName } else { '' }
if ($Route -notlike 'ctrl*') { "window: hwnd=$hwnd owner=$owner($ownerName) class=$cls title='$title'" }
$consoleHwnd = [regex]::Match($logTxt[0], 'consoleHwnd=(0x[0-9a-f]+)').Groups[1].Value
if ($Route -like 'conhost*') {
  if ([Convert]::ToInt64($consoleHwnd,16) -ne $hwnd) { throw "console window of sp2 ($consoleHwnd) is not the window found by title ($hwnd); not closing anything" }
  "proof: sp2's GetConsoleWindow == found window ($consoleHwnd)"
}
if ($Route -like 'wt*') {
  if ($ownerName -ne 'WindowsTerminal') { throw "window owner is $ownerName, not WindowsTerminal; not closing anything" }
  if (($chain -join ' ') -notmatch "WindowsTerminal\.exe\($owner\)") { "note: WindowsTerminal($owner) is not in sp2's ancestor chain" }
}
"visible window time: first-seen-not-iconic -> minimized = " + $(if ($visFirst) { "$($visMin - $visFirst) ms" } else { '0 ms (appeared minimized / none)' })
$wt = $null
$ids = @($p.sp2, $p.shared, $p.own)
"new console hosts since launch: " + ((Get-CimInstance Win32_Process | ? { ($_.Name -eq 'conhost.exe' -and $chBefore -notcontains [int]$_.ProcessId) -or ($_.Name -eq 'OpenConsole.exe' -and $ocBefore -notcontains [int]$_.ProcessId) } | % { "$($_.Name)($($_.ProcessId)) ppid=$($_.ParentProcessId)" }) -join '; ')
$watch = New-Object SpWatch (,[int[]]$ids)
Start-Sleep -Milliseconds 300
$tAct = Get-SpNow
switch ($Route) {
  'wt'          { [void][SpWin]::Close($hwnd); $what = 'PostMessage WM_CLOSE to WT window' }
  'conhost'     { [void][SpWin]::Close($hwnd); $what = 'PostMessage WM_CLOSE to console window' }
  'wtkill'      { # the OpenConsole that hosts sp2: owner of sp2's pseudo console window
                  # the OpenConsole hosting sp2 = the ONLY OpenConsole that is new since before launch, child of the WT process, born within 3 s of sp2
                  $new = @(Get-CimInstance Win32_Process -Filter "Name='OpenConsole.exe'" | ? { $ocBefore -notcontains [int]$_.ProcessId })
                  $sp2p = Get-SpProc $p.sp2
                  if ($new.Count -ne 1) { throw "expected exactly 1 new OpenConsole, found $($new.Count); not killing anything" }
                  $ocp = $new[0]; $ocPid = [int]$ocp.ProcessId
                  if ($ocp.ParentProcessId -ne $owner) { throw "OpenConsole $ocPid parent is $($ocp.ParentProcessId), not the WT process $owner" }
                  if ([math]::Abs(($sp2p.CreationDate - $ocp.CreationDate).TotalSeconds) -gt 3) { throw 'OpenConsole not born next to sp2' }
                  Stop-Process -Id $ocPid -Force; $what = "TerminateProcess OpenConsole.exe pid $ocPid (hosts only sp2's tab; parent WT $owner NOT touched)" }
  'conhostkill' { $new = @(Get-CimInstance Win32_Process -Filter "Name='conhost.exe'" | ? { $chBefore -notcontains [int]$_.ProcessId -and $_.ParentProcessId -eq $p.sp2 })
                  if ($new.Count -ne 1) { throw "expected exactly 1 new conhost whose parent is sp2, found $($new.Count); not killing anything" }
                  $chPid = [int]$new[0].ProcessId
                  Stop-Process -Id $chPid -Force; $what = "TerminateProcess conhost.exe pid $chPid (new since launch, parent = sp2)" }
  'ctrlc'       { $tAct = Get-SpNow; $o = & $Exe -sendctrl $p.sp2 c; $what = "GenerateConsoleCtrlEvent CTRL_C: $o" }
  'ctrlbreak'   { $tAct = Get-SpNow; $o = & $Exe -sendctrl $p.sp2 break; $what = "GenerateConsoleCtrlEvent CTRL_BREAK: $o" }
}
"ACTION at t0: $what"
$watch.Poll(15000)
Start-Sleep -Milliseconds 500
"--- deaths (ms after action t0):"
$names = 'sp2','child-sharedConsole','child-ownConsole'
for ($i = 0; $i -lt 3; $i++) { "{0,-22} pid {1,6}  {2}" -f $names[$i], $ids[$i], $(if ($watch.DeadAt[$i] -gt 0) { "died +$($watch.DeadAt[$i] - $tAct) ms" } elseif ($watch.Alive($i)) { 'STILL ALIVE after 15 s' } else { 'already gone' }) }
"--- sp2 log, ms relative to t0 (tick lines thinned to every 4th):"
$n = 0
Get-Content $log | % { $e,$t,$m = $_ -split ' ',3; if ($m -like 'tick*') { $n++; if ($n % 4) { return } }; "{0,8}  {1}" -f ([long]$e - $tAct), $m }
if ($Route -like 'wt*') { "WT window still present after: " + @([SpWin]::Find($null, $title)).Count }
if ($owner) { "window owner process $owner ($ownerName) still alive: " + [bool](Get-Process -Id $owner -ErrorAction SilentlyContinue) }
# cleanup by pid
for ($i = 0; $i -lt 3; $i++) { if ($watch.Alive($i)) { Stop-Process -Id $ids[$i] -Force -ErrorAction SilentlyContinue; "cleanup: killed survivor $($names[$i]) pid $($ids[$i])" } }
if ($Route -like 'conhost*' -or $Route -like 'wt*') {
  foreach ($w in [SpWin]::Find($null, $title)) { $f = $w.Split('|'); if ([int]$f[1] -eq $owner) { [void][SpWin]::Close([long]$f[0]); "cleanup: WM_CLOSE to leftover window $($f[0]) owner $owner"; Start-Sleep 1 } }
}
