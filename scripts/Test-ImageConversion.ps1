#requires -Version 7.4
[CmdletBinding()]
param([switch]$Live, [string]$Config = 'local/controller.json')
. (Join-Path $PSScriptRoot 'Common.ps1')
. (Join-Path $PSScriptRoot 'HyperV.ps1')

$script:diskAssertions = 0
function Assert-Disk($Condition, [string]$Message) { $script:diskAssertions++; if (-not $Condition) { throw $Message } }
function Assert-DiskFailure([scriptblock]$Action, [string]$Pattern) {
    $message = ''; try { & $Action | Out-Null } catch { $message = $_.Exception.Message }
    Assert-Disk ($message -match $Pattern) "Expected '$Pattern', got '$message'"
}

$scratchRoot = if ($Live) { (Get-RunnerSettings $Config).Settings.hyperv.imageDirectory } else { [IO.Path]::GetTempPath() }
$version = 'ubuntu2404-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-' + [guid]::NewGuid().ToString('N').Substring(0,12)
$scratch = Join-Path $scratchRoot $version
$created = $false
try {
    $null = New-Item -ItemType Directory -Path $scratch -ErrorAction Stop
    $created = $true
    $source = Join-Path $scratch 'source.qcow2'
    $destination = Join-Path $scratch 'converted.vhdx'
    $owner = [guid]::NewGuid().ToString()
    Write-RunnerJson (Join-Path $scratch 'build-intent.json') @{owner=$owner;version=$version}
    $settings = if ($Live) { (Get-RunnerSettings $Config).Settings.hyperv } else { [pscustomobject]@{qemuImg='mock-qemu';imageDirectory=$scratchRoot;diskGiB=1} }
    # A lightweight real storage test; full guest boot remains PrepareImage's gate.
    $settings.diskGiB = 1
    $request = @{owner=$owner;settings=$settings;records=@();disk=@{source=$source;path=$destination;version=$version}}
    if ($Live) {
        $raw = Join-Path $scratch 'source.raw'
        $stream = [IO.File]::Open($raw,[IO.FileMode]::CreateNew)
        try {
            $stream.SetLength(64MB)
            $pattern = [byte[]]::new(1MB); [Array]::Fill[byte]($pattern,0x5A)
            $stream.Write($pattern); $stream.Position=63MB; $stream.Write($pattern); $stream.Flush($true)
        } finally { $stream.Dispose() }
        $null = Invoke-CLCQemuImg $settings.qemuImg @('convert','-f','raw','-O','qcow2',$raw,$source)
        $result = Invoke-CLCHyperV $request ConvertImage
        Assert-Disk ($result.validated -and $result.sourceSize -eq 64MB -and $result.size -eq 1GB) 'Live logical sizes differ'
        $null = Invoke-CLCQemuImg $settings.qemuImg @('compare','-f','qcow2','-F','vhdx',$source,$destination)
        Write-Host 'Live VHDX test passed: New-VHD, convert -n, identical guest sectors, allowed NTFS attributes, Get-VHD/Test-VHD and Resize-VHD to 1 GiB.'
    } else {
        Set-Content -LiteralPath $source -Value 'synthetic QCOW2'
        & {
            $script:steps = [Collections.Generic.List[string]]::new()
            $script:failure = ''; $script:vhdSize = 64MB; $script:sourceVirtualSize=64MB; $script:badAttributes = [IO.FileAttributes]::Normal
            function Get-Item {
                [CmdletBinding()]param($LiteralPath,[switch]$Force)
                if (($LiteralPath -eq $destination -and $script:steps.Contains('convert')) -or
                    ($LiteralPath -eq $scratch -and $script:failure -eq 'directory')) {
                    return [pscustomobject]@{Attributes=$script:badAttributes}
                }
                Microsoft.PowerShell.Management\Get-Item -LiteralPath $LiteralPath -Force:$Force
            }
            function New-VHD {
                [CmdletBinding()]param($Path,[switch]$Dynamic,$SizeBytes,$BlockSizeBytes,$LogicalSectorSizeBytes,$PhysicalSectorSizeBytes)
                Assert-Disk ($Path -eq $destination -and $Dynamic -and $SizeBytes -eq $script:sourceVirtualSize -and $BlockSizeBytes -eq 1MB -and $LogicalSectorSizeBytes -eq 512 -and $PhysicalSectorSizeBytes -eq 4096) 'New-VHD must create a dynamic, source-sized 512-sector VHDX'
                $script:steps.Add('New-VHD'); Set-Content -LiteralPath $Path -Value 'Hyper-V VHDX'; $script:vhdSize=$script:sourceVirtualSize
            }
            function Get-VHD {
                [CmdletBinding()]param($Path)
                $script:steps.Add('Get-VHD')
                [pscustomobject]@{VhdFormat='VHDX';VhdType='Dynamic';Size=$script:vhdSize;LogicalSectorSize=512}
            }
            function Test-VHD { [CmdletBinding()]param($Path) $script:steps.Add('Test-VHD'); -not ($script:failure -eq 'invalid' -and $script:steps.Contains('convert')) }
            function Resize-VHD {
                [CmdletBinding()]param($Path,$SizeBytes)
                $script:steps.Add('Resize-VHD')
                if ($script:failure -eq 'resize') { throw 'Resize-VHD failed' }
                Assert-Disk ($SizeBytes -eq [long]$settings.diskGiB*1GB) 'Configured logical size lost'
                if ($script:failure -ne 'wrong-resize') { $script:vhdSize=$SizeBytes }
            }
            function Invoke-CLCQemuImg([string]$Program,[string[]]$Arguments) {
                $action=$Arguments[0]; $script:steps.Add($action)
                if ($script:failure -eq $action) { throw "qemu-img $action failed" }
                switch ($action) {
                    'info' { @{format='qcow2';'virtual-size'=$(if ($script:failure -eq 'oversize') { [long]$settings.diskGiB*1GB+512 } else { $script:sourceVirtualSize })} | ConvertTo-Json -Compress }
                    'convert' {
                        Assert-Disk ($Arguments.Count -eq 8 -and ($Arguments[0..5] -join '|') -ceq 'convert|-n|-f|qcow2|-O|vhdx' -and
                            $Arguments[6] -ieq $source -and $Arguments[7] -ieq $destination) 'QEMU must use -n without creation options and the normalized owned paths'
                        if ($script:failure -eq 'wrong-size') { $script:vhdSize+=512 }
                    }
                    'compare' { Assert-Disk ($Arguments.Count -eq 7 -and ($Arguments[0..4] -join '|') -ceq 'compare|-f|qcow2|-F|vhdx' -and
                        $Arguments[5] -ieq $source -and $Arguments[6] -ieq $destination) 'Guest sectors must be compared before resizing using the normalized owned paths' }
                    default { throw 'Unexpected QEMU operation' }
                }
            }
            $result = Convert-CLCImage $request
            Assert-Disk ($result.validated -and $result.size -eq 1GB -and $result.sourceSize -eq 64MB) 'Valid conversion did not return exact sizes'
            Assert-Disk (($script:steps -join ',') -ceq 'info,New-VHD,Get-VHD,Test-VHD,convert,Get-VHD,Test-VHD,compare,Resize-VHD,Get-VHD,Test-VHD') 'Conversion/validation/resize ordering changed'
            Remove-Item -LiteralPath $destination -Force
            $script:steps.Clear()
            $request.disk.path = (Join-Path $scratch '.\converted.vhdx').ToUpperInvariant().Replace('\','/')
            $request.disk.source = $source.ToUpperInvariant().Replace('\','/')
            Assert-Disk (Convert-CLCImage $request).validated 'Equivalent Windows case, separators and dot segments were rejected'
            $request.disk.path = $destination; $request.disk.source = $source
            Remove-Item -LiteralPath $destination -Force;$script:steps.Clear()
            $settings.diskGiB=20;$script:sourceVirtualSize=3584MB
            $result=Convert-CLCImage $request
            Assert-Disk ($result.validated -and $result.sourceSize -eq 3584MB -and $result.size -eq 20GB) 'Ubuntu-sized source did not grow to the new 20 GiB default'
            $settings.diskGiB=1;$script:sourceVirtualSize=64MB
            foreach ($failure in @('convert','compare','resize','wrong-size','wrong-resize','invalid','oversize','SparseFile','Compressed','Encrypted','directory')) {
                Remove-Item -LiteralPath $destination -Force -ErrorAction SilentlyContinue
                $script:steps.Clear(); $script:failure=$failure; $script:badAttributes=[IO.FileAttributes]::Normal
                $pattern = switch ($failure) {
                    { $_ -in @('SparseFile','Compressed','Encrypted') } { $script:badAttributes=[IO.FileAttributes]::$failure; 'forbidden NTFS attributes '+$failure }
                    'directory' { $script:badAttributes=[IO.FileAttributes]::Compressed; 'forbidden NTFS attributes Compressed' }
                    { $_ -in @('wrong-size','wrong-resize') } { 'Unexpected VHDX' }
                    'invalid' { 'Test-VHD rejected' }
                    'oversize' { 'exceeds configured disk size' }
                    default { $failure+'.*failed' }
                }
                Assert-DiskFailure { Convert-CLCImage $request } $pattern
                if ($failure -notin @('resize','wrong-resize')) { Assert-Disk (-not $script:steps.Contains('Resize-VHD')) 'Failed candidate reached resize' }
            }
            $script:failure=''; $script:badAttributes=[IO.FileAttributes]::Normal; $script:steps.Clear()
            Set-Content -LiteralPath $destination -Value 'preserve incomplete candidate'
            Assert-DiskFailure { Convert-CLCImage $request } 'target already exists'
            Assert-Disk ((Get-Content -LiteralPath $destination -Raw).Trim() -ceq 'preserve incomplete candidate') 'Retry overwrote an existing file'
            Assert-Disk ($script:steps.Count -eq 0) 'Retry touched an incomplete candidate'
            Remove-Item -LiteralPath $destination -Force
            $intentPath = Join-Path $scratch 'build-intent.json'
            foreach ($invalid in @('foreign-owner','wrong-version','missing','malformed','missing-owner','missing-version','empty-owner')) {
                $intent = @{owner=$owner;version=$version}
                switch ($invalid) {
                    'foreign-owner' { $intent.owner=[guid]::NewGuid().ToString() }
                    'wrong-version' { $intent.version=$version.Replace('ubuntu2404','ubuntu2204') }
                    'missing-owner' { $intent.Remove('owner') }
                    'missing-version' { $intent.Remove('version') }
                    'empty-owner' { $intent.owner='' }
                }
                Write-RunnerJson $intentPath $intent
                if ($invalid -eq 'missing') { Remove-Item -LiteralPath $intentPath -Force }
                if ($invalid -eq 'malformed') { Set-Content -LiteralPath $intentPath -Value '{invalid JSON' }
                Assert-DiskFailure { Convert-CLCImage $request } 'matching owned build intent'
                Assert-Disk ($script:steps.Count -eq 0) "Invalid intent ($invalid) reached disk creation"
            }
            Write-RunnerJson $intentPath @{owner=$owner;version=$version}
            $otherVersion = $version.Substring(0,$version.Length-12) + 'abcdef123456'
            foreach ($invalid in @('outside-root','parent-traversal','other-build','nested-build','wrong-file','alternate-stream','missing-version','mismatched-version','missing-owner')) {
                $request.disk.path=$destination; $request.disk.version=$version; $request.owner=$owner
                switch ($invalid) {
                    'outside-root' { $request.disk.path=Join-Path (Split-Path $scratchRoot -Parent) 'converted.vhdx' }
                    'parent-traversal' { $request.disk.path=Join-Path $scratch '..\converted.vhdx' }
                    'other-build' { $request.disk.path=Join-Path (Join-Path $scratchRoot $otherVersion) 'converted.vhdx' }
                    'nested-build' { $request.disk.path=Join-Path (Join-Path (Join-Path $scratchRoot 'nested') $version) 'converted.vhdx' }
                    'wrong-file' { $request.disk.path=Join-Path $scratch 'base.vhdx' }
                    'alternate-stream' { $request.disk.path=$destination+':unrelated' }
                    'missing-version' { $request.disk.Remove('version') }
                    'mismatched-version' { $request.disk.version=$otherVersion }
                    'missing-owner' { $request.owner='' }
                }
                Assert-DiskFailure { Convert-CLCImage $request } 'matching owned build intent|escapes resource root'
                Assert-Disk ($script:steps.Count -eq 0) "Invalid request ($invalid) reached disk creation"
            }
            $request.disk.path=$destination; $request.disk.version=$version; $request.owner=$owner
            $request.disk.source=Join-Path (Split-Path $scratchRoot -Parent) 'foreign.qcow2'
            Assert-DiskFailure { Convert-CLCImage $request } 'escapes resource root'
            Assert-Disk ($script:steps.Count -eq 0) 'Unrelated source reached disk creation'
            $request.disk.source=$source
        }
        Write-Host "Image conversion: $diskAssertions assertions passed (New-VHD/-n, sector comparison, NTFS attributes, sizes, resize, failed/retried/foreign candidates)."
    }
} finally {
    # Remove only this test's unique, checked directory; no VM was created.
    if ($created -and (Test-Path -LiteralPath $scratch)) {
        $absolute = Assert-CLCPath $scratch $scratchRoot
        if ((Split-Path $absolute -Leaf) -cne $version) { throw 'Test cleanup path differs' }
        foreach ($entry in @(Get-ChildItem -LiteralPath $absolute -Force -Recurse)) { $null=Assert-CLCPath $entry.FullName $absolute }
        Remove-Item -LiteralPath $absolute -Recurse -Force
    }
}
