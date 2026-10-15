# Spike B matrix + mpv baseline, ONE process (so Keep-Awake holds), results to files under $env:TEMP\stage0\data.
# Order = value first: noise floor, sub-streams (browsers then mpv), mains (browsers then mpv), 720p, 5 sub + 1 main, then the hidden-tab runs.
# The matrix cells run WITHOUT -Cap: with a fake audio capture Chrome offers every interface's address and six simultaneous
# tiles often failed to reach the loopback-only MediaMTX (ICE failed). Decoder implementation is therefore read from separate
# single-tile -Cap runs (I-*), which is the only way Chromium exposes it in getStats.
# Firefox is not in the matrix: it could not connect to the loopback-only sandbox (see the report).
param([string[]]$Browsers = @('chrome','edge'), [int]$WarmupSec = 30, [int]$SampleSec = 180, [switch]$SkipHidden)
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
. (Join-Path $SpikeDir 'sampler.ps1')
Keep-Awake
$sw = [Diagnostics.Stopwatch]::StartNew()
function Say($m) { '{0,5:n0}s {1}' -f $sw.Elapsed.TotalSeconds, $m }
$measure = Join-Path $SpikeDir 'measure.ps1'; $pub = Join-Path $SpikeDir 'pub.ps1'; $mpvb = Join-Path $SpikeDir 'mpv-baseline.ps1'

# noise floor: the rig (MediaMTX + 6 publishers) and whatever else the PC is doing, no browser
& $pub -Action start -Set s | Out-Null
$m = [ordered]@{ run='B-idle'; browser='none'; hw='-'; tiles='(rig + PC only)'; layout='-'; sample_s=60 }
$m.sample_start_ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
Invoke-Sampling -GetPids { ,@($PID) } -Seconds 60 -OutFile (Join-Path $DataDir 'B-idle.samples.jsonl')
$m.sample_end_ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds(); ($m | ConvertTo-Json -Compress) | Set-Content (Join-Path $DataDir 'B-idle.meta.json') -Encoding ascii
Say 'idle done'
# decoder implementation, single tile, capture on (Chromium only exposes decoderImplementation then)
& $pub -Action start -Set m -Count 1 | Out-Null
foreach($b in $Browsers){ foreach($hw in 'on','off'){ foreach($t in 's1','m1'){
  & $measure -Browser $b -Run "I-$b-hw$hw-$t" -Tiles $t -Layout single -Hw $hw -WarmupSec 15 -SampleSec 10 -Cap | Out-Null; Say "I-$b-hw$hw-$t done" } } }
& $pub -Action stop -Set m | Out-Null

function Run-Layout($tag, $tiles, $layout, $sets) {
  foreach($b in $Browsers){ foreach($hw in 'on','off'){
    $run = "B-$tag-$b-hw$hw"
    & $measure -Browser $b -Run $run -Tiles $tiles -Layout $layout -Hw $hw -WarmupSec $WarmupSec -SampleSec $SampleSec | Out-Null
    Say "$run done"
  } }
}
# (ii) six 640x360
Run-Layout 'sub6' 's1,s2,s3,s4,s5,s6' 'grid'
& $mpvb -Run 'B-sub6-mpv' -Tiles 's1,s2,s3,s4,s5,s6' -WarmupSec $WarmupSec -SampleSec $SampleSec | Out-Null; Say 'mpv sub6 done'
& $pub -Action stop -Set s | Out-Null
# (i) six 2560x1440
& $pub -Action start -Set m | Out-Null
Run-Layout 'main6' 'm1,m2,m3,m4,m5,m6' 'grid'
& $mpvb -Run 'B-main6-mpv' -Tiles 'm1,m2,m3,m4,m5,m6' -WarmupSec $WarmupSec -SampleSec $SampleSec | Out-Null; Say 'mpv main6 done'
& $pub -Action stop -Set m | Out-Null
# (iii) six 1280x720
& $pub -Action start -Set h | Out-Null
Run-Layout 'hd6' 'h1,h2,h3,h4,h5,h6' 'grid'
& $pub -Action stop -Set h | Out-Null
# (iv) 5 sub + 1 main focused (3x3, main spans 2x2)
& $pub -Action start -Set m -Count 1 | Out-Null; & $pub -Action start -Set s -Count 5 | Out-Null
Run-Layout 'focus' 'm1,s1,s2,s3,s4,s5' 'focus'
& $pub -Action stop -Set m | Out-Null; & $pub -Action stop -Set s | Out-Null
# hidden-tab runs on the mains (worst case): 60 s visible, minimize, ~60 s hidden
if(-not $SkipHidden){ & $pub -Action start -Set m | Out-Null; foreach($b in $Browsers){ & $measure -Browser $b -Run "B-main6-$b-hidden" -Tiles 'm1,m2,m3,m4,m5,m6' -Layout grid -Hw on -WarmupSec $WarmupSec -SampleSec 130 -HideAtSec 60 | Out-Null; Say "hidden $b done" }; & $pub -Action stop -Set m | Out-Null }
Say 'MATRIX COMPLETE'
