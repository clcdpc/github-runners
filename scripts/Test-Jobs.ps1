#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
$repository = $script:RunnerRoot
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('clc-jobs-test-' + [guid]::NewGuid().ToString('N'))
$script:jobsAssertions = 0
function Assert-Jobs($Condition, [string]$Message) {
    $script:jobsAssertions++
    if (-not $Condition) { throw $Message }
}
try {
    New-Item -ItemType Directory (Join-Path $scratch 'scripts') -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $repository 'runners.ps1') -Destination (Join-Path $scratch 'runners.ps1')
    foreach ($name in @('Dependencies','Host','Startup','Activate','Restart')) { [IO.File]::WriteAllText((Join-Path $scratch "scripts/$name.ps1"), '') }
    # Exercise the real dispatcher in a child process (it calls exit), with all
    # external behavior replaced. Any attempt to lock/manage resources fails.
    $common = @'
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
function Protect-RunnerText([string]$Text) { $Text }
function Enter-RunnerFileLock { throw 'Jobs must not acquire management locks' }
function Request-RunnerRestart([string]$Config) {
    if ($Config -ne 'custom fixture.json') { throw 'RestartWhenIdle lost -Config' }
    'Graceful restart acknowledged promptly'
}
function Invoke-RunnerCommand([string]$Command,[string]$Config,[string[]]$ExtraArguments=@(),[switch]$CaptureOutput) {
    if ($Config -ne 'custom fixture.json') { throw 'Custom -Config was lost' }
    if ($Command -eq 'status') { '{"runtime":{},"state":{},"vms":[]}' ; return }
    if ($Command -ne 'jobs') { throw 'Unexpected mutating command' }
    if ($CaptureOutput) {
        if ($ExtraArguments.Count -ne 1 -or $ExtraArguments[0] -ne '--json') { throw 'Missing JSON flag' }
        '{"schemaVersion":1,"maximumConcurrency":3,"occupiedCapacity":0,"availableCapacity":3,"runners":[]}'
    } else {
        if ($ExtraArguments.Count) { throw 'Default Jobs requested JSON' }
        'CLC GitHub Runners - Active Jobs'
    }
}
'@
    [IO.File]::WriteAllText((Join-Path $scratch 'scripts/Common.ps1'), $common)
    $entry = Join-Path $scratch 'runners.ps1'
    $text = & pwsh.exe -NoProfile -NonInteractive -File $entry Jobs -Config 'custom fixture.json'
    Assert-Jobs ($LASTEXITCODE -eq 0 -and $text -ceq 'CLC GitHub Runners - Active Jobs') 'Default Jobs dispatch/output failed'
    $text = & pwsh.exe -NoProfile -NonInteractive -File $entry Jobs -Config 'custom fixture.json' -Json
    Assert-Jobs ($LASTEXITCODE -eq 0) 'JSON Jobs dispatch failed'
    $report = ($text -join "`n") | ConvertFrom-Json
    Assert-Jobs ($report.schemaVersion -eq 1 -and $report.maximumConcurrency -eq 3 -and $report.runners.Count -eq 0) 'JSON stdout was contaminated'
    $text = & pwsh.exe -NoProfile -NonInteractive -File $entry Status -Config 'custom fixture.json'
    Assert-Jobs ($LASTEXITCODE -eq 0 -and ($text -join "`n") -ceq '{"runtime":{},"state":{},"vms":[]}') 'Existing Status dispatch changed'
    $text = & pwsh.exe -NoProfile -NonInteractive -File $entry RestartWhenIdle -Config 'custom fixture.json'
    Assert-Jobs ($LASTEXITCODE -eq 0 -and ($text -join "`n") -eq 'Graceful restart acknowledged promptly') 'RestartWhenIdle dispatch acquired a management lock, lost config or did not acknowledge'

    & {
        $script:RunnerRoot = $scratch
        $script:readerBuilds = 0
        $live = Join-Path $scratch 'bin/clc-runners.exe'
        New-Item -ItemType Directory (Split-Path $live -Parent) -Force | Out-Null
        [IO.File]::WriteAllText($live, 'active controller executable')
        $before = (Get-FileHash -LiteralPath $live).Hash
        function Get-RunnerBuildState { [pscustomobject]@{ Binary=$live; Current=$false; Fingerprint=('a' * 64) } }
        function Get-RunnerGo { 'Invoke-JobsFixtureGo' }
        function Assert-RunnerBinaryIdle { throw 'Reader build attempted to replace/check the live binary' }
        function git.exe { 'fixture-revision' }
        function Invoke-JobsFixtureGo {
            $script:readerBuilds++
            $destination = $args[[array]::IndexOf($args, '-o') + 1]
            [IO.File]::WriteAllText($destination, 'compiled reader fixture')
            $global:LASTEXITCODE = 0
        }
        $first = Build-RunnerController -ReadOnly
        $second = Build-RunnerController -ReadOnly
        Assert-Jobs ($first -eq $second -and $first -ne $live -and $first.StartsWith((Join-Path $scratch 'bin/readers'))) 'Reader was not isolated from the live executable'
        Assert-Jobs ($script:readerBuilds -eq 1) 'Unchanged reader rebuilt on every Jobs call'
        Assert-Jobs ((Get-FileHash -LiteralPath $live).Hash -eq $before) 'Read-only build replaced the live controller'
    }
    & {
        $script:RunnerRoot=$scratch;$script:stageCalls=[Collections.Generic.List[string]]::new();$script:failStage=$false
        $live=Join-Path $scratch 'bin/clc-runners.exe';$before=(Get-FileHash $live).Hash
        function Get-RunnerBuildState { [pscustomobject]@{Binary=$live;Current=$false;Fingerprint=('b'*64)} }
        function Get-RunnerGo { 'Invoke-StageFixtureGo' }
        function Assert-RunnerBinaryIdle { throw 'Staging attempted to replace the live binary' }
        function git.exe { 'fixture-revision' }
        function Invoke-StageFixtureGo {
            $stageCalls.Add($args[0]);$global:LASTEXITCODE=0
            if ($args[0] -eq 'test' -and $failStage) { $global:LASTEXITCODE=17;return }
            if ($args[0] -eq 'build') { [IO.File]::WriteAllText($args[[array]::IndexOf($args,'-o')+1],'validated stage fixture') }
        }
        $candidate=Build-RunnerController -Stage
        Assert-Jobs (($stageCalls -join ',') -eq 'test,vet,build') 'Staging did not validate before compiling'
        Assert-Jobs ((Read-RunnerJson ($candidate+'.build.json'))['validated']) 'Candidate lacks validation receipt'
        $null=Build-RunnerController -Stage
        Assert-Jobs ($stageCalls.Count -eq 3) 'An unchanged validated candidate rebuilt unnecessarily'
        $script:failStage=$true;$failure='';try { $null=Build-RunnerController -Stage -Force } catch { $failure=$_.Exception.Message }
        Assert-Jobs ($failure -match 'regression tests failed') 'Failed regression tests published an update'
        Assert-Jobs ((Get-FileHash $live).Hash -eq $before) 'Staging changed the active executable'
    }
    Write-Host "Jobs PowerShell dispatch and isolated reader: $jobsAssertions assertions passed."
} finally {
    $script:RunnerRoot = $repository
    $absolute = [IO.Path]::GetFullPath($scratch)
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if ($absolute.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf).StartsWith('clc-jobs-test-')) { Remove-Item -LiteralPath $absolute -Recurse -Force -ErrorAction SilentlyContinue }
}
