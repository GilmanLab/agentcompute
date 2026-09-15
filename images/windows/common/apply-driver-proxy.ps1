#Requires -Version 5.1
<#
.SYNOPSIS
    Install or replace cua-driver-proxy.exe on an already-provisioned desktop VM.

.DESCRIPTION
    For the unsealed golden and any later repair: copy a host-built proxy next to
    the pinned Cua binary, hash-check it, and leave cua-driver.exe and the
    interactive autostart task untouched. Refuses Server Core. Safe to re-run.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string] $Source,

    [Parameter(Mandatory)]
    [string] $ExpectedSha256
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$stateDir = Join-Path $env:ProgramData 'agentcompute'
$installDir = Join-Path $stateDir 'cua-driver'
$original = Join-Path $installDir 'cua-driver.exe'
$destination = Join-Path $installDir 'cua-driver-proxy.exe'
$reportPath = Join-Path $stateDir 'driver-proxy-apply.json'

function Assert-FileHash {
    param(
        [Parameter(Mandatory)] [string] $Path,
        [Parameter(Mandatory)] [string] $Expected
    )

    $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $Expected.ToLowerInvariant()) {
        throw "SHA-256 mismatch for $Path : expected $Expected, got $actual"
    }
    return $actual
}

$cv = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
if ($cv.InstallationType -eq 'Server Core') {
    throw 'refusing to install the Driver proxy on Server Core'
}

if (-not (Test-Path -LiteralPath $original)) {
    throw "refusing to install the proxy: original Driver is missing at $original"
}

if (-not (Test-Path -LiteralPath $Source)) {
    throw "proxy source is missing: $Source"
}

New-Item -ItemType Directory -Force -Path $installDir | Out-Null
$sourceHash = Assert-FileHash -Path $Source -Expected $ExpectedSha256
Copy-Item -LiteralPath $Source -Destination $destination -Force
$installedHash = Assert-FileHash -Path $destination -Expected $ExpectedSha256

$originalHash = (Get-FileHash -LiteralPath $original -Algorithm SHA256).Hash.ToLowerInvariant()
$report = [ordered]@{
    schema_version     = 1
    status             = 'ok'
    original           = $original
    original_sha256    = $originalHash
    original_untouched = $true
    proxy              = $destination
    proxy_sha256       = $installedHash
    source             = $Source
    source_sha256      = $sourceHash
    finished           = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
}
$report | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $reportPath -Encoding UTF8
Write-Output ($report | ConvertTo-Json -Depth 4 -Compress)
exit 0
