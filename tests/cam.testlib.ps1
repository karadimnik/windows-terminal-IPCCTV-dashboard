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
        [string[]]$Paths = @('/', '/index.html', '/style.css', '/app.js', '/api/status', '/api/cameras', '/api/config', '/api/footage'),
        [string[]]$ExtraPaths = @(),
        [int]$MaxBytes = 2097152,
        [string]$Token
    )

    $Paths = @($Paths) + @($ExtraPaths)

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

    # Read a bounded prefix rather than the whole body, and abort the request
    # when the cap is reached. /api/clip is a 170 MB remux produced on the fly,
    # so DownloadData would both exhaust memory and hold an ffmpeg open for the
    # length of a segment. An error body is scanned too: a refusal that echoed a
    # path or a URL back would be just as much of a leak as a success that did.
    $readBounded = {
        param([string]$url)

        $request = [System.Net.HttpWebRequest]::Create($url)
        $request.Method = 'GET'
        if ($Token) { $request.Headers.Add('Authorization', "Bearer $Token") }
        $response = $null
        $failure = ''
        try { $response = $request.GetResponse() }
        catch [System.Net.WebException] {
            $failure = $_.Exception.Message
            $response = $_.Exception.Response
        }
        if ($null -eq $response) { return [pscustomobject]@{ Bytes = $null; Error = $failure } }

        $buffer = New-Object System.IO.MemoryStream
        $chunk = New-Object byte[] 65536
        try {
            $stream = $response.GetResponseStream()
            while ($buffer.Length -lt $MaxBytes) {
                $read = $stream.Read($chunk, 0, $chunk.Length)
                if ($read -le 0) { break }
                $buffer.Write($chunk, 0, $read)
            }
        }
        catch { if (-not $failure) { $failure = $_.Exception.Message } }
        finally {
            $request.Abort()
            $response.Close()
        }
        return [pscustomobject]@{ Bytes = $buffer.ToArray(); Error = $failure }
    }

    $endpoints = @()
    $hits = @()
    foreach ($path in $Paths) {
        $url = ($BaseUrl.TrimEnd('/')) + $path
        $fetched = & $readBounded $url
        $bytes = $fetched.Bytes
        $fetchError = ''
        if ($null -eq $bytes -or $bytes.Length -eq 0) { $fetchError = $fetched.Error }

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


# ---------------------------------------------------------------------------
# A warden and the window it watches
# ---------------------------------------------------------------------------

function Start-CamTestWardenWindow {
    <#
        A sandbox stand-in for the NVR: one terminal window of its own, one
        pane inside a job of its own, and a warden watching that window.

        The pane does nothing but join the job and sleep. What is under test is
        the WARDEN - what it does when the window goes - and a sleeping pane
        proves the kernel sweep just as well as a supervisor would while
        costing no camera session and no ffmpeg.

        Everything runs from the SANDBOX copy of cam.common.ps1, so the
        warden's Get-CamConfig resolves to the sandbox's own config and it
        writes the sandbox's status.json, never the Operator's.

        No -StopViewers: a test must not stop the Operator's dashboard.

        Returns the warden pid, the pane pid, and the warden's log path. The
        caller is responsible for Stop-CamTestWardenWindow.
    #>
    param(
        [Parameter(Mandatory = $true)]$Sandbox,
        [Parameter(Mandatory = $true)][string]$Title,
        [Parameter(Mandatory = $true)][string]$JobName,
        [int]$TimeoutSeconds = 60
    )

    Import-CamJobApi
    $lib = (Join-Path -Path $Sandbox.Dir -ChildPath 'cam.common.ps1').Replace("'", "''")
    $log = Join-Path -Path $Sandbox.Dir -ChildPath ('warden-' + $Title + '.log')
    $panePidFile = Join-Path -Path $Sandbox.Dir -ChildPath ('pane-' + $Title + '.pid')
    Remove-Item -LiteralPath $log, $panePidFile -Force -ErrorAction SilentlyContinue

    $wardenScript = ". '{0}'`nStart-CamJobWarden -JobName '{1}' -WindowTitle '{2}' -LogPath '{3}' -AppearTimeoutSeconds {4} -AlertOnWindowClose" -f `
        $lib, $JobName.Replace("'", "''"), $Title.Replace("'", "''"), $log.Replace("'", "''"), $TimeoutSeconds
    $warden = Start-CamDetachedProcess -Script $wardenScript

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline -and -not [CamJob]::Exists($JobName)) { Start-Sleep -Milliseconds 250 }

    $paneScript = ". '{0}'`nAdd-CamJobMember -JobName '{1}'`n`"`$PID`" | Out-File -FilePath '{2}' -Encoding ascii`nwhile (`$true) {{ Start-Sleep -Seconds 5 }}" -f `
        $lib, $JobName.Replace("'", "''"), $panePidFile.Replace("'", "''")
    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($paneScript))
    & wt.exe @('-w', ('ipcctv-test-' + $Title), 'new-tab', '--title', $Title, '--suppressApplicationTitle',
        'powershell.exe', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-EncodedCommand', $encoded)

    $panePid = 0
    $windowFound = $false
    $watching = $false
    $hwnd = [IntPtr]::Zero
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        if (-not $windowFound) {
            # Kept as a handle, not just a count: once a foreign tab is added
            # the title no longer finds this window, and the handle is then the
            # only way the test can check on it or close it.
            $found = @(Get-CamTerminalWindow -Title $Title)
            if ($found.Count -ge 1) { $windowFound = $true; $hwnd = $found[0] }
        }
        if ($panePid -le 0 -and (Test-Path -LiteralPath $panePidFile)) {
            try { $panePid = [int](Get-Content -LiteralPath $panePidFile -Raw).Trim() } catch { }
        }
        if (Test-Path -LiteralPath $log) {
            $watching = [bool]@(Get-Content -LiteralPath $log | Select-String -SimpleMatch 'watching window').Count
        }
        if ($windowFound -and $panePid -gt 0 -and $watching) { break }
        Start-Sleep -Milliseconds 500
    }

    return [pscustomobject]@{
        Title       = $Title
        JobName     = $JobName
        WardenPid   = $warden.Id
        PanePid     = $panePid
        WindowFound = $windowFound
        Watching    = $watching
        LogPath     = $log
        Hwnd        = $hwnd.ToInt64()
        WtName      = 'ipcctv-test-' + $Title
        ForeignPid  = 0
    }
}


function Add-CamTestForeignTab {
    <#
        Adds a second tab to a test window, which is how a Windows Terminal
        window stops answering to our title: the title follows the ACTIVE tab,
        and this machine's windowingBehavior is useExisting, so a console
        started elsewhere lands in an existing window exactly like this. The
        live NVR window had acquired one named 'nk' on 2026-10-14 and
        Get-CamTerminalWindow -Title cam.nvr found nothing.

        The tab sets its own title so the divergence is assertable rather than
        whatever the shell happens to display, and writes its pid down so the
        cleanup can reach it if it outlives the window.

        Returns the title the window now shows, or '' if it never changed.
    #>
    param(
        [Parameter(Mandatory = $true)]$Window,
        [string]$ForeignTitle = 'cam.test.foreign',
        [int]$TimeoutSeconds = 30
    )

    Import-CamWindowApi
    $hwnd = [IntPtr]::new([int64]$Window.Hwnd)
    $pidFile = Join-Path -Path $env:TEMP -ChildPath ('cam-tests\foreign-' + $Window.Title + '.pid')
    Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue

    $script = "`$host.ui.RawUI.WindowTitle = '{0}'`n`"`$PID`" | Out-File -FilePath '{1}' -Encoding ascii`nwhile (`$true) {{ Start-Sleep -Seconds 5 }}" -f `
        $ForeignTitle.Replace("'", "''"), $pidFile.Replace("'", "''")
    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($script))

    # No --suppressApplicationTitle: this tab is standing in for a foreign
    # console, and a foreign console titles itself.
    & wt.exe @('-w', $Window.WtName, 'new-tab', 'powershell.exe', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-EncodedCommand', $encoded)

    # Waits for the tab's OWN title, not merely for the title to change. The
    # window reads 'powershell.exe' the instant the tab appears and before the
    # script in it has run, so a wait on "anything but ours" returns that and
    # races the pid file - which then never gets read, leaving the foreign
    # console with nothing to clean it up by if it outlives the window.
    $now = ''
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $now = [CamWin]::TitleOf($hwnd)
        if ($now -ceq $ForeignTitle -and (Test-Path -LiteralPath $pidFile)) { break }
        Start-Sleep -Milliseconds 500
    }
    if (Test-Path -LiteralPath $pidFile) {
        try { $Window.ForeignPid = [int](Get-Content -LiteralPath $pidFile -Raw).Trim() } catch { }
    }
    if ($now -ceq $Window.Title) { return '' }
    return $now
}


function Close-CamTestWindow {
    <#
        Closes the window and touches no process, which is what clicking the X
        does - so the warden gets to see it go and run its own shutdown. A
        test that needs to observe what the warden DOES on a close has to use
        this and not Stop-CamTestWardenWindow, which force-kills the warden
        and would leave it no chance to run anything.

        The close goes by TITLE first and then by the recorded HANDLE, because
        a test that added a foreign tab has left the window unfindable by
        title - and a window with two tabs raises the close-all-tabs overlay,
        which only a second WM_CLOSE answers. Both are deliberate conditions
        here, so the close has to survive them or a test leaves a window on
        the Operator's desktop.

        Never Stop-Process the terminal: one Windows Terminal process owns
        every terminal window on the desktop, including the Operator's.

        Returns $true when the window is gone.
    #>
    param([Parameter(Mandatory = $true)]$Window)

    Import-CamWindowApi
    [void](Stop-CamTerminalWindow -Title $Window.Title)

    if (-not $Window.Hwnd -or [int64]$Window.Hwnd -eq 0) { return $true }

    $hwnd = [IntPtr]::new([int64]$Window.Hwnd)
    $attempt = 0
    while ($attempt -lt 4 -and [CamWin]::IsTerminalWindow($hwnd)) {
        $attempt++
        [void][CamWin]::PostMessage($hwnd, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero)
        Start-Sleep -Milliseconds 750
    }
    return (-not [CamWin]::IsTerminalWindow($hwnd))
}


function Stop-CamTestWardenWindow {
    <#
        Takes down whatever Start-CamTestWardenWindow left: the window by
        WM_CLOSE, then the warden, the pane and any foreign tab by pid if they
        outlived it.

        The pid sweep is a backstop, not the method, and it is why a test that
        wants to watch the warden react must close the window with
        Close-CamTestWindow first and wait for the warden to exit on its own.
    #>
    param([Parameter(Mandatory = $true)]$Window)

    [void](Close-CamTestWindow -Window $Window)

    foreach ($id in @($Window.WardenPid, $Window.PanePid, $Window.ForeignPid)) {
        if ([int]$id -le 0) { continue }
        try { if (Get-Process -Id $id -ErrorAction SilentlyContinue) { Stop-Process -Id $id -Force } } catch { }
    }
}


# ---------------------------------------------------------------------------
# Pruner: recorder functions in-process, and a filesystem you can diff
# ---------------------------------------------------------------------------

function Get-CamTestRecorderFunctions {
    <#
        The named functions of cam.recorder.ps1 as one scriptblock, for the
        caller to dot-source into its own scope:

            . (Get-CamTestRecorderFunctions -Name 'Invoke-CamPrune', 'Write-CamHost')

        Taken from the parsed file rather than by dot-sourcing the script,
        which would run its entry point. Named, never all: the recorder has
        functions that share a name with this harness's own (Add-CamTestResult)
        and would shadow them. Dot-sourced into a check's scope, a function the
        check defines there - Get-CamFreeSpaceGB, Send-CamAlert - replaces the
        product's for every call the loaded functions make, because PowerShell
        resolves commands through the caller's scopes.
    #>
    param([Parameter(Mandatory = $true)][string[]]$Name)

    $path = Join-Path -Path (Get-CamTestRepo) -ChildPath 'cam.recorder.ps1'
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$null, [ref]$null)
    $found = @{}
    foreach ($definition in $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
        $found[$definition.Name] = $definition.Extent.Text
    }

    $text = New-Object System.Text.StringBuilder
    foreach ($wanted in $Name) {
        if (-not $found.ContainsKey($wanted)) { throw "cam.recorder.ps1 defines no function '$wanted'." }
        [void]$text.AppendLine($found[$wanted])
    }
    return [scriptblock]::Create($text.ToString())
}


function Set-CamTestFileAge {
    <# Backdates a file's write AND creation time; the pruner reads both. #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][DateTime]$Time
    )
    $item = Get-Item -LiteralPath $Path -Force
    $item.CreationTime = $Time
    $item.LastWriteTime = $Time
}


function New-CamTestFileReparsePoint {
    <#
        Makes $Path a FILE reparse point. A real symbolic link to $Target when
        this account may create one (elevated, or Developer Mode), otherwise a
        reparse point with a private non-Microsoft tag, which needs no
        privilege. Both carry FILE_ATTRIBUTE_REPARSE_POINT, which is what the
        pruner refuses on; the second simply cannot be opened by anybody.

        Returns which kind was made, for the evidence.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Target,
        # Applied to the stand-in before it is tagged, after which nothing can
        # open it to set a time. A symbolic link reports its own times.
        [DateTime]$Time = [DateTime]::Now
    )

    try {
        [void](New-Item -ItemType SymbolicLink -Path $Path -Target $Target -ErrorAction Stop)
        return 'symbolic link'
    }
    catch { }

    if (-not ('CamTestReparse' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;
public static class CamTestReparse {
    [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
    static extern SafeFileHandle CreateFileW(string name, uint access, uint share, IntPtr security, uint disposition, uint flags, IntPtr template);
    [DllImport("kernel32.dll", SetLastError = true)]
    static extern bool DeviceIoControl(SafeFileHandle handle, uint code, byte[] input, int inputLength, IntPtr output, int outputLength, out int returned, IntPtr overlapped);
    // FSCTL_SET_REPARSE_POINT with a REPARSE_GUID_DATA_BUFFER: any tag
    // without the Microsoft bit needs only write access to the file.
    public static void Set(string path) {
        using (SafeFileHandle handle = CreateFileW(path, 0x40000000, 0, IntPtr.Zero, 3, 0x00200000 | 0x02000000, IntPtr.Zero)) {
            if (handle.IsInvalid) throw new System.ComponentModel.Win32Exception();
            byte[] data = new byte[] { 0x43, 0x41, 0x4D, 0x54 };
            byte[] buffer = new byte[24 + data.Length];
            BitConverter.GetBytes((uint)0x00004341).CopyTo(buffer, 0);
            BitConverter.GetBytes((ushort)data.Length).CopyTo(buffer, 4);
            new Guid("6a0f3c1e-2b7d-4c55-9f3e-1d2c3b4a5f60").ToByteArray().CopyTo(buffer, 8);
            data.CopyTo(buffer, 24);
            int returned;
            if (!DeviceIoControl(handle, 0x000900A4, buffer, buffer.Length, IntPtr.Zero, 0, out returned, IntPtr.Zero)) {
                throw new System.ComponentModel.Win32Exception();
            }
        }
    }
}
'@
    }
    [System.IO.File]::WriteAllText($Path, 'reparse stand-in')
    Set-CamTestFileAge -Path $Path -Time $Time
    [CamTestReparse]::Set($Path)
    return 'private-tag reparse point (this account may not create symbolic links)'
}


function Get-CamTestFileManifest {
    <#
        Every file under $Path, keyed by full path, with a sha256 of its
        content. Never descends through a reparse point - that is the bug this
        harness is looking for - and records the reparse point itself by its
        attributes, size and times instead of a hash, since some cannot be
        opened at all.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        # Size, attributes and write time only, no file opened. For the live
        # archive, which is read-only to every test and far too big to hash.
        [switch]$MetadataOnly
    )

    $manifest = @{}
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $pending = New-Object System.Collections.Stack
        $pending.Push((Get-Item -LiteralPath $Path -Force))
        while ($pending.Count -gt 0) {
            $dir = $pending.Pop()
            foreach ($item in @(Get-ChildItem -LiteralPath $dir.FullName -Force)) {
                $reparse = (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
                if ($reparse) {
                    $manifest[$item.FullName] = "reparse|{0}|{1}" -f $item.Attributes, $item.LastWriteTimeUtc.Ticks
                    continue
                }
                if ($item.PSIsContainer) { $pending.Push($item); continue }
                if ($MetadataOnly) {
                    $manifest[$item.FullName] = "{0}|{1}|{2}" -f $item.Length, $item.Attributes, $item.LastWriteTimeUtc.Ticks
                    continue
                }
                $stream = [System.IO.File]::Open($item.FullName, 'Open', 'Read', 'ReadWrite')
                try { $hash = [System.BitConverter]::ToString($sha.ComputeHash($stream)) }
                finally { $stream.Dispose() }
                $manifest[$item.FullName] = "{0}|{1}|{2}" -f $hash, $item.Length, $item.Attributes
            }
        }
    }
    finally { $sha.Dispose() }
    return $manifest
}


function Compare-CamTestFileManifest {
    <# What changed between two manifests: Removed, Added and Changed paths. #>
    param(
        [Parameter(Mandatory = $true)][hashtable]$Before,
        [Parameter(Mandatory = $true)][hashtable]$After
    )

    $diff = [pscustomobject]@{ Removed = @(); Added = @(); Changed = @() }
    foreach ($key in $Before.Keys) {
        if (-not $After.ContainsKey($key)) { $diff.Removed += $key }
        elseif ($After[$key] -ne $Before[$key]) { $diff.Changed += $key }
    }
    foreach ($key in $After.Keys) {
        if (-not $Before.ContainsKey($key)) { $diff.Added += $key }
    }
    $diff.Removed = @($diff.Removed | Sort-Object)
    $diff.Added = @($diff.Added | Sort-Object)
    $diff.Changed = @($diff.Changed | Sort-Object)
    return $diff
}


function Remove-CamTestTree {
    <#
        Deletes a test directory without ever following a reparse point out of
        it: every junction and link inside is unlinked first, then the tree.
        The test trees under cam-tests hold junctions to bait directories on
        purpose.
    #>
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) { return }
    $pending = New-Object System.Collections.Stack
    $pending.Push((Get-Item -LiteralPath $Path -Force))
    while ($pending.Count -gt 0) {
        $dir = $pending.Pop()
        foreach ($item in @(Get-ChildItem -LiteralPath $dir.FullName -Force)) {
            if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                if ($item.PSIsContainer) { [System.IO.Directory]::Delete($item.FullName, $false) }
                else { [System.IO.File]::Delete($item.FullName) }
            }
            elseif ($item.PSIsContainer) { $pending.Push($item) }
        }
    }
    Remove-Item -LiteralPath $Path -Recurse -Force
}


function New-CamTestPruneRoot {
    <#
        An empty archive root at %TEMP%\cam-tests\<Name>\archive with a config
        pointing at it: both rules off, so a test switches on exactly the rule
        it is about. The directory is cleared first, junctions unlinked rather
        than followed. Log is the supervisor log the pruner writes to, kept
        outside the directory so it never shows up in a manifest of it.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [hashtable]$Config = @{}
    )

    $dir = Join-Path -Path $env:TEMP -ChildPath ("cam-tests\" + $Name)
    Remove-CamTestTree -Path $dir
    $root = Join-Path -Path $dir -ChildPath 'archive'
    [void](New-Item -ItemType Directory -Path $root -Force)
    $log = Join-Path -Path $env:TEMP -ChildPath ("cam-tests\" + $Name + '.log')
    if (Test-Path -LiteralPath $log) { Remove-Item -LiteralPath $log -Force }

    $settings = Get-CamConfigDefault
    $settings.RootPath = $root
    $settings.ReserveGB = 0
    $settings.PruneHysteresisGB = 0
    $settings.MaxRetentionDays = $null
    foreach ($key in $Config.Keys) { $settings[$key] = $Config[$key] }

    return [pscustomobject]@{ Dir = $dir; Root = $root; Config = $settings; Log = $log }
}


function New-CamTestSegment {
    <#
        Writes a file named the way the recorder names a segment,
        <camera>_<yyyyMMdd>_<HHmmss><+hhmm><extension>, backdated to $Time.

        The name is spelled out here from the naming scheme on purpose, NOT
        produced by the product's own pattern code: a test built on it then
        checks that code instead of repeating it. The content embeds the name
        so every file hashes differently. Returns the full path.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Dir,
        [Parameter(Mandatory = $true)][string]$Camera,
        [Parameter(Mandatory = $true)][DateTime]$Time,
        [int]$Bytes = 1024,
        [string]$Extension = '.mkv'
    )

    $offset = [TimeZoneInfo]::Local.GetUtcOffset($Time)
    $sign = $(if ($offset -lt [TimeSpan]::Zero) { '-' } else { '+' })
    $zone = '{0}{1:00}{2:00}' -f $sign, [math]::Abs($offset.Hours), [math]::Abs($offset.Minutes)
    $name = '{0}_{1}{2}{3}' -f $Camera, $Time.ToString('yyyyMMdd_HHmmss', [System.Globalization.CultureInfo]::InvariantCulture), $zone, $Extension

    if (-not (Test-Path -LiteralPath $Dir)) { [void](New-Item -ItemType Directory -Path $Dir -Force) }
    $path = Join-Path -Path $Dir -ChildPath $name
    $content = New-Object byte[] $Bytes
    $stamp = [System.Text.Encoding]::ASCII.GetBytes($path)
    for ($i = 0; $i -lt $Bytes; $i++) { $content[$i] = $stamp[$i % $stamp.Length] }
    [System.IO.File]::WriteAllBytes($path, $content)
    Set-CamTestFileAge -Path $path -Time $Time
    return $path
}
