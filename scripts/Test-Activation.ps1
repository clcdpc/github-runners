#requires -Version 7.4
# Dependency-free behavioral tests: external systems are mocked, while actual
# configuration, diagnostic output and file locking use private scratch paths.
. (Join-Path $PSScriptRoot 'Common.ps1')
$repository = $script:RunnerRoot
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('clc-activation-test-' + [guid]::NewGuid().ToString('N'))
$script:activationAssertions = 0
function Assert-Activation($Condition, [string]$Message) {
    $script:activationAssertions++
    if (-not $Condition) { throw $Message }
}
function Assert-ActivationFailure([scriptblock]$Action, [string]$Pattern) {
    $caught = $null
    try { & $Action *>&1 | Out-Null } catch { $caught = $_.Exception.Message }
    Assert-Activation ($caught -and $caught -match $Pattern) "Expected failure matching '$Pattern'; got '$caught'"
}
$mocks = {
    $script:fixture = @{
        Config=(Join-Path $caseDirectory 'controller.json'); Binary=(Join-Path $caseDirectory 'controller.exe')
        Calls=[Collections.Generic.List[string]]::new(); BuildCurrent=$true; ImageCurrent=$true
        Configured=$true; Running=$false; Busy=$false; AuthError=''; StartupError=''; ValidationError=''
        Changed=$false; Health='listening'; Force=$false; Imported=$false; ImageError=''; RestartSupported=$false; StageError=''
    }
    [IO.File]::WriteAllText($fixture.Binary, 'mock controller')
    function Test-RunnerHost([string]$Config) { }
    function Ensure-RunnerHost([string]$Config) { }
    function Install-RunnerDependencies([string]$Config) { $fixture.Calls.Add('dependencies') }
    function Get-RunnerBuildState { [pscustomobject]@{ Current=$fixture.BuildCurrent; Binary=$fixture.Binary } }
    function Get-RunnerBinary { $fixture.Binary }
    function Get-RunnerScaleLockPath([string]$Config) { Join-Path $caseDirectory 'scale.lock' }
    function Build-RunnerController([switch]$Force) {
        $fixture.Calls.Add('build-controller'); $fixture.Force=[bool]$Force; $fixture.BuildCurrent=$true
        $fixture.Binary
    }
    function Get-RunnerImage([string]$Config) { @{ Id='test-image'; Version='test-version'; Reason=$(if ($fixture.ImageCurrent) { 'provisioning inputs unchanged' } else { 'provisioning inputs changed: guest/provision.sh' }) } }
    function Test-RunnerImageCurrent($Image) { $fixture.ImageCurrent }
    function Build-RunnerImage([string]$Config, [switch]$NoCache) { $fixture.Calls.Add('build-image'); if ($fixture.ImageError) { throw $fixture.ImageError }; $fixture.ImageCurrent=$true }
    function Set-RunnerAuthentication([string]$Config, [switch]$FromGitHubCLI) {
        $fixture.Calls.Add('set-token'); $fixture.Imported=[bool]$FromGitHubCLI
        [IO.File]::WriteAllText((Get-RunnerSettings $Config).Settings.controller.patFile, 'mock encrypted credential')
    }
    function Invoke-RunnerCommand([string]$Command,[string]$Config,[string[]]$ExtraArguments=@(),[switch]$CaptureOutput) {
        Assert-Activation ($Config -eq $fixture.Config) 'A nested command lost the custom -Config path'
        $fixture.Calls.Add($Command + ':' + ($ExtraArguments -join ','))
        if ($Command -eq 'check-auth' -and $fixture.AuthError) { throw $fixture.AuthError }
        if ($Command -eq 'setup') {
            if ($ExtraArguments -contains '--check') { return (@{current=$fixture.Configured} | ConvertTo-Json -Compress) }
            $fixture.Configured=$true
        }
    }
    function Get-RunnerRuntime([string]$Config) {
        $settings = (Get-RunnerSettings $Config).Settings
        $status = @{
            pid=123; nonce='per-process-nonce'; status=$fixture.Health
            restartSupported=$fixture.RestartSupported
            configHash=(Get-FileHash -LiteralPath $Config).Hash; binaryHash=(Get-FileHash -LiteralPath $fixture.Binary).Hash
            patHash= $(if (Test-Path -LiteralPath $settings.controller.patFile) { (Get-FileHash -LiteralPath $settings.controller.patFile).Hash } else { '' })
            imageId='test-image'
            runtimeHash=(Get-FileHash -LiteralPath $settings.hyperv.script -Algorithm SHA256).Hash
        }
        if ($fixture.Changed) { $status.configHash='old-configuration' }
        [pscustomobject]@{ Alive=$fixture.Running; Healthy=($fixture.Running -and $fixture.Health -eq 'listening'); Status=$status }
    }
    function Stop-RunnerController([string]$Config,[switch]$Wait) {
        $fixture.Calls.Add('stop')
        Assert-Activation $Wait 'Update did not request a confirmed graceful drain'
        if ($fixture.Busy) { throw 'Controller is still draining active jobs. No jobs were terminated.' }
        $fixture.Running=$false; $fixture.Changed=$false
    }
    function Start-RunnerController([string]$Config) { $fixture.Calls.Add('start'); $fixture.Running=$true }
    function Test-RunnerInstallation([string]$Config) { $fixture.Calls.Add('validate'); if ($fixture.ValidationError) { throw $fixture.ValidationError } }
    function Enable-RunnerStartup([string]$Config,[string]$Mode,[PSCredential]$Credential,[scriptblock]$PasswordTaskRegistrar) {
        $fixture.Calls.Add('startup'); if ($fixture.StartupError) { throw $fixture.StartupError }
        $fixture.SelectedStartupMode=$Mode
        $fixture.StartupCredentialUser=if ($Credential) { $Credential.UserName } else { '' }
        if ($PasswordTaskRegistrar) { & $PasswordTaskRegistrar }
        [pscustomobject]@{Mode='Interactive';Current=$true;Summary='At Windows sign-in'}
    }
    function Get-RunnerStartupSummary([string]$Config) { 'At Windows sign-in (account must remain signed in)' }
    function New-RunnerRestartRuntime { @{} }
    function Stage-RunnerControllerUpdate([string]$Config) { $fixture.Calls.Add('stage'); if ($fixture.StageError) { throw $fixture.StageError } }
    function Initialize-TestInstallation {
        $local = Initialize-RunnerConfig $fixture.Config
        [IO.File]::WriteAllText($local.Settings.controller.patFile, 'mock encrypted credential')
    }
    function Invoke-TestActivation { param([switch]$NoStartup,[switch]$ForceRebuild,[switch]$FromGitHubCLI)
        Invoke-RunnerActivation -Config $fixture.Config -NoStartup:$NoStartup -ForceRebuild:$ForceRebuild -FromGitHubCLI:$FromGitHubCLI *>&1 | Out-Null
    }
}
$cases = [ordered]@{
    'Explicit Password startup applies to a live controller instead of only staging' = {
        Initialize-TestInstallation;$fixture.RestartSupported=$true;$fixture.Running=$true
        Invoke-RunnerActivation -Config $fixture.Config -StartupMode Password *>&1 | Out-Null
        Assert-Activation (-not $fixture.Calls.Contains('stage') -and $fixture.Calls.Contains('startup')) 'Explicit Password startup was ignored by live staging'
        Assert-Activation ($fixture.SelectedStartupMode -eq 'Password') 'Activation lost the selected startup mode'
    }
    'A startup credential reaches setup instead of the live staging shortcut' = {
        Initialize-TestInstallation;$fixture.RestartSupported=$true;$fixture.Running=$true
        $account=[Security.Principal.WindowsIdentity]::GetCurrent().Name
        $credential=[PSCredential]::new($account,(ConvertTo-SecureString 'synthetic startup credential' -AsPlainText -Force))
        Invoke-RunnerActivation -Config $fixture.Config -StartupCredential $credential *>&1 | Out-Null
        Assert-Activation (-not $fixture.Calls.Contains('stage') -and $fixture.Calls.Contains('startup')) 'Startup credential was ignored by live staging'
        Assert-Activation ($fixture.StartupCredentialUser -eq $account) 'Activation lost the in-memory startup credential'
    }
    'Restart-capable busy controller stages without stopping or rebuilding an image' = {
        Initialize-TestInstallation;$fixture.RestartSupported=$true;$fixture.Running=$true;$fixture.Busy=$true;$fixture.BuildCurrent=$false
        Invoke-TestActivation
        Assert-Activation ($fixture.Running -and $fixture.Calls.Contains('stage')) 'Live Activate did not stage the candidate'
        foreach ($call in @('stop','start','build-image','startup')) { Assert-Activation (-not $fixture.Calls.Contains($call)) "Live staging performed $call" }
    }
    'Failed candidate staging preserves active jobs' = {
        Initialize-TestInstallation;$fixture.RestartSupported=$true;$fixture.Running=$true;$fixture.Busy=$true;$fixture.StageError='candidate regression validation failed'
        Assert-ActivationFailure { Invoke-TestActivation } 'candidate regression validation failed'
        Assert-Activation ($fixture.Running -and -not $fixture.Calls.Contains('stop')) 'Staging failure interrupted the live controller'
    }
    'Bootstrap reuses the account PAT and forwards registration after validation' = {
        Initialize-TestInstallation
        $broker=@{Calls=0}
        Invoke-RunnerActivation -Config $fixture.Config -StartupMode Password -TokenProvider { throw 'Unexpected PAT prompt' } -PasswordTaskRegistrar {
            Assert-Activation ($fixture.Calls.Contains('validate')) 'Administrator registration was requested before installation validation'
            $broker.Calls++
        } *> $null
        Assert-Activation ($broker.Calls -eq 1 -and -not $fixture.Calls.Contains('auth')) 'Bootstrap repeated authentication or lost its registration broker'
    }
    'Bootstrap startup failure does not launch a session-bound controller' = {
        Initialize-TestInstallation
        $fixture.StartupError='Access denied registering the boot task'
        Assert-ActivationFailure {
            Invoke-RunnerActivation -Config $fixture.Config -StartupMode Password -PasswordTaskRegistrar {}
        } 'Unattended startup failed during account setup'
        Assert-Activation (-not $fixture.Running -and -not $fixture.Calls.Contains('start')) 'Bootstrap fell back to a controller tied to the setup session'
    }
    'Empty bootstrap token stops before provisioning or startup' = {
        Initialize-TestInstallation
        Remove-Item -LiteralPath (Get-RunnerSettings $fixture.Config).Settings.controller.patFile
        Assert-ActivationFailure { Invoke-RunnerActivation -Config $fixture.Config -StartupMode Password -TokenProvider { [Security.SecureString]::new() } } 'nonempty secure PAT'
        Assert-Activation (-not $fixture.Calls.Contains('setup:--quiet') -and -not $fixture.Calls.Contains('startup') -and -not $fixture.Running) 'Empty PAT allowed provisioning or startup'
    }
    'Fresh configuration and missing PAT' = {
        $fixture.BuildCurrent=$false; $fixture.ImageCurrent=$false; $fixture.Configured=$false
        Invoke-TestActivation
        Assert-Activation (Test-Path -LiteralPath $fixture.Config) 'Fresh activation did not create configuration'
        Assert-Activation ($fixture.Calls.Contains('set-token')) 'Missing PAT did not use secure token workflow'
        Assert-Activation ($fixture.Calls.Contains('build-controller') -and $fixture.Calls.Contains('build-image')) 'Fresh installation skipped a build'
        Assert-Activation ($fixture.Calls.IndexOf('check-auth:') -lt $fixture.Calls.IndexOf('build-image')) 'Image built before permission validation'
        Assert-Activation $fixture.Running 'Fresh controller was not started'
    }
    'Existing valid installation and repeat activation' = {
        Initialize-TestInstallation
        $original = [IO.File]::ReadAllText($fixture.Config)
        Invoke-TestActivation
        $fixture.Calls.Clear()
        Invoke-TestActivation
        Assert-Activation ([IO.File]::ReadAllText($fixture.Config) -ceq $original) 'Existing configuration changed'
        foreach ($call in @('set-token','build-controller','build-image','stop','start','setup:--quiet')) {
            Assert-Activation (-not $fixture.Calls.Contains($call)) "No-op activation performed $call"
        }
    }
    'GitHub 401 retains token and stops early' = {
        Initialize-TestInstallation; $fixture.AuthError='GitHub HTTP 401: invalid or expired PAT; run SetToken'
        $pat = (Get-RunnerSettings $fixture.Config).Settings.controller.patFile; $before=(Get-FileHash $pat).Hash
        Assert-ActivationFailure { Invoke-TestActivation } '401.*SetToken'
        Assert-Activation ((Get-FileHash $pat).Hash -eq $before -and -not $fixture.Calls.Contains('set-token')) 'Invalid saved PAT was overwritten'
        Assert-Activation (-not $fixture.Calls.Contains('build-image') -and -not $fixture.Calls.Contains('startup')) 'Authentication failure did not stop activation'
    }
    'GitHub 403 precedes expensive image construction' = {
        Initialize-TestInstallation; $fixture.ImageCurrent=$false; $fixture.AuthError='GitHub HTTP 403: organization permission denied; check approval and Self-hosted runners write'
        Assert-ActivationFailure { Invoke-TestActivation } '403.*permission'
        Assert-Activation (-not $fixture.Calls.Contains('build-image')) '403 did not fence image construction'
    }
    'Explicit CLI import for missing credential' = {
        Invoke-TestActivation -FromGitHubCLI
        Assert-Activation $fixture.Imported 'Explicit GitHub CLI import was not forwarded'
    }
    'Existing credential preserved with CLI option' = {
        Initialize-TestInstallation; Invoke-TestActivation -FromGitHubCLI
        Assert-Activation (-not $fixture.Calls.Contains('set-token')) 'Existing PAT unexpectedly replaced by CLI import'
    }
    'Changed controller safely drains before rebuild' = {
        Initialize-TestInstallation; $fixture.BuildCurrent=$false; $fixture.Running=$true
        Invoke-TestActivation
        Assert-Activation ($fixture.Calls.IndexOf('stop') -lt $fixture.Calls.IndexOf('build-controller')) 'Controller rebuilt before draining'
        Assert-Activation $fixture.Calls.Contains('start') 'Updated controller was not restarted'
    }
    'Image reuse and rebuild reasons are visible and logged' = {
        Initialize-TestInstallation
        $output=Invoke-RunnerActivation -Config $fixture.Config *>&1
        Assert-Activation (($output -join "`n") -match 'Reusing existing image test-version.*provisioning inputs unchanged') 'Reuse reason was hidden from the operator'
        $fixture.ImageCurrent=$false
        $output=Invoke-RunnerActivation -Config $fixture.Config *>&1
        Assert-Activation (($output -join "`n") -match 'Building runner image.*guest/provision.sh') 'Specific provisioning change was hidden'
        $logs=Get-Content -Raw -LiteralPath (Get-ChildItem (Join-Path $caseDirectory 'logs') -Filter 'activation-*.log').FullName
        Assert-Activation (($logs -join "`n") -match 'Reusing existing image' -and ($logs -join "`n") -match 'guest/provision.sh') 'Image decisions were absent from activation diagnostics'
    }
    'Cloud-init verification error survives activation summary' = {
        Initialize-TestInstallation; $fixture.ImageCurrent=$false
        $fixture.ImageError='guest cloud-init/tool provisioning failed: CLC_VERIFICATION_FAILED exit=43 line=250 command=fixture-verify; image diagnostics: images/candidate.log'
        $output=[Collections.Generic.List[string]]::new(); $caught=''
        try { Invoke-RunnerActivation -Config $fixture.Config *>&1 | ForEach-Object { $output.Add([string]$_) } } catch { $caught=$_.Exception.Message }
        Assert-Activation ($caught -match 'exit=43.*fixture-verify') 'Specific guest command/exit status was lost'
        Assert-Activation (($output -join "`n") -match 'Failure: Runner image:.*exit=43.*images/candidate.log') 'Activation summary omitted the actual failure and image log'
        Assert-Activation (-not $fixture.Running -and -not $fixture.Calls.Contains('setup:--quiet')) 'Failed provisioning started later lifecycle operations'
    }
    'Changed image inputs trigger image rebuild only' = {
        Initialize-TestInstallation; $fixture.ImageCurrent=$false; $fixture.Running=$true
        Invoke-TestActivation
        Assert-Activation ($fixture.Calls.Contains('build-image') -and -not $fixture.Calls.Contains('build-controller')) 'Image-only change rebuilt the wrong artifacts'
        Assert-Activation ($fixture.Calls.IndexOf('stop') -lt $fixture.Calls.IndexOf('build-image')) 'Image rebuilt before draining'
    }
    'Configuration change during image inspection invalidates the reuse decision' = {
        Initialize-TestInstallation
        $settings=Read-RunnerJson $fixture.Config; $settings.hyperv.diskGiB=100; Write-RunnerJson $fixture.Config $settings
        function Get-RunnerImage([string]$Config) {
            $settings=Read-RunnerJson $Config; $settings.hyperv.diskGiB=20; Write-RunnerJson $Config $settings
            @{Id='test-image';Version='test-version';Current=$true;Reason='provisioning inputs unchanged'}
        }
        Assert-ActivationFailure { Invoke-TestActivation } 'Configuration changed during activation.*rerun Activate'
        Assert-Activation (-not $fixture.Calls.Contains('startup') -and -not $fixture.Calls.Contains('start') -and -not $fixture.Calls.Contains('setup:--check')) 'A stale image decision reached infrastructure, startup or controller operations'
    }
    'Configuration change during image build stops later activation steps' = {
        Initialize-TestInstallation; $fixture.ImageCurrent=$false
        function Build-RunnerImage([string]$Config, [switch]$NoCache) {
            $fixture.Calls.Add('build-image'); $fixture.ImageCurrent=$true
            $settings=Read-RunnerJson $Config; $settings.hyperv.diskGiB=100; Write-RunnerJson $Config $settings
        }
        Assert-ActivationFailure { Invoke-TestActivation } 'Configuration changed during activation.*rerun Activate'
        Assert-Activation (-not $fixture.Calls.Contains('setup:--check') -and -not $fixture.Calls.Contains('start')) 'A changed configuration continued after image construction'
    }
    'Configuration change during startup validation prevents controller launch' = {
        Initialize-TestInstallation
        function Enable-RunnerStartup([string]$Config,[string]$Mode,[PSCredential]$Credential) {
            $fixture.Calls.Add('startup')
            $settings=Read-RunnerJson $Config; $settings.hyperv.diskGiB=100; Write-RunnerJson $Config $settings
        }
        Assert-ActivationFailure { Invoke-TestActivation } 'Configuration changed during activation.*rerun Activate'
        Assert-Activation (-not $fixture.Calls.Contains('start')) 'Controller started after its configuration changed during startup validation'
    }
    'ForceRebuild rebuilds both artifacts' = {
        Initialize-TestInstallation; $fixture.Running=$true
        Invoke-TestActivation -ForceRebuild
        Assert-Activation ($fixture.Force -and $fixture.Calls.Contains('build-image')) 'ForceRebuild did not force both builds'
    }
    'NoStartup preserves current registration' = {
        Initialize-TestInstallation; Invoke-TestActivation -NoStartup
        Assert-Activation (-not $fixture.Calls.Contains('startup')) 'NoStartup registered a task'
        Assert-Activation $fixture.Running 'NoStartup did not start controller'
    }
    'Active jobs block update without replacing artifacts' = {
        Initialize-TestInstallation; $fixture.Running=$true; $fixture.Busy=$true; $fixture.BuildCurrent=$false
        Assert-ActivationFailure { Invoke-TestActivation } 'draining active jobs'
        Assert-Activation ($fixture.Running -and -not $fixture.Calls.Contains('build-controller') -and -not $fixture.Calls.Contains('build-image')) 'Busy controller was modified'
    }
    'Changed configuration requires a graceful restart' = {
        Initialize-TestInstallation; $fixture.Running=$true; $fixture.Changed=$true
        Invoke-TestActivation
        Assert-Activation ($fixture.Calls.Contains('stop') -and $fixture.Calls.Contains('start')) 'Changed running inputs were not applied'
    }
    'Hardware-only update reuses image and preserves startup registration' = {
        Initialize-TestInstallation; $fixture.Running=$true; $fixture.Changed=$true
        Invoke-TestActivation -NoStartup
        Assert-Activation ($fixture.Calls.Contains('stop') -and $fixture.Calls.Contains('start')) 'Hardware-only change was not restarted after drain'
        Assert-Activation (-not $fixture.Calls.Contains('build-image') -and -not $fixture.Calls.Contains('startup')) 'Hardware-only update rebuilt the base or replaced startup'
    }
    'Busy runner blocks hardware-only activation before artifact replacement' = {
        Initialize-TestInstallation; $fixture.Running=$true; $fixture.Changed=$true; $fixture.Busy=$true
        Assert-ActivationFailure { Invoke-TestActivation -NoStartup } 'draining active jobs'
        Assert-Activation ($fixture.Running -and -not $fixture.Calls.Contains('start') -and -not $fixture.Calls.Contains('build-image')) 'Busy runner was restarted for hardware-only settings'
    }
    'Infrastructure repair resumes from actual state' = {
        Initialize-TestInstallation; $fixture.Configured=$false; $fixture.ValidationError='injected validation failure'
        Assert-ActivationFailure { Invoke-TestActivation } 'injected validation'
        Assert-Activation $fixture.Configured 'Successful provisioning was rolled back'
        $fixture.ValidationError=''; $fixture.Calls.Clear(); Invoke-TestActivation
        Assert-Activation (-not $fixture.Calls.Contains('setup:--quiet') -and -not $fixture.Calls.Contains('build-image')) 'Resume repeated completed work'
    }
    'Startup failure is partial success with an error' = {
        Initialize-TestInstallation; $fixture.StartupError='Access denied registering Interactive task; inspect Task Scheduler'
        Assert-ActivationFailure { Invoke-TestActivation } 'Partial activation.*startup failed'
        Assert-Activation $fixture.Running 'Startup failure prevented interactive controller operation'
    }
    'Retrying process is not activation success' = {
        Initialize-TestInstallation; $fixture.Health='retrying'
        Assert-ActivationFailure { Invoke-TestActivation } 'retrying, not listening'
    }
    'Secrets are redacted in errors and diagnostics' = {
        Initialize-TestInstallation; $fixture.AuthError='GitHub HTTP 403 github_pat_SYNTHETIC_SECRET_TOKEN Bearer synthetic-secret'
        Assert-ActivationFailure { Invoke-TestActivation } '403.*REDACTED'
        $logs = Get-Content -Raw -LiteralPath (Get-ChildItem (Join-Path $caseDirectory 'logs') -Filter 'activation-*.log').FullName
        Assert-Activation ($logs -notmatch 'SYNTHETIC_SECRET_TOKEN|synthetic-secret') 'Credential escaped into diagnostics'
    }
}
try {
    Protect-RunnerDirectory $scratch
    foreach ($entry in $cases.GetEnumerator()) {
        & {
            . (Join-Path $repository 'scripts/Common.ps1')
            . (Join-Path $repository 'scripts/Dependencies.ps1')
            . (Join-Path $repository 'scripts/Startup.ps1')
            . (Join-Path $repository 'scripts/Activate.ps1')
            $caseDirectory = Join-Path $scratch ([guid]::NewGuid().ToString('N'))
            New-Item -ItemType Directory $caseDirectory | Out-Null
            . $mocks
            & $entry.Value
        }
    }
    Write-Host "Activate orchestration: $($cases.Count) scenarios and $activationAssertions assertions passed."
} finally {
    $absolute = [IO.Path]::GetFullPath($scratch)
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if ($absolute.StartsWith($tempRoot,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf).StartsWith('clc-activation-test-')) { Remove-Item -LiteralPath $absolute -Recurse -Force -ErrorAction SilentlyContinue }
}
