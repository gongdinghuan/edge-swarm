param(
    [string]$StateDirectory,
    [switch]$Admin,
    [switch]$LocalGpu,
    [switch]$Copy
)

. "$PSScriptRoot/local-common.ps1"
if ($LocalGpu -and -not $StateDirectory) { $StateDirectory = Join-Path $script:ProjectRoot 'data/local-gpu' }
$state = Resolve-SwarmStateDirectory $StateDirectory
$credentials = Get-Content -LiteralPath (Join-Path $state 'credentials.json') -Encoding UTF8 -Raw | ConvertFrom-Json
$key = if ($Admin) { Unprotect-SwarmValue $credentials.admin } else { Unprotect-SwarmValue $credentials.api_key }
if ($Copy) {
    Set-Clipboard -Value $key
    Write-Host 'Credential copied to the clipboard.'
} else {
    $key
}
