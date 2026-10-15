# Baseline: six mpv instances reading the sandbox MediaMTX over RTSP, flags copied from the $Flags line of
# mpv.cam.autolayout.autorestart.ps1 (original repo, read-only) plus its Traffic overlay, tiled 3x2 on 1920x1080
# (640x540 panes, as the dashboard computes for six streams). No --hwdec, as in the dashboard. Added by this spike and NOT
# in the dashboard: --ontop (the Operator's windows must not occlude the panes) and --input-ipc-server (to read drop counters).
param([Parameter(Mandatory)][string]$Run, [Parameter(Mandatory)][string]$Tiles, [int]$WarmupSec = 30, [int]$SampleSec = 180)
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
. (Join-Path $SpikeDir 'sampler.ps1')
Keep-Awake
$paths = $Tiles -split ','
$flags = "--no-border --no-audio --profile=low-latency --rtsp-transport=tcp --correct-pts=no --display-fps-override=1 --network-timeout=3 --demuxer-lavf-o=timeout=5000000 --msg-level=all=error --force-window=immediate"
$overlay = "--osd-level=1 --osd-msg1=`"Traffic: `${video-bitrate}`""
$pids = New-Object System.Collections.Generic.List[int]; $pipes = @()
$meta = [ordered]@{ run=$Run; browser='mpv'; hw='n/a (software, no --hwdec, as dashboard)'; tiles=$Tiles; layout='3x2 640x540'; warmup_s=$WarmupSec; sample_s=$SampleSec }
function Get-MpvProps($pipe) {
  $o = @{}
  try {
    $c = New-Object System.IO.Pipes.NamedPipeClientStream('.', $pipe, [System.IO.Pipes.PipeDirection]::InOut); $c.Connect(3000)
    $w = New-Object System.IO.StreamWriter($c); $w.AutoFlush = $true; $r = New-Object System.IO.StreamReader($c)
    $i = 0
    foreach($p in 'frame-drop-count','decoder-frame-drop-count','estimated-vf-fps','hwdec-current','width','height','video-codec'){
      $i++; $w.WriteLine((@{ command = @('get_property',$p); request_id = $i } | ConvertTo-Json -Compress))
      for($k=0;$k -lt 8;$k++){ $t = $r.ReadLineAsync(); if(-not $t.Wait(800)){ break }; $j = $t.Result | ConvertFrom-Json; if($j.request_id -eq $i){ $o[$p] = $j.data; break } }
    }
    $c.Dispose()
  } catch { $o.err = $_.Exception.Message }
  $o
}
try {
  for($i=0;$i -lt $paths.Count;$i++){
    $x = ($i % 3) * 640; $y = [math]::Floor($i / 3) * 540
    $pipe = "s0mpv-$Run-$i"; $pipes += $pipe
    $a = "$flags $overlay --geometry=640x540+$x+$y --ontop --input-ipc-server=$pipe rtsp://127.0.0.1:$($Mtx.Rtsp)/$($paths[$i])"
    $p = Start-Visible 'C:\mpv\mpv.exe' $a $Rt "mpv-$Run-$i"; $pids.Add($p.Id)
  }
  $meta.launch_ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds(); $meta.root_pids = ($pids -join ',')
  Start-Sleep -Seconds $WarmupSec
  Save-Screenshot (Join-Path $DataDir "$Run.png")
  $before = @(); foreach($pp in $pipes){ $before += ,(Get-MpvProps $pp) }
  $meta.sample_start_ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  Invoke-Sampling -GetPids { ,@($pids | ? { Get-Process -Id $_ -ErrorAction SilentlyContinue }) } -Seconds $SampleSec -OutFile (Join-Path $DataDir "$Run.samples.jsonl")
  $meta.sample_end_ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  $after = @(); foreach($pp in $pipes){ $after += ,(Get-MpvProps $pp) }
  $mp = @(); for($i=0;$i -lt $paths.Count;$i++){ $mp += [ordered]@{ path=$paths[$i]; before=$before[$i]; after=$after[$i] } }
  $meta.mpv = $mp
  $meta.alive_at_end = @($pids | ? { Get-Process -Id $_ -ErrorAction SilentlyContinue }).Count
} finally {
  foreach($id in $pids){ Stop-Process -Id $id -Force -ErrorAction SilentlyContinue }
  ($meta | ConvertTo-Json -Compress -Depth 6) | Set-Content (Join-Path $DataDir "$Run.meta.json") -Encoding ascii
}
"mpv baseline $Run done"
