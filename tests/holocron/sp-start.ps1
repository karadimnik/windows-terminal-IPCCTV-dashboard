# Start a hidden process with stdout/stderr redirected to files; write its pid to <Out>.pid. The caller kills it by pid.
# Call it with the host's own output redirected to a file and in the background (the child inherits the host's pipe otherwise).
param([string]$Exe, [string]$ArgLine, [string]$Out, [string]$WorkDir = 'C:\Windows\Temp\holocron-sp')
$p = Start-Process -FilePath $Exe -ArgumentList $ArgLine -WorkingDirectory $WorkDir -WindowStyle Hidden -PassThru -RedirectStandardOutput "$Out.out" -RedirectStandardError "$Out.err"
[IO.File]::WriteAllText("$Out.pid", [string]$p.Id)
