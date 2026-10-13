<#
.SYNOPSIS
    Applies a settings change from the control UI through the recorder's own
    writers.

.DESCRIPTION
    camui.exe never writes cameras.json or recorder.config.json itself. It hands
    the desired state to this script, which dot-sources cam.common.ps1 and goes
    through Set-CamList and Move-CamFileAtomic - the same atomic writers the CLI
    uses. That is what keeps the UTF-8 byte-order mark, every field camui does
    not know about, and the crash-safe replace, rather than reimplementing all
    three in Go and getting one of them subtly wrong.

    This script is EMBEDDED IN camui.exe and written to a temporary directory
    for the duration of one call. It is not a command surface: cam.ps1 and
    cam.recorder.ps1 remain the only scripts the Operator runs. It is kept in
    the repository so the harness parses it and so the write path is readable.

    Input and output are JSON files rather than arguments and stdout, so no
    value has to survive a command line or a console codepage.

.PARAMETER Repo
    The directory holding cam.common.ps1, cameras.json and recorder.config.json.

.PARAMETER Kind
    cameras - replace the camera list with the rows in InputPath.
    config  - apply the allow-listed recorder.config.json keys in InputPath.

.PARAMETER InputPath
    JSON request. For cameras: { "cameras": [ { "originalName", "name", "ip",
    "quality", "record" } ] }, in the order they should appear in the file; an
    empty originalName means a new row. For config: { "config": { key: value } }.

.PARAMETER OutputPath
    JSON result: { "ok", "changed", "message", "path" } or { "ok": false,
    "error" }. Also reflected in the exit code, 0 or 1.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Repo,
    [Parameter(Mandatory = $true)][ValidateSet('cameras', 'config')][string]$Kind,
    [Parameter(Mandatory = $true)][string]$InputPath,
    [Parameter(Mandatory = $true)][string]$OutputPath
)

$ErrorActionPreference = 'Stop'

# The allow-list, repeated here on purpose. camui validates the request and
# this script is the thing that writes, so the writer refuses a key outside the
# list on its own authority. A single list in one place would be bypassed by
# any future caller that forgot to check.
$script:EditableConfigKeys = @('SegmentSeconds', 'ReserveGB', 'PruneHysteresisGB', 'MaxRetentionDays')


function Write-ApplyResult {
    param(
        [Parameter(Mandatory = $true)][bool]$Ok,
        # Never $Error: that is an automatic variable, and a parameter of the
        # same name (in any casing) IS it.
        [string]$ErrorText = '',
        [int]$Changed = 0,
        [string]$Message = '',
        [string]$Path = ''
    )

    $result = [pscustomobject]@{
        ok      = $Ok
        error   = $ErrorText
        changed = $Changed
        message = $Message
        path    = $Path
    }
    # No BOM: this file is read back by camui, not by a PowerShell that needs
    # one, and a BOM in front of '{' is one more thing a JSON reader has to
    # tolerate.
    [System.IO.File]::WriteAllText($OutputPath, (ConvertTo-Json -InputObject $result -Depth 4),
        (New-Object System.Text.UTF8Encoding($false)))
}


function Get-ApplyRequest {
    # Get-Content -Raw honours a BOM if one is there and tolerates none.
    $text = Get-Content -Raw -LiteralPath $InputPath
    if ([string]::IsNullOrWhiteSpace($text)) { throw "The request file '$InputPath' is empty." }
    return (ConvertFrom-Json -InputObject $text)
}


function Set-ApplyCameraList {
    <#
        Rebuilds cameras.json from the requested rows.

        Rows are matched to the existing file by originalName, and the matched
        object is REUSED rather than rebuilt, so any field camui never saw -
        anything a future version of the recorder adds, anything the Operator
        put there by hand - survives the edit. The output order is the order the
        request asked for.
    #>
    param([Parameter(Mandatory = $true)]$Request)

    $rows = @($Request.cameras)
    if ($null -eq $Request.cameras) { throw 'The request has no "cameras" array.' }

    # Assigned FIRST, then wrapped. Get-CamList ends in "return , @($parsed)"
    # to stop a one-camera list collapsing to a scalar, and @(Get-CamList)
    # NESTS that - the result is one element whose value is the whole array, so
    # every name comparison then runs against a space-joined string and matches
    # nothing. @($variable) enumerates correctly; only @(function-call) traps.
    $fromFile = Get-CamList
    $existing = @($fromFile)
    $before = ConvertTo-Json -InputObject $existing -Depth 10

    $output = @()
    $seen = @{}
    foreach ($row in $rows) {
        $name = [string]$row.name
        if ([string]::IsNullOrWhiteSpace($name)) { throw 'A camera row has an empty name.' }

        # Defence in depth: camui validates this, and the writer refuses it too.
        [void](Get-CamSafeName -Name $name)

        $key = $name.ToUpperInvariant()
        if ($seen.ContainsKey($key)) { throw "Two camera rows are both named '$name'." }
        $seen[$key] = $true

        $camera = $null
        $original = [string]$row.originalName
        if (-not [string]::IsNullOrWhiteSpace($original)) {
            $matched = @($existing | Where-Object { [string]$_.Name -eq $original })
            if ($matched.Count -eq 0) {
                throw "No camera named '$original' is in cameras.json; it may have been renamed since this page was loaded."
            }
            if ($matched.Count -gt 1) {
                throw "cameras.json holds more than one camera named '$original'."
            }
            $camera = $matched[0]
        }
        else {
            $camera = [pscustomobject]@{ Name = $name; IP = ''; Quality = 'stream1'; Record = $false }
        }

        Set-ApplyProperty -Object $camera -Name 'Name' -Value $name
        Set-ApplyProperty -Object $camera -Name 'IP' -Value ([string]$row.ip)
        Set-ApplyProperty -Object $camera -Name 'Quality' -Value ([string]$row.quality)
        Set-ApplyProperty -Object $camera -Name 'Record' -Value ([bool]$row.record)

        $output += $camera
    }

    $after = ConvertTo-Json -InputObject @($output) -Depth 10
    if ($after -eq $before) {
        Write-ApplyResult -Ok $true -Changed 0 -Message 'Nothing changed; cameras.json was left untouched.' -Path (Get-ApplyCameraPath)
        return
    }

    Set-CamList -Cameras $output

    $count = @($output).Count
    $reconcile = $script:ApplyConfig.ReconcileSeconds
    Write-ApplyResult -Ok $true -Changed $count -Path (Get-ApplyCameraPath) `
        -Message "cameras.json updated: $count camera(s). The running supervisor applies this within ${reconcile}s."
}


function Set-ApplyProperty {
    param(
        [Parameter(Mandatory = $true)]$Object,
        [Parameter(Mandatory = $true)][string]$Name,
        [AllowEmptyString()][AllowNull()]$Value
    )

    $property = $Object.PSObject.Properties[$Name]
    if ($property) { $Object.$Name = $Value }
    else { $Object | Add-Member -MemberType NoteProperty -Name $Name -Value $Value }
}


function Get-ApplyCameraPath { return (Join-Path -Path $Repo -ChildPath 'cameras.json') }
function Get-ApplyConfigPath { return (Join-Path -Path $Repo -ChildPath 'recorder.config.json') }


function Set-ApplyRecorderConfig {
    <#
        Applies the allow-listed keys to recorder.config.json.

        The file is parsed and re-serialised, which preserves every key, their
        order and their types, but DOES reindent the whole file on the first
        write from Windows PowerShell 5.1 - its ConvertTo-Json uses four spaces
        and two after the colon. That is a formatting change, not a data change,
        and the alternative - patching the JSON text - breaks on any file whose
        layout differs from what the patch expected.

        The existing byte-order mark and line ending are both preserved, and the
        replace goes through Move-CamFileAtomic.
    #>
    param([Parameter(Mandatory = $true)]$Request)

    if ($null -eq $Request.config) { throw 'The request has no "config" object.' }

    $path = Get-ApplyConfigPath
    if (-not (Test-Path -LiteralPath $path)) { throw "recorder.config.json not found at '$path'." }

    $bytes = [System.IO.File]::ReadAllBytes($path)
    $hasBom = ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF)
    $text = [System.Text.Encoding]::UTF8.GetString($bytes)
    if ($hasBom) { $text = $text.Substring(1) }
    $usesCrLf = $text.Contains("`r`n")

    $document = ConvertFrom-Json -InputObject $text

    $changed = 0
    $applied = @()
    foreach ($property in @($Request.config.PSObject.Properties)) {
        $key = $property.Name
        if ($script:EditableConfigKeys -notcontains $key) {
            throw "'$key' is not an editable configuration key."
        }
        $value = $property.Value
        $existing = $document.PSObject.Properties[$key]
        $before = if ($existing) { $existing.Value } else { $null }
        Set-ApplyProperty -Object $document -Name $key -Value $value

        # Compared as values, and rendered by interpolation: the -f operator
        # formats a double in the machine locale, so "20.5" would be written
        # into the message as "20,5" on this machine.
        $same = ($null -eq $before -and $null -eq $value) -or
                ($null -ne $before -and $null -ne $value -and $before -eq $value)
        if (-not $same) {
            $changed++
            $was = if ($null -eq $before) { 'none' } else { "$before" }
            $now = if ($null -eq $value) { 'none' } else { "$value" }
            $applied += "${key}: $was -> $now"
        }
    }

    if ($changed -eq 0) {
        Write-ApplyResult -Ok $true -Changed 0 -Message 'Nothing changed; recorder.config.json was left untouched.' -Path $path
        return
    }

    $json = ConvertTo-Json -InputObject $document -Depth 10
    # ConvertTo-Json emits CRLF on Windows. Normalise, then restore whichever
    # ending the file already used so the edit is not also a line-ending change.
    $json = $json -replace "`r`n", "`n"
    if ($usesCrLf) { $json = $json -replace "`n", "`r`n" }
    $json = $json + $(if ($usesCrLf) { "`r`n" } else { "`n" })

    $temp = "$path.tmp"
    if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Force }
    [System.IO.File]::WriteAllText($temp, $json, (New-Object System.Text.UTF8Encoding($hasBom)))
    Move-CamFileAtomic -TempPath $temp -Path $path

    $summary = $applied -join '; '
    Write-ApplyResult -Ok $true -Changed $changed -Path $path `
        -Message "recorder.config.json updated ($summary). The recorder reads it at startup, so this takes effect the next time it starts."
}


try {
    $library = Join-Path -Path $Repo -ChildPath 'cam.common.ps1'
    if (-not (Test-Path -LiteralPath $library)) {
        throw "cam.common.ps1 not found at '$library'. camui's script directory does not hold the recorder scripts."
    }
    . $library

    $script:ApplyConfig = Get-CamConfig
    $request = Get-ApplyRequest

    switch ($Kind) {
        'cameras' { Set-ApplyCameraList -Request $request }
        'config' { Set-ApplyRecorderConfig -Request $request }
    }
    exit 0
}
catch {
    $message = [string]$_.Exception.Message
    try { Write-ApplyResult -Ok $false -ErrorText $message }
    catch { Write-Error $message }
    exit 1
}
