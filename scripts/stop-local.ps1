param([string]$StateDirectory, [switch]$LocalGpu)

. "$PSScriptRoot/local-common.ps1"
if ($LocalGpu -and -not $StateDirectory) { $StateDirectory = Join-Path $script:ProjectRoot 'data/local-gpu' }
$state = Resolve-SwarmStateDirectory $StateDirectory
if (-not (Test-Path -LiteralPath $state -PathType Container)) { return }
$lock = Enter-SwarmLock $state
try {
    $processFile = Join-Path $state 'processes.json'
    if (-not (Test-Path -LiteralPath $processFile)) { return }
    $records = @((Get-Content -LiteralPath $processFile -Encoding UTF8 -Raw | ConvertFrom-Json).processes)
    Stop-SwarmProcessSet $records $processFile
    Write-Output "Local processes stopped. State remains at $state."
} finally { $lock.Dispose() }
