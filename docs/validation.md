# Validation and acceptance

## Administrator-session account setup, November 7, 2026

`scripts/Setup-RunnerAccount.ps1` prepares/activates a non-admin local account
without a desktop sign-in. A profile-backed worker stores the account's DPAPI PAT
and performs activation and the Password probe. The administrator registers the
fixed Limited boot task after validation; startup failure does not fall back to
a controller tied to the setup session. Retries preserve the prepared identity
and saved credentials, and unused-preparation reset removes the setup receipt.

`go test -count=1 ./...`, `go vet ./...`, and the complete PowerShell suite passed.
The new setup suite passed **72 assertions**, including real child PowerShell
process pipes and CurrentUser DPAPI, non-ASCII passwords, quoted/spaced paths,
error redaction, native task-definition construction, task ACLs, ownership
conflicts, dry runs and retries. Account switching, host provisioning and task
registration/COM are mocked. Activation passed **30 scenarios / 141 assertions**;
Password startup passed **144 assertions**, including validation before brokered
registration; preparation reset passed **195 assertions**. The setup suite runs
inside a scratch working directory, checks write targets and verifies the real
checkout configuration hash is unchanged.

Native acceptance still requires the target Windows Server 2022 host: run setup
for a local account that has never signed in, confirm profile creation and actual
Password probe/registration, inspect the task's account and Limited run level,
then complete a workflow after signing out the administrator and after a manual
reboot. Real credential-based process creation, Scheduler ACL application and
Hyper-V/boot operation were not exercised in this session.

## Storage root and 20 GiB disks, November 6, 2026

Top-level `storageRoot` now bases relative data, image, log and PAT paths on one
folder. Omitted/empty roots preserve the configuration directory; repository
inputs and absolute overrides retain their existing interpretation. Both Go and
PowerShell loaders agree, including resolved recovery snapshots and legacy
migration ownership checks. Loading settings does not move/create storage or
rewrite the configuration.

New configurations use a 20 GiB logical disk and a 20 GiB growth budget; the
minimum configured disk is now 20 GiB. Explicit old allocations remain valid.
The cached Ubuntu QCOW2 was inspected with `qemu-img info` and has a 3.5 GiB
logical source size, below the new target. Changing disk size changes image
inputs and requires a newly built image, rather than shrinking active disks.

`go test ./...`, `go vet ./...` and the complete PowerShell suite passed.
The Hyper-V helper suite passed **93 mocked assertions**, including storage-root
resolution/fallback/overrides and the 35 GiB default admission threshold. The
image conversion suite passed **93 assertions**, including conversion from a
3.5 GiB source to a 20 GiB target. Native image provisioning and clone boot at
20 GiB still require validation on the prepared target host. No existing
controller configuration, VM, VHDX or storage location was changed.

## Host migration controls, November 6, 2026

vCPU admission now permits overcommit while retaining assigned/logical CPU
diagnostics and the existing RAM/disk guards. The Hyper-V helper suite passed
**73 mocked assertions**, including admission with 64 assigned vCPUs on 16
logical processors and continued rejection of RAM/disk shortages.

`controller.startupRunLevel` defaults to `Limited`; `Highest` supports temporary
administrator diagnostics on the target host. Both probe and permanent task use
the configured level. The Password probe verifies the worker's actual elevation,
and changing run level invalidates cached startup validation. The Password suite
passed **129 assertions**, including task-level mismatch rejection, worker-token
mismatch rejection, elevated validation and restoring a limited task. Go config
tests cover old configurations, supported levels and invalid values.

`go test ./...`, `go vet ./...` and the complete PowerShell suite passed.
Task Scheduler and Hyper-V behavior in these suites is mocked; this does not
establish elevated execution or native VM acceptance on the destination host.
No account membership or installed startup task was changed. The existing
`CLC-Linux-Runners` task remained `S4U`/`Ready` at run level `Limited`.

## Password-backed boot startup, November 6, 2026

`EnableStartup -StartupMode Password` installs a boot-triggered task under the
existing controller account, without requiring an interactive sign-in session.
It preserves CurrentUser DPAPI and uses a same-SID Windows credential supplied
locally. A temporary noninteractive probe must decrypt the actual saved PAT,
validate a restricted Hyper-V clone, report session 0/noninteractive execution and
exit successfully before the permanent startup definition is replaced.
`Start`, activation and graceful restart use the registered Password task;
explicit startup-mode changes bypass the ordinary live-update staging shortcut.
The Password fingerprint binds host/account/PAT/runtime inputs across reboots.

The targeted Password suite passed **100 assertions**, including real synthetic
DPAPI round trips, unreadable saved-PAT rejection, same-account checks, password
redaction, boot/action/recovery settings, no interactive fallback, session/host
fingerprints, credential refresh, restart preflight and real CLI dispatch with a
mocked Scheduler. The complete existing PowerShell suite, Go tests and `go vet`
also passed during implementation.

The installed `CLC-Linux-Runners` task remained `S4U`/`Ready`; no real Windows
password was available to register or execute a Password task. Native session-0
Hyper-V, sign-out survival and cold-boot startup have **not** been established in
this session. Enable the mode locally under the intended controller account and
complete those operational checks before treating unattended startup as accepted.

## Runner network restrictions, November 6, 2026

The common native `Create` / `CreateBuild` path now installs and reads back
Hyper-V extended port ACLs while the VM is off. `Start` independently verifies
the complete rule set, single owned-switch adapter, static MAC and guards before
boot. Private/local IPv4, multicast and IPv6 are blocked on that adapter; public
IPv4 uses the allocated guest address. A stateful exception permits controller
SSH from the configured gateway and replies to that session, without allowing
runner-initiated host connections. Builders and validation clones use the same
policy, without changing image provisioning inputs.

`go test ./...`, `go vet ./...` and the full `scripts/Test-Scripts.ps1` suite
passed. The final targeted network suite passed **216 mocked assertions**,
covering private-range boundaries, public IPv4 and DNS packets, IPv6, spoofed
guest addresses, controller SSH session replies, unrelated source-port-22
connections, custom subnets, adapter changes, extra/duplicate/missing rules,
unreadable ACLs, partial installs and refusal to boot after validation failure.
The existing provisioning suite passed **68 assertions**, including installation
of the policy before either runner or build creation returns.

Live validation was unavailable in this session: Windows denied `Get-VM` and
`Get-VMSwitch` because the execution account lacks Hyper-V management permission.
The packet tests model the documented ACL behavior; they do not establish native
ACL readback, NAT connectivity or SSH reachability on the target host. Run
`runners.ps1 TestHyperV` with the intended controller identity on a prepared host
to verify its real clone boot, controller SSH, public DNS/HTTPS and Docker checks.
Image provisioning itself must likewise be exercised under that identity when
building an image. Older running VMs adopt the restrictions when recreated;
stopped VMs without the current policy cannot be started through the controller.

## Jobs command, November 1, 2026

`runners.ps1 Test` passed Go tests with coverage, `go vet ./...`, PowerShell syntax
checks and all existing mocked activation/Hyper-V/image checks. New tests cover
table/JSON contracts, empty and concurrent assignments (including the same
repository/run/display name), terminal width, UTC/DST and malformed timestamps,
timeouts/permissions/pagination, isolated completion, replay/restart recovery,
old v2 journals and read-only command execution. Final affected-package checks
also passed after the recovery and Unicode-width review. Jobs view/enrichment
coverage was 94.2%; PowerShell reader/dispatch checks passed seven assertions.

Safe live checks at approximately 23:17 Eastern used:

```powershell
.\runners.ps1 Jobs
.\runners.ps1 Jobs -Json
.\runners.ps1 Status
```

Both JSON outputs parsed successfully. The existing controller remained listening
with the same process identity and executable SHA256 before and after the checks. No
controller restart, VM replacement or workflow dispatch was performed. The reader
reported one occupied slot out of three, matching the existing journal. The
runner identifier below is illustrative:

```text
CLC GitHub Runners - Active Jobs

Runner       State        Repository             Job                  Elapsed
0123456789ab Unknown      -                      -                    -

1 unknown | 1/3 capacity
Some job metadata is unavailable; see Jobs -Json for details.
```

This older controller has no persisted job metadata/observation timestamps, so
its busy runner was intentionally not asserted to be Running. Actual repository,
workflow-job URL, advancing elapsed time and concurrent GitHub association are
**not live verified**. Native Status inventory/admission limitations remained
visible in its existing diagnostic fields. At a planned update, follow
[job visibility activation and verification](operations.md#job-visibility), then
compare the next ordinary jobs' exact IDs/URLs with their GitHub Actions pages.

## Runner resource optimization, November 1, 2026

The evaluated target was 2 vCPUs, 10 GiB static RAM, 100 GiB logical disk and
three on-demand slots with zero idle runners. See the
[resource qualification report](resource-qualification-2026-11-01.md) for the
confirmed memory-admission/retry behavior, five successful application baseline runs,
local regression results and exact safe activation commands. The active full CI
job was preserved; native reduced-resource and concurrent GitHub acceptance
remain pending. Hardware-only changes do not require image preparation.

## Activate image repair, October 28, 2026

The supplied activation diagnostics and current image manifest establish two
separate defects. Commit `5fd4f5f` introduced transcript capture using
`$HOME/.clc/validation.log` at line 111. Commit `54c6ef3` retained that dependency,
moved it to line 143 and added another HOME use for the final success marker.
Line 143 expanded `$HOME` in `mkdir` before installing the verifier's EXIT trap.
The build's NoCloud `runcmd` invokes
`bash /opt/clc/prepare.sh`, which then invoked the verifier as root. Neither
script provided a login environment. `set -u` therefore terminated verification
before its first Docker check, and cloud-init subsequently failed `scripts_user`.
TestHyperV supplied the verifier over SSH as runner, whose login environment
contained HOME; it did not exercise the root cloud-init build environment.
The successful image log contains `CLC_VALIDATION_PASSED`; the failed log's actual
error is `/opt/clc/verify.sh: line 143: HOME: unbound variable`.

Cloud-init's [scripts-user module](https://github.com/canonical/cloud-init/blob/main/cloudinit/config/cc_scripts_user.py)
runs scripts through `runparts`; its [subprocess implementation](https://github.com/canonical/cloud-init/blob/main/cloudinit/subp.py)
copies the service's environment. It does not synthesize a login HOME. Root UID
does not imply that `$HOME` is set in a non-login Bash process.

The old fingerprint hashed the raw recipe, `diskGiB`, and every embedded guest
file. Recomputing it from Git and the unchanged local LF recipe/disk setting
produces these exact values:

| Inputs from commit | Legacy SHA256 fingerprint |
| --- | --- |
| `ec5960e`, used by the validated image | `2b054e95ee248fed24dd608781e5c23c688b03df4252959ad1f3ecd9c12e8ac8` |
| `5fd4f5f` | `44e0c93ecd50335a23484012f5f69a9e860b2e9da6b301bf88cbc0b98eb7e84f` |
| `54c6ef3` | `7b23dc92df45dadccb8ef5b58b67829395467180bbec9abb92849a5e85bcd79f` |

**Only `guest/verify.sh` changed** among those fingerprint inputs. Recipe, disk
capacity, installation commands, seal script, runner, hooks and systemd unit did
not change. The rebuild was deterministic under the previous rule but unnecessary
for the base image. Controller source and Go tests were never directly hashed;
external TestHyperV improvements affected the image through the shared verifier.

The repair separates unchanged installation commands into `guest/provision.sh`.
The build harness sets root's HOME explicitly, records installation commands and
exit status, and runs smoke verification as runner. The verifier derives HOME
from `getent passwd` for the effective numeric UID and rejects missing/mismatched
accounts and unavailable or relative homes. It ignores inherited HOME and
SUDO_USER when choosing caches and transcript paths. Its ordered smoke/full
validation stages, timeouts, stdin isolation, redaction and cleanup remain required.
The other guest scripts use explicit `/home/runner` or `/root` paths; the runner's
systemd service already declares `HOME=/home/runner`.

New fingerprints include installation/sealing/runner/service/hook contents,
source recipe settings, QEMU version and disk capacity. Build/verification
harnesses, external validation/controller code, build timeout and text formatting
do not invalidate the base. New manifests record component hashes to name actual
changes. Legacy compatibility reconstructs the **entire old fingerprint** using
current provisioning bytes and an archived, never-executed original verifier;
unknown fingerprints and changed provisioning cannot match. No image-ID exemption,
manifest rewrite or skipped VHDX integrity check is used. Fresh clone verification
also receives the current verifier through pinned SSH rather than baked checks.

Activate now displays reuse/rebuild reasons and the current provisioning command.
Command/exit records and legacy Bash errors reach its failure summary. Build logs,
both cloud-init logs, cloud-init status and guest verification evidence are retained
outside disposable VM directories; `data/image-build.json` identifies the log and
failure. Capture and cleanup use independent deadlines, and interrupted-build
recovery attempts evidence collection before deleting VM/SSH files. Logger and
capture failures cannot produce a successful build. The closure review also moved
verification's temporary allocation under its cleanup trap, retained original
failure statuses, bound provisioning to its recipe snapshot, and rejected reuse of
another deployment's image.
Controller startup and offline cleanup share the same evidence-preserving
auxiliary-VM recovery path, including when activation reuses an existing image.

Read-only inspection with the repaired binary reports `current: true` for the
existing Ubuntu 24.04 image after checking its unchanged VHDX SHA256 against
the saved manifest.
Its manifest and `current.json` remain unchanged; no image was built or replaced.
The pre-existing full TestHyperV report at `2026-10-29T00:26:22Z` records all 29
stages, full guest validation and successful cleanup for that disk ID. This repair
does not claim a new live activation: this account's read-only `Get-VM` still fails
with the Hyper-V authorization-policy error. Evidence is retained in
`artifacts/activation-home-hyperv-access.log`. Native race testing requires a
supported C compiler, which is unavailable; `TestRace` reports that prerequisite.

Retry under the controller account with Hyper-V management access, in PowerShell 7
from this checkout. The normal command should reuse the validated image:

```powershell
.\runners.ps1 Activate -Config .\local\controller.json
```

Do not add `-ForceRebuild` or `-NoCache` for this repair; those explicitly request
a new image. Automated results are saved in `artifacts/activation-home-tests.log`
and `artifacts/activation-home-dotnet-tests.log`. They cover unset/misdirected HOME,
root/runner separation, invalid accounts, logger failure, command/exit reporting,
fingerprint stability/invalidation, exact legacy reuse, disk integrity, cancellation
and evidence retention before safe cleanup. They do not establish live activation.
The complete `Test` command passes 82 top-level Go tests with coverage and vet,
seven Bash syntax checks, 19 PowerShell syntax checks, 18 activation scenarios /
95 assertions, 61 activation helper assertions, 50 Hyper-V helper assertions and
88 image conversion assertions. All three sample .NET tests pass. The final
startup-recovery change also passes the focused pipeline/recovery/integrity tests
and Go vet.

## TestHyperV completeness audit, October 28, 2026

The apparent full pass at **18:55 EDT** was invalid. Its transcript ended after
`Compose exec proof`, followed by cleanup and `CLC_VALIDATION_PASSED`; it did not
execute SQL, .NET integration/publishing or Chromium. Those checks were still in
the source. TestHyperV sends the current verifier through `bash -s` over SSH,
while `docker compose exec -T` still forwards stdin by default. Its client consumed
the remaining script. Bash reached EOF with exit status zero, and the EXIT trap
treated that as success. [Docker documents `--interactive=false` for scripts](https://docs.docker.com/reference/cli/docker/compose/exec/).
The previous regression tests ran `bash verify.sh`, so a child reading stdin
could not consume their program. The controller also accepted any occurrence of
the success marker and initialized `fullPlatformTest` from the requested mode.

The verifier now redirects probe, retry and diagnostic commands from `/dev/null`
and disables Compose exec interactivity explicitly. Its mandatory ordered stage
list exists before workloads start. The EXIT trap requires every stage and the
explicit end of the sequence, successful workload cleanup and drained transcript
capture before emitting completion and success. Early EOF, `exit 0`, a skipped
full block or a missing stage fails closed. The controller independently checks
all ordered stage records, the requested completion mode and the final exact
success line. A legacy marker alone and smoke-only results cannot pass TestHyperV.

Full validation requires all **29 stages**, including the following groups:

| Checks | Required execution |
| --- | --- |
| Guest/toolchain/network | Cloud-init and pinned SSH readiness; Docker daemon, Compose and Buildx; .NET 10 SDK and writable install directory; Node 24, npm, PowerShell 7, Python, Git, runner executable; GitHub DNS/HTTPS |
| Native Docker | hello-world pull/run; build; running-container exec proof; Compose health readiness, exec proof and shutdown |
| SQL Server 2022 | Resource preflight; pull/start `mcr.microsoft.com/mssql/server:2022-latest`; running-state checks; bounded real queries inside the server and through Ubuntu's published loopback using a host-network client; server major version 16 and result 42; exact port mapping and localhost socket |
| .NET/workflow prerequisites | SqlClient query from Ubuntu through `localhost,1433`; actual MSTest SQL assertions via `dotnet test`; self-contained win-x64 publish; nonempty ZIP creation and CRC integrity check |
| Chromium | Pinned Playwright download; shared-library inspection of the actual Chromium executable with `ldd`; normal Playwright host dependency validation; headless launch; HTTP test-page navigation, title/heading assertions and a button click that must change the DOM; SQL must still be running afterward |
| Disposal/evidence | Bounded diagnostics before container removal, saved redacted transcript, VM/disk/seed/key/allocation removal and successful host transcript writes |

The audit found no actual `dotnet test` or ZIP validation in the previous platform
sequence; both now execute. Chromium dependencies are verified without installing
missing packages during validation, so an incomplete base image fails. Image
preparation continues to install the dependencies through the pinned Playwright
CLI. SQL retry budgets, per-attempt process/login/query limits, memory checks,
startup/error/OOM diagnostics and independent VM cleanup deadlines remain in force.
The final SQL state check also detects a server that dies during later checks.

Reports include `validationMode`, `requiredStages` and `completedStages`.
`fullPlatformTest` begins false and becomes true only after a full guest sequence,
transcript capture and VM/file cleanup succeed. `guestValidationPassed` may remain
true when later VM disposal fails; overall `passed` and `fullPlatformTest` remain
false. Startup probes use a separate report and always keep `fullPlatformTest`
false. Existing immutable images receive the current verifier over SSH; rebuilding
the image is unnecessary to obtain these checks.

Regression tests now execute the embedded Bash through stdin with a Compose-like
stdin consumer. They reject skipped SQL/browser stages, early successful exit,
wrong SQL results, test/publish/package failures, missing browser dependencies,
browser failure, late SQL exit and cleanup failure. Independent controller tests
remove each required stage in turn. The actual embedded browser JavaScript is
also exercised against simulated browser APIs and a real local HTTP server to
prove that missing libraries, wrong page contents and a failed DOM change fail.
These automated fixtures do not establish live SQL or Chromium compatibility.

`.\runners.ps1 Test` passes **77 top-level Go tests**, coverage and vet, all six
embedded Bash syntax checks, 19 PowerShell syntax checks, 16 activation scenarios /
82 assertions, 58 activation helper assertions, 50 Hyper-V helper assertions and
88 image conversion assertions. All three sample .NET tests pass. The exact
embedded SQL console and MSTest probes compile, the SQL test is discovered, and
self-contained win-x64 publishing plus the embedded ZIP integrity check pass.
The SQL test itself requires the real guest server and was not run locally.
Probe build/publish logs retain the pre-existing NuGet warnings for SqlClient's
transitive `System.Security.Cryptography.Pkcs` 9.0.11 dependency; versions were not
changed by this audit. Native Go race testing remains unavailable because no
supported C compiler is configured.

This account still cannot perform live Hyper-V validation: read-only `Get-VM`
returns “You do not have the required permission to complete this task.” No VM
was allocated by this audit. The invalid report was preserved as
`artifacts/testhyperv-audit-false-pass.json`; host permission evidence is in
`artifacts/testhyperv-audit-hyperv-access.log`. Automated test output is saved in
`artifacts/testhyperv-audit-tests.log`.

The operator should rerun `TestHyperV` under the controller account with Hyper-V
management access after draining/stopping the controller, then inspect the log
and the complete report:

```powershell
.\runners.ps1 TestHyperV -Config local/controller.json
if ($LASTEXITCODE) { throw 'Inspect the printed diagnostic log and report.' }
$report = Get-Content local/data/hyperv-validation.json -Raw | ConvertFrom-Json
Get-Content -LiteralPath $report.diagnosticLog
if (-not ($report.passed -and $report.guestValidationPassed -and
          $report.fullPlatformTest -and $report.cleanupPassed -and
          $report.validationMode -eq 'full') -or
    $report.requiredStages.Count -ne 29 -or
    ($report.requiredStages -join ',') -ne ($report.completedStages -join ',')) {
    throw 'Mandatory full-platform checks and disposal have not all passed.'
}
```

The original acceptance review also retains the separate live gates below:
GitHub runner registration/JIT, Docker-action and workflow service-container
jobs, scaling/cancellation/restart recovery, the unchanged reference application workflow
(including its SDK selection, accessibility tests and artifact upload), and
logout/cold-boot startup. TestHyperV does not claim these results, and
`githubWorkflowValidated` remains false. A corrected platform pass is required
before proceeding to those gates.

## TestHyperV SQL Server repair, October 28, 2026

The saved real execution at `2026-10-28T22:20:11Z` reached Docker run/build/Compose
and downloaded SQL Server 2022. The implementation suppressed every internal
`sqlcmd` attempt and proceeded only after one succeeded. The visible login timeout
and TCP `0x2749` therefore came from the **next, single-attempt host-network
query**, rather than the internal readiness loop. The report says cleanup passed.
The old VM/container has already been removed, and no SQL exit state, errorlog,
port inventory or resource snapshot was preserved. Its exact runtime cause
cannot be reconstructed from that report alone.

Inspection established:

- `docker run -d --name clc-sql` created a regular bridge container with
  `ACCEPT_EULA=Y` and `MSSQL_SA_PASSWORD`. The guest-generated password contains
  48 random hexadecimal characters plus a fixed complexity prefix/suffix; it
  satisfies the documented password requirements. The initial successful query
  also demonstrates that the credentials worked at that moment.
- The local configuration supplies 4 CPUs and 16 GiB of fixed guest memory. The
  old Docker invocation set no container memory limit, so there is no evidence
  that it was capped below SQL Server's minimum. There was also no measurement of
  available memory or OOM status. [Microsoft's container prerequisites](https://learn.microsoft.com/en-us/sql/linux/quickstart-install-connect-docker?view=sql-server-ver16&pivots=cs1-bash)
  require at least 2 GiB for SQL Server.
- `-p 127.0.0.1:1433:1433` correctly publishes TCP 1433 on Ubuntu's IPv4 loopback.
  The old client used `localhost`, which can also resolve to IPv6 `::1`.
  [Docker documents IPv4 and IPv6 loopback publications separately](https://docs.docker.com/engine/network/port-publishing/).
  The old report does not show which address the failed client tried.
- Internal readiness retried 90 times with two-second sleeps, but had no explicit
  login/query/process timeout or container-state check. The Ubuntu host query,
  socket probe and .NET open had no readiness retry. SQL could exit after the
  internal query, and the old code would report only a connection failure.

The repaired sequence uses 180-second wall-clock budgets independently for the
internal and Ubuntu loopback SQL queries. Each attempt checks process state before
and after querying, stops immediately for exit/restart/OOM, and uses `sqlcmd -b`
with three-second login, five-second query and at-most-ten-second process limits.
The OS timeout has a two-second kill grace. Inspect calls and sleeps are bounded
too. A bad port mapping fails explicitly; a live server with an unreachable
published port produces a separate networking timeout. The host-network client
uses explicit TCP `127.0.0.1,1433`, matching the publication. A Python socket probe
checks IPv4 `localhost`, and the real .NET SqlClient probe still queries
`localhost,1433`, with a 30-second cancellation budget and at most ten attempts.

SQL preflight records Docker/guest memory and requires at least 4 GiB Docker memory
and 3 GiB available guest memory. The server has a 4 GiB Docker limit (no extra
swap allowance), a 3072 MiB SQL memory limit and explicit Developer edition.
Credentials are inherited through environment variables, not put in command
arguments. Passwords, credential assignments, generated SQL secrets and common
GitHub tokens are redacted before streaming or saving output.

Failure evidence includes `docker ps -a`, selected exit/OOM/error state and
resource limits, Docker logs, published ports, stats, guest CPU/memory/disk and
cgroup information, localhost resolution, listeners and Docker/kernel journals.
`docker cp` retrieves `/var/opt/mssql/log/errorlog` even from a stopped server.
Individual unavailable diagnostics are reported explicitly. Guest diagnostics
run before container cleanup; the controller saves a transcript and report
snapshot outside the VM before removing it. If SSH fails or is cancelled, a
separate bounded recovery attempt retrieves the saved guest log and runs current
diagnostic-only checks before host cleanup. Diagnostic recovery never restarts
or removes containers. Overall success requires successful guest checks,
transcript capture **and** VM/file cleanup.

TestHyperV sends the current embedded script over pinned SSH stdin. An existing
validated Ubuntu base therefore uses these checks immediately, without rebuilding
or changing the base image. Long operations show phase messages, retry counts and
20-second host heartbeats. Per-run redacted transcripts remain under
`local/logs/validation/<vm>.log`; `local/data/hyperv-validation.json` contains the
exact `diagnosticLog` path and separate guest/cleanup results. Guest copies live
at `/home/runner/.clc/validation.log` until the disposable VM is removed.

The complete sequence was reviewed. Docker daemon and exec readiness now retry;
Compose uses a real health check and `up --wait --wait-timeout 30`. Its proof is
checked before shutdown. The probe workload no longer expires after a fixed
five-minute sleep during later downloads. SQL success continues through .NET
querying, self-contained win-x64 publishing, Playwright installation and a real
Chromium launch/page assertion. Downloads/builds/browser execution have finite
process limits. Guest success is emitted only after successful workload cleanup.
Cloud-init/SSH/guest-ready gates and the owned VM lifecycle remain required.

The non-elevated regression suite executes the actual embedded Bash script with
simulated external tools. It covers delayed Docker/SQL readiness, early exit,
exit after an internal query and during the host query, wrong mappings, unreachable
loopback, hung queries, timeout evidence/redaction, low memory, unhealthy Compose,
Chromium failure, cleanup failure, smoke mode and interrupted diagnostic recovery.
Controller tests verify current-script delivery, streamed stdout/stderr, split
credential redaction, output limits, failed transcript writes, cancellation,
missing success markers and durable evidence before VM destruction.

`.\runners.ps1 Test` passes 74 top-level Go tests with coverage and vet, all six embedded
Bash syntax checks, 19 PowerShell syntax checks, 16 activation scenarios / 82
assertions, 58 activation helper assertions, 50 Hyper-V helper assertions and 88
image conversion assertions. All three sample .NET tests pass. The exact embedded
SQL C# probe also compiles on .NET 10; this is a compilation check, not proof of
SQL connectivity. Its self-contained win-x64 publish also succeeds. Its existing
pinned SqlClient dependency produces NuGet audit warnings for transitive
`System.Security.Cryptography.Pkcs` 9.0.11; dependency
versions were retained for the existing compatibility target. Detailed output
is in `artifacts/testhyperv-dotnet-tests.log`,
`artifacts/testhyperv-sql-probe-build.log` and
`artifacts/testhyperv-sql-probe-publish.log`.

The real retry in this session stopped at host preflight: Hyper-V `Get-VM` on
the validation host returned “You do not have the required permission to complete this
task.” No new VM or container was created, and the existing base was not changed.
Docker Desktop's Linux engine is not running, so a separate local Docker/SQL run
was unavailable too. This repair does **not** claim a successful real SQL/browser
execution. Evidence is in `artifacts/testhyperv-live.log`, the original report is
copied to `artifacts/testhyperv-original-failure.json`, and automated results are
in `artifacts/testhyperv-tests.log`. Race detection needs a supported Windows C
compiler; the attempted `TestRace` reports that none is configured.

Operator retry, from PowerShell 7 under the controller account with Hyper-V
management access (sign out/in after a membership change, or use an authorized
elevated session):

```powershell
Set-Location C:\Apps\github-runners
Import-Module Hyper-V
Get-VM -ErrorAction Stop  # must succeed under the token that runs TestHyperV
# Drain/stop the controller before the offline test; let active jobs finish.
.\runners.ps1 Stop
.\runners.ps1 Test
.\runners.ps1 TestHyperV -Config local/controller.json
if ($LASTEXITCODE) { throw 'Inspect the printed report and diagnostic log paths.' }
$report = Get-Content local/data/hyperv-validation.json -Raw | ConvertFrom-Json
Get-Content -LiteralPath $report.diagnosticLog
if (-not ($report.passed -and $report.guestValidationPassed -and $report.cleanupPassed)) {
    throw 'Full guest validation and cleanup have not both passed.'
}
```

Use `PrepareHost` only if host/group/network preparation is missing. With the
existing validated base, rerun `TestHyperV` directly; `PrepareImage` is not needed
to pick up this repair. Retain the per-run log and JSON if SQL fails again. The
diagnostics distinguish process death, insufficient resources and a published-port
failure without guessing that every timeout is slow initialization.

## PrepareImage ownership repair, October 28, 2026

The follow-up failure came from `Convert-CLCImage` in `scripts/HyperV.ps1`.
`PrepareImage` correctly wrote `build-intent.json` and chose that build's
`converted.vhdx`, but constructed a temporary conversion client with only
`Config`, `Command` and `Redactor`. `diskOperation` serialized that client's empty
`Owner`, empty `Scale` and relative `root: "vms"`. The native ownership check
correctly rejected the empty owner against the real build intent. The earlier
pipeline mock decoded only disk paths, so it missed this protocol regression.

The conversion client now retains deployment identity and paths while extending
the command timeout. The request also explicitly carries the build version.
Native validation requires a valid owner, the matching durable intent and exactly
`<imageDirectory>/<version>/converted.vhdx`. Windows full-path normalization and
ordinal case-insensitive path comparison accept equivalent separators, case and
dot segments while rejecting other builds, nested directories, traversal,
alternate streams and other disk names. Intent/version/owner checks remain strict;
the destination must not already exist and owned paths cannot be reparse points.

The preparation sequence is:

1. The existing management/controller/scale locks and image lock serialize work.
   An unchanged, validated current image can be reused.
2. Native preflight checks the host/network; durable owned image/validation VM
   records are recovered without touching job or unrelated VMs.
3. A unique version directory is allocated and its owner/version intent is
   atomically written before downloads, VHDX creation or conversion.
4. The official checksum manifest is refreshed. Cached QCOW2 data is rehashed;
   complete, owned legacy sources are migrated into the independent source cache.
5. Previous incomplete builds are reconciled against matching intents, recognized
   regular files, all registered VM disk chains and durable record references.
   Current/active/published/referenced images and the source cache are preserved.
   Unknown files or links refuse deletion. Ownership metadata remains until all
   candidate disk files are deleted, allowing partial cleanup to resume.
6. Native capacity is checked. `New-VHD` creates a dynamic, source-sized VHDX;
   `Get-VHD`, `Test-VHD` and NTFS attribute checks validate it. QEMU populates it
   using `convert -n`, and guest sectors are compared against the QCOW2. Native
   resizing grows it to the configured size and repeats validation. This retains
   the sparse-file correction for `0xC03A001A`.
7. The converted disk moves into the separately journaled builder VM directory.
   Ubuntu provisioning, sealing and clean shutdown precede VM removal and sealed
   disk promotion to `base.vhdx`. Native validation and read-only protection apply.
8. A fresh differencing clone must boot with a different machine identity and
   pass Docker smoke validation. Cleanup must succeed before the image manifest
   and `current.json` are atomically published. Failures leave the old current
   image in place; the next rebuild uses a new intent and candidate destination.

`.\runners.ps1 Test` passed **67 top-level Go tests**, vet, **19 PowerShell syntax
checks**, **16 activation scenarios / 82 assertions**, **56 activation helper
assertions**, **50 Hyper-V helper assertions**, and **88 image conversion
assertions**. Regression coverage now inspects the serialized conversion owner,
scale, resource root, build version, destination and existing intent. Fresh
preparation and successful retries after conversion/resize failures exercise the
entire mocked pipeline. Cleanup tests cover foreign/missing/mismatched intent,
registered/durable references, invalid inventory, current/published/active
versions, unknown files and directories. A native Windows file-lock test proves
partial deletion retains intent and succeeds on retry after the lock is released.
Three symlink subtests skip on this token because Windows denies symlink creation.

The real `.\runners.ps1 PrepareImage` attempt failed at **host preflight**:
the current token cannot manage Hyper-V on the validation host. The exact error is
“You do not have the required permission to complete this task.” The session is
not elevated; read-only `Get-VM` confirms the same denial. The real small-disk
`Test-ImageConversion.ps1 -Live` request passes ownership/path validation but is
also denied at `New-VHD`; its scratch directory is removed by its cleanup.
No prepared base image, builder VM or clone was created/promoted by this repair.
Hashes confirm local controller
configuration, deployment state and network metadata were unchanged.

Two old owned directories contain only intent/checksum metadata, with no disks.
They remain in place: automatic execution policy rejected direct metadata-only
cleanup as “blocked by policy” without a more specific explanation. Full retry
cleanup also requires successful Hyper-V inventory before deleting candidates.
The verified source cache remains intact. Local test and execution logs are
`artifacts/prepare-image-ownership-tests.log` and
`artifacts/prepare-image-ownership-live.log`; the storage probe is recorded in
`artifacts/prepare-image-ownership-live-storage.log` and the preserved build
inventory in `artifacts/prepare-image-ownership-recovery.json`.

Under an account/token with Hyper-V management access, refresh group membership
by signing out/in if needed, then run `scripts/Test-ImageConversion.ps1 -Live`,
`runners.ps1 PrepareImage` and `runners.ps1 TestHyperV`. If group/network setup is
missing, use the existing elevated `runners.ps1 PrepareHost` first. Actual native
conversion/resize, Ubuntu boot, sealing, clone validation and final promotion
remain unverified until those live checks succeed.

## PrepareImage VHDX repair, October 28, 2026

The first real build failed in `Resize-VHD` with `0xC03A001A`. The old pipeline
let QEMU create the dynamic VHDX and moved that file into the build VM directory.
The installed **QEMU 11.1.0 (v11.1.0-12130-ge470268ff4)** reproduces the cause:
its output has `Archive, SparseFile` NTFS attributes. [QEMU's Windows file creator](https://github.com/qemu/qemu/blob/master/block/file-win32.c)
explicitly calls `FSCTL_SET_SPARSE`; renaming the file preserves that attribute.
Hyper-V rejects it even when the guest disk contents are valid.

The repaired pipeline creates a dynamic VHDX with Hyper-V `New-VHD`, at the
QCOW2's exact virtual size with 512-byte logical sectors. QEMU populates it using
`convert -n`, without recreating it. NTFS attributes, Hyper-V format/type/size and
`Test-VHD` are checked before and after conversion. `qemu-img compare` verifies
guest sectors before `Resize-VHD` grows it to the configured size; another native
validation follows. VM creation and sealed-base promotion also check the disks.
Ubuntu builder boot, sealing, fresh clone identity and Docker smoke validation
remain mandatory promotion gates.

A native 64 MiB experiment confirmed the old QEMU creation sets `SparseFile` and
`convert -n` leaves an existing non-sparse container with only `Archive` and
identical guest sectors. That experiment used a non-sparse stream copy of a VHDX;
it does **not** establish that `New-VHD` conversion or Hyper-V boot passed.
The original failed VHDX and QCOW2 were already absent; the old pipeline deleted
the source immediately after conversion. The repaired pipeline retains verified
sources across failures/rebuilds and never reuses a partial VHDX.

`.\runners.ps1 Test` passed **65 top-level Go tests**, vet, **19 PowerShell syntax
checks**, **16 activation scenarios / 82 assertions**, **56 activation helper
assertions**, **50 Hyper-V/network/tool assertions**, and **51 image conversion
assertions**. Native Windows tests reject real sparse and compressed files even
when the content hash is unchanged. Other regressions cover post-conversion
attributes, wrong logical sizes, comparison/resize/Test-VHD failures, source
cache integrity and reuse, partial downloads/builds, unchanged current images,
preservation of runner/unrelated VMs and files, and the expected no-IPv4 CIM query
on a new adapter. Module verification and syntax checks for all **six** embedded
Bash scripts passed. Race tests were attempted but require a C compiler, which
is unavailable on PATH/CC in this session.

The real `PrepareImage` retry stopped at host preflight: **Hyper-V management
permission denied** for `Get-VM` on the validation host. The new native
`scripts/Test-ImageConversion.ps1 -Live` test also stopped at `New-VHD` with the
same denial. Hyper-V is now installed/enabled, with about **177 GiB free**, but
this session is not elevated and its current token cannot manage Hyper-V.
No live image was promoted or marked validated. No real VM was created by this
repair task. Local evidence is in `artifacts/hyperv-image-tests.log`,
`artifacts/hyperv-vhdx-probe.json`, `artifacts/hyperv-image-live-storage.log`,
`artifacts/hyperv-prepare-image-retry.log` and `local/data/host-validation.json`.

With Hyper-V access available under the controller account (refresh the logon
token after any group membership change), complete these live checks:

```powershell
.\scripts\Test-ImageConversion.ps1 -Live
.\runners.ps1 PrepareImage
.\runners.ps1 TestHyperV
```

The first proves the actual `New-VHD`/QEMU/native-resize path on a small patterned
disk. The second must boot/seal Ubuntu, boot a fresh clone and pass Docker smoke
checks before publishing `current.json`. The third exercises the full guest
platform. Nested guest boot and full application acceptance remain unverified here.

## Earlier migration observations, October 28, 2026

This migration is implemented and unit-tested, but **live Ubuntu Hyper-V boot,
Docker/SQL execution in a nested VM and the complete application CI run are outstanding**.
Mocked provisioning and image tests are not proof that this host can boot Ubuntu.

Observed host: Windows 11 Pro x64, Microsoft Hyper-V guest, 16 logical processors,
64 GiB RAM, PowerShell 7.6.6, pinned Go 1.27.3. Hyper-V's PowerShell module is
missing; this session is not elevated. WMI detects a hypervisor but cannot prove
nested extension availability. C: had approximately **5.7–6.4 GiB free** during checks, versus the
then-default minimum **115 GiB at runner creation**, plus image-build storage.
No physical-host change, Windows restart, network creation or guest VM creation
was attempted past these blockers.

Real `Install` successfully migrated local configuration and built the Windows
controller. `Status` migrated the empty journal to version 2 while preserving
the deployment owner and scale-set ID. Exact legacy
configuration/state backups were saved. The existing CurrentUser DPAPI PAT was
not replaced. A real organization authentication/runner-write check **passed**,
and read-only `setup --check` returned **`current:true`** for existing GitHub
infrastructure. No runner-group grants, scale-set identity or application files changed.

`Activate -NoStartup` fails clearly on missing Hyper-V and disk capacity.
`PrepareHost` correctly reports that elevated PowerShell is required, with the
supported command. `Status` returns stopped state plus an explicitly unavailable
Hyper-V inventory. Existing startup registration is preserved pending Hyper-V
validation; prior WSLC startup evidence is not used as Hyper-V evidence.

## Automated checks

Run in normal PowerShell 7:

```powershell
.\runners.ps1 Test
```

This runs the Go suite, vet, PowerShell parsing, ACL/configuration checks, existing
activation/dependency/startup/exit-status tests and mocked Hyper-V helper tests.
The original migration run passed **60 top-level Go tests**, **18 PowerShell syntax checks**,
**16 activation scenarios / 82 assertions**, **56 management/startup helper
assertions**, and **36 Hyper-V/network/migration assertions**. Module verification
and all embedded Bash syntax checks passed. Native Windows OpenSSH generated real
per-instance keys; their private/public pair and seed contents matched through
an independent ISO reader under the actual private-directory ACL preparation.
Coverage includes official listener ACK/replay/acquisition, scaling/capacity,
concurrent callbacks and VM allocation, boot timeout/busy fences, restart adoption,
lost JIT/create replies, partial cleanup, ownership/path checks, configuration and
state migration, DPAPI, redaction and idempotent activation/network preparation.

The image pipeline is tested end-to-end with mocked native operations/downloads:
verified source, conversion, unattended VM setup, sealing, different clone machine
identity, smoke validation, promotion, reuse and cleanup. Checksum, conversion,
sealing and clone failures preserve current. Image integrity, immutable paths and
referenced-parent pruning are tested. The NoCloud ISO also round-trips through
Windows' independent `tar` ISO reader, including exact Joliet filenames/content.
PowerShell tests verify actual command parameters: Generation 2, differencing
disk, Linux Secure Boot, static resources, NIC guards and safe deletion.

Race testing is separate:

```powershell
.\runners.ps1 TestRace
```

Windows Go race detection requires a supported C compiler on PATH or `CC`.
This host currently has no detected compiler, so no native race-detector result
is claimed. Ordinary tests exercise concurrency but do not substitute for a race
detector. No WSLC container is used for tests or provisioning.

## Real Hyper-V platform integration

Resolve storage and install Hyper-V with elevated `PrepareHost`; manually restart
if requested, rerun preparation and refresh newly granted group membership.
Normal-token `PrepareImage` builds, seals, boots a fresh clone and smoke-tests it.
Then explicitly run this **resource-intensive** path while the controller is
stopped (allow up to 45 minutes):

```powershell
.\runners.ps1 PrepareImage
.\runners.ps1 TestHyperV
```

It creates a disposable Ubuntu clone and verifies guest initialization, pinned
SSH and native Docker run/exec/build/Compose. It starts SQL Server 2022, waits for
a real query, checks the published `localhost:1433` through Ubuntu's network
namespace and a .NET 10 SqlClient program, publishes that program for win-x64,
verifies Node/npm/PowerShell/Python/Git and launches real Playwright Chromium.
The VM, disk, seed, keys and allocation must be gone before overall success.
Results go to `local/data/hyperv-validation.json`, with software and cleanup
results distinguished, and redacted per-run transcripts in
`local/logs/validation/<vm>.log`. With an existing validated base, run TestHyperV
directly: it sends the current checks over SSH without changing the base image.
This test does not pretend to dispatch a GitHub workflow.

To test ordinary runner startup/registration, run `Activate`, queue an authorized
workflow on `clc-linux`, observe a JIT runner reach listening/job-started, and
verify its removal after job completion. `samples/platform-validation.yml`
contains separate Docker-action and service-container jobs for an approved
repository. Installing/running that workflow requires the repository owner's
normal authorization; it has not been pushed or run by this task.

Verify with real workloads: two queued jobs at maximum one; zero VMs after idle;
failed/cancelled jobs; kill/restart only the controller during an active job;
repeat Activate unchanged; exercise owned partial-create recovery. Confirm the
same scale-set ID and unchanged group permissions afterward. Increase maximum
only when CPU/RAM/disk budgets allow, then verify unique IP/MAC/machine identities.

## Application workflow acceptance gate

The reference application workflow, global.json and package.json were inspected
through GitHub. That workload's source and workflow are not distributed here.
It requests .NET **10.0.303**, Node 24, SQL Server 2022, Playwright **1.62.1**,
real SQL/.NET tests, frontend/browser accessibility, PowerShell, win-x64 publishing,
ZIP validation and artifact upload. All must pass; a version/checkout probe is
insufficient.

After host/image/platform tests pass, use an approved validation repository and
branch. Change only the chosen job's `runs-on` from `ubuntu-latest` to `clc-linux`
and run the full existing workflow with deployment disabled. Preserve its tests,
scripts, credentials, service ports and any separate deployment jobs.
Record the Actions run URL, exact commit, image ID, all job results, artifact
identity and controller cleanup evidence. Until that complete run succeeds,
application compatibility remains an acceptance target, not a verified result.

## Unattended acceptance

After platform testing, run `TestUnattended`, inspect failed registration/execution
stages and run `EnableStartup` for a passing mode. Validate actual logout and
cold boot separately, including DPAPI, Hyper-V/file access, NAT, listener recovery
and absence of orphaned owned disks. Do not infer cold-boot readiness from a
same-session probe or hide S4U Access Denied with Interactive-mode success.

## Graceful controller restart

Implemented following the [pre-change audit](restart-audit.md). See
[operations](operations.md#automatic-graceful-restart) for the first upgrade,
prepared updates, validation blockers, diagnostics and recovery commands.

Automated verification performed:

* Pinned Go: `tools/go/bin/go.exe test -count=1 -cover ./...` passed across all
  packages. Controller coverage was 86.1%, Jobs 94.3%, restart 75.8% and CLI 32.2%.
  The restart package was rerun after the final dependency-identity hardening.
  `tools/go/bin/go.exe vet ./...` passed.
* `scripts/Test-Scripts.ps1` parses all 23 PowerShell scripts and runs the existing
  ACL/configuration, activation, Jobs, dependency/fingerprint/lock/startup,
  Hyper-V helper and image conversion tests, plus restart helper tests.
* Go regression cases include stable/no-op identities, executable/config/image/
  runtime changes, ignored source/log/state files, partial/invalid/rapid edits,
  same-mtime detection, equivalent receipts/candidate paths, stable invalid-tool
  blockers, superseding/reverted requests, busy/idle manual requests, repeated
  requests, failed-identity suppression, interrupted drain/handoff recovery and
  PID creation-time startup gating.
* Controller tests exercise zero/one/three assignments, different completion
  orders, accepted work without a VM, guest boot/JIT delivery, registration,
  cancellation, cleanup/API/inventory failures, lost acquisition responses and a
  simultaneous acquisition/drain transition. The official listener test proves
  zero advertised capacity, declined offers and normal ACKs during draining.
  Existing cleanup, guest, resource admission and job visibility tests still pass.
* Real Windows process tests launch a PowerShell helper from disposable parents
  under ordinary hosting, a breakaway-enabled kill-on-close Job Object, and a
  Job Object prohibiting native breakaway. The helper survives parent exit and
  job closure. This host retained an outer Job Object after native breakaway, so
  the successful independent path used the WMI broker. A completely job-free
  native-success host remains a separate environment-specific check.
* `Test-Restart.ps1` builds a disposable controller executable with stand-in
  GitHub/Hyper-V validators in a unique temporary installation/scale identity.
  The actual pinned helper and production detached launcher perform a complete
  unchanged/manual handoff and a staged binary/configuration handoff. The latter
  includes an intentional unexpected original-process exit after quiescent
  commit. Replacement nonce, effective hashes, listening readiness, atomic
  backup preservation and use of the existing-task dispatch path are checked.
  The existing-task API is stubbed; it never registers or changes a real task.
  Additional cases cover duplicate lease rejection, nonce/PID/start mismatch,
  original exit timeout, invalid configuration/metadata, failed readiness,
  immediate replacement startup failure, ordinary-start recovery and repair
  without rotating corrupt bytes over the recoverable previous executable.
* Jobs schema v1/status properties remain compatible; restart properties are
  additive. CLI dispatch tests cover RestartWhenIdle, default Jobs and Jobs -Json.
  Live activation of a restart-capable busy controller stages instead of stopping;
  a failed candidate test preserves the original. Staging tests enforce test/vet
  before build and preserve the active executable on failure.

Go's race detector was unavailable: the pinned toolchain reports
`CGO_ENABLED=0` and no supported C compiler was found. Deterministic concurrent
acquisition/drain and existing reconciliation tests ran without that detector.

Read-only live Status/Jobs checks were made before changes and again after tests.
Initially the old controller had two busy runners and three assigned jobs;
the later check still had one busy runner/one assigned job. The process identity,
original nonce and executable remained unchanged. Configuration remained maximum
three, two CPUs, 10 GiB static RAM and 100 GiB logical disk. The current terminal
could not inventory Hyper-V VMs because its token lacks permission. No live
controller stop/restart/activation, configuration edit, image build or workflow
dispatch was performed. Local credentials, production runtime state and image
evidence were preserved.

Still required on the actual controller account/token in a safe maintenance
window: the first upgrade from the older controller; a real GitHub job continuing
through automatic/manual draining; additional work remaining queued; completion,
deregistration and VM cleanup; updated configuration loading and queued-work
resumption; and real scheduled S4U/sign-out/cold-boot recovery. The isolated
handoff proves independent process survival and installation/readiness behavior,
not those outstanding live acceptance tests. Binary, hardware, image or probe
changes that invalidate TestUnattended remain explicitly blocked until validated.
