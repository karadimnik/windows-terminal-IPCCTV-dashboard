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

    -Lan (or -LanAddress <ip>) makes the live instance listen on 0.0.0.0 and advertise this PC's private IPv4 for the
    media as well as loopback. The address is auto-detected: physical adapters only (WSL, Hyper-V, VirtualBox, VMware,
    VPN adapters are skipped), preferring the one with the default route; if that is ambiguous, or there is none, the
    demo stops and asks for -LanAddress. In this mode the live instance runs from C:\mediamtx\mediamtx.exe (read only)
    rather than a temp copy, so an existing Windows Firewall rule for that program applies and no new prompt for a
    new path is needed; the demo itself never creates a firewall rule. camui (the page) stays on loopback.

    -Tls (implies -Lan) also serves the whole page over HTTPS on the LAN address, for a phone: camui gets an HTTPS listener
    (-TlsPort, default 18443) behind a login, view-only, with a throwaway certificate authority and a throwaway auth folder in
    the demo's own temp folder (NOT %LOCALAPPDATA%\IPCCTV\tls or ...\camui\auth). It creates the user "demo" with a fresh random
    strong password, handed to camui.exe -set-password on standard input (never on a command line), and prints the user name
    and the password once to this console. It prints the https URL, the CA file and SHA-256 fingerprint, short phone steps
    and the exact ELEVATED firewall commands (Private profile, LocalSubnet); it never applies them. To keep those rules stable
    camui runs from a FIXED path, %LOCALAPPDATA%\IPCCTV\demo\camui.exe (-ProgramPath), which is left in place afterwards.
    A temp-path program has no allow rule, and Windows would prompt (a dismissed prompt can create a block rule), so the demo
    REFUSES to bind beyond loopback, prints the commands and stops, unless a read-only look at the firewall shows an enabled
    inbound Allow rule for that program (TCP) and for C:\mediamtx\mediamtx.exe (UDP) on the Private profile, or you pass
    -FirewallReady after running the commands yourself. At the end it deletes the demo TLS folder and every temp folder.

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
    [ValidateRange(1024, 65535)][int]$Port = 18789,
    # Also advertise this PC's private LAN address for the media (browsers that ignore loopback, such as Firefox, need it).
    [switch]$Lan,
    # With -Lan: the address to use, when this PC has several candidates. Must be a private IPv4 of an up adapter here.
    [string]$LanAddress = '',
    # Also serve the page over HTTPS on the LAN address (implies -Lan). See above.
    [switch]$Tls,
    [ValidateRange(1024, 65535)][int]$TlsPort = 18443,
    # With -Tls: the program camui runs from. Default %LOCALAPPDATA%\IPCCTV\demo\camui.exe, so a printed firewall rule is stable.
    [string]$ProgramPath = '',
    # With -Tls: skip the read-only firewall check because you ran the printed commands (or know a rule covers both programs).
    [switch]$FirewallReady
)

$ErrorActionPreference = 'Stop'
. (Join-Path -Path (Split-Path -Parent $PSScriptRoot) -ChildPath 'cam.testlib.ps1')

$rig = $null; $ui = $null
$binDir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\livedemo-bin'
$uiCopy = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\camui-livedemo'
$savedExe = $env:CAMUI_TEST_EXE
$refused = $false
if ($Tls) { $Lan = $true }
if (-not $ProgramPath) { $ProgramPath = Join-Path -Path $env:LOCALAPPDATA -ChildPath 'IPCCTV\demo\camui.exe' }
$mediaProgram = 'C:\mediamtx\mediamtx.exe'
$mediaPort = 18189

function Test-CamDemoFirewallCovered {
    # Read-only: is there an ENABLED inbound ALLOW rule for $Program that applies to Private networks and to this protocol/port?
    param([string]$Program, [string]$Protocol, [int]$LocalPort)
    foreach ($filter in @(Get-NetFirewallApplicationFilter -ErrorAction SilentlyContinue)) {
        if ([string]$filter.Program -ne $Program) { continue }
        $rule = $filter | Get-NetFirewallRule -ErrorAction SilentlyContinue
        if (-not $rule -or $rule.Enabled -ne 'True' -or $rule.Direction -ne 'Inbound' -or $rule.Action -ne 'Allow' -or [string]$rule.Profile -notmatch 'Private|Any') { continue }
        $ports = @($rule | Get-NetFirewallPortFilter -ErrorAction SilentlyContinue)
        if (-not $ports.Count) { return $true }
        foreach ($pf in $ports) {
            if (([string]$pf.Protocol -in 'Any', $Protocol) -and ([string]$pf.LocalPort -eq 'Any' -or @(([string]$pf.LocalPort) -split ',') -contains [string]$LocalPort)) { return $true }
        }
    }
    return $false
}

try {
    # Its own ports must be free; the real system's are only reported.
    $busy = @(@($Port, 18554, 18557, 18564, 18567, 18889) | Where-Object { -not (Test-CamTestPortFree -Port $_) })
    if (-not (Test-CamTestUdpPortFree -Port 18189)) { $busy += 'udp 18189' }
    if ($busy.Count) { throw ("port(s) {0} are already in use (another demo, or a leftover of a test); nothing was started." -f ($busy -join ', ')) }
    $real = @(@(8787, 8554, 9997, 8889) | Where-Object { -not (Test-CamTestPortFree -Port $_) })
    if ($real.Count) { Write-Host ("note: port(s) {0} are held by something else (the real system, if it is running). The demo does not use or touch them." -f ($real -join ', ')) -ForegroundColor DarkYellow }
    foreach ($need in @('C:\mediamtx\mediamtx.exe', (Get-CamConfigDefault).FfmpegPath)) { if (-not (Test-Path -LiteralPath $need)) { throw "$need is needed (the demo copies MediaMTX and uses ffmpeg for the synthetic cameras)." } }

    $liveConfig = @{}
    if ($Lan -or $LanAddress) {
        $candidates = @(Get-CamLanCandidates)
        if ($LanAddress) {
            $pick = @($candidates | Where-Object { $_.Address -eq $LanAddress })[0]
            if (-not $pick) { throw "-LanAddress $LanAddress is not a private IPv4 address of an up adapter on this PC (found: $(@($candidates | ForEach-Object { $_.Address }) -join ', '))." }
        }
        else {
            $physical = @($candidates | Where-Object { -not $_.Virtual })
            $routed = @($physical | Where-Object { $_.Gateway })
            $list = (@($candidates | ForEach-Object { '{0} {1} ({2}, profile {3}, default route {4})' -f $_.Alias, $_.Address, $(if ($_.Virtual) { 'virtual' } else { 'physical' }), $_.Profile, $_.Gateway }) -join '; ')
            if ($routed.Count -eq 1) { $pick = $routed[0] }
            elseif ($routed.Count -eq 0 -and $physical.Count -eq 1) { $pick = $physical[0] }
            elseif ($physical.Count -eq 0) { throw "no physical private IPv4 address found on this PC (candidates incl. virtual adapters: $list). Pass -LanAddress <ip> to use one anyway." }
            else { throw "more than one physical private address and no single default route: $list. Pass -LanAddress <ip>." }
        }
        Write-Host ("LAN address: {0} on '{1}' ({2}, profile {3})" -f $pick.Address, $pick.Alias, $(if ($pick.Virtual) { 'virtual adapter' } else { 'physical' }), $pick.Profile) -ForegroundColor Cyan
        if ($pick.Profile -ne 'Private') { Write-Host "warning: that network is '$($pick.Profile)', not Private: a firewall rule scoped to Private networks will not apply to it." -ForegroundColor Yellow }
        $liveConfig = @{ LiveUdpAddress = '0.0.0.0:18189'; LiveAdvertiseHosts = @($pick.Address); LiveExePath = 'C:\mediamtx\mediamtx.exe' }
    }


    $demoPassword = ''
    $fwTcp = "New-NetFirewallRule -DisplayName 'IPCCTV demo camui HTTPS (TCP $TlsPort)' -Direction Inbound -Action Allow -Protocol TCP -LocalPort $TlsPort -Program '$ProgramPath' -Profile Private -RemoteAddress LocalSubnet"
    $fwUdp = "New-NetFirewallRule -DisplayName 'IPCCTV demo live media (UDP $mediaPort)' -Direction Inbound -Action Allow -Protocol UDP -LocalPort $mediaPort -Program '$mediaProgram' -Profile Private -RemoteAddress LocalSubnet"
    $fwRemove = "Remove-NetFirewallRule -DisplayName 'IPCCTV demo camui HTTPS (TCP $TlsPort)', 'IPCCTV demo live media (UDP $mediaPort)'"
    if ($Tls) {
        if (-not (Test-CamTestPortFree -Port $TlsPort)) { throw "TCP port $TlsPort (-TlsPort) is already in use; nothing was started." }
        $coveredTcp = Test-CamDemoFirewallCovered -Program $ProgramPath -Protocol 'TCP' -LocalPort $TlsPort
        $coveredUdp = Test-CamDemoFirewallCovered -Program $mediaProgram -Protocol 'UDP' -LocalPort $mediaPort
        Write-Host ("firewall (read-only look): an allow rule covers camui at the demo path on TCP {0}: {1}; MediaMTX on UDP {2}: {3}" -f $TlsPort, $coveredTcp, $mediaPort, $coveredUdp)
        if (-not (($coveredTcp -and $coveredUdp) -or $FirewallReady)) {
            $refused = $true
            Write-Host ''
            Write-Host 'Not starting: binding beyond loopback with no allow rule would make Windows prompt (and a dismissed prompt can create a block rule).' -ForegroundColor Yellow
            Write-Host 'Run these in an ELEVATED (Administrator) PowerShell, then start the demo again (add -FirewallReady if you skip the check):'
            Write-Host "  $fwTcp"
            Write-Host "  $fwUdp"
            Write-Host 'Take them away again afterwards with:'
            Write-Host "  $fwRemove"
            Write-Host ("Also: the Wi-Fi network must be a Private profile (now '{0}'): Get-NetConnectionProfile; Set-NetConnectionProfile -InterfaceAlias '{1}' -NetworkCategory Private" -f $pick.Profile, $pick.Alias)
            throw 'refused: no firewall rule is known to cover the demo (nothing was started).'
        }
        if ($FirewallReady -and -not ($coveredTcp -and $coveredUdp)) { Write-Host 'warning: -FirewallReady given but no covering rule was found; if you have not run the commands, Windows may prompt.' -ForegroundColor Yellow }
    }

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
    $tlsExtra = @()
    $tlsDir = ''
    if ($Tls) {
        # The fixed program path keeps the firewall rule stable; the exe copy holds no key and is left in place.
        [void](New-Item -ItemType Directory -Path (Split-Path -Parent $ProgramPath) -Force)
        Copy-Item -LiteralPath $exe -Destination $ProgramPath -Force
        $tlsDir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\livedemo\tls'
        $tlsExtra = @('-tls-listen', ('{0}:{1}' -f $pick.Address, $TlsPort), '-tls-dir', $tlsDir)
    }

    Write-Host 'starting the rig: supervisor, live-view MediaMTX and six synthetic cameras (about 30 s) ...'
    $cameras = 'Front Entrance', 'Rear Balcony', 'Field Front', 'Car Garage', 'Workshop Entrance', 'Workshop' |
        ForEach-Object { [pscustomobject]@{ Name = $_; IP = '127.0.0.1:18554'; Quality = 'stream1'; Record = ($_ -eq 'Front Entrance') } }
    $rig = Start-CamTestLiveRig -Name 'livedemo' -Cameras $cameras -CameraPath @('stream1', 'stream2') -LiveCloseAfterSeconds 10 -Config $liveConfig
    if ($Tls) {
        $tlsDir = Join-Path -Path $rig.Dir -ChildPath 'tls'
        $tlsExtra = @('-tls-listen', ('{0}:{1}' -f $pick.Address, $TlsPort), '-tls-dir', $tlsDir)
        # -print-ca creates the throwaway CA in the demo's own folder (never %LOCALAPPDATA%\IPCCTV\tls) and prints its fingerprint.
        $caText = @(& $ProgramPath '-print-ca' '-tls-dir' $tlsDir)
        $caFile = ([regex]::Match(($caText -join "`n"), 'file:\s+(\S.*)')).Groups[1].Value.Trim()
        $caFp = ([regex]::Match(($caText -join "`n"), 'SHA-256:\s+([0-9A-F:]{95})')).Groups[1].Value
        # The throwaway user. The password is random (24 characters from a 57-character alphabet, about 140 bits, from the system generator, without modulo bias) and goes to camui on standard input.
        $authDir = Join-Path -Path $rig.Dir -ChildPath 'auth'
        $alphabet = [char[]]'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'
        $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
        $demoPassword = -join (1..24 | ForEach-Object { $b = New-Object byte[] 1; do { $rng.GetBytes($b) } while ($b[0] -ge (256 - (256 % $alphabet.Count))); $alphabet[$b[0] % $alphabet.Count] })
        $made = @($demoPassword | & $ProgramPath '-set-password' 'demo' '-auth-dir' $authDir 2>&1)
        if (($made -join ' ') -notmatch 'created user demo') { throw ('could not create the demo user: ' + ($made -join ' ')) }
        $tlsExtra += @('-auth-dir', $authDir)
    }
    $ui = Start-CamTestLiveCamui -Rig $rig -Port $Port -Name 'camui-livedemo' -ExtraArgs $tlsExtra -RunExe $(if ($Tls) { $ProgramPath } else { '' })
    if (-not $ui.Started) { throw ('camui did not start: ' + $ui.Reason) }

    Write-Host ''
    Write-Host ("  Live screen:  http://127.0.0.1:{0}/#live" -f $Port) -ForegroundColor Green
    Write-Host ("  One camera:   http://127.0.0.1:{0}/#live/Front%20Entrance" -f $Port)
    Write-Host '  Sign-in:      none (loopback only)'
    if ($liveConfig.Count -and -not $Tls) { Write-Host '  Media:        loopback and the LAN address above. The page itself is still loopback only: other devices cannot open it yet.' }
    if ($Tls) {
        $up = $false
        for ($i = 0; $i -lt 40 -and -not $up; $i++) { $tcp = New-Object System.Net.Sockets.TcpClient; try { $tcp.Connect($pick.Address, $TlsPort); $up = $true } catch { Start-Sleep -Milliseconds 250 } finally { $tcp.Close() } }
        if (-not $up) { throw 'the HTTPS listener did not come up.' }
        Write-Host '  ---- HTTPS for a phone or another PC on this Wi-Fi (view-only, fake cameras) ----' -ForegroundColor Green
        Write-Host ("  Open:         https://{0}:{1}/#live" -f $pick.Address, $TlsPort) -ForegroundColor Green
        Write-Host '  User name:    demo' -ForegroundColor Green
        Write-Host ("  Password:     {0}" -f $demoPassword) -ForegroundColor Green
        Write-Host '                (shown once, here only; it is not in any command line or file, and the demo user is deleted with the demo)'
        Write-Host ("  CA file:      {0}  (this throwaway CA is deleted when the demo ends)" -f $caFile)
        Write-Host ("  Fingerprint:  {0}" -f $caFp)
        Write-Host '  On the phone: 1) be on this PC''s Wi-Fi (a Private network). 2) Open the link; the first certificate warning is expected: continue, then sign in.'
        Write-Host '                3) Settings tab > Connect a phone: compare the fingerprint, download the certificate, install it (iPhone: Settings > General > VPN & Device'
        Write-Host '                Management, then About > Certificate Trust Settings; Android: Settings > Security > Install a certificate > CA certificate), reopen the link.'
        Write-Host '  Firewall rules this needs (ELEVATED PowerShell; NOT applied by the demo):'
        Write-Host "    $fwTcp"
        Write-Host "    $fwUdp"
        Write-Host "    remove: $fwRemove"
    }
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
    if ($Tls -and -not $refused) {
        $left = @(Get-CimInstance Win32_Process -Filter "Name = 'camui.exe' or Name = 'mediamtx.exe' or Name = 'ffmpeg.exe'" -ErrorAction SilentlyContinue | Where-Object { $_.CommandLine -and $_.CommandLine -like '*livedemo*' })
        Write-Host ("remaining: demo processes {0}; demo TLS folder (CA and keys) present: {1}; left in place on purpose: {2} (a copy of camui, no key)" -f $left.Count, $(if ($tlsDir) { Test-Path -LiteralPath $tlsDir } else { $false }), $ProgramPath)
    }
    Write-Host $(if ($refused) { 'done: nothing was started.' } else { 'done: everything the demo started is stopped and its temp folders are removed.' })
}
