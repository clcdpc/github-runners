# Preparation failure repair — October 29, 2026

Run `20261029-154037-032b6c6f` passed SQL readiness, then failed during physical file verification. The defect was a filename case mismatch between SQL's catalog and the Linux filesystem, not a SQL startup delay.

## Exact sequence and evidence

After `SELECT 1` succeeds, preparation:

1. Inspects the container's `/var/opt/mssql` bind mount.
2. Queries `sys.master_files` and requires tempdb data/log entries.
3. Checks each mapped guest file and its backing device.
4. Queries SQL version, .NET SDK version and Docker version.
5. Requires SQL Server major version 16 and no user databases.
6. Verifies default data/log paths, creates `ClcPlacementProbe`, verifies its files and drops it.
7. Removes the SQL container, unmounts the disk, shuts down the VM and prepares the immutable baselines.

The original archive discarded subprocess details. A fresh isolated Hyper-V trace reproduced the error and captured:

```text
Stage: verify_sql_files.file_exists
Command: sudo test -f /mnt/clc-disk-benchmark/mssql/data/MSDBData.mdf
Exit code: 1
stdout: empty
stderr: empty
```

Evidence: `artifacts/hyperv-disk/preparation-trace-20261029/guest-failure.tar.gz`, member `results/preparation/failure.json`. Its traceback connects `main` → `verify_files` → `command` → `subprocess.run`. The container was running without OOM and its bind mount was correct.

SQL Server `16.0.4305.5` reports `MSDBData.mdf` and `MSDBLog.ldf` in `sys.master_files`. The actual files are `msdbdata.mdf` and `msdblog.ldf`. A case-sensitive Linux `test -f` against the catalog spelling fails even though SQL is using the real files successfully. The same mismatch was independently reproduced inside the exact SQL image.

## Correction and diagnostics

The benchmark resolves each component of the catalog-relative path against actual directory entries, requiring exactly one case-insensitive match. It requires a regular file, rejects ambiguity and paths/symlinks escaping the benchmark mount, and runs the existing device check against the resolved path. It does not rename SQL files or skip msdb, data, log or tempdb checks. No retries or timeout increases were introduced.

`sql-file-placement.json` and successful `result.json` retain both the catalog spelling and resolved guest path. `stages.jsonl` records preparation progress. A failed subprocess now preserves:

- Preparation stage and sanitized command identity.
- Exit code or timeout status, stdout and stderr.
- A traceback containing source frames without locals or the exception's unsafe raw command representation.
- SQL/Docker state and logs through the existing diagnostic archive and cleanup path.

Password arguments, SQL/script payloads, known credentials and connection strings are redacted; stdin and environment dictionaries are not exported. The original failure record survives a later diagnostic-collection failure, and owned-container cleanup still runs.

## Complete preparation validation

`artifacts/hyperv-disk/complete-preparation-20261029/execution.json` records a successful independent **full** `-Action Prepare` run, including the application build and SDK **10.0.303**. SQL Server 2022 initialized with Docker **29.9.2**, all database files were verified on **`/dev/sdb`**, and the default-path probe database was created, checked and deleted. The preparation VM, disks, checkpoint artifacts, seed and keys were removed.

The successful result explicitly records:

| SQL catalog path | Resolved guest file |
| --- | --- |
| `/var/opt/mssql/data/MSDBData.mdf` | `/mnt/clc-disk-benchmark/mssql/data/msdbdata.mdf` |
| `/var/opt/mssql/data/MSDBLog.ldf` | `/mnt/clc-disk-benchmark/mssql/data/msdblog.ldf` |

After this validation passed, the approved sequence completed all nine A/B/C repetitions, with 27 passing selected tests and successful cleanup. Medians were 119.246 / 116.012 / 135.337 seconds for migration tests, 12.986 / 10.553 / 11.660 seconds for preconditioned SQL, and 7.809 / 39.762 / 4.946 seconds for shutdown/reset, in A/B/C order. See the [measured comparison and decision](REPORT.md).

Regression checks passed: 23 offline tests and 17 existing ownership/cleanup assertions. Coverage includes catalog/actual filename case, missing/ambiguous files, mount-escape rejection, sanitized command/exit/streams/traceback, and preservation of the original subprocess failure when later log collection fails. Complete live Hyper-V preparation and all nine repetitions provide the end-to-end validation.

## Rerun

```powershell
# Full preparation only: builds the workload and verifies SQL, files and SDK; no A/B/C runs.
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action Prepare

# Existing balanced comparison, with its preparation gate and owned cleanup.
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action Run
```

Both require Hyper-V management rights. The active controller, GitHub configuration, production images and unrelated VMs are unchanged by this repair.
