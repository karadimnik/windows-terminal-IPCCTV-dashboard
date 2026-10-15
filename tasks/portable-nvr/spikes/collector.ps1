# Tiny local collector: serves the test page and appends every POSTed JSON line to <DataDir>\<run>.jsonl.
# GET /  -> page/index.html    GET /t0.json -> publisher start times    POST /stats?run=ID -> JSONL    GET /health
param([int]$Port = 18080, [Parameter(Mandatory)][string]$PageFile, [Parameter(Mandatory)][string]$DataDir, [Parameter(Mandatory)][string]$T0File)
$ErrorActionPreference = 'Continue'
$l = New-Object System.Net.HttpListener
$l.Prefixes.Add("http://127.0.0.1:$Port/")
$l.Start()
while ($l.IsListening) {
  $c = $null
  try {
    $c = $l.GetContext(); $req = $c.Request; $res = $c.Response
    $path = $req.Url.AbsolutePath
    if ($req.HttpMethod -eq 'POST' -and $path -eq '/stats') {
      $sr = New-Object IO.StreamReader($req.InputStream, [Text.Encoding]::UTF8); $body = $sr.ReadToEnd()
      $run = [Uri]::UnescapeDataString(($req.Url.Query -replace '^\?run=', '')) -replace '[^A-Za-z0-9_.-]', '_'
      [IO.File]::AppendAllText((Join-Path $DataDir "$run.jsonl"), $body + "`n")
      $res.StatusCode = 204
    } elseif ($path -eq '/t0.json') {
      $b = if (Test-Path $T0File) { [IO.File]::ReadAllBytes($T0File) } else { [Text.Encoding]::ASCII.GetBytes('{}') }
      $res.ContentType = 'application/json'; $res.OutputStream.Write($b, 0, $b.Length)
    } elseif ($path -eq '/health') {
      $b = [Text.Encoding]::ASCII.GetBytes('ok'); $res.OutputStream.Write($b, 0, $b.Length)
    } else {
      $page = [IO.File]::ReadAllBytes($PageFile); $res.ContentType = 'text/html; charset=utf-8'; $res.OutputStream.Write($page, 0, $page.Length)
    }
    $res.Close()
  } catch { try { if ($c) { $c.Response.Abort() } } catch {} }
}
