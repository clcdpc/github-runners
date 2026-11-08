#requires -Version 7.4
# No real Hyper-V calls. Exercise deletion boundaries and foreign-resource refusal.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'Helpers.ps1')
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('clc-disk-benchmark-test-' + [guid]::NewGuid().ToString('N'))
$script:assertions = 0
function Assert-True([bool]$Value, [string]$Message) { $script:assertions++; if (-not $Value) { throw $Message } }
function Assert-Rejected([scriptblock]$Action) {
    $rejected = $false
    try { & $Action } catch { $rejected = $true }
    Assert-True $rejected 'Unsafe action was accepted.'
}
function New-Fixture {
    $script:root = Join-Path $scratch ([guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path (Join-Path $script:root 'vm') -Force | Out-Null
    $owner = [guid]::NewGuid().ToString()
    $script:record = [pscustomobject]@{schema=1; owner=$owner; name='clc-disk-benchmark'; resourceRoot=$script:root; vmId=[guid]::NewGuid().ToString(); marker="clc-disk-benchmark/v1|$owner"}
    $script:vm = [pscustomobject]@{Id=$script:record.vmId; Name=$script:record.name; Notes=$script:record.marker; State='Running'; ConfigurationLocation=(Join-Path $script:root 'vm')}
    $script:all = @($script:vm)
    $script:drives = @([pscustomobject]@{Path=(Join-Path $script:root 'sql.vhdx')})
    Set-Content -LiteralPath $script:drives[0].Path -Value 'owned fixture'
    Write-BenchJson (Join-Path $script:root 'ownership.json') $script:record
    $script:stops = 0; $script:removes = 0
}
# Function mocks shadow the real cmdlets; this script never imports Hyper-V.
function Get-VM { param($ErrorAction) $script:all }
function Get-VMHardDiskDrive { param($VM) $script:drives }
function Get-VHD { param($Path,$ErrorAction) [pscustomobject]@{ParentPath=''} }
function Get-VMSnapshot { param($VM) @() }
function Stop-VM { param($VM,[switch]$TurnOff,[switch]$Force) $script:stops++ }
function Remove-VM { param($VM,[switch]$Force) $script:removes++ }
function Remove-VMSnapshot { param($Confirm) }

try {
    New-Fixture
    Assert-True ((Assert-BenchPath (Join-Path $root 'sql.vhdx') $root) -eq $drives[0].Path) 'Owned child path rejected.'
    Assert-Rejected { Assert-BenchPath (Join-Path $root '../escape.vhdx') $root }
    Assert-Rejected { Assert-BenchPath ($root + '-other\sql.vhdx') $root }
    Assert-Rejected { Assert-BenchPath $root $root }
    $record.owner = 'invalid'
    Assert-Rejected { Remove-BenchResources $record $root }
    Assert-True ($stops -eq 0 -and $removes -eq 0) 'Invalid ownership modified a VM.'

    New-Fixture
    $vm.Notes = 'clc-runners/v1|production'
    Assert-Rejected { Remove-BenchResources $record $root }
    Assert-True ($stops -eq 0 -and $removes -eq 0) 'Foreign VM was modified.'

    New-Fixture
    $record.vmId = [guid]::NewGuid().ToString()
    Assert-Rejected { Remove-BenchResources $record $root }
    Assert-True ($stops -eq 0 -and $removes -eq 0) 'Name-only match modified a VM.'

    New-Fixture
    $drives[0].Path = Join-Path $scratch 'production.vhdx'
    Assert-Rejected { Remove-BenchResources $record $root }
    Assert-True ($stops -eq 0 -and $removes -eq 0) 'External disk reference modified a VM.'

    New-Fixture
    $script:all += [pscustomobject]@{Id=[guid]::NewGuid().ToString(); Name='production'; Notes=''; State='Running'; ConfigurationLocation=$scratch}
    Assert-Rejected { Remove-BenchResources $record $root }
    Assert-True ($stops -eq 0 -and $removes -eq 0) 'Shared disk reference modified a VM.'

    New-Fixture
    $junction = Join-Path $root 'link'
    $outside = Join-Path $scratch 'outside'
    New-Item -ItemType Directory -Path $outside -Force | Out-Null
    New-Item -ItemType Junction -Path $junction -Target $outside | Out-Null
    Assert-Rejected { Assert-BenchPath (Join-Path $junction 'any.vhdx') $root }
    Remove-Item -LiteralPath $junction -Force

    New-Fixture
    Remove-BenchResources $record $root
    Assert-True ($stops -eq 1 -and $removes -eq 1 -and -not (Test-Path -LiteralPath $root)) 'Owned cleanup failed.'
    Assert-True (Test-Path -LiteralPath $outside) 'Cleanup touched a separate directory.'
    Write-Host "Benchmark safety: $assertions assertions passed. No real VM commands executed."
} finally {
    $absolute = [IO.Path]::GetFullPath($scratch)
    $prefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if ($absolute.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf) -match '^clc-disk-benchmark-test-[a-f0-9]{32}$') {
        foreach ($link in @(Get-ChildItem -LiteralPath $absolute -Recurse -Force -Attributes ReparsePoint -ErrorAction SilentlyContinue)) {
            $null = Assert-BenchPath $link.Parent.FullName $absolute
            Remove-Item -LiteralPath $link.FullName -Force
        }
        Remove-Item -LiteralPath $absolute -Recurse -Force
    }
}
