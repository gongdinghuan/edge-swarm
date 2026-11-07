$ErrorActionPreference = 'Stop'
$project = Split-Path -Parent $PSScriptRoot
$fixture = Join-Path $project ('data/package-check-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $fixture -Force
$payload = Join-Path $fixture 'agent.exe'
[IO.File]::WriteAllBytes($payload, [byte[]](1, 2, 3))
$manifest = @{
    schema = 1
    package = 'worker'
    platform = 'windows-amd64'
    files = @(@{ path = 'agent.exe'; bytes = 3; sha256 = (Get-FileHash $payload).Hash })
}
$manifestPath = Join-Path $fixture 'manifest.json'
function Save-FixtureManifest { $manifest | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifestPath -Encoding UTF8 }
function Assert-VerificationFailure([string]$Expected) {
    try { & $verifier -Root $fixture | Out-Null }
    catch {
        if ($_.Exception.Message -notlike "*$Expected*") { throw }
        return
    }
    throw "Expected $Expected but verification accepted the fixture."
}
$verifier = Join-Path $project 'packaging/windows/common/Verify-Package.ps1'
Save-FixtureManifest
& $verifier -Root $fixture | Out-Null
[IO.File]::WriteAllBytes($payload, [byte[]](3, 2, 1))
Assert-VerificationFailure 'PACKAGE_CHECKSUM'
Remove-Item -LiteralPath $payload
Assert-VerificationFailure 'PACKAGE_MISSING'
$manifest.files[0].path = '../outside.exe'
Save-FixtureManifest
Assert-VerificationFailure 'PACKAGE_MANIFEST'
Remove-Item -LiteralPath $manifestPath
[IO.Directory]::Delete($fixture)
'Package verifier checks passed.'
