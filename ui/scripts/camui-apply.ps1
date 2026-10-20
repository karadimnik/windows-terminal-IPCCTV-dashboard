<#
.SYNOPSIS
    Shim: the settings writer moved with the web app to internal\web\scripts\camui-apply.ps1
    (it is embedded in camui.exe and in holocron.exe from there).

.DESCRIPTION
    This file stays at its old path because the PowerShell harness scans ui\scripts. It forwards
    every argument to the real script and is not a command surface.
#>
& (Join-Path -Path $PSScriptRoot -ChildPath '..\..\internal\web\scripts\camui-apply.ps1') @args
exit $LASTEXITCODE
