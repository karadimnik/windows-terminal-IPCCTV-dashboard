<#
    spike-c.ps1 - Stage 0, Spike C: what do the real cameras' stream1 and stream2 look like?

    THE OPERATOR RUNS THIS, in his own session. The agent that wrote it did not run it and never saw a credential.

    BEFORE YOU RUN IT: the recorder, relay and dashboard must be STOPPED. Each camera allows only TWO simultaneous
    sessions; this script opens ONE session at a time (stream1, then stream2, then the next camera), and it needs both
    free slots if the system is stopped. Do not run it while the system is running.

    HOW:   powershell -NoProfile -ExecutionPolicy Bypass -File <this file>
           (nothing else is needed; it reads cameras.json and camera_creds.xml itself, through the product's own
            Get-CamList / Get-CamCredential / Get-CamRtspUrl from the ORIGINAL repository, read-only)

    PRINTS ONLY: camera name, stream, resolution, codec and profile, frame rate, bit rate, whether B-frames are present,
    and the audio codec. It never prints the URL, the credential or the environment, and ffprobe's own error text is
    discarded rather than shown.

    DOCUMENTED RESIDUAL: the credentialed URL is on ffprobe's COMMAND LINE for about a second to ten seconds per camera
    stream (ffmpeg has no out-of-band credential input), so a same-user process listing during that moment can read it.
    This is the same exposure the recorder's direct path has today. The URL is only ever a local variable here.

    Bit rate is MEASURED from 3 seconds of packets (RTSP streams usually report none); B-frames are detected from
    pts != dts on the video packets.
#>
$ErrorActionPreference = 'Stop'
# The original repository sits BESIDE this checkout: <workspace>\windows-terminal-IPCCTV-dashboard
# (this script is <workspace>\windows-terminal-IPCCTV-dashboard-portable\tasks\portable-nvr\spikes\spike-c.ps1)
$OrigRepo = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..\..\..\..\windows-terminal-IPCCTV-dashboard')).Path
$ffprobe = 'C:\ffmpeg\bin\ffprobe.exe'
$ProbeSeconds = 3
$TimeoutMs = 15000

. (Join-Path $OrigRepo 'cam.common.ps1')
$cameras = Get-CamList -Path (Join-Path $OrigRepo 'cameras.json')
$cred = Get-CamCredential -Path (Join-Path $OrigRepo 'camera_creds.xml')

function Invoke-Probe([string]$Url) {
    # One ffprobe, a hard timeout, stdout captured, stderr discarded. Returns the parsed JSON or $null.
    $argText = ('-v error -rtsp_transport tcp -timeout 5000000 -read_intervals %+{0} -show_entries stream=index,codec_type,codec_name,profile,width,height,avg_frame_rate,r_frame_rate,bit_rate,sample_rate,channels:packet=stream_index,codec_type,pts,dts,size -of json "{1}"' -f $ProbeSeconds, $Url)
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $ffprobe; $psi.Arguments = $argText
    $psi.UseShellExecute = $false; $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true; $psi.RedirectStandardError = $true
    $p = [System.Diagnostics.Process]::Start($psi)
    $outTask = $p.StandardOutput.ReadToEndAsync(); $null = $p.StandardError.ReadToEndAsync()
    if (-not $p.WaitForExit($TimeoutMs)) { try { $p.Kill() } catch {}; return $null }
    $p.WaitForExit()
    if ($p.ExitCode -ne 0) { return $null }
    try { return (ConvertFrom-Json -InputObject $outTask.Result) } catch { return $null }
}

function Get-Fps([string]$Frac) {
    if ($Frac -match '^(\d+)/(\d+)$' -and [int]$Matches[2] -ne 0) { return [math]::Round([double]$Matches[1] / [double]$Matches[2], 2) }
    return $null
}

foreach ($cam in $cameras) {
    foreach ($stream in 'stream1', 'stream2') {
        $probeCam = [pscustomobject]@{ Name = $cam.Name; IP = $cam.IP; Quality = $stream }
        $url = Get-CamRtspUrl -Camera $probeCam -Credential $cred
        $j = Invoke-Probe $url
        $url = $null
        $line = [ordered]@{ camera = [string]$cam.Name; stream = $stream }
        if (-not $j) {
            $line.result = 'no answer (stream missing, camera busy, or timeout)'
        } else {
            $v = @($j.streams | Where-Object { $_.codec_type -eq 'video' })[0]
            $a = @($j.streams | Where-Object { $_.codec_type -eq 'audio' })[0]
            $vp = @($j.packets | Where-Object { $_.codec_type -eq 'video' })
            $bytes = 0; foreach ($k in $vp) { $bytes += [int]$k.size }
            $bframes = $false; foreach ($k in $vp) { if ($null -ne $k.pts -and $null -ne $k.dts -and $k.pts -ne $k.dts) { $bframes = $true; break } }
            if ($v) {
                $line.resolution = '{0}x{1}' -f $v.width, $v.height
                $line.codec = '{0} {1}' -f $v.codec_name, $v.profile
                $line.fps = Get-Fps $v.avg_frame_rate; if (-not $line.fps -or $line.fps -eq 0) { $line.fps = Get-Fps $v.r_frame_rate }
                $line.video_kbps = if ($v.bit_rate) { [math]::Round([double]$v.bit_rate / 1000) } else { [math]::Round($bytes * 8 / $ProbeSeconds / 1000) }
                $line.bframes = if ($vp.Count -ge 5) { $bframes } else { 'unknown (too few packets)' }
            }
            $line.audio = if ($a) { '{0} {1} Hz {2} ch' -f $a.codec_name, $a.sample_rate, $a.channels } else { 'none' }
        }
        ($line.GetEnumerator() | ForEach-Object { '{0}={1}' -f $_.Key, $_.Value }) -join '  '
    }
}
