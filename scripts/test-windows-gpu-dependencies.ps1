$ErrorActionPreference = 'Stop'
$project = Split-Path -Parent $PSScriptRoot
. (Join-Path $project 'packaging/windows/gpu/Gpu.Dependencies.ps1')

function Assert-Failure([scriptblock]$Action, [string]$Message) {
    try { & $Action } catch {
        if ($_.Exception.Message -like "*$Message*") { return }
        throw
    }
    throw "Expected failure matching: $Message"
}

# Exercise the installer decisions without writing system directories, networking, or UAC.
$script:hasVulkan = $true
$script:missingVc = @()
$script:downloads = 0
$script:installs = 0
$script:downloadPath = ''
$script:signatureStatus = 'Valid'
$script:publisher = 'Microsoft Corporation'
$script:originalFilename = 'VC_redist.x64.exe'
$script:installerExit = 0
$script:repairWorks = $true
function Get-SwarmMissingVcRuntime { $script:missingVc }
function Test-SwarmVulkanLoader { $script:hasVulkan }
function Invoke-WebRequest($Uri, $OutFile, $TimeoutSec, $MaximumRedirection, [switch]$UseBasicParsing) {
    if ($Uri -ne 'https://aka.ms/vc14/vc_redist.x64.exe' -or $TimeoutSec -le 0) { throw 'Unexpected installer download settings.' }
    $script:downloads++
    $script:downloadPath = $OutFile
    [IO.File]::WriteAllText($OutFile, 'test fixture, never executed')
}
function Get-AuthenticodeSignature($LiteralPath) {
    if ($LiteralPath -ne $script:downloadPath) { throw 'Verified the wrong installer file.' }
    $cert = [pscustomobject]@{}
    $cert | Add-Member -MemberType ScriptMethod -Name GetNameInfo -Value { $script:publisher }
    [pscustomobject]@{ Status = $script:signatureStatus; SignatureType = 'Authenticode'; SignerCertificate = $cert }
}
function Get-Item($LiteralPath) {
    if ($LiteralPath -ne $script:downloadPath) { throw 'Read metadata from the wrong installer file.' }
    [pscustomobject]@{ VersionInfo = [pscustomobject]@{ OriginalFilename = $script:originalFilename; FileMajorPart = 14 } }
}
function Start-Process($FilePath, $ArgumentList, $Verb, $WindowStyle, [switch]$Wait, [switch]$PassThru) {
    if ($FilePath -ne $script:downloadPath -or !$Wait -or !$PassThru -or $Verb -ne 'RunAs' -or
        '/install' -notin $ArgumentList -or '/quiet' -notin $ArgumentList -or '/norestart' -notin $ArgumentList) {
        throw 'Incorrect installer launch arguments.'
    }
    $script:installs++
    if ($script:repairWorks -and $script:installerExit -eq 0) { $script:missingVc = @() }
    [pscustomobject]@{ ExitCode = $script:installerExit }
}

Assert-SwarmGpuDependencies -InstallDependencies
if ($script:downloads -ne 0) { throw 'Downloaded an installer for an already healthy host.' }
$script:missingVc = @('vcruntime140.dll')
Assert-Failure { Assert-SwarmGpuDependencies } 'rerun with -InstallDependencies'
if ($script:downloads -ne 0) { throw 'Downloaded an installer without being asked.' }
$script:signatureStatus = 'NotSigned'
Assert-Failure { Assert-SwarmGpuDependencies -InstallDependencies } 'Authenticode'
if ($script:installs -ne 0 -or (Test-Path -LiteralPath $script:downloadPath)) { throw 'Unsigned installer was executed or retained.' }
$script:signatureStatus = 'Valid'
$script:publisher = 'Another Publisher'
Assert-Failure { Assert-SwarmGpuDependencies -InstallDependencies } 'Authenticode'
if ($script:installs -ne 0) { throw 'An installer with the wrong publisher was executed.' }
$script:publisher = 'Microsoft Corporation'
$script:originalFilename = 'VC_redist.x86.exe'
Assert-Failure { Assert-SwarmGpuDependencies -InstallDependencies } 'not the Visual C++ v14 x64 installer'
if ($script:installs -ne 0) { throw 'An installer for the wrong architecture was executed.' }
$script:originalFilename = 'VC_redist.x64.exe'
$script:installerExit = 3010
Assert-Failure { Assert-SwarmGpuDependencies -InstallDependencies } 'restart Windows'
if ($script:installs -ne 1) { throw 'Expected one installer invocation before a restart is requested.' }
$script:installerExit = 0
$script:repairWorks = $false
Assert-Failure { Assert-SwarmGpuDependencies -InstallDependencies } 'still missing'
$script:repairWorks = $true
Assert-SwarmGpuDependencies -InstallDependencies
Assert-SwarmGpuDependencies -InstallDependencies
if ($script:installs -ne 3 -or (Test-Path -LiteralPath $script:downloadPath)) { throw 'Unexpected extra installation or uncleaned temporary file.' }
$script:hasVulkan = $false
Assert-Failure { Assert-SwarmGpuDependencies -InstallDependencies } 'Vulkan-capable driver'
if ($script:installs -ne 3) { throw 'Missing Vulkan loader triggered another installer.' }
Write-Output 'PASS Windows GPU dependency install decisions, signature/architecture gates, reboot, retry and no-op behavior.'
