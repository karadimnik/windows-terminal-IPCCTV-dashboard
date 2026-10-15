# Out-of-process sampler. Dot-source after common.ps1.
# Invoke-Sampling: every IntervalSec, for the process set returned by -GetPids (refreshed every 10 s):
#   cpu_pct (sum of per-process CPU-time deltas, percent of ONE core), busiest thread (percent of one core),
#   working set, system CPU (GetSystemTimes), rig CPU (mediamtx + publishers from state.json).
# GPU Engine utilization runs in a background runspace (the wildcard counter takes ~2.5 s per read, so GPU samples
# arrive every ~3 s, not 2 s) and is summed per (adapter LUID, engine type) for the test pids ("mine") and all pids ("all").
# Adapter mapping: the LUID is in the counter instance name (luid_0x00000000_0x000149B4); HKLM\SOFTWARE\Microsoft\DirectX
# lists each adapter's Description + AdapterLuid, so LUID -> "NVIDIA GeForce GTX 1070 Ti" / "Intel(R) HD Graphics 530".
function Get-AdapterMap {
  $m = @{}
  foreach($k in Get-ChildItem 'HKLM:\SOFTWARE\Microsoft\DirectX'){ $p = Get-ItemProperty $k.PSPath
    if($p.Description -and $p.AdapterLuid){ $m[('{0:X}' -f [uint64]$p.AdapterLuid)] = $p.Description } }
  $m
}
function Get-ThreadCpu($ids) {
  $d = @{}
  foreach($id in $ids){ $p = Get-Process -Id $id -ErrorAction SilentlyContinue; if(-not $p){ continue }
    $tot = 0.0; try { $tot = $p.TotalProcessorTime.TotalSeconds } catch {}
    $th = @{}; try { foreach($t in $p.Threads){ try { $th[$t.Id] = $t.TotalProcessorTime.TotalSeconds } catch {} } } catch {}
    $d[$id] = @{ cpu = $tot; ws = $p.WorkingSet64; threads = $th } }
  $d
}

$script:GpuSb = {
  param($shared, $adapters, $outFile)
  $lines = New-Object System.Collections.Generic.List[string]
  while(-not $shared.stop){
    $samples = (Get-Counter -Counter '\GPU Engine(*)\Utilization Percentage' -ErrorAction SilentlyContinue).CounterSamples
    $idset = New-Object 'System.Collections.Generic.HashSet[int]'; foreach($i in $shared.ids){ [void]$idset.Add([int]$i) }
    $mine = @{}; $all = @{}
    foreach($s in $samples){
      if($s.InstanceName -match '^pid_(\d+)_luid_0x[0-9A-Fa-f]+_0x([0-9A-Fa-f]+)_phys_\d+_eng_\d+_engtype_(\w+)$'){
        $l = $Matches[2].TrimStart('0').ToUpper(); $name = if($adapters.ContainsKey($l)){ $adapters[$l] } else { "luid$l" }
        $key = $name + '|' + $Matches[3]
        $all[$key] = [double]$all[$key] + $s.CookedValue
        if($idset.Contains([int]$Matches[1])){ $mine[$key] = [double]$mine[$key] + $s.CookedValue } } }
    $rm = @{}; foreach($k in $mine.Keys){ $rm[$k] = [math]::Round($mine[$k],1) }
    $ra = @{}; foreach($k in $all.Keys){ if($all[$k] -ge 0.5){ $ra[$k] = [math]::Round($all[$k],1) } }
    $lines.Add((([ordered]@{ t = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds(); gpu_mine = $rm; gpu_all = $ra }) | ConvertTo-Json -Compress -Depth 4))
  }
  [IO.File]::WriteAllLines($outFile, $lines)
}
function Get-SysTimes { $i=0L;$k=0L;$u=0L; [void][S0.Win]::GetSystemTimes([ref]$i,[ref]$k,[ref]$u); ,@($i,$k,$u) }
function Invoke-Sampling([scriptblock]$GetPids, [int]$Seconds, [string]$OutFile, [int]$IntervalSec = 2, [hashtable]$AtSecond = $null) {
  $adapters = Get-AdapterMap
  $rig = @(); foreach($e in @((Get-State).pids)){ if($e.what -match 'mediamtx|^pub-'){ $rig += [int]$e.pid } }
  $ids = & $GetPids; $lastRefresh = Get-Date
  $shared = [hashtable]::Synchronized(@{ stop = $false; ids = $ids })
  $rs = [powershell]::Create(); [void]$rs.AddScript($script:GpuSb.ToString()).AddArgument($shared).AddArgument($adapters).AddArgument(($OutFile -replace '\.samples\.jsonl$','.gpu.jsonl'))
  $async = $rs.BeginInvoke()
  $prev = Get-ThreadCpu $ids; $prevRig = Get-ThreadCpu $rig; $tPrev = [Diagnostics.Stopwatch]::StartNew(); $st0 = Get-SysTimes
  $end = (Get-Date).AddSeconds($Seconds); $sw = [Diagnostics.Stopwatch]::StartNew(); $fired = $false
  $lines = New-Object System.Collections.Generic.List[string]
  while((Get-Date) -lt $end){
    $tick = [Diagnostics.Stopwatch]::StartNew()
    if($AtSecond -and -not $fired -and $sw.Elapsed.TotalSeconds -ge $AtSecond.Seconds){ & $AtSecond.Action; $fired = $true }
    if(((Get-Date) - $lastRefresh).TotalSeconds -ge 10){ $ids = & $GetPids; $lastRefresh = Get-Date; $shared.ids = $ids }
    $cur = Get-ThreadCpu $ids; $curRig = Get-ThreadCpu $rig; $dt = $tPrev.Elapsed.TotalSeconds; $tPrev.Restart(); $st1 = Get-SysTimes
    $dTot = ($st1[1]-$st0[1]) + ($st1[2]-$st0[2]); $sys = if($dTot -gt 0){ [math]::Round(100*(1 - ($st1[0]-$st0[0])/$dTot),1) } else { $null }; $st0 = $st1
    $cpu = 0.0; $ws = 0.0; $best = 0.0; $bestId = ''
    foreach($id in $cur.Keys){ if($prev.ContainsKey($id)){ $cpu += ($cur[$id].cpu - $prev[$id].cpu)
        foreach($tid in $cur[$id].threads.Keys){ if($prev[$id].threads.ContainsKey($tid)){ $v = $cur[$id].threads[$tid] - $prev[$id].threads[$tid]; if($v -gt $best){ $best = $v; $bestId = "$id/$tid" } } } }
      $ws += $cur[$id].ws }
    $rigCpu = 0.0; foreach($id in $curRig.Keys){ if($prevRig.ContainsKey($id)){ $rigCpu += ($curRig[$id].cpu - $prevRig[$id].cpu) } }
    $rec = [ordered]@{ t = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds(); dt = [math]::Round($dt,2); n = $ids.Count
      cpu_pct = [math]::Round(100*$cpu/$dt,1); busiest_pct = [math]::Round(100*$best/$dt,1); busiest = $bestId; ws_mb = [math]::Round($ws/1MB,0)
      sys_cpu_pct = $sys; rig_cpu_pct = [math]::Round(100*$rigCpu/$dt,1) }
    $lines.Add(($rec | ConvertTo-Json -Compress -Depth 4))
    $prev = $cur; $prevRig = $curRig
    $left = $IntervalSec*1000 - $tick.ElapsedMilliseconds; if($left -gt 0){ Start-Sleep -Milliseconds $left }
  }
  $shared.stop = $true; [void]$rs.EndInvoke($async); $rs.Dispose()
  [IO.File]::WriteAllLines($OutFile, $lines)
}
