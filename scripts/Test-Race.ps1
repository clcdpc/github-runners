#requires -Version 7.4
param([string]$Config='local/controller.json')
. (Join-Path $PSScriptRoot 'Common.ps1')
$go=Get-RunnerGo
if (-not (Get-Command gcc.exe -ErrorAction SilentlyContinue) -and -not $env:CC) { throw 'Windows Go race tests need a supported C compiler (for example MSYS2 UCRT64 GCC) on PATH or CC. Install it explicitly, then rerun TestRace. Ordinary Test does not require it.' }
$previousCGO=$env:CGO_ENABLED
Push-Location $script:RunnerRoot
try {
    $env:CGO_ENABLED='1'
    & $go test -race -count=1 ./...
    if ($LASTEXITCODE) { throw 'Go race detector tests failed.' }
} finally { $env:CGO_ENABLED=$previousCGO; Pop-Location }
