#requires -Version 7.4
# These functions are shared by Activate and the pinned, one-shot helper.

function New-RunnerRestartRuntime([string]$Binary = (Get-RunnerBinary)) {
    $names = @('Common.ps1','Startup.ps1','Restart.ps1','Restart-Controller.ps1','Test-Unattended.ps1','Invoke-StartupProbe.ps1','HyperV.ps1')
    $sources = @($names | ForEach-Object { Join-Path $script:RunnerRoot "scripts/$_" })
    $binaryHash = (Get-FileHash -LiteralPath $Binary -Algorithm SHA256).Hash.ToLowerInvariant()
    $fingerprint = Get-RunnerFingerprint $sources $binaryHash
    $bundle = Join-Path $script:RunnerRoot "bin/restart/$fingerprint"
    New-Item -ItemType Directory -Path (Join-Path $bundle 'scripts') -Force | Out-Null
    $files = @{}; $sourceHashes = @{}
    foreach ($source in $sources) {
        $hash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
        $destination = Join-Path $bundle ('scripts/' + (Split-Path $source -Leaf))
        if (Test-Path -LiteralPath $destination) {
            if ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash -ne $hash) { throw 'Prepared restart runtime was modified; preserve diagnostics and remove the damaged bundle explicitly before Activate.' }
        } else { Copy-Item -LiteralPath $source -Destination $destination }
        if ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash -ne $hash) { throw 'Restart scripts changed during staging; rerun Activate.' }
        $files[$destination] = $hash; $sourceHashes[$source] = $hash
    }
    $manifest = @{version=1;bundle=$bundle;binary=$Binary;binaryHash=$binaryHash;files=$files;sources=$sourceHashes}
    Write-RunnerJson (Join-Path $script:RunnerRoot 'bin/restart-runtime.json') $manifest
    $manifest
}

function Assert-RunnerRestartManifest($Request) {
    $manifestPath = Join-Path $Request.root 'bin/restart-runtime.json'
    if ($Request.target.manifestPath -ne $manifestPath -or (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash -ne $Request.target.manifestHash) { throw 'Prepared update changed; wait for a stable update and retry.' }
    $manifest = Read-RunnerJson $manifestPath
    if (-not $manifest -or $manifest.version -ne 1 -or -not [IO.Path]::IsPathFullyQualified($manifest.bundle)) { throw 'Invalid restart installation metadata.' }
    foreach ($path in $manifest.files.Keys) {
        $relative = [IO.Path]::GetRelativePath($manifest.bundle,$path)
        if ($relative -eq '..' -or $relative.StartsWith('..' + [IO.Path]::DirectorySeparatorChar) -or [IO.Path]::IsPathFullyQualified($relative)) { throw 'Restart bundle path escapes its installation.' }
        if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $manifest.files[$path]) { throw 'Prepared restart bundle checksum mismatch.' }
    }
    foreach ($path in $manifest.sources.Keys) {
        if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $manifest.sources[$path]) { throw 'Startup/runtime scripts changed; run Activate to prepare the update.' }
    }
    $metadata = Read-RunnerJson ($manifest.binary + '.build.json')
    $hash = (Get-FileHash -LiteralPath $manifest.binary -Algorithm SHA256).Hash
    if (-not $metadata -or $metadata['binary'] -ne $hash -or $manifest.binaryHash -ne $hash -or $Request.target.binaryHash -ne $hash) { throw 'Candidate executable does not match validated build metadata; run Activate.' }
    if ($manifest.binary -ne (Get-RunnerBinary) -and -not $metadata['validated']) { throw 'Staged executable lacks successful regression-test/vet validation; run Activate.' }
    $manifest
}

function Invoke-RunnerRestartBinary([string]$Binary,[ValidateSet('validate','image-status','check-auth','setup','check-hyperv')][string]$Verb,[string]$Config,[string[]]$Extra=@()) {
    if ($Verb -eq 'setup' -and ($Extra.Count -ne 1 -or $Extra[0] -cne '--check')) { throw 'Restart validation permits only read-only setup --check.' }
    $info=[Diagnostics.ProcessStartInfo]::new($Binary)
    $info.UseShellExecute=$false;$info.CreateNoWindow=$true
    $info.RedirectStandardOutput=$true;$info.RedirectStandardError=$true
    foreach ($argument in @($Verb,'--config',$Config)+$Extra) { $info.ArgumentList.Add($argument) }
    $process=[Diagnostics.Process]::new();$process.StartInfo=$info
    try {
        if (-not $process.Start()) { throw "Cannot start restart validation $Verb." }
        $stdout=$process.StandardOutput.ReadToEndAsync();$stderr=$process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit(180000)) {
            # These commands are read-only validators, never run/cleanup or a
            # workflow process. Bound their entire local subprocess tree.
            $process.Kill($true);$process.WaitForExit()
            throw "Restart validation $Verb timed out; controller and jobs were preserved."
        }
        $output=$stdout.GetAwaiter().GetResult();$errorOutput=$stderr.GetAwaiter().GetResult()
        if ($process.ExitCode) { throw (Protect-RunnerText ("Restart validation $Verb failed: " + $output + $errorOutput)) }
        $output.Trim()
    } finally { $process.Dispose() }
}

function Test-RunnerRestartPlan($Request) {
    $manifest = Assert-RunnerRestartManifest $Request
    $local = Get-RunnerSettings $Request.config
    $settings = $local.Settings
    if ($local.Path -ne $Request.config -or (Join-Path $settings.controller.dataDirectory 'restart.json') -ne $Request.requestPath) { throw 'Restart request configuration/data path mismatch.' }
    foreach ($entry in @(@($Request.config,$Request.target.configHash),@($settings.controller.patFile,$Request.target.patHash),@($settings.hyperv.script,$Request.target.runtimeHash))) {
        if ((Get-FileHash -LiteralPath $entry[0] -Algorithm SHA256).Hash -ne $entry[1]) { throw 'Desired inputs changed during validation; wait for a stable update.' }
    }
    $null = Invoke-RunnerRestartBinary $manifest.binary validate $Request.config
    $image = Invoke-RunnerRestartBinary $manifest.binary image-status $Request.config | ConvertFrom-Json -AsHashtable
    if (-not $image.current -or -not $image.image -or $image.image.id -ne $Request.target.imageId) { throw 'Runner image requires explicit preparation. Stop safely, then run PrepareImage or Activate; automatic restart never builds images.' }
    $task = Get-RunnerStartupTask
    if ($task) {
        $mode = [string]$task.Principal.LogonType
        if ($mode -notin @('S4U','Interactive','Password') -or -not (Test-RunnerStartupTask $task $Request.config $mode)) { throw 'Existing startup task ownership/settings do not match. Repair with Activate; no task was changed.' }
        $report = Get-RunnerStartupValidation $Request.config -Binary $manifest.binary -ImageID $image.image.id
        if (-not $report -or -not $report.modes.Contains($mode) -or -not $report.modes[$mode].passed) {
            $probeCommand=if ($mode -eq 'Password') { 'TestUnattended -StartupMode Password' } else { 'TestUnattended' }
            throw "Unattended validation required for the desired executable, image, hardware, probe or account/boot/session. Drain with Stop, run $probeCommand, then Activate."
        }
    }
    $null = Invoke-RunnerRestartBinary $manifest.binary check-auth $Request.config
    $infrastructure = Invoke-RunnerRestartBinary $manifest.binary setup $Request.config @('--check') | ConvertFrom-Json
    if (-not $infrastructure.current) { throw 'GitHub infrastructure requires explicit Setup/Activate while stopped.' }
    $null = Invoke-RunnerRestartBinary $manifest.binary check-hyperv $Request.config
    $manifest
}

function Stage-RunnerControllerUpdate([string]$Config) {
    $binary = Build-RunnerController -Stage
    $null = Invoke-RunnerRestartBinary $binary validate $Config
    $null = New-RunnerRestartRuntime $binary
    $pending=Read-RunnerJson (Join-Path (Get-RunnerSettings $Config).Settings.controller.dataDirectory 'restart.json')
    if ($pending -and $pending['pending']) { Request-RunnerRestart $Config }
    Write-Host 'Validated controller update staged. Automatic detection will apply it after safe draining; Status shows any required unattended/image validation.'
}

function Request-RunnerRestart([string]$Config) {
    $local = Get-RunnerSettings $Config -EffectiveFallback
    $runtime = Get-RunnerRuntime $Config -EffectiveFallback
    if (-not $runtime.Alive) { throw 'Controller is not running. Run Activate to start it.' }
    if (-not $runtime.Status['restartSupported']) { throw 'Running controller predates graceful restart support. Stop and wait for accepted jobs/cleanup, then Activate once to install this version.' }
    $nonce = [guid]::Empty
    if (-not [guid]::TryParse($runtime.Status['nonce'],[ref]$nonce)) { throw 'Controller restart nonce is invalid; no request was sent.' }
    Write-RunnerJson (Join-Path $local.Settings.controller.dataDirectory 'restart-request.json') @{nonce=$runtime.Status['nonce'];requestId=[guid]::NewGuid().ToString()}
    Write-Host 'Graceful restart requested. Already accepted jobs and cleanup will finish; use Status or Jobs to inspect progress.'
}

function Install-RunnerStagedController($Manifest) {
    $binary = Get-RunnerBinary
    if ($Manifest.binary -eq $binary) { return }
    $metadata=Read-RunnerJson ($Manifest.binary+'.build.json')
    if (-not $metadata -or $metadata['binary'] -ne $Manifest.binaryHash) { throw 'Candidate metadata changed during installation.' }
    Install-RunnerControllerBinary $Manifest.binary $metadata
}

function Wait-RunnerRestartHealthy($Request,[int]$TimeoutSeconds=120) {
    $began=[DateTime]::UtcNow
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $runtime = Get-RunnerRuntime $Request.config
        if ($runtime.Alive -and $runtime.Status['nonce'] -eq $Request.controllerNonce) { throw 'Original controller is still running; no replacement was verified.' }
        if ($runtime.Healthy) {
            foreach ($key in @('configHash','patHash','binaryHash','runtimeHash','imageId')) {
                if ($runtime.Status[$key] -ne $Request.target[$key]) { throw "Replacement controller loaded unexpected $key; inspect Status. Its jobs were preserved." }
            }
            return $runtime
        }
        if (-not $runtime.Alive -and ([DateTime]::UtcNow-$began).TotalSeconds -ge 5 -and -not @(Get-RunnerControllerProcesses).Count) { throw 'Replacement controller exited before readiness. Inspect startup stderr/controller logs, correct the blocker, then Activate; previous executable is preserved.' }
        Start-Sleep -Milliseconds 500
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'Replacement controller failed readiness checks. Inspect startup stderr/controller logs; correct the blocker and run Activate. No process or job was terminated.'
}

function Enter-RunnerRestartLock([string]$Path,[int]$TimeoutSeconds=30) {
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do { try { return Enter-RunnerFileLock $Path } catch { if ([DateTime]::UtcNow -ge $deadline) { throw }; Start-Sleep -Milliseconds 250 } } while ($true)
}

function Invoke-RunnerRestartHandoff($Request,[int]$ExitTimeoutSeconds=120) {
    $data = (Get-RunnerSettings $Request.config).Settings.controller.dataDirectory
    $lease = Enter-RunnerFileLock (Join-Path $data 'restart-helper.lock')
    $management = $null; $dataLock = $null; $scopeLock = $null
    try {
        $current = Read-RunnerJson $Request.requestPath
        if (-not $current -or $current.requestId -ne $Request.requestId -or $current.controllerNonce -ne $Request.controllerNonce -or $current.state -ne 'ReadyToRestart') { throw 'Obsolete or duplicate restart helper; request is no longer ready.' }
        if ($Request.accountSid -ne [Security.Principal.WindowsIdentity]::GetCurrent().User.Value) { throw 'Restart helper account SID differs from the controller; handoff refused.' }
        $runtime = Get-RunnerRuntime $Request.config
        if (-not $runtime.Status -or $runtime.Status['nonce'] -ne $Request.controllerNonce -or $runtime.Status['pid'] -ne $Request.pid -or $runtime.Status['startedAt'] -ne $Request.startedAt) { throw 'Controller instance nonce/PID/start identity mismatch; obsolete helper refused.' }
        # Pin the process object/creation time, never terminate it. A recycled
        # PID is not the original process and cannot authorize any destructive action.
        $original = Get-Process -Id $Request.pid -ErrorAction SilentlyContinue
        if ($original) {
            $age = (([DateTimeOffset]$Request.startedAt).UtcDateTime - $original.StartTime.ToUniversalTime()).TotalSeconds
            if ($original.Path -ne $Request.executable -or $age -lt 0 -or $age -ge 60) { throw 'Original PID was reused or executable identity differs; helper refused.' }
        }
        # A brokered helper can have a different logon token/session. Validate
        # actual DPAPI, Hyper-V and the unchanged unattended fingerprint here,
        # before acknowledging and allowing the original process to exit.
        $null = Test-RunnerRestartPlan $Request
        $current = Read-RunnerJson $Request.requestPath
        if (-not $current -or $current.requestId -ne $Request.requestId -or $current.state -ne 'ReadyToRestart') { throw 'Restart was superseded during helper validation.' }
        Write-RunnerJson (Join-Path $data 'restart-ack.json') @{requestId=$Request.requestId;nonce=$Request.controllerNonce;helperPid=$PID;startedAt=(Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')}
        $deadline = [DateTime]::UtcNow.AddSeconds($ExitTimeoutSeconds)
        while ($original -and -not $original.HasExited) {
            if ([DateTime]::UtcNow -ge $deadline) { throw 'Original controller did not exit before timeout; it and its jobs were preserved.' }
            Start-Sleep -Milliseconds 250
            $original.Refresh()
        }
        $current = Read-RunnerJson $Request.requestPath
        if (-not $current -or $current.requestId -ne $Request.requestId -or $current.state -ne 'Restarting') { throw 'Restart commit is absent or stale; no installation was changed.' }
        $newRuntime = Get-RunnerRuntime $Request.config
        if ($newRuntime.Alive) { throw 'Another controller is already alive; obsolete helper will not replace or terminate it.' }
        $management = Enter-RunnerRestartLock (Join-Path $script:RunnerRoot 'bin/management.lock')
        $dataLock = Enter-RunnerRestartLock (Join-Path $data 'controller.lock')
        $scopeLock = Enter-RunnerRestartLock (Get-RunnerScaleLockPath $Request.config)
        $journal = Read-RunnerJson (Join-Path $data 'state.json')
        if (-not $journal -or $journal.runners.Count -ne 0 -or ($journal.Contains('acquisitions') -and $journal.acquisitions.Count -ne 0)) { throw 'Owned work remains in the journal; restart installation refused.' }
        $manifest = Test-RunnerRestartPlan $Request
        Install-RunnerStagedController $manifest
        # Recheck the exact selected update after copying. New edits must never
        # be described as a successful activation of the validated target.
        $null = Assert-RunnerRestartManifest $Request
        $current['startAllowed']=$true
        Write-RunnerJson $Request.requestPath $current
        $scopeLock.Dispose(); $scopeLock=$null; $dataLock.Dispose(); $dataLock=$null
        $task = Get-RunnerStartupTask
        if ($task) {
            $deadline = [DateTime]::UtcNow.AddSeconds(30)
            while ([string]$task.State -eq 'Running' -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 250; $task = Get-RunnerStartupTask }
            if ([string]$task.State -eq 'Running') { throw 'Existing startup task has not released the old invocation; inspect Task Scheduler. No task was stopped.' }
            Start-ScheduledTask -TaskName 'CLC-Linux-Runners' -ErrorAction Stop
        } else { Start-RunnerController $Request.config -Installed }
        $replacement = Wait-RunnerRestartHealthy $Request
        $current['state']='Completed'; $current['pending']=$false; $current['blocker']=''
        $current['lastSuccess']=(Get-Date).ToUniversalTime().ToString('o'); $current['lastFailure']=''
        $current['replacementNonce']=$replacement.Status['nonce']
        Write-RunnerJson $Request.requestPath $current
    } catch {
        $failure = Protect-RunnerText $_.Exception.Message
        $current = Read-RunnerJson $Request.requestPath
        if ($current -and $current.requestId -eq $Request.requestId -and $current.state -in @('ReadyToRestart','Restarting')) {
            $current['state']='Failed'; $current['pending']=$true; $current['blocker']=$failure; $current['lastFailure']=$failure
            Write-RunnerJson $Request.requestPath $current
        }
        throw $failure
    } finally {
        if ($scopeLock) { $scopeLock.Dispose() }; if ($dataLock) { $dataLock.Dispose() }
        if ($management) { $management.Dispose() }; $lease.Dispose()
    }
}
