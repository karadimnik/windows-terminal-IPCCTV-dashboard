# SP6 PowerShell reader. Modes: gc (Get-Content -Raw), openread ([IO.File]::OpenRead = FileShare.Read),
# fs-rw (FileStream, FileShare.ReadWrite), fs-rwd (FileStream, FileShare.ReadWrite|Delete). -HoldMs keeps the stream open per read.
param([string]$Path, [string]$Mode = 'gc', [int]$Seconds = 10, [int]$HoldMs = 0)
$ok = 0; $bad = 0; $total = 0; $errs = @{}
$end = (Get-Date).AddSeconds($Seconds)
while ((Get-Date) -lt $end) {
  $total++
  try {
    switch ($Mode) {
      'gc' { $t = Get-Content -LiteralPath $Path -Raw -ErrorAction Stop; if ($HoldMs) { Start-Sleep -Milliseconds $HoldMs } }
      default {
        $fs = switch ($Mode) {
          'openread' { [IO.File]::OpenRead($Path) }
          'fs-rw'    { New-Object IO.FileStream($Path, 'Open', 'Read', 'ReadWrite') }
          'fs-rwd'   { New-Object IO.FileStream($Path, 'Open', 'Read', ([IO.FileShare]'ReadWrite, Delete')) }
        }
        try { $sr = New-Object IO.StreamReader($fs); $t = $sr.ReadToEnd(); if ($HoldMs) { Start-Sleep -Milliseconds $HoldMs } } finally { $fs.Dispose() }
      }
    }
    $null = $t | ConvertFrom-Json -ErrorAction Stop; $ok++
  } catch {
    $m = $_.Exception.GetType().Name + ': ' + ($_.Exception.Message -replace '\s+', ' ')
    if ($m.Length -gt 90) { $m = $m.Substring(0, 90) }
    if ($m -match 'ConvertFrom|JSON|json|Conversion|Invalid') { $bad++ } else { $errs[$m] = 1 + [int]$errs[$m] }
  }
}
'PS-READER mode={0} hold={1}ms reads={2} ok={3} partial_or_invalid={4} errors=[{5}]' -f $Mode, $HoldMs, $total, $ok, $bad, (($errs.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ' | ')
