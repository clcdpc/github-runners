#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
$repository = $script:RunnerRoot
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('clc-preparation-reset-' + [guid]::NewGuid().ToString('N'))
$resetChecks = @{ Count=0 }
function Assert-Reset($Condition, [string]$Message) {
    $resetChecks.Count++
    if (-not $Condition) { throw $Message }
}
function Assert-ResetFailure([scriptblock]$Action, [string]$Pattern) {
    $message = ''
    try { & $Action | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Reset ($message -match $Pattern) "Expected '$Pattern', got '$message'"
}
try {
    $null = New-Item -ItemType Directory -Path $scratch
    & {
        # Execute the real reset entry point and real path/state/ownership checks
        # in isolated checkouts. Only the host inventory/removal calls are mocked.
        $resetTestState = @{}
        function New-ResetFixture([string]$Name) {
            $root = Join-Path $scratch $Name
            $scripts = Join-Path $root 'scripts'
            $data = Join-Path $root 'local/data'
            $null = New-Item -ItemType Directory -Path $scripts, $data, (Join-Path $root 'config') -Force
            foreach ($file in @('Reset-RunnerPreparation.ps1', 'Common.ps1', 'RunnerAccount.ps1', 'PreparationReset.ps1')) {
                Copy-Item -LiteralPath (Join-Path $repository "scripts/$file") -Destination $scripts
            }
            [IO.File]::WriteAllText((Join-Path $scripts 'Host.ps1'), 'function Test-RunnerAdministrator { $resetTestState.Admin }')
            Copy-Item -LiteralPath (Join-Path $repository 'config/controller.example.json') -Destination (Join-Path $root 'config/controller.example.json')
            $config = Join-Path $root 'local/controller.json'
            Copy-Item -LiteralPath (Join-Path $root 'config/controller.example.json') -Destination $config
            $owner = [guid]::NewGuid().ToString()
            $switch = [pscustomobject]@{ Name='CLC-Runners'; Id=[guid]::NewGuid(); Notes=('clc-runners-network/v1|' + $owner); SwitchType='Internal' }
            $foreignSwitch = [pscustomobject]@{ Name='UnrelatedSwitch'; Id=[guid]::NewGuid(); Notes=''; SwitchType='Internal' }
            $nat = [pscustomobject]@{ Name='CLC-Runners'; InternalIPInterfaceAddressPrefix='192.168.231.0/24'; InstanceID=[guid]::NewGuid().ToString() }
            $foreignNat = [pscustomobject]@{ Name='UnrelatedNat'; InternalIPInterfaceAddressPrefix='10.20.0.0/24'; InstanceID=[guid]::NewGuid().ToString() }
            $resetTestState.Clear()
            foreach ($item in @{
                Admin=$true; Process=$false; Task=$false; ExpectRecord=$true; Switches=@($switch, $foreignSwitch); Nats=@($nat, $foreignNat)
                Adapters=@([pscustomobject]@{ IsManagementOs=$true; SwitchId=$switch.Id }); Mappings=@()
                Failure=''; SwitchReads=0; ChangeOnRead=0; Events=[Collections.Generic.List[string]]::new()
            }.GetEnumerator()) { $resetTestState[$item.Key] = $item.Value }
            $network = Join-Path $data 'network.json'
            Write-RunnerJson $network @{ version=1; owner=$owner; switchId=[string]$switch.Id; phase='ready'; switchName=$switch.Name; natName=$nat.Name; subnet=$nat.InternalIPInterfaceAddressPrefix; gateway='192.168.231.1' }
            Write-RunnerJson (Join-Path $data 'account-preparation.json') @{ version=1; accountSid='S-1-5-21-101-102-103-1001'; config=$config }
            Write-RunnerJson (Join-Path $data 'host-validation.json') @{ ready=$true }
            [IO.File]::WriteAllText((Join-Path $root 'keep.txt'), 'checkout content')
            [pscustomobject]@{ Root=$root; Data=$data; Config=$config; Network=$network; Entry=(Join-Path $scripts 'Reset-RunnerPreparation.ps1'); Switch=$switch; Nat=$nat; ForeignSwitch=$foreignSwitch; ForeignNat=$foreignNat }
        }
        function Import-Module { [CmdletBinding()]param($Name) Assert-Reset ($Name -eq 'Hyper-V') 'Unexpected module import' }
        function Get-RunnerControllerProcesses { @() }
        function Get-Process { [CmdletBinding()]param($Name) if ($resetTestState.Process) { [pscustomobject]@{ Name='clc-runners'; Path='C:\another-checkout\bin\clc-runners.exe' } } }
        function Get-ScheduledTask { [CmdletBinding()]param($TaskName) if ($resetTestState.Task) { [pscustomobject]@{ TaskName=$TaskName } } }
        function Get-VMSwitch {
            [CmdletBinding()]param()
            $resetTestState.SwitchReads++
            if ($resetTestState.ChangeOnRead -eq $resetTestState.SwitchReads) { $resetTestState.Switches[0].Id = [guid]::NewGuid() }
            $resetTestState.Switches
        }
        function Get-NetNat { [CmdletBinding()]param() if ($resetTestState.Failure -eq 'inventory') { throw 'NAT inventory denied' }; $resetTestState.Nats }
        function Get-VMNetworkAdapter { [CmdletBinding()]param([switch]$All) Assert-Reset $All 'VM inventory was not host-wide'; $resetTestState.Adapters }
        function Get-NetNatStaticMapping {
            [CmdletBinding()]param($NatName)
            Assert-Reset ($NatName -eq 'CLC-Runners') 'Static mapping query escaped the selected NAT'
            if ($resetTestState.Failure -eq 'mapping-inventory') { throw 'Static mapping inventory denied' }
            if (-not $resetTestState.Mappings.Count) { Write-Error 'No mapping objects found' -Category ObjectNotFound -ErrorId CmdletizationQuery_NotFound; return }
            $resetTestState.Mappings
        }
        function Remove-NetNat {
            [CmdletBinding(SupportsShouldProcess)]param($InputObject)
            Assert-Reset ($InputObject.InstanceID -eq $fixture.Nat.InstanceID) 'NAT removal escaped the selected identity'
            $resetTestState.Events.Add('nat')
            if ($resetTestState.Failure -eq 'nat-removal') { throw 'NAT removal failed' }
            if ($resetTestState.Failure -ne 'nat-remains') { $resetTestState.Nats = @($resetTestState.Nats | Where-Object InstanceID -NE $InputObject.InstanceID) }
        }
        function Remove-VMSwitch {
            [CmdletBinding(SupportsShouldProcess)]param($VMSwitch, [switch]$Force)
            Assert-Reset ($VMSwitch.Id -eq $fixture.Switch.Id) 'Switch removal escaped the selected identity'
            $resetTestState.Events.Add('switch')
            if ($resetTestState.ExpectRecord) { Assert-Reset (Test-Path -LiteralPath $fixture.Network) 'Ownership record was deleted before network removal completed' }
            if ($resetTestState.Failure -eq 'switch-removal') { throw 'Switch removal failed' }
            $resetTestState.Switches = @($resetTestState.Switches | Where-Object Id -NE $VMSwitch.Id)
        }

        $fixture = New-ResetFixture 'preview-and-reset'
        $configHash = (Get-FileHash -LiteralPath $fixture.Config).Hash
        $networkHash = (Get-FileHash -LiteralPath $fixture.Network).Hash
        & $fixture.Entry -WhatIf 6>$null
        Assert-Reset ($resetTestState.Events.Count -eq 0 -and (Get-FileHash -LiteralPath $fixture.Network).Hash -eq $networkHash) 'WhatIf changed resources or metadata'
        Assert-Reset (-not (Test-Path -LiteralPath (Join-Path $fixture.Root 'bin')) -and -not (Test-Path -LiteralPath ($fixture.Network + '.lock'))) 'WhatIf created lock files/directories'
        & $fixture.Entry -Confirm:$false 6>$null
        Assert-Reset (($resetTestState.Events -join ',') -eq 'nat,switch') 'Reset did not remove NAT before switch'
        Assert-Reset ($resetTestState.Switches.Count -eq 1 -and $resetTestState.Switches[0].Id -eq $fixture.ForeignSwitch.Id -and $resetTestState.Nats.Count -eq 1 -and $resetTestState.Nats[0].InstanceID -eq $fixture.ForeignNat.InstanceID) 'Reset changed unrelated network resources'
        Assert-Reset (@(Get-ChildItem -LiteralPath $fixture.Data | Where-Object Name -NE 'network.json.lock').Count -eq 0) 'Preparation records remained'
        Assert-Reset ((Get-FileHash -LiteralPath $fixture.Config).Hash -eq $configHash -and (Get-Content -LiteralPath (Join-Path $fixture.Root 'keep.txt')) -eq 'checkout content') 'Reset changed the configuration or checkout'
        & $fixture.Entry -Confirm:$false 6>$null
        Assert-Reset (($resetTestState.Events -join ',') -eq 'nat,switch') 'Repeating reset removed more resources'

        $fixture = New-ResetFixture 'missing-record'
        Remove-Item -LiteralPath $fixture.Network
        $resetTestState.ExpectRecord = $false
        & $fixture.Entry -Confirm:$false 6>$null
        Assert-Reset (($resetTestState.Events -join ',') -eq 'nat,switch') 'The marked network could not be reset from a fresh checkout'

        $fixture = New-ResetFixture 'missing-config'
        Remove-Item -LiteralPath $fixture.Config
        & $fixture.Entry -Confirm:$false 6>$null
        Assert-Reset (($resetTestState.Events -join ',') -eq 'nat,switch' -and -not (Test-Path -LiteralPath $fixture.Config)) 'Default reset required or created a configuration'

        foreach ($scenario in @('foreign-switch', 'external-switch', 'wrong-owner', 'wrong-id', 'wrong-prefix', 'attached-vm', 'nat-only-unverified', 'nat-only-intent', 'mappings', 'inventory', 'mapping-inventory', 'active-controller', 'startup-task', 'activated-storage', 'invalid-record', 'not-admin')) {
            $fixture = New-ResetFixture $scenario
            $pattern = switch ($scenario) {
                'foreign-switch' { $fixture.Switch.Notes='unrelated'; 'not a marked runner' }
                'external-switch' { $fixture.Switch.SwitchType='External'; 'not a marked runner' }
                'wrong-owner' { $fixture.Switch.Notes='clc-runners-network/v1|' + [guid]::NewGuid().ToString(); 'ownership mismatch' }
                'wrong-id' { $record=Read-RunnerJson $fixture.Network; $record.switchId=[guid]::NewGuid().ToString(); Write-RunnerJson $fixture.Network $record; 'ownership mismatch' }
                'wrong-prefix' { $fixture.Nat.InternalIPInterfaceAddressPrefix='10.9.0.0/24'; 'NAT prefix or name differs' }
                'attached-vm' { $resetTestState.Adapters += [pscustomobject]@{ IsManagementOs=$false; SwitchId=$fixture.Switch.Id }; 'VMs are still attached' }
                'nat-only-unverified' { Remove-Item -LiteralPath $fixture.Network; $resetTestState.Switches=@($fixture.ForeignSwitch); 'NAT ownership cannot be verified' }
                'nat-only-intent' { $record=Read-RunnerJson $fixture.Network; $record.switchId=''; $record.phase='intent'; Write-RunnerJson $fixture.Network $record; $resetTestState.Switches=@($fixture.ForeignSwitch); 'NAT ownership cannot be verified' }
                'mappings' { $resetTestState.Mappings=@([pscustomobject]@{ NatName='CLC-Runners' }); 'unexpected static mappings' }
                'inventory' { $resetTestState.Failure='inventory'; 'NAT inventory denied' }
                'mapping-inventory' { $resetTestState.Failure='mapping-inventory'; 'Static mapping inventory denied' }
                'active-controller' { $resetTestState.Process=$true; 'controller is still running' }
                'startup-task' { $resetTestState.Task=$true; 'startup task already exists' }
                'activated-storage' { Write-RunnerJson (Join-Path $fixture.Data 'state.json') @{ version=1 }; 'credentials or controller state' }
                'invalid-record' { Set-Content -LiteralPath $fixture.Network -Value 'invalid json'; 'ownership record is invalid' }
                'not-admin' { $resetTestState.Admin=$false; 'as administrator' }
            }
            Assert-ResetFailure { & $fixture.Entry -Confirm:$false 6>$null } $pattern
            Assert-Reset ($resetTestState.Events.Count -eq 0) "Rejected $scenario modified the network"
            Assert-Reset (Test-Path -LiteralPath (Join-Path $fixture.Data 'account-preparation.json')) "Rejected $scenario removed preparation metadata"
        }

        foreach ($failure in @('nat-removal', 'nat-remains', 'switch-removal')) {
            $fixture = New-ResetFixture $failure
            $resetTestState.Failure = $failure
            Assert-ResetFailure { & $fixture.Entry -Confirm:$false 6>$null } $(if ($failure -eq 'nat-remains') { 'NAT remains after removal' } else { 'removal failed' })
            Assert-Reset (Test-Path -LiteralPath $fixture.Network) "Failed $failure lost the ownership record"
            Assert-Reset (Test-Path -LiteralPath (Join-Path $fixture.Data 'account-preparation.json')) "Failed $failure lost the preparation receipt"
            if ($failure -ne 'switch-removal') { Assert-Reset (-not $resetTestState.Events.Contains('switch')) 'Failed NAT removal proceeded to switch removal' }
            $resetTestState.Failure = ''
            & $fixture.Entry -Confirm:$false 6>$null
            Assert-Reset (-not (Test-Path -LiteralPath $fixture.Network) -and $resetTestState.Switches.Count -eq 1 -and $resetTestState.Nats.Count -eq 1) "Retry after $failure did not finish cleanup"
        }

        $fixture = New-ResetFixture 'nat-only-with-record'
        $resetTestState.Switches = @($fixture.ForeignSwitch)
        & $fixture.Entry -Confirm:$false 6>$null
        Assert-Reset (($resetTestState.Events -join ',') -eq 'nat' -and -not (Test-Path -LiteralPath $fixture.Network)) 'An owned NAT-only remainder could not be reset'

        $fixture = New-ResetFixture 'changed-after-preview'
        Remove-Item -LiteralPath $fixture.Network
        $resetTestState.ChangeOnRead = 2
        Assert-ResetFailure { & $fixture.Entry -Confirm:$false 6>$null } 'changed after preview'
        Assert-Reset ($resetTestState.Events.Count -eq 0) 'A changed resource identity was removed'

        $fixture = New-ResetFixture 'locked-preparation'
        $lock = [IO.File]::Open(($fixture.Network + '.lock'), 'OpenOrCreate', 'ReadWrite', 'None')
        try {
            Assert-ResetFailure { & $fixture.Entry -Confirm:$false 6>$null } 'operation is locked'
            Assert-Reset ($resetTestState.Events.Count -eq 0 -and (Test-Path -LiteralPath $fixture.Network)) 'A locked preparation was changed'
        } finally { $lock.Dispose() }
    }
    Write-Host "Preparation reset: $($resetChecks.Count) preview, ownership, isolation, pre-activation, locking and retry assertions passed (mocked host inventory/removal)."
} finally {
    $absolute = [IO.Path]::GetFullPath($scratch)
    $prefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if ($absolute.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf) -match '^clc-preparation-reset-[a-f0-9]{32}$') { Remove-Item -LiteralPath $absolute -Recurse -Force }
}
