# Windows x64 Vulkan transport/GPU demonstration runtime, pinned to reviewed artifacts.
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/local-common.ps1"
if ([Environment]::OSVersion.Platform -ne 'Win32NT' -or -not [Environment]::Is64BitOperatingSystem) {
    throw 'The local GPU runtime requires Windows x64 and a Vulkan-capable GPU driver.'
}
$directory = Join-Path $script:ProjectRoot '.tools/gpu-smoke'
$null = New-Item -ItemType Directory -Force -Path $directory
$lock = [IO.File]::Open((Join-Path $directory 'install.lock'),'OpenOrCreate','ReadWrite','None')

function Get-VerifiedArtifact([string]$Url, [string]$Destination, [string]$Hash) {
    if (Test-Path -LiteralPath $Destination -PathType Leaf) {
        if ((Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash -ne $Hash) {
            throw "Existing artifact hash mismatch at $Destination. Review that file before retrying."
        }
        return
    }
    $temporary = "$Destination.download-$([guid]::NewGuid().ToString('N'))"
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $temporary -TimeoutSec 600
        if ((Get-FileHash -LiteralPath $temporary -Algorithm SHA256).Hash -ne $Hash) {
            throw 'Downloaded artifact SHA256 verification failed.'
        }
        Move-Item -LiteralPath $temporary -Destination $Destination -ErrorAction Stop
    } finally {
        if (Test-Path -LiteralPath $temporary -PathType Leaf) { Remove-Item -LiteralPath $temporary }
    }
}

try {
    $archive = Join-Path $directory 'llama-vulkan.zip'
    Get-VerifiedArtifact `
        'https://github.com/ggml-org/llama.cpp/releases/download/b12110/llama-b12110-bin-win-vulkan-x64.zip' `
        $archive '9732A5E48887CA54D0BFE4AF4324DC85FD56CD8CDA19B52DF3542FA6EBE6874F'

    $runtime = Join-Path $directory 'llama'
    if (-not (Test-Path -LiteralPath $runtime)) {
        $staging = Join-Path $directory ('extract-' + [guid]::NewGuid().ToString('N'))
        Expand-Archive -LiteralPath $archive -DestinationPath $staging
        if (-not (Test-Path -LiteralPath (Join-Path $staging 'llama-server.exe') -PathType Leaf)) {
            throw "Unexpected runtime layout. Inspect $staging before retrying."
        }
        Move-Item -LiteralPath $staging -Destination $runtime
    }

    Get-VerifiedArtifact `
        'https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/9217f5db79a29953eb74d5343926648285ec7e67/qwen2.5-0.5b-instruct-q4_k_m.gguf?download=true' `
        (Join-Path $directory 'qwen2.5-0.5b-instruct-q4_k_m.gguf') `
        '74A4DA8C9FDBCD15BD1F6D01D621410D31C6FC00986F5EB687824E7B93D7A9DB'

    $null = Get-SwarmGpuRuntime
    Write-Output 'Pinned llama.cpp b12110 Vulkan and Qwen2.5 0.5B Q4_K_M files verified. Start with scripts/start-local.ps1 -LocalGpu.'
} finally { $lock.Dispose() }
