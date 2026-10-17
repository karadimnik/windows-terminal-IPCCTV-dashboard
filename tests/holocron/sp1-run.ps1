# SP1 Windows driver: the same vector as sp1-run.sh, each case a hidden process with its own TZ (or none).
# usage: powershell -NoProfile -File sp1-run.ps1 -Ffmpeg C:\ffmpeg\bin\ffmpeg.exe -Out C:\Windows\Temp\holocron-sp\sp1\win2 -Cases sys,utc
param([string]$Ffmpeg, [string]$Out, [string[]]$Cases)
$tz = @{ sys = $null; utc = 'UTC'; bucI = 'Europe/Bucharest'; kolI = 'Asia/Kolkata'; stjI = 'America/St_Johns';
         bucP = 'EET-2EEST'; kolP = 'IST-5:30'; stjP = 'NST3:30NDT' }
$Cases = @($Cases | ForEach-Object { $_ -split "," })
New-Item -ItemType Directory -Force -Path $Out | Out-Null
$procs = @()
foreach ($key in $Cases) { foreach ($seg in 10, 7) {
  $case = "${key}_s$seg"; $dir = Join-Path $Out $case; New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $t = if ($seg -eq 10) { 45 } else { 35 }
  $pattern = Join-Path $dir 'Test_%Y%m%d_%H%M%S%z.mkv'
  $a = @('-hide_banner','-loglevel','warning','-nostdin',
    '-re','-f','lavfi','-i','testsrc2=size=320x180:rate=10','-re','-f','lavfi','-i','sine=frequency=440:sample_rate=8000',
    '-map','0:v:0','-map','1:a:0?','-c:v','libx264','-preset','ultrafast','-g','10','-c:a','pcm_alaw','-ar','8000','-ac','1','-t',"$t",
    '-f','segment','-segment_time',"$seg",'-segment_atclocktime','1','-reset_timestamps','1','-strftime','1','-segment_format','matroska',$pattern)
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $Ffmpeg; $psi.Arguments = ($a | ForEach-Object { if ($_ -match '\s') { '"' + $_ + '"' } else { $_ } }) -join ' '
  $psi.UseShellExecute = $false; $psi.CreateNoWindow = $true; $psi.RedirectStandardError = $true; $psi.RedirectStandardOutput = $true
  if ($psi.EnvironmentVariables.ContainsKey('TZ')) { $psi.EnvironmentVariables.Remove('TZ') }
  if ($tz[$key]) { $psi.EnvironmentVariables['TZ'] = $tz[$key] }
  $p = [System.Diagnostics.Process]::Start($psi)
  $procs += [pscustomobject]@{ Case = $case; P = $p; Err = $p.StandardError.ReadToEndAsync(); Outp = $p.StandardOutput.ReadToEndAsync(); Args = $psi.Arguments }
} }
foreach ($x in $procs) {
  $x.P.WaitForExit()
  [IO.File]::WriteAllText((Join-Path $Out "$($x.Case).stderr"), $x.Err.Result)
  [IO.File]::WriteAllText((Join-Path $Out "$($x.Case).stdout"), $x.Outp.Result)
  [IO.File]::WriteAllText((Join-Path $Out "$($x.Case).exit"), "$($x.P.ExitCode)")
}
[IO.File]::WriteAllText((Join-Path $Out 'args.txt'), $procs[0].Args)
'done'
