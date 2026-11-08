#requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Config,
    [Parameter(Mandatory)][string]$AccountSid,
    [switch]$RefreshAuthentication,[switch]$ForceRebuild,[switch]$NoCache
)
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Dependencies.ps1')
. (Join-Path $PSScriptRoot 'Host.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
. (Join-Path $PSScriptRoot 'Activate.ps1')
. (Join-Path $PSScriptRoot 'Restart.ps1')
[Console]::InputEncoding=[Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false)
$OutputEncoding=[Text.UTF8Encoding]::new($false)

function Read-RunnerAccountSetupMessage {
    $line=[Console]::ReadLine()
    if (-not $line) { throw 'Administrator setup channel closed.' }
    $message=$line | ConvertFrom-Json -AsHashtable
    if ($message.version -ne 1) { throw 'Unsupported account setup message.' }
    $message
}

function Request-RunnerAccountSetup([ValidateSet('TOKEN','STARTUP')][string]$Operation) {
    # Activation progress uses Write-Host -NoNewline; begin protocol messages
    # on their own line so the parent can distinguish them from diagnostics.
    [Console]::Out.WriteLine()
    [Console]::Out.WriteLine('CLC-ACCOUNT-SETUP:'+$script:AccountSetupChannel+':'+$Operation)
    [Console]::Out.Flush()
    Read-RunnerAccountSetupMessage
}

$lock=$null
$credential=$null
$message=$null
try {
    $identity=[Security.Principal.WindowsIdentity]::GetCurrent()
    if ($identity.User.Value -ne $AccountSid -or (Test-RunnerAdministrator)) { throw 'Setup worker must run as the requested non-admin local account.' }
    if (-not [Console]::IsInputRedirected) { throw 'Run Setup-RunnerAccount.ps1 from the administrator session.' }
    $local=Get-RunnerSettings $Config
    if ($local.Settings.controller.startupRunLevel -ne 'Limited') { throw 'Account setup requires controller.startupRunLevel to be Limited.' }
    $message=Read-RunnerAccountSetupMessage
    if (-not $message.password) { throw 'Windows credential was not supplied through the setup channel.' }
    $channelGuid=[guid]::Empty
    if (-not [guid]::TryParseExact([string]$message.channel,'N',[ref]$channelGuid)) { throw 'Invalid account setup channel.' }
    $script:AccountSetupChannel=$message.channel
    $credential=[PSCredential]::new($identity.Name,(ConvertTo-SecureString $message.password -AsPlainText -Force))
    $message=$null
    $lock=Enter-RunnerFileLock (Join-Path $script:RunnerRoot 'bin/management.lock')
    $tokenProvider={
        $response=Request-RunnerAccountSetup TOKEN
        try {
            if (-not $response.token) { throw 'PAT was not supplied through the setup channel.' }
            ConvertTo-SecureString $response.token -AsPlainText -Force
        } finally { $response=$null }
    }
    $registrar={
        $response=Request-RunnerAccountSetup STARTUP
        if (-not $response.ok) { throw ('Administrator startup registration failed: '+$response.error) }
    }
    Invoke-RunnerActivation -Config $local.Path -StartupMode Password -StartupCredential $credential -TokenProvider $tokenProvider -RefreshAuthentication:$RefreshAuthentication -PasswordTaskRegistrar $registrar -ForceRebuild:$ForceRebuild -NoCache:$NoCache
} catch {
    $errorText=$_.Exception.Message
    if ($credential) { $errorText=$errorText.Replace($credential.GetNetworkCredential().Password,'[REDACTED]') }
    if ($message -and $message.Contains('password') -and $message.password) { $errorText=$errorText.Replace($message.password,'[REDACTED]') }
    [Console]::Error.WriteLine((Protect-RunnerText $errorText))
    exit 1
} finally {
    $message=$null
    if ($credential) { $credential.Password.Dispose(); $credential=$null }
    if ($lock) { $lock.Dispose() }
}
