# Shared helpers for the SP2/SP3 Windows drivers. Dot-source it.
# SpWin: exact-match window lookup (never a pattern), WM_CLOSE to one handle, minimize, and a millisecond death watcher over pids that
# were opened (handle held) BEFORE the action, so a pid reuse cannot be mistaken for survival.
Add-Type -TypeDefinition @'
using System; using System.Collections.Generic; using System.Runtime.InteropServices; using System.Text; using System.Threading; using System.Diagnostics;
public class SpWatch {
  [DllImport("kernel32.dll")] static extern IntPtr OpenProcess(uint acc, bool inh, int pid);
  [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
  [DllImport("kernel32.dll")] static extern uint WaitForSingleObject(IntPtr h, uint ms);
  public int[] Pids; public IntPtr[] H; public long[] DeadAt;
  public static long NowMs() { return (long)(DateTime.UtcNow - new DateTime(1970,1,1,0,0,0,DateTimeKind.Utc)).TotalMilliseconds; }
  public SpWatch(int[] pids) {
    Pids = pids; H = new IntPtr[pids.Length]; DeadAt = new long[pids.Length];
    for (int i = 0; i < pids.Length; i++) { H[i] = OpenProcess(0x100000 | 0x1000, false, pids[i]); DeadAt[i] = (H[i] == IntPtr.Zero) ? 0 : -1; }
  }
  // Poll until every pid is dead or timeoutMs passes; DeadAt = first observation (epoch ms), -1 = still alive, 0 = could not open (already gone).
  public void Poll(int timeoutMs) {
    long end = NowMs() + timeoutMs;
    while (true) {
      bool all = true;
      for (int i = 0; i < Pids.Length; i++) {
        if (DeadAt[i] != -1) continue;
        if (WaitForSingleObject(H[i], 0) == 0) DeadAt[i] = NowMs(); else all = false;
      }
      if (all || NowMs() >= end) return;
      Thread.Sleep(5);
    }
  }
  public bool Alive(int i) { return DeadAt[i] == -1; }
}
public static class SpWin {
  delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc p, IntPtr l);
  [DllImport("user32.dll")] static extern int GetWindowThreadProcessId(IntPtr h, out int pid);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] static extern bool IsIconic(IntPtr h);
  [DllImport("user32.dll")] static extern bool PostMessage(IntPtr h, uint m, IntPtr w, IntPtr l);
  [DllImport("user32.dll")] static extern bool ShowWindowAsync(IntPtr h, int cmd);
  // Top-level windows whose class AND title both equal the arguments exactly (null or empty = any). Returns "hwnd|pid|class|title|visible|iconic".
  public static List<string> Find(string cls, string title) {
    List<string> r = new List<string>();
    EnumWindows(delegate(IntPtr h, IntPtr l) {
      StringBuilder c = new StringBuilder(256); GetClassName(h, c, 256);
      StringBuilder t = new StringBuilder(512); GetWindowText(h, t, 512);
      if ((String.IsNullOrEmpty(cls) || c.ToString() == cls) && (String.IsNullOrEmpty(title) || t.ToString() == title)) {
        int pid; GetWindowThreadProcessId(h, out pid);
        r.Add(h.ToInt64() + "|" + pid + "|" + c + "|" + t + "|" + IsWindowVisible(h) + "|" + IsIconic(h));
      }
      return true;
    }, IntPtr.Zero);
    return r;
  }
  public static bool Close(long hwnd) { return PostMessage(new IntPtr(hwnd), 0x0010, IntPtr.Zero, IntPtr.Zero); }
  public static void Minimize(long hwnd) { ShowWindowAsync(new IntPtr(hwnd), 7); } // SW_SHOWMINNOACTIVE
  public static bool Iconic(long hwnd) { return IsIconic(new IntPtr(hwnd)); }
}
'@

function Get-SpNow { [SpWatch]::NowMs() }
function Get-SpProc([int]$ProcessId) { Get-CimInstance Win32_Process -Filter "ProcessId=$ProcessId" }
# pid + all descendants (by CIM parent links), as objects with ProcessId, ParentProcessId, Name, CreationDate
function Get-SpTree([int]$Root) {
  $all = @(Get-CimInstance Win32_Process)
  $out = New-Object System.Collections.ArrayList
  $q = New-Object System.Collections.Queue; $q.Enqueue($Root)
  while ($q.Count) { $p = $q.Dequeue(); foreach ($c in $all | ? { $_.ParentProcessId -eq $p -and $_.ProcessId -ne $p }) { [void]$out.Add($c); $q.Enqueue([int]$c.ProcessId) } }
  $out
}
