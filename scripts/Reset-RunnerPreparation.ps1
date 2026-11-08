#requires -Version 7.4
<#
.SYNOPSIS
Remove an unused runner preparation so host setup can start again.
.DESCRIPTION
Removes the selected marked internal switch, matching NAT and pre-activation
preparation records. Works from a new checkout when the old network.json is
unavailable, provided the switch still has its runner ownership marker.
Retains the Windows account, configuration, checkout, installed tools and host
features for reuse. Use the normal controller commands to drain/clean up an
activated installation, then select fresh checkout/storage for account preparation.
.EXAMPLE
.\scripts\Reset-RunnerPreparation.ps1 -WhatIf
.EXAMPLE
.\scripts\Reset-RunnerPreparation.ps1 -Config C:\OldCheckout\github-runners\local\controller.json
#>
[CmdletBinding(SupportsShouldProcess, ConfirmImpact='High')]
param(
    [string]$Config = 'local/controller.json',
    [string]$StorageRoot
)
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Host.ps1')
. (Join-Path $PSScriptRoot 'RunnerAccount.ps1')
. (Join-Path $PSScriptRoot 'PreparationReset.ps1')

if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') { throw 'Run this script on Windows x64 with PowerShell 7.4 or newer.' }
if (-not (Test-RunnerAdministrator)) { throw 'Open PowerShell 7 as administrator, then run scripts/Reset-RunnerPreparation.ps1.' }
Import-Module Hyper-V -ErrorAction Stop
$plan = Get-RunnerPreparationResetPlan $Config $StorageRoot
Write-Host "Configuration: $($plan.Config)"
if ($plan.Switch) { Write-Host "Remove switch: $($plan.Switch.Name) ($($plan.Switch.Id))" }
if ($plan.Nat) { Write-Host "Remove NAT: $($plan.Nat.Name) ($($plan.Nat.InternalIPInterfaceAddressPrefix))" }
foreach ($file in $plan.Files) { Write-Host "Remove record: $file" }
if (-not $plan.Switch -and -not $plan.Nat -and -not $plan.Files.Count) { Write-Host 'No preparation resources found.'; return }
if (-not $PSCmdlet.ShouldProcess($plan.Config, 'Remove unused runner switch/NAT and preparation records')) { return }

$managementLock = $null
$networkLock = $null
$accountSetupLock = $null
try {
    $accountSetupLock = Enter-RunnerFileLock (Join-Path $plan.Root 'bin/account-setup.lock')
    $managementLock = Enter-RunnerFileLock (Join-Path $plan.Root 'bin/management.lock')
    $networkLock = Enter-RunnerFileLock (Join-Path $plan.Data 'network.json.lock')
    $current = Get-RunnerPreparationResetPlan $Config $StorageRoot
    if ($current.Identity -cne $plan.Identity) { throw 'Preparation changed after preview. Rerun reset to review the current resources; nothing was removed.' }
    Remove-RunnerPreparation $current
    Write-Host 'Runner preparation reset. Run scripts/Prepare-RunnerAccount.ps1 from the intended checkout to start again.'
} finally {
    if ($networkLock) { $networkLock.Dispose() }
    if ($managementLock) { $managementLock.Dispose() }
    if ($accountSetupLock) { $accountSetupLock.Dispose() }
}
