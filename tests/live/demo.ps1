<#
.SYNOPSIS
    Try the Live screen in your own browser without touching the real system.

.DESCRIPTION
    Starts a throwaway copy of the whole live-view rig in a temp folder: the
    real supervisor with LiveViewEnabled, a live-view MediaMTX, six SYNTHETIC
    cameras (colour bars, fake credentials - camera_creds.xml is never read), and
    a camui built to a temp path, all on non-default ports (camui 18789, camera
    18554, relay 18564, live 18889/18189). Prints the URL, waits for Enter or
    -Minutes, then stops exactly the processes it started and deletes its temp
    folders. It never starts, stops or reads the real recorder, relay, dashboard
    or camui, and never uses their ports (8787, 8554, 9997, 8889, 8189): if one
    is held it only says so. It refuses to start when one of ITS OWN ports is
    already in use.

    Run it from a LOCAL directory in Windows PowerShell 5.1:
        cd C:\Windows\Temp
        powershell -NoProfile -ExecutionPolicy Bypass -File <repo>\tests\live\demo.ps1 -Minutes 15
    camui is built with Go through WSL (~/.local/go/bin) unless -CamuiExe, or
    CAMUI_TEST_EXE, names an exe built from this tree.
#>
[CmdletBinding()]
param(
    # How long to keep it up when Enter is not pressed.
    [ValidateRange(1, 240)][int]$Minutes = 15,
    # A camui.exe built from this tree; empty builds one into a temp folder.
    [string]$CamuiExe = '',
    [ValidateRange(1024, 65535)][int]$Port = 18789
)

$ErrorActionPreference = 'Stop'
. (Join-Path -Path (Split-Path -Parent $PSScriptRoot) -ChildPath 'cam.testlib.ps1')

$rig = $null; $ui = $null
$binDir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\livedemo-bin'
$uiCopy = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\camui-livedemo'
$savedExe = $env:CAMUI_TEST_EXE

try {
    # Its own ports must be free; the real system's are only reported.
    $busy = @(@($Port, 18554, 18557, 18564, 18567, 18889) | Where-Object { -not (Test-CamTestPortFree -Port $_) })
    if (-not (Test-CamTestUdpPortFree -Port 18189)) { $busy += 'udp 18189' }
    if ($busy.Count) { throw ("port(s) {0} are already in use (another demo, or a leftover of a test); nothing was started." -f ($busy -join ', ')) }
    $real = @(@(8787, 8554, 9997, 8889) | Where-Object { -not (Test-CamTestPortFree -Port $_) })
    if ($real.Count) { Write-Host ("note: port(s) {0} are held by something else (the real system, if it is running). The demo does not use or touch them." -f ($real -join ', ')) -ForegroundColor DarkYellow }
    foreach ($need in @('C:\mediamtx\mediamtx.exe', (Get-CamConfigDefault).FfmpegPath)) { if (-not (Test-Path -LiteralPath $need)) { throw "$need is needed (the demo copies MediaMTX and uses ffmpeg for the synthetic cameras)." } }

    $exe = $CamuiExe
    if (-not $exe) { $exe = $env:CAMUI_TEST_EXE }
    if (-not $exe) {
        Write-Host 'building camui (Go through WSL) ...'
        [void](New-Item -ItemType Directory -Path $binDir -Force)
        $exe = Join-Path -Path $binDir -ChildPath 'camui-demo.exe'
        $uiLinux = ((& wsl.exe -e wslpath -a -u (Join-Path (Get-CamTestRepo) 'ui')) | Select-Object -First 1)
        $outLinux = ((& wsl.exe -e wslpath -a -u $exe) | Select-Object -First 1)
        # A tiny script file: quoting a go build line through wsl.exe and bash -c does not survive.
        $script = Join-Path -Path $binDir -ChildPath 'build.sh'
        [System.IO.File]::WriteAllText($script, "#!/bin/bash`nexport PATH=`"`$PATH:`$HOME/.local/go/bin`"`ncd `"`$1`" || exit 1`nCGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags `"-s -w -X main.version=demo`" -o `"`$2`" .`n", (New-Object System.Text.UTF8Encoding($false)))
        $scriptLinux = ((& wsl.exe -e wslpath -a -u $script) | Select-Object -First 1)
        Push-Location -LiteralPath $env:TEMP
        try { & wsl.exe -e bash $scriptLinux $uiLinux $outLinux } finally { Pop-Location }
        if (-not (Test-Path -LiteralPath $exe)) { throw 'could not build camui; pass -CamuiExe (a camui.exe built from this tree).' }
    }
    $env:CAMUI_TEST_EXE = $exe

    Write-Host 'starting the rig: supervisor, live-view MediaMTX and six synthetic cameras (about 30 s) ...'
    $cameras = 'Front Entrance', 'Rear Balcony', 'Field Front', 'Car Garage', 'Workshop Entrance', 'Workshop' |
        ForEach-Object { [pscustomobject]@{ Name = $_; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = ($_ -eq 'Front Entrance') } }
    $rig = Start-CamTestLiveRig -Name 'livedemo' -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds 10
    $ui = Start-CamTestLiveCamui -Rig $rig -Port $Port -Name 'camui-livedemo'
    if (-not $ui.Started) { throw ('camui did not start: ' + $ui.Reason) }

    Write-Host ''
    Write-Host ("  Live screen:  http://127.0.0.1:{0}/#live" -f $Port) -ForegroundColor Green
    Write-Host ("  One camera:   http://127.0.0.1:{0}/#live/Front%20Entrance" -f $Port)
    Write-Host '  Token:        none (loopback only)'
    Write-Host '  The cameras are synthetic colour bars; nothing real is read or started.'
    Write-Host ("  Press Enter to stop everything, or it stops by itself in {0} minute(s)." -f $Minutes)
    Write-Host ''

    $interactive = $true
    try { if ([Console]::IsInputRedirected) { $interactive = $false } else { [void][Console]::KeyAvailable } } catch { $interactive = $false }
    $deadline = [DateTime]::Now.AddMinutes($Minutes)
    while ([DateTime]::Now -lt $deadline) {
        if ($rig.Supervisor.HasExited -or $ui.Process.HasExited) { Write-Host 'a demo process exited; stopping.' -ForegroundColor Yellow; break }
        if ($interactive -and [Console]::KeyAvailable) { if ([Console]::ReadKey($true).Key -eq 'Enter') { break } }
        Start-Sleep -Milliseconds 300
    }
}
finally {
    Write-Host 'stopping what this demo started ...'
    if ($ui) { [void](Stop-CamTestUi -Ui $ui) }
    if ($rig) { foreach ($line in @(Stop-CamTestLiveRig -Rig $rig)) { Write-Host ('  ' + $line) } }
    foreach ($path in @($(if ($rig) { $rig.Dir }), $uiCopy, $binDir)) {
        if ($path -and (Test-Path -LiteralPath $path)) { try { Remove-CamTestTree -Path $path } catch { Write-Host "  could not remove $path" -ForegroundColor Yellow } }
    }
    $env:CAMUI_TEST_EXE = $savedExe
    Write-Host 'done: everything the demo started is stopped and its temp folders are removed.'
}
