param([string]$Root = $PSScriptRoot)
$ErrorActionPreference = 'Stop'
if ([Environment]::OSVersion.Platform -ne 'Win32NT' -or
    [Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture -ne 'X64') {
    throw 'PACKAGE_PLATFORM: Run this Windows x64 package in 64-bit PowerShell on Windows. See the deployment guide for Linux.'
}
$rootPath = [IO.Path]::GetFullPath($Root).TrimEnd('\', '/')
$manifestPath = Join-Path $rootPath 'manifest.json'
if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
    throw 'PACKAGE_MANIFEST: manifest.json is missing. Extract the entire original ZIP into a new writable folder.'
}
try { $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json }
catch { throw 'PACKAGE_MANIFEST: manifest.json is invalid. Extract a fresh copy of the ZIP.' }
if ($manifest.schema -ne 1 -or $manifest.platform -ne 'windows-amd64' -or
    $manifest.package -notin @('manager', 'worker', 'gpu') -or @($manifest.files).Count -eq 0) {
    throw 'PACKAGE_MANIFEST: Unexpected package metadata. Extract a fresh copy of the ZIP.'
}
$seen = @{}
foreach ($entry in $manifest.files) {
    $relative = [string]$entry.path
    if ($relative -notmatch '^[A-Za-z0-9._/-]+$' -or $relative.StartsWith('/') -or
        $relative -match '(^|/)\.{1,2}(/|$)' -or $seen.ContainsKey($relative) -or
        [string]$entry.sha256 -notmatch '^[0-9A-Fa-f]{64}$' -or [string]$entry.bytes -notmatch '^\d+$') {
        throw 'PACKAGE_MANIFEST: An invalid or duplicate file entry was found.'
    }
    $seen[$relative] = $true
    $path = Join-Path $rootPath $relative
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "PACKAGE_MISSING: $relative. Extract the full ZIP again. If an executable disappeared, inspect Windows Security quarantine before replacing it."
    }
    if ((Get-Item -LiteralPath $path).Length -ne [long]$entry.bytes) {
        throw "PACKAGE_CHECKSUM: $relative has the wrong size. Replace it from the verified ZIP."
    }
    $hash = [Security.Cryptography.SHA256]::Create()
    $stream = $null
    try {
        $stream = [IO.File]::OpenRead($path)
        $actual = [BitConverter]::ToString($hash.ComputeHash($stream)).Replace('-', '')
    } finally {
        if ($stream) { $stream.Dispose() }
        $hash.Dispose()
    }
    if ($actual -ne $entry.sha256) {
        throw "PACKAGE_CHECKSUM: $relative is damaged or has changed. Replace the extracted files from a verified ZIP, keeping the data directory."
    }
}
[pscustomobject]@{ Package = $manifest.package; FilesVerified = $seen.Count }
