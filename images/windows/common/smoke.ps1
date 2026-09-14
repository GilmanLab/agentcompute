#Requires -Version 5.1
<#
.SYNOPSIS
    In-guest qualification for a fresh clone of a captured Windows image.

.DESCRIPTION
    bake.py runs this through `incus exec` on a clone launched from the
    candidate fingerprint with the runtime device set, and fails the promotion
    if it reports anything but ok. It emits one JSON object on stdout and
    nothing else, so the host can parse it without scraping console text.

    What it establishes, per role:

      both       the machine SID and computer name are the clone's own, not the
                 golden source's; exec works; the OS identity matches what was
                 baked; the OS volume is not encrypted.
      desktop    the Driver daemon is running in an interactive session
                 (Session 1+, not Session 0), a real window enumeration comes
                 back through the named pipe, and the VNC service is listening.
      server     the installation type really is Server Core.
#>
[CmdletBinding()]
param(
    [ValidateSet('desktop', 'server-core')]
    [string] $Role = 'desktop',

    [int] $VncPort = 5900
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$stateDir = Join-Path $env:ProgramData 'agentcompute'
$driver = Join-Path $stateDir 'cua-driver\cua-driver.exe'
$checks = [System.Collections.Generic.List[object]]::new()

function Add-Check {
    param(
        [Parameter(Mandatory)] [string] $Name,
        [Parameter(Mandatory)] [bool] $Pass,
        $Detail
    )

    $checks.Add([ordered]@{ name = $Name; pass = $Pass; detail = $Detail })
    return $Pass
}

function Get-Text {
    param([string] $FilePath, [string[]] $Arguments = @())

    try {
        return (& $FilePath @Arguments 2>&1 | Out-String).Trim()
    } catch {
        return "error: $($_.Exception.Message)"
    }
}

$cv = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
$os = Get-CimInstance Win32_OperatingSystem

# The machine SID is the account-domain SID of any local account; a
# generalized image must produce a different one per clone.
$machineSid = (Get-LocalUser -Name 'Administrator').SID.AccountDomainSid.Value

$identity = [ordered]@{
    computer_name = $env:COMPUTERNAME
    machine_sid   = $machineSid
    caption       = $os.Caption
    version       = $os.Version
    build         = $os.BuildNumber
    ubr           = $cv.UBR
    install_type  = $cv.InstallationType
    boot_time     = $os.LastBootUpTime.ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    uptime_seconds = [math]::Round(((Get-Date).ToUniversalTime() - $os.LastBootUpTime.ToUniversalTime()).TotalSeconds, 1)
}

Add-Check 'machine-sid-present' ([bool]$machineSid) $machineSid | Out-Null
Add-Check 'computer-name-present' ([bool]$env:COMPUTERNAME) $env:COMPUTERNAME | Out-Null

$ver = Get-Text "$env:SystemRoot\System32\cmd.exe" @('/c', 'ver')
Add-Check 'cmd-ver' ($ver -match 'Microsoft Windows') $ver | Out-Null

$bitlocker = Get-Text "$env:SystemRoot\System32\manage-bde.exe" @('-status', 'C:')
Add-Check 'volume-decrypted' (-not ($bitlocker -match 'Conversion Status:\s*(?!Fully Decrypted)')) $bitlocker | Out-Null

$agentService = Get-CimInstance Win32_Service -Filter "Name='incus-agent'"
$agentDetail = 'missing'
if ($agentService) { $agentDetail = "$($agentService.State)/$($agentService.StartMode)" }
Add-Check 'incus-agent-running' ($null -ne $agentService -and $agentService.State -eq 'Running') `
    $agentDetail | Out-Null

$deployReportPath = Join-Path $stateDir 'deploy-report.json'
$deployReport = if (Test-Path -LiteralPath $deployReportPath) {
    Get-Content -LiteralPath $deployReportPath -Raw | ConvertFrom-Json
} else { $null }

if ($Role -eq 'desktop') {
    Add-Check 'deploy-report-present' ($null -ne $deployReport) $deployReportPath | Out-Null
    if ($deployReport) {
        Add-Check 'deploy-report-ok' ($deployReport.status -eq 'ok') $deployReport.status | Out-Null
    }

    $status = Get-Text $driver @('status')
    Add-Check 'driver-daemon-running' ($status -match 'running') $status | Out-Null

    $session = -1
    if ($status -match '(?m)session:\s*(\d+)') { $session = [int]$Matches[1] }
    # Session 0 is the services session and has no attached desktop, so a
    # daemon there cannot see windows at all.
    Add-Check 'driver-session-interactive' ($session -ge 1) "session=$session" | Out-Null

    $windows = Get-Text $driver @('call', 'list_windows', '{}')
    Add-Check 'driver-list-windows' ($windows -notmatch '^error:' -and $windows.Length -gt 0) $windows | Out-Null

    $apps = Get-Text $driver @('call', 'list_apps', '{}')
    Add-Check 'driver-list-apps' ($apps -notmatch '^error:' -and $apps.Length -gt 0) `
        ($apps.Substring(0, [Math]::Min(400, $apps.Length))) | Out-Null

    $vnc = Get-CimInstance Win32_Service -Filter "Name='uvnc_service'"
    $vncDetail = 'missing'
    if ($vnc) { $vncDetail = "$($vnc.State)/$($vnc.StartMode)" }
    Add-Check 'vnc-service-running' ($null -ne $vnc -and $vnc.State -eq 'Running') $vncDetail | Out-Null

    $listening = Get-NetTCPConnection -State Listen -LocalPort $VncPort -ErrorAction SilentlyContinue
    Add-Check 'vnc-listening' ($null -ne $listening) "port=$VncPort" | Out-Null

    $logon = Get-CimInstance Win32_LogonSession -Filter 'LogonType=2' -ErrorAction SilentlyContinue
    Add-Check 'interactive-logon-present' ($null -ne $logon) `
        (($logon | ForEach-Object { $_.LogonId }) -join ',') | Out-Null

    # A PNG that decodes is not a working screenshot: the Linux desktop image
    # was observed returning fully black full-screen captures while windows
    # were visible. So the full-desktop capture is inspected for real content,
    # and the file is left in place for the host to pull as visual evidence.
    $shot = Join-Path $stateDir ('cua-' + [guid]::NewGuid().ToString('N').Substring(0, 12) + '.png')
    $desktopState = Get-Text $driver @('call', 'get_desktop_state', '{}', '--screenshot-out-file', $shot)
    $shotDetail = [ordered]@{ path = $shot; exists = Test-Path -LiteralPath $shot }
    $shotPass = $false
    if ($shotDetail.exists) {
        Add-Type -AssemblyName System.Drawing
        $bitmap = New-Object System.Drawing.Bitmap $shot
        try {
            $colors = New-Object 'System.Collections.Generic.HashSet[int]'
            $luma = [System.Collections.Generic.List[double]]::new()
            $stepX = [Math]::Max(1, [int]($bitmap.Width / 64))
            $stepY = [Math]::Max(1, [int]($bitmap.Height / 64))
            for ($x = 0; $x -lt $bitmap.Width; $x += $stepX) {
                for ($y = 0; $y -lt $bitmap.Height; $y += $stepY) {
                    $pixel = $bitmap.GetPixel($x, $y)
                    [void]$colors.Add($pixel.ToArgb())
                    $luma.Add(0.2126 * $pixel.R + 0.7152 * $pixel.G + 0.0722 * $pixel.B)
                }
            }

            $nonBlack = @($luma | Where-Object { $_ -gt 8 }).Count
            $shotDetail['width'] = $bitmap.Width
            $shotDetail['height'] = $bitmap.Height
            $shotDetail['bytes'] = (Get-Item -LiteralPath $shot).Length
            $shotDetail['sampled_pixels'] = $luma.Count
            $shotDetail['distinct_colors'] = $colors.Count
            $shotDetail['non_black_fraction'] = [math]::Round($nonBlack / [Math]::Max(1, $luma.Count), 4)
            $shotDetail['mean_luma'] = [math]::Round(($luma | Measure-Object -Average).Average, 2)
            # A rendered Windows desktop has a wallpaper and a taskbar: many
            # distinct colors and most sampled pixels lit.
            $shotPass = $colors.Count -ge 16 -and $shotDetail['non_black_fraction'] -ge 0.5
        } finally {
            $bitmap.Dispose()
        }
    } else {
        $shotDetail['driver_output'] = $desktopState.Substring(0, [Math]::Min(400, $desktopState.Length))
    }

    Add-Check 'desktop-screenshot-not-black' $shotPass $shotDetail | Out-Null
} else {
    Add-Check 'server-core' ($cv.InstallationType -eq 'Server Core') $cv.InstallationType | Out-Null
    # No Desktop Experience means no shell: explorer.exe must not be present.
    $explorer = Test-Path -LiteralPath "$env:SystemRoot\explorer.exe"
    Add-Check 'no-desktop-experience' (-not $explorer) "explorer.exe present=$explorer" | Out-Null
    Add-Check 'no-driver-installed' (-not (Test-Path -LiteralPath $driver)) $driver | Out-Null
}

$failed = @($checks | Where-Object { -not $_.pass })
$overall = 'ok'
if ($failed.Count -ne 0) { $overall = 'failed' }
[ordered]@{
    schema_version = 1
    status         = $overall
    role           = $Role
    identity       = $identity
    deploy_report  = $deployReport
    checks         = $checks
    failed         = @($failed | ForEach-Object { $_.name })
} | ConvertTo-Json -Depth 8 -Compress

if ($failed.Count -ne 0) { exit 1 }
exit 0
