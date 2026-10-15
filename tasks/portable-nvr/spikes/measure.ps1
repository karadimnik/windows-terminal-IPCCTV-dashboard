# One measurement cell: launch a test browser on the page with the given tiles, warm up, sample from OUTSIDE, kill the tree by pid.
param(
  [Parameter(Mandatory)][ValidateSet('chrome','edge','firefox')][string]$Browser,
  [Parameter(Mandatory)][string]$Run,
  [Parameter(Mandatory)][string]$Tiles,                 # e.g. m1,m2,m3,m4,m5,m6
  [ValidateSet('grid','focus','single')][string]$Layout = 'grid',
  [ValidateSet('on','off')][string]$Hw = 'on',
  [int]$WarmupSec = 30, [int]$SampleSec = 180,
  [int]$HideAtSec = 0,                                  # minimize the window this many seconds into the sample window
  [int]$UnmuteAtSec = 0,                                # page unmutes tile 0 this many seconds after page start (Spike A audio)
  [switch]$Probe,                                       # latency barcode probe (adds canvas readback; Spike A only)
  [switch]$Cap,                                         # hold a fake audio capture (Chromium then exposes decoderImplementation in getStats)
  [switch]$Shot                                         # screenshot of the primary screen at the middle of the window
)
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
. (Join-Path $SpikeDir 'sampler.ps1')
$exe = @{ chrome='C:\Program Files\Google\Chrome\Application\chrome.exe'; edge='C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe'; firefox='C:\Program Files\Mozilla Firefox\firefox.exe' }[$Browser]
Keep-Awake
$prof = Join-Path $Rt ("prof-{0}-{1}" -f $Run, [guid]::NewGuid().ToString('N').Substring(0,6))
New-Item -ItemType Directory -Force $prof | Out-Null
$q = "run=$Run&tiles=$Tiles&layout=$Layout"; if($Cap){ $q += '&cap=1' }; if($Probe){ $q += '&probe=1' }; if($UnmuteAtSec -gt 0){ $q += "&unmute=$UnmuteAtSec" }
$url = "http://127.0.0.1:$CollectorPort/?$q"
if($Browser -eq 'firefox'){
  $hwPref = if($Hw -eq 'on'){ 'true' } else { 'false' }
  @("user_pref('media.hardware-video-decoding.enabled', $hwPref);", "user_pref('media.peerconnection.ice.loopback', true);", "user_pref('media.peerconnection.ice.default_address_only', false);", "user_pref('media.peerconnection.ice.no_host', false);", "user_pref('media.peerconnection.ice.link_local', true);", "user_pref('media.peerconnection.ice.obfuscate_host_addresses', false);", "user_pref('media.navigator.permission.disabled', true);", "user_pref('media.navigator.streams.fake', true);",
    "user_pref('media.autoplay.default', 0);", "user_pref('media.autoplay.blocking_policy', 0);",
    "user_pref('browser.shell.checkDefaultBrowser', false);", "user_pref('browser.aboutwelcome.enabled', false);",
    "user_pref('datareporting.policy.dataSubmissionEnabled', false);", "user_pref('toolkit.telemetry.reportingpolicy.firstRun', false);",
    "user_pref('browser.startup.homepage_override.mstone', 'ignore');", "user_pref('browser.tabs.warnOnClose', false);",
    "user_pref('app.update.auto', false);", "user_pref('app.update.enabled', false);", "user_pref('network.captive-portal-service.enabled', false);") |
    Set-Content (Join-Path $prof 'user.js') -Encoding ascii
  $argLine = "-no-remote -profile `"$prof`" -new-window `"$url`""
} else {
  $argLine = "--user-data-dir=`"$prof`" --new-window --no-first-run --no-default-browser-check --window-position=0,0 --window-size=1920,1080 --autoplay-policy=no-user-gesture-required"; if($Cap){ $argLine += ' --use-fake-ui-for-media-stream --use-fake-device-for-media-stream' }
  if($Hw -eq 'off'){ $argLine += ' --disable-accelerated-video-decode' }
  $argLine += " `"$url`""
}
$meta = [ordered]@{ run=$Run; browser=$Browser; hw=$Hw; tiles=$Tiles; layout=$Layout; probe=[bool]$Probe; warmup_s=$WarmupSec; sample_s=$SampleSec; hide_at_s=$HideAtSec; profile=$prof }
$proc = Start-Visible $exe $argLine $Rt "test-$Browser-$Run"
$meta.root_pid = $proc.Id; $meta.launch_ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
try {
  Start-Sleep -Seconds 4
  $win = $null
  for($i=0;$i -lt 20 -and -not $win;$i++){ $w = Get-TestWindows (Get-TestPids $proc.Id $prof); if($w.Count){ $win = $w[0] } else { Start-Sleep -Milliseconds 500 } }
  if($win){ Set-WindowRect $win 0 0 1920 1080; $rc = New-Object S0.Win+RECT; [S0.Win]::GetWindowRect($win,[ref]$rc) | Out-Null; $meta.window = "$($rc.L),$($rc.T),$($rc.R),$($rc.B)" } else { $meta.window = 'NOT FOUND' }
  $elapsed = ([DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() - $meta.launch_ms)/1000
  Start-Sleep -Seconds ([math]::Max(0, $WarmupSec - $elapsed))
  $meta.sample_start_ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  $getPids = { Get-TestPids $proc.Id $prof }
  $at = $null
  if($HideAtSec -gt 0){ $at = @{ Seconds = $HideAtSec; Action = { $ws = Get-TestWindows (Get-TestPids $proc.Id $prof); foreach($h in $ws){ [S0.Win]::ShowWindow($h, 6) | Out-Null }; $script:hideMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() } } }
  if($Shot){ $at = @{ Seconds = [int]($SampleSec/2); Action = { Save-Screenshot (Join-Path $DataDir "$Run.png") } } }
  Invoke-Sampling -GetPids $getPids -Seconds $SampleSec -OutFile (Join-Path $DataDir "$Run.samples.jsonl") -AtSecond $at
  $meta.sample_end_ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  if($script:hideMs){ $meta.hide_ms = $script:hideMs }
  $meta.pids_end = @(Get-TestPids $proc.Id $prof).Count
} finally {
  foreach($id in @(Get-TestPids $proc.Id $prof)){ Stop-Process -Id $id -Force -ErrorAction SilentlyContinue }
  Start-Sleep -Milliseconds 1500
  for($i=0;$i -lt 5;$i++){ Remove-Item $prof -Recurse -Force -ErrorAction SilentlyContinue; if(-not (Test-Path $prof)){ break }; Start-Sleep 1 }
  $meta.profile_removed = -not (Test-Path $prof)
  ($meta | ConvertTo-Json -Compress) | Set-Content (Join-Path $DataDir "$Run.meta.json") -Encoding ascii
}
"cell $Run done: $($meta | ConvertTo-Json -Compress)"
