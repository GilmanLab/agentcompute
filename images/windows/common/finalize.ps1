#Requires -Version 5.1
<#
.SYNOPSIS
    Verify, clean and seal the golden source before capture.

.DESCRIPTION
    Two phases, because sealing ends the conversation:

      -Phase verify   Check that everything the image promises is installed and
                      running, prove the OS volume is not encrypted against
                      this build VM's vTPM, then delete setup media caches and
                      cached answer files. Writes
                      C:\ProgramData\agentcompute\finalize-report.json, which
                      the host pulls before sealing.

      -Phase seal     Run Sysprep /generalize /oobe /mode:vm /shutdown /quiet
                      with the deployment answer file. The VM powers off and
                      must never be booted again.

    /quiet is not optional: Server Core has no UI to suppress. /mode:vm is
    only supported when clones run on the same hypervisor and virtual hardware
    profile, which is why the build VM and the runtime contract carry the same
    devices.
#>
[CmdletBinding()]
param(
    [ValidateSet('verify', 'seal')]
    [string] $Phase = 'verify',

    [string] $DeployAnswer = 'C:\image\deploy-unattend.xml'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$stateDir = Join-Path $env:ProgramData 'agentcompute'
$logDir = Join-Path $stateDir 'logs'
$reportPath = Join-Path $stateDir 'finalize-report.json'
New-Item -ItemType Directory -Force -Path $stateDir, $logDir | Out-Null

function Get-Text {
    param([string] $FilePath, [string[]] $Arguments = @())

    try {
        return (& $FilePath @Arguments 2>&1 | Out-String).Trim()
    } catch {
        return "error: $($_.Exception.Message)"
    }
}

function Get-BitLockerState {
    $manageBde = "$env:SystemRoot\System32\manage-bde.exe"
    if (-not (Test-Path -LiteralPath $manageBde)) {
        return [ordered]@{ available = $false; status = 'manage-bde.exe not present'; encrypted = $false }
    }

    $text = Get-Text $manageBde @('-status')
    # Anything other than "Fully Decrypted" on any volume means the captured
    # disk could be sealed to this VM's vTPM, which no clone can unseal.
    $encrypted = $text -match 'Conversion Status:\s*(?!Fully Decrypted)'
    return [ordered]@{ available = $true; status = $text; encrypted = [bool]$encrypted }
}

function Invoke-Decrypt {
    $manageBde = "$env:SystemRoot\System32\manage-bde.exe"
    Get-Text $manageBde @('-off', 'C:') | Out-Host
    for ($attempt = 1; $attempt -le 120; $attempt++) {
        Start-Sleep -Seconds 10
        $state = Get-BitLockerState
        if (-not $state.encrypted) { return $state }
    }

    throw 'C: did not reach Fully Decrypted within 20 minutes'
}

function Test-Components {
    $facts = [ordered]@{}

    $os = Get-CimInstance Win32_OperatingSystem
    $cv = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
    $facts['os'] = [ordered]@{
        caption      = $os.Caption
        version      = $os.Version
        build        = $os.BuildNumber
        ubr          = $cv.UBR
        install_type = $cv.InstallationType
        computer_name = $env:COMPUTERNAME
    }

    $agent = Get-CimInstance Win32_Service -Filter "Name='incus-agent'"
    if (-not $agent) { throw 'incus-agent service is missing; capture would produce an unusable image' }
    $facts['incus_agent'] = [ordered]@{ state = $agent.State; start = $agent.StartMode }

    $bootstrapReport = Join-Path $stateDir 'bootstrap-report.json'
    if (-not (Test-Path -LiteralPath $bootstrapReport)) { throw "Missing $bootstrapReport" }
    $bootstrap = Get-Content -LiteralPath $bootstrapReport -Raw | ConvertFrom-Json
    if ($bootstrap.status -ne 'ok') { throw "bootstrap reported $($bootstrap.status): $($bootstrap.error)" }
    $facts['bootstrap'] = [ordered]@{ status = $bootstrap.status; role = $bootstrap.facts.role; seconds = $bootstrap.seconds }

    if ($bootstrap.facts.role -eq 'desktop') {
        $driver = Join-Path $stateDir 'cua-driver\cua-driver.exe'
        if (-not (Test-Path -LiteralPath $driver)) { throw "Missing $driver" }
        $facts['cua_driver'] = [ordered]@{
            version          = Get-Text $driver @('--version')
            status           = Get-Text $driver @('status')
            autostart_status = Get-Text $driver @('autostart', 'status')
        }

        $vnc = Get-CimInstance Win32_Service -Filter "Name='uvnc_service'"
        if (-not $vnc) { throw 'uvnc_service is missing; the VNC fallback would not exist on clones' }
        $facts['ultravnc'] = [ordered]@{ state = $vnc.State; start = $vnc.StartMode }
    } else {
        if ($cv.InstallationType -ne 'Server Core') {
            throw "Expected a Server Core installation, found '$($cv.InstallationType)'"
        }
    }

    return $facts
}

function Remove-SetupResidue {
    # Microsoft warns that cached answer files keep sensitive data, so the
    # cached copies go before capture. Sysprep writes a fresh one from
    # $DeployAnswer afterwards.
    $removed = @()
    $targets = @(
        'C:\Windows\Panther\unattend.xml',
        'C:\Windows\Panther\Unattend\unattend.xml',
        'C:\Windows\System32\Sysprep\unattend.xml',
        'C:\Windows\Panther\setup.exe.log',
        'C:\Windows\Panther\UnattendGC'
    )

    foreach ($target in $targets) {
        if (Test-Path -LiteralPath $target) {
            Remove-Item -LiteralPath $target -Recurse -Force -ErrorAction SilentlyContinue
            $removed += $target
        }
    }

    foreach ($dir in @($env:TEMP, 'C:\Windows\Temp', 'C:\Windows\SoftwareDistribution\Download')) {
        if (Test-Path -LiteralPath $dir) {
            Get-ChildItem -LiteralPath $dir -Force -ErrorAction SilentlyContinue |
                Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
            $removed += "$dir\*"
        }
    }

    # Any leftover copy of the pinned payload would ship installers inside the
    # image for no reason.
    $stagedPayload = Join-Path $stateDir 'payload'
    if (Test-Path -LiteralPath $stagedPayload) {
        Remove-Item -LiteralPath $stagedPayload -Recurse -Force
        $removed += $stagedPayload
    }

    & "$env:SystemRoot\System32\wevtutil.exe" el |
        ForEach-Object { & "$env:SystemRoot\System32\wevtutil.exe" cl "$_" 2>$null }

    return $removed
}

if ($Phase -eq 'verify') {
    Start-Transcript -Path (Join-Path $logDir 'finalize-verify.log') -Append | Out-Null
    $status = 'ok'
    $failure = $null
    $report = $null
    $started = (Get-Date).ToUniversalTime()

    try {
        $facts = Test-Components
        $bitlockerBefore = Get-BitLockerState
        $bitlockerAfter = if ($bitlockerBefore.encrypted) { Invoke-Decrypt } else { $bitlockerBefore }
        $removed = Remove-SetupResidue

        $report = [ordered]@{
            schema_version    = 1
            status            = $status
            error             = $failure
            facts             = $facts
            bitlocker_before  = $bitlockerBefore
            bitlocker_after   = $bitlockerAfter
            removed           = $removed
            deploy_answer     = $DeployAnswer
            deploy_answer_present = Test-Path -LiteralPath $DeployAnswer
            finished          = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
            seconds           = [math]::Round(((Get-Date).ToUniversalTime() - $started).TotalSeconds, 3)
        }

        if (-not $report.deploy_answer_present) { throw "Deployment answer file $DeployAnswer is missing" }
        if ($bitlockerAfter.encrypted) { throw 'C: is still encrypted; refusing to capture' }
    } catch {
        $status = 'failed'
        $failure = $_.Exception.Message
        $report = [ordered]@{
            schema_version = 1
            status         = $status
            error          = $failure
            finished       = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
        }
    }

    $report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $reportPath -Encoding UTF8
    Stop-Transcript | Out-Null
    if ($status -ne 'ok') { exit 1 }
    exit 0
}

# seal
if (-not (Test-Path -LiteralPath $reportPath)) {
    throw "Refusing to seal: $reportPath is missing, so the verify phase never passed"
}

$verify = Get-Content -LiteralPath $reportPath -Raw | ConvertFrom-Json
if ($verify.status -ne 'ok') {
    throw "Refusing to seal: verify phase reported $($verify.status): $($verify.error)"
}

if (-not (Test-Path -LiteralPath $DeployAnswer)) {
    throw "Refusing to seal: $DeployAnswer is missing"
}

$sysprep = "$env:SystemRoot\System32\Sysprep\Sysprep.exe"
Start-Process -FilePath $sysprep -ArgumentList @(
    '/generalize', '/oobe', '/mode:vm', '/shutdown', '/quiet', "/unattend:$DeployAnswer"
)
exit 0
