#requires -Version 7.4
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$script:RunnerRoot = Split-Path $PSScriptRoot -Parent

function Protect-RunnerText([string]$Text) {
    $Text -replace '(?i)(github_pat_[a-z0-9_]+|gh[pousr]_[a-z0-9_]+|Bearer\s+\S+)', '[REDACTED]'
}

function Protect-RunnerDirectory([string]$Path) {
    $resolved = [IO.Path]::GetFullPath($Path)
    if ($resolved.TrimEnd('\') -in @([IO.Path]::GetPathRoot($resolved).TrimEnd('\'), $env:USERPROFILE, $script:RunnerRoot)) {
        throw "Refusing to change permissions on a broad directory: $resolved"
    }
    New-Item -ItemType Directory -Path $resolved -Force | Out-Null
    $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $directoryInfo = [IO.DirectoryInfo]::new($resolved)
    $acl = [IO.FileSystemAclExtensions]::GetAccessControl($directoryInfo, [Security.AccessControl.AccessControlSections]::Access)
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($rule in @($acl.GetAccessRules($true, $false, [Security.Principal.SecurityIdentifier]))) { $acl.RemoveAccessRuleAll($rule) }
    $inherit = [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
    foreach ($identity in @($sid, [Security.Principal.SecurityIdentifier]::new('S-1-5-18'))) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($identity, 'FullControl', $inherit, 'None', 'Allow'))
    }
    [IO.FileSystemAclExtensions]::SetAccessControl($directoryInfo, $acl)
}

function Get-RunnerSettings([string]$Config,[switch]$EffectiveFallback) {
    $fullPath = [IO.Path]::GetFullPath($Config, $script:RunnerRoot)
    if (-not (Test-Path -LiteralPath $fullPath) -and -not ($EffectiveFallback -and (Test-Path -LiteralPath ($fullPath+'.effective.json')))) { throw 'Local configuration is missing. Run .\runners.ps1 Activate first.' }
    try {
        $settingsPath = if ($EffectiveFallback -and (Test-Path -LiteralPath ($fullPath+'.effective.json'))) { $fullPath+'.effective.json' } else { $fullPath }
        $settings = Get-Content -Raw -LiteralPath $settingsPath | ConvertFrom-Json
    }
    catch {
        if (-not $EffectiveFallback -or -not (Test-Path -LiteralPath ($fullPath+'.effective.json'))) { throw }
        $settings = Get-Content -Raw -LiteralPath ($fullPath+'.effective.json') | ConvertFrom-Json
    }
    $base = Split-Path $fullPath -Parent
    if ($settings.PSObject.Properties.Name -notcontains 'storageRoot') {
        $settings | Add-Member -NotePropertyName storageRoot -NotePropertyValue '.'
    } elseif ([string]::IsNullOrEmpty($settings.storageRoot)) {
        $settings.storageRoot='.'
    }
    $settings.storageRoot=[IO.Path]::GetFullPath($settings.storageRoot,$base)
    if ($settings.controller.PSObject.Properties.Name -notcontains 'startupRunLevel') {
        $settings.controller | Add-Member -NotePropertyName startupRunLevel -NotePropertyValue 'Limited'
    } elseif ([string]::IsNullOrEmpty($settings.controller.startupRunLevel)) {
        $settings.controller.startupRunLevel='Limited'
    }
    if ($settings.controller.startupRunLevel -cnotin @('Limited','Highest')) { throw 'controller.startupRunLevel must be Limited or Highest.' }
    foreach ($key in @('dataDirectory','logDirectory','patFile')) {
        $settings.controller.$key = [IO.Path]::GetFullPath($settings.controller.$key, $settings.storageRoot)
    }
    if ($settings.PSObject.Properties.Name -contains 'hyperv') {
        if ($settings.hyperv.PSObject.Properties.Name -notcontains 'diskGrowthGiB') { $settings.hyperv | Add-Member -NotePropertyName diskGrowthGiB -NotePropertyValue 20 }
        $settings.hyperv.imageDirectory=[IO.Path]::GetFullPath($settings.hyperv.imageDirectory,$settings.storageRoot)
        foreach ($key in @('imageRecipe','script')) { $settings.hyperv.$key = [IO.Path]::GetFullPath($settings.hyperv.$key, $base) }
    }
    [pscustomobject]@{ Path = $fullPath; Settings = $settings }
}

function Initialize-RunnerConfig([string]$Config) {
    $fullPath = [IO.Path]::GetFullPath($Config, $script:RunnerRoot)
    if (-not (Test-Path -LiteralPath $fullPath)) {
        Protect-RunnerDirectory (Split-Path $fullPath -Parent)
        Copy-Item -LiteralPath (Join-Path $script:RunnerRoot 'config/controller.example.json') -Destination $fullPath
        $newSettings=Read-RunnerJson $fullPath
        $newSettings.hyperv.script=Join-Path $script:RunnerRoot 'scripts/HyperV.ps1'
        $newSettings.hyperv.imageRecipe=Join-Path $script:RunnerRoot 'image/recipe.json'
        Write-RunnerJson $fullPath $newSettings
    }
    Update-RunnerConfig $fullPath
    $localSettings = Get-RunnerSettings $fullPath
    foreach ($directory in @($localSettings.Settings.controller.dataDirectory, $localSettings.Settings.controller.logDirectory, (Split-Path $localSettings.Settings.controller.patFile -Parent))) { Protect-RunnerDirectory $directory }
    Protect-RunnerDirectory $localSettings.Settings.hyperv.imageDirectory
    $localSettings
}

function Update-RunnerConfig([string]$Path) {
    $old=Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json -AsHashtable
    if (-not $old.Contains('wslc')) { return }
    if ($old.Contains('hyperv')) { throw 'Configuration contains both legacy and Hyper-V settings. Resolve this ambiguity explicitly.' }
    $data=(Get-RunnerSettings $Path).Settings.controller.dataDirectory
    $state=Read-RunnerJson (Join-Path $data 'state.json')
    if ($state -and $state.runners.Count) { throw 'Legacy journal contains runners. Drain and Cleanup with the previous controller before migrating; no WSLC resources were deleted.' }
    if (@(Get-RunnerControllerProcesses).Count) { throw 'Stop and drain the existing controller before migrating configuration.' }
    $lock=Enter-RunnerFileLock (Join-Path $data 'controller.lock')
    try {
        # A one-time, read-only check of the retiring runtime prevents abandoning
        # an unjournaled owned container. Never stop/delete WSLC resources here.
        if ($state -and (Get-Command $old.wslc.executable -ErrorAction SilentlyContinue)) {
            $args=@(); if ($old.wslc.session) { $args+=@('--session',$old.wslc.session) }
            $inventory=& $old.wslc.executable @args list --all --no-trunc --format json 2>&1
            if ($LASTEXITCODE) { throw 'Cannot inspect legacy WSLC ownership during migration. Recover the old session and run its Cleanup before migrating.' }
            foreach ($line in $inventory) {
                if (-not ([string]$line).Trim()) { continue }
                $row=([string]$line) | ConvertFrom-Json
                if ($row.Names.StartsWith($old.github.scaleSet+'-'+$state.owner.Substring(0,8)+'-')) {
                    $json=& $old.wslc.executable @args inspect --type container $row.ID 2>&1
                    if ($LASTEXITCODE) { throw 'Cannot verify a legacy candidate container; migration stopped without deleting it.' }
                    foreach ($item in @(($json -join "`n") | ConvertFrom-Json)) {
                        if ($item.Config.Labels.'io.clcdpc.github-runners.owner' -ceq $state.owner -and $item.Config.Labels.'io.clcdpc.github-runners.scale-set' -ceq $old.github.scaleSet) { throw 'Application-owned legacy WSLC container remains. Run Cleanup with the previous controller, then rerun Activate. No resources were deleted.' }
                    }
                }
            }
        }
        $backup=$Path+'.wslc-v1.bak'
        if (-not (Test-Path -LiteralPath $backup)) { Copy-Item -LiteralPath $Path -Destination $backup }
        $example=Get-Content -LiteralPath (Join-Path $script:RunnerRoot 'config/controller.example.json') -Raw | ConvertFrom-Json -AsHashtable
        $old.hyperv=$example.hyperv
        # Template paths are repository-relative even for an external config.
        $old.hyperv.script=Join-Path $script:RunnerRoot 'scripts/HyperV.ps1'
        $old.hyperv.imageRecipe=Join-Path $script:RunnerRoot 'image/recipe.json'
        [void]$old.Remove('wslc'); [void]$old.runners.Remove('image')
        Write-RunnerJson $Path $old
        Write-Host 'Migrated configuration to Hyper-V; GitHub settings, credentials and controller paths preserved. Exact legacy configuration backup saved.'
    } finally { $lock.Dispose() }
}

function Get-RunnerGo {
    $goPath = Join-Path $script:RunnerRoot 'tools/go/bin/go.exe'
    if (-not (Test-Path -LiteralPath $goPath)) { throw 'Pinned Go toolchain missing. Run .\runners.ps1 Install.' }
    $goPath
}

function Get-RunnerBinary { Join-Path $script:RunnerRoot 'bin/clc-runners.exe' }

function Get-RunnerGoVersion([string]$GoPath) {
    $previousToolchain=$env:GOTOOLCHAIN
    try {
        $env:GOTOOLCHAIN='local'
        $version=(& $GoPath version) -join ''
        if ($LASTEXITCODE) { throw 'Cannot inspect the pinned Go toolchain. Run Activate to repair it.' }
        $version.Trim()
    } finally { $env:GOTOOLCHAIN=$previousToolchain }
}

function Get-RunnerFingerprint([string[]]$Paths, [string]$Context = '') {
    $lines = @($Context)
    foreach ($path in ($Paths | Sort-Object -CaseSensitive)) {
        $name = [IO.Path]::GetRelativePath($script:RunnerRoot, $path).Replace('\','/')
        $lines += $name + ':' + (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
    }
    [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes(($lines -join "`n")))).ToLowerInvariant()
}

function Get-RunnerBuildState {
    $goPath = Get-RunnerGo
    $binary = Get-RunnerBinary
    $sources = @(Get-ChildItem -LiteralPath (Join-Path $script:RunnerRoot 'cmd'), (Join-Path $script:RunnerRoot 'internal') -File -Recurse | Where-Object { $_.Name -notlike '*_test.go' })
    $sources += Get-Item -LiteralPath (Join-Path $script:RunnerRoot 'go.mod'), (Join-Path $script:RunnerRoot 'go.sum')
    $goVersion = Get-RunnerGoVersion $goPath
    $fingerprint = Get-RunnerFingerprint $sources.FullName ("windows/amd64;trimpath;" + $goVersion)
    $current = $false
    $metadata = Read-RunnerJson ($binary + '.build.json')
    if ($metadata -and $metadata['inputs'] -eq $fingerprint -and (Test-Path -LiteralPath $binary)) {
        $current = $metadata['binary'] -eq (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash
    }
    [pscustomobject]@{ Current=$current; Fingerprint=$fingerprint; Binary=$binary }
}

function Read-RunnerJson([string]$Path) {
    if (Test-Path -LiteralPath $Path) {
        try { return Get-Content -Raw -LiteralPath $Path | ConvertFrom-Json -AsHashtable }
        catch { Write-Verbose "Cannot read JSON metadata at $Path" }
    }
    return $null
}

function Write-RunnerJson([string]$Path, $Value) {
    $temporary = $Path + '.' + [guid]::NewGuid().ToString('N') + '.pending'
    try {
        $Value | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $temporary -Encoding utf8NoBOM
        Move-Item -LiteralPath $temporary -Destination $Path -Force
    } finally { if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary } }
}

function Enter-RunnerFileLock([string]$Path) {
    New-Item -ItemType Directory -Force (Split-Path $Path -Parent) | Out-Null
    try { return [IO.File]::Open($Path, [IO.FileMode]::OpenOrCreate, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None) }
    catch { throw "Runner operation is locked ($Path). Another controller or management command may be active; wait for it and rerun. No processes were terminated." }
}

function Get-RunnerControllerProcesses {
    $binary = Get-RunnerBinary
    @(Get-Process -Name 'clc-runners' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $binary })
}

function Assert-RunnerBinaryIdle {
    if (@(Get-RunnerControllerProcesses).Count) { throw 'The controller executable is in use. Run Activate with the active configuration to drain it before updating; other configurations sharing this executable must also be stopped.' }
}

function Install-RunnerControllerBinary([string]$Candidate,$Metadata) {
    $binary=Get-RunnerBinary
    Assert-RunnerBinaryIdle
    $temporary=$binary+'.'+[guid]::NewGuid().ToString('N')+'.pending'
    try {
        Copy-Item -LiteralPath $Candidate -Destination $temporary
        if (-not $Metadata -or (Get-FileHash -LiteralPath $temporary -Algorithm SHA256).Hash -ne $Metadata['binary']) { throw 'Candidate executable changed during installation; previous installation preserved.' }
        if (Test-Path -LiteralPath $binary) {
            $installed=Read-RunnerJson ($binary+'.build.json')
            # A corrupt/unrecognized current file must not rotate away the
            # recoverable previous installation. Keep it as separate evidence.
            $backup=if ($installed -and $installed['binary'] -eq (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash) { $binary+'.previous' } else { $binary+'.unvalidated-'+[guid]::NewGuid().ToString('N') }
            if (Test-Path -LiteralPath ($binary+'.build.json')) { Copy-Item -LiteralPath ($binary+'.build.json') -Destination ($backup+'.build.json') -Force }
            [IO.File]::Replace($temporary,$binary,$backup)
        } else { Move-Item -LiteralPath $temporary -Destination $binary }
        Write-RunnerJson ($binary+'.build.json') $Metadata
    } finally { if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary } }
}

function Build-RunnerController([switch]$Force, [switch]$ReadOnly, [switch]$Stage) {
    $state = Get-RunnerBuildState
    $binary = $state.Binary
    $current = $state.Current
    if ($ReadOnly -or $Stage) {
        # A content-addressed reader can inspect older live controllers without
        # replacing their executable or requiring a drain/activation.
        $kind = if ($Stage) { 'staged' } else { 'readers' }
        $binary = Join-Path $script:RunnerRoot ("bin/$kind/" + $state.Fingerprint + '/clc-runners.exe')
        $metadata = Read-RunnerJson ($binary + '.build.json')
        $current = $metadata -and $metadata['inputs'] -eq $state.Fingerprint -and (Test-Path -LiteralPath $binary) -and $metadata['binary'] -eq (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash
        if ($Stage -and $metadata -and -not $metadata['validated']) { $current=$false }
    }
    if ($Force -or -not $current) {
        if (-not $ReadOnly -and -not $Stage) { Assert-RunnerBinaryIdle }
        $goPath = Get-RunnerGo
        New-Item -ItemType Directory -Force (Split-Path $binary -Parent) | Out-Null
        $temporary = Join-Path (Split-Path $binary -Parent) ('build-' + [guid]::NewGuid().ToString('N') + '.exe')
        Push-Location $script:RunnerRoot
        $previousToolchain = $env:GOTOOLCHAIN
        try {
            $env:GOTOOLCHAIN = 'local'
            if ($Stage) {
                & $goPath test -count=1 ./...
                if ($LASTEXITCODE) { throw 'Controller regression tests failed; no update was staged.' }
                & $goPath vet ./...
                if ($LASTEXITCODE) { throw 'Controller vet failed; no update was staged.' }
            }
            $revision = & git.exe rev-parse --short HEAD
            & $goPath build -trimpath -ldflags "-X main.version=$revision" -o $temporary ./cmd/clc-runners
            if ($LASTEXITCODE) { throw 'Controller compilation failed; the previous executable is intact.' }
            if ((Get-RunnerBuildState).Fingerprint -ne $state.Fingerprint) { throw 'Controller sources changed during build/validation; previous installation is intact. Rerun Activate.' }
            if (-not $ReadOnly -and -not $Stage) { Assert-RunnerBinaryIdle }
            $metadata=@{inputs=$state.Fingerprint;binary=(Get-FileHash -LiteralPath $temporary -Algorithm SHA256).Hash;validated=[bool]$Stage}
            if (-not $ReadOnly -and -not $Stage) { Install-RunnerControllerBinary $temporary $metadata }
            else { Move-Item -LiteralPath $temporary -Destination $binary -Force;Write-RunnerJson ($binary+'.build.json') $metadata }
        } finally {
            $env:GOTOOLCHAIN = $previousToolchain
            Pop-Location
            if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary }
        }
    }
    $binary
}

function Invoke-RunnerCommand([string]$Command, [string]$Config, [string[]]$ExtraArguments = @(), [switch]$CaptureOutput) {
    $binary = Get-RunnerBinary
    if ($Command -eq 'jobs') {
        $ExtraArguments += @('--controller-executable', $binary)
        $binary = Build-RunnerController -ReadOnly
    } elseif ($Command -ne 'status' -or -not (Test-Path -LiteralPath $binary)) { $binary = Build-RunnerController }
    $settings = Get-RunnerSettings $Config -EffectiveFallback:($Command -in @('status','jobs'))
    if ($Command -in @('prepare-image','test-hyperv','probe-hyperv') -and -not $CaptureOutput) {
        # Do not buffer a long validation run: Go/SSH emit redacted progress and
        # also persist it outside the disposable VM before cleanup.
        $recent = [Collections.Generic.Queue[string]]::new()
        & $binary $Command --config $settings.Path @ExtraArguments 2>&1 | ForEach-Object {
            $line = Protect-RunnerText ([string]$_)
            Write-Host $line
            $recent.Enqueue($line)
            if ($recent.Count -gt 32) { $null = $recent.Dequeue() }
        }
        if ($LASTEXITCODE) { throw ("Controller $Command failed (exit $LASTEXITCODE). " + ($recent -join "`n")) }
        return
    }
    $output = & $binary $Command --config $settings.Path @ExtraArguments 2>&1
    if ($LASTEXITCODE) { throw ("Controller $Command failed (exit $LASTEXITCODE). " + ($output -join "`n")) }
    if ($CaptureOutput) { return ($output -join "`n") }
    $output | ForEach-Object { Write-Host $_ }
}

function Get-RunnerRuntime([string]$Config,[switch]$EffectiveFallback) {
    $localSettings = Get-RunnerSettings $Config -EffectiveFallback:$EffectiveFallback
    $status = Read-RunnerJson (Join-Path $localSettings.Settings.controller.dataDirectory 'status.json')
    $alive = $false
    if ($status -and $status['pid'] -and $status['startedAt']) {
        $existing = Get-Process -Id $status['pid'] -ErrorAction SilentlyContinue
        if ($existing -and $existing.Path -eq (Get-RunnerBinary)) {
            # PS 7.6 deserializes ISO timestamps as DateTime by default. A
            # Parse(string) round trip loses fractional seconds under that
            # version and can make a genuine process look older than status.
            $age = (([DateTimeOffset]$status['startedAt']).UtcDateTime - $existing.StartTime.ToUniversalTime()).TotalSeconds
            $alive = $age -ge 0 -and $age -lt 60
        }
    }
    # The process remains alive briefly after writing "stopped" while releasing
    # the journal lock. Do not replace its executable in that interval.
    $healthy = $alive -and $status['status'] -eq 'listening' -and ((Get-Date).ToUniversalTime() - ([DateTimeOffset]$status['updatedAt']).UtcDateTime).TotalSeconds -lt [Math]::Max(60, 3 * $localSettings.Settings.controller.reconcileIntervalSeconds)
    [pscustomobject]@{ Alive=$alive; Healthy=$healthy; Status=$status }
}

function Stop-RunnerController([string]$Config, [switch]$Wait, [int]$TimeoutSeconds = 30) {
    $localSettings = Get-RunnerSettings $Config
    $runtime = Get-RunnerRuntime $Config
    if (-not $runtime.Alive) { Write-Host 'Controller is not running.'; return }
    $nonce = [guid]::Empty
    if (-not [guid]::TryParse($runtime.Status['nonce'], [ref]$nonce)) { throw 'Live controller has no valid stop nonce. Inspect its status and logs; no PID was terminated.' }
    Write-RunnerJson (Join-Path $localSettings.Settings.controller.dataDirectory 'stop.json') @{nonce=$runtime.Status['nonce']}
    Write-Host 'Graceful stop requested. Active jobs will finish before the controller exits.'
    if ($Wait) {
        $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
        while ((Get-RunnerRuntime $Config).Alive) {
            if ((Get-Date) -ge $deadline) { throw 'Controller is still draining active jobs or remote cleanup. No jobs were terminated and no executable/image was replaced. Wait for Stop/Status to show the process exited, then rerun Activate.' }
            Start-Sleep -Milliseconds 500
        }
    }
}

function Invoke-RunnerStopped([string]$Config, [scriptblock]$Operation) {
    $localSettings = Get-RunnerSettings $Config
    if ((Get-RunnerRuntime $Config).Alive) { throw 'Controller is running. Run Activate to drain before updating, or Stop and wait for it to exit.' }
    $lock = Enter-RunnerFileLock (Join-Path $localSettings.Settings.controller.dataDirectory 'controller.lock')
    try {
        $scopeLock = Enter-RunnerScopeLock $Config
        try { & $Operation } finally { $scopeLock.Dispose() }
    } finally { $lock.Dispose() }
}

function Get-RunnerScaleLockPath([string]$Config) {
    $settings = (Get-RunnerSettings $Config).Settings
    $organization = ([uri]$settings.github.organizationUrl).AbsolutePath.Trim('/')
    $scope = ($organization + '|' + $settings.github.runnerGroup + '|' + $settings.github.scaleSet).ToLowerInvariant()
    $hash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($scope))).ToLowerInvariant()
    Join-Path ([Environment]::GetFolderPath('LocalApplicationData')) "clc-runners/locks/$hash.lock"
}

function Enter-RunnerScopeLock([string]$Config) { Enter-RunnerFileLock (Get-RunnerScaleLockPath $Config) }

function Start-RunnerController([string]$Config, [switch]$Foreground, [switch]$Installed) {
    $localSettings = Get-RunnerSettings $Config
    $runtime = Get-RunnerRuntime $Config
    if ($runtime.Alive) { Write-Host "Controller already running (PID $($runtime.Status['pid']), $($runtime.Status['status']))."; return }
    $binary = if ($Installed) { Get-RunnerBinary } else { Build-RunnerController }
    if ($Foreground) {
        & $binary run --config $localSettings.Path
        if ($LASTEXITCODE) { throw 'Controller stopped with an error.' }
        return
    }
    if (Get-Command Get-RunnerStartupTask -CommandType Function -ErrorAction SilentlyContinue) {
        $startupTask=Get-RunnerStartupTask
        if ($startupTask -and [string]$startupTask.Principal.LogonType -eq 'Password') {
            if (-not (Test-RunnerStartupTask $startupTask $Config Password)) { throw 'Password startup task ownership/settings differ; repair with EnableStartup -StartupMode Password.' }
            $validation=Get-RunnerStartupValidation $Config -Binary $binary
            if (-not $validation -or $validation['validationMode'] -ne 'Password' -or -not $validation.modes.Password.passed) { throw 'Noninteractive startup validation is stale. Run TestUnattended -StartupMode Password, then EnableStartup -StartupMode Password.' }
            Start-ScheduledTask -TaskName 'CLC-Linux-Runners' -ErrorAction Stop
            $null=Wait-RunnerHealthy $Config -TimeoutSeconds 120 -StartupGraceSeconds 15
            Write-Host 'Controller started through its Password task; no interactive sign-in session is required.'
            return
        }
    }
    # Check the same lock as the controller. The controller acquires it itself
    # after launch too, fencing scheduled tasks and non-script invocations.
    $lock = Enter-RunnerFileLock (Join-Path $localSettings.Settings.controller.dataDirectory 'controller.lock')
    try { $scopeLock = Enter-RunnerScopeLock $Config; $scopeLock.Dispose() } finally { $lock.Dispose() }
    $errorFile = Join-Path $localSettings.Settings.controller.dataDirectory ('startup-' + [guid]::NewGuid().ToString('N') + '.stderr.log')
    $arguments = @('run','--config',('"' + $localSettings.Path + '"'),'--quiet')
    $process = Start-Process -FilePath $binary -ArgumentList $arguments -WindowStyle Hidden -PassThru -RedirectStandardError $errorFile
    Start-Sleep -Seconds 2
    $process.Refresh()
    if ($process.HasExited -and -not (Get-RunnerRuntime $Config).Alive) { throw ('Controller could not start. ' + (Get-Content -Raw -LiteralPath $errorFile)) }
    Write-Host 'Controller process started. Run .\runners.ps1 Status for listener health.'
}

function Wait-RunnerHealthy([string]$Config, [int]$TimeoutSeconds = 30,[int]$StartupGraceSeconds=0) {
    $began=Get-Date
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        $runtime = Get-RunnerRuntime $Config
        if ($runtime.Healthy) { return $runtime }
        if (-not $runtime.Alive -and -not @(Get-RunnerControllerProcesses).Count -and ((Get-Date)-$began).TotalSeconds -ge $StartupGraceSeconds) { throw 'Controller exited before listening. Inspect startup stderr and controller logs, correct the problem, then rerun Activate.' }
        if ($runtime.Alive -and $runtime.Status['status'] -in @('draining','retrying','stopped')) { throw "Controller is $($runtime.Status['status']), not listening. Inspect controller logs, correct the problem, then rerun Activate." }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)
    throw 'Controller has not reached a healthy listening state. Inspect controller logs and rerun Activate; its process and jobs have been preserved.'
}

function Get-RunnerImage([string]$Config,[string]$Binary) {
    if ($Binary) {
        $output = & $Binary image-status --config (Get-RunnerSettings $Config).Path 2>&1
        if ($LASTEXITCODE) { throw (Protect-RunnerText ($output -join "`n")) }
        $status = ($output -join "`n") | ConvertFrom-Json -AsHashtable
    } else { $status=Invoke-RunnerCommand image-status $Config -CaptureOutput | ConvertFrom-Json -AsHashtable }
    if (-not $status.image) { return $null }
    @{ Id=$status.image.id; Inputs=$status.image.inputs; Current=$status.current; Reason=$status.reason; ChangedInputs=$status.changedInputs; Version=$status.image.version; Path=$status.image.path }
}

function Test-RunnerImageCurrent($Image) { $Image -and $Image.Current }

function Build-RunnerImage([string]$Config, [switch]$NoCache) {
    Ensure-RunnerHost $Config
    if ((Get-RunnerRuntime $Config).Alive) { throw 'Drain the controller before image preparation. Run Stop and wait, or Activate -ForceRebuild.' }
    Install-RunnerImageTools $Config
    $extra=@(); if ($NoCache) { $extra+='--force' }
    Invoke-RunnerCommand prepare-image $Config $extra
}

function Test-RunnerInstallation([string]$Config) {
    Invoke-RunnerCommand validate $Config
    Ensure-RunnerHost $Config
    $image = Get-RunnerImage $Config
    if (-not $image) { throw 'Ubuntu base image missing. Run PrepareImage or Activate.' }
    if (-not (Test-RunnerImageCurrent $image)) { throw "Ubuntu base image does not match the configuration ($($image.Reason)); rerun Activate to prepare a validated image." }
    Write-Host 'Configuration, Hyper-V and immutable Ubuntu base image ready. Live guest validation uses TestHyperV.'
}
