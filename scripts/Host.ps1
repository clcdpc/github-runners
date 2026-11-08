#requires -Version 7.4

function Test-RunnerAdministrator {
    [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function ConvertTo-RunnerIPv4Number([string]$Address) {
    $bytes = [Net.IPAddress]::Parse($Address).GetAddressBytes()
    if ($bytes.Length -ne 4) { throw 'IPv4 required.' }
    [uint64]$bytes[0]*16777216 + [uint64]$bytes[1]*65536 + [uint64]$bytes[2]*256 + $bytes[3]
}
function Test-RunnerSubnetOverlap([string]$First, [string]$Second) {
    $a=$First.Split('/'); $b=$Second.Split('/')
    $length=[Math]::Min([int]$a[1],[int]$b[1])
    $size=[Math]::Pow(2,32-$length)
    [Math]::Floor((ConvertTo-RunnerIPv4Number $a[0])/$size) -eq [Math]::Floor((ConvertTo-RunnerIPv4Number $b[0])/$size)
}

function Get-RunnerHostReport([string]$Config) {
    $settings=(Get-RunnerSettings $Config).Settings
    $os=Get-CimInstance Win32_OperatingSystem
    $computer=Get-CimInstance Win32_ComputerSystem
    $processors=@(Get-CimInstance Win32_Processor)
    $errors=[Collections.Generic.List[string]]::new()
    $warnings=[Collections.Generic.List[string]]::new()
    $admin=Test-RunnerAdministrator
    $edition=(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').EditionID
    if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64' -or $edition -notmatch 'Professional|Enterprise|Education|Server') { $errors.Add('Windows Pro, Enterprise, Education or Server x64 with Hyper-V is required.') }
    $feature=@(Get-CimInstance Win32_OptionalFeature -Filter "Name='Microsoft-Hyper-V-All'" -ErrorAction SilentlyContinue)
    $featureState=if ($feature.Count) { [string]$feature[0].InstallState } else { 'Unavailable' }
    $module=[bool](Get-Module -ListAvailable Hyper-V)
    if (-not $module -or $featureState -eq '2') { $errors.Add('Hyper-V is not installed/enabled. In elevated PowerShell 7 run .\runners.ps1 PrepareHost; it enables Microsoft-Hyper-V-All with -NoRestart. Restart Windows yourself if requested, then rerun PrepareHost.') }
    $vmAccess=$false; $switches=@(); $nats=@()
    if ($module) {
        try { Import-Module Hyper-V; $null=Get-VM -ErrorAction Stop; $switches=@(Get-VMSwitch | Select-Object Name,Id,SwitchType); $vmAccess=$true }
        catch { $errors.Add('Hyper-V management access failed: ' + $_.Exception.Message + '. PrepareHost can grant Hyper-V Administrators membership; sign out/in afterward.') }
    }
    $virtualization=if ($computer.HypervisorPresent) { 'Hypervisor detected; nested extensions must be proven by TestHyperV, not WMI CPU flags' } elseif (@($processors | Where-Object { $_.VirtualizationFirmwareEnabled -and $_.SecondLevelAddressTranslationExtensions }).Count) { 'CPU virtualization and SLAT exposed' } else { 'Virtualization extensions unavailable'; $errors.Add('Virtualization/SLAT not exposed. Nested virtualization was expected; physical-host changes are outside this tool. No physical host was modified.') }
    if ([long]$os.FreePhysicalMemory*1KB -lt ($settings.hyperv.memoryGiB+$settings.hyperv.reserveMemoryGiB)*1GB) { $warnings.Add('Current free memory is below guest plus reserve. Provisioning will check again before every VM.') }
    $free=@{}
    $requiredDiskGiB=[long]$settings.hyperv.diskGrowthGiB+$settings.hyperv.reserveDiskGiB
    foreach ($path in @($settings.controller.dataDirectory,$settings.hyperv.imageDirectory)) {
        $existing=$path
        while (-not (Test-Path -LiteralPath $existing)) { $existing=Split-Path $existing -Parent }
        $volume=Get-Volume -FilePath ($existing.TrimEnd('\')+'\')
        $free[$path]=[Math]::Round($volume.SizeRemaining/1GB,1)
        if ($volume.SizeRemaining -lt $requiredDiskGiB*1GB) { $errors.Add("$path has $($free[$path]) GiB free; at least $requiredDiskGiB GiB is required for one guest's disk-growth budget and Windows reserve. Free space or relocate the configured directories; no automatic pruning occurs.") }
    }
    try { $nats=@(Get-NetNat -ErrorAction Stop | Select-Object Name,InternalIPInterfaceAddressPrefix) }
    catch { $warnings.Add('NAT inventory unavailable under this token: ' + $_.Exception.Message + '. PrepareHost requires elevation.') }
    foreach ($tool in @($settings.hyperv.ssh,$settings.hyperv.sshKeygen)) { if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { $errors.Add("OpenSSH tool missing: $tool. In elevated PowerShell run Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0") } }
    [ordered]@{ checkedAt=(Get-Date).ToString('o'); os=$os.Caption; edition=$edition; architecture=$os.OSArchitecture; elevated=$admin; hypervFeatureInstallState=$featureState; hypervModule=$module; managementAccess=$vmAccess; virtualization=$virtualization; logicalCPUs=$computer.NumberOfLogicalProcessors; freeMemoryGiB=[Math]::Round([long]$os.FreePhysicalMemory*1KB/1GB,1); freeDiskGiB=$free; switches=$switches; nats=$nats; warnings=@($warnings); errors=@($errors); ready=$errors.Count -eq 0; nestedBootValidated=$false }
}

function Test-RunnerHost([string]$Config) {
    $report=Get-RunnerHostReport $Config
    $settings=(Get-RunnerSettings $Config).Settings
    Write-RunnerJson (Join-Path $settings.controller.dataDirectory 'host-validation.json') $report
    foreach ($warning in $report.warnings) { Write-Warning $warning }
    if (-not $report.ready) { throw ($report.errors -join "`n") }
    Write-Host 'Windows, Hyper-V management, OpenSSH and host capacity checks passed. Guest boot requires TestHyperV.'
}

function Initialize-RunnerNetwork([string]$Config) {
    $settings=(Get-RunnerSettings $Config).Settings
    $net=$settings.hyperv
    $path=Join-Path $settings.controller.dataDirectory 'network.json'
    $lock=Enter-RunnerFileLock ($path+'.lock')
    try {
        $record=Read-RunnerJson $path
        if ($record) {
            foreach ($key in @('subnet','gateway','switchName','natName')) { if ($record[$key] -cne $net.$key) { throw 'Saved network differs from configuration. Restore it; this tool never reconfigures an existing network.' } }
        } else {
            if (@(Get-VMSwitch | Where-Object Name -EQ $net.switchName).Count -or @(Get-NetNat | Where-Object Name -EQ $net.natName).Count) { throw 'Configured switch/NAT name already exists without this installation ownership record. Choose unused names; existing resources were preserved.' }
            $record=@{ version=1; owner=[guid]::NewGuid().ToString(); subnet=$net.subnet; gateway=$net.gateway; switchName=$net.switchName; natName=$net.natName; switchId=''; phase='intent' }
        }
        $switch=@(Get-VMSwitch | Where-Object Name -EQ $net.switchName)
        if ($switch.Count) {
            if ($switch.Count -ne 1 -or $switch[0].Notes -cne ('clc-runners-network/v1|'+$record.owner) -or $switch[0].SwitchType -ne 'Internal' -or ($record.switchId -and [string]$switch[0].Id -ne $record.switchId)) { throw 'Switch ownership mismatch; no network was changed.' }
            $switch=$switch[0]
        } else { $switch=$null }
        $adapter=if ($switch) { @(Get-NetAdapter | Where-Object Name -CEQ "vEthernet ($($net.switchName))") | Select-Object -First 1 } else { $null }
        $nats=@(Get-NetNat)
        foreach ($nat in $nats) {
            if ($nat.Name -cne $net.natName -or $nat.InternalIPInterfaceAddressPrefix -cne $net.subnet) { throw "WinNAT supports one internal prefix; existing NAT '$($nat.Name)' prevents dedicated NAT creation. It was preserved. Resolve the conflict explicitly before rerunning PrepareHost." }
        }
        foreach ($route in @(Get-NetRoute -AddressFamily IPv4)) {
            if ($route.DestinationPrefix -eq '0.0.0.0/0' -or ($adapter -and $route.InterfaceIndex -eq $adapter.ifIndex)) { continue }
            if (Test-RunnerSubnetOverlap $net.subnet $route.DestinationPrefix) { throw "Subnet $($net.subnet) overlaps route $($route.DestinationPrefix) on interface $($route.InterfaceIndex), including possible WSL/VPN networks. Choose a free subnet and allocation range before network creation." }
        }
        if (-not $switch) {
            Write-RunnerJson $path $record
            $switch=New-VMSwitch -Name $net.switchName -SwitchType Internal -Notes ('clc-runners-network/v1|'+$record.owner)
            $record.switchId=[string]$switch.Id; $record.phase='switch'; Write-RunnerJson $path $record
            $adapter=@(Get-NetAdapter | Where-Object Name -CEQ "vEthernet ($($net.switchName))") | Select-Object -First 1
        }
        if (-not $adapter) { throw 'Dedicated internal switch management adapter is unavailable.' }
        # MSFT_NetIPAddress reports an empty CIM query as ObjectNotFound. That
        # is expected for a new adapter; other network failures must still stop.
        try { $addresses=@(Get-NetIPAddress -InterfaceIndex $adapter.ifIndex -AddressFamily IPv4 -ErrorAction Stop) }
        catch {
            if ($_.CategoryInfo.Category -ne [Management.Automation.ErrorCategory]::ObjectNotFound -or $_.FullyQualifiedErrorId -notlike 'CmdletizationQuery_NotFound*') { throw }
            $addresses=@()
        }
        foreach ($address in $addresses) { if ($address.IPAddress -ne $net.gateway -and $address.IPAddress -notlike '169.254.*') { throw 'Owned switch has an unexpected IP address; refusing to reconfigure it.' } }
        foreach ($address in @($addresses | Where-Object IPAddress -EQ $net.gateway)) { if ($address.PrefixLength -ne [int]$net.subnet.Split('/')[1]) { throw 'Owned gateway prefix differs; restore its configuration explicitly before retrying.' } }
        if (-not @($addresses | Where-Object IPAddress -EQ $net.gateway).Count) { $null=New-NetIPAddress -InterfaceIndex $adapter.ifIndex -IPAddress $net.gateway -PrefixLength ([int]$net.subnet.Split('/')[1]) }
        if (-not $nats.Count) { $null=New-NetNat -Name $net.natName -InternalIPInterfaceAddressPrefix $net.subnet }
        $record.switchId=[string]$switch.Id; $record.phase='ready'; Write-RunnerJson $path $record
        Write-Host "Dedicated internal switch $($net.switchName), NAT $($net.subnet), gateway $($net.gateway) ready. Guest addresses/DNS come from NoCloud; no inbound port mappings were added."
    } finally { $lock.Dispose() }
}

function Initialize-RunnerHost([string]$Config, [Security.Principal.SecurityIdentifier]$AccountSid) {
    if (-not (Test-RunnerAdministrator)) { throw 'Host preparation requires elevated PowerShell 7. Open PowerShell as administrator under the controller account, cd to this repository, and run .\runners.ps1 PrepareHost. The controller itself uses Hyper-V Administrators rights.' }
    $edition=(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').EditionID
    if ($edition -notmatch 'Professional|Enterprise|Education|Server' -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') { throw 'Supported Windows Pro/Enterprise/Education/Server x64 edition required.' }
    if ((Get-CimInstance Win32_OperatingSystem).ProductType -ne 1) {
        Import-Module ServerManager
        if (-not (Get-WindowsFeature -Name Hyper-V).Installed) {
            $result=Install-WindowsFeature -Name Hyper-V -IncludeManagementTools -Restart:$false
            if (-not $result.Success) { throw 'Hyper-V server role installation failed; inspect Windows feature diagnostics.' }
            if ([string]$result.RestartNeeded -eq 'Yes') { throw 'Hyper-V installed. Restart Windows manually and rerun PrepareHost; no restart was initiated.' }
        }
    } else {
        $feature=Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All
        if ($feature.State -ne 'Enabled') {
            $result=Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All -All -NoRestart
            if ($result.RestartNeeded) { throw 'Hyper-V feature installed. Restart Windows manually, then run .\runners.ps1 PrepareHost again. No restart was initiated.' }
        }
    }
    Import-Module Hyper-V
    if ((Get-Service vmms).Status -ne 'Running') { Start-Service vmms }
    $sid=if ($AccountSid) { $AccountSid } else { [Security.Principal.WindowsIdentity]::GetCurrent().User }
    $group=Get-LocalGroup -SID 'S-1-5-32-578'
    if (-not @(Get-LocalGroupMember -Group $group | Where-Object { $_.SID -eq $sid }).Count) {
        Add-LocalGroupMember -Group $group -Member $sid.Value
        Write-Host "Added account $sid to Hyper-V Administrators. Sign out/in after preparation to refresh its normal user token."
    }
    $capability=Get-WindowsCapability -Online -Name 'OpenSSH.Client~~~~0.0.1.0'
    if ($capability.State -ne 'Installed') { $null=Add-WindowsCapability -Online -Name 'OpenSSH.Client~~~~0.0.1.0' }
    Initialize-RunnerNetwork $Config
    Test-RunnerHost $Config
    Install-RunnerImageTools $Config
}

function Ensure-RunnerHost([string]$Config) {
    Test-RunnerHost $Config
    $settings=(Get-RunnerSettings $Config).Settings
    if (-not (Test-Path -LiteralPath (Join-Path $settings.controller.dataDirectory 'network.json'))) {
        if (Test-RunnerAdministrator) { Initialize-RunnerNetwork $Config }
        else { throw 'Dedicated network is not prepared. In elevated PowerShell 7 run .\runners.ps1 PrepareHost; then Activate under the normal controller account.' }
    }
    Invoke-RunnerCommand check-hyperv $Config
}

function Install-RunnerImageTools([string]$Config) {
    $settings=(Get-RunnerSettings $Config).Settings
    $recipe=Get-Content -LiteralPath $settings.hyperv.imageRecipe -Raw | ConvertFrom-Json
    if (Test-Path -LiteralPath $settings.hyperv.qemuImg) {
        $version=(& $settings.hyperv.qemuImg --version) -join "`n"
        if ($LASTEXITCODE -or $version -notmatch ('(?m)^qemu-img version '+[regex]::Escape($recipe.qemuVersion)+'(?:\s|$)')) { throw 'Configured qemu-img version does not match image/recipe.json. Choose the intended version explicitly; existing software was not upgraded.' }
        return
    }
    if (-not (Get-Command winget.exe -ErrorAction SilentlyContinue)) { throw 'Install the QEMU Windows build linked by qemu.org, verify its published hash, and set hyperv.qemuImg; alternatively install App Installer/winget and rerun PrepareImage.' }
    # WinGet verifies the installer hash from Microsoft's versioned package
    # manifest. Never use --ignore-security-hash or download an unverified zip.
    & winget.exe install --id SoftwareFreedomConservancy.QEMU --exact --version $recipe.qemuVersion --source winget --silent --accept-package-agreements --accept-source-agreements --disable-interactivity
    if ($LASTEXITCODE -or -not (Test-Path -LiteralPath $settings.hyperv.qemuImg)) { throw 'QEMU installation failed or configured qemuImg path differs. Resolve the reported winget/elevation error and rerun PrepareImage.' }
    Install-RunnerImageTools $Config
}
