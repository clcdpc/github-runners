#requires -Version 7.4
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-BenchJson([string]$Path, $Value) {
    [IO.File]::WriteAllText($Path, ($Value | ConvertTo-Json -Depth 30), [Text.UTF8Encoding]::new($false))
}

function Assert-BenchPath([string]$Path, [string]$Root) {
    $full = [IO.Path]::GetFullPath($Path)
    $rootPath = [IO.Path]::GetFullPath($Root).TrimEnd('\','/')
    if (-not $full.StartsWith($rootPath + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Benchmark path escapes its dedicated directory: $full"
    }
    $cursor = $full
    while ($cursor -and $cursor.Length -ge $rootPath.Length) {
        if ((Test-Path -LiteralPath $cursor) -and ((Get-Item -LiteralPath $cursor -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw 'Benchmark paths must not contain reparse points.'
        }
        $cursor = Split-Path $cursor -Parent
    }
    $full
}

function Assert-BenchManifest($Manifest, [string]$Root) {
    $id = [guid]::Empty
    $absolute = [IO.Path]::GetFullPath($Root).TrimEnd('\')
    if ($absolute -ieq [IO.Path]::GetPathRoot($absolute).TrimEnd('\') -or $absolute -ieq $env:USERPROFILE -or
        (Test-Path -LiteralPath (Join-Path $absolute '.git')) -or (Test-Path -LiteralPath (Join-Path $absolute 'Invoke-DiskBenchmark.ps1'))) {
        throw 'Refusing benchmark cleanup against a broad/source directory.'
    }
    if (-not [guid]::TryParse($Manifest.owner, [ref]$id) -or $id -eq [guid]::Empty -or
        $Manifest.schema -ne 1 -or $Manifest.name -cne 'clc-disk-benchmark' -or
        [IO.Path]::GetFullPath($Manifest.resourceRoot).TrimEnd('\') -ine [IO.Path]::GetFullPath($Root).TrimEnd('\') -or
        $Manifest.marker -cne "clc-disk-benchmark/v1|$($Manifest.owner)") {
        throw 'Benchmark ownership manifest mismatch; no resource was modified.'
    }
    $null = Assert-BenchPath (Join-Path $Root 'ownership.json') $Root
}

function Get-BenchVM($Manifest, [string]$Root) {
    Assert-BenchManifest $Manifest $Root
    $all = @(Get-VM -ErrorAction Stop)
    $vm = @($all | Where-Object { [string]$_.Id -ceq $Manifest.vmId })
    if (-not $vm.Count) {
        if (@($all | Where-Object Name -CEQ $Manifest.name).Count) { throw 'Benchmark name exists with a different VM ID; refusing modification.' }
        return $null
    }
    if ($vm.Count -ne 1 -or $vm[0].Name -cne $Manifest.name -or $vm[0].Notes -cne $Manifest.marker) {
        throw 'Benchmark VM ID/name/ownership marker mismatch; refusing modification.'
    }
    $null = Assert-BenchPath $vm[0].ConfigurationLocation $Root
    foreach ($drive in @(Get-VMHardDiskDrive -VM $vm[0])) {
        $path = $drive.Path
        while ($path) { $null = Assert-BenchPath $path $Root; $path = (Get-VHD -Path $path).ParentPath }
    }
    foreach ($snapshot in @(Get-VMSnapshot -VM $vm[0])) {
        if ($snapshot.Name -cne "baseline-$($Manifest.owner)") { throw 'Unexpected checkpoint on benchmark VM; refusing removal.' }
    }
    $vm[0]
}

function Grant-BenchWritableDisk([string]$Path, $Manifest, [string]$Root) {
    Assert-BenchManifest $Manifest $Root
    $file = [IO.FileInfo]::new((Assert-BenchPath $Path $Root))
    $acl = [IO.FileSystemAclExtensions]::GetAccessControl($file)
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new("NT VIRTUAL MACHINE\$($Manifest.vmId)", 'FullControl', 'Allow'))
    [IO.FileSystemAclExtensions]::SetAccessControl($file, $acl)
}

function Remove-BenchResources($Manifest, [string]$Root) {
    $vm = Get-BenchVM $Manifest $Root
    # Check every reference before stopping/removing even the benchmark VM.
    foreach ($other in @(Get-VM -ErrorAction Stop)) {
        if ([string]$other.Id -ceq $Manifest.vmId) { continue }
        $references = @(Get-VMHardDiskDrive -VM $other)
        foreach ($snapshot in @(Get-VMSnapshot -VM $other)) { $references += @(Get-VMHardDiskDrive -VMSnapshot $snapshot) }
        foreach ($drive in $references) {
            $path = $drive.Path
            while ($path) {
                if ([IO.Path]::GetFullPath($path).StartsWith([IO.Path]::GetFullPath($Root).TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase)) {
                    throw 'Another VM references benchmark files; preserving all resources.'
                }
                $path = (Get-VHD -Path $path -ErrorAction Stop).ParentPath
            }
        }
    }
    if ($vm) {
        if ($vm.State -ne 'Off') { Stop-VM -VM $vm -TurnOff -Force }
        Get-VMSnapshot -VM $vm | Remove-VMSnapshot -Confirm:$false
        Remove-VM -VM $vm -Force
    }
    # Enumerate and validate exact absolute targets; never use a VM/name wildcard.
    foreach ($entry in @(Get-ChildItem -LiteralPath $Root -Force -Recurse)) { $null = Assert-BenchPath $entry.FullName $Root }
    foreach ($entry in @(Get-ChildItem -LiteralPath $Root -Force)) {
        if ($entry.Name -eq 'ownership.json') { continue }
        $target = Assert-BenchPath $entry.FullName $Root
        Remove-Item -LiteralPath $target -Recurse -Force
    }
    Remove-Item -LiteralPath (Assert-BenchPath (Join-Path $Root 'ownership.json') $Root) -Force
    # The durable, validated manifest above establishes this exact directory's ownership.
    Remove-Item -LiteralPath ([IO.Path]::GetFullPath($Root)) -Force
}
