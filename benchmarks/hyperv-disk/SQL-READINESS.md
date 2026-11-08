# SQL readiness repair — October 29, 2026

The readiness failure was corrected and independently validated against the **same SQL image digest** used by the failed Hyper-V benchmark. This report records that earlier repair; the later full A/B/C results are in [REPORT.md](REPORT.md).

## Cause and comparison with the reference CI workflow

The elevated run in `artifacts/hyperv-disk/20261029-150328-b01f0041` successfully built the application workload and pulled SQL. Its SQL logs show client readiness at **19:05:58 UTC** and completed recovery at **19:06:01 UTC**. The benchmark nevertheless reported a readiness timeout.

Its sqlcmd command combined **`-h -1` and `-y 0`**. Running those flags in the matching Linux SQL image reproduces:

```text
Sqlcmd: The -h and the -y 0 options are mutually exclusive.
```

The client returns exit code **1 before attempting a login**. The old retry loop discarded stderr and interpreted every client error as server startup delay. Its “180 seconds” was also just 90 sleeps; it did not bound the duration of the SQL client operations themselves.

The reference application workflow uses `/opt/mssql-tools18/bin/sqlcmd`, localhost, the SA credentials, trusted server certificate, and `-Q` executing `SELECT 1`. The benchmark's path, target and certificate-trust choice were already correct. The incompatible output flags caused the original failure. The external workflow source is not included here.

The corrected command keeps that connection setup. Readiness has short login/query timeouts, an actual monotonic 180-second deadline, and requires both exit code zero and the returned scalar **1**. It fails immediately on invalid options or a missing client. JSON queries retain `-y 0` and omit `-h`/`-W`.

Passwords continue to travel through stdin into `SQLCMDPASSWORD`, never through `-P`, shell interpolation or recorded arguments. Independent Windows-to-Linux validation also identified text-mode CRLF translation corrupting the password line; binary stdin now preserves LF on both platforms. This was a validation transport issue, separate from the original Linux guest's invalid flags.

Validation found a follow-on SQL initialization issue: direct `FOR JSON` returned no sqlcmd output for an empty database list. Metadata queries now use an explicit `nvarchar(max)` scalar/`COALESCE` wrapper, following the workload's DMV collector. Empty database lists parse as `[]`; long JSON is returned as one document without silent truncation or streamed-row boundaries.

## Independent validation results

The opt-in live regression ran on Docker Desktop's Linux daemon, with a new UUID-owned container and named volume, no published ports, four CPUs and a 4-GiB memory cap. It did not modify any Hyper-V VM or production controller.

| Check | Result |
| --- | --- |
| Image | `mcr.microsoft.com/mssql/server@sha256:cf8f0ab2ebe61feec2eed2629061a07d247f791c2a8ddda74a4ac9f06a291718` |
| SQL version | SQL Server 2022, `16.0.4305.5` |
| Real `SELECT 1` | Passed; exit 0, returned 1, empty stderr |
| Cold-start readiness | 11.631 s; four attempts |
| JSON larger than 256 / 2,033 characters | 4,096-character value round-tripped successfully |
| Database creation | Passed |
| Focused write workload | 2,000 committed transactions, tempdb work and checkpoint passed |
| Database deletion and clean-state query | Passed; no user databases remained |
| Original invalid flags in the Linux image | Reproduced; exit 1, expected stderr preserved |
| Container state and both SQL log captures | Preserved successfully |
| Password scan | Passed before archive creation; no generated password in exported diagnostics |
| Cleanup | Exact owned container and volume removed |

The focused workload's initialization/write/drop phases were 0.459 / 11.627 / 0.046 seconds in this validation. These are **functional validation observations on Docker Desktop**, not A/B/C disk measurements and not evidence for a runner redesign.

Two earlier independent validation attempts were deliberately retained as failures: the first exposed Windows newline corruption of stdin credentials; the second passed `SELECT 1` and the write workload but exposed empty JSON output during cleanup verification. Their owned containers/volumes were also removed. Neither is counted as a successful initialization.

Final local evidence:

- `artifacts/hyperv-disk/sql-readiness-validated-20261029/validation.json`
- `artifacts/hyperv-disk/sql-readiness-validated-20261029/diagnostics.tar.gz`
- Earlier failed validations: `sql-readiness-validation-20261029` and `sql-readiness-validation-20261029-binary` under the same artifacts directory.

The guest now preserves sanitized probe stdout/stderr and exit/timeout details, allow-listed Docker state, SQL stdout and error logs, a safe failure summary and cleanup result. Preparation failures are included in the existing `guest-failure.tar.gz` archive before VM teardown. Diagnostics and Docker removal are independently protected so a failed log read cannot prevent owned-container cleanup.

Final checks passed: 18 offline regression tests, the separate live SQL initialization test, 17 ownership/cleanup safety assertions, and PowerShell/Bash syntax checks. Docker inventory confirmed zero validation containers and volumes remaining.

## Hyper-V validation and next command

`-Action ValidateSql` skips source/SDK installation, compilation and all migration/A/B/C runs while retaining the existing capacity, idle-state, ownership, placement and cleanup checks. It verifies a real query, SQL version, default/physical data and log paths including tempdb, and a fresh database lifecycle on the benchmark disk.

The validation session lacked Hyper-V management rights. Its attempt is recorded at `artifacts/hyperv-disk/hyperv-sql-validation-20261029`: preflight denied `Get-VM`, no VM was created, and no A/B/C workload was started in that session. The earlier successful elevated preflight belonged to a different Windows token. The independent Docker validation alone does not establish that the Hyper-V-only mode ran successfully; later end-to-end evidence is recorded in [REPORT.md](REPORT.md).

From an elevated PowerShell 7 under the controller account:

```powershell
.\benchmarks\hyperv-disk\Invoke-DiskBenchmark.ps1 -Action ValidateSql
```

After that succeeds, use `-Action Run` for the comparison. There is no controller/configuration/workflow change in this repair.
