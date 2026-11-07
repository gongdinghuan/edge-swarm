$ErrorActionPreference = 'Stop'
$script:WorkerRoot = [IO.Path]::GetFullPath($PSScriptRoot)
$script:WorkerExe = Join-Path $script:WorkerRoot 'bin/agent.exe'
$script:WorkerState = Join-Path $script:WorkerRoot 'state'
$script:WorkerConfig = Join-Path $script:WorkerState 'worker.json'
$script:WorkerProcess = Join-Path $script:WorkerState 'process.json'

function Assert-WorkerExecutable {
    if (-not (Test-Path -LiteralPath $script:WorkerExe -PathType Leaf)) {
        throw 'Missing bin/agent.exe. Extract the full Windows worker package before running this script.'
    }
}

function Enter-WorkerLock {
    try {
        [IO.File]::Open((Join-Path $script:WorkerState 'worker.lock'), 'OpenOrCreate', 'ReadWrite', 'None')
    } catch [IO.IOException] {
        throw 'Worker state is locked. Wait for the other install/start/stop command to finish.'
    }
}

function Save-WorkerJson([string]$Path, $Value) {
    $temp = "$Path.tmp"
    [IO.File]::WriteAllText($temp, (ConvertTo-Json -InputObject $Value -Depth 5))
    if ([IO.File]::Exists($Path)) {
        [IO.File]::Replace($temp, $Path, [NullString]::Value)
    } else {
        [IO.File]::Move($temp, $Path)
    }
}

function Get-WorkerProcess {
    if (-not (Test-Path -LiteralPath $script:WorkerProcess -PathType Leaf)) { return }
    $record = Get-Content -LiteralPath $script:WorkerProcess -Encoding UTF8 -Raw | ConvertFrom-Json
    if ($record.path -ne $script:WorkerExe -or $record.pid -le 0) {
        throw 'The process record does not belong to this package. Verify state/process.json before proceeding.'
    }
    $process = Get-Process -Id $record.pid -ErrorAction SilentlyContinue
    if ($null -ne $process -and $process.Path -eq $script:WorkerExe -and
        $process.StartTime.ToUniversalTime().Ticks.ToString() -eq $record.started) {
        $process
    }
}

function Get-WorkerUrl([string]$Value, [string]$Name) {
    $uri = $null
    if (-not [uri]::TryCreate($Value.Trim(), [UriKind]::Absolute, [ref]$uri) -or
        $uri.Scheme -notin @('http','https') -or -not $uri.Host -or $uri.UserInfo -or
        $uri.Query -or $uri.Fragment -or $Value -match '["\\\x00-\x1f]') {
        throw "$Name must be an absolute HTTP(S) URL without credentials, a query or a fragment."
    }
    if ($Name -eq 'ControlUrl' -and $uri.Scheme -eq 'http' -and -not $uri.IsLoopback) {
        throw 'A remote ControlUrl must use HTTPS. HTTP is supported for localhost only.'
    }
    $uri.AbsoluteUri.TrimEnd('/')
}

function Assert-WorkerIdentity([string]$Id, [string]$ModelName) {
    if ($Id -notmatch '^[0-9a-f]{32}$') { throw 'Node ID must be the 32-character ID returned when the node was enrolled.' }
    if ($ModelName -notmatch '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$') {
        throw 'Model must be the registered model alias using letters, digits, or . _ : / -.'
    }
}

function Get-WorkerPlaintext([Security.SecureString]$Value) {
    $buffer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($Value)
    try { [Runtime.InteropServices.Marshal]::PtrToStringBSTR($buffer) }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($buffer) }
}

function Read-WorkerSecret([string]$Ciphertext) {
    try { Get-WorkerPlaintext (ConvertTo-SecureString -String $Ciphertext) }
    catch { throw 'Stored credentials cannot be decrypted. Run Install-Worker.ps1 as the Windows user who will run this worker.' }
}

function Get-WorkerHttpStatus($Failure) {
    if ($null -ne $Failure.Exception.Response) { return [int]$Failure.Exception.Response.StatusCode }
    return 0
}

function Test-WorkerDependencies([string]$ControlUrl, [string]$BackendUrl, [string]$Model, [string]$BackendKey) {
    try {
        $ready = Invoke-RestMethod -UseBasicParsing -Uri "$ControlUrl/readyz" -TimeoutSec 6 -MaximumRedirection 0
    } catch {
        $status = Get-WorkerHttpStatus $_
        if ($status) { throw "Center readiness endpoint returned HTTP $status. Verify ControlUrl and wait for the center to become ready." }
        throw 'Cannot reach the center readiness endpoint. Check DNS, HTTPS certificate trust, firewall and outbound access.'
    }
    if ($ready.status -ne 'ready') { throw 'Unexpected center readiness response. Use the base URL of the Swarm control service.' }

    $headers = @{}
    if ($BackendKey) { $headers.Authorization = "Bearer $BackendKey" }
    try {
        $models = Invoke-RestMethod -UseBasicParsing -Uri "$BackendUrl/models" -Headers $headers -TimeoutSec 6 -MaximumRedirection 0
    } catch {
        $status = Get-WorkerHttpStatus $_
        if ($status -in @(401,403)) { throw 'The backend rejected its API key. Run Install-Worker.ps1 with the correct backend key.' }
        if ($status) { throw "Backend model discovery returned HTTP $status. Use its OpenAI-compatible base URL (normally ending in /v1)." }
        throw 'Cannot reach the backend model list. Start the model server and check its address, port and TLS certificate.'
    }
    if (@($models.data.id) -cnotcontains $Model) { throw "Backend /models does not list '$Model'. Configure the same served alias on the backend and node enrollment." }
}
