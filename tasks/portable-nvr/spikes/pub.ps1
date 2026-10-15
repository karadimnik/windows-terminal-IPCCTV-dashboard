# Start or stop the six -c copy publishers of one clip set (m = 2560x1440, h = 1280x720, s = 640x360) -> paths m1..m6 etc.
# Each publisher is: ffmpeg -re -stream_loop -1 -i clip -c copy -f rtsp -rtsp_transport tcp rtsp://127.0.0.1:18554/<path>
param([Parameter(Mandatory)][ValidateSet('start','stop')][string]$Action, [Parameter(Mandatory)][string[]]$Set, [int]$Count = 6)
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
$t0File = Join-Path $Rt 't0.json'
foreach($k in $Set){
  if($Action -eq 'start'){
    $t0 = @{}; $cur = Get-Content $t0File -Raw | ConvertFrom-Json; foreach($pr in $cur.PSObject.Properties){ $t0[$pr.Name] = $pr.Value }
    $clip = Join-Path $ClipDir ("clip-{0}x{1}.mkv" -f $ClipSets[$k].W,$ClipSets[$k].H)
    for($i=1;$i -le $Count;$i++){
      $name = "$k$i"
      $argLine = '-hide_banner -loglevel error -re -stream_loop -1 -i "{0}" -c copy -f rtsp -rtsp_transport tcp rtsp://127.0.0.1:{1}/{2}' -f $clip,$Mtx.Rtsp,$name
      $t0[$name] = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
      $null = Start-Hidden $Ffmpeg $argLine $Rt "pub-$name"
      Start-Sleep -Milliseconds 120
    }
    ($t0 | ConvertTo-Json) | Set-Content $t0File -Encoding ascii
    for($w=0;$w -lt 40;$w++){ $items = @((Invoke-RestMethod "http://127.0.0.1:$($Mtx.Api)/v3/paths/list").items | ? { $_.name -like "$k*" -and $_.ready }); if($items.Count -ge $Count){ break }; Start-Sleep -Milliseconds 250 }
    # t0[name] = process start (clip time 0 is at or just after it: upper bound for latency); t0[name_ready] = API readyTime (later than clip time 0).
    foreach($it in $items){ $t0[$it.name + '_ready'] = ([DateTimeOffset]::Parse([string]$it.readyTime)).ToUnixTimeMilliseconds() }
    ($t0 | ConvertTo-Json) | Set-Content $t0File -Encoding ascii
    "set $k started, $($items.Count) of $Count paths ready"
  } else {
    foreach($e in @((Get-State).pids)){ if($e.what -match "^pub-$k\d$"){ $p = Get-Process -Id $e.pid -ErrorAction SilentlyContinue; if($p -and $p.ProcessName -eq 'ffmpeg'){ Stop-Process -Id $e.pid -Force } } }
    "set $k stopped"
  }
}
