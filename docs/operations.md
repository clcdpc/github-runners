# Operations

## Prepare a dedicated non-admin account

For a **fresh installation**, open PowerShell 7.4+ as an administrator in the
standalone checkout that the runner account will use. A dedicated location such
as `C:\Apps\github-runners` makes this checkout easy to access from both accounts.
Run:

```powershell
.\scripts\Prepare-RunnerAccount.ps1 -UserName github-runners -CreateLocalUser
```

The script prompts securely for the new local user's Windows password. Omit
`-CreateLocalUser` to prepare an existing enabled, non-admin local user; its password
is preserved. `user`, `.\user` and `COMPUTER\user` name local accounts. Domain and
Microsoft accounts are outside this script's scope.

To choose dedicated storage or preview the operation:

```powershell
.\scripts\Prepare-RunnerAccount.ps1 -UserName github-runners -CreateLocalUser -StorageRoot D:\CLC-Runners -WhatIf
.\scripts\Prepare-RunnerAccount.ps1 -UserName github-runners -CreateLocalUser -StorageRoot D:\CLC-Runners
```

`-WhatIf` performs read-only checks and prints the intended paths without creating
an account, prompting for a password, or changing host configuration. `-Config`
selects a configuration; the default is `local/controller.json`. If it is missing,
the script creates it from the example. For custom GitHub/network settings, create
and edit a fresh configuration before running the script. An existing configuration
is preserved, and `-StorageRoot` cannot relocate it.

Preparation adds the selected user to Users and Hyper-V Administrators, grants
**Log on as a batch job**, and prepares Hyper-V, OpenSSH, the owned switch/NAT and
QEMU through the normal host preparation routines. Checkout access includes the
write and ACL permissions needed to install tools, build the controller and manage
VM disk access. The checkout and `.git` directory owners become the runner user
so Git recognizes the checkout after sign-in. Runtime/configuration directories
initially allow only that user, SYSTEM and local Administrators; ordinary activation
then applies the existing user/SYSTEM protection to data, images, logs and secrets.

The script rejects administrator accounts, logon-deny policy conflicts, broad
directories, linked worktrees/reparse points, and installations with saved PATs,
controller state, runtime files or an existing controller startup task. It prepares
a fresh installation. Repeating preparation for the same account/configuration
before activation is supported, including after a requested manual restart. Deny
policies are preserved; domain policy can overwrite local rights and must be
resolved by its administrator.

Follow any manual restart instructions and rerun the **same account-preparation
command** as administrator. Then sign out and sign in as the runner user to create
its profile and load the new group membership. In normal PowerShell 7:

```powershell
Set-Location C:\Apps\github-runners
.\runners.ps1 Activate -StartupMode Password
```

Use the same `-Config` if one was supplied. Activation saves the PAT with the runner
user's CurrentUser DPAPI, builds/validates the image, and validates/registers startup
using that user's Windows password. Local Users membership permits ordinary console
sign-in under the usual Windows policy; remote desktop access is configured separately.
The [startup probe](#unattended-boot-startup) verifies the target user's actual
noninteractive Hyper-V and DPAPI access. Machine-specific ACL or managed-policy
restrictions may still need their exact reported errors resolved.

### Recover preparation started in another checkout

The switch and NAT belong to the Windows host, while their ownership record is
stored in `network.json` under the configured `controller.dataDirectory`. Running
preparation from a different checkout with fresh storage therefore reports that
the configured switch/NAT already exists without its ownership record. The
checkout is determined by the script's location, so invoke the script in the
intended checkout after changing directories.

If preparation was started in the wrong checkout on the **same host**, and neither
installation has been activated, copy only the original `network.json` into the
intended data directory and rerun preparation. With default storage paths, replace
the old checkout path below and run in elevated PowerShell 7:

```powershell
Set-Location C:\Apps\github-runners
$oldNetwork = 'C:\OldCheckout\github-runners\local\data\network.json'
$newData = Join-Path $PWD.Path 'local\data'
New-Item -ItemType Directory -Path $newData -Force | Out-Null
[IO.File]::Copy($oldNetwork, (Join-Path $newData 'network.json'), $false)
.\scripts\Prepare-RunnerAccount.ps1 -UserName github-runners
```

For custom storage/configuration, use the actual source and target data directories
and supply the same `-Config` and `-StorageRoot` options as the intended preparation
command. The copy fails if the destination record already exists; inspect that
record before proceeding. Preparation validates the saved switch GUID, ownership
notes, names, subnet and gateway before reusing the network, and writes a new
account-preparation receipt for the intended configuration. Keep using the intended
checkout after recovery; running two controllers on this network can allocate
duplicate guest addresses.

To reuse the original network when its record is missing, recover the record from
a backup. To discard unused preparation and start again, use the reset below. If
the original installation was already activated, follow the existing installation's
recovery process; account preparation supports fresh installations only.

### Reset unused preparation

After running account preparation in the wrong checkout, you can explicitly reset
its unused network and preparation records. From the intended checkout, in
elevated PowerShell 7 on that Windows host:

```powershell
.\scripts\Reset-RunnerPreparation.ps1 -WhatIf
.\scripts\Reset-RunnerPreparation.ps1
.\scripts\Prepare-RunnerAccount.ps1 -UserName github-runners
```

The preview lists the exact switch, NAT and files. The actual reset asks for
confirmation, removes the marked runner internal switch and matching NAT, then
removes `network.json`, `host-validation.json` and `account-preparation.json` in
the selected data directory. A failed network removal retains its records for
retry. The Windows account, group membership/batch-logon rights, configuration,
checkout, installed tools and Hyper-V feature remain ready for reuse. Empty
directories and lock files can also be reused.

If the original checkout still exists, select its configuration to also clear
the preparation records there, then run preparation from the intended checkout:

```powershell
.\scripts\Reset-RunnerPreparation.ps1 -Config C:\OldCheckout\github-runners\local\controller.json -WhatIf
.\scripts\Reset-RunnerPreparation.ps1 -Config C:\OldCheckout\github-runners\local\controller.json
```

The script accepts the same `-Config`/`-StorageRoot` selection as preparation and
uses the example settings if the selected configuration is missing. When the old
checkout/record is unavailable, the switch must still carry its valid
`clc-runners-network/v1|<owner-guid>` marker, and the NAT name/prefix must match the
selected settings. A NAT-only remainder requires the saved ownership record.

Reset refuses unmarked/external switches, mismatched records, NATs with unexpected
static mappings, attached VMs (even stopped ones), running controllers, existing
startup tasks and selected storage containing activation state, credentials or
images. Use the [normal stop and cleanup process](#stop-cleanup-and-recovery) for
an activated installation. `-WhatIf` is read-only and creates no lock files or
directories.

## Prepare this Windows VM

Use PowerShell 7.4+ under the same Windows account that owns the saved DPAPI PAT.
Supported targets are x64 Windows Pro, Enterprise, Education and Server with the
Hyper-V feature. The outer Hyper-V host must expose nested virtualization; this
tool does not modify it. Actual guest boot, rather than WMI CPU flags, is the
required nesting test.

In elevated PowerShell 7:

```powershell
Set-Location C:\Apps\github-runners
.\runners.ps1 PrepareHost
```

This enables Hyper-V when missing, adds the current account to Hyper-V
Administrators, installs OpenSSH Client and verified QEMU tooling if missing,
and prepares the dedicated internal switch/NAT. If Windows requests a restart, the command stops and tells
you to restart manually, then rerun it. It never restarts Windows automatically.
The equivalent client-Windows feature operation is:

```powershell
Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All -All -NoRestart
```

Refresh newly granted group membership by signing out/in. Run ordinary controller
commands under the normal token. On machines with restrictive Hyper-V/file ACL
policy, retain the precise error and verify `TestHyperV` under that token before
assuming group membership is sufficient. Creating the NAT requires elevation;
ordinary VM provisioning never creates or reconfigures host networks.

The default subnet is `192.168.231.0/24`, gateway `.1`, allocation `.10`–`.250`,
DNS `1.1.1.1` and `8.8.8.8`. Use reachable public resolvers; the runner network
policy blocks private DNS servers. Before network creation, choose unused
`switchName`, `natName`, subnet, gateway and address range in the JSON. The pool
needs maximum runners plus one build/validation address. No DHCP is installed
or assumed.

An existing incompatible WinNAT, switch name, WSL/HNS route or VPN subnet causes
failure. Inspect without deleting anything:

```powershell
Get-VMSwitch
Get-NetNat
Get-NetRoute -AddressFamily IPv4
Get-NetIPAddress -AddressFamily IPv4
```

Do not apply generic Internet advice to remove all NATs or switches. Preparation
never deletes one. Once owned networking exists, restore its saved configuration
before recovery; automatic subnet replacement is deliberately unsupported.

### Runner network restrictions

Every runner, image-build and image-validation VM receives Hyper-V extended port
ACLs while it is off. Creation reads the installed rules back, and every start of
a stopped VM checks the complete policy, its single adapter, owned switch, static
MAC and DHCP/router guards before calling `Start-VM`. A failed ACL command,
unreadable policy, missing rule, extra rule or changed adapter prevents boot.
Already running VMs keep their sessions; drain and recreate older VMs to apply
this policy. A stopped VM without the current policy must be recreated rather
than started without restrictions. No image rebuild is needed for this host-side
policy change.

The adapter permits public IPv4 traffic using only the VM's allocated source
address. Higher-priority rules block private/local destinations and sources:

| Address ranges | Purpose |
| --- | --- |
| `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` | Private networks, including the host gateway and other runners |
| `100.64.0.0/10`, `169.254.0.0/16` | Shared address space and link-local services |
| `0.0.0.0/8`, `127.0.0.0/8` | This-network and loopback addresses on the VM adapter |
| `224.0.0.0/4`, `240.0.0.0/4` | Multicast, reserved addresses and limited broadcast |
| `::/0` | All IPv6 through the adapter |

The only private-network exception permits the controller at the configured
gateway to initiate TCP connections to the guest's port 22. This rule is
**stateful**: replies to that exact SSH session are permitted, while new
runner-initiated connections to the host remain blocked, including attempts
using source port 22. There is no general gateway or runner-subnet exemption.
Hyper-V's [stateful ACL documentation](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/create-security-policies-extended-port-access-control-lists#stateful-acl-rules)
describes the reverse-flow behavior.

The destination filters preserve NAT internet forwarding through a private
gateway. Public DNS, including the configured `1.1.1.1` and `8.8.8.8`, remains
available; private DNS resolvers are blocked. These ACLs apply only to the owned
VM adapter, so guest-local loopback and Docker bridge networks remain available.
Internal services addressed with public IPs require separate restrictions.

`scripts/Test-NetworkPolicy.ps1` tests representative packet decisions, SSH
sessions, adapter scope and boot failures using mocked Hyper-V commands; it also
runs in `scripts/Test-Scripts.ps1`. On a prepared host with Hyper-V management
permission, `runners.ps1 TestHyperV` exercises a real restricted validation clone,
including controller SSH, public DNS/HTTPS and guest-local Docker networking.
The same restrictions apply during `PrepareImage` provisioning and clone checks.

## Capacity and storage

Assigned vCPUs may exceed the host's logical processor count. CPU counts remain
in admission diagnostics; they do not reserve physical cores or reject a VM.
Hyper-V enforces its supported per-VM processor configuration. Free RAM must
cover the configured fixed guest RAM plus `reserveMemoryGiB`: the default
allocation is 10 GiB plus an explicit 8 GiB Windows reserve, so one new VM needs
at least 18 GiB available.
Already running guest RAM is reflected in available memory and is not charged
again. Where accessible, the Hyper-V Dynamic Memory Balancer available-memory
counter must also cover the guest allocation; Hyper-V's own reserve is already
excluded from that counter, so another Windows reserve is not subtracted from
it. Counter errors are reported and the Windows check/native Hyper-V start
fence remain in force. See Microsoft's [available-memory guidance](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/dynamic-memory).

Admission runs before JIT registration and again before VM files are allocated.
Each decision logs the runner limit/count/slots, requested RAM, measured available
Windows RAM, reserve, CPUs, Hyper-V RAM and disk budget. A shortage defers demand
without reconnecting the listener or registering another runner. Retries start
at `max(30, controller.retrySeconds)` seconds, double after repeated failures,
and cap at five minutes. Cleanup of an established runner permits an immediate
recheck. Unexpected provisioning failures retain ownership intent and back off
while normal reconciliation removes partial resources and registrations.

Disk checks use `hyperv.diskGrowthGiB`, a
**20 GiB** physical growth budget per VM, plus `reserveDiskGiB`
(**15 GiB** for Windows). The default **20 GiB** `diskGiB` sets the guest's
logical disk size. Existing disposable disks reserve `max(0, diskGrowthGiB - current file size)`
in addition to one new growth budget and the Windows reserve. Incomplete
creations without a disk retain their full growth budget; removed VMs release it.

The default check therefore needs **35 GiB free at VM creation** with no existing
VMs. Host validation checks both data and image volumes against the same budget.
Existing configurations that omit `diskGrowthGiB` also default to 20 GiB without
rewriting the file. Adjust this setting for the expected job size; it does not
change the guest disk size or require an image rebuild. The default budgets the
full logical disk. A custom growth budget below `diskGiB` can be exceeded by a
larger job. Actual disk usage already reduces the volume's reported free space.

Three default runner disks have 60 GiB of combined logical capacity. Base images
and image-build workspace need additional space. Earlier explicit 100 GiB disks
and 25 GiB growth budgets remain unchanged until edited. To adopt the defaults, set
both `diskGiB` and `diskGrowthGiB` to `20` for the smaller allocation. Changing
`diskGiB` requires a new base image; activation builds it rather than shrinking
an existing VHDX. The minimum supported configured disk is 20 GiB.

Allow additional space for downloads, conversion, populated base images and
retained versions during image builds. Existing VM disks are not automatically
resized or moved, and image pruning is explicit.

Set top-level `storageRoot` to a dedicated local NTFS folder before preparing a
new installation. For example, in the controller configuration:

```json
"storageRoot": "D:\\CLC-Runners"
```

Relative `controller.dataDirectory`, `controller.logDirectory`,
`controller.patFile` and `hyperv.imageDirectory` resolve beneath that root. The
example therefore uses `D:\CLC-Runners\data` for VM/state files, `images` for
base images and source downloads, `logs` for logs, and `secrets` for the PAT.
The default `"."`, an empty value or an omitted field keeps storage beside the
configuration file. Absolute per-path overrides remain authoritative.
`hyperv.script` and `hyperv.imageRecipe` remain relative to the configuration
file, and installed tools/code remain in their existing locations.

Changing the root does not relocate an initialized installation. Drain and clean
up owned VMs before changing storage locations; the old files remain in place.
Preserve `state.json` and ownership if moving an initialized data directory.
Never move/delete a parent VHDX with children. Exact path identities in existing
manifests mean live resource relocation requires draining/cleanup first. Do not
use a shared image directory for another deployment owner.

Keep runtime configuration, PATs, keys, logs, diagnostics and VM/build artifacts
under ignored `local/` storage or outside the checkout. Common runtime filenames
and artifact formats are also ignored. If custom paths inside the checkout use
other names, add those paths to `.git/info/exclude` before running setup. Review
`git status --short` before committing; ignore rules do not protect tracked files
or files added with `git add -f`. Diagnostic redaction does not make operational
archives suitable for publication.

## Activate

```powershell
.\runners.ps1 Activate
.\runners.ps1 Activate -NoStartup
.\runners.ps1 Status
```

`Activate` reuses compatible configuration, a valid saved PAT, the permanent
scale set, unchanged builds/images and validated startup. Activation logs go to
`activation-*.log` under `controller.logDirectory`. Repetition is safe; a process
alone is not a healthy listener. With a restart-capable live controller, ordinary
activation tests/vets and stages a controller candidate while jobs continue.
Status reports its automatic
application or validation blocker. Explicit image rebuilds and initial upgrades
from older controllers still use the stopped installation path and its 30-second
graceful-stop wait. They never force-kill a job to replace a binary.

## Unattended boot startup

Use `Password` startup for a controller that runs independently of an interactive
Windows session. It uses Windows Task Scheduler under the existing controller
account, starts at boot, survives sign-out, restarts after process failures and
keeps the same CurrentUser DPAPI PAT and Hyper-V/file permissions. The runtime
does not require a Windows service wrapper or a different account.

In PowerShell 7 under the controller account, during an idle window:

```powershell
.\runners.ps1 EnableStartup -StartupMode Password
```

The local credential prompt requires the account's **Windows password**. Windows
Hello PINs and GitHub tokens cannot authenticate a batch logon. For explicit
credential input or a password refresh, keep the credential in memory:

```powershell
$startupCredential = Get-Credential -UserName ([Security.Principal.WindowsIdentity]::GetCurrent().Name)
.\runners.ps1 EnableStartup -StartupMode Password -StartupCredential $startupCredential
Remove-Variable startupCredential
```

`Activate -StartupMode Password` also supports `-StartupCredential`. An explicit
startup-mode or credential change is applied after safe draining rather than
being treated as a request to stage only a controller update. If work is still
draining after the normal stop deadline, wait and rerun the command. Active jobs
are not terminated. Use the same `-Config` as the controller.

The account needs **Log on as a batch job**, access to its profile/PAT and runner
storage, and Hyper-V management permission. Task registration may require an
elevated PowerShell under that same account or a local-policy adjustment. Setup
does not create accounts or change logon rights during activation; the separate
admin-run [account preparation](#prepare-a-dedicated-non-admin-account) handles those.
Windows Task Scheduler stores
the password; our task definitions, arguments, reports and repository files do
not contain it. Re-register with `-StartupCredential` after changing the account
password. The credential must resolve to the current controller SID.

Before changing production startup, a temporary Password task must decrypt both
a DPAPI sentinel and the **actual saved PAT**, boot/SSH-test/remove a restricted
Ubuntu clone, exit successfully, and report the controller SID in noninteractive
**session 0** at the configured elevation level. A failure preserves the existing
startup definition. Successful registration installs boot and sign-in triggers,
duplicate-instance suppression, unlimited execution time and one-minute crash
recovery. The command stages the
restart helper and starts the controller via the registered task immediately;
`Start` also uses that task and checks listener health. Password-mode startup
failures do not fall back to a user-session process.

Validation binds machine identity, account SID, executable, saved PAT, image,
Hyper-V configuration, startup run level and probe/runtime sources. It remains
valid across normal reboots and interactive session changes. Changed inputs
require a fresh `TestUnattended -StartupMode Password` or `EnableStartup -StartupMode Password`
with the account credential. `Auto` preserves an existing Password task.

Verify actual sign-out and cold-boot operation separately in a maintenance
window. This change does not initiate either. Inspect the task and controller:

```powershell
Get-ScheduledTask -TaskName CLC-Linux-Runners | Select-Object State, Principal
Get-ScheduledTaskInfo -TaskName CLC-Linux-Runners
.\runners.ps1 Status
.\runners.ps1 Jobs
```

### Temporary diagnostic elevation

For an initial test with the controller account temporarily in local Administrators,
set `controller.startupRunLevel` to `"Highest"` in the target configuration. From
an elevated PowerShell 7 under that same account, run:

```powershell
.\runners.ps1 EnableStartup -StartupMode Password
```

The probe and permanent task use that run level, and the Password probe verifies
the worker's actual administrator token. Merely adding the account to Administrators
does not elevate a `Limited` task; see Microsoft's
[Task Scheduler security contexts](https://learn.microsoft.com/en-us/windows/win32/taskschd/security-contexts-for-running-tasks).
The default remains `"Limited"`. This option grants no group membership or logon rights.
An elevated controller and the scripts it executes have full host privileges,
including access to other VMs. Networking, clone and cleanup checks still run.

After diagnosing other issues, set the value back to `"Limited"`, remove the
temporary Administrators membership and sign out/in to obtain a fresh token.
Retain the required Hyper-V, batch-logon and storage permissions, then rerun
`EnableStartup -StartupMode Password`. It revalidates and replaces the elevated
task, gracefully restarting the controller under the configured run level.
Also verify sign-out and cold-boot operation on the target host.

## Automatic graceful restart

The controller polls a fixed allowlist every five seconds and requires a stable
identity for at least five seconds. SHA256 values are cached against size/mtime,
with a full recheck within one minute. It watches the desired JSON configuration,
encrypted PAT file, installed/staged executable and validation receipt, prepared
PowerShell runtime/startup scripts, resolved PowerShell/SSH executables, image
promotion/provenance and recipe. The large VHDX is checksummed during validation,
not each poll. Logs, state/status updates, unrelated files and Go source edits
do not request restarts. Equal effective installations remain equal even when a
receipt is reformatted or an identical candidate moves to a staging directory.

The process retains its loaded configuration, image and pinned Hyper-V bridge
while draining. Ordinary runner count, reconcile/backoff and logging settings
can be validated automatically. Changes to deployment ownership, data directory
or PAT **path** require explicit stopped activation. PAT content rotation is
validated against GitHub before handoff. Configuration edits never automatically
build an Ubuntu image. An incompatible image/recipe reports explicit preparation
as the blocker.

`Activate` is the explicit preparation step for changed Go source or PowerShell
runtime code. A live activation builds into `bin/staged/<input-fingerprint>/`,
runs Go regression tests and vet first, and publishes a checksummed script bundle
through `bin/restart-runtime.json`. It leaves the installed executable running.
Changing source by itself never compiles or runs that source automatically.
Directly replacing executables or editing a prepared bundle is unsupported;
use Activate and preserve damaged files for diagnosis.

The durable states are None, Pending, Draining, ReadyToRestart, Restarting,
Completed and Failed. Automatic requests validate before draining. The explicit
manual request fences acceptance immediately, even if subsequent validation
finds a blocker:

```powershell
.\runners.ps1 RestartWhenIdle
.\runners.ps1 Status
.\runners.ps1 Jobs
.\runners.ps1 Jobs -Json
```

RestartWhenIdle returns promptly and carries the same protected-directory,
per-process nonce authorization as Stop. Repeated pending requests coalesce.
It works without a configuration change. It never terminates a workflow.
Stop cancels a pending restart and requests graceful shutdown without relaunch.

Draining shares the acquisition/provisioning mutex and advertises zero scale-set
capacity on subsequent polls. An older in-flight poll can deliver offers, but
the fenced callback does not acquire them; GitHub keeps them queued. The listener
continues receiving starts/completions. Already accepted assignments retain VM
provisioning and normal lifecycle/cleanup, including accepted jobs that have no
runner yet. Capacity remains configuration-driven (defaults: maximum three,
two CPUs, 10 GiB static RAM, 20 GiB logical disk); minimum zero is preserved.

Acquisition intent is journaled before the API call so a lost reply/crash cannot
erase ownership. Exact completion events release those intents. Missing
cancellation/completion events require repeated fresh remote-zero observations
over at least `max(15 seconds, 2 * reconcileIntervalSeconds)` before reconciliation.
An empty runner count alone never permits restart: all ownership, JIT/VM work,
deregistration and cleanup must finish, fresh GitHub statistics must be zero,
and a successful owned-VM inventory must be empty. The official listener's final
callback, ACK and session close are then joined and quiescence checked again.
API/inventory/cleanup failures keep draining visibly blocked.

The controller starts the pinned `scripts/Restart-Controller.ps1` as an independent,
one-shot process. Native creation requests Job Object breakaway and checks the
suspended child before allowing it to run. Restrictive/nested jobs use Windows'
existing WMI `Win32_Process.Create` broker with breakaway and no window. The broker
and helper verify the account SID; the helper validates actual credential,
Hyper-V and startup access in its own token **before acknowledging**. Only after
that ACK does the original commit Restarting and exit. No new restart scheduled
task, service or permanent supervisor is installed. See Microsoft's
[Job Object behavior](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)
and [WMI process creation](https://learn.microsoft.com/en-us/windows/win32/cimwin32prov/create-method-in-class-win32-process).

The helper leases a crash-released file lock, verifies PID/nonce/creation time
and executable, and waits up to 120 seconds for the original process. It never
kills a controller. After exit it acquires the existing management/data/scale-set
locks and revalidates the exact target. Executable promotion uses atomic
replacement with `bin/clc-runners.exe.previous` and matching previous build
metadata retained. When configured, it triggers the existing `CLC-Linux-Runners`
task with its existing account, mode, working directory and permissions. Otherwise
it uses the ordinary installed-executable Start path. Readiness requires a fresh
listening replacement nonce and matching config/PAT/binary/runtime/image hashes.
Only then is lastSuccess recorded. Individual validators have 180-second process
timeouts, preflight has a ten-minute budget, helper ACK has three minutes and
replacement readiness has 120 seconds. Lock/task waits are bounded.

Unattended safeguards remain strict. The existing fingerprint already separates
ordinary controller/runner settings from the full Hyper-V hardware configuration.
It binds SID, config/data/PAT paths, executable, image and probe/startup
implementation. S4U/Interactive modes also bind boot/session; Password mode
instead binds machine identity and the saved PAT hash and survives those session
changes. CPU/RAM/disk, binary, image and probe changes can
therefore stage successfully yet report **Unattended validation required**.
They need a quiet, stopped TestUnattended/Activate cycle; no previous result is
silently carried forward. A brokered helper whose session/token is incompatible
also fails before ACK. Actual S4U sign-out/cold-boot behavior remains subject to
the existing separate validation, not a claimed consequence of restart tests.

Status retains its JSON structure and adds `runtime.restartSupported` and
`runtime.restart` (also read from durable `restart.json` after a helper failure).
Restart includes request source/reason, desired identity, detection/drain/attempt/
success timestamps, assignment/cleanup counts, blocker and lastFailure. Capacity
shows a connected listener with zero listener maximum while draining. Jobs adds
one bounded-width restart line; Jobs -Json retains schema version 1 and adds
`controller.restart`, with full diagnostics. `<config>.effective.json` contains
only nonsecret loaded settings and supports read-only diagnostics while the desired
configuration is malformed, missing or has an incompatible ownership/path edit.

Failure does not automatically retry the same target. Correct the reported input,
rerun Activate to prepare/validate it, or explicitly retry RestartWhenIdle. New
stable inputs supersede a pending target; reverting an automatic change resumes
the running identity. Helper launch/ACK failures keep the original process alive
and resume acceptance. Failures after original exit retain diagnostics and the
previous executable; the existing boot/sign-in/crash recovery path remains.
No automatic rollback is attempted with an unproven configuration/version pair.

Inspect `local/data/restart.json`, `restart-check.json`, `restart-ack.json`,
controller logs and `startup-*.stderr.log`. An interrupted drain rebinds to the
new instance nonce; an interrupted handoff becomes a visible Failed state when
normal startup recovery takes over. A live helper gates premature startup until
installation is released. Stale helpers cannot replace/terminate a newer process.
Do not delete locks/state or kill processes to bypass a drain. After correcting
configuration, permissions, startup validation or storage, use Activate. Manual
binary recovery may restore `.previous` **and its build metadata only while all
controllers sharing the binary are stopped and the previous version is known
compatible with the configuration/image, with no outstanding owned jobs or
cleanup**; otherwise rebuild through Activate. Unrecognized current executable
bytes are saved separately as `.unvalidated-<id>` and never replace `.previous`.

For the first upgrade from a controller without `restartSupported`, wait for a
quiet GitHub queue and **zero assigned jobs and zero journaled runners** before
Stop. The older shutdown implementation cannot provide the new acceptance fence.
Under the controller account/token with working Hyper-V access:

```powershell
.\runners.ps1 Status
.\runners.ps1 Jobs
# After the quiet/zero-assignment/zero-runner condition above:
.\runners.ps1 Stop
# Confirm Status shows processAlive=false and no remaining runner cleanup.
.\runners.ps1 Activate
# Activate performs TestUnattended again when the startup fingerprint is stale.
.\runners.ps1 Status
.\runners.ps1 Jobs
# Later, to request a manual graceful restart:
.\runners.ps1 RestartWhenIdle
```

Use `TestUnattended` explicitly while stopped when diagnosing a validation blocker;
include `-StartupMode Password` for a Password task.
Keep the same config path and correct controller account. No image force-rebuild
is needed for this controller-only upgrade. Review the
[pre-change audit](restart-audit.md) and [restart validation evidence](validation.md#graceful-controller-restart).

## Credentials and GitHub identities

The existing PAT is preserved. For a genuinely missing token, interactive
activation prompts once with hidden input. Noninteractive runs require a saved
token, or explicit `Activate -FromGitHubCLI` to import a missing one. Explicit
rotation uses `SetToken` or `SetToken -FromGitHubCLI`. A fine-grained PAT needs
resource owner `clcdpc`, organization Administration read and Self-hosted runners
read/write, plus any required organization approval/SSO. See
[GitHub authentication guidance](https://docs.github.com/en/actions/how-tos/manage-runners/use-actions-runner-controller/authenticate-to-the-api).
HTTP 401/403 preserves the credential and reports the permission failure.

`Setup` reconciles the exact existing `CLC-Runners` / `clc-linux` identities and
saved scale-set ID. An ID mismatch fails closed. Leave `github.repositories` empty
to preserve externally managed repository grants/public-repository policy.
Optional configured repository names add grants; existing grants are not removed.
Stopping never deletes the scale set. `DeleteScaleSet -ConfirmName clc-linux` is
a separate guarded operation requiring no owned runners or jobs; it never deletes
the runner group and is not part of migration or normal recovery.

## Image preparation and updates

```powershell
.\runners.ps1 Activate -ForceRebuild
# Or, after Stop has finished and Status shows stopped:
.\runners.ps1 PrepareImage
.\runners.ps1 PrepareImage -ForceRebuild
```

`BuildImage` is an alias for `PrepareImage`; `-NoCache` also forces a fresh image.
The builder takes up to 90 minutes, writes build/package logs under
`local/logs/images/`, and preserves the previous current image on failure.
Metadata and validation live beside each read-only VHDX; `current.json` selects
the promoted version. Keep parent files at their published paths.

Activate's runner-image step reports the reused version or the reason for a new
build. Installation commands are in `internal/hyperv/guest/provision.sh`; changes
to that file, sealing, runner scripts/hooks, the service, source recipe or disk
capacity invalidate the image. Build/verification harnesses and controller or
external TestHyperV changes do not. Keep installation changes in `provision.sh`.
Current verification is sent separately over pinned SSH. Existing legacy images
are reusable only when their complete original fingerprint can be reconstructed
from the same provisioning inputs; their manifest and disk checks remain required.

During a build, progress names download, conversion, cloud-init installation,
verification, sealing and clone validation. Failure summaries include the guest's
command/exit status or Bash error when available. `local/data/image-build.json`
records the result and retained diagnostic path. The image log includes both
cloud-init logs and status; log capture precedes VM disposal. An interrupted VM
gets a separate `<vm>-recovery.log` before cleanup. Preserve these logs when
diagnosing an unsuccessful Activate. See the [HOME/fingerprint repair audit](validation.md#activate-image-repair-october-28-2026)
for the October 28 failure and exact retry command.

On Windows, QEMU-created files carry NTFS's `SparseFile` flag, which Hyper-V
rejects with `0xC03A001A`. Image preparation instead uses `New-VHD -Dynamic` at
the QCOW2's logical size, then `qemu-img convert -n -f qcow2 -O vhdx` to populate
that existing container. It compares guest-visible sectors with `qemu-img compare`,
grows the disk to `hyperv.diskGiB` using `Resize-VHD`, and requires `Get-VHD` and
`Test-VHD` checks before VM creation. Full builder boot, sealing and fresh clone
boot/Docker smoke validation still gate promotion.

Sparse, compressed or encrypted VHDX files are rejected with their path and
attribute names, including cached bases and disks moved between configured
directories. Use uncompressed/unencrypted locations for both `imageDirectory`
and `dataDirectory`; rerun `PrepareImage -ForceRebuild` to replace an invalid
base. Never change attributes or contents on a base referenced by an active VM.

Verified Ubuntu downloads are retained in `images/sources/<sha256>.qcow2` under
the configured image directory. Each rebuild refreshes Canonical's small checksum
manifest and rehashes the cached source before reuse; QEMU is reused when its
installed version matches the recipe. Retries use fresh candidate directories
and recover only owned build/validation VMs. Each candidate gets a durable
`build-intent.json` before conversion; the conversion request carries the same
deployment owner and build version. Its normalized Windows destination must be
exactly `images/<version>/converted.vhdx` under the configured image directory.
An interrupted legacy source is migrated into the verified cache only with
matching build ownership and the expected source checksum.
Partial or corrupt downloads and partial VHDX files never count as valid images;
failed candidates remain available until the next rebuild or `CleanupImages`.
Before conversion, a rebuild removes only incomplete candidates with matching
build intents, recognized regular files, and no registered VM disk-chain or
durable record references. Published manifests, the current version, the active
build and source cache are preserved. Links or unknown files stop deletion;
ownership metadata is deleted last so interrupted cleanup can be retried.
If a cached source is corrupt, the error identifies the exact file to move aside.
The source cache is retained by `CleanupImages`.

A lightweight native storage regression test (no VM, 1 GiB logical disk) is:

```powershell
.\scripts\Test-ImageConversion.ps1 -Live
```

It requires Hyper-V management access and a prepared image directory. Run
`PrepareImage` afterward to validate actual Ubuntu boot and the sealed clone.

`image/recipe.json` selects the official source, optional pinned SHA256, QEMU
version and build timeout. Tool versions and configuration are in
`internal/hyperv/guest/`. Bump the runner version **and archive hash** together;
review GitHub's disabled-update runner version requirements regularly. SDK setup
actions can install the workflow's precise SDK version. The validation workload
uses SDK 10.0.303 and Playwright system dependencies for version 1.62.1.

QEMU is installed only if missing, using WinGet with its package manifest hash:

```powershell
winget install --id SoftwareFreedomConservancy.QEMU --exact --version 11.1.0 --source winget --silent --accept-package-agreements --accept-source-agreements --disable-interactivity
```

If local policy denies this installation, install the Windows build linked from
[qemu.org](https://www.qemu.org/download/#windows), verify its published checksum,
and set `hyperv.qemuImg` to `qemu-img.exe`. OpenSSH, WinGet/QEMU installation or
Hyper-V feature operations can require administrator rights; no unverified
download or hash-bypass flag is used. There is no runtime WSL dependency.

## Stop, cleanup and recovery

```powershell
.\runners.ps1 Stop
.\runners.ps1 Status
# Once controller is stopped:
.\runners.ps1 Cleanup
.\runners.ps1 CleanupImages
.\runners.ps1 Activate
```

Stop is asynchronous: it writes the process nonce, stops polling, and waits for
active jobs to finish. Credential/network outages can delay remote cleanup.
Cleanup preserves a server-reported busy runner. A controller restart adopts
owned VMs and their job markers; Windows restart can interrupt jobs because the
whole execution host restarted, but remaining owned resources are reconciled.
Build/validation leftovers are safely distinguishable from job VMs.

Preserve `local/data/state.json`, `network.json`, per-VM `record.json`, image
manifests and configuration. Corruption/mismatched ownership fails closed; restore
the correct record/configuration rather than weakening identity checks. Migration
backs up legacy configuration and empty state as `*.wslc-v1.bak`, preserving
owner/scale-set ID. Nonempty legacy journals must be drained with the previous
controller before upgrading. No old containers, installations or unrelated
software are deleted automatically.

CleanupImages protects registered disk chains, durable image references, current
and newest validated versions. Abandoned unvalidated candidates require matching
build intent and known files. Unrecognized content is preserved/refused rather
than recursively pruned. A failed VM removal retains its manifest/allocation.

## Job visibility

```powershell
.\runners.ps1 Jobs
.\runners.ps1 Jobs -Json
# Custom installation:
.\runners.ps1 Jobs -Config C:\path\controller.json -Json
```

`Jobs` lists every runner still in the controller journal, sorted by full name.
Provisioning, Idle, Assigned, Running, Completing, Cleaning up, Failed and Unknown
describe the existing lifecycle and execution evidence. Completed jobs lose their
assignment metadata immediately; the occupied runner remains until VM cleanup
finishes. Fully retired runners disappear. Empty journals display an explicit
message. Console names use the final 12 characters; long names are truncated to
fit an ordinary terminal. JSON retains full identities and metadata.

The scale-set `JobStarted` event supplies the exact runner name/ID, request ID,
repository owner/name, display name, workflow reference, workflow run ID and opaque
assignment job ID. The latter is **not** the numeric REST workflow job ID. No
queued-workflow or display-name matching is used. Workflow name and YAML job key
are separate fields; unavailable values remain empty. Existing job-start hook
marker timestamps are read during the regular SSH lifecycle probe, with no new
guest hooks, guest endpoint, workflow changes or image rebuild.

Only `Jobs` performs optional workflow-jobs API GETs, grouped by the known
repository/run, matching both exact runner ID and name across run attempts. This
adds workflow name, display name, numeric job ID, run attempt, job URL and actual
start time. There are at most three concurrent requests, five pages per run and
a five-second overall lookup timeout. It uses the existing DPAPI credential;
no token permissions are changed. The API requires repository Actions read
access for private repositories ([GitHub API documentation](https://docs.github.com/en/rest/actions/workflow-jobs#list-jobs-for-a-workflow-run)).
Missing access, unavailable credentials, network errors or delayed API metadata
leave local assignments visible with a JSON `lookupError`. There is no additional
continuous API polling. If the API confirms completion, the view clears that
assignment without writing controller state.

Job elapsed time comes from API `started_at`, or the existing job-start marker's
UTC timestamp, never VM creation or assignment time. `18s`, `4m`, `38m` and
`1h 12m` are compact display examples. An asterisk means elapsed time in the
provisioning/completion/cleanup/failure phase instead. Missing, malformed or future
timestamps display `-`; no negative durations are emitted. UTC arithmetic handles
DST. Restarted controllers must observe new listener/guest evidence before local
execution is confirmed. A stopped/stale controller or failed probe makes an
unconfirmed assignment Unknown; its elapsed time is withheld. Exact API evidence
can still confirm execution when the controller is unavailable.

JSON schema version 1 has `schemaVersion`, `generatedAt`, `controller`,
`maximumConcurrency`, `occupiedCapacity`, `availableCapacity` and a `runners`
array (including when empty). Concurrency is the configured cap; occupied slots
include partial provisioning and cleanup, and available capacity is clamped at
zero. Each row includes full runner/VM identities, original `phase`, display
`state`, `executionState`/`executionSource`, workflow/repository/job identifiers,
`assignedAt`, `jobStartedAt`, nullable numeric `elapsedSeconds`, `elapsedSource`,
`metadataStatus` and `lookupError`. Unknown IDs are zero, strings empty and
timestamps/durations null. `metadataStatus` is `not-applicable`, `missing`,
`assignment`, `github-api` or `completed`. `elapsedSource` is `runner-hook`,
`github-api`, `phase` or empty. Optional journal fields extend state version 2;
existing state files remain readable without migration.

`Status` continues to return its detailed diagnostic JSON, including native VM
inventory/admission checks. `Jobs` reads only the journal/runtime files and
optional API data; it does not probe guests, provision, stop, cancel, register or
delete anything. PowerShell caches a reader executable under
`bin/readers/<source-fingerprint>/` so source updates can be inspected while the
current controller binary remains in use. This compilation cache is the only
local build output; controller configuration, journal, credentials, images and
live executable are untouched.

An older running controller has not saved assignment metadata or observation
timestamps. Its runners therefore show Unknown with placeholders until a planned
activation loads the new controller. No automatic restart is performed by Jobs.
At the next approved maintenance window, use the normal graceful update:

```powershell
.\runners.ps1 Activate -NoStartup
.\runners.ps1 Jobs
.\runners.ps1 Jobs -Json
.\runners.ps1 Status
```

Run activation under the controller account with the same elevation/access as the
live controller so its process identity can be verified. Activation drains active
jobs before replacing its binary and reuses the compatible Ubuntu image. Do not
kill the controller or replace active VMs to test visibility. During the next
ordinary workflow runs, compare JSON job IDs/URLs and runner IDs with GitHub,
repeat Jobs to check elapsed time, and observe retirement; two or three lightweight
jobs can validate concurrency without expensive full CI runs.

## Diagnostics and unattended startup

`Status` preserves its existing JSON keys and adds:

- `configuredResources`: requested settings from the JSON on disk;
- `configurationCurrent`: whether that file matches the running controller's
  loaded configuration hash, and `runtimeFresh`: process/heartbeat freshness;
- `runtime.resources`: settings actually loaded at controller startup;
- `runtime.capacity`: maximum/minimum, current/busy counts, available slots,
  desired/assigned demand, last successful capacity report, admission measurements,
  resource blockage, next retry and most recent provisioning issue;
- `admission` or `admissionError`: a current read-only check for one additional VM
  using the requested settings;
- native `vms[].cpus`, `memoryStartupBytes`, `memoryAssignedBytes` and
  `dynamicMemoryEnabled`, read with `Get-VMProcessor` and `Get-VMMemory`.

Older controllers omit `runtime.resources`/`runtime.capacity`; those values are
unknown until activation, rather than inferred from edited configuration.
Historical admission/advertisement measurements include timestamps. The
advertised maximum is the total scale-set cap, not the number of free slots.
An inaccessible inventory is reported explicitly and never presented as no VMs.
The most recent issue also persists in `state.lastProvisioningIssue` across
controller restarts. Status does not read or display PAT/JIT contents.

`Status` includes historical runtime state, current journal and actual VM
inventory; `inventoryAvailable:false` explicitly distinguishes an unavailable
Hyper-V module from an empty inventory. Host diagnostics are in
`local/data/host-validation.json`. Controller daily JSON logs retain local times
with offsets; redacted guest journal/runner logs are under `local/logs/runners/`.
See [runner diagnostics](runner-diagnostics.md) for per-source limits, partial
capture reports, redaction and caller-attribution coverage. If the VM is already
off or SSH never worked, guest logs may be unavailable.
Image/cloud-init failures retain build diagnostics when guest SSH can be reached.
Inspect the owned VM console during a failing build with `vmconnect.exe localhost
<exact-vm-name>`; do not alter unrelated VMs. Firmware, host capacity, networking,
cloud-init and Docker failures surface separately in operations/logs.

`EnableStartup` / `TestUnattended` run S4U and Interactive probes at the configured run level
that decrypt a synthetic CurrentUser DPAPI blob and create, boot, SSH-test and
remove an Ubuntu clone. Results include the failed stage and Scheduler exit code.
An S4U registration `Access is denied` is not silently repaired or attributed to
Hyper-V. Auto selects a mode that actually passed. Interactive startup is at
sign-in and requires the account to remain signed in. Password startup is an
explicit additional mode with its own session-0 and saved-PAT checks, described
under [unattended boot startup](#unattended-boot-startup). No auto-logon or
service-account migration is installed.

The old startup task is preserved until a matching Hyper-V probe allows its
replacement. A partial activation can leave the interactive controller running
while returning failure for requested startup registration. `-NoStartup` explicitly
defers that step. `DisableStartup` removes the named task without stopping jobs.
**Logout/cold boot must be validated separately in a maintenance window**, even
after an S4U probe passes. They have not been validated in this migration.
