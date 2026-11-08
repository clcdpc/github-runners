# Hyper-V SQL disk screening benchmark

See [REPORT.md](REPORT.md) for the completed October 29 comparison: **nine successful repetitions, 27 passing selected tests, and cleanup complete**. Direct fixed storage saved 2.7% on the migration command and increased shutdown/reset cost; the recommendation is to leave the runner architecture unchanged. Earlier SQL-readiness findings remain in [SQL-READINESS.md](SQL-READINESS.md).

The subsequent filesystem-verification failure is corrected as well: [preparation investigation](PREPARATION-FAILURE.md) identifies the exact failed command, catalog/filesystem case mismatch, diagnostic schema, and successful full Hyper-V preparation validation.

This standalone harness reads the existing runner configuration and image. It never invokes controller activation, changes the runner scale set, registers a GitHub runner, changes the application workload, or runs the full suite. It uses the existing NoCloud ISO writer and PowerShell path/ACL helpers.

## Requirements

- Windows PowerShell **7.4+**, with an enabled Hyper-V Administrators token or an elevated administrator token. Use PowerShell 7 under the controller account with Hyper-V management access; changing controller startup is unnecessary.
- Existing validated Ubuntu 24.04 runner image and dedicated CLC switch/NAT. No image rebuild or boot-disk conversion is performed.
- Four spare vCPUs, 16 GiB RAM plus 8 GiB Windows reserve, native OpenSSH, Git/tar, the repository's pinned Go installation, and host Python 3.10+.
- Idle controller journal and no active CLC runner VMs. The controller stays running; the harness checks again during guest commands and aborts if a real runner appears. Concurrent allocations can still race a check.
- A compatible local application workload checkout containing `tests/Asap.Tests/Asap.Tests.csproj`, the selected migration tests below and the existing fixture/performance instrumentation used by `measure.py`. That external workload source is not distributed here. The harness's historical source path/revision defaults and workload identifiers remain for compatibility; supply `-AsapRepository` and `-Commit` for your authorized checkout. The controller and its unit tests do not require that source. Results from a different revision are not a reproduction of the historical measurements.
- Space for two fixed 40-GiB disks, a private copy of the existing boot parent, 20 GiB bounded boot-overlay growth, 5 GiB SQL differencing-disk growth, and a 15-GiB free reserve. Approximately **126.4 GiB free** with the current 6.37-GiB parent. Budgets are checked every five seconds during guest commands. This bounded experiment does not reserve two complete 100-GiB boot-disk growth potentials as a permanent pool would.

Run from the `github-runners` checkout. Select an exact revision in your compatible local workload checkout before running preflight. Replace the example path with your checkout:

```powershell
$workloadRepository = 'C:\Workloads\migration-benchmark'
$workloadCommit = git -C $workloadRepository rev-parse --verify 'HEAD^{commit}'
if ($LASTEXITCODE) { throw 'Select a compatible local workload checkout.' }
$benchmarkSource = @{ AsapRepository = $workloadRepository; Commit = $workloadCommit.Trim() }
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action Check @benchmarkSource
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action ValidateSql @benchmarkSource
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action Prepare @benchmarkSource
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action Run @benchmarkSource
```

`Run` prepares the VM and builds the test project and its references once, then executes nine repetitions. It downloads the SQL image once and records its immutable digest; all repetitions use that digest. An absent requested .NET SDK is installed inside the benchmark VM before building. Preparation/network/download/build failures remain failures, with available logs retained.

`Prepare` executes that complete preparation and build, validates SQL/file placement and the SDK, then cleans up without starting any A/B/C workload. Use it to isolate preparation failures. Resolved guest filenames are checked against the benchmark disk even when SQL's catalog preserves a different case, such as `MSDBData.mdf` versus `msdbdata.mdf`.

`ValidateSql` prepares only the isolated VM, SQL disk and pinned SQL container. It does **not** download the source, install an SDK, build the application workload, or run migration/A/B/C workloads. It requires a real `SELECT 1`, SQL Server major version 16, physical/default file and tempdb placement checks, and creation/deletion of a probe database. Its existing preflight still requires the local source revision for provenance checks; ownership and automatic cleanup remain in force. Complete this check before launching the full comparison in an elevated shell. A successful SQL-only run has zero A/B/C repetitions and cannot support a storage-performance conclusion.

Readiness uses `/opt/mssql-tools18/bin/sqlcmd`, `-S localhost -U sa -C -d master`, short login/query timeouts and `-Q` with a real `SELECT 1`. The password travels only through binary stdin into the container's `SQLCMDPASSWORD`. Scalar readiness uses `-h -1`; JSON commands use `-y 0` without `-h` or `-W`. The incompatible header/width combination caused the original false timeout.

Each preparation/repetition records sanitized `sql-probe.jsonl` (stdout/stderr, exit code, timeout, attempt and duration), `sql-ready.json`, allow-listed `sql-state.json`, `sql-container.log`, `sql-error.log`, `sql-diagnostics.json`, and `sql-cleanup.json`. Failed queries/failures also produce `sql-command-failures.jsonl`/`failure.json`. These files are preserved under `results/` in `guest-failure.tar.gz` before VM cleanup. No probe input, full environment, connection string or password is archived; Docker state excludes health-check output.

`stages.jsonl` records the preparation sequence; `failure.json` now includes the failing stage, sanitized argv, exit/timeout, stdout/stderr and a traceback without locals. `sql-file-placement.json` records catalog names and resolved guest files. Missing or ambiguous files and paths escaping the benchmark mount fail validation rather than being ignored.

The default VM name is exactly `clc-disk-benchmark`. Its dedicated resources live in `local/benchmarks/hyperv-disk`, alongside existing runner storage. It uses `192.168.231.254`, outside the controller's `.10`–`.250` pool, after checking existing NIC addresses and ping. For another existing /24 configuration, supply an unused `-BenchmarkAddress` outside its configured allocation range. The harness does not change networking.

Useful explicit parameters:

```powershell
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action Run `
  -Config local/controller.json @benchmarkSource `
  -OutputDirectory artifacts/hyperv-disk/my-fresh-run -Repetitions 3

.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action Report `
  -OutputDirectory artifacts/hyperv-disk/my-fresh-run
```

Use a **fresh** output directory for each Check/Run; prior results are never reused. Output defaults to a unique directory under ignored `artifacts/hyperv-disk`. Preparation is outside SQL/test timing. The three tests took a combined 101 seconds in the historical CI run; nine subset repetitions should avoid another full 58-minute SQL suite, although download/build and host conditions affect overall time.

## Disk and workload method

| Scenario | SQL disk | Recurring reset measured |
| --- | --- | --- |
| A | Writable differencing child of a prepared 40-GiB fixed parent | Recreate boot/SQL children |
| B | Writable copy of that exact fixed parent, no checkpoint | Recreate boot child and copy the entire 40-GiB SQL baseline |
| C | Writable fixed copy with a real **offline standard VM checkpoint** | Actual `Restore-VMSnapshot`, restoring both attached disks |

All scenarios use the same private boot baseline. Each measurement starts on a fresh boot differencing child. C adds a checkpoint layer to **both** SQL and boot disks; it does not isolate SQL overhead perfectly. C's initial fixed copy/checkpoint creation and final merge/deletion are reported separately from recurring checkpoint restoration. There is no saved guest memory in the offline checkpoint.

The fixed SQL baseline is fully allocated before timing; ext4 inode/journal initialization is completed synchronously with discard disabled, and the guest's periodic trim timer is disabled. SQL initializes its system databases, proves the default data/log placement with a disposable database, then shuts down cleanly. A bind mount maps `/mnt/clc-disk-benchmark/mssql` to `/var/opt/mssql`. The guest checks SQL's physical file paths, corresponding host files, default database paths, both tempdb data/log files, and the mount's device, distinct from the boot disk. It performs no raw disk writes until it identifies exactly one unmounted 40-GiB whole disk.

The prepared OS and SQL baseline are immutable. Fresh boot children and restored/copied SQL contents prevent Docker containers, test output, user databases or source changes from carrying over. SQL is started afresh for each repetition. The runner service is masked; no JIT or GitHub credentials are supplied.

Orders are A/B/C, B/C/A, C/A/B. For each repetition:

1. Restore clean disks; start the guest and SQL outside timing.
2. Run a separately timed first-use SQL workload: create 128-MiB data/64-MiB log files, commit 2,000 transactions with 8,000-byte rows and delayed durability disabled, write tempdb, checkpoint, capture file I/O **before deletion**, and drop the database.
3. Run exactly these three migration tests with `--no-build --no-restore`, reusing the workload's CI performance and fixture instrumentation:
   - `ImportUsesSourceStaffEmailAndReconcilesFreshSqlTargetWithoutIdentityMap`
   - `ImportMapsFormatAndFoundRequestWithCanonicalTag`
   - `ImportPreservesAdditionalCopyHistoryAndUsesCreatedWhenUpdatedIsMissing`
4. Repeat the focused SQL workload after preconditioning, separately from the tests; require no remaining user databases.
5. Copy diagnostics, gracefully shut down, measure restoration, and remove that repetition's disks.

Report medians, min/max, population stdev, absolute/percentage differences, individual tests, instrumented DACPAC times, first-use/preconditioned SQL, wait deltas, SQL/guest file latency, CPU/I/O pressure, disk growth and restoration/startup costs. The application workload fixture timer covers its central DACPAC helper, not every direct deployment or migration subprocess. File I/O sampling can miss short-lived databases; focused SQL counters avoid that loss. No caches on the outer host are forcibly flushed.

Exactly three passing TRX results, successful fixture instrumentation, a stable SQL digest, and no SQL restarts/OOM are required. A failed, skipped, missing or unexpectedly broad test selection cannot count as a fast repetition. A range/median above 20% flags inconsistency; inspect the raw evidence before increasing `-Repetitions`, which accepts 3–9. The declared initial materiality screen is a ≥15% and ≥5-second migration-test saving. Favorable screening results require focused full-VM validation before a controller redesign.

## Results and cleanup

Each output directory contains `comparison.md`, `preflight.json`, `execution.json`, per-repetition `result.json`/`host.json`, TRX, fixture/SQL/guest diagnostics and host VHDX growth samples. Available host storage counters are discovered and matched to this VM; missing latency counters are explicitly unavailable. Raw logs stay in ignored local artifacts and are not suitable for indiscriminate publication.

Run automatically removes the benchmark-owned VM, checkpoints, disks, ISO and SSH keys in `finally`. To recover an interrupted run:

```powershell
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action Cleanup
```

If a custom `-ResourceDirectory` was used, supply the identical absolute directory. Cleanup requires its durable GUID manifest, exact VM ID, name, notes marker, owned disk chains and configuration directory. It rejects foreign references (including checkpoints) and reparse points. It never deletes by a name prefix, changes production image ACLs, or modifies the controller journal. Missing ownership/permissions preserve resources and produce an explicit error.

Checks that do not require Hyper-V access:

```powershell
.\benchmarks\hyperv-disk\Test-Harness.ps1
python -B -m unittest discover -s benchmarks/hyperv-disk -p 'test_*.py' -v
.\tools\go\bin\go.exe test ./benchmarks/hyperv-disk/seed ./internal/seed
```

For an independent SQL client/container regression on an available Linux Docker daemon, the opt-in live test uses the locally cached `2022-latest` image, resolves its digest, and creates a UUID-labelled container and named volume. It publishes no host port, caps CPU/RAM, removes only its exact owned resources, scans diagnostics for the generated secret, and writes `diagnostics.tar.gz`. It does not validate Hyper-V disk placement or performance:

```powershell
$env:CLC_BENCHMARK_LIVE_SQL = '1'
try {
  python -B -m unittest discover -s benchmarks/hyperv-disk -p 'test_sql_readiness.py' -v
} finally {
  Remove-Item Env:CLC_BENCHMARK_LIVE_SQL -ErrorAction SilentlyContinue
}
```
