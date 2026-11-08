"""Readiness regressions; optional live test owns a unique Docker container/volume."""
import json
import os
from pathlib import Path, PurePosixPath
import secrets
import subprocess
import tarfile
import tempfile
import sys
import unittest
from unittest import mock
import uuid

import measure


class SqlReadinessTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.output = Path(self.directory.name)
        self.password = "Fixture!$'\"Password"
        self.enterContext(mock.patch.dict(os.environ, {
            "ASAP_TEST_SQL_SA_PASSWORD": self.password, "MSSQL_SA_PASSWORD": self.password,
            "ASAP_TEST_SQL_CONNECTION_STRING": "Server=fixture;Password=" + self.password + ";Encrypt=True"}))

    def process(self, stdout="1\n", stderr="", returncode=0):
        return subprocess.CompletedProcess([], returncode, stdout, stderr)

    def test_readiness_executes_bounded_select_with_ci_path_target_credentials_and_tls(self):
        with mock.patch.object(measure.subprocess, "run", return_value=self.process()) as run:
            measure.sql_result("SELECT 1;", readiness=True)
        args = run.call_args.args[0]
        options = run.call_args.kwargs
        self.assertEqual(args[:5], ["docker", "exec", "--interactive", "asap-sql", "/bin/sh"])
        self.assertIn('exec "$@"', args[6])
        self.assertIn(measure.SQLCMD_PATH, args)
        for flag, value in (("-S", "localhost"), ("-U", "sa"), ("-d", "master"), ("-l", "2"), ("-t", "3")):
            self.assertEqual(args[args.index(flag) + 1], value)
        self.assertIn("-C", args)
        self.assertIn("-b", args)
        self.assertIn("SELECT 1;", args[args.index("-Q") + 1])
        self.assertNotIn("-y", args)
        self.assertNotIn("-P", args)
        self.assertNotIn(self.password, " ".join(args))
        self.assertEqual(options["input"], (self.password + "\n").encode("utf-8"))
        self.assertFalse(options["text"])
        self.assertEqual(options["timeout"], 8)

    def test_json_queries_keep_unlimited_width_without_conflicting_header_flag(self):
        with mock.patch.object(measure.subprocess, "run", return_value=self.process('[{"value":"long"}]')) as run:
            self.assertEqual(json.loads(measure.sql("SELECT 'long' AS value FOR JSON PATH;"))[0]["value"], "long")
        args = run.call_args.args[0]
        self.assertEqual(args[args.index("-y") + 1], "0")
        self.assertNotIn("-h", args)
        self.assertNotIn("-W", args)

    def test_empty_database_list_is_an_explicit_scalar_json_result(self):
        with mock.patch.object(measure, "sql", return_value="[]") as sql:
            measure.assert_clean()
        query = sql.call_args.args[0]
        self.assertTrue(query.startswith("SELECT CONVERT(nvarchar(max), COALESCE(("))
        self.assertIn("FOR JSON PATH), N'[]')", query)
        with mock.patch.object(measure, "sql", return_value='[{"name":"StaleDatabase"}]'):
            with self.assertRaisesRegex(ValueError, "Stale SQL"):
                measure.assert_clean()

    def test_exit_zero_without_select_result_is_not_ready(self):
        values = [self.process(""), self.process(" 1\n")]
        with mock.patch.object(measure.subprocess, "run", side_effect=values), \
             mock.patch.object(measure, "container_state", return_value={"state": {"Running": True, "OOMKilled": False}}), \
             mock.patch.object(measure.time, "sleep"):
            ready = measure.wait_ready(self.output)
        self.assertEqual(ready["attempts"], 2)
        self.assertEqual(ready["select_1"], 1)
        rows = [json.loads(s) for s in (self.output / "sql-probe.jsonl").read_text().splitlines()]
        self.assertFalse(rows[0]["select_1_succeeded"])
        self.assertTrue(rows[1]["select_1_succeeded"])

    def test_client_invocation_error_is_recorded_and_fails_immediately(self):
        error = "Sqlcmd: The -h and the -y 0 options are mutually exclusive. " + self.password
        with mock.patch.object(measure.subprocess, "run", return_value=self.process("", error, 1)), \
             mock.patch.object(measure.time, "sleep") as sleep:
            with self.assertRaisesRegex(ValueError, "client invocation failed"):
                measure.wait_ready(self.output)
        sleep.assert_not_called()
        text = (self.output / "sql-probe.jsonl").read_text()
        self.assertNotIn(self.password, text)
        self.assertNotIn(json.dumps(self.password)[1:-1], text)
        self.assertEqual(json.loads(text)["exit_code"], 1)
        self.assertIn("mutually exclusive", text)

    def test_missing_sqlcmd_fails_immediately(self):
        with mock.patch.object(measure.subprocess, "run", return_value=self.process("", "sqlcmd: not found", 127)):
            with self.assertRaisesRegex(ValueError, "client invocation failed"):
                measure.wait_ready(self.output)

    def test_deadline_includes_probe_and_docker_state_time(self):
        clock = [0.0]
        def monotonic():
            return clock[0]
        def slow_probe(*args, **kwargs):
            self.assertLessEqual(kwargs["timeout"], 0.5)
            clock[0] += kwargs["timeout"]
            return {"exit_code": None, "timed_out": True, "stdout": "", "stderr": "timed out"}
        with mock.patch.object(measure.time, "monotonic", side_effect=monotonic), \
             mock.patch.object(measure, "sql_result", side_effect=slow_probe), \
             mock.patch.object(measure, "container_state") as state:
            with self.assertRaisesRegex(ValueError, "within 0.5 seconds"):
                measure.wait_ready(self.output, timeout_seconds=0.5)
        state.assert_not_called()
        self.assertFalse((self.output / "sql-ready.json").exists())

    def test_timeout_partial_stderr_is_sanitized_and_retained(self):
        error = subprocess.TimeoutExpired(["docker"], 1, output=b"partial", stderr=("Password=" + self.password).encode())
        with mock.patch.object(measure.subprocess, "run", side_effect=error):
            row = measure.sql_result("SELECT 1;", readiness=True)
        self.assertIsNone(row["exit_code"])
        self.assertTrue(row["timed_out"])
        self.assertIn("[REDACTED]", row["stderr"])
        self.assertNotIn(self.password, json.dumps(row))

    def test_diagnostics_keep_state_and_both_logs_without_health_or_env(self):
        state = {"Running": False, "OOMKilled": True, "ExitCode": 137, "Error": "Password=" + self.password,
                 "Health": {"Log": [{"Output": "sensitive health content"}]}, "Env": ["sensitive env content"]}
        values = [self.process(json.dumps(state)), self.process("SQL log " + self.password),
                  self.process("SQL error log " + self.password)]
        with mock.patch.object(measure.subprocess, "run", side_effect=values):
            measure.preserve_sql_diagnostics(self.output)
        saved = json.loads((self.output / "sql-state.json").read_text())
        self.assertEqual(saved["state"]["ExitCode"], 137)
        self.assertTrue(saved["state"]["OOMKilled"])
        self.assertNotIn("Health", saved["state"])
        text = "\n".join(p.read_text() for p in self.output.iterdir())
        self.assertNotIn(self.password, text)
        self.assertNotIn("sensitive env content", text)
        self.assertNotIn("sensitive health content", text)
        self.assertIn("SQL log", text)
        self.assertIn("SQL error log", text)

    def test_main_preserves_failure_and_cleans_even_if_log_capture_fails(self):
        # Readiness timeout must not prevent the existing owned-container cleanup.
        (self.output / "password").write_text(self.password)
        (self.output / "image-digest").write_text("mcr.microsoft.com/mssql/server@sha256:" + "a" * 64)
        with mock.patch.object(measure, "ROOT", self.output), \
             mock.patch.object(measure.sys, "argv", ["measure.py", "validate", "preparation", "/dev/sdb"]), \
             mock.patch.object(measure, "SQL_DIAGNOSTICS", None), \
             mock.patch.object(measure, "command", return_value=""), \
             mock.patch.object(measure, "wait_ready", side_effect=ValueError("SQL readiness failed")), \
             mock.patch.object(measure, "preserve_sql_diagnostics", side_effect=OSError("log sink unavailable")), \
             mock.patch.object(measure, "diagnostic_command", return_value={"exit_code": 0}) as cleanup:
            with self.assertRaises(OSError):
                measure.main()
        cleanup.assert_called_once_with(["docker", "rm", "--force", "asap-sql"], timeout=30)
        failure = json.loads((self.output / "results/preparation/failure.json").read_text())
        self.assertEqual(failure["message"], "SQL readiness failed")

    def test_validate_mode_initializes_sql_without_sdk_or_migration_tests(self):
        (self.output / "password").write_text(self.password)
        (self.output / "image-digest").write_text("mcr.microsoft.com/mssql/server@sha256:" + "a" * 64)
        defaults = {"data_path": "/var/opt/mssql/data/", "log_path": "/var/opt/mssql/data/"}
        def sql(query, **kwargs):
            if "ProductMajorVersion" in query:
                return "16"
            return "SQL Server 2022" if "@@VERSION" in query else ""
        with mock.patch.object(measure, "ROOT", self.output), \
             mock.patch.object(measure.sys, "argv", ["measure.py", "validate", "preparation", "/dev/sdb"]), \
             mock.patch.object(measure, "SQL_DIAGNOSTICS", None), \
             mock.patch.object(measure, "command", return_value="") as command, \
             mock.patch.object(measure, "sql", side_effect=sql), \
             mock.patch.object(measure, "sql_json", side_effect=[[], defaults]), \
             mock.patch.object(measure, "verify_files", return_value=[]), \
             mock.patch.object(measure, "wait_ready", return_value={"select_1": 1}), \
             mock.patch.object(measure, "run_tests") as tests, \
             mock.patch.object(measure, "focused") as workload, \
             mock.patch.object(measure, "preserve_sql_diagnostics"), \
             mock.patch.object(measure, "diagnostic_command", return_value={"exit_code": 0}):
            measure.main()
        tests.assert_not_called()
        workload.assert_not_called()
        self.assertFalse(any("dotnet" in call.args[0] for call in command.call_args_list))
        value = json.loads((self.output / "results/preparation/result.json").read_text())
        self.assertTrue(value["success"])
        self.assertEqual(value["action"], "validate")
        self.assertEqual(value["readiness"]["select_1"], 1)
        self.assertIsNone(value["sdk"])

    def test_subprocess_failure_records_stage_identity_exit_streams_and_traceback(self):
        args = ["test-client", "-P", self.password, "-Q", "SELECT '" + self.password + "';"]
        error = subprocess.CalledProcessError(23, args, output="output " + self.password, stderr="error " + self.password)
        with mock.patch.object(measure, "SQL_DIAGNOSTICS", self.output), \
             mock.patch.object(measure, "STAGE", "verify_sql_files.resolve_file"), \
             mock.patch.object(measure.subprocess, "run", side_effect=error):
            with self.assertRaises(subprocess.CalledProcessError):
                measure.command(args)
        text = (self.output / "failure.json").read_text()
        row = json.loads(text)
        self.assertEqual(row["stage"], "verify_sql_files.resolve_file")
        self.assertEqual(row["exit_code"], 23)
        self.assertEqual(row["command"], ["test-client", "-P", "[REDACTED]", "-Q", "[REDACTED]"])
        self.assertIn("output [REDACTED]", row["stdout"])
        self.assertIn("error [REDACTED]", row["stderr"])
        self.assertIn("command", row["traceback"])
        self.assertNotIn(self.password, text)

    def test_catalog_case_mismatch_resolves_actual_regular_file(self):
        root = self.output / "mssql"
        (root / "data").mkdir(parents=True)
        actual = root / "data/msdbdata.mdf"
        actual.write_text("fixture")
        self.assertEqual(measure.resolve_sql_file(root, "data/MSDBData.mdf"), str(actual.resolve()))
        result = subprocess.run([sys.executable, str(measure.ROOT / "measure.py"), "resolve-sql-file", str(root),
                                 "data/MSDBData.mdf"], capture_output=True, text=True, check=True)
        self.assertEqual(result.stdout.strip(), str(actual.resolve()))
        with self.assertRaises(ValueError):
            measure.resolve_sql_file(root, "../outside.mdf")
        with self.assertRaises(ValueError):
            measure.resolve_sql_file(root, "data/missing.mdf")

    def test_catalog_case_resolution_rejects_ambiguous_entries(self):
        entry1 = mock.Mock(name="entry1")
        entry1.name = "MSDBData.mdf"
        entry2 = mock.Mock(name="entry2")
        entry2.name = "msdbdata.mdf"
        with mock.patch.object(Path, "iterdir", return_value=iter([entry1, entry2])):
            with self.assertRaisesRegex(ValueError, "found 2"):
                measure.resolve_sql_file(self.output, "MSDBData.mdf")

    def test_catalog_case_resolution_rejects_a_link_escaping_the_mount(self):
        entry = mock.Mock(name="entry")
        entry.name = "data"
        entry.resolve.return_value = self.output.parent.resolve()
        with mock.patch.object(Path, "iterdir", return_value=iter([entry])):
            with self.assertRaisesRegex(ValueError, "escapes the benchmark mount"):
                measure.resolve_sql_file(self.output, "data")

    def test_main_keeps_subprocess_failure_when_diagnostic_collection_also_fails(self):
        (self.output / "password").write_text(self.password)
        (self.output / "image-digest").write_text("mcr.microsoft.com/mssql/server@sha256:" + "a" * 64)
        failed_args = ["sudo", "test", "-f", "/mnt/clc-disk-benchmark/mssql/data/MSDBData.mdf"]
        def process(args, **kwargs):
            if args == failed_args:
                raise subprocess.CalledProcessError(1, args, output="stdout " + self.password,
                                                    stderr="stderr " + self.password)
            return self.process("")
        def verify(device):
            measure.set_stage("verify_sql_files.file_exists")
            measure.command(failed_args)
        with mock.patch.object(measure, "ROOT", self.output), \
             mock.patch.object(measure.sys, "argv", ["measure.py", "validate", "preparation", "/dev/sdb"]), \
             mock.patch.object(measure, "SQL_DIAGNOSTICS", None), \
             mock.patch.object(measure.subprocess, "run", side_effect=process), \
             mock.patch.object(measure, "wait_ready", return_value={"select_1": 1}), \
             mock.patch.object(measure, "verify_files", side_effect=verify), \
             mock.patch.object(measure, "preserve_sql_diagnostics", side_effect=OSError("log read failed")), \
             mock.patch.object(measure, "diagnostic_command", return_value={"exit_code": 0}) as cleanup:
            with self.assertRaisesRegex(OSError, "log read failed"):
                measure.main()
        cleanup.assert_called_once_with(["docker", "rm", "--force", "asap-sql"], timeout=30)
        path = self.output / "results/preparation/failure.json"
        value = json.loads(path.read_text())
        self.assertEqual(value["type"], "CalledProcessError")
        self.assertEqual(value["command"], failed_args)
        self.assertEqual(value["exit_code"], 1)
        self.assertIn("main", value["traceback"])
        self.assertIn("command", value["traceback"])
        archive_path = self.output / "failure.tar.gz"
        with tarfile.open(archive_path, "w:gz") as archive:
            archive.add(path, arcname="results/preparation/failure.json")
        with tarfile.open(archive_path, "r:gz") as archive:
            saved = archive.extractfile("results/preparation/failure.json").read().decode()
        self.assertNotIn(self.password, saved)
        self.assertIn("[REDACTED]", saved)


@unittest.skipUnless(os.environ.get("CLC_BENCHMARK_LIVE_SQL") == "1", "opt-in isolated Docker SQL initialization")
class SqlReadinessLiveTest(unittest.TestCase):
    def test_initialization_select_json_and_database_lifecycle(self):
        owner = uuid.uuid4().hex
        container = "clc-disk-benchmark-sqlcheck-" + owner
        volume = container + "-data"
        label = "org.clc.disk-benchmark-owner"
        output = Path(os.environ.get("CLC_SQL_VALIDATION_OUTPUT", str(measure.ROOT.parents[1] / "artifacts/hyperv-disk" /
                                                                    ("sql-validation-" + owner)))).resolve()
        output.mkdir(parents=True, exist_ok=False)
        env = {"ASAP_TEST_SQL_SA_PASSWORD": "Bench" + secrets.token_hex(24) + "Aa1!"}
        env["MSSQL_SA_PASSWORD"] = env["ASAP_TEST_SQL_SA_PASSWORD"]
        metadata = {"mode": "isolated Docker readiness validation, not Hyper-V storage benchmarking", "owner": owner,
                    "container": container, "volume": volume, "success": False}
        with mock.patch.dict(os.environ, env), mock.patch.object(measure, "SQL_DIAGNOSTICS", output):
            def checked(args, timeout=60):
                result = measure.diagnostic_command(args, timeout=timeout)
                if result["exit_code"] != 0:
                    measure.write_json(output / "docker-command-failure.json", result)
                    raise AssertionError("Owned Docker operation failed; see sanitized validation evidence")
                return result["stdout"].strip()
            try:
                # Resolve the cached image once, then use only its digest.
                image = "mcr.microsoft.com/mssql/server:2022-latest"
                digest = checked(["docker", "image", "inspect", image, "--format", "{{index .RepoDigests 0}}"])
                metadata["image_digest"] = digest
                checked(["docker", "volume", "create", "--label", label + "=" + owner, volume])
                checked(["docker", "run", "--detach", "--name", container, "--label", label + "=" + owner,
                         "--memory", "4g", "--cpus", "4", "--env", "ACCEPT_EULA=Y", "--env", "MSSQL_SA_PASSWORD",
                         "--mount", "type=volume,src=" + volume + ",dst=/var/opt/mssql", digest])
                metadata["readiness"] = measure.wait_ready(output, container=container)
                metadata["files_at_readiness"] = measure.sql_json(
                    "SELECT database_id,name,type_desc,physical_name FROM sys.master_files FOR JSON PATH;", container=container)
                measure.write_json(output / "files-at-readiness.json", metadata["files_at_readiness"])
                for item in metadata["files_at_readiness"]:
                    requested = PurePosixPath(item["physical_name"])
                    names = checked(["docker", "exec", "--user", "0", container, "find", str(requested.parent),
                                     "-maxdepth", "1", "-type", "f", "-printf", "%f\n"]).splitlines()
                    matches = [name for name in names if name.casefold() == requested.name.casefold()]
                    self.assertEqual(len(matches), 1)
                    actual = str(requested.parent / matches[0]).replace("\\", "/")
                    item["actual_path"] = actual
                    checked(["docker", "exec", "--user", "0", container, "test", "-f", actual])
                measure.write_json(output / "files-at-readiness.json", metadata["files_at_readiness"])
                self.assertEqual(measure.sql("SELECT 1;", container=container).strip(), "1")
                self.assertEqual(measure.sql("SELECT CONVERT(varchar(10),SERVERPROPERTY('ProductMajorVersion'));", container=container), "16")
                long_json = measure.sql_json("SELECT REPLICATE(CONVERT(nvarchar(max),N'x'),4096) AS value FOR JSON PATH;", container=container)
                self.assertEqual(len(long_json[0]["value"]), 4096)
                metadata["workload"] = json.loads(measure.sql((measure.ROOT / "workload.sql").read_text(), container=container))
                self.assertEqual(measure.sql_json("SELECT name FROM sys.databases WHERE database_id>4 FOR JSON PATH;", container=container), [])
                metadata["sql_version"] = measure.sql("SELECT @@VERSION;", container=container)
                # Reproduce the original defect inside the actual Linux SQL image.
                bad = measure.diagnostic_command(["docker", "exec", container, measure.SQLCMD_PATH,
                                                  "-h", "-1", "-y", "0", "-Q", "SELECT 1;"])
                measure.write_json(output / "original-probe-error.json", bad)
                self.assertNotEqual(bad["exit_code"], 0)
                self.assertIn("mutually exclusive", bad["stderr"] + bad["stdout"])
                metadata["success"] = True
            except BaseException as error:
                metadata["failure_type"] = type(error).__name__
                raise
            finally:
                try:
                    measure.preserve_sql_diagnostics(output, container=container)
                finally:
                    # Identity/label checks precede every removal; never match prefixes.
                    for kind, name, template in (("container", container, '{{index .Config.Labels "' + label + '"}}'),
                                                  ("volume", volume, '{{index .Labels "' + label + '"}}')):
                        identity = measure.diagnostic_command(["docker", kind, "inspect", name, "--format", template])
                        if identity["exit_code"] == 0:
                            if identity["stdout"].strip() != owner:
                                raise AssertionError("Resource ownership differs; preserving it")
                            removal = checked(["docker", kind, "rm", *(["--force"] if kind == "container" else []), name])
                            metadata[kind + "_removed"] = bool(removal)
                    measure.write_json(output / "validation.json", metadata)
                    for path in output.iterdir():
                        if path.is_file():
                            self.assertTrue(env["ASAP_TEST_SQL_SA_PASSWORD"] not in path.read_text(encoding="utf-8"),
                                            "Credential appeared in validation diagnostics")
                    with tarfile.open(output / "diagnostics.tar.gz", "w:gz") as archive:
                        for path in output.iterdir():
                            if path.is_file() and path.name != "diagnostics.tar.gz":
                                archive.add(path, arcname=path.name)
        print("Isolated SQL initialization evidence: " + str(output))


if __name__ == "__main__":
    unittest.main()
