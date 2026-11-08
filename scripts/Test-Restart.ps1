#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
. (Join-Path $PSScriptRoot 'Restart.ps1')
$repository=$script:RunnerRoot
$scratch=Join-Path ([IO.Path]::GetTempPath()) ('clc-restart-test-'+[guid]::NewGuid().ToString('N'))
$script:restartAssertions=0
function Assert-Restart($Condition,[string]$Message) { $script:restartAssertions++; if (-not $Condition) { throw $Message } }
function Assert-RestartFailure([scriptblock]$Action,[string]$Pattern) {
    $message=''; try { & $Action *>&1 | Out-Null } catch { $message=$_.Exception.Message }
    Assert-Restart ($message -match $Pattern) "Expected '$Pattern', got '$message'"
}
$original=$null; $replacement=$null
try {
    Protect-RunnerDirectory $scratch
    New-Item -ItemType Directory (Join-Path $scratch 'scripts'),(Join-Path $scratch 'bin'),(Join-Path $scratch 'data'),(Join-Path $scratch 'logs') | Out-Null
    foreach ($name in @('Common.ps1','Restart.ps1','Restart-Controller.ps1','Test-Unattended.ps1','Invoke-StartupProbe.ps1','HyperV.ps1')) { Copy-Item -LiteralPath (Join-Path $repository "scripts/$name") -Destination (Join-Path $scratch "scripts/$name") }
    # This scratch installation has no startup task. Production task APIs are
    # unreachable from the helper and no VM/image/auth operation is real.
    [IO.File]::WriteAllText((Join-Path $scratch 'scripts/Startup.ps1'),'function Get-RunnerStartupTask { return $null }')
    $binary=Join-Path $scratch 'bin/clc-runners.exe'
    Push-Location $repository
    try { & (Get-RunnerGo) build -o $binary ./scripts/testdata/restart-controller; if ($LASTEXITCODE) { throw 'Cannot build disposable restart fixture' } } finally { Pop-Location }
    $script:RunnerRoot=$scratch
    $configPath=Join-Path $scratch 'controller.json'
    $settings=Get-Content -Raw -LiteralPath (Join-Path $repository 'config/controller.example.json') | ConvertFrom-Json -AsHashtable
    $settings.github.scaleSet='restart-fixture-'+[guid]::NewGuid().ToString('N').Substring(0,8)
    $settings.controller.dataDirectory=Join-Path $scratch 'data'; $settings.controller.logDirectory=Join-Path $scratch 'logs'; $settings.controller.patFile=Join-Path $scratch 'credential.dpapi'
    $settings.hyperv.script=Join-Path $scratch 'scripts/HyperV.ps1'; $settings.hyperv.imageDirectory=Join-Path $scratch 'images'; $settings.hyperv.imageRecipe=Join-Path $repository 'image/recipe.json'
    Write-RunnerJson $configPath $settings
    [IO.File]::WriteAllText($settings.controller.patFile,'synthetic encrypted fixture')
    Write-RunnerJson ($binary+'.build.json') @{inputs=('a'*64);binary=(Get-FileHash $binary).Hash;validated=$true}
    $manifest=New-RunnerRestartRuntime
    Start-RunnerController $configPath -Installed
    $runtime=Get-RunnerRuntime $configPath; Assert-Restart $runtime.Healthy 'Original disposable controller did not become healthy'
    $original=Get-Process -Id $runtime.Status.pid
    Write-RunnerJson (Join-Path $scratch 'data/state.json') @{version=2;runners=@{}}
    $requestPath=Join-Path $scratch 'data/restart.json'
    $request=@{version=1;state='ReadyToRestart';pending=$true;automatic=$false;requestId=[guid]::NewGuid().ToString();controllerNonce=$runtime.Status.nonce;pid=$runtime.Status.pid;startedAt=$runtime.Status.startedAt;config=$configPath;root=$scratch;executable=$binary;accountSid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value;lastAttempt=(Get-Date).ToUniversalTime().ToString('o');target=@{identity='fixture';configHash=(Get-FileHash $configPath).Hash;patHash=(Get-FileHash $settings.controller.patFile).Hash;binaryHash=(Get-FileHash $binary).Hash;runtimeHash=(Get-FileHash $settings.hyperv.script).Hash;imageId='fixture-image';manifestPath=(Join-Path $scratch 'bin/restart-runtime.json');manifestHash=(Get-FileHash (Join-Path $scratch 'bin/restart-runtime.json')).Hash}}
    Write-RunnerJson $requestPath $request
    $request['requestPath']=$requestPath
    & { function Get-RunnerStartupTask { return $null }; $null=Test-RunnerRestartPlan $request }
    $lease=Enter-RunnerFileLock (Join-Path $scratch 'data/restart-helper.lock')
    try { Assert-RestartFailure { Invoke-RunnerRestartHandoff $request } 'locked' } finally { $lease.Dispose() }
    $bad=$request.Clone();$bad.controllerNonce=[guid]::NewGuid().ToString();Write-RunnerJson $requestPath $bad
    Assert-RestartFailure { Invoke-RunnerRestartHandoff $bad } 'identity mismatch'
    Assert-Restart (-not $original.HasExited) 'Nonce mismatch terminated original controller'
    $bad=$request.Clone();$bad.pid=$PID;Write-RunnerJson $requestPath $bad
    Assert-RestartFailure { Invoke-RunnerRestartHandoff $bad } 'identity mismatch'
    $bad=$request.Clone();$bad.startedAt=(Get-Date).AddHours(-1).ToUniversalTime().ToString('o');Write-RunnerJson $requestPath $bad
    Assert-RestartFailure { Invoke-RunnerRestartHandoff $bad } 'identity mismatch'
    & {
        function Get-RunnerStartupTask { return $null }
        Write-RunnerJson $requestPath $request
        Assert-RestartFailure { Invoke-RunnerRestartHandoff $request -ExitTimeoutSeconds 0 } 'did not exit before timeout'
    }
    Assert-Restart (-not $original.HasExited) 'Exit timeout terminated original controller'
    Assert-Restart ((Read-RunnerJson $requestPath).state -eq 'Failed') 'Helper failure was not persisted'
    # Tampered metadata and invalid config are rejected before any handoff.
    $metadata=Read-RunnerJson ($binary+'.build.json');Write-RunnerJson ($binary+'.build.json') @{binary='wrong'}
    Assert-RestartFailure { Assert-RunnerRestartManifest $request } 'metadata'
    Write-RunnerJson ($binary+'.build.json') $metadata
    $before=[IO.File]::ReadAllText($configPath);[IO.File]::WriteAllText($configPath,'{')
    Assert-RestartFailure { Test-RunnerRestartPlan $request } 'JSON|Json|convert|Unexpected'
    [IO.File]::WriteAllText($configPath,$before)
    Write-RunnerJson $requestPath $request
    # Launch the actual, pinned PowerShell helper through the same detached
    # launcher as production. The original exits only after the ACK is durable.
    $helperOutput=& $binary launch-helper --config $configPath 2>&1
    Assert-Restart ($LASTEXITCODE -eq 0) ('Detached fixture helper failed: '+($helperOutput -join "`n"))
    $deadline=[DateTime]::UtcNow.AddSeconds(35);$ackPath=Join-Path $scratch 'data/restart-ack.json'
    do { $ack=Read-RunnerJson $ackPath; if ($ack -and $ack.helperPid -eq [int]($helperOutput -join '')) { break }; Start-Sleep -Milliseconds 100 } while ([DateTime]::UtcNow -lt $deadline)
    Assert-Restart ($ack -and $ack.requestId -eq $request.requestId -and $ack.helperPid -eq [int]($helperOutput -join '')) 'Independent helper never acknowledged the expected controller'
    $request.state='Restarting';Write-RunnerJson $requestPath $request
    [IO.File]::WriteAllText((Join-Path $scratch "data/fixture-exit-$($original.Id)"),'exit cleanly')
    Assert-Restart ($original.WaitForExit(10000)) 'Disposable original did not exit cleanly'
    do { $result=Read-RunnerJson $requestPath; if ($result.state -in @('Completed','Failed')) { break }; Start-Sleep -Milliseconds 200 } while ([DateTime]::UtcNow -lt $deadline)
    Assert-Restart ($result.state -eq 'Completed') ('Full helper handoff failed: '+($result | ConvertTo-Json -Compress -Depth 8))
    $runtime=Get-RunnerRuntime $configPath; $replacement=Get-Process -Id $runtime.Status.pid
    Assert-Restart ($runtime.Healthy -and $runtime.Status.nonce -ne $request.controllerNonce -and $result.replacementNonce -eq $runtime.Status.nonce) 'Readiness did not verify a new controller instance'
    Assert-Restart ($result.lastSuccess -and -not $result.pending) 'Success was not recorded after readiness'
    Assert-Restart ((Wait-RunnerRestartHealthy $request -TimeoutSeconds 0).Healthy) 'Healthy replacement was rejected'
    $wrong=$request.Clone();$wrong.target=$request.target.Clone();$wrong.target.configHash='unexpected'
    Assert-RestartFailure { Wait-RunnerRestartHealthy $wrong -TimeoutSeconds 0 } 'unexpected configHash'
    $notReady=Join-Path $scratch 'data/fixture-not-ready';[IO.File]::WriteAllText($notReady,'simulate listener not ready')
    $deadline=[DateTime]::UtcNow.AddSeconds(5)
    do { $probe=Get-RunnerRuntime $configPath; if ($probe.Status.status -eq 'starting') { break }; Start-Sleep -Milliseconds 100 } while ([DateTime]::UtcNow -lt $deadline)
    Assert-RestartFailure { Wait-RunnerRestartHealthy $request -TimeoutSeconds 0 } 'failed readiness checks'
    Remove-Item -LiteralPath $notReady
    Assert-Restart ((Wait-RunnerRestartHealthy $request -TimeoutSeconds 5).Healthy) 'Readiness did not recover after listener recovery'
    # Repeat with a separately staged PE and an ordinary controller setting.
    # Appended fixture provenance changes its SHA256 while leaving this known
    # disposable program runnable; production candidates come from Go tests/vet.
    $oldBinaryHash=(Get-FileHash $binary).Hash
    $candidate=Join-Path $scratch 'bin/staged/clc-runners.exe'
    New-Item -ItemType Directory (Split-Path $candidate -Parent) | Out-Null
    Copy-Item -LiteralPath $binary -Destination $candidate
    [IO.File]::AppendAllText($candidate,'validated disposable fixture version two')
    Write-RunnerJson ($candidate+'.build.json') @{inputs=('b'*64);binary=(Get-FileHash $candidate).Hash;validated=$true}
    $settings.controller.logLevel='debug';Write-RunnerJson $configPath $settings
    [IO.File]::WriteAllText((Join-Path $scratch 'scripts/Startup.ps1'),@'
function Get-RunnerStartupTask { [pscustomobject]@{State='Ready';Principal=[pscustomobject]@{LogonType='Interactive'}} }
function Test-RunnerStartupTask { param($Task,$Config,$Mode) return $true }
function Get-RunnerStartupValidation { param($Config,$Binary,$ImageID) @{modes=@{Interactive=@{passed=$true}}} }
function Start-ScheduledTask { param($TaskName)
    if ($TaskName -ne 'CLC-Linux-Runners') { throw 'Unexpected scheduled task' }
    [IO.File]::WriteAllText((Join-Path $script:RunnerRoot 'data/existing-task-reused'),'existing task triggered')
    Start-RunnerController (Join-Path $script:RunnerRoot 'controller.json') -Installed
}
'@)
    $null=New-RunnerRestartRuntime $candidate
    $request=$request.Clone();$request.target=$request.target.Clone()
    $request.state='ReadyToRestart';$request.requestId=[guid]::NewGuid().ToString();$request.controllerNonce=$runtime.Status.nonce;$request.pid=$runtime.Status.pid;$request.startedAt=$runtime.Status.startedAt
    $request.target.binaryHash=(Get-FileHash $candidate).Hash;$request.target.configHash=(Get-FileHash $configPath).Hash;$request.target.manifestHash=(Get-FileHash $request.target.manifestPath).Hash
    Write-RunnerJson $requestPath $request
    $original=$replacement
    $helperOutput=& $binary launch-helper --config $configPath 2>&1
    Assert-Restart ($LASTEXITCODE -eq 0) ('Staged fixture helper failed: '+($helperOutput -join "`n"))
    $deadline=[DateTime]::UtcNow.AddSeconds(35)
    do { $ack=Read-RunnerJson $ackPath; if ($ack -and $ack.requestId -eq $request.requestId) { break }; Start-Sleep -Milliseconds 100 } while ([DateTime]::UtcNow -lt $deadline)
    Assert-Restart ($ack.requestId -eq $request.requestId) 'Staged update helper did not acknowledge'
    $request.state='Restarting';Write-RunnerJson $requestPath $request
    [IO.File]::WriteAllText((Join-Path $scratch "data/fixture-crash-$($original.Id)"),'simulate unexpected exit after quiescence/commit')
    $exited=$original.WaitForExit(10000)
    $crashProof=Join-Path $scratch "data/fixture-crashed-$($original.Id)"
    Assert-Restart ($exited -and (Test-Path -LiteralPath $crashProof) -and [IO.File]::ReadAllText($crashProof) -eq 'intentional os.Exit(23)') 'Unexpected original exit fixture did not run'
    do { $result=Read-RunnerJson $requestPath; if ($result.state -in @('Completed','Failed')) { break }; Start-Sleep -Milliseconds 200 } while ([DateTime]::UtcNow -lt $deadline)
    Assert-Restart ($result.state -eq 'Completed') ('Staged helper failed: '+($result | ConvertTo-Json -Compress -Depth 8))
    $runtime=Get-RunnerRuntime $configPath;$replacement=Get-Process -Id $runtime.Status.pid
    Assert-Restart ($runtime.Healthy -and $runtime.Status.configHash -eq $request.target.configHash -and $runtime.Status.binaryHash -eq $request.target.binaryHash) 'Staged binary/configuration was not loaded'
    Assert-Restart ((Get-FileHash ($binary+'.previous')).Hash -eq $oldBinaryHash) 'Atomic installation did not preserve the known-good executable'
    Assert-Restart (Test-Path -LiteralPath (Join-Path $scratch 'data/existing-task-reused')) 'Helper did not reuse the existing startup task path'
    # A replacement that exits immediately produces Failed, preserves evidence
    # and can be started through the ordinary path after correcting the cause.
    $request=$request.Clone();$request.state='ReadyToRestart';$request.requestId=[guid]::NewGuid().ToString();$request.controllerNonce=$runtime.Status.nonce;$request.pid=$runtime.Status.pid;$request.startedAt=$runtime.Status.startedAt
    Write-RunnerJson $requestPath $request;$original=$replacement
    $startFailure=Join-Path $scratch 'data/fixture-start-fail';[IO.File]::WriteAllText($startFailure,'injected startup failure')
    $helperOutput=& $binary launch-helper --config $configPath 2>&1
    Assert-Restart ($LASTEXITCODE -eq 0) 'Failure fixture helper did not launch'
    $deadline=[DateTime]::UtcNow.AddSeconds(35)
    do { $ack=Read-RunnerJson $ackPath; if ($ack -and $ack.requestId -eq $request.requestId) { break }; Start-Sleep -Milliseconds 100 } while ([DateTime]::UtcNow -lt $deadline)
    Assert-Restart ($ack.requestId -eq $request.requestId) 'Failure fixture helper did not ACK'
    $request.state='Restarting';Write-RunnerJson $requestPath $request
    [IO.File]::WriteAllText((Join-Path $scratch "data/fixture-exit-$($original.Id)"),'exit cleanly');$null=$original.WaitForExit(10000)
    do { $result=Read-RunnerJson $requestPath; if ($result.state -in @('Completed','Failed')) { break }; Start-Sleep -Milliseconds 200 } while ([DateTime]::UtcNow -lt $deadline)
    Assert-Restart ($result.state -eq 'Failed' -and $result.pending -and $result.lastFailure -match 'startup failure|could not start|readiness|exited before') 'Replacement failure did not produce visible failure diagnostics'
    Remove-Item -LiteralPath $startFailure
    Start-RunnerController $configPath -Installed
    $runtime=Wait-RunnerHealthy $configPath;$replacement=Get-Process -Id $runtime.Status.pid
    Assert-Restart $runtime.Healthy 'Ordinary startup could not recover after helper failure'
    [IO.File]::WriteAllText((Join-Path $scratch "data/fixture-exit-$($replacement.Id)"),'exit cleanly');$null=$replacement.WaitForExit(10000)
    $knownGoodHash=(Get-FileHash ($binary+'.previous')).Hash
    [IO.File]::WriteAllText($binary,'corrupt unvalidated executable')
    Install-RunnerStagedController (Read-RunnerJson (Join-Path $scratch 'bin/restart-runtime.json'))
    Assert-Restart ((Get-FileHash ($binary+'.previous')).Hash -eq $knownGoodHash) 'Repair overwrote the recoverable previous executable with corrupt bytes'
    Assert-Restart (@(Get-ChildItem -LiteralPath (Join-Path $scratch 'bin') -Filter 'clc-runners.exe.unvalidated-*').Count -gt 0) 'Repair discarded unvalidated executable diagnostics'
} finally {
    try {
        if (Test-Path -LiteralPath (Join-Path $scratch 'controller.json')) {
            $finalRuntime=Get-RunnerRuntime (Join-Path $scratch 'controller.json')
            if ($finalRuntime.Alive) { $replacement=Get-Process -Id $finalRuntime.Status.pid }
        }
    } catch { Write-Verbose 'Fixture runtime was unavailable during cleanup; its independent 60-second timeout remains.' }
    # Only disposable fixtures are asked to exit; no real controller is touched.
    foreach ($fixtureProcess in @($original,$replacement)) { if ($fixtureProcess -and -not $fixtureProcess.HasExited) { [IO.File]::WriteAllText((Join-Path $scratch "data/fixture-exit-$($fixtureProcess.Id)"),'test cleanup'); $null=$fixtureProcess.WaitForExit(10000) } }
    $script:RunnerRoot=$repository
    $absolute=[IO.Path]::GetFullPath($scratch);$tempRoot=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')+'\'
    if ($absolute.StartsWith($tempRoot,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf).StartsWith('clc-restart-test-')) { Remove-Item -LiteralPath $absolute -Recurse -Force -ErrorAction SilentlyContinue }
}
Write-Host "Restart helpers: $restartAssertions assertions passed, including a real detached PowerShell/controller handoff in an isolated installation."
