#requires -Version 7.4
<#
.SYNOPSIS
Prepare a non-admin local controller account from an administrator session.
.DESCRIPTION
For a fresh installation, prepares host dependencies/networking, Hyper-V membership,
batch-logon rights and checkout/storage access. Finish PAT and startup setup after
signing in as the runner account. Supports -WhatIf without prompting for a password.
.EXAMPLE
.\scripts\Prepare-RunnerAccount.ps1 -UserName github-runners -CreateLocalUser
.EXAMPLE
.\scripts\Prepare-RunnerAccount.ps1 -UserName github-runners -StorageRoot D:\CLC-Runners -WhatIf
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)][ValidateNotNullOrEmpty()][string]$UserName,
    [switch]$CreateLocalUser,
    [Security.SecureString]$Password,
    [string]$Config = 'local/controller.json',
    [string]$StorageRoot
)
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Host.ps1')
. (Join-Path $PSScriptRoot 'RunnerAccount.ps1')

if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') { throw 'Run this script on Windows x64 with PowerShell 7.4 or newer.' }
if (-not (Test-RunnerAdministrator)) { throw 'Open PowerShell 7 as administrator, then run scripts/Prepare-RunnerAccount.ps1 with the intended runner user name.' }
$UserName = ConvertTo-RunnerLocalUserName $UserName
$account = @(Get-LocalUser | Where-Object Name -EQ $UserName)
if (-not $account.Count -and -not $CreateLocalUser) { throw "Local account '$UserName' does not exist. Supply -CreateLocalUser to create it with a password prompt." }
if ($account.Count) {
    $account = $account[0]
    if (-not $account.Enabled) { throw 'The runner account is disabled. Enable the intended account explicitly before preparation.' }
    if ($Password) { throw '-Password is only for creating a missing local user; existing passwords are never reset.' }
} else { $account = $null }
$plan = Get-RunnerAccountPlan $Config $StorageRoot
if (-not ('CLCRunners.AccountPolicy' -as [type])) { Add-Type -Path (Join-Path $PSScriptRoot 'RunnerAccountPolicy.cs') }
if ($account) {
    Assert-RunnerAccountPolicy $account.SID @(Get-RunnerLocalGroupSids $account.SID)
    if ($plan.Receipt -and $plan.Receipt.accountSid -ne $account.SID.Value) { throw 'This storage was prepared for another account. Use fresh storage.' }
} elseif ($plan.Receipt) { throw 'This storage was prepared for another account. Use fresh storage.' }

Write-Host "Account: $env:COMPUTERNAME\$UserName"
Write-Host "Checkout: $($plan.Root)"
Write-Host "Configuration: $($plan.Config)"
Write-Host ('Protected directories: ' + ($plan.Directories -join ', '))
if (-not $PSCmdlet.ShouldProcess("$env:COMPUTERNAME\$UserName", 'Prepare local account, batch logon, Hyper-V host/network/tools and checkout/storage permissions')) { return }

$lock = Enter-RunnerFileLock (Join-Path $plan.Root 'bin/management.lock')
try {
    # Repeat the state checks after acquiring the same lock used by controller setup.
    $plan = Get-RunnerAccountPlan $Config $StorageRoot
    if (-not $account) {
        if (-not $Password) { $Password = Read-Host "Windows password for $env:COMPUTERNAME\$UserName" -AsSecureString }
        if (-not $Password.Length) { throw 'A nonempty Windows password is required for unattended startup.' }
        $account = New-LocalUser -Name $UserName -Password $Password -Description 'CLC GitHub Actions runner controller'
        $Password = $null
    }
    Assert-RunnerAccountPolicy $account.SID @(Get-RunnerLocalGroupSids $account.SID)
    Add-RunnerLocalGroupMember 'S-1-5-32-545' $account.SID # Users: ordinary local sign-in.
    Grant-RunnerBatchLogon $account.SID.Value
    if ('SeBatchLogonRight' -notin @(Get-RunnerPolicyRights $account.SID.Value)) { throw 'Batch-logon permission did not persist. Check local/domain security policy.' }
    Set-RunnerAccountDirectory $plan.Root $account.SID -Repository
    foreach ($directory in $plan.Directories) { Set-RunnerAccountDirectory $directory $account.SID }
    Set-RunnerCheckoutOwner $plan.Root $account.SID
    if (-not $plan.Existing) { Write-RunnerJson $plan.Config $plan.Settings }
    Write-RunnerJson (Join-Path $plan.Data 'account-preparation.json') @{ version=1; accountSid=$account.SID.Value; account="$env:COMPUTERNAME\$UserName"; config=$plan.Config }
    Initialize-RunnerHost -Config $plan.Config -AccountSid $account.SID
    Write-Host ''
    Write-Host "Prepared $env:COMPUTERNAME\$UserName. Sign out and sign in as that account to refresh group membership and create its profile."
    Write-Host 'Then open normal PowerShell 7 and run:'
    Write-Host ("Set-Location '" + $plan.Root.Replace("'", "''") + "'")
    Write-Host (".\runners.ps1 Activate -Config '" + $plan.Config.Replace("'", "''") + "' -StartupMode Password")
    Write-Host 'Activation prompts for the GitHub PAT and Windows password, then validates Hyper-V/DPAPI under this account.'
    Write-Host 'To finish from this administrator session without signing in as the runner account, instead run:'
    Write-Host (".\scripts\Setup-RunnerAccount.ps1 -UserName '" + $UserName.Replace("'", "''") + "' -Config '" + $plan.Config.Replace("'", "''") + "'")
} finally {
    $Password = $null
    $lock.Dispose()
}
