#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Dependencies.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
. (Join-Path $PSScriptRoot 'Activate.ps1')
$repository = $script:RunnerRoot
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('clc-activation-helper-' + [guid]::NewGuid().ToString('N'))
$script:helperAssertions = 0
function Assert-Helper($Condition, [string]$Message) {
    $script:helperAssertions++
    if (-not $Condition) { throw $Message }
}
try {
    Protect-RunnerDirectory $scratch
    $configPath = Join-Path $scratch 'controller.json'
    $local = Initialize-RunnerConfig $configPath
    [IO.File]::WriteAllText($local.Settings.controller.patFile, 'encrypted test fixture')

    # Real controller/image fingerprints detect edits even when checkout tools
    # preserve mtimes, and detect deleted/added inputs. Test-only Go changes do
    # not invalidate a production build.
    & {
        $script:RunnerRoot = $scratch
        $realGo = Join-Path $repository 'tools/go/bin/go.exe'
        function Get-RunnerGo { $realGo }
        foreach ($directory in @('cmd','internal','image','bin')) { New-Item -ItemType Directory (Join-Path $scratch $directory) | Out-Null }
        foreach ($name in @('go.mod','go.sum','cmd/main.go','internal/code.go','image/Dockerfile')) { [IO.File]::WriteAllText((Join-Path $scratch $name), $name) }
        $binary = Get-RunnerBinary
        [IO.File]::WriteAllText($binary, 'compiled fixture')
        $state = Get-RunnerBuildState
        Assert-Helper (-not $state.Current) 'Unknown binary was incorrectly assumed current'
        Write-RunnerJson ($binary + '.build.json') @{inputs=$state.Fingerprint;binary=(Get-FileHash $binary).Hash}
        Assert-Helper (Get-RunnerBuildState).Current 'Matching binary/input hashes were not reused'
        $source = Join-Path $scratch 'cmd/main.go'
        $stamp = (Get-Item $source).LastWriteTimeUtc
        [IO.File]::WriteAllText($source, 'changed code'); (Get-Item $source).LastWriteTimeUtc=$stamp
        Assert-Helper (-not (Get-RunnerBuildState).Current) 'Same-mtime source change was missed'
        [IO.File]::WriteAllText($source, 'cmd/main.go')
        [IO.File]::WriteAllText((Join-Path $scratch 'cmd/main_test.go'), 'test-only change')
        Assert-Helper (Get-RunnerBuildState).Current 'Test-only source caused an unnecessary rebuild'
        $deleted = Join-Path $scratch 'internal/code.go'
        Remove-Item -LiteralPath $deleted
        Assert-Helper (-not (Get-RunnerBuildState).Current) 'Deleted source was not detected'
        [IO.File]::WriteAllText($deleted, 'internal/code.go')
        [IO.File]::WriteAllText($binary, 'replaced binary')
        Assert-Helper (-not (Get-RunnerBuildState).Current) 'Changed executable was trusted by stale metadata'
        Assert-Helper (Test-RunnerImageCurrent @{Current=$true}) 'Current immutable image was not reused'
        Assert-Helper (-not (Test-RunnerImageCurrent @{Current=$false})) 'Changed image recipe was trusted'
        Assert-Helper (-not (Test-RunnerImageCurrent $null)) 'Missing base image was trusted'
    }
    $script:RunnerRoot = $repository

    & {
        function Invoke-RunnerCommand { param($Command,$Config) }
        function Ensure-RunnerHost { param($Config) }
        $imageState=@{Image=$null}
        function Get-RunnerImage { param($Config) $imageState.Image }
        $message=''
        try { Test-RunnerInstallation $configPath *> $null } catch { $message=$_.Exception.Message }
        Assert-Helper ($message -match 'Ubuntu base image missing') 'Installation validation accepted a missing image'
        $imageState.Image=@{Current=$false;Reason='diskGiB changed from 100 to 20; provisioning inputs changed: diskGiB'}
        $message=''
        try { Test-RunnerInstallation $configPath *> $null } catch { $message=$_.Exception.Message }
        Assert-Helper ($message -match 'diskGiB changed from 100 to 20' -and $message -match 'rerun Activate') 'Installation validation accepted an incompatible image or lost the rebuild reason'
        $imageState.Image=@{Current=$true;Reason='provisioning inputs unchanged'}
        Test-RunnerInstallation $configPath *> $null
    }

    & {
        $depRoot=Join-Path $scratch 'dependencies'
        New-Item -ItemType Directory $depRoot | Out-Null
        $script:RunnerRoot=$depRoot
        $depConfig=Join-Path $depRoot 'controller.json'
        $depSettings=Get-Content -Raw $configPath | ConvertFrom-Json
        $depSettings | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $depConfig -Encoding utf8NoBOM
        $script:downloads=0; $script:badChecksum=$false; $script:interruptExtraction=$false
        function Get-RunnerGoVersion([string]$GoPath) { [IO.File]::ReadAllText($GoPath) }
        function Invoke-WebRequest([string]$Uri,[string]$OutFile) {
            Assert-Helper ($Uri -eq 'https://go.dev/dl/go1.27.3.windows-amd64.zip') 'SDK download did not use the pinned URL'
            $script:downloads++; [IO.File]::WriteAllText($OutFile,'synthetic archive')
        }
        function Get-FileHash { param($LiteralPath,$Algorithm)
            [pscustomobject]@{Hash=$(if ($badChecksum) {'bad'} else {'F26081F438319757F8A1B46D7D0B2C5EC7583D4038E485E0AA725CFDE7885FFF'})}
        }
        function Expand-Archive { param($LiteralPath,$DestinationPath)
            foreach ($name in @('bin/go.exe','src/runtime/runtime2.go','pkg/tool/windows_amd64/compile.exe','pkg/tool/windows_amd64/link.exe')) {
                $path=Join-Path $DestinationPath "go/$name"
                New-Item -ItemType Directory (Split-Path $path -Parent) -Force | Out-Null
                [IO.File]::WriteAllText($path,'go version go1.27.3 windows/amd64')
            }
            if ($interruptExtraction) { throw 'simulated interrupted extraction' }
        }
        Install-RunnerDependencies $depConfig *> $null
        Assert-Helper ($downloads -eq 1) 'Fresh SDK was not installed exactly once'
        Install-RunnerDependencies $depConfig *> $null
        Assert-Helper ($downloads -eq 1) 'Current SDK was unnecessarily downloaded'
        $installed=Join-Path $depRoot 'tools/go/bin/go.exe'
        [IO.File]::WriteAllText($installed,'go version go1.26.0 windows/amd64')
        Install-RunnerDependencies $depConfig *> $null
        Assert-Helper ($downloads -eq 2 -and (Get-RunnerGoVersion $installed) -eq 'go version go1.27.3 windows/amd64') 'Wrong SDK version was not replaced'
        [IO.File]::WriteAllText($installed,'go version go1.26.0 windows/amd64')
        $script:badChecksum=$true; $failed=$false
        try { Install-RunnerDependencies $depConfig *> $null } catch { $failed=$_.Exception.Message -match 'SHA256 mismatch' }
        Assert-Helper ($failed -and (Get-RunnerGoVersion $installed) -eq 'go version go1.26.0 windows/amd64') 'Unverified archive replaced the installed SDK'
        $script:badChecksum=$false; $script:interruptExtraction=$true; $failed=$false
        try { Install-RunnerDependencies $depConfig *> $null } catch { $failed=$_.Exception.Message -match 'interrupted extraction' }
        Assert-Helper ($failed -and (Get-RunnerGoVersion $installed) -eq 'go version go1.26.0 windows/amd64') 'Interrupted extraction corrupted the installed SDK'
        $script:interruptExtraction=$false
        Install-RunnerDependencies $depConfig *> $null
        Assert-Helper ((Get-RunnerGoVersion $installed) -eq 'go version go1.27.3 windows/amd64') 'SDK installation could not resume after interruption'
        Assert-Helper (@(Get-ChildItem (Join-Path $depRoot 'tools') -Filter 'go-*-*').Count -eq 0) 'Temporary SDK installation directories were retained'
    }
    $script:RunnerRoot=$repository

    & {
        # Exercise the actual native-output path: both live progress and the
        # exception retained by Activate must include the guest failure.
        $nativeFixture=Join-Path $scratch 'image-command.cmd'
        [IO.File]::WriteAllText($nativeFixture, "@echo off`r`necho [PrepareImage] Installing toolchain`r`necho CLC_VERIFICATION_FAILED exit=43 command=fixture-verify`r`necho image diagnostics: retained-cloud-init.log`r`necho github_pat_STREAM_FIXTURE_SECRET`r`nexit /b 43`r`n")
        function Get-RunnerBinary { $nativeFixture }
        function Build-RunnerController { $nativeFixture }
        $stream=[Collections.Generic.List[string]]::new(); $message=''
        try { Invoke-RunnerCommand prepare-image $configPath *>&1 | ForEach-Object { $stream.Add([string]$_) } } catch { $message=$_.Exception.Message }
        Assert-Helper (($stream -join "`n") -match 'Installing toolchain') 'Image provisioning progress was buffered/hidden'
        Assert-Helper ($message -match 'exit 43.*' -and $message -match 'CLC_VERIFICATION_FAILED exit=43 command=fixture-verify' -and $message -match 'retained-cloud-init.log') 'Native image failure lost the command, exit status or retained log path'
        Assert-Helper (($message + ($stream -join "`n")) -notmatch 'STREAM_FIXTURE_SECRET') 'Native image output leaked a credential'
    }

    $lockPath = Join-Path $scratch 'operation.lock'
    $first = Enter-RunnerFileLock $lockPath
    try {
        $locked = $false
        try { $second = Enter-RunnerFileLock $lockPath; $second.Dispose() } catch { $locked=$true }
        Assert-Helper $locked 'Management lock allowed overlapping operations'
    } finally { $first.Dispose() }
    $second = Enter-RunnerFileLock $lockPath
    $second.Dispose()
    $lock = Enter-RunnerFileLock (Join-Path $local.Settings.controller.dataDirectory 'controller.lock')
    try {
        $script:ranLockedOperation=$false; $failed=$false
        try { Invoke-RunnerStopped $configPath { $script:ranLockedOperation=$true } } catch { $failed=$true }
        Assert-Helper ($failed -and -not $ranLockedOperation) 'Mutation bypassed the actual controller lock'
    } finally { $lock.Dispose() }

    # A stored PID belonging to this PowerShell process with the wrong identity
    # cannot be used to signal a stop, even though the PID is still alive.
    Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'status.json') @{
        pid=$PID; startedAt=(Get-Date).ToString('o'); updatedAt=(Get-Date).ToString('o');status='listening';nonce=[guid]::NewGuid().ToString()
    }
    Stop-RunnerController $configPath *> $null
    Assert-Helper (-not (Test-Path (Join-Path $local.Settings.controller.dataDirectory 'stop.json'))) 'Stop trusted a recycled/foreign process identity'

    & {
        $current=Get-Process -Id $PID
        function Get-RunnerBinary { $current.Path }
        $statusPath=Join-Path $local.Settings.controller.dataDirectory 'status.json'
        Write-RunnerJson $statusPath @{
            pid=$PID; startedAt=$current.StartTime.AddMilliseconds(1).ToString('o')
            updatedAt=(Get-Date).ToString('o');status='listening';nonce=[guid]::NewGuid().ToString()
        }
        $runtime=Get-RunnerRuntime $configPath
        Assert-Helper ($runtime.Alive -and $runtime.Healthy) 'ISO/DateTime conversion lost fractional process creation time'
        $status=Read-RunnerJson $statusPath
        $status['startedAt']=$current.StartTime.AddSeconds(-1).ToString('o')
        Write-RunnerJson $statusPath $status
        Assert-Helper (-not (Get-RunnerRuntime $configPath).Alive) 'A pre-creation timestamp identified a recycled process as live'
    }

    & {
        $nonce = [guid]::NewGuid().ToString()
        function Get-RunnerRuntime([string]$Config) { [pscustomobject]@{Alive=$true;Status=@{nonce=$nonce;status='draining'}} }
        $failed=$false
        try { Stop-RunnerController $configPath -Wait -TimeoutSeconds 0 *> $null } catch { $failed=$_.Exception.Message -match 'No jobs were terminated' }
        $request = Read-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'stop.json')
        Assert-Helper ($failed -and $request['nonce'] -eq $nonce) 'Drain timeout did not preserve the nonce-based stop request'
    }

    # Exercise real CurrentUser DPAPI storage with a synthetic SecureString;
    # neither successful output nor the persisted blob contains the plaintext.
    $synthetic='github_pat_SYNTHETIC_STORAGE_TEST'
    $secureToken=ConvertTo-SecureString $synthetic -AsPlainText -Force
    $output=& (Join-Path $repository 'scripts/Set-RunnerToken.ps1') -Config $configPath -Token $secureToken *>&1
    $blob=[IO.File]::ReadAllBytes($local.Settings.controller.patFile)
    $plain=[Security.Cryptography.ProtectedData]::Unprotect($blob,$null,[Security.Cryptography.DataProtectionScope]::CurrentUser)
    try {
        Assert-Helper (([Text.Encoding]::UTF8.GetString($plain)) -ceq $synthetic) 'Stored PAT did not round-trip with CurrentUser DPAPI'
        Assert-Helper (($output -join '') -notmatch $synthetic -and ([Text.Encoding]::UTF8.GetString($blob)) -notmatch $synthetic) 'Secure token workflow exposed plaintext'
    } finally { [Array]::Clear($plain); $secureToken.Dispose(); $synthetic=$null }

    & {
        $probeShim=Join-Path $scratch 'probe-scripts'
        New-Item -ItemType Directory $probeShim | Out-Null
        Copy-Item -LiteralPath (Join-Path $repository 'scripts/Test-Unattended.ps1') -Destination $probeShim
        Set-Content -LiteralPath (Join-Path $probeShim 'Common.ps1') -Value (". '" + ($repository.Replace("'","''")) + "/scripts/Common.ps1'") -Encoding utf8NoBOM
        Set-Content -LiteralPath (Join-Path $probeShim 'Startup.ps1') -Value "function Get-RunnerStartupFingerprint { 'synthetic-probe-inputs' }" -Encoding utf8NoBOM
        $probeConfig=Join-Path $scratch 'probe-config.json'
        $probeSettings=Get-Content -Raw $configPath | ConvertFrom-Json
        $probeSettings | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $probeConfig -Encoding utf8NoBOM
        $probeState=@{Starts=0;RegistrationFails=$true;ExitCode=0}
        $registered=[Collections.Generic.HashSet[string]]::new()
        function New-ScheduledTaskAction { @{} }
        function New-ScheduledTaskPrincipal { @{} }
        function New-ScheduledTaskTrigger { @{} }
        function New-ScheduledTaskSettingsSet { @{} }
        function New-ScheduledTask { @{} }
        function Register-ScheduledTask {
            [CmdletBinding()]param($TaskName,$InputObject)
            if ($probeState.RegistrationFails) { Write-Error 'Access is denied (synthetic registration failure)' }
            $null=$registered.Add($TaskName)
        }
        function Start-ScheduledTask {
            [CmdletBinding()]param($TaskName)
            $probeState.Starts++
            if ($probeState.RegistrationFails) { throw 'Failed registration must not reach start-task' }
            $null=$TaskName -match '^CLC-Startup-Probe-([a-f0-9]+)-(S4U|Interactive)$'
            Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory ("clc-startup-probe-$($Matches[1])-$($Matches[2]).json")) @{passed=$true;errors=@()}
        }
        function Get-ScheduledTask { [CmdletBinding()]param($TaskName)
            if ($registered.Contains($TaskName)) { [pscustomobject]@{State='Ready'} }
        }
        function Get-ScheduledTaskInfo { [CmdletBinding()]param($TaskName) [pscustomobject]@{LastTaskResult=$probeState.ExitCode} }
        function Stop-ScheduledTask { [CmdletBinding()]param($TaskName) }
        function Unregister-ScheduledTask { [CmdletBinding()]param($TaskName,[switch]$Confirm) $null=$registered.Remove($TaskName) }
        $failed=$false
        try { & (Join-Path $probeShim 'Test-Unattended.ps1') -Config $probeConfig *> $null } catch { $failed=$_.Exception.Message -match 'Neither scheduled task mode passed' }
        $report=Read-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json')
        Assert-Helper ($failed -and $probeState.Starts -eq 0) 'Probe continued after a nonterminating registration failure'
        foreach ($mode in @('S4U','Interactive')) {
            Assert-Helper ($report['modes'][$mode]['failedStage'] -eq 'register-task' -and -not $report['modes'][$mode]['passed']) "Incorrect failure stage reported for $mode registration"
        }
        $probeState.RegistrationFails=$false
        try { & (Join-Path $probeShim 'Test-Unattended.ps1') -Config $probeConfig *> $null }
        catch {
            $diagnostic=Read-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json')
            throw ('Synthetic startup probe unexpectedly failed: ' + ($diagnostic | ConvertTo-Json -Depth 8 -Compress))
        }
        $report=Read-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json')
        Assert-Helper ($report['modes']['Interactive']['passed'] -and $report['modes']['Interactive']['taskResult'] -eq 0) 'Successful worker/task result was not reported accurately'
        $probeState.ExitCode=1; $failed=$false
        try { & (Join-Path $probeShim 'Test-Unattended.ps1') -Config $probeConfig *> $null } catch { $failed=$_.Exception.Message -match 'Neither scheduled task mode passed' }
        $report=Read-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json')
        Assert-Helper ($failed -and -not $report['modes']['Interactive']['passed'] -and $report['modes']['Interactive']['taskResult'] -eq 1) 'Nonzero worker exit was falsely reported as validated startup'
    }

    & {
        $script:startupTask=$null; $script:probes=0; $script:registrations=0
        $script:probeFingerprint='current-inputs'
        function Get-RunnerStartupFingerprint([string]$Config) { $probeFingerprint }
        function Build-RunnerController { Get-RunnerBinary }
        function Get-RunnerStartupTask { $startupTask }
        function Unregister-ScheduledTask { param($TaskName,[switch]$Confirm) $script:startupTask=$null }
        function New-TestStartupTask {
            [pscustomobject]@{
                Description=('CLC Linux Hyper-V runner controller; ' + $configPath)
                Actions=@([pscustomobject]@{Execute=(Get-RunnerBinary);Arguments=('run --quiet --config "{0}"' -f $configPath);WorkingDirectory=$repository})
                Principal=[pscustomobject]@{UserId=[Security.Principal.WindowsIdentity]::GetCurrent().Name;LogonType='Interactive';RunLevel='Limited'}
                Triggers=@([pscustomobject]@{CimClass=[pscustomobject]@{CimClassName='MSFT_TaskLogonTrigger'};Enabled=$true;UserId=[Security.Principal.WindowsIdentity]::GetCurrent().Name})
                Settings=[pscustomobject]@{Enabled=$true;MultipleInstances=2;ExecutionTimeLimit='PT0S';RestartCount=99;RestartInterval='PT1M';StartWhenAvailable=$true;DisallowStartIfOnBatteries=$false;StopIfGoingOnBatteries=$false;RunOnlyIfIdle=$false}
            }
        }
        function Test-RunnerUnattended([string]$Config) {
            $script:probes++
            Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json') @{
                fingerprint=$probeFingerprint; logoutAndRebootValidated=$false
                modes=@{S4U=@{passed=$false;errors=@('register-task: Access is denied')};Interactive=@{passed=$true}}
            }
        }
        function Register-ScheduledTask { param($TaskName,$InputObject,[switch]$Force)
            $script:registrations++; $script:startupTask=New-TestStartupTask
        }
        # Constructors below never talk to Task Scheduler. Registration above
        # stores a complete fake task for the real comparison function to check.
        function New-ScheduledTaskAction { param($Execute,$Argument,$WorkingDirectory) @{} }
        function New-ScheduledTaskPrincipal { param($UserId,$LogonType,$RunLevel) @{} }
        function New-ScheduledTaskTrigger { param([switch]$AtLogOn,[switch]$AtStartup,$User) @{} }
        function New-ScheduledTaskSettingsSet { param($MultipleInstances,$RestartCount,$RestartInterval,$ExecutionTimeLimit,[switch]$StartWhenAvailable,[switch]$AllowStartIfOnBatteries,[switch]$DontStopIfGoingOnBatteries) @{} }
        function New-ScheduledTask { param($Action,$Principal,$Trigger,$Settings,$Description) @{} }

        $result = Enable-RunnerStartup $configPath
        Assert-Helper ($probes -eq 1 -and $registrations -eq 1 -and $result.Mode -eq 'Interactive') 'Startup did not select the actually validated mode'
        Assert-Helper ($result.Summary -match 'sign-in' -and $result.Summary -notmatch 'boot-only') 'Interactive startup was misrepresented'
        $result = Enable-RunnerStartup $configPath
        Assert-Helper ($probes -eq 1 -and $registrations -eq 1 -and $result.Current) 'Correct task/report was unnecessarily recreated'
        $report=Read-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json')
        $report['modes']['S4U']['passed']=$true
        Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json') $report
        $result=Enable-RunnerStartup $configPath
        Assert-Helper ($result.Mode -eq 'Interactive' -and $result.Current) 'Auto replaced a previously validated working mode unnecessarily'
        $startupTask.Actions[0].Arguments='run --config wrong.json'
        $result = Enable-RunnerStartup $configPath
        Assert-Helper ($registrations -eq 2 -and $probes -eq 1) 'Drifted task action was not repaired'
        $script:probeFingerprint='changed-image-or-session'
        $result = Enable-RunnerStartup $configPath
        Assert-Helper ($probes -eq 2 -and $registrations -eq 2) 'Changed probe inputs did not trigger validation, or recreated a correct task'
        $startupTask.Description='another configuration'
        $failed=$false
        try { $null=Enable-RunnerStartup $configPath } catch { $failed=$_.Exception.Message -match 'another configuration' }
        Assert-Helper ($failed -and $registrations -eq 2) 'Task ownership conflict was overwritten'
        $failed=$false;try { Disable-RunnerStartup $configPath } catch { $failed=$_.Exception.Message -match 'another configuration' }
        Assert-Helper ($failed -and $startupTask) 'Disabling startup removed another configuration task'
        $script:startupTask=New-TestStartupTask
        $failed=$false
        try { $null=Enable-RunnerStartup $configPath -Mode S4U } catch { $failed=$_.Exception.Message -match 'S4U did not pass' }
        Assert-Helper ($failed -and $probes -eq 3) 'Unvalidated S4U was accepted or failed mode was not retested'
        Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json') @{fingerprint=$probeFingerprint;modes=@{S4U=@{passed=$false};Interactive=@{passed=$false}}}
        $null=Enable-RunnerStartup $configPath
        Assert-Helper ($probes -eq 4) 'Failed previous startup probe could not recover automatically'
        Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'unattended-validation.json') @{fingerprint=$probeFingerprint;modes=@{}}
        $null=Enable-RunnerStartup $configPath
        Assert-Helper ($probes -eq 5) 'Interrupted startup report with no completed modes did not trigger a new probe'
        Disable-RunnerStartup $configPath
        Disable-RunnerStartup $configPath
        Assert-Helper (-not $startupTask) 'Owned startup task was not removed idempotently'
    }

    # The first progress record must reach the consumer while the producer is
    # still running. Also exercise native failure propagation and extra args.
    & {
        $streamMarker=Join-Path $scratch 'stream-observed'
        $streamBinary=Join-Path $scratch 'stream-fixture.ps1'
        $fixtureSource=@'
param($Verb)
if ('proof' -notin $args) { throw 'Validation arguments lost' }
Write-Output 'guest validation progress'
if (-not (Test-Path -LiteralPath '__MARKER__')) { throw 'Validation progress buffered until process exit' }
if ($Verb -eq 'test-hyperv') { $global:LASTEXITCODE=17 } else { $global:LASTEXITCODE=0 }
'@
        [IO.File]::WriteAllText($streamBinary,$fixtureSource.Replace('__MARKER__',$streamMarker.Replace("'","''")))
        function Get-RunnerBinary { $streamBinary }
        function Build-RunnerController { $streamBinary }
        function Get-RunnerSettings { param($Config) [pscustomobject]@{Path='fixture.json'} }
        function Write-Host { param($Object) if ([string]$Object -eq 'guest validation progress') { Set-Content -LiteralPath $streamMarker -Value 'observed during execution' } }
        $message=''
        try { Invoke-RunnerCommand test-hyperv fixture.json @('proof') } catch { $message=$_.Exception.Message }
        Assert-Helper ($message -match 'exit 17' -and (Test-Path -LiteralPath $streamMarker)) 'Streaming validation buffered progress or lost native failure'
        Remove-Item -LiteralPath $streamMarker
        Invoke-RunnerCommand probe-hyperv fixture.json @('proof')
        Assert-Helper ((Test-Path -LiteralPath $streamMarker) -and $LASTEXITCODE -eq 0) 'Startup probe did not stream progress or preserve success'
    }

    # Exercise the real dispatcher in separate noninteractive processes, with
    # only its activation function stubbed. This verifies OS exit status and
    # parameter forwarding without network, prompts or Windows task changes.
    $shim = Join-Path $scratch 'dispatcher'
    $shimScripts = Join-Path $shim 'scripts'
    New-Item -ItemType Directory $shimScripts -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $repository 'runners.ps1') -Destination $shim
    foreach ($name in @('Common.ps1','Dependencies.ps1','Host.ps1','Startup.ps1','Activate.ps1','Restart.ps1')) { Copy-Item -LiteralPath (Join-Path $repository "scripts/$name") -Destination $shimScripts }
    Add-Content -LiteralPath (Join-Path $shimScripts 'Activate.ps1') -Value @'

function Invoke-RunnerActivation {
    param($Config,[switch]$NoStartup,[switch]$ForceRebuild,[switch]$FromGitHubCLI,[switch]$NoCache,$StartupMode)
    if ($Config -eq 'fail') { throw 'GitHub HTTP 403 github_pat_DISPATCHER_SECRET; run SetToken' }
    if (-not $NoStartup -or -not $ForceRebuild -or -not $FromGitHubCLI -or $Config -ne 'custom.json') { throw 'Dispatcher lost arguments' }
    $global:LASTEXITCODE=23 # simulate a benign native cleanup result
    Write-Host 'dispatched successfully'
}
function Get-RunnerRuntime { [pscustomobject]@{Alive=$false} }
function Initialize-RunnerConfig { param($Config) [pscustomobject]@{} }
function Build-RunnerController { 'fixture.exe' }
function Start-RunnerController {
    param($Config,[switch]$Foreground)
    $probe=Enter-RunnerFileLock (Join-Path $script:RunnerRoot 'bin/management.lock')
    $probe.Dispose()
    Write-Host 'foreground management lock released'
}
'@
    $output = & pwsh.exe -NoProfile -NonInteractive -File (Join-Path $shim 'runners.ps1') Activate -Config custom.json -NoStartup -ForceRebuild -FromGitHubCLI 2>&1
    Assert-Helper ($LASTEXITCODE -eq 0 -and ($output -join '') -match 'dispatched successfully') 'Dispatcher success or parameter forwarding failed'
    $output=& (Join-Path $shim 'runners.ps1') Activate -Config custom.json -NoStartup -ForceRebuild -FromGitHubCLI *>&1
    Assert-Helper ($LASTEXITCODE -eq 0 -and ($output -join '') -match 'dispatched successfully') 'Successful dispatcher leaked a native cleanup exit code to its PowerShell caller'
    $output = & pwsh.exe -NoProfile -NonInteractive -File (Join-Path $shim 'runners.ps1') Activate -Config fail 2>&1
    Assert-Helper ($LASTEXITCODE -ne 0 -and ($output -join '') -match '403.*SetToken' -and ($output -join '') -notmatch 'DISPATCHER_SECRET') 'Dispatcher did not return a safe nonzero failure'
    $output=& pwsh.exe -NoProfile -NonInteractive -File (Join-Path $shim 'runners.ps1') Start -Config custom.json -Foreground 2>&1
    Assert-Helper ($LASTEXITCODE -eq 0 -and ($output -join '') -match 'foreground management lock released') 'Foreground Start held the management lock for the controller lifetime'
    $management=Enter-RunnerFileLock (Join-Path $shim 'bin/management.lock')
    try {
        foreach ($command in @('Activate','Start','BuildImage','TestHyperV','TestRace')) {
            $output=& pwsh.exe -NoProfile -NonInteractive -File (Join-Path $shim 'runners.ps1') $command -Config custom.json 2>&1
            Assert-Helper ($LASTEXITCODE -ne 0 -and ($output -join '') -match 'locked') "$command did not respect management serialization"
        }
    } finally { $management.Dispose() }
    $canPrompt = & pwsh.exe -NoProfile -NonInteractive -Command ". '$repository/scripts/Common.ps1'; . '$repository/scripts/Activate.ps1'; Test-RunnerCanPrompt"
    Assert-Helper (($canPrompt -join '').Trim() -eq 'False') 'Noninteractive activation could prompt for a credential'
    Write-Host "Activate helpers: $helperAssertions dependency, fingerprint, lock, startup, identity and exit-status assertions passed."
} finally {
    $script:RunnerRoot=$repository
    $absolute=[IO.Path]::GetFullPath($scratch)
    $tempRoot=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if ($absolute.StartsWith($tempRoot,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf).StartsWith('clc-activation-helper-')) { Remove-Item -LiteralPath $absolute -Recurse -Force -ErrorAction SilentlyContinue }
}
