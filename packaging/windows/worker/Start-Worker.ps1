param([switch]$Check)

$verifier = Join-Path $PSScriptRoot 'Verify-Package.ps1'
if (-not (Test-Path -LiteralPath $verifier -PathType Leaf)) { throw 'PACKAGE_MISSING: Verify-Package.ps1. Extract the complete worker ZIP again.' }
$verified = & $verifier
if ($verified.Package -ne 'worker') { throw 'Expected the worker package.' }
. "$PSScriptRoot/Worker.Common.ps1"
Assert-WorkerExecutable
if (-not (Test-Path -LiteralPath $script:WorkerConfig -PathType Leaf)) { throw 'Run Install-Worker.ps1 before starting the worker.' }

$lock = Enter-WorkerLock
try {
    $config = Get-Content -LiteralPath $script:WorkerConfig -Encoding UTF8 -Raw | ConvertFrom-Json
    if ($config.version -ne 1) { throw 'Unsupported worker configuration. Run Install-Worker.ps1 to configure this package.' }
    $control = Get-WorkerUrl $config.control_url 'ControlUrl'
    $backend = Get-WorkerUrl $config.backend_url 'BackendUrl'
    Assert-WorkerIdentity $config.node_id $config.model
    $nodeToken = Read-WorkerSecret $config.node_token
    $backendKey = ''
    if ($config.backend_key) { $backendKey = Read-WorkerSecret $config.backend_key }

    if (-not $Check -and (Get-WorkerProcess)) { throw 'This worker is already running. Run Stop-Worker.ps1 before restarting.' }
    Test-WorkerDependencies $control $backend $config.model $backendKey
    if ($Check) {
        Write-Output "Center readiness and backend model discovery passed for '$($config.model)'."
        return
    }

    $envBefore = @{}
    $vars = @{SWARM_NODE_ID = $config.node_id; SWARM_NODE_TOKEN = $nodeToken; SWARM_BACKEND_KEY = $backendKey}
    foreach ($name in $vars.Keys) { $envBefore[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
    $stdout = Join-Path $script:WorkerState 'worker.log'
    $stderr = Join-Path $script:WorkerState 'worker.err'
    try {
        foreach ($name in $vars.Keys) { [Environment]::SetEnvironmentVariable($name, $vars[$name], 'Process') }
        $process = Start-Process -FilePath $script:WorkerExe -WorkingDirectory $script:WorkerState `
            -ArgumentList @('-control', $control, '-model', $config.model, '-backend', $backend) `
            -WindowStyle Hidden -PassThru -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    } finally {
        foreach ($name in $vars.Keys) { [Environment]::SetEnvironmentVariable($name, $envBefore[$name], 'Process') }
    }
    $saved = $false
    try {
        Save-WorkerJson $script:WorkerProcess @{
            pid = $process.Id
            path = $script:WorkerExe
            started = $process.StartTime.ToUniversalTime().Ticks.ToString()
        }
        $saved = $true
    } finally {
        if (-not $saved -and -not $process.HasExited) { $process.Kill() }
    }
    if ($process.WaitForExit(1200)) {
        Remove-Item -LiteralPath $script:WorkerProcess
        throw "Worker exited during startup. Check $stderr and confirm the enrolled token has not been revoked."
    }
    Write-Output "Worker process started (PID $($process.Id)). Confirm the node appears online in the center dashboard. Logs: $stderr"
} finally { $lock.Dispose() }
