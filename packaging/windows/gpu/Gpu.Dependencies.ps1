function Get-SwarmMissingVcRuntime {
    foreach ($name in @('msvcp140.dll', 'vcruntime140.dll', 'vcruntime140_1.dll')) {
        if (-not (Test-Path -LiteralPath (Join-Path ([Environment]::SystemDirectory) $name) -PathType Leaf)) { $name }
    }
}

function Test-SwarmVulkanLoader {
    Test-Path -LiteralPath (Join-Path ([Environment]::SystemDirectory) 'vulkan-1.dll') -PathType Leaf
}

function Install-SwarmVcRuntime {
    $uri = 'https://aka.ms/vc14/vc_redist.x64.exe'
    $fileName = 'swarm-vc-redist-x64-' + [guid]::NewGuid().ToString('N') + '.exe'
    $installer = Join-Path ([IO.Path]::GetTempPath()) $fileName
    $log = "$installer.log"
    $lock = $null
    try {
        Write-Host 'Downloading the Microsoft Visual C++ v14 x64 redistributable.'
        $protocols = [Net.ServicePointManager]::SecurityProtocol
        try {
            if ($PSVersionTable.PSEdition -eq 'Desktop') {
                [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
            }
            Invoke-WebRequest -Uri $uri -OutFile $installer -UseBasicParsing -TimeoutSec 120 -MaximumRedirection 5
        } catch {
            throw "Visual C++ v14 x64 download failed. Check access to Microsoft's download service and retry. $($_.Exception.Message)"
        } finally {
            if ($PSVersionTable.PSEdition -eq 'Desktop') { [Net.ServicePointManager]::SecurityProtocol = $protocols }
        }

        # Keep the verified file open without sharing writes or deletion until execution finishes.
        $lock = [IO.File]::Open($installer, 'Open', 'Read', 'Read')
        $signature = Get-AuthenticodeSignature -LiteralPath $installer
        if ($signature.Status -ne 'Valid' -or $signature.SignatureType -ne 'Authenticode' -or
            $null -eq $signature.SignerCertificate -or
            $signature.SignerCertificate.GetNameInfo('SimpleName', $false) -ne 'Microsoft Corporation') {
            throw 'The Visual C++ installer failed Microsoft Authenticode verification. Check Windows certificate trust and retry.'
        }
        $version = (Get-Item -LiteralPath $installer).VersionInfo
        if ($version.OriginalFilename -ne 'VC_redist.x64.exe' -or $version.FileMajorPart -ne 14) {
            throw 'The Microsoft-signed download is not the Visual C++ v14 x64 installer.'
        }

        Write-Host 'Installing the verified x64 runtime. Approve the Windows elevation prompt if shown.'
        try {
            $process = Start-Process -FilePath $installer -ArgumentList @('/install', '/quiet', '/norestart', '/log', ('"{0}"' -f $log)) `
                -Verb RunAs -WindowStyle Hidden -Wait -PassThru
        } catch {
            throw "Could not start the Visual C++ installer. Allow administrator elevation and retry. $($_.Exception.Message)"
        }
        if ($process.ExitCode -in @(1641, 3010, -2147023255, -2147021886)) {
            throw "The Visual C++ installer requires a reboot. Please restart Windows, then run the GPU check again. Log: $log"
        }
        if ($process.ExitCode -ne 0) {
            throw "Visual C++ installation failed with exit code $($process.ExitCode). Check the installer log and retry. Log: $log"
        }
        return $log
    } finally {
        if ($lock) { $lock.Dispose() }
        if (Test-Path -LiteralPath $installer -PathType Leaf) { Remove-Item -LiteralPath $installer -ErrorAction SilentlyContinue }
    }
}

function Assert-SwarmGpuDependencies([switch]$InstallDependencies) {
    $missing = @(Get-SwarmMissingVcRuntime)
    $problems = @()
    if ($missing.Count) {
        if ($InstallDependencies) {
            $log = Install-SwarmVcRuntime
            $missing = @(Get-SwarmMissingVcRuntime)
            if ($missing.Count) {
                $problems += "Visual C++ runtime DLLs are still missing ($($missing -join ', ')). Repair the installed x64 runtime in Windows Settings. Log: $log"
            }
        } else {
            $problems += "Missing Microsoft Visual C++ v14 x64 runtime DLLs ($($missing -join ', ')). To install, rerun with -InstallDependencies."
        }
    }
    if (-not (Test-SwarmVulkanLoader)) {
        $problems += 'Missing the x64 Vulkan loader (vulkan-1.dll). Install or repair the Vulkan-capable driver from your GPU vendor and retry.'
    }
    if ($problems.Count) { throw ($problems -join [Environment]::NewLine) }
}
