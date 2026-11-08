#requires -Version 7.4
param([string]$Config, [string]$ResultPath, [string]$ProbeFile,[switch]$RequirePAT)
. (Join-Path $PSScriptRoot 'Common.ps1')
$probeIdentity=[Security.Principal.WindowsIdentity]::GetCurrent()
$result=[ordered]@{ account=$probeIdentity.Name; accountSid=$probeIdentity.User.Value; sessionId=(Get-Process -Id $PID).SessionId; interactive=[Environment]::UserInteractive; elevated=([Security.Principal.WindowsPrincipal]::new($probeIdentity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)); dpapi=$false; pat=$false; hypervBoot=$false; passed=$false; errors=@() }
try {
    try {
        $plain=[Security.Cryptography.ProtectedData]::Unprotect([IO.File]::ReadAllBytes($ProbeFile),$null,[Security.Cryptography.DataProtectionScope]::CurrentUser)
        try { $result.dpapi=[Text.Encoding]::UTF8.GetString($plain) -ceq 'clc-dpapi-probe' } finally { [Array]::Clear($plain) }
    } catch { $result.errors+=('DPAPI: '+$_.Exception.Message) }
    if ($RequirePAT) {
        try {
            $settings=(Get-RunnerSettings $Config).Settings
            $patBytes=[Security.Cryptography.ProtectedData]::Unprotect([IO.File]::ReadAllBytes($settings.controller.patFile),$null,[Security.Cryptography.DataProtectionScope]::CurrentUser)
            try {
                $prefix=[Text.Encoding]::UTF8.GetBytes('github_pat_')
                $result.pat=$patBytes.Length -gt $prefix.Length
                for ($index=0;$result.pat -and $index -lt $prefix.Length;$index++) { $result.pat=$patBytes[$index] -eq $prefix[$index] }
                if (-not $result.pat) { throw 'Saved PAT is not an organization fine-grained token; run SetToken under the controller account.' }
            } finally { [Array]::Clear($patBytes) }
        } catch { $result.errors+=('Saved PAT: '+$_.Exception.Message) }
    }
    try {
        Invoke-RunnerCommand probe-hyperv $Config
        $result.hypervBoot=$true
    } catch { $result.errors+=('Hyper-V clone: '+$_.Exception.Message) }
    $result.passed=$result.dpapi -and $result.hypervBoot -and (-not $RequirePAT -or $result.pat)
} finally { Write-RunnerJson $ResultPath $result }
if (-not $result.passed) { exit 1 }
exit 0
