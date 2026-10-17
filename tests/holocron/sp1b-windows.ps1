# SP1b (Windows): credential delivery methods against a throwaway MediaMTX that requires fakeuser/fakepass (127.0.0.1:18554, path t).
# Prints per method: did ffmpeg record media (authenticated), is the password visible in its Win32_Process command line, and in its stderr.
param([string]$Ffmpeg = 'C:\ffmpeg\bin\ffmpeg.exe', [string]$Work = 'C:\Windows\Temp\holocron-sp\sp1b-win')
New-Item -ItemType Directory -Force -Path $Work | Out-Null
$u = 'rtsp://fakeuser:fakepass@127.0.0.1:18554/t'
[IO.File]::WriteAllText("$Work\url.txt", "$u`n")
[IO.File]::WriteAllText("$Work\list.txt", "file '$u'`n")
[IO.File]::WriteAllText("$Work\list2.ffconcat", "ffconcat version 1.0`nfile '$u'`noption rtsp_transport tcp`noption timeout 10000000`n")
$methods = [ordered]@{
  M0_url_in_argv  = @{ Args = "-nostdin -loglevel warning -rtsp_transport tcp -timeout 10000000 -i $u -c copy -t 4 -y $Work\M0.mkv"; Env = @{} }
  M1_slash_i_file = @{ Args = "-nostdin -loglevel warning -rtsp_transport tcp -/i $Work\url.txt -c copy -t 4 -y $Work\M1.mkv"; Env = @{} }
  M2_concat_file  = @{ Args = "-nostdin -loglevel warning -rtsp_transport tcp -timeout 10000000 -f concat -safe 0 -protocol_whitelist file,rtsp,tcp,rtp,udp -i $Work\list.txt -c copy -t 4 -y $Work\M2.mkv"; Env = @{} }
 M2b_concat_opts = @{ Args = "-nostdin -loglevel warning -f concat -safe 0 -protocol_whitelist file,rtsp,tcp,rtp,udp -i $Work\list2.ffconcat -c copy -t 4 -y $Work\M2b.mkv"; Env = @{} }
  M3_env_vars     = @{ Args = "-nostdin -loglevel warning -rtsp_transport tcp -i rtsp://127.0.0.1:18554/t -c copy -t 4 -y $Work\M3.mkv"; Env = @{ RTSP_USER = 'fakeuser'; RTSP_PASSWORD = 'fakepass' } }
  M5_no_credential = @{ Args = "-nostdin -loglevel warning -rtsp_transport tcp -i rtsp://127.0.0.1:18554/t -c copy -t 4 -y $Work\M5.mkv"; Env = @{} }
}
foreach ($k in $methods.Keys) {
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $Ffmpeg; $psi.Arguments = $methods[$k].Args; $psi.UseShellExecute = $false; $psi.CreateNoWindow = $true
  $psi.RedirectStandardError = $true; $psi.RedirectStandardOutput = $true
  foreach ($e in $methods[$k].Env.Keys) { $psi.EnvironmentVariables[$e] = $methods[$k].Env[$e] }
  $p = [System.Diagnostics.Process]::Start($psi)
  $err = $p.StandardError.ReadToEndAsync(); $out = $p.StandardOutput.ReadToEndAsync()
  Start-Sleep -Milliseconds 1500
  $cl = [string](Get-CimInstance Win32_Process -Filter "ProcessId=$($p.Id)").CommandLine
  $p.WaitForExit()
  $file = "$Work\" + ($k -split '_')[0] + '.mkv'
  $size = if (Test-Path $file) { (Get-Item $file).Length } else { 0 }
  $dur = if ($size -gt 0) { (& C:\ffmpeg\bin\ffprobe.exe -v error -show_entries format=duration -of csv=p=0 $file) } else { '-' }
  '{0} | recorded={1} bytes dur={2} | exit={3} | fakepass_in_cmdline={4} | fakepass_in_stderr={5}' -f $k, $size, $dur, $p.ExitCode, ($cl -match 'fakepass'), ($err.Result -match 'fakepass')
  '   cmdline: ' + $cl.Substring(0, [Math]::Min(170, $cl.Length))
  '   stderr: ' + (($err.Result -replace "\r?\n", '|') + ' ').Substring(0, [Math]::Min(220, $err.Result.Length + 1))
}
