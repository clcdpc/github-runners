#!/usr/bin/env python3
"""Guest measurement of unchanged ASAP tests and a small SQL workload; stdlib only."""
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import time
import traceback
import xml.etree.ElementTree as ET
from datetime import datetime, timezone

TESTS = (
    "ImportUsesSourceStaffEmailAndReconcilesFreshSqlTargetWithoutIdentityMap",
    "ImportMapsFormatAndFoundRequestWithCanonicalTag",
    "ImportPreservesAdditionalCopyHistoryAndUsesCreatedWhenUpdatedIsMissing",
)
WAITS = ("WRITELOG", "PREEMPTIVE_OS_FLUSHFILEBUFFERS")
ROOT = Path(__file__).resolve().parent
SQL_DIAGNOSTICS = None
SQLCMD_PATH = "/opt/mssql-tools18/bin/sqlcmd"
STAGE = "startup"


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def command(args, **kwargs):
    try:
        return subprocess.run(args, check=True, text=True, capture_output=True, timeout=300, **kwargs).stdout.strip()
    except (subprocess.SubprocessError, OSError) as error:
        error.benchmark_failure = failure_record(error, args=args)
        if SQL_DIAGNOSTICS is not None:
            write_json(SQL_DIAGNOSTICS / "failure.json", error.benchmark_failure)
        raise


def sanitize(value):
    if isinstance(value, bytes):
        value = value.decode("utf-8", errors="replace")
    value = str(value or "")
    secrets = {os.environ.get(key, "") for key in (
        "ASAP_TEST_SQL_SA_PASSWORD", "MSSQL_SA_PASSWORD", "ASAP_TEST_SQL_CONNECTION_STRING")}
    for secret in sorted(secrets - {""}, key=len, reverse=True):
        value = value.replace(secret, "[REDACTED]").replace(json.dumps(secret)[1:-1], "[REDACTED]")
    return re.sub(r"(?i)\b(password|pwd|SQLCMDPASSWORD|MSSQL_SA_PASSWORD|ASAP_TEST_SQL_SA_PASSWORD)\s*=\s*"
                  r"(?:\[REDACTED\]|\"[^\"]*\"|'[^']*'|[^;,\s\"'\\}\]]+)", r"\1=[REDACTED]", value)


def command_identity(args):
    """Keep actionable argv, excluding credentials and SQL/script payloads."""
    result = []
    omitted = False
    for arg in args:
        value = str(arg)
        result.append("[REDACTED]" if omitted else sanitize(value))
        omitted = value in ("-P", "--password", "--connection-string", "-Q", "-q", "-c")
    return result


def set_stage(stage):
    global STAGE
    STAGE = stage
    if SQL_DIAGNOSTICS is not None:
        with (SQL_DIAGNOSTICS / "stages.jsonl").open("a", encoding="utf-8") as stream:
            stream.write(json.dumps({"utc": datetime.now(timezone.utc).isoformat(), "stage": stage}) + "\n")


def failure_record(error, *, args=None):
    previous = getattr(error, "benchmark_failure", {})
    identity = command_identity(args) if args is not None else previous.get("command")
    if identity is None and isinstance(error, (subprocess.CalledProcessError, subprocess.TimeoutExpired)):
        identity = command_identity(error.cmd if isinstance(error.cmd, (list, tuple)) else [error.cmd])
    if isinstance(error, subprocess.CalledProcessError):
        message = f"Subprocess exited with code {error.returncode}"
    elif isinstance(error, subprocess.TimeoutExpired):
        message = f"Subprocess timed out after {error.timeout} seconds"
    else:
        message = sanitize(str(error))
    # Format stack frames without locals or the exception's raw command repr.
    stack = sanitize("".join(traceback.format_list(traceback.extract_tb(error.__traceback__))))
    return {"utc": datetime.now(timezone.utc).isoformat(), "type": type(error).__name__,
            "message": message, "stage": previous.get("stage", STAGE), "command": identity,
            "exit_code": getattr(error, "returncode", previous.get("exit_code")),
            "timed_out": isinstance(error, subprocess.TimeoutExpired) or previous.get("timed_out", False),
            "stdout": sanitize(getattr(error, "stdout", previous.get("stdout", ""))),
            "stderr": sanitize(getattr(error, "stderr", previous.get("stderr", ""))),
            "traceback": stack + type(error).__name__ + ": " + message}


def diagnostic_command(args, timeout=10, **kwargs):
    """Capture bounded, sanitized output; never persist input, argv or Docker env."""
    started = time.monotonic()
    try:
        result = subprocess.run(args, text=False, capture_output=True, check=False, timeout=timeout, **kwargs)
        data = {"exit_code": result.returncode, "timed_out": False,
                "stdout": sanitize(result.stdout), "stderr": sanitize(result.stderr)}
    except subprocess.TimeoutExpired as error:
        data = {"exit_code": None, "timed_out": True,
                "stdout": sanitize(error.stdout), "stderr": sanitize(error.stderr)}
    except OSError as error:
        data = {"exit_code": None, "timed_out": False, "stdout": "", "stderr": sanitize(str(error))}
    return {"utc": datetime.now(timezone.utc).isoformat(), "stage": STAGE, "command": command_identity(args),
            "seconds": time.monotonic() - started, **data}


def sql_result(text, *, readiness=False, timeout=None, container="asap-sql"):
    # Same path, local target, SA login and certificate trust as ASAP's CI probe.
    # The shell reads only the password; -Q executes SQL independently of stdin.
    options = ["-h", "-1"] if readiness else ["-y", "0", "-w", "65535"]
    args = ["docker", "exec", "--interactive", container, "/bin/sh", "-c",
            'IFS= read -r SQLCMDPASSWORD || exit 1; export SQLCMDPASSWORD; exec "$@"',
            "clc-sqlcmd", SQLCMD_PATH, "-S", "localhost", "-U", "sa", "-C", "-d", "master", "-b", "-r", "1",
            "-l", "2" if readiness else "3", "-t", "3" if readiness else "180",
            *options, "-Q", "SET NOCOUNT ON;\n" + text]
    # ODBC sqlcmd rejects -h combined with -y 0 before attempting a connection.
    # Binary input also works from a Windows validation process: text-mode stdin
    # would translate LF to CRLF, making /bin/sh read a trailing CR as password data.
    return diagnostic_command(args, timeout=timeout if timeout is not None else (8 if readiness else 195),
                              input=(os.environ["ASAP_TEST_SQL_SA_PASSWORD"] + "\n").encode("utf-8"))


def sql(text, *, container="asap-sql"):
    result = sql_result(text, container=container)
    if result["exit_code"] != 0 or result["timed_out"]:
        if SQL_DIAGNOSTICS is not None:
            with (SQL_DIAGNOSTICS / "sql-command-failures.jsonl").open("a", encoding="utf-8") as stream:
                stream.write(json.dumps(result) + "\n")
        error = ValueError(f"SQL command failed (exit {result['exit_code']}, timeout={result['timed_out']}); see SQL diagnostics")
        error.benchmark_failure = result
        raise error
    return result["stdout"].strip()


def sql_json(query, *, container="asap-sql"):
    # Like ASAP's DMV collector, make FOR JSON a scalar max-width result.
    # Direct FOR JSON can produce no sqlcmd output for an empty rowset and
    # stream long JSON over multiple rows; neither is a reliable JSON document.
    query = query.rstrip().removesuffix(";")
    return json.loads(sql("SELECT CONVERT(nvarchar(max), COALESCE((" + query + "), N'[]'));", container=container))


def container_state(container="asap-sql", timeout=5):
    result = diagnostic_command(["docker", "inspect", container, "--format", "{{json .State}}"], timeout=timeout)
    state = None
    if result["exit_code"] == 0:
        try:
            raw = json.loads(result["stdout"])
            state = {key: raw[key] for key in ("Status", "Running", "Paused", "Restarting", "OOMKilled", "Dead",
                                               "Pid", "ExitCode", "Error", "StartedAt", "FinishedAt") if key in raw}
        except (ValueError, TypeError):
            result["stderr"] = "Docker state response was not valid JSON"
    # Do not export .Config.Env or health-check command/output fields.
    return {**{k: v for k, v in result.items() if k != "stdout"}, "state": state}


def wait_ready(output, *, timeout_seconds=180, container="asap-sql"):
    started = time.monotonic()
    deadline = started + timeout_seconds
    attempt = 0
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        attempt += 1
        result = sql_result("SELECT 1;", readiness=True, timeout=min(8, remaining), container=container)
        result["attempt"] = attempt
        result["select_1_succeeded"] = result["exit_code"] == 0 and result["stdout"].strip() == "1"
        with (output / "sql-probe.jsonl").open("a", encoding="utf-8") as stream:
            stream.write(json.dumps(result) + "\n")
        if result["select_1_succeeded"]:
            ready = {"select_1": 1, "exit_code": 0, "attempts": attempt, "seconds": time.monotonic() - started,
                     "sqlcmd_path": SQLCMD_PATH, "target": "localhost", "database": "master", "trust_server_certificate": True}
            write_json(output / "sql-ready.json", ready)
            return ready
        error = (result["stdout"] + "\n" + result["stderr"]).lower()
        if result["exit_code"] == 127 or "mutually exclusive" in error or "invalid option" in error:
            raise ValueError("SQL readiness client invocation failed; see sql-probe.jsonl")
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        state = container_state(container, timeout=min(5, remaining))
        write_json(output / "sql-state.json", state)
        if state["state"] and (not state["state"].get("Running") or state["state"].get("OOMKilled")):
            raise ValueError("SQL exited or was OOM-killed during startup; see sql-state.json")
        time.sleep(min(2, max(0, deadline - time.monotonic())))
    raise ValueError(f"SQL did not execute SELECT 1 within {timeout_seconds:g} seconds; see sql-probe.jsonl")


def preserve_sql_diagnostics(output, *, container="asap-sql"):
    write_json(output / "sql-state.json", container_state(container))
    logs = diagnostic_command(["docker", "logs", "--tail", "200", container])
    (output / "sql-container.log").write_text(logs["stdout"] + logs["stderr"], encoding="utf-8")
    errorlog = diagnostic_command(["docker", "exec", container, "tail", "-c", "262144", "/var/opt/mssql/log/errorlog"])
    (output / "sql-error.log").write_text(errorlog["stdout"] + errorlog["stderr"], encoding="utf-8")
    write_json(output / "sql-diagnostics.json", {"docker_logs": {k: v for k, v in logs.items() if k not in ("stdout", "stderr")},
                                               "sql_errorlog": {k: v for k, v in errorlog.items() if k not in ("stdout", "stderr")}})


def wait_snapshot():
    return sql_json("SELECT wait_type, wait_time_ms, waiting_tasks_count, signal_wait_time_ms "
                    "FROM sys.dm_os_wait_stats WHERE wait_type IN "
                    "('WRITELOG','PREEMPTIVE_OS_FLUSHFILEBUFFERS') FOR JSON PATH;")


def wait_delta(before, after):
    old = {row["wait_type"]: row for row in before}
    result = {}
    for row in after:
        prior = old.get(row["wait_type"])
        if prior is None:
            raise ValueError("Missing baseline SQL wait counter")
        values = {key: row[key] - prior[key] for key in ("wait_time_ms", "waiting_tasks_count", "signal_wait_time_ms")}
        if min(values.values()) < 0:
            raise ValueError("SQL wait counters reset during measurement")
        result[row["wait_type"]] = values
    if set(result) != set(WAITS):
        raise ValueError("Required SQL waits unavailable")
    return result


def parse_trx(path):
    root = ET.parse(path).getroot()
    rows = []
    for element in root.iter():
        if element.tag.rsplit("}", 1)[-1] != "UnitTestResult":
            continue
        hours, minutes, seconds = element.attrib["duration"].split(":")
        rows.append({"test": element.attrib["testName"], "outcome": element.attrib["outcome"],
                     "seconds": int(hours) * 3600 + int(minutes) * 60 + float(seconds)})
    # Exact identity/count is essential: zero tests, skipped tests and broad filters fail.
    if len(rows) != 3 or {r["test"] for r in rows} != set(TESTS) or any(r["outcome"] != "Passed" for r in rows):
        raise ValueError("Expected exactly the three selected, passing migration tests")
    return rows


def assert_clean():
    if sql_json("SELECT name FROM sys.databases WHERE database_id>4 FOR JSON PATH;"):
        raise ValueError("Stale SQL user database; repetition is invalid")


def resolve_sql_file(root, relative):
    """Resolve SQL's case-insensitive catalog path on a case-sensitive guest FS."""
    relative = PurePosixPath(relative)
    if relative.is_absolute() or not relative.parts or ".." in relative.parts:
        raise ValueError("SQL file path escapes the benchmark mount")
    root = Path(root).resolve(strict=True)
    current = root
    for part in relative.parts:
        matches = [entry for entry in current.iterdir() if entry.name.casefold() == part.casefold()]
        if len(matches) != 1:
            raise ValueError(f"Expected one filesystem match for SQL path {relative}; found {len(matches)} at {part}")
        current = matches[0].resolve(strict=True)
        if not current.is_relative_to(root):
            raise ValueError("Resolved SQL file escapes the benchmark mount")
    if not current.is_file():
        raise ValueError(f"SQL physical path is not a regular file: {relative}")
    return str(current)


def verify_files(device):
    set_stage("verify_sql_files.inspect_bind_mount")
    mounts = json.loads(command(["docker", "inspect", "asap-sql", "--format", "{{json .Mounts}}"]))
    matches = [m for m in mounts if m["Destination"] == "/var/opt/mssql" and m["Type"] == "bind" and m["RW"]]
    if len(matches) != 1 or matches[0]["Source"] != "/mnt/clc-disk-benchmark/mssql":
        raise ValueError("SQL bind mount differs from the benchmark disk")
    set_stage("verify_sql_files.list_files")
    files = sql_json("SELECT database_id, type_desc, physical_name FROM sys.master_files FOR JSON PATH;")
    if SQL_DIAGNOSTICS is not None:
        write_json(SQL_DIAGNOSTICS / "sql-file-placement.json", {"mounts": mounts, "files": files})
    if not any(f["database_id"] == 2 and f["type_desc"] == "ROWS" for f in files) or not any(
            f["database_id"] == 2 and f["type_desc"] == "LOG" for f in files):
        raise ValueError("tempdb data/log verification unavailable")
    for item in files:
        name = item["physical_name"]
        if not name.startswith("/var/opt/mssql/") or ".." in Path(name).parts:
            raise ValueError("SQL file is outside /var/opt/mssql")
        # sys.master_files retains MSDBData.mdf/MSDBLog.ldf on this SQL build,
        # while the actual Linux files are msdbdata.mdf/msdblog.ldf. Resolve
        # unique directory entries rather than changing names or skipping files.
        set_stage("verify_sql_files.resolve_file")
        guest_path = command(["sudo", "python3", str(Path(__file__).resolve()), "resolve-sql-file",
                              "/mnt/clc-disk-benchmark/mssql", name.removeprefix("/var/opt/mssql/")])
        set_stage("verify_sql_files.mount_source")
        if command(["sudo", "findmnt", "-n", "-o", "SOURCE", "--target", guest_path]) != device:
            raise ValueError("SQL physical file is on a different guest device")
        item["resolved_guest_path"] = guest_path
        if SQL_DIAGNOSTICS is not None:
            write_json(SQL_DIAGNOSTICS / "sql-file-placement.json", {"mounts": mounts, "files": files})
    return files


def focused():
    assert_clean()
    before = wait_snapshot()
    started = time.monotonic()
    value = json.loads(sql((ROOT / "workload.sql").read_text(encoding="utf-8")))
    value["seconds"] = time.monotonic() - started
    value["waits"] = wait_delta(before, wait_snapshot())
    assert_clean()
    return value


def run_tests(output):
    source = ROOT / "source"
    output.mkdir()
    env = os.environ.copy()
    env["ASAP_CI_FIXTURE_TIMINGS"] = str(output / "fixture-timings.jsonl")
    args = ["python3", "scripts/ci/performance.py", "--output", str(output), "--interval", "2",
            "--docker-interval", "10", "--sql-interval", "5", "--", "dotnet", "test", "--project",
            "tests/Asap.Tests/Asap.Tests.csproj", "--configuration", "Release", "--no-build", "--no-restore",
            "--minimum-expected-tests", "3", "--report-trx", "--report-trx-filename", "tests.trx",
            "--results-directory", str(output), "--filter", "|".join("FullyQualifiedName=" +
                "Asap.Tests.Migration.MigrationCliTests." + name for name in TESTS)]
    before = wait_snapshot()
    started = time.monotonic()
    with (output / "tests.log").open("w", encoding="utf-8") as log:
        completed = subprocess.run(args, cwd=source, env=env, stdout=log, stderr=subprocess.STDOUT, timeout=900)
    elapsed = time.monotonic() - started
    if completed.returncode:
        raise ValueError(f"Selected ASAP tests failed (exit {completed.returncode}); see tests.log")
    rows = parse_trx(output / "tests.trx")
    instrumentation = json.loads((output / "environment.json").read_text(encoding="utf-8"))
    if instrumentation["command_exit_code"] != 0 or instrumentation["command_duration_seconds"] <= 0:
        raise ValueError("CI instrumentation did not record a successful test command")
    fixtures = [json.loads(line) for line in (output / "fixture-timings.jsonl").read_text().splitlines()]
    if not fixtures or any(row["succeeded"] is not True for row in fixtures):
        raise ValueError("DACPAC fixture timings missing or unsuccessful")
    assert_clean()
    return {"seconds": instrumentation["command_duration_seconds"], "collector_elapsed_seconds": elapsed,
            "individual": rows, "fixtures": fixtures, "waits": wait_delta(before, wait_snapshot())}


def main():
    global SQL_DIAGNOSTICS
    action, label, device = sys.argv[1:]
    if action not in ("prepare", "run", "validate") or not re.fullmatch(r"[A-Za-z0-9-]+", label):
        raise ValueError("Invalid measurement action/label")
    output = ROOT / "results" / label
    output.mkdir(parents=True)
    SQL_DIAGNOSTICS = output
    set_stage("read_inputs")
    password = (ROOT / "password").read_text().strip()
    digest = (ROOT / "image-digest").read_text().strip()
    if not re.fullmatch(r"mcr.microsoft.com/mssql/server@sha256:[0-9a-f]{64}", digest):
        raise ValueError("SQL image digest was not pinned")
    os.environ["ASAP_TEST_SQL_SA_PASSWORD"] = password
    os.environ["MSSQL_SA_PASSWORD"] = password
    os.environ["ASAP_TEST_SQL_CONNECTION_STRING"] = (
        "Server=localhost,1433;Database=master;User ID=sa;Password=" + password +
        ";Encrypt=True;TrustServerCertificate=True")
    set_stage("check_clean_docker_state")
    if command(["docker", "ps", "-a", "--format", "{{.Names}}"]):
        raise ValueError("Unexpected Docker state in clean benchmark VM")
    try:
        started = time.monotonic()
        set_stage("start_sql_container")
        command(["docker", "run", "--detach", "--name", "asap-sql", "--publish", "127.0.0.1:1433:1433",
                 "--env", "ACCEPT_EULA=Y", "--env", "MSSQL_SA_PASSWORD", "--mount",
                 "type=bind,src=/mnt/clc-disk-benchmark/mssql,dst=/var/opt/mssql", digest])
        set_stage("sql_readiness")
        ready = wait_ready(output)
        startup_seconds = time.monotonic() - started
        files = verify_files(device)
        set_stage("sql_version")
        sql_version = sql("SELECT @@VERSION;")
        set_stage("sdk_version")
        sdk = command(["dotnet", "--version"], cwd=ROOT / "source") if action != "validate" else None
        set_stage("docker_version")
        docker_version = command(["docker", "version", "--format", "{{.Server.Version}}"])
        result = {"schema": 1, "action": action, "label": label, "success": False, "image_digest": digest,
                  "sql_start_seconds": startup_seconds, "device": device,
                  "sql_files": files, "sql_version": sql_version, "readiness": ready,
                  "sdk": sdk, "docker_version": docker_version}
        set_stage("sql_major_version")
        if sql("SELECT CONVERT(varchar(10),SERVERPROPERTY('ProductMajorVersion'));") != "16":
            raise ValueError("SQL Server 2022 (major version 16) is required")
        set_stage("check_clean_sql_state")
        assert_clean()
        set_stage("sql_default_paths")
        defaults = sql_json("SELECT CONVERT(nvarchar(4000),SERVERPROPERTY('InstanceDefaultDataPath')) AS data_path, "
                            "CONVERT(nvarchar(4000),SERVERPROPERTY('InstanceDefaultLogPath')) AS log_path "
                            "FOR JSON PATH, WITHOUT_ARRAY_WRAPPER;")
        if any(not value.startswith("/var/opt/mssql/") for value in defaults.values()):
            raise ValueError("New migration database defaults are outside the benchmark disk")
        result["sql_default_paths"] = defaults
        if action in ("prepare", "validate"):
            set_stage("create_placement_probe_database")
            sql("CREATE DATABASE ClcPlacementProbe;")
            result["sql_files_with_default_database"] = verify_files(device)
            set_stage("drop_placement_probe_database")
            sql("DROP DATABASE ClcPlacementProbe;")
        if action == "run":
            # Separate first-use allocation/cache behavior; this also preconditions the small workload.
            set_stage("first_use_sql_workload")
            result["first_use_sql"] = focused()
            set_stage("selected_asap_tests")
            result["tests"] = run_tests(output / "asap")
            set_stage("preconditioned_sql_workload")
            result["sql"] = focused()
            result["sql_files_after"] = verify_files(device)
            state = json.loads(command(["docker", "inspect", "asap-sql", "--format", "{{json .State}}"]))
            if not state["Running"] or state["OOMKilled"] or command(
                    ["docker", "inspect", "asap-sql", "--format", "{{.RestartCount}}" ]) != "0":
                raise ValueError("SQL exited, restarted or was OOM-killed during measurement")
        result["success"] = True
        set_stage("complete")
        write_json(output / "result.json", result)
    except BaseException as error:
        write_json(output / "failure.json", failure_record(error))
        raise
    finally:
        try:
            preserve_sql_diagnostics(output)
        finally:
            cleanup = diagnostic_command(["docker", "rm", "--force", "asap-sql"], timeout=30)
            write_json(output / "sql-cleanup.json", cleanup)
            if cleanup["exit_code"] != 0:
                raise ValueError("SQL container cleanup failed; see sql-cleanup.json")


if __name__ == "__main__":
    try:
        if len(sys.argv) == 4 and sys.argv[1] == "resolve-sql-file":
            print(resolve_sql_file(sys.argv[2], sys.argv[3]))
        else:
            main()
    except Exception as error:
        # Tracebacks omit locals and exception command reprs; argv/stdout/stderr
        # are exported only through the sanitized structured record.
        print("Benchmark failed: " + json.dumps(failure_record(error)), file=sys.stderr)
        sys.exit(1)
