#requires -Version 7.4

function Install-RunnerDependencies([string]$Config) {
    if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') { throw 'This installation supports Windows x64.' }
    $goPath = Join-Path $script:RunnerRoot 'tools/go/bin/go.exe'
    $goCurrent = $false
    if (Test-Path -LiteralPath $goPath) {
        try {
            $version = Get-RunnerGoVersion $goPath
            $goCurrent = $version -eq 'go version go1.27.3 windows/amd64'
            foreach ($required in @('src/runtime/runtime2.go','pkg/tool/windows_amd64/compile.exe','pkg/tool/windows_amd64/link.exe')) {
                if (-not (Test-Path -LiteralPath (Join-Path $script:RunnerRoot "tools/go/$required"))) { $goCurrent=$false }
            }
        } catch { $goCurrent=$false }
    }
    New-Item -ItemType Directory -Force (Join-Path $script:RunnerRoot 'tools') | Out-Null
    # Keep the SDK's compiler fixtures outside our module without rewriting a
    # correct file or downloading an already installed pinned toolchain.
    $modulePath = Join-Path $script:RunnerRoot 'tools/go.mod'
    $module = "module tools.invalid`n`ngo 1.27.3`n"
    if (-not (Test-Path -LiteralPath $modulePath) -or [IO.File]::ReadAllText($modulePath) -ne $module) { [IO.File]::WriteAllText($modulePath, $module) }
    if (-not $goCurrent) {
        $archive = Join-Path $script:RunnerRoot 'tools/go1.27.3.windows-amd64.zip'
        $stage = Join-Path $script:RunnerRoot ('tools/go-install-' + [guid]::NewGuid().ToString('N'))
        $backup = Join-Path $script:RunnerRoot ('tools/go-previous-' + [guid]::NewGuid().ToString('N'))
        $installedGo = Join-Path $script:RunnerRoot 'tools/go'
        $promoted = $false
        try {
            Invoke-WebRequest 'https://go.dev/dl/go1.27.3.windows-amd64.zip' -OutFile $archive
            if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne 'F26081F438319757F8A1B46D7D0B2C5EC7583D4038E485E0AA725CFDE7885FFF') { throw 'Go archive SHA256 mismatch.' }
            # Complete extraction before touching the installed SDK. An
            # interrupted download/extraction cannot masquerade as a current
            # installation merely because go.exe was extracted first.
            Expand-Archive -LiteralPath $archive -DestinationPath $stage
            $stagedGo = Join-Path $stage 'go/bin/go.exe'
            $version = Get-RunnerGoVersion $stagedGo
            if ($version -ne 'go version go1.27.3 windows/amd64') { throw 'Downloaded Go toolchain failed verification.' }
            $toolsRoot=[IO.Path]::GetFullPath((Join-Path $script:RunnerRoot 'tools')).TrimEnd('\') + '\'
            foreach ($target in @($stage,$backup,$installedGo)) {
                if (-not [IO.Path]::GetFullPath($target).StartsWith($toolsRoot,[StringComparison]::OrdinalIgnoreCase)) { throw 'Go installation path escaped the tools directory.' }
            }
            if (Test-Path -LiteralPath $installedGo) { Move-Item -LiteralPath $installedGo -Destination $backup }
            try { Move-Item -LiteralPath (Join-Path $stage 'go') -Destination $installedGo; $promoted=$true }
            catch {
                if (-not (Test-Path -LiteralPath $installedGo) -and (Test-Path -LiteralPath $backup)) { Move-Item -LiteralPath $backup -Destination $installedGo }
                throw
            }
        } finally {
            if (Test-Path -LiteralPath $archive) { Remove-Item -LiteralPath $archive }
            $absolute=[IO.Path]::GetFullPath($stage)
            $toolsRoot=[IO.Path]::GetFullPath((Join-Path $script:RunnerRoot 'tools')).TrimEnd('\') + '\'
            if ($absolute.StartsWith($toolsRoot,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $absolute -Leaf).StartsWith('go-install-')) { Remove-Item -LiteralPath $absolute -Recurse -Force -ErrorAction SilentlyContinue }
            $previous=[IO.Path]::GetFullPath($backup)
            if ($promoted -and $previous.StartsWith($toolsRoot,[StringComparison]::OrdinalIgnoreCase) -and (Split-Path $previous -Leaf).StartsWith('go-previous-')) { Remove-Item -LiteralPath $previous -Recurse -Force -ErrorAction SilentlyContinue }
        }
        $version = Get-RunnerGoVersion $goPath
        if ($version -ne 'go version go1.27.3 windows/amd64') { throw 'Pinned Go installation failed. Rerun Activate to repair it.' }
    }
    Write-Host 'Windows x64 and pinned Go 1.27.3 ready. Hyper-V is checked separately.'
}
