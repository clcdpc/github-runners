# Runner failure diagnostics

Before stopping/deleting a runner VM, cleanup captures independent, private,
redacted evidence under the configured log directory's `runners/<vm>/` folder.
This runs for every cleanup, including failures. It does not change updates,
runner timeouts, service restart policy, firewall rules, audit rules or permissions.

## Files and limits

| File | Evidence | Maximum retained output |
| --- | --- | --- |
| `trace-state.log` | Continuous numeric PID 1 decisions and unit state | 2 MiB |
| `trace-bus.log` | Public-bus requests, authenticated peer metadata, replies and jobs | 2 MiB |
| `trace-kernel.log` | Process ancestry, signal generation, probe health/loss | 4 MiB |
| `service-state.log` | Allowlisted systemd state, exit status, timestamps and dependencies | 128 KiB |
| `services.log` | Current-boot runner, cloud-final, Docker, containerd, observer and oomd journal | 512 KiB |
| `kernel.log` | Current-boot kernel/OOM journal | 512 KiB |
| `auth.log` | SSH, sudo, logind and repository service-control journal | 512 KiB |
| `packages.log` | apt-daily, apt-daily-upgrade and unattended-upgrades journal | 512 KiB |
| `dpkg-files.log` | Current and previous dpkg log | 512 KiB |
| `apt-files.log` | Current apt history/terminal logs and previous `.1.gz` logs | 512 KiB |
| `unattended-files.log` | Current/previous unattended-upgrade and dpkg logs, including gzip | 512 KiB |
| `runner.log` | Newest two standard `Runner_*.log` files | 1 MiB |
| `worker.log` | Newest two standard `Worker_*.log` files | 1 MiB |
| `boot.log` | All current-boot journal sources | 2 MiB |
| `collection.json` | UTC capture time, per-source byte counts, bounds, file-updated flags, truncation and capture errors | Small metadata |

The output budget totals 15.625 MiB plus omission markers and metadata per runner.
Existing configured archive retention still applies. Each source has an independent
budget, so noisy boot output cannot evict Worker or authentication evidence.
Journal timestamps are UTC. Files retain their original timestamps/time zone.
Old rotated files are read before current files so tail retention favors current
records. Previous compressed rotations beyond the listed files are not collected.

Collection has a 90-second overall deadline, a 15-second host deadline per source,
and a 10-second remote process-group timeout. Cleanup records unavailable or timed
out sources and preserves their partial output, then proceeds to resource removal.
Failure to write evidence on the host still prevents removal. If a retry produces
no new output, an older source file is preserved; `fileUpdated: false` means that
file is **not** evidence captured by the latest attempt. An absent/off/unreachable
VM cannot supply missing logs retroactively.

A boot journal query has no 1,000-line filter, but the saved file is a **bounded
sanitized tail**, not a guarantee of complete boot history. Earlier journal rotation,
source failures, byte/time limits, redaction and missing logs can remove evidence.
A truncation marker and collection report make capture limits visible.

## Redaction and read boundaries

All stdout/stderr is streamed through the diagnostic redactor **before** retention
limits or host writes. Native write boundaries, partial SSH failures and known
credentials split across chunks are covered. Incomplete final transport records are
discarded after failure. Structured payloads (including Worker job JSON), PEM key
blocks, credential-bearing lines, sensitive URLs and ambiguous multiline credentials
are conservatively omitted. Oversized records stop retention of that source rather
than exposing a later fragment. These choices intentionally trade some detail for
privacy. The existing controller/image progress logging path is unchanged.

The collector only opens fixed system log paths and strictly named Runner/Worker
logs. Every directory component and file is opened without following symlinks;
nonregular and multiply-linked files are refused. It never walks a job workspace,
reads runner credential files, dumps environment/process arguments, or requests
systemd Environment/ExecStart properties. Raw data is not written to temporary files.
File scans are bounded at 32 MiB (including decompressed data), directory selection
at 1,024 entries, and file records at 64 KiB. Oversized raw files and incomplete file
records are omitted instead of cutting through credential fragments.

No heuristic can recognize every arbitrary, unlabeled secret a workload prints.
Treat these as private operational artifacts and review them before external sharing;
redaction is not permission to publish logs.

## Layered attribution and its limits

Future images install the official Ubuntu `bpftrace`, `python3-dbus` and
`python3-gi` packages and enable three independent observers. They are weak
`Wants` of the runner, never `Requires`, `PartOf` or `BindsTo`. A runner stop does
not stop its observers. Startup ordering establishes launch order, not readiness:
each layer reports its own ready state and startup/stop gaps.

- **Public system bus:** a passive monitor watches only selected service-control
  methods, logind power requests and lifecycle metadata. A separate connection
  obtains authenticated sender PID/UID and subscribes to systemd job signals.
  Request identity includes observer session, unique sender's numeric components
  and request serial. Accepted replies can identify the returned job ID; several
  requests may legitimately map to one merged job. Subscription readiness,
  disconnects, owner changes, credential races and pending-map eviction/expiry
  are explicit. Unmatched reply bodies, arbitrary error text, transient-unit
  properties and environment/command values are never persisted.
- **Kernel:** bounded raw tracepoints observe fork/exec/exit ancestry and signal
  generation independently of service-control wrappers. Records contain numeric
  PID/TGID/parent/start-time/UID/GID/cgroup fields and fixed program-class enums.
  No filenames, argv, environment or stack data are emitted. A signal record's
  context is where the kernel generated the signal, not necessarily its original
  human requester; generation does not prove delivery, termination or a restart.
  Program classes compare a short `comm` to a fixed list, so they are hints and
  can be spoofed, not verified executable identities.
- **PID 1 decisions:** a continuous journal reader requests a strict structured
  field allowlist, excluding `MESSAGE` and `COMMAND`. Records require journal
  `_PID=1` and known subject units. It distinguishes automatic restart scheduling,
  service failure results (including watchdog/timeout/OOM), process exit, and job
  outcomes. A separate five-second property snapshot retains current policy,
  restart counts, exit state, invocation IDs and transition timestamps.

`service-control.py` still provides request/result records for repository-owned
runner starts and image-sealing stops. Those fixed-source events contain no argv
or arbitrary reason strings. A successful nonblocking start means only that the
request was accepted, not that the service reached running state.

### What this can and cannot establish

A public-bus request with verified credentials and a returned job ID establishes
that the connection requested that operation and systemd accepted it into that
job. It does not prove the request was the sole cause: jobs can merge and
transactions can propagate through dependencies. An explicit PID 1 automatic-
restart record is stronger evidence of an internal restart than timing alone.

**Root systemctl can use `/run/systemd/private`, bypassing the public bus.** The
kernel trace can preserve its process/parent context and subsequent signals,
but cannot prove its exact unit/action request. Arbitrary private-bus clients,
complete transaction dependency causes, socket inheritance, hard VM resets,
host power loss and root tampering remain gaps. A process near a stop is only a
correlation. There is no universal attribution guarantee.

Closing the private-bus semantic gap requires exact-build PID 1 probes validated
against matching ELF/DWARF, or a maintained systemd source patch. Generic getter
uprobes, guessed v255 offsets, global debug logging, core dumps and audit argv
capture are not enabled by this patch. They would introduce misleading coverage
or unacceptable secret exposure without the required validation.

### Bounds, privacy and health

Guest trace files are root-private (directory 0700, files 0600), with a 2 MiB
current file plus one rotated file per observer: at most 12 MiB across all three.
Every persisted record passes the shared numeric-only sink. Arbitrary strings,
objects, unknown events and oversized integers are rejected, not redacted later.
Event types and numeric enum dictionaries are defined beside each observer; unit
codes are **source-specific** and must not be joined by number alone.

The sink admits at most 2,000 ordinary records/second per source and reserves a
separate 512/second budget for signals and health. Kernel-side budgets independently
limit lifecycle events to 256 and signals to 128 per CPU/second. CPU-keyed maps
support at most 4,096 CPU IDs and never grow with process count. Profile sampling
reports cumulative counters. Perf-buffer loss, queue overflow, parser rejection,
rate loss, stale/reset CPU counters, file rotation and disk-write failure are
visible. A healthy observer is not proof that no events were lost.

Systemd units bound userspace memory to 384 MiB for kernel tracing and 64 MiB each
for bus/state, with CPU quotas of 20%, 5% and 5% of one CPU. These are ceilings,
not measured typical overhead. Kernel probe overhead is not fully governed by
userspace CPU quota. Core dumps are disabled; persistent service filesystem writes are
restricted to the dedicated log directory. No security protections, lockdown,
updates, service restart policies or workload timeouts are weakened. Unsupported
BTF, helpers, tracing permissions, or optional packages produce degraded coverage;
the runner remains usable.

Kernel event timestamps explicitly use monotonic time. Kernel process start
identities are CLOCK_BOOTTIME nanoseconds; `/proc` snapshots and public-bus PID
augmentation retain coarse start ticks and clock frequency. Do not subtract these
process-identity fields from event timestamps or silently treat coarse ticks as
exact nanosecond identities. The logger adds boot/session IDs, UTC and monotonic
capture times, sequence numbers and sink loss counters.

A small initial `/proc` snapshot records preexisting numeric ancestry and fixed
program classes only. It never reads command lines or environments, and races or
bounds are reported. Old trace files are read before current ones during archive
capture, and trace sources are prioritized before the broader legacy log scan.

The helper is supplied to new clones, while continuous observers require an
image built from these new provisioning inputs. Existing running guests are not
modified by collection. Deployment and image rebuild require separate approval.

## Verification

`runners.ps1 Test` runs Go/PowerShell checks. The Go suite also invokes the isolated
Python collector/helper/observer tests when Python is available. They mock Linux operations
and never read the real journal, control services or touch production configuration.
Tests cover chunk boundaries, prefixed multiline records, PEM/Worker payloads,
URL/CLI credentials, output limits, partial failures, safe file flags, rotation,
gzip expansion limits, authenticated caller races, merged jobs, subscriptions,
loss accounting, metadata schema rejection and logger failure. An end-to-end
archive test ensures numeric trace records survive ordinary redaction while a
forged `CLC_TRACE` prefix cannot exempt secret-bearing content.

Before deployment, validate collection and helper events in a disposable Ubuntu
24.04 guest with benign test secrets, concurrent log writes, failed source commands
and an intentional service request. Unit/mock tests are not live systemd or Hyper-V
validation. No deployment or live runner changes are part of this patch.

### Validation performed for this change

- Windows isolated Python tests and the existing Go/PowerShell checks exercise
  parsers, resource bounds, privacy canaries and failure paths.
- A disposable **non-privileged Ubuntu 24.04 container** validated the packaged
  Python D-Bus/GLib dependencies and systemd 255 unit syntax. It did not run PID 1
  as systemd and did not exercise actual systemd service jobs.
- The guarded `diagnostics/test_bus_live.py` test runs a real private D-Bus daemon,
  a deliberately fake systemd Manager, and the real observer. It verifies
  Subscribe, authenticated caller identity, accepted request/reply/job matching,
  lifecycle signals, secret exclusion and graceful shutdown without touching
  the host system bus or controlling real services.
- Noble bpftrace 0.20.2 compiler validation and source review drove fixes to
  string-field handling, count aggregation, explicit clocks and loss parsing. Full production-script
  compilation/attachment remains **unverified** because helper feature probing is
  unavailable in the restricted container. A parser/typecheck fixture substituting
  only the cgroup helper passed; that is not a successful production BPF test.

### Required disposable-VM qualification before rollout

Use an Ubuntu 24.04 guest with the exact intended kernel/systemd packages and
normal security policy. Do not disable lockdown or grant extra privileges just to
make a check pass. Record exact versions, all three ready states, subscription
status, lost-event counters and the full result of each scenario:

1. Ordinary and absolute-path root systemctl, sudo, direct public and private bus
   calls, stop/start pairs, try-restart and reload-or-restart. Explicitly preserve
   the documented private-socket semantic gap.
2. Concurrent callers, coalesced jobs, rejected requests, sender disconnect,
   manager owner replacement/reexec and an intentionally short-lived caller.
3. Dependency/target stop propagation, automatic Restart= policy, clean exit,
   signal failure, watchdog/start-stop timeout, kernel OOM and systemd-oomd.
4. SIGTERM/SIGKILL through ordinary kill, group/thread/pidfd paths; distinguish
   kernel execution context, generated signals and observed process exit.
5. PID reuse, multithreaded exec, namespaces/container descendants and preexisting
   processes. Never join a PID without boot/start identity and namespace context.
6. Observer crash/restart, missing BTF/permissions/helpers, queue/rate pressure,
   CPU-counter reset/staleness, rotation and full disk. Coverage must degrade
   visibly and the runner must keep operating.
7. Canary secrets in argv, environment, unrelated D-Bus bodies and exception
   paths. Assert absence from **every** persisted trace and collected artifact.

Passing mocks, the private-bus integration fixture or unit syntax checks does not
satisfy this VM qualification. Keep the PR in draft until those deployment-specific
checks are intentionally performed and the remaining gaps are accepted.
