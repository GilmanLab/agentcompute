#Requires -Version 5.1
<#
.SYNOPSIS
    The single provisioning entry point for both Windows images.

.DESCRIPTION
    The answer file runs exactly one FirstLogonCommands entry, and that entry
    runs this script. Microsoft now starts every FirstLogonCommands entry at
    the same time regardless of the SynchronousCommand name, so ordering has to
    live inside one script instead of across several entries.

    On Windows 11 this runs in the automation user's interactive logon, which
    is the only place `cua-driver autostart enable` can register a task that
    lands in Session 1+. On Server Core it runs in the one-shot Administrator
    logon and installs no GUI automation at all.

    Everything it installs comes from the read-only payload volume and is hash
    checked against the values repack.py rendered from pins.lock.yaml. The
    script never reaches the network.

    It is idempotent: re-running it re-verifies and repairs rather than
    duplicating state, so a failed bake can be retried in place.
#>
[CmdletBinding()]
param(
    [string] $PayloadRoot
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$script:StateDir = Join-Path $env:ProgramData 'agentcompute'
$script:LogDir = Join-Path $script:StateDir 'logs'
$script:ReportPath = Join-Path $script:StateDir 'bootstrap-report.json'
$script:Steps = [System.Collections.Generic.List[object]]::new()
$script:Facts = [ordered]@{}

function Write-Log {
    param([string] $Message)
    $stamp = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ss.fffZ')
    Write-Host "[$stamp] $Message"
}

function Invoke-Step {
    param(
        [Parameter(Mandatory)] [string] $Name,
        [Parameter(Mandatory)] [scriptblock] $Body
    )

    $started = Get-Date
    Write-Log "step $Name : start"
    try {
        $result = & $Body
        $script:Steps.Add([ordered]@{
            name     = $Name
            status   = 'ok'
            seconds  = [math]::Round(((Get-Date) - $started).TotalSeconds, 3)
            detail   = $result
        })
        Write-Log "step $Name : ok"
        return $result
    } catch {
        $script:Steps.Add([ordered]@{
            name    = $Name
            status  = 'failed'
            seconds = [math]::Round(((Get-Date) - $started).TotalSeconds, 3)
            detail  = $_.Exception.Message
        })
        Write-Log "step $Name : FAILED: $($_.Exception.Message)"
        throw
    }
}

function Find-PayloadRoot {
    if ($PayloadRoot) {
        if (-not (Test-Path (Join-Path $PayloadRoot 'agentcompute\config.json'))) {
            throw "No agentcompute\config.json under the supplied payload root $PayloadRoot"
        }

        return $PayloadRoot
    }

    foreach ($drive in Get-PSDrive -PSProvider FileSystem) {
        if (-not $drive.Root) { continue }
        $candidate = Join-Path $drive.Root 'agentcompute\config.json'
        if (Test-Path -LiteralPath $candidate) {
            return $drive.Root.TrimEnd('\')
        }
    }

    throw 'Payload volume not found: no drive carries agentcompute\config.json'
}

function Find-AgentMedia {
    # The agent CD-ROM label is not part of the documented Incus contract for
    # Windows, but its contents are: install.ps1 plus incus-agent.exe.
    foreach ($drive in Get-PSDrive -PSProvider FileSystem) {
        if (-not $drive.Root) { continue }
        $installer = Join-Path $drive.Root 'install.ps1'
        $agent = Join-Path $drive.Root 'incus-agent.exe'
        if ((Test-Path -LiteralPath $installer) -and (Test-Path -LiteralPath $agent)) {
            return $drive.Root.TrimEnd('\')
        }
    }

    throw 'Incus agent CD-ROM not found: no drive carries install.ps1 and incus-agent.exe'
}

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

function Invoke-Native {
    param(
        [Parameter(Mandatory)] [string] $FilePath,
        [string[]] $Arguments = @(),
        [int[]] $AllowedExitCodes = @(0),
        [switch] $PassThruOutput
    )

    $stdout = New-TemporaryFile
    $stderr = New-TemporaryFile
    try {
        $process = Start-Process -FilePath $FilePath -ArgumentList $Arguments -Wait -PassThru `
            -NoNewWindow -RedirectStandardOutput $stdout -RedirectStandardError $stderr
        $out = (Get-Content -LiteralPath $stdout -Raw -ErrorAction SilentlyContinue)
        $err = (Get-Content -LiteralPath $stderr -Raw -ErrorAction SilentlyContinue)
        if ($AllowedExitCodes -notcontains $process.ExitCode) {
            throw "$FilePath $($Arguments -join ' ') exited $($process.ExitCode): $err$out"
        }

        if ($PassThruOutput) { return "$out$err" }
        return $null
    } finally {
        Remove-Item -LiteralPath $stdout, $stderr -Force -ErrorAction SilentlyContinue
    }
}

function Initialize-StateDir {
    param([Parameter(Mandatory)] [string] $AutomationUser)

    New-Item -ItemType Directory -Force -Path $script:StateDir, $script:LogDir | Out-Null

    # The Driver daemon runs as the automation user in Session 1+ and writes
    # screenshots here; the Incus agent service runs as SYSTEM and reads and
    # removes them. Make both explicit instead of relying on the inherited
    # ProgramData ACL, which only grants CREATOR OWNER write.
    $acl = Get-Acl -LiteralPath $script:StateDir
    $rights = [System.Security.AccessControl.FileSystemRights]::Modify
    $inherit = [System.Security.AccessControl.InheritanceFlags]'ContainerInherit, ObjectInherit'
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule(
        $AutomationUser, $rights, $inherit,
        [System.Security.AccessControl.PropagationFlags]::None,
        [System.Security.AccessControl.AccessControlType]::Allow)
    $acl.SetAccessRule($rule)
    Set-Acl -LiteralPath $script:StateDir -AclObject $acl

    return (Get-Acl -LiteralPath $script:StateDir).Access |
        Where-Object { $_.IdentityReference -like "*$AutomationUser" } |
        ForEach-Object { "$($_.IdentityReference)=$($_.FileSystemRights)" }
}

function Install-IncusAgent {
    $media = Find-AgentMedia
    $installer = Join-Path $media 'install.ps1'
    Write-Log "installing Incus agent from $installer"
    & $installer | Out-Host

    $service = Get-Service -Name 'incus-agent' -ErrorAction SilentlyContinue
    if (-not $service) {
        throw "Incus agent install.ps1 completed but no incus-agent service exists"
    }

    if ($service.Status -ne 'Running') {
        Start-Service -Name 'incus-agent'
        $service = Get-Service -Name 'incus-agent'
    }

    return [ordered]@{
        media   = $media
        service = $service.Status.ToString()
        start   = (Get-CimInstance Win32_Service -Filter "Name='incus-agent'").StartMode
    }
}

function Disable-OnlineServicing {
    # A bake must install exactly the pinned servicing set. Automatic updates
    # would silently change the captured image, so they are switched off with
    # the documented policy keys before anything else is installed.
    $au = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU'
    New-Item -Path $au -Force | Out-Null
    Set-ItemProperty -Path $au -Name 'NoAutoUpdate' -Type DWord -Value 1
    Set-ItemProperty -Path $au -Name 'AUOptions' -Type DWord -Value 1

    # Windows 11 24H2 and later can turn on device encryption by itself. A disk
    # sealed to the build VM's vTPM would be useless as an image, so refuse it
    # up front and verify with manage-bde in finalize.ps1.
    $bitlocker = 'HKLM:\SYSTEM\CurrentControlSet\Control\BitLocker'
    New-Item -Path $bitlocker -Force | Out-Null
    Set-ItemProperty -Path $bitlocker -Name 'PreventDeviceEncryption' -Type DWord -Value 1

    return [ordered]@{
        NoAutoUpdate            = (Get-ItemProperty -Path $au).NoAutoUpdate
        PreventDeviceEncryption = (Get-ItemProperty -Path $bitlocker).PreventDeviceEncryption
    }
}

function Install-PinnedServicing {
    param(
        [Parameter(Mandatory)] [string] $Root,
        [Parameter(Mandatory)] [object[]] $Packages
    )

    $applied = @()
    foreach ($package in $Packages) {
        $path = Join-Path $Root $package.file
        Assert-FileHash -Path $path -Expected $package.sha256 | Out-Null
        Write-Log "applying servicing package $($package.file)"
        # 3010 is the documented reboot-required exit code and is expected for
        # cumulative packages; the bake reboots after bootstrap either way.
        Invoke-Native -FilePath "$env:SystemRoot\System32\dism.exe" `
            -Arguments @('/Online', '/Quiet', '/NoRestart', "/Add-Package:/PackagePath:$path") `
            -AllowedExitCodes @(0, 3010) | Out-Null
        $applied += $package.file
    }

    return [ordered]@{ count = $applied.Count; packages = $applied }
}

function Install-CuaDriver {
    param(
        [Parameter(Mandatory)] [string] $Root,
        [Parameter(Mandatory)] [object] $Pin,
        [Parameter(Mandatory)] [string] $InstallDir
    )

    $archive = Join-Path $Root $Pin.file
    Assert-FileHash -Path $archive -Expected $Pin.sha256 | Out-Null

    $staging = Join-Path $env:TEMP ('cua-driver-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $staging | Out-Null
    try {
        Expand-Archive -LiteralPath $archive -DestinationPath $staging -Force
        $source = Join-Path $staging $Pin.archive_root
        if (-not (Test-Path -LiteralPath $source)) {
            throw "Archive $($Pin.file) does not contain the pinned root $($Pin.archive_root)"
        }

        New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
        Copy-Item -Path (Join-Path $source '*') -Destination $InstallDir -Recurse -Force
    } finally {
        Remove-Item -LiteralPath $staging -Recurse -Force -ErrorAction SilentlyContinue
    }

    $exe = Join-Path $InstallDir 'cua-driver.exe'
    if (-not (Test-Path -LiteralPath $exe)) {
        throw "cua-driver.exe missing from $InstallDir after extraction"
    }

    # Machine PATH so both the interactive daemon and the Session 0 agent can
    # invoke the same binary without a per-user profile.
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (($machinePath -split ';') -notcontains $InstallDir) {
        [Environment]::SetEnvironmentVariable('Path', "$machinePath;$InstallDir", 'Machine')
    }

    $env:Path = "$env:Path;$InstallDir"

    $version = (Invoke-Native -FilePath $exe -Arguments @('--version') -PassThruOutput).Trim()

    # Session 0 has no interactive desktop, so the daemon has to be started by
    # a task registered with LogonType Interactive. `enable` registers it and
    # `kick` starts it now instead of at the next logon. Both must run from
    # this interactive logon.
    Invoke-Native -FilePath $exe -Arguments @('autostart', 'enable') -PassThruOutput | Out-Null
    Invoke-Native -FilePath $exe -Arguments @('autostart', 'kick') -PassThruOutput | Out-Null

    $status = $null
    for ($attempt = 1; $attempt -le 30; $attempt++) {
        Start-Sleep -Seconds 2
        try {
            $status = (Invoke-Native -FilePath $exe -Arguments @('status') -PassThruOutput).Trim()
        } catch {
            $status = $_.Exception.Message
            continue
        }

        # "is not running" contains "running", so require the positive
        # statement and the absence of the negative one.
        if ($status -match 'running' -and $status -notmatch 'not running') { break }
    }

    if (-not ($status -match 'running') -or $status -match 'not running') {
        throw "Cua Driver daemon did not come up in the interactive session: $status"
    }

    $autostart = (Invoke-Native -FilePath $exe -Arguments @('autostart', 'status') -PassThruOutput).Trim()
    $session = if ($status -match '(?m)session:\s*(\S+)') { $Matches[1] } else { $null }

    return [ordered]@{
        version          = $version
        executable       = $exe
        status           = $status
        session          = $session
        autostart_status = $autostart
    }
}

function Install-UltraVnc {
    param(
        [Parameter(Mandatory)] [string] $Root,
        [Parameter(Mandatory)] [object] $Pin
    )

    $setup = Join-Path $Root $Pin.file
    Assert-FileHash -Path $setup -Expected $Pin.sha256 | Out-Null

    $inf = Join-Path $Root 'ultravnc.inf'
    $ini = Join-Path $Root 'ultravnc.ini'
    $log = Join-Path $script:LogDir 'ultravnc-setup.log'

    Invoke-Native -FilePath $setup -Arguments @(
        '/silent', '/norestart', "/loadinf=$inf", "/log=$log"
    ) | Out-Null

    $winvnc = Get-ChildItem -Path "$env:ProgramFiles", "${env:ProgramFiles(x86)}" `
        -Filter 'winvnc.exe' -Recurse -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if (-not $winvnc) {
        throw "UltraVNC setup completed but winvnc.exe was not installed (see $log)"
    }

    # The server reads ultravnc.ini from its own directory. AuthRequired=0 keeps
    # the image free of a baked reusable credential, the same posture the Linux
    # desktop image uses for X0tigervnc; reachability is limited by the sandbox
    # network and the firewall rule below.
    Copy-Item -LiteralPath $ini -Destination (Join-Path $winvnc.DirectoryName 'ultravnc.ini') -Force

    if (Get-Service -Name $Pin.service -ErrorAction SilentlyContinue) {
        Invoke-Native -FilePath $winvnc.FullName -Arguments @('-uninstall') -AllowedExitCodes @(0, 1) | Out-Null
        Start-Sleep -Seconds 2
    }

    # A boot service, not a session process: the fallback console has to answer
    # at the login screen, before any interactive logon exists.
    Invoke-Native -FilePath $winvnc.FullName -Arguments @('-install') -AllowedExitCodes @(0, 1) | Out-Null
    Start-Sleep -Seconds 2

    $service = Get-Service -Name $Pin.service -ErrorAction SilentlyContinue
    if (-not $service) {
        throw "winvnc -install did not create the $($Pin.service) service (see $log)"
    }

    Set-Service -Name $Pin.service -StartupType Automatic
    if ($service.Status -ne 'Running') { Start-Service -Name $Pin.service }

    $ruleName = 'agentcompute UltraVNC (sandbox subnet only)'
    Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule -ErrorAction SilentlyContinue
    New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Action Allow `
        -Protocol TCP -LocalPort $Pin.port -RemoteAddress LocalSubnet -Profile Any | Out-Null

    $listening = $false
    for ($attempt = 1; $attempt -le 15; $attempt++) {
        if (Get-NetTCPConnection -State Listen -LocalPort $Pin.port -ErrorAction SilentlyContinue) {
            $listening = $true
            break
        }

        Start-Sleep -Seconds 2
    }

    if (-not $listening) {
        throw "UltraVNC service is not listening on TCP $($Pin.port)"
    }

    return [ordered]@{
        version    = $Pin.version
        executable = $winvnc.FullName
        service    = (Get-Service -Name $Pin.service).Status.ToString()
        port       = $Pin.port
        firewall   = $ruleName
        auth       = 'none (AuthRequired=0), reachable only from the sandbox subnet'
    }
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
    Set-ItemProperty -Path $winlogon -Name 'DefaultDomainName' -Value $env:COMPUTERNAME -Type String
    Set-ItemProperty -Path $winlogon -Name 'DefaultPassword' -Value '' -Type String
    foreach ($stale in 'AutoLogonCount', 'AutoLogonSID') {
        Remove-ItemProperty -Path $winlogon -Name $stale -ErrorAction SilentlyContinue
    }

    $values = Get-ItemProperty -Path $winlogon
    return [ordered]@{
        AutoAdminLogon    = $values.AutoAdminLogon
        DefaultUserName   = $values.DefaultUserName
        DefaultDomainName = $values.DefaultDomainName
        AutoLogonCount    = (Get-ItemProperty -Path $winlogon -Name 'AutoLogonCount' -ErrorAction SilentlyContinue).AutoLogonCount
    }
}


function Copy-CaptureAssets {
    param([Parameter(Mandatory)] [string] $Root)

    # finalize.ps1, smoke.ps1 and the deploy answer file have to outlive the
    # payload volume: the media is detached before Sysprep, and the clone runs
    # the deploy first-logon repair from disk.
    $imageDir = 'C:\image'
    New-Item -ItemType Directory -Force -Path $imageDir | Out-Null
    $source = Join-Path $Root 'agentcompute'
    Copy-Item -LiteralPath (Join-Path $source 'finalize.ps1') -Destination $script:StateDir -Force
    Copy-Item -LiteralPath (Join-Path $source 'smoke.ps1') -Destination $script:StateDir -Force
    Copy-Item -LiteralPath (Join-Path $source 'deploy-firstlogon.ps1') -Destination $script:StateDir -Force
    Copy-Item -LiteralPath (Join-Path $source 'deploy-unattend.xml') -Destination $imageDir -Force

    return [ordered]@{
        state_dir   = $script:StateDir
        deploy_answer = Join-Path $imageDir 'deploy-unattend.xml'
    }
}

$status = 'ok'
$failure = $null
New-Item -ItemType Directory -Force -Path $script:StateDir, $script:LogDir | Out-Null
$transcript = Join-Path $script:LogDir 'bootstrap.log'
Start-Transcript -Path $transcript -Append | Out-Null
$started = (Get-Date).ToUniversalTime()

try {
    $root = Invoke-Step 'locate-payload' { Find-PayloadRoot }
    $config = Get-Content -LiteralPath (Join-Path $root 'agentcompute\config.json') -Raw |
        ConvertFrom-Json
    $script:Facts['image'] = $config.image
    $script:Facts['role'] = $config.role
    $script:Facts['build_id'] = $config.build_id
    $script:Facts['payload_root'] = $root
    Write-Log "payload $root image=$($config.image) role=$($config.role) build=$($config.build_id)"

    $payloadRoot = Join-Path $root 'agentcompute\payload'

    $script:Facts['state_dir_acl'] = Invoke-Step 'state-dir' {
        Initialize-StateDir -AutomationUser $config.automation_user
    }
    $script:Facts['os'] = Invoke-Step 'os-identity' {
        $os = Get-CimInstance Win32_OperatingSystem
        [ordered]@{
            caption = $os.Caption
            version = $os.Version
            build   = $os.BuildNumber
            ubr     = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').UBR
            install_type = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').InstallationType
        }
    }
    $script:Facts['servicing_policy'] = Invoke-Step 'disable-online-servicing' { Disable-OnlineServicing }
    $script:Facts['agent'] = Invoke-Step 'incus-agent' { Install-IncusAgent }
    $script:Facts['servicing'] = Invoke-Step 'pinned-servicing' {
        Install-PinnedServicing -Root $payloadRoot -Packages @($config.payload.servicing)
    }

    if ($config.role -eq 'desktop') {
        $script:Facts['session'] = Invoke-Step 'interactive-session' {
            $query = (Invoke-Native -FilePath "$env:SystemRoot\System32\query.exe" `
                -Arguments @('session') -PassThruOutput -AllowedExitCodes @(0, 1)).Trim()
            [ordered]@{
                user    = "$env:USERDOMAIN\$env:USERNAME"
                query   = $query
                session = (Get-Process -Id $PID).SessionId
            }
        }
        $script:Facts['cua_driver'] = Invoke-Step 'cua-driver' {
            Install-CuaDriver -Root $payloadRoot -Pin $config.payload.cua_driver `
                -InstallDir $config.driver_dir
        }
        $script:Facts['autologon'] = Invoke-Step 'persistent-autologon' {
            Set-PersistentAutoLogon -User $config.automation_user
        }
        $script:Facts['ultravnc'] = Invoke-Step 'ultravnc' {
            Install-UltraVnc -Root $payloadRoot -Pin $config.payload.ultravnc
        }
    } else {
        Write-Log "role $($config.role): no desktop automation is installed"
    }

    $script:Facts['capture_assets'] = Invoke-Step 'capture-assets' { Copy-CaptureAssets -Root $root }
} catch {
    $status = 'failed'
    $failure = $_.Exception.Message
} finally {
    $report = [ordered]@{
        schema_version = 1
        status         = $status
        error          = $failure
        started        = $started.ToString('yyyy-MM-ddTHH:mm:ssZ')
        finished       = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
        seconds        = [math]::Round(((Get-Date).ToUniversalTime() - $started).TotalSeconds, 3)
        facts          = $script:Facts
        steps          = $script:Steps
    }

    $report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $script:ReportPath -Encoding UTF8
    Write-Log "bootstrap $status ; report at $script:ReportPath"
    Stop-Transcript | Out-Null
}

if ($status -ne 'ok') { exit 1 }
exit 0
