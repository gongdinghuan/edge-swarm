. "$PSScriptRoot/Worker.Common.ps1"
if (-not (Test-Path -LiteralPath $script:WorkerState -PathType Container)) { return }
$lock = Enter-WorkerLock
try {
    $process = Get-WorkerProcess
    if ($null -ne $process -and -not $process.HasExited) {
        try { $process.Kill() } catch { if (-not $process.HasExited) { throw } }
        if (-not $process.WaitForExit(10000)) { throw 'Worker did not stop within ten seconds.' }
    }
    if (Test-Path -LiteralPath $script:WorkerProcess -PathType Leaf) { Remove-Item -LiteralPath $script:WorkerProcess }
    Write-Output 'Worker stopped. Configuration and logs are retained.'
} finally { $lock.Dispose() }
