#requires -Version 7.4
[CmdletBinding()]
param([ValidateSet('Check','Capacity','Admission','Inventory','Create','CreateBuild','Start','Stop','Remove','DetachSeed','DeleteFiles','ConvertImage','InspectDisk','ImageReferences')][string]$Operation)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-CLCPath([string]$Path, [string]$Root) {
    $full = [IO.Path]::GetFullPath($Path)
    $prefix = [IO.Path]::GetFullPath($Root).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
    if (-not $full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { throw "Owned path escapes resource root: $full" }
    $cursor = $full
    while ($cursor -and $cursor.Length -ge $prefix.TrimEnd('\').Length) {
        if (Test-Path -LiteralPath $cursor) {
            if ((Get-Item -LiteralPath $cursor -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Reparse points are forbidden in owned VM/image paths.' }
        }
        $parent = Split-Path $cursor -Parent
        if ($parent -eq $cursor) { break }
        $cursor = $parent
    }
    $full
}

function Get-CLCMarker($Record) { "clc-runners/v1|$($Record.owner)|$($Record.scale)|$($Record.name)|$($Record.nonce)" }

function Assert-CLCRecord($Request, $Record) {
    $directory = Assert-CLCPath $Record.directory $Request.root
    if ($Record.owner -cne $Request.owner -or $Record.scale -cne $Request.scale -or
        $Record.name -notmatch ('^' + [regex]::Escape($Request.scale + '-' + $Request.owner.Substring(0,8) + '-') + '[a-f0-9]{12}$') -or
        (Split-Path $directory -Leaf) -cne $Record.name) { throw 'Resource ownership record mismatch.' }
    $stored = Get-Content -LiteralPath (Join-Path $directory 'record.json') -Raw | ConvertFrom-Json
    if ($stored.owner -cne $Record.owner -or $stored.scale -cne $Record.scale -or $stored.name -cne $Record.name -or
        $stored.nonce -cne $Record.nonce -or $stored.directory -cne $directory -or $stored.vmId -cne $Record.vmId -or
        $stored.vmSid -cne $Record.vmSid -or [bool]$stored.removed -ne [bool]$Record.removed) { throw 'Durable ownership record mismatch.' }
}

function Get-CLCVM($Record) {
    $vm = if ($Record.vmId) { Get-VM | Where-Object { [string]$_.Id -eq $Record.vmId } } else { Get-VM | Where-Object Name -CEQ $Record.name }
    if (@($vm).Count -gt 1) { throw 'Ambiguous VM identity.' }
    if (-not $vm) { return $null }
    $expectedDisk = Join-Path $Record.directory 'os.vhdx'
    $disks = @(Get-VMHardDiskDrive -VM $vm)
    $configuration=[IO.Path]::GetFullPath($vm.ConfigurationLocation).TrimEnd('\')
    $owned=[IO.Path]::GetFullPath($Record.directory).TrimEnd('\')
    if ($vm.Name -cne $Record.name -or $disks.Count -ne 1 -or $disks[0].Path -ine $expectedDisk -or
        ($configuration -ine $owned -and -not $configuration.StartsWith($owned+'\',[StringComparison]::OrdinalIgnoreCase))) { throw 'VM identity, configuration path or disk differs from durable intent; refusing modification.' }
    $marker = Get-CLCMarker $Record
    if ($vm.Notes -cne $marker) {
        # Recover the narrow New-VM/Set-VM crash window using the already owned
        # configuration AND exact disk path. A matching name alone never suffices.
        if ($Record.vmId -or $vm.Notes) { throw 'VM ownership metadata differs; refusing modification.' }
        Set-VM -VM $vm -Notes $marker
    }
    $vm
}

function Assert-CLCNetwork($Request) {
    $path = Join-Path (Split-Path $Request.root -Parent) 'network.json'
    if (-not (Test-Path -LiteralPath $path)) { throw 'Dedicated network missing. Run runners.ps1 PrepareHost in elevated PowerShell 7.' }
    $network = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
    $switch = @(Get-VMSwitch | Where-Object { [string]$_.Id -eq $network.switchId })
    if ($network.phase -ne 'ready' -or $network.subnet -cne $Request.settings.subnet -or $network.gateway -cne $Request.settings.gateway -or
        $network.switchName -cne $Request.settings.switchName -or $network.natName -cne $Request.settings.natName -or
        $switch.Count -ne 1 -or $switch[0].SwitchType -ne 'Internal' -or
        $switch[0].Notes -cne ('clc-runners-network/v1|' + $network.owner)) { throw 'Dedicated network configuration or ownership changed; run PrepareHost to diagnose.' }
    $switch[0]
}

function Get-CLCNetworkPolicy($Request, $Record) {
    # Higher weights win. Outbound private-address denies match the destination,
    # not the guest's source address or next-hop MAC, preserving WinNAT egress.
    foreach ($direction in @('Inbound','Outbound')) {
        @{ Direction=$direction; Action='Deny'; Weight=1 }
        @{ Direction=$direction; Action='Allow'; Weight=10; LocalIPAddress=($Record.ip+'/32'); RemoteIPAddress='0.0.0.0/0' }
        @{ Direction=$direction; Action='Deny'; Weight=200; RemoteIPAddress='::/0' }
        $weight=100
        foreach ($prefix in @('0.0.0.0/8','10.0.0.0/8','100.64.0.0/10','127.0.0.0/8','169.254.0.0/16','172.16.0.0/12','192.168.0.0/16','224.0.0.0/4','240.0.0.0/4')) {
            @{ Direction=$direction; Action='Deny'; Weight=$weight; RemoteIPAddress=$prefix }
            $weight++
        }
    }
    # A stateful inbound rule permits only this flow's replies through the
    # outbound private-address denies. A stateless source-port-22 exception
    # would let a privileged job open arbitrary connections to the host.
    @{ Direction='Inbound'; Action='Allow'; Weight=300; LocalIPAddress=($Record.ip+'/32'); RemoteIPAddress=($Request.settings.gateway+'/32'); LocalPort='22'; Protocol='TCP'; Stateful=$true; IdleSessionTimeout=3600 }
}

function Get-CLCNetworkAdapter($Request, $Record, $VM) {
    $switch=Assert-CLCNetwork $Request
    $adapters=@(Get-VMNetworkAdapter -VM $VM -ErrorAction Stop)
    if ($adapters.Count -ne 1 -or [string]$adapters[0].SwitchId -ne [string]$switch.Id -or
        $adapters[0].MacAddress -ine $Record.mac -or $adapters[0].DynamicMacAddressEnabled -or
        [string]$adapters[0].MacAddressSpoofing -ne 'Off' -or
        [string]$adapters[0].DhcpGuard -ne 'On' -or [string]$adapters[0].RouterGuard -ne 'On') {
        throw 'Owned VM network adapter, switch, address or guards differ; refusing to boot.'
    }
    $adapters[0]
}

function ConvertTo-CLCACLAddress([string]$Value) {
    if (-not $Value -or $Value -in @('ANY','*')) { return 'ANY' }
    $parts=$Value.Split('/')
    $address=[Net.IPAddress]::Parse($parts[0])
    $bits=if ($address.AddressFamily -eq [Net.Sockets.AddressFamily]::InterNetwork) { 32 } else { 128 }
    # Hyper-V may return a single address with or without its host prefix.
    $prefix=if ($parts.Count -eq 2) { [int]$parts[1] } else { $bits }
    $address.ToString()+'/'+$prefix
}

function Get-CLCACLSignature($Rule) {
    $fields=[ordered]@{}
    foreach ($name in @('Direction','Action','Weight','LocalIPAddress','RemoteIPAddress','LocalPort','RemotePort','Protocol','Stateful','IdleSessionTimeout','IsolationID')) {
        $value=if ($Rule -is [Collections.IDictionary]) { $Rule[$name] } else { $Rule.$name }
        switch ($name) {
            { $_ -in @('LocalIPAddress','RemoteIPAddress') } { $value=ConvertTo-CLCACLAddress ([string]$value) }
            { $_ -in @('LocalPort','RemotePort','Protocol') } {
                $value=([string]$value).ToUpperInvariant()
                if (-not $value -or $value -eq '*') { $value='ANY' }
                if ($name -eq 'Protocol') {
                    $value=switch ($value) { '6' { 'TCP' }; '17' { 'UDP' }; '256' { 'ANY' }; default { $value } }
                }
            }
            'Stateful' { $value=[bool]$value }
            'IdleSessionTimeout' { $value=if ($fields['Stateful']) { [int]$value } else { 0 } }
            { $_ -in @('Weight','IsolationID') } { $value=[int]$value }
            default { $value=([string]$value).ToUpperInvariant() }
        }
        $fields[$name]=$value
    }
    $fields | ConvertTo-Json -Compress
}

function Assert-CLCNetworkPolicy($Request, $Record, $VM) {
    $adapter=Get-CLCNetworkAdapter $Request $Record $VM
    $expected=@(Get-CLCNetworkPolicy $Request $Record | ForEach-Object { Get-CLCACLSignature $_ } | Sort-Object)
    $actual=@(Get-VMNetworkAdapterExtendedAcl -VMNetworkAdapter $adapter -ErrorAction Stop | ForEach-Object { Get-CLCACLSignature $_ } | Sort-Object)
    if (($expected -join "`n") -cne ($actual -join "`n")) {
        throw 'Owned VM network restrictions are missing or changed; refusing to boot. Recreate the stopped VM through the controller to install the current policy.'
    }
}

function Set-CLCNetworkPolicy($Request, $Record, $VM) {
    if ($VM.State -ne 'Off') { throw 'Network restrictions must be installed while the owned VM is off.' }
    $adapter=Get-CLCNetworkAdapter $Request $Record $VM
    if (@(Get-VMNetworkAdapterExtendedAcl -VMNetworkAdapter $adapter -ErrorAction Stop).Count) {
        throw 'New owned VM already has network ACLs; refusing to replace an unexpected policy.'
    }
    foreach ($rule in @(Get-CLCNetworkPolicy $Request $Record)) {
        Add-VMNetworkAdapterExtendedAcl -VMNetworkAdapter $adapter @rule -ErrorAction Stop
    }
    Assert-CLCNetworkPolicy $Request $Record $VM
}

function Get-CLCHyperVAvailableMemory {
    # This counter measures Hyper-V's allocatable memory, which can differ from
    # Windows available RAM. Counter access/localization failures are observable;
    # Windows admission and Hyper-V's native Start-VM fence still apply.
    $samples = @((Get-Counter '\Hyper-V Dynamic Memory Balancer(*)\Available Memory' -ErrorAction Stop).CounterSamples | Where-Object { $_.Status -eq 0 -and $_.CookedValue -ge 0 })
    if ($samples.Count -ne 1) { throw 'Hyper-V available-memory counter is unavailable or ambiguous.' }
    [long][Math]::Floor($samples[0].CookedValue * 1MB)
}

function Get-CLCCapacity($Request) {
    $settings = $Request.settings
    $os = Get-CimInstance Win32_OperatingSystem
    $computer = Get-CimInstance Win32_ComputerSystem
    $active = @(Get-VM | Where-Object State -NE 'Off')
    $usedCPU = 0
    foreach ($vm in $active) { $usedCPU += (Get-VMProcessor -VM $vm).Count }
    $availableMemory = [long]$os.FreePhysicalMemory * 1KB
    $hypervMemory = $null; $hypervMemoryError = ''
    try { $hypervMemory = Get-CLCHyperVAvailableMemory } catch { $hypervMemoryError = $_.Exception.Message }
    $directory = Split-Path $Request.root -Parent
    $volume = Get-Volume -FilePath ($directory.TrimEnd('\') + '\')
    $growthBudget = [long]$settings.diskGrowthGiB * 1GB
    $committed = [long]0
    foreach ($record in $Request.records) {
        if (-not $record.removed) {
            $disk = Join-Path $record.directory 'os.vhdx'
            $actual = if (Test-Path -LiteralPath $disk) { (Get-Item -LiteralPath $disk).Length } else { 0 }
            $committed += [Math]::Max([long]0, $growthBudget - [long]$actual)
        }
    }
    $required = $committed + $growthBudget + [long]$settings.reserveDiskGiB * 1GB
    $reason = ''
    # Assigned vCPUs are diagnostic data, not a reservation of physical CPUs.
    # Hyper-V schedules overcommitted vCPUs; admission still protects RAM/disk.
    if ($availableMemory -lt ([long]$settings.memoryGiB + $settings.reserveMemoryGiB) * 1GB) { $reason = "Insufficient free memory: $([Math]::Round($availableMemory/1GB,2)) GiB available; $($settings.memoryGiB) GiB guest plus $($settings.reserveMemoryGiB) GiB Windows reserve required." }
    elseif ($null -ne $hypervMemory -and $hypervMemory -lt [long]$settings.memoryGiB * 1GB) { $reason = "Insufficient Hyper-V available memory: $([Math]::Round($hypervMemory/1GB,2)) GiB available; $($settings.memoryGiB) GiB guest required. Hyper-V's own host reserve is already excluded." }
    elseif ($volume.SizeRemaining -lt $required) { $reason = "Insufficient disk capacity: $([Math]::Round($volume.SizeRemaining/1GB,1)) GiB free; $([Math]::Ceiling($required/1GB)) GiB required including per-VM disk-growth budgets and Windows reserve. Free space or relocate dataDirectory; no files were pruned." }
    @{
        ready=($reason -eq ''); decision=$(if ($reason) { 'Deferred' } else { 'Accepted' }); reason=$reason
        checkedAt=[DateTimeOffset]::UtcNow.ToString('o'); requestedCPUs=[int]$settings.cpus
        assignedCPUs=$usedCPU; logicalProcessors=[int]$computer.NumberOfLogicalProcessors
        requestedMemoryGiB=[int]$settings.memoryGiB; reserveMemoryGiB=[int]$settings.reserveMemoryGiB
        availableMemoryBytes=$availableMemory; hypervAvailableMemoryBytes=$hypervMemory; hypervMemoryError=$hypervMemoryError
        availableDiskBytes=[long]$volume.SizeRemaining; requiredDiskBytes=[long]$required
    }
}

function Assert-CLCCapacity($Request) {
    $admission = Get-CLCCapacity $Request
    if (-not $admission.ready) { throw $admission.reason }
}

function Grant-CLCVMFile([string]$Path, [string]$ID) {
    $file = [IO.FileInfo]::new($Path)
    $acl = [IO.FileSystemAclExtensions]::GetAccessControl($file)
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new("NT VIRTUAL MACHINE\$ID", 'Read', 'Allow'))
    [IO.FileSystemAclExtensions]::SetAccessControl($file,$acl)
}

function Revoke-CLCVMFile([string]$Path, [string]$SID) {
    if (-not $SID -or -not (Test-Path -LiteralPath $Path)) { return }
    $file=[IO.FileInfo]::new($Path)
    $acl=[IO.FileSystemAclExtensions]::GetAccessControl($file)
    foreach ($rule in @($acl.GetAccessRules($true,$false,[Security.Principal.SecurityIdentifier]))) {
        if ($rule.IdentityReference.Value -eq $SID) { $acl.RemoveAccessRuleSpecific($rule) }
    }
    [IO.FileSystemAclExtensions]::SetAccessControl($file,$acl)
}

function Get-CLCVMAccountSID([string]$ID) { ([Security.Principal.NTAccount]::new("NT VIRTUAL MACHINE\$ID")).Translate([Security.Principal.SecurityIdentifier]).Value }

function Assert-CLCFileAttributes([string]$Path) {
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    $forbidden = @('SparseFile','Compressed','Encrypted') | Where-Object { $item.Attributes -band [IO.FileAttributes]::$_ }
    if ($forbidden) {
        throw "Hyper-V cannot use '$Path': forbidden NTFS attributes $($forbidden -join ', '). VHDX files must be non-sparse, uncompressed and unencrypted. Use uncompressed/unencrypted imageDirectory and dataDirectory locations, then rerun PrepareImage -ForceRebuild. Never change an active base image."
    }
}

function Assert-CLCVHD([string]$Path, [long]$Size, [string]$Type = 'Dynamic') {
    Assert-CLCFileAttributes $Path
    $vhd = Get-VHD -Path $Path -ErrorAction Stop
    if ($vhd.VhdFormat -ne 'VHDX' -or $vhd.VhdType -ne $Type -or $vhd.Size -ne $Size -or $vhd.LogicalSectorSize -ne 512) {
        throw "Unexpected VHDX format/type/logical size/sector size for '$Path'; expected $Type VHDX, $Size bytes, 512-byte sectors. Rebuild with PrepareImage; candidate was not validated."
    }
    if (-not (Test-VHD -Path $Path -ErrorAction Stop)) { throw "Hyper-V Test-VHD rejected '$Path'; candidate was not validated. Rebuild with PrepareImage." }
}

function Invoke-CLCQemuImg([string]$Program, [string[]]$Arguments) {
    $output = & $Program @Arguments 2>&1
    if ($LASTEXITCODE) { throw "qemu-img $($Arguments[0]) failed (exit $LASTEXITCODE): $($output -join "`n")" }
    $output
}

function Convert-CLCImage($Request) {
    $owner = [guid]::Empty
    $version = [string]$Request.disk['version']
    if (-not [guid]::TryParse($Request.owner, [ref]$owner) -or $owner -eq [guid]::Empty -or
        $version -cnotmatch '^ubuntu2404-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{12}$') {
        throw 'Image conversion requires a matching owned build intent and converted.vhdx path (invalid owner or build version).'
    }
    $path = Assert-CLCPath $Request.disk.path $Request.settings.imageDirectory
    $source = Assert-CLCPath $Request.disk.source $Request.settings.imageDirectory
    $directory = Assert-CLCPath (Join-Path $Request.settings.imageDirectory $version) $Request.settings.imageDirectory
    $expected = [IO.Path]::GetFullPath((Join-Path $directory 'converted.vhdx'))
    # Windows paths are case-insensitive; GetFullPath also normalizes separators
    # and dot segments. The caller's build version fixes the one permitted file.
    if (-not $path.Equals($expected, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Image conversion requires a matching owned build intent and converted.vhdx path (destination differs from requested build).'
    }
    $intentPath = Assert-CLCPath (Join-Path $directory 'build-intent.json') $Request.settings.imageDirectory
    try {
        if (-not (Test-Path -LiteralPath $intentPath -PathType Leaf)) { throw 'Build intent missing.' }
        $intent = Get-Content -LiteralPath $intentPath -Raw | ConvertFrom-Json -AsHashtable
    } catch { throw 'Image conversion requires a matching owned build intent and converted.vhdx path (missing or invalid build intent).' }
    if ($intent -isnot [Collections.IDictionary] -or
        $intent['owner'] -cne $Request.owner -or $intent['version'] -cne $version) { throw 'Image conversion requires a matching owned build intent and converted.vhdx path.' }
    if (Test-Path -LiteralPath $path) { throw "Image conversion target already exists: '$path'. Preserve it for diagnostics; rerun PrepareImage to create a fresh candidate." }
    Assert-CLCFileAttributes $directory
    $qemu = $Request.settings.qemuImg
    $info = (Invoke-CLCQemuImg $qemu @('info','--output=json','-f','qcow2',$source)) -join "`n" | ConvertFrom-Json
    $sourceSize = [long]$info.'virtual-size'
    $targetSize = [long]$Request.settings.diskGiB * 1GB
    if ($info.format -ne 'qcow2' -or $sourceSize -le 0 -or $sourceSize % 512 -ne 0 -or $sourceSize -gt $targetSize) {
        throw "Ubuntu QCOW2 logical size $sourceSize is invalid or exceeds configured disk size $targetSize; choose a diskGiB large enough for the source. No VHDX was created."
    }
    # QEMU's Windows file creation sets FSCTL_SET_SPARSE even with -S 0.
    # -n opens the Hyper-V container without recreating it. Create at the source
    # size first so conversion cannot populate the larger target with zero blocks.
    $null = New-VHD -Path $path -Dynamic -SizeBytes $sourceSize -BlockSizeBytes 1MB -LogicalSectorSizeBytes 512 -PhysicalSectorSizeBytes 4096 -ErrorAction Stop
    Assert-CLCVHD $path $sourceSize
    $null = Invoke-CLCQemuImg $qemu @('convert','-n','-f','qcow2','-O','vhdx',$source,$path)
    Assert-CLCVHD $path $sourceSize
    # Compare guest-visible sectors, not container bytes or allocation maps.
    $null = Invoke-CLCQemuImg $qemu @('compare','-f','qcow2','-F','vhdx',$source,$path)
    if ($sourceSize -lt $targetSize) { Resize-VHD -Path $path -SizeBytes $targetSize -ErrorAction Stop }
    Assert-CLCVHD $path $targetSize
    @{ size=$targetSize; sourceSize=$sourceSize; validated=$true }
}

function Invoke-CLCHyperV($Request, [string]$Action) {
    Import-Module Hyper-V -ErrorAction Stop
    foreach ($record in $Request.records) { Assert-CLCRecord $Request $record }
    switch ($Action) {
        'ConvertImage' { return Convert-CLCImage $Request }
        'InspectDisk' {
            $path = Assert-CLCPath $Request.disk.path $Request.settings.imageDirectory
            Assert-CLCVHD $path ([long]$Request.settings.diskGiB * 1GB)
            return @{ validated=$true }
        }
        'Check' {
            $null = Get-VM -ErrorAction Stop
            $null = Assert-CLCNetwork $Request
            return @{ ready=$true }
        }
        'Capacity' { Assert-CLCCapacity $Request; return @{ ready=$true } }
        'Admission' { return Get-CLCCapacity $Request }
        'Inventory' {
            $result = @()
            foreach ($record in $Request.records) {
                $vm = Get-CLCVM $record
                $item = @{ id=$(if ($vm) { [string]$vm.Id } else { '' }); name=$record.name; runnerId=$record.runnerId; created=$record.created; running=[bool]($vm -and $vm.State -ne 'Off'); status=$(if ($vm) { [string]$vm.State } else { 'Missing' }) }
                if ($vm) {
                    $memory = Get-VMMemory -VM $vm
                    $item.cpus = [int](Get-VMProcessor -VM $vm).Count
                    $item.memoryStartupBytes = [long]$memory.Startup
                    $item.memoryAssignedBytes = [long]$vm.MemoryAssigned
                    $item.dynamicMemoryEnabled = [bool]$memory.DynamicMemoryEnabled
                }
                $result += $item
            }
            # A missing manifest is never permission to delete an orphan VM.
            foreach ($vm in @(Get-VM)) {
                if ($vm.Notes -clike "clc-runners/v1|$($Request.owner)|$($Request.scale)|*") {
                    if (-not @($Request.records | Where-Object { (Get-CLCMarker $_) -ceq $vm.Notes }).Count) { throw "Owned VM $($vm.Id) has no durable manifest. Restore the matching record before reconciliation; VM preserved." }
                }
            }
            return ,$result
        }
        'ImageReferences' {
            $paths = @()
            foreach ($vm in @(Get-VM)) {
                foreach ($disk in @(Get-VMHardDiskDrive -VM $vm)) {
                    $path=$disk.Path
                    while ($path) { $paths += $path; $path=(Get-VHD -Path $path -ErrorAction Stop).ParentPath }
                }
            }
            return ,$paths
        }
    }
    if (@($Request.records).Count -ne 1) { throw 'One owned resource is required.' }
    $record = $Request.records[0]
    $disk = Join-Path $record.directory 'os.vhdx'
    $seed = Join-Path $record.directory 'seed.iso'
    if ($Action -in @('Create','CreateBuild')) {
        $switch = Assert-CLCNetwork $Request
        if (Get-CLCVM $record) { throw 'VM already exists; reconcile its durable record instead of recreating it.' }
        if (Test-Path -LiteralPath $disk) { throw 'Owned disk already exists after an interrupted create; cleanup and retry.' }
        Assert-CLCFileAttributes $record.directory
        Assert-CLCFileAttributes $record.image
        if ($Action -eq 'Create') {
            if (-not (Get-Item -LiteralPath $record.image).IsReadOnly) { throw 'Base image is not immutable.' }
            $null = New-VHD -Path $disk -ParentPath $record.image -Differencing
        } else {
            Assert-CLCVHD $record.image ([long]$Request.settings.diskGiB * 1GB)
            Move-Item -LiteralPath $record.image -Destination $disk
        }
        Assert-CLCVHD $disk ([long]$Request.settings.diskGiB * 1GB) $(if ($Action -eq 'Create') { 'Differencing' } else { 'Dynamic' })
        $vm = New-VM -Name $record.name -Generation 2 -Path $record.directory -VHDPath $disk -MemoryStartupBytes ([long]$Request.settings.memoryGiB * 1GB) -SwitchName $switch.Name
        Set-VM -VM $vm -Notes (Get-CLCMarker $record) -AutomaticStartAction Nothing -AutomaticStopAction ShutDown -CheckpointType Disabled
        Set-VMMemory -VM $vm -DynamicMemoryEnabled $false
        Set-VMProcessor -VM $vm -Count $Request.settings.cpus
        Set-VMNetworkAdapter -VM $vm -StaticMacAddress $record.mac -MacAddressSpoofing Off -DhcpGuard On -RouterGuard On
        Set-CLCNetworkPolicy $Request $record $vm
        if ($Request.settings.secureBoot) { Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate MicrosoftUEFICertificateAuthority }
        else { Set-VMFirmware -VM $vm -EnableSecureBoot Off }
        Add-VMDvdDrive -VM $vm -Path $seed
        Set-VMFirmware -VM $vm -FirstBootDevice (Get-VMHardDiskDrive -VM $vm)
        Grant-CLCVMFile $seed ([string]$vm.Id)
        if ($Action -eq 'Create') { Grant-CLCVMFile $record.image ([string]$vm.Id) }
        return @{ id=[string]$vm.Id; sid=(Get-CLCVMAccountSID ([string]$vm.Id)) }
    }
    $vm = Get-CLCVM $record
    switch ($Action) {
        'Start' {
            if (-not $vm) { throw 'Owned VM missing.' }
            if ($vm.State -eq 'Off') {
                Assert-CLCNetworkPolicy $Request $record $vm
                Start-VM -VM $vm
            }
            return @{}
        }
        'Stop' { if ($vm -and $vm.State -ne 'Off') { Stop-VM -VM $vm -TurnOff -Force }; return @{} }
        'DetachSeed' { if ($vm) { Get-VMDvdDrive -VM $vm | Where-Object Path -EQ $seed | Set-VMDvdDrive -Path $null }; return @{} }
        'Remove' {
            $sid=$record.vmSid
            if (-not $sid -and $vm) { $sid=Get-CLCVMAccountSID ([string]$vm.Id) }
            if ($vm) {
                if ($vm.State -ne 'Off') { throw 'VM must be stopped before removal.' }
                Revoke-CLCVMFile $record.image $sid
                Remove-VM -VM $vm -Force
            } else {
                Revoke-CLCVMFile $record.image $sid
            }
            return @{}
        }
        'DeleteFiles' {
            if ($vm -or -not $record.removed) { throw 'VM removal must be durable before file deletion.' }
            foreach ($other in @(Get-VM)) {
                foreach ($drive in @(Get-VMHardDiskDrive -VM $other)) {
                    $path=$drive.Path
                    while ($path) {
                        if ($path.StartsWith($record.directory.TrimEnd('\')+'\',[StringComparison]::OrdinalIgnoreCase)) { throw 'A VM still references owned disk files; refusing deletion.' }
                        $path=(Get-VHD -Path $path -ErrorAction Stop).ParentPath
                    }
                }
            }
            foreach ($entry in @(Get-ChildItem -LiteralPath $record.directory -Force -Recurse)) {
                $null = Assert-CLCPath $entry.FullName $Request.root
            }
            $null = Assert-CLCPath $record.directory $Request.root
            # Preserve the durable record across a partially failed deletion.
            # Remove it only after every disk, seed, key and config file is gone.
            foreach ($entry in @(Get-ChildItem -LiteralPath $record.directory -Force)) {
                if ($entry.Name -eq 'record.json') { continue }
                Remove-Item -LiteralPath $entry.FullName -Recurse -Force
            }
            Remove-Item -LiteralPath (Join-Path $record.directory 'record.json') -Force
            Remove-Item -LiteralPath $record.directory -Force
            return @{}
        }
        default { throw "Unsupported Hyper-V operation: $Action" }
    }
}

if ($Operation) {
    try {
        $request = [Console]::In.ReadToEnd() | ConvertFrom-Json -AsHashtable
        $result = Invoke-CLCHyperV $request $Operation
        ConvertTo-Json -InputObject $result -Depth 12 -Compress
    } catch {
        # Input may contain private paths, but never credentials. Avoid dumping
        # the request/command specification or PowerShell's invocation details.
        [Console]::Error.WriteLine($_.Exception.Message)
        exit 1
    }
}
