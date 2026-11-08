#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Host.ps1')
. (Join-Path $PSScriptRoot 'RunnerAccount.ps1')
if (-not ('CLCRunners.AccountPolicy' -as [type])) { Add-Type -Path (Join-Path $PSScriptRoot 'RunnerAccountPolicy.cs') }
$repository = $script:RunnerRoot
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('clc-runner-account-'+[guid]::NewGuid().ToString('N'))
$accountChecks = @{ Count=0 }
function Assert-Account($Condition, [string]$Message) {
    $accountChecks.Count++
    if (-not $Condition) { throw $Message }
}
function Assert-AccountFailure([scriptblock]$Action, [string]$Pattern) {
    $message = ''
    try { & $Action | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Account ($message -match $Pattern) "Expected '$Pattern', got '$message'"
}
try {
    $null = New-Item -ItemType Directory -Path $scratch
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $targetSid = [Security.Principal.SecurityIdentifier]::new('S-1-5-21-101-102-103-1001')
    Assert-Account ((ConvertTo-RunnerLocalUserName '.\github-runners') -eq 'github-runners') 'Local name was not normalized'
    Assert-Account ((ConvertTo-RunnerLocalUserName "$env:COMPUTERNAME\github-runners") -eq 'github-runners') 'Qualified local name was not normalized'
    foreach ($name in @('DOMAIN\runner','runner@example.com','..','bad/name','runner ','123456789012345678901')) {
        Assert-AccountFailure { ConvertTo-RunnerLocalUserName $name } 'local user name'
    }
    foreach ($path in @([IO.Path]::GetPathRoot($scratch), $env:USERPROFILE, (Split-Path $env:USERPROFILE -Parent), (Join-Path $env:SystemRoot 'Temp'), '\\server\share\runners')) {
        Assert-AccountFailure { Assert-RunnerAccountPath $path } 'broad directory|outside Windows|local absolute'
    }
    & {
        $script:RunnerRoot = Join-Path $scratch 'checkout'
        $null = New-Item -ItemType Directory -Path (Join-Path $script:RunnerRoot 'config') -Force
        Copy-Item -LiteralPath (Join-Path $repository 'config/controller.example.json') -Destination (Join-Path $script:RunnerRoot 'config/controller.example.json')
        function Get-RunnerControllerProcesses { @() }
        function Get-ScheduledTask { [CmdletBinding()]param($TaskName) if ($taskPresent) { [pscustomobject]@{ TaskName=$TaskName } } }
        $taskPresent = $false
        $plan = Get-RunnerAccountPlan 'local/controller.json'
        Assert-Account (-not $plan.Existing -and -not (Test-Path -LiteralPath $plan.Config)) 'Planning created a configuration'
        Assert-Account ($plan.Data -eq (Join-Path $script:RunnerRoot 'local/data')) 'Default storage paths changed'
        Assert-Account ($plan.Settings.hyperv.script -eq (Join-Path $script:RunnerRoot 'scripts/HyperV.ps1')) 'External config would resolve host script incorrectly'
        $external = Join-Path $scratch 'storage'
        $externalPlan = Get-RunnerAccountPlan 'local/controller.json' $external
        Assert-Account ($externalPlan.Data -eq (Join-Path $external 'data') -and -not (Test-Path -LiteralPath $external)) 'Storage override was not planned without mutation'
        Assert-AccountFailure { Assert-RunnerAccountPath $script:RunnerRoot } 'must not contain the checkout'
        $null = New-Item -ItemType Directory -Path (Join-Path $script:RunnerRoot '.git')
        Set-RunnerAccountDirectory $script:RunnerRoot $sid -Repository
        Set-RunnerCheckoutOwner $script:RunnerRoot $sid
        Assert-Account ((Get-Acl -LiteralPath $script:RunnerRoot).GetOwner([Security.Principal.SecurityIdentifier]) -eq $sid -and (Get-Acl -LiteralPath (Join-Path $script:RunnerRoot '.git')).GetOwner([Security.Principal.SecurityIdentifier]) -eq $sid) 'Checkout ownership was not handed to the requested SID'
        foreach ($directory in $plan.Directories) { Set-RunnerAccountDirectory $directory $sid }
        $child = Join-Path $plan.Data 'network.json'
        [IO.File]::WriteAllText($child, '{}')
        Set-RunnerAccountDirectory $plan.Data $sid
        Set-RunnerAccountDirectory $plan.Data $sid
        $acl = Get-Acl -LiteralPath $plan.Data
        $rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
        Assert-Account ($acl.AreAccessRulesProtected -and $rules.Count -eq 3) 'Preparation ACL was not stable and restricted'
        foreach ($rule in $rules) {
            Assert-Account ($rule.IdentityReference.Value -in @($sid.Value, 'S-1-5-18','S-1-5-32-544') -and $rule.FileSystemRights.HasFlag([Security.AccessControl.FileSystemRights]::FullControl)) 'Unexpected preparation ACL'
        }
        $childAcl = Get-Acl -LiteralPath $child
        Assert-Account (@($childAcl.GetAccessRules($true,$true,[Security.Principal.SecurityIdentifier]) | Where-Object { $_.IdentityReference.Value -eq $sid.Value -and $_.IsInherited }).Count -eq 1) 'Host-created files did not inherit account access'
        Write-RunnerJson $plan.Config $plan.Settings
        $plan = Get-RunnerAccountPlan 'local/controller.json'
        Assert-Account $plan.Existing 'Preparation could not be retried with an existing fresh config'
        $hash = (Get-FileHash -LiteralPath $plan.Config).Hash
        Assert-AccountFailure { Get-RunnerAccountPlan 'local/controller.json' $external } 'cannot relocate'
        Assert-Account ((Get-FileHash -LiteralPath $plan.Config).Hash -eq $hash) 'Rejected storage override changed configuration'
        $markers = @(($plan.Config+'.effective.json'), (Join-Path $plan.Data 'state.json'), (Join-Path $plan.Data 'status.json'), (Join-Path (Split-Path $plan.Data -Parent) 'secrets/github-pat.dpapi'))
        foreach ($marker in $markers) {
            [IO.File]::WriteAllText($marker, '{}')
            Assert-AccountFailure { Get-RunnerAccountPlan 'local/controller.json' } 'credentials or controller state|saved credentials'
            Remove-Item -LiteralPath $marker
        }
        $foreign = Join-Path $plan.Data 'foreign.txt'
        [IO.File]::WriteAllText($foreign, 'preserve')
        Assert-AccountFailure { Get-RunnerAccountPlan 'local/controller.json' } 'existing runtime files'
        Assert-Account ([IO.File]::ReadAllText($foreign) -eq 'preserve') 'Foreign file changed'
        Remove-Item -LiteralPath $foreign
        $otherPat = Join-Path $script:RunnerRoot 'other-account.dpapi'
        [IO.File]::WriteAllText($otherPat, 'preserve')
        Assert-AccountFailure { Get-RunnerAccountPlan 'different/controller.json' } 'saved credentials'
        Remove-Item -LiteralPath $otherPat
        $taskPresent = $true
        Assert-AccountFailure { Get-RunnerAccountPlan 'local/controller.json' } 'startup task already exists'
        $taskPresent = $false
        Write-RunnerJson (Join-Path $plan.Data 'account-preparation.json') @{ version=1; accountSid=$targetSid.Value; config='C:\another\controller.json' }
        Assert-AccountFailure { Get-RunnerAccountPlan 'local/controller.json' } 'different configuration'
        Remove-Item -LiteralPath (Join-Path $plan.Data 'account-preparation.json')
        $link = Join-Path $plan.Data 'outside'
        try {
            $null = New-Item -ItemType Junction -Path $link -Target $externalPlan.Root
            Assert-AccountFailure { Get-RunnerAccountPlan 'local/controller.json' } 'Reparse point'
        } finally { if (Test-Path -LiteralPath $link) { Remove-Item -LiteralPath $link -Force } }
    }
    & {
        $policy = @{}
        function Get-RunnerPolicyRights([string]$Sid) { if ($policy.ContainsKey($Sid)) { $policy[$Sid] } }
        Assert-RunnerAccountPolicy $targetSid @()
        Assert-AccountFailure { Assert-RunnerAccountPolicy $targetSid @('S-1-5-32-544') } 'member of Administrators'
        foreach ($subject in @($targetSid.Value, 'S-1-5-32-545','S-1-5-32-578','S-1-1-0','S-1-5-113','S-1-5-3')) {
            $policy[$subject] = @('SeDenyBatchLogonRight')
            Assert-AccountFailure { Assert-RunnerAccountPolicy $targetSid @() } 'denied batch/interactive logon'
            $policy.Clear()
        }
        $policy['S-1-5-4'] = @('SeDenyInteractiveLogonRight')
        Assert-AccountFailure { Assert-RunnerAccountPolicy $targetSid @() } 'denied batch/interactive logon'
    }
    & {
        $hostState = @{ Members=@(); Added=@(); Steps=@() }
        function Test-RunnerAdministrator { $true }
        function Get-ItemProperty { param($Path) [pscustomobject]@{ EditionID='Professional' } }
        function Get-CimInstance { param($Class) [pscustomobject]@{ ProductType=1 } }
        function Get-WindowsOptionalFeature { param([switch]$Online,$FeatureName) [pscustomobject]@{ State='Enabled' } }
        function Import-Module { param($Name) }
        function Get-Service { param($Name) [pscustomobject]@{ Status='Running' } }
        function Get-LocalGroup { param($SID) 'hyperv' }
        function Get-LocalGroupMember { param($Group) foreach ($value in $hostState.Members) { [pscustomobject]@{ SID=$value } } }
        function Add-LocalGroupMember { param($Group,$Member) $hostState.Added += $Member; $hostState.Members += $Member }
        function Get-WindowsCapability { param([switch]$Online,$Name) [pscustomobject]@{ State='Installed' } }
        function Initialize-RunnerNetwork { param($Config) $hostState.Steps += 'network' }
        function Test-RunnerHost { param($Config) $hostState.Steps += 'host' }
        function Install-RunnerImageTools { param($Config) $hostState.Steps += 'tools' }
        Initialize-RunnerHost 'fixture' -AccountSid $targetSid
        Initialize-RunnerHost 'fixture' -AccountSid $targetSid
        Assert-Account ($hostState.Added.Count -eq 1 -and $hostState.Added[0] -eq $targetSid.Value) 'Host preparation added the admin account instead of the requested runner, or was not idempotent'
        Assert-Account (($hostState.Steps -join ',') -eq 'network,host,tools,network,host,tools') 'Host dependency/network preparation was skipped'
        Initialize-RunnerHost 'fixture'
        Assert-Account ($hostState.Added[-1] -eq $sid.Value) 'Existing PrepareHost stopped granting the current account'
    }
    & {
        # Run the real entry script with host/account shims; no users, policies or VMs are changed.
        $shim = Join-Path $scratch 'entry'
        $shimScripts = Join-Path $shim 'scripts'
        $null = New-Item -ItemType Directory -Path $shimScripts, (Join-Path $shim 'local/data') -Force
        Copy-Item -LiteralPath (Join-Path $repository 'scripts/Prepare-RunnerAccount.ps1'), (Join-Path $repository 'scripts/Common.ps1') -Destination $shimScripts
        $entryState = @{ Admin=$true; Exists=$false; Events=[Collections.Generic.List[string]]::new(); Sid=$sid; Batch=$false }
        $entryPlan = [pscustomobject]@{ Root=$shim; Config=(Join-Path $shim 'local/controller.json'); Existing=$false; Settings=@{ fixture=$true }; Directories=@((Join-Path $shim 'local')); Data=(Join-Path $shim 'local/data'); Receipt=$null }
        [IO.File]::WriteAllText((Join-Path $shimScripts 'Host.ps1'), @'
function Test-RunnerAdministrator { $entryState.Admin }
function Get-LocalUser { if ($entryState.Exists) { [pscustomobject]@{ Name='github-runners'; Enabled=$true; SID=$entryState.Sid } } }
function New-LocalUser { param($Name,$Password,$Description) $entryState.Events.Add('create'); $entryState.Exists=$true; [pscustomobject]@{ SID=$entryState.Sid } }
function Initialize-RunnerHost { param($Config,$AccountSid) if ($AccountSid -ne $entryState.Sid) { throw 'Wrong host account SID' }; $entryState.Events.Add('host') }
function Read-Host { throw 'Unexpected password prompt' }
'@)
        [IO.File]::WriteAllText((Join-Path $shimScripts 'RunnerAccount.ps1'), @'
function ConvertTo-RunnerLocalUserName($UserName) { $UserName }
function Get-RunnerAccountPlan { param($Config,$StorageRoot) $entryPlan }
function Get-RunnerLocalGroupSids { param($Sid) @() }
function Assert-RunnerAccountPolicy { param($Sid,$Groups) }
function Add-RunnerLocalGroupMember { param($GroupSid,$Sid) $entryState.Events.Add('group') }
function Grant-RunnerBatchLogon { param($Sid) $entryState.Events.Add('batch'); $entryState.Batch=$true }
function Get-RunnerPolicyRights { param($Sid) if ($entryState.Batch) { 'SeBatchLogonRight' } }
function Set-RunnerAccountDirectory { param($Path,$Sid,[switch]$Repository) $entryState.Events.Add('acl') }
function Set-RunnerCheckoutOwner { param($Path,$Sid) $entryState.Events.Add('owner') }
'@)
        $entry = Join-Path $shimScripts 'Prepare-RunnerAccount.ps1'
        & $entry -UserName github-runners -CreateLocalUser -WhatIf
        Assert-Account ($entryState.Events.Count -eq 0 -and -not (Test-Path -LiteralPath $entryPlan.Config) -and -not (Test-Path -LiteralPath (Join-Path $shim 'bin')) ) 'WhatIf created a user, files or host configuration'
        $entryState.Admin = $false
        Assert-AccountFailure { & $entry -UserName github-runners -CreateLocalUser } 'as administrator'
        $entryState.Admin = $true
        $secret = ConvertTo-SecureString 'Synthetic account password !42' -AsPlainText -Force
        & $entry -UserName github-runners -CreateLocalUser -Password $secret
        Assert-Account (($entryState.Events -join ',') -eq 'create,group,batch,acl,acl,owner,host') 'Account setup sequence or target identity changed'
        $configHash = (Get-FileHash -LiteralPath $entryPlan.Config).Hash
        $entryPlan.Existing = $true
        & $entry -UserName github-runners
        Assert-Account (@($entryState.Events | Where-Object { $_ -eq 'create' }).Count -eq 1 -and (Get-FileHash -LiteralPath $entryPlan.Config).Hash -eq $configHash) 'Retry recreated the user or rewrote configuration'
        $entryPlan.Receipt = @{ accountSid=$targetSid.Value }
        $before = $entryState.Events.Count
        Assert-AccountFailure { & $entry -UserName github-runners } 'another account'
        Assert-Account ($entryState.Events.Count -eq $before) 'An account mismatch changed permissions'
        Assert-AccountFailure { & $entry -UserName github-runners -Password $secret } 'existing passwords are never reset'
    }
    Write-Host "Runner account: $($accountChecks.Count) path, ACL, policy, identity, dry-run and setup assertions passed (mocked account/LSA/Hyper-V changes)."
} finally {
    $script:RunnerRoot = $repository
    $absolute = [IO.Path]::GetFullPath($scratch)
    $prefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')+'\'
    if ($absolute.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf) -match '^clc-runner-account-[a-f0-9]{32}$') { Remove-Item -LiteralPath $absolute -Recurse -Force }
}
