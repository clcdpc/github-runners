#!/usr/bin/env python3
"""Summarize successful repetitions, retaining failures and missing evidence explicitly."""
import json
from pathlib import Path
import statistics
import sys
from measure import TESTS


def read(path, default=None):
    return json.loads(path.read_text(encoding="utf-8-sig")) if path.exists() else default


def jsonl(path):
    return [json.loads(line) for line in path.read_text(encoding="utf-8-sig").splitlines() if line.strip()] if path.exists() else []


def fmt(value):
    return "unmeasured" if value is None else f"{value:.3f}"


def median(values):
    return statistics.median(values) if values else None


def weighted(samples, *keys):
    pairs = []
    for row in samples:
        value = row
        for key in keys:
            value = value.get(key) if isinstance(value, dict) else None
        if isinstance(value, (int, float)) and row.get("interval_seconds", 0) > 0:
            pairs.append((value, row["interval_seconds"]))
    return sum(v * w for v, w in pairs) / sum(w for _, w in pairs) if pairs else None


def metrics(result, host, directory):
    deployments = [f["duration_seconds"] for f in result["tests"]["fixtures"]
                   if f["operation"] == "dacpac_deploy_including_database_creation"]
    files = result["sql"]["files"]
    writes = sum(f["num_of_writes"] for f in files)
    samples = [r for r in jsonl(directory / "asap/samples.jsonl") if r.get("kind") == "host"]
    value = {
        "Migration tests (s)": result["tests"]["seconds"],
        "DACPAC deployments total (s)": sum(deployments) if deployments else None,
        "DACPAC individual-call median (s)": median(deployments),
        "SQL create/write/drop, preconditioned (s)": result["sql"]["seconds"],
        "SQL create/write/drop, first use (s)": result["first_use_sql"]["seconds"],
        "SQL database initialization (s)": result["sql"]["database_init_seconds"],
        "SQL writes/checkpoint (s)": result["sql"]["writes_seconds"],
        "SQL database drop (s)": result["sql"]["drop_seconds"],
        "SQL file write latency before drop (ms/op)": sum(f["io_stall_write_ms"] for f in files) / writes if writes else None,
        "Disk reset/restoration incl. shutdown (s)": host["resetSeconds"],
        "VM boot to SSH ready (s)": host["bootReadySeconds"],
        "SQL startup (s)": result["sql_start_seconds"],
        "Guest CPU busy during ASAP (%)": weighted(samples, "cpu", "busy_percent"),
        "Guest I/O pressure during ASAP (%)": weighted(samples, "pressure", "io", "some", "stall_percent"),
    }
    for phase in ("tests", "sql"):
        for wait in ("WRITELOG", "PREEMPTIVE_OS_FLUSHFILEBUFFERS"):
            value[f"{phase}: {wait} (ms)"] = result[phase]["waits"][wait]["wait_time_ms"]
    device = Path(result["device"]).name
    data_samples = [r["disks"][device] for r in samples if device in r.get("disks", {})]
    # CI collector preserves raw completed-operation deltas, avoiding mean-of-means.
    write_ms = sum(r.get("write_time_ms") or 0 for r in data_samples)
    operations = sum(r.get("write_operations") or 0 for r in data_samples)
    value["Guest benchmark disk write await (ms/op)"] = write_ms / operations if operations else None
    start = next((d for d in host["layout"] if d["slot"] == 1), None)
    end = next((d for d in host.get("endLayout", []) if d["slot"] == 1), None)
    value["SQL VHDX file growth (MiB)"] = (end["bytes"] - start["bytes"]) / 1024**2 if start and end else None
    return value


def build_report(output):
    preflight = read(output / "preflight.json", {})
    execution = read(output / "execution.json", {"status": "unknown", "errors": [], "cleanup": "unknown"})
    groups = {key: [] for key in "ABC"}
    failures = []
    preparation = read(output / "preparation/result.json", {})
    expected_digest = preparation.get("image_digest") if preparation.get("success") is True else None
    for directory in sorted(output.iterdir()):
        if not directory.is_dir() or len(directory.name.split("-")) != 2 or directory.name[0] not in groups:
            continue
        result = read(directory / "result.json", {})
        host = read(directory / "host.json", {})
        if result.get("success") is not True or host.get("success") is not True:
            failures.append(directory.name + ": missing successful result/host restoration record")
            continue
        expected_digest = expected_digest or result["image_digest"]
        if result["image_digest"] != expected_digest:
            failures.append(directory.name + ": SQL image digest mismatch")
            continue
        if len(result.get("tests", {}).get("individual", [])) != 3 or {t["test"] for t in result["tests"]["individual"]} != set(TESTS) or any(t["outcome"] != "Passed" for t in result["tests"]["individual"]):
            failures.append(directory.name + ": test success/count validation failed")
            continue
        groups[host["scenario"]].append((result, host, metrics(result, host, directory)))
    enough = all(len(groups[key]) >= 3 for key in groups) and execution["status"] == "complete" and not execution.get("errors") and not failures
    lines = ["# Hyper-V SQL-data disk screening comparison", "",
             "Recommendation: **INCONCLUSIVE**; leave the runner architecture unchanged pending a complete comparison.", "",
             f"Execution status: **{execution['status']}**. Successful repetitions A/B/C: " + "/".join(str(len(groups[k])) for k in "ABC") + ".",
             "", "## Environment", "",
             f"Host: {preflight.get('hostName', 'unknown')}; PowerShell {preflight.get('powerShell', 'unknown')}; "
             f"{preflight.get('logicalProcessors', 'unknown')} logical CPUs. Requested guest: Ubuntu 24.04, 4 vCPU, 16 GiB RAM, separate 40-GiB SQL disk.",
             f"Source commit: `{preflight.get('sourceCommit', 'unknown')}`; tree: `{preflight.get('sourceTree', 'unknown')}`.",
             f"Runner image: `{preflight.get('image', {}).get('version', 'unknown')}`.",
             f"SQL image digest: `{expected_digest or 'not resolved: SQL not run'}`.",
             f"Order: {', '.join(execution.get('order', [])) or 'no timed runs'}.", "",
             "## Median comparison", "", "Missing measurements are never replaced by zeros or CI historical timings.", "",
             "| Metric | A: Differencing | B: Fixed | C: Actual VM checkpoint |", "| --- | ---: | ---: | ---: |"]
    if execution.get("mode") == "ValidateSql":
        valid = preparation.get("success") is True and preparation.get("readiness", {}).get("select_1") == 1
        lines[4] += " SQL-only initialization: " + ("SELECT 1 and database placement checks passed." if valid else "not validated.")
    keys = list(next((item[2] for values in groups.values() for item in values), {
        "Migration tests (s)": None, "DACPAC deployments total (s)": None,
        "SQL create/write/drop (s)": None, "WRITELOG (ms)": None,
        "PREEMPTIVE_OS_FLUSHFILEBUFFERS (ms)": None, "SQL disk write latency (ms/op)": None,
        "Disk reset/recovery (s)": None}))
    medians = {k: {} for k in groups}
    for metric in keys:
        for key in groups:
            medians[key][metric] = median([item[2][metric] for item in groups[key] if item[2].get(metric) is not None])
        lines.append("| " + metric + " | " + " | ".join(fmt(medians[k][metric]) for k in "ABC") + " |")
    lines += ["", "## Absolute and percentage differences", "",
              "Differences are destination minus reference; negative values mean lower time/latency. Percent = 100 × (destination − reference) / reference.", "",
              "| Metric | B − A | B vs A (%) | C − A | C vs A (%) | C − B | C vs B (%) |",
              "| --- | ---: | ---: | ---: | ---: | ---: | ---: |"]
    for metric in keys:
        cells = []
        for target, reference in (("B", "A"), ("C", "A"), ("C", "B")):
            a, b = medians[target][metric], medians[reference][metric]
            cells += [fmt(a - b) if a is not None and b is not None else "unmeasured",
                      fmt(100 * (a - b) / b) if a is not None and b else
                      ("n/a (zero reference)" if a is not None and b == 0 else "unmeasured")]
        lines.append("| " + metric + " | " + " | ".join(cells) + " |")
    lines += ["", "## Repetitions and variability", "",
              "| Run | ASAP (s) | DACPAC total (s) | SQL preconditioned (s) | SQL first use (s) | Reset (s) |",
              "| --- | ---: | ---: | ---: | ---: | ---: |"]
    for key in groups:
        for result, host, value in groups[key]:
            lines.append("| " + result["label"] + " | " + " | ".join(fmt(value[k]) for k in (
                "Migration tests (s)", "DACPAC deployments total (s)", "SQL create/write/drop, preconditioned (s)",
                "SQL create/write/drop, first use (s)", "Disk reset/restoration incl. shutdown (s)")) + " |")
    lines += [""]
    inconsistent = False
    for key in groups:
        for label, metric in (("ASAP", "Migration tests (s)"), ("SQL", "SQL create/write/drop, preconditioned (s)"),
                              ("DACPAC total", "DACPAC deployments total (s)"), ("reset", "Disk reset/restoration incl. shutdown (s)")):
            values = [r[2][metric] for r in groups[key] if r[2][metric] is not None]
            if values:
                spread = (max(values) - min(values)) / statistics.median(values) if statistics.median(values) else 0
                inconsistent |= label in ("ASAP", "SQL") and spread > 0.20
                lines += [f"- {key} {label}: min/median/max {min(values):.3f}/{statistics.median(values):.3f}/{max(values):.3f} s; "
                          f"population stdev {statistics.pstdev(values):.3f} s; range/median {100*spread:.1f}%." ]
    lines += ["", "## Individual migration tests", "",
              "| Test | A median (s) | B median (s) | C median (s) |", "| --- | ---: | ---: | ---: |"]
    names = sorted({t["test"] for values in groups.values() for result, _, _ in values for t in result["tests"]["individual"]})
    for name in names:
        lines.append("| " + name + " | " + " | ".join(fmt(median([t["seconds"] for result, _, _ in groups[k]
                     for t in result["tests"]["individual"] if t["test"] == name])) for k in "ABC") + " |")
    lines += ["", "## Interpretation", ""]
    if enough and not inconsistent:
        a, b, c = (medians[k]["Migration tests (s)"] for k in "ABC")
        saving, percent = a - b, 100 * (a - b) / a
        material = saving >= 5 and percent >= 15
        lines[2] = ("Recommendation: **PROCEED to focused full-VM validation** before any runner redesign." if material else
                    "Recommendation: **DO NOT PROCEED with a disk-driven runner redesign** on this screening evidence.")
        lines += [f"- Fixed disk migration-test saving: {saving:.3f} s ({percent:.1f}%). The declared screening threshold is ≥15% and ≥5 s with range/median ≤20%.",
                  f"- Checkpoint overhead versus direct fixed: {c-b:.3f} s ({100*(c-b)/b:.1f}%)."]
        if material:
            lost = 100 * (c - b) / saving
            lines += [f"- Checkpoint loses {lost:.1f}% of the observed direct-fixed advantage; >50% means most of the advantage is lost."]
        elif saving > 0:
            lines += ["- The direct-fixed difference is below the materiality threshold; expressing checkpoint cost as a percentage of that small advantage would be misleading."]
        lines += ["- A checkpoint pool is a candidate only if C consistently improves A after its reset cost; a direct-fixed pool must also justify the measured full-disk copy cost.",
                  "- This isolated SQL-data experiment cannot establish how much of the GitHub-hosted/CLC timing gap is caused by disk provisioning. A comparable hosted subset and full-VM storage validation are still required."]
    else:
        lines += ["- Fixed-disk advantage: unknown; no architecture decision is supported by incomplete or inconsistent measurements.",
                  "- Whether checkpoints eliminate the advantage: unknown until B and C both have three successful comparable runs.",
                  "- Disk provisioning's contribution to the hosted/CLC gap: unknown. Historical flush waits identify a hypothesis, not the benefit obtainable by changing VHDX type.",
                  "- Neither a checkpoint pool nor direct-fixed restoration is justified yet. Leave the existing process alone while gathering evidence."]
        if inconsistent:
            lines += ["- Range/median exceeds 20%; inspect outliers/host samples and run additional repetitions before interpreting the medians."]
    lines += ["", "## Limitations and timing boundaries", "",
              "- A separate SQL-data disk is a screening benchmark. Boot, Docker image layers, .NET binaries and source remain on boot differencing disks. A uses a fixed parent, while production uses a dynamic boot parent.",
              "- C is an actual offline standard VM checkpoint; it redirects both SQL and boot writes. A/B use the same boot baseline without that extra checkpoint layer. Layouts and paths are in each host.json.",
              "- Reset includes graceful VM shutdown plus A/B disk recreation (including B's full fixed-disk copy), or C's actual Restore-VMSnapshot. C's initial baseline copy/checkpoint creation is reported as diskCreateSeconds, outside recurring restore cost; final disk deletion/checkpoint merge is teardownSeconds. VM boot and SQL startup are separate.",
              "- Every run starts from the same shut-down SQL/OS baselines. Docker containers, test files and databases do not carry over between repetitions. Build, restore, pulls, disk preparation and SQL startup are outside test/SQL timings.",
              "- First-use SQL and preconditioned SQL are recorded separately. Each first-use workload drops its database before the ASAP tests; preconditioning is not proof of long-running steady state. Host caches are not forcibly flushed.",
              "- DACPAC timing includes database creation, using unchanged ASAP fixture instrumentation. It does not separately time all migration subprocess initialization. Focused SQL initialization is separately measured.",
              "- The focused workload captures its data/log counters before database deletion. ASAP's sampled file I/O can miss databases created/dropped between samples; raw diagnostics preserve missing/reset intervals.",
              "- SQL waits accumulate across workers and cannot be added to elapsed time. Guest await and SQL file stall measure different layers. Missing host latency counters are reported as unavailable.",
              "- Boot and SQL differencing-disk growth are bounded by their declared budgets, checked with the 15-GiB host reserve every five seconds during guest commands. This is not the production controller's full 100-GiB potential-growth reservation.",
              "", "## Failures and cleanup", ""]
    lines += ["- " + str(e).replace("\n", " ") for e in execution.get("errors", []) + failures] or ["- No reported measurement failures."]
    lines += ["- " + execution.get("cleanup", "unknown"),
              "- Host virtual-disk latency: " + preflight.get("hostVirtualDiskLatency", "unavailable"),
              "", "Raw per-run results, TRX, fixture JSONL, CI SQL/guest diagnostics, disk layouts and host growth samples live beside this report.", ""]
    return "\n".join(lines)


if __name__ == "__main__":
    output = Path(sys.argv[1]).resolve()
    (output / "comparison.md").write_text(build_report(output), encoding="utf-8")
