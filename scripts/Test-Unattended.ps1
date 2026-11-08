#requires -Version 7.4
[CmdletBinding()]
param([string]$Config = 'local/controller.json',[ValidateSet('Auto','Password')][string]$Mode='Auto',[PSCredential]$Credential)
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
$localSettings = Initialize-RunnerConfig $Config
$settings = $localSettings.Settings
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if ($Mode -eq 'Password') { $Credential=Get-RunnerStartupCredential $Credential }
$suffix = [guid]::NewGuid().ToString('N').Substring(0,12)
$sentinel = 'clc-startup-probe-' + $suffix
$probeFile = Join-Path $settings.controller.dataDirectory ($sentinel + '.dpapi')
$worker = Join-Path $PSScriptRoot 'Invoke-StartupProbe.ps1'
$pwshPath = (Get-Command pwsh.exe).Source

$report = [ordered]@{
    checkedAt=(Get-Date).ToString('o'); config=$localSettings.Path; account=$identity.Name; sid=$identity.User.Value
    runtime='Hyper-V'; validationMode=$Mode; runLevel=$settings.controller.startupRunLevel
    modes=[ordered]@{ S4U=@{passed=$false;errors=@('Not tested')}; Interactive=@{passed=$false;errors=@('Not tested')} }
    logoutAndRebootValidated=$false
    fingerprint=(Get-RunnerStartupFingerprint $Config -Mode $Mode)
}
try {
    $probeBytes = [Text.Encoding]::UTF8.GetBytes('clc-dpapi-probe')
    try { [IO.File]::WriteAllBytes($probeFile, [Security.Cryptography.ProtectedData]::Protect($probeBytes, $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)) }
    finally { [Array]::Clear($probeBytes) }
    $probeModes=if ($Mode -eq 'Password') { @('Password') } else { @('S4U','Interactive') }
    foreach ($probeMode in $probeModes) {
        $taskName = "CLC-Startup-Probe-$suffix-$probeMode"
        $resultPath = Join-Path $settings.controller.dataDirectory "$sentinel-$probeMode.json"
        $modeResult = [ordered]@{ passed=$false; errors=@() }
        $stage = 'prepare-task'
        try {
            $actionArguments = '-NoProfile -NonInteractive -WindowStyle Hidden -File "{0}" -Config "{1}" -ResultPath "{2}" -ProbeFile "{3}"' -f $worker,$localSettings.Path,$resultPath,$probeFile
            if ($probeMode -eq 'Password') { $actionArguments+=' -RequirePAT' }
            $action = New-ScheduledTaskAction -Execute $pwshPath -Argument $actionArguments -WorkingDirectory $script:RunnerRoot -ErrorAction Stop
            $principal = New-ScheduledTaskPrincipal -UserId $identity.Name -LogonType $probeMode -RunLevel $settings.controller.startupRunLevel -ErrorAction Stop
            $trigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddHours(1) -ErrorAction Stop
            $taskSettings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::FromMinutes(12)) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ErrorAction Stop
            $task = New-ScheduledTask -Action $action -Principal $principal -Trigger $trigger -Settings $taskSettings -ErrorAction Stop
            $stage = 'register-task'
            if ($probeMode -eq 'Password') { Register-RunnerPasswordTask $taskName $task $Credential }
            else { Register-ScheduledTask -TaskName $taskName -InputObject $task -ErrorAction Stop | Out-Null }
            $stage = 'start-task'
            Start-ScheduledTask -TaskName $taskName -ErrorAction Stop
            $stage = 'wait-for-result'
            $deadline = (Get-Date).AddMinutes(11)
            while (-not (Test-Path -LiteralPath $resultPath) -and (Get-Date) -lt $deadline) { Start-Sleep -Seconds 1 }
            if (Test-Path -LiteralPath $resultPath) {
                $modeResult = Get-Content -Raw -LiteralPath $resultPath | ConvertFrom-Json -AsHashtable
                $stage='wait-for-task-exit'
                while ([string](Get-ScheduledTask -TaskName $taskName -ErrorAction Stop).State -eq 'Running' -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 200 }
                $info=Get-ScheduledTaskInfo -TaskName $taskName -ErrorAction Stop
                $modeResult['taskResult']=$info.LastTaskResult
                if ($info.LastTaskResult -ne 0) {
                    $modeResult['passed']=$false
                    $modeResult.errors += "Probe process did not exit successfully; Task Scheduler result: $($info.LastTaskResult)"
                }
                if ($probeMode -eq 'Password' -and ($modeResult['pat'] -ne $true -or $modeResult['accountSid'] -ne $identity.User.Value -or
                    -not $modeResult.Contains('elevated') -or $modeResult['elevated'] -ne ($settings.controller.startupRunLevel -eq 'Highest') -or
                    -not $modeResult.Contains('sessionId') -or $modeResult['sessionId'] -ne 0 -or
                    -not $modeResult.Contains('interactive') -or $modeResult['interactive'] -ne $false)) {
                    $modeResult['passed']=$false
                    $modeResult.errors += 'Password probe must decrypt the saved PAT under the controller SID in noninteractive session 0 at the configured elevation level.'
                }
            }
            else { $info = Get-ScheduledTaskInfo -TaskName $taskName -ErrorAction Stop; $modeResult.errors += "Probe timed out; Task Scheduler result: $($info.LastTaskResult)" }
        } catch { $modeResult['passed']=$false; $modeResult.errors += ($stage + ': ' + $_.Exception.Message); $modeResult['failedStage'] = $stage }
        finally {
            $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
            if ($task) {
                try { Stop-ScheduledTask -TaskName $taskName -ErrorAction Stop; Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction Stop }
                catch { $modeResult['passed']=$false; $modeResult.errors += ('cleanup-task: ' + $_.Exception.Message) }
            }
            if (Test-Path -LiteralPath $resultPath) { Remove-Item -LiteralPath $resultPath }
        }
        $report.modes[$probeMode] = $modeResult
        Write-Host "$probeMode scheduled task: passed=$($modeResult.passed)"
    }
} finally {

    if (Test-Path -LiteralPath $probeFile) { Remove-Item -LiteralPath $probeFile }
    $reportPath = Join-Path $settings.controller.dataDirectory 'unattended-validation.json'
    $report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $reportPath -Encoding utf8NoBOM
    Write-Host "Unattended validation report: $reportPath"
}
if ($Mode -eq 'Password') {
    if (-not $report.modes.Password.passed) { throw ('Password scheduled task did not pass: '+($report.modes.Password.errors -join '; ')+'. See unattended-validation.json for exact failures.') }
} elseif (-not $report.modes.Interactive.passed -and -not $report.modes.S4U.passed) { throw 'Neither scheduled task mode passed. See unattended-validation.json for exact failures.' }
