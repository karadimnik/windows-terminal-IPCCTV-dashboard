# After the matrix, with ICE-over-TCP enabled in the sandbox (rig-up.ps1): (1) decoder-implementation runs, one tile, capture on,
# hardware decode on and off, 360p and 1440p; (2) Spike A again WITHOUT capture so it uses the same UDP/mDNS path as the matrix.
param([string[]]$Browsers = @('chrome','edge'))
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
Keep-Awake
$sw = [Diagnostics.Stopwatch]::StartNew()
$measure = Join-Path $SpikeDir 'measure.ps1'; $pub = Join-Path $SpikeDir 'pub.ps1'
& $pub -Action start -Set m -Count 1 | Out-Null; & $pub -Action start -Set s -Count 1 | Out-Null
foreach($b in $Browsers){ foreach($hw in 'on','off'){ foreach($t in 's1','m1'){
  & $measure -Browser $b -Run "I2-$b-hw$hw-$t" -Tiles $t -Layout single -Hw $hw -WarmupSec 15 -SampleSec 10 -Cap | Out-Null; "{0:n0}s I2-$b-hw$hw-$t done" -f $sw.Elapsed.TotalSeconds } } }
& $pub -Action stop -Set m | Out-Null; & $pub -Action stop -Set s | Out-Null
& (Join-Path $SpikeDir 'run-spike-a.ps1') -Browsers $Browsers -NoCap -Prefix A2
"{0:n0}s EXTRA COMPLETE" -f $sw.Elapsed.TotalSeconds
