#requires -Version 7.4
[CmdletBinding()]
param([string]$Config = 'local/controller.json')
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Dependencies.ps1')
Install-RunnerDependencies -Config $Config
