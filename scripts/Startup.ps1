#requires -Version 7.4

function Get-RunnerAccountSid([string]$Account) {
    if ($Account -match '^S-1-') { return $Account }
    ([Security.Principal.NTAccount]::new($Account)).Translate([Security.Principal.SecurityIdentifier]).Value
}

function Get-RunnerStartupFingerprint([string]$Config,[string]$Binary = (Get-RunnerBinary),[string]$ImageID,[ValidateSet('Auto','Password')][string]$Mode='Auto') {
    $localSettings = Get-RunnerSettings $Config
    $settings = $localSettings.Settings
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    if (-not $ImageID) { $image = Get-RunnerImage $Config -Binary $Binary; if (-not $image) { throw 'Build the runner image before testing startup.' }; $ImageID=$image['Id'] }
    $context = [ordered]@{
        sid=$identity.User.Value; config=$localSettings.Path; imageId=$ImageID; hyperv=($settings.hyperv | ConvertTo-Json -Compress)
        runtime='Hyper-V'
        startupRunLevel=$settings.controller.startupRunLevel
        data=$settings.controller.dataDirectory; pat=$settings.controller.patFile
        binaryHash=(Get-FileHash -LiteralPath $Binary).Hash
    }
    if ($Mode -eq 'Password') {
        # Password logons authenticate afresh at boot. Validation must survive
        # sign-out, reboot and the restart helper's noninteractive session.
        $context['startupMode']='Password'
        $context['machineGuid']=(Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\Microsoft\Cryptography' -Name MachineGuid -ErrorAction Stop).MachineGuid
        $context['patHash']=(Get-FileHash -LiteralPath $settings.controller.patFile -ErrorAction Stop).Hash
    } else {
        $context['windowsSession']=(Get-Process -Id $PID).SessionId
        $context['boot']=(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o')
    }
    $context=$context | ConvertTo-Json -Compress
    $sources = @('Test-Unattended.ps1','Invoke-StartupProbe.ps1','Startup.ps1','HyperV.ps1','Common.ps1','Restart.ps1','Restart-Controller.ps1') | ForEach-Object { Join-Path $script:RunnerRoot "scripts/$_" }
    Get-RunnerFingerprint $sources $context
}

function Get-RunnerStartupValidation([string]$Config,[string]$Binary = (Get-RunnerBinary),[string]$ImageID) {
    $settings = (Get-RunnerSettings $Config).Settings
    $report = Read-RunnerJson (Join-Path $settings.controller.dataDirectory 'unattended-validation.json')
    if (-not $report -or $report['modes'] -isnot [Collections.IDictionary]) { return $null }
    foreach ($mode in @('S4U','Interactive')) {
        if ($report['modes'][$mode] -isnot [Collections.IDictionary] -or $report['modes'][$mode]['passed'] -isnot [bool]) { return $null }
    }
    $mode=if ($report['validationMode'] -eq 'Password') { 'Password' } else { 'Auto' }
    if ($mode -eq 'Password' -and ($report['modes']['Password'] -isnot [Collections.IDictionary] -or $report['modes']['Password']['passed'] -isnot [bool])) { return $null }
    if ($report['fingerprint'] -eq (Get-RunnerStartupFingerprint $Config -Binary $Binary -ImageID $ImageID -Mode $mode)) { return $report }
    return $null
}

function Test-RunnerUnattended([string]$Config,[ValidateSet('Auto','Password')][string]$Mode='Auto',[PSCredential]$Credential) {
    if ($Mode -eq 'Password') {
        & (Join-Path $script:RunnerRoot 'scripts/Test-Unattended.ps1') -Config $Config -Mode Password -Credential $Credential
    } else {
        & (Join-Path $script:RunnerRoot 'scripts/Test-Unattended.ps1') -Config $Config
    }
}

function Get-RunnerStartupCredential([PSCredential]$Credential) {
    $identity=[Security.Principal.WindowsIdentity]::GetCurrent()
    if (-not $Credential) {
        if (-not [Environment]::UserInteractive -or [Console]::IsInputRedirected) { throw 'Password startup needs the controller account Windows credential. Run EnableStartup -StartupMode Password -StartupCredential (Get-Credential) in PowerShell under that account; never put the password on the command line.' }
        $Credential=Get-Credential -UserName $identity.Name -Message 'Windows password for unattended runner startup (not your PIN or GitHub PAT)'
    }
    if (-not $Credential -or (Get-RunnerAccountSid $Credential.UserName) -ne $identity.User.Value) { throw 'Startup credential must belong to the current controller account that owns the DPAPI PAT.' }
    if ($Credential.Password.Length -eq 0) { throw 'Password startup requires a nonempty Windows account password.' }
    [PSCredential]::new($identity.Name,$Credential.Password)
}

function Register-RunnerPasswordTask([string]$TaskName,$Task,[PSCredential]$Credential,[switch]$Force) {
    $Credential=Get-RunnerStartupCredential $Credential
    $password=$null
    try {
        # Task Scheduler requires a managed string here. It never enters a
        # subprocess argument, task definition, report or repository file.
        $password=$Credential.GetNetworkCredential().Password
        Register-ScheduledTask -TaskName $TaskName -InputObject $Task -User $Credential.UserName -Password $password -Force:$Force -ErrorAction Stop | Out-Null
    } catch {
        $message=$_.Exception.Message
        if ($password) { $message=$message.Replace($password,'[REDACTED]') }
        throw (Protect-RunnerText $message)
    } finally { $password=$null }
}

function Get-RunnerStartupTask {
    # A missing task is normal. Scheduler/service/permission failures must surface.
    @(Get-ScheduledTask -TaskPath '\' -ErrorAction Stop | Where-Object TaskName -EQ 'CLC-Linux-Runners') | Select-Object -First 1
}

function Disable-RunnerStartup([string]$Config) {
    $task=Get-RunnerStartupTask
    if (-not $task) { return }
    $path=(Get-RunnerSettings $Config).Path
    $descriptions=@(('CLC Linux Hyper-V runner controller; '+$path),('CLC Linux WSLC runner controller; '+$path))
    if ($task.Description -notin $descriptions -or (Get-RunnerAccountSid $task.Principal.UserId) -ne [Security.Principal.WindowsIdentity]::GetCurrent().User.Value) { throw 'Startup task belongs to another configuration/account; no task was removed.' }
    Unregister-ScheduledTask -TaskName 'CLC-Linux-Runners' -Confirm:$false -ErrorAction Stop
}

function Test-RunnerStartupTask($Task, [string]$Config, [string]$Mode) {
    if (-not $Task) { return $false }
    $localSettings = Get-RunnerSettings $Config
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $expectedArguments = 'run --quiet --config "{0}"' -f $localSettings.Path
    if ($Task.Description -ne ('CLC Linux Hyper-V runner controller; ' + $localSettings.Path) -or
        @($Task.Actions).Count -ne 1 -or $Task.Actions[0].Execute -ne (Get-RunnerBinary) -or
        $Task.Actions[0].Arguments -cne $expectedArguments -or $Task.Actions[0].WorkingDirectory -ne $script:RunnerRoot -or
        (Get-RunnerAccountSid $Task.Principal.UserId) -ne $identity.User.Value -or
        [string]$Task.Principal.LogonType -ne $Mode -or [string]$Task.Principal.RunLevel -ne $localSettings.Settings.controller.startupRunLevel) { return $false }
    $settings = $Task.Settings
    if (-not $settings.Enabled -or [int]$settings.MultipleInstances -ne 2 -or $settings.ExecutionTimeLimit -ne 'PT0S' -or
        $settings.RestartCount -ne 99 -or $settings.RestartInterval -ne 'PT1M' -or -not $settings.StartWhenAvailable -or
        $settings.DisallowStartIfOnBatteries -or $settings.StopIfGoingOnBatteries -or $settings.RunOnlyIfIdle) { return $false }
    $logon = @($Task.Triggers | Where-Object { $_.CimClass.CimClassName -eq 'MSFT_TaskLogonTrigger' })
    $boot = @($Task.Triggers | Where-Object { $_.CimClass.CimClassName -eq 'MSFT_TaskBootTrigger' })
    if ($logon.Count -ne 1 -or -not $logon[0].Enabled -or (Get-RunnerAccountSid $logon[0].UserId) -ne $identity.User.Value) { return $false }
    if ($Mode -in @('S4U','Password')) { return @($Task.Triggers).Count -eq 2 -and $boot.Count -eq 1 -and $boot[0].Enabled }
    return @($Task.Triggers).Count -eq 1
}

function Get-RunnerStartupSummary([string]$Config) {
    $task = Get-RunnerStartupTask
    if (-not $task) { return 'Not configured' }
    if ($task.Description -eq ('CLC Linux WSLC runner controller; ' + (Get-RunnerSettings $Config).Path)) { return 'Legacy startup task preserved; Hyper-V startup validation required' }
    if ($task.Description -ne ('CLC Linux Hyper-V runner controller; ' + (Get-RunnerSettings $Config).Path)) { return 'Task belongs to another configuration' }
    if (-not $task.Settings.Enabled) { return 'Disabled' }
    $mode=[string]$task.Principal.LogonType
    if ($mode -notin @('Interactive','S4U','Password')) { return "Task uses unsupported startup mode $mode; validation required" }
    if (-not (Test-RunnerStartupTask $task $Config $mode)) { return 'Task present; settings require validation/repair' }
    if ([string]$task.Principal.LogonType -eq 'Interactive') { return 'At Windows sign-in (account must remain signed in)' }
    if ($mode -eq 'Password') { return "At Windows boot under the controller account (Password task; no sign-in required; run level $($task.Principal.RunLevel))" }
    return 'S4U task configured; logout and cold boot require separate validation'
}

function Enable-RunnerStartup([string]$Config, [ValidateSet('Auto','S4U','Interactive','Password')][string]$Mode = 'Auto',[PSCredential]$Credential,[scriptblock]$PasswordTaskRegistrar) {
    $localSettings = Get-RunnerSettings $Config
    $binary = Build-RunnerController
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $taskName = 'CLC-Linux-Runners'
    $description = 'CLC Linux Hyper-V runner controller; ' + $localSettings.Path
    $existing = Get-RunnerStartupTask
    if ($existing -and $existing.Description -notin @($description,('CLC Linux WSLC runner controller; ' + $localSettings.Path))) { throw 'CLC-Linux-Runners belongs to another configuration. Use -NoStartup for this installation or explicitly resolve task ownership; no task was changed.' }
    if ($existing -and (Get-RunnerAccountSid $existing.Principal.UserId) -ne $identity.User.Value) { throw 'Startup task belongs to another account; no task was changed.' }
    if ($Credential -and $Mode -notin @('Auto','Password')) { throw 'A Windows startup credential is supported only with Password startup.' }
    if ($Mode -eq 'Auto' -and ($Credential -or ($existing -and [string]$existing.Principal.LogonType -eq 'Password'))) { $Mode='Password' }
    if ($Mode -eq 'Password') { return Enable-RunnerPasswordStartup $Config $binary $existing $Credential -PasswordTaskRegistrar $PasswordTaskRegistrar }
    $report = Get-RunnerStartupValidation $Config
    $needsProbe = -not $report -or (-not $report['modes']['S4U']['passed'] -and -not $report['modes']['Interactive']['passed'])
    if ($Mode -ne 'Auto' -and $report -and -not $report['modes'][$Mode]['passed']) { $needsProbe = $true }
    if ($needsProbe) {
        Stop-RunnerController $Config -Wait
        Test-RunnerUnattended $Config
        $report = Get-RunnerStartupValidation $Config
    }
    if (-not $report) { throw 'Unattended validation does not match the account, boot/session, configuration, image or probe code. Rerun TestUnattended using the same -Config.' }
    if ($Mode -eq 'Auto') {
        $previousMode = if ($existing) { [string]$existing.Principal.LogonType } else { '' }
        $Mode = if ($previousMode -in @('S4U','Interactive') -and $report['modes'][$previousMode]['passed']) { $previousMode }
                elseif ($report['modes']['S4U']['passed']) { 'S4U' } else { 'Interactive' }
    }
    if (-not $report['modes'][$Mode]['passed']) { throw "$Mode did not pass Hyper-V/DPAPI validation. Inspect unattended-validation.json and choose a mode that passed TestUnattended." }
    if (-not (Test-Path -LiteralPath $localSettings.Settings.controller.patFile)) { throw 'Configure the PAT with SetToken before enabling startup.' }
    $current = Test-RunnerStartupTask $existing $Config $Mode
    if (-not $current) {
        $action = New-ScheduledTaskAction -Execute $binary -Argument ('run --quiet --config "{0}"' -f $localSettings.Path) -WorkingDirectory $script:RunnerRoot -ErrorAction Stop
        $principal = New-ScheduledTaskPrincipal -UserId $identity.Name -LogonType $Mode -RunLevel $localSettings.Settings.controller.startupRunLevel -ErrorAction Stop
        $triggers = @(New-ScheduledTaskTrigger -AtLogOn -User $identity.Name -ErrorAction Stop)
        if ($Mode -eq 'S4U') { $triggers += New-ScheduledTaskTrigger -AtStartup -ErrorAction Stop }
        $settings = New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew -RestartCount 99 -RestartInterval ([TimeSpan]::FromMinutes(1)) -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ErrorAction Stop
        $task = New-ScheduledTask -Action $action -Principal $principal -Trigger $triggers -Settings $settings -Description $description -ErrorAction Stop
        Register-ScheduledTask -TaskName $taskName -InputObject $task -Force -ErrorAction Stop | Out-Null
        if (-not (Test-RunnerStartupTask (Get-RunnerStartupTask) $Config $Mode)) { throw 'Startup task registration did not produce the expected action/principal/settings. Inspect Task Scheduler and rerun Activate.' }
    }
    [pscustomobject]@{ Mode=$Mode; Current=$current; Summary=(Get-RunnerStartupSummary $Config) }
}

function New-RunnerPasswordStartupTask([string]$ConfigPath,[string]$Binary,[string]$Account,[string]$WorkingDirectory,[ValidateSet('Limited','Highest')][string]$RunLevel='Limited') {
    $action=New-ScheduledTaskAction -Execute $Binary -Argument ('run --quiet --config "{0}"' -f $ConfigPath) -WorkingDirectory $WorkingDirectory -ErrorAction Stop
    $principal=New-ScheduledTaskPrincipal -UserId $Account -LogonType Password -RunLevel $RunLevel -ErrorAction Stop
    $triggers=@(New-ScheduledTaskTrigger -AtStartup -ErrorAction Stop; New-ScheduledTaskTrigger -AtLogOn -User $Account -ErrorAction Stop)
    $settings=New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew -RestartCount 99 -RestartInterval ([TimeSpan]::FromMinutes(1)) -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ErrorAction Stop
    New-ScheduledTask -Action $action -Principal $principal -Trigger $triggers -Settings $settings -Description ('CLC Linux Hyper-V runner controller; '+$ConfigPath) -ErrorAction Stop
}

function Enable-RunnerPasswordStartup([string]$Config,[string]$Binary,$Existing,[PSCredential]$Credential,[scriptblock]$PasswordTaskRegistrar) {
    $local=Get-RunnerSettings $Config
    if (-not (Test-Path -LiteralPath $local.Settings.controller.patFile)) { throw 'Configure the PAT with SetToken under the controller account before enabling Password startup.' }
    if ($Credential) { $Credential=Get-RunnerStartupCredential $Credential }
    $report=Get-RunnerStartupValidation $Config -Binary $Binary
    if (-not $report -or $report['validationMode'] -ne 'Password' -or -not $report.modes.Password.passed) {
        $Credential=Get-RunnerStartupCredential $Credential
        Stop-RunnerController $Config -Wait
        Test-RunnerUnattended $Config -Mode Password -Credential $Credential
        $report=Get-RunnerStartupValidation $Config -Binary $Binary
    }
    if (-not $report -or $report['validationMode'] -ne 'Password' -or -not $report.modes.Password.passed) { throw 'Password startup did not pass its noninteractive DPAPI/Hyper-V probe. The previous startup task was preserved; inspect unattended-validation.json.' }
    $current=Test-RunnerStartupTask $Existing $Config Password
    if (-not $current -or $Credential) {
        $Credential=Get-RunnerStartupCredential $Credential
        Stop-RunnerController $Config -Wait
        $identity=[Security.Principal.WindowsIdentity]::GetCurrent()
        if ($PasswordTaskRegistrar) {
            # The administrator bootstrap registers boot triggers. The worker
            # still validates the exact task and starts it under its own SID.
            & $PasswordTaskRegistrar
        } else {
            $task=New-RunnerPasswordStartupTask $local.Path $Binary $identity.Name $script:RunnerRoot $local.Settings.controller.startupRunLevel
            Register-RunnerPasswordTask 'CLC-Linux-Runners' $task $Credential -Force
        }
        $current=$false
        if (-not (Test-RunnerStartupTask (Get-RunnerStartupTask) $Config Password)) { throw 'Password startup task registration did not match the expected account, action and settings.' }
    }
    [pscustomobject]@{ Mode='Password'; Current=$current; Summary=(Get-RunnerStartupSummary $Config) }
}
