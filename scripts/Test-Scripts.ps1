#requires -Version 7.4
. (Join-Path $PSScriptRoot 'Common.ps1')
$files = @(Get-ChildItem -LiteralPath $PSScriptRoot -Filter '*.ps1') + @(Get-Item -LiteralPath (Join-Path $script:RunnerRoot 'runners.ps1'))
foreach ($file in $files) {
    $tokens = $null; $parseErrors = $null
    $null = [Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count) { throw ($file.Name + ': ' + ($parseErrors.Message -join '; ')) }
}
Write-Host "PowerShell syntax: $($files.Count) files passed."
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('clc-script-check-' + [guid]::NewGuid().ToString('N'))
try {
    Protect-RunnerDirectory $scratch
    Protect-RunnerDirectory $scratch
    $acl = [IO.FileSystemAclExtensions]::GetAccessControl([IO.DirectoryInfo]::new($scratch), [Security.AccessControl.AccessControlSections]::Access)
    $rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    if (-not $acl.AreAccessRulesProtected -or $rules.Count -ne 2) { throw 'Repeated directory protection did not preserve the intended ACL.' }
    $expected = @([Security.Principal.WindowsIdentity]::GetCurrent().User.Value, 'S-1-5-18')
    foreach ($rule in $rules) { if ($rule.IdentityReference.Value -notin $expected) { throw 'Unexpected principal can access runner data.' } }
    $example = Join-Path $script:RunnerRoot 'config/controller.example.json'
    $configFile = Join-Path $scratch 'controller.json'
    Copy-Item -LiteralPath $example -Destination $configFile
    $resolved = Get-RunnerSettings $configFile
    if ($resolved.Settings.controller.dataDirectory -ne (Join-Path $scratch 'data')) { throw 'Configuration paths did not resolve against their file.' }
    Write-Host 'PowerShell repeated ACL protection and configuration path checks passed.'
} finally {
    # Only the explicitly verified, uniquely named scratch directory is removed.
    $absolute = [IO.Path]::GetFullPath($scratch)
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if ($absolute.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf).StartsWith('clc-script-check-')) { Remove-Item -LiteralPath $absolute -Recurse -Force -ErrorAction SilentlyContinue }
}
& (Join-Path $PSScriptRoot 'Test-Activation.ps1')
& (Join-Path $PSScriptRoot 'Test-Jobs.ps1')
& (Join-Path $PSScriptRoot 'Test-ActivationHelpers.ps1')
& (Join-Path $PSScriptRoot 'Test-PasswordStartup.ps1')
& (Join-Path $PSScriptRoot 'Test-HyperVHelpers.ps1')
& (Join-Path $PSScriptRoot 'Test-RunnerAccount.ps1')
& (Join-Path $PSScriptRoot 'Test-NetworkPolicy.ps1')
& (Join-Path $PSScriptRoot 'Test-ImageConversion.ps1')
& (Join-Path $PSScriptRoot 'Test-Restart.ps1')
