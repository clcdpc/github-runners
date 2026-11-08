#requires -Version 7.4
[CmdletBinding()]
param([string]$Config = 'local/controller.json', [switch]$FromGitHubCLI, [Security.SecureString]$Token)
. (Join-Path $PSScriptRoot 'Common.ps1')
$localSettings = Initialize-RunnerConfig $Config
if ($FromGitHubCLI) {
    $text = & gh.exe auth token
    if ($LASTEXITCODE) { throw 'GitHub CLI has no available credential.' }
} else {
    if (-not $Token) { $Token = Read-Host 'Organization fine-grained PAT (input hidden)' -AsSecureString }
    $pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($Token)
    try { $text = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer) }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer) }
}
try {
    if (-not $text.StartsWith('github_pat_')) { throw 'An organization-scoped fine-grained PAT is required.' }
    $bytes = [Text.Encoding]::UTF8.GetBytes($text.Trim())
    try {
        $encrypted = [Security.Cryptography.ProtectedData]::Protect($bytes, $null, [Security.Cryptography.DataProtectionScope]::CurrentUser)
        $target = $localSettings.Settings.controller.patFile
        $temporary = "$target.pending"
        [IO.File]::WriteAllBytes($temporary, $encrypted)
        Move-Item -LiteralPath $temporary -Destination $target -Force
    } finally { [Array]::Clear($bytes) }
} finally { $text = $null; $Token = $null }
Write-Host 'PAT stored with Windows CurrentUser DPAPI. Run .\runners.ps1 Activate to validate permissions and activate.'
