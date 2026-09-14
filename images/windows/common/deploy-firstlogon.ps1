#Requires -Version 5.1
<#
.SYNOPSIS
    First-logon repair for clones of the generalized Windows 11 image.

.DESCRIPTION
    Whether the Cua Driver's Interactive-logon scheduled task survives Sysprep
    generalization was an open question for this image family. This script
    answers it on every clone instead of once: it records the pre-repair state
    (did the task exist, was the daemon already running, in which session)
    before doing anything, then repairs idempotently if needed.

    It is registered by the deployment answer file's single FirstLogonCommands
    entry, so it runs inside the clone's interactive logon, which is the only
    session where `autostart enable` can place the daemon in Session 1+.

    Its report is the evidence source for the Sysprep-survival finding:
    C:\ProgramData\agentcompute\deploy-report.json.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$stateDir = Join-Path $env:ProgramData 'agentcompute'
$logDir = Join-Path $stateDir 'logs'
$reportPath = Join-Path $stateDir 'deploy-report.json'
$driver = Join-Path $stateDir 'cua-driver\cua-driver.exe'

New-Item -ItemType Directory -Force -Path $stateDir, $logDir | Out-Null
Start-Transcript -Path (Join-Path $logDir 'deploy-firstlogon.log') -Append | Out-Null

function Get-DriverText {
    param([string[]] $Arguments)

    if (-not (Test-Path -LiteralPath $driver)) { return 'cua-driver.exe missing' }

    try {
        $output = & $driver @Arguments 2>&1 | Out-String
        return $output.Trim()
    } catch {
        return "error: $($_.Exception.Message)"
    }
}

$status = 'ok'
$failure = $null
$started = (Get-Date).ToUniversalTime()

try {
    $before = [ordered]@{
        driver_present   = Test-Path -LiteralPath $driver
        autostart_status = Get-DriverText @('autostart', 'status')
        daemon_status    = Get-DriverText @('status')
        task             = (& "$env:SystemRoot\System32\schtasks.exe" /query /fo LIST 2>&1 |
                             Select-String -SimpleMatch 'cua-driver' | ForEach-Object { $_.Line.Trim() }) -join '; '
    }

    $survived = ($before.daemon_status -match 'running')
    $repaired = $false

    if (-not $survived) {
        # Idempotent: enable is a no-op when the task already exists, and kick
        # starts it now rather than at the next logon.
        Get-DriverText @('autostart', 'enable') | Out-Host
        Get-DriverText @('autostart', 'kick') | Out-Host
        $repaired = $true
    }

    $after = $null
    for ($attempt = 1; $attempt -le 30; $attempt++) {
        Start-Sleep -Seconds 2
        $after = Get-DriverText @('status')
        if ($after -match 'running') { break }
    }

    if (-not ($after -match 'running')) {
        $status = 'failed'
        $failure = "Cua Driver daemon not running after repair: $after"
    }

    $report = [ordered]@{
        schema_version   = 1
        status           = $status
        error            = $failure
        computer_name    = $env:COMPUTERNAME
        user             = "$env:USERDOMAIN\$env:USERNAME"
        session          = (Get-Process -Id $PID).SessionId
        # The finding: true means the generalized image's scheduled task came
        # back by itself, false means this repair was required.
        autostart_survived_sysprep = $survived
        repaired         = $repaired
        before           = $before
        after_status     = $after
        finished         = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
        seconds          = [math]::Round(((Get-Date).ToUniversalTime() - $started).TotalSeconds, 3)
    }
} catch {
    $status = 'failed'
    $failure = $_.Exception.Message
    $report = [ordered]@{
        schema_version = 1
        status         = $status
        error          = $failure
        computer_name  = $env:COMPUTERNAME
        finished       = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    }
}

$report | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $reportPath -Encoding UTF8
Stop-Transcript | Out-Null

if ($status -ne 'ok') { exit 1 }
exit 0
