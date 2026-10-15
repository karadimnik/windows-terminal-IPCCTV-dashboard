# Shared settings for the Stage 0 spikes. Dot-source this file. PowerShell 5.1.
$ErrorActionPreference = 'Stop'
$script:SpikeDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$script:Rt       = Join-Path $env:TEMP 'stage0'                 # LOCAL runtime folder (never a UNC path)
$script:ClipDir  = Join-Path $Rt 'clips'
$script:DataDir  = Join-Path $Rt 'data'
$script:StateFile = Join-Path $Rt 'state.json'
$script:Mtx = @{ Exe='C:\mediamtx\mediamtx.exe'; Whep=18889; Udp=18189; Rtsp=18554; Api=18997 }
$script:CollectorPort = 18080
$script:Ffmpeg = 'C:\ffmpeg\bin\ffmpeg.exe'
$script:Ffprobe = 'C:\ffmpeg\bin\ffprobe.exe'
# Clip sets -> publish path prefix: m=2560x1440 main, h=1280x720, s=640x360 sub stand-in; paths m1..m6, h1..h6, s1..s6
$script:ClipSets = @{ m=@{W=2560;H=1440}; h=@{W=1280;H=720}; s=@{W=640;H=360} }
$script:Fps = 20
$script:ClipSeconds = 30
New-Item -ItemType Directory -Force $Rt,$ClipDir,$DataDir | Out-Null

function Get-State { if(Test-Path $StateFile){ Get-Content $StateFile -Raw | ConvertFrom-Json } else { [pscustomobject]@{ pids=@() } } }
function Add-StatePid($procId, $what) {
  $s = Get-State; $list = @(); foreach($p in $s.pids){ $list += $p }
  $list += [pscustomobject]@{ pid=[int]$procId; what=$what; started=(Get-Date).ToUniversalTime().ToString('o') }
  ([pscustomobject]@{ pids=$list } | ConvertTo-Json -Depth 4) | Set-Content $StateFile -Encoding ascii
}
# Kill a process tree by pid only (never by image name).
function Stop-Tree($rootPid) {
  $all = Get-CimInstance Win32_Process | Select-Object ProcessId, ParentProcessId
  $todo = New-Object System.Collections.Generic.List[int]; $todo.Add([int]$rootPid)
  for($i=0; $i -lt $todo.Count; $i++){ foreach($c in $all){ if($c.ParentProcessId -eq $todo[$i] -and -not $todo.Contains([int]$c.ProcessId)){ $todo.Add([int]$c.ProcessId) } } }
  foreach($id in $todo){ Stop-Process -Id $id -Force -ErrorAction SilentlyContinue }
}
function Get-Descendants($rootPid) {
  $all = Get-CimInstance Win32_Process | Select-Object ProcessId, ParentProcessId, CommandLine
  $set = New-Object System.Collections.Generic.List[int]; $set.Add([int]$rootPid)
  for($i=0; $i -lt $set.Count; $i++){ foreach($c in $all){ if($c.ParentProcessId -eq $set[$i] -and -not $set.Contains([int]$c.ProcessId)){ $set.Add([int]$c.ProcessId) } } }
  ,$set
}
function Start-Hidden($exe, $argLine, $wd, $tag) {
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $exe; $psi.Arguments = $argLine; $psi.WorkingDirectory = $wd
  $psi.UseShellExecute = $true; $psi.WindowStyle = 'Hidden'   # no handle inheritance, so a calling shell never waits on the child
  $p = [System.Diagnostics.Process]::Start($psi); Add-StatePid $p.Id $tag; $p
}

# Start a normal VISIBLE GUI process (browsers, mpv). ShellExecute, so no handle is inherited by the calling shell.
function Start-Visible($exe, $argLine, $wd, $tag) {
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $exe; $psi.Arguments = $argLine; $psi.WorkingDirectory = $wd; $psi.UseShellExecute = $true
  $p = [System.Diagnostics.Process]::Start($psi); Add-StatePid $p.Id $tag; $p
}
# Pids whose command line carries $Marker, plus every descendant of $RootPid. Marker is the unique profile path.
function Get-TestPids($rootPid, $marker) {
  $all = @(Get-CimInstance Win32_Process | Select-Object ProcessId, ParentProcessId, CommandLine)
  $set = New-Object System.Collections.Generic.HashSet[int]
  if($rootPid){ [void]$set.Add([int]$rootPid) }
  if($marker){ foreach($c in $all){ if($c.CommandLine -and $c.CommandLine.Contains($marker)){ [void]$set.Add([int]$c.ProcessId) } } }
  do { $n = $set.Count; foreach($c in $all){ if($set.Contains([int]$c.ParentProcessId)){ [void]$set.Add([int]$c.ProcessId) } } } while($set.Count -ne $n)
  ,@($set | Sort-Object)
}
Add-Type -ErrorAction SilentlyContinue -Name Win -Namespace S0 -MemberDefinition @'
[DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
public delegate bool EnumProc(IntPtr h, IntPtr l);
[DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
[DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
[DllImport("user32.dll")] public static extern int GetWindowTextLength(IntPtr h);
[DllImport("user32.dll", SetLastError=true)] public static extern bool SetWindowPos(IntPtr h, IntPtr a, int x, int y, int cx, int cy, uint f);
[DllImport("kernel32.dll")] public static extern bool GetSystemTimes(out long i, out long k, out long u);
[DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
[DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int c);
[DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
[StructLayout(LayoutKind.Sequential)] public struct RECT { public int L,T,R,B; }
public static System.Collections.Generic.List<IntPtr> Top(System.Collections.Generic.HashSet<uint> pids){
  var l = new System.Collections.Generic.List<IntPtr>();
  EnumWindows((h,x)=>{ uint p; GetWindowThreadProcessId(h,out p); if(pids.Contains(p) && IsWindowVisible(h) && GetWindowTextLength(h)>0) l.Add(h); return true; }, IntPtr.Zero);
  return l;
}
'@
function Get-TestWindows($pids) { $hs = New-Object 'System.Collections.Generic.HashSet[uint32]'; foreach($p in $pids){ [void]$hs.Add([uint32]$p) }; @([S0.Win]::Top($hs)) }
# TOPMOST so the Operator's own windows cannot occlude the test window (an occluded window would not decode).
function Set-WindowRect($h, $x, $y, $w, $hgt) { [S0.Win]::ShowWindow($h, 9) | Out-Null; [S0.Win]::SetWindowPos($h, [IntPtr](-1), $x, $y, $w, $hgt, 0x0040) | Out-Null; [S0.Win]::SetForegroundWindow($h) | Out-Null }
function Save-Screenshot($file) {
  Add-Type -AssemblyName System.Windows.Forms, System.Drawing
  $b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
  $bmp = New-Object System.Drawing.Bitmap $b.Width, $b.Height
  $g = [System.Drawing.Graphics]::FromImage($bmp); $g.CopyFromScreen($b.Location, [System.Drawing.Point]::Empty, $b.Size)
  $bmp.Save($file, [System.Drawing.Imaging.ImageFormat]::Png); $g.Dispose(); $bmp.Dispose()
}

# A sleeping display makes every browser report the page hidden and stops rendering (found on this PC: screenshots were
# a stale frame). Hold the display awake for the life of THIS process (no system setting changes) and nudge input once.
Add-Type -ErrorAction SilentlyContinue -Name Pw -Namespace S0 -MemberDefinition @'
[DllImport("kernel32.dll")] public static extern uint SetThreadExecutionState(uint f);
[DllImport("user32.dll")] public static extern void mouse_event(uint f, int dx, int dy, uint d, UIntPtr x);
'@
function Keep-Awake { [void][S0.Pw]::SetThreadExecutionState([uint32]2147483651); [S0.Pw]::mouse_event(1,0,0,0,[UIntPtr]::Zero) }
