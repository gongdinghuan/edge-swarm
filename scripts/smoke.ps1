param([int]$ControlPort = 18080, [int]$ModelPort = 18081)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
Set-Location -LiteralPath $projectRoot
$goExe = Join-Path $projectRoot '.tools/go/bin/go.exe'
if (-not (Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
New-Item -ItemType Directory -Force 'bin' | Out-Null
& $goExe build -o bin/ ./cmd/...
if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
$runDir = Join-Path $projectRoot ('data/smoke-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $runDir | Out-Null
$admin = [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N')
$apiKey = [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N')
$keyMap = @{}; $keyMap[$apiKey] = 'demo'
$savedEnv = @{}
foreach ($name in @('SWARM_ADMIN_TOKEN','SWARM_API_KEYS','SWARM_NODE_ID','SWARM_NODE_TOKEN','SWARM_DATABASE_URL')) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name,'Process') }
$processes = @()
try {
    $env:SWARM_ADMIN_TOKEN = $admin
    $env:SWARM_API_KEYS = $keyMap | ConvertTo-Json -Compress
    $env:SWARM_DATABASE_URL = ''
    # Each child runs hidden; only processes started here are stopped below.
    $processes += Start-Process -FilePath (Join-Path $projectRoot 'bin/control.exe') -ArgumentList @('-listen',"127.0.0.1:$ControlPort",'-dsn','swarm.db') -WorkingDirectory $runDir -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $runDir 'control.log') -RedirectStandardError (Join-Path $runDir 'control.err')
    $processes += Start-Process -FilePath (Join-Path $projectRoot 'bin/simulator.exe') -ArgumentList @('-listen',"127.0.0.1:$ModelPort") -WorkingDirectory $runDir -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $runDir 'simulator.log') -RedirectStandardError (Join-Path $runDir 'simulator.err')
    $base = "http://127.0.0.1:$ControlPort"
    $ready = $false
    for ($attempt = 0; $attempt -lt 50; $attempt++) {
        try { $null = Invoke-RestMethod "$base/healthz"; $ready = $true; break } catch { Start-Sleep -Milliseconds 100 }
    }
    if (-not $ready) { throw 'Control did not become healthy' }
    $identity = Invoke-RestMethod "$base/swarm/v1/nodes/enroll" -Method Post -Headers @{Authorization="Bearer $admin"} -ContentType 'application/json' -Body '{"model":"demo"}'
    $env:SWARM_NODE_ID = $identity.node_id
    $env:SWARM_NODE_TOKEN = $identity.token
    $processes += Start-Process -FilePath (Join-Path $projectRoot 'bin/agent.exe') -ArgumentList @('-control',$base,'-model','demo','-backend',"http://127.0.0.1:$ModelPort/v1") -WorkingDirectory $runDir -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $runDir 'agent.log') -RedirectStandardError (Join-Path $runDir 'agent.err')
    $headers = @{Authorization="Bearer $apiKey"}
    $chat = Invoke-RestMethod "$base/v1/chat/completions" -Method Post -Headers $headers -ContentType 'application/json' -Body '{"model":"demo","messages":[{"role":"user","content":"hello"}],"max_tokens":32}' -TimeoutSec 15
    if ($chat.choices[0].message.content -notlike '*SIMULATED*') { throw 'Unexpected chat result' }
    $job = Invoke-RestMethod "$base/swarm/v1/batches" -Method Post -Headers $headers -ContentType 'application/json' -Body '{"model":"demo","messages":[{"role":"user","content":"batch"}],"max_tokens":32}'
    for ($attempt = 0; $attempt -lt 100; $attempt++) {
        $status = Invoke-RestMethod "$base/swarm/v1/jobs/$($job.id)" -Headers $headers
        if ($status.status -eq 'succeeded') { break }
        if ($status.status -in @('failed','interrupted','cancelled')) { throw "Batch ended: $($status.status)" }
        Start-Sleep -Milliseconds 100
    }
    if ($status.status -ne 'succeeded') { throw 'Batch did not complete' }
    [pscustomobject]@{Chat=$chat.choices[0].message.content;Batch=$status.status;Model='SIMULATED: no GPU used';Logs=$runDir} | ConvertTo-Json
} finally {
    foreach ($process in $processes) { if (-not $process.HasExited) { Stop-Process -Id $process.Id -ErrorAction SilentlyContinue } }
    foreach ($name in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($name,$savedEnv[$name],'Process') }
}
