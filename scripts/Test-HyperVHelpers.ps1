#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Host.ps1')
. (Join-Path $PSScriptRoot 'HyperV.ps1')
$script:hvAssertions=0
function Assert-HV($Condition,[string]$Message) { $script:hvAssertions++; if (-not $Condition) { throw $Message } }
function Assert-HVFailure([scriptblock]$Action,[string]$Pattern) {
    $message='';try { & $Action | Out-Null } catch { $message=$_.Exception.Message }
    Assert-HV ($message -match $Pattern) "Expected '$Pattern', got '$message'"
}
$scratch=Join-Path ([IO.Path]::GetTempPath()) ('clc-hyperv-tests-'+[guid]::NewGuid().ToString('N'))
try {
    Protect-RunnerDirectory $scratch
    Assert-HV (Test-RunnerSubnetOverlap '192.168.231.0/24' '192.168.0.0/16') 'Nested subnet conflict missed'
    Assert-HV (-not (Test-RunnerSubnetOverlap '192.168.231.0/24' '172.20.0.0/20')) 'Distinct WSL subnet falsely overlaps'
    Assert-HV (Test-RunnerSubnetOverlap '192.168.231.0/24' '192.168.231.10/32') 'Host route conflict missed'
    Assert-HVFailure { Assert-CLCPath (Join-Path $scratch '../outside') $scratch } 'escapes'

    & {
        $config=Join-Path $scratch 'storage-config.json'
        $settings=Get-Content -LiteralPath (Join-Path $script:RunnerRoot 'config/controller.example.json') -Raw | ConvertFrom-Json -AsHashtable
        [void]$settings.Remove('storageRoot');Write-RunnerJson $config $settings
        $original=(Get-FileHash -LiteralPath $config).Hash
        $resolved=(Get-RunnerSettings $config).Settings
        Assert-HV ($resolved.storageRoot -eq $scratch -and $resolved.controller.dataDirectory -eq (Join-Path $scratch 'data')) 'An existing configuration changed its storage location'
        Assert-HV ((Get-FileHash -LiteralPath $config).Hash -eq $original) 'Reading a configuration rewrote its missing storage root'
        foreach ($root in @('', '.', 'unused/../runtime', (Join-Path $scratch 'absolute-runtime'))) {
            $settings.storageRoot=$root;Write-RunnerJson $config $settings
            $resolved=(Get-RunnerSettings $config).Settings
            $expected=if ($root -in @('', '.')) { $scratch } elseif ($root -eq 'unused/../runtime') { Join-Path $scratch 'runtime' } else { $root }
            Assert-HV ($resolved.storageRoot -eq $expected -and $resolved.controller.dataDirectory -eq (Join-Path $expected 'data') -and $resolved.controller.logDirectory -eq (Join-Path $expected 'logs')) 'Data/log paths did not follow storageRoot'
            Assert-HV ($resolved.controller.patFile -eq (Join-Path $expected 'secrets/github-pat.dpapi') -and $resolved.hyperv.imageDirectory -eq (Join-Path $expected 'images')) 'PAT/image paths did not follow storageRoot'
            Assert-HV ($resolved.hyperv.script -eq [IO.Path]::GetFullPath('../scripts/HyperV.ps1',$scratch) -and $resolved.hyperv.imageRecipe -eq [IO.Path]::GetFullPath('../image/recipe.json',$scratch) -and $resolved.hyperv.qemuImg -eq $settings.hyperv.qemuImg) 'Storage root redirected repository inputs or installed tools'
            Assert-HV (-not (Test-Path -LiteralPath $resolved.controller.dataDirectory)) 'Reading settings created runtime storage'
        }
        Write-RunnerJson ($config+'.effective.json') $resolved
        $settings.storageRoot='different-runtime';Write-RunnerJson $config $settings
        $fallback=(Get-RunnerSettings $config -EffectiveFallback).Settings
        Assert-HV ($fallback.storageRoot -eq $resolved.storageRoot -and $fallback.controller.dataDirectory -eq $resolved.controller.dataDirectory) 'Effective fallback redirected recovery to new storage'
        $settings.controller.dataDirectory=Join-Path $scratch 'override-data';$settings.controller.logDirectory=Join-Path $scratch 'override-logs'
        $settings.controller.patFile=Join-Path $scratch 'override-secrets/pat.dpapi';$settings.hyperv.imageDirectory=Join-Path $scratch 'override-images'
        Write-RunnerJson $config $settings;$resolved=(Get-RunnerSettings $config).Settings
        Assert-HV ($resolved.controller.dataDirectory -eq $settings.controller.dataDirectory -and $resolved.controller.logDirectory -eq $settings.controller.logDirectory -and $resolved.controller.patFile -eq $settings.controller.patFile -and $resolved.hyperv.imageDirectory -eq $settings.hyperv.imageDirectory) 'Absolute storage overrides were rebased'
    }

    & {
        $config=Join-Path $scratch 'legacy-config.json'
        # Migration fixtures must not depend on a production controller running.
        function Get-RunnerControllerProcesses { @() }
        $old=Get-Content -LiteralPath (Join-Path $script:RunnerRoot 'config/controller.example.json') -Raw | ConvertFrom-Json -AsHashtable
        [void]$old.Remove('hyperv');$old.wslc=@{executable=(Join-Path $scratch 'absent-legacy.exe');session='';cpus=2;memory='4G';commandTimeoutSeconds=60};$old.runners.image='legacy:1'
        $old.storageRoot='migration-storage';$old.controller.dataDirectory='migration-data';$old.controller.logDirectory='migration-logs';$old.github.repositories=@('asap-pocketbase')
        Write-RunnerJson $config $old
        $data=Join-Path $scratch 'migration-storage/migration-data';New-Item -ItemType Directory $data -Force | Out-Null
        Write-RunnerJson (Join-Path $data 'state.json') @{version=1;owner=[guid]::NewGuid().ToString();scaleSetId=42;scope='https://github.com/clcdpc|CLC-Runners|clc-linux|';runners=@{}}
        $pat=Join-Path $scratch 'migration-storage/secrets/github-pat.dpapi';New-Item -ItemType Directory (Split-Path $pat -Parent) -Force | Out-Null;[IO.File]::WriteAllBytes($pat,[byte[]](1,2,3,4))
        $before=(Get-FileHash -LiteralPath $pat).Hash;$original=(Get-FileHash -LiteralPath $config).Hash
        Update-RunnerConfig $config *> $null
        $updated=Read-RunnerJson $config
        Assert-HV (-not $updated.Contains('wslc') -and $updated.hyperv.memoryGiB -eq 10 -and $updated.hyperv.cpus -eq 2) 'Legacy runtime was not replaced with VM defaults'
        Assert-HV ($updated.github.repositories[0] -eq 'asap-pocketbase' -and $updated.controller.dataDirectory -eq 'migration-data') 'Migration changed compatible settings'
        Assert-HV ((Get-FileHash -LiteralPath $pat).Hash -eq $before -and (Get-FileHash -LiteralPath ($config+'.wslc-v1.bak')).Hash -eq $original) 'Migration changed credential or lost exact backup'
        $stamp=(Get-Item -LiteralPath $config).LastWriteTimeUtc
        Update-RunnerConfig $config
        Assert-HV ((Get-Item -LiteralPath $config).LastWriteTimeUtc -eq $stamp) 'Repeat migration unnecessarily rewrote configuration'
        $active=Join-Path $scratch 'active-legacy.json';Write-RunnerJson $active $old
        Write-RunnerJson (Join-Path $data 'state.json') @{version=1;owner=[guid]::NewGuid().ToString();scaleSetId=42;runners=@{active=@{}}}
        Assert-HVFailure { Update-RunnerConfig $active } 'Legacy journal contains runners'
        Assert-HV (-not (Test-Path -LiteralPath ($active+'.wslc-v1.bak'))) 'Rejected migration modified configuration'
    }

    & {
        $config=Join-Path $scratch 'capacity-config.json'
        $settings=Get-Content -LiteralPath (Join-Path $script:RunnerRoot 'config/controller.example.json') -Raw | ConvertFrom-Json -AsHashtable
        [void]$settings.hyperv.Remove('diskGrowthGiB')
        [void]$settings.controller.Remove('startupRunLevel')
        Write-RunnerJson $config $settings
        $original=(Get-FileHash -LiteralPath $config).Hash
        $local=Get-RunnerSettings $config
        Assert-HV ($local.Settings.hyperv.diskGrowthGiB -eq 20 -and (Get-FileHash -LiteralPath $config).Hash -eq $original) 'Existing configuration did not receive the growth default without being rewritten'
        Assert-HV ($local.Settings.controller.startupRunLevel -eq 'Limited' -and (Get-FileHash -LiteralPath $config).Hash -eq $original) 'Existing configuration did not retain limited startup without being rewritten'
        $settings.controller.startupRunLevel='highest';Write-RunnerJson $config $settings
        Assert-HVFailure { Get-RunnerSettings $config } 'startupRunLevel must be Limited or Highest'
        $settings.controller.startupRunLevel='Limited';Write-RunnerJson $config $settings
        $script:dataFree=35GB;$script:imageFree=35GB
        function Test-RunnerAdministrator { $false }
        function Get-ItemProperty { param($Path) [pscustomobject]@{EditionID='Professional'} }
        function Get-CimInstance { param($ClassName,$Filter,$ErrorAction)
            switch ($ClassName) {
                'Win32_OperatingSystem' { [pscustomobject]@{Caption='Windows';OSArchitecture='64-bit';FreePhysicalMemory=(64GB/1KB)} }
                'Win32_ComputerSystem' { [pscustomobject]@{NumberOfLogicalProcessors=16;HypervisorPresent=$true} }
                'Win32_OptionalFeature' { [pscustomobject]@{InstallState=1} }
                'Win32_Processor' { [pscustomobject]@{VirtualizationFirmwareEnabled=$true;SecondLevelAddressTranslationExtensions=$true} }
            }
        }
        function Get-Module { param([switch]$ListAvailable,$Name) $true }
        function Import-Module { param($Name) }
        function Get-VM { param($ErrorAction) }
        function Get-VMSwitch { }
        function Get-NetNat { param($ErrorAction) }
        function Get-Command { param($Name,$ErrorAction) $true }
        function Get-Volume { param($FilePath) [pscustomobject]@{SizeRemaining=$(if ($FilePath -eq ($local.Settings.controller.dataDirectory+'\')) { $script:dataFree } else { $script:imageFree })} }
        # Distinct existing paths ensure both configured volumes are checked.
        New-Item -ItemType Directory -Path $local.Settings.controller.dataDirectory,$local.Settings.hyperv.imageDirectory | Out-Null
        Assert-HV (Get-RunnerHostReport $config).ready 'Host rejected the default 35 GiB free-space boundary'
        $script:dataFree=35GB-1
        Assert-HV (@((Get-RunnerHostReport $config).errors | Where-Object { $_ -match 'at least 35 GiB' }).Count -eq 1) 'Host did not enforce the data-volume boundary'
        $script:dataFree=35GB;$script:imageFree=35GB-1
        Assert-HV (@((Get-RunnerHostReport $config).errors | Where-Object { $_ -match 'at least 35 GiB' }).Count -eq 1) 'Host did not enforce the image-volume boundary'
        $settings.hyperv.diskGrowthGiB=10;Write-RunnerJson $config $settings
        $script:dataFree=25GB;$script:imageFree=25GB
        Assert-HV (Get-RunnerHostReport $config).ready 'Host ignored the configured smaller growth budget'
        $settings.hyperv.cpus=16;Write-RunnerJson $config $settings
        Assert-HV (Get-RunnerHostReport $config).ready 'Host still reserved a physical CPU instead of permitting vCPU overcommit'
    }

    & {
        $config=Join-Path $scratch 'network-config.json'
        $local=Initialize-RunnerConfig $config
        $script:switches=@();$script:nats=@();$script:ips=@();$script:routes=@();$script:netCreates=0;$script:ipFailure=''
        function Get-VMSwitch { $script:switches }
        function Get-NetNat { $script:nats }
        function Get-NetRoute { param($AddressFamily) $script:routes }
        function Get-NetAdapter { if ($script:switches.Count) { [pscustomobject]@{Name='vEthernet (CLC-Runners)';ifIndex=77} } }
        function Get-NetIPAddress {
            [CmdletBinding()]param($InterfaceIndex,$AddressFamily)
            if ($script:ipFailure) { Write-Error 'IPv4 query access denied' -Category PermissionDenied -ErrorId 'AccessDenied'; return }
            if (-not $script:ips.Count) { Write-Error 'No matching MSFT_NetIPAddress objects found' -Category ObjectNotFound -ErrorId 'CmdletizationQuery_NotFound'; return }
            $script:ips
        }
        function New-VMSwitch { param($Name,$SwitchType,$Notes) $script:netCreates++;$item=[pscustomobject]@{Name=$Name;Id=[guid]::NewGuid();SwitchType=$SwitchType;Notes=$Notes};$script:switches=@($item);$item }
        function New-NetIPAddress { param($InterfaceIndex,$IPAddress,$PrefixLength) $script:netCreates++;$script:ips=@([pscustomobject]@{IPAddress=$IPAddress;PrefixLength=$PrefixLength}) }
        function New-NetNat { param($Name,$InternalIPInterfaceAddressPrefix) $script:netCreates++;$script:nats=@([pscustomobject]@{Name=$Name;InternalIPInterfaceAddressPrefix=$InternalIPInterfaceAddressPrefix}) }
        Initialize-RunnerNetwork $config *> $null
        $first=Read-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'network.json')
        Initialize-RunnerNetwork $config *> $null
        $second=Read-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'network.json')
        Assert-HV ($netCreates -eq 3 -and $first.owner -eq $second.owner) 'Repeated network preparation mutated/recreated resources'

        # A fresh checkout must require the original ownership record, then reuse
        # the verified network without recreating it or changing the source record.
        $newConfig=Join-Path $scratch 'relocated-checkout/local/controller.json'
        $newLocal=Initialize-RunnerConfig $newConfig
        $sourceRecord=Join-Path $local.Settings.controller.dataDirectory 'network.json'
        $targetRecord=Join-Path $newLocal.Settings.controller.dataDirectory 'network.json'
        $sourceHash=(Get-FileHash -LiteralPath $sourceRecord).Hash
        Assert-HVFailure { Initialize-RunnerNetwork $newConfig } 'restore its matching network.json here and rerun'
        Assert-HV ($netCreates -eq 3 -and -not (Test-Path -LiteralPath $targetRecord)) 'Missing ownership record adopted or changed an existing network'
        [IO.File]::Copy($sourceRecord, $targetRecord, $false)
        Initialize-RunnerNetwork $newConfig *> $null
        $restored=Read-RunnerJson $targetRecord
        Assert-HV ($netCreates -eq 3 -and $restored.owner -ceq $first.owner -and $restored.switchId -ceq $first.switchId -and $restored.phase -eq 'ready') 'Restoring ownership metadata did not reuse the original network'
        Assert-HV ((Get-FileHash -LiteralPath $sourceRecord).Hash -eq $sourceHash) 'Recovery changed the original checkout ownership record'
        $restored.switchId=[guid]::NewGuid().ToString(); Write-RunnerJson $targetRecord $restored
        Assert-HVFailure { Initialize-RunnerNetwork $newConfig } 'ownership mismatch'
        Assert-HV ($netCreates -eq 3) 'An unrelated restored switch identity changed the network'

        $script:ipFailure='denied'
        Assert-HVFailure { Initialize-RunnerNetwork $config } 'access denied'
        Assert-HV ($netCreates -eq 3) 'An IPv4 query failure changed the network'
        $script:ipFailure=''
        $script:nats=@([pscustomobject]@{Name='UnrelatedNAT';InternalIPInterfaceAddressPrefix='10.0.0.0/24'})
        Assert-HVFailure { Initialize-RunnerNetwork $config } 'existing NAT'
        Assert-HV ($netCreates -eq 3) 'Foreign NAT was modified'
        $script:nats=@([pscustomobject]@{Name='CLC-Runners';InternalIPInterfaceAddressPrefix='192.168.231.0/24'})
        $script:switches[0].Notes='foreign owner'
        Assert-HVFailure { Initialize-RunnerNetwork $config } 'ownership mismatch'
        Assert-HV ($netCreates -eq 3) 'Foreign switch was modified'
        $script:switches[0].Notes='clc-runners-network/v1|'+$first.owner
        $script:routes=@([pscustomobject]@{DestinationPrefix='192.168.0.0/16';InterfaceIndex=11})
        Assert-HVFailure { Initialize-RunnerNetwork $config } 'overlaps route'
    }

    & {
        $config=Join-Path $scratch 'tools-config.json'
        $local=Initialize-RunnerConfig $config
        $qemu=Join-Path $scratch 'qemu-img.ps1'
        Set-Content -LiteralPath $qemu -Value 'Write-Output "qemu-img version 11.1.0 (test)"; $global:LASTEXITCODE=0'
        $settings=Read-RunnerJson $config; $settings.hyperv.qemuImg=$qemu; Write-RunnerJson $config $settings
        $script:installs=0
        function winget.exe { $script:installs++; throw 'Existing QEMU must not be reinstalled' }
        Install-RunnerImageTools $config
        Install-RunnerImageTools $config
        Assert-HV ($script:installs -eq 0) 'Retry reinstalled an existing QEMU tool'
        Set-Content -LiteralPath $qemu -Value 'Write-Output "qemu-img version 9.0.0 (test)"; $global:LASTEXITCODE=0'
        Assert-HVFailure { Install-RunnerImageTools $config } 'version does not match'
        Assert-HV ($script:installs -eq 0) 'Version mismatch silently upgraded QEMU'
    }

    & {
        $owner=[guid]::NewGuid().ToString();$root=Join-Path $scratch 'vms'
        $name='clc-linux-'+$owner.Substring(0,8)+'-123456abcdef'
        $directory=Join-Path $root $name;New-Item -ItemType Directory -Path $directory -Force | Out-Null
        $base=Join-Path $scratch 'immutable.vhdx';Set-Content -LiteralPath $base -Value 'base';(Get-Item $base).IsReadOnly=$true
        $record=@{owner=$owner;scale='clc-linux';name=$name;nonce=[guid]::NewGuid().ToString();vmId='';vmSid='';directory=$directory;image=$base;created=(Get-Date).ToString('o');runnerId=42;mac='00155DA8E70A';removed=$false}
        Write-RunnerJson (Join-Path $directory 'record.json') $record
        Set-Content -LiteralPath (Join-Path $directory 'seed.iso') -Value 'seed'
        $settings=(Get-Content -Raw (Join-Path $script:RunnerRoot 'config/controller.example.json') | ConvertFrom-Json).hyperv
        $request=@{owner=$owner;scale='clc-linux';root=$root;records=@($record);settings=$settings}
        $script:calls=[Collections.Generic.List[string]]::new();$script:fakeVM=$null;$script:freeMemoryKB=64GB/1KB;$script:freeDisk=500GB;$script:assignedCPU=0;$script:diskType='Differencing'
        function Import-Module { param($Name,$ErrorAction) }
        function Get-VM { if ($script:fakeVM) { $script:fakeVM } }
        function Get-VMProcessor { param($VM) [pscustomobject]@{Count=$script:assignedCPU} }
        function Get-VMMemory { param($VM) [pscustomobject]@{Startup=$script:startupMemory;DynamicMemoryEnabled=$script:dynamicMemory} }
        function Get-CimInstance { param($ClassName)
            if ($ClassName -eq 'Win32_OperatingSystem') { [pscustomobject]@{FreePhysicalMemory=$script:freeMemoryKB} }
            else { [pscustomobject]@{NumberOfLogicalProcessors=16} }
        }
        function Get-Volume { param($FilePath) [pscustomobject]@{SizeRemaining=$script:freeDisk} }
        $script:hypervMemory=64GB
        function Get-CLCHyperVAvailableMemory { $script:hypervMemory }
        function Assert-CLCNetwork { param($Request) [pscustomobject]@{Name='CLC-Runners'} }
        function Get-VMHardDiskDrive { param($VM) [pscustomobject]@{Path=$(if ($VM.Name -eq 'foreign') { Join-Path $scratch 'foreign-child.vhdx' } else { Join-Path $directory 'os.vhdx' })} }
        function Get-VHD { param($Path,$ErrorAction) [pscustomobject]@{VhdFormat='VHDX';VhdType=$script:diskType;Size=20GB;LogicalSectorSize=512;ParentPath=$(if ($Path -eq (Join-Path $scratch 'foreign-child.vhdx')) { Join-Path $directory 'os.vhdx' } else { '' })} }
        function Test-VHD { param($Path,$ErrorAction) $true }
        function Resize-VHD { throw 'CreateBuild must receive an already resized/validated disk' }
        function New-VHD { param($Path,$ParentPath,[switch]$Differencing) Assert-HV ($Differencing -and $ParentPath -eq $base) 'Expected differencing disk';$script:calls.Add('New-VHD');Set-Content -LiteralPath $Path -Value 'child' }
        function New-VM { param($Name,$Generation,$Path,$VHDPath,$MemoryStartupBytes,$SwitchName)
            Assert-HV ($Generation -eq 2 -and $MemoryStartupBytes -eq 10GB -and $Path -eq $directory -and $SwitchName -eq 'CLC-Runners') 'Wrong Generation 2 VM settings'
            $script:startupMemory=$MemoryStartupBytes
            $script:calls.Add('New-VM');$script:fakeVM=[pscustomobject]@{Name=$Name;Id=[guid]::NewGuid();Notes='';ConfigurationLocation=(Join-Path $Path $Name);State='Off';MemoryAssigned=0};$script:fakeVM
        }
        function Set-VM { param($VM,$Notes,$AutomaticStartAction,$AutomaticStopAction,$CheckpointType) $VM.Notes=$Notes;Assert-HV ($AutomaticStopAction -eq 'ShutDown' -and $CheckpointType -eq 'Disabled') 'Unsafe VM restart/checkpoint settings' }
        function Set-VMMemory { param($VM,$DynamicMemoryEnabled) Assert-HV (-not $DynamicMemoryEnabled) 'Dynamic memory enabled unexpectedly';$script:dynamicMemory=$DynamicMemoryEnabled }
        function Set-VMProcessor { param($VM,$Count) Assert-HV ($Count -eq 2) 'CPU setting lost';$script:assignedCPU=$Count }
        function Set-VMNetworkAdapter { param($VM,$StaticMacAddress,$MacAddressSpoofing,$DhcpGuard,$RouterGuard) Assert-HV ($StaticMacAddress -eq $record.mac -and $MacAddressSpoofing -eq 'Off' -and $DhcpGuard -eq 'On') 'Network identity/guard configuration lost' }
        function Set-CLCNetworkPolicy { param($Request,$Record,$VM) Assert-HV ($VM.State -eq 'Off') 'Network policy installed after boot';$script:calls.Add('NetworkPolicy') }
        function Set-VMFirmware { param($VM,$EnableSecureBoot,$SecureBootTemplate,$FirstBootDevice) if ($EnableSecureBoot) { Assert-HV ($SecureBootTemplate -eq 'MicrosoftUEFICertificateAuthority') 'Linux Secure Boot template missing' } }
        function Add-VMDvdDrive { param($VM,$Path) Assert-HV ($Path -eq (Join-Path $directory 'seed.iso')) 'NoCloud seed not attached' }
        function Grant-CLCVMFile { param($Path,$ID) $script:calls.Add('ACL') }
        function Get-CLCVMAccountSID { param($ID) 'S-1-5-83-123-456' }
        function Remove-VM { param($VM,[switch]$Force) $script:calls.Add('Remove-VM');$script:fakeVM=$null }
        Assert-CLCCapacity $request
        $script:freeMemoryKB=18GB/1KB
        $admission=Invoke-CLCHyperV $request Admission
        Assert-HV ($admission.ready -and $admission.decision -eq 'Accepted' -and $admission.requestedMemoryGiB -eq 10 -and $admission.reserveMemoryGiB -eq 8 -and $admission.availableMemoryBytes -eq 18GB) 'Guest plus reserve boundary or admission diagnostics incorrect'
        $script:freeMemoryKB=(18GB-1KB)/1KB
        $admission=Invoke-CLCHyperV $request Admission
        Assert-HV (-not $admission.ready -and $admission.decision -eq 'Deferred' -and $admission.reason -match 'Insufficient free memory') 'Memory shortage did not defer cleanly'
        $script:freeMemoryKB=64GB/1KB;$script:hypervMemory=10GB-1
        Assert-HVFailure { Assert-CLCCapacity $request } 'Insufficient Hyper-V available memory'
        $script:hypervMemory=10GB;Assert-CLCCapacity $request
        & {
            function Get-CLCHyperVAvailableMemory { throw 'Counter unavailable' }
            $admission=Get-CLCCapacity $request
            Assert-HV ($admission.ready -and $null -eq $admission.hypervAvailableMemoryBytes -and $admission.hypervMemoryError -eq 'Counter unavailable') 'Unavailable optional counter was hidden or bypassed Windows admission'
        }
        $script:hypervMemory=64GB
        $script:freeMemoryKB=2GB/1KB;Assert-HVFailure { Assert-CLCCapacity $request } 'Insufficient free memory';$script:freeMemoryKB=64GB/1KB
        $script:freeDisk=5GB;Assert-HVFailure { Assert-CLCCapacity $request } 'Insufficient disk';$script:freeDisk=500GB
        & {
            $request.records=@()
            $script:freeDisk=35GB;Assert-CLCCapacity $request
            $script:freeDisk=35GB-1;Assert-HVFailure { Assert-CLCCapacity $request } '35 GiB required'
            $settings.diskGrowthGiB=10
            $script:freeDisk=25GB;Assert-CLCCapacity $request
            $script:freeDisk=25GB-1;Assert-HVFailure { Assert-CLCCapacity $request } '25 GiB required'
            $settings.diskGrowthGiB=20
            $request.records=@($record)
            $script:freeDisk=55GB;Assert-CLCCapacity $request
            $script:freeDisk=55GB-1;Assert-HVFailure { Assert-CLCCapacity $request } '55 GiB required'
            $disk=Join-Path $directory 'os.vhdx';Set-Content -LiteralPath $disk -Value 'mock disk'
            $script:allocatedDisk=10GB
            function Get-Item { param($LiteralPath) [pscustomobject]@{Length=$script:allocatedDisk} }
            $script:freeDisk=45GB;Assert-CLCCapacity $request
            $script:freeDisk=45GB-1;Assert-HVFailure { Assert-CLCCapacity $request } '45 GiB required'
            $pending=@{directory=(Join-Path $root 'pending');removed=$false}
            $request.records=@($record,$pending)
            $script:freeDisk=65GB;Assert-CLCCapacity $request
            $script:freeDisk=65GB-1;Assert-HVFailure { Assert-CLCCapacity $request } '65 GiB required'
            $pending.removed=$true
            $script:freeDisk=45GB;Assert-CLCCapacity $request
            $script:allocatedDisk=30GB
            $script:freeDisk=35GB;Assert-CLCCapacity $request
            $script:freeDisk=35GB-1;Assert-HVFailure { Assert-CLCCapacity $request } '35 GiB required'
            Remove-Item -LiteralPath $disk
        }
        $request.records=@($record);$script:freeDisk=500GB
        $created=Invoke-CLCHyperV $request Create
        Assert-HV ($created.id -and ($calls -join ',') -eq 'New-VHD,New-VM,NetworkPolicy,ACL,ACL') 'Provisioning/network policy/ACL operations did not complete in order'
        $record.vmId=$created.id;Write-RunnerJson (Join-Path $directory 'record.json') $record
        $script:fakeVM.State='Running';$script:fakeVM.MemoryAssigned=10GB
        $inventory=Invoke-CLCHyperV $request Inventory
        Assert-HV ($inventory.Count -eq 1 -and $inventory[0].cpus -eq 2 -and $inventory[0].memoryStartupBytes -eq 10GB -and $inventory[0].memoryAssignedBytes -eq 10GB -and -not $inventory[0].dynamicMemoryEnabled) 'Native processor/memory inventory did not preserve actual allocations'
        $script:fakeVM.State='Running';$script:assignedCPU=14
        Assert-CLCCapacity $request
        $script:assignedCPU=64
        $capacity=Get-CLCCapacity $request
        Assert-HV ($capacity.ready -and $capacity.assignedCPUs -eq 64 -and $capacity.logicalProcessors -eq 16) 'vCPU overcommit was rejected or disappeared from diagnostics'
        $script:freeMemoryKB=1GB/1KB
        Assert-HVFailure { Assert-CLCCapacity $request } 'Insufficient free memory'
        $script:freeMemoryKB=64GB/1KB;$script:freeDisk=1GB
        Assert-HVFailure { Assert-CLCCapacity $request } 'Insufficient disk capacity'
        $script:freeDisk=500GB
        Assert-HVFailure { Invoke-CLCHyperV $request Remove } 'must be stopped'
        $script:fakeVM.State='Off';$script:fakeVM.Notes='foreign metadata'
        Assert-HVFailure { Invoke-CLCHyperV $request Remove } 'ownership metadata differs'
        Assert-HV (-not $calls.Contains('Remove-VM')) 'Foreign/live VM deleted'
        $script:fakeVM.Notes=Get-CLCMarker $record
        $null=Invoke-CLCHyperV $request Remove
        Assert-HV ($calls.Contains('Remove-VM')) 'Stopped owned VM was not removed'
        $null=Invoke-CLCHyperV $request Remove
        Assert-HVFailure { Invoke-CLCHyperV $request DeleteFiles } 'must be durable'
        $record.removed=$true;Write-RunnerJson (Join-Path $directory 'record.json') $record
        $script:fakeVM=[pscustomobject]@{Id=[guid]::NewGuid();Name='foreign';State='Running'}
        Assert-HVFailure { Invoke-CLCHyperV $request DeleteFiles } 'still references owned disk'
        Assert-HV (Test-Path -LiteralPath $directory) 'Foreign dependent disk lost its parent'
        $script:fakeVM=$null
        $null=Invoke-CLCHyperV $request DeleteFiles
        Assert-HV (-not (Test-Path -LiteralPath $directory)) 'Owned temporary files remained'

        # Build VM creation consumes the validated dynamic container unchanged.
        $null=New-Item -ItemType Directory -Path $directory
        $converted=Join-Path $scratch 'converted.vhdx';Set-Content -LiteralPath $converted -Value 'validated Ubuntu sectors'
        $record.image=$converted;$record.vmId='';$record.removed=$false
        Write-RunnerJson (Join-Path $directory 'record.json') $record
        Set-Content -LiteralPath (Join-Path $directory 'seed.iso') -Value 'seed'
        $script:calls.Clear();$script:diskType='Dynamic'
        $created=Invoke-CLCHyperV $request CreateBuild
        Assert-HV (($calls -join ',') -eq 'New-VM,NetworkPolicy,ACL' -and $created.id) 'Build VM did not consume the precreated disk and install network policy'
        Assert-HV (-not (Test-Path -LiteralPath $converted) -and (Get-Content -LiteralPath (Join-Path $directory 'os.vhdx') -Raw).Trim() -eq 'validated Ubuntu sectors') 'Build VM disk move lost source contents'
        $record.vmId=$created.id;Write-RunnerJson (Join-Path $directory 'record.json') $record
        $null=Invoke-CLCHyperV $request Remove
        $record.removed=$true;Write-RunnerJson (Join-Path $directory 'record.json') $record
        $null=Invoke-CLCHyperV $request DeleteFiles
    }
    Write-Host "Hyper-V helpers: $hvAssertions mocked provisioning, capacity, ownership and network assertions passed."
} finally {
    $absolute=[IO.Path]::GetFullPath($scratch)
    $prefix=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')+'\'
    if ($absolute.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf).StartsWith('clc-hyperv-tests-')) { Remove-Item -LiteralPath $absolute -Recurse -Force }
}
