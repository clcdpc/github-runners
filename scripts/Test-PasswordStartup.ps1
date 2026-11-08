#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
. (Join-Path $PSScriptRoot 'Restart.ps1')
$repository=$script:RunnerRoot
$scratch=Join-Path ([IO.Path]::GetTempPath()) ('clc-password-startup-'+[guid]::NewGuid().ToString('N'))
$passwordCheckState=@{Count=0}
function Assert-Password($Condition,[string]$Message) {
    $passwordCheckState.Count++
    if (-not $Condition) { throw $Message }
}
function Assert-PasswordFailure([scriptblock]$Action,[string]$Pattern) {
    $message=''
    try { & $Action | Out-Null } catch { $message=$_.Exception.Message }
    Assert-Password ($message -match $Pattern) "Expected '$Pattern', got '$message'"
}
try {
    Protect-RunnerDirectory $scratch
    $configPath=Join-Path $scratch 'controller.json'
    $local=Initialize-RunnerConfig $configPath
    $config=Read-RunnerJson $configPath
    $config.hyperv.script=Join-Path $repository 'scripts/HyperV.ps1'
    Write-RunnerJson $configPath $config
    [IO.File]::WriteAllText($local.Settings.controller.patFile,'synthetic encrypted PAT fixture')
    $binary=Join-Path $scratch 'controller.exe';[IO.File]::WriteAllText($binary,'binary fixture')
    $identity=[Security.Principal.WindowsIdentity]::GetCurrent()
    $secret='Synthetic Windows credential !42'
    $credential=[PSCredential]::new($identity.Name,(ConvertTo-SecureString $secret -AsPlainText -Force))

    & {
        # Exercise the real startup lookup. An exact CIM query reports a
        # missing task/root as an error, unlike an empty mocked task list.
        $lookup=@{Mode='MissingPath';Name='';Path='';Task=[pscustomobject]@{TaskName='CLC-Linux-Runners';TaskPath='\'}}
        function Get-ScheduledTask {
            [CmdletBinding()]param([string[]]$TaskName,[string[]]$TaskPath)
            $lookup.Name=$TaskName -join ',';$lookup.Path=$TaskPath -join ','
            switch ($lookup.Mode) {
                'MissingPath' { Write-Error "No MSFT_ScheduledTask objects found with property 'TaskPath' equal to ''. Verify the value of the property and retry." -Category ObjectNotFound -ErrorId CmdletizationQuery_NotFound_TaskPath; return }
                'MissingName' { Write-Error 'No MSFT_ScheduledTask objects found with the requested TaskName.' -Category ObjectNotFound -ErrorId CmdletizationQuery_NotFound_TaskName; return }
                'Denied' { Write-Error 'Scheduler access denied' -Category PermissionDenied -ErrorId SchedulerAccessDenied; return }
                'Unavailable' { Write-Error 'Scheduler RPC unavailable' -Category ResourceUnavailable -ErrorId SchedulerUnavailable; return }
                'OtherNotFound' { Write-Error 'Scheduler class not found' -Category ObjectNotFound -ErrorId SchedulerClassNotFound; return }
            }
            $lookup.Task
        }
        Assert-Password ($null -eq (Get-RunnerStartupTask)) 'An empty root folder was not treated as missing startup'
        $lookup.Mode='MissingName'
        Assert-Password ($null -eq (Get-RunnerStartupTask)) 'A missing controller task was not treated as missing startup'
        $lookup.Mode='Found'
        Assert-Password ([object]::ReferenceEquals($lookup.Task,(Get-RunnerStartupTask))) 'Existing startup task was not returned'
        Assert-Password ($lookup.Name -eq 'CLC-Linux-Runners' -and $lookup.Path -eq '\') 'Startup lookup was not restricted to the controller task in the root folder'
        $lookup.Mode='Denied'
        Assert-PasswordFailure { Get-RunnerStartupTask } 'Scheduler access denied'
        $lookup.Mode='Unavailable'
        Assert-PasswordFailure { Get-RunnerStartupTask } 'Scheduler RPC unavailable'
        $lookup.Mode='OtherNotFound'
        Assert-PasswordFailure { Get-RunnerStartupTask } 'Scheduler class not found'
    }

    & {
        $script:simulatedSession=1;$script:simulatedBoot=[DateTime]'2026-11-01T00:00:00Z'
        $script:simulatedMachine='machine-one'
        function Get-Process { param($Id) [pscustomobject]@{SessionId=$script:simulatedSession} }
        function Get-CimInstance { param($ClassName) [pscustomobject]@{LastBootUpTime=$script:simulatedBoot} }
        function Get-ItemProperty { [CmdletBinding()]param($LiteralPath,$Name) [pscustomobject]@{MachineGuid=$script:simulatedMachine} }
        $passwordBefore=Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID fixture -Mode Password
        $interactiveBefore=Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID fixture
        $script:simulatedSession=0;$script:simulatedBoot=$script:simulatedBoot.AddDays(1)
        Assert-Password ($passwordBefore -eq (Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID fixture -Mode Password)) 'Password validation depended on interactive session or current boot'
        Assert-Password ($interactiveBefore -ne (Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID fixture)) 'Legacy session/boot validation was weakened'
        $config.controller.startupRunLevel='Highest';Write-RunnerJson $configPath $config
        Assert-Password ($passwordBefore -ne (Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID fixture -Mode Password)) 'Password validation ignored the configured elevation level'
        $config.controller.startupRunLevel='Limited';Write-RunnerJson $configPath $config
        $script:simulatedMachine='machine-two'
        Assert-Password ($passwordBefore -ne (Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID fixture -Mode Password)) 'Password validation could be reused on another machine'
        $script:simulatedMachine='machine-one'
        [IO.File]::WriteAllText($local.Settings.controller.patFile,'changed encrypted PAT fixture')
        Assert-Password ($passwordBefore -ne (Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID fixture -Mode Password)) 'Password validation ignored a replacement PAT'
        [IO.File]::WriteAllText($local.Settings.controller.patFile,'synthetic encrypted PAT fixture')
        [IO.File]::WriteAllText($binary,'changed binary')
        Assert-Password ($passwordBefore -ne (Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID fixture -Mode Password)) 'Password validation ignored executable changes'
        Assert-Password ($passwordBefore -ne (Get-RunnerStartupFingerprint $configPath -Binary $binary -ImageID another-image -Mode Password)) 'Password validation ignored image changes'
        [IO.File]::WriteAllText($binary,'binary fixture')
    }

    & {
        $script:startupTask=$null;$script:passwordFingerprint='validated-inputs';$script:passwordProbePass=$true
        $script:passwordRegistrations=0;$script:passwordProbes=0;$script:passwordStarts=0
        $script:passwordRegistrationError=$false;$script:passwordStartError=$false;$script:passwordEvents=[Collections.Generic.List[string]]::new()
        function Build-RunnerController { $binary }
        function Get-RunnerBinary { $binary }
        function Get-RunnerStartupTask { $script:startupTask }
        function Get-RunnerStartupFingerprint { param($Config,$Binary,$ImageID,$Mode) Assert-Password ($Mode -eq 'Password') 'Password report used a session-bound fingerprint';$script:passwordFingerprint }
        function Stop-RunnerController { param($Config,[switch]$Wait) Assert-Password $Wait 'Startup conversion did not wait for graceful draining';$script:passwordEvents.Add('drain') }
        function Test-RunnerUnattended {
            param($Config,$Mode,$Credential)
            Assert-Password ($Mode -eq 'Password' -and $Credential.UserName -eq $identity.Name) 'Password probe lost its selected identity/mode'
            $script:passwordProbes++;$script:passwordEvents.Add('probe')
            Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json') @{
                validationMode='Password';fingerprint=$script:passwordFingerprint
                modes=@{S4U=@{passed=$false};Interactive=@{passed=$false};Password=@{passed=$script:passwordProbePass}}
            }
        }
        function Get-Credential { throw 'An unexpected credential prompt was attempted' }
        function New-ScheduledTaskAction { [CmdletBinding()]param($Execute,$Argument,$WorkingDirectory) [pscustomobject]@{Execute=$Execute;Arguments=$Argument;WorkingDirectory=$WorkingDirectory} }
        function New-ScheduledTaskPrincipal { [CmdletBinding()]param($UserId,$LogonType,$RunLevel) [pscustomobject]@{UserId=$UserId;LogonType=$LogonType;RunLevel=$RunLevel} }
        function New-ScheduledTaskTrigger {
            [CmdletBinding()]param([switch]$AtLogOn,[switch]$AtStartup,$User,[switch]$Once,$At)
            [pscustomobject]@{CimClass=[pscustomobject]@{CimClassName=$(if ($AtStartup) {'MSFT_TaskBootTrigger'} elseif ($AtLogOn) {'MSFT_TaskLogonTrigger'} else {'MSFT_TaskTimeTrigger'})};Enabled=$true;UserId=$User}
        }
        function New-ScheduledTaskSettingsSet {
            [CmdletBinding()]param($MultipleInstances,$RestartCount,$RestartInterval,$ExecutionTimeLimit,[switch]$StartWhenAvailable,[switch]$AllowStartIfOnBatteries,[switch]$DontStopIfGoingOnBatteries)
            [pscustomobject]@{Enabled=$true;MultipleInstances=2;ExecutionTimeLimit='PT0S';RestartCount=$RestartCount;RestartInterval='PT1M';StartWhenAvailable=[bool]$StartWhenAvailable;DisallowStartIfOnBatteries=(-not $AllowStartIfOnBatteries);StopIfGoingOnBatteries=(-not $DontStopIfGoingOnBatteries);RunOnlyIfIdle=$false}
        }
        function New-ScheduledTask { [CmdletBinding()]param($Action,$Principal,$Trigger,$Settings,$Description) [pscustomobject]@{TaskName='CLC-Linux-Runners';Actions=@($Action);Principal=$Principal;Triggers=@($Trigger);Settings=$Settings;Description=$Description;State='Ready'} }
        function Register-ScheduledTask {
            [CmdletBinding()]param($TaskName,$InputObject,$User,$Password,[switch]$Force)
            Assert-Password ($User -eq $identity.Name -and $Password -ceq $secret -and $Force) 'Password task was registered with the wrong credentials or without ownership-checked replacement'
            if ($script:passwordRegistrationError) { throw "Synthetic Scheduler error containing $Password" }
            $script:passwordRegistrations++;$script:passwordEvents.Add('register');$script:startupTask=$InputObject
        }
        $result=Enable-RunnerStartup $configPath -Mode Password -Credential $credential
        Assert-Password ($result.Mode -eq 'Password' -and -not $result.Current -and $result.Summary -match 'no sign-in required') 'Password startup was misreported'
        Assert-Password (($script:passwordEvents -join ',') -eq 'drain,probe,drain,register') 'Task registration preceded validation or graceful draining'
        Assert-Password (Test-RunnerStartupTask $script:startupTask $configPath Password) 'Boot trigger, action, account or restart settings were wrong'
        $encodedTask=$script:startupTask | ConvertTo-Json -Depth 10
        $report=Get-Content -Raw (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json')
        Assert-Password (-not $encodedTask.Contains($secret) -and -not $report.Contains($secret)) 'Windows password entered a task definition or validation report'
        $result=Enable-RunnerStartup $configPath
        Assert-Password ($result.Mode -eq 'Password' -and $result.Current -and $script:passwordProbes -eq 1 -and $script:passwordRegistrations -eq 1) 'Auto replaced Password startup, repeated a valid probe or requested credentials unnecessarily'
        $script:startupTask.Actions[0].Arguments='wrong-config'
        $result=Enable-RunnerStartup $configPath -Mode Password -Credential $credential
        Assert-Password ($script:passwordProbes -eq 1 -and $script:passwordRegistrations -eq 2 -and -not $result.Current) 'Task drift was not repaired using valid credentials'
        $result=Enable-RunnerStartup $configPath -Mode Password -Credential $credential
        Assert-Password ($script:passwordRegistrations -eq 3 -and -not $result.Current) 'An explicit password refresh did not update Scheduler credentials'
        $previous=$script:startupTask;$script:passwordFingerprint='changed-inputs';$script:passwordProbePass=$false
        Assert-PasswordFailure { Enable-RunnerStartup $configPath -Mode Password -Credential $credential } 'did not pass'
        Assert-Password ($script:passwordRegistrations -eq 3 -and [object]::ReferenceEquals($previous,$script:startupTask)) 'Failed noninteractive validation replaced the existing task'
        $script:passwordProbePass=$true
        $null=Enable-RunnerStartup $configPath -Mode Password -Credential $credential
        $wrong=[PSCredential]::new('S-1-5-18',$credential.Password)
        Assert-PasswordFailure { Enable-RunnerStartup $configPath -Mode Password -Credential $wrong } 'current controller account'
        $empty=[PSCredential]::new($identity.Name,[Security.SecureString]::new())
        Assert-PasswordFailure { Get-RunnerStartupCredential $empty } 'nonempty Windows account password'
        $script:passwordRegistrationError=$true
        $message='';try { Register-RunnerPasswordTask 'CLC-Linux-Runners' $script:startupTask $credential -Force } catch { $message=$_.Exception.Message }
        Assert-Password ($message -match 'REDACTED' -and -not $message.Contains($secret)) 'Scheduler registration failure leaked the Windows password'
        $script:passwordRegistrationError=$false
        $script:startupTask.Principal.UserId='S-1-5-18'
        Assert-PasswordFailure { Enable-RunnerStartup $configPath -Mode Password -Credential $credential } 'another account'
        $script:startupTask.Principal.UserId=$identity.Name

        $broker=@{Calls=0}
        $beforeBroker=$script:passwordRegistrations
        $registrar={
            $broker.Calls++
            $script:startupTask=New-RunnerPasswordStartupTask $configPath $binary $identity.Name $repository Limited
        }
        $null=Enable-RunnerStartup $configPath -Mode Password -Credential $credential -PasswordTaskRegistrar $registrar
        Assert-Password ($broker.Calls -eq 1 -and $script:passwordRegistrations -eq $beforeBroker -and (Test-RunnerStartupTask $script:startupTask $configPath Password)) 'Bootstrap worker registered the boot task itself or accepted an invalid broker result'
        $script:passwordFingerprint='unvalidated-bootstrap';$script:passwordProbePass=$false
        Assert-PasswordFailure { Enable-RunnerStartup $configPath -Mode Password -Credential $credential -PasswordTaskRegistrar $registrar } 'did not pass'
        Assert-Password ($broker.Calls -eq 1) 'Failed session-0 validation requested privileged registration'
        $script:passwordFingerprint='changed-inputs';$script:passwordProbePass=$true
        $null=Enable-RunnerStartup $configPath -Mode Password -Credential $credential

        function Get-RunnerRuntime { param($Config) [pscustomobject]@{Alive=$false;Status=@{}} }
        function Start-ScheduledTask { [CmdletBinding()]param($TaskName) if ($script:passwordStartError) { throw 'Synthetic task start failure' };Assert-Password ($TaskName -eq 'CLC-Linux-Runners') 'Wrong background task started';$script:passwordStarts++ }
        function Wait-RunnerHealthy { param($Config,$TimeoutSeconds,$StartupGraceSeconds) Assert-Password ($TimeoutSeconds -eq 120 -and $StartupGraceSeconds -eq 15) 'Scheduled launch lacked startup grace';[pscustomobject]@{Healthy=$true} }
        function Start-Process { throw 'Password mode fell back to an interactive-session process' }
        Start-RunnerController $configPath -Installed *> $null
        Assert-Password ($script:passwordStarts -eq 1) 'Controller did not start via its noninteractive Scheduler task'
        $script:passwordStartError=$true
        Assert-PasswordFailure { Start-RunnerController $configPath -Installed } 'task start failure'
        $script:passwordStartError=$false;$script:passwordFingerprint='stale-probe'
        Assert-PasswordFailure { Start-RunnerController $configPath -Installed } 'validation is stale'
        $script:passwordFingerprint='changed-inputs'

        function Assert-RunnerRestartManifest { param($Request) @{binary=$binary} }
        function Invoke-RunnerRestartBinary { param($Binary,$Verb,$Config,$Extra)
            switch ($Verb) { 'image-status' {'{"current":true,"image":{"id":"fixture"}}'}; 'setup' {'{"current":true}'} }
        }
        $request=@{config=$configPath;requestPath=(Join-Path $local.Settings.controller.dataDirectory 'restart.json');target=@{
            imageId='fixture';configHash=(Get-FileHash $configPath).Hash;patHash=(Get-FileHash $local.Settings.controller.patFile).Hash;runtimeHash=(Get-FileHash $config.hyperv.script).Hash
        }}
        $null=Test-RunnerRestartPlan $request
        $script:passwordFingerprint='new-runtime'
        Assert-PasswordFailure { Test-RunnerRestartPlan $request } 'TestUnattended -StartupMode Password'

        $config.controller.startupRunLevel='Highest';Write-RunnerJson $configPath $config
        Assert-Password (-not (Test-RunnerStartupTask $script:startupTask $configPath Password)) 'A limited task matched an elevated configuration'
        $null=Enable-RunnerStartup $configPath -Mode Password -Credential $credential
        Assert-Password ($script:startupTask.Principal.RunLevel -eq 'Highest' -and (Test-RunnerStartupTask $script:startupTask $configPath Password)) 'Diagnostic elevation was not applied to the permanent task'
        $config.controller.startupRunLevel='Limited';Write-RunnerJson $configPath $config
        Assert-Password (-not (Test-RunnerStartupTask $script:startupTask $configPath Password)) 'An elevated task matched a limited configuration'
        $null=Enable-RunnerStartup $configPath -Mode Password -Credential $credential
        Assert-Password ($script:startupTask.Principal.RunLevel -eq 'Limited') 'Limited startup was not restored after diagnostic elevation'
    }

    # Run the actual probe script with mocked Scheduler commands. It must reject
    # an interactive session, wrong SID and a failing worker before replacing the
    # production startup task; every temporary task/blob is cleaned up.
    & {
        $shim=Join-Path $scratch 'probe';New-Item -ItemType Directory $shim | Out-Null
        Copy-Item -LiteralPath (Join-Path $repository 'scripts/Test-Unattended.ps1') -Destination $shim
        [IO.File]::WriteAllText((Join-Path $shim 'Common.ps1'),(". '"+$repository.Replace("'","''")+"/scripts/Common.ps1'"))
        [IO.File]::WriteAllText((Join-Path $shim 'Startup.ps1'),(". '"+$repository.Replace("'","''")+"/scripts/Startup.ps1'`nfunction Get-RunnerStartupFingerprint { 'password-probe-fixture' }"))
        $tasks=[Collections.Generic.HashSet[string]]::new();$probe=@{Session=0;Interactive=$false;SID=$identity.User.Value;Exit=0;RegisterFails=$false;PAT=$true;RunLevel='Limited';Elevated=$false}
        function New-ScheduledTaskAction { [CmdletBinding()]param($Execute,$Argument,$WorkingDirectory) Assert-Password (-not $Argument.Contains($secret) -and $Argument.Contains('-RequirePAT')) 'Probe command leaked a password or omitted saved PAT validation';@{} }
        function New-ScheduledTaskPrincipal { [CmdletBinding()]param($UserId,$LogonType,$RunLevel) Assert-Password ($LogonType -eq 'Password' -and $RunLevel -eq $probe.RunLevel) 'Probe did not use Password batch logon at the configured run level';@{} }
        function New-ScheduledTaskTrigger { [CmdletBinding()]param([switch]$Once,$At) @{} }
        function New-ScheduledTaskSettingsSet { [CmdletBinding()]param($ExecutionTimeLimit,[switch]$AllowStartIfOnBatteries,[switch]$DontStopIfGoingOnBatteries) @{} }
        function New-ScheduledTask { [CmdletBinding()]param($Action,$Principal,$Trigger,$Settings) @{} }
        function Register-ScheduledTask {
            [CmdletBinding()]param($TaskName,$InputObject,$User,$Password,[switch]$Force)
            Assert-Password ($TaskName -match '^CLC-Startup-Probe-[a-f0-9]{12}-Password$' -and -not $Force) 'Probe tried to replace a permanent task'
            Assert-Password ($User -eq $identity.Name -and $Password -ceq $secret) 'Password probe lost its Windows credential'
            if ($probe.RegisterFails) { throw "Probe registration failure: $Password" }
            [void]$tasks.Add($TaskName)
        }
        function Start-ScheduledTask {
            [CmdletBinding()]param($TaskName)
            $null=$TaskName -match '^CLC-Startup-Probe-([a-f0-9]{12})-Password$'
            Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory ("clc-startup-probe-$($Matches[1])-Password.json")) @{
                passed=$true;dpapi=$true;pat=$probe.PAT;hypervBoot=$true;errors=@();accountSid=$probe.SID;sessionId=$probe.Session;interactive=$probe.Interactive;elevated=$probe.Elevated
            }
        }
        function Get-ScheduledTask { [CmdletBinding()]param($TaskName) if ($tasks.Contains($TaskName)) { [pscustomobject]@{State='Ready'} } }
        function Get-ScheduledTaskInfo { [CmdletBinding()]param($TaskName) [pscustomobject]@{LastTaskResult=$probe.Exit} }
        function Stop-ScheduledTask { [CmdletBinding()]param($TaskName) }
        function Unregister-ScheduledTask { [CmdletBinding()]param($TaskName,[switch]$Confirm) [void]$tasks.Remove($TaskName) }
        $runProbe={ & (Join-Path $shim 'Test-Unattended.ps1') -Config $configPath -Mode Password -Credential $credential *> $null }
        $reportPath=Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json'
        try { & $runProbe } catch { throw ('Synthetic Password probe failed: '+((Read-RunnerJson $reportPath | ConvertTo-Json -Depth 10 -Compress).Replace($secret,'[REDACTED]'))) }
        $report=Read-RunnerJson $reportPath
        Assert-Password ($report.validationMode -eq 'Password' -and $report.modes.Password.passed -and $tasks.Count -eq 0) 'Successful noninteractive probe was not recorded or cleaned up'
        Assert-Password (-not (Get-Content -Raw $reportPath).Contains($secret)) 'Probe report persisted the Windows password'
        $probe.Session=1;Assert-PasswordFailure $runProbe 'Password scheduled task did not pass';$probe.Session=0
        $probe.Interactive=$true;Assert-PasswordFailure $runProbe 'Password scheduled task did not pass';$probe.Interactive=$false
        $probe.SID='S-1-5-18';Assert-PasswordFailure $runProbe 'Password scheduled task did not pass';$probe.SID=$identity.User.Value
        $probe.Exit=1;Assert-PasswordFailure $runProbe 'Password scheduled task did not pass';$probe.Exit=0
        $probe.PAT=$false;Assert-PasswordFailure $runProbe 'Password scheduled task did not pass';$probe.PAT=$true
        $probe.Elevated=$true;Assert-PasswordFailure $runProbe 'Password scheduled task did not pass';$probe.Elevated=$false
        $config.controller.startupRunLevel='Highest';Write-RunnerJson $configPath $config;$probe.RunLevel='Highest'
        Assert-PasswordFailure $runProbe 'Password scheduled task did not pass'
        $probe.Elevated=$true;& $runProbe
        $report=Read-RunnerJson $reportPath
        Assert-Password ($report.runLevel -eq 'Highest' -and $report.modes.Password.passed -and $report.modes.Password.elevated) 'Elevated diagnostics did not validate the actual worker token'
        $config.controller.startupRunLevel='Limited';Write-RunnerJson $configPath $config;$probe.RunLevel='Limited';$probe.Elevated=$false
        $probe.RegisterFails=$true;Assert-PasswordFailure $runProbe 'Password scheduled task did not pass'
        Assert-Password (-not (Get-Content -Raw $reportPath).Contains($secret)) 'Failed probe report leaked the registration password'
        Assert-Password ($tasks.Count -eq 0 -and -not @(Get-ChildItem -LiteralPath $local.Settings.controller.dataDirectory -Filter 'clc-startup-probe-*').Count) 'Probe left a temporary task, result or DPAPI blob behind'
    }
    & {
        # Exercise real CurrentUser DPAPI for both the sentinel and the saved
        # token. Only the expensive Hyper-V clone operation is substituted.
        $shim=Join-Path $scratch 'worker';New-Item -ItemType Directory $shim | Out-Null
        Copy-Item -LiteralPath (Join-Path $repository 'scripts/Invoke-StartupProbe.ps1') -Destination $shim
        $common=". '"+$repository.Replace("'","''")+"/scripts/Common.ps1'`nfunction Invoke-RunnerCommand { param(`$Verb,`$Config) if (`$Verb -ne 'probe-hyperv') { throw 'Unexpected native operation' } }"
        [IO.File]::WriteAllText((Join-Path $shim 'Common.ps1'),$common)
        $sentinel=Join-Path $scratch 'worker-probe.dpapi';$resultPath=Join-Path $scratch 'worker-result.json'
        $syntheticPAT='github_pat_SYNTHETIC_PASSWORD_STARTUP_TEST'
        [IO.File]::WriteAllBytes($sentinel,[Security.Cryptography.ProtectedData]::Protect([Text.Encoding]::UTF8.GetBytes('clc-dpapi-probe'),$null,[Security.Cryptography.DataProtectionScope]::CurrentUser))
        [IO.File]::WriteAllBytes($local.Settings.controller.patFile,[Security.Cryptography.ProtectedData]::Protect([Text.Encoding]::UTF8.GetBytes($syntheticPAT),$null,[Security.Cryptography.DataProtectionScope]::CurrentUser))
        & (Join-Path $shim 'Invoke-StartupProbe.ps1') -Config $configPath -ResultPath $resultPath -ProbeFile $sentinel -RequirePAT
        $result=Read-RunnerJson $resultPath
        Assert-Password ($LASTEXITCODE -eq 0 -and $result.passed -and $result.pat) 'Worker did not decrypt the actual saved token'
        Assert-Password (-not (Get-Content -Raw $resultPath).Contains($syntheticPAT)) 'Worker report leaked the decrypted token'
        [IO.File]::WriteAllText($local.Settings.controller.patFile,'corrupt encrypted token')
        & (Join-Path $shim 'Invoke-StartupProbe.ps1') -Config $configPath -ResultPath $resultPath -ProbeFile $sentinel -RequirePAT
        $result=Read-RunnerJson $resultPath
        Assert-Password ($LASTEXITCODE -eq 1 -and -not $result.passed -and -not $result.pat -and ($result.errors -join ' ') -match 'Saved PAT') 'Fresh sentinel incorrectly validated an unreadable saved token'
        $global:LASTEXITCODE=0
    }
    & {
        $launchState=@{Attempts=0}
        function Get-RunnerRuntime { param($Config) $launchState.Attempts++;[pscustomobject]@{Alive=($launchState.Attempts -gt 1);Healthy=($launchState.Attempts -gt 1);Status=@{}} }
        function Get-RunnerControllerProcesses { @() }
        function Start-Sleep { param($Milliseconds) }
        $result=Wait-RunnerHealthy $configPath -TimeoutSeconds 3 -StartupGraceSeconds 15
        Assert-Password ($result.Healthy -and $launchState.Attempts -eq 2) 'Scheduler launch latency was reported as a crashed controller'
        $launchState.Attempts=0
        Assert-PasswordFailure { Wait-RunnerHealthy $configPath -TimeoutSeconds 3 } 'exited before listening'
    }
    & {
        # Real dispatcher, isolated fake installation: typed credentials must
        # reach setup in memory, and explicit Password mode must be forwarded.
        $shim=Join-Path $scratch 'dispatcher';$shimScripts=Join-Path $shim 'scripts'
        New-Item -ItemType Directory $shimScripts -Force | Out-Null
        Copy-Item -LiteralPath (Join-Path $repository 'runners.ps1') -Destination $shim
        Copy-Item -LiteralPath (Join-Path $repository 'scripts/Common.ps1') -Destination $shimScripts
        foreach ($name in @('Dependencies.ps1','Host.ps1')) { [IO.File]::WriteAllText((Join-Path $shimScripts $name),'') }
        [IO.File]::WriteAllText((Join-Path $shimScripts 'Activate.ps1'),@'
function Invoke-RunnerActivation {
    param($Config,[switch]$NoStartup,[switch]$ForceRebuild,[switch]$FromGitHubCLI,[switch]$NoCache,$StartupMode,[PSCredential]$StartupCredential)
    if ($StartupMode -ne 'Password' -or -not $StartupCredential) { throw 'Password activation arguments lost' }
    Add-Content (Join-Path $script:RunnerRoot 'events.txt') 'activate'
}
'@)
        [IO.File]::WriteAllText((Join-Path $shimScripts 'Startup.ps1'),@'
function Enable-RunnerStartup {
    param($Config,$Mode,[PSCredential]$Credential)
    if ($Mode -ne 'Password' -or -not $Credential) { throw 'Password startup arguments lost' }
    Add-Content (Join-Path $script:RunnerRoot 'events.txt') 'enable'
    [pscustomobject]@{Mode='Password';Summary='background';Current=$false}
}
function Start-RunnerController {
    param($Config,[switch]$Installed)
    if (-not $Installed) { throw 'Expected installed controller' }
    Add-Content (Join-Path $script:RunnerRoot 'events.txt') 'start'
}
function Test-RunnerUnattended {
    param($Config,$Mode,[PSCredential]$Credential)
    if ($Mode -ne 'Password' -or -not $Credential) { throw 'Password probe arguments lost' }
    Add-Content (Join-Path $script:RunnerRoot 'events.txt') 'probe'
}
'@)
        [IO.File]::WriteAllText((Join-Path $shimScripts 'Restart.ps1'),"function New-RunnerRestartRuntime { Add-Content (Join-Path `$script:RunnerRoot 'events.txt') 'runtime' }")
        $wrapper=Join-Path $shim 'invoke.ps1'
        [IO.File]::WriteAllText($wrapper,@'
param($Command)
$credential=[PSCredential]::new([Security.Principal.WindowsIdentity]::GetCurrent().Name,(ConvertTo-SecureString 'synthetic dispatcher password' -AsPlainText -Force))
& (Join-Path $PSScriptRoot 'runners.ps1') $Command -StartupMode Password -StartupCredential $credential
exit $LASTEXITCODE
'@)
        foreach ($command in @('EnableStartup','Activate','TestUnattended')) {
            $output=& (Get-Command pwsh.exe).Source -NoProfile -NonInteractive -File $wrapper $command 2>&1
            Assert-Password ($LASTEXITCODE -eq 0) ("Password dispatcher failed: $command "+($output -join ' '))
        }
        $events=Get-Content (Join-Path $shim 'events.txt')
        Assert-Password (($events -join ',') -eq 'enable,runtime,start,activate,probe') 'Password setup did not stage restart support and start independently'
    }
    Write-Host "Password startup: $($passwordCheckState.Count) credential, session-0 probe, boot, fingerprint, Scheduler launch and restart assertions passed (mocked Scheduler)."
} finally {
    $absolute=[IO.Path]::GetFullPath($scratch)
    $prefix=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')+'\'
    if ($absolute.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf) -match '^clc-password-startup-[a-f0-9]{32}$') { Remove-Item -LiteralPath $absolute -Recurse -Force }
}
