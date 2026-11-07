param(
    [string]$ControlUrl,
    [string]$NodeId,
    [string]$Model,
    [string]$BackendUrl,
    [Security.SecureString]$NodeToken,
    [Security.SecureString]$BackendKey,
    [switch]$NoBackendKey
)

$verifier = Join-Path $PSScriptRoot 'Verify-Package.ps1'
if (-not (Test-Path -LiteralPath $verifier -PathType Leaf)) { throw 'PACKAGE_MISSING: Verify-Package.ps1. Extract the complete worker ZIP again.' }
$verified = & $verifier
if ($verified.Package -ne 'worker') { throw 'Expected the worker package.' }
. "$PSScriptRoot/Worker.Common.ps1"
Assert-WorkerExecutable

if (-not $ControlUrl) { $ControlUrl = Read-Host 'Center URL (https://...)' }
if (-not $NodeId) { $NodeId = Read-Host 'Enrolled node ID' }
if (-not $Model) { $Model = Read-Host 'Exact enrolled model name' }
if (-not $BackendUrl) {
    $BackendUrl = Read-Host 'Backend base URL [http://127.0.0.1:8000/v1]'
    if (-not $BackendUrl) { $BackendUrl = 'http://127.0.0.1:8000/v1' }
}
$ControlUrl = Get-WorkerUrl $ControlUrl 'ControlUrl'
$BackendUrl = Get-WorkerUrl $BackendUrl 'BackendUrl'
$NodeId = $NodeId.Trim().ToLowerInvariant()
$Model = $Model.Trim()
Assert-WorkerIdentity $NodeId $Model

if (-not $NodeToken) { $NodeToken = Read-Host 'Node token (hidden)' -AsSecureString }
if ($NodeToken.Length -lt 32 -or (Get-WorkerPlaintext $NodeToken) -match '\s') { throw 'Enter the complete node token returned by enrollment.' }
if ($NoBackendKey -and $BackendKey) { throw 'Choose either -BackendKey or -NoBackendKey.' }
if (-not $BackendKey -and -not $NoBackendKey) { $BackendKey = Read-Host 'Backend API key (optional; Enter for none)' -AsSecureString }

$encryptedBackendKey = ''
if ($null -ne $BackendKey -and $BackendKey.Length) {
    if ((Get-WorkerPlaintext $BackendKey) -match '\s') { throw 'Backend API key cannot contain whitespace.' }
    $encryptedBackendKey = ConvertFrom-SecureString $BackendKey
}

$null = New-Item -ItemType Directory -Force -Path $script:WorkerState
$lock = Enter-WorkerLock
try {
    if (Get-WorkerProcess) { throw 'Stop the running worker with Stop-Worker.ps1 before replacing its configuration.' }
    Save-WorkerJson $script:WorkerConfig ([ordered]@{
        version = 1
        control_url = $ControlUrl
        node_id = $NodeId
        model = $Model
        backend_url = $BackendUrl
        node_token = ConvertFrom-SecureString $NodeToken
        backend_key = $encryptedBackendKey
    })
} finally { $lock.Dispose() }

Write-Output 'Worker configuration saved. Run .\Start-Worker.ps1 -Check to verify the center and backend, or .\Start-Worker.ps1 to launch.'
