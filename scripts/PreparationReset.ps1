#requires -Version 7.4

function Get-RunnerPreparationResetPlan([string]$Config, [string]$StorageRoot) {
    # Share the preparation path and pre-activation checks, including reparse
    # points, credentials, disks, controller state and installed startup tasks.
    $accountPlan = Get-RunnerAccountPlan $Config $StorageRoot
    if (@(Get-Process -Name 'clc-runners' -ErrorAction SilentlyContinue).Count) {
        throw 'A runner controller is still running on this host. Stop and clean up the active installation before resetting preparation.'
    }
    $net = $accountPlan.Settings.hyperv
    $networkPath = Join-Path $accountPlan.Data 'network.json'
    $record = Read-RunnerJson $networkPath
    if (Test-Path -LiteralPath $networkPath) {
        $owner = [guid]::Empty
        $switchId = [guid]::Empty
        if ($record -isnot [Collections.IDictionary] -or $record['version'] -ne 1 -or
            -not [guid]::TryParseExact([string]$record['owner'], 'D', [ref]$owner) -or $owner -eq [guid]::Empty -or
            $record['phase'] -cnotin @('intent', 'switch', 'ready') -or
            ($record['switchId'] -and (-not [guid]::TryParseExact([string]$record['switchId'], 'D', [ref]$switchId) -or $switchId -eq [guid]::Empty))) {
            throw 'The saved network ownership record is invalid. Restore the original record before resetting preparation.'
        }
        foreach ($key in @('subnet', 'gateway', 'switchName', 'natName')) {
            if ($record[$key] -cne $net[$key]) { throw 'Saved network differs from configuration. Select the original configuration before resetting preparation.' }
        }
    }
    $switches = @(Get-VMSwitch -ErrorAction Stop | Where-Object Name -EQ $net.switchName)
    $nats = @(Get-NetNat -ErrorAction Stop | Where-Object Name -EQ $net.natName)
    if ($switches.Count -gt 1 -or $nats.Count -gt 1) { throw 'Network identity is ambiguous; no resources were removed.' }
    $switch = if ($switches.Count) { $switches[0] } else { $null }
    $nat = if ($nats.Count) { $nats[0] } else { $null }
    if ($switch) {
        $markerOwner = [guid]::Empty
        $parts = ([string]$switch.Notes).Split('|')
        if ($switch.SwitchType -ne 'Internal' -or $switch.Name -cne $net.switchName -or $parts.Count -ne 2 -or
            $parts[0] -cne 'clc-runners-network/v1' -or
            -not [guid]::TryParseExact($parts[1], 'D', [ref]$markerOwner) -or $markerOwner -eq [guid]::Empty) {
            throw 'The configured switch is not a marked runner internal switch; no resources were removed.'
        }
        if ($record -and ($switch.Notes -cne ('clc-runners-network/v1|' + $record['owner']) -or
            ($record['switchId'] -and [string]$switch.Id -ne $record['switchId']))) {
            throw 'Switch ownership mismatch; no resources were removed.'
        }
        $attached = @(Get-VMNetworkAdapter -All -ErrorAction Stop | Where-Object {
            -not $_.IsManagementOs -and [string]$_.SwitchId -eq [string]$switch.Id
        })
        if ($attached.Count) { throw 'VMs are still attached to the runner switch. Stop and run Cleanup in the active installation before resetting preparation.' }
    }
    if ($nat) {
        if ($nat.Name -cne $net.natName -or $nat.InternalIPInterfaceAddressPrefix -cne $net.subnet) {
            throw 'The configured NAT prefix or name differs; no resources were removed.'
        }
        if (-not $switch -and (-not $record -or -not $record['switchId'] -or $record['phase'] -ceq 'intent')) {
            throw 'NAT ownership cannot be verified without the marked switch or its saved switch identity in network.json. Restore the original record before resetting preparation.'
        }
        try { $mappings = @(Get-NetNatStaticMapping -NatName $nat.Name -ErrorAction Stop) }
        catch {
            if ($_.CategoryInfo.Category -ne [Management.Automation.ErrorCategory]::ObjectNotFound -or $_.FullyQualifiedErrorId -notlike 'CmdletizationQuery_NotFound*') { throw }
            $mappings = @()
        }
        if ($mappings.Count) {
            throw 'The runner NAT has unexpected static mappings; inspect them before resetting preparation.'
        }
    }
    $files = @(foreach ($name in @('network.json', 'host-validation.json', 'account-preparation.json')) {
        $path = Join-Path $accountPlan.Data $name
        if (Test-Path -LiteralPath $path) { $path }
    })
    # Bind approval to the selected identities and file contents. Replanning
    # under the management/network locks must reproduce this exact selection.
    $identity = [ordered]@{
        config = $accountPlan.Config; data = $accountPlan.Data
        settings = $accountPlan.Settings
        switch = $(if ($switch) { @([string]$switch.Id, $switch.Name, $switch.Notes) } else { @() })
        nat = $(if ($nat) { $nat | Select-Object Name, InternalIPInterfaceAddressPrefix, InstanceID } else { $null })
        files = @(foreach ($file in $files) { @($file, (Get-FileHash -LiteralPath $file).Hash) })
    } | ConvertTo-Json -Depth 10 -Compress
    [pscustomobject]@{ Root=$accountPlan.Root; Config=$accountPlan.Config; Data=$accountPlan.Data; Switch=$switch; Nat=$nat; Files=$files; Identity=$identity }
}

function Remove-RunnerPreparation($Plan) {
    # Keep the ownership record until both network resources have gone so a
    # failed NAT/switch removal can be retried, including a NAT-only remainder.
    if ($Plan.Nat) {
        Remove-NetNat -InputObject $Plan.Nat -Confirm:$false -ErrorAction Stop
        if (@(Get-NetNat -ErrorAction Stop | Where-Object Name -EQ $Plan.Nat.Name).Count) {
            throw 'Runner NAT remains after removal; preparation records were preserved for retry.'
        }
    }
    if ($Plan.Switch) {
        Remove-VMSwitch -VMSwitch $Plan.Switch -Force -Confirm:$false -ErrorAction Stop
        if (@(Get-VMSwitch -ErrorAction Stop | Where-Object { [string]$_.Id -eq [string]$Plan.Switch.Id }).Count) {
            throw 'Runner switch remains after removal; preparation records were preserved for retry.'
        }
    }
    foreach ($file in $Plan.Files) {
        $full = [IO.Path]::GetFullPath($file)
        if ((Split-Path $full -Parent) -ine $Plan.Data) { throw 'Preparation file escaped the selected data directory.' }
        $null = Assert-RunnerAccountPath $Plan.Data
        if (Test-Path -LiteralPath $full) {
            $item = Get-Item -LiteralPath $full -Force
            if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Preparation record is not a regular file: $full" }
            Remove-Item -LiteralPath $full -Force -ErrorAction Stop
        }
    }
}
