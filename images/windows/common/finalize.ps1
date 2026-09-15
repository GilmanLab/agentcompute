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

function Get-NativeText {
    param([string] $FilePath, [string[]] $Arguments = @())

    $output = (& $FilePath @Arguments 2>&1 | Out-String).Trim()
    return [ordered]@{ exit = $LASTEXITCODE; text = $output }
}

function Get-BitLockerState {
    <#
        Fails closed. A capture must only proceed on a positive statement that
        the OS volume is fully decrypted, never on the absence of a match: a
        swallowed manage-bde error would otherwise read as "not encrypted".

        Win32_EncryptableVolume is the authoritative source because its values
        are numeric (ConversionStatus 0 = FullyDecrypted, EncryptionPercentage
        0, ProtectionStatus 0) rather than localized text.
    #>
    $state = [ordered]@{
        method                = $null
        determined            = $false
        encrypted             = $true
        conversion_status     = $null
        encryption_percentage = $null
        protection_status     = $null
        manage_bde_exit       = $null
        status                = $null
        error                 = $null
    }

    $manageBde = "$env:SystemRoot\System32\manage-bde.exe"
    $manageBdePresent = Test-Path -LiteralPath $manageBde

    try {
        $volume = Get-CimInstance -Namespace 'root\CIMV2\Security\MicrosoftVolumeEncryption' `
            -ClassName Win32_EncryptableVolume -Filter "DriveLetter='C:'" -ErrorAction Stop
        if (-not $volume) { throw "Win32_EncryptableVolume has no entry for C:" }

        $conversion = Invoke-CimMethod -InputObject $volume -MethodName GetConversionStatus -ErrorAction Stop
        if ($conversion.ReturnValue -ne 0) {
            throw "GetConversionStatus returned 0x$('{0:X8}' -f $conversion.ReturnValue)"
        }

        $state.method = 'Win32_EncryptableVolume'
        $state.conversion_status = [int]$conversion.ConversionStatus
        $state.encryption_percentage = [int]$conversion.EncryptionPercentage
        $state.protection_status = [int]$volume.ProtectionStatus
        $state.determined = $true
        $state.encrypted = -not (
            $state.conversion_status -eq 0 -and
            $state.encryption_percentage -eq 0 -and
            $state.protection_status -eq 0
        )
    } catch {
        $state.error = $_.Exception.Message
        if (-not $manageBdePresent) {
            # No BitLocker WMI provider and no manage-bde: the guest has no
            # volume-encryption capability at all, so there is nothing that
            # could have sealed the disk to this VM's vTPM.
            $state.method = 'none (no BitLocker capability present)'
            $state.determined = $true
            $state.encrypted = $false
        }
    }

    if ($manageBdePresent) {
        $run = Get-NativeText $manageBde @('-status', 'C:')
        $state.manage_bde_exit = $run.exit
        $state.status = $run.text
        if (-not $state.determined) {
            # Fall back only on an explicit positive pair, and only when the
            # command itself succeeded.
            $decrypted = $run.exit -eq 0 -and
                $run.text -match 'Conversion Status:\s+Fully Decrypted' -and
                $run.text -match 'Percentage Encrypted:\s+0([.,]0+)?%'
            if ($decrypted) {
                $state.method = 'manage-bde text'
                $state.determined = $true
                $state.encrypted = $false
            }
        }
    }

    if (-not $state.determined) {
        $state.error = "could not determine encryption state: $($state.error)"
    }

    return $state
}

function Invoke-Decrypt {
    $manageBde = "$env:SystemRoot\System32\manage-bde.exe"
    Get-Text $manageBde @('-off', 'C:') | Out-Host
    for ($attempt = 1; $attempt -le 120; $attempt++) {
        Start-Sleep -Seconds 10
        $state = Get-BitLockerState
        if ($state.determined -and -not $state.encrypted) { return $state }
    }

    throw 'C: did not reach Fully Decrypted within 20 minutes'
}

function Set-PersistentAutoLogon {
    param([Parameter(Mandatory)] [string] $User)

    <#
        The answer file only asks for one automatic logon, which is what
        Microsoft's AutoLogon reference requires it to declare. The image needs
        one at every boot, because the Cua Driver daemon can only see windows
        from an interactive session, so the persistent state is written here
        instead: AutoAdminLogon on, no logon counter left behind, and an empty
        DefaultPassword so no reusable credential is stored.
    #>
    $winlogon = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
    Set-ItemProperty -Path $winlogon -Name 'AutoAdminLogon' -Value '1' -Type String
    Set-ItemProperty -Path $winlogon -Name 'DefaultUserName' -Value $User -Type String
    Set-ItemProperty -Path $winlogon -Name 'DefaultDomainName' -Value ([Environment]::MachineName) -Type String
    Set-ItemProperty -Path $winlogon -Name 'DefaultPassword' -Value '' -Type String
    foreach ($stale in 'AutoLogonCount', 'AutoLogonSID') {
        Remove-ItemProperty -Path $winlogon -Name $stale -ErrorAction SilentlyContinue
    }

    $values = Get-ItemProperty -Path $winlogon
    $count = $values.PSObject.Properties['AutoLogonCount']
    return [ordered]@{
        AutoAdminLogon    = $values.AutoAdminLogon
        DefaultUserName   = $values.DefaultUserName
        DefaultDomainName = $values.DefaultDomainName
        AutoLogonCount    = if ($null -ne $count) { $count.Value } else { $null }
    }
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
        computer_name = [Environment]::MachineName
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
        $proxy = Join-Path $stateDir 'cua-driver\cua-driver-proxy.exe'
        if (-not (Test-Path -LiteralPath $driver)) { throw "Missing $driver" }
        if (-not (Test-Path -LiteralPath $proxy)) { throw "Missing $proxy" }

        $proxyHash = (Get-FileHash -LiteralPath $proxy -Algorithm SHA256).Hash.ToLowerInvariant()
        $expectedProxy = $null
        if ($bootstrap.facts.cua_driver.PSObject.Properties['proxy_sha256']) {
            $expectedProxy = [string]$bootstrap.facts.cua_driver.proxy_sha256
        }
        if ($expectedProxy -and $expectedProxy.ToLowerInvariant() -ne $proxyHash) {
            throw "cua-driver-proxy.exe sha256 $proxyHash != bootstrap $expectedProxy"
        }

        $daemons = @(Get-CimInstance Win32_Process -Filter "Name='cua-driver.exe'" |
            Where-Object {
                $_.SessionId -ge 1 -and $_.ExecutablePath -and
                ([System.IO.Path]::GetFullPath($_.ExecutablePath) -eq
                    [System.IO.Path]::GetFullPath($driver))
            })
        if ($daemons.Count -eq 0) {
            throw 'Cua Driver daemon is not running in an interactive session'
        }

        $facts['cua_driver'] = [ordered]@{
            version          = Get-Text $driver @('--version')
            autostart_status = Get-Text $driver @('autostart', 'status')
            proxy            = $proxy
            proxy_sha256     = $proxyHash
            daemon_pid       = [int]$daemons[0].ProcessId
            session          = [int]$daemons[0].SessionId
            command_line     = $daemons[0].CommandLine
        }
        if ($facts['cua_driver'].session -lt 1) {
            throw "Cua Driver daemon session $($facts['cua_driver'].session) is not interactive"
        }

        $vnc = Get-CimInstance Win32_Service -Filter "Name='uvnc_service'"
        if (-not $vnc) { throw 'uvnc_service is missing; the VNC fallback would not exist on clones' }
        $facts['ultravnc'] = [ordered]@{ state = $vnc.State; start = $vnc.StartMode }

        # Idempotent: guarantees the captured image boots into the interactive
        # session regardless of how many logons the answer file requested.
        $facts['autologon'] = Set-PersistentAutoLogon -User 'automation'
        if ($facts['autologon'].AutoAdminLogon -ne '1') {
            throw 'AutoAdminLogon is not enabled; clones would stop at the login screen'
        }
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
        if (-not $bitlockerAfter.determined) {
            throw "Refusing to capture: encryption state of C: is unknown ($($bitlockerAfter.error))"
        }

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

# Incus exec is SYSTEM. Microsoft documents that sealing under SYSTEM skips
# XAML AppX registration on Windows 11 24H2/25H2 and Server 2025.
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$taskName = 'agentcompute-sysprep'
if ($identity.User.Value -eq 'S-1-5-18') {
    $user = (Get-CimInstance Win32_ComputerSystem).UserName
    if (-not $user) { throw 'Sysprep requires a logged-on administrator' }
    $account = New-Object Security.Principal.NTAccount $user
    $sid = $account.Translate([Security.Principal.SecurityIdentifier]).Value
    $principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Highest
    $arguments = '-NoProfile -ExecutionPolicy Bypass -File "{0}" -Phase seal -DeployAnswer "{1}"' -f $PSCommandPath, $DeployAnswer
    $action = New-ScheduledTaskAction `
        -Execute "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" `
        -Argument $arguments
    Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal -Force | Out-Null
    Start-ScheduledTask -TaskName $taskName
    [ordered]@{ dispatched = $true; user = $user; sid = $sid } | ConvertTo-Json -Compress
    exit 0
}

$principal = New-Object Security.Principal.WindowsPrincipal $identity
$session = (Get-Process -Id $PID).SessionId
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator) -or $session -lt 1) {
    throw 'Sysprep must run elevated in the logged-on administrator session'
}

# Deleting an on-demand task does not interrupt its running process.
Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
[ordered]@{
    user = $identity.Name
    sid = $identity.User.Value
    session = $session
    elevated = $true
    started = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
} | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $stateDir 'seal-context.json') -Encoding UTF8

$sysprep = "$env:SystemRoot\System32\Sysprep\Sysprep.exe"
Start-Process -FilePath $sysprep -WorkingDirectory (Split-Path -Parent $sysprep) -ArgumentList @(
    '/generalize', '/oobe', '/mode:vm', '/shutdown', '/quiet', "/unattend:$DeployAnswer"
)
exit 0
