#requires -Version 7.4
. (Join-Path $PSScriptRoot 'HyperV.ps1')

$script:networkAssertions=0
function Assert-Network($Condition, [string]$Message) {
    $script:networkAssertions++
    if (-not $Condition) { throw $Message }
}
function Assert-NetworkFailure([scriptblock]$Action, [string]$Pattern) {
    $message=''
    try { & $Action | Out-Null } catch { $message=$_.Exception.Message }
    Assert-Network ($message -match $Pattern) "Expected '$Pattern', got '$message'"
}

& {
    $switchID=[guid]::NewGuid()
    $record=@{ ip='192.168.231.10'; mac='00155DA8E70A'; directory='C:\owned\runner' }
    $request=@{ settings=@{ gateway='192.168.231.1' }; records=@($record) }
    $vm=[pscustomobject]@{ Name='owned-runner'; State='Off' }
    $expectedVM=$vm
    $adapter=[pscustomobject]@{ SwitchId=$switchID; MacAddress=$record.mac; DynamicMacAddressEnabled=$false; MacAddressSpoofing='Off'; DhcpGuard='On'; RouterGuard='On' }
    $script:networkACLs=[Collections.Generic.List[object]]::new()
    $script:networkCalls=[Collections.Generic.List[string]]::new()
    $script:networkSessions=[Collections.Generic.HashSet[string]]::new()
    $script:networkAddFailure=0
    $script:networkReadFailure=$false
    $script:networkReadbackMissing=$false
    $script:networkAdapters=@($adapter)

    function Import-Module { param($Name,$ErrorAction) }
    function Assert-CLCRecord { param($Request,$Record) }
    function Get-CLCVM { param($Record) $vm }
    function Assert-CLCNetwork { param($Request) [pscustomobject]@{ Id=$switchID; Name='CLC-Runners' } }
    function Get-VMNetworkAdapter {
        [CmdletBinding()]param($VM)
        Assert-Network ($VM -eq $expectedVM) 'Network query escaped the selected VM'
        $script:networkAdapters
    }
    function Get-VMNetworkAdapterExtendedAcl {
        [CmdletBinding()]param($VMNetworkAdapter)
        if ($script:networkReadFailure) { throw 'ACL read access denied' }
        Assert-Network ($VMNetworkAdapter -eq $adapter) 'ACL query escaped the selected adapter'
        $script:networkCalls.Add('ReadACLs')
        if ($script:networkReadbackMissing -and $script:networkACLs.Count) { return $script:networkACLs | Select-Object -Skip 1 }
        $script:networkACLs
    }
    function Add-VMNetworkAdapterExtendedAcl {
        [CmdletBinding()]param($VMNetworkAdapter,$Direction,$Action,[int]$Weight,$LocalIPAddress='',$RemoteIPAddress='',$LocalPort='',$RemotePort='',$Protocol='', [bool]$Stateful=$false,[int]$IdleSessionTimeout=0)
        Assert-Network ($VMNetworkAdapter -eq $adapter -and $vm.State -eq 'Off') 'ACL write escaped the stopped runner adapter'
        if ($script:networkAddFailure -and $script:networkACLs.Count -eq $script:networkAddFailure) { throw 'ACL installation failed' }
        $script:networkCalls.Add('AddACL')
        # Model provider readback rather than echoing the input dictionaries:
        # wildcard fields can be empty, protocol IDs numeric and host IPs bare.
        $protocolID=switch ($Protocol) { 'TCP' { '6' }; 'UDP' { '17' }; default { $Protocol } }
        $script:networkACLs.Add([pscustomobject]@{
            Direction=$Direction; Action=$Action; Weight=$Weight
            LocalIPAddress=($LocalIPAddress -replace '/32$',''); RemoteIPAddress=($RemoteIPAddress -replace '/32$','')
            LocalPort=$LocalPort; RemotePort=$RemotePort; Protocol=$protocolID
            Stateful=$Stateful; IdleSessionTimeout=$IdleSessionTimeout; IsolationID=0
        })
    }
    function Start-VM { param($VM) $script:networkCalls.Add('StartVM');$VM.State='Running' }

    # Evaluate representative packets against the installed rules independently
    # of the production policy builder. Session state follows Hyper-V's documented
    # five-tuple behavior; native enforcement still needs a permitted Hyper-V host.
    function Test-NetworkPrefix([string]$Address, [string]$Prefix) {
        if (-not $Prefix -or $Prefix -in @('ANY','*')) { return $true }
        $parts=$Prefix.Split('/')
        $left=[Net.IPAddress]::Parse($Address).GetAddressBytes()
        $right=[Net.IPAddress]::Parse($parts[0]).GetAddressBytes()
        if ($left.Length -ne $right.Length) { return $false }
        $bits=if ($parts.Count -eq 2) { [int]$parts[1] } else { $right.Length*8 }
        for ($index=0; $bits -gt 0; $index++) {
            $mask=256-[int][Math]::Pow(2,8-[Math]::Min(8,$bits))
            if (($left[$index] -band $mask) -ne ($right[$index] -band $mask)) { return $false }
            $bits-=8
        }
        $true
    }
    function Get-NetworkDecision([string]$Direction,[string]$Remote,[string]$Protocol='TCP',[int]$LocalPort=45000,[int]$RemotePort=443,[string]$Local=$record.ip) {
        $flow="$Local|$Remote|$Protocol|$LocalPort|$RemotePort"
        if ($script:networkSessions.Contains($flow)) { return 'Allow' }
        $protocolID=switch ($Protocol) { 'TCP' { '6' }; 'UDP' { '17' }; default { $Protocol } }
        foreach ($rule in @($script:networkACLs | Sort-Object Weight -Descending)) {
            if ($rule.Direction -ne $Direction -or -not (Test-NetworkPrefix $Local $rule.LocalIPAddress) -or -not (Test-NetworkPrefix $Remote $rule.RemoteIPAddress)) { continue }
            if ($rule.Protocol -and $rule.Protocol -ne 'ANY' -and $rule.Protocol -ne $protocolID) { continue }
            if ($rule.LocalPort -and $rule.LocalPort -ne 'ANY' -and $rule.LocalPort -ne [string]$LocalPort) { continue }
            if ($rule.RemotePort -and $rule.RemotePort -ne 'ANY' -and $rule.RemotePort -ne [string]$RemotePort) { continue }
            if ($rule.Stateful -and $rule.Action -eq 'Allow') { [void]$script:networkSessions.Add($flow) }
            return [string]$rule.Action
        }
        'Deny'
    }

    Set-CLCNetworkPolicy $request $record $vm
    Assert-Network ($script:networkACLs.Count -gt 0 -and $vm.State -eq 'Off') 'Policy installation booted the VM'
    foreach ($remote in @('0.0.0.1','10.0.0.1','10.255.255.254','100.64.0.1','100.127.255.254','127.0.0.1','169.254.169.254','172.16.0.1','172.31.255.254','192.168.0.1','192.168.231.1','192.168.231.11','192.168.255.254','224.0.0.1','239.255.255.250','240.0.0.1','255.255.255.255')) {
        foreach ($direction in @('Inbound','Outbound')) {
            Assert-Network ((Get-NetworkDecision $direction $remote) -eq 'Deny') "$direction private/local traffic escaped: $remote"
        }
    }
    foreach ($remote in @('1.1.1.1','8.8.8.8','140.82.112.3','172.15.255.254','172.32.0.1','100.63.255.254','100.128.0.1')) {
        foreach ($direction in @('Inbound','Outbound')) {
            Assert-Network ((Get-NetworkDecision $direction $remote) -eq 'Allow') "$direction public IPv4 traffic blocked: $remote"
        }
    }
    foreach ($protocol in @('TCP','UDP')) {
        Assert-Network ((Get-NetworkDecision Outbound '1.1.1.1' $protocol 45000 53) -eq 'Allow') 'Public DNS query blocked'
        Assert-Network ((Get-NetworkDecision Inbound '1.1.1.1' $protocol 45000 53) -eq 'Allow') 'Public DNS reply blocked'
    }
    Assert-Network ((Get-NetworkDecision Outbound '140.82.112.3' TCP 45000 22) -eq 'Allow') 'Public SSH/git traffic blocked'
    Assert-Network ((Get-NetworkDecision Outbound '1.1.1.1' '1' 0 0) -eq 'Allow') 'Public ICMP traffic blocked'
    Assert-Network ((Get-NetworkDecision Outbound '1.1.1.1' TCP 45000 443 '192.168.231.99') -eq 'Deny') 'Spoofed guest source IP bypassed policy'
    foreach ($remote in @('fe80::1','fd00::1','2606:4700:4700::1111','ff02::1')) {
        foreach ($direction in @('Inbound','Outbound')) {
            Assert-Network ((Get-NetworkDecision $direction $remote TCP 45000 443 'fe80::10') -eq 'Deny') "IPv6 bypassed $direction filtering"
        }
    }
    Assert-Network ((Get-NetworkDecision Outbound '192.168.231.1' TCP 22 52000) -eq 'Deny') 'Source-port-22 exception permitted runner-initiated host access'
    Assert-Network ((Get-NetworkDecision Inbound '192.168.231.11' TCP 22 52000) -eq 'Deny') 'Another runner could initiate management SSH'
    Assert-Network ((Get-NetworkDecision Inbound '192.168.231.1' TCP 22 52000) -eq 'Allow') 'Controller could not initiate SSH'
    Assert-Network ((Get-NetworkDecision Outbound '192.168.231.1' TCP 22 52000) -eq 'Allow') 'Established controller SSH reply blocked'
    Assert-Network ((Get-NetworkDecision Outbound '192.168.231.1' TCP 22 52001) -eq 'Deny') 'SSH session allowed an unrelated host connection'
    Assert-Network ((Get-NetworkDecision Outbound '192.168.231.1' TCP 45000 22) -eq 'Deny') 'Runner could initiate host SSH'

    # Readback, not a journal marker, gates Start-VM. Repeated starts of an
    # already running VM do not rewrite ACLs or disrupt established jobs.
    $script:networkCalls.Clear()
    $null=Invoke-CLCHyperV $request Start
    Assert-Network (($script:networkCalls -join ',') -eq 'ReadACLs,StartVM') 'Start bypassed policy readback'
    $null=Invoke-CLCHyperV $request Start
    Assert-Network (($script:networkCalls -join ',') -eq 'ReadACLs,StartVM') 'Running VM was reconfigured or restarted'
    Assert-NetworkFailure { Set-CLCNetworkPolicy $request $record $vm } 'must be installed.*off'
    $vm.State='Off'

    $removed=$script:networkACLs[0];$script:networkACLs.RemoveAt(0)
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'restrictions are missing or changed'
    $script:networkACLs.Add($removed)
    $extra=$removed.PSObject.Copy();$extra.Action='Allow';$extra.Weight=1000;$script:networkACLs.Add($extra)
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'restrictions are missing or changed'
    [void]$script:networkACLs.Remove($extra)
    $duplicate=$removed.PSObject.Copy();$script:networkACLs.Add($duplicate)
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'restrictions are missing or changed'
    [void]$script:networkACLs.Remove($duplicate)
    $ssh=$script:networkACLs | Where-Object { $_.Stateful }
    $ssh.Stateful=$false
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'restrictions are missing or changed'
    $ssh.Stateful=$true
    $ssh.RemoteIPAddress='192.168.231.0/24'
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'restrictions are missing or changed'
    $ssh.RemoteIPAddress='192.168.231.1'
    $ssh.LocalPort=''
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'restrictions are missing or changed'
    $ssh.LocalPort='22'
    $script:networkReadFailure=$true
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'ACL read access denied'
    $script:networkReadFailure=$false
    $adapter.SwitchId=[guid]::NewGuid()
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'adapter, switch, address or guards differ'
    $adapter.SwitchId=$switchID
    $adapter.MacAddress='00155DA8E799'
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'adapter, switch, address or guards differ'
    $adapter.MacAddress=$record.mac
    $adapter.DynamicMacAddressEnabled=$true
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'adapter, switch, address or guards differ'
    $adapter.DynamicMacAddressEnabled=$false
    $script:networkAdapters=@($adapter,$adapter)
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'adapter, switch, address or guards differ'
    $script:networkAdapters=@($adapter)
    foreach ($guard in @('MacAddressSpoofing','DhcpGuard','RouterGuard')) {
        $original=$adapter.$guard;$adapter.$guard=if ($original -eq 'On') { 'Off' } else { 'On' }
        Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'adapter, switch, address or guards differ'
        $adapter.$guard=$original
    }
    Assert-Network (@($script:networkCalls | Where-Object { $_ -eq 'StartVM' }).Count -eq 1 -and $vm.State -eq 'Off') 'Failed policy validation booted a VM'
    Assert-NetworkFailure { Set-CLCNetworkPolicy $request $record $vm } 'already has network ACLs'
    $script:networkACLs.Clear();$script:networkAddFailure=3
    Assert-NetworkFailure { Set-CLCNetworkPolicy $request $record $vm } 'ACL installation failed'
    Assert-NetworkFailure { Invoke-CLCHyperV $request Start } 'restrictions are missing or changed'
    $script:networkACLs.Clear();$script:networkAddFailure=0;$script:networkReadbackMissing=$true
    Assert-NetworkFailure { Set-CLCNetworkPolicy $request $record $vm } 'restrictions are missing or changed'
    Assert-Network ($vm.State -eq 'Off') 'Partial or unreadable ACL installation booted a VM'
    $script:networkReadbackMissing=$false

    # Management endpoints come from configuration and the allocated guest,
    # never from the example subnet or an unrestricted gateway allow rule.
    $script:networkACLs.Clear();$script:networkSessions.Clear()
    $request.settings.gateway='10.88.0.1';$record.ip='10.88.0.10'
    Set-CLCNetworkPolicy $request $record $vm
    Assert-Network ((Get-NetworkDecision Inbound '10.88.0.1' TCP 22 52000) -eq 'Allow') 'Configured controller SSH address ignored'
    Assert-Network ((Get-NetworkDecision Outbound '10.88.0.1' TCP 22 52000) -eq 'Allow') 'Configured controller SSH reply blocked'
    Assert-Network ((Get-NetworkDecision Outbound '10.88.0.1' TCP 45000 443) -eq 'Deny') 'Configured controller received a blanket exception'
    Assert-Network ((Get-NetworkDecision Outbound '8.8.8.8' UDP 45000 53) -eq 'Allow') 'Custom-subnet public DNS blocked'
}
Write-Host "Network policy: $networkAssertions packet, SSH session, adapter scope and fail-closed boot assertions passed (mocked Hyper-V)."
