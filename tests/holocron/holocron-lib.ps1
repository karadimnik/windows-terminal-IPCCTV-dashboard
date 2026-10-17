<#
    holocron-lib.ps1 - shared helpers of the Holocron contract tooling (W0).

    Dot-source it, do not run it. It loads the PowerShell product exactly as the
    harness does (tests\cam.testlib.ps1 dot-sources cam.common.ps1) and takes
    single functions out of cam.recorder.ps1 through the parsed file, so the
    recorder's entry point never runs. Nothing here touches a real archive, a
    real config, a port, a process or a scheduled task: every file it writes is
    in a throwaway folder under %TEMP% or in the output folder it was given.

    ASCII only on purpose: Windows PowerShell 5.1 reads a script without a BOM
    in the ANSI code page, so a literal non-ASCII character would change.
#>

$ErrorActionPreference = 'Stop'

$script:HolocronLibRoot = $PSScriptRoot
$script:HolocronRepo = Split-Path -Path (Split-Path -Path $script:HolocronLibRoot -Parent) -Parent

. (Join-Path -Path $script:HolocronRepo -ChildPath 'tests\cam.testlib.ps1')

# The zone every committed golden was generated in. The DST goldens and every
# offset in a timestamp depend on it, so the generators refuse to run elsewhere.
$script:HolocronZone = 'GTB Standard Time'


function Get-HolocronRepo { return $script:HolocronRepo }


function Assert-HolocronEnvironment {
    if ($PSVersionTable.PSVersion.Major -ne 5) { throw "Windows PowerShell 5.1 is required, this is $($PSVersionTable.PSVersion)." }
    if ([TimeZoneInfo]::Local.Id -ne $script:HolocronZone) {
        throw "The goldens are generated in the zone '$($script:HolocronZone)'; this machine is in '$([TimeZoneInfo]::Local.Id)'."
    }
}


function New-HolocronSandbox {
    $dir = Join-Path -Path ([System.IO.Path]::GetTempPath()) -ChildPath ('holocron-w0-' + [guid]::NewGuid().ToString('N'))
    [void](New-Item -ItemType Directory -Path $dir -Force)
    return $dir
}


function Remove-HolocronSandbox {
    param([string]$Path)
    if ($Path -and (Test-Path -LiteralPath $Path) -and ((Split-Path -Leaf $Path) -like 'holocron-w0-*')) {
        Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction SilentlyContinue
    }
}


function Save-HolocronText {
    <# UTF-8 without BOM, the text exactly as given, the folder created if needed. #>
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    $dir = Split-Path -Path $Path -Parent
    if ($dir -and -not (Test-Path -LiteralPath $dir)) { [void](New-Item -ItemType Directory -Path $dir -Force) }
    [System.IO.File]::WriteAllText($Path, $Text, (New-Object System.Text.UTF8Encoding($false)))
}


function Read-HolocronText {
    param([Parameter(Mandatory = $true)][string]$Path)
    return [System.IO.File]::ReadAllText($Path, (New-Object System.Text.UTF8Encoding($false)))
}


function ConvertTo-HolocronJson {
    <#
        ConvertTo-Json with depth 12, every character above U+007E written as a
        \uXXXX escape (so a golden is pure ASCII whatever the console code page
        is), and one trailing newline. Dictionaries keep their order when they
        are [ordered].
    #>
    param([Parameter(Mandatory = $true)]$InputObject)
    $json = ConvertTo-Json -InputObject $InputObject -Depth 12
    $builder = New-Object System.Text.StringBuilder
    foreach ($ch in $json.ToCharArray()) {
        if ([int]$ch -gt 126) { [void]$builder.AppendFormat('\u{0:x4}', [int]$ch) } else { [void]$builder.Append($ch) }
    }
    return $builder.ToString() + "`n"
}
