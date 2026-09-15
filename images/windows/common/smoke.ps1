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
$proxy = Join-Path $stateDir 'cua-driver\cua-driver-proxy.exe'
$pipe = '\\.\pipe\cua-driver'
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

function Convert-CuaJson {
    param([string] $Text)

    $trim = $Text.Trim()
    if (-not $trim) { return $null }
    try { return $trim | ConvertFrom-Json } catch {}
    $obj = $trim.IndexOf('{')
    $arr = $trim.IndexOf('[')
    $start = -1
    if ($obj -ge 0 -and ($arr -lt 0 -or $obj -lt $arr)) { $start = $obj }
    elseif ($arr -ge 0) { $start = $arr }
    if ($start -lt 0) { return $null }
    try { return $trim.Substring($start) | ConvertFrom-Json } catch { return $null }
}

function Get-CuaInteractiveDaemons {
    param([Parameter(Mandatory)] [string] $DriverPath)

    $expected = [System.IO.Path]::GetFullPath($DriverPath)
    $found = @(Get-CimInstance Win32_Process -Filter "Name='cua-driver.exe'" |
        Where-Object {
            $_.SessionId -ge 1 -and $_.ExecutablePath -and
            ([System.IO.Path]::GetFullPath($_.ExecutablePath) -eq $expected)
        })
    $serve = @($found | Where-Object { $_.CommandLine -match '\bserve\b' })
    if ($serve.Count -gt 0) { return $serve }
    $notClient = @($found | Where-Object {
        $_.CommandLine -notmatch '\b(call|mcp|status|autostart)\b'
    })
    if ($notClient.Count -gt 0) { return $notClient }
    return $found
}

function Get-CuaWindowRecords {
    param($Parsed)

    if ($null -eq $Parsed) { return @() }
    if ($Parsed -is [System.Collections.IEnumerable] -and $Parsed -isnot [string]) {
        $items = @($Parsed)
        if ($items.Count -gt 0 -and $null -ne $items[0] -and
            ($items[0].PSObject.Properties['window_id'] -or
             $items[0].PSObject.Properties['hwnd'])) {
            return $items
        }
    }
    foreach ($name in 'windows', 'items', 'result') {
        if ($Parsed.PSObject.Properties[$name] -and $null -ne $Parsed.$name) {
            return @($Parsed.$name)
        }
    }
    return @()
}

$cv = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
$os = Get-CimInstance Win32_OperatingSystem

# The machine SID is the account-domain SID of any local account; a
# generalized image must produce a different one per clone.
$machineSid = (Get-LocalUser -Name 'Administrator').SID.AccountDomainSid.Value

$identity = [ordered]@{
    computer_name = [Environment]::MachineName
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
Add-Check 'computer-name-present' ([bool]$identity.computer_name) $identity.computer_name | Out-Null

$sealContextPath = Join-Path $stateDir 'seal-context.json'
$sealContext = if (Test-Path -LiteralPath $sealContextPath) {
    Get-Content -LiteralPath $sealContextPath -Raw | ConvertFrom-Json
} else { $null }
$supportedSeal = $null -ne $sealContext -and $sealContext.sid -and
    $sealContext.sid -ne 'S-1-5-18' -and $sealContext.elevated -and $sealContext.session -ge 1
Add-Check 'source-sealed-as-interactive-administrator' ([bool]$supportedSeal) $sealContext | Out-Null

$ver = Get-Text "$env:SystemRoot\System32\cmd.exe" @('/c', 'ver')
Add-Check 'cmd-ver' ($ver -match 'Microsoft Windows') $ver | Out-Null

# Fail closed: a positive numeric statement that C: is fully decrypted, not
# the absence of a text match, which a swallowed manage-bde error would fake.
$encryption = [ordered]@{ method = $null; determined = $false; decrypted = $false }
try {
    $volume = Get-CimInstance -Namespace 'root\CIMV2\Security\MicrosoftVolumeEncryption' `
        -ClassName Win32_EncryptableVolume -Filter "DriveLetter='C:'" -ErrorAction Stop
    $conversion = Invoke-CimMethod -InputObject $volume -MethodName GetConversionStatus -ErrorAction Stop
    if ($conversion.ReturnValue -ne 0) {
        throw "GetConversionStatus returned 0x$('{0:X8}' -f $conversion.ReturnValue)"
    }

    $encryption.method = 'Win32_EncryptableVolume'
    $encryption['conversion_status'] = [int]$conversion.ConversionStatus
    $encryption['encryption_percentage'] = [int]$conversion.EncryptionPercentage
    $encryption['protection_status'] = [int]$volume.ProtectionStatus
    $encryption.determined = $true
    $encryption.decrypted = $encryption['conversion_status'] -eq 0 -and
        $encryption['encryption_percentage'] -eq 0 -and
        $encryption['protection_status'] -eq 0
} catch {
    $encryption['error'] = $_.Exception.Message
    $manageBde = "$env:SystemRoot\System32\manage-bde.exe"
    if (Test-Path -LiteralPath $manageBde) {
        $text = (& $manageBde '-status' 'C:' 2>&1 | Out-String)
        $encryption['manage_bde_exit'] = $LASTEXITCODE
        $encryption['status'] = $text.Trim()
        if ($LASTEXITCODE -eq 0 -and $text -match 'Conversion Status:\s+Fully Decrypted' -and
            $text -match 'Percentage Encrypted:\s+0([.,]0+)?%') {
            $encryption.method = 'manage-bde text'
            $encryption.determined = $true
            $encryption.decrypted = $true
        }
    } else {
        $encryption.method = 'none (no BitLocker capability present)'
        $encryption.determined = $true
        $encryption.decrypted = $true
    }
}

Add-Check 'volume-decrypted' ($encryption.determined -and $encryption.decrypted) $encryption | Out-Null

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

    Add-Check 'driver-present' (Test-Path -LiteralPath $driver) $driver | Out-Null
    Add-Check 'driver-proxy-present' (Test-Path -LiteralPath $proxy) $proxy | Out-Null

    $daemons = @(Get-CuaInteractiveDaemons -DriverPath $driver)
    $daemonDetail = @($daemons | ForEach-Object {
        [ordered]@{
            pid           = $_.ProcessId
            session       = $_.SessionId
            command_line  = $_.CommandLine
            executable    = $_.ExecutablePath
        }
    })
    $session = if ($daemons.Count -gt 0) { [int]$daemons[0].SessionId } else { -1 }
    Add-Check 'driver-daemon-running' ($daemons.Count -gt 0) $daemonDetail | Out-Null
    # Session 0 is the services session and has no attached desktop. Cua
    # status text has no session field and SYSTEM cannot read the pid file.
    Add-Check 'driver-session-interactive' ($session -ge 1) `
        "session=$session pid=$(if ($daemons.Count -gt 0) { $daemons[0].ProcessId } else { 'none' })" | Out-Null

    $status = Get-Text $proxy @('status', '--socket', $pipe)
    Add-Check 'driver-status-via-proxy' `
        (($status -match 'running') -and ($status -notmatch 'not running')) $status | Out-Null

    $windowsText = Get-Text $proxy @('call', '--socket', $pipe, 'list_windows', '{}')
    $windowsParsed = Convert-CuaJson $windowsText
    $windowRecords = @(Get-CuaWindowRecords $windowsParsed)
    $windowOk = $windowRecords.Count -gt 0 -and (
        $windowRecords[0].PSObject.Properties['window_id'] -or
        $windowRecords[0].PSObject.Properties['hwnd'] -or
        $windowRecords[0].PSObject.Properties['title']
    )
    Add-Check 'driver-list-windows' $windowOk ([ordered]@{
        count   = $windowRecords.Count
        sample  = @($windowRecords | Select-Object -First 3)
        raw     = $windowsText.Substring(0, [Math]::Min(400, $windowsText.Length))
    }) | Out-Null

    $appsText = Get-Text $proxy @('call', '--socket', $pipe, 'list_apps', '{}')
    $appsParsed = Convert-CuaJson $appsText
    $apps = @()
    if ($null -ne $appsParsed -and $appsParsed.PSObject.Properties['apps']) {
        $apps = @($appsParsed.apps)
    } elseif ($null -ne $appsParsed -and $appsParsed -is [System.Collections.IEnumerable] -and $appsParsed -isnot [string]) {
        $apps = @($appsParsed)
    }
    Add-Check 'driver-list-apps' ($apps.Count -gt 0) ([ordered]@{
        count = $apps.Count
        raw   = $appsText.Substring(0, [Math]::Min(400, $appsText.Length))
    }) | Out-Null

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
    $desktopState = Get-Text $proxy @('call', '--socket', $pipe, '--screenshot-out-file', $shot, 'get_desktop_state', '{}')
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
    Add-Check 'no-proxy-installed' (-not (Test-Path -LiteralPath $proxy)) $proxy | Out-Null
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
