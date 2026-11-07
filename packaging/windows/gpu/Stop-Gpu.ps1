param([string]$StateDirectory)

$ErrorActionPreference = 'Stop'
& (Join-Path $PSScriptRoot 'scripts/stop-local.ps1') -LocalGpu @PSBoundParameters
