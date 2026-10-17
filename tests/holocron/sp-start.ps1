# Start a hidden process with stdout/stderr redirected to files; print its pid. The caller kills it by pid.
# usage: sp-start.ps1 -Exe <path> -ArgLine <string> -Out <file prefix> [-Env TZ=x]
param([string]$Exe, [string]$ArgLine, [string]$Out, [string]$WorkDir = 'C:\Windows\Temp\holocron-sp')
$p = Start-Process -FilePath $Exe -ArgumentList $ArgLine -WorkingDirectory $WorkDir -WindowStyle Hidden -PassThru -RedirectStandardOutput "$Out.out" -RedirectStandardError "$Out.err"
$p.Id
