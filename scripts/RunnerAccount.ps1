#requires -Version 7.4

function ConvertTo-RunnerLocalUserName([string]$UserName) {
    $parts = $UserName.Split('\')
    if ($parts.Count -eq 2 -and $parts[0] -in @('.', $env:COMPUTERNAME)) { $UserName = $parts[1] }
    elseif ($parts.Count -ne 1) { throw 'Specify a local user name, .\user, or COMPUTER\user. Domain accounts are not supported by this script.' }
    if ([string]::IsNullOrWhiteSpace($UserName) -or $UserName.Length -gt 20 -or $UserName -match '["/\\\[\]:;|=,+*?<>@\x00-\x1f]' -or $UserName -match '[.\s]$' -or $UserName -match '^\.+$') {
        throw 'Specify a valid local user name of at most 20 characters.'
    }
    $UserName
}

function Assert-RunnerAccountPath([string]$Path, [switch]$Repository) {
    $full = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    if (-not [IO.Path]::IsPathFullyQualified($Path) -or $full -notmatch '^[A-Za-z]:\\') { throw "A local absolute directory is required: $Path" }
    $broad = @([IO.Path]::GetPathRoot($full).TrimEnd('\'), $env:SystemRoot, $env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:ProgramData, $env:PUBLIC, $env:USERPROFILE)
    $profileRoot = Split-Path $env:USERPROFILE -Parent
    $broad += $profileRoot
    # Never hand over a profile root, including a profile belonging to another user.
    if ($full -in $broad -or (Split-Path $full -Parent) -eq $profileRoot) { throw "Refusing to configure a broad directory: $full" }
    foreach ($systemPath in @($env:SystemRoot, $env:ProgramFiles, ${env:ProgramFiles(x86)})) {
        if ($systemPath -and $full.StartsWith($systemPath.TrimEnd('\')+'\', [StringComparison]::OrdinalIgnoreCase)) { throw "Use a dedicated checkout/storage directory outside Windows and Program Files: $full" }
    }
    if (-not $Repository -and ($script:RunnerRoot -eq $full -or $script:RunnerRoot.StartsWith($full+'\', [StringComparison]::OrdinalIgnoreCase))) {
        throw "A protected runtime directory must not contain the checkout: $full"
    }
    $ancestor = $full
    while ($ancestor) {
        if (Test-Path -LiteralPath $ancestor) {
            $item = Get-Item -LiteralPath $ancestor -Force
            if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "Directory is a file or reparse point: $ancestor" }
        }
        $ancestor = Split-Path $ancestor -Parent
    }
    $full
}

function Get-RunnerAccountPlan([string]$Config, [string]$StorageRoot) {
    $root = Assert-RunnerAccountPath $script:RunnerRoot -Repository
    $gitDirectory = Join-Path $root '.git'
    if ((Test-Path -LiteralPath $gitDirectory) -and -not (Get-Item -LiteralPath $gitDirectory -Force).PSIsContainer) { throw 'Use a standalone clone, rather than a linked worktree, for the runner account.' }
    $configPath = [IO.Path]::GetFullPath($Config, $root)
    $configDirectory = Assert-RunnerAccountPath (Split-Path $configPath -Parent)
    $existing = Test-Path -LiteralPath $configPath
    if ($existing) {
        if ((Get-Item -LiteralPath $configPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Configuration must not be a reparse point.' }
        $settings = Get-Content -LiteralPath $configPath -Raw | ConvertFrom-Json -AsHashtable
    } else {
        $settings = Get-Content -LiteralPath (Join-Path $root 'config/controller.example.json') -Raw | ConvertFrom-Json -AsHashtable
        $settings.hyperv.script = Join-Path $root 'scripts/HyperV.ps1'
        $settings.hyperv.imageRecipe = Join-Path $root 'image/recipe.json'
    }
    if (-not $settings.Contains('hyperv') -or $settings.Contains('wslc')) { throw 'Use a fresh Hyper-V configuration; account preparation does not migrate installations.' }
    if ($settings.controller['startupRunLevel'] -and $settings.controller['startupRunLevel'] -cne 'Limited') { throw 'Non-admin account preparation requires controller.startupRunLevel to be Limited.' }
    if ($StorageRoot) {
        $requested = [IO.Path]::GetFullPath($StorageRoot, $root)
        $configured = [IO.Path]::GetFullPath($(if ($settings['storageRoot']) { $settings.storageRoot } else { '.' }), $configDirectory)
        if ($existing -and $requested -ne $configured) { throw '-StorageRoot cannot relocate an existing configuration. Edit a fresh configuration before preparation.' }
        $settings.storageRoot = $requested
    }
    $storage = Assert-RunnerAccountPath ([IO.Path]::GetFullPath($(if ($settings['storageRoot']) { $settings.storageRoot } else { '.' }), $configDirectory))
    $data = Assert-RunnerAccountPath ([IO.Path]::GetFullPath($settings.controller.dataDirectory, $storage))
    $logs = Assert-RunnerAccountPath ([IO.Path]::GetFullPath($settings.controller.logDirectory, $storage))
    $pat = [IO.Path]::GetFullPath($settings.controller.patFile, $storage)
    $secrets = Assert-RunnerAccountPath (Split-Path $pat -Parent)
    $images = Assert-RunnerAccountPath ([IO.Path]::GetFullPath($settings.hyperv.imageDirectory, $storage))
    $directories = @($configDirectory, $storage, $data, $logs, $secrets, $images) | Sort-Object -Unique
    foreach ($path in $directories) {
        # Reject junctions/symlinks before inherited ACLs or ownership can reach outside these directories.
        if (Test-Path -LiteralPath $path) {
            if (@(Get-ChildItem -LiteralPath $path -Force -Recurse | Where-Object { $_.Attributes -band [IO.FileAttributes]::ReparsePoint }).Count) { throw "Reparse point found beneath $path" }
        }
    }
    $checkoutItems = @(Get-ChildItem -LiteralPath $root -Force -Recurse)
    if (@($checkoutItems | Where-Object { $_.Attributes -band [IO.FileAttributes]::ReparsePoint }).Count) { throw 'The checkout contains reparse points. Use a dedicated checkout without links.' }
    if (@($checkoutItems | Where-Object { -not $_.PSIsContainer -and $_.Name -match '\.(dpapi|vhdx?|avhdx|qcow2|effective\.json)$' }).Count) {
        throw 'The checkout contains saved credentials, VM disks or effective controller configuration. Use a fresh checkout for the runner account.'
    }
    foreach ($path in @($pat, ($configPath+'.effective.json'), (Join-Path $data 'state.json'), (Join-Path $data 'status.json'))) {
        if (Test-Path -LiteralPath $path) { throw "Installation has credentials or controller state ($path). Use a fresh checkout/configuration; account preparation does not transfer an existing installation." }
    }
    foreach ($path in @($data, $images, $secrets)) {
        if (Test-Path -LiteralPath $path) {
            $allowed = if ($path -eq $data) { @('network.json','network.json.lock','host-validation.json','account-preparation.json') } else { @() }
            if (@(Get-ChildItem -LiteralPath $path -Force | Where-Object { $_.Name -notin $allowed -or $_.PSIsContainer }).Count) { throw "Directory contains existing runtime files: $path. Use fresh storage for this account." }
        }
    }
    if (@(Get-RunnerControllerProcesses).Count) { throw 'A controller is using this checkout. Use a fresh checkout for account preparation.' }
    if (Get-ScheduledTask -TaskName 'CLC-Linux-Runners' -ErrorAction SilentlyContinue) { throw 'A CLC-Linux-Runners startup task already exists. Resolve the existing installation before preparing a new account.' }
    $receiptPath = Join-Path $data 'account-preparation.json'
    $receipt = Read-RunnerJson $receiptPath
    if ((Test-Path -LiteralPath $receiptPath) -and -not $receipt) { throw 'Storage has an invalid account-preparation receipt. Use fresh storage.' }
    if ($receipt -and ($receipt.version -ne 1 -or $receipt.config -ne $configPath -or -not $receipt.accountSid)) { throw 'Storage preparation belongs to a different configuration or has an invalid receipt. Use fresh storage.' }
    [pscustomobject]@{ Root=$root; Config=$configPath; Existing=$existing; Settings=$settings; Directories=@($directories); Data=$data; Receipt=$receipt }
}

function Get-RunnerLocalGroupSids([Security.Principal.SecurityIdentifier]$Sid) {
    foreach ($group in Get-LocalGroup) {
        if (@(Get-LocalGroupMember -Group $group -ErrorAction Stop | Where-Object { $_.SID -eq $Sid }).Count) { $group.SID.Value }
    }
}

function Get-RunnerPolicyRights([string]$Sid) { [CLCRunners.AccountPolicy]::GetRights($Sid) }
function Grant-RunnerBatchLogon([string]$Sid) { [CLCRunners.AccountPolicy]::GrantBatchLogon($Sid) }

function Assert-RunnerAccountPolicy([Security.Principal.SecurityIdentifier]$Sid, [string[]]$Groups) {
    if ('S-1-5-32-544' -in $Groups) { throw 'The runner account is a member of Administrators. Choose a non-admin local account.' }
    $subjects = @($Sid.Value, 'S-1-1-0', 'S-1-5-11', 'S-1-5-113', 'S-1-5-32-545', 'S-1-5-32-578') + $Groups
    foreach ($subject in ($subjects + @('S-1-5-3','S-1-5-4') | Sort-Object -Unique)) {
        $rights = @(Get-RunnerPolicyRights $subject)
        if (($subject -ne 'S-1-5-4' -and 'SeDenyBatchLogonRight' -in $rights) -or ($subject -ne 'S-1-5-3' -and 'SeDenyInteractiveLogonRight' -in $rights)) {
            throw "Account or group $subject is denied batch/interactive logon by security policy. Resolve that policy explicitly; this script does not remove deny rights."
        }
    }
}

function Add-RunnerLocalGroupMember([string]$GroupSid, [Security.Principal.SecurityIdentifier]$Sid) {
    $group = Get-LocalGroup -SID $GroupSid
    if (-not @(Get-LocalGroupMember -Group $group -ErrorAction Stop | Where-Object { $_.SID -eq $Sid }).Count) { Add-LocalGroupMember -Group $group -Member $Sid.Value }
}

function Set-RunnerAccountDirectory([string]$Path, [Security.Principal.SecurityIdentifier]$Sid, [switch]$Repository) {
    $null = Assert-RunnerAccountPath $Path -Repository:$Repository
    $null = New-Item -ItemType Directory -Path $Path -Force
    $directory = [IO.DirectoryInfo]::new($Path)
    $acl = [IO.FileSystemAclExtensions]::GetAccessControl($directory, [Security.AccessControl.AccessControlSections]::Access)
    if (-not $Repository) {
        $acl.SetAccessRuleProtection($true, $false)
        foreach ($rule in @($acl.GetAccessRules($true, $false, [Security.Principal.SecurityIdentifier]))) { $null = $acl.RemoveAccessRuleAll($rule) }
    }
    $inherit = [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
    foreach ($identity in @($Sid, [Security.Principal.SecurityIdentifier]::new('S-1-5-18'), [Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'))) {
        $acl.SetAccessRule([Security.AccessControl.FileSystemAccessRule]::new($identity, 'FullControl', $inherit, 'None', 'Allow'))
    }
    [IO.FileSystemAclExtensions]::SetAccessControl($directory, $acl)
}

function Set-RunnerCheckoutOwner([string]$Path, [Security.Principal.SecurityIdentifier]$Sid) {
    # Git checks the worktree and git-directory owners. Other files inherit the account's FullControl ACL.
    foreach ($directory in @($Path, (Join-Path $Path '.git'))) {
        if (Test-Path -LiteralPath $directory) {
            & (Join-Path $env:SystemRoot 'System32/icacls.exe') $directory /setowner ('*'+$Sid.Value) /L /Q | Out-Host
            if ($LASTEXITCODE) { throw "Could not set checkout ownership for $directory (icacls exit $LASTEXITCODE)." }
        }
    }
}
