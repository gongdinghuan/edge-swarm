param(
    [ValidateRange(1,65535)][int]$ControlPort,
    [ValidateRange(1,65535)][int]$ModelPort,
    [string]$StateDirectory,
    [switch]$Check,
    [switch]$InstallDependencies
)

$ErrorActionPreference = 'Stop'
$verified = & (Join-Path $PSScriptRoot 'Verify-Package.ps1')
if ($verified.Package -ne 'gpu') { throw 'Expected the GPU bundle. Extract a clean copy of the GPU ZIP and retry.' }

if ([Environment]::OSVersion.Platform -ne 'Win32NT' -or
    [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne [Runtime.InteropServices.Architecture]::X64 -or
    -not [Environment]::Is64BitProcess) {
    throw 'Start-Gpu requires Windows x64 and a 64-bit PowerShell process.'
}

. (Join-Path $PSScriptRoot 'Gpu.Dependencies.ps1')
Assert-SwarmGpuDependencies -InstallDependencies:$InstallDependencies
if ($Check) {
    Write-Output 'GPU package integrity and system dependency checks passed.'
    return
}

$null = $PSBoundParameters.Remove('Check')
$null = $PSBoundParameters.Remove('InstallDependencies')
& (Join-Path $PSScriptRoot 'scripts/start-local.ps1') -LocalGpu -SkipBuild @PSBoundParameters
