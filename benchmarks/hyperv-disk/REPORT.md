# Hyper-V SQL disk screening benchmark — October 29, 2026

**Recommendation: DO NOT PROCEED with a disk-driven runner redesign on this evidence.** Complete preparation and all nine A/B/C repetitions passed. Direct fixed disks reduced the selected migration-test median by only **3.234 s (2.7%)**, while increasing the measured shutdown/reset median by **31.953 s**. The focused SQL workload improved by **18.7%**, which did not translate into a material application-test improvement.

The preparation defect and diagnostics are fixed. The failed command was `sudo test -f /mnt/clc-disk-benchmark/mssql/data/MSDBData.mdf`: SQL's catalog spelling differed from the real Linux filename `msdbdata.mdf`. The repair resolves unique filesystem matches, preserves all file/device checks, and archives detailed sanitized subprocess failures. See [the investigation](PREPARATION-FAILURE.md).

## Execution and environment

| Item | Measured / verified value |
| --- | --- |
| Successful repetitions | A: 3; B: 3; C: 3 |
| Selected test outcomes | 27 passed; no failed or skipped selected tests |
| Order | A-1, B-1, C-1, B-2, C-2, A-2, C-3, A-3, B-3 |
| Host | Nested Windows Hyper-V host, 16 logical processors, approximately 64 GiB RAM |
| Guest | Ubuntu 24.04; kernel `6.8.0-147-generic`; 4 vCPUs; fixed 16 GiB RAM |
| SQL storage | Separate 40-GiB ext4 disk, verified as `/dev/sdb` |
| Boot image | Existing sealed Ubuntu 24.04 image, 100-GiB logical boot parent; no conversion/rebuild |
| SQL Server | 2022, `16.0.4305.5` |
| Docker / SDK | Docker `29.9.2`; .NET SDK `10.0.303` |
| SQL image digest | `mcr.microsoft.com/mssql/server@sha256:cf8f0ab2ebe61feec2eed2629061a07d247f791c2a8ddda74a4ac9f06a291718` in all runs |
| Source provenance | All repetitions used the same application workload tree; exact private source/CI provenance remains in archived operational evidence |
| Network / registration | Existing CLC switch/NAT; isolated `.254` address outside the controller pool; no GitHub registration |
| Capacity / isolation | Idle-runner checks remained active; minimum sampled free host storage was 63.8 GiB, above the 15-GiB reserve |

A uses a writable differencing child of a prepared fixed SQL parent. B writes a fixed copy directly without a checkpoint. C uses a real offline standard VM checkpoint and actual `Restore-VMSnapshot`; workload samples and end layouts confirm both SQL and boot writes went to AVHDXs. Hyper-V finalized the boot-disk redirect at startup, so the preboot layout alone was not used to verify it.

Every repetition starts from equivalent shut-down SQL and OS baselines. Docker state, user databases and test output do not carry over. Restore, image downloads, compilation, initial SQL preparation and startup are outside test/workload timings. The necessary projects were built once in the measured environment before the repetitions; the independent preparation validation also built its own isolated copy. The full 1,444-test suite was not run.

## Median timings

Differences are B minus A; negative means faster. Percent differences use A as the denominator.

| Metric | A: Differencing | B: Fixed | C: VM checkpoint | B minus A | B vs A |
| --- | ---: | ---: | ---: | ---: | ---: |
| Selected migration command (s) | 119.246 | 116.012 | 135.337 | -3.234 | -2.7% |
| Instrumented DACPAC total per repetition (s) | 17.063 | 15.346 | 17.832 | -1.717 | -10.1% |
| Individual instrumented DACPAC call median (s) | 4.155 | 3.750 | 4.452 | -0.406 | -9.8% |
| SQL create/write/drop, preconditioned (s) | 12.986 | 10.553 | 11.660 | -2.434 | -18.7% |
| SQL create/write/drop, first use (s) | 11.615 | 9.525 | 12.001 | -2.090 | -18.0% |
| SQL database initialization (s) | 0.476 | 0.431 | 0.444 | -0.045 | -9.5% |
| SQL writes/checkpoint phase (s) | 12.359 | 10.097 | 11.059 | -2.262 | -18.3% |
| SQL database deletion (s) | 0.047 | 0.054 | 0.049 | +0.008 | +16.3% |
| Shutdown and recurring disk restoration (s) | 7.809 | 39.762 | 4.946 | +31.953 | +409.2% |
| Boot to SSH ready (s) | 8.097 | 8.180 | 8.375 | +0.083 | +1.0% |
| SQL startup (s) | 8.786 | 8.801 | 9.011 | +0.015 | +0.2% |

C's migration median was **16.091 s (13.5%) slower than A** and **19.325 s (16.7%) slower than B**. Its preconditioned SQL workload was **1.326 s (10.2%) faster than A**, but **1.108 s (10.5%) slower than B**. C's shutdown/restore median was **2.863 s (36.7%) lower than A** and **34.816 s (87.6%) lower than B**.

Recurring reset includes graceful shutdown plus A/B disk recreation, including B's full 40-GiB copy, or C's actual checkpoint restoration. C's initial fixed copy/checkpoint creation and final checkpoint merge/deletion are recorded separately. Boot and SQL startup are separate rows. Do not add independently calculated medians to manufacture a measured end-to-end job time.

There were four successful central DACPAC-helper timing records per repetition, 36 in total. Their timings include database creation. Direct deployments and migration subprocess initialization outside that helper are not completely instrumented; the table labels refer to the observed helper calls.

## SQL waits, latency and pressure

These are medians across repetitions. Waits accumulate across workers; they are not additive components of wall time. SQL file latency and guest device await describe different layers.

| Metric | Differencing | Fixed | Checkpoint |
| --- | ---: | ---: | ---: |
| Application workload `WRITELOG` delta (ms) | 15,816 | 16,771 | 15,735 |
| Application workload `PREEMPTIVE_OS_FLUSHFILEBUFFERS` delta (ms) | 52,496 | 43,710 | 55,720 |
| Focused SQL `WRITELOG` delta (ms) | 2,108 | 2,001 | 1,893 |
| Focused SQL `PREEMPTIVE_OS_FLUSHFILEBUFFERS` delta (ms) | 9,324 | 7,122 | 8,459 |
| Focused SQL file write latency (ms/op) | 0.858 | 0.812 | 0.774 |
| Guest benchmark-disk write await during application workload (ms/op) | 1.359 | 1.101 | 1.462 |
| Guest CPU busy during application workload (%) | 26.947 | 27.764 | 26.986 |
| Guest I/O pressure during application workload (%) | 8.892 | 7.400 | 8.247 |
| SQL leaf VHDX growth through execution (MiB) | 512 | 0 | 512 |

Fixed disks lowered the guest write-await median by **0.258 ms/op (19.0%)** and the application workload flush-wait delta by **8,786 ms (16.7%)**, while application workload `WRITELOG` increased by **955 ms (6.0%)**. Neither the wait counters nor the focused workload supports treating the small migration-time difference as a large whole-job speedup.

Host storage counter samples are preserved in `host-counters.jsonl` with their installed counter names, values and status. Their units were not independently normalized into an outer-host physical latency claim. SQL file counters were captured before deleting the focused workload database; sampled application workload I/O can miss databases that live entirely between snapshots.

## Every successful repetition

| Run | Migration command (s) | Instrumented DACPAC total (s) | SQL preconditioned (s) | SQL first use (s) | Shutdown/reset (s) |
| --- | ---: | ---: | ---: | ---: | ---: |
| A-1 | 128.987 | 18.729 | 12.986 | 12.405 | 7.808 |
| A-2 | 119.246 | 16.285 | 13.107 | 10.934 | 7.809 |
| A-3 | 114.431 | 17.063 | 10.919 | 11.615 | 7.901 |
| B-1 | 116.012 | 14.998 | 10.507 | 9.506 | 50.791 |
| B-2 | 133.367 | 15.642 | 10.553 | 9.638 | 39.762 |
| B-3 | 112.342 | 15.346 | 11.076 | 9.525 | 37.135 |
| C-1 | 135.337 | 17.832 | 11.450 | 12.872 | 2.903 |
| C-2 | 126.620 | 18.337 | 11.660 | 12.001 | 5.024 |
| C-3 | 136.033 | 17.339 | 12.660 | 11.194 | 4.946 |

Migration min/max and population standard deviation were A **114.431–128.987 / 6.055 s**, B **112.342–133.367 / 9.170 s**, and C **126.620–136.033 / 4.283 s**. Migration range/median was **12.2%, 18.1%, 7.0%**; focused SQL range/median was **16.8%, 5.4%, 10.4%**. These remained below the declared 20% variability screen. Fixed reset ranged **37.135–50.791 s** and checkpoint reset **2.903–5.024 s**; that variability did not reverse their ordering against A's **7.808–7.901 s**. No failed or skipped test was counted as a successful fast run.

| Individual test median (s) | Differencing | Fixed | Checkpoint |
| --- | ---: | ---: | ---: |
| `ImportUsesSourceStaffEmailAndReconcilesFreshSqlTargetWithoutIdentityMap` | 32.175 | 31.368 | 36.581 |
| `ImportMapsFormatAndFoundRequestWithCanonicalTag` | 67.590 | 66.780 | 80.427 |
| `ImportPreservesAdditionalCopyHistoryAndUsesCreatedWhenUpdatedIsMissing` | 16.899 | 15.227 | 16.320 |

## Decision and limits

- **Are fixed VHDXs materially faster for application workload migration tests?** Not in this screen: 3.234 seconds / 2.7% is below the declared 15% and 5-second screen and smaller than the run-to-run spread. The focused write workload improved, but that improvement did not carry through to a material migration-test benefit.
- **Do checkpoints preserve the fixed-disk advantage?** C had a slower migration median than A and B. Its focused SQL advantage over A was smaller than B's. Expressing a percentage of the tiny migration advantage lost would be misleading.
- **Does provisioning explain much of the hosted/self-hosted gap?** This experiment does not establish that. It does not include a matched hosted subset or a full fixed-boot/Docker-storage validation. The historical reference run took 3,499.346 seconds for its full SQL/.NET command and showed substantial flush waits; those counters alone did not predict a large benefit from changing this disk's type.
- **Is a checkpoint pool worthwhile?** Checkpoint restoration was fast, but C did not improve migration execution here. Pool benefits from caching SDKs, images or other startup work remain separate hypotheses; no controller redesign is justified by these SQL timings.
- **Are direct fixed disks worth complex restoration?** Not on this evidence: approximately 3.2 seconds saved in the selected command versus approximately 32 seconds of extra measured shutdown/reset cost.
- **Proceed?** Leave the existing runner architecture unchanged. A separate SQL-data disk is a screening benchmark, not a full reproduction of boot-disk/Docker writes. Any later evidence of a substantial full-job advantage should receive focused full-VM validation before a controller change.

Three repetitions per scenario give a small screening sample, not a precision performance guarantee. Host caches were not forcibly flushed. First-use and preconditioned workload results are separate; neither proves long-running steady state. C redirects the boot disk too, so its result cannot isolate SQL-disk checkpoint overhead alone.

## Repair validation, failures and cleanup

The latest reported preparation failure was traced and reproduced with full diagnostics before applying the case-resolution fix. The original failing command returned 1 with empty streams; it was not an authentication or readiness failure. The failed trace VM was cleaned up. The independent full `Prepare` run then passed SDK/build, SQL initialization, file/default/tempdb placement and probe-database creation/deletion before the benchmark began.

All nine benchmark records, all 27 selected test outcomes, SQL readiness and cleanup exits, identical image digests and scenario layouts passed a separate result audit. No resource guard, test or SQL measurement failed. C's boot/data AVHDX redirection was checked in every workload host sample and end layout. The controller process remained running, and the original image remained read-only and unchanged in size/timestamp.

**Cleanup completed:** benchmark VM, checkpoints, VHDXs, ISO and SSH keys removed; `local/benchmarks/hyperv-disk` is absent. Production controller/configuration, GitHub configuration, production images and unrelated VMs were not changed. Existing user changes were preserved.

Local evidence under `artifacts/hyperv-disk/`:

- `20261029-154037-032b6c6f/`: original failed run.
- `preparation-trace-20261029/guest-failure.tar.gz`: exact failing command, exit/streams and sanitized traceback.
- `complete-preparation-20261029/`: successful independent full preparation and cleanup.
- `abc-measured-20261029/comparison.md`: complete generated metrics, absolute/percentage differences and variability.
- `abc-measured-20261029/measured-results.json`: audited full-precision metrics and individual results.
- `abc-measured-20261029/`: each run's JSON, host layouts/restoration timings, TRX, fixture and SQL/guest diagnostics, archives and host counter/growth samples.
- `prepared-benchmark-status-20261029.json`: completed approved preparation → measurement sequence.

Use the [rerun and cleanup instructions](README.md). The repair adds `-Action Prepare` for complete preparation without timed workloads and improves `failure.json`/`stages.jsonl` preservation. No new package or framework is required.
