param(
    [ValidatePattern('^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?$')][string]$Version = '0.1.1',
    [switch]$IncludeGpu
)
$ErrorActionPreference = 'Stop'
$project = Split-Path -Parent $PSScriptRoot
$go = Join-Path $project '.tools/go/bin/go.exe'
if (-not (Test-Path -LiteralPath $go -PathType Leaf)) { $go = (Get-Command go -ErrorAction Stop).Source }
$stage = Join-Path $project ('data/windows-package-' + [guid]::NewGuid().ToString('N'))
$dist = Join-Path $project 'dist/windows'
$null = New-Item -ItemType Directory -Path $stage, $dist -Force
$exeDir = Join-Path $stage 'compiled'
$null = New-Item -ItemType Directory -Path $exeDir

Push-Location -LiteralPath $project
$previous = @{}
try {
    foreach ($entry in @{ GOOS = 'windows'; GOARCH = 'amd64'; CGO_ENABLED = '0' }.GetEnumerator()) {
        $previous[$entry.Key] = [Environment]::GetEnvironmentVariable($entry.Key, 'Process')
        [Environment]::SetEnvironmentVariable($entry.Key, $entry.Value, 'Process')
    }
    & $go build -trimpath -o ($exeDir + [IO.Path]::DirectorySeparatorChar) ./cmd/control ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw 'Windows executable build failed. Check Go and the module cache.' }
    $moduleLines = & $go list -deps -f '{{with .Module}}{{.Path}}|{{.Dir}}{{end}}' ./cmd/control ./cmd/agent | Sort-Object -Unique
    if ($LASTEXITCODE -ne 0) { throw 'Failed to list dependency license locations.' }
    $goroot = & $go env GOROOT
    if ($LASTEXITCODE -ne 0) { throw 'Cannot locate the Go license.' }
} finally {
    foreach ($name in $previous.Keys) { [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process') }
    Pop-Location
}

function Copy-DependencyNotices([string]$Destination) {
    $licenses = Join-Path $Destination 'licenses'
    $goLicenses = Join-Path $licenses 'go'
    $null = New-Item -ItemType Directory -Path $goLicenses -Force
    foreach ($name in @('LICENSE', 'PATENTS')) { Copy-Item -LiteralPath (Join-Path $goroot $name) -Destination $goLicenses }
    foreach ($line in $moduleLines) {
        if ($line -notlike '*|*') { continue }
        $moduleName, $moduleDir = $line -split '\|', 2
        if ($moduleName -eq 'edgeswarm') { continue }
        $files = Get-ChildItem -LiteralPath $moduleDir -File | Where-Object { $_.Name -match '^(licen[sc]e|copying|notice|copyright|patents)([.-].+)?$' }
        if ($files) {
            $target = Join-Path $licenses ($moduleName -replace '[^A-Za-z0-9._-]', '_')
            $null = New-Item -ItemType Directory -Path $target -Force
            foreach ($file in $files) { Copy-Item -LiteralPath $file.FullName -Destination $target }
        }
    }
}

function Add-GpuFiles([string]$Destination) {
    $source = Join-Path $project '.tools/gpu-smoke'
    $target = Join-Path $Destination '.tools/gpu-smoke'
    $null = New-Item -ItemType Directory -Path $target -Force
    $archive = Join-Path $source 'llama-vulkan.zip'
    $model = Join-Path $source 'qwen2.5-0.5b-instruct-q4_k_m.gguf'
    if (-not (Test-Path -LiteralPath $archive -PathType Leaf) -or -not (Test-Path -LiteralPath $model -PathType Leaf)) {
        throw 'Pinned GPU files are missing. Run scripts/install-local-gpu.ps1 on the build machine.'
    }
    if ((Get-FileHash -LiteralPath $archive).Hash -ne '9732A5E48887CA54D0BFE4AF4324DC85FD56CD8CDA19B52DF3542FA6EBE6874F' -or
        (Get-FileHash -LiteralPath $model).Hash -ne '74A4DA8C9FDBCD15BD1F6D01D621410D31C6FC00986F5EB687824E7B93D7A9DB') {
        throw 'Pinned GPU input checksum mismatch. Repair the build cache before packaging.'
    }
    Expand-Archive -LiteralPath $archive -DestinationPath (Join-Path $target 'llama')
    Copy-Item -LiteralPath $model -Destination $target
    $scriptTarget = Join-Path $Destination 'scripts'
    $null = New-Item -ItemType Directory -Path $scriptTarget
    foreach ($name in @('start-local.ps1', 'stop-local.ps1', 'local-common.ps1', 'local-credentials.ps1')) {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination $scriptTarget
    }
    foreach ($name in @('llama.cpp-MIT.txt', 'Qwen-Apache-2.0.txt')) {
        Copy-Item -LiteralPath (Join-Path $project "packaging/licenses/$name") -Destination (Join-Path $Destination 'licenses')
    }
}

Add-Type -AssemblyName System.IO.Compression.FileSystem
$roles = @('manager', 'worker')
if ($IncludeGpu) { $roles += 'gpu' }
foreach ($role in $roles) {
    $name = "Swarm-$role-$Version-windows-amd64"
    $payload = Join-Path $stage $name
    $bin = Join-Path $payload 'bin'
    $null = New-Item -ItemType Directory -Path $bin -Force
    $binaries = switch ($role) { 'manager' { 'control' }; 'worker' { 'agent' }; 'gpu' { 'control'; 'agent' } }
    foreach ($exe in $binaries) { Copy-Item -LiteralPath (Join-Path $exeDir "$exe.exe") -Destination $bin }
    $entrypoints = Join-Path $project "packaging/windows/$role"
    foreach ($file in (Get-ChildItem -LiteralPath $entrypoints -File)) { Copy-Item -LiteralPath $file.FullName -Destination $payload }
    Copy-Item -LiteralPath (Join-Path $project 'packaging/windows/common/Verify-Package.ps1') -Destination $payload
    Copy-DependencyNotices $payload
    if ($role -eq 'gpu') { Add-GpuFiles $payload }
    $records = foreach ($file in (Get-ChildItem -LiteralPath $payload -Recurse -File | Sort-Object FullName)) {
        @{
            path = $file.FullName.Substring($payload.Length + 1).Replace('\', '/')
            bytes = $file.Length
            sha256 = (Get-FileHash -LiteralPath $file.FullName).Hash
        }
    }
    @{ schema = 1; package = $role; version = $Version; platform = 'windows-amd64'; files = @($records) } |
        ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $payload 'manifest.json') -Encoding UTF8
    & (Join-Path $payload 'Verify-Package.ps1') | Out-Null
    $zip = Join-Path $stage "$name.zip"
    [IO.Compression.ZipFile]::CreateFromDirectory($payload, $zip, [IO.Compression.CompressionLevel]::Fastest, $true)
    $hash = (Get-FileHash -LiteralPath $zip).Hash
    Copy-Item -LiteralPath $zip -Destination $dist -Force
    "$hash  $name.zip" | Set-Content -LiteralPath (Join-Path $dist "$name.zip.sha256") -Encoding ASCII
    [pscustomobject]@{ Package = $role; Archive = Join-Path $dist "$name.zip"; SHA256 = $hash; Bytes = (Get-Item -LiteralPath $zip).Length }
}
