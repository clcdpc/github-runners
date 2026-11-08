#requires -Version 7.4

function Get-RunnerAccountSetupPlan([string]$Config,[string]$StorageRoot,$Account) {
    $receiptPath=Join-Path $script:RunnerRoot 'bin/account-setup.json'
    if (-not (Test-Path -LiteralPath $receiptPath)) {
        $plan=Get-RunnerAccountPlan $Config $StorageRoot
        $plan | Add-Member -NotePropertyName Prepared -NotePropertyValue $false
        return $plan
    }
    # This non-secret receipt stays outside the account/SYSTEM-only runtime
    # directories, so an administrator can resume after partial activation.
    $receipt=Read-RunnerJson $receiptPath
    $root=Assert-RunnerAccountPath $script:RunnerRoot -Repository
    $path=[IO.Path]::GetFullPath($Config,$root)
    if ($receipt -isnot [Collections.IDictionary] -or $receipt['version'] -ne 1 -or -not $Account -or $receipt['accountSid'] -ne $Account.SID.Value -or
        $receipt['root'] -ne $root -or $receipt['config'] -ne $path -or -not $receipt['storageRoot']) { throw 'Account setup receipt belongs to another account/checkout/configuration or is invalid. No installation was changed.' }
    $gitDirectory=Join-Path $root '.git'
    if (-not (Test-Path -LiteralPath $gitDirectory -PathType Container)) { throw 'Use the prepared standalone checkout, not a linked worktree.' }
    foreach ($directory in @($root,$gitDirectory)) {
        if ((Get-Item -LiteralPath $directory -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Prepared checkout must not contain a linked root or .git directory.' }
        if ((Get-Acl -LiteralPath $directory).GetOwner([Security.Principal.SecurityIdentifier]).Value -ne $Account.SID.Value) { throw 'Prepared checkout ownership no longer matches the runner account.' }
    }
    $null=Assert-RunnerAccountPath (Split-Path $path -Parent)
    if ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Configuration must not be a reparse point.' }
    $local=Get-RunnerSettings $path
    if ($local.Settings.controller.startupRunLevel -ne 'Limited') { throw 'Account setup requires controller.startupRunLevel to be Limited.' }
    if ($local.Settings.storageRoot -ne $receipt.storageRoot -or ($StorageRoot -and [IO.Path]::GetFullPath($StorageRoot,$root) -ne $receipt.storageRoot)) { throw '-StorageRoot cannot relocate a prepared installation.' }
    [pscustomobject]@{ Root=$root; Config=$path; Prepared=$true }
}

function Get-RunnerAccountSetupCredential([string]$UserName,[PSCredential]$Credential) {
    $name="$env:COMPUTERNAME\$UserName"
    if (-not $Credential) { $Credential=Get-Credential -UserName $name -Message 'Windows password for the local runner account (setup and unattended startup)' }
    if (-not $Credential -or (ConvertTo-RunnerLocalUserName $Credential.UserName) -ne $UserName) { throw 'The Windows credential must belong to the requested local runner account.' }
    if (-not $Credential.Password.Length) { throw 'A nonempty Windows password is required.' }
    [PSCredential]::new($name,$Credential.Password)
}

function New-RunnerAccountSetupProcessInfo([string]$Config,[string]$AccountSid,[PSCredential]$Credential,[switch]$RefreshAuthentication,[switch]$ForceRebuild,[switch]$NoCache) {
    $info=[Diagnostics.ProcessStartInfo]::new((Get-Command pwsh.exe -ErrorAction Stop).Source)
    $info.UseShellExecute=$false
    $info.CreateNoWindow=$true
    $info.WindowStyle=[Diagnostics.ProcessWindowStyle]::Hidden
    $info.LoadUserProfile=$true
    $info.Domain=$env:COMPUTERNAME
    $info.UserName=ConvertTo-RunnerLocalUserName $Credential.UserName
    $info.Password=$Credential.Password
    $info.WorkingDirectory=$script:RunnerRoot
    $info.RedirectStandardInput=$true
    $info.RedirectStandardOutput=$true
    $info.RedirectStandardError=$true
    $encoding=[Text.UTF8Encoding]::new($false)
    $info.StandardInputEncoding=$encoding
    $info.StandardOutputEncoding=$encoding
    $info.StandardErrorEncoding=$encoding
    foreach ($argument in @('-NoLogo','-NoProfile','-NonInteractive','-File',(Join-Path $script:RunnerRoot 'scripts/Invoke-AccountSetup.ps1'),'-Config',$Config,'-AccountSid',$AccountSid)) { $info.ArgumentList.Add($argument) }
    foreach ($option in @(@('RefreshAuthentication',$RefreshAuthentication),@('ForceRebuild',$ForceRebuild),@('NoCache',$NoCache))) { if ($option[1]) { $info.ArgumentList.Add('-'+$option[0]) } }
    # Do not populate Environment: Windows must construct it from the target
    # user's loaded profile, rather than inheriting the administrator's profile.
    $info
}

function Start-RunnerAccountSetupProcess([Diagnostics.ProcessStartInfo]$Info) { [Diagnostics.Process]::Start($Info) }

function Register-RunnerAccountStartup([string]$Config,[string]$AccountSid,[PSCredential]$Credential) {
    $existing=Get-RunnerStartupTask
    $description='CLC Linux Hyper-V runner controller; '+$Config
    if ($existing -and ($existing.Description -notin @($description,('CLC Linux WSLC runner controller; '+$Config)) -or
        (Get-RunnerAccountSid $existing.Principal.UserId) -ne $AccountSid)) { throw 'Startup task belongs to another account/configuration; no task was changed.' }
    # Construct the definition here: the lower-privilege worker cannot choose
    # an executable, principal, run level or arbitrary task for the administrator.
    $task=New-RunnerPasswordStartupTask $Config (Get-RunnerBinary) $Credential.UserName $script:RunnerRoot Limited
    $password=$null
    try {
        $password=$Credential.GetNetworkCredential().Password
        Register-ScheduledTask -TaskName 'CLC-Linux-Runners' -InputObject $task -User $Credential.UserName -Password $password -Force -ErrorAction Stop | Out-Null
        $scheduler=New-Object -ComObject 'Schedule.Service'
        $scheduler.Connect()
        $registered=$scheduler.GetFolder('\').GetTask('CLC-Linux-Runners')
        $registered.SetSecurityDescriptor("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;$AccountSid)",0)
    } catch {
        $message=$_.Exception.Message
        if ($password) { $message=$message.Replace($password,'[REDACTED]') }
        throw (Protect-RunnerText $message)
    } finally { $password=$null }
}

function Invoke-RunnerAccountSetup([string]$Config,[string]$AccountSid,[PSCredential]$Credential,[Security.SecureString]$Token,[switch]$ForceRebuild,[switch]$NoCache) {
    $info=New-RunnerAccountSetupProcessInfo $Config $AccountSid $Credential -RefreshAuthentication:([bool]$Token) -ForceRebuild:$ForceRebuild -NoCache:$NoCache
    $process=$null
    $password=$null
    $payload=$null
    $channel=[guid]::NewGuid().ToString('N')
    $prefix='CLC-ACCOUNT-SETUP:'+ $channel + ':'
    try {
        $password=$Credential.GetNetworkCredential().Password
        $process=Start-RunnerAccountSetupProcess $info
        $errors=$process.StandardError.ReadToEndAsync()
        # Anonymous, inherited process pipes carry secrets only in memory.
        # Arguments, environment, task definitions and files contain none.
        $payload=@{version=1;password=$password;channel=$channel} | ConvertTo-Json -Compress
        $process.StandardInput.WriteLine($payload)
        $process.StandardInput.Flush()
        $payload=$null
        while ($null -ne ($line=$process.StandardOutput.ReadLine())) {
            if ($line -ceq ($prefix+'TOKEN')) {
                $secureToken=$Token
                if (-not $secureToken) { $secureToken=Read-Host 'Organization fine-grained PAT (input hidden)' -AsSecureString }
                if (-not $secureToken -or -not $secureToken.Length) { throw 'A nonempty PAT is required.' }
                $pointer=[Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureToken)
                try {
                    $payload=@{version=1;token=[Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer)} | ConvertTo-Json -Compress
                    $process.StandardInput.WriteLine($payload)
                    $process.StandardInput.Flush()
                } finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer); $payload=$null; if ($secureToken -ne $Token) { $secureToken.Dispose() }; $secureToken=$null }
            } elseif ($line -ceq ($prefix+'STARTUP')) {
                try {
                    Register-RunnerAccountStartup $Config $AccountSid $Credential
                    $reply=@{version=1;ok=$true}
                } catch { $reply=@{version=1;ok=$false;error=(Protect-RunnerText $_.Exception.Message.Replace($password,'[REDACTED]'))} }
                $process.StandardInput.WriteLine(($reply | ConvertTo-Json -Compress))
                $process.StandardInput.Flush()
            } elseif ($line.StartsWith($prefix,[StringComparison]::Ordinal)) { throw 'Unknown account setup request; no operation was performed.' }
            else { Write-Host (Protect-RunnerText $line.Replace($password,'[REDACTED]')) }
        }
        $process.WaitForExit()
        $errorText=Protect-RunnerText ($errors.GetAwaiter().GetResult()).Replace($password,'[REDACTED]')
        if ($errorText) { Write-Host $errorText }
        if ($process.ExitCode -ne 0) { throw "Runner account setup failed (exit $($process.ExitCode)): $errorText`nCorrect the reported error and rerun the same Setup-RunnerAccount command." }
    } catch {
        $message=$_.Exception.Message
        if ($password) { $message=$message.Replace($password,'[REDACTED]') }
        throw (Protect-RunnerText $message)
    }
    finally {
        $payload=$null
        $password=$null
        $info.Password=$null
        if ($process) {
            $process.StandardInput.Dispose()
            if (-not $process.HasExited) { $process.Kill($true); $process.WaitForExit() }
            $process.Dispose()
        }
    }
}
