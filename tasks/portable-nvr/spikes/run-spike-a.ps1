# Spike A: one live 1440p tile (path m1, layout single) per browser.
#  (1) clean run: 30 s warm-up + 180 s sampled, muted, no probe  -> CPU / GPU / decoder / jitter buffer / drops
#  (2) probe run: unmute at 12 s, barcode latency probe on, 60 s sampled -> audio energy, latency estimate, receive-to-callback
# Runs in ONE powershell process so Keep-Awake holds for the whole spike.
param([string[]]$Browsers = @('chrome','edge','firefox'), [int]$SampleSec = 180, [switch]$NoCap, [string]$Prefix = 'A')
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
Keep-Awake
$sw = [Diagnostics.Stopwatch]::StartNew()
& (Join-Path $SpikeDir 'pub.ps1') -Action start -Set m
foreach($b in $Browsers){
  $cap = @{}; if($b -ne 'firefox' -and -not $NoCap){ $cap['Cap'] = $true }
  if($b -eq 'firefox'){ # does not connect to the loopback-only sandbox (see report): one short run documents it
    & (Join-Path $SpikeDir 'measure.ps1') -Browser $b -Run "$Prefix-$b-clean" -Tiles m1 -Layout single -Hw on -WarmupSec 15 -SampleSec 20 -Shot | Out-Null
  } else {
    & (Join-Path $SpikeDir 'measure.ps1') -Browser $b -Run "$Prefix-$b-clean" -Tiles m1 -Layout single -Hw on -WarmupSec 30 -SampleSec $SampleSec -Shot @cap | Out-Null
    & (Join-Path $SpikeDir 'measure.ps1') -Browser $b -Run "$Prefix-$b-probe" -Tiles m1 -Layout single -Hw on -WarmupSec 20 -SampleSec 60 -UnmuteAtSec 12 -Probe @cap | Out-Null
  }
  "spike A $b done at {0:n0} s" -f $sw.Elapsed.TotalSeconds
}
& (Join-Path $SpikeDir 'pub.ps1') -Action stop -Set m
"spike A total {0:n0} s" -f $sw.Elapsed.TotalSeconds
