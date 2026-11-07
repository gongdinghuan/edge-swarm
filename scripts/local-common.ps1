$ErrorActionPreference = 'Stop'
$script:ProjectRoot = Split-Path -Parent $PSScriptRoot

function Resolve-SwarmStateDirectory([string]$Path) {
    if (-not $Path) { $Path = Join-Path $script:ProjectRoot 'data/local' }
    $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
}

function Save-SwarmJson([string]$Path, $Value) {
    $temp = "$Path.tmp"
    $json = ConvertTo-Json -InputObject $Value -Depth 8
    [IO.File]::WriteAllText($temp, $json)
    if ([IO.File]::Exists($Path)) {
        [IO.File]::Replace($temp, $Path, [NullString]::Value)
    } else {
        [IO.File]::Move($temp, $Path)
    }
}

function Get-SwarmProcess($Record) {
    $expectedPath = Get-SwarmExecutable $Record.role
    if ($Record.path -ne $expectedPath) { throw 'Process path in local state does not match this workspace.' }
    try {
        $process = Get-Process -Id $Record.pid -ErrorAction Stop
        if ($process.Path -eq $Record.path -and $process.StartTime.ToUniversalTime().Ticks.ToString() -eq $Record.started) {
            return $process
        }
    } catch {
        if ($_.Exception -isnot [Microsoft.PowerShell.Commands.ProcessCommandException]) { throw }
    }
}

function Get-SwarmExecutable([string]$Role) {
    if ($Role -eq 'llama-server') {
        return [IO.Path]::GetFullPath((Join-Path $script:ProjectRoot '.tools/gpu-smoke/llama/llama-server.exe'))
    }
    if ($Role -notin @('control','agent','simulator')) { throw 'Unrecognized process role in local state.' }
    [IO.Path]::GetFullPath((Join-Path $script:ProjectRoot "bin/$Role.exe"))
}

function Get-SwarmGpuRuntime {
    $files = @{
        '.tools/gpu-smoke/llama/llama-server.exe' = '00D50686815AF38249D0218C91558FBD0E9093AC8C074BAC86196907F4305E31'
        '.tools/gpu-smoke/llama/ggml-vulkan.dll' = 'E863E7556E10EEEA6888894755552A917AB919004882BDF89D2D684D2B54FC8C'
        '.tools/gpu-smoke/qwen2.5-0.5b-instruct-q4_k_m.gguf' = '74A4DA8C9FDBCD15BD1F6D01D621410D31C6FC00986F5EB687824E7B93D7A9DB'
    }
    foreach ($relative in $files.Keys) {
        $path = Join-Path $script:ProjectRoot $relative
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw 'GPU runtime is missing. Run scripts/install-local-gpu.ps1.' }
        if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $files[$relative]) {
            throw "GPU runtime SHA256 mismatch at $relative. Review the installation before running it."
        }
    }
    @{ modelPath = Join-Path $script:ProjectRoot '.tools/gpu-smoke/qwen2.5-0.5b-instruct-q4_k_m.gguf' }
}

function Stop-SwarmProcess($Record) {
    $process = Get-SwarmProcess $Record
    if ($null -ne $process -and -not $process.HasExited) {
        try { $process.Kill() } catch { if (-not $process.HasExited) { throw } }
        if (-not $process.WaitForExit(10000)) { throw "Process $($Record.role) did not stop." }
    }
}

function Stop-SwarmProcessSet($Records, [string]$ProcessFile) {
    $remaining = @()
    $failures = @()
    for ($index = $Records.Count - 1; $index -ge 0; $index--) {
        $record = $Records[$index]
        try { Stop-SwarmProcess $record }
        catch {
            $remaining = @($record) + $remaining
            $failures += "$($record.role): $($_.Exception.Message)"
        }
    }
    if ($remaining.Count) {
        Save-SwarmJson $ProcessFile @{ processes = $remaining }
        throw "Some processes could not be stopped. Retry Stop after checking: $($failures -join ' | ')"
    }
    if (Test-Path -LiteralPath $ProcessFile) { Remove-Item -LiteralPath $ProcessFile }
}

function Protect-SwarmValue([string]$Value) {
    ConvertFrom-SecureString (ConvertTo-SecureString -String $Value -AsPlainText -Force)
}

function Unprotect-SwarmValue([string]$Value) {
    $valueSecure = ConvertTo-SecureString -String $Value
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($valueSecure)
    try { [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr) }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
}

function New-SwarmKey {
    $buffer = New-Object byte[] 32
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($buffer) } finally { $rng.Dispose() }
    -join ($buffer | ForEach-Object { $_.ToString('x2') })
}

function Enter-SwarmLock([string]$Directory) {
    [IO.File]::Open((Join-Path $Directory 'process.lock'), 'OpenOrCreate', 'ReadWrite', 'None')
}
