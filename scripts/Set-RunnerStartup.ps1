#requires -Version 7.4
[CmdletBinding()]
param([string]$Config = 'local/controller.json', [ValidateSet('Auto','S4U','Interactive','Password')][string]$Mode = 'Auto',[PSCredential]$Credential)
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
. (Join-Path $PSScriptRoot 'Restart.ps1')
$result = Enable-RunnerStartup -Config $Config -Mode $Mode -Credential $Credential
if ($result.Mode -eq 'Password') {
    $null=New-RunnerRestartRuntime
    Start-RunnerController -Config $Config -Installed
}
Write-Host "Startup ($($result.Mode)): $($result.Summary). Current=$($result.Current)"
