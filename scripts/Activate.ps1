#requires -Version 7.4

function Test-RunnerCanPrompt {
    [Environment]::UserInteractive -and -not [Console]::IsInputRedirected -and
        -not (@([Environment]::GetCommandLineArgs() | Where-Object { $_ -match '^[-/]noni' }).Count)
}

function Set-RunnerAuthentication([string]$Config, [switch]$FromGitHubCLI) {
    if (-not $FromGitHubCLI -and -not (Test-RunnerCanPrompt)) {
        throw 'PAT missing in a noninteractive session. Run runners.ps1 SetToken interactively under the controller account with the same -Config, or explicitly select Activate -FromGitHubCLI.'
    }
    & (Join-Path $script:RunnerRoot 'scripts/Set-RunnerToken.ps1') -Config $Config -FromGitHubCLI:$FromGitHubCLI
}

function Assert-RunnerActivationConfig([string]$Config, [string]$ConfigHash) {
    if (-not (Test-Path -LiteralPath $Config) -or (Get-FileHash -LiteralPath $Config -Algorithm SHA256).Hash -ne $ConfigHash) {
        throw 'Configuration changed during activation. Finish saving the configuration, then rerun Activate; completed work is preserved.'
    }
}

function Invoke-RunnerActivationStep([int]$Number, [string]$Name, [string]$Result, [scriptblock]$Action, [string]$LogPath, [switch]$ShowProgress, [string]$Config, [string]$ConfigHash) {
    Write-Host ("[{0}/8] {1} ... " -f $Number,$Name) -NoNewline
    try {
        Assert-RunnerActivationConfig $Config $ConfigHash
        & $Action *>&1 | ForEach-Object {
            $line = Protect-RunnerText ([string]$_)
            Add-Content -LiteralPath $LogPath -Value $line -Encoding utf8NoBOM
            if ($ShowProgress) { Write-Host $line }
        }
        Assert-RunnerActivationConfig $Config $ConfigHash
        Write-Host $Result
    } catch {
        Write-Host 'Failed'
        $message = Protect-RunnerText $_.Exception.Message
        Add-Content -LiteralPath $LogPath -Value ("${Name}: $message") -Encoding utf8NoBOM
        throw "${Name}: $message"
    }
}

function Write-RunnerActivationSummary([string]$Config, [string]$Startup) {
    $settings = (Get-RunnerSettings $Config).Settings
    $runtime = Get-RunnerRuntime $Config
    $status = if ($runtime.Alive) { $runtime.Status['status'] } else { 'Not running' }
    Write-Host ''
    Write-Host "Organization: $($settings.github.organizationUrl)"
    Write-Host "Runner group: $($settings.github.runnerGroup)"
    Write-Host "Scale set: $($settings.github.scaleSet)"
    Write-Host "Maximum runners: $($settings.runners.maximum)"
    Write-Host "VM resources: $($settings.hyperv.cpus) vCPUs, $($settings.hyperv.memoryGiB) GiB static RAM, $($settings.hyperv.diskGiB) GiB logical disk"
    Write-Host "Controller: $status"
    Write-Host "Startup: $Startup"
}

function Invoke-RunnerActivation(
    [string]$Config = 'local/controller.json', [switch]$NoStartup, [switch]$ForceRebuild,
    [switch]$FromGitHubCLI, [switch]$NoCache,
    [ValidateSet('Auto','S4U','Interactive','Password')][string]$StartupMode = 'Auto',
    [PSCredential]$StartupCredential,
    [scriptblock]$TokenProvider, [switch]$RefreshAuthentication,
    [scriptblock]$PasswordTaskRegistrar
) {
    if ($RefreshAuthentication -and -not $TokenProvider) { throw 'Refreshing authentication requires a token provider.' }
    if ($TokenProvider -and $FromGitHubCLI) { throw 'Choose either a token provider or GitHub CLI authentication.' }
    Write-Host 'CLC GitHub Runners - Activation'
    $localSettings = Initialize-RunnerConfig $Config
    $Config = $localSettings.Path
    $settings = $localSettings.Settings
    $logPath = Join-Path $settings.controller.logDirectory ('activation-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N').Substring(0,8) + '.log')
    $stepArguments = @{LogPath=$logPath;Config=$Config;ConfigHash=(Get-FileHash -LiteralPath $Config -Algorithm SHA256).Hash}
    $startup = 'Not checked'
    $startupError = $null
    try {
        $live = Get-RunnerRuntime $Config
        if ($live.Alive -and $live.Status['restartSupported'] -and -not $ForceRebuild -and -not $NoCache -and -not $RefreshAuthentication -and $StartupMode -eq 'Auto' -and -not $StartupCredential) {
            Stage-RunnerControllerUpdate $Config
            return
        }
        Invoke-RunnerActivationStep 1 'Prerequisites' 'Ready' {
            Install-RunnerDependencies -Config $Config
            Test-RunnerHost $Config
        } @stepArguments

        $build = Get-RunnerBuildState
        $rebuild = $ForceRebuild -or -not $build.Current
        Invoke-RunnerActivationStep 2 'Controller installation' $(if ($rebuild) {'Built'} else {'Current'}) {
            if ($rebuild) {
                Stop-RunnerController $Config -Wait
                Invoke-RunnerStopped $Config { $null = Build-RunnerController -Force:$ForceRebuild }
            }
            Ensure-RunnerHost $Config
        } @stepArguments

        Invoke-RunnerActivationStep 3 'GitHub authentication' 'Valid (organization read and runner write)' {
            if ($TokenProvider -and ($RefreshAuthentication -or -not (Test-Path -LiteralPath $settings.controller.patFile))) {
                $token = & $TokenProvider
                try {
                    if ($token -isnot [Security.SecureString] -or -not $token.Length) { throw 'A nonempty secure PAT is required.' }
                    & (Join-Path $script:RunnerRoot 'scripts/Set-RunnerToken.ps1') -Config $Config -Token $token
                } finally { if ($token -is [Security.SecureString]) { $token.Dispose() }; $token = $null }
            } elseif (-not (Test-Path -LiteralPath $settings.controller.patFile)) {
                Set-RunnerAuthentication -Config $Config -FromGitHubCLI:$FromGitHubCLI
            }
            Invoke-RunnerCommand check-auth $Config
        } @stepArguments

        $image = Get-RunnerImage $Config
        $rebuildImage = $ForceRebuild -or $NoCache -or -not (Test-RunnerImageCurrent $image)
        Invoke-RunnerActivationStep 4 'Runner image' $(if ($rebuildImage) {'Built'} else {'Reused'}) {
            $reason = if ($ForceRebuild) { 'explicit -ForceRebuild requested' } elseif ($NoCache) { 'explicit -NoCache requested' } elseif (-not $image) { 'no published base image exists' } else { $image.Reason }
            if (-not $reason) { $reason = if ($rebuildImage) { 'provisioning fingerprint changed' } else { 'provisioning inputs unchanged' } }
            Write-Host ''
            if ($rebuildImage) { Write-Host "Building runner image: $reason" } else { Write-Host "Reusing existing image $($image.Version): $reason; VHDX SHA256 verified." }
            if ($rebuildImage) {
                Stop-RunnerController $Config -Wait
                Build-RunnerImage -Config $Config -NoCache:($NoCache -or $ForceRebuild)
            }
        } @stepArguments -ShowProgress
        if ($rebuildImage) { $image = Get-RunnerImage $Config }

        $infrastructure = Invoke-RunnerCommand setup $Config @('--check') -CaptureOutput | ConvertFrom-Json
        Invoke-RunnerActivationStep 5 'GitHub infrastructure' $(if ($infrastructure.current) {'Current'} else {'Configured'}) {
            if (-not $infrastructure.current) {
                Stop-RunnerController $Config -Wait
                Invoke-RunnerCommand setup $Config @('--quiet')
                $verified = Invoke-RunnerCommand setup $Config @('--check') -CaptureOutput | ConvertFrom-Json
                if (-not $verified.current) { throw 'GitHub setup did not converge. Inspect configuration and controller logs, then rerun Activate.' }
            }
        } @stepArguments

        Invoke-RunnerActivationStep 6 'Installation validation' 'Ready' { Test-RunnerInstallation $Config } @stepArguments
        $null = New-RunnerRestartRuntime

        if ($NoStartup) {
            try { $startup = Get-RunnerStartupSummary $Config } catch { $startup = 'Not inspected' }
            Write-Host '[7/8] Automatic startup ... Skipped (-NoStartup; existing registration preserved)'
        } else {
            try {
                Invoke-RunnerActivationStep 7 'Automatic startup' 'Ready' {
                    $startupArguments=@{Config=$Config;Mode=$StartupMode}
                    if ($StartupCredential) { $startupArguments['Credential']=$StartupCredential }
                    if ($PasswordTaskRegistrar) { $startupArguments['PasswordTaskRegistrar']=$PasswordTaskRegistrar }
                    $null = Enable-RunnerStartup @startupArguments
                } @stepArguments
                $startup = Get-RunnerStartupSummary $Config
            } catch {
                $startupError = Protect-RunnerText $_.Exception.Message
                $startup = 'FAILED - ' + $startupError
            }
        }

        if ($startupError -and $PasswordTaskRegistrar) { throw ('Unattended startup failed during account setup: '+$startupError) }

        Invoke-RunnerActivationStep 8 'Controller' 'Listening' {
            $runtime = Get-RunnerRuntime $Config
            if ($runtime.Alive) {
                $expected = @{
                    configHash=(Get-FileHash -LiteralPath $Config -Algorithm SHA256).Hash
                    patHash=(Get-FileHash -LiteralPath $settings.controller.patFile -Algorithm SHA256).Hash
                    binaryHash=(Get-FileHash -LiteralPath (Get-RunnerBinary) -Algorithm SHA256).Hash
                    imageId=$image['Id']
                    runtimeHash=(Get-FileHash -LiteralPath $settings.hyperv.script -Algorithm SHA256).Hash
                }
                $changed = @($expected.Keys | Where-Object { $runtime.Status[$_] -ne $expected[$_] }).Count -gt 0
                if ($changed -or $runtime.Status['status'] -eq 'draining') { Stop-RunnerController $Config -Wait }
            }
            if (-not (Get-RunnerRuntime $Config).Alive) { Start-RunnerController $Config }
            $null = Wait-RunnerHealthy $Config
        } @stepArguments

        if ($startupError) { throw 'Partial activation: controller is listening, but requested automatic startup failed. Correct the startup error above and rerun Activate; use -NoStartup only when sign-in startup is not requested.' }
        Write-RunnerActivationSummary $Config $startup
        Write-Host ''
        Write-Host 'Activation complete.'
        Write-Host "Diagnostics: $logPath"
    } catch {
        $activationError = Protect-RunnerText $_.Exception.Message
        try { Write-RunnerActivationSummary $Config $startup } catch { Write-Verbose 'Runtime summary unavailable; see the activation error.' }
        Write-Host ''
        Write-Host "Failure: $activationError"
        Write-Host 'Activation incomplete. Correct the reported issue and rerun the same Activate command; completed work is preserved.'
        Write-Host "Diagnostics: $logPath"
        throw $activationError
    }
}
