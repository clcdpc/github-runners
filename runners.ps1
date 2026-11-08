#requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Position=0, Mandatory)]
    [ValidateSet('Activate','Install','Validate','PrepareHost','PrepareImage','BuildImage','SetToken','Setup','Start','Stop','RestartWhenIdle','Status','Jobs','Cleanup','CleanupImages','Test','TestHyperV','TestRace','TestUnattended','EnableStartup','DisableStartup','DeleteScaleSet')]
    [string]$Command,
    [string]$Config = 'local/controller.json',
    [switch]$Foreground,
    [switch]$Json,
    [switch]$FromGitHubCLI,
    [switch]$NoCache,
    [switch]$NoStartup,
    [switch]$ForceRebuild,
    [ValidateSet('Auto','S4U','Interactive','Password')][string]$StartupMode = 'Auto',
    [PSCredential]$StartupCredential,
    [string]$ConfirmName
)
. (Join-Path $PSScriptRoot 'scripts/Common.ps1')
. (Join-Path $PSScriptRoot 'scripts/Dependencies.ps1')
. (Join-Path $PSScriptRoot 'scripts/Host.ps1')
. (Join-Path $PSScriptRoot 'scripts/Startup.ps1')
. (Join-Path $PSScriptRoot 'scripts/Activate.ps1')
. (Join-Path $PSScriptRoot 'scripts/Restart.ps1')
$managementLock = $null
try {
    if ($Json -and $Command -ne 'Jobs') { throw '-Json is supported only with Jobs.' }
    if ($Command -in @('Activate','Install','PrepareHost','PrepareImage','BuildImage','SetToken','Setup','Start','Cleanup','CleanupImages','DeleteScaleSet','TestHyperV','TestRace','TestUnattended','EnableStartup','DisableStartup')) {
        $managementLock = Enter-RunnerFileLock (Join-Path $PSScriptRoot 'bin/management.lock')
    }
    switch ($Command) {
    'Activate' {
        $activationArguments=@{Config=$Config;NoStartup=$NoStartup;ForceRebuild=$ForceRebuild;FromGitHubCLI=$FromGitHubCLI;NoCache=$NoCache;StartupMode=$StartupMode}
        if ($StartupCredential) { $activationArguments['StartupCredential']=$StartupCredential }
        Invoke-RunnerActivation @activationArguments
    }
    'Install' {
        Install-RunnerDependencies -Config $Config
        $null = Initialize-RunnerConfig $Config
        $null = Build-RunnerController
        Invoke-RunnerCommand validate $Config
    }
    'SetToken' { & (Join-Path $PSScriptRoot 'scripts/Set-RunnerToken.ps1') -Config $Config -FromGitHubCLI:$FromGitHubCLI }
    'BuildImage' { $null=Initialize-RunnerConfig $Config; Build-RunnerImage -Config $Config -NoCache:$NoCache }
    'PrepareImage' { $null=Initialize-RunnerConfig $Config; Build-RunnerImage -Config $Config -NoCache:($NoCache -or $ForceRebuild) }
    'PrepareHost' { $null=Initialize-RunnerConfig $Config; Initialize-RunnerHost $Config }
    'CleanupImages' { Invoke-RunnerCommand prune-images $Config }
    'Validate' { Test-RunnerInstallation $Config }
    'Setup' { Invoke-RunnerCommand setup $Config }
    'Start' {
        $null=Initialize-RunnerConfig $Config
        if ($Foreground) {
            # Serialize any compilation, then let the controller's own locks
            # protect its lifetime. Activate/Stop must remain usable while a
            # foreground listener is running.
            if (-not (Get-RunnerRuntime $Config).Alive) { $null = Build-RunnerController }
            $managementLock.Dispose(); $managementLock=$null
        }
        Start-RunnerController -Config $Config -Foreground:$Foreground
    }
    'Stop' { Stop-RunnerController $Config }
    'RestartWhenIdle' { Request-RunnerRestart $Config }
    'Status' { Invoke-RunnerCommand status $Config }
    'Jobs' {
        if ($Json) { Invoke-RunnerCommand jobs $Config @('--json') -CaptureOutput }
        else { Invoke-RunnerCommand jobs $Config }
    }
    'Cleanup' { Invoke-RunnerCommand cleanup $Config }
    'DeleteScaleSet' { Invoke-RunnerCommand delete-scale-set $Config @('--confirm-name', $ConfirmName) }
    'Test' {
        $goPath = Get-RunnerGo
        Push-Location $PSScriptRoot
        try {
            & $goPath test -count=1 -cover ./...
            if ($LASTEXITCODE) { throw 'Automated tests failed.' }
            & $goPath vet ./...
            if ($LASTEXITCODE) { throw 'Go vet failed.' }
            & (Join-Path $PSScriptRoot 'scripts/Test-Scripts.ps1')
        } finally { Pop-Location }
    }
    'TestHyperV' {
        $null=Initialize-RunnerConfig $Config
        Write-Host 'RESOURCE-INTENSIVE: disposable Ubuntu VM, Docker builds, SQL Server 2022 and Chromium validation; allow up to 45 minutes.'
        Ensure-RunnerHost $Config
        Invoke-RunnerCommand test-hyperv $Config
    }
    'TestUnattended' {
        if ($StartupMode -eq 'Password' -or $StartupCredential) { Test-RunnerUnattended -Config $Config -Mode Password -Credential $StartupCredential }
        else { Test-RunnerUnattended -Config $Config }
    }
    'TestRace' { & (Join-Path $PSScriptRoot 'scripts/Test-Race.ps1') -Config $Config }
    'EnableStartup' {
        $result = Enable-RunnerStartup -Config $Config -Mode $StartupMode -Credential $StartupCredential
        if ($result.Mode -eq 'Password') {
            $null=New-RunnerRestartRuntime
            Start-RunnerController -Config $Config -Installed
        }
        Write-Host "Startup ($($result.Mode)): $($result.Summary). Current=$($result.Current)"
    }
    'DisableStartup' { Disable-RunnerStartup $Config }
}
} catch {
    $message = "${Command} failed: " + (Protect-RunnerText $_.Exception.Message)
    if ($Command -eq 'Jobs' -and $Json) { Write-Error $message -ErrorAction Continue }
    else { Write-Host $message -ForegroundColor Red }
    exit 1
} finally { if ($managementLock) { $managementLock.Dispose() } }
exit 0
