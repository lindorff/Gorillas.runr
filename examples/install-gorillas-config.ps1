<#
.SYNOPSIS
    Installs the shared Gorillas runr config as your runr config.

.DESCRIPTION
    Copies gorillas.toml (next to this script) to %AppData%\runr\config.toml.
    If a config already exists there, it is first saved as
    config.toml.bak-<timestamp> in the same folder.

.EXAMPLE
    .\examples\install-gorillas-config.ps1
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$source = Join-Path $PSScriptRoot 'gorillas.toml'
if (-not (Test-Path -LiteralPath $source)) {
    throw "Can't find $source"
}

$targetDir = Join-Path $env:APPDATA 'runr'
$target = Join-Path $targetDir 'config.toml'

New-Item -ItemType Directory -Path $targetDir -Force | Out-Null

if (Test-Path -LiteralPath $target) {
    $backup = "$target.bak-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
    Copy-Item -LiteralPath $target -Destination $backup
    Write-Host "Saved your existing config as $backup"
}

Copy-Item -LiteralPath $source -Destination $target -Force
Write-Host "Installed $source as $target"

if ($env:RUNR_CONFIG) {
    Write-Warning "RUNR_CONFIG is set to '$env:RUNR_CONFIG', so runr reads that file instead of $target."
}
if (-not $env:RUNR_ROOT) {
    Write-Warning 'RUNR_ROOT is not set. Set it to the folder that holds your Gorillas repositories, e.g. [Environment]::SetEnvironmentVariable(''RUNR_ROOT'', ''C:\projects'', ''User''), then open a new terminal.'
}
