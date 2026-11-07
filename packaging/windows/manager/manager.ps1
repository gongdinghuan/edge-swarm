param(
    [Parameter(Position = 0)]
    [ValidateSet('start', 'stop', 'status', 'credentials', 'help')]
    [string]$Action = 'start',
    [ValidateRange(1, 65535)][int]$ControlPort = 18080,
    [string]$StateDirectory,
    [ValidateRange(1, 120)][int]$TimeoutSeconds = 20,
    [switch]$NoBrowser,
    [switch]$Admin,
    [switch]$Copy
)

$ErrorActionPreference = 'Stop'
if ($Action -eq 'help') {
    Write-Output @'
manager.cmd [start|stop|status|credentials|help]
  start [-ControlPort 18080] [-NoBrowser] [-TimeoutSeconds 20]
  stop
  status
  credentials [-Admin] [-Copy]
  Any command: -StateDirectory <path>
Run credentials without -Admin for the business key. Add -Copy to avoid printing it.
'@
    return
}
if ([Environment]::OSVersion.Platform -ne 'Win32NT') { throw 'This launcher requires Windows.' }

$controlExe = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot 'bin/control.exe'))
if (-not $StateDirectory) { $StateDirectory = Join-Path $PSScriptRoot 'data/manager' }
$state = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($StateDirectory)
$processFile = Join-Path $state 'process.json'
$credentialFile = Join-Path $state 'credentials.json'

function Save-ManagerJson([string]$Path, $Value) {
    $temp = "$Path.tmp"
    [IO.File]::WriteAllText($temp, (ConvertTo-Json -InputObject $Value -Compress))
    if ([IO.File]::Exists($Path)) {
        [IO.File]::Replace($temp, $Path, [NullString]::Value)
    } else {
        [IO.File]::Move($temp, $Path)
    }
}

function New-ManagerKey {
    $bytes = New-Object byte[] 32
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    -join ($bytes | ForEach-Object { $_.ToString('x2') })
}

function Protect-ManagerKey([string]$Key) {
    Add-Type -AssemblyName System.Security
    [Convert]::ToBase64String([Security.Cryptography.ProtectedData]::Protect(
        [Text.Encoding]::UTF8.GetBytes($Key), $null, [Security.Cryptography.DataProtectionScope]::CurrentUser))
}

function Unprotect-ManagerKey([string]$Value) {
    Add-Type -AssemblyName System.Security
    $bytes = [Security.Cryptography.ProtectedData]::Unprotect(
        [Convert]::FromBase64String($Value), $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)
    [Text.Encoding]::UTF8.GetString($bytes)
}

function Read-ManagerKeys {
    if (-not [IO.File]::Exists($credentialFile)) {
        throw 'No local credentials found. Start the manager first.'
    }
    try {
        $saved = [IO.File]::ReadAllText($credentialFile) | ConvertFrom-Json
        $keys = @{
            admin = Unprotect-ManagerKey $saved.admin
            business = Unprotect-ManagerKey $saved.business
        }
        if ($keys.admin -notmatch '^[a-f0-9]{64}$' -or $keys.business -notmatch '^[a-f0-9]{64}$' -or
            $keys.admin -eq $keys.business) { throw 'Invalid key format.' }
        return $keys
    } catch {
        throw "Cannot decrypt credentials in $credentialFile. Use the original Windows account and machine."
    }
}

function Read-ManagerRecord {
    if (-not [IO.File]::Exists($processFile)) { return }
    $record = [IO.File]::ReadAllText($processFile) | ConvertFrom-Json
    if ($record.path -ne $controlExe -or $record.pid -lt 1 -or
        $record.port -lt 1 -or $record.port -gt 65535 -or $record.started -notmatch '^\d+$') {
        throw "Unexpected process state in $processFile. Confirm no manager is running before removing it."
    }
    return $record
}

function Find-ManagerProcess($Record) {
    if ($null -eq $Record) { return }
    $found = Get-Process -Id $Record.pid -ErrorAction SilentlyContinue
    if ($found -and $found.Path -eq $controlExe) {
        try {
            # Hold the process handle so stopping never targets a recycled PID.
            $null = $found.Handle
            if (-not $found.HasExited -and $found.StartTime.ToUniversalTime().Ticks.ToString() -eq $Record.started) {
                return $found
            }
        } catch [InvalidOperationException] { }
    }
}

function Stop-ManagerProcess([Diagnostics.Process]$Process) {
    if ($null -eq $Process -or $Process.HasExited) { return }
    try { $Process.Kill() } catch { if (-not $Process.HasExited) { throw } }
    if (-not $Process.WaitForExit(10000)) { throw 'The manager did not stop within 10 seconds.' }
}

function Assert-ManagerPortFree([int]$Port) {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $Port)
    try {
        $listener.ExclusiveAddressUse = $true
        $listener.Start()
    } catch {
        throw "Cannot bind 127.0.0.1:${Port}. Check for another listener or choose -ControlPort."
    } finally { $listener.Stop() }
}

function Wait-ManagerReady([Diagnostics.Process]$Process, [int]$Port) {
    Add-Type -AssemblyName System.Net.Http
    $transport = [Net.Http.HttpClientHandler]::new()
    $transport.UseProxy = $false
    $transport.AllowAutoRedirect = $false
    $client = [Net.Http.HttpClient]::new($transport)
    $client.Timeout = [TimeSpan]::FromSeconds(1)
    $clock = [Diagnostics.Stopwatch]::StartNew()
    try {
        while ($clock.Elapsed.TotalSeconds -lt $TimeoutSeconds) {
            if ($Process.HasExited) { throw "Control exited with code $($Process.ExitCode). See $state/control.err." }
            $ready = $false
            try {
                $reply = $client.GetStringAsync("http://127.0.0.1:${Port}/readyz").GetAwaiter().GetResult()
                $ready = ($reply | ConvertFrom-Json).status -eq 'ready'
            } catch { }
            if ($ready) {
                $owner = Get-NetTCPConnection -LocalAddress 127.0.0.1 -LocalPort $Port -State Listen -ErrorAction SilentlyContinue |
                    Where-Object { $_.OwningProcess -eq $Process.Id }
                if ($owner -and -not $Process.HasExited) { return }
            }
            Start-Sleep -Milliseconds 200
        }
        throw "Control did not become ready within $TimeoutSeconds seconds. See $state/control.err."
    } finally { $client.Dispose() }
}

function Open-ManagerPage([int]$Port) {
    $url = "http://127.0.0.1:$Port/"
    Write-Output "Manager is available at $url"
    Write-Output 'Use manager.cmd credentials [-Admin] -Copy to copy a console key.'
    if (-not $NoBrowser) {
        try { Start-Process $url } catch { Write-Warning "Open $url manually in your browser." }
    }
}

if ($Action -eq 'start') {
    $verifier = Join-Path $PSScriptRoot 'Verify-Package.ps1'
    if (-not (Test-Path -LiteralPath $verifier -PathType Leaf)) { throw 'PACKAGE_MISSING: Verify-Package.ps1. Extract the complete manager ZIP again.' }
    $verified = & $verifier
    if ($verified.Package -ne 'manager') { throw 'Expected the manager package.' }
    if (-not (Test-Path -LiteralPath $controlExe -PathType Leaf)) {
        throw "Missing $controlExe. Extract the complete Windows manager package before starting."
    }
    $null = [IO.Directory]::CreateDirectory($state)
} elseif (-not [IO.Directory]::Exists($state)) {
    if ($Action -eq 'credentials') { throw 'No local credentials found. Start the manager first.' }
    Write-Output 'Manager is stopped.'
    return
}

try {
    $lock = [IO.File]::Open((Join-Path $state 'process.lock'), 'OpenOrCreate', 'ReadWrite', 'None')
} catch {
    throw "Cannot lock $state. Another manager command may be in progress."
}
try {
    if ($Action -eq 'credentials') {
        $keys = Read-ManagerKeys
        $value = if ($Admin) { $keys.admin } else { $keys.business }
        if ($Copy) {
            Set-Clipboard -Value $value
            Write-Output 'Credential copied to the clipboard.'
        } else { Write-Output $value }
        return
    }
    $record = Read-ManagerRecord
    $process = Find-ManagerProcess $record
    if ($Action -eq 'status') {
        if ($process) { Write-Output "Manager is running at http://127.0.0.1:$($record.port)/ (PID $($record.pid))." }
        else { Write-Output 'Manager is stopped.' }
        return
    }
    if ($Action -eq 'stop') {
        Stop-ManagerProcess $process
        if ([IO.File]::Exists($processFile)) { Remove-Item -LiteralPath $processFile }
        Write-Output "Manager is stopped. Data remains in $state."
        return
    }

    if ($process) {
        if ($PSBoundParameters.ContainsKey('ControlPort') -and $ControlPort -ne $record.port) {
            throw "Manager already uses port $($record.port). Stop it before changing ports."
        }
        Wait-ManagerReady $process $record.port
        Open-ManagerPage $record.port
        return
    }
    Assert-ManagerPortFree $ControlPort
    if (-not [IO.File]::Exists($credentialFile)) {
        Save-ManagerJson $credentialFile @{
            admin = Protect-ManagerKey (New-ManagerKey)
            business = Protect-ManagerKey (New-ManagerKey)
        }
    }
    $keys = Read-ManagerKeys
    $map = @{}
    $map[$keys.business] = 'local'
    $overrides = @{
        SWARM_ADMIN_TOKEN = $keys.admin
        SWARM_API_KEYS = (ConvertTo-Json -InputObject $map -Compress)
        SWARM_DATABASE_URL = $null
        SWARM_MODEL_ACCESS = $null
    }
    $oldEnvironment = @{}
    foreach ($name in $overrides.Keys) { $oldEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
    $complete = $false
    try {
        try {
            foreach ($name in $overrides.Keys) {
                [Environment]::SetEnvironmentVariable($name, $overrides[$name], 'Process')
            }
            $process = Start-Process -FilePath $controlExe -ArgumentList @('-listen', "127.0.0.1:$ControlPort", '-dsn', 'swarm.db') `
                -WorkingDirectory $state -WindowStyle Hidden -PassThru `
                -RedirectStandardOutput (Join-Path $state 'control.log') -RedirectStandardError (Join-Path $state 'control.err')
        } finally {
            foreach ($name in $oldEnvironment.Keys) {
                [Environment]::SetEnvironmentVariable($name, $oldEnvironment[$name], 'Process')
            }
        }
        Save-ManagerJson $processFile @{
            pid = $process.Id
            path = $controlExe
            started = $process.StartTime.ToUniversalTime().Ticks.ToString()
            port = $ControlPort
        }
        Wait-ManagerReady $process $ControlPort
        $complete = $true
    } finally {
        if (-not $complete) {
            Stop-ManagerProcess $process
            if ([IO.File]::Exists($processFile)) { Remove-Item -LiteralPath $processFile }
        }
    }
    Open-ManagerPage $ControlPort
} finally { $lock.Dispose() }
