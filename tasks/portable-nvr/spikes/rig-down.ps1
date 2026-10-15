# Stops everything rig-up/measure started, by pid, after checking the pid still has the expected image name.
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
$exp = @('mediamtx','ffmpeg','powershell','mpv','chrome','msedge','firefox')
foreach($e in @((Get-State).pids)){
  $p = Get-Process -Id $e.pid -ErrorAction SilentlyContinue
  if($p -and ($exp -contains $p.ProcessName) -and ($p.StartTime.ToUniversalTime() -le [datetime]::Parse($e.started).ToUniversalTime().AddSeconds(3))){ Stop-Tree $e.pid; "stopped $($e.what) pid $($e.pid)" }
}
Remove-Item $StateFile -ErrorAction SilentlyContinue
