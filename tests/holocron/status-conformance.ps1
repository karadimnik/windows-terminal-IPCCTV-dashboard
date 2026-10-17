<#
    status-conformance.ps1 - the PowerShell PRODUCER check of the status.json
    contract (W0a).

    Runs the product's real writers (Write-CamStatusFile and the two patch
    functions) against synthetic inputs with the REAL clock, real free space and
    the real pid, and writes the documents to testdata\golden\status\generated\
    (gitignored). `go test ./internal/status` validates every file there against
    status.schema.json when it is present and skips with a message when not.

    Also prints what was observed about the two known differences. Needs
    Windows PowerShell 5.1 in the zone the goldens use; touches no real archive,
    config, port, process or task.

        powershell.exe -NoProfile -ExecutionPolicy Bypass -File tests\holocron\status-conformance.ps1
#>
param([string]$OutDir)

$ErrorActionPreference = 'Stop'
. (Join-Path -Path $PSScriptRoot -ChildPath 'holocron-lib.ps1')
. (Join-Path -Path $PSScriptRoot -ChildPath 'status-docs.ps1')

if (-not $OutDir) { $OutDir = Join-Path -Path (Get-HolocronRepo) -ChildPath 'testdata\golden\status\generated' }
if (Test-Path -LiteralPath $OutDir) { Get-ChildItem -LiteralPath $OutDir -Filter '*.json' | Remove-Item -Force }
[void](New-Item -ItemType Directory -Path $OutDir -Force)

$result = New-HolocronStatusDocuments -OutDir $OutDir
$count = @(Get-ChildItem -LiteralPath $OutDir -Filter '*.json').Count
Write-Host ("generated {0} documents in {1}" -f $count, $OutDir)
Write-Host ("F1: the file before the restart had LastWindowClose = {0}; the document the writer wrote after has it = {1}" -f $result.F1BeforeHadLastWindowClose, $result.F1AfterHasLastWindowClose)
Write-Host ("window-closed: rejected by Write-CamStatusFile = {0}" -f $result.WindowClosedRejectedByWriter)
if ($result.WindowClosedRejection) { Write-Host ("  " + $result.WindowClosedRejection) }
