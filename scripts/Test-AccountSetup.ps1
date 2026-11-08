#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Host.ps1')
. (Join-Path $PSScriptRoot 'RunnerAccount.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
. (Join-Path $PSScriptRoot 'AccountSetup.ps1')
$repository=$script:RunnerRoot
$scratch=Join-Path ([IO.Path]::GetTempPath()) ('clc-account-setup-'+[guid]::NewGuid().ToString('N'))
$workspaceConfig=Join-Path $repository 'local/controller.json'
$workspaceConfigHash=if (Test-Path -LiteralPath $workspaceConfig) { (Get-FileHash -LiteralPath $workspaceConfig).Hash } else { '' }
$locationPushed=$false
$checks=@{Count=0}
function Assert-AccountSetup($Condition,[string]$Message) {
    $checks.Count++
    if (-not $Condition) { throw $Message }
}
function Assert-AccountSetupFailure([scriptblock]$Action,[string]$Pattern) {
    $message=''
    try { & $Action *> $null } catch { $message=$_.Exception.Message }
    Assert-AccountSetup ($message -match $Pattern) "Expected '$Pattern', got '$message'"
}
try {
    Protect-RunnerDirectory $scratch
    Push-Location $scratch
    $locationPushed=$true
    $secret='Synthetic Windows password $` " ü !42'
    $pat='github_pat_SYNTHETIC_ACCOUNT_SETUP_TEST'
    $credential=[PSCredential]::new("$env:COMPUTERNAME\github-runners",(ConvertTo-SecureString $secret -AsPlainText -Force))
    $token=ConvertTo-SecureString $pat -AsPlainText -Force
    $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User
    $otherSid='S-1-5-21-101-102-103-1001'
    $canonical=Get-RunnerAccountSetupCredential github-runners $credential
    Assert-AccountSetup ($canonical.UserName -eq "$env:COMPUTERNAME\github-runners") 'Setup credential lost its local machine qualifier'
    $wrong=[PSCredential]::new('DOMAIN\github-runners',$credential.Password)
    Assert-AccountSetupFailure { Get-RunnerAccountSetupCredential github-runners $wrong } 'local user name'
    $wrong=[PSCredential]::new('.\another-user',$credential.Password)
    Assert-AccountSetupFailure { Get-RunnerAccountSetupCredential github-runners $wrong } 'requested local runner account'
    $empty=[PSCredential]::new('.\github-runners',[Security.SecureString]::new())
    Assert-AccountSetupFailure { Get-RunnerAccountSetupCredential github-runners $empty } 'nonempty Windows password'

    & {
        # Exercise real process pipes and the real worker in a child pwsh. Only
        # Windows account switching, host provisioning and Scheduler are mocked.
        $shim=Join-Path $scratch "checkout with spaces and 'quotes'"
        $scripts=Join-Path $shim 'scripts'
        $null=New-Item -ItemType Directory -Path $scripts,(Join-Path $shim 'config'),(Join-Path $shim '.git') -Force
        Copy-Item -LiteralPath (Join-Path $repository 'scripts/Invoke-AccountSetup.ps1'),(Join-Path $repository 'scripts/Set-RunnerToken.ps1') -Destination $scripts
        Copy-Item -LiteralPath (Join-Path $repository 'config/controller.example.json') -Destination (Join-Path $shim 'config/controller.example.json')
        [IO.File]::WriteAllText((Join-Path $scripts 'Common.ps1'),(". '"+$repository.Replace("'","''")+"/scripts/Common.ps1'`n`$script:RunnerRoot=Split-Path `$PSScriptRoot -Parent`n"))
        [IO.File]::WriteAllText((Join-Path $scripts 'Host.ps1'),'function Test-RunnerAdministrator { $false }')
        foreach ($name in @('Dependencies.ps1','Startup.ps1')) { [IO.File]::WriteAllText((Join-Path $scripts $name),'# Fixture: no external operations.') }
        [IO.File]::WriteAllText((Join-Path $scripts 'Activate.ps1'),@'
function Invoke-RunnerActivation {
    param($Config,$StartupMode,$StartupCredential,$TokenProvider,[switch]$RefreshAuthentication,$PasswordTaskRegistrar,[switch]$ForceRebuild,[switch]$NoCache)
    if ($StartupMode -ne 'Password' -or $StartupCredential.UserName -ne [Security.Principal.WindowsIdentity]::GetCurrent().Name) { throw 'Worker lost its target identity or startup mode' }
    if ($NoCache) { [Console]::Out.WriteLine('CLC-ACCOUNT-SETUP:'+$script:AccountSetupChannel+':UNKNOWN'); return }
    $token=& $TokenProvider
    try { & (Join-Path $PSScriptRoot 'Set-RunnerToken.ps1') -Config $Config -Token $token }
    finally { $token.Dispose() }
    $local=Get-RunnerSettings $Config
    $plain=[Security.Cryptography.ProtectedData]::Unprotect([IO.File]::ReadAllBytes($local.Settings.controller.patFile),$null,[Security.Cryptography.DataProtectionScope]::CurrentUser)
    try { $text=[Text.Encoding]::UTF8.GetString($plain); if ($text -ne 'github_pat_SYNTHETIC_ACCOUNT_SETUP_TEST') { throw 'PAT did not survive the pipe and CurrentUser DPAPI' } }
    finally { [Array]::Clear($plain) }
    if ($StartupCredential.GetNetworkCredential().Password -cne 'Synthetic Windows password $` " ü !42') { throw 'Windows password was changed by pipe encoding' }
    if ($ForceRebuild) {
        Write-Host ($StartupCredential.GetNetworkCredential().Password+' '+$text)
        [Console]::Error.WriteLine($StartupCredential.GetNetworkCredential().Password+' '+$text)
        throw ('Synthetic failure: '+$StartupCredential.GetNetworkCredential().Password+' '+$text)
    }
    & $PasswordTaskRegistrar
    Write-Host 'Worker verified identity, pipes and DPAPI.'
}
'@)
        $script:RunnerRoot=$shim
        $configPath=Join-Path $shim 'local/controller.json'
        $local=Initialize-RunnerConfig $configPath
        $testState=@{Registrations=0;RegistrationFails=$false;Prompts=0}
        function Start-RunnerAccountSetupProcess {
            param($Info)
            Assert-AccountSetup ($Info.LoadUserProfile -and -not $Info.UseShellExecute -and $Info.CreateNoWindow -and $Info.WindowStyle -eq 'Hidden') 'Worker did not request a loaded user profile in a hidden process'
            Assert-AccountSetup ($Info.RedirectStandardInput -and $Info.RedirectStandardOutput -and $Info.RedirectStandardError) 'Setup did not use process pipes'
            Assert-AccountSetup ($Info.UserName -eq 'github-runners' -and $Info.Domain -eq $env:COMPUTERNAME) 'Worker was not configured for the requested local account'
            Assert-AccountSetup (-not ($Info.ArgumentList -join ' ').Contains($secret) -and -not ($Info.ArgumentList -join ' ').Contains($pat)) 'Process arguments exposed a secret'
            Assert-AccountSetup ($Info.ArgumentList.Contains($configPath)) 'Config path with spaces and quotes was not passed as one argument'
            # Execute in this test account instead of creating/logging on a user.
            $Info.UserName='';$Info.Domain='';$Info.Password=$null
            [Diagnostics.Process]::Start($Info)
        }
        function Register-RunnerAccountStartup {
            param($Config,$AccountSid,$Credential)
            Assert-AccountSetup ($Config -eq $configPath -and $AccountSid -eq $sid.Value -and $Credential.UserName -eq "$env:COMPUTERNAME\github-runners") 'Startup request lost its parent-selected identity/configuration'
            $testState.Registrations++
            if ($testState.RegistrationFails) { throw ('Synthetic registration failure: '+$Credential.GetNetworkCredential().Password) }
        }
        function Read-Host { param($Prompt,[switch]$AsSecureString) $testState.Prompts++;ConvertTo-SecureString $pat -AsPlainText -Force }
        $output=Invoke-RunnerAccountSetup $configPath $sid.Value $credential -Token $token 6>&1 | Out-String
        Assert-AccountSetup ($output.Contains('Worker verified identity, pipes and DPAPI.') -and $testState.Registrations -eq 1) 'Actual child worker did not finish the request/response flow'
        Assert-AccountSetup (-not $output.Contains($secret) -and -not $output.Contains($pat)) 'Successful setup output exposed a secret'
        Assert-AccountSetup ($testState.Prompts -eq 0) 'Supplied PAT still prompted'
        Invoke-RunnerAccountSetup $configPath $sid.Value $credential *> $null
        Assert-AccountSetup ($testState.Prompts -eq 1 -and $testState.Registrations -eq 2) 'Administrator-side secure PAT prompt was not used'
        $failedOutput=[Collections.Generic.List[string]]::new()
        try { Invoke-RunnerAccountSetup $configPath $sid.Value $credential -Token $token -ForceRebuild 6>&1 | ForEach-Object { $failedOutput.Add([string]$_) } }
        catch { $failedOutput.Add($_.Exception.Message) }
        $text=$failedOutput -join "`n"
        Assert-AccountSetup ($text.Contains('Runner account setup failed') -and $text.Contains('[REDACTED]') -and -not $text.Contains($secret) -and -not $text.Contains($pat)) 'Failure output/exception did not redact both secrets'
        Assert-AccountSetup ($testState.Registrations -eq 2) 'Failed activation registered startup'
        $testState.RegistrationFails=$true
        $registrationOutput=[Collections.Generic.List[string]]::new()
        try { Invoke-RunnerAccountSetup $configPath $sid.Value $credential -Token $token 6>&1 | ForEach-Object { $registrationOutput.Add([string]$_) } }
        catch { $registrationOutput.Add($_.Exception.Message) }
        $text=$registrationOutput -join "`n"
        Assert-AccountSetup ($text.Contains('Administrator startup registration failed') -and -not $text.Contains($secret)) 'Parent registration failure was not returned safely to the worker'
        Assert-AccountSetupFailure { Invoke-RunnerAccountSetup $configPath $sid.Value $credential -Token $token -NoCache } 'Unknown account setup request'
        Assert-AccountSetupFailure { Invoke-RunnerAccountSetup $configPath $otherSid $credential -Token $token } 'Runner account setup failed'

        # Resume uses a non-secret receipt outside protected runtime storage.
        $receiptPath=Join-Path $shim 'bin/account-setup.json'
        Write-RunnerJson $receiptPath @{version=1;root=$shim;config=$configPath;accountSid=$sid.Value;storageRoot=$local.Settings.storageRoot}
        $account=[pscustomobject]@{SID=$sid}
        $plan=Get-RunnerAccountSetupPlan $configPath '' $account
        Assert-AccountSetup ($plan.Prepared -and $plan.Config -eq $configPath) 'Partial activation with an existing DPAPI PAT could not be resumed'
        Assert-AccountSetup (-not (Get-Content -LiteralPath $receiptPath -Raw).Contains($secret) -and -not (Get-Content -LiteralPath $receiptPath -Raw).Contains($pat)) 'Setup receipt contained a secret'
        Assert-AccountSetupFailure { Get-RunnerAccountSetupPlan $configPath '' ([pscustomobject]@{SID=[Security.Principal.SecurityIdentifier]::new($otherSid)}) } 'another account/checkout/configuration'
        Assert-AccountSetupFailure { Get-RunnerAccountSetupPlan 'another/controller.json' '' $account } 'another account/checkout/configuration'
        Assert-AccountSetupFailure { Get-RunnerAccountSetupPlan $configPath (Join-Path $scratch 'different-storage') $account } 'cannot relocate'
        $config=Read-RunnerJson $configPath
        $config.controller.startupRunLevel='Highest';Write-RunnerJson $configPath $config
        Assert-AccountSetupFailure { Get-RunnerAccountSetupPlan $configPath '' $account } 'startupRunLevel to be Limited'
    }
    & {
        # Use native task-definition objects, but intercept registration/COM so
        # no task or machine credential is created by this test.
        $script:RunnerRoot=$repository
        $parentTest=@{Task=$null;Registrations=0;Fail=$false}
        $parentCredential=[PSCredential]::new([Security.Principal.WindowsIdentity]::GetCurrent().Name,$credential.Password)
        $parentConfig=Join-Path $scratch 'broker/controller.json'
        $registered=[pscustomobject]@{Descriptor='';Flags=-1}
        $registered | Add-Member -MemberType ScriptMethod -Name SetSecurityDescriptor -Value { param($Descriptor,$Flags) $this.Descriptor=$Descriptor;$this.Flags=$Flags }
        $folder=[pscustomobject]@{Registered=$registered}
        $folder | Add-Member -MemberType ScriptMethod -Name GetTask -Value { param($Name) $this.Registered }
        $scheduler=[pscustomobject]@{Folder=$folder}
        $scheduler | Add-Member -MemberType ScriptMethod -Name Connect -Value {}
        $scheduler | Add-Member -MemberType ScriptMethod -Name GetFolder -Value { param($Path) $this.Folder }
        function Get-RunnerStartupTask { $parentTest.Task }
        function Register-ScheduledTask {
            [CmdletBinding()]param($TaskName,$InputObject,$User,$Password,[switch]$Force)
            Assert-AccountSetup ($TaskName -eq 'CLC-Linux-Runners' -and $Force -and $User -eq $parentCredential.UserName -and $Password -ceq $secret) 'Parent registration used the wrong task or credential'
            if ($parentTest.Fail) { throw ('Synthetic Scheduler failure: '+$Password) }
            $parentTest.Task=$InputObject;$parentTest.Registrations++
        }
        function New-Object { param($ComObject) Assert-AccountSetup ($ComObject -eq 'Schedule.Service') 'Unexpected COM operation';$scheduler }
        Register-RunnerAccountStartup $parentConfig $sid.Value $parentCredential
        $task=$parentTest.Task
        Assert-AccountSetup ($task.Principal.RunLevel -eq 'Limited' -and $task.Principal.LogonType -eq 'Password') 'Administrator registered an elevated or interactive principal'
        Assert-AccountSetup ($task.Actions[0].Execute -eq (Get-RunnerBinary) -and $task.Actions[0].Arguments -eq ('run --quiet --config "{0}"' -f $parentConfig) -and $task.Actions[0].WorkingDirectory -eq $repository) 'Administrator did not construct the fixed controller action'
        $descriptor=[Security.AccessControl.RawSecurityDescriptor]::new($registered.Descriptor)
        $taskSids=@($descriptor.DiscretionaryAcl | ForEach-Object { $_.SecurityIdentifier.Value })
        Assert-AccountSetup ($taskSids.Count -eq 3 -and $sid.Value -in $taskSids -and 'S-1-5-18' -in $taskSids -and 'S-1-5-32-544' -in $taskSids -and $registered.Flags -eq 0) 'Runner task ACL did not grant access only to the runner, SYSTEM and administrators'
        $parentTest.Task=[pscustomobject]@{Description='Another installation';Principal=[pscustomobject]@{UserId=$sid.Value}}
        Assert-AccountSetupFailure { Register-RunnerAccountStartup $parentConfig $sid.Value $parentCredential } 'another account/configuration'
        $parentTest.Task=[pscustomobject]@{Description=('CLC Linux Hyper-V runner controller; '+$parentConfig);Principal=[pscustomobject]@{UserId='S-1-5-18'}}
        Assert-AccountSetupFailure { Register-RunnerAccountStartup $parentConfig $sid.Value $parentCredential } 'another account/configuration'
        Assert-AccountSetup ($parentTest.Registrations -eq 1) 'Foreign startup task was overwritten'
        $parentTest.Task=$null;$parentTest.Fail=$true
        $message=''
        try { Register-RunnerAccountStartup $parentConfig $sid.Value $parentCredential } catch { $message=$_.Exception.Message }
        Assert-AccountSetup ($message.Contains('[REDACTED]') -and -not $message.Contains($secret)) 'Administrator Scheduler error leaked the Windows password'
    }
    & {
        # WhatIf validates the existing preparation plan without credentials,
        # files, accounts, processes, policies or startup registration.
        $shim=Join-Path $scratch 'entry'
        $scripts=Join-Path $shim 'scripts'
        $null=New-Item -ItemType Directory -Path $scripts -Force
        Copy-Item -LiteralPath (Join-Path $repository 'scripts/Setup-RunnerAccount.ps1'),(Join-Path $repository 'scripts/Common.ps1') -Destination $scripts
        [IO.File]::WriteAllText((Join-Path $scripts 'Host.ps1'),@'
function Test-RunnerAdministrator { $entryState.Admin }
function Get-LocalUser { [CmdletBinding()]param($Name) if ($entryState.Exists) { [pscustomobject]@{SID=$sid;Enabled=$true} } }
'@)
        [IO.File]::WriteAllText((Join-Path $scripts 'RunnerAccount.ps1'),@'
function ConvertTo-RunnerLocalUserName($Name) { $Name }
function Assert-RunnerAccountPolicy { param($Sid,$Groups) }
function Get-RunnerLocalGroupSids { param($Sid) @() }
'@)
        [IO.File]::WriteAllText((Join-Path $scripts 'Startup.ps1'),'# No Scheduler calls allowed.')
        [IO.File]::WriteAllText((Join-Path $scripts 'AccountSetup.ps1'),@'
function Get-RunnerAccountSetupPlan { param($Config,$StorageRoot,$Account) [pscustomobject]@{Root=$shim;Config=(Join-Path $shim 'local/controller.json');Prepared=(Test-Path -LiteralPath (Join-Path $shim 'bin/account-setup.json'))} }
function Get-RunnerAccountSetupCredential { param($UserName,$Credential) if (-not $Credential) { throw 'Unexpected password prompt' }; $Credential }
function Invoke-RunnerAccountSetup {
    param($Config,$AccountSid,$Credential,$Token,[switch]$ForceRebuild,[switch]$NoCache)
    Assert-AccountSetup ($Config -eq (Join-Path $shim 'local/controller.json') -and $AccountSid -eq $sid.Value -and $Credential.UserName -eq "$env:COMPUTERNAME\github-runners") 'Entry lost the selected account/configuration'
    $entryState.Activations++
    if ($entryState.Fail) { throw 'Synthetic activation failure' }
}
'@)
        [IO.File]::WriteAllText((Join-Path $scripts 'Prepare-RunnerAccount.ps1'),@'
[CmdletBinding(SupportsShouldProcess)]param($UserName,[switch]$CreateLocalUser,$Password,$Config,$StorageRoot)
Assert-AccountSetup ($CreateLocalUser -and $Password.Length -gt 0) 'Creation did not reuse the prompted Windows password'
$fixtureRoot=Split-Path $PSScriptRoot -Parent
$Config=[IO.Path]::GetFullPath($Config,$fixtureRoot)
if (-not $Config.StartsWith($fixtureRoot+'\',[StringComparison]::OrdinalIgnoreCase)) { throw 'Fixture configuration escaped its scratch checkout' }
$entryState.Preparations++
$entryState.Exists=$true
$null=New-Item -ItemType Directory -Path (Split-Path $Config -Parent) -Force
Copy-Item -LiteralPath $entryState.Template -Destination $Config
'@)
        if (-not ('CLCRunners.AccountPolicy' -as [type])) { Add-Type -Path (Join-Path $repository 'scripts/RunnerAccountPolicy.cs') }
        $entryState=@{Admin=$true;Exists=$false;Preparations=0;Activations=0;Fail=$false;Template=(Join-Path $repository 'config/controller.example.json')}
        $entry=Join-Path $scripts 'Setup-RunnerAccount.ps1'
        & $entry -UserName github-runners -CreateLocalUser -WhatIf *> $null
        Assert-AccountSetup (-not (Test-Path -LiteralPath (Join-Path $shim 'bin')) -and -not (Test-Path -LiteralPath (Join-Path $shim 'local'))) 'WhatIf created setup files'
        $entryState.Admin=$false
        Assert-AccountSetupFailure { & $entry -UserName github-runners -CreateLocalUser } 'as administrator'
        $entryState.Admin=$true
        Assert-AccountSetupFailure { & $entry -UserName github-runners } 'does not exist'
        & $entry -UserName github-runners -CreateLocalUser -Credential $credential -Token $token *> $null
        Assert-AccountSetup ($entryState.Preparations -eq 1 -and $entryState.Activations -eq 1) 'Administrator entry did not prepare and activate a new account'
        $entryState.Fail=$true
        Assert-AccountSetupFailure { & $entry -UserName github-runners -Credential $credential } 'Synthetic activation failure'
        $entryState.Fail=$false
        & $entry -UserName github-runners -Credential $credential *> $null
        Assert-AccountSetup ($entryState.Preparations -eq 1 -and $entryState.Activations -eq 3) 'Retry repeated preparation or failed to resume activation'
    }
    Write-Host "Account setup: $($checks.Count) identity, profile, real process-pipe, DPAPI, resume and failure assertions passed (account switching/host/Scheduler mocked)."
} finally {
    $script:RunnerRoot=$repository
    if ($locationPushed) { Pop-Location }
    $absolute=[IO.Path]::GetFullPath($scratch)
    $prefix=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')+'\'
    if ($absolute.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf) -match '^clc-account-setup-[a-f0-9]{32}$') { Remove-Item -LiteralPath $absolute -Recurse -Force }
    $afterConfigHash=if (Test-Path -LiteralPath $workspaceConfig) { (Get-FileHash -LiteralPath $workspaceConfig).Hash } else { '' }
    if ($afterConfigHash -ne $workspaceConfigHash) { throw 'Account setup tests changed the workspace configuration.' }
}
