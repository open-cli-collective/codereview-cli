$ErrorActionPreference = 'Stop'

$version = $env:ChocolateyPackageVersion
$toolsDir = Split-Path -Parent $MyInvocation.MyCommand.Definition

# Checksums injected by release workflow - DO NOT EDIT MANUALLY
$checksumAmd64 = 'CHECKSUM_AMD64_PLACEHOLDER'
$checksumArm64 = 'CHECKSUM_ARM64_PLACEHOLDER'

if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
    $arch = 'arm64'
    $checksum = $checksumArm64
} elseif ([Environment]::Is64BitOperatingSystem) {
    $arch = 'amd64'
    $checksum = $checksumAmd64
} else {
    throw "32-bit Windows is not supported. cr requires 64-bit Windows."
}

$baseUrl = "https://github.com/open-cli-collective/codereview-cli/releases/download/v${version}"
$zipFile = "cr_v${version}_windows_${arch}.zip"
$url = "${baseUrl}/${zipFile}"

Write-Host "Installing cr ${version} for Windows ${arch}..."
Write-Host "URL: ${url}"
Write-Host "Checksum (SHA256): ${checksum}"

Install-ChocolateyZipPackage -PackageName $env:ChocolateyPackageName `
    -Url $url `
    -UnzipLocation $toolsDir `
    -Checksum $checksum `
    -ChecksumType 'sha256'

Write-Host "cr installed successfully. Run 'cr --help' to get started."
