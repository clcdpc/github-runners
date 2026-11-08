#requires -Version 7.4
[CmdletBinding()]
param(
    [ValidateSet('Check','Prepare','Run','ValidateSql','Cleanup','Report')][string]$Action = 'Check',
    [string]$Config = 'local/controller.json',
    [string]$AsapRepository = '../asap-pocketbase',
    [ValidatePattern('^[a-f0-9]{40}$')][string]$Commit = '31bbd5e42cd272731790b5faba63309225bb722a',
    [string]$ResourceDirectory = '',
    [string]$OutputDirectory = '',
    [string]$BenchmarkAddress = '192.168.231.254',
    [ValidateRange(3,9)][int]$Repetitions = 3,
    [ValidateRange(15,100)][int]$BootGrowthBudgetGiB = 20,
    [ValidateRange(2,40)][int]$SqlChildGrowthBudgetGiB = 5
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
. (Join-Path $repo 'scripts/Common.ps1')
. (Join-Path $repo 'scripts/HyperV.ps1')
. (Join-Path $PSScriptRoot 'Helpers.ps1')
# Read settings only. Never Initialize-RunnerConfig, Activate, or controller operations.
$resolved = Get-RunnerSettings ([IO.Path]::GetFullPath($Config, $repo))
$settings = $resolved.Settings
$configHash = (Get-FileHash -LiteralPath $resolved.Path).Hash
$script:counterPaths = @()
$AsapRepository = [IO.Path]::GetFullPath($AsapRepository, $repo)
if (-not $ResourceDirectory) { $ResourceDirectory = Join-Path (Split-Path $settings.controller.dataDirectory -Parent) 'benchmarks/hyperv-disk' }
$ResourceDirectory = [IO.Path]::GetFullPath($ResourceDirectory, $repo)
$resourceParent = Split-Path $ResourceDirectory -Parent
if (-not $OutputDirectory) { $OutputDirectory = Join-Path $repo ('artifacts/hyperv-disk/' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N').Substring(0,8)) }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory, $repo)
if ($OutputDirectory.StartsWith($ResourceDirectory.TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase) -or $OutputDirectory -ieq $ResourceDirectory) {
    throw 'Results must live outside the disposable resource directory.'
}

function Invoke-BenchNative([string]$Program, [string[]]$Arguments) {
    $output = & $Program @Arguments 2>&1
    if ($LASTEXITCODE) { throw "$Program failed (exit $LASTEXITCODE): $($output -join "`n")" }
    $output
}

function Assert-BenchIdle {
    $journal = Get-Content -LiteralPath (Join-Path $settings.controller.dataDirectory 'state.json') -Raw | ConvertFrom-Json
    if (@($journal.runners.PSObject.Properties).Count) { throw 'Controller journal contains real runner allocations; retry while idle. Controller left running.' }
    $active = @(Get-VM -ErrorAction Stop | Where-Object { $_.State -ne 'Off' -and $_.Notes -clike 'clc-runners/v1|*' })
    if ($active.Count) { throw 'A real CLC runner VM is active; benchmark aborted. Controller and runner left running.' }
}

function Get-BenchPreflight {
    $computer = Get-CimInstance Win32_ComputerSystem
    $os = Get-CimInstance Win32_OperatingSystem
    $imageCurrent = Get-Content -LiteralPath (Join-Path $settings.hyperv.imageDirectory 'current.json') -Raw | ConvertFrom-Json
    if ($imageCurrent.version -cnotmatch '^ubuntu2404-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{12}$') { throw 'Invalid current image pointer.' }
    $image = Get-Content -LiteralPath (Join-Path $settings.hyperv.imageDirectory ($imageCurrent.version + '/image.json')) -Raw | ConvertFrom-Json
    $expectedImage = Join-Path $settings.hyperv.imageDirectory ($imageCurrent.version + '/base.vhdx')
    if ($image.path -ine $expectedImage -or $image.diskGiB -ne 100 -or -not $image.validation.cloneBoot -or -not $image.validation.dockerSmoke -or
        -not (Get-Item -LiteralPath $image.path).IsReadOnly) { throw 'A validated immutable 100-GiB Ubuntu runner image is required; this harness will not rebuild it.' }
    $volume = Get-Volume -FilePath ($settings.controller.dataDirectory.TrimEnd('\') + '\')
    $existingAncestor = $resourceParent
    while (-not (Test-Path -LiteralPath $existingAncestor)) { $existingAncestor = Split-Path $existingAncestor -Parent }
    if ((Get-Volume -FilePath ($existingAncestor.TrimEnd('\') + '\')).UniqueId -ne $volume.UniqueId) {
        throw 'Benchmark resources must use the existing runner storage volume.'
    }
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    $info = [ordered]@{
        utc = [DateTime]::UtcNow.ToString('o'); hostName = $env:COMPUTERNAME; powerShell = [string]$PSVersionTable.PSVersion
        user = $identity.Name; elevated = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
        hyperVGroupEnabled = $principal.IsInRole('Hyper-V Administrators')
        logicalProcessors = $computer.NumberOfLogicalProcessors; totalMemoryBytes = $computer.TotalPhysicalMemory
        freeMemoryBytes = [long]$os.FreePhysicalMemory * 1KB; storageFreeBytes = $volume.SizeRemaining
        storageVolume = [string]$volume.UniqueId; sourceCommit = $Commit; image = $image
        requested = @{ cpus=4; memoryGiB=16; sqlDiskGiB=40; bootGrowthBudgetGiB=$BootGrowthBudgetGiB; sqlChildGrowthBudgetGiB=$SqlChildGrowthBudgetGiB; storageReserveGiB=15 }
        hyperVAccessible = $false; blockers = @(); hostVirtualDiskLatency = 'Unavailable: no matched Hyper-V latency counter observed yet.'
    }
    $null = Invoke-BenchNative git @('-C', $AsapRepository, 'cat-file', '-e', ($Commit + '^{commit}'))
    $info.sourceTree = [string](Invoke-BenchNative git @('-C', $AsapRepository, 'rev-parse', ($Commit + '^{tree}')))
    # Reserve 80 GiB for fixed template+working copy, a private boot-parent copy,
    # bounded boot-overlay growth, and 15 GiB free for Windows/production work.
    $info.requiredFreeBytes = (80 + $BootGrowthBudgetGiB + $SqlChildGrowthBudgetGiB + 15) * 1GB + (Get-Item -LiteralPath $image.path).Length
    if ($info.freeMemoryBytes -lt 24GB) { $info.blockers += 'Need 16 GiB guest RAM plus 8 GiB Windows reserve.' }
    if ($info.storageFreeBytes -lt $info.requiredFreeBytes) { $info.blockers += 'Insufficient storage for fixed disks, bounded boot growth and Windows reserve.' }
    try {
        Import-Module Hyper-V -ErrorAction Stop
        $all = @(Get-VM -ErrorAction Stop)
        $info.hyperVAccessible = $true
        $info.vmInventory = @($all | Select-Object Name, @{n='id';e={[string]$_.Id}}, @{n='state';e={[string]$_.State}}, CPUUsage, MemoryAssigned)
        $assigned = 0
        foreach ($vm in @($all | Where-Object State -NE 'Off')) { $assigned += (Get-VMProcessor -VM $vm).Count }
        if ($assigned + 4 -gt $computer.NumberOfLogicalProcessors - 1) { $info.blockers += 'Need four spare logical CPUs and one Windows CPU reserve.' }
        Assert-BenchIdle
        if (@($all | Where-Object Name -CEQ 'clc-disk-benchmark').Count) { $info.blockers += 'Benchmark VM name is already occupied. Use explicit owned Cleanup if appropriate.' }
        $network = Get-Content -LiteralPath (Join-Path $settings.controller.dataDirectory 'network.json') -Raw | ConvertFrom-Json
        $switch = @(Get-VMSwitch | Where-Object { [string]$_.Id -eq $network.switchId })
        if ($network.phase -ne 'ready' -or $switch.Count -ne 1 -or $switch[0].Notes -cne ('clc-runners-network/v1|' + $network.owner) -or
            $switch[0].Name -cne $settings.hyperv.switchName -or $network.subnet -cne $settings.hyperv.subnet) { throw 'Existing dedicated network ownership/settings mismatch.' }
        if (($settings.hyperv.subnet -split '/')[1] -ne '24') { throw 'This small harness requires the existing IPv4 /24 network.' }
        $address = [Net.IPAddress]::Parse($BenchmarkAddress).GetAddressBytes()
        $first = [Net.IPAddress]::Parse($settings.hyperv.firstAddress).GetAddressBytes()
        $last = [Net.IPAddress]::Parse($settings.hyperv.lastAddress).GetAddressBytes()
        if ($address.Length -ne 4 -or ($address[0..2] -join '.') -ne ($first[0..2] -join '.') -or $address[3] -in @(0,255) -or
            ($address[3] -ge $first[3] -and $address[3] -le $last[3]) -or $BenchmarkAddress -eq $settings.hyperv.gateway) {
            throw 'Benchmark address must be in the dedicated subnet and outside the controller allocation pool.'
        }
		$vmAddressInUse = @(
			Get-VMNetworkAdapter -All | Where-Object {
				$ipProperty = $_.PSObject.Properties['IPAddresses']
				$null -ne $ipProperty -and
					@($ipProperty.Value) -contains $BenchmarkAddress
			}
		).Count -gt 0

		$hostAddressInUse = @(
			Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop |
				Where-Object { $_.IPAddress -eq $BenchmarkAddress }
		).Count -gt 0

		$addressResponds = Test-Connection -TargetName $BenchmarkAddress -Count 1 -TimeoutSeconds 1 -Quiet

		if ($vmAddressInUse -or $hostAddressInUse -or $addressResponds) {
			throw 'Benchmark IP address is already in use.'
		}
        $info.network = @{ switchName=$switch[0].Name; address=$BenchmarkAddress; subnet=$network.subnet; gateway=$network.gateway }
    } catch { $info.blockers += $_.Exception.Message }
    if (Test-Path -LiteralPath $ResourceDirectory) { $info.blockers += 'Resource directory already exists; inspect it and use explicit owned Cleanup before another run.' }
    [pscustomobject]$info
}

function Get-BenchSshArguments {
    @('-i', (Join-Path $ResourceDirectory 'client'), '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=5', '-o', 'StrictHostKeyChecking=yes',
      '-o', ('UserKnownHostsFile=' + (Join-Path $ResourceDirectory 'known_hosts')), '-o', 'HostKeyAlias=clc-disk-benchmark')
}

function Update-BenchCounterPaths {
    try {
        # Instances can appear only after boot and change when disks are reset.
        $storageCounters = @(Get-Counter -ListSet '*Hyper-V*Storage*' -ErrorAction Stop)
        Write-BenchJson (Join-Path $OutputDirectory 'host-counter-discovery.json') @($storageCounters | Select-Object CounterSetName,PathsWithInstances)
        $pattern = [regex]::Escape($script:manifest.name) + '|' + [regex]::Escape($script:manifest.vmId)
        $script:counterPaths = @($storageCounters.PathsWithInstances | Where-Object { $_ -match $pattern -and $_ -match 'Write.*(Time|Latency)|(Time|Latency).*Write' })
        if ($script:counterPaths.Count) { $preflight.hostVirtualDiskLatency = 'Matched host counter samples are in host-counters.jsonl; counter units follow the installed counter definitions.' }
        else { $preflight.hostVirtualDiskLatency = 'Unavailable: installed storage counters have no write time/latency instance matched to this VM.' }
    } catch { $script:counterPaths = @(); $preflight.hostVirtualDiskLatency = 'Unavailable: Hyper-V storage performance counter discovery failed.' }
}

function Invoke-BenchGuest([string]$Command, [string]$LogName, [int]$TimeoutSeconds = 1200) {
    $info = [Diagnostics.ProcessStartInfo]::new('ssh.exe')
    $info.UseShellExecute = $false
    $info.RedirectStandardOutput = $true; $info.RedirectStandardError = $true
    foreach ($arg in @((Get-BenchSshArguments)) + @("runner@$BenchmarkAddress", $Command)) { $info.ArgumentList.Add($arg) }
    $process = [Diagnostics.Process]::Start($info)
    $stdout = $process.StandardOutput.ReadToEndAsync(); $stderr = $process.StandardError.ReadToEndAsync()
    $watch = [Diagnostics.Stopwatch]::StartNew()
    try {
        while (-not $process.WaitForExit(5000)) {
            Assert-BenchIdle
            $volume = Get-Volume -FilePath ($ResourceDirectory.TrimEnd('\') + '\')
            if ($volume.SizeRemaining -lt 15GB) { throw 'Storage reserve reached; terminating only benchmark work.' }
            $bootBytes = [long]0
            $sqlChildBytes = [long]0
            foreach ($entry in @(Get-ChildItem -LiteralPath $ResourceDirectory -File -Recurse | Where-Object Extension -In @('.vhdx','.avhdx'))) {
                if ($entry.Name -notin @('boot-parent.vhdx','sql-baseline.vhdx','sql-active.vhdx') -and $entry.Name -notlike 'sql-active_*.avhdx') { $bootBytes += $entry.Length }
                if ($entry.Name -like 'sql-active*' -and (Get-VHD -Path $entry.FullName).VhdType -eq 'Differencing') { $sqlChildBytes += $entry.Length }
            }
            if ($bootBytes -gt $BootGrowthBudgetGiB * 1GB) { throw 'Benchmark boot-overlay growth exceeded the reserved budget.' }
            if ($sqlChildBytes -gt $SqlChildGrowthBudgetGiB * 1GB) { throw 'Benchmark SQL differencing-disk growth exceeded the reserved budget.' }
            if ($watch.Elapsed.TotalSeconds -gt $TimeoutSeconds) { throw 'Benchmark guest command timed out.' }
            if ($script:manifest.vmId) {
                $vm = Get-BenchVM $script:manifest $ResourceDirectory
                $disks = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object { @{path=$_.Path; bytes=(Get-Item -LiteralPath $_.Path).Length} })
                $sample = @{utc=[DateTime]::UtcNow.ToString('o'); phase=$LogName; disks=$disks; vmCpuPercent=$vm.CPUUsage; freeBytes=$volume.SizeRemaining}
                Add-Content -LiteralPath (Join-Path $OutputDirectory 'host-samples.jsonl') -Value ($sample | ConvertTo-Json -Depth 5 -Compress)
                if ($script:counterPaths.Count) {
                    try {
                        $counters = (Get-Counter -Counter $script:counterPaths -ErrorAction Stop).CounterSamples | Select-Object Path,CookedValue,Status
                        Add-Content -LiteralPath (Join-Path $OutputDirectory 'host-counters.jsonl') -Value (@{utc=[DateTime]::UtcNow.ToString('o'); phase=$LogName; counters=@($counters)} | ConvertTo-Json -Depth 5 -Compress)
                    } catch { $script:counterPaths = @(); $preflight.hostVirtualDiskLatency = 'Unavailable: matched host counter sampling failed.' }
                }
            }
        }
        Assert-BenchIdle
    } finally {
        if (-not $process.HasExited) { $process.Kill($true); $process.WaitForExit() }
        [IO.File]::WriteAllText((Join-Path $OutputDirectory ($LogName + '.log')), $stdout.GetAwaiter().GetResult() + $stderr.GetAwaiter().GetResult())
    }
    if ($process.ExitCode) { throw "Guest command failed (exit $($process.ExitCode)); see $LogName.log" }
}

function Start-BenchGuest {
    Assert-BenchIdle
    $vm = Get-BenchVM $script:manifest $ResourceDirectory
    Start-VM -VM $vm
    $watch = [Diagnostics.Stopwatch]::StartNew()
    while ($watch.Elapsed.TotalSeconds -lt 180) {
        & ssh.exe @(Get-BenchSshArguments) "runner@$BenchmarkAddress" 'test -f /home/runner/benchmark-ready' 2>$null | Out-Null
        if (-not $LASTEXITCODE) { $bootSeconds=$watch.Elapsed.TotalSeconds; Update-BenchCounterPaths; return $bootSeconds }
        Assert-BenchIdle
        Start-Sleep -Seconds 2
    }
    throw 'Benchmark VM SSH/cloud-init readiness timed out.'
}

function Stop-BenchGuest {
    $vm = Get-BenchVM $script:manifest $ResourceDirectory
    if ($vm.State -ne 'Off') {
        Invoke-BenchGuest 'sudo systemctl poweroff --no-block' 'shutdown' 30
        $watch = [Diagnostics.Stopwatch]::StartNew()
        while ((Get-VM -Id $vm.Id).State -ne 'Off') {
            if ($watch.Elapsed.TotalSeconds -gt 60) { throw 'Graceful benchmark shutdown timed out.' }
            Start-Sleep -Seconds 1
        }
    }
}

function Copy-BenchResults([string]$Label) {
    Invoke-BenchGuest "cd /home/runner/clc-disk-benchmark; tar -czf /home/runner/benchmark-results.tar.gz -C results $Label" 'collect'
    $target = Join-Path $OutputDirectory ($Label + '.tar.gz')
    $null = Invoke-BenchNative scp.exe (@(Get-BenchSshArguments) + @("runner@${BenchmarkAddress}:/home/runner/benchmark-results.tar.gz", $target))
    $null = Invoke-BenchNative tar.exe @('-xzf', $target, '-C', $OutputDirectory)
}

if ($Action -eq 'Cleanup') {
    if (-not (Test-Path -LiteralPath $ResourceDirectory)) { Write-Host 'No benchmark resource directory exists; nothing to clean.'; return }
    $manifest = Get-Content -LiteralPath (Join-Path $ResourceDirectory 'ownership.json') -Raw | ConvertFrom-Json
    Import-Module Hyper-V
    Remove-BenchResources $manifest $ResourceDirectory
    Write-Host "Removed only benchmark-owned VM/files from $ResourceDirectory"
    return
}
if ($Action -eq 'Report') {
    $null = Invoke-BenchNative python @((Join-Path $PSScriptRoot 'report.py'), $OutputDirectory)
    Write-Host (Join-Path $OutputDirectory 'comparison.md')
    return
}
if ((Test-Path -LiteralPath $OutputDirectory) -and @(Get-ChildItem -LiteralPath $OutputDirectory -Force).Count) {
    throw 'Use a fresh output directory so prior results cannot enter a new comparison.'
}
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
$preflight = Get-BenchPreflight
Write-BenchJson (Join-Path $OutputDirectory 'preflight.json') $preflight
if ($preflight.blockers.Count) {
    Write-BenchJson (Join-Path $OutputDirectory 'execution.json') @{status='blocked'; mode=$Action; errors=$preflight.blockers; cleanup='No benchmark resources created.'}
    $null = Invoke-BenchNative python @((Join-Path $PSScriptRoot 'report.py'), $OutputDirectory)
    throw ('Preflight blocked: ' + ($preflight.blockers -join ' ') + " Evidence: $OutputDirectory")
}
if ($Action -eq 'Check') { Write-Host "Benchmark preflight passed: $OutputDirectory"; return }

$script:manifest = [ordered]@{schema=1; owner=[guid]::NewGuid().ToString(); name='clc-disk-benchmark'; resourceRoot=$ResourceDirectory; vmId=''}
$script:manifest.marker = "clc-disk-benchmark/v1|$($script:manifest.owner)"
$execution = [ordered]@{status='running'; mode=$Action; errors=@(); cleanup='pending'; repetitions=$(if ($Action -in @('Prepare','ValidateSql')) {0} else {$Repetitions}); order=@()}
$owned = $false
try {
    Assert-BenchIdle
    $null = Assert-BenchPath $ResourceDirectory $resourceParent
    Protect-RunnerDirectory $ResourceDirectory
    $owned = $true
    Write-BenchJson (Join-Path $ResourceDirectory 'ownership.json') $script:manifest
    $parent = Join-Path $ResourceDirectory 'boot-parent.vhdx'
    Copy-Item -LiteralPath $preflight.image.path -Destination $parent
    (Get-Item -LiteralPath $parent).IsReadOnly = $true
    $boot = Join-Path $ResourceDirectory 'boot-baseline.vhdx'
    $sqlBaseline = Join-Path $ResourceDirectory 'sql-baseline.vhdx'
    $null = New-VHD -Path $boot -ParentPath $parent -Differencing
    $null = New-VHD -Path $sqlBaseline -Fixed -SizeBytes 40GB -LogicalSectorSizeBytes 512 -PhysicalSectorSizeBytes 4096
    foreach ($diskPath in @($parent,$boot,$sqlBaseline)) { Assert-CLCFileAttributes $diskPath }
    foreach ($key in @('client','host')) { $null = Invoke-BenchNative ssh-keygen.exe @('-q','-t','ed25519','-N','','-f',(Join-Path $ResourceDirectory $key)) }
    $public = (Get-Content -LiteralPath (Join-Path $ResourceDirectory 'client.pub') -Raw).Trim()
    $hostPublic = (Get-Content -LiteralPath (Join-Path $ResourceDirectory 'host.pub') -Raw).Trim()
    $userData = @{ssh_pwauth=$false; disable_root=$true; ssh_deletekeys=$true
        ssh_keys=@{ed25519_private=(Get-Content -LiteralPath (Join-Path $ResourceDirectory 'host') -Raw); ed25519_public=$hostPublic}
        users=@(@{name='runner'; shell='/bin/bash'; lock_passwd=$true; sudo='ALL=(ALL) NOPASSWD:ALL'; groups=@('sudo','docker'); ssh_authorized_keys=@($public)})
        # The image installs its unit in /etc, so systemctl mask alone cannot
        # replace it. This changes only the new benchmark guest's private OS.
        bootcmd=@('ln -sfn /dev/null /etc/systemd/system/clc-runner.service && systemctl daemon-reload')
        runcmd=@('systemctl mask --now clc-runner.service && systemctl start docker && touch /home/runner/benchmark-ready')}
    [IO.File]::WriteAllText((Join-Path $ResourceDirectory 'user-data'), "#cloud-config`n" + ($userData | ConvertTo-Json -Depth 12))
    Write-BenchJson (Join-Path $ResourceDirectory 'meta-data') @{'instance-id'=$script:manifest.owner; 'local-hostname'=$script:manifest.name}
    $octets = [Net.IPAddress]::Parse($BenchmarkAddress).GetAddressBytes()
    $mac = '00155D{0:X2}{1:X2}{2:X2}' -f $octets[1],$octets[2],$octets[3]
    $macColon = ($mac -replace '(..)(?!$)','$1:').ToLowerInvariant()
    Write-BenchJson (Join-Path $ResourceDirectory 'network-config') @{version=2; ethernets=@{benchmark=@{match=@{macaddress=$macColon}; 'set-name'='eth0'; dhcp4=$false; dhcp6=$false; addresses=@("$BenchmarkAddress/24"); routes=@(@{to='default'; via=$settings.hyperv.gateway}); nameservers=@{addresses=$settings.hyperv.dns}}}}
    [IO.File]::WriteAllText((Join-Path $ResourceDirectory 'known_hosts'), "clc-disk-benchmark $hostPublic`n")
    $seed = Join-Path $ResourceDirectory 'seed.iso'
    Push-Location $repo
    try { $null = Invoke-BenchNative (Get-RunnerGo) @('run','./benchmarks/hyperv-disk/seed',$ResourceDirectory,$seed) } finally { Pop-Location }
    $vm = New-VM -Name $script:manifest.name -Generation 2 -Path (Join-Path $ResourceDirectory 'vm') -VHDPath $boot -MemoryStartupBytes 16GB -SwitchName $settings.hyperv.switchName
    $script:manifest.vmId = [string]$vm.Id
    Write-BenchJson (Join-Path $ResourceDirectory 'ownership.json') $script:manifest
    Set-VM -VM $vm -Notes $script:manifest.marker -AutomaticStartAction Nothing -AutomaticStopAction ShutDown -AutomaticCheckpointsEnabled $false -CheckpointType Disabled
    Set-VMProcessor -VM $vm -Count 4
    Set-VMMemory -VM $vm -DynamicMemoryEnabled $false
    Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate MicrosoftUEFICertificateAuthority
    Set-VMNetworkAdapter -VM $vm -StaticMacAddress $mac -MacAddressSpoofing Off -DhcpGuard On -RouterGuard On
    Add-VMHardDiskDrive -VM $vm -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 1 -Path $sqlBaseline
    Add-VMDvdDrive -VM $vm -Path $seed
    Grant-CLCVMFile $parent ([string]$vm.Id)
    Grant-CLCVMFile $seed ([string]$vm.Id)
    foreach ($diskPath in @($boot,$sqlBaseline)) { Grant-BenchWritableDisk $diskPath $script:manifest $ResourceDirectory }
    $null = Start-BenchGuest
    Invoke-BenchGuest 'cloud-init status --wait >/dev/null; mkdir /home/runner/clc-disk-benchmark' 'cloud-init'
    if ($Action -ne 'ValidateSql') {
        $archive = Join-Path $ResourceDirectory 'source.tar'
        $null = Invoke-BenchNative git @('-C',$AsapRepository,'archive','--format=tar',('--output='+$archive),$Commit)
        $null = Invoke-BenchNative scp.exe (@(Get-BenchSshArguments) + @($archive,"runner@${BenchmarkAddress}:/home/runner/clc-disk-benchmark/source.tar"))
    }
    $upload = Join-Path $ResourceDirectory 'scripts.tar'
    $null = Invoke-BenchNative tar.exe @('-cf',$upload,'-C',$PSScriptRoot,'guest.sh','measure.py','workload.sql')
    $null = Invoke-BenchNative scp.exe (@(Get-BenchSshArguments) + @($upload,"runner@${BenchmarkAddress}:/home/runner/clc-disk-benchmark/scripts.tar"))
    $guestAction = if ($Action -eq 'ValidateSql') { 'validate' } else { 'prepare' }
    Invoke-BenchGuest "cd /home/runner/clc-disk-benchmark; tar -xf scripts.tar; bash guest.sh $guestAction" 'prepare' 1800
    Copy-BenchResults 'preparation'
    Stop-BenchGuest
    Get-VMDvdDrive -VM $vm | Set-VMDvdDrive -Path $null
    Get-VMHardDiskDrive -VM $vm | Remove-VMHardDiskDrive
    (Get-Item -LiteralPath $boot).IsReadOnly = $true
    (Get-Item -LiteralPath $sqlBaseline).IsReadOnly = $true
    Grant-CLCVMFile $boot ([string]$vm.Id)
    Grant-CLCVMFile $sqlBaseline ([string]$vm.Id)
    $privateSeedFiles = @('user-data','host','seed.iso','scripts.tar')
    if ($Action -ne 'ValidateSql') { $privateSeedFiles += 'source.tar' }
    foreach ($privateSeedFile in $privateSeedFiles) {
        Remove-Item -LiteralPath (Assert-BenchPath (Join-Path $ResourceDirectory $privateSeedFile) $ResourceDirectory) -Force
    }
    $preparation = Get-Content -LiteralPath (Join-Path $OutputDirectory 'preparation/result.json') -Raw | ConvertFrom-Json
    if (-not $preparation.success -or $preparation.readiness.select_1 -ne 1) { throw 'SQL initialization did not complete a successful SELECT 1.' }
    for ($round=0; $round -lt $execution.repetitions; $round++) {
        foreach ($offset in 0..2) {
            $scenario = @('A','B','C')[($round + $offset) % 3]
            $label = '{0}-{1}' -f $scenario,($round+1)
            $execution.order += $label
            Write-Host "Measuring $label (only three selected migration tests)."
            Assert-BenchIdle
            $reset = [Diagnostics.Stopwatch]::StartNew()
            $osActive = Join-Path $ResourceDirectory 'boot-active.vhdx'
            $sqlActive = Join-Path $ResourceDirectory 'sql-active.vhdx'
            $null = New-VHD -Path $osActive -ParentPath $boot -Differencing
            if ($scenario -eq 'A') { $null = New-VHD -Path $sqlActive -ParentPath $sqlBaseline -Differencing }
            else { Copy-Item -LiteralPath $sqlBaseline -Destination $sqlActive; (Get-Item -LiteralPath $sqlActive).IsReadOnly = $false }
            foreach ($diskPath in @($osActive,$sqlActive)) { Grant-BenchWritableDisk $diskPath $script:manifest $ResourceDirectory }
            Add-VMHardDiskDrive -VM $vm -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 0 -Path $osActive
            Add-VMHardDiskDrive -VM $vm -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 1 -Path $sqlActive
            Set-VMFirmware -VM $vm -FirstBootDevice (Get-VMHardDiskDrive -VM $vm | Where-Object ControllerLocation -EQ 0)
            if ($scenario -eq 'C') {
                Set-VM -VM $vm -CheckpointType Standard
                Checkpoint-VM -VM $vm -SnapshotName "baseline-$($script:manifest.owner)"
                if (@(Get-VMSnapshot -VM $vm).Count -ne 1) { throw 'Actual VM checkpoint missing.' }
            } else { Set-VM -VM $vm -CheckpointType Disabled }
            $layout = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object {
                $vhd=Get-VHD -Path $_.Path
                @{slot=$_.ControllerLocation; path=$_.Path; type=[string]$vhd.VhdType; parent=$vhd.ParentPath; bytes=(Get-Item -LiteralPath $_.Path).Length; blockSize=$vhd.BlockSize; logicalSectorSize=$vhd.LogicalSectorSize; physicalSectorSize=$vhd.PhysicalSectorSize}
            })
            $dataDrive = @($layout | Where-Object slot -EQ 1)[0]
            if (($scenario -eq 'B' -and ($dataDrive.type -ne 'Fixed' -or $dataDrive.parent)) -or
                ($scenario -in @('A','C') -and $dataDrive.type -ne 'Differencing') -or
                ($scenario -eq 'C' -and [IO.Path]::GetExtension($dataDrive.path) -ine '.avhdx')) { throw 'Scenario disk layout differs from the requested storage configuration.' }
            $reset.Stop()
            $createSeconds = $reset.Elapsed.TotalSeconds
            $bootSeconds = Start-BenchGuest
            $measurementError = $null
            try { Invoke-BenchGuest "cd /home/runner/clc-disk-benchmark; bash guest.sh run $label" $label 1200 }
            catch { $measurementError = $_ }
            # Preserve partial diagnostics even when tests/SQL fail; never count a fast failure.
            Copy-BenchResults $label
            $endLayout = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object { @{slot=$_.ControllerLocation; path=$_.Path; bytes=(Get-Item -LiteralPath $_.Path).Length} })
            $reset.Restart()
            Stop-BenchGuest
            $shutdownSeconds = $reset.Elapsed.TotalSeconds
            $checkpointRestoreSeconds = $null
            if ($scenario -eq 'C') {
                $restore = [Diagnostics.Stopwatch]::StartNew()
                $checkpoint = @(Get-VMSnapshot -VM $vm)
                if ($checkpoint.Count -ne 1 -or $checkpoint[0].Name -cne "baseline-$($script:manifest.owner)") { throw 'Owned baseline checkpoint missing before restoration.' }
                Restore-VMSnapshot -VMSnapshot $checkpoint[0] -Confirm:$false
                $checkpointRestoreSeconds = $restore.Elapsed.TotalSeconds
            }
            $reset.Restart()
            Get-VMSnapshot -VM $vm | Remove-VMSnapshot -Confirm:$false
            Get-VMHardDiskDrive -VM $vm | Remove-VMHardDiskDrive
            foreach ($path in @($osActive,$sqlActive)) { Remove-Item -LiteralPath (Assert-BenchPath $path $ResourceDirectory) -Force }
            $reset.Stop()
            $recoverySeconds = $shutdownSeconds + $(if ($scenario -eq 'C') { $checkpointRestoreSeconds } else { $createSeconds })
            Write-BenchJson (Join-Path $OutputDirectory "$label/host.json") @{success=(-not $measurementError); scenario=$scenario; round=($round+1); layout=$layout; endLayout=$endLayout; diskCreateSeconds=$createSeconds; bootReadySeconds=$bootSeconds; shutdownSeconds=$shutdownSeconds; checkpointRestoreSeconds=$checkpointRestoreSeconds; teardownSeconds=$reset.Elapsed.TotalSeconds; resetSeconds=$recoverySeconds}
            if ($measurementError) { throw $measurementError }
            $result = Get-Content -LiteralPath (Join-Path $OutputDirectory "$label/result.json") -Raw | ConvertFrom-Json
            if (-not $result.success -or $result.image_digest -cne $preparation.image_digest) { throw 'Measurement failed or SQL image digest changed.' }
        }
    }
    $execution.status = 'complete'
} catch {
    $execution.status = 'failed'
    $execution.errors += $_.Exception.Message
    Write-Warning $_.Exception.Message
    if ($script:manifest.vmId) {
        try {
            $failedVM = Get-BenchVM $script:manifest $ResourceDirectory
            if ($failedVM -and $failedVM.State -eq 'Running') {
                & ssh.exe @(Get-BenchSshArguments) "runner@$BenchmarkAddress" 'cd /home/runner/clc-disk-benchmark; tar -czf /home/runner/benchmark-failure.tar.gz --ignore-failed-read build.log docker-pull.log dotnet-install.sha256 results 2>/dev/null' 2>$null | Out-Null
                $null = Invoke-BenchNative scp.exe (@(Get-BenchSshArguments) + @("runner@${BenchmarkAddress}:/home/runner/benchmark-failure.tar.gz",(Join-Path $OutputDirectory 'guest-failure.tar.gz')))
            }
        } catch { $execution.errors += 'Partial guest diagnostics could not be retrieved.' }
    }
} finally {
    if ($owned) {
        try { Remove-BenchResources $script:manifest $ResourceDirectory; $execution.cleanup = 'Benchmark VM, checkpoints, disks, seed and SSH keys removed.' }
        catch { $execution.cleanup = 'Cleanup requires attention: ' + $_.Exception.Message; $execution.errors += $execution.cleanup }
    }
    if ((Get-FileHash -LiteralPath $resolved.Path).Hash -cne $configHash) { $execution.errors += 'Controller configuration changed externally during the benchmark.' }
    Write-BenchJson (Join-Path $OutputDirectory 'preflight.json') $preflight
    Write-BenchJson (Join-Path $OutputDirectory 'execution.json') $execution
    $null = Invoke-BenchNative python @((Join-Path $PSScriptRoot 'report.py'), $OutputDirectory)
}
Write-Host "Comparison and raw results: $OutputDirectory"
if ($execution.status -ne 'complete' -or $execution.errors.Count) { throw 'Benchmark did not complete successfully; see execution.json and comparison.md.' }
