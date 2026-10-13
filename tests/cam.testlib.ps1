<#
    cam.testlib.ps1 - reusable test primitives for the IPCCTV recorder.

    Dot-source it, do not run it:

        . "$PSScriptRoot\cam.testlib.ps1"

    This exists because four separate commissions each rebuilt the same
    scaffolding in throwaway files: launching a supervisor, scanning the disk
    for the camera credential, parse-checking every script, proving no process
    was left behind. Extend this file rather than writing another one.

    What lives here is only the primitives. The checks that use them, and the
    pass or fail decision, live in cam.tests.ps1.

    Targets Windows PowerShell 5.1.
#>

$ErrorActionPreference = 'Stop'

$script:CamTestRoot = $PSScriptRoot
if (-not $script:CamTestRoot) { $script:CamTestRoot = (Get-Location).Path }
$script:CamTestRepo = Split-Path -Path $script:CamTestRoot -Parent

. (Join-Path -Path $script:CamTestRepo -ChildPath 'cam.common.ps1')

# cameras.json as the Operator maintains it. A commission that changes the file
# updates these two constants in the same commit, deliberately. The pin exists
# to catch a TEST RUN mutating the file, not to freeze the Operator's choices:
# this value is all six cameras with Record true, which is how he runs it.
$script:CamTestCameraListSha256 = '950627520b799752bce4f2192f8f794c3f8a3832b3d82d6dc9af3e5ce6d9b131'
$script:CamTestCameraListBom = 'efbbbf'


function Get-CamTestRepo {
    <# The repository directory - the parent of tests\. #>
    return $script:CamTestRepo
}


# ---------------------------------------------------------------------------
# Result accounting
# ---------------------------------------------------------------------------

function New-CamTestRun {
    <# A results accumulator. One per invocation of the runner. #>
    return [pscustomobject]@{
        Results   = New-Object System.Collections.ArrayList
        StartedAt = [DateTime]::Now
    }
}


function Add-CamTestResult {
    <#
        Records one check. Evidence is the point of this harness: a status
        without the command output behind it is a claim, not a result, so every
        caller passes the lines it actually measured.
    #>
    param(
        [Parameter(Mandatory = $true)]$Run,
        [Parameter(Mandatory = $true)][string]$Check,
        [Parameter(Mandatory = $true)][ValidateSet('PASS', 'FAIL', 'SKIP')][string]$Status,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Message,
        [string[]]$Evidence = @()
    )

    [void]$Run.Results.Add([pscustomobject]@{
        Check    = $Check
        Status   = $Status
        Message  = $Message
        Evidence = @($Evidence)
    })

    $colour = @{ PASS = 'Green'; FAIL = 'Red'; SKIP = 'Yellow' }[$Status]
    Write-Host ("[{0}] {1,-18} {2}" -f $Status, $Check, (Get-CamRedacted -Text $Message)) -ForegroundColor $colour
    foreach ($line in @($Evidence)) {
        Write-Host ("         | {0}" -f (Get-CamRedacted -Text $line)) -ForegroundColor 'DarkGray'
    }
}


function Write-CamTestReport {
    <# Prints the tally and returns the process exit code to use. #>
    param([Parameter(Mandatory = $true)]$Run)

    $pass = @($Run.Results | Where-Object { $_.Status -eq 'PASS' }).Count
    $fail = @($Run.Results | Where-Object { $_.Status -eq 'FAIL' }).Count
    $skip = @($Run.Results | Where-Object { $_.Status -eq 'SKIP' }).Count
    $seconds = ([DateTime]::Now - $Run.StartedAt).TotalSeconds

    Write-Host ''
    Write-Host ("{0} passed, {1} failed, {2} skipped in {3}s" -f `
        $pass, $fail, $skip, (Format-CamNumber -Value $seconds -Format '0.0')) `
        -ForegroundColor $(if ($fail -gt 0) { 'Red' } else { 'Green' })
    foreach ($result in @($Run.Results | Where-Object { $_.Status -ne 'PASS' })) {
        Write-Host ("  {0}: {1} - {2}" -f $result.Status, $result.Check, $result.Message) -ForegroundColor 'Yellow'
    }
    if ($fail -gt 0) { return 1 }
    return 0
}


# ---------------------------------------------------------------------------
# Static checks over the repository
# ---------------------------------------------------------------------------

function Test-CamScriptParse {
    <#
        Parses every .ps1 in the repository with the running PowerShell's own
        parser. Run under 5.1 this is the only check that catches PowerShell 7
        only syntax before the Operator hits it.
    #>
    param([string]$Repo)

    if (-not $Repo) { $Repo = Get-CamTestRepo }
    $files = @(Get-ChildItem -LiteralPath $Repo -Filter '*.ps1' -Recurse -File | Sort-Object FullName)
    $lines = @()
    $failed = 0

    foreach ($file in $files) {
        $errors = $null
        [void][System.Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$null, [ref]$errors)
        $count = @($errors).Count
        if ($count -eq 0) {
            $lines += ("OK   {0}" -f $file.Name)
        }
        else {
            $failed++
            $lines += ("FAIL {0}: {1} error(s)" -f $file.Name, $count)
            foreach ($item in @($errors)) {
                $lines += ("       line {0}: {1}" -f $item.Extent.StartLineNumber, $item.Message)
            }
        }
    }

    return [pscustomobject]@{
        Ok      = ($failed -eq 0 -and $files.Count -gt 0)
        Files   = $files.Count
        Failed  = $failed
        Lines   = $lines
        Version = $PSVersionTable.PSVersion.ToString()
    }
}


function Test-CamCameraListIntegrity {
    <#
        cameras.json is the Operator's own file. Every commission has to leave
        it byte-identical, including the UTF-8 BOM that the dashboard relies on,
        so the hash and the first three bytes are both asserted.
    #>
    param([string]$Path, [string]$ExpectedSha256, [string]$ExpectedBom)

    if (-not $Path) { $Path = Join-Path -Path (Get-CamTestRepo) -ChildPath 'cameras.json' }
    if (-not $ExpectedSha256) { $ExpectedSha256 = $script:CamTestCameraListSha256 }
    if (-not $ExpectedBom) { $ExpectedBom = $script:CamTestCameraListBom }

    $bytes = [System.IO.File]::ReadAllBytes($Path)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { $hash = ($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString('x2') }) -join '' }
    finally { $sha.Dispose() }

    $bom = ''
    if ($bytes.Length -ge 3) { $bom = (($bytes[0..2]) | ForEach-Object { $_.ToString('x2') }) -join '' }

    return [pscustomobject]@{
        Ok     = ($hash -eq $ExpectedSha256 -and $bom -eq $ExpectedBom)
        Sha256 = $hash
        Bom    = $bom
        Bytes  = $bytes.Length
        Path   = $Path
    }
}


# ---------------------------------------------------------------------------
# Processes
# ---------------------------------------------------------------------------

function Get-CamTestProcessCount {
    <#
        How many ffmpeg and mediamtx processes exist right now. A leaked
        mediamtx is the worst case on this machine: it holds an RTSP session on
        all six cameras at once, and each camera serves only two.
    #>
    $ffmpeg = @(Get-Process -Name 'ffmpeg' -ErrorAction SilentlyContinue)
    $mediamtx = @(Get-Process -Name 'mediamtx' -ErrorAction SilentlyContinue)
    return [pscustomobject]@{
        Ffmpeg      = $ffmpeg.Count
        MediaMtx    = $mediamtx.Count
        FfmpegIds   = @($ffmpeg | ForEach-Object { $_.Id })
        MediaMtxIds = @($mediamtx | ForEach-Object { $_.Id })
        Text        = ("ffmpeg={0} mediamtx={1}" -f $ffmpeg.Count, $mediamtx.Count)
    }
}


# ---------------------------------------------------------------------------
# Credential leak scan
# ---------------------------------------------------------------------------

function Test-CamByteSequence {
    <# Substring search over raw bytes, so an encoding cannot hide a secret. #>
    param([byte[]]$Haystack, [byte[]]$Needle)

    if ($null -eq $Needle -or $Needle.Length -eq 0) { return $false }
    if ($null -eq $Haystack -or $Haystack.Length -lt $Needle.Length) { return $false }
    for ($i = 0; $i -le $Haystack.Length - $Needle.Length; $i++) {
        $match = $true
        for ($j = 0; $j -lt $Needle.Length; $j++) {
            if ($Haystack[$i + $j] -ne $Needle[$j]) { $match = $false; break }
        }
        if ($match) { return $true }
    }
    return $false
}


function Invoke-CamCredentialLeakScan {
    <#
        Looks for the camera credential on disk. Needles are read from the DPAPI
        store into memory and are never printed or written anywhere.

        Two different questions, because they have two different answers on this
        machine:
          - the PASSWORD must not appear at all, in any form;
          - the USERNAME is only a leak in URL userinfo shape (scheme://x:y@),
            because this machine's Windows account name contains the camera
            username, so every path naming the user's home directory would
            otherwise count as a hit.

        A clean scan proves nothing on its own, so a positive control file
        carrying both shapes is written, scanned and deleted. If the control
        does not light up, the scan is reported as inconclusive rather than
        passing.
    #>
    param(
        [string[]]$Roots,
        [string[]]$ExcludeNames = @('camera_creds.xml')
    )

    $credential = Get-CamCredential
    $password = $credential.GetNetworkCredential().Password
    $user = $credential.UserName

    $forms = @($password, [System.Uri]::EscapeDataString($password)) | Select-Object -Unique
    $needles = @()
    foreach ($form in $forms) {
        $needles += , @('utf8', [System.Text.Encoding]::UTF8.GetBytes($form))
        $needles += , @('utf16', [System.Text.Encoding]::Unicode.GetBytes($form))
    }

    if (-not $Roots) {
        $config = Get-CamConfig
        $Roots = @((Get-CamTestRepo), [string]$config.RootPath, (Split-Path -Path ([string]$config.RestreamerExePath) -Parent))
    }

    $files = @()
    foreach ($root in @($Roots | Select-Object -Unique)) {
        if (-not $root -or -not (Test-Path -LiteralPath $root)) { continue }
        $files += @(Get-ChildItem -LiteralPath $root -Recurse -File -Force -ErrorAction SilentlyContinue |
            Where-Object {
                $_.Length -lt 50MB -and $_.Extension -ne '.exe' -and $_.Extension -ne '.mkv' -and
                $ExcludeNames -notcontains $_.Name -and $_.FullName -notlike '*\.git\*'
            })
    }

    $userinfo = '(?i)[a-z][a-z0-9+.\-]*://([^/\s@"'']+)@'
    $passwordHits = @()
    $userinfoHits = @()

    foreach ($file in $files) {
        $bytes = $null
        try { $bytes = [System.IO.File]::ReadAllBytes($file.FullName) } catch { continue }

        foreach ($needle in $needles) {
            if (Test-CamByteSequence -Haystack $bytes -Needle $needle[1]) {
                $passwordHits += ("{0} [{1}]" -f $file.FullName, $needle[0])
                break
            }
        }

        $text = ''
        try { $text = [System.Text.Encoding]::UTF8.GetString($bytes) } catch { continue }
        foreach ($match in [regex]::Matches($text, $userinfo)) {
            if ($match.Groups[1].Value -like "*$user*") {
                $userinfoHits += ("{0} :: {1}" -f $file.FullName, $match.Value)
            }
        }
    }

    # Positive control.
    $controlDir = Join-Path -Path $env:TEMP -ChildPath ('cam-leakscan-control-' + [Guid]::NewGuid().ToString('n').Substring(0, 8))
    $controlPassword = 0
    $controlUserinfo = 0
    try {
        [void](New-Item -ItemType Directory -Path $controlDir -Force)
        $controlFile = Join-Path -Path $controlDir -ChildPath 'control.txt'
        [System.IO.File]::WriteAllText($controlFile, ("rtsp://{0}:{1}@192.168.50.99/stream1" -f $user, $password), (New-Object System.Text.UTF8Encoding($false)))
        $controlBytes = [System.IO.File]::ReadAllBytes($controlFile)
        foreach ($needle in $needles) {
            if (Test-CamByteSequence -Haystack $controlBytes -Needle $needle[1]) { $controlPassword++ }
        }
        $controlText = [System.Text.Encoding]::UTF8.GetString($controlBytes)
        foreach ($match in [regex]::Matches($controlText, $userinfo)) {
            if ($match.Groups[1].Value -like "*$user*") { $controlUserinfo++ }
        }
    }
    finally {
        Remove-Item -LiteralPath $controlDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    $controlOk = ($controlPassword -ge 1 -and $controlUserinfo -eq 1)
    return [pscustomobject]@{
        Ok              = ($passwordHits.Count -eq 0 -and $userinfoHits.Count -eq 0 -and $controlOk)
        ControlOk       = $controlOk
        ControlRemoved  = (-not (Test-Path -LiteralPath $controlDir))
        Roots           = @($Roots)
        Files           = $files.Count
        PasswordHits    = @($passwordHits)
        UserinfoHits    = @($userinfoHits)
        Text            = ("{0} file(s) over {1} root(s): password hits {2}, URL-userinfo hits {3}, positive control {4}" -f `
                            $files.Count, @($Roots).Count, $passwordHits.Count, $userinfoHits.Count, $(if ($controlOk) { 'lit up' } else { 'DID NOT LIGHT UP' }))
    }
}


function Start-CamTestUi {
    <#
        Starts camui.exe on a loopback port for the UI leak scan, from a copy
        under %TEMP% rather than from the repo: the repo may live on the
        \\wsl.localhost share and a Windows process should not be launched from
        there.

        Returns the process, the base URL and the log paths, or $null with a
        Reason when camui.exe has not been built. Stop it with Stop-CamTestUi,
        which stops the pid this returned - never a process matched by name.
    #>
    param(
        [int]$Port = 18787,
        [string]$StatusFile,
        [string]$ScriptDir,
        [string]$Token
    )

    $source = Join-Path -Path (Get-CamTestRepo) -ChildPath 'camui.exe'
    if (-not (Test-Path -LiteralPath $source)) {
        return [pscustomobject]@{ Started = $false; Reason = 'camui.exe is not built; run ui/build.sh' }
    }
    if (-not $StatusFile) {
        $StatusFile = Join-Path -Path ([string](Get-CamConfig).RootPath) -ChildPath '_state\status.json'
    }
    # The repository, not the %TEMP% copy's own directory: /api/cameras and
    # /api/config read cameras.json and recorder.config.json from here, and the
    # point of the leak scan is to prove the OPERATOR's real files do not leak.
    # Both endpoints are read-only in this scan; nothing is written.
    if (-not $ScriptDir) { $ScriptDir = Get-CamTestRepo }

    $dir = Join-Path -Path $env:TEMP -ChildPath 'cam-tests\camui'
    [void](New-Item -ItemType Directory -Path $dir -Force)
    $exe = Join-Path -Path $dir -ChildPath 'camui.exe'
    Copy-Item -LiteralPath $source -Destination $exe -Force

    $out = Join-Path -Path $dir -ChildPath 'camui.out.log'
    $err = Join-Path -Path $dir -ChildPath 'camui.err.log'
    Remove-Item -LiteralPath $out, $err -Force -ErrorAction SilentlyContinue

    if ($Token) { $env:CAMUI_TOKEN = $Token } else { Remove-Item Env:\CAMUI_TOKEN -ErrorAction SilentlyContinue }
    $process = Start-Process -FilePath $exe `
        -ArgumentList '-listen', ("127.0.0.1:{0}" -f $Port), '-status-file', $StatusFile, '-script-dir', $ScriptDir `
        -PassThru -WindowStyle Hidden -RedirectStandardOutput $out -RedirectStandardError $err

    $base = "http://127.0.0.1:$Port"
    $deadline = (Get-Date).AddSeconds(15)
    $listening = $false
    while ((Get-Date) -lt $deadline) {
        if ($process.HasExited) { break }
        if (Get-NetTCPConnection -LocalPort $Port -State Listen -OwningProcess $process.Id -ErrorAction SilentlyContinue) {
            $listening = $true
            break
        }
        Start-Sleep -Milliseconds 200
    }

    return [pscustomobject]@{
        Started    = $listening
        Reason     = $(if ($listening) { '' } elseif ($process.HasExited) { "camui.exe exited with $($process.ExitCode)" } else { 'camui.exe did not start listening within 15s' })
        Process    = $process
        BaseUrl    = $base
        Port       = $Port
        StatusFile = $StatusFile
        ScriptDir  = $ScriptDir
        StdOut     = $out
        StdErr     = $err
    }
}


function Stop-CamTestUi {
    <# Stops the pid Start-CamTestUi returned, and returns its stdout. #>
    param($Ui)

    if (-not $Ui -or -not $Ui.Process) { return @() }
    if (-not $Ui.Process.HasExited) {
        Stop-Process -Id $Ui.Process.Id -ErrorAction SilentlyContinue
        [void]$Ui.Process.WaitForExit(5000)
    }
    Remove-Item Env:\CAMUI_TOKEN -ErrorAction SilentlyContinue
    $lines = @()
    foreach ($log in @($Ui.StdOut, $Ui.StdErr)) {
        if ($log -and (Test-Path -LiteralPath $log)) {
            $lines += @(Get-Content -LiteralPath $log -ErrorAction SilentlyContinue)
        }
    }
    return $lines
}


function Invoke-CamUiCredentialLeakScan {
    <#
        Fetches every camui endpoint and scans the RESPONSE BYTES for the
        camera credential and for anything address-shaped. This is the Frigate
        CVE-2026-33469 check: any logged-in viewer there could read the camera
        RTSP credentials, so what matters is not what the status file holds but
        what crosses the wire.

        Three questions, because they have three different answers:
          - the PASSWORD must not appear at all, raw or URL-escaped, in UTF-8
            or UTF-16 over every byte of the response;
          - the USERNAME is only a leak in URL userinfo shape (scheme://x:y@),
            which is the only shape redaction has to prevent and the only one
            that cannot collide with this machine's Windows account name;
          - no URL of any scheme may survive at all, because a camera is
            addressed here by NAME and the page needs no address - that is what
            catches Restreamer.RelayBase and an ffmpeg error carrying an
            rtsp:// URL.

        /api/cameras is in the list and is the one response that legitimately
        contains camera addresses: cameras.json is the Operator's own file and
        an editor that cannot show what it is editing is not an editor. A bare
        dotted quad is therefore not a hit - but a URL of any scheme still is,
        so a credential or a relay base appearing there is still caught. The
        allow-list behind /api/config is what the URL rule tests there:
        RelayBase lives in recorder.config.json and must not come through.

        Needles are read from the DPAPI store into memory and are never printed
        or written anywhere. A clean scan proves nothing on its own, so a
        synthetic body carrying all three shapes is run through the same
        scanner; if it does not light up the result is inconclusive, not a pass.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$BaseUrl,
        [string[]]$Paths = @('/', '/index.html', '/style.css', '/app.js', '/api/status', '/api/cameras', '/api/config'),
        [string]$Token
    )

    $credential = Get-CamCredential
    $password = $credential.GetNetworkCredential().Password
    $user = $credential.UserName

    $needles = @()
    foreach ($form in (@($password, [System.Uri]::EscapeDataString($password)) | Select-Object -Unique)) {
        $needles += , @('utf8', [System.Text.Encoding]::UTF8.GetBytes($form))
        $needles += , @('utf16', [System.Text.Encoding]::Unicode.GetBytes($form))
    }
    $userinfoPattern = '(?i)[a-z][a-z0-9+.\-]*://([^/\s@"'']+)@'
    $urlPattern = '(?i)[a-z][a-z0-9+.\-]*://[^\s"'',)]+'

    # One scanner, used for the real responses and for the positive control.
    $scanBytes = {
        param([byte[]]$Bytes, [string]$Label)

        $found = @()
        foreach ($needle in $needles) {
            if (Test-CamByteSequence -Haystack $Bytes -Needle $needle[1]) {
                $found += ("PASSWORD in {0} [{1}]" -f $Label, $needle[0])
            }
        }
        $text = [System.Text.Encoding]::UTF8.GetString($Bytes)
        foreach ($match in [regex]::Matches($text, $userinfoPattern)) {
            if ($match.Groups[1].Value -like "*$user*") {
                $found += ("USERINFO in {0} :: {1}" -f $Label, $match.Value)
            }
        }
        foreach ($match in [regex]::Matches($text, $urlPattern)) {
            $found += ("URL in {0} :: {1}" -f $Label, $match.Value)
        }
        return @($found)
    }

    $endpoints = @()
    $hits = @()
    foreach ($path in $Paths) {
        $url = ($BaseUrl.TrimEnd('/')) + $path
        $client = New-Object System.Net.WebClient
        if ($Token) { $client.Headers.Add('Authorization', "Bearer $Token") }
        $bytes = $null
        $fetchError = ''
        try { $bytes = $client.DownloadData($url) }
        catch { $fetchError = $_.Exception.Message }
        finally { $client.Dispose() }

        if ($null -eq $bytes) {
            $endpoints += [pscustomobject]@{ Path = $path; Bytes = 0; Hits = 0; Error = $fetchError }
            continue
        }
        $found = @(& $scanBytes $bytes $path)
        $hits += $found
        $endpoints += [pscustomobject]@{ Path = $path; Bytes = $bytes.Length; Hits = $found.Count; Error = '' }
    }

    # Positive control: the same scanner over a body that really does leak.
    $controlBody = [System.Text.Encoding]::UTF8.GetBytes(
        ('{{"RelayBase":"rtsp://127.0.0.1:8554","LastError":"open rtsp://{0}:{1}@192.168.50.99/stream1 failed"}}' -f $user, $password))
    $controlHits = @(& $scanBytes $controlBody 'positive-control')
    $controlOk = (@($controlHits | Where-Object { $_ -like 'PASSWORD*' }).Count -ge 1 -and
                  @($controlHits | Where-Object { $_ -like 'USERINFO*' }).Count -ge 1 -and
                  @($controlHits | Where-Object { $_ -like 'URL*' }).Count -ge 2)

    $failed = @($endpoints | Where-Object { $_.Error })
    return [pscustomobject]@{
        Ok        = ($hits.Count -eq 0 -and $failed.Count -eq 0 -and $controlOk)
        ControlOk = $controlOk
        Endpoints = @($endpoints)
        Hits      = @($hits)
        Unreached = @($failed)
        Text      = ("{0} endpoint(s) scanned, {1} byte(s): password/userinfo/URL hits {2}, unreachable {3}, positive control {4}" -f `
                        @($endpoints).Count, (@($endpoints) | Measure-Object -Property Bytes -Sum).Sum, $hits.Count, $failed.Count,
                        $(if ($controlOk) { 'lit up' } else { 'DID NOT LIGHT UP' }))
    }
}


# ---------------------------------------------------------------------------
# Sandbox: the product scripts with their own config beside them
# ---------------------------------------------------------------------------

function New-CamTestSandbox {
    <#
        Copies the product scripts into a scratch directory with their own
        cameras.json, recorder.config.json and credential store.

        This is the only safe way to run the supervisor in a test: every path in
        cam.common.ps1 is resolved relative to the script's own directory, so a
        sandbox copy cannot touch the Operator's cameras.json or archive.

        -UseRealCredential copies the DPAPI store so the sandbox can talk to the
        real cameras. Without it a throwaway credential is created, which is
        what every camera-free check wants.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][object[]]$Cameras,
        [hashtable]$Config = @{},
        [switch]$UseRealCredential
    )

    $repo = Get-CamTestRepo
    $dir = Join-Path -Path $env:TEMP -ChildPath ("cam-tests\" + $Name)
    if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force }
    [void](New-Item -ItemType Directory -Path $dir -Force)

    foreach ($file in @('cam.common.ps1', 'cam.recorder.ps1', 'cam.health.ps1', 'mediamtx.template.yml')) {
        Copy-Item -LiteralPath (Join-Path -Path $repo -ChildPath $file) -Destination $dir -Force
    }

    # BOM, like the Operator's own file, so the sandbox exercises the same read.
    $json = ConvertTo-Json -InputObject @($Cameras) -Depth 5
    [System.IO.File]::WriteAllText((Join-Path -Path $dir -ChildPath 'cameras.json'), $json, (New-Object System.Text.UTF8Encoding($true)))

    $root = Join-Path -Path $dir -ChildPath 'archive'
    [void](New-Item -ItemType Directory -Path $root -Force)

    $settings = [ordered]@{
        RootPath                = $root
        SegmentSeconds          = 20
        ReserveGB               = 0.5
        PruneHysteresisGB       = 0.1
        PruneIntervalSeconds    = 60
        ReconcileSeconds        = 5
        StatusIntervalSeconds   = 5
        ArchiveScanSeconds      = 15
        BackoffStartSeconds     = 3
        BackoffMaxSeconds       = 10
        StopTimeoutSeconds      = 45
        RestreamerEnabled       = $false
        RelayStartupWaitSeconds = 10
    }
    foreach ($key in $Config.Keys) { $settings[$key] = $Config[$key] }
    [System.IO.File]::WriteAllText((Join-Path -Path $dir -ChildPath 'recorder.config.json'),
        (ConvertTo-Json -InputObject $settings -Depth 5), (New-Object System.Text.UTF8Encoding($false)))

    if ($UseRealCredential) {
        Copy-Item -LiteralPath (Join-Path -Path $repo -ChildPath 'camera_creds.xml') -Destination $dir -Force
    }
    else {
        # Deliberately greppable and fake, so a redaction check has a needle it
        # is allowed to know.
        $secure = ConvertTo-SecureString 'FakePassword-DoNotUse-9c41' -AsPlainText -Force
        (New-Object System.Management.Automation.PSCredential('fakeuser', $secure)) |
            Export-Clixml -Path (Join-Path -Path $dir -ChildPath 'camera_creds.xml')
    }

    return [pscustomobject]@{
        Dir        = $dir
        Recorder   = (Join-Path -Path $dir -ChildPath 'cam.recorder.ps1')
        Health     = (Join-Path -Path $dir -ChildPath 'cam.health.ps1')
        Cameras    = (Join-Path -Path $dir -ChildPath 'cameras.json')
        ConfigPath = (Join-Path -Path $dir -ChildPath 'recorder.config.json')
        Config     = (Get-CamConfig -Path (Join-Path -Path $dir -ChildPath 'recorder.config.json'))
        Root       = $root
        ConsoleLog = (Join-Path -Path $dir -ChildPath 'supervisor.console.log')
    }
}


function Invoke-CamTestScript {
    <#
        Runs one of the product scripts in its own 5.1 process and returns the
        exit code with the combined output. A separate process is the point:
        dot-sourcing the recorder into the harness would share state and would
        not prove the script works as the Operator invokes it.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [string[]]$Arguments = @(),
        [int]$TimeoutSeconds = 300
    )

    $outFile = Join-Path -Path $env:TEMP -ChildPath ('cam-test-out-' + [Guid]::NewGuid().ToString('n').Substring(0, 8) + '.txt')
    $errFile = "$outFile.err"
    $argumentList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $Path) + $Arguments

    $process = Start-Process -FilePath 'powershell.exe' -ArgumentList $argumentList -PassThru `
        -WindowStyle Hidden -RedirectStandardOutput $outFile -RedirectStandardError $errFile
    [void]$process.Handle
    if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
        try { $process.Kill() } catch { }
        throw "'$Path $($Arguments -join ' ')' did not finish within ${TimeoutSeconds}s."
    }

    $out = ''
    $err = ''
    try { $out = [System.IO.File]::ReadAllText($outFile) } catch { }
    try { $err = [System.IO.File]::ReadAllText($errFile) } catch { }
    Remove-Item -LiteralPath $outFile, $errFile -Force -ErrorAction SilentlyContinue

    return [pscustomobject]@{
        ExitCode = $process.ExitCode
        Output   = $out
        Error    = $err
        Lines    = @(($out + $err) -split "`r?`n" | Where-Object { $_.Trim().Length -gt 0 })
    }
}


function Start-CamTestSupervisor {
    <#
        Launches the sandbox supervisor detached, with its console captured, and
        waits until its own status file says it is running. Returns the
        powershell process.
    #>
    param(
        [Parameter(Mandatory = $true)]$Sandbox,
        [int]$TimeoutSeconds = 90
    )

    $process = Start-Process -FilePath 'powershell.exe' -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $Sandbox.ConsoleLog -RedirectStandardError "$($Sandbox.ConsoleLog).err" `
        -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $Sandbox.Recorder, '-Run')
    [void]$process.Handle

    $deadline = [DateTime]::Now.AddSeconds($TimeoutSeconds)
    while ([DateTime]::Now -lt $deadline) {
        if ($process.HasExited) {
            throw "The sandbox supervisor exited immediately with code $($process.ExitCode). Console: $($Sandbox.ConsoleLog)"
        }
        $state = Get-CamSupervisorState -Config $Sandbox.Config -Snapshot (Get-CamStatusSnapshot -Config $Sandbox.Config)
        if ($state.Running) { return $process }
        Start-Sleep -Milliseconds 500
    }
    throw "The sandbox supervisor did not report itself running within ${TimeoutSeconds}s. Console: $($Sandbox.ConsoleLog)"
}


function Stop-CamTestSupervisor {
    <#
        Stops the sandbox supervisor through the product's own -Stop, which is
        what the Operator uses and what has to leave nothing behind. A hard kill
        happens only if -Stop failed, and is reported, never hidden: leaving a
        mediamtx or an ffmpeg running would hold camera sessions.
    #>
    param(
        [Parameter(Mandatory = $true)]$Sandbox,
        $Process,
        [int]$TimeoutSeconds = 120
    )

    $result = Invoke-CamTestScript -Path $Sandbox.Recorder -Arguments @('-Stop') -TimeoutSeconds $TimeoutSeconds
    $killed = @()

    if ($Process -and -not $Process.HasExited) {
        if (-not $Process.WaitForExit(15000)) {
            $killed += ("supervisor powershell pid {0}" -f $Process.Id)
            try { $Process.Kill() } catch { }
            [void]$Process.WaitForExit(10000)
        }
    }

    return [pscustomobject]@{
        Ok       = ($result.ExitCode -eq 0 -and $killed.Count -eq 0)
        ExitCode = $result.ExitCode
        Lines    = $result.Lines
        Killed   = @($killed)
    }
}


function Remove-CamTestCredential {
    <#
        Deletes the sandbox's copy of the DPAPI credential store.

        A live check has to copy it so the product can authenticate against the
        real cameras, and the sandbox is deliberately left behind afterwards for
        inspection - but there is no reason for a second copy of the credential
        store to outlive the run, so every live check calls this when it is
        done. The logs and the status files it captured are unaffected.
    #>
    param([Parameter(Mandatory = $true)]$Sandbox)

    $path = Join-Path -Path $Sandbox.Dir -ChildPath 'camera_creds.xml'
    if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
    return (-not (Test-Path -LiteralPath $path))
}


function Get-CamTestStatusJson {
    <# The sandbox status file as text and as an object. #>
    param([Parameter(Mandatory = $true)]$Sandbox)

    $path = Get-CamStatusFilePath -Config $Sandbox.Config
    if (-not (Test-Path -LiteralPath $path)) { return $null }
    $text = Get-CamFileText -Path $path
    return [pscustomobject]@{
        Path     = $path
        Text     = $text
        Snapshot = (ConvertFrom-Json -InputObject $text)
    }
}
