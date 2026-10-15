# Starts the sandbox rig: MediaMTX (WebRTC on non-default loopback ports), the collector, and -c copy publishers.
# Everything started is recorded by pid in $env:TEMP\stage0\state.json so rig-down.ps1 can stop it by pid.
# Publishers are managed separately by pub.ps1.
param()
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
foreach($p in $Mtx.Whep,$Mtx.Rtsp,$Mtx.Api,$CollectorPort){ if(@(Get-NetTCPConnection -State Listen -LocalPort $p -ErrorAction SilentlyContinue).Count){ throw "port $p is already in use" } }
if(@(Get-NetUDPEndpoint -LocalPort $Mtx.Udp -ErrorAction SilentlyContinue).Count){ throw "udp $($Mtx.Udp) in use" }
Remove-Item $StateFile -ErrorAction SilentlyContinue
$cfgDir = Join-Path $Rt 'mtx'; New-Item -ItemType Directory -Force $cfgDir | Out-Null
$yml = @"
logLevel: info
logDestinations: [file]
logFile: $(Join-Path $cfgDir 'mediamtx.log')
readTimeout: 10s
writeTimeout: 10s
writeQueueSize: 2048
api: yes
apiAddress: 127.0.0.1:$($Mtx.Api)
metrics: no
pprof: no
playback: no
rtsp: yes
rtspTransports: [tcp]
rtspAddress: 127.0.0.1:$($Mtx.Rtsp)
rtmp: no
hls: no
srt: no
moq: no
webrtc: yes
webrtcAddress: 127.0.0.1:$($Mtx.Whep)
webrtcEncryption: no
webrtcAllowOrigins: ['*']
webrtcLocalUDPAddress: 127.0.0.1:$($Mtx.Udp)
webrtcLocalTCPAddress: ''
webrtcIPsFromInterfaces: yes
webrtcIPsFromInterfacesList: ['Loopback Pseudo-Interface 1']
webrtcAdditionalHosts: []
pathDefaults:
  rtspTransport: tcp
paths:
  all_others:
"@
$yml | Set-Content (Join-Path $cfgDir 'mediamtx.yml') -Encoding ascii
$mtxProc = Start-Hidden $Mtx.Exe ('"{0}"' -f (Join-Path $cfgDir 'mediamtx.yml')) $cfgDir 'mediamtx'
Start-Sleep -Milliseconds 1500
if($mtxProc.HasExited){ throw "mediamtx exited: $(Get-Content (Join-Path $cfgDir 'mediamtx.log') -Raw)" }
$ok = $false
for($i=0;$i -lt 30 -and -not $ok;$i++){ try { $null = Invoke-WebRequest "http://127.0.0.1:$($Mtx.Api)/v3/paths/list" -UseBasicParsing -TimeoutSec 2; $ok=$true } catch { Start-Sleep -Milliseconds 300 } }
if(-not $ok){ throw 'mediamtx API did not come up' }
"mediamtx pid $($mtxProc.Id)"

'{}' | Set-Content (Join-Path $Rt 't0.json') -Encoding ascii
$col = Start-Hidden 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' ('-NoProfile -ExecutionPolicy Bypass -File "{0}" -Port {1} -PageFile "{2}" -DataDir "{3}" -T0File "{4}"' -f (Join-Path $SpikeDir 'collector.ps1'),$CollectorPort,(Join-Path $SpikeDir 'page\index.html'),$DataDir,(Join-Path $Rt 't0.json')) $Rt 'collector'
for($i=0;$i -lt 30;$i++){ try { $null = Invoke-WebRequest "http://127.0.0.1:$CollectorPort/health" -UseBasicParsing -TimeoutSec 2; break } catch { Start-Sleep -Milliseconds 300 } }
"collector pid $($col.Id) on 127.0.0.1:$CollectorPort"
