#requires -Version 7.4
[CmdletBinding()]
param([string]$Config = 'local/controller.json', [switch]$NoCache)
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Host.ps1')
$null=Initialize-RunnerConfig $Config
Build-RunnerImage -Config $Config -NoCache:$NoCache
