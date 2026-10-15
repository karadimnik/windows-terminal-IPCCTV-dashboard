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
        [int]$TimeoutSeconds = 300,
        # Variables the child sees in place of this process's, such as a
        # sandbox LOCALAPPDATA. Restored here once the child has started.
        [hashtable]$Environment = @{}
    )

    $outFile = Join-Path -Path $env:TEMP -ChildPath ('cam-test-out-' + [Guid]::NewGuid().ToString('n').Substring(0, 8) + '.txt')
    $errFile = "$outFile.err"
    $argumentList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $Path) + $Arguments

    $saved = @{}
    foreach ($key in $Environment.Keys) {
        $saved[$key] = [Environment]::GetEnvironmentVariable($key)
        [Environment]::SetEnvironmentVariable($key, $Environment[$key])
    }
    try {
        $process = Start-Process -FilePath 'powershell.exe' -ArgumentList $argumentList -PassThru `
            -WindowStyle Hidden -RedirectStandardOutput $outFile -RedirectStandardError $errFile
    }
    finally {
        foreach ($key in $saved.Keys) { [Environment]::SetEnvironmentVariable($key, $saved[$key]) }
    }
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
# A synthetic camera, and a sandbox that cannot raise a real alert
# ---------------------------------------------------------------------------

function Test-CamTestPortFree {
    <# True when nothing on this machine listens on TCP $Port, on any address. #>
    param([Parameter(Mandatory = $true)][int]$Port)

    $listeners = [System.Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties().GetActiveTcpListeners()
    return (@($listeners | Where-Object { $_.Port -eq $Port }).Count -eq 0)
}


function Start-CamTestSyntheticCamera {
    <#
        A stand-in for an IP camera, so the real supervisor can record without
        any camera being contacted: a sandbox MediaMTX on loopback:$Port, with
        one ffmpeg publisher per path pushing a lavfi test pattern as H.264 and
        a sine tone as G.711 A-law - the two codecs these cameras send.

        MediaMTX rather than ffmpeg as its own RTSP listener, because a
        listening ffmpeg serves exactly one client and exits when it leaves:
        the checks that kill the recorder's child need the source to accept
        the reconnect, as a camera does.

        Everything but RTSP over TCP is switched off, so no default port of the
        Operator's relay (8554, 9997, 8000/8001 UDP, 1935, 8888...) is opened.
        The publishers use a 1 s GOP so a -c copy segment can cut within a
        second of its boundary. Returns the processes for
        Stop-CamTestSyntheticCamera, which kills exactly these pids.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Dir,
        [Parameter(Mandatory = $true)][int]$Port,
        [Parameter(Mandatory = $true)][string[]]$PathName,
        # Repeats every fifth video timestamp, which makes the recorder's
        # ffmpeg warn "Non-monotonic DTS" the way these cameras make it warn,
        # so the per-camera log has something to grow on.
        [switch]$NonMonotonic,
        # With -NonMonotonic, every Nth video frame repeats the previous
        # timestamp; and the publisher's frame rate. Together they set how fast
        # the recorder's ffmpeg warns, so a soak can fill a 2 MB camera log.
        [ValidateRange(2, 100)][int]$NonMonotonicEvery = 5,
        [ValidateRange(5, 120)][int]$FrameRate = 15,
        # With both, reading needs this user and password (a wrong or absent
        # one is refused); publishing stays open on loopback so the
        # publishers need none. Fake credentials only.
        [string]$ReadUser,
        [string]$ReadPass,
        [int]$TimeoutSeconds = 20
    )

    if (-not (Test-CamTestPortFree -Port $Port)) { throw "TCP port $Port is already in use; the synthetic camera needs it free." }

    $work = Join-Path -Path $Dir -ChildPath 'synthcam'
    [void](New-Item -ItemType Directory -Path $work -Force)
    $config = Join-Path -Path $work -ChildPath 'mediamtx.yml'
    $log = Join-Path -Path $work -ChildPath 'mediamtx.out'
    $yml = @(
        'logLevel: info', 'logDestinations: [stdout]', 'api: no', 'metrics: no', 'pprof: no', 'playback: no',
        'rtsp: yes', 'rtspTransports: [tcp]', "rtspAddress: 127.0.0.1:$Port",
        'rtmp: no', 'hls: no', 'webrtc: no', 'srt: no', 'moq: no'
    )
    if ($ReadUser -and $ReadPass) {
        $yml += @('authMethod: internal', 'authInternalUsers:',
            '  - user: any', "    pass:", "    ips: ['127.0.0.1']", '    permissions:', '      - action: publish',
            "  - user: $ReadUser", "    pass: $ReadPass", '    ips: []', '    permissions:', '      - action: read')
    }
    $yml = $yml + @('paths:') + @($PathName | ForEach-Object { "  ${_}: {}" })
    [System.IO.File]::WriteAllText($config, (($yml -join "`n") + "`n"), (New-Object System.Text.UTF8Encoding($false)))

    $camera = [pscustomobject]@{
        Port       = $Port
        Dir        = $work
        Log        = $log
        MediaMtx   = $null
        Publishers = @()
    }

    try {
        # A local working directory: started from the repository share MediaMTX
        # writes files there, and it must never depend on the caller's folder.
        $camera.MediaMtx = Start-Process -FilePath 'C:\mediamtx\mediamtx.exe' -ArgumentList @("`"$config`"") -PassThru -WindowStyle Hidden -WorkingDirectory $work `
            -RedirectStandardOutput $log -RedirectStandardError "$log.err"
        [void]$camera.MediaMtx.Handle

        $ffmpeg = (Get-CamConfigDefault).FfmpegPath
        foreach ($name in $PathName) {
            $publish = @('-hide_banner', '-loglevel', 'error', '-nostdin',
                '-re', '-f', 'lavfi', '-i', "testsrc2=size=640x360:rate=$FrameRate",
                '-re', '-f', 'lavfi', '-i', 'sine=frequency=440:sample_rate=8000',
                '-map', '0:v', '-map', '1:a',
                '-c:v', 'libx264', '-preset', 'ultrafast', '-tune', 'zerolatency', '-pix_fmt', 'yuv420p',
                '-g', "$FrameRate", '-keyint_min', "$FrameRate", '-sc_threshold', '0',
                '-c:a', 'pcm_alaw', '-ar', '8000', '-ac', '1')
            if ($NonMonotonic) { $publish += '-bsf:v', ('setts=ts=if(eq(mod(N\,{0})\,{1})\,PREV_OUTPTS\,PTS)' -f $NonMonotonicEvery, ($NonMonotonicEvery - 1)) }
            $publish += @('-f', 'rtsp', '-rtsp_transport', 'tcp', "rtsp://127.0.0.1:$Port/$name")
            $publisherLog = Join-Path -Path $work -ChildPath "publish-$name.err"
            # Retried until MediaMTX is listening: a publisher that connects
            # first exits at once, and that is the only race here.
            $publisher = $null
            $deadline = [DateTime]::Now.AddSeconds($TimeoutSeconds)
            while ($true) {
                $online = @(Get-CamFileTailLines -Path $log -Count 200 | Where-Object { $_ -like "*listeners on 127.0.0.1:$Port*" }).Count -gt 0
                if ($online) {
                    $publisher = Start-Process -FilePath $ffmpeg -ArgumentList (Get-CamCommandLine -Arguments $publish) -PassThru -WindowStyle Hidden `
                        -RedirectStandardOutput "$publisherLog.out" -RedirectStandardError $publisherLog
                    [void]$publisher.Handle
                    break
                }
                if ($camera.MediaMtx.HasExited) { throw "The synthetic camera's MediaMTX exited with code $($camera.MediaMtx.ExitCode): $log" }
                if ([DateTime]::Now -ge $deadline) { throw "The synthetic camera's MediaMTX did not listen on $Port within ${TimeoutSeconds}s: $log" }
                Start-Sleep -Milliseconds 250
            }
            $camera.Publishers += $publisher
        }

        $deadline = [DateTime]::Now.AddSeconds($TimeoutSeconds)
        while ($true) {
            $lines = @(Get-CamFileTailLines -Path $log -Count 400)
            $waiting = @($PathName | Where-Object { $n = $_; @($lines | Where-Object { $_.Contains("[path $n] stream is available and online") }).Count -eq 0 })
            if ($waiting.Count -eq 0) { break }
            foreach ($publisher in $camera.Publishers) {
                if ($publisher.HasExited) { throw "A synthetic camera publisher exited with code $($publisher.ExitCode); see $work" }
            }
            if ([DateTime]::Now -ge $deadline) { throw "The synthetic camera did not serve $($waiting -join ', ') within ${TimeoutSeconds}s: $log" }
            Start-Sleep -Milliseconds 250
        }
    }
    catch {
        [void](Stop-CamTestSyntheticCamera -Camera $camera)
        throw
    }

    return $camera
}


function Stop-CamTestSyntheticCamera {
    <#
        Kills the synthetic camera's own processes by pid - publishers first,
        then MediaMTX - and never anything by image name: ffmpeg and mediamtx
        on this machine may be the Operator's. Returns $true when every one of
        them has exited.
    #>
    param([Parameter(Mandatory = $true)]$Camera)

    $all = @(@($Camera.Publishers) + @($Camera.MediaMtx) | Where-Object { $_ })
    foreach ($process in $all) {
        if (-not $process.HasExited) {
            try { $process.Kill() } catch { }
            [void]$process.WaitForExit(10000)
        }
    }
    return (@($all | Where-Object { -not $_.HasExited }).Count -eq 0)
}


function Set-CamTestAlertStub {
    <#
        Replaces Send-CamAlert in the SANDBOX copy of cam.common.ps1 with one
        that appends to <sandbox>\alerts.log and writes the product's ALERT
        line to the log it was given, so a supervisor run in the sandbox can
        never raise a toast or write the Operator's event log. Appended last,
        so it is the definition the dot-sourced library ends up with.
    #>
    param([Parameter(Mandatory = $true)]$Sandbox)

    $stub = @'


# --- test sandbox only: alerts are recorded, never shown ---
function Send-CamAlert {
    param(
        [Parameter(Mandatory = $true)][string]$Title,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Message,
        [int]$EventId = 100,
        [string]$EntryType = 'Warning',
        [AllowEmptyString()][string]$LogPath = '',
        [int]$LogMaxBytes = 0
    )
    $title = Get-CamRedacted -Text $Title
    $body = Get-CamRedacted -Text $Message
    $channels = 'recorded by the test sandbox stub, not shown'
    [System.IO.File]::AppendAllText((Join-Path -Path $script:CamLibRoot -ChildPath 'alerts.log'),
        ("{0} {1} {2} - {3}" -f $EventId, $EntryType, $title, $body) + [Environment]::NewLine)
    if (-not [string]::IsNullOrEmpty($LogPath)) {
        try { Write-CamLog -Path $LogPath -Message ("ALERT {0} - {1} [{2}]" -f $title, $body, $channels) -MaxBytes $LogMaxBytes }
        catch { }
    }
    return @{ Title = $title; Message = $body; Channels = $channels }
}
'@
    $common = Join-Path -Path $Sandbox.Dir -ChildPath 'cam.common.ps1'
    [System.IO.File]::AppendAllText($common, $stub, (New-Object System.Text.UTF8Encoding($false)))
    return (Join-Path -Path $Sandbox.Dir -ChildPath 'alerts.log')
}


function Get-CamTestCadenceLimit {
    <#
        The longest gap a runtime check accepts between two runs of a
        supervisor timer that fires every $IntervalSeconds: 3 x interval +
        1.5 s. A timer fires on the first 1 s loop tick past its interval, and
        a tick can be held by the work before it - directory scans, a child
        start, a prune run, a slow disk under a loaded harness - so a healthy
        1 s status timer was measured at 1.1-1.6 s and once at 3.7 s, which
        failed the fixed 3 s limit. 3 x interval absorbs two late ticks, the
        1.5 s a loaded host. A real stall is not near it: a frozen status
        timer reads tens of seconds (30.9 s in the [86] red run), and a
        watchdog-killed or dead loop never writes again.
    #>
    param([Parameter(Mandatory = $true)][double]$IntervalSeconds)
    return (3 * $IntervalSeconds + 1.5)
}


function Find-CamClockLintProblems {
    <#
        [86] Lines of product PowerShell that read the LOCAL clock or a LOCAL
        file time outside the display forms. Interval, age and deadline
        arithmetic must use Get-CamWallClock (UTC) and LastWriteTimeUtc /
        CreationTimeUtc, because DateTime subtraction ignores the offset and
        a daylight-saving change would move it by an hour.

        Allowed without comment - they can only produce text:
            Get-Date -Format ...      [DateTime]::Now.ToString(...)
            $x.LastWriteTime.ToString(...)   $x.CreationTime.ToString(...)
        Anything else needs "# clock: display" on the same line, saying why
        it is display only.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyCollection()][AllowEmptyString()][string[]]$Lines, [string]$Label = '')

    $problems = @()
    $read = '\[DateTime\]::(Now|Today)\b|\bGet-Date\b|\.(LastWriteTime|CreationTime|LastAccessTime)\b'
    $display = '\bGet-Date\s+-Format\b|\[DateTime\]::Now\.ToString\(|\.(LastWriteTime|CreationTime|LastAccessTime)\.ToString\(|#\s*clock:\s*display'
    for ($i = 0; $i -lt $Lines.Count; $i++) {
        $line = $Lines[$i]
        if ($line -match $read -and $line -notmatch $display) { $problems += ("{0}:{1}: {2}" -f $Label, ($i + 1), $line.Trim()) }
    }
    return , $problems
}


function Set-CamTestSandboxClock {
    <#
        Rewires every clock reading of the SANDBOX copies of cam.common.ps1 and
        cam.recorder.ps1 so a test can move time without touching the machine
        clock or time zone. Nothing happens until Set-CamTestClockChange names
        the instant; from then on readings of instants at or after it shift by
        its delta:

          -Mode local  a daylight-saving change: local readings shift
                       ([DateTime]::Now, (Get-Date), LastWriteTime,
                       CreationTime); UTC readings do not, as in a real one.
          -Mode utc    a step of the clock itself: the seam
                       Get-CamWallClock and the UTC file times shift too, the
                       way a file stamped after a real step would read.

        Readings are rewritten in place - [DateTime]::Now and (Get-Date) become
        Get-CamSimLocal of UtcNow, $x.LastWriteTime(Utc) and
        $x.CreationTime(Utc) become Get-CamSimLocal / Get-CamSimUtc of the UTC
        time - and the simulator is appended last, in utc mode with its own
        Get-CamWallClock.
        Works on any version of the scripts, which is what lets the same run
        show the pre-[86] product failing.
    #>
    param(
        [Parameter(Mandatory = $true)]$Sandbox,
        [Parameter(Mandatory = $true)][ValidateSet('local', 'utc')][string]$Mode
    )

    $now = '(Get-CamSimLocal -Utc ([DateTime]::UtcNow))'
    foreach ($file in @('cam.common.ps1', 'cam.recorder.ps1')) {
        $path = Join-Path -Path $Sandbox.Dir -ChildPath $file
        $text = [System.IO.File]::ReadAllText($path)
        $text = [regex]::Replace($text, '\[DateTime\]::Now\b', $now)
        $text = [regex]::Replace($text, '\(Get-Date\)', $now)
        $text = [regex]::Replace($text, '(\$\w+(?:\.\w+)*)\.(LastWriteTime|CreationTime)Utc\b', '(Get-CamSimUtc -Utc ${1}.${2}Utc)')
        $text = [regex]::Replace($text, '(\$\w+(?:\.\w+)*)\.(LastWriteTime|CreationTime)\b(?!Utc)', '(Get-CamSimLocal -Utc ${1}.${2}Utc)')
        [System.IO.File]::WriteAllText($path, $text, (New-Object System.Text.UTF8Encoding($false)))
    }

    $simulator = @'


# --- test sandbox only: a simulated clock change, see Set-CamTestSandboxClock ---
$script:CamSimMode = '{MODE}'
$script:CamSimAt = $null
$script:CamSimDelta = 0.0
function Get-CamSimShift {
    param([DateTime]$Utc)
    if ($null -eq $script:CamSimAt) {
        $file = Join-Path -Path $script:CamLibRoot -ChildPath 'clock.change'
        if (-not (Test-Path -LiteralPath $file)) { return 0.0 }
        $parts = ([System.IO.File]::ReadAllText($file)).Trim() -split ' '
        $script:CamSimAt = [DateTime]::new([int64]$parts[0], [DateTimeKind]::Utc)
        $script:CamSimDelta = [double]$parts[1]
    }
    if ($Utc -ge $script:CamSimAt) { return $script:CamSimDelta }
    return 0.0
}
function Get-CamSimUtc {
    param([DateTime]$Utc)
    if ($script:CamSimMode -eq 'utc') { return $Utc.AddSeconds((Get-CamSimShift -Utc $Utc)) }
    return $Utc
}
function Get-CamSimLocal {
    param([DateTime]$Utc)
    if ($script:CamSimMode -eq 'utc') { return $Utc.AddSeconds((Get-CamSimShift -Utc $Utc)).ToLocalTime() }
    return $Utc.ToLocalTime().AddSeconds((Get-CamSimShift -Utc $Utc))
}
'@
    # A step moves the seam; a daylight-saving change leaves the product's
    # own Get-CamWallClock alone, rewritten above like any other reading.
    if ($Mode -eq 'utc') { $simulator += "`r`nfunction Get-CamWallClock { return (Get-CamSimUtc -Utc ([DateTime]::UtcNow)) }`r`n" }
    $common = Join-Path -Path $Sandbox.Dir -ChildPath 'cam.common.ps1'
    [System.IO.File]::AppendAllText($common, $simulator.Replace('{MODE}', $Mode), (New-Object System.Text.UTF8Encoding($false)))
}


function Set-CamTestClockChange {
    <# Schedules the simulated change of Set-CamTestSandboxClock: at $AtUtc, by $DeltaSeconds. #>
    param(
        [Parameter(Mandatory = $true)]$Sandbox,
        [Parameter(Mandatory = $true)][DateTime]$AtUtc,
        [Parameter(Mandatory = $true)][double]$DeltaSeconds
    )

    $path = Join-Path -Path $Sandbox.Dir -ChildPath 'clock.change'
    $temp = "$path.tmp"
    [System.IO.File]::WriteAllText($temp, ("{0} {1}" -f $AtUtc.ToUniversalTime().Ticks, $DeltaSeconds))
    Move-Item -LiteralPath $temp -Destination $path -Force
}


function New-CamTestSynthSandbox {
    <#
        A sandbox whose cameras are synthetic: each camera's IP is
        127.0.0.1:$Port and its stream path is its own name, so the product
        builds an rtsp:// URL to the synthetic camera through Get-CamRtspUrl
        and Get-CamFfmpegArgs, unmodified. Throwaway credential, relay off,
        Send-CamAlert stubbed.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string[]]$CameraName,
        [Parameter(Mandatory = $true)][int]$Port,
        [hashtable]$Config = @{}
    )

    $cameras = @($CameraName | ForEach-Object { [pscustomobject]@{ Name = $_; IP = "127.0.0.1:$Port"; Quality = $_; Record = $true } })
    $sandbox = New-CamTestSandbox -Name $Name -Cameras $cameras -Config $Config
    $alerts = Set-CamTestAlertStub -Sandbox $sandbox
    $sandbox | Add-Member -NotePropertyName AlertLog -NotePropertyValue $alerts
    $sandbox | Add-Member -NotePropertyName SupervisorLog -NotePropertyValue (Join-Path -Path $sandbox.Root -ChildPath '_logs\recorder.log')
    return $sandbox
}


function Get-CamTestChildPid {
    <#
        The pid status.json reports for one camera's recorder child, or 0.
        Only a pid that is alive, is ffmpeg.exe and is a CHILD of this sandbox
        supervisor is returned, so a caller that kills it can never hit an
        ffmpeg of the Operator's.
    #>
    param(
        [Parameter(Mandatory = $true)]$Sandbox,
        [Parameter(Mandatory = $true)]$SupervisorProcess,
        [Parameter(Mandatory = $true)][string]$CameraName
    )

    $status = Get-CamTestStatusJson -Sandbox $Sandbox
    if (-not $status) { return 0 }
    $childPid = 0
    foreach ($row in @($status.Snapshot.Cameras)) { if ($row.Name -eq $CameraName -and $row.Pid) { $childPid = [int]$row.Pid } }
    if ($childPid -le 0) { return 0 }
    $process = Get-CimInstance -ClassName Win32_Process -Filter "ProcessId = $childPid" -ErrorAction SilentlyContinue
    if (-not $process -or $process.Name -ne 'ffmpeg.exe' -or [int]$process.ParentProcessId -ne $SupervisorProcess.Id) { return 0 }
    return $childPid
}


function Wait-CamTestChildPid {
    <# Waits for a recorder child other than -Not to be reported; returns its pid, or 0 on timeout. #>
    param(
        [Parameter(Mandatory = $true)]$Sandbox,
        [Parameter(Mandatory = $true)]$SupervisorProcess,
        [Parameter(Mandatory = $true)][string]$CameraName,
        [int]$Not = 0,
        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::Now.AddSeconds($TimeoutSeconds)
    while ([DateTime]::Now -lt $deadline) {
        $childPid = Get-CamTestChildPid -Sandbox $Sandbox -SupervisorProcess $SupervisorProcess -CameraName $CameraName
        if ($childPid -gt 0 -and $childPid -ne $Not) { return $childPid }
        Start-Sleep -Milliseconds 200
    }
    return 0
}


function Stop-CamTestChildHard {
    <#
        Hard-kills one recorder child by pid, the way a crash ends it, and
        waits for it to exit. The pid must come from Get-CamTestChildPid.
        Returns $true once it has gone.
    #>
    param([Parameter(Mandatory = $true)][int]$ProcessId)

    $process = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
    if (-not $process) { return $true }
    try { $process.Kill() } catch { }
    return $process.WaitForExit(10000)
}


function Get-CamTestLiveLength {
    <#
        A file's true length, read through a handle. The directory entry of a
        file another process holds open is not refreshed on NTFS, so
        FileInfo.Length can say 0 for a segment that already has data.
        -1 when the file is gone.
    #>
    param([Parameter(Mandatory = $true)][string]$Path)
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read,
            ([System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete))
        try { return $stream.Length } finally { $stream.Dispose() }
    }
    catch { return -1 }
}


function Get-CamTestSegmentProbe {
    <#
        ffprobe's view of one segment: the codec of every stream, the format
        start time and duration. Duration is $null for a file whose writer
        was killed, because a matroska file stopped mid-write has no duration
        element.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [string]$FfprobePath = (Get-CamConfigDefault).FfprobePath
    )

    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $json = (& $FfprobePath -v error -show_entries 'format=duration,start_time:stream=codec_name' -of json $Path 2>&1) -join "`n" }
    finally { $ErrorActionPreference = $previous }
    $codecs = @()
    $start = $null
    $duration = $null
    try {
        $parsed = ConvertFrom-Json -InputObject $json
        foreach ($stream in @($parsed.streams)) { $codecs += [string]$stream.codec_name }
        $value = 0.0
        if ([double]::TryParse([string]$parsed.format.start_time, [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$value)) { $start = $value }
        if ([double]::TryParse([string]$parsed.format.duration, [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$value)) { $duration = $value }
    }
    catch { }
    return [pscustomobject]@{ Codecs = $codecs; Start = $start; Duration = $duration; Raw = $json }
}

function Suspend-CamTestProcess {
    <#
        Freezes every thread of one process (NtSuspendProcess), the way a
        child that keeps its session open but moves no data looks from
        outside: alive, holding its files, writing nothing. By pid only, and
        the pid must come from Get-CamTestChildPid so it is the sandbox's own
        ffmpeg. Every caller must Resume-CamTestProcess in a finally.
    #>
    param([Parameter(Mandatory = $true)][int]$ProcessId)

    Add-CamTestNtProcessType
    $process = Get-Process -Id $ProcessId
    $status = [CamTest.NtProcess]::NtSuspendProcess($process.Handle)
    if ($status -ne 0) { throw ("NtSuspendProcess({0}) returned 0x{1:X8}" -f $ProcessId, $status) }
}


function Resume-CamTestProcess {
    <#
        Undoes Suspend-CamTestProcess. Returns $true when the process is gone
        or no thread of it is left suspended.
    #>
    param([Parameter(Mandatory = $true)][int]$ProcessId)

    Add-CamTestNtProcessType
    $process = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
    if (-not $process) { return $true }
    [void][CamTest.NtProcess]::NtResumeProcess($process.Handle)
    return (-not (Test-CamTestProcessSuspended -ProcessId $ProcessId))
}


function Test-CamTestProcessSuspended {
    <# True when the process exists and any of its threads is suspended. #>
    param([Parameter(Mandatory = $true)][int]$ProcessId)

    $process = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
    if (-not $process) { return $false }
    $process.Refresh()
    foreach ($thread in $process.Threads) {
        if ($thread.ThreadState -eq 'Wait' -and $thread.WaitReason -eq 'Suspended') { return $true }
    }
    return $false
}


function Add-CamTestNtProcessType {
    if ('CamTest.NtProcess' -as [type]) { return }
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace CamTest {
    public static class NtProcess {
        [DllImport("ntdll.dll")] public static extern int NtSuspendProcess(IntPtr processHandle);
        [DllImport("ntdll.dll")] public static extern int NtResumeProcess(IntPtr processHandle);
    }
}
'@
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


function Wait-CamTestWindowRecord {
    <# The record the warden of a test window published, or $null if none appeared in time. #>
    param(
        [Parameter(Mandatory = $true)]$Sandbox,
        [Parameter(Mandatory = $true)][string]$Title,
        [int]$TimeoutSeconds = 20
    )

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $record = Get-CamWindowRecord -Config $Sandbox.Config -Title $Title
        if ($record) { return $record }
        Start-Sleep -Milliseconds 250
    }
    return $null
}


function Select-CamTestTab {
    <#
        Makes tab $Index of a test window the active one, and waits until the
        window shows $ExpectTitle. Index 0 is the warden's own pane: after
        Add-CamTestForeignTab this puts our title back on the window, which is
        the NVR's dead host tab being the active one at the moment a start
        clears it.

        Returns $true when the window shows the expected title.
    #>
    param(
        [Parameter(Mandatory = $true)]$Window,
        [Parameter(Mandatory = $true)][int]$Index,
        [Parameter(Mandatory = $true)][string]$ExpectTitle,
        [int]$TimeoutSeconds = 15
    )

    Import-CamWindowApi
    $hwnd = [IntPtr]::new([int64]$Window.Hwnd)
    & wt.exe @('-w', $Window.WtName, 'focus-tab', '--target', [string]$Index)

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        if ([CamWin]::TitleOf($hwnd) -ceq $ExpectTitle) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}


function Wait-CamTestProcessExit {
    <# $true once none of these pids is running. #>
    param(
        [AllowEmptyCollection()][int[]]$ProcessId = @(),
        [int]$TimeoutSeconds = 15
    )

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ($true) {
        $alive = @($ProcessId | Where-Object { $_ -gt 0 -and (Get-Process -Id $_ -ErrorAction SilentlyContinue) })
        if ($alive.Count -eq 0) { return $true }
        if ((Get-Date) -ge $deadline) { return $false }
        Start-Sleep -Milliseconds 250
    }
}


# ---------------------------------------------------------------------------
# Pruner: recorder functions in-process, and a filesystem you can diff
# ---------------------------------------------------------------------------

function Get-CamTestRecorderFunctions {
    <#
        The named functions of cam.recorder.ps1 - or of -Script, such as
        cam.ps1 - as one scriptblock, for the caller to dot-source into its
        own scope:

            . (Get-CamTestRecorderFunctions -Name 'Invoke-CamPrune', 'Write-CamHost')
            . (Get-CamTestRecorderFunctions -Script 'cam.ps1' -Name 'Invoke-CamInstallAll')

        Taken from the parsed file rather than by dot-sourcing the script,
        which would run its entry point. Named, never all: the recorder has
        functions that share a name with this harness's own (Add-CamTestResult)
        and would shadow them. Dot-sourced into a check's scope, a function the
        check defines there - Get-CamFreeSpaceGB, Send-CamAlert - replaces the
        product's for every call the loaded functions make, because PowerShell
        resolves commands through the caller's scopes.
    #>
    param(
        [Parameter(Mandatory = $true)][string[]]$Name,
        [string]$Script = 'cam.recorder.ps1'
    )

    $path = Join-Path -Path (Get-CamTestRepo) -ChildPath $Script
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$null, [ref]$null)
    $found = @{}
    foreach ($definition in $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
        $found[$definition.Name] = $definition.Extent.Text
    }

    $text = New-Object System.Text.StringBuilder
    foreach ($wanted in $Name) {
        if (-not $found.ContainsKey($wanted)) { throw "$Script defines no function '$wanted'." }
        [void]$text.AppendLine($found[$wanted])
    }
    return [scriptblock]::Create($text.ToString())
}


function Get-CamTestDstTransitions {
    <#
        The next fall-back and the next spring-forward of this machine's time
        zone, as UTC instants, found from the zone itself rather than written
        down so the clock checks follow whatever zone they run in. Each item:
        Kind (FallBack | SpringForward), AtUtc, DeltaSeconds (-3600 / +3600 for
        a one-hour change). Empty for a zone without daylight saving.
    #>
    param([DateTime]$FromUtc = [DateTime]::UtcNow)

    $zone = [TimeZoneInfo]::Local
    $found = @{}
    $hour = [DateTime]::new($FromUtc.Year, $FromUtc.Month, $FromUtc.Day, 0, 0, 0, [DateTimeKind]::Utc)
    for ($i = 0; $i -lt 24 * 400 -and $found.Count -lt 2; $i++) {
        $next = $hour.AddHours(1)
        $before = $zone.GetUtcOffset($hour)
        $after = $zone.GetUtcOffset($next)
        if ($before -ne $after) {
            $at = $next
            while ($zone.GetUtcOffset($at.AddMinutes(-1)) -eq $after) { $at = $at.AddMinutes(-1) }
            $kind = $(if ($after -lt $before) { 'FallBack' } else { 'SpringForward' })
            if (-not $found.ContainsKey($kind)) {
                $found[$kind] = [pscustomobject]@{ Kind = $kind; AtUtc = $at; DeltaSeconds = ($after - $before).TotalSeconds }
            }
        }
        $hour = $next
    }
    return @($found.Values | Sort-Object -Property AtUtc)
}


function Set-CamTestFileAge {
    <#
        Backdates a file's write AND creation time; the pruner reads both. A
        UTC $Time sets the exact instant, including one inside the repeated
        hour of a fall-back that a local time cannot name.
    #>
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


function New-CamTestAlertRecorder {
    <#
        A list for a check's Send-CamAlert stub to record into. The stub itself
        has to be defined in the check's own scope to replace the product's, so
        each check writes the one line that does it:

            function Send-CamAlert { param($Title, $Message, $EventId, $EntryType, $LogPath, $LogMaxBytes) Add-CamTestAlertCall -List $recordedAlerts -Title $Title -Message $Message -EventId $EventId -EntryType $EntryType }
    #>
    return , (New-Object System.Collections.ArrayList)
}

function Add-CamTestAlertCall {
    <# Records one Send-CamAlert call and answers the way the product's does. #>
    param($List, $Title, $Message, $EventId, $EntryType)
    [void]$List.Add([pscustomobject]@{ Title = [string]$Title; Message = [string]$Message; EventId = [int]$EventId; EntryType = [string]$EntryType })
    return @{ Title = $Title; Message = $Message; Channels = 'recorded by the test stub, not shown' }
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


# ---------------------------------------------------------------------------
# -Install / -Uninstall
# ---------------------------------------------------------------------------

function Get-CamTestLogonTaskSnapshot {
    <#
        Every registered task whose name starts with IPCCTV-, one line each:
        path, name, state and a sha256 of its exported XML. Two snapshots that
        compare equal mean no such task was added, removed or re-registered.
    #>
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $lines = foreach ($task in @(Get-ScheduledTask -TaskName 'IPCCTV-*' -ErrorAction SilentlyContinue)) {
            $xml = Export-ScheduledTask -TaskName $task.TaskName -TaskPath $task.TaskPath
            $hash = [System.BitConverter]::ToString($sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($xml))).Replace('-', '')
            '{0}{1} {2} {3}' -f $task.TaskPath, $task.TaskName, $task.State, $hash.Substring(0, 16)
        }
    }
    finally { $sha.Dispose() }
    return @($lines | Sort-Object)
}


function Get-CamTestLauncherManifest {
    <#
        Get-CamTestFileManifest of a launcher folder, with every log reduced
        to its presence: a running camui appends to camui.log, so its hash
        changes without anything here having touched it. Empty when the
        folder does not exist.
    #>
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) { return @{} }
    $manifest = Get-CamTestFileManifest -Path $Path
    foreach ($key in @($manifest.Keys)) {
        if ($key -match '\.log(\.prev)?$') { $manifest[$key] = 'log' }
    }
    return $manifest
}


function New-CamTestInstallSandbox {
    <#
        A New-CamTestSandbox that cam.ps1 -Install can run in. It adds cam.ps1
        and the dashboard script, a LOCALAPPDATA of its own for the launchers,
        a MediaMTX config path inside the sandbox, and a stand-in file for
        every binary the preflight looks for - ffmpeg, MediaMTX, mpv and
        camui.exe - so that each one can be taken away by a check.

        -TaskPrefix rewrites the three logon task names in the sandbox's copy
        of cam.ps1 and cam.recorder.ps1, so a sandbox that really installs
        cannot touch the Operator's IPCCTV-* tasks. It throws if any real name
        is left in either file. Without it the sandbox keeps the real names and
        must only ever be run with -WhatIf.

        -MediaMtxPath points RestreamerExePath at a real binary, for a sandbox
        whose -InstallRestreamer will run it with --version.
    #>
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [string]$TaskPrefix,
        [string]$MediaMtxPath
    )

    $cameras = @([pscustomobject]@{ Name = 'Front Entrance'; IP = '10.254.254.254'; Quality = 'stream1'; Record = $false })
    $dir = Join-Path -Path $env:TEMP -ChildPath ("cam-tests\" + $Name)
    $bin = Join-Path -Path $dir -ChildPath 'bin'
    $mediamtx = $(if ($MediaMtxPath) { $MediaMtxPath } else { Join-Path -Path $bin -ChildPath 'mediamtx.exe' })
    $sandbox = New-CamTestSandbox -Name $Name -Cameras $cameras -Config @{
        FfmpegPath           = (Join-Path -Path $bin -ChildPath 'ffmpeg.exe')
        RestreamerExePath    = $mediamtx
        RestreamerConfigPath = (Join-Path -Path $dir -ChildPath 'mediamtx\mediamtx.yml')
    }

    $repo = Get-CamTestRepo
    foreach ($file in @('cam.ps1', 'mpv.cam.autolayout.autorestart.ps1')) {
        Copy-Item -LiteralPath (Join-Path -Path $repo -ChildPath $file) -Destination $dir -Force
    }
    [void](New-Item -ItemType Directory -Path $bin -Force)
    foreach ($file in @('ffmpeg.exe', 'mediamtx.exe', 'mpv.com')) {
        [System.IO.File]::WriteAllText((Join-Path -Path $bin -ChildPath $file), 'stand-in')
    }
    [System.IO.File]::WriteAllText((Join-Path -Path $dir -ChildPath 'camui.exe'), 'stand-in')

    $dashboard = Join-Path -Path $dir -ChildPath 'mpv.cam.autolayout.autorestart.ps1'
    $text = [System.IO.File]::ReadAllText($dashboard)
    $mpv = Join-Path -Path $bin -ChildPath 'mpv.com'
    $text = [regex]::Replace($text, '(?m)^\$MpvPath = "[^"]*"', ('$MpvPath = "{0}"' -f $mpv))
    if (-not $text.Contains($mpv)) { throw 'could not point the sandbox dashboard at its stand-in mpv' }
    [System.IO.File]::WriteAllText($dashboard, $text)

    $tasks = [ordered]@{ Recorder = 'IPCCTV-Recorder'; Dashboard = 'IPCCTV-Dashboard'; Ui = 'IPCCTV-Ui' }
    if ($TaskPrefix) {
        foreach ($file in @('cam.ps1', 'cam.recorder.ps1')) {
            $path = Join-Path -Path $dir -ChildPath $file
            $text = [System.IO.File]::ReadAllText($path)
            foreach ($key in @($tasks.Keys)) {
                $text = $text.Replace(("= '{0}'" -f $tasks[$key]), ("= '{0}{1}'" -f $TaskPrefix, $key))
            }
            if ($text -match "'IPCCTV-(Recorder|Dashboard|Ui)'") { throw "the sandbox $file still names a real task: $($Matches[0])" }
            [System.IO.File]::WriteAllText($path, $text)
        }
        foreach ($key in @($tasks.Keys)) { $tasks[$key] = $TaskPrefix + $key }
    }

    $localAppData = Join-Path -Path $dir -ChildPath 'localappdata'
    [void](New-Item -ItemType Directory -Path $localAppData -Force)
    Add-Member -InputObject $sandbox -NotePropertyName 'Cam' -NotePropertyValue (Join-Path -Path $dir -ChildPath 'cam.ps1')
    Add-Member -InputObject $sandbox -NotePropertyName 'Bin' -NotePropertyValue $bin
    Add-Member -InputObject $sandbox -NotePropertyName 'Dashboard' -NotePropertyValue $dashboard
    Add-Member -InputObject $sandbox -NotePropertyName 'LocalAppData' -NotePropertyValue $localAppData
    Add-Member -InputObject $sandbox -NotePropertyName 'LauncherDir' -NotePropertyValue (Join-Path -Path $localAppData -ChildPath 'IPCCTV')
    Add-Member -InputObject $sandbox -NotePropertyName 'TaskNames' -NotePropertyValue $tasks
    return $sandbox
}


function Get-CamTestComponentStatus {
    <#
        The status column of one row of the table -Install and -Uninstall end
        with, or $null when there is no row for that component.
    #>
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Component
    )

    $pattern = '(?m)^  ' + [regex]::Escape($Component) +
        '\s+(already installed|would install|not installed|would remove|installed|skipped|failed|removed|kept)(\s|$)'
    $match = [regex]::Match($Text, $pattern)
    if (-not $match.Success) { return $null }
    return $match.Groups[1].Value
}


function Get-CamMarkdownAnchors {
    <#
        The anchors GitHub gives the headings of one markdown text: lower case,
        punctuation dropped, spaces to hyphens, and -1, -2 on repeats. A line
        starting with # inside a fenced block is code, not a heading.
    #>
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)

    $anchors = New-Object System.Collections.Generic.HashSet[string]
    $seen = @{}
    $inFence = $false
    foreach ($line in ($Text -split "`r?`n")) {
        if ($line -match '^\s{0,3}```') { $inFence = -not $inFence; continue }
        if ($inFence) { continue }
        $heading = [regex]::Match($line, '^\s{0,3}#{1,6}\s+(.*?)(?:\s+#+)?\s*$')
        if (-not $heading.Success) { continue }
        $words = [regex]::Replace($heading.Groups[1].Value, '!?\[([^\]]*)\]\([^)]*\)', '$1')
        $slug = [regex]::Replace($words.ToLowerInvariant(), '[^\w\- ]', '').Replace(' ', '-')
        if ($seen.ContainsKey($slug)) {
            $seen[$slug]++
            [void]$anchors.Add(('{0}-{1}' -f $slug, $seen[$slug]))
        } else {
            $seen[$slug] = 0
            [void]$anchors.Add($slug)
        }
    }
    return , $anchors
}


function Find-CamDocLinkProblems {
    <#
        Every relative link in the given markdown documents, checked: the file
        it names must exist, and an #anchor - into another file or into the
        same one - must be a heading of its target. Documents is a hashtable of
        full path to text; a target in it is judged by that text rather than by
        the file on disk, which is what lets a check break a link in memory.
        Links inside code are not links. URLs with a scheme are not checked.
        Returns Links, Anchors (counts checked) and Problems (strings).
    #>
    param([Parameter(Mandatory = $true)][hashtable]$Documents)

    $problems = New-Object System.Collections.ArrayList
    $anchorCache = @{}
    $links = 0
    $anchors = 0

    foreach ($path in @($Documents.Keys | Sort-Object)) {
        $prose = New-Object System.Collections.ArrayList
        $inFence = $false
        foreach ($line in ($Documents[$path] -split "`r?`n")) {
            if ($line -match '^\s{0,3}```') { $inFence = -not $inFence; continue }
            if (-not $inFence) { [void]$prose.Add([regex]::Replace($line, '`[^`]*`', '')) }
        }

        foreach ($link in [regex]::Matches(($prose -join "`n"), '!?\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)')) {
            $target = $link.Groups[1].Value
            if ($target -match '^[A-Za-z][A-Za-z0-9+.-]*:' -or $target.StartsWith('//')) { continue }
            $links++
            $file, $anchor = $target -split '#', 2
            if ($file) {
                $resolved = [System.IO.Path]::GetFullPath((Join-Path -Path (Split-Path -Path $path -Parent) -ChildPath ($file -replace '/', '\')))
                if (-not $Documents.ContainsKey($resolved) -and -not (Test-Path -LiteralPath $resolved)) {
                    [void]$problems.Add(("{0}: '{1}' - no such file" -f (Split-Path -Path $path -Leaf), $target))
                    continue
                }
            } else {
                $resolved = $path
            }
            if (-not $anchor) { continue }
            $anchors++
            if (-not $anchorCache.ContainsKey($resolved)) {
                $text = $(if ($Documents.ContainsKey($resolved)) { $Documents[$resolved] } else { [System.IO.File]::ReadAllText($resolved) })
                $anchorCache[$resolved] = Get-CamMarkdownAnchors -Text $text
            }
            if (-not $anchorCache[$resolved].Contains($anchor)) {
                [void]$problems.Add(("{0}: '{1}' - {2} has no heading with anchor #{3}" -f (Split-Path -Path $path -Leaf), $target, (Split-Path -Path $resolved -Leaf), $anchor))
            }
        }
    }

    return [pscustomobject]@{ Links = $links; Anchors = $anchors; Problems = @($problems) }
}
