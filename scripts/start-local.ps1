param(
    [ValidateRange(1,65535)][int]$ControlPort = 18080,
    [ValidateRange(1,65535)][int]$ModelPort = 18081,
    [string]$BackendUrl,
    [string]$Model,
    [string]$StateDirectory,
    [switch]$SkipBuild,
    [switch]$LocalGpu
)

. "$PSScriptRoot/local-common.ps1"
if ([Environment]::OSVersion.Platform -ne 'Win32NT') { throw 'This launcher requires Windows.' }

if ($LocalGpu -and ($BackendUrl -or $Model)) { throw '-LocalGpu uses a fixed local backend and model alias. Omit -BackendUrl and -Model.' }
$useSimulator = -not $BackendUrl -and -not $LocalGpu
$gpuRuntime = $null
if ($LocalGpu) {
    $gpuRuntime = Get-SwarmGpuRuntime
    if (-not $StateDirectory) { $StateDirectory = Join-Path $script:ProjectRoot 'data/local-gpu' }
    $Model = 'qwen-05b'
    $BackendUrl = "http://127.0.0.1:$ModelPort/v1"
}
if ($useSimulator) {
    if ($ControlPort -eq $ModelPort) { throw 'ControlPort and ModelPort must differ.' }
    if (-not $Model) { $Model = 'demo' }
    $BackendUrl = "http://127.0.0.1:$ModelPort/v1"
} elseif (-not $Model) {
    throw 'Specify -Model matching the backend served model name.'
}
if ($LocalGpu -and $ControlPort -eq $ModelPort) { throw 'ControlPort and ModelPort must differ.' }
if ($Model -notmatch '^[A-Za-z0-9][A-Za-z0-9._/-]{0,119}$') { throw 'Invalid model name.' }
$backend = [uri]$BackendUrl
if (-not $backend.IsAbsoluteUri -or $backend.Scheme -notin @('http','https') -or
    -not $backend.IsLoopback -or $backend.UserInfo -or $backend.Query -or $backend.Fragment -or
    $BackendUrl -match '[\s"\\]') {
    throw 'BackendUrl must be a local HTTP(S) URL without user info, query or fragment.'
}

$state = Resolve-SwarmStateDirectory $StateDirectory
$null = New-Item -ItemType Directory -Force -Path $state
$lock = Enter-SwarmLock $state
$processFile = Join-Path $state 'processes.json'
$credentialFile = Join-Path $state 'credentials.json'
$identityFile = Join-Path $state 'identity.json'
$processes = @()
$complete = $false
$secretNames = @('SWARM_ADMIN_TOKEN','SWARM_API_KEYS','SWARM_NODE_ID','SWARM_NODE_TOKEN','SWARM_DATABASE_URL','SWARM_BACKEND_KEY','LLAMA_API_KEY')
$backendKey = [Environment]::GetEnvironmentVariable('SWARM_BACKEND_KEY','Process')
if ($LocalGpu) { $backendKey = New-SwarmKey }

function Start-SwarmLocalProcess([string]$Role, [string[]]$ProcessArguments, [hashtable]$Variables) {
    $exe = Get-SwarmExecutable $Role
    $original = @{}
    foreach ($name in $secretNames) { $original[$name] = [Environment]::GetEnvironmentVariable($name,'Process') }
    try {
        foreach ($name in $secretNames) { [Environment]::SetEnvironmentVariable($name, $Variables[$name], 'Process') }
        $process = Start-Process -FilePath $exe -ArgumentList $ProcessArguments -WorkingDirectory $state `
            -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $state "$Role.log") `
            -RedirectStandardError (Join-Path $state "$Role.err")
        return [pscustomobject]@{
            role = $Role
            pid = $process.Id
            path = [IO.Path]::GetFullPath($exe)
            started = $process.StartTime.ToUniversalTime().Ticks.ToString()
        }
    } finally {
        foreach ($name in $secretNames) { [Environment]::SetEnvironmentVariable($name, $original[$name], 'Process') }
    }
}

function Wait-SwarmLocal([scriptblock]$Check, [string]$Failure, [int]$TimeoutSeconds = 20) {
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        foreach ($record in $processes) {
            if (-not (Get-SwarmProcess $record)) {
                throw "$($record.role) exited during startup. Check $($record.role).err in $state for the failure details."
            }
        }
        try { if (& $Check) { return } } catch { }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Failure Check logs in $state."
}

try {
    if (Test-Path -LiteralPath $processFile) {
        $previous = Get-Content -LiteralPath $processFile -Encoding UTF8 -Raw | ConvertFrom-Json
        foreach ($record in @($previous.processes)) {
            if (Get-SwarmProcess $record) { throw 'A local stack process is running. Run stop-local.ps1 for the same state directory.' }
        }
    }

    if (-not $SkipBuild) {
        $go = Join-Path $script:ProjectRoot '.tools/go/bin/go.exe'
        if (-not (Test-Path -LiteralPath $go)) { $go = (Get-Command go -ErrorAction Stop).Source }
        Push-Location -LiteralPath $script:ProjectRoot
        try {
            $null = New-Item -ItemType Directory -Force -Path 'bin'
            & $go build -o bin/ ./cmd/control ./cmd/agent ./cmd/simulator
            if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
        } finally { Pop-Location }
    }

    $roles = @('control','agent')
    if ($useSimulator) { $roles += 'simulator' }
    foreach ($role in $roles) {
        if (-not (Test-Path -LiteralPath (Join-Path $script:ProjectRoot "bin/$role.exe") -PathType Leaf)) {
            throw "Missing bin/$role.exe. Build without -SkipBuild."
        }
    }

    # Ensure an unrelated listener cannot satisfy readiness or receive credentials.
    $ports = @($ControlPort)
    if ($useSimulator -or $LocalGpu) { $ports += $ModelPort }
    foreach ($port in $ports) {
        $probe = New-Object Net.Sockets.TcpListener ([Net.IPAddress]::Loopback), $port
        try { $probe.Start() } catch { throw "Local port $port is already in use." }
        finally { $probe.Stop() }
    }

    if (-not (Test-Path -LiteralPath $credentialFile)) {
        Save-SwarmJson $credentialFile @{
            admin = Protect-SwarmValue (New-SwarmKey)
            api_key = Protect-SwarmValue (New-SwarmKey)
        }
    }
    $credentials = Get-Content -LiteralPath $credentialFile -Encoding UTF8 -Raw | ConvertFrom-Json
    $admin = Unprotect-SwarmValue $credentials.admin
    $apiKey = Unprotect-SwarmValue $credentials.api_key
    $keyMap = @{}
    $keyMap[$apiKey] = 'local'
    $base = "http://127.0.0.1:$ControlPort"

    $processes += Start-SwarmLocalProcess 'control' @('-listen', "127.0.0.1:$ControlPort", '-dsn', 'swarm.db') @{
        SWARM_ADMIN_TOKEN = $admin
        SWARM_API_KEYS = ConvertTo-Json -InputObject $keyMap -Compress
    }
    Save-SwarmJson $processFile @{ processes = $processes }
    Wait-SwarmLocal { (Invoke-RestMethod "$base/healthz" -TimeoutSec 2).status -eq 'ok' } 'Control did not become healthy.'

    if ($useSimulator) {
        $processes += Start-SwarmLocalProcess 'simulator' @('-listen', "127.0.0.1:$ModelPort", '-model', $Model) @{}
        Save-SwarmJson $processFile @{ processes = $processes }
        Wait-SwarmLocal {
            $connection = New-Object Net.Sockets.TcpClient
            try { $connection.Connect('127.0.0.1', $ModelPort); $true } finally { $connection.Dispose() }
        } 'Simulator did not open its local port.'
    }

    if ($LocalGpu) {
        $modelArgument = '"' + $gpuRuntime.modelPath + '"'
        $processes += Start-SwarmLocalProcess 'llama-server' @(
            '--host','127.0.0.1','--port',"$ModelPort",'-m',$modelArgument,
            '--alias',$Model,'--device','Vulkan0','-ngl','99','-c','2048','-np','1','-lv','5'
        ) @{ LLAMA_API_KEY = $backendKey }
        Save-SwarmJson $processFile @{ processes = $processes }
        Wait-SwarmLocal {
            (Invoke-RestMethod "http://127.0.0.1:$ModelPort/health" -Headers @{ Authorization = "Bearer $backendKey" } -TimeoutSec 2).status -eq 'ok'
        } 'Local Vulkan model did not become healthy.' 45
        # Health may become ready before the child's redirected stderr has
        # flushed. A dry-run can report layers but a zero-byte model buffer.
        Wait-SwarmLocal {
            $gpuLog = Get-Content -LiteralPath (Join-Path $state 'llama-server.err') -Raw
            $gpuLog -match 'offloaded [1-9][0-9]*/[0-9]+ layers to GPU' -and
                $gpuLog -match 'Vulkan0 model buffer size\s*=\s*[1-9][0-9]*\.[0-9]+ MiB'
        } 'The local model did not report a populated Vulkan model buffer.' 20
    }

    $identity = $null
    if (Test-Path -LiteralPath $identityFile) {
        $identity = Get-Content -LiteralPath $identityFile -Encoding UTF8 -Raw | ConvertFrom-Json
        if ($identity.model -ne $Model) { throw 'This state directory is enrolled for another model. Use a different -StateDirectory.' }
    }
    if ($null -eq $identity) {
        $registration = Invoke-RestMethod "$base/swarm/v1/nodes/enroll" -Method Post -TimeoutSec 10 `
            -Headers @{ Authorization = "Bearer $admin" } -ContentType 'application/json' `
            -Body (ConvertTo-Json -InputObject @{ model = $Model } -Compress)
        $identity = @{ node_id = $registration.node_id; token = Protect-SwarmValue $registration.token; model = $Model }
        Save-SwarmJson $identityFile $identity
    }

    $agentArguments = @('-control', $base, '-model', $Model, '-backend', $BackendUrl)
    if ($LocalGpu) {
        $agentArguments += @('-backend-kind','llama.cpp')
        $deviceLog = Get-Content -LiteralPath (Join-Path $state 'llama-server.err') -Raw
        $device = [regex]::Match($deviceLog, '(?m)\bVulkan0\s*:\s*([A-Za-z0-9 ._()/+-]{1,160}?)\s+\(([0-9]+) MiB,')
        if ($device.Success) {
            $agentArguments += @('-gpu',('"' + $device.Groups[1].Value.Trim() + '"'),'-vram-mib',$device.Groups[2].Value)
        }
    }
    $processes += Start-SwarmLocalProcess 'agent' $agentArguments @{
        SWARM_NODE_ID = $identity.node_id
        SWARM_NODE_TOKEN = Unprotect-SwarmValue $identity.token
        SWARM_BACKEND_KEY = $backendKey
    }
    Save-SwarmJson $processFile @{ processes = $processes }
    Wait-SwarmLocal {
        $models = Invoke-RestMethod "$base/v1/models" -Headers @{ Authorization = "Bearer $apiKey" } -TimeoutSec 2
        @($models.data.id) -contains $Model
    } 'Agent did not register as ready.'

    $complete = $true
    [pscustomobject]@{ endpoint = $base; model = $Model; simulated = $useSimulator; local_gpu = [bool]$LocalGpu; state_directory = $state } | ConvertTo-Json
} finally {
    try {
        if (-not $complete -and $processes.Count -gt 0) {
            try { Stop-SwarmProcessSet $processes $processFile }
            catch { Write-Warning $_.Exception.Message }
        }
    } finally { $lock.Dispose() }
}
