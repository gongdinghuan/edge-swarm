param([string]$Version = '0.1.1', [switch]$IncludeGpu)
$ErrorActionPreference = 'Stop'
$project = Split-Path -Parent $PSScriptRoot
$fixtureLabel = 'ZIP ' + [char]0x9A8C + [char]0x6536
$fixture = Join-Path (Join-Path $project 'data') ($fixtureLabel + ' ' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $fixture
function Expand-Bundle([string]$Role) {
    $name = "Swarm-$Role-$Version-windows-amd64"
    $archive = Join-Path $project "dist/windows/$name.zip"
    $expected = (Get-Content -LiteralPath "$archive.sha256" -Raw).Trim().Split(' ')[0]
    if ((Get-FileHash -LiteralPath $archive).Hash -ne $expected) { throw "$Role ZIP checksum mismatch." }
    Expand-Archive -LiteralPath $archive -DestinationPath $fixture
    $path = Join-Path $fixture $name
    & (Join-Path $path 'Verify-Package.ps1') | Out-Null
    $path
}
function Expect-Failure([scriptblock]$Operation, [string]$Pattern) {
    try { & $Operation | Out-Null }
    catch {
        if ($_.Exception.Message -notmatch $Pattern) { throw }
        return
    }
    throw "Expected failure matching $Pattern."
}

$manager = Expand-Bundle 'manager'
$worker = Expand-Bundle 'worker'
$managerEntry = Join-Path $manager 'manager.ps1'
$workerInstall = Join-Path $worker 'Install-Worker.ps1'
$workerStart = Join-Path $worker 'Start-Worker.ps1'
$workerStop = Join-Path $worker 'Stop-Worker.ps1'
$controlPort = 19880
$backendPort = 19881
$base = "http://127.0.0.1:$controlPort"
$backendBase = "http://127.0.0.1:$backendPort/v1"
$child = $null
$gpu = $null
$managerStarted = $false
$workerStarted = $false
$gpuStarted = $false
try {
    $controlExe = Join-Path $manager 'bin/control.exe'
    Move-Item -LiteralPath $controlExe -Destination "$controlExe.test-backup"
    try { Expect-Failure { & $managerEntry start -ControlPort $controlPort -NoBrowser } 'PACKAGE_MISSING.*control.exe' }
    finally { Move-Item -LiteralPath "$controlExe.test-backup" -Destination $controlExe }

    $occupied = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $controlPort)
    try {
        $occupied.Start()
        Expect-Failure { & $managerEntry start -ControlPort $controlPort -NoBrowser } 'Cannot bind 127\.0\.0\.1:'
    } finally { $occupied.Stop() }

    & $managerEntry start -ControlPort $controlPort -NoBrowser | Out-Null
    $managerStarted = $true
    $admin = & $managerEntry credentials -Admin
    $business = & $managerEntry credentials
    $registration = Invoke-RestMethod "$base/swarm/v1/nodes/enroll" -Method Post -Headers @{ Authorization = "Bearer $admin" } `
        -ContentType application/json -Body '{"model":"demo"}' -TimeoutSec 5
    $nodeToken = ConvertTo-SecureString $registration.token -AsPlainText -Force
    & $workerInstall -ControlUrl $base -NodeId $registration.node_id -Model 'demo' -BackendUrl $backendBase -NodeToken $nodeToken -NoBackendKey | Out-Null
    Expect-Failure { & $workerStart -Check } 'Cannot reach the backend model list'
    if (Test-Path -LiteralPath (Join-Path $worker 'state/process.json')) { throw 'Failed preflight left a worker process record.' }

    $simulator = Join-Path $project 'bin/simulator.exe'
    $child = Start-Process -FilePath $simulator -ArgumentList @('-listen', "127.0.0.1:$backendPort", '-model', 'demo') `
        -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $fixture 'sim.log') -RedirectStandardError (Join-Path $fixture 'sim.err')
    for ($attempt = 0; $attempt -lt 20; $attempt++) {
        try { $null = Invoke-RestMethod "$backendBase/models" -TimeoutSec 1; break }
        catch { if ($child.HasExited -or $attempt -eq 19) { throw }; Start-Sleep -Milliseconds 100 }
    }
    & $workerStart -Check | Out-Null
    & $workerStart | Out-Null
    $workerStarted = $true
    $answer = Invoke-RestMethod "$base/v1/chat/completions" -Method Post -Headers @{ Authorization = "Bearer $business" } `
        -ContentType application/json -Body '{"model":"demo","messages":[{"role":"user","content":"packaging fixture"}]}' -TimeoutSec 15
    if ($answer.choices[0].message.content -notmatch '\[SIMULATED\]') { throw 'Packaged worker did not serve the fixture request.' }
    & $workerStop | Out-Null
    $workerStarted = $false
    & $managerEntry stop | Out-Null
    $managerStarted = $false
    & $managerEntry start -ControlPort $controlPort -NoBrowser | Out-Null
    $managerStarted = $true
    if ((& $managerEntry credentials) -ne $business) { throw 'Manager credentials changed after restart.' }

    if ($IncludeGpu) {
        $gpu = Expand-Bundle 'gpu'
        $gpuEntry = Join-Path $gpu 'Start-Gpu.ps1'
        if (Test-Path -LiteralPath (Join-Path $gpu 'Gpu.Dependencies.ps1') -PathType Leaf) {
            $preflightState = Join-Path $fixture 'gpu-check-state'
            & $gpuEntry -Check -StateDirectory $preflightState | Out-Null
            if (Test-Path -LiteralPath $preflightState) { throw 'GPU dependency check created a runtime state directory.' }
        }
        $gpuDll = Join-Path $gpu '.tools/gpu-smoke/llama/ggml-vulkan.dll'
        Move-Item -LiteralPath $gpuDll -Destination "$gpuDll.test-backup"
        try { Expect-Failure { & $gpuEntry -ControlPort 19882 -ModelPort 19883 } 'PACKAGE_MISSING.*ggml-vulkan.dll' }
        finally { Move-Item -LiteralPath "$gpuDll.test-backup" -Destination $gpuDll }
        & $gpuEntry -ControlPort 19882 -ModelPort 19883 | Out-Null
        $gpuStarted = $true
        $key = & (Join-Path $gpu 'scripts/local-credentials.ps1') -LocalGpu
        $answer = Invoke-RestMethod 'http://127.0.0.1:19882/v1/chat/completions' -Method Post -Headers @{ Authorization = "Bearer $key" } `
            -ContentType application/json -Body '{"model":"qwen-05b","messages":[{"role":"user","content":"What is the capital of France? Answer with one word."}],"max_tokens":16,"temperature":0}' -TimeoutSec 35
        if ($answer.choices[0].message.content -notmatch 'Paris') { throw 'GPU package inference failed.' }
    }
    [pscustomobject]@{ ManagerZip = 'passed'; WorkerZip = 'passed'; GpuZip = if ($IncludeGpu) { 'passed' } else { 'skipped' }; State = $fixture }
} finally {
    $cleanupFailures = @()
    foreach ($cleanup in @(
        { if ($gpuStarted) { & (Join-Path $gpu 'Stop-Gpu.ps1') | Out-Null } },
        { if ($workerStarted) { & $workerStop | Out-Null } },
        { if ($child -and -not $child.HasExited) { Stop-Process -Id $child.Id } },
        { if ($managerStarted) { & $managerEntry stop | Out-Null } }
    )) {
        try { & $cleanup }
        catch { $cleanupFailures += $_.Exception.Message }
    }
    if ($cleanupFailures.Count) { throw "Package test cleanup failed. $($cleanupFailures -join ' | ')" }
}
