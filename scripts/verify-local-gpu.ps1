param([int]$ControlPort = 18080, [int]$ModelPort = 18081, [switch]$KeepRunning)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$state = if ($KeepRunning) { Join-Path $root 'data/local-gpu' } else { Join-Path $root ('data/gpu-verify-' + [guid]::NewGuid().ToString('N')) }
$launch = @{ LocalGpu = $true; ControlPort = $ControlPort; ModelPort = $ModelPort; StateDirectory = $state }
$owned = $false
$passed = $false
try {
    $null = & "$PSScriptRoot/start-local.ps1" @launch
    $owned = $true
    $base = "http://127.0.0.1:$ControlPort"
    $business = & "$PSScriptRoot/local-credentials.ps1" -LocalGpu -StateDirectory $state
    $admin = & "$PSScriptRoot/local-credentials.ps1" -LocalGpu -StateDirectory $state -Admin
    $apiHeaders = @{ Authorization = "Bearer $business" }
    $adminHeaders = @{ Authorization = "Bearer $admin" }
    $request = @{ model = 'qwen-05b'; messages = @(@{ role='user'; content='What is the capital of France? Answer with one word.' }); max_tokens=16; temperature=0 }
    $answer = Invoke-RestMethod "$base/v1/chat/completions" -Method Post -Headers $apiHeaders -ContentType 'application/json' -Body ($request | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 45
    if ($answer.choices[0].message.content -notmatch 'Paris') { throw 'Expected a real Qwen reply containing Paris.' }
    $request.stream = $true
    $stream = Invoke-WebRequest "$base/v1/chat/completions" -UseBasicParsing -Method Post -Headers $apiHeaders -ContentType 'application/json' -Body ($request | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 45
    if ($stream.Content -notmatch '\[DONE\]' -or $stream.Content -notmatch 'Paris') { throw 'Stream did not deliver a complete real-model result.' }
    $request.stream = $false
    $group = Invoke-RestMethod "$base/swarm/v1/batches" -Method Post -Headers $apiHeaders -ContentType 'application/json' -Body (@{ items=@($request,$request) } | ConvertTo-Json -Depth 8 -Compress) -TimeoutSec 10
    $end = (Get-Date).AddSeconds(60)
    do {
        $batch = Invoke-RestMethod "$base/swarm/v1/batches/$($group.id)" -Headers $apiHeaders -TimeoutSec 10
        if ($batch.status -eq 'succeeded') { break }
        if ($batch.status -in @('cancelled','completed_with_errors')) { throw "Batch ended: $($batch.status)" }
        Start-Sleep -Milliseconds 200
    } while ((Get-Date) -lt $end)
    if ($batch.status -ne 'succeeded' -or $batch.jobs.Count -ne 2) { throw 'Two-item batch did not succeed.' }
    $null = & "$PSScriptRoot/stop-local.ps1" -LocalGpu -StateDirectory $state
    $owned = $false
    # Read the redirected stream after Windows has closed the writer handle.
    $logPath = Join-Path $state 'llama-server.err'
    $logText = [IO.File]::ReadAllText($logPath)
    $layerRecord = [regex]::Match($logText, 'offloaded ([1-9][0-9]*)/([0-9]+) layers to GPU')
    if (-not $layerRecord.Success) { throw 'No GPU offload evidence.' }
    $offload = "$($layerRecord.Groups[1].Value)/$($layerRecord.Groups[2].Value)"
    $null = & "$PSScriptRoot/start-local.ps1" @launch -SkipBuild
    $owned = $true
    $afterRestart = Invoke-RestMethod "$base/swarm/v1/batches/$($group.id)" -Headers $apiHeaders -TimeoutSec 10
    if ($afterRestart.status -ne 'succeeded' -or $afterRestart.jobs.Count -ne 2) { throw 'Batch did not survive stack restart.' }
    $nodes = Invoke-RestMethod "$base/swarm/v1/nodes" -Headers $adminHeaders -TimeoutSec 10
    $ready = $nodes.data | Where-Object { $_.ready -and $_.model -eq 'qwen-05b' } | Select-Object -First 1
    if (-not $ready) { throw 'GPU node did not come back after restart.' }
    $passed = $true
    [pscustomobject]@{ chat=$answer.choices[0].message.content; stream_done=$true; batch_jobs=2; batch_persisted=$true; gpu=$ready.gpu; layers_offloaded=$offload; console=$base; running=[bool]$KeepRunning; state_directory=$state } | ConvertTo-Json
} finally {
    if ($owned -and (-not $KeepRunning -or -not $passed)) { & "$PSScriptRoot/stop-local.ps1" -LocalGpu -StateDirectory $state }
}
