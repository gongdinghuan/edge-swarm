#Requires -Version 7.4
param(
    [ValidatePattern('^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?$')][string]$Version = '0.1.1'
)
$ErrorActionPreference = 'Stop'
$project = Split-Path -Parent $PSScriptRoot
$go = Join-Path $project '.tools/go/bin/go.exe'
if (-not (Test-Path -LiteralPath $go -PathType Leaf)) { $go = (Get-Command go -ErrorAction Stop).Source }
$stage = Join-Path $project ('data/linux-package-' + [guid]::NewGuid().ToString('N'))
$dist = Join-Path $project 'dist/linux'
$source = Join-Path $project 'packaging/linux'
$manual = Join-Path $project 'docs/runbooks/linux-packages.md'
$manualContent = [IO.File]::ReadAllText($manual).Replace('0.1.1', $Version).Replace("`r`n", "`n")
$null = New-Item -ItemType Directory -Path $stage, $dist -Force

function Copy-Notices($Destination, $GoRoot, $Modules) {
    $licenses = Join-Path $Destination 'licenses'
    $goLicenses = Join-Path $licenses 'go'
    $null = New-Item -ItemType Directory -Path $goLicenses -Force
    foreach ($name in @('LICENSE', 'PATENTS')) { Copy-Item -LiteralPath (Join-Path $GoRoot $name) -Destination $goLicenses }
    foreach ($line in $Modules) {
        if ($line -notlike '*|*') { continue }
        $moduleName, $moduleDir = $line -split '\|', 2
        if ($moduleName -eq 'edgeswarm') { continue }
        $files = Get-ChildItem -LiteralPath $moduleDir -File | Where-Object { $_.Name -match '^(licen[sc]e|copying|notice|copyright|patents)([.-].+)?$' }
        if ($files) {
            $target = Join-Path $licenses ($moduleName -replace '[^A-Za-z0-9._-]', '_')
            $null = New-Item -ItemType Directory -Path $target -Force
            Copy-Item -LiteralPath $files.FullName -Destination $target
        }
    }
}

function Write-Archive($Directory, $Archive) {
    $file = [IO.File]::Create($Archive)
    try {
        $gzip = [IO.Compression.GZipStream]::new($file, [IO.Compression.CompressionLevel]::Optimal, $true)
        try {
            $tar = [System.Formats.Tar.TarWriter]::new($gzip, [System.Formats.Tar.TarEntryFormat]::Pax, $true)
            try {
                $root = Split-Path -Leaf $Directory
                foreach ($asset in Get-ChildItem -LiteralPath $Directory -Recurse -File | Sort-Object FullName) {
                    $relative = [IO.Path]::GetRelativePath($Directory, $asset.FullName).Replace('\', '/')
                    $entry = [System.Formats.Tar.PaxTarEntry]::new([System.Formats.Tar.TarEntryType]::RegularFile, "$root/$relative")
                    $entry.Mode = if ($relative -like 'bin/*' -or $relative -eq 'swarm.sh') {
                        [IO.UnixFileMode]493 # 0755
                    } else {
                        [IO.UnixFileMode]420 # 0644
                    }
                    $inputFile = $asset.OpenRead()
                    try {
                        $entry.DataStream = $inputFile
                        $tar.WriteEntry($entry)
                    } finally { $inputFile.Dispose() }
                }
            } finally { $tar.Dispose() }
        } finally { $gzip.Dispose() }
    } finally { $file.Dispose() }
}

$previous = @{}
Push-Location -LiteralPath $project
try {
    foreach ($variable in @('GOOS', 'GOARCH', 'GOAMD64', 'GOARM64', 'CGO_ENABLED')) {
        $previous[$variable] = [Environment]::GetEnvironmentVariable($variable, 'Process')
    }
    $env:GOOS = 'linux'
    $env:GOAMD64 = 'v1'
    $env:GOARM64 = 'v8.0'
    $env:CGO_ENABLED = '0'
    $goRoot = & $go env GOROOT
    if ($LASTEXITCODE -ne 0) { throw 'Cannot locate the Go license.' }

    foreach ($arch in @('amd64', 'arm64')) {
        $env:GOARCH = $arch
        $compiled = Join-Path $stage "compiled-$arch"
        $null = New-Item -ItemType Directory -Path $compiled
        & $go build -trimpath -buildvcs=false -ldflags '-s -w' -o ($compiled + [IO.Path]::DirectorySeparatorChar) ./cmd/control ./cmd/agent
        if ($LASTEXITCODE -ne 0) { throw "Linux/$arch build failed. Check the Go toolchain and module cache." }

        foreach ($role in @('manager', 'worker')) {
            $exe = if ($role -eq 'manager') { 'control' } else { 'agent' }
            $modules = & $go list -deps -f '{{with .Module}}{{.Path}}|{{.Dir}}{{end}}' "./cmd/$exe" | Sort-Object -Unique
            if ($LASTEXITCODE -ne 0) { throw "Cannot list $role dependency licenses." }
            $name = "Swarm-$role-$Version-linux-$arch"
            $payload = Join-Path $stage $name
            $bin = Join-Path $payload 'bin'
            $null = New-Item -ItemType Directory -Path $bin -Force
            Copy-Item -LiteralPath (Join-Path $compiled $exe) -Destination $bin
            [IO.File]::WriteAllText((Join-Path $payload 'README.zh-CN.md'), $manualContent)
            Copy-Notices $payload $goRoot $modules
            # A Windows checkout may have CRLF line endings. Ship LF files for Linux.
            $assets = @(Get-ChildItem -LiteralPath (Join-Path $source 'common') -File -Filter '*.sh')
            $assets += Get-ChildItem -LiteralPath (Join-Path $source $role) -File
            foreach ($asset in $assets) {
                $content = [IO.File]::ReadAllText($asset.FullName).Replace("`r`n", "`n")
                [IO.File]::WriteAllText((Join-Path $payload $asset.Name), $content)
            }
            [IO.File]::WriteAllText((Join-Path $payload 'package.info'), "$role $arch`n")
            $hashes = foreach ($asset in Get-ChildItem -LiteralPath $payload -Recurse -File | Sort-Object FullName) {
                $relative = [IO.Path]::GetRelativePath($payload, $asset.FullName).Replace('\', '/')
                $hash = (Get-FileHash -LiteralPath $asset.FullName).Hash.ToLowerInvariant()
                "$hash  $relative"
            }
            [IO.File]::WriteAllText((Join-Path $payload 'SHA256SUMS'), (($hashes -join "`n") + "`n"))
            Write-Archive $payload (Join-Path $stage "$name.tar.gz")
        }
    }
} finally {
    foreach ($variable in $previous.Keys) { [Environment]::SetEnvironmentVariable($variable, $previous[$variable], 'Process') }
    Pop-Location
}

$releaseHashes = foreach ($archive in Get-ChildItem -LiteralPath $stage -Filter '*.tar.gz' -File | Sort-Object Name) {
    $hash = (Get-FileHash -LiteralPath $archive.FullName).Hash.ToLowerInvariant()
    Copy-Item -LiteralPath $archive.FullName -Destination $dist -Force
    $record = "$hash  $($archive.Name)"
    [IO.File]::WriteAllText((Join-Path $dist "$($archive.Name).sha256"), "$record`n")
    $record
}
[IO.File]::WriteAllText((Join-Path $dist "SHA256SUMS-$Version"), (($releaseHashes -join "`n") + "`n"))
[IO.File]::WriteAllText((Join-Path $dist 'Linux-部署手册.md'), $manualContent)
$releaseHashes
