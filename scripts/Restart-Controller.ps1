#requires -Version 7.4
[CmdletBinding()]
param([Parameter(Mandatory)][string]$RequestPath,[switch]$Check)
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'Startup.ps1')
. (Join-Path $PSScriptRoot 'Restart.ps1')
try {
    $request = Read-RunnerJson $RequestPath
    $requestNonce = [guid]::Empty; $controllerNonce = [guid]::Empty
    if (-not $request -or -not [guid]::TryParse($request.requestId,[ref]$requestNonce) -or
        -not [guid]::TryParse($request.controllerNonce,[ref]$controllerNonce) -or
        -not [IO.Path]::IsPathFullyQualified($request.root) -or -not [IO.Path]::IsPathFullyQualified($request.config)) { throw 'Invalid restart request identity.' }
    $request['requestPath'] = [IO.Path]::GetFullPath($RequestPath)
    if ($Check) { $request['requestPath'] = Join-Path (Split-Path $RequestPath -Parent) 'restart.json' }
    $script:RunnerRoot = $request.root
    if ($Check) {
        $result = @{ready=$false;blocker=''}
        try { $null = Test-RunnerRestartPlan $request; $result.ready=$true }
        catch { $result.blocker=Protect-RunnerText $_.Exception.Message }
        Write-RunnerJson (Join-Path (Split-Path $RequestPath -Parent) 'restart-check.json') $result
        $result | ConvertTo-Json -Compress
    } else { Invoke-RunnerRestartHandoff $request }
} catch {
    Write-Error (Protect-RunnerText $_.Exception.Message)
    exit 1
}
