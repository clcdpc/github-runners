#requires -Version 7.4
<#
.SYNOPSIS
Prepare and activate a non-admin local runner account without signing in as it.
.DESCRIPTION
Run in elevated PowerShell 7.4+. Prompts for the local account's Windows password
and, when needed, its GitHub PAT. Windows loads the user's profile for a hidden
setup process; secrets travel over process pipes and the PAT is stored with that
user's CurrentUser DPAPI. The administrator registers Limited Password startup
after the user's noninteractive Hyper-V/DPAPI probe passes.
.EXAMPLE
.\scripts\Setup-RunnerAccount.ps1 -UserName github-runners -CreateLocalUser
.EXAMPLE
.\scripts\Setup-RunnerAccount.ps1 -UserName github-runners
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)][ValidateNotNullOrEmpty()][string]$UserName,
    [switch]$CreateLocalUser,
    [PSCredential]$Credential,
    [Security.SecureString]$Token,
    [string]$Config='local/controller.json',
    [string]$StorageRoot,
    [switch]$ForceRebuild,[switch]$NoCache
)
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Host.ps1')
. (Join-Path $PSScriptRoot 'RunnerAccount.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
. (Join-Path $PSScriptRoot 'AccountSetup.ps1')

if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') { throw 'Run this script on Windows x64 with PowerShell 7.4 or newer.' }
if (-not (Test-RunnerAdministrator)) { throw 'Open PowerShell 7 as administrator, then run scripts/Setup-RunnerAccount.ps1.' }
$UserName=ConvertTo-RunnerLocalUserName $UserName
$account=Get-LocalUser -Name $UserName -ErrorAction SilentlyContinue
if (-not $account -and -not $CreateLocalUser) { throw "Local account '$UserName' does not exist. Supply -CreateLocalUser to create it." }
if ($account -and -not $account.Enabled) { throw 'Enable the intended runner account explicitly before setup.' }
if (-not ('CLCRunners.AccountPolicy' -as [type])) { Add-Type -Path (Join-Path $PSScriptRoot 'RunnerAccountPolicy.cs') }
if ($account) { Assert-RunnerAccountPolicy $account.SID @(Get-RunnerLocalGroupSids $account.SID) }
$plan=Get-RunnerAccountSetupPlan $Config $StorageRoot $account
Write-Host "Account: $env:COMPUTERNAME\$UserName"
Write-Host "Checkout: $($plan.Root)"
Write-Host "Configuration: $($plan.Config)"
if (-not $PSCmdlet.ShouldProcess("$env:COMPUTERNAME\$UserName",'Prepare and activate runners, store user-protected PAT and register unattended startup')) { return }

$setupLock=Enter-RunnerFileLock (Join-Path $plan.Root 'bin/account-setup.lock')
try {
    $account=Get-LocalUser -Name $UserName -ErrorAction SilentlyContinue
    if ($account -and -not $account.Enabled) { throw 'Enable the intended runner account explicitly before setup.' }
    $plan=Get-RunnerAccountSetupPlan $Config $StorageRoot $account
    $Credential=Get-RunnerAccountSetupCredential $UserName $Credential
    if (-not $plan.Prepared) {
        $prepare=@{UserName=$UserName;Config=$plan.Config;CreateLocalUser=$CreateLocalUser;Confirm=$false}
        if ($StorageRoot) { $prepare.StorageRoot=$StorageRoot }
        if (-not $account) { $prepare.Password=$Credential.Password }
        & (Join-Path $PSScriptRoot 'Prepare-RunnerAccount.ps1') @prepare
        $account=Get-LocalUser -Name $UserName -ErrorAction Stop
        $local=Get-RunnerSettings $plan.Config
        Write-RunnerJson (Join-Path $plan.Root 'bin/account-setup.json') @{version=1;root=$plan.Root;config=$local.Path;accountSid=$account.SID.Value;storageRoot=$local.Settings.storageRoot}
    }
    Assert-RunnerAccountPolicy $account.SID @(Get-RunnerLocalGroupSids $account.SID)
    Write-Host 'Activating under the runner account; Windows loads its profile without an interactive sign-in.'
    Invoke-RunnerAccountSetup -Config $plan.Config -AccountSid $account.SID.Value -Credential $Credential -Token $Token -ForceRebuild:$ForceRebuild -NoCache:$NoCache
} finally {
    $Credential=$null
    $Token=$null
    $setupLock.Dispose()
}
